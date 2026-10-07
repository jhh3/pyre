package codex

import "encoding/json"

// Minimal Codex app-server protocol types (0.160.0), only for the methods
// this adapter uses. Generated schema: `codex app-server
// generate-json-schema --out DIR` (index in spikes/runtime/codex-schema).

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeCapabilities struct {
	ExperimentalAPI           bool     `json:"experimentalApi"`
	OptOutNotificationMethods []string `json:"optOutNotificationMethods,omitempty"`
}

type initializeParams struct {
	ClientInfo   clientInfo             `json:"clientInfo"`
	Capabilities initializeCapabilities `json:"capabilities"`
}

type threadStatus struct {
	Type        string   `json:"type"` // notLoaded | idle | systemError | active
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

type turnError struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo,omitempty"`
}

type turn struct {
	ID     string `json:"id"`
	Status string `json:"status"` // completed | interrupted | failed | inProgress
	// StartedAt is the turn start (unix seconds); items carry no time.
	StartedAt *int64            `json:"startedAt,omitempty"`
	Items     []json.RawMessage `json:"items,omitempty"`
	Error     *turnError        `json:"error,omitempty"`
}

type thread struct {
	ID             string       `json:"id"`
	Status         threadStatus `json:"status"`
	ParentThreadID string       `json:"parentThreadId,omitempty"`
	ForkedFromID   string       `json:"forkedFromId,omitempty"`
	Turns          []turn       `json:"turns,omitempty"`
	Cwd            string       `json:"cwd,omitempty"`
}

type threadStartParams struct {
	Cwd                   string `json:"cwd,omitempty"`
	ApprovalPolicy        string `json:"approvalPolicy,omitempty"`
	Sandbox               string `json:"sandbox,omitempty"`
	Model                 string `json:"model,omitempty"`
	DeveloperInstructions string `json:"developerInstructions,omitempty"`
	// Config holds per-thread config overrides (dotted keys, like `-c`).
	Config map[string]any `json:"config,omitempty"`
}

type threadResumeParams struct {
	ThreadID       string         `json:"threadId"`
	Cwd            string         `json:"cwd,omitempty"`
	ApprovalPolicy string         `json:"approvalPolicy,omitempty"`
	Sandbox        string         `json:"sandbox,omitempty"`
	Model          string         `json:"model,omitempty"`
	ExcludeTurns   bool           `json:"excludeTurns,omitempty"`
	Config         map[string]any `json:"config,omitempty"`
}

type threadSetNameParams struct {
	ThreadID string `json:"threadId"`
	Name     string `json:"name"`
}

// threadInjectItemsParams appends raw Responses API items to a thread's
// model-visible history.
type threadInjectItemsParams struct {
	ThreadID string         `json:"threadId"`
	Items    []responseItem `json:"items"`
}

type responseItem struct {
	Type    string            `json:"type"`
	Role    string            `json:"role"`
	Content []responseContent `json:"content"`
}

type responseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type configValueWriteParams struct {
	KeyPath       string `json:"keyPath"`
	MergeStrategy string `json:"mergeStrategy"`
	Value         any    `json:"value"`
}

type threadResponse struct {
	Thread thread `json:"thread"`
}

type userInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type turnStartParams struct {
	ThreadID            string      `json:"threadId"`
	Input               []userInput `json:"input"`
	ClientUserMessageID string      `json:"clientUserMessageId,omitempty"`
}

type turnStartResponse struct {
	Turn turn `json:"turn"`
}

type turnSteerParams struct {
	ThreadID            string      `json:"threadId"`
	ExpectedTurnID      string      `json:"expectedTurnId"`
	Input               []userInput `json:"input"`
	ClientUserMessageID string      `json:"clientUserMessageId,omitempty"`
}

type turnSteerResponse struct {
	TurnID string `json:"turnId"`
}

type turnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type threadIDParams struct {
	ThreadID string `json:"threadId"`
}

type turnsListParams struct {
	ThreadID      string `json:"threadId"`
	Cursor        string `json:"cursor,omitempty"`
	Limit         int    `json:"limit,omitempty"`
	ItemsView     string `json:"itemsView,omitempty"` // full
	SortDirection string `json:"sortDirection,omitempty"`
}

type turnsListResponse struct {
	Data       []turn `json:"data"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// item is the subset of ThreadItem fields Pyre reads.
type item struct {
	Type          string            `json:"type"`
	ID            string            `json:"id"`
	ClientID      string            `json:"clientId,omitempty"`
	Content       []json.RawMessage `json:"content,omitempty"`
	Text          string            `json:"text,omitempty"`
	Command       string            `json:"command,omitempty"`
	ExitCode      *int              `json:"exitCode,omitempty"`
	Status        string            `json:"status,omitempty"`
	Kind          string            `json:"kind,omitempty"`
	AgentThreadID string            `json:"agentThreadId,omitempty"`
	AgentPath     string            `json:"agentPath,omitempty"`
	Server        string            `json:"server,omitempty"` // mcpToolCall
	Tool          string            `json:"tool,omitempty"`   // mcpToolCall
	Changes       []struct {
		Path string `json:"path"`
	} `json:"changes,omitempty"`
}

func (it item) userText() string {
	var out string
	for _, c := range it.Content {
		var u userInput
		if json.Unmarshal(c, &u) == nil && u.Type == "text" {
			if out != "" {
				out += "\n"
			}
			out += u.Text
		}
	}
	return out
}

type itemNotification struct {
	Item     json.RawMessage `json:"item"`
	ThreadID string          `json:"threadId"`
	TurnID   string          `json:"turnId"`
}

type turnNotification struct {
	ThreadID string `json:"threadId"`
	Turn     turn   `json:"turn"`
}

type statusNotification struct {
	ThreadID string       `json:"threadId"`
	Status   threadStatus `json:"status"`
}

type resolvedNotification struct {
	ThreadID  string          `json:"threadId"`
	RequestID json.RawMessage `json:"requestId"`
}

type errorNotification struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	Error     turnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

type threadStartedNotification struct {
	Thread thread `json:"thread"`
}

// approvalParams covers item/*/requestApproval and item/tool/requestUserInput.
type approvalParams struct {
	ThreadID           string            `json:"threadId"`
	TurnID             string            `json:"turnId"`
	ItemID             string            `json:"itemId"`
	Command            string            `json:"command,omitempty"`
	Cwd                string            `json:"cwd,omitempty"`
	Reason             string            `json:"reason,omitempty"`
	GrantRoot          string            `json:"grantRoot,omitempty"`
	AvailableDecisions []json.RawMessage `json:"availableDecisions,omitempty"`
	Permissions        json.RawMessage   `json:"permissions,omitempty"`
	Questions          []struct {
		ID       string `json:"id"`
		Header   string `json:"header"`
		Question string `json:"question"`
		Options  []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options,omitempty"`
	} `json:"questions,omitempty"`
}
