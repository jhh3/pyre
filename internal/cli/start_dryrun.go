package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/envfile"
	"github.com/jhh3/pyre/internal/harnessargs"
)

// StartPlan is `pyre start --dry-run` output: what the start would send,
// checked locally (the node checks the harness arguments again).
type StartPlan struct {
	Harness      string   `json:"harness"`
	Node         string   `json:"node,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
	Permissions  string   `json:"permissions,omitempty"`
	NativeResume string   `json:"native_resume,omitempty"`
	HarnessArgs  []string `json:"harness_args"` // canonical "--flag=value"
	// EnvFile is the node path sent; EnvFileHere is its effect when a file
	// at that path is readable on this machine (values redacted:
	// KEY=<redacted>, KEY= or KEY (unset)), EnvFileNote why it is not.
	EnvFile     string   `json:"env_file,omitempty"`
	EnvFileHere []string `json:"env_file_here,omitempty"`
	EnvFileNote string   `json:"env_file_note,omitempty"`
	// EnvFileWarnings are variables in the file that change how Pyre
	// reaches the session (Claude's monitor blockers).
	EnvFileWarnings []string `json:"env_file_warnings,omitempty"`
	// Argv is the Claude command line the node's adapter builds, with
	// run-specific paths and the agent guide as placeholders (adapter
	// order: internal/runtime/claude Launch).
	Argv []string `json:"argv,omitempty"`
}

func (a *App) startDryRun(sp StartParams) error {
	if !validHarness(sp.Harness) {
		return usagef("--harness must be claude, codex or shell (got %q)", sp.Harness)
	}
	hargs, err := startHarnessArgs(sp)
	if err != nil {
		return err
	}
	if sp.NativeResume != "" && (c.Harness(sp.Harness) != c.HarnessClaude || !nativeSessionRe.MatchString(strings.TrimSpace(sp.NativeResume))) {
		return usagef("--native-resume takes a Claude session id (a UUID) with --harness claude")
	}
	plan := StartPlan{Harness: sp.Harness, Node: sp.Node, Cwd: sp.Cwd, Permissions: sp.Permissions, NativeResume: strings.TrimSpace(sp.NativeResume),
		HarnessArgs: hargs}
	if plan.HarnessArgs == nil {
		plan.HarnessArgs = []string{}
	}
	if _, err := envFileArgs(sp); err != nil {
		return err
	}
	if plan.EnvFile = strings.TrimSpace(sp.EnvFile); plan.EnvFile != "" {
		// The node reads its own copy at launch; a copy here (the node is
		// this machine, or a shared path) is checked the same way.
		if !filepath.IsAbs(plan.EnvFile) {
			// A path for a node on another OS (C:\... checked from Unix).
			plan.EnvFileNote = "a path for another OS; the node reads and checks it at launch"
		} else if v, err := envfile.Read(plan.EnvFile); err == nil {
			plan.EnvFileHere = v.Redacted()
			if plan.EnvFileHere == nil {
				plan.EnvFileHere = []string{}
			}
			if c.Harness(sp.Harness) == c.HarnessClaude {
				for _, k := range c.ClaudeMonitorBlockers {
					if _, set := v.Set[k]; set {
						plan.EnvFileWarnings = append(plan.EnvFileWarnings, k+" turns off Claude's plugin monitors: queued Pyre mail waits for a turn end or paste")
					}
				}
			}
		} else if errors.Is(err, fs.ErrNotExist) {
			plan.EnvFileNote = "not on this machine; the node reads and checks it at launch"
		} else {
			return usagef("%v", err)
		}
	}
	if c.Harness(sp.Harness) == c.HarnessClaude {
		plan.Argv = claudePlanArgv(plan, sp.Prompt != "")
	}
	return a.emit(plan, func(w io.Writer) {
		fmt.Fprintf(w, "harness:       %s\n", plan.Harness)
		if plan.Node != "" {
			fmt.Fprintf(w, "node:          %s\n", plan.Node)
		}
		if plan.Cwd != "" {
			fmt.Fprintf(w, "cwd:           %s\n", plan.Cwd)
		}
		if plan.NativeResume != "" {
			fmt.Fprintf(w, "continues:     claude conversation %s (native resume)\n", plan.NativeResume)
		}
		fmt.Fprintf(w, "harness args:  %s\n", orDash(harnessargs.String(plan.HarnessArgs)))
		if plan.EnvFile != "" {
			if plan.EnvFileNote != "" {
				fmt.Fprintf(w, "env file:      %s (%s)\n", plan.EnvFile, plan.EnvFileNote)
			} else {
				fmt.Fprintf(w, "env file:      %s (values redacted; the node reads its copy at every launch)\n", plan.EnvFile)
				for _, kv := range plan.EnvFileHere {
					fmt.Fprintf(w, "  %s\n", kv)
				}
				for _, warn := range plan.EnvFileWarnings {
					fmt.Fprintf(w, "  warning: %s\n", warn)
				}
			}
		}
		if len(plan.Argv) > 0 {
			fmt.Fprintf(w, "launch:        %s\n", harnessargs.String(plan.Argv))
		}
		fmt.Fprintln(w, "(dry run: nothing started; the node checks the arguments again)")
	})
}

// claudePlanArgv mirrors the Claude adapter's argv order with placeholders
// for what only the node knows (run dir, session id, guide text).
func claudePlanArgv(p StartPlan, prompt bool) []string {
	argv := []string{"claude", "--settings", "<run>/pyre-hooks.json"}
	if p.NativeResume != "" {
		argv = append(argv, "--resume", p.NativeResume)
	} else {
		argv = append(argv, "--session-id", "<new uuid>")
	}
	switch p.Permissions {
	case "bypass":
		argv = append(argv, "--dangerously-skip-permissions")
	case "ask":
		argv = append(argv, "--permission-mode", "default")
	default:
		argv = append(argv, "<permissions: the node's pyred --permissions>")
	}
	guide := "<pyre agent guide>"
	var rest []string
	for _, a := range p.HarnessArgs {
		if v, ok := strings.CutPrefix(a, "--append-system-prompt="); ok {
			guide += "\n\n" + v // joined into the one flag Claude keeps
			continue
		}
		rest = append(rest, a)
	}
	argv = append(argv, "--append-system-prompt", guide, "--plugin-dir=<run>/pyre-plugin")
	argv = append(argv, rest...)
	argv = append(argv, "--mcp-config=<run>/pyre-mcp.json")
	if prompt {
		argv = append(argv, "<prompt>")
	}
	return argv
}
