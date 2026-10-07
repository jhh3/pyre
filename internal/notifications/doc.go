// Package notifications delivers best-effort alerts for attention items.
//
// The durable Inbox (internal/attention) is the source of truth. This
// package only presents it: a Dispatcher reads notification jobs that the
// attention store queued transactionally with each item state transition,
// applies presentation preferences (per-kind toggles, per-project mute,
// minimum severity, TZ-aware quiet hours), rate-limits and debounces
// bursts, delivers through a provider with bounded retries, and records a
// contracts.NotificationStatus on the item. Delivery status is separate
// from the human acting: nothing here changes an item's State.
//
// # Providers
//
//   - FCM (HTTP v1): data-only messages carrying exactly attention_id, kind,
//     severity, deep_link, group_key, action ("notify" | "dismiss") and a
//     generic title such as "Pyre: approval needed". No message, approval,
//     comment, or transcript text is ever placed in a push; the app fetches
//     details over its authenticated hub connection. Tokens reported
//     UNREGISTERED (or SENDER_ID_MISMATCH / 404) are removed. When an item a
//     phone was alerted about is read, handled, snoozed, or withdrawn
//     (e.g. an approval resolved in the terminal), a silent "dismiss" message
//     tells the app to withdraw the alert.
//   - hub-stream: no outbound push. Items are published on the hub event
//     stream as attention.changed; status "sent" (provider "hub-stream")
//     means prefs allow alerting for that transition, so a stream-holding
//     client (the Android app's optional foreground service, or `pyre
//     watch`) should alert only on attention.changed whose notification
//     state is "sent".
//   - Desktop: a client-side helper for `pyre watch` using osascript
//     (macOS) or notify-send (Linux), invoked without a shell.
//
// # Operational prerequisite for real Android push
//
// Real background push to Android requires all of the following, none of
// which is committed to the repository:
//
//  1. A Firebase project owned by the user, with Cloud Messaging enabled.
//  2. The Android app built with that project's google-services.json
//     (private build input, kept outside Git like the signing key).
//  3. A Google service-account JSON key for the same project with
//     permission to send FCM messages, stored on the hub host with
//     owner-only permissions and passed to the hub as a file path
//     (FCMConfig.ServiceAccountFile). The hub exchanges it for short-lived
//     OAuth2 access tokens (JWT bearer, RS256) and caches them.
//
// Without the service account the hub reports FCM registrations as
// Enabled=false ("FCM not configured on hub") and alerts remain available
// only through the Inbox and the hub event stream. Sideloading the APK or a
// working foreground connection is not evidence that push works; verify
// with the phone backgrounded and the screen off.
package notifications
