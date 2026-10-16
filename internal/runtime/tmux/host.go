package tmux

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/sessionhost"
)

// Kind implements sessionhost.Host.
func (s *Server) Kind() string { return "tmux" }

// SocketPath implements sessionhost.Host.
func (s *Server) SocketPath() string { return s.Socket }

// MinVersion is the oldest tmux the adapters support.
var MinVersion = Version{Major: 3, Minor: 2}

// Check implements sessionhost.Host: tmux >= MinVersion is required.
func (s *Server) Check(ctx context.Context) (string, error) {
	v, err := s.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("tmux >= %d.%d required (%v)", MinVersion.Major, MinVersion.Minor, err)
	}
	desc := "tmux " + strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor)
	if !v.AtLeast(MinVersion.Major, MinVersion.Minor) {
		return desc, fmt.Errorf("tmux >= %d.%d required, found %s", MinVersion.Major, MinVersion.Minor, v.Raw)
	}
	return desc, nil
}

// AttachDescriptor implements sessionhost.Host (BuildAttach with this
// server's binary and socket).
func (s *Server) AttachDescriptor(cfg sessionhost.AttachConfig, runID, name string, req contracts.AttachCommand, now time.Time) (contracts.AttachDescriptor, error) {
	return BuildAttach(AttachConfig{NodeID: cfg.NodeID, SSHAlias: cfg.SSHAlias, TmuxBin: s.Bin, Socket: s.Socket, TTL: cfg.TTL}, runID, name, req, now)
}
