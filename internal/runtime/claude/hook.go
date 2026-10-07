package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/tmux"
)

// HookEvents are the Claude Code hook events Pyre registers. Every name is
// in the "Valid events" list Claude Code 2.1.289 prints (and in the hooks
// reference); an unknown name makes Claude open a "Settings Warning" dialog
// at startup that blocks the session, so only verified names are used.
//
//   - PostToolUseFailure: an executed tool call failed. With PostToolUse it
//     shows an approved call actually ran (previously an allowed-then-failed
//     call stayed "submitted"). Verified on 2.1.289: a call denied by the
//     PermissionRequest hook or in the TUI fires neither event; those
//     denials are classified from the turn moving on / the hook exiting.
//   - PermissionDenied: auto mode's classifier denied a tool call.
//   - PostCompact: compaction finished (transcript cursors must survive it).
var HookEvents = []string{
	"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse",
	"PostToolUseFailure", "PermissionRequest", "PermissionDenied",
	"Notification", "Stop", "StopFailure", "SubagentStart", "SubagentStop",
	"SessionEnd", "PreCompact", "PostCompact",
}

// toolMatcherEvents take a tool-name matcher; "*" matches every tool.
var toolMatcherEvents = map[string]bool{
	"PreToolUse": true, "PostToolUse": true, "PostToolUseFailure": true,
	"PermissionRequest": true, "PermissionDenied": true,
}

// toolDoneEvents end a tool call: no later PermissionRequest belongs to it.
var toolDoneEvents = map[string]bool{"PostToolUse": true, "PostToolUseFailure": true, "PermissionDenied": true}

// StateDirEnv is exported into the claude process so hook commands can find
// the run state dir even when invoked without the positional argument.
const StateDirEnv = "PYRE_CLAUDE_STATE_DIR"

type brokerConfig struct {
	WaitSeconds int `json:"wait_seconds"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

type hookMatcher struct {
	Matcher string      `json:"matcher,omitempty"`
	Hooks   []hookEntry `json:"hooks"`
}

// writeHookSettings writes <stateDir>/pyre-hooks.json whose commands run
// hookCmd + [event, stateDir]. Hooks given via --settings are additive: the
// user's own hooks and statusline still run.
func writeHookSettings(stateDir string, hookCmd []string, approvalWait time.Duration, allow []string) (string, error) {
	return writeRunSettings(stateDir, hookCmd, approvalWait, allow, "")
}

// writeRunSettings writes the run's single --settings file: Pyre's runtime
// hooks and permissions, merged with the environment generation's managed
// Claude settings overlay (managedPath, "" = none). Claude Code takes one
// --settings file, so the managed settings and hooks travel in the run's
// own file; the owner's ~/.claude/settings.json is never written.
func writeRunSettings(stateDir string, hookCmd []string, approvalWait time.Duration, allow []string, managedPath string) (string, error) {
	if len(hookCmd) == 0 {
		return "", errors.New("claude: HookCommand is required")
	}
	hooks := map[string][]hookMatcher{}
	for _, ev := range HookEvents {
		cmd := tmux.ShellJoin(append(append([]string{}, hookCmd...), ev, stateDir))
		timeout := 15
		if ev == "PermissionRequest" {
			timeout = int(approvalWait/time.Second) + 15
		}
		m := hookMatcher{Hooks: []hookEntry{{Type: "command", Command: cmd, Timeout: timeout}}}
		if toolMatcherEvents[ev] {
			m.Matcher = "*"
		}
		hooks[ev] = []hookMatcher{m}
	}
	settings := map[string]any{"hooks": hooks}
	if len(allow) > 0 {
		settings["permissions"] = map[string]any{"allow": allow}
	}
	if managedPath != "" {
		merged, err := mergeManagedSettings(settings, managedPath)
		if err != nil {
			return "", err
		}
		settings = merged
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	p := filepath.Join(stateDir, fileHooks)
	return p, writeFileAtomic(p, b)
}

// mergeManagedSettings merges a managed settings overlay (a JSON object
// written by the configuration manager for one environment generation) into
// Pyre's runtime settings. The managed layers were already merged and
// deduplicated against the user's own files by the configuration package;
// this only combines two disjoint fragments:
//   - hooks: per event, the managed groups follow Pyre's runtime groups
//     (Pyre's evidence/approval hooks always stay registered);
//   - permissions.allow/deny/ask: union, Pyre's entries first; other
//     permissions keys come from the overlay;
//   - every other top-level key comes from the overlay.
//
// disableAllHooks=true is refused: it would switch off the hooks that record
// the run's state and acceptance evidence.
func mergeManagedSettings(runtime map[string]any, path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "managed Claude settings %s: %v", path, err)
	}
	var ov map[string]any
	if err := json.Unmarshal(raw, &ov); err != nil || ov == nil {
		return nil, contracts.Errorf(contracts.ErrInvalid, "managed Claude settings %s: not a JSON object", path)
	}
	// Work on a generic copy of the runtime settings.
	b, err := json.Marshal(runtime)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(ov))
	for k := range ov {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := ov[k]
		switch k {
		case "hooks":
			hm, ok := v.(map[string]any)
			if !ok {
				return nil, contracts.Errorf(contracts.ErrInvalid, "managed Claude settings %s: hooks is not an object", path)
			}
			cur, _ := out["hooks"].(map[string]any)
			if cur == nil {
				cur = map[string]any{}
			}
			for ev, groups := range hm {
				gl, ok := groups.([]any)
				if !ok {
					return nil, contracts.Errorf(contracts.ErrInvalid, "managed Claude settings %s: hooks.%s is not a list", path, ev)
				}
				existing, _ := cur[ev].([]any)
				cur[ev] = append(existing, gl...)
			}
			out["hooks"] = cur
		case "permissions":
			pm, ok := v.(map[string]any)
			if !ok {
				return nil, contracts.Errorf(contracts.ErrInvalid, "managed Claude settings %s: permissions is not an object", path)
			}
			cur, _ := out["permissions"].(map[string]any)
			if cur == nil {
				cur = map[string]any{}
			}
			for pk, pv := range pm {
				switch pk {
				case "allow", "deny", "ask":
					list, ok := pv.([]any)
					if !ok {
						return nil, contracts.Errorf(contracts.ErrInvalid, "managed Claude settings %s: permissions.%s is not a list", path, pk)
					}
					existing, _ := cur[pk].([]any)
					seen := map[string]bool{}
					for _, e := range existing {
						if s, ok := e.(string); ok {
							seen[s] = true
						}
					}
					for _, e := range list {
						if s, ok := e.(string); ok && seen[s] {
							continue
						} else if ok {
							seen[s] = true
						}
						existing = append(existing, e)
					}
					cur[pk] = existing
				default:
					cur[pk] = pv
				}
			}
			out["permissions"] = cur
		case "disableAllHooks":
			if on, _ := v.(bool); on {
				return nil, contracts.Errorf(contracts.ErrConflict, "managed Claude settings %s set disableAllHooks, which would disable Pyre's run hooks (state, approvals, delivery evidence); remove it from the profile", path)
			}
			out[k] = v
		default:
			out[k] = v
		}
	}
	return out, nil
}

// HookMain is the entry point for `pyred hook claude <event> <state-dir>`.
// args are the arguments after "hook claude". It always exits 0: a non-zero
// (especially 2) hook exit has blocking semantics in Claude Code.
func HookMain(args []string, stdin io.Reader, stdout io.Writer) int {
	event, stateDir := "", os.Getenv(StateDirEnv)
	if len(args) > 0 {
		event = args[0]
	}
	if len(args) > 1 {
		stateDir = args[1]
	}
	out, _ := RunHook(context.Background(), event, stdin, stateDir)
	if len(out) > 0 {
		_, _ = stdout.Write(out)
	}
	return 0
}

// RunHook handles one Claude Code hook invocation. It appends the (bounded)
// payload to <stateDir>/events.jsonl with flock+fsync and returns the hook's
// stdout. The payload never leaves the node. PermissionRequest runs the
// approval broker (when broker.json exists); Stop claims one queued Stop-hook
// injection (when the inject dir has one). exit is always 0.
func RunHook(ctx context.Context, eventName string, stdin io.Reader, stateDir string) (stdout []byte, exit int) {
	if stateDir == "" {
		return nil, 0
	}
	if st, err := os.Stat(stateDir); err != nil || !st.IsDir() {
		return nil, 0 // run cleaned up; never fail the harness
	}
	raw, _ := io.ReadAll(io.LimitReader(stdin, 8<<20))
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	ev := eventName
	if ev == "" {
		ev = str(p, "hook_event_name")
	}
	if ev == "" {
		ev = "Unknown"
	}
	rec := hookRecord{
		ID: newEventID(), At: time.Now().UTC(), Event: ev,
		SessionID: str(p, "session_id"), TranscriptPath: str(p, "transcript_path"),
		ToolUseID: str(p, "tool_use_id"), ToolName: str(p, "tool_name"),
		Payload: boundPayload(raw),
	}
	if ev == "UserPromptSubmit" {
		rec.Markers = findMarkers(str(p, "prompt"))
	}
	if ev == "PostToolUse" {
		// Doorbell evidence: message ids returned by the pyre inbox tools or
		// acked, from the untruncated payload (stored payloads are bounded).
		rec.Markers = pyreToolMessageIDs(rec.ToolName, p, raw)
	}
	events := filepath.Join(stateDir, fileEvents)
	switch ev {
	case "PermissionRequest":
		if cfg, ok := readBroker(stateDir); ok {
			return runBroker(ctx, stateDir, rec, p, cfg), 0
		}
	case "Stop":
		if out := claimInjection(stateDir, rec); out != nil {
			return out, 0
		}
	}
	_ = appendJSONL(events, rec)
	return nil, 0
}

func readBroker(stateDir string) (brokerConfig, bool) {
	b, err := os.ReadFile(filepath.Join(stateDir, fileBroker))
	if err != nil {
		return brokerConfig{}, false
	}
	var c brokerConfig
	if json.Unmarshal(b, &c) != nil {
		return brokerConfig{}, false
	}
	if c.WaitSeconds <= 0 {
		c.WaitSeconds = 120
	}
	return c, true
}

// pendingRequest is written by the broker hook for each PermissionRequest.
type pendingRequest struct {
	RequestID string    `json:"request_id"`
	SessionID string    `json:"session_id,omitempty"`
	ToolName  string    `json:"tool_name,omitempty"`
	ToolUseID string    `json:"tool_use_id,omitempty"` // correlated PreToolUse
	Summary   string    `json:"summary,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	HookPID   int       `json:"hook_pid"`
	EventID   string    `json:"event_id"`
}

// answerFile is written by Answer() and consumed (renamed) by the hook.
type answerFile struct {
	RequestID string    `json:"request_id"`
	RunID     string    `json:"run_id"`
	OptionID  string    `json:"option_id"`
	Behavior  string    `json:"behavior"` // allow | deny
	Message   string    `json:"message,omitempty"`
	At        time.Time `json:"at"`
}

func toolSummary(p map[string]any) string {
	ti, _ := p["tool_input"].(map[string]any)
	for _, k := range []string{"command", "file_path", "path", "url", "pattern", "description", "prompt"} {
		if s := str(ti, k); s != "" {
			return truncStr(s, 500)
		}
	}
	if ti != nil {
		b, _ := json.Marshal(ti)
		return truncStr(string(b), 500)
	}
	return ""
}

func canonicalToolInput(p map[string]any) string {
	b, _ := json.Marshal(p["tool_input"]) // map keys are sorted by encoding/json
	return string(b)
}

// correlateToolUse finds the latest PreToolUse with the same tool and input
// that has not completed: PermissionRequest carries no tool_use_id.
func correlateToolUse(stateDir, tool, input string) string {
	recs, _, _ := readEvents(stateDir, 0)
	done := map[string]bool{}
	for i := len(recs) - 1; i >= 0; i-- {
		r := recs[i]
		switch {
		case toolDoneEvents[r.Event]:
			done[r.ToolUseID] = true
		case r.Event == "PreToolUse":
			if r.ToolUseID == "" || done[r.ToolUseID] || r.ToolName != tool {
				continue
			}
			if canonicalToolInput(r.payload()) == input {
				return r.ToolUseID
			}
		}
	}
	return ""
}

func runBroker(ctx context.Context, stateDir string, rec hookRecord, p map[string]any, cfg brokerConfig) []byte {
	events := filepath.Join(stateDir, fileEvents)
	rid := "perm-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
	rec.RequestID = rid
	rec.HookPID = os.Getpid()
	tb, _ := json.Marshal(truncateValue(p["tool_input"], 0))
	rec.ToolUseID = correlateToolUse(stateDir, rec.ToolName, string(tb))
	wait := time.Duration(cfg.WaitSeconds) * time.Second
	pr := pendingRequest{
		RequestID: rid, SessionID: rec.SessionID, ToolName: rec.ToolName, ToolUseID: rec.ToolUseID,
		Summary: toolSummary(p), CreatedAt: rec.At, ExpiresAt: rec.At.Add(wait), HookPID: rec.HookPID, EventID: rec.ID,
	}
	_ = os.MkdirAll(filepath.Join(stateDir, dirPending), 0o700)
	_ = os.MkdirAll(filepath.Join(stateDir, dirAnswers), 0o700)
	pb, _ := json.Marshal(pr)
	_ = writeFileAtomic(filepath.Join(stateDir, dirPending, rid+".json"), pb)
	_ = appendJSONL(events, rec)

	sctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer stop()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(150 * time.Millisecond)
	defer tick.Stop()
	ansPath := filepath.Join(stateDir, dirAnswers, rid+".json")
	for {
		select {
		case <-sctx.Done():
			_ = appendJSONL(events, hookRecord{ID: newEventID(), At: time.Now().UTC(), Event: evApprovalWithdrawn, RequestID: rid})
			return nil
		case <-deadline.C:
			// No decision: Claude falls back to its own TUI dialog.
			_ = appendJSONL(events, hookRecord{ID: newEventID(), At: time.Now().UTC(), Event: evApprovalTimeout, RequestID: rid})
			return nil
		case <-tick.C:
			consumed := ansPath[:len(ansPath)-len(".json")] + ".consumed"
			if err := os.Rename(ansPath, consumed); err != nil {
				continue // not answered yet (rename is the single-consumer claim)
			}
			b, err := os.ReadFile(consumed)
			var a answerFile
			if err != nil || json.Unmarshal(b, &a) != nil || (a.Behavior != "allow" && a.Behavior != "deny") {
				continue
			}
			dec := map[string]any{"behavior": a.Behavior}
			if a.Behavior == "deny" {
				msg := a.Message
				if msg == "" {
					msg = "Denied through Pyre"
				}
				dec["message"] = msg
			}
			out, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "PermissionRequest", "decision": dec}})
			_ = appendJSONL(events, hookRecord{ID: newEventID(), At: time.Now().UTC(), Event: evApprovalAnswered, RequestID: rid, Behavior: a.Behavior})
			return out
		}
	}
}

type injection struct {
	MessageID string    `json:"message_id"`
	AttemptID string    `json:"attempt_id"`
	Text      string    `json:"text"`
	At        time.Time `json:"at"`
}

// claimInjection atomically claims the oldest queued Stop-hook injection and
// returns the block decision; nil when nothing is queued.
func claimInjection(stateDir string, rec hookRecord) []byte {
	dir := filepath.Join(stateDir, dirInject)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".msg") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		src := filepath.Join(dir, n)
		dst := strings.TrimSuffix(src, ".msg") + ".consumed"
		if os.Rename(src, dst) != nil {
			continue // claimed or withdrawn concurrently
		}
		b, err := os.ReadFile(dst)
		var inj injection
		if err != nil || json.Unmarshal(b, &inj) != nil {
			continue
		}
		rec.MessageID = inj.MessageID
		_ = appendJSONL(filepath.Join(stateDir, fileEvents), rec)
		_ = appendJSONL(filepath.Join(stateDir, fileEvents), hookRecord{
			ID: newEventID(), At: time.Now().UTC(), Event: evStopInjected, MessageID: inj.MessageID, Markers: findMarkers(inj.Text)})
		out, _ := json.Marshal(map[string]any{"decision": "block", "reason": inj.Text})
		return out
	}
	return nil
}

func injectionName(seq int64, messageID string) string {
	return fmt.Sprintf("%020d-%s", seq, messageID)
}
