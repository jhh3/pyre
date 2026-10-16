package daemon

import (
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/hostsel"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
	"github.com/jhh3/pyre/internal/runtime/termhost"
)

// termhostFor returns the Pyre session host serving a run when this node
// uses the termhost (Windows, or PYRE_SESSION_HOST=termhost); nil means the
// run lives in tmux and the tmux helpers apply. Run handles record the
// termhost socket path itself in TmuxSocket.
func termhostFor(h contracts.RunHandle) sessionhost.Host {
	if hostsel.Kind() != hostsel.KindTermhost || h.TmuxSocket == "" {
		return nil
	}
	host, err := termhost.New(h.TmuxSocket)
	if err != nil {
		return nil
	}
	return host
}
