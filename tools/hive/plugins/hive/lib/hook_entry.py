"""Hook entrypoint: reads the hook payload from stdin, dispatches by event.

Called only via scripts/hook-harness.sh (which owns timeouts, breaker,
logging, and the always-exit-0 contract). This process may print JSON with
additionalContext to stdout for events that support injection.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))

from hive import hooks  # noqa: E402


def main() -> int:
    event = sys.argv[1] if len(sys.argv) > 1 else "unknown"
    try:
        payload = json.load(sys.stdin)
    except (json.JSONDecodeError, ValueError):
        payload = {}
    handler = {
        "SessionStart": hooks.on_session_start,
        "Stop": hooks.on_stop,
        "PreCompact": hooks.on_pre_compact,
        "SessionEnd": hooks.on_session_end,
        "UserPromptSubmit": hooks.on_user_prompt_submit,
        "PostToolUse": hooks.on_post_tool_use,
    }.get(event)
    if handler is None:
        return 0
    return handler(payload) or 0


if __name__ == "__main__":
    sys.exit(main())
