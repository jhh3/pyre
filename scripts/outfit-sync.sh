#!/usr/bin/env bash
# outfit-sync.sh: copy this machine's Outfit connections (credentials
# included) to other machines, so every machine holds its own copy.
#
#   scripts/outfit-sync.sh [options] TARGET...
#
# TARGET is an ssh host, or HOST=NODE when the Pyre node name differs from
# the ssh host (e.g. win-home=win-home-pc). For each target:
#
#   1. outfit backup create (once, here) into a private temp dir, encrypted
#      with a random one-time passphrase kept in a 0600 file;
#   2. scp the backup and passphrase file into a private temp dir there;
#   3. outfit backup restore --replace there (the passphrase and backup are
#      deleted on the target as soon as it returns), then outfit doctor;
#   4. check each connection there, and print its result: FAILED for one
#      the restore did not report as ready (it is never checked or called
#      verified); else `pyre connections verify` (pyred runs the connector
#      test in the user's login session: what Pyre sessions there get) when
#      the target is a connected Pyre node, and `outfit connector test` in an
#      ssh login (what scripts and ssh users get). On Windows the two can
#      differ: Credential Manager is usually out of reach of an ssh key login;
#   5. delete the target's temp dir right away; the local one at the end
#      (also on failure or Ctrl-C). A cleanup failure is reported with the
#      path to remove by hand.
#
# Left out, and listed in the final summary:
#   - connections whose secret is missing here (outfit doctor: credential
#     missing); the script then exits 1;
#   - Outfit-managed OAuth connections, unless --include-oauth: copies of one
#     refresh token on several machines invalidate each other (and the
#     source's copy) when the provider rotates it. Connect those on each
#     machine instead.
#
# The passphrase never appears in argv or the environment of a remote
# command; it travels as a file and is passed with --passphrase-file.
#
# Where the restore runs (--via):
#   pyre  as a Pyre shell session on the target node. pyred runs in the
#         user's login session there, so the macOS login keychain, the Linux
#         Secret Service and the Windows Credential Manager accept writes.
#   ssh   directly over ssh. Works where the vault accepts non-interactive
#         writes: Outfit's file vault (`outfit vault use file`), or a
#         keychain that is already unlocked. A macOS login keychain usually
#         refuses ("User interaction is not allowed"), as does Credential
#         Manager under ssh key logins; the restore then stops before
#         touching any account (it probes the vault first).
#   auto  (default) pyre when the pyre CLI is here and lists the node as
#         connected, else ssh.
# Windows targets need PowerShell as the shell ssh and Pyre run.
#
# Options:
#   --only a,b         copy only these connections
#   --include-oauth    also copy Outfit-managed OAuth connections
#   --no-replace       fail instead of replacing a same-named connection there
#   --via auto|pyre|ssh
#   --outfit PATH      outfit on the targets (default: outfit on PATH,
#                      else ~/.local/bin/outfit); a leading ~/ or $HOME/ is
#                      the target user's home
#
# Remote commands run in a non-login shell; the script puts ~/.local/bin,
# ~/bin, ~/go/bin, ~/.cargo/bin, /opt/homebrew/bin and /usr/local/bin (on
# Windows %USERPROFILE%\.local\bin) in front of its PATH there, so outfit and
# the provider tools it runs (gh, agent-slack, ...) are found.
#   --timeout SECONDS  how long to wait for each restore (default 900)
set -euo pipefail

only="" replace="--replace" via="auto" remote_outfit="" timeout=900 include_oauth=""
targets=()
while [ $# -gt 0 ]; do
	case "$1" in
	--only) only="$2"; shift 2 ;;
	--include-oauth) include_oauth=1; shift ;;
	--no-replace) replace=""; shift ;;
	--via) via="$2"; shift 2 ;;
	--outfit) remote_outfit="$2"; shift 2 ;;
	--timeout) timeout="$2"; shift 2 ;;
	-h|--help) sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
	-*) echo "outfit-sync: unknown option $1" >&2; exit 2 ;;
	*) targets+=("$1"); shift ;;
	esac
done
[ ${#targets[@]} -gt 0 ] || { echo "usage: $0 [--only a,b] [--include-oauth] [--no-replace] [--via auto|pyre|ssh] TARGET..." >&2; exit 2; }
case "$via" in auto|pyre|ssh) ;; *) echo "outfit-sync: --via must be auto, pyre or ssh" >&2; exit 2 ;; esac
case "$timeout" in ''|*[!0-9]*) echo "outfit-sync: --timeout takes seconds" >&2; exit 2 ;; esac
for target in "${targets[@]}"; do
	for part in "${target%%=*}" "${target#*=}"; do
		case "$part" in
		-*|'') echo "outfit-sync: refusing target '$target'" >&2; exit 2 ;;
		esac
		if ! printf %s "$part" | grep -Eq '^[A-Za-z0-9._@-]+$'; then
			echo "outfit-sync: refusing target '$target' (letters, digits, . _ @ - only)" >&2; exit 2
		fi
	done
done
command -v outfit >/dev/null || { echo "outfit-sync: outfit is not installed here" >&2; exit 1; }
command -v python3 >/dev/null || { echo "outfit-sync: python3 is required here" >&2; exit 1; }

local_dir=$(mktemp -d "${TMPDIR:-/tmp}/outfit-sync.XXXXXX")
chmod 700 "$local_dir"
pending=() # cleanup commands for the target in progress
run_cleanups() {
	local c
	for c in ${pending[@]+"${pending[@]}"}; do
		if ! eval "$c" >/dev/null 2>&1; then
			echo "outfit-sync: CLEANUP FAILED, remove by hand: $c" >&2
		fi
	done
	pending=()
}
cleanup() {
	run_cleanups
	rm -rf "$local_dir"
}
trap cleanup EXIT
trap 'echo "outfit-sync: interrupted" >&2; exit 130' INT TERM

# sq quotes one word for a POSIX shell; psq for PowerShell; pse encodes a
# PowerShell script for -EncodedCommand (safe through cmd or PowerShell as
# the remote login shell).
sq() { printf "'%s'" "$(printf %s "$1" | sed "s/'/'\\\\''/g")"; }
psq() { printf "'%s'" "$(printf %s "$1" | sed "s/'/''/g")"; }
pse() { printf %s "\$ProgressPreference = 'SilentlyContinue'; $1" | iconv -f UTF-8 -t UTF-16LE | base64 | tr -d '\n'; }

# Remote commands run in a non-login shell (ssh host CMD, sh -c): put the
# user's tool directories first so outfit and the CLIs it runs are found.
# shellcheck disable=SC2016
UNIX_PATH='PATH="$HOME/.local/bin:$HOME/bin:$HOME/go/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; export PATH;'
# shellcheck disable=SC2016
WIN_PATH='$env:Path = (Join-Path $env:USERPROFILE ".local\bin") + ";" + $env:Path;'

# unix_outfit / win_outfit: the remote outfit as a shell word / PowerShell
# expression; a leading ~/ or $HOME/ (as typed, or kept literal by quoting)
# is the target user's home, not this machine's.
home_rest() { # path -> the part after a leading home reference, or fails
	case "$1" in
	\~/*|\~\\*) printf %s "${1#??}" ;;
	'$HOME/'*|'$HOME\'*) printf %s "${1#??????}" ;;
	'${HOME}/'*|'${HOME}\'*) printf %s "${1#????????}" ;;
	'$env:USERPROFILE/'*|'$env:USERPROFILE\'*) printf %s "${1#?????????????????}" ;;
	"$HOME/"*) printf %s "${1#"$HOME"/}" ;; # expanded by this shell from an unquoted $HOME
	*) return 1 ;;
	esac
}
unix_outfit() {
	local rest
	if [ -z "$remote_outfit" ]; then
		# shellcheck disable=SC2016
		printf %s '"$(command -v outfit || echo "$HOME/.local/bin/outfit")"'
	elif rest=$(home_rest "$remote_outfit"); then
		printf '"$HOME"/%s' "$(sq "$rest")"
	else
		sq "$remote_outfit"
	fi
}
win_outfit() {
	local rest
	if [ -z "$remote_outfit" ]; then
		printf %s "'outfit'"
	elif rest=$(home_rest "$remote_outfit"); then
		printf '(Join-Path $env:USERPROFILE %s)' "$(psq "$(printf %s "$rest" | tr / '\\')")"
	else
		psq "$remote_outfit"
	fi
}

# check_connection NAME: the connection's state on the current target, in
# Pyre sessions (pyred runs the connector test; when the target is a
# connected Pyre node) and in an ssh login. FAILED when the context that
# counts (Pyre when there is a node, else ssh) does not verify it.
check_connection() {
	local n=$1 v state detail out
	if [ -n "$node_id" ]; then
		# exits 1 unless verified; its JSON still carries the node's result
		out=$(pyre --json connections verify "$n" --node "$node_id" 2>/dev/null || true)
		v=$(printf '%s' "$out" | python3 -c '
import json,sys
try:
    a = json.load(sys.stdin)
    v = a[0] if isinstance(a, list) and a else {}
except Exception:
    v = {"state": "unknown", "detail": "no answer from pyre connections verify"}
print((v.get("state") or "unknown") + "\t" + " ".join((v.get("detail") or "").split())[:200])')
		state=${v%%	*} detail=${v#*	}
		if [ "$state" = verified ]; then
			echo "   verified $n on $node in Pyre sessions (pyred ran the connector test there)"
		else
			echo "   FAILED $n on $node in Pyre sessions: $state${detail:+ ($detail)}"
			failed_conns+=("$host:$n"); status=1
		fi
	fi
	if out=$(ssh_check "$n" 2>&1); then
		if [ -n "$node_id" ]; then echo "   ssh login on $host: $n works"; else echo "   verified $n on $host in an ssh login (not a connected Pyre node: Pyre sessions not checked)"; fi
	else
		out=$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-200)
		if [ -n "$node_id" ]; then
			echo "   ssh login on $host: $n does not work there (${out:-no output}); Pyre sessions are what count"
		else
			echo "   FAILED $n on $host in an ssh login: ${out:-no output}"
			failed_conns+=("$host:$n"); status=1
		fi
	fi
}

# One-time passphrase: 32 random bytes, base64, in a private file.
(umask 077; head -c 32 /dev/urandom | base64 | tr -d '\n=' >"$local_dir/passphrase")
backup="$local_dir/fleet.outfit"
echo "== creating backup on $(hostname -s 2>/dev/null || hostname)"
create_args=(--passphrase-file "$local_dir/passphrase" --allow-missing)
[ -n "$include_oauth" ] || create_args+=(--exclude-oauth)
outfit backup create "$backup" "${create_args[@]}" 2>"$local_dir/create.err" || { cat "$local_dir/create.err" >&2; exit 1; }
cat "$local_dir/create.err" >&2
missing=$(sed -n 's/^  \([^:]*\): .*/\1/p' "$local_dir/create.err" | tr '\n' ' ')
oauth_left=$(sed -n 's/^left out (OAuth, --exclude-oauth): //p' "$local_dir/create.err" | tr -d ',')
if [ -n "$only" ]; then # only the selected connections matter
	keep() { local out="" n; for n in $1; do case ",$only," in *",$n,"*) out="$out $n" ;; esac; done; printf %s "$out"; }
	missing=$(keep "$missing") oauth_left=$(keep "$oauth_left")
fi
left_out=" $missing $oauth_left "

names=()
if [ -n "$only" ]; then
	IFS=, read -r -a names <<<"$only"
else
	while IFS= read -r n; do
		[ -n "$n" ] && [ "${left_out#* "$n" }" = "$left_out" ] && names+=("$n")
	done < <(OUTFIT_AGENT=1 outfit ls --json 2>/dev/null |
		python3 -c 'import json,sys; [print(r["name"]) for r in (json.load(sys.stdin).get("data") or [])]' 2>/dev/null || true)
fi

pyre_node() { # name -> prints the node_id of a connected Pyre node whose name, ssh alias or ID it is
	command -v pyre >/dev/null || return 1
	pyre --json nodes 2>/dev/null | python3 -c '
import json,sys
want=sys.argv[1]
for n in json.load(sys.stdin).get("items") or []:
    if want in (n.get("node_id"), n.get("name"), n.get("ssh_alias")) and (n.get("connection") or {}).get("state") == "connected":
        print(n["node_id"]); sys.exit(0)
sys.exit(1)' "$1" 2>/dev/null
}
session_id() { python3 -c 'import json,sys; print(json.load(sys.stdin)["session"]["session"]["session_id"])'; }
session_ended() { # sid -> 0 when its run has ended
	pyre --json sessions show "$1" 2>/dev/null | python3 -c '
import json,sys
v=json.load(sys.stdin)
sys.exit(0 if ((v.get("run") or {}).get("state") in ("exited","failed")) else 1)' 2>/dev/null
}

status=0
failed_targets=()
failed_conns=()
for target in "${targets[@]}"; do
	host=${target%%=*} node=${target#*=}
	node_id=$(pyre_node "$node" || true)
	mode=$via
	if [ "$mode" = auto ]; then
		if [ -n "$node_id" ]; then mode=pyre; else mode=ssh; fi
	fi
	if [ "$mode" = pyre ] && [ -z "$node_id" ]; then
		echo "outfit-sync: $node is not a connected Pyre node (pyre nodes); use --via ssh" >&2
		status=1; failed_targets+=("$host"); continue
	fi
	if ! ssh -o BatchMode=yes -o ConnectTimeout=15 -- "$host" exit 0 >/dev/null 2>&1; then
		echo "outfit-sync: $host is unreachable over ssh; skipped" >&2
		status=1; failed_targets+=("$host"); continue
	fi
	echo "== $host (restore via $mode${node_id:+, node $node_id})"
	if ssh -o BatchMode=yes -- "$host" uname -s >/dev/null 2>&1; then os=unix; else os=windows; fi
	rc=1 sid=""
	if [ "$os" = unix ]; then
		rdir=$(ssh -o BatchMode=yes -- "$host" 'umask 077; mktemp -d "${TMPDIR:-/tmp}/outfit-sync.XXXXXX"' || true)
		case "$rdir" in
		/*/outfit-sync.??????) ;;
		*) echo "outfit-sync: $host: could not create a private temp dir (got '$rdir')" >&2; status=1; failed_targets+=("$host"); continue ;;
		esac
		pending+=("ssh -o BatchMode=yes -- $(sq "$host") rm -rf -- $(sq "$rdir")")
		scp -q -o BatchMode=yes "$backup" "$local_dir/passphrase" "$host:$rdir/"
		of=$(unix_outfit)
		args="$(sq "$rdir/fleet.outfit") --passphrase-file $(sq "$rdir/passphrase") $replace"
		[ -n "$only" ] && args="$args --only $(sq "$only")"
		script="$UNIX_PATH cd $(sq "$rdir") && { export OUTFIT_AGENT=0; o=$of; \"\$o\" setup >/dev/null 2>&1; \"\$o\" backup restore $args; rc=\$?; rm -f passphrase fleet.outfit; \"\$o\" doctor; echo 'connector skills written (where):'; \"\$o\" skill list; } >log 2>&1; echo \$rc >exit.tmp && mv exit.tmp exit"
		if [ "$mode" = pyre ]; then
			sid=$(pyre --json start --node "$node_id" --harness shell --cwd "$rdir" --title "outfit sync" --no-report -- -lc "$script" | session_id)
			pending+=("pyre sessions archive $(sq "$sid")")
		else
			ssh -o BatchMode=yes -- "$host" "sh -c $(sq "$script")" || true
		fi
		# (bounded where timeout(1) exists: a store waiting for an unlock prompt must not hang the run)
		ssh_check() { ssh -o BatchMode=yes -- "$host" "sh -c $(sq "$UNIX_PATH o=$of; tm=; command -v timeout >/dev/null 2>&1 && tm='timeout 90'; t=\$(OUTFIT_AGENT=0 \$tm \"\$o\" connector test $(sq "$1") 2>&1); c=\$?; printf '%s\\n' \"\$t\" | head -n 3; exit \$c")"; }
		have_exit() { ssh -o BatchMode=yes -- "$host" test -f "$rdir/exit" 2>/dev/null; }
		read_log() { ssh -o BatchMode=yes -- "$host" cat "$rdir/log" 2>/dev/null; }
		read_rc() { ssh -o BatchMode=yes -- "$host" cat "$rdir/exit" 2>/dev/null; }
	else
		rps() { ssh -o BatchMode=yes -- "$host" powershell -NoProfile -NonInteractive -EncodedCommand "$(pse "$WIN_PATH $1")"; }
		rdir=$(rps '$d = Join-Path $env:TEMP ("outfit-sync-" + [guid]::NewGuid()); New-Item -ItemType Directory $d | Out-Null; $d' | tr -d '\r' | tail -n 1 || true)
		if ! printf %s "$rdir" | grep -Eq '^[A-Za-z]:\\.*\\outfit-sync-[0-9a-f-]{36}$'; then
			echo "outfit-sync: $host: could not create a private temp dir (got '$rdir')" >&2; status=1; failed_targets+=("$host"); continue
		fi
		pending+=("ssh -o BatchMode=yes -- $(sq "$host") powershell -NoProfile -NonInteractive -EncodedCommand $(pse "Remove-Item -Recurse -Force -LiteralPath $(psq "$rdir")")")
		scp -q -o BatchMode=yes "$backup" "$local_dir/passphrase" "$host:$(printf %s "$rdir" | tr '\\' /)/"
		of=$(win_outfit)
		args="$(psq "$rdir\\fleet.outfit") --passphrase-file $(psq "$rdir\\passphrase") $replace"
		[ -n "$only" ] && args="$args --only $(psq "$only")"
		script="$WIN_PATH Set-Location -LiteralPath $(psq "$rdir"); \$env:OUTFIT_AGENT = '0'; \$o = $of; . { & \$o setup *> \$null; & \$o backup restore $args; \$rc = \$LASTEXITCODE; Remove-Item -Force passphrase, fleet.outfit; & \$o doctor; 'connector skills written (where):'; & \$o skill list } *> log; Set-Content -Path exit -Value \$rc"
		if [ "$mode" = pyre ]; then
			sid=$(pyre --json start --node "$node_id" --harness shell --cwd "$rdir" --title "outfit sync" --no-report -- -NoProfile -NonInteractive -EncodedCommand "$(pse "$script")" | session_id)
			pending+=("pyre sessions archive $(sq "$sid")")
		else
			rps "$script" || true
		fi
		ssh_check() { rps "\$env:OUTFIT_AGENT = '0'; \$o = $of; \$t = & \$o connector test $(psq "$1") 2>&1; \$c = \$LASTEXITCODE; \$t | Select-Object -First 3; exit \$c" | tr -d '\r'; }
		have_exit() { [ "$(rps "Test-Path -LiteralPath $(psq "$rdir\\exit")" 2>/dev/null | tr -d '\r')" = True ]; }
		read_log() { rps "Get-Content -LiteralPath $(psq "$rdir\\log")" 2>/dev/null | tr -d '\r'; }
		read_rc() { rps "Get-Content -LiteralPath $(psq "$rdir\\exit")" 2>/dev/null | tr -d '\r[:space:]'; }
	fi
	waited=0 finished=""
	while :; do
		if have_exit; then finished=1; break; fi
		if [ -n "$sid" ] && session_ended "$sid" && ! have_exit; then
			echo "   the Pyre shell session ended without finishing the restore (does the node run a POSIX shell / PowerShell?)" >&2
			break
		fi
		if [ "$waited" -ge "$timeout" ]; then
			echo "   restore did not finish in ${timeout}s" >&2
			[ -n "$sid" ] && pyre stop "$sid" >/dev/null 2>&1 || true
			break
		fi
		sleep 3; waited=$((waited + 3))
	done
	log=$(read_log || true)
	printf '%s\n' "$log" | sed 's/^/   /'
	if [ -n "$finished" ]; then rc=$(read_rc || echo 1); fi
	restore_ok=1
	if [ "${rc:-1}" != 0 ]; then
		echo "   restore on $host failed or was partial (exit ${rc:-?}); see above" >&2
		status=1; failed_targets+=("$host"); restore_ok=0
	fi
	# Outfit reports each restored connection as "done<TAB>NAME is ready".
	progress=0
	printf '%s\n' "$log" | grep -q "^done	" && progress=1
	for n in ${names[@]+"${names[@]}"}; do
		if ! printf '%s\n' "$log" | grep -qF "done	$n is ready" && { [ "$restore_ok" = 0 ] || [ "$progress" = 1 ]; }; then
			echo "   FAILED $n on $host: not restored (restore exit ${rc:-?}); not checked"
			failed_conns+=("$host:$n"); status=1; continue
		fi
		check_connection "$n"
	done
	run_cleanups
done

echo "== summary"
[ -n "${oauth_left// /}" ] && echo "not copied (OAuth; connect on each machine, or --include-oauth): $oauth_left"
if [ -n "${missing// /}" ]; then
	echo "NOT COPIED (secret missing here; outfit credential replace NAME): $missing"
	status=1
fi
[ ${#failed_targets[@]} -gt 0 ] && echo "FAILED targets: ${failed_targets[*]}"
[ ${#failed_conns[@]} -gt 0 ] && echo "FAILED connections: ${failed_conns[*]}"
[ "$status" = 0 ] && echo "all targets restored and verified"
exit $status
