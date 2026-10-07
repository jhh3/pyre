// Package artifacts implements contracts.ArtifactAPI: immutable, versioned
// published output stored in a content-addressed blob store with manifests
// in the hub's SQLite database (migration namespace "artifacts", plus
// sqlitex.OperationsMigration).
//
// # Composition (hub, WP05)
//
//	arts, err := artifacts.New(ctx, db, sink, artifacts.Options{
//		DataDir:  filepath.Join(hubDataDir, "artifacts"),
//		Projects: projStore,          // *projects.Store (validates project IDs)
//		Clock:    clock,
//		// MaxBytes (default 512 MiB), MaxTotalBytes, UploadTTL (24h), VerifyTTL (0 = every open)
//	})
//	mux.Handle("PUT /v1/uploads/{upload_id}", ...) // auth, then arts.UploadChunk(ctx, id, offset, r.Body)
//	mux.Handle("GET /v1/content/{version_id}", auth(artifacts.ContentHandler(arts)))
//	previewMux.Handle("GET /preview/{version_id}/{token}", artifacts.PreviewHandler(arts, tickets))
//	go every(time.Hour, func() { arts.CleanupExpired(ctx); arts.VerifyBlobs(ctx) })
//
// Every mutation (BeginUpload, FinalizeUpload) runs in sqlitex.Idempotent
// with the principal from contracts.PrincipalFrom(ctx); FinalizeUpload emits
// artifact.published inside the publishing transaction. UploadChunk is not
// a Mutation; it is idempotent by byte range instead.
//
// # Storage layout and guarantees
//
//	<DataDir>/blobs/sha256/<hex[0:2]>/<hex[2:]>  immutable verified blobs (0400)
//	<DataDir>/uploads/<upload_id>                staged bytes of open uploads
//
// Guarantees:
//
//   - Storage paths derive only from validated hex digests and store-generated
//     upload IDs. Filenames are display-only (SafeFilename) and are never
//     used as paths, so path traversal is impossible.
//   - Uploads are resumable: Upload.Received is the resume offset; re-sending
//     an identical range is a no-op; overlapping different bytes is
//     ErrConflict; a chunk extending past the declared size is rejected
//     whole with ErrPayloadTooLarge (received unchanged).
//   - FinalizeUpload verifies size and sha256 before publishing. On mismatch
//     nothing is published, the upload becomes "failed" and ErrConflict is
//     returned. On success the blob rename and manifest insert happen in one
//     transaction: readers see no version or a complete, verified one.
//   - Versions are immutable (SQLite triggers; 0400 blobs; content addressed),
//     with seq, previous_version_id, author and provenance.
//   - OpenContent re-verifies the digest (see Options.VerifyTTL) and refuses
//     corrupted blobs; VerifyBlobs sweeps all blobs on a schedule.
//   - Media type: declared, else by extension, else sniffed.
//   - ContentURL = /v1/content/<version_id>. Active types (HTML, SVG, XML)
//     get PreviewURL = /preview/<version_id>/{token}; MintPreviewURL issues
//     a real one-time link via contracts.TokenIssuer (purpose "preview").
//
// # Preview isolation
//
// PreviewHandler authenticates only with the one-time preview ticket (never
// a bearer token or cookie) and serves bytes with
// "Content-Security-Policy: sandbox; default-src 'none'; img-src data:
// 'self'; style-src 'unsafe-inline'", nosniff, CORP same-origin and
// no-referrer, so HTML cannot run script or reach management APIs. Deploy it
// on a separate origin/port from the management API. Tickets are one-time:
// a reload needs a freshly minted URL.
//
// The client-side verified offline cache lives in internal/artifacts/cache.
package artifacts
