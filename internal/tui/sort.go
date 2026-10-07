package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jhh3/pyre/internal/contracts"
)

// Attention classes and the list sort. Every session falls in one class,
// read from the run state, the session's attention count and its open
// attention items (so it works before and after per-session attention
// flags): needs you (approval, question, failure, open items other than a
// message), message for you (only messages are open), working, idle
// (waiting for input, offline or stale: the run may live on) and ended.
// The "attention" grouping groups by class; the attention sort floats
// needs-you sessions to the top inside any grouping.

type attnClass int

const (
	attnNeeds attnClass = iota
	attnMessage
	attnWorking
	attnIdle
	attnEnded
)

// attnGroupKeys are the attention grouping's group keys, in order.
var attnGroupKeys = []string{"1needs", "2message", "3working", "4idle", "5ended"}

var attnGroupLabels = map[string]string{"1needs": "Needs you", "2message": "Message for you", "3working": "Working", "4idle": "Idle", "5ended": "Ended"}

// openAttnKinds maps sessions to the kinds of their open attention items.
func (m *Model) openAttnKinds() map[string]map[contracts.AttentionKind]bool {
	out := map[string]map[contracts.AttentionKind]bool{}
	for _, a := range m.attention() {
		if a.State != contracts.AttnOpen || a.SessionID == "" {
			continue
		}
		if out[a.SessionID] == nil {
			out[a.SessionID] = map[contracts.AttentionKind]bool{}
		}
		out[a.SessionID][a.Kind] = true
	}
	return out
}

// attentionClass classifies s; kinds is openAttnKinds (nil: computed).
func (m *Model) attentionClass(s contracts.SessionView, kinds map[string]map[contracts.AttentionKind]bool) attnClass {
	if kinds == nil {
		kinds = m.openAttnKinds()
	}
	if sessionEnded(s) && (s.Run == nil || s.Run.State != contracts.RunFailed) {
		return attnEnded
	}
	st := sessionStatus(s, m.nodeOf(s))
	if st.tone == toneWarn || st.tone == toneBad {
		return attnNeeds // approval, question, failed
	}
	if k := kinds[s.Session.SessionID]; len(k) > 0 || s.Attention > 0 {
		if len(k) > 0 && onlyMessages(k) {
			return attnMessage
		}
		return attnNeeds
	}
	switch st.label {
	case "working", "running", "starting", "background", "exiting":
		return attnWorking
	}
	return attnIdle
}

func onlyMessages(k map[contracts.AttentionKind]bool) bool {
	for kind := range k {
		if kind != contracts.AttnMessage {
			return false
		}
	}
	return true
}

// ---- sort ----

type sortMode string

const (
	sortName      sortMode = "name"      // machine, then title (default)
	sortAttention sortMode = "attention" // needs you, message, then name
	sortActivity  sortMode = "activity"  // most recent activity first
)

var sortModes = []sortMode{sortName, sortAttention, sortActivity}

func parseSortMode(s string) (sortMode, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "name", "title", "machine", "default":
		return sortName, true
	case "attention", "needs", "needs-you", "urgent":
		return sortAttention, true
	case "activity", "recent", "time":
		return sortActivity, true
	}
	return "", false
}

func (m *Model) sortMode() sortMode {
	if m.grouping.sort == "" {
		return sortName
	}
	return m.grouping.sort
}

// SortMode is the Sessions list sort ("name", "attention", "activity").
func (m *Model) SortMode() string { return string(m.sortMode()) }

// setSortMode switches the sort inside groups and remembers it.
func (m *Model) setSortMode(s sortMode) tea.Cmd {
	m.grouping.sort = s
	m.syncSessions()
	label := map[sortMode]string{sortName: "sorted by machine, then title", sortAttention: "needs you first, then messages", sortActivity: "most recent activity first"}[s]
	return tea.Batch(m.saveState(), m.setToast(label, toneMuted), m.schedulePreview())
}

// cycleSortMode is g s.
func (m *Model) cycleSortMode() tea.Cmd {
	cur := m.sortMode()
	for i, s := range sortModes {
		if s == cur {
			return m.setSortMode(sortModes[(i+1)%len(sortModes)])
		}
	}
	return m.setSortMode(sortName)
}

// sortCommand is :sort [name|attention|activity]; no argument cycles.
func (m *Model) sortCommand() command {
	return command{name: "sort", args: "[mode]", desc: "order sessions inside their groups by name, attention (needs you first) or activity (no mode: next)",
		run: func(m *Model, args string) tea.Cmd {
			if strings.TrimSpace(args) == "" {
				return m.cycleSortMode()
			}
			s, ok := parseSortMode(args)
			if !ok {
				return m.setToast("sort by name, attention or activity", toneWarn)
			}
			return m.setSortMode(s)
		}}
}
