# Outfit evals

## Agent routing

This suite measures whether a real Codex or Claude Code session chooses the blessed Outfit connection, selected mode, canonical operation, operation class, and explicit write boundary for representative user tasks. It does not call providers: the eval asks the agent to route only. The candidate is copied to a content-addressed, host-readable staging path; its state is snapshotted into an isolated `OUTFIT_HOME`; and its generated guidance is synchronized without changing the user's live connections.

Run a baseline and candidate:

```sh
python3 scripts/eval-agent-routing.py run --host codex --trials 3 --label baseline --output /tmp/outfit-baseline.jsonl

# Change a connector contract, generated skill, or plugin prompt, then install/sync it.
outfit skill sync --host both

python3 scripts/eval-agent-routing.py run --host codex --trials 3 --label candidate --output /tmp/outfit-candidate.jsonl
python3 scripts/eval-agent-routing.py compare /tmp/outfit-baseline.jsonl /tmp/outfit-candidate.jsonl
```

Use `--host claude` to exercise Claude Code, `--case ID` for one scenario, and `--limit N` for a cheap smoke pass. `--trials N` repeats every decision in an independent cold-start session so a lucky answer cannot hide unstable routing. Cases whose required connection is not active are reported as skipped rather than failed. `--outfit PATH` evaluates an uninstalled candidate binary while still requiring proposed portable commands to begin with `outfit`. The scorer retains the host's actual command trace, requires the staged candidate to be inspected, and rejects provider-capable calls during a routing-only eval.

Exercise every selected-mode link route without real credentials:

```sh
go build -o /tmp/outfit-candidate ./cmd/outfit
python3 scripts/eval-agent-routing.py matrix --host codex \
  --outfit /tmp/outfit-candidate --output /tmp/outfit-mode-matrix.jsonl
```

`evals/mode-matrix.jsonl` covers all 15 blessed GitHub, Google Workspace, Intercom, Linear, Notion, Sentry, Slack, and Stripe link-capable modes. `scripts/eval-state` builds the isolated package/connection/tool surface used by the matrix; it never copies credentials.

Exercise agent recovery choices for expired OAuth, missing scopes, ambiguous links, disabled enrichment, missing pinned CLIs, and provider throttling:

```sh
python3 scripts/eval-agent-routing.py recovery --host codex \
  --outfit /tmp/outfit-candidate --output /tmp/outfit-recovery.jsonl
```

Recovery cases require the agent to inspect live candidate help, choose a non-destructive supported command, and say whether the original operation should be retried.

Every completed case/trial is atomically checkpointed. If a host process is interrupted, rerun the same command with `--resume`: passed, semantically failed, and skipped decisions are preserved, while operational `error` rows are retried. `--host-attempts` bounds timeout/exit/malformed-output retries (default 2), and `--host-timeout` controls each attempt. Each row retains elapsed time plus exact `host_timeout`, `host_exit`, `host_output_invalid`, or runner diagnostics, so infrastructure failures are not misreported as routing misses.

The scorer checks the connection, literal selected mode ID, canonical operation, class, explicit write boundary, required task values, callable command route, and inspection trace. Every non-URL proposal is executed through Outfit's credential-free `--dry-run` planner, so a plausible-looking command with a wrong flag, missing required input, or invalid alias fails without calling the provider. URL proposals must use the selected-mode `outfit get URL` abstraction and preserve the exact URL.

The comparison exits nonzero when the candidate loses any previously passing trial or lowers the aggregate score. This makes a prompt, skill, or operation-description hill climb mechanical: keep candidate changes only when wins are additive and losses are zero. Raw model decisions, trial numbers, exact expected values, planner errors, and required task values remain in JSONL for diagnosis.

When the case contract or scorer improves, reapply it to an old artifact before comparing it with a new run:

```sh
python3 scripts/eval-agent-routing.py rescore /tmp/outfit-baseline.jsonl \
  --outfit /path/to/candidate/outfit \
  --output /tmp/outfit-baseline-rescored.jsonl
```

Append routing cases to `evals/routing.jsonl` with a stable `id`, natural user `prompt`, expected connection, canonical operation, operation class, and whether the final command must carry explicit write intent. CI runs `--self-test`, which validates cases, response parsing, scoring, and regression comparison without invoking a model.

## Deterministic execution and failure states

The product evals execute the same connector packages, credential bindings,
runtime, pagination, link resolver, heavy-read recursion, and trust-framed context
renderer used by the CLI and host hooks. Provider-shaped fixtures are local so a
release gate never depends on mutable third-party accounts or network state.

```sh
go test ./internal/linkcontext -run 'TestBlessedExecutionEval|TestEveryBuiltinLinkPattern|TestMultipleAccounts|TestLinkFailureDiagnostics|TestFailureEval'
```

This suite executes every declared blessed resource resolver: 22 resource types
across Slack, Notion, Sentry, all five GitHub resources, Linear, Intercom, all
seven Stripe resources, and all five Google Workspace resources through the
pinned CLI boundary. A mechanical coverage assertion fails whenever a resolver
is added without provider-shaped execution evidence. It also covers pagination,
heavy-read recursion, every URL pattern, multi-account ambiguity, and
disconnected/unhealthy/expired/unsupported recovery. It complements live
`outfit connector test` verification; it does not pretend a fixture proves a
user's current credential is healthy.
