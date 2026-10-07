package credentials

import (
	"context"
	"fmt"
	"time"
)

// SecretServiceStore keeps secrets in the freedesktop Secret Service (GNOME
// Keyring, KeePassXC, ...) through libsecret's `secret-tool`. Values are
// written on stdin and read from stdout; attributes are the Ref's service and
// account. A missing provider, missing D-Bus session, or locked collection
// produces an actionable error, never a plaintext fallback.
type SecretServiceStore struct {
	Tool    string        // path to secret-tool
	Timeout time.Duration // per call (default 10s); a locked collection awaiting a prompt times out
	run     runFunc
}

func (s *SecretServiceStore) Backend() string { return BackendSecretService }

func (s *SecretServiceStore) exec(ctx context.Context, args []string, stdin []byte) (cliResult, error) {
	ctx, cancel := withTimeout(ctx, s.Timeout)
	defer cancel()
	run := s.run
	if run == nil {
		run = runCLI
	}
	tool := s.Tool
	if tool == "" {
		tool = "secret-tool"
	}
	return run(ctx, tool, args, stdin)
}

func (s *SecretServiceStore) check(ref Ref, op string) error {
	if ref.Backend != BackendSecretService {
		return storeErr(KindInvalid, BackendSecretService, op, ref, "", "ref belongs to backend "+ref.Backend)
	}
	if err := ref.Validate(); err != nil {
		return storeErr(KindInvalid, BackendSecretService, op, ref, "", err.Error())
	}
	return nil
}

func attrs(ref Ref) []string {
	return []string{"service", ref.Service, "account", ref.Account}
}

func (s *SecretServiceStore) Get(ctx context.Context, ref Ref) (Secret, error) {
	if err := s.check(ref, "get"); err != nil {
		return Secret{}, err
	}
	res, err := s.exec(ctx, append([]string{"lookup"}, attrs(ref)...), nil)
	if err != nil {
		return Secret{}, storeErr(KindUnavailable, BackendSecretService, "get", ref, secretToolHint, Diagnostic(nil, []byte(err.Error())))
	}
	if res.timedOut || len(res.stderr) > 0 && res.exit != 0 {
		return Secret{}, classifySecretTool(res, "get", ref, nil)
	}
	if res.exit != 0 || len(res.stdout) == 0 {
		// secret-tool exits 1 with no output when nothing matches.
		return Secret{}, storeErr(KindNotFound, BackendSecretService, "get", ref, "provision the credential on this node first", "")
	}
	return SecretFromBytes(res.stdout), nil
}

func (s *SecretServiceStore) Put(ctx context.Context, ref Ref, value Secret) error {
	if err := s.check(ref, "put"); err != nil {
		return err
	}
	if value.IsZero() {
		return storeErr(KindInvalid, BackendSecretService, "put", ref, "", "empty value")
	}
	args := append([]string{"store", "--label=Pyre credential " + ref.Service + "/" + ref.Account}, attrs(ref)...)
	res, err := s.exec(ctx, args, value.Bytes())
	red := NewRedactor(value)
	if err != nil {
		return storeErr(KindUnavailable, BackendSecretService, "put", ref, secretToolHint, Diagnostic(red, []byte(err.Error())))
	}
	if res.timedOut || res.exit != 0 {
		return classifySecretTool(res, "put", ref, red)
	}
	return nil
}

func (s *SecretServiceStore) Delete(ctx context.Context, ref Ref) error {
	if err := s.check(ref, "delete"); err != nil {
		return err
	}
	res, err := s.exec(ctx, append([]string{"clear"}, attrs(ref)...), nil)
	if err != nil {
		return storeErr(KindUnavailable, BackendSecretService, "delete", ref, secretToolHint, Diagnostic(nil, []byte(err.Error())))
	}
	if res.timedOut || (res.exit != 0 && len(res.stderr) > 0) {
		return classifySecretTool(res, "delete", ref, nil)
	}
	return nil // clear of a missing item is a no-op
}

const secretServiceLockedHint = "unlock the default keyring collection (log in to a desktop session, or run `gnome-keyring-daemon --unlock` / unlock KeePassXC) and retry; Pyre will not fall back to plaintext storage"

func classifySecretTool(res cliResult, op string, ref Ref, red *Redactor) error {
	diag := Diagnostic(red, res.stderr)
	text := string(res.stderr)
	switch {
	case res.timedOut:
		return storeErr(KindLocked, BackendSecretService, op, ref, "timed out waiting for the Secret Service; the collection may be locked and waiting for an unlock prompt. "+secretServiceLockedHint, "")
	case containsAny(text, "locked", "prompt", "dismissed", "unlock"):
		return storeErr(KindLocked, BackendSecretService, op, ref, secretServiceLockedHint, diag)
	case containsAny(text, "org.freedesktop.secrets", "was not provided by any .service", "cannot autolaunch", "d-bus", "dbus", "no such secret collection", "not activatable"):
		return storeErr(KindUnavailable, BackendSecretService, op, ref, "no Secret Service provider is reachable on the session bus. "+secretToolHint, diag)
	}
	return storeErr(KindUnavailable, BackendSecretService, op, ref, secretToolHint, fmt.Sprintf("exit %d: %s", res.exit, diag))
}
