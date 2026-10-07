#!/bin/sh
set -eu

# End-to-end acceptance for the managed-connectors release. This deliberately
# downloads the real pinned provider archives. GWS can exercise its local auth
# boundary with a synthetic token; set OUTFIT_STRIPE_TEST_KEY to add the optional
# bounded Stripe test-account canary.

repo_root=$(cd -- "$(dirname -- "$0")/.." && pwd)
accept_root=$(mktemp -d)
source_home="$accept_root/source-home"
target_home="$accept_root/target-home"
source_hosts="$accept_root/source-hosts"
target_hosts="$accept_root/target-hosts"
backup_path="$accept_root/managed-connectors.outfit"
trap 'rm -rf "$accept_root"' EXIT

if [ -n "${OUTFIT_BINARY:-}" ]; then
  outfit=$OUTFIT_BINARY
else
  outfit="$accept_root/outfit"
  (cd "$repo_root" && go build -o "$outfit" ./cmd/outfit)
fi

run_source() {
	OUTFIT_AGENT=0 OUTFIT_HOME="$source_home" OUTFIT_VAULT_BACKEND=file \
    CODEX_HOME="$source_hosts/codex" CLAUDE_CONFIG_DIR="$source_hosts/claude" \
    "$outfit" "$@"
}

run_target() {
	OUTFIT_AGENT=0 OUTFIT_HOME="$target_home" OUTFIT_VAULT_BACKEND=file \
    CODEX_HOME="$target_hosts/codex" CLAUDE_CONFIG_DIR="$target_hosts/claude" \
    "$outfit" "$@"
}

assert_selected_gws_skills() {
  host_root=$1
  find "$host_root" -type f -name SKILL.md | grep 'gws-shared'
  find "$host_root" -type f -name SKILL.md | grep 'gws-gmail'
  if find "$host_root" -type f -name SKILL.md | grep -q 'gws-drive'; then
    echo 'Gmail-only connection unexpectedly installed a Drive skill' >&2
    exit 1
  fi
}

run_source setup
run_source tool install stripe-cli
run_source tool status stripe-cli | grep '1.50.4'
stripe_binary=$(run_source tool path stripe-cli)
"$stripe_binary" --map=json >"$accept_root/stripe-map.json"
python3 - "$accept_root/stripe-map.json" <<'PY'
import json
import sys

tree = json.load(open(sys.argv[1], encoding="utf-8"))
commands = {item["name"]: {child["name"] for child in item.get("commands", [])} for item in tree["commands"]}
required = {
    "balance": {"retrieve"}, "balance_transactions": {"list"},
    "customers": {"list", "retrieve", "search", "create", "update", "delete"},
    "subscriptions": {"list", "retrieve", "create", "update", "cancel"},
    "invoices": {"list", "retrieve", "create_preview", "create", "finalize_invoice", "void_invoice"},
    "payment_intents": {"list", "retrieve"}, "charges": {"list", "retrieve"},
    "refunds": {"list", "retrieve", "create"}, "events": {"list", "retrieve", "resend"},
    "webhook_endpoints": {"list", "retrieve"},
}
for resource, operations in required.items():
    missing = operations - commands.get(resource, set())
    if missing:
        raise SystemExit(f"Stripe {resource} is missing pinned operations: {sorted(missing)}")
for root_command in ("get", "listen", "logs", "trigger"):
    if root_command not in commands:
        raise SystemExit(f"Stripe command tree is missing {root_command}")
PY
run_source connector add google-workspace \
  --name gws-gmail \
  --mode cli \
  --credential-method managed-access-token \
  --set GOOGLE_WORKSPACE_CLI_TOKEN=acceptance-token \
  --set GWS_SERVICES=gmail
run_source run gws-gmail auth_status | tee "$accept_root/gws-source.json"
grep 'token_env_var' "$accept_root/gws-source.json"
run_source describe gws-gmail | tee "$accept_root/gws-description.txt"
grep '^capabilities: gmail$' "$accept_root/gws-description.txt"
grep '^schema_digest: sha256:' "$accept_root/gws-description.txt"
if run_source run gws-gmail drive_files_list >/dev/null 2>&1; then
  echo 'Gmail-only connection unexpectedly exposed a Drive operation' >&2
  exit 1
fi
run_source plugin sync --host both
assert_selected_gws_skills "$source_hosts/codex/skills"
assert_selected_gws_skills "$source_hosts/claude/skills"

if [ -n "${OUTFIT_STRIPE_TEST_KEY:-}" ]; then
  run_source connector add stripe \
    --name stripe-test \
    --mode cli \
    --credential-method managed-restricted-key \
    --set "STRIPE_API_KEY=$OUTFIT_STRIPE_TEST_KEY"
  run_source run stripe-test customers_list --arg limit=1 >"$accept_root/stripe-read.json"
  grep '"data"' "$accept_root/stripe-read.json"
else
  echo 'OUTFIT_STRIPE_TEST_KEY is unset; verified the real Stripe CLI archive, but skipped the optional account canary.'
fi

OUTFIT_BACKUP_PASSPHRASE='managed-connectors-acceptance' run_source backup create "$backup_path"
OUTFIT_BACKUP_PASSPHRASE='managed-connectors-acceptance' run_target backup restore "$backup_path"
run_target run gws-gmail auth_status | tee "$accept_root/gws-restored.json"
grep 'token_env_var' "$accept_root/gws-restored.json"
run_target plugin sync --host both
assert_selected_gws_skills "$target_hosts/codex/skills"
assert_selected_gws_skills "$target_hosts/claude/skills"
run_target doctor | tee "$accept_root/doctor.txt"
grep '^gws-gmail[[:space:]]*ok$' "$accept_root/doctor.txt"

echo 'managed connector acceptance passed: real tools, runtime, selective skills, backup, and fresh-home restore'
