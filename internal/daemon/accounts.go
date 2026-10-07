package daemon

import (
	"context"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// AccountBindRequest is the full launch-time account binding request.
type AccountBindRequest struct {
	Harness   contracts.Harness
	Policy    contracts.AccountPolicy
	RunID     string
	SessionID string
	Exclude   []string
}

// AccountBinding is a node-local launch binding. Env carries profile paths
// (CLAUDE_CONFIG_DIR / CODEX_HOME); Unset lists inherited variables the
// launcher must remove so a managed run can neither use another login nor
// fall into API billing.
type AccountBinding struct {
	ProfileDir string
	Env        map[string]string
	Unset      []string
	Assignment *contracts.AccountAssignment
}

// AccountBinder is optionally implemented by an AccountResolver (WP13
// accounts.Service via an adapter); it is preferred over Resolve because it
// carries Unset.
type AccountBinder interface {
	Bind(ctx context.Context, req AccountBindRequest) (AccountBinding, error)
}

// AssignmentChecker re-validates a run's assignment (grant generation)
// before launch and after Recover.
type AssignmentChecker interface {
	CheckAssignment(ctx context.Context, runID string) error
}

// AccountReleaser releases a run's assignment when the run ends.
type AccountReleaser interface {
	Release(runID string)
}

func (d *Daemon) bindAccount(ctx context.Context, h contracts.Harness, p contracts.AccountPolicy, runID, sessionID string) (AccountBinding, error) {
	if b, ok := d.cfg.Accounts.(AccountBinder); ok {
		return b.Bind(ctx, AccountBindRequest{Harness: h, Policy: p, RunID: runID, SessionID: sessionID})
	}
	dir, env, asg, err := d.cfg.Accounts.Resolve(ctx, h, p, runID)
	return AccountBinding{ProfileDir: dir, Env: env, Assignment: asg}, err
}

func (d *Daemon) releaseAccount(runID string) {
	if r, ok := d.cfg.Accounts.(AccountReleaser); ok {
		r.Release(runID)
	}
}

func (d *Daemon) checkAssignmentIf(ctx context.Context, needed bool, runID string) error {
	if !needed {
		return nil
	}
	return d.checkAssignment(ctx, runID)
}

func (d *Daemon) checkAssignment(ctx context.Context, runID string) error {
	if c, ok := d.cfg.Accounts.(AssignmentChecker); ok {
		return c.CheckAssignment(ctx, runID)
	}
	return nil
}

// SessionInfo returns the session record of a session with a run on this
// node (e.g. its project, for connection grant scope checks).
func (d *Daemon) SessionInfo(ctx context.Context, sessionID string) (contracts.Session, bool) {
	rec, ok, err := d.st.latestRunForSession(ctx, sessionID)
	if err != nil || !ok {
		return contracts.Session{}, false
	}
	return rec.Session, true
}

// SessionForNative returns the Pyre session and run on this node whose
// harness-native session ID (a Claude session UUID or Codex thread ID) is
// nativeID, e.g. to attribute a transcript a memory system captured through
// the harness's own hooks. ok is false for sessions Pyre did not launch.
func (d *Daemon) SessionForNative(ctx context.Context, nativeID string) (sessionID, runID string, projectID *string, ok bool) {
	if nativeID == "" {
		return "", "", nil, false
	}
	rec, ok, err := d.st.runForNative(ctx, nativeID)
	if err != nil || !ok {
		return "", "", nil, false
	}
	return rec.Run.SessionID, rec.Run.RunID, rec.Session.ProjectID, true
}

// ---- run control for account fallback (accounts.RunController) ----

// RunInfo is the fallback planner's view of a run.
type RunInfo struct {
	SessionID    string
	RunID        string
	Harness      contracts.Harness
	Capabilities []string
	NativeRef    *contracts.NativeRef
	AccountID    string
	TurnInFlight bool
	Live         bool
}

// CurrentRun describes the session's current run on this node.
func (d *Daemon) CurrentRun(ctx context.Context, sessionID string) (RunInfo, error) {
	rec, ok, err := d.st.latestRunForSession(ctx, sessionID)
	if err != nil {
		return RunInfo{}, err
	}
	if !ok {
		return RunInfo{}, contracts.Errorf(contracts.ErrNotFound, "session has no run on this node").WithRef(contracts.KindSession, sessionID)
	}
	if lr, live := d.getLive(rec.Run.RunID); live {
		rec, _ = lr.snapshot()
	}
	r := rec.Run
	return RunInfo{SessionID: r.SessionID, RunID: r.RunID, Harness: r.Harness, Capabilities: r.Capabilities, NativeRef: r.NativeRef,
		AccountID: r.AccountID, TurnInFlight: r.State == contracts.RunRunning, Live: rec.Phase == phaseLive}, nil
}

// QuiesceRun stops a run (as the adapter supports) and waits until it has
// ended or ctx is done. turnInFlight reports whether a turn was running.
func (d *Daemon) QuiesceRun(ctx context.Context, sessionID, runID string) (exited, turnInFlight bool, detail string, err error) {
	lr, err := d.liveTarget(ctx, sessionID, runID)
	if err != nil {
		if contracts.IsCode(err, contracts.ErrConflict) { // already not live
			return true, false, "run already ended", nil
		}
		return false, false, "", err
	}
	rec, _ := lr.snapshot()
	turnInFlight = rec.Run.State == contracts.RunRunning
	if !contracts.HasCap(rec.Run.Capabilities, contracts.CapStop) {
		return false, turnInFlight, "adapter cannot stop runs", contracts.Errorf(contracts.ErrUnsupported, "adapter %s cannot stop runs", lr.adapter.Name())
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	_, err = lr.adapter.Stop(cctx, *rec.Handle)
	cancel()
	if err != nil {
		return false, turnInFlight, "", err
	}
	for {
		d.PollNow(ctx, rec.Run.RunID)
		if _, live := d.getLive(rec.Run.RunID); !live {
			return true, turnInFlight, "stopped for account fallback", nil
		}
		select {
		case <-ctx.Done():
			return false, turnInFlight, "timed out waiting for exit", nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ContinuationRequest asks for a successor run bound to an account.
type ContinuationRequest struct {
	SessionID     string
	PreviousRunID string
	AccountID     string
	Kind          contracts.ContinuationKind // native_resume | handoff
	Reason        string
}

// StartContinuation starts a successor run in the same session, bound to
// AccountID via an explicit policy. Native resume passes the previous
// native reference; handoff starts a reconstructed continuation with a
// handoff-note request. The interrupted instruction is never replayed.
// The successor stays in the same Pyre session (lineage via previous run);
// creating a linked successor session needs the hub and is not done here.
func (d *Daemon) StartContinuation(ctx context.Context, c ContinuationRequest) (string, error) {
	prev, ok, err := d.st.run(ctx, c.PreviousRunID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", contracts.Errorf(contracts.ErrNotFound, "previous run not on this node").WithRef(contracts.KindRun, c.PreviousRunID)
	}
	tok, _, _ := d.st.tokenForSession(ctx, c.SessionID)
	runID := contracts.NewID(contracts.PrefixRun)
	prevRun := prev.Run
	sc := contracts.StartRunCommand{Session: prev.Session, RunID: runID, Cwd: prevRun.Cwd,
		AccountPolicy: contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: c.AccountID},
		SessionToken:  tok, PreviousRun: &prevRun, AllowHandoff: c.Kind == contracts.ContinuationHandoff}
	cmd := contracts.NodeCommand{CommandID: contracts.NewID(contracts.PrefixCommand), Kind: contracts.CmdResume,
		Actor: contracts.Principal{Kind: contracts.PrincipalSystem, ID: "account-fallback"}, IssuedAt: d.now()}
	res, err := d.startRun(ctx, cmd, sc, true)
	if err != nil {
		return "", err
	}
	if res.Outcome == contracts.OutcomeFailed || res.Outcome == contracts.OutcomeUnsupported {
		return "", contracts.Errorf(contracts.ErrConflict, "successor run not started")
	}
	return runID, nil
}

// ActiveRun is a supervised run as seen by configuration/updates (WP14):
// its environment generation and pinned executable.
type ActiveRun struct {
	RunID        string
	SessionID    string
	ProjectID    string
	Harness      contracts.Harness
	GenerationID string
	Executable   string
	Cwd          string
	State        contracts.RunState
	NativeID     string // harness-native session (Claude session / Codex thread)
	ProfileDir   string // bound harness config home, "" for the default
}

// ActiveRuns lists runs that are live, launching, or orphaned (their
// generation files and executables must not be pruned).
func (d *Daemon) ActiveRuns(ctx context.Context) ([]ActiveRun, error) {
	recs, err := d.st.runs(ctx, `phase IN (?,?,?)`, phaseLive, phaseLaunching, phaseOrphaned)
	if err != nil {
		return nil, err
	}
	out := make([]ActiveRun, 0, len(recs))
	for _, r := range recs {
		if lr, ok := d.getLive(r.Run.RunID); ok {
			r, _ = lr.snapshot()
		}
		a := ActiveRun{RunID: r.Run.RunID, SessionID: r.Run.SessionID, Harness: r.Run.Harness,
			GenerationID: r.Run.EnvGenerationID, Executable: r.Run.Runtime.Executable, Cwd: r.Run.Cwd, State: r.Run.State}
		if r.Run.NativeRef != nil {
			a.NativeID = r.Run.NativeRef.ID
		}
		if r.Handle != nil {
			a.ProfileDir = r.Handle.Data[HandleProfileDir]
		}
		if r.Session.ProjectID != nil {
			a.ProjectID = *r.Session.ProjectID
		}
		out = append(out, a)
	}
	return out, nil
}

// HarnessInfo returns the adapter probe results (cached, bounded).
func (d *Daemon) HarnessInfo(ctx context.Context) []contracts.HarnessInfo { return d.harnessInfo(ctx) }

// Inventory returns the daemon's built-in inventory (harnesses, pyred and
// tmux versions, config roots, integration presence). Integrations with
// richer inventories (WP14 updates.Inventory) merge into it.
func (d *Daemon) Inventory(ctx context.Context) contracts.NodeInventory {
	v, _ := d.actionInventory(ctx, nil)
	return v.(contracts.NodeInventory)
}
