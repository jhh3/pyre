package daemon

import (
	"context"
	"slices"
	"sync"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

type launchRecorder struct {
	contracts.RuntimeAdapter
	mu   sync.Mutex
	last map[string]contracts.ResolvedLaunch
}

func (r *launchRecorder) Launch(ctx context.Context, l contracts.ResolvedLaunch) (contracts.RunHandle, error) {
	r.mu.Lock()
	r.last[l.RunID] = l
	r.mu.Unlock()
	return r.RuntimeAdapter.Launch(ctx, l)
}

func (r *launchRecorder) get(id string) contracts.ResolvedLaunch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last[id]
}

func TestRunPermissions(t *testing.T) {
	rec := &launchRecorder{RuntimeAdapter: mock.NewAdapter(), last: map[string]contracts.ResolvedLaunch{}}
	n := startNode(t, t.TempDir(), Config{Adapters: []contracts.RuntimeAdapter{rec}, Permissions: PermissionsBypass})
	cwd := t.TempDir()

	// Node default bypass: env tells the adapter, the run advertises it.
	r := n.start(startCmd("ses_p1", "run_p1", contracts.HarnessMock, cwd))
	if !contracts.HasCap(r.Capabilities, contracts.CapPermissionsBypass) || r.AdapterData[adapterDataPermissions] != PermissionsBypass {
		t.Fatalf("bypass run: caps %v data %v", r.Capabilities, r.AdapterData)
	}
	if rec.get("run_p1").Env[PermissionsEnv] != PermissionsBypass {
		t.Fatal("adapter not told about bypass")
	}

	// Per-launch override to ask; the pseudo-arg never reaches the adapter.
	sc := startCmd("ses_p2", "run_p2", contracts.HarnessMock, cwd)
	sc.Args = []string{"--pyre-permissions=ask", "--model", "synthetic"}
	r = n.start(sc)
	if contracts.HasCap(r.Capabilities, contracts.CapPermissionsBypass) || r.AdapterData[adapterDataPermissions] != PermissionsAsk {
		t.Fatalf("ask override: caps %v data %v", r.Capabilities, r.AdapterData)
	}
	l := rec.get("run_p2")
	if l.Env[PermissionsEnv] != PermissionsAsk || slices.ContainsFunc(l.Args, func(a string) bool { return a == "--pyre-permissions=ask" }) ||
		!slices.Equal(l.Args, []string{"--model", "synthetic"}) {
		t.Fatalf("ask override launch: env %q args %v", l.Env[PermissionsEnv], l.Args)
	}

	// An invalid mode is refused.
	bad := startCmd("ses_p3", "run_p3", contracts.HarnessMock, cwd)
	bad.Args = []string{"--pyre-permissions", "yolo"}
	if res := n.do(command(contracts.CmdStartRun, bad)); res.Error == nil {
		t.Fatal("invalid permissions accepted")
	}

	// A resume keeps the previous run's mode when none is requested.
	if got := n.d.runPermissions(contracts.HarnessClaude, "", &contracts.Run{AdapterData: map[string]string{adapterDataPermissions: PermissionsAsk}}); got != PermissionsAsk {
		t.Fatalf("resume mode = %s", got)
	}
	if got := n.d.runPermissions(contracts.HarnessShell, PermissionsBypass, nil); got != PermissionsAsk {
		t.Fatalf("shell mode = %s", got)
	}
}

func TestSplitPermissionsArg(t *testing.T) {
	args, mode, err := splitPermissionsArg([]string{"-m", "x", "--pyre-permissions", "bypass"})
	if err != nil || mode != PermissionsBypass || !slices.Equal(args, []string{"-m", "x"}) {
		t.Fatal(args, mode, err)
	}
	if _, _, err := splitPermissionsArg([]string{"--pyre-permissions"}); err == nil {
		t.Fatal("missing value accepted")
	}
}
