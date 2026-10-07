package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/jhh3/outfit/internal/agentio"
	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/invoke"
	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/linkcontext"
	"github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/state"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func Run(ctx context.Context, store *state.Store, service connection.Service) error {
	server, err := New(ctx, store, service)
	if err != nil {
		return err
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

func New(ctx context.Context, store *state.Store, service connection.Service) (*mcp.Server, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "outfit", Version: "0.1.0"}, nil)
	catalog, err := operationCatalog(ctx, store, service)
	if err != nil {
		return nil, err
	}
	readOnly := true
	destructive := false
	server.AddTool(&mcp.Tool{
		Name: "outfit_search", Title: "Find an Outfit CLI operation",
		Description:  "Search active Outfit connections and return concise typed CLI entry points. Prefer running the returned CLI command in a shell; this tool is discovery fallback only.",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string", "description": "Task, provider, resource, or operation to find."}}, "required": []string{"query"}, "additionalProperties": false},
		OutputSchema: resultSchema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive},
	}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments, failure := toolArguments(request)
		if failure != nil {
			return failure, nil
		}
		query, _ := arguments["query"].(string)
		query = strings.TrimSpace(query)
		if query == "" {
			return compactFailure("search", "invalid_arguments", "query is required", "Use outfit search '<task>' in a shell for the richest interactive search."), nil
		}
		matches := make([]map[string]any, 0)
		for _, entry := range catalog {
			if operationMatches(entry, query) {
				matches = append(matches, entry.summary())
			}
		}
		truncated := len(matches) > 50
		if truncated {
			matches = matches[:50]
		}
		envelope := invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "search", Class: "read", Data: matches, Meta: invoke.Meta{Pages: 1, Items: len(matches), Truncated: truncated}}
		return envelopeResult(envelope, false), nil
	})

	server.AddTool(&mcp.Tool{
		Name: "outfit_describe", Title: "Describe an Outfit CLI operation",
		Description:  "Return the selected connection mode, operation schema, and preferred typed CLI command. Prefer executing through the Outfit CLI.",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{"connection": map[string]any{"type": "string"}, "operation": map[string]any{"type": "string", "description": "Optional canonical operation name."}}, "required": []string{"connection"}, "additionalProperties": false},
		OutputSchema: resultSchema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive},
	}, func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments, failure := toolArguments(request)
		if failure != nil {
			return failure, nil
		}
		connectionName, _ := arguments["connection"].(string)
		operationName, _ := arguments["operation"].(string)
		matches := make([]map[string]any, 0)
		for _, entry := range catalog {
			if entry.Connection != connectionName || operationName != "" && entry.Operation.Name != operationName {
				continue
			}
			matches = append(matches, entry.detail())
		}
		if len(matches) == 0 {
			return compactFailure("describe", "operation_not_found", "No active allowed Outfit operation matched that connection and operation.", "Run outfit ls, then outfit describe CONNECTION [OPERATION]."), nil
		}
		envelope := invoke.Envelope{Schema: invoke.Schema, OK: true, Connection: connectionName, Operation: "describe", Class: "read", Data: map[string]any{"cli_first": "Use the typed CLI command returned for execution; the MCP facade is a compact fallback.", "operations": matches}, Meta: invoke.Meta{Pages: 1, Items: len(matches)}}
		return envelopeResult(envelope, false), nil
	})

	server.AddTool(&mcp.Tool{
		Name: "outfit_run", Title: "Run one read-only Outfit operation",
		Description: "Fallback dispatcher for one read-only operation. Prefer the typed Outfit CLI shown by outfit_search/outfit_describe; mutations are intentionally CLI-only.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"connection": map[string]any{"type": "string"}, "operation": map[string]any{"type": "string"},
			"arguments": map[string]any{"type": "object", "additionalProperties": true}, "all": map[string]any{"type": "boolean"},
			"max_pages": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}, "max_items": map[string]any{"type": "integer", "minimum": 1, "maximum": 100000},
		}, "required": []string{"connection", "operation"}, "additionalProperties": false},
		OutputSchema: resultSchema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &destructive},
	}, func(callCtx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		arguments, failure := toolArguments(request)
		if failure != nil {
			return failure, nil
		}
		connectionName, _ := arguments["connection"].(string)
		operationName, _ := arguments["operation"].(string)
		entry, ok := findCatalogOperation(catalog, connectionName, operationName)
		if !ok {
			return compactFailure("run", "operation_not_found", "No active allowed Outfit operation matched that connection and operation.", "Run outfit search '<task>' in a shell."), nil
		}
		if entry.Operation.Class != "read" {
			return compactFailure("run", "cli_required_for_mutation", "The compact MCP facade is read-only.", entry.cliCommand()+" --write requires explicit human intent in a shell."), nil
		}
		flat := flattenArguments(arguments["arguments"])
		options := invoke.Options{All: boolArgument(arguments, "all"), MaxPages: intArgument(arguments, "max_pages"), MaxItems: intArgument(arguments, "max_items")}
		envelope, runErr := invoke.Run(callCtx, invoke.Metadata{Connection: entry.Connection, Connector: entry.Connector, Mode: entry.Mode}, entry.Operation, flat, options, func(pageCtx context.Context, pageArguments map[string]string) (runtime.Result, error) {
			return service.Run(pageCtx, entry.Connection, entry.Operation.Name, pageArguments, false)
		})
		return envelopeResult(envelope, runErr != nil), nil
	})

	preferences, _ := linkcontext.LoadPreferences(ctx, store)
	getTool := &mcp.Tool{Name: "outfit_get_url", Title: "Read a linked provider resource", Description: "Resolve a supported provider URL through the active selected mode. Prefer `outfit get URL` in a shell; this compact fallback returns bounded untrusted external evidence.", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string", "description": "HTTPS provider URL"}, "mode": map[string]any{"type": "string", "enum": []string{"heavy", "light"}, "description": "Optional one-shot depth; defaults to the connection preference."}, "connection": map[string]any{"type": "string", "description": "Named connection to use when more than one account matches."}}, "required": []string{"url"}, "additionalProperties": false}, OutputSchema: resultSchema(), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, DestructiveHint: &destructive}}
	server.AddTool(getTool, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments map[string]any
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return envelopeResult(invoke.Envelope{Schema: invoke.Schema, OK: false, Operation: "get_url", Class: "read", Meta: invoke.Meta{}, Error: &invoke.Failure{Code: "invalid_arguments", Message: err.Error()}}, true), nil
		}
		urlValue, _ := arguments["url"].(string)
		modeValue, _ := arguments["mode"].(string)
		connectionValue, _ := arguments["connection"].(string)
		links := linkcontext.Service{State: store, Connections: service, Preferences: preferences}
		options := linkcontext.ResolveOptions{Connection: connectionValue}
		if modeValue != "" {
			mode, err := linkcontext.ParseMode(modeValue)
			if err != nil || mode == linkcontext.ModeOff {
				return envelopeResult(invoke.Envelope{Schema: invoke.Schema, OK: false, Operation: "get_url", Class: "read", Meta: invoke.Meta{}, Error: &invoke.Failure{Code: "invalid_arguments", Message: "mode must be heavy or light"}}, true), nil
			}
			options.Mode = mode
		}
		resolution := links.ResolveDetailed(ctx, urlValue, options)
		if len(resolution.Resources) == 0 {
			failure := &invoke.Failure{Code: "link_not_resolved", Message: "No active selected-mode link resolver matched the URL.", Hint: "Use outfit ls and outfit connector show CONNECTOR to inspect supported links."}
			if len(resolution.Diagnostics) > 0 {
				diagnostic := resolution.Diagnostics[0]
				failure = &invoke.Failure{Code: diagnostic.Code, Message: diagnostic.Message, Hint: diagnostic.Hint}
			}
			return envelopeResult(invoke.Envelope{Schema: invoke.Schema, OK: false, Operation: "get_url", Class: "read", Meta: invoke.Meta{}, Error: failure}, true), nil
		}
		return envelopeResult(invoke.Envelope{Schema: invoke.Schema, OK: true, Operation: "get_url", Class: "read", Data: resolution.Resources, Meta: invoke.Meta{Pages: 1, Items: len(resolution.Resources)}}, false), nil
	})
	return server, nil
}

type catalogOperation struct {
	Connection, Connector, Mode string
	Operation                   kit.Operation
}

func operationCatalog(ctx context.Context, store *state.Store, service connection.Service) ([]catalogOperation, error) {
	connections, err := store.Connections(ctx)
	if err != nil {
		return nil, err
	}
	var catalog []catalogOperation
	for _, item := range connections {
		if item.Status != "active" || !service.Allows(item.Name) {
			continue
		}
		installed, err := store.Package(ctx, item.ConnectorKey)
		if err != nil {
			continue
		}
		mode, ok := runtime.FindMode(installed.Package, item.SelectedMode)
		if !ok {
			continue
		}
		operations := append([]kit.Operation{}, mode.Operations...)
		known := map[string]bool{}
		for index := range operations {
			operations[index] = kit.NormalizeOperation(operations[index])
			known[operations[index].Name] = true
		}
		tools, _ := store.Tools(ctx, item.ID)
		for _, tool := range tools {
			if !known[tool.Name] {
				operations = append(operations, kit.NormalizeOperation(kit.Operation{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, Input: tool.InputSchema, MCPTool: tool.SourceName}))
			}
		}
		for _, operation := range operations {
			if service.AllowsOperation(item.Name, operation.Name) {
				catalog = append(catalog, catalogOperation{Connection: item.Name, Connector: item.ConnectorKey, Mode: mode.ID, Operation: operation})
			}
		}
	}
	sort.Slice(catalog, func(left, right int) bool {
		if catalog[left].Connection != catalog[right].Connection {
			return catalog[left].Connection < catalog[right].Connection
		}
		return catalog[left].Operation.Name < catalog[right].Operation.Name
	})
	return catalog, nil
}

func (entry catalogOperation) cliCommand() string {
	return "outfit " + entry.Connection + " " + invoke.FriendlyOperationName(entry.Connector, entry.Operation.Name)
}

func (entry catalogOperation) summary() map[string]any {
	return map[string]any{"connection": entry.Connection, "connector": entry.Connector, "mode": entry.Mode, "operation": entry.Operation.Name, "title": entry.Operation.Title, "description": entry.Operation.Description, "class": entry.Operation.Class, "cli": entry.cliCommand() + " --help"}
}

func (entry catalogOperation) detail() map[string]any {
	result := entry.summary()
	result["input"] = entry.Operation.Input
	result["cli"] = entry.cliCommand()
	if entry.Operation.Class != "read" {
		result["cli"] = entry.cliCommand() + " --write"
	}
	return result
}

func operationMatches(entry catalogOperation, query string) bool {
	haystack := strings.ToLower(strings.Join([]string{entry.Connection, entry.Connector, entry.Mode, entry.Operation.Name, entry.Operation.Title, entry.Operation.Description, entry.Operation.Class}, " "))
	for _, term := range strings.Fields(strings.ToLower(query)) {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func findCatalogOperation(catalog []catalogOperation, connectionName, operationName string) (catalogOperation, bool) {
	for _, entry := range catalog {
		if entry.Connection == connectionName && entry.Operation.Name == operationName {
			return entry, true
		}
	}
	return catalogOperation{}, false
}

func toolArguments(request *mcp.CallToolRequest) (map[string]any, *mcp.CallToolResult) {
	arguments := map[string]any{}
	if len(request.Params.Arguments) > 0 {
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, compactFailure("tool", "invalid_arguments", err.Error(), "Inspect the tool input schema and retry.")
		}
	}
	return arguments, nil
}

func flattenArguments(value any) map[string]string {
	object, _ := value.(map[string]any)
	flat := make(map[string]string, len(object))
	for key, value := range object {
		if text, ok := value.(string); ok {
			flat[key] = text
		} else {
			encoded, _ := json.Marshal(value)
			flat[key] = string(encoded)
		}
	}
	return flat
}

func compactFailure(operation, code, message, hint string) *mcp.CallToolResult {
	return envelopeResult(invoke.Envelope{Schema: invoke.Schema, OK: false, Operation: operation, Class: "read", Meta: invoke.Meta{}, Error: &invoke.Failure{Code: code, Message: message, Hint: hint}}, true)
}

func resultSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"schema": map[string]any{"type": "string"}, "ok": map[string]any{"type": "boolean"}, "connection": map[string]any{"type": "string"}, "connector": map[string]any{"type": "string"}, "mode": map[string]any{"type": "string"}, "operation": map[string]any{"type": "string"}, "class": map[string]any{"type": "string"}, "data": map[string]any{}, "meta": map[string]any{"type": "object"}, "error": map[string]any{"type": "object"}}, "required": []string{"schema", "ok", "meta"}, "additionalProperties": false}
}

func envelopeResult(envelope invoke.Envelope, isError bool) *mcp.CallToolResult {
	prepared, encoded, err := agentio.Prepare(envelope, true)
	if err != nil {
		prepared = invoke.Envelope{Schema: invoke.Schema, OK: false, Operation: envelope.Operation, Class: envelope.Class, Meta: invoke.Meta{}, Error: invoke.Classify(err, "result_serialization_failed", "Retry the operation or use the typed Outfit CLI.")}
		encoded, _ = invoke.Marshal(prepared)
		isError = true
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}, StructuredContent: prepared, IsError: isError}
}

func boolArgument(arguments map[string]any, name string) bool {
	value, _ := arguments[name].(bool)
	return value
}

func intArgument(arguments map[string]any, name string) int {
	switch value := arguments[name].(type) {
	case float64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	default:
		return 0
	}
}
