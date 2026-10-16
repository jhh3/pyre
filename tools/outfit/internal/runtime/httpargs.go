package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jhh3/outfit/internal/kit"
)

// preparedHTTP is everything argument-dependent about one HTTP request,
// computed identically for plans (with redacted secrets) and execution.
type preparedHTTP struct {
	method  string
	target  *url.URL
	headers map[string]string
	hasBody bool
	payload any
}

func prepareHTTP(mode kit.Mode, operation kit.Operation, arguments, secret map[string]string) (preparedHTTP, error) {
	contract := operation.HTTP
	arguments, err := normalizeRelativeTimes(operation, arguments, time.Now())
	if err != nil {
		return preparedHTTP{}, err
	}
	prepared := preparedHTTP{method: strings.ToUpper(contract.Method), headers: map[string]string{}}
	if contract.MethodArgument != "" {
		requested := strings.ToUpper(strings.TrimSpace(arguments[contract.MethodArgument]))
		allowed := false
		for _, method := range contract.Methods {
			allowed = allowed || strings.ToUpper(method) == requested
		}
		if !allowed {
			return preparedHTTP{}, fmt.Errorf("%s must be one of %s", contract.MethodArgument, strings.Join(contract.Methods, ", "))
		}
		prepared.method = requested
	}
	if prepared.target, err = httpTarget(mode, operation, arguments); err != nil {
		return preparedHTTP{}, err
	}
	for _, header := range contract.ArgumentHeaders {
		value, ok := arguments[header.Argument]
		if !ok || strings.TrimSpace(value) == "" {
			continue
		}
		if header.Encoding == "url" {
			value = url.PathEscape(value)
		} else if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return preparedHTTP{}, fmt.Errorf("%s may not contain control characters", header.Argument)
		}
		prepared.headers[header.Name] = value
	}
	switch {
	case contract.BodyArgument != "":
		raw, ok := arguments[contract.BodyArgument]
		if ok && strings.TrimSpace(raw) != "" {
			decoder := json.NewDecoder(strings.NewReader(raw))
			decoder.UseNumber()
			var body any
			if err := decoder.Decode(&body); err != nil || decoder.More() {
				return preparedHTTP{}, fmt.Errorf("%s must be one JSON value (object or array)", contract.BodyArgument)
			}
			prepared.hasBody, prepared.payload = true, body
		}
	case len(contract.Body) > 0 || len(contract.BodyValues) > 0:
		prepared.hasBody, prepared.payload = true, httpPayload(operation, arguments, secret)
	}
	return prepared, nil
}

// argumentCoder turns one string argument into its JSON body value.
type argumentCoder struct {
	literal   map[string]bool
	templates map[string]any
}

func newArgumentCoder(operation kit.Operation) argumentCoder {
	coder := argumentCoder{literal: map[string]bool{}, templates: operation.HTTP.ItemTemplates}
	for _, name := range operation.HTTP.StringArguments {
		coder.literal[name] = true
	}
	return coder
}

func (coder argumentCoder) value(name, raw string) any {
	if coder.literal[name] {
		return raw
	}
	value := coerceJSON(raw)
	template, ok := coder.templates[name]
	if !ok {
		return value
	}
	items, ok := value.([]any)
	if !ok {
		return value
	}
	expanded := make([]any, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			expanded = append(expanded, fillItem(template, text))
		} else {
			expanded = append(expanded, item)
		}
	}
	return expanded
}

func fillItem(template any, item string) any {
	switch current := template.(type) {
	case string:
		if current == "{{item}}" {
			return item
		}
		return strings.ReplaceAll(current, "{{item}}", item)
	case map[string]any:
		result := make(map[string]any, len(current))
		for key, value := range current {
			result[key] = fillItem(value, item)
		}
		return result
	case []any:
		result := make([]any, 0, len(current))
		for _, value := range current {
			result = append(result, fillItem(value, item))
		}
		return result
	default:
		return template
	}
}

// normalizeRelativeTimes rewrites relative-time arguments into RFC 3339 UTC
// timestamps (or the JSON literal null) without changing the caller's map.
func normalizeRelativeTimes(operation kit.Operation, arguments map[string]string, now time.Time) (map[string]string, error) {
	if len(operation.HTTP.RelativeTimeArguments) == 0 {
		return arguments, nil
	}
	result := cloneStrings(arguments)
	for _, name := range operation.HTTP.RelativeTimeArguments {
		raw, ok := arguments[name]
		if !ok {
			continue
		}
		value, err := relativeTime(raw, now)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		result[name] = value
	}
	return result, nil
}

func relativeTime(raw string, now time.Time) (string, error) {
	value := strings.TrimSpace(raw)
	switch strings.ToLower(value) {
	case "", "0", "none", "null", "clear", "off":
		return "null", nil
	}
	if minutes, err := strconv.ParseFloat(value, 64); err == nil {
		if minutes < 0 {
			return "", errors.New("minutes must not be negative")
		}
		return now.Add(time.Duration(minutes * float64(time.Minute))).UTC().Format(time.RFC3339), nil
	}
	if days, ok := strings.CutSuffix(value, "d"); ok {
		if count, err := strconv.ParseFloat(days, 64); err == nil && count >= 0 {
			return now.Add(time.Duration(count * 24 * float64(time.Hour))).UTC().Format(time.RFC3339), nil
		}
	}
	if duration, err := time.ParseDuration(value); err == nil {
		if duration < 0 {
			return "", errors.New("duration must not be negative")
		}
		return now.Add(duration).UTC().Format(time.RFC3339), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UTC().Format(time.RFC3339), nil
		}
	}
	return "", errors.New("use minutes (60), a duration (90m, 12h, 7d), an RFC 3339 time, or 0 to clear")
}

// rawPath validates a caller-supplied API path and returns it with every
// segment percent-encoded. It can only ever name a path below the fixed
// base URL: schemes, hosts, queries, fragments, backslashes, empty or dot
// segments, and encoded separators are all rejected.
func rawPath(base *url.URL, raw string) (string, error) {
	if raw == "" {
		return "", errors.New("path is required")
	}
	if !strings.HasPrefix(raw, "/") {
		return "", errors.New(`path must start with "/" and be relative to ` + base.Path)
	}
	if strings.Contains(raw, "//") {
		return "", errors.New(`path may not contain "//" (no scheme or host)`)
	}
	if strings.ContainsAny(raw, "?#\\") {
		return "", errors.New("path may not contain ?, #, or backslashes; pass query parameters separately")
	}
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return "", errors.New("path may not contain whitespace or control characters")
	}
	if basePath := strings.TrimSuffix(base.Path, "/"); basePath != "" && (raw == basePath || strings.HasPrefix(raw, basePath+"/")) {
		return "", errors.New("path is already relative to " + basePath + "; drop that prefix")
	}
	var encoded strings.Builder
	for _, segment := range strings.Split(raw[1:], "/") {
		decoded, err := url.PathUnescape(segment)
		if err != nil {
			return "", fmt.Errorf("path segment %q has invalid percent-encoding", segment)
		}
		if decoded == "" || decoded == "." || decoded == ".." {
			return "", errors.New(`path may not contain empty, "." or ".." segments`)
		}
		if strings.ContainsAny(decoded, "/\\?#") || strings.IndexFunc(decoded, unicode.IsControl) >= 0 {
			return "", fmt.Errorf("path segment %q decodes to a separator or control character", segment)
		}
		encoded.WriteString("/")
		encoded.WriteString(url.PathEscape(decoded))
	}
	return encoded.String(), nil
}

// queryArgument accepts a JSON object (scalar or array values) or an
// a=1&b=2 string and returns the query parameters it names.
func queryArgument(raw string) (url.Values, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasPrefix(trimmed, "{") {
		values, err := url.ParseQuery(strings.TrimPrefix(trimmed, "?"))
		if err != nil {
			return nil, errors.New("query must be a JSON object or a=1&b=2")
		}
		return values, nil
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || decoder.More() {
		return nil, errors.New("query must be a JSON object")
	}
	values := url.Values{}
	for key, value := range object {
		items, isList := value.([]any)
		if !isList {
			items = []any{value}
		}
		for _, item := range items {
			text, ok := queryScalar(item)
			if !ok {
				return nil, fmt.Errorf("query %s must be a string, number, boolean, or a list of them", key)
			}
			if item != nil {
				values.Add(key, text)
			}
		}
	}
	return values, nil
}

func queryScalar(value any) (string, bool) {
	switch current := value.(type) {
	case nil:
		return "", true
	case string:
		return current, true
	case json.Number:
		return current.String(), true
	case bool:
		return strconv.FormatBool(current), true
	default:
		return "", false
	}
}
