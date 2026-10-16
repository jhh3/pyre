package connection

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/vault"
)

type Service struct {
	State                *state.Store
	Vault                vault.Store
	Executor             runtime.Executor
	Allowed              func(string) bool
	AllowedOperation     func(string, string) bool
	AllowedLinks         func(string) bool
	AllowedLink          func(string, string) bool
	AllowedConfiguration func(string, string, string) bool
	ResolveTool          func(kit.ToolRequirement) (string, error)
	OAuthAuthorize       OAuthAuthorizer
}

type ExecutionPlan struct {
	Connection       string       `json:"connection"`
	Connector        string       `json:"connector"`
	Mode             string       `json:"mode"`
	CredentialMethod string       `json:"credentialMethod"`
	Operation        string       `json:"operation"`
	PackageVersion   string       `json:"packageVersion"`
	PackageDigest    string       `json:"packageDigest"`
	PackageTrust     string       `json:"packageTrust"`
	Status           string       `json:"status"`
	Policy           string       `json:"policy"`
	Runtime          runtime.Plan `json:"runtime"`
}

func (service Service) Allows(name string) bool {
	return service.Allowed == nil || service.Allowed(name)
}

func (service Service) AllowsOperation(connection, operation string) bool {
	return service.AllowedOperation == nil || service.AllowedOperation(connection, operation)
}

func (service Service) AllowsLinks(connection string) bool {
	return service.AllowedLinks == nil || service.AllowedLinks(connection)
}

func (service Service) AllowsLink(connection, rule string) bool {
	return service.AllowedLink == nil || service.AllowedLink(connection, rule)
}

func (service Service) Configure(ctx context.Context, name, connectorKey, modeID string, fields map[string]string) (state.Connection, error) {
	if err := validateConnectionName(name); err != nil {
		return state.Connection{}, err
	}
	if service.State == nil {
		return state.Connection{}, errors.New("connection service is not initialized")
	}
	installed, err := service.State.Package(ctx, connectorKey)
	if err != nil {
		return state.Connection{}, fmt.Errorf("connector package %s is not installed: %w", connectorKey, err)
	}
	mode, ok := runtime.FindMode(installed.Package, modeID)
	if !ok {
		return state.Connection{}, fmt.Errorf("connector %s has no mode %s", connectorKey, modeID)
	}
	return service.ConfigureWithMethod(ctx, name, connectorKey, modeID, mode.DefaultCredentialMethod, fields)
}

func (service Service) ConfigureWithMethod(ctx context.Context, name, connectorKey, modeID, methodID string, fields map[string]string) (state.Connection, error) {
	fields = cloneStrings(fields)
	if service.AllowedConfiguration != nil && !service.AllowedConfiguration(name, connectorKey, modeID) {
		return state.Connection{}, fmt.Errorf("connection %s with %s/%s is not declared by this workspace; edit and apply the workspace first", name, connectorKey, modeID)
	}
	if err := validateConnectionName(name); err != nil {
		return state.Connection{}, err
	}
	installed, err := service.State.Package(ctx, connectorKey)
	if err != nil {
		return state.Connection{}, fmt.Errorf("connector package %s is not installed: %w", connectorKey, err)
	}
	if installed.Trust == "untrusted" {
		return state.Connection{}, fmt.Errorf("connector package %s is untrusted; review `outfit kit diff` and reinstall with --trust", connectorKey)
	}
	mode, ok := runtime.FindMode(installed.Package, modeID)
	if !ok {
		return state.Connection{}, fmt.Errorf("connector %s has no mode %s", connectorKey, modeID)
	}
	method, ok := kit.FindCredentialMethod(installed.Package, methodID)
	if !ok || !containsString(mode.CredentialMethods, methodID) {
		return state.Connection{}, fmt.Errorf("connector %s mode %s does not support credential method %s", connectorKey, modeID, methodID)
	}
	if err := validateFields(method, fields); err != nil {
		return state.Connection{}, err
	}
	capabilities, err := validateCapabilities(method, mode, fields)
	if err != nil {
		return state.Connection{}, err
	}
	oauthIdentity := map[string]string{}
	if method.OAuth != nil {
		if fields[oauthRefreshTokenField] != "" {
			fields, err = RefreshOAuthFields(ctx, method, fields)
			if err != nil {
				return state.Connection{}, err
			}
			oauthIdentity, err = OAuthIdentity(ctx, method, fields)
			if err != nil {
				return state.Connection{}, err
			}
			if oauthIdentity == nil {
				oauthIdentity = map[string]string{}
			}
			oauthIdentity["scopes"] = fields[oauthGrantedScopesField]
		} else {
			authorize := service.OAuthAuthorize
			if authorize == nil {
				authorize = AuthorizeBrowser
			}
			grant, err := authorize(ctx, method, fields, capabilities)
			if err != nil {
				return state.Connection{}, fmt.Errorf("authorize %s: %w", method.ID, err)
			}
			for key, value := range grant.Fields {
				fields[key] = value
			}
			for key, value := range grant.Identity {
				oauthIdentity[key] = value
			}
		}
		if err := validateGeneratedFields(method, fields); err != nil {
			return state.Connection{}, err
		}
	}
	credentialContext, err := service.credentialContext(installed.Package, mode, method)
	if err != nil {
		return state.Connection{}, err
	}
	var verifiedExecutor *runtime.Result
	if method.Custody == "executor" && method.Setup != nil {
		if method.Verify != nil {
			if result, verifyErr := service.Executor.RunCommandTemplate(ctx, *method.Verify, fields, credentialContext); verifyErr == nil {
				verifiedExecutor = &result
			}
		}
		if verifiedExecutor == nil {
			if _, err := service.Executor.RunCommandTemplate(ctx, *method.Setup, fields, credentialContext); err != nil {
				return state.Connection{}, fmt.Errorf("native login: %w", err)
			}
		}
	}
	vaultPayload := secretFields(method, fields)
	candidateRef := ""
	if len(vaultPayload) > 0 {
		candidateRef, err = service.Vault.Put(ctx, vaultPayload)
		if err != nil {
			return state.Connection{}, err
		}
	}
	committed := false
	defer func() {
		if !committed && candidateRef != "" {
			_ = service.Vault.Delete(context.Background(), candidateRef)
		}
	}()
	identity := map[string]string{}
	identity["fingerprint"] = credentialFingerprint(method, fields)
	for key, value := range safeSettings(method, fields) {
		identity["setting."+key] = value
	}
	if environment := credentialEnvironment(fields); environment != "" {
		identity["environment"] = environment
	}
	if len(capabilities) > 0 {
		identity["capabilities"] = strings.Join(capabilities, ",")
	}
	for key, value := range oauthIdentity {
		if value != "" {
			identity[key] = value
		}
	}
	var discovered []state.Tool
	if mode.Executor.Kind == "mcp" {
		tools, err := runtime.DiscoverMCPWithCredentials(ctx, mode, fields, credentialContext)
		if err != nil {
			return state.Connection{}, fmt.Errorf("discover MCP tools: %w", err)
		}
		if len(tools) == 0 {
			return state.Connection{}, errors.New("MCP server exposed no tools")
		}
		for _, tool := range tools {
			schema, _ := tool.InputSchema.(map[string]any)
			discovered = append(discovered, state.Tool{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, SourceName: tool.Name, InputSchema: schema})
		}
		identity["tools"] = fmt.Sprint(len(tools))
	}
	if method.Verify != nil {
		result := runtime.Result{}
		if verifiedExecutor != nil {
			result = *verifiedExecutor
		} else {
			var verifyErr error
			result, verifyErr = service.Executor.RunCommandTemplate(ctx, *method.Verify, fields, credentialContext)
			if verifyErr != nil {
				return state.Connection{}, fmt.Errorf("verify %s: %w", method.ID, verifyErr)
			}
		}
		identity["verified"] = "true"
		identity["status"] = fmt.Sprint(result.Status)
		mergeSafeIdentity(identity, result.Data)
	} else if mode.Verify != "" {
		operation, _ := runtime.FindOperation(mode, mode.Verify)
		result, verifyErr := service.Executor.RunWithCredentials(ctx, mode, operation, map[string]string{}, fields, credentialContext)
		if verifyErr != nil {
			return state.Connection{}, fmt.Errorf("verify %s: %w", mode.Verify, verifyErr)
		}
		identity["verified"] = "true"
		identity["status"] = fmt.Sprint(result.Status)
		mergeSafeIdentity(identity, result.Data)
	}
	existing, existingErr := service.State.Connection(ctx, name)
	if existingErr == nil && existing.Status == "removed" {
		// A removed connection keeps its row (and name) as history; adding
		// the name again replaces it with a fresh connection.
		if err := service.State.PurgeRemovedConnection(ctx, name); err != nil {
			return state.Connection{}, err
		}
		existing, existingErr = service.State.Connection(ctx, name)
	}
	if state.IsNotFound(existingErr) {
		connection := state.Connection{ID: newID("con"), Name: name, ConnectorKey: connectorKey, SelectedMode: modeID, CredentialMethod: methodID, Status: "active", CredentialRef: candidateRef, SafeIdentity: identity}
		for index := range discovered {
			discovered[index].ConnectionID = connection.ID
		}
		if err := service.State.CreateConnectionWithTools(ctx, connection, discovered); err != nil {
			return state.Connection{}, err
		}
		committed = true
		return service.State.Connection(ctx, name)
	}
	if existingErr != nil {
		return state.Connection{}, existingErr
	}
	if existing.ConnectorKey != connectorKey {
		return state.Connection{}, errors.New("a connection name cannot change connector type")
	}
	cleanupID := newID("clean")
	for index := range discovered {
		discovered[index].ConnectionID = existing.ID
	}
	switched, cleanupOld, err := service.State.SwitchConnection(ctx, name, existing.CredentialRef, modeID, methodID, candidateRef, "active", cleanupID, identity, discovered)
	if err != nil {
		return state.Connection{}, err
	}
	if !switched {
		return state.Connection{}, errors.New("connection changed concurrently; retry")
	}
	committed = true
	if cleanupOld && existing.CredentialRef != "" {
		if err := service.Vault.Delete(ctx, existing.CredentialRef); err == nil {
			_ = service.State.FinishCleanup(ctx, cleanupID)
		}
	}
	return service.State.Connection(ctx, name)
}

// DetectExecutorCredential verifies an existing native CLI login without
// starting an interactive login or storing anything. It is used by setup
// surfaces to prefer an already-authenticated provider CLI over asking for a
// second credential.
func (service Service) DetectExecutorCredential(ctx context.Context, pkg kit.Package, mode kit.Mode) (string, map[string]string, bool) {
	for _, methodID := range mode.CredentialMethods {
		method, ok := kit.FindCredentialMethod(pkg, methodID)
		if !ok || method.Custody != "executor" {
			continue
		}
		fields := map[string]string{}
		for _, field := range method.Fields {
			if !field.Generated && field.Default != "" {
				fields[field.Key] = field.Default
			}
		}
		credentialContext, err := service.credentialContext(pkg, mode, method)
		if err != nil {
			continue
		}
		var result runtime.Result
		if method.Verify != nil {
			result, err = service.Executor.RunCommandTemplate(ctx, *method.Verify, fields, credentialContext)
		} else if mode.Verify != "" {
			operation, found := runtime.FindOperation(mode, mode.Verify)
			if !found {
				continue
			}
			result, err = service.Executor.RunWithCredentials(ctx, mode, operation, map[string]string{}, fields, credentialContext)
		} else {
			continue
		}
		if err != nil {
			continue
		}
		identity := map[string]string{"verified": "true", "status": fmt.Sprint(result.Status)}
		mergeSafeIdentity(identity, result.Data)
		return method.ID, identity, true
	}
	return "", nil, false
}

// ReuseCredential verifies an existing active credential against a new named
// connection and attaches the same credential record without duplicating its
// vault material. Non-secret method settings (for example selected GWS
// services) are supplied per connection.
func (service Service) ReuseCredential(ctx context.Context, credentialID, name, modeID string, settings map[string]string) (state.Connection, error) {
	if err := validateConnectionName(name); err != nil {
		return state.Connection{}, err
	}
	credential, err := service.State.Credential(ctx, credentialID)
	if err != nil {
		return state.Connection{}, err
	}
	if credential.Status != "active" {
		return state.Connection{}, fmt.Errorf("credential is %s", credential.Status)
	}
	if service.AllowedConfiguration != nil && !service.AllowedConfiguration(name, credential.ConnectorKey, modeID) {
		return state.Connection{}, fmt.Errorf("connection %s with %s/%s is not declared by this workspace; edit and apply the workspace first", name, credential.ConnectorKey, modeID)
	}
	if _, existingErr := service.State.Connection(ctx, name); existingErr == nil {
		return state.Connection{}, fmt.Errorf("connection %s already exists", name)
	} else if !state.IsNotFound(existingErr) {
		return state.Connection{}, existingErr
	}
	installed, err := service.State.Package(ctx, credential.ConnectorKey)
	if err != nil {
		return state.Connection{}, err
	}
	mode, ok := kit.FindMode(installed.Package, modeID)
	if !ok {
		return state.Connection{}, fmt.Errorf("connector %s has no mode %s", credential.ConnectorKey, modeID)
	}
	method, ok := kit.FindCredentialMethod(installed.Package, credential.MethodID)
	if !ok || !containsString(mode.CredentialMethods, method.ID) {
		return state.Connection{}, fmt.Errorf("mode %s cannot use credential method %s", modeID, credential.MethodID)
	}
	fields := map[string]string{}
	if credential.VaultRef != "" {
		stored, err := service.Vault.Get(ctx, credential.VaultRef)
		if err != nil {
			return state.Connection{}, fmt.Errorf("read credential: %w", err)
		}
		for key, value := range stored {
			fields[key] = value
		}
	}
	for _, declared := range method.Fields {
		if declared.Default != "" {
			fields[declared.Key] = declared.Default
		}
	}
	for key, value := range settings {
		fields[key] = value
	}
	if method.OAuth != nil {
		fields, err = RefreshOAuthFields(ctx, method, fields)
		if err != nil {
			return state.Connection{}, err
		}
	}
	if err := validateFields(method, fields); err != nil {
		return state.Connection{}, err
	}
	capabilities, err := validateCapabilities(method, mode, fields)
	if err != nil {
		return state.Connection{}, err
	}
	credentialContext, err := service.credentialContext(installed.Package, mode, method)
	if err != nil {
		return state.Connection{}, err
	}
	identity := map[string]string{
		"fingerprint":  credential.Fingerprint,
		"environment":  credential.Environment,
		"account_id":   credential.AccountID,
		"account_name": credential.AccountName,
		"principal":    credential.PrincipalName,
	}
	for key, value := range safeSettings(method, fields) {
		identity["setting."+key] = value
	}
	if len(capabilities) > 0 {
		identity["capabilities"] = strings.Join(capabilities, ",")
	}
	var discovered []state.Tool
	if mode.Executor.Kind == "mcp" {
		tools, err := runtime.DiscoverMCPWithCredentials(ctx, mode, fields, credentialContext)
		if err != nil {
			return state.Connection{}, fmt.Errorf("discover MCP tools: %w", err)
		}
		if len(tools) == 0 {
			return state.Connection{}, errors.New("MCP server exposed no tools")
		}
		for _, tool := range tools {
			schema, _ := tool.InputSchema.(map[string]any)
			discovered = append(discovered, state.Tool{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, SourceName: tool.Name, InputSchema: schema})
		}
		identity["tools"] = fmt.Sprint(len(tools))
	}
	if method.Verify != nil {
		result, verifyErr := service.Executor.RunCommandTemplate(ctx, *method.Verify, fields, credentialContext)
		if verifyErr != nil {
			return state.Connection{}, fmt.Errorf("verify %s: %w", method.ID, verifyErr)
		}
		identity["verified"], identity["status"] = "true", fmt.Sprint(result.Status)
		mergeSafeIdentity(identity, result.Data)
	} else if mode.Verify != "" {
		operation, _ := runtime.FindOperation(mode, mode.Verify)
		result, verifyErr := service.Executor.RunWithCredentials(ctx, mode, operation, map[string]string{}, fields, credentialContext)
		if verifyErr != nil {
			return state.Connection{}, fmt.Errorf("verify %s: %w", mode.Verify, verifyErr)
		}
		identity["verified"], identity["status"] = "true", fmt.Sprint(result.Status)
		mergeSafeIdentity(identity, result.Data)
	}
	connection := state.Connection{ID: newID("con"), Name: name, ConnectorKey: credential.ConnectorKey, SelectedMode: modeID, CredentialMethod: credential.MethodID, Status: "active", CredentialID: credential.ID, CredentialRef: credential.VaultRef, SafeIdentity: identity}
	for index := range discovered {
		discovered[index].ConnectionID = connection.ID
	}
	if err := service.State.AttachConnectionWithCredential(ctx, connection, credential.ID, discovered); err != nil {
		return state.Connection{}, err
	}
	return service.State.Connection(ctx, name)
}

func validateConnectionName(name string) error {
	if matched, _ := regexp.MatchString(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`, name); !matched || name == "." || name == ".." {
		return errors.New("connection name must use 1-64 letters, numbers, dots, underscores, or hyphens")
	}
	return nil
}

func (service Service) Remove(ctx context.Context, name string) error {
	if !service.Allows(name) {
		return fmt.Errorf("connection %s is not enabled by this project", name)
	}
	connection, err := service.State.Connection(ctx, name)
	if err != nil {
		return err
	}
	if connection.Status == "removed" {
		// Already removed: drop the leftover row so the name is free.
		return service.State.PurgeRemovedConnection(ctx, name)
	}
	cleanupID := newID("clean")
	removed, cleanupCredential, err := service.State.RemoveConnection(ctx, name, connection.CredentialRef, cleanupID)
	if err != nil {
		return err
	}
	if !removed {
		return errors.New("connection changed concurrently; retry")
	}
	if cleanupCredential && connection.CredentialRef != "" {
		if err := service.Vault.Delete(ctx, connection.CredentialRef); err == nil {
			return service.State.FinishCleanup(ctx, cleanupID)
		}
	}
	return nil
}

func (service Service) Run(ctx context.Context, connectionName, operationName string, arguments map[string]string, allowWrite bool) (runtime.Result, error) {
	return service.RunWithIO(ctx, connectionName, operationName, arguments, allowWrite, nil, nil)
}

func (service Service) RunWithIO(ctx context.Context, connectionName, operationName string, arguments map[string]string, allowWrite bool, stdout, stderr io.Writer) (runtime.Result, error) {
	connection, installed, mode, operation, err := service.resolve(ctx, connectionName, operationName, allowWrite)
	if err != nil {
		return runtime.Result{}, err
	}
	secret, err := service.secret(ctx, connection)
	if err != nil {
		return runtime.Result{}, err
	}
	method, _, err := resolveCredentials(installed.Package, mode, connection.CredentialMethod)
	if err != nil {
		return runtime.Result{}, err
	}
	credentialContext, err := service.credentialContext(installed.Package, mode, method)
	if err != nil {
		return runtime.Result{}, err
	}
	result, runErr := service.Executor.RunWithCredentialsIO(ctx, mode, operation, arguments, secret, credentialContext, stdout, stderr)
	outcome := "ok"
	if runErr != nil {
		outcome = "error"
	}
	_ = service.State.Audit(ctx, connection.ID, operation.Name, operation.Class, outcome, map[string]any{"mode": mode.ID, "status": result.Status})
	if runErr == nil {
		_ = service.State.TouchCredential(ctx, connection.CredentialID, false)
	}
	if runErr == nil && operation.Class == "read" {
		service.cacheResult(ctx, connection, operation, arguments, result)
	}
	return result, runErr
}

func (service Service) ExecCLI(ctx context.Context, connectionName string, argv []string, allowWrite bool) (runtime.Result, error) {
	if len(argv) == 0 {
		return runtime.Result{}, errors.New("provider CLI arguments are required")
	}
	if !service.Allows(connectionName) {
		return runtime.Result{}, fmt.Errorf("connection %s is not enabled by this project", connectionName)
	}
	connection, err := service.State.Connection(ctx, connectionName)
	if err != nil {
		return runtime.Result{}, err
	}
	if connection.Status != "active" {
		return runtime.Result{}, fmt.Errorf("connection is %s", connection.Status)
	}
	installed, err := service.State.Package(ctx, connection.ConnectorKey)
	if err != nil {
		return runtime.Result{}, err
	}
	mode, ok := kit.FindMode(installed.Package, connection.SelectedMode)
	if !ok || mode.Executor.Kind != "cli" {
		return runtime.Result{}, fmt.Errorf("connection %s does not use a CLI mode", connectionName)
	}
	class := "admin"
	if cliReadOnly(argv) {
		class = "read"
	}
	if class != "read" && !allowWrite {
		return runtime.Result{}, errors.New("raw provider command may mutate state; repeat with `outfit exec --write` only when the human requested the change")
	}
	secret, err := service.secret(ctx, connection)
	if err != nil {
		return runtime.Result{}, err
	}
	method, _, err := resolveCredentials(installed.Package, mode, connection.CredentialMethod)
	if err != nil {
		return runtime.Result{}, err
	}
	credentials, err := service.credentialContext(installed.Package, mode, method)
	if err != nil {
		return runtime.Result{}, err
	}
	operation := kit.Operation{Name: "raw_cli", Description: "Provider CLI command", Class: class, CLI: &kit.CLIOperation{Args: append([]string{}, argv...)}}
	result, runErr := service.Executor.RunWithCredentials(ctx, mode, operation, map[string]string{}, secret, credentials)
	outcome := "ok"
	if runErr != nil {
		outcome = "error"
	}
	_ = service.State.Audit(ctx, connection.ID, "raw_cli", class, outcome, map[string]any{"mode": mode.ID, "argv0": argv[0], "status": result.Status})
	if runErr == nil {
		_ = service.State.TouchCredential(ctx, connection.CredentialID, false)
	}
	return result, runErr
}

func cliReadOnly(argv []string) bool {
	readVerbs := map[string]bool{"cat": true, "describe": true, "download": true, "export": true, "get": true, "help": true, "inspect": true, "list": true, "logs": true, "read": true, "retrieve": true, "search": true, "show": true, "status": true, "tail": true, "version": true, "whoami": true}
	for index := len(argv) - 1; index >= 0; index-- {
		value := strings.ToLower(strings.TrimLeft(argv[index], "-"))
		if value == "help" || value == "version" {
			return true
		}
		if strings.HasPrefix(argv[index], "-") || strings.HasPrefix(argv[index], "{") {
			continue
		}
		return readVerbs[value]
	}
	return false
}

func (service Service) cacheResult(ctx context.Context, connection state.Connection, operation kit.Operation, arguments map[string]string, result runtime.Result) {
	var config state.CacheConfig
	if service.State.ReadSetting(ctx, "cache.config", &config) != nil || !config.Enabled {
		return
	}
	ttl := time.Duration(config.TTLSeconds) * time.Second
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	encodedArguments, _ := json.Marshal(arguments)
	digest := sha256.Sum256(append([]byte(connection.Name+"\x00"+operation.Name+"\x00"), encodedArguments...))
	handle := "res_" + hex.EncodeToString(digest[:12])
	body := string(result.Raw)
	if body == "" {
		encoded, _ := json.Marshal(result.Data)
		body = string(encoded)
	}
	if len(body) > 1<<20 {
		body = body[:1<<20]
	}
	now := time.Now().UTC()
	values := make(map[string]any, len(arguments))
	for key, value := range arguments {
		values[key] = value
	}
	_ = service.State.PutContext(ctx, state.ContextItem{Handle: handle, URL: "outfit://" + connection.Name + "/" + operation.Name + "/" + handle, ConnectionName: connection.Name, ConnectorKey: connection.ConnectorKey, Operation: operation.Name, ResourceType: "operation-result", Title: connection.Name + " " + operation.Title, Body: body, Arguments: values, Provenance: map[string]any{"kind": "operation", "mode": connection.SelectedMode, "status": result.Status}, FetchedAt: now, ExpiresAt: now.Add(ttl)})
}

func (service Service) Plan(ctx context.Context, connectionName, operationName string, arguments map[string]string, allowWrite bool) (ExecutionPlan, error) {
	connection, installed, mode, operation, err := service.resolve(ctx, connectionName, operationName, allowWrite)
	if err != nil {
		return ExecutionPlan{}, err
	}
	method, _, err := resolveCredentials(installed.Package, mode, connection.CredentialMethod)
	if err != nil {
		return ExecutionPlan{}, err
	}
	credentialContext, err := service.credentialContext(installed.Package, mode, method)
	if err != nil {
		return ExecutionPlan{}, err
	}
	runtimePlan, err := service.Executor.PlanWithCredentials(mode, operation, arguments, credentialContext)
	if err != nil {
		return ExecutionPlan{}, err
	}
	return ExecutionPlan{Connection: connection.Name, Connector: connection.ConnectorKey, Mode: mode.ID, CredentialMethod: connection.CredentialMethod, Operation: operation.Name, PackageVersion: installed.Version, PackageDigest: installed.Digest, PackageTrust: installed.Trust, Status: connection.Status, Policy: "allowed", Runtime: runtimePlan}, nil
}

func (service Service) Explain(ctx context.Context, connectionName, operationName string, arguments map[string]string, allowWrite bool) (ExecutionPlan, error) {
	connection, installed, mode, operation, err := service.lookup(ctx, connectionName, operationName)
	if err != nil {
		return ExecutionPlan{}, err
	}
	method, _, err := resolveCredentials(installed.Package, mode, connection.CredentialMethod)
	if err != nil {
		return ExecutionPlan{}, err
	}
	credentialContext, err := service.credentialContext(installed.Package, mode, method)
	if err != nil {
		return ExecutionPlan{}, err
	}
	runtimePlan, err := service.Executor.PlanWithCredentials(mode, operation, arguments, credentialContext)
	if err != nil {
		return ExecutionPlan{}, err
	}
	policy := "allowed"
	switch {
	case connection.Status != "active":
		policy = "unavailable: connection is " + connection.Status
	case !service.Allows(connectionName):
		policy = "denied: connection is not enabled by this project"
	case !service.AllowsOperation(connectionName, operationName):
		policy = "denied: operation is not enabled by this project"
	case !operationCapabilitiesAllowed(connection, mode, operation):
		policy = "denied: operation requires an unselected connector capability"
	case operation.Class != "read" && !allowWrite:
		policy = "requires explicit --write acknowledgement"
	}
	return ExecutionPlan{Connection: connection.Name, Connector: connection.ConnectorKey, Mode: mode.ID, CredentialMethod: connection.CredentialMethod, Operation: operation.Name, PackageVersion: installed.Version, PackageDigest: installed.Digest, PackageTrust: installed.Trust, Status: connection.Status, Policy: policy, Runtime: runtimePlan}, nil
}

func (service Service) resolve(ctx context.Context, connectionName, operationName string, allowWrite bool) (state.Connection, state.InstalledPackage, kit.Mode, kit.Operation, error) {
	connection, installed, mode, operation, err := service.lookup(ctx, connectionName, operationName)
	if err != nil {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, err
	}
	if !service.Allows(connectionName) {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, fmt.Errorf("connection %s is not enabled by this project", connectionName)
	}
	if !service.AllowsOperation(connectionName, operationName) {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, fmt.Errorf("operation %s is not enabled for connection %s by this project", operationName, connectionName)
	}
	if !operationCapabilitiesAllowed(connection, mode, operation) {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, fmt.Errorf("operation %s requires connector capabilities that are not selected for %s", operationName, connectionName)
	}
	if connection.Status != "active" {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, fmt.Errorf("connection is %s", connection.Status)
	}
	if operation.Class != "read" && !allowWrite {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, fmt.Errorf("operation is %s; use outfit write to make the mutation explicit", operation.Class)
	}
	return connection, installed, mode, operation, nil
}

func operationCapabilitiesAllowed(connection state.Connection, mode kit.Mode, operation kit.Operation) bool {
	if len(operation.Capabilities) == 0 {
		return true
	}
	selected := map[string]bool{}
	if value := strings.TrimSpace(connection.SafeIdentity["capabilities"]); value != "" {
		for _, capability := range strings.Split(value, ",") {
			selected[strings.TrimSpace(capability)] = true
		}
	} else {
		for _, capability := range mode.Capabilities {
			selected[capability] = true
		}
	}
	for _, capability := range operation.Capabilities {
		if !selected[capability] {
			return false
		}
	}
	return true
}

// OperationCapabilitiesAllowed reports whether an operation is part of the
// capability set selected for this named connection. Listing and agent-facing
// surfaces use the same predicate as execution so unavailable services are not
// advertised merely because the managed binary supports them.
func OperationCapabilitiesAllowed(connection state.Connection, mode kit.Mode, operation kit.Operation) bool {
	return operationCapabilitiesAllowed(connection, mode, operation)
}

func (service Service) lookup(ctx context.Context, connectionName, operationName string) (state.Connection, state.InstalledPackage, kit.Mode, kit.Operation, error) {
	connection, err := service.State.Connection(ctx, connectionName)
	if err != nil {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, err
	}
	installed, err := service.State.Package(ctx, connection.ConnectorKey)
	if err != nil {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, err
	}
	mode, ok := runtime.FindMode(installed.Package, connection.SelectedMode)
	if !ok {
		return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, errors.New("selected mode no longer exists")
	}
	operation, ok := runtime.FindOperation(mode, operationName)
	if !ok {
		tool, err := service.State.Tool(ctx, connection.ID, operationName)
		if err != nil {
			return state.Connection{}, state.InstalledPackage{}, kit.Mode{}, kit.Operation{}, fmt.Errorf("selected mode %s has no operation %s", mode.ID, operationName)
		}
		operation = kit.Operation{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, Input: tool.InputSchema, MCPTool: tool.SourceName}
	}
	operation = kit.NormalizeOperation(operation)
	return connection, installed, mode, operation, nil
}

func (service Service) Verify(ctx context.Context, name string) error {
	if !service.Allows(name) {
		return fmt.Errorf("connection %s is not enabled by this project", name)
	}
	connection, err := service.State.Connection(ctx, name)
	if err != nil {
		return err
	}
	installed, err := service.State.Package(ctx, connection.ConnectorKey)
	if err != nil {
		return err
	}
	mode, ok := runtime.FindMode(installed.Package, connection.SelectedMode)
	if !ok {
		return errors.New("selected mode no longer exists")
	}
	secret, err := service.secret(ctx, connection)
	if err != nil {
		return err
	}
	method, _, err := resolveCredentials(installed.Package, mode, connection.CredentialMethod)
	if err != nil {
		return err
	}
	credentialContext, err := service.credentialContext(installed.Package, mode, method)
	if err != nil {
		return err
	}
	if method.Verify != nil {
		if _, err := service.Executor.RunCommandTemplate(ctx, *method.Verify, secret, credentialContext); err != nil {
			return err
		}
		return service.State.TouchCredential(ctx, connection.CredentialID, true)
	}
	if mode.Executor.Kind == "mcp" {
		tools, err := runtime.DiscoverMCPWithCredentials(ctx, mode, secret, credentialContext)
		if err != nil {
			return err
		}
		if len(tools) == 0 {
			return errors.New("MCP server exposed no tools")
		}
		discovered := make([]state.Tool, 0, len(tools))
		for _, tool := range tools {
			schema, _ := tool.InputSchema.(map[string]any)
			discovered = append(discovered, state.Tool{ConnectionID: connection.ID, Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, SourceName: tool.Name, InputSchema: schema})
		}
		if err := service.State.ReplaceTools(ctx, connection.ID, discovered); err != nil {
			return err
		}
		return service.State.TouchCredential(ctx, connection.CredentialID, true)
	}
	if mode.Verify == "" {
		return errors.New("connector mode has no verification operation")
	}
	operation, ok := runtime.FindOperation(mode, mode.Verify)
	if !ok {
		return errors.New("verification operation is missing")
	}
	_, err = service.Executor.RunWithCredentials(ctx, mode, operation, map[string]string{}, secret, credentialContext)
	if err == nil {
		_ = service.State.TouchCredential(ctx, connection.CredentialID, true)
	}
	return err
}

func (service Service) SweepSecrets(ctx context.Context) error {
	cleanups, err := service.State.Cleanups(ctx)
	if err != nil {
		return err
	}
	var failures []string
	for _, cleanup := range cleanups {
		if err := service.Vault.Delete(ctx, cleanup.CredentialRef); err != nil {
			_ = service.State.FailCleanup(ctx, cleanup.ID, err)
			failures = append(failures, err.Error())
			continue
		}
		_ = service.State.FinishCleanup(ctx, cleanup.ID)
	}
	if len(failures) > 0 {
		return fmt.Errorf("secret cleanup: %s", strings.Join(failures, "; "))
	}
	return nil
}

func validateFields(method kit.CredentialMethod, fields map[string]string) error {
	declared := map[string]kit.CredentialField{}
	for _, field := range method.Fields {
		declared[field.Key] = field
		value := fields[field.Key]
		if !field.Optional && !field.Generated && strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", field.Key)
		}
		if value != "" && field.Pattern != "" {
			matched, err := regexp.MatchString("^(?:"+field.Pattern+")$", value)
			if err != nil || !matched {
				return fmt.Errorf("%s has an invalid value", field.Key)
			}
		}
	}
	for key := range fields {
		if _, ok := declared[key]; !ok {
			return fmt.Errorf("undeclared credential field %s", key)
		}
	}
	return nil
}

func validateGeneratedFields(method kit.CredentialMethod, fields map[string]string) error {
	for _, field := range method.Fields {
		if field.Generated && !field.Optional && strings.TrimSpace(fields[field.Key]) == "" {
			return fmt.Errorf("OAuth authorization did not produce %s", field.Key)
		}
	}
	return nil
}

func validateCapabilities(method kit.CredentialMethod, mode kit.Mode, fields map[string]string) ([]string, error) {
	if method.CapabilityField == "" {
		return nil, nil
	}
	allowed := map[string]bool{}
	for _, capability := range mode.Capabilities {
		allowed[capability] = true
	}
	seen := map[string]bool{}
	var result []string
	for _, capability := range strings.Split(fields[method.CapabilityField], ",") {
		capability = strings.TrimSpace(capability)
		if capability == "" {
			continue
		}
		if !allowed[capability] {
			return nil, fmt.Errorf("capability %s is not available in mode %s", capability, mode.ID)
		}
		if !seen[capability] {
			seen[capability] = true
			result = append(result, capability)
		}
	}
	sort.Strings(result)
	return result, nil
}

func resolveCredentials(pkg kit.Package, mode kit.Mode, methodID string) (kit.CredentialMethod, []kit.CredentialBinding, error) {
	method, ok := kit.FindCredentialMethod(pkg, methodID)
	if !ok || !containsString(mode.CredentialMethods, methodID) {
		return kit.CredentialMethod{}, nil, fmt.Errorf("selected credential method %s no longer exists", methodID)
	}
	return method, kit.CredentialBindingsFor(mode, method), nil
}

func (service Service) credentialContext(pkg kit.Package, mode kit.Mode, method kit.CredentialMethod) (runtime.CredentialContext, error) {
	result := runtime.CredentialContext{Method: method, Bindings: kit.CredentialBindingsFor(mode, method)}
	if mode.Executor.Tool == "" {
		return result, nil
	}
	var requirement kit.ToolRequirement
	for _, candidate := range pkg.Tools {
		if candidate.ID == mode.Executor.Tool {
			requirement = candidate
			break
		}
	}
	if requirement.ID == "" {
		return runtime.CredentialContext{}, fmt.Errorf("managed tool %s is not declared", mode.Executor.Tool)
	}
	if service.ResolveTool == nil {
		return runtime.CredentialContext{}, fmt.Errorf("managed tool %s %s is not installed; run outfit tool install %s", requirement.ID, requirement.Version, requirement.ID)
	}
	path, err := service.ResolveTool(requirement)
	if err != nil {
		return runtime.CredentialContext{}, err
	}
	result.Binary = path
	return result, nil
}

func (service Service) secret(ctx context.Context, connection state.Connection) (map[string]string, error) {
	installed, err := service.State.Package(ctx, connection.ConnectorKey)
	if err != nil {
		return nil, err
	}
	method, ok := kit.FindCredentialMethod(installed.Package, connection.CredentialMethod)
	if !ok {
		return nil, fmt.Errorf("selected credential method %s no longer exists", connection.CredentialMethod)
	}
	result, err := service.storedCredentialFields(ctx, connection)
	if err != nil {
		return nil, err
	}
	if method.OAuth != nil {
		refreshed, refreshErr := RefreshOAuthFields(ctx, method, result)
		if refreshErr != nil {
			if OAuthReconnectRequired(refreshErr) {
				if connection.ConnectorKey == "slack" && connection.CredentialMethod == "adopted-host-oauth" {
					return nil, fmt.Errorf("slack host-owned MCP OAuth grant cannot be refreshed outside its original coding host; switch this connection to the recommended local CLI with `outfit connector add slack --mode cli --name %s`: %w", connection.Name, refreshErr)
				}
				return nil, fmt.Errorf("OAuth sign-in for %s expired or was revoked; run `outfit credential replace %s` to reauthenticate: %w", connection.Name, connection.Name, refreshErr)
			}
			return nil, refreshErr
		}
		if oauthGrantChanged(result, refreshed) {
			result, err = service.persistRefreshedOAuthGrant(ctx, connection, method, refreshed)
		} else {
			result = refreshed
		}
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (service Service) storedCredentialFields(ctx context.Context, connection state.Connection) (map[string]string, error) {
	result := map[string]string{}
	if connection.CredentialRef != "" {
		stored, err := service.Vault.Get(ctx, connection.CredentialRef)
		if err != nil {
			return nil, err
		}
		for key, value := range stored {
			result[key] = value
		}
	}
	for key, value := range connection.SafeIdentity {
		if strings.HasPrefix(key, "setting.") {
			result[strings.TrimPrefix(key, "setting.")] = value
		}
	}
	return result, nil
}

func oauthGrantChanged(before, after map[string]string) bool {
	for _, key := range []string{oauthAccessTokenField, oauthRefreshTokenField, oauthTokenExpiryField} {
		if before[key] != after[key] {
			return true
		}
	}
	return false
}

func (service Service) persistRefreshedOAuthGrant(ctx context.Context, connection state.Connection, method kit.CredentialMethod, fields map[string]string) (map[string]string, error) {
	candidateRef, err := service.Vault.Put(ctx, secretFields(method, fields))
	if err != nil {
		return nil, fmt.Errorf("store refreshed OAuth grant: %w", err)
	}
	cleanupID := newID("clean")
	rotated, err := service.State.RotateCredentialSecret(ctx, connection.CredentialID, connection.CredentialRef, candidateRef, cleanupID, safeSettings(method, fields))
	if err != nil {
		_ = service.Vault.Delete(context.Background(), candidateRef)
		return nil, fmt.Errorf("commit refreshed OAuth grant: %w", err)
	}
	if !rotated {
		_ = service.Vault.Delete(context.Background(), candidateRef)
		current, currentErr := service.State.Connection(ctx, connection.Name)
		if currentErr != nil {
			return nil, errors.New("OAuth grant changed concurrently; retry")
		}
		return service.storedCredentialFields(ctx, current)
	}
	if connection.CredentialRef != "" {
		if deleteErr := service.Vault.Delete(ctx, connection.CredentialRef); deleteErr == nil {
			_ = service.State.FinishCleanup(ctx, cleanupID)
		}
	}
	return fields, nil
}

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func credentialFingerprint(method kit.CredentialMethod, fields map[string]string) string {
	hash := sha256.New()
	secretCount := 0
	for _, field := range method.Fields {
		if !field.Secret {
			continue
		}
		secretCount++
		_, _ = hash.Write([]byte(field.Key))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(fields[field.Key]))
		_, _ = hash.Write([]byte{0})
	}
	if secretCount == 0 {
		return "executor-managed"
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))[:12]
}

func secretFields(method kit.CredentialMethod, fields map[string]string) map[string]string {
	result := map[string]string{}
	for _, field := range method.Fields {
		if field.Secret && fields[field.Key] != "" {
			result[field.Key] = fields[field.Key]
		}
	}
	return result
}

func safeSettings(method kit.CredentialMethod, fields map[string]string) map[string]string {
	result := map[string]string{}
	for _, field := range method.Fields {
		if !field.Secret && fields[field.Key] != "" {
			result[field.Key] = fields[field.Key]
		}
	}
	return result
}

func credentialEnvironment(fields map[string]string) string {
	for _, value := range fields {
		switch {
		case strings.HasPrefix(value, "rk_test_"), strings.HasPrefix(value, "sk_test_"):
			return "test"
		case strings.HasPrefix(value, "rk_live_"), strings.HasPrefix(value, "sk_live_"):
			return "live"
		}
	}
	return ""
}

func mergeSafeIdentity(identity map[string]string, value any) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, paths := range map[string][]string{
		"account_id":   {"id", "account_id", "accountId"},
		"account_name": {"name", "display_name", "displayName", "business_profile.name", "login"},
		"principal":    {"email", "principal", "user.email", "user"},
	} {
		for _, path := range paths {
			if found := nestedString(object, path); found != "" {
				identity[key] = found
				break
			}
		}
	}
	if livemode, ok := object["livemode"].(bool); ok {
		if livemode {
			identity["environment"] = "live"
		} else {
			identity["environment"] = "test"
		}
	}
}

func nestedString(object map[string]any, path string) string {
	var value any = object
	for _, component := range strings.Split(path, ".") {
		current, ok := value.(map[string]any)
		if !ok {
			return ""
		}
		value = current[component]
	}
	switch current := value.(type) {
	case string:
		return current
	case fmt.Stringer:
		return current.String()
	default:
		return ""
	}
}

func newID(prefix string) string {
	raw := make([]byte, 12)
	_, _ = rand.Read(raw)
	return prefix + "_" + hex.EncodeToString(raw)
}
