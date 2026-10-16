package client

import (
	"context"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/terminal"
)

// DialTerminal opens a web_terminal descriptor's stream through this hub's
// terminal relay (contracts.PathTerminal) with the client's pinned TLS:
// no ssh to the run's machine. fit (read-only descriptors only) asks the
// node to size the stream to the tmux window (contracts.TerminalFit). The
// descriptor's URL must be a path on this hub; its ticket is sent in the
// first frame and never logged.
func (cl *Client) DialTerminal(ctx context.Context, d c.AttachDescriptor, cols, rows int, fit bool) (*terminal.ClientConn, error) {
	if cl.cfgErr != nil {
		return nil, cl.cfgErr
	}
	if d.Mechanism != c.AttachWebTerminal || d.Ticket == "" {
		return nil, c.Errorf(c.ErrInvalid, "%v", terminal.ErrNotWebTerminal)
	}
	if !strings.HasPrefix(d.URL, "/") || strings.HasPrefix(d.URL, "//") {
		return nil, c.Errorf(c.ErrInvalid, "web terminal URL %q is not a path on this hub", d.URL)
	}
	u := *cl.base
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	default:
		u.Scheme = "ws"
	}
	u.Path = cl.base.Path + d.URL
	u.RawQuery = ""
	return terminal.Dial(ctx, u.String(), d.Ticket, terminal.DialOptions{HTTPClient: cl.hc, Cols: cols, Rows: rows, ReadOnly: d.ReadOnly, Fit: fit})
}
