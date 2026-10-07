package importer

import (
	"context"
	"errors"
	"net/url"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
)

func importMCP(ctx context.Context, options Options) (Result, error) {
	if options.Source == "" {
		return Result{}, errors.New("MCP endpoint is required")
	}
	endpoint, err := url.Parse(options.Source)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return Result{}, errors.New("MCP source must be a fixed HTTPS endpoint")
	}
	mode := kit.Mode{ID: "mcp", Label: "Hosted MCP", Description: "Imported from live Streamable HTTP MCP discovery.", Recommendation: kit.Recommendation{Rank: 1, Reason: "The hosted MCP server is the imported command surface."}, Executor: kit.Executor{Kind: "mcp", Endpoint: options.Source, PrivateNet: options.PrivateNetwork}}
	discover := options.MCPDiscover
	if discover == nil {
		discover = runtime.DiscoverMCP
	}
	tools, err := discover(ctx, mode, options.Secrets)
	if err != nil {
		return Result{}, err
	}
	if len(tools) == 0 {
		return Result{}, errors.New("MCP endpoint exposed no tools")
	}
	for _, tool := range tools {
		schema, _ := tool.InputSchema.(map[string]any)
		mode.Operations = append(mode.Operations, kit.Operation{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: tool.Class, Input: schema, MCPTool: tool.Name})
	}
	mode.Operations = uniqueOperations(mode.Operations)
	result := Result{Package: basePackage(options, mode)}
	result.Warnings = importWarnings(options, "Unannotated MCP tools are classified destructive; review upstream annotations and generated classifications.")
	return result, nil
}
