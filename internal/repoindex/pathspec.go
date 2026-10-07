package repoindex

import (
	"path"
	"strings"
)

// Plain-directory launch targets (W-qlpath): `pyre new ~/scratch` and the
// quick-launch bar take a path wherever they take a repository name.

// IsPathSpec reports whether a launch target is a directory path rather
// than a repository name: it starts with /, ~, ./ or ../ (or is ~, . or ..).
// The word "home" is a path only when no repository has that name, which
// the caller decides.
func IsPathSpec(s string) bool {
	switch s {
	case "~", ".", "..":
		return true
	}
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") || strings.HasPrefix(s, "./") || strings.HasPrefix(s, "../")
}

// NormPath cleans a path spec into "~", "~/…" or an absolute path, without
// a trailing slash (except "/"). "home" is "~". Relative specs resolve
// against cwd, and a result inside home is written with "~". dir reports a
// trailing slash in the spec (the folder itself, its contents listed).
func NormPath(spec, cwd, home string) (p string, dir bool) {
	dir = strings.HasSuffix(spec, "/") || spec == "~" || spec == "home"
	switch {
	case spec == "home" || spec == "~":
		return "~", true
	case strings.HasPrefix(spec, "~/"):
		rest := path.Clean("/" + strings.TrimPrefix(spec, "~/"))
		if rest == "/" {
			return "~", true
		}
		return "~" + rest, dir
	case strings.HasPrefix(spec, "/"):
		p = path.Clean(spec)
	default:
		if cwd == "" {
			cwd = "/"
		}
		p = path.Join(cwd, spec)
	}
	if home != "" && home != "/" {
		h := strings.TrimSuffix(home, "/")
		if p == h {
			return "~", true
		}
		if strings.HasPrefix(p, h+"/") {
			return "~" + p[len(h):], dir || p == "/"
		}
	}
	return p, dir || p == "/"
}

// SplitPath gives the folder whose listing completes p, and the name typed
// inside it ("" when p itself is the folder to list).
func SplitPath(p string, dir bool) (parent, name string) {
	if dir || p == "~" || p == "/" {
		return p, ""
	}
	i := strings.LastIndexByte(p, '/')
	switch {
	case i < 0:
		return "~", p
	case i == 0:
		return "/", p[1:]
	}
	return p[:i], p[i+1:]
}

// JoinPath appends a child folder name to a normalised path.
func JoinPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// SplitTarget splits a launch line into its first word (the target) and the
// remaining whitespace-separated words. The target may contain spaces when
// quoted ('My Docs' or "My Docs", also mid-word as ~/"My Docs") or escaped
// with a backslash (~/My\ Docs); an unterminated quote runs to the end of the
// line. Only the target is unquoted: the rest (machine, harness, prompt) is
// split on whitespace as typed, so a prompt like "don't" is left alone.
// open reports that the target is still being typed (nothing follows it, not
// even a space).
func SplitTarget(line string) (target string, rest []string, open bool) {
	s := strings.TrimLeft(line, " \t")
	var b strings.Builder
	var quote rune
	i := 0
	rs := []rune(s)
	for ; i < len(rs); i++ {
		c := rs[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(rs) && (rs[i+1] == '"' || rs[i+1] == '\\') {
				i++
				b.WriteRune(rs[i])
			} else {
				b.WriteRune(c)
			}
			continue
		case c == '\'' || c == '"':
			quote = c
			continue
		case c == '\\' && i+1 < len(rs):
			i++
			b.WriteRune(rs[i])
			continue
		case c == ' ' || c == '\t':
		default:
			b.WriteRune(c)
			continue
		}
		break
	}
	tail := string(rs[i:])
	if rest = strings.Fields(tail); len(rest) == 0 {
		rest = nil
	}
	return b.String(), rest, tail == ""
}

// QuoteTarget writes a target so SplitTarget reads it back: spaces, quotes
// and backslashes are backslash-escaped (a leading ~ stays bare).
func QuoteTarget(s string) string {
	if !strings.ContainsAny(s, " \t'\"\\") {
		return s
	}
	var b strings.Builder
	for _, c := range s {
		if c == ' ' || c == '\t' || c == '\'' || c == '"' || c == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}
