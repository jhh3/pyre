package hive

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// Codex rollout compatibility for Hive 0.2.3's capture adapter.
//
// Hive's Codex adapter (verified upstream against codex 0.147) reads the
// request from `event_msg/user_message`, the outcome from
// `event_msg/agent_message`, and file grounding from `function_call
// exec_command {cmd}`. Rollouts written by codex 0.160 (the app-server
// threads Pyre drives) carry none of those: turns appear as
// `event_msg/item_completed` items (UserMessage, AgentMessage,
// CommandExecution) and tool calls as `custom_tool_call exec` scripts. Hive
// then judges every such session "trivial" and stores nothing (observed on
// server, `session_end noop reason=trivial_session`).
//
// NormalizeCodexRollout bridges that gap without changing Hive: it copies
// the rollout and, after each item_completed line, adds the legacy line Hive
// understands. Rollouts that already contain legacy lines are not copied.
// The copy is a staging file that exists only for the capture call.

// MaxNormalizeBytes bounds the rollout size the shim will copy.
const MaxNormalizeBytes = 64 << 20

// CodexRolloutNeedsShim reports whether a rollout lacks the legacy
// user_message/agent_message events Hive 0.2.3 reads.
func CodexRolloutNeedsShim(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	codex := false
	for sc.Scan() {
		ln := sc.Bytes()
		if bytes.Contains(ln, []byte(`"session_meta"`)) {
			codex = true
		}
		if bytes.Contains(ln, []byte(`"type":"user_message"`)) || bytes.Contains(ln, []byte(`"type":"agent_message"`)) {
			return false, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, err
	}
	return codex, nil
}

type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type completedItem struct {
	Type string `json:"type"`
	Item struct {
		Type    string `json:"type"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		ParsedCmd []struct {
			Cmd string `json:"cmd"`
		} `json:"parsed_cmd"`
	} `json:"item"`
}

type legacyMsg struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type legacyCall struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// NormalizeCodexRollout writes src plus the legacy lines to w and returns
// how many lines it added.
func NormalizeCodexRollout(src io.Reader, w io.Writer) (int, error) {
	sc := bufio.NewScanner(src)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	bw := bufio.NewWriter(w)
	added := 0
	emit := func(ts, typ string, payload any) error {
		b, err := json.Marshal(struct {
			Timestamp string `json:"timestamp"`
			Type      string `json:"type"`
			Payload   any    `json:"payload"`
		}{ts, typ, payload})
		if err != nil {
			return err
		}
		added++
		if _, err := bw.Write(append(b, '\n')); err != nil {
			return err
		}
		return nil
	}
	for sc.Scan() {
		ln := sc.Bytes()
		if _, err := bw.Write(append(append([]byte(nil), ln...), '\n')); err != nil {
			return added, err
		}
		if !bytes.Contains(ln, []byte(`"item_completed"`)) {
			continue
		}
		var rl rolloutLine
		if json.Unmarshal(ln, &rl) != nil || rl.Type != "event_msg" {
			continue
		}
		var it completedItem
		if json.Unmarshal(rl.Payload, &it) != nil || it.Type != "item_completed" {
			continue
		}
		var parts []string
		for _, c := range it.Item.Content {
			if strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
		text := strings.Join(parts, "\n")
		var err error
		switch it.Item.Type {
		case "UserMessage":
			if text != "" {
				err = emit(rl.Timestamp, "event_msg", legacyMsg{"user_message", text})
			}
		case "AgentMessage":
			if text != "" {
				err = emit(rl.Timestamp, "event_msg", legacyMsg{"agent_message", text})
			}
		case "CommandExecution":
			for _, pc := range it.Item.ParsedCmd {
				if strings.TrimSpace(pc.Cmd) == "" {
					continue
				}
				args, _ := json.Marshal(map[string]string{"cmd": pc.Cmd})
				if err = emit(rl.Timestamp, "response_item", legacyCall{"function_call", "exec_command", string(args)}); err != nil {
					break
				}
			}
		}
		if err != nil {
			return added, err
		}
	}
	if err := sc.Err(); err != nil {
		return added, fmt.Errorf("codex rollout: %w", err)
	}
	return added, bw.Flush()
}
