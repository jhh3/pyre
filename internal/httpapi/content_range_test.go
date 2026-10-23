package httpapi

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// rangerAPI is a node proxy: it can fetch a byte range upstream itself.
// A Range request must use it, not stream the whole version and discard
// the prefix (that re-downloads everything already received on resume).
type rangerAPI struct {
	*fakeAPI
	calls       [][2]int64
	fullOpens   int
	unsupported bool
}

func (r *rangerAPI) GetVersion(ctx context.Context, req contracts.GetRequest) (contracts.ArtifactVersion, error) {
	b, ok := r.content[req.ID]
	if !ok {
		return contracts.ArtifactVersion{}, contracts.Errorf(contracts.ErrNotFound, "no version")
	}
	return contracts.ArtifactVersion{VersionID: req.ID, Size: int64(len(b)), MediaType: "application/gzip", Filename: "kit.tgz", Digest: "sha256:abc", CreatedAt: time.Unix(0, 0)}, nil
}

func (r *rangerAPI) OpenContent(ctx context.Context, id string) (io.ReadCloser, contracts.ArtifactVersion, error) {
	r.fullOpens++
	return r.fakeAPI.OpenContent(ctx, id)
}

func (r *rangerAPI) OpenContentRange(ctx context.Context, id string, offset, length int64) (io.ReadCloser, error) {
	if r.unsupported {
		return nil, contracts.Errorf(contracts.ErrUnsupported, "no ranges upstream")
	}
	r.calls = append(r.calls, [2]int64{offset, length})
	b := r.content[id]
	return io.NopCloser(bytes.NewReader(b[offset : offset+length])), nil
}

func TestContentRangeForwardedThroughRanger(t *testing.T) {
	api := &rangerAPI{fakeAPI: &fakeAPI{content: map[string][]byte{"ver_a": []byte("0123456789")}}}
	srv, _ := newTestServer(t, api, Options{})
	u := srv.URL + "/v1/content/ver_a"

	r := do(t, "GET", u, "secret-token-123", map[string]string{"Range": "bytes=6-"}, "")
	if r.status != 206 || string(r.body) != "6789" || r.header.Get("Content-Range") != "bytes 6-9/10" {
		t.Fatalf("forwarded range: %d %q %q", r.status, r.body, r.header.Get("Content-Range"))
	}
	if len(api.calls) != 1 || api.calls[0] != [2]int64{6, 4} || api.fullOpens != 0 {
		t.Fatalf("range must be forwarded upstream, not read whole: calls=%v full=%d", api.calls, api.fullOpens)
	}
	if r.header.Get("X-Content-Type-Options") != "nosniff" || r.header.Get("ETag") != `"sha256:abc"` {
		t.Fatalf("content headers missing on a forwarded range: %v", r.header)
	}

	// No Range, an unsatisfiable range, or an upstream without ranges:
	// the normal path answers.
	if r := do(t, "GET", u, "secret-token-123", nil, ""); r.status != 200 || string(r.body) != "0123456789" {
		t.Fatalf("whole body: %d %q", r.status, r.body)
	}
	if r := do(t, "GET", u, "secret-token-123", map[string]string{"Range": "bytes=20-"}, ""); r.status != 416 {
		t.Fatalf("unsatisfiable: %d", r.status)
	}
	api.unsupported = true
	if r := do(t, "GET", u, "secret-token-123", map[string]string{"Range": "bytes=6-"}, ""); r.status != 206 || string(r.body) != "6789" {
		t.Fatalf("fallback range: %d %q", r.status, r.body)
	}
}
