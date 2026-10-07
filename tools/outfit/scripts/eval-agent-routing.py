#!/usr/bin/env python3
"""Run and compare real Outfit routing decisions from Codex or Claude Code."""

from __future__ import annotations

import argparse
from dataclasses import dataclass
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from typing import Any


ROOT = Path(__file__).resolve().parents[1]
DEFAULT_CASES = ROOT / "evals" / "routing.jsonl"
DEFAULT_MATRIX_CASES = ROOT / "evals" / "mode-matrix.jsonl"
DEFAULT_RECOVERY_CASES = ROOT / "evals" / "recovery.jsonl"
DECISION_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "connection": {"type": "string"},
        "mode": {"type": "string"},
        "operation": {"type": "string"},
        "class": {"type": "string", "enum": ["read", "write", "destructive"]},
        "command": {"type": "string"},
        "reason": {"type": "string"},
    },
    "required": ["connection", "mode", "operation", "class", "command", "reason"],
    "additionalProperties": False,
}

RECOVERY_SCHEMA: dict[str, Any] = {
    "type": "object",
    "properties": {
        "diagnosis": {"type": "string"},
        "action": {"type": "string", "enum": ["reauthenticate", "disambiguate", "enable-link-context", "install-tool", "wait-and-retry"]},
        "command": {"type": "string"},
        "retry": {"type": "boolean", "description": "True only when the returned command repairs/prepares state and the original user task must be issued afterward. False when the returned command itself performs the original task."},
        "reason": {"type": "string"},
    },
    "required": ["diagnosis", "action", "command", "retry", "reason"],
    "additionalProperties": False,
}


@dataclass(frozen=True)
class Candidate:
    executable: str
    digest: str
    root: Path


@dataclass(frozen=True)
class EvalContext:
    candidate: Candidate
    state_root: Path
    guidance: str
    environment: dict[str, str]


def eval_runtime_root(candidate: Candidate) -> Path:
    configured = os.environ.get("OUTFIT_EVAL_RUNTIME_ROOT")
    base = Path(configured).expanduser() if configured else Path.home() / ".cache" / "outfit" / "evals"
    root = (base / candidate.digest).resolve()
    root.mkdir(parents=True, exist_ok=True)
    root.chmod(0o700)
    return root


class HostFailure(RuntimeError):
    """An operational host failure that occurred before a routing decision."""

    def __init__(self, code: str, message: str, **details: Any) -> None:
        super().__init__(message)
        self.code = code
        self.details = details

    def diagnostic(self, attempt: int, elapsed_ms: int) -> dict[str, Any]:
        return {
            "attempt": attempt,
            "status": "error",
            "code": self.code,
            "message": str(self),
            "elapsed_ms": elapsed_ms,
            **self.details,
        }


def excerpt(value: str | bytes | None, limit: int = 2000) -> str:
    if isinstance(value, bytes):
        value = value.decode(errors="replace")
    return (value or "").strip()[-limit:]


def resolve_state_root() -> Path:
    if configured := os.environ.get("OUTFIT_HOME"):
        return Path(configured).expanduser().resolve()
    if configured := os.environ.get("XDG_CONFIG_HOME"):
        return (Path(configured).expanduser() / "outfit").resolve()
    if sys.platform == "darwin":
        return (Path.home() / "Library" / "Application Support" / "outfit").resolve()
    return (Path.home() / ".config" / "outfit").resolve()


def stage_candidate(executable: str) -> Candidate:
    source = Path(executable).expanduser()
    if not source.is_absolute():
        located = shutil.which(executable)
        if not located:
            raise ValueError(f"candidate executable {executable!r} was not found")
        source = Path(located)
    source = source.resolve()
    digest = hashlib.sha256(source.read_bytes()).hexdigest()
    root = ROOT / ".outfit-eval" / "candidates" / digest
    destination = root / "bin" / "outfit"
    destination.parent.mkdir(parents=True, exist_ok=True)
    if not destination.exists() or hashlib.sha256(destination.read_bytes()).hexdigest() != digest:
        temporary = destination.with_suffix(".tmp")
        shutil.copy2(source, temporary)
        temporary.chmod(0o755)
        os.replace(temporary, destination)
    return Candidate(str(destination), digest, root)


def copy_tool_state(source: Path, destination: Path) -> None:
    tools = source / "tools"
    target = destination / "tools"
    if tools.is_dir() and not target.exists():
        shutil.copytree(tools, target)


def snapshot_state(candidate: Candidate, label: str) -> Path:
    source = resolve_state_root()
    database = source / "outfit.db"
    if not database.is_file():
        raise ValueError(f"Outfit state database does not exist at {database}")
    destination = eval_runtime_root(candidate) / "states" / safe_name(label)
    if destination.exists():
        shutil.rmtree(destination)
    destination.mkdir(parents=True)
    copy_tool_state(source, destination)
    with sqlite3.connect(database) as origin, sqlite3.connect(destination / "outfit.db") as snapshot:
        origin.backup(snapshot)
    os.chmod(destination / "outfit.db", 0o600)
    return destination


def safe_name(value: str) -> str:
    normalized = re.sub(r"[^A-Za-z0-9_.-]+", "-", value).strip("-.")
    return normalized or "eval"


def candidate_environment(candidate: Candidate, state_root: Path, label: str) -> dict[str, str]:
    host_root = eval_runtime_root(candidate) / "hosts" / safe_name(label)
    environment = os.environ.copy()
    environment.update(
        {
            "OUTFIT_HOME": str(state_root),
            "OUTFIT_AGENT": "1",
            "CODEX_HOME": str(host_root / "codex"),
            "CLAUDE_CONFIG_DIR": str(host_root / "claude"),
        }
    )
    real_codex = Path(os.environ.get("CODEX_HOME", Path.home() / ".codex"))
    staged_codex = Path(environment["CODEX_HOME"])
    staged_codex.mkdir(parents=True, exist_ok=True)
    for name in ("auth.json", ".credentials.json"):
        if (real_codex / name).is_file() and not (staged_codex / name).exists():
            shutil.copy2(real_codex / name, staged_codex / name)
    real_claude = Path(os.environ.get("CLAUDE_CONFIG_DIR", Path.home() / ".claude"))
    staged_claude = Path(environment["CLAUDE_CONFIG_DIR"])
    staged_claude.mkdir(parents=True, exist_ok=True)
    for name in (".credentials.json", "credentials.json"):
        if (real_claude / name).is_file() and not (staged_claude / name).exists():
            shutil.copy2(real_claude / name, staged_claude / name)
    return environment


def prepare_context(candidate: Candidate, state_root: Path, connection: str, label: str) -> EvalContext:
    environment = candidate_environment(candidate, state_root, label)
    codex_root = Path(environment["CODEX_HOME"])
    codex_root.mkdir(parents=True, exist_ok=True)
    if (codex_root / "skills").exists():
        shutil.rmtree(codex_root / "skills")
    completed = subprocess.run(
        [candidate.executable, "skill", "sync", "--host", "codex"],
        cwd=ROOT,
        env=environment,
        text=True,
        capture_output=True,
        timeout=60,
        check=False,
    )
    if completed.returncode != 0:
        raise RuntimeError(completed.stdout.strip() or completed.stderr.strip() or "candidate skill sync failed")
    sections = [(ROOT / "plugins" / "outfit" / "skills" / "using-outfit" / "SKILL.md").read_text()]
    generated = codex_root / "skills" / ("outfit-" + safe_name(connection)) / "SKILL.md"
    if generated.is_file():
        sections.append(generated.read_text())
    session = subprocess.run(
        [candidate.executable, "session", "context", "--hook", "codex"],
        cwd=ROOT,
        env=environment,
        text=True,
        capture_output=True,
        timeout=30,
        check=False,
    )
    if session.returncode == 0 and session.stdout.strip():
        try:
            payload = json.loads(session.stdout)
            context = payload.get("hookSpecificOutput", {}).get("additionalContext", "")
            if context:
                sections.append(str(context))
        except json.JSONDecodeError:
            sections.append(session.stdout.strip())
    guidance = "\n\n--- exact candidate guidance ---\n\n".join(sections)
    return EvalContext(candidate, state_root, guidance[-24000:], environment)


def load_jsonl(path: Path) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    seen: set[str] = set()
    for line_number, line in enumerate(path.read_text().splitlines(), 1):
        if not line.strip():
            continue
        row = json.loads(line)
        missing = {"id", "prompt", "connection", "operation", "class", "write"} - row.keys()
        if missing:
            raise ValueError(f"{path}:{line_number}: missing {sorted(missing)}")
        if row["id"] in seen:
            raise ValueError(f"{path}:{line_number}: duplicate id {row['id']}")
        if row["class"] not in {"read", "write", "destructive"} or not isinstance(row["write"], bool):
            raise ValueError(f"{path}:{line_number}: invalid class/write contract")
        if not isinstance(row.get("command_contains", []), list) or not all(isinstance(value, str) and value for value in row.get("command_contains", [])):
            raise ValueError(f"{path}:{line_number}: command_contains must contain non-empty strings")
        seen.add(row["id"])
        rows.append(row)
    if not rows:
        raise ValueError(f"{path}: no cases")
    return rows


def load_recovery_cases(path: Path) -> list[dict[str, Any]]:
    rows = load_raw_jsonl(path)
    required = {"id", "connector", "mode", "connection", "failure", "action", "retry"}
    seen: set[str] = set()
    for index, row in enumerate(rows, 1):
        missing = required - row.keys()
        if missing:
            raise ValueError(f"{path}:{index}: missing {sorted(missing)}")
        if row["id"] in seen or not isinstance(row["retry"], bool):
            raise ValueError(f"{path}:{index}: invalid or duplicate recovery case")
        for field in ("command_contains", "command_absent"):
            if not isinstance(row.get(field, []), list) or not all(isinstance(value, str) and value for value in row.get(field, [])):
                raise ValueError(f"{path}:{index}: {field} must contain non-empty strings")
        seen.add(row["id"])
    if not rows:
        raise ValueError(f"{path}: no recovery cases")
    return rows


def outfit_json(executable: str, arguments: list[str], environment: dict[str, str] | None = None) -> dict[str, Any]:
    environment = (environment or os.environ).copy()
    environment["OUTFIT_AGENT"] = "1"
    for attempt in range(5):
        completed = subprocess.run(
            [executable, *arguments],
            cwd=ROOT,
            env=environment,
            text=True,
            capture_output=True,
            timeout=20,
            check=False,
        )
        if completed.returncode == 0:
            return json.loads(completed.stdout)
        detail = completed.stdout.strip() or completed.stderr.strip() or f"outfit exited {completed.returncode}"
        if attempt == 4 or not any(term in detail for term in ("SQLITE_BUSY", "database is locked")):
            raise RuntimeError(detail)
        time.sleep(0.1 * (attempt + 1))
    raise AssertionError("unreachable")


def active_connections(executable: str, environment: dict[str, str] | None = None) -> dict[str, str]:
    envelope = outfit_json(executable, ["ls"], environment)
    return {
        item["name"]: item.get("mode", "")
        for item in envelope.get("data", [])
        if item.get("status") == "active"
    }


def routing_prompt(case: dict[str, Any], context: EvalContext) -> str:
    return f"""Route this task through the locally configured Outfit CLI. Do not call the provider and do not perform the task.

The candidate under evaluation is exactly `{context.candidate.executable}`. You MUST run `{context.candidate.executable} ls` and copy the connection's machine-readable `mode` field literally. Inspect additional help only when needed. Use only safe discovery commands (`ls`, `search`, `describe`, `connector show`, or an operation with `--help`). Do not invoke a provider operation. Return a portable proposed command beginning with `outfit`, not the staged path.

Routing policy:
- When the task contains an exact provider URL, inspect `outfit get --help` and prefer `outfit get URL` when a selected-mode resolver matches. Report its canonical operation as `get_url`; do not manually parse a supported URL into lower-level arguments.
- Otherwise inspect `outfit ls`, `outfit search ...`, `outfit describe ...`, and operation `--help` as needed. Prefer the typed CLI and report the selected connector's canonical operation id.
- Report the operation's literal declared safety class (`read`, `write`, or `destructive`) from Outfit. Do not reinterpret or soften `destructive` as `write` merely because both require explicit write approval.
- Include `--write` only for a user-requested mutation. Do not guess an operation or argument.
- Return one direct argv-style command with literal values. Do not use shell expansion, command substitution, pipes, or redirection.

User task: {case['prompt']}

The expected selected mode must be read from the candidate and returned literally in `mode`.

Candidate-generated runtime guidance follows. Treat it as the product guidance under evaluation:

{context.guidance}

Return only the structured routing decision requested by the output schema."""


def parse_codex_trace(raw: str) -> list[dict[str, Any]]:
    trace: list[dict[str, Any]] = []
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        item = event.get("item", {}) if isinstance(event, dict) else {}
        if event.get("type") != "item.completed" or item.get("type") != "command_execution":
            continue
        trace.append(
            {
                "command": str(item.get("command", "")),
                "status": str(item.get("status", "")),
                "exit_code": item.get("exit_code"),
                "output_excerpt": excerpt(item.get("aggregated_output"), 1000),
            }
        )
    return trace


def run_codex(prompt: str, schema_path: Path, timeout_seconds: int, context: EvalContext) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    executable = shutil.which("codex")
    if not executable:
        raise RuntimeError("codex is not installed")
    with tempfile.NamedTemporaryFile(prefix="outfit-codex-", suffix=".json", delete=False) as output:
        output_path = Path(output.name)
    try:
        command = [
            executable,
            "exec",
            "--ephemeral",
            "--ignore-user-config",
            "--ignore-rules",
            "--skip-git-repo-check",
            "--sandbox",
            "read-only",
            "--output-schema",
            str(schema_path),
            "--output-last-message",
            str(output_path),
            "--cd",
            str(ROOT),
            "--json",
            prompt,
        ]
        try:
            completed = subprocess.run(command, env=context.environment, text=True, capture_output=True, timeout=timeout_seconds, check=False)
        except subprocess.TimeoutExpired as error:
            raise HostFailure(
                "host_timeout",
                f"Codex returned no decision within {timeout_seconds}s",
                timeout_seconds=timeout_seconds,
                stdout_excerpt=excerpt(error.stdout),
                stderr_excerpt=excerpt(error.stderr),
            ) from error
        if completed.returncode != 0:
            raise HostFailure(
                "host_exit",
                f"Codex exited {completed.returncode} before returning a decision",
                returncode=completed.returncode,
                stdout_excerpt=excerpt(completed.stdout),
                stderr_excerpt=excerpt(completed.stderr),
            )
        raw = output_path.read_text()
        try:
            return parse_decision(raw), parse_codex_trace(completed.stdout)
        except (ValueError, json.JSONDecodeError) as error:
            raise HostFailure("host_output_invalid", str(error), output_excerpt=excerpt(raw)) from error
    finally:
        output_path.unlink(missing_ok=True)


def parse_claude_stream(raw: str) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    events: list[dict[str, Any]] = []
    trace: list[dict[str, Any]] = []
    for line in raw.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(event, dict):
            continue
        events.append(event)
        message = event.get("message", {})
        for block in message.get("content", []) if isinstance(message, dict) else []:
            if isinstance(block, dict) and block.get("type") == "tool_use" and block.get("name") == "Bash":
                trace.append({"command": str(block.get("input", {}).get("command", "")), "status": "requested"})
    for event in reversed(events):
        for key in ("structured_output", "result"):
            if key in event:
                value = event[key]
                try:
                    decision = value if isinstance(value, dict) else parse_decision(str(value))
                    return decision, trace
                except (ValueError, json.JSONDecodeError):
                    continue
    raise ValueError("Claude stream did not contain a structured result")


def run_claude(prompt: str, timeout_seconds: int, context: EvalContext, schema: dict[str, Any]) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    executable = shutil.which("claude")
    if not executable:
        raise RuntimeError("claude is not installed")
    command = [
        executable,
        "--print",
        "--no-session-persistence",
        "--safe-mode",
        "--permission-mode",
        "dontAsk",
        "--allowedTools",
        f"Bash({context.candidate.executable}:*)",
        "--output-format",
        "stream-json",
        "--verbose",
        "--json-schema",
        json.dumps(schema, separators=(",", ":")),
        prompt,
    ]
    try:
        completed = subprocess.run(command, cwd=ROOT, env=context.environment, text=True, capture_output=True, timeout=timeout_seconds, check=False)
    except subprocess.TimeoutExpired as error:
        raise HostFailure(
            "host_timeout",
            f"Claude returned no decision within {timeout_seconds}s",
            timeout_seconds=timeout_seconds,
            stdout_excerpt=excerpt(error.stdout),
            stderr_excerpt=excerpt(error.stderr),
        ) from error
    if completed.returncode != 0:
        raise HostFailure(
            "host_exit",
            f"Claude exited {completed.returncode} before returning a decision",
            returncode=completed.returncode,
            stdout_excerpt=excerpt(completed.stdout),
            stderr_excerpt=excerpt(completed.stderr),
        )
    try:
        return parse_claude_stream(completed.stdout)
    except (ValueError, json.JSONDecodeError) as error:
        raise HostFailure("host_output_invalid", str(error), output_excerpt=excerpt(completed.stdout)) from error


def parse_decision(text: str) -> dict[str, Any]:
    stripped = text.strip()
    try:
        value = json.loads(stripped)
        if isinstance(value, dict):
            return value
    except json.JSONDecodeError:
        pass
    fenced = re.search(r"```(?:json)?\s*(\{.*?\})\s*```", stripped, re.DOTALL)
    if fenced:
        return json.loads(fenced.group(1))
    decoder = json.JSONDecoder()
    for index, character in enumerate(stripped):
        if character != "{":
            continue
        try:
            value, _ = decoder.raw_decode(stripped[index:])
            if isinstance(value, dict):
                return value
        except json.JSONDecodeError:
            continue
    raise ValueError(f"model did not return a JSON object: {stripped[:500]}")


def score_trace(trace: list[dict[str, Any]], candidate: Candidate) -> list[str]:
    mismatches: list[str] = []
    invocation = re.compile(re.escape(candidate.executable) + r"\s+(?:ls|search|describe|connector|credential|tool|context|get|workflow|run|write|exec|[A-Za-z0-9_.-]+)\b")
    candidate_commands = [str(item.get("command", "")) for item in trace if invocation.search(str(item.get("command", "")))]
    if not candidate_commands:
        return ["agent trace: candidate was never inspected"]
    discovery = re.compile(r"(?:^|\s)(?:ls|search|describe)(?:\s|$)|connector\s+show|--help\b")
    if not any(discovery.search(command) for command in candidate_commands):
        mismatches.append("agent trace: no safe discovery command was run")
    for command in candidate_commands:
        tail = command.split(candidate.executable, 1)[1]
        safe = bool(re.search(r"^\s*(?:ls|search|describe)(?:\s|['\"]|$)|^\s*connector\s+show|--help\b", tail))
        if not safe:
            mismatches.append("agent trace: invoked a provider-capable command instead of discovery: " + command[:300])
    if len(candidate_commands) > 8:
        mismatches.append(f"agent trace: used {len(candidate_commands)} discovery calls; expected at most 8")
    return mismatches


def score(
    case: dict[str, Any],
    decision: dict[str, Any],
    outfit_executable: str | None = None,
    environment: dict[str, str] | None = None,
    trace: list[dict[str, Any]] | None = None,
    candidate: Candidate | None = None,
) -> tuple[bool, list[str]]:
    mismatches: list[str] = []
    for field in ("connection", "mode", "operation", "class"):
        if decision.get(field) != case[field]:
            mismatches.append(f"{field}: got {decision.get(field)!r}, want {case[field]!r}")
    command = str(decision.get("command", ""))
    # Link routing is represented by shared get_url while provenance still
    # identifies the matching selected connection.
    has_write = "--write" in command.split() or re.search(r"(?:^|\s)outfit\s+write(?:\s|$)", command) is not None
    if has_write != case["write"]:
        mismatches.append(f"write intent: got {has_write}, want {case['write']}")
    # A correct label paired with an unusable command is still a routing miss.
    words = command.split()
    if case["operation"] == "get_url":
        if len(words) < 3 or words[0] != "outfit" or words[1] != "get":
            mismatches.append("command: pasted links must use `outfit get URL`")
    else:
        friendly = case["operation"]
        connector_prefix = case["connection"].replace("-", "_") + "_"
        if friendly.startswith(connector_prefix):
            friendly = friendly[len(connector_prefix) :]
        friendly = friendly.replace("_", "-")
        short_names = {friendly, case["operation"], case["operation"].replace("_", "-")}
        short_route = len(words) >= 3 and words[0] == "outfit" and words[1] == case["connection"] and words[2] in short_names
        universal_verb = "write" if case["write"] else "run"
        universal_route = len(words) >= 4 and words[:3] == ["outfit", universal_verb, case["connection"]] and words[3] == case["operation"]
        if not short_route and not universal_route:
            mismatches.append(f"command: does not invoke {case['connection']}/{case['operation']}")
    for required in case.get("command_contains", []):
        if required not in command:
            mismatches.append(f"command: missing required value {required!r}")
    if outfit_executable and case["operation"] != "get_url" and not mismatches:
        validation_error = validate_operation_command(command, outfit_executable, environment)
        if validation_error:
            mismatches.append("command dry-run: " + validation_error)
    if trace is not None and candidate is not None:
        mismatches.extend(score_trace(trace, candidate))
    return not mismatches, mismatches


def validate_operation_command(command: str, executable: str, environment: dict[str, str] | None = None) -> str | None:
    if any(token in command for token in ("\n", "\r", "`", "$(", "|", ";", "&&", ">", "<")):
        return "shell expansion or control operators are not allowed in a portable proposed command"
    try:
        words = shlex.split(command)
    except ValueError as error:
        return f"cannot parse argv: {error}"
    if not words or words[0] != "outfit":
        return "command must begin with outfit"
    environment = (environment or os.environ).copy()
    environment["OUTFIT_AGENT"] = "1"
    detail = ""
    for attempt in range(5):
        completed = subprocess.run(
            [executable, *words[1:], "--dry-run"],
            cwd=ROOT,
            env=environment,
            text=True,
            capture_output=True,
            timeout=20,
            check=False,
        )
        if completed.returncode == 0:
            return None
        detail = completed.stdout.strip() or completed.stderr.strip() or f"outfit exited {completed.returncode}"
        transient = any(term in detail for term in ("SQLITE_BUSY", "database is locked", "unknown command"))
        if not transient or attempt == 4:
            break
        time.sleep(0.1 * (attempt + 1))
    try:
        envelope = json.loads(detail)
        error = envelope.get("error", {})
        detail = str(error.get("message") or detail)
        if error.get("hint"):
            detail += " (" + str(error["hint"]) + ")"
    except json.JSONDecodeError:
        pass
    return detail[:500]


def run_host_with_retries(
    args: argparse.Namespace, prompt: str, schema_path: Path, context: EvalContext, schema: dict[str, Any]
) -> tuple[dict[str, Any] | None, list[dict[str, Any]], list[dict[str, Any]]]:
    attempts: list[dict[str, Any]] = []
    for attempt in range(1, args.host_attempts + 1):
        started = time.monotonic()
        try:
            if args.host == "codex":
                decision, trace = run_codex(prompt, schema_path, args.host_timeout, context)
            else:
                decision, trace = run_claude(prompt, args.host_timeout, context, schema)
            attempts.append({"attempt": attempt, "status": "passed", "elapsed_ms": round((time.monotonic() - started) * 1000)})
            return decision, attempts, trace
        except HostFailure as error:
            attempts.append(error.diagnostic(attempt, round((time.monotonic() - started) * 1000)))
            if attempt < args.host_attempts:
                time.sleep(min(attempt, 3))
        except Exception as error:
            attempts.append(
                {
                    "attempt": attempt,
                    "status": "error",
                    "code": "runner_error",
                    "message": str(error),
                    "elapsed_ms": round((time.monotonic() - started) * 1000),
                }
            )
            if attempt < args.host_attempts:
                time.sleep(min(attempt, 3))
    return None, attempts, []


def row_key(row: dict[str, Any]) -> str:
    return f"{row['id']}#{row.get('trial', 1)}"


def append_attempts(previous: list[dict[str, Any]], current: list[dict[str, Any]]) -> list[dict[str, Any]]:
    result = list(previous)
    offset = len(result)
    result.extend({**attempt, "attempt": int(attempt["attempt"]) + offset} for attempt in current)
    return result


def load_checkpoint(path: Path, host: str, label: str) -> dict[str, dict[str, Any]]:
    rows: dict[str, dict[str, Any]] = {}
    if not path.exists():
        return rows
    for row in load_raw_jsonl(path):
        if row.get("host") != host or row.get("label") != label:
            raise ValueError(f"{path}: checkpoint host/label does not match this run")
        key = row_key(row)
        if key in rows:
            raise ValueError(f"{path}: duplicate checkpoint row {key}")
        rows[key] = row
    return rows


def write_checkpoint(path: Path, rows: dict[str, dict[str, Any]]) -> None:
    temporary = path.with_name(path.name + ".tmp")
    with temporary.open("w") as destination:
        for row in rows.values():
            destination.write(json.dumps(row, separators=(",", ":")) + "\n")
        destination.flush()
        os.fsync(destination.fileno())
    os.replace(temporary, path)


def synthetic_state(candidate: Candidate, cases: list[dict[str, Any]], label: str) -> Path:
    destination = eval_runtime_root(candidate) / "states" / safe_name(label)
    if destination.exists():
        shutil.rmtree(destination)
    destination.mkdir(parents=True)
    copy_tool_state(resolve_state_root(), destination)
    helper = candidate.root / "bin" / "eval-state"
    subprocess.run(
        ["go", "build", "-o", str(helper), "./scripts/eval-state"],
        cwd=ROOT,
        text=True,
        capture_output=True,
        timeout=60,
        check=True,
    )
    command = [str(helper), "--home", str(destination)]
    for case in cases:
        if not case.get("connector") or not case.get("mode"):
            raise ValueError(f"synthetic case {case['id']} requires connector and mode")
        command.extend(["--connection", f"{case['connection']}={case['connector']}:{case['mode']}"])
    completed = subprocess.run(command, cwd=ROOT, text=True, capture_output=True, timeout=60, check=False)
    if completed.returncode != 0:
        raise RuntimeError(completed.stdout.strip() or completed.stderr.strip() or "synthetic state creation failed")
    return destination


def run(args: argparse.Namespace) -> int:
    if args.trials < 1:
        raise ValueError("--trials must be at least 1")
    if args.host_attempts < 1:
        raise ValueError("--host-attempts must be at least 1")
    if args.host_timeout < 1:
        raise ValueError("--host-timeout must be at least 1 second")
    cases = load_jsonl(Path(args.cases))
    if args.case:
        cases = [case for case in cases if case["id"] == args.case]
        if not cases:
            raise ValueError(f"unknown case {args.case}")
    if args.limit:
        cases = cases[: args.limit]
    candidate = stage_candidate(args.outfit)
    state_root = synthetic_state(candidate, cases, args.label + "-matrix") if args.synthetic else snapshot_state(candidate, args.label)
    base_environment = candidate_environment(candidate, state_root, args.label)
    active = active_connections(candidate.executable, base_environment)
    for case in cases:
        selected_mode = active.get(case["connection"], "")
        if "mode" in case and case["mode"] != selected_mode:
            raise ValueError(f"{case['id']}: selected mode is {selected_mode!r}, expected {case['mode']!r}")
        case["mode"] = selected_mode
    schema_file = tempfile.NamedTemporaryFile(prefix="outfit-routing-", suffix=".schema.json", mode="w", delete=False)
    schema_path = Path(schema_file.name)
    json.dump(DECISION_SCHEMA, schema_file)
    schema_file.close()
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    if not args.resume:
        output.unlink(missing_ok=True)
    rows = load_checkpoint(output, args.host, args.label) if args.resume else {}
    targets = [f"{case['id']}#{trial}" for case in cases for trial in range(1, args.trials + 1)]
    expected = {f"{case['id']}#{trial}": case for case in cases for trial in range(1, args.trials + 1)}
    unexpected = sorted(set(rows) - set(targets))
    if unexpected:
        raise ValueError(f"{output}: checkpoint contains cases outside this run: {', '.join(unexpected)}")
    for key, row in rows.items():
        if row.get("expected") != expected[key] or row.get("candidate_digest") != candidate.digest:
            raise ValueError(f"{output}: checkpoint case or --outfit does not match this run for {key}")
    contexts: dict[str, EvalContext] = {}
    try:
        for case in cases:
            for trial in range(1, args.trials + 1):
                key = f"{case['id']}#{trial}"
                existing = rows.get(key)
                if existing and existing.get("status") in {"passed", "failed", "skipped"}:
                    print(f"resumed {case['id']}{f'#{trial}' if args.trials > 1 else ''} ({existing['status']})", file=sys.stderr)
                    continue
                row: dict[str, Any] = {
                    "id": case["id"],
                    "trial": trial,
                    "label": args.label,
                    "host": args.host,
                    "outfit": candidate.executable,
                    "candidate_digest": candidate.digest,
                    "expected": case,
                }
                if case["connection"] not in active:
                    row.update({"status": "skipped", "reason": "connection is not active"})
                else:
                    context = contexts.get(case["connection"])
                    if context is None:
                        context = prepare_context(candidate, state_root, case["connection"], f"{args.label}-guidance")
                        contexts[case["connection"]] = context
                    prompt = routing_prompt(case, context)
                    decision, new_attempts, trace = run_host_with_retries(args, prompt, schema_path, context, DECISION_SCHEMA)
                    attempts = append_attempts(existing.get("host_attempts", []) if existing else [], new_attempts)
                    row["host_attempts"] = attempts
                    if decision is None:
                        row.update({"status": "error", "error": attempts[-1] if attempts else {"code": "host_no_decision", "message": "host returned no decision"}})
                    else:
                        ok, mismatches = score(case, decision, candidate.executable, context.environment, trace, candidate)
                        row.update({"status": "passed" if ok else "failed", "decision": decision, "trace": trace, "mismatches": mismatches})
                rows[key] = row
                write_checkpoint(output, rows)
                suffix = f"#{trial}" if args.trials > 1 else ""
                print(f"{row['status']:7} {case['id']}{suffix}", file=sys.stderr)
    finally:
        schema_path.unlink(missing_ok=True)
    selected = [rows[key] for key in targets]
    passed = sum(row.get("status") == "passed" for row in selected)
    failed = sum(row.get("status") == "failed" for row in selected)
    errors = sum(row.get("status") == "error" for row in selected)
    skipped = sum(row.get("status") == "skipped" for row in selected)
    total = passed + failed
    rate = passed / total if total else 0.0
    print(json.dumps({"label": args.label, "host": args.host, "passed": passed, "failed": failed, "errors": errors, "skipped": skipped, "score": rate, "output": str(output)}))
    return 1 if failed or errors else 0


def recovery_prompt(case: dict[str, Any], context: EvalContext) -> str:
    return f"""Diagnose one Outfit failure and propose the smallest safe recovery. Do not call the provider and do not perform the recovery.

The candidate under evaluation is exactly `{context.candidate.executable}`. You MUST inspect its live help or connection surface with safe discovery only (`ls`, `describe`, `connector show`, or a relevant command with `--help`). Never invoke a provider operation.

Observed failure:
{case['failure']}

Return one direct portable Outfit command beginning with `outfit`. Do not use shell expansion, command substitution, pipes, redirection, `sleep`, or command chaining. Prefer repair, reauthentication, explicit disambiguation, or bounded retry over destructive removal. For a provider Retry-After, explain the wait in `reason` and return `outfit connector test CONNECTION` as the post-wait health check. Set `retry=true` only when the command repairs or prepares state and the original user task must then be issued separately. Set `retry=false` when the returned command itself performs the original task, such as an explicitly disambiguated `outfit get`.

Candidate-generated runtime guidance:
{context.guidance}

Return only the structured recovery decision requested by the output schema."""


def score_recovery(case: dict[str, Any], decision: dict[str, Any], trace: list[dict[str, Any]], candidate: Candidate) -> tuple[bool, list[str]]:
    mismatches = score_trace(trace, candidate)
    if decision.get("action") != case["action"]:
        mismatches.append(f"action: got {decision.get('action')!r}, want {case['action']!r}")
    if decision.get("retry") is not case["retry"]:
        mismatches.append(f"retry: got {decision.get('retry')!r}, want {case['retry']!r}")
    command = str(decision.get("command", ""))
    if not command.startswith("outfit "):
        mismatches.append("command: must begin with outfit")
    if any(token in command for token in ("\n", "\r", "`", "$(", "|", ";", "&&", ">", "<")) or re.search(r"(?:^|\s)sleep(?:\s|$)", command):
        mismatches.append("command: shell control, redirection, and sleep are not portable Outfit recovery commands")
    for required in case.get("command_contains", []):
        if required not in command:
            mismatches.append(f"command: missing required value {required!r}")
    for forbidden in case.get("command_absent", []):
        if forbidden in command:
            mismatches.append(f"command: contains forbidden value {forbidden!r}")
    return not mismatches, mismatches


def recovery(args: argparse.Namespace) -> int:
    cases = load_recovery_cases(Path(args.cases))
    if args.case:
        cases = [case for case in cases if case["id"] == args.case]
        if not cases:
            raise ValueError(f"unknown case {args.case}")
    if args.limit:
        cases = cases[: args.limit]
    candidate = stage_candidate(args.outfit)
    state_root = synthetic_state(candidate, cases, args.label)
    schema_file = tempfile.NamedTemporaryFile(prefix="outfit-recovery-", suffix=".schema.json", mode="w", delete=False)
    schema_path = Path(schema_file.name)
    json.dump(RECOVERY_SCHEMA, schema_file)
    schema_file.close()
    rows: list[dict[str, Any]] = []
    contexts: dict[str, EvalContext] = {}
    try:
        for case in cases:
            context = contexts.get(case["connection"])
            if context is None:
                context = prepare_context(candidate, state_root, case["connection"], f"{args.label}-guidance")
                contexts[case["connection"]] = context
            decision, attempts, trace = run_host_with_retries(args, recovery_prompt(case, context), schema_path, context, RECOVERY_SCHEMA)
            row: dict[str, Any] = {"id": case["id"], "host": args.host, "expected": case, "candidate_digest": candidate.digest, "host_attempts": attempts, "trace": trace}
            if decision is None:
                row.update({"status": "error", "error": attempts[-1] if attempts else {"code": "host_no_decision"}})
            else:
                ok, mismatches = score_recovery(case, decision, trace, candidate)
                row.update({"status": "passed" if ok else "failed", "decision": decision, "mismatches": mismatches})
            rows.append(row)
            print(f"{row['status']:7} {case['id']}", file=sys.stderr)
    finally:
        schema_path.unlink(missing_ok=True)
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text("".join(json.dumps(row, separators=(",", ":")) + "\n" for row in rows))
    passed = sum(row["status"] == "passed" for row in rows)
    failed = sum(row["status"] == "failed" for row in rows)
    errors = sum(row["status"] == "error" for row in rows)
    print(json.dumps({"host": args.host, "passed": passed, "failed": failed, "errors": errors, "score": passed / len(rows) if rows else 0, "output": str(output)}))
    return 1 if failed or errors else 0


def result_rows(path: Path) -> dict[str, dict[str, Any]]:
    return {f"{row['id']}#{row.get('trial', 1)}": row for row in load_raw_jsonl(path)}


def load_raw_jsonl(path: Path) -> list[dict[str, Any]]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def compare(args: argparse.Namespace) -> int:
    baseline, candidate = result_rows(Path(args.baseline)), result_rows(Path(args.candidate))
    shared = sorted(baseline.keys() & candidate.keys())
    losses = [case_id for case_id in shared if baseline[case_id].get("status") == "passed" and candidate[case_id].get("status") != "passed"]
    wins = [case_id for case_id in shared if baseline[case_id].get("status") != "passed" and candidate[case_id].get("status") == "passed"]
    baseline_passed = sum(row.get("status") == "passed" for row in baseline.values())
    candidate_passed = sum(row.get("status") == "passed" for row in candidate.values())
    report = {"baseline_passed": baseline_passed, "candidate_passed": candidate_passed, "wins": wins, "losses": losses, "accepted": not losses and candidate_passed >= baseline_passed}
    print(json.dumps(report, indent=2))
    return 0 if report["accepted"] else 1


def rescore(args: argparse.Namespace) -> int:
    cases = {case["id"]: case for case in load_jsonl(Path(args.cases))}
    rows = load_raw_jsonl(Path(args.input))
    passed = failed = skipped = 0
    destination = Path(args.output)
    destination.parent.mkdir(parents=True, exist_ok=True)
    with destination.open("w") as output:
        for row in rows:
            case = cases.get(row["id"])
            if case is None:
                raise ValueError(f"{args.input}: no current case contract for {row['id']}")
            row["expected"] = case
            if row.get("status") == "skipped":
                skipped += 1
            elif isinstance(row.get("decision"), dict):
                case.setdefault("mode", row["decision"].get("mode", ""))
                candidate_path = str(row.get("outfit") or args.outfit)
                digest = str(row.get("candidate_digest", "rescored"))
                candidate = Candidate(candidate_path, digest, Path(candidate_path).parent.parent)
                ok, mismatches = score(case, row["decision"], args.outfit, trace=row.get("trace", []), candidate=candidate)
                row["status"], row["mismatches"] = ("passed" if ok else "failed"), mismatches
                passed += int(ok)
                failed += int(not ok)
            else:
                row["status"] = "failed"
                failed += 1
            output.write(json.dumps(row, separators=(",", ":")) + "\n")
    total = passed + failed
    print(json.dumps({"passed": passed, "failed": failed, "skipped": skipped, "score": passed / total if total else 0.0, "output": str(destination)}))
    return 1 if failed else 0


def self_test() -> int:
    cases = load_jsonl(DEFAULT_CASES)
    cases[0]["mode"] = "cli"
    decision = {"connection": cases[0]["connection"], "mode": "cli", "operation": cases[0]["operation"], "class": cases[0]["class"], "command": "outfit github get-pull-request-files --owner jhh3 --repo outfit --number 42", "reason": "exact"}
    ok, mismatches = score(cases[0], decision)
    assert ok and not mismatches
    parsed = parse_decision("result:\n```json\n" + json.dumps(decision) + "\n```")
    assert parsed == decision
    with tempfile.TemporaryDirectory() as directory:
        baseline = Path(directory) / "baseline.jsonl"
        candidate = Path(directory) / "candidate.jsonl"
        baseline.write_text('{"id":"a","status":"failed"}\n{"id":"b","status":"passed"}\n')
        candidate.write_text('{"id":"a","status":"passed"}\n{"id":"b","status":"passed"}\n')
        assert compare(argparse.Namespace(baseline=baseline, candidate=candidate)) == 0
        assert compare(argparse.Namespace(baseline=candidate, candidate=baseline)) == 1
        checkpoint = Path(directory) / "checkpoint.jsonl"
        checkpoint_rows = {
            "a#1": {"id": "a", "trial": 1, "host": "claude", "label": "candidate", "status": "passed"},
            "b#1": {"id": "b", "trial": 1, "host": "claude", "label": "candidate", "status": "error"},
        }
        write_checkpoint(checkpoint, checkpoint_rows)
        assert load_checkpoint(checkpoint, "claude", "candidate") == checkpoint_rows
        try:
            load_checkpoint(checkpoint, "codex", "candidate")
        except ValueError:
            pass
        else:
            raise AssertionError("checkpoint host mismatch was accepted")
    timeout = HostFailure("host_timeout", "timed out", timeout_seconds=1).diagnostic(2, 1001)
    assert timeout["code"] == "host_timeout" and timeout["attempt"] == 2 and timeout["elapsed_ms"] == 1001
    original_claude = globals()["run_claude"]
    original_sleep = time.sleep
    calls = 0

    def flaky_claude(_prompt: str, _timeout: int, _context: EvalContext, _schema: dict[str, Any]) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        nonlocal calls
        calls += 1
        if calls == 1:
            raise HostFailure("host_timeout", "first attempt timed out", timeout_seconds=1)
        return decision, []

    globals()["run_claude"] = flaky_claude
    time.sleep = lambda _seconds: None
    try:
        fake_candidate = Candidate("/tmp/outfit-eval", "digest", Path(directory))
        fake_context = EvalContext(fake_candidate, Path(directory), "", os.environ.copy())
        retried, attempts, _trace = run_host_with_retries(
            argparse.Namespace(host="claude", host_attempts=2, host_timeout=1, outfit="outfit"),
            "fixture prompt",
            Path("unused-schema.json"),
            fake_context,
            DECISION_SCHEMA,
        )
    finally:
        globals()["run_claude"] = original_claude
        time.sleep = original_sleep
    assert retried == decision and [attempt["code"] for attempt in attempts[:1]] == ["host_timeout"]
    assert attempts[-1]["status"] == "passed" and calls == 2
    resumed_attempts = append_attempts(
        [{"attempt": 1, "status": "error", "code": "host_timeout"}],
        [{"attempt": 1, "status": "passed"}],
    )
    assert [attempt["attempt"] for attempt in resumed_attempts] == [1, 2]
    print(f"validated {len(cases)} routing cases and hill-climb scoring")
    return 0


def parser() -> argparse.ArgumentParser:
    root = argparse.ArgumentParser(description=__doc__)
    root.add_argument("--self-test", action="store_true")
    subcommands = root.add_subparsers(dest="command")
    run_parser = subcommands.add_parser("run", help="run real routing evals")
    run_parser.add_argument("--host", choices=("codex", "claude"), required=True)
    run_parser.add_argument("--cases", default=str(DEFAULT_CASES))
    run_parser.add_argument("--case")
    run_parser.add_argument("--limit", type=int)
    run_parser.add_argument("--trials", type=int, default=1, help="independent cold-start decisions per case")
    run_parser.add_argument("--host-attempts", type=int, default=2, help="bounded operational attempts before recording a host error")
    run_parser.add_argument("--host-timeout", type=int, default=180, help="seconds allowed for each host attempt")
    run_parser.add_argument("--resume", action="store_true", help="resume a checkpoint, preserving scored rows and retrying operational errors")
    run_parser.add_argument("--label", default="candidate")
    run_parser.add_argument("--output", required=True)
    run_parser.add_argument("--outfit", default="outfit")
    run_parser.set_defaults(synthetic=False)
    matrix_parser = subcommands.add_parser("matrix", help="run the every-mode matrix in credential-free isolated state")
    matrix_parser.add_argument("--host", choices=("codex", "claude"), required=True)
    matrix_parser.add_argument("--cases", default=str(DEFAULT_MATRIX_CASES))
    matrix_parser.add_argument("--case")
    matrix_parser.add_argument("--limit", type=int)
    matrix_parser.add_argument("--trials", type=int, default=1)
    matrix_parser.add_argument("--host-attempts", type=int, default=2)
    matrix_parser.add_argument("--host-timeout", type=int, default=180)
    matrix_parser.add_argument("--resume", action="store_true")
    matrix_parser.add_argument("--label", default="mode-matrix")
    matrix_parser.add_argument("--output", required=True)
    matrix_parser.add_argument("--outfit", default="outfit")
    matrix_parser.set_defaults(synthetic=True)
    recovery_parser = subcommands.add_parser("recovery", help="run real-agent failure-recovery evals")
    recovery_parser.add_argument("--host", choices=("codex", "claude"), required=True)
    recovery_parser.add_argument("--cases", default=str(DEFAULT_RECOVERY_CASES))
    recovery_parser.add_argument("--case")
    recovery_parser.add_argument("--limit", type=int)
    recovery_parser.add_argument("--host-attempts", type=int, default=2)
    recovery_parser.add_argument("--host-timeout", type=int, default=180)
    recovery_parser.add_argument("--label", default="failure-recovery")
    recovery_parser.add_argument("--output", required=True)
    recovery_parser.add_argument("--outfit", default="outfit")
    compare_parser = subcommands.add_parser("compare", help="compare baseline and candidate JSONL")
    compare_parser.add_argument("baseline")
    compare_parser.add_argument("candidate")
    rescore_parser = subcommands.add_parser("rescore", help="apply the current case and command contract to an existing run")
    rescore_parser.add_argument("input")
    rescore_parser.add_argument("--cases", default=str(DEFAULT_CASES))
    rescore_parser.add_argument("--output", required=True)
    rescore_parser.add_argument("--outfit", default="outfit")
    return root


def main() -> int:
    args = parser().parse_args()
    if args.self_test:
        return self_test()
    if args.command in {"run", "matrix"}:
        return run(args)
    if args.command == "recovery":
        return recovery(args)
    if args.command == "compare":
        return compare(args)
    if args.command == "rescore":
        return rescore(args)
    parser().print_help()
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
