package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
)

// Downloads over a slow link to the hub (a node relayed through a distant
// DERP, ~1.6 s RTT) must not look like a silent hang: `pyre artifact get`
// reports progress on stderr, aborts an attempt when no bytes arrive for
// the stall timeout, and resumes with HTTP Range from a <dest>.part file,
// both automatically (a few retries) and on a rerun. There is no overall
// deadline: a big file on a slow link is legitimate.

const (
	defaultStallTimeout = 60 * time.Second
	downloadRetries     = 3
	progressEveryTTY    = time.Second
	progressEveryLog    = 5 * time.Second // agent transcripts: keep them small
)

// downloadBackoff is the wait before automatic retry n (0-based); tests
// shorten it.
var downloadBackoff = func(n int) time.Duration { return time.Second << n }

// contentRanger is implemented by *client.Client: resuming needs Range.
type contentRanger interface {
	OpenContentRange(ctx context.Context, versionID string, offset, length int64) (io.ReadCloser, error)
}

// errStalled marks an attempt aborted by the stall watchdog.
var errStalled = errors.New("stalled")

// errNoRange: the server answered a Range request with the whole body.
var errNoRange = errors.New("range not supported")

type fetcher struct {
	a     *App
	api   c.API
	ver   c.ArtifactVersion
	name  string        // shown in progress and errors
	stall time.Duration // <= 0 disables the watchdog
	part  string        // the kept partial file ("" for stdout)
	prog  *progress
}

// fetch writes the version's bytes from *off to w (h hashes everything
// written), retrying stalls and transient failures with backoff and
// resuming with Range each time. restart (nil when the bytes cannot be
// taken back, as on stdout) rewinds the destination when the server cannot
// resume. *off is the number of bytes in w when fetch returns.
func (f *fetcher) fetch(ctx context.Context, w io.Writer, h hash.Hash, off *int64, restart func() error) error {
	ranger, _ := f.api.(contentRanger)
	failures, attempts := 0, 0
	for *off < f.ver.Size {
		attempts++
		n, err := f.attempt(ctx, ranger, w, h, *off)
		*off += n
		if err == nil && *off < f.ver.Size {
			err = io.ErrUnexpectedEOF // the body ended early
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errNoRange) {
			ranger = nil
			if *off == 0 {
				continue
			}
		} else if !transientDownloadErr(err) {
			return err
		}
		if n > 0 && ranger != nil {
			failures = 0 // progress is kept: a trickling link still finishes
		}
		if failures >= downloadRetries {
			return f.giveUp(err, *off, attempts)
		}
		if ranger == nil && *off > 0 {
			if restart == nil {
				return f.giveUp(err, *off, attempts)
			}
			if rerr := restart(); rerr != nil {
				return rerr
			}
			*off = 0
			h.Reset()
		}
		wait := downloadBackoff(failures)
		failures++
		f.prog.clear()
		f.a.warnf("pyre: download of %s %s at %s of %s; retrying in %s (retry %d of %d, resuming)",
			f.name, describeDownloadErr(err, f.stall), mb(*off), mb(f.ver.Size), wait, failures, downloadRetries)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// attempt runs one request from offset and copies what arrives. The stall
// watchdog covers the wait for response headers as well as the body.
func (f *fetcher) attempt(ctx context.Context, ranger contentRanger, w io.Writer, h hash.Hash, off int64) (int64, error) {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	wd := newWatchdog(f.stall, cancel)
	defer wd.stop()
	var rc io.ReadCloser
	var err error
	if ranger != nil {
		rc, err = ranger.OpenContentRange(actx, f.ver.VersionID, off, 0)
		if c.IsCode(err, c.ErrUnsupported) {
			return 0, errNoRange
		}
	} else {
		rc, _, err = f.api.OpenContent(actx, f.ver.VersionID)
	}
	if err != nil {
		if wd.fired() {
			return 0, errStalled
		}
		return 0, err
	}
	defer rc.Close()
	return copyWatched(io.MultiWriter(w, h), rc, wd, func(n int64) { f.prog.update(off + n) })
}

// watchdog cancels a request when it is not kicked for stall (<= 0: never).
type watchdog struct {
	stall   time.Duration
	timer   *time.Timer
	tripped atomic.Bool
}

func newWatchdog(stall time.Duration, cancel context.CancelFunc) *watchdog {
	wd := &watchdog{stall: stall}
	if stall > 0 {
		wd.timer = time.AfterFunc(stall, func() { wd.tripped.Store(true); cancel() })
	}
	return wd
}

func (wd *watchdog) kick() {
	if wd.timer != nil {
		wd.timer.Reset(wd.stall)
	}
}

func (wd *watchdog) fired() bool { return wd.tripped.Load() }

func (wd *watchdog) stop() {
	if wd.timer != nil {
		wd.timer.Stop()
	}
}

// copyWatched copies src to dst, kicking wd on every read that returns
// bytes. A read failing because wd fired is reported as errStalled.
// onBytes gets the running total.
func copyWatched(dst io.Writer, src io.Reader, wd *watchdog, onBytes func(int64)) (int64, error) {
	buf := make([]byte, 64<<10)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			wd.kick()
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			onBytes(total)
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			if wd.fired() {
				return total, errStalled
			}
			return total, err
		}
	}
}

// transientDownloadErr: worth another attempt (a stall, a dropped or
// refused connection, a retryable hub answer). A digest mismatch or a
// definite refusal is not.
func transientDownloadErr(err error) bool {
	if errors.Is(err, errStalled) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var ce *c.Error
	if errors.As(err, &ce) {
		return client.Retryable(err)
	}
	return true // raw transport error mid-body
}

func describeDownloadErr(err error, stall time.Duration) string {
	if errors.Is(err, errStalled) {
		return fmt.Sprintf("stalled (no bytes for %s)", stall)
	}
	return "failed (" + err.Error() + ")"
}

func (f *fetcher) giveUp(err error, off int64, attempts int) error {
	f.prog.clear()
	what := describeDownloadErr(err, f.stall)
	keep := "the bytes already written to stdout are incomplete; rerun to download again"
	if f.part != "" {
		keep = "the partial download is kept at " + f.part + "; rerun the same command to resume from it"
	}
	return c.Errorf(c.ErrUnavailable, "download of %s %s with %s of %s received (%d attempts): the link to the hub looks slow or stalled; %s",
		f.name, what, mb(off), mb(f.ver.Size), attempts, keep)
}

// downloadToFile downloads into <out>.part, resuming a kept partial file,
// verifies size and digest, and renames it to out.
func (a *App) downloadToFile(ctx context.Context, api c.API, ver c.ArtifactVersion, out string, stall time.Duration) error {
	part := out + ".part"
	pf, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer pf.Close()
	h := sha256.New()
	off, err := hashPartial(pf, h, ver.Size)
	if err != nil {
		return err
	}
	prog := a.newProgress(ver, off)
	if off > 0 {
		a.warnf("pyre: resuming %s from %s of %s (%s)", ver.Filename, mb(off), mb(ver.Size), part)
	}
	f := &fetcher{a: a, api: api, ver: ver, name: displayName(ver), stall: stall, part: part, prog: prog}
	restart := func() error {
		if err := pf.Truncate(0); err != nil {
			return err
		}
		_, err := pf.Seek(0, io.SeekStart)
		return err
	}
	err = f.fetch(ctx, pf, h, &off, restart)
	prog.finish()
	if err != nil {
		return err
	}
	if err := checkDigest(ver, off, h.Sum(nil)); err != nil {
		pf.Close()
		os.Remove(part) // never resume from bytes that do not verify
		return err
	}
	if err := pf.Close(); err != nil {
		return err
	}
	return os.Rename(part, out)
}

// downloadToWriter streams to w (stdout). A failed attempt still resumes
// with Range, since the bytes written so far are kept by the reader.
func (a *App) downloadToWriter(ctx context.Context, api c.API, ver c.ArtifactVersion, w io.Writer, stall time.Duration) error {
	h := sha256.New()
	var off int64
	prog := a.newProgress(ver, 0)
	f := &fetcher{a: a, api: api, ver: ver, name: displayName(ver), stall: stall, prog: prog}
	err := f.fetch(ctx, w, h, &off, nil)
	prog.finish()
	if err == nil {
		err = checkDigest(ver, off, h.Sum(nil))
	}
	return err
}

// hashPartial hashes a kept partial file and positions it at its end. A
// partial longer than the version is discarded.
func hashPartial(pf *os.File, h hash.Hash, size int64) (int64, error) {
	st, err := pf.Stat()
	if err != nil {
		return 0, err
	}
	if st.Size() > size {
		if err := pf.Truncate(0); err != nil {
			return 0, err
		}
		_, err := pf.Seek(0, io.SeekStart)
		return 0, err
	}
	n, err := io.Copy(h, pf)
	if err != nil {
		return 0, err
	}
	return n, nil
}

func displayName(v c.ArtifactVersion) string {
	if v.Filename != "" {
		return v.Filename
	}
	return v.VersionID
}

// ---- progress ----

// progress prints "downloading NAME: 12.3/54.1 MB, 210 KB/s" to stderr:
// on a terminal at most once a second (overwriting one line), otherwise
// at most once every 5 s. Nothing is printed for a download that finishes
// before the first interval, so a fast get stays one line.
type progress struct {
	w     io.Writer
	name  string
	total int64
	tty   bool
	every time.Duration
	now   func() time.Time

	last    time.Time
	lastN   int64
	printed bool // a TTY line is open (needs a newline)
}

func (a *App) newProgress(ver c.ArtifactVersion, start int64) *progress {
	tty := false
	if f, ok := a.Stderr.(*os.File); ok {
		tty = isTTY(f)
	}
	return newProgress(a.Stderr, displayName(ver), ver.Size, start, tty, time.Now)
}

func newProgress(w io.Writer, name string, total, start int64, tty bool, now func() time.Time) *progress {
	every := progressEveryLog
	if tty {
		every = progressEveryTTY
	}
	return &progress{w: w, name: name, total: total, tty: tty, every: every, now: now, last: now(), lastN: start}
}

func (p *progress) update(n int64) {
	if p == nil {
		return
	}
	t := p.now()
	dt := t.Sub(p.last)
	if dt < p.every {
		return
	}
	rate := float64(n-p.lastN) / dt.Seconds()
	line := fmt.Sprintf("downloading %s: %s/%s, %s", p.name, mbNum(n), mb(p.total), rateText(rate))
	if p.tty {
		fmt.Fprintf(p.w, "\r%s\x1b[K", line)
		p.printed = true
	} else {
		fmt.Fprintln(p.w, line)
	}
	p.last, p.lastN = t, n
}

// clear ends an open TTY line before another diagnostic is printed.
func (p *progress) clear() {
	if p != nil && p.printed {
		fmt.Fprintln(p.w)
		p.printed = false
	}
}

func (p *progress) finish() { p.clear() }

func mbNum(n int64) string { return fmt.Sprintf("%.1f", float64(n)/1e6) }

func mb(n int64) string { return mbNum(n) + " MB" }

func rateText(bps float64) string {
	switch {
	case bps >= 1e6:
		return fmt.Sprintf("%.1f MB/s", bps/1e6)
	default:
		return fmt.Sprintf("%.0f KB/s", bps/1e3)
	}
}
