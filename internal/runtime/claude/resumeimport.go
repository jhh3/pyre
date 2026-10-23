package claude

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Native resume across config homes. `claude --resume <id>` only finds a
// conversation under its own CLAUDE_CONFIG_DIR (projects/<cwd>/<id>.jsonl).
// Pyre account profiles share one transcript store, but a run on the node's
// default login (~/.claude) writes outside it, so resuming that session on
// any account profile used to exit at once (server reboot, 2026-10-11). Before
// a resume launch the adapter copies the transcript (and its <id>/ sidecar:
// sub-agents, tool results) from wherever it is on this node into the new
// run's config home. The old run is over, so the copy is the conversation
// from here on; the original is left untouched.

// transcriptHomes are the Claude config homes searched for a session's
// transcript: the default login and the configured homes (globs allowed).
func (a *Adapter) transcriptHomes() []string {
	homes := []string{defaultConfigDir()}
	for _, h := range a.o.TranscriptHomes {
		if m, err := filepath.Glob(h); err == nil && len(m) > 0 {
			homes = append(homes, m...)
		} else {
			homes = append(homes, h)
		}
	}
	return homes
}

// findTranscript returns an existing transcript of session sid under one of
// homes: the cwd's project dir first, then any project dir (a cwd reached
// through a different path). "" when none.
func findTranscript(homes []string, cwd, sid string) string {
	for _, h := range homes {
		if p := guessTranscriptPath(h, cwd, sid); p != "" && isFile(p) {
			return p
		}
	}
	for _, h := range homes {
		if h == "" {
			continue
		}
		if m, _ := filepath.Glob(filepath.Join(h, "projects", "*", sid+".jsonl")); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// NativeThreadOnNode reports whether prev's conversation is on this node,
// so a resume on another account can still continue it natively (the
// launch imports it into that account's config home).
func (a *Adapter) NativeThreadOnNode(prev contracts.Run) bool {
	if prev.NativeRef == nil || prev.NativeRef.Kind != "claude_session" || !uuidRe.MatchString(prev.NativeRef.ID) {
		return false
	}
	return findTranscript(a.transcriptHomes(), prev.Cwd, prev.NativeRef.ID) != ""
}

// importTranscript makes session sid resumable under configDir. It does
// nothing when the transcript is already there (or nowhere on this node).
func (a *Adapter) importTranscript(configDir, cwd, sid string) error {
	dst := guessTranscriptPath(configDir, cwd, sid)
	if dst == "" {
		return nil
	}
	if isFile(dst) {
		// A copy made by an earlier adoption may be stale: the owner went
		// on with the conversation outside Pyre. A newer transcript that
		// extends this copy replaces it (never one that diverged).
		if src := newerExtension(a.transcriptHomes(), cwd, sid, dst); src != "" {
			if err := os.Remove(dst); err != nil {
				return err
			}
			return copyFile(src, dst)
		}
		return nil
	}
	src := findTranscript(a.transcriptHomes(), cwd, sid)
	if src == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	side := filepath.Join(filepath.Dir(src), sid)
	if fi, err := os.Stat(side); err == nil && fi.IsDir() {
		return copyTree(side, filepath.Join(filepath.Dir(dst), sid))
	}
	return nil
}

// allTranscripts returns every distinct transcript file of sid under homes.
func allTranscripts(homes []string, cwd, sid string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || !isFile(p) {
			return
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			real = p
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, p)
		}
	}
	for _, h := range homes {
		if h == "" {
			continue
		}
		add(guessTranscriptPath(h, cwd, sid))
		m, _ := filepath.Glob(filepath.Join(h, "projects", "*", sid+".jsonl"))
		for _, p := range m {
			add(p)
		}
	}
	return out
}

// newerExtension returns a transcript of sid elsewhere on the node that is
// newer and longer than dst and begins with dst's bytes ("" when none).
func newerExtension(homes []string, cwd, sid, dst string) string {
	di, err := os.Stat(dst)
	if err != nil {
		return ""
	}
	dreal, _ := filepath.EvalSymlinks(dst)
	best := ""
	var bestT int64
	for _, p := range allTranscripts(homes, cwd, sid) {
		if real, _ := filepath.EvalSymlinks(p); real == dreal {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil || fi.Size() <= di.Size() || !fi.ModTime().After(di.ModTime()) {
			continue
		}
		if fi.ModTime().UnixNano() > bestT && hasPrefixFile(p, dst, di.Size()) {
			best, bestT = p, fi.ModTime().UnixNano()
		}
	}
	return best
}

// hasPrefixFile reports whether file p begins with the n bytes of prefix.
func hasPrefixFile(p, prefix string, n int64) bool {
	a, err := os.Open(p)
	if err != nil {
		return false
	}
	defer a.Close()
	b, err := os.Open(prefix)
	if err != nil {
		return false
	}
	defer b.Close()
	ba, bb := make([]byte, 64<<10), make([]byte, 64<<10)
	for n > 0 {
		k := int64(len(ba))
		if n < k {
			k = n
		}
		if _, err := io.ReadFull(a, ba[:k]); err != nil {
			return false
		}
		if _, err := io.ReadFull(b, bb[:k]); err != nil {
			return false
		}
		if !bytes.Equal(ba[:k], bb[:k]) {
			return false
		}
		n -= k
	}
	return true
}

// ConversationInUse reports whether conversation sid looks live outside
// Pyre: a transcript written in the last 30 seconds, or (Linux /proc;
// macOS lsof) a process holding one open. Adopting it then would give the
// conversation two writers.
func (a *Adapter) ConversationInUse(cwd, sid string) (bool, string) {
	for _, p := range allTranscripts(a.transcriptHomes(), cwd, sid) {
		if fi, err := os.Stat(p); err == nil && time.Since(fi.ModTime()) < 30*time.Second {
			return true, p + " was written in the last 30 seconds"
		}
		if pid := fileHolder(p); pid > 0 {
			return true, fmt.Sprintf("process %d has %s open", pid, p)
		}
	}
	return false, ""
}

func isFile(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// copyFile copies src to dst (0600) through a temp file and rename; an
// existing dst is kept.
func copyFile(src, dst string) error {
	if isFile(dst) {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
	}
	return err
}

// copyTree copies regular files and directories under src into dst,
// keeping files dst already has; symlinks and special files are skipped.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		t := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(t, 0o700)
		case d.Type().IsRegular():
			return copyFile(p, t)
		}
		return nil
	})
}
