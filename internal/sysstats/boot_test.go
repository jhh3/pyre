package sysstats

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestBootTimeLinuxAndDarwin(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu  1 2 3 4 5 6 7 8\nintr 0\nctxt 9\nbtime 1792823434\nprocesses 12\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := linuxSampler(t, root, &fakeClock{t: time.Unix(1_792_830_000, 0)})
	if got, err := s.BootTime(); err != nil || !got.Equal(time.Unix(1792823434, 0)) {
		t.Fatalf("linux boot = %v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte("cpu  1 2 3 4\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BootTime(); err == nil {
		t.Fatal("no btime line: want an error")
	}

	d := NewSampler()
	d.GOOS = "darwin"
	d.Sysctl = func(name string) ([]byte, error) {
		if name != "kern.boottime" {
			return nil, errors.New("synthetic: no such sysctl")
		}
		return le(uint64(1792823434), uint32(250000), uint32(0)), nil // timeval + padding, NUL trimmed
	}
	if got, err := d.BootTime(); err != nil || !got.Equal(time.Unix(1792823434, 250_000_000)) {
		t.Fatalf("darwin boot = %v, %v", got, err)
	}
	d.Sysctl = func(string) ([]byte, error) { return []byte{1, 2}, nil }
	if _, err := d.BootTime(); err == nil {
		t.Fatal("short kern.boottime: want an error")
	}
}

// The real machine's boot time is in the past (and not absurdly so).
func TestBootTimeReal(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("no boot time on " + runtime.GOOS)
	}
	got, err := BootTime()
	if err != nil {
		t.Fatal(err)
	}
	if now := time.Now(); !got.Before(now) || now.Sub(got) > 5*365*24*time.Hour {
		t.Fatalf("boot time %v (now %v)", got, now)
	}
}
