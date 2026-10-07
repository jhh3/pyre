package configuration

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

func sortStrings(s []string) { sort.Strings(s) }

func randSuffix() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeFileAtomic writes data to path via a temp file in the same directory,
// fsync and rename, so readers see either the old or the new content.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".pyre-tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

// writeTree materializes files (relative path -> content) under dir, which
// must not exist yet.
func writeTree(dir string, files map[string][]byte, mode fs.FileMode) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for rel, b := range files {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || strings.HasPrefix(clean, "..") || filepath.IsAbs(clean) {
			return fmt.Errorf("unsafe relative path %q", rel)
		}
		p := filepath.Join(dir, clean)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, b, mode); err != nil {
			return err
		}
	}
	return nil
}

// readTree loads a directory into memory (bounded).
func readTree(dir string) (map[string][]byte, error) {
	out := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		total += len(b)
		if len(out) > 2000 || total > 64<<20 {
			return fmt.Errorf("%s: directory too large to manage (limit 2000 files / 64 MiB)", dir)
		}
		out[filepath.ToSlash(rel)] = b
		return nil
	})
	return out, err
}

// backupFile copies src to dst as a private (0600, parent 0700) backup: a
// backup of a user file may contain whatever the user keeps there.
func backupFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// backupTree copies a directory tree (regular files only) as a private backup.
func backupTree(src, dst string) error {
	files, err := readTree(src)
	if err != nil {
		return err
	}
	return writeTree(dst, files, 0o600)
}

// liveDigest returns the digest of a file or directory, "" if absent.
func liveDigest(path string) (contracts.Digest, bool, error) {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if st.IsDir() {
		d, err := extensions.DigestDir(path)
		return d, true, err
	}
	d, err := extensions.DigestFile(path)
	return d, true, err
}

// lineDiff renders a compact line diff ("- old", "+ new", one line of
// context, "..." between hunks). Large inputs degrade to a summary.
func lineDiff(oldText, newText string) string {
	a := splitLines(oldText)
	b := splitLines(newText)
	if len(a)*len(b) > 4_000_000 {
		return fmt.Sprintf("~ %d lines -> %d lines (diff too large to render)", len(a), len(b))
	}
	// LCS table
	n, m := len(a), len(b)
	dp := make([][]int32, n+1)
	for i := range dp {
		dp[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	type op struct {
		k byte
		s string
	}
	var ops []op
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, op{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, op{'-', a[i]})
			i++
		default:
			ops = append(ops, op{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, op{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, op{'+', b[j]})
	}
	keep := make([]bool, len(ops))
	for k, o := range ops {
		if o.k != ' ' {
			for c := k - 1; c <= k+1; c++ {
				if c >= 0 && c < len(ops) {
					keep[c] = true
				}
			}
		}
	}
	var sb strings.Builder
	gap := false
	for k, o := range ops {
		if !keep[k] {
			gap = true
			continue
		}
		if gap && sb.Len() > 0 {
			sb.WriteString("...\n")
		}
		gap = false
		sb.WriteByte(o.k)
		sb.WriteByte(' ')
		sb.WriteString(o.s)
		sb.WriteByte('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// treeSummaryDiff describes the difference between two in-memory trees.
func treeSummaryDiff(oldFiles, newFiles map[string][]byte) string {
	var lines []string
	for _, k := range sortedFileKeys(newFiles) {
		ob, ok := oldFiles[k]
		switch {
		case !ok:
			lines = append(lines, "+ "+k)
		case string(ob) != string(newFiles[k]):
			lines = append(lines, "~ "+k)
		}
	}
	for _, k := range sortedFileKeys(oldFiles) {
		if _, ok := newFiles[k]; !ok {
			lines = append(lines, "- "+k)
		}
	}
	return strings.Join(lines, "\n")
}

func sortedFileKeys(m map[string][]byte) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
