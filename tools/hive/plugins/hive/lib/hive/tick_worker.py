"""Detached tick accelerator: `python3 -m hive.tick_worker --phases enrich,sync`.

Spawn-detached by hooks (hooks.on_session_end) so enrichment and sync run
seconds after a close instead of waiting for the */5 cron. It is only an
accelerator — tick's lock makes a concurrent cron pass skip, and the cron is
the guarantee if this process never runs. The small --delay lets the hook's
own DB writes land first.
"""

from __future__ import annotations

import argparse
import sys
import time

from . import tick


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="hive.tick_worker")
    parser.add_argument("--phases", default="enrich,sync")
    parser.add_argument("--delay", type=int, default=2)
    args = parser.parse_args(sys.argv[1:] if argv is None else argv)
    if args.delay > 0:
        time.sleep(min(args.delay, 60))
    return tick.run(["--phases", args.phases])


if __name__ == "__main__":
    sys.exit(main())
