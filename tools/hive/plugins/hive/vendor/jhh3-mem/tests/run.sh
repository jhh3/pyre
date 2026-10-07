#!/usr/bin/env bash
# run.sh — the jhh3-mem test runner. Plain bash; `bats` is absent on this host.
#
#   bash tests/run.sh                 run every suite that exists
#   bash tests/run.sh t-invariants    run named suites (with or without the t- prefix)
#   bash tests/run.sh --strict        unimplemented suites become failures (release gate)
#   bash tests/run.sh --list          inventory: which suites exist, who owns the rest
#   bash tests/run.sh --quiet         suite summaries only
#
#   JHH3_MEM_TEST_LIVE=1              include the real `claude --plugin-dir` session
#
# Exit status: 0 when no suite failed; 1 otherwise. PEND (a component another
# lane has not written yet) is reported loudly but is not a failure unless
# --strict. Nothing here ever touches the real store: every suite exports
# JHH3_MEM_HOME into a mktemp directory before doing anything.
set -uo pipefail

_here="${BASH_SOURCE[0]}"
case "$_here" in */*) TESTS_DIR="${_here%/*}" ;; *) TESTS_DIR="." ;; esac
TESTS_DIR="$(cd -- "$TESTS_DIR" && pwd -P)"
PLUGIN_ROOT="$(cd -- "$TESTS_DIR/.." && pwd -P)"

# ── the crontab firewall ────────────────────────────────────────────────────
# JHH3_MEM_HOME isolates the store, but a crontab is per-USER, not per-HOME. Any
# suite that exercises `uninstall --purge` or `schedule --remove` reaches
# jm_schedule_remove, which rewrites the developer's REAL crontab -- so running
# the tests silently deleted the nightly job on this machine, while both `setup`
# and the suite reported success. Found in the wild, not in review.
#
# Stubbing per suite is the fix that failed: t-setup did it correctly and
# t-uninstall and t-e2e did not, and nothing enforced the difference. So the stub
# lives here, ahead of every suite, and a suite that wants to assert on cron
# content still prepends its own (later PATH entry wins).
JT_CRON_STUB="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-cronstub.XXXXXX")"
JT_CRON_FILE="$JT_CRON_STUB/crontab.txt"; : > "$JT_CRON_FILE"
cat > "$JT_CRON_STUB/crontab" <<STUB
#!/usr/bin/env bash
case "\${1:-}" in
  -l) cat "$JT_CRON_FILE" ;;
  -r) : > "$JT_CRON_FILE" ;;
  -)  cat > "$JT_CRON_FILE" ;;
  *)  [ -n "\${1:-}" ] && [ -f "\${1:-}" ] && cat "\$1" > "$JT_CRON_FILE" ;;
esac
exit 0
STUB
chmod +x "$JT_CRON_STUB/crontab"
PATH="$JT_CRON_STUB:$PATH"; export PATH

# ── the launcher firewall ───────────────────────────────────────────────────
# The same defect as the crontab, found the same way -- by the launcher going
# missing on a machine that had one. JM_SHIM defaults to $HOME/.local/bin/jhh3-mem,
# `uninstall --purge` calls jm_shim_remove, and t-uninstall and t-e2e (the two
# suites that run purge) do not isolate HOME. So a full test run deleted the user's
# real launcher, silently: `jhh3-mem` kept working because a plugin cache bin
# directory is also on PATH, and quietly resolved to an OLDER version -- which is
# worse than a clean failure, because cron then runs a binary without the verbs the
# cron line names.
#
# Overriding JM_SHIM rather than HOME is deliberate: the live-session suite needs
# the real HOME to find the host's credentials, so blanking HOME would trade this
# bug for a broken E2E.
JT_SHIM_DIR="$(mktemp -d "${TMPDIR:-/tmp}/jhh3-shim.XXXXXX")"
JM_SHIM="$JT_SHIM_DIR/jhh3-mem"; export JM_SHIM
JT_REAL_SHIM="$HOME/.local/bin/jhh3-mem"
JT_REAL_SHIM_PRESENT=0; [ -e "$JT_REAL_SHIM" ] && JT_REAL_SHIM_PRESENT=1

# Belt and braces: snapshot the real crontab and verify on exit that no suite
# reached past the stub. An assertion is cheaper than rediscovering this.
JT_REAL_CRONTAB="$(command -p crontab -l 2>/dev/null | md5sum 2>/dev/null | cut -d' ' -f1)"
_jt_cron_check() {
  local now
  now="$(command -p crontab -l 2>/dev/null | md5sum 2>/dev/null | cut -d' ' -f1)"
  local rc=0
  if [ "$now" != "$JT_REAL_CRONTAB" ]; then
    printf '\n!! THE TEST RUN MODIFIED YOUR REAL CRONTAB -- restore it with `jhh3-mem schedule`\n' >&2
    rc=1
  fi
  if [ "$JT_REAL_SHIM_PRESENT" = '1' ] && [ ! -e "$JT_REAL_SHIM" ]; then
    printf '\n!! THE TEST RUN DELETED YOUR LAUNCHER (%s) -- restore it with `jhh3-mem setup`\n' \
      "$JT_REAL_SHIM" >&2
    rc=1
  fi
  return "$rc"
}
trap '_jt_cron_check || true; rm -rf -- "$JT_CRON_STUB" "$JT_SHIM_DIR" 2>/dev/null' EXIT

# The full suite inventory, in run order: <suite>|<owning lane>|<what it covers>
# A suite listed here but absent from disk is reported as NOT-IMPLEMENTED rather
# than silently ignored — an empty test directory must never look like success.
SUITES=(
  "t-invariants|E-manifests|SPEC 14.1 build invariants (INV-1..8)"
  "t-packaging|E-manifests|manifests, marketplace entry, repo conventions, UNI-11"
  "t-redact|E-capture|SPEC 14.3 RED-1..10"
  "t-capture|E-capture|SPEC 14.2 CAP-1..15"
  "t-failopen|E-capture|SPEC 14.4 FO-1..15"
  "t-concurrency|E-capture|SPEC 14.7 CON-1..8"
  "t-inject-bounds|E-retrieval|SPEC 14.5 INJ-1..17"
  "t-recall|E-retrieval|SPEC 14.6 REC-1..15"
  "t-hosts|E-capture|cross-host: adapter dispatch, Codex rollouts, host tagging (HOST-1..8)"
  "t-enrich|E-lifecycle|out-of-band enrichment via a stub model (ENR-1..8)"
  "t-dream|E-lifecycle|between-session consolidation, incl. idempotence (DRM-1..7)"
  "t-embed|E-retrieval|optional semantic lane: absent, broken and hostile paths (EMB-1..8)"
  "t-setup|E-lifecycle|one-command install/update and the nightly job (SET-1..4, SCH-1..5)"
  "t-migrate|E-lifecycle|SPEC 14.8 MIG-1..14"
  "t-uninstall|E-lifecycle|SPEC 14.9 UNI-1..12"
  "t-regressions|E-core|one case per defect adversarial review found (REG-*)"
  "t-lint|E-manifests|bash -n everywhere; shellcheck -S style (SKIPs if absent)"
  "t-e2e|E-manifests|full lifecycle: bootstrap, capture, inject, recall, remove"
)

STRICT=0
QUIET=0
WANTED=()
for arg in "$@"; do
  case "$arg" in
    --strict) STRICT=1 ;;
    --quiet | -q) QUIET=1 ;;
    --list | -l)
      printf '%-18s %-14s %-8s %s\n' SUITE OWNER STATE COVERS
      for row in "${SUITES[@]}"; do
        IFS='|' read -r s o c <<<"$row"
        st="missing"
        [ -f "$TESTS_DIR/$s.sh" ] && st="present"
        printf '%-18s %-14s %-8s %s\n' "$s" "$o" "$st" "$c"
      done
      exit 0
      ;;
    --help | -h)
      sed -n '2,20p' "$_here"
      exit 0
      ;;
    -*)
      printf 'run.sh: unknown option: %s\n' "$arg" >&2
      exit 2
      ;;
    *) WANTED+=("${arg%.sh}") ;;
  esac
done

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
  C_OK=$'\033[32m'; C_BAD=$'\033[31m'; C_PEND=$'\033[33m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_OK=""; C_BAD=""; C_PEND=""; C_DIM=""; C_OFF=""
fi

want() {
  [ "${#WANTED[@]}" -eq 0 ] && return 0
  local w
  for w in "${WANTED[@]}"; do
    [ "$w" = "$1" ] || [ "t-$w" = "$1" ] && return 0
  done
  return 1
}

TOT_PASS=0
TOT_FAIL=0
TOT_PEND=0
TOT_SKIP=0
RAN=0
FAILED_SUITES=()
MISSING_SUITES=()

printf '%sjhh3-mem test run — %s%s\n' "$C_DIM" "$(date '+%Y-%m-%d %H:%M:%S')" "$C_OFF"
printf '%splugin: %s%s\n\n' "$C_DIM" "$PLUGIN_ROOT" "$C_OFF"

started=$(date +%s)
for row in "${SUITES[@]}"; do
  IFS='|' read -r suite owner covers <<<"$row"
  want "$suite" || continue
  path="$TESTS_DIR/$suite.sh"
  if [ ! -f "$path" ]; then
    MISSING_SUITES+=("$suite ($owner) — $covers")
    printf '%sNOT-IMPLEMENTED%s %-18s owner: %-14s %s\n' \
      "$C_PEND" "$C_OFF" "$suite" "$owner" "$covers"
    continue
  fi
  RAN=$((RAN + 1))
  out="$(bash "$path" 2>&1)"
  rc=$?
  [ "$QUIET" = 1 ] || printf '%s\n' "$out"
  # Preferred: the harness protocol (tests/lib/harness.sh).
  # Tolerated: a suite written against its own conventions — this runner must
  # never report a passing suite as "crashed" just because it counts its own
  # results differently. Order: `## SUITE`, then an "N passed, N failed"
  # trailer, then per-line ok/fail tallies, then the exit status alone.
  p=""; f=""; d=0; k=0; fmt="harness"
  summary="$(printf '%s\n' "$out" | grep -E '^## SUITE ' | tail -1)"
  if [ -n "$summary" ]; then
    p=$(printf '%s\n' "$summary" | sed -n 's/.*pass=\([0-9]*\).*/\1/p')
    f=$(printf '%s\n' "$summary" | sed -n 's/.*fail=\([0-9]*\).*/\1/p')
    d=$(printf '%s\n' "$summary" | sed -n 's/.*pend=\([0-9]*\).*/\1/p')
    k=$(printf '%s\n' "$summary" | sed -n 's/.*skip=\([0-9]*\).*/\1/p')
  else
    trailer="$(printf '%s\n' "$out" | grep -iE '[0-9]+ passed,[[:space:]]*[0-9]+ failed' | tail -1)"
    if [ -n "$trailer" ]; then
      fmt="counted"
      p=$(printf '%s\n' "$trailer" | sed -n 's/.*[^0-9]\([0-9]\{1,\}\) passed.*/\1/p')
      f=$(printf '%s\n' "$trailer" | sed -n 's/.*[^0-9]\([0-9]\{1,\}\) failed.*/\1/p')
      k=$(printf '%s\n' "$trailer" | sed -n 's/.*[^0-9]\([0-9]\{1,\}\) skipped.*/\1/p')
    else
      p="$(printf '%s\n' "$out" | grep -cE '^[[:space:]]*(ok|PASS)[[:space:]]')"
      f="$(printf '%s\n' "$out" | grep -cE '^[[:space:]]*(fail|FAIL|not ok)[[:space:]]')"
      if [ "$p" -eq 0 ] && [ "$f" -eq 0 ]; then
        fmt="exit-status-only"
        if [ "$rc" -eq 0 ]; then p=1; f=0; else p=0; f=1; fi
      else
        fmt="tallied"
      fi
    fi
    printf '%s%-18s reported in its own format (%s); counted pass=%s fail=%s%s\n' \
      "$C_DIM" "$suite" "$fmt" "$p" "$f" "$C_OFF"
  fi
  p="${p:-0}"; f="${f:-0}"; d="${d:-0}"; k="${k:-0}"
  # A suite that exits non-zero while reporting no failures is still a failure.
  if [ "$rc" -ne 0 ] && [ "$f" -eq 0 ]; then
    printf '%s%-18s exited %s with 0 reported failures — counting it as failed%s\n' \
      "$C_BAD" "$suite" "$rc" "$C_OFF"
    f=1
  fi
  TOT_PASS=$((TOT_PASS + p))
  TOT_FAIL=$((TOT_FAIL + f))
  TOT_PEND=$((TOT_PEND + d))
  TOT_SKIP=$((TOT_SKIP + k))
  [ "$f" -eq 0 ] || FAILED_SUITES+=("$suite")
  if [ "$QUIET" = 1 ]; then
    printf '%-18s pass=%-4s fail=%-4s pend=%-4s skip=%s\n' "$suite" "$p" "$f" "$d" "$k"
  fi
  printf '\n'
done
elapsed=$(($(date +%s) - started))

printf '%s================================================================%s\n' "$C_DIM" "$C_OFF"
printf 'suites run %d · %spass %d%s · %sfail %d%s · %spend %d%s · skip %d · %ds\n' \
  "$RAN" "$C_OK" "$TOT_PASS" "$C_OFF" "$C_BAD" "$TOT_FAIL" "$C_OFF" \
  "$C_PEND" "$TOT_PEND" "$C_OFF" "$TOT_SKIP" "$elapsed"

if [ "${#FAILED_SUITES[@]}" -gt 0 ]; then
  printf '%sfailing suites:%s %s\n' "$C_BAD" "$C_OFF" "${FAILED_SUITES[*]}"
fi
if [ "${#MISSING_SUITES[@]}" -gt 0 ]; then
  printf '%snot implemented yet (%d):%s\n' "$C_PEND" "${#MISSING_SUITES[@]}" "$C_OFF"
  for m in "${MISSING_SUITES[@]}"; do printf '  %s\n' "$m"; done
fi
if [ "$TOT_PEND" -gt 0 ]; then
  printf '%s%d pending assertion(s):%s a component they need has not been written yet.\n' \
    "$C_PEND" "$TOT_PEND" "$C_OFF"
fi
if [ "${JHH3_MEM_TEST_LIVE:-0}" != "1" ]; then
  printf '%slive session not exercised: JHH3_MEM_TEST_LIVE=1 bash tests/run.sh t-e2e%s\n' \
    "$C_DIM" "$C_OFF"
fi

rc=0
[ "$TOT_FAIL" -eq 0 ] || rc=1
if [ "$STRICT" = 1 ] && { [ "$TOT_PEND" -gt 0 ] || [ "${#MISSING_SUITES[@]}" -gt 0 ]; }; then
  printf '%s--strict: pending work counts as failure%s\n' "$C_BAD" "$C_OFF"
  rc=1
fi
if [ "$rc" -eq 0 ]; then
  printf '%sOK%s\n' "$C_OK" "$C_OFF"
else
  printf '%sNOT OK%s\n' "$C_BAD" "$C_OFF"
fi
exit "$rc"
