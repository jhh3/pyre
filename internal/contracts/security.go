package contracts

import "time"

// EvSecurityAlert (additive) is a node-reported event (ReportEvent): a
// run's tool call looked like a credential being sent out, or used a
// planted decoy credential (decision I79). Payload SecurityAlert. The hub
// audits every alert (AuditSessionSecretEgress) and raises an urgent
// AttnSecurity inbox item for a "high" one. Flag only: the command ran.
const EvSecurityAlert EventKind = "security.alert"

// AttnSecurity (additive): a high-severity security alert in a session.
// One item per session (repeats count); pushed even in quiet hours.
const AttnSecurity AttentionKind = "security"

// AuditSessionSecretEgress (additive): a security alert (Reason: severity,
// rule, the matched sources/sinks/hosts and the redacted command).
const AuditSessionSecretEgress = "session.secret_egress"

// SecurityAlert is one flagged tool call.
type SecurityAlert struct {
	// AlertID is stable for the tool call (the hook record or Codex item),
	// so a re-observed alert is reported once.
	AlertID   string  `json:"alert_id"`
	RunID     string  `json:"run_id,omitempty"`
	SessionID string  `json:"session_id,omitempty"`
	NodeID    string  `json:"node_id,omitempty"`
	NodeName  string  `json:"node_name,omitempty"`
	Harness   Harness `json:"harness,omitempty"`
	Tool      string  `json:"tool,omitempty"` // Bash, PowerShell, commandExecution, an MCP tool
	// Rule: decoy-value, decoy+network, decoy-read, secret+network,
	// env-dump+network (internal/secretegress; secret+provider is no
	// longer produced, decision I88).
	Rule     string   `json:"rule"`
	Severity string   `json:"severity"`          // "high" | "low"
	Secrets  []string `json:"secrets,omitempty"` // names and kinds, never values ("$GH_TOKEN", "gh auth token")
	Sinks    []string `json:"sinks,omitempty"`
	Hosts    []string `json:"hosts,omitempty"`
	Encoded  bool     `json:"encoded,omitempty"`
	// Command is the tool call's command with credential values redacted.
	Command string    `json:"command,omitempty"`
	Reason  string    `json:"reason"`
	At      time.Time `json:"at"`
}
