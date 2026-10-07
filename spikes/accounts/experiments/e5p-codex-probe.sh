#!/bin/bash
# E5p: find a Codex 0.160 configuration whose inference traffic reaches the fake provider.
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e5p}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
start_fake "$SPIKE_ROOT/fake.jsonl"
mkdir -p "$SPIKE_ROOT/proj" && cd "$SPIKE_ROOT/proj"
mk_codex_home "$SPIKE_ROOT/A" A
BASE=(-c 'cli_auth_credentials_store="file"' -m gpt-5.5 -c 'analytics.enabled=false' -c 'check_for_update_on_startup=false')
echo "### builtin openai provider + chatgpt_base_url/openai_base_url"
$TIMEOUT 60 $(codex_env "$SPIKE_ROOT/A") "$CODEX_BIN" "${BASE[@]}" $(codex_cfg) exec --skip-git-repo-check --json "hi builtin" </dev/null > "$SPIKE_ROOT/b.out" 2> "$SPIKE_ROOT/b.err"
echo "rc=$?"; grep -o '"type":"[a-z.]*"\|pyre-fake-reply label=[A-Za-z]*\|"message":"[^"]\{0,120\}' "$SPIKE_ROOT/b.out" | sort | uniq -c | head
echo "### custom provider requiring ChatGPT auth"
CUSTOM=(-c "chatgpt_base_url=\"http://127.0.0.1:$FAKE_PORT/backend-api/\"" -c 'model_provider="pyre"'
  -c 'model_providers.pyre.name="pyre-fake"' -c "model_providers.pyre.base_url=\"http://127.0.0.1:$FAKE_PORT/backend-api/codex\""
  -c 'model_providers.pyre.wire_api="responses"' -c 'model_providers.pyre.requires_openai_auth=true'
  -c 'model_providers.pyre.supports_websockets=false')
$TIMEOUT 60 $(codex_env "$SPIKE_ROOT/A") "$CODEX_BIN" "${BASE[@]}" "${CUSTOM[@]}" exec --skip-git-repo-check --json "hi custom" </dev/null > "$SPIKE_ROOT/c.out" 2> "$SPIKE_ROOT/c.err"
echo "rc=$?"; grep -o '"type":"[a-z.]*"\|pyre-fake-reply label=[A-Za-z]*\|"message":"[^"]\{0,120\}' "$SPIKE_ROOT/c.out" | sort | uniq -c | head
tail -c 600 "$SPIKE_ROOT/c.err"; echo
stop_fake
python3 "$SPIKE_DIR/showlog.py" "$SPIKE_ROOT/fake.jsonl"
