package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const defaultTimeout = 15 * time.Second

type Result struct {
	Data        any
	Raw         []byte
	ContentType string
	Status      int
}

type DiscoveredTool struct {
	Name, Title, Description, Class string
	InputSchema                     any
}

type Executor struct {
	HTTPClient    *http.Client
	OpenBrowser   func(string) error
	BrowserPrompt func(url, verificationCode string)
}

type CredentialContext struct {
	Method   kit.CredentialMethod
	Bindings []kit.CredentialBinding
	Binary   string
}

type Plan struct {
	Executor           string            `json:"executor"`
	Destination        string            `json:"destination"`
	Method             string            `json:"method,omitempty"`
	Path               string            `json:"path,omitempty"`
	Argv               []string          `json:"argv,omitempty"`
	Stdin              string            `json:"stdin,omitempty"`
	Tool               string            `json:"tool,omitempty"`
	Arguments          map[string]string `json:"arguments"`
	Headers            map[string]string `json:"headers,omitempty"`
	Body               any               `json:"body,omitempty"`
	CredentialFields   []string          `json:"credentialFields,omitempty"`
	CredentialBoundary string            `json:"credentialBoundary"`
	Class              string            `json:"class"`
	Available          bool              `json:"available"`
	Availability       string            `json:"availability"`
	Steps              []Plan            `json:"steps,omitempty"`
}

func (e Executor) Plan(mode kit.Mode, operation kit.Operation, arguments map[string]string) (Plan, error) {
	return e.PlanWithCredentials(mode, operation, arguments, CredentialContext{})
}

func (e Executor) PlanWithCredentials(mode kit.Mode, operation kit.Operation, arguments map[string]string, credentials CredentialContext) (Plan, error) {
	if err := validateArguments(operation, arguments); err != nil {
		return Plan{}, err
	}
	plan := Plan{Executor: mode.Executor.Kind, Arguments: cloneStrings(arguments), Class: operation.Class, CredentialBoundary: "no credential is required"}
	redacted := map[string]string{}
	for _, field := range credentials.Method.Fields {
		if !field.Secret {
			continue
		}
		plan.CredentialFields = append(plan.CredentialFields, field.Key)
		redacted[field.Key] = "<secret:" + field.Key + ">"
	}
	sort.Strings(plan.CredentialFields)
	if len(operation.Compose) > 0 {
		return e.planCompose(mode, operation, arguments, credentials, plan)
	}
	switch mode.Executor.Kind {
	case "http":
		if operation.HTTP == nil {
			return Plan{}, errors.New("operation has no HTTP contract")
		}
		request, err := prepareHTTP(mode, operation, arguments, redacted)
		if err != nil {
			return Plan{}, err
		}
		target := request.target
		plan.Destination, plan.Method, plan.Path = target.Scheme+"://"+target.Host, request.method, target.RequestURI()
		plan.Available, plan.Availability = true, "contract is valid; live reachability is intentionally not tested by planning"
		plan.Headers = map[string]string{"Accept": "application/json"}
		for name, value := range mode.Executor.Headers {
			plan.Headers[name] = value
		}
		if request.hasBody {
			plan.Headers["Content-Type"] = "application/json"
			plan.Body = request.payload
		}
		for name, value := range operation.HTTP.Headers {
			plan.Headers[name] = expand(value, arguments, redacted)
		}
		for name, value := range request.headers {
			plan.Headers[name] = value
		}
		for _, binding := range credentials.Bindings {
			if binding.Target == "http-header" {
				plan.Headers[binding.Name] = expand(binding.Value, arguments, redacted)
			}
		}
		if len(plan.CredentialFields) > 0 {
			plan.CredentialBoundary = "credential remains in the local outfit vault and is injected only into the selected HTTP request"
		}
	case "cli":
		if operation.CLI == nil {
			return Plan{}, errors.New("operation has no CLI contract")
		}
		binary := credentials.Binary
		if binary == "" {
			binary = mode.Executor.Binary
		}
		plan.Destination = binary
		for _, value := range mode.Executor.Args {
			plan.Argv = append(plan.Argv, expand(value, arguments, redacted))
		}
		for _, value := range operation.CLI.Args {
			plan.Argv = append(plan.Argv, expand(value, arguments, redacted))
		}
		optionalArgv, err := appendCLIOptional(plan.Argv, operation.CLI.OptionalArgs, arguments)
		if err != nil {
			return Plan{}, err
		}
		plan.Argv = optionalArgv
		if operation.CLI.Stdin != "" {
			plan.Stdin = expand(operation.CLI.Stdin, arguments, redacted)
		}
		for _, binding := range credentials.Bindings {
			if binding.Target == "stdin" {
				plan.Stdin = expand(binding.Value, arguments, redacted)
			}
		}
		if len(plan.CredentialFields) > 0 {
			var boundaries []string
			if operation.CLI.Stdin != "" {
				boundaries = append(boundaries, "selected child-process stdin")
			}
			for _, binding := range credentials.Bindings {
				switch binding.Target {
				case "process-env":
					boundaries = append(boundaries, "selected child-process environment")
				case "unset-env":
					boundaries = append(boundaries, "conflicting inherited credential environment removed from the selected child process")
				case "temporary-directory":
					boundaries = append(boundaries, "isolated temporary configuration directory removed after the selected process exits")
				case "stdin":
					boundaries = append(boundaries, "selected child-process stdin")
				case "temporary-file":
					boundaries = append(boundaries, "temporary 0600 file removed after the selected process exits")
				}
			}
			if len(boundaries) == 0 {
				boundaries = append(boundaries, "selected child process")
			}
			plan.CredentialBoundary = "credential remains in the local outfit vault and is injected only through " + strings.Join(boundaries, ", ") + "; it never enters argv"
		} else {
			plan.CredentialBoundary = "the native CLI owns its login; outfit passes no vaulted secret in argv or stdin"
		}
		if path, err := exec.LookPath(binary); err == nil {
			plan.Available, plan.Availability = true, "resolved on PATH as "+path
		} else {
			plan.Availability = "binary is not currently available on PATH"
		}
	case "mcp":
		plan.Tool = operation.MCPTool
		if mode.Executor.Endpoint != "" {
			plan.Destination = mode.Executor.Endpoint
			plan.Available, plan.Availability = true, "transport is configured; live reachability is intentionally not tested by planning"
		} else {
			plan.Destination = mode.Executor.Command
			for _, value := range mode.Executor.Args {
				plan.Argv = append(plan.Argv, expand(value, arguments, redacted))
			}
			if path, err := exec.LookPath(mode.Executor.Command); err == nil {
				plan.Available, plan.Availability = true, "resolved on PATH as "+path
			} else {
				plan.Availability = "MCP command is not currently available on PATH"
			}
		}
		if len(plan.CredentialFields) > 0 {
			plan.CredentialBoundary = "credential remains in the local outfit vault and is attached only to the selected MCP transport"
		}
	default:
		return Plan{}, fmt.Errorf("unsupported executor %q", mode.Executor.Kind)
	}
	return plan, nil
}

func (e Executor) Run(ctx context.Context, mode kit.Mode, operation kit.Operation, arguments, secret map[string]string) (Result, error) {
	return e.RunWithCredentials(ctx, mode, operation, arguments, secret, CredentialContext{})
}

func (e Executor) RunWithCredentials(ctx context.Context, mode kit.Mode, operation kit.Operation, arguments, secret map[string]string, credentials CredentialContext) (Result, error) {
	return e.RunWithCredentialsIO(ctx, mode, operation, arguments, secret, credentials, nil, nil)
}

func (e Executor) RunWithCredentialsIO(ctx context.Context, mode kit.Mode, operation kit.Operation, arguments, secret map[string]string, credentials CredentialContext, stdout, stderr io.Writer) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("context is required")
	}
	if _, err := e.PlanWithCredentials(mode, operation, arguments, credentials); err != nil {
		return Result{}, err
	}
	if len(operation.Compose) > 0 {
		return e.runCompose(ctx, mode, operation, arguments, secret, credentials)
	}
	switch mode.Executor.Kind {
	case "http":
		return e.runHTTP(ctx, mode, operation, arguments, secret, credentials)
	case "cli":
		return e.runCLI(ctx, mode, operation, arguments, secret, credentials, stdout, stderr)
	case "mcp":
		return e.runMCP(ctx, mode, operation, arguments, secret, credentials)
	default:
		return Result{}, fmt.Errorf("unsupported executor %q", mode.Executor.Kind)
	}
}

// RunCommandTemplate executes a connector-declared setup, verification, or
// logout command through the same exact managed binary boundary as operations.
func (e Executor) RunCommandTemplate(ctx context.Context, template kit.CommandTemplate, fields map[string]string, credentials CredentialContext) (Result, error) {
	result, err := e.runCommandTemplateOnce(ctx, template, fields, credentials)
	if err != nil || template.BrowserContinuation == nil {
		return result, err
	}
	return e.completeBrowserContinuation(ctx, template, fields, credentials, result)
}

func (e Executor) runCommandTemplateOnce(ctx context.Context, template kit.CommandTemplate, fields map[string]string, credentials CredentialContext) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("context is required")
	}
	binary := template.Program
	if credentials.Binary != "" {
		binary = credentials.Binary
	}
	if binary == "" {
		return Result{}, errors.New("command template has no resolved binary")
	}
	args := make([]string, 0, len(template.Args))
	for _, argument := range template.Args {
		args = append(args, expand(argument, fields, fields))
	}
	command, cleanup, err := boundCommand(ctx, binary, args, fields, fields, credentials.Bindings, expand(template.Stdin, fields, fields))
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	output, err := command.Output()
	if exit := new(exec.ExitError); errors.As(err, &exit) {
		detail := commandFailureDetail(output, exit.Stderr, fields, credentials.Method)
		result := Result{Data: detail, Raw: []byte(detail), ContentType: "text/plain", Status: exit.ExitCode()}
		return result, fmt.Errorf("%s failed: %s", filepath.Base(binary), detail)
	}
	if err != nil {
		return Result{}, err
	}
	return decodeResult(output, "application/json", 200), nil
}

func (e Executor) completeBrowserContinuation(ctx context.Context, template kit.CommandTemplate, fields map[string]string, credentials CredentialContext, setup Result) (Result, error) {
	flow := template.BrowserContinuation
	payload, ok := setup.Data.(map[string]any)
	if !ok {
		return Result{}, errors.New("native login did not return a browser continuation object")
	}
	browserURL := textField(payload, flow.URLField)
	nextStep := textField(payload, flow.NextStepField)
	code := textField(payload, flow.VerificationCodeField)
	if browserURL == "" || nextStep == "" {
		return Result{}, fmt.Errorf("native login omitted browser continuation fields %s or %s", flow.URLField, flow.NextStepField)
	}
	parsedURL, err := url.Parse(browserURL)
	if err != nil || parsedURL.Scheme != "https" || parsedURL.Host == "" || parsedURL.User != nil {
		return Result{}, errors.New("native login returned an unsafe browser URL")
	}
	arguments, err := parseDirectCommand(nextStep)
	if err != nil || len(arguments) < 2 {
		return Result{}, errors.New("native login returned an invalid completion command")
	}
	binary := template.Program
	if credentials.Binary != "" {
		binary = credentials.Binary
	}
	if filepath.Base(arguments[0]) != filepath.Base(binary) && filepath.Base(arguments[0]) != filepath.Base(template.Program) {
		return Result{}, errors.New("native login completion command changed the pinned executable")
	}
	opener := e.OpenBrowser
	if opener == nil {
		opener = openBrowser
	}
	if e.BrowserPrompt != nil {
		e.BrowserPrompt(browserURL, code)
	}
	// A remote/headless shell cannot launch the person's browser. The prompt is
	// authoritative in that case, so a failed best-effort opener must not cancel
	// the CLI's waiting completion command.
	_ = opener(browserURL)
	timeout := time.Duration(flow.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	completionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	completion := kit.CommandTemplate{Program: template.Program, Args: arguments[1:]}
	result, err := e.runCommandTemplateOnce(completionCtx, completion, fields, credentials)
	if err != nil {
		return result, fmt.Errorf("complete browser login (verification code %s; reopen %s if needed): %w", code, browserURL, err)
	}
	return result, nil
}

func commandFailureDetail(stdout, stderr []byte, fields map[string]string, method kit.CredentialMethod) string {
	parts := []string{}
	for _, value := range [][]byte{stderr, stdout} {
		if text := strings.TrimSpace(string(value)); text != "" {
			parts = append(parts, text)
		}
	}
	detail := strings.Join(parts, "\n")
	for _, field := range method.Fields {
		if field.Secret && fields[field.Key] != "" {
			detail = strings.ReplaceAll(detail, fields[field.Key], "<redacted>")
		}
	}
	if len(detail) > 8192 {
		detail = detail[:8192] + "…"
	}
	if detail == "" {
		return "command exited unsuccessfully without an error message"
	}
	return detail
}

func textField(payload map[string]any, name string) string {
	value, _ := payload[name].(string)
	return strings.TrimSpace(value)
}

func parseDirectCommand(value string) ([]string, error) {
	var arguments []string
	var current strings.Builder
	var quote rune
	escaped, started := false, false
	flush := func() {
		if started {
			arguments = append(arguments, current.String())
			current.Reset()
			started = false
		}
	}
	for _, char := range value {
		if char == '\x00' || char == '\n' || char == '\r' {
			return nil, errors.New("completion command contains a forbidden control character")
		}
		if escaped {
			current.WriteRune(char)
			escaped, started = false, true
			continue
		}
		if char == '\\' && quote != '\'' {
			escaped, started = true, true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			started = true
			continue
		}
		switch char {
		case '\'', '"':
			quote, started = char, true
		case ' ', '\t':
			flush()
		default:
			current.WriteRune(char)
			started = true
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("completion command has incomplete quoting")
	}
	flush()
	return arguments, nil
}

func openBrowser(target string) error {
	var command *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		command = exec.Command("open", target)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		command = exec.Command("xdg-open", target)
	}
	return command.Run()
}

func (e Executor) runHTTP(ctx context.Context, mode kit.Mode, operation kit.Operation, arguments, secret map[string]string, credentials CredentialContext) (Result, error) {
	if operation.HTTP == nil {
		return Result{}, errors.New("operation has no HTTP contract")
	}
	prepared, err := prepareHTTP(mode, operation, arguments, secret)
	if err != nil {
		return Result{}, err
	}
	target, hasBody := prepared.target, prepared.hasBody
	var encoded []byte
	if hasBody {
		if encoded, err = json.Marshal(prepared.payload); err != nil {
			return Result{}, err
		}
	}
	client := e.HTTPClient
	if client == nil {
		client = safeHTTPClient(mode.Executor.PrivateNet)
	}
	for attempt := 0; ; attempt++ {
		var body io.Reader
		if hasBody {
			body = bytes.NewReader(encoded)
		}
		request, err := http.NewRequestWithContext(ctx, prepared.method, target.String(), body)
		if err != nil {
			return Result{}, err
		}
		request.Header.Set("Accept", "application/json")
		if hasBody {
			request.Header.Set("Content-Type", "application/json")
		}
		for name, value := range mode.Executor.Headers {
			request.Header.Set(name, value)
		}
		for name, value := range operation.HTTP.Headers {
			request.Header.Set(name, expand(value, arguments, secret))
		}
		for name, value := range prepared.headers {
			request.Header.Set(name, value)
		}
		query := request.URL.Query()
		for _, binding := range credentials.Bindings {
			switch binding.Target {
			case "http-header":
				request.Header.Set(binding.Name, expand(binding.Value, arguments, secret))
			case "http-query":
				query.Set(binding.Name, expand(binding.Value, arguments, secret))
			}
		}
		request.URL.RawQuery = query.Encode()
		response, err := client.Do(request)
		if err != nil {
			return Result{}, err
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		response.Body.Close()
		if err != nil {
			return Result{}, err
		}
		result := decodeResult(raw, response.Header.Get("Content-Type"), response.StatusCode)
		if response.StatusCode == http.StatusTooManyRequests {
			delay := rateLimitDelay(response.Header, result.Data)
			if attempt < maxRateLimitRetries && delay <= maxRateLimitWait {
				if err := sleepContext(ctx, delay); err != nil {
					return result, err
				}
				continue
			}
			return result, fmt.Errorf("upstream returned %s (rate limited; retry after %s)", response.Status, delay.Round(time.Millisecond))
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			if operation.HTTP != nil {
				if payload, ok := result.Data.(map[string]any); ok {
					detail := providerErrorText(payload[operation.HTTP.ErrorField])
					if detail == "" {
						detail = providerErrorText(payload["message"])
					}
					if detail != "" {
						return result, fmt.Errorf("upstream returned %s: %s", response.Status, detail)
					}
				}
			}
			return result, fmt.Errorf("upstream returned %s", response.Status)
		}
		if err := providerEnvelopeError(operation, result.Data); err != nil {
			return result, err
		}
		return result, nil
	}
}

// A 429 means the provider rejected the request without executing it, so a
// bounded retry after the provider-stated delay is safe for writes as well.
const (
	maxRateLimitRetries = 3
	maxRateLimitWait    = 30 * time.Second
)

// rateLimitDelay reads the provider's requested wait: a JSON retry_after
// (Discord: float seconds), the standard Retry-After header (seconds or
// HTTP date), or X-RateLimit-Reset-After. Unknown shapes wait one second.
func rateLimitDelay(header http.Header, data any) time.Duration {
	if payload, ok := data.(map[string]any); ok {
		if seconds, ok := numberValue(payload["retry_after"]); ok && seconds >= 0 {
			return time.Duration(seconds * float64(time.Second))
		}
	}
	for _, name := range []string{"Retry-After", "X-RateLimit-Reset-After"} {
		value := strings.TrimSpace(header.Get(name))
		if value == "" {
			continue
		}
		if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
			return time.Duration(seconds * float64(time.Second))
		}
		if when, err := http.ParseTime(value); err == nil {
			if wait := time.Until(when); wait > 0 {
				return wait
			}
			return 0
		}
	}
	return time.Second
}

func numberValue(value any) (float64, bool) {
	switch current := value.(type) {
	case float64:
		return current, true
	case json.Number:
		parsed, err := current.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(current, 64)
		return parsed, err == nil
	}
	return 0, false
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (e Executor) planCompose(mode kit.Mode, operation kit.Operation, arguments map[string]string, credentials CredentialContext, plan Plan) (Plan, error) {
	plan.Executor = "compose"
	plan.Available, plan.Availability = true, "every step is available"
	for _, step := range operation.Compose {
		target, ok := FindOperation(mode, step.Operation)
		if !ok {
			return Plan{}, fmt.Errorf("compose step cites unknown operation %s", step.Operation)
		}
		stepPlan, err := e.PlanWithCredentials(mode, target, composeArguments(step, target, arguments), credentials)
		if err != nil {
			return Plan{}, fmt.Errorf("compose step %s: %w", step.As, err)
		}
		if plan.Destination == "" {
			plan.Destination = stepPlan.Destination
		}
		if !stepPlan.Available {
			plan.Available, plan.Availability = false, "step "+step.As+": "+stepPlan.Availability
		}
		plan.CredentialBoundary = stepPlan.CredentialBoundary
		plan.Steps = append(plan.Steps, stepPlan)
	}
	return plan, nil
}

// runCompose executes a composite read's steps sequentially and returns one
// object keyed by step name. Any failing step fails the whole read so a
// monitor never mistakes a partial snapshot for "nothing changed".
func (e Executor) runCompose(ctx context.Context, mode kit.Mode, operation kit.Operation, arguments, secret map[string]string, credentials CredentialContext) (Result, error) {
	combined := map[string]any{}
	for _, step := range operation.Compose {
		target, ok := FindOperation(mode, step.Operation)
		if !ok {
			return Result{}, fmt.Errorf("compose step cites unknown operation %s", step.Operation)
		}
		result, err := e.RunWithCredentials(ctx, mode, target, composeArguments(step, target, arguments), secret, credentials)
		if err != nil {
			return result, fmt.Errorf("compose step %s (%s): %w", step.As, step.Operation, err)
		}
		combined[step.As] = projectFields(result.Data, step.ItemsPath, step.Fields)
	}
	raw, err := json.Marshal(combined)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: combined, Raw: raw, ContentType: "application/json", Status: http.StatusOK}, nil
}

func composeArguments(step kit.ComposeStep, target kit.Operation, arguments map[string]string) map[string]string {
	result := map[string]string{}
	properties, _ := target.Input["properties"].(map[string]any)
	for name := range properties {
		if _, mapped := step.Arguments[name]; mapped {
			continue
		}
		if value, ok := arguments[name]; ok {
			result[name] = value
		}
	}
	for name, source := range step.Arguments {
		if strings.HasPrefix(source, "=") {
			result[name] = strings.TrimPrefix(source, "=")
		} else if value, ok := arguments[source]; ok {
			result[name] = value
		}
	}
	return result
}

func projectFields(data any, itemsPath string, fields []string) any {
	if len(fields) == 0 {
		return data
	}
	items := data
	path := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(itemsPath), "$"), ".")
	if path != "" {
		for _, key := range strings.Split(path, ".") {
			object, ok := items.(map[string]any)
			if !ok {
				return data
			}
			items = object[key]
		}
	}
	list, ok := items.([]any)
	if !ok {
		return data
	}
	projected := make([]any, 0, len(list))
	for _, item := range list {
		object, ok := item.(map[string]any)
		if !ok {
			projected = append(projected, item)
			continue
		}
		compact := map[string]any{}
		for _, field := range fields {
			if value, ok := object[field]; ok {
				compact[field] = value
			}
		}
		projected = append(projected, compact)
	}
	return projected
}

func providerEnvelopeError(operation kit.Operation, data any) error {
	if operation.HTTP == nil || (operation.HTTP.SuccessField == "" && operation.HTTP.ErrorField == "") {
		return nil
	}
	payload, ok := data.(map[string]any)
	if !ok {
		return errors.New("upstream returned a malformed provider response")
	}
	if field := operation.HTTP.SuccessField; field != "" {
		success, exists := payload[field]
		if !exists {
			return fmt.Errorf("upstream response omitted %s", field)
		}
		if value, ok := success.(bool); !ok || !value {
			return providerReportedError(payload, operation.HTTP.ErrorField)
		}
	}
	if field := operation.HTTP.ErrorField; field != "" && envelopeValuePresent(payload[field]) {
		return providerReportedError(payload, field)
	}
	return nil
}

func providerReportedError(payload map[string]any, field string) error {
	if message := providerErrorText(payload[field]); message != "" {
		return fmt.Errorf("upstream reported %s", message)
	}
	if field != "" {
		return fmt.Errorf("upstream reported an error in %s", field)
	}
	return errors.New("upstream reported an unsuccessful response")
}

// providerErrorText renders an error field as plain text: a string as is, or
// the "message" of each object in a list (GraphQL's errors array).
func providerErrorText(value any) string {
	switch current := value.(type) {
	case string:
		return strings.TrimSpace(current)
	case map[string]any:
		if message, ok := current["message"].(string); ok {
			return strings.TrimSpace(message)
		}
	case []any:
		messages := make([]string, 0, len(current))
		for _, item := range current {
			if message := providerErrorText(item); message != "" {
				messages = append(messages, message)
			}
		}
		return strings.Join(messages, "; ")
	}
	return ""
}

func envelopeValuePresent(value any) bool {
	switch current := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(current) != ""
	case []any:
		return len(current) > 0
	case map[string]any:
		return len(current) > 0
	default:
		return true
	}
}

func httpPayload(operation kit.Operation, arguments, secret map[string]string) map[string]any {
	payload := map[string]any{}
	coder := newArgumentCoder(operation)
	for name, value := range operation.HTTP.BodyValues {
		if expanded, present := expandAnyPresent(value, arguments, secret, coder); present {
			payload[name] = expanded
		}
	}
	for _, name := range operation.HTTP.Body {
		if value, ok := arguments[name]; ok {
			providerName := operation.HTTP.BodyNames[name]
			if providerName == "" {
				providerName = name
			}
			payload[providerName] = coder.value(name, value)
		}
	}
	return payload
}

func expandAnyPresent(value any, arguments, secret map[string]string, coder argumentCoder) (any, bool) {
	switch current := value.(type) {
	case string:
		if match := placeholder.FindStringSubmatch(current); len(match) > 0 && match[0] == current {
			if match[1] == "arg" {
				raw, ok := arguments[match[2]]
				if !ok {
					return nil, false
				}
				return coder.value(match[2], raw), true
			}
			raw, ok := secret[match[2]]
			if !ok {
				return nil, false
			}
			return raw, true
		}
		return expand(current, arguments, secret), true
	case map[string]any:
		result := make(map[string]any, len(current))
		for key, item := range current {
			if expanded, present := expandAnyPresent(item, arguments, secret, coder); present {
				result[key] = expanded
			}
		}
		return result, true
	case []any:
		result := make([]any, 0, len(current))
		for _, item := range current {
			if expanded, present := expandAnyPresent(item, arguments, secret, coder); present {
				result = append(result, expanded)
			}
		}
		return result, true
	default:
		return value, true
	}
}

func httpTarget(mode kit.Mode, operation kit.Operation, arguments map[string]string) (*url.URL, error) {
	base, err := url.Parse(mode.Executor.BaseURL)
	if err != nil {
		return nil, err
	}
	path, err := expandPath(operation.HTTP.Path, arguments)
	if err != nil {
		return nil, err
	}
	var target *url.URL
	if operation.HTTP.PathArgument != "" {
		suffix, err := rawPath(base, arguments[operation.HTTP.PathArgument])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", operation.HTTP.PathArgument, err)
		}
		basePath := strings.TrimSuffix(base.EscapedPath(), "/")
		prefix := strings.Trim(path, "/")
		if prefix != "" {
			prefix = "/" + prefix
		}
		if target, err = url.Parse(base.Scheme + "://" + base.Host + basePath + prefix + suffix); err != nil {
			return nil, err
		}
	} else {
		relative, err := url.Parse(strings.TrimPrefix(path, "/"))
		if err != nil {
			return nil, err
		}
		target = base.ResolveReference(relative)
	}
	if target.Scheme != base.Scheme || target.Host != base.Host || target.User != nil || !strings.HasPrefix(target.Path, base.Path) {
		return nil, errors.New("operation escaped its fixed HTTP origin")
	}
	query := target.Query()
	for name, value := range operation.HTTP.QueryValues {
		query.Set(name, value)
	}
	for _, name := range operation.HTTP.Query {
		if value, ok := arguments[name]; ok {
			providerName := operation.HTTP.QueryNames[name]
			if providerName == "" {
				providerName = name
			}
			query.Set(providerName, value)
		}
	}
	if name := operation.HTTP.QueryArgument; name != "" {
		if raw, ok := arguments[name]; ok && strings.TrimSpace(raw) != "" {
			extra, err := queryArgument(raw)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			for key, values := range extra {
				query.Del(key)
				for _, value := range values {
					query.Add(key, value)
				}
			}
		}
	}
	target.RawQuery = query.Encode()
	return target, nil
}

func (e Executor) runCLI(ctx context.Context, mode kit.Mode, operation kit.Operation, arguments, secret map[string]string, credentials CredentialContext, stdout, stderr io.Writer) (Result, error) {
	if operation.CLI == nil {
		return Result{}, errors.New("operation has no CLI contract")
	}
	binary := credentials.Binary
	if binary == "" {
		binary = mode.Executor.Binary
	}
	if binary == "" {
		return Result{}, errors.New("managed CLI binary is not resolved")
	}
	args := make([]string, 0, len(mode.Executor.Args)+len(operation.CLI.Args))
	for _, argument := range mode.Executor.Args {
		args = append(args, expand(argument, arguments, secret))
	}
	for _, argument := range operation.CLI.Args {
		args = append(args, expand(argument, arguments, secret))
	}
	args, err := appendCLIOptional(args, operation.CLI.OptionalArgs, arguments)
	if err != nil {
		return Result{}, err
	}
	command, cleanup, err := boundCommand(ctx, binary, args, arguments, secret, credentials.Bindings, operation.CLI.Stdin)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	if operation.Interaction == "stream" {
		command.Stdout = stdout
		command.Stderr = stderr
		err := command.Run()
		if ctx.Err() != nil {
			return Result{Status: 200}, ctx.Err()
		}
		if err != nil {
			return Result{}, fmt.Errorf("%s stream failed: %w", filepath.Base(binary), err)
		}
		return Result{Status: 200}, nil
	}
	output, err := command.Output()
	if exit := new(exec.ExitError); errors.As(err, &exit) {
		return Result{Raw: exit.Stderr}, fmt.Errorf("%s failed: %s", filepath.Base(binary), strings.TrimSpace(string(exit.Stderr)))
	}
	if err != nil {
		return Result{}, err
	}
	return decodeResult(output, "application/json", 200), nil
}

func appendCLIOptional(args []string, optionalArgs []kit.CLIOptionalArg, arguments map[string]string) ([]string, error) {
	for _, optional := range optionalArgs {
		value, present := arguments[optional.Argument]
		if !present || value == "" {
			continue
		}
		if optional.Boolean {
			enabled, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("argument %s must be true or false", optional.Argument)
			}
			if enabled && optional.Flag != "" {
				args = append(args, optional.Flag)
			}
			continue
		}
		if optional.Flag != "" {
			args = append(args, optional.Flag)
		}
		args = append(args, value)
	}
	return args, nil
}

func (e Executor) runMCP(ctx context.Context, mode kit.Mode, operation kit.Operation, arguments, secret map[string]string, credentials CredentialContext) (Result, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "outfit", Version: "dev"}, nil)
	transport, cleanup, err := mcpTransport(ctx, mode, arguments, secret, credentials)
	if err != nil {
		return Result{}, err
	}
	defer cleanup()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return Result{}, err
	}
	defer session.Close()
	structuredArguments := map[string]any{}
	for key, value := range arguments {
		structuredArguments[key] = coerceJSON(value)
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: operation.MCPTool, Arguments: structuredArguments})
	if err != nil {
		return Result{}, err
	}
	encoded, _ := json.Marshal(result)
	if result.IsError {
		return Result{Data: result, Raw: encoded, ContentType: "application/json", Status: 200}, errors.New("MCP tool returned an error")
	}
	return Result{Data: result, Raw: encoded, ContentType: "application/json", Status: 200}, nil
}

func DiscoverMCP(ctx context.Context, mode kit.Mode, secret map[string]string) ([]DiscoveredTool, error) {
	return DiscoverMCPWithCredentials(ctx, mode, secret, CredentialContext{})
}

func DiscoverMCPWithCredentials(ctx context.Context, mode kit.Mode, secret map[string]string, credentials CredentialContext) ([]DiscoveredTool, error) {
	if mode.Executor.Kind != "mcp" {
		return nil, errors.New("mode is not MCP")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "outfit", Version: "dev"}, nil)
	transport, cleanup, err := mcpTransport(ctx, mode, nil, secret, credentials)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()
	var result []DiscoveredTool
	cursor := ""
	for page := 0; page < 20; page++ {
		listed, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, tool := range listed.Tools {
			class := "destructive"
			if tool.Annotations != nil && tool.Annotations.ReadOnlyHint {
				class = "read"
			} else if tool.Annotations != nil && tool.Annotations.DestructiveHint != nil && !*tool.Annotations.DestructiveHint {
				class = "write"
			}
			result = append(result, DiscoveredTool{Name: tool.Name, Title: tool.Title, Description: tool.Description, Class: class, InputSchema: tool.InputSchema})
		}
		if listed.NextCursor == "" {
			return result, nil
		}
		cursor = listed.NextCursor
	}
	return nil, errors.New("MCP tool discovery exceeded 20 pages")
}

func mcpTransport(ctx context.Context, mode kit.Mode, arguments, secret map[string]string, credentials CredentialContext) (mcp.Transport, func(), error) {
	if mode.Executor.Command != "" {
		commandName := mode.Executor.Command
		if credentials.Binary != "" {
			commandName = credentials.Binary
		}
		args := make([]string, 0, len(mode.Executor.Args))
		for _, arg := range mode.Executor.Args {
			args = append(args, expand(arg, arguments, secret))
		}
		command, cleanup, err := boundCommand(ctx, commandName, args, arguments, secret, credentials.Bindings, "")
		if err != nil {
			return nil, func() {}, err
		}
		return &mcp.CommandTransport{Command: command}, cleanup, nil
	}
	endpoint, err := url.Parse(mode.Executor.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, func() {}, errors.New("MCP endpoint must be a fixed HTTPS URL")
	}
	baseClient := safeHTTPClient(mode.Executor.PrivateNet)
	httpClient := &http.Client{Timeout: defaultTimeout, CheckRedirect: baseClient.CheckRedirect, Transport: authTransport{base: baseClient.Transport, bindings: credentials.Bindings, secret: secret}}
	return &mcp.StreamableClientTransport{Endpoint: mode.Executor.Endpoint, HTTPClient: httpClient, DisableStandaloneSSE: true, MaxRetries: 1}, func() {}, nil
}

func boundCommand(ctx context.Context, binary string, args []string, arguments, secret map[string]string, bindings []kit.CredentialBinding, operationStdin string) (*exec.Cmd, func(), error) {
	command := exec.CommandContext(ctx, binary, args...)
	env := append([]string{}, os.Environ()...)
	stdin := operationStdin
	var directories []string
	cleanup := func() {
		for _, directory := range directories {
			_ = os.RemoveAll(directory)
		}
	}
	for _, binding := range bindings {
		switch binding.Target {
		case "process-env":
			env = replaceEnv(env, binding.Name, expand(binding.Value, arguments, secret))
		case "unset-env":
			env = removeEnv(env, binding.Name)
		case "stdin":
			stdin = binding.Value
		case "temporary-file":
			directory, err := os.MkdirTemp("", "outfit-credential-")
			if err != nil {
				cleanup()
				return nil, func() {}, err
			}
			directories = append(directories, directory)
			if err := os.Chmod(directory, 0o700); err != nil {
				cleanup()
				return nil, func() {}, err
			}
			name := filepath.Base(expand(binding.Path, arguments, secret))
			if name == "." || name == "" {
				name = "credential"
			}
			path := filepath.Join(directory, name)
			mode := os.FileMode(binding.Mode)
			if mode == 0 {
				mode = 0o600
			}
			if err := os.WriteFile(path, []byte(expand(binding.Value, arguments, secret)), mode); err != nil {
				cleanup()
				return nil, func() {}, err
			}
			if binding.Name != "" {
				env = replaceEnv(env, binding.Name, path)
			}
		case "temporary-directory":
			directory, err := os.MkdirTemp("", "outfit-config-")
			if err != nil {
				cleanup()
				return nil, func() {}, err
			}
			directories = append(directories, directory)
			if err := os.Chmod(directory, 0o700); err != nil {
				cleanup()
				return nil, func() {}, err
			}
			env = replaceEnv(env, binding.Name, directory)
		}
	}
	command.Env = env
	if stdin != "" {
		command.Stdin = strings.NewReader(expand(stdin, arguments, secret))
	}
	return command, cleanup, nil
}

func replaceEnv(values []string, name, value string) []string {
	prefix := name + "="
	result := values[:0]
	for _, item := range values {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func removeEnv(values []string, name string) []string {
	prefix := name + "="
	result := values[:0]
	for _, item := range values {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return result
}

func safeHTTPClient(allowPrivate bool) *http.Client {
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		var failures []string
		for _, resolved := range ips {
			if !allowPrivate && unsafeIP(resolved.IP) {
				failures = append(failures, resolved.IP.String()+" is private")
				continue
			}
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
			if err == nil {
				return connection, nil
			}
			failures = append(failures, err.Error())
		}
		return nil, fmt.Errorf("no safe address for %s: %s", host, strings.Join(failures, "; "))
	}}
	client := &http.Client{Timeout: defaultTimeout, Transport: transport}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		original := via[0].URL
		if request.URL.Scheme != original.Scheme || request.URL.Host != original.Host {
			return errors.New("redirect escaped fixed origin")
		}
		return nil
	}
	return client
}

// SafeHTTPClient returns the same fixed-origin, DNS-rebinding-resistant client used by connector execution.
// Importers use it for explicit live contract discovery without gaining a weaker network path.
func SafeHTTPClient(allowPrivate bool) *http.Client { return safeHTTPClient(allowPrivate) }

func unsafeIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()
}

type authTransport struct {
	base     http.RoundTripper
	bindings []kit.CredentialBinding
	secret   map[string]string
}

func (transport authTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	for _, binding := range transport.bindings {
		if binding.Target == "mcp-header" {
			clone.Header.Set(binding.Name, expand(binding.Value, nil, transport.secret))
		}
	}
	return transport.base.RoundTrip(clone)
}

var placeholder = regexp.MustCompile(`\{\{(arg|secret|setting)\.([A-Za-z0-9_]+)\}\}`)

func expand(value string, arguments map[string]string, secret map[string]string) string {
	return placeholder.ReplaceAllStringFunc(value, func(match string) string {
		parts := placeholder.FindStringSubmatch(match)
		if parts[1] == "arg" || parts[1] == "setting" {
			return arguments[parts[2]]
		}
		return secret[parts[2]]
	})
}

// coerceJSON decodes JSON-looking arguments. Numbers stay json.Number so
// large integers such as 64-bit snowflake IDs keep every digit.
func coerceJSON(value string) any {
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.UseNumber()
	if decoder.Decode(&decoded) != nil {
		return value
	}
	if _, err := decoder.Token(); err != io.EOF {
		return value
	}
	return decoded
}

func validateArguments(operation kit.Operation, arguments map[string]string) error {
	var required []string
	switch values := operation.Input["required"].(type) {
	case []string:
		required = values
	case []any:
		for _, value := range values {
			if text, ok := value.(string); ok {
				required = append(required, text)
			}
		}
	}
	for _, name := range required {
		if strings.TrimSpace(arguments[name]) == "" {
			return fmt.Errorf("missing required argument %s", name)
		}
	}
	if operation.Input["additionalProperties"] == false {
		properties, _ := operation.Input["properties"].(map[string]any)
		for name := range arguments {
			if _, ok := properties[name]; !ok {
				return fmt.Errorf("undeclared argument %s", name)
			}
		}
	}
	return nil
}

func cloneStrings(value map[string]string) map[string]string {
	result := make(map[string]string, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func expandPath(template string, arguments map[string]string) (string, error) {
	segments := strings.Split(template, "/")
	for index, segment := range segments {
		name := ""
		if strings.HasPrefix(segment, ":") {
			name = strings.TrimPrefix(segment, ":")
		} else if match := placeholder.FindStringSubmatch(segment); len(match) == 3 && match[0] == segment && (match[1] == "arg" || match[1] == "setting") {
			name = match[2]
		}
		if name == "" {
			continue
		}
		value, ok := arguments[name]
		if !ok || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("missing path argument %s", name)
		}
		segments[index] = url.PathEscape(value)
	}
	return strings.Join(segments, "/"), nil
}

func decodeResult(raw []byte, contentType string, status int) Result {
	result := Result{Raw: raw, ContentType: contentType, Status: status}
	if len(raw) > 0 && (strings.Contains(contentType, "json") || json.Valid(raw)) {
		_ = json.Unmarshal(raw, &result.Data)
	} else {
		result.Data = string(raw)
	}
	return result
}

func FindMode(pkg kit.Package, modeID string) (kit.Mode, bool) {
	for _, mode := range pkg.Modes {
		if mode.ID == modeID {
			return mode, true
		}
	}
	return kit.Mode{}, false
}

func FindOperation(mode kit.Mode, name string) (kit.Operation, bool) {
	for _, operation := range mode.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return kit.Operation{}, false
}
