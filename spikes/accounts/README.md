# WP02 accounts spike

Experiments showing how Pyre can bind a subscription account to one session/run for Claude Code
and Codex without touching other sessions or the default login. Every credential here is
synthetic (`pyre-synthetic-<LABEL>-…`, or unsigned JWTs carrying `"pyre_synthetic": true`).
Findings: the WP02 worker report; the integration owner places it at
`docs/evidence/accounts/REPORT.md`. Raw transcripts are in `docs/evidence/accounts/raw/`.

## Pieces

| File | Purpose |
|---|---|
| `fakeprovider.py` | One local process acting as the Anthropic API, the ChatGPT/Codex backend, the token endpoints (emulates single-use refresh-token rotation), and an HTTPS proxy. The proxy blocks every CONNECT, except that with `--mitm-ca` it TLS-terminates the listed fixed hosts (`platform.claude.com`, `auth.openai.com`) using a throwaway CA. Logs JSONL. Any non-synthetic credential is redacted, rejected, and flagged `ALARM`. |
| `shims/security` | A fake macOS `security` placed first on PATH. Claude's keychain reads and writes for the profile hit it. It logs the service name, reports "not found", and refuses writes, so the real login keychain is never touched. |
| `lib.sh` | Helpers: synthetic Claude/Codex homes, a clean `env -i` launch environment (no inherited API keys), `claude_env` / `codex_env` / `codex_cfg`, a throwaway CA, mtime/size snapshots of the default credential files. |
| `showlog.py` | Summarises a provider log, one line per request. |
| `appserver_probe.py` | Sends `account/read` and `account/rateLimits/read` to `codex app-server` in the current `CODEX_HOME`, as rota does. |
| `experiments/e1…e8` | The experiments. The report has the per-experiment results. |
| `run-mac.sh` | Runs e1–e8 and writes transcripts to `docs/evidence/accounts/raw/`. |
| `run-linux.sh`, `run-linux-codex.sh` | Linux runs. Codex uses an isolated 0.160.0 binary under `~/.cache/pyre-spike-accounts/codex`. |
| `real-profile-check.sh` | **The user runs this; the worker never runs it.** Creates a fresh native device login in an isolated profile and checks that it is isolated. |
| `tools/strctx.py` | Prints code context around strings in a binary. Used to find keychain naming and env vars. |

## Reproduce

```sh
# macOS (needs claude, codex, python3, openssl, tmux on PATH)
bash spikes/accounts/run-mac.sh


# Linux host (ssh alias "server" in this example)
scp -r spikes/accounts server:.cache/pyre-spike-accounts/
ssh server bash -l .cache/pyre-spike-accounts/accounts/run-linux.sh
ssh server bash -l .cache/pyre-spike-accounts/accounts/run-linux-codex.sh
```

Each experiment builds its state under `$SPIKE_ROOT`, which defaults to `$TMPDIR/pyre-wp02-<exp>`.
That includes a synthetic `$HOME`. Nothing reads `~/.claude`, `~/.codex`, the keychain or rota
snapshots. The only exception is a before/after **mtime/size** `stat` of the default credential
files, which never reads their content.

## Safety notes

- Harnesses are launched with `env -i`, so the parent shell's `ANTHROPIC_API_KEY` /
  `OPENAI_API_KEY` never reach them.
- `HTTPS_PROXY` points at the fake provider and `NO_PROXY` covers only loopback. Anything the
  harness tries to reach outside the fake (`api.anthropic.com`, `chatgpt.com`, `github.com`) is
  logged and blocked.
- Codex runs force `cli_auth_credentials_store="file"` so a synthetic login can never land in
  the OS keyring.
