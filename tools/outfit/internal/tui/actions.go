package tui

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jhh3/outfit/internal/application"
	"github.com/jhh3/outfit/internal/hostmcp"
	"github.com/jhh3/outfit/internal/linkcontext"
	"github.com/jhh3/outfit/internal/state"
)

func (model Model) openSelected() (tea.Model, tea.Cmd) {
	if model.itemCount() == 0 {
		return model, nil
	}
	switch model.section {
	case homeSection:
		item := model.workbench.Home[model.cursor]
		if item.Kind == "provider" {
			model.section, model.providerKey, model.providerCursor, model.screen = connectorsSection, item.ProviderKey, 0, providerScreen
			return model, nil
		}
		if item.Kind == "update" {
			model.section, model.systemCategory, model.screen = systemSection, updatesCategory, systemScreen
			for index, update := range model.filtered.Updates {
				if update.ID == item.ID {
					model.cursor = index
					break
				}
			}
			return model, nil
		}
	case connectorsSection:
		provider := model.workbench.Providers[model.cursor]
		model.providerKey, model.providerCursor, model.screen = provider.Connector.Key, 0, providerScreen
	case projectsSection:
		model.screen = projectScreen
	case systemSection:
		model.systemCategory, model.screen, model.cursor = systemCategory(model.cursor), systemScreen, 0
	}
	return model, nil
}

func (model Model) updateProvider(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	provider, ok := model.currentProvider()
	if !ok {
		model.screen = browseScreen
		return model, nil
	}
	rows := len(provider.Connections) + len(provider.Recipes)
	switch key.String() {
	case "esc", "left", "h":
		model.screen, model.providerKey, model.providerCursor = browseScreen, "", 0
		return model, nil
	case "up", "k":
		if model.providerCursor > 0 {
			model.providerCursor--
		}
	case "down", "j":
		if model.providerCursor+1 < rows {
			model.providerCursor++
		}
	case "enter":
		if recipe, selected := model.selectedRecipe(provider); selected {
			return model.beginRecipe(provider, recipe)
		}
	case "a":
		if len(provider.Recipes) > 0 {
			return model.beginRecipe(provider, provider.Recipes[0])
		}
	case "t":
		if connection, selected := model.selectedConnection(provider); selected {
			return model.testConnection(connection)
		}
	case "p":
		if connection, selected := model.selectedConnection(provider); selected {
			return model.replaceConnection(connection)
		}
	case "n":
		if connection, selected := model.selectedConnection(provider); selected {
			if credential, found := credentialFor(provider, connection); found {
				return model.beginCredentialReuse(credential)
			}
			model.err = "This account has no reusable outfit credential"
		}
	case "l":
		if connection, selected := model.selectedConnection(provider); selected {
			return model.toggleLinkEnrichment(provider, connection)
		}
	case "d":
		if connection, selected := model.selectedConnection(provider); selected {
			return model.removeConnection(connection)
		}
	case "u":
		for _, tool := range provider.Tools {
			if tool.UpdateAvailable || tool.Current == nil {
				return model.applyToolUpdate(tool)
			}
		}
	}
	return model, nil
}

func (model Model) toggleLinkEnrichment(provider application.ProviderCard, connection state.Connection) (tea.Model, tea.Cmd) {
	if len(provider.Connector.Links) == 0 {
		model.err = "This connector has no automatic link resolvers"
		return model, nil
	}
	current := provider.LinkModes[connection.Name]
	next := linkcontext.ModeHeavy
	if current == linkcontext.ModeHeavy || current == "" {
		next = linkcontext.ModeLight
	} else if current == linkcontext.ModeLight {
		next = linkcontext.ModeOff
	}
	model.screen, model.status, model.err = busyScreen, "Setting automatic link context to "+string(next)+"…", ""
	return model, tea.Batch(model.spinner.Tick, func() tea.Msg {
		err := model.service.SetLinkEnrichmentMode(model.ctx, connection.Name, next)
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		if err != nil {
			return actionResult{snapshot: snapshot, err: err, failure: providerScreen}
		}
		status := "Automatic link context set to " + string(next) + " for " + connection.Name
		return actionResult{snapshot: snapshot, status: status, err: snapshotErr}
	})
}

func (model Model) beginRecipe(provider application.ProviderCard, recipe application.SetupRecipe) (tea.Model, tea.Cmd) {
	if recipe.BlockedReason != "" && recipe.Import == nil {
		model.err = recipe.BlockedReason
		return model, nil
	}
	if recipe.Import != nil {
		model.beginAdoptionWizard(*recipe.Import)
		if model.screen == wizardScreen {
			return model, textinput.Blink
		}
		return model, nil
	}
	model.modeIndex, model.methodIndex = -1, -1
	for index, mode := range provider.Connector.Modes {
		if mode.ID == recipe.ModeID {
			model.modeIndex = index
			break
		}
	}
	for index, method := range model.modeMethods(provider.Connector) {
		if method.ID == recipe.MethodID {
			model.methodIndex = index
			break
		}
	}
	if model.modeIndex < 0 || model.methodIndex < 0 {
		model.err = "This setup recipe is no longer available; refresh connector packages"
		return model, nil
	}
	model.beginWizard(provider.Connector)
	return model, textinput.Blink
}

func (model Model) selectedConnection(provider application.ProviderCard) (state.Connection, bool) {
	if model.providerCursor >= 0 && model.providerCursor < len(provider.Connections) {
		return provider.Connections[model.providerCursor], true
	}
	return state.Connection{}, false
}

func (model Model) selectedRecipe(provider application.ProviderCard) (application.SetupRecipe, bool) {
	index := model.providerCursor - len(provider.Connections)
	if index >= 0 && index < len(provider.Recipes) {
		return provider.Recipes[index], true
	}
	return application.SetupRecipe{}, false
}

func (model Model) updateSystem(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	count := model.systemItemCount()
	if count == 0 && key.String() != "esc" && key.String() != "left" && key.String() != "h" {
		return model, nil
	}
	switch key.String() {
	case "esc", "left", "h":
		model.screen, model.cursor = browseScreen, int(model.systemCategory)
		return model, nil
	case "up", "k":
		if model.cursor > 0 {
			model.cursor--
		}
	case "down", "j":
		if model.cursor+1 < count {
			model.cursor++
		}
	case "enter", "u":
		switch model.systemCategory {
		case updatesCategory:
			return model.applyUpdate(model.filtered.Updates[model.cursor])
		case toolsCategory:
			return model.applyToolUpdate(model.filtered.Tools[model.cursor])
		case skillsCategory:
			return model.applySkillUpdate(model.filtered.Skills[model.cursor])
		case backupsCategory:
			action := "create"
			if model.cursor == 1 {
				action = "restore"
			}
			model.beginBackupWizard(action)
			return model, textinput.Blink
		}
	case "a":
		if model.systemCategory == toolsCategory && model.cursor < len(model.filtered.Tools) {
			return model.adoptTool(model.filtered.Tools[model.cursor])
		}
	case "x":
		if model.systemCategory == skillsCategory && model.cursor < len(model.filtered.Skills) {
			return model.toggleSkill(model.filtered.Skills[model.cursor])
		}
	case "c":
		if model.systemCategory == backupsCategory {
			model.beginBackupWizard("create")
			return model, textinput.Blink
		}
	case "shift+r", "R":
		if model.systemCategory == backupsCategory {
			model.beginBackupWizard("restore")
			return model, textinput.Blink
		}
	}
	return model, nil
}

func (model Model) systemItemCount() int {
	switch model.systemCategory {
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

func (model Model) testConnection(connection state.Connection) (tea.Model, tea.Cmd) {
	model.screen, model.status, model.err = busyScreen, "Verifying "+connection.Name+"…", ""
	return model, tea.Batch(model.spinner.Tick, func() tea.Msg {
		err := model.service.TestConnection(model.ctx, connection.Name)
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		if err != nil {
			return actionResult{snapshot: snapshot, err: err, failure: providerScreen}
		}
		return actionResult{snapshot: snapshot, status: connection.Name + " verified now", err: snapshotErr}
	})
}

func (model Model) removeConnection(connection state.Connection) (tea.Model, tea.Cmd) {
	if model.confirm != "remove:"+connection.Name {
		model.confirm = "remove:" + connection.Name
		model.err = "Press d again to disconnect " + connection.Name + " and destroy its outfit-managed credential"
		return model, nil
	}
	model.screen, model.status, model.err = busyScreen, "Disconnecting "+connection.Name+"…", ""
	return model, tea.Batch(model.spinner.Tick, func() tea.Msg {
		err := model.service.RemoveConnection(model.ctx, connection.Name)
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		if err != nil {
			return actionResult{snapshot: snapshot, err: err, failure: providerScreen}
		}
		return actionResult{snapshot: snapshot, status: connection.Name + " disconnected", err: snapshotErr}
	})
}

func (model Model) replaceConnection(connection state.Connection) (tea.Model, tea.Cmd) {
	provider, ok := model.currentProvider()
	if !ok {
		model.err = "Connector package is no longer available"
		return model, nil
	}
	model.modeIndex = 0
	for index, mode := range provider.Connector.Modes {
		if mode.ID == connection.SelectedMode {
			model.modeIndex = index
			break
		}
	}
	model.methodIndex = 0
	for index, method := range model.modeMethods(provider.Connector) {
		if method.ID == connection.CredentialMethod {
			model.methodIndex = index
			break
		}
	}
	model.beginWizard(provider.Connector)
	model.fields[0].input.SetValue(connection.Name)
	model.status = "Enter and verify the replacement; the current credential stays active until commit"
	return model, textinput.Blink
}

func (model Model) applyToolUpdate(card application.ToolCard) (tea.Model, tea.Cmd) {
	if card.RecommendedVersion == "" {
		if card.DetectedPath != "" {
			model.err = "This CLI is already on PATH; press a to adopt it into outfit"
		} else {
			model.err = "Install this provider CLI first, then return to adopt it"
		}
		return model, nil
	}
	current := "not installed"
	if card.Current != nil {
		current = card.Current.Version
	}
	return model.applyUpdate(application.Update{Kind: "CLI", ID: card.ID, Current: current, Available: card.RecommendedVersion})
}

func (model Model) applySkillUpdate(card application.SkillCard) (tea.Model, tea.Cmd) {
	receipt := card.Receipt
	return model.applyUpdate(application.Update{Kind: "Skill", ID: receipt.ID, Current: receipt.Ref, Available: receipt.Ref, Connection: receipt.Connection, SourceID: receipt.SourceID, Path: receipt.Path, Host: receipt.Host})
}

func (model Model) applyUpdate(update application.Update) (tea.Model, tea.Cmd) {
	model.screen, model.status, model.err = busyScreen, "Installing and verifying "+update.ID+"…", ""
	return model, tea.Batch(model.spinner.Tick, func() tea.Msg {
		status, err := model.service.ApplyUpdate(model.ctx, update)
		if err != nil {
			return actionResult{err: err, failure: systemScreen}
		}
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		return actionResult{snapshot: snapshot, status: status, err: snapshotErr}
	})
}

func (model Model) adoptTool(card application.ToolCard) (tea.Model, tea.Cmd) {
	if card.Current != nil || card.DetectedPath == "" {
		model.err = "No unmanaged PATH binary is available to adopt"
		return model, nil
	}
	model.screen, model.status, model.err = busyScreen, "Verifying and adopting the existing binary…", ""
	return model, tea.Batch(model.spinner.Tick, func() tea.Msg {
		receipt, err := model.service.AdoptTool(model.ctx, card.ID, card.DetectedPath)
		if err != nil {
			return actionResult{err: err, failure: systemScreen}
		}
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		return actionResult{snapshot: snapshot, status: receipt.Display + " adopted from " + receipt.Path, err: snapshotErr}
	})
}

func (model Model) toggleSkill(card application.SkillCard) (tea.Model, tea.Cmd) {
	if card.Kind == "generated" {
		model.err = "Generated connector skills follow connection enablement"
		return model, nil
	}
	enabled := !card.Enabled
	model.screen, model.status, model.err = busyScreen, "Updating skill selection…", ""
	return model, tea.Batch(model.spinner.Tick, func() tea.Msg {
		err := model.service.SetSkillEnabled(model.ctx, card.Receipt, enabled)
		if err != nil {
			return actionResult{err: err, failure: systemScreen}
		}
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		verb := "disabled"
		if enabled {
			verb = "enabled"
		}
		return actionResult{snapshot: snapshot, status: card.ID + " " + verb + "; restart agent sessions to load the change", err: snapshotErr}
	})
}

func (model *Model) beginAdoptionWizard(candidate hostmcp.Candidate) {
	if !candidate.Transferable {
		model.err = candidate.Reason
		return
	}
	name := textinput.New()
	name.Placeholder = candidate.ConnectorKey
	name.SetValue(candidate.ConnectorKey)
	name.CharLimit = 64
	model.fields = []formField{{key: "__name", label: "Connection name", required: true, input: name}}
	model.fieldIndex = 0
	model.fields[0].input.Focus()
	model.adoptionID, model.reuseCredential = candidate.ID, ""
	model.wizardConnector = application.ConnectorCard{Key: candidate.ConnectorKey, Display: candidate.Display}
	model.screen, model.status, model.err = wizardScreen, "Transfer verified OAuth from "+strings.Join(candidate.Hosts, "+")+" into outfit", ""
}

func (model *Model) beginWizard(card application.ConnectorCard) {
	if model.modeIndex < 0 || model.modeIndex >= len(card.Modes) {
		model.err = "Selected setup mode is unavailable"
		return
	}
	mode := card.Modes[model.modeIndex]
	methods := model.modeMethods(card)
	if model.methodIndex < 0 || model.methodIndex >= len(methods) {
		model.err = "Selected setup method is unavailable"
		return
	}
	method := methods[model.methodIndex]
	name := textinput.New()
	name.Placeholder = card.Key
	name.SetValue(card.Key)
	name.CharLimit = 64
	model.fields = []formField{{key: "__name", label: "Connection name", required: true, input: name}}
	for _, declared := range method.Fields {
		if declared.Generated {
			continue
		}
		input := textinput.New()
		input.Placeholder = declared.Placeholder
		input.SetValue(declared.Default)
		detectedFrom := ""
		if input.Value() == "" {
			for _, variable := range declared.Environment {
				if value, ok := os.LookupEnv(variable); ok && strings.TrimSpace(value) != "" {
					input.SetValue(value)
					detectedFrom = variable
					break
				}
			}
		}
		input.CharLimit = 1 << 20
		if declared.Secret {
			input.EchoMode, input.EchoCharacter = textinput.EchoPassword, '•'
		}
		model.fields = append(model.fields, formField{key: declared.Key, label: declared.Label, description: declared.Description, detectedFrom: detectedFrom, required: !declared.Optional, input: input})
	}
	model.fieldIndex, model.wizardConnector = 0, card
	model.reuseCredential, model.adoptionID = "", ""
	model.fields[0].input.Focus()
	model.screen, model.status, model.err = wizardScreen, "Selected "+mode.Label+" · "+method.Label, ""
	if card.DetectedCredentialMethod == method.ID {
		model.status += " · existing authenticated CLI will be verified and imported"
	} else if len(card.ExistingSetup) > 0 {
		model.status += " · existing credential values loaded and hidden"
	}
}

func (model Model) updateWizard(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" {
		return model, tea.Quit
	}
	if key.String() == "esc" {
		model.screen, model.fields, model.reuseCredential, model.adoptionID = providerScreen, nil, "", ""
		return model, nil
	}
	if len(model.fields) == 0 {
		return model, nil
	}
	current := &model.fields[model.fieldIndex]
	if key.String() != "enter" {
		var command tea.Cmd
		current.input, command = current.input.Update(key)
		return model, command
	}
	if current.required && strings.TrimSpace(current.input.Value()) == "" {
		model.err = current.label + " is required"
		return model, nil
	}
	current.input.Blur()
	if model.fieldIndex+1 < len(model.fields) {
		model.fieldIndex++
		model.fields[model.fieldIndex].input.Focus()
		model.err = ""
		return model, textinput.Blink
	}
	adoptionID, reuseCredential := model.adoptionID, model.reuseCredential
	var request application.SetupRequest
	if adoptionID != "" {
		request.Name = strings.TrimSpace(model.fields[0].input.Value())
	} else {
		request = model.setupRequest()
	}
	model.screen, model.status, model.err = busyScreen, "Installing, verifying, and synchronizing…", ""
	progress := make(chan actionProgress, 8)
	model.actionProgress = progress
	return model, tea.Batch(model.spinner.Tick, waitForActionProgress(progress), func() tea.Msg {
		defer close(progress)
		report := func(stage, detail string) {
			progress <- actionProgress{stage: stage, detail: detail}
		}
		var configured state.Connection
		var err error
		if adoptionID != "" {
			configured, _, err = model.service.AdoptHostMCP(model.ctx, adoptionID, request.Name, report)
		} else if reuseCredential != "" {
			configured, err = model.service.ReuseCredential(model.ctx, reuseCredential, request.Name, request.Mode, request.Fields)
		} else {
			configured, err = model.service.ApplySetup(model.ctx, request, report)
		}
		if err != nil {
			return actionResult{err: err, failure: wizardScreen}
		}
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		status := configured.Name + " is connected and its skills are synchronized"
		if adoptionID != "" {
			status = configured.Name + " transferred to outfit; restart host sessions that had the old MCP open"
		}
		return actionResult{snapshot: snapshot, status: status, err: snapshotErr}
	})
}

func waitForActionProgress(progress <-chan actionProgress) tea.Cmd {
	if progress == nil {
		return nil
	}
	return func() tea.Msg {
		update, ok := <-progress
		if !ok {
			return nil
		}
		return update
	}
}

func (model Model) setupRequest() application.SetupRequest {
	card := model.wizardConnector
	mode := card.Modes[model.modeIndex]
	method := model.modeMethods(card)[model.methodIndex]
	fields := map[string]string{}
	name := card.Key
	for _, field := range model.fields {
		if field.key == "__name" {
			name = field.input.Value()
		} else {
			fields[field.key] = field.input.Value()
		}
	}
	return application.SetupRequest{Name: name, Connector: card.Key, Mode: mode.ID, CredentialMethod: method.ID, Fields: fields}
}

func (model *Model) beginBackupWizard(action string) {
	home, _ := os.UserHomeDir()
	defaultPath := filepath.Join(home, "Downloads", "outfit-backup-"+time.Now().Format("2006-01-02")+".outfit")
	path := textinput.New()
	path.SetValue(defaultPath)
	path.CharLimit = 4096
	passphrase := textinput.New()
	passphrase.EchoMode, passphrase.EchoCharacter, passphrase.CharLimit = textinput.EchoPassword, '•', 1024
	model.fields = []formField{{key: "path", label: "Backup path", required: true, input: path}, {key: "passphrase", label: "Passphrase", required: true, input: passphrase}}
	if action == "create" {
		confirm := textinput.New()
		confirm.EchoMode, confirm.EchoCharacter, confirm.CharLimit = textinput.EchoPassword, '•', 1024
		model.fields = append(model.fields, formField{key: "confirm", label: "Confirm passphrase", required: true, input: confirm})
	} else {
		replace := textinput.New()
		replace.SetValue("no")
		replace.CharLimit = 3
		model.fields = append(model.fields, formField{key: "replace", label: "Replace name conflicts?", input: replace})
	}
	model.backupAction, model.fieldIndex, model.screen, model.err = action, 0, backupWizardScreen, ""
	model.fields[0].input.Focus()
}

func (model Model) updateBackupWizard(key tea.KeyMsg) (tea.Model, tea.Cmd) {
	if key.String() == "ctrl+c" {
		return model, tea.Quit
	}
	if key.String() == "esc" {
		model.screen = systemScreen
		return model, nil
	}
	current := &model.fields[model.fieldIndex]
	if key.String() != "enter" {
		var command tea.Cmd
		current.input, command = current.input.Update(key)
		return model, command
	}
	if current.required && strings.TrimSpace(current.input.Value()) == "" {
		model.err = current.label + " is required"
		return model, nil
	}
	current.input.Blur()
	if model.fieldIndex+1 < len(model.fields) {
		model.fieldIndex++
		model.fields[model.fieldIndex].input.Focus()
		model.err = ""
		return model, textinput.Blink
	}
	values := map[string]string{}
	for _, field := range model.fields {
		values[field.key] = field.input.Value()
	}
	if model.backupAction == "create" && values["passphrase"] != values["confirm"] {
		model.err, model.fieldIndex = "Backup passphrases do not match", 1
		model.fields[1].input.Focus()
		return model, textinput.Blink
	}
	action := model.backupAction
	model.screen, model.status, model.err = busyScreen, "Processing encrypted backup…", ""
	return model, tea.Batch(model.spinner.Tick, func() tea.Msg {
		var err error
		if action == "create" {
			err = model.service.CreateBackup(model.ctx, values["path"], values["passphrase"])
		} else {
			err = model.service.RestoreBackup(model.ctx, values["path"], values["passphrase"], strings.EqualFold(values["replace"], "yes"), nil)
		}
		if err != nil {
			return actionResult{err: err, failure: backupWizardScreen}
		}
		snapshot, snapshotErr := model.service.Snapshot(model.ctx)
		return actionResult{snapshot: snapshot, status: "Encrypted backup " + action + " completed: " + values["path"], err: snapshotErr}
	})
}

func (model Model) beginCredentialReuse(credential state.CredentialRecord) (tea.Model, tea.Cmd) {
	provider, ok := model.currentProvider()
	if !ok {
		model.err = "Connector package is no longer available"
		return model, nil
	}
	model.modeIndex = -1
	bestRank := int(^uint(0) >> 1)
	for index, mode := range provider.Connector.Modes {
		for _, method := range mode.CredentialMethods {
			if method == credential.MethodID && mode.Recommendation.Rank < bestRank {
				model.modeIndex, bestRank = index, mode.Recommendation.Rank
			}
		}
	}
	if model.modeIndex < 0 {
		model.err = "No execution mode can reuse this credential"
		return model, nil
	}
	methods := model.modeMethods(provider.Connector)
	model.methodIndex = -1
	for index, method := range methods {
		if method.ID == credential.MethodID {
			model.methodIndex = index
			break
		}
	}
	if model.methodIndex < 0 {
		model.err = "Credential method is no longer available"
		return model, nil
	}
	name := textinput.New()
	name.SetValue(credential.ConnectorKey + "-additional")
	name.CharLimit = 64
	model.fields = []formField{{key: "__name", label: "New connection name", required: true, input: name}}
	for _, declared := range methods[model.methodIndex].Fields {
		if declared.Secret || declared.Generated {
			continue
		}
		input := textinput.New()
		input.SetValue(declared.Default)
		input.Placeholder = declared.Placeholder
		input.CharLimit = 1 << 20
		model.fields = append(model.fields, formField{key: declared.Key, label: declared.Label, required: !declared.Optional, input: input})
	}
	model.fieldIndex = 0
	model.fields[0].input.Focus()
	model.wizardConnector = provider.Connector
	model.screen = wizardScreen
	model.reuseCredential = credential.ID
	model.adoptionID = ""
	model.status = "Reusing " + credential.Fingerprint + " for a new connection"
	model.err = ""
	return model, textinput.Blink
}

func credentialFor(provider application.ProviderCard, connection state.Connection) (state.CredentialRecord, bool) {
	for _, credential := range provider.Credentials {
		if credential.ID == connection.CredentialID {
			return credential, true
		}
	}
	return state.CredentialRecord{}, false
}
