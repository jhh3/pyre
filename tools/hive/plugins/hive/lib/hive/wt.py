"""hive wt — worktree, tmux session, and stream birth/death (PLAN §2).

The zsh shim (shims/wt.zsh) keeps only what must live in the shell: the final
`cd` and completion. Everything else runs here so wt, `ws spawn`, cron
dispatch, and cross-machine opens share one code path.

Constraints that shape this module:

- `--print-path` output is machine-read by the shim (`cd "$(…)"`), so in that
  mode stdout carries exactly one line — the worktree path — and every other
  byte goes to stderr. Informational output goes to stderr unconditionally;
  only machine-read results (ls rows, picked path, repo names) use stdout.
- Birth registration (stream + ledger) is strictly best-effort: a broken
  vault or a not-yet-built ledger module must never fail the worktree verb.
- `rm` is the archive-before-destroy verb: close pipeline first, destruction
  second, and the branch is never deleted non-interactively without --yes.
- Every external command (git/tmux/fzf/setup recipes) routes through _sh so
  tests can stub the world out; nothing shells out at import time.
"""

from __future__ import annotations

import argparse
import getpass
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path

from . import config, util, vendored

_LINEAR_URL = re.compile(r"^https://linear\.app/[^/]+/issue/([^/]+)")
_FALLBACK_REPO = Path.home() / "code" / "acme" / "webapp"
_CLAUDE = "claude --dangerously-skip-permissions"


class WtError(Exception):
    """User-facing failure; run() prints it and returns 1."""


# -- naming ------------------------------------------------------------------

def is_linear(intake: str) -> bool:
    return intake.startswith("https://linear.app/") and "/issue/" in intake


def parse_linear(url: str) -> str:
    """Linear issue URL -> branch: …/issue/ENG-1234/slug -> <prefix>/eng-1234.

    The prefix is $WT_BRANCH_PREFIX, else the login name.
    """
    m = _LINEAR_URL.match(url)
    if not m:
        raise WtError(f"could not parse issue ID from {url}")
    return f"{_branch_prefix()}/{m.group(1).lower()}"


def _branch_prefix() -> str:
    return os.environ.get("WT_BRANCH_PREFIX") or getpass.getuser()


def dir_name(branch: str) -> str:
    """Worktree directory name: branch slashes become dashes."""
    return branch.replace("/", "-")


def session_name(dname: str) -> str:
    """tmux session names cannot contain '.' or ':' (also the stream slug —
    one namespace: tmux session = stream slug = worktree dir)."""
    return dname.replace(".", "-").replace(":", "-")


# -- subprocess seam ---------------------------------------------------------

def _sh(cmd: list[str], cwd: Path | None = None, capture: bool = False,
        out=None, env_extra: dict | None = None,
        input_text: str | None = None) -> subprocess.CompletedProcess:
    """Single seam for git/tmux/fzf/setup commands (tests replace this).

    `out` redirects child stdout (print-path mode sends it to stderr); the
    default inherits the terminal so attach/fzf/setup stay interactive.
    """
    env = dict(os.environ, **env_extra) if env_extra else None
    kwargs: dict = {"cwd": str(cwd) if cwd else None, "text": True, "env": env}
    if capture:
        kwargs["capture_output"] = True
    elif out is not None:
        kwargs["stdout"] = out
    if input_text is not None:
        kwargs["input"] = input_text
    try:
        return subprocess.run(cmd, **kwargs)
    except FileNotFoundError as e:
        raise WtError(f"missing command: {cmd[0]}") from e


def _info(msg: str) -> None:
    print(msg, file=sys.stderr)


# -- repo registry -----------------------------------------------------------

def resolve_repo(repo_arg: str | None) -> tuple[str, Path, dict]:
    """Resolve --repo (recipe name, or a path containing .git) to
    (name, root, recipe).

    Default order: recipe marked `default = true` (or literally named
    "default"), then $WT_REPO, then ~/code/acme/webapp.
    """
    recipes = config.repos()
    if repo_arg:
        if repo_arg in recipes:
            return _from_recipe(repo_arg, recipes[repo_arg])
        p = Path(repo_arg).expanduser()
        if (p / ".git").exists():
            return _recipe_for_path(p, recipes)
        known = ", ".join(sorted(recipes)) or "none"
        raise WtError(f"unknown repo '{repo_arg}' (known: {known})")
    for name, recipe in recipes.items():
        if name == "default" or recipe.get("default"):
            return _from_recipe(name, recipe)
    env_repo = os.environ.get("WT_REPO")
    if env_repo:
        return _recipe_for_path(Path(env_repo).expanduser(), recipes)
    return "webapp", _FALLBACK_REPO, recipes.get("webapp", {})


def _from_recipe(name: str, recipe: dict) -> tuple[str, Path, dict]:
    path = recipe.get("path")
    if not path:
        raise WtError(f"repo recipe '{name}' has no path")
    return name, Path(str(path)).expanduser(), recipe


def _recipe_for_path(p: Path, recipes: dict) -> tuple[str, Path, dict]:
    """Match a filesystem path back to its recipe so setup/launch/env apply."""
    rp = p.resolve()
    for name, recipe in recipes.items():
        cand = recipe.get("path")
        if cand and Path(str(cand)).expanduser().resolve() == rp:
            return name, rp, recipe
    return rp.name, rp, {}


def setup_commands(recipe: dict, wt_path: Path) -> list[str]:
    """Shell commands preparing a fresh worktree.

    An explicit `setup` list in the recipe wins; otherwise the wt.zsh
    fallback: `make setup` when a Makefile exists, plus the backend one.
    """
    if recipe.get("setup"):
        return [str(c) for c in recipe["setup"]]
    cmds = []
    if (wt_path / "Makefile").is_file():
        cmds.append("make setup")
    if (wt_path / "backend" / "Makefile").is_file():
        cmds.append("make -C backend setup")
    return cmds


# -- git / tmux --------------------------------------------------------------

def worktrees(root: Path) -> list[tuple[str, Path]]:
    """(branch, path) pairs under <root>/.worktrees, parsed from
    `git worktree list --porcelain` (the main checkout is excluded)."""
    res = _sh(["git", "-C", str(root), "worktree", "list", "--porcelain"],
              capture=True)
    if res.returncode != 0:
        return []
    prefix = str(root / ".worktrees") + os.sep
    out: list[tuple[str, Path]] = []
    cur: Path | None = None
    for line in res.stdout.splitlines():
        if line.startswith("worktree "):
            cur = Path(line[len("worktree "):])
        elif line.startswith("branch refs/heads/") and cur is not None:
            if str(cur).startswith(prefix):
                out.append((line[len("branch refs/heads/"):], cur))
            cur = None
    return out


def _tmux_has(session: str) -> bool:
    return _sh(["tmux", "has-session", f"-t={session}"],
               capture=True).returncode == 0


def _open_tmux(wt_path: Path, session: str, recipe: dict, launch: str,
               detached: bool) -> None:
    if not _tmux_has(session):
        cmd = ["tmux", "new-session", "-d", "-s", session, "-c", str(wt_path)]
        for k, v in (recipe.get("env") or {}).items():
            cmd += ["-e", f"{k}={v}"]
        if _sh(cmd, capture=True).returncode != 0:
            raise WtError(f"tmux new-session failed for {session}")
        _info(f"+ created tmux session {session}")
        if launch:
            _info(f"> launching: {launch}")
            _sh(["tmux", "send-keys", "-t", session, launch, "Enter"],
                capture=True)
    if detached:
        return
    _info(f"-> attaching {session}")
    if os.environ.get("TMUX"):
        _sh(["tmux", "switch-client", "-t", session])
    else:
        _sh(["tmux", "attach", "-t", session])


# -- best-effort hooks (ledger / lifecycle land in M2) -----------------------

def _register_birth(slug: str, branch: str, repo: Path, worktree: Path,
                    tmux_session: str) -> None:
    """Birth = registration (PLAN §2). Never fails the worktree verb."""
    try:
        if not (config.STREAMS_VAULT / "streams" / slug).is_dir():
            vendored.ws(["register", slug, "--title", branch])
    except Exception:
        pass
    try:
        from . import ledger
        ledger.record_wt_birth(slug=slug, repo=str(repo),
                               worktree=str(worktree), branch=branch,
                               tmux_session=tmux_session)
    except Exception:
        pass


def _ledger_annotate(rows: list[dict]) -> list[dict]:
    """Hook point: the ledger (M2) adds live/parked, title, machine and
    last-activity columns to ls rows. Degrades to the plain rows."""
    try:
        from . import ledger
        return ledger.annotate_worktrees(rows)
    except Exception:
        return rows


# -- streams -----------------------------------------------------------------

def _stream_frontmatter(slug: str) -> dict[str, str]:
    md = config.STREAMS_VAULT / "streams" / slug / "STREAM.md"
    try:
        lines = md.read_text().splitlines()
    except OSError:
        raise WtError(f"no stream '{slug}' ({md})")
    if not lines or lines[0].strip() != "---":
        raise WtError(f"stream '{slug}' has no frontmatter")
    fm: dict[str, str] = {}
    for line in lines[1:]:
        if line.strip() == "---":
            break
        key, sep, val = line.partition(":")
        if sep:
            fm[key.strip()] = val.strip().strip('"')
    return fm


def _intake_from_stream(slug: str) -> tuple[str, str | None]:
    """--from <slug>: (branch, repo-hint) out of STREAM.md frontmatter.

    The repo hint is the root derived from the recorded worktree path
    (…/.worktrees/x -> …), or a recipe name matching the tail of the
    `repo:` field (Acme/webapp -> webapp)."""
    fm = _stream_frontmatter(slug)
    branch = fm.get("branch")
    if not branch:
        raise WtError(f"stream '{slug}' records no branch")
    recorded = fm.get("worktree", "")
    if "/.worktrees/" in recorded:
        return branch, recorded.split("/.worktrees/")[0]
    tail = fm.get("repo", "").rsplit("/", 1)[-1]
    return branch, tail if tail in config.repos() else None


# -- verbs -------------------------------------------------------------------

def cmd_open(args: argparse.Namespace) -> int:
    intake, repo_hint = args.intake, None
    if args.from_stream:
        if intake:
            raise WtError("give either an intake or --from, not both")
        intake, repo_hint = _intake_from_stream(args.from_stream)
    if not intake:
        raise WtError("nothing to open: give a branch, Linear URL, or --from <stream>")

    linear_url = ""
    if is_linear(intake):
        linear_url, intake = intake, parse_linear(intake)
        _info(f"> Linear issue -> {intake}")
    branch = intake

    _, root, recipe = resolve_repo(args.repo or repo_hint)
    dname = dir_name(branch)
    wt_path = root / ".worktrees" / dname
    child_out = sys.stderr if args.print_path else None

    if wt_path.is_dir():
        _info(f"-> {wt_path}")
    else:
        _create_worktree(root, branch, wt_path, recipe, child_out)

    slug = session_name(dname)
    use_tmux = args.tmux or args.detached
    if use_tmux:
        launch = str(recipe.get("launch") or "")
        if linear_url:
            claude = f"{_CLAUDE} {os.environ.get('_WT_CLAUDE_EXTRA', '')}".strip()
            launch = f"echo '/acme:linear-issue-work {linear_url}' | {claude}"
        _open_tmux(wt_path, slug, recipe, launch, args.detached)

    _register_birth(slug=slug, branch=branch, repo=root, worktree=wt_path,
                    tmux_session=slug if use_tmux else "")

    if args.print_path:
        print(wt_path)
    return 0


def _create_worktree(root: Path, branch: str, wt_path: Path, recipe: dict,
                     out) -> None:
    _info("~ fetching origin/main…")
    if _sh(["git", "-C", str(root), "fetch", "origin", "main"],
           out=out).returncode != 0:
        raise WtError(f"git fetch failed in {root}")
    _info(f"+ creating worktree {branch}…")
    wt_path.parent.mkdir(parents=True, exist_ok=True)
    if _sh(["git", "-C", str(root), "worktree", "add", "-b", branch,
            str(wt_path), "origin/main"], out=out).returncode != 0:
        raise WtError("git worktree add failed")
    env_extra = recipe.get("env") or None
    for cmd in setup_commands(recipe, wt_path):
        _info(f"* setup: {cmd}")
        # Setup failures are reported by the tool itself but do not abort —
        # matches wt.zsh, and the worktree is still usable.
        _sh(["sh", "-c", cmd], cwd=wt_path, out=out, env_extra=env_extra)
    _info("ok worktree ready")


def cmd_ls(args: argparse.Namespace) -> int:
    _, root, _ = resolve_repo(args.repo)
    rows = [{"branch": b, "path": str(p),
             "session": session_name(dir_name(b))}
            for b, p in worktrees(root)]
    for row in rows:
        row["live"] = _tmux_has(row["session"])
    rows = _ledger_annotate(rows)
    if not rows:
        _info("no worktrees — `hive wt open <branch>` creates one")
        return 0
    for row in rows:
        mark = " [tmux]" if row.get("live") else ""
        extra = f"  {row['note']}" if row.get("note") else ""
        print(f"  {row['branch']:<35} {row['path']}{mark}{extra}")
    return 0


def cmd_rm(args: argparse.Namespace) -> int:
    _, root, _ = resolve_repo(args.repo)
    branch = args.branch or _pick(root, want="branch")
    if not branch:
        return 0
    dname = dir_name(branch)
    wt_path = root / ".worktrees" / dname
    if not wt_path.is_dir():
        raise WtError(f"no worktree found for {branch}")
    slug = session_name(dname)

    # 1. Archive before destroying: close pipeline + stream log, best-effort.
    try:
        from . import lifecycle
        lifecycle.close_stream(slug, reason="wt-rm")
    except Exception:
        pass
    try:
        if (config.STREAMS_VAULT / "streams" / slug).is_dir():
            vendored.ws(["log", slug, "wt rm: archiving + removing worktree"])
    except Exception:
        pass

    # 2. Session.
    if _tmux_has(slug):
        _info(f"! killing tmux session {slug}")
        _sh(["tmux", "kill-session", "-t", slug], capture=True)

    # 3. Worktree.
    _info(f"- removing worktree {branch}…")
    if _sh(["git", "-C", str(root), "worktree", "remove", str(wt_path),
            "--force"]).returncode != 0:
        raise WtError("git worktree remove failed")

    # 4. Branch: the destructive tail stays gated.
    if args.yes:
        _delete_branch(root, branch)
    elif sys.stdin.isatty():
        try:
            answer = input(f"Delete branch {branch}? [y/N] ")
        except EOFError:
            answer = ""
        if answer.strip().lower() in ("y", "yes"):
            _delete_branch(root, branch)
    else:
        _info(f"branch {branch} kept (non-interactive; pass --yes to delete)")
    _info("ok done")
    return 0


def _delete_branch(root: Path, branch: str) -> None:
    if _sh(["git", "-C", str(root), "show-ref", "--verify", "--quiet",
            f"refs/heads/{branch}"], capture=True).returncode == 0:
        _sh(["git", "-C", str(root), "branch", "-D", branch])
        _info("- branch deleted")
    else:
        _info("branch not found locally")


def cmd_pick(args: argparse.Namespace) -> int:
    _, root, _ = resolve_repo(args.repo)
    choice = _pick(root, want="branch" if args.branch else "path")
    if choice is None:
        return 1
    print(choice)
    return 0


def _pick(root: Path, want: str = "path") -> str | None:
    pairs = worktrees(root)
    if not pairs:
        _info("no worktrees yet — create one with `wt <branch>`")
        return None
    branches = [b for b, _ in pairs]
    branch = (_fzf(branches, root) if shutil.which("fzf")
              else _menu(branches))
    if not branch:
        return None
    if want == "branch":
        return branch
    for b, p in pairs:
        if b == branch:
            return str(p)
    return None


def _fzf(branches: list[str], root: Path) -> str | None:
    res = _sh(["fzf", "--height=40%", "--reverse", "--border", "--ansi",
               "--header=Select worktree", "--preview",
               f"git -C {root} log --oneline --color=always -15 {{}}"],
              capture=True, input_text="\n".join(branches) + "\n")
    return res.stdout.strip() if res.returncode == 0 else None


def _menu(branches: list[str]) -> str | None:
    for i, b in enumerate(branches, 1):
        _info(f"  {i}) {b}")
    sys.stderr.write("worktree #: ")
    sys.stderr.flush()
    try:
        raw = input().strip()
    except EOFError:
        return None
    if raw.isdigit() and 1 <= int(raw) <= len(branches):
        return branches[int(raw) - 1]
    return None


def cmd_repos(args: argparse.Namespace) -> int:
    for name, recipe in config.repos().items():
        if args.names:
            print(name)
        else:
            path = Path(str(recipe.get("path", ""))).expanduser()
            print(f"{name}\t{path}")
    return 0


# -- dispatch ----------------------------------------------------------------

def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="hive wt",
        description="worktree + tmux + session birth/death (shim: shims/wt.zsh)")
    sub = p.add_subparsers(dest="verb")

    o = sub.add_parser("open", help="open or create a worktree")
    o.add_argument("intake", nargs="?",
                   help="branch name or Linear issue URL")
    o.add_argument("--from", dest="from_stream", metavar="SLUG",
                   help="recreate from a stream's STREAM.md record")
    o.add_argument("--repo", help="recipe name or repo path")
    o.add_argument("-t", "--tmux", action="store_true",
                   help="open/attach a tmux session named after the worktree")
    o.add_argument("-d", "--detached", action="store_true",
                   help="tmux without attaching (implies --tmux)")
    o.add_argument("--print-path", action="store_true",
                   help="print only the worktree path on stdout (for the shim)")

    l = sub.add_parser("ls", help="list worktrees")
    l.add_argument("--repo")

    r = sub.add_parser("rm", help="archive + remove a worktree")
    r.add_argument("branch", nargs="?")
    r.add_argument("--repo")
    r.add_argument("-y", "--yes", action="store_true",
                   help="also delete the branch, without prompting")

    k = sub.add_parser("pick", help="pick a worktree; prints its path")
    k.add_argument("--repo")
    k.add_argument("--branch", action="store_true",
                   help="print the branch instead of the path")

    n = sub.add_parser("repos", help="list repo recipes")
    n.add_argument("--names", action="store_true",
                   help="names only (for shell completion)")
    return p


def run(argv: list[str]) -> int:
    parser = _parser()
    if not argv:
        parser.print_help(sys.stderr)
        return 0
    args = parser.parse_args(argv)
    handlers = {"open": cmd_open, "ls": cmd_ls, "rm": cmd_rm,
                "pick": cmd_pick, "repos": cmd_repos}
    handler = handlers.get(args.verb)
    if handler is None:
        parser.print_help(sys.stderr)
        return 2
    try:
        return handler(args)
    except WtError as e:
        util.eprint(f"hive wt: {e}")
        return 1
