"""Tests for hive.admin — pure functions, hook wiring, migrate/rebuild.

Run: cd ~/code/hive && PYTHONPATH=plugins/hive/lib python3 -m pytest tests/test_admin.py -q

These tests never run `crontab -`, never write ~/.claude/settings.json, and
never touch the real DB or ~/streams: crontab/settings fixtures are built
from strings, and migrate/rebuild run against a tmp_path DB + vault.
"""

from __future__ import annotations

import json
import os
import sqlite3
import time
from pathlib import Path

import pytest

from hive import admin, config, db

REPO = Path(__file__).resolve().parent.parent
PLUGIN = REPO / "plugins" / "hive"
HOME = str(Path.home())


# ───────────────────────────── new_crontab ─────────────────────────────────

FIXTURE_CRONTAB = """\
# PAUSED by chef 2026-07-08 (was noisy): 4-59/10 * * * * /home/x/keepalive-inject.sh >> /home/x/.keepalive.log 2>&1
30 6 * * * /home/x/bin/backup.sh >> /home/x/backup.log 2>&1
7 * * * * /home/x/.local/bin/ws sync >> /home/x/.local/state/ws/sync.log 2>&1
@reboot sleep 45 && /usr/bin/tmux new-session -d -s _ws-boot "sleep 150" && sleep 30 && WS_ROOT=/home/x/streams /home/x/.local/bin/ws up --quiet >> /home/x/.local/state/ws/up.log 2>&1
*/15 * * * * WS_ROOT=/home/x/streams /home/x/.local/bin/ws watchdog --revive --quiet >> /home/x/.local/state/ws/watchdog.log 2>&1
3 3 * * * PATH="/home/x/.local/bin:/usr/local/bin:/usr/bin:/bin" JHH3_MEM_HOME="/home/x/.local/share/jhh3-mem" /bin/sh -c '. "/home/x/.env.private" 2>/dev/null; "/home/x/.local/bin/jhh3-mem" nightly' >/dev/null 2>&1 # jhh3-mem-schedule
17 * * * * PATH="/home/x/.local/bin:/usr/local/bin:/usr/bin:/bin" JHH3_MEM_HOME="/home/x/.local/share/jhh3-mem" /bin/sh -c '. "/home/x/.env.private" 2>/dev/null; "/home/x/.local/bin/jhh3-mem" catchup' >/dev/null 2>&1 # jhh3-mem-schedule
"""


def test_new_crontab_drops_legacy_lines():
    out = admin.new_crontab(FIXTURE_CRONTAB, PLUGIN)
    assert "jhh3-mem" not in out
    assert "ws sync" not in out
    assert "ws watchdog" not in out
    assert "ws up" not in out
    assert "/bin/ws " not in out
    # Unrelated lines survive verbatim.
    assert "keepalive-inject.sh" in out
    assert "30 6 * * * /home/x/bin/backup.sh" in out


def test_new_crontab_adds_tagged_hive_lines():
    out = admin.new_crontab(FIXTURE_CRONTAB, PLUGIN)
    tick_lines = [ln for ln in out.splitlines() if ln.endswith("# hive-tick")]
    assert len(tick_lines) == 2
    five_min = [ln for ln in tick_lines if ln.startswith("*/5 * * * * ")]
    reboot = [ln for ln in tick_lines if ln.startswith("@reboot ")]
    assert len(five_min) == 1 and len(reboot) == 1
    # $HOME literally expanded (cron won't expand it in the schedule position).
    assert "$HOME" not in out
    assert f"{HOME}/.local/bin/hive tick" in five_min[0]
    assert "systemd-run --user --scope" in five_min[0]
    assert "MemoryMax=" in five_min[0] and "CPUQuota=" in five_min[0]
    assert f"{HOME}/.local/bin/hive tick --boot" in reboot[0]
    assert f". {HOME}/.env.private 2>/dev/null" in five_min[0]
    assert out.endswith("\n")


def test_new_crontab_idempotent():
    once = admin.new_crontab(FIXTURE_CRONTAB, PLUGIN)
    twice = admin.new_crontab(once, PLUGIN)
    assert once == twice
    # Also idempotent from empty.
    assert admin.new_crontab("", PLUGIN) == admin.new_crontab(
        admin.new_crontab("", PLUGIN), PLUGIN)


# ───────────────────────────── strip_agent_deck ────────────────────────────

FIXTURE_SETTINGS = {
    "model": "fable",
    "env": {"FOO": "1"},
    "hooks": {
        "Notification": [
            {
                "matcher": "permission_prompt|agent_needs_input",
                "hooks": [
                    {"type": "command", "command": "claude-hook-notify", "async": True}
                ],
            },
            {
                "matcher": "permission_prompt|elicitation_dialog",
                "hooks": [
                    {"type": "command", "command": "agent-deck hook-handler", "async": True}
                ],
            },
        ],
        "PermissionRequest": [
            {"hooks": [{"type": "command", "command": "agent-deck hook-handler"}]}
        ],
        "PreCompact": [
            {"hooks": [{"type": "command", "command": "agent-deck hook-handler"}]}
        ],
        "Stop": [
            {
                "hooks": [
                    {"type": "command", "command": "claude-hook-notify --stop", "async": True},
                    {"type": "command", "command": "agent-deck hook-handler"},
                ]
            }
        ],
        "SessionStart": [
            {"hooks": [{"type": "command", "command": "agent-deck hook-handler", "async": True}]}
        ],
    },
    "enabledPlugins": {"ws@streams": True},
}


def test_strip_agent_deck_removes_only_agent_deck():
    out = admin.strip_agent_deck(FIXTURE_SETTINGS)
    assert "agent-deck" not in json.dumps(out)
    # claude-hook-notify entries preserved verbatim.
    assert out["hooks"]["Notification"] == [
        {
            "matcher": "permission_prompt|agent_needs_input",
            "hooks": [
                {"type": "command", "command": "claude-hook-notify", "async": True}
            ],
        }
    ]
    assert out["hooks"]["Stop"] == [
        {
            "hooks": [
                {"type": "command", "command": "claude-hook-notify --stop", "async": True}
            ]
        }
    ]
    # Events that only held agent-deck are deleted entirely.
    for event in ("PermissionRequest", "PreCompact", "SessionStart"):
        assert event not in out["hooks"]
    # Everything else preserved.
    assert out["model"] == "fable"
    assert out["env"] == {"FOO": "1"}
    assert out["enabledPlugins"] == {"ws@streams": True}


def test_strip_agent_deck_does_not_mutate_input():
    snapshot = json.dumps(FIXTURE_SETTINGS, sort_keys=True)
    admin.strip_agent_deck(FIXTURE_SETTINGS)
    assert json.dumps(FIXTURE_SETTINGS, sort_keys=True) == snapshot


def test_strip_agent_deck_no_hooks_key():
    assert admin.strip_agent_deck({"model": "fable"}) == {"model": "fable"}


# ───────────────────────────── hooks.json wiring ───────────────────────────

def _hook_commands(doc: dict):
    for groups in doc["hooks"].values():
        for group in groups:
            for entry in group["hooks"]:
                yield entry


def test_hooks_json_parses_and_scripts_exist():
    doc = json.loads((PLUGIN / "hooks" / "hooks.json").read_text())
    events = set(doc["hooks"].keys())
    assert events == {"SessionStart", "UserPromptSubmit", "PostToolUse",
                      "Stop", "PreCompact", "SessionEnd"}
    seen = []
    for entry in _hook_commands(doc):
        assert entry["type"] == "command"
        cmd = entry["command"]
        assert cmd.startswith("bash ${CLAUDE_PLUGIN_ROOT}/")
        rel = cmd.split("${CLAUDE_PLUGIN_ROOT}/", 1)[1].split()[0]
        script = PLUGIN / rel
        assert script.is_file(), f"missing script: {rel}"
        assert os.access(script, os.X_OK), f"not executable: {rel}"
        seen.append(rel)
    # ws Stop must run FIRST (its {"decision":"block"} stdout passes through
    # unwrapped); harness runs after it.
    stop = [e["command"] for e in doc["hooks"]["Stop"][0]["hooks"]]
    assert "vendored-ws-stop.sh" in stop[0]
    assert "hook-harness.sh Stop" in stop[1]
    # PostToolUse is vendored-ws only: hive adds no per-tool-call work.
    ptu = doc["hooks"]["PostToolUse"][0]
    assert ptu["matcher"] == "*"
    assert len(ptu["hooks"]) == 1
    assert "vendored-ws-post-tool-use.sh" in ptu["hooks"][0]["command"]


def test_vendored_wrappers_target_real_vendor_scripts():
    for wrapper in sorted((PLUGIN / "scripts").glob("vendored-*.sh")):
        text = wrapper.read_text()
        assert 'export CLAUDE_PLUGIN_ROOT="$HERE/vendor/' in text
        vendor = text.split('$HERE/vendor/', 1)[1].split('"', 1)[0]
        target = [ln for ln in text.splitlines() if ln.startswith("exec bash ")][0]
        rel = target.split('$CLAUDE_PLUGIN_ROOT/', 1)[1].rstrip('"')
        assert (PLUGIN / "vendor" / vendor / rel).is_file(), \
            f"{wrapper.name} -> vendor/{vendor}/{rel} missing"


# ───────────────────────────── migrate + rebuild ───────────────────────────

SYNTHETIC = [
    # (uid, source_bytes, origin, kind, title, request, outcome, learned, files)
    ("sess-aaa", 1000, "live", "session", "Fix flaky test in CI",
     "please fix the flaky test", "Pinned the clock; test green.",
     "freezegun beats sleep()", [("tests/test_clock.py", 2)]),
    ("sess-aaa", 2000, "live", "session", "Fix flaky test in CI (resumed)",
     "continue", "Also fixed the other flake.", "",
     [("tests/test_clock.py", 2), ("src/clock.py", 2)]),
    ("sess-bbb", 500, "live", "session", "Refactor config loader",
     "token=abcdefgh12345678 is my secret", "Loader refactored.", "",
     [("src/config.py", 2)]),
    ("note:1:deadbeef", 0, "noted", "decision", "Use sqlite WAL everywhere",
     "", "Decision recorded.", "WAL avoids writer starvation", []),
    ("lgc:sum:42", 0, "legacy-session", "session", "Old migrated session",
     "", "Legacy outcome text.", "legacy learned text", []),
]


@pytest.fixture()
def env(tmp_path, monkeypatch):
    """Isolated DB + vault: nothing touches the real system."""
    monkeypatch.delenv("HIVE_DB", raising=False)
    monkeypatch.setenv("HIVE_MACHINE", "testbox")
    src_db = tmp_path / "memory.db"
    streams = tmp_path / "streams"
    monkeypatch.setattr(config, "DB_PATH", tmp_path / "no-such-index.db")
    monkeypatch.setattr(config, "STREAMS_VAULT", streams)
    monkeypatch.setattr(config, "STATE_DIR", tmp_path / "state")
    monkeypatch.setattr(config, "DATA_DIR", tmp_path / "data")
    monkeypatch.setattr(db, "JHH3_MEM_DB", src_db)

    conn = sqlite3.connect(src_db)
    conn.executescript(admin.MEM_SCHEMA_SQL.read_text())
    now = int(time.time())
    for i, (uid, sb, origin, kind, title, request, outcome, learned, files) in \
            enumerate(SYNTHETIC):
        cur = conn.execute(
            "INSERT INTO entries (uid, source_bytes, origin, producer, host,"
            " repo_key, kind, ts, title, request, outcome, learned)"
            " VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            (uid, sb, origin, "test/0.0", "claude-code", "test/repo", kind,
             now - i * 86400, title, request, outcome, learned))
        eid = cur.lastrowid
        for p, mode in files:
            conn.execute("INSERT OR IGNORE INTO path (p, base) VALUES (?, ?)",
                         (p, os.path.basename(p)))
            pid = conn.execute("SELECT id FROM path WHERE p=?", (p,)).fetchone()[0]
            conn.execute("INSERT OR IGNORE INTO entry_file (path_id, entry_id,"
                         " mode) VALUES (?,?,?)", (pid, eid, mode))
    conn.execute("UPDATE entries SET superseded=1"
                 " WHERE uid='sess-aaa' AND source_bytes=1000")
    conn.commit()
    conn.close()
    return {"db": src_db, "streams": streams, "tmp": tmp_path}


def test_migrate_export_verify_rebuild_roundtrip(env, capsys):
    # dry-run plan never writes lanes
    assert admin.cmd_migrate([]) == 0
    lanes = env["streams"] / "mem" / "lanes" / "testbox"
    assert not lanes.exists() or not list(lanes.glob("*.ndjson"))

    # export
    assert admin.cmd_migrate(["--export-history"]) == 0
    files = sorted(lanes.glob("history-*.ndjson"))
    assert files, "no history lane files written"
    recs = []
    for f in files:
        for line in f.read_text().splitlines():
            recs.append(json.loads(line))
    assert len(recs) == len(SYNTHETIC)
    by_key = {(r["entry"]["uid"], r["entry"]["source_bytes"]): r for r in recs}
    assert len(by_key) == len(SYNTHETIC)
    # host + source_bytes + files survive; rev=1 put records
    r = by_key[("sess-aaa", 2000)]
    assert r["op"] == "put" and r["rev"] == 1
    assert r["entry"]["host"] == "claude-code"
    assert sorted(p for p, _ in r["files"]) == ["src/clock.py", "tests/test_clock.py"]
    assert all(m == 2 for _, m in r["files"])
    # redact.sed gate ran: the planted secret never reaches the lane
    secret_rec = by_key[("sess-bbb", 500)]
    assert "abcdefgh12345678" not in json.dumps(secret_rec)
    assert "[REDACTED" in secret_rec["entry"]["request"]

    # idempotency guard: a second export refuses without --force
    assert admin.cmd_migrate(["--export-history"]) == 1
    assert admin.cmd_migrate(["--export-history", "--force"]) == 0
    total = sum(len(f.read_text().splitlines())
                for f in lanes.glob("history-*.ndjson"))
    assert total == len(SYNTHETIC)

    # verify: counts + sampled field equality
    assert admin.cmd_migrate(["--verify"]) == 0
    out = capsys.readouterr().out
    assert "PASS" in out and "FAIL" not in out

    # rebuild into a fresh DB
    dst = env["tmp"] / "rebuilt.db"
    assert admin.cmd_rebuild(["--to", str(dst)]) == 0
    assert admin.cmd_rebuild(["--to", str(dst)]) == 1  # refuses existing

    src = sqlite3.connect(env["db"])
    new = sqlite3.connect(dst)
    new.row_factory = src.row_factory = sqlite3.Row
    try:
        assert (new.execute("SELECT count(*) FROM entries").fetchone()[0]
                == len(SYNTHETIC))
        assert new.execute("SELECT count(*) FROM path").fetchone()[0] == 3
        assert new.execute("SELECT count(*) FROM entry_file").fetchone()[0] == 4
        for uid, sb, *_ in SYNTHETIC:
            a = src.execute("SELECT * FROM entries WHERE uid=? AND source_bytes=?",
                            (uid, sb)).fetchone()
            b = new.execute("SELECT * FROM entries WHERE uid=? AND source_bytes=?",
                            (uid, sb)).fetchone()
            assert b is not None
            for col in ("origin", "host", "kind", "ts", "title", "outcome",
                        "learned", "superseded", "capture_mode", "durability"):
                assert b[col] == a[col], f"{uid}/{sb} col {col}"
        # FTS populated by the schema triggers during insert
        hit = new.execute("SELECT count(*) FROM entries_fts"
                          " WHERE entries_fts MATCH 'flaky'").fetchone()[0]
        assert hit >= 1
        # hive_* schema present in the rebuilt DB
        assert new.execute("SELECT count(*) FROM sqlite_master WHERE type='table'"
                           " AND name LIKE 'hive_%'").fetchone()[0] > 0
    finally:
        src.close()
        new.close()


def test_rebuild_requires_to(env):
    assert admin.cmd_rebuild([]) == 2


# ───────────────────────────── doctor telemetry ────────────────────────────

def _metric_line(now: float, age_s: float, event: str, status: str,
                 rc: int, ms: int) -> str:
    import datetime
    iso = datetime.datetime.fromtimestamp(now - age_s).isoformat()
    return f"{iso} [{event}] {status} rc={rc} ms={ms}"


def test_parse_hook_metrics_window_and_malformed_lines():
    now = time.time()
    lines = [
        _metric_line(now, 60, "SessionStart", "ok", 0, 120),
        _metric_line(now, 120, "SessionStart", "fail", 1, 800),
        _metric_line(now, 3600, "Stop", "ok", 0, 40),
        _metric_line(now, 8 * 86400, "Stop", "ok", 0, 9999),  # outside 7d
        "garbage that is not a metrics line",
        "2026-08-27T10:00:00 [Stop] ok rc=0",                 # no ms=
        "",
    ]
    stats = admin.parse_hook_metrics(lines, now=now)
    assert stats == {"SessionStart": [120, 800], "Stop": [40]}


def test_percentile_nearest_rank():
    values = list(range(1, 101))
    assert admin.percentile(values, 0.5) == 51
    assert admin.percentile(values, 0.95) == 96
    assert admin.percentile([42], 0.95) == 42
    assert admin.percentile([300, 100, 200], 0.5) == 200


def test_sweep_backlog_count(tmp_path, monkeypatch):
    dbp = tmp_path / "m.db"
    monkeypatch.setenv("HIVE_DB", str(dbp))
    projects = tmp_path / "claude-projects"
    monkeypatch.setattr(config, "CLAUDE_PROJECTS", projects)
    monkeypatch.setattr(config, "CODEX_SESSIONS", tmp_path / "codex-sessions")

    conn = sqlite3.connect(dbp)
    db.ensure_hive_schema(conn)
    proj = projects / "proj"
    proj.mkdir(parents=True)
    (proj / "synced.jsonl").write_bytes(b"x" * 100)
    (proj / "behind.jsonl").write_bytes(b"x" * 200)
    (proj / "unknown.jsonl").write_bytes(b"x" * 10)
    stale = proj / "old.jsonl"
    stale.write_bytes(b"x" * 10)
    old = time.time() - 8 * 86400
    os.utime(stale, (old, old))
    for sid, off in (("synced", 100), ("behind", 50)):
        conn.execute(
            "INSERT INTO hive_capture_state (harness, session_id, offset_bytes)"
            " VALUES ('claude-code', ?, ?)", (sid, off))
    conn.commit()
    conn.close()

    # behind (size != offset) + unknown (no state row); synced and >7d excluded.
    assert admin.sweep_backlog_count() == 2
    assert admin.sweep_backlog_count(limit=1) <= 1
