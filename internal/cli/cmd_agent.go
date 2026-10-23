package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
)

// GuideResult is `pyre guide --json`.
type GuideResult struct {
	Text string `json:"text"`
}

// CommandDoc is `pyre commands <name> --json`: one agent slash command
// with its full SKILL.md.
type CommandDoc struct {
	claudeplugin.Skill
	Text string `json:"text"`
}

func (a *App) addAgentCommands(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use:   "commands [name]",
		Short: "List the agent slash commands (/kickoff, /ship, ...) every Pyre Claude session has",
		Long: `List the slash commands Pyre adds to every Claude session it starts: type
/<name> in the session. They are skills Pyre writes into the session's Claude
config dir (<config>/skills/<name>, marked .pyre-managed); your own skill with
the same name wins. With a name, print that command's full procedure (the
SKILL.md the agent follows).`,
		Args: cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				s, ok := claudeplugin.LookupSkill(args[0])
				if !ok {
					return usagef("no agent command %q (pyre commands lists them)", args[0])
				}
				return a.emit(CommandDoc{Skill: s, Text: s.Body}, func(w io.Writer) { fmt.Fprint(w, s.Body) })
			}
			skills := claudeplugin.Skills()
			return a.emit(skills, func(w io.Writer) {
				fmt.Fprintln(w, "Agent slash commands in every Claude session Pyre starts:")
				fmt.Fprintln(w)
				rows := make([][]string, 0, len(skills))
				for _, s := range skills {
					rows = append(rows, []string{"  " + s.Usage, s.Short})
				}
				table(w, "  USAGE\tDESCRIPTION", rows)
				fmt.Fprintln(w, "\n`pyre commands <name>` prints a command's full procedure.")
			})
		}),
	})

	var gProject string
	var gMCP bool
	guide := &cobra.Command{
		Use:   "guide [--project P]",
		Short: "Print the short working guide given to agent sessions",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			opts := agentguide.Options{MCP: gMCP, ParentSessionID: a.Getenv(agentguide.EnvParentSessionID),
				ReportsToParent: a.Getenv(agentguide.EnvReportToParent) == "1", WorktreeRoot: a.Getenv("PYRE_WORKTREE_ROOT"), RunTmpDir: a.runTmpDir(), Harness: a.guideHarness()}
			if gProject != "" {
				ops, err := a.connect()
				if err != nil {
					return err
				}
				p, err := ops.ResolveProject(cmd.Context(), gProject)
				if err != nil {
					return err
				}
				lc, err := ops.Context(cmd.Context(), ContextParams{Project: p.ProjectID, MaxBytes: agentguide.DefaultMaxBriefBytes})
				if err != nil {
					return err
				}
				opts.ProjectName, opts.ProjectBrief = p.Name, lc.Text
			}
			text := agentguide.Text(opts)
			return a.emit(GuideResult{Text: text}, func(w io.Writer) { fmt.Fprint(w, text) })
		}),
	}
	guide.Flags().StringVar(&gProject, "project", "", "append this project's bounded brief")
	guide.Flags().BoolVar(&gMCP, "mcp", false, "mention the MCP tool names")
	root.AddCommand(guide)

	var listTools bool
	mcp := &cobra.Command{
		Use:   "mcp",
		Short: "Run an MCP stdio server exposing Pyre operations as tools",
		Long: `Serve the Model Context Protocol over stdin/stdout. The tools call the same
operations as the CLI commands (same parameters, same semantics, same
connection and credential resolution). Diagnostics go to stderr.

Claude Code:  claude --mcp-config agents/tools/claude-mcp.json
Codex:        codex -c 'mcp_servers.pyre.command="pyre"' -c 'mcp_servers.pyre.args=["mcp"]'`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			tools := MCPTools()
			if listTools {
				enc := json.NewEncoder(a.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(tools)
			}
			srv := &MCPServer{Connect: a.connect, Tools: tools, Log: a.Stderr, Instructions: agentguide.Text(agentguide.Options{MCP: true,
				ParentSessionID: a.Getenv(agentguide.EnvParentSessionID), ReportsToParent: a.Getenv(agentguide.EnvReportToParent) == "1", WorktreeRoot: a.Getenv("PYRE_WORKTREE_ROOT"), RunTmpDir: a.runTmpDir(),
				Harness: a.guideHarness()})}
			return srv.Serve(cmd.Context(), a.Stdin, a.Stdout)
		}),
	}
	mcp.Flags().BoolVar(&listTools, "list-tools", false, "print the tool manifest as JSON and exit")
	root.AddCommand(mcp)
}

// guideHarness is the harness the guide is for (callerHarness; "" =
// unknown, the guide then covers both ways to wait).
func (a *App) guideHarness() string { return a.callerHarness() }

// runTmpDir reports whether pyred gave the calling run its own TMPDIR
// (<state>/tmp/runs/<run_id>).
func (a *App) runTmpDir() bool {
	run := a.Getenv("PYRE_RUN_ID")
	return run != "" && filepath.Base(a.Getenv("TMPDIR")) == run
}

// callerHarness is the calling session's harness: $PYRE_HARNESS, which
// pyred sets in every run. Outside a Pyre session ($PYRE_SESSION_ID unset)
// CLAUDECODE=1 means Claude Code; inside one without $PYRE_HARNESS (an
// older pyred) it is unknown, since CLAUDECODE may leak from whatever
// started pyred.
func (a *App) callerHarness() string {
	if h := a.Getenv(EnvHarness); h != "" {
		return h
	}
	if a.Getenv(EnvSessionID) == "" && a.Getenv("CLAUDECODE") == "1" {
		return "claude"
	}
	return ""
}
