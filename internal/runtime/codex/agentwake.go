package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Local-agent wake (bgwake.go's queue and turn, for Codex's own
// collaboration subagents).
//
// A subagent the model spawned (collaboration.spawn_agent, followup_task)
// sends its report to the root as an inter-agent message with
// trigger_turn false: Codex queues it for the root's next turn and starts
// none. When it finishes, the app-server sends item/completed for a
// subAgentActivity item (kind "completed", id "subagent-completed-<its
// turn>", agentPath "/root/<name>") on the root thread, carrying the root
// turn that last addressed it, not an active one. A root that ended its turn
// waiting on its agents stayed idle until someone else started a turn
// (iss_…, codex 0.163.0: two completions at
// 05:45:51 and 05:51:40 PT, the root idle until its parent's message at
// 05:54:54, whose turn got both reports).
//
// Pyre starts that turn. A completion while the root is idle is queued with
// the background-command completions and shares their wake (settle window,
// retries, stop and archive rules). Any turn that starts delivers Codex's
// queued reports, so a turn/started drops the queued agent completions: mail
// delivered first, or a wake for a command, never leaves a second wake for an
// agent whose report it carried. A completion during a root turn is held
// until that turn ends: the model got the report when the rollout has the
// agent's FINAL_ANSWER message after it (finalsSeen); otherwise it is still
// queued in Codex and wakes the root like one after the turn. An interrupted
// turn's are dropped (the owner stopped the work). Not recovered across a
// pyred restart (bgrecover.go reads commands only).

// agentDone is one local agent that finished.
type agentDone struct {
	ItemID, AgentPath, AgentThreadID string
	At                               time.Time
	InTurn                           bool
	attempts                         int
}

// agentSeenSlack: a FINAL_ANSWER this long before Pyre received the
// completion still counts as its report (Codex may queue the report just
// before it emits the item).
const agentSeenSlack = 5 * time.Second

// noteAgentDone queues (idle root) or holds (active root) a finished local
// agent. Called with ts.mu held, for item/completed.
func (ts *threadState) noteAgentDone(it item, at time.Time) bool {
	if it.Type != "subAgentActivity" || it.Kind != "completed" || it.ID == "" {
		return false
	}
	if ts.closing || at.Before(ts.wakeMute) || ts.wakeSeen[it.ID] {
		return false
	}
	ts.markSeen(it.ID)
	d := agentDone{ItemID: it.ID, AgentPath: it.AgentPath, AgentThreadID: it.AgentThreadID, At: at}
	if ts.status.Type == "active" {
		d.InTurn = true
		ts.agentHeld = capAgents(append(ts.agentHeld, d))
		return false
	}
	ts.agentWakes = capAgents(append(ts.agentWakes, d))
	return true
}

// agentsDelivered: a root turn started, and Codex hands it the queued
// reports. Called with ts.mu held.
func (ts *threadState) agentsDelivered() {
	ts.agentWakes = nil
}

// settleAgentWakes runs when a root turn ends: the completions held while it
// ran become InTurn candidates (judged from the rollout at wake time),
// except an interrupted turn's. Called with ts.mu held.
func (ts *threadState) settleAgentWakes(status string) {
	held := ts.agentHeld
	ts.agentHeld = nil
	if status == "interrupted" {
		return
	}
	ts.agentWakes = capAgents(append(ts.agentWakes, held...))
}

func capAgents(ds []agentDone) []agentDone {
	if len(ds) > wakeQueueMax {
		ds = ds[len(ds)-wakeQueueMax:]
	}
	return ds
}

// unseenAgents drops the batch's InTurn completions whose report the model
// got (finalsSeen); one the rollout says nothing about is dropped too (the
// turn ran on after it, and the model most likely got it).
func (a *Adapter) unseenAgents(rr *runRef, batch []agentDone) []agentDone {
	inTurn := false
	for _, d := range batch {
		inTurn = inTurn || d.InTurn
	}
	if !inTurn {
		return batch
	}
	var seen map[string]bool
	if path := rolloutPath(rr.stateDir, rr.meta); path != "" {
		seen = finalsSeen(path, batch)
	}
	kept := batch[:0]
	for _, d := range batch {
		if d.InTurn {
			if s, ok := seen[d.ItemID]; !ok || s {
				continue
			}
		}
		kept = append(kept, d)
	}
	return kept
}

var markAgentMsg = []byte(`"agent_message"`)

// finalsSeen reports, by item id, whether the rollout at path has the
// agent's FINAL_ANSWER message to the root after the completion (less
// agentSeenSlack). A completion with no agent path, or a rollout that
// cannot be read, is absent; one whose final is not there is false.
func finalsSeen(path string, ds []agentDone) map[string]bool {
	out := map[string]bool{}
	var want []agentDone
	for _, d := range ds {
		if d.InTurn && d.AgentPath != "" {
			want = append(want, d)
		}
	}
	if len(want) == 0 {
		return out
	}
	f, r, _, err := openRolloutTail(path)
	if err != nil {
		return out
	}
	defer f.Close()
	for _, d := range want {
		out[d.ItemID] = false
	}
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, markAgentMsg) {
			var l struct {
				Timestamp time.Time `json:"timestamp"`
				Type      string    `json:"type"`
				Payload   struct {
					Type    string `json:"type"`
					Author  string `json:"author"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &l) == nil && l.Type == "response_item" && l.Payload.Type == "agent_message" {
				final := false
				for _, c := range l.Payload.Content {
					final = final || strings.HasPrefix(c.Text, "Message Type: FINAL_ANSWER")
				}
				for _, d := range want {
					if final && l.Payload.Author == d.AgentPath && !l.Timestamp.Before(d.At.Add(-agentSeenSlack)) {
						out[d.ItemID] = true
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return out
}

// agentNotice is the wake turn's input for finished local agents.
func agentNotice(batch []agentDone) string {
	names := make([]string, 0, len(batch))
	for i, d := range batch {
		if i == wakeMaxListed {
			names = append(names, fmt.Sprintf("and %d more", len(batch)-wakeMaxListed))
			break
		}
		names = append(names, agentName(d))
	}
	var b strings.Builder
	if len(batch) == 1 {
		fmt.Fprintf(&b, "[pyre] local agent %s finished; its report is queued for you and arrives in this turn.", names[0])
	} else {
		fmt.Fprintf(&b, "[pyre] %d local agents finished (%s); their reports are queued for you and arrive in this turn.", len(batch), strings.Join(names, ", "))
	}
	b.WriteString("\nPyre started this turn because Codex starts none when a local agent finishes. Act on the report if it matters to your task; otherwise reply in one line and end the turn.")
	return b.String()
}

func agentName(d agentDone) string {
	if d.AgentPath != "" {
		return oneLine(d.AgentPath, 120)
	}
	return "thread " + d.AgentThreadID
}
