package hive

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
)

// entry is one row of hive-mem's JSON output (`search/recent --json` emit the
// rank.sql shape; `show --json` emits the full row). Unknown fields are
// ignored so additive Hive changes do not break decoding.
type entry struct {
	ID         int64   `json:"id"`
	UID        string  `json:"uid"`
	TS         int64   `json:"ts"`
	Kind       string  `json:"kind"`
	RepoKey    string  `json:"repo_key"`
	Worktree   string  `json:"worktree"`
	Branch     string  `json:"branch"`
	RepoPath   string  `json:"repo_path"`
	Origin     string  `json:"origin"`
	Title      string  `json:"title"`
	Cont       string  `json:"cont"`
	Request    string  `json:"request"`
	Outcome    string  `json:"outcome"`
	Learned    string  `json:"learned"`
	Decided    string  `json:"decided"`
	NextSteps  string  `json:"next_steps"`
	Superseded int     `json:"superseded"`
	Score      float64 `json:"score"`
}

// Bridge marker. Every hub note published into Hive starts with
//
//	[pyre:<note_id> <kind>[ <project_id>][ promoted]]
//
// It sits in the first 200 bytes so it survives into Hive's title (hive-mem
// flattens the note text into the title), is a single FTS token
// (`note_…`: '_' is a tokenchar), and passes Hive's redaction and
// sanitisation unchanged. It is the deterministic key that makes publishing
// idempotent and lets search hits be mapped back to hub notes.
var markerRE = regexp.MustCompile(`\[pyre:(note_[A-Za-z0-9.:-]+)(?: (note|handoff|decision))?(?: (prj_[A-Za-z0-9.:-]+))?( promoted)?\]`)

type marker struct {
	NoteID    string
	Kind      string
	ProjectID string
	Promoted  bool
}

func formatMarker(m marker) string {
	var b strings.Builder
	b.WriteString("[pyre:")
	b.WriteString(m.NoteID)
	b.WriteString(" ")
	b.WriteString(m.Kind)
	if m.ProjectID != "" {
		b.WriteString(" ")
		b.WriteString(m.ProjectID)
	}
	if m.Promoted {
		b.WriteString(" promoted")
	}
	b.WriteString("]")
	return b.String()
}

func parseMarker(s string) (marker, bool) {
	sub := markerRE.FindStringSubmatch(s)
	if sub == nil {
		return marker{}, false
	}
	m := marker{NoteID: sub[1], Kind: sub[2], ProjectID: sub[3], Promoted: sub[4] != ""}
	if m.Kind == "" {
		m.Kind = "note"
	}
	return m, true
}

func stripMarker(s string) string {
	return strings.TrimSpace(markerRE.ReplaceAllString(s, ""))
}

// noteKindFor maps a hub note kind onto hive-mem's note kinds (it has no
// "handoff"; the marker carries that). Unknown future kinds are tolerated
// and published as plain notes.
func noteKindFor(k contracts.NoteKind) (hiveKind, markerKind string) {
	switch k {
	case contracts.NoteDecision:
		return "decision", "decision"
	case contracts.NoteHandoff:
		return "note", "handoff"
	default:
		return "note", "note"
	}
}

// classify maps a Hive entry to MemoryHit.Kind and Authoritative.
//
// Only hub material that was explicitly promoted to a project document
// revision is authoritative. Hive-captured "decision" rows are distilled from
// transcripts (supporting evidence), and explicit Hive/hub notes are notes;
// neither is silently promoted.
func classify(e entry) (kind string, authoritative bool, m *marker) {
	if mk, ok := parseMarker(e.Title); ok {
		return mk.Kind, mk.Promoted, &mk
	}
	if e.Origin == "noted" {
		if e.Kind == "decision" {
			return "decision", false, nil
		}
		return "note", false, nil
	}
	// live / legacy-session / legacy-note / legacy-prompt and any future
	// origin: transcript-derived evidence.
	return "transcript", false, nil
}

// source renders provenance as space-separated key=value pairs, omitting
// unknown parts: "hive machine=… session=… path=… repo=… uid=…".
func source(e entry, prov *sessionProv, localMachine string, m *marker) string {
	parts := []string{"hive"}
	machine := ""
	if prov != nil && prov.Machine != "" {
		machine = prov.Machine
	} else if e.Origin == "noted" && localMachine != "" && m != nil {
		machine = localMachine // bridge notes are written on this node
	}
	if machine != "" {
		parts = append(parts, "machine="+machine)
	}
	if e.Origin == "live" && e.UID != "" {
		parts = append(parts, "session="+e.UID)
	}
	if prov != nil && prov.TranscriptPath != "" {
		parts = append(parts, "path="+prov.TranscriptPath)
	} else if e.RepoPath != "" {
		parts = append(parts, "path="+e.RepoPath)
	}
	if e.RepoKey != "" {
		parts = append(parts, "repo="+e.RepoKey)
	}
	if m != nil {
		parts = append(parts, "pyre_note="+m.NoteID)
	}
	parts = append(parts, "uid="+e.UID)
	return strings.Join(parts, " ")
}

func toHit(e entry, prov *sessionProv, localMachine string, snippetMax int) contracts.MemoryHit {
	kind, auth, m := classify(e)
	snip := e.Cont
	if snip == "" {
		snip = e.Title
	}
	h := contracts.MemoryHit{
		ID:            e.UID,
		Kind:          kind,
		Title:         truncBytes(flatten(stripMarker(e.Title)), 200),
		Snippet:       truncBytes(flatten(stripMarker(snip)), snippetMax),
		Source:        source(e, prov, localMachine, m),
		At:            time.Unix(e.TS, 0).UTC(),
		Authoritative: auth,
	}
	if e.Origin == "live" {
		h.SessionRef = e.UID
	}
	if m != nil {
		h.ProjectID = m.ProjectID
	}
	return h
}

// detailText assembles a bounded body for memory.get from the distilled
// fields (no transcript text beyond what Hive stored).
func detailText(e entry, max int) string {
	var b strings.Builder
	add := func(label, v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		if label != "" {
			b.WriteString(label + ": ")
		}
		b.WriteString(v)
	}
	add("", stripMarker(e.Outcome))
	add("learned", e.Learned)
	add("decided", e.Decided)
	add("next", e.NextSteps)
	if b.Len() == 0 {
		add("", stripMarker(e.Title))
	}
	return truncBytes(b.String(), max)
}

func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

// truncBytes cuts s to at most n bytes on a UTF-8 boundary, marking the cut
// with "…".
func truncBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const ell = "…"
	cut := n - len(ell)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}
