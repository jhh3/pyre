package application

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jhh3/outfit/internal/hostmcp"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/linkcontext"
	"github.com/jhh3/outfit/internal/state"
)

type ProviderState string

const (
	ProviderConnected      ProviderState = "connected"
	ProviderDetected       ProviderState = "detected"
	ProviderImportable     ProviderState = "ready to import"
	ProviderNeedsAttention ProviderState = "needs attention"
	ProviderNotConfigured  ProviderState = "not configured"
)

type SetupRecipe struct {
	ID, Label, Description, ModeID, ModeLabel, MethodID, MethodLabel string
	Reason, Source, Account, BlockedReason                           string
	Rank                                                             int
	Recommended, Detected, Importable                                bool
	Import                                                           *hostmcp.Candidate
	Mode                                                             kit.Mode
	Method                                                           kit.CredentialMethod
}

type ProviderCard struct {
	Connector   ConnectorCard
	State       ProviderState
	StateDetail string
	Connections []state.Connection
	Credentials []state.CredentialRecord
	Imports     []hostmcp.Candidate
	Tools       []ToolCard
	Skills      []SkillCard
	Recipes     []SetupRecipe
	LinkModes   map[string]linkcontext.Mode
}

type HomeItem struct {
	Kind, ProviderKey, ID, Title, Detail string
	Priority                             int
}

type Workbench struct {
	Providers []ProviderCard
	Home      []HomeItem
}

func BuildWorkbench(snapshot Snapshot) Workbench {
	connections := map[string][]state.Connection{}
	credentials := map[string][]state.CredentialRecord{}
	imports := map[string][]hostmcp.Candidate{}
	for _, item := range snapshot.Connections {
		if item.Status != "removed" {
			connections[item.ConnectorKey] = append(connections[item.ConnectorKey], item)
		}
	}
	for _, item := range snapshot.Credentials {
		credentials[item.ConnectorKey] = append(credentials[item.ConnectorKey], item)
	}
	for _, item := range snapshot.Imports {
		imports[item.ConnectorKey] = append(imports[item.ConnectorKey], item)
	}

	result := Workbench{}
	for _, connector := range snapshot.Connectors {
		provider := ProviderCard{
			Connector:   connector,
			Connections: append([]state.Connection(nil), connections[connector.Key]...),
			Credentials: append([]state.CredentialRecord(nil), credentials[connector.Key]...),
			Imports:     append([]hostmcp.Candidate(nil), imports[connector.Key]...),
			LinkModes:   map[string]linkcontext.Mode{},
		}
		for _, connection := range provider.Connections {
			provider.LinkModes[connection.Name] = snapshot.LinkModes[connection.Name]
		}
		for _, tool := range snapshot.Tools {
			if providerUsesTool(connector, tool.ID) {
				provider.Tools = append(provider.Tools, tool)
			}
		}
		for _, skill := range snapshot.Skills {
			if skill.Connector == connector.Key {
				provider.Skills = append(provider.Skills, skill)
			}
		}
		provider.Recipes = setupRecipes(connector, provider.Imports)
		provider.State, provider.StateDetail = providerState(provider)
		result.Providers = append(result.Providers, provider)
	}
	sort.SliceStable(result.Providers, func(i, j int) bool {
		left, right := providerStateRank(result.Providers[i].State), providerStateRank(result.Providers[j].State)
		if left != right {
			return left < right
		}
		return result.Providers[i].Connector.Display < result.Providers[j].Connector.Display
	})
	result.Home = homeItems(result.Providers, snapshot.Updates)
	return result
}

func setupRecipes(connector ConnectorCard, imports []hostmcp.Candidate) []SetupRecipe {
	methods := map[string]kit.CredentialMethod{}
	for _, method := range connector.Methods {
		methods[method.ID] = method
	}
	var recipes []SetupRecipe
	for _, mode := range connector.Modes {
		for _, methodID := range mode.CredentialMethods {
			method, ok := methods[methodID]
			if !ok {
				continue
			}
			recommended := mode.ID == connector.RecommendedMode && (method.ID == mode.DefaultCredentialMethod || method.Recommended || len(mode.CredentialMethods) == 1)
			detected := method.ID == connector.DetectedCredentialMethod
			label := mode.Label
			if len(mode.CredentialMethods) > 1 || method.Label != mode.Label {
				label += " · " + method.Label
			}
			reason := mode.Recommendation.Reason
			if detected {
				reason = "Authenticated setup detected on this machine"
			}
			blockedReason := ""
			if requiresHostOAuthImport(method) {
				blockedReason = "No transferable sign-in was found. Connect this provider's hosted MCP in Claude Code or Codex, complete its browser sign-in, then refresh Outfit and choose the detected import recipe."
			}
			recipes = append(recipes, SetupRecipe{
				ID: mode.ID + ":" + method.ID, Label: label, Description: method.Description,
				ModeID: mode.ID, ModeLabel: mode.Label, MethodID: method.ID, MethodLabel: method.Label,
				Reason: reason, BlockedReason: blockedReason, Rank: mode.Recommendation.Rank, Recommended: recommended, Detected: detected,
				Mode: mode, Method: method,
			})
		}
	}
	for index := range imports {
		candidate := imports[index]
		label := "Import OAuth from " + strings.Join(candidate.Hosts, " + ")
		reason := candidate.Reason
		if candidate.Transferable {
			reason = "Verified existing sign-in; transfer it into outfit and remove the duplicate host configuration"
		}
		rank := 1000
		if candidate.Transferable {
			rank = -100
		}
		recipes = append(recipes, SetupRecipe{
			ID: "import:" + candidate.ID, Label: label, Description: candidate.Endpoint,
			Reason: reason, Source: strings.Join(candidate.Hosts, "+"), Rank: rank,
			Importable: candidate.Transferable, Import: &candidate,
		})
	}
	sort.SliceStable(recipes, func(i, j int) bool {
		left, right := recipeRank(recipes[i]), recipeRank(recipes[j])
		if left != right {
			return left < right
		}
		return recipes[i].Label < recipes[j].Label
	})
	return recipes
}

func requiresHostOAuthImport(method kit.CredentialMethod) bool {
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

func recipeRank(recipe SetupRecipe) int {
	if recipe.Detected {
		return -300
	}
	if recipe.Importable {
		return -200
	}
	if recipe.Recommended {
		return -100
	}
	return recipe.Rank
}

func providerState(provider ProviderCard) (ProviderState, string) {
	active := 0
	for _, connection := range provider.Connections {
		if connection.Status == "active" {
			active++
		} else {
			return ProviderNeedsAttention, connection.Name + " is " + connection.Status
		}
	}
	if active > 0 {
		return ProviderConnected, fmt.Sprintf("%d active account(s)", active)
	}
	for _, item := range provider.Imports {
		if item.Transferable {
			return ProviderImportable, "Existing OAuth sign-in can be transferred"
		}
	}
	if provider.Connector.DetectedCredentialMethod != "" || len(provider.Connector.ExistingSetup) > 0 {
		return ProviderDetected, "Existing setup found on this machine"
	}
	return ProviderNotConfigured, "Choose a setup recipe"
}

func providerStateRank(state ProviderState) int {
	switch state {
	case ProviderNeedsAttention:
		return 0
	case ProviderImportable:
		return 1
	case ProviderDetected:
		return 2
	case ProviderConnected:
		return 3
	default:
		return 4
	}
}

func homeItems(providers []ProviderCard, updates []Update) []HomeItem {
	var result []HomeItem
	for _, provider := range providers {
		switch provider.State {
		case ProviderNeedsAttention:
			result = append(result, HomeItem{Kind: "provider", ProviderKey: provider.Connector.Key, Title: provider.Connector.Display + " needs attention", Detail: provider.StateDetail, Priority: 0})
		case ProviderImportable:
			result = append(result, HomeItem{Kind: "provider", ProviderKey: provider.Connector.Key, Title: "Import " + provider.Connector.Display, Detail: provider.StateDetail, Priority: 1})
		case ProviderDetected:
			result = append(result, HomeItem{Kind: "provider", ProviderKey: provider.Connector.Key, Title: "Finish connecting " + provider.Connector.Display, Detail: provider.StateDetail, Priority: 2})
		}
	}
	for _, update := range updates {
		result = append(result, HomeItem{Kind: "update", ID: update.ID, Title: "Update " + update.ID, Detail: update.Current + " → " + update.Available, Priority: 3})
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority < result[j].Priority
		}
		return result[i].Title < result[j].Title
	})
	return result
}

func providerUsesTool(connector ConnectorCard, toolID string) bool {
	for _, mode := range connector.Modes {
		if mode.Executor.Tool == toolID {
			return true
		}
	}
	return false
}
