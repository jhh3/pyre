package digest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Source is the subset of contracts.API the digest reads. Every call is a
// read; the digest never changes hub state.
type Source interface {
	ListEvents(ctx context.Context, req c.ListEventsRequest) (c.ListEventsResponse, error)
	ListSessions(ctx context.Context, req c.ListSessionsRequest) (c.Page[c.SessionView], error)
	ListAttention(ctx context.Context, req c.ListAttentionRequest) (c.Page[c.AttentionItem], error)
	ReadSession(ctx context.Context, req c.ReadSessionRequest) (c.ReadSessionResponse, error)
}

// Options bound a Fetch.
type Options struct {
	// AfterCursor, when > 0, is the exclusive event cursor to start after
	// (the owner's "last seen"). Otherwise Since is used.
	AfterCursor int64
	// Since is the time cutoff. With AfterCursor it is the time of that
	// cursor (used to filter transcripts and attention); without it, the
	// first event ingested after Since is located by binary search.
	Since time.Time
	// Label describes the cutoff for display ("last seen", "2h").
	Label string
	Now   time.Time
	// MaxEvents bounds how many events are read (default 20000).
	MaxEvents int
	// TranscriptTail is how many entries are read per session (default 200).
	TranscriptTail int
	// Parallel bounds concurrent transcript reads (default 4).
	Parallel int
	// CallTimeout bounds each transcript read (default 5s); a machine that
	// does not answer costs one warning, not the digest.
	CallTimeout time.Duration
	// NoTranscripts skips transcript reads (summaries fall back to
	// attention, artifacts and state).
	NoTranscripts bool
}

func (o Options) withDefaults() Options {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	if o.MaxEvents <= 0 {
		o.MaxEvents = 20000
	}
	if o.TranscriptTail <= 0 {
		o.TranscriptTail = 200
	}
	if o.Parallel <= 0 {
		o.Parallel = 4
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 5 * time.Second
	}
	return o
}

// Fetch reads the hub and builds the digest.
func Fetch(ctx context.Context, src Source, o Options) (Digest, error) {
	in, err := Gather(ctx, src, o)
	if err != nil {
		return Digest{}, err
	}
	return Build(in), nil
}

// Gather reads everything Build needs.
func Gather(ctx context.Context, src Source, o Options) (Input, error) {
	o = o.withDefaults()
	in := Input{Now: o.Now, SinceLabel: o.Label, Since: o.Since, SinceCursor: o.AfterCursor, Transcripts: map[string]c.ReadSessionResponse{}}

	head, oldest, err := bounds(ctx, src)
	if err != nil {
		return in, err
	}
	in.HeadCursor = head
	switch {
	case o.AfterCursor > head:
		// A cursor from another hub or a restored backup: fall back to time.
		in.SinceCursor = 0
		fallthrough
	case o.AfterCursor <= 0:
		cur, err := cursorAt(ctx, src, o.Since, oldest, head)
		if err != nil {
			return in, err
		}
		in.SinceCursor = cur
	}
	if in.SinceCursor < oldest-1 {
		in.Truncated = true // history before the cutoff was pruned
		in.SinceCursor = oldest - 1
	}

	// Events after the cutoff, bounded.
	after := in.SinceCursor
	for len(in.Events) < o.MaxEvents && after < head {
		resp, err := src.ListEvents(ctx, c.ListEventsRequest{AfterCursor: after, Limit: 1000, Kinds: EventKinds})
		if err != nil {
			if c.IsCode(err, c.ErrResetRequired) {
				in.Truncated = true
				break
			}
			return in, err
		}
		for _, ev := range resp.Events {
			if ev.Cursor > after {
				in.Events = append(in.Events, ev)
			}
		}
		next := resp.NextCursor
		if len(resp.Events) > 0 && resp.Events[len(resp.Events)-1].Cursor > next {
			next = resp.Events[len(resp.Events)-1].Cursor
		}
		if next <= after || len(resp.Events) == 0 {
			break
		}
		after = next
	}
	if len(in.Events) >= o.MaxEvents && after < head {
		in.Truncated = true
	}

	sessions, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.SessionView], error) {
		return src.ListSessions(ctx, c.ListSessionsRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: 200}, IncludeArchived: true})
	})
	if err != nil {
		return in, err
	}
	in.Sessions = sessions
	att, err := listAll(ctx, func(ctx context.Context, cur string) (c.Page[c.AttentionItem], error) {
		return src.ListAttention(ctx, c.ListAttentionRequest{PageRequest: c.PageRequest{Cursor: cur, Limit: 200},
			States: []c.AttentionState{c.AttnOpen}})
	})
	if err != nil {
		return in, err
	}
	in.Attention = att

	if !o.NoTranscripts {
		in.Transcripts, in.Warnings = readTranscripts(ctx, src, o, candidates(in))
	}
	return in, nil
}

// candidates are the sessions whose transcripts are read: those with
// events after the cutoff, plus live agent sessions (a long turn may have
// produced no event while the owner was away).
func candidates(in Input) []c.SessionView {
	active := map[string]bool{}
	for _, ev := range in.Events {
		if ev.SessionID != "" {
			active[ev.SessionID] = true
		}
	}
	var out []c.SessionView
	for _, sv := range in.Sessions {
		if sv.Run == nil || sv.Session.Harness == c.HarnessShell {
			continue
		}
		live := !sv.Run.State.Terminal()
		ended := sv.Run.EndedAt != nil && sv.Run.EndedAt.After(in.Since)
		if active[sv.Session.SessionID] || (live && !sv.Session.Archived) || ended {
			out = append(out, sv)
		}
	}
	return out
}

func readTranscripts(ctx context.Context, src Source, o Options, ss []c.SessionView) (map[string]c.ReadSessionResponse, []string) {
	out := map[string]c.ReadSessionResponse{}
	var warn []string
	var mu sync.Mutex
	sem := make(chan struct{}, o.Parallel)
	var wg sync.WaitGroup
	for _, sv := range ss {
		wg.Add(1)
		sem <- struct{}{}
		go func(sv c.SessionView) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, o.CallTimeout)
			defer cancel()
			resp, err := src.ReadSession(cctx, c.ReadSessionRequest{SessionID: sv.Session.SessionID, Tail: true, Limit: o.TranscriptTail})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if c.IsCode(err, c.ErrNotFound) {
					return // no run to read (e.g. a launch that failed): nothing to summarize
				}
				if errors.Is(err, context.DeadlineExceeded) {
					err = fmt.Errorf("no answer within %s", o.CallTimeout)
				}
				warn = append(warn, fmt.Sprintf("transcript of %q unavailable: %v", sv.Session.Title, err))
				return
			}
			out[sv.Session.SessionID] = resp
		}(sv)
	}
	wg.Wait()
	return out, warn
}

// bounds returns the head cursor and the oldest retained cursor.
func bounds(ctx context.Context, src Source) (head, oldest int64, err error) {
	resp, err := src.ListEvents(ctx, c.ListEventsRequest{AfterCursor: 0, Limit: 1})
	switch {
	case err == nil:
		head = resp.HeadCursor
		oldest = 1
		if len(resp.Events) > 0 {
			oldest = resp.Events[0].Cursor
		}
		return head, oldest, nil
	case c.IsCode(err, c.ErrResetRequired):
		ce := c.AsError(err)
		return detailInt(ce, "snapshot_cursor"), detailInt(ce, "oldest_cursor"), nil
	}
	return 0, 0, err
}

func detailInt(ce *c.Error, k string) int64 {
	if ce == nil || ce.Details == nil {
		return 0
	}
	switch v := ce.Details[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// cursorAt returns the cursor just before the first event ingested after
// t (binary search over [oldest, head]; about log2(head-oldest) reads).
func cursorAt(ctx context.Context, src Source, t time.Time, oldest, head int64) (int64, error) {
	if head <= 0 {
		return 0, nil
	}
	if oldest <= 0 {
		oldest = 1
	}
	lo, hi := oldest-1, head // invariant: events <= lo are before t; the answer is in [lo, hi]
	for lo < hi {
		mid := lo + (hi-lo)/2
		resp, err := src.ListEvents(ctx, c.ListEventsRequest{AfterCursor: mid, Limit: 1})
		if err != nil {
			if c.IsCode(err, c.ErrResetRequired) {
				lo = mid + 1
				continue
			}
			return 0, err
		}
		if len(resp.Events) == 0 {
			hi = mid
			continue
		}
		ev := resp.Events[0]
		if ev.IngestedAt.After(t) {
			hi = mid
		} else {
			lo = ev.Cursor
		}
	}
	return lo, nil
}

func listAll[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) (c.Page[T], error)) ([]T, error) {
	var out []T
	cur := ""
	for i := 0; i < 50; i++ {
		p, err := fetch(ctx, cur)
		if err != nil {
			return out, err
		}
		out = append(out, p.Items...)
		if p.NextCursor == "" || p.NextCursor == cur {
			break
		}
		cur = p.NextCursor
	}
	return out, nil
}
