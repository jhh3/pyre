package cli

import (
	"context"
	"slices"
	"testing"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon"
)

type startCapture struct {
	c.API
	req c.StartSessionRequest
}

func (s *startCapture) StartSession(_ context.Context, r c.StartSessionRequest) (c.StartSessionResponse, error) {
	s.req = r
	return c.StartSessionResponse{}, nil
}

// pyre start --permissions / MCP start_session permissions travel to the
// node as its per-launch override arg; "" leaves the node default.
func TestStartPermissionsOverride(t *testing.T) {
	if permissionsArg != daemon.PermissionsArg {
		t.Fatalf("CLI %q != node %q", permissionsArg, daemon.PermissionsArg)
	}
	api := &startCapture{}
	o := &Ops{API: api, Self: Identity{NodeID: "node_synth-mac"}}
	ctx := context.Background()
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic", Permissions: "bypass"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.req.Args, []string{"--pyre-permissions=bypass"}) {
		t.Fatalf("args %v", api.req.Args)
	}
	if _, err := o.StartSession(ctx, StartParams{Harness: "codex", Cwd: "/synthetic"}); err != nil || len(api.req.Args) != 0 {
		t.Fatalf("default: args %v err %v", api.req.Args, err)
	}
	if _, err := o.StartSession(ctx, StartParams{Harness: "claude", Cwd: "/synthetic", Permissions: "yolo"}); err == nil {
		t.Fatal("invalid permissions accepted")
	}
}

func TestStartPermissionsFlag(t *testing.T) {
	h := newHub(t)
	owner := newEnv(t, h)
	expectCode(t, run(t, owner, nil, "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/x", "--permissions", "maybe"), ExitUsage)
	expectCode(t, run(t, owner, nil, "start", "--node", "synth-mac", "--harness", "claude", "--cwd", "/home/synth/x", "--permissions", "bypass"), 0)
}
