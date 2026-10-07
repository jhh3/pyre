# W-secrets: secrets kept off process command lines

Date: 2026-10-04 (local evening; hub timestamps 2026-10-05 UTC). Branch `w-secrets`, based on main `2e25695`. Commits `db6452a` (tmux), `7155f88` (CLI), `c56739c` (test scripts) and the docs commit. The real-host binaries were built from `c56739c`.

## Finding

`internal/runtime/tmux` passed each run's environment to tmux as `new-session -e K=V` (and as `respawn-pane -e K=V`). This had two consequences:

- `PYRE_SESSION_TOKEN`, environment-overlay values (for example `GH_TOKEN`), connection env, and anything else in `ResolvedLaunch.Env` sat in the argv of the short-lived tmux client. Any local user could read it with `ps` or `/proc/*/cmdline`.
- The values also landed in the tmux **session environment**. Every window a human opened in a run's session inherited the token, and `tmux show-environment` showed it.

The `daemon.runEnv` comment claimed the token was "never put on a command line".

The negative control below reproduces the leak on real hosts with the main build.

## Audit (main `2e25695`)

| Where | What, to which process | Verdict | Fix |
|---|---|---|---|
| internal/runtime/tmux/tmux.go:362-373 `envArgs`, used at :507 (`NewSession`) and :528 (`RespawnPane`) | whole run env as `-e K=V` → tmux client argv and session env | **LEAK** | pane launcher plus 0600 env file (below) |
| internal/runtime/tmux/tmux.go:639 | `capture-pane -e` is the escape-sequence flag | not a leak | none |
| internal/daemon/launch.go:183-185 | comment "never … put on a command line" | **misleading** | comment rewritten; it is now true |
| internal/cli/cmd_system.go:160-166 | `pyre pair <link>`: the one-time pairing code on `pyre` argv (the documented flow) | **LEAK** | link read from stdin (prompt or pipe); a link argument is refused |
| internal/cli/cmd_system.go:199,253 | `pyre login --token TOKEN` accepted a value on argv | **LEAK** (option) | only `--token -` (stdin) or `PYRE_TOKEN`; a value is refused and not echoed |
| cmd/pyred/enroll.go:31 | `pyred enroll --ticket SECRET` (unused) | **LEAK** (option) | flag removed; `--secret-stdin` only |
| internal/updates/fetch.go:127-140 → stage.go:545 | a source URL with userinfo goes on `npm pack` argv and into records/errors | low | sources with embedded credentials are refused, without echoing them |
| internal/cli/cmd_fleet.go:177,234, docs/operations.md:259,272 | hints said `echo <secret> \| …` and `pyre pair '<link>'` | hygiene | hints say paste on stdin |
| tests/e2e/integ_remote.sh:109,224, integ_live.sh:58 | `outfit … --set TOKEN=$TOKEN` (synthetic) | test LEAK | token via the connector's declared env var, set inside `sh` (`env -i` arguments are argv too) |
| tests/e2e/integ_remote.sh:180,183, integ_live.sh:124, scripts/install/rehearse-install.sh:125,264, scripts/backup/rehearse-restore.sh:279 | `grep -F -e "$TOKEN"` (synthetic stacks) | test LEAK | `grep -F -f <0600 pattern file>` |
| android/scripts/realhub-it.sh:35 | pairing link as `-Ppyre.realhub.uri=` (gradle and test-JVM argv) | test LEAK | `-Ppyre.realhub.urifile=<0600 file>` |

The audit found the following paths clean. Each was checked at its exec site.

- Enrollment over ssh: the secret goes on stdin (`--secret-stdin`), and the plan is redacted.
- Keychain writes: `security -i` with the command on stdin. Linux uses `secret-tool` with the value on stdin.
- rota checkout: `-X <hex>` goes on stdin of `security -i`.
- Outfit invocations: args go on stdin, and `childEnv` carries no Pyre tokens.
- `hive` and `rota --help`.
- Codex: `codex app-server --listen` and `codex resume --remote` argv. `CODEX_HOME` travels through env.
- The doorbell `pyred inbox-watch --run-dir` takes the token from its inherited env.
- `pyre mcp`: its config file holds no secrets.
- Claude hooks: `pyred hook claude <Event>` reads only `PYRE_CLAUDE_STATE_DIR`.
- systemd scope wrap: `systemd-run --scope … -- tmux … start-server`. The tmux server always starts with a bare `start-server`.
- launchd plists and systemd units contain paths only.
- `install.sh`/`upgrade.sh`: the token goes through an env prefix or stdin.
- Backup: the passphrase comes from a file or env. age runs in process.
- Release signing: `ssh-keygen -Y sign -f <keyfile>`.
- HTTP: bearer tokens go in headers. No `?token=` query parameters, no request dumps.
- Diagnostics runs `ps` without `command`/`args` columns.
- `internal/supervisor/tmux.go`: run env goes through the client process env. `NewSession` is used only by tests.

## Design: pane launcher

`internal/runtime/tmux/execenv.go`:

1. `NewSession`/`RespawnPane` write `Env` and `Unset` to a new file `pyre-env-*` (0600, `os.CreateTemp`) in the socket directory, which is 0700 and already created by `PrepareDir`.
   - The format is NUL-separated records, `S` + `K=V` or `U` + `K`. It handles any value except NUL, which env cannot hold anyway and which is rejected.
2. The pane command becomes `<this executable> __pyre_exec_env <file> -- argv…`.
   - Every binary that links the tmux package acts as the launcher through an `init` sentinel. In production that is pyred; tests use their own test binaries, so no extra wiring is needed.
   - `Server.ExecHelper` can override the launcher.
3. The launcher refuses the file unless it is a regular file owned by the user with no group/other bits. It reads the file and deletes it before doing anything else.
4. It applies unsets first, then sets, so an explicit value beats `Unset`. It resolves `argv[0]` with the new `PATH`, as tmux's execvp would, then `execve`s in place. The PID and process group are unchanged, so `pane_pid`, `TerminateSession` and the doorbell's parent watch behave as before.
5. A failed tmux call removes the file. A file left behind by a pane that never started is swept by the next launch after 2 min.
   - No env files were left after any real run (`env_files_left: 0`).
6. A `sh -c '. file'` loader was rejected. `/bin/sh` on macOS (bash) aborts on `export UID=…` (readonly) and leaves the file behind (probe in the worker scratch dir). `tmux source-file` would put values into the session environment.

**Later windows do not inherit the run environment (decision).** Nothing in Pyre opens extra windows; there is no `new-window`/`split-window` in the code. Everything that needs `PYRE_SESSION_TOKEN`/`PYRE_SOCKET` is a child of the harness process and inherits from it: Claude hooks, the pyre MCP server, the doorbell monitor, and agent Bash calls. A human who opens a window in a shell run's session can still use `pyre` with their own client credential.

**Codex TUI respawn:** tmux no longer holds the env, so the adapter keeps the TUI launch env in memory (`runRef.tuiEnv`, never in `run.json`) and passes it again on respawn. After a pyred restart only `CODEX_HOME` is known, as before for the `startTUI(nil)` path.

## Tests

- `TestLaunchArgvHasNoSecrets` (unit) checks the exact `new-session` and `respawn-pane` client argv. No env value, no `-e`, no variable name. The pane command is the launcher plus the file, the file is 0600 in a 0700 dir and holds the values, and it is gone after the read. The NUL value is rejected.
- `TestReadEnvFileRefusesSharedFile`: a 0644 file is refused and removed.
- `TestLaunchEnvReachesPaneOnly` (real tmux) uses a tmux shim that logs every client argv. It checks that:
  - the pane gets values containing quotes, `$`, newlines and `;|&` exactly, and `Unset` applies;
  - the respawned pane gets the new env;
  - `show-environment -t` and `-g` lack the token;
  - a later `new-window` lacks it;
  - no client argv in the shim log carries it;
  - no env file is left.
- `TestLaunchProcessTableHasNoSecrets` (real process table) samples every process's command line during 5 new-session and 5 respawn calls. It uses `/proc/*/cmdline` on Linux and `ps` on macOS. Slow shims keep the tmux client and launcher visible long enough for macOS `ps`. The test requires the sampler to have seen tmux clients, launchers and panes, and to have found no secret.
  - Mac: 75 samples; clients 48, launcher 68, panes 249.
  - server (cross-compiled test binary, tmux 3.6): 588 samples; clients 507, launcher 687, panes 4994.
- **Negative control:** re-adding `-e K=V` in a throwaway copy of the package makes all three tests fail, 3 runs out of 3. The process-table test then reports `secret on a command line: [... -e PYRE_SESSION_TOKEN=SYNTHETIC… ]`.
- CLI: `pair` refuses an argument (ExitUsage, the code is not echoed) and reads stdin; `login` refuses a token value and does not echo it.
- updates: `TestSourceRefusesEmbeddedCredentials`.
- Gate:
  - `go vet ./...` passes for darwin and linux.
  - `go test -count=1 -p 4 ./...` passes on Mac.
  - The runtime test binaries for tmux, codex, shell and claude pass on server.
  - `make lint` reports 0 issues for linux and darwin.

## Real verification

For each host the setup was:

- an isolated stack under `~/.cache/pyre-wsecrets`, with a real `pyre-hub` on 127.0.0.1:18660 and a real `pyred` (`--rota-checkout`);
- one non-active rota account per harness, checked out through Pyre;
- the driver `verify.py` and the sampler `argvscan`, both in the worker scratch dir.

`argvscan` samples the argv of every process. It uses `kern.procargs2` on macOS and `/proc` on Linux. Each iteration rereads processes younger than 3 s, and a full sweep runs every 100 ms. The mean iteration took 0.2–0.8 ms. It flags any `py[cnsp]_` token shape, and in phase B also the exact session tokens. Those are read from each harness process's environment into 0600 files and are never printed.

- **Phase A** covered the launches: Claude (Opus, rota account), Codex (server; app-server plus TUI), a shell run, and 8 extra shell launches.
- **Phase B** covered in-session use:
  - In the shell run, `pyre whoami` and `pyre send` to the Claude session, using the absolute path of the stack's `pyre`.
  - With a writable tmux client attached to the Claude run, so delivery has to go through the doorbell, the peer message is read with the MCP inbox tool and acked.
  - A later `new-window` in the shell run's session.
  - Codex: kill the TUI process, then `pyre attach --print`, so the adapter respawns it.

Builds: branch `wsecrets-c56739c`; control `main-2e25695` (identical driver, separate stack on port 18661).

| | Mac (macOS 26.6, tmux 3.4, direct) | server (Ubuntu 26.04, tmux 3.6, `PYRE_SYSTEMD_SCOPE=on`) |
|---|---|---|
| tmux server placement | direct | systemd scope `pyre-tmux-….scope` in `pyre-runs.slice` (recorded with its cgroup) |
| Phase A sampling | 170,177 iterations, 1.50 M argv reads | 53,497 iterations, 1.32 M argv reads |
| Sightings, phase A | 137 tmux `new-session` clients, 795 pane-launcher samples (`pyred __pyre_exec_env …/pyre-env-NNN -- claude …`) | 20 `new-session` clients, 225 launcher samples (Claude, Codex app-server, Codex TUI, shells) |
| **Token on any command line** | **0** (phase A shape match; phase B shape match plus 2 exact tokens, 72,312 iterations) | **0** (phase A; phase B with 3 exact tokens, 38,234 iterations, incl. the `respawn-pane` client and launcher) |
| Harness process has `PYRE_SESSION_TOKEN` | Claude yes (3 of 4 processes in its tree carry it); shell yes, read by the shell itself because macOS hides a platform binary's env from `ps` | Claude yes (5/5 in its tree); Codex TUI yes; shell yes |
| tmux session env / global env hold the token | no / no | no / no |
| Window opened later in the session | no token (82 env lines) | no token (35 env lines) |
| `pyre whoami` in the shell run | principal kind `session`, this session, `token_source=PYRE_SESSION_TOKEN` | same |
| `pyre send` from the shell run | stored, message id returned | same |
| Doorbell + pyre MCP (writable client attached) | accepted via `claude.tool.inbox_read` (runtime reason "doorbell: read with the pyre inbox tool"), `ack=acknowledged`; tools `mcp__pyre__inbox`, `mcp__pyre__ack_message`; 2 inbox-watch processes | same (several deferred attempts while attached, then one `inbox_read` acceptance) |
| Claude hooks | 13 events: SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, Stop, SubagentStop | 11 events, same kinds without SubagentStop |
| Codex TUI respawn | not run (Codex is the server case) | TUI killed → pane dead → respawned (new pid) with the same `PYRE_SESSION_TOKEN` |
| Env files left | 0 | 0 |

Negative control, main `2e25695`, same driver (shell run plus 8 extra launches):

| | Mac | server |
|---|---|---|
| Token on a command line | **9 distinct leaking lines**: every launch was caught, e.g. `/opt/homebrew/bin/tmux -u -S …/pyre.sock … new-session -d -s run_… -e PATH=… -e PYRE_SESSION_TOKEN=pys_<REDACTED> -e PYRE_…` | **6 distinct leaking lines**, same shape (`/usr/bin/tmux … -e PYRE_SESSION_TOKEN=pys_<REDACTED>`) |
| tmux session env holds the token | yes | yes |
| Window opened later inherits the token | yes | yes |

An earlier run used a slower sampler (`ps`-equivalent full sweeps, about 4.8 ms). On server it missed the single control launch. The faster young-process sampler and the 8 extra launches were added so that the control is reliably caught. The branch results above come from runs with that same sampler.

**Side effect observed: `PATH`.** Under `-e`, `pyre` was not on `PATH` inside shell runs on either host (`command -v pyre` empty). With the launcher, the run's `PATH` applies: `pyre` resolves to the stack's binary. `runEnv` intended pyre's bin dir to be first. This is consistent with tmux setting a new pane's `PATH` from the creating client.

Cleanup: sessions were stopped, the stacks went down, and the isolated tmux servers and the outer test tmux server were killed. `~/.cache/pyre-wsecrets` was removed on both hosts. No rota lock remained, and no `pyre-wsecrets` process or `pyre-tmux-*` scope was left.

## Remaining limitations

- **Claude argv still carries user content, not credentials.** Specifically `--append-system-prompt <agent guide + launch context>` and the initial prompt, for the life of the Claude process. A prompt that contains a secret would be visible in `ps`. Codex sends its prompt over RPC.
- Other user content on argv: Hive search query text (`hive search -- <query>`) and `pyre connections invoke --arg k=v` values (operation arguments, not credentials).
- Outfit's own CLI accepts credentials via `--set`. Pyre never calls `connector add`/`credential replace`. Only the test scripts did, and they now use the declared env var.
- **Inherited environment:** panes still inherit pyred's own environment (the server's global env). If pyred itself runs with a secret in its env, runs get it unless it is in `ResolvedLaunch.Unset`. That is not an argv exposure.
- macOS `ps` cannot read a platform binary's environment (`/bin/zsh`), so the Mac shell run's token was read by the shell itself (`printf` builtin into a 0600 file).
- Not run: remote attach (`ssh -t`) as the client, a Codex run on the Mac, a pyred restart followed by a Codex TUI respawn (expected: only `CODEX_HOME`), and the Android `realhub-it.sh` change (script syntax checked; no device or gradle run).
- The rehearsal scripts' grep changes (`rehearse-install.sh`, `rehearse-restore.sh`, `integ_*`) were syntax-checked (`bash -n`), not re-run end to end.

## Proposed MATRIX rows

| Case | Requirements | Status | Environment | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| S-argv | security (credentials never on command lines) | pass | real hub + pyred + real Claude (rota) on Mac and server, Codex on server, shell on both; server in systemd scope mode | [argv secrets](../security/argv-secrets-2026-10-04.md) | 0 tokens on any command line across more than 2.1 M argv reads per host during launches, respawn and in-session use; main build leaks on every launch (control); doorbell, hooks, pyre MCP and `pyre send` authenticate; later windows do not inherit the token |
| A04 | R03, R04 | partial (unchanged) | as above | add [argv secrets](../security/argv-secrets-2026-10-04.md) | adds: doorbell + MCP inbox/ack re-verified after the env-file change (Mac, server) |
