// Package install_test exercises scripts/install in a throwaway home: fake
// binaries, --home (which disables the service manager), nothing installed
// on this machine.
package install_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func scripts(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "scripts", "install")
}

// fakeRelease writes a release directory whose binaries print their version.
func fakeRelease(t *testing.T, dir, version string) string {
	t.Helper()
	d := filepath.Join(dir, "rel-"+version)
	if err := os.MkdirAll(filepath.Join(d, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, b := range []string{"pyre", "pyred", "pyre-hub"} {
		body := "#!/bin/sh\necho '{\"version\":\"" + version + "\",\"pyred\":\"" + version + "\"}'\n"
		if err := os.WriteFile(filepath.Join(d, "bin", b), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d, "VERSION"), []byte(version+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func run(t *testing.T, wantOK bool, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{script}, args...)...)
	cmd.Env = append(os.Environ(), "PYRE_INSTALL_NO_SERVICE_MANAGER=1")
	out, err := cmd.CombinedOutput()
	if (err == nil) != wantOK {
		t.Fatalf("%s %v: err=%v (want ok=%v)\n%s", filepath.Base(script), args, err, wantOK, out)
	}
	return string(out)
}

// Installing or upgrading prunes versions other than current and .previous,
// leaving "<version> -> current" so absolute hook paths of sessions started
// under them keep working; a pruned version is no rollback target, and
// reinstalling it replaces the alias.
func TestInstallPrunesOldVersionsToCurrentAlias(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("install scripts target macOS and Linux")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	s := scripts(t)
	install, upgrade, uninstall := filepath.Join(s, "install.sh"), filepath.Join(s, "upgrade.sh"), filepath.Join(s, "uninstall.sh")
	prefix := filepath.Join(home, ".local", "share", "pyre")
	common := []string{"--home", home, "--allow-unsigned"}
	inst := func(v string, extra ...string) string {
		return run(t, true, install, append(append([]string{"--from", fakeRelease(t, dir, v)}, common...), extra...)...)
	}
	link := func(name string) string {
		l, _ := os.Readlink(filepath.Join(prefix, name))
		return l
	}
	isDir := func(name string) bool {
		fi, err := os.Lstat(filepath.Join(prefix, name))
		return err == nil && fi.IsDir()
	}
	hookOut := func(v string) string { // what an old absolute hook path runs now
		out, err := exec.Command(filepath.Join(prefix, v, "bin", "pyred")).Output()
		if err != nil {
			t.Fatalf("%s/bin/pyred: %v", v, err)
		}
		return string(out)
	}

	inst("v1")
	inst("v2")
	if !isDir("v1") || !isDir("v2") {
		t.Fatal("v1 and v2 (previous and current) must both stay")
	}
	out := inst("v3")
	if link("current") != "v3" || !isDir("v2") || link("v1") != "current" {
		t.Fatalf("after v3: current=%q v2 dir=%v v1=%q\n%s", link("current"), isDir("v2"), link("v1"), out)
	}
	if !strings.Contains(hookOut("v1"), `"v3"`) {
		t.Fatalf("v1 hook path does not reach the current binaries: %s", hookOut("v1"))
	}

	// upgrade.sh prunes after a successful switch.
	out = run(t, true, upgrade, append([]string{"--from", fakeRelease(t, dir, "v4"), "--service", "none", "--skip-hub-check"}, common...)...)
	if link("current") != "v4" || !isDir("v3") || link("v2") != "current" || link("v1") != "current" {
		t.Fatalf("after upgrade to v4: current=%q v3 dir=%v v2=%q v1=%q\n%s", link("current"), isDir("v3"), link("v2"), link("v1"), out)
	}
	if !strings.Contains(hookOut("v2"), `"v4"`) {
		t.Fatalf("v2 hook path: %s", hookOut("v2"))
	}

	// A pruned version is refused as a rollback target; .previous works.
	out = run(t, false, upgrade, "--rollback", "--to", "v1", "--service", "none", "--home", home)
	if !strings.Contains(out, "v1 was pruned") || link("current") != "v4" {
		t.Fatalf("rollback to a pruned version: current=%q\n%s", link("current"), out)
	}
	run(t, true, upgrade, "--rollback", "--service", "none", "--home", home)
	if link("current") != "v3" {
		t.Fatalf("rollback to previous: current=%q", link("current"))
	}

	// --no-prune keeps everything; reinstalling a pruned version replaces its alias.
	out = inst("v1", "--no-prune")
	if link("current") != "v1" || !isDir("v1") || !isDir("v3") || !isDir("v4") || link("v2") != "current" {
		t.Fatalf("reinstall v1: current=%q v1 dir=%v v3 dir=%v v4 dir=%v\n%s", link("current"), isDir("v1"), isDir("v3"), isDir("v4"), out)
	}

	// uninstall removes aliases too.
	run(t, true, uninstall, "--home", home)
	if _, err := os.Lstat(prefix); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(prefix)
		t.Fatalf("uninstall left %s: %v", prefix, entries)
	}
}
