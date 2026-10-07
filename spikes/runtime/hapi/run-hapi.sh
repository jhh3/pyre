#!/usr/bin/env bash
# WP01 runtime spike: bounded, isolated run of HAPI (tiann/hapi) hub + runner.
#
# Usage:
#   run-hapi.sh setup     # rsync pinned checkout to scratch + bun install
#   run-hapi.sh start     # start hub (127.0.0.1 only, relay/push off) + runner
#   run-hapi.sh probe     # hit REST API: auth, machines, sessions; measure RSS
#   run-hapi.sh spawn     # POST /api/machines/:id/spawn (claude, remote mode)
#   run-hapi.sh send      # POST a message with a localId twice (dedupe check)
#   run-hapi.sh stop      # kill only the processes this script started
#   run-hapi.sh verify    # confirm global Claude/Codex config untouched
#
# Isolation:
#   - HAPI_HOME points at scratch, so nothing is written to ~/.hapi.
#   - HOME is left intact so a spawned claude can use the real login. Source
#     review (see WP01 findings) shows HAPI's Claude path does not write to
#     ~/.claude/settings.json or ~/.claude.json: it passes per-session
#     --settings <HAPI_HOME>/tmp/hooks/*.json and --mcp-config inline JSON.
#     Codex sessions are NOT spawned here: the shared-runtime path writes
#     ownership records into $CODEX_HOME/hapi-runtime-owners/.
#   - Hub bound to 127.0.0.1, relay disabled (--no-relay, default off),
#     iOS/Android push relays off, Telegram unset, version handoff disabled.
set -euo pipefail

PIN=86c88df93baf5d1f738dd4b202078bdf6dec376e
UPSTREAM=${UPSTREAM:?set UPSTREAM to a HAPI checkout}
SCR=${SCR:-/tmp/pyre-wp01/hapi}
SRC=$SCR/src
PORT=${PORT:-37006}
API=http://127.0.0.1:$PORT
WS=$SCR/workspace

export HAPI_HOME=$SCR/hapi-home
export HAPI_LISTEN_HOST=127.0.0.1
export HAPI_LISTEN_PORT=$PORT
export HAPI_IOS_PUSH=off
export HAPI_ANDROID_PUSH=off
export HAPI_DISABLE_VERSION_HANDOFF=1
export HAPI_API_URL=$API
unset TELEGRAM_BOT_TOKEN SERVERCHAN_SENDKEY HAPI_RELAY_API HAPI_PUBLIC_URL || true

redact() { sed -E 's/("(token|accessToken|cliApiToken)"[[:space:]]*:[[:space:]]*")([A-Za-z0-9_.:-]{4})[^"]*"/\1\3...REDACTED"/g'; }

cli_token() { bun -e "console.log(JSON.parse(require('fs').readFileSync('$HAPI_HOME/settings.json','utf8')).cliApiToken)"; }

jwt() {
  curl -s -X POST "$API/api/auth" -H 'Content-Type: application/json' \
    -d "{\"accessToken\":\"$(cli_token)\"}" | bun -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>console.log(JSON.parse(s).token))'
}

rss() {
  # hub, runner, and every descendant of them (runner children are detached
  # with setsid, so also include processes whose cwd is the spike workspace)
  echo "--- hapi processes (pid ppid rss_kb command, truncated)"
  local roots pids p
  roots="$(cat "$SCR/hub.pid" "$SCR/runner.pid" 2>/dev/null || true)"
  pids="$roots"
  for p in $(lsof -t +d "$WS" 2>/dev/null | sort -u); do pids="$pids $p"; done
  for _ in 1 2 3 4; do for p in $pids; do pids="$pids $(pgrep -P "$p" 2>/dev/null | tr '\n' ' ')"; done; done
  pids=$(echo $pids | tr ' ' '\n' | sort -un | tr '\n' ',' | sed 's/,$//')
  [ -n "$pids" ] && ps -o pid=,ppid=,rss=,command= -p "$pids" 2>/dev/null | cut -c1-200 || true
}

case "${1:-}" in
  setup)
    mkdir -p "$SRC" "$WS"
    git -C "$UPSTREAM" rev-parse HEAD | grep -q "^$PIN" || { echo "upstream not at pin $PIN"; exit 1; }
    rsync -a --exclude node_modules "$UPSTREAM/" "$SRC/"
    (cd "$SRC" && bun install)
    ;;
  start)
    mkdir -p "$HAPI_HOME" "$WS"
    # exec so the recorded pid is bun itself; </dev/null + redirects so no pipe is held open
    (cd "$SRC/hub" && exec bun run src/index.ts --no-relay </dev/null >"$SCR/hub.log" 2>&1) &
    echo $! >"$SCR/hub.pid"
    for _ in $(seq 1 60); do curl -sf -m 2 "$API/health" >/dev/null 2>&1 && break; sleep 0.5; done
    echo "hub pid $(cat "$SCR/hub.pid")"; tail -n 20 "$SCR/hub.log" | redact
    export CLI_API_TOKEN=$(cli_token)
    (cd "$SRC/cli" && exec bun src/index.ts runner start-sync --workspace-root "$WS" </dev/null >"$SCR/runner.log" 2>&1) &
    echo $! >"$SCR/runner.pid"
    sleep 8
    echo "runner pid $(cat "$SCR/runner.pid")"; tail -n 30 "$SCR/runner.log" | redact
    ;;
  probe)
    T=$(jwt); echo "jwt: ${T:0:4}..."
    echo "--- GET /api/machines"; curl -s "$API/api/machines" -H "Authorization: Bearer $T" | head -c 3000; echo
    echo "--- GET /api/sessions"; curl -s "$API/api/sessions" -H "Authorization: Bearer $T" | head -c 3000; echo
    rss
    ;;
  spawn)
    T=$(jwt)
    MID=$(curl -s "$API/api/machines" -H "Authorization: Bearer $T" | bun -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const j=JSON.parse(s);const m=(j.machines||j)[0];console.log(m.id)})')
    echo "machine $MID"
    curl -s -X POST "$API/api/machines/$MID/spawn" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
      -d "{\"directory\":\"$WS\",\"agent\":\"claude\",\"model\":\"haiku\",\"startingMode\":\"remote\"}"; echo
    ;;
  send)
    T=$(jwt); SID=${2:?session id}; LID=${3:-pyre-spike-$(date +%s)}
    for i in 1 2; do
      echo "--- POST #$i localId=$LID"
      curl -s -X POST "$API/api/sessions/$SID/messages" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
        -d "{\"text\":\"reply with the single word ok\",\"localId\":\"$LID\"}"; echo
    done
    sleep 1
    curl -s -X POST "$API/api/sessions/$SID/messages/queued-state" -H "Authorization: Bearer $T" -H 'Content-Type: application/json' -d "{\"localIds\":[\"$LID\"]}"; echo
    ;;
  stop)
    for f in runner hub; do
      [ -f "$SCR/$f.pid" ] && kill "$(cat "$SCR/$f.pid")" 2>/dev/null || true
    done
    sleep 2; rss
    ;;
  verify)
    shasum -c "$SCR/baseline.sha" && ls -d "$HOME/.hapi" 2>/dev/null && echo "WARNING: ~/.hapi exists" || echo "~/.hapi absent (ok)"
    ls "$HOME/.codex" | grep -i hapi && echo "WARNING: hapi files in ~/.codex" || echo "no hapi files in ~/.codex (ok)"
    ;;
  *) sed -n 2,12p "$0"; exit 2 ;;
esac
