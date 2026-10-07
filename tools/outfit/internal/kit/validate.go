package kit

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var (
	keyPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}[a-z0-9]$`)
	fieldPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,63}$`)
	namePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
	idPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

var segmentKinds = map[string]bool{
	"slug": true, "integer": true, "sha": true, "hex": true,
	"linear-identifier": true, "slack-channel": true, "slack-ts": true,
	"opaque-token": true, "path": true,
	"notion-page":         true,
	"google-calendar-eid": true,
}

func Validate(pkg Package) []string {
	var issues []string
	if pkg.APIVersion != APIVersion {
		issues = append(issues, fmt.Sprintf("apiVersion must be %s", APIVersion))
	}
	if !keyPattern.MatchString(pkg.Key) {
		issues = append(issues, "key must be 3-64 lowercase kebab-case characters")
	}
	if strings.TrimSpace(pkg.Display) == "" {
		issues = append(issues, "display is required")
	}
	if strings.TrimSpace(pkg.Version) == "" {
		issues = append(issues, "version is required")
	}
	if len(pkg.Modes) == 0 {
		issues = append(issues, "at least one mode is required")
	}
	methodByID := map[string]CredentialMethod{}
	recommendedMethods := 0
	for _, method := range pkg.CredentialMethods {
		if !idPattern.MatchString(method.ID) {
			issues = append(issues, fmt.Sprintf("credential method %q needs a lowercase id", method.ID))
		}
		if _, exists := methodByID[method.ID]; exists {
			issues = append(issues, fmt.Sprintf("credential method %s is defined more than once", method.ID))
		}
		methodByID[method.ID] = method
		if method.Recommended {
			recommendedMethods++
		}
		issues = append(issues, validateCredentialMethod(method)...)
	}
	if len(methodByID) == 0 {
		issues = append(issues, "at least one credential method is required")
	} else if recommendedMethods != 1 {
		issues = append(issues, fmt.Sprintf("exactly one credential method must be recommended (got %d)", recommendedMethods))
	}
	toolByID := map[string]ToolRequirement{}
	for _, tool := range pkg.Tools {
		if !idPattern.MatchString(tool.ID) || strings.TrimSpace(tool.Binary) == "" || strings.TrimSpace(tool.Version) == "" {
			issues = append(issues, "managed tools need a lowercase id, binary, and version")
		}
		if _, exists := toolByID[tool.ID]; exists {
			issues = append(issues, fmt.Sprintf("managed tool %s is defined more than once", tool.ID))
		}
		toolByID[tool.ID] = tool
		issues = append(issues, validateTool(tool)...)
	}
	recommended := 0
	ranks := map[int]bool{}
	modeByID := map[string]Mode{}
	for _, mode := range pkg.Modes {
		if mode.Recommendation.Rank == 1 {
			recommended++
		}
		if mode.Recommendation.Rank <= 0 || strings.TrimSpace(mode.Recommendation.Reason) == "" {
			issues = append(issues, fmt.Sprintf("mode %s needs a positive recommendation rank and reason", mode.ID))
		} else if ranks[mode.Recommendation.Rank] {
			issues = append(issues, fmt.Sprintf("recommendation rank %d is used more than once", mode.Recommendation.Rank))
		}
		ranks[mode.Recommendation.Rank] = true
		if !namePattern.MatchString(mode.ID) {
			issues = append(issues, fmt.Sprintf("mode %q needs a lowercase id", mode.ID))
		}
		if _, exists := modeByID[mode.ID]; exists {
			issues = append(issues, fmt.Sprintf("mode %s is defined more than once", mode.ID))
		}
		modeByID[mode.ID] = mode
		issues = append(issues, validateMode(pkg.Key, mode, methodByID, toolByID)...)
	}
	for _, source := range pkg.SkillSources {
		issues = append(issues, validateSkillSource(source, pkg)...)
	}
	if recommended != 1 {
		issues = append(issues, fmt.Sprintf("exactly one mode must be recommended (got %d)", recommended))
	}
	resolverIDs := map[string]bool{}
	for _, resolver := range pkg.Links {
		if resolverIDs[resolver.ID] {
			issues = append(issues, fmt.Sprintf("link resolver %s is defined more than once", resolver.ID))
		}
		resolverIDs[resolver.ID] = true
		lookups := resolver.LookupOptions()
		if len(lookups) == 0 {
			issues = append(issues, fmt.Sprintf("link resolver %s needs at least one lookup", resolver.ID))
		}
		if len(resolver.Lookups) > 0 && (resolver.Lookup.Mode != "" || resolver.Lookup.Operation != "") {
			issues = append(issues, fmt.Sprintf("link resolver %s must use lookup or lookups, not both", resolver.ID))
		}
		seenModes := map[string]bool{}
		for _, lookup := range lookups {
			mode, ok := modeByID[lookup.Mode]
			if !ok {
				issues = append(issues, fmt.Sprintf("link resolver %s cites unknown mode %s", resolver.ID, lookup.Mode))
			} else if operation, ok := findOperation(mode, lookup.Operation); ok && operation.Class != "read" {
				issues = append(issues, fmt.Sprintf("link resolver %s must cite a read operation", resolver.ID))
			} else if !ok && mode.Executor.Kind != "mcp" {
				issues = append(issues, fmt.Sprintf("link resolver %s cites unknown operation %s", resolver.ID, lookup.Operation))
			}
			if seenModes[lookup.Mode] {
				issues = append(issues, fmt.Sprintf("link resolver %s has more than one lookup for mode %s", resolver.ID, lookup.Mode))
			}
			seenModes[lookup.Mode] = true
			for _, heavy := range lookup.HeavyReads {
				operation, found := findOperation(mode, heavy.Operation)
				if !ok || (!found && mode.Executor.Kind != "mcp") {
					issues = append(issues, fmt.Sprintf("link resolver %s heavy read cites unknown operation %s", resolver.ID, heavy.Operation))
				} else if found && operation.Class != "read" {
					issues = append(issues, fmt.Sprintf("link resolver %s heavy read must cite a read operation", resolver.ID))
				}
				if heavy.Recurse != nil {
					recurse := heavy.Recurse
					if recurse.ItemsPath == "" || recurse.IDPath == "" || recurse.Argument == "" {
						issues = append(issues, fmt.Sprintf("link resolver %s recursive heavy read needs itemsPath, idPath, and argument", resolver.ID))
					}
					if recurse.MaxDepth < 0 || recurse.MaxDepth > 12 || recurse.MaxCalls < 0 || recurse.MaxCalls > 100 {
						issues = append(issues, fmt.Sprintf("link resolver %s recursive heavy read exceeds depth or call bounds", resolver.ID))
					}
				}
			}
		}
		issues = append(issues, validateResolver(resolver)...)
	}
	sort.Strings(issues)
	return issues
}

func validateMode(connector string, mode Mode, methods map[string]CredentialMethod, tools map[string]ToolRequirement) []string {
	var issues []string
	prefix := fmt.Sprintf("%s/%s", connector, mode.ID)
	if strings.TrimSpace(mode.Label) == "" || strings.TrimSpace(mode.Description) == "" {
		issues = append(issues, prefix+": label and description are required")
	}
	switch mode.Executor.Kind {
	case "http":
		parsed, err := fixedHTTPS(mode.Executor.BaseURL)
		if err != nil {
			issues = append(issues, prefix+": "+err.Error())
		} else if parsed.Path != "" && !strings.HasSuffix(parsed.Path, "/") {
			issues = append(issues, prefix+": baseUrl path must end in /")
		}
	case "cli":
		if (strings.TrimSpace(mode.Executor.Binary) == "") == (strings.TrimSpace(mode.Executor.Tool) == "") {
			issues = append(issues, prefix+": CLI mode needs exactly one binary or managed tool")
		}
		if mode.Executor.Tool != "" {
			if _, ok := tools[mode.Executor.Tool]; !ok {
				issues = append(issues, prefix+": cites unknown managed tool "+mode.Executor.Tool)
			}
		}
	case "mcp":
		if (mode.Executor.Endpoint == "") == (mode.Executor.Command == "") {
			issues = append(issues, prefix+": MCP mode needs exactly one endpoint or command")
		}
		if mode.Executor.Endpoint != "" {
			if _, err := fixedHTTPS(mode.Executor.Endpoint); err != nil {
				issues = append(issues, prefix+": MCP endpoint "+err.Error())
			}
		}
	default:
		issues = append(issues, prefix+": executor kind must be http, cli, or mcp")
	}
	for _, argument := range mode.Executor.Args {
		if strings.Contains(argument, "{{secret.") {
			issues = append(issues, prefix+": secrets may not enter process argv")
		}
	}
	if len(mode.CredentialMethods) == 0 {
		issues = append(issues, prefix+": at least one credential method is required")
	}
	availableMethods := map[string]bool{}
	for _, id := range mode.CredentialMethods {
		if availableMethods[id] {
			issues = append(issues, prefix+": duplicate credential method "+id)
		}
		availableMethods[id] = true
		method, ok := methods[id]
		if !ok {
			issues = append(issues, prefix+": cites unknown credential method "+id)
			continue
		}
		for _, binding := range CredentialBindingsFor(mode, method) {
			issues = append(issues, validateBinding(prefix+"/"+id, mode.Executor.Kind, binding)...)
		}
	}
	if !availableMethods[mode.DefaultCredentialMethod] {
		issues = append(issues, prefix+": defaultCredentialMethod must belong to the mode")
	}
	for id := range mode.CredentialBindings {
		if !availableMethods[id] {
			issues = append(issues, prefix+": bindings cite unavailable credential method "+id)
		}
	}
	operations := map[string]bool{}
	for _, operation := range mode.Operations {
		if !namePattern.MatchString(operation.Name) {
			issues = append(issues, prefix+": operation needs a lowercase id")
		}
		if operations[operation.Name] {
			issues = append(issues, prefix+": duplicate operation "+operation.Name)
		}
		operations[operation.Name] = true
		for _, issue := range AuditOperationSchema(operation) {
			issues = append(issues, prefix+"/"+operation.Name+": "+issue)
		}
		if !contains([]string{"read", "write", "admin", "destructive"}, operation.Class) {
			issues = append(issues, prefix+"/"+operation.Name+": class must be read, write, admin, or destructive")
		}
		if operation.Interaction != "" && !contains([]string{"request", "stream", "daemon"}, operation.Interaction) {
			issues = append(issues, prefix+"/"+operation.Name+": interaction must be request, stream, or daemon")
		}
		configured := 0
		if operation.HTTP != nil {
			configured++
		}
		if operation.CLI != nil {
			configured++
		}
		if operation.MCPTool != "" {
			configured++
		}
		if configured != 1 {
			issues = append(issues, prefix+"/"+operation.Name+": exactly one operation executor is required")
		}
		if mode.Executor.Kind == "http" && operation.HTTP == nil {
			issues = append(issues, prefix+"/"+operation.Name+": HTTP mode needs an HTTP operation")
		}
		if mode.Executor.Kind == "cli" && operation.CLI == nil {
			issues = append(issues, prefix+"/"+operation.Name+": CLI mode needs a CLI operation")
		}
		if mode.Executor.Kind == "mcp" && operation.MCPTool == "" {
			issues = append(issues, prefix+"/"+operation.Name+": MCP mode needs mcpTool")
		}
		if operation.CLI != nil {
			optionalArguments := map[string]bool{}
			if strings.ContainsAny(operation.CLI.Schema, "\r\n\x00") || strings.HasPrefix(operation.CLI.Schema, "-") {
				issues = append(issues, prefix+"/"+operation.Name+": CLI schema identifier must be one plain provider path")
			}
			for _, argument := range operation.CLI.Args {
				if strings.Contains(argument, "{{secret.") {
					issues = append(issues, prefix+"/"+operation.Name+": secrets may not enter process argv")
				}
			}
			for _, optional := range operation.CLI.OptionalArgs {
				if !namePattern.MatchString(optional.Argument) || strings.Contains(optional.Flag, "{{") {
					issues = append(issues, prefix+"/"+operation.Name+": optional CLI args need a plain flag and lowercase argument name")
				}
				if optional.Boolean && optional.Flag == "" {
					issues = append(issues, prefix+"/"+operation.Name+": boolean CLI args need a flag")
				}
				if optionalArguments[optional.Argument] {
					issues = append(issues, prefix+"/"+operation.Name+": duplicate optional CLI argument "+optional.Argument)
				}
				optionalArguments[optional.Argument] = true
			}
		}
		if operation.Pagination != nil {
			pagination := operation.Pagination
			if !namePattern.MatchString(pagination.CursorArgument) {
				issues = append(issues, prefix+"/"+operation.Name+": pagination cursorArgument needs a lowercase argument name")
			}
			if pagination.DefaultMaxPages < 0 || pagination.DefaultMaxPages > 100 || pagination.DefaultMaxItems < 0 || pagination.DefaultMaxItems > 100000 {
				issues = append(issues, prefix+"/"+operation.Name+": pagination defaults exceed the supported bounds")
			}
		}
	}
	if mode.Verify != "" {
		operation, ok := findOperation(mode, mode.Verify)
		if !ok || operation.Class != "read" {
			issues = append(issues, prefix+": verify must cite a read operation")
		}
	}
	return issues
}

func validateCredentialMethod(method CredentialMethod) []string {
	var issues []string
	prefix := "credential method " + method.ID
	if strings.TrimSpace(method.Label) == "" || strings.TrimSpace(method.Description) == "" {
		issues = append(issues, prefix+": label and description are required")
	}
	if !contains([]string{"outfit", "executor", "oauth", "none"}, method.Custody) {
		issues = append(issues, prefix+": custody must be outfit, executor, oauth, or none")
	}
	fields := map[string]bool{}
	for _, field := range method.Fields {
		if !fieldPattern.MatchString(field.Key) {
			issues = append(issues, prefix+": field key must be UPPER_SNAKE_CASE")
		}
		if fields[field.Key] {
			issues = append(issues, prefix+": duplicate field "+field.Key)
		}
		fields[field.Key] = true
		if field.Pattern != "" {
			if _, err := regexp.Compile("^(?:" + field.Pattern + ")$"); err != nil {
				issues = append(issues, prefix+": invalid field pattern for "+field.Key)
			}
		}
		seenEnvironment := map[string]bool{}
		for _, variable := range field.Environment {
			if !fieldPattern.MatchString(variable) {
				issues = append(issues, prefix+": environment source must be UPPER_SNAKE_CASE for "+field.Key)
			}
			if seenEnvironment[variable] {
				issues = append(issues, prefix+": duplicate environment source "+variable+" for "+field.Key)
			}
			seenEnvironment[variable] = true
		}
		if field.Generated && len(field.Environment) > 0 {
			issues = append(issues, prefix+": generated field "+field.Key+" cannot have environment sources")
		}
	}
	for _, binding := range method.Bindings {
		issues = append(issues, validateBinding(prefix, "", binding)...)
	}
	if method.CapabilityField != "" && !fields[method.CapabilityField] {
		issues = append(issues, prefix+": capabilityField must cite a declared field")
	}
	if method.OAuth != nil {
		if method.Custody != "oauth" {
			issues = append(issues, prefix+": OAuth configuration requires oauth custody")
		}
		if strings.TrimSpace(method.OAuth.AuthorizationURL) == "" || strings.TrimSpace(method.OAuth.TokenURL) == "" {
			issues = append(issues, prefix+": OAuth authorizationUrl and tokenUrl are required")
		}
		for capability := range method.OAuth.CapabilityScopes {
			if !namePattern.MatchString(capability) {
				issues = append(issues, prefix+": OAuth capability scope key must be lowercase")
			}
		}
	}
	for _, command := range []*CommandTemplate{method.Setup, method.Verify, method.Logout} {
		if command != nil && strings.TrimSpace(command.Program) == "" {
			issues = append(issues, prefix+": command template needs a program")
		}
		if command != nil {
			for _, arg := range command.Args {
				if strings.Contains(arg, "{{secret.") {
					issues = append(issues, prefix+": secrets may not enter process argv")
				}
			}
		}
	}
	if method.Setup != nil && method.Setup.BrowserContinuation != nil {
		flow := method.Setup.BrowserContinuation
		if !namePattern.MatchString(flow.URLField) || !namePattern.MatchString(flow.NextStepField) {
			issues = append(issues, prefix+": browser continuation needs lowercase urlField and nextStepField")
		}
		if flow.VerificationCodeField != "" && !namePattern.MatchString(flow.VerificationCodeField) {
			issues = append(issues, prefix+": browser continuation verificationCodeField must be lowercase")
		}
		if flow.TimeoutSeconds < 0 || flow.TimeoutSeconds > 900 {
			issues = append(issues, prefix+": browser continuation timeoutSeconds must be between 0 and 900")
		}
	}
	for _, command := range []*CommandTemplate{method.Verify, method.Logout} {
		if command != nil && command.BrowserContinuation != nil {
			issues = append(issues, prefix+": browser continuation is supported only for setup commands")
		}
	}
	return issues
}

func validateBinding(prefix, executorKind string, binding CredentialBinding) []string {
	var issues []string
	if !contains([]string{"process-env", "unset-env", "stdin", "temporary-file", "temporary-directory", "http-header", "http-query", "mcp-header"}, binding.Target) {
		return []string{prefix + ": unknown credential binding target " + binding.Target}
	}
	if !contains([]string{"temporary-directory", "unset-env"}, binding.Target) && strings.TrimSpace(binding.Value) == "" {
		issues = append(issues, prefix+": credential binding value is required")
	}
	if contains([]string{"process-env", "unset-env", "temporary-directory", "http-header", "http-query", "mcp-header"}, binding.Target) && strings.TrimSpace(binding.Name) == "" {
		issues = append(issues, prefix+": credential binding "+binding.Target+" needs a name")
	}
	if binding.Target == "temporary-file" && strings.TrimSpace(binding.Path) == "" {
		issues = append(issues, prefix+": temporary-file binding needs a path template")
	}
	if executorKind != "" {
		allowed := map[string][]string{
			"cli":  {"process-env", "unset-env", "stdin", "temporary-file", "temporary-directory"},
			"http": {"http-header", "http-query"},
			"mcp":  {"mcp-header", "process-env", "unset-env", "temporary-file", "temporary-directory"},
		}
		if !contains(allowed[executorKind], binding.Target) {
			issues = append(issues, prefix+": credential binding "+binding.Target+" is incompatible with "+executorKind)
		}
	}
	return issues
}

func validateTool(tool ToolRequirement) []string {
	var issues []string
	switch tool.Source.Kind {
	case "github-release":
		if strings.TrimSpace(tool.Source.Repository) == "" || len(tool.Source.Assets) == 0 {
			issues = append(issues, "managed tool "+tool.ID+": GitHub release source needs repository and assets")
		}
	case "fixed-url":
		if _, err := fixedHTTPS(tool.Source.URL); err != nil || len(tool.Source.Checksums) == 0 {
			issues = append(issues, "managed tool "+tool.ID+": fixed URL source needs HTTPS URL and checksums")
		}
	case "adopt-only":
	default:
		issues = append(issues, "managed tool "+tool.ID+": source kind must be github-release, fixed-url, or adopt-only")
	}
	if len(tool.Verify) == 0 {
		issues = append(issues, "managed tool "+tool.ID+": verify command is required")
	}
	return issues
}

func validateSkillSource(source SkillSource, pkg Package) []string {
	var issues []string
	if !idPattern.MatchString(source.ID) || len(source.Paths) == 0 {
		issues = append(issues, "skill source needs a lowercase id and at least one path")
	}
	if source.Kind == "github-directory" {
		if source.Repository == "" || source.Ref == "" || source.Digest == "" {
			issues = append(issues, "skill source "+source.ID+": GitHub directory needs repository, immutable ref, and digest")
		}
	} else if source.Kind != "authored" && source.Kind != "generated-operations" {
		issues = append(issues, "skill source "+source.ID+": unsupported kind "+source.Kind)
	}
	capabilities := map[string]bool{}
	operations := map[string]bool{}
	for _, mode := range pkg.Modes {
		for _, capability := range mode.Capabilities {
			capabilities[capability] = true
		}
		for _, operation := range mode.Operations {
			operations[operation.Name] = true
		}
	}
	for _, path := range source.Paths {
		if strings.TrimSpace(path.Path) == "" || filepathEscapes(path.Path) {
			issues = append(issues, "skill source "+source.ID+": invalid skill path")
		}
		for _, capability := range path.Capabilities {
			if !capabilities[capability] {
				issues = append(issues, "skill source "+source.ID+": path cites unknown capability "+capability)
			}
		}
		for _, operation := range path.Operations {
			if !operations[operation] {
				issues = append(issues, "skill source "+source.ID+": path cites unknown operation "+operation)
			}
		}
	}
	return issues
}

func filepathEscapes(path string) bool {
	return strings.HasPrefix(path, "/") || path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../")
}

func validateResolver(resolver LinkResolver) []string {
	var issues []string
	if !namePattern.MatchString(resolver.ID) || strings.TrimSpace(resolver.ResourceType) == "" {
		issues = append(issues, "link resolver needs an id and resourceType")
	}
	if len(resolver.Patterns) == 0 {
		issues = append(issues, fmt.Sprintf("link resolver %s needs at least one pattern", resolver.ID))
	}
	for _, pattern := range resolver.Patterns {
		if len(pattern.Hosts) == 0 || !strings.HasPrefix(pattern.Path, "/") {
			issues = append(issues, fmt.Sprintf("link resolver %s patterns need hosts and an absolute path template", resolver.ID))
		}
		for _, host := range pattern.Hosts {
			plain := strings.TrimPrefix(host, "*.")
			if strings.ContainsAny(plain, "*/:@?#") || strings.TrimSpace(host) != strings.ToLower(host) || strings.Count(host, "*") > 1 || (strings.Contains(host, "*") && !strings.HasPrefix(host, "*.")) {
				issues = append(issues, fmt.Sprintf("link resolver %s has invalid host %q", resolver.ID, host))
			}
		}
		for _, environment := range pattern.Environments {
			if !contains([]string{"test", "live"}, environment) {
				issues = append(issues, fmt.Sprintf("link resolver %s has invalid environment %q", resolver.ID, environment))
			}
		}
		for name, rule := range pattern.Segments {
			if !strings.Contains(pattern.Path, ":"+name) {
				issues = append(issues, fmt.Sprintf("link resolver %s declares unused segment %s", resolver.ID, name))
			}
			if rule.Kind != "" && !segmentKinds[rule.Kind] {
				issues = append(issues, fmt.Sprintf("link resolver %s has unknown segment kind %s", resolver.ID, rule.Kind))
			}
			if (rule.Kind == "") == (len(rule.OneOf) == 0) {
				issues = append(issues, fmt.Sprintf("link resolver %s segment %s needs exactly one kind or oneOf", resolver.ID, name))
			}
			if rule.Prefix != "" && (rule.Kind == "" || !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(rule.Prefix)) {
				issues = append(issues, fmt.Sprintf("link resolver %s segment %s prefix needs a kind and a plain literal", resolver.ID, name))
			}
		}
		for name, rule := range pattern.Query {
			if strings.TrimSpace(name) == "" {
				issues = append(issues, fmt.Sprintf("link resolver %s has an empty query capture", resolver.ID))
			}
			if rule.Kind != "" && !segmentKinds[rule.Kind] {
				issues = append(issues, fmt.Sprintf("link resolver %s has unknown query kind %s", resolver.ID, rule.Kind))
			}
			if (rule.Kind == "") == (len(rule.OneOf) == 0) {
				issues = append(issues, fmt.Sprintf("link resolver %s query %s needs exactly one kind or oneOf", resolver.ID, name))
			}
			if rule.Prefix != "" && (rule.Kind == "" || !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(rule.Prefix)) {
				issues = append(issues, fmt.Sprintf("link resolver %s query %s prefix needs a kind and a plain literal", resolver.ID, name))
			}
		}
	}
	if resolver.Limits.TimeoutMS < 0 || resolver.Limits.TimeoutMS > 12000 {
		issues = append(issues, fmt.Sprintf("link resolver %s timeout must be at most 12000ms", resolver.ID))
	}
	if resolver.Limits.MaxResultBytes < 0 || resolver.Limits.MaxResultBytes > 262144 {
		issues = append(issues, fmt.Sprintf("link resolver %s result limit must be at most 262144 bytes", resolver.ID))
	}
	return issues
}

func fixedHTTPS(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("needs a fixed HTTPS URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("URL must not contain userinfo, query, or fragment")
	}
	return parsed, nil
}

func findOperation(mode Mode, name string) (Operation, bool) {
	for _, operation := range mode.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return Operation{}, false
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}
