# Android tests

Automated tests live in `android/app/src/test` and run on the JVM:

```sh
android/gradlew -p android testDebugUnitTest
bash android/scripts/mockhub-it.sh      # optional; needs Go; MOCK evidence only
bash android/scripts/realhub-it.sh      # optional; real local pyre-hub + pyred (tests/e2e/local_stack.sh)
python3 android/scripts/test-failures.py android/app/build/test-results/testDebugUnitTest
```

| Suite | What it proves |
|---|---|
| `contract/FixtureContractTest` | Every `tests/fixtures/contracts` bundle decodes **strictly** (unknown key = failure, so models track the Go contract); every exchange request round-trips to identical JSON; stale/offline status and independent delivery facts from fixtures 02/03; unknown enum values decode. |
| `net/HubClientTest` | RPC binding (path, bearer, `X-Pyre-Protocol`, body), ErrorBody decoding, no retry on conflict/protocol mismatch/auth, bounded retries reusing the same `operation_id`, `outcome_indeterminate` retried only for mutations, token never in URL. |
| `net/PinningTest` | Exact-leaf pin accepted with a non-matching hostname, wrong pin rejected, self-signed rejected in CA mode, pin normalization, https/dev-cleartext rules. |
| `net/EventStreamTest` | Header auth + `after_cursor`, `reset_required` → snapshot cursor, 401 fatal, bounded jittered backoff. |
| `pairing/PairingTest` | Link parsing/redaction, exchange over pinned TLS, unauthenticated exchange with code only in the body, expired code, http refused. |
| `ui/ApprovalViewModelTest` | Double tap answers once; stale native resolution shown, nothing sent; resolved between open and tap; replaced run never answered; unknown outcome retry reuses the operation id across a restart; offline before validation sends nothing. |
| `ui/SessionDetailViewModelTest` | Indeterminate delivery: reissue blocked until risk accepted, double tap one operation id; resolve does not resend; offline send pending then replayed once; steer gated on capability. |
| `notify/AttentionNotifierTest` | Alerts only `notification.state == "sent"`; withdrawn/resolved approvals cancel; stale approvals never get controls; FCM `dismiss`/`notify`; generic alert when the hub is unreachable. |
| `ui/ScreensUiTest` (Robolectric Compose) | Sessions show an offline node honestly; approval double tap; stale link without controls; reconcile dialog risk gate; terminal vs chat; inbox deep link; accounts stale/unknown. |
| `ui/RedesignUiTest` (Robolectric Compose) | Inbox one-tap Allow answers once by `(approval_id, run_id)` even on a double tap; 48dp card menu; empty inbox says what happens next; Settings hides client id / fingerprint until "About this connection" is opened and never shows Firebase build instructions; quiet hours use a time picker (no HH:MM fields) and save on change; pairing confirms hub and fingerprint without showing the one-time code, no developer switch for https links. |
| `ui/ThemeContrastTest` | WCAG 4.5:1 for every text/background pair of Pyre's light and dark palettes (3:1 for outlines); the app opens on the Inbox with exactly three bottom destinations. |
| `terminal/TerminalRenderTest` (Robolectric, native graphics) | Byte streams captured from real Claude Code and Codex TUIs through a tmux attach (`scripts/capture-tui.py`, isolated HOME, no credentials) rendered by the vendored Termux emulator: text, 256-colour attributes, box drawing, block glyphs, alternate screen; truecolor + CJK/emoji widths + DECCKM; extra keys and sticky Ctrl/Alt bytes; TerminalView draws real pixels in several colours (`app/build/terminal-render-claude.png`). |
| `data/CredentialMigrationTest` | Old EncryptedSharedPreferences fixture written by the real old code (over a fake AndroidKeyStore provider) migrates to Keystore AES-GCM + DataStore, old file and Tink key deleted; restart; lost key; interrupted migration. |
| `ui/NavigationAndDataTest` | Deep links (all fixture links resolve); ULIDs; intent-id persistence; offline cache labels; digest verification; preview URL acceptance; markdown. |
| `domain/IdentityAndHealthTest` | Message sender/recipient/thread/reply/attachment wording matches the CLI/TUI; artifact attachments link to the exact version; a delivering outbox replay clears "Hub unreachable", an empty one does not. |
| `terminal/TerminalSocketTest` | Resize/input issued before the handshake never precede the auth frame (device-found race); ticket in the first frame (not URL), binary output, error frame + 4401 → error not output, bare 4503 close → 503, enrollment-pending node label. |
| `integration/RealHubIntegrationTest` | Against a real local hub + pyred: `pyre pair-client --kind android` link → pinned exchange (hello.mock=false), one-time code, idempotent launch, message idempotency/conflict and delivery facts (mock runtime), shell web terminal echo through the relay, one-time ticket reuse → error, `attention.get`, artifact digest, `artifacts.preview_url` (200 + sandbox CSP, second use 403), event stream, cleanup stop. |
| `integration/MockHubIntegrationTest` | Against `pyre-mockhub --tls`: clients.pair → `pyre://pair` → exchange over pinned TLS, one-time code, reads, approval answered once, message idempotency + conflict, artifact download + digest, event stream, attach. Skipped unless the script provides the hub. |

## Physical-device checklist (WP19, A18)

Run on 2026-10-05 partly on a Pixel 10 Pro and otherwise on an Android 16 emulator; see
`docs/evidence/android/emulator-2026-10-05.md` for what ran where. Record results with the build's version:

1. Install the release APK signed with the real key; pair over HTTPS with the pinned self-signed hub and via Twingate; reject a wrong pin.
2. Launch a session on a selected node; read the transcript; queue a reply; see delivery facts update.
3. Answer a question and an approval in-app and from a notification; double-tap; answer one in the terminal and confirm the phone withdraws it.
4. Review an artifact: Markdown in-app, image/PDF via the system viewer, HTML preview isolated (JS off); comment on a version.
5. Hub-stream mode with screen off for 30+ minutes (battery exemption on/off), network switch, hub restart; FCM build if configured.
6. Airplane mode: cached lists labelled, queued message pending, reconnect replays once.
7. Install an update signed with the same key over `adb install -r`; confirm pairing and cache survive.

## Screenshots

`screens/` holds before/after captures of every screen from the 2026-10-05 UI redesign (Android 16
emulator, synthetic `pyre-mockhub` demo fleet; the terminal from a real local hub + shell session):
`before-<screen>-<mode>.webp` and `after-<screen>-<mode>.webp`, modes `dark`, `light`, `dark-2x`,
`light-2x` (system font scale 2.0). See `docs/evidence/android/ux-redesign-2026-10-05.md`.
