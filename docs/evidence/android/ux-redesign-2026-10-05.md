# Android UI/UX redesign (2026-10-05)

Package W-android-ux, branch `w-android-ux`. The owner, looking at the app on a Pixel 10 Pro: "the mobile
app view looks like actual dog shit / not usable / ugly — figure out how to streamline the UI/UX."

This pass rebuilds every screen in the existing Compose code. Behaviour and contracts are unchanged: the
same ViewModels, repository calls, operation ids, outbox, delivery facts and approval answerer. The only
new behaviour is UI-level: one-tap answers on inbox cards (through the existing `ApprovalAnswerer`),
a confirm step before Interrupt, quiet-hours changes saved on each change instead of with a Save button,
and an appearance setting.

**Where it ran.** All screenshots come from an Android 16 emulator (API 36, google_apis arm64, 1080×2400
@ 420 dpi, 2 GB, isolated AVD and adb server), never the owner's phone. Data is synthetic: the
`pyre-mockhub` demo fleet (`tests/fixtures/mock/demo-fleet.json`) over pinned TLS through `adb reverse`;
the live terminal comes from a real local `pyre-hub` + `pyred --dev-mock` shell session in
`~/.cache/pyre-wandroidux`. Modes: dark and light, system font scale 1.0 and 2.0 (`-2x`). The emulator's
wallpaper gives Material You a lavender palette; the owner's phone will show its own wallpaper colours
(see "Pyre palette" for the fallback).

## What was wrong

The owner's Settings screen (phone capture) and every other screen on the
emulator ([before set](../../../tests/android/screens/)), critiqued honestly:

| Screen | Problems |
| --- | --- |
| Settings | A wall of internals first: hub URL, TLS pin, client id, hub version. Firebase build instructions ("build the `fcm` flavor with your own app/google-services.json…") shown to the user. Label/value columns 120dp wide that wrap into ragged blocks. Quiet hours as free-text `HH:MM` fields with a manual Save. Three different control styles (switch, text link, filter chips) for similar choices. Uppercase orange headers everywhere, so nothing stands out. |
| Home (Sessions) | Opens on the fleet list, not on what needs you. Every row carries two coloured badges (a "Needs you · 2" pill and a coloured status), dividers between every row, green/blue/orange all at once. The "+ Launch" FAB covers the last row. |
| Bottom bar | Four tabs with text glyphs (`●`, `!`, `▤`, `≡`) instead of icons; "More" is a junk drawer of four text rows (Machines, Accounts, Artifacts, Settings). |
| Inbox | Every card has "Read" and "Handled" buttons and a "open · updated 5m ago" status line; nothing can be answered from the list, so every approval is two screens away. Kind pills compete with titles. |
| Approval / question | Usable, but the run id is shown as primary information, "permission · pending" is jargon, and the free-text answer sits next to choice buttons with equal weight. |
| Session detail | Starts with eight label/value rows (harness, machine, directory, account, observed, capabilities list) before anything useful. A paragraph explaining the terminal. The transcript is `system · Oct 4 03:50` labels over plain text. Every message card prints all four delivery facts plus From/To in small type. "Interrupt" is a one-tap text button next to the composer. |
| Terminal | A permanent orange "TERMINAL — keystrokes go straight…" banner, a status line, then the terminal; extra keys are small text buttons. Not full screen. |
| Message | Message id and RFC 3339 timestamps as headings; delivery facts as a dense block. |
| Projects / project / document | "All published artifacts (incl. standalone) ›" as an orange text link; repeated revision/digest rows; raw Markdown tables (`| case | p99 |`). |
| Artifact | Version picker as `◉ v1 · bench.md · text/markdown · 84 B · …`, a digest wall, then the comment form with three anchor chips and a session-id field always open. |
| Machines | One card per machine with ~14 label/value rows each (load, memory, RSS, CPU, versions); the useful bit (online? which agents?) is lost. |
| Accounts | With no accounts: a disclaimer paragraph and an empty screen, no hint of what to do. |
| Pairing | Shows the raw `pyre://pair?code=…` link, so the one-time code is on screen; the command in the instructions is wrong (`pyre pair --android`); a "DEVELOPER: allow plain http" switch is always visible. |
| Offline | A stuck "Hub unreachable — showing cached data; actions queue where safe." banner on every screen, plus a second "Cached 1m ago · hub unreachable" box per screen. |

## Design decisions

Same principles as the TUI clean-up (`tests/tui/DESIGN.md`): restraint over decoration, one accent,
colour only for what needs action, whitespace instead of rules and boxes, plain language.

- **Home answers "what needs me?".** The app opens on the Inbox. *Needs you* (approvals, questions,
  replies, failures, unknown deliveries) comes first, then *Updates* (reviews, machine status). A
  permission request has **Deny / Allow once** on the card (48dp, equal width, Allow filled), extra
  options ("Always allow") one tap away on the request screen. A question shows its choices as tonal
  buttons and "Type an answer…". Answers go through the same `ApprovalAnswerer` as the request screen and
  notification actions: the live request is re-fetched, the answer is bound to `(approval_id, run_id)`,
  one operation id per intent, so a double tap answers once (`RedesignUiTest`). Read/done moved to a card
  menu; informational updates keep a single "Done".
- **Three destinations.** Inbox (badge with the open count), Sessions, Projects, with Material icons.
  Settings is the gear on every top-level screen; Machines is an icon on Sessions (and tapping a
  machine group opens it); Accounts is an icon on Machines; all artifacts is a row on Projects. "More"
  is gone.
- **Sessions grouped by machine** with quiet group labels (name + dot + "Online"/"Offline · last seen…").
  One row per session: a dot, the title, then "status word · harness · detail". Only "Needs approval"
  and failures are coloured; working is plain text, idle/offline/stale/exited are muted (same rule as the
  TUI). A count badge replaces the "Needs you" pill.
- **Session detail** leads with the status in large type ("Needs approval", "Working"), one line of where
  and when, then **Terminal** and **Interrupt** (now confirmed) buttons. Run facts moved into a collapsed
  "Details". Requests that need you get a card with "Review and answer". The transcript reads as a chat:
  your prompts on the right, the agent on the left, system and tool lines as quiet text. Message cards keep
  From/To (A04 identity) and show the runtime fact ("Delivery: received by node, not yet delivered…");
  all four facts, evidence and thread are one tap away in "Delivery details", open by default when the
  outcome is unknown. The composer is a rounded field with a send button; "Steer now" only when the
  adapter supports it.
- **Terminal is full screen**: a slim header (back, "Terminal", a dot with "Live · keys go straight to the
  session" or "View only"), the real TUI edge to edge, and a 48dp extra-keys row (Esc, Ctrl, Alt, Tab,
  arrows, ^C, Home/End, PgUp/PgDn) above the keyboard; sticky Ctrl/Alt are highlighted and announced.
  Reconnect and take-over are in the header menu.
- **Settings holds only what a person changes**: *Notifications* (Allow notifications, Live alerts =
  hub-stream mode, Alerts with the screen off = battery exemption), *Quiet hours* (a switch, start/end
  rows that open the Material time picker, a searchable time-zone list, "Alert me about" radio rows; each
  change is saved to the hub with a "Saved"/"Couldn't save · Retry" line), *Appearance* (system / light /
  dark, wallpaper colours), *This phone* (Unpair, confirmed in a dialog). Hub address, security in words,
  the certificate fingerprint in groups of four, client id, versions and push status are inside a
  collapsed **About this connection**. Firebase build instructions are no longer shown anywhere in the
  app; the README keeps them.
- **Pairing** is a two-step first run: "Connect to your hub" with the correct command
  (`pyre pair-client --kind android`), a big **Paste link** button and "Type it instead" (masked field).
  Then "Connect to this hub?": host:port, "Encrypted. This phone will trust only this certificate:" with
  the fingerprint in groups of four to compare, the phone's name, **Connect**. The one-time code is never
  rendered. The developer cleartext switch appears only for an `http://` link.
- **Material 3 done properly**: dynamic colour (Material You) by default with Pyre's own tuned palette as
  fallback (flame accent; near-black neutral dark surfaces), the M3 type scale (titleLarge screen titles,
  titleMedium card titles, bodyLarge rows, bodyMedium supporting, labelLarge meta), M3 top app bar and
  navigation bar, edge-to-edge with Scaffold insets (`enableEdgeToEdge` was already on), predictive back
  (`enableOnBackInvokedCallback` + `PredictiveBackHandler`: the screen scales and slides with the
  gesture, then pops), pull to refresh on Inbox and Sessions.
- **States that say what to do.** Empty: "You're all caught up", "No sessions yet · New session",
  "No accounts reported — machines report subscriptions here once rota manages them". Loading: a centred
  spinner. Errors: "Couldn't load this", the reason, **Try again**. Offline: one calm line under the top
  bar, "Can't reach your hub. Showing saved data. · Retry" (Retry reloads the open screen and flushes the
  outbox); per-screen copies say "Saved copy from 12m ago". Only a refused protocol or a revoked phone get
  a tinted notice with an action ("Pair again").
- **Accessibility.** Every icon button has a label ("Back", "Settings", "Machines", "More actions for
  <title>"); status is always a word next to the dot; rows merge into one TalkBack node; the inbox tab
  announces "Inbox, 7 new"; the extended FAB is labelled explicitly (its animated label is hidden from
  accessibility services, found with uiautomator); extra keys have spoken names. Controls are ≥ 48dp.
  Label-above-value layouts replace fixed 120dp columns, so 200% font wraps instead of clipping; long
  single-line headers (top bar title, card kind) ellipsize. Pyre's palettes pass WCAG 4.5:1 for every
  text pair (`ThemeContrastTest`); Material You schemes carry their own tonal contrast.

## Before and after

Dark, font scale 1.0 unless noted. Every screen also exists as `-light`, `-dark-2x` and `-light-2x` in
[`tests/android/screens/`](../../../tests/android/screens/).

| Screen | Before | After |
| --- | --- | --- |
| Home | ![](../../../tests/android/screens/before-sessions-dark.webp) | ![](../../../tests/android/screens/after-inbox-dark.webp) |
| Inbox (rest) | ![](../../../tests/android/screens/before-inbox-dark.webp) | ![](../../../tests/android/screens/after-inbox-more-dark.webp) |
| Approval | ![](../../../tests/android/screens/before-approval-dark.webp) | ![](../../../tests/android/screens/after-approval-dark.webp) |
| Question | ![](../../../tests/android/screens/before-question-dark.webp) | ![](../../../tests/android/screens/after-question-dark.webp) |
| Message | ![](../../../tests/android/screens/before-message-dark.webp) | ![](../../../tests/android/screens/after-message-dark.webp) |
| Sessions | ![](../../../tests/android/screens/before-sessions-dark.webp) | ![](../../../tests/android/screens/after-sessions-dark.webp) |
| Session | ![](../../../tests/android/screens/before-session-dark.webp) | ![](../../../tests/android/screens/after-session-dark.webp) |
| Session activity | ![](../../../tests/android/screens/before-session-messages-dark.webp) | ![](../../../tests/android/screens/after-session-activity-dark.webp) |
| Messages | ![](../../../tests/android/screens/before-session-messages-dark.webp) | ![](../../../tests/android/screens/after-session-messages-dark.webp) |
| Terminal (live) | ![](../../../tests/android/screens/before-terminal-live-dark.webp) | ![](../../../tests/android/screens/after-terminal-live-dark.webp) |
| Terminal (SSH route) | ![](../../../tests/android/screens/before-terminal-dark.webp) | ![](../../../tests/android/screens/after-terminal-dark.webp) |
| New session | ![](../../../tests/android/screens/before-launch-dark.webp) | ![](../../../tests/android/screens/after-launch-dark.webp) |
| Projects | ![](../../../tests/android/screens/before-projects-dark.webp) | ![](../../../tests/android/screens/after-projects-dark.webp) |
| Project | ![](../../../tests/android/screens/before-project-dark.webp) | ![](../../../tests/android/screens/after-project-dark.webp) |
| Document | ![](../../../tests/android/screens/before-project-docs-dark.webp) | ![](../../../tests/android/screens/after-document-dark.webp) |
| Artifact | ![](../../../tests/android/screens/before-artifact-open-dark.webp) | ![](../../../tests/android/screens/after-artifact-open-dark.webp) |
| All artifacts | ![](../../../tests/android/screens/before-artifacts-dark.webp) | ![](../../../tests/android/screens/after-artifacts-dark.webp) |
| More → top bar | ![](../../../tests/android/screens/before-more-dark.webp) | (no "More": icons in the top bar) |
| Machines | ![](../../../tests/android/screens/before-machines-dark.webp) | ![](../../../tests/android/screens/after-machines-dark.webp) |
| Accounts (empty) | ![](../../../tests/android/screens/before-accounts-dark.webp) | ![](../../../tests/android/screens/after-accounts-dark.webp) |
| Settings | ![](../../../tests/android/screens/before-settings-dark.webp) | ![](../../../tests/android/screens/after-settings-dark.webp) |
| Settings (lower) | ![](../../../tests/android/screens/before-settings-bottom-dark.webp) | ![](../../../tests/android/screens/after-settings-about-dark.webp) |
| Pairing | ![](../../../tests/android/screens/before-pairing-dark.webp) | ![](../../../tests/android/screens/after-pairing-dark.webp) |
| Pairing confirm | (same screen) | ![](../../../tests/android/screens/after-pairing-confirm-dark.webp) |
| Offline | ![](../../../tests/android/screens/before-offline-dark.webp) | ![](../../../tests/android/screens/after-offline-dark.webp) |

Light and 200% font:

| Screen | Before light | After light | Before 200% | After 200% |
| --- | --- | --- | --- | --- |
| Home | ![](../../../tests/android/screens/before-sessions-light.webp) | ![](../../../tests/android/screens/after-inbox-light.webp) | ![](../../../tests/android/screens/before-sessions-dark-2x.webp) | ![](../../../tests/android/screens/after-inbox-dark-2x.webp) |
| Session | ![](../../../tests/android/screens/before-session-light.webp) | ![](../../../tests/android/screens/after-session-light.webp) | ![](../../../tests/android/screens/before-session-light-2x.webp) | ![](../../../tests/android/screens/after-session-light-2x.webp) |
| Settings | ![](../../../tests/android/screens/before-settings-light.webp) | ![](../../../tests/android/screens/after-settings-light.webp) | ![](../../../tests/android/screens/before-settings-dark-2x.webp) | ![](../../../tests/android/screens/after-settings-dark-2x.webp) |
| Pairing | ![](../../../tests/android/screens/before-pairing-light.webp) | ![](../../../tests/android/screens/after-pairing-confirm-light.webp) | ![](../../../tests/android/screens/before-pairing-light-2x.webp) | ![](../../../tests/android/screens/after-pairing-confirm-light-2x.webp) |

Pyre palette (wallpaper colours off):

| Dark | Light |
| --- | --- |
| ![](../../../tests/android/screens/after-pyre-inbox-dark.webp) | ![](../../../tests/android/screens/after-pyre-inbox-light.webp) |
| ![](../../../tests/android/screens/after-pyre-session-dark.webp) | ![](../../../tests/android/screens/after-pyre-session-light.webp) |

## Verification

- `android/gradlew -p android clean assembleDebug testDebugUnitTest lintDebug` on the branch after merging
  main (e07be2f): BUILD SUCCESSFUL; **99 tests, 0 failed, 3 skipped** (the mock/real/protocol hub ITs that
  need their scripts); lint: **No issues found**. Before this pass the suite had 90 tests; the new ones are
  `RedesignUiTest` (6) and `ThemeContrastTest` (3). `ScreensUiTest.sessionsShowOfflineNodeHonestly` now
  matches the status word as a substring because it leads the row's supporting line.
- On the emulator, every screen in the tables was reached by a scripted tour (uiautomator taps, guarded
  to the Pyre package) in four modes; 187 captures, synthetic data only. The before set used the
  pre-redesign debug build of main e6dcc7a against the same mock hub.
- Checked by hand on the emulator: one-tap Allow/Deny and question choices are present on cards (the
  double-tap guarantee is covered by `RedesignUiTest` against a fake hub, so the mock fleet's requests
  stayed pending for the screenshots); back navigation through Sessions → Machines → Accounts and back;
  the hub going away shows the inline "Can't reach your hub · Retry" line within one poll; a revoked
  pairing shows "Pair again"; a live web terminal through the real local hub accepts typed input with the
  extra-keys row above the keyboard.
- Found while checking accessibility: Material's `ExtendedFloatingActionButton` exposes no text to
  uiautomator/TalkBack, so "New session" is labelled explicitly.
- Review build: debug APK `pyre-android-ux-debug.apk` (`dev.jhh3.pyre.debug`, SDK debug key,
  15,284,959 bytes, sha256 `012c90bc…8e28b4a`), built from the branch head; installs next to the signed
  release. The lead builds the signed release after merging.

## Not done / limitations

- **Not run on the physical Pixel.** The owner's phone was in use for A18; everything here ran on the
  emulator. Wallpaper colours on the Pixel will differ from the emulator's lavender default, and the
  physical-device check of gesture back, TalkBack and the keyboard/extra-keys layout remains to do.
- TalkBack was not driven end to end; labels were checked through the accessibility tree (uiautomator)
  and Compose semantics in tests. Contrast is asserted for Pyre's own palettes only; dynamic schemes rely
  on Material's tonal palette.
- At 200% some single-line labels ellipsize by design (top bar title, inbox card kind, long session
  detail lines); nothing clips or overlaps, and body text wraps.
- Pairing still takes a pasted or opened link: there is no in-app QR scanner (no camera dependency was
  added, and the CLI does not print a QR code). The pairing screen cannot show a hub *name* because the
  hub's name is not known before pairing; it shows host:port and the certificate fingerprint.
- Session detail opens the request screen for answers ("Review and answer"); one-tap answers are on the
  Inbox cards only.
- The debug and screenshot builds were run against the mock hub's synthetic fleet; the mock has no web
  terminal, so its "Terminal" shows the SSH route, and the live terminal came from a real local hub.
- `LocalPrefs` gained `themeMode` and `dynamicColor`; older builds ignore the extra keys
  (`ignoreUnknownKeys`), so a downgrade keeps the other settings.

## Proposed MATRIX rows

| Case | Requirements | Status | Environment | Evidence | Notes |
| --- | --- | --- | --- | --- | --- |
| A18 (UI note) | R13 | pass (status unchanged; UI redesigned) | Android 16 emulator; synthetic mock hub + real local hub for the terminal | [ux redesign](../android/ux-redesign-2026-10-05.md) | Redesigned UI (Inbox home with one-tap answers through the same answerer, three destinations, settings without internals, pairing without the code on screen, full-screen terminal); behaviour and contracts unchanged; 99 JVM tests + lint clean. The new UI has not run on the physical Pixel yet; a short phone re-check of A18 with the next signed build is recommended. |
