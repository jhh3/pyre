#!/usr/bin/env python3
"""Validate the documentation set and its local links, without claiming to test the application."""

import re
import sys
from pathlib import Path
from urllib.parse import unquote, urlsplit

ROOT = Path(__file__).resolve().parents[1]
REQUIRED_FILES = (
    "AGENTS.md", "README.md", "CONTRIBUTING.md", "SECURITY.md",
    "docs/product-spec.md", "docs/decisions.md", "docs/architecture.md",
    "docs/contracts.md", "docs/research.md", "docs/integrations.md",
    "docs/acceptance.md", "docs/experience.md", "docs/operations.md",
)
REQUIREMENTS = {f"R{i:02d}" for i in range(1, 15)}
errors = []


def check(condition, message):
    if not condition:
        errors.append(message)


def text(path):
    return (ROOT / path).read_text(encoding="utf-8")


def prose_lines(source):
    """Ignore fenced code examples when examining actual Markdown links."""
    fence = None
    for number, line in enumerate(source.splitlines(), 1):
        marker = re.match(r"^\s*(`{3,}|~{3,})", line)
        if marker:
            value = marker.group(1)
            if fence is None:
                fence = value
            elif value[0] == fence[0] and len(value) >= len(fence):
                fence = None
            continue
        if fence is None:
            yield number, line


def validate_links():
    count = 0
    # These documents use inline links. Check target paths, not remote URLs or anchors.
    pattern = re.compile(r"\[[^\]\n]+\]\((<[^>\n]+>|[^\s)]+)(?:\s+\"[^\"]*\")?\)")
    for path in sorted(ROOT.rglob("*.md")):
        if any(part.startswith(".") or part in {"node_modules", "build", "dist"}
               for part in path.relative_to(ROOT).parts):
            continue
        if path.name.startswith("UPSTREAM-"):
            continue  # verbatim vendored upstream text; its links point into the upstream repo
        for number, line in prose_lines(path.read_text(encoding="utf-8")):
            for match in pattern.finditer(line):
                target = match.group(1).strip("<>")
                url = urlsplit(target)
                if url.scheme or url.netloc or not url.path:
                    continue
                resolved = (path.parent / unquote(url.path)).resolve()
                check(resolved.is_relative_to(ROOT),
                      f"{path.relative_to(ROOT)}:{number}: link escapes repository: {target}")
                check(resolved.exists(),
                      f"{path.relative_to(ROOT)}:{number}: missing link target: {target}")
                count += 1
    return count


def main():
    for path in REQUIRED_FILES:
        check((ROOT / path).is_file(), f"Missing required document: {path}")
    if errors:
        return finish()
    product_ids = re.findall(r"^\| (R\d{2}) \|", text("docs/product-spec.md"), re.MULTILINE)
    check(set(product_ids) == REQUIREMENTS and len(product_ids) == len(REQUIREMENTS),
          "Product spec must define each R01–R14 exactly once")
    acceptance_source = text("docs/acceptance.md")
    cases = re.findall(r"^\| (A\d{2}) \| ([^|]+) \|", acceptance_source, re.MULTILINE)
    check(len(cases) >= 20 and len({c[0] for c in cases}) == len(cases),
          "Expected at least 20 uniquely identified acceptance cases")
    case_requirements = set()
    for case_id, requirements in cases:
        refs = set(re.findall(r"R\d{2}", requirements))
        check(bool(refs) and refs <= REQUIREMENTS, f"{case_id}: invalid requirement mapping")
        case_requirements.update(refs)
    check(case_requirements == REQUIREMENTS, "Acceptance cases do not cover every requirement")
    links = validate_links()
    if not errors:
        print(f"Documentation checks passed: {len(REQUIRED_FILES)} required documents, "
              f"{links} local links, {len(REQUIREMENTS)} requirements, "
              f"{len(cases)} acceptance cases.")
        print("Application behavior has not been validated by this check.")
    return finish()


def finish():
    for error in errors:
        print(f"ERROR: {error}", file=sys.stderr)
    return 1 if errors else 0


if __name__ == "__main__":
    sys.exit(main())
