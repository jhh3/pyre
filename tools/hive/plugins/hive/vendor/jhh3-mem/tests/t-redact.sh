#!/usr/bin/env bash
# jhh3-mem — tests/t-redact.sh   SPEC §14.3 (RED-1 .. RED-10)
set -uo pipefail

_t_src="${BASH_SOURCE[0]}"
case "$_t_src" in */*) _t_dir="${_t_src%/*}" ;; *) _t_dir="." ;; esac
JT_SUITE=redact
# shellcheck source=./lib/e2-harness.sh
. "$_t_dir/lib/e2-harness.sh"
trap jt_cleanup EXIT INT TERM

jt_store
# shellcheck source=../lib/capture.sh
JM_CAPTURE_SOURCED='' . "$JT_PLUGIN_ROOT/lib/capture.sh"

# red LINE — one line through the shipped ruleset.
red() { printf '%s\n' "$1" | LC_ALL=C sed -E -f "$JT_PLUGIN_ROOT/lib/redact.sed"; }

# ── RED-1 ─ each of the nine patterns produces its own marker ───────────────
jt_eq RED-1 'postgres://postgres:[REDACTED:userinfo]@localhost:5431/acme' \
  "$(red 'postgres://postgres:hunter2@localhost:5431/acme')" '1 url userinfo'
jt_eq RED-1 'sk-ant-api03-[REDACTED:anthropic]' \
  "$(red 'sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjKlMnOpQrStUvWxYz')" '2 anthropic'
jt_eq RED-1 '[REDACTED:aws]' "$(red 'AKIAIOSFODNN7EXAMPLE')" '3 aws'
jt_eq RED-1 'ghp_[REDACTED:github]' \
  "$(red 'ghp_1234567890abcdefghijklmnopqrstuvwxyz')" '4 github'
jt_eq RED-1 '[REDACTED:slack]' \
  "$(red 'xo''xb-123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx')" '5 slack'
jt_eq RED-1 '[REDACTED:jwt]' \
  "$(red 'eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U')" '6 jwt'
jt_eq RED-1 '[REDACTED:privkey]' "$(red '-----BEGIN RSA PRIVATE KEY-----')" '7 privkey'
jt_eq RED-1 '[REDACTED:gcp]' "$(red 'AIzaSyA1234567890abcdefghijklmnopqrstuv')" '8 gcp'
jt_eq RED-1 'SIGNING_KEY=[REDACTED:kv]' "$(red 'SIGNING_KEY=abunchofsecretvalue')" '9 key=value'

# ── RED-1b ─ shapes added after adversarial review. Every one of these was
# verified to pass through the ruleset UNCHANGED before the rule existed.
jt_eq RED-1b '[REDACTED:aws]' "$(red 'ASIAY34FZKBOKMUTVV7A')" 'aws STS temporary (ASIA)'
jt_eq RED-1b '[REDACTED:github]' \
  "$(red 'github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz0123456789')" 'github fine-grained PAT'
jt_eq RED-1b '[REDACTED:openai]' \
  "$(red 'sk''-proj-abcdefghijklmnopqrstuvwxyzT3BlbkFJ0123456789abcdef')" 'openai project key'
jt_eq RED-1b '[REDACTED:openai]' \
  "$(red 'sk-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKL')" 'openai classic key'
jt_eq RED-1b 'Authorization: Bearer [REDACTED:authz]' \
  "$(red 'Authorization: Bearer aaaaaaaaaaaabbbbbbbbbbbbccccccccccc')" 'bearer header'
jt_eq RED-1b 'Authorization: Basic [REDACTED:authz]' \
  "$(red 'Authorization: Basic YWRtaW46c3VwZXJzZWNyZXRwYXNzd29yZA==')" 'basic header'
jt_eq RED-1b '[REDACTED:vendor]' "$(red 'glpat-abcdefghijklmnopqrst')" 'gitlab PAT'
jt_eq RED-1b '[REDACTED:vendor]' "$(red 'dckr_pat_abcdefghijklmnopqrstuvwx')" 'docker hub PAT'
jt_eq RED-1b '[REDACTED:sendgrid]' \
  "$(red 'SG.abcdefghijklmnopqrst.uvwxyz0123456789abcdefg')" 'sendgrid key'
jt_eq RED-1b 'https://hooks.slack.com/services/[REDACTED:webhook]' \
  "$(red 'https://hooks.sl''ack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX')" 'slack webhook'
# The ANTHROPIC rule must still win its prefix-preserving behaviour over the new
# generic sk- rule; ordering is the whole of this assertion.
jt_eq RED-1b 'sk-ant-api03-[REDACTED:anthropic]' \
  "$(red 'sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789AbCdEfGhIjKlMnOpQrStUvWxYz')" 'sk-ant- still wins'

# ── RED-1c ─ a PEM private key loses its BODY, not just its header ──────────
# The old rule replaced the header only, and its comment claimed "the header
# alone is enough to blank the block". For a line-oriented sed that is false:
# the base64 body and the END line were persisted verbatim.
pem_multi="$(printf -- '-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAZZZaaaabbbbccccddddeeeeffffgggghhhhiiiijjjjkkkk\nllllmmmmnnnnooooppppqqqqrrrrssssttttuuuuvvvvwwwwxxxxyyyyzzzz1234\n-----END RSA PRIVATE KEY-----' | LC_ALL=C sed -E -f "$JT_PLUGIN_ROOT/lib/redact.sed")"
jt_hasnt RED-1c "$pem_multi" 'MIIEpAIBAAKCAQEA' 'multi-line: key body is gone'
jt_hasnt RED-1c "$pem_multi" 'llllmmmmnnnn'     'multi-line: second body line is gone'
jt_hasnt RED-1c "$pem_multi" 'END RSA PRIVATE'  'multi-line: END line is gone'
jt_has   RED-1c "$pem_multi" '[REDACTED:privkey]' 'multi-line: marker present'
# The shape capture ACTUALLY sees: _jm_cap_enc flattens LF to a literal \n
# BEFORE redaction, so the whole key arrives as one line.
pem_flat="$(red 'user pasted: -----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAZZZaaaabbbbcccc\n-----END RSA PRIVATE KEY----- and asked why it is rejected')"
jt_hasnt RED-1c "$pem_flat" 'MIIEpAIBAAKCAQEA' 'flattened: key body is gone'
jt_has   RED-1c "$pem_flat" '[REDACTED:privkey]' 'flattened: marker present'
jt_has   RED-1c "$pem_flat" 'asked why it is rejected' 'flattened: surrounding prose survives'

# ── RED-2 ─ the host and port survive, the password does not ────────────────
o="$(red 'postgres://postgres:hunter2@localhost:5431/db')"
jt_hasnt RED-2 "$o" 'hunter2'          'password gone'
jt_has   RED-2 "$o" 'localhost:5431/db' 'host:port/db intact'

# ── RED-3 ─ a credential-free URL is byte-identical ─────────────────────────
jt_eq RED-3 'postgres://localhost:5431/db' "$(red 'postgres://localhost:5431/db')" 'untouched'

# ── RED-4 ─ high-value negatives ────────────────────────────────────────────
for n in '6f1d2c3b4a59687766554433221100ffeeddccbb' '721759' 'useYjsCanvasSync' \
         'sqlite3_step' 'host.example.com:5432' 'cache_key=user-42-profile-v3' \
         'partition_key=tenant-91' 'https://github.com/Acme/agent-plugins.git'; do
  jt_eq RED-4 "$n" "$(red "$n")" 'byte-identical'
done

# ── RED-5 ─ the name stays searchable, the value goes ───────────────────────
o="$(red 'SIGNING_KEY=abunchofsecretvalue')"
jt_eq     RED-5 'SIGNING_KEY=[REDACTED:kv]' "$o" 'value redacted'
jt_hasnt  RED-5 "$o" 'abunchofsecretvalue' 'secret gone'
# ...and `signing_key` is still one FTS token (tokenchars '_' keeps it whole).
jt_capture "$JT_FIXTURES/transcript-small.jsonl" 'red50000-0000-0000-0000-000000000005' "$JT_TMP" other >/dev/null 2>&1
jt_sql "UPDATE entries SET outcome='$o' WHERE 1;" >/dev/null
jt_eq RED-5 1 "$(jt_sql "SELECT count(*) FROM entries_fts WHERE entries_fts MATCH 'signing_key';")" 'signing_key searchable'

# ── RED-6 ─ prose that merely contains credential words ─────────────────────
jt_eq RED-6 'a bearer bond and a token of esteem' \
  "$(red 'a bearer bond and a token of esteem')" 'byte-identical'
jt_eq RED-6 'SIGNING_KEY is documented in the runbook' \
  "$(red 'SIGNING_KEY is documented in the runbook')" 'byte-identical'

# ── RED-7 ─ a secret spanning the truncation boundary ───────────────────────
# The secret is planted so the CHECK-limit cut would land inside it. Redaction
# runs first (§7.4 step 7), so no fragment can survive into the row.
jt_store
sec='ghp_ZZZZ111122223333444455556666777788889999'
boundary="$JT_TMP/boundary.jsonl"
# 3980 pad bytes put the 42-byte token across byte 4000 of the RAW outcome: a
# truncate-then-redact implementation would store `ghp_ZZZZ1111...` as a live
# fragment. 300 trailing bytes guarantee the cut still fires after redaction.
pad="$(head -c 3980 /dev/zero | tr '\0' 'w')"
tail300="$(head -c 300 /dev/zero | tr '\0' 'q')"
{
  printf '%s\n' '{"type":"user","promptSource":"typed","timestamp":"2026-08-17T15:00:00.000Z","cwd":"/repo","message":{"role":"user","content":"Rotate the token."}}'
  printf '%s\n' '{"type":"assistant","timestamp":"2026-08-17T15:00:01.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/.env"}}]}}'
  printf '{"type":"assistant","timestamp":"2026-08-17T15:00:02.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"text","text":"%s %s %s"}]}}\n' "$pad" "$sec" "$tail300"
  printf '%s\n' '{"type":"ai-title","aiTitle":"Rotate the deploy token"}'
} >"$boundary"
jt_capture "$boundary" 'red70000-0000-0000-0000-000000000007' "$JT_TMP" other >/dev/null 2>&1
row="$(jt_sql "SELECT title||' '||request||' '||outcome||' '||files_touched FROM entries;")"
jt_hasnt RED-7 "$row" 'ZZZZ1111' 'no fragment of the secret'
jt_hasnt RED-7 "$row" 'ghp_ZZZZ' 'no prefix of the secret'
jt_has   RED-7 "$row" '[REDACT'  'a redaction marker sits at the boundary'
jt_eq    RED-7 4000 "$(jt_sql 'SELECT length(outcome) FROM entries;')" 'truncation really did fire'

# ── RED-8 ─ sed forced to fail: fail CLOSED ─────────────────────────────────
# Two independent failure modes, both of which must end with zero rows written.
#
# (a) The ruleset itself is unreadable. Exercised against a COPY of the plugin so
#     the repo's own lib/redact.sed is never chmod-ed by a test.
jt_store
if [ "$(id -u)" -eq 0 ]; then
  jt_skip RED-8 'running as root: chmod 000 is not enforced'
else
  clone="$JT_TMP/plugin-clone"
  mkdir -p "$clone"
  cp -R "$JT_PLUGIN_ROOT/lib" "$JT_PLUGIN_ROOT/db" "$clone/"
  chmod 0000 "$clone/lib/redact.sed"
  bash "$clone/lib/capture.sh" --transcript "$JT_FIXTURES/transcript-small.jsonl" \
    --session 'red80000-0000-0000-0000-00000000000a' --cwd "$JT_TMP" --reason other >/dev/null 2>&1
  rc=$?
  jt_ne RED-8 0 "$rc" 'unreadable ruleset -> capture reports failure'
  jt_eq RED-8 0 "$(jt_sql 'SELECT count(*) FROM entries;')" '0 rows written'
  jt_has RED-8 "$(cat "$JT_LOG" 2>/dev/null)" 'reason=redact_failed' 'error logged'
  chmod 0644 "$clone/lib/redact.sed"
fi

# (b) The function contract itself: rc 1 fail-closed, rc 2 misuse.
printf 'hello\n' >"$JT_TMP/in.txt"
rc=0; jm_capture_redact "$JT_TMP/nonexistent-input" "$JT_TMP/out.txt" >/dev/null 2>&1 || rc=$?
jt_eq RED-8 1 "$rc" 'unreadable input -> rc 1 (fail closed)'
rc=0; jm_capture_redact '' '' >/dev/null 2>&1 || rc=$?
jt_eq RED-8 2 "$rc" 'misuse -> rc 2'
rc=0; jm_capture_redact "$JT_TMP/in.txt" "$JT_TMP/out.txt" >/dev/null 2>&1 || rc=$?
jt_eq RED-8 0 "$rc" 'readable input -> rc 0'

# (c) `sed` missing from PATH entirely. The worker dies where it stands (set -e)
#     and the GUARD is what converts that into exit 0 plus one log line.
jt_store
noext="$JT_TMP/noexec"
mkdir -p "$noext"
printf '#!/bin/sh\nexit 3\n' >"$noext/sed"
chmod +x "$noext/sed"
pl8="$JT_TMP/p8.json"
jt_payload "$pl8" 'red80000-0000-0000-0000-00000000000b' "$JT_FIXTURES/transcript-small.jsonl" "$JT_TMP" other
out="$(PATH="$noext:$PATH" bash "$JT_SESSION_END" <"$pl8" 2>"$JT_TMP/e8")"
jt_eq RED-8 0 $? 'guard exit 0 with sed broken'
jt_eq RED-8 '' "$out$(cat "$JT_TMP/e8")" 'guard silent with sed broken'
rows="$(jt_sql 'SELECT count(*) FROM entries;')"
jt_eq RED-8 0 "${rows:-0}" '0 rows written (the worker died before bootstrap)'
jt_has RED-8 "$(cat "$JT_LOG" 2>/dev/null)" 'session_end' 'guard logged the failure'

# ── RED-9 ─ the stored count matches, and the secret is nowhere in the DB ───
jt_store
multi="$JT_TMP/multi.jsonl"
{
  printf '%s\n' '{"type":"user","promptSource":"typed","timestamp":"2026-08-17T16:00:00.000Z","cwd":"/repo","message":{"role":"user","content":"Here is my key ghp_AAAA111122223333444455556666777788889999 please rotate."}}'
  printf '%s\n' '{"type":"assistant","timestamp":"2026-08-17T16:00:01.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit","input":{"file_path":"/repo/.env"}}]}}'
  printf '%s\n' '{"type":"assistant","timestamp":"2026-08-17T16:00:02.000Z","cwd":"/repo","message":{"role":"assistant","content":[{"type":"text","text":"Rotated. The old DSN was postgres://svc:s3cr3tpw@db.internal:5432/app and AWS key AKIAIOSFODNN7EXAMPLE is revoked. This closing note is padded out past the two hundred character trivial-session floor so the row is definitely written rather than skipped by the deterministic skip rule."}]}}'
  printf '%s\n' '{"type":"ai-title","aiTitle":"Rotate leaked credentials"}'
} >"$multi"
jt_capture "$multi" 'red90000-0000-0000-0000-000000000009' "$JT_TMP" other >/dev/null 2>&1
stored="$(jt_sql 'SELECT redactions FROM entries;')"
jt_eq RED-9 3 "$stored" 'three substitutions recorded'
dump="$(jt_sql "SELECT group_concat(title||request||outcome||files_touched||commands,' ') FROM entries;")"
jt_hasnt RED-9 "$dump" 'ghp_AAAA'      'github token absent'
jt_hasnt RED-9 "$dump" 's3cr3tpw'      'dsn password absent'
jt_hasnt RED-9 "$dump" 'AKIAIOSFODNN7' 'aws key absent'
jt_has   RED-9 "$dump" 'db.internal:5432/app' 'debugging value preserved'
jt_eq RED-9 0 "$(jt_sql "SELECT count(*) FROM entries_fts WHERE entries_fts MATCH 'AKIAIOSFODNN7EXAMPLE';")" 'not in the fts index either'

# ── RED-10 ─ full-corpus regression against the legacy archive ──────────────
# The 10k-line sample is NOT committed: it is client prose from 19 orgs, and
# shipping it in a shared repo is the exact exposure this plugin exists to
# prevent. It is instead drawn deterministically from the read-only archive
# (ORDER BY id LIMIT 10000, immutable=1) and compared against a recorded golden.
ARCHIVE="${JHH3_MEM_LEGACY:-$HOME/mem-archive/claude-mem.db}"
if [ ! -r "$ARCHIVE" ]; then
  jt_skip RED-10 "archive absent: $ARCHIVE"
else
  sample="$JT_TMP/corpus-sample.txt"
  sqlite3 -readonly -batch -noheader "file:$ARCHIVE?mode=ro&immutable=1" \
    "SELECT replace(replace(coalesce(title,'')||' | '||coalesce(subtitle,'')||' | '||coalesce(narrative,'')||' | '||coalesce(facts,''),char(10),' '),char(13),' ') FROM observations ORDER BY id LIMIT 10000;" \
    >"$sample" 2>/dev/null
  jt_eq RED-10 10000 "$(wc -l <"$sample" | tr -d ' ')" 'sample line count'
  jt_eq RED-10 '8a1b4a5b93c8485b' "$(sha256sum "$sample" | cut -c1-16)" 'sample is deterministic'
  hist="$(LC_ALL=C sed -E -f "$JT_PLUGIN_ROOT/lib/redact.sed" "$sample" \
    | grep -o '\[REDACTED:[a-z]*\]' | sort | uniq -c | LC_ALL=C awk '{printf "%s:%s ", $2, $1}')"
  # GOLDEN UPDATED when the ruleset gained Authorization-header coverage: the
  # same 10k-line sample now yields two additional [REDACTED:authz] hits, which
  # are two live bearer tokens that used to be persisted in cleartext. A golden
  # that only ever moves DOWN is a golden that hides new coverage.
  jt_eq RED-10 '[REDACTED:authz]:2 [REDACTED:kv]:45 [REDACTED:userinfo]:35 ' "$hist" 'golden histogram'
  # No credential shape survives the pass.
  post="$JT_TMP/corpus-post.txt"
  LC_ALL=C sed -E -f "$JT_PLUGIN_ROOT/lib/redact.sed" "$sample" >"$post"
  # NB the userinfo probe excludes '[' so it cannot match '[REDACTED:userinfo]'
  # — the marker itself is a colon-bearing value between ':' and '@'.
  for pat in 'ghp_[A-Za-z0-9]\{30,\}' 'AKIA[0-9A-Z]\{16\}' 'ASIA[0-9A-Z]\{16\}' \
             'github_pat_[A-Za-z0-9_]\{20,\}' 'sk-proj-[A-Za-z0-9_-]\{20,\}' \
             'xox[baprse]-[A-Za-z0-9-]\{20,\}' \
             'sk-ant-[A-Za-z0-9_-]\{18,\}' 'BEGIN [A-Z ]*PRIVATE KEY' '://[^:/@ ]*:[^@ []\{3,\}@'; do
    n="$(grep -c "$pat" "$post" 2>/dev/null)" || true
    jt_eq RED-10 0 "${n:-0}" "residual /$pat/"
  done
  # The archive itself was not touched.
  jt_eq RED-10 '' "$(find "${ARCHIVE%/*}" -name "$( { b="${ARCHIVE##*/}"; printf '%s' "${b}-journal"; } )" 2>/dev/null)" 'no journal sidecar created'
fi

jt_summary
