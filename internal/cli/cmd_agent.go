package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/internal/agentguide"
)

// GuideResult is `pyre guide --json`.
type GuideResult struct {
	Text string `json:"text"`
}

func (a *App) addAgentCommands(root *cobra.Command) {
	var gProject string
	var gMCP bool
	guide := &cobra.Command{
		Use:   "guide [--project P]",
		Short: "Print the short working guide given to agent sessions",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			opts := agentguide.Options{MCP: gMCP, ParentSessionID: a.Getenv(agentguide.EnvParentSessionID),
				ReportsToParent: a.Getenv(agentguide.EnvReportToParent) == "1"}
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
				ParentSessionID: a.Getenv(agentguide.EnvParentSessionID), ReportsToParent: a.Getenv(agentguide.EnvReportToParent) == "1"})}
			return srv.Serve(cmd.Context(), a.Stdin, a.Stdout)
		}),
	}
	mcp.Flags().BoolVar(&listTools, "list-tools", false, "print the tool manifest as JSON and exit")
	root.AddCommand(mcp)
}
