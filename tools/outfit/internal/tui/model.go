package tui

import (
	"context"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jhh3/outfit/internal/application"
	"github.com/jhh3/outfit/internal/kit"
)

type section int

const (
	homeSection section = iota
	connectorsSection
	projectsSection
	systemSection
)

var sectionNames = []string{"Home", "Connectors", "Projects", "System"}

type screen int

const (
	browseScreen screen = iota
	providerScreen
	projectScreen
	systemScreen
	wizardScreen
	backupWizardScreen
	busyScreen
	helpScreen
	paletteScreen
)

type systemCategory int

const (
	updatesCategory systemCategory = iota
	toolsCategory
	skillsCategory
	backupsCategory
	activityCategory
)

var systemCategoryNames = []string{"Updates", "CLIs", "Skills", "Backups", "Activity"}

type formField struct {
	key, label, description, detectedFrom string
	required                              bool
	input                                 textinput.Model
}

type command struct {
	name, hint string
	action     string
}

var commands = []command{
	{name: "Go to Home", hint: "status and next actions", action: "home"},
	{name: "Go to Connectors", hint: "connect and manage providers", action: "connectors"},
	{name: "Go to Projects", hint: "workspace access", action: "projects"},
	{name: "Go to System", hint: "updates, CLIs, skills, backups, activity", action: "system"},
	{name: "Refresh discovery", hint: "recheck local auth and host imports", action: "refresh"},
	{name: "Create encrypted backup", hint: "portable outfit state", action: "backup-create"},
	{name: "Restore encrypted backup", hint: "validate before changing state", action: "backup-restore"},
	{name: "Quit outfit", hint: "close the workbench", action: "quit"},
}

type Model struct {
	ctx       context.Context
	service   application.Service
	snapshot  application.Snapshot
	filtered  application.Snapshot
	workbench application.Workbench

	section section
	screen  screen
	cursor  int
	width   int
	height  int

	providerKey    string
	providerCursor int
	systemCategory systemCategory
	returnScreen   screen

	search        textinput.Model
	palette       textinput.Model
	paletteCursor int
	status        string
	err           string
	confirm       string
	refreshing    bool

	modeIndex       int
	methodIndex     int
	fields          []formField
	fieldIndex      int
	backupAction    string
	reuseCredential string
	adoptionID      string
	wizardConnector application.ConnectorCard
	spinner         spinner.Model
	actionProgress  <-chan actionProgress
}

type actionResult struct {
	snapshot application.Snapshot
	status   string
	err      error
	failure  screen
}

type refreshResult struct {
	snapshot application.Snapshot
	err      error
}

type actionProgress struct {
	stage, detail string
}

func New(ctx context.Context, service application.Service) (Model, error) {
	snapshot, err := service.SnapshotLocal(ctx)
	if err != nil {
		return Model{}, err
	}
	search := textinput.New()
	search.Placeholder = "Search this view"
	search.Prompt = "/ "
	search.CharLimit = 120
	search.Width = 48
	palette := textinput.New()
	palette.Placeholder = "Type a command"
	palette.Prompt = ": "
	palette.CharLimit = 120
	palette.Width = 56
	spin := spinner.New()
	spin.Spinner = spinner.Dot
	model := Model{
		ctx: ctx, service: service, snapshot: snapshot, filtered: snapshot,
		workbench: application.BuildWorkbench(snapshot), search: search, palette: palette,
		spinner: spin, refreshing: true,
	}
	return model, nil
}

func Run(ctx context.Context, service application.Service) error {
	model, err := New(ctx, service)
	if err != nil {
		return err
	}
	_, err = tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

func (model Model) Init() tea.Cmd {
	return tea.Batch(model.spinner.Tick, model.refreshCmd())
}

func (model Model) refreshCmd() tea.Cmd {
	return func() tea.Msg {
		snapshot, err := model.service.Snapshot(model.ctx)
		return refreshResult{snapshot: snapshot, err: err}
	}
}

func (model Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	var command tea.Cmd
	switch current := message.(type) {
	case tea.WindowSizeMsg:
		model.width, model.height = current.Width, current.Height
		model.search.Width = max(20, min(54, current.Width-10))
		model.palette.Width = max(24, min(62, current.Width-10))
	case refreshResult:
		model.refreshing = false
		if current.err != nil {
			model.err = "Discovery could not finish: " + current.err.Error()
		} else {
			model.setSnapshot(current.snapshot)
			model.status = "Local accounts and host integrations checked"
			model.err = ""
		}
		return model, nil
	case actionResult:
		model.actionProgress = nil
		if current.err != nil {
			model.screen = current.failure
			model.err, model.status = current.err.Error(), ""
		} else {
			model.setSnapshot(current.snapshot)
			model.screen = model.returnAfterAction()
			model.status, model.err = current.status, ""
			model.fields, model.backupAction, model.reuseCredential, model.adoptionID = nil, "", "", ""
		}
		model.clampCursor()
		return model, nil
	case actionProgress:
		model.status = current.detail
		if current.stage != "" {
			model.status = current.stage + ": " + current.detail
		}
		return model, waitForActionProgress(model.actionProgress)
	case spinner.TickMsg:
		if !model.refreshing && model.screen != busyScreen {
			return model, nil
		}
		model.spinner, command = model.spinner.Update(message)
		return model, command
	case tea.KeyMsg:
		return model.updateKey(current)
	}
	return model, command
}

func (model Model) updateKey(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if model.screen == busyScreen {
		if key.String() == "ctrl+c" {
			return model, tea.Quit
		}
		return model, nil
	}
	if model.screen == wizardScreen {
		return model.updateWizard(key)
	}
	if model.screen == backupWizardScreen {
		return model.updateBackupWizard(key)
	}
	if model.screen == paletteScreen {
		return model.updatePalette(key)
	}
	if model.screen == helpScreen {
		if key.String() == "?" || key.String() == "esc" || key.String() == "enter" {
			model.screen = model.returnScreen
		}
		return model, nil
	}
	if model.search.Focused() {
		return model.updateSearch(key)
	}

	switch key.String() {
	case "q", "ctrl+c":
		return model, tea.Quit
	case "?":
		model.returnScreen, model.screen = model.screen, helpScreen
		return model, nil
	case ":":
		model.returnScreen, model.screen, model.paletteCursor = model.screen, paletteScreen, 0
		model.palette.SetValue("")
		model.palette.Focus()
		return model, textinput.Blink
	case "/":
		model.search.Focus()
		return model, textinput.Blink
	case "r":
		model.refreshing, model.status, model.err = true, "Checking local accounts and host integrations…", ""
		return model, tea.Batch(model.spinner.Tick, model.refreshCmd())
	}

	if model.screen == providerScreen {
		return model.updateProvider(key)
	}
	if model.screen == projectScreen {
		if key.String() == "esc" || key.String() == "left" || key.String() == "h" {
			model.screen = browseScreen
		}
		return model, nil
	}
	if model.screen == systemScreen {
		return model.updateSystem(key)
	}
	return model.updateBrowse(key)
}

func (model Model) updateBrowse(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch key.String() {
	case "tab", "right", "l":
		model.section = (model.section + 1) % section(len(sectionNames))
		model.cursor, model.confirm = 0, ""
	case "shift+tab", "left", "h":
		model.section = (model.section + section(len(sectionNames)) - 1) % section(len(sectionNames))
		model.cursor, model.confirm = 0, ""
	case "up", "k":
		if model.cursor > 0 {
			model.cursor--
		}
	case "down", "j":
		if model.cursor+1 < model.itemCount() {
			model.cursor++
		}
	case "enter", "a":
		return model.openSelected()
	case "b":
		model.beginBackupWizard("create")
		return model, textinput.Blink
	}
	return model, nil
}

func (model Model) updateSearch(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.String() == "esc" || key.String() == "enter" {
		model.search.Blur()
		model.cursor = 0
		return model, nil
	}
	if key.String() == "ctrl+c" {
		return model, tea.Quit
	}
	var command tea.Cmd
	model.search, command = model.search.Update(key)
	model.applyFilter()
	model.cursor = 0
	return model, command
}

func (model Model) updatePalette(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	visible := model.filteredCommands()
	switch key.String() {
	case "esc":
		model.palette.Blur()
		model.screen = model.returnScreen
		return model, nil
	case "ctrl+c":
		return model, tea.Quit
	case "up", "ctrl+p":
		if model.paletteCursor > 0 {
			model.paletteCursor--
		}
		return model, nil
	case "down", "ctrl+n":
		if model.paletteCursor+1 < len(visible) {
			model.paletteCursor++
		}
		return model, nil
	case "enter":
		if len(visible) == 0 {
			return model, nil
		}
		model.palette.Blur()
		return model.executeCommand(visible[model.paletteCursor].action)
	}
	var command tea.Cmd
	model.palette, command = model.palette.Update(key)
	model.paletteCursor = 0
	return model, command
}

func (model Model) executeCommand(action string) (tea.Model, tea.Cmd) {
	model.screen = browseScreen
	switch action {
	case "home":
		model.section = homeSection
	case "connectors":
		model.section = connectorsSection
	case "projects":
		model.section = projectsSection
	case "system":
		model.section = systemSection
	case "refresh":
		model.refreshing, model.status = true, "Checking local accounts and host integrations…"
		return model, tea.Batch(model.spinner.Tick, model.refreshCmd())
	case "backup-create":
		model.section, model.systemCategory = systemSection, backupsCategory
		model.beginBackupWizard("create")
		return model, textinput.Blink
	case "backup-restore":
		model.section, model.systemCategory = systemSection, backupsCategory
		model.beginBackupWizard("restore")
		return model, textinput.Blink
	case "quit":
		return model, tea.Quit
	}
	model.cursor = 0
	return model, nil
}

func (model Model) filteredCommands() []command {
	query := strings.ToLower(strings.TrimSpace(model.palette.Value()))
	if query == "" {
		return commands
	}
	var result []command
	for _, item := range commands {
		if strings.Contains(strings.ToLower(item.name+" "+item.hint), query) {
			result = append(result, item)
		}
	}
	return result
}

func (model *Model) setSnapshot(snapshot application.Snapshot) {
	model.snapshot = snapshot
	model.applyFilter()
}

func (model *Model) applyFilter() {
	model.filtered = application.Search(model.snapshot, model.search.Value())
	model.workbench = application.BuildWorkbench(model.filtered)
	model.clampCursor()
}

func (model Model) returnAfterAction() screen {
	if model.section == connectorsSection && model.providerKey != "" {
		return providerScreen
	}
	if model.section == systemSection {
		return systemScreen
	}
	return browseScreen
}

func (model *Model) clampCursor() {
	count := model.itemCount()
	if count == 0 {
		model.cursor = 0
	} else if model.cursor >= count {
		model.cursor = count - 1
	}
	if provider, ok := model.currentProvider(); ok {
		rows := len(provider.Connections) + len(provider.Recipes)
		if rows == 0 {
			model.providerCursor = 0
		} else if model.providerCursor >= rows {
			model.providerCursor = rows - 1
		}
	}
}

func (model Model) itemCount() int {
	switch model.section {
	case homeSection:
		return len(model.workbench.Home)
	case connectorsSection:
		return len(model.workbench.Providers)
	case projectsSection:
		return len(model.filtered.Workspaces)
	case systemSection:
		return len(systemCategoryNames)
	default:
		return 0
	}
}

func (model Model) currentProvider() (application.ProviderCard, bool) {
	if model.providerKey != "" {
		for _, provider := range model.workbench.Providers {
			if provider.Connector.Key == model.providerKey {
				return provider, true
			}
		}
		for _, provider := range application.BuildWorkbench(model.snapshot).Providers {
			if provider.Connector.Key == model.providerKey {
				return provider, true
			}
		}
	}
	if model.cursor < len(model.workbench.Providers) {
		return model.workbench.Providers[model.cursor], true
	}
	return application.ProviderCard{}, false
}

func (model Model) modeMethods(card application.ConnectorCard) []kit.CredentialMethod {
	if model.modeIndex < 0 || model.modeIndex >= len(card.Modes) {
		return nil
	}
	mode := card.Modes[model.modeIndex]
	var result []kit.CredentialMethod
	for _, id := range mode.CredentialMethods {
		for _, method := range card.Methods {
			if method.ID == id {
				result = append(result, method)
			}
		}
	}
	return result
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
