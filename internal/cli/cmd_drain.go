package cli

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	c "github.com/jhh3/pyre/internal/contracts"
)

// `pyre node drain <node>` and `pyre node resume <node>`: before a reboot,
// tell every live session on a node to reach a stopping point (and tag it
// drained:<node_id>); after the reboot, natively resume the sessions the
// reboot interrupted and tell them what happened. Client-side only: it
// uses sessions.list, sessions.update (tags), messages.send and
// sessions.resume. Safe to rerun: tags are idempotent and message IDs are
// stable per node+session+drain.

// drainTagPrefix marks a session interrupted by a drain of node <id>.
const drainTagPrefix = "drained:"

// exitNotObserved is the state_reason fragment pyred records for runs it
// could not find after a restart ("run process not found after pyred
// restart (exit not observed)").
const exitNotObserved = "exit not observed"

func drainTag(nodeID string) string { return drainTagPrefix + nodeID }

// ---- tags ----

// withTag returns tags plus tag, and whether that changed anything.
func withTag(tags []string, tag string) ([]string, bool) {
	if slices.Contains(tags, tag) {
		return tags, false
	}
	return append(slices.Clone(tags), tag), true
}

// withoutTag returns tags minus every copy of tag, and whether that changed
// anything.
func withoutTag(tags []string, tag string) ([]string, bool) {
	if !slices.Contains(tags, tag) {
		return tags, false
	}
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		if t != tag {
			out = append(out, t)
		}
	}
	return out, true
}

// setSessionTag adds (present) or removes a tag: it reads the session's
// current tags and writes the full list only when it changes.
func (o *Ops) setSessionTag(ctx context.Context, sessionID, tag string, present bool) (bool, error) {
	sv, err := o.API.GetSession(ctx, c.GetRequest{ID: sessionID})
	if err != nil {
		return false, err
	}
	var tags []string
	var changed bool
	if present {
		tags, changed = withTag(sv.Session.Tags, tag)
	} else {
		tags, changed = withoutTag(sv.Session.Tags, tag)
	}
	if !changed {
		return false, nil
	}
	_, err = o.API.UpdateSession(ctx, c.UpdateSessionRequest{Mutation: mut(""), SessionID: sessionID, Tags: &tags})
	return err == nil, err
}

// ---- stable message IDs ----

// idPart is the body of an ID (after "prefix_"), reduced to the characters
// a message ID allows.
func idPart(id string) string {
	if i := strings.IndexByte(id, '_'); i >= 0 {
		id = id[i+1:]
	}
	b := []byte(id)
	for i, ch := range b {
		if !(ch >= '0' && ch <= '9' || ch >= 'A' && ch <= 'Z' || ch >= 'a' && ch <= 'z' || ch == '-' || ch == '.' || ch == ':') {
			b[i] = '-'
		}
	}
	if len(b) == 0 {
		return "x"
	}
	return string(b)
}

func drainMessageID(nodeID, sessionID string, minute int64) string {
	return "msg_drain-" + idPart(nodeID) + "-" + strconv.FormatInt(minute, 10) + "-" + idPart(sessionID)
}

func resumeNoteID(sessionID, runID string) string {
	return "msg_resumed-" + idPart(sessionID) + "-" + idPart(runID)
}

// ---- texts ----

func drainText(nodeName string, minutes int) string {
	return fmt.Sprintf("[from the owner] %s is shutting down in about %d minutes. Get to a good stopping point: "+
		"finish or pause your current step, don't start anything long-running, save or commit work in progress, "+
		"and write down where you are and what comes next. Then end your turn and wait. You'll be resumed after "+
		"the restart; when you are, check the state of your last step before continuing. No reply needed.", nodeName, minutes)
}

func drainStopText(nodeName string) string {
	return fmt.Sprintf("[from the owner] STOP NOW. %s is being powered off in about 2 minutes and anything still "+
		"running will be killed. Do not start any new step, command or tool call. Stop background jobs you started. "+
		"Save or commit work in progress, write down where you are and what comes next, then end your turn and wait. "+
		"You'll be resumed after the restart. No reply needed.", nodeName)
}

func restartNoteText(nodeName string) string {
	return fmt.Sprintf("[from the owner] %s has restarted and you were resumed. Commands, background jobs and servers "+
		"you had running were stopped by the reboot. If you were in the middle of work, check the state of your last "+
		"step before continuing, then carry on. If you were waiting for input, keep waiting. No reply needed.", nodeName)
}

// ---- selection ----

// drainSelect picks the node's sessions to drain: not archived, current run
// on the node and not ended (exiting runs are already on their way out).
// Shell sessions are reported as skipped: a message would be typed into
// the shell as a command.
func drainSelect(svs []c.SessionView, nodeID string) (drain []c.SessionView, skipped []nodeOpItem) {
	for _, sv := range svs {
		r := sv.Run
		if sv.Session.Archived || r == nil || r.NodeID != nodeID || r.State.Terminal() || r.State == c.RunExiting {
			continue
		}
		if sv.Session.Harness == c.HarnessShell {
			it := newItem(sv)
			it.Action, it.Reason = "skipped", "shell session (a message would run as a command)"
			skipped = append(skipped, it)
			continue
		}
		drain = append(drain, sv)
	}
	return drain, skipped
}

// working reports a run state that is still doing something a power-off
// would interrupt.
func working(s c.RunState) bool {
	return s == c.RunRunning || s == c.RunWaitingBackground || s == c.RunStarting
}

// liveState reports a run that is up and can take input.
func liveState(s c.RunState) bool {
	switch s {
	case c.RunRunning, c.RunWaitingInput, c.RunWaitingBackground, c.RunWaitingApproval:
		return true
	}
	return false
}

// Resume actions.
const (
	resumeLaunch   = "resume"   // ended by the reboot: native resume, then note
	resumeNote     = "note"     // still live (reconnected by itself): note only
	resumeWait     = "wait"     // a launch is already starting: wait, then note
	resumeSkip     = "skip"     // ended deliberately: untag, leave alone
	resumeKeep     = "keep"     // can't tell yet: keep the tag for a rerun
	resumeArchived = "archived" // archived since: untag
)

// classifyResume decides what `node resume` does with a drained session.
func classifyResume(sv c.SessionView, all bool) (action, reason string) {
	if sv.Session.Archived {
		return resumeArchived, "archived"
	}
	r := sv.Run
	if r == nil {
		if all {
			return resumeLaunch, "no run (--all)"
		}
		return resumeSkip, "no run"
	}
	switch {
	case liveState(r.State):
		return resumeNote, "run is live (" + string(r.State) + ")"
	case r.State == c.RunStarting:
		return resumeWait, "run is starting"
	case r.State == c.RunExiting:
		return resumeKeep, "run is exiting; rerun shortly"
	case r.State == c.RunUnknown:
		return resumeKeep, "run state unknown (is the node back?)"
	case !r.State.Terminal():
		return resumeKeep, "run state " + string(r.State)
	}
	reason = string(r.State)
	if r.StateReason != "" {
		reason += ": " + r.StateReason
	}
	switch {
	case all:
		return resumeLaunch, reason + " (--all)"
	case strings.Contains(strings.ToLower(r.StateReason), exitNotObserved):
		return resumeLaunch, reason
	case r.State == c.RunFailed && (r.PreviousRunID != "" || r.ContinuationKind == c.ContinuationNativeResume || r.ContinuationKind == c.ContinuationHandoff):
		return resumeLaunch, "resume launch failed: " + reason
	}
	return resumeSkip, reason
}

// ---- results ----

type nodeOpItem struct {
	SessionID   string `json:"session_id"`
	Title       string `json:"title,omitempty"`
	Harness     string `json:"harness,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	State       string `json:"state,omitempty"`
	StateReason string `json:"state_reason,omitempty"`
	Action      string `json:"action"`
	Reason      string `json:"reason,omitempty"`
	MessageID   string `json:"message_id,omitempty"`
	Delivery    string `json:"delivery,omitempty"`
	NewRunID    string `json:"new_run_id,omitempty"`
	NewState    string `json:"new_state,omitempty"`
	Tagged      bool   `json:"tagged,omitempty"`
	Untagged    bool   `json:"untagged,omitempty"`
	Error       string `json:"error,omitempty"`
}

func newItem(sv c.SessionView) nodeOpItem {
	it := nodeOpItem{SessionID: sv.Session.SessionID, Title: sv.Session.Title, Harness: string(sv.Session.Harness)}
	if sv.Run != nil {
		it.RunID, it.State, it.StateReason = sv.Run.RunID, string(sv.Run.State), sv.Run.StateReason
	}
	return it
}

type drainResult struct {
	NodeID       string         `json:"node_id"`
	NodeName     string         `json:"node_name"`
	Tag          string         `json:"tag"`
	DryRun       bool           `json:"dry_run,omitempty"`
	Minutes      int            `json:"minutes"`
	Sessions     []nodeOpItem   `json:"sessions"`
	Skipped      []nodeOpItem   `json:"skipped,omitempty"`
	Counts       map[string]int `json:"counts"`
	Failures     int            `json:"failures"`
	Waited       bool           `json:"waited,omitempty"`
	StillWorking []string       `json:"still_working,omitempty"`
	StopSent     []nodeOpItem   `json:"stop_sent,omitempty"`
	FollowUp     string         `json:"follow_up"`
}

type resumeResult struct {
	NodeID   string         `json:"node_id"`
	NodeName string         `json:"node_name"`
	Tag      string         `json:"tag"`
	DryRun   bool           `json:"dry_run,omitempty"`
	All      bool           `json:"all,omitempty"`
	Sessions []nodeOpItem   `json:"sessions"`
	Counts   map[string]int `json:"counts"`
	Failures int            `json:"failures"`
}

// ---- pacing ----

// pace scales a production duration by PollInterval (1s by default), so
// tests with a short PollInterval run fast.
func (a *App) pace(d time.Duration) time.Duration {
	if a.PollInterval <= 0 {
		return d
	}
	return time.Duration(float64(d) * float64(a.PollInterval) / float64(time.Second))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ---- commands ----

func (a *App) addDrainCommands(nodes *cobra.Command) {
	var minutes int
	var text string
	var wait, dry bool
	drain := &cobra.Command{
		Use:   "drain <node> [--minutes N] [--message TEXT] [--wait] [--dry-run]",
		Short: "Before a reboot: tell every live session on a node to reach a stopping point",
		Long: `Tags every live session on the node drained:<node_id> and sends it an owner
message asking it to reach a stopping point within about N minutes. With
--wait, polls until no tagged session is working (or the deadline, when the
ones still working get a final steered STOP NOW message). After the reboot,
run: pyre node resume <node>. Safe to rerun.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if minutes <= 0 {
				return usagef("--minutes must be > 0")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			res, err := a.nodeDrain(cmd.Context(), ops, args[0], minutes, text, wait, dry)
			if err != nil {
				return err
			}
			if err := a.emit(res, func(w io.Writer) { printDrain(w, res) }); err != nil || res.Failures == 0 {
				return err
			}
			return withStatus(ExitError, fmt.Errorf("%d failure(s); rerun to retry", res.Failures))
		}),
	}
	drain.Flags().IntVar(&minutes, "minutes", 10, "minutes until shutdown")
	drain.Flags().StringVar(&text, "message", "", "replace the default message")
	drain.Flags().BoolVar(&wait, "wait", false, "wait until the sessions stop working (or the deadline)")
	drain.Flags().BoolVar(&dry, "dry-run", false, "list what would be drained; tag and send nothing")
	nodes.AddCommand(drain)

	var all, rdry bool
	resume := &cobra.Command{
		Use:   "resume <node> [--all] [--dry-run]",
		Short: "After a reboot: resume the sessions a drain tagged and the reboot interrupted",
		Long: `For every session tagged drained:<node_id>: natively resumes those whose run
the reboot ended (state reason "exit not observed") or whose resume launch
failed, sends sessions that are still live a restart note, and leaves
deliberately ended sessions alone (--all resumes every ended one). The tag
is removed once a session is handled and kept when a resume fails, so a
rerun retries.`,
		Args: cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			res, err := a.nodeResume(cmd.Context(), ops, args[0], all, rdry)
			if err != nil {
				return err
			}
			if err := a.emit(res, func(w io.Writer) { printNodeResume(w, res) }); err != nil || res.Failures == 0 {
				return err
			}
			return withStatus(ExitError, fmt.Errorf("%d failure(s); rerun to retry", res.Failures))
		}),
	}
	resume.Flags().BoolVar(&all, "all", false, "resume every ended tagged session, whatever its end reason")
	resume.Flags().BoolVar(&rdry, "dry-run", false, "list what would be done; resume, send and untag nothing")
	nodes.AddCommand(resume)
}

func (a *App) progressf(format string, args ...any) {
	if a.Stderr != nil {
		fmt.Fprintf(a.Stderr, format+"\n", args...)
	}
}

func (a *App) nodeDrain(ctx context.Context, ops *Ops, ref string, minutes int, text string, wait, dry bool) (drainResult, error) {
	node, err := ops.ResolveNode(ctx, ref)
	if err != nil {
		return drainResult{}, err
	}
	page, err := ops.ListSessions(ctx, SessionFilter{Node: node.NodeID})
	if err != nil {
		return drainResult{}, err
	}
	targets, skipped := drainSelect(page.Items, node.NodeID)
	tag := drainTag(node.NodeID)
	res := drainResult{NodeID: node.NodeID, NodeName: node.Name, Tag: tag, DryRun: dry, Minutes: minutes,
		Sessions: []nodeOpItem{}, Skipped: skipped, Counts: map[string]int{},
		FollowUp: "after the restart: pyre node resume " + node.Name}
	if text == "" {
		text = drainText(node.Name, minutes)
	}
	minute := time.Now().Unix() / 60
	for _, sv := range targets {
		it := newItem(sv)
		res.Counts[it.State]++
		id := sv.Session.SessionID
		self := id == ops.Self.SessionID
		it.MessageID = drainMessageID(node.NodeID, id, minute)
		if self {
			it.MessageID = ""
		}
		if dry {
			it.Action = "would drain"
			if self {
				it.Action, it.Reason = "would tag", "the calling session (not messaged)"
			}
			res.Sessions = append(res.Sessions, it)
			continue
		}
		it.Action = "drained"
		if it.Tagged, err = ops.setSessionTag(ctx, id, tag, true); err != nil {
			it.Action, it.Error = "failed", "tag: "+err.Error()
			res.Failures++
			res.Sessions = append(res.Sessions, it)
			continue
		}
		if self {
			it.Action, it.Reason = "tagged", "the calling session (not messaged)"
			res.Sessions = append(res.Sessions, it)
			continue
		}
		m, err := ops.Send(ctx, SendParams{To: id, Text: text, MessageID: it.MessageID})
		if err != nil {
			it.Action, it.Error = "failed", "send: "+err.Error()
			res.Failures++
		} else {
			it.Delivery = string(m.Status.Runtime)
		}
		res.Sessions = append(res.Sessions, it)
	}
	if !wait || dry || len(targets) == 0 {
		return res, nil
	}
	res.Waited = true
	ids := map[string]bool{}
	for _, it := range res.Sessions {
		if it.Error == "" && it.SessionID != ops.Self.SessionID {
			ids[it.SessionID] = true
		}
	}
	deadline := time.Now().Add(time.Duration(minutes) * time.Minute)
	var still []c.SessionView
	for {
		if err := sleepCtx(ctx, a.pace(15*time.Second)); err != nil {
			return res, err
		}
		page, err := ops.ListSessions(ctx, SessionFilter{Node: node.NodeID})
		if err != nil {
			a.warnf("drain: listing sessions: %v", err)
			continue
		}
		still = still[:0]
		by := map[c.RunState]int{}
		for _, sv := range page.Items {
			if ids[sv.Session.SessionID] && sv.Run != nil && sv.Run.NodeID == node.NodeID && working(sv.Run.State) {
				still = append(still, sv)
				by[sv.Run.State]++
			}
		}
		left := time.Until(deadline).Round(time.Second)
		if len(still) == 0 {
			a.progressf("drain: no drained session on %s is working", node.Name)
			return res, nil
		}
		a.progressf("drain: %d of %d still working (%d running, %d waiting_on_background, %d starting); %s left",
			len(still), len(ids), by[c.RunRunning], by[c.RunWaitingBackground], by[c.RunStarting], max(left, 0))
		if left <= 0 {
			break
		}
	}
	stop := drainStopText(node.Name)
	for _, sv := range still {
		res.StillWorking = append(res.StillWorking, sv.Session.SessionID)
		it := newItem(sv)
		it.Action = "stop sent"
		it.MessageID = drainMessageID(node.NodeID, sv.Session.SessionID, minute) + "-stop"
		m, err := ops.sendSteerOrQueue(ctx, sv, stop, it.MessageID)
		if err != nil {
			it.Action, it.Error = "failed", "send: "+err.Error()
			res.Failures++
		} else {
			it.MessageID, it.Delivery = m.Envelope.MessageID, string(m.Status.Runtime)
		}
		res.StopSent = append(res.StopSent, it)
	}
	return res, nil
}

// sendSteerOrQueue steers when the run advertises it, and sends normally
// (under a distinct ID) when the runtime refuses the steer.
func (o *Ops) sendSteerOrQueue(ctx context.Context, sv c.SessionView, text, msgID string) (c.MessageView, error) {
	if sv.Run != nil && c.HasCap(sv.Run.Capabilities, c.CapInputSteer) {
		m, err := o.Send(ctx, SendParams{To: sv.Session.SessionID, Text: text, Steer: true, MessageID: msgID})
		if err == nil && m.Status.Runtime != c.DeliveryRejected {
			return m, nil
		}
	}
	return o.Send(ctx, SendParams{To: sv.Session.SessionID, Text: text, MessageID: msgID + "-q"})
}

func (a *App) nodeResume(ctx context.Context, ops *Ops, ref string, all, dry bool) (resumeResult, error) {
	node, err := ops.ResolveNode(ctx, ref)
	if err != nil {
		return resumeResult{}, err
	}
	tag := drainTag(node.NodeID)
	res := resumeResult{NodeID: node.NodeID, NodeName: node.Name, Tag: tag, DryRun: dry, All: all, Sessions: []nodeOpItem{}, Counts: map[string]int{}}
	page, err := ops.ListSessions(ctx, SessionFilter{IncludeArchived: true})
	if err != nil {
		return res, err
	}
	var tagged []c.SessionView
	for _, sv := range page.Items {
		if slices.Contains(sv.Session.Tags, tag) {
			tagged = append(tagged, sv)
		}
	}
	if !dry && len(tagged) > 0 && node.Connection.State != c.ConnConnected {
		return res, c.Errorf(c.ErrUnavailable, "node %s is %s; wait for it to reconnect, then rerun", node.Name, node.Connection.State)
	}
	note := restartNoteText(node.Name)
	untag := func(it *nodeOpItem) {
		ok, err := ops.setSessionTag(ctx, it.SessionID, tag, false)
		if err != nil {
			it.Error = strings.TrimPrefix(it.Error+"; untag: "+err.Error(), "; ")
			res.Failures++
		}
		it.Untagged = ok
	}
	sendNote := func(it *nodeOpItem, runID string) bool {
		it.MessageID = resumeNoteID(it.SessionID, runID)
		m, err := ops.Send(ctx, SendParams{To: it.SessionID, Text: note, MessageID: it.MessageID})
		if err != nil {
			it.Error = "note: " + err.Error()
			res.Failures++
			return false
		}
		it.Delivery = string(m.Status.Runtime)
		return true
	}
	items := make([]nodeOpItem, len(tagged))
	var pending []int // launched or already starting: wait for live
	launched := 0
	for i, sv := range tagged {
		it := newItem(sv)
		action, reason := classifyResume(sv, all)
		it.Action, it.Reason = action, reason
		if dry {
			items[i] = it
			continue
		}
		switch action {
		case resumeArchived, resumeSkip:
			untag(&it)
		case resumeNote:
			if sendNote(&it, it.RunID) {
				untag(&it)
			}
		case resumeWait:
			it.NewRunID = it.RunID
			pending = append(pending, i)
		case resumeLaunch:
			if launched > 0 {
				if err := sleepCtx(ctx, a.pace(1500*time.Millisecond)); err != nil {
					return res, err
				}
			}
			launched++
			r, err := ops.Resume(ctx, ResumeParams{Session: sv.Session.SessionID})
			if err != nil {
				it.Action, it.Error = "failed", "resume: "+err.Error()
				res.Failures++
				break
			}
			if r.Session.Run != nil {
				it.NewRunID, it.NewState = r.Session.Run.RunID, string(r.Session.Run.State)
			}
			pending = append(pending, i)
		}
		items[i] = it
	}
	// Wait (bounded) for the launches to come up, then note and untag.
	deadline := time.Now().Add(a.pace(90 * time.Second))
	for len(pending) > 0 {
		var next []int
		for _, i := range pending {
			it := &items[i]
			sv, err := ops.API.GetSession(ctx, c.GetRequest{ID: it.SessionID})
			if err != nil || sv.Run == nil {
				next = append(next, i)
				continue
			}
			if it.NewRunID == "" {
				if sv.Run.RunID == it.RunID {
					next = append(next, i) // the new run is not recorded yet
					continue
				}
				it.NewRunID = sv.Run.RunID
			}
			if sv.Run.RunID != it.NewRunID {
				next = append(next, i)
				continue
			}
			it.NewState = string(sv.Run.State)
			switch {
			case liveState(sv.Run.State):
				it.Action = "resumed"
				if sendNote(it, sv.Run.RunID) {
					untag(it)
				}
			case sv.Run.State.Terminal():
				it.Action, it.Error = "failed", "resumed run "+string(sv.Run.State)+": "+orDash(sv.Run.StateReason)
				res.Failures++
			default:
				next = append(next, i)
			}
		}
		pending = next
		if len(pending) == 0 {
			break
		}
		if time.Now().After(deadline) {
			for _, i := range pending {
				items[i].Action, items[i].Error = "failed", "not live after 90s (state "+orDash(items[i].NewState)+"); tag kept, rerun to retry"
				res.Failures++
			}
			break
		}
		a.progressf("resume: waiting for %d run(s) on %s to come up", len(pending), node.Name)
		if err := sleepCtx(ctx, a.pace(3*time.Second)); err != nil {
			return res, err
		}
	}
	for _, it := range items {
		res.Counts[it.Action]++
	}
	res.Sessions = items
	return res, nil
}

// ---- printing ----

func countsText(m map[string]int) string {
	keys := sortedKeys(m)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%d %s", m[k], orDash(k))
	}
	return strings.Join(parts, ", ")
}

func printOpItem(w io.Writer, it nodeOpItem) {
	fmt.Fprintf(w, "  %s  %-10s %-22s %s", it.SessionID, it.Action, orDash(it.State), orDash(it.Title))
	if it.Reason != "" {
		fmt.Fprintf(w, "  (%s)", it.Reason)
	}
	if it.NewRunID != "" {
		fmt.Fprintf(w, "  -> %s %s", it.NewRunID, it.NewState)
	}
	if it.Error != "" {
		fmt.Fprintf(w, "  ERROR: %s", it.Error)
	}
	fmt.Fprintln(w)
}

func printDrain(w io.Writer, r drainResult) {
	verb := "drained"
	if r.DryRun {
		verb = "would drain"
	}
	fmt.Fprintf(w, "%s %d session(s) on %s (%s)", verb, len(r.Sessions), r.NodeName, r.NodeID)
	if len(r.Counts) > 0 {
		fmt.Fprintf(w, ": %s", countsText(r.Counts))
	}
	fmt.Fprintln(w)
	for _, it := range r.Sessions {
		printOpItem(w, it)
	}
	for _, it := range r.Skipped {
		printOpItem(w, it)
	}
	if len(r.StopSent) > 0 {
		fmt.Fprintf(w, "deadline reached; STOP NOW sent to %d session(s) still working:\n", len(r.StopSent))
		for _, it := range r.StopSent {
			printOpItem(w, it)
		}
	} else if r.Waited {
		fmt.Fprintln(w, "no drained session is working")
	}
	if r.Failures > 0 {
		fmt.Fprintf(w, "%d failure(s); rerun to retry\n", r.Failures)
	}
	fmt.Fprintln(w, r.FollowUp)
}

func printNodeResume(w io.Writer, r resumeResult) {
	if len(r.Sessions) == 0 {
		fmt.Fprintf(w, "no session on %s is tagged %s\n", r.NodeName, r.Tag)
		return
	}
	prefix := ""
	if r.DryRun {
		prefix = "(dry run) "
	}
	fmt.Fprintf(w, "%s%d drained session(s) on %s: %s\n", prefix, len(r.Sessions), r.NodeName, countsText(r.Counts))
	for _, it := range r.Sessions {
		printOpItem(w, it)
	}
	if r.Failures > 0 {
		fmt.Fprintf(w, "%d failure(s); tags kept where a resume failed, rerun to retry\n", r.Failures)
	}
}
