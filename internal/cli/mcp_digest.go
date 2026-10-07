package cli

import (
	"context"
	"os"
	"time"

	"github.com/jhh3/pyre/internal/clientconn"
	"github.com/jhh3/pyre/internal/digest"
)

func init() { mcpToolProviders = append(mcpToolProviders, digestMCPTools) }

// mcpDigestStatePath locates the owner's last-seen mark on this machine
// (beside the client config). The MCP tool only reads it.
var mcpDigestStatePath = func() string { return digest.StateFile(clientconn.DefaultConfigPath(os.Getenv)) }

func digestMCPTools() []MCPTool {
	return []MCPTool{
		tool("get_digest", "While you were away",
			"Catch the owner up: per-agent digest since they last looked, ranked needs-you, done, working: what each did, files/docs/artifacts/messages, blockers, a pyre:// link per line. Read-only.",
			obj(props{"since": str("\"last\" (the owner's last-seen mark on this machine, else 12h; default), a duration such as 2h, 90m or 1d, or an RFC 3339 time"),
				"no_transcripts": boo("skip transcript reads (faster; less detail)")}),
			hints("While you were away", readOnly, true),
			func(ctx context.Context, o *Ops, p DigestParams) (digest.Digest, error) {
				return o.Digest(ctx, p, mcpDigestStatePath(), time.Now())
			}),
	}
}
