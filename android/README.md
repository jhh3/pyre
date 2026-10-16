# Pyre for Android

Sideloadable Kotlin/Jetpack Compose companion for a Pyre hub (`applicationId`
`dev.jhh3.pyre`, app name **Pyre**). It talks to the hub's v1 HTTP binding
(`POST /v1/<op>`, `X-Pyre-Protocol: 1`, bearer token) defined in
`internal/contracts/routes.go`, and is tested against the shared fixtures in
`tests/fixtures/contracts/`.

Written from scratch for Pyre (no HAPI code; see "Provenance").

## What it does

| Area | Behaviour |
|---|---|
| Pairing | First run: open the `pyre://pair?hub=…&pin=…&id=…&code=…` link on the phone or tap **Paste link**; the confirm step names the hub (host:port) and shows the certificate fingerprint in groups of four, never the one-time code; **Connect** → `POST /v1/pair/exchange`. The pairing record (hub, pin, client id, token) is encrypted with a non-exportable AES-256-GCM key in the Android Keystore (StrongBox when available; no unlock requirement so lock-screen notification actions work) and kept in a DataStore file; backups and device transfer are disabled. A pairing stored by older builds in EncryptedSharedPreferences is migrated on first read, verified, then the old file and its Tink master key are deleted. HTTPS only; plain http only for `localhost`/`127.0.0.1`/`10.0.2.2` behind a labelled developer switch. |
| TLS | With a pin: exactly the leaf certificate whose SHA-256 matches is accepted (self-signed hubs; the pin replaces hostname/CA checks). Without a pin: normal CA + hostname validation. Tokens are never put in URLs or logs (`toString()` of every credential-bearing type is redacted; no HTTP logging interceptor). |
| Home (Sessions) | The app opens on Sessions. Each session carries its own attention flag (*! needs you*, *✉ message*) and the header line counts them. A **While you were away** card at the top lists each agent since this phone's own last-seen mark (needs you → done → still working; tap a line for the request or the transcript moment; **Dismiss** marks it seen), computed client-side from existing hub reads on open and pull-to-refresh. Approvals and questions are answered on the request screen (one stable `operation_id` per intent, so a double tap answers once). |
| Inbox | Bottom-bar destination (badge = unread items) over the hub's attention items (`attention.list`) and, when the hub advertises the Hello feature `asks`, its asks (`asks.list`). Segmented tabs **Asks · Messages · All · Done** (Asks hidden on hubs without asks; opens on Asks when one is waiting). Rows: unread dot, kind chip, session (and project), title, age; an ask adds its deadline ("defaults to Merge in 42m", tinted under 15 minutes or urgent). Swipe right = done, swipe left = snooze (in 1 hour / tomorrow 8 AM; also TalkBack actions); long press starts multi-select for batch read / snooze / done. Filter chips: Unread, Project, Snoozed (N). Read/done/snooze are `attention.update` calls applied at once and replayed from the durable outbox when offline. Tapping an ask opens its sheet: context (Markdown) and merged updates, one button per option with the default filled and marked, free-text answer, **Stage** box (choices collect, the sheet walks to the session's next open ask, then **Send N answers** is one `asks.answer` call with one stable `operation_id`), **Open the chat**. Other items open their session (or object). `pyre://attention/inbox` and `pyre://inbox/…` open the inbox, `pyre://ask/<id>` the ask's sheet, `pyre://attention/<id>` the ask's sheet or the item's session. Code in `ui/inbox/`, `domain/AskAnswerer.kt`. |
| Sessions | Fleet list grouped by project (default), needs-you or machine, pinned first (status dot + word), a count when a session needs you, honest status: stale observations, disconnected nodes and `unknown` runs show as **Unknown (stale)**, never idle. Unknown future states are displayed verbatim. |
| Session detail | Transcript (`sessions.read`, tail + cursor, gap marker), run state/freshness, account, capabilities, input lease. Refreshes every 5 s while the screen is in front (newer transcript entries appended, queued sends replayed); nothing polls in the background. Messages in both directions (→ received / ← sent) name sender and recipient like the CLI/TUI (`<title> (session:<id>)`, `owner (<client>)`) with thread, reply-to and attachments; reply, ack and artifact references open the referenced object. **Queue** needs `input.queue`; **Steer** only appears with `input.steer`. Delivery facts shown separately (stored / runtime / acknowledged / outcome, plus evidence). Indeterminate deliveries get a resolve/reissue dialog that requires ticking "I accept the duplication risk" before reissue. |
| Approvals / questions | Answered by `(approval_id, run_id)`. The live approval is re-fetched first; a stable per-intent `operation_id` makes double taps, retries and notification actions resolve at most once. Stale links show the existing resolution (including `native` terminal answers and the resolution `effect`). Replaced runs are never answered. |
| Launch | Online machine → available harness → directory (`nodes.action dir.list`) → optional project; previews the resolved account (`account.select` dry run). One `operation_id` per launch draft. |
| Projects | Brief and docs with revision/author/digest/history; artifacts with versions and provenance. Text/Markdown render in-app; images/PDF/other open via FileProvider + `ACTION_VIEW` after SHA-256 verification against the published digest (mismatch is refused); the screen states the verified byte count, and "Save a copy…" (Storage Access Framework) keeps any type, including files no installed app can open. HTML only in a sandboxed WebView loading a one-time link minted with `artifacts.preview_url` (v1.2) immediately before opening (hub-relative or a separate https preview origin): JavaScript, DOM storage, cookies, file/content access off, no JS bridge, navigation blocked, cross-origin subresources blocked, TLS errors accepted only for the paired pin. |
| Comments | On an exact document revision or artifact version, with optional anchor (line/quote/page) and session mention; offline comments queue. `pyre://comment/<id>` opens the commented version. |
| Machines / Accounts | Connection (nodes in `never` with label `pyre.enrollment=pending` show "Enrollment pending"), last seen, versions, harness availability, resources, Pyre and agent overhead, `ssh` alias copy. Credential kinds are labelled and unknown kinds shown verbatim. Accounts: grouped by provider ("Anthropic · 2 of 8 ready", amber when nothing is ready or all ready cards are near limit) then machine; one card per account per machine holding a login, using that machine's observation (`by_node`) else the latest. State in precedence Needs re-auth → At limit → Unknown (stale/unknown, reason in plain words, last-known bars dimmed) → Near limit (≥ 80%) → Healthy; "5h" and "Week" bars with local reset times, model-scoped weekly and extra-usage windows as one secondary line, "In use by <session> on <machine>", disabled accounts as one muted card, and "your own login (not managed)" for machines running the harness without a Pyre-managed login. Derivation in `ui/manage/AccountUsage.kt`. No tokens anywhere. |
| Activity | From Accounts (top-bar history icon): the hub's audit log (`audit.list`, read-only), newest first: time (HH:mm today, weekday this week, else date) · machine · the hub's one-line summary, an **auto** chip when Pyre acted on its own, automatic account switches/waits tinted; filter chips All / Accounts / Sessions / Fleet, pull to refresh, **Show older** pages, tap a row for why/who/ids. A hub that predates `audit.list` (HTTP 404) shows "This hub does not offer the activity log yet; upgrade pyre-hub." Code in `ui/manage/Activity.kt`. |
| Settings | Only what a person changes: notifications (permission, **Live alerts** = hub-stream mode, **Alerts with the screen off** = battery exemption), quiet hours with time pickers, time zone search and severity (`notifications.set_prefs`, saved on each change), appearance (system/light/dark, wallpaper colours), unpair (confirmed). Hub address, certificate fingerprint, client id, versions and push status sit in a collapsed **About this connection**, in plain words; Firebase build instructions never appear in the UI. |
| Terminal | The session's **Terminal** button calls `sessions.attach` with `[web_terminal, local_tmux, ssh_tmux, read_only]` (local_tmux only lets the hub's web relay obtain the node-local attach; see below). `web_terminal` opens the vendored Termux terminal emulator/view (xterm-256 and truecolor, box drawing, wide CJK/emoji glyphs, alternate screen, scrollback, pinch-to-zoom; soft keyboard plus an extra-keys row with sticky CTRL/ALT, ESC, TAB, arrows, HOME/END, PGUP/PGDN, ^C) over a pinned WebSocket that sends the one-time ticket in the **first frame**. Server `{"type":"error"}` frames and close codes 4000+status (4401/4409/4410/4503) show an error banner with "Reopen", never terminal output. `ssh_tmux` shows the command with a copy button. The terminal is full-screen with a slim header ("Terminal · Live · keys go straight to the session") and is reached from the session's **Terminal** button, separate from chat. |
| Offline | Last-read lists/documents cached on disk with "Cached 12m ago" banners. Sends and comments go through a durable outbox with fixed `operation_id`/`message_id`; they show as pending and replay on reconnect without duplicating. Approvals are deliberately not queued offline. |

### Notifications

1. **Default (oss) build — Firebase-free.** Settings → About this connection → *Instant push* says it is
   not part of this build (the FCM setup steps below live only in this README, never in the app).
2. **Hub-stream mode (optional, user-enabled).** A `remoteMessaging` foreground service holds
   `/v1/events/stream` (bearer in the upgrade headers, `?after_cursor=`, persisted cursor committed
   after each event, bounded exponential backoff with jitter, `reset_required` → re-snapshot).
   `attention.changed` alerts only when the item's `notification.state == "sent"` (the hub applies
   quiet hours/mutes/dedupe) and the notification is withdrawn when the item leaves open/read (e.g.
   an approval answered in the terminal). Approve/deny actions are offered only for plain permission
   requests and always re-validate the live approval before answering. An ask (attention kind `ask`)
   alerts on the attention channel with up to three option buttons (four options: the default is kept;
   "(default)" on its button, "Defaults to X in 42m" in the text); a button re-fetches the ask and
   answers through `asks.answer` with the same per-intent `operation_id` as the in-app button. An
   `ask.changed` event for a decided ask (answered elsewhere, defaulted, expired, withdrawn,
   superseded) withdraws the notification at once; tapping opens the ask's sheet. Exempt the app from battery
   optimization for screen-off delivery.
3. **`fcm` flavor (optional).** Exists only when `app/google-services.json` is present (never commit it;
   `.gitignore` blocks it). Then the build has `oss` and `fcm` flavors and task names gain the flavor
   (`assembleFcmRelease`, `testOssDebugUnitTest`, …). The fcm build registers its token via
   `notifications.register` (provider `fcm`) and handles data-only messages with the
   `contracts.PushDataKeys` (`attention_id, kind, severity, deep_link, group_key, action, title`):
   `dismiss` cancels the alert; `notify` fetches the item with `attention.get` first (generic alert without
   controls if the hub is unreachable). Setup: create a Firebase project with Android apps
   `dev.jhh3.pyre` (and `dev.jhh3.pyre.debug`), download `google-services.json` to `android/app/`,
   configure the hub with the same Firebase project's service account.

Notification taps open `pyre://<kind>/<id>`; the target screen re-fetches the object, so stale approvals show their current state.

## Design

Phone-first and calm, following the TUI clean-up principles: one accent (Material You wallpaper colour,
or Pyre's flame palette when wallpaper colours are off), colour only for what needs you, whitespace
instead of rules and boxes, plain words. Five destinations (Sessions, Inbox, Projects, Machines, Accounts); settings
and activity open from the top bar. Edge-to-edge with predictive back, 48dp touch targets,
TalkBack labels, layouts that wrap at 200% font size. Decisions and before/after screens:
[docs/evidence/android/ux-redesign-2026-10-05.md](../docs/evidence/android/ux-redesign-2026-10-05.md);
captures in [tests/android/screens/](../tests/android/screens/).

## Build

Prerequisites: JDK 17+ (verified with Homebrew OpenJDK 23.0.2), Android SDK with platforms
`android-37.0` (compile) and `android-36`, build-tools 36.0.0. Install them with the SDK's
`sdkmanager "platforms;android-37.0" "platforms;android-36" "build-tools;36.0.0"`. Point Gradle at
the SDK with `android/local.properties` (`sdk.dir=/path/to/Android/sdk`, gitignored) or `ANDROID_HOME`.

Toolchain (S-H, versions checked against Google Maven / Maven Central on 2026-10-05): Gradle 9.8.0
(wrapper, checksum-pinned) · AGP 9.4.1 with built-in Kotlin (no `kotlin-android` plugin) · Kotlin
2.4.20 (Compose + serialization compiler plugins) · Compose BOM 2026.09.00 · OkHttp 5.5.0 (tests on
`mockwebserver3`) · Robolectric 4.17 · compile SDK 37 (required by the BOM's libraries), **target SDK
36** · min SDK 26 · Java/Kotlin target 17. R8 runs in full mode (AGP default; not overridden).

```sh
android/gradlew -p android assembleDebug        # app/build/outputs/apk/debug/app-debug.apk
android/gradlew -p android testDebugUnitTest    # JVM + Robolectric tests
android/gradlew -p android lintDebug
android/gradlew -p android assembleRelease      # signed if the signing file exists (below)
bash android/scripts/mockhub-it.sh              # client flow against pyre-mockhub (MOCK evidence)
bash android/scripts/realhub-it.sh              # client flow against a REAL local pyre-hub + pyred
bash android/scripts/protocol-it.sh             # hub swapped 1..1 -> 2..2 -> 1..1 -> 1..2 under the client
```

Measured sizes (2026-10-05, v0.2.0 / versionCode 2): debug APK 15.4 MB (`dev.jhh3.pyre.debug`,
unminified); release APK 2.1 MB (2,129,442 bytes; R8 full mode, APK Signature Scheme v2, signed with
the owner key, certificate SHA-256 `13:16:54:7a:…:aa:a0`). `bash android/scripts/release-verify.sh`
builds it and prints the signer digest so an update can be checked against the installed build. The debug build uses the
`.debug` application id suffix and the SDK debug key, so it installs next to the release build.

## Install and update (sideload)

First install, from a machine with `adb` and the phone connected (USB debugging, or wireless
debugging):

```sh
shasum -a 256 -c dist/android/SHA256SUMS          # or compare with the published sha256
adb install -r dist/android/pyre-android-<version>.apk
```

Without adb, copy the APK to the phone and open it from Files or the browser. Android asks you
to allow "Install unknown apps" for that app the first time.

Updating:

1. Build the new release with a higher version code. The code must increase on every release,
   because Android refuses to install a lower one:
   `android/gradlew -p android assembleRelease -PpyreVersionCode=4 -PpyreVersionName=0.2.2` (0.2.1 is versionCode 3).
2. Check that it is signed with the release key. The certificate SHA-256 printed by
   `apksigner verify --print-certs` must equal the fingerprint below.
3. Run `adb install -r <new.apk>`, or open the APK on the phone. Android installs it over the
   old version and keeps the app's data (pairing token, cache, outbox).

An APK signed with any other key, such as the SDK debug key or a lost and recreated release
key, fails with `INSTALL_FAILED_UPDATE_INCOMPATIBLE`. The only way past that error is to
uninstall, which deletes the pairing and any offline outbox. After an uninstall, pair again
with `pyre pair-client --kind android`. The debug build (`dev.jhh3.pyre.debug`) installs next
to the release build and does not touch its data.

## Release signing

The owner's release key was created on 2026-10-04 and lives outside the repository:

| File | Contents |
|---|---|
| `~/.config/pyre/pyre-release.jks` | PKCS12 keystore, alias `pyre`, RSA 4096, `CN=Pyre owner`, valid until 2054 |
| `~/.config/pyre/android-signing.properties` | `storeFile`, `storePassword`, `keyAlias`, `keyPassword` (randomly generated password) |

The directory is mode 0700 and both files are 0600. Release certificate SHA-256 (public; every
release APK must show it):

```text
13:16:54:7A:73:D0:2C:07:F0:E2:F3:75:EE:0F:E3:ED:F9:D2:A0:9D:A4:67:F2:48:E7:5D:E3:C6:51:1F:AA:A0
```

**Back up both files together**, for example in a password manager or an encrypted offline
backup. Do not put them in Git, in the hub backup or on a shared drive. An update has to be
signed with this exact key. If the key or its password is lost, every installed phone must
uninstall the app and pair again. Neither the repository nor `uninstall.sh --purge` deletes
`~/.config/pyre` as a directory.

On a new build machine, restore both files to `~/.config/pyre/` with the same modes. If the
JKS is in a different place, edit `storeFile` (a leading `~` is expanded).

`assembleRelease` signs automatically when the properties file has all four keys. Without the
file, the release APK is unsigned and cannot be installed.
`-PpyreSigningProperties=/abs/path` selects a different file, which is how throwaway
verification builds are signed. Verify a build:

```sh
$ANDROID_HOME/build-tools/36.0.0/apksigner verify --print-certs \
  android/app/build/outputs/apk/release/app-release.apk
```

To create a key for a different owner or fork, run this once. keytool prompts for the password,
which you then write into the properties file:

```sh
mkdir -p ~/.config/pyre && chmod 700 ~/.config/pyre
keytool -genkeypair -keystore ~/.config/pyre/pyre-release.jks -storetype PKCS12 \
  -alias pyre -keyalg RSA -keysize 4096 -validity 10000 -dname "CN=Pyre owner"
chmod 600 ~/.config/pyre/pyre-release.jks ~/.config/pyre/android-signing.properties
```

With PKCS12, `keyPassword` must equal `storePassword`. Build evidence for v0.1.0-rc1 (APK
sha256, size, certificate) is in
[docs/evidence/release/BUILD-2026-10-04.md](../docs/evidence/release/BUILD-2026-10-04.md).

## Pairing a device

On a paired machine create a pairing ticket (`clients.pair`, kind `android`; via the Pyre CLI),
then open/paste its `pyre://pair` link in the app. To exercise the deep link over adb:

```sh
adb shell am start -a android.intent.action.VIEW -d 'pyre://pair?hub=https%3A%2F%2Fhub.example%3A7443&pin=<sha256>&id=<pairing_id>&code=<code>'
```

The app always shows a confirmation screen (hub, certificate pin, device name) before exchanging the code.
Against an emulator and a local `pyre-mockhub --tls`, use `https://10.0.2.2:7788` with the printed pin.

## Protocol mismatch

The app speaks protocol 1 (`X-Pyre-Protocol` on every request). A hub that does not accept it answers
`protocol_mismatch` (HTTP 426) before running anything:

- Pairing is refused before the one-time code is redeemed (the hub checks the header on
  `/v1/pair/exchange`): the screen says which protocols the hub supports, nothing is stored, and an
  existing pairing is untouched. The same link still works once the versions overlap.
- A paired app keeps its credential. Screens show the red "Protocol mismatch" banner and cached data;
  queued actions (outbox) stay pending instead of failing; the hub stream pauses and retries at its
  maximum backoff instead of turning itself off. When the hub is rolled back (or the app updated), the
  next call clears the banner, the outbox replays once (same operation id) and the stream resumes.

`scripts/protocol-it.sh` checks this against real hubs built from this tree (protocol 2 by
`go build -overlay`).

## Terminal frames

Implemented per `routes.go` `PathTerminal` (v1.2): first client text frame
`{"type":"auth","ticket":"…","cols":N,"rows":M}` (input and resizes issued before the handshake are held back
and folded into it, because OkHttp writes frames queued before open first); binary frames carry raw terminal bytes both ways;
client text `{"type":"resize",…}`; server text `{"type":"error","error":{…}}` before close; close code
4000 + HTTP status, 1000 normal. The descriptor URL must be on the paired hub or the ticket is not sent.
Verified against a real local hub + pyred shell adapter (`scripts/realhub-it.sh`): typed `echo`
rendered from the PTY; reusing the one-time ticket returned an error (409), no output.

## Third-party code

`android/termux-terminal/` is a vendored copy of Termux's `terminal-emulator` and `terminal-view`
modules (v0.118.3), which upstream's LICENSE.md releases under Apache-2.0 as an exception to the
GPLv3 app. The JNI subprocess code was removed and `TerminalSession` rewritten to be fed by the relay;
see `termux-terminal/NOTICE`, `LICENSE` and `UPSTREAM-LICENSE.md`. No other Termux code is included.
Release notices must carry that NOTICE.

## Code map

`model/` contract mirror (snake_case via naming strategy, string enums tolerant of new values) ·
`net/` HubClient (RPC + retries keyed by operation_id), pinning, event stream · `data/`
credentials, cache, outbox, intent ids, repository · `domain/` status honesty, delivery facts,
approval answering · `notify/` notifier, hub-stream service, action receiver, push glue ·
`terminal/` VT screen + socket · `ui/` Compose screens with manual DI (`AppGraph`) and a tiny
navigator. `src/fcm/` is the optional Firebase flavor.

## Provenance

The G0 decision was native adapters without HAPI. HAPI's AGPL Android client was read for ideas only;
no files or code were copied. Ideas reflected generically: conditional Google-services plugin so the
default build needs no Firebase config, and a fragment
version floor because firebase-messaging pulls an old fragment.

## Device verification

[docs/evidence/android/emulator-2026-10-05.md](../docs/evidence/android/emulator-2026-10-05.md):
signed rc1 → 0.2.1 update with pairing, cache and outbox kept on a Pixel 10 Pro; notifications
(screen-off, forced Doze), approvals, questions, artifact/document review and the HTML preview on an
Android 16 emulator. Still open: those emulator-only steps on a physical phone with a signed build that
contains the 2026-10-05 fixes, FCM, Twingate and long screen-off periods. See `tests/android/README.md`.
