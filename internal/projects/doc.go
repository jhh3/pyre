// Package projects implements the hub's optional projects, versioned
// documents and notes/launch context (contracts.ProjectAPI,
// contracts.DocumentAPI and contracts.NoteAPI) on the hub's shared SQLite
// database (migration namespace "projects", plus sqlitex.OperationsMigration).
//
// # Composition (hub, WP05)
//
//	proj, err := projects.New(ctx, db, sink, projects.Options{Clock: clock})
//	arts, err := artifacts.New(ctx, db, sink, artifacts.Options{DataDir: dir, Projects: proj, Clock: clock})
//	proj.SetArtifactIndex(arts) // artifact pointers in launch context
//	rev, err := review.New(ctx, db, sink, proj, arts, review.Options{Clock: clock})
//
// db comes from sqlitex.Open; sink is the hub's contracts.EventSink. Every
// mutation runs in one write transaction wrapped in sqlitex.Idempotent with
// the principal from contracts.PrincipalFrom(ctx) (ErrUnauthorized when
// absent) and appends its events with sink.AppendTx in that transaction:
// project.changed, document.revised, document.conflict, note.created.
//
// # Semantics
//
//   - A project needs only a name. Repos, checkouts (shared, worktree or
//     plain), links, env profile and connection names are optional.
//     UpdateProject is compare-and-swap on Version; Archived=true archives.
//   - Creating a project with a Brief creates the "brief" document at
//     revision 1 and sets BriefDocumentID.
//   - Documents may be standalone (nil project). Slugs are unique within a
//     project, and separately among standalone documents.
//   - Revisions are immutable (enforced by SQLite triggers) and carry a
//     sha256 digest. ProposeRevision commits iff the head equals the base
//     revision; otherwise the caller's content is preserved as a conflicted
//     DocumentProposal and ErrConflict carries {current_head, proposal_id}.
//     There is no automatic merge. Re-proposing identical content on top of
//     the new head marks the earlier conflicted proposal applied;
//     DiscardProposal marks it discarded (content kept, no event).
//   - PromoteNote is the only way a note becomes document content: a new
//     document, or a CAS proposal on an existing one; Note.PromotedTo
//     records the revision.
//   - Event payloads follow the pinned v1 table in contracts/events.go
//     (Project; DocumentRevision/DocumentProposal without content; Note
//     without body).
//   - List filters with a *string project: nil = all, "" = standalone only,
//     otherwise that project.
//   - GetLaunchContext returns a bounded brief (default 8 KiB, max 64 KiB):
//     project, truncated current brief, recent decision/handoff titles,
//     document and artifact pointers. It records the brief revision and the
//     references included and never dumps transcripts or note bodies.
//
// Lookup helpers used by sibling services: ProjectExists (artifacts) and
// LookupRevision (review).
package projects
