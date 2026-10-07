package extensions

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/jhh3/pyre/internal/contracts"
)

// Digest limits keep directory hashing bounded on large skill trees.
const (
	maxDigestFiles     = 2000
	maxDigestFileBytes = 4 << 20
)

// DigestBytes returns "sha256:<hex>" of b.
func DigestBytes(b []byte) contracts.Digest {
	s := sha256.Sum256(b)
	return contracts.Digest("sha256:" + hex.EncodeToString(s[:]))
}

// DigestStrings digests an ordered list of strings unambiguously.
func DigestStrings(parts ...string) contracts.Digest {
	h := sha256.New()
	for _, p := range parts {
		io.WriteString(h, strconv.Itoa(len(p)))
		io.WriteString(h, ":")
		io.WriteString(h, p)
		io.WriteString(h, ";")
	}
	return contracts.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

// DigestFile returns the digest of a regular file's content.
func DigestFile(path string) (contracts.Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return contracts.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}

// DigestDir digests a directory tree: sorted relative paths, modes and
// contents. The Pyre ownership marker is excluded so a managed directory's
// digest equals the digest of its source. Files larger than the per-file cap
// contribute their size only; the walk is bounded by maxDigestFiles.
func DigestDir(root string) (contracts.Digest, error) {
	type ent struct {
		rel  string
		path string
		size int64
		link string
	}
	var ents []ent
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if rel == MarkerFile {
			return nil
		}
		if len(ents) >= maxDigestFiles {
			return filepath.SkipAll
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, _ := os.Readlink(p)
			ents = append(ents, ent{rel: filepath.ToSlash(rel), link: t})
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			ents = append(ents, ent{rel: filepath.ToSlash(rel), path: p, size: info.Size()})
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].rel < ents[j].rel })
	h := sha256.New()
	for _, e := range ents {
		io.WriteString(h, e.rel)
		io.WriteString(h, "\x00")
		switch {
		case e.link != "":
			io.WriteString(h, "L"+e.link)
		case e.size > maxDigestFileBytes:
			io.WriteString(h, "S"+strconv.FormatInt(e.size, 10))
		default:
			f, err := os.Open(e.path)
			if err != nil {
				return "", err
			}
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil {
				return "", err
			}
		}
		io.WriteString(h, "\x00")
	}
	return contracts.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), nil
}

// DigestFileMap digests an in-memory directory (relative path -> content) the
// same way DigestDir digests a tree on disk, so a desired managed directory
// can be compared with what is installed.
func DigestFileMap(files map[string][]byte) contracts.Digest {
	keys := make([]string, 0, len(files))
	for k := range files {
		if k == MarkerFile {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		b := files[k]
		io.WriteString(h, filepath.ToSlash(k))
		io.WriteString(h, "\x00")
		if len(b) > maxDigestFileBytes {
			io.WriteString(h, "S"+strconv.Itoa(len(b)))
		} else {
			h.Write(b)
		}
		io.WriteString(h, "\x00")
	}
	return contracts.Digest("sha256:" + hex.EncodeToString(h.Sum(nil)))
}
