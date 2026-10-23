package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jhh3/outfit/internal/admin"
	"github.com/jhh3/outfit/internal/agentguide"
	"github.com/jhh3/outfit/internal/agentio"
	"github.com/jhh3/outfit/internal/application"
	"github.com/jhh3/outfit/internal/backup"
	"github.com/jhh3/outfit/internal/builtin"
	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/importer"
	"github.com/jhh3/outfit/internal/invoke"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/linkcontext"
	"github.com/jhh3/outfit/internal/mcpserver"
	"github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/skillmanager"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/toolchain"
	"github.com/jhh3/outfit/internal/tui"
	"github.com/jhh3/outfit/internal/vault"
	"github.com/jhh3/outfit/internal/workspace"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type BuildInfo struct{ Version, Commit, Date string }
type environment struct {
	state           *state.Store
	vault           vault.Store
	service         connection.Service
	tools           toolchain.Manager
	skills          skillmanager.Manager
	policy          *workspace.Policy
	linkPreferences *linkcontext.Preferences
}

func Execute(build BuildInfo) error {
	root := &cobra.Command{Use: "outfit", Short: "Local-first connectors for people and coding agents", SilenceErrors: true, SilenceUsage: true}
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return cmd.Help()
		}
		return withEnvironment(cmd, func(env *environment) error { return tui.Run(cmd.Context(), applicationFor(env)) })
	}
	root.Version = build.Version
	root.SetVersionTemplate(fmt.Sprintf("outfit %s (%s, %s)\n", build.Version, build.Commit, build.Date))
	root.AddCommand(versionCommand(build), setupCommand(), tuiCommand(), workspaceCommand(), workspaceLockCommand(), workspaceApplyCommand(), connectorCommand(), credentialCommand(), toolCommand(), skillCommand(), backupCommand(), listCommand(), searchCommand(), cacheCommand(), describeCommand(), explainCommand(), runCommand(false), runCommand(true), getCommand(), workflowCommand(), execCommand(), logCommand(), contextCommand(), sessionCommand(), profileCommand(), pluginCommand(), mcpCommand(), adminCommand(), doctorCommand(), kitCommand(), vaultCommand())
	if wantsDynamicConnectionCommands(root, os.Args[1:]) {
		registerDynamicConnectionCommands(root)
	}
	err := root.Execute()
	if err == nil || agentio.IsReported(err) {
		return err
	}
	if agentio.Enabled(os.Stdout) || agentio.Requested(os.Args[1:]) {
		failure := invoke.Classify(err, "", "")
		envelope := invoke.Envelope{Schema: invoke.Schema, OK: false, Operation: requestedCommand(os.Args[1:]), Class: "read", Meta: invoke.Meta{}, Error: failure}
		if renderErr := agentio.Render(os.Stdout, envelope, agentio.Enabled(os.Stdout)); renderErr == nil {
			return agentio.Reported(err)
		}
	}
	return err
}

func requestedCommand(arguments []string) string {
	for _, argument := range arguments {
		if !strings.HasPrefix(argument, "-") {
			return argument
		}
	}
	return "outfit"
}

// IsReportedError lets the process entry point avoid appending plain text to a
// structured failure envelope that has already been written to stdout.
func IsReportedError(err error) bool { return agentio.IsReported(err) }

func wantsDynamicConnectionCommands(root *cobra.Command, arguments []string) bool {
	if len(arguments) == 0 || arguments[0] == "--version" || arguments[0] == "-v" {
		return false
	}
	if arguments[0] == "--help" || arguments[0] == "-h" {
		return true
	}
	requested := arguments[0]
	for _, command := range root.Commands() {
		if command.Name() == requested {
			return false
		}
		for _, alias := range command.Aliases {
			if alias == requested {
				return false
			}
		}
	}
	return true
}

// registerDynamicConnectionCommands gives every active connection a concise,
// typed namespace (`outfit slack read-thread ...`). The command delegates to
// the same operation path as `outfit run`/`write`; it is presentation, not a
// second runtime.
func registerDynamicConnectionCommands(root *cobra.Command) {
	env, err := openBaseEnvironment(context.Background())
	if err != nil {
		return
	}
	defer env.state.Close()
	connections, err := env.state.Connections(context.Background())
	if err != nil {
		return
	}
	reserved := map[string]bool{}
	for _, command := range root.Commands() {
		reserved[command.Name()] = true
		for _, alias := range command.Aliases {
			reserved[alias] = true
		}
	}
	connectorCounts := map[string]int{}
	for _, item := range connections {
		if item.Status == "active" {
			connectorCounts[item.ConnectorKey]++
		}
	}
	for _, item := range connections {
		if item.Status != "active" || reserved[item.Name] {
			continue
		}
		installed, _, operations := selectedOperations(context.Background(), env, item)
		if len(operations) == 0 {
			continue
		}
		item := item
		connectionCommand := &cobra.Command{Use: item.Name, Short: "Use the active " + installed.Package.Display + " connection", Args: cobra.NoArgs}
		if connectorCounts[item.ConnectorKey] == 1 && item.ConnectorKey != item.Name && !reserved[item.ConnectorKey] {
			connectionCommand.Aliases = []string{item.ConnectorKey}
			reserved[item.ConnectorKey] = true
		}
		used := map[string]bool{}
		for _, operation := range operations {
			operation := operation
			friendly := invoke.FriendlyOperationName(installed.Key, operation.Name)
			if used[friendly] {
				friendly = strings.ReplaceAll(operation.Name, "_", "-")
			}
			used[friendly] = true
			aliases := []string{}
			for _, alias := range []string{operation.Name, strings.ReplaceAll(operation.Name, "_", "-")} {
				if alias != friendly && !used[alias] {
					aliases = append(aliases, alias)
					used[alias] = true
				}
			}
			operationCommand := &cobra.Command{
				Use:                friendly + " [operation flags]",
				Aliases:            aliases,
				Short:              strings.ToUpper(operation.Class[:1]) + operation.Class[1:] + ": " + operation.Description,
				DisableFlagParsing: true,
				Args:               cobra.ArbitraryArgs,
				RunE: func(cmd *cobra.Command, args []string) error {
					return executeOperationCommand(cmd, item.Name, operation.Name, args, false, operationCLIOptions{})
				},
			}
			connectionCommand.AddCommand(operationCommand)
		}
		root.AddCommand(connectionCommand)
		reserved[item.Name] = true
	}
}

func applicationFor(env *environment) application.Service {
	workDir, _ := os.Getwd()
	var linkPolicy func(string) bool
	if env.policy != nil {
		linkPolicy = env.policy.Links
	}
	return application.Service{State: env.state, Vault: env.vault, Connections: env.service, Tools: env.tools, Skills: env.skills, Host: "both", WorkDir: workDir, LinkPreferences: env.linkPreferences, LinkPolicy: linkPolicy}
}

func tuiCommand() *cobra.Command {
	return &cobra.Command{Use: "tui", Short: "Open the provider-centric connector workbench", RunE: func(cmd *cobra.Command, _ []string) error {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("outfit tui requires an interactive terminal")
		}
		return withEnvironment(cmd, func(env *environment) error { return tui.Run(cmd.Context(), applicationFor(env)) })
	}}
}

func openEnvironment(ctx context.Context) (*environment, error) {
	env, err := openBaseEnvironment(ctx)
	if err != nil {
		return nil, err
	}
	loaded, err := workspace.Read(".")
	if err != nil {
		env.state.Close()
		return nil, err
	}
	if loaded.Path != "" {
		policy, err := workspace.NewPolicy(loaded)
		if err != nil {
			env.state.Close()
			return nil, err
		}
		var approved string
		if err := env.state.ReadSetting(ctx, workspace.ApprovalKey(loaded.Root), &approved); err != nil || approved != policy.Digest {
			env.state.Close()
			return nil, errors.New("workspace is not locally approved; review .outfit/workspace.yaml and .outfit/workspace.lock.yaml, then run outfit apply")
		}
		env.policy = &policy
		env.service.Allowed = policy.Allows
		env.service.AllowedOperation = policy.AllowsOperation
		env.service.AllowedLinks = policy.Links
		env.service.AllowedLink = policy.AllowsLink
		env.service.AllowedConfiguration = policy.AllowsConfiguration
		if err := validateWorkspaceState(ctx, env, loaded); err != nil {
			env.state.Close()
			return nil, err
		}
		if err := validateWorkspaceSkills(env, loaded); err != nil {
			env.state.Close()
			return nil, err
		}
		if err := configureLinkPreferences(ctx, env); err != nil {
			env.state.Close()
			return nil, err
		}
		return env, nil
	}
	if err := configureLinkPreferences(ctx, env); err != nil {
		env.state.Close()
		return nil, err
	}
	return env, nil
}

func configureLinkPreferences(ctx context.Context, env *environment) error {
	preferences, err := linkcontext.LoadPreferences(ctx, env.state)
	if err != nil {
		return err
	}
	policyAllows := env.service.AllowedLinks
	env.linkPreferences = preferences
	env.service.AllowedLinks = func(name string) bool {
		return preferences.Enabled(name) && (policyAllows == nil || policyAllows(name))
	}
	return nil
}

func validateWorkspaceState(ctx context.Context, env *environment, loaded workspace.Loaded) error {
	for _, locked := range loaded.Lock.Tools {
		receipt, err := env.tools.Resolve(locked.ID)
		if err != nil || receipt.Version != locked.Version || (locked.Checksums[receipt.Platform] != "" && locked.Checksums[receipt.Platform] != receipt.ActualSHA) {
			return fmt.Errorf("workspace tool %s does not match its lock; run outfit apply", locked.ID)
		}
	}
	for _, locked := range loaded.Lock.Packages {
		installed, err := env.state.Package(ctx, locked.Key)
		if err != nil || installed.Digest != locked.Digest {
			return fmt.Errorf("workspace package %s does not match its lock; run outfit apply", locked.Key)
		}
	}
	for _, declared := range loaded.Lock.Connectors {
		connection, err := env.state.Connection(ctx, declared.Name)
		if err != nil || connection.Status != "active" || connection.ConnectorKey != declared.Package || connection.SelectedMode != declared.Mode || connection.CredentialMethod != declared.CredentialMethod {
			return fmt.Errorf("workspace connection %s is missing or drifted; run outfit apply", declared.Name)
		}
		installed, err := env.state.Package(ctx, declared.Package)
		if err != nil {
			return fmt.Errorf("workspace package %s is missing; run outfit apply", declared.Package)
		}
		mode, ok := runtime.FindMode(installed.Package, declared.Mode)
		if !ok {
			return fmt.Errorf("workspace mode %s/%s is missing; run outfit lock", declared.Package, declared.Mode)
		}
		available := map[string]bool{}
		for _, operation := range mode.Operations {
			available[operation.Name] = true
		}
		if mode.Executor.Kind == "mcp" {
			tools, err := env.state.Tools(ctx, connection.ID)
			if err != nil {
				return err
			}
			for _, tool := range tools {
				available[tool.Name] = true
			}
		}
		for _, operation := range declared.Operations {
			if !available[operation] {
				return fmt.Errorf("workspace connection %s is missing locked operation %s; run outfit apply", declared.Name, operation)
			}
		}
	}
	return nil
}

func validateWorkspaceSkills(env *environment, loaded workspace.Loaded) error {
	if len(loaded.Lock.Skills) == 0 {
		return nil
	}
	inventory, err := env.skills.Inventory("both")
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, skill := range inventory {
		installed[strings.Join([]string{skill.Connection, skill.Source, skill.Path, skill.Ref, skill.Digest}, "|")] = true
	}
	for _, locked := range loaded.Lock.Skills {
		for _, path := range locked.Paths {
			key := strings.Join([]string{locked.Connector, locked.Repository, path, locked.Ref, locked.Digest}, "|")
			// Skill receipts name the source repository in Source, while authored/generated
			// skills are represented by the connector package and need no upstream pin check.
			if locked.Repository != "" && !installed[key] {
				return fmt.Errorf("workspace skill %s/%s is missing or drifted; run outfit skill sync", locked.Connector, path)
			}
		}
	}
	return nil
}

func openBaseEnvironment(ctx context.Context) (*environment, error) {
	paths, err := state.ResolvePaths()
	if err != nil {
		return nil, err
	}
	store, err := state.Open(ctx, paths)
	if err != nil {
		return nil, err
	}
	secret, err := vault.Open(paths)
	if err != nil {
		store.Close()
		return nil, err
	}
	packages, err := builtin.All()
	if err != nil {
		store.Close()
		return nil, err
	}
	for _, pkg := range packages {
		if err := store.InstallPackage(ctx, pkg, "builtin"); err != nil {
			store.Close()
			return nil, err
		}
	}
	tools := toolchain.Manager{Paths: paths}
	skills := skillmanager.Manager{Paths: paths}
	return &environment{state: store, vault: secret, tools: tools, skills: skills, service: connection.Service{State: store, Vault: secret, Executor: runtime.Executor{}, ResolveTool: tools.ResolveRequirement}}, nil
}

func withEnvironment(cmd *cobra.Command, fn func(*environment) error) error {
	env, err := openEnvironment(cmd.Context())
	if err != nil {
		return err
	}
	defer env.state.Close()
	return fn(env)
}

func versionCommand(build BuildInfo) *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print build information", Run: func(cmd *cobra.Command, _ []string) {
		fmt.Fprintf(cmd.OutOrStdout(), "outfit %s (%s, %s)\n", build.Version, build.Commit, build.Date)
	}}
}

func setupCommand() *cobra.Command {
	return &cobra.Command{Use: "setup", Short: "Initialize local state and install built-in connector packages", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			if err := env.service.SweepSecrets(cmd.Context()); err != nil {
				return err
			}
			snapshot, err := applicationFor(env).Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "outfit is ready in %s\n%d connector packages · %d connected\n", env.state.Paths().Root, len(snapshot.Connectors), len(snapshot.Connections))
			var existing []string
			for _, connector := range snapshot.Connectors {
				if len(connector.ExistingSetup) > 0 && len(connector.Connections) == 0 {
					existing = append(existing, connector.Display+" ("+strings.Join(connector.ExistingSetup, ", ")+")")
				}
			}
			if len(existing) > 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "\nExisting setup detected — not connected until you choose and verify it:")
				for _, item := range existing {
					fmt.Fprintln(cmd.OutOrStdout(), "  "+item)
				}
			}
			if len(snapshot.Imports) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d Claude/Codex MCP OAuth connection(s) can be reviewed with: outfit connector imports\n", len(snapshot.Imports))
			}
			var detected []string
			for _, tool := range snapshot.Tools {
				if tool.Current == nil && tool.DetectedPath != "" {
					detected = append(detected, tool.ID)
				}
			}
			if len(detected) > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%d existing CLI(s) found on PATH but not managed by outfit: %s\n", len(detected), strings.Join(detected, ", "))
				fmt.Fprintln(cmd.OutOrStdout(), "Adopt one with: outfit tool adopt ID PATH")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nOpen the guided workbench with: outfit tui")
			return nil
		})
	}}
}

func toolCommand() *cobra.Command {
	root := &cobra.Command{Use: "tool", Short: "Install, inspect, update, and repair provider CLIs managed by outfit"}
	root.AddCommand(toolCatalogCommand(), toolStatusCommand(), toolPathCommand(), toolInstallCommand("install"), toolInstallCommand("update"), toolInstallCommand("repair"), toolUseCommand(), toolAdoptCommand(), toolRemoveCommand(), toolGCCommand())
	return root
}

func toolCatalogCommand() *cobra.Command {
	return &cobra.Command{Use: "catalog [query]", Short: "Search the blessed provider CLI catalog", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		query := ""
		if len(args) == 1 {
			query = strings.ToLower(args[0])
		}
		for _, entry := range toolchain.Catalog() {
			haystack := strings.ToLower(strings.Join([]string{entry.ID, entry.Display, entry.Binary, entry.Category, entry.Description}, " "))
			if query != "" && !strings.Contains(haystack, query) {
				continue
			}
			version, source := "adopt", "system"
			if entry.Requirement.ID != "" {
				version, source = entry.Requirement.Version, entry.Requirement.Source.Kind
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-12s %-16s %-8s %s\n", entry.ID, entry.Binary, version, source, entry.Description)
		}
		return nil
	}}
}

func toolStatusCommand() *cobra.Command {
	return &cobra.Command{Use: "status [id]", Short: "Show installed CLI versions, digests, paths, and current selection", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			receipts, err := env.tools.List()
			if err != nil {
				return err
			}
			filter := ""
			if len(args) == 1 {
				filter = args[0]
			}
			for _, receipt := range receipts {
				if filter != "" && receipt.ID != filter {
					continue
				}
				current := ""
				if selected, err := env.tools.Resolve(receipt.ID); err == nil && selected.Version == receipt.Version && selected.Platform == receipt.Platform {
					current = "current"
				}
				kind := "managed"
				if receipt.Adopted {
					kind = "adopted"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s %s %s %-7s %-7s %s\n", receipt.ID, receipt.Version, receipt.Platform, kind, current, receipt.Path)
			}
			return nil
		})
	}}
}

func toolPathCommand() *cobra.Command {
	return &cobra.Command{Use: "path ID", Short: "Print the selected provider CLI path", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			requirement, err := resolveToolRequirement(cmd.Context(), env, args[0])
			if err != nil {
				return err
			}
			path, err := env.tools.ResolveRequirement(requirement)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		})
	}}
}

func toolInstallCommand(verb string) *cobra.Command {
	return &cobra.Command{Use: verb + " ID", Short: strings.ToUpper(verb[:1]) + verb[1:] + " the tested provider CLI release", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			requirement, err := resolveToolRequirement(cmd.Context(), env, args[0])
			if err != nil {
				return err
			}
			receipt, err := env.tools.Install(cmd.Context(), requirement)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s ready at %s\n%s\n", receipt.Display, receipt.Version, receipt.Path, receipt.VersionText)
			return nil
		})
	}}
}

func toolUseCommand() *cobra.Command {
	return &cobra.Command{Use: "use ID VERSION", Short: "Select an already installed CLI version", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			receipts, err := env.tools.List()
			if err != nil {
				return err
			}
			for _, receipt := range receipts {
				if receipt.ID == args[0] && receipt.Version == args[1] {
					if err := env.tools.Use(receipt.ID, receipt.Version, receipt.Platform); err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s now uses %s for %s\n", receipt.ID, receipt.Version, receipt.Platform)
					return nil
				}
			}
			return fmt.Errorf("%s %s is not installed", args[0], args[1])
		})
	}}
}

func toolAdoptCommand() *cobra.Command {
	return &cobra.Command{Use: "adopt ID PATH", Short: "Explicitly adopt an existing provider CLI", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			requirement, err := resolveToolRequirement(cmd.Context(), env, args[0])
			if err != nil {
				return err
			}
			receipt, err := env.tools.Adopt(cmd.Context(), requirement, args[1])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "adopted %s %s at %s\n", receipt.ID, receipt.VersionText, receipt.Path)
			return nil
		})
	}}
}

func toolRemoveCommand() *cobra.Command {
	return &cobra.Command{Use: "remove ID VERSION", Short: "Remove a non-current managed CLI version", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			receipts, err := env.tools.List()
			if err != nil {
				return err
			}
			for _, receipt := range receipts {
				if receipt.ID == args[0] && receipt.Version == args[1] {
					return env.tools.Remove(receipt.ID, receipt.Version, receipt.Platform)
				}
			}
			return fmt.Errorf("%s %s is not installed", args[0], args[1])
		})
	}}
}

func toolGCCommand() *cobra.Command {
	return &cobra.Command{Use: "gc", Short: "Remove installed CLI versions that are not current", RunE: func(cmd *cobra.Command, _ []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			removed, err := env.tools.GC()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "removed %d unused CLI versions\n", len(removed))
			return nil
		})
	}}
}

func withBaseEnvironment(cmd *cobra.Command, fn func(*environment) error) error {
	env, err := openBaseEnvironment(cmd.Context())
	if err != nil {
		return err
	}
	defer env.state.Close()
	return fn(env)
}

func resolveToolRequirement(ctx context.Context, env *environment, id string) (kit.ToolRequirement, error) {
	packages, err := env.state.Packages(ctx)
	if err != nil {
		return kit.ToolRequirement{}, err
	}
	for _, installed := range packages {
		for _, requirement := range installed.Package.Tools {
			if requirement.ID == id {
				return requirement, nil
			}
		}
	}
	entry, ok := toolchain.CatalogByID(id)
	if !ok {
		return kit.ToolRequirement{}, fmt.Errorf("unknown tool %s; run outfit tool catalog", id)
	}
	if entry.Requirement.ID != "" {
		return entry.Requirement, nil
	}
	return kit.ToolRequirement{ID: entry.ID, Display: entry.Display, Binary: entry.Binary, Version: "system", Source: kit.ToolSource{Kind: "adopt-only"}, Verify: []string{"--version"}}, nil
}

func skillCommand() *cobra.Command {
	root := &cobra.Command{Use: "skill", Short: "Inspect, update, enable, disable, synchronize, and repair connector skills"}
	root.AddCommand(skillCatalogCommand(), skillListCommand(), skillSyncCommand("sync"), skillSyncCommand("update"), skillSyncCommand("repair"), skillToggleCommand(true), skillToggleCommand(false), skillDiffCommand(), skillVerifySourceCommand())
	return root
}

func skillVerifySourceCommand() *cobra.Command {
	return &cobra.Command{Use: "verify-source CONNECTOR", Args: cobra.ExactArgs(1), Short: "Download and validate every pinned upstream skill without changing installed skills", RunE: func(cmd *cobra.Command, args []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			installed, err := env.state.Package(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			verified := 0
			for _, source := range installed.Package.SkillSources {
				if source.Kind != "github-directory" {
					continue
				}
				count, err := env.skills.VerifySource(cmd.Context(), source)
				if err != nil {
					return fmt.Errorf("%s: %w", source.ID, err)
				}
				verified += count
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s@%s\t%d skills\t%s\n", source.ID, source.Repository, source.Ref, count, source.Digest)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "validated %d pinned upstream skill paths without changing installed skills\n", verified)
			return nil
		})
	}}
}

func skillCatalogCommand() *cobra.Command {
	return &cobra.Command{Use: "catalog", Short: "List authored, generated, and upstream skills declared by installed connectors", RunE: func(cmd *cobra.Command, _ []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			packages, err := env.state.Packages(cmd.Context())
			if err != nil {
				return err
			}
			for _, installed := range packages {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\tgenerated\tconnector package\n", installed.Key)
				for _, source := range installed.Package.SkillSources {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s@%s\t%d paths\n", installed.Key, source.ID, source.Repository, source.Ref, len(source.Paths))
				}
			}
			return nil
		})
	}}
}

func skillListCommand() *cobra.Command {
	var host string
	command := &cobra.Command{Use: "list", Aliases: []string{"status"}, Short: "Show installed connector skills and their pinned provenance", RunE: func(cmd *cobra.Command, _ []string) error {
		return withBaseEnvironment(cmd, func(env *environment) error {
			inventory, err := env.skills.Inventory(host)
			if err != nil {
				return err
			}
			for _, skill := range inventory {
				ref := skill.Ref
				if len(ref) > 12 {
					ref = ref[:12]
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-8s %-20s %-32s %-18s %-12s %s\n", skill.Host, skill.Connection, skill.ID, skill.Kind, ref, skill.InstalledPath)
			}
			return nil
		})
	}}
	command.Flags().StringVar(&host, "host", "both", "codex, claude, or both")
	return command
}

func skillSyncCommand(action string) *cobra.Command {
	var host string
	command := &cobra.Command{Use: action, Short: strings.ToUpper(action[:1]) + action[1:] + " active connector skills", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			written, err := syncSkills(cmd.Context(), env, host)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "synchronized %d connector skills; restart Codex/Claude sessions to load changes\n", len(written))
			return nil
		})
	}}
	command.Flags().StringVar(&host, "host", "both", "codex, claude, or both")
	return command
}

func skillToggleCommand(enabled bool) *cobra.Command {
	verb := "enable"
	if !enabled {
		verb = "disable"
	}
	var host string
	command := &cobra.Command{Use: verb + " CONNECTION SOURCE PATH", Short: strings.ToUpper(verb[:1]) + verb[1:] + " an optional upstream connector skill", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		if host == "both" {
			for _, target := range []string{"codex", "claude"} {
				if err := withBaseEnvironment(cmd, func(env *environment) error { return env.skills.SetEnabled(args[0], args[1], args[2], target, enabled) }); err != nil {
					return err
				}
			}
		} else {
			if err := withBaseEnvironment(cmd, func(env *environment) error { return env.skills.SetEnabled(args[0], args[1], args[2], host, enabled) }); err != nil {
				return err
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s for %s; run outfit skill sync to apply\n", verb, args[2], host)
		return nil
	}}
	command.Flags().StringVar(&host, "host", "both", "codex, claude, or both")
	return command
}

func skillDiffCommand() *cobra.Command {
	return &cobra.Command{Use: "diff", Short: "Show pinned skill sources that would be synchronized", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			connections, err := env.state.Connections(cmd.Context())
			if err != nil {
				return err
			}
			for _, connection := range connections {
				installed, err := env.state.Package(cmd.Context(), connection.ConnectorKey)
				if err != nil || connection.Status != "active" {
					continue
				}
				for _, source := range installed.Package.SkillSources {
					digest := source.Digest
					if len(digest) > 12 {
						digest = digest[:12]
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s@%s\t%s\n", connection.Name, source.ID, source.Repository, source.Ref, digest)
				}
			}
			return nil
		})
	}}
}

func backupCommand() *cobra.Command {
	root := &cobra.Command{Use: "backup", Short: "Create, inspect, and restore encrypted portable outfit backups"}
	root.AddCommand(backupCreateCommand(), backupInspectCommand(), backupRestoreCommand())
	return root
}

func backupCreateCommand() *cobra.Command {
	var allowMissing, excludeOAuth bool
	command := &cobra.Command{Use: "create PATH", Short: "Encrypt connections, credentials, tool pins, and skill preferences into one portable file", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		passphrase, err := backupPassphrase(cmd, true)
		if err != nil {
			return err
		}
		return withBaseEnvironment(cmd, func(env *environment) error {
			skipped, err := applicationFor(env).CreateBackup(cmd.Context(), args[0], passphrase, backup.BuildOptions{ExcludeOAuth: excludeOAuth})
			if err != nil {
				return err
			}
			var missing, oauth []string
			for _, item := range skipped {
				if item.Kind == backup.SkipOAuth {
					oauth = append(oauth, item.Name)
				} else {
					missing = append(missing, item.Name)
				}
			}
			if len(oauth) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "left out (OAuth, --exclude-oauth): %s\n", strings.Join(oauth, ", "))
			}
			if len(missing) > 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), "WARNING: %d connection(s) left out because their secret is missing from the vault; reconnect them with `outfit credential replace NAME`:\n", len(missing))
				for _, item := range skipped {
					if item.Kind != backup.SkipOAuth {
						fmt.Fprintf(cmd.ErrOrStderr(), "  %s: %s\n", item.Name, item.Reason)
					}
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "encrypted outfit backup written to %s\n", args[0])
			if len(missing) > 0 && !allowMissing {
				return fmt.Errorf("backup is incomplete: not copied (secret missing): %s; pass --allow-missing to accept", strings.Join(missing, ", "))
			}
			return nil
		})
	}}
	addPassphraseFileFlag(command)
	command.Flags().BoolVar(&allowMissing, "allow-missing", false, "succeed even when connections are left out because their secret is missing")
	command.Flags().BoolVar(&excludeOAuth, "exclude-oauth", false, "leave out connections whose credential is an Outfit-managed OAuth grant (copies of one refresh token invalidate each other)")
	return command
}

func backupInspectCommand() *cobra.Command {
	return &cobra.Command{Use: "inspect PATH", Short: "Inspect non-secret backup metadata without decrypting it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		envelope, err := backup.Inspect(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "format: %s\nencryption: AES-256-GCM\nkdf: %s (time=%d memory=%d KiB parallelism=%d)\ncreated: %s\nconnections: %d\n", envelope.Format, envelope.KDF, envelope.KDFTime, envelope.KDFMemory, envelope.KDFParallelism, envelope.CreatedAt.Format(time.RFC3339), envelope.ConnectionCount)
		return nil
	}}
}

func backupRestoreCommand() *cobra.Command {
	var replace bool
	var only string
	command := &cobra.Command{Use: "restore PATH", Short: "Restore a portable backup, reinstall pinned CLIs, verify accounts, and resynchronize skills", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		passphrase, err := backupPassphrase(cmd, false)
		if err != nil {
			return err
		}
		return withBaseEnvironment(cmd, func(env *environment) error {
			progress := func(stage, detail string) { fmt.Fprintf(cmd.ErrOrStderr(), "%s\t%s\n", stage, detail) }
			var selected []string
			if strings.TrimSpace(only) != "" {
				selected = strings.Split(only, ",")
			}
			if err := applicationFor(env).RestoreBackupSelected(cmd.Context(), args[0], passphrase, replace, selected, progress); err != nil {
				var incomplete *application.RestoreIncompleteError
				if errors.As(err, &incomplete) && len(incomplete.Restored) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), "restored: %s\n", strings.Join(incomplete.Restored, ", "))
				}
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "backup restored; credentials were reverified and connector skills synchronized")
			return nil
		})
	}}
	command.Flags().BoolVar(&replace, "replace", false, "replace matching local connections after backup preflight")
	command.Flags().StringVar(&only, "only", "", "restore only these comma-separated connection names")
	addPassphraseFileFlag(command)
	return command
}

func addPassphraseFileFlag(command *cobra.Command) {
	command.Flags().String("passphrase-file", "", "read the backup passphrase from this file (first line) instead of OUTFIT_BACKUP_PASSPHRASE or a prompt")
}

// backupPassphrase reads the passphrase from --passphrase-file, then
// OUTFIT_BACKUP_PASSPHRASE, then an interactive prompt. It never takes the
// passphrase itself as an argument, which would expose it in process lists.
func backupPassphrase(cmd *cobra.Command, confirm bool) (string, error) {
	if path, _ := cmd.Flags().GetString("passphrase-file"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read passphrase file: %w", err)
		}
		passphrase, _, _ := strings.Cut(string(raw), "\n")
		passphrase = strings.TrimSuffix(passphrase, "\r")
		if passphrase == "" {
			return "", errors.New("passphrase file is empty")
		}
		return passphrase, nil
	}
	if passphrase := os.Getenv("OUTFIT_BACKUP_PASSPHRASE"); passphrase != "" {
		return passphrase, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", errors.New("set OUTFIT_BACKUP_PASSPHRASE or run in an interactive terminal")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "Backup passphrase: ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil || len(raw) == 0 {
		return "", errors.New("backup passphrase is required")
	}
	if confirm {
		fmt.Fprint(cmd.ErrOrStderr(), "Confirm passphrase: ")
		repeated, repeatErr := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if repeatErr != nil || string(raw) != string(repeated) {
			return "", errors.New("backup passphrases do not match")
		}
	}
	return string(raw), nil
}

func workspaceCommand() *cobra.Command {
	root := &cobra.Command{Use: "workspace", Short: "Declare, lock, approve, and apply this repository's connector environment"}
	root.AddCommand(workspaceInitCommand(), workspaceLockCommand(), workspaceApplyCommand(), workspaceStatusCommand())
	return root
}

func workspaceInitCommand() *cobra.Command {
	var name string
	command := &cobra.Command{Use: "init", Short: "Create a secret-free .outfit/workspace.yaml", RunE: func(cmd *cobra.Command, _ []string) error {
		root, err := filepath.Abs(".")
		if err != nil {
			return err
		}
		path := filepath.Join(root, ".outfit", "workspace.yaml")
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists", path)
		}
		if name == "" {
			name = filepath.Base(root)
		}
		manifest := workspace.Manifest{APIVersion: workspace.APIVersion, Name: name, Connectors: []workspace.Connector{}}
		if err := workspace.WriteManifest(path, manifest); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", path)
		return nil
	}}
	command.Flags().StringVar(&name, "name", "", "workspace name; defaults to the directory name")
	return command
}

func workspaceLockCommand() *cobra.Command {
	return &cobra.Command{Use: "lock", Short: "Resolve package digests and the exact allowed operation set", RunE: func(cmd *cobra.Command, _ []string) error {
		loaded, err := workspace.Read(".")
		if err != nil {
			return err
		}
		if loaded.Path == "" {
			return errors.New("no .outfit/workspace.yaml found")
		}
		env, err := openBaseEnvironment(cmd.Context())
		if err != nil {
			return err
		}
		defer env.state.Close()
		installed, err := workspacePackages(cmd.Context(), env)
		if err != nil {
			return err
		}
		discovered := workspaceDiscoveredOperations(cmd.Context(), env, loaded)
		locked, err := workspace.Resolve(loaded, installed, discovered)
		if err != nil {
			return err
		}
		if err := workspace.WriteLock(loaded.LockPath, locked); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "locked %d connectors and %d packages in %s\n", len(locked.Connectors), len(locked.Packages), loaded.LockPath)
		return nil
	}}
}

func workspaceDiscoveredOperations(ctx context.Context, env *environment, loaded workspace.Loaded) map[string][]string {
	result := map[string][]string{}
	for _, declared := range loaded.Manifest.SelectedConnectors() {
		connection, err := env.state.Connection(ctx, declared.Name)
		if err != nil || connection.Status != "active" || connection.ConnectorKey != declared.Package || connection.SelectedMode != declared.Mode {
			continue
		}
		tools, err := env.state.Tools(ctx, connection.ID)
		if err != nil {
			continue
		}
		for _, tool := range tools {
			result[declared.Name] = append(result[declared.Name], tool.Name)
		}
	}
	return result
}

func workspacePackages(ctx context.Context, env *environment) (map[string]kit.Loaded, error) {
	items, err := env.state.Packages(ctx)
	if err != nil {
		return nil, err
	}
	result := map[string]kit.Loaded{}
	for _, item := range items {
		result[item.Key] = kit.Loaded{Package: item.Package, Digest: item.Digest, Source: item.Source}
	}
	return result, nil
}

func workspaceApplyCommand() *cobra.Command {
	var values stringMapFlag
	var trust bool
	command := &cobra.Command{Use: "apply", Short: "Approve the lock locally and make its selected connections ready", RunE: func(cmd *cobra.Command, _ []string) error {
		loaded, err := workspace.Read(".")
		if err != nil {
			return err
		}
		if loaded.Path == "" || loaded.Lock.APIVersion == "" {
			return errors.New("workspace and lock are required; run outfit workspace init and outfit lock first")
		}
		policy, err := workspace.NewPolicy(loaded)
		if err != nil {
			return err
		}
		env, err := openBaseEnvironment(cmd.Context())
		if err != nil {
			return err
		}
		defer env.state.Close()
		if err := installWorkspacePackages(cmd.Context(), env, loaded, trust); err != nil {
			return err
		}
		env.policy = &policy
		env.service.Allowed = policy.Allows
		env.service.AllowedOperation = policy.AllowsOperation
		env.service.AllowedLinks = policy.Links
		env.service.AllowedLink = policy.AllowsLink
		env.service.AllowedConfiguration = policy.AllowsConfiguration
		for _, declared := range loaded.Lock.Connectors {
			installed, err := env.state.Package(cmd.Context(), declared.Package)
			if err != nil {
				return err
			}
			if installed.Digest != lockedPackageDigest(loaded.Lock, declared.Package) {
				return fmt.Errorf("package %s no longer matches the lock; run outfit lock and review the change", declared.Package)
			}
			mode, ok := runtime.FindMode(installed.Package, declared.Mode)
			if !ok {
				return fmt.Errorf("locked mode %s is missing", declared.Mode)
			}
			if mode.Executor.Tool != "" {
				requirement, _ := packageTool(installed.Package, mode.Executor.Tool)
				if _, resolveErr := env.tools.ResolveRequirement(requirement); resolveErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "installing locked %s %s for %s...\n", requirement.Display, requirement.Version, declared.Name)
					if _, installErr := env.tools.Install(cmd.Context(), requirement); installErr != nil {
						return installErr
					}
				}
			}
			existing, existingErr := env.state.Connection(cmd.Context(), declared.Name)
			if existingErr == nil && existing.ConnectorKey == declared.Package && existing.SelectedMode == declared.Mode && existing.CredentialMethod == declared.CredentialMethod && existing.Status == "active" {
				if modeNeedsToolRefresh(cmd.Context(), env, existing, installed.Package, declared) {
					if err := env.service.Verify(cmd.Context(), declared.Name); err != nil {
						return fmt.Errorf("refresh tools for %s: %w", declared.Name, err)
					}
				}
				continue
			}
			method, ok := kit.FindCredentialMethod(installed.Package, declared.CredentialMethod)
			if !ok || !stringSliceContains(mode.CredentialMethods, declared.CredentialMethod) {
				return fmt.Errorf("locked credential method %s is missing", declared.CredentialMethod)
			}
			fields := map[string]string{}
			for _, field := range method.Fields {
				fields[field.Key] = values[declared.Name+"."+field.Key]
			}
			if method.CapabilityField != "" && fields[method.CapabilityField] == "" {
				fields[method.CapabilityField] = strings.Join(declared.Capabilities, ",")
			}
			if err := collectFields(cmd, method.Fields, fields); err != nil {
				return fmt.Errorf("connection %s: %w", declared.Name, err)
			}
			progress := func(stage, message string) { fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", stage, message) }
			if _, err := applicationFor(env).ApplySetup(cmd.Context(), application.SetupRequest{Name: declared.Name, Connector: declared.Package, Mode: declared.Mode, CredentialMethod: declared.CredentialMethod, Fields: fields}, progress); err != nil {
				return fmt.Errorf("configure %s: %w", declared.Name, err)
			}
		}
		if err := validateWorkspaceState(cmd.Context(), env, loaded); err != nil {
			return err
		}
		if _, err := syncSkills(cmd.Context(), env, "both"); err != nil {
			return err
		}
		if err := validateWorkspaceSkills(env, loaded); err != nil {
			return err
		}
		if err := env.state.Setting(cmd.Context(), workspace.ApprovalKey(loaded.Root), policy.Digest); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "approved and applied %s (%d connections)\n", loaded.Path, len(loaded.Lock.Connectors))
		return nil
	}}
	command.Flags().Var(&values, "set", "connection.FIELD=value; repeatable and stored only in the local vault")
	command.Flags().BoolVar(&trust, "trust-packages", false, "trust package paths pinned by this reviewed lock")
	return command
}

func modeNeedsToolRefresh(ctx context.Context, env *environment, connection state.Connection, pkg kit.Package, declared workspace.LockedConnector) bool {
	mode, ok := runtime.FindMode(pkg, declared.Mode)
	if !ok || mode.Executor.Kind != "mcp" {
		return false
	}
	available := map[string]bool{}
	for _, operation := range mode.Operations {
		available[operation.Name] = true
	}
	tools, _ := env.state.Tools(ctx, connection.ID)
	for _, tool := range tools {
		available[tool.Name] = true
	}
	for _, operation := range declared.Operations {
		if !available[operation] {
			return true
		}
	}
	return false
}

func installWorkspacePackages(ctx context.Context, env *environment, loaded workspace.Loaded, trust bool) error {
	declared := map[string]string{}
	for _, item := range loaded.Manifest.Packages {
		declared[item.Key] = item.Path
	}
	for _, item := range loaded.Lock.Packages {
		current, err := env.state.Package(ctx, item.Key)
		if err == nil && current.Digest == item.Digest && current.Trust != "untrusted" {
			continue
		}
		path := declared[item.Key]
		if path == "" {
			return fmt.Errorf("locked package %s is not installed at digest %s", item.Key, item.Digest[:12])
		}
		if !trust {
			return fmt.Errorf("package %s requires local installation; review its lock and repeat with --trust-packages", item.Key)
		}
		path, err = workspace.PackagePath(loaded.Root, path)
		if err != nil {
			return err
		}
		packageValue, err := kit.Load(path)
		if err != nil {
			return err
		}
		if packageValue.Digest != item.Digest {
			return fmt.Errorf("package %s changed after locking", item.Key)
		}
		if err := env.state.InstallPackage(ctx, packageValue, "trusted"); err != nil {
			return err
		}
	}
	return nil
}

func lockedPackageDigest(value workspace.Lock, key string) string {
	for _, item := range value.Packages {
		if item.Key == key {
			return item.Digest
		}
	}
	return ""
}

func workspaceStatusCommand() *cobra.Command {
	return &cobra.Command{Use: "status", Short: "Show lock, approval, and local connection readiness", RunE: func(cmd *cobra.Command, _ []string) error {
		loaded, err := workspace.Read(".")
		if err != nil {
			return err
		}
		if loaded.Path == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "no workspace")
			return nil
		}
		env, err := openBaseEnvironment(cmd.Context())
		if err != nil {
			return err
		}
		defer env.state.Close()
		var approved string
		_ = env.state.ReadSetting(cmd.Context(), workspace.ApprovalKey(loaded.Root), &approved)
		policy, policyErr := workspace.NewPolicy(loaded)
		if policyErr != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\nlock: false\napproved: false\nerror: %v\n", loaded.Path, policyErr)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\nlock: %t\napproved: %t\n", loaded.Path, loaded.Lock.WorkspaceDigest == loaded.Digest, approved == policy.Digest)
		for _, declared := range loaded.Lock.Connectors {
			status := "missing"
			if item, err := env.state.Connection(cmd.Context(), declared.Name); err == nil {
				status = item.Status
				if item.ConnectorKey != declared.Package || item.SelectedMode != declared.Mode {
					status = "drifted"
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", declared.Name, declared.Package, declared.Mode, status)
		}
		return nil
	}}
}

func connectorCommand() *cobra.Command {
	root := &cobra.Command{Use: "connector", Short: "Install, configure, inspect, and remove connectors"}
	root.AddCommand(connectorCatalogCommand(), connectorShowCommand(), connectorImportsCommand(), connectorAdoptMCPCommand(), connectorAddCommand(), connectorRemoveCommand(), connectorTestCommand(), connectorInstallCommand())
	return root
}

func connectorImportsCommand() *cobra.Command {
	return &cobra.Command{Use: "imports", Short: "Find transferable MCP OAuth connections in Claude Code and Codex", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			snapshot, err := applicationFor(env).Snapshot(cmd.Context())
			if err != nil {
				return err
			}
			for _, warning := range snapshot.DiscoveryWarnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", warning)
			}
			if len(snapshot.Imports) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No transferable MCP OAuth connections found in Claude Code or Codex.")
				return nil
			}
			for _, item := range snapshot.Imports {
				status := "ready"
				if !item.Transferable {
					status = "reconnect-required"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", item.ID, item.Display, strings.Join(item.Hosts, "+"), status, item.Endpoint)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nTransfer one with: outfit connector adopt-mcp IMPORT_ID [--name NAME]")
			return nil
		})
	}}
}

func connectorAdoptMCPCommand() *cobra.Command {
	var name string
	command := &cobra.Command{Use: "adopt-mcp IMPORT_ID", Args: cobra.ExactArgs(1), Short: "Verify and transfer a Claude/Codex MCP OAuth connection into outfit", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			progress := func(stage, message string) { fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", stage, message) }
			configured, receipt, err := applicationFor(env).AdoptHostMCP(cmd.Context(), args[0], name, progress)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s connected through outfit; disabled the duplicate MCP in %s\nbackup: %s\nrestart affected Claude Code and Codex sessions to release an already-open MCP connection\n", configured.Name, strings.Join(receipt.Hosts, ", "), receipt.BackupPath)
			return nil
		})
	}}
	command.Flags().StringVar(&name, "name", "", "outfit connection name; defaults to the connector key")
	return command
}

func connectorCatalogCommand() *cobra.Command {
	return &cobra.Command{Use: "catalog", Short: "List installed connector packages and their modes", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			packages, err := env.state.Packages(cmd.Context())
			if err != nil {
				return err
			}
			for _, pkg := range packages {
				var modes []string
				for _, mode := range pkg.Package.Modes {
					label := mode.ID
					if mode.Recommendation.Rank == 1 {
						label += " (recommended)"
					}
					modes = append(modes, label)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", pkg.Key, pkg.Package.Display, strings.Join(modes, ", "))
			}
			return nil
		})
	}}
}

func connectorShowCommand() *cobra.Command {
	return &cobra.Command{Use: "show CONNECTOR", Args: cobra.ExactArgs(1), Short: "Show every supported mode and its from-scratch setup", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			installed, err := env.state.Package(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			renderConnectorGuide(cmd.OutOrStdout(), installed.Package)
			return nil
		})
	}}
}

func renderConnectorGuide(output io.Writer, pkg kit.Package) {
	fmt.Fprintf(output, "%s\n%s\nVersion %s · %s\n", pkg.Display, pkg.Description, pkg.Version, firstText(pkg.Category, "uncategorized"))
	if len(pkg.Limitations) > 0 {
		fmt.Fprintln(output, "\nBefore you start")
		for _, limitation := range pkg.Limitations {
			fmt.Fprintln(output, "-", limitation)
		}
	}
	for _, mode := range pkg.Modes {
		recommended := ""
		if mode.Recommendation.Rank == 1 {
			recommended = " · RECOMMENDED"
		}
		operations := fmt.Sprintf("%d declared", len(mode.Operations))
		if mode.Executor.Kind == "mcp" && len(mode.Operations) == 0 {
			operations = "discovered from the hosted server after OAuth import"
		}
		fmt.Fprintf(output, "\n%s (%s)%s\n%s\nWhy: %s\nExecutor: %s\nOperations: %s\n", mode.Label, mode.ID, recommended, mode.Description, mode.Recommendation.Reason, executorGuide(mode.Executor), operations)
		for _, methodID := range credentialMethodOrder(mode) {
			method, ok := kit.FindCredentialMethod(pkg, methodID)
			if !ok {
				continue
			}
			defaultLabel := ""
			if method.ID == mode.DefaultCredentialMethod {
				defaultLabel = " · DEFAULT"
			}
			fmt.Fprintf(output, "\n  %s (%s)%s\n  %s\n  Custody: %s\n", method.Label, method.ID, defaultLabel, method.Description, custodyGuide(method.Custody))
			if requiresImportedHostOAuth(method) {
				fmt.Fprintln(output, "  Setup in Codex:  codex mcp add "+pkg.Key+" --url "+mode.Executor.Endpoint)
				fmt.Fprintln(output, "                    codex mcp login "+pkg.Key)
				fmt.Fprintln(output, "  Or in Claude:     claude mcp add --transport http --scope user "+pkg.Key+" "+mode.Executor.Endpoint)
				fmt.Fprintln(output, "                    Start Claude, run /mcp, and finish browser sign-in.")
				fmt.Fprintln(output, "  Then:  outfit connector imports")
				fmt.Fprintln(output, "         outfit connector adopt-mcp IMPORT_ID --name "+pkg.Key)
				continue
			}
			if method.Setup != nil {
				fmt.Fprintln(output, "  Login command: "+commandGuide(*method.Setup))
			}
			for _, field := range method.Fields {
				if field.Generated {
					continue
				}
				required := "required"
				if field.Optional {
					required = "optional"
				}
				fmt.Fprintf(output, "  - %s [%s]", field.Label, required)
				if field.Default != "" {
					fmt.Fprintf(output, " · default %s", field.Default)
				}
				fmt.Fprintln(output)
				if field.Description != "" {
					fmt.Fprintln(output, "    "+field.Description)
				}
				if len(field.Environment) > 0 {
					fmt.Fprintln(output, "    Environment: "+strings.Join(field.Environment, ", ")+detectedEnvironmentSuffix(field.Environment))
				}
				if len(field.Choices) > 0 {
					choices := make([]string, 0, len(field.Choices))
					for _, choice := range field.Choices {
						choices = append(choices, firstText(choice.Label, choice.Value)+"="+choice.Value)
					}
					fmt.Fprintln(output, "    Choices: "+strings.Join(choices, ", "))
				}
			}
			fmt.Fprintf(output, "  Connect: outfit connector add %s --mode %s --credential-method %s --name %s\n", pkg.Key, mode.ID, method.ID, pkg.Key)
		}
	}
	fmt.Fprintf(output, "\nAfter setup\n- Verify: outfit connector test %s\n- Inspect: outfit ls && outfit describe %s\n- Open the guided workbench: outfit tui\n", pkg.Key, pkg.Key)
}

func credentialMethodOrder(mode kit.Mode) []string {
	result := make([]string, 0, len(mode.CredentialMethods))
	if mode.DefaultCredentialMethod != "" {
		result = append(result, mode.DefaultCredentialMethod)
	}
	for _, methodID := range mode.CredentialMethods {
		if methodID != mode.DefaultCredentialMethod {
			result = append(result, methodID)
		}
	}
	return result
}

func executorGuide(executor kit.Executor) string {
	switch executor.Kind {
	case "cli":
		if executor.Tool != "" {
			return "Outfit installs and pins the declared CLI tool automatically"
		}
		return "requires " + executor.Binary + " on PATH; its native login remains in that CLI"
	case "http":
		return "Outfit's fixed-origin HTTP runner at " + executor.BaseURL
	case "mcp":
		return "hosted MCP at " + executor.Endpoint
	default:
		return executor.Kind
	}
}

func custodyGuide(custody string) string {
	switch custody {
	case "outfit":
		return "Outfit stores the credential in the OS keychain and supplies it only to this connector"
	case "oauth":
		return "Outfit stores and refreshes the OAuth grant in the OS keychain"
	case "executor":
		return "the provider CLI owns its login; Outfit stores no provider secret"
	default:
		return custody
	}
}

func commandGuide(command kit.CommandTemplate) string {
	return strings.TrimSpace(strings.Join(append([]string{command.Program}, command.Args...), " "))
}

func detectedEnvironmentSuffix(names []string) string {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok && strings.TrimSpace(value) != "" {
			return " (" + name + " detected; value hidden)"
		}
	}
	return ""
}

func requiresImportedHostOAuth(method kit.CredentialMethod) bool {
	if method.ID != "adopted-host-oauth" {
		return false
	}
	for _, field := range method.Fields {
		if !field.Generated && !field.Optional {
			return false
		}
	}
	return true
}

func firstText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func connectorInstallCommand() *cobra.Command {
	var trust, approveUpdate bool
	command := &cobra.Command{Use: "install PATH", Args: cobra.ExactArgs(1), Short: "Validate and install a local connector package", RunE: func(cmd *cobra.Command, args []string) error {
		loaded, err := kit.Load(args[0])
		if err != nil {
			return err
		}
		return withEnvironment(cmd, func(env *environment) error {
			if env.policy != nil {
				if digest, locked := env.policy.PackageDigest(loaded.Package.Key); locked && digest != loaded.Digest {
					return errors.New("this package is pinned by the project workspace; update its declared source, run outfit lock, then outfit apply")
				}
			}
			if current, currentErr := env.state.Package(cmd.Context(), loaded.Package.Key); currentErr == nil && current.Digest != loaded.Digest {
				changes := kit.Diff(current.Package, loaded.Package)
				if !approveUpdate {
					for _, change := range changes {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s\t%s\t%s\n", change.Severity, change.Path, change.Description)
					}
					return errors.New("package changed; review the semantic diff and repeat with --approve-update")
				}
			} else if currentErr != nil && !state.IsNotFound(currentErr) {
				return currentErr
			}
			level := "untrusted"
			if trust {
				level = "trusted"
			}
			if err := env.state.InstallPackage(cmd.Context(), loaded, level); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "installed %s %s (%s)\n", loaded.Package.Key, loaded.Package.Version, level)
			return nil
		})
	}}
	command.Flags().BoolVar(&trust, "trust", false, "trust this package to execute declared local commands")
	command.Flags().BoolVar(&approveUpdate, "approve-update", false, "approve the displayed semantic and security capability changes")
	return command
}

type stringMapFlag map[string]string

func stringSliceContains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func (value *stringMapFlag) String() string { return "KEY=value" }
func (value *stringMapFlag) Type() string   { return "key=value" }
func (value *stringMapFlag) Set(raw string) error {
	key, item, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return errors.New("expected KEY=value")
	}
	if *value == nil {
		*value = stringMapFlag{}
	}
	(*value)[key] = item
	return nil
}

func connectorAddCommand() *cobra.Command {
	var name, modeID, methodID string
	var installTool bool
	var values stringMapFlag
	command := &cobra.Command{Use: "add CONNECTOR", Args: cobra.ExactArgs(1), Short: "Create or reconfigure one connection with exactly one selected mode", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			installed, err := env.state.Package(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if name == "" {
				name = installed.Key
			}
			if modeID == "" {
				for _, mode := range installed.Package.Modes {
					if mode.Recommendation.Rank == 1 {
						modeID = mode.ID
					}
				}
			}
			mode, ok := runtime.FindMode(installed.Package, modeID)
			if !ok {
				return fmt.Errorf("unknown mode %s", modeID)
			}
			if values == nil {
				values = stringMapFlag{}
			}
			if mode.Executor.Tool != "" {
				requirement, _ := packageTool(installed.Package, mode.Executor.Tool)
				if _, err := env.tools.ResolveRequirement(requirement); err != nil {
					if !installTool {
						return err
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "installing %s %s...\n", requirement.Display, requirement.Version)
					if _, installErr := env.tools.Install(cmd.Context(), requirement); installErr != nil {
						return installErr
					}
				}
			}
			if methodID == "" {
				methodID = mode.DefaultCredentialMethod
				if detected, _, ok := env.service.DetectExecutorCredential(cmd.Context(), installed.Package, mode); ok {
					methodID = detected
					fmt.Fprintf(cmd.ErrOrStderr(), "credential: found authenticated %s; importing its native login\n", detected)
				}
			}
			method, ok := kit.FindCredentialMethod(installed.Package, methodID)
			if !ok || !stringSliceContains(mode.CredentialMethods, methodID) {
				return fmt.Errorf("unknown credential method %s for mode %s", methodID, modeID)
			}
			if err := collectFields(cmd, method.Fields, values); err != nil {
				return err
			}
			progress := func(stage, message string) { fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", stage, message) }
			configured, err := applicationFor(env).ApplySetup(cmd.Context(), application.SetupRequest{Name: name, Connector: installed.Key, Mode: modeID, CredentialMethod: methodID, Fields: values}, progress)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s connected via %s with %s\n", configured.Name, configured.SelectedMode, configured.CredentialMethod)
			return nil
		})
	}}
	command.Flags().StringVar(&name, "name", "", "local connection name")
	command.Flags().StringVar(&modeID, "mode", "", "execution mode; defaults to the package recommendation")
	command.Flags().StringVar(&methodID, "credential-method", "", "credential method; defaults to the selected mode recommendation")
	command.Flags().Var(&values, "set", "credential field KEY=value; repeatable")
	command.Flags().BoolVar(&installTool, "install-tool", true, "install the exact tested provider CLI when it is missing")
	return command
}

func collectFields(cmd *cobra.Command, fields []kit.CredentialField, values map[string]string) error {
	reader := bufio.NewReader(cmd.InOrStdin())
	for _, field := range fields {
		if field.Generated {
			continue
		}
		if values[field.Key] == "" && field.Default != "" {
			values[field.Key] = field.Default
		}
		if values[field.Key] == "" {
			for _, variable := range field.Environment {
				if value, ok := os.LookupEnv(variable); ok && strings.TrimSpace(value) != "" {
					values[field.Key] = value
					fmt.Fprintf(cmd.ErrOrStderr(), "%s: using %s from the environment (value hidden)\n", field.Label, variable)
					break
				}
			}
		}
		if values[field.Key] != "" || field.Optional {
			continue
		}
		if !term.IsTerminal(int(syscall.Stdin)) {
			return fmt.Errorf("%s is required; pass --set %s=value", field.Key, field.Key)
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "%s: ", field.Label)
		if field.Secret {
			raw, err := term.ReadPassword(int(syscall.Stdin))
			fmt.Fprintln(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			values[field.Key] = string(raw)
		} else {
			raw, err := reader.ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return err
			}
			values[field.Key] = strings.TrimSpace(raw)
		}
	}
	return nil
}

func packageTool(pkg kit.Package, id string) (kit.ToolRequirement, bool) {
	for _, requirement := range pkg.Tools {
		if requirement.ID == id {
			return requirement, true
		}
	}
	return kit.ToolRequirement{}, false
}

func connectorRemoveCommand() *cobra.Command {
	return &cobra.Command{Use: "remove NAME", Args: cobra.ExactArgs(1), Short: "Remove a connection and destroy its secret", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			if err := env.service.Remove(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s removed\n", args[0])
			return nil
		})
	}}
}

func connectorTestCommand() *cobra.Command {
	return &cobra.Command{Use: "test NAME", Args: cobra.ExactArgs(1), Short: "Reverify the selected mode without changing it", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			if err := env.service.Verify(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is healthy\n", args[0])
			return nil
		})
	}}
}

func credentialCommand() *cobra.Command {
	root := &cobra.Command{Use: "credential", Aliases: []string{"credentials", "creds"}, Short: "Inspect, test, replace, and remove credentials managed by outfit"}
	root.AddCommand(credentialListCommand(), credentialTestCommand(), credentialReuseCommand(), credentialReplaceCommand(), credentialRemoveCommand())
	return root
}

func credentialReuseCommand() *cobra.Command {
	var mode string
	var values stringMapFlag
	command := &cobra.Command{Use: "reuse ID CONNECTION", Args: cobra.ExactArgs(2), Short: "Verify and attach an existing credential to another named connection", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			credential, err := env.state.Credential(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			installed, err := env.state.Package(cmd.Context(), credential.ConnectorKey)
			if err != nil {
				return err
			}
			method, ok := kit.FindCredentialMethod(installed.Package, credential.MethodID)
			if !ok {
				return fmt.Errorf("credential method %s is no longer available", credential.MethodID)
			}
			if mode == "" {
				for _, candidate := range installed.Package.Modes {
					if hasString(candidate.CredentialMethods, credential.MethodID) && (mode == "" || candidate.Recommendation.Rank < modeRank(installed.Package, mode)) {
						mode = candidate.ID
					}
				}
			}
			if mode == "" {
				return errors.New("no execution mode can use this credential")
			}
			if values == nil {
				values = stringMapFlag{}
			}
			var settings []kit.CredentialField
			for _, field := range method.Fields {
				if !field.Secret {
					settings = append(settings, field)
				}
			}
			if err := collectFields(cmd, settings, values); err != nil {
				return err
			}
			connection, err := applicationFor(env).ReuseCredential(cmd.Context(), credential.ID, args[1], mode, values)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s now reuses %s in %s mode; %d total consumer(s)\n", connection.Name, credential.ID, connection.SelectedMode, len(credential.Consumers)+1)
			return nil
		})
	}}
	command.Flags().StringVar(&mode, "mode", "", "execution mode; defaults to the highest-ranked compatible mode")
	command.Flags().Var(&values, "set", "non-secret connection setting KEY=value; repeatable")
	return command
}

func modeRank(pkg kit.Package, id string) int {
	for _, mode := range pkg.Modes {
		if mode.ID == id {
			return mode.Recommendation.Rank
		}
	}
	return int(^uint(0) >> 1)
}

func hasString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func credentialListCommand() *cobra.Command {
	return &cobra.Command{Use: "list", Aliases: []string{"ls", "status"}, Short: "List credential metadata and consumers without revealing secret values", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			credentials, err := env.state.Credentials(cmd.Context())
			if err != nil {
				return err
			}
			for _, item := range credentials {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\t%s\n", item.ID, item.DisplayName, item.ConnectorKey, item.MethodID, item.Fingerprint, strings.Join(item.Consumers, ","))
			}
			return nil
		})
	}}
}

func credentialTestCommand() *cobra.Command {
	return &cobra.Command{Use: "test ID", Args: cobra.ExactArgs(1), Short: "Verify a credential through one active consumer", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			if err := applicationFor(env).TestCredential(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is healthy\n", args[0])
			return nil
		})
	}}
}

func credentialReplaceCommand() *cobra.Command {
	var values stringMapFlag
	command := &cobra.Command{Use: "replace CONNECTION", Aliases: []string{"rotate", "reauthenticate"}, Args: cobra.ExactArgs(1), Short: "Verify a candidate and atomically replace the selected connection credential", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			connection, err := env.state.Connection(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			installed, err := env.state.Package(cmd.Context(), connection.ConnectorKey)
			if err != nil {
				return err
			}
			method, ok := kit.FindCredentialMethod(installed.Package, connection.CredentialMethod)
			if !ok {
				return fmt.Errorf("credential method %s is no longer available", connection.CredentialMethod)
			}
			if values == nil {
				values = stringMapFlag{}
			}
			if cmd.CalledAs() == "reauthenticate" && method.OAuth != nil {
				if connection.CredentialRef != "" {
					stored, readErr := env.vault.Get(cmd.Context(), connection.CredentialRef)
					if readErr != nil {
						return readErr
					}
					for key, value := range stored {
						values[key] = value
					}
				}
				for key, value := range connection.SafeIdentity {
					if strings.HasPrefix(key, "setting.") {
						values[strings.TrimPrefix(key, "setting.")] = value
					}
				}
				delete(values, "OAUTH_ACCESS_TOKEN")
				delete(values, "OAUTH_REFRESH_TOKEN")
				delete(values, "OAUTH_TOKEN_EXPIRY")
			}
			if err := collectFields(cmd, method.Fields, values); err != nil {
				return err
			}
			replaced, err := env.service.ConfigureWithMethod(cmd.Context(), connection.Name, connection.ConnectorKey, connection.SelectedMode, connection.CredentialMethod, values)
			if err != nil {
				return err
			}
			if _, err := syncSkills(cmd.Context(), env, "both"); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s credential replaced after verification; fingerprint %s\n", replaced.Name, replaced.SafeIdentity["fingerprint"])
			return nil
		})
	}}
	command.Flags().Var(&values, "set", "credential field KEY=value; repeatable")
	return command
}

func credentialRemoveCommand() *cobra.Command {
	var yes bool
	command := &cobra.Command{Use: "remove ID", Args: cobra.ExactArgs(1), Short: "Disconnect every consumer and destroy a credential", RunE: func(cmd *cobra.Command, args []string) error {
		if !yes {
			return errors.New("credential removal disconnects all consumers; repeat with --yes")
		}
		return withEnvironment(cmd, func(env *environment) error {
			if err := applicationFor(env).RemoveCredential(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s removed\n", args[0])
			return nil
		})
	}}
	command.Flags().BoolVar(&yes, "yes", false, "confirm removal of the credential and all connection consumers")
	return command
}

func listCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "ls", Short: "List connections and selected-mode operations", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			connections, err := env.state.Connections(cmd.Context())
			if err != nil {
				return err
			}
			items := make([]map[string]any, 0, len(connections))
			for _, item := range connections {
				if !env.service.Allows(item.Name) {
					continue
				}
				installed, _, operations := selectedOperations(cmd.Context(), env, item)
				items = append(items, map[string]any{"name": item.Name, "connector": item.ConnectorKey, "display": installed.Package.Display, "mode": item.SelectedMode, "status": item.Status, "operations": len(operations)})
				if !jsonOutput && !agentio.Enabled(cmd.OutOrStdout()) {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%d operations\n", item.Name, installed.Package.Display, item.SelectedMode, item.Status, len(operations))
				}
			}
			if jsonOutput || agentio.Enabled(cmd.OutOrStdout()) {
				return renderInvocationEnvelope(cmd, invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "ls", Class: "read", Data: items, Meta: invoke.Meta{Pages: 1, Items: len(items)}})
			}
			return nil
		})
	}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "return the stable agent result envelope")
	return command
}

func searchCommand() *cobra.Command {
	var cached, includeExpired, jsonOutput bool
	var connectionName string
	var limit int
	command := &cobra.Command{Use: "search WORDS...", Args: cobra.MinimumNArgs(1), Short: "Search selected-mode operations or the opt-in local knowledge cache", RunE: func(cmd *cobra.Command, args []string) error {
		query := strings.ToLower(strings.Join(args, " "))
		return withEnvironment(cmd, func(env *environment) error {
			if cached {
				var config state.CacheConfig
				if err := env.state.ReadSetting(cmd.Context(), "cache.config", &config); err != nil || !config.Enabled {
					return errors.New("local knowledge cache is disabled; run outfit cache enable")
				}
				if connectionName != "" && !env.service.Allows(connectionName) {
					return fmt.Errorf("connection %s is not enabled by this project", connectionName)
				}
				items, err := env.state.SearchContexts(cmd.Context(), query, connectionName, time.Now(), includeExpired, 100)
				if err != nil {
					return err
				}
				shown := 0
				results := make([]map[string]any, 0)
				for _, item := range items {
					if !env.service.Allows(item.ConnectionName) || (item.Operation != "" && !env.service.AllowsOperation(item.ConnectionName, item.Operation)) {
						continue
					}
					freshness := "fresh"
					if time.Now().After(item.ExpiresAt) {
						freshness = "expired"
					}
					results = append(results, map[string]any{"handle": item.Handle, "connection": item.ConnectionName, "operation": item.Operation, "freshness": freshness, "title": item.Title})
					if !jsonOutput && !agentio.Enabled(cmd.OutOrStdout()) {
						fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", item.Handle, item.ConnectionName, item.Operation, freshness, item.Title)
					}
					shown++
					if limit > 0 && shown >= limit {
						break
					}
				}
				if jsonOutput || agentio.Enabled(cmd.OutOrStdout()) {
					return renderInvocationEnvelope(cmd, invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "search_cached", Class: "read", Data: results, Meta: invoke.Meta{Pages: 1, Items: len(results), Truncated: limit > 0 && len(items) > len(results)}})
				}
				return nil
			}
			connections, err := env.state.Connections(cmd.Context())
			if err != nil {
				return err
			}
			results := make([]map[string]any, 0)
			for _, item := range connections {
				if item.Status != "active" || !env.service.Allows(item.Name) {
					continue
				}
				installed, mode, operations := selectedOperations(cmd.Context(), env, item)
				for _, route := range selectedLinkRoutes(installed.Package, mode, operations) {
					haystack := strings.ToLower(strings.Join([]string{item.Name, item.ConnectorKey, installed.Package.Display, route.ID, route.ResourceType, route.UnderlyingOperation, "read pasted provider url link with heavy context"}, " "))
					if searchWordsMatch(haystack, query) {
						results = append(results, map[string]any{"connection": item.Name, "connector": item.ConnectorKey, "operation": "get_url", "underlying_operation": route.UnderlyingOperation, "class": "read", "description": "Read a supported " + route.ResourceType + " URL through the selected mode with configured context depth.", "command": "outfit get URL"})
						if !jsonOutput && !agentio.Enabled(cmd.OutOrStdout()) {
							fmt.Fprintf(cmd.OutOrStdout(), "%s\tget_url\tread\t%s links via outfit get URL\n", item.Name, route.ResourceType)
						}
						if limit > 0 && len(results) >= limit {
							break
						}
					}
				}
				if limit > 0 && len(results) >= limit {
					break
				}
				for _, operation := range operations {
					haystack := strings.ToLower(operation.Name + " " + operation.Title + " " + operation.Description)
					if searchWordsMatch(haystack, query) {
						results = append(results, map[string]any{"connection": item.Name, "connector": item.ConnectorKey, "operation": operation.Name, "class": operation.Class, "description": operation.Description, "command": fmt.Sprintf("outfit %s %s --help", item.Name, invoke.FriendlyOperationName(item.ConnectorKey, operation.Name))})
						if !jsonOutput && !agentio.Enabled(cmd.OutOrStdout()) {
							fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", item.Name, operation.Name, operation.Class, operation.Description)
						}
						if limit > 0 && len(results) >= limit {
							break
						}
					}
				}
				if limit > 0 && len(results) >= limit {
					break
				}
			}
			if jsonOutput || agentio.Enabled(cmd.OutOrStdout()) {
				return renderInvocationEnvelope(cmd, invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "search", Class: "read", Data: results, Meta: invoke.Meta{Pages: 1, Items: len(results), Truncated: limit > 0 && len(results) >= limit}})
			}
			return nil
		})
	}}
	command.Flags().BoolVar(&cached, "cached", false, "search previously fetched resource content with local SQLite FTS")
	command.Flags().BoolVar(&includeExpired, "include-expired", false, "include expired cached evidence")
	command.Flags().StringVar(&connectionName, "connection", "", "limit cached results to one connection")
	command.Flags().IntVar(&limit, "limit", 20, "maximum results")
	command.Flags().BoolVar(&jsonOutput, "json", false, "return the stable agent result envelope")
	return command
}

func searchWordsMatch(haystack, query string) bool {
	haystack, query = strings.ToLower(haystack), strings.ToLower(strings.TrimSpace(query))
	if query == "" || strings.Contains(haystack, query) {
		return true
	}
	for _, word := range strings.Fields(query) {
		if !strings.Contains(haystack, word) {
			return false
		}
	}
	return true
}

func cacheCommand() *cobra.Command {
	root := &cobra.Command{Use: "cache", Short: "Control the opt-in local searchable resource cache"}
	var ttl time.Duration
	enable := &cobra.Command{Use: "enable", Short: "Cache successful read results locally with provenance and TTL", RunE: func(cmd *cobra.Command, _ []string) error {
		if ttl < time.Minute || ttl > 30*24*time.Hour {
			return errors.New("TTL must be between 1m and 720h")
		}
		return withEnvironment(cmd, func(env *environment) error {
			if err := env.state.Setting(cmd.Context(), "cache.config", state.CacheConfig{Enabled: true, TTLSeconds: int(ttl.Seconds())}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "local knowledge cache enabled with TTL %s\n", ttl)
			return nil
		})
	}}
	enable.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "expiration for operation results")
	root.AddCommand(enable)
	root.AddCommand(&cobra.Command{Use: "disable", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			var config state.CacheConfig
			_ = env.state.ReadSetting(cmd.Context(), "cache.config", &config)
			config.Enabled = false
			if err := env.state.Setting(cmd.Context(), "cache.config", config); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "local knowledge cache disabled; existing entries remain local until purged")
			return nil
		})
	}})
	root.AddCommand(&cobra.Command{Use: "status", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			var config state.CacheConfig
			if err := env.state.ReadSetting(cmd.Context(), "cache.config", &config); err != nil && !state.IsNotFound(err) {
				return err
			}
			count, err := env.state.CountContexts(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enabled: %t\nttl: %s\nentries: %d\n", config.Enabled, time.Duration(config.TTLSeconds)*time.Second, count)
			return nil
		})
	}})
	var purgeAll bool
	purge := &cobra.Command{Use: "purge", Short: "Delete expired cached resources, or every entry with --all", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			var count int64
			var err error
			if purgeAll {
				count, err = env.state.PurgeAllContexts(cmd.Context())
			} else {
				count, err = env.state.PurgeExpiredContexts(cmd.Context(), time.Now())
			}
			if err != nil {
				return err
			}
			kind := "expired"
			if purgeAll {
				kind = "cached"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "purged %d %s entries\n", count, kind)
			return nil
		})
	}}
	purge.Flags().BoolVar(&purgeAll, "all", false, "delete every cached resource immediately")
	root.AddCommand(purge)
	return root
}

func describeCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "describe CONNECTION [OPERATION]", Args: cobra.RangeArgs(1, 2), Short: "Describe a selected mode or operation", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			item, err := env.state.Connection(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			installed, mode, operations := selectedOperations(cmd.Context(), env, item)
			linkRoutes := selectedLinkRoutes(installed.Package, mode, operations)
			if len(args) == 1 {
				if jsonOutput || agentio.Enabled(cmd.OutOrStdout()) {
					data := map[string]any{"display": installed.Package.Display, "description": installed.Package.Description, "identity": item.SafeIdentity, "operations": operations, "link_routes": linkRoutes}
					return renderInvocationEnvelope(cmd, invoke.Envelope{Schema: invoke.Schema, OK: true, Connection: item.Name, Connector: installed.Key, Mode: mode.ID, Operation: "describe", Class: "read", Data: data, Meta: invoke.Meta{Pages: 1, Items: len(operations)}})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s — %s\nmode: %s (%s)\n", installed.Package.Display, installed.Package.Description, mode.Label, mode.Description)
				for _, key := range []string{"account_name", "account_id", "principal", "environment", "capabilities", "schema_digest"} {
					if value := item.SafeIdentity[key]; value != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", key, value)
					}
				}
				if len(linkRoutes) > 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "preferred pasted-link route: outfit get URL (configured context depth; heavy by default)")
					for _, route := range linkRoutes {
						fmt.Fprintf(cmd.OutOrStdout(), "  %s\tget_url\tunderlying %s\n", route.ResourceType, route.UnderlyingOperation)
					}
				}
				for _, op := range operations {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\t%s\t%s\n", op.Name, op.Class, op.Description)
				}
				return nil
			}
			var op kit.Operation
			ok := false
			for _, candidate := range operations {
				if candidate.Name == args[1] {
					op, ok = candidate, true
					break
				}
			}
			if !ok {
				return fmt.Errorf("unknown operation %s", args[1])
			}
			if jsonOutput || agentio.Enabled(cmd.OutOrStdout()) {
				return renderInvocationEnvelope(cmd, invoke.Envelope{Schema: invoke.Schema, OK: true, Connection: item.Name, Connector: installed.Key, Mode: mode.ID, Operation: "describe", Class: "read", Data: op, Meta: invoke.Meta{Pages: 1, Items: 1}})
			}
			encoded, _ := json.MarshalIndent(op, "", "  ")
			fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return nil
		})
	}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "return the stable agent result envelope")
	return command
}

type linkRouteDescription struct {
	ID                  string   `json:"id"`
	ResourceType        string   `json:"resource_type"`
	Operation           string   `json:"operation"`
	UnderlyingOperation string   `json:"underlying_operation"`
	Command             string   `json:"command"`
	DefaultDepth        string   `json:"default_depth"`
	Hosts               []string `json:"hosts,omitempty"`
}

func selectedLinkRoutes(pkg kit.Package, mode kit.Mode, operations []kit.Operation) []linkRouteDescription {
	allowed := map[string]bool{}
	for _, operation := range operations {
		allowed[operation.Name] = true
	}
	var routes []linkRouteDescription
	for _, resolver := range pkg.Links {
		for _, lookup := range resolver.LookupOptions() {
			if (lookup.Mode != "" && lookup.Mode != mode.ID) || !allowed[lookup.Operation] {
				continue
			}
			hostSet := map[string]bool{}
			var hosts []string
			for _, pattern := range resolver.Patterns {
				for _, host := range pattern.Hosts {
					if !hostSet[host] {
						hostSet[host] = true
						hosts = append(hosts, host)
					}
				}
			}
			sort.Strings(hosts)
			routes = append(routes, linkRouteDescription{ID: resolver.ID, ResourceType: resolver.ResourceType, Operation: "get_url", UnderlyingOperation: lookup.Operation, Command: "outfit get URL", DefaultDepth: "heavy", Hosts: hosts})
			break
		}
	}
	return routes
}

func runCommand(write bool) *cobra.Command {
	verb, short := "run", "Run a read operation"
	if write {
		verb, short = "write", "Run an explicitly requested mutating operation"
	}
	var values stringMapFlag
	var dryRun, jsonOutput bool
	command := &cobra.Command{Use: verb + " CONNECTION OPERATION [--ARG VALUE ...]", Args: cobra.MinimumNArgs(2), Short: short, RunE: func(cmd *cobra.Command, args []string) error {
		initial := operationCLIOptions{Values: values, DryRun: dryRun, JSON: jsonOutput}
		return executeOperationCommand(cmd, args[0], args[1], args[2:], write, initial)
	}}
	command.Flags().Var(&values, "arg", "operation argument KEY=value; repeatable")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "validate and show the exact execution plan without reading a credential or calling the provider")
	command.Flags().BoolVar(&jsonOutput, "json", false, "print the dry-run plan as JSON")
	command.Flags().SetInterspersed(false)
	return command
}

type operationCLIOptions struct {
	Values             stringMapFlag
	DryRun, JSON, All  bool
	AllowWrite         bool
	Input, JSONArgs    string
	MaxPages, MaxItems int
}

func executeOperationCommand(cmd *cobra.Command, connectionName, operationName string, tokens []string, requestedWrite bool, options operationCLIOptions) error {
	options.JSON = options.JSON || agentio.Enabled(cmd.OutOrStdout())
	return withEnvironment(cmd, func(env *environment) error {
		item, err := env.state.Connection(cmd.Context(), connectionName)
		if err != nil {
			return fmt.Errorf("connection %s was not found; run `outfit ls` to list active names: %w", connectionName, err)
		}
		installed, mode, operations := selectedOperations(cmd.Context(), env, item)
		operation, ok := findSelectedOperationAlias(installed.Key, operations, operationName)
		if !ok {
			return unknownOperationError(connectionName, operationName, installed.Key, operations)
		}
		showHelp, err := parseInvocationArguments(tokens, operation.Input, &options)
		if err != nil {
			return invalidOperationArgumentsError(connectionName, installed.Key, operation, err)
		}
		if showHelp {
			renderOperationHelp(cmd.OutOrStdout(), connectionName, installed.Key, operation, requestedWrite || options.AllowWrite)
			return nil
		}
		if err := loadInvocationInput(cmd, &options); err != nil {
			return invalidOperationArgumentsError(connectionName, installed.Key, operation, err)
		}
		allowWrite := requestedWrite || options.AllowWrite
		if operation.Class != "read" && !allowWrite {
			friendly := invoke.FriendlyOperationName(installed.Key, operation.Name)
			return fmt.Errorf("[%s] %s is a %s operation; run `outfit %s %s --write` or `outfit write %s %s` only when the human requested this mutation", "write_confirmation_required", operation.Name, operation.Class, connectionName, friendly, connectionName, operation.Name)
		}
		if err := invoke.ValidateBounds(options.MaxPages, options.MaxItems); err != nil {
			return invalidOperationArgumentsError(connectionName, installed.Key, operation, err)
		}
		if options.DryRun {
			plan, err := env.service.Plan(cmd.Context(), connectionName, operation.Name, options.Values, allowWrite)
			if err != nil {
				return err
			}
			if !options.JSON {
				return renderExecutionPlan(cmd, plan, false)
			}
			envelope := invoke.Envelope{Schema: invoke.Schema, OK: true, Connection: connectionName, Connector: installed.Key, Mode: mode.ID, Operation: operation.Name, Class: operation.Class, Data: plan, Meta: invoke.Meta{Pages: 0}}
			return renderInvocationEnvelope(cmd, envelope)
		}

		metadata := invoke.Metadata{Connection: connectionName, Connector: installed.Key, Mode: mode.ID}
		envelope, runErr := invoke.Run(cmd.Context(), metadata, operation, options.Values, invoke.Options{All: options.All, MaxPages: options.MaxPages, MaxItems: options.MaxItems}, func(ctx context.Context, arguments map[string]string) (runtime.Result, error) {
			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			var capturedOut, capturedErr bytes.Buffer
			if options.JSON {
				stdout, stderr = &capturedOut, &capturedErr
			}
			result, callErr := env.service.RunWithIO(ctx, connectionName, operation.Name, arguments, allowWrite, stdout, stderr)
			if options.JSON && (capturedOut.Len() > 0 || capturedErr.Len() > 0) {
				result.Data = map[string]any{"stdout": capturedOut.String(), "stderr": capturedErr.String()}
				result.Raw = nil
			}
			return result, callErr
		})
		if envelope.Error != nil && envelope.Error.Correction == "" {
			envelope.Error.Correction = operationHelpCommand(connectionName, installed.Key, operation)
		}
		if options.JSON {
			if err := renderInvocationEnvelope(cmd, envelope); err != nil {
				return err
			}
			return agentio.Reported(runErr)
		}
		if runErr != nil {
			failure := invoke.Classify(runErr, "", "")
			if failure.Hint != "" {
				return fmt.Errorf("[%s] %s\nTry: %s", failure.Code, failure.Message, failure.Hint)
			}
			return fmt.Errorf("[%s] %s", failure.Code, failure.Message)
		}
		return renderInvocationData(cmd.OutOrStdout(), envelope.Data)
	})
}

func operationHelpCommand(connectionName, connector string, operation kit.Operation) string {
	return fmt.Sprintf("outfit %s %s --help", connectionName, invoke.FriendlyOperationName(connector, operation.Name))
}

func invalidOperationArgumentsError(connectionName, connector string, operation kit.Operation, err error) error {
	return fmt.Errorf("[invalid_arguments] %s\nTry: %s", strings.TrimSpace(err.Error()), operationHelpCommand(connectionName, connector, operation))
}

func renderInvocationEnvelope(cmd *cobra.Command, envelope invoke.Envelope) error {
	return agentio.Render(cmd.OutOrStdout(), envelope, agentio.Enabled(cmd.OutOrStdout()))
}

func renderInvocationData(output io.Writer, data any) error {
	if data == nil {
		return nil
	}
	if text, ok := data.(string); ok {
		fmt.Fprintln(output, text)
		return nil
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintln(output, string(encoded))
	return nil
}

func getCommand() *cobra.Command {
	var modeText string
	var jsonOutput bool
	var connectionName string
	command := &cobra.Command{Use: "get URL...", Args: cobra.MinimumNArgs(1), Short: "Read supported provider links through the active matching connection", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			mode := linkcontext.Mode("")
			if modeText != "" {
				parsed, err := linkcontext.ParseMode(modeText)
				if err != nil || parsed == linkcontext.ModeOff {
					return errors.New("get mode must be heavy or light")
				}
				mode = parsed
			}
			service := linkcontext.Service{State: env.state, Connections: env.service, Preferences: env.linkPreferences}
			prompt := strings.Join(args, "\n")
			resolution := service.ResolveDetailed(cmd.Context(), prompt, linkcontext.ResolveOptions{Mode: mode, Connection: connectionName})
			if len(resolution.Resources) == 0 {
				if len(resolution.Diagnostics) > 0 {
					diagnostic := resolution.Diagnostics[0]
					return fmt.Errorf("[%s] %s\nTry: %s", diagnostic.Code, diagnostic.Message, diagnostic.Hint)
				}
				return errors.New("[link_not_resolved] no URL was found in the input")
			}
			if jsonOutput || agentio.Enabled(cmd.OutOrStdout()) {
				envelope := invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "get_url", Class: "read", Data: resolution.Resources, Meta: invoke.Meta{Pages: 1, Items: len(resolution.Resources)}}
				return renderInvocationEnvelope(cmd, envelope)
			}
			fmt.Fprintln(cmd.OutOrStdout(), linkcontext.Render(resolution.Resources))
			return nil
		})
	}}
	command.Flags().StringVar(&modeText, "mode", "", "one-shot context depth: heavy or light; defaults to the connection preference")
	command.Flags().StringVar(&connectionName, "connection", "", "use one named connection when multiple accounts can resolve the URL")
	command.Flags().BoolVar(&jsonOutput, "json", false, "return the stable agent result envelope")
	return command
}

func workflowCommand() *cobra.Command {
	root := &cobra.Command{Use: "workflow", Aliases: []string{"workflows", "recipe", "recipes"}, Short: "List and render connector-authored workflow recipes"}
	root.AddCommand(&cobra.Command{Use: "list [CONNECTION]", Args: cobra.MaximumNArgs(1), Short: "List workflows available through active selected modes", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			connections, err := env.state.Connections(cmd.Context())
			if err != nil {
				return err
			}
			for _, item := range connections {
				if item.Status != "active" || !env.service.Allows(item.Name) || (len(args) == 1 && item.Name != args[0]) {
					continue
				}
				installed, _, _ := selectedOperations(cmd.Context(), env, item)
				for _, workflow := range installed.Package.Skill.Workflows {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.Name, workflowSlug(workflow.Title), workflow.Title)
				}
			}
			return nil
		})
	}})
	var jsonOutput bool
	show := &cobra.Command{Use: "show CONNECTION [WORKFLOW]", Args: cobra.RangeArgs(1, 2), Short: "Render one or every workflow with exact CLI and MCP entry points", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			item, err := env.state.Connection(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			installed, mode, operations := selectedOperations(cmd.Context(), env, item)
			selected := installed.Package.Skill.Workflows
			if len(args) == 2 {
				selected = nil
				for _, workflow := range installed.Package.Skill.Workflows {
					if workflowSlug(workflow.Title) == workflowSlug(args[1]) {
						selected = append(selected, workflow)
					}
				}
			}
			if len(selected) == 0 {
				return fmt.Errorf("no matching workflow; run `outfit workflow list %s`", item.Name)
			}
			if jsonOutput {
				return renderInvocationEnvelope(cmd, invoke.Envelope{Schema: invoke.Schema, OK: true, Connection: item.Name, Connector: installed.Key, Mode: mode.ID, Operation: "workflow", Class: "read", Data: selected, Meta: invoke.Meta{Pages: 1, Items: len(selected)}})
			}
			for _, workflow := range selected {
				renderWorkflow(cmd.OutOrStdout(), item.Name, installed.Key, workflow, operations)
			}
			return nil
		})
	}}
	show.Flags().BoolVar(&jsonOutput, "json", false, "return the stable agent result envelope")
	root.AddCommand(show)
	return root
}

func workflowSlug(title string) string {
	var output strings.Builder
	lastDash := false
	for _, char := range strings.ToLower(title) {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' {
			output.WriteRune(char)
			lastDash = false
		} else if !lastDash && output.Len() > 0 {
			output.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(output.String(), "-")
}

func renderWorkflow(output io.Writer, connectionName, connector string, workflow kit.SkillWorkflow, operations []kit.Operation) {
	fmt.Fprintf(output, "%s (%s)\n", workflow.Title, workflowSlug(workflow.Title))
	for index, step := range workflow.Steps {
		fmt.Fprintf(output, "%d. %s\n", index+1, step)
	}
	query := strings.ToLower(workflow.Title + " " + strings.Join(workflow.Steps, " "))
	var matched []kit.Operation
	for _, operation := range operations {
		words := strings.Fields(strings.NewReplacer("_", " ", "-", " ").Replace(strings.ToLower(operation.Name + " " + operation.Title)))
		for _, word := range words {
			if len(word) >= 4 && strings.Contains(query, word) {
				matched = append(matched, operation)
				break
			}
		}
	}
	if len(matched) == 0 && len(operations) <= 8 {
		matched = operations
	}
	if len(matched) > 0 {
		fmt.Fprintln(output, "Entry points:")
		for _, operation := range matched {
			friendly := invoke.FriendlyOperationName(connector, operation.Name)
			suffix := ""
			if operation.Class != "read" {
				suffix = " --write"
			}
			fmt.Fprintf(output, "- CLI: outfit %s %s --help%s\n", connectionName, friendly, suffix)
		}
	}
	fmt.Fprintln(output)
}

func findSelectedOperationAlias(connector string, operations []kit.Operation, name string) (kit.Operation, bool) {
	normalized := strings.ReplaceAll(strings.TrimSpace(name), "-", "_")
	var matched []kit.Operation
	for _, operation := range operations {
		if operation.Name == name || operation.Name == normalized || strings.ReplaceAll(invoke.FriendlyOperationName(connector, operation.Name), "-", "_") == normalized {
			matched = append(matched, operation)
		}
	}
	if len(matched) == 1 {
		return matched[0], true
	}
	return kit.Operation{}, false
}

func unknownOperationError(connectionName, requested, connector string, operations []kit.Operation) error {
	available := make([]string, 0, len(operations))
	for _, operation := range operations {
		available = append(available, invoke.FriendlyOperationName(connector, operation.Name))
	}
	sort.Strings(available)
	return fmt.Errorf("[unknown_operation] %s has no operation %q; available short operations: %s\nTry: outfit describe %s", connectionName, requested, strings.Join(available, ", "), connectionName)
}

func parseOperationArguments(tokens []string, schema map[string]any, values *stringMapFlag, dryRun, jsonOutput *bool) (bool, error) {
	options := operationCLIOptions{Values: *values, DryRun: *dryRun, JSON: *jsonOutput}
	help, err := parseInvocationArguments(tokens, schema, &options)
	*values, *dryRun, *jsonOutput = options.Values, options.DryRun, options.JSON
	return help, err
}

func parseInvocationArguments(tokens []string, schema map[string]any, options *operationCLIOptions) (bool, error) {
	if options.Values == nil {
		options.Values = stringMapFlag{}
	}
	properties, _ := schema["properties"].(map[string]any)
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		switch token {
		case "-h", "--help":
			return true, nil
		case "--dry-run":
			options.DryRun = true
			continue
		case "--json":
			options.JSON = true
			continue
		case "--all":
			options.All = true
			continue
		case "--write":
			options.AllowWrite = true
			continue
		case "--arg", "--input", "--json-args", "--max-pages", "--max-items":
			index++
			if index >= len(tokens) {
				return false, fmt.Errorf("%s requires a value", token)
			}
			switch token {
			case "--arg":
				if err := setOperationArgument(options.Values, properties, tokens[index]); err != nil {
					return false, fmt.Errorf("--arg: %w", err)
				}
			case "--input":
				options.Input = tokens[index]
			case "--json-args":
				options.JSONArgs = tokens[index]
			case "--max-pages":
				value, err := strconv.Atoi(tokens[index])
				if err != nil {
					return false, errors.New("--max-pages needs an integer")
				}
				options.MaxPages = value
			case "--max-items":
				value, err := strconv.Atoi(tokens[index])
				if err != nil {
					return false, errors.New("--max-items needs an integer")
				}
				options.MaxItems = value
			}
			continue
		}
		if raw, ok := strings.CutPrefix(token, "--arg="); ok {
			if err := setOperationArgument(options.Values, properties, raw); err != nil {
				return false, fmt.Errorf("--arg: %w", err)
			}
			continue
		}
		for prefix, target := range map[string]*string{"--input=": &options.Input, "--json-args=": &options.JSONArgs} {
			if raw, ok := strings.CutPrefix(token, prefix); ok {
				*target = raw
				goto nextToken
			}
		}
		for prefix, target := range map[string]*int{"--max-pages=": &options.MaxPages, "--max-items=": &options.MaxItems} {
			if raw, ok := strings.CutPrefix(token, prefix); ok {
				value, err := strconv.Atoi(raw)
				if err != nil {
					return false, fmt.Errorf("%s needs an integer", strings.TrimSuffix(prefix, "="))
				}
				*target = value
				goto nextToken
			}
		}
		if strings.HasPrefix(token, "--") {
			flag, inline, hasInline := strings.Cut(strings.TrimPrefix(token, "--"), "=")
			key, err := resolveOperationFlag(flag, properties)
			if err != nil {
				return false, err
			}
			value := inline
			if !hasInline {
				if operationPropertyType(properties[key]) == "boolean" && (index+1 >= len(tokens) || strings.HasPrefix(tokens[index+1], "--")) {
					value = "true"
				} else {
					index++
					if index >= len(tokens) {
						return false, fmt.Errorf("--%s requires a value", flag)
					}
					value = tokens[index]
				}
			}
			options.Values[key] = value
			continue
		}
		if strings.Contains(token, "=") {
			if err := setOperationArgument(options.Values, properties, token); err != nil {
				return false, err
			}
			continue
		}
		if key, ok := soleMissingRequiredArgument(schema, options.Values); ok {
			options.Values[key] = token
			continue
		}
		return false, fmt.Errorf("unexpected argument %q; use --help to see this operation's arguments", token)
	nextToken:
	}
	return false, nil
}

func setOperationArgument(values stringMapFlag, properties map[string]any, raw string) error {
	key, value, ok := strings.Cut(raw, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return errors.New("expected KEY=value")
	}
	resolved, err := resolveOperationFlag(key, properties)
	if err != nil {
		return err
	}
	values[resolved] = value
	return nil
}

func soleMissingRequiredArgument(schema map[string]any, values stringMapFlag) (string, bool) {
	required := requiredArgumentNames(schema["required"])
	var missing []string
	for _, key := range required {
		if _, ok := values[key]; !ok {
			missing = append(missing, key)
		}
	}
	returnValue := ""
	if len(missing) == 1 {
		returnValue = missing[0]
	}
	return returnValue, len(missing) == 1
}

func requiredArgumentNames(value any) []string {
	switch required := value.(type) {
	case []string:
		return required
	case []any:
		result := make([]string, 0, len(required))
		for _, item := range required {
			if name, ok := item.(string); ok {
				result = append(result, name)
			}
		}
		return result
	default:
		return nil
	}
}

func loadInvocationInput(cmd *cobra.Command, options *operationCLIOptions) error {
	fromJSON := stringMapFlag{}
	if options.Input != "" {
		var raw []byte
		var err error
		if options.Input == "-" {
			raw, err = io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
		} else {
			raw, err = os.ReadFile(options.Input)
		}
		if err != nil {
			return fmt.Errorf("read operation input: %w", err)
		}
		if err := decodeInvocationJSON(raw, fromJSON); err != nil {
			return fmt.Errorf("operation input: %w", err)
		}
	}
	if options.JSONArgs != "" {
		if err := decodeInvocationJSON([]byte(options.JSONArgs), fromJSON); err != nil {
			return fmt.Errorf("--json-args: %w", err)
		}
	}
	for key, value := range options.Values {
		fromJSON[key] = value
	}
	options.Values = fromJSON
	return nil
}

func decodeInvocationJSON(raw []byte, target stringMapFlag) error {
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return errors.New("expected one JSON object: " + err.Error())
	}
	for key, value := range values {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			target[key] = text
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("argument %s: %w", key, err)
		}
		target[key] = string(encoded)
	}
	return nil
}

func resolveOperationFlag(flag string, properties map[string]any) (string, error) {
	normalized := strings.ReplaceAll(strings.TrimSpace(flag), "-", "_")
	matches := kit.MatchArgumentNames(properties, normalized)
	if len(matches) == 1 {
		return matches[0], nil
	}
	available := make([]string, 0, len(properties))
	for key := range properties {
		available = append(available, "--"+strings.ReplaceAll(key, "_", "-"))
	}
	sort.Strings(available)
	if len(matches) > 1 {
		return "", fmt.Errorf("--%s is ambiguous; use one of %s", flag, strings.Join(matches, ", "))
	}
	suggestion := closestOperationFlag(normalized, properties)
	if suggestion != "" {
		return "", fmt.Errorf("unknown flag --%s; did you mean --%s? Available operation flags: %s (or use --arg KEY=value)", flag, strings.ReplaceAll(suggestion, "_", "-"), strings.Join(available, ", "))
	}
	return "", fmt.Errorf("unknown flag --%s; available operation flags: %s (or use --arg KEY=value)", flag, strings.Join(available, ", "))
}

func closestOperationFlag(requested string, properties map[string]any) string {
	best, bestDistance := "", 4
	for candidate := range properties {
		distance := editDistance(requested, candidate)
		if distance < bestDistance || distance == bestDistance && candidate < best {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

func editDistance(left, right string) int {
	previous := make([]int, len(right)+1)
	for index := range previous {
		previous[index] = index
	}
	for leftIndex, leftRune := range []rune(left) {
		current := make([]int, len(previous))
		current[0] = leftIndex + 1
		for rightIndex, rightRune := range []rune(right) {
			cost := 0
			if leftRune != rightRune {
				cost = 1
			}
			current[rightIndex+1] = min(current[rightIndex]+1, previous[rightIndex+1]+1, previous[rightIndex]+cost)
		}
		previous = current
	}
	return previous[len(previous)-1]
}

func operationPropertyType(value any) string {
	property, _ := value.(map[string]any)
	kind, _ := property["type"].(string)
	return kind
}

func renderOperationHelp(output io.Writer, connection, connector string, operation kit.Operation, write bool) {
	verb := "run"
	if write {
		verb = "write"
	}
	fmt.Fprintf(output, "%s\n\nUsage:\n  outfit %s %s %s [operation flags]\n", operation.Description, verb, connection, operation.Name)
	friendly := invoke.FriendlyOperationName(connector, operation.Name)
	fmt.Fprintf(output, "  outfit %s %s [operation flags]", connection, friendly)
	if operation.Class != "read" {
		fmt.Fprint(output, " --write")
	}
	fmt.Fprintln(output)
	properties, _ := operation.Input["properties"].(map[string]any)
	required := map[string]bool{}
	switch requiredValues := operation.Input["required"].(type) {
	case []any:
		for _, value := range requiredValues {
			if key, ok := value.(string); ok {
				required[key] = true
			}
		}
	case []string:
		for _, key := range requiredValues {
			required[key] = true
		}
	}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		fmt.Fprintln(output, "\nOperation flags:")
	}
	for _, key := range keys {
		property, _ := properties[key].(map[string]any)
		description, _ := property["description"].(string)
		marker := ""
		if required[key] {
			marker = " (required)"
		}
		details := []string{}
		if aliases, ok := property["x-outfit-aliases"].([]string); ok && len(aliases) > 0 {
			details = append(details, "aliases: --"+strings.Join(aliases, ", --"))
		} else if aliases, ok := property["x-outfit-aliases"].([]any); ok && len(aliases) > 0 {
			var values []string
			for _, alias := range aliases {
				if value, ok := alias.(string); ok {
					values = append(values, value)
				}
			}
			details = append(details, "aliases: --"+strings.Join(values, ", --"))
		}
		if value, ok := property["default"]; ok {
			details = append(details, fmt.Sprintf("default: %v", value))
		} else if values, ok := property["examples"].([]any); ok && len(values) > 0 {
			details = append(details, fmt.Sprintf("example: %v", values[0]))
		}
		if values, ok := property["enum"].([]any); ok && len(values) > 0 {
			details = append(details, fmt.Sprintf("one of: %v", values))
		}
		if len(details) > 0 {
			description += " [" + strings.Join(details, "; ") + "]"
		}
		fmt.Fprintf(output, "  --%-22s %s%s\n", strings.ReplaceAll(key, "_", "-"), description, marker)
	}
	fmt.Fprintln(output, "\nInput forms:")
	fmt.Fprintln(output, "  --flag VALUE · KEY=value · --arg KEY=value")
	fmt.Fprintln(output, "  --json-args '{\"key\":\"value\"}' · --input args.json · --input -")
	fmt.Fprintln(output, "\nAgent output and pagination:")
	fmt.Fprintln(output, "  --json (automatic for captured agent calls) · --all --max-pages 10 --max-items 1000")
	fmt.Fprintln(output, "  Preferred agent interface: typed Outfit CLI")
}

func execCommand() *cobra.Command {
	var allowWrite bool
	command := &cobra.Command{Use: "exec CONNECTION -- PROVIDER_ARGS...", Short: "Run the selected pinned provider CLI with this connection's credential", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			result, err := env.service.ExecCLI(cmd.Context(), args[0], args[1:], allowWrite)
			if len(result.Raw) > 0 {
				var pretty bytes.Buffer
				if json.Indent(&pretty, result.Raw, "", "  ") == nil {
					fmt.Fprintln(cmd.OutOrStdout(), pretty.String())
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), string(result.Raw))
				}
			}
			return err
		})
	}}
	command.Flags().BoolVar(&allowWrite, "write", false, "allow a raw provider command that may mutate state")
	return command
}

func explainCommand() *cobra.Command {
	var values stringMapFlag
	var allowWrite, jsonOutput bool
	command := &cobra.Command{Use: "explain CONNECTION OPERATION", Args: cobra.ExactArgs(2), Short: "Explain policy, credential boundary, and the exact executor plan", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			plan, err := env.service.Explain(cmd.Context(), args[0], args[1], values, allowWrite)
			if err != nil {
				return err
			}
			return renderExecutionPlan(cmd, plan, jsonOutput)
		})
	}}
	command.Flags().Var(&values, "arg", "operation argument KEY=value; repeatable")
	command.Flags().BoolVar(&allowWrite, "write", false, "explain a mutating operation without executing it")
	command.Flags().BoolVar(&jsonOutput, "json", false, "print machine-readable JSON")
	return command
}

func renderExecutionPlan(cmd *cobra.Command, plan connection.ExecutionPlan, jsonOutput bool) error {
	if jsonOutput {
		encoded, err := json.MarshalIndent(plan, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
		return nil
	}
	digest := plan.PackageDigest
	if len(digest) > 12 {
		digest = digest[:12]
	}
	fmt.Fprintf(cmd.OutOrStdout(), "connection: %s (%s, %s)\nmode: %s\noperation: %s [%s]\npolicy: %s\npackage: %s %s (%s, %s)\nexecutor: %s\ndestination: %s\n", plan.Connection, plan.Connector, plan.Status, plan.Mode, plan.Operation, plan.Runtime.Class, plan.Policy, plan.Connector, plan.PackageVersion, digest, plan.PackageTrust, plan.Runtime.Executor, plan.Runtime.Destination)
	fmt.Fprintf(cmd.OutOrStdout(), "available: %t (%s)\n", plan.Runtime.Available, plan.Runtime.Availability)
	if plan.Runtime.Method != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "request: %s %s\n", plan.Runtime.Method, plan.Runtime.Path)
	}
	if len(plan.Runtime.Argv) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "argv: %s\n", strings.Join(plan.Runtime.Argv, " "))
	}
	if plan.Runtime.Stdin != "" {
		encoded, _ := json.Marshal(plan.Runtime.Stdin)
		fmt.Fprintf(cmd.OutOrStdout(), "stdin: %s\n", encoded)
	}
	if plan.Runtime.Tool != "" {
		fmt.Fprintf(cmd.OutOrStdout(), "tool: %s\n", plan.Runtime.Tool)
	}
	if len(plan.Runtime.Arguments) > 0 {
		encoded, _ := json.Marshal(plan.Runtime.Arguments)
		fmt.Fprintf(cmd.OutOrStdout(), "arguments: %s\n", encoded)
	}
	if len(plan.Runtime.Headers) > 0 {
		names := make([]string, 0, len(plan.Runtime.Headers))
		for name := range plan.Runtime.Headers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(cmd.OutOrStdout(), "header: %s: %s\n", name, plan.Runtime.Headers[name])
		}
	}
	if plan.Runtime.Body != nil {
		encoded, _ := json.Marshal(plan.Runtime.Body)
		fmt.Fprintf(cmd.OutOrStdout(), "body: %s\n", encoded)
	}
	if len(plan.Runtime.CredentialFields) > 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "credential fields: %s\n", strings.Join(plan.Runtime.CredentialFields, ", "))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "credential boundary: %s\n", plan.Runtime.CredentialBoundary)
	return nil
}

func logCommand() *cobra.Command {
	var limit int
	command := &cobra.Command{Use: "log", Short: "Show the local connector operation receipt ledger", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			connections, err := env.state.Connections(cmd.Context())
			if err != nil {
				return err
			}
			allowedIDs := map[string]bool{}
			for _, connection := range connections {
				if env.service.Allows(connection.Name) {
					allowedIDs[connection.ID] = true
				}
			}
			items, err := env.state.Audits(cmd.Context(), limit)
			if err != nil {
				return err
			}
			for _, item := range items {
				if item.ConnectionID != "" && !allowedIDs[item.ConnectionID] {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\n", item.At.Format(time.RFC3339), item.ConnectionID, item.Operation, item.Class, item.Outcome)
			}
			return nil
		})
	}}
	command.Flags().IntVar(&limit, "limit", 50, "maximum receipts")
	return command
}

func doctorCommand() *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{Use: "doctor", Short: "Check local state, executors, and secret cleanup", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			results := make([]map[string]any, 0)
			if err := env.service.SweepSecrets(cmd.Context()); err != nil {
				results = append(results, map[string]any{"component": "secret_cleanup", "status": "warning", "detail": err.Error()})
				if !jsonOutput && !agentio.Enabled(cmd.OutOrStdout()) {
					fmt.Fprintf(cmd.OutOrStdout(), "WARN secret cleanup: %v\n", err)
				}
			}
			backend, source := vault.BackendSource(env.state.Paths())
			results = append(results, map[string]any{"component": "vault", "status": "ok", "detail": backend + " (" + source + ")"})
			if !jsonOutput && !agentio.Enabled(cmd.OutOrStdout()) {
				fmt.Fprintf(cmd.OutOrStdout(), "vault\t%s (%s)\n", backend, source)
			}
			connections, _ := env.state.Connections(cmd.Context())
			for _, item := range connections {
				if !env.service.Allows(item.Name) {
					continue
				}
				installed, _ := env.state.Package(cmd.Context(), item.ConnectorKey)
				mode, ok := runtime.FindMode(installed.Package, item.SelectedMode)
				status := "ok"
				if !ok || item.Status != "active" {
					status = "repair required"
				} else if err := credentialHealth(cmd.Context(), env.vault, item); err != nil {
					status = err.Error()
				} else if mode.Executor.Kind == "cli" {
					if err := cliExecutorHealth(installed.Package, mode, env.tools); err != nil {
						status = err.Error()
					}
				}
				results = append(results, map[string]any{"connection": item.Name, "connector": item.ConnectorKey, "mode": item.SelectedMode, "status": status})
				if !jsonOutput && !agentio.Enabled(cmd.OutOrStdout()) {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", item.Name, status)
				}
			}
			if jsonOutput || agentio.Enabled(cmd.OutOrStdout()) {
				return renderInvocationEnvelope(cmd, invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "doctor", Class: "read", Data: results, Meta: invoke.Meta{Pages: 1, Items: len(results)}})
			}
			return nil
		})
	}}
	command.Flags().BoolVar(&jsonOutput, "json", false, "return the stable agent result envelope")
	return command
}

// credentialHealth reports a connection whose stored secret is missing or
// unreadable. It reads the vault but never prints or returns the value.
func credentialHealth(ctx context.Context, secrets vault.Store, connection state.Connection) error {
	if connection.CredentialRef == "" {
		return nil
	}
	if _, err := secrets.Get(ctx, connection.CredentialRef); vault.IsNotFound(err) {
		return fmt.Errorf("credential missing: its secret is not in the vault; run `outfit credential replace %s`", connection.Name)
	} else if err != nil {
		return fmt.Errorf("credential unreadable: %v", err)
	}
	return nil
}

func vaultCommand() *cobra.Command {
	root := &cobra.Command{Use: "vault", Short: "Show or choose where new credential secrets are stored"}
	root.AddCommand(&cobra.Command{Use: "status", Short: "Print the vault backend for new secrets and where that choice comes from", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		paths, err := state.ResolvePaths()
		if err != nil {
			return err
		}
		backend, source := vault.BackendSource(paths)
		fmt.Fprintf(cmd.OutOrStdout(), "vault: %s (%s)\n", backend, source)
		return nil
	}}, &cobra.Command{Use: "use file|keyring", Short: "Record the vault backend for runs without OUTFIT_VAULT_BACKEND (existing secrets stay readable where they are)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		paths, err := state.ResolvePaths()
		if err != nil {
			return err
		}
		if err := vault.SetBackend(paths, args[0]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "new secrets go to the %s vault\n", args[0])
		return nil
	}})
	return root
}

func cliExecutorHealth(pkg kit.Package, mode kit.Mode, tools toolchain.Manager) error {
	if mode.Executor.Tool != "" {
		for _, requirement := range pkg.Tools {
			if requirement.ID != mode.Executor.Tool {
				continue
			}
			if _, err := tools.ResolveRequirement(requirement); err != nil {
				return err
			}
			return nil
		}
		return fmt.Errorf("missing managed tool contract %s", mode.Executor.Tool)
	}
	if _, err := execLookPath(mode.Executor.Binary); err != nil {
		return fmt.Errorf("missing binary %s", mode.Executor.Binary)
	}
	return nil
}

func contextCommand() *cobra.Command {
	root := &cobra.Command{Use: "context", Short: "Resolve and inspect linked-resource context"}
	root.AddCommand(contextResolveCommand(false), contextHookCommand(), &cobra.Command{Use: "mode [CONNECTION] [heavy|light|off]", Args: cobra.RangeArgs(0, 2), Short: "Inspect or set automatic link enrichment depth", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			if len(args) == 0 {
				connections, err := env.state.Connections(cmd.Context())
				if err != nil {
					return err
				}
				for _, connection := range connections {
					installed, packageErr := env.state.Package(cmd.Context(), connection.ConnectorKey)
					if packageErr == nil && connection.Status == "active" && len(installed.Package.Links) > 0 {
						mode := env.linkPreferences.Mode(connection.Name)
						if !env.service.AllowsLinks(connection.Name) {
							mode = linkcontext.ModeOff
						}
						fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", connection.Name, mode)
					}
				}
				return nil
			}
			if len(args) == 1 {
				if _, err := env.state.Connection(cmd.Context(), args[0]); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", args[0], env.linkPreferences.Mode(args[0]))
				return nil
			}
			mode, err := linkcontext.ParseMode(args[1])
			if err != nil {
				return err
			}
			if err := applicationFor(env).SetLinkEnrichmentMode(cmd.Context(), args[0], mode); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s link context: %s\n", args[0], mode)
			return nil
		})
	}}, &cobra.Command{Use: "list", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			items, err := env.state.Contexts(cmd.Context(), 20)
			if err != nil {
				return err
			}
			for _, item := range items {
				if !env.service.Allows(item.ConnectionName) || (item.Operation != "" && !env.service.AllowsOperation(item.ConnectionName, item.Operation)) {
					continue
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", item.Handle, item.ConnectionName, item.ResourceType, item.Title)
			}
			return nil
		})
	}}, &cobra.Command{Use: "read HANDLE", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			item, err := env.state.Context(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !env.service.Allows(item.ConnectionName) || (item.Operation != "" && !env.service.AllowsOperation(item.ConnectionName, item.Operation)) {
				return errors.New("cached resource is not enabled by this project")
			}
			fmt.Fprintln(cmd.OutOrStdout(), item.Body)
			return nil
		})
	}}, &cobra.Command{Use: "inspect HANDLE", Args: cobra.ExactArgs(1), Short: "Show cached content metadata, provenance, arguments, and expiration", RunE: func(cmd *cobra.Command, args []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			item, err := env.state.Context(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if !env.service.Allows(item.ConnectionName) || (item.Operation != "" && !env.service.AllowsOperation(item.ConnectionName, item.Operation)) {
				return errors.New("cached resource is not enabled by this project")
			}
			encoded, _ := json.MarshalIndent(item, "", "  ")
			fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return nil
		})
	}})
	return root
}

func contextResolveCommand(hook bool) *cobra.Command {
	return &cobra.Command{Use: "resolve", Short: "Resolve supported links from stdin", RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
		if err != nil {
			return err
		}
		return withEnvironment(cmd, func(env *environment) error {
			service := linkcontext.Service{State: env.state, Connections: env.service, Preferences: env.linkPreferences}
			rendered := linkcontext.Render(service.Resolve(cmd.Context(), string(raw)))
			if rendered != "" {
				fmt.Fprintln(cmd.OutOrStdout(), rendered)
			}
			return nil
		})
	}}
}

func contextHookCommand() *cobra.Command {
	var host string
	command := &cobra.Command{Use: "hook", Short: "Host adapter for UserPromptSubmit", RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
		if err != nil {
			return nil
		}
		prompt := hookPrompt(raw)
		if prompt == "" {
			return nil
		}
		return withEnvironment(cmd, func(env *environment) error {
			service := linkcontext.Service{State: env.state, Connections: env.service, Preferences: env.linkPreferences}
			rendered := linkcontext.Render(service.Resolve(cmd.Context(), prompt))
			if rendered == "" {
				return nil
			}
			payload := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "UserPromptSubmit", "additionalContext": rendered}}
			encoded, _ := json.Marshal(payload)
			fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return nil
		})
	}}
	command.Flags().StringVar(&host, "host", "auto", "codex, claude, or auto")
	_ = host
	return command
}

func hookPrompt(raw []byte) string {
	var object map[string]any
	if json.Unmarshal(raw, &object) == nil {
		for _, key := range []string{"prompt", "user_prompt", "message", "input"} {
			if value, ok := object[key].(string); ok {
				return value
			}
		}
	}
	return strings.TrimSpace(string(raw))
}

func sessionCommand() *cobra.Command {
	root := &cobra.Command{Use: "session", Short: "Render host session context"}
	var hook string
	contextCommand := &cobra.Command{Use: "context", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			written, _ := syncSkills(cmd.Context(), env, "both")
			connections, _ := env.state.Connections(cmd.Context())
			if len(connections) == 0 {
				return nil
			}
			var lines []string
			var needsAction []string
			for _, item := range connections {
				if item.Status == "active" && env.service.Allows(item.Name) {
					_, _, operations := selectedOperations(cmd.Context(), env, item)
					linkState := ""
					if installed, err := env.state.Package(cmd.Context(), item.ConnectorKey); err == nil && len(installed.Package.Links) > 0 {
						linkState = ", link context " + string(env.linkPreferences.Mode(item.Name))
						if !env.service.AllowsLinks(item.Name) {
							linkState = ", link context off"
						}
					}
					lines = append(lines, fmt.Sprintf("%s (%s, %s, %d operations%s)", item.Name, item.ConnectorKey, item.SelectedMode, len(operations), linkState))
				} else if item.Status != "removed" {
					needsAction = append(needsAction, fmt.Sprintf("%s (%s)", item.Name, item.Status))
				}
			}
			if len(lines) == 0 && len(needsAction) == 0 {
				return nil
			}
			text := agentguide.SessionContext(lines, needsAction, len(written))
			payload := map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart", "additionalContext": text}}
			encoded, _ := json.Marshal(payload)
			fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
			return nil
		})
	}}
	contextCommand.Flags().StringVar(&hook, "hook", "auto", "host hook adapter (auto, claude, or codex)")
	root.AddCommand(contextCommand)
	return root
}

func pluginCommand() *cobra.Command {
	root := &cobra.Command{Use: "plugin", Short: "Install and synchronize the Codex and Claude plugins"}
	root.AddCommand(pluginSyncCommand(), pluginInstallCommand(), pluginHostCommand("status"), pluginHostCommand("update"), pluginHostCommand("remove"))
	return root
}

func pluginHostCommand(action string) *cobra.Command {
	var host string
	command := &cobra.Command{Use: action, Short: strings.ToUpper(action[:1]) + action[1:] + " the host plugin installation", RunE: func(cmd *cobra.Command, _ []string) error {
		if host != "both" && host != "codex" && host != "claude" {
			return errors.New("host must be codex, claude, or both")
		}
		if host == "both" || host == "codex" {
			var args []string
			switch action {
			case "status":
				args = []string{"plugin", "list"}
			case "update":
				args = []string{"plugin", "marketplace", "upgrade", "outfit"}
			case "remove":
				args = []string{"plugin", "remove", "outfit@outfit"}
			}
			if err := runHost(cmd, "codex", args...); err != nil {
				return err
			}
		}
		if host == "both" || host == "claude" {
			var args []string
			switch action {
			case "status":
				args = []string{"plugin", "list"}
			case "update":
				args = []string{"plugin", "update", "outfit@jhh3-outfit", "--scope", "user", "--yes"}
			case "remove":
				args = []string{"plugin", "uninstall", "outfit@jhh3-outfit", "--scope", "user", "--yes"}
			}
			if err := runHost(cmd, "claude", args...); err != nil {
				return err
			}
		}
		if action == "update" {
			fmt.Fprintln(cmd.OutOrStdout(), "updated; restart the host to load the new plugin")
		}
		return nil
	}}
	command.Flags().StringVar(&host, "host", "both", "codex, claude, or both")
	return command
}

func profileCommand() *cobra.Command {
	root := &cobra.Command{Use: "profile", Short: "Infer or select the connector profile that activates in this project"}
	root.AddCommand(&cobra.Command{Use: "use PROFILE", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		loaded, err := workspace.Read(".")
		if err != nil {
			return err
		}
		if loaded.Path == "" {
			return errors.New("no .outfit/workspace.yaml found; use outfit profile infer --write or outfit workspace init")
		}
		if _, ok := loaded.Manifest.Profiles[args[0]]; !ok {
			return fmt.Errorf("workspace has no profile %s", args[0])
		}
		loaded.Manifest.Profile = args[0]
		if err := workspace.WriteManifest(loaded.Path, loaded.Manifest); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "selected profile %s in %s; run outfit lock, review it, then outfit apply\n", args[0], loaded.Path)
		return nil
	}}, &cobra.Command{Use: "show", RunE: func(cmd *cobra.Command, _ []string) error {
		if loaded, err := workspace.Read("."); err != nil {
			return err
		} else if loaded.Path != "" {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\nprofile: %s\n", loaded.Path, loaded.Manifest.Profile)
			for _, item := range loaded.Manifest.SelectedConnectors() {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", item.Name, item.Package, item.Mode)
			}
			return nil
		}
		fmt.Fprintln(cmd.OutOrStdout(), "no project workspace; all local connections are available")
		return nil
	}})
	var write, jsonOutput bool
	infer := &cobra.Command{Use: "infer", Short: "Propose a profile from Git remotes, manifests, MCP config, dependencies, and docs", RunE: func(cmd *cobra.Command, _ []string) error {
		env, err := openBaseEnvironment(cmd.Context())
		if err != nil {
			return err
		}
		defer env.state.Close()
		connections, err := env.state.Connections(cmd.Context())
		if err != nil {
			return err
		}
		packages, err := env.state.Packages(cmd.Context())
		if err != nil {
			return err
		}
		sources := map[string]string{}
		for _, item := range packages {
			sources[item.Key] = item.Source
		}
		available := make([]workspace.AvailableConnection, 0, len(connections))
		for _, item := range connections {
			if item.Status == "active" {
				available = append(available, workspace.AvailableConnection{Name: item.Name, Package: item.ConnectorKey, Mode: item.SelectedMode, Source: sources[item.ConnectorKey]})
			}
		}
		inferred, err := workspace.Infer(".", available)
		if err != nil {
			return err
		}
		if jsonOutput {
			encoded, _ := json.MarshalIndent(inferred, "", "  ")
			fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
		} else {
			fmt.Fprint(cmd.OutOrStdout(), workspace.FormatInference(inferred))
		}
		if !write {
			fmt.Fprintln(cmd.ErrOrStderr(), "proposal only; repeat with --write, then review outfit lock and approve with outfit apply")
			return nil
		}
		root, _ := filepath.Abs(".")
		path := filepath.Join(root, ".outfit", "workspace.yaml")
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; refusing to overwrite", path)
		}
		if err := workspace.WriteManifest(path, inferred.Manifest); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "wrote proposal %s; review it, run outfit lock, then outfit apply\n", path)
		return nil
	}}
	infer.Flags().BoolVar(&write, "write", false, "write the proposal to .outfit/workspace.yaml")
	infer.Flags().BoolVar(&jsonOutput, "json", false, "print machine-readable evidence")
	root.AddCommand(infer)
	return root
}

func selectedOperations(ctx context.Context, env *environment, item state.Connection) (state.InstalledPackage, kit.Mode, []kit.Operation) {
	installed, _ := env.state.Package(ctx, item.ConnectorKey)
	mode, _ := runtime.FindMode(installed.Package, item.SelectedMode)
	operations := make([]kit.Operation, 0, len(mode.Operations))
	for _, operation := range mode.Operations {
		if env.service.AllowsOperation(item.Name, operation.Name) && connection.OperationCapabilitiesAllowed(item, mode, operation) {
			operations = append(operations, kit.NormalizeOperation(operation))
		}
	}
	known := map[string]bool{}
	for _, operation := range operations {
		known[operation.Name] = true
	}
	tools, _ := env.state.Tools(ctx, item.ID)
	for _, tool := range tools {
		if !known[tool.Name] && env.service.AllowsOperation(item.Name, tool.Name) {
			operations = append(operations, kit.NormalizeOperation(kit.Operation{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, Input: tool.InputSchema, MCPTool: tool.SourceName}))
		}
	}
	return installed, mode, operations
}

func mcpCommand() *cobra.Command {
	return &cobra.Command{Use: "mcp", Short: "Serve active selected-mode operations over local stdio MCP", RunE: func(cmd *cobra.Command, _ []string) error {
		env, err := openEnvironment(cmd.Context())
		if err != nil {
			return err
		}
		defer env.state.Close()
		return mcpserver.Run(cmd.Context(), env.state, env.service)
	}}
}

func adminCommand() *cobra.Command {
	var noOpen bool
	var address string
	command := &cobra.Command{Use: "admin", Short: "Open the local connector workbench", RunE: func(cmd *cobra.Command, _ []string) error {
		env, err := openEnvironment(cmd.Context())
		if err != nil {
			return err
		}
		defer env.state.Close()
		return admin.Serve(cmd.Context(), address, env.state, env.service, func(adminURL string) {
			fmt.Fprintf(cmd.ErrOrStderr(), "outfit admin: %s\n", adminURL)
			if !noOpen {
				_ = openBrowser(cmd.Context(), adminURL)
			}
		})
	}}
	command.Flags().BoolVar(&noOpen, "no-open", false, "do not open the browser")
	command.Flags().StringVar(&address, "listen", "127.0.0.1:0", "loopback listen address")
	return command
}

func openBrowser(ctx context.Context, target string) error {
	var program string
	var args []string
	switch goruntime.GOOS {
	case "darwin":
		program, args = "open", []string{target}
	case "windows":
		program, args = "rundll32", []string{"url.dll,FileProtocolHandler", target}
	default:
		program, args = "xdg-open", []string{target}
	}
	path, err := exec.LookPath(program)
	if err != nil {
		return err
	}
	return exec.CommandContext(ctx, path, args...).Start()
}

func pluginSyncCommand() *cobra.Command {
	var host string
	command := &cobra.Command{Use: "sync", Short: "Generate connector-specific skills for active connections", RunE: func(cmd *cobra.Command, _ []string) error {
		return withEnvironment(cmd, func(env *environment) error {
			written, err := syncSkills(cmd.Context(), env, host)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "synchronized %d connector host skills\n", len(written))
			return nil
		})
	}}
	command.Flags().StringVar(&host, "host", "both", "codex, claude, or both")
	return command
}

func syncSkills(ctx context.Context, env *environment, host string) ([]string, error) {
	connections, err := env.state.Connections(ctx)
	if err != nil {
		return nil, err
	}
	installed, err := env.state.Packages(ctx)
	if err != nil {
		return nil, err
	}
	packages := map[string]kit.Package{}
	for _, item := range installed {
		packages[item.Key] = item.Package
	}
	filtered := connections[:0]
	for _, item := range connections {
		if env.service.Allows(item.Name) && (env.policy == nil || env.policy.Skills(item.Name)) {
			filtered = append(filtered, item)
		}
	}
	tools := map[string][]state.Tool{}
	for _, item := range filtered {
		tools[item.Name], _ = env.state.Tools(ctx, item.ID)
	}
	var allowed func(string, string) bool
	if env.policy != nil {
		allowed = env.policy.AllowsOperation
	}
	if _, err := env.skills.Sync(ctx, host, filtered, packages, tools, allowed); err != nil {
		return nil, err
	}
	receipts, err := env.skills.Inventory(host)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(receipts))
	for _, receipt := range receipts {
		paths = append(paths, receipt.InstalledPath)
	}
	return paths, nil
}

func pluginInstallCommand() *cobra.Command {
	var host string
	command := &cobra.Command{Use: "install", Short: "Install outfit from its private Git marketplace", RunE: func(cmd *cobra.Command, _ []string) error {
		steps, err := pluginInstallPlan(host)
		if err != nil {
			return err
		}
		for _, step := range steps {
			if err := runHost(cmd, step.program, step.args...); err != nil {
				if step.optional {
					continue
				}
				return err
			}
		}
		fmt.Fprintln(cmd.OutOrStdout(), "plugin installed or updated; restart the host to load it")
		return nil
	}}
	command.Flags().StringVar(&host, "host", "both", "codex, claude, or both")
	return command
}

type hostCommand struct {
	program  string
	args     []string
	optional bool
}

func pluginInstallPlan(host string) ([]hostCommand, error) {
	if host != "both" && host != "codex" && host != "claude" {
		return nil, errors.New("host must be codex, claude, or both")
	}
	var steps []hostCommand
	if host == "both" || host == "codex" {
		steps = append(steps,
			hostCommand{program: "codex", args: []string{"plugin", "remove", "cw@personal"}, optional: true},
			hostCommand{program: "codex", args: []string{"plugin", "marketplace", "add", "jhh3/outfit", "--ref", "main"}},
			hostCommand{program: "codex", args: []string{"plugin", "marketplace", "upgrade", "outfit"}},
			hostCommand{program: "codex", args: []string{"plugin", "add", "outfit@outfit"}},
		)
	}
	if host == "both" || host == "claude" {
		steps = append(steps,
			hostCommand{program: "claude", args: []string{"plugin", "uninstall", "cw@jhh3-cw", "--scope", "user", "--yes"}, optional: true},
			hostCommand{program: "claude", args: []string{"plugin", "marketplace", "add", "jhh3/outfit"}},
			hostCommand{program: "claude", args: []string{"plugin", "marketplace", "update", "jhh3-outfit"}},
			hostCommand{program: "claude", args: []string{"plugin", "install", "outfit@jhh3-outfit", "--scope", "user", "--yes"}},
			hostCommand{program: "claude", args: []string{"plugin", "update", "outfit@jhh3-outfit", "--scope", "user", "--yes"}},
		)
	}
	return steps, nil
}

func runHost(cmd *cobra.Command, program string, args ...string) error {
	path, err := exec.LookPath(program)
	if err != nil {
		return fmt.Errorf("%s is not installed", program)
	}
	process := exec.CommandContext(cmd.Context(), path, args...)
	process.Stdin = cmd.InOrStdin()
	process.Stdout = cmd.OutOrStdout()
	process.Stderr = cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", program, strings.Join(args, " "), err)
	}
	return nil
}

var execLookPath = exec.LookPath

func kitCommand() *cobra.Command {
	root := &cobra.Command{Use: "kit", Short: "Author and validate connector packages"}
	var kind string
	newCommand := &cobra.Command{Use: "new KEY", Args: cobra.ExactArgs(1), Short: "Scaffold an HTTP, MCP, or CLI connector package", RunE: func(cmd *cobra.Command, args []string) error {
		directory, err := kit.Scaffold(".", args[0], kind)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "created %s\n", directory)
		return nil
	}}
	newCommand.Flags().StringVar(&kind, "kind", "http", "http, mcp, or cli")
	root.AddCommand(newCommand)
	root.AddCommand(kitImportCommand())
	root.AddCommand(&cobra.Command{Use: "validate PATH", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		loaded, err := kit.Load(args[0])
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s %s is valid (%s)\n", loaded.Package.Key, loaded.Package.Version, loaded.Digest[:12])
		return nil
	}})
	root.AddCommand(&cobra.Command{Use: "test PATH", Args: cobra.ExactArgs(1), Short: "Run package conformance checks", RunE: func(cmd *cobra.Command, args []string) error {
		loaded, err := kit.Load(args[0])
		if err != nil {
			return err
		}
		operations, links := 0, len(loaded.Package.Links)
		ergonomic := 0
		for _, mode := range loaded.Package.Modes {
			operations += len(mode.Operations)
			for _, operation := range mode.Operations {
				if issues := kit.AuditOperationSchema(operation); len(issues) > 0 {
					return fmt.Errorf("operation %s/%s schema: %s", mode.ID, operation.Name, strings.Join(issues, "; "))
				}
				ergonomic++
			}
			if mode.Executor.Kind == "cli" {
				if _, err := exec.LookPath(mode.Executor.Binary); err != nil {
					return fmt.Errorf("mode %s requires missing binary %s", mode.ID, mode.Executor.Binary)
				}
			}
		}
		fmt.Fprintf(cmd.OutOrStdout(), "PASS manifest\nPASS %d modes · %d operations · %d link resolvers\nPASS %d ergonomic operation schemas\nPASS selected-mode and read-only enrichment invariants\n", len(loaded.Package.Modes), operations, links, ergonomic)
		return nil
	}})
	root.AddCommand(&cobra.Command{Use: "diff OLD NEW", Args: cobra.ExactArgs(2), Short: "Show semantic and security changes between package versions", RunE: func(cmd *cobra.Command, args []string) error {
		before, err := kit.Load(args[0])
		if err != nil {
			return err
		}
		after, err := kit.Load(args[1])
		if err != nil {
			return err
		}
		changes := kit.Diff(before.Package, after.Package)
		if len(changes) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "no semantic changes")
			return nil
		}
		for _, change := range changes {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", change.Severity, change.Path, change.Description)
		}
		return nil
	}})
	return root
}

func kitImportCommand() *cobra.Command {
	var options importer.Options
	var output string
	var secrets stringMapFlag
	command := &cobra.Command{Use: "import KIND SOURCE", Args: cobra.ExactArgs(2), Short: "Generate a connector from OpenAPI, MCP, Postman, CLI help, or GraphQL introspection", RunE: func(cmd *cobra.Command, args []string) error {
		options.Kind, options.Source, options.Secrets = args[0], args[1], secrets
		if (options.Kind == "mcp" || options.Kind == "graphql") && options.AuthField != "" && options.Secrets[options.AuthField] == "" {
			if options.Secrets == nil {
				options.Secrets = map[string]string{}
			}
			if err := collectFields(cmd, []kit.CredentialField{{Key: options.AuthField, Label: options.AuthField, Secret: true}}, options.Secrets); err != nil {
				return fmt.Errorf("live discovery credential: %w", err)
			}
		}
		result, err := importer.Import(cmd.Context(), options)
		if err != nil {
			return err
		}
		directory, err := importer.Write(output, result)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "created %s with %d operations\n", directory, len(result.Package.Modes[0].Operations))
		for _, warning := range result.Warnings {
			fmt.Fprintf(cmd.ErrOrStderr(), "REVIEW: %s\n", warning)
		}
		return nil
	}}
	command.Flags().StringVar(&options.Key, "key", "", "connector package key")
	command.Flags().StringVar(&options.Display, "display", "", "display name")
	command.Flags().StringVar(&options.BaseURL, "base-url", "", "fixed HTTPS API base URL when not declared by the source")
	command.Flags().StringVar(&options.Binary, "binary", "", "PATH-resolved CLI binary")
	command.Flags().StringVar(&options.LinkHost, "link-host", "", "explicit human-facing host for suggested OpenAPI link resolvers")
	command.Flags().StringVar(&options.AuthField, "auth-field", "", "UPPER_SNAKE_CASE credential field; omitted means no injected auth")
	command.Flags().StringVar(&options.AuthHeader, "auth-header", "Authorization", "credential header name")
	command.Flags().StringVar(&options.AuthFormat, "auth-format", "", "header format; defaults to Bearer {{secret.FIELD}}")
	command.Flags().BoolVar(&options.PrivateNetwork, "private-network", false, "allow the generated fixed endpoint to resolve to private addresses")
	command.Flags().Var(&secrets, "set", "temporary FIELD=value used only for live MCP/GraphQL discovery")
	command.Flags().StringVarP(&output, "output", "o", ".", "parent directory for the generated package")
	_ = command.MarkFlagRequired("key")
	return command
}
