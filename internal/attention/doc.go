// Package attention is the hub's durable Inbox.
//
// A Store (contracts.AttentionAPI) keeps attention items in the shared hub
// SQLite database under the "attention" migration namespace. A Deriver
// consumes the hub event stream through contracts.EventAPI with a persisted
// cursor and turns events into items:
//
//	approval.requested             -> "approval" / "question" (urgent, one per approval)
//	approval.resolved / .withdrawn -> that item withdrawn on every surface
//	approval.withdrawn with state expired -> also an urgent "approval" item for the session
//	                                  ("Answer in its terminal: …", group terminal:<approval>) until
//	                                  the native prompt is resolved or the run ends
//	message.stored to the owner    -> "message" (collapsed per thread, or per sender without a thread;
//	                                  summary = excerpt of the newest message)
//	message.stored from the owner  -> the matching message item marked handled (the owner replied)
//	message.ack (ack/reply) on a message to the owner -> that message's item marked handled
//	message.delivery indeterminate -> "indeterminate_delivery" (urgent); a later accepted/rejected/
//	                                  expired/cancelled withdraws it
//	account.exhausted              -> "account_exhausted" (normal, grouped per provider); none when it
//	                                  belongs to a run's fallback plan (that plan's account.fallback raises it)
//	account.fallback waiting|intervention -> "account_exhausted" (urgent, grouped per provider)
//	run.ended with state failed    -> "failure" (grouped per session; "unknown" is never failure); none
//	                                  for a session under a parent (the hub reports it there); an
//	                                  owner-started launch with no account capacity joins the one
//	                                  "capacity:<provider>" item listing its sessions (members.go)
//	run.ended (any)                -> the session's "account_exhausted" items withdrawn
//	run.started / run.observed live, session.updated archived
//	                               -> the session's "failure" / "account_exhausted" items withdrawn and
//	                                  it leaves capacity groups
//	artifact.published with review_requested -> "review_ready" (grouped per artifact)
//	comment.created mentioning "owner"       -> "comment" (grouped per review target)
//	document.conflict (non-owner author)     -> "document_conflict" (grouped per document)
//	update.result failure          -> "update" (grouped per tool+node; a later success withdraws it)
//	account.grant dead|expired     -> "node" "Re-login needed for <label> on <node>" (grouped per
//	                                  grant; a later valid or revoked state withdraws it)
//	ask.changed open (created or merged), run.observed running, run.ended, session archived
//	                               -> the session's "waiting" item withdrawn (raised by the hub at a
//	                                  top-level session's turn end, Store.RaiseWaiting; one per session)
//
// Agent-to-agent messages (including progress chatter) never create
// attention, and attention.changed events are never consumed, so the Inbox
// cannot loop on its own output. Repeated events collapse into the active
// item for their GroupKey (Count++, UpdatedAt); a read item that receives
// new activity reopens. Approvals and specific deliveries are one-shot: they
// are never reopened, and a resolution seen first leaves a withdrawn
// tombstone so a late duplicate request cannot resurrect it.
//
// Titles and summaries come from short record titles and fixed phrases;
// approval, comment, and transcript bodies are never copied. The
// exceptions are a message item's summary (a sanitized excerpt of the
// message, MessageExcerpt) and a "waiting" item, whose title is the
// question's first line and summary an excerpt of the session's last
// message around it. Push payloads carry generic titles only.
//
// # Event payload conventions
//
// contracts v1 does not pin event payload schemas. The deriver accepts the
// natural record as payload (Approval, MessageView or MessageEnvelope,
// MessageView/DeliveryStatus/DeliveryReceipt, AccountObservation,
// FallbackPlan (+ optional "provider"), Run, PublishReceipt or
// ArtifactVersion (+ "review_requested"), ReviewComment, DocumentProposal,
// ToolVersion (+ optional "outcome", "rolled_back", "node_id"), DeviceGrant) and falls
// back to Event.Object / SessionID / ProjectID / Origin.
//
// # Restart and reset
//
// Each event is applied in one transaction that also advances the cursor
// and records the event ID, so restarts resume exactly and re-sent events
// are idempotent. On reset_required the deriver re-derives pending
// approvals and indeterminate deliveries from current state (Snapshotter),
// withdraws one-shot items that are no longer pending, records the gap
// explicitly (Store.Gaps), and resumes from the hub's snapshot cursor.
//
// # Notifications
//
// Every transition into "open" increments a per-item transition sequence
// and queues one notify job in the same transaction (a transactional
// outbox); leaving open/read after an FCM push queues a dismiss job. The
// Store implements notifications.Source; internal/notifications.Dispatcher
// delivers and records NotificationStatus without changing item state.
package attention
