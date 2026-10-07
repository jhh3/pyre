"""hive.wt tests: pure naming/recipe logic plus mocked-subprocess verb flows.

No real git or tmux runs here — every external command goes through wt._sh,
which these tests replace with a recorder.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "plugins" / "hive" / "lib"))

from hive import wt  # noqa: E402


class ShRecorder:
    """Stands in for wt._sh: records every command, returns canned rcs."""

    def __init__(self, rc_for: dict[tuple, int] | None = None,
                 stdout_for: dict[tuple, str] | None = None):
        self.calls: list[list[str]] = []
        self.rc_for = rc_for or {}
        self.stdout_for = stdout_for or {}

    def __call__(self, cmd, **kw):
        self.calls.append(list(cmd))
        rc, out = 0, ""
        for prefix, code in self.rc_for.items():
            if tuple(cmd[:len(prefix)]) == prefix:
                rc = code
        for prefix, text in self.stdout_for.items():
            if tuple(cmd[:len(prefix)]) == prefix:
                out = text
        return subprocess.CompletedProcess(cmd, rc, stdout=out, stderr="")

    def ran(self, *words) -> bool:
        return any(all(w in c for w in words) for c in self.calls)


class NotATty:
    def isatty(self) -> bool:
        return False


# -- intake parsing ----------------------------------------------------------

def test_linear_url_to_branch(monkeypatch):
    monkeypatch.setenv("WT_BRANCH_PREFIX", "alice")
    url = "https://linear.app/acme/issue/ENG-1234/fix-the-thing"
    assert wt.is_linear(url)
    assert wt.parse_linear(url) == "alice/eng-1234"


def test_linear_url_without_slug_tail(monkeypatch):
    monkeypatch.setenv("WT_BRANCH_PREFIX", "alice")
    assert wt.parse_linear("https://linear.app/t/issue/ENG-7") == "alice/eng-7"


def test_plain_branch_is_not_linear():
    assert not wt.is_linear("alice/eng-1234")
    assert not wt.is_linear("https://linear.app/acme/project/foo")


def test_unparseable_linear_url_raises():
    with pytest.raises(wt.WtError):
        wt.parse_linear("https://linear.app/t/issue/")


def test_dir_name_dashes_slashes():
    assert wt.dir_name("alice/eng-1234") == "alice-eng-1234"
    assert wt.dir_name("plain") == "plain"


def test_session_name_sanitizes_tmux_specials():
    assert wt.session_name("feat-v1.2:hot") == "feat-v1-2-hot"
    assert wt.session_name("alice-eng-1234") == "alice-eng-1234"


# -- recipes -----------------------------------------------------------------

def test_setup_recipe_wins_over_fallback(tmp_path):
    (tmp_path / "Makefile").write_text("setup:\n")
    cmds = wt.setup_commands({"setup": ["uv sync", "make db"]}, tmp_path)
    assert cmds == ["uv sync", "make db"]


def test_setup_makefile_fallback(tmp_path):
    (tmp_path / "Makefile").write_text("setup:\n")
    assert wt.setup_commands({}, tmp_path) == ["make setup"]
    (tmp_path / "backend").mkdir()
    (tmp_path / "backend" / "Makefile").write_text("setup:\n")
    assert wt.setup_commands({}, tmp_path) == ["make setup",
                                               "make -C backend setup"]


def test_setup_nothing_when_no_makefile(tmp_path):
    assert wt.setup_commands({}, tmp_path) == []


def test_default_repo_from_recipe_flag(tmp_path, monkeypatch):
    monkeypatch.setattr(wt.config, "repos", lambda: {
        "other": {"path": str(tmp_path / "o")},
        "webapp": {"path": str(tmp_path / "s"), "default": True},
    })
    name, root, recipe = wt.resolve_repo(None)
    assert name == "webapp"
    assert root == tmp_path / "s"
    assert recipe.get("default") is True


def test_repo_arg_accepts_path_with_git(tmp_path, monkeypatch):
    monkeypatch.setattr(wt.config, "repos", lambda: {})
    root = tmp_path / "myrepo"
    (root / ".git").mkdir(parents=True)
    name, path, recipe = wt.resolve_repo(str(root))
    assert (name, path, recipe) == ("myrepo", root, {})


def test_repo_arg_unknown_name_raises(monkeypatch):
    monkeypatch.setattr(wt.config, "repos", lambda: {})
    with pytest.raises(wt.WtError):
        wt.resolve_repo("nope")


# -- worktree listing --------------------------------------------------------

def test_worktrees_parses_porcelain_and_skips_main(tmp_path, monkeypatch):
    root = tmp_path / "repo"
    porcelain = (
        f"worktree {root}\nHEAD aaa\nbranch refs/heads/main\n\n"
        f"worktree {root}/.worktrees/alice-eng-1\nHEAD bbb\n"
        f"branch refs/heads/alice/eng-1\n"
    )
    monkeypatch.setattr(wt, "_sh", ShRecorder(
        stdout_for={("git",): porcelain}))
    assert wt.worktrees(root) == [
        ("alice/eng-1", root / ".worktrees" / "alice-eng-1")]


# -- verb flows (mocked) -----------------------------------------------------

@pytest.fixture
def repo(tmp_path, monkeypatch):
    """A fake repo with one existing worktree; hive side effects stubbed."""
    root = tmp_path / "repo"
    (root / ".git").mkdir(parents=True)
    (root / ".worktrees" / "feat-x").mkdir(parents=True)
    monkeypatch.setattr(wt.config, "repos", lambda: {})
    monkeypatch.setattr(wt.config, "STREAMS_VAULT", tmp_path / "streams")
    monkeypatch.setattr(wt.vendored, "ws",
                        lambda *a, **k: subprocess.CompletedProcess([], 0, "", ""))
    return root


def test_open_print_path_prints_only_the_path(repo, monkeypatch, capsys):
    sh = ShRecorder(rc_for={("tmux", "has-session"): 1})
    monkeypatch.setattr(wt, "_sh", sh)
    rc = wt.run(["open", "feat/x", "--repo", str(repo), "--print-path"])
    assert rc == 0
    out = capsys.readouterr().out
    assert out == str(repo / ".worktrees" / "feat-x") + "\n"
    assert not sh.ran("fetch")  # worktree existed; nothing created


def test_open_registers_stream_when_missing(repo, monkeypatch):
    registered = []
    monkeypatch.setattr(wt.vendored, "ws",
                        lambda args, **k: registered.append(list(args)))
    monkeypatch.setattr(wt, "_sh", ShRecorder())
    assert wt.run(["open", "feat/x", "--repo", str(repo),
                   "--print-path"]) == 0
    assert ["register", "feat-x", "--title", "feat/x"] in registered


def test_rm_refuses_branch_delete_without_yes(repo, monkeypatch):
    sh = ShRecorder(rc_for={("tmux", "has-session"): 1})
    monkeypatch.setattr(wt, "_sh", sh)
    monkeypatch.setattr(wt.sys, "stdin", NotATty())
    rc = wt.run(["rm", "feat/x", "--repo", str(repo)])
    assert rc == 0
    assert sh.ran("worktree", "remove", "--force")
    assert not sh.ran("branch", "-D")


def test_rm_with_yes_deletes_branch(repo, monkeypatch):
    sh = ShRecorder(rc_for={("tmux", "has-session"): 1})
    monkeypatch.setattr(wt, "_sh", sh)
    monkeypatch.setattr(wt.sys, "stdin", NotATty())
    rc = wt.run(["rm", "feat/x", "--repo", str(repo), "--yes"])
    assert rc == 0
    assert sh.ran("show-ref", "refs/heads/feat/x")
    assert sh.ran("branch", "-D", "feat/x")


def test_rm_interactive_decline_keeps_branch(repo, monkeypatch):
    sh = ShRecorder(rc_for={("tmux", "has-session"): 1})
    monkeypatch.setattr(wt, "_sh", sh)

    class Tty:
        def isatty(self):
            return True

    monkeypatch.setattr(wt.sys, "stdin", Tty())
    monkeypatch.setattr("builtins.input", lambda prompt="": "n")
    assert wt.run(["rm", "feat/x", "--repo", str(repo)]) == 0
    assert not sh.ran("branch", "-D")


def test_rm_kills_live_tmux_session(repo, monkeypatch):
    sh = ShRecorder()  # has-session rc 0 -> session exists
    monkeypatch.setattr(wt, "_sh", sh)
    monkeypatch.setattr(wt.sys, "stdin", NotATty())
    assert wt.run(["rm", "feat/x", "--repo", str(repo)]) == 0
    assert sh.ran("kill-session", "feat-x")
