package importer

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
)

func importPostman(options Options) (Result, error) {
	document, err := readDocument(options.Source)
	if err != nil {
		return Result{}, err
	}
	variables := map[string]string{}
	for _, raw := range sliceValue(document["variable"]) {
		item := mapValue(raw)
		variables[stringValue(item["key"])] = stringValue(item["value"])
	}
	var requests []postmanRequest
	collectPostman(sliceValue(document["item"]), "", variables, &requests)
	if len(requests) == 0 {
		return Result{}, errors.New("postman collection contains no requests")
	}
	baseURL := options.BaseURL
	if baseURL == "" {
		first, err := url.Parse(requests[0].URL)
		if err != nil {
			return Result{}, err
		}
		baseURL = first.Scheme + "://" + first.Host + "/"
	}
	baseURL, err = normalizeBaseURL(baseURL)
	if err != nil {
		return Result{}, err
	}
	base, _ := url.Parse(baseURL)
	var operations []kit.Operation
	var warnings []string
	for _, request := range requests {
		target, err := url.Parse(request.URL)
		if err != nil || target.Scheme != base.Scheme || target.Host != base.Host {
			warnings = append(warnings, "Skipped cross-origin or unresolved request "+request.Name)
			continue
		}
		input := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
		properties := input["properties"].(map[string]any)
		var query, body []string
		queryNames, bodyNames := map[string]string{}, map[string]string{}
		for name := range target.Query() {
			providerName := name
			name = operationName(providerName)
			properties[name] = map[string]any{"type": "string"}
			query = append(query, name)
			if name != providerName {
				queryNames[name] = providerName
			}
		}
		if request.Body != "" {
			var object map[string]any
			if json.Unmarshal([]byte(request.Body), &object) == nil {
				for name, value := range object {
					providerName := name
					name = operationName(providerName)
					properties[name] = inferredSchema(value)
					body = append(body, name)
					if name != providerName {
						bodyNames[name] = providerName
					}
				}
			} else {
				warnings = append(warnings, "Request "+request.Name+" has a non-object body and needs manual modeling")
			}
		}
		path := ensureRelativeToBase(baseURL, target.Path)
		for _, match := range regexp.MustCompile(`\{\{([A-Za-z0-9_.-]+)\}\}`).FindAllStringSubmatch(path, -1) {
			name := operationName(match[1])
			path = strings.ReplaceAll(path, match[0], ":"+name)
			properties[name] = map[string]any{"type": "string"}
		}
		for _, segment := range strings.Split(strings.Trim(path, "/"), "/") {
			if strings.HasPrefix(segment, ":") {
				name := operationName(strings.TrimPrefix(segment, ":"))
				properties[name] = map[string]any{"type": "string"}
			}
		}
		sort.Strings(query)
		sort.Strings(body)
		operations = append(operations, kit.Operation{Name: request.Name, Title: request.Name, Description: request.Description, Class: classifyMethod(request.Method), Input: input, HTTP: &kit.HTTPOperation{Method: request.Method, Path: path, Query: query, QueryNames: queryNames, Body: body, BodyNames: bodyNames}})
	}
	if len(operations) == 0 {
		return Result{}, errors.New("postman collection has no requests on the selected fixed origin")
	}
	operations = uniqueOperations(operations)
	mode := kit.Mode{ID: "rest", Label: "REST API", Description: "Generated from a Postman collection with one fixed HTTPS origin.", Recommendation: kit.Recommendation{Rank: 1, Reason: "The fixed-origin API is the imported command surface."}, Executor: kit.Executor{Kind: "http", BaseURL: baseURL, PrivateNet: options.PrivateNetwork}, Operations: operations, Verify: safeVerify(operations)}
	warnings = append(warnings, "Postman scripts, dynamic variables, cookies, and collection auth are not executed; model credentials explicitly with import flags.")
	return Result{Package: basePackage(options, mode), Warnings: importWarnings(options, warnings...)}, nil
}

type postmanRequest struct{ Name, Description, Method, URL, Body string }

func collectPostman(items []any, prefix string, variables map[string]string, result *[]postmanRequest) {
	for _, raw := range items {
		item := mapValue(raw)
		name := stringValue(item["name"])
		if request := mapValue(item["request"]); len(request) > 0 {
			rawURL := request["url"]
			urlText := stringValue(rawURL)
			if object := mapValue(rawURL); len(object) > 0 {
				urlText = stringValue(object["raw"])
				if urlText == "" {
					urlText = postmanStructuredURL(object)
				}
			}
			for key, value := range variables {
				urlText = strings.ReplaceAll(urlText, "{{"+key+"}}", value)
			}
			body := stringValue(mapValue(request["body"])["raw"])
			description := stringValue(request["description"])
			if description == "" {
				description = stringValue(item["description"])
			}
			if description == "" {
				description = strings.ToUpper(stringValue(request["method"])) + " " + urlText
			}
			*result = append(*result, postmanRequest{Name: prefix + name, Description: description, Method: strings.ToUpper(stringValue(request["method"])), URL: urlText, Body: body})
		}
		if children := sliceValue(item["item"]); len(children) > 0 {
			collectPostman(children, prefix+name+"_", variables, result)
		}
	}
}

func postmanStructuredURL(value map[string]any) string {
	protocol := stringValue(value["protocol"])
	if protocol == "" {
		protocol = "https"
	}
	var host, path []string
	for _, item := range sliceValue(value["host"]) {
		host = append(host, stringValue(item))
	}
	for _, item := range sliceValue(value["path"]) {
		path = append(path, stringValue(item))
	}
	target := protocol + "://" + strings.Join(host, ".") + "/" + strings.Join(path, "/")
	var query []string
	for _, raw := range sliceValue(value["query"]) {
		item := mapValue(raw)
		if item["disabled"] == true {
			continue
		}
		query = append(query, url.QueryEscape(stringValue(item["key"]))+"="+url.QueryEscape(stringValue(item["value"])))
	}
	if len(query) > 0 {
		target += "?" + strings.Join(query, "&")
	}
	return target
}

func inferredSchema(value any) map[string]any {
	switch value.(type) {
	case bool:
		return map[string]any{"type": "boolean"}
	case float64, int, int64:
		return map[string]any{"type": "number"}
	case []any:
		return map[string]any{"type": "array"}
	case map[string]any:
		return map[string]any{"type": "object"}
	default:
		return map[string]any{"type": "string"}
	}
}
