package secretegress

import (
	"path"
	"strings"
)

// command is one simple command of a shell line: its words (quotes
// removed) and the bodies of heredocs that feed it.
type command struct {
	words    []string
	heredocs []string
	// quotedDocs are the bodies of heredocs with a quoted delimiter
	// (<<'EOF'): the shell does not expand them.
	quotedDocs []string
}

// name is the command's program: the first word that is not an
// assignment or a wrapper (sudo, env with arguments, nohup, timeout N,
// xargs, ...), as a lowercased base name without ".exe". rest are the
// words after it.
func (c command) name() (string, []string) {
	w := c.words
	for len(w) > 0 {
		word := w[0]
		if isAssignment(word) {
			w = w[1:]
			continue
		}
		n := progName(word)
		switch n {
		case "{", "}", "!", "if", "then", "else", "elif", "do", "while", "until":
			w = w[1:]
			continue
		case "sudo", "doas", "command", "exec", "nohup", "time", "nice", "stdbuf", "caffeinate", "builtin", "unbuffer":
			w = skipFlags(w[1:])
			continue
		case "timeout":
			w = skipFlags(w[1:])
			if len(w) > 0 {
				w = w[1:] // the duration
			}
			continue
		case "xargs":
			w = skipFlags(w[1:])
			continue
		case "env":
			r := skipFlags(w[1:])
			for len(r) > 0 && isAssignment(r[0]) {
				r = r[1:]
			}
			if len(r) == 0 {
				return "env", nil // a bare env prints the environment
			}
			w = r
			continue
		}
		return n, w[1:]
	}
	return "", nil
}

func progName(word string) string {
	n := strings.ToLower(path.Base(strings.ReplaceAll(word, "\\", "/")))
	return strings.TrimSuffix(n, ".exe")
}

func skipFlags(w []string) []string {
	for len(w) > 0 && strings.HasPrefix(w[0], "-") {
		w = w[1:]
	}
	return w
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := w[j]
		if !(c == '_' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || j > 0 && '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// maxDepth bounds recursion into bash -c strings and $(...) substitutions.
const maxDepth = 4

// parse splits a shell line into simple commands. It is a forgiving lexer,
// not a shell: quotes group words, | ; & ( ) newlines and backticks end a
// command, $(...) inside double quotes is parsed as its own commands, and a
// heredoc body is attached to the command that reads it instead of being
// parsed as commands (a commit message that mentions curl is not a call).
func parse(s string, depth int) []command {
	var (
		cmds    []command
		cur     command
		word    strings.Builder
		inWord  bool
		pending []heredocSpec // heredocs opened on this line, read at its newline
	)
	endWord := func() {
		if inWord {
			cur.words = append(cur.words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(cur.words) > 0 || len(cur.heredocs) > 0 {
			cmds = append(cmds, cur)
		}
		cur = command{}
	}
	sub := func(inner string) {
		if depth < maxDepth {
			cmds = append(cmds, parse(inner, depth+1)...)
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			if s[i] != '\n' {
				word.WriteByte(s[i])
				inWord = true
			}
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				j = len(s) - i - 1
			}
			word.WriteString(s[i+1 : i+1+j])
			inWord = true
			i += j + 1
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '$' && j+1 < len(s) && s[j+1] == '(' {
					if k := matchParen(s, j+1); k > 0 {
						sub(s[j+2 : k])
						j = k + 1
						continue
					}
				}
				j++
			}
			if j > len(s) {
				j = len(s)
			}
			word.WriteString(s[i+1 : j])
			inWord = true
			i = j
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			if k := matchParen(s, i+1); k > 0 {
				sub(s[i+2 : k])
				word.WriteString(s[i : k+1])
				inWord = true
				i = k
				continue
			}
			word.WriteByte(c)
			inWord = true
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			endWord()
			spec, n := readHeredocSpec(s[i+2:])
			i += 1 + n
			if spec.delim != "" {
				pending = append(pending, spec)
			}
		case c == '\n':
			endCmd()
			if len(pending) > 0 {
				// The heredoc bodies follow this line; attach them to the
				// last command of the line (the reader of a pipe's input).
				pos := i + 1
				for k, h := range pending {
					body, n := readHeredocBody(s[pos:], h)
					if len(cmds) > 0 {
						last := &cmds[len(cmds)-1]
						last.heredocs = append(last.heredocs, body)
						if h.quoted {
							last.quotedDocs = append(last.quotedDocs, body)
						}
					}
					pos += n
					if k < len(pending)-1 && pos < len(s) && s[pos] == '\n' {
						pos++
					}
				}
				i = pos - 1 // the next byte is the newline after the last delimiter
				pending = nil
			}
		case c == '|' || c == ';' || c == '&' || c == '(' || c == ')' || c == '`':
			endCmd()
		case c == ' ' || c == '\t' || c == '\r':
			endWord()
		case c == '#' && !inWord:
			// A comment runs to the end of the line.
			j := strings.IndexByte(s[i:], '\n')
			if j < 0 {
				i = len(s)
			} else {
				i += j - 1
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	endCmd()
	return cmds
}

// matchParen returns the index of the ')' closing the '(' at open, or -1.
// Quoted text and heredoc bodies inside are skipped, so a PR body written
// with "$(cat <<'EOF' ... EOF)" may hold any punctuation.
func matchParen(s string, open int) int {
	d := 0
	var pending []heredocSpec
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			d++
		case ')':
			d--
			if d == 0 {
				return i
			}
		case '\\':
			i++
		case '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
				i += j + 1
			}
		case '"':
			for i++; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' {
					i++
				}
			}
		case '<':
			if strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<") {
				spec, n := readHeredocSpec(s[i+2:])
				i += 1 + n
				if spec.delim != "" {
					pending = append(pending, spec)
				}
			}
		case '\n':
			for _, h := range pending {
				_, n := readHeredocBody(s[i+1:], h)
				i += n
				if i+1 < len(s) && s[i+1] == '\n' {
					i++
				}
			}
			pending = nil
		}
	}
	return -1
}

type heredocSpec struct {
	delim  string
	strip  bool // <<- strips leading tabs
	quoted bool // <<'EOF': the body is not expanded
}

// readHeredocSpec reads "[-~] ['"]DELIM['"]" after "<<"; n is how many
// bytes it consumed.
func readHeredocSpec(s string) (heredocSpec, int) {
	var h heredocSpec
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '~') {
		h.strip = true
		i++
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	q := byte(0)
	if i < len(s) && (s[i] == '\'' || s[i] == '"') {
		q = s[i]
		h.quoted = true
		i++
	}
	start := i
	for i < len(s) {
		c := s[i]
		if q != 0 && c == q || q == 0 && (c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '|' || c == '&' || c == ')' || c == '<' || c == '>') {
			break
		}
		i++
	}
	h.delim = s[start:i]
	if q != 0 && i < len(s) {
		i++
	}
	return h, i
}

// readHeredocBody reads lines until one equal to the delimiter; n is how
// many bytes it consumed (through the delimiter line, not its newline).
func readHeredocBody(s string, h heredocSpec) (string, int) {
	off := 0
	for off <= len(s) {
		end := strings.IndexByte(s[off:], '\n')
		line := ""
		if end < 0 {
			line = s[off:]
		} else {
			line = s[off : off+end]
		}
		t := strings.TrimRight(line, "\r")
		if h.strip {
			t = strings.TrimLeft(t, "\t ")
		}
		if t == h.delim {
			return s[:off], off + len(line)
		}
		if end < 0 {
			break
		}
		off += end + 1
	}
	return s, len(s)
}
