package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
)

// eventWatcher is the slice of *client.Client used to follow hub events.
type eventWatcher interface {
	ListEvents(ctx context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error)
	WatchEventsFiltered(ctx context.Context, req contracts.ListEventsRequest, fn func(contracts.Event) error) error
}

// watchNotes calls notify on every hub note.created event so the Hive
// bridge publishes promptly. It starts at the current head (the bridge's
// own 30 s scan covers anything earlier or missed), re-snapshots on
// reset_required, and gives up quietly if the node credential may not
// watch events (the poll keeps working).
func watchNotes(ctx context.Context, w eventWatcher, notify func(), log *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		// Once the hub pruned its log, a plain list from cursor 0 is
		// reset_required; the head then comes from the reset's snapshot
		// cursor (before the A20 soak this looped without delay).
		head, err := client.HeadCursor(ctx, w)
		if err == nil {
			err = w.WatchEventsFiltered(ctx, contracts.ListEventsRequest{AfterCursor: head, Kinds: []contracts.EventKind{contracts.EvNoteCreated}},
				func(ev contracts.Event) error {
					if ev.Kind == contracts.EvNoteCreated {
						notify()
					}
					return nil
				})
		}
		if ctx.Err() != nil {
			return
		}
		switch contracts.CodeOf(err) {
		case contracts.ErrResetRequired:
			// The watch fell behind retention: restart from the new head
			// (the bridge scan catches up), never in a tight loop.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		case contracts.ErrUnauthorized, contracts.ErrForbidden, contracts.ErrProtocolMismatch, contracts.ErrUnsupported:
			log.Info("hub note events unavailable to this node; hive bridge keeps its periodic scan", "err", err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
