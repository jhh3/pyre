package wsimport

import (
	"context"
	"fmt"

	"github.com/jhh3/pyre/internal/contracts"
)

// ApplyOptions bound an import run.
type ApplyOptions struct {
	// MaxConsecutiveErrors stops the run (default 5); rerunning is safe.
	MaxConsecutiveErrors int
	// Progress is called after each item (optional).
	Progress func(done, total int, it Item, err error)
}

// ApplyResult summarises an import run.
type ApplyResult struct {
	Digest    string   `json:"digest"` // watermark identity of the imported snapshot
	Total     int      `json:"total"`
	Stored    int      `json:"stored"`    // hub returned a stored message (new or idempotent replay)
	Conflicts int      `json:"conflicts"` // same id, different content already on the hub
	Errors    int      `json:"errors"`
	Stopped   bool     `json:"stopped"` // aborted after consecutive errors
	Failed    []string `json:"failed,omitempty"`
	// LastCursor is the highest inbox cursor seen in responses (informational).
	LastCursor int64 `json:"last_cursor"`
}

// Apply sends the plan through api, one request at a time, in plan order
// (parents before replies). Every request carries its deterministic
// MessageID/OperationID, so a rerun stores nothing twice. It never
// acknowledges anything: legacy read state is recorded only as an
// observation in each message header.
func Apply(ctx context.Context, api contracts.MessageAPI, p *Plan, opts ApplyOptions) (ApplyResult, error) {
	if opts.MaxConsecutiveErrors <= 0 {
		opts.MaxConsecutiveErrors = 5
	}
	res := ApplyResult{Digest: p.Report.Digest, Total: len(p.Items)}
	consecutive := 0
	for i, it := range p.Items {
		if err := ctx.Err(); err != nil {
			res.Stopped = true
			return res, err
		}
		v, err := api.SendMessage(ctx, it.Request)
		switch {
		case err == nil:
			res.Stored++
			consecutive = 0
			if v.Cursor > res.LastCursor {
				res.LastCursor = v.Cursor
			}
		case contracts.IsCode(err, contracts.ErrConflict):
			res.Conflicts++
			consecutive = 0
			res.Failed = appendBounded(res.Failed, fmt.Sprintf("%s (%s): conflict: %v", it.ImportID, it.LegacyID, err))
		default:
			res.Errors++
			consecutive++
			res.Failed = appendBounded(res.Failed, fmt.Sprintf("%s (%s): %v", it.ImportID, it.LegacyID, err))
		}
		if opts.Progress != nil {
			opts.Progress(i+1, len(p.Items), it, err)
		}
		if consecutive >= opts.MaxConsecutiveErrors {
			res.Stopped = true
			return res, fmt.Errorf("wsimport: stopped after %d consecutive errors (rerun is idempotent)", consecutive)
		}
	}
	return res, nil
}

func appendBounded(s []string, v string) []string {
	if len(s) >= 50 {
		return s
	}
	return append(s, v)
}
