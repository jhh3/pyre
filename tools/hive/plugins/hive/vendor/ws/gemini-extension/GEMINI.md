# ws — work-stream registry

This machine runs the `ws` work-stream registry (vault at `$WS_ROOT`, default
`~/streams`). You may be working inside a registered stream — the SessionStart
hook tells you which.

Rules of the road:

- **Mail**: when a hook or a 📬 banner says you have messages, run
  `ws inbox read` (or the `ws_inbox` MCP tool) and ACT on them. Reply to
  messages marked "reply expected" with
  `ws msg <sender> --reply-to <id> "..."` before going idle.
- **Keep the record current**: `ws log "..."` at milestones,
  `ws update --next "..."` when the plan changes, `ws task done <id>` when a
  milestone completes.
- **Register** long-lived work: `ws register <slug>` when the work will
  outlive this session or span multiple PRs; then write VISION.md.
- **Reach others**: `ws msg <stream|hq|#channel> "..."` — `hq` is the human's
  mailbox; use `--kind question --expect-reply` when you're blocked on an
  answer.

`ws help` is the full reference.
