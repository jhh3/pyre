"""vault-raw pusher: PID lock, bounded batches, rebatch of oversized
history, failure backoff, and the tick only *requesting* a push."""

import os
import subprocess

import pytest

from hive import config, raw, raw_push  # noqa: E402
from test_sync import _jsonl_bytes, git, insert_session, world  # noqa: F401


@pytest.fixture
def rawvault(world):  # noqa: F811
    """world + a vault-raw with a bare origin holding one initial commit."""
    vault = config.RAW_VAULT
    bare = world.tmp / "raw-origin.git"
    subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(bare)],
                   check=True, timeout=30)
    vault.mkdir(parents=True)
    git(vault, "init", "-q", "-b", "main")
    (vault / "README").write_text("raw\n")
    git(vault, "add", "-A")
    git(vault, "commit", "-q", "-m", "init")
    git(vault, "remote", "add", "origin", str(bare))
    git(vault, "push", "-q", "origin", "main")
    world.mp.setattr(config, "TICK_LOCK", world.tmp / "tick.lock")
    world.bare = bare
    world.spawned = []
    world.mp.setattr(raw_push, "_spawn_pusher",
                     lambda v: world.spawned.append(v))
    return world


def _commit_blob(vault, name: str, nbytes: int) -> str:
    p = vault / "raw" / "macha" / "claude-code" / name
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_bytes(os.urandom(nbytes))
    git(vault, "add", "-A")
    git(vault, "commit", "-q", "-m", f"hive raw {name}")
    return git(vault, "rev-parse", "HEAD").strip()


def _remote_head(w) -> str:
    return subprocess.run(["git", "-C", str(w.bare), "rev-parse", "main"],
                          capture_output=True, text=True).stdout.strip()


# ── lock ────────────────────────────────────────────────────────────────────

def test_lock_is_exclusive_and_reclaims_dead_owner(rawvault):
    lock = raw_push.lock_path()
    assert raw_push.acquire_lock()
    assert lock.read_text().strip() == str(os.getpid())
    assert raw_push.lock_held()
    assert not raw_push.acquire_lock()  # held by a live pid (us)

    raw_push.release_lock()
    assert not lock.exists()

    dead = subprocess.Popen(["true"])
    dead.wait()
    lock.write_text(f"{dead.pid}\n")
    assert not raw_push.lock_held()
    assert raw_push.acquire_lock()      # stale owner reclaimed
    assert lock.read_text().strip() == str(os.getpid())

    lock.write_text("1\n")              # someone else's (live) lock
    raw_push.release_lock()             # never removes a lock we don't own
    assert lock.exists()
    assert not raw_push.acquire_lock()


def test_second_pusher_exits_without_pushing(rawvault):
    head = _commit_blob(config.RAW_VAULT, "a.bin", 1000)
    raw_push.lock_path().parent.mkdir(parents=True, exist_ok=True)
    raw_push.lock_path().write_text("1\n")  # pid 1 is always alive
    assert raw_push.run_pusher(config.RAW_VAULT) == 0
    assert _remote_head(rawvault) != head


# ── batching ────────────────────────────────────────────────────────────────

def test_backlog_pushes_in_bounded_batches(rawvault, monkeypatch):
    monkeypatch.setattr(raw_push, "BATCH_BYTES", 250_000)
    shas = [_commit_blob(config.RAW_VAULT, f"s{i}.bin", 100_000)
            for i in range(7)]
    pushed = []
    real = raw_push._git

    def spy(vault, *args, **kw):
        if args[0] == "push":
            pushed.append(args[-1].split(":")[0])
        return real(vault, *args, **kw)

    monkeypatch.setattr(raw_push, "_git", spy)
    assert raw_push.run_pusher(config.RAW_VAULT) == 0
    # 100 KB commits under a 250 KB batch: two per batch, last one alone.
    assert pushed == [shas[1], shas[3], shas[5], shas[6]]
    assert _remote_head(rawvault) == shas[-1]
    st = raw_push.load_state()
    assert st["failures"] == 0 and st["last_ok_at"] > 0


def test_batches_cut_before_overflow():
    commits = [("a", 60, 60), ("b", 60, 60), ("c", 200, 200), ("d", 1, 1)]
    assert raw_push.batches(commits, limit=150) == [
        ("b", 120), ("c", 200), ("d", 1)]
    assert raw_push.batches([], limit=10) == []


def test_oversized_file_triggers_rebatch_with_split(rawvault, monkeypatch):
    """A segment over GitHub's file limit is re-split and the unpushed
    history replaced by bounded commits; reassembly stays byte-identical."""
    vault = config.RAW_VAULT
    transcript = rawvault.tmp / "big.jsonl"
    data = os.urandom(300_000)
    transcript.write_bytes(data)
    insert_session(rawvault.conn_a, "macha", session_id="sess-big",
                   transcript_path=str(transcript), cwd="/home/x/big",
                   ended_at=None)
    monkeypatch.setattr(raw_push, "SEGMENT_MAX_BYTES", 10**9)
    raw.tick_phase()  # one 300 KB segment (written before the cap existed)
    _commit_blob(vault, "small.bin", 1000)
    old_head = git(vault, "rev-parse", "HEAD").strip()

    monkeypatch.setattr(raw_push, "GITHUB_BLOB_LIMIT", 200_000)
    monkeypatch.setattr(raw_push, "SPLIT_BYTES", 150_000)
    monkeypatch.setattr(raw_push, "SEGMENT_MAX_BYTES", 100_000)
    monkeypatch.setattr(raw_push, "BATCH_BYTES", 180_000)
    assert raw_push.run_pusher(vault, force=True) == 0, \
        raw_push.load_state().get("last_error")

    seg_dir = vault / "raw" / "macha" / "claude-code" / "sess-big"
    segs = sorted(seg_dir.glob("*.jsonl.zst"))
    assert len(segs) == 3 and all(s.stat().st_size < 200_000 for s in segs)
    assert _remote_head(rawvault) == git(vault, "rev-parse", "HEAD").strip()
    assert git(vault, "status", "--porcelain").strip() == ""
    refs = git(vault, "for-each-ref", "refs/hive/pre-rebatch/")
    assert old_head in refs
    sizes = git(vault, "rev-list", "--objects", "main").split()
    assert sizes  # remote holds the rewritten history
    big = subprocess.run(
        ["git", "-C", str(rawvault.bare), "rev-list", "--objects", "main"],
        capture_output=True, text=True).stdout
    check = subprocess.run(
        ["git", "-C", str(rawvault.bare), "cat-file",
         "--batch-check=%(objecttype) %(objectsize)"],
        input="\n".join(line.split()[0] for line in big.splitlines()) + "\n",
        capture_output=True, text=True).stdout
    assert max(int(x.split()[1]) for x in check.splitlines()
               if x.startswith("blob")) < 200_000

    dest = raw.import_session("sess-big")
    assert dest.read_bytes() == data


def test_segments_are_capped_at_write(rawvault, monkeypatch):
    monkeypatch.setattr(raw_push, "SEGMENT_MAX_BYTES", 10_000)
    transcript = rawvault.tmp / "t.jsonl"
    data = _jsonl_bytes(35_000, "cap")
    transcript.write_bytes(data)
    insert_session(rawvault.conn_a, "macha", session_id="sess-cap",
                   transcript_path=str(transcript), cwd="/home/x/cap",
                   ended_at=None)
    raw.tick_phase()
    seg_dir = config.RAW_VAULT / "raw" / "macha" / "claude-code" / "sess-cap"
    assert len(list(seg_dir.glob("*.jsonl.zst"))) == 4
    assert raw.import_session("sess-cap").read_bytes() == data


# ── backoff ─────────────────────────────────────────────────────────────────

def test_failure_backs_off_exponentially_and_success_resets(rawvault):
    vault = config.RAW_VAULT
    _commit_blob(vault, "a.bin", 1000)
    git(vault, "config", "remote.origin.pushurl", "DISABLED:/dev/null")
    assert raw_push.run_pusher(vault) == 1
    st = raw_push.load_state()
    assert st["failures"] == 1 and "batch 1/1" in st["last_error"]
    assert st["next_attempt_at"] - st["last_failure_at"] == 300
    assert "FAILED" in config.TICK_LOG.read_text()

    # Inside the backoff window nothing is attempted.
    assert raw_push.run_pusher(vault) == 0
    assert raw_push.load_state()["failures"] == 1

    assert raw_push.run_pusher(vault, force=True) == 1
    st = raw_push.load_state()
    assert st["next_attempt_at"] - st["last_failure_at"] == 600

    assert [raw_push.backoff_delay(n) for n in (1, 2, 3, 7, 8, 50)] == \
        [300, 600, 1200, 19200, 21600, 21600]

    git(vault, "config", "--unset", "remote.origin.pushurl")
    assert raw_push.run_pusher(vault, force=True) == 0
    st = raw_push.load_state()
    assert st["failures"] == 0 and st["next_attempt_at"] == 0


def test_timeout_kills_the_whole_process_group(tmp_path):
    marker = tmp_path / "child-survived"
    r = raw_push.run_bounded(
        ["sh", "-c", f"(sleep 3; touch {marker}) & sleep 30"], timeout=1)
    assert r.returncode == -9 and "timed out" in r.stderr
    import time
    time.sleep(3.5)
    assert not marker.exists()


# ── request: the tick never pushes itself ───────────────────────────────────

def test_tick_only_requests_one_pusher(rawvault, monkeypatch):
    transcript = rawvault.tmp / "live.jsonl"
    transcript.write_bytes(_jsonl_bytes(6000, "live"))
    insert_session(rawvault.conn_a, "macha", session_id="sess-live",
                   transcript_path=str(transcript), ended_at=None)
    pushes = []
    real = raw_push._git
    monkeypatch.setattr(raw_push, "_git", lambda v, *a, **k: (
        pushes.append(a) if a[0] == "push" else None) or real(v, *a, **k))

    raw.tick_phase()
    assert pushes == []                      # tick never pushes
    assert len(rawvault.spawned) == 1        # it starts one pusher
    assert raw_push.wanted_path().exists()

    # Pusher alive → later ticks only re-mark "wanted".
    raw_push.lock_path().write_text("1\n")
    with open(transcript, "ab") as f:
        f.write(_jsonl_bytes(6000, "more"))
    raw.tick_phase()
    assert len(rawvault.spawned) == 1

    # In backoff → no spawn either, even with the lock free.
    raw_push.lock_path().unlink()
    raw_push.record_failure("boom")
    assert not raw_push.request(config.RAW_VAULT)
    assert len(rawvault.spawned) == 1

    # Recent success → debounced.
    raw_push.record_success()
    assert not raw_push.request(config.RAW_VAULT)
    assert raw_push.request(config.RAW_VAULT,
                            now=raw_push.load_state()["last_ok_at"] + 10_000)
    assert len(rawvault.spawned) == 2
