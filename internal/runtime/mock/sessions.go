package mock

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/attention/sessionflag"
	"github.com/jhh3/pyre/internal/autotitle"
	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/usage"
)

// MockCaps is the capability set of runs launched by the mock.
var MockCaps = []string{
	c.CapAttachReadOnly, c.CapInputQueue, c.CapInputSteer, c.CapInputIdempotent, c.CapInputReconcile,
	c.CapApprovals, c.CapQuestions, c.CapInterrupt, c.CapStop, c.CapResumeNative, c.CapResumeHandoff,
	c.CapTranscript, c.CapScreen,
}

func (b *Backend) viewLocked(s *c.Session) c.SessionView {
	v := c.SessionView{Session: *s, Freshness: mockFreshness(b.now())}
	if r, ok := b.runs[s.CurrentRunID]; ok {
		rc := *r
		v.Run = &rc
		v.Freshness = r.Freshness
		if n, ok := b.nodes[r.NodeID]; ok {
			v.NodeName = n.Name
		}
	}
	var mine []c.AttentionItem
	for _, a := range b.attention {
		if a.SessionID != s.SessionID {
			continue
		}
		if a.State == c.AttnOpen || a.State == c.AttnRead {
			v.Attention++
		}
		mine = append(mine, *a)
	}
	v.Flag = sessionflag.WithRun(sessionflag.Compute(mine), v.Run)
	return v
}

func (b *Backend) currentRun(sessionID string) *c.Run {
	s, ok := b.sessions[sessionID]
	if !ok {
		return nil
	}
	return b.runs[s.CurrentRunID]
}

func (b *Backend) ListSessions(ctx context.Context, req c.ListSessionsRequest) (c.Page[c.SessionView], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.SessionView], error) {
		var out []c.SessionView
		q := strings.ToLower(req.Query)
		for _, s := range b.sessions {
			if s.Archived && !req.IncludeArchived {
				continue
			}
			if req.Harness != "" && s.Harness != req.Harness {
				continue
			}
			if req.ProjectID != nil {
				if *req.ProjectID == "" {
					if s.ProjectID != nil {
						continue
					}
				} else if s.ProjectID == nil || *s.ProjectID != *req.ProjectID {
					continue
				}
			}
			if q != "" && !strings.Contains(strings.ToLower(s.Title), q) && !strings.Contains(strings.ToLower(s.SessionID), q) {
				continue
			}
			v := b.viewLocked(s)
			if req.NodeID != "" && (v.Run == nil || v.Run.NodeID != req.NodeID) {
				continue
			}
			if len(req.States) > 0 {
				if v.Run == nil {
					continue
				}
				match := false
				for _, st := range req.States {
					if v.Run.State == st {
						match = true
					}
				}
				if !match {
					continue
				}
			}
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].Session.CreatedAt.Equal(out[j].Session.CreatedAt) {
				return out[i].Session.CreatedAt.After(out[j].Session.CreatedAt)
			}
			return out[i].Session.SessionID < out[j].Session.SessionID
		})
		return paginate(out, req.PageRequest, b.now())
	})
}

func (b *Backend) GetSession(ctx context.Context, req c.GetRequest) (c.SessionView, error) {
	return query(b, ctx, func(p c.Principal) (c.SessionView, error) {
		s, ok := b.sessions[req.ID]
		if !ok {
			return c.SessionView{}, notFound(c.KindSession, req.ID)
		}
		return b.viewLocked(s), nil
	})
}

func (b *Backend) selectNode(req c.StartSessionRequest) (*c.Node, error) {
	if req.NodeID != "" {
		n, ok := b.nodes[req.NodeID]
		if !ok {
			return nil, notFound(c.KindNode, req.NodeID)
		}
		return n, nil
	}
	if req.NodeSelector == nil {
		return nil, c.Errorf(c.ErrInvalid, "node_id or node_selector is required")
	}
	var cands []*c.Node
	for _, id := range sortedKeys(b.nodes) {
		n := b.nodes[id]
		if n.Connection.State != c.ConnConnected {
			continue
		}
		if req.NodeSelector.Platform != "" && n.Platform != req.NodeSelector.Platform {
			continue
		}
		ok := true
		for k, v := range req.NodeSelector.Labels {
			if n.Labels[k] != v {
				ok = false
			}
		}
		if !ok || !harnessAvailable(n, req.Harness) {
			continue
		}
		cands = append(cands, n)
	}
	if len(cands) == 0 {
		return nil, c.Errorf(c.ErrUnavailable, "no connected node matches the selector")
	}
	if req.NodeSelector.LeastLoaded {
		sort.SliceStable(cands, func(i, j int) bool {
			return loadOf(cands[i]) < loadOf(cands[j])
		})
	}
	return cands[0], nil
}

func loadOf(n *c.Node) float64 {
	if n.Resources.CPUCount <= 0 {
		return n.Resources.Load1
	}
	return n.Resources.Load1 / float64(n.Resources.CPUCount)
}

func harnessInfo(n *c.Node, h c.Harness) (c.HarnessInfo, bool) {
	for _, hi := range n.Harnesses {
		if hi.Harness == h {
			return hi, true
		}
	}
	return c.HarnessInfo{}, false
}

func harnessAvailable(n *c.Node, h c.Harness) bool {
	if h == c.HarnessMock {
		return true
	}
	hi, ok := harnessInfo(n, h)
	return ok && hi.Available
}

func validHarness(h c.Harness) bool {
	switch h {
	case c.HarnessClaude, c.HarnessCodex, c.HarnessShell, c.HarnessMock:
		return true
	}
	return false
}

// checkNodeForLaunch validates connection, harness, and capacity.
func (b *Backend) checkNodeForLaunch(n *c.Node, h c.Harness) error {
	switch n.Connection.State {
	case c.ConnRevoked:
		return c.Errorf(c.ErrForbidden, "node %s is revoked", n.Name).WithRef(c.KindNode, n.NodeID)
	case c.ConnConnected:
	default:
		return c.Errorf(c.ErrUnavailable, "node %s is not connected; nothing was launched", n.Name).WithRef(c.KindNode, n.NodeID)
	}
	if !harnessAvailable(n, h) {
		reason := "not installed"
		if hi, ok := harnessInfo(n, h); ok && hi.Reason != "" {
			reason = hi.Reason
		}
		return c.Errorf(c.ErrUnsupported, "harness %s unavailable on %s: %s", h, n.Name, reason).WithRef(c.KindNode, n.NodeID)
	}
	if n.Resources.MaxRuns > 0 && n.Resources.ActiveRuns >= n.Resources.MaxRuns {
		return c.Errorf(c.ErrCapacityExhausted, "node %s is at its run limit (%d)", n.Name, n.Resources.MaxRuns).WithRef(c.KindNode, n.NodeID)
	}
	return nil
}

// chooseAccount applies the account policy. Returns ("", reason) for
// ambient/no-provider launches.
func (b *Backend) chooseAccount(h c.Harness, pol c.AccountPolicy) (string, string, error) {
	prov := c.ProviderFor(h)
	if prov == "" {
		return "", "harness has no subscription provider", nil
	}
	mode := pol.Mode
	if mode == "" {
		mode = c.AccountEligible
	}
	switch mode {
	case c.AccountAmbient:
		return "", "ambient node login (no Pyre assignment)", nil
	case c.AccountExplicit:
		a, ok := b.accounts[pol.AccountID]
		if !ok {
			return "", "", notFound(c.KindAccount, pol.AccountID)
		}
		if a.Account.Provider != prov {
			return "", "", c.Errorf(c.ErrInvalid, "account %s is %s; harness %s needs %s (fallback never crosses providers)", a.Account.AccountID, a.Account.Provider, h, prov)
		}
		if a.Account.Disabled {
			return "", "", c.Errorf(c.ErrForbidden, "account %s is disabled", a.Account.AccountID)
		}
		if a.Latest != nil && (a.Latest.Eligibility == c.EligibleExhausted || a.Latest.Eligibility == c.EligibleUnusable) {
			return "", "", c.Errorf(c.ErrCapacityExhausted, "account %s is %s", a.Account.AccountID, a.Latest.Eligibility).WithRef(c.KindAccount, a.Account.AccountID)
		}
		return a.Account.AccountID, "explicit account", nil
	case c.AccountEligible:
	default:
		return "", "", c.Errorf(c.ErrInvalid, "unknown account policy mode %q", mode)
	}
	var eligible, unknown []string
	any := false
	order := append([]string(nil), pol.Prefer...)
	order = append(order, sortedKeys(b.accounts)...)
	seen := map[string]bool{}
	for _, id := range order {
		a, ok := b.accounts[id]
		if !ok || seen[id] || a.Account.Provider != prov || a.Account.Disabled {
			continue
		}
		seen[id] = true
		any = true
		el := c.EligibleUnknown
		if a.Latest != nil {
			el = a.Latest.Eligibility
		}
		switch el {
		case c.EligibleYes:
			eligible = append(eligible, id)
		case c.EligibleUnknown:
			unknown = append(unknown, id)
		}
	}
	switch {
	case len(eligible) > 0:
		// Same rule as the node selector: a preferred account first, else
		// the most 5-hour headroom, then weekly (usage.Headroom).
		for _, id := range pol.Prefer {
			for _, e := range eligible {
				if e == id {
					return id, "preferred account (" + usage.HeadroomOf(b.accounts[id].Latest.Windows).Numbers() + ")", nil
				}
			}
		}
		best := eligible[0]
		for _, id := range eligible[1:] {
			if usage.HeadroomOf(b.accounts[id].Latest.Windows).Compare(usage.HeadroomOf(b.accounts[best].Latest.Windows)) < 0 {
				best = id
			}
		}
		return best, usage.Explain(usage.HeadroomOf(b.accounts[best].Latest.Windows), len(eligible)), nil
	case len(unknown) > 0:
		return unknown[0], "eligibility unknown (stale observation); not treated as unlimited", nil
	case any:
		return "", "", c.Errorf(c.ErrCapacityExhausted, "no eligible %s subscription; work can wait for a reset", prov).WithDetail("outcome", string(c.FallbackWaiting))
	}
	return "", "no configured " + string(prov) + " accounts; ambient login", nil
}

func (b *Backend) StartSession(ctx context.Context, req c.StartSessionRequest) (c.StartSessionResponse, error) {
	return mutate(b, ctx, c.OpStartSession, req.Mutation, req, func(p c.Principal) (c.StartSessionResponse, error) {
		if err := requireNotNode(p); err != nil {
			return c.StartSessionResponse{}, err
		}
		if !validHarness(req.Harness) {
			return c.StartSessionResponse{}, c.Errorf(c.ErrInvalid, "unknown harness %q", req.Harness)
		}
		var project *c.Project
		if req.ProjectID != nil {
			pr, ok := b.projects[*req.ProjectID]
			if !ok {
				return c.StartSessionResponse{}, notFound(c.KindProject, *req.ProjectID)
			}
			if pr.Archived {
				return c.StartSessionResponse{}, c.Errorf(c.ErrConflict, "project %s is archived", pr.ProjectID).WithRef(c.KindProject, pr.ProjectID)
			}
			project = pr
		}
		n, err := b.selectNode(req)
		if err != nil {
			return c.StartSessionResponse{}, err
		}
		if err := b.checkNodeForLaunch(n, req.Harness); err != nil {
			return c.StartSessionResponse{}, err
		}
		cwd := req.Cwd
		var checkout *c.CheckoutBinding
		if req.Checkout != nil {
			if req.Checkout.RepoID != "" && project == nil {
				return c.StartSessionResponse{}, c.Errorf(c.ErrInvalid, "checkout.repo_id requires project_id")
			}
			mode := req.Checkout.Mode
			if mode == "" {
				mode = c.CheckoutPlain
			}
			p := req.Checkout.Path
			if p == "" {
				p = path.Join("/home/synth/src", req.Checkout.RepoID)
				if mode == c.CheckoutWorktree {
					p += "-wt/" + strings.ReplaceAll(req.Checkout.Branch, "/", "-")
				}
			}
			checkout = &c.CheckoutBinding{NodeID: n.NodeID, Path: p, RepoID: req.Checkout.RepoID, Mode: mode, Branch: req.Checkout.Branch}
			if cwd == "" {
				cwd = p
			}
		}
		if cwd == "" {
			return c.StartSessionResponse{}, c.Errorf(c.ErrInvalid, "cwd or checkout is required")
		}
		if !path.IsAbs(cwd) && !strings.HasPrefix(cwd, "~") {
			return c.StartSessionResponse{}, c.Errorf(c.ErrInvalid, "cwd must be absolute")
		}
		accountID, accountReason, err := b.chooseAccount(req.Harness, req.AccountPolicy)
		if err != nil {
			return c.StartSessionResponse{}, err
		}
		now := b.now()
		title := req.Title
		if title == "" {
			title = path.Base(cwd)
		}
		s := &c.Session{
			SessionID: c.NewID(c.PrefixSession), Harness: req.Harness, Title: title, ProjectID: cloneStrPtr(req.ProjectID),
			CreatedBy: p, CreatedAt: now, Tags: autotitle.LaunchTags(p, req.Title, req.Harness, append([]string(nil), req.Tags...)),
		}
		s.Lineage.ParentSessionID = req.ParentSessionID
		if p.Kind == c.PrincipalSession && s.Lineage.ParentSessionID == "" {
			s.Lineage.ParentSessionID = p.ID
		}
		r := b.newRunLocked(s, n, req.Harness, cwd, checkout, accountID, accountReason)
		r.EnvGenerationID = b.currentEnvID(n.NodeID)
		s.CurrentRunID = r.RunID
		b.sessions[s.SessionID] = s
		b.pendingCmd[r.RunID] = "start"
		if req.InitialPrompt != "" {
			b.appendTranscript(r.RunID, "user", truncate(req.InitialPrompt, 4000), "")
			b.busy[r.RunID] = b.opts.BusyTicks
		}
		b.emit(c.EvSessionCreated, ref(c.KindSession, s.SessionID), s.SessionID, s.ProjectID, s)
		b.emit(c.EvRunStarted, ref(c.KindRun, r.RunID), s.SessionID, s.ProjectID, r)
		if accountID != "" {
			b.assignAccountLocked(accountID, r, accountReason)
		}
		return c.StartSessionResponse{Session: b.viewLocked(s), CommandID: c.NewID(c.PrefixCommand)}, nil
	})
}

func (b *Backend) newRunLocked(s *c.Session, n *c.Node, h c.Harness, cwd string, checkout *c.CheckoutBinding, accountID, reason string) *c.Run {
	now := b.now()
	caps := append([]string(nil), MockCaps...)
	version := "mock"
	if hi, ok := harnessInfo(n, h); ok && len(hi.Capabilities) > 0 {
		caps = append([]string(nil), hi.Capabilities...)
		version = hi.Version
	}
	r := &c.Run{
		RunID: c.NewID(c.PrefixRun), SessionID: s.SessionID, NodeID: n.NodeID, Adapter: "mock", Harness: h,
		Cwd: cwd, Checkout: checkout, Runtime: c.RuntimeVersion{Harness: h, Version: version},
		AccountID: accountID, State: c.RunStarting, StateReason: "mock launch queued",
		Freshness: mockFreshness(now), StartedAt: now, Capabilities: caps,
		NativeRef:   &c.NativeRef{Kind: "mock_session", ID: c.NewID("mock")},
		AdapterData: map[string]string{"mock.account_reason": reason},
	}
	b.runs[r.RunID] = r
	b.transcripts[r.RunID] = []c.TranscriptEntry{{Seq: 1, RunID: r.RunID, At: now, Kind: "system", Text: "[mock] run started on " + n.Name + "; not a real runtime"}}
	n.Resources.ActiveRuns++
	return r
}

func (b *Backend) currentEnvID(nodeID string) string {
	if gens := b.envs[nodeID]; len(gens) > 0 {
		return gens[len(gens)-1].GenerationID
	}
	return ""
}

func (b *Backend) appendTranscript(runID, kind, text, messageID string) {
	entries := b.transcripts[runID]
	seq := int64(len(entries) + 1)
	if len(entries) > 0 {
		seq = entries[len(entries)-1].Seq + 1
	}
	b.transcripts[runID] = append(entries, c.TranscriptEntry{Seq: seq, RunID: runID, At: b.now(), Kind: kind, Text: text, MessageID: messageID})
	if len(b.transcripts[runID]) > 2000 {
		b.transcripts[runID] = append([]c.TranscriptEntry(nil), b.transcripts[runID][len(b.transcripts[runID])-2000:]...)
	}
}

func (b *Backend) ReadSession(ctx context.Context, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	return query(b, ctx, func(p c.Principal) (c.ReadSessionResponse, error) {
		s, ok := b.sessions[req.SessionID]
		if !ok {
			return c.ReadSessionResponse{}, notFound(c.KindSession, req.SessionID)
		}
		runID := req.RunID
		if runID == "" {
			runID = s.CurrentRunID
		}
		r, ok := b.runs[runID]
		if !ok || r.SessionID != s.SessionID {
			return c.ReadSessionResponse{}, notFound(c.KindRun, runID)
		}
		entries := b.transcripts[runID]
		var after int64
		if req.Cursor != "" {
			n, err := parseInt64(req.Cursor)
			if err != nil {
				return c.ReadSessionResponse{}, c.Errorf(c.ErrInvalid, "invalid cursor")
			}
			after = n
		}
		limit := req.Limit
		if limit <= 0 || limit > 500 {
			limit = 100
		}
		resp := c.ReadSessionResponse{Entries: []c.TranscriptEntry{}, Freshness: r.Freshness}
		var sel []c.TranscriptEntry
		for _, e := range entries {
			if e.Seq > after {
				sel = append(sel, e)
			}
		}
		if len(entries) > 0 && after > 0 && entries[0].Seq > after+1 {
			resp.Gap = true
		}
		if req.Tail && len(sel) > limit {
			sel = sel[len(sel)-limit:]
		} else if len(sel) > limit {
			sel = sel[:limit]
		}
		resp.Entries = append(resp.Entries, sel...)
		if len(sel) > 0 {
			resp.NextCursor = formatInt64(sel[len(sel)-1].Seq)
		} else {
			resp.NextCursor = formatInt64(after)
		}
		last := ""
		if len(entries) > 0 {
			last = entries[len(entries)-1].Text
		}
		resp.Screen = "[mock screen] " + s.Title + " — " + string(r.State) + "\n" + truncate(last, 200) + "\n> "
		return resp, nil
	})
}

func (b *Backend) AttachSession(ctx context.Context, req c.AttachRequest) (c.AttachDescriptor, error) {
	return mutate(b, ctx, c.OpAttachSession, req.Mutation, req, func(p c.Principal) (c.AttachDescriptor, error) {
		r := b.currentRun(req.SessionID)
		if _, ok := b.sessions[req.SessionID]; !ok {
			return c.AttachDescriptor{}, notFound(c.KindSession, req.SessionID)
		}
		if r == nil || r.State.Terminal() {
			return c.AttachDescriptor{}, c.Errorf(c.ErrConflict, "session has no live run to attach to").WithRef(c.KindSession, req.SessionID)
		}
		n := b.nodes[r.NodeID]
		d := c.AttachDescriptor{RunID: r.RunID, NodeID: r.NodeID, ExpiresAt: b.now().Add(time.Minute), ReadOnly: true, Mechanism: c.AttachReadOnly,
			Notes: "MOCK backend: there is no live terminal; showing transcript/screen only"}
		if l := b.activeLease(r); l != nil {
			lc := *l
			d.Lease = &lc
		}
		wantNative := !req.ReadOnly
		if wantNative && len(req.Preferred) > 0 {
			wantNative = false
			for _, m := range req.Preferred {
				if m == c.AttachSSHTmux || m == c.AttachLocalTmux {
					wantNative = true
				}
			}
		}
		if wantNative && c.HasCap(r.Capabilities, c.CapAttachNative) && n != nil {
			alias := n.SSHAlias
			if alias == "" {
				alias = n.Name
			}
			d.Mechanism = c.AttachSSHTmux
			d.ReadOnly = false
			d.Command = []string{"ssh", "-t", alias, "tmux", "-L", "pyre-mock", "attach", "-t", r.RunID}
			d.Notes = "MOCK backend: illustrative command only; no live tmux session exists"
		}
		return d, nil
	})
}

// runForCommand validates the session/run pair for interrupt/stop.
func (b *Backend) runForCommand(req c.SessionCommandRequest, cap string) (*c.Run, error) {
	if _, ok := b.sessions[req.SessionID]; !ok {
		return nil, notFound(c.KindSession, req.SessionID)
	}
	r := b.currentRun(req.SessionID)
	if r == nil {
		return nil, c.Errorf(c.ErrConflict, "session has no run").WithRef(c.KindSession, req.SessionID)
	}
	if req.RunID != "" && req.RunID != r.RunID {
		return nil, c.Errorf(c.ErrConflict, "run %s is not the session's current run (%s)", req.RunID, r.RunID).WithRef(c.KindRun, req.RunID)
	}
	if r.State.Terminal() {
		return nil, c.Errorf(c.ErrConflict, "run %s already %s", r.RunID, r.State).WithRef(c.KindRun, r.RunID)
	}
	if !c.HasCap(r.Capabilities, cap) {
		return nil, c.Errorf(c.ErrUnsupported, "adapter does not advertise %s", cap).WithRef(c.KindRun, r.RunID)
	}
	if n := b.nodes[r.NodeID]; n == nil || n.Connection.State != c.ConnConnected {
		return nil, c.Errorf(c.ErrUnavailable, "node %s is not connected; command not sent", r.NodeID).WithRef(c.KindNode, r.NodeID)
	}
	return r, nil
}

func (b *Backend) InterruptSession(ctx context.Context, req c.SessionCommandRequest) (c.CommandReceipt, error) {
	return mutate(b, ctx, c.OpInterruptSession, req.Mutation, req, func(p c.Principal) (c.CommandReceipt, error) {
		if err := requireNotNode(p); err != nil {
			return c.CommandReceipt{}, err
		}
		r, err := b.runForCommand(req, c.CapInterrupt)
		if err != nil {
			return c.CommandReceipt{}, err
		}
		b.pendingCmd[r.RunID] = "interrupt"
		return c.CommandReceipt{CommandID: c.NewID(c.PrefixCommand), Accepted: true, Outcome: c.OutcomeDelivered, Detail: "mock: effect observed on next tick", ObservedAt: b.now()}, nil
	})
}

func (b *Backend) StopSession(ctx context.Context, req c.SessionCommandRequest) (c.CommandReceipt, error) {
	return mutate(b, ctx, c.OpStopSession, req.Mutation, req, func(p c.Principal) (c.CommandReceipt, error) {
		if err := requireNotNode(p); err != nil {
			return c.CommandReceipt{}, err
		}
		r, err := b.runForCommand(req, c.CapStop)
		if err != nil {
			return c.CommandReceipt{}, err
		}
		b.pendingCmd[r.RunID] = "stop"
		b.setRunStateLocked(r, c.RunExiting, "stop requested", "hub")
		return c.CommandReceipt{CommandID: c.NewID(c.PrefixCommand), Accepted: true, Outcome: c.OutcomeDelivered, Detail: "mock: exit observed on next tick", ObservedAt: b.now()}, nil
	})
}

func (b *Backend) ResumeSession(ctx context.Context, req c.ResumeRequest) (c.StartSessionResponse, error) {
	return mutate(b, ctx, c.OpResumeSession, req.Mutation, req, func(p c.Principal) (c.StartSessionResponse, error) {
		if err := requireNotNode(p); err != nil {
			return c.StartSessionResponse{}, err
		}
		s, ok := b.sessions[req.SessionID]
		if !ok {
			return c.StartSessionResponse{}, notFound(c.KindSession, req.SessionID)
		}
		prev := b.runs[s.CurrentRunID]
		if prev != nil && !prev.State.Terminal() {
			return c.StartSessionResponse{}, c.Errorf(c.ErrConflict, "run %s is still %s; stop it before resuming (no duplicate current runs)", prev.RunID, prev.State).WithRef(c.KindRun, prev.RunID)
		}
		nodeID := req.NodeID
		if nodeID == "" && prev != nil {
			nodeID = prev.NodeID
		}
		n, ok := b.nodes[nodeID]
		if !ok {
			return c.StartSessionResponse{}, notFound(c.KindNode, nodeID)
		}
		if err := b.checkNodeForLaunch(n, s.Harness); err != nil {
			return c.StartSessionResponse{}, err
		}
		kind := c.ContinuationFresh
		if prev != nil {
			switch {
			case c.HasCap(prev.Capabilities, c.CapResumeNative) && prev.NativeRef != nil:
				kind = c.ContinuationNativeResume
			case req.AllowHandoff && c.HasCap(prev.Capabilities, c.CapResumeHandoff):
				kind = c.ContinuationHandoff
			case req.AllowHandoff:
				kind = c.ContinuationHandoff
			default:
				return c.StartSessionResponse{}, c.Errorf(c.ErrUnsupported, "native resume unsupported for run %s; set allow_handoff for a reconstructed continuation", prev.RunID).WithRef(c.KindRun, prev.RunID)
			}
		}
		accountID, reason, err := b.chooseAccount(s.Harness, req.AccountPolicy)
		if err != nil {
			return c.StartSessionResponse{}, err
		}
		cwd := "/home/synth"
		var checkout *c.CheckoutBinding
		if prev != nil {
			cwd = prev.Cwd
			if prev.Checkout != nil {
				cb := *prev.Checkout
				cb.NodeID = n.NodeID
				checkout = &cb
			}
		}
		r := b.newRunLocked(s, n, s.Harness, cwd, checkout, accountID, reason)
		r.ContinuationKind = kind
		r.EnvGenerationID = b.currentEnvID(n.NodeID)
		if prev != nil {
			r.PreviousRunID = prev.RunID
			if kind == c.ContinuationNativeResume && prev.NativeRef != nil {
				nr := *prev.NativeRef
				r.NativeRef = &nr
			}
		}
		s.CurrentRunID = r.RunID
		b.pendingCmd[r.RunID] = "start"
		b.emit(c.EvRunStarted, ref(c.KindRun, r.RunID), s.SessionID, s.ProjectID, r)
		b.emit(c.EvSessionUpdated, ref(c.KindSession, s.SessionID), s.SessionID, s.ProjectID, s)
		if accountID != "" {
			b.assignAccountLocked(accountID, r, reason)
		}
		return c.StartSessionResponse{Session: b.viewLocked(s), CommandID: c.NewID(c.PrefixCommand)}, nil
	})
}

func (b *Backend) AssociateSession(ctx context.Context, req c.AssociateRequest) (c.SessionView, error) {
	return mutate(b, ctx, c.OpAssociateSession, req.Mutation, req, func(p c.Principal) (c.SessionView, error) {
		if err := requireNotNode(p); err != nil {
			return c.SessionView{}, err
		}
		s, ok := b.sessions[req.SessionID]
		if !ok {
			return c.SessionView{}, notFound(c.KindSession, req.SessionID)
		}
		if req.ProjectID != nil {
			if _, ok := b.projects[*req.ProjectID]; !ok {
				return c.SessionView{}, notFound(c.KindProject, *req.ProjectID)
			}
		}
		s.ProjectID = cloneStrPtr(req.ProjectID)
		b.emit(c.EvSessionUpdated, ref(c.KindSession, s.SessionID), s.SessionID, s.ProjectID, s)
		return b.viewLocked(s), nil
	})
}

func (b *Backend) UpdateSession(ctx context.Context, req c.UpdateSessionRequest) (c.SessionView, error) {
	return mutate(b, ctx, c.OpUpdateSession, req.Mutation, req, func(p c.Principal) (c.SessionView, error) {
		if err := requireNotNode(p); err != nil {
			return c.SessionView{}, err
		}
		s, ok := b.sessions[req.SessionID]
		if !ok {
			return c.SessionView{}, notFound(c.KindSession, req.SessionID)
		}
		if req.Title != nil {
			if err := autotitle.AllowRename(p, *s, *req.Title); err != nil { // W-names
				return c.SessionView{}, err
			}
		}
		if req.Archived != nil && p.Kind == c.PrincipalSession && p.ID != s.SessionID && !s.StartedBy(p.ID) { // W-childnotify
			return c.SessionView{}, c.Errorf(c.ErrForbidden, "an agent may archive only its own session and the sessions it started")
		}
		if req.Tags != nil {
			s.Tags = append([]string(nil), (*req.Tags)...)
		}
		if req.Title != nil {
			s.Title = *req.Title
			s.Tags = autotitle.RenameTags(p, s.Tags)
		}
		if req.Archived != nil {
			s.Archived = *req.Archived
		}
		b.emit(c.EvSessionUpdated, ref(c.KindSession, s.SessionID), s.SessionID, s.ProjectID, s)
		return b.viewLocked(s), nil
	})
}

func (b *Backend) ListRuns(ctx context.Context, req c.ListRunsRequest) (c.Page[c.Run], error) {
	return query(b, ctx, func(p c.Principal) (c.Page[c.Run], error) {
		if _, ok := b.sessions[req.SessionID]; !ok {
			return c.Page[c.Run]{}, notFound(c.KindSession, req.SessionID)
		}
		var out []c.Run
		for _, r := range b.runs {
			if r.SessionID == req.SessionID {
				out = append(out, *r)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			if !out[i].StartedAt.Equal(out[j].StartedAt) {
				return out[i].StartedAt.Before(out[j].StartedAt)
			}
			return out[i].RunID < out[j].RunID
		})
		return paginate(out, req.PageRequest, b.now())
	})
}

// setRunStateLocked records a run observation and emits run.observed (or
// run.ended for terminal states).
func (b *Backend) setRunStateLocked(r *c.Run, st c.RunState, reason, origin string) {
	if r.State == st && r.StateReason == reason {
		return
	}
	now := b.now()
	r.State = st
	r.StateReason = reason
	r.Freshness = mockFreshness(now)
	kind := c.EvRunObserved
	if st.Terminal() {
		kind = c.EvRunEnded
		r.EndedAt = &now
		if r.ExitCode == nil && st == c.RunExited {
			zero := 0
			r.ExitCode = &zero
		}
		if n := b.nodes[r.NodeID]; n != nil && n.Resources.ActiveRuns > 0 {
			n.Resources.ActiveRuns--
		}
		delete(b.busy, r.RunID)
		b.withdrawRunApprovalsLocked(r, "run ended")
		b.withdrawAttentionLocked("flag:session:"+r.SessionID, c.AttnWithdrawn) // the run that asked is gone
		b.releaseAssignmentsLocked(r.RunID)
	}
	var pid *string
	if s := b.sessions[r.SessionID]; s != nil {
		pid = s.ProjectID
	}
	b.emitFrom(origin, kind, ref(c.KindRun, r.RunID), r.SessionID, pid, r)
}

func (b *Backend) markNodeRunsStale(nodeID, reason string) {
	for _, r := range b.runs {
		if r.NodeID != nodeID || r.State.Terminal() {
			continue
		}
		if r.State != c.RunUnknown {
			b.lastState[r.RunID] = r.State
		}
		prev := b.lastState[r.RunID]
		observed := r.Freshness.ObservedAt
		b.setRunStateLocked(r, c.RunUnknown, reason+"; last observed "+string(prev), "hub")
		r.Freshness.ObservedAt = observed
		r.Freshness.Stale = true
		r.Freshness.Reason = reason
	}
}
