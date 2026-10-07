package tmux

import (
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// AttachConfig describes how clients reach this node's tmux server.
type AttachConfig struct {
	NodeID   string
	SSHAlias string // alias other machines use to ssh to this node ("" = no remote attach)
	TmuxBin  string // absolute tmux path on this node
	Socket   string
	TTL      time.Duration // descriptor lifetime (default 5m)
}

// DetachNotes is the human hint shown with tmux attach descriptors.
const DetachNotes = "native tmux attach; detach with Ctrl-b d (the run keeps going)"

// BuildAttach returns a tmux-native attach descriptor for session name.
// A client on this node (or with no node) gets local_tmux; a client on
// another node gets ssh_tmux through cfg.SSHAlias. Read-only is honoured
// with `tmux attach -r`. It never returns secrets.
func BuildAttach(cfg AttachConfig, runID, name string, req contracts.AttachCommand, now time.Time) (contracts.AttachDescriptor, error) {
	readOnly := req.ReadOnly
	wantLocal, wantSSH, wantRO, other := false, false, false, false
	for _, m := range req.Preferred {
		switch m {
		case contracts.AttachLocalTmux:
			wantLocal = true
		case contracts.AttachSSHTmux:
			wantSSH = true
		case contracts.AttachReadOnly:
			wantRO = true
		default:
			other = true
		}
	}
	if len(req.Preferred) > 0 && !wantLocal && !wantSSH && !wantRO && other {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnsupported,
			"attach mechanisms %v unsupported; this adapter offers local_tmux and ssh_tmux", req.Preferred)
	}
	if wantRO && !wantLocal && !wantSSH {
		readOnly = true
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	d := contracts.AttachDescriptor{
		RunID:     runID,
		NodeID:    cfg.NodeID,
		ExpiresAt: now.Add(ttl),
		ReadOnly:  readOnly,
		Notes:     DetachNotes,
	}
	remote := req.ClientNodeID != "" && req.ClientNodeID != cfg.NodeID
	if wantSSH && !wantLocal {
		remote = true
	}
	if !remote {
		d.Mechanism = contracts.AttachLocalTmux
		d.Command = AttachArgv(cfg.TmuxBin, cfg.Socket, name, readOnly)
		return d, nil
	}
	if cfg.SSHAlias == "" {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable,
			"remote attach needs an ssh alias for node %s (not configured)", cfg.NodeID)
	}
	argv, err := RemoteAttachArgv(cfg.SSHAlias, cfg.TmuxBin, cfg.Socket, name, readOnly)
	if err != nil {
		return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrInvalid, "%v", err)
	}
	d.Mechanism = contracts.AttachSSHTmux
	d.Command = argv
	return d, nil
}
