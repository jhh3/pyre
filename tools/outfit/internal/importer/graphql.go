package importer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
)

const introspectionQuery = `query OutfitIntrospection { __schema { queryType { name } mutationType { name } types { kind name fields { name description args { name type { kind name ofType { kind name ofType { kind name } } } } type { kind name ofType { kind name ofType { kind name } } } } } } }`

type graphQLType struct {
	Kind, Name string
	OfType     *graphQLType `json:"ofType"`
}

type graphQLField struct {
	Name, Description string
	Args              []struct {
		Name string
		Type graphQLType
	}
	Type graphQLType
}

func importGraphQL(ctx context.Context, options Options) (Result, error) {
	endpoint, err := url.Parse(options.Source)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return Result{}, errors.New("GraphQL source must be a fixed HTTPS endpoint")
	}
	body, _ := json.Marshal(map[string]string{"query": introspectionQuery})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Result{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if method := importedCredential(options, "http"); len(method.Bindings) > 0 {
		binding := method.Bindings[0]
		value := binding.Value
		for key, secret := range options.Secrets {
			value = strings.ReplaceAll(value, "{{secret."+key+"}}", secret)
		}
		request.Header.Set(binding.Name, value)
	}
	client := options.HTTPClient
	if client == nil {
		client = runtime.SafeHTTPClient(options.PrivateNetwork)
	}
	response, err := client.Do(request)
	if err != nil {
		return Result{}, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return Result{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return Result{}, fmt.Errorf("GraphQL introspection returned %s", response.Status)
	}
	var payload struct {
		Data struct {
			Schema struct {
				QueryType, MutationType struct{ Name string }
				Types                   []struct {
					Kind, Name string
					Fields     []graphQLField
				}
			} `json:"__schema"`
		}
		Errors []any
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Result{}, err
	}
	if len(payload.Errors) > 0 || payload.Data.Schema.QueryType.Name == "" {
		return Result{}, errors.New("GraphQL introspection was refused or incomplete")
	}
	fieldsByType := map[string][]graphQLField{}
	for _, item := range payload.Data.Schema.Types {
		fieldsByType[item.Name] = item.Fields
	}
	var operations []kit.Operation
	operations = append(operations, graphQLOperations(fieldsByType[payload.Data.Schema.QueryType.Name], "read", endpoint.EscapedPath())...)
	operations = append(operations, graphQLOperations(fieldsByType[payload.Data.Schema.MutationType.Name], "write", endpoint.EscapedPath())...)
	if len(operations) == 0 {
		return Result{}, errors.New("GraphQL schema exposed no query or mutation fields")
	}
	operations = uniqueOperations(operations)
	baseURL := endpoint.Scheme + "://" + endpoint.Host + "/"
	mode := kit.Mode{ID: "graphql", Label: "GraphQL API", Description: "Generated from live GraphQL introspection against one fixed HTTPS endpoint.", Recommendation: kit.Recommendation{Rank: 1, Reason: "The fixed-origin GraphQL API is the imported command surface."}, Executor: kit.Executor{Kind: "http", BaseURL: baseURL, PrivateNet: options.PrivateNetwork}, Operations: operations}
	for _, operation := range operations {
		if operation.Name == "schema_info" {
			mode.Verify = operation.Name
			break
		}
	}
	return Result{Package: basePackage(options, mode), Warnings: importWarnings(options, "Mutations default to write; mark destructive mutations explicitly after review.", "Generated object-returning fields select only __typename; add provider-specific projections for useful results.")}, nil
}

func graphQLOperations(fields []graphQLField, class, path string) []kit.Operation {
	var result []kit.Operation
	if class == "read" {
		result = append(result, kit.Operation{Name: "schema_info", Title: "GraphQL service identity", Description: "Verify the GraphQL endpoint without reading provider data.", Class: "read", Input: map[string]any{"type": "object", "additionalProperties": false}, HTTP: &kit.HTTPOperation{Method: "POST", Path: path, BodyValues: map[string]any{"query": "query OutfitIdentity { __typename }"}}})
	}
	for _, field := range fields {
		input := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		properties := input["properties"].(map[string]any)
		variables := map[string]any{}
		var definitions, calls, required []string
		for _, argument := range field.Args {
			typeName := graphQLTypeName(argument.Type)
			definitions = append(definitions, "$"+argument.Name+": "+typeName)
			calls = append(calls, argument.Name+": $"+argument.Name)
			variables[argument.Name] = "{{arg." + argument.Name + "}}"
			properties[argument.Name] = map[string]any{"type": graphQLJSONType(argument.Type)}
			if argument.Type.Kind == "NON_NULL" {
				required = append(required, argument.Name)
			}
		}
		if len(required) > 0 {
			input["required"] = required
		}
		definition := ""
		if len(definitions) > 0 {
			definition = "(" + strings.Join(definitions, ", ") + ")"
		}
		call := ""
		if len(calls) > 0 {
			call = "(" + strings.Join(calls, ", ") + ")"
		}
		selection := ""
		if graphQLObject(argumentType(field.Type)) {
			selection = " { __typename }"
		}
		query := "query"
		if class != "read" {
			query = "mutation"
		}
		query += " OUTFIT_" + operationName(field.Name) + definition + " { " + field.Name + call + selection + " }"
		description := field.Description
		if description == "" {
			description = query + " field " + field.Name
		}
		result = append(result, kit.Operation{Name: field.Name, Title: field.Name, Description: description, Class: class, Input: input, HTTP: &kit.HTTPOperation{Method: "POST", Path: path, BodyValues: map[string]any{"query": query, "variables": variables}}})
	}
	return result
}

func graphQLTypeName(value graphQLType) string {
	if value.Kind == "NON_NULL" {
		if value.OfType == nil {
			return "String!"
		}
		return graphQLTypeName(*value.OfType) + "!"
	}
	if value.Kind == "LIST" {
		if value.OfType == nil {
			return "[String]"
		}
		return "[" + graphQLTypeName(*value.OfType) + "]"
	}
	return value.Name
}

func graphQLJSONType(value graphQLType) string {
	for value.Kind == "NON_NULL" && value.OfType != nil {
		value = *value.OfType
	}
	if value.Kind == "LIST" {
		return "array"
	}
	switch value.Name {
	case "Boolean":
		return "boolean"
	case "Int", "Float":
		return "number"
	default:
		if value.Kind == "LIST" {
			return "array"
		}
		return "string"
	}
}

func argumentType(value graphQLType) graphQLType {
	for value.OfType != nil && (value.Kind == "NON_NULL" || value.Kind == "LIST") {
		value = *value.OfType
	}
	return value
}

func graphQLObject(value graphQLType) bool {
	return value.Kind == "OBJECT" || value.Kind == "INTERFACE" || value.Kind == "UNION"
}
