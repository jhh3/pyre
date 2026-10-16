//go:build !windows

package claude

// S-D doorbell measurement on the real Claude Code (PYRE_REAL_HARNESS=1).
// Uses the default login, an isolated tmux server, an already-trusted
// scratch cwd (PYRE_REAL_CWD), sonnet/low, and synthetic messages. The
// Pyre API is the fake API above (real httpapi handler); the agent's pyre
// tools are the real `pyre mcp`; the monitor is the real watcher.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

const doorbellGuide = `You are part of an automated Pyre test. Pyre may notify you with a line like "Pyre: N new messages for this session (...)". When that happens, call the pyre "inbox" tool with unacked=true, then call "ack_message" once for each message id. Do not reply to the messages and do not run other tools for them. All messages are synthetic.`

func TestRealClaudeDoorbell(t *testing.T) {
	if os.Getenv("PYRE_REAL_HARNESS") != "1" {
		t.Skip("set PYRE_REAL_HARNESS=1")
	}
	cwd := os.Getenv("PYRE_REAL_CWD")
	if cwd == "" {
		t.Fatal("PYRE_REAL_CWD required")
	}
	exe, _ := os.Executable()
	dir, _ := os.MkdirTemp("", "pb")
	defer os.RemoveAll(dir)
	ctx := context.Background()
	api := newFakeAPI("ses_door")
	sock := filepath.Join(dir, "api.sock")
	token := "synthetic-session-token"
	if err := api.serve(sock, token); err != nil {
		t.Fatal(err)
	}
	defer api.down()
	mcp := filepath.Join(dir, "mcp.json")
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"pyre": map[string]any{"type": "stdio", "command": "/usr/bin/env", "args": []string{"PYRE_TEST_ROLE=pyremcp", exe}}}})
	_ = os.WriteFile(mcp, b, 0o600)

	o := realOptions(t, dir)
	o.DoorbellCommand = []string{"/usr/bin/env", "PYRE_TEST_ROLE=inboxwatch", exe}
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	defer a.srv.KillServer(ctx)
	state := filepath.Join(dir, "run")
	h, err := a.Launch(ctx, contracts.ResolvedLaunch{RunID: "run_door", SessionID: "ses_door", Cwd: cwd, StateDir: state,
		Args: append(append([]string{}, realArgs...), "--mcp-config="+mcp), AgentGuide: doorbellGuide,
		Env:   map[string]string{"PYRE_SOCKET": sock, "PYRE_SESSION_TOKEN": token, "PYRE_SESSION_ID": "ses_door", "PYRE_TEST_DOORBELL_INTERVAL_MS": envOr("PYRE_DOORBELL_INTERVAL_MS", "5000")},
		Unset: []string{"PYRE_HUB", "PYRE_TOKEN", "PYRE_HUB_PIN"}})
	if err != nil {
		t.Fatal(err)
	}
	var results []string
	res := func(format string, args ...any) {
		s := fmt.Sprintf(format, args...)
		results = append(results, s)
		t.Log(s)
	}
	defer func() {
		t.Log("==== DOORBELL RESULTS ====")
		for _, r := range results {
			t.Log(r)
		}
	}()
	obs := func() contracts.Observation { o, _ := a.Observe(ctx, h, ""); return o }
	waitState := func(want contracts.RunState, d time.Duration) bool {
		end := time.Now().Add(d)
		for time.Now().Before(end) {
			if obs().State == want {
				return true
			}
			time.Sleep(300 * time.Millisecond)
		}
		return false
	}
	readAt := func(id string) (time.Time, bool) {
		for _, r := range obs().Receipts {
			if r.MessageID == id && r.Evidence == contracts.EvidenceClaudeInboxRead {
				return r.ObservedAt, true
			}
		}
		return time.Time{}, false
	}
	waitAck := func(id string, d time.Duration) (time.Duration, bool) {
		start := time.Now()
		for time.Since(start) < d {
			if t, ok := api.ackedAt(id); ok {
				return t.Sub(start), true
			}
			time.Sleep(250 * time.Millisecond)
		}
		return 0, false
	}
	notifCount := func(id string) int {
		b, _ := os.ReadFile(filepath.Join(state, "doorbell-lines.log"))
		return strings.Count(string(b), id+" ") + strings.Count(string(b), id+")") + strings.Count(string(b), id+",")
	}
	lineCount := func() int {
		b, _ := os.ReadFile(filepath.Join(state, "doorbell-lines.log"))
		return strings.Count(string(b), "\n")
	}
	inboxCalls := func() int {
		recs, _, _ := readEvents(state, 0)
		n := 0
		for _, r := range recs {
			if r.Event == "PostToolUse" && r.ToolName == toolPyreInbox {
				n++
			}
		}
		return n
	}
	promptHooks := func() int {
		recs, _, _ := readEvents(state, 0)
		n := 0
		for _, r := range recs {
			if r.Event == "UserPromptSubmit" {
				n++
			}
		}
		return n
	}
	screen := func() string {
		s, _ := a.srv.CapturePane(ctx, h.TmuxTarget, tmux.CaptureOptions{Join: true})
		return s
	}
	scen := os.Getenv("PYRE_DOORBELL_SCENARIOS")
	if scen == "" {
		scen = "ABCDEFG"
	}
	want := func(x string) bool { return strings.Contains(scen, x) }
	eventTimes := func(event, tool string, after time.Time) []time.Time {
		recs, _, _ := readEvents(state, 0)
		var out []time.Time
		for _, r := range recs {
			if r.Event == event && (tool == "" || r.ToolName == tool) && r.At.After(after) {
				out = append(out, r.At)
			}
		}
		return out
	}
	msg := func(n int) string {
		return fmt.Sprintf("Synthetic Pyre test message %d. No action needed beyond acknowledging it.", n)
	}

	if !waitState(contracts.RunWaitingInput, 60*time.Second) {
		t.Fatalf("not idle: %+v\n%s", obs(), screen())
	}
	time.Sleep(2 * time.Second)
	res("monitor running: %v (status line shows monitor: %v)", fileHas(filepath.Join(state, "doorbell-watch.log"), "start pid="), strings.Contains(screen(), "monitor"))

	// A. idle wake.
	var d time.Duration
	var ok, read bool
	var r contracts.DeliveryResult
	_, _, _, _ = d, ok, read, r
	if want("A") {
		api.post("msg_dbA", "ses_peer", msg(1))
		d, ok = waitAck("msg_dbA", 120*time.Second)
		_, read = readAt("msg_dbA")
		res("A idle: acked=%v after %.1fs; inbox_read receipt=%v; notification lines for id=%d", ok, d.Seconds(), read, notifCount("msg_dbA"))
		waitState(contracts.RunWaitingInput, 60*time.Second)
	}

	// B. human attached (write client).
	if want("B") {
		dd, _ := a.Attach(ctx, h, contracts.AttachCommand{RunID: h.RunID})
		outer, _ := tmux.NewServer("", filepath.Join(dir, "outer.sock"))
		_ = outer.NewSession(ctx, tmux.SessionSpec{Name: "human", Argv: dd.Command, Width: 160, Height: 45})
		time.Sleep(time.Second)
		cs, _ := a.srv.ListClients(ctx, h.TmuxTarget)
		r, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_dbB", AttemptID: "dlv_B", Text: msg(2)})
		res("B attached: writable clients=%d; Deliver=%s (%s)", tmux.WritableClients(cs), r.State, r.Detail)
		api.post("msg_dbB", "ses_peer", msg(2))
		d, ok = waitAck("msg_dbB", 120*time.Second)
		_, read = readAt("msg_dbB")
		r2, _ := a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_dbB", AttemptID: "dlv_B2", Text: msg(2)})
		res("B attached: acked=%v after %.1fs; inbox_read receipt=%v; later Deliver=%s (%s)", ok, d.Seconds(), read, r2.State, r2.Detail)
		_ = outer.KillServer(ctx)
		waitState(contracts.RunWaitingInput, 60*time.Second)
	}

	// C. busy: a long turn via paste, then mail.
	if want("C") {
		r, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_long", AttemptID: "dlv_long", Text: "Write the integers from 1 to 600 separated by spaces, no other text."})
		busy := waitState(contracts.RunRunning, 10*time.Second)
		postC := time.Now()
		api.post("msg_dbC", "ses_peer", msg(3))
		var turnEnd time.Time
		for i := 0; i < 600; i++ {
			if obs().State == contracts.RunWaitingInput && turnEnd.IsZero() {
				turnEnd = time.Now()
			}
			if _, ok := api.ackedAt("msg_dbC"); ok {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		ackC, okC := api.ackedAt("msg_dbC")
		order := "after the long turn ended"
		if okC && (turnEnd.IsZero() || ackC.Before(turnEnd)) {
			order = "BEFORE the long turn ended (mid-turn)"
		}
		_ = order
		_ = r
		// Precise order from hooks: the long turn's Stop vs the inbox read.
		stops := eventTimes("Stop", "", postC.Add(-30*time.Second))
		reads := eventTimes("PostToolUse", toolPyreInbox, postC)
		prompts := eventTimes("UserPromptSubmit", "", postC)
		var firstStop, firstRead, firstPrompt time.Time
		if len(stops) > 0 {
			firstStop = stops[0]
		}
		if len(reads) > 0 {
			firstRead = reads[0]
		}
		if len(prompts) > 0 {
			firstPrompt = prompts[0]
		}
		res("C busy: running when posted=%v; acked=%v +%.1fs; long-turn Stop at +%.1fs; notification turn UserPromptSubmit at +%.1fs; inbox read at +%.1fs => read %s the long turn's Stop",
			busy, okC, ackC.Sub(postC).Seconds(), firstStop.Sub(postC).Seconds(), firstPrompt.Sub(postC).Seconds(), firstRead.Sub(postC).Seconds(),
			map[bool]string{true: "AFTER", false: "BEFORE"}[!firstStop.IsZero() && firstRead.After(firstStop)])
		waitState(contracts.RunWaitingInput, 60*time.Second)
	}

	// D. approval dialog open.
	if want("D") {
		target := filepath.Join(cwd, "pyre-wp07-doorbell.txt")
		_ = os.Remove(target)
		_, _ = a.Deliver(ctx, h, contracts.DeliveryInput{MessageID: "msg_appr", AttemptID: "dlv_appr", Text: "Use the Bash tool to run exactly: touch pyre-wp07-doorbell.txt  Then reply with the single word done."})
		var ap contracts.Approval
		for i := 0; i < 240 && ap.RequestID == ""; i++ {
			for _, x := range obs().Approvals {
				if x.State == contracts.ApprovalPending {
					ap = x
				}
			}
			time.Sleep(250 * time.Millisecond)
		}
		api.post("msg_dbD", "ses_peer", msg(4))
		time.Sleep(25 * time.Second)
		_, ackedDuring := api.ackedAt("msg_dbD")
		still := false
		for _, x := range obs().Approvals {
			if x.RequestID == ap.RequestID && x.State == contracts.ApprovalPending {
				still = true
			}
		}
		res("D dialog: approval pending=%v; doorbell lines for id=%d; acked while dialog open=%v; approval still pending after 25s=%v; screen shows dialog=%v",
			ap.RequestID != "", notifCount("msg_dbD"), ackedDuring, still, strings.Contains(screen(), "Do you want to proceed"))
		if ap.RequestID != "" && still {
			_, _ = a.Answer(ctx, h, ap, "allow", "")
		}
		d, ok = waitAck("msg_dbD", 180*time.Second)
		res("D dialog: after answering, msg acked=%v (+%.1fs)", ok, d.Seconds())
		_ = os.Remove(target)
		waitState(contracts.RunWaitingInput, 60*time.Second)
	}

	// E. pyred restart (API down, mail arrives meanwhile, API back).
	if want("E") {
		linesBefore := lineCount()
		api.down()
		time.Sleep(3 * time.Second)
		api.post("msg_dbE", "ses_peer", msg(5))
		time.Sleep(5 * time.Second)
		if err := api.serve(sock, token); err != nil {
			t.Fatal(err)
		}
		d, ok = waitAck("msg_dbE", 180*time.Second)
		dups := 0
		for _, id := range []string{"msg_dbA", "msg_dbB", "msg_dbC", "msg_dbD", "msg_dbE"} {
			if c := notifCount(id); c > 1 {
				dups += c - 1
			}
		}
		res("E restart: msg acked=%v (+%.1fs after API back); new lines=%d; duplicate notifications so far=%d", ok, d.Seconds(), lineCount()-linesBefore, dups)
		waitState(contracts.RunWaitingInput, 60*time.Second)
	}

	// F. storm: 15 messages at once.
	if want("F") {
		linesBefore, callsBefore, promptsBefore := lineCount(), inboxCalls(), promptHooks()
		stormStart := time.Now()
		for i := 0; i < 15; i++ {
			api.post(fmt.Sprintf("msg_dbF%02d", i), "ses_storm", msg(100+i))
		}
		allAcked := false
		for time.Since(stormStart) < 300*time.Second && !allAcked {
			allAcked = true
			for i := 0; i < 15; i++ {
				if _, ok := api.ackedAt(fmt.Sprintf("msg_dbF%02d", i)); !ok {
					allAcked = false
				}
			}
			time.Sleep(time.Second)
		}
		res("F storm: 15 msgs; all acked=%v in %.1fs; notification lines=%d; inbox calls=%d; UserPromptSubmit hooks=%d; monitor alive=%v",
			allAcked, time.Since(stormStart).Seconds(), lineCount()-linesBefore, inboxCalls()-callsBefore, promptHooks()-promptsBefore, watcherAlive(state))
		waitState(contracts.RunWaitingInput, 60*time.Second)
	}

	// H. raw burst: coalescing effectively off (1ms), 20 messages 150ms
	// apart -> up to 20 monitor lines; how does Claude handle a noisy monitor?
	if want("H") {
		linesBefore, callsBefore, promptsBefore := lineCount(), inboxCalls(), promptHooks()
		burstStart := time.Now()
		for i := 0; i < 20; i++ {
			api.post(fmt.Sprintf("msg_dbH%02d", i), "ses_burst", msg(200+i))
			time.Sleep(150 * time.Millisecond)
		}
		allAcked := false
		for time.Since(burstStart) < 300*time.Second && !allAcked {
			allAcked = true
			for i := 0; i < 20; i++ {
				if _, ok := api.ackedAt(fmt.Sprintf("msg_dbH%02d", i)); !ok {
					allAcked = false
				}
			}
			time.Sleep(time.Second)
		}
		time.Sleep(5 * time.Second)
		res("H raw burst: 20 msgs 150ms apart, coalescing 1ms; all acked=%v in %.1fs; monitor lines=%d; inbox calls=%d; UserPromptSubmit hooks=%d; monitor alive after=%v",
			allAcked, time.Since(burstStart).Seconds(), lineCount()-linesBefore, inboxCalls()-callsBefore, promptHooks()-promptsBefore, watcherAlive(state))
		waitState(contracts.RunWaitingInput, 60*time.Second)
	}

	// G. monitor process killed: does Claude restart it?
	if want("G") {
		pid := watcherPID(state)
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Second)
		restarted := watcherPID(state) != pid && watcherAlive(state)
		api.post("msg_dbG", "ses_peer", msg(7))
		d, ok = waitAck("msg_dbG", 60*time.Second)
		res("G monitor killed (pid %d): restarted by Claude=%v; msg acked=%v (+%.1fs); DoorbellAlive=%v", pid, restarted, ok, d.Seconds(), DoorbellAlive(state))
	}

	o2 := obs()
	res("totals: doorbell notifications seen in transcript=%d; inbox calls=%d; UserPromptSubmit hooks=%d; lines printed=%d; fake API ListMessages calls=%d",
		doorbellsSeen(a, h), inboxCalls(), promptHooks(), lineCount(), api.lists)
	_ = o2
	// Distinct task-notification texts (synthetic content only).
	{
		e := a.entry(h)
		e.mu.Lock()
		a.refresh(h, e)
		path := e.rs.tPath
		e.mu.Unlock()
		seen := map[string]bool{}
		_, _ = readJSONLFrom(path, 0, 0, func(line []byte, _ int64) {
			var tr transcriptRecord
			if json.Unmarshal(line, &tr) != nil || !tr.isPromptRecord() {
				return
			}
			txt := tr.text()
			if !strings.Contains(txt, "<task-notification>") {
				return
			}
			txt = oneLine(markerRe.ReplaceAllString(txt, ""), 400)
			for _, id := range []string{"msg_db"} {
				_ = id
			}
			key := txt
			if len(key) > 120 {
				key = key[:120]
			}
			if !seen[key] {
				seen[key] = true
				res("task-notification: %s", txt)
			}
		})
	}
	rc, _ := a.Stop(ctx, h)
	res("stop: %s %s; monitor alive after stop=%v", rc.Outcome, rc.Detail, watcherAlive(state))
}

func fileHas(p, s string) bool {
	b, _ := os.ReadFile(p)
	return strings.Contains(string(b), s)
}

// watcherPID returns the latest watcher pid from doorbell-watch.log.
func watcherPID(state string) int {
	b, _ := os.ReadFile(filepath.Join(state, "doorbell-watch.log"))
	pid := 0
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.Index(l, "start pid="); i >= 0 {
			f := strings.Fields(l[i+len("start pid="):])
			if len(f) > 0 {
				pid, _ = strconv.Atoi(f[0])
			}
		}
	}
	return pid
}

func watcherAlive(state string) bool {
	pid := watcherPID(state)
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func doorbellsSeen(a *Adapter, h contracts.RunHandle) int {
	e := a.entry(h)
	e.mu.Lock()
	defer e.mu.Unlock()
	a.refresh(h, e)
	return e.rs.doorbells
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
