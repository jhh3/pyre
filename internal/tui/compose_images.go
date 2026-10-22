package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/atotto/clipboard"

	"github.com/jhh3/pyre/internal/artifacts"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
)

// Images in the composer, the terminal's version of the web inbox's paste
// and drop (fdea983). A terminal never sends image bytes: dropping a file
// types its path (shell-escaped or quoted, several separated by spaces),
// and pasting a screenshot sends nothing at all. So:
//
//   - pasted or typed text that is only paths of existing PNG/JPEG/GIF/WebP
//     files becomes pending images instead of text (other paths stay text);
//   - ctrl+v reads an image (or a copied image file) from the OS clipboard
//     (macOS osascript, Linux wl-paste or xclip), else pastes its text as
//     the textarea always did.
//
// Pending images are read into memory when they are added (a temp file may
// go away) and published to the hub's artifact store on send, so the
// session's node, wherever it is, fetches them like a web reply's images.

// maxComposeImages bounds the images on one message (the web composer's limit).
const maxComposeImages = 10

// imageSendTimeout bounds a send that uploads images (LoadTimeout is sized
// for small API calls, not 20 MB uploads).
const imageSendTimeout = 2 * time.Minute

// maxDropScan is the longest paste examined for dropped paths; anything
// longer is prose and goes in as typed.
const maxDropScan = 16 << 10

// pendingImage is an image waiting in the composer.
type pendingImage struct {
	name      string // filename sent to the hub (artifacts.ImageFilename)
	mediaType string
	data      []byte
	sum       string // hex sha256 of data
}

func newPendingImage(name string, data []byte) (pendingImage, error) {
	if len(data) == 0 {
		return pendingImage{}, fmt.Errorf("%s is empty", name)
	}
	if len(data) > artifacts.MaxImageBytes {
		return pendingImage{}, fmt.Errorf("%s is %s; images are limited to %d MiB", name, humanBytes(int64(len(data))), artifacts.MaxImageBytes>>20)
	}
	mt, ok := artifacts.SniffImage(data)
	if !ok {
		return pendingImage{}, fmt.Errorf("%s is %s; only PNG, JPEG, GIF and WebP images can be attached", name, strings.SplitN(mt, ";", 2)[0])
	}
	sum := sha256.Sum256(data)
	return pendingImage{name: artifacts.ImageFilename(name, mt), mediaType: mt, data: data, sum: hex.EncodeToString(sum[:])}, nil
}

// errNotImage marks a file that is not an image at all (a dropped PDF): it
// stays text without a message. An image that cannot be attached (too large,
// HEIC, SVG) is reported.
var errNotImage = errors.New("not an image")

// loadImageFile reads the image at path. Files that are not images return
// errNotImage; images that cannot be attached return why.
func loadImageFile(path string) (pendingImage, error) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return pendingImage{}, errNotImage
	}
	name := filepath.Base(path)
	if st.Size() > artifacts.MaxImageBytes {
		if !looksLikeImageName(name) {
			return pendingImage{}, errNotImage
		}
		return pendingImage{}, fmt.Errorf("%s is %s; images are limited to %d MiB", name, humanBytes(st.Size()), artifacts.MaxImageBytes>>20)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pendingImage{}, fmt.Errorf("%s: %v", name, err)
	}
	img, err := newPendingImage(name, data)
	if mt, _ := artifacts.SniffImage(data); err != nil && !looksLikeImageName(name) && !strings.HasPrefix(mt, "image/") {
		return pendingImage{}, errNotImage
	}
	return img, err
}

// looksLikeImageName reports an image extension, including the ones that
// cannot be attached, so dropping one explains why it stayed text.
func looksLikeImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".heic", ".heif", ".tif", ".tiff", ".bmp", ".svg", ".avif", ".ico":
		return true
	}
	return false
}

// splitDropped splits text the way a shell would read dropped paths:
// whitespace separates words, a backslash escapes the next character, and
// '…' or "…" quote (in "…" a backslash escapes only \ " $ `). It reports
// false for anything else: an unbalanced quote, or a word that is not an
// absolute path, ~/path or file:// URL (that is prose, not a drop).
func splitDropped(text string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	in, quote := false, rune(0)
	rs := []rune(text)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch {
			case r == '"':
				quote = 0
			case r == '\\' && i+1 < len(rs) && strings.ContainsRune(`\"$`+"`", rs[i+1]):
				i++
				cur.WriteRune(rs[i])
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			if i+1 < len(rs) {
				i++
				cur.WriteRune(rs[i])
			}
			in = true
		case r == '\'' || r == '"':
			quote, in = r, true
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if quote != 0 {
		return nil, false
	}
	if in {
		words = append(words, cur.String())
	}
	if len(words) == 0 {
		return nil, false
	}
	for i, w := range words {
		p, ok := droppedPath(w)
		if !ok {
			return nil, false
		}
		words[i] = p
	}
	return words, true
}

// droppedPath turns one dropped word into a local path: /abs, ~/rel or a
// file:// URL (percent-decoded).
func droppedPath(w string) (string, bool) {
	switch {
	case strings.HasPrefix(w, "file://"):
		u, err := url.Parse(w)
		if err != nil || (u.Host != "" && u.Host != "localhost") || u.Path == "" {
			return "", false
		}
		return u.Path, true
	case strings.HasPrefix(w, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return filepath.Join(home, w[2:]), true
	case filepath.IsAbs(w):
		return w, true
	}
	return "", false
}

// dropResult is what a paste of dropped paths became.
type dropResult struct {
	images []pendingImage
	text   string   // what still goes in as text ("" when every word was an image)
	notes  []string // images that could not be attached, and why
}

// takeDropped turns pasted text that is only file paths into images: each
// path to an attachable image becomes one; other paths stay as text, in
// their original spelling (re-quoted). Text that is not only paths (or
// holds no image) is returned whole.
func takeDropped(text string) dropResult {
	if len(text) > maxDropScan {
		return dropResult{text: text}
	}
	paths, ok := splitDropped(text)
	if !ok {
		return dropResult{text: text}
	}
	var res dropResult
	var rest []string
	for _, p := range paths {
		img, err := loadImageFile(p)
		switch {
		case err == nil:
			res.images = append(res.images, img)
			continue
		case !errors.Is(err, errNotImage):
			res.notes = append(res.notes, err.Error())
		}
		rest = append(rest, shellQuote(p))
	}
	if len(res.images) == 0 {
		return dropResult{text: text, notes: res.notes}
	}
	if len(rest) > 0 {
		lead := text[:len(text)-len(strings.TrimLeft(text, " \t\r\n"))]
		res.text = lead + strings.Join(rest, " ") + " "
	}
	return res
}

// shellQuote spells p the way the composer received it, quoted when needed.
func shellQuote(p string) string {
	if p != "" && !strings.ContainsAny(p, " \t\n'\"\\$`!*?[]{}()<>|&;#~") {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}

// typedDrop finds a dropped path at the end of v that a terminal typed as
// keys (no bracketed paste): it is followed by the space terminals add
// after a dropped path. It returns the offset where the path starts and
// what it became, or false.
func typedDrop(v string) (int, dropResult, bool) {
	if !strings.HasSuffix(v, " ") || strings.HasSuffix(v, "\\ ") {
		return 0, dropResult{}, false
	}
	body := v[:len(v)-1]
	// Earliest start first: "/a\ b.png" must not match from "b.png".
	for i := 0; i < len(body); i++ {
		if i > 0 && !strings.ContainsRune(" \t\n", rune(body[i-1])) {
			continue
		}
		if !strings.ContainsRune(`/~'"f`, rune(body[i])) {
			continue
		}
		cand := body[i:]
		if len(cand) > maxDropScan {
			continue
		}
		ws, ok := splitDropped(cand)
		if !ok || len(ws) != 1 {
			continue
		}
		res := takeDropped(cand)
		if len(res.images) == 0 && len(res.notes) == 0 {
			continue
		}
		return i, res, true
	}
	return 0, dropResult{}, false
}

// ---- clipboard ----

// ClipboardContent is what ctrl+v in the composer read from the system
// clipboard: image bytes (a screenshot, an image copied from a browser), the
// path of a copied file (Finder), or else its text, which is pasted.
type ClipboardContent struct {
	Data []byte
	Path string
	Text string
}

// ReadClipboardFunc reads the system clipboard for ctrl+v. With
// ErrNoClipboardTool (no way to read an image here) Text may still hold the
// clipboard's text.
type ReadClipboardFunc func(ctx context.Context) (ClipboardContent, error)

var (
	// ErrNoClipboardImage: the clipboard holds no image (internal: the
	// reader then returns the clipboard's text).
	ErrNoClipboardImage = errors.New("no image on the clipboard")
	// ErrNoClipboardTool: this system cannot read a clipboard image.
	ErrNoClipboardTool = errors.New("reading a clipboard image needs osascript (macOS), wl-paste (Wayland) or xclip (X11)")
)

// cmdRunner runs a command and returns its stdout (tests fake it).
type cmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil && errb.Len() > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), err
}

// clipboardReader reads clipboard images with the system's own tools: no
// new dependency, nothing installed.
type clipboardReader struct {
	goos     string
	getenv   func(string) string
	lookPath func(string) (string, error)
	run      cmdRunner
	tempDir  func() string
	readText func() (string, error) // the text paste ctrl+v always did
}

// DefaultReadClipboard reads this machine's clipboard. Over ssh that is the
// remote machine's clipboard, not the one the terminal runs on.
func DefaultReadClipboard(ctx context.Context) (ClipboardContent, error) {
	return clipboardReader{goos: runtime.GOOS, getenv: os.Getenv, lookPath: exec.LookPath, run: execRunner,
		tempDir: os.TempDir, readText: clipboard.ReadAll}.read(ctx)
}

// macClipboardScript prints "file:<path>" for a copied file, else writes
// the clipboard's PNG to argv 1 and prints "png", else prints "none". It
// asks clipboard info first: AppleScript coerces plain text to «class furl»
// (a relative "file"), so a coercion that succeeds proves nothing.
// «class PNGf» is the PNG flavor screenshots and browsers put there.
var macClipboardScript = []string{
	"on run argv",
	"set hasFile to false",
	"set hasPNG to false",
	"repeat with f in (clipboard info)",
	"if item 1 of f is «class furl» then set hasFile to true",
	"if item 1 of f is «class PNGf» then set hasPNG to true",
	"end repeat",
	"if hasFile then",
	`return "file:" & POSIX path of (the clipboard as «class furl»)`,
	"end if",
	"if not hasPNG then",
	`return "none"`,
	"end if",
	"set d to the clipboard as «class PNGf»",
	"set fh to open for access (POSIX file (item 1 of argv)) with write permission",
	"set eof fh to 0",
	"write d to fh",
	"close access fh",
	`return "png"`,
	"end run",
}

func (r clipboardReader) read(ctx context.Context) (ClipboardContent, error) {
	ci, err := r.readImage(ctx)
	if errors.Is(err, ErrNoClipboardImage) || errors.Is(err, ErrNoClipboardTool) {
		text, terr := r.readText()
		if terr != nil && errors.Is(err, ErrNoClipboardImage) {
			return ClipboardContent{}, fmt.Errorf("no image on the clipboard, and its text could not be read: %w", terr)
		}
		if errors.Is(err, ErrNoClipboardImage) {
			err = nil
		}
		return ClipboardContent{Text: text}, err
	}
	return ci, err
}

func (r clipboardReader) readImage(ctx context.Context) (ClipboardContent, error) {
	has := func(name string) bool { _, err := r.lookPath(name); return err == nil }
	switch {
	case r.goos == "darwin" && has("osascript"):
		return r.readMac(ctx)
	case r.goos == "darwin":
		return ClipboardContent{}, ErrNoClipboardTool
	case r.getenv("WAYLAND_DISPLAY") != "" && has("wl-paste"):
		return r.readCmd(ctx, "wl-paste", "--no-newline", "--type", "image/png")
	case r.getenv("DISPLAY") != "" && has("xclip"):
		return r.readCmd(ctx, "xclip", "-selection", "clipboard", "-t", "image/png", "-o")
	}
	return ClipboardContent{}, ErrNoClipboardTool
}

func (r clipboardReader) readMac(ctx context.Context) (ClipboardContent, error) {
	f, err := os.CreateTemp(r.tempDir(), "pyre-clipboard-*.png")
	if err != nil {
		return ClipboardContent{}, err
	}
	tmp := f.Name()
	f.Close()
	defer os.Remove(tmp)
	args := make([]string, 0, 2*len(macClipboardScript)+1)
	for _, l := range macClipboardScript {
		args = append(args, "-e", l)
	}
	out, err := r.run(ctx, "osascript", append(args, tmp)...)
	if err != nil {
		return ClipboardContent{}, fmt.Errorf("osascript: %w", err)
	}
	res := strings.TrimSpace(string(out))
	switch {
	case strings.HasPrefix(res, "file:"):
		return ClipboardContent{Path: strings.TrimPrefix(res, "file:")}, nil
	case res == "png":
		data, err := readCapped(tmp)
		if err != nil {
			return ClipboardContent{}, err
		}
		if len(data) == 0 {
			return ClipboardContent{}, ErrNoClipboardImage
		}
		return ClipboardContent{Data: data}, nil
	}
	return ClipboardContent{}, ErrNoClipboardImage
}

// readCmd runs a tool that prints the clipboard's image/png (exit status
// nonzero, or nothing, when there is none).
func (r clipboardReader) readCmd(ctx context.Context, name string, args ...string) (ClipboardContent, error) {
	out, err := r.run(ctx, name, args...)
	if err != nil || len(out) == 0 {
		return ClipboardContent{}, ErrNoClipboardImage
	}
	return ClipboardContent{Data: out}, nil
}

// readCapped reads a file of at most MaxImageBytes+1 bytes (enough to tell
// that it is too large).
func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, artifacts.MaxImageBytes+1))
}

// clipboardPendingImage turns what the clipboard held into a pending image.
func clipboardPendingImage(ci ClipboardContent) (pendingImage, error) {
	if ci.Path != "" {
		img, err := loadImageFile(ci.Path)
		if errors.Is(err, errNotImage) {
			return pendingImage{}, fmt.Errorf("the copied file %s is not an image", filepath.Base(ci.Path))
		}
		return img, err
	}
	name := "clipboard-" + time.Now().Format("150405") + ".png"
	return newPendingImage(name, ci.Data)
}

// ---- upload ----

// uploadErr is an image that did not reach the hub: nothing was sent yet.
type uploadErr struct {
	name string
	err  error
}

func (e uploadErr) Error() string { return e.name + " did not upload: " + errText(e.err) }
func (e uploadErr) Unwrap() error { return e.err }

// publishImages publishes each image as a standalone artifact (like the web
// inbox's POST /api/uploads/image) and returns the attachments. opIDFor
// gives each image a stable operation ID, so a retry resumes or replays the
// same upload rather than publishing a copy.
func publishImages(ctx context.Context, api contracts.ArtifactAPI, imgs []pendingImage, opIDFor func(pendingImage) string) ([]contracts.Attachment, error) {
	var atts []contracts.Attachment
	for _, img := range imgs {
		rec, err := client.Publish(ctx, api, bytes.NewReader(img.data), client.PublishMeta{
			OperationID: opIDFor(img),
			Title:       img.name,
			Description: "Image attached to a message in the terminal UI",
			Filename:    img.name,
			MediaType:   img.mediaType,
		})
		if err != nil {
			return nil, uploadErr{img.name, err}
		}
		atts = append(atts, contracts.Attachment{ArtifactVersionID: rec.Version.VersionID})
	}
	return atts, nil
}
