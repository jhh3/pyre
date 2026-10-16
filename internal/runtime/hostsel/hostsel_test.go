package hostsel

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/runtime/termhost"
)

// Without termhost/hostproc linked (macOS/Linux builds without the
// pyre_termhost tag), selecting the termhost fails clearly.
func TestTermhostNotBuilt(t *testing.T) {
	if termhost.HostEntryBuilt() {
		t.Skip("host entry registered")
	}
	_, err := NewKind(KindTermhost, "", filepath.Join(t.TempDir(), "pyre.sock"))
	if !errors.Is(err, termhost.ErrNotBuilt) {
		t.Fatalf("got %v", err)
	}
}
