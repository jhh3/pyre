package tui_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

// synthPNG is a small valid PNG; seed varies its bytes.
func synthPNG(t *testing.T, seed uint8) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	img.Set(1, 1, color.RGBA{seed, 2, 3, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func writeFile(t *testing.T, path string, data []byte) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func openComposer(t *testing.T, mod func(*tui.Options)) (*tuitest.Harness, *tuitest.Recorder, *mock.Backend) {
	t.Helper()
	h, rec, b := startMock(t, 120, 34, nil, mod)
	selectSession(t, h, "ses_synth-b")
	h.Key("m")
	if !h.Contains("Message") {
		t.Fatalf("composer did not open:\n%s", h.View())
	}
	return h, rec, b
}

// sendAndGet sends the composer and returns the one SendMessage request.
func sendAndGet(t *testing.T, h *tuitest.Harness, rec *tuitest.Recorder) contracts.SendMessageRequest {
	t.Helper()
	h.Key("ctrl+s")
	h.WaitFor("send", 5*sec, func() bool { return len(rec.Calls("SendMessage")) == 1 })
	h.Settle()
	return rec.Calls("SendMessage")[0].Request.(contracts.SendMessageRequest)
}

// attachedDigests reads each attached version back from the hub (mock) and
// returns its sha256, after checking it is a published image.
func attachedDigests(t *testing.T, b *mock.Backend, r contracts.SendMessageRequest) []string {
	t.Helper()
	var out []string
	for _, a := range r.Attachments {
		v, err := b.GetVersion(context.Background(), contracts.GetRequest{ID: a.ArtifactVersionID})
		if err != nil {
			t.Fatalf("attached version %s not in the artifact store: %v", a.ArtifactVersionID, err)
		}
		if v.MediaType != "image/png" {
			t.Errorf("version %s media type %q", v.VersionID, v.MediaType)
		}
		out = append(out, strings.TrimPrefix(string(v.Digest), "sha256:"))
	}
	return out
}

func sum(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

// A drop (a bracketed paste of shell-escaped, quoted and file:// paths)
// becomes pending images; send publishes each and attaches the versions.
func TestComposerDroppedImagePathsAttach(t *testing.T) {
	dir := t.TempDir()
	a, bb, cc := synthPNG(t, 1), synthPNG(t, 2), synthPNG(t, 3)
	pa := writeFile(t, filepath.Join(dir, "My Shots", "shot one.png"), a)
	pb := writeFile(t, filepath.Join(dir, "it's.png"), bb)
	pc := writeFile(t, filepath.Join(dir, "c d.png"), cc)
	escaped := strings.ReplaceAll(pa, " ", `\ `)                                // Terminal.app, iTerm2, Ghostty
	quoted := "'" + strings.ReplaceAll(pb, "'", `'\''`) + "'"                   // single-quoted
	fileURL := "file://" + strings.ReplaceAll(filepath.ToSlash(pc), " ", "%20") // URL form
	h, rec, b := openComposer(t, nil)
	h.Paste(escaped + " " + quoted + " " + fileURL + " ")
	v := h.View()
	for _, want := range []string{"1. shot one.png", "2. it's.png", "3. c d.png"} {
		if !strings.Contains(v, want) {
			t.Errorf("pending image %q missing:\n%s", want, v)
		}
	}
	if strings.Contains(v, "My\\ Shots") || strings.Contains(v, "file://") {
		t.Errorf("dropped paths were inserted as text:\n%s", v)
	}
	h.Type("look at these")
	r := sendAndGet(t, h, rec)
	if r.Body.Text != "look at these" {
		t.Errorf("text = %q", r.Body.Text)
	}
	got := attachedDigests(t, b, r)
	want := []string{sum(a), sum(bb), sum(cc)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("attached digests = %v, want %v", got, want)
	}
}

// Non-image files and paths that do not exist stay text, verbatim; a
// non-image path dropped with an image stays text beside it.
func TestComposerNonImagePathsStayText(t *testing.T) {
	dir := t.TempDir()
	pdf := writeFile(t, filepath.Join(dir, "notes.pdf"), []byte("%PDF-1.4 synthetic"))
	missing := filepath.Join(dir, "gone.png")
	img := writeFile(t, filepath.Join(dir, "x.png"), synthPNG(t, 9))
	h, rec, b := openComposer(t, nil)
	h.Paste(missing + " ")
	h.Paste("see " + img) // prose with a path: not a drop
	h.Paste(" " + pdf + " " + img + " ")
	r := sendAndGet(t, h, rec)
	want := missing + " see " + img + " " + pdf
	if r.Body.Text != want {
		t.Errorf("text = %q\nwant  %q", r.Body.Text, want)
	}
	if got := attachedDigests(t, b, r); len(got) != 1 {
		t.Errorf("attachments = %v, want only the dropped x.png", got)
	}
}

// Images that cannot be attached say why and stay text.
func TestComposerOversizeAndUnsupportedImagesRefused(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "huge.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(21 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	svg := writeFile(t, filepath.Join(dir, "logo.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	h, rec, _ := openComposer(t, nil)
	h.Paste(big + " ")
	if !h.Contains("images are limited to 20 MiB") {
		t.Errorf("no oversize message:\n%s", h.View())
	}
	h.Paste(svg + " ")
	if !h.Contains("only PNG, JPEG, GIF and WebP") {
		t.Errorf("no unsupported-type message:\n%s", h.View())
	}
	r := sendAndGet(t, h, rec)
	if len(r.Attachments) != 0 || !strings.Contains(r.Body.Text, "huge.png") {
		t.Errorf("request = %+v", r)
	}
}

// A drop the terminal types as keys (no bracketed paste) becomes an image
// when the trailing space arrives; ctrl+x removes it again.
func TestComposerTypedDropAndRemove(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, filepath.Join(dir, "a b.png"), synthPNG(t, 4))
	h, rec, _ := openComposer(t, nil)
	h.Type("hi " + strings.ReplaceAll(p, " ", `\ `) + " ")
	if !h.Contains("1. a b.png") {
		t.Fatalf("typed drop not attached:\n%s", h.View())
	}
	h.Key("ctrl+x")
	if h.Contains("1. a b.png") {
		t.Fatalf("ctrl+x did not remove the image:\n%s", h.View())
	}
	r := sendAndGet(t, h, rec)
	if r.Body.Text != "hi" || len(r.Attachments) != 0 {
		t.Errorf("request = %+v", r)
	}
}

// ctrl+v attaches the clipboard's image; with no image on the clipboard it
// pastes the clipboard's text, as it always did.
func TestComposerCtrlVAttachesClipboardImage(t *testing.T) {
	data := synthPNG(t, 7)
	calls := 0
	h, rec, b := openComposer(t, func(o *tui.Options) {
		o.ReadClipboard = func(context.Context) (tui.ClipboardContent, error) {
			calls++
			if calls > 1 {
				return tui.ClipboardContent{Text: "words from the clipboard"}, nil
			}
			return tui.ClipboardContent{Data: data}, nil
		}
	})
	h.Key("ctrl+v")
	h.WaitFor("clipboard image", 3*sec, func() bool { return h.Contains("1. clipboard-") })
	h.Key("ctrl+v") // no image now: the text pastes
	h.WaitFor("clipboard text", 3*sec, func() bool { return h.Contains("words from the clipboard") })
	if h.Contains("2. clipboard-") {
		t.Errorf("second ctrl+v attached again:\n%s", h.View())
	}
	r := sendAndGet(t, h, rec)
	if r.Body.Text != "words from the clipboard" {
		t.Errorf("text = %q", r.Body.Text)
	}
	if got := attachedDigests(t, b, r); len(got) != 1 || got[0] != sum(data) {
		t.Fatalf("attached = %v, want %s", got, sum(data))
	}
}

// A clipboard read that fails says so and attaches nothing.
func TestComposerCtrlVReportsClipboardError(t *testing.T) {
	h, _, _ := openComposer(t, func(o *tui.Options) {
		o.ReadClipboard = func(context.Context) (tui.ClipboardContent, error) {
			return tui.ClipboardContent{}, errors.New("osascript: synthetic failure")
		}
	})
	h.Key("ctrl+v")
	h.WaitFor("clipboard error", 3*sec, func() bool { return h.Contains("synthetic failure") })
}
