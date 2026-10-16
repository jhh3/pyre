package daemon

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Local doorbell waits (W-localwait). A Claude session's inbox watcher used
// to hold a messages.list long-poll against the hub for its whole life (one
// hub request per idle session). It now blocks here instead, on an
// in-memory per-session signal, and lists the hub inbox once (non-waiting)
// when woken. pyred learns about new mail from the deliver commands the hub
// already sends over the node link; a (re)connected hub link wakes every
// waiter, since commands may have been missed meanwhile. Nothing here stores
// or interprets message content, so delivery semantics are untouched: the
// hub stays the source of truth and the watcher's own checks (paste path,
// acks, runtime state) decide whether to ring.

// maxInboxWait bounds one wait; the caller re-arms (and lists the hub inbox
// on each return, which is the safety net for anything not signalled).
const maxInboxWait = 60 * time.Second

type inboxWaiter struct {
	ch chan struct{} // closed on signal
	n  int           // goroutines waiting on ch
}

// inboxWaiters is usable as its zero value.
type inboxWaiters struct {
	mu    sync.Mutex
	epoch string
	next  int64            // last generation issued
	gen   map[string]int64 // session -> generation of its last signal
	all   int64            // generation of the last wake-all
	wait  map[string]*inboxWaiter
}

func (w *inboxWaiters) initLocked() {
	if w.epoch == "" {
		now := time.Now().UnixNano()
		w.epoch = strconv.FormatInt(now, 36)
		w.next = 1
		w.gen = map[string]int64{}
		w.wait = map[string]*inboxWaiter{}
	}
}

func (w *inboxWaiters) currentLocked(session string) int64 {
	return max(w.gen[session], w.all, 1)
}

// signal wakes the waiters of one session.
func (w *inboxWaiters) signal(session string) {
	if session == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()
	w.next++
	w.gen[session] = w.next
	if x := w.wait[session]; x != nil {
		close(x.ch)
		delete(w.wait, session)
	}
}

// signalAll wakes every waiter (hub link reconnected).
func (w *inboxWaiters) signalAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.initLocked()
	w.next++
	w.all = w.next
	w.gen = map[string]int64{} // all supersedes every older per-session generation
	for s, x := range w.wait {
		close(x.ch)
		delete(w.wait, s)
	}
}

// waiting reports how many goroutines currently wait (tests, diagnostics).
func (w *inboxWaiters) waiting() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, x := range w.wait {
		n += x.n
	}
	return n
}

// await blocks until session is signalled past (epoch, after), d elapses,
// or ctx ends.
func (w *inboxWaiters) await(ctx context.Context, session, epoch string, after int64, d time.Duration) (contracts.InboxWaitResponse, error) {
	w.mu.Lock()
	w.initLocked()
	cur := w.currentLocked(session)
	if epoch != w.epoch || cur > after || d <= 0 {
		resp := contracts.InboxWaitResponse{Epoch: w.epoch, Generation: cur, Woken: epoch != w.epoch || cur > after}
		w.mu.Unlock()
		return resp, nil
	}
	x := w.wait[session]
	if x == nil {
		x = &inboxWaiter{ch: make(chan struct{})}
		w.wait[session] = x
	}
	x.n++
	w.mu.Unlock()

	t := time.NewTimer(d)
	defer t.Stop()
	var err error
	select {
	case <-x.ch:
	case <-t.C:
	case <-ctx.Done():
		err = ctx.Err()
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.wait[session] == x { // not signalled: drop our reference
		x.n--
		if x.n == 0 {
			delete(w.wait, session)
		}
	}
	cur = w.currentLocked(session)
	return contracts.InboxWaitResponse{Epoch: w.epoch, Generation: cur, Woken: cur > after}, err
}

// signalInbox wakes the local doorbell waiters of a session.
func (d *Daemon) signalInbox(session string) { d.inboxWait.signal(session) }

var _ contracts.InboxWaitAPI = (*LocalAPI)(nil)

// InboxWait implements contracts.OpInboxWait on the local socket. A session
// credential may wait only on its own inbox; the owner on any.
func (l *LocalAPI) InboxWait(ctx context.Context, r contracts.InboxWaitRequest) (contracts.InboxWaitResponse, error) {
	p, ok := contracts.PrincipalFrom(ctx)
	if !ok {
		return contracts.InboxWaitResponse{}, contracts.Errorf(contracts.ErrUnauthorized, "no principal")
	}
	if !contracts.ValidID(r.SessionID, contracts.PrefixSession) {
		return contracts.InboxWaitResponse{}, contracts.Errorf(contracts.ErrInvalid, "session_id is required")
	}
	switch p.Kind {
	case contracts.PrincipalOwner:
	case contracts.PrincipalSession:
		if p.ID != r.SessionID {
			return contracts.InboxWaitResponse{}, contracts.Errorf(contracts.ErrForbidden, "a session may wait only on its own inbox").WithRef(contracts.KindSession, r.SessionID)
		}
	default:
		return contracts.InboxWaitResponse{}, contracts.Errorf(contracts.ErrForbidden, "%s principals cannot wait on a session inbox", p.Kind)
	}
	wait := min(time.Duration(max(r.WaitSeconds, 0))*time.Second, maxInboxWait)
	resp, err := l.d.inboxWait.await(ctx, r.SessionID, r.Epoch, r.AfterGeneration, wait)
	if err != nil && ctx.Err() != nil {
		return resp, err // caller went away
	}
	return resp, nil
}
