#!/usr/bin/env python3
"""Small dependency-free release guard for the checked-in outfit plugin."""

import json
import pathlib
import sys


root = pathlib.Path(sys.argv[1]).resolve()
codex = json.loads((root / ".codex-plugin" / "plugin.json").read_text())
claude = json.loads((root / ".claude-plugin" / "plugin.json").read_text())
if codex.get("name") != "outfit" or claude.get("name") != "outfit":
    raise SystemExit("both plugin manifests must identify outfit")
if codex.get("version") != claude.get("version"):
    raise SystemExit("Codex and Claude plugin versions differ")
if not (root / ".mcp.json").is_file() or not (root / "hooks" / "hooks.json").is_file():
    raise SystemExit("plugin MCP or hooks manifest is missing")
skill = (root / "skills" / "using-outfit" / "SKILL.md").read_text()
if not skill.startswith("---\n") or "\nname: using-outfit\n" not in skill or "\ndescription:" not in skill:
    raise SystemExit("using-outfit skill frontmatter is invalid")
print(f"validated outfit plugin {codex['version']} for Codex and Claude")
