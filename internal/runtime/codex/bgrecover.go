package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Background-command wakes across a pyred restart (bgwake.go).
//
// The wake queue and the in-turn completions held for judgement live in
// pyred's memory, and the app-server sends a re-subscribing client nothing
// that happened before it. So a recovered run lost what the previous pyred
// held (iss_…: an exec_command exited unseen 4.5
// minutes into a long turn, pyred restarted for an upgrade 1.5 minutes
// later, the turn ended 40 s after that and no wake came), and exits while
// pyred was down were never seen at all.
//
// The rollout has every completion (item_completed with the turn that
// started it, also for an exit after that turn ended) and every turn
// boundary. A recovered run backfills from it once, at the first point the
// previous pyred would have acted:
//   - recovered while a turn runs: when that turn ends, the completions
//     since it started that this pyred did not receive. Its own exec_command
//     exits are in-turn candidates (judged by unseenExits; none when it was
//     interrupted), the others are exits after their turn ended. The
//     previous pyred woke none of these: it would have waited for this turn
//     to end.
//   - recovered idle: the completions since the last turn started, classed
//     the same way against that turn. Any wake the previous pyred started
//     for them is a turn after them, so they are not in that span.
//
// A turn that ended while pyred was down is the last turn when recovered
// idle, so its in-turn exits are judged too (the judgement is the rollout's,
// so one the previous pyred judged comes out the same).
//
// Not backfilled: completions before the run launched; when recovered
// idle, ones that completed (in-turn ones: whose turn ended) more than
// backfillIdleWindow before, which the previous pyred handled; ones Pyre's
// own clean killed (bgCleanedFile, plus wakeMuteAfterClean). A completion
// whose wake Codex refused three times before the restart is reported once
// more.

// bgCleanedFile (in the run's state dir) holds when Pyre last cleaned the
// thread's background terminals, so a recovered run does not wake for the
// exits of commands Pyre itself killed before the restart.
const bgCleanedFile = "bg-cleaned-at"

// backfillIdleWindow bounds how far back a run recovered idle reads
// completions (or, for in-turn ones, turn ends): older ones the previous
// pyred handled. A pyred restart takes seconds; an outage longer than this
// misses older exits.
const backfillIdleWindow = 15 * time.Minute

// backfill is a recovered run's pending rollout backfill.
type backfill struct {
	fired bool
	live  map[string]bool // commandExecution completions this pyred received
}

// armBackfill marks a recovered run for one rollout backfill. Called with
// ts.mu held.
func (ts *threadState) armBackfill() {
	if ts.backfill == nil {
		ts.backfill = &backfill{live: map[string]bool{}}
	}
}

// noteLiveDone records a completion this pyred received (never
// backfilled). Called with ts.mu held.
func (ts *threadState) noteLiveDone(id string) {
	if bf := ts.backfill; bf != nil && !bf.fired && len(bf.live) < wakeSeenMax {
		bf.live[id] = true
	}
}

// takeBackfill fires the backfill once. Called with ts.mu held.
func (ts *threadState) takeBackfill() *backfill {
	bf := ts.backfill
	if bf == nil || bf.fired {
		return nil
	}
	bf.fired = true
	return bf
}

// backfillExits queues the completions a recovered run missed (see above):
// turnID is the turn that just ended (status its turn/completed status),
// or "" when the run was recovered idle.
func (a *Adapter) backfillExits(ts *threadState, bf *backfill, turnID, status string) {
	a.mu.Lock()
	rr := a.runs[ts.runID]
	a.mu.Unlock()
	if rr == nil || rr.ts != ts || stopped(rr.stateDir) {
		return
	}
	path := rolloutPath(rr.stateDir, rr.meta)
	if path == "" {
		return
	}
	missed := missedExits(path, turnID, rr.meta.LaunchedAt)
	if len(missed) == 0 {
		return
	}
	cleaned := readCleanedAt(rr.stateDir)
	now := a.o.Now()
	// Recovered idle, what the previous pyred may have dropped is recent:
	// what it held at the restart (the settle window, a wait on an
	// approval or a limit) and what ended while it was down.
	var notBefore time.Time
	if turnID == "" {
		notBefore = now.Add(-backfillIdleWindow)
	}
	var cands []bgDone
	for _, m := range missed {
		w := m.done
		if !cleaned.IsZero() && m.at.Before(cleaned.Add(wakeMuteAfterClean)) {
			continue
		}
		if w.InTurn && status == "interrupted" {
			continue
		}
		if ref := m.at; !notBefore.IsZero() {
			if w.InTurn && !m.turnEnd.IsZero() {
				ref = m.turnEnd // judged when its turn ended
			}
			if ref.Before(notBefore) {
				continue
			}
		}
		cands = append(cands, w)
	}
	// Judge the in-turn ones now (their turn has ended): a long turn has
	// dozens whose exit exec_command returned, which would crowd the queue
	// (iss_…).
	cands = a.unseenExits(rr, cands)
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.closing {
		return
	}
	queued := map[string]bool{}
	for _, w := range ts.wakes {
		queued[w.ItemID] = true
	}
	for _, w := range cands {
		if bf.live[w.ItemID] || ts.wakeSeen[w.ItemID] || ts.wakeIgnore[w.ItemID] || queued[w.ItemID] {
			continue
		}
		ts.markSeen(w.ItemID)
		w.At = now
		ts.wakes = append(ts.wakes, w)
	}
	ts.trimWakes()
	a.armWake(ts)
}

// missedExit is one completion read back from the rollout.
type missedExit struct {
	done    bgDone
	at      time.Time // when it completed
	turnEnd time.Time // InTurn: when its turn ended, if known
}

// rolloutEvent is the part of a rollout event line missedExits reads.
type rolloutEvent struct {
	Timestamp time.Time `json:"timestamp"`
	Type      string    `json:"type"`
	Payload   struct {
		Type          string `json:"type"`
		TurnID        string `json:"turn_id"`
		CompletedAtMs int64  `json:"completed_at_ms"`
		Item          *struct {
			Type             string   `json:"type"`
			ID               string   `json:"id"`
			ProcessID        string   `json:"process_id"`
			Command          []string `json:"command"`
			Cwd              string   `json:"cwd"`
			Source           string   `json:"source"`
			Status           string   `json:"status"`
			AggregatedOutput string   `json:"aggregated_output"`
			ExitCode         *int     `json:"exit_code"`
			Duration         *struct {
				Secs  int64 `json:"secs"`
				Nanos int64 `json:"nanos"`
			} `json:"duration"`
		} `json:"item"`
	} `json:"payload"`
}

var (
	markStarted  = []byte(`"task_started"`)
	markComplete = []byte(`"task_complete"`)
	markAborted  = []byte(`"turn_aborted"`)
)

// missedExits reads the commandExecution completions since turn turnID
// started ("" = the last turn the rollout names) from the rollout at path,
// in order. A completion of that turn before it ended is InTurn (only
// exec_command ones, with a process id; none when the turn was aborted);
// any other is an exit after its own turn. Completions before since are
// left out. When the turn's start is not in the scanned part (a long turn
// in a large rollout), everything scanned counts as since its start.
func missedExits(path, turnID string, since time.Time) []missedExit {
	f, r, cut, err := openRolloutTail(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	type ev struct {
		kind string // started, ended, aborted, done
		turn string
		e    rolloutEvent
	}
	var evs []ev
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && (bytes.Contains(line, markDone) || bytes.Contains(line, markStarted) ||
			bytes.Contains(line, markComplete) || bytes.Contains(line, markAborted)) {
			var e rolloutEvent
			if json.Unmarshal(line, &e) == nil && e.Type == "event_msg" {
				kind := ""
				switch e.Payload.Type {
				case "item_completed":
					if e.Payload.Item != nil && e.Payload.Item.Type == "CommandExecution" {
						kind = "done"
					}
				case "task_started":
					kind = "started"
				case "task_complete":
					kind = "ended"
				case "turn_aborted":
					kind = "aborted"
				}
				if kind != "" {
					evs = append(evs, ev{kind: kind, turn: e.Payload.TurnID, e: e})
				}
			}
		}
		if err != nil {
			break
		}
	}
	if turnID == "" {
		for i := len(evs) - 1; i >= 0 && turnID == ""; i-- {
			if evs[i].kind != "done" {
				turnID = evs[i].turn
			}
		}
		if turnID == "" {
			return nil
		}
	}
	start := -1
	for i, e := range evs {
		if e.kind == "started" && e.turn == turnID {
			start = i
		}
	}
	if start < 0 && !cut {
		return nil // the turn is not in this rollout
	}
	var out []missedExit
	aborted, endAt := false, time.Time{} // interrupted: none of its own exits
	for _, e := range evs[start+1:] {
		if (e.kind == "ended" || e.kind == "aborted") && e.turn == turnID {
			aborted, endAt = e.kind == "aborted", e.e.Timestamp
		}
	}
	ended := false
	for _, e := range evs[start+1:] {
		switch e.kind {
		case "ended", "aborted":
			ended = ended || e.turn == turnID
			continue
		case "started":
			continue
		}
		it, p := e.e.Payload.Item, e.e.Payload
		if it.ID == "" || it.Status == "declined" || it.Status == "in_progress" || it.Status == "inProgress" {
			continue
		}
		at := e.e.Timestamp
		if p.CompletedAtMs > 0 {
			at = time.UnixMilli(p.CompletedAtMs)
		}
		if at.Before(since) {
			continue
		}
		inTurn := e.turn == turnID && !ended
		exec := it.Source == "unified_exec_startup" || it.Source == execStartup
		if inTurn && (aborted || !exec || !pidRe.MatchString(it.ProcessID)) {
			continue
		}
		w := bgDone{ItemID: it.ID, TurnID: e.turn, Command: rolloutCommand(it.Command), Cwd: strings.TrimPrefix(it.Cwd, "file://"),
			Status: it.Status, ExitCode: it.ExitCode, Output: lastBytes(it.AggregatedOutput, 4*wakeTailBytes), InTurn: inTurn}
		if exec {
			w.ProcessID = it.ProcessID
		}
		if d := it.Duration; d != nil {
			ms := d.Secs*1000 + d.Nanos/1e6
			w.DurationMs = &ms
		}
		m := missedExit{done: w, at: at}
		if inTurn {
			m.turnEnd = endAt
		}
		out = append(out, m)
	}
	return out
}

// rolloutCommand is a rollout command's text without the `bash -lc`
// wrapper.
func rolloutCommand(argv []string) string {
	if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
		return argv[2]
	}
	return strings.Join(argv, " ")
}

func writeCleanedAt(stateDir string, at time.Time) {
	_ = writeFileAtomic(filepath.Join(stateDir, bgCleanedFile), []byte(at.UTC().Format(time.RFC3339Nano)))
}

func readCleanedAt(stateDir string) time.Time {
	b, err := os.ReadFile(filepath.Join(stateDir, bgCleanedFile))
	if err != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	return t
}
