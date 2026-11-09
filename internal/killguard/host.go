package killguard

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// protectedComms are programs whose death stops every session on a host.
var protectedComms = map[string]string{
	"systemd":       "a systemd manager (systemd --user, or init)",
	"init":          "init",
	"launchd":       "launchd",
	"pyred":         "a pyred process (the node daemon, its sentinel, or a session's Pyre tools)",
	"pyre-sentinel": "the Pyre sentinel",
	"pyre-hub":      "the Pyre hub",
	"tmux: server":  "a tmux server (every pane in it)",
}

// Protect picks the protected processes of a table: PID 1, the programs in
// protectedComms, and every ancestor of self (the harness and the tmux
// server that run this command's shell).
func Protect(procs []Proc, self int) map[int]Protected {
	by := make(map[int]Proc, len(procs))
	for _, p := range procs {
		by[p.PID] = p
	}
	out := map[int]Protected{1: {Proc: Proc{PID: 1, Comm: by[1].Comm, Cmdline: by[1].Cmdline}, Label: "PID 1 (init)"}}
	for _, p := range procs {
		if l, ok := protectedComms[p.Comm]; ok && p.PID != 1 {
			if p.Comm == "systemd" {
				l = "systemd --user (the user's service manager: pyred, the sentinel and every session run under it)"
			}
			out[p.PID] = Protected{Proc: p, Label: l}
		}
	}
	seen := map[int]bool{}
	for pid := by[self].PPID; pid > 1 && !seen[pid]; pid = by[pid].PPID {
		seen[pid] = true
		p, ok := by[pid]
		if !ok {
			break
		}
		if _, done := out[pid]; !done {
			out[pid] = Protected{Proc: p, Label: p.Comm + ", which runs this session (an ancestor of its shell)"}
		}
	}
	return out
}

// LoadHost reads the process table: /proc on Linux, one ps call elsewhere
// (bounded to 2s). Command lines are read only for protected processes.
// Any failure leaves only PID 1 protected.
func LoadHost() Host {
	self := os.Getpid()
	var procs []Proc
	if runtime.GOOS == "linux" {
		procs = readProc()
	} else if runtime.GOOS != "windows" {
		procs = readPS()
	}
	prot := Protect(procs, self)
	if runtime.GOOS == "linux" {
		for pid, p := range prot {
			if p.Cmdline == "" {
				b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
				p.Cmdline = strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
				prot[pid] = p
			}
		}
	}
	return Host{Protected: prot}
}

func readProc() []Proc {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]Proc, 0, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		if p, ok := parseStat(pid, string(b)); ok {
			out = append(out, p)
		}
	}
	return out
}

// parseStat reads "pid (comm) state ppid ..." (comm may hold spaces and
// parentheses; it ends at the last ')').
func parseStat(pid int, s string) (Proc, bool) {
	i, j := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if i < 0 || j < i {
		return Proc{}, false
	}
	f := strings.Fields(s[j+1:])
	if len(f) < 2 {
		return Proc{}, false
	}
	ppid, _ := strconv.Atoi(f[1])
	return Proc{PID: pid, PPID: ppid, Comm: s[i+1 : j]}, true
}

func readPS() []Proc {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,args=").Output()
	if err != nil {
		return nil
	}
	var out []Proc
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		args := strings.Join(f[2:], " ")
		comm := filepath.Base(f[2])
		out = append(out, Proc{PID: pid, PPID: ppid, Comm: comm, Cmdline: args})
	}
	return out
}
