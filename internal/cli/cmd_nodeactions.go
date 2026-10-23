package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/jhh3/pyre/integrations/hive/wsimport"
	c "github.com/jhh3/pyre/internal/contracts"
)

func (a *App) addNodeActionCommands(root *cobra.Command) {
	// ---- memory ----
	memory := &cobra.Command{Use: "memory", Short: "Shared memory (Hive) through a node", Args: cobra.NoArgs}
	var mp MemorySearchParams
	search := &cobra.Command{
		Use:   `search "query" [--node N] [--project P]`,
		Short: "Search shared memory; results carry provenance and freshness",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			p := mp
			p.Query = args[0]
			res, err := ops.MemorySearch(cmd.Context(), p)
			if err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				for _, h := range res.Hits {
					auth := ""
					if h.Authoritative {
						auth = " [authoritative]"
					}
					fmt.Fprintf(w, "%s  %s  %s%s\n  %s\n  source %s, %s\n", h.ID, h.Kind, h.Title, auth, clip(h.Snippet, 200), orDash(h.Source), ago(h.At))
				}
				if len(res.Hits) == 0 {
					fmt.Fprintln(w, "no matches")
				}
				fmt.Fprintf(w, "-- memory observed %s via %s", ago(res.Freshness.ObservedAt), orDash(res.Freshness.Source))
				if res.Freshness.Stale {
					fmt.Fprint(w, " (STALE)")
				}
				if res.Truncated {
					fmt.Fprint(w, ", truncated")
				}
				fmt.Fprintln(w)
			})
		}),
	}
	search.Flags().StringVar(&mp.Node, "node", "", "node whose Hive to query (default: this session's node)")
	search.Flags().StringVar(&mp.Project, "project", "", "restrict to a project")
	search.Flags().IntVar(&mp.Limit, "limit", 0, "max hits")
	memory.AddCommand(search)
	var msNode string
	status := &cobra.Command{
		Use:   "status [--node N]",
		Short: "Show the memory backend's sync/index freshness on a node",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			var st c.MemoryStatus
			if _, err := ops.NodeAction(cmd.Context(), msNode, c.ActionMemoryStatus, struct{}{}, &st, a.opID); err != nil {
				return err
			}
			return a.emit(st, func(w io.Writer) {
				fmt.Fprintf(w, "backend %s %s; last sync %s, last index %s, pending %d\n", st.Backend, orDash(st.Version), agoPtr(st.LastSyncAt), agoPtr(st.LastIndexAt), st.PendingItems)
				if st.Detail != "" {
					fmt.Fprintln(w, st.Detail)
				}
			})
		}),
	}
	status.Flags().StringVar(&msNode, "node", "", "node (default: this session's node)")
	memory.AddCommand(status)
	root.AddCommand(memory)

	// ---- accounts ----
	var provider string
	var watch bool
	var every time.Duration
	var refresh bool
	accounts := &cobra.Command{
		Use:     "accounts [ls|register|checkouts|checkin]",
		Aliases: []string{"account"},
		Short:   "Subscription accounts: 5-hour and weekly usage, state and who is using each login",
		Args:    cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] != "ls" {
				return usagef("unknown accounts subcommand %q", args[0])
			}
			if refresh { // probe token accounts now (accounts_token.go)
				if ops, err := a.connect(); err == nil {
					ops.RefreshAccounts(cmd.Context(), c.Provider(provider), a.warnf)
				}
			}
			return a.runAccounts(cmd.Context(), provider, watch, every) // accounts_usage.go
		}),
	}
	accounts.Flags().StringVar(&provider, "provider", "", "anthropic or openai")
	accounts.Flags().BoolVarP(&watch, "watch", "w", false, "refresh until interrupted")
	accounts.Flags().DurationVar(&every, "interval", 30*time.Second, "refresh interval with --watch")
	accounts.Flags().BoolVar(&refresh, "refresh", false, "probe token accounts' usage now (1 output token each)")
	a.addAccountOnboarding(accounts)
	a.addAccountRename(accounts) // accounts_rename.go
	a.addAccountTier(accounts)   // accounts_tier.go
	a.addAccountLogin(accounts)  // cmd_login.go
	a.addTokenCommands(accounts) // accounts_token.go
	root.AddCommand(accounts)

	// ---- connections ----
	var cNode, cProject string
	connections := &cobra.Command{
		Use:     "connections [ls|check <connection> --session S]",
		Aliases: []string{"connection"},
		Short:   "Named connections (metadata only) and their per-node availability",
		Args:    cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] != "ls" {
				return usagef("unknown connections subcommand %q", args[0])
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			req := c.ListConnectionsRequest{}
			if cNode != "" {
				n, err := ops.ResolveNode(cmd.Context(), cNode)
				if err != nil {
					return err
				}
				req.NodeID = n.NodeID
			}
			if cProject != "" {
				pid, err := ops.projectPtr(cmd.Context(), cProject)
				if err != nil {
					return err
				}
				req.ProjectID = *pid
			}
			page, err := listAll(cmd.Context(), func(ctx context.Context, cur string) (c.Page[c.Connection], error) {
				r := req
				r.Cursor = cur
				return ops.API.ListConnections(ctx, r)
			})
			if err != nil {
				return err
			}
			return a.emit(page, func(w io.Writer) { printConnections(w, page.Items) })
		}),
	}
	connections.Flags().StringVar(&cNode, "node", "", "availability on this node")
	connections.Flags().StringVar(&cProject, "project", "", "connections referenced by this project")
	var checkSession string
	check := &cobra.Command{
		Use:   "check <connection> --session <session>",
		Short: "Ask the hub whether a session may use a connection now (use-time grant decision)",
		Args:  cobra.ExactArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if checkSession == "" {
				return usagef("--session is required")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			cid, err := ops.ResolveConnection(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			sid, err := ops.sessionID(cmd.Context(), checkSession)
			if err != nil {
				return err
			}
			d, err := ops.API.CheckConnectionGrant(cmd.Context(), c.CheckGrantRequest{ConnectionID: cid, SessionID: sid})
			if err != nil {
				return err
			}
			return a.emit(d, func(w io.Writer) {
				verdict := "denied"
				if d.Allowed {
					verdict = "allowed"
				}
				fmt.Fprintf(w, "%s for %s: %s (%s; catalog generation %d)\n", cid, sid, verdict, d.Reason, d.Generation)
			})
		}),
	}
	check.Flags().StringVar(&checkSession, "session", "", "session ID or title")
	connections.AddCommand(check)
	a.addConnectionSubcommands(connections) // grant, revoke, invoke, verify (cmd_connections.go)
	root.AddCommand(connections)

	// ---- inventory ----
	var invNode string
	inventory := &cobra.Command{
		Use:   "inventory <node> | --node N",
		Short: "Installed harnesses, tools, extensions, config roots and drift on a node",
		Args:  cobra.MaximumNArgs(1),
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if invNode != "" && invNode != args[0] {
					return usagef("give the node once: as an argument or with --node")
				}
				invNode = args[0]
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			var inv c.NodeInventory
			if _, err := ops.NodeAction(cmd.Context(), invNode, c.ActionInventory, struct{}{}, &inv, a.opID); err != nil {
				return err
			}
			return a.emit(inv, func(w io.Writer) { printInventory(w, inv) })
		}),
	}
	inventory.Flags().StringVar(&invNode, "node", "", "node ID or name")
	root.AddCommand(inventory)

	// ---- env ----
	env := &cobra.Command{Use: "env", Short: "Environment generations: plan, apply, drift, rollback (per node)", Args: cobra.NoArgs}
	var envNode string
	env.PersistentFlags().StringVar(&envNode, "node", "", "node ID or name")
	var ep c.EnvPlanParams
	var sets []string
	plan := &cobra.Command{
		Use:   "plan --node N [--scope global|project:<id>|session:<id>] [--profile P]",
		Short: "Preview the changes an environment profile would make (nothing is applied)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			if ep.Scope == "" {
				ep.Scope = "global"
			}
			kv, err := parsePairs(sets, "--set")
			if err != nil {
				return err
			}
			for _, p := range kv {
				if ep.Overrides == nil {
					ep.Overrides = map[string]string{}
				}
				ep.Overrides[p[0]] = p[1]
			}
			var cp c.ChangePlan
			if _, err := ops.NodeAction(cmd.Context(), envNode, c.ActionEnvPlan, ep, &cp, a.opID); err != nil {
				return err
			}
			return a.emit(cp, func(w io.Writer) { printPlan(w, cp) })
		}),
	}
	plan.Flags().StringVar(&ep.Scope, "scope", "global", "global, project:<id> or session:<id>")
	plan.Flags().StringVar(&ep.Profile, "profile", "", "managed profile name")
	plan.Flags().StringVar(&ep.ProjectID, "project", "", "project ID")
	plan.Flags().StringSliceVar(&sets, "set", nil, "session override key=value")
	env.AddCommand(plan)
	var ap c.EnvApplyParams
	var apDigest string
	apply := &cobra.Command{
		Use:   "apply --node N --plan PLAN_ID --digest sha256:...",
		Short: "Apply a reviewed plan (its result digest must match)",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if ap.PlanID == "" || apDigest == "" {
				return usagef("--plan and --digest (from `pyre env plan`) are required")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			ap.ResultDigest = c.Digest(apDigest)
			var gen c.EnvironmentGeneration
			if _, err := ops.NodeAction(cmd.Context(), envNode, c.ActionEnvApply, ap, &gen, a.opID); err != nil {
				return err
			}
			return a.emit(gen, func(w io.Writer) { printGeneration(w, gen) })
		}),
	}
	apply.Flags().StringVar(&ap.PlanID, "plan", "", "plan ID")
	apply.Flags().StringVar(&apDigest, "digest", "", "the plan's result digest")
	env.AddCommand(apply)
	env.AddCommand(&cobra.Command{
		Use:   "drift --node N",
		Short: "Report managed items that differ from the applied generation",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			var items []c.DriftItem
			if _, err := ops.NodeAction(cmd.Context(), envNode, c.ActionEnvDrift, struct{}{}, &items, a.opID); err != nil {
				return err
			}
			if items == nil {
				items = []c.DriftItem{}
			}
			return a.emit(items, func(w io.Writer) { printDrift(w, items) })
		}),
	})
	var rbGen string
	rollback := &cobra.Command{
		Use:   "rollback --node N --generation GEN",
		Short: "Roll back to an earlier environment generation",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if rbGen == "" {
				return usagef("--generation is required")
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			var gen c.EnvironmentGeneration
			if _, err := ops.NodeAction(cmd.Context(), envNode, c.ActionEnvRollback, c.EnvRollbackParams{GenerationID: rbGen}, &gen, a.opID); err != nil {
				return err
			}
			return a.emit(gen, func(w io.Writer) { printGeneration(w, gen) })
		}),
	}
	rollback.Flags().StringVar(&rbGen, "generation", "", "generation ID")
	env.AddCommand(rollback)
	env.AddCommand(a.envProfileCommand(&envNode))
	root.AddCommand(env)

	// ---- update ----
	update := &cobra.Command{Use: "update", Short: "Tool versions: stage, activate, rollback, list (per node)", Args: cobra.NoArgs}
	var upNode string
	var up c.UpdateParams
	var upDigest string
	update.PersistentFlags().StringVar(&upNode, "node", "", "node ID or name")
	update.PersistentFlags().StringVar(&up.Tool, "tool", "", "tool name (claude, codex, pyred, plugin)")
	update.PersistentFlags().StringVar(&up.Version, "version", "", "version")
	update.PersistentFlags().StringVar(&up.Source, "source", "", "URL or package spec")
	update.PersistentFlags().StringVar(&upDigest, "digest", "", "expected sha256 digest")
	for _, x := range []struct{ name, action, short string }{
		{"stage", c.ActionUpdateStage, "Download, verify and health-check a version without activating it"},
		{"activate", c.ActionUpdateActivate, "Make a staged version the default for new runs (running sessions keep theirs)"},
		{"rollback", c.ActionUpdateRollback, "Return to the previous version"},
	} {
		x := x
		update.AddCommand(&cobra.Command{
			Use:   x.name + " --node N --tool T [--version V]",
			Short: x.short,
			Args:  cobra.NoArgs,
			RunE: a.run(func(cmd *cobra.Command, args []string) error {
				if up.Tool == "" {
					return usagef("--tool is required")
				}
				ops, err := a.connect()
				if err != nil {
					return err
				}
				p := up
				p.Digest = c.Digest(upDigest)
				var tv c.ToolVersion
				if _, err := ops.NodeAction(cmd.Context(), upNode, x.action, p, &tv, a.opID); err != nil {
					return err
				}
				return a.emit(tv, func(w io.Writer) { printTools(w, []c.ToolVersion{tv}) })
			}),
		})
	}
	update.AddCommand(&cobra.Command{
		Use:   "ls --node N",
		Short: "List installed, staged and previous tool versions",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			var tools []c.ToolVersion
			if _, err := ops.NodeAction(cmd.Context(), upNode, c.ActionUpdateList, struct{}{}, &tools, a.opID); err != nil {
				return err
			}
			if tools == nil {
				tools = []c.ToolVersion{}
			}
			return a.emit(tools, func(w io.Writer) { printTools(w, tools) })
		}),
	})
	root.AddCommand(update)

	// ---- migrate ----
	migrate := &cobra.Command{Use: "migrate", Short: "One-time migrations from legacy tools", Args: cobra.NoArgs}
	migrate.AddCommand(&cobra.Command{
		Use:   "ws-mail [--root COPY_DIR] [--alias-map FILE] [--apply] [wsimport flags]",
		Short: "Import legacy ws mail into Pyre messages (dry run unless --apply)",
		Long: `Import a COPY of the legacy ws mail tree into durable Pyre messages.
Dry run by default; --apply sends the plan idempotently with this CLI's
credential. The importer refuses the live ws root. Put global flags (--hub,
--config) before "migrate"; all other flags are the importer's (see
` + "`pyre migrate ws-mail --help`" + `).`,
		DisableFlagParsing: true,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			for _, x := range args {
				if x == "-h" || x == "--help" {
					return cmd.Help()
				}
			}
			factory := func(ctx context.Context) (c.MessageAPI, error) {
				ops, err := a.connect()
				if err != nil {
					return nil, err
				}
				return ops.API, nil
			}
			if code := wsimport.Main(cmd.Context(), args, a.Stdout, a.Stderr, a.Getenv, factory); code != 0 {
				return withStatus(code, fmt.Errorf("ws-mail import exited with status %d", code))
			}
			return nil
		}),
	})
	root.AddCommand(migrate)
}

// ---- formatting ----

func printConnections(w io.Writer, cs []c.Connection) {
	var rows [][]string
	for _, cn := range cs {
		var av []string
		for _, x := range cn.Availability {
			av = append(av, x.NodeID+":"+x.State)
		}
		scope := orDash(cn.Scope.Access)
		if len(cn.Scope.Nodes) > 0 {
			scope += " nodes=" + strings.Join(cn.Scope.Nodes, ",")
		}
		if len(cn.Scope.Projects) > 0 {
			scope += " projects=" + strings.Join(cn.Scope.Projects, ",")
		}
		rows = append(rows, []string{cn.ConnectionID, cn.Name, cn.Service, strings.Join(cn.Modes, ","), orDash(strings.Join(av, " ")), scope})
	}
	table(w, "ID\tNAME\tSERVICE\tMODES\tAVAILABILITY\tSCOPE", rows)
}

func printInventory(w io.Writer, inv c.NodeInventory) {
	fmt.Fprintf(w, "node %s inventory collected %s, generation %s\n", inv.NodeID, ago(inv.CollectedAt), orDash(inv.CurrentGeneration))
	fmt.Fprintf(w, "harnesses: %s\n", harnessText(inv.Harnesses))
	for _, k := range sortedKeys(inv.Integrations) {
		s := inv.Integrations[k]
		dirty := ""
		if s.Dirty {
			dirty = " (local modifications preserved)"
		}
		fmt.Fprintf(w, "integration %s: present=%v %s %s%s\n", k, s.Present, orDash(s.Version), orDash(s.Health), dirty)
	}
	if len(inv.Tools) > 0 {
		printTools(w, inv.Tools)
	}
	for _, e := range inv.Extensions {
		m := "adopted"
		if e.Managed {
			m = "managed"
		}
		fmt.Fprintf(w, "extension %s %s (%s, %s layer) %s\n", e.Name, orDash(e.Version), m, e.Layer, orDash(string(e.Harness)))
	}
	for _, r := range inv.ConfigRoots {
		fmt.Fprintf(w, "config root %s %s exists=%v managed=%v\n", r.Harness, r.Path, r.Exists, r.Managed)
	}
	if len(inv.Drift) > 0 {
		printDrift(w, inv.Drift)
	}
}

func printPlan(w io.Writer, p c.ChangePlan) {
	fmt.Fprintf(w, "plan %s for %s scope %s, result %s\n", p.PlanID, p.NodeID, p.Scope, p.ResultDigest)
	for _, ch := range p.Changes {
		fmt.Fprintf(w, "  %-10s %s\n", ch.Action, ch.Path)
		if ch.Diff != "" {
			for _, l := range strings.Split(strings.TrimRight(ch.Diff, "\n"), "\n") {
				fmt.Fprintf(w, "      %s\n", l)
			}
		}
	}
	for _, cf := range p.Conflicts {
		fmt.Fprintf(w, "  CONFLICT %s %s: %s\n", cf.Path, cf.Key, cf.Detail)
	}
	if len(p.RestartRequired) > 0 {
		fmt.Fprintf(w, "  restart required for sessions: %s\n", strings.Join(p.RestartRequired, ", "))
	}
	fmt.Fprintf(w, "apply with: pyre env apply --node %s --plan %s --digest %s\n", p.NodeID, p.PlanID, p.ResultDigest)
}

func printGeneration(w io.Writer, g c.EnvironmentGeneration) {
	fmt.Fprintf(w, "generation %s on %s profile %s (previous %s), digest %s\n", g.GenerationID, g.NodeID, orDash(g.Profile), orDash(g.PreviousID), g.Digest)
	for _, k := range sortedKeys(g.RuntimeVersions) {
		fmt.Fprintf(w, "  %s %s\n", k, g.RuntimeVersions[k])
	}
	for _, v := range g.Values {
		rs := ""
		if v.RestartRequired {
			rs = " (restart required)"
		}
		fmt.Fprintf(w, "  %s = %s  [%s]%s\n", v.Key, v.Value, v.Layer, rs)
	}
}

func printDrift(w io.Writer, items []c.DriftItem) {
	if len(items) == 0 {
		fmt.Fprintln(w, "no drift")
		return
	}
	var rows [][]string
	for _, d := range items {
		rows = append(rows, []string{d.Kind, d.Path, orDash(d.Key), orDash(d.Expected), orDash(d.Actual), d.Detail})
	}
	table(w, "KIND\tPATH\tKEY\tEXPECTED\tACTUAL\tDETAIL", rows)
}

func printTools(w io.Writer, ts []c.ToolVersion) {
	var rows [][]string
	for _, t := range ts {
		rows = append(rows, []string{t.Tool, t.Version, t.State, orDash(t.Health), ago(t.InstalledAt), orDash(strings.Join(t.InUseByRuns, ","))})
	}
	table(w, "TOOL\tVERSION\tSTATE\tHEALTH\tINSTALLED\tIN USE BY", rows)
}

// envProfileResult mirrors configuration.ProfileSetResult/ProfileGetResult
// (env.profile.set / env.profile.get node actions).
type envProfileResult struct {
	Scope             string          `json:"scope"`
	Name              string          `json:"name"`
	Present           *bool           `json:"present,omitempty"`
	Profile           json.RawMessage `json:"profile,omitempty"`
	ProfileDigest     c.Digest        `json:"profile_digest,omitempty"`
	Pending           *bool           `json:"pending,omitempty"`
	Desired           json.RawMessage `json:"desired,omitempty"`
	DesiredDigest     c.Digest        `json:"desired_digest,omitempty"`
	AppliedGeneration string          `json:"applied_generation,omitempty"`
	Plan              *c.ChangePlan   `json:"plan,omitempty"`
}

// envProfileCommand: pyre env profile set|get.
func (a *App) envProfileCommand(node *string) *cobra.Command {
	prof := &cobra.Command{Use: "profile", Short: "Desired managed environment profile on a node (set, get)", Args: cobra.NoArgs}
	var file, project, name string
	set := &cobra.Command{
		Use:   "set --node N --file PROFILE.json [--project P]",
		Short: "Store the desired profile and print the plan that applies it (new runs change only after env apply)",
		Long: `Store a managed environment profile (JSON: name, settings, persist, skills,
plugins, mcp_servers, hooks, connections, runtimes) as the node's desired state
for the global scope, or for a project with --project, and print the plan for
it. Profiles never carry secrets. Once a scope has an applied generation, new
runs keep using the applied profile until the plan is applied with
` + "`pyre env apply`" + `; running sessions keep their generation either way (the plan
lists them as restart-required).`,
		Args: cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			if file == "" {
				return usagef("--file is required (use - for stdin)")
			}
			var body string
			var err error
			if file == "-" {
				body, err = a.readArg("-")
			} else {
				body, err = a.textOrFile("", file)
			}
			if err != nil {
				return err
			}
			if !json.Valid([]byte(body)) {
				return usagef("%s is not valid JSON", file)
			}
			ops, err := a.connect()
			if err != nil {
				return err
			}
			params := map[string]any{"profile": json.RawMessage(body)}
			if project != "" {
				pid, err := ops.projectPtr(cmd.Context(), project)
				if err != nil {
					return err
				}
				params["project_id"] = *pid
			}
			var res envProfileResult
			if _, err := ops.NodeAction(cmd.Context(), *node, c.ActionEnvProfileSet, params, &res, a.opID); err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) {
				fmt.Fprintf(w, "desired profile %s (%s) stored, digest %s\n", res.Name, res.Scope, res.ProfileDigest)
				if res.Plan != nil {
					printPlan(w, *res.Plan)
				}
			})
		}),
	}
	set.Flags().StringVar(&file, "file", "", "profile JSON file (- for stdin)")
	set.Flags().StringVar(&project, "project", "", "project ID or name (default: global scope)")
	prof.AddCommand(set)
	var gProject string
	get := &cobra.Command{
		Use:   "get --node N [--name P] [--project P]",
		Short: "Show the desired profile and whether it differs from the applied generation",
		Args:  cobra.NoArgs,
		RunE: a.run(func(cmd *cobra.Command, args []string) error {
			ops, err := a.connect()
			if err != nil {
				return err
			}
			params := map[string]any{}
			if name != "" {
				params["name"] = name
			}
			if gProject != "" {
				pid, err := ops.projectPtr(cmd.Context(), gProject)
				if err != nil {
					return err
				}
				params["project_id"] = *pid
			}
			var res envProfileResult
			if _, err := ops.NodeAction(cmd.Context(), *node, c.ActionEnvProfileGet, params, &res, a.opID); err != nil {
				return err
			}
			return a.emit(res, func(w io.Writer) { printEnvProfile(w, res) })
		}),
	}
	get.Flags().StringVar(&name, "name", "", "profile name (default \"default\")")
	get.Flags().StringVar(&gProject, "project", "", "project ID or name (default: global scope)")
	prof.AddCommand(get)
	return prof
}

func printEnvProfile(w io.Writer, res envProfileResult) {
	if res.Present != nil && *res.Present {
		fmt.Fprintf(w, "active profile %s (%s), digest %s; new runs use it\n", res.Name, res.Scope, res.ProfileDigest)
	} else {
		fmt.Fprintf(w, "no active profile %s for %s\n", res.Name, res.Scope)
	}
	fmt.Fprintf(w, "applied generation %s\n", orDash(res.AppliedGeneration))
	if res.Pending != nil && *res.Pending {
		fmt.Fprintf(w, "pending desired profile, digest %s: not applied (pyre env plan, then pyre env apply)\n", res.DesiredDigest)
	}
	if len(res.Profile) > 0 {
		fmt.Fprintf(w, "active: %s\n", res.Profile)
	}
	if len(res.Desired) > 0 {
		fmt.Fprintf(w, "desired: %s\n", res.Desired)
	}
}
