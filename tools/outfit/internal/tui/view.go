package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/jhh3/outfit/internal/application"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/linkcontext"
	"github.com/jhh3/outfit/internal/state"
)

var (
	accent      = lipgloss.NewStyle().Foreground(lipgloss.Color("212")).Bold(true)
	brand       = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true)
	quiet       = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
	dim         = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	good        = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	warning     = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	bad         = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	selected    = lipgloss.NewStyle().Foreground(lipgloss.Color("230")).Background(lipgloss.Color("57")).Bold(true)
	panelTitle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Bold(true)
	sectionHint = lipgloss.NewStyle().Foreground(lipgloss.Color("246"))
	divider     = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	keyStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("213")).Bold(true)
	cardBorder  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("238")).Padding(0, 1)
)

func (model Model) View() string {
	if model.width == 0 {
		return "Loading outfit…"
	}
	if model.screen == paletteScreen {
		return model.frame(model.paletteView(), "↑/↓ choose · enter run · esc close")
	}
	if model.screen == helpScreen {
		return model.frame(model.helpView(), "esc close help")
	}
	if model.screen == busyScreen {
		return model.frame("\n"+model.spinner.View()+" "+model.status+"\n", "ctrl+c quit")
	}
	if model.screen == wizardScreen {
		return model.frame(model.wizardView(), "enter next/apply · esc keep current connection")
	}
	if model.screen == backupWizardScreen {
		return model.frame(model.backupWizardView(), "enter next/apply · esc back")
	}

	var content string
	switch model.screen {
	case providerScreen:
		content = model.providerScreenView()
	case projectScreen:
		content = model.projectScreenView()
	case systemScreen:
		content = model.systemScreenView()
	default:
		content = model.browseView()
	}
	return model.frame(content, model.footer())
}

func (model Model) frame(content, footer string) string {
	var body strings.Builder
	header := model.header()
	body.WriteString(header)
	body.WriteString("\n")
	showSearch := model.screen != paletteScreen && model.screen != helpScreen && model.screen != busyScreen && model.screen != wizardScreen && model.screen != backupWizardScreen
	if showSearch {
		body.WriteString(model.search.View())
		body.WriteString("\n\n")
	}
	reserved := lipgloss.Height(header) + 4
	if showSearch {
		reserved += 2
	}
	if model.err != "" || model.status != "" {
		reserved += 2
	}
	body.WriteString(clipLines(content, max(3, model.height-reserved)))
	if model.err != "" {
		body.WriteString("\n\n" + bad.Render("! "+model.err))
	} else if model.status != "" {
		body.WriteString("\n\n" + good.Render("✓ "+model.status))
	}
	body.WriteString("\n\n")
	body.WriteString(quiet.Render(footer))
	return body.String()
}

func (model Model) header() string {
	full := application.BuildWorkbench(model.snapshot)
	connected, ready, issues := 0, 0, 0
	for _, provider := range full.Providers {
		switch provider.State {
		case application.ProviderConnected:
			connected++
		case application.ProviderImportable, application.ProviderDetected:
			ready++
		case application.ProviderNeedsAttention:
			issues++
		}
	}
	var body strings.Builder
	body.WriteString(brand.Render("outfit"))
	body.WriteString("  " + quiet.Render("connector workbench"))
	body.WriteString("    ")
	body.WriteString(good.Render(fmt.Sprintf("%d connected", connected)))
	body.WriteString(quiet.Render(fmt.Sprintf(" · %d ready · ", ready)))
	if issues > 0 {
		body.WriteString(bad.Render(fmt.Sprintf("%d need attention", issues)))
	} else {
		body.WriteString(good.Render("healthy"))
	}
	if len(model.snapshot.Updates) > 0 {
		body.WriteString(warning.Render(fmt.Sprintf(" · %d update(s)", len(model.snapshot.Updates))))
	}
	if model.refreshing {
		body.WriteString("  " + model.spinner.View() + quiet.Render(" checking local setup"))
	}
	body.WriteString("\n")
	for index, name := range sectionNames {
		label := " " + name + " "
		if section(index) == model.section {
			body.WriteString(selected.Render(label))
		} else {
			body.WriteString(quiet.Render(label))
		}
		body.WriteString(" ")
	}
	return body.String()
}

func (model Model) browseView() string {
	var left, right string
	switch model.section {
	case homeSection:
		left, right = model.homeListView(), model.homeOverviewView()
	case connectorsSection:
		left, right = model.providerListView(), model.providerOverviewView()
	case projectsSection:
		left, right = model.projectListView(), model.projectPreviewView()
	case systemSection:
		left, right = model.systemCategoryListView(), model.systemCategoryPreviewView()
	}
	return model.twoPane(left, right)
}

func (model Model) twoPane(left, right string) string {
	if model.width < 92 {
		if strings.TrimSpace(right) == "" {
			return left
		}
		previewHeight := max(3, min(5, model.height/4))
		return left + "\n" + divider.Render(strings.Repeat("─", max(8, min(model.width-4, 60)))) + "\n" + clipLines(right, previewHeight)
	}
	leftWidth := min(40, max(30, model.width/3))
	rightWidth := max(42, model.width-leftWidth-5)
	leftPanel := lipgloss.NewStyle().Width(leftWidth).Render(left)
	rightPanel := lipgloss.NewStyle().Width(rightWidth).PaddingLeft(2).BorderLeft(true).BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("238")).Render(right)
	return lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)
}

func (model Model) homeListView() string {
	var body strings.Builder
	body.WriteString(panelTitle.Render("Next actions") + "\n")
	if len(model.workbench.Home) == 0 {
		body.WriteString("\n" + good.Render("✓ You're all caught up") + "\n")
		body.WriteString(quiet.Render("Connect another provider whenever you need it."))
		return body.String()
	}
	height := model.listHeight()
	if model.width < 92 {
		height = max(2, height/2)
	}
	start, end := windowRange(len(model.workbench.Home), model.cursor, height)
	for index := start; index < end; index++ {
		item := model.workbench.Home[index]
		line := item.Title + "\n  " + quiet.Render(item.Detail)
		body.WriteString(selectLine(index == model.cursor, line) + "\n")
	}
	return strings.TrimRight(body.String(), "\n")
}

func (model Model) homeOverviewView() string {
	full := application.BuildWorkbench(model.snapshot)
	connected, available := 0, 0
	var connectedNames []string
	for _, provider := range full.Providers {
		if provider.State == application.ProviderConnected {
			connected++
			connectedNames = append(connectedNames, provider.Connector.Display)
		} else {
			available++
		}
	}
	var body strings.Builder
	body.WriteString(panelTitle.Render("Your connector workspace") + "\n")
	body.WriteString(sectionHint.Render("One place for accounts, execution, skills, updates, and project access.") + "\n\n")
	body.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, metric("Connected", connected, "verified providers"), "  ", metric("Available", available, "more providers")))
	body.WriteString("\n\n")
	if len(connectedNames) > 0 {
		body.WriteString("Connected now\n")
		body.WriteString(good.Render("  ✓ " + strings.Join(connectedNames, " · ")))
		body.WriteString("\n\n")
	}
	body.WriteString("How it works\n")
	body.WriteString(quiet.Render("  1. Choose a provider\n  2. Pick one recommended setup recipe\n  3. Verify the account\n  4. outfit installs the CLI and skills that recipe needs"))
	return body.String()
}

func metric(label string, value int, detail string) string {
	return cardBorder.Render(fmt.Sprintf("%s\n%s %s", quiet.Render(label), accent.Render(fmt.Sprint(value)), detail))
}

func (model Model) providerListView() string {
	var body strings.Builder
	body.WriteString(panelTitle.Render("Providers") + "\n")
	if len(model.workbench.Providers) == 0 {
		return body.String() + "\n" + quiet.Render("No providers match this search.")
	}
	start, end := windowRange(len(model.workbench.Providers), model.cursor, model.listHeight())
	for index := start; index < end; index++ {
		provider := model.workbench.Providers[index]
		line := fmt.Sprintf("%-18s %s", provider.Connector.Display, stateBadge(provider.State))
		body.WriteString(selectLine(index == model.cursor, line) + "\n")
	}
	return strings.TrimRight(body.String(), "\n")
}

func (model Model) providerOverviewView() string {
	provider, ok := model.currentProvider()
	if !ok {
		return quiet.Render("Choose a provider to see setup options.")
	}
	var body strings.Builder
	fmt.Fprintf(&body, "%s  %s\n%s\n\n", accent.Render(provider.Connector.Display), stateBadge(provider.State), provider.Connector.Description)
	body.WriteString("Best next step\n")
	if len(provider.Recipes) > 0 {
		recipe := provider.Recipes[0]
		fmt.Fprintf(&body, "  %s\n  %s\n", recipe.Label, quiet.Render(recipe.Reason))
	}
	if len(provider.Connections) > 0 {
		body.WriteString("\nAccounts\n")
		for _, connection := range provider.Connections {
			fmt.Fprintf(&body, "  %s  %s via %s\n", statusDot(connection.Status), connection.Name, connection.SelectedMode)
		}
	}
	if len(provider.Imports) > 0 || provider.Connector.DetectedCredentialMethod != "" {
		body.WriteString("\nDetected on this machine\n")
		for _, item := range provider.Connector.ExistingSetup {
			fmt.Fprintf(&body, "  %s %s\n", warning.Render("●"), item)
		}
	}
	fmt.Fprintf(&body, "\n%s\n", quiet.Render(fmt.Sprintf("%d setup recipe(s) · %d operation(s) · %d installed skill(s)", len(provider.Recipes), providerOperationCount(provider), len(provider.Skills))))
	body.WriteString("\n" + keyStyle.Render("enter") + " manage and connect")
	return body.String()
}

func (model Model) providerScreenView() string {
	provider, ok := model.currentProvider()
	if !ok {
		return quiet.Render("Provider is no longer available. Press esc to return.")
	}
	return model.twoPane(model.providerRowsView(provider), model.providerSelectionView(provider))
}

func (model Model) providerRowsView(provider application.ProviderCard) string {
	var body strings.Builder
	body.WriteString(panelTitle.Render(provider.Connector.Display) + "  " + stateBadge(provider.State) + "\n")
	row := 0
	if len(provider.Connections) > 0 {
		body.WriteString("\n" + quiet.Render("ACCOUNTS") + "\n")
		for _, connection := range provider.Connections {
			line := fmt.Sprintf("%s %-18s %s", statusDot(connection.Status), connection.Name, quiet.Render(connection.SelectedMode))
			body.WriteString(selectLine(row == model.providerCursor, line) + "\n")
			row++
		}
	}
	body.WriteString("\n" + quiet.Render("WAYS TO CONNECT") + "\n")
	for _, recipe := range provider.Recipes {
		var tags []string
		if recipe.Detected {
			tags = append(tags, "detected")
		}
		if recipe.Importable {
			tags = append(tags, "import")
		}
		if recipe.Recommended {
			tags = append(tags, "recommended")
		}
		if recipe.BlockedReason != "" {
			tags = append(tags, "setup needed")
		}
		line := recipe.Label
		if len(tags) > 0 {
			line += "  " + warning.Render(strings.Join(tags, " · "))
		}
		body.WriteString(selectLine(row == model.providerCursor, line) + "\n")
		row++
	}
	return strings.TrimRight(body.String(), "\n")
}

func (model Model) providerSelectionView(provider application.ProviderCard) string {
	if connection, ok := model.selectedConnection(provider); ok {
		return model.connectionView(provider, connection)
	}
	if recipe, ok := model.selectedRecipe(provider); ok {
		return model.recipeView(recipe)
	}
	return model.providerOverviewView()
}

func (model Model) connectionView(provider application.ProviderCard, connection state.Connection) string {
	var body strings.Builder
	fmt.Fprintf(&body, "%s  %s\n", accent.Render(connection.Name), connectionBadge(connection.Status))
	fmt.Fprintf(&body, "Account for %s\n\n", provider.Connector.Display)
	fmt.Fprintf(&body, "Execution    %s\nAuthentication %s\n", connection.SelectedMode, connection.CredentialMethod)
	if credential, ok := credentialFor(provider, connection); ok {
		identity := firstNonempty(credential.AccountName, credential.PrincipalName, credential.AccountID, credential.Environment)
		fmt.Fprintf(&body, "Identity     %s\nCredential   %s · %s\nStored       outfit vault / OS keychain\nVerified     %s\n", identity, credential.DisplayName, credential.Fingerprint, formatTime(credential.LastVerifiedAt))
	}
	for _, key := range []string{"account_name", "account_id", "environment", "principal", "capabilities", "verified"} {
		if value := connection.SafeIdentity[key]; value != "" {
			fmt.Fprintf(&body, "%-13s%s\n", titleLabel(key), value)
		}
	}
	body.WriteString("\nAvailable to agents\n")
	mode := modeByID(provider, connection.SelectedMode)
	if len(mode.Operations) == 0 {
		body.WriteString(quiet.Render("  Operations are discovered from the connected executor."))
	} else {
		for _, operation := range mode.Operations[:min(6, len(mode.Operations))] {
			fmt.Fprintf(&body, "  %s %s\n", classIcon(operation.Class), firstNonempty(operation.Title, operation.Name))
		}
		if len(mode.Operations) > 6 {
			fmt.Fprintf(&body, "  %s\n", quiet.Render(fmt.Sprintf("+ %d more", len(mode.Operations)-6)))
		}
	}
	if len(provider.Tools) > 0 || len(provider.Skills) > 0 {
		body.WriteString("\nManaged runtime\n")
		for _, tool := range provider.Tools {
			status := "available on PATH"
			if tool.Current != nil {
				status = tool.Current.Version + " managed by outfit"
			}
			fmt.Fprintf(&body, "  CLI     %s · %s\n", tool.Display, status)
		}
		fmt.Fprintf(&body, "  Skills  %d installed for connected hosts\n", len(provider.Skills))
	}
	if len(provider.Connector.Links) > 0 {
		mode := provider.LinkModes[connection.Name]
		if mode == "" {
			mode = linkcontext.ModeHeavy
		}
		body.WriteString("\nAutomatic link context  " + linkModeBadge(mode) + "\n")
		detail := "Fetches and injects the richest bounded resource content before the agent starts thinking."
		if mode == linkcontext.ModeLight {
			detail = "Fetches compact metadata and a short excerpt before the agent starts thinking."
		} else if mode == linkcontext.ModeOff {
			detail = "Pasted links are not resolved automatically."
		}
		body.WriteString(quiet.Render("  "+detail) + "\n")
		for _, resolver := range provider.Connector.Links[:min(4, len(provider.Connector.Links))] {
			fmt.Fprintf(&body, "  %s  %s\n", good.Render("↗"), firstNonempty(resolver.ResourceType, resolver.ID))
		}
		if len(provider.Connector.Links) > 4 {
			fmt.Fprintf(&body, "  %s\n", quiet.Render(fmt.Sprintf("+ %d more URL type(s)", len(provider.Connector.Links)-4)))
		}
		body.WriteString(quiet.Render("  Press l to cycle heavy → light → off. Project workspaces can further restrict URL types.") + "\n")
	}
	body.WriteString("\n" + keyStyle.Render("t") + " test  " + keyStyle.Render("p") + " sign-in  " + keyStyle.Render("n") + " reuse  " + keyStyle.Render("l") + " links  " + keyStyle.Render("d") + " disconnect")
	return body.String()
}

func (model Model) recipeView(recipe application.SetupRecipe) string {
	var body strings.Builder
	body.WriteString(accent.Render(recipe.Label))
	if recipe.Detected {
		body.WriteString("  " + warning.Render("DETECTED"))
	} else if recipe.Importable {
		body.WriteString("  " + good.Render("READY TO IMPORT"))
	} else if recipe.Recommended {
		body.WriteString("  " + good.Render("RECOMMENDED"))
	}
	body.WriteString("\n")
	body.WriteString(firstNonempty(recipe.Description, recipe.Reason) + "\n\n")
	if recipe.BlockedReason != "" {
		body.WriteString(warning.Render("NOT READY YET") + "\n" + recipe.BlockedReason + "\n\n")
	}
	if recipe.Import != nil {
		fmt.Fprintf(&body, "Found in       %s\nEndpoint       %s\n", strings.Join(recipe.Import.Hosts, " + "), recipe.Import.Endpoint)
		if len(recipe.Import.Scopes) > 0 {
			fmt.Fprintf(&body, "Scopes         %s\n", strings.Join(recipe.Import.Scopes, ", "))
		}
		body.WriteString("\nTransfer verifies and refreshes the account first, stores it in outfit, backs up host configuration, then removes the duplicate host MCP entry.\n")
	} else {
		fmt.Fprintf(&body, "Execution      %s\nAuthentication %s\nCustody        %s\n", recipe.ModeLabel, recipe.MethodLabel, recipe.Method.Custody)
		if recipe.Mode.Executor.Tool != "" {
			fmt.Fprintf(&body, "CLI            %s (outfit can install or adopt it)\n", recipe.Mode.Executor.Tool)
		}
		if len(recipe.Mode.Capabilities) > 0 {
			fmt.Fprintf(&body, "Services       %s\n", strings.Join(recipe.Mode.Capabilities, ", "))
		}
		if recipe.Reason != "" {
			fmt.Fprintf(&body, "\nWhy this way\n  %s\n", recipe.Reason)
		}
	}
	if len(recipe.Mode.Operations) > 0 {
		body.WriteString("\nAgent surface\n")
		fmt.Fprintf(&body, "  %d operation(s) become available through this one selected mode.\n", len(recipe.Mode.Operations))
	}
	body.WriteString("\n" + keyStyle.Render("enter") + " use this setup")
	return body.String()
}

func (model Model) projectListView() string {
	var body strings.Builder
	body.WriteString(panelTitle.Render("Projects") + "\n")
	if len(model.filtered.Workspaces) == 0 {
		return body.String() + "\n" + quiet.Render("No outfit workspace in the current directory.")
	}
	for index, item := range model.filtered.Workspaces {
		line := fmt.Sprintf("%-20s %s", firstNonempty(item.Name, filepathBase(item.Root)), stateText(item.Status))
		body.WriteString(selectLine(index == model.cursor, line) + "\n")
	}
	return strings.TrimRight(body.String(), "\n")
}

func (model Model) projectPreviewView() string {
	if model.cursor >= len(model.filtered.Workspaces) {
		return quiet.Render("Projects declare which connectors a workspace may use.")
	}
	return workspaceView(model.filtered.Workspaces[model.cursor])
}

func (model Model) projectScreenView() string {
	if model.cursor >= len(model.filtered.Workspaces) {
		return quiet.Render("Project is no longer available.")
	}
	return workspaceView(model.filtered.Workspaces[model.cursor])
}

func workspaceView(item application.WorkspaceCard) string {
	profile := firstNonempty(item.Profile, "all declared connectors")
	return fmt.Sprintf("%s  %s\n\nRoot        %s\nManifest    %s\nProfile     %s\nConnectors  %d\nLocked      %t\nApproved    %t\nDrift       %t\n\n%s\n\nCommands: outfit lock · outfit apply · outfit workspace status", accent.Render(firstNonempty(item.Name, filepathBase(item.Root))), stateText(item.Status), item.Root, item.Path, profile, item.Connectors, item.Locked, item.Approved, item.Drifted, item.Detail)
}

func (model Model) systemCategoryListView() string {
	var body strings.Builder
	body.WriteString(panelTitle.Render("System") + "\n")
	for index, name := range systemCategoryNames {
		count := model.categoryCount(systemCategory(index))
		line := fmt.Sprintf("%-14s %s", name, quiet.Render(fmt.Sprintf("%d", count)))
		body.WriteString(selectLine(index == model.cursor, line) + "\n")
	}
	return strings.TrimRight(body.String(), "\n")
}

func (model Model) systemCategoryPreviewView() string {
	category := systemCategory(model.cursor)
	details := []string{
		"Install tested CLI and skill updates together.",
		"Adopt binaries already on PATH or install outfit-pinned builds.",
		"Skills are generated and installed only for connected providers.",
		"Create or restore encrypted, portable outfit state.",
		"Inspect connection and operation outcomes without exposing secrets.",
	}
	return fmt.Sprintf("%s\n%s\n\n%s\n", accent.Render(systemCategoryNames[category]), details[category], quiet.Render(fmt.Sprintf("%d item(s)", model.categoryCount(category))))
}

func (model Model) categoryCount(category systemCategory) int {
	switch category {
	case updatesCategory:
		return len(model.filtered.Updates)
	case toolsCategory:
		return len(model.filtered.Tools)
	case skillsCategory:
		return len(model.filtered.Skills)
	case backupsCategory:
		return 2
	case activityCategory:
		return len(model.filtered.Activity)
	default:
		return 0
	}
}

func (model Model) systemScreenView() string {
	return model.twoPane(model.systemItemsView(), model.systemItemDetailView())
}

func (model Model) systemItemsView() string {
	var body strings.Builder
	body.WriteString(panelTitle.Render(systemCategoryNames[model.systemCategory]) + "\n")
	count := model.systemItemCount()
	if count == 0 {
		if model.systemCategory == updatesCategory {
			return body.String() + "\n" + good.Render("✓ Everything is current")
		}
		return body.String() + "\n" + quiet.Render("No items.")
	}
	start, end := windowRange(count, model.cursor, model.listHeight())
	for index := start; index < end; index++ {
		body.WriteString(selectLine(index == model.cursor, model.systemItemLine(index)) + "\n")
	}
	if end < count {
		fmt.Fprintf(&body, "%s\n", dim.Render(fmt.Sprintf("  … %d more", count-end)))
	}
	return strings.TrimRight(body.String(), "\n")
}

func (model Model) systemItemLine(index int) string {
	switch model.systemCategory {
	case updatesCategory:
		item := model.filtered.Updates[index]
		return fmt.Sprintf("%-18s %s → %s", item.ID, item.Current, item.Available)
	case toolsCategory:
		item := model.filtered.Tools[index]
		status := "not installed"
		if item.Current != nil {
			status = item.Current.Version
		} else if item.DetectedPath != "" {
			status = "on PATH"
		}
		return fmt.Sprintf("%-20s %s", item.Display, quiet.Render(status))
	case skillsCategory:
		item := model.filtered.Skills[index]
		return fmt.Sprintf("%-24s %s", item.ID, enabledBadge(item.Enabled))
	case backupsCategory:
		if index == 0 {
			return "Create encrypted backup"
		}
		return "Restore encrypted backup"
	case activityCategory:
		item := model.filtered.Activity[index]
		return fmt.Sprintf("%s %-16s %s", item.At.Local().Format("01-02 15:04"), item.Connection, item.Outcome)
	default:
		return ""
	}
}

func (model Model) systemItemDetailView() string {
	if model.systemItemCount() == 0 {
		return model.systemCategoryPreviewView()
	}
	switch model.systemCategory {
	case updatesCategory:
		item := model.filtered.Updates[model.cursor]
		return fmt.Sprintf("%s\n\nKind       %s\nCurrent    %s\nAvailable  %s\n\n%s\n\n%s apply verified update", accent.Render(item.ID), item.Kind, item.Current, item.Available, item.Detail, keyStyle.Render("enter"))
	case toolsCategory:
		return toolView(model.filtered.Tools[model.cursor])
	case skillsCategory:
		return skillView(model.filtered.Skills[model.cursor])
	case backupsCategory:
		if model.cursor == 0 {
			return "Create encrypted backup\n\nConnections, outfit-managed credentials, CLI pins, and skill preferences are encrypted into one portable file.\n\n" + keyStyle.Render("enter") + " create"
		}
		return "Restore encrypted backup\n\nThe complete file is authenticated and validated before local state changes.\n\n" + keyStyle.Render("enter") + " restore"
	case activityCategory:
		item := model.filtered.Activity[model.cursor]
		return fmt.Sprintf("%s  %s\n\nConnection  %s\nOperation   %s\nClass       %s\nOutcome     %s\nDetail      %v", accent.Render(item.At.Local().Format(time.RFC3339)), item.Outcome, item.Connection, item.Operation, item.Class, item.Outcome, item.Detail)
	}
	return ""
}

func toolView(item application.ToolCard) string {
	var body strings.Builder
	fmt.Fprintf(&body, "%s\n%s\n\nBinary       %s\n", accent.Render(item.Display), item.Description, item.Binary)
	if item.Current != nil {
		fmt.Fprintf(&body, "Status       managed by outfit\nVersion      %s\nPlatform     %s\nPath         %s\nSHA-256      %s\n", item.Current.Version, item.Current.Platform, item.Current.Path, item.Current.ActualSHA)
	} else if item.DetectedPath != "" {
		fmt.Fprintf(&body, "Status       detected on PATH\nPath         %s\n", item.DetectedPath)
	} else {
		body.WriteString("Status       not installed\n")
	}
	if item.RecommendedVersion != "" {
		fmt.Fprintf(&body, "Tested       %s\n\n%s install/update", item.RecommendedVersion, keyStyle.Render("u"))
	}
	if item.DetectedPath != "" && item.Current == nil {
		body.WriteString("  " + keyStyle.Render("a") + " adopt existing")
	}
	return body.String()
}

func skillView(item application.SkillCard) string {
	return fmt.Sprintf("%s  %s\n\nConnection  %s\nHost        %s\nKind        %s\nSource      %s\nRef         %s\nPath        %s\nInstalled   %s\n\n%s enable/disable · %s validate/sync", accent.Render(item.ID), enabledBadge(item.Enabled), item.Connection, item.Host, item.Kind, item.Source, item.Ref, item.Path, item.InstalledPath, keyStyle.Render("x"), keyStyle.Render("u"))
}

func (model Model) wizardView() string {
	var body strings.Builder
	title := "Connect " + model.wizardConnector.Display
	if model.adoptionID != "" {
		title = "Import " + model.wizardConnector.Display + " into outfit"
	} else if model.reuseCredential != "" {
		title = "Reuse account for " + model.wizardConnector.Display
	}
	body.WriteString(accent.Render(title) + "\n")
	body.WriteString(sectionHint.Render(model.status) + "\n\n")
	for index, field := range model.fields {
		marker := "  "
		if index == model.fieldIndex {
			marker = accent.Render("› ")
		}
		required := ""
		if field.required {
			required = quiet.Render(" required")
		}
		fmt.Fprintf(&body, "%s%s%s\n", marker, field.label, required)
		if field.description != "" {
			fmt.Fprintf(&body, "  %s\n", quiet.Render(field.description))
		}
		if field.detectedFrom != "" {
			fmt.Fprintf(&body, "  %s\n", good.Render("Detected "+field.detectedFrom+"; value remains hidden."))
		}
		fmt.Fprintf(&body, "  %s\n\n", field.input.View())
	}
	if model.adoptionID != "" {
		body.WriteString(quiet.Render("outfit verifies and refreshes the account before changing host configuration. A backup is created automatically."))
	} else {
		body.WriteString(quiet.Render("outfit commits the verified account, selected execution recipe, CLI version, and generated skills together."))
	}
	return body.String()
}

func (model Model) backupWizardView() string {
	var body strings.Builder
	body.WriteString(accent.Render(titleLabel(model.backupAction)+" encrypted backup") + "\n\n")
	for index, field := range model.fields {
		marker := "  "
		if index == model.fieldIndex {
			marker = accent.Render("› ")
		}
		fmt.Fprintf(&body, "%s%s\n  %s\n\n", marker, field.label, field.input.View())
	}
	body.WriteString(quiet.Render("The passphrase is never stored. Restore validates the complete encrypted file before changing local state."))
	return body.String()
}

func (model Model) paletteView() string {
	var body strings.Builder
	body.WriteString(accent.Render("Command palette") + "\n\n")
	body.WriteString(model.palette.View() + "\n\n")
	visible := model.filteredCommands()
	if len(visible) == 0 {
		body.WriteString(quiet.Render("No matching commands."))
	}
	for index, item := range visible {
		line := fmt.Sprintf("%-30s %s", item.name, quiet.Render(item.hint))
		body.WriteString(selectLine(index == model.paletteCursor, line) + "\n")
	}
	return strings.TrimRight(body.String(), "\n")
}

func (model Model) helpView() string {
	return accent.Render("Keyboard help") + `

Navigate
  tab / shift+tab     switch Home, Connectors, Projects, System
  ↑/↓ or j/k          move within the current view
  enter               open or use the selected item
  esc / ←             go back

Find and act
  /                   search the current view
  :                   command palette from anywhere
  r                   refresh local accounts and host integrations
  ?                   this help

Connector detail
  a                   use the first recommended/detected setup
  t                   test selected connection
  p                   replace selected sign-in atomically
  l                   cycle pasted-link context: heavy, light, off
  d twice             disconnect and destroy managed credential

System detail
  u                   install/update selected CLI or skill
  a                   adopt a detected PATH binary
  x                   enable/disable a non-generated skill

  q                   quit`
}

func (model Model) footer() string {
	if model.width < 92 {
		switch model.screen {
		case providerScreen:
			return "↑/↓ choose · enter use · a best · t test · l links · d disconnect · esc back"
		case projectScreen:
			return "esc back · : commands · ? help"
		case systemScreen:
			return "↑/↓ move · enter apply · u update · esc back · ? help"
		default:
			return "tab sections · ↑/↓ move · enter open · / search · : commands · ? help"
		}
	}
	base := "tab sections · ↑/↓ move · enter open · / search · : commands · r refresh · ? help · q quit"
	switch model.screen {
	case providerScreen:
		return "↑/↓ account or setup · enter use · a best · t test · p sign-in · n reuse · l links · d disconnect · esc back"
	case projectScreen:
		return "esc back · : commands · ? help"
	case systemScreen:
		return "↑/↓ move · enter apply/open · u update · a adopt · x toggle · esc back · : commands"
	default:
		return base
	}
}

func (model Model) listHeight() int {
	if model.width < 92 {
		return max(4, (model.height-13)/2)
	}
	return max(6, model.height-12)
}

func clipLines(value string, height int) string {
	lines := strings.Split(value, "\n")
	if len(lines) <= height {
		return value
	}
	if height <= 1 {
		return quiet.Render("…")
	}
	return strings.Join(lines[:height-1], "\n") + "\n" + quiet.Render("…")
}

func windowRange(count, cursor, height int) (int, int) {
	if count <= height {
		return 0, count
	}
	start := cursor - height/2
	if start < 0 {
		start = 0
	}
	if start+height > count {
		start = count - height
	}
	return start, start + height
}

func selectLine(active bool, value string) string {
	if active {
		return selected.Render("› " + value)
	}
	return "  " + value
}

func stateBadge(state application.ProviderState) string {
	switch state {
	case application.ProviderConnected, application.ProviderState("active"):
		return good.Render("✓ connected")
	case application.ProviderDetected:
		return warning.Render("● detected")
	case application.ProviderImportable:
		return good.Render("↳ ready to import")
	case application.ProviderNeedsAttention, application.ProviderState("broken"):
		return bad.Render("! needs attention")
	default:
		return quiet.Render("○ not configured")
	}
}

func stateText(value string) string {
	if value == "active" || value == "approved" {
		return good.Render(value)
	}
	if strings.Contains(value, "need") || value == "broken" || value == "invalid" {
		return warning.Render(value)
	}
	return quiet.Render(value)
}

func statusDot(status string) string {
	if status == "active" {
		return good.Render("●")
	}
	return bad.Render("●")
}

func connectionBadge(status string) string {
	if status == "active" {
		return good.Render("✓ connected")
	}
	return bad.Render("! " + status)
}

func enabledBadge(enabled bool) string {
	if enabled {
		return good.Render("on")
	}
	return quiet.Render("off")
}

func linkModeBadge(mode linkcontext.Mode) string {
	if mode == linkcontext.ModeHeavy {
		return good.Render("heavy · default")
	}
	if mode == linkcontext.ModeLight {
		return warning.Render("light")
	}
	return quiet.Render("off")
}

func classIcon(class string) string {
	if class == "read" || class == "safe" {
		return good.Render("○")
	}
	return warning.Render("◆")
}

func providerOperationCount(provider application.ProviderCard) int {
	seen := map[string]bool{}
	for _, mode := range provider.Connector.Modes {
		for _, operation := range mode.Operations {
			seen[operation.Name] = true
		}
	}
	return len(seen)
}

func modeByID(provider application.ProviderCard, id string) kit.Mode {
	for _, mode := range provider.Connector.Modes {
		if mode.ID == id {
			return mode
		}
	}
	return kit.Mode{}
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return "—"
}

func titleLabel(value string) string {
	value = strings.ReplaceAll(value, "_", " ")
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "never"
	}
	return value.Local().Format("2006-01-02 15:04")
}

func filepathBase(value string) string {
	value = strings.TrimRight(value, "/")
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}
