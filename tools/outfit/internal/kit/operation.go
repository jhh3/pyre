package kit

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

var operationArgument = regexp.MustCompile(`\{\{(?:arg|setting)\.([A-Za-z0-9_]+)\}\}`)

// NormalizeOperation compiles implicit arguments in fixed HTTP/CLI templates
// into the same JSON schema consumed by the CLI, MCP facade, generated skills,
// validation, and runtime. Explicit author-supplied properties always win.
func NormalizeOperation(operation Operation) Operation {
	operation.Input = EffectiveInput(operation)
	return operation
}

// EffectiveInput returns a detached object schema. Connector authors may write
// a full schema, or rely on deterministic inference for path/argv templates and
// optional CLI/query/body arguments.
func EffectiveInput(operation Operation) map[string]any {
	input := cloneObject(operation.Input)
	if input == nil {
		input = map[string]any{}
	}
	input["type"] = "object"
	properties, _ := input["properties"].(map[string]any)
	properties = cloneObject(properties)
	if properties == nil {
		properties = map[string]any{}
	}
	required := requiredSet(input["required"])

	add := func(name string, isRequired bool) {
		if strings.TrimSpace(name) == "" {
			return
		}
		if _, exists := properties[name]; !exists {
			properties[name] = map[string]any{"type": "string", "description": "Connector operation argument " + name + "."}
		}
		if isRequired {
			required[name] = true
		}
	}
	addTemplates := func(values []string, isRequired bool) {
		for _, value := range values {
			for _, match := range operationArgument.FindAllStringSubmatch(value, -1) {
				add(match[1], isRequired)
			}
		}
	}

	if operation.CLI != nil {
		addTemplates(operation.CLI.Args, true)
		addTemplates([]string{operation.CLI.Stdin}, true)
		for _, optional := range operation.CLI.OptionalArgs {
			add(optional.Argument, false)
			if property, ok := properties[optional.Argument].(map[string]any); ok && optional.Boolean {
				property["type"] = "boolean"
			}
		}
	}
	if operation.HTTP != nil {
		addTemplates([]string{operation.HTTP.Path}, true)
		for _, segment := range strings.Split(operation.HTTP.Path, "/") {
			if strings.HasPrefix(segment, ":") {
				add(strings.TrimPrefix(segment, ":"), true)
			}
		}
		for _, name := range operation.HTTP.Query {
			add(name, false)
		}
		for _, name := range operation.HTTP.Body {
			add(name, false)
		}
		walkTemplateValues(operation.HTTP.BodyValues, func(name string) { add(name, false) })
		for _, value := range operation.HTTP.Headers {
			addTemplates([]string{value}, false)
		}
		for _, header := range operation.HTTP.ArgumentHeaders {
			add(header.Argument, false)
		}
		add(operation.HTTP.PathArgument, true)
		add(operation.HTTP.MethodArgument, true)
		add(operation.HTTP.QueryArgument, false)
		add(operation.HTTP.BodyArgument, false)
	}
	for _, step := range operation.Compose {
		for _, source := range step.Arguments {
			if !strings.HasPrefix(source, "=") {
				add(source, false)
			}
		}
	}
	if operation.Pagination != nil {
		add(operation.Pagination.CursorArgument, false)
	}

	input["properties"] = properties
	if _, exists := input["additionalProperties"]; !exists {
		input["additionalProperties"] = false
	}
	if len(required) == 0 {
		delete(input, "required")
	} else {
		keys := make([]string, 0, len(required))
		for key := range required {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		input["required"] = keys
	}
	aliases := ArgumentAliases(properties)
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(stringValue(property["description"])) == "" {
			property["description"] = "Connector operation argument " + name + "."
		}
		if _, hasDefault := property["default"]; !hasDefault && property["examples"] == nil && property["enum"] == nil {
			property["examples"] = []any{argumentExample(name, stringValue(property["type"]))}
		}
		if values := aliases[name]; len(values) > 0 {
			property["x-outfit-aliases"] = values
		}
	}
	return input
}

// ArgumentAliases returns only unambiguous, deterministic spellings. The same
// table is embedded in operation schemas and consumed by the CLI parser.
func ArgumentAliases(properties map[string]any) map[string][]string {
	candidates := map[string]map[string]bool{}
	for name := range properties {
		values := []string{strings.ReplaceAll(name, "_", "-")}
		parts := strings.Split(name, "_")
		camel := parts[0]
		for _, part := range parts[1:] {
			if part != "" {
				camel += strings.ToUpper(part[:1]) + part[1:]
			}
		}
		values = append(values, camel)
		if strings.HasSuffix(name, "_id") {
			values = append(values, strings.TrimSuffix(name, "_id"))
		}
		switch name {
		case "query":
			values = append(values, "q")
		case "q":
			values = append(values, "query")
		case "channel_id":
			values = append(values, "channel")
		case "message_ts", "thread_ts", "timestamp":
			values = append(values, "ts")
		}
		for _, alias := range values {
			alias = strings.TrimSpace(alias)
			if alias != "" && alias != name {
				if candidates[alias] == nil {
					candidates[alias] = map[string]bool{}
				}
				candidates[alias][name] = true
			}
		}
	}
	result := map[string][]string{}
	for alias, nameSet := range candidates {
		var names []string
		for name := range nameSet {
			names = append(names, name)
		}
		if len(names) == 1 {
			result[names[0]] = append(result[names[0]], alias)
		}
	}
	for name := range result {
		sort.Strings(result[name])
	}
	return result
}

func MatchArgumentNames(properties map[string]any, requested string) []string {
	normalized := strings.ReplaceAll(strings.TrimSpace(requested), "-", "_")
	if _, ok := properties[normalized]; ok {
		return []string{normalized}
	}
	signature := argumentSignature(normalized)
	var matches []string
	aliases := ArgumentAliases(properties)
	for name := range properties {
		if argumentSignature(name) == signature {
			matches = append(matches, name)
			continue
		}
		for _, alias := range aliases[name] {
			if argumentSignature(alias) == signature {
				matches = append(matches, name)
				break
			}
		}
	}
	sort.Strings(matches)
	return matches
}

func AuditOperationSchema(operation Operation) []string {
	schema := EffectiveInput(operation)
	properties, _ := schema["properties"].(map[string]any)
	var issues []string
	for name := range requiredSet(schema["required"]) {
		if _, ok := properties[name]; !ok {
			issues = append(issues, fmt.Sprintf("required argument %s has no property", name))
		}
	}
	for name, raw := range properties {
		if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`).MatchString(name) {
			issues = append(issues, fmt.Sprintf("argument %s needs a plain identifier", name))
		}
		property, ok := raw.(map[string]any)
		if !ok {
			issues = append(issues, fmt.Sprintf("argument %s schema must be an object", name))
			continue
		}
		kind := stringValue(property["type"])
		if !contains([]string{"string", "integer", "number", "boolean", "object", "array"}, kind) {
			issues = append(issues, fmt.Sprintf("argument %s has unsupported type %q", name, kind))
		}
		if strings.TrimSpace(stringValue(property["description"])) == "" {
			issues = append(issues, fmt.Sprintf("argument %s needs a description", name))
		}
		if value, exists := property["default"]; exists && !schemaValueMatches(kind, value) {
			issues = append(issues, fmt.Sprintf("argument %s default does not match %s", name, kind))
		}
		if examples, exists := property["examples"]; exists {
			values, ok := examples.([]any)
			if !ok || len(values) == 0 {
				issues = append(issues, fmt.Sprintf("argument %s examples must be a non-empty array", name))
			} else {
				for _, value := range values {
					if !schemaValueMatches(kind, value) {
						issues = append(issues, fmt.Sprintf("argument %s example does not match %s", name, kind))
						break
					}
				}
			}
		}
		if enum, exists := property["enum"]; exists {
			values, ok := enum.([]any)
			if !ok || len(values) == 0 {
				issues = append(issues, fmt.Sprintf("argument %s enum must be a non-empty array", name))
			} else {
				for _, value := range values {
					if !schemaValueMatches(kind, value) {
						issues = append(issues, fmt.Sprintf("argument %s enum value does not match %s", name, kind))
						break
					}
				}
				if value, hasDefault := property["default"]; hasDefault && !containsSchemaValue(values, value) {
					issues = append(issues, fmt.Sprintf("argument %s default is not in enum", name))
				}
			}
		}
	}
	sort.Strings(issues)
	return issues
}

func argumentSignature(value string) string {
	var result strings.Builder
	for _, character := range strings.ToLower(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			result.WriteRune(character)
		}
	}
	return result.String()
}

func argumentExample(name, kind string) any {
	switch kind {
	case "boolean":
		return true
	case "integer":
		return 10
	case "number":
		return 1.5
	case "object":
		return map[string]any{}
	case "array":
		return []any{}
	}
	lower := strings.ToLower(name)
	switch {
	case strings.Contains(lower, "query") || lower == "q":
		return "search terms"
	case strings.Contains(lower, "cursor") || strings.Contains(lower, "after"):
		return "next_cursor"
	case strings.Contains(lower, "channel"):
		return "C012ABC"
	case strings.Contains(lower, "timestamp") || strings.HasSuffix(lower, "_ts"):
		return "1712345678.123456"
	case strings.HasSuffix(lower, "_id") || lower == "id":
		return "example_id"
	default:
		return "example"
	}
}

func schemaValueMatches(kind string, value any) bool {
	switch kind {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		// A manifest that went through JSON (a backup, the state database)
		// carries whole numbers as float64 or json.Number; they are still
		// integers.
		switch number := value.(type) {
		case json.Number:
			if _, err := number.Int64(); err == nil {
				return true
			}
			parsed, err := number.Float64()
			return err == nil && wholeNumber(parsed)
		case float64:
			return wholeNumber(number)
		case float32:
			return wholeNumber(float64(number))
		}
		valueKind := reflect.TypeOf(value)
		return valueKind != nil && ((valueKind.Kind() >= reflect.Int && valueKind.Kind() <= reflect.Int64) || (valueKind.Kind() >= reflect.Uint && valueKind.Kind() <= reflect.Uint64))
	case "number":
		if number, ok := value.(json.Number); ok {
			_, err := number.Float64()
			return err == nil
		}
		valueKind := reflect.TypeOf(value)
		return valueKind != nil && (valueKind.Kind() >= reflect.Int && valueKind.Kind() <= reflect.Float64)
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		valueKind := reflect.TypeOf(value)
		return valueKind != nil && valueKind.Kind() == reflect.Slice
	default:
		return false
	}
}

func wholeNumber(value float64) bool {
	return !math.IsInf(value, 0) && !math.IsNaN(value) && value == math.Trunc(value)
}

func containsSchemaValue(values []any, wanted any) bool {
	for _, value := range values {
		if reflect.DeepEqual(value, wanted) {
			return true
		}
	}
	return false
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func walkTemplateValues(value any, add func(string)) {
	switch current := value.(type) {
	case string:
		for _, match := range operationArgument.FindAllStringSubmatch(current, -1) {
			add(match[1])
		}
	case map[string]any:
		for _, item := range current {
			walkTemplateValues(item, add)
		}
	case []any:
		for _, item := range current {
			walkTemplateValues(item, add)
		}
	}
}

func requiredSet(value any) map[string]bool {
	result := map[string]bool{}
	switch values := value.(type) {
	case []string:
		for _, item := range values {
			result[item] = true
		}
	case []any:
		for _, item := range values {
			if text, ok := item.(string); ok {
				result[text] = true
			}
		}
	}
	return result
}

func cloneObject(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	result := make(map[string]any, len(value))
	for key, item := range value {
		if nested, ok := item.(map[string]any); ok {
			result[key] = cloneObject(nested)
		} else {
			result[key] = item
		}
	}
	return result
}
