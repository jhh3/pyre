package repoindex

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Choosing what a quick launch means, shared by `pyre new`, MCP
// start_session and the TUI quick-launch bar.

// Contenders keeps the groups that match about as well as the best one
// (within 60% of its score): weak, incidental matches never make a
// choice ambiguous.
func Contenders(groups [][]contracts.RepoEntry) [][]contracts.RepoEntry {
	if len(groups) == 0 {
		return groups
	}
	top := groups[0][0].Score
	out := groups[:0:0]
	for _, g := range groups {
		if g[0].Score*10 >= top*6 {
			out = append(out, g)
		}
	}
	return out
}

// PickGroup resolves a spec without asking: a single contender, or a
// unique exact name match. ok is false when the choice is ambiguous.
func PickGroup(spec string, groups [][]contracts.RepoEntry) (int, bool) {
	if len(groups) == 1 {
		return 0, true
	}
	exact := -1
	for i, g := range groups {
		if strings.EqualFold(g[0].Name, spec) {
			if exact >= 0 {
				return 0, false
			}
			exact = i
		}
	}
	return exact, exact >= 0
}

// ErrNoCommonMachine: the chosen repositories are not all on one machine.
var ErrNoCommonMachine = errors.New("no single machine has all of these repositories")

// ErrNotOnMachine: a requested machine lacks a chosen repository.
var ErrNotOnMachine = errors.New("the repository is not on that machine")

// Machines lists the machines holding every chosen repository, best
// first: connected, then most recently used there, then most recently
// active.
func Machines(chosen [][]contracts.RepoEntry, connected map[string]bool) []string {
	type cand struct {
		id           string
		used, active time.Time
	}
	count := map[string]int{}
	cs := map[string]*cand{}
	for _, g := range chosen {
		seen := map[string]bool{}
		for _, e := range g {
			if seen[e.NodeID] {
				continue
			}
			seen[e.NodeID] = true
			count[e.NodeID]++
			cd := cs[e.NodeID]
			if cd == nil {
				cd = &cand{id: e.NodeID}
				cs[e.NodeID] = cd
			}
			if e.LastUsedAt != nil && e.LastUsedAt.After(cd.used) {
				cd.used = *e.LastUsedAt
			}
			if e.LastActivity.After(cd.active) {
				cd.active = e.LastActivity
			}
		}
	}
	var all []*cand
	for id, cd := range cs {
		if count[id] == len(chosen) {
			all = append(all, cd)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if connected[a.id] != connected[b.id] {
			return connected[a.id]
		}
		if !a.used.Equal(b.used) {
			return a.used.After(b.used)
		}
		if !a.active.Equal(b.active) {
			return a.active.After(b.active)
		}
		return a.id < b.id
	})
	out := make([]string, len(all))
	for i, c := range all {
		out[i] = c.id
	}
	return out
}

// ChooseMachine picks the machine for the chosen repositories: want if it
// holds them all, else the best of Machines.
func ChooseMachine(chosen [][]contracts.RepoEntry, connected map[string]bool, want string) (string, error) {
	ms := Machines(chosen, connected)
	if len(ms) == 0 {
		return "", ErrNoCommonMachine
	}
	if want == "" {
		return ms[0], nil
	}
	for _, m := range ms {
		if m == want {
			return want, nil
		}
	}
	return "", ErrNotOnMachine
}

// EntryOn returns the group's entry on a machine.
func EntryOn(g []contracts.RepoEntry, nodeID string) (contracts.RepoEntry, bool) {
	for _, e := range g {
		if e.NodeID == nodeID {
			return e, true
		}
	}
	return contracts.RepoEntry{}, false
}

// ParseLine splits a quick-launch line `<repo>[+<repo>…] [@machine]
// [harness] [prompt…]`: @machine and the harness may follow the
// repositories in either order; everything after them is the prompt.
func ParseLine(fields []string) (repos []string, node, harness, prompt string) {
	if len(fields) == 0 {
		return nil, "", "", ""
	}
	for _, r := range strings.Split(fields[0], "+") {
		repos = append(repos, strings.TrimSpace(r))
	}
	i := 1
	for ; i < len(fields) && i <= 2; i++ {
		a := fields[i]
		lower := strings.ToLower(a)
		if strings.HasPrefix(a, "@") && node == "" && len(a) > 1 {
			node = a[1:]
			continue
		}
		if harness == "" && (lower == "claude" || lower == "codex" || lower == "agy" || lower == "shell") {
			harness = lower
			continue
		}
		break
	}
	prompt = strings.TrimSpace(strings.Join(fields[i:], " "))
	return repos, node, harness, prompt
}
