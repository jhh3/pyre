package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNvmToolDirsNewestFirst(t *testing.T) {
	home := t.TempDir()
	for _, v := range []string{"v20.1.0", "v22.23.1", "v22.9.0"} {
		d := filepath.Join(home, ".nvm", "versions", "node", v, "bin")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if v != "v20.1.0" {
			os.WriteFile(filepath.Join(d, "codex"), []byte("#!/bin/sh\n"), 0o755)
		}
	}
	got := nvmToolDirs(home)
	if len(got) != 2 || !strings.Contains(got[0], "v22.23.1") || !strings.Contains(got[1], "v22.9.0") {
		t.Fatalf("got %v", got)
	}
}

func TestWidenToolPathKeepsPrecedenceAndAddsExplicit(t *testing.T) {
	extra := t.TempDir()
	os.WriteFile(filepath.Join(extra, "pyre-synthetic-tool"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("SHELL", "")
	t.Setenv("PYRE_TOOL_PATH", "")
	t.Setenv("PATH", "/usr/bin:/bin")
	widenToolPath(context.Background(), []string{extra, "relative/ignored"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := filepath.SplitList(os.Getenv("PATH"))
	if p[0] != "/usr/bin" || p[1] != "/bin" {
		t.Fatalf("existing PATH lost precedence: %v", p)
	}
	found := false
	for _, d := range p {
		if d == extra {
			found = true
		}
		if d == "relative/ignored" {
			t.Fatal("relative dir added")
		}
	}
	if !found {
		t.Fatalf("explicit dir not added: %v", p)
	}
}
