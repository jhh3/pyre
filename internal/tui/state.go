package tui

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
)

// tuiState is the small per-user TUI state file (Options.StateFile).
type tuiState struct {
	TipsDismissed bool `json:"tips_dismissed,omitempty"`
	ShowEnded     bool `json:"show_ended,omitempty"` // Sessions list shows exited/failed sessions
	// Sessions list grouping (grouping.go): project (default), machine,
	// repo or none, and the folded groups ("mode:key"). GroupVersion 2
	// marks Group as an explicit choice (before it, machine was the
	// unwritten default; see groupStateVersion).
	Group        string   `json:"group,omitempty"`
	GroupVersion int      `json:"group_version,omitempty"`
	Collapsed    []string `json:"collapsed,omitempty"`
	// Session trees (tree.go): parents whose children are folded.
	FoldedTrees []string `json:"folded_trees,omitempty"`
	// Sessions view (sessions_modes.go): sidenav (default), table or grid;
	// the list order (sort.go): grouped (default) or attention.
	SessionsView string `json:"sessions_view,omitempty"`
	Sort         string `json:"sort,omitempty"`
	// NewSession remembers the new-session panel's last machine and agent
	// overall, and the machine, agent and workspace per repository
	// (newsession.go).
	NewSession *nsMemory `json:"new_session,omitempty"`
	// Lead signals (leads.go): when each session was last on screen past
	// the dwell (bounded, archived pruned), and the baseline before which
	// nothing reads new (the first launch with lead signals).
	Seen      map[string]time.Time `json:"seen,omitempty"`
	SeenSince *time.Time           `json:"seen_since,omitempty"`
}

// loadTUIState reads Options.StateFile. Without a state file there is no
// first-launch tips bar (tests and embedders opt in).
func (m *Model) loadTUIState() {
	p := m.opts.StateFile
	if p == "" || p == "none" {
		return
	}
	var st tuiState
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	m.showTips = !st.TipsDismissed
	m.showEnded = st.ShowEnded
	m.grouping.load(st)
	m.sessView.load(st)
	m.seen.load(st, m.now())
	if st.NewSession != nil {
		m.nsMem = *st.NewSession
	}
}

// saveState writes the remembered TUI state (tips dismissed, ended
// sessions shown) in the background.
func (m *Model) saveState() tea.Cmd {
	p := m.opts.StateFile
	if p == "" || p == "none" {
		return nil
	}
	st := tuiState{TipsDismissed: !m.showTips, ShowEnded: m.showEnded}
	m.grouping.save(&st)
	m.sessView.save(&st)
	m.seen.save(&st)
	if !m.nsMem.empty() {
		mem := m.nsMem.clone()
		st.NewSession = &mem
	}
	seq := stateWrites.seq.Add(1)
	return func() tea.Msg {
		// Saves run on their own goroutines; an older one that runs late
		// must not overwrite a newer state, and two must not share the tmp
		// file at once.
		stateWrites.mu.Lock()
		defer stateWrites.mu.Unlock()
		if seq > stateWrites.last[p] {
			stateWrites.last[p] = seq
			_ = saveTUIState(p, st)
		}
		return nil
	}
}

// stateWrites orders background state saves per file.
var stateWrites = struct {
	seq  atomic.Uint64
	mu   sync.Mutex
	last map[string]uint64
}{last: map[string]uint64{}}

// dismissTips hides the tips bar and remembers that in the state file.
func (m *Model) dismissTips() tea.Cmd {
	if !m.showTips {
		return nil
	}
	m.showTips = false
	return m.saveState()
}

func saveTUIState(p string, st tuiState) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// tipsKeys are the five most useful keys, shown once on first launch.
var tipsKeys = []hint{{"n", "new session"}, {"enter", "type in"}, {"1-4", "switch tabs"}, {"␣␣", "find anything"}, {"?", "help"}}

// viewTips renders the first-launch tips bar.
func (m *Model) viewTips(w int) string {
	segs := make([]string, 0, len(tipsKeys))
	for _, h := range tipsKeys {
		segs = append(segs, m.hintSeg(h))
	}
	left := " " + m.th.accent.Render("Tips") + "  " + strings.Join(segs, m.th.faint.Render(" · ")) +
		m.th.faint.Render(" · ") + m.th.dim.Render("mouse: click & scroll")
	hide := m.mark(zoneDef{kind: zoneTipsHide}, m.th.key.Render("esc")+" "+m.th.dim.Render("hide ✕"))
	if width(left)+width(hide)+3 > w {
		left = trunc(left, max(0, w-width(hide)-3))
	}
	return left + strings.Repeat(" ", max(1, w-width(left)-width(hide)-1)) + hide
}
