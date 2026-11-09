package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/sysstats"
)

// prepareReclaim switches pyred.db to incremental auto-vacuum so the
// daemon's maintenance can return pages freed by retention to the
// filesystem (W-dbspace). A database with little free space is left as it
// is until a later start finds enough to win (freed pages are reused
// meanwhile); converting one runs a full VACUUM before the daemon serves,
// about a second per few hundred MB of live data. A failure is logged,
// never fatal.
func prepareReclaim(ctx context.Context, db *sql.DB, path string, log *slog.Logger) {
	rep, err := sqlitex.PrepareReclaim(ctx, db, path, sqlitex.ReclaimPolicy{DiskAvail: sysstats.DiskAvail})
	switch {
	case err != nil:
		log.Warn("pyred.db: switching to incremental vacuum failed", "err", err)
	case rep.Vacuumed:
		log.Info("pyred.db: vacuumed once to enable incremental vacuum", "before_mb", rep.Before.Pages*rep.Before.PageSize>>20,
			"after_mb", rep.After.Pages*rep.After.PageSize>>20, "took", rep.Took.Round(time.Millisecond).String())
	case rep.Skipped != "":
		log.Debug("pyred.db: not switched to incremental vacuum yet", "reason", rep.Skipped)
	}
}
