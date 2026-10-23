package daemon

import (
	"context"
	"net"
	"os"

	"github.com/jhh3/pyre/internal/contracts"
)

// Peer credentials of local-socket connections (Linux SO_PEERCRED, macOS
// LOCAL_PEERCRED). The socket's file mode already limits who can connect;
// endpoints that act with the node's own credential (PathLocalExecStart)
// also check the connecting uid. Platforms without peer credentials
// (Windows: the state directory's ACL) record nothing.

type peerKey struct{}

type peerInfo struct {
	uid int
	err error
}

// withPeer records c's peer uid in ctx (http.Server.ConnContext).
func withPeer(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok || !peerCredSupported {
		return ctx
	}
	uid, err := peerUID(uc)
	return context.WithValue(ctx, peerKey{}, peerInfo{uid: uid, err: err})
}

// checkPeer refuses a connection from another user (root included). It
// fails closed: where peer credentials exist, a request without them (a
// listener other than ServeLocal's) is refused. Platforms without them
// (Windows: the state directory's ACL; FreeBSD and other BSDs, which have
// LOCAL_PEERCRED but are not wired here) rely on the socket's file mode.
func checkPeer(ctx context.Context) error {
	p, ok := ctx.Value(peerKey{}).(peerInfo)
	switch {
	case !ok && peerCredSupported:
		return contracts.Errorf(contracts.ErrForbidden, "no peer credentials for this connection (not the node's local socket)")
	case !ok:
		return nil
	case p.err != nil:
		return contracts.Errorf(contracts.ErrForbidden, "cannot identify the connecting process: %v", p.err)
	case p.uid != os.Getuid():
		return contracts.Errorf(contracts.ErrForbidden, "the connecting process runs as uid %d, not as this node's user (%d)", p.uid, os.Getuid())
	}
	return nil
}
