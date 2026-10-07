#!/usr/bin/env python3
import base64
import hashlib
import socket
import urllib.parse
import importlib.machinery
import importlib.util
import io
import json
import os
import tempfile
import textwrap
import unittest
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import patch


os.environ["ROTA_NO_KEYCHAIN"] = "1"  # tests must never touch the real macOS Keychain
ROTA_PATH = Path(__file__).with_name("rota")
loader = importlib.machinery.SourceFileLoader("rota_cli", str(ROTA_PATH))
spec = importlib.util.spec_from_loader(loader.name, loader)
rota = importlib.util.module_from_spec(spec)
loader.exec_module(rota)


class CodexUsageTests(unittest.TestCase):
    def test_profiles_include_active_and_configured_homes_once(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            active = root / "active"
            other = root / "work"
            with patch.dict(os.environ, {
                "CODEX_HOME": str(active),
                "ROTA_CODEX_HOMES": os.pathsep.join((str(active), str(other))),
            }, clear=False):
                profiles = rota.codex_profiles()
        self.assertEqual(["default", "work"], [profile["name"] for profile in profiles])
        self.assertEqual([True, False], [profile["active"] for profile in profiles])

    def test_multi_bucket_limits_use_durations_and_names(self):
        data = {
            "rateLimits": {},
            "rateLimitsByLimitId": {
                "codex": {
                    "limitId": "codex",
                    "primary": {
                        "usedPercent": 21,
                        "windowDurationMins": 300,
                        "resetsAt": 1,
                    },
                    "secondary": {
                        "usedPercent": 43,
                        "windowDurationMins": 10080,
                        "resetsAt": 2,
                    },
                },
                "codex_spark": {
                    "limitId": "codex_spark",
                    "limitName": "Codex Spark",
                    "primary": {
                        "usedPercent": 5,
                        "windowDurationMins": 10080,
                        "resetsAt": 3,
                    },
                },
            },
        }
        rows = rota.codex_limit_rows(data)
        self.assertEqual(
            ["session (5h)", "weekly", "weekly (Codex Spark)"],
            [row["label"] for row in rows],
        )
        self.assertEqual([21, 43, 5], [row["percent"] for row in rows])

    def test_app_server_client_matches_ids_and_ignores_notifications(self):
        server = textwrap.dedent("""\
            #!/usr/bin/env python3
            import json
            import sys

            for line in sys.stdin:
                message = json.loads(line)
                method = message.get("method")
                if method == "initialize":
                    print(json.dumps({"id": message["id"], "result": {"codexHome": "/tmp"}}), flush=True)
                elif method == "account/read":
                    print(json.dumps({"method": "account/updated", "params": {}}), flush=True)
                    print(json.dumps({"id": message["id"], "result": {
                        "account": {"type": "chatgpt", "email": "dev@example.com", "planType": "pro"},
                        "requiresOpenaiAuth": True
                    }}), flush=True)
                elif method == "account/rateLimits/read":
                    print(json.dumps({"id": message["id"], "result": {
                        "rateLimits": {
                            "limitId": "codex",
                            "primary": {"usedPercent": 12, "windowDurationMins": 10080, "resetsAt": 2000000000},
                            "credits": {"hasCredits": True, "unlimited": False, "balance": "10.5000"}
                        },
                        "rateLimitsByLimitId": None
                    }}), flush=True)
            """)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            home = root / "home"
            home.mkdir()
            fake = root / "codex"
            fake.write_text(server)
            fake.chmod(0o755)
            data, error = rota.codex_usage_for(
                {"name": "default", "home": home, "active": True},
                codex_bin=str(fake),
                timeout=3,
            )
        self.assertIsNone(error)
        self.assertEqual("dev@example.com", data["account"]["email"])
        self.assertEqual(12, rota.codex_limit_rows(data["rateLimits"])[0]["percent"])

    def test_api_key_account_does_not_request_subscription_limits(self):
        server = textwrap.dedent("""\
            #!/usr/bin/env python3
            import json
            import sys

            for line in sys.stdin:
                message = json.loads(line)
                method = message.get("method")
                if method == "initialize":
                    print(json.dumps({"id": message["id"], "result": {}}), flush=True)
                elif method == "account/read":
                    print(json.dumps({"id": message["id"], "result": {
                        "account": {"type": "apiKey"}, "requiresOpenaiAuth": False
                    }}), flush=True)
                elif method == "account/rateLimits/read":
                    raise RuntimeError("rate limits should not be requested")
            """)
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            home = root / "home"
            home.mkdir()
            fake = root / "codex"
            fake.write_text(server)
            fake.chmod(0o755)
            data, error = rota.codex_usage_for(
                {"name": "default", "home": home, "active": True},
                codex_bin=str(fake),
                timeout=3,
            )
        self.assertIsNone(error)
        self.assertEqual("apiKey", data["account"]["type"])
        self.assertIsNone(data["rateLimits"])

    def test_render_includes_plan_usage_and_credits(self):
        entry = {
            "profile": {"name": "default", "home": Path("/tmp/codex"), "active": True},
            "error": None,
            "data": {
                "account": {"type": "chatgpt", "email": "dev@example.com", "planType": "plus"},
                "rateLimits": {
                    "rateLimits": {
                        "limitId": "codex",
                        "primary": {"usedPercent": 30, "windowDurationMins": 10080, "resetsAt": None},
                        "credits": {"hasCredits": True, "unlimited": False, "balance": "4.2500"},
                    }
                },
            },
        }
        output = io.StringIO()
        with redirect_stdout(output):
            rota.render_codex_usage([entry])
        rendered = output.getvalue()
        self.assertIn("plus subscription", rendered)
        self.assertIn("weekly", rendered)
        self.assertIn("30%", rendered)
        self.assertIn("credits: 4.25 remaining", rendered)


def fake_auth(account_id, email, plan="pro"):
    claims = {
        "sub": account_id,
        "email": email,
        "https://api.openai.com/auth": {"chatgpt_plan_type": plan},
    }
    payload = base64.urlsafe_b64encode(json.dumps(claims).encode()).decode().rstrip("=")
    return {
        "auth_mode": "chatgpt",
        "OPENAI_API_KEY": None,
        "tokens": {
            "id_token": f"header.{payload}.sig",
            "access_token": f"access-{account_id}",
            "refresh_token": f"refresh-{account_id}",
            "account_id": account_id,
        },
        "last_refresh": "2026-01-01T00:00:00Z",
    }


class CodexAccountTests(unittest.TestCase):
    def codex_home(self, tmp):
        home = Path(tmp) / "codex"
        home.mkdir()
        return home

    def save_account(self, home, name, auth):
        account_dir = home / "accounts" / name
        account_dir.mkdir(parents=True)
        (account_dir / "auth.json").write_text(json.dumps(auth))

    def test_identity_parses_chatgpt_and_api_key_auth(self):
        kind, ident, email = rota.codex_identity(fake_auth("acct-1", "dev@example.com"))
        self.assertEqual(("chatgpt", "acct-1", "dev@example.com"), (kind, ident, email))
        self.assertEqual(
            ("apiKey", "sk-test", None),
            rota.codex_identity({"OPENAI_API_KEY": "sk-test", "tokens": None}),
        )
        self.assertEqual((None, None, None), rota.codex_identity(None))

    def test_plan_extracted_from_id_token(self):
        self.assertEqual("pro", rota.codex_plan(fake_auth("acct-1", "dev@example.com")))

    def test_active_name_matches_saved_account(self):
        with tempfile.TemporaryDirectory() as tmp:
            home = self.codex_home(tmp)
            (home / "auth.json").write_text(json.dumps(fake_auth("acct-1", "a@example.com")))
            self.save_account(home, "a@example.com", fake_auth("acct-1", "a@example.com"))
            self.save_account(home, "b@example.com", fake_auth("acct-2", "b@example.com"))
            with patch.dict(os.environ, {"CODEX_HOME": str(home)}, clear=False):
                self.assertEqual("a@example.com", rota.codex_active_name())

    def test_profiles_include_saved_accounts_with_live_home_for_active(self):
        with tempfile.TemporaryDirectory() as tmp:
            home = self.codex_home(tmp)
            (home / "auth.json").write_text(json.dumps(fake_auth("acct-1", "a@example.com")))
            self.save_account(home, "a@example.com", fake_auth("acct-1", "a@example.com"))
            self.save_account(home, "b@example.com", fake_auth("acct-2", "b@example.com"))
            with patch.dict(os.environ, {"CODEX_HOME": str(home)}, clear=False):
                profiles = rota.codex_profiles()
        self.assertEqual(
            ["a@example.com", "b@example.com"],
            [profile["name"] for profile in profiles],
        )
        self.assertEqual([True, False], [profile["active"] for profile in profiles])
        self.assertEqual(home, profiles[0]["home"])
        self.assertEqual(home / "accounts" / "b@example.com", profiles[1]["home"])

    def test_switch_swaps_only_auth_json_and_syncs_outgoing(self):
        with tempfile.TemporaryDirectory() as tmp:
            home = self.codex_home(tmp)
            live = fake_auth("acct-1", "a@example.com")
            live["last_refresh"] = "2026-02-02T00:00:00Z"  # newer than the snapshot
            (home / "auth.json").write_text(json.dumps(live))
            (home / "config.toml").write_text("model = 'gpt-5.3'\n")
            self.save_account(home, "a@example.com", fake_auth("acct-1", "a@example.com"))
            self.save_account(home, "b@example.com", fake_auth("acct-2", "b@example.com"))
            with patch.dict(os.environ, {"CODEX_HOME": str(home)}, clear=False):
                with redirect_stdout(io.StringIO()):
                    rota.codex_do_switch("b@example.com")
                self.assertEqual("b@example.com", rota.codex_active_name())
            new_live = json.loads((home / "auth.json").read_text())
            self.assertEqual("acct-2", new_live["tokens"]["account_id"])
            synced = json.loads(
                (home / "accounts" / "a@example.com" / "auth.json").read_text()
            )
            self.assertEqual("2026-02-02T00:00:00Z", synced["last_refresh"])
            self.assertEqual("model = 'gpt-5.3'\n", (home / "config.toml").read_text())

    def test_best_hint_ranks_switchable_accounts_by_worst_window(self):
        def entry(name, active, percent):
            return {
                "profile": {"name": name, "home": Path("/tmp"), "active": active,
                            "switchable": True},
                "error": None,
                "data": {
                    "account": {"type": "chatgpt", "email": name, "planType": "pro"},
                    "rateLimits": {
                        "rateLimits": {
                            "limitId": "codex",
                            "primary": {"usedPercent": percent,
                                        "windowDurationMins": 10080, "resetsAt": None},
                        }
                    },
                },
            }
        output = io.StringIO()
        with redirect_stdout(output):
            rota.render_codex_usage([
                entry("a@example.com", True, 80),
                entry("b@example.com", False, 20),
            ])
        rendered = output.getvalue()
        self.assertIn("best:", rendered)
        self.assertIn("rota codex switch b@example.com", rendered)


if __name__ == "__main__":
    unittest.main()


class ClaudeTokenTests(unittest.TestCase):
    """Token refresh, dead-account marking, 429 handling, and the usage cache."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.store = root / "accounts"
        self.store.mkdir()
        self.patches = [
            patch.object(rota, "STORE_DIR", self.store),
            patch.object(rota, "CACHE_FILE", self.store / ".usage-cache.json"),
            patch.object(rota, "CREDS_FILE", root / ".credentials.json"),
            patch.object(rota, "CLAUDE_JSON", root / ".claude.json"),
            patch.object(rota, "user_agent", lambda: "test"),
        ]
        for p in self.patches:
            p.start()

    def tearDown(self):
        for p in self.patches:
            p.stop()
        self.tmp.cleanup()

    def snap(self, name, expires_in_ms=3_600_000, **extra):
        snap = {
            "name": name,
            "claudeAiOauth": {
                "accessToken": f"access-{name}", "refreshToken": f"refresh-{name}",
                "expiresAt": rota.now_ms() + expires_in_ms,
                "refreshTokenExpiresAt": rota.now_ms() + 30 * 86400_000,
                "subscriptionType": "max",
            },
            "oauthAccount": {"emailAddress": name, "accountUuid": f"uuid-{name}"},
            "savedAt": rota.now_ms(),
        }
        snap.update(extra)
        rota.write_json_atomic(self.store / f"{name}.json", snap)
        return snap

    def test_tokens_from_response_tracks_refresh_expiry_and_scopes(self):
        before = {"refreshToken": "old", "refreshTokenExpiresAt": 1, "subscriptionType": "max"}
        new = rota.tokens_from_response({
            "access_token": "a", "refresh_token": "r", "expires_in": 100,
            "refresh_token_expires_in": 1000, "scope": "user:profile user:inference",
        }, before)
        self.assertEqual("r", new["refreshToken"])
        self.assertEqual("max", new["subscriptionType"])
        self.assertEqual(["user:profile", "user:inference"], new["scopes"])
        self.assertGreater(new["refreshTokenExpiresAt"], rota.now_ms() + 900_000)
        no_expiry = rota.tokens_from_response({"access_token": "a", "expires_in": 5}, before)
        self.assertNotIn("refreshTokenExpiresAt", no_expiry)
        self.assertEqual("old", no_expiry["refreshToken"])

    def test_dead_account_skips_network(self):
        snap = self.snap("a@x", authDead={"at": rota.now_ms() - 60_000, "reason": "Refresh token expired"})
        with patch.object(rota, "http_json", side_effect=AssertionError("no network expected")):
            data, err = rota.usage_for("a@x", snap, False)
        self.assertIsNone(data)
        self.assertIn("Refresh token expired", err)
        self.assertIn("rota reauth a@x", err)

    def test_expired_access_token_refreshes_before_fetching(self):
        snap = self.snap("a@x", expires_in_ms=-1)
        calls = []

        def fake_http(url, data=None, headers=None, timeout=30):
            calls.append((url, data))
            if url == rota.TOKEN_URL:
                return {"access_token": "fresh", "refresh_token": "rotated",
                        "expires_in": 28800, "refresh_token_expires_in": 100}
            self.assertEqual("Bearer fresh", headers["Authorization"])
            return {"limits": [{"kind": "session", "percent": 5}]}

        with patch.object(rota, "http_json", fake_http):
            data, err = rota.usage_for("a@x", snap, False)
        self.assertIsNone(err)
        self.assertEqual([rota.TOKEN_URL, rota.USAGE_URL], [c[0] for c in calls])
        saved = json.loads((self.store / "a@x.json").read_text())
        self.assertEqual("rotated", saved["claudeAiOauth"]["refreshToken"])

    def test_invalid_grant_marks_account_dead(self):
        snap = self.snap("a@x", expires_in_ms=-1)
        body = b'{"error": "invalid_grant", "error_description": "Refresh token expired"}'

        def fake_http(url, data=None, headers=None, timeout=30):
            raise rota.HttpError(400, body, {})

        with patch.object(rota, "http_json", fake_http):
            data, err = rota.usage_for("a@x", snap, False)
        self.assertIn("Refresh token expired", err)
        self.assertIn("rota reauth a@x", err)
        saved = json.loads((self.store / "a@x.json").read_text())
        self.assertEqual("Refresh token expired", saved["authDead"]["reason"])
        # and the next run never touches the network
        with patch.object(rota, "http_json", side_effect=AssertionError("no network expected")):
            _, err2 = rota.usage_for("a@x", saved, False)
        self.assertIn("rota reauth a@x", err2)

    def test_empty_tokens_are_marked_dead_without_network(self):
        snap = self.snap("a@x", expires_in_ms=-1)
        snap["claudeAiOauth"]["refreshToken"] = ""
        snap["claudeAiOauth"]["accessToken"] = ""
        with patch.object(rota, "http_json", side_effect=AssertionError("no network expected")):
            data, err = rota.usage_for("a@x", snap, False)
        self.assertIn("no refresh token", err)
        self.assertIn("rota reauth a@x", err)
        self.assertIn("authDead", json.loads((self.store / "a@x.json").read_text()))

    def test_rate_limit_reports_retry_after_without_sleeping(self):
        snap = self.snap("a@x")

        def fake_http(url, data=None, headers=None, timeout=30):
            raise rota.HttpError(429, b'{"error":{"type":"rate_limit_error"}}', {"Retry-After": "2407"})

        with patch.object(rota, "http_json", fake_http), patch.object(rota.time, "sleep") as sleep:
            data, err = rota.usage_for("a@x", snap, False)
        self.assertIsNone(data)
        self.assertIn("rate limited", err)
        self.assertIn("in 40m", err)
        sleep.assert_not_called()

    def test_short_rate_limit_is_retried(self):
        snap = self.snap("a@x")
        attempts = []

        def fake_http(url, data=None, headers=None, timeout=30):
            attempts.append(url)
            if len(attempts) == 1:
                raise rota.HttpError(429, b"", {"Retry-After": "2"})
            return {"limits": []}

        with patch.object(rota, "http_json", fake_http), patch.object(rota.time, "sleep") as sleep:
            data, err = rota.usage_for("a@x", snap, False)
        self.assertIsNone(err)
        self.assertEqual(2, len(attempts))
        sleep.assert_called_once_with(2)

    def test_collect_usage_caches_results_for_error_fallback(self):
        good = self.snap("good@x")
        bad = self.snap("bad@x")
        accounts = {"good@x": good, "bad@x": bad}

        def ok(url, data=None, headers=None, timeout=30):
            return {"limits": [{"kind": "session", "percent": 12}]}

        with patch.object(rota, "http_json", ok):
            results, errors = rota.collect_usage(accounts, None)
        self.assertEqual({"good@x", "bad@x"}, set(results))
        cache = rota.cache_load()
        self.assertEqual(12, cache["bad@x"]["data"]["limits"][0]["percent"])

        def limited(url, data=None, headers=None, timeout=30):
            raise rota.HttpError(429, b"", {"Retry-After": "600"})

        output = io.StringIO()
        with patch.object(rota, "http_json", limited), patch.object(rota, "accounts_incl_live",
                                                                     lambda: (accounts, None)):
            with redirect_stdout(output):
                rota.cmd_usage(["bad@x"])
        rendered = output.getvalue()
        self.assertIn("cached", rendered)
        self.assertIn("12%", rendered)
        self.assertIn("rate limited", rendered)

    def test_switch_refuses_dead_account(self):
        self.snap("a@x", authDead={"at": rota.now_ms(), "reason": "Refresh token expired"})
        with self.assertRaises(SystemExit), redirect_stdout(io.StringIO()):
            rota.do_switch("a@x")

    def test_token_health_levels(self):
        dead = {"claudeAiOauth": {}, "authDead": {"at": 1}}
        self.assertEqual(("needs re-auth", rota.RED), rota.token_health(dead))
        soon = {"claudeAiOauth": {"refreshTokenExpiresAt": rota.now_ms() + 2 * 86400_000}}
        self.assertEqual(rota.YELLOW, rota.token_health(soon)[1])
        fine = {"claudeAiOauth": {"refreshTokenExpiresAt": rota.now_ms() + 30 * 86400_000}}
        self.assertEqual(rota.DIM, rota.token_health(fine)[1])
        self.assertIn("login good for 30d", rota.token_health(fine)[0])


class OAuthLoginTests(unittest.TestCase):
    def test_authorize_url_matches_claude_code_parameters(self):
        url = rota.authorize_url("chal", "st", "http://localhost:1234/callback", "me@x")
        parsed = urllib.parse.urlparse(url)
        q = {k: v[0] for k, v in urllib.parse.parse_qs(parsed.query).items()}
        self.assertEqual("https://claude.com/cai/oauth/authorize", f"{parsed.scheme}://{parsed.netloc}{parsed.path}")
        self.assertEqual({
            "code": "true", "client_id": rota.CLIENT_ID, "response_type": "code",
            "redirect_uri": "http://localhost:1234/callback", "scope": " ".join(rota.LOGIN_SCOPES),
            "code_challenge": "chal", "code_challenge_method": "S256", "state": "st",
            "login_hint": "me@x",
        }, q)

    def test_pkce_challenge_is_s256_of_verifier(self):
        verifier, challenge = rota.pkce_pair()
        expected = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).decode().rstrip("=")
        self.assertEqual(expected, challenge)

    def test_parse_auth_input_accepts_url_code_hash_state_and_bare_code(self):
        self.assertEqual(("abc", "st"), rota.parse_auth_input(
            "http://localhost:5/callback?code=abc&state=st", "expected"))
        self.assertEqual(("abc", "st"), rota.parse_auth_input("abc#st\n", "expected"))
        self.assertEqual(("abc", "expected"), rota.parse_auth_input("  abc ", "expected"))

    def test_callback_handler_validates_state_and_answers_browser(self):
        def request(path):
            a, b = socket.socketpair()
            a.sendall(f"GET {path} HTTP/1.1\r\nHost: localhost\r\n\r\n".encode())
            got = rota.handle_callback(b, "good")
            reply = a.recv(65536).decode()
            a.close()
            return got, reply
        got, reply = request("/callback?code=c1&state=good")
        self.assertEqual(("c1", "good"), got)
        self.assertTrue(reply.startswith("HTTP/1.1 200"))
        got, reply = request("/callback?code=c1&state=other")
        self.assertIsNone(got)
        self.assertTrue(reply.startswith("HTTP/1.1 400"))
        got, reply = request("/favicon.ico")
        self.assertIsNone(got)
        self.assertTrue(reply.startswith("HTTP/1.1 404"))
        with self.assertRaises(RuntimeError):
            request("/callback?error=access_denied&state=good")

    def test_account_from_login_builds_snapshot_like_claude_code(self):
        responses = {
            rota.PROFILE_URL: {
                "account": {"uuid": "u1", "email": "me@x", "display_name": "Me",
                            "full_name": "Me Too", "created_at": "2025-01-01T00:00:00Z"},
                "organization": {"uuid": "o1", "name": "Me's Org", "organization_type": "claude_max",
                                 "rate_limit_tier": "default_claude_max_20x",
                                 "has_extra_usage_enabled": True, "billing_type": "stripe_subscription",
                                 "cc_onboarding_flags": {}},
            },
            rota.ROLES_URL: {"organization_role": "admin", "workspace_role": None,
                             "organization_name": "Me's Org"},
        }
        with patch.object(rota, "http_json", lambda url, data=None, headers=None, timeout=30: responses[url]):
            tokens, account = rota.account_from_login({
                "access_token": "a", "refresh_token": "r", "expires_in": 28800,
                "refresh_token_expires_in": 5_000_000, "scope": "user:profile user:inference",
                "account": {"uuid": "u1", "email_address": "me@x"}, "organization": {"uuid": "o1"},
            })
        self.assertEqual("max", tokens["subscriptionType"])
        self.assertEqual("default_claude_max_20x", tokens["rateLimitTier"])
        self.assertEqual("u1", account["accountUuid"])
        self.assertEqual("me@x", account["emailAddress"])
        self.assertEqual("admin", account["organizationRole"])
        self.assertEqual("default_claude_max_20x", account["organizationRateLimitTier"])
        self.assertEqual("claude_max", account["organizationType"])

    def test_save_login_writes_snapshot_and_optionally_live(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            with patch.object(rota, "STORE_DIR", root / "accounts"), \
                 patch.object(rota, "CREDS_FILE", root / ".credentials.json"), \
                 patch.object(rota, "CLAUDE_JSON", root / ".claude.json"):
                (root / ".credentials.json").write_text(json.dumps({"mcpOAuth": {"keep": 1}}))
                tokens = {"accessToken": "a", "refreshToken": "r"}
                account = {"emailAddress": "me@x", "accountUuid": "u1"}
                rota.save_login(tokens, account, replace_live=False)
                self.assertFalse("claudeAiOauth" in json.loads((root / ".credentials.json").read_text()))
                snap = json.loads((root / "accounts" / "me@x.json").read_text())
                self.assertEqual("r", snap["claudeAiOauth"]["refreshToken"])
                self.assertNotIn("authDead", snap)
                rota.save_login(tokens, account, replace_live=True)
                creds = json.loads((root / ".credentials.json").read_text())
                self.assertEqual({"keep": 1}, creds["mcpOAuth"])
                self.assertEqual("a", creds["claudeAiOauth"]["accessToken"])
                self.assertEqual("u1", json.loads((root / ".claude.json").read_text())["oauthAccount"]["accountUuid"])
