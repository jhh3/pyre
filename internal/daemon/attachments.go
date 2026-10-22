package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/messaging/delivery"
)

// attachmentFiles is the delivery engine's AttachmentFiles: it copies the
// images a message attaches (artifact versions) from the hub into
// <StateDir>/attachments/<run_id>/<version_id><ext>, as the recipient
// session, so the rendered message can name a local path the harness opens
// (Claude Code and Codex both read images by path). Only PNG, JPEG, GIF and
// WebP up to artifacts.MaxImageBytes are copied, and only when the bytes
// match the hub's digest and sniff as that image type; anything else stays
// a "pyre artifact get" reference.
type attachmentFiles struct{ d *Daemon }

var _ delivery.AttachmentFiles = attachmentFiles{}

func (f attachmentFiles) Fetch(ctx context.Context, t delivery.Target, e contracts.MessageEnvelope) map[string]string {
	d := f.d
	runID, sessionID := t.Run.RunID, t.Run.SessionID
	if d.hubUnreachable() || !contracts.ValidID(runID, contracts.PrefixRun) || sessionID == "" {
		return nil
	}
	tok, ok, err := d.st.tokenForSession(ctx, sessionID)
	if err != nil || !ok {
		return nil
	}
	up, err := d.upstream(tok)
	if err != nil {
		return nil
	}
	pctx := contracts.WithPrincipal(ctx, contracts.Principal{Kind: contracts.PrincipalSession, ID: sessionID})
	dir := filepath.Join(d.cfg.StateDir, "attachments", runID)
	out := map[string]string{}
	for _, a := range e.Attachments {
		id := a.ArtifactVersionID
		if id == "" || out[id] != "" {
			continue
		}
		p, err := fetchImage(pctx, up, dir, id)
		if err != nil {
			d.cfg.Logger.Warn("attachment not fetched; rendered as a reference", "message", e.MessageID, "version", id, "err", err)
			continue
		}
		out[id] = p
	}
	return out
}

// fetchImage materializes one image version under dir and returns its
// absolute path. An existing copy with the right digest is reused (a retried
// attempt renders the same text).
func fetchImage(ctx context.Context, api contracts.ArtifactAPI, dir, versionID string) (string, error) {
	if !contracts.ValidID(versionID, contracts.PrefixVersion) {
		return "", contracts.Errorf(contracts.ErrInvalid, "malformed version id")
	}
	v, err := api.GetVersion(ctx, contracts.GetRequest{ID: versionID})
	if err != nil {
		return "", err
	}
	ext := artifacts.ImageExt(v.MediaType)
	if ext == "" {
		return "", contracts.Errorf(contracts.ErrUnsupported, "%s is not an attachable image", v.MediaType)
	}
	if v.Size <= 0 || v.Size > artifacts.MaxImageBytes {
		return "", contracts.Errorf(contracts.ErrPayloadTooLarge, "image is %d bytes; limit %d", v.Size, artifacts.MaxImageBytes)
	}
	abs, err := filepath.Abs(filepath.Join(dir, versionID+ext))
	if err != nil {
		return "", err
	}
	if sum, err := fileDigest(abs); err == nil && sum == v.Digest {
		return abs, nil
	}
	rc, cv, err := api.OpenContent(client.WithMaxAttempts(ctx, 1), versionID)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, artifacts.MaxImageBytes+1))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if got := contracts.Digest("sha256:" + hex.EncodeToString(sum[:])); got != v.Digest || got != cv.Digest {
		return "", contracts.Errorf(contracts.ErrConflict, "content digest %s does not match the published %s", got, v.Digest)
	}
	if mt, ok := artifacts.SniffImage(data); !ok || mt != v.MediaType {
		return "", contracts.Errorf(contracts.ErrUnsupported, "content sniffs as %s, published as %s", mt, v.MediaType)
	}
	if err := fileperm.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".fetch-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), abs); err != nil {
		return "", err
	}
	return abs, nil
}

func fileDigest(p string) (contracts.Digest, error) {
	f, err := fileperm.OpenNoFollow(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, artifacts.MaxImageBytes+1)); err != nil {
		return "", err
	}
	return contracts.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}
