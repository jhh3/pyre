package backup

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/scrypt"
)

// Legacy encrypted archive format (pyre-hub-backup-enc/1), READ ONLY.
//
// Backups written before the move to age used this format. New backups are
// always age files (crypt.go); this reader keeps old archives restorable.
//
//	"PYRE-HUB-BACKUP-ENC/1\n"
//	header JSON + "\n"  {"kdf":"scrypt","n":..,"r":..,"p":..,"salt":..,"nonce_prefix":..,"chunk":65536}
//	chunks: uint32 big-endian ciphertext length, AES-256-GCM ciphertext
//
// The key is scrypt(passphrase, salt). Each chunk seals at most "chunk"
// plaintext bytes with nonce = nonce_prefix(7) || counter(4) || final(1)
// and the exact header bytes as additional data, so reordering, header
// edits and truncation (a missing final chunk) all fail authentication.
const legacyMagic = "PYRE-HUB-BACKUP-ENC/1\n"

// legacyKDF are scrypt cost parameters of a legacy archive.
type legacyKDF struct {
	N int `json:"n"`
	R int `json:"r"`
	P int `json:"p"`
}

type legacyHeader struct {
	KDF string `json:"kdf"`
	legacyKDF
	Salt        []byte `json:"salt"`
	NoncePrefix []byte `json:"nonce_prefix"`
	Chunk       int    `json:"chunk"`
}

// isLegacy reports whether b starts with the legacy encrypted-backup magic.
func isLegacy(b []byte) bool { return bytes.HasPrefix(b, []byte(legacyMagic)) }

func legacyAEAD(pass, salt []byte, k legacyKDF) (cipher.AEAD, error) {
	if len(pass) == 0 {
		return nil, errors.New("backup: empty passphrase")
	}
	if k.N < 2 || k.N > 1<<20 || k.R < 1 || k.R > 32 || k.P < 1 || k.P > 16 {
		return nil, fmt.Errorf("backup: unsupported scrypt parameters %+v", k)
	}
	key, err := scrypt.Key(pass, salt, k.N, k.R, k.P, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func legacyNonce(prefix []byte, n uint32, final bool) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix[:7])
	binary.BigEndian.PutUint32(nonce[7:11], n)
	if final {
		nonce[11] = 1
	}
	return nonce
}

type legacyReader struct {
	r      *bufio.Reader
	aead   cipher.AEAD
	header []byte
	prefix []byte
	chunk  int
	n      uint32
	out    []byte
	done   bool
}

// newLegacyReader parses the header and returns a reader of the
// authenticated plaintext. Any authentication failure, truncation or
// trailing data is an error.
func newLegacyReader(r io.Reader, pass []byte) (*legacyReader, error) {
	br := bufio.NewReader(r)
	magic := make([]byte, len(legacyMagic))
	if _, err := io.ReadFull(br, magic); err != nil || string(magic) != legacyMagic {
		return nil, errors.New("backup: not a legacy encrypted backup")
	}
	line, err := br.ReadSlice('\n')
	if err != nil || len(line) > 4096 {
		return nil, errors.New("backup: malformed encrypted-backup header")
	}
	var h legacyHeader
	if err := json.Unmarshal(line, &h); err != nil || h.KDF != "scrypt" || len(h.Salt) < 16 || len(h.NoncePrefix) != 7 || h.Chunk <= 0 || h.Chunk > 16<<20 {
		return nil, errors.New("backup: malformed encrypted-backup header")
	}
	aead, err := legacyAEAD(pass, h.Salt, h.legacyKDF)
	if err != nil {
		return nil, err
	}
	header := append([]byte(legacyMagic), line...)
	return &legacyReader{r: br, aead: aead, header: header, prefix: h.NoncePrefix, chunk: h.Chunk}, nil
}

var errLegacyDecrypt = errors.New("backup: decryption failed: wrong passphrase or corrupted archive")

func (d *legacyReader) next() error {
	var l [4]byte
	if _, err := io.ReadFull(d.r, l[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("backup: encrypted archive is truncated (no final chunk)")
		}
		return fmt.Errorf("backup: read encrypted archive: %w", err)
	}
	n := int(binary.BigEndian.Uint32(l[:]))
	if n < d.aead.Overhead() || n > d.chunk+d.aead.Overhead() {
		return errLegacyDecrypt
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(d.r, ct); err != nil {
		return errors.New("backup: encrypted archive is truncated")
	}
	for _, final := range []bool{false, true} {
		pt, err := d.aead.Open(nil, legacyNonce(d.prefix, d.n, final), ct, d.header)
		if err != nil {
			continue
		}
		d.n++
		d.out = pt
		if final {
			d.done = true
			if _, err := d.r.ReadByte(); !errors.Is(err, io.EOF) {
				return errors.New("backup: unexpected data after the final chunk")
			}
		}
		return nil
	}
	return errLegacyDecrypt
}

func (d *legacyReader) Read(p []byte) (int, error) {
	for len(d.out) == 0 {
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			return 0, err
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}
