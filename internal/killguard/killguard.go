// Package killguard refuses agent shell commands that would signal the
// processes every session on a machine depends on: PID 1, a systemd user
// manager (systemd --user), pyred and its sentinel, pyre-hub, a tmux server,
// or the harness running the command. It also refuses a kill whose target
// was looked up through a parent PID ($PPID, ps -o ppid=, /proc/N/stat) or
// an unfiltered ps listing, and systemctl/loginctl verbs that stop Pyre's
// units or the user manager (iss_…: a loop's parent
// PID, read after the loop was reparented, was systemd --user; killing it
// took down pyred, the sentinel and every session on the node).
//
// Check runs inside the harness PreToolUse hook, so it is cheap: a line
// with no kill/pkill/killall/systemctl/loginctl word returns at once, and
// the process table is read only when a signal command needs it. It is a
// guard against honest mistakes, not a sandbox: a signal sent from a
// script file, an interpreter (os.kill) or a remote shell is not seen.
package killguard

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/jhh3/pyre/internal/secretegress"
)

// Proc is one process of the host table.
type Proc struct {
	PID, PPID int
	Comm      string // base name of the program (Linux: /proc/N/comm)
	Cmdline   string // argv joined with spaces; filled for protected processes
}

// Host is what Check matches targets against.
type Host struct {
	// Protected maps a PID to the process it is, with a short label.
	Protected map[int]Protected
}

// Protected is a process whose signal Check refuses.
type Protected struct {
	Proc
	Label string
}

// Block is a refused command; Reason says why and what to do instead.
type Block struct{ Reason string }

const advice = " Stop a job by the systemd unit you started it in (systemctl --user stop <unit>) or by the PID your own launch recorded ($! written to a file); never signal a PID looked up with ps, pgrep or a parent PID: after reparenting, a process's parent is systemd --user or init."

var quick = regexp.MustCompile(`kill|systemctl|loginctl`)

// Check returns a Block when line sends a signal Pyre refuses, else nil.
// load is called at most once, only when a target must be resolved.
func Check(line string, load func() Host) *Block {
	if !quick.MatchString(line) {
		return nil
	}
	cmds := secretegress.Commands(line)
	var host *Host
	get := func() *Host {
		if host == nil {
			h := load()
			host = &h
		}
		return host
	}
	for _, c := range cmds {
		var b *Block
		switch c.Name {
		case "kill":
			b = checkKill(c, cmds, line, get)
		case "pkill", "killall":
			b = checkPkill(c, get)
		case "systemctl":
			b = checkSystemctl(c)
		case "loginctl":
			b = checkLoginctl(c)
		}
		if b != nil {
			return b
		}
	}
	return nil
}

func block(format string, a ...any) *Block {
	return &Block{Reason: "Pyre refused this command: " + fmt.Sprintf(format, a...) + "." + advice}
}

// --- kill ---------------------------------------------------------------

var (
	ppidRe = regexp.MustCompile(`(?i)ppid|/proc/[^ ]*/stat`)
	pidRe  = regexp.MustCompile(`^-?[0-9]+$`)
)

func checkKill(c secretegress.Cmd, all []secretegress.Cmd, line string, get func() *Host) *Block {
	targets, list := killTargets(c.Args)
	if list {
		return nil // kill -l / -L lists signal names
	}
	viaXargs := len(c.Words) > 0 && progBase(c.Words[0]) == "xargs" || containsWord(c.Words, "xargs")
	unresolved := viaXargs || len(targets) == 0
	for _, t := range targets {
		switch {
		case t == "$PPID" || t == "${PPID}":
			return block("it signals $PPID, the parent of this shell (the harness, or systemd --user once reparented)")
		case pidRe.MatchString(t):
			if b := checkPID(t, get); b != nil {
				return b
			}
		case strings.HasPrefix(t, "%"):
			// a job of this shell
		default:
			unresolved = true
		}
	}
	if !unresolved {
		return nil
	}
	// The target is computed (a $(...) substitution, a variable, xargs):
	// look at how the line finds PIDs.
	if ppidRe.MatchString(line) || strings.Contains(line, "$PPID") || strings.Contains(line, "${PPID}") {
		return block("its target PID comes from a parent-PID lookup, which names systemd --user or init once the process was reparented")
	}
	return checkLookups(all, get)
}

// killTargets returns kill's operands; list is true for kill -l/-L.
func killTargets(args []string) (targets []string, list bool) {
	sig := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return append(targets, args[i+1:]...), false
		case a == "-l" || a == "-L" || a == "--list" || a == "--table":
			return nil, true
		case a == "-s" || a == "-n" || a == "--signal" || a == "-q" || a == "--queue":
			i++
			sig = true
		case a == "--timeout":
			i += 2
		case strings.HasPrefix(a, "--"):
			// --signal=X, --verbose, ...
		case strings.HasPrefix(a, "-") && !sig:
			sig = true // -9, -KILL, -SIGTERM: the signal
		default:
			targets = append(targets, a)
		}
	}
	return targets, false
}

func checkPID(t string, get func() *Host) *Block {
	n, err := strconv.Atoi(t)
	if err != nil {
		return nil
	}
	switch {
	case n == -1:
		return block("kill -1 signals every process you own, including pyred and every session")
	case n == 0:
		return nil // this shell's own process group
	case n < 0:
		if p, ok := get().Protected[-n]; ok {
			return block("it signals process group %d, led by %s", -n, p.Label)
		}
	default:
		if p, ok := get().Protected[n]; ok {
			return block("PID %d is %s; signalling it stops every session on this machine", n, p.Label)
		}
	}
	return nil
}

// checkLookups refuses a computed kill target when the line finds PIDs in
// a way that can name a protected process: pgrep/pidof patterns that match
// one, ps output filtered by grep patterns that match one, or ps output
// with no filter at all.
func checkLookups(all []secretegress.Cmd, get func() *Host) *Block {
	var ps bool
	var greps []string
	for _, c := range all {
		switch c.Name {
		case "pgrep":
			if b := checkPkill(c, get); b != nil {
				return b
			}
		case "pidof":
			for _, a := range c.Args {
				if !strings.HasPrefix(a, "-") {
					for _, p := range get().Protected {
						if a == p.Comm || a == progBase(firstWord(p.Cmdline)) {
							return block("pidof %s finds %s (PID %d)", a, p.Label, p.PID)
						}
					}
				}
			}
		case "ps":
			ps = true
		case "grep", "egrep", "rg":
			if p := grepPattern(c.Args); p != "" {
				greps = append(greps, p)
			}
		}
	}
	if !ps {
		return nil
	}
	if len(greps) == 0 {
		return block("its target PIDs come from ps output, which can name systemd --user, init or pyred")
	}
	for _, g := range greps {
		re := compile(g, false)
		for _, p := range get().Protected {
			if re(p.Comm) || re(p.Cmdline) {
				return block("its target PIDs come from ps output filtered by %q, which matches %s (PID %d)", g, p.Label, p.PID)
			}
		}
	}
	return nil
}

func grepPattern(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-e" || a == "--regexp":
			if i+1 < len(args) {
				return args[i+1]
			}
		case a == "-v" || a == "--invert-match":
			return "" // an inverted filter keeps everything else
		case a == "-m" || a == "-A" || a == "-B" || a == "-C" || a == "-f":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			return a
		}
	}
	return ""
}

// --- pkill / pgrep / killall -------------------------------------------

func checkPkill(c secretegress.Cmd, get func() *Host) *Block {
	full, exact, regex, userOnly := false, false, c.Name != "killall", false
	var pattern []string
	var parents, groups []string
	for i := 0; i < len(c.Args); i++ {
		a := c.Args[i]
		val := func() string {
			if j := strings.IndexByte(a, '='); j > 0 && strings.HasPrefix(a, "--") {
				return a[j+1:]
			}
			if i+1 < len(c.Args) {
				i++
				return c.Args[i]
			}
			return ""
		}
		switch {
		case a == "-f" || a == "--full":
			full = true
		case a == "-x" || a == "--exact" || a == "-e" && c.Name == "killall":
			exact = true
		case a == "-r" || a == "--regexp":
			regex = true
		case a == "-P" || strings.HasPrefix(a, "--parent"):
			parents = append(parents, strings.Split(val(), ",")...)
		case a == "-g" || a == "-s" && c.Name != "killall" || strings.HasPrefix(a, "--pgroup") || strings.HasPrefix(a, "--session"):
			groups = append(groups, strings.Split(val(), ",")...)
		case a == "-u" || a == "-U" || a == "-G" || a == "--user" || strings.HasPrefix(a, "--euid") || strings.HasPrefix(a, "--uid") || strings.HasPrefix(a, "--group"):
			val()
			userOnly = true
		case a == "-t" || a == "-F" || a == "--pidfile" || a == "--ns" || a == "--nslist" || a == "-o" && c.Name == "killall" || a == "-y" || a == "--signal" || a == "-s" || a == "--cgroup" || a == "-Z":
			val()
		case strings.HasPrefix(a, "-"):
		default:
			pattern = append(pattern, a)
		}
	}
	verb := c.Name
	for _, p := range parents {
		if p == "$PPID" || p == "${PPID}" {
			return block("%s -P $PPID signals the harness's other children", verb)
		}
		if n, err := strconv.Atoi(p); err == nil {
			if pr, ok := get().Protected[n]; ok {
				return block("%s -P %d signals the children of %s, which are Pyre's services and sessions", verb, n, pr.Label)
			}
		}
	}
	for _, g := range groups {
		if n, err := strconv.Atoi(g); err == nil {
			if pr, ok := get().Protected[n]; ok {
				return block("%s signals process group/session %d of %s", verb, n, pr.Label)
			}
		}
	}
	if len(pattern) == 0 {
		if userOnly {
			return block("%s with only a user selects every process you own, including pyred and every session", verb)
		}
		return nil
	}
	if c.Name == "pgrep" && len(parents) > 0 {
		return nil
	}
	for _, pat := range pattern {
		var match func(string) bool
		if c.Name == "killall" && !regex {
			match = func(s string) bool { return s == pat }
		} else {
			match = compile(pat, exact)
		}
		for _, p := range get().Protected {
			hit := match(p.Comm)
			if c.Name == "killall" && !hit {
				hit = match(progBase(firstWord(p.Cmdline)))
			}
			if full && !hit {
				hit = match(p.Cmdline)
			}
			if hit {
				return block("%s %q matches %s (PID %d)", verb, pat, p.Label, p.PID)
			}
		}
	}
	return nil
}

// compile turns a pgrep/grep pattern into a matcher; a pattern Go cannot
// compile is matched as a substring.
func compile(pat string, exact bool) func(string) bool {
	expr := pat
	if exact {
		expr = "^(?:" + pat + ")$"
	}
	if re, err := regexp.Compile(expr); err == nil {
		return func(s string) bool { return s != "" && re.MatchString(s) }
	}
	return func(s string) bool { return s != "" && strings.Contains(s, pat) }
}

// --- systemctl / loginctl ----------------------------------------------

// systemctl options that take a value as the next word.
var systemctlValue = map[string]bool{
	"-s": true, "--signal": true, "--kill-whom": true, "--kill-who": true, "--kill-value": true,
	"-H": true, "--host": true, "-M": true, "--machine": true, "-p": true, "--property": true,
	"-t": true, "--type": true, "--state": true, "-n": true, "--lines": true, "-o": true, "--output": true,
	"--job-mode": true, "--root": true, "--image": true, "--what": true, "--when": true, "--timestamp": true,
	"--message": true, "--reboot-argument": true, "--boot-loader-menu": true, "--boot-loader-entry": true,
	"--drop-in": true, "--preset-mode": true,
}

var stopVerbs = map[string]bool{"stop": true, "kill": true, "freeze": true}

var managerVerbs = map[string]bool{"exit": true, "halt": true, "poweroff": true, "reboot": true, "kexec": true, "soft-reboot": true}

func checkSystemctl(c secretegress.Cmd) *Block {
	user, now, remote := false, false, false
	var words []string
	for i := 0; i < len(c.Args); i++ {
		a := c.Args[i]
		name := a
		if j := strings.IndexByte(a, '='); j > 0 && strings.HasPrefix(a, "--") {
			name = a[:j]
		} else if systemctlValue[a] {
			i++
		}
		switch {
		case name == "--user":
			user = true
		case name == "--now":
			now = true
		case name == "-H" || name == "--host" || name == "-M" || name == "--machine":
			remote = true
		case strings.HasPrefix(a, "-"):
		default:
			words = append(words, a)
		}
	}
	if remote || len(words) == 0 {
		return nil
	}
	verb, units := words[0], words[1:]
	if user && managerVerbs[verb] {
		return block("systemctl --user %s stops the user manager, and with it pyred and every session", verb)
	}
	if !stopVerbs[verb] && !((verb == "mask" || verb == "disable") && now) {
		return nil
	}
	for _, u := range units {
		if label := protectedUnit(u); label != "" {
			return block("systemctl %s %s stops %s", verb, u, label)
		}
	}
	return nil
}

// protectedUnit names what stopping unit u takes down, or "".
func protectedUnit(u string) string {
	n := strings.ToLower(u)
	samples := []string{"pyred.service", "pyre-sentinel.service", "pyre-hub.service", "pyre.slice", "pyre-runs.slice",
		"pyre-tmux-0.scope", "pyre-run-0.scope", "user@1000.service", "init.scope", "app.slice", "user.slice", "-.slice"}
	if strings.ContainsAny(n, "*?[") {
		for _, s := range samples {
			if ok, _ := path.Match(n, s); ok {
				return "Pyre's units (it matches " + s + ")"
			}
			if ok, _ := path.Match(n, strings.TrimSuffix(s, ".service")); ok {
				return "Pyre's units (it matches " + s + ")"
			}
		}
		return ""
	}
	switch {
	case strings.HasPrefix(n, "pyre"):
		return "a Pyre unit (pyred, its sentinel, the hub, the tmux server or a run)"
	case strings.HasPrefix(n, "user@"), n == "init.scope", n == "app.slice", n == "user.slice", n == "-.slice":
		return "the user manager's units, and with them pyred and every session"
	}
	return ""
}

func checkLoginctl(c secretegress.Cmd) *Block {
	for _, a := range c.Args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "terminate-user", "kill-user", "terminate-session", "kill-session":
			return block("loginctl %s ends the user's processes, including pyred and every session", a)
		}
		return nil
	}
	return nil
}

// --- helpers -------------------------------------------------------------

func progBase(w string) string { return strings.ToLower(path.Base(w)) }

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i]
	}
	return s
}

func containsWord(ws []string, w string) bool {
	for _, x := range ws {
		if progBase(x) == w {
			return true
		}
	}
	return false
}
