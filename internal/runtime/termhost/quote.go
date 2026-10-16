package termhost

import (
	"regexp"
	"runtime"
	"strings"
)

var safeArgRe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./\\-]+$`)

// remoteCommand renders argv as the command an ssh server runs on this
// node: POSIX sh quoting, or Windows command-line quoting on Windows
// (OpenSSH for Windows hands the string to its default shell, cmd.exe).
func remoteCommand(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		if runtime.GOOS == "windows" {
			q[i] = windowsQuote(a)
		} else {
			q[i] = posixQuote(a)
		}
	}
	return strings.Join(q, " ")
}

func posixQuote(s string) string {
	if s != "" && safeArgRe.MatchString(s) && !strings.Contains(s, `\`) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// windowsQuote quotes one argument by the CommandLineToArgvW rules.
func windowsQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\v\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			slashes++
		case '"':
			// The slashes already written plus slashes+1 more: 2n+1 before a quote.
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
			b.WriteByte('"')
			continue
		default:
			slashes = 0
		}
		b.WriteByte(c)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}
