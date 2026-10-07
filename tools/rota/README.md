# rota

> Part of [Pyre](../../README.md), in `tools/rota`. Optional: Pyre works with or without it, and with a separately installed copy. Install the bundled copy with `scripts/install/install.sh --with-tools` (an existing install is detected and left alone), or build it with `make tools`. The bundled copy is the single-file `rota` script (`<prefix>/tools/rota/bin/rota`); installing it runs no login and touches no credentials.

Rotate between multiple Claude subscription accounts in Claude Code, and see usage
headroom for both Claude and Codex subscriptions in one place.

```
$ rota
● dev@example.com  (max 20x)  ← active
    session (5h)    █░░░░░░░░░░░░░░░░░░░   3%  resets thu 8:10pm (in 4h 47m)
    weekly (all)    ████████░░░░░░░░░░░░  42%  resets sat 2:00am (in 1d 10h)
    weekly (Fable)  ██████████████░░░░░░  71%  resets sat 2:00am (in 1d 10h)
    extra usage: $3,401.42 used
...
fleet avg: session 27% · weekly 50% · Fable 77%
best: dev@example.com (Fable 73% · session 9% used)  (already active)

Codex
● dev@example.com  (pro subscription)  ← active
    weekly                        ███░░░░░░░░░░░░░░░░░  16%  resets wed 8:31pm (in 5d 5h)
    weekly (GPT-5.3-Codex-Spark)  ░░░░░░░░░░░░░░░░░░░░   0%  resets fri 2:49pm (in 6d 23h)
    credits: 205.85 remaining
```

## Commands

| Command | What it does |
|---|---|
| `rota` / `rota usage` | Usage bars for every Claude account and every Codex account |
| `rota switch [email]` | Switch the active Claude account; no email = auto-pick best. Prefixes work: `rota switch dev` |
| `rota add [email]` | Enroll another account via an in-process browser login (doesn't switch, doesn't log anything out) |
| `rota reauth <email>` | Re-authenticate a saved account whose refresh token expired, in place. `--manual` if the browser can't reach localhost |
| `rota list` / `rota whoami` | Housekeeping; `list` shows token health per account |
| `rota export` / `rota import <file\|->` | Move account snapshots between machines |
| `rota remove <email>` / `rota save` | Delete a snapshot / repair the active one |
| `rota codex` | Codex subscription plans, rate limits, reset times, and credits only |
| `rota codex switch [name]` | Switch the active Codex account; no name = auto-pick most headroom |
| `rota codex add` | Enroll another Codex account via `codex login` (doesn't switch) |
| `rota codex list` / `save [name]` / `remove <name>` / `whoami` | Codex housekeeping |

"Best" = an account that is usable right now without dipping into extra-usage
credits (session, weekly (all), **and** Fable weekly each < 100%), ranked by
most Fable headroom, then session, then overall weekly.

Usage checks run in parallel across accounts and never touch the live login —
switching identity only happens on `rota switch`.

## Codex accounts

Codex accounts get the same treatment as Claude accounts, completely
independently: snapshots live in `$CODEX_HOME/accounts/<name>/auth.json`, and
`rota codex switch` swaps only `$CODEX_HOME/auth.json` — Codex config and
history stay shared, and the Claude login is never touched (and vice versa:
`rota switch` never touches Codex). Each snapshot directory doubles as a
`CODEX_HOME` for usage checks, so refreshed tokens persist per account.
Bootstrap: `rota codex save` captures the current login; `rota codex add`
enrolls each additional one. Extra read-only homes can still be listed via the
path-separated `ROTA_CODEX_HOMES`.

## Install

```sh
make install    # copies rota to ~/.local/bin
```

Bootstrap: with an existing `claude` login, `rota save` captures it; `rota add`
enrolls each additional account.

## Logins, expiry and re-auth

`rota add` and `rota reauth` run the same OAuth PKCE flow `claude /login` uses,
but inside rota: a local callback listener on a random port, the browser opens
(or you paste the callback URL / `code#state` if it can't reach localhost —
`--manual` uses the hosted code page instead), the code is exchanged, and the
profile is fetched to build the account block. Only the snapshot for the account
that signed in is written; the live login is updated only if that account is
already the active one. Safe to run from inside a Claude Code session.

Refresh tokens rotate on every refresh but their lifetime is fixed at login
(roughly two months) — refreshing doesn't extend it. `rota list` shows how long
each login is good for and warns in the last week. When a refresh is rejected
(`invalid_grant`), the account is marked dead in its snapshot and skipped on later
runs; before this, retrying dead tokens against the usage API earned a 429 with
a 40-minute `Retry-After`. `rota reauth <email>` clears the mark.

A real 429 is honored: a short `Retry-After` is slept through once, a long one is
reported with the wait time, and the last good numbers from
`~/.claude/accounts/.usage-cache.json` are shown marked with their age.

Codex usage requires the `codex` CLI and a ChatGPT login. Rota automatically reads
the active `CODEX_HOME` (normally `~/.codex`) through `codex app-server`; it never
reads or copies Codex tokens itself. To display additional Codex subscriptions,
keep each login in a separate Codex home and provide the extra directories as a
path-separated list:

```sh
ROTA_CODEX_HOMES="$HOME/.codex-work:$HOME/.codex-personal" rota
```

`CODEX_HOME` identifies the active subscription and is always included, so it does
not need to be repeated in `ROTA_CODEX_HOMES`.

## How it works

- Claude account snapshots (OAuth tokens + identity) live in `~/.claude/accounts/<email>.json`, chmod 600.
- Claude usage comes from the same OAuth endpoint the `/usage` command uses.
- Codex account and limit data comes from the local App Server's `account/read` and
  `account/rateLimits/read` methods. Multi-bucket limits are labeled by their
  reported names and window durations rather than assuming fixed primary/secondary meanings.
- Switching swaps the `claudeAiOauth` key in `~/.claude/.credentials.json`
  (MCP server OAuth is untouched) and the `oauthAccount` block in `~/.claude.json`.
  Running sessions keep their old identity; new ones pick up the switch.
- Refresh tokens are single-use (rotated on every refresh), so rota refreshes
  when an access token has expired or a request 401s, and immediately writes
  rotated tokens (and the new refresh-token expiry) back to both the snapshot
  and the live file.

## Caveats

- Codex subscription limits are available for ChatGPT-authenticated Codex homes.
  API-key authentication is reported, but its Platform billing is not treated as a subscription.
- Rota displays multiple Codex homes but does not switch or snapshot their credentials;
  select one for Codex itself by setting `CODEX_HOME`.
- Exports contain OAuth secrets. `make export` writes `accounts.export.json`
  (gitignored, chmod 600) — treat it like a password file.
- If two machines actively use the same account, token rotation will eventually
  invalidate one of them. Prefer one machine per account, or re-auth with `rota add`.

## License

MIT, as part of Pyre (see [LICENSE](../../LICENSE)). Standard library Python only, no third-party dependencies.

Tests: `python3 -m unittest -v test_rota.py` (sets `ROTA_NO_KEYCHAIN=1`; never touches the real Keychain), or `make tools-test` from the Pyre root.
