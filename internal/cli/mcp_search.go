package cli

import (
	"context"

	c "github.com/jhh3/pyre/internal/contracts"
)

func init() { mcpToolProviders = append(mcpToolProviders, searchMCPTools) }

// searchMCPTools: the read-only global search, so agents can find past work
// across every session and machine (pre-approved in Claude runs).
func searchMCPTools() []MCPTool {
	return []MCPTool{
		compactly(tool("search", "Search everything", "Find past work: search every session transcript on every machine, plus memory, notes and docs. Hits give session, run and seq; read_session with cursor seq-1 opens the turn. Matches are marked « ».",
			obj(props{
				"query":    str("words to find (all must match; \"quoted phrase\" for exact)"),
				"node":     str("search one machine only (name or ID)"),
				"since":    str("only newer matches: 7d, 36h, 90m or a date"),
				"limit":    num("max results (default 40)"),
				"archived": boo("include archived sessions"),
				"sources":  strs("limit to: transcript, memory, note, document, artifact"),
			}, "query"),
			hints("Search everything", readOnly, true),
			func(ctx context.Context, o *Ops, p SearchParams) (c.SearchResponse, error) { return o.Search(ctx, p) }), renderSearch),
	}
}
