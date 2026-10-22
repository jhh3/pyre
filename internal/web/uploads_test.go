package web

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/artifacts"
	c "github.com/jhh3/pyre/internal/contracts"
)

// synthPNG is a small valid PNG (synthetic pixels).
func synthPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{R: 200, G: 40, B: 10, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// upload POSTs raw bytes to the image route the way the app does.
func (e *tenv) upload(query string, body []byte, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest("POST", uploadImagePath+query, bytes.NewReader(body))
	r.Header.Set(HeaderEmail, testEmail)
	r.Header.Set("Content-Type", "application/octet-stream")
	r.Header.Set("Origin", testOrigin)
	for _, m := range mod {
		m(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func decodeUpload(t *testing.T, w *httptest.ResponseRecorder) Upload {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body)
	}
	var u Upload
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func TestUploadImagePublishesSniffedPNG(t *testing.T) {
	e := newTestEnv(t, false)
	data := synthPNG(t)
	// The filename lies about the type; the bytes decide.
	u := decodeUpload(t, e.upload("?filename=screenshot.txt&operation_id=synth-up1", data))
	if u.MediaType != "image/png" || u.Filename != "screenshot.png" || u.Size != int64(len(data)) || !c.ValidID(u.VersionID, c.PrefixVersion) {
		t.Fatalf("upload: %+v", u)
	}
	rc, v, err := e.b.OpenContent(ownerCtx(), u.VersionID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) || v.MediaType != "image/png" || v.Filename != "screenshot.png" {
		t.Fatalf("published version: %+v (%d bytes)", v, len(got))
	}
}

func TestUploadImageRefusesNonImagesAndOversize(t *testing.T) {
	e := newTestEnv(t, false)
	// Text bytes named .png and declared image/png in the query: refused
	// by sniffing.
	w := e.upload("?filename=evil.png", []byte("<script>alert('synthetic')</script>"))
	if w.Code != http.StatusBadRequest || errCode(t, w) != string(c.ErrInvalid) || !strings.Contains(w.Body.String(), "PNG, JPEG, GIF and WebP") {
		t.Fatalf("text as png: %d %s", w.Code, w.Body)
	}
	// SVG is an image to the browser but active content: refused.
	w = e.upload("?filename=a.svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("svg: %d %s", w.Code, w.Body)
	}
	if w = e.upload("", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("empty: %d %s", w.Code, w.Body)
	}
	// One byte over the limit, with a real PNG header.
	big := make([]byte, artifacts.MaxImageBytes+1)
	copy(big, synthPNG(t))
	w = e.upload("", big)
	if w.Code != http.StatusRequestEntityTooLarge || errCode(t, w) != string(c.ErrPayloadTooLarge) {
		t.Fatalf("oversize: %d %s", w.Code, w.Body)
	}
	// Same, without a Content-Length (chunked): the read limit catches it.
	w = e.upload("", big, func(r *http.Request) { r.ContentLength = -1 })
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize chunked: %d %s", w.Code, w.Body)
	}
	// Nothing was published by any refused upload.
	if p, err := e.b.ListArtifacts(ownerCtx(), c.ListArtifactsRequest{}); err != nil || len(p.Items) != 1 {
		t.Fatalf("artifacts after refusals: %v %d", err, len(p.Items))
	}
}

func TestUploadImageCSRF(t *testing.T) {
	e := newTestEnv(t, false)
	data := synthPNG(t)
	cases := []struct {
		name string
		mod  func(*http.Request)
		code int
	}{
		// A declared image type is not accepted (only octet-stream needs no trust in it).
		{"image content type", func(r *http.Request) { r.Header.Set("Content-Type", "image/png") }, 400},
		{"form content type", func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=x") }, 400},
		{"no origin", func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"cross-site fetch", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
	}
	for _, tc := range cases {
		if w := e.upload("", data, tc.mod); w.Code != tc.code {
			t.Errorf("%s: got %d want %d (%s)", tc.name, w.Code, tc.code, w.Body)
		}
	}
	// octet-stream is accepted on the upload route only.
	w := e.req("POST", "/api/threads/msg_synth-none/read", map[string]any{}, func(r *http.Request) { r.Header.Set("Content-Type", "application/octet-stream") })
	if w.Code != http.StatusBadRequest {
		t.Fatalf("octet-stream elsewhere: %d %s", w.Code, w.Body)
	}
}

func TestReplyWithImageAttachments(t *testing.T) {
	e := newTestEnv(t, false)
	lead := e.start("synth lead", "")
	m1 := e.mail(lead, "Synthetic: send me the screenshot.", "")
	u1 := decodeUpload(t, e.upload("?filename=one.png", synthPNG(t)))
	u2 := decodeUpload(t, e.upload("?filename=two.png", append(synthPNG(t), 0))) // a different blob

	// Images without text are a valid reply.
	var rep struct {
		Message Msg `json:"message"`
	}
	e.json("POST", "/api/threads/"+m1.Envelope.MessageID+"/reply", map[string]any{"text": "  ", "attachments": []string{u1.VersionID, u2.VersionID}}, &rep)
	if rep.Message.Attachments != 2 {
		t.Fatalf("reply view: %+v", rep.Message)
	}
	sent, err := e.b.GetMessage(ownerCtx(), c.GetRequest{ID: rep.Message.MessageID})
	want := []c.Attachment{{ArtifactVersionID: u1.VersionID}, {ArtifactVersionID: u2.VersionID}}
	if err != nil || sent.Envelope.Recipient.ID != lead || len(sent.Envelope.Attachments) != 2 ||
		sent.Envelope.Attachments[0] != want[0] || sent.Envelope.Attachments[1] != want[1] {
		t.Fatalf("sent: %v %+v", err, sent.Envelope)
	}
	tr := e.thread(m1.Envelope.MessageID)
	if n := len(tr.Messages); n != 2 || tr.Messages[1].Attachments != 2 {
		t.Fatalf("thread: %+v", tr.Messages)
	}

	// Text-only replies carry no attachments.
	rep.Message = Msg{}
	e.json("POST", "/api/threads/"+m1.Envelope.MessageID+"/reply", map[string]any{"text": "Synthetic: text only"}, &rep)
	sent, _ = e.b.GetMessage(ownerCtx(), c.GetRequest{ID: rep.Message.MessageID})
	if len(sent.Envelope.Attachments) != 0 || rep.Message.Attachments != 0 || sent.Envelope.Body.Text != "Synthetic: text only" {
		t.Fatalf("text-only: %+v", sent.Envelope)
	}

	// Malformed, unknown and too many references are refused.
	for _, tc := range []struct {
		atts []string
		code int
	}{
		{[]string{"/etc/passwd"}, 400},
		{[]string{"art_synth-report"}, 400},
		{[]string{"ver_synth-missing"}, 404},
		{strings.Split(strings.Repeat(u1.VersionID+",", maxReplyImages+1), ",")[:maxReplyImages+1], 413},
	} {
		w := e.req("POST", "/api/threads/"+m1.Envelope.MessageID+"/reply", map[string]any{"text": "x", "attachments": tc.atts})
		if w.Code != tc.code {
			t.Errorf("attachments %v: got %d want %d (%s)", tc.atts, w.Code, tc.code, w.Body)
		}
	}
}
