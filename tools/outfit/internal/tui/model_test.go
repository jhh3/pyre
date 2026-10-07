package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jhh3/outfit/internal/application"
	"github.com/jhh3/outfit/internal/hostmcp"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/linkcontext"
	"github.com/jhh3/outfit/internal/skillmanager"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/toolchain"
)

func TestWorkbenchHasFourProductSectionsInsteadOfStorageTabs(t *testing.T) {
	model := testModel(application.Snapshot{}, homeSection)
	view := model.View()
	for _, section := range []string{"Home", "Connectors", "Projects", "System"} {
		if !strings.Contains(view, section) {
			t.Fatalf("missing product section %q: %s", section, view)
		}
	}
	for _, retiredTab := range []string{"Imports", "Connections", "Credentials", "Backups", "Activity"} {
		if strings.Contains(strings.Split(view, "\n")[1], retiredTab) {
			t.Fatalf("internal entity %q is still a top-level tab: %s", retiredTab, view)
		}
	}
}

func TestProviderListUsesDistinctTruthfulStates(t *testing.T) {
	snapshot := application.Snapshot{
		Connectors: []application.ConnectorCard{
			{Key: "github", Display: "GitHub", RecommendedMode: "cli", Modes: []kit.Mode{{ID: "cli"}}},
			{Key: "linear", Display: "Linear", RecommendedMode: "mcp", Modes: []kit.Mode{{ID: "mcp"}}},
			{Key: "stripe", Display: "Stripe", RecommendedMode: "cli", DetectedCredentialMethod: "native-login", Modes: []kit.Mode{{ID: "cli"}}},
		},
		Connections: []state.Connection{{Name: "github", ConnectorKey: "github", Status: "active", SelectedMode: "cli"}},
		Imports:     []hostmcp.Candidate{{ID: "linear", ConnectorKey: "linear", Hosts: []string{"claude"}, Transferable: true}},
	}
	model := testModel(snapshot, connectorsSection)
	view := model.View()
	for _, want := range []string{"✓ connected", "ready to import", "detected"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing state %q: %s", want, view)
		}
	}
}

func TestSetupRecipesFlattenModeAndCredentialAndPreferDetectedLogin(t *testing.T) {
	card := application.ConnectorCard{
		Key: "google-workspace", Display: "Google Workspace", RecommendedMode: "cli", DetectedCredentialMethod: "native-login",
		Modes:   []kit.Mode{{ID: "cli", Label: "gws CLI", CredentialMethods: []string{"managed", "native-login"}, DefaultCredentialMethod: "managed", Recommendation: kit.Recommendation{Rank: 0, Reason: "best coverage"}}},
		Methods: []kit.CredentialMethod{{ID: "managed", Label: "outfit-managed OAuth"}, {ID: "native-login", Label: "Use existing gws login", Description: "Import the authenticated local account"}},
	}
	snapshot := application.Snapshot{Connectors: []application.ConnectorCard{card}}
	model := testModel(snapshot, connectorsSection)
	opened, _ := model.openSelected()
	result := opened.(Model)
	if result.screen != providerScreen {
		t.Fatalf("screen = %v", result.screen)
	}
	view := result.View()
	if !strings.Contains(view, "gws CLI · Use existing gws login") || !strings.Contains(view, "detected") {
		t.Fatalf("flattened detected recipe missing: %s", view)
	}
	provider, _ := result.currentProvider()
	if provider.Recipes[0].MethodID != "native-login" {
		t.Fatalf("first recipe = %#v", provider.Recipes[0])
	}
}

func TestProviderDetailNestsAccountCredentialRuntimeAndSkills(t *testing.T) {
	card := application.ConnectorCard{
		Key: "github", Display: "GitHub", RecommendedMode: "cli",
		Modes: []kit.Mode{{ID: "cli", Label: "gh + git", Executor: kit.Executor{Tool: "gh"}, Operations: []kit.Operation{{Name: "repo.view", Title: "View repository", Class: "read"}}}},
	}
	snapshot := application.Snapshot{
		Connectors:  []application.ConnectorCard{card},
		Connections: []state.Connection{{ID: "con_1", Name: "github", ConnectorKey: "github", SelectedMode: "cli", CredentialMethod: "native-login", CredentialID: "cred_1", Status: "active"}},
		Credentials: []state.CredentialRecord{{ID: "cred_1", ConnectorKey: "github", DisplayName: "GitHub account", Fingerprint: "sha256:abc", AccountName: "jhh3"}},
		Tools:       []application.ToolCard{{ID: "gh", Display: "GitHub CLI", Current: &toolchain.Receipt{Version: "2.80.0"}}},
		Skills:      []application.SkillCard{{Receipt: skillmanager.Receipt{ID: "github-repos", Connector: "github", Connection: "github", Enabled: true}}},
	}
	model := testModel(snapshot, connectorsSection)
	model.providerKey, model.screen = "github", providerScreen
	view := model.View()
	for _, want := range []string{"native-login", "jhh3", "sha256:abc", "outfit vault / OS keychain", "View repository", "GitHub CLI", "1 installed"} {
		if !strings.Contains(view, want) {
			t.Fatalf("provider detail missing %q: %s", want, view)
		}
	}
}

func TestProviderDetailMakesAutomaticLinkContextVisibleAndToggleable(t *testing.T) {
	card := application.ConnectorCard{
		Key: "notion", Display: "Notion",
		Modes: []kit.Mode{{ID: "hosted_mcp", Label: "Hosted MCP"}},
		Links: []kit.LinkResolver{{ID: "page", ResourceType: "notion.page"}},
	}
	snapshot := application.Snapshot{
		Connectors:  []application.ConnectorCard{card},
		Connections: []state.Connection{{ID: "con_1", Name: "notion", ConnectorKey: "notion", SelectedMode: "hosted_mcp", Status: "active"}},
		LinkModes:   map[string]linkcontext.Mode{"notion": linkcontext.ModeHeavy},
	}
	model := testModel(snapshot, connectorsSection)
	model.providerKey, model.screen = "notion", providerScreen
	view := model.View()
	for _, want := range []string{"Automatic link context", "heavy · default", "notion.page", "Press l to cycle"} {
		if !strings.Contains(view, want) {
			t.Fatalf("link context control missing %q: %s", want, view)
		}
	}
	updated, command := model.updateProvider(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("l")})
	if command == nil || updated.(Model).screen != busyScreen {
		t.Fatal("link enrichment toggle did not start an in-place account action")
	}
}

func TestHostOAuthImportLivesInsideProviderRecipes(t *testing.T) {
	item := hostmcp.Candidate{ID: "linear-123", ConnectorKey: "linear", Display: "Linear", Endpoint: "https://mcp.linear.app/mcp", Hosts: []string{"claude", "codex"}, Scopes: []string{"read", "write"}, Transferable: true}
	card := application.ConnectorCard{Key: "linear", Display: "Linear", Modes: []kit.Mode{{ID: "mcp", Label: "Hosted MCP"}}}
	model := testModel(application.Snapshot{Connectors: []application.ConnectorCard{card}, Imports: []hostmcp.Candidate{item}}, connectorsSection)
	model.providerKey, model.screen = "linear", providerScreen
	view := model.View()
	if !strings.Contains(view, "Import OAuth from claude + codex") || !strings.Contains(view, "READY TO IMPORT") || !strings.Contains(view, "host configuration") {
		t.Fatalf("integrated import recipe missing: %s", view)
	}
	provider, _ := model.currentProvider()
	updated, _ := model.beginRecipe(provider, provider.Recipes[0])
	result := updated.(Model)
	if result.screen != wizardScreen || result.adoptionID != item.ID {
		t.Fatalf("import recipe did not open transfer wizard: %#v", result)
	}
	updated, command := result.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(Model).screen != busyScreen || command == nil {
		t.Fatal("host import attempted to read a nonexistent mode instead of starting transfer")
	}
}

func TestConnectorWizardMasksSecretAndRetainsItAfterFailure(t *testing.T) {
	card := application.ConnectorCard{
		Key: "stripe", Display: "Stripe", RecommendedMode: "cli",
		Modes:   []kit.Mode{{ID: "cli", Label: "Stripe CLI", CredentialMethods: []string{"managed"}, DefaultCredentialMethod: "managed"}},
		Methods: []kit.CredentialMethod{{ID: "managed", Label: "Managed key", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Restricted key", Secret: true}}}},
	}
	model := testModel(application.Snapshot{Connectors: []application.ConnectorCard{card}}, connectorsSection)
	model.modeIndex, model.methodIndex, model.providerKey = 0, 0, "stripe"
	model.beginWizard(card)
	model.fields[1].input.SetValue("rk_test_supersecret")
	view := model.wizardView()
	if strings.Contains(view, "rk_test_supersecret") || !strings.Contains(view, "•") {
		t.Fatalf("secret was not masked: %q", view)
	}
	updated, _ := model.Update(actionResult{err: errors.New("verification failed"), failure: wizardScreen})
	retained := updated.(Model)
	if retained.screen != wizardScreen || retained.fields[1].input.Value() != "rk_test_supersecret" {
		t.Fatal("verification failure discarded wizard state")
	}
}

func TestBusySetupShowsBrowserApprovalProgress(t *testing.T) {
	progress := make(chan actionProgress, 1)
	model := testModel(application.Snapshot{}, connectorsSection)
	model.screen = busyScreen
	model.actionProgress = progress
	updated, command := model.Update(actionProgress{stage: "browser", detail: "Open https://connect.example.test and confirm verification code blue-cat"})
	result := updated.(Model)
	if command == nil || !strings.Contains(result.View(), "https://connect.example.test") || !strings.Contains(result.View(), "blue-cat") {
		t.Fatalf("browser approval was not visible while setup waited: %s", result.View())
	}
	close(progress)
}

func TestConnectorWizardShowsFieldInstructionsAndDetectedEnvironment(t *testing.T) {
	t.Setenv("ACME_TOKEN", "secret-from-env")
	card := application.ConnectorCard{
		Key: "acme", Display: "Acme", RecommendedMode: "rest",
		Modes:   []kit.Mode{{ID: "rest", Label: "REST", CredentialMethods: []string{"managed"}, DefaultCredentialMethod: "managed"}},
		Methods: []kit.CredentialMethod{{ID: "managed", Label: "Managed token", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Access token", Description: "Create it in Acme Settings → API.", Secret: true, Environment: []string{"ACME_TOKEN"}}}}},
	}
	model := testModel(application.Snapshot{Connectors: []application.ConnectorCard{card}}, connectorsSection)
	model.modeIndex, model.methodIndex = 0, 0
	model.beginWizard(card)
	view := model.wizardView()
	for _, want := range []string{"Create it in Acme Settings", "Detected ACME_TOKEN", "value remains hidden", "••"} {
		if !strings.Contains(view, want) {
			t.Fatalf("wizard omitted %q: %s", want, view)
		}
	}
	if strings.Contains(view, "secret-from-env") {
		t.Fatalf("wizard exposed the detected secret: %s", view)
	}
}

func TestBlockedHostedOAuthRecipeDoesNotOpenAnEmptyWizard(t *testing.T) {
	recipe := application.SetupRecipe{Label: "Hosted MCP", BlockedReason: "Connect it in Claude Code or Codex first."}
	model := testModel(application.Snapshot{}, connectorsSection)
	updated, command := model.beginRecipe(application.ProviderCard{}, recipe)
	result := updated.(Model)
	if command != nil || result.screen == wizardScreen || result.err != recipe.BlockedReason {
		t.Fatalf("blocked recipe started anyway: %#v", result)
	}
}

func TestConnectorWizardPreloadsDeclaredEnvironmentBehindMask(t *testing.T) {
	t.Setenv("ACME_TOKEN", "secret-from-env")
	card := application.ConnectorCard{
		Key: "acme", Display: "Acme", ExistingSetup: []string{"ACME_TOKEN"},
		Modes:   []kit.Mode{{ID: "api", Label: "API", CredentialMethods: []string{"managed"}, DefaultCredentialMethod: "managed"}},
		Methods: []kit.CredentialMethod{{ID: "managed", Label: "Managed", Fields: []kit.CredentialField{{Key: "TOKEN", Label: "Token", Secret: true, Environment: []string{"ACME_TOKEN"}}}}},
	}
	model := testModel(application.Snapshot{}, connectorsSection)
	model.beginWizard(card)
	view := model.wizardView()
	if model.fields[1].input.Value() != "secret-from-env" || strings.Contains(view, "secret-from-env") || !strings.Contains(model.status, "loaded and hidden") {
		t.Fatalf("environment preload was not safe: value=%q status=%q view=%q", model.fields[1].input.Value(), model.status, view)
	}
}

func TestWideWorkbenchUsesPersistentTwoPaneAndNarrowStacks(t *testing.T) {
	snapshot := application.Snapshot{Connectors: []application.ConnectorCard{{Key: "github", Display: "GitHub", Description: "Code hosting", Modes: []kit.Mode{{ID: "cli"}}}}}
	wide := testModel(snapshot, connectorsSection)
	wide.width = 120
	view := wide.View()
	if !strings.Contains(view, "│") || !strings.Contains(view, "Code hosting") {
		t.Fatalf("wide view is not two-pane: %q", view)
	}
	narrow := wide
	narrow.width = 52
	view = narrow.View()
	if strings.Contains(view, "│") || !strings.Contains(view, "GitHub") || !strings.Contains(view, "Code hosting") {
		t.Fatalf("narrow view did not stack: %q", view)
	}
}

func TestCommandPaletteNavigatesAndExposesGlobalActions(t *testing.T) {
	model := testModel(application.Snapshot{}, homeSection)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(":")})
	palette := updated.(Model)
	if palette.screen != paletteScreen || !strings.Contains(palette.View(), "Create encrypted backup") {
		t.Fatalf("palette did not open: %s", palette.View())
	}
	palette.palette.SetValue("projects")
	updated, _ = palette.Update(tea.KeyMsg{Type: tea.KeyEnter})
	result := updated.(Model)
	if result.section != projectsSection || result.screen != browseScreen {
		t.Fatalf("palette navigation = section %v screen %v", result.section, result.screen)
	}
}

func TestSystemConsolidatesUpdatesToolsSkillsBackupsAndActivity(t *testing.T) {
	snapshot := application.Snapshot{
		Tools:    []application.ToolCard{{ID: "gws", Display: "Google Workspace CLI"}},
		Skills:   []application.SkillCard{{Receipt: skillmanager.Receipt{ID: "gws-gmail", Enabled: true}}},
		Updates:  []application.Update{{ID: "gws", Current: "1", Available: "2"}},
		Activity: []application.ActivityCard{{ID: 1, Connection: "google-workspace", Outcome: "ok"}},
	}
	model := testModel(snapshot, systemSection)
	view := model.View()
	for _, want := range []string{"Updates", "CLIs", "Skills", "Backups", "Activity"} {
		if !strings.Contains(view, want) {
			t.Fatalf("system inventory missing %q: %s", want, view)
		}
	}
	if strings.Contains(strings.Split(view, "\n")[1], "CLIs") {
		t.Fatalf("system internals leaked back into top navigation: %s", view)
	}
}

func TestEmptySystemCategoryIgnoresApplyWithoutPanicking(t *testing.T) {
	model := testModel(application.Snapshot{}, systemSection)
	model.screen, model.systemCategory = systemScreen, updatesCategory
	updated, command := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if updated.(Model).screen != systemScreen || command != nil {
		t.Fatalf("empty category changed state: %#v", updated)
	}
}

func TestBackupCategoryOpensCreateAndRestoreForms(t *testing.T) {
	for key, action := range map[string]string{"c": "create", "R": "restore"} {
		model := testModel(application.Snapshot{}, systemSection)
		model.screen, model.systemCategory = systemScreen, backupsCategory
		updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		result := updated.(Model)
		if result.screen != backupWizardScreen || result.backupAction != action || len(result.fields) != 3 {
			t.Fatalf("%s did not open %s form: %#v", key, action, result)
		}
		if result.fields[1].input.EchoMode != textinput.EchoPassword {
			t.Fatal("backup passphrase input is not masked")
		}
	}
}

func testModel(snapshot application.Snapshot, section section) Model {
	search := textinput.New()
	search.Placeholder = "Search this view"
	palette := textinput.New()
	spin := spinner.New()
	model := Model{
		snapshot: snapshot, filtered: snapshot, workbench: application.BuildWorkbench(snapshot),
		section: section, screen: browseScreen, width: 120, height: 32,
		search: search, palette: palette, spinner: spin,
	}
	return model
}
