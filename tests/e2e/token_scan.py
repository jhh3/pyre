#!/usr/bin/env python3
"""Count occurrences of secret values in test state; print counts only.

  token_scan.py [--codex-snapshot AUTH_JSON]... [--session-tokens-under DIR]... ROOT...

Secrets scanned:
  * the access/refresh/id tokens (and JWT signatures) and API key in a Codex auth.json
    given with --codex-snapshot (read in memory, never printed);
  * Pyre session tokens found in thread-config.json files under the given
    dirs (the per-run Codex identity file) -- these may legitimately live in
    that one 0600 file and the node database's secret table; any other hit is
    reported.
Files: every regular file under ROOT (sqlite DB/WAL included, as bytes).
"""
import argparse, json, os, sys

ap = argparse.ArgumentParser()
ap.add_argument("--codex-snapshot", action="append", default=[])
ap.add_argument("--session-tokens-under", action="append", default=[])
ap.add_argument("roots", nargs="+")
a = ap.parse_args()

provider = []
for path in a.codex_snapshot:
    try:
        d = json.load(open(path))
    except OSError as e:
        print(f"cannot read snapshot: {e.strerror}")
        continue
    tok = d.get("tokens") or {}
    # Secrets only: account_id is a non-secret identity (Pyre stores it as
    # the account's provider_ref).
    for k in ("access_token", "refresh_token", "id_token"):
        v = tok.get(k)
        if isinstance(v, str) and len(v) >= 24:
            provider.append(v.encode())
            if v.count(".") == 2:  # a JWT's signature alone is also secret
                provider.append(v.rsplit(".", 1)[1].encode())
    if d.get("OPENAI_API_KEY"):
        provider.append(d["OPENAI_API_KEY"].encode())

session = []
for base in a.session_tokens_under:
    for root, _, files in os.walk(base):
        if "thread-config.json" in files:
            try:
                cfg = json.load(open(os.path.join(root, "thread-config.json")))
            except (OSError, ValueError):
                continue
            t = cfg.get("shell_environment_policy.set.PYRE_SESSION_TOKEN")
            if t:
                session.append(t.encode())

files = 0
prov_hits, sess_hits = [], []
for r in a.roots:
    for root, _, fs in os.walk(r):
        for f in fs:
            p = os.path.join(root, f)
            if os.path.islink(p) or not os.path.isfile(p):
                continue
            try:
                b = open(p, "rb").read()
            except OSError:
                continue
            files += 1
            for t in provider:
                if t in b:
                    prov_hits.append(p)
            for t in session:
                if t in b:
                    sess_hits.append(p)
print(f"scanned {files} files under {len(a.roots)} root(s); provider secrets: {len(provider)}, pyre session tokens: {len(session)}")
print(f"provider-token hits: {len(prov_hits)}")
for p in sorted(set(prov_hits)):
    print("  PROVIDER", p)
# Expected homes of a run's Pyre session token: the run's thread-config.json
# and the node database (secret table, and its WAL).
allowed = ("thread-config.json", "pyred.db", "pyred.db-wal", "pyred.db-shm")
expected = sorted({p for p in sess_hits if p.endswith(allowed)})
other = sorted({p for p in sess_hits if not p.endswith(allowed)})
print(f"pyre-session-token hits in expected places (thread-config.json, node db): {len(expected)}")
print(f"pyre-session-token hits elsewhere: {len(other)}")
for p in other:
    print("  SESSION", p)
sys.exit(1 if prov_hits or other else 0)
