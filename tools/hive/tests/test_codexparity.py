"""hive.codexparity tests.

The real ~/.codex/AGENTS.md is never touched: AGENTS_MD is monkeypatched to
a tmp path in every test, and digest() runs against a recorded fake of
vendored.mem (no subprocesses).
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO_ROOT / "plugins" / "hive" / "lib"))

from hive import codexparity  # noqa: E402


@pytest.fixture
def agents_md(tmp_path, monkeypatch):
    p = tmp_path / "codex" / "AGENTS.md"
    monkeypatch.setattr(codexparity, "AGENTS_MD", p)
    return p


SURROUNDING = (
    "# My Codex rules\n\nAlways run the linter.\n\n"
    f"{codexparity.BEGIN}\nstale hive text\n{codexparity.END}\n\n"
    "## After\n\nKeep this tail intact.\n"
)


# ── ensure_agents_md ────────────────────────────────────────────────────────

def test_create_when_missing(agents_md):
    assert not agents_md.exists()
    assert codexparity.ensure_agents_md() is True
    text = agents_md.read_text()
    assert text.startswith(codexparity.BEGIN)
    assert codexparity.END in text
    assert "hive ask" in text and "hive grep" in text and "hive brief" in text


def test_append_when_no_markers(agents_md):
    agents_md.parent.mkdir(parents=True)
    agents_md.write_text("# Existing instructions\n\nDo the thing.\n")
    assert codexparity.ensure_agents_md() is True
    text = agents_md.read_text()
    assert text.startswith("# Existing instructions\n")
    assert "Do the thing." in text
    assert text.index(codexparity.BEGIN) > text.index("Do the thing.")


def test_replace_stale_block_only(agents_md):
    agents_md.parent.mkdir(parents=True)
    agents_md.write_text(SURROUNDING)
    assert codexparity.ensure_agents_md() is True
    text = agents_md.read_text()
    assert "stale hive text" not in text
    assert "hive ask" in text
    # Everything outside the markers survives byte-for-byte.
    assert text.startswith("# My Codex rules\n\nAlways run the linter.\n\n")
    assert text.endswith("\n\n## After\n\nKeep this tail intact.\n")
    assert text.count(codexparity.BEGIN) == 1
    assert text.count(codexparity.END) == 1


def test_noop_when_current_and_idempotent(agents_md):
    assert codexparity.ensure_agents_md() is True
    first = agents_md.read_text()
    assert codexparity.ensure_agents_md() is False
    assert agents_md.read_text() == first


def test_malformed_markers_leave_file_alone(agents_md, capsys):
    bad = f"before\n{codexparity.END}\nmiddle\n{codexparity.BEGIN}\nafter\n"
    agents_md.parent.mkdir(parents=True)
    agents_md.write_text(bad)
    assert codexparity.ensure_agents_md() is False
    assert agents_md.read_text() == bad
    assert "malformed" in capsys.readouterr().err


def test_dry_run_never_writes(agents_md, capsys):
    # missing file: reports a change, creates nothing
    assert codexparity.ensure_agents_md(dry_run=True) is True
    assert not agents_md.exists()
    assert "dry-run" in capsys.readouterr().out
    # stale block: reports a change, file untouched
    agents_md.parent.mkdir(parents=True)
    agents_md.write_text(SURROUNDING)
    assert codexparity.ensure_agents_md(dry_run=True) is True
    assert agents_md.read_text() == SURROUNDING
    # current block: no-op under dry_run too
    codexparity.ensure_agents_md()
    written = agents_md.read_text()
    assert codexparity.ensure_agents_md(dry_run=True) is False
    assert agents_md.read_text() == written


# ── digest ──────────────────────────────────────────────────────────────────

class MemRecorder:
    def __init__(self, stdout="", rc=0, exc=None):
        self.stdout, self.rc, self.exc = stdout, rc, exc
        self.calls: list[list[str]] = []

    def __call__(self, args, timeout=60, capture=True):
        self.calls.append(list(args))
        if self.exc:
            raise self.exc
        return subprocess.CompletedProcess(args, self.rc,
                                           stdout=self.stdout, stderr="")


def test_digest_passes_cwd_through(monkeypatch):
    rec = MemRecorder(stdout="## memory digest\n- a thing\n")
    monkeypatch.setattr(codexparity.vendored, "mem", rec)
    out = codexparity.digest("/home/x/code/proj")
    assert out == "## memory digest\n- a thing\n"
    assert rec.calls == [["explain-inject", "--cwd", "/home/x/code/proj"]]


def test_digest_empty_on_failure(monkeypatch):
    monkeypatch.setattr(codexparity.vendored, "mem", MemRecorder(rc=1))
    assert codexparity.digest("/tmp/x") == ""
    monkeypatch.setattr(codexparity.vendored, "mem",
                        MemRecorder(exc=OSError("no binary")))
    assert codexparity.digest("/tmp/x") == ""
