package web

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/client"
	c "github.com/jhh3/pyre/internal/contracts"
)

// uploadImagePath is the one POST that takes raw bytes instead of JSON
// (checkCSRF accepts application/octet-stream here only).
const uploadImagePath = "/api/uploads/image"

// maxReplyImages bounds the images on one reply (the composer's limit).
const maxReplyImages = 10

// Upload is a published image the composer attaches to a reply.
type Upload struct {
	VersionID string `json:"version_id"`
	MediaType string `json:"media_type"`
	Size      int64  `json:"size"`
	Filename  string `json:"filename"`
}

// handleUploadImage publishes one pasted or dropped image as a standalone
// artifact and returns its version, which the reply then attaches. The body
// is the raw image (its own limit, artifacts.MaxImageBytes, not maxBody);
// the type comes from the bytes, never the request's headers or filename.
func (s *Server) handleUploadImage(w http.ResponseWriter, r *http.Request) {
	if r.ContentLength > artifacts.MaxImageBytes {
		writeErr(w, tooLargeImage())
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, artifacts.MaxImageBytes+1))
	if err != nil {
		writeErr(w, c.Errorf(c.ErrInvalid, "The image did not arrive whole: %v", err))
		return
	}
	if len(data) > artifacts.MaxImageBytes {
		writeErr(w, tooLargeImage())
		return
	}
	if len(data) == 0 {
		writeErr(w, c.Errorf(c.ErrInvalid, "The image is empty."))
		return
	}
	mt, ok := artifacts.SniffImage(data)
	if !ok {
		writeErr(w, c.Errorf(c.ErrInvalid, "Only PNG, JPEG, GIF and WebP images can be attached (this file is %s).", strings.SplitN(mt, ";", 2)[0]))
		return
	}
	q := r.URL.Query()
	name := artifacts.ImageFilename(q.Get("filename"), mt)
	ctx, cancel := hubCtx(r)
	defer cancel()
	rec, err := client.Publish(ctx, s.api, bytes.NewReader(data), client.PublishMeta{
		OperationID: opID(q.Get("operation_id")),
		Title:       name,
		Description: "Image attached to a reply in the web inbox",
		Filename:    name,
		MediaType:   mt,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Upload{VersionID: rec.Version.VersionID, MediaType: mt, Size: int64(len(data)), Filename: name})
}

func tooLargeImage() error {
	return c.Errorf(c.ErrPayloadTooLarge, "The image is larger than %d MB.", artifacts.MaxImageBytes>>20)
}
