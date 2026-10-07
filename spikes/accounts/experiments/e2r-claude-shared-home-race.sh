#!/bin/bash
# E2r: repeat the shared-profile refresh race (N processes x T trials) to check that
# Claude's per-config-dir lock serialises refresh (expect exactly one /v1/oauth/token per trial).
set -u
export SPIKE_ROOT="${SPIKE_ROOT:-${TMPDIR:-/tmp}/pyre-wp02-e2r}"; rm -rf "$SPIKE_ROOT"
source "$(dirname "$0")/../lib.sh"
N="${N:-3}"; T="${T:-3}"
mk_ca
mkdir -p "$SPIKE_ROOT/proj" && cd "$SPIKE_ROOT/proj"
for t in $(seq "$T"); do
  start_fake "$SPIKE_ROOT/fake-$t.jsonl" --mitm-ca "$MITM_CA" --sleep-refresh 2
  mk_claude_home "$SPIKE_ROOT/S$t" "R" expired
  for i in $(seq "$N"); do
    $TIMEOUT 90 $(claude_env "$SPIKE_ROOT/S$t") NODE_EXTRA_CA_CERTS="$MITM_CA" ANTHROPIC_CUSTOM_HEADERS="x-pyre-run: t$t-p$i" \
      "$CLAUDE_BIN" -p "hi" --output-format json </dev/null > "$SPIKE_ROOT/out-$t-$i.json" 2>&1 &
  done
  wait $(jobs -p | grep -v "^$FAKE_PID$")
  stop_fake
  ok=$(grep -l '"result":"pyre-fake-reply' "$SPIKE_ROOT"/out-$t-*.json | wc -l | tr -d ' ')
  refresh=$(grep -c '"/v1/oauth/token"' "$SPIKE_ROOT/fake-$t.jsonl")
  reused=$(grep -c 'REUSED' "$SPIKE_ROOT/fake-$t.jsonl")
  echo "trial $t: processes=$N succeeded=$ok token_endpoint_calls=$refresh reused=$reused final=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["claudeAiOauth"]["refreshToken"])' "$SPIKE_ROOT/S$t/.credentials.json")"
done
