package digest

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// firstSentence returns the first sentence of an agent message, as plain
// text, at most max runes: markdown headings, bullets, emphasis and code
// fences are dropped, the first non-empty line is taken, and it is cut at
// the first sentence end (". ", "! ", "? ", or a colon introducing a list).
func firstSentence(text string, max int) string {
	inFence := false
	line, heading := "", ""
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || t == "" || isRule(t) || strings.HasPrefix(t, "|") {
			continue
		}
		if strings.HasPrefix(t, "#") { // a heading titles what follows; use it only if nothing does
			if heading == "" {
				heading = t
			}
			continue
		}
		line = t
		break
	}
	if line == "" {
		line = heading
	}
	line = strings.TrimLeft(line, "#>*-+• \t")
	line = stripNumbering(line)
	line = strings.NewReplacer("**", "", "__", "", "`", "").Replace(line)
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return ""
	}
	if i := sentenceEnd(line); i > 0 {
		line = line[:i]
	}
	line = strings.TrimRight(line, " :;,")
	return clip(line, max)
}

func isRule(t string) bool {
	return strings.Trim(t, "-=*_ ") == "" && len(t) >= 3
}

func stripNumbering(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') && i+1 < len(s) && s[i+1] == ' ' {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

// sentenceEnd returns the byte index just after the first sentence's
// terminal punctuation, or 0. "e.g. ", "i.e. ", version numbers and file
// names ("main.go") do not end a sentence.
func sentenceEnd(s string) int {
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch != '.' && ch != '!' && ch != '?' {
			continue
		}
		if i+1 < len(s) && s[i+1] != ' ' {
			continue // "main.go", "v1.2", "?!"
		}
		if ch == '.' {
			w := lastWord(s[:i])
			switch strings.ToLower(w) {
			case "e.g", "i.e", "etc", "vs", "mr", "dr", "cf", "no":
				continue
			}
			if len(w) == 1 && unicode.IsUpper(rune(w[0])) {
				continue // an initial
			}
		}
		if i < 12 && i+1 < len(s) {
			continue // too short to be a whole sentence ("Done. ", "OK. ") — keep going
		}
		return i + 1
	}
	return 0
}

func lastWord(s string) string {
	i := strings.LastIndexAny(s, " (")
	return s[i+1:]
}

func clip(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	cut := string(r[:max-1])
	if i := strings.LastIndex(cut, " "); i > max*2/3 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}

// toolCalls counts the tool calls in a transcript tool entry and returns
// the files they changed. Claude entries join calls with "; " as
// "Name: argument"; Codex entries are "file change: a, b", "command: …"
// or "mcp: server.tool".
func toolCalls(text string) (int, []string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0, nil
	}
	if rest, ok := strings.CutPrefix(text, "file change: "); ok {
		var files []string
		for _, f := range strings.Split(rest, ", ") {
			if f = strings.TrimSpace(f); f != "" {
				files = append(files, f)
			}
		}
		return 1, files
	}
	if strings.HasPrefix(text, "command: ") || strings.HasPrefix(text, "mcp: ") {
		return 1, nil
	}
	parts := strings.Split(text, "; ")
	var files []string
	for _, p := range parts {
		name, arg, _ := strings.Cut(p, ": ")
		switch strings.TrimSpace(name) {
		case "Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch":
			if a := strings.TrimSpace(arg); a != "" && !strings.ContainsAny(a, "\n") {
				files = append(files, a)
			}
		}
	}
	return len(parts), files
}
