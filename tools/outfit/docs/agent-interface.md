# Agent interface

The typed `outfit` CLI is the primary interface for Codex, Claude Code, and other coding agents. Connector Kit compiles the selected connection mode into one operation schema shared by command help, validation, execution, generated skills, link enrichment, and the compact MCP fallback.

## Automatic structured output

When stdout is a real non-terminal file or pipe, the agent-facing commands `outfit ls`, `search`, `describe`, `doctor`, `get`, and every connector operation return one `outfit.jhh3.dev/result/v1` JSON envelope. Interactive terminals keep concise human output. Use `--json` to force structured output or `OUTFIT_AGENT=0` to force human output in a pipeline.

Successful and failed operation calls share the same envelope. Failures carry a stable code, corrective hint, and operation help command; the process still exits nonzero. The launcher does not append a second plain-text error after a structured failure.

## Large results

Inline agent output is bounded independently of provider pagination. When a serialized result exceeds 64 KiB, Outfit writes the complete original envelope to an OS temp file with mode `0600`. The inline envelope preserves provenance and returns:

```json
{
  "data": {"spilled": true, "path": "/tmp/outfit-result-....json"},
  "meta": {
    "spill": {
      "path": "/tmp/outfit-result-....json",
      "bytes": 123456,
      "sha256": "..."
    }
  }
}
```

The agent must read the file before reasoning from the result. The spill contains the original complete envelope, not a second payload format. `OUTFIT_AGENT_MAX_BYTES` adjusts the boundary between 4 KiB and 10 MiB. Explicit `--json` in an interactive terminal remains inline unless automatic agent mode is also active. The local MCP fallback uses the same spill contract because it runs on the same machine.

## Routing quality

`evals/routing.jsonl` and `scripts/eval-agent-routing.py` run real Codex or Claude Code routing decisions without calling a provider. The scorer checks exact connection, canonical operation, class, and explicit write intent. Baseline/candidate comparison rejects any loss and supports incremental skill and prompt hill climbing. See [the eval guide](../evals/README.md).

## Execution boundary

The compact MCP facade exposes only discovery, description, read dispatch, and URL retrieval. It does not duplicate every connector operation. Agents should discover with `outfit search`, inspect with `outfit describe` or typed `--help`, and execute the typed CLI. Mutations remain CLI-only and require `--write` or the `outfit write` namespace after explicit human intent.

For an exact supported provider URL, `outfit get URL` is the preferred route before manually decomposing the URL into connector arguments. `outfit search` and `outfit describe CONNECTION` expose these selected-mode link routes and their underlying operation. For ordinary operations, both the friendly hyphenated name and canonical operation ID are callable; schema-backed arguments accept flags, `KEY=value`, camel/snake spelling variants, and a bare value when the operation has exactly one required input. `--dry-run` validates the complete command without reading a credential or contacting the provider.
