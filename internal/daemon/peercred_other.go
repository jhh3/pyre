//go:build !linux && !darwin

package daemon

import (
	"errors"
	"net"
)

const peerCredSupported = false

func peerUID(*net.UnixConn) (int, error) { return 0, errors.New("peer credentials unsupported") }
