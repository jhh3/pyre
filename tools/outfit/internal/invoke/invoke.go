package invoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
)

const Schema = "outfit.jhh3.dev/result/v1"

type Metadata struct {
	Connection string
	Connector  string
	Mode       string
}

type Options struct {
	All      bool
	MaxPages int
	MaxItems int
}

type Meta struct {
	Status      int    `json:"status,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Pages       int    `json:"pages"`
	Items       int    `json:"items,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
	NextCursor  string `json:"next_cursor,omitempty"`
	Spill       *Spill `json:"spill,omitempty"`
}

// Spill points at the complete result when an agent-facing response was too
// large to safely place inline in a model context. The referenced file contains
// the original Envelope as JSON and is readable only by the current user.
type Spill struct {
	Path   string `json:"path"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Failure struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Hint       string `json:"hint,omitempty"`
	Correction string `json:"correction,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

type Envelope struct {
	Schema     string   `json:"schema"`
	OK         bool     `json:"ok"`
	Connection string   `json:"connection,omitempty"`
	Connector  string   `json:"connector,omitempty"`
	Mode       string   `json:"mode,omitempty"`
	Operation  string   `json:"operation,omitempty"`
	Class      string   `json:"class,omitempty"`
	Data       any      `json:"data,omitempty"`
	Meta       Meta     `json:"meta"`
	Error      *Failure `json:"error,omitempty"`
}

type Runner func(context.Context, map[string]string) (runtime.Result, error)

// Run executes one operation and, when requested, follows the connector's
// declared cursor contract or the standard MCP/GraphQL/REST cursor shapes.
// Pages remain separate so provider payloads are never silently reshaped.
func Run(ctx context.Context, metadata Metadata, operation kit.Operation, arguments map[string]string, options Options, runner Runner) (Envelope, error) {
	operation = kit.NormalizeOperation(operation)
	maxPages, maxItems := limits(operation, options)
	current := clone(arguments)
	pages := make([]any, 0, maxPages)
	envelope := Envelope{Schema: Schema, OK: true, Connection: metadata.Connection, Connector: metadata.Connector, Mode: metadata.Mode, Operation: operation.Name, Class: operation.Class, Meta: Meta{Pages: 0}}
	var last runtime.Result
	for page := 0; page < maxPages; page++ {
		result, err := runner(ctx, current)
		last = result
		if err != nil {
			envelope.OK = false
			envelope.Error = Classify(err, "", "")
			envelope.Meta.Status = result.Status
			envelope.Meta.ContentType = result.ContentType
			if len(pages) > 0 {
				envelope.Data = pages
				envelope.Meta.Truncated = true
			}
			return envelope, err
		}
		data := result.Data
		if data == nil && len(result.Raw) > 0 {
			data = string(result.Raw)
		}
		pages = append(pages, data)
		envelope.Meta.Pages = len(pages)
		envelope.Meta.Items += itemCount(operation, data)
		if !options.All {
			break
		}
		cursorArgument, cursor := nextCursor(operation, data)
		if cursorArgument == "" || cursor == "" {
			break
		}
		envelope.Meta.NextCursor = cursor
		if envelope.Meta.Items >= maxItems {
			envelope.Meta.Truncated = true
			break
		}
		if page+1 >= maxPages {
			envelope.Meta.Truncated = true
			break
		}
		if current[cursorArgument] == cursor {
			envelope.OK = false
			err := errors.New("provider returned the same pagination cursor twice")
			envelope.Error = Classify(err, "pagination_stalled", "Retry without --all or inspect the provider cursor contract.")
			return envelope, err
		}
		current[cursorArgument] = cursor
	}
	if len(pages) == 1 {
		envelope.Data = pages[0]
	} else {
		envelope.Data = pages
	}
	envelope.Meta.Status = last.Status
	envelope.Meta.ContentType = last.ContentType
	if !envelope.Meta.Truncated {
		envelope.Meta.NextCursor = ""
	}
	return envelope, nil
}

func limits(operation kit.Operation, options Options) (int, int) {
	maxPages, maxItems := options.MaxPages, options.MaxItems
	if operation.Pagination != nil {
		if maxPages <= 0 {
			maxPages = operation.Pagination.DefaultMaxPages
		}
		if maxItems <= 0 {
			maxItems = operation.Pagination.DefaultMaxItems
		}
	}
	if maxPages <= 0 {
		maxPages = 10
	}
	if maxItems <= 0 {
		maxItems = 1000
	}
	if maxPages > 100 {
		maxPages = 100
	}
	if maxItems > 100000 {
		maxItems = 100000
	}
	if !options.All {
		maxPages = 1
	}
	return maxPages, maxItems
}

func nextCursor(operation kit.Operation, data any) (string, string) {
	if operation.Pagination != nil {
		spec := operation.Pagination
		if spec.HasMorePath != "" && !truthy(valueAt(data, spec.HasMorePath)) {
			return "", ""
		}
		if value := textValue(valueAt(data, spec.NextCursorPath)); value != "" {
			return spec.CursorArgument, value
		}
		if spec.CursorFromLastItemPath != "" {
			if item := lastItem(valueAt(data, spec.ItemsPath)); item != nil {
				return spec.CursorArgument, textValue(valueAt(item, spec.CursorFromLastItemPath))
			}
		}
		return "", ""
	}
	properties, _ := operation.Input["properties"].(map[string]any)
	cursorArgument := firstPresent(properties, "cursor", "after", "page_token", "pageToken", "starting_after")
	if cursorArgument == "" {
		return "", ""
	}
	for _, path := range []string{"$.next_cursor", "$.nextCursor", "$.next_page_token", "$.nextPageToken", "$.response_metadata.next_cursor", "$._meta.nextCursor", "$.pageInfo.endCursor", "$.data.pageInfo.endCursor"} {
		if value := textValue(valueAt(data, path)); value != "" {
			return cursorArgument, value
		}
	}
	if pageInfo := findMapWithKey(data, "pageInfo"); pageInfo != nil && truthy(pageInfo["hasNextPage"]) {
		return cursorArgument, textValue(pageInfo["endCursor"])
	}
	if cursorArgument == "starting_after" && truthy(valueAt(data, "$.has_more")) {
		if item := lastItem(valueAt(data, "$.data")); item != nil {
			return cursorArgument, textValue(valueAt(item, "$.id"))
		}
	}
	return "", ""
}

func itemCount(operation kit.Operation, data any) int {
	if operation.Pagination != nil && operation.Pagination.ItemsPath != "" {
		return collectionLength(valueAt(data, operation.Pagination.ItemsPath))
	}
	if length := collectionLength(data); length > 0 {
		return length
	}
	for _, path := range []string{"$.data", "$.items", "$.results", "$.nodes", "$.messages", "$.edges"} {
		if length := collectionLength(valueAt(data, path)); length > 0 {
			return length
		}
	}
	return 1
}

func collectionLength(value any) int {
	switch values := value.(type) {
	case []any:
		return len(values)
	case []map[string]any:
		return len(values)
	default:
		return 0
	}
}

func valueAt(value any, path string) any {
	path = strings.TrimPrefix(strings.TrimSpace(path), "$")
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return value
	}
	current := value
	for _, raw := range strings.Split(path, ".") {
		name, index, indexed := splitIndex(raw)
		if name != "" {
			object, ok := current.(map[string]any)
			if !ok {
				return nil
			}
			current = object[name]
		}
		if indexed {
			items, ok := current.([]any)
			if !ok || len(items) == 0 {
				return nil
			}
			if index < 0 {
				index = len(items) + index
			}
			if index < 0 || index >= len(items) {
				return nil
			}
			current = items[index]
		}
	}
	return current
}

func splitIndex(value string) (string, int, bool) {
	open := strings.LastIndex(value, "[")
	if open < 0 || !strings.HasSuffix(value, "]") {
		return value, 0, false
	}
	index, err := strconv.Atoi(strings.TrimSuffix(value[open+1:], "]"))
	if err != nil {
		return value, 0, false
	}
	return value[:open], index, true
}

func lastItem(value any) any {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	return items[len(items)-1]
}

func findMapWithKey(value any, key string) map[string]any {
	switch current := value.(type) {
	case map[string]any:
		if nested, ok := current[key].(map[string]any); ok {
			return nested
		}
		for _, item := range current {
			if found := findMapWithKey(item, key); found != nil {
				return found
			}
		}
	case []any:
		for _, item := range current {
			if found := findMapWithKey(item, key); found != nil {
				return found
			}
		}
	}
	return nil
}

func firstPresent(values map[string]any, names ...string) string {
	for _, name := range names {
		if _, ok := values[name]; ok {
			return name
		}
	}
	return ""
}

func truthy(value any) bool {
	switch current := value.(type) {
	case bool:
		return current
	case string:
		parsed, _ := strconv.ParseBool(current)
		return parsed
	default:
		return false
	}
}

func textValue(value any) string {
	switch current := value.(type) {
	case string:
		return current
	case json.Number:
		return current.String()
	case float64:
		return strconv.FormatFloat(current, 'f', -1, 64)
	case int:
		return strconv.Itoa(current)
	default:
		return ""
	}
}

func clone(value map[string]string) map[string]string {
	result := make(map[string]string, len(value)+1)
	for key, item := range value {
		result[key] = item
	}
	return result
}

func Classify(err error, code, hint string) *Failure {
	if err == nil {
		return nil
	}
	message := strings.TrimSpace(err.Error())
	if code == "" {
		switch {
		case strings.Contains(message, "missing required argument"), strings.Contains(message, "missing path argument"):
			code, hint = "missing_argument", "Run the operation with --help to see required inputs."
		case strings.Contains(message, "unknown operation") || strings.Contains(message, "has no operation"):
			code, hint = "unknown_operation", "Run `outfit describe CONNECTION` or `outfit search WORDS` to choose an available operation."
		case strings.Contains(message, "unknown flag") || strings.Contains(message, "unexpected argument"):
			code, hint = "invalid_arguments", "Run the same operation with --help; schema flags, KEY=value, --arg, and JSON input are supported."
		case strings.Contains(message, "may mutate"), strings.Contains(message, "write operation"):
			code, hint = "write_confirmation_required", "Use `outfit write` or add --write only when the human explicitly requested the mutation."
		case strings.Contains(message, "context deadline exceeded"), strings.Contains(message, "timeout"):
			code, hint = "provider_timeout", "Retry once or narrow the request."
		default:
			code = "operation_failed"
		}
	}
	return &Failure{Code: code, Message: message, Hint: hint, Retryable: code == "provider_timeout"}
}

// FriendlyOperationName is the stable short spelling generated from the
// canonical operation id. The canonical id remains accepted everywhere.
func FriendlyOperationName(connector, operation string) string {
	name := operation
	prefix := strings.ReplaceAll(connector, "-", "_") + "_"
	name = strings.TrimPrefix(name, prefix)
	return strings.ReplaceAll(name, "_", "-")
}

func Marshal(envelope Envelope) ([]byte, error) {
	return json.MarshalIndent(envelope, "", "  ")
}

func ValidateBounds(maxPages, maxItems int) error {
	if maxPages < 0 || maxPages > 100 {
		return fmt.Errorf("max-pages must be between 1 and 100")
	}
	if maxItems < 0 || maxItems > 100000 {
		return fmt.Errorf("max-items must be between 1 and 100000")
	}
	return nil
}
