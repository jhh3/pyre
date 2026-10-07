# Security tests (WP18)

Black-box security checks over the merged packages, plus real-stack checks
over `pyre-hub` and `pyred` processes (`stack_test.go`, harness in
`tests/resilience/stack`). Each test builds a
fresh hub fixture in a temp directory: real `transport.AuthStore`,
`enrollment`, `topology` (fleet), `projects`, `artifacts` on one on-disk
SQLite file, the deterministic mock for sessions/messages/approvals (whose
authorization semantics are pinned by `internal/conformance`), the real
`httpapi` server over TLS, the real `NodeServer` on `/v1/node/connect`, the
real enrollment exchange handlers, and a **separate TLS origin** serving only
`artifacts.PreviewHandler`. Credentials are minted at runtime and are
synthetic.

Leak checks use `internal/diagnostics.Scanner`: every secret minted during a
test is registered as a known value, then the hub log ring, the SQLite file
plus `-wal`/`-shm`, the artifact store and captured response/error/event
payloads are scanned for those values and for token-shaped patterns
(`pyc_/pyn_/pys_/pyt_/pye_/pyp_`, bearer values, OAuth/provider tokens,
JWTs, private keys). `TestLeakScannerControl` proves the scan is not
vacuous.

## Run

```sh
go test -race ./tests/security/...          # ~40 s (includes real-stack scenarios)
go test -race -short ./tests/security/...   # ~15 s
go test -v ./tests/security 2>&1 | grep -E 'DEFECT|NOTE'
```

## Covered

| test | checks |
|---|---|
| TestTokenAuthRejectsBadCredentials | missing, unknown, malformed, wrong-kind prefix, ticket-as-bearer, revoked, suffixed tokens → uniform 401 without echo; live revocation; no leak in logs/DB/responses |
| TestTicketsExpireAndRedeemOnce | purpose binding (wrong purpose does not consume), one-time redemption, expiry, TTL cap |
| TestNoPlaintextSecretsAtRest | full lifecycle (owner bootstrap, enroll + redeem, pair over HTTP + redeem, session tokens, tickets, replayed `nodes.enroll` rotating the secret, revocation): no minted secret in SQLite/WAL/SHM, logs or stored events; superseded enrollment secret rejected |
| TestConstantTimeComparePresence | static guard: secret/pin verification uses `crypto/subtle` (`SecretMatches`, `PinnedTLSConfig` ×2, `StaticTokens`), and auth/ticket/enrollment/pairing paths go through `SecretMatches` |
| TestLeakScannerControl | negative control for the at-rest scan |
| TestNodePrincipalCannotActAsOwner | node token → 403 for enroll, revoke, pair, revoke client, answer approval, send message; fleet store refuses node principals directly |
| TestNodeCannotSpeakForAnotherNode | hello claiming another node → forbidden error frame, other node's record unchanged; owner/session/no credential cannot open the node channel; revoked node cut off |
| TestSessionCannotSpoofSenderOrAnswerApprovals | body `sender` rejected by strict decoding; stored sender = authenticated session; non-recipient ack forbidden; sessions cannot answer approvals (own or other); owner answer bound to the approval's run |
| TestSessionTokenRevokedWithSession | `RevokeSessionTokens` takes effect |
| TestEnrollmentOneTimeAndBruteForceLockout | via HTTP exchange endpoints: one-time redeem, identical responses for unknown node vs wrong secret, lockout after `MaxFailedAttempts` even for the correct secret, expiry; pairing one-time + lockout |
| TestExchangeRateLimited | per-IP limit → 429 + Retry-After |
| TestEnrollmentRequiresOwner | sessions / anonymous cannot mint enrollment or pairing tickets |
| TestTLSPinMismatchRejected | wrong pin: client error is non-retryable `unauthorized` and no request reaches the server; NodeLink never connects and no bearer crosses TLS; enrollment exchange fails without consuming the ticket |
| TestArtifactFilenamesCannotTraverse | hostile filenames (`../`, `..\`, absolute, NUL, bidi, CRLF header injection, quotes) are display-only and sanitized; all stored files are content-addressed under the data dir; traversal in content/preview URLs never returns 200 |
| TestArtifactContentRequiresAuth | anonymous and cookie-only downloads 401; downloads are `attachment` with `sandbox` CSP + nosniff |
| TestPreviewIsolation | preview: one-time ticket bound to the version, sandbox CSP without scripts/same-origin/connect, no cookies, no-store, no-referrer; bearer never accepted on preview; preview ticket never accepted by the API; preview origin serves no management routes; separate origin |
| TestUploadDigestMismatchNeverPublished | digest mismatch and short upload → conflict; no artifact, no blob, no `artifact.published` event; failed upload cannot be revived |
| TestStackRevokedNode | real pyre-hub + pyred: revocation cuts the live connection, node state `revoked`, token rejected on the API, reconnect attempts back off (3 in 8 s), pyred keeps running, run not reassigned/relaunched (reads `unknown`) |
| TestStackSecretScanAfterRun | real stack after enrollment, shell + mock runs (session tokens injected), messages, artifact, pairing, unredeemed ticket, stop: hub data dir, node state dir and both process logs contain no credential outside `owner-credential.json`, `tls/hub-key.pem`, `node-credential.json`, `pyred.db` (all required 0600, dirs 0700); no session tokens in the NodeLink journal (WP18-5 fixed: withheld from the journal, held in memory) |
| TestCredentialNonDisclosure | provisioned values reach only the launch env map: launch plan/manifest, connection reports, local availability checks, invoke errors (adversarial Outfit echoing a token), locked-store failures, node logs, hub catalog rows/events and `Secret` formatting never contain them |

## Known defects (skipped, routed to owners)

| id | test | owner |
|---|---|---|
| WP18-2 (fixed, WP19) | TestUploadOwnershipEnforced | WP11 `internal/artifacts/upload.go` (`GetUpload`) |
| WP18-3 (fixed, WP19) | TestConnectionCheckDetailRedacted | WP12 `internal/connections/node.go` (`Check`) |
| WP18-5 (fixed, WP19) | TestStackSecretScanAfterRun | WP04 `internal/transport/nodelink.go` (command journal) |

## Known limitations / not covered

- **Session scoping of reads:** any authenticated principal (including an
  agent session outside a project) can read any artifact's content and
  metadata; contracts §10 says session tools default to their provisioned
  scope. Logged as a NOTE by `TestArtifactContentRequiresAuth`; needs a
  scoping design (hub core, WP05/WP11) before it can be a pass/fail check.
- Owner/node/session tokens do not expire (revocable only). Tickets and
  enrollment/pairing secrets expire.
- `connection.invoke` success output is trusted from Outfit (Pyre cannot
  know Outfit-held secret values; it relies on Outfit's redaction markers).
- Designated node credential stores: `pyred.db` keeps live runs' session
  tokens in `daemon_secrets` by design (local socket identity, file 0600).
- Pending targets: hub-core authorization for sessions/messages/approvals
  through the real hub with session tokens (the fixture-level checks use the
  mock's semantics; the real hub is exercised for revocation and at-rest
  scans), daemon local socket permission checks, web terminal ticket flow
  (`/v1/terminal`), Android pairing on device.
