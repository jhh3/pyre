package updates

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

// Synthetic signing keys for tests only; never the owner's release key.

func pubBlob(pub ed25519.PublicKey) []byte {
	return append(sshString([]byte("ssh-ed25519")), sshString(pub)...)
}

func allowedLine(pub ed25519.PublicKey, opts string) string {
	if opts != "" {
		opts += " "
	}
	return fmt.Sprintf("pyre-release %sssh-ed25519 %s synthetic-test-key\n", opts, base64.StdEncoding.EncodeToString(pubBlob(pub)))
}

// signSSHSig produces the armored SSHSIG format `ssh-keygen -Y sign` writes.
func signSSHSig(priv ed25519.PrivateKey, namespace string, msg []byte) []byte {
	h := sha512.Sum512(msg)
	signed := []byte("SSHSIG")
	signed = append(signed, sshString([]byte(namespace))...)
	signed = append(signed, sshString(nil)...)
	signed = append(signed, sshString([]byte("sha512"))...)
	signed = append(signed, sshString(h[:])...)
	sig := ed25519.Sign(priv, signed)
	blob := []byte("SSHSIG")
	var v [4]byte
	binary.BigEndian.PutUint32(v[:], 1)
	blob = append(blob, v[:]...)
	blob = append(blob, sshString(pubBlob(priv.Public().(ed25519.PublicKey)))...)
	blob = append(blob, sshString([]byte(namespace))...)
	blob = append(blob, sshString(nil)...)
	blob = append(blob, sshString([]byte("sha512"))...)
	blob = append(blob, sshString(append(sshString([]byte("ssh-ed25519")), sshString(sig)...))...)
	enc := base64.StdEncoding.EncodeToString(blob)
	var b strings.Builder
	b.WriteString("-----BEGIN SSH SIGNATURE-----\n")
	for len(enc) > 70 {
		b.WriteString(enc[:70] + "\n")
		enc = enc[70:]
	}
	b.WriteString(enc + "\n-----END SSH SIGNATURE-----\n")
	return []byte(b.String())
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func mustSigners(t *testing.T, text string) []AllowedSigner {
	t.Helper()
	s, err := ParseAllowedSigners([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifySSHSigFailurePaths(t *testing.T) {
	pub, priv := newKey(t)
	otherPub, otherPriv := newKey(t)
	msg := []byte("f81c3a3e  pyre-v0.1.0-synth-darwin-arm64.tar.gz\n")
	sig := signSSHSig(priv, ReleaseNamespace, msg)
	signers := mustSigners(t, allowedLine(pub, `namespaces="pyre-release"`))

	if _, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, msg, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	cases := map[string]func() error{
		"tampered message": func() error {
			_, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, append([]byte("0"), msg...), sig)
			return err
		},
		"signed by another key": func() error {
			_, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, msg, signSSHSig(otherPriv, ReleaseNamespace, msg))
			return err
		},
		"other key allowed only": func() error {
			_, err := VerifySSHSig(mustSigners(t, allowedLine(otherPub, "")), ReleaseNamespace, ReleaseNamespace, msg, sig)
			return err
		},
		"wrong namespace in signature": func() error {
			_, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, msg, signSSHSig(priv, "file", msg))
			return err
		},
		"key not allowed for namespace": func() error {
			_, err := VerifySSHSig(mustSigners(t, allowedLine(pub, `namespaces="git"`)), ReleaseNamespace, ReleaseNamespace, msg, sig)
			return err
		},
		"wrong principal": func() error {
			_, err := VerifySSHSig(signers, "someone-else", ReleaseNamespace, msg, sig)
			return err
		},
		"garbage": func() error {
			_, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, msg, []byte("-----BEGIN SSH SIGNATURE-----\nAAAA\n-----END SSH SIGNATURE-----\n"))
			return err
		},
		"flipped signature byte": func() error {
			lines := strings.Split(string(sig), "\n") // BEGIN, base64..., END, ""
			raw, _ := base64.StdEncoding.DecodeString(strings.Join(lines[1:len(lines)-2], ""))
			raw[len(raw)-1] ^= 1
			armored := "-----BEGIN SSH SIGNATURE-----\n" + base64.StdEncoding.EncodeToString(raw) + "\n-----END SSH SIGNATURE-----\n"
			_, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, msg, []byte(armored))
			return err
		},
	}
	for name, run := range cases {
		if err := run(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseAllowedSigners(t *testing.T) {
	pub, _ := newKey(t)
	s := mustSigners(t, "# comment\n\n"+allowedLine(pub, `namespaces="file,pyre-release"`))
	if len(s) != 1 || len(s[0].Namespaces) != 2 || s[0].Namespaces[1] != "pyre-release" || !s[0].Key.Equal(pub) {
		t.Fatalf("parsed: %+v", s)
	}
	for name, text := range map[string]string{
		"empty":          "",
		"only comments":  "# nothing\n",
		"cert-authority": allowedLine(pub, "cert-authority"),
		"rsa only":       "pyre-release ssh-rsa AAAAB3NzaC1yc2E= x\n",
		"bad base64":     "pyre-release ssh-ed25519 !!! x\n",
		"truncated key":  "pyre-release ssh-ed25519 " + base64.StdEncoding.EncodeToString(sshString([]byte("ssh-ed25519"))) + "\n",
	} {
		if _, err := ParseAllowedSigners([]byte(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Interoperability with the real `ssh-keygen -Y sign` that release.sh uses.
func TestVerifySSHSigMatchesSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not installed")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "synthetic-release-key")
	if out, err := exec.Command(keygen, "-q", "-t", "ed25519", "-N", "", "-C", "synthetic", "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	sums := filepath.Join(dir, "SHA256SUMS")
	msg := []byte("0000000000000000000000000000000000000000000000000000000000000000  synthetic.tar.gz\n")
	if err := os.WriteFile(sums, msg, 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(keygen, "-Y", "sign", "-q", "-f", key, "-n", ReleaseNamespace, sums).CombinedOutput(); err != nil {
		t.Skipf("ssh-keygen -Y sign unsupported: %v %s", err, out)
	}
	pubLine, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(string(pubLine))
	signers := mustSigners(t, `pyre-release namespaces="pyre-release" `+f[0]+" "+f[1]+"\n")
	sig, err := os.ReadFile(sums + ".sig")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, msg, sig); err != nil {
		t.Fatalf("ssh-keygen signature rejected: %v", err)
	}
	if _, err := VerifySSHSig(signers, ReleaseNamespace, ReleaseNamespace, append(msg, '\n'), sig); err == nil {
		t.Fatal("tampered SHA256SUMS accepted")
	}
	// And the reverse: ssh-keygen accepts the signature format the tests build.
	_, priv := newKey(t)
	own := signSSHSig(priv, ReleaseNamespace, msg)
	allowed := filepath.Join(dir, "allowed")
	os.WriteFile(allowed, []byte(allowedLine(priv.Public().(ed25519.PublicKey), `namespaces="pyre-release"`)), 0o644)
	os.WriteFile(sums+".own.sig", own, 0o644)
	cmd := exec.Command(keygen, "-Y", "verify", "-f", allowed, "-I", ReleaseNamespace, "-n", ReleaseNamespace, "-s", sums+".own.sig")
	cmd.Stdin = bytes.NewReader(msg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen rejects the test signature format: %v %s", err, out)
	}
}

type release struct {
	dir     string
	tarball string
	sums    []byte
	digest  contracts.Digest
}

// writeRelease writes a synthetic signed release dir: tarball + SHA256SUMS (+ .sig).
func writeRelease(t *testing.T, dir, version string, priv ed25519.PrivateKey) release {
	t.Helper()
	name := "pyre-" + version + "-synth-arm64"
	archive := tarGz(t, map[string]string{
		name + "/bin/pyred": fakeScript(version, 0, 1, 1),
		name + "/VERSION":   version + "\n",
	})
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	tb := filepath.Join(dir, name+".tar.gz")
	if err := os.WriteFile(tb, archive, 0o644); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(archive)
	sums := []byte(hex.EncodeToString(h[:]) + "  " + name + ".tar.gz\n" +
		strings.Repeat("ab", 32) + "  pyre-" + version + "-synth-amd64.tar.gz\n")
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), sums, 0o644); err != nil {
		t.Fatal(err)
	}
	if priv != nil {
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS.sig"), signSSHSig(priv, ReleaseNamespace, sums), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return release{dir: dir, tarball: tb, sums: sums, digest: contracts.Digest("sha256:" + hex.EncodeToString(h[:]))}
}

func pinnedFixture(t *testing.T, pub ed25519.PublicKey, mod func(*Options)) *fixture {
	t.Helper()
	pin := filepath.Join(t.TempDir(), "release-allowed-signers")
	if err := os.WriteFile(pin, []byte(allowedLine(pub, `namespaces="pyre-release"`)), 0o644); err != nil {
		t.Fatal(err)
	}
	return newFixture(t, func(o *Options) {
		o.ReleaseSigners = pin
		if mod != nil {
			mod(o)
		}
	})
}

func assertNothingStaged(t *testing.T, f *fixture, tool string) {
	t.Helper()
	list, err := f.m.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tv := range list {
		if tv.Tool == tool && tv.State != StateFailed {
			t.Fatalf("%s was staged: %+v", tool, tv)
		}
	}
	entries, _ := os.ReadDir(f.m.toolDir(tool))
	for _, e := range entries {
		if e.Name() != "current" && !strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("payload left behind: %s", e.Name())
		}
	}
}

func TestSignedReleaseStagingRefusesTampering(t *testing.T) {
	ctx := context.Background()
	pub, priv := newKey(t)
	_, otherPriv := newKey(t)

	t.Run("valid signature stages with the signed digest", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", priv)
		tv, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball})
		if err != nil {
			t.Fatal(err)
		}
		if tv.Version != "0.2.0" || tv.Digest != rel.digest || tv.State != StateStaged {
			t.Fatalf("staged: %+v", tv)
		}
	})
	t.Run("tampered SHA256SUMS", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", priv)
		os.WriteFile(filepath.Join(rel.dir, "SHA256SUMS"), bytes.Replace(rel.sums, []byte("abab"), []byte("abac"), 1), 0o644)
		_, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball})
		if !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "signature verification failed") {
			t.Fatalf("tampered sums: %v", err)
		}
		assertNothingStaged(t, f, "pyred")
	})
	t.Run("tarball replaced after signing", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", priv)
		evil := tarGz(t, map[string]string{"pyre-0.2.0-synth-arm64/bin/pyred": fakeScript("0.2.0", 0, 1, 1) + "# evil\n"})
		os.WriteFile(rel.tarball, evil, 0o644)
		_, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball})
		if !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "checksum mismatch") {
			t.Fatalf("replaced tarball: %v", err)
		}
		assertNothingStaged(t, f, "pyred")
	})
	t.Run("signed by another key", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", otherPriv)
		_, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball})
		if !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "not an allowed signer") {
			t.Fatalf("wrong key: %v", err)
		}
		assertNothingStaged(t, f, "pyred")
	})
	t.Run("wrong key even with AllowUnsignedRelease", func(t *testing.T) {
		f := pinnedFixture(t, pub, func(o *Options) { o.AllowUnsignedRelease = true })
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", otherPriv)
		if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball}); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Fatalf("bad signature accepted under AllowUnsignedRelease: %v", err)
		}
	})
	t.Run("unsigned release", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", nil)
		_, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball, Digest: rel.digest})
		if !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "unsigned release") {
			t.Fatalf("unsigned: %v", err)
		}
		assertNothingStaged(t, f, "pyred")
		g := pinnedFixture(t, pub, func(o *Options) { o.AllowUnsignedRelease = true })
		rel2 := writeRelease(t, filepath.Join(g.root, "rel"), "0.2.0", nil)
		if _, err := g.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel2.tarball, Digest: rel2.digest}); err != nil {
			t.Fatalf("AllowUnsignedRelease: %v", err)
		}
	})
	t.Run("supplied digest disagrees with signed SHA256SUMS", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", priv)
		_, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball, Digest: contracts.Digest("sha256:" + strings.Repeat("cd", 32))})
		if !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "disagrees") {
			t.Fatalf("digest disagreement: %v", err)
		}
	})
	t.Run("tarball not listed", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", priv)
		renamed := filepath.Join(rel.dir, "pyre-0.2.0-synth-other.tar.gz")
		os.Rename(rel.tarball, renamed)
		if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: renamed}); !contracts.IsCode(err, contracts.ErrInvalid) || !strings.Contains(err.Error(), "not listed") {
			t.Fatalf("unlisted tarball: %v", err)
		}
	})
	t.Run("plain binary refused while a key is pinned", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		p, d := f.artifact("pyred", "0.2.0", 0)
		if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: p, Digest: d}); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Fatalf("unsigned binary accepted: %v", err)
		}
	})
	t.Run("unusable pinned file fails closed", func(t *testing.T) {
		f := newFixture(t, nil)
		pin := filepath.Join(f.root, "broken-allowed-signers")
		os.WriteFile(pin, []byte("pyre-release ssh-rsa AAAA x\n"), 0o644)
		f.m.opt.ReleaseSigners = pin
		rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.2.0", priv)
		if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: rel.tarball}); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Fatalf("broken pin accepted: %v", err)
		}
	})
	t.Run("other tools unaffected by the pin", func(t *testing.T) {
		f := pinnedFixture(t, pub, nil)
		p, d := f.artifact("claude", "2.1.300", 0)
		if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "claude", Source: p, Digest: d}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestSignedReleaseOverHTTPS(t *testing.T) {
	ctx := context.Background()
	pub, priv := newKey(t)
	files := map[string][]byte{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	f := pinnedFixture(t, pub, func(o *Options) { o.HTTP = srv.Client() })
	rel := writeRelease(t, filepath.Join(f.root, "rel"), "0.3.0", priv)
	load := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join(rel.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	tbName := filepath.Base(rel.tarball)
	files["/r/"+tbName] = load(tbName)
	files["/r/SHA256SUMS"] = load("SHA256SUMS")
	files["/r/SHA256SUMS.sig"] = load("SHA256SUMS.sig")
	url := srv.URL + "/r/" + tbName

	// Tampered SHA256SUMS on the server.
	files["/r/SHA256SUMS"] = append(load("SHA256SUMS"), []byte(strings.Repeat("ef", 32)+"  extra.tar.gz\n")...)
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: url}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("tampered remote sums: %v", err)
	}
	// Missing signature.
	files["/r/SHA256SUMS"] = load("SHA256SUMS")
	delete(files, "/r/SHA256SUMS.sig")
	if _, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: url}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("unsigned remote release: %v", err)
	}
	assertNothingStaged(t, f, "pyred")
	// Signed: no digest needed, the signed SHA256SUMS provides it.
	files["/r/SHA256SUMS.sig"] = load("SHA256SUMS.sig")
	tv, err := f.m.Stage(ctx, contracts.UpdateParams{Tool: "pyred", Source: url})
	if err != nil {
		t.Fatal(err)
	}
	if tv.Digest != rel.digest || tv.Version != "0.3.0" {
		t.Fatalf("staged: %+v", tv)
	}
}
