#!/bin/sh
set -eu

: "${GH_TOKEN:?GH_TOKEN is required for the private release and GitHub connector smoke test}"
install_root="$(mktemp -d)"
state_root="$(mktemp -d)"
host_root="$(mktemp -d)"
workspace_root="$(mktemp -d)"
infer_root="$(mktemp -d)"
trap 'rm -rf "$install_root" "$state_root" "$host_root" "$workspace_root" "$infer_root"' EXIT

OUTFIT_INSTALL_DIR="$install_root" OUTFIT_VERSION="${OUTFIT_VERSION:-}" sh scripts/install.sh
outfit="$install_root/outfit"
export OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file CODEX_HOME="$host_root/codex" CLAUDE_CONFIG_DIR="$host_root/claude"

"$outfit" version
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" setup
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector catalog | grep '^github[[:space:]]'
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector install testdata/smoke-connector --trust
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector add smoke-github --name release-smoke
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector test release-smoke
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" ls | grep '"name": "release-smoke"'
printf '%s\n' 'https://github.com/modelcontextprotocol/go-sdk/issues/1' \
  | OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" context resolve \
  | tee "$state_root/context.txt"
grep '<resource provider="smoke-github" connection="release-smoke" type="github.issue"' "$state_root/context.txt"
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file CODEX_HOME="$host_root/codex" \
  CLAUDE_CONFIG_DIR="$host_root/claude" "$outfit" plugin sync --host both
test -f "$host_root/codex/skills/outfit-release-smoke/SKILL.md"
test -f "$host_root/claude/skills/outfit-release-smoke/SKILL.md"
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" doctor

mkdir -p "$infer_root/.git"
printf '%s\n' '[remote "origin"]' '  url = https://github.com/jhh3/outfit.git' >"$infer_root/.git/config"
printf '%s\n' '# Inference smoke' 'This project deploys with gcloud.' >"$infer_root/README.md"
mkdir -p "$infer_root/connectors"
cp -R testdata/smoke-connector "$infer_root/connectors/gcp"
sed -i 's/^key: smoke-github$/key: gcp/' "$infer_root/connectors/gcp/connector.yaml"
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector install "$infer_root/connectors/gcp" --trust
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector add gcp --name inferred-gcp --mode rest
(
  cd "$infer_root"
  "$outfit" profile infer --write | grep 'wrote proposal'
  grep 'name: inferred-gcp' .outfit/workspace.yaml
  grep 'path: connectors/gcp' .outfit/workspace.yaml
  "$outfit" lock
  "$outfit" apply
  "$outfit" workspace status | grep 'approved: true'
)

mkdir -p "$workspace_root/imported"
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" kit import openapi testdata/import-openapi.yaml \
  --key imported-github --output "$workspace_root/imported"
OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" kit validate "$workspace_root/imported/imported-github"

mkdir -p "$workspace_root/connectors"
cp -R testdata/workspace-smoke/. "$workspace_root/"
cp -R testdata/smoke-connector "$workspace_root/connectors/smoke-github"
(
  cd "$workspace_root"
  "$outfit" lock
  "$outfit" apply --trust-packages
  "$outfit" workspace status | grep 'approved: true'
  "$outfit" explain public-issues get_issue \
    --arg owner=modelcontextprotocol --arg repo=go-sdk --arg number=1 | grep 'policy: allowed'
  "$outfit" run public-issues get_issue \
    --arg owner=modelcontextprotocol --arg repo=go-sdk --arg number=1 --dry-run --json | grep '"executor": "http"'
  if "$outfit" search --cached github >/dev/null 2>&1; then
    echo 'disabled cache unexpectedly allowed search' >&2
    exit 1
  fi
  "$outfit" cache enable --ttl 1h
  "$outfit" run public-issues get_issue \
    --arg owner=modelcontextprotocol --arg repo=go-sdk --arg number=1 >/dev/null
  handle="$(OUTFIT_AGENT=0 "$outfit" search --cached github | awk 'NR == 1 { print $1 }')"
  test -n "$handle"
  "$outfit" context inspect "$handle" | grep '"kind": "operation"'
  "$outfit" cache disable
  "$outfit" cache purge --all | grep 'purged [1-9][0-9]* cached entries'
  "$outfit" cache status | grep 'entries: 0'
)

if [ -n "${OUTFIT_SMOKE_GITHUB_TOKEN:-}" ]; then
  OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector add github --name builtin-github \
    --mode rest --set "TOKEN=$OUTFIT_SMOKE_GITHUB_TOKEN"
  OUTFIT_HOME="$state_root" OUTFIT_VAULT_BACKEND=file "$outfit" connector test builtin-github
fi

OUTFIT_BINARY="$outfit" sh scripts/accept-managed-connectors.sh
