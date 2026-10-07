# Hive — Unified Session Memory, Workstreams, Worktrees & Lifecycle

*Final plan v2. Produced by a 5-lens brainstorm → synthesis → two adversarial critiques, with critique fixes folded in. v2 amendments (2026-08-27): `wt` unification, raw-transcript sync, agent-deck removal, resource-safety guardrails from the claude-mem post-mortem, and the owner's answers to the five open questions.*

---

## 0. Decisions on record (owner, 2026-08-27)

1. **Trust boundary:** private personal repo(s) are fine — no encryption layer needed.
2. **claude-mem post-mortem:** it crashed the machine (resource exhaustion), was more marketing than substance, maintainers untrustworthy. → Guardrails re-ranked: **resource safety and auditability first** (§8). Hive is 100% the owner's own code, no third-party memory services, no resident daemons, no vector-DB server — sqlite-vec in-process only, hard resource caps on everything background.
3. **Sync payload:** **raw transcripts sync** (the owner: "much better for resuming and/or searching") — see §4 raw lane. Distilled lanes remain the fast query/injection layer; raw is the fidelity layer.
4. **agent-deck: removed entirely.** Already retired in daily use (2026-07-29, per wt.zsh header) in favor of `ws serve`; hive finishes the job — its hooks leave `~/.claude/settings.json`, its code paths leave `wt`, and hive becomes the sole naming/observation authority. The old "title authority conflict" question dissolves.
5. **`ask` default scope** = current-repo-boosted-global: `hive ask` searches all repos' memory but ranks the current repo's entries higher, so cross-repo answers (shared infra, tooling decisions made elsewhere) still surface. `--repo only` for hard scoping.

---

## 1. Product definition

**Name:** `hive` (working name).

**Pitch.** One plugin + CLI unifying **jhh3-mem** (episodic memory), **ws** (workstreams, mail, watchdog, dashboard), **streams-vault** (git sync), and **wt** (worktree + tmux session lifecycle). Every agent session — Claude Code or Codex, on any machine — is *born registered, continuously checkpointed, enriched within minutes, and synced (distilled + raw) to private git vaults without a human ever typing a verb*. `wt` is the birth-and-death channel; streams are the human-facing spine; episodic memory is the queryable record; one identity ledger ties slug ↔ session ↔ worktree ↔ machine ↔ tmux so that parking work on one machine and typing `wt <branch>` on another is a warm start.

**The core loop:**
1. **Observe** — `wt`/`ws spawn` register at birth with full intent (repo, branch, Linear issue); SessionStart covers sessions born outside wt; Stop-hook checkpoints upsert a live entry; the tick sweep catches everything hooks miss.
2. **Bank** — PreCompact and SessionEnd run deterministic capture; the raw lane mirrors transcript bytes; nothing is destroyed silently.
3. **Enrich** — event-driven, machine-local `claude -p` within ~1–2 min of close/compaction; crons demoted to janitor phases inside the tick.
4. **Sync** — distilled entries *and live checkpoints* append to per-machine NDJSON lanes; raw transcript segments mirror to the raw vault; other machines import idempotently.
5. **Serve** — SessionStart digests, `hive ask` citations, `hive grep` (fleet-wide), `wt`-triggered resume-context, live tmux titles: all projections of the same data.

Manual verbs (`hive done`/`park`/`pin`) are an intent channel, not a safety mechanism — plus automation suggestions (PR merged → "done `foo`?").

## 2. What unifies (and how)

One plugin repo (`jhh3/hive`), one `hooks.json`, one CLI, one skill, one MCP registration. `ws`, `jhh3-mem` ship as alias shims; `wt` remains a thin zsh function (it must stay a shell function to `cd` your shell) that delegates everything non-interactive to `hive wt …`. Internals: four modules with separate failure domains — `hive.mem` (capture/enrich/index), `hive.ws` (streams/mail/watchdog/serve), `hive.wt` (worktrees/tmux/session birth-death), `hive.core` (identity ledger, queue, lanes/sync, hook harness, tick). Staged code convergence, not a rewrite: milestone 1 is a wrapper; the modules migrate onto `hive.core` incrementally.

### `hive wt` — the birth and death channel

`wt` today: create worktree off origin/main (+ make setup), open/attach tmux session named after the worktree dir, Linear-URL → `alice/eng-NNNN` branch → auto-launch claude, `wt rm` kills session + removes worktree, multi-repo registry. Unification makes those moments do the right hive things:

- **Birth = registration.** `wt <branch|linear-url>` creates worktree + tmux session *and* registers the stream (slug = branch), stamps the ledger (repo, worktree, branch, tmux session, machine, harness once claude starts), and stores the Linear ref in stream frontmatter. No inference needed — wt *knows* the intent. Auto-registration (§6) remains the fallback for sessions born outside wt.
- **One namespace.** tmux session name = stream slug = worktree dir name. The three naming authorities collapse to one (agent-deck removed; `ws-<slug>` sessions renamed to match).
- **Death = archive.** `wt rm` today destroys context (kills tmux, removes worktree, no snapshot). Amended: `wt rm` → close pipeline first — final capture, priority enrichment, handoff doc, stream → done/parked — then kill/remove. A `--keep-branch`/`--park` variant archives without deleting the branch. The destructive tail (branch delete) stays behind the existing y/N prompt.
- **Cross-machine `wt` = the machine-switch verb.** On machine B, `wt <branch>` for a stream that lives elsewhere: recreates the worktree from the stream record (repo, branch, fetch), opens the tmux session under the same slug, and either **imports the raw transcript for a true `claude --resume`** (§4 raw lane; cctrace-style placement with path mapping) or falls back to a fresh session with `resume-context` injected. "Park on the desktop, `wt` on the laptop" becomes one command.
- **`wt ls`** gains ledger columns: live/parked, current title, machine, last activity — replacing the dead `[deck]` indicator.
- `ws spawn` and `wt -d` merge onto the same primitive (`hive wt open --detached`); wt is the human front door, ws spawn the programmatic one.

### agent-deck removal (M1 task)

Remove its seven hook entries from `~/.claude/settings.json` (hive's harness replaces the useful ones), delete the `WT_USE_DECK` paths from wt.zsh, keep `claude-hook-notify` for notifications. Fleet visibility = `ws serve` + `wt ls` + `hive doctor`. No absorption of the Go TUI — it's retired, not ported.

## 3. Process topology

**Two actors.** (1) **Hooks** — bounded synchronous capture + enqueue only; no LLMs, no network, no big file copies. (2) **`hive tick`** — one cron (*/5), one lock, one log, sequential phases: sweep (uncaptured transcript bytes, both harnesses) → reap (dead sessions → close pipeline) → enrich (machine-local queue drain, `claude -p` haiku-class, cap 2) → sync (distilled lanes + raw segments, pull-rebase-push) → janitor (requeue, decay, embed, commit-xref; nightly-gated phases in-process). Losing every hook loses latency, never data. The tick is the entire Codex capture story. **Boot:** one `@reboot` script: reap → single vault pull → `ws up` resume ladder → interruption briefs.

**Resource safety (the claude-mem lesson, Q2):** the tick and every enrichment run inside a `systemd-run --scope` with `MemoryMax` (e.g. 2G) and `CPUQuota`, `nice`/`ionice`'d; hard wall-clock budgets per phase; no resident processes between ticks; embeddings via in-process fastembed (bounded batch), never a vector-DB server. `hive doctor` shows last-tick resource peaks. If hive ever OOMs, it OOMs its own scope — never the machine. (the owner's box already runs zram + swapfile; PSI pressure from a memory plugin is how claude-mem died here. Never again.)

## 4. Architecture

### Storage

**Two vaults, both private GitHub repos (Q1: no encryption layer):**

1. **`streams-vault` (extended)** — the distilled, small, always-mirrored layer:
   - `streams/` (as today) · `mem/lanes/<machine>/<YYYY-MM>.ndjson` (append-only entry revisions) · `mem/tombstones/` · `handoffs/<slug>/…md` · `meta/` (machines, schema, epoch).
2. **`vault-raw` (new, Q3)** — the fidelity layer: raw transcript mirrors.
   - `raw/<machine>/<harness>/<session-id>/<seq>.jsonl.zst` — transcripts are append-only, so the tick syncs **zstd-compressed segments** (new bytes since last segment) plus a small `manifest.json` (cwd, repo, slug, byte map, harness version). Live sessions mirror on the same ~10–15 min debounce as checkpoint lanes.
   - Enables: **fleet-wide `hive grep`** (search exact errors/flags/SQL across every machine's sessions), **cross-machine resume** (`hive raw import <session>` reassembles the JSONL into `~/.claude/projects/<mapped-cwd-hash>/` with path rewriting — the tawanorg/claude-sync trick — then `claude --resume`; Codex likewise into `~/.codex/sessions`), and re-parse after a format-fix without the original machine.
   - Kept separate from streams-vault so `hive rebuild` of the index stays fast; machines mirror vault-raw fully by default (disk is cheap), `--shallow` for constrained boxes fetches manifests only + segments on demand.
   - **Redaction policy differs by layer:** distilled lanes + handoffs are masked (they feed injection and `ask`); the raw lane syncs unredacted to the private repo — resume fidelity requires it and Q1 accepts it — with a gitleaks-class *warn-only* scan so a leaked live credential still gets flagged for rotation.
3. **Derived, local, rebuildable:** `~/.local/share/hive/index.db` — SQLite WAL, FTS5 + sqlite-vec over distilled entries, plus a lightweight FTS index over raw segments (rebuildable from vault-raw). `hive rebuild` = shallow-clone vaults + reindex; FTS immediately, embeddings backfill async.

**Still rejected:** syncing SQLite in any form · PostToolUse content capture · LLMs in hook paths · per-entry markdown files.

### Lanes, conflicts, git hygiene

Lane record `{op, uid, rev, kind, stream_slug, session_ref, fields…, ts}`; `rev` monotonic; enrichment appends `rev+1`. **Single-writer-per-lane is the entire conflict strategy** (raw segments are per-machine too — same property). Vault history is disposable: monthly squash + force-push with `meta/epoch` bump; importers reset on epoch change and refuse to push across it (this is also what makes post-leak `hive scrub` stick). Shallow clones; pull-rebase-retry; single-flight pulls. Pre-flight before M4: measure what the 793MB DB decomposes to; vault-raw growth gets a monthly size report in doctor (segments of old scratch sessions are prunable by policy, non-scratch kept indefinitely until the owner says otherwise).

### Identity

`stream_slug` (human-facing; = worktree dir = tmux session) · `session_ref = (harness, session_id, machine)` · entry `uid = <machine-short>-<ULID>` (durable citations).

```
sessions(session_id, harness, machine, repo_key, worktree, branch,
         stream_slug, tmux_session, transcript_path, title,
         started_at, last_activity, last_checkpoint_seq, ended_at,
         end_reason[clean|compacted|crashed|reaped|interrupted],
         predecessor_session_id, raw_synced_through)
```

Synced via `kind=session_meta` lane records. `commit_xref (sha, pr, session_id, uid, slug)` from the `Claude-Session:` trailers already in months of git history (incremental `git log --grep` sweep; Codex gets the trailer via commit template) → `hive why <path>`, `hive session <sha|pr>`, auto-done suggestions. Cloud sessions: observed as `machine=cloud`, not captured (v1 non-goal).

### Live-session sync

Significant Stop-checkpoints append lane revs (not just local upserts) and raw segments mirror on a ~10–15 min debounce — a dead machine loses minutes, not days; the morning brief and `hive ask`/`hive grep` see in-flight work everywhere (staleness-labeled). Enrichment is strictly machine-local (`hive enrich --uid` = manual anywhere-escape).

## 5. Automation surface

Rules: hooks enqueue, tick executes; every stage idempotent on `(session_uid, checkpoint_seq)`; sweep is the guarantee, hooks the accelerators; transcript JSONL is the WAL, checkpoints at turn boundaries.

| Event (Claude Code) | Synchronous (budgeted, fail-open) | Queued for tick |
|---|---|---|
| SessionStart (startup/resume) | Ensure stream + ledger row; inject digest + stream context from **local state only** (~3KB, one merged injector) | Debounced background pull; backfill; title refresh |
| SessionStart (compact) | + inject `compact-snapshot.md` (≤2KB) | — |
| SessionStart (resume post-restart) | + interruption brief | — |
| UserPromptSubmit / PostToolUse | ws inbox delivery; heartbeat; throttled files/commands ledger — nothing more | — |
| Stop | Gated checkpoint (≥8KB new or ≥10min): incremental capture upsert ~100ms; title derivation | Significance-gated enrich + lane append + raw segment |
| PreCompact | Capture of the doomed span; compact-snapshot; **(path, byte_offset, mtime) + hardlink — never a synchronous copy** | Priority enrich; async raw segment |
| SessionEnd | Final capture ~140ms; mark ended | Priority enrich → lanes + raw → debounced push; handoff |

Fail-open harness on every hook: hard timeouts, exit 0 always, stderr to `~/.local/state/hive/hooks.log`, `HIVE_DISABLE=1`, per-module breaker with a loud one-line degradation notice. With agent-deck's hooks gone, hive's budget ≈ the total budget: SessionStart p95 <300ms added.

**Codex:** capture via tick sweep of `~/.codex/sessions` (same pipeline; idle/process-gone → close). Injection parity: `wt`/`ws spawn`-wrapped codex prepends digest/resume-context to the initial prompt; generated `AGENTS.md` teaches the MCP verbs; managed hooks adopted when mature. Codex can't get: native rename, compact-snapshot reinjection.

**Auto-registration** (for sessions born outside wt): significance-gated (≥3 files, stateful commands, or ≥45min), no-ledger tier for non-interactive (`claude -p`, no TTY, hive-spawn marker), `scratch` class hidden by default, suggestions-only parking. Never auto-kill an idle session.

**Titles:** deterministic title from qualifying checkpoints (enrichment upgrades it) → `tmux rename-window` + `rename-session` (hive is now sole authority) + stream frontmatter + native session rename where supported. Throttle ≥5min; freeze after 2 identical derivations.

**Manual = intent only:** `hive done`/`park`/`pin`, `wt rm`, plus automation suggestions (PR-merged → done?, idle → park?).

## 6. Query surface

- `hive ask "<q>"` — flagship. Hybrid retrieval (top-20; RRF-fuse BM25 + vec; same-repo ×2 soft boost — **default scope: all repos, current-repo-ranked-higher (Q5)**; kind boosts; ~45-day recency half-life) → haiku-class synthesis with **binary-validated citations**: any cite outside the candidate set or low coverage → discard synthesis, fall back to reranked hits; thin retrieval → forced "not in memory"; 8s timeout → hits. `[m:<uid>]` cites + Sources footer with drill-downs.
- `hive grep <pattern> [--all-machines]` — verbatim search over raw transcripts, now **fleet-wide** via vault-raw (Q3). The exact error/flag/SQL from three weeks ago, any machine.
- `hive search --explain` · `hive show <uid>` · `hive timeline [--stream|--repo|--file|--since|--machine]` · `hive why <path>[:line]` · `hive session <sha|pr>`.
- `hive resume-context <stream>` — machine-switch payload (STREAM.md + LOG + linked entries + handoff, ≤4KB, cited), wired into `wt` cross-machine open, ws resume fallback, and SessionStart-after-elsewhere.
- `hive raw import <session>` — reassemble a synced transcript for true `--resume` on this machine (path-mapped).
- Plus `hive ws …`, `wt` verbs, `note`, `forget`, `wrong <uid>`, `sync`, `rebuild`, `doctor`, `migrate`, `enrich --uid`, `bundle`.

**Agents:** skill-first (explicit triggers: "have we hit this", "why is it built this way", unfamiliar repo, about-to-re-derive; drill-down loop ask → show → act); MCP shim for Codex/non-skill harnesses. **Injection:** deterministic/extractive only, ~3KB, one merged injector. Prompt-time hints: backlog, dark-launched first.

## 7. Transcript retention

Hive manages native GC on both harnesses: raise `cleanupPeriodDays`/Codex equivalents; the raw lane means GC'd local transcripts are no longer data loss (vault-raw holds them); `hive doctor` warns if a referenced transcript is missing locally and `hive raw import` restores it.

## 8. Guardrails (re-ranked per Q2: resource safety first)

| Failure mode | Defense |
|---|---|
| **Resource exhaustion crashes the machine** (what claude-mem did) | systemd-run scopes with MemoryMax/CPUQuota on tick + enrichment; nice/ionice; no resident daemons; no DB servers; in-process embeddings, bounded batches; per-phase wall-clock budgets; doctor reports last-tick peak RSS/CPU; PSI check before heavy phases (skip embed/dream under memory pressure) |
| **Trust/auditability** (claude-mem was hype + sketchy maintainers) | 100% own code in `jhh3/hive`; dependency budget ≈ zstd, fastembed, gitleaks-class scanner; no telemetry, no cloud services; every automated write attributable in `ops_log`; `explain-inject` for every injected byte |
| Hook blocks a live session | Fail-open harness; timeouts; exit 0; breaker + loud degradation notice; `HIVE_DISABLE=1` |
| Wrong context injected | Extractive-only injection; `hive wrong <uid>`; provisional flags until close-enrichment; decay demotes from injection only |
| Hallucinated answers | Binary citation validation + abstention + `--raw` |
| Format drift | Vendored fixtures per harness version; `doctor --contracts`; quarantined parsers; durable record = hook-handed data + captures |
| Secrets | Distilled/handoffs masked + planted-secrets e2e before first push; raw = warn-only scan to a private repo (Q1/Q3 tradeoff, on record); `forget` tombstones; `scrub` epoch-fenced rewrite |
| Enrichment storms | One queue, cap 2, shutdown skips, tick catches up |
| Sync divergence | Single-writer lanes/segments; `rm index.db && hive rebuild`; doctor last-push/pull per machine |
| Registry spam | Significance gate, no-ledger tier, scratch hidden |

**Definition of done** additions to v1 list: a `stress` test drives 10 concurrent sessions + enrichment under `MemoryMax` and the desktop stays responsive (PSI monitored); `wt rm` on a live stream archives before destroying; second-machine `wt <branch>` produces a working resumed-or-briefed session end-to-end.

## 9. Migration

Strangler, every stage revertible: (1) **Attach** — harness wraps existing jhh3-mem + ws hooks; **remove agent-deck hooks** (rollback: re-add). (2) **Capture spine** into the existing DB. (3) **Ledger + wt integration + shadow streams.** (4) **Journal shadow** — export 80,346 entries to lanes; one-shot verification script run twice a day apart; `memory.db.pre-hive` kept forever. (5) **Cutover** — lanes canonical; acceptance = second-machine bootstrap. (6) **Converge** — queue owns enrichment; slug backfill; shims stay. streams-vault gains directories; vault-raw is new; wt.zsh becomes the shim.

## 10. Build sequence

| # | Milestone | Contents | Size |
|---|---|---|---|
| **M1** | **Stop the bleeding** | Fail-open harness; agent-deck hook removal; PreCompact capture + snapshot reinjection; `hive tick` skeleton (sweep/reap/enrich) in a resource-capped scope; close → immediate enrichment; contract fixtures. Spikes: Stop semantics; JSONL across compaction; PreCompact wall-time; systemd scope behavior | ~4–5d |
| **M2** | **Identity + wt + titles** | sessions ledger; `hive wt` (birth registration, rm-archives, ls-from-ledger, deck-path deletion); significance-gated auto-registration + no-ledger tier; Stop checkpointing; reaper; titles (tmux session+window + frontmatter) | ~4–5d |
| **M3** | **Query — on the existing DB** | Ranking layer; `search/show/timeline`; local `hive grep`; **`hive ask`** (binary citation validation + abstention); skill + MCP shim | ~4d |
| **M4** | **Cross-machine** | Sizing pre-flight; distilled lanes + live-checkpoint sync; **raw lane (vault-raw): segment writer/mirrorer, `hive grep --all-machines`, `hive raw import` + path mapping**; redaction (mask distilled, warn raw) + secrets corpus; migrate + verify + cutover; `rebuild`; epoch policy | ~7–9d (honest) |
| **M5** | **Continuity** | `resume-context` into wt/ws/SessionStart; cross-machine `wt <branch>` end-to-end (worktree recreate + raw import or brief); auto-handoff; boot reconciler; retention management | ~4–5d |
| **M6** | **Leverage** | commit-xref + auto-done; Codex injection parity (wt/spawn wrapper + AGENTS.md); morning brief; injection hygiene; cross-machine mail via lanes | ~4d |
| **Backlog** | After a month of real usage | Prompt-time hints (dark→live); usefulness flywheel; weekly digest; facts board | — |

M1+M2 close every data-loss gap and finish the agent-deck removal. M3 delivers `ask` before any migration risk. M4 is the headline: full memory *and* raw transcripts on every machine.

**v1 decisions on record:** subagent transcripts not independently captured (parent's files/commands carry the effects; revisit) · cloud sessions observed only · no PostToolUse content capture · no LLM in hooks · no SQLite sync · agent-deck removed, not absorbed.

## 11. Remaining open questions (small)

1. **vault-raw pruning:** keep raw segments forever, or auto-prune `scratch`-class sessions after N months? (Proposed: prune scratch at 3 months, keep everything else.)
2. **`_WT_REPOS` registry:** fold into hive config (`hive.toml`) so wt, streams, and commit-xref share one repo list? (Proposed: yes, in M2.)
