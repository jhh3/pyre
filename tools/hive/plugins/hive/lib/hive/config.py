"""Paths, machine identity, and hive.toml configuration.

Everything import-light: this module is on the hook path, so no heavy imports
and no I/O beyond reading small local files.
"""

from __future__ import annotations

import functools
import json
import os
import socket
import tomllib
from pathlib import Path

HOME = Path.home()

# XDG-ish state layout
DATA_DIR = Path(os.environ.get("HIVE_DATA_DIR", HOME / ".local/share/hive"))
STATE_DIR = Path(os.environ.get("HIVE_STATE_DIR", HOME / ".local/state/hive"))
CONFIG_PATH = Path(os.environ.get("HIVE_CONFIG", HOME / ".config/hive/hive.toml"))

DB_PATH = DATA_DIR / "index.db"
HOOK_LOG = STATE_DIR / "hooks.log"
TICK_LOG = STATE_DIR / "tick.log"
TICK_LOCK = STATE_DIR / "tick.lock"
QUARANTINE_DIR = DATA_DIR / "raw" / "quarantine"
SNAPSHOT_DIR = DATA_DIR / "raw" / "snapshots"

# Vaults (working copies)
STREAMS_VAULT = Path(os.environ.get("HIVE_STREAMS_VAULT", HOME / "streams"))
RAW_VAULT = Path(os.environ.get("HIVE_RAW_VAULT", HOME / ".local/share/hive/vault-raw"))

# Harness transcript roots
CLAUDE_PROJECTS = HOME / ".claude" / "projects"
CODEX_SESSIONS = HOME / ".codex" / "sessions"

KILL_SWITCH_ENV = "HIVE_DISABLE"
NO_CAPTURE_ENV = "HIVE_NO_CAPTURE"  # set on our own claude -p calls (self-capture guard)

DEFAULTS = {
    "checkpoint": {"min_new_bytes": 8192, "min_interval_s": 600},
    "enrich": {"concurrency": 2, "job_budget_s": 120, "model": "haiku"},
    "sync": {"live_debounce_s": 900, "close_debounce_s": 45},
    "inject": {"max_bytes": 3072, "budget_s": 4},
    "tick": {"memory_max": "2G", "cpu_quota": "50%"},
    "registration": {"significant_files": 3, "significant_minutes": 45},
    "titles": {"throttle_s": 300, "max_len": 40},
    "raw": {"scratch_prune_months": 3},
}


def machine_id() -> str:
    """Short stable machine identifier used in uids, lane paths, slugs."""
    override = os.environ.get("HIVE_MACHINE")
    if override:
        return override
    mid_file = DATA_DIR / "machine-id"
    try:
        return mid_file.read_text().strip()
    except OSError:
        pass
    name = socket.gethostname().split(".")[0].lower() or "m0"
    name = "".join(c for c in name if c.isalnum() or c == "-")[:12]
    try:
        DATA_DIR.mkdir(parents=True, exist_ok=True)
        mid_file.write_text(name + "\n")
    except OSError:
        pass
    return name


@functools.lru_cache(maxsize=1)
def load() -> dict:
    """Merged config: DEFAULTS <- hive.toml. Repos section carries wt recipes."""
    cfg = json.loads(json.dumps(DEFAULTS))  # deep copy
    try:
        with open(CONFIG_PATH, "rb") as f:
            user = tomllib.load(f)
    except FileNotFoundError:
        user = {}
    except (OSError, tomllib.TOMLDecodeError):
        # A broken config must never break a hook: fall back to defaults.
        user = {}
    for key, val in user.items():
        if isinstance(val, dict) and isinstance(cfg.get(key), dict):
            cfg[key].update(val)
        else:
            cfg[key] = val
    return cfg


def repos() -> dict[str, dict]:
    """wt repo registry: {name: {path, setup: [cmds], launch: str, env: {}}}."""
    return {k: dict(v) for k, v in load().get("repos", {}).items()}


def disabled() -> bool:
    return os.environ.get(KILL_SWITCH_ENV, "") not in ("", "0")


def ensure_dirs() -> None:
    for d in (DATA_DIR, STATE_DIR, QUARANTINE_DIR, SNAPSHOT_DIR):
        d.mkdir(parents=True, exist_ok=True)
