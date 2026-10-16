# Pyre agent tools

Agents use the same operations as humans, through either the `pyre` CLI
(`--json` on every command) or the `pyre mcp` stdio server. There is no
separate agent API, no required workflow, and no coordinator role.

| File | Purpose |
| --- | --- |
| `pyre-mcp-tools.json` | Tool manifest (`pyre mcp --list-tools`); kept in sync by a test |
| `claude-mcp.json` | `claude --mcp-config agents/tools/claude-mcp.json` |
| `codex-mcp.toml` | Codex `mcp_servers.pyre` snippet (or `-c` flags) |

The short working guide that sessions receive is `pyre guide`
(`internal/agentguide`); the MCP server sends the same text as its
`instructions`.

## Identity and connection inside a session

The node daemon starts sessions with `PYRE_SOCKET`, `PYRE_SESSION_TOKEN`,
`PYRE_SESSION_ID`, `PYRE_RUN_ID` and `PYRE_NODE_ID`. Both the CLI and the MCP
server pick these up: calls go to the local daemon and act as the session
(the hub records it as the sender; a session cannot claim another identity).
`pyre inbox` defaults to the session's own inbox, `pyre start` records the
caller as the peer's parent, and `pyre publish` stamps session/run/node
provenance.

Resolution order: `--hub`/`PYRE_HUB`, then `PYRE_SOCKET`, then
`~/.config/pyre/client.json`. Credential: `PYRE_SESSION_TOKEN`, then
`PYRE_TOKEN`, then the client config.

## Try it against the mock

```sh
go run ./cmd/pyre-mockhub --token dev-token &      # synthetic data, labelled MOCK
export PYRE_HUB=http://127.0.0.1:7788 PYRE_TOKEN=dev-token
```

Every line below runs against the mock (`TestReadmeExamples` executes them):

```sh
pyre hello
pyre --json nodes
pyre sessions --node synth-mac
pyre sessions show ses_synth-b
pyre start --node synth-mac --harness claude --cwd /home/synth/peer --title "peer review" --prompt "review the report"
pyre read ses_synth-b --tail 20
pyre send ses_synth-b "please review ver_synth-report-2" --attach ver_synth-report-2 --request-ack
pyre inbox --unacked
pyre msg show msg_synth-indeterminate
pyre approvals
pyre comments ver_synth-report-2
pyre comment ver_synth-report-2 "section 2 needs numbers" --anchor line:12-18
pyre note "handoff: report review" --handoff --body "reviewed ver_synth-report-2; comments posted"
pyre context --project prj_synth-outfit
pyre doc show brief --project prj_synth-outfit
pyre memory search "brief" --node synth-mac
pyre search "synthetic task" --since 7d
pyre accounts
pyre connections
pyre attention
pyre guide
```

Exit codes are stable (`pyre exit-codes`): 0 ok, 1 error, 2 usage,
3 conflict, 4 not found, 5 unavailable/timeout, 6 unsupported,
7 indeterminate. With `--json`, stdout carries the contract type from
`internal/contracts` and errors are an `{"error":{...}}` body on stderr.

## Messages and uncertain delivery

Input that starts with `[pyre-msg:<id>]` is a Pyre message. Reply with
`pyre send --reply-to <id> "..."` (recipient and thread are inferred).
A message has separate facts: stored on the hub, accepted by the recipient
runtime (with evidence), acknowledged/replied. `indeterminate` means the
outcome is unknown: inspect it, then `pyre resolve-delivery <id>
accepted|rejected`, or `pyre reissue <id> --accept-duplicate-risk`. Never
resend blindly.

## MCP

```sh
claude --mcp-config agents/tools/claude-mcp.json
codex -c 'mcp_servers.pyre.command="pyre"' -c 'mcp_servers.pyre.args=["mcp"]'
```

Tool errors come back as `isError` results whose text is
`{"error": <contracts.Error>, "exit_code": n}`, matching the CLI.

Protocol: the server negotiates MCP 2025-11-25 (also 2025-06-18 and
older). It deliberately does not offer 2026-07-28: Claude Code does not
register a channel server on that revision, and a stdio client probing it
(`server/discover`, e.g. Claude Code with `MCP_PROTOCOL_NEGOTIATION=auto`)
gets -32601 and falls back to the legacy handshake. Every tool has a
title and explicit readOnly/destructive/idempotent/openWorld hints. Tools
that act have an `outputSchema` derived from their contract result type;
successful results carry conforming `structuredContent`.
`internal/cli/mcp_conformance_test.go` checks this with the official MCP Go
SDK client.

Read tools that agents call often (`inbox`, `get_message`, `list_sessions`,
`get_session`, `read_session`, `list_nodes`, `list_approvals`,
`list_projects`, `search`, `get_digest`) answer in compact text by default:
one line per item with the ids a follow-up call needs (`[pyre-msg:<id>]`
per message, session ids, `next_cursor`), no receipts or freshness records.
Listings shorten long message bodies but keep paths and links whole;
`get_message` shows the full body. `format: "json"` returns the full
contract result with `structuredContent`. These tools declare no
`outputSchema`, because their default result is text. The MCP `inbox`
lists unacked mail unless `unacked: false` (or a thread, outbox or all) is
requested.
