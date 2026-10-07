"""Raw transcript lane: zstd-compressed segments in vault-raw (PLAN.md §4).

Transcripts are append-only, so each sync writes only the new bytes since
raw_synced_through as raw/<machine>/<harness>/<session-id>/<seq>.jsonl.zst
plus a small manifest.json byte map. Segments are per-machine — the same
single-writer property as the distilled lanes, so there are no conflicts.

The raw vault is its own git repo (init'd here if missing); a remote is
optional — with none configured, segments stay local and we log that once.
Unlike the distilled lanes, raw content is NOT redacted (resume fidelity;
PLAN.md §4 Q1/Q3 decision on record).
"""

from __future__ import annotations

import json
import subprocess
import time
from pathlib import Path

from . import config, db, util

SEGMENT_MIN_NEW_BYTES = 4096
LOCAL_MTIME_DAYS = 90


class RawError(Exception):
    pass


# ── git helpers (deliberately local — raw must not depend on sync) ──────────

def _iso() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%S")


def _git(vault: Path, *args: str, timeout: int = 30) -> subprocess.CompletedProcess:
    return util.run(["git", "-C", str(vault), *args], timeout=timeout)


def _has_remote(vault: Path) -> bool:
    r = _git(vault, "remote")
    return r.returncode == 0 and bool(r.stdout.strip())


def _current_branch(vault: Path) -> str:
    r = _git(vault, "rev-parse", "--abbrev-ref", "HEAD")
    b = r.stdout.strip()
    return b if r.returncode == 0 and b and b != "HEAD" else "main"


def _git_push(vault: Path) -> bool:
    """push with rebase-retry (2 attempts)."""
    branch = _current_branch(vault)
    p = None
    for _ in range(2):
        p = _git(vault, "push", "origin", branch, timeout=60)
        if p.returncode == 0:
            return True
        _git(vault, "pull", "--rebase", "--autostash", "origin", branch, timeout=60)
    util.log(config.TICK_LOG,
             f"raw push failed after retry: "
             f"{(p.stderr if p else '').strip()[:200]}")
    return False


def _ensure_vault() -> Path:
    vault = config.RAW_VAULT
    if not (vault / ".git").exists():
        vault.mkdir(parents=True, exist_ok=True)
        r = util.run(["git", "init", "-q", "-b", "main", str(vault)],
                     timeout=15)
        if r.returncode != 0:
            raise RawError(f"git init failed at {vault}: "
                           f"{r.stderr.strip()[:200]}")
    return vault


# ── zstd (subprocess; every call bounded) ───────────────────────────────────

def _zstd_c(data: bytes) -> bytes:
    p = subprocess.run(["zstd", "-q", "-c"], input=data,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                       timeout=120)
    if p.returncode != 0:
        raise RawError(f"zstd compress failed: {p.stderr[:200]!r}")
    return p.stdout


def _zstd_d(path: Path) -> bytes:
    p = subprocess.run(["zstd", "-q", "-d", "-c", str(path)],
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                       timeout=120)
    if p.returncode != 0:
        raise RawError(f"zstd decompress failed for {path}: "
                       f"{p.stderr[:200]!r}")
    return p.stdout


# ── segment writer (tick phase) ─────────────────────────────────────────────

def _sync_session(conn, vault: Path, machine: str, s) -> int:
    """Write one new segment for a session if enough new bytes exist."""
    tp = Path(s["transcript_path"])
    try:
        size = tp.stat().st_size
    except OSError:
        return 0
    start = int(s["raw_synced_through"] or 0)
    if size <= start + SEGMENT_MIN_NEW_BYTES:
        return 0
    with open(tp, "rb") as f:
        f.seek(start)
        data = f.read(size - start)
    if not data:
        return 0
    end = start + len(data)

    d = vault / "raw" / machine / s["harness"] / s["session_id"]
    d.mkdir(parents=True, exist_ok=True)
    seq = len(list(d.glob("*.jsonl.zst")))
    (d / f"{seq:06d}.jsonl.zst").write_bytes(_zstd_c(data))

    mpath = d / "manifest.json"
    try:
        manifest = json.loads(mpath.read_text())
        if not isinstance(manifest, dict):
            raise ValueError
    except (OSError, ValueError):
        manifest = {"session_id": s["session_id"], "harness": s["harness"],
                    "machine": machine, "segments": []}
    manifest.update({"cwd": s["cwd"], "repo_key": s["repo_key"],
                     "stream_slug": s["stream_slug"]})
    manifest.setdefault("segments", []).append(
        {"seq": seq, "start": start, "end": end})
    mpath.write_text(json.dumps(manifest, indent=1) + "\n")

    conn.execute(
        "UPDATE hive_sessions SET raw_synced_through=? "
        "WHERE harness=? AND session_id=? AND machine=?",
        (end, s["harness"], s["session_id"], machine))
    conn.commit()
    return 1


def _push_debounced(vault: Path) -> None:
    interval = int(config.load()["sync"]["live_debounce_s"])
    with util.flocked(config.STATE_DIR / "raw.lock") as got:
        if not got:
            return
        if not _has_remote(vault):
            flag = config.STATE_DIR / "raw.no-remote"
            if not flag.exists():
                util.log(config.TICK_LOG,
                         f"raw: {vault} has no git remote — segments stay "
                         f"local until one is configured (logged once)")
                try:
                    flag.parent.mkdir(parents=True, exist_ok=True)
                    flag.touch()
                except OSError:
                    pass
            return
        stamp = config.STATE_DIR / "raw.push.stamp"
        now = time.time()
        try:
            last = float(stamp.read_text().strip())
        except (OSError, ValueError):
            last = 0.0
        if now - last < interval:
            return
        if _git_push(vault):
            try:
                stamp.write_text(str(now))
            except OSError:
                pass


def tick_phase() -> None:
    """Mirror new transcript bytes of live/recently-ended sessions. Fail-soft."""
    try:
        vault = _ensure_vault()
        machine = config.machine_id()
        now = db.now()
        conn = db.connect()
        db.ensure_hive_schema(conn)
    except Exception as e:
        util.log(config.TICK_LOG, f"raw: setup failed: {e!r}")
        return
    wrote = 0
    try:
        rows = conn.execute(
            "SELECT * FROM hive_sessions WHERE machine=? "
            "AND transcript_path != '' "
            "AND ((ended_at IS NULL AND last_activity > ?) OR ended_at > ?)",
            (machine, now - 86400, now - 86400)).fetchall()
        for s in rows:
            try:
                wrote += _sync_session(conn, vault, machine, s)
            except Exception as e:
                util.log(config.TICK_LOG,
                         f"raw: segment for {s['session_id']} failed: {e!r}")
    finally:
        conn.close()
    if not wrote:
        return
    try:
        _git(vault, "add", "-A", "--", "raw")
        if _git(vault, "diff", "--cached", "--quiet").returncode != 0:
            c = _git(vault, "commit", "-m", f"hive raw {_iso()} {machine}")
            if c.returncode != 0:
                util.log(config.TICK_LOG,
                         f"raw commit failed: {c.stderr.strip()[:200]}")
                return
        _push_debounced(vault)
    except Exception as e:
        util.log(config.TICK_LOG, f"raw: git phase failed: {e!r}")


# ── reassembly / query surface ──────────────────────────────────────────────

def _find_session_dir(session_id: str, harness: str) -> Path:
    root = config.RAW_VAULT / "raw"
    matches: list[Path] = []
    if root.is_dir():
        for mdir in sorted(root.iterdir()):
            hdir = mdir / harness
            if not hdir.is_dir():
                continue
            matches.extend(d for d in sorted(hdir.iterdir())
                           if d.is_dir() and d.name.startswith(session_id))
    exact = [d for d in matches if d.name == session_id]
    if exact:
        return exact[0]
    if not matches:
        raise RawError(f"no synced segments for {harness} session "
                       f"{session_id!r} under {root}")
    if len(matches) > 1:
        names = ", ".join(d.name for d in matches[:5])
        raise RawError(f"ambiguous session prefix {session_id!r}: {names}")
    return matches[0]


def import_session(session_id: str, harness: str = "claude-code") -> Path:
    """Reassemble a synced transcript for `claude --resume` on this machine."""
    d = _find_session_dir(session_id, harness)
    segs = sorted(d.glob("*.jsonl.zst"))
    if not segs:
        raise RawError(f"no segments in {d}")
    data = b"".join(_zstd_d(seg) for seg in segs)
    try:
        manifest = json.loads((d / "manifest.json").read_text())
        if not isinstance(manifest, dict):
            manifest = {}
    except (OSError, ValueError):
        manifest = {}
    sid = manifest.get("session_id") or d.name
    cwd = manifest.get("cwd") or ""

    if harness != "claude-code":
        dest = config.DATA_DIR / "raw" / "restored" / f"{sid}.jsonl"
        if dest.exists():
            raise RawError(f"refusing to overwrite existing file: {dest}")
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(data)
        print(f"reassembled {harness} transcript: {dest}")
        print(f"codex transcripts have no drop-in location; "
              f"resume with: codex resume {sid}")
        return dest

    # Claude's project dir name: cwd with every non-alphanumeric char
    # replaced by '-' (e.g. /home/user/code/x -> -home-user-code-x).
    proj = "".join(c if (c.isascii() and c.isalnum()) else "-"
                   for c in cwd) or "-unknown"
    dest = config.CLAUDE_PROJECTS / proj / f"{sid}.jsonl"
    if dest.exists():
        raise RawError(f"refusing to overwrite existing transcript: {dest}")
    dest.parent.mkdir(parents=True, exist_ok=True)
    dest.write_bytes(data)
    print(f"restored transcript: {dest}")
    print(f"resume with: cd {cwd or '<project dir>'} && "
          f"claude --resume {sid}")
    return dest


def iter_transcripts(all_machines: bool = False):
    """Yield (label, text) pairs for grep: local transcripts, and — with
    all_machines — decompressed foreign segment streams from vault-raw."""
    cutoff = time.time() - LOCAL_MTIME_DAYS * 86400
    for root in (config.CLAUDE_PROJECTS, config.CODEX_SESSIONS):
        if not root.is_dir():
            continue
        for p in sorted(root.rglob("*.jsonl")):
            try:
                if p.stat().st_mtime < cutoff:
                    continue
                yield str(p), p.read_text(errors="replace")
            except OSError:
                continue
    if not all_machines:
        return
    ours = config.machine_id()
    root = config.RAW_VAULT / "raw"
    if not root.is_dir():
        return
    for mdir in sorted(root.iterdir()):
        if not mdir.is_dir() or mdir.name == ours:
            continue
        for hdir in sorted(p for p in mdir.iterdir() if p.is_dir()):
            for sdir in sorted(p for p in hdir.iterdir() if p.is_dir()):
                segs = sorted(sdir.glob("*.jsonl.zst"))
                if not segs:
                    continue
                try:
                    data = b"".join(_zstd_d(s) for s in segs)
                except RawError as e:
                    util.log(config.TICK_LOG, f"raw grep: {e}")
                    continue
                label = f"{mdir.name}:{hdir.name}:{sdir.name}"
                yield label, data.decode("utf-8", errors="replace")


# ── CLI ─────────────────────────────────────────────────────────────────────

def _status() -> int:
    vault = config.RAW_VAULT
    machine = config.machine_id()
    root = vault / "raw"
    total = 0
    if root.is_dir():
        for mdir in sorted(p for p in root.iterdir() if p.is_dir()):
            for hdir in sorted(p for p in mdir.iterdir() if p.is_dir()):
                sdirs = [p for p in hdir.iterdir() if p.is_dir()]
                nbytes = sum(f.stat().st_size for s in sdirs
                             for f in s.glob("*.jsonl.zst"))
                print(f"{mdir.name}/{hdir.name}: {len(sdirs)} session(s), "
                      f"{nbytes / 1048576:.1f} MiB compressed")
                total += len(sdirs)
    if not total:
        print(f"raw vault empty ({root})")
    try:
        conn = db.connect()
        db.ensure_hive_schema(conn)
    except Exception:
        return 0
    pending = 0
    try:
        for s in conn.execute(
                "SELECT transcript_path, raw_synced_through FROM "
                "hive_sessions WHERE machine=? AND transcript_path != ''",
                (machine,)):
            try:
                size = Path(s["transcript_path"]).stat().st_size
            except OSError:
                continue
            if size > s["raw_synced_through"]:
                pending += size - s["raw_synced_through"]
    finally:
        conn.close()
    print(f"unsynced local transcript bytes: {pending}")
    return 0


def _prune() -> int:
    """List scratch-class prune candidates. Dry-run only in v1 — no deletion."""
    months = int(config.load()["raw"]["scratch_prune_months"])
    cutoff = db.now() - months * 30 * 86400
    vault = config.RAW_VAULT
    conn = db.connect()
    db.ensure_hive_schema(conn)
    n = 0
    try:
        for s in conn.execute(
                "SELECT harness, session_id, machine, ended_at FROM "
                "hive_sessions WHERE stream_slug='' "
                "AND ended_at IS NOT NULL AND ended_at < ?", (cutoff,)):
            d = vault / "raw" / s["machine"] / s["harness"] / s["session_id"]
            if d.is_dir():
                ended = time.strftime("%Y-%m-%d", time.gmtime(s["ended_at"]))
                print(f"would prune {d} (scratch, ended {ended})")
                n += 1
    finally:
        conn.close()
    print(f"{n} scratch-class candidate(s) older than {months} months — "
          f"dry-run only in v1, nothing deleted")
    return 0


USAGE = """\
hive raw — raw transcript segments (vault-raw)

  hive raw import <session-id> [--harness claude-code|codex]
                          reassemble a synced transcript for --resume here
  hive raw status         per-machine segment inventory + local sync lag
  hive raw prune [--dry-run]
                          list scratch-class prune candidates (v1: list only)
"""


def run(argv: list[str]) -> int:
    if not argv or argv[0] in ("-h", "--help", "help"):
        print(USAGE)
        return 0 if argv else 2
    verb, rest = argv[0], argv[1:]
    if verb == "import":
        args = [a for a in rest if not a.startswith("--")]
        if not args:
            util.eprint("usage: hive raw import <session-id> [--harness H]")
            return 2
        harness = "claude-code"
        if "--harness" in rest:
            i = rest.index("--harness")
            if i + 1 >= len(rest):
                util.eprint("hive raw: --harness needs a value")
                return 2
            harness = rest[i + 1]
        try:
            import_session(args[0], harness)
        except RawError as e:
            util.eprint(f"hive raw: {e}")
            return 1
        return 0
    if verb == "status":
        return _status()
    if verb == "prune":
        return _prune()
    util.eprint(f"hive raw: unknown subcommand '{verb}' (try `hive raw help`)")
    return 2
