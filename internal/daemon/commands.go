package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/internal/terminal"
)

// HandleCommand executes one hub command (transport.CommandHandler). The
// NodeLink has already recorded and deduplicated it by CommandID; handlers
// are additionally idempotent by object ID (run ID, message ID) so a
// re-issued command can never start a second run or store a message twice.
// Long work (launch, async actions) returns an interim "delivered" result
// and reports the final CommandResult durably.
func (d *Daemon) HandleCommand(ctx context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error) {
	res, err := d.dispatch(ctx, cmd)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if ctx.Err() != nil {
				return contracts.CommandResult{}, err // NodeLink reports indeterminate
			}
		}
		return d.failResult(cmd, err), nil
	}
	res.CommandID, res.Kind = cmd.CommandID, cmd.Kind
	if res.At.IsZero() {
		res.At = d.now()
	}
	if res.Outcome == "" {
		res.Outcome = contracts.OutcomeSucceeded
	}
	return res, nil
}

func (d *Daemon) failResult(cmd contracts.NodeCommand, err error) contracts.CommandResult {
	ce := contracts.AsError(err)
	ce.Message = redact(ce.Message)
	outcome := contracts.OutcomeFailed
	switch ce.Code {
	case contracts.ErrUnsupported:
		outcome = contracts.OutcomeUnsupported
	case contracts.ErrOutcomeIndeterminate:
		outcome = contracts.OutcomeIndeterminate
	}
	return contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: outcome, Error: ce, At: d.now()}
}

func okResult(outcome contracts.CommandOutcome, v any) (contracts.CommandResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return contracts.CommandResult{}, err
	}
	return contracts.CommandResult{Outcome: outcome, Result: b}, nil
}

func decode[T any](raw contracts.RawJSON) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, contracts.Errorf(contracts.ErrInvalid, "command payload is empty")
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, contracts.Errorf(contracts.ErrInvalid, "decode command payload: %v", err)
	}
	return v, nil
}

func (d *Daemon) dispatch(ctx context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error) {
	switch cmd.Kind {
	case contracts.CmdStartRun:
		sc, err := decode[contracts.StartRunCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.startRun(ctx, cmd, sc, false)
	case contracts.CmdResume:
		rc, err := decode[contracts.ResumeCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.startRun(ctx, cmd, rc.StartRunCommand, true)
	case contracts.CmdDeliver:
		dc, err := decode[contracts.DeliverCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.deliver(ctx, dc)
	case contracts.CmdCancelDelivery:
		p, err := decode[struct {
			MessageID string `json:"message_id"`
		}](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		m, err := d.engine.Cancel(ctx, p.MessageID)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return okResult(contracts.OutcomeSucceeded, contracts.DeliveryReceipt{MessageID: p.MessageID, SessionID: m.SessionID, NodeID: d.nodeID,
			State: m.State, Evidence: contracts.EvidenceNone, Detail: m.Reason, ObservedAt: d.now()})
	case contracts.CmdInterrupt, contracts.CmdStop:
		rc, err := decode[contracts.RunCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.runControl(ctx, cmd, rc)
	case contracts.CmdAttach:
		ac, err := decode[contracts.AttachCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		desc, err := d.attach(ctx, ac)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return okResult(contracts.OutcomeSucceeded, desc)
	case terminal.CmdTerminalOpen:
		tc, err := decode[contracts.TerminalOpenCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.terminalOpen(ctx, tc)
	case contracts.CmdAnswer:
		ac, err := decode[contracts.AnswerCommand](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.answer(ctx, cmd, ac)
	case contracts.CmdReadTranscript:
		rr, err := decode[contracts.ReadSessionRequest](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		resp, err := d.readTranscript(ctx, rr)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return okResult(contracts.OutcomeSucceeded, resp)
	case contracts.CmdLeaseChanged:
		l, err := decode[contracts.InputLease](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.leaseChanged(ctx, l)
	case contracts.CmdAction:
		ar, err := decode[contracts.NodeActionRequest](cmd.Payload)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		return d.invokeAction(ctx, cmd, ar)
	}
	return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "command kind %q is not supported by this pyred", cmd.Kind)
}

// ---- start / resume ----

// WaitLaunches blocks until in-flight asynchronous launches and actions
// finish.
func (d *Daemon) WaitLaunches() { d.asyncWG.Wait() }

func (d *Daemon) startRun(ctx context.Context, cmd contracts.NodeCommand, sc contracts.StartRunCommand, resume bool) (contracts.CommandResult, error) {
	if !contracts.ValidID(sc.RunID, contracts.PrefixRun) {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid run_id %q", sc.RunID)
	}
	if !contracts.ValidID(sc.Session.SessionID, contracts.PrefixSession) {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrInvalid, "invalid session_id %q", sc.Session.SessionID)
	}
	// Idempotent by run ID: an existing record is returned, never relaunched.
	if rec, ok, err := d.st.run(ctx, sc.RunID); err != nil {
		return contracts.CommandResult{}, err
	} else if ok {
		if lr, live := d.getLive(sc.RunID); live {
			rec, _ = lr.snapshot()
		}
		if rec.Run.SessionID != sc.Session.SessionID {
			return contracts.CommandResult{}, contracts.Errorf(contracts.ErrConflict, "run %s belongs to session %s", sc.RunID, rec.Run.SessionID).WithRef(contracts.KindRun, sc.RunID)
		}
		return okResult(contracts.OutcomeSucceeded, rec.Run)
	}
	// Never two current runs for one session on this node.
	if prev, ok, err := d.st.latestRunForSession(ctx, sc.Session.SessionID); err != nil {
		return contracts.CommandResult{}, err
	} else if ok && prev.Phase != phaseEnded {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrConflict, "session already has run %s (%s) on this node; stop it before starting another", prev.Run.RunID, prev.Run.State).WithRef(contracts.KindRun, prev.Run.RunID)
	}
	harness := sc.Session.Harness
	adapter := d.byHarness[harness]
	if adapter == nil {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "harness %q is not available on this node", harness)
	}
	probe := d.refreshProbes(ctx)[harness]
	if !probe.Available {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnavailable, "harness %q unavailable on this node: %s", harness, probe.Reason)
	}
	if d.cfg.MaxRuns > 0 && d.activeRuns() >= d.cfg.MaxRuns {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrCapacityExhausted, "node is at its run capacity (%d)", d.cfg.MaxRuns)
	}
	if sc.Checkout == nil || sc.Checkout.Mode == contracts.CheckoutWorktree {
		if _, err := validateDir(sc.Cwd); err != nil {
			return contracts.CommandResult{}, err
		}
	}
	if sc.EnvProfile != "" && d.cfg.Environments == nil {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "environment profile %q requested but no environment manager is configured on this node", sc.EnvProfile)
	}

	// Account binding before launch (fixed for the run). Only harnesses
	// with a subscription provider (claude, codex) bind accounts; shells
	// and the mock never do.
	var binding AccountBinding
	args, permReq, err := splitPermissionsArg(sc.Args)
	if err != nil {
		return contracts.CommandResult{}, err
	}
	sc.Args = args
	needsAccount := contracts.ProviderFor(harness) != ""
	if !needsAccount && (sc.AccountPolicy.Mode == contracts.AccountExplicit || sc.AccountPolicy.AccountID != "") {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported,
			"harness %s has no subscription provider; an explicit account cannot apply", harness)
	}
	if needsAccount {
		b, err := d.bindAccount(ctx, harness, sc.AccountPolicy, sc.RunID, sc.Session.SessionID)
		if err != nil {
			return contracts.CommandResult{}, err
		}
		binding = b
	}
	handedOff := false // the async launch owns the assignment from here
	defer func() {
		if !handedOff && needsAccount {
			d.releaseAccount(sc.RunID)
		}
	}()
	if needsAccount {
		if err := d.checkAssignment(ctx, sc.RunID); err != nil {
			return contracts.CommandResult{}, err
		}
	}
	assignment := binding.Assignment

	cont := contracts.ContinuationFresh
	var resumeRef *contracts.NativeRef
	prevRunID := ""
	if resume {
		prev := sc.PreviousRun
		if prev == nil {
			return contracts.CommandResult{}, contracts.Errorf(contracts.ErrInvalid, "resume requires previous_run")
		}
		if prev.Harness != harness || contracts.ProviderFor(prev.Harness) != contracts.ProviderFor(harness) {
			return contracts.CommandResult{}, contracts.Errorf(contracts.ErrInvalid, "resume never crosses harness/provider (%s -> %s)", prev.Harness, harness)
		}
		if lr, ok := d.getLive(prev.RunID); ok {
			if r, _ := lr.snapshot(); !r.Run.State.Terminal() {
				return contracts.CommandResult{}, contracts.Errorf(contracts.ErrConflict, "previous run %s is still live on this node", prev.RunID).WithRef(contracts.KindRun, prev.RunID)
			}
		}
		prevRunID = prev.RunID
		// Lineage records prev; the native continuation comes from the last
		// run that actually ran (a resume whose launch failed has no thread).
		basis := d.resumeBasis(ctx, *prev)
		same := assignment == nil && basis.AccountID == "" || assignment != nil && assignment.AccountID == basis.AccountID
		kind, ref, rerr := adapter.ResumeSpec(ctx, basis, same)
		if rerr == nil && kind != contracts.ContinuationNativeResume {
			// An adapter that can only hand off is a native-resume refusal:
			// the caller's AllowHandoff decides, and the handoff carries the
			// lineage prompt, exactly like ErrUnsupported.
			rerr = contracts.Errorf(contracts.ErrUnsupported, "no native thread to resume on this account")
		}
		switch {
		case rerr == nil && kind == contracts.ContinuationNativeResume:
			cont, resumeRef = kind, ref
		case rerr == nil:
			cont = kind
		case contracts.IsCode(rerr, contracts.ErrUnsupported) && sc.AllowHandoff:
			cont = contracts.ContinuationHandoff
			sc.InitialPrompt = handoffPrompt(sc, basis) + strings.TrimSpace("\n\n"+sc.InitialPrompt)
		case contracts.IsCode(rerr, contracts.ErrUnsupported):
			return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "native resume unsupported for run %s and handoff not allowed: %v", prev.RunID, rerr)
		default:
			return contracts.CommandResult{}, rerr
		}
	}

	now := d.now()
	run := contracts.Run{RunID: sc.RunID, SessionID: sc.Session.SessionID, NodeID: d.nodeID, Adapter: adapter.Name(), Harness: harness,
		Cwd: expandHome(sc.Cwd), State: contracts.RunStarting, StateReason: "launching", Freshness: d.freshness(false, ""),
		StartedAt: now, Capabilities: probe.Capabilities, PreviousRunID: prevRunID, ContinuationKind: cont,
		Runtime:     contracts.RuntimeVersion{Harness: harness, Version: probe.Version, Executable: probe.Executable},
		AdapterData: map[string]string{}}
	if assignment != nil {
		assignment.RunID, assignment.SessionID, assignment.NodeID = sc.RunID, sc.Session.SessionID, d.nodeID
		if assignment.AssignedAt.IsZero() {
			assignment.AssignedAt = now
		}
		run.AccountID, run.AssignmentID = assignment.AccountID, assignment.AssignmentID
	} else if harness != contracts.HarnessShell {
		run.AdapterData["pyre.account"] = string(contracts.AccountAmbient)
	}
	if harness != contracts.HarnessShell {
		var prevRun *contracts.Run
		if resume {
			prevRun = sc.PreviousRun
		}
		perm := d.runPermissions(harness, permReq, prevRun)
		run.AdapterData[adapterDataPermissions] = perm
		if perm == PermissionsBypass {
			run.Capabilities = append(append([]string(nil), run.Capabilities...), contracts.CapPermissionsBypass)
		}
	}
	session := sc.Session
	session.CurrentRunID = sc.RunID
	rec := runRecord{Session: session, Run: run, Phase: phaseLaunching, CreatedAt: now}
	rec.Fingerprint = fingerprint(run)
	err = sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
		if err := putRunTx(ctx, tx, rec, now); err != nil {
			return err
		}
		if err := putSecretTx(ctx, tx, sc.RunID, sc.Session.SessionID, sc.SessionToken, now); err != nil {
			return err
		}
		if assignment != nil {
			if _, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportAssignment, now, *assignment); err != nil {
				return err
			}
		}
		_, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportRun, now, run)
		return err
	})
	if err != nil {
		return contracts.CommandResult{}, err
	}
	d.rep.Kick()

	handedOff = true
	// The launch goroutine mutates its record; give it its own map.
	ad := make(map[string]string, len(rec.Run.AdapterData))
	for k, v := range rec.Run.AdapterData {
		ad[k] = v
	}
	rec.Run.AdapterData = ad
	d.asyncWG.Add(1)
	go func() {
		defer d.asyncWG.Done()
		d.launch(context.WithoutCancel(ctx), cmd, sc, rec, adapter, binding, resumeRef)
	}()
	return okResult(contracts.OutcomeDelivered, run)
}

// resumeBasis returns the run a resume continues natively: prev itself, or,
// when prev never ran (its launch failed, e.g. no eligible account during a
// cooldown, so it has no native reference), the nearest earlier run in its
// lineage that this node recorded. Bounded; prev is returned when nothing
// better is known.
func (d *Daemon) resumeBasis(ctx context.Context, prev contracts.Run) contracts.Run {
	cur := prev
	for i := 0; i < 8; i++ {
		if cur.NativeRef != nil || cur.State != contracts.RunFailed || cur.PreviousRunID == "" || cur.AccountID != "" {
			return cur
		}
		rec, ok, err := d.st.run(ctx, cur.PreviousRunID)
		if err != nil || !ok || rec.Run.Harness != prev.Harness {
			return cur
		}
		cur = rec.Run
	}
	return cur
}

func handoffPrompt(sc contracts.StartRunCommand, prev contracts.Run) string {
	var b strings.Builder
	b.WriteString("Pyre handoff: this session (" + sc.Session.SessionID + ") continues previous run " + prev.RunID)
	if prev.NodeID != "" {
		b.WriteString(" on node " + prev.NodeID)
	}
	b.WriteString(". Native resume is unavailable, so this is a reconstructed continuation, not the same conversation thread.\n")
	if sc.Session.Lineage.HandoffNoteID != "" {
		b.WriteString("Read the handoff note first: pyre note get " + sc.Session.Lineage.HandoffNoteID + "\n")
	}
	b.WriteString("Review the previous run: pyre session read " + sc.Session.SessionID + " --run " + prev.RunID + " --tail\n")
	b.WriteString("Do not repeat actions whose outcome is unclear without checking their effects first. ")
	b.WriteString("Then publish a short handoff note of what you reconstructed: pyre note create --kind handoff --session " + sc.Session.SessionID)
	return b.String()
}

// launch performs the slow part of start_run and reports the final result.
func (d *Daemon) launch(ctx context.Context, cmd contracts.NodeCommand, sc contracts.StartRunCommand, rec runRecord, adapter contracts.RuntimeAdapter,
	binding AccountBinding, resumeRef *contracts.NativeRef) {
	profileDir, acctEnv, assignment := binding.ProfileDir, binding.Env, binding.Assignment
	fail := func(err error) {
		d.releaseAccount(rec.Run.RunID)
		ce := contracts.AsError(err)
		ce.Message = redact(ce.Message)
		now := d.now()
		rec.Phase = phaseEnded
		rec.Run.State = contracts.RunFailed
		rec.Run.StateReason = "launch failed: " + ce.Message
		rec.Run.EndedAt = &now
		rec.Run.Freshness = d.freshness(false, "")
		rec.Fingerprint = fingerprint(rec.Run)
		res := contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: contracts.OutcomeFailed, Error: ce, At: now}
		if ce.Code == contracts.ErrUnsupported {
			res.Outcome = contracts.OutcomeUnsupported
		}
		err2 := sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
			if err := putRunTx(ctx, tx, rec, now); err != nil {
				return err
			}
			if assignment != nil {
				a := *assignment
				a.ReleasedAt = &now
				if _, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportAssignment, now, a); err != nil {
					return err
				}
			}
			if _, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportRun, now, rec.Run); err != nil {
				return err
			}
			_, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportCommandResult, now, res)
			return err
		})
		if err2 != nil {
			d.noteErr("record launch failure for %s: %v", rec.Run.RunID, err2)
		}
		d.rep.Kick()
		d.cfg.Logger.Warn("launch failed", "run", rec.Run.RunID, "err", ce.Message)
	}

	runDir := d.runDir(sc.RunID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		fail(err)
		return
	}
	cwd, checkout, err := d.resolveCheckout(ctx, sc)
	if err != nil {
		fail(err)
		return
	}
	envBase := map[string]string{}
	var extraArgs, grants []string
	genID := ""
	if d.cfg.Environments != nil {
		er, err := d.cfg.Environments.Resolve(ctx, EnvRequest{NodeID: d.nodeID, Harness: rec.Run.Harness, Profile: sc.EnvProfile,
			ProjectID: rec.Session.ProjectID, SessionID: sc.Session.SessionID, RunID: sc.RunID, Cwd: cwd, ConfigHome: profileDir})
		if err != nil {
			fail(err)
			return
		}
		for k, v := range er.Env {
			envBase[k] = v
		}
		extraArgs, genID, grants = er.Args, er.GenerationID, er.ConnectionGrants
		if er.Executable != "" {
			rec.Run.Runtime.Executable = er.Executable
			if er.RuntimeVersion != "" {
				rec.Run.Runtime.Version = er.RuntimeVersion
			}
		}
		if er.ProfileDir != "" && profileDir == "" {
			profileDir = er.ProfileDir
		}
	}
	if d.cfg.LaunchPrep != nil {
		lp, err := d.cfg.LaunchPrep(ctx, LaunchPrepRequest{SessionID: sc.Session.SessionID, RunID: sc.RunID,
			ProjectID: rec.Session.ProjectID, Harness: rec.Run.Harness, ConnectionGrants: grants})
		if err != nil {
			fail(err)
			return
		}
		for k, v := range lp.Env {
			envBase[k] = v
		}
		if len(lp.Manifest) > 0 && string(lp.Manifest) != "null" {
			rec.Run.AdapterData["pyre.connections"] = string(lp.Manifest)
		}
	}
	for k, v := range acctEnv {
		envBase[k] = v
	}
	if perm := rec.Run.AdapterData[adapterDataPermissions]; perm != "" {
		envBase[PermissionsEnv] = perm
	}
	guide := ""
	if d.cfg.AgentGuide != nil && rec.Run.Harness != contracts.HarnessShell {
		guide = d.cfg.AgentGuide(rec.Run.Harness, rec.Session)
		if parent := rec.Session.Lineage.ParentSessionID; parent != "" && !strings.Contains(guide, "## Started by another session") { // W-childnotify
			guide = strings.TrimRight(guide, "\n") + "\n" + agentguide.ParentNote(parent, slices.Contains(rec.Session.Tags, contracts.TagReportParent))
		}
		if sc.LaunchContext != nil && sc.LaunchContext.Text != "" {
			guide = strings.TrimSpace(guide + "\n\n" + sc.LaunchContext.Text)
		}
	}
	args := append(append([]string(nil), extraArgs...), sc.Args...)
	if rec.Run.Harness == contracts.HarnessClaude {
		mcpArgs, err := d.agentMCPArgs(args, runDir)
		if err != nil {
			fail(err)
			return
		}
		args = append(args, mcpArgs...)
	}
	l := contracts.ResolvedLaunch{RunID: sc.RunID, SessionID: sc.Session.SessionID, Harness: rec.Run.Harness, Title: sc.Session.Title,
		Cwd: cwd, Executable: rec.Run.Runtime.Executable, Args: args,
		Env: d.runEnv(envBase, sc, runDir), Unset: binding.Unset, StateDir: runDir, ProfileDir: profileDir, InitialPrompt: sc.InitialPrompt,
		EnvGeneration: genID, Resume: resumeRef, AgentGuide: guide}
	lctx, cancel := context.WithTimeout(ctx, d.cfg.LaunchTimeout)
	h, err := adapter.Launch(lctx, l)
	cancel()
	if err != nil {
		// The adapter may have started something before failing; only an
		// owned tmux socket under the run dir is cleaned up.
		_ = supervisor.Tmux{Socket: runDir + "/tmux.sock", OwnedRoot: d.cfg.StateDir}.Kill(context.Background())
		fail(err)
		return
	}
	if profileDir != "" {
		if h.Data == nil {
			h.Data = map[string]string{}
		}
		h.Data[HandleProfileDir] = profileDir
	}
	now := d.now()
	rec.Phase = phaseLive
	rec.Handle = &h
	rec.Run.Cwd = cwd
	rec.Run.Checkout = checkout
	rec.Run.EnvGenerationID = genID
	rec.Run.StateReason = "launched"
	rec.Run.Capabilities = contracts.WithoutCaps(rec.Run.Capabilities, h.Data[contracts.HandleCapsUnavailable])
	if h.NativeRef != nil {
		rec.Run.NativeRef = h.NativeRef
	}
	rec.Run.Freshness = d.freshness(false, "")
	rec.Fingerprint = fingerprint(rec.Run)
	resRaw, _ := json.Marshal(rec.Run)
	res := contracts.CommandResult{CommandID: cmd.CommandID, Kind: cmd.Kind, Outcome: contracts.OutcomeSucceeded, Result: resRaw, At: now}
	err = sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
		if err := putRunTx(ctx, tx, rec, now); err != nil {
			return err
		}
		if _, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportRun, now, rec.Run); err != nil {
			return err
		}
		_, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportCommandResult, now, res)
		return err
	})
	if err != nil {
		// The process runs but its handle could not be persisted: keep
		// supervising in memory; on restart it is reported orphaned.
		d.noteErr("persist handle for %s: %v", rec.Run.RunID, err)
	}
	d.rep.Kick()
	d.addLive(&liveRun{rec: rec, adapter: adapter, recovered: true, interval: d.cfg.Poll.Interval})
	d.engine.Kick(sc.Session.SessionID)
	d.cfg.Logger.Info("launched run", "run", sc.RunID, "session", sc.Session.SessionID, "adapter", adapter.Name())
}

// ---- deliver ----

func (d *Daemon) deliver(ctx context.Context, dc contracts.DeliverCommand) (contracts.CommandResult, error) {
	if dc.SessionID == "" && dc.Message.Recipient.Kind == contracts.AddrSession {
		dc.SessionID = dc.Message.Recipient.ID
	}
	m, _, err := d.inbox.Put(ctx, dc, d.now())
	if err != nil {
		return contracts.CommandResult{}, err
	}
	d.engine.Kick(m.SessionID)
	state := m.State
	if state == contracts.DeliveryQueued {
		state = contracts.DeliveryNodeReceived
	}
	return okResult(contracts.OutcomeSucceeded, contracts.DeliveryReceipt{MessageID: m.Envelope.MessageID, SessionID: m.SessionID,
		RunID: m.BoundRunID, NodeID: d.nodeID, State: state, Evidence: contracts.EvidenceNone, Detail: m.Reason, ObservedAt: d.now()})
}

// ---- run lookup ----

// liveTarget finds the live run addressed by (sessionID, runID).
func (d *Daemon) liveTarget(ctx context.Context, sessionID, runID string) (*liveRun, error) {
	if runID == "" {
		rec, ok, err := d.st.latestRunForSession(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, contracts.Errorf(contracts.ErrNotFound, "session has no run on this node").WithRef(contracts.KindSession, sessionID)
		}
		runID = rec.Run.RunID
	}
	lr, ok := d.getLive(runID)
	if !ok {
		rec, found, err := d.st.run(ctx, runID)
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, contracts.Errorf(contracts.ErrNotFound, "run not on this node").WithRef(contracts.KindRun, runID)
		}
		return nil, contracts.Errorf(contracts.ErrConflict, "run is %s (not live)", rec.Run.State).WithRef(contracts.KindRun, runID)
	}
	rec, _ := lr.snapshot()
	if sessionID != "" && rec.Run.SessionID != sessionID {
		return nil, contracts.Errorf(contracts.ErrConflict, "run %s belongs to session %s", runID, rec.Run.SessionID).WithRef(contracts.KindRun, runID)
	}
	if lr.adapter == nil || rec.Handle == nil || !lr.recovered {
		return nil, contracts.Errorf(contracts.ErrUnavailable, "run %s is not under control (state %s: %s)", runID, rec.Run.State, rec.Run.StateReason).WithRef(contracts.KindRun, runID)
	}
	return lr, nil
}

func requireCap(rec runRecord, adapter contracts.RuntimeAdapter, caps ...string) error {
	for _, c := range caps {
		if contracts.HasCap(rec.Run.Capabilities, c) {
			return nil
		}
	}
	return contracts.Errorf(contracts.ErrUnsupported, "adapter %s does not support %s", adapter.Name(), strings.Join(caps, "/")).WithRef(contracts.KindRun, rec.Run.RunID)
}

func (d *Daemon) runControl(ctx context.Context, cmd contracts.NodeCommand, rc contracts.RunCommand) (contracts.CommandResult, error) {
	lr, err := d.liveTarget(ctx, rc.SessionID, rc.RunID)
	if err != nil {
		return contracts.CommandResult{}, err
	}
	rec, attached := lr.snapshot()
	cap := contracts.CapInterrupt
	if cmd.Kind == contracts.CmdStop {
		cap = contracts.CapStop
	}
	if err := requireCap(rec, lr.adapter, cap); err != nil {
		return contracts.CommandResult{}, err
	}
	if cmd.Kind == contracts.CmdInterrupt && rawInterrupt(rec.Run.Capabilities) {
		if err := d.checkRawWrite(rec, attached, cmd.Actor); err != nil {
			return contracts.CommandResult{}, err
		}
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	defer cancel()
	var rcpt contracts.CommandReceipt
	if cmd.Kind == contracts.CmdStop {
		rcpt, err = lr.adapter.Stop(cctx, *rec.Handle)
	} else {
		rcpt, err = lr.adapter.Interrupt(cctx, *rec.Handle)
	}
	if err != nil {
		return contracts.CommandResult{}, err
	}
	rcpt.CommandID = cmd.CommandID
	if rcpt.ObservedAt.IsZero() {
		rcpt.ObservedAt = d.now()
	}
	d.PollNow(ctx, rec.Run.RunID) // observe the effect promptly
	outcome := rcpt.Outcome
	if outcome == "" {
		outcome = contracts.OutcomeDelivered
	}
	return okResult(outcome, rcpt)
}

// ---- attach ----

func (d *Daemon) attach(ctx context.Context, ac contracts.AttachCommand) (contracts.AttachDescriptor, error) {
	lr, err := d.liveTarget(ctx, ac.SessionID, ac.RunID)
	if err != nil {
		return contracts.AttachDescriptor{}, err
	}
	rec, _ := lr.snapshot()
	// The adapter states the mechanism; a read-only-capable adapter returns
	// a read_only descriptor rather than pretending to offer a terminal.
	if err := requireCap(rec, lr.adapter, contracts.CapAttachNative, contracts.CapAttachWeb, contracts.CapAttachCodexRemote, contracts.CapAttachReadOnly); err != nil {
		return contracts.AttachDescriptor{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	desc, err := lr.adapter.Attach(cctx, *rec.Handle, ac)
	cancel()
	if err != nil {
		return desc, err
	}
	desc.RunID, desc.NodeID = rec.Run.RunID, d.nodeID
	desc.Lease = rec.Run.Lease
	if desc.ExpiresAt.IsZero() {
		desc.ExpiresAt = d.now().Add(5 * time.Minute)
	}
	remote := ac.ClientNodeID != "" && ac.ClientNodeID != d.nodeID
	if remote && desc.Mechanism == contracts.AttachLocalTmux {
		if d.cfg.SSHAlias == "" {
			return contracts.AttachDescriptor{}, contracts.Errorf(contracts.ErrUnavailable, "remote attach needs an ssh alias for this node (pyred --ssh-alias)").WithRef(contracts.KindRun, rec.Run.RunID)
		}
		desc = sshDescriptor(desc, d.cfg.SSHAlias)
	}
	return desc, nil
}

// terminalOpen serves a relayed web terminal: the run's own local attach
// argv (its isolated tmux socket; -r when read-only) runs in a PTY owned by
// cfg.Terminal, which dials the hub back. Closing it detaches only that
// attach client; the run keeps going.
func (d *Daemon) terminalOpen(ctx context.Context, tc contracts.TerminalOpenCommand) (contracts.CommandResult, error) {
	if d.cfg.Terminal == nil {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "web terminals are not configured on this node")
	}
	lr, err := d.liveTarget(ctx, "", tc.RunID)
	if err != nil {
		return contracts.CommandResult{}, err
	}
	rec, _ := lr.snapshot()
	if err := requireCap(rec, lr.adapter, contracts.CapAttachNative, contracts.CapAttachWeb); err != nil {
		return contracts.CommandResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	desc, err := lr.adapter.Attach(cctx, *rec.Handle, contracts.AttachCommand{SessionID: rec.Run.SessionID, RunID: rec.Run.RunID, ReadOnly: tc.ReadOnly})
	cancel()
	if err != nil {
		return contracts.CommandResult{}, err
	}
	if desc.Mechanism != contracts.AttachLocalTmux || len(desc.Command) == 0 {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "run offers no local terminal to relay (mechanism %s)", desc.Mechanism).WithRef(contracts.KindRun, rec.Run.RunID)
	}
	if tc.ReadOnly && !desc.ReadOnly {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrUnsupported, "adapter returned an interactive attach for a read-only request").WithRef(contracts.KindRun, rec.Run.RunID)
	}
	if err := d.cfg.Terminal.Open(ctx, tc, desc.Command); err != nil {
		return contracts.CommandResult{}, err
	}
	return okResult(contracts.OutcomeSucceeded, map[string]string{"stream_id": tc.StreamID, "run_id": rec.Run.RunID})
}

// sshDescriptor wraps a local tmux attach command for another machine:
// ssh -t <alias> "<argv>" (via a login shell when the binary is not an
// absolute path, so PATH matches an interactive login; runtime REPORT C18).
func sshDescriptor(desc contracts.AttachDescriptor, alias string) contracts.AttachDescriptor {
	remote := supervisor.ShellJoin(desc.Command)
	if len(desc.Command) > 0 && !strings.HasPrefix(desc.Command[0], "/") {
		remote = "bash -lc " + supervisor.ShellQuote(remote)
	}
	desc.Mechanism = contracts.AttachSSHTmux
	desc.Command = []string{"ssh", "-t", alias, remote}
	if desc.Notes == "" {
		desc.Notes = "detach with Ctrl-b d; the run keeps going"
	}
	return desc
}

// ---- answer ----

func (d *Daemon) answer(ctx context.Context, cmd contracts.NodeCommand, ac contracts.AnswerCommand) (contracts.CommandResult, error) {
	ap := ac.Approval
	lr, err := d.liveTarget(ctx, ap.SessionID, ap.RunID)
	if err != nil {
		return contracts.CommandResult{}, err
	}
	rec, _ := lr.snapshot()
	stored, ok, err := getApproval(ctx, d.db, ap.ApprovalID)
	if err != nil {
		return contracts.CommandResult{}, err
	}
	if !ok && ap.RequestID != "" {
		stored, ok, err = getApprovalByRequest(ctx, d.db, ap.RunID, ap.RequestID)
		if err != nil {
			return contracts.CommandResult{}, err
		}
	}
	if !ok {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrNotFound, "approval is not pending on this node").WithRef(contracts.KindApproval, ap.ApprovalID)
	}
	if stored.RunID != rec.Run.RunID {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrConflict, "approval belongs to run %s", stored.RunID).WithRef(contracts.KindApproval, stored.ApprovalID)
	}
	if stored.State != contracts.ApprovalPending {
		code := contracts.ErrConflict
		if stored.State == contracts.ApprovalExpired {
			code = contracts.ErrExpired
		}
		e := contracts.Errorf(code, "approval is already %s", stored.State).WithRef(contracts.KindApproval, stored.ApprovalID)
		if stored.Resolution != nil {
			e.WithDetail("resolution", stored.Resolution)
		}
		return contracts.CommandResult{}, e
	}
	if stored.ExpiresAt != nil && !d.now().Before(*stored.ExpiresAt) {
		return contracts.CommandResult{}, contracts.Errorf(contracts.ErrExpired, "approval expired").WithRef(contracts.KindApproval, stored.ApprovalID)
	}
	need := contracts.CapApprovals
	if stored.Kind == contracts.ApprovalQuestion {
		need = contracts.CapQuestions
	}
	if err := requireCap(rec, lr.adapter, need); err != nil {
		return contracts.CommandResult{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	rcpt, err := lr.adapter.Answer(cctx, *rec.Handle, stored, ac.OptionID, ac.Text)
	cancel()
	if err != nil {
		return contracts.CommandResult{}, err
	}
	now := d.now()
	stored.State = contracts.ApprovalResolved
	stored.Resolution = &contracts.ApprovalResolution{OptionID: ac.OptionID, Text: ac.Text, Actor: cmd.Actor, ResolvedAt: now, Source: "pyre", Effect: "submitted"}
	err = sqlitex.Tx(ctx, d.db, func(tx *sql.Tx) error {
		if err := putApprovalTx(ctx, tx, stored, now); err != nil {
			return err
		}
		_, err := d.rep.EnqueueTx(ctx, tx, contracts.ReportApproval, now, stored)
		return err
	})
	if err != nil {
		return contracts.CommandResult{}, err
	}
	d.rep.Kick()
	rcpt.CommandID = cmd.CommandID
	if rcpt.ObservedAt.IsZero() {
		rcpt.ObservedAt = now
	}
	d.PollNow(ctx, rec.Run.RunID)
	outcome := rcpt.Outcome
	if outcome == "" {
		outcome = contracts.OutcomeDelivered
	}
	return okResult(outcome, rcpt)
}

// ---- transcript ----

func (d *Daemon) readTranscript(ctx context.Context, rr contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	runID := rr.RunID
	var rec runRecord
	var ok bool
	var err error
	if runID == "" {
		rec, ok, err = d.st.latestRunForSession(ctx, rr.SessionID)
	} else {
		rec, ok, err = d.st.run(ctx, runID)
	}
	if err != nil {
		return contracts.ReadSessionResponse{}, err
	}
	if !ok {
		return contracts.ReadSessionResponse{}, contracts.Errorf(contracts.ErrNotFound, "no run of this session on this node").WithRef(contracts.KindSession, rr.SessionID)
	}
	adapter := d.adapters[rec.Run.Adapter]
	if adapter == nil || rec.Handle == nil {
		return contracts.ReadSessionResponse{}, contracts.Errorf(contracts.ErrUnavailable, "transcript of run %s is not readable on this node", rec.Run.RunID)
	}
	if !contracts.HasCap(rec.Run.Capabilities, contracts.CapTranscript) && !contracts.HasCap(rec.Run.Capabilities, contracts.CapScreen) {
		return contracts.ReadSessionResponse{}, contracts.Errorf(contracts.ErrUnsupported, "adapter %s cannot read transcripts", adapter.Name())
	}
	if rr.Limit <= 0 || rr.Limit > 500 {
		rr.Limit = 200
	}
	rr.RunID = rec.Run.RunID
	cctx, cancel := context.WithTimeout(ctx, d.cfg.CommandTimeout)
	defer cancel()
	return adapter.ReadTranscript(cctx, *rec.Handle, rr)
}

// ---- lease ----

func (d *Daemon) leaseChanged(ctx context.Context, l contracts.InputLease) (contracts.CommandResult, error) {
	lr, ok := d.getLive(l.RunID)
	if !ok {
		return okResult(contracts.OutcomeSucceeded, map[string]string{"detail": "run not live on this node; lease noted"})
	}
	var lease *contracts.InputLease
	if l.Active(d.hubNow()) {
		cp := l
		lease = &cp
	}
	d.applyState(ctx, lr, func(r *contracts.Run) { r.Lease = lease })
	rec, _ := lr.snapshot()
	d.engine.Kick(rec.Run.SessionID)
	return okResult(contracts.OutcomeSucceeded, rec.Run)
}

// rawInterrupt reports whether the adapter interrupts by writing raw keys
// into the terminal (the Claude TUI's Escape; marked by input.paste) rather
// than through a structured API (Codex turn/interrupt).
func rawInterrupt(caps []string) bool {
	return contracts.HasCap(caps, contracts.CapInputPaste) && !contracts.HasCap(caps, contracts.CapInputAppServer)
}

// checkRawWrite applies input-lease semantics to a raw terminal write: an
// active lease held by someone else, or an attached terminal client (an
// implicit lease) when the actor does not hold the lease, refuses the write
// with a conflict. The caller must acquire (or take over) the lease first.
func (d *Daemon) checkRawWrite(rec runRecord, attached bool, actor contracts.Principal) error {
	l := rec.Run.Lease
	hubNow := d.hubNow()
	holds := l != nil && l.Active(hubNow) && l.Holder.Kind == actor.Kind && l.Holder.ID == actor.ID
	if l != nil && l.Active(hubNow) && !holds {
		return contracts.Errorf(contracts.ErrConflict, "input lease on run %s is held by %s:%s; acquire or take over the lease to send a raw interrupt",
			rec.Run.RunID, l.Holder.Kind, l.Holder.ID).WithRef(contracts.KindLease, l.LeaseID)
	}
	if attached && !holds {
		return contracts.Errorf(contracts.ErrConflict, "a terminal client is attached to run %s; acquire the input lease (or interrupt from the attached terminal) before a raw interrupt",
			rec.Run.RunID).WithRef(contracts.KindRun, rec.Run.RunID)
	}
	return nil
}
