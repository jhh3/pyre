package contracts

import "time"

// Conditional list reads and response compression (HTTP binding, no
// version bump; both are negotiated per request, so old and new peers mix).
//
//   - The hub tags the answer to a ConditionalOps read with a strong ETag
//     over the response body without its snapshot_at. A client that sends
//     that tag back in If-None-Match gets 304 Not Modified with no body and
//     SnapshotAtHeader (the hub's read time, RFC 3339); it reuses the body
//     it kept. A hub that predates this ignores If-None-Match and answers
//     200 as before; a client that predates it never sends the header.
//   - The hub gzips a large JSON answer when the request carries
//     Accept-Encoding: gzip (Go's and OkHttp's transports add it and
//     decompress transparently). Without that header the answer is plain.

// SnapshotAtHeader carries Page.SnapshotAt on a 304 answer.
const SnapshotAtHeader = "X-Pyre-Snapshot-At"

// ConditionalOps are the list reads that clients poll and the hub tags
// with an ETag.
var ConditionalOps = map[string]bool{
	OpListAccounts:  true,
	OpListAttention: true,
	OpListNodes:     true,
	OpListNotes:     true,
	OpListProjects:  true,
	OpListSessions:  true,
	OpAsksList:      true,
}

// SnapshotTime is the page's read time (lets the HTTP binding leave it out
// of the ETag, which then changes only when the content does).
func (p Page[T]) SnapshotTime() time.Time { return p.SnapshotAt }
