"""Single-flight, batched, backed-off pusher for vault-raw.

`python3 -m hive.raw_push` — spawned detached by raw.request(); never run
per tick. The tick only marks "push wanted" (raw.push.wanted) and starts a
pusher when none is alive, the last success is older than live_debounce_s,
and no failure backoff is pending. At most one pusher runs per vault: an
O_EXCL PID lock file whose owner is checked for liveness, so a crashed
pusher's lock is reclaimed instead of wedging sync forever.

The backlog is pushed in bounded batches: unpushed commits are walked
oldest-first and an intermediate commit is pushed (`<sha>:refs/heads/main`)
whenever the accumulated new-blob bytes would exceed BATCH_BYTES, so no
single pack is large enough to time out or hit GitHub's push limits. A
commit that can never push as-is (a blob over GitHub's 100 MiB file limit,
or one commit bigger than a batch) triggers a one-time local rebatch: the
unpushed commits — hive's own, never on the remote — are replaced by
bounded commits of the same tree (oversized segments re-split), with the
old head kept at refs/hive/pre-rebatch/<ts>.

Every git call runs in its own process group and the whole group is killed
on timeout: a timed-out `git push` otherwise leaves its ssh/pack-objects
children uploading in the background, and the next tick starts another.

After a failure the next attempt waits BACKOFF_BASE_S * 2^(n-1), capped at
BACKOFF_MAX_S, recorded with the reason in raw.push.state.json.
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

from . import config, util

BATCH_BYTES = 200 * 1024 * 1024
GITHUB_BLOB_LIMIT = 100 * 1024 * 1024
SPLIT_BYTES = 95 * 1024 * 1024   # re-split segments above this on rebatch
SEGMENT_MAX_BYTES = 32 * 1024 * 1024  # raw bytes per segment (raw.py too)
BACKOFF_BASE_S = 300
BACKOFF_MAX_S = 6 * 3600
MIN_RATE_BPS = 256 * 1024        # push timeout = 120s + bytes / this
MAX_ROUNDS = 4                   # re-plan rounds per pusher run
TICK_LOCK_WAIT_S = 600
ZERO_SHA = "0" * 40


def lock_path() -> Path:
    return config.STATE_DIR / "raw.push.lock"


def wanted_path() -> Path:
    return config.STATE_DIR / "raw.push.wanted"


def state_path() -> Path:
    return config.STATE_DIR / "raw.push.state.json"


def _log(msg: str) -> None:
    util.log(config.TICK_LOG, f"raw push: {msg}")


# ── bounded subprocess (kills the whole process group on timeout) ───────────

def run_bounded(cmd: list[str], timeout: float,
                input_data: str | None = None) -> subprocess.CompletedProcess:
    env = dict(os.environ, GIT_TERMINAL_PROMPT="0")
    p = subprocess.Popen(cmd, stdin=subprocess.PIPE if input_data is not None
                         else subprocess.DEVNULL,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                         text=True, start_new_session=True, env=env)
    try:
        out, err = p.communicate(input_data, timeout=timeout)
        return subprocess.CompletedProcess(cmd, p.returncode, out, err)
    except subprocess.TimeoutExpired:
        for sig in (signal.SIGTERM, signal.SIGKILL):
            try:
                os.killpg(p.pid, sig)
            except ProcessLookupError:
                break
            try:
                p.wait(timeout=5)
                break
            except subprocess.TimeoutExpired:
                continue
        out, err = p.communicate()
        return subprocess.CompletedProcess(
            cmd, -9, out or "", f"timed out after {int(timeout)}s; "
            f"{(err or '').strip()[-200:]}")


def _git(vault: Path, *args: str, timeout: float = 60,
         input_data: str | None = None) -> subprocess.CompletedProcess:
    return run_bounded(["git", "-C", str(vault), *args], timeout, input_data)


# ── PID lock ────────────────────────────────────────────────────────────────

def _pid_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    return True


def _lock_owner(path: Path) -> int:
    try:
        return int(path.read_text().strip() or 0)
    except (OSError, ValueError):
        return 0


def lock_held(path: Path | None = None) -> bool:
    path = path or lock_path()
    return path.exists() and _pid_alive(_lock_owner(path))


def acquire_lock(path: Path | None = None) -> bool:
    """Create the lock with our PID; reclaim it if its owner is dead."""
    path = path or lock_path()
    path.parent.mkdir(parents=True, exist_ok=True)
    for _ in range(2):
        try:
            fd = os.open(path, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o644)
        except FileExistsError:
            # Serialize stale-lock reclaim so two reclaimers cannot both
            # unlink and one of them delete the other's fresh lock.
            with util.flocked(path.with_name(path.name + ".reclaim"),
                              blocking=True):
                owner = _lock_owner(path)
                if path.exists() and (_pid_alive(owner) or (
                        owner == 0 and time.time() - path.stat().st_mtime < 10)):
                    return False  # live owner (or one mid-write)
                _log(f"reclaiming stale lock (pid {owner} is gone)")
                try:
                    path.unlink()
                except FileNotFoundError:
                    pass
            continue
        with os.fdopen(fd, "w") as f:
            f.write(f"{os.getpid()}\n")
        return True
    return False


def release_lock(path: Path | None = None) -> None:
    path = path or lock_path()
    if _lock_owner(path) == os.getpid():
        try:
            path.unlink()
        except FileNotFoundError:
            pass


# ── backoff state ───────────────────────────────────────────────────────────

def load_state() -> dict:
    try:
        data = json.loads(state_path().read_text())
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _save_state(state: dict) -> None:
    p = state_path()
    p.parent.mkdir(parents=True, exist_ok=True)
    tmp = p.with_name(p.name + ".tmp")
    tmp.write_text(json.dumps(state, indent=1, sort_keys=True) + "\n")
    tmp.replace(p)


def backoff_delay(failures: int) -> int:
    if failures <= 0:
        return 0
    return min(BACKOFF_BASE_S * 2 ** min(failures - 1, 30), BACKOFF_MAX_S)


def record_failure(reason: str, now: float | None = None) -> dict:
    now = time.time() if now is None else now
    state = load_state()
    n = int(state.get("failures", 0)) + 1
    delay = backoff_delay(n)
    state.update(failures=n, last_error=reason[:500], last_failure_at=now,
                 next_attempt_at=now + delay)
    _save_state(state)
    _log(f"FAILED ({n} in a row), next attempt in {delay}s: {reason[:300]}")
    return state


def record_success(now: float | None = None) -> None:
    now = time.time() if now is None else now
    state = load_state()
    state.update(failures=0, last_error="", next_attempt_at=0, last_ok_at=now)
    _save_state(state)


def in_backoff(now: float | None = None) -> bool:
    now = time.time() if now is None else now
    return float(load_state().get("next_attempt_at", 0) or 0) > now


# ── request (called by the tick; never pushes itself) ───────────────────────

def _spawn_pusher(vault: Path) -> None:
    plugin_lib = str(Path(__file__).resolve().parent.parent)
    util.spawn_detached(
        [sys.executable, "-m", "hive.raw_push", "--vault", str(vault)],
        logfile=config.TICK_LOG,
        env_extra={"PYTHONPATH": plugin_lib,
                   "HIVE_STATE_DIR": str(config.STATE_DIR),
                   "HIVE_RAW_VAULT": str(vault),
                   config.NO_CAPTURE_ENV: "1"},
    )


def request(vault: Path, now: float | None = None) -> bool:
    """Mark a push wanted; start one background pusher if due. True = spawned."""
    now = time.time() if now is None else now
    try:
        wanted_path().parent.mkdir(parents=True, exist_ok=True)
        wanted_path().write_text(f"{now}\n")
    except OSError:
        pass
    if lock_held():
        return False  # the live pusher re-checks "wanted" before exiting
    state = load_state()
    if float(state.get("next_attempt_at", 0) or 0) > now:
        return False
    interval = int(config.load()["sync"]["live_debounce_s"])
    if now - float(state.get("last_ok_at", 0) or 0) < interval:
        return False
    _spawn_pusher(vault)
    return True


# ── planning ────────────────────────────────────────────────────────────────

def _branch(vault: Path) -> str:
    r = _git(vault, "rev-parse", "--abbrev-ref", "HEAD", timeout=15)
    b = r.stdout.strip()
    return b if r.returncode == 0 and b and b != "HEAD" else "main"


def remote_head(vault: Path, branch: str) -> tuple[bool, str | None]:
    """(ok, sha) of origin's branch; sha None when the branch doesn't exist."""
    r = _git(vault, "ls-remote", "origin", f"refs/heads/{branch}", timeout=60)
    if r.returncode != 0:
        return False, None
    line = r.stdout.strip().splitlines()
    return True, (line[0].split()[0] if line else None)


def _has_commit(vault: Path, sha: str) -> bool:
    return _git(vault, "cat-file", "-e", f"{sha}^{{commit}}",
                timeout=15).returncode == 0


def _is_ancestor(vault: Path, a: str, b: str) -> bool:
    return _git(vault, "merge-base", "--is-ancestor", a, b,
                timeout=30).returncode == 0


def plan(vault: Path, remote: str | None, head: str = "HEAD"
         ) -> list[tuple[str, int, int]]:
    """Unpushed commits oldest-first as (sha, new_blob_bytes, max_blob)."""
    rng = [head] if remote is None else [f"{remote}..{head}"]
    r = _git(vault, "log", "--reverse", "--raw", "--no-abbrev", "--no-renames",
             "--format=commit %H", *rng, timeout=300)
    if r.returncode != 0:
        raise RuntimeError(f"git log failed: {r.stderr.strip()[:200]}")
    commits: list[tuple[str, list[str]]] = []
    for line in r.stdout.splitlines():
        if line.startswith("commit "):
            commits.append((line.split()[1], []))
        elif line.startswith(":") and commits:
            fields = line.split("\t", 1)[0].split()
            if len(fields) >= 4 and fields[3] != ZERO_SHA:
                commits[-1][1].append(fields[3])
    blobs = sorted({b for _, bs in commits for b in bs})
    sizes: dict[str, int] = {}
    if blobs:
        c = _git(vault, "cat-file", "--batch-check=%(objectname) %(objectsize)",
                 timeout=300, input_data="\n".join(blobs) + "\n")
        for line in c.stdout.splitlines():
            parts = line.split()
            if len(parts) == 2 and parts[1].isdigit():
                sizes[parts[0]] = int(parts[1])
    out = []
    for sha, bs in commits:
        s = [sizes.get(b, 0) for b in bs]
        out.append((sha, sum(s), max(s, default=0)))
    return out


def batches(commits: list[tuple[str, int, int]],
            limit: int | None = None) -> list[tuple[str, int]]:
    """Greedy cut points: (sha to push, bytes in that batch)."""
    limit = BATCH_BYTES if limit is None else limit
    out: list[tuple[str, int]] = []
    acc, last = 0, None
    for sha, nbytes, _ in commits:
        if last is not None and acc + nbytes > limit:
            out.append((last, acc))
            acc = 0
        acc += nbytes
        last = sha
    if last is not None:
        out.append((last, acc))
    return out


def needs_rebatch(commits: list[tuple[str, int, int]]) -> str:
    for sha, nbytes, biggest in commits:
        if biggest > GITHUB_BLOB_LIMIT:
            return (f"commit {sha[:9]} has a {biggest / 1048576:.0f} MiB file "
                    f"(GitHub rejects files over 100 MiB)")
        if nbytes > BATCH_BYTES:
            return (f"commit {sha[:9]} adds {nbytes / 1048576:.0f} MiB "
                    f"(over the {BATCH_BYTES // 1048576} MiB batch size)")
    return ""


# ── rebatch (local-only history rewrite, under the tick lock) ───────────────

def _zstd_c(data: bytes) -> bytes:
    p = subprocess.run(["zstd", "-q", "-c"], input=data, capture_output=True,
                       timeout=300)
    if p.returncode != 0:
        raise RuntimeError(f"zstd compress failed: {p.stderr[:200]!r}")
    return p.stdout


def resplit_session_dir(d: Path, chunk: int | None = None) -> int:
    """Rewrite a session's segments so none holds more than `chunk` raw
    bytes. Byte-identical reassembly; returns the new segment count."""
    chunk = SEGMENT_MAX_BYTES if chunk is None else chunk
    segs = sorted(d.glob("*.jsonl.zst"))
    mpath = d / "manifest.json"
    try:
        manifest = json.loads(mpath.read_text())
        if not isinstance(manifest, dict):
            manifest = {}
    except (OSError, ValueError):
        manifest = {}
    old = {int(s.get("seq", -1)): s for s in manifest.get("segments") or []
           if isinstance(s, dict)}
    tmp = d / ".resplit"
    if tmp.exists():
        for f in tmp.iterdir():
            f.unlink()
    tmp.mkdir()
    new_segments = []
    seq, pos = 0, None
    for i, seg in enumerate(segs):
        meta = old.get(i, {})
        start = int(meta.get("start", pos if pos is not None else 0))
        p = subprocess.Popen(["zstd", "-q", "-d", "-c", str(seg)],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        try:
            while True:
                buf = p.stdout.read(chunk)
                if not buf:
                    break
                (tmp / f"{seq:06d}.jsonl.zst").write_bytes(_zstd_c(buf))
                new_segments.append({"seq": seq, "start": start,
                                     "end": start + len(buf)})
                start += len(buf)
                seq += 1
        finally:
            p.stdout.close()
            err = p.stderr.read()
            p.stderr.close()
            rc = p.wait()
        if rc != 0:
            raise RuntimeError(f"zstd decompress failed for {seg}: {err[:200]!r}")
        pos = start
    for seg in segs:
        seg.unlink()
    for f in sorted(tmp.iterdir()):
        f.replace(d / f.name)
    tmp.rmdir()
    if manifest or segs:
        manifest["segments"] = new_segments
        mpath.write_text(json.dumps(manifest, indent=1) + "\n")
    return seq


def rebatch(vault: Path, remote: str | None, machine: str) -> int:
    """Replace unpushed commits with bounded ones. Caller holds the tick lock.
    Returns the number of commits written."""
    head = _git(vault, "rev-parse", "HEAD", timeout=15).stdout.strip()
    rng = [remote, head] if remote else [
        _git(vault, "hash-object", "-t", "tree", "/dev/null",
             timeout=15).stdout.strip(), head]
    r = _git(vault, "diff", "--name-only", "--no-renames", "-z", *rng,
             timeout=300)
    if r.returncode != 0:
        raise RuntimeError(f"git diff failed: {r.stderr.strip()[:200]}")
    paths = [p for p in r.stdout.split("\0") if p]

    # Re-split any session dir holding a segment GitHub would reject.
    dirs = sorted({(vault / p).parent for p in paths
                   if p.endswith(".jsonl.zst") and (vault / p).is_file()
                   and (vault / p).stat().st_size > SPLIT_BYTES})
    for d in dirs:
        rel_d = d.relative_to(vault).as_posix()
        # Old names (tracked ones included) so their deletion gets staged.
        tracked = _git(vault, "ls-files", "-z", "--", rel_d, timeout=60)
        paths.extend(p for p in tracked.stdout.split("\0") if p)
        n = resplit_session_dir(d)
        _log(f"rebatch: re-split {rel_d} into {n} segment(s)")
        paths.extend(f.relative_to(vault).as_posix() for f in d.iterdir())

    stamp = time.strftime("%Y%m%dT%H%M%S")
    _git(vault, "update-ref", f"refs/hive/pre-rebatch/{stamp}", head)
    if remote:
        reset = _git(vault, "reset", "-q", "--mixed", remote, timeout=300)
    else:
        branch = _branch(vault)
        reset = _git(vault, "update-ref", "-d", f"refs/heads/{branch}")
        if reset.returncode == 0:
            reset = _git(vault, "read-tree", "--empty")
    if reset.returncode != 0:
        raise RuntimeError(f"rebatch reset failed: {reset.stderr.strip()[:200]}")

    def size(p: str) -> int:
        try:
            return (vault / p).stat().st_size if (vault / p).is_file() else 0
        except OSError:
            return 0

    groups: list[list[str]] = []
    acc = 0
    for p in sorted(set(paths)):
        s = size(p)
        if groups and acc + s <= BATCH_BYTES:
            groups[-1].append(p)
            acc += s
        else:
            groups.append([p])
            acc = s
    written = 0
    for i, group in enumerate(groups, 1):
        a = _git(vault, "add", "-A", "--pathspec-from-file=-",
                 "--pathspec-file-nul", timeout=600,
                 input_data="\0".join(group) + "\0")
        if a.returncode != 0:
            raise RuntimeError(f"rebatch add failed: {a.stderr.strip()[:200]}")
        if _git(vault, "diff", "--cached", "--quiet").returncode == 0:
            continue
        c = _git(vault, "commit", "-q", "-m",
                 f"hive raw batch {i}/{len(groups)} "
                 f"{time.strftime('%Y-%m-%dT%H:%M:%S')} {machine}", timeout=300)
        if c.returncode != 0:
            raise RuntimeError(f"rebatch commit failed: {c.stderr.strip()[:200]}")
        written += 1
    _log(f"rebatch: {written} bounded commit(s) replace the unpushed history "
         f"(old head kept at refs/hive/pre-rebatch/{stamp})")
    return written


def _with_tick_lock(fn, wait_s: float = TICK_LOCK_WAIT_S):
    deadline = time.time() + wait_s
    while True:
        with util.flocked(config.TICK_LOCK) as got:
            if got:
                return fn()
        if time.time() >= deadline:
            raise RuntimeError("tick lock busy; could not rewrite local history")
        time.sleep(5)


# ── push ────────────────────────────────────────────────────────────────────

def push_backlog(vault: Path) -> tuple[bool, str]:
    """Push every unpushed commit in bounded batches. (ok, reason)."""
    branch = _branch(vault)
    rebatched = False
    for _ in range(MAX_ROUNDS):
        ok, remote = remote_head(vault, branch)
        if not ok:
            return False, "cannot reach origin (ls-remote failed)"
        if remote and not (_has_commit(vault, remote)
                           and _is_ancestor(vault, remote, "HEAD")):
            # Remote moved (another writer): fetch + rebase our local-only
            # commits on top, under the tick lock so no tick commits mid-way.
            f = _git(vault, "fetch", "--quiet", "origin", branch, timeout=1800)
            if f.returncode != 0:
                return False, f"fetch failed: {f.stderr.strip()[:200]}"
            r = _with_tick_lock(lambda: _git(
                vault, "rebase", "--autostash", "FETCH_HEAD", timeout=600))
            if r.returncode != 0:
                _git(vault, "rebase", "--abort")
                return False, f"rebase onto origin failed: {r.stderr.strip()[:200]}"
            continue
        head = _git(vault, "rev-parse", "HEAD", timeout=15).stdout.strip()
        if remote == head:
            return True, "up to date"
        commits = plan(vault, remote, head)
        why = needs_rebatch(commits)
        if why:
            if rebatched:
                return False, f"still unpushable after rebatch: {why}"
            _log(f"rebatching unpushed history: {why}")
            _with_tick_lock(lambda: rebatch(vault, remote, config.machine_id()))
            rebatched = True
            continue
        cuts = batches(commits)
        total = sum(n for _, n in cuts)
        _log(f"{len(commits)} commit(s), {total / 1048576:.1f} MiB "
             f"in {len(cuts)} batch(es)")
        for i, (sha, nbytes) in enumerate(cuts, 1):
            p = _git(vault, "push", "--quiet", "origin",
                     f"{sha}:refs/heads/{branch}",
                     timeout=120 + nbytes / MIN_RATE_BPS)
            if p.returncode != 0:
                return False, (f"batch {i}/{len(cuts)} ({sha[:9]}, "
                               f"{nbytes / 1048576:.1f} MiB): "
                               f"{p.stderr.strip()[-300:]}")
            _log(f"batch {i}/{len(cuts)} ok ({nbytes / 1048576:.1f} MiB)")
        # Loop: the tick may have committed more while we pushed.
    return True, "pushed (more may remain for the next run)"


def run_pusher(vault: Path, force: bool = False) -> int:
    if not acquire_lock():
        _log("another pusher is running; exiting")
        return 0
    try:
        if not force and in_backoff():
            return 0
        for _ in range(MAX_ROUNDS):
            try:
                seen = wanted_path().stat().st_mtime
            except OSError:
                seen = 0.0
            try:
                ok, reason = push_backlog(vault)
            except Exception as e:  # never leave a pusher crash unrecorded
                ok, reason = False, f"crashed: {e!r}"
            if not ok:
                record_failure(reason)
                return 1
            record_success()
            try:
                again = wanted_path().stat().st_mtime > seen
            except OSError:
                again = False
            if not again:
                try:
                    wanted_path().unlink()
                except OSError:
                    pass
                break
        return 0
    finally:
        release_lock()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="hive.raw_push")
    parser.add_argument("--vault", default=str(config.RAW_VAULT))
    parser.add_argument("--force", action="store_true",
                        help="ignore the failure backoff")
    args = parser.parse_args(sys.argv[1:] if argv is None else argv)
    try:
        os.nice(10)
    except OSError:
        pass
    return run_pusher(Path(args.vault), force=args.force)


if __name__ == "__main__":
    sys.exit(main())
