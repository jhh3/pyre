package sqlitex

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Write-lock hygiene (W-dbhygiene).
//
// Every transaction opened through this package takes SQLite's write lock
// at BEGIN (_txlock=immediate). Two failure modes held the hub's lock for
// the whole busy_timeout in production:
//
//   - a nested write: code inside an open transaction calls something that
//     writes on its own pooled connection (another store, a token issuer).
//     That BEGIN waits for the lock its own goroutine holds, so it can only
//     fail after busy_timeout, and every other writer queues behind it;
//   - a long transaction: slow work inside the closure.
//
// Tx/Begin detect the first case when it is a nested Tx/Begin on the same
// *sql.DB in the same goroutine and fail it immediately (ErrNestedTx)
// instead of waiting; and log a warning with the call site when BEGIN
// waited, or the transaction was held, longer than SlowTxThreshold.

// ErrNestedTx is returned when a goroutine that already holds a write
// transaction on a database opens a second one on the same database: the
// second BEGIN could only time out (self-deadlock).
var ErrNestedTx = fmt.Errorf("sqlitex: nested write transaction on the same database in one goroutine (self-deadlock); use the open *sql.Tx or move the call after commit")

var (
	slowTxNanos atomic.Int64 // 0: default (1s); <0: disabled
	txLogger    atomic.Pointer[slog.Logger]
	// open write transactions: goroutine id -> databases it holds a tx on.
	heldMu sync.Mutex
	held   = map[uint64][]*sql.DB{}
	// nestedPanic makes a detected nested transaction panic (tests) instead
	// of returning ErrNestedTx.
	nestedPanic atomic.Bool
)

// DefaultSlowTx is the default threshold for slow BEGIN/transaction warnings.
const DefaultSlowTx = time.Second

// SetSlowTxThreshold sets the duration above which a BEGIN wait or a held
// write transaction is logged (0 restores the default, <0 disables).
func SetSlowTxThreshold(d time.Duration) {
	if d < 0 {
		slowTxNanos.Store(-1)
		return
	}
	slowTxNanos.Store(int64(d))
}

func slowTx() time.Duration {
	n := slowTxNanos.Load()
	switch {
	case n < 0:
		return 0
	case n == 0:
		return DefaultSlowTx
	}
	return time.Duration(n)
}

// SetLogger sets where slow-transaction and nested-transaction warnings go
// (nil: slog.Default()).
func SetLogger(l *slog.Logger) { txLogger.Store(l) }

func logger() *slog.Logger {
	if l := txLogger.Load(); l != nil {
		return l
	}
	return slog.Default()
}

// PanicOnNestedTx makes a detected nested transaction panic with the stack
// instead of returning ErrNestedTx. Tests enable it so a self-lock fails
// loudly at the offending call.
func PanicOnNestedTx(on bool) { nestedPanic.Store(on) }

// goid returns the current goroutine's id (parsed from the stack header
// "goroutine N [...]"; about a microsecond).
func goid() uint64 {
	var buf [64]byte
	b := buf[:runtime.Stack(buf[:], false)]
	b = b[len("goroutine "):]
	if i := strings.IndexByte(string(b), ' '); i > 0 {
		b = b[:i]
	}
	id, _ := strconv.ParseUint(string(b), 10, 64)
	return id
}

// txWatch tracks one write transaction's lock wait and hold time.
type txWatch struct {
	db    *sql.DB
	gid   uint64
	start time.Time // before BEGIN
	began time.Time // after BEGIN returned
}

// enter registers a write transaction about to begin on db. It fails with
// ErrNestedTx if this goroutine already holds one on db.
func enter(db *sql.DB) (*txWatch, error) {
	w := &txWatch{db: db, gid: goid(), start: time.Now()}
	heldMu.Lock()
	for _, d := range held[w.gid] {
		if d == db {
			heldMu.Unlock()
			site := callSite()
			logger().Error("sqlite: nested write transaction (self-deadlock avoided)", "caller", site)
			if nestedPanic.Load() {
				panic(fmt.Sprintf("%v\ncaller: %s\n%s", ErrNestedTx, site, stack()))
			}
			return nil, fmt.Errorf("%w (at %s)", ErrNestedTx, site)
		}
	}
	held[w.gid] = append(held[w.gid], db)
	heldMu.Unlock()
	return w, nil
}

// begun records that BEGIN returned (successfully or not).
func (w *txWatch) begun() {
	w.began = time.Now()
	if t := slowTx(); t > 0 {
		if wait := w.began.Sub(w.start); wait > t {
			logger().Warn("sqlite: BEGIN waited for the write lock", "wait", wait.Round(time.Millisecond).String(), "caller", callSite())
		}
	}
}

// exit unregisters the transaction and logs it if it held the lock long.
func (w *txWatch) exit() {
	heldMu.Lock()
	dbs := held[w.gid]
	for i, d := range dbs {
		if d == w.db {
			dbs = append(dbs[:i], dbs[i+1:]...)
			break
		}
	}
	if len(dbs) == 0 {
		delete(held, w.gid)
	} else {
		held[w.gid] = dbs
	}
	heldMu.Unlock()
	if w.began.IsZero() {
		return
	}
	if t := slowTx(); t > 0 {
		if d := time.Since(w.began); d > t {
			logger().Warn("sqlite: write transaction held the lock", "held", d.Round(time.Millisecond).String(), "caller", callSite())
		}
	}
}

// Begin starts a write transaction with Tx's instrumentation and nested
// transaction check. The caller must call end exactly once after Commit or
// Rollback (also when Begin failed, end is a no-op then).
func Begin(ctx context.Context, db *sql.DB) (tx *sql.Tx, end func(), err error) {
	w, err := enter(db)
	if err != nil {
		return nil, func() {}, err
	}
	tx, err = db.BeginTx(ctx, nil)
	w.begun()
	if err != nil {
		w.began = time.Time{} // no hold to report
		w.exit()
		return nil, func() {}, err
	}
	var once sync.Once
	return tx, func() { once.Do(w.exit) }, nil
}

// wrapperPrefixes are transaction helpers skipped when naming the caller.
var wrapperPrefixes = []string{
	"github.com/jhh3/pyre/internal/storage/sqlitex.Tx",
	"github.com/jhh3/pyre/internal/storage/sqlitex.Begin",
	"github.com/jhh3/pyre/internal/storage/sqlitex.Batched",
	"github.com/jhh3/pyre/internal/storage/sqlitex.PruneOperations",
	"github.com/jhh3/pyre/internal/storage/sqlitex.(*txWatch)",
	"github.com/jhh3/pyre/internal/storage/sqlitex.enter",
	"github.com/jhh3/pyre/internal/storage/sqlitex.callSite",
	"github.com/jhh3/pyre/internal/persistence.Mutate",
	"github.com/jhh3/pyre/internal/projects/svcutil.Mutate",
	"sync.",
	"runtime.",
}

var wrapperSuffixes = []string{"(*Hub).tx", "(*Hub).mutate"}

func isWrapper(fn string) bool {
	for _, p := range wrapperPrefixes {
		if strings.HasPrefix(fn, p) {
			return true
		}
	}
	for _, s := range wrapperSuffixes {
		if strings.HasSuffix(fn, s) {
			return true
		}
	}
	return false
}

// callSite names the first two frames outside the transaction helpers,
// i.e. who opened the transaction (only computed on the slow path).
func callSite() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	for {
		f, more := frames.Next()
		if f.Function != "" && !isWrapper(f.Function) {
			fn := f.Function
			if i := strings.LastIndexByte(fn, '/'); i >= 0 {
				fn = fn[i+1:]
			}
			out = append(out, fmt.Sprintf("%s (%s:%d)", fn, shortFile(f.File), f.Line))
			if len(out) == 2 {
				break
			}
		}
		if !more {
			break
		}
	}
	return strings.Join(out, " <- ")
}

func shortFile(p string) string {
	if i := strings.Index(p, "/internal/"); i >= 0 {
		return p[i+1:]
	}
	if i := strings.Index(p, "/cmd/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func stack() string {
	buf := make([]byte, 16<<10)
	return string(buf[:runtime.Stack(buf, false)])
}
