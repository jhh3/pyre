// Package contracts defines Pyre's shared, versioned wire and storage
// semantics: identities, records, errors, the hub API, the node protocol,
// the runtime adapter boundary, and fixture bundles.
//
// This package is owned by the integration owner (WP00). Other packages
// consume it; they request changes rather than editing it. Every change to
// these semantics must update tests/fixtures/contracts, the affected
// consumers, and docs/decisions.md in the same integration change.
//
// Conventions (see docs/contracts.md):
//   - IDs are opaque, prefixed strings. Human names are mutable aliases and
//     never deduplication keys.
//   - Times are UTC and informational; durable cursors/sequences order data.
//   - Every mutating request carries an OperationID. Same ID + same body
//     returns the stored result; same ID + different body is a conflict.
//   - A successful durable-write response means committed storage, never
//     completed downstream execution.
//   - project_id is optional everywhere it appears.
//   - Credentials and raw tokens are never serialized into these records.
package contracts

// ProtocolVersion is the hub/node/client wire protocol version. Bump on any
// incompatible change; peers reject mismatches with ErrProtocolMismatch.
const ProtocolVersion = 1

// MinProtocolVersion is the oldest peer protocol this build still accepts.
const MinProtocolVersion = 1

// SchemaVersion is stamped into persisted envelopes and fixture bundles.
const SchemaVersion = 1

// ProtocolHeader carries the client's protocol version on every HTTP call.
const ProtocolHeader = "X-Pyre-Protocol"
