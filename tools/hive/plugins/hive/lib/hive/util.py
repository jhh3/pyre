"""Small shared utilities: ULIDs, locks, logging, subprocess helpers.

Import-light; used on hook paths.
"""

from __future__ import annotations

import fcntl
import os
import secrets
import subprocess
import sys
import time
from contextlib import contextmanager
from pathlib import Path

from . import config

_ULID_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"  # Crockford base32


def ulid(ts: float | None = None) -> str:
    """26-char ULID: 48-bit ms timestamp + 80 bits randomness. Sortable."""
    ms = int((time.time() if ts is None else ts) * 1000)
    out = []
    for shift in range(45, -5, -5):
        out.append(_ULID_ALPHABET[(ms >> shift) & 0x1F])
    rand = secrets.randbits(80)
    for shift in range(75, -5, -5):
        out.append(_ULID_ALPHABET[(rand >> shift) & 0x1F])
    return "".join(out)


def new_uid() -> str:
    """Entry uid: <machine>-<ULID>. Collision-free without coordination."""
    return f"{config.machine_id()}-{ulid()}"


def log(logfile: Path, msg: str) -> None:
    """Append a timestamped line; never raises."""
    try:
        logfile.parent.mkdir(parents=True, exist_ok=True)
        stamp = time.strftime("%Y-%m-%dT%H:%M:%S")
        with open(logfile, "a") as f:
            f.write(f"{stamp} [{os.getpid()}] {msg}\n")
    except OSError:
        pass


@contextmanager
def flocked(path: Path, blocking: bool = False):
    """File lock. Yields True if acquired, False if not (non-blocking mode)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    f = open(path, "w")
    try:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | (0 if blocking else fcntl.LOCK_NB))
            acquired = True
        except BlockingIOError:
            acquired = False
        yield acquired
    finally:
        try:
            if acquired:
                fcntl.flock(f, fcntl.LOCK_UN)
        except OSError:
            pass
        f.close()


def run(cmd: list[str], timeout: int = 30, cwd: str | Path | None = None,
        check: bool = False, env_extra: dict | None = None) -> subprocess.CompletedProcess:
    env = None
    if env_extra:
        env = dict(os.environ, **env_extra)
    return subprocess.run(
        cmd, capture_output=True, text=True, timeout=timeout,
        cwd=str(cwd) if cwd else None, check=check, env=env,
    )


def spawn_detached(cmd: list[str], logfile: Path | None = None,
                   env_extra: dict | None = None) -> None:
    """Fire-and-forget: survives the parent (hook) exiting.

    Uses start_new_session so the child detaches from the hook's process
    group; stdout/err go to a logfile or /dev/null so nothing ever reaches
    the live session.
    """
    env = dict(os.environ, **(env_extra or {}))
    out = open(logfile, "a") if logfile else subprocess.DEVNULL
    try:
        subprocess.Popen(
            cmd, stdout=out, stderr=out, stdin=subprocess.DEVNULL,
            start_new_session=True, env=env,
        )
    finally:
        if logfile:
            out.close()


def read_json_line(line: str):
    import json
    try:
        return json.loads(line)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return None


def eprint(*a) -> None:
    print(*a, file=sys.stderr)
