// Command eval-state creates credential-free Outfit state for agent-routing evals.
// It is intentionally a repository test utility, not part of the released CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/outfit/internal/builtin"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/state"
)

type connectionFlags []string

func (values *connectionFlags) String() string { return strings.Join(*values, ",") }
func (values *connectionFlags) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func main() {
	var home string
	var connections connectionFlags
	flag.StringVar(&home, "home", "", "isolated OUTFIT_HOME to create")
	flag.Var(&connections, "connection", "NAME=CONNECTOR:MODE (repeatable)")
	flag.Parse()
	if home == "" || len(connections) == 0 {
		fatal(errors.New("--home and at least one --connection are required"))
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		fatal(err)
	}
	if err := os.Setenv("OUTFIT_HOME", abs); err != nil {
		fatal(err)
	}
	paths, err := state.ResolvePaths()
	if err != nil {
		fatal(err)
	}
	store, err := state.Open(context.Background(), paths)
	if err != nil {
		fatal(err)
	}
	defer store.Close()
	loaded, err := builtin.All()
	if err != nil {
		fatal(err)
	}
	packages := map[string]kit.Loaded{}
	for _, item := range loaded {
		packages[item.Package.Key] = item
		if err := store.InstallPackage(context.Background(), item, "builtin"); err != nil {
			fatal(err)
		}
	}
	for _, specification := range connections {
		name, connector, modeID, err := parseConnection(specification)
		if err != nil {
			fatal(err)
		}
		installed, ok := packages[connector]
		if !ok {
			fatal(fmt.Errorf("unknown connector %s", connector))
		}
		mode, ok := kit.FindMode(installed.Package, modeID)
		if !ok {
			fatal(fmt.Errorf("connector %s has no mode %s", connector, modeID))
		}
		method := mode.DefaultCredentialMethod
		if method == "" && len(mode.CredentialMethods) > 0 {
			method = mode.CredentialMethods[0]
		}
		identity := map[string]string{"account_name": "Synthetic " + name}
		if connector == "stripe" {
			identity["environment"] = "test"
		}
		connection := state.Connection{
			ID:               "eval_" + strings.NewReplacer("-", "_", ".", "_").Replace(name),
			Name:             name,
			ConnectorKey:     connector,
			SelectedMode:     modeID,
			CredentialMethod: method,
			Status:           "active",
			SafeIdentity:     identity,
		}
		if err := store.CreateConnectionWithTools(context.Background(), connection, syntheticTools(installed.Package, mode)); err != nil {
			fatal(fmt.Errorf("create %s: %w", name, err))
		}
	}
}

func parseConnection(value string) (string, string, string, error) {
	name, target, ok := strings.Cut(value, "=")
	if !ok {
		return "", "", "", fmt.Errorf("invalid connection %q: want NAME=CONNECTOR:MODE", value)
	}
	connector, mode, ok := strings.Cut(target, ":")
	if !ok || name == "" || connector == "" || mode == "" {
		return "", "", "", fmt.Errorf("invalid connection %q: want NAME=CONNECTOR:MODE", value)
	}
	return name, connector, mode, nil
}

func syntheticTools(pkg kit.Package, mode kit.Mode) []state.Tool {
	if mode.Executor.Kind != "mcp" {
		return nil
	}
	operations := map[string]map[string]bool{}
	for _, resolver := range pkg.Links {
		for _, lookup := range resolver.LookupOptions() {
			if lookup.Mode != mode.ID {
				continue
			}
			collectLookup(operations, lookup.Operation, lookup.Arguments)
			for _, heavy := range lookup.HeavyReads {
				collectLookup(operations, heavy.Operation, heavy.Arguments)
			}
		}
	}
	names := make([]string, 0, len(operations))
	for name := range operations {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]state.Tool, 0, len(names))
	for _, name := range names {
		properties := map[string]any{}
		var required []string
		for argument, literal := range operations[name] {
			properties[argument] = map[string]any{"type": "string"}
			if !literal {
				required = append(required, argument)
			}
		}
		sort.Strings(required)
		schema := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			schema["required"] = required
		}
		tools = append(tools, state.Tool{Name: name, Title: name, Description: "Synthetic provider-shaped eval tool.", Class: "read", InputSchema: schema, SourceName: name})
	}
	return tools
}

func collectLookup(operations map[string]map[string]bool, name string, arguments map[string]string) {
	if name == "" {
		return
	}
	if operations[name] == nil {
		operations[name] = map[string]bool{}
	}
	for argument, source := range arguments {
		operations[name][argument] = strings.HasPrefix(source, "=")
	}
}

func fatal(err error) {
	encoded, _ := json.Marshal(map[string]string{"error": err.Error()})
	fmt.Fprintln(os.Stderr, string(encoded))
	os.Exit(1)
}
