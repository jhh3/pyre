package cli

import (
	"context"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, auditMCPTools) }

// auditMCPTools: the read-only audit log (W-audit).
func auditMCPTools() []MCPTool {
	return []MCPTool{
		tool("audit_log", "Audit log",
			"Audit log, newest first: account chosen per run and why, automatic account switches (from/to, usage, reason), re-auth, cooldowns, tokens, session starts/stops/renames by whom, machines and upgrades. Read-only; no secrets.",
			obj(props{
				"since":   str("how far back: 24h (default), 90m, 7d, an RFC 3339 time, or all"),
				"kind":    str("only account, session or fleet events"),
				"session": str("only this session (ID or exact title)"),
				"node":    str("only this machine (ID or name)"),
				"account": str("only this account (ID or label)"),
				"limit":   num("max entries (default 200)"),
			}),
			hints("Audit log", readOnly, true),
			func(ctx context.Context, o *Ops, p AuditParams) (c.Page[c.AuditEntry], error) {
				return o.Audit(ctx, p, time.Now())
			}),
	}
}
