package credentials

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// keychainValuePrefix marks values Pyre wrote. Values are base64 encoded so
// `security find-generic-password -w` always prints printable ASCII (it
// switches to hex for non-printable data, which would be ambiguous).
const keychainValuePrefix = "pyre-b64:"

// KeychainStore keeps secrets as macOS generic-password items via the
// `security` CLI. Values reach `security` only on stdin (`security -i` with a
// hex-encoded argument), never in argv, logs, or errors.
type KeychainStore struct {
	Security string        // path to security (default "/usr/bin/security")
	Keychain string        // optional keychain file; empty = default search list
	Timeout  time.Duration // per call (default 10s)
	run      runFunc
}

func (k *KeychainStore) Backend() string { return BackendKeychain }

func (k *KeychainStore) bin() string {
	if k.Security == "" {
		return "/usr/bin/security"
	}
	return k.Security
}

func (k *KeychainStore) exec(ctx context.Context, args []string, stdin []byte) (cliResult, error) {
	ctx, cancel := withTimeout(ctx, k.Timeout)
	defer cancel()
	run := k.run
	if run == nil {
		run = runCLI
	}
	return run(ctx, k.bin(), args, stdin)
}

func (k *KeychainStore) withKeychain(args []string) []string {
	if k.Keychain != "" {
		args = append(args, k.Keychain)
	}
	return args
}

func (k *KeychainStore) check(ref Ref, op string) error {
	if ref.Backend != BackendKeychain {
		return storeErr(KindInvalid, BackendKeychain, op, ref, "", "ref belongs to backend "+ref.Backend)
	}
	if err := ref.Validate(); err != nil {
		return storeErr(KindInvalid, BackendKeychain, op, ref, "", err.Error())
	}
	if strings.ContainsAny(k.Keychain, " \t\n\"'\\") {
		return storeErr(KindInvalid, BackendKeychain, op, ref, "use a keychain path without spaces or quotes", "")
	}
	return nil
}

func (k *KeychainStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if err := k.check(ref, "get"); err != nil {
		return Secret{}, err
	}
	res, err := k.exec(ctx, k.withKeychain([]string{"find-generic-password", "-s", ref.Service, "-a", ref.Account, "-w"}), nil)
	if err != nil {
		return Secret{}, storeErr(KindUnavailable, BackendKeychain, "get", ref, "verify /usr/bin/security is executable", Diagnostic(nil, []byte(err.Error())))
	}
	if res.timedOut || res.exit != 0 {
		return Secret{}, classifyKeychain(res, "get", ref, nil)
	}
	out := strings.TrimSuffix(string(res.stdout), "\n")
	if rest, ok := strings.CutPrefix(out, keychainValuePrefix); ok {
		raw, err := base64.StdEncoding.DecodeString(rest)
		if err != nil {
			return Secret{}, storeErr(KindInvalid, BackendKeychain, "get", ref, "re-provision this credential", "stored value is not valid Pyre encoding")
		}
		return SecretFromBytes(raw), nil
	}
	return NewSecret(out), nil
}

func (k *KeychainStore) Put(ctx context.Context, ref Ref, value Secret) error {
	if err := k.check(ref, "put"); err != nil {
		return err
	}
	if value.IsZero() {
		return storeErr(KindInvalid, BackendKeychain, "put", ref, "", "empty value")
	}
	encoded := keychainValuePrefix + base64.StdEncoding.EncodeToString(value.v)
	// `security -i` reads commands from stdin with a ~4 KiB line buffer
	// (measured on macOS 26: 4106-byte lines fail). Base64 contains no
	// characters its tokenizer treats specially, so it is passed as -w.
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l pyre-credential -j pyre-managed -w %s", ref.Service, ref.Account, encoded)
	if k.Keychain != "" {
		line += " " + k.Keychain
	}
	if len(line) > maxSecurityLine {
		return storeErr(KindInvalid, BackendKeychain, "put", ref, fmt.Sprintf("values up to about %d bytes fit the keychain CLI; use the Secret Service or file store for larger credentials", MaxKeychainValue), fmt.Sprintf("value is %d bytes", value.Len()))
	}
	res, err := k.exec(ctx, []string{"-i"}, []byte(line+"\n"))
	if err != nil {
		return storeErr(KindUnavailable, BackendKeychain, "put", ref, "verify /usr/bin/security is executable", "could not run security")
	}
	if res.timedOut || res.exit != 0 || containsAny(string(res.stderr), "returned -") {
		// Never echo tool output for writes: a tool may repeat its input.
		return classifyKeychain(cliResult{exit: res.exit, timedOut: res.timedOut, stderr: []byte(statusCodes(res.stderr))}, "put", ref, nil)
	}
	return nil
}

func (k *KeychainStore) Delete(ctx context.Context, ref Ref) error {
	if err := k.check(ref, "delete"); err != nil {
		return err
	}
	res, err := k.exec(ctx, k.withKeychain([]string{"delete-generic-password", "-s", ref.Service, "-a", ref.Account}), nil)
	if err != nil {
		return storeErr(KindUnavailable, BackendKeychain, "delete", ref, "verify /usr/bin/security is executable", Diagnostic(nil, []byte(err.Error())))
	}
	if res.timedOut || res.exit != 0 {
		e := classifyKeychain(res, "delete", ref, nil)
		if IsNotFound(e) {
			return nil // idempotent delete
		}
		return e
	}
	return nil
}

// maxSecurityLine is the longest command line sent to `security -i`.
const maxSecurityLine = 4000

// MaxKeychainValue is the conservative raw value limit implied by maxSecurityLine.
const MaxKeychainValue = 2048

var statusCode = regexp.MustCompile(`-\d{2,6}\b|User interaction is not allowed|could not be found in the keychain`)

// statusCodes keeps only OSStatus codes and known phrases from tool output.
func statusCodes(stderr []byte) string {
	return strings.Join(statusCode.FindAllString(string(stderr), 4), " ")
}

const keychainLockedHint = "unlock the keychain from a logged-in session (e.g. `security unlock-keychain`) and retry; Pyre will not fall back to plaintext storage"

func classifyKeychain(res cliResult, op string, ref Ref, red *Redactor) error {
	diag := Diagnostic(red, res.stderr)
	errText := string(res.stderr)
	switch {
	case res.timedOut:
		return storeErr(KindLocked, BackendKeychain, op, ref, "timed out waiting for the keychain; it may be locked and waiting for an unlock prompt. "+keychainLockedHint, "")
	case res.exit == 44 || containsAny(errText, "-25300", "could not be found in the keychain"):
		return storeErr(KindNotFound, BackendKeychain, op, ref, "provision the credential on this node first", "")
	case containsAny(errText, "-25308", "user interaction is not allowed", "-25293", "-128", "canceled", "cancelled", "locked"):
		return storeErr(KindLocked, BackendKeychain, op, ref, keychainLockedHint, diag)
	case containsAny(errText, "-25294", "-25307", "no such keychain", "no default keychain"):
		return storeErr(KindUnavailable, BackendKeychain, op, ref, "no usable keychain; check `security list-keychains` and the configured keychain path", diag)
	}
	return storeErr(KindUnavailable, BackendKeychain, op, ref, "inspect the keychain with Keychain Access or `security`", fmt.Sprintf("exit %d: %s", res.exit, diag))
}
