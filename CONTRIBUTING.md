# Contributing to Pyre

Thanks for your interest. Bug reports, fixes, docs and small focused features are all welcome. For anything large, such as a new harness adapter, a protocol change or a new top-level command, please open an issue first so we can agree on the shape before you write the code.

## Build

```sh
make build          # bin/pyre, bin/pyred, bin/pyre-hub, bin/pyre-mockhub (Go 1.27+, CGO off)
make help           # every target
```

The Makefile uses `~/sdk/go1.27.3/bin/go` when it exists, otherwise `go` on your PATH. Override it with `make GO=/path/to/go`.

Try your change without any real machines:

```sh
go run ./internal/tui/demo                          # TUI against the synthetic in-process fleet
go run ./cmd/pyre-mockhub --token dev-token &       # a mock hub for the CLI and the Android app
PYRE_HUB=http://127.0.0.1:7788 PYRE_TOKEN=dev-token bin/pyre sessions
```

## Test

```sh
make test vet                                   # Go tests and go vet
make check                                      # pre-merge gate: gofmt check, vet, golangci-lint, tests
make test-fast                                  # short suite as parallel test processes (what CI runs)
go test ./internal/tui/... ./tests/tui/...      # one area
go test ./tests/tui -run TestGoldenScreens -update   # regenerate TUI goldens after an intended UI change
android/gradlew -p android testDebugUnitTest    # Android unit + Robolectric tests
python3 scripts/check_docs.py                   # Markdown links and planning-doc consistency
```

CI has two workflows, both started by hand (`gh workflow run CI --ref <branch>`): `CI` is the one-minute check (ubuntu: tidy, vet, generated files, build, golangci-lint for linux and darwin, and the short suite sharded by `tools/testshard`; it also runs on each merge to main to refresh the caches), and `CI full` holds the slow gates (the full suite with `-race` on ubuntu and macOS, govulncheck, the reproducible release build). `tools/testshard` balances shards with `tools/testshard/weights.json`; when tests move a lot, refresh it with `make test-weights`, then set `assignSeed` in `tools/testshard/main.go` to the seed `go run ./tools/testshard -plan -shards 6` prints (both reshuffle the shards once, so the next CI runs rebuild their caches).

Tests must be hermetic. They use isolated config homes, state directories, tmux sockets, databases and ports, plus obviously synthetic credentials (`sk-ant-oat01-pyre-synthetic-…`). A test must never touch your real `~/.config/pyre`, your default tmux server, your `~/.claude` or `~/.codex` logins, or your keychain. Tests that need real hosts or real Claude/Codex logins skip unless their environment variables are set (see `tests/e2e/`).

To capture the TUI as PNG for review, use `tests/tui/screenshot.py`. It drives the demo in a private tmux server and rasterizes each step with headless Chrome. The README GIF in `docs/media/` was made this way: capture a key script (`… --cmd <demo binary> --cols 120 --rows 34 "down=⌥↓" "table=v" …`), then assemble the PNGs with ffmpeg's concat demuxer and `fps=10,scale=1000:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];[b][p]paletteuse=dither=none:diff_mode=rectangle`.

## Project layout

| Path | What |
|---|---|
| `cmd/pyre` | CLI, TUI and MCP server entry point |
| `cmd/pyred` | Node daemon: runs, adapters, hub link, local socket, integrations wiring |
| `cmd/pyre-hub` | Hub server, backup and restore |
| `cmd/pyre-mockhub` | Deterministic mock hub for clients and tests (not shipped) |
| `internal/contracts` | Shared types, the API interface and error codes. Change it deliberately: CLI, hub, daemon and Android all depend on it |
| `internal/hub`, `internal/httpapi`, `internal/transport` | Hub service, HTTP/WebSocket API, node channel |
| `internal/daemon`, `internal/supervisor`, `internal/runtime/{claude,codex,shell,tmux,mock}` | Run supervision and the harness adapters |
| `internal/accounts`, `internal/credentials`, `internal/usage` | Account pool, isolated profiles, token storage, usage probes |
| `internal/messaging`, `internal/attention`, `internal/digest`, `internal/search`, `internal/audit` | Messages and delivery states, the inbox, digest, search, audit log |
| `internal/projects`, `internal/artifacts`, `internal/review` | Optional projects, versioned documents, artifacts, comments |
| `internal/cli`, `internal/tui` | Commands and the Bubble Tea TUI (`internal/tui/demo` is the synthetic demo) |
| `integrations/` | Optional Hive, Outfit and Rota integrations |
| `android/` | Kotlin/Compose companion app |
| `packaging/`, `scripts/install/`, `scripts/backup/` | Release build, service templates, install/upgrade/uninstall, hub backup |
| `tests/` | Cross-package, TUI golden, resilience, security and real-host (e2e) tests |
| `docs/` | Design, contracts, operations, decisions |
| `schemas/`, `agents/tools/` | Generated JSON schemas, MCP tool manifest and agent configs |

## Pull requests

- Keep each PR focused, and run `make check` before opening it. Generated files must be committed in sync: fixtures and schemas (`make check-generated` verifies them), TUI goldens, and `agents/tools/pyre-mcp-tools.json` (`PYRE_UPDATE_GOLDEN=1 go test ./internal/cli -run TestMCPToolManifest`).
- Update the docs when behavior changes: the operations runbook for anything an operator sees, `docs/contracts.md` for interface changes, `tests/tui/DESIGN.md` for UI changes.
- Keep the semantics honest. A stored message, a message the runtime accepted, and a finished task are different facts. Never re-send an instruction whose outcome is unknown, and never infer that two accounts are isolated because their credential files were copied.
- Never commit tokens, real transcripts, personal hostnames or other private data. Fixtures must be obviously synthetic.
- New dependencies need a reason, a license compatible with MIT, and an entry in `NOTICE` if they are linked into a shipped binary or the APK.

By contributing, you agree that your contributions are licensed under the [MIT License](LICENSE).
