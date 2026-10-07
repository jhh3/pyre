#!/bin/bash
# real-profile-check.sh — USER-RUN ONLY. Not executed by the WP02 worker.
#
# Creates a FRESH, independent device login for one subscription account inside an isolated
# Pyre profile directory and verifies that it is isolated from the default login. It never
# copies, reads out, or prints token values, never touches rota snapshots, and never runs
# rota switch / codex logout / claude logout against the default login.
#
# Narrow scope: proves (1) a native login can live in a per-profile dir, (2) the default login
# and keychain item keep their identity/mtime, (3) the profile's runtime identity is the
# intended account. It does NOT test quota exhaustion, recovery, or cross-host sharing.
#
# Usage:
#   real-profile-check.sh claude-login  <name> [--live]  # browser login into an isolated CLAUDE_CONFIG_DIR
#   real-profile-check.sh claude-token  <name> [--live]  # `claude setup-token` (long-lived, no refresh)
#   real-profile-check.sh codex-login   <name> [--live]  # `codex login --device-auth` into an isolated CODEX_HOME
#   real-profile-check.sh verify        <name> [--live]  # re-run checks for an existing profile
#   real-profile-check.sh remove        <name>           # log out of the PROFILE only and delete it
# --live sends one tiny prompt through the profile (consumes a little of that account's quota).
set -euo pipefail
cmd="${1:-}"; name="${2:-}"; live="${3:-}"
[ -n "$cmd" ] && [ -n "$name" ] || { sed -n '2,22p' "$0"; exit 2; }
[[ "$name" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad profile name" >&2; exit 2; }
ROOT="${PYRE_PROFILE_ROOT:-$HOME/.local/state/pyre/profiles}"
mkdir -p "$ROOT/$name"; chmod 700 "$ROOT" "$ROOT/$name" 2>/dev/null || true
ROOT="$(cd "$ROOT" && pwd -P)"          # canonical: Claude's keychain suffix hashes this exact path
CL="$ROOT/$name/claude"; CX="$ROOT/$name/codex"
for p in "$CL" "$CX"; do
  case "$p" in "$HOME/.claude"|"$HOME/.codex"|"$HOME/.claude/"*|"$HOME/.codex/"*) echo "refusing $p" >&2; exit 2;; esac
done
os="$(uname)"

stat_line() { if [ "$os" = Darwin ]; then stat -f "%N size=%z mtime=%m" "$1" 2>/dev/null || echo "$1 missing"; else stat -c "%n size=%s mtime=%Y" "$1" 2>/dev/null || echo "$1 missing"; fi; }
kc_mdat() { # attributes only; never -w / -g
  [ "$os" = Darwin ] || { echo "n/a"; return; }
  security find-generic-password -s "$1" 2>/dev/null | sed -n 's/.*"mdat"<timedate>=0x[0-9A-F]* *"\([0-9]*Z\).*/\1/p' || true
}
sha8() { printf %s "$1" | { shasum -a 256 2>/dev/null || sha256sum; } | cut -c1-8; }
snapshot_defaults() {
  { stat_line "$HOME/.claude/.credentials.json"; stat_line "$HOME/.codex/auth.json"
    echo "keychain 'Claude Code-credentials' mdat=$(kc_mdat 'Claude Code-credentials')"; } | sed "s#$HOME#~#"
}
default_claude_email() { env -u CLAUDE_CONFIG_DIR -u CLAUDE_CODE_OAUTH_TOKEN claude auth status --json 2>/dev/null | python3 -c 'import json,sys;print(json.load(sys.stdin).get("email",""))' 2>/dev/null || true; }
codex_ident() { # prints "email plan account-id-prefix" decoded locally from id_token claims
  python3 - "$1/auth.json" <<'PY' 2>/dev/null || echo "(none)"
import base64,json,sys
t=json.load(open(sys.argv[1]))["tokens"]; p=t["id_token"].split(".")[1]; p+="="*(-len(p)%4)
c=json.loads(base64.urlsafe_b64decode(p)); a=c.get("https://api.openai.com/auth",{})
print(c.get("email"), a.get("chatgpt_plan_type"), (t.get("account_id") or "")[:8]+"…")
PY
}

echo "== defaults BEFORE (mtime/size only; a concurrently running default session may legitimately refresh these)"
snapshot_defaults | tee "$ROOT/$name/.defaults.before"
DEF_EMAIL="$(default_claude_email)"
DEF_CODEX="$(codex_ident "$HOME/.codex")"

case "$cmd" in
  claude-login)
    mkdir -p "$CL"; chmod 700 "$CL"
    echo "== Claude: log in to the INTENDED account in the browser. Profile: $CL"
    echo "   Expected keychain item (macOS): 'Claude Code-credentials-$(sha8 "$CL")'"
    CLAUDE_CONFIG_DIR="$CL" claude auth login --claudeai ;;
  claude-token)
    mkdir -p "$CL"; chmod 700 "$CL"
    echo "== Claude: generating a long-lived token. Paste it at the hidden prompt below (stored 0600 at $CL/oauth-token)."
    CLAUDE_CONFIG_DIR="$CL" claude setup-token
    read -r -s -p "token: " tok; echo
    umask 077; printf %s "$tok" > "$CL/oauth-token"; unset tok ;;
  codex-login)
    mkdir -p "$CX"; chmod 700 "$CX"
    echo "== Codex: device login into $CX (file store so the profile is self-contained)"
    CODEX_HOME="$CX" codex -c 'cli_auth_credentials_store="file"' login --device-auth ;;
  verify) : ;;
  remove)
    [ -d "$CL" ] && [ ! -f "$CL/oauth-token" ] && CLAUDE_CONFIG_DIR="$CL" claude auth logout || true
    [ -f "$CX/auth.json" ] && CODEX_HOME="$CX" codex logout || true
    read -r -p "delete $ROOT/$name ? [y/N] " a; [ "$a" = y ] && rm -rf "${ROOT:?}/$name"
    exit 0 ;;
  *) echo "unknown command $cmd" >&2; exit 2 ;;
esac

echo "== profile identity"
if [ -d "$CL" ]; then
  if [ -f "$CL/oauth-token" ]; then
    CLAUDE_CONFIG_DIR="$CL" CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3 claude auth status --json 3<"$CL/oauth-token"
    echo "(env/fd token: auth status cannot show the account; use --live and compare usage/identity in the provider UI)"
  else
    CLAUDE_CONFIG_DIR="$CL" claude auth status --json
    [ "$os" = Darwin ] && echo "profile keychain item mdat=$(kc_mdat "Claude Code-credentials-$(sha8 "$CL")")"
    [ -f "$CL/.credentials.json" ] && echo "note: plaintext $CL/.credentials.json present (expected on Linux)"
  fi
fi
[ -f "$CX/auth.json" ] && { CODEX_HOME="$CX" codex login status; echo "codex id_token claims: $(codex_ident "$CX")"; }

if [ "$live" = "--live" ]; then
  echo "== live one-turn check through the profile"
  if [ -f "$CL/oauth-token" ]; then CLAUDE_CONFIG_DIR="$CL" CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=3 claude -p "Reply with OK" 3<"$CL/oauth-token" </dev/null
  elif [ -d "$CL" ]; then CLAUDE_CONFIG_DIR="$CL" claude -p "Reply with OK" </dev/null; fi
  [ -f "$CX/auth.json" ] && CODEX_HOME="$CX" codex exec --skip-git-repo-check "Reply with OK" </dev/null
fi

echo "== defaults AFTER"
snapshot_defaults | tee "$ROOT/$name/.defaults.after"
diff "$ROOT/$name/.defaults.before" "$ROOT/$name/.defaults.after" && echo "default credential files/keychain item: unchanged"
[ "$(default_claude_email)" = "$DEF_EMAIL" ] && echo "default Claude identity: unchanged" || echo "WARNING: default Claude identity changed"
[ "$(codex_ident "$HOME/.codex")" = "$DEF_CODEX" ] && echo "default Codex identity: unchanged" || echo "WARNING: default Codex identity changed"
