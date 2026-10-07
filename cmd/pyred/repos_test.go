package main

import (
	"strings"
	"testing"
)

func TestRepoIndexConfigFlagsAndEnv(t *testing.T) {
	defer func() { repoFlags.roots, repoFlags.disabled = "", false }()
	t.Setenv(EnvRepoRoots, "")
	if c := repoIndexConfig(); !c.Enabled || len(c.Roots) != 0 {
		t.Fatalf("default: %+v (enabled, default roots)", c)
	}
	t.Setenv(EnvRepoRoots, " ~/work , /srv/git ")
	if c := repoIndexConfig(); !c.Enabled || strings.Join(c.Roots, "|") != "~/work|/srv/git" {
		t.Fatalf("env roots: %+v", c)
	}
	repoFlags.roots = "~/code"
	if c := repoIndexConfig(); strings.Join(c.Roots, "|") != "~/code" {
		t.Fatalf("flag wins over env: %+v", c)
	}
	repoFlags.roots = "off"
	if c := repoIndexConfig(); c.Enabled {
		t.Fatalf("off: %+v", c)
	}
	repoFlags.roots, repoFlags.disabled = "", true
	if c := repoIndexConfig(); c.Enabled {
		t.Fatalf("--no-repo-index: %+v", c)
	}
}
