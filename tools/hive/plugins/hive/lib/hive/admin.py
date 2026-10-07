"""hive admin — install / uninstall / doctor / status / migrate / rebuild.

Machine wiring lives here: crontab lines, launcher shims, settings.json
surgery, vault directories, migration of the jhh3-mem store into NDJSON
lanes, and rebuilding a fresh index from those lanes.

Crontab and settings.json edits are structured as PURE functions over
text/dicts (`new_crontab`, `strip_agent_deck`) so tests never touch the
live system. Every mutating verb supports --dry-run.
"""

from __future__ import annotations

import copy
import json
import os
import random
import shutil
import sqlite3
import subprocess
import sys
import time
from pathlib import Path

from . import config, db, util, vendored

PLUGIN_ROOT = Path(__file__).resolve().parent.parent.parent
SHIMS_DIR = PLUGIN_ROOT / "shims"
REDACT_SED = PLUGIN_ROOT / "vendor" / "jhh3-mem" / "lib" / "redact.sed"
MEM_SCHEMA_SQL = PLUGIN_ROOT / "vendor" / "jhh3-mem" / "db" / "schema.sql"

SETTINGS_PATH = Path.home() / ".claude" / "settings.json"
INSTALLED_PLUGINS = Path.home() / ".claude" / "plugins" / "installed_plugins.json"
LOCAL_BIN = Path.home() / ".local" / "bin"

CRON_TAG = "# hive-tick"

# Text columns used for the lane-bytes estimate and verify comparison.
_TEXT_COLS = ("title", "request", "outcome", "learned", "decided", "next_steps",
              "facts", "concepts", "files_touched", "commands")


# ───────────────────────────── pure functions ──────────────────────────────

def new_crontab(old_text: str, plugin_root: Path | None = None,
                home: Path | None = None) -> str:
    """Return the crontab text after hive takes over scheduling.

    Drops the legacy jhh3-mem + ws cron lines (matched conservatively by the
    live shapes: the `# jhh3-mem-schedule` tag, and invocations of the ws
    launcher / its sync|watchdog|up verbs), strips any pre-existing
    `# hive-tick` lines (idempotency), and appends the two hive lines.

    $HOME is expanded at generation time — cron does not expand it in the
    schedule position.
    """
    home = Path(home) if home else Path.home()
    h = str(home)

    def _drop(line: str) -> bool:
        stripped = line.strip()
        if CRON_TAG in line:
            return True
        if "jhh3-mem" in line:
            return True
        for pat in ("/bin/ws ", "ws watchdog", "ws up", "ws sync"):
            if pat in line:
                return True
        return False

    kept = [ln for ln in old_text.splitlines() if not _drop(ln)]
    # Trim trailing blank lines so appended lines don't drift downward.
    while kept and not kept[-1].strip():
        kept.pop()

    tick = config.load().get("tick", config.DEFAULTS["tick"])
    mem_max = tick.get("memory_max", "2G")
    cpu_quota = tick.get("cpu_quota", "50%")

    # PATH must carry ~/.local/bin: enrichment resolves `claude` there and
    # cron's default PATH is /usr/bin:/bin (same prefix the old jhh3-mem
    # cron used). --scope children inherit the exported environment.
    path = f'PATH="{h}/.local/bin:/usr/local/bin:/usr/bin:/bin"'
    hive_lines = [
        (f"*/5 * * * * /bin/sh -c '. {h}/.env.private 2>/dev/null; "
         f"export {path}; "
         f"systemd-run --user --scope -q -p MemoryMax={mem_max} "
         f"-p CPUQuota={cpu_quota} --slice=hive {h}/.local/bin/hive tick' "
         f">/dev/null 2>&1 {CRON_TAG}"),
        (f"@reboot sleep 20; /bin/sh -c '. {h}/.env.private 2>/dev/null; "
         f"export {path}; "
         f"{h}/.local/bin/hive tick --boot' >/dev/null 2>&1 {CRON_TAG}"),
    ]
    return "\n".join(kept + hive_lines) + "\n"


def strip_agent_deck(settings: dict) -> dict:
    """Remove every hook entry whose command contains 'agent-deck'.

    Now-empty matcher groups and events are deleted; everything else —
    notably claude-hook-notify entries — is preserved verbatim.
    """
    out = copy.deepcopy(settings)
    hooks = out.get("hooks")
    if not isinstance(hooks, dict):
        return out
    for event in list(hooks.keys()):
        groups = hooks.get(event)
        if not isinstance(groups, list):
            continue
        new_groups = []
        for group in groups:
            entries = group.get("hooks") if isinstance(group, dict) else None
            if isinstance(entries, list):
                entries = [e for e in entries
                           if "agent-deck" not in str(e.get("command", ""))]
                group = dict(group, hooks=entries)
                if not entries:
                    continue  # empty matcher group: delete
            new_groups.append(group)
        if new_groups:
            hooks[event] = new_groups
        else:
            del hooks[event]  # empty event: delete
    return out


# ───────────────────────────── small helpers ───────────────────────────────

def _say(msg: str) -> None:
    print(f"hive: {msg}")


def _step(dry_run: bool, desc: str, fn) -> None:
    """Log one install step; on --dry-run print what would change and skip."""
    if dry_run:
        _say(f"[dry-run] would {desc}")
        return
    _say(desc)
    fn()


def _read_crontab() -> str:
    try:
        p = subprocess.run(["crontab", "-l"], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.TimeoutExpired):
        return ""
    return p.stdout if p.returncode == 0 else ""


def _write_crontab(text: str) -> None:
    subprocess.run(["crontab", "-"], input=text, text=True, check=True, timeout=10)


def _backup(text: str, name: str) -> Path:
    config.STATE_DIR.mkdir(parents=True, exist_ok=True)
    dst = config.STATE_DIR / f"{name}.bak.{int(time.time())}"
    dst.write_text(text)
    return dst


def _write_shim(src_name: str, dst_name: str) -> Path:
    src = SHIMS_DIR / src_name
    dst = LOCAL_BIN / dst_name
    LOCAL_BIN.mkdir(parents=True, exist_ok=True)
    dst.write_text(src.read_text())
    dst.chmod(0o755)
    return dst


def _connect_ro(path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=2.0)
    conn.row_factory = sqlite3.Row
    return conn


def _fmt_size(n: int) -> str:
    for unit in ("B", "KB", "MB", "GB"):
        if n < 1024 or unit == "GB":
            return f"{n:.1f}{unit}" if unit != "B" else f"{n}B"
        n /= 1024
    return f"{n}GB"


def _fmt_age(seconds: float) -> str:
    if seconds < 90:
        return f"{int(seconds)}s"
    if seconds < 5400:
        return f"{int(seconds / 60)}m"
    if seconds < 172800:
        return f"{seconds / 3600:.1f}h"
    return f"{seconds / 86400:.1f}d"


# ───────────────────────────── doctor helpers ──────────────────────────────

def parse_hook_metrics(lines: list[str], now: float | None = None,
                       window_s: int = 7 * 86400) -> dict[str, list[int]]:
    """{event: [ms, ...]} from hook-harness metrics lines within the window.

    Line shape (hook-harness.sh): `<iso> [<event>] ok|fail rc=<rc> ms=<ms>`.
    Malformed lines are skipped, never fatal.
    """
    import datetime
    now = time.time() if now is None else now
    out: dict[str, list[int]] = {}
    for ln in lines:
        parts = ln.split()
        if len(parts) < 4 or not (parts[1].startswith("[")
                                  and parts[1].endswith("]")):
            continue
        try:
            ts = datetime.datetime.fromisoformat(parts[0]).timestamp()
        except ValueError:
            continue
        if now - ts > window_s:
            continue
        ms_part = next((p for p in parts if p.startswith("ms=")), None)
        if ms_part is None:
            continue
        try:
            ms = int(ms_part[3:])
        except ValueError:
            continue
        out.setdefault(parts[1][1:-1], []).append(ms)
    return out


def percentile(values: list[int], q: float) -> int:
    """Nearest-rank percentile; values need not be sorted."""
    vs = sorted(values)
    return vs[min(len(vs) - 1, max(0, int(q * len(vs))))]


def _hook_metric_lines() -> list[str]:
    lines: list[str] = []
    for name in ("hook-metrics.log.1", "hook-metrics.log"):
        try:
            lines.extend((config.STATE_DIR / name).read_text().splitlines())
        except OSError:
            pass
    return lines


def _stamp_age(name: str, now: float) -> float | None:
    try:
        return now - float((config.STATE_DIR / name).read_text().strip())
    except (OSError, ValueError):
        return None


def _last_commit_age(vault: Path, grep: str, now: float) -> float | None:
    """Age of the last commit whose subject matches `grep` (our sync commits)."""
    try:
        r = subprocess.run(
            ["git", "-C", str(vault), "log", "-1", "--format=%ct",
             f"--grep={grep}"], capture_output=True, text=True, timeout=15)
        if r.returncode == 0 and r.stdout.strip():
            return now - int(r.stdout.strip().splitlines()[0])
    except (OSError, subprocess.TimeoutExpired, ValueError):
        pass
    return None


def _last_pull_age(vault: Path, now: float) -> float | None:
    """Age of the most recent pull/fetch reflog entry (bounded scan)."""
    try:
        r = subprocess.run(
            ["git", "-C", str(vault), "log", "-g", "-n", "200",
             "--format=%ct %gs", "HEAD"],
            capture_output=True, text=True, timeout=15)
        if r.returncode != 0:
            return None
        for line in r.stdout.splitlines():
            ts, _, msg = line.partition(" ")
            if "pull" in msg or "fetch" in msg:
                return now - int(ts)
    except (OSError, subprocess.TimeoutExpired, ValueError):
        pass
    return None


def sweep_backlog_count(limit: int = 4000) -> int | None:
    """Recent transcripts whose size differs from the recorded capture offset.

    Read-only and bounded: at most `limit` files are examined. None means the
    DB was unreadable (no signal, not zero).
    """
    dbp = db.path()
    if not dbp.exists():
        return None
    offsets: dict[tuple[str, str], int] = {}
    try:
        conn = _connect_ro(dbp)
        try:
            offsets = {(r["harness"], r["session_id"]): r["offset_bytes"]
                       for r in conn.execute(
                           "SELECT harness, session_id, offset_bytes "
                           "FROM hive_capture_state")}
        except sqlite3.Error:
            pass  # hive schema not applied yet: every recent transcript counts
        finally:
            conn.close()
    except sqlite3.Error:
        return None
    cutoff = time.time() - 7 * 86400
    backlog = scanned = 0
    for root, harness, pattern in (
            (config.CLAUDE_PROJECTS, "claude-code", "*/*.jsonl"),
            (config.CODEX_SESSIONS, "codex", "**/*.jsonl")):
        if not root.is_dir():
            continue
        for p in root.glob(pattern):
            if scanned >= limit:
                break
            scanned += 1
            try:
                st = p.stat()
            except OSError:
                continue
            if st.st_mtime < cutoff:
                continue
            if offsets.get((harness, p.name.removesuffix(".jsonl"))) != st.st_size:
                backlog += 1
    return backlog


# ───────────────────────────── install / uninstall ─────────────────────────

def _minimal_toml() -> str:
    return ("# hive configuration — minimal stub written by `hive install`.\n"
            "# See docs/hive.toml.example in jhh3/hive for the full reference.\n"
            "[repos]\n")


def _toml_example() -> Path | None:
    for cand in (PLUGIN_ROOT.parent.parent / "docs" / "hive.toml.example",
                 PLUGIN_ROOT / "docs" / "hive.toml.example"):
        if cand.exists():
            return cand
    return None


def cmd_install(rest: list[str]) -> int:
    dry_run = "--dry-run" in rest
    take_over = "--take-over" in rest
    remove_deck = "--remove-agent-deck" in rest
    unknown = [a for a in rest if a not in ("--dry-run", "--take-over", "--remove-agent-deck")]
    if unknown:
        util.eprint(f"hive install: unknown args: {' '.join(unknown)}")
        return 2

    # 1. config file
    if config.CONFIG_PATH.exists():
        _say(f"config present: {config.CONFIG_PATH}")
    else:
        example = _toml_example()

        def _write_cfg():
            config.CONFIG_PATH.parent.mkdir(parents=True, exist_ok=True)
            if example:
                shutil.copyfile(example, config.CONFIG_PATH)
            else:
                config.CONFIG_PATH.write_text(_minimal_toml())

        src = str(example) if example else "minimal [repos] stub"
        _step(dry_run, f"write {config.CONFIG_PATH} from {src}", _write_cfg)

    # 2. launcher shim
    _step(dry_run, f"write {LOCAL_BIN / 'hive'} from shims/hive.sh (chmod +x)",
          lambda: _write_shim("hive.sh", "hive"))

    # 2a. codex parity: AGENTS.md hooks (module may not be built yet; skip).
    try:
        from . import codexparity
    except ImportError:
        codexparity = None
    if codexparity is not None:
        try:
            codexparity.ensure_agents_md(dry_run=dry_run)
        except Exception as e:
            _say(f"codexparity.ensure_agents_md failed (non-fatal): {e!r}")
    else:
        _say("codexparity module absent — skipping AGENTS.md parity step")

    # 2b. zsh completion (shipped with the plugin when present).
    comp_src = SHIMS_DIR / "hive-completion.zsh"
    if comp_src.exists():
        comp_dst = Path.home() / ".zsh" / "hive-completion.zsh"

        def _copy_comp():
            comp_dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(comp_src, comp_dst)

        _step(dry_run, f"copy shims/hive-completion.zsh -> {comp_dst}",
              _copy_comp)
    else:
        _say("no shims/hive-completion.zsh — skipping completion install")

    # 3. take-over alias shims
    if take_over:
        for src_name, dst_name in (("ws.sh", "ws"), ("jhh3-mem.sh", "jhh3-mem")):
            _step(dry_run,
                  f"overwrite {LOCAL_BIN / dst_name} with hive alias shim "
                  "(original launcher was plugin-generated and recreatable via "
                  "`ws setup` / `jhh3-mem setup`)",
                  lambda s=src_name, d=dst_name: _write_shim(s, d))

    # 4. crontab
    old = _read_crontab()
    if take_over:
        new = new_crontab(old, PLUGIN_ROOT)
    else:
        # Without --take-over the legacy ws/jhh3-mem schedules stay in place;
        # only refresh hive's own lines.
        kept = [ln for ln in old.splitlines() if CRON_TAG not in ln]
        while kept and not kept[-1].strip():
            kept.pop()
        hive_only = [ln for ln in new_crontab("", PLUGIN_ROOT).splitlines() if ln.strip()]
        new = "\n".join(kept + hive_only) + "\n"
    if new == old:
        _say("crontab already current")
    else:
        def _apply_cron():
            bak = _backup(old, "crontab")
            _say(f"crontab backup -> {bak}")
            _write_crontab(new)

        desc = "install hive-tick crontab lines"
        if take_over:
            desc += " and remove jhh3-mem/ws cron lines"
        _step(dry_run, desc, _apply_cron)
        if dry_run:
            removed = [ln for ln in old.splitlines()
                       if ln.strip() and ln not in new.splitlines()]
            for ln in removed:
                _say(f"[dry-run]   - {ln}")
            for ln in new.splitlines():
                if CRON_TAG in ln and ln not in old.splitlines():
                    _say(f"[dry-run]   + {ln}")

    # 5. settings.json agent-deck removal
    if remove_deck:
        try:
            settings = json.loads(SETTINGS_PATH.read_text())
        except (OSError, json.JSONDecodeError) as e:
            util.eprint(f"hive install: cannot read {SETTINGS_PATH}: {e}")
            return 1
        stripped = strip_agent_deck(settings)
        if stripped == settings:
            _say("settings.json has no agent-deck hooks")
        else:
            def _apply_settings():
                bak = SETTINGS_PATH.with_name(f"settings.json.bak.{int(time.time())}")
                bak.write_text(SETTINGS_PATH.read_text())
                _say(f"settings.json backup -> {bak}")
                SETTINGS_PATH.write_text(json.dumps(stripped, indent=2) + "\n")

            _step(dry_run, f"remove agent-deck hook entries from {SETTINGS_PATH} "
                           "(claude-hook-notify preserved)", _apply_settings)

    # 6. vaults
    raw = config.RAW_VAULT
    if not (raw / ".git").exists():
        def _init_raw():
            raw.mkdir(parents=True, exist_ok=True)
            subprocess.run(["git", "init", "-q", "-b", "main", str(raw)],
                           check=True, timeout=30, capture_output=True)

        _step(dry_run, f"git init raw vault at {raw}", _init_raw)
    else:
        _say(f"raw vault present: {raw}")

    streams = config.STREAMS_VAULT
    machine = config.machine_id()
    lane_dirs = [streams / "mem" / "lanes" / machine,
                 streams / "mem" / "tombstones",
                 streams / "handoffs",
                 streams / "meta"]

    def _mk_streams():
        for d in lane_dirs:
            d.mkdir(parents=True, exist_ok=True)
        epoch = streams / "meta" / "epoch"
        if not epoch.exists():
            epoch.write_text("1\n")

    _step(dry_run, f"create mem/lanes/{machine}, mem/tombstones, handoffs/, "
                   f"meta/epoch=1 under {streams} (mkdir only — hive never "
                   "commits ~/streams itself)", _mk_streams)

    # 7. next steps
    print()
    _say("next steps:")
    print("  claude plugin marketplace add jhh3/hive")
    print("  claude plugin install hive@hive")
    print("  hive doctor")
    if not take_over:
        print("  hive install --take-over   # when ready: alias shims + cron takeover")
    return 0


def cmd_uninstall(rest: list[str]) -> int:
    dry_run = "--dry-run" in rest
    old = _read_crontab()
    kept = [ln for ln in old.splitlines() if CRON_TAG not in ln]
    new = "\n".join(kept)
    if new and not new.endswith("\n"):
        new += "\n"
    if new != old:
        def _apply():
            bak = _backup(old, "crontab")
            _say(f"crontab backup -> {bak}")
            _write_crontab(new)

        _step(dry_run, "remove hive-tick crontab lines", _apply)
    else:
        _say("no hive-tick crontab lines")

    for name in ("hive", "ws", "jhh3-mem"):
        p = LOCAL_BIN / name
        try:
            text = p.read_text()
        except OSError:
            continue
        if "hive" in text:  # only remove shims hive itself generated
            _step(dry_run, f"remove shim {p}", lambda q=p: q.unlink())
    _say("hooks unwire themselves when the plugin is uninstalled; "
         "data (DB, vaults) left untouched")
    return 0


# ───────────────────────────── doctor / status ─────────────────────────────

def _check(status: str, label: str, detail: str = "") -> bool:
    line = f"  [{status:>4}] {label}"
    if detail:
        line += f" — {detail}"
    print(line)
    return status == "FAIL"


def cmd_doctor(rest: list[str]) -> int:
    failed = False
    print("hive doctor")

    print("deps:")
    for dep in ("python3", "sqlite3", "zstd", "git", "tmux", "systemd-run"):
        if shutil.which(dep):
            _check("OK", dep)
        else:
            failed |= _check("FAIL" if dep in ("python3", "git", "sqlite3") else "WARN",
                             dep, "not on PATH")

    print("database:")
    dbp = db.path()
    if not dbp.exists():
        failed |= _check("FAIL", str(dbp), "missing")
    else:
        try:
            conn = _connect_ro(dbp)
            try:
                n = conn.execute("SELECT count(*) FROM entries").fetchone()[0]
                pending = conn.execute(
                    "SELECT count(*) FROM entries "
                    "WHERE capture_mode='deterministic' AND superseded=0").fetchone()[0]
                _check("OK", str(dbp),
                       f"{_fmt_size(dbp.stat().st_size)}, {n} entries, "
                       f"{pending} pending enrich")
                hive_tables = [r[0] for r in conn.execute(
                    "SELECT name FROM sqlite_master WHERE type='table' "
                    "AND name LIKE 'hive_%' ORDER BY name")]
                if hive_tables:
                    _check("OK", "hive_* tables", ", ".join(hive_tables))
                else:
                    _check("WARN", "hive_* tables", "absent (schema not applied yet)")
            finally:
                conn.close()
        except sqlite3.Error as e:
            failed |= _check("FAIL", str(dbp), f"unreadable: {e}")

    print("hooks/tick:")
    tripped = []
    for b in sorted(config.STATE_DIR.glob("breaker.*")):
        if b.name.endswith(".notified"):
            continue
        try:
            if int(b.read_text().strip() or 0) >= 3:
                tripped.append(b.name.split(".", 1)[1])
        except (OSError, ValueError):
            pass
    if tripped:
        failed |= _check("WARN", "breakers", f"tripped: {', '.join(tripped)}")
    else:
        _check("OK", "breakers", "none tripped")
    if config.TICK_LOG.exists():
        age = time.time() - config.TICK_LOG.stat().st_mtime
        status = "WARN" if age > 20 * 60 else "OK"
        _check(status, "last tick", f"{_fmt_age(age)} ago")
    else:
        _check("WARN", "last tick", "no tick log yet")
    if CRON_TAG in _read_crontab():
        _check("OK", "crontab", f"{CRON_TAG} present")
    else:
        _check("WARN", "crontab", f"no {CRON_TAG} lines (run `hive install`)")
    stats = parse_hook_metrics(_hook_metric_lines())
    if stats:
        for event in sorted(stats):
            ms = stats[event]
            p50, p95 = percentile(ms, 0.5), percentile(ms, 0.95)
            status = "WARN" if (event == "SessionStart" and p95 > 300) else "OK"
            _check(status, f"{event} latency",
                   f"{len(ms)} run(s) 7d, p50 {p50}ms, p95 {p95}ms")
    else:
        _check("OK", "hook latency", "no metrics recorded yet")
    backlog = sweep_backlog_count()
    if backlog is None:
        _check("WARN", "sweep backlog", "db unreadable")
    else:
        _check("WARN" if backlog > 25 else "OK", "sweep backlog",
               f"{backlog} transcript(s)")

    print("sync:")
    now = time.time()
    for label, vault, stamp, grep in (
            ("streams", config.STREAMS_VAULT, "sync.push.stamp", "^hive sync"),
            ("raw", config.RAW_VAULT, "raw.push.stamp", "^hive raw")):
        if not (vault / ".git").exists():
            _check("WARN", f"{label} sync", "no git repo")
            continue
        page = _stamp_age(stamp, now)
        if page is None:
            page = _last_commit_age(vault, grep, now)
        if page is None:
            _check("WARN", f"{label} last push", "none recorded")
        else:
            _check("WARN" if page > 26 * 3600 else "OK",
                   f"{label} last push", f"{_fmt_age(page)} ago")
        page = _last_pull_age(vault, now)
        if page is None:
            _check("OK", f"{label} last pull", "none in reflog")
        else:
            _check("WARN" if page > 26 * 3600 else "OK",
                   f"{label} last pull", f"{_fmt_age(page)} ago")

    print("vaults:")
    streams = config.STREAMS_VAULT
    if (streams / ".git").exists():
        try:
            out = subprocess.run(
                ["git", "-C", str(streams), "log", "-1", "--format=%ct"],
                capture_output=True, text=True, timeout=15)
            if out.returncode == 0 and out.stdout.strip():
                age = time.time() - int(out.stdout.strip())
                _check("WARN" if age > 26 * 3600 else "OK",
                       "streams last commit", f"{_fmt_age(age)} ago")
            sb = subprocess.run(["git", "-C", str(streams), "status", "-sb"],
                                capture_output=True, text=True, timeout=15)
            head = sb.stdout.splitlines()[0] if sb.stdout else ""
            drift = "ahead" in head or "behind" in head
            _check("WARN" if drift else "OK", "streams vs origin",
                   head or "unknown", )
        except (OSError, subprocess.TimeoutExpired, ValueError) as e:
            failed |= _check("WARN", "streams git", str(e))
    else:
        failed |= _check("FAIL", str(streams), "not a git repo")
    raw = config.RAW_VAULT
    if (raw / ".git").exists():
        rem = subprocess.run(["git", "-C", str(raw), "remote", "get-url", "origin"],
                             capture_output=True, text=True, timeout=15)
        if rem.returncode == 0:
            _check("OK", "raw vault", rem.stdout.strip())
        else:
            _check("WARN", "raw vault", "no origin remote configured")
    else:
        _check("WARN", str(raw), "missing (run `hive install`)")

    print("vendored tools:")
    try:
        rc = vendored.mem(["status"], timeout=20).returncode
        _check("OK" if rc == 0 else "WARN", "jhh3-mem status", f"rc={rc}")
    except (OSError, subprocess.TimeoutExpired) as e:
        failed |= _check("FAIL", "jhh3-mem", str(e))
    try:
        rc = vendored.ws(["help"], timeout=20).returncode
        _check("OK" if rc == 0 else "WARN", "ws help", f"rc={rc}")
    except (OSError, subprocess.TimeoutExpired) as e:
        failed |= _check("FAIL", "ws", str(e))

    print("decommission:")
    try:
        stext = SETTINGS_PATH.read_text()
        if "agent-deck" in stext:
            _check("WARN", "settings.json",
                   "still contains agent-deck hooks "
                   "(run `hive install --remove-agent-deck`)")
        else:
            _check("OK", "settings.json", "no agent-deck hooks")
    except OSError:
        _check("WARN", "settings.json", "unreadable")
    try:
        plugins = json.loads(INSTALLED_PLUGINS.read_text()).get("plugins", {})
        legacy = [k for k in ("jhh3-mem@acme-plugins", "ws@streams") if k in plugins]
        if legacy:
            _check("WARN", "old plugins",
                   f"decommission pending: {', '.join(legacy)}")
        else:
            _check("OK", "old plugins", "none installed")
    except (OSError, json.JSONDecodeError):
        _check("WARN", "installed_plugins.json", "unreadable")

    return 1 if failed else 0


def cmd_status(rest: list[str]) -> int:
    machine = config.machine_id()
    dbp = db.path()
    entries = live = pending = "-"
    size = "-"
    if dbp.exists():
        size = _fmt_size(dbp.stat().st_size)
        try:
            conn = _connect_ro(dbp)
            try:
                def _q(sql):
                    try:
                        return conn.execute(sql).fetchone()[0]
                    except sqlite3.Error:
                        return "-"
                entries = _q("SELECT count(*) FROM entries")
                live = _q("SELECT count(*) FROM hive_sessions WHERE ended_at IS NULL")
                pending = _q("SELECT count(*) FROM hive_jobs WHERE state='pending'")
            finally:
                conn.close()
        except sqlite3.Error:
            pass
    tick = "never"
    if config.TICK_LOG.exists():
        tick = f"{_fmt_age(time.time() - config.TICK_LOG.stat().st_mtime)} ago"
    vault = "no vault"
    if (config.STREAMS_VAULT / ".git").exists():
        out = subprocess.run(
            ["git", "-C", str(config.STREAMS_VAULT), "log", "-1", "--format=%ct"],
            capture_output=True, text=True, timeout=15)
        if out.returncode == 0 and out.stdout.strip():
            vault = f"last commit {_fmt_age(time.time() - int(out.stdout.strip()))} ago"

    print(f"machine:       {machine}")
    print(f"db:            {dbp} ({size}, {entries} entries)")
    print(f"live sessions: {live}")
    print(f"last tick:     {tick}")
    print(f"vault lag:     {vault}")
    print(f"pending jobs:  {pending}")
    return 0


# ───────────────────────────── migrate ─────────────────────────────────────

def _lanes_dir() -> Path:
    return config.STREAMS_VAULT / "mem" / "lanes" / config.machine_id()


def _history_files() -> list[Path]:
    d = _lanes_dir()
    return sorted(d.glob("history-*.ndjson")) if d.exists() else []


def _entry_columns(conn: sqlite3.Connection) -> list[str]:
    return [r[1] for r in conn.execute("PRAGMA table_info(entries)")]


def _redact_lines(lines: list[str]) -> list[str]:
    """Pass NDJSON lines through the vendored redact.sed gate. Fail closed."""
    text = "\n".join(lines) + "\n" if lines else ""
    if not text:
        return []
    p = subprocess.run(["sed", "-E", "-f", str(REDACT_SED)],
                       input=text, capture_output=True, text=True, timeout=120)
    if p.returncode != 0 or (text and not p.stdout):
        raise RuntimeError(
            f"redaction failed (sed rc={p.returncode}); refusing to export")
    out = p.stdout.splitlines()
    if len(out) != len(lines):
        raise RuntimeError("redaction changed line count; refusing to export")
    # sed can eat JSON structure (key=value rules spanning escaped quotes).
    # Field content was already redacted at capture time; on corruption keep
    # the pre-sed line so lanes never carry an unparseable record.
    fixed = []
    for src, red in zip(lines, out):
        if red != src:
            try:
                json.loads(red)
            except ValueError:
                red = src
        fixed.append(red)
    return fixed


def _month_of(ts) -> str:
    try:
        return time.strftime("%Y-%m", time.localtime(int(ts)))
    except (ValueError, TypeError, OverflowError):
        return "unknown"


def cmd_migrate(rest: list[str]) -> int:
    export = "--export-history" in rest
    verify = "--verify" in rest
    force = "--force" in rest

    dbp = db.path()
    if not dbp.exists():
        util.eprint(f"hive migrate: no database at {dbp}")
        return 1
    conn = _connect_ro(dbp)
    try:
        if export:
            rc = _migrate_export(conn, force)
            if rc != 0:
                return rc
        if verify:
            return _migrate_verify(conn)
        if not export:
            return _migrate_plan(conn, dbp)
        return 0
    finally:
        conn.close()


def _migrate_plan(conn: sqlite3.Connection, dbp: Path) -> int:
    total = conn.execute("SELECT count(*) FROM entries").fetchone()[0]
    print(f"hive migrate (dry-run) — {dbp}")
    print(f"  db size: {_fmt_size(dbp.stat().st_size)}, entries: {total}")
    for facet in ("origin", "host", "kind"):
        rows = conn.execute(
            f"SELECT {facet}, count(*) FROM entries GROUP BY {facet} "
            "ORDER BY count(*) DESC").fetchall()
        print(f"  by {facet}: " + ", ".join(f"{r[0]}={r[1]}" for r in rows))
    est = conn.execute(
        "SELECT SUM(" + "+".join(f"length({c})" for c in _TEXT_COLS) +
        ") FROM entries").fetchone()[0] or 0
    # NDJSON framing + keys roughly doubles the raw text bytes.
    print(f"  text bytes: {_fmt_size(est)} (lane estimate ~{_fmt_size(int(est * 1.6))})")
    print(f"  plan: export ALL {total} entries (batched 2000, ORDER BY id) as "
          f"rev=1 put records into {_lanes_dir()}/history-<YYYY-MM>.ndjson,")
    print("        including host, source_bytes and the path/entry_file lane, "
          "through the redact.sed gate (fail-closed);")
    print("        then `hive migrate --verify` recounts and content-compares "
          "200 random uids. Run with --export-history to execute.")
    return 0


def _migrate_export(conn: sqlite3.Connection, force: bool) -> int:
    existing = _history_files()
    if existing and not force:
        util.eprint(
            f"hive migrate: history lanes already exist under {_lanes_dir()} "
            f"({len(existing)} file(s)); delete them and pass --force to redo")
        return 1
    if existing and force:
        for f in existing:
            f.unlink()
    lanes = _lanes_dir()
    lanes.mkdir(parents=True, exist_ok=True)

    cols = _entry_columns(conn)
    machine = config.machine_id()
    total = 0
    last_id = 0
    handles: dict[str, object] = {}
    try:
        while True:
            rows = conn.execute(
                f"SELECT {', '.join(cols)} FROM entries WHERE id > ? "
                "ORDER BY id LIMIT 2000", (last_id,)).fetchall()
            if not rows:
                break
            months, lines = [], []
            for row in rows:
                entry = {c: row[c] for c in cols}
                last_id = entry["id"]
                files = conn.execute(
                    "SELECT p.p, ef.mode FROM entry_file ef "
                    "JOIN path p ON p.id = ef.path_id WHERE ef.entry_id = ? "
                    "ORDER BY p.p", (entry["id"],)).fetchall()
                rec = {
                    "op": "put",
                    "rev": 1,
                    "uid": entry["uid"],
                    "kind": entry.get("kind", ""),
                    "ts": entry.get("ts", 0),
                    "machine": machine,
                    "entry": entry,
                    "files": [[f[0], f[1]] for f in files],
                }
                months.append(_month_of(entry.get("ts")))
                lines.append(json.dumps(rec, ensure_ascii=False,
                                        separators=(",", ":")))
            try:
                redacted = _redact_lines(lines)
            except RuntimeError as e:
                util.eprint(f"hive migrate: {e}")
                return 1
            for month, line in zip(months, redacted):
                fh = handles.get(month)
                if fh is None:
                    fh = open(lanes / f"history-{month}.ndjson", "a",
                              encoding="utf-8")
                    handles[month] = fh
                fh.write(line + "\n")
            total += len(rows)
            print(f"  exported {total} entries…", end="\r", flush=True)
    finally:
        for fh in handles.values():
            fh.close()
    # Seed hive_revs at 1 for every exported uid: future local edits must
    # export at rev 2+, or importers that saw the history skip them.
    # (The export conn may be read-only; seed through a write connection.)
    try:
        with db.connect() as wconn:
            db.ensure_hive_schema(wconn)
            wconn.execute(
                "INSERT INTO hive_revs(uid, rev) SELECT uid, 1 FROM entries "
                "GROUP BY uid ON CONFLICT(uid) DO UPDATE SET rev=MAX(rev, 1)")
            wconn.commit()
    except sqlite3.Error as e:
        util.eprint(f"hive migrate: hive_revs seeding failed ({e}) — "
                    f"run it manually before enabling sync on other machines")
    print(f"hive migrate: exported {total} entries into "
          f"{len(_history_files())} lane file(s) under {lanes}")
    return 0


def _migrate_verify(conn: sqlite3.Connection) -> int:
    files = _history_files()
    if not files:
        util.eprint("hive migrate --verify: no history lanes to verify")
        return 1
    total_db = conn.execute("SELECT count(*) FROM entries").fetchone()[0]

    ids = [r[0] for r in conn.execute("SELECT id FROM entries")]
    sample = set(random.sample(ids, min(200, len(ids))))
    wanted: dict[tuple, sqlite3.Row] = {}
    for eid in sample:
        row = conn.execute("SELECT * FROM entries WHERE id = ?", (eid,)).fetchone()
        wanted[(row["uid"], row["source_bytes"])] = row

    lane_count = 0
    matched = 0
    mismatched: list[str] = []
    for f in files:
        with open(f, encoding="utf-8") as fh:
            for line in fh:
                if not line.strip():
                    continue
                lane_count += 1
                rec = util.read_json_line(line)
                if not rec:
                    continue
                entry = rec.get("entry", {})
                key = (entry.get("uid"), entry.get("source_bytes"))
                row = wanted.get(key)
                if row is None:
                    continue
                ok = all(
                    json.dumps(entry.get(c, "")) == json.dumps(row[c])
                    for c in ("title", "outcome", "learned"))
                if ok:
                    matched += 1
                else:
                    mismatched.append(str(key))
                del wanted[key]

    print(f"hive migrate --verify: lanes={lane_count} db={total_db}")
    count_ok = lane_count == total_db
    content_ok = not mismatched and not wanted
    print(f"  count: {'PASS' if count_ok else 'FAIL'}")
    print(f"  content ({matched + len(mismatched)} sampled): "
          f"{'PASS' if content_ok else 'FAIL'}")
    if mismatched:
        print(f"  mismatched: {', '.join(mismatched[:5])}")
    if wanted:
        print(f"  missing from lanes: {len(wanted)} sampled row(s)")
    return 0 if (count_ok and content_ok) else 1


# ───────────────────────────── rebuild ─────────────────────────────────────

def _apply_put(conn: sqlite3.Connection, rec: dict,
               cols: list[str] | None = None) -> bool:
    """Apply one lane 'put' record to a DB with the jhh3-mem schema.

    Local re-implementation on purpose: rebuild must not depend on hive.sync
    (built in parallel). Returns True if a row was inserted.
    """
    entry = rec.get("entry") or {}
    if not entry.get("uid"):
        return False
    if cols is None:
        cols = _entry_columns(conn)
    use = [c for c in cols if c != "id" and c in entry]
    cur = conn.execute(
        f"INSERT OR IGNORE INTO entries ({', '.join(use)}) "
        f"VALUES ({', '.join('?' for _ in use)})",
        [entry[c] for c in use])
    if cur.rowcount == 0:
        return False
    entry_id = cur.lastrowid
    # History records carry files at the top level; live sync records nest
    # them under entry. Accept both.
    for item in (rec.get("files") or entry.get("files") or []):
        try:
            p, mode = item[0], int(item[1])
        except (IndexError, TypeError, ValueError):
            continue
        conn.execute("INSERT OR IGNORE INTO path (p, base) VALUES (?, ?)",
                     (p, os.path.basename(p)))
        pid = conn.execute("SELECT id FROM path WHERE p = ?", (p,)).fetchone()[0]
        conn.execute(
            "INSERT OR IGNORE INTO entry_file (path_id, entry_id, mode) "
            "VALUES (?, ?, ?)", (pid, entry_id, mode))
    return True


def cmd_rebuild(rest: list[str]) -> int:
    to: str | None = None
    args = list(rest)
    while args:
        a = args.pop(0)
        if a == "--to" and args:
            to = args.pop(0)
        elif a.startswith("--to="):
            to = a.split("=", 1)[1]
        else:
            util.eprint(f"hive rebuild: unknown arg {a}")
            return 2
    if not to:
        util.eprint("hive rebuild: --to PATH is required")
        return 2
    dst = Path(to).expanduser()
    if dst.exists():
        util.eprint(f"hive rebuild: {dst} already exists; refusing to overwrite")
        return 1

    lanes_root = config.STREAMS_VAULT / "mem" / "lanes"
    lane_files = sorted(lanes_root.glob("*/*.ndjson")) if lanes_root.exists() else []
    if not lane_files:
        util.eprint(f"hive rebuild: no lane files under {lanes_root}")
        return 1

    dst.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(dst)
    conn.row_factory = sqlite3.Row
    try:
        conn.executescript(MEM_SCHEMA_SQL.read_text())
        db.ensure_hive_schema(conn)
        cols = _entry_columns(conn)
        applied = skipped = 0
        for f in lane_files:
            with open(f, encoding="utf-8") as fh:
                for line in fh:
                    if not line.strip():
                        continue
                    rec = util.read_json_line(line)
                    if not rec or rec.get("op") != "put":
                        skipped += 1
                        continue
                    if _apply_put(conn, rec, cols):
                        applied += 1
                    else:
                        skipped += 1
            conn.commit()
        conn.commit()
        n = conn.execute("SELECT count(*) FROM entries").fetchone()[0]
        npath = conn.execute("SELECT count(*) FROM path").fetchone()[0]
        nef = conn.execute("SELECT count(*) FROM entry_file").fetchone()[0]
    finally:
        conn.close()
    print(f"hive rebuild: {dst}")
    print(f"  lane files: {len(lane_files)}, records applied: {applied}, "
          f"skipped: {skipped}")
    print(f"  entries: {n}, paths: {npath}, entry_files: {nef}")
    print("  FTS was populated by the entries triggers during insert; "
          "embeddings can be regenerated via `hive mem embed`.")
    return 0


# ───────────────────────────── dispatch ────────────────────────────────────

def run(verb: str, rest: list[str]) -> int:
    try:
        if verb == "install":
            return cmd_install(rest)
        if verb == "uninstall":
            return cmd_uninstall(rest)
        if verb == "doctor":
            return cmd_doctor(rest)
        if verb == "status":
            return cmd_status(rest)
        if verb == "migrate":
            return cmd_migrate(rest)
        if verb == "rebuild":
            return cmd_rebuild(rest)
    except KeyboardInterrupt:
        return 130
    util.eprint(f"hive admin: unknown verb {verb}")
    return 2
