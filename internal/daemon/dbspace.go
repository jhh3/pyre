package daemon

import (
	"context"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// Returning pyred.db's free pages to the filesystem (W-dbspace). pyred
// opens the database in auto_vacuum=INCREMENTAL mode when there is enough
// to win (cmd/pyred, sqlitex.PrepareReclaim); each maintenance pass then
// releases free pages beyond a reserve. The reserve absorbs the transcript
// index's own churn (FTS5 merges free and reuse pages continuously) so the
// file does not shrink and regrow every pass; the bound keeps one pass to a
// few seconds of 8 MiB write transactions.
var (
	dbReclaimKeepBytes int64 = 64 << 20
	dbReclaimMaxBytes  int64 = 256 << 20
)

// reclaimDBSpace runs one bounded incremental vacuum of the node database
// (maintenanceLoop, every 10 minutes). It does nothing until the database
// is in incremental mode.
func (d *Daemon) reclaimDBSpace(ctx context.Context) {
	if _, err := sqlitex.ReclaimFree(ctx, d.db, dbReclaimKeepBytes, dbReclaimMaxBytes); err != nil && ctx.Err() == nil {
		d.noteErr("reclaim database space: %v", err)
	}
}
