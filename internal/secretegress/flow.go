package secretegress

import (
	"regexp"
	"strings"
)

// statements splits a shell line into its top-level statements: the parts
// separated by ; && || & and newlines, outside quotes, $(...), subshells,
// heredoc bodies and compound commands (if/for/while/case/{ } stay whole).
// A pipeline is one statement: its data flows from one command to the next.
func statements(s string) []string {
	var out []string
	start, depth := 0, 0
	atStart := true
	var pending []heredocSpec
	cut := func(end int) {
		if depth == 0 {
			if t := strings.TrimSpace(s[start:end]); t != "" {
				out = append(out, t)
			}
			start = end + 1
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			i++
			atStart = false
		case c == '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
				i += j + 1
			} else {
				i = len(s)
			}
			atStart = false
		case c == '"':
			j := i + 1
			for j < len(s) && s[j] != '"' {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '$' && j+1 < len(s) && s[j+1] == '(' {
					if k := matchParen(s, j+1); k > 0 {
						j = k + 1
						continue
					}
				}
				j++
			}
			i = j
			atStart = false
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			if k := matchParen(s, i+1); k > 0 {
				i = k
			}
			atStart = false
		case c == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j >= 0 {
				i += j + 1
			}
			atStart = false
		case c == '(':
			if k := matchParen(s, i); k > 0 {
				i = k
			}
			atStart = false
		case c == '<' && strings.HasPrefix(s[i:], "<<") && !strings.HasPrefix(s[i:], "<<<"):
			spec, n := readHeredocSpec(s[i+2:])
			i += 1 + n
			if spec.delim != "" {
				pending = append(pending, spec)
			}
		case c == '#' && (i == 0 || strings.IndexByte(" \t\n;", s[i-1]) >= 0):
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j - 1
			} else {
				i = len(s)
			}
		case c == '\n':
			if len(pending) > 0 {
				pos := i + 1
				for k, h := range pending {
					_, n := readHeredocBody(s[pos:], h)
					pos += n
					if k < len(pending)-1 && pos < len(s) && s[pos] == '\n' {
						pos++
					}
				}
				pending = nil
				i = pos - 1 // the newline after the last delimiter ends the statement
				continue
			}
			if !strings.HasSuffix(strings.TrimRight(s[start:i], " \t\r"), "|") {
				cut(i)
			}
			atStart = true
		case c == ';' || c == '|' || c == '&':
			next := byte(0)
			if i+1 < len(s) {
				next = s[i+1]
			}
			switch {
			case c == '|' && next == '|', c == '&' && next == '&', c == ';' && next == ';':
				cut(i)
				i++
				if depth == 0 {
					start = i + 1
				}
			case c == '|':
				// a pipe: same statement
			case c == '&' && (next == '>' || i > 0 && (s[i-1] == '>' || s[i-1] == '<')):
				// &> and >&: redirections
				continue
			default:
				cut(i)
			}
			atStart = true
		case c == ' ' || c == '\t' || c == '\r':
		default:
			if atStart {
				j := i
				for j < len(s) && strings.IndexByte(" \t\r\n;|&()<>", s[j]) < 0 {
					j++
				}
				switch s[i:j] {
				case "if", "while", "until", "for", "case", "select", "{":
					depth++
				case "fi", "done", "esac", "}":
					if depth > 0 {
						depth--
					}
				case "then", "do", "else", "elif", "!", "time":
				default:
					// An ordinary word: not consumed here, so a quote or $( in
					// it is still read by the cases above.
					atStart = false
					continue
				}
				switch s[i:j] {
				case "for", "case", "select", "fi", "done", "esac", "}":
					atStart = false
				}
				i = j - 1
			}
		}
	}
	depth = 0
	cut(len(s))
	return out
}

// flow follows secrets through a command's statements: a variable assigned
// from a secret, a file a secret is written to, a variable exported for a
// later program to read.
type flow struct {
	d        Decoys
	taint    map[string][]secretRef // variable -> the secrets it holds
	exported map[string]bool
	files    []taintedFile
	lits     map[string][]string // NAME=literal assignments and for lists, to resolve hosts like https://$h/x
}

type taintedFile struct {
	path string
	refs []secretRef
}

func newFlow(scan string, d Decoys) *flow {
	return &flow{d: d, taint: map[string][]secretRef{}, exported: map[string]bool{}, lits: shellVars(scan)}
}

var forRe = regexp.MustCompile(`\bfor\s+([A-Za-z_][A-Za-z0-9_]*)\s+in\s+([^;\n]*)`)

// shellVars: variables a command sets to literals (NAME=value, export
// NAME=value, for NAME in a b c), for resolving names and hosts.
func shellVars(scan string) map[string][]string {
	vars := map[string][]string{}
	for _, c := range parse(scan, 0) {
		for _, w := range assignmentWords(c.words) {
			name, val, _ := strings.Cut(w, "=")
			if val != "" && !strings.Contains(val, "$(") && !strings.ContainsAny(val, "`\"' \t\n") {
				vars[name] = []string{val}
			}
		}
	}
	for _, m := range forRe.FindAllStringSubmatch(scan, -1) {
		var vs []string
		for _, w := range strings.Fields(m[2]) {
			w = strings.Trim(w, `"'`)
			if w != "" && !strings.Contains(w, "$(") && !strings.ContainsAny(w, "`*?[") {
				vs = append(vs, w)
			}
		}
		if len(vs) > 0 {
			vars[m[1]] = vs
		}
	}
	return vars
}

// assignmentWords: NAME=value words of a simple command, as prefixes or
// as the arguments of export/local/declare/readonly/typeset.
func assignmentWords(words []string) []string {
	var out []string
	i := 0
	for i < len(words) && isAssignment(words[i]) {
		out = append(out, words[i])
		i++
	}
	if i < len(words) && declarers[progName(words[i])] {
		for _, w := range words[i+1:] {
			if isAssignment(w) {
				out = append(out, w)
			}
		}
	}
	return out
}

var declarers = map[string]bool{"export": true, "local": true, "declare": true, "typeset": true, "readonly": true}

// refsIn: the secrets a piece of text uses, with variables and files that
// earlier statements filled from a secret replaced by that secret.
func (fl *flow) refsIn(text string) []secretRef {
	var out []secretRef
	for _, r := range findSecrets(text, fl.d) {
		if r.kind == refVar && fl.taint[strings.TrimPrefix(r.label, "$")] != nil {
			continue
		}
		out = append(out, r)
	}
	seen := map[string]bool{}
	for _, m := range envRefRe.FindAllStringSubmatch(text, -1) {
		n := firstSet(m[1:]...)
		if rs := fl.taint[n]; rs != nil && !seen[n] && n != "" {
			seen[n] = true
			out = append(out, rs...)
		}
	}
	for _, f := range fl.files {
		if strings.Contains(text, f.path) {
			out = append(out, f.refs...)
		}
	}
	return out
}

// envReaders: refs of exported tainted variables a statement's programs can
// read from their environment (interpreter code, gh's token variables).
func (fl *flow) envReaders(sinks []string) []secretRef {
	var out []secretRef
	for n := range fl.exported {
		rs := fl.taint[n]
		if rs == nil {
			continue
		}
		for _, s := range sinks {
			if strings.Contains(s, "(") || strings.HasPrefix(s, "gh ") && ghTokenVars[n] {
				out = append(out, rs...)
				break
			}
		}
	}
	return out
}

var ghTokenVars = map[string]bool{"GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_ENTERPRISE_TOKEN": true, "GITHUB_ENTERPRISE_TOKEN": true}

var (
	redirRe = regexp.MustCompile(`(?:^|[^0-9&<>])>>?\s*([^\s|;&<>()]+)|\btee\s+(?:-a\s+)?([^\s|;&<>()]+)`)
	nameOK  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// record notes what a statement fills from a secret: its assignments, its
// exports and the files it writes.
func (fl *flow) record(st string, refs []secretRef) {
	for _, c := range parse(st, 0) {
		for _, w := range assignmentWords(c.words) {
			name, val, _ := strings.Cut(w, "=")
			if rs := fl.refsIn(val); len(rs) > 0 && !answerOnly(val) {
				fl.taint[name] = rs
			} else {
				delete(fl.taint, name)
			}
		}
		i := 0
		for i < len(c.words) && isAssignment(c.words[i]) {
			i++
		}
		if i < len(c.words) && (progName(c.words[i]) == "export" ||
			progName(c.words[i]) == "declare" && hasWord(c.words[i+1:], "-x")) {
			for _, w := range c.words[i+1:] {
				name, _, _ := strings.Cut(w, "=")
				if nameOK.MatchString(name) {
					fl.exported[name] = true
				}
			}
		}
	}
	if len(refs) == 0 || answerOnly("$("+st+")") {
		return // a file a network or DB client writes holds its answer
	}
	for _, m := range redirRe.FindAllStringSubmatch(st, -1) {
		p := strings.Trim(firstSet(m[1:]...), `"'`)
		if len(p) < 3 || strings.HasPrefix(p, "/dev/") {
			continue
		}
		fl.files = append(fl.files, taintedFile{p, refs})
	}
}

// answerOnly: a value captured from a program that uses the secret to ask
// something (X=$(curl -H "$K" ...), X=$(psql "$DB_URL" -c ...)): the
// variable holds the answer, not the secret. ssh brings its remote output
// back, so it is not one.
func answerOnly(val string) bool {
	if !strings.Contains(val, "$(") && !strings.Contains(val, "`") {
		return false
	}
	sinks, _, _ := findSinks(val, nil)
	for _, s := range sinks {
		if s != "ssh" {
			return true
		}
	}
	return dbClientRe.MatchString(val)
}

var dbClientRe = regexp.MustCompile(`\b(?:psql|pg_dump|pgcli|mysql|redis-cli|mongosh|sqlite3)\b`)

// ghContent: a secret written into a gh command's arguments or input (an
// issue or PR body, a comment, gh api fields) is published, not used to
// sign in, so github.com is no excuse. -H values (an auth header) and
// gh secret/variable set are not content.
func (fl *flow) ghContent(st string, stmtRefs []secretRef) bool {
	for _, c := range parse(st, 0) {
		name, rest := c.name()
		if name != "gh" {
			continue
		}
		if sink, _ := ghSink(rest); sink == "" || sink == "gh secret set" || sink == "gh variable set" {
			continue
		}
		var words []string
		stdin := false
		for i := 0; i < len(rest); i++ {
			w := rest[i]
			if w == "-H" || w == "--header" {
				i++
				continue
			}
			if w == "-" || strings.HasSuffix(w, "@-") || strings.HasSuffix(w, "=-") {
				stdin = true
			}
			words = append(words, w)
		}
		if len(fl.refsIn(strings.Join(words, " "))) > 0 {
			return true
		}
		if stdin && len(stmtRefs) > 0 {
			return true
		}
	}
	return false
}

// dbDriverRe: code or a client that uses a database URL to reach the
// database (not an HTTP request).
var dbDriverRe = regexp.MustCompile(`(?i)asyncpg|psycopg|sqlalchemy|create_engine|pg8000|pymysql|mysql\.connector|sqlite3|redis\.(?:from_url|redis|strictredis)|\bpg\.(?:client|pool)|\bpostgres\s*\(|prisma|knex|drizzle|\b(?:psql|pg_dump|pg_restore|pgcli|mysql|redis-cli|mongosh|mongo)\b`)

// dbRefsOK drops database URLs that go to a database: not named in a
// network program's own arguments, and (for code) used by a DB driver.
func (fl *flow) dbRefsOK(st string, refs []secretRef, sinks []string) []secretRef {
	hasDB := false
	for _, r := range refs {
		hasDB = hasDB || r.family == "db"
	}
	if !hasDB {
		return refs
	}
	var netWords []string
	for _, c := range parse(st, 0) {
		if name, rest := c.name(); netPrograms[name] || name == "gh" {
			netWords = append(netWords, rest...)
		}
	}
	inNet := false
	for _, r := range fl.refsIn(strings.Join(netWords, " ")) {
		inNet = inNet || r.family == "db"
	}
	code := false
	for _, s := range sinks {
		code = code || strings.Contains(s, "(")
	}
	if inNet || code && !dbDriverRe.MatchString(st) {
		return refs
	}
	var out []secretRef
	for _, r := range refs {
		if r.family != "db" {
			out = append(out, r)
		}
	}
	return out
}
