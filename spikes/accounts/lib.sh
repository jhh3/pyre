# shellcheck shell=bash
# Shared helpers for WP02 account-isolation experiments. Source from bash.
# Everything is synthetic and lives under $SPIKE_ROOT (a scratch dir). Never point these
# helpers at a real ~/.claude, ~/.codex, or rota snapshot.

SPIKE_DIR="$(CDPATH= cd -- "$(dirname "${BASH_SOURCE[0]}")" >/dev/null && pwd)"
: "${SPIKE_ROOT:?set SPIKE_ROOT to a scratch directory}"
mkdir -p "$SPIKE_ROOT"
case "$SPIKE_ROOT" in "$HOME"/.claude*|"$HOME"/.codex*) echo "refusing SPIKE_ROOT=$SPIKE_ROOT" >&2; return 1;; esac

CLAUDE_BIN="${CLAUDE_BIN:-$(command -v claude)}"
CODEX_BIN="${CODEX_BIN:-$(command -v codex)}"
FAKE_HOME="$SPIKE_ROOT/home"          # synthetic $HOME so nothing reads the user's dotfiles
mkdir -p "$FAKE_HOME"

now_ms() { python3 -c 'import time;print(int(time.time()*1000))'; }

# mk_claude_home DIR LABEL [expired]
mk_claude_home() {
  local dir="$1" lab="$2" exp="${3:-}" t
  mkdir -p "$dir"
  t=$(now_ms)
  if [ "$exp" = expired ]; then t=$((t - 3600000)); else t=$((t + 3600000)); fi
  cat > "$dir/.credentials.json" <<EOF
{"claudeAiOauth":{"accessToken":"pyre-synthetic-$lab-access-initial","refreshToken":"pyre-synthetic-$lab-refresh-initial","expiresAt":$t,"scopes":["user:inference","user:profile","user:sessions:claude_code"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}
EOF
  chmod 600 "$dir/.credentials.json"
  cat > "$dir/.claude.json" <<EOF
{"hasCompletedOnboarding":true,"numStartups":1,"autoUpdates":false,
 "oauthAccount":{"accountUuid":"00000000-0000-4000-8000-00000000000$lab","emailAddress":"acct-$lab@synthetic.invalid","organizationUuid":"00000000-0000-4000-9000-00000000000$lab","organizationName":"synthetic-org-$lab"}}
EOF
}

# mk_codex_home DIR LABEL [expired]
mk_codex_home() {
  local dir="$1" lab="$2" exp="${3:-}"
  mkdir -p "$dir"
  python3 - "$dir" "$lab" "$exp" <<'EOF'
import base64, json, sys, time, datetime
d, lab, exp = sys.argv[1:4]
enc = lambda o: base64.urlsafe_b64encode(json.dumps(o).encode()).rstrip(b"=").decode()
now = int(time.time())
e = now - 3600 if exp == "expired" else now + 3600
claims = {"pyre_synthetic": True, "pyre_label": lab, "exp": e, "iat": now - 60,
          "email": f"acct-{lab}@synthetic.invalid", "sub": f"synthetic-sub-{lab}",
          "https://api.openai.com/auth": {"chatgpt_plan_type": "pro",
                                          "chatgpt_account_id": f"synthetic-acct-{lab}",
                                          "chatgpt_user_id": f"synthetic-user-{lab}"}}
jwt = lambda c: f"{enc({'alg':'none','typ':'JWT'})}.{enc(c)}.sig"
last = datetime.datetime.now(datetime.timezone.utc)
if exp == "expired":
    last -= datetime.timedelta(days=30)
auth = {"OPENAI_API_KEY": None, "auth_mode": "chatgpt",
        "tokens": {"id_token": jwt(claims), "access_token": jwt({**claims, "kind": "access"}),
                   "refresh_token": f"pyre-synthetic-{lab}-refresh-initial",
                   "account_id": f"synthetic-acct-{lab}"},
        "last_refresh": last.isoformat().replace("+00:00", "Z")}
open(f"{d}/auth.json", "w").write(json.dumps(auth, indent=2))
EOF
  chmod 600 "$dir/auth.json"
}

# start_fake LOG [extra args...]  -> sets FAKE_PORT FAKE_PID
start_fake() {
  local logf="$1"; shift
  local pf="$SPIKE_ROOT/port.$$.$RANDOM"
  python3 "$SPIKE_DIR/fakeprovider.py" --log "$logf" --port-file "$pf" "$@" &
  FAKE_PID=$!
  for _ in $(seq 50); do [ -s "$pf" ] && break; sleep 0.1; done
  FAKE_PORT=$(cat "$pf")
  export FAKE_PORT FAKE_PID
}
stop_fake() { [ -n "${FAKE_PID:-}" ] && kill "$FAKE_PID" 2>/dev/null; wait "$FAKE_PID" 2>/dev/null; true; }

# Minimal, clean environment: no inherited API keys or Claude session vars.
base_env() {
  local pth="$SPIKE_DIR/shims:$(dirname "$CLAUDE_BIN"):$(dirname "$CODEX_BIN"):/usr/bin:/bin:/usr/sbin:/sbin"
  [ "$(uname)" = Darwin ] || pth="$(dirname "$CLAUDE_BIN"):$(dirname "$CODEX_BIN"):/usr/bin:/bin"
  echo env -i HOME="$FAKE_HOME" USER="$USER" LOGNAME="$USER" PATH="$pth" TERM="${TERM:-xterm-256color}" \
    LANG=en_US.UTF-8 TMPDIR="$SPIKE_ROOT/tmp" \
    PYRE_SECURITY_SHIM_LOG="$SPIKE_ROOT/security-shim.jsonl" \
    HTTPS_PROXY="http://127.0.0.1:$FAKE_PORT" https_proxy="http://127.0.0.1:$FAKE_PORT" \
    NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
}
mkdir -p "$SPIKE_ROOT/tmp"

# claude_env HOME_DIR -> env prefix for a Claude run bound to that profile
claude_env() {
  echo "$(base_env)" CLAUDE_CONFIG_DIR="$1" ANTHROPIC_BASE_URL="http://127.0.0.1:$FAKE_PORT" \
    DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
}

# codex_env HOME_DIR -> env prefix for a Codex run bound to that profile
codex_env() {
  echo "$(base_env)" CODEX_HOME="$1" CODEX_REFRESH_TOKEN_URL_OVERRIDE="http://127.0.0.1:$FAKE_PORT/oauth/token"
}
# codex_cfg -> -c overrides pointing ChatGPT-mode traffic at the fake provider
# The built-in openai provider performs "workspace routing discovery" whose response shape
# the fake does not implement, so inference uses a custom provider that still requires
# ChatGPT auth (requires_openai_auth=true): same auth.json, same bearer + chatgpt-account-id.
codex_cfg() {
  echo -c "chatgpt_base_url=\"http://127.0.0.1:$FAKE_PORT/backend-api/\"" \
       -c 'model_provider="pyre"' -c 'model_providers.pyre.name="pyre-fake"' \
       -c "model_providers.pyre.base_url=\"http://127.0.0.1:$FAKE_PORT/backend-api/codex\"" \
       -c 'model_providers.pyre.wire_api="responses"' -c 'model_providers.pyre.requires_openai_auth=true' \
       -c 'model_providers.pyre.supports_websockets=false' \
       -c 'analytics.enabled=false' -c 'check_for_update_on_startup=false'
}

# stat_defaults FILE... -> "path size mtime" without reading content
stat_defaults() {
  local f
  for f in "$@"; do
    if [ "$(uname)" = Darwin ]; then stat -f "%N size=%z mtime=%m" "$f" 2>/dev/null || echo "$f missing"
    else stat -c "%n size=%s mtime=%Y" "$f" 2>/dev/null || echo "$f missing"; fi
  done | sed "s#$HOME#~#"
}

# portable timeout (macOS lacks coreutils timeout)
if command -v timeout >/dev/null; then TIMEOUT=timeout; elif command -v gtimeout >/dev/null; then TIMEOUT=gtimeout
else TIMEOUT=""; fi  # no timeout available: run unbounded

# mk_ca -> creates a throwaway CA under $SPIKE_ROOT/ca (2-day validity), sets MITM_CA
mk_ca() {
  mkdir -p "$SPIKE_ROOT/ca"
  MITM_CA="$SPIKE_ROOT/ca/ca.crt"
  [ -f "$MITM_CA" ] || openssl req -x509 -newkey rsa:2048 -nodes -days 2 \
      -keyout "$SPIKE_ROOT/ca/ca.key" -out "$MITM_CA" -subj "/CN=pyre-wp02-synthetic-ca" \
      -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign" >/dev/null 2>&1
  export MITM_CA
}
