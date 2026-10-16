package accounts

// Shared Claude conversation store (W-xacct).
//
// Disk layout (node-local; <state> = ~/.local/state/pyre):
//
//	<state>/shared/claude/{projects,todos,file-history,plans,tasks,
//	                        session-env,shell-snapshots}/   real directories
//	<state>/shared/claude/history.jsonl                     real file
//	<state>/profiles/<account>/<claude|claude-token>/<name> -> the above
//
// Every Claude profile (token, rota checkout, device login) links these
// names into the one shared store, so a session's transcript
// (projects/<cwd>/<uuid>.jsonl) and everything `claude --resume <uuid>`
// reads back is visible under every account's CLAUDE_CONFIG_DIR: switching
// accounts resumes the same conversation natively. Credentials, the
// keychain item, .claude.json and the live-process registry (sessions/)
// stay per profile.
//
// Migration (idempotent; at pyred start for every Claude profile, and at
// each launch for the profile used): a profile entry that is still a real
// directory/file is renamed aside to "<name>.pyre-migrating", the link
// takes its place at once, and the aside's content is merged into the
// shared store with renames (same filesystem, so open files keep their
// inode and live writers are not lost): entries the store lacks move in,
// directories present on both sides merge recursively, identical files are
// dropped, and differing files with the same name are kept as
// "<name>.from-<account>-<profile>". history.jsonl is appended. Nothing is
// deleted that was not moved or identical; anything that cannot be moved
// stays in the aside and is retried on the next pass.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/osutil"
)

const migratingSuffix = ".pyre-migrating"

// SharedStoreMigration reports one MigrateSharedStores pass.
type SharedStoreMigration struct {
	Profiles  int      `json:"profiles"`            // Claude profiles visited
	Adopted   []string `json:"adopted,omitempty"`   // "<profile>/<name>" merged into the store
	Conflicts []string `json:"conflicts,omitempty"` // left as they were (with why)
}

// MigrateSharedStores links every Claude profile under ProfileRoot to the
// shared conversation store, merging what a profile still holds itself.
func (s *Service) MigrateSharedStores(ctx context.Context) SharedStoreMigration {
	var res SharedStoreMigration
	root := s.cfg.ProfileRoot
	accts, err := os.ReadDir(root)
	if err != nil {
		return res
	}
	sharedRoot, err := ensureDir(filepath.Join(s.cfg.SharedRoot, string(contracts.HarnessClaude)))
	if err != nil {
		res.Conflicts = append(res.Conflicts, "shared store: "+err.Error())
		return res
	}
	for _, a := range accts {
		if !a.IsDir() || ctx.Err() != nil {
			continue
		}
		subs, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() || !strings.HasPrefix(sub.Name(), string(contracts.HarnessClaude)) {
				continue
			}
			dir := filepath.Join(root, a.Name(), sub.Name())
			var info ProfileInfo
			s.composeShared(&info, dir, sharedRoot, claudeShared)
			res.Profiles++
			for _, n := range info.Generated {
				res.Adopted = append(res.Adopted, a.Name()+"/"+sub.Name()+"/"+n)
			}
			for _, c := range info.Conflicts {
				res.Conflicts = append(res.Conflicts, a.Name()+"/"+sub.Name()+": "+c)
			}
		}
	}
	if len(res.Adopted) > 0 || len(res.Conflicts) > 0 {
		s.log.Info("accounts: shared Claude conversation store", "profiles", res.Profiles,
			"adopted", strings.Join(res.Adopted, ","), "conflicts", strings.Join(res.Conflicts, "; "))
	}
	return res
}

// adoptShared turns a profile's own copy of a shared entry (p, a real
// directory or file) into the link to target, merging its content into the
// store (see the file comment). A leftover aside from an interrupted pass
// is merged first. Merged entries are listed in info.Generated as
// "<name> (merged into the shared store)"; problems in info.Conflicts.
func (s *Service) adoptShared(info *ProfileInfo, p, target string, isDir bool) {
	name := filepath.Base(p)
	tag := filepath.Base(filepath.Dir(filepath.Dir(p))) + "-" + filepath.Base(filepath.Dir(p)) // <account>-<profile>

	aside := p + migratingSuffix
	if _, err := os.Lstat(aside); err == nil {
		if err := mergeAside(aside, target, isDir, tag); err != nil {
			info.Conflicts = append(info.Conflicts, name+": earlier migration not finished: "+err.Error())
		} else {
			info.Generated = append(info.Generated, name+" (merged into the shared store)")
		}
	}
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&osutil.LinkModes != 0 {
		return // missing (link() creates it) or already a link (link() judges it)
	}
	if fi.IsDir() != isDir || !fi.IsDir() && !fi.Mode().IsRegular() {
		info.Conflicts = append(info.Conflicts, name+": unexpected file type; left untouched")
		return
	}
	if _, err := os.Lstat(aside); err == nil {
		return // the aside could not be merged; never stack a second one
	}
	if err := os.Rename(p, aside); err != nil {
		info.Conflicts = append(info.Conflicts, name+": "+err.Error())
		return
	}
	if err := osutil.Symlink(target, p); err != nil {
		_ = os.Rename(aside, p) // put it back; nothing changed
		info.Conflicts = append(info.Conflicts, name+": "+err.Error())
		return
	}
	if err := mergeAside(aside, target, isDir, tag); err != nil {
		info.Conflicts = append(info.Conflicts, name+": linked; some entries not merged yet (kept in "+filepath.Base(aside)+"): "+err.Error())
		return
	}
	info.Generated = append(info.Generated, name+" (merged into the shared store)")
}

// mergeAside merges an aside directory (or appends an aside file) into the
// shared target and removes the aside once empty.
func mergeAside(aside, target string, isDir bool, tag string) error {
	if !isDir {
		if err := appendFile(aside, target); err != nil {
			return err
		}
		return os.Remove(aside)
	}
	left, err := mergeMove(aside, target, tag)
	if err != nil {
		return err
	}
	if left > 0 {
		return fmt.Errorf("%d entries could not be moved", left)
	}
	return nil
}

// mergeMove moves every entry of src into dst (both directories on one
// filesystem), recursing into directories present on both sides. A file
// already in dst stays; src's copy is dropped only when identical, else it
// moves in as "<name>.from-<tag>[-n]". src is removed when empty. left
// counts entries that could not be moved.
func mergeMove(src, dst, tag string) (left int, err error) {
	ents, err := os.ReadDir(src)
	if err != nil {
		return 0, err
	}
	for _, e := range ents {
		sp, dp := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		di, derr := os.Lstat(dp)
		if errors.Is(derr, fs.ErrNotExist) {
			if os.Rename(sp, dp) != nil {
				left++
			}
			continue
		}
		if derr != nil {
			left++
			continue
		}
		si, serr := os.Lstat(sp)
		if serr != nil {
			left++
			continue
		}
		if si.IsDir() && di.IsDir() {
			l, err := mergeMove(sp, dp, tag)
			if err != nil {
				l++
			}
			left += l
			continue
		}
		if si.Mode().IsRegular() && di.Mode().IsRegular() && sameContent(sp, dp, si.Size(), di.Size()) {
			if os.Remove(sp) != nil {
				left++
			}
			continue
		}
		if os.Rename(sp, freeName(dp+".from-"+tag)) != nil {
			left++
		}
	}
	if left == 0 {
		if err := os.Remove(src); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 1, err // a writer added an entry meanwhile: retried next pass
		}
	}
	return left, nil
}

// freeName returns p, or p-2, p-3, ... whichever does not exist.
func freeName(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
		return p
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s-%d", p, i)
		if _, err := os.Lstat(c); errors.Is(err, fs.ErrNotExist) {
			return c
		}
	}
}

// sameContent compares two regular files (up to 8 MiB; larger ones count
// as different, so both are kept).
func sameContent(a, b string, sa, sb int64) bool {
	if sa != sb || sa > 8<<20 {
		return false
	}
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && bytes.Equal(x, y)
}

// appendFile appends src's content to dst (created 0600 when missing),
// line-separated.
func appendFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return nil
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	if st, err := out.Stat(); err == nil && st.Size() > 0 {
		last := make([]byte, 1)
		if r, err := os.Open(dst); err == nil {
			if _, err := r.ReadAt(last, st.Size()-1); err == nil && last[0] != '\n' {
				_, _ = out.Write([]byte{'\n'})
			}
			r.Close()
		}
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
