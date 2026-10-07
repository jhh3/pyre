"""Make plugins/hive/lib importable and isolate every test from the real home.

hive.config resolves its data/state/config/vault paths from the environment at
import time, so the redirection below runs before any test module imports
hive. Without it, tests that reach config.machine_id() or the default index.db
write into the user's real ~/.local/share/hive.
"""

import atexit
import os
import shutil
import sys
import tempfile
from pathlib import Path

_REAL_HOME = os.path.expanduser("~")
_TEST_HOME = Path(tempfile.mkdtemp(prefix="hive-test-home."))
atexit.register(shutil.rmtree, _TEST_HOME, True)

# Tools installed under the real home (pyenv shims, uv) keep working.
if os.path.isdir(os.path.join(_REAL_HOME, ".pyenv")):
    os.environ.setdefault("PYENV_ROOT", os.path.join(_REAL_HOME, ".pyenv"))

os.environ.update({
    "HOME": str(_TEST_HOME),
    "XDG_DATA_HOME": str(_TEST_HOME / ".local/share"),
    "XDG_STATE_HOME": str(_TEST_HOME / ".local/state"),
    "XDG_CONFIG_HOME": str(_TEST_HOME / ".config"),
    "XDG_CACHE_HOME": str(_TEST_HOME / ".cache"),
    "HIVE_DATA_DIR": str(_TEST_HOME / ".local/share/hive"),
    "HIVE_STATE_DIR": str(_TEST_HOME / ".local/state/hive"),
    "HIVE_CONFIG": str(_TEST_HOME / ".config/hive/hive.toml"),
    "HIVE_STREAMS_VAULT": str(_TEST_HOME / "streams"),
    "HIVE_RAW_VAULT": str(_TEST_HOME / ".local/share/hive/vault-raw"),
    "JHH3_MEM_HOME": str(_TEST_HOME / ".local/share/jhh3-mem"),
    # A temp HOME has no ~/.gitconfig; tests that commit need an identity.
    "GIT_AUTHOR_NAME": "hive-test",
    "GIT_AUTHOR_EMAIL": "hive-test@example.invalid",
    "GIT_COMMITTER_NAME": "hive-test",
    "GIT_COMMITTER_EMAIL": "hive-test@example.invalid",
})
for _k in ("HIVE_MACHINE", "TMUX", "TMUX_PANE"):
    os.environ.pop(_k, None)

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "plugins/hive/lib"))
