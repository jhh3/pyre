"""Subprocess access to the vendored tools (ws, jhh3-mem) for internal use.

`hive ws` / `hive mem` exec the tools directly (cli.py); internal modules use
these helpers when they need a vendored verb's behavior without replacing the
current process. CLAUDE_PLUGIN_ROOT is remapped so the vendored scripts find
their own libs.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

PLUGIN_ROOT = Path(__file__).resolve().parent.parent.parent


def _run(tool: str, args: list[str], timeout: int = 60,
         capture: bool = True) -> subprocess.CompletedProcess:
    binpath = PLUGIN_ROOT / "vendor" / tool / "bin" / tool
    env = dict(os.environ, CLAUDE_PLUGIN_ROOT=str(PLUGIN_ROOT / "vendor" / tool))
    return subprocess.run(
        [str(binpath)] + args, env=env, timeout=timeout,
        capture_output=capture, text=True,
    )


def ws(args: list[str], timeout: int = 60, capture: bool = True):
    return _run("ws", args, timeout, capture)


def mem(args: list[str], timeout: int = 60, capture: bool = True):
    return _run("jhh3-mem", args, timeout, capture)
