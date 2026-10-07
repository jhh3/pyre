package backup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
	"filippo.io/age/agessh"
	"filippo.io/age/armor"
)

// Encryption: new archives are age files (age-encryption.org/v1), so any
// age implementation decrypts them, e.g.
//
//	age -d -i key.txt pyre-hub-….tar.age | tar -t
//
// Modes (exactly one per archive):
//   - passphrase: an age scrypt recipient (age -p compatible);
//   - recipients: one or more public keys — X25519 (age1…), post-quantum
//     hybrid ML-KEM-768+X25519 (age1pq1…) or SSH (ssh-ed25519/ssh-rsa).
//     age refuses to mix hybrid recipients with classic ones, because that
//     would defeat the post-quantum protection;
//   - unencrypted: an explicit opt-out.
//
// The tar is encrypted while it is produced; no plaintext archive is ever
// written. Restore detects the format from the first bytes: age (binary or
// ASCII armored), the legacy pyre format (legacy.go, read only), or a
// plaintext tar.

// Format names the detected archive encoding.
type Format string

const (
	FormatPlain    Format = "plaintext tar"
	FormatAge      Format = "age"
	FormatAgeArmor Format = "age (armored)"
	FormatLegacy   Format = "legacy pyre-hub-backup-enc/1"
)

const (
	ageMagic   = "age-encryption.org/v1\n"
	armorMagic = "-----BEGIN AGE ENCRYPTED FILE-----"
)

var (
	// ErrPassphraseRequired: a legacy archive needs its passphrase.
	ErrPassphraseRequired = errors.New("the backup is encrypted with a passphrase (legacy pyre format); provide its passphrase")
	// ErrKeyRequired: an age archive was given without any identity or
	// passphrase.
	ErrKeyRequired = errors.New("the backup is encrypted (age); provide an identity file or the passphrase")
	// ErrWrongKey: none of the supplied identities/passphrase opens the
	// archive.
	ErrWrongKey = errors.New("none of the supplied keys opens the backup")
	// ErrNoEncryption: Create was given neither a passphrase nor recipients
	// and Unencrypted is false.
	ErrNoEncryption = errors.New("backup: refusing to write an unencrypted archive; give a passphrase or recipients (or opt out explicitly)")
	// ErrMixedRecipients: a passphrase was combined with recipients, or
	// post-quantum recipients with classic ones.
	ErrMixedRecipients = errors.New("backup: incompatible encryption options")
)

// Mode describes how Options encrypt a new archive.
func (o Options) Mode() string {
	switch {
	case len(o.Passphrase) > 0:
		return "age, passphrase (scrypt)"
	case len(o.Recipients) > 0:
		if hasHybrid(o.Recipients) {
			return fmt.Sprintf("age, %d post-quantum hybrid recipient(s)", len(o.Recipients))
		}
		return fmt.Sprintf("age, %d recipient(s)", len(o.Recipients))
	default:
		return "UNENCRYPTED"
	}
}

func hasHybrid(rs []age.Recipient) bool {
	for _, r := range rs {
		if _, ok := r.(*age.HybridRecipient); ok {
			return true
		}
	}
	return false
}

// recipients validates the encryption options and returns the age
// recipients for a new archive (nil: unencrypted).
func (o Options) recipients() ([]age.Recipient, error) {
	switch {
	case len(o.Passphrase) > 0 && len(o.Recipients) > 0:
		return nil, fmt.Errorf("%w: a passphrase cannot be combined with recipients (age requires a passphrase to be the only recipient)", ErrMixedRecipients)
	case (len(o.Passphrase) > 0 || len(o.Recipients) > 0) && o.Unencrypted:
		return nil, fmt.Errorf("%w: Unencrypted conflicts with a passphrase or recipients", ErrMixedRecipients)
	case len(o.Passphrase) > 0:
		r, err := age.NewScryptRecipient(string(o.Passphrase))
		if err != nil {
			return nil, fmt.Errorf("backup: passphrase: %w", err)
		}
		if o.ScryptLogN > 0 {
			r.SetWorkFactor(o.ScryptLogN)
		}
		return []age.Recipient{r}, nil
	case len(o.Recipients) > 0:
		pq := 0
		for _, r := range o.Recipients {
			if _, ok := r.(*age.HybridRecipient); ok {
				pq++
			}
		}
		if pq > 0 && pq < len(o.Recipients) {
			return nil, fmt.Errorf("%w: post-quantum (age1pq1…) recipients cannot be mixed with X25519/SSH recipients; use only age1pq1… keys, or only classic keys", ErrMixedRecipients)
		}
		return append([]age.Recipient(nil), o.Recipients...), nil
	case o.Unencrypted:
		return nil, nil
	default:
		return nil, ErrNoEncryption
	}
}

// ParseRecipient parses one public recipient: age1… (X25519), age1pq1…
// (post-quantum hybrid) or an SSH public key line (ssh-ed25519, ssh-rsa).
func ParseRecipient(s string) (age.Recipient, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "age1pq1"):
		return age.ParseHybridRecipient(s)
	case strings.HasPrefix(s, "age1"):
		return age.ParseX25519Recipient(s)
	case strings.HasPrefix(s, "ssh-"):
		return agessh.ParseRecipient(s)
	case strings.HasPrefix(s, "AGE-SECRET-KEY"):
		return nil, errors.New("this is an age PRIVATE key; give the public recipient (age1…), e.g. from `age-keygen -y`")
	}
	return nil, fmt.Errorf("unsupported recipient %.12q… (want age1…, age1pq1… or ssh-ed25519/ssh-rsa; plugin recipients need the age CLI)", s)
}

// ReadRecipientsFile parses a recipients file: one recipient per line,
// empty lines and lines starting with "#" ignored (age -R syntax).
func ReadRecipientsFile(path string) ([]age.Recipient, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []age.Recipient
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r, err := ParseRecipient(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: no recipients", path)
	}
	return out, nil
}

// ReadIdentityFile parses an age identity file (AGE-SECRET-KEY-1… and
// AGE-SECRET-KEY-PQ-1… lines, "#" comments) or an unencrypted OpenSSH
// private key. Passphrase-protected SSH keys and plugin identities are not
// supported here; decrypt those with the age CLI and pipe the tar to
// `pyre-hub restore --in -`.
func ReadIdentityFile(path string) ([]age.Identity, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(b, []byte("-----BEGIN")) && bytes.Contains(b, []byte("PRIVATE KEY-----")) {
		id, err := agessh.ParseIdentity(b)
		if err != nil {
			return nil, fmt.Errorf("%s: SSH identity: %w (passphrase-protected SSH keys: use the age CLI)", path, err)
		}
		return []age.Identity{id}, nil
	}
	ids, err := age.ParseIdentities(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ids, nil
}

// stanzaTypes lists the recipient stanza types in an age header (best
// effort, from the peeked prefix; armored files return nil).
func stanzaTypes(head []byte) []string {
	if !bytes.HasPrefix(head, []byte(ageMagic)) {
		return nil
	}
	var out []string
	for _, line := range bytes.Split(head[len(ageMagic):], []byte("\n")) {
		if bytes.HasPrefix(line, []byte("---")) {
			break
		}
		if f := bytes.Fields(line); len(f) >= 2 && string(f[0]) == "->" {
			out = append(out, string(f[1]))
		}
	}
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// openArchive detects the archive format from r and returns the plaintext
// tar stream. Decryption is streaming: nothing is written to disk.
func openArchive(r io.Reader, opts Options) (io.Reader, Format, error) {
	br := bufio.NewReaderSize(r, 256<<10)
	head, _ := br.Peek(64 << 10) // short archives: whatever is there
	switch {
	case isLegacy(head):
		if len(opts.Passphrase) == 0 {
			return nil, FormatLegacy, ErrPassphraseRequired
		}
		lr, err := newLegacyReader(br, opts.Passphrase)
		if err != nil {
			return nil, FormatLegacy, err
		}
		return lr, FormatLegacy, nil
	case bytes.HasPrefix(head, []byte(ageMagic)), bytes.HasPrefix(bytes.TrimLeft(head, " \t\r\n"), []byte(armorMagic)):
		format := FormatAge
		var src io.Reader = br
		if !bytes.HasPrefix(head, []byte(ageMagic)) {
			format = FormatAgeArmor
			src = armor.NewReader(br)
		}
		types := stanzaTypes(head)
		ids := append([]age.Identity(nil), opts.Identities...)
		if len(opts.Passphrase) > 0 {
			sid, err := age.NewScryptIdentity(string(opts.Passphrase))
			if err != nil {
				return nil, format, err
			}
			ids = append(ids, sid)
		}
		if len(ids) == 0 {
			if contains(types, "scrypt") {
				return nil, format, fmt.Errorf("%w (this one uses a passphrase)", ErrKeyRequired)
			}
			if len(types) > 0 {
				return nil, format, fmt.Errorf("%w (it is encrypted to: %s)", ErrKeyRequired, strings.Join(types, ", "))
			}
			return nil, format, ErrKeyRequired
		}
		pr, err := age.Decrypt(src, ids...)
		if err != nil {
			var nm *age.NoIdentityMatchError
			if errors.As(err, &nm) {
				types = nm.StanzaTypes
				if contains(types, "scrypt") {
					if len(opts.Passphrase) == 0 {
						return nil, format, fmt.Errorf("%w: the backup uses a passphrase; provide it", ErrWrongKey)
					}
					return nil, format, fmt.Errorf("%w: wrong passphrase", ErrWrongKey)
				}
				return nil, format, fmt.Errorf("%w (it is encrypted to: %s)", ErrWrongKey, strings.Join(types, ", "))
			}
			return nil, format, fmt.Errorf("backup: age: %w", err)
		}
		return pr, format, nil
	default:
		return br, FormatPlain, nil
	}
}
