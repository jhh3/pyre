package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyWriter is the pre-age encrypted-backup writer, kept only in tests
// so old-format archives can be produced to prove they still restore.
// Production code can no longer write this format.
type legacyWriter struct {
	w      io.Writer
	aead   cipher.AEAD
	header []byte
	prefix []byte
	buf    []byte
	n      uint32
}

const legacyChunk = 64 << 10

var testLegacyKDF = legacyKDF{N: 1 << 10, R: 8, P: 1}

func newLegacyWriter(w io.Writer, pass []byte, k legacyKDF) (*legacyWriter, error) {
	h := legacyHeader{KDF: "scrypt", legacyKDF: k, Salt: make([]byte, 16), NoncePrefix: make([]byte, 7), Chunk: legacyChunk}
	rand.Read(h.Salt)
	rand.Read(h.NoncePrefix)
	aead, err := legacyAEAD(pass, h.Salt, k)
	if err != nil {
		return nil, err
	}
	hb, _ := json.Marshal(h)
	header := append(append([]byte(legacyMagic), hb...), '\n')
	if _, err := w.Write(header); err != nil {
		return nil, err
	}
	return &legacyWriter{w: w, aead: aead, header: header, prefix: h.NoncePrefix, buf: make([]byte, 0, legacyChunk)}, nil
}

func (e *legacyWriter) flush(final bool) error {
	ct := e.aead.Seal(nil, legacyNonce(e.prefix, e.n, final), e.buf, e.header)
	e.n++
	e.buf = e.buf[:0]
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
	if _, err := e.w.Write(l[:]); err != nil {
		return err
	}
	_, err := e.w.Write(ct)
	return err
}

func (e *legacyWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if len(e.buf) == legacyChunk {
			if err := e.flush(false); err != nil {
				return total - len(p), err
			}
		}
		k := min(legacyChunk-len(e.buf), len(p))
		e.buf = append(e.buf, p[:k]...)
		p = p[k:]
	}
	return total, nil
}

func (e *legacyWriter) Close() error { return e.flush(true) }

func legacySeal(t *testing.T, pass string, plain []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := newLegacyWriter(&buf, []byte(pass), testLegacyKDF)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(plain)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func legacyOpen(pass string, ct []byte) ([]byte, error) {
	r, err := newLegacyReader(bytes.NewReader(ct), []byte(pass))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestLegacyReaderRoundTripAndTamper(t *testing.T) {
	plain := make([]byte, 3*legacyChunk+123) // several chunks + a partial one
	rand.Read(plain)
	ct := legacySeal(t, "synthetic passphrase", plain)
	if !isLegacy(ct) {
		t.Fatal("ciphertext lacks the legacy header")
	}
	got, err := legacyOpen("synthetic passphrase", ct)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %v (len %d)", err, len(got))
	}
	if _, err := legacyOpen("wrong passphrase", ct); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("wrong passphrase: %v", err)
	}
	bad := append([]byte{}, ct...)
	bad[len(bad)/2] ^= 1
	if _, err := legacyOpen("synthetic passphrase", bad); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	hdrLen := len(ct) - (4*(legacyChunk+16+4) - (legacyChunk - 123))
	if _, err := legacyOpen("synthetic passphrase", ct[:hdrLen+2*(legacyChunk+16+4)]); err == nil {
		t.Fatal("truncated ciphertext accepted")
	}
}

// An archive written by the pre-age pyre-hub (legacy format) still
// restores, detected by its header, with the passphrase.
func TestRestoreLegacyArchive(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	data, marker := makeHub(t, base, 300<<10)
	// Produce the plaintext tar in memory (test only) and seal it with the
	// legacy writer, as the old `pyre-hub backup --passphrase-file` did.
	var tarBuf bytes.Buffer
	m, err := CreateTo(ctx, data, &tarBuf, Options{Unencrypted: true})
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(base, "old.tar")
	var enc bytes.Buffer
	bw := bufio.NewWriter(&enc)
	lw, err := newLegacyWriter(bw, []byte("synthetic legacy passphrase"), testLegacyKDF)
	if err != nil {
		t.Fatal(err)
	}
	lw.Write(tarBuf.Bytes())
	lw.Close()
	bw.Flush()
	if bytes.Contains(enc.Bytes(), marker) {
		t.Fatal("legacy ciphertext contains plaintext")
	}
	os.WriteFile(archive, enc.Bytes(), 0o600)

	if _, err := Restore(ctx, archive, filepath.Join(base, "r0"), Options{}); !errors.Is(err, ErrPassphraseRequired) {
		t.Fatalf("legacy without passphrase: %v", err)
	}
	if _, err := Restore(ctx, archive, filepath.Join(base, "r1"), Options{Passphrase: []byte("nope")}); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("legacy wrong passphrase: %v", err)
	}
	assertNoDir(t, filepath.Join(base, "r1"))
	got, err := Restore(ctx, archive, filepath.Join(base, "restored"), Options{Passphrase: []byte("synthetic legacy passphrase")})
	if err != nil {
		t.Fatal(err)
	}
	if got.HubID != m.HubID || len(got.Entries) != len(m.Entries) {
		t.Fatalf("manifest %+v", got)
	}
	assertSameTree(t, data, filepath.Join(base, "restored"))
	// Truncated legacy archive: rejected, nothing restored.
	os.WriteFile(archive+".trunc", enc.Bytes()[:enc.Len()-200], 0o600)
	if _, err := Restore(ctx, archive+".trunc", filepath.Join(base, "rt"), Options{Passphrase: []byte("synthetic legacy passphrase")}); err == nil {
		t.Fatal("truncated legacy archive restored")
	}
	assertNoDir(t, filepath.Join(base, "rt"))
}
