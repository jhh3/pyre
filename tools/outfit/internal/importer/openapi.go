package importer

import (
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
)

var openAPIParameter = regexp.MustCompile(`\{([A-Za-z0-9_.-]+)\}`)

func importOpenAPI(options Options) (Result, error) {
	document, err := readDocument(options.Source)
	if err != nil {
		return Result{}, err
	}
	if stringValue(document["openapi"]) == "" && stringValue(document["swagger"]) == "" {
		return Result{}, errors.New("document is not OpenAPI or Swagger")
	}
	baseURL := options.BaseURL
	if baseURL == "" {
		servers := sliceValue(document["servers"])
		if len(servers) > 0 {
			baseURL = stringValue(mapValue(servers[0])["url"])
		}
	}
	if baseURL == "" {
		baseURL = swaggerBaseURL(document)
	}
	baseURL, err = normalizeBaseURL(baseURL)
	if err != nil {
		return Result{}, err
	}
	paths := mapValue(document["paths"])
	var operations []kit.Operation
	var suggestions []string
	methods := []string{"get", "post", "put", "patch", "delete", "head", "options"}
	for path, rawPath := range paths {
		pathItem := mapValue(rawPath)
		for _, method := range methods {
			rawOperation, ok := pathItem[method]
			if !ok {
				continue
			}
			operationValue := mapValue(rawOperation)
			name := stringValue(operationValue["operationId"])
			if name == "" {
				name = method + "_" + strings.Trim(path, "/")
			}
			parameters := append(sliceValue(pathItem["parameters"]), sliceValue(operationValue["parameters"])...)
			input := map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
			properties := input["properties"].(map[string]any)
			var required, query, body []string
			queryNames, bodyNames := map[string]string{}, map[string]string{}
			for _, rawParameter := range parameters {
				parameter := mapValue(resolveLocalRef(document, rawParameter))
				providerParameterName := stringValue(parameter["name"])
				if providerParameterName == "" {
					continue
				}
				parameterName := operationName(providerParameterName)
				schema := mapValue(resolveLocalRef(document, parameter["schema"]))
				if len(schema) == 0 {
					schema = map[string]any{"type": "string"}
				}
				location := stringValue(parameter["in"])
				if location == "body" {
					for property, value := range mapValue(schema["properties"]) {
						propertyName := operationName(property)
						properties[propertyName] = resolveLocalRef(document, value)
						body = append(body, propertyName)
						if propertyName != property {
							bodyNames[propertyName] = property
						}
					}
					for _, value := range sliceValue(schema["required"]) {
						required = append(required, operationName(stringValue(value)))
					}
					continue
				}
				properties[parameterName] = schema
				if parameter["required"] == true {
					required = append(required, parameterName)
				}
				if location == "query" {
					query = append(query, parameterName)
					if parameterName != providerParameterName {
						queryNames[parameterName] = providerParameterName
					}
				}
			}
			requestBody := mapValue(resolveLocalRef(document, operationValue["requestBody"]))
			content := mapValue(requestBody["content"])
			media := mapValue(content["application/json"])
			bodySchema := mapValue(resolveLocalRef(document, media["schema"]))
			for property, schema := range mapValue(bodySchema["properties"]) {
				propertyName := operationName(property)
				properties[propertyName] = resolveLocalRef(document, schema)
				body = append(body, propertyName)
				if propertyName != property {
					bodyNames[propertyName] = property
				}
			}
			if values := sliceValue(bodySchema["required"]); len(values) > 0 {
				for _, value := range values {
					required = append(required, operationName(stringValue(value)))
				}
			}
			required = sortedUniqueStrings(required)
			query = sortedUniqueStrings(query)
			body = sortedUniqueStrings(body)
			if len(required) > 0 {
				input["required"] = required
			}
			description := stringValue(operationValue["summary"])
			if description == "" {
				description = stringValue(operationValue["description"])
			}
			if description == "" {
				description = strings.ToUpper(method) + " " + path
			}
			operations = append(operations, kit.Operation{Name: name, Title: stringValue(operationValue["summary"]), Description: description, Class: classifyMethod(method), Input: input, HTTP: &kit.HTTPOperation{Method: strings.ToUpper(method), Path: openAPIPath(ensureRelativeToBase(baseURL, path)), Query: query, QueryNames: queryNames, Body: body, BodyNames: bodyNames}})
			if strings.EqualFold(method, "get") && strings.Contains(path, "{") {
				suggestions = append(suggestions, path)
			}
		}
	}
	if len(operations) == 0 {
		return Result{}, errors.New("OpenAPI document contains no supported operations")
	}
	operations = uniqueOperations(operations)
	sort.Strings(suggestions)
	mode := kit.Mode{ID: "rest", Label: "REST API", Description: "Generated from the provider OpenAPI contract.", Recommendation: kit.Recommendation{Rank: 1, Reason: "The fixed-origin API is the imported command surface."}, Executor: kit.Executor{Kind: "http", BaseURL: baseURL, PrivateNet: options.PrivateNetwork}, Operations: operations}
	mode.Verify = safeVerify(operations)
	result := Result{Package: basePackage(options, mode), Suggestions: suggestions, Warnings: importWarnings(options, "Non-GET operations default to write and DELETE defaults to destructive; review provider semantics before trust.")}
	if options.LinkHost != "" {
		result.Package.Links = openAPILinks(options.LinkHost, operations)
	}
	return result, nil
}

func resolveLocalRef(document map[string]any, value any) any {
	current := value
	seen := map[string]bool{}
	for {
		object := mapValue(current)
		reference := stringValue(object["$ref"])
		if !strings.HasPrefix(reference, "#/") || seen[reference] {
			return current
		}
		seen[reference] = true
		var resolved any = document
		for _, raw := range strings.Split(strings.TrimPrefix(reference, "#/"), "/") {
			part := strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~")
			resolved = mapValue(resolved)[part]
			if resolved == nil {
				return current
			}
		}
		current = resolved
	}
}

func normalizeBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("a fixed HTTPS --base-url or OpenAPI server URL is required")
	}
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	return parsed.String(), nil
}

func swaggerBaseURL(document map[string]any) string {
	host := stringValue(document["host"])
	if host == "" {
		return ""
	}
	scheme := "https"
	if schemes := sliceValue(document["schemes"]); len(schemes) > 0 && stringValue(schemes[0]) == "https" {
		scheme = "https"
	}
	return scheme + "://" + host + strings.TrimSuffix(stringValue(document["basePath"]), "/") + "/"
}

func openAPIPath(path string) string {
	return openAPIParameter.ReplaceAllStringFunc(path, func(match string) string {
		parts := openAPIParameter.FindStringSubmatch(match)
		return ":" + operationName(parts[1])
	})
}

func safeVerify(operations []kit.Operation) string {
	for _, operation := range operations {
		if operation.Class != "read" {
			continue
		}
		required, _ := operation.Input["required"].([]string)
		if len(required) == 0 && !strings.Contains(operation.HTTP.Path, ":") {
			return operation.Name
		}
	}
	return ""
}

func openAPILinks(host string, operations []kit.Operation) []kit.LinkResolver {
	if strings.ContainsAny(host, "/:@?#*") || strings.ToLower(host) != host {
		return nil
	}
	var result []kit.LinkResolver
	for _, operation := range operations {
		if operation.Class != "read" || operation.HTTP == nil || !strings.Contains(operation.HTTP.Path, ":") {
			continue
		}
		path := operation.HTTP.Path
		arguments := map[string]string{}
		segments := map[string]kit.SegmentRule{}
		matches := regexp.MustCompile(`:([A-Za-z0-9_]+)`).FindAllStringSubmatch(path, -1)
		for _, match := range matches {
			arguments[match[1]] = match[1]
			segments[match[1]] = kit.SegmentRule{Kind: "slug"}
		}
		result = append(result, kit.LinkResolver{ID: operationName("link_" + operation.Name), ResourceType: operation.Name, Patterns: []kit.LinkPattern{{Hosts: []string{host}, Path: path, Segments: segments}}, Lookup: kit.LinkLookup{Mode: "rest", Operation: operation.Name, Arguments: arguments}, Presentation: kit.LinkPresentation{CanonicalID: "id", Title: "name", Summary: "description"}, Limits: kit.LinkLimits{TimeoutMS: 10000, MaxResultBytes: 65536, CacheTTLSeconds: 60}})
	}
	return result
}

func ensureRelativeToBase(baseURL, rawPath string) string {
	base, err := url.Parse(baseURL)
	if err != nil || base.Path == "/" {
		return rawPath
	}
	trimmed := strings.TrimSuffix(base.Path, "/")
	if strings.HasPrefix(rawPath, trimmed+"/") {
		return strings.TrimPrefix(rawPath, trimmed)
	}
	return rawPath
}
