package notifications

import (
	"context"
	"errors"
	"slices"
	"testing"
)

func withDesktop(t *testing.T, goos string, found bool) *[][]string {
	t.Helper()
	oldOS, oldLook, oldRun := desktopGOOS, desktopLookPath, desktopRun
	t.Cleanup(func() { desktopGOOS, desktopLookPath, desktopRun = oldOS, oldLook, oldRun })
	var calls [][]string
	desktopGOOS = goos
	desktopLookPath = func(name string) (string, error) {
		if !found {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}
	desktopRun = func(_ context.Context, path string, args ...string) error {
		calls = append(calls, append([]string{path}, args...))
		return nil
	}
	return &calls
}

func TestDesktopMacPassesTextAsArgv(t *testing.T) {
	calls := withDesktop(t, "darwin", true)
	title := `Pyre: "approval" needed`
	body := "line1\nline2\" & do shell script \"rm -rf ~\""
	if err := Desktop(title, body); err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c[0] != "/usr/bin/osascript" {
		t.Fatalf("binary = %s", c[0])
	}
	// The AppleScript program is fixed; user text appears only as argv.
	if !slices.Equal(c[1:7], osascriptProgram) {
		t.Fatalf("program = %v", c[1:7])
	}
	if c[7] != title || c[8] != `line1 line2" & do shell script "rm -rf ~"` {
		t.Fatalf("argv = %q", c[7:])
	}
	for _, a := range c[1:7] {
		if a == title || a == body {
			t.Fatal("user text interpolated into the program")
		}
	}
}

func TestDesktopLinux(t *testing.T) {
	calls := withDesktop(t, "linux", true)
	if err := Desktop("-rf", "--help"); err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	want := []string{"/usr/bin/notify-send", "--app-name=Pyre", "--", "‐rf", "‐-help"}
	if !slices.Equal(c, want) {
		t.Fatalf("args = %q want %q", c, want)
	}
}

func TestDesktopMissingNotifier(t *testing.T) {
	calls := withDesktop(t, "linux", false)
	if err := Desktop("t", "b"); !errors.Is(err, ErrNoDesktopNotifier) {
		t.Fatalf("err = %v", err)
	}
	if len(*calls) != 0 {
		t.Fatal("ran a command without a notifier")
	}
}
