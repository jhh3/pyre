#!/usr/bin/env bash
# jhh3-mem — tests/t-hosts.sh
# Cross-host capture: adapter dispatch, the Codex rollout adapter, host tagging,
# and the one property the whole design exists for — a Codex session surfacing in
# a Claude Code digest from the same store.
set -uo pipefail
TDIR="$(cd -- "${BASH_SOURCE[0]%/*}" && pwd -P)"
ROOT="$(cd -- "$TDIR/.." && pwd -P)"
if [ "${1:-}" = '--list' ]; then
  printf '%s\n' HOST-1 HOST-2 HOST-3 HOST-4 HOST-5 HOST-6 HOST-7 HOST-8 HOST-9; exit 0
fi
PASS=0; FAIL=0
ok()  { PASS=$((PASS+1)); printf '  ok   %-8s %s\n' "$1" "${2:-}"; }
bad() { FAIL=$((FAIL+1)); printf '  FAIL %-8s %s\n' "$1" "${2:-}"; }
check(){ if [ "$2" = "$3" ]; then ok "$1" "$4"; else bad "$1" "$4 — want [$2] got [$3]"; fi; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-hosts.XXXXXX")"
trap 'rm -rf -- "$WORK"' EXIT
STORE="$WORK/store"; mkdir -p "$STORE"
export JHH3_MEM_HOME="$STORE"
CWD="$WORK/repo"; mkdir -p "$CWD"
git -C "$CWD" init -q 2>/dev/null
git -C "$CWD" remote add origin 'git@github.com:jhh3test/hosts.git' 2>/dev/null
NOW="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# --- fixtures: one transcript per host, minimal but shape-accurate --------------
CC="$WORK/cc.jsonl"
printf '{"type":"user","promptSource":"typed","timestamp":"%s","cwd":"%s","message":{"role":"user","content":"add the parser"}}\n' "$NOW" "$CWD" >"$CC"
printf '{"type":"assistant","timestamp":"%s","cwd":"%s","gitBranch":"main","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"%s/a.sh"}},{"type":"text","text":"Added the parser."}]}}\n' "$NOW" "$CWD" "$CWD" >>"$CC"

CX="$WORK/cx.jsonl"
{
  printf '{"timestamp":"%s","type":"session_meta","payload":{"session_id":"cx-abc","cwd":"%s","originator":"codex_exec","cli_version":"0.147.0"}}\n' "$NOW" "$CWD"
  printf '{"timestamp":"%s","type":"event_msg","payload":{"type":"user_message","message":"review the collab server"}}\n' "$NOW"
  printf '{"timestamp":"%s","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\\"cmd\\":\\"rg -n handler backend\\",\\"workdir\\":\\"%s\\"}"}}\n' "$NOW" "$CWD"
  printf '{"timestamp":"%s","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\\"cmd\\":\\"apply_patch <<EOF\\",\\"workdir\\":\\"%s\\"}"}}\n' "$NOW" "$CWD"
  printf '{"timestamp":"%s","type":"event_msg","payload":{"type":"agent_message","message":"Fixed the handler."}}\n' "$NOW"
} >"$CX"

end() { printf '{"session_id":"%s","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionEnd","reason":"other"}\n' "$1" "$2" "$CWD" | bash "$ROOT/scripts/session-end.sh" >/dev/null 2>&1; }
sq() { sqlite3 -bail "$STORE/memory.db" "$@" 2>/dev/null; }

# --- HOST-1/2: dispatch picks the right adapter from content alone --------------
h() { ( . "$ROOT/lib/common.sh"; . "$ROOT/lib/capture.sh"; _jm_cap_detect_host "$1" ) 2>/dev/null; }
check HOST-1 "claude-code" "$(h "$CC")" "a Claude Code transcript dispatches to claude-code"
check HOST-2 "codex"       "$(h "$CX")" "a Codex rollout dispatches to codex"

# --- HOST-3: the Codex adapter produces a usable row ----------------------------
end cc-1 "$CC"; end cx-1 "$CX"
check HOST-3 "2" "$(sq 'select count(*) from entries;')" "both hosts captured into one store"

# --- HOST-4: rows carry their host ----------------------------------------------
check HOST-4 "claude-code codex" \
  "$(sq 'select group_concat(host, " ") from (select distinct host from entries order by host);')" \
  "each row records which host produced it"

# --- HOST-5: Codex mutation detection (no Edit/Write tool exists there) ---------
# One apply_patch and one rg: exactly one mutating command, one read.
check HOST-5 "1" "$(sq "select n_mod from entries where host='codex';")" \
  "a shell-mediated file write is counted as a mutation"

# --- HOST-6: and that keeps Codex rows above the injection floor ----------------
check HOST-6 "1" "$(sq "select injectable from entries where host='codex';")" \
  "a Codex session clears the injection floor"

# --- HOST-7: title falls back to the first user message on Codex ----------------
check HOST-7 "review the collab server" "$(sq "select title from entries where host='codex';")" \
  "Codex rows title from the first user message (no ai-title exists there)"

# --- HOST-8: THE point — cross-host recall --------------------------------------
DIG="$(printf '{"session_id":"next","transcript_path":"%s","cwd":"%s","hook_event_name":"SessionStart","source":"startup"}\n' "$CC" "$CWD" | bash "$ROOT/scripts/session-start.sh" 2>/dev/null)"
if printf '%s' "$DIG" | grep -q 'review the collab server' && printf '%s' "$DIG" | grep -q 'add the parser'; then
  ok HOST-8 "a Codex session and a Claude Code session appear in one digest"
else
  bad HOST-8 "cross-host digest missing a host — got: $(printf '%s' "$DIG" | tr '\n' ' ' | cut -c1-120)"
fi

# --- HOST-9: the OTHER Codex tool-call shape ------------------------------------
# codex 0.147.0 emits apply_patch as response_item/custom_tool_call, with a raw patch
# in payload.input rather than JSON args. A live session writing a file produced
# n_mod=0 until this was handled -- the row then sat under the injection floor.
CX2="$WORK/cx2.jsonl"
{
  printf '{"timestamp":"%s","type":"session_meta","payload":{"session_id":"cx2","cwd":"%s"}}\n' "$NOW" "$CWD"
  printf '{"timestamp":"%s","type":"event_msg","payload":{"type":"user_message","message":"create hello.txt"}}\n' "$NOW"
  printf '{"timestamp":"%s","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch\\n*** Add File: hello.txt\\n+hello\\n*** End Patch\\n"}}\n' "$NOW"
  printf '{"timestamp":"%s","type":"event_msg","payload":{"type":"agent_message","message":"Created it."}}\n' "$NOW"
} >"$CX2"
end cx-2 "$CX2"
check HOST-9 "1|hello.txt" \
  "$(sq "select n_mod || '|' || files_touched from entries where uid=(select uid from entries order by id desc limit 1);")" \
  "an apply_patch custom_tool_call counts as a mutation and yields its real path"

printf '\n  passed %-3s failed %s\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
