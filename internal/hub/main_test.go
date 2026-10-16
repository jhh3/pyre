package hub

import (
	"os"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/storage/sqlitex"
)

// TestMain makes a write that waits on a lock its own goroutine holds fail
// fast (W-dbhygiene): a 1s busy timeout instead of 10s, and a nested
// sqlitex transaction on the same database panics at the offending call.
func TestMain(m *testing.M) {
	sqlitex.SetBusyTimeout(time.Second)
	sqlitex.PanicOnNestedTx(true)
	os.Exit(m.Run())
}
