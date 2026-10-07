package main

import (
	"flag"
	"os"
	"strings"

	"github.com/jhh3/pyre/internal/daemon"
)

// Repo index flags (W-quicklaunch). The node discovers git repositories
// under these roots for `pyre new <repo>` and the quick-launch bar; only
// repository metadata leaves the machine, never file contents.
var repoFlags struct {
	roots    string
	disabled bool
}

// EnvRepoRoots overrides the scanned roots (comma-separated; "off"
// disables the index).
const EnvRepoRoots = "PYRE_REPO_ROOTS"

func addRepoFlags(fs *flag.FlagSet) {
	fs.StringVar(&repoFlags.roots, "repo-roots", "", "comma-separated directories scanned for git repositories (depth 4, metadata only; default ~/code,~/src,~/Documents/code,~/projects; also "+EnvRepoRoots+")")
	fs.BoolVar(&repoFlags.disabled, "no-repo-index", false, "do not index git repositories (pyre new <repo> then cannot find repositories on this node)")
}

func repoIndexConfig() daemon.RepoIndexConfig {
	roots := repoFlags.roots
	if roots == "" {
		roots = os.Getenv(EnvRepoRoots)
	}
	cfg := daemon.RepoIndexConfig{Enabled: !repoFlags.disabled && !strings.EqualFold(roots, "off")}
	for _, r := range strings.Split(roots, ",") {
		if r = strings.TrimSpace(r); r != "" && !strings.EqualFold(r, "off") {
			cfg.Roots = append(cfg.Roots, r)
		}
	}
	return cfg
}
