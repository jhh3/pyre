package updates

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Release signatures: packaging/release.sh signs the release SHA256SUMS with
// `ssh-keygen -Y sign -n pyre-release` (an offline Ed25519 key) and writes
// SHA256SUMS.sig next to it. Nodes pin the public key in an OpenSSH
// allowed-signers file (principal "pyre-release"), the same file
// scripts/install/upgrade.sh uses.

// ReleaseNamespace is the ssh-keygen -Y namespace and principal of Pyre
// release signatures.
const ReleaseNamespace = "pyre-release"

// DefaultReleaseSigners returns the pinned allowed-signers path:
// $PYRE_RELEASE_ALLOWED_SIGNERS, else ~/.config/pyre/release-allowed-signers.
func DefaultReleaseSigners() string {
	if p := os.Getenv("PYRE_RELEASE_ALLOWED_SIGNERS"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "pyre", "release-allowed-signers")
}

// AllowedSigner is one usable line of an OpenSSH allowed-signers file.
type AllowedSigner struct {
	Principals []string // patterns, e.g. "pyre-release"
	Namespaces []string // empty = any namespace
	Key        ed25519.PublicKey
}

// ParseAllowedSigners parses the subset of the allowed-signers format Pyre
// uses: "<principals> [options] ssh-ed25519 <base64> [comment]". Entries with
// other key types or with cert-authority/validity options are skipped; a file
// without any usable entry is an error (fail closed).
func ParseAllowedSigners(data []byte) ([]AllowedSigner, error) {
	var out []AllowedSigner
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 3 {
			return nil, fmt.Errorf("allowed signers line %d: too few fields", n)
		}
		ki := -1
		for i := 1; i < len(f)-1; i++ {
			if strings.HasPrefix(f[i], "ssh-") || strings.HasPrefix(f[i], "ecdsa-") || strings.HasPrefix(f[i], "sk-") {
				ki = i
				break
			}
		}
		if ki < 0 {
			return nil, fmt.Errorf("allowed signers line %d: no public key", n)
		}
		if f[ki] != "ssh-ed25519" {
			continue
		}
		s := AllowedSigner{Principals: strings.Split(f[0], ",")}
		skip := false
		for _, opt := range splitOptions(strings.Join(f[1:ki], " ")) {
			switch {
			case opt == "":
			case strings.HasPrefix(strings.ToLower(opt), "namespaces="):
				v := strings.Trim(opt[len("namespaces="):], `"`)
				s.Namespaces = append(s.Namespaces, strings.Split(v, ",")...)
			default:
				skip = true // cert-authority, valid-after, valid-before: unsupported here
			}
		}
		if skip {
			continue
		}
		blob, err := base64.StdEncoding.DecodeString(f[ki+1])
		if err != nil {
			return nil, fmt.Errorf("allowed signers line %d: bad base64 key", n)
		}
		key, err := parseEd25519Blob(blob)
		if err != nil {
			return nil, fmt.Errorf("allowed signers line %d: %v", n, err)
		}
		s.Key = key
		out = append(out, s)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New("allowed signers file has no usable ssh-ed25519 entry")
	}
	return out, nil
}

// splitOptions splits an allowed-signers option list on commas outside quotes.
func splitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	quoted := false
	for _, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
			cur.WriteRune(c)
		case c == ',' && !quoted:
			out = append(out, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}
	return append(out, strings.TrimSpace(cur.String()))
}

// sshReader reads SSH wire-format fields.
type sshReader struct{ b []byte }

func (r *sshReader) uint32() (uint32, error) {
	if len(r.b) < 4 {
		return 0, errors.New("truncated")
	}
	v := binary.BigEndian.Uint32(r.b)
	r.b = r.b[4:]
	return v, nil
}

func (r *sshReader) str() ([]byte, error) {
	n, err := r.uint32()
	if err != nil {
		return nil, err
	}
	if uint64(n) > uint64(len(r.b)) {
		return nil, errors.New("truncated")
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v, nil
}

func sshString(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out, uint32(len(b)))
	copy(out[4:], b)
	return out
}

func parseEd25519Blob(blob []byte) (ed25519.PublicKey, error) {
	r := &sshReader{b: blob}
	t, err := r.str()
	if err != nil || string(t) != "ssh-ed25519" {
		return nil, errors.New("not an ssh-ed25519 key")
	}
	k, err := r.str()
	if err != nil || len(k) != ed25519.PublicKeySize || len(r.b) != 0 {
		return nil, errors.New("malformed ssh-ed25519 key")
	}
	return ed25519.PublicKey(append([]byte(nil), k...)), nil
}

// VerifySSHSig verifies an armored `ssh-keygen -Y sign` signature (SSHSIG
// format, Ed25519 keys) of message under namespace, signed by a key that
// signers allow for principal and namespace. It returns the signer key.
func VerifySSHSig(signers []AllowedSigner, principal, namespace string, message, armored []byte) (ed25519.PublicKey, error) {
	const begin, end = "-----BEGIN SSH SIGNATURE-----", "-----END SSH SIGNATURE-----"
	s := string(armored)
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		return nil, errors.New("not an SSH signature (missing armor)")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s[i+len(begin):j]), ""))
	if err != nil {
		return nil, errors.New("SSH signature: bad base64")
	}
	if len(raw) < 6 || string(raw[:6]) != "SSHSIG" {
		return nil, errors.New("SSH signature: bad magic")
	}
	r := &sshReader{b: raw[6:]}
	ver, err := r.uint32()
	if err != nil || ver != 1 {
		return nil, errors.New("SSH signature: unsupported version")
	}
	pubBlob, err1 := r.str()
	ns, err2 := r.str()
	reserved, err3 := r.str()
	hashAlg, err4 := r.str()
	sigBlob, err5 := r.str()
	if err := errors.Join(err1, err2, err3, err4, err5); err != nil || len(r.b) != 0 {
		return nil, errors.New("SSH signature: malformed")
	}
	if string(ns) != namespace {
		return nil, fmt.Errorf("SSH signature: namespace %q, want %q", ns, namespace)
	}
	pub, err := parseEd25519Blob(pubBlob)
	if err != nil {
		return nil, fmt.Errorf("SSH signature: %v", err)
	}
	if !allowed(signers, principal, namespace, pub) {
		return nil, errors.New("SSH signature: signed by a key that is not an allowed signer")
	}
	var digest []byte
	switch string(hashAlg) {
	case "sha512":
		h := sha512.Sum512(message)
		digest = h[:]
	case "sha256":
		h := sha256.Sum256(message)
		digest = h[:]
	default:
		return nil, fmt.Errorf("SSH signature: unsupported hash %q", hashAlg)
	}
	sr := &sshReader{b: sigBlob}
	st, err1 := sr.str()
	sig, err2 := sr.str()
	if err1 != nil || err2 != nil || string(st) != "ssh-ed25519" || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("SSH signature: malformed ed25519 signature")
	}
	signed := []byte("SSHSIG")
	signed = append(signed, sshString(ns)...)
	signed = append(signed, sshString(reserved)...)
	signed = append(signed, sshString(hashAlg)...)
	signed = append(signed, sshString(digest)...)
	if !ed25519.Verify(pub, signed, sig) {
		return nil, errors.New("SSH signature: invalid (content modified or wrong key)")
	}
	return pub, nil
}

func allowed(signers []AllowedSigner, principal, namespace string, key ed25519.PublicKey) bool {
	for _, s := range signers {
		if !s.Key.Equal(key) {
			continue
		}
		pOK := false
		for _, p := range s.Principals {
			if ok, _ := path.Match(p, principal); ok {
				pOK = true
			}
		}
		nOK := len(s.Namespaces) == 0
		for _, n := range s.Namespaces {
			if ok, _ := path.Match(n, namespace); ok {
				nOK = true
			}
		}
		if pOK && nOK {
			return true
		}
	}
	return false
}

// SumsDigest returns the digest SHA256SUMS lists for file name.
func SumsDigest(sums []byte, name string) (contracts.Digest, bool) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 2 {
			continue
		}
		if strings.TrimPrefix(f[1], "*") == name {
			if _, err := hex.DecodeString(f[0]); err == nil && len(f[0]) == 64 {
				return contracts.Digest("sha256:" + strings.ToLower(f[0])), true
			}
		}
	}
	return "", false
}

func isReleaseTool(tool string) bool { return tool == "pyred" || tool == "pyre" }

// releaseSigners loads the pinned allowed-signers file. ok=false means no key
// is pinned; an unreadable or unusable pinned file is an error (fail closed).
func (m *Manager) releaseSigners() ([]AllowedSigner, bool, error) {
	p := m.opt.ReleaseSigners
	if p == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, contracts.Errorf(contracts.ErrInvalid, "read pinned release key %s: %v", p, err)
	}
	s, err := ParseAllowedSigners(data)
	if err != nil {
		return nil, true, contracts.Errorf(contracts.ErrInvalid, "pinned release key %s: %v", p, err)
	}
	return s, true, nil
}

// releaseDigest enforces the release signature for pyred/pyre sources while a
// release key is pinned: SHA256SUMS and SHA256SUMS.sig beside the artifact
// must verify, and the digest SHA256SUMS lists for the artifact becomes the
// required digest. It returns "" when no key is pinned (legacy behaviour).
func (m *Manager) releaseDigest(ctx context.Context, p contracts.UpdateParams, kind, loc, staging string) (contracts.Digest, error) {
	if !isReleaseTool(p.Tool) {
		return "", nil
	}
	signers, pinned, err := m.releaseSigners()
	if err != nil {
		return "", err
	}
	if !pinned {
		return "", nil
	}
	refuse := func(format string, a ...any) error {
		if m.opt.AllowUnsignedRelease {
			return nil
		}
		return contracts.Errorf(contracts.ErrInvalid, format+" (release key pinned in %s); candidate refused", append(a, m.opt.ReleaseSigners)...)
	}
	var sums, sig []byte
	var name string
	switch kind {
	case "file":
		info, err := os.Stat(loc)
		if err != nil {
			return "", contracts.Errorf(contracts.ErrInvalid, "source: %v", err)
		}
		if info.IsDir() {
			return "", refuse("%s is a directory; pyred/pyre must come from a signed release tarball", loc)
		}
		name = filepath.Base(loc)
		dir := filepath.Dir(loc)
		sums, err = os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
		if err != nil {
			return "", refuse("no SHA256SUMS next to %s", loc)
		}
		sig, err = os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
		if err != nil {
			return "", refuse("unsigned release: no SHA256SUMS.sig next to %s", loc)
		}
	case "https":
		u, err := url.Parse(loc)
		if err != nil {
			return "", contracts.Errorf(contracts.ErrInvalid, "invalid URL %q", loc)
		}
		name = path.Base(u.Path)
		fetch := func(file string) ([]byte, error) {
			v := *u
			v.Path = path.Join(path.Dir(u.Path), file)
			v.RawQuery = ""
			dst := filepath.Join(staging, "rel-"+file)
			if _, err := m.download(ctx, v.String(), dst); err != nil {
				return nil, err
			}
			return os.ReadFile(dst)
		}
		if sums, err = fetch("SHA256SUMS"); err != nil {
			return "", refuse("cannot fetch SHA256SUMS beside %s: %v", loc, err)
		}
		if sig, err = fetch("SHA256SUMS.sig"); err != nil {
			return "", refuse("unsigned release: cannot fetch SHA256SUMS.sig beside %s: %v", loc, err)
		}
	default:
		return "", refuse("%s source %q cannot carry a release signature", kind, p.Source)
	}
	if len(sums) > 1<<20 || len(sig) > 1<<16 {
		return "", contracts.Errorf(contracts.ErrInvalid, "SHA256SUMS or its signature is implausibly large; candidate refused")
	}
	// A bad signature is refused even with AllowUnsignedRelease.
	if _, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, sums, sig); err != nil {
		return "", contracts.Errorf(contracts.ErrInvalid, "release signature verification failed for %s: %v; candidate refused", p.Source, err)
	}
	want, ok := SumsDigest(sums, name)
	if !ok {
		return "", contracts.Errorf(contracts.ErrInvalid, "%s is not listed in the signed SHA256SUMS; candidate refused", name)
	}
	if p.Digest != "" && p.Digest != want {
		return "", contracts.Errorf(contracts.ErrInvalid, "digest %s disagrees with the signed SHA256SUMS (%s); candidate refused", p.Digest, want)
	}
	return want, nil
}
