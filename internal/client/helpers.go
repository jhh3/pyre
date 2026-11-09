package client

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// MaxPages bounds All so a misbehaving server cannot loop forever.
const MaxPages = 10000

// All follows NextCursor until the end and returns every item. fetch is
// called with "" first. A repeated cursor or more than MaxPages pages is an
// error rather than an infinite loop. Each page is a separate snapshot:
// items changed between pages may be missing or repeated.
//
//	sessions, err := client.All(ctx, func(ctx context.Context, cur string) (contracts.Page[contracts.SessionView], error) {
//		return api.ListSessions(ctx, contracts.ListSessionsRequest{PageRequest: contracts.PageRequest{Cursor: cur}})
//	})
func All[T any](ctx context.Context, fetch func(ctx context.Context, cursor string) (c.Page[T], error)) ([]T, error) {
	var out []T
	cursor := ""
	seen := map[string]bool{}
	for i := 0; i < MaxPages; i++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		page, err := fetch(ctx, cursor)
		if err != nil {
			return out, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" {
			return out, nil
		}
		if seen[page.NextCursor] {
			return out, c.Errorf(c.ErrInternal, "pagination cursor %q repeated", page.NextCursor)
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
	return out, c.Errorf(c.ErrInternal, "pagination exceeded %d pages", MaxPages)
}

// PublishMeta describes an artifact version to publish. Either ArtifactID
// (new version of an existing artifact) or Title (new artifact).
type PublishMeta struct {
	ArtifactID  string
	ProjectID   *string
	Title       string
	Description string
	Filename    string
	MediaType   string
	Note        string
	Provenance  c.Provenance
	// ReviewRequested asks the owner to review the published version.
	ReviewRequested bool
	// OperationID makes the whole publish resumable across process
	// restarts: reusing it returns the same upload, whose Received offset
	// resumes the transfer. Empty = generated.
	OperationID string
	// ChunkSize defaults to 4 MiB.
	ChunkSize int64
	// RetryWindow bounds how long Publish retries through an outage (a
	// restarting hub or pyred: transport failures, 502/503/504, a few
	// internal errors) before giving up; 0 = DefaultPublishRetryWindow,
	// negative = no retries beyond the client's own.
	RetryWindow time.Duration
}

// publishProgressEvery spaces upload progress notices (WithNotices).
const publishProgressEvery = 10 * time.Second

// Publish uploads src as a new artifact version through any
// contracts.ArtifactAPI (HTTP client, mock, or hub): it computes size and
// sha256, begins an upload, sends chunks starting from the server's
// Received offset (resuming after interruptions), and finalizes. The hub
// verifies digest and size; a mismatch publishes nothing.
//
// Through an outage (a hub or pyred restarting) every step is retried for
// up to meta.RetryWindow, resuming the upload where it stopped; each step
// is idempotent (operation id or byte range), so a retried publish never
// creates a second version. Retries and progress are reported to the
// context's notice sink (WithNotices). Giving up returns "unavailable"
// saying so, with the operation id that resumes the upload.
//
// src is read twice (hash, then upload). A non-seekable src is first
// spooled to a temporary file.
func Publish(ctx context.Context, api c.ArtifactAPI, src io.Reader, meta PublishMeta) (c.PublishReceipt, error) {
	rs, cleanup, err := seekable(src)
	if err != nil {
		return c.PublishReceipt{}, err
	}
	defer cleanup()
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return c.PublishReceipt{}, c.Errorf(c.ErrInvalid, "publish: seek: %v", err)
	}
	h := sha256.New()
	size, err := io.Copy(h, rs)
	if err != nil {
		return c.PublishReceipt{}, c.Errorf(c.ErrInvalid, "publish: read source: %v", err)
	}
	digest := c.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
	if meta.ChunkSize <= 0 {
		meta.ChunkSize = DefaultChunkSize
	}
	opID := meta.OperationID
	if opID == "" {
		opID = c.NewID(c.PrefixOperation)
	}
	retry := &publishRetry{ctx: ctx, window: meta.RetryWindow, opID: opID}
	if retry.window == 0 {
		retry.window = DefaultPublishRetryWindow
	}
	var up c.Upload
	if err := retry.do("begin upload", func() error {
		var err error
		up, err = api.BeginUpload(ctx, c.BeginUploadRequest{
			Mutation:        c.Mutation{OperationID: opID},
			ArtifactID:      meta.ArtifactID,
			ProjectID:       meta.ProjectID,
			Title:           meta.Title,
			Description:     meta.Description,
			Filename:        meta.Filename,
			MediaType:       meta.MediaType,
			Size:            size,
			Digest:          digest,
			Note:            meta.Note,
			Provenance:      meta.Provenance,
			ReviewRequested: meta.ReviewRequested,
		})
		return err
	}); err != nil {
		return c.PublishReceipt{}, err
	}
	retry.uploadID = up.UploadID
	// A replayed BeginUpload returns the original (stale) Upload; ask for
	// the current resume offset (best effort: chunks are idempotent by
	// byte range, so a stale offset only re-sends bytes).
	_ = retry.do("read upload offset", func() error {
		cur, err := api.GetUpload(ctx, c.GetRequest{ID: up.UploadID})
		if err == nil {
			up = cur
		}
		return err
	})
	if ctx.Err() != nil {
		return c.PublishReceipt{}, ctx.Err()
	}
	if up.State == "finalized" {
		// The whole publish already completed under this operation id;
		// finalize below replays the stored receipt.
		up.Received = size
	}
	offset := up.Received
	stalls := 0
	started, lastNote := time.Now(), time.Now()
	for offset < size {
		if err := ctx.Err(); err != nil {
			return c.PublishReceipt{}, err
		}
		if now := time.Now(); now.Sub(lastNote) >= publishProgressEvery {
			lastNote = now
			notify(ctx, fmt.Sprintf("publish: uploaded %s of %s (%d%%) in %s", mib(offset), mib(size), offset*100/size, now.Sub(started).Round(time.Second)))
		}
		n := min(meta.ChunkSize, size-offset)
		var got c.Upload
		err := retry.do(fmt.Sprintf("upload at %s of %s", mib(offset), mib(size)), func() error {
			if _, err := rs.Seek(offset, io.SeekStart); err != nil {
				return c.Errorf(c.ErrInvalid, "publish: seek: %v", err)
			}
			var err error
			got, err = api.UploadChunk(ctx, up.UploadID, offset, io.LimitReader(rs, n))
			return err
		})
		if err != nil {
			if recv, ok := receivedFrom(err); ok && recv != offset && recv <= size {
				offset = recv // server has a different offset; resume there
				stalls++
				if stalls > 3 {
					return c.PublishReceipt{}, err
				}
				continue
			}
			return c.PublishReceipt{}, err
		}
		if got.Received <= offset {
			stalls++
			if stalls > 3 {
				return c.PublishReceipt{}, c.Errorf(c.ErrInternal, "publish: upload %s made no progress at offset %d", up.UploadID, offset)
			}
			continue
		}
		stalls = 0
		offset = got.Received
	}
	var rec c.PublishReceipt
	err = retry.do("finalize", func() error {
		var err error
		rec, err = api.FinalizeUpload(ctx, c.FinalizeUploadRequest{Mutation: c.Mutation{OperationID: opID + ".finalize"}, UploadID: up.UploadID})
		return err
	})
	return rec, err
}

// PublishFile publishes the file at path; Filename defaults to its base name.
func PublishFile(ctx context.Context, api c.ArtifactAPI, path string, meta PublishMeta) (c.PublishReceipt, error) {
	f, err := os.Open(path)
	if err != nil {
		return c.PublishReceipt{}, c.Errorf(c.ErrInvalid, "publish: %v", err)
	}
	defer f.Close()
	if meta.Filename == "" {
		meta.Filename = filepath.Base(path)
	}
	return Publish(ctx, api, f, meta)
}

// Publish is a convenience for client.Publish(ctx, cl, src, meta).
func (cl *Client) Publish(ctx context.Context, src io.Reader, meta PublishMeta) (c.PublishReceipt, error) {
	return Publish(ctx, cl, src, meta)
}

// PublishFile is a convenience for client.PublishFile(ctx, cl, path, meta).
func (cl *Client) PublishFile(ctx context.Context, path string, meta PublishMeta) (c.PublishReceipt, error) {
	return PublishFile(ctx, cl, path, meta)
}

func seekable(src io.Reader) (io.ReadSeeker, func(), error) {
	if rs, ok := src.(io.ReadSeeker); ok {
		return rs, func() {}, nil
	}
	f, err := os.CreateTemp("", "pyre-publish-*")
	if err != nil {
		return nil, nil, c.Errorf(c.ErrUnavailable, "publish: spool: %v", err)
	}
	cleanup := func() { f.Close(); os.Remove(f.Name()) }
	if _, err := io.Copy(f, src); err != nil {
		cleanup()
		return nil, nil, c.Errorf(c.ErrInvalid, "publish: read source: %v", err)
	}
	return f, cleanup, nil
}

// receivedFrom extracts details.received from an upload offset conflict.
func receivedFrom(err error) (int64, bool) {
	var ce *c.Error
	if !errors.As(err, &ce) || ce.Code != c.ErrConflict || ce.Details == nil {
		return 0, false
	}
	switch v := ce.Details["received"].(type) {
	case int64:
		return v, true
	case int:
		return int64(v), true
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	}
	return 0, false
}
