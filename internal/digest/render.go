package digest

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Glyph is the one-cell marker for a line: ! needs you, ✗ blocked, ✓ done,
// ● working.
func (a Agent) Glyph() string {
	switch {
	case a.Rank == RankNeedsYou && len(a.NeedsYou) == 0:
		return "✗"
	case a.Rank == RankNeedsYou:
		return "!"
	case a.Rank == RankDone:
		return "✓"
	case a.Rank == RankWorking:
		return "●"
	}
	return "·"
}

// Word is the short state word for a line.
func (a Agent) Word() string {
	switch {
	case a.Rank == RankNeedsYou && len(a.NeedsYou) == 0:
		return "blocked"
	case a.Rank == RankNeedsYou:
		return "needs you"
	case a.Rank == RankDone:
		return "done"
	case a.Rank == RankWorking:
		return "working"
	}
	return "quiet"
}

// Facts is the compact activity line: "3 turns · 14 tools · 6 files ·
// 1 artifact · 2 messages". Partial counts carry a "+".
func (a Agent) Facts() string {
	var out []string
	add := func(n int, one, many string) {
		if n <= 0 {
			return
		}
		s := plural(n, one, many)
		if a.Partial && (one == "turn" || one == "tool") {
			s = strconv.Itoa(n) + "+ " + many
			if n == 1 {
				s = "1+ " + one
			}
		}
		out = append(out, s)
	}
	add(a.Turns, "turn", "turns")
	add(a.Tools, "tool", "tools")
	add(len(a.Files), "file", "files")
	add(len(a.Artifacts), "artifact", "artifacts")
	add(len(a.Docs), "doc", "docs")
	add(len(a.Messages), "message", "messages")
	add(len(a.Notes), "note", "notes")
	return strings.Join(out, " · ")
}

// Where is "Claude on server".
func (a Agent) Where() string {
	h := titleWord(string(a.Harness))
	switch {
	case h != "" && a.Node != "":
		return h + " on " + a.Node
	case h != "":
		return h
	}
	return a.Node
}

// Header is the one-line title: "While you were away · last seen 3h ago ·
// 5 agents, 2 need you".
func (d Digest) Header(now time.Time) string {
	parts := []string{"While you were away"}
	switch {
	case d.SinceLabel == "last seen":
		parts = append(parts, "since "+d.Since.Local().Format("15:04")+" ("+Ago(now, d.Since)+")")
	case d.SinceLabel != "":
		parts = append(parts, d.SinceLabel)
	}
	n := len(d.Agents)
	s := plural(n, "agent", "agents")
	if k := d.Count(RankNeedsYou); k > 0 {
		s += ", " + strconv.Itoa(k) + " need"
		if k == 1 {
			s += "s"
		}
		s += " you"
	}
	parts = append(parts, s)
	return strings.Join(parts, " · ")
}

// Ago is a compact relative time: "now", "4m ago", "3h ago", "2d ago".
func Ago(now, t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	}
	return strconv.Itoa(int(d.Hours()/24)) + "d ago"
}

func sectionName(r Rank) string {
	switch r {
	case RankNeedsYou:
		return "Needs you"
	case RankDone:
		return "Done"
	case RankWorking:
		return "Still working"
	}
	return ""
}

// WriteText renders the digest for a terminal (no colour): one block per
// agent, grouped needs you → done → still working, each with its deep
// link. Files, artifacts, documents and messages are listed under the line.
func WriteText(w io.Writer, d Digest, now time.Time) {
	fmt.Fprintln(w, d.Header(now))
	if d.Summary != "" {
		fmt.Fprintln(w)
		for _, l := range strings.Split(strings.TrimSpace(d.Summary), "\n") {
			fmt.Fprintln(w, "  "+l)
		}
	}
	if d.Empty() {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Nothing happened that needs a summary.")
		if d.Quiet > 0 {
			fmt.Fprintf(w, "(%s with only status changes)\n", plural(d.Quiet, "quiet session", "quiet sessions"))
		}
		return
	}
	tw := 0
	for _, a := range d.Agents {
		tw = max(tw, len([]rune(a.Title)))
	}
	tw = min(max(tw, 8), 28)
	indent := strings.Repeat(" ", 2+2+tw+2)
	cur := Rank("")
	for _, a := range d.Agents {
		if a.Rank != cur {
			cur = a.Rank
			fmt.Fprintln(w)
			fmt.Fprintln(w, sectionName(cur))
		}
		fmt.Fprintf(w, "  %s %s  %s\n", a.Glyph(), padRunes(clip(a.Title, tw), tw), a.Summary)
		meta := []string{}
		if s := a.Where(); s != "" {
			meta = append(meta, s)
		}
		meta = append(meta, Ago(now, a.LastActivity))
		if f := a.Facts(); f != "" {
			meta = append(meta, f)
		}
		fmt.Fprintln(w, indent+strings.Join(meta, " · "))
		for _, r := range a.NeedsYou[min(1, len(a.NeedsYou)):] {
			fmt.Fprintln(w, indent+"also needs you: "+r.Title+"  "+r.Link)
		}
		if len(a.Files) > 0 {
			fmt.Fprintln(w, indent+"files: "+listSome(a.Files, 5))
		}
		for _, r := range a.Artifacts {
			fmt.Fprintln(w, indent+"artifact: "+r.Title+"  "+r.Link)
		}
		for _, r := range a.Docs {
			fmt.Fprintln(w, indent+"doc: "+r.Title+"  "+r.Link)
		}
		for _, r := range a.Notes {
			fmt.Fprintln(w, indent+"note: "+r.Title+"  "+r.Link)
		}
		if n := len(a.Messages); n > 0 {
			last := a.Messages[n-1]
			fmt.Fprintln(w, indent+"message "+last.Direction+": "+last.Title+"  "+last.Link)
		}
		fmt.Fprintln(w, indent+a.Link)
	}
	if len(d.Other) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Also needs you")
		for _, r := range d.Other {
			fmt.Fprintf(w, "  ! %s  %s\n", r.Title, r.Link)
		}
	}
	var tail []string
	if d.Quiet > 0 {
		tail = append(tail, plural(d.Quiet, "quiet session", "quiet sessions")+" collapsed")
	}
	if d.Truncated {
		tail = append(tail, "older history was not read (bound or retention)")
	}
	for _, wn := range d.Warnings {
		tail = append(tail, wn)
	}
	if len(tail) > 0 {
		fmt.Fprintln(w)
		for _, t := range tail {
			fmt.Fprintln(w, t)
		}
	}
}

func listSome(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + " +" + strconv.Itoa(len(xs)-n)
}

func padRunes(s string, n int) string {
	if k := len([]rune(s)); k < n {
		return s + strings.Repeat(" ", n-k)
	}
	return s
}
