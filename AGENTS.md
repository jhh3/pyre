# Working in Pyre (for coding agents)

- Read README.md, CONTRIBUTING.md and the doc for the area you change (docs/operations.md,
  docs/contracts.md, tests/tui/DESIGN.md) before editing.
- Build with `make build`; run `make check` before proposing a change, plus the targeted tests.
- Tests must be hermetic: isolated config homes, state dirs, tmux sockets, databases, ports and
  obviously synthetic credentials. Never touch the user's ~/.config/pyre, default tmux server,
  ~/.claude or ~/.codex logins, or keychain.
- Never log or commit tokens, transcripts, hostnames or other private data.
- Stored messages, runtime acceptance and task completion are different facts. Never replay an
  instruction whose outcome is unknown; never infer account isolation from a copied credential.
- Preserve native harness behavior (subagents, skills, plugins, settings); advertise unsupported
  capabilities instead of silently changing behavior.
- Update docs when behavior changes; regenerate fixtures, schemas, goldens and the MCP manifest
  when affected. Run `python3 scripts/check_docs.py` after documentation changes.
