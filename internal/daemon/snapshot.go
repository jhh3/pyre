package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jhh3/pyre/internal/contracts"
)

// Run snapshot bounds: the most recent snapshotMaxRecords run records (all
// unfinished runs are always included), split into reports of at most
// snapshotChunkBytes of record JSON so each stays far below the transport's
// frame limit.
const (
	snapshotMaxRecords = 1000
	snapshotChunkBytes = 1 << 20
)

// SendRunSnapshot enqueues run_snapshot reports with this node's recent run
// records and their sessions (contract v1.5). The transport calls it when
// the hub's committed report watermark regressed below what it acknowledged
// before (hub restored from an older backup or replaced), so the hub can
// reconcile runs that ended meanwhile and sessions it never saw. It returns
// the number of records sent.
func (d *Daemon) SendRunSnapshot(ctx context.Context, reason string) (int, error) {
	recs, err := d.st.runs(ctx, "")
	if err != nil {
		return 0, err
	}
	if len(recs) > snapshotMaxRecords {
		keep := recs[len(recs)-snapshotMaxRecords:]
		var older []runRecord
		for _, r := range recs[:len(recs)-snapshotMaxRecords] {
			if r.Phase != phaseEnded {
				older = append(older, r)
			}
		}
		recs = append(older, keep...)
	}
	var chunk []contracts.SessionRun
	size, sent := 0, 0
	flush := func() error {
		if len(chunk) == 0 {
			return nil
		}
		if _, err := d.rep.Enqueue(ctx, contracts.ReportRunSnapshot, contracts.RunSnapshot{Records: chunk, Reason: reason}); err != nil {
			return fmt.Errorf("enqueue run snapshot: %w", err)
		}
		sent += len(chunk)
		chunk, size = nil, 0
		return nil
	}
	for _, r := range recs {
		if lr, ok := d.getLive(r.Run.RunID); ok {
			r, _ = lr.snapshot()
		}
		sr := contracts.SessionRun{Session: r.Session, Run: r.Run}
		if sr.Run.NodeID == "" {
			sr.Run.NodeID = d.nodeID
		}
		b, err := json.Marshal(sr)
		if err != nil {
			continue
		}
		if size+len(b) > snapshotChunkBytes {
			if err := flush(); err != nil {
				return sent, err
			}
		}
		chunk = append(chunk, sr)
		size += len(b)
	}
	if err := flush(); err != nil {
		return sent, err
	}
	return sent, nil
}
