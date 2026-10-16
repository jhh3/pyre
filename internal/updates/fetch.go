package updates

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/osutil"
)

// Runner runs a command with a bounded output capture.
type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (stdout, stderr string, err error)
}

// ExecRunner runs real processes.
type ExecRunner struct{}

const maxCapture = 1 << 20

type capBuf struct{ bytes.Buffer }

func (c *capBuf) Write(p []byte) (int, error) {
	if room := maxCapture - c.Len(); room > 0 {
		if len(p) > room {
			c.Buffer.Write(p[:room])
		} else {
			c.Buffer.Write(p)
		}
	}
	return len(p), nil
}

func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	cmd.WaitDelay = 2 * time.Second
	var out, errb capBuf
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// HTTPDoer is the subset of *http.Client used for downloads.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func defaultHTTP() HTTPDoer { return &http.Client{Timeout: 10 * time.Minute} }

func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func sha256File(p string) (contracts.Digest, error) {
	f, err := os.Open(p)
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

func validDigest(d contracts.Digest) bool {
	s := strings.TrimPrefix(string(d), "sha256:")
	if len(s) != 64 || !strings.HasPrefix(string(d), "sha256:") {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// sourceKind classifies an UpdateParams.Source.
func sourceKind(src string) (string, string, error) {
	switch {
	case strings.HasPrefix(src, "npm:"):
		spec := strings.TrimPrefix(src, "npm:")
		if spec == "" || strings.ContainsAny(spec, " \t\n;|&`$") || strings.HasPrefix(spec, "-") {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "invalid npm package spec %q", spec)
		}
		// The spec is an npm argument and is recorded; a URL spec must not
		// embed credentials (they would be visible in ps and in records).
		if i := strings.Index(spec, "://"); i >= 0 {
			if u, err := url.Parse(spec[strings.LastIndex(spec[:i], "+")+1:]); err != nil || u.User != nil {
				return "", "", contracts.Errorf(contracts.ErrInvalid, "npm package spec must not embed credentials; configure them in npm instead")
			}
		}
		return "npm", spec, nil
	case strings.HasPrefix(src, "https://"):
		u, err := url.Parse(src)
		if err != nil || u.Host == "" {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "invalid URL")
		}
		if u.User != nil {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "source URL must not embed credentials (they would be recorded and logged)")
		}
		return "https", src, nil
	case strings.HasPrefix(src, "http://"):
		return "", "", contracts.Errorf(contracts.ErrInvalid, "refusing plain http source %q; use https", src)
	case strings.HasPrefix(src, "file://"):
		u, err := url.Parse(src)
		if err != nil || !filepath.IsAbs(u.Path) {
			return "", "", contracts.Errorf(contracts.ErrInvalid, "invalid file URL %q", src)
		}
		return "file", u.Path, nil
	case filepath.IsAbs(src):
		return "file", filepath.Clean(src), nil
	}
	return "", "", contracts.Errorf(contracts.ErrInvalid, "source %q: want an absolute path, file://, https:// or npm:<package>@<version>", src)
}

// download fetches an https URL into dst, bounded and hashed.
func (m *Manager) download(ctx context.Context, src, dst string) (contracts.Digest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return "", err
	}
	resp, err := m.opt.HTTP.Do(req)
	if err != nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "download %s: %v", src, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", contracts.Errorf(contracts.ErrUnavailable, "download %s: HTTP %d", src, resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, m.opt.MaxDownloadBytes+1))
	if err != nil {
		return "", contracts.Errorf(contracts.ErrUnavailable, "download %s: %v", src, err)
	}
	if n > m.opt.MaxDownloadBytes {
		return "", contracts.Errorf(contracts.ErrPayloadTooLarge, "download %s exceeds %d bytes", src, m.opt.MaxDownloadBytes)
	}
	return contracts.Digest("sha256:" + hex.EncodeToString(h.Sum(nil))), f.Sync()
}

func isArchive(p string) bool {
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz") || isZip(p)
}

func isZip(p string) bool { return strings.HasSuffix(strings.ToLower(p), ".zip") }

// extractArchive extracts a .tar.gz/.tgz or (Windows release) .zip.
func extractArchive(archive, dir string, limit int64) error {
	if isZip(archive) {
		return extractZip(archive, dir, limit)
	}
	return extractTarGz(archive, dir, limit)
}

// extractZip extracts a zip into dir with the same rules as extractTarGz:
// no absolute or escaping paths, bounded total size, symlinks and special
// files skipped.
func extractZip(archive, dir string, limit int64) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("not a zip archive: %w", err)
	}
	defer zr.Close()
	var total int64
	for _, f := range zr.File {
		name := filepath.Clean(filepath.FromSlash(f.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || filepath.VolumeName(name) != "" || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes the destination", f.Name)
		}
		target := filepath.Join(dir, name)
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode.IsRegular():
			total += int64(f.UncompressedSize64)
			if f.UncompressedSize64 > uint64(limit) || total > limit {
				return fmt.Errorf("archive expands beyond %d bytes", limit)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			perm := fs.FileMode(0o644)
			if mode&0o111 != 0 || strings.EqualFold(filepath.Ext(name), ".exe") {
				perm = 0o755
			}
			rc, err := f.Open()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, perm)
			if err != nil {
				rc.Close()
				return err
			}
			_, err = io.Copy(out, io.LimitReader(rc, int64(f.UncompressedSize64)))
			rc.Close()
			out.Close()
			if err != nil {
				return err
			}
		default:
			// symlinks, devices: skipped
		}
	}
	return nil
}

// extractTarGz extracts an archive into dir, rejecting unsafe paths and
// bounding total size.
func extractTarGz(archive, dir string, limit int64) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("not a gzip archive: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("archive entry %q escapes the destination", hdr.Name)
		}
		target := filepath.Join(dir, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += hdr.Size
			if total > limit {
				return fmt.Errorf("archive expands beyond %d bytes", limit)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if hdr.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(out, io.LimitReader(tr, hdr.Size))
			out.Close()
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			ln := filepath.Clean(filepath.Join(filepath.Dir(name), hdr.Linkname))
			if filepath.IsAbs(hdr.Linkname) || ln == ".." || strings.HasPrefix(ln, ".."+string(filepath.Separator)) {
				return fmt.Errorf("archive symlink %q escapes the destination", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		default:
			// devices, fifos, hard links: skipped
		}
	}
}

// findExecutable locates name under dir (bounded depth), preferring bin/.
func findExecutable(dir, name string) (string, error) {
	var found []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() && strings.Count(rel, string(filepath.Separator)) >= 4 {
			return filepath.SkipDir
		}
		if !d.IsDir() && d.Name() == name {
			// Windows has no execute bit: the name (pyred.exe, codex.cmd)
			// is what makes a file executable there.
			if info, err := os.Stat(p); err == nil && osutil.IsExecutable(p, info) {
				found = append(found, rel)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(found) == 0 {
		return "", fmt.Errorf("no executable named %q in the package", name)
	}
	for _, f := range found {
		if filepath.Base(filepath.Dir(f)) == "bin" {
			return f, nil
		}
	}
	return found[0], nil
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			return nil // never follow symlinks out of a source tree
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				return err
			}
			mode := fs.FileMode(0o644)
			if osutil.IsExecutable(p, info) {
				mode = 0o755
			}
			return copyFile(p, target, mode)
		}
		return nil
	})
}
