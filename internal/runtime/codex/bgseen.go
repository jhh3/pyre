package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"time"
)

// Did the model get a command's exit? (bgwake.go)
//
// exec_command returns either the exit of a command that finished within
// its yield, or a running process's session id; the model then learns the
// exit only from a later write_stdin naming that session id. Codex reports
// none of this on the app-server: item/commandExecution/terminalInteraction
// is not sent for a poll that finds the process exited. The rollout has it:
// every tool call with its input, every tool output, and an item_completed
// event (with process_id) for each command, in order. Both tool shapes are
// read: function_call/function_call_output (exec_command, write_stdin;
// output "Process running with session ID N") and code mode's
// custom_tool_call/custom_tool_call_output (a script calling
// tools.exec_command / tools.write_stdin; output JSON with "session_id":N).

// rolloutScanBytes bounds how much of a rollout's end exitsSeen reads.
const rolloutScanBytes = 8 << 20

// execRef names one finished command: its item id and process (session) id.
type execRef struct{ ItemID, ProcessID string }

var (
	pidRe = regexp.MustCompile(`^[0-9]{1,10}$`)
	// exitRe: a tool output reporting an exit (code mode's "exit_code":N,
	// classic "Process exited with code N").
	exitRe = regexp.MustCompile(`(?i)exit_code\W{0,6}-?[0-9]|process exited with code`)
	// varPollRe: a code-mode write_stdin whose session_id is not a literal.
	varPollRe = regexp.MustCompile(`write_stdin\W{0,4}\{[^}]*session_id(\W{0,8}[A-Za-z_$]|\s*[,}])`)
)

// exitsSeen reports, by item id, whether the model got each command's
// exit, judged from the rollout at path. A command is absent from the result
// when its item_completed line is not in the scanned part (or the rollout
// cannot be read). It is seen when no tool output ever named its session
// id (exec_command returned the exit itself), or when a poll of it returned
// after the exit: a tool call whose input names the session id more often
// than its output does (each output naming it reports it still running),
// or a code-mode script that polled it by variable (write_stdin with a
// non-literal session_id) and whose output reports an exit after its last
// mention of the session. An output that names the session id is never
// itself the exit: it is the yield (iss_…: a
// script yielded the session, ran another command meanwhile, and its
// output, written after the exit, named only the yield). When the scanned
// part starts after the command did and holds no yield of it, it is absent
// too: its yield may be before the cut (iss_…:
// inline images made a 10.9 MB rollout, and a 240 s command whose yield sat
// before the last 8 MB was judged to have returned its exit).
func exitsSeen(path string, cmds []execRef) map[string]bool {
	type state struct {
		ref           execRef
		re            *regexp.Regexp
		done          bool
		started       time.Time // the command's start (item_completed started_at_ms), if known
		yielded, seen bool
	}
	var st []*state
	for _, c := range cmds {
		if c.ItemID == "" || !pidRe.MatchString(c.ProcessID) {
			continue
		}
		st = append(st, &state{ref: c, re: regexp.MustCompile(`(?i)session[ _]?id\W{0,8}` + c.ProcessID + `\b`)})
	}
	out := map[string]bool{}
	if len(st) == 0 {
		return out
	}
	f, r, cut, err := openRolloutTail(path)
	if err != nil {
		return out
	}
	defer f.Close()
	var from time.Time           // the scanned part's first line, when the rollout was cut
	calls := map[string]string{} // call_id -> input or arguments
	for first := true; ; first = false {
		line, err := r.ReadBytes('\n')
		if first && cut {
			var l struct {
				Timestamp time.Time `json:"timestamp"`
			}
			if json.Unmarshal(line, &l) == nil {
				from = l.Timestamp
			}
		}
		if len(line) > 0 {
			scanRolloutLine(line, calls, func(itemID, pid string, started time.Time) {
				for _, s := range st {
					if s.ref.ItemID == itemID || (itemID == "" && s.ref.ProcessID == pid) {
						s.done, s.started = true, started
					}
				}
			}, func(callID string, output []byte) {
				in := calls[callID]
				for _, s := range st {
					running := s.re.FindAllIndex(output, -1)
					if len(running) > 0 {
						s.yielded = true
					}
					if !s.done || s.seen {
						continue
					}
					if len(s.re.FindAllStringIndex(in, -1)) > len(running) ||
						len(running) > 0 && varPollRe.MatchString(in) && exitRe.Match(output[running[len(running)-1][1]:]) {
						s.seen = true
					}
				}
			})
		}
		if err != nil {
			break
		}
	}
	for _, s := range st {
		if !s.done || cut && !s.yielded && (from.IsZero() || s.started.IsZero() || s.started.Before(from)) {
			continue
		}
		out[s.ref.ItemID] = s.seen || !s.yielded
	}
	return out
}

// openRolloutTail opens the rollout at path positioned at the first whole
// line of its last rolloutScanBytes (cut reports that earlier lines were
// skipped).
func openRolloutTail(path string) (f *os.File, r *bufio.Reader, cut bool, err error) {
	if f, err = os.Open(path); err != nil {
		return nil, nil, false, err
	}
	r = bufio.NewReaderSize(f, 64<<10)
	if fi, serr := f.Stat(); serr == nil && fi.Size() > rolloutScanBytes {
		cut = true
		if _, err = f.Seek(fi.Size()-rolloutScanBytes, io.SeekStart); err == nil {
			r.Reset(f)
			_, err = r.ReadBytes('\n') // a partial line
		}
		if err != nil {
			f.Close()
			return nil, nil, false, err
		}
	}
	return f, r, cut, nil
}

// rolloutLine is the part of a rollout line exitsSeen reads.
type rolloutLine struct {
	Type    string `json:"type"`
	Payload struct {
		Type      string          `json:"type"`
		CallID    string          `json:"call_id"`
		Input     string          `json:"input"`         // custom_tool_call
		Arguments string          `json:"arguments"`     // function_call
		Output    json.RawMessage `json:"output"`        // a string or content parts
		StartedAt int64           `json:"started_at_ms"` // item_completed: the command's start
		Item      *struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ProcessID string `json:"process_id"`
		} `json:"item"`
	} `json:"payload"`
}

var (
	markCall = []byte(`_call`)
	markDone = []byte(`"item_completed"`)
)

// scanRolloutLine records a tool call's input, and reports a command's
// completion (done) and a tool output (output: the raw JSON, which keeps a
// code-mode output's escaped JSON text intact for matching).
func scanRolloutLine(line []byte, calls map[string]string, done func(itemID, pid string, started time.Time), output func(callID string, raw []byte)) {
	if !bytes.Contains(line, markCall) && !bytes.Contains(line, markDone) {
		return
	}
	var l rolloutLine
	if json.Unmarshal(line, &l) != nil {
		return
	}
	p := l.Payload
	switch {
	case l.Type == "event_msg" && p.Type == "item_completed" && p.Item != nil && p.Item.Type == "CommandExecution":
		var started time.Time
		if p.StartedAt > 0 {
			started = time.UnixMilli(p.StartedAt)
		}
		done(p.Item.ID, p.Item.ProcessID, started)
	case l.Type == "response_item" && (p.Type == "function_call" || p.Type == "custom_tool_call") && p.CallID != "":
		calls[p.CallID] = p.Input + p.Arguments
	case l.Type == "response_item" && (p.Type == "function_call_output" || p.Type == "custom_tool_call_output"):
		output(p.CallID, p.Output)
	}
}
