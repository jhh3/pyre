#!/bin/bash
# Real-host release install/update test (A16), run ON the host by
# tests/e2e/release_update.sh. Everything lives under one isolated directory
# (default ~/.cache/pyre-wupdates): a fake home for the per-user install
# (--home, so no launchctl/systemctl is ever run, no persistent unit, no
# linger), the hub and node state, and the uploaded signed releases. The hub
# and node are started from the rendered units by
# scripts/install/rehearsal-supervisor.sh (env -i + the unit's command line),
# which upgrade.sh also uses to restart them (PYRE_UPGRADE_RESTART_HOOK).
#
#   release_update_host.sh setup PORT NODE PLATFORM [install.sh node args...]
#   release_update_host.sh shell TITLE            start a shell session with a tick loop
#   release_update_host.sh claude TITLE ACCOUNT   start a Claude session on a hub account
#   release_update_host.sh check                  versions, daemon health, runs, pinned paths
#   release_update_host.sh upgrade CASE [upgrade.sh args...]   CASE = tampered|proto2|bad|good|again
#   release_update_host.sh rollback
#   release_update_host.sh pyre ARGS...           run the installed pyre CLI (isolated config)
#   release_update_host.sh teardown               stop runs, hub, node; remove the directory
#
# Layout of the uploaded kit: $W/rel/{rc2,rc3,rc3bad,proto2}/ (tarballs,
# SHA256SUMS, SHA256SUMS.sig), $W/rel/allowed-signers (the throwaway test key),
# $W/kit/rehearsal-supervisor.sh, $W/kit/synth_mcp.py.
set -uo pipefail
export PATH="$HOME/homebrew/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
W=${WUPD_DIR:-$HOME/.cache/pyre-wupdates}
case "$W" in /*) ;; *) W=$HOME/$W ;; esac
case "$W" in "$HOME"|"$HOME/"|/|"") echo "refusing: bad work dir $W" >&2; exit 2 ;; esac
H=$W/h ST=$W/st HUBD=$W/hub RUN=$W/run REL=$W/rel KIT=$W/kit X=$W/x WORK=$W/work LOGS=$W/logs
export PYRE_REHEARSAL_HOME=$H PYRE_REHEARSAL_RUN=$RUN PYRE_REHEARSAL_SERVICE_HOME=$HOME
export PYRE_UPGRADE_RESTART_HOOK=$KIT/rehearsal-supervisor.sh
export PYRE_CONFIG=$H/.config/pyre/client.json PYRE_STATE_DIR=$W/cli-state
# The node trusts the release key the test install pinned (in the fake home),
# not the owner's ~/.config/pyre; a first boot of a pyred handed off by an
# in-product update must prove health within the handoff timeout.
export PYRE_RELEASE_ALLOWED_SIGNERS=$H/.config/pyre/release-allowed-signers
export PYRE_DAEMON_HANDOFF_TIMEOUT=${WUPD_HANDOFF_TIMEOUT:-45s}
export PYRE_REHEARSAL_PASS_ENV="PYRE_RELEASE_ALLOWED_SIGNERS PYRE_DAEMON_HANDOFF_TIMEOUT"
BASE=${WUPD_BASE:-rc2}
unset PYRE_HUB PYRE_HUB_PIN PYRE_SOCKET PYRE_TOKEN PYRE_SESSION_TOKEN PYRE_SESSION_ID
PREFIX=$H/.local/share/pyre
P=$PREFIX/current/bin/pyre
mkdir -p "$LOGS" "$RUN"
cmd=${1:?command}; shift
ts() { date -u +%H:%M:%S; }
say() { echo "[$(ts)] $*"; }
plat() { cat "$W/platform"; }
tarball() { echo "$REL/$1/$(cd "$REL/$1" && ls pyre-*-"$(plat)".tar.gz)"; }
kit() { echo "$X/$(cd "$X" && ls -d pyre-*)/scripts/install"; }
jget() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
cur() { basename "$(readlink "$PREFIX/current" 2>/dev/null)" 2>/dev/null; }

wait_link() { # wait until pyred status reports connected (max $1 s)
  local i
  for i in $(seq 1 "${1:-60}"); do
    if "$PREFIX/current/bin/pyred" status --state "$ST" --json 2>/dev/null | grep -q '"link":"connected"'; then return 0; fi
    sleep 1
  done
  return 1
}

# run_of SESSION -> the session's current run ID.
run_of() { "$P" sessions show "$1" --json 2>/dev/null | jget "d['run']['run_id']"; }

# pane_of SESSION -> "socket target" of the session's tmux pane (Pyre's tmux
# servers are on private sockets under the node state; a run's tmux session is
# named after its run ID).
pane_of() {
  local run s
  run=$(run_of "$1")
  for s in "$ST"/tmux/*.sock "$ST"/runs/*/tmux.sock; do
    [ -S "$s" ] || continue
    if tmux -S "$s" has-session -t "=$run" 2>/dev/null; then echo "$s =$run:"; return 0; fi
  done
  echo "- -"
}

# proc_env PID KEY: one environment value of a process (never the whole env).
proc_env() {
  if [ -r "/proc/$1/environ" ]; then
    tr '\0' '\n' <"/proc/$1/environ" | sed -n "s/^$2=//p" | head -n 1
  else
    ps -E -ww -o command= -p "$1" 2>/dev/null | tr ' ' '\n' | sed -n "s/^$2=//p" | head -n 1
  fi
}

case "$cmd" in
setup)
  PORT=${1:?port}; NODE=${2:?node}; PLAT=${3:?platform}; shift 3
  [ ! -e "$PREFIX" ] || { echo "refusing: $PREFIX exists (teardown first)" >&2; exit 2; }
  echo "$PLAT" >"$W/platform"; echo "$PORT" >"$W/port"; echo "$NODE" >"$W/node"
  mkdir -p "$H" "$WORK" "$X"
  # A node service whose HOME is the fake home (svc) starts zsh there:
  # without a .zshrc, zsh-newuser-install would swallow the test's keys.
  echo "# synthetic test home" >"$H/.zshrc"
  tar -xzf "$(tarball $BASE)" -C "$X"
  I=$(kit)
  say "== install.sh refuses tampered artifacts (fresh homes, signed rc2 copies)"
  mkdir -p "$W/tampered-tarball" "$W/tampered-sums" "$W/h-t1" "$W/h-t2"
  cp "$(tarball $BASE)" "$REL/$BASE/SHA256SUMS" "$REL/$BASE/SHA256SUMS.sig" "$W/tampered-tarball/"
  printf 'synthetic tampering\n' >>"$W/tampered-tarball/$(basename "$(tarball $BASE)")"
  cp "$(tarball $BASE)" "$REL/$BASE/SHA256SUMS" "$REL/$BASE/SHA256SUMS.sig" "$W/tampered-sums/"
  echo "0000000000000000000000000000000000000000000000000000000000000000  pyre-synthetic-extra.tar.gz" >>"$W/tampered-sums/SHA256SUMS"
  bash "$I/install.sh" --tarball "$W/tampered-tarball/$(basename "$(tarball $BASE)")" --home "$W/h-t1" --allowed-signers "$REL/allowed-signers" >"$LOGS/install-tampered-tarball.log" 2>&1
  echo "install tampered tarball: exit $? (files left: $(find "$W/h-t1" -mindepth 1 | wc -l | tr -d ' '))"; tail -n 2 "$LOGS/install-tampered-tarball.log"
  bash "$I/install.sh" --tarball "$W/tampered-sums/$(basename "$(tarball $BASE)")" --home "$W/h-t2" --allowed-signers "$REL/allowed-signers" >"$LOGS/install-tampered-sums.log" 2>&1
  echo "install tampered SHA256SUMS: exit $? (files left: $(find "$W/h-t2" -mindepth 1 | wc -l | tr -d ' '))"; tail -n 2 "$LOGS/install-tampered-sums.log"
  bash "$I/install.sh" --tarball "$(tarball $BASE)" --home "$W/h-t2" >"$LOGS/install-no-trust.log" 2>&1
  echo "install signed rc2 with no trusted key (run from the tarball, no pin, no --allowed-signers): exit $?"; tail -n 1 "$LOGS/install-no-trust.log"
  rm -rf "$W/h-t1" "$W/h-t2"
  say "== install $BASE (signature verified, key pinned)"
  bash "$I/install.sh" --tarball "$(tarball $BASE)" --home "$H" --allowed-signers "$REL/allowed-signers" 2>&1 | tee "$LOGS/install-rc2.log" | grep -E "signature|checksum|pinned|installed|current|health" ; echo "exit ${PIPESTATUS[0]}"
  "$PREFIX/current/bin/pyre-hub" init --data "$HUBD" --hub-url "https://127.0.0.1:$PORT" --tls-host 127.0.0.1 >"$LOGS/hub-init.log" 2>&1 || { cat "$LOGS/hub-init.log"; exit 1; }
  bash "$I/install.sh" --tarball "$(tarball $BASE)" --home "$H" --service hub --hub-data "$HUBD" --hub-listen "127.0.0.1:$PORT" --hub-url "https://127.0.0.1:$PORT" >"$LOGS/install-hub.log" 2>&1 || { cat "$LOGS/install-hub.log"; exit 1; }
  bash "$KIT/rehearsal-supervisor.sh" start hub
  for _ in $(seq 1 100); do curl -sk --max-time 1 "https://127.0.0.1:$PORT/healthz" | grep -q ok && break; sleep 0.2; done
  PIN=$(python3 -c "import json;print(json.load(open('$HUBD/owner-credential.json'))['hub_cert_sha256'])")
  python3 -c "import json;print(json.load(open('$HUBD/owner-credential.json'))['token'])" | "$P" login --hub "https://127.0.0.1:$PORT" --pin "$PIN" --token - >/dev/null || exit 1
  "$P" hello
  mkdir -p "$ST/profiles"; chmod 700 "$ST"
  "$P" enroll-node --name "$NODE" --json >"$W/ticket.json" 2>/dev/null || exit 1
  python3 -c "import json;print(json.load(open('$W/ticket.json'))['secret'])" | "$PREFIX/current/bin/pyred" enroll --hub "https://127.0.0.1:$PORT" --pin "$PIN" --node-id "$(python3 -c "import json;print(json.load(open('$W/ticket.json'))['node_id'])")" --credential-file "$ST/node-credential.json" --secret-stdin >/dev/null || exit 1
  rm -f "$W/ticket.json"
  bash "$I/install.sh" --tarball "$(tarball $BASE)" --home "$H" --service node --state "$ST" --name "$NODE" "$@" >"$LOGS/install-node.log" 2>&1 || { cat "$LOGS/install-node.log"; exit 1; }
  grep -E "wrote|warning" "$LOGS/install-node.log"
  if [ "${WUPD_NO_NODE:-0}" = 1 ]; then say "node unit rendered; not started (svc start runs it under the real service manager)"; exit 0; fi
  bash "$KIT/rehearsal-supervisor.sh" start node
  if wait_link 60; then say "node $NODE connected"; else say "node did not connect"; tail -n 20 "$RUN/node.log" "$H/Library/Logs/pyre/pyred.err.log" 2>/dev/null; exit 1; fi
  "$P" nodes
  ;;
shell)
  T=${1:?title}
  "$P" start --node "$(cat "$W/node")" --harness shell --cwd "$WORK" --title "$T" --json >"$W/s-$T.json" || exit 1
  S=$(jget "d['session']['session']['session_id']" <"$W/s-$T.json")
  echo "$S" >"$W/sid-$T"
  sleep 3
  bash "$0" tick "$T"
  ;;
tick)
  # The loop records the time and the PYRE_CLI the session's shell was given.
  T=${1:?title}; S=$(cat "$W/sid-$T")
  read -r SOCK TGT < <(pane_of "$S")
  tmux -S "$SOCK" send-keys -t "$TGT" "while :; do echo \$(date +%s) \$PYRE_CLI >$W/tick-$T; sleep 2; done" Enter
  sleep 3
  say "shell session $T = $S (pane $TGT): $(cat "$W/tick-$T" 2>/dev/null)"
  ;;
claude)
  T=${1:?title}; ACCT=${2:?account}
  "$P" start --node "$(cat "$W/node")" --harness claude --account "$ACCT" --cwd "$WORK" --title "$T" --json >"$W/s-$T.json" || exit 1
  S=$(jget "d['session']['session']['session_id']" <"$W/s-$T.json")
  echo "$S" >"$W/sid-$T"
  say "claude session $T = $S"
  ;;
check)
  echo "current: $(cur)  previous: $(cat "$PREFIX/.previous" 2>/dev/null || echo -)  failed: $(cd "$PREFIX" && ls -d */.failed 2>/dev/null | tr '\n' ' ')"
  echo "pinned key: $(awk '{print $3, $5}' "$H/.config/pyre/release-allowed-signers" 2>/dev/null | cut -c1-40 || echo none)"
  "$PREFIX/current/bin/pyred" status --state "$ST" --json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("pyred status: version %s link %s live_runs %s" % (d["versions"].get("pyred"), d.get("diagnostics", {}).get("link"), d.get("live_runs")))'
  "$P" hello --json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("hub: version %s protocol %s..%s" % (d.get("hub_version"), d.get("min_protocol"), d.get("protocol")))'
  for f in "$W"/sid-*; do
    [ -f "$f" ] || continue
    T=${f##*/sid-}; S=$(cat "$f")
    st=$("$P" sessions show "$S" --json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
r = d.get("run") or {}
print("run %s state %s runtime %s %s" % (r.get("run_id", "?"), r.get("state", "?"), (r.get("runtime") or {}).get("version", "-"), (r.get("runtime") or {}).get("executable", "-")))' 2>/dev/null)
    read -r SOCK TGT < <(pane_of "$S")
    pp=$(tmux -S "$SOCK" display -p -t "$TGT" '#{pane_pid}' 2>/dev/null || echo -)
    tick="" cli=""
    if [ -f "$W/tick-$T" ]; then
      read -r tt cli <"$W/tick-$T"
      tick=" tick_age=$(( $(date +%s) - tt ))s"
    else
      cli=$(proc_env "$pp" PYRE_CLI)
    fi
    echo "session $T $S: $st pane_pid=$pp$tick PYRE_CLI=${cli:-?}"
  done
  ;;
upgrade)
  CASE=${1:?case}; shift
  I=$(kit)
  case "$CASE" in
    tampered) TB="$W/tampered-sums/$(basename "$(tarball "$BASE")")" ;;
    proto2) TB=$(tarball proto2) ;;
    bad) TB=$(tarball rc3bad) ;;
    good|again) TB=$(tarball rc3) ;;
    *) [ -d "$REL/$CASE" ] || { echo "unknown case $CASE" >&2; exit 2; }; TB=$(tarball "$CASE") ;;
  esac
  say "== upgrade.sh --tarball $(basename "$TB") ($CASE); current $(cur)"
  bash "$I/upgrade.sh" --tarball "$TB" --home "$H" --state "$ST" "$@" >"$LOGS/upgrade-$CASE.log" 2>&1
  rc=$?
  grep -vE '^\s*$' "$LOGS/upgrade-$CASE.log" | grep -E "signature|protocol|health|current ->|upgraded|rolled back|refus|error|failed|staging|restart|CRITICAL|mismatch|pinned" | sed 's/^/    /'
  say "exit $rc; current $(cur)"
  ;;
rollback)
  I=$(kit)
  bash "$I/upgrade.sh" --rollback --home "$H" --state "$ST" "$@" >"$LOGS/rollback.log" 2>&1
  rc=$?
  sed 's/^/    /' "$LOGS/rollback.log" | grep -E "health|current ->|rolled back|error|CRITICAL"
  say "exit $rc; current $(cur)"
  ;;
pyre)
  "$P" "$@"
  ;;
startx)
  # startx HARNESS TITLE [pyre start args...]: start a session; sid saved.
  HN=${1:?harness}; T=${2:?title}; shift 2
  mkdir -p "$WORK/$T"
  "$P" start --node "$(cat "$W/node")" --harness "$HN" --cwd "$WORK/$T" --title "$T" "$@" --json >"$W/s-$T.json" || exit 1
  S=$(jget "d['session']['session']['session_id']" <"$W/s-$T.json")
  echo "$S" >"$W/sid-$T"
  say "$HN session $T = $S"
  ;;
state)
  # state TITLE: run id, state and reason of a session.
  "$P" sessions show "$(cat "$W/sid-${1:?title}")" --json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin); r = d.get("run") or {}
print("run %s state %s (%s) gen %s runtime %s" % (r.get("run_id"), r.get("state"), r.get("state_reason", ""), r.get("env_generation_id") or "-", (r.get("runtime") or {}).get("version", "-")))'
  ;;
codexpick)
  # One rota Codex account that is not the live default login and not
  # locked (expired-at-rest access tokens are allowed: the checkout
  # refreshes inside the Pyre profile). Name stays on this host.
  umask 077
  python3 - "$W/rota-codex-name" <<'PY'
import json, os, sys
from pathlib import Path
home = Path(os.environ.get("CODEX_HOME", Path.home() / ".codex"))
try:
    live = (json.load(open(home / "auth.json")).get("tokens") or {}).get("account_id")
except Exception:
    live = None
pick = None
for d in sorted((home / "accounts").glob("*")):
    f = d / "auth.json"
    if not f.is_file() or (d / "pyre-lock").exists():
        continue
    try:
        t = json.load(open(f)).get("tokens") or {}
    except Exception:
        continue
    if not t.get("refresh_token") or (t.get("account_id") and t.get("account_id") == live):
        continue
    pick = d.name
    break
if not pick:
    print("no eligible codex account"); sys.exit(1)
open(sys.argv[1], "w").write(pick + "\n")
print("picked acct-X (codex, non-default, unlocked)")
PY
  ;;
register-codex)
  "$P" accounts register --node "$(cat "$W/node")" --provider openai --rota "$(cat "$W/rota-codex-name")" --label "${1:-acct-X}" --json 2>&1 |
    sed -E 's/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+/<redacted>/g' | python3 -c '
import json, sys
raw = sys.stdin.read()
try:
    d = json.loads(raw)
    a = d.get("account") or d
    print(json.dumps({k: a.get(k) for k in ("account_id", "label", "provider", "state") if k in a}), "verified=%s" % d.get("verified"))
except ValueError:
    print(raw[:400])'
  ;;
envprep2)
  # A15 project-scope fixtures (W-envupd): node (global) and project
  # profiles for both harnesses. The env home holds a synthetic user setup.
  E=$W/envhome
  mkdir -p "$E/.claude/skills/user-own-skill" "$E/.codex" "$W/src/skill-home" "$W/profiles"
  printf '{\n  "cleanupPeriodDays": 20,\n  "env": {"SYNTHETIC_USER_SETTING": "kept"}\n}\n' >"$E/.claude/settings.json"
  printf '# synthetic user codex config (must be preserved)\nmodel_reasoning_effort = "medium"\n' >"$E/.codex/config.toml"
  printf -- '---\nname: user-own-skill\ndescription: synthetic user skill (must be preserved)\n---\nsynthetic\n' >"$E/.claude/skills/user-own-skill/SKILL.md"
  printf -- '---\nname: wenv-home-skill\ndescription: synthetic managed skill installed into the env home (test only)\n---\nv1\n' >"$W/src/skill-home/SKILL.md"
  PY=$(command -v python3)
  python3 - "$W" "$PY" <<'PY'
import json, sys
w, py = sys.argv[1], sys.argv[2]
def mcp(tag):
    return {"name": "wenv-scope", "command": py, "args": [w + "/kit/synth_mcp.py", tag, w + "/mcp.log"]}
def hook(tag):
    return {"harness": "claude", "event": "SessionStart",
            "command": 'echo "%s scope=$WENV_SCOPE gen=$PYRE_ENV_GENERATION" >> %s/hook.log' % (tag, w)}
g1 = {"name": "default", "mcp_servers": [mcp("node")], "hooks": [hook("node-hook")],
      "settings": {"claude.env.WENV_SCOPE": "node", "codex.model_reasoning_effort": "high"},
      "skills": [{"name": "wenv-home-skill", "harness": "claude", "source_dir": w + "/src/skill-home", "delivery": "home"}]}
def proj(tag):
    return {"name": "default", "mcp_servers": [mcp(tag)], "hooks": [hook("project-hook")],
            "settings": {"claude.env.WENV_SCOPE": tag, "codex.model_reasoning_effort": "low"}}
for n, p in (("g1", g1), ("pp1", proj("project-v1")), ("pp2", proj("project-v2"))):
    json.dump(p, open("%s/profiles/%s.json" % (w, n), "w"), indent=2)
PY
  ls "$W/profiles"
  ;;
claudeinfo)
  # claudeinfo TITLE: the run's single --settings file (managed keys merged
  # with Pyre's hooks) and the MCP servers of its generation.
  R=$(run_of "$(cat "$W/sid-${1:?title}")")
  python3 - "$ST/runs/$R" <<'PY'
import json, os, sys
d = sys.argv[1]
s = json.load(open(os.path.join(d, "pyre-hooks.json")))
m = json.load(open(os.path.join(d, "run.json")))
hooks = s.get("hooks", {})
pyre = sum(1 for ev, gs in hooks.items() for g in gs for h in g.get("hooks", []) if " hook claude " in h.get("command", ""))
other = ["%s: %s" % (ev, h.get("command", "")[:60]) for ev, gs in hooks.items() for g in gs for h in g.get("hooks", []) if " hook claude " not in h.get("command", "")]
print("managed_settings: %s" % m.get("managed_settings", "-").replace(os.environ.get("HOME", "~"), "~"))
print("env: %s" % s.get("env"))
print("pyre hook entries: %d; managed hook entries: %s" % (pyre, other))
PY
  ;;
codexinfo)
  # codexinfo TITLE: the run's app-server (pid, executable) and the keys of
  # its per-thread config (values of PYRE_* identity keys are not printed).
  R=$(run_of "$(cat "$W/sid-${1:?title}")")
  python3 - "$ST/runs/$R" "$ST" <<'PY'
import json, os, subprocess, sys, glob
d, st = sys.argv[1], sys.argv[2]
m = json.load(open(os.path.join(d, "run.json")))
cfg = {}
p = os.path.join(d, "thread-config.json")
if os.path.exists(p):
    cfg = json.load(open(p))
print("thread %s home_key %s executable %s" % (m.get("thread_id"), m.get("home_key"), (m.get("executable") or "(default)").replace(st, "<state>")))
for k in sorted(cfg):
    v = cfg[k]
    if "PYRE_" in k or k == "mcp_servers.pyre":
        v = "<identity>"
    print("  config %s = %s" % (k, json.dumps(v)[:120]))
sock = None
for s in glob.glob(os.path.join(st, "tmux", "*.sock")):
    r = subprocess.run(["tmux", "-S", s, "display", "-p", "-t", "=codex-as-%s:" % m.get("home_key"), "#{pane_pid}"], capture_output=True, text=True)
    if r.returncode == 0 and r.stdout.strip():
        pid = int(r.stdout.strip())
        def kids(p):
            out = []
            for q in os.listdir("/proc"):
                if q.isdigit():
                    try:
                        if int(open("/proc/%s/stat" % q).read().rsplit(")", 1)[1].split()[1]) == p:
                            out.append(int(q))
                    except Exception:
                        pass
            return out
        todo = [pid]
        while todo:
            q = todo.pop(0)
            try:
                exe = os.readlink("/proc/%d/exe" % q)
                argv = open("/proc/%d/cmdline" % q, "rb").read().split(b"\0")
            except Exception:
                continue
            a = " ".join(x.decode(errors="replace") for x in argv[:3] if x)
            if "codex" in exe or "codex" in a:
                print("  app-server proc %d exe %s argv %s" % (q, exe.replace(st, "<state>"), a.replace(st, "<state>")[:160]))
            todo += kids(q)
PY
  ;;
hooklog)
  [ -f "$W/hook.log" ] && cat "$W/hook.log" || echo "(no managed hook ran yet)"
  ;;
ownercheck)
  # The owner's real homes are never targets: digests, mtimes and counts only.
  for f in "$HOME/.claude/settings.json" "$HOME/.codex/config.toml" "$HOME/.codex/auth.json" "$HOME/.claude.json"; do
    [ -f "$f" ] && echo "$(basename "$(dirname "$f")")/$(basename "$f"): sha256 $(sha256sum "$f" | cut -c1-16) mtime $(stat -c %Y "$f")"
  done
  echo "~/.claude/skills wenv entries: $(ls "$HOME/.claude/skills" 2>/dev/null | grep -c wenv)"
  echo "~/.codex entries: $(ls -A "$HOME/.codex" 2>/dev/null | wc -l)"
  ;;
nodepid)
  pid=$(cat "$RUN/node.pid")
  echo "node pid $pid exe $(readlink "/proc/$pid/exe" 2>/dev/null || ps -o comm= -p "$pid") version $("$PREFIX/current/bin/pyred" status --state "$ST" --json 2>/dev/null | jget "d['versions'].get('pyred')")"
  ;;
pyredupd)
  # pyredupd stage|activate RELDIR: in-product pyred update from an uploaded release.
  A=${1:?stage|activate}; R=${2:?release dir}
  TB=$(tarball "$R"); V=$(basename "$TB" | sed -E 's/^pyre-(.*)-[a-z]+-[a-z0-9]+\.tar\.gz$/\1/')
  case "$A" in
    stage) "$P" update stage --node "$(cat "$W/node")" --tool pyred --source "$TB" 2>&1 | sed "s|$W|<w>|g" ;;
    activate) "$P" update activate --node "$(cat "$W/node")" --tool pyred --version "$V" 2>&1 | sed "s|$W|<w>|g" ;;
  esac
  ;;
daemonlog)
  # daemonlog [N]: the daemon's handoff/rollback/restart lines (pyred.log and the service log).
  python3 - "$ST/logs/pyred.log" "$LOGS/node-svc.log" "${1:-25}" "$W" <<'PY2'
import json, re, sys
keys = re.compile(r"handing off|first boot|rolling back|restarting through|did not become|exec |superseded|no longer the active|reported update results|SYNTHETIC|pyred starting|daemon started|recovered")
out = []
for path in sys.argv[1:3]:
    try:
        lines = open(path, errors="replace").read().splitlines()
    except OSError:
        continue
    for l in lines:
        if not keys.search(l):
            continue
        try:
            d = json.loads(l)
            extra = {k: v for k, v in d.items() if k in ("version", "first_boot", "launcher", "self", "active", "err", "count", "runs", "recovered")}
            out.append("%s %s %s" % (d.get("time", "")[11:23], d.get("msg"), extra or ""))
        except ValueError:
            out.append("(stderr) " + l)
for l in out[-int(sys.argv[3]):]:
    print("    " + l.replace(sys.argv[4], "<w>"))
PY2
  ;;
events)
  # events KIND: durable hub events of one kind (e.g. update.result).
  "$P" watch --after 1 --kind "${1:?kind}" --no-notify --json >"$W/events.out" 2>/dev/null &
  wp=$!; sleep 4; kill "$wp" 2>/dev/null; wait "$wp" 2>/dev/null
  python3 - "$W/events.out" "${1}" <<'PY2'
import json, sys
for l in open(sys.argv[1]):
    try:
        e = json.loads(l)
    except ValueError:
        continue
    if e.get("kind") != sys.argv[2]:
        continue
    p = e.get("payload") or {}
    print("    #%s %s %s %s %s -> %s %s" % (e.get("cursor"), e.get("occurred_at", "")[11:19], p.get("tool"), p.get("version"), p.get("outcome"), p.get("reverted_to") or "-", p.get("detail", "")))
PY2
  ;;
upd)
  # upd ACTION [args...]: pyre update ACTION --node <this node> ...
  A=${1:?action}; shift
  "$P" update "$A" --node "$(cat "$W/node")" "$@"
  ;;
envprep)
  # A15 fixtures: an isolated env home (passed to pyred as --env-home) with a
  # synthetic user setup, synthetic skills/plugin, and three profiles.
  E=$W/envhome
  mkdir -p "$E/.claude/skills/user-own-skill" "$W/src/skill-overlay" "$W/src/skill-home" "$W/src/plugin/.claude-plugin" "$W/src/plugin/commands" "$W/profiles"
  printf '{\n  "cleanupPeriodDays": 20,\n  "env": {"SYNTHETIC_USER_SETTING": "kept"}\n}\n' >"$E/.claude/settings.json"
  printf -- '---\nname: user-own-skill\ndescription: synthetic user skill (must be preserved)\n---\nsynthetic\n' >"$E/.claude/skills/user-own-skill/SKILL.md"
  printf -- '---\nname: wupd-overlay-skill\ndescription: synthetic managed skill delivered per generation (test only)\n---\nv1\n' >"$W/src/skill-overlay/SKILL.md"
  printf -- '---\nname: wupd-home-skill\ndescription: synthetic managed skill installed into the env home (test only)\n---\nv1\n' >"$W/src/skill-home/SKILL.md"
  printf '{"name": "wupd-plugin", "version": "0.0.1", "description": "synthetic test plugin"}\n' >"$W/src/plugin/.claude-plugin/plugin.json"
  printf -- '---\ndescription: synthetic test command\n---\nReply with the word synthetic.\n' >"$W/src/plugin/commands/wupd-hello.md"
  PY=$(command -v python3)
  python3 - "$W" "$PY" <<'PY'
import json, sys
w, py = sys.argv[1], sys.argv[2]
def mcp(name):
    return {"name": "wupd-" + name, "harness": "claude", "command": py, "args": [w + "/kit/synth_mcp.py", name, w + "/mcp.log"]}
base = {
    "skills": [
        {"name": "wupd-overlay-skill", "harness": "claude", "source_dir": w + "/src/skill-overlay", "delivery": "overlay"},
        {"name": "wupd-home-skill", "harness": "claude", "source_dir": w + "/src/skill-home", "delivery": "home"},
    ],
    "plugins": [{"name": "wupd-plugin", "harness": "claude", "dir": w + "/src/plugin"}],
}
p1 = dict(base, name="default", mcp_servers=[mcp("alpha")])
p2 = dict(base, name="default", mcp_servers=[mcp("beta")])
p3 = dict(p2, settings={"claude.cleanupPeriodDays": 45})
for n, p in (("p1", p1), ("p2", p2), ("p3-claude-setting", p3)):
    json.dump(p, open("%s/profiles/%s.json" % (w, n), "w"), indent=2)
PY
  ls "$W/profiles"
  ;;
procinfo)
  # procinfo SESSION: the harness process of a session's pane: executable,
  # the overlay flags and PYRE_ENV_GENERATION (never the whole environment).
  S=$(cat "$W/sid-${1:?title}")
  read -r SOCK TGT < <(pane_of "$S")
  pp=$(tmux -S "$SOCK" display -p -t "$TGT" '#{pane_pid}' 2>/dev/null)
  python3 - "$pp" <<'PY'
import os, sys
root = int(sys.argv[1])
def children(pid):
    out = []
    for d in os.listdir("/proc"):
        if d.isdigit():
            try:
                st = open("/proc/%s/stat" % d).read()
                if int(st.rsplit(")", 1)[1].split()[1]) == pid:
                    out.append(int(d))
            except Exception:
                pass
    return out
todo, seen = [root], []
while todo:
    p = todo.pop(0); seen.append(p); todo += children(p)
for p in seen:
    try:
        argv = open("/proc/%d/cmdline" % p, "rb").read().split(b"\0")
        exe = os.readlink("/proc/%d/exe" % p)
    except Exception:
        continue
    a = [x.decode(errors="replace") for x in argv if x]
    if not a or "claude" not in (exe + " " + a[0]):
        continue
    env = open("/proc/%d/environ" % p, "rb").read().split(b"\0")
    gen = [e.decode().split("=", 1)[1] for e in env if e.startswith(b"PYRE_ENV_GENERATION=")]
    cfg = [e.decode().split("=", 1)[1] for e in env if e.startswith(b"CLAUDE_CONFIG_DIR=")]
    dau = [e.decode().split("=", 1)[1] for e in env if e.startswith(b"DISABLE_AUTOUPDATER=")]
    print("pid %d exe %s" % (p, exe))
    print("  PYRE_ENV_GENERATION=%s CLAUDE_CONFIG_DIR=%s DISABLE_AUTOUPDATER=%s" % (gen[0] if gen else "-", cfg[0] if cfg else "-", dau[0] if dau else "-"))
    for x in a:
        if x.startswith(("--mcp-config", "--plugin-dir", "--settings")):
            print("  " + x)
        if x.startswith("--mcp-config="):
            import json
            for n, d in sorted(json.load(open(x.split("=", 1)[1])).get("mcpServers", {}).items()):
                print("    mcp %s: %s %s" % (n, d.get("command"), " ".join(d.get("args", []))))
    break
PY
  ;;
rotapick)
  # Choose one rota Claude account that is not the active login, not checked
  # out by anyone, and has a healthy login (rota list is local-only). The
  # name stays on this host ($W/rota-name, 0600); output is redacted.
  umask 077
  rota list 2>/dev/null | sed -E 's/\x1b\[[0-9;]*m//g' | python3 -c '
import re, sys
for line in sys.stdin:
    if "active" in line or "in use" in line or "expires" in line or "needs re-auth" in line or " max " not in line:
        continue
    t = line.replace("[", " ").replace("]", " ").split()
    g = re.search(r"login good for (\d+)d", line)
    if len(t) > 2 and t[2] == "max" and g and int(g.group(1)) >= 3:
        print(t[1]); break' >"$W/rota-name"
  [ -s "$W/rota-name" ] && echo "picked acct-A (non-active, unlocked, login good >= 3d)" || { echo "no eligible account"; exit 1; }
  ;;
register)
  "$P" accounts register --node "$(cat "$W/node")" --provider anthropic --rota "$(cat "$W/rota-name")" --label "${1:-acct-A}" --json 2>&1 |
    sed -E 's/[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+/<redacted>/g' | python3 -c '
import json, sys
raw = sys.stdin.read()
try:
    d = json.loads(raw)
    print(json.dumps({k: d.get(k) for k in ("account_id", "label", "provider", "state", "node_id") if k in d} or d)[:400])
except ValueError:
    print(raw[:400])'
  ;;
toolbreak)
  # toolbreak TOOL VERSION: force the staged version's health check to fail
  # (clear the execute bits of its payload; contents are unchanged).
  d="$ST/tools/${1:?tool}/${2:?version}"
  [ -d "$d" ] || { echo "no $d" >&2; exit 1; }
  find "$d" -type f -perm -u+x -print -exec chmod a-x {} + | sed "s|$ST|<state>|"
  ;;
capture)
  S=$(cat "$W/sid-${1:?title}")
  read -r SOCK TGT < <(pane_of "$S")
  tmux -S "$SOCK" capture-pane -p -t "$TGT" | grep -v '^\s*$' | tail -n "${2:-25}"
  ;;
keys)
  # keys TITLE KEY...: send tmux keys to a session pane (dialog answers only).
  T=${1:?title}; shift
  S=$(cat "$W/sid-$T")
  read -r SOCK TGT < <(pane_of "$S")
  tmux -S "$SOCK" send-keys -t "$TGT" "$@"
  ;;
mcplog)
  [ -f "$W/mcp.log" ] && cat "$W/mcp.log" || echo "(no MCP server started yet)"
  ;;
homecheck)
  E=$W/envhome
  echo "env home skills: $(ls "$E/.claude/skills" 2>/dev/null | tr '\n' ' ')"
  for f in "$E"/.claude/skills/*/SKILL.md; do echo "  $(basename "$(dirname "$f")"): $(tail -n 1 "$f") $(ls -A "$(dirname "$f")" | tr '\n' ' ')"; done
  echo "env home settings.json: $(python3 -c "import json;print(json.load(open('$E/.claude/settings.json')))")"
  # The owner's real Claude config is never a target: record digests only.
  for f in "$HOME/.claude/settings.json"; do
    [ -f "$f" ] && echo "real $(basename "$f"): sha256 $(sha256sum "$f" 2>/dev/null | cut -c1-16 || shasum -a 256 "$f" | cut -c1-16) mtime $(stat -c %Y "$f" 2>/dev/null || stat -f %m "$f")"
  done
  echo "real ~/.claude/skills: $(ls "$HOME/.claude/skills" 2>/dev/null | grep -c wupd) wupd entries"
  ;;
svc)
  # The node under the REAL service manager, as a temporary unit/agent built
  # from the unit install.sh rendered into the fake home (only its own
  # label/unit name and log paths differ; nothing persistent is installed):
  #   Linux: a transient systemd user service (systemd-run --user) with the
  #          rendered [Service]/[Unit] policy and scope settings; runs go to
  #          transient scopes in <tag>-runs.slice (not pyre-runs.slice).
  #   macOS: the rendered LaunchAgent with its own label, loaded with
  #          launchctl bootstrap (never `launchctl enable`).
  #   svc start|status|kill|wait-restart N|stop|clean
  TAG=$(basename "$W")              # pyre-wa16
  UNIT="$TAG-node"                  # systemd: pyre-wa16-node.service
  SLICE="$TAG-runs.slice"
  LABEL="dev.pyre.test.${TAG#pyre-}" # launchd: dev.pyre.test.wa16
  PLIST="$W/$LABEL.plist"
  DOM="gui/$(id -u)"
  SVCLOG="$LOGS/node-svc.log"
  sub=${1:?start|status|kill|wait-restart|stop|clean}; shift
  svc_pid() {
    if command -v systemd-run >/dev/null 2>&1; then
      systemctl --user show "$UNIT" -p MainPID --value 2>/dev/null
    else
      launchctl print "$DOM/$LABEL" 2>/dev/null | awk '$1 == "pid" {print $3; exit}'
    fi
  }
  svc_restarts() {
    if command -v systemd-run >/dev/null 2>&1; then
      systemctl --user show "$UNIT" -p NRestarts --value 2>/dev/null
    else
      launchctl print "$DOM/$LABEL" 2>/dev/null | awk '$1 == "runs" {print $3; exit}'
    fi
  }
  case "$sub" in
  start)
    bash "$KIT/rehearsal-supervisor.sh" stop node >/dev/null 2>&1
    PASS="HOME=$H USER=$(id -un) LOGNAME=$(id -un) PYRE_RELEASE_ALLOWED_SIGNERS=$PYRE_RELEASE_ALLOWED_SIGNERS PYRE_DAEMON_HANDOFF_TIMEOUT=$PYRE_DAEMON_HANDOFF_TIMEOUT"
    if command -v systemd-run >/dev/null 2>&1; then
      systemctl --user cat "$UNIT.service" >/dev/null 2>&1 && { echo "refusing: $UNIT already exists" >&2; exit 2; }
      ARGS=()
      while IFS= read -r -d '' a; do ARGS+=("$a"); done < <(python3 - "$H/.config/systemd/user/pyred.service" "$UNIT" "$SLICE" "$H" "$SVCLOG" $PASS <<'PY'
import shlex, sys
unit, name, slice_, home, log = sys.argv[1:6]
extra = sys.argv[6:]
def unesc(w):
    return w.replace("%%", "\x00").replace("$$", "$").replace("\x00", "%").replace("%h", home)
sect, args, env, props = None, [], [], []
for line in open(unit):
    line = line.strip()
    if not line or line.startswith("#"):
        continue
    if line.startswith("["):
        sect = line
        continue
    k, _, v = line.partition("=")
    if k == "ExecStart":
        args = [unesc(w) for w in shlex.split(v)]
    elif k == "Environment":
        env += [unesc(w) for w in shlex.split(v)]
    elif sect == "[Unit]" and k.startswith("StartLimit"):
        props.append("%s=%s" % (k, v))
    elif sect == "[Service]" and k not in ("WorkingDirectory", "SyslogIdentifier"):
        props.append("%s=%s" % (k, unesc(v)))
env = [e for e in env if not e.startswith("PYRE_SYSTEMD_SLICE=")]
env.append("PYRE_SYSTEMD_SLICE=" + slice_)
out = ["--user", "--unit=" + name, "--description=Pyre test node (temporary, " + name + ")"]
out += ["--property=" + p for p in props]
out += ["--property=WorkingDirectory=" + home, "--property=StandardOutput=append:" + log,
        "--property=StandardError=append:" + log, "--property=SyslogIdentifier=" + name]
out += ["--setenv=" + e for e in env + extra]
out += ["--"] + args
sys.stdout.write("".join(x + "\0" for x in out))
PY
)
      echo "systemd-run ${ARGS[*]}" | sed "s|$HOME|~|g" | tr ' ' '\n' | grep -E '^--(property|unit)|PYRE_SYSTEMD' | tr '\n' ' '; echo
      systemd-run "${ARGS[@]}" || exit 1
    else
      launchctl print "$DOM/$LABEL" >/dev/null 2>&1 && { echo "refusing: $LABEL already loaded" >&2; exit 2; }
      cp "$H/Library/LaunchAgents/dev.jhh3.pyre.pyred.plist" "$PLIST"
      /usr/libexec/PlistBuddy -c "Set :Label $LABEL" "$PLIST"
      for kv in $PASS; do
        k=${kv%%=*} v=${kv#*=}
        case "$k" in HOME|USER|LOGNAME) continue ;; esac
        /usr/libexec/PlistBuddy -c "Add :EnvironmentVariables:$k string $v" "$PLIST"
      done
      /usr/libexec/PlistBuddy -c "Set :StandardOutPath $SVCLOG" -c "Set :StandardErrorPath $SVCLOG" "$PLIST"
      plutil -lint "$PLIST" >/dev/null || exit 1
      echo "launchctl bootstrap $DOM $PLIST (KeepAlive $(plutil -extract KeepAlive json -o - "$PLIST"), ThrottleInterval $(plutil -extract ThrottleInterval raw "$PLIST"), AbandonProcessGroup $(plutil -extract AbandonProcessGroup raw "$PLIST"))" | sed "s|$HOME|~|g"
      launchctl bootstrap "$DOM" "$PLIST" || exit 1
    fi
    echo "$UNIT $LABEL" >"$W/svc"
    : >"$W/svc-used"
    if wait_link 90; then say "node connected under the service manager"; else say "node did not connect"; tail -n 20 "$SVCLOG"; exit 1; fi
    bash "$0" svc status
    ;;
  status)
    pid=$(svc_pid)
    if command -v systemd-run >/dev/null 2>&1; then
      echo "unit $UNIT: $(systemctl --user show "$UNIT" -p ActiveState -p SubState -p MainPID -p NRestarts -p Result -p ExecMainStatus 2>/dev/null | tr '\n' ' ')"
      [ -n "$pid" ] && [ "$pid" != 0 ] && echo "  main pid $pid exe $(readlink "/proc/$pid/exe" | sed "s|$W|<w>|") cgroup $(cut -d: -f3 "/proc/$pid/cgroup" | sed 's|.*/user@[0-9]*.service/||')"
      for s in "$ST"/tmux/*.sock; do
        [ -S "$s" ] || continue
        tp=$(tmux -S "$s" display -p '#{pid}' 2>/dev/null) || continue
        echo "  tmux server $tp cgroup $(cut -d: -f3 "/proc/$tp/cgroup" | sed 's|.*/user@[0-9]*.service/||')"
      done
    else
      echo "agent $LABEL: $(launchctl print "$DOM/$LABEL" 2>/dev/null | awk -F' = ' '/^\t[a-z]/ && $1 ~ /^\t(state|pid|runs|last exit code|last terminating signal)$/ {sub(/^\t/, "", $1); printf "%s=%s; ", $1, $2}')"
      [ -n "$pid" ] && echo "  main pid $pid exe $(ps -o comm= -p "$pid" | sed "s|$W|<w>|") pgid $(ps -o pgid= -p "$pid" | tr -d ' ')"
      for s in "$ST"/tmux/*.sock; do
        [ -S "$s" ] || continue
        tp=$(tmux -S "$s" display -p '#{pid}' 2>/dev/null) || continue
        echo "  tmux server $tp ppid $(ps -o ppid= -p "$tp" | tr -d ' ') pgid $(ps -o pgid= -p "$tp" | tr -d ' ')"
      done
    fi
    "$PREFIX/current/bin/pyred" status --state "$ST" --json 2>/dev/null | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("  pyred status: version %s link %s live_runs %s" % (d["versions"].get("pyred"), d.get("diagnostics", {}).get("link"), d.get("live_runs")))'
    ;;
  kill)
    pid=$(svc_pid)
    [ -n "$pid" ] && [ "$pid" != 0 ] || { echo "no main pid" >&2; exit 1; }
    say "SIGKILL main pid $pid (restarts so far: $(svc_restarts))"
    kill -9 "$pid"
    ;;
  restart)
    # What upgrade.sh does with a real service manager.
    pid=$(svc_pid)
    if command -v systemd-run >/dev/null 2>&1; then
      say "systemctl --user restart $UNIT (main pid $pid)"
      systemctl --user restart "$UNIT"
    else
      say "launchctl kickstart -k $DOM/$LABEL (main pid $pid)"
      launchctl kickstart -k "$DOM/$LABEL"
    fi
    ;;
  journal)
    # The service manager's own record of exits and restarts.
    if command -v systemd-run >/dev/null 2>&1; then
      journalctl --user -u "$UNIT" --no-pager -o short-iso 2>/dev/null | grep -E "Main process exited|Failed with result|Scheduled restart|Started|Stopp|Deactivated" | sed -E 's/^([^ ]+) [^ ]+ [^ ]+: /    \1 /' | tail -n "${1:-20}"
    else
      launchctl print "$DOM/$LABEL" 2>/dev/null | awk -F' = ' '$1 ~ /^\t(state|pid|runs|last exit code|last terminating signal|immediate reason)$/ {sub(/^\t/, "", $1); print "    " $1 " = " $2}'
    fi
    ;;
  wait-restart)
    # wait-restart OLDPID: until the service manager runs a new main pid and
    # the node is connected again.
    old=${1:?old pid}
    for i in $(seq 1 120); do
      pid=$(svc_pid)
      if [ -n "$pid" ] && [ "$pid" != 0 ] && [ "$pid" != "$old" ]; then break; fi
      sleep 1
    done
    say "new main pid $pid after ${i}s (restarts: $(svc_restarts))"
    if wait_link 120; then say "node connected again"; else say "node did not reconnect"; tail -n 20 "$SVCLOG"; exit 1; fi
    ;;
  stop)
    if command -v systemd-run >/dev/null 2>&1; then
      systemctl --user stop "$UNIT" 2>/dev/null
      systemctl --user reset-failed "$UNIT" 2>/dev/null
      echo "stopped and reset $UNIT: $(systemctl --user show "$UNIT" -p LoadState -p ActiveState --value 2>/dev/null | tr '\n' ' ')"
    else
      launchctl bootout "$DOM/$LABEL" 2>/dev/null
      for _ in $(seq 1 50); do launchctl print "$DOM/$LABEL" >/dev/null 2>&1 || break; sleep 0.2; done
      rm -f "$PLIST"
      echo "booted out $LABEL; plist removed"
    fi
    rm -f "$W/svc"
    ;;
  clean)
    # After teardown killed the tmux servers: nothing of ours is left.
    if command -v systemd-run >/dev/null 2>&1; then
      for u in $(systemctl --user list-units --all --no-legend --plain 'pyre-tmux-*' "$TAG*" 2>/dev/null | awk '{print $1}'); do
        case "$u" in pyre-tmux-*.scope)
          # only scopes in our slice
          [ "$(systemctl --user show "$u" -p Slice --value)" = "$SLICE" ] || continue ;;
        esac
        systemctl --user stop "$u" 2>/dev/null; systemctl --user reset-failed "$u" 2>/dev/null
      done
      systemctl --user stop "$SLICE" "$TAG.slice" 2>/dev/null
      echo "systemctl --user list-units --all 'pyre-*':"
      systemctl --user list-units --all --no-legend --plain 'pyre-*' | awk '{print "  " $1, $3, $4}'
      left=$(systemctl --user list-units --all --no-legend --plain "$TAG*" "$UNIT*" 2>/dev/null | awk '{print $1}')
      echo "units of this test left: ${left:-none}"
    else
      if launchctl print "$DOM/$LABEL" >/dev/null 2>&1; then echo "$LABEL still loaded"; else echo "launchctl print $DOM/$LABEL: fails (not loaded)"; fi
      echo "plist present: $([ -e "$PLIST" ] && echo yes || echo no)"
    fi
    ;;
  esac
  ;;
teardown)
  if [ -x "$P" ]; then
    for f in "$W"/sid-*; do [ -f "$f" ] && "$P" stop "$(cat "$f")" >/dev/null 2>&1; done
    sleep 5
    # Rota checkouts are checked in when an account's last run has exited
    # (Claude exits gracefully, which takes a while): wait for that before
    # the node stops, or the rota locks outlive the work dir.
    for _ in $(seq 1 90); do
      n=$("$P" accounts checkouts --node "$(cat "$W/node")" --json 2>/dev/null | python3 -c 'import json,sys
try: d = json.load(sys.stdin)
except ValueError: d = []
print(sum(1 for c in (d or []) if c.get("state") in ("active", "pending")))' 2>/dev/null)
      [ "${n:-0}" = 0 ] && break
      sleep 2
    done
    echo "rota checkouts still active: ${n:-?}"
  fi
  [ -f "$W/svc" ] && bash "$0" svc stop
  bash "$KIT/rehearsal-supervisor.sh" stop node 2>/dev/null
  bash "$KIT/rehearsal-supervisor.sh" stop hub 2>/dev/null
  for s in "$ST"/tmux/*.sock "$ST"/runs/*/tmux.sock; do [ -S "$s" ] && tmux -S "$s" kill-server 2>/dev/null; done
  sleep 1
  [ -f "$W/svc-used" ] && bash "$0" svc clean
  left=$(ps -axww -o pid=,command= | grep -F "$W" | grep -v grep | grep -v release_update_host || true)
  if [ -n "$left" ]; then echo "processes still referencing $W:"; echo "$left"; fi
  if [ "${1:-}" = --keep ]; then echo "kept $W"; else rm -rf "$W"; echo "removed $W"; fi
  ;;
*) sed -n '2,24p' "$0"; exit 2 ;;
esac
