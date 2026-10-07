package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// actionFsMkdir creates a directory (with parents) for a launch in a new
// plain folder. Only paths inside the node user's home are accepted, checked
// lexically and again after resolving symlinks of the existing ancestor.
func (d *Daemon) actionFsMkdir(_ context.Context, raw json.RawMessage) (any, error) {
	var p contracts.FsMkdirParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, contracts.Errorf(contracts.ErrInvalid, "fs.mkdir params: %v", err)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, contracts.Errorf(contracts.ErrUnavailable, "no home directory: %v", err)
	}
	return mkdirUnderHome(home, p.Path)
}

func mkdirUnderHome(home, p string) (contracts.FsMkdirResult, error) {
	var res contracts.FsMkdirResult
	if strings.TrimSpace(p) == "" {
		return res, contracts.Errorf(contracts.ErrInvalid, "fs.mkdir: path is required")
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	if !filepath.IsAbs(p) {
		return res, contracts.Errorf(contracts.ErrInvalid, "fs.mkdir: %q must be absolute or start with ~/", p)
	}
	p = filepath.Clean(p)
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		realHome = filepath.Clean(home)
	}
	if !within(filepath.Clean(home), p) && !within(realHome, p) {
		return res, contracts.Errorf(contracts.ErrForbidden, "fs.mkdir: %s is outside the home directory", p)
	}
	// The deepest existing ancestor must also resolve inside home (a symlink
	// under home must not lead the new folder elsewhere).
	anc := p
	for {
		if _, err := os.Lstat(anc); err == nil {
			break
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			break
		}
		anc = parent
	}
	if r, err := filepath.EvalSymlinks(anc); err == nil && !within(realHome, r) && !within(filepath.Clean(home), r) {
		return res, contracts.Errorf(contracts.ErrForbidden, "fs.mkdir: %s resolves outside the home directory", anc)
	}
	if st, err := os.Stat(p); err == nil {
		if !st.IsDir() {
			return res, contracts.Errorf(contracts.ErrInvalid, "fs.mkdir: %s exists and is not a directory", p)
		}
		return contracts.FsMkdirResult{Path: p}, nil
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return res, contracts.Errorf(contracts.ErrInvalid, "fs.mkdir: %v", err)
	}
	return contracts.FsMkdirResult{Path: p, Created: true}, nil
}

// within reports whether p is root or inside it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
