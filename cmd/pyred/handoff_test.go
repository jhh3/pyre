package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/updates"
)

// The launcher hands off to a managed pyred activated over it (exec with the
// launcher's path/version and the first-boot watch flag); a managed daemon
// that is no longer the active version hands back to the launcher.
func TestDaemonBootHandoff(t *testing.T) {
	t.Setenv(envLauncher, "")
	t.Setenv(envLauncherVersion, "")
	t.Setenv(envHandoffWatch, "")
	os.Unsetenv(envLauncher)
	state := t.TempDir()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if ex := daemonBoot(state, log); ex != nil {
		t.Fatalf("no managed pyred, got %+v", ex)
	}
	src := filepath.Join(t.TempDir(), "pyred")
	if err := os.WriteFile(src, []byte("#!/bin/sh\necho '{\"version\":\"9.9.9-synthetic\",\"protocol\":1,\"min_protocol\":1}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	um, err := updates.New(updates.Options{StateDir: state, DaemonBase: buildinfo.Version, ReleaseSigners: filepath.Join(state, "none")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := um.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Version: "9.9.9-synthetic", Source: src}); err != nil {
		t.Fatal(err)
	}
	if _, err := um.Activate(ctx, contracts.UpdateParams{Tool: "pyred", Version: "9.9.9-synthetic"}); err != nil {
		t.Fatal(err)
	}
	ex := daemonBoot(state, log)
	if ex == nil || !strings.HasSuffix(ex.path, filepath.Join("9.9.9-synthetic", "bin", "pyred")) {
		t.Fatalf("launcher did not hand off: %+v", ex)
	}
	env := strings.Join(ex.env, "\n")
	self, _ := os.Executable()
	for _, want := range []string{envLauncher + "=" + self, envLauncherVersion + "=" + buildinfo.Version, envHandoffWatch + "=1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("handoff env lacks %s", want)
		}
	}
	// Running as the managed daemon, but the active version moved on.
	t.Setenv(envLauncher, "/synthetic/launcher/pyred")
	t.Setenv(envLauncherVersion, buildinfo.Version)
	back := daemonBoot(state, log)
	if back == nil || back.path != "/synthetic/launcher/pyred" || strings.Contains(strings.Join(back.env, "\n"), envLauncher+"=") {
		t.Fatalf("stale managed daemon did not hand back: %+v", back)
	}
}
