package kit

import "time"

const APIVersion = "outfit.jhh3.dev/connector/v1alpha2"

type Package struct {
	APIVersion        string             `json:"apiVersion" yaml:"apiVersion"`
	Key               string             `json:"key" yaml:"key"`
	Display           string             `json:"display" yaml:"display"`
	Version           string             `json:"version" yaml:"version"`
	Description       string             `json:"description,omitempty" yaml:"description,omitempty"`
	Category          string             `json:"category,omitempty" yaml:"category,omitempty"`
	Aliases           []string           `json:"aliases,omitempty" yaml:"aliases,omitempty"`
	UseCases          []string           `json:"useCases,omitempty" yaml:"useCases,omitempty"`
	Limitations       []string           `json:"limitations,omitempty" yaml:"limitations,omitempty"`
	Tools             []ToolRequirement  `json:"tools,omitempty" yaml:"tools,omitempty"`
	CredentialMethods []CredentialMethod `json:"credentialMethods" yaml:"credentialMethods"`
	Modes             []Mode             `json:"modes" yaml:"modes"`
	Links             []LinkResolver     `json:"links,omitempty" yaml:"links,omitempty"`
	Skill             Skill              `json:"skill" yaml:"skill"`
	SkillSources      []SkillSource      `json:"skillSources,omitempty" yaml:"skillSources,omitempty"`
}

type Mode struct {
	ID                      string                         `json:"id" yaml:"id"`
	Label                   string                         `json:"label" yaml:"label"`
	Description             string                         `json:"description" yaml:"description"`
	Recommendation          Recommendation                 `json:"recommendation" yaml:"recommendation"`
	Executor                Executor                       `json:"executor" yaml:"executor"`
	CredentialMethods       []string                       `json:"credentialMethods" yaml:"credentialMethods"`
	DefaultCredentialMethod string                         `json:"defaultCredentialMethod" yaml:"defaultCredentialMethod"`
	CredentialBindings      map[string][]CredentialBinding `json:"credentialBindings,omitempty" yaml:"credentialBindings,omitempty"`
	Capabilities            []string                       `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Operations              []Operation                    `json:"operations,omitempty" yaml:"operations,omitempty"`
	Verify                  string                         `json:"verify,omitempty" yaml:"verify,omitempty"`
}

type Recommendation struct {
	Rank   int    `json:"rank" yaml:"rank"`
	Reason string `json:"reason" yaml:"reason"`
}

type Executor struct {
	Kind       string   `json:"kind" yaml:"kind"`
	Tool       string   `json:"tool,omitempty" yaml:"tool,omitempty"`
	BaseURL    string   `json:"baseUrl,omitempty" yaml:"baseUrl,omitempty"`
	Binary     string   `json:"binary,omitempty" yaml:"binary,omitempty"`
	Endpoint   string   `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Command    string   `json:"command,omitempty" yaml:"command,omitempty"`
	Args       []string `json:"args,omitempty" yaml:"args,omitempty"`
	PrivateNet bool     `json:"privateNetwork,omitempty" yaml:"privateNetwork,omitempty"`
}

type CredentialMethod struct {
	ID              string              `json:"id" yaml:"id"`
	Label           string              `json:"label" yaml:"label"`
	Description     string              `json:"description" yaml:"description"`
	Recommended     bool                `json:"recommended,omitempty" yaml:"recommended,omitempty"`
	Custody         string              `json:"custody" yaml:"custody"`
	Fields          []CredentialField   `json:"fields,omitempty" yaml:"fields,omitempty"`
	CapabilityField string              `json:"capabilityField,omitempty" yaml:"capabilityField,omitempty"`
	Bindings        []CredentialBinding `json:"bindings,omitempty" yaml:"bindings,omitempty"`
	Setup           *CommandTemplate    `json:"setup,omitempty" yaml:"setup,omitempty"`
	Verify          *CommandTemplate    `json:"verify,omitempty" yaml:"verify,omitempty"`
	Logout          *CommandTemplate    `json:"logout,omitempty" yaml:"logout,omitempty"`
	OAuth           *OAuthConfig        `json:"oauth,omitempty" yaml:"oauth,omitempty"`
}

type CredentialField struct {
	Key         string   `json:"key" yaml:"key"`
	Label       string   `json:"label" yaml:"label"`
	Secret      bool     `json:"secret,omitempty" yaml:"secret,omitempty"`
	Optional    bool     `json:"optional,omitempty" yaml:"optional,omitempty"`
	Multiple    bool     `json:"multiple,omitempty" yaml:"multiple,omitempty"`
	Description string   `json:"description,omitempty" yaml:"description,omitempty"`
	Placeholder string   `json:"placeholder,omitempty" yaml:"placeholder,omitempty"`
	Pattern     string   `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	Default     string   `json:"default,omitempty" yaml:"default,omitempty"`
	Choices     []Choice `json:"choices,omitempty" yaml:"choices,omitempty"`
	Normalize   string   `json:"normalize,omitempty" yaml:"normalize,omitempty"`
	Generated   bool     `json:"generated,omitempty" yaml:"generated,omitempty"`
	Environment []string `json:"environment,omitempty" yaml:"environment,omitempty"`
}

type Choice struct {
	Value string `json:"value" yaml:"value"`
	Label string `json:"label,omitempty" yaml:"label,omitempty"`
}

type CredentialBinding struct {
	Target         string `json:"target" yaml:"target"`
	Name           string `json:"name,omitempty" yaml:"name,omitempty"`
	Value          string `json:"value" yaml:"value"`
	Path           string `json:"path,omitempty" yaml:"path,omitempty"`
	Mode           uint32 `json:"mode,omitempty" yaml:"mode,omitempty"`
	ClearInherited bool   `json:"clearInherited,omitempty" yaml:"clearInherited,omitempty"`
}

type CommandTemplate struct {
	Program             string               `json:"program" yaml:"program"`
	Args                []string             `json:"args,omitempty" yaml:"args,omitempty"`
	Stdin               string               `json:"stdin,omitempty" yaml:"stdin,omitempty"`
	BrowserContinuation *BrowserContinuation `json:"browserContinuation,omitempty" yaml:"browserContinuation,omitempty"`
}

// BrowserContinuation describes a trusted CLI's non-interactive browser-login
// handoff. Outfit opens the declared URL and executes the returned next step
// directly as argv through the same pinned binary; it never invokes a shell.
type BrowserContinuation struct {
	URLField              string `json:"urlField" yaml:"urlField"`
	VerificationCodeField string `json:"verificationCodeField,omitempty" yaml:"verificationCodeField,omitempty"`
	NextStepField         string `json:"nextStepField" yaml:"nextStepField"`
	TimeoutSeconds        int    `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`
}

type OAuthConfig struct {
	ClientID            string              `json:"clientId,omitempty" yaml:"clientId,omitempty"`
	AuthorizationURL    string              `json:"authorizationUrl,omitempty" yaml:"authorizationUrl,omitempty"`
	TokenURL            string              `json:"tokenUrl,omitempty" yaml:"tokenUrl,omitempty"`
	UserInfoURL         string              `json:"userInfoUrl,omitempty" yaml:"userInfoUrl,omitempty"`
	Scopes              []string            `json:"scopes,omitempty" yaml:"scopes,omitempty"`
	CapabilityScopes    map[string][]string `json:"capabilityScopes,omitempty" yaml:"capabilityScopes,omitempty"`
	DynamicRegistration bool                `json:"dynamicRegistration,omitempty" yaml:"dynamicRegistration,omitempty"`
}

type Operation struct {
	Name         string         `json:"name" yaml:"name"`
	Title        string         `json:"title,omitempty" yaml:"title,omitempty"`
	Description  string         `json:"description" yaml:"description"`
	Class        string         `json:"class" yaml:"class"`
	Interaction  string         `json:"interaction,omitempty" yaml:"interaction,omitempty"`
	Capabilities []string       `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Input        map[string]any `json:"input,omitempty" yaml:"input,omitempty"`
	HTTP         *HTTPOperation `json:"http,omitempty" yaml:"http,omitempty"`
	CLI          *CLIOperation  `json:"cli,omitempty" yaml:"cli,omitempty"`
	MCPTool      string         `json:"mcpTool,omitempty" yaml:"mcpTool,omitempty"`
	Pagination   *Pagination    `json:"pagination,omitempty" yaml:"pagination,omitempty"`
	Output       Output         `json:"output,omitempty" yaml:"output,omitempty"`
}

// Pagination describes a cursor-shaped provider contract. Outfit also knows
// the common cursor spellings used by MCP, GraphQL, Stripe, and REST APIs, but
// an authored connector can pin the exact paths so every invocation surface
// paginates identically.
type Pagination struct {
	CursorArgument         string `json:"cursorArgument" yaml:"cursorArgument"`
	NextCursorPath         string `json:"nextCursorPath,omitempty" yaml:"nextCursorPath,omitempty"`
	ItemsPath              string `json:"itemsPath,omitempty" yaml:"itemsPath,omitempty"`
	HasMorePath            string `json:"hasMorePath,omitempty" yaml:"hasMorePath,omitempty"`
	CursorFromLastItemPath string `json:"cursorFromLastItemPath,omitempty" yaml:"cursorFromLastItemPath,omitempty"`
	DefaultMaxPages        int    `json:"defaultMaxPages,omitempty" yaml:"defaultMaxPages,omitempty"`
	DefaultMaxItems        int    `json:"defaultMaxItems,omitempty" yaml:"defaultMaxItems,omitempty"`
}

type HTTPOperation struct {
	Method       string            `json:"method" yaml:"method"`
	Path         string            `json:"path" yaml:"path"`
	Query        []string          `json:"query,omitempty" yaml:"query,omitempty"`
	QueryNames   map[string]string `json:"queryNames,omitempty" yaml:"queryNames,omitempty"`
	Body         []string          `json:"body,omitempty" yaml:"body,omitempty"`
	BodyNames    map[string]string `json:"bodyNames,omitempty" yaml:"bodyNames,omitempty"`
	BodyValues   map[string]any    `json:"bodyValues,omitempty" yaml:"bodyValues,omitempty"`
	Headers      map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	SuccessField string            `json:"successField,omitempty" yaml:"successField,omitempty"`
	ErrorField   string            `json:"errorField,omitempty" yaml:"errorField,omitempty"`
}

type CLIOperation struct {
	Args         []string         `json:"args" yaml:"args"`
	OptionalArgs []CLIOptionalArg `json:"optionalArgs,omitempty" yaml:"optionalArgs,omitempty"`
	Stdin        string           `json:"stdin,omitempty" yaml:"stdin,omitempty"`
	Schema       string           `json:"schema,omitempty" yaml:"schema,omitempty"`
}

type CLIOptionalArg struct {
	Flag     string `json:"flag,omitempty" yaml:"flag,omitempty"`
	Argument string `json:"argument" yaml:"argument"`
	Boolean  bool   `json:"boolean,omitempty" yaml:"boolean,omitempty"`
}

type Output struct {
	Format string   `json:"format,omitempty" yaml:"format,omitempty"`
	Pick   []string `json:"pick,omitempty" yaml:"pick,omitempty"`
}

type LinkResolver struct {
	ID           string           `json:"id" yaml:"id"`
	ResourceType string           `json:"resourceType" yaml:"resourceType"`
	Priority     int              `json:"priority,omitempty" yaml:"priority,omitempty"`
	Patterns     []LinkPattern    `json:"patterns" yaml:"patterns"`
	Lookup       LinkLookup       `json:"lookup,omitempty" yaml:"lookup,omitempty"`
	Lookups      []LinkLookup     `json:"lookups,omitempty" yaml:"lookups,omitempty"`
	Presentation LinkPresentation `json:"presentation" yaml:"presentation"`
	Limits       LinkLimits       `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// LookupOptions returns the mode-specific lookup alternatives for a link.
// A singular lookup remains accepted so existing connector-kit packages keep
// loading, while blessed packages use lookups whenever modes differ.
func (r LinkResolver) LookupOptions() []LinkLookup {
	if len(r.Lookups) > 0 {
		return r.Lookups
	}
	if r.Lookup.Mode != "" || r.Lookup.Operation != "" || len(r.Lookup.Arguments) > 0 {
		return []LinkLookup{r.Lookup}
	}
	return nil
}

type LinkPattern struct {
	Hosts           []string               `json:"hosts" yaml:"hosts"`
	Path            string                 `json:"path" yaml:"path"`
	Environments    []string               `json:"environments,omitempty" yaml:"environments,omitempty"`
	Segments        map[string]SegmentRule `json:"segments,omitempty" yaml:"segments,omitempty"`
	Query           map[string]SegmentRule `json:"query,omitempty" yaml:"query,omitempty"`
	Fragment        string                 `json:"fragment,omitempty" yaml:"fragment,omitempty"`
	MaxRestSegments int                    `json:"maxRestSegments,omitempty" yaml:"maxRestSegments,omitempty"`
}

type SegmentRule struct {
	Kind   string   `json:"kind,omitempty" yaml:"kind,omitempty"`
	OneOf  []string `json:"oneOf,omitempty" yaml:"oneOf,omitempty"`
	Prefix string   `json:"prefix,omitempty" yaml:"prefix,omitempty"`
}

func (r *SegmentRule) UnmarshalYAML(unmarshal func(any) error) error {
	var scalar string
	if err := unmarshal(&scalar); err == nil {
		r.Kind = scalar
		return nil
	}
	type plain SegmentRule
	var value plain
	if err := unmarshal(&value); err != nil {
		return err
	}
	*r = SegmentRule(value)
	return nil
}

type LinkLookup struct {
	Mode       string            `json:"mode" yaml:"mode"`
	Operation  string            `json:"operation" yaml:"operation"`
	Arguments  map[string]string `json:"arguments,omitempty" yaml:"arguments,omitempty"`
	HeavyReads []LinkHeavyRead   `json:"heavyReads,omitempty" yaml:"heavyReads,omitempty"`
}

// LinkHeavyRead declares additional read-only evidence fetched when automatic
// link context runs in heavy mode. Arguments reference the validated arguments
// of the primary lookup. Recurse supports bounded tree-shaped APIs such as
// Notion block children without putting provider-specific behavior in outfit.
type LinkHeavyRead struct {
	Operation string            `json:"operation" yaml:"operation"`
	Arguments map[string]string `json:"arguments,omitempty" yaml:"arguments,omitempty"`
	Recurse   *LinkRecursion    `json:"recurse,omitempty" yaml:"recurse,omitempty"`
}

type LinkRecursion struct {
	ItemsPath       string `json:"itemsPath" yaml:"itemsPath"`
	IDPath          string `json:"idPath" yaml:"idPath"`
	HasChildrenPath string `json:"hasChildrenPath,omitempty" yaml:"hasChildrenPath,omitempty"`
	Argument        string `json:"argument" yaml:"argument"`
	MaxDepth        int    `json:"maxDepth,omitempty" yaml:"maxDepth,omitempty"`
	MaxCalls        int    `json:"maxCalls,omitempty" yaml:"maxCalls,omitempty"`
}

type LinkPresentation struct {
	CanonicalID string            `json:"canonicalId" yaml:"canonicalId"`
	Title       string            `json:"title,omitempty" yaml:"title,omitempty"`
	Summary     string            `json:"summary,omitempty" yaml:"summary,omitempty"`
	State       string            `json:"state,omitempty" yaml:"state,omitempty"`
	Author      string            `json:"author,omitempty" yaml:"author,omitempty"`
	UpdatedAt   string            `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
	Fields      map[string]string `json:"fields,omitempty" yaml:"fields,omitempty"`
	Excerpts    string            `json:"excerpts,omitempty" yaml:"excerpts,omitempty"`
}

type LinkLimits struct {
	Timeout         time.Duration `json:"-" yaml:"-"`
	TimeoutMS       int           `json:"timeoutMs,omitempty" yaml:"timeoutMs,omitempty"`
	MaxResultBytes  int           `json:"maxResultBytes,omitempty" yaml:"maxResultBytes,omitempty"`
	CacheTTLSeconds int           `json:"cacheTtlSeconds,omitempty" yaml:"cacheTtlSeconds,omitempty"`
}

type Skill struct {
	Summary    string          `json:"summary" yaml:"summary"`
	Guidance   []string        `json:"guidance,omitempty" yaml:"guidance,omitempty"`
	Examples   []string        `json:"examples,omitempty" yaml:"examples,omitempty"`
	Workflows  []SkillWorkflow `json:"workflows,omitempty" yaml:"workflows,omitempty"`
	References []string        `json:"references,omitempty" yaml:"references,omitempty"`
}

type SkillWorkflow struct {
	Title string   `json:"title" yaml:"title"`
	Steps []string `json:"steps" yaml:"steps"`
}

type SkillSource struct {
	ID          string      `json:"id" yaml:"id"`
	Kind        string      `json:"kind" yaml:"kind"`
	Repository  string      `json:"repository,omitempty" yaml:"repository,omitempty"`
	Ref         string      `json:"ref,omitempty" yaml:"ref,omitempty"`
	Paths       []SkillPath `json:"paths" yaml:"paths"`
	Digest      string      `json:"digest,omitempty" yaml:"digest,omitempty"`
	Attribution string      `json:"attribution,omitempty" yaml:"attribution,omitempty"`
}

type SkillPath struct {
	Path         string   `json:"path" yaml:"path"`
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Operations   []string `json:"operations,omitempty" yaml:"operations,omitempty"`
}

type ToolRequirement struct {
	ID      string     `json:"id" yaml:"id"`
	Display string     `json:"display" yaml:"display"`
	Binary  string     `json:"binary" yaml:"binary"`
	Version string     `json:"version" yaml:"version"`
	Source  ToolSource `json:"source" yaml:"source"`
	Verify  []string   `json:"verify" yaml:"verify"`
}

type ToolSource struct {
	Kind       string            `json:"kind" yaml:"kind"`
	Repository string            `json:"repository,omitempty" yaml:"repository,omitempty"`
	Tag        string            `json:"tag,omitempty" yaml:"tag,omitempty"`
	URL        string            `json:"url,omitempty" yaml:"url,omitempty"`
	Assets     map[string]string `json:"assets,omitempty" yaml:"assets,omitempty"`
	Checksums  map[string]string `json:"checksums,omitempty" yaml:"checksums,omitempty"`
	BinaryPath string            `json:"binaryPath,omitempty" yaml:"binaryPath,omitempty"`
}
