// Package envfile reads a session's environment file (pyre start
// --env-file, MCP start_session env_file): dotenv-style KEY=VALUE lines the
// node applies to the harness process at every launch of the session.
//
// Only the file's path travels and is recorded; the values are read on the
// node, never stored on the hub or in run adapter data, and never logged:
// every error here names a line number or a key, never a value.
package envfile

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/fileperm"
)

// MaxSize bounds an environment file.
const MaxSize = 64 << 10

// Vars is a parsed environment file: variables to set and variables to
// remove (a refused auth key given empty, e.g. ANTHROPIC_API_KEY=).
type Vars struct {
	Set   map[string]string
	Unset []string
}

// Keys lists every key the file names, sorted (for messages and dry runs).
func (v Vars) Keys() []string {
	out := make([]string, 0, len(v.Set)+len(v.Unset))
	for k := range v.Set {
		out = append(out, k)
	}
	out = append(out, v.Unset...)
	slices.Sort(out)
	return out
}

// Redacted renders the file's effect without values: KEY=<redacted> for a
// set variable (KEY= for an empty one) and KEY (unset) for a removed one.
func (v Vars) Redacted() []string {
	var out []string
	for _, k := range v.Keys() {
		val, set := v.Set[k]
		switch {
		case !set:
			out = append(out, k+" (unset)")
		case val == "":
			out = append(out, k+"=")
		default:
			out = append(out, k+"=<redacted>")
		}
	}
	return out
}

var keyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Pyre sets these for every run (identity, account binding, tools on PATH,
// home directories, its tmux and private temp dirs): a session's file never
// replaces them. Compared upper-cased: Windows names are case-insensitive,
// and nobody needs path= or pyre_x= on Unix either.
var controlledKeys = []string{"HOME", "PATH", "CLAUDE_CONFIG_DIR",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
	"TMUX", "TMUX_PANE", "CLAUDE_CODE_TMPDIR"}

// Refused says why key=value cannot come from a session's environment file
// ("" when it can). Login, credential, config-home and provider variables
// (contracts.IsLoginEnv, the list the account binding unsets) may only be
// cleared: an empty value removes the variable.
func Refused(key, value string) string {
	k := strings.ToUpper(key)
	if slices.Contains(controlledKeys, k) || strings.HasPrefix(k, "PYRE_") {
		return key + " is set by Pyre"
	}
	if value != "" && contracts.IsLoginEnv(k) {
		return key + " is a login, provider or config-home variable the run's account decides (only an empty value, which unsets it, is allowed)"
	}
	return ""
}

// IsAuthKey reports whether key is a login/provider variable (the ones an
// empty value unsets).
func IsAuthKey(key string) bool { return contracts.IsLoginEnv(key) }

// FoldKey is the identity of an environment variable name on this OS:
// upper-cased on Windows, where names are case-insensitive.
func FoldKey(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

// Parse reads dotenv-style lines: blank lines and # comments are skipped;
// each other line is [export ]KEY=VALUE. VALUE is taken literally after
// trimming surrounding whitespace, a " #" comment ends an unquoted value,
// 'single quotes' keep their content exactly, and "double quotes" also
// understand \n, \t, \" and \\. A key may appear once. Refused keys
// (Refused) are errors; an auth key with an empty value lands in Unset.
func Parse(r io.Reader) (Vars, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxSize+1))
	if err != nil {
		return Vars{}, err
	}
	if len(b) > MaxSize {
		return Vars{}, fmt.Errorf("larger than %d KiB", MaxSize>>10)
	}
	if bytes.IndexByte(b, 0) >= 0 {
		return Vars{}, fmt.Errorf("contains a NUL byte")
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	v := Vars{Set: map[string]string{}}
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), MaxSize+1)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(strings.TrimSuffix(sc.Text(), "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = strings.TrimSpace(rest)
		}
		key, raw, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || !keyRe.MatchString(key) {
			return Vars{}, fmt.Errorf("line %d: not KEY=VALUE", n)
		}
		val, err := parseValue(strings.TrimSpace(raw))
		if err != nil {
			return Vars{}, fmt.Errorf("line %d (%s): %v", n, key, err)
		}
		if first, dup := seen[FoldKey(key)]; dup {
			return Vars{}, fmt.Errorf("line %d: %s is already set on line %d", n, key, first)
		}
		seen[FoldKey(key)] = n
		if why := Refused(key, val); why != "" {
			return Vars{}, fmt.Errorf("line %d: %s", n, why)
		}
		if val == "" && IsAuthKey(key) {
			v.Unset = append(v.Unset, key)
			continue
		}
		v.Set[key] = val
	}
	if err := sc.Err(); err != nil {
		return Vars{}, err
	}
	return v, nil
}

func parseValue(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	switch s[0] {
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", fmt.Errorf("unterminated single quote (values span one line)")
		}
		if err := trailing(s[end+2:]); err != nil {
			return "", err
		}
		return s[1 : end+1], nil
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			ch := s[i]
			switch {
			case ch == '"':
				if err := trailing(s[i+1:]); err != nil {
					return "", err
				}
				return b.String(), nil
			case ch == '\\' && i+1 < len(s):
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case '"', '\\':
					b.WriteByte(s[i])
				default:
					b.WriteByte('\\')
					b.WriteByte(s[i])
				}
			default:
				b.WriteByte(ch)
			}
		}
		return "", fmt.Errorf("unterminated double quote (values span one line)")
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '#' && (s[i-1] == ' ' || s[i-1] == '\t') {
			return strings.TrimSpace(s[:i]), nil
		}
	}
	return s, nil
}

// trailing allows only whitespace or a comment after a quoted value.
func trailing(s string) error {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "#") {
		return nil
	}
	return fmt.Errorf("unexpected text after the closing quote")
}

// Read checks path and parses the file: an absolute path outside device
// and kernel namespaces (/dev, /proc, /sys; UNC and \\.\ paths), a regular
// file (checked before opening, so a FIFO or device never blocks the
// caller, and again on the open descriptor), owned by the current user and
// private to it (CheckFile).
func Read(path string) (Vars, error) {
	if err := CheckPath(path); err != nil {
		return Vars{}, err
	}
	lst, err := os.Lstat(path)
	if err != nil {
		return Vars{}, fmt.Errorf("env file %s: %w", path, err)
	}
	if !lst.Mode().IsRegular() {
		return Vars{}, fmt.Errorf("env file %s is not a regular file (%s)", path, kind(lst.Mode()))
	}
	f, err := openNonBlocking(path)
	if err != nil {
		return Vars{}, fmt.Errorf("env file %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Vars{}, fmt.Errorf("env file %s: %w", path, err)
	}
	if !os.SameFile(lst, fi) {
		return Vars{}, fmt.Errorf("env file %s changed while it was opened", path)
	}
	if err := CheckFile(path, fi); err != nil {
		return Vars{}, err
	}
	v, err := Parse(f)
	if err != nil {
		return Vars{}, fmt.Errorf("env file %s: %w", path, err)
	}
	return v, nil
}

// CheckPath refuses a path that is not absolute on this OS or that names a
// device, kernel or network namespace.
func CheckPath(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("env file %q: the path must be absolute (a path on the node)", path)
	}
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, "//") {
		return fmt.Errorf("env file %s: network and device paths (\\\\server\\share, \\\\.\\, \\\\?\\) are refused; use a local file", path)
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	for _, ns := range []string{"/dev", "/proc", "/sys"} {
		if clean == ns || strings.HasPrefix(clean, ns+"/") {
			return fmt.Errorf("env file %s: paths under %s are refused; use a regular file", path, ns)
		}
	}
	return nil
}

func kind(m os.FileMode) string {
	switch {
	case m&os.ModeSymlink != 0:
		return "a symbolic link"
	case m&os.ModeNamedPipe != 0:
		return "a named pipe"
	case m&os.ModeSocket != 0:
		return "a socket"
	case m&os.ModeDevice != 0:
		return "a device"
	case m.IsDir():
		return "a directory"
	}
	return "not a regular file"
}

// CheckFile refuses an environment file that is not private to this node's
// user: not a regular file, owned by someone else, or accessible to group
// or others (Unix: any group/other mode bit; Windows: any ACL entry for
// another principal).
func CheckFile(path string, fi os.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("env file %s is not a regular file", path)
	}
	if who, foreign := fileperm.ForeignOwner(path, fi); foreign {
		return fmt.Errorf("env file %s is owned by %s, not this node's user", path, who)
	}
	if why, bad := fileperm.Exposed(path, fi); bad {
		// Unix: any group/other bit (it holds secrets: chmod 600, as the
		// launcher's own file); Windows: an ACL entry for another principal.
		return fmt.Errorf("env file %s is accessible to other users (%s); %s", path, why, fileperm.FixHint(path, false))
	}
	return nil
}
