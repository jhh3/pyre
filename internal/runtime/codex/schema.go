package codex

import (
	"reflect"
	"strings"

	"github.com/jhh3/pyre/internal/runtime/codex/codexschema"
)

// schemaBinding ties a struct in types.go to the generated definition it
// encodes/decodes. Every JSON field of the struct must exist in the schema.
type schemaBinding struct {
	v   any
	def string
}

var structBindings = []schemaBinding{
	{clientInfo{}, "ClientInfo"},
	{initializeCapabilities{}, "InitializeCapabilities"},
	{initializeParams{}, "InitializeParams"},
	{turnError{}, "TurnError"},
	{turn{}, "Turn"},
	{thread{}, "Thread"},
	{threadStartParams{}, "ThreadStartParams"},
	{threadResumeParams{}, "ThreadResumeParams"},
	{threadResponse{}, "ThreadStartResponse"},
	{threadResponse{}, "ThreadResumeResponse"},
	{turnStartParams{}, "TurnStartParams"},
	{turnStartResponse{}, "TurnStartResponse"},
	{turnSteerParams{}, "TurnSteerParams"},
	{turnSteerResponse{}, "TurnSteerResponse"},
	{turnInterruptParams{}, "TurnInterruptParams"},
	{threadIDParams{}, "ThreadUnsubscribeParams"},
	{threadIDParams{}, "ThreadBackgroundTerminalsCleanParams"},
	{threadIDParams{}, "ThreadClosedNotification"},
	{turnsListParams{}, "ThreadTurnsListParams"},
	{turnsListResponse{}, "ThreadTurnsListResponse"},
	{itemNotification{}, "ItemStartedNotification"},
	{itemNotification{}, "ItemCompletedNotification"},
	{turnNotification{}, "TurnCompletedNotification"},
	{turnNotification{}, "TurnStartedNotification"},
	{statusNotification{}, "ThreadStatusChangedNotification"},
	{resolvedNotification{}, "ServerRequestResolvedNotification"},
	{errorNotification{}, "ErrorNotification"},
	{threadStartedNotification{}, "ThreadStartedNotification"},
	{configValueWriteParams{}, "ConfigValueWriteParams"},
	{threadSetNameParams{}, "ThreadSetNameParams"},
	{threadInjectItemsParams{}, "ThreadInjectItemsParams"},
}

func jsonFields(v any) []string {
	t := reflect.TypeOf(v)
	var out []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// SchemaRequirements lists every method, field and enum value the adapter
// relies on (Codex 0.160.0 baseline). Checked by schema_test.go against
// `codex app-server generate-json-schema` for the installed version, and by
// `go run ./tools/codexschema`.
// The fields internal/accounts decodes from account/rateLimits/read are
// checked alongside these via accounts.CodexSchemaRequirements (schema_test.go
// and tools/codexschema).
func SchemaRequirements() []codexschema.Req {
	cr, cn, sn, sr := codexschema.ClientRequest, codexschema.ClientNotification, codexschema.ServerNotification, codexschema.ServerRequest
	reqs := []codexschema.Req{
		// Client requests we send.
		{Kind: cr, Method: "initialize", Why: "handshake"},
		{Kind: cr, Method: "thread/start", Why: "Launch"},
		{Kind: cr, Method: "thread/resume", Why: "Recover/resume/subscribe"},
		{Kind: cr, Method: "thread/unsubscribe", Why: "Stop"},
		{Kind: cr, Method: "turn/start", Why: "Deliver"},
		{Kind: cr, Method: "turn/steer", Why: "Deliver steer"},
		{Kind: cr, Method: "turn/interrupt", Why: "Interrupt"},
		{Kind: cr, Method: "thread/turns/list", Why: "Reconcile/ReadTranscript"},
		{Kind: cr, Method: "thread/backgroundTerminals/clean", Why: "Interrupt (experimental)"},
		{Kind: cr, Method: "account/read", Fields: []string{"refreshToken"}, Why: "ReadAccount"},
		{Kind: cr, Method: "account/rateLimits/read", Why: "ReadRateLimits (WP13)"},
		{Kind: cr, Method: "config/value/write", Why: "TUI folder trust (ensureTrust)"},
		{Kind: cr, Method: "thread/name/set", Why: "name a new thread (session title)"},
		{Kind: cr, Method: "thread/inject_items", Why: "materialize a new thread's rollout for the native TUI"},
		{Kind: cn, Method: "initialized", Why: "handshake"},
		// Server notifications we consume.
		{Kind: sn, Method: "thread/started", Why: "subagent parent map"},
		{Kind: sn, Method: "thread/status/changed", Why: "run state"},
		{Kind: sn, Method: "thread/closed", Why: "unload"},
		{Kind: sn, Method: "turn/started", Why: "active turn"},
		{Kind: sn, Method: "turn/completed", Why: "turn end / limits"},
		{Kind: sn, Method: "item/started", Why: "evidence/activity"},
		{Kind: sn, Method: "item/completed", Why: "evidence/activity"},
		{Kind: sn, Method: "serverRequest/resolved", Why: "approval truth"},
		{Kind: sn, Method: "error", Why: "limit events"},
		{Kind: sn, Method: "account/rateLimits/updated", Fields: []string{"rateLimits"}, Why: "own-login usage from runs (OnRateLimits)"},
		// Server requests we answer.
		{Kind: sr, Method: "item/commandExecution/requestApproval", Fields: []string{"threadId", "turnId", "itemId", "command", "cwd", "reason", "availableDecisions"}, Why: "approvals"},
		{Kind: sr, Method: "item/fileChange/requestApproval", Fields: []string{"threadId", "turnId", "itemId", "reason", "grantRoot"}, Why: "approvals"},
		{Kind: sr, Method: "item/permissions/requestApproval", Fields: []string{"threadId", "turnId", "itemId", "reason", "permissions"}, Why: "approvals"},
		{Kind: sr, Method: "item/tool/requestUserInput", Fields: []string{"threadId", "turnId", "itemId", "questions"}, Why: "questions"},
		// Responses we send to server requests.
		{Type: "CommandExecutionRequestApprovalResponse", Fields: []string{"decision"}, Why: "Answer"},
		{Type: "CommandExecutionApprovalDecision", Enum: []string{"accept", "acceptForSession", "decline", "cancel", "acceptWithExecpolicyAmendment"}, Why: "approval options"},
		{Type: "FileChangeRequestApprovalResponse", Fields: []string{"decision"}, Why: "Answer"},
		{Type: "FileChangeApprovalDecision", Enum: []string{"accept", "acceptForSession", "decline", "cancel"}, Why: "approval options"},
		{Type: "PermissionsRequestApprovalResponse", Fields: []string{"permissions"}, Why: "Answer"},
		{Type: "ToolRequestUserInputResponse", Fields: []string{"answers"}, Why: "Answer question"},
		{Type: "ToolRequestUserInputAnswer", Fields: []string{"answers"}, Why: "Answer question"},
		{Type: "ToolRequestUserInputQuestion", Fields: []string{"id", "header", "question", "options"}, Why: "question rendering"},
		{Type: "ToolRequestUserInputOption", Fields: []string{"label", "description"}, Why: "question options"},
		// Unions decoded through generic structs.
		{Type: "ThreadItem", Variant: "userMessage", Fields: []string{"id", "clientId", "content"}, Why: "acceptance evidence (clientId)"},
		{Type: "ThreadItem", Variant: "agentMessage", Fields: []string{"id", "text"}, Why: "activity/transcript"},
		{Type: "ThreadItem", Variant: "commandExecution", Fields: []string{"id", "command", "exitCode", "status"}, Why: "activity/transcript"},
		{Type: "ThreadItem", Variant: "fileChange", Fields: []string{"id", "changes", "status"}, Why: "transcript"},
		{Type: "ThreadItem", Variant: "mcpToolCall", Fields: []string{"id", "server", "tool", "status"}, Why: "transcript"},
		{Type: "ThreadItem", Variant: "subAgentActivity", Fields: []string{"id", "kind", "agentThreadId", "agentPath"}, Why: "subagents"},
		{Type: "UserInput", Variant: "text", Fields: []string{"type", "text"}, Why: "turn input"},
		{Type: "ThreadStatus", Variant: "active", Fields: []string{"type", "activeFlags"}, Why: "run state"},
		{Type: "ThreadStatus", Enum: []string{"notLoaded", "idle", "systemError", "active"}, Why: "run state"},
		{Type: "ThreadActiveFlag", Enum: []string{"waitingOnApproval", "waitingOnUserInput"}, Why: "waiting_approval"},
		{Type: "TurnStatus", Enum: []string{"completed", "interrupted", "failed", "inProgress"}, Why: "turn end"},
		{Type: "CodexErrorInfo", Enum: []string{"usageLimitExceeded", "sessionBudgetExceeded", "rateLimitExceeded", "unauthorized"}, Why: "limit events"},
		{Type: "AskForApproval", Enum: []string{"untrusted", "on-request", "never"}, Why: "approval policy"},
		{Type: "SandboxMode", Enum: []string{"read-only", "workspace-write", "danger-full-access"}, Why: "sandbox"},
		{Type: "TurnItemsView", Enum: []string{"full"}, Why: "turns/list"},
		{Type: "MergeStrategy", Enum: []string{"upsert"}, Why: "ensureTrust"},
		{Type: "SortDirection", Enum: []string{"asc", "desc"}, Why: "turns/list"},
		{Type: "GetAccountResponse", Fields: []string{"account", "requiresOpenaiAuth"}, Why: "ReadAccount"},
		{Type: "Account", Variant: "chatgpt", Fields: []string{"type", "planType"}, Why: "ReadAccount"},
		{Type: "GetAccountRateLimitsResponse", Fields: []string{"rateLimits", "rateLimitsByLimitId"}, Why: "WP13 rate-limit parsing"},
		// internal/accounts decodes these too; its decode-struct fields are
		// checked separately (accounts.CodexSchemaRequirements).
		{Type: "RateLimitSnapshot", Fields: []string{"primary", "secondary", "limitId", "limitName", "rateLimitReachedType"}, Why: "WP13 rate-limit parsing"},
		{Type: "RateLimitWindow", Fields: []string{"usedPercent", "resetsAt", "windowDurationMins"}, Why: "WP13 rate-limit parsing"},
	}
	for _, b := range structBindings {
		reqs = append(reqs, codexschema.Req{Type: b.def, Fields: jsonFields(b.v), Why: "types.go " + reflect.TypeOf(b.v).Name()})
	}
	return reqs
}
