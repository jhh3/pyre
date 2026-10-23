package hub

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/persistence"
)

// nodeInfo is the hub's merged view of a node for launch validation.
type nodeInfo struct {
	ID        string
	Name      string
	Enrolled  bool
	Revoked   bool
	Platform  string
	Labels    map[string]string
	Harnesses []contracts.HarnessInfo
	Resources contracts.NodeResources
	Reported  bool // harness inventory known
	Connected bool
}

// nodeInfo merges the fleet registry (if configured) with the node's own
// hello/status reports. Without a fleet domain, a node is considered
// enrolled once it has authenticated and said hello.
func (h *Hub) nodeInfo(ctx context.Context, nodeID string) (nodeInfo, error) {
	ni := nodeInfo{ID: nodeID}
	var hello, status sql.NullString
	err := h.db.QueryRowContext(ctx, `SELECT hello, status FROM node_state WHERE node_id=?`, nodeID).Scan(&hello, &status)
	if err != nil && err != sql.ErrNoRows {
		return ni, err
	}
	if h.cfg.Fleet != nil {
		n, err := h.cfg.Fleet.GetNode(h.systemCtx(ctx), contracts.GetRequest{ID: nodeID})
		if err != nil {
			if contracts.IsCode(err, contracts.ErrNotFound) {
				return ni, contracts.Errorf(contracts.ErrNotFound, "node is not enrolled").WithRef(contracts.KindNode, nodeID)
			}
			return ni, err
		}
		if n.Labels[contracts.NodeLabelEnrollment] == "pending" {
			return ni, contracts.Errorf(contracts.ErrNotFound, "node %s has not completed enrollment (its ticket is unredeemed); run pyred enroll on it first", n.Name).WithRef(contracts.KindNode, nodeID)
		}
		ni.Enrolled = true
		ni.Name = n.Name
		ni.Platform = n.Platform
		ni.Labels = n.Labels
		ni.Revoked = n.Connection.State == contracts.ConnRevoked
		if len(n.Harnesses) > 0 {
			ni.Harnesses, ni.Reported = n.Harnesses, true
		}
		ni.Resources = n.Resources
	}
	if hello.Valid {
		var nh contracts.NodeHello
		if json.Unmarshal([]byte(hello.String), &nh) == nil {
			ni.Enrolled = true
			if ni.Name == "" {
				ni.Name = nh.Hostname
			}
			if ni.Platform == "" {
				ni.Platform = nh.Platform
			}
			if len(nh.Harnesses) > 0 {
				ni.Harnesses, ni.Reported = nh.Harnesses, true
			}
			ni.Resources = nh.Resources
		}
	}
	if status.Valid {
		var st contracts.NodeStatusReport
		if json.Unmarshal([]byte(status.String), &st) == nil {
			if len(st.Harnesses) > 0 {
				ni.Harnesses, ni.Reported = st.Harnesses, true
			}
			ni.Resources = st.Resources
		}
	}
	if !ni.Enrolled {
		return ni, contracts.Errorf(contracts.ErrNotFound, "node is not enrolled (or has never connected)").WithRef(contracts.KindNode, nodeID)
	}
	ni.Connected, _ = h.nodeConnected(ctx, h.db, nodeID)
	return ni, nil
}

func (ni nodeInfo) harness(hn contracts.Harness) (contracts.HarnessInfo, bool) {
	for _, x := range ni.Harnesses {
		if x.Harness == hn {
			return x, true
		}
	}
	return contracts.HarnessInfo{}, false
}

func (h *Hub) activeRunsOn(ctx context.Context, q queryer, nodeID string) int {
	var n int
	_ = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE node_id=? AND state NOT IN (?,?)`, nodeID, string(contracts.RunExited), string(contracts.RunFailed)).Scan(&n)
	return n
}

// checkLaunchTarget validates that a node can launch harness hn now.
func (h *Hub) checkLaunchTarget(ctx context.Context, ni nodeInfo, hn contracts.Harness) (contracts.HarnessInfo, error) {
	ref := func(e *contracts.Error) *contracts.Error { return e.WithRef(contracts.KindNode, ni.ID) }
	if ni.Revoked {
		return contracts.HarnessInfo{}, ref(contracts.Errorf(contracts.ErrForbidden, "node is revoked"))
	}
	if !ni.Connected {
		return contracts.HarnessInfo{}, ref(contracts.Errorf(contracts.ErrUnavailable, "node is disconnected; launch requires a connected node to validate the directory, environment and account"))
	}
	if !ni.Reported {
		return contracts.HarnessInfo{}, ref(contracts.Errorf(contracts.ErrUnavailable, "node has not reported its harnesses yet"))
	}
	hi, ok := ni.harness(hn)
	if !ok || !hi.Available {
		reason := "not installed"
		if ok && hi.Reason != "" {
			reason = hi.Reason
		}
		return hi, ref(contracts.Errorf(contracts.ErrUnsupported, "harness %s is not available on node %s: %s", hn, nodeLabel(ni), reason))
	}
	active := h.activeRunsOn(ctx, h.db, ni.ID)
	if ni.Resources.ActiveRuns > active {
		active = ni.Resources.ActiveRuns
	}
	if ni.Resources.MaxRuns > 0 && active >= ni.Resources.MaxRuns {
		return hi, ref(contracts.Errorf(contracts.ErrCapacityExhausted, "node %s is at its advertised capacity (%d/%d runs)", nodeLabel(ni), active, ni.Resources.MaxRuns))
	}
	return hi, nil
}

func nodeLabel(ni nodeInfo) string {
	if ni.Name != "" {
		return ni.Name
	}
	return ni.ID
}

// selectNode applies a caller-supplied selector over known nodes.
func (h *Hub) selectNode(ctx context.Context, sel *contracts.NodeSelector, hn contracts.Harness) (nodeInfo, contracts.HarnessInfo, error) {
	ids := map[string]bool{}
	if h.cfg.Fleet != nil {
		page, err := h.cfg.Fleet.ListNodes(h.systemCtx(ctx), contracts.ListNodesRequest{PageRequest: contracts.PageRequest{Limit: persistence.MaxLimit}})
		if err != nil {
			return nodeInfo{}, contracts.HarnessInfo{}, err
		}
		for _, n := range page.Items {
			ids[n.NodeID] = true
		}
	}
	rows, err := h.db.QueryContext(ctx, `SELECT node_id FROM node_state WHERE hello IS NOT NULL`)
	if err != nil {
		return nodeInfo{}, contracts.HarnessInfo{}, err
	}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids[id] = true
		}
	}
	rows.Close()
	type cand struct {
		ni   nodeInfo
		hi   contracts.HarnessInfo
		load int
	}
	var cands []cand
	var lastErr error
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	for _, id := range sorted {
		ni, err := h.nodeInfo(ctx, id)
		if err != nil {
			continue
		}
		if sel.Platform != "" && ni.Platform != sel.Platform {
			continue
		}
		match := true
		for k, v := range sel.Labels {
			if ni.Labels[k] != v {
				match = false
			}
		}
		if !match {
			continue
		}
		hi, err := h.checkLaunchTarget(ctx, ni, hn)
		if err != nil {
			lastErr = err
			continue
		}
		load := h.activeRunsOn(ctx, h.db, id)
		cands = append(cands, cand{ni, hi, load})
	}
	if len(cands) == 0 {
		if lastErr != nil {
			return nodeInfo{}, contracts.HarnessInfo{}, contracts.Errorf(contracts.CodeOf(lastErr), "no node matches the selector: %v", contracts.AsError(lastErr).Message)
		}
		return nodeInfo{}, contracts.HarnessInfo{}, contracts.Errorf(contracts.ErrNotFound, "no node matches the selector")
	}
	if sel.LeastLoaded {
		sort.SliceStable(cands, func(i, j int) bool { return cands[i].load < cands[j].load })
	}
	return cands[0].ni, cands[0].hi, nil
}

func (h *Hub) validateAccountPolicy(ctx context.Context, hn contracts.Harness, pol *contracts.AccountPolicy) error {
	if pol.Mode == "" {
		pol.Mode = contracts.AccountEligible
	}
	switch pol.Mode {
	case contracts.AccountEligible, contracts.AccountAmbient:
		if pol.AccountID != "" {
			return contracts.Errorf(contracts.ErrInvalid, "account_id is only valid with mode explicit")
		}
	case contracts.AccountExplicit:
		if pol.AccountID == "" {
			return contracts.Errorf(contracts.ErrInvalid, "account policy explicit requires account_id")
		}
	default:
		return contracts.Errorf(contracts.ErrInvalid, "unknown account policy mode %q", pol.Mode)
	}
	prov := contracts.ProviderFor(hn)
	// A folded account's ID names its survivor (accounts_merge.go).
	pol.AccountID = h.resolveAccountAlias(ctx, h.db, pol.AccountID)
	for i, id := range pol.Prefer {
		pol.Prefer[i] = h.resolveAccountAlias(ctx, h.db, id)
	}
	ids := append([]string{}, pol.Prefer...)
	if pol.AccountID != "" {
		ids = append(ids, pol.AccountID)
	}
	for _, id := range ids {
		acct, err := h.getAccount(ctx, h.db, id)
		if contracts.IsCode(err, contracts.ErrNotFound) {
			if id == pol.AccountID {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if prov == "" || acct.Provider != prov {
			return contracts.Errorf(contracts.ErrInvalid, "account %s is a %s account; harness %s never uses another provider's account", id, acct.Provider, hn).WithRef(contracts.KindAccount, id)
		}
		if id == pol.AccountID && acct.Disabled && pol.Strict { // non-strict: the node falls back
			return contracts.Errorf(contracts.ErrConflict, "account %s is disabled", id).WithRef(contracts.KindAccount, id)
		}
	}
	return nil
}

// StartSession creates a session and its first run on a validated node and
// queues the launch command. It returns once the session is durably stored;
// observe the run for the launch outcome.
func (h *Hub) StartSession(ctx context.Context, req contracts.StartSessionRequest) (contracts.StartSessionResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.StartSessionResponse{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.StartSessionResponse{}, err
	}
	if req.Harness == "" {
		return contracts.StartSessionResponse{}, contracts.Errorf(contracts.ErrInvalid, "harness is required")
	}
	if p.Kind == contracts.PrincipalSession {
		req.ParentSessionID = p.ID // set automatically; cannot be spoofed
	}
	byNode := nodeStarter(p)
	if byNode {
		if err := checkNodeStart(p, &req); err != nil { // nodestart.go
			return contracts.StartSessionResponse{}, err
		}
	}
	var ni nodeInfo
	var hi contracts.HarnessInfo
	var launchCtx *contracts.LaunchContext
	replay := h.opExists(ctx, req.OperationID)
	switch {
	case replay:
		// A retry returns the stored result even if the node went away.
	case req.NodeID != "":
		ni, err = h.nodeInfo(ctx, req.NodeID)
		if err == nil {
			hi, err = h.checkLaunchTarget(ctx, ni, req.Harness)
		}
	case req.NodeSelector != nil:
		ni, hi, err = h.selectNode(ctx, req.NodeSelector, req.Harness)
	default:
		err = contracts.Errorf(contracts.ErrInvalid, "node_id or node_selector is required")
	}
	if err != nil {
		return contracts.StartSessionResponse{}, err
	}
	if req.AccountPolicy.Mode == "" {
		req.AccountPolicy.Mode = contracts.AccountEligible
	}
	// The operation records the request as sent: the first execution
	// below fills in the parent's project and resolves account aliases, a
	// replay skips that, and both must hash to the same body (a retried
	// child start conflicted with itself).
	sent := req
	sent.AccountPolicy.Prefer = slices.Clone(req.AccountPolicy.Prefer)
	if !replay {
		if err := h.validateAccountPolicy(ctx, req.Harness, &req.AccountPolicy); err != nil {
			return contracts.StartSessionResponse{}, err
		}
		// A child started without a project joins its parent's project
		// (an explicit "" keeps it outside any project).
		if req.ProjectID == nil && req.ParentSessionID != "" {
			if parent, err := h.getSession(ctx, h.db, req.ParentSessionID); err == nil && parent.ProjectID != nil && *parent.ProjectID != "" {
				pid := *parent.ProjectID
				req.ProjectID = &pid
			}
		}
		if req.ProjectID != nil && *req.ProjectID != "" && h.cfg.Projects != nil {
			if _, err := h.cfg.Projects.GetProject(ctx, contracts.GetRequest{ID: *req.ProjectID}); err != nil {
				return contracts.StartSessionResponse{}, err
			}
		}
		if req.IncludeContext && h.cfg.Notes != nil {
			lc, err := h.cfg.Notes.GetLaunchContext(ctx, contracts.ContextRequest{ProjectID: req.ProjectID})
			if err == nil {
				launchCtx = &lc
			}
		}
		// Every session: the project's fact sheet and the standing grants
		// it holds (launchfacts.go).
		if !byNode { // a headless run injects nothing
			text, refs := h.standingContext(ctx, req.ProjectID, req.ParentSessionID)
			launchCtx = withStandingContext(launchCtx, text, refs)
		}
	}
	var out contracts.StartSessionResponse
	err = h.mutate(ctx, req.OperationID, contracts.OpStartSession, p, sent, &out, func(tx *sql.Tx) (any, error) {
		if req.ParentSessionID != "" {
			if _, err := h.getSession(ctx, tx, req.ParentSessionID); err != nil {
				return nil, contracts.Errorf(contracts.ErrInvalid, "parent session %s not found", req.ParentSessionID)
			}
		}
		now := h.now()
		projectID := req.ProjectID
		if projectID != nil && *projectID == "" {
			projectID = nil
		}
		tags := req.Tags
		if tags == nil {
			tags = []string{}
		}
		title := req.Title
		if title == "" {
			title = defaultTitle(req)
		}
		tags = autotitle.LaunchTags(p, req.Title, req.Harness, tags)                                           // W-names
		tags = reportTags(tags, req.ParentSessionID, req.ReportToParent, p.Kind == contracts.PrincipalSession) // W-childnotify
		if byNode {
			tags = forceNodeStartTags(tags)
		}
		sess := contracts.Session{
			SessionID: contracts.NewID(contracts.PrefixSession), Harness: req.Harness, Title: title, ProjectID: projectID,
			CreatedBy: p, CreatedAt: now, Lineage: contracts.Lineage{ParentSessionID: req.ParentSessionID}, Tags: tags,
		}
		run := contracts.Run{
			RunID: contracts.NewID(contracts.PrefixRun), SessionID: sess.SessionID, NodeID: ni.ID, Harness: req.Harness, Cwd: req.Cwd,
			Runtime: contracts.RuntimeVersion{Harness: req.Harness, Version: hi.Version, Executable: hi.Executable},
			State:   contracts.RunStarting, StateReason: "launch queued", Freshness: contracts.Freshness{ObservedAt: now, Source: "hub"},
			StartedAt: now, Capabilities: append([]string{}, hi.Capabilities...),
		}
		sess.CurrentRunID = run.RunID
		if err := h.insertSessionTx(ctx, tx, sess); err != nil {
			return nil, err
		}
		cmd, err := h.enqueueTx(ctx, tx, ni.ID, contracts.CmdStartRun, p, contracts.StartRunCommand{
			Session: sess, RunID: run.RunID, Cwd: req.Cwd, Checkout: req.Checkout, EnvProfile: req.EnvProfile,
			AccountPolicy: req.AccountPolicy, InitialPrompt: req.InitialPrompt, Args: req.Args, LaunchContext: launchCtx,
		}, h.opt.StartCommandTTL, contracts.ObjectRef{Kind: contracts.KindRun, ID: run.RunID})
		if err != nil {
			return nil, err
		}
		if err := h.putRunTx(ctx, tx, run, 0, cmd.CommandID); err != nil {
			return nil, err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionCreated, sess, "hub", 0); err != nil {
			return nil, err
		}
		if err := h.emitRun(ctx, tx, contracts.EvRunStarted, run, projectID, "hub", 0); err != nil {
			return nil, err
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionStarted, Actor: actorOf(p), NodeID: ni.ID, SessionID: sess.SessionID, RunID: run.RunID, To: string(req.Harness)})
		v, err := h.sessionView(ctx, tx, sess, false)
		if err != nil {
			return nil, err
		}
		if ni.Name != "" {
			// Inside the transaction sessionView cannot ask the fleet
			// registry and falls back to the hello hostname; two nodes on
			// one host would look alike. ni was resolved before the tx.
			v.NodeName = ni.Name
		}
		return contracts.StartSessionResponse{Session: v, CommandID: cmd.CommandID}, nil
	})
	if err == nil {
		h.withNodeName(ctx, &out.Session)
	}
	return out, err
}

func defaultTitle(req contracts.StartSessionRequest) string {
	base := req.Cwd
	if req.Checkout != nil && req.Checkout.Branch != "" {
		base = req.Checkout.Branch
	}
	if i := strings.LastIndex(strings.TrimRight(base, "/"), "/"); i >= 0 {
		base = strings.TrimRight(base, "/")[i+1:]
	}
	if base == "" {
		base = string(req.Harness)
	}
	return base
}

// GetSession returns a session with its current run and freshness.
func (h *Hub) GetSession(ctx context.Context, req contracts.GetRequest) (contracts.SessionView, error) {
	if _, err := principal(ctx); err != nil {
		return contracts.SessionView{}, err
	}
	s, err := h.getSession(ctx, h.db, req.ID)
	if err != nil {
		return contracts.SessionView{}, err
	}
	v, err := h.sessionView(ctx, h.db, s, true)
	if err == nil {
		views := []contracts.SessionView{v}
		h.attachMonitors(ctx, h.db, views)
		v = views[0]
	}
	return v, err
}

// ListSessions lists sessions newest first. State filters apply to the
// presented (freshness-adjusted) state. Monitor sessions are left out
// unless req.IncludeMonitors; each view lists the live monitors that wake
// its session.
func (h *Hub) ListSessions(ctx context.Context, req contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Page[contracts.SessionView]{}, err
	}
	page, err := h.listSessions(ctx, req)
	if err == nil {
		h.attachMonitors(ctx, h.db, page.Items)
	}
	return page, err
}

func (h *Hub) listSessions(ctx context.Context, req contracts.ListSessionsRequest) (contracts.Page[contracts.SessionView], error) {
	// A list of shell sessions alone is how CLIs before include_monitors
	// list monitors (pyre monitor ls/rm): it keeps them.
	includeMonitors := req.IncludeMonitors || req.Harness == contracts.HarnessShell
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return contracts.Page[contracts.SessionView]{}, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)
	where := []string{"1=1"}
	args := []any{}
	if !req.IncludeArchived {
		where = append(where, "s.archived = 0")
	}
	if req.ProjectID != nil {
		if *req.ProjectID == "" {
			where = append(where, "s.project_id IS NULL")
		} else {
			where = append(where, "s.project_id = ?")
			args = append(args, *req.ProjectID)
		}
	}
	if req.Harness != "" {
		where = append(where, "s.harness = ?")
		args = append(args, string(req.Harness))
	}
	if req.NodeID != "" {
		where = append(where, "r.node_id = ?")
		args = append(args, req.NodeID)
	}
	if req.Query != "" {
		where = append(where, "(s.title LIKE ? OR s.session_id = ?)")
		args = append(args, "%"+req.Query+"%", req.Query)
	}
	states := map[contracts.RunState]bool{}
	for _, st := range req.States {
		states[st] = true
	}
	page := contracts.Page[contracts.SessionView]{Items: []contracts.SessionView{}, SnapshotAt: h.now()}
	ctx = withNodeMemo(ctx)
	curK, curI := pc.K, pc.I
	for {
		w := append([]string{}, where...)
		a := append([]any{}, args...)
		if curK != "" {
			w = append(w, "(s.created_at < ? OR (s.created_at = ? AND s.session_id < ?))")
			a = append(a, curK, curK, curI)
		}
		const batch = 100
		a = append(a, batch)
		rows, err := h.db.QueryContext(ctx, `SELECT s.session_id,s.harness,s.title,s.project_id,s.created_by,s.created_at,s.current_run_id,s.lineage,s.tags,s.archived,s.icon,s.owner_active_at,r.run
			FROM sessions s LEFT JOIN runs r ON r.run_id = s.current_run_id WHERE `+strings.Join(w, " AND ")+` ORDER BY s.created_at DESC, s.session_id DESC LIMIT ?`, a...)
		if err != nil {
			return page, err
		}
		var batchSessions []contracts.Session
		var bodies []sql.NullString
		for rows.Next() {
			var body sql.NullString
			s, err := scanSession(extraScan{sc: rows, extra: []any{&body}})
			if err != nil {
				rows.Close()
				return page, err
			}
			batchSessions = append(batchSessions, s)
			bodies = append(bodies, body)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return page, err
		}
		h.prefetchList(ctx, h.db, batchSessions, bodies) // W-dbhygiene: no per-session run/approval/lease queries
		for _, s := range batchSessions {
			curK, curI = persistence.Time(s.CreatedAt), s.SessionID
			if !includeMonitors && s.IsMonitor() {
				continue
			}
			v, err := h.sessionView(ctx, h.db, s, true)
			if err != nil {
				return page, err
			}
			if len(states) > 0 && (v.Run == nil || !states[v.Run.State]) {
				continue
			}
			page.Items = append(page.Items, v)
			if len(page.Items) == limit {
				page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{K: curK, I: curI})
				return page, nil
			}
		}
		if len(batchSessions) < batch {
			return page, nil
		}
	}
}

// ListRuns lists a session's runs, newest first.
func (h *Hub) ListRuns(ctx context.Context, req contracts.ListRunsRequest) (contracts.Page[contracts.Run], error) {
	if _, err := principal(ctx); err != nil {
		return contracts.Page[contracts.Run]{}, err
	}
	if _, err := h.getSession(ctx, h.db, req.SessionID); err != nil {
		return contracts.Page[contracts.Run]{}, err
	}
	pc, err := persistence.DecodeCursor(req.Cursor)
	if err != nil {
		return contracts.Page[contracts.Run]{}, err
	}
	limit := persistence.ClampLimit(req.Limit, persistence.DefaultLimit, persistence.MaxLimit)
	q := `SELECT run, started_at, run_id FROM runs WHERE session_id=?`
	args := []any{req.SessionID}
	if pc.K != "" {
		q += ` AND (started_at < ? OR (started_at = ? AND run_id < ?))`
		args = append(args, pc.K, pc.K, pc.I)
	}
	q += ` ORDER BY started_at DESC, run_id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		return contracts.Page[contracts.Run]{}, err
	}
	var runs []contracts.Run
	var keys [][2]string
	for rows.Next() {
		var body, k, id string
		if err := rows.Scan(&body, &k, &id); err != nil {
			rows.Close()
			return contracts.Page[contracts.Run]{}, err
		}
		var r contracts.Run
		if err := json.Unmarshal([]byte(body), &r); err != nil {
			rows.Close()
			return contracts.Page[contracts.Run]{}, err
		}
		runs = append(runs, r)
		keys = append(keys, [2]string{k, id})
	}
	rows.Close()
	page := contracts.Page[contracts.Run]{Items: []contracts.Run{}, SnapshotAt: h.now()}
	for i, r := range runs {
		if i == limit {
			page.NextCursor = persistence.EncodeCursor(persistence.PageCursor{K: keys[i-1][0], I: keys[i-1][1]})
			break
		}
		h.decorateRun(ctx, h.db, &r)
		page.Items = append(page.Items, r)
	}
	return page, nil
}

// currentRunFor returns the session's current run, checking an explicit
// run ID matches it.
func (h *Hub) currentRunFor(ctx context.Context, q queryer, sessionID, runID string) (contracts.Session, contracts.Run, error) {
	s, err := h.getSession(ctx, q, sessionID)
	if err != nil {
		return s, contracts.Run{}, err
	}
	if s.CurrentRunID == "" {
		return s, contracts.Run{}, contracts.Errorf(contracts.ErrConflict, "session has no run").WithRef(contracts.KindSession, sessionID)
	}
	if runID != "" && runID != s.CurrentRunID {
		return s, contracts.Run{}, contracts.Errorf(contracts.ErrConflict, "run %s is not the session's current run (%s)", runID, s.CurrentRunID).WithRef(contracts.KindRun, runID)
	}
	r, err := h.getRun(ctx, q, s.CurrentRunID)
	return s, r, err
}

func (h *Hub) runControl(ctx context.Context, req contracts.SessionCommandRequest, op string, kind contracts.CommandKind, capName string, ttl time.Duration) (contracts.CommandReceipt, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.CommandReceipt{}, err
	}
	var cmdID string
	err = h.mutate(ctx, req.OperationID, op, p, req, &cmdID, func(tx *sql.Tx) (any, error) {
		_, run, err := h.currentRunFor(ctx, tx, req.SessionID, req.RunID)
		if err != nil {
			return nil, err
		}
		if !contracts.HasCap(run.Capabilities, capName) {
			return nil, contracts.Errorf(contracts.ErrUnsupported, "run adapter does not support %s", capName).WithRef(contracts.KindRun, run.RunID)
		}
		if run.State.Terminal() {
			return nil, contracts.Errorf(contracts.ErrConflict, "run has already %s", run.State).WithRef(contracts.KindRun, run.RunID)
		}
		cmd, err := h.enqueueTx(ctx, tx, run.NodeID, kind, p, contracts.RunCommand{SessionID: run.SessionID, RunID: run.RunID, Reason: req.Reason}, ttl,
			contracts.ObjectRef{Kind: contracts.KindRun, ID: run.RunID})
		if err != nil {
			return nil, err
		}
		action := contracts.AuditSessionStopped
		if kind == contracts.CmdInterrupt {
			action = contracts.AuditSessionInterrupted
		}
		h.auditTx(ctx, tx, auditRec{Action: action, Actor: actorOf(p), NodeID: run.NodeID, SessionID: run.SessionID, RunID: run.RunID, Reason: req.Reason})
		return cmd.CommandID, nil
	})
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	rec, err := h.waitCommand(ctx, cmdID, h.waitIfConnected(ctx, cmdID, h.opt.ControlWait))
	if err != nil {
		return contracts.CommandReceipt{}, err
	}
	return h.receiptFor(rec), nil
}

// waitIfConnected returns d if the command's node is connected, else 0.
func (h *Hub) waitIfConnected(ctx context.Context, cmdID string, d time.Duration) time.Duration {
	r, err := h.getCommand(ctx, h.db, cmdID)
	if err != nil {
		return 0
	}
	if c, _ := h.nodeConnected(ctx, h.db, r.NodeID); !c {
		return 0
	}
	return d
}

// InterruptSession interrupts the current agent turn (capability interrupt).
func (h *Hub) InterruptSession(ctx context.Context, req contracts.SessionCommandRequest) (contracts.CommandReceipt, error) {
	return h.runControl(ctx, req, contracts.OpInterruptSession, contracts.CmdInterrupt, contracts.CapInterrupt, h.opt.InterruptTTL)
}

// StopSession stops the current run (capability stop).
func (h *Hub) StopSession(ctx context.Context, req contracts.SessionCommandRequest) (contracts.CommandReceipt, error) {
	return h.runControl(ctx, req, contracts.OpStopSession, contracts.CmdStop, contracts.CapStop, h.opt.StopTTL)
}

// ResumeSession creates a new run continuing the session. Native resume
// needs resume.native; a reconstructed handoff needs resume.handoff AND
// AllowHandoff. A session whose current run is observed live cannot be
// resumed (no second current run).
func (h *Hub) ResumeSession(ctx context.Context, req contracts.ResumeRequest) (contracts.StartSessionResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.StartSessionResponse{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.StartSessionResponse{}, err
	}
	if nodeStarter(p) { // nodestart.go: a resume is a start on any node, of any harness
		return contracts.StartSessionResponse{}, contracts.Errorf(contracts.ErrForbidden, "a node credential cannot resume sessions").WithRef(contracts.KindNode, p.ID)
	}
	var prev contracts.Run
	var ni nodeInfo
	var hi contracts.HarnessInfo
	kind := contracts.ContinuationNone
	if req.AccountPolicy.Mode == "" {
		req.AccountPolicy.Mode = contracts.AccountEligible
	}
	if !h.opExists(ctx, req.OperationID) {
		if prev, ni, hi, kind, err = h.planResume(ctx, &req); err != nil {
			return contracts.StartSessionResponse{}, err
		}
	}
	var resumeArgs []string
	if req.EnvFile != "" {
		// Only the path (or "none") travels; the node reads and checks it.
		if !h.opExists(ctx, req.OperationID) && !contracts.HasCap(hi.Capabilities, contracts.CapLaunchEnvFile) {
			return contracts.StartSessionResponse{}, contracts.Errorf(contracts.ErrUnsupported,
				"the node's pyred cannot apply an environment file for %s (an older version); upgrade it", hi.Harness)
		}
		resumeArgs = []string{contracts.StartArgEnvFile + "=" + req.EnvFile}
	}
	var out contracts.StartSessionResponse
	err = h.mutate(ctx, req.OperationID, contracts.OpResumeSession, p, req, &out, func(tx *sql.Tx) (any, error) {
		sess, err := h.getSession(ctx, tx, req.SessionID)
		if err != nil {
			return nil, err
		}
		if sess.CurrentRunID != prev.RunID {
			return nil, contracts.Errorf(contracts.ErrConflict, "session's current run changed to %s; retry", sess.CurrentRunID).WithRef(contracts.KindSession, sess.SessionID)
		}
		cur, err := h.getRun(ctx, tx, prev.RunID)
		if err != nil {
			return nil, err
		}
		view := cur
		h.decorateRun(ctx, tx, &view)
		if !view.State.Terminal() && view.State != contracts.RunUnknown {
			return nil, contracts.Errorf(contracts.ErrConflict, "current run %s is %s; stop it before resuming", cur.RunID, view.State).WithRef(contracts.KindRun, cur.RunID)
		}
		now := h.now()
		run := contracts.Run{
			RunID: contracts.NewID(contracts.PrefixRun), SessionID: sess.SessionID, NodeID: ni.ID, Harness: sess.Harness, Cwd: cur.Cwd, Checkout: cur.Checkout,
			Runtime: contracts.RuntimeVersion{Harness: sess.Harness, Version: hi.Version, Executable: hi.Executable},
			State:   contracts.RunStarting, StateReason: "resume queued", Freshness: contracts.Freshness{ObservedAt: now, Source: "hub"},
			StartedAt: now, Capabilities: append([]string{}, hi.Capabilities...), PreviousRunID: cur.RunID, ContinuationKind: kind,
		}
		cmd, err := h.enqueueTx(ctx, tx, ni.ID, contracts.CmdResume, p, contracts.ResumeCommand{StartRunCommand: contracts.StartRunCommand{
			Session: sess, RunID: run.RunID, Cwd: cur.Cwd, Checkout: nil, AccountPolicy: req.AccountPolicy, PreviousRun: &cur, AllowHandoff: req.AllowHandoff,
			Args: resumeArgs,
		}}, h.opt.StartCommandTTL, contracts.ObjectRef{Kind: contracts.KindRun, ID: run.RunID})
		if err != nil {
			return nil, err
		}
		if err := h.putRunTx(ctx, tx, run, 0, cmd.CommandID); err != nil {
			return nil, err
		}
		sess.CurrentRunID = run.RunID
		if err := h.updateSessionTx(ctx, tx, sess); err != nil {
			return nil, err
		}
		if err := h.withdrawApprovalsTx(ctx, tx, cur.RunID, "its run was replaced"); err != nil {
			return nil, err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionUpdated, sess, "hub", 0); err != nil {
			return nil, err
		}
		if err := h.emitRun(ctx, tx, contracts.EvRunStarted, run, sess.ProjectID, "hub", 0); err != nil {
			return nil, err
		}
		if err := h.routeUnattemptedTx(ctx, tx, sess.SessionID, run); err != nil {
			return nil, err
		}
		how := "native resume"
		if kind == contracts.ContinuationHandoff {
			how = "handoff"
		}
		if ni.ID != cur.NodeID {
			how += " from another machine"
		}
		h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionResumed, Actor: actorOf(p), NodeID: ni.ID, SessionID: sess.SessionID, RunID: run.RunID, To: how})
		v, err := h.sessionView(ctx, tx, sess, false)
		if err != nil {
			return nil, err
		}
		if ni.Name != "" {
			// Inside the transaction sessionView cannot ask the fleet
			// registry and falls back to the hello hostname; two nodes on
			// one host would look alike. ni was resolved before the tx.
			v.NodeName = ni.Name
		}
		return contracts.StartSessionResponse{Session: v, CommandID: cmd.CommandID}, nil
	})
	if err == nil {
		h.withNodeName(ctx, &out.Session)
	}
	return out, err
}

// AssociateSession attaches a session to a project (or detaches with null).
func (h *Hub) AssociateSession(ctx context.Context, req contracts.AssociateRequest) (contracts.SessionView, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.SessionView{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.SessionView{}, err
	}
	if req.ProjectID != nil && *req.ProjectID != "" && h.cfg.Projects != nil {
		if _, err := h.cfg.Projects.GetProject(ctx, contracts.GetRequest{ID: *req.ProjectID}); err != nil {
			return contracts.SessionView{}, err
		}
	}
	return h.editSession(ctx, req.OperationID, contracts.OpAssociateSession, p, req, req.SessionID, nil, func(s *contracts.Session) map[string]any {
		prev := s.ProjectID
		s.ProjectID = req.ProjectID
		if s.ProjectID != nil && *s.ProjectID == "" {
			s.ProjectID = nil
		}
		return map[string]any{"project_id": s.ProjectID, "previous_project_id": prev}
	})
}

// UpdateSession edits title, tags, or archive state. Archiving revokes the
// session's agent tokens.
func (h *Hub) UpdateSession(ctx context.Context, req contracts.UpdateSessionRequest) (contracts.SessionView, error) {
	p, err := principal(ctx)
	if err != nil {
		return contracts.SessionView{}, err
	}
	if err := requireOp(req.Mutation); err != nil {
		return contracts.SessionView{}, err
	}
	if !h.opExists(ctx, req.OperationID) {
		if err := h.checkRename(ctx, p, req); err != nil { // W-names
			return contracts.SessionView{}, err
		}
		if err := h.checkArchive(ctx, p, req); err != nil { // W-childnotify
			return contracts.SessionView{}, err
		}
		if err := h.checkReparent(ctx, p, req); err != nil { // W-reparent
			return contracts.SessionView{}, err
		}
		if err := h.checkNodeRetag(ctx, p, req); err != nil { // nodestart.go
			return contracts.SessionView{}, err
		}
	}
	var reparented *contracts.Session // W-reparent: the session before a parent change made by this call
	if req.Icon != nil {              // W-archive2: any principal may set any session's icon
		icon, err := autotitle.ValidIcon(*req.Icon)
		if err != nil {
			return contracts.SessionView{}, err
		}
		req.Icon = &icon
	}
	// W-archive2: archiving a live session "when done" records the tag;
	// the archive-when-done worker stops the run after its turn and then
	// archives it (archivewhen.go).
	schedule := req.Archived != nil && *req.Archived && req.WhenDone && h.runLive(ctx, req.SessionID)
	auditEdit := func(ctx context.Context, tx *sql.Tx, before, after contracts.Session) {
		act := actorOf(p)
		node := ""
		if r, err := h.getRun(ctx, tx, after.CurrentRunID); err == nil {
			node = r.NodeID
		}
		if before.Parent() != after.Parent() {
			b := before
			reparented = &b
			h.reparentTx(ctx, tx, act, node, before, after)
		}
		if before.Icon != after.Icon {
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionIconSet, Actor: act, NodeID: node, SessionID: after.SessionID, From: before.Icon, To: after.Icon})
		}
		if before.Title != after.Title {
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionRenamed, Actor: act, NodeID: node, SessionID: after.SessionID, From: before.Title, To: after.Title})
		}
		if before.Archived != after.Archived {
			a := contracts.AuditSessionUnarchived
			if after.Archived {
				a = contracts.AuditSessionArchived
			}
			reason := ""
			if hasArchiveWhenDone(before) && after.Archived {
				reason = "it was scheduled to archive when done"
			}
			h.auditTx(ctx, tx, auditRec{Action: a, Actor: act, NodeID: node, SessionID: after.SessionID, Reason: reason})
		}
		switch had, has := hasArchiveWhenDone(before), hasArchiveWhenDone(after); {
		case !had && has:
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionArchiveScheduled, Actor: act, NodeID: node, SessionID: after.SessionID,
				Reason: "archive when the current turn ends"})
		case had && !has && !after.Archived:
			h.auditTx(ctx, tx, auditRec{Action: contracts.AuditSessionArchiveCanceled, Actor: act, NodeID: node, SessionID: after.SessionID})
		}
	}
	v, err := h.editSession(ctx, req.OperationID, contracts.OpUpdateSession, p, req, req.SessionID, auditEdit, func(s *contracts.Session) map[string]any {
		ch := map[string]any{}
		if req.Tags != nil {
			s.Tags = append([]string{}, (*req.Tags)...)
			ch["tags"] = s.Tags
		}
		if req.Icon != nil {
			s.Icon = *req.Icon
			ch["icon"] = *req.Icon
		}
		if req.Parent != nil {
			for k, v := range h.applyReparent(s, reparentTarget(req.Parent)) {
				ch[k] = v
			}
		}
		if req.Title != nil {
			s.Title = *req.Title
			ch["title"] = *req.Title
			s.Tags = autotitle.RenameTags(p, s.Tags) // W-names: an explicit rename ends auto-naming
			ch["tags"] = s.Tags
		}
		switch {
		case schedule:
			if !hasArchiveWhenDone(*s) {
				s.Tags = append(slices.Clone(s.Tags), contracts.TagArchiveWhenDone)
			}
			ch["tags"] = s.Tags
		case req.Archived != nil:
			s.Archived = *req.Archived
			ch["archived"] = *req.Archived
			if hasArchiveWhenDone(*s) { // archived now, or the pending archive is canceled
				s.Tags = withoutTag(s.Tags, contracts.TagArchiveWhenDone)
				ch["tags"] = s.Tags
			}
		}
		return ch
	})
	if err == nil && reparented != nil {
		h.reparentNotices(ctx, p, req.OperationID, v.Session, reparented.Parent())
	}
	if err == nil && req.Archived != nil && *req.Archived && (isOwner(p) || p.Kind == contracts.PrincipalSession) {
		// Its live descendants are archived with it (archivecascade.go); a
		// scheduled archive cascades when it completes.
		if v.Session.Archived {
			h.cascadeArchive(ctx, req.SessionID, req.SessionID)
		}
		v.ArchiveCascade = h.archiveCascadeSummary(ctx, req.SessionID)
	}
	if err == nil && schedule {
		h.noteArchiveScheduled(req.SessionID)
		return v, nil // its run keeps its tokens until the turn ends
	}
	if err == nil && req.Archived != nil && *req.Archived && h.cfg.Tokens != nil {
		if rerr := h.cfg.Tokens.RevokeSessionTokens(ctx, req.SessionID); rerr != nil {
			return v, contracts.Errorf(contracts.ErrUnavailable, "session archived but token revocation failed: %v", rerr)
		}
	}
	return v, err
}

// checkArchive lets a session principal archive (or unarchive) only itself
// and the sessions under it (it started them, or is an ancestor within
// archiveTreeDepth levels), and never archive itself when the owner
// started it; other principals are unaffected. An archive it may not do is
// filed for the owner's approval (approval_requested, owneractions.go).
func (h *Hub) checkArchive(ctx context.Context, p contracts.Principal, req contracts.UpdateSessionRequest) error {
	if req.Archived == nil || p.Kind != contracts.PrincipalSession {
		return nil
	}
	if p.ID == req.SessionID {
		if !*req.Archived {
			return nil
		}
		// A session the owner started is the owner's conversation: it never
		// archives itself (Codex agents archived owner chats right after
		// answering, 2026-10-09). Helpers started by another session may
		// still clean up after themselves.
		s, err := h.getSession(ctx, h.db, req.SessionID)
		if err != nil {
			return err
		}
		if s.CreatedBy.Kind == contracts.PrincipalOwner && s.Parent() == "" && !s.Archived {
			return h.archiveApproval(ctx, p, s) // owneractions.go: filed for the owner's approval
		}
		return nil
	}
	s, err := h.getSession(ctx, h.db, req.SessionID)
	if err != nil {
		return err
	}
	if s.StartedBy(p.ID) || h.isAncestor(ctx, p.ID, s) || s.Archived && *req.Archived {
		return nil // archiving an archived session changes nothing
	}
	if *req.Archived {
		return h.archiveApproval(ctx, p, s) // owneractions.go: filed for the owner's approval
	}
	return contracts.Errorf(contracts.ErrForbidden, "an agent may unarchive only its own session and the sessions under it; the owner can restore %s", req.SessionID).
		WithRef(contracts.KindSession, req.SessionID)
}

// editSession applies edit in a mutation; auditEdit (optional) records the
// change from before to after in the same transaction.
func (h *Hub) editSession(ctx context.Context, opID, op string, p contracts.Principal, req any, sessionID string,
	auditEdit func(ctx context.Context, tx *sql.Tx, before, after contracts.Session), edit func(*contracts.Session) map[string]any) (contracts.SessionView, error) {
	var out contracts.SessionView
	err := h.mutate(ctx, opID, op, p, req, &out, func(tx *sql.Tx) (any, error) {
		s, err := h.getSession(ctx, tx, sessionID)
		if err != nil {
			return nil, err
		}
		before := s
		edit(&s)
		if auditEdit != nil {
			auditEdit(ctx, tx, before, s)
		}
		if err := h.updateSessionTx(ctx, tx, s); err != nil {
			return nil, err
		}
		if err := h.emitSession(ctx, tx, contracts.EvSessionUpdated, s, "hub", 0); err != nil {
			return nil, err
		}
		return h.sessionView(ctx, tx, s, false)
	})
	if err == nil {
		h.withNodeName(ctx, &out)
	}
	return out, err
}

// planResume validates a resume request outside the write transaction.
func (h *Hub) planResume(ctx context.Context, req *contracts.ResumeRequest) (prev contracts.Run, ni nodeInfo, hi contracts.HarnessInfo, kind contracts.ContinuationKind, err error) {
	sess, err := h.getSession(ctx, h.db, req.SessionID)
	if err != nil {
		return prev, ni, hi, kind, err
	}
	if sess.CurrentRunID == "" {
		return prev, ni, hi, kind, contracts.Errorf(contracts.ErrConflict, "session has no previous run to resume").WithRef(contracts.KindSession, sess.SessionID)
	}
	prev, err = h.getRun(ctx, h.db, sess.CurrentRunID)
	if err != nil {
		return prev, ni, hi, kind, err
	}
	nodeID := req.NodeID
	if nodeID == "" {
		nodeID = prev.NodeID
	}
	ni, err = h.nodeInfo(ctx, nodeID)
	if err != nil {
		return prev, ni, hi, kind, err
	}
	hi, err = h.checkLaunchTarget(ctx, ni, sess.Harness)
	if err != nil {
		return prev, ni, hi, kind, err
	}
	switch {
	case contracts.HasCap(prev.Capabilities, contracts.CapResumeNative) && nodeID == prev.NodeID:
		kind = contracts.ContinuationNativeResume
	case contracts.HasCap(prev.Capabilities, contracts.CapResumeHandoff) && req.AllowHandoff:
		kind = contracts.ContinuationHandoff
	case contracts.HasCap(prev.Capabilities, contracts.CapResumeNative) && !req.AllowHandoff:
		return prev, ni, hi, kind, contracts.Errorf(contracts.ErrUnsupported, "native resume is only supported on the run's own node (%s); set allow_handoff for a reconstructed continuation", prev.NodeID)
	default:
		return prev, ni, hi, kind, contracts.Errorf(contracts.ErrUnsupported, "run adapter supports neither native resume nor (allowed) handoff").WithRef(contracts.KindRun, prev.RunID)
	}
	if err := h.validateAccountPolicy(ctx, sess.Harness, &req.AccountPolicy); err != nil {
		return prev, ni, hi, kind, err
	}
	return prev, ni, hi, kind, nil
}

// opExists reports whether an operation ID was already recorded (so a retry
// skips pre-validation and replays the stored result).
func (h *Hub) opExists(ctx context.Context, opID string) bool {
	var one int
	return h.db.QueryRowContext(ctx, `SELECT 1 FROM operations WHERE operation_id=?`, opID).Scan(&one) == nil
}
