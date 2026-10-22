package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeClip builds a clipboardReader whose tools exist when listed in have,
// and whose commands answer through run.
func fakeClip(goos string, env map[string]string, have []string, run cmdRunner) clipboardReader {
	return clipboardReader{
		goos:   goos,
		getenv: func(k string) string { return env[k] },
		lookPath: func(name string) (string, error) {
			for _, h := range have {
				if h == name {
					return "/usr/bin/" + name, nil
				}
			}
			return "", errors.New("not found")
		},
		run:      run,
		tempDir:  os.TempDir,
		readText: func() (string, error) { return "clipboard words", nil },
	}
}

var pngHead = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x02\x00\x00\x00")

func TestClipboardReaderMacPNG(t *testing.T) {
	var tmp string
	r := fakeClip("darwin", nil, []string{"osascript"}, func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name != "osascript" || !strings.Contains(strings.Join(args, "\n"), "«class PNGf»") {
			t.Fatalf("ran %s %v", name, args)
		}
		tmp = args[len(args)-1] // the script writes the PNG to argv 1
		return []byte("png\n"), os.WriteFile(tmp, pngHead, 0o600)
	})
	ci, err := r.read(context.Background())
	if err != nil || string(ci.Data) != string(pngHead) || ci.Path != "" {
		t.Fatalf("read = %+v, %v", ci, err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Errorf("temp file %s left behind", tmp)
	}
}

func TestClipboardReaderMacCopiedFileAndNone(t *testing.T) {
	out := "file:/Users/synth/Pictures/a b.png\n"
	r := fakeClip("darwin", nil, []string{"osascript"}, func(context.Context, string, ...string) ([]byte, error) { return []byte(out), nil })
	if ci, err := r.read(context.Background()); err != nil || ci.Path != "/Users/synth/Pictures/a b.png" {
		t.Fatalf("copied file: %+v, %v", ci, err)
	}
	out = "none\n"
	if _, err := r.readImage(context.Background()); !errors.Is(err, ErrNoClipboardImage) {
		t.Fatalf("text-only clipboard: err = %v", err)
	}
	// read falls back to the clipboard's text (the paste ctrl+v always did).
	if cc, err := r.read(context.Background()); err != nil || cc.Text != "clipboard words" || cc.Data != nil {
		t.Fatalf("text fallback: %+v, %v", cc, err)
	}
}

func TestClipboardReaderLinux(t *testing.T) {
	var ran []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		ran = append(ran, name+" "+strings.Join(args, " "))
		return pngHead, nil
	}
	wl := fakeClip("linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0", "DISPLAY": ":0"}, []string{"wl-paste", "xclip"}, run)
	if ci, err := wl.read(context.Background()); err != nil || len(ci.Data) == 0 {
		t.Fatalf("wayland: %+v %v", ci, err)
	}
	x := fakeClip("linux", map[string]string{"DISPLAY": ":0"}, []string{"xclip"}, run)
	if _, err := x.read(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"wl-paste --no-newline --type image/png", "xclip -selection clipboard -t image/png -o"}
	if strings.Join(ran, "|") != strings.Join(want, "|") {
		t.Errorf("ran %q, want %q", ran, want)
	}
	empty := fakeClip("linux", map[string]string{"DISPLAY": ":0"}, []string{"xclip"}, func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 1: target image/png not available")
	})
	if _, err := empty.readImage(context.Background()); !errors.Is(err, ErrNoClipboardImage) {
		t.Errorf("no image: err = %v", err)
	}
	none := fakeClip("linux", nil, nil, run)
	if cc, err := none.read(context.Background()); !errors.Is(err, ErrNoClipboardTool) || cc.Text != "clipboard words" {
		t.Errorf("no tool: %+v, err = %v (text should still paste)", cc, err)
	}
}

func TestSplitDropped(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, tc := range []struct {
		in   string
		want []string
		ok   bool
	}{
		{`/a/My\ Shot.png`, []string{"/a/My Shot.png"}, true},
		{`'/a/it'\''s.png' "/b/x \"y\".png"`, []string{"/a/it's.png", `/b/x "y".png`}, true},
		{"/a/1.png /a/2.png\n", []string{"/a/1.png", "/a/2.png"}, true},
		{"file:///a/c%20d.png", []string{"/a/c d.png"}, true},
		{"~/x.png", []string{filepath.Join(home, "x.png")}, true},
		{"look at /a/1.png", nil, false},
		{"'/a/unclosed.png", nil, false},
		{"relative.png", nil, false},
		{"file://remote-host/a.png", nil, false},
	} {
		got, ok := splitDropped(tc.in)
		if ok != tc.ok || strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("splitDropped(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
