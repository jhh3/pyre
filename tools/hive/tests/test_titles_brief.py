"""Pane-discovery backfill (titles) and morning brief tests.

All state under tmp: config paths, db.JHH3_MEM_DB, and titles.PROC_ROOT are
monkeypatched, tmux goes through a mocked util.run. Never touches the real
DB, streams vault, /proc, or tmux server.
"""

import os
import sys
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(REPO / "plugins/hive/lib"))

from hive import brief, config, db, titles  # noqa: E402


@pytest.fixture()
def tmp_env(tmp_path, monkeypatch):
    monkeypatch.setattr(config, "DATA_DIR", tmp_path / "data")
    monkeypatch.setattr(config, "STATE_DIR", tmp_path / "state")
    monkeypatch.setattr(config, "DB_PATH", tmp_path / "data/index.db")
    monkeypatch.setattr(config, "TICK_LOG", tmp_path / "state/tick.log")
    monkeypatch.setattr(config, "STREAMS_VAULT", tmp_path / "streams")
    monkeypatch.setattr(db, "JHH3_MEM_DB", tmp_path / "nonexistent-mem.db")
    monkeypatch.setattr(titles, "PROC_ROOT", str(tmp_path / "proc"))
    monkeypatch.setenv("HIVE_MACHINE", "testbox")
    monkeypatch.delenv("HIVE_DB", raising=False)
    config.load.cache_clear()
    return tmp_path


def _mkdb():
    conn = db.connect(config.DB_PATH)
    schema = (REPO / "plugins/hive/vendor/jhh3-mem/db/schema.sql").read_text()
    conn.executescript(schema)
    db.ensure_hive_schema(conn)
    return conn


def _seed_session(conn, sid, machine="testbox", tmux_session="", tmux_pane="",
                  transcript_path="", title="", stream_slug="",
                  last_activity=1, ended_at=None, harness="claude-code"):
    conn.execute(
        """INSERT INTO hive_sessions
           (session_id, harness, machine, tmux_session, tmux_pane,
            transcript_path, title, stream_slug, started_at, last_activity,
            ended_at)
           VALUES (?,?,?,?,?,?,?,?,?,?,?)""",
        (sid, harness, machine, tmux_session, tmux_pane, transcript_path,
         title, stream_slug, 1, last_activity, ended_at))


def _mkproc(root, pid, comm=None, cwd=None, children=()):
    d = root / str(pid)
    (d / "task" / str(pid)).mkdir(parents=True)
    (d / "task" / str(pid) / "children").write_text(
        " ".join(str(c) for c in children))
    if comm is not None:
        (d / "comm").write_text(comm + "\n")
    if cwd is not None:
        os.symlink(str(cwd), d / "cwd")


def _tmux_mock(monkeypatch, panes, renames=None):
    """util.run mock: list-panes returns the fixture pane list; rename-window
    (and anything else) succeeds and is recorded."""
    def fake_run(cmd, timeout=3, **kw):
        r = type("R", (), {"returncode": 0, "stdout": "", "stderr": ""})()
        if cmd[:2] == ["tmux", "list-panes"]:
            r.stdout = panes
        elif cmd[:2] == ["tmux", "rename-window"] and renames is not None:
            renames.append(cmd)
        return r
    monkeypatch.setattr(titles.util, "run", fake_run)


# ── cwd -> project-dir mapping ───────────────────────────────────────────────

def test_project_dir_mapping():
    assert titles._project_dir("/home/user/code/x") == "-home-user-code-x"
    assert titles._project_dir("/home/j.h_h/a b") == "-home-j-h-h-a-b"
    assert titles._project_dir("/srv/Repo9/wt.2") == "-srv-Repo9-wt-2"


# ── pane discovery ───────────────────────────────────────────────────────────

def test_discover_panes_backfills_newest_row(tmp_env, monkeypatch):
    workdir = tmp_env / "code" / "x"
    workdir.mkdir(parents=True)
    proc = tmp_env / "proc"
    _mkproc(proc, 100, comm="zsh", children=[101])          # pane shell
    _mkproc(proc, 101, comm="claude", cwd=workdir)          # the harness
    mapped = titles._project_dir(str(workdir))
    tp = f"/home/u/.claude/projects/{mapped}/{{}}.jsonl"

    conn = _mkdb()
    _seed_session(conn, "sid-old", transcript_path=tp.format("sid-old"),
                  last_activity=50)
    _seed_session(conn, "sid-new", transcript_path=tp.format("sid-new"),
                  last_activity=100)
    conn.commit()
    conn.close()

    _tmux_mock(monkeypatch, "%7\tmysess\t100\n")
    assert titles.discover_panes() == 1
    with db.connect(config.DB_PATH) as conn:
        new = conn.execute("SELECT * FROM hive_sessions WHERE session_id="
                           "'sid-new'").fetchone()
        old = conn.execute("SELECT * FROM hive_sessions WHERE session_id="
                           "'sid-old'").fetchone()
    assert new["tmux_session"] == "mysess" and new["tmux_pane"] == "%7"
    assert old["tmux_session"] == "" and old["tmux_pane"] == ""

    # Idempotent: the row no longer matches, nothing to update.
    assert titles.discover_panes() == 0


def test_discover_never_overwrites_nonempty(tmp_env, monkeypatch):
    workdir = tmp_env / "wt"
    workdir.mkdir()
    proc = tmp_env / "proc"
    _mkproc(proc, 200, comm="claude", cwd=workdir)  # pane pid IS the harness
    mapped = titles._project_dir(str(workdir))

    conn = _mkdb()
    _seed_session(conn, "sid-half", tmux_session="keepme",
                  transcript_path=f"/x/{mapped}/sid-half.jsonl")
    conn.commit()
    conn.close()

    _tmux_mock(monkeypatch, "%9\tothersess\t200\n")
    assert titles.discover_panes() == 1
    with db.connect(config.DB_PATH) as conn:
        row = conn.execute("SELECT * FROM hive_sessions").fetchone()
    assert row["tmux_session"] == "keepme"      # not overwritten
    assert row["tmux_pane"] == "%9"             # empty half filled


def test_discover_skips_tmux_when_no_candidates(tmp_env, monkeypatch):
    conn = _mkdb()
    _seed_session(conn, "sid-full", tmux_session="s1", tmux_pane="%1")
    conn.commit()
    conn.close()

    def boom(cmd, timeout=3, **kw):
        raise AssertionError("tmux must not be invoked with no candidates")
    monkeypatch.setattr(titles.util, "run", boom)
    assert titles.discover_panes() == 0


def test_discover_ignores_non_harness_and_missing_match(tmp_env, monkeypatch):
    workdir = tmp_env / "elsewhere"
    workdir.mkdir()
    proc = tmp_env / "proc"
    _mkproc(proc, 300, comm="vim", cwd=workdir)   # not a harness
    _mkproc(proc, 400, comm="codex", cwd=workdir)  # harness, no matching row

    conn = _mkdb()
    _seed_session(conn, "sid-x", transcript_path="/x/-some-other-dir/x.jsonl")
    conn.commit()
    conn.close()

    _tmux_mock(monkeypatch, "%1\ta\t300\n%2\tb\t400\n")
    assert titles.discover_panes() == 0


def test_discover_fail_soft_on_garbled_pane_list(tmp_env, monkeypatch):
    conn = _mkdb()
    _seed_session(conn, "sid-y")
    conn.commit()
    conn.close()
    _tmux_mock(monkeypatch, "not-tab-separated\n\n%5\tonly-two-fields\n")
    assert titles.discover_panes() == 0


def test_tick_phase_uses_discovered_pane(tmp_env, monkeypatch):
    """End to end: sweep-seeded row (no tmux) gets discovered, then renamed."""
    workdir = tmp_env / "code" / "y"
    workdir.mkdir(parents=True)
    proc = tmp_env / "proc"
    _mkproc(proc, 500, comm="bash", children=[501])
    _mkproc(proc, 501, comm="claude", cwd=workdir)
    mapped = titles._project_dir(str(workdir))

    conn = _mkdb()
    conn.execute(
        "INSERT INTO entries (uid, origin, producer, repo_key, kind, ts, title)"
        " VALUES ('sid-t', 'live', 't', 'r/k', 'session', 5, 'Build the brief')")
    _seed_session(conn, "sid-t",
                  transcript_path=f"/x/{mapped}/sid-t.jsonl")
    conn.commit()
    conn.close()

    renames = []
    _tmux_mock(monkeypatch, "%3\tws-y\t500\n", renames=renames)
    assert titles.tick_phase() == 1
    assert renames and renames[0][3] == "%3"
    assert renames[0][4] == "Build the brief"


# ── brief ────────────────────────────────────────────────────────────────────

def _write_stream(root, slug, status, updated, next_line="", garbled=False):
    d = root / "streams" / slug
    d.mkdir(parents=True)
    if garbled:
        (d / "STREAM.md").write_bytes(b"\xff\xfenot: [frontmatter\n---")
        return
    (d / "STREAM.md").write_text(
        "---\n"
        f"title: {slug}\n"
        f"status: {status}\n"
        "kind: agent\n"
        f"updated: {updated}\n"
        f"next: {next_line}\n"
        "---\n\nbody\n")


def _iso(ts):
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(ts))


@pytest.fixture()
def brief_world(tmp_env):
    now = db.now()
    conn = _mkdb()
    _seed_session(conn, "aaaa1111", title="fix importer",
                  stream_slug="hive-core", last_activity=now - 720)
    _seed_session(conn, "bbbb2222", machine="laptop",
                  last_activity=now - 3 * 3600)
    _seed_session(conn, "cccc3333", title="finished thing",
                  last_activity=now - 7200, ended_at=now - 7200)
    _seed_session(conn, "dddd4444", title="ancient ended",
                  last_activity=now - 3 * 86400, ended_at=now - 3 * 86400)
    _seed_session(conn, "eeee5555", title="marathon session",
                  last_activity=now - 3 * 86400)  # live, idle 3d
    conn.commit()
    conn.close()

    vault = config.STREAMS_VAULT
    _write_stream(vault, "alpha", "active", _iso(now - 10 * 86400),
                  next_line="ship it")
    _write_stream(vault, "beta", "blocked", _iso(now - 3600),
                  next_line="waiting on review")
    _write_stream(vault, "parked-one", "parked", _iso(now - 60))
    _write_stream(vault, "garbled", "active", "", garbled=True)
    return now


def test_brief_renders_sessions_streams_suggestions(brief_world, capsys):
    assert brief.run([]) == 0
    out = capsys.readouterr().out

    # Sessions: live local with title, slug, staleness, LIVE tag.
    assert "testbox · fix importer · stream:hive-core · 12m [LIVE]" in out
    # Foreign machine labeled with data age, id-prefix fallback label.
    assert "laptop · bbbb2222 · stream:- · data 3h old [LIVE]" in out
    # Ended <24h shows without LIVE; ended 3d ago excluded.
    ended_line = next(l for l in out.splitlines() if "finished thing" in l)
    assert "[LIVE]" not in ended_line
    assert "ancient ended" not in out

    # Streams: active/blocked shown with next, parked and garbled tolerated.
    assert "blocked beta · 1h · next: waiting on review" in out
    assert "active  alpha · 10d · next: ship it" in out
    assert "parked-one" not in out
    beta_pos, alpha_pos = out.index("beta"), out.index("alpha ")
    assert beta_pos < alpha_pos  # updated desc

    # Suggestions: stale active stream -> park; idle live session mentioned,
    # never a kill suggestion.
    assert "consider: hive park alpha" in out
    assert "marathon session" in out and "idle 3d" in out
    assert "kill" not in out.lower()


def test_brief_machine_filter(brief_world, capsys):
    assert brief.run(["--machine", "testbox"]) == 0
    out = capsys.readouterr().out
    assert "laptop" not in out and "fix importer" in out


def test_brief_stream_cap_and_all(tmp_env, capsys):
    _mkdb().close()
    now = db.now()
    for i in range(18):
        _write_stream(config.STREAMS_VAULT, f"s{i:02d}", "active",
                      _iso(now - i * 60), next_line="go")
    assert brief.run([]) == 0
    out = capsys.readouterr().out
    assert "… 3 more stream(s) — hive brief --all" in out
    assert "s17" not in out
    assert brief.run(["--all"]) == 0
    out = capsys.readouterr().out
    assert "s17" in out and "more stream(s)" not in out


def test_brief_empty_world(tmp_env, capsys):
    _mkdb().close()
    assert brief.run([]) == 0
    out = capsys.readouterr().out
    assert out.count("(none)") == 3


def test_age_formatting():
    now = 1_000_000
    assert brief._age(now, now - 12 * 60) == "12m"
    assert brief._age(now, now - 59 * 60) == "59m"
    assert brief._age(now, now - 3 * 3600) == "3h"
    assert brief._age(now, now - 2 * 86400) == "2d"
    assert brief._age(now, now + 500) == "0m"   # clock skew -> floor at 0
