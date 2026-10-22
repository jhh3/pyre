package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// artHub is a hub that serves artifact versions and records the bearer of
// each content read.
type artHub struct {
	fakeHub
	vmu      sync.Mutex
	versions map[string]contracts.ArtifactVersion
	content  map[string][]byte
	bearers  []string
}

func (h *artHub) client(bearer string) contracts.API {
	return &artClient{hubClient{h: &h.fakeHub, bearer: bearer}, h}
}

type artClient struct {
	hubClient
	a *artHub
}

func (c *artClient) GetVersion(ctx context.Context, r contracts.GetRequest) (contracts.ArtifactVersion, error) {
	if err := c.unavailable(); err != nil {
		return contracts.ArtifactVersion{}, err
	}
	c.a.vmu.Lock()
	defer c.a.vmu.Unlock()
	v, ok := c.a.versions[r.ID]
	if !ok {
		return v, contracts.Errorf(contracts.ErrNotFound, "no version %s", r.ID)
	}
	return v, nil
}

func (c *artClient) OpenContent(ctx context.Context, id string) (io.ReadCloser, contracts.ArtifactVersion, error) {
	v, err := c.GetVersion(ctx, contracts.GetRequest{ID: id})
	if err != nil {
		return nil, v, err
	}
	c.a.vmu.Lock()
	defer c.a.vmu.Unlock()
	c.a.bearers = append(c.a.bearers, c.bearer)
	return io.NopCloser(bytes.NewReader(c.a.content[id])), v, nil
}

func (h *artHub) add(id, mediaType string, data []byte, digestOf []byte) {
	sum := sha256.Sum256(digestOf)
	h.versions[id] = contracts.ArtifactVersion{VersionID: id, ArtifactID: "art_" + id, Digest: contracts.Digest("sha256:" + hex.EncodeToString(sum[:])),
		Size: int64(len(data)), MediaType: mediaType, Filename: id}
	h.content[id] = data
}

// synthPNGBytes is a PNG signature plus synthetic bytes (sniffs as image/png).
var synthPNGBytes = append([]byte("\x89PNG\r\n\x1a\n"), []byte("synthetic-pixels-0123456789")...)

func newArtHub() *artHub {
	h := &artHub{fakeHub: fakeHub{byOp: map[string]contracts.MessageView{}}, versions: map[string]contracts.ArtifactVersion{}, content: map[string][]byte{}}
	h.add("ver_png", "image/png", synthPNGBytes, synthPNGBytes)
	h.add("ver_text", "text/plain", []byte("synthetic notes"), []byte("synthetic notes"))
	h.add("ver_fakepng", "image/png", []byte("<script>synthetic</script>"), []byte("<script>synthetic</script>")) // labeled png, is not
	h.add("ver_tampered", "image/png", synthPNGBytes, []byte("other bytes"))                                      // digest mismatch
	return h
}

func deliverWith(session, msgID string, atts ...string) contracts.NodeCommand {
	env := contracts.MessageEnvelope{SchemaVersion: 1, MessageID: msgID, OperationID: "op_" + msgID, Sender: contracts.Principal{Kind: contracts.PrincipalOwner, ID: "o"},
		Recipient: contracts.Address{Kind: contracts.AddrSession, ID: session}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: "synthetic: see the screenshot"}}
	for _, a := range atts {
		env.Attachments = append(env.Attachments, contracts.Attachment{ArtifactVersionID: a})
	}
	return command(contracts.CmdDeliver, contracts.DeliverCommand{SessionID: session, Message: env})
}

func deliveredText(t *testing.T, ma *mock.Adapter, runID, msgID string) string {
	t.Helper()
	resp, _ := ma.ReadTranscript(context.Background(), contracts.RunHandle{RunID: runID}, contracts.ReadSessionRequest{})
	for _, e := range resp.Entries {
		if e.MessageID == msgID {
			return e.Text
		}
	}
	t.Fatalf("%s not delivered: %+v", msgID, resp.Entries)
	return ""
}

// An attached image is fetched from the hub as the recipient session,
// verified, written under the run's attachment dir and named by absolute
// path in the delivered text; other attachments stay references.
func TestDeliveredImageAttachmentIsMaterialized(t *testing.T) {
	dir := shortDir(t)
	hub := newArtHub()
	ma := mock.NewAdapter()
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ma}, Upstream: hub.client, OwnerToken: "pyre_owner_test_token_0000000000", OutboxFlush: time.Hour})
	sc := startCmd("ses_img", "run_img", contracts.HarnessMock, t.TempDir())
	sc.SessionToken = "pyre_s_session_token_image_000000000"
	n.start(sc)

	n.do(deliverWith("ses_img", "msg_img", "ver_png", "ver_text", "ver_fakepng", "ver_tampered"))
	n.d.engine.ProcessSession(context.Background(), "ses_img", true)
	text := deliveredText(t, ma, "run_img", "msg_img")

	want := filepath.Join(dir, "attachments", "run_img", "ver_png.png")
	if !filepath.IsAbs(want) {
		t.Fatalf("not absolute: %s", want)
	}
	lines := []string{
		"\n- image " + want + " (pyre artifact get ver_png)",
		"\n- pyre artifact get ver_text",
		"\n- pyre artifact get ver_fakepng",
		"\n- pyre artifact get ver_tampered",
	}
	for _, l := range lines {
		if !strings.Contains(text, l) {
			t.Fatalf("delivered text lacks %q:\n%s", l, text)
		}
	}
	got, err := os.ReadFile(want)
	if err != nil || !bytes.Equal(got, synthPNGBytes) {
		t.Fatalf("materialized file: %v %q", err, got)
	}
	if fi, _ := os.Stat(want); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("materialized file mode %v", fi.Mode())
	}
	ents, _ := os.ReadDir(filepath.Dir(want))
	if len(ents) != 1 {
		t.Fatalf("only the verified image is written: %v", ents)
	}
	for _, b := range hub.bearers {
		if b != sc.SessionToken {
			t.Fatalf("content read with bearer %q, want the session's", b)
		}
	}
}

// Without a reachable hub the message is still delivered, with references.
func TestImageAttachmentHubDownDeliversReference(t *testing.T) {
	dir := shortDir(t)
	hub := newArtHub()
	hub.down.Store(true)
	ma := mock.NewAdapter()
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ma}, Upstream: hub.client, OwnerToken: "pyre_owner_test_token_0000000000", OutboxFlush: time.Hour})
	sc := startCmd("ses_img2", "run_img2", contracts.HarnessMock, t.TempDir())
	sc.SessionToken = "pyre_s_session_token_image_000000002"
	n.start(sc)
	n.do(deliverWith("ses_img2", "msg_img2", "ver_png"))
	n.d.engine.ProcessSession(context.Background(), "ses_img2", true)
	if text := deliveredText(t, ma, "run_img2", "msg_img2"); !strings.HasSuffix(text, "\nAttachments:\n- pyre artifact get ver_png") {
		t.Fatalf("delivered: %q", text)
	}
}

// A second fetch of the same version reuses the verified copy.
func TestFetchImageReusesVerifiedCopy(t *testing.T) {
	hub := newArtHub()
	api := hub.client("pyre_s_synthetic")
	dir := t.TempDir()
	p1, err := fetchImage(context.Background(), api, dir, "ver_png")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := fetchImage(context.Background(), api, dir, "ver_png")
	if err != nil || p1 != p2 || len(hub.bearers) != 1 {
		t.Fatalf("second fetch: %v %s %s reads=%d", err, p1, p2, len(hub.bearers))
	}
	if _, err := fetchImage(context.Background(), api, dir, "../ver_png"); err == nil {
		t.Fatal("malformed id accepted")
	}
}
