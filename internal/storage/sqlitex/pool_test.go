package sqlitex

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// A20 soak: with database/sql's default of 2 idle connections, concurrent
// readers (long-polls, node reports) made the pool close and reopen SQLite
// connections all the time, and every new connection re-parsed the schema.
// Concurrent use within the pool size must reuse connections.
func TestPoolReusesConnectionsUnderConcurrency(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	round := func() {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < maxConns; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				rows, err := db.QueryContext(ctx, `SELECT x FROM t`)
				if err != nil {
					t.Error(err)
					return
				}
				for rows.Next() {
				}
				rows.Close()
			}()
		}
		close(start)
		wg.Wait()
	}
	for i := 0; i < 20; i++ {
		round()
	}
	st := db.Stats()
	if st.MaxIdleClosed != 0 {
		t.Fatalf("pool closed %d connections for exceeding the idle limit (opened %d); want reuse", st.MaxIdleClosed, st.OpenConnections)
	}
	if st.OpenConnections > maxConns {
		t.Fatalf("open connections %d > %d", st.OpenConnections, maxConns)
	}
}
