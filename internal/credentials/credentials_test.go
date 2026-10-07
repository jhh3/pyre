package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Synthetic credential values. They are obviously fake and exist only so the
// tests can grep for disclosure.
const (
	synthA = "SYNTHETIC-pyre-cred-alpha-7f3a9c"
	synthB = "SYNTHETIC-pyre-cred-bravo-19be04"
)

func TestSecretNeverFormatsOrSerializes(t *testing.T) {
	s := NewSecret(synthA)
	for _, out := range []string{fmt.Sprint(s), fmt.Sprintf("%s %v %+v %#v %q %x", s, s, s, s, s, s), s.String()} {
		if strings.Contains(out, synthA) || !strings.Contains(out, Redacted) {
			t.Fatalf("secret leaked through formatting: %q", out)
		}
	}
	if _, err := json.Marshal(s); err == nil {
		t.Fatal("json.Marshal(Secret) must fail")
	}
	if _, err := json.Marshal(struct{ S Secret }{s}); err == nil {
		t.Fatal("json.Marshal of a struct holding a Secret must fail")
	}
	var buf bytes.Buffer
	slog.New(slog.NewTextHandler(&buf, nil)).Info("x", "secret", s, "err", fmt.Errorf("wrap %v", s))
	if strings.Contains(buf.String(), synthA) {
		t.Fatalf("secret leaked through slog: %s", buf.String())
	}
	if s.Reveal() != synthA {
		t.Fatal("Reveal must return the value")
	}
	s.Wipe()
	if !s.IsZero() {
		t.Fatal("Wipe must clear")
	}
}

func TestRefRoundTripAndValidation(t *testing.T) {
	r := NewRef(BackendKeychain, "conn_github-work", "GH_TOKEN")
	if r.String() != "keychain:pyre:conn_github-work/GH_TOKEN" {
		t.Fatalf("unexpected ref %s", r)
	}
	got, err := ParseRef(r.String())
	if err != nil || got != r {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	for _, bad := range []Ref{
		{Backend: BackendKeychain, Service: "dev.jhh3.outfit", Account: "x"}, // not Pyre-owned
		{Backend: BackendKeychain, Service: "pyre:a b", Account: "x"},
		{Backend: BackendKeychain, Service: "pyre:a", Account: "-w"},
		{Backend: "plaintext", Service: "pyre:a", Account: "x"},
	} {
		if bad.Validate() == nil {
			t.Fatalf("expected invalid: %+v", bad)
		}
	}
}

// fakeRunner records CLI invocations and answers from a script.
type fakeRunner struct {
	calls []fakeCall
	reply func(args []string, stdin []byte) cliResult
}

type fakeCall struct {
	path  string
	args  []string
	stdin []byte
}

func (f *fakeRunner) run(_ context.Context, path string, args []string, stdin []byte) (cliResult, error) {
	f.calls = append(f.calls, fakeCall{path, append([]string(nil), args...), append([]byte(nil), stdin...)})
	return f.reply(args, stdin), nil
}

func TestKeychainPassesValueOnlyOnStdin(t *testing.T) {
	stored := map[string]string{}
	fr := &fakeRunner{reply: func(args []string, stdin []byte) cliResult {
		switch args[0] {
		case "-i":
			fields := strings.Fields(string(stdin))
			var svc, acct, pw string
			for i := 0; i+1 < len(fields); i++ {
				switch fields[i] {
				case "-s":
					svc = fields[i+1]
				case "-a":
					acct = fields[i+1]
				case "-w":
					pw = fields[i+1]
				}
			}
			stored[svc+"/"+acct] = pw
			return cliResult{}
		case "find-generic-password":
			v, ok := stored[args[2]+"/"+args[4]]
			if !ok {
				return cliResult{exit: 44, stderr: []byte("security: SecKeychainSearchCopyNext: The specified item could not be found in the keychain.\n")}
			}
			return cliResult{stdout: []byte(v + "\n")}
		}
		return cliResult{}
	}}
	k := &KeychainStore{Security: "/usr/bin/security", run: fr.run}
	ref := NewRef(BackendKeychain, "conn_x", "TOKEN")
	value := synthA + "\nwith newline and ünïcode"
	if err := k.Put(context.Background(), ref, NewSecret(value)); err != nil {
		t.Fatal(err)
	}
	got, err := k.Get(context.Background(), ref)
	if err != nil || got.Reveal() != value {
		t.Fatalf("get = %q, %v", got.Reveal(), err)
	}
	for _, c := range fr.calls {
		if strings.Contains(strings.Join(c.args, " "), synthA) || strings.Contains(strings.Join(c.args, " "), fmt.Sprintf("%x", synthA)) {
			t.Fatalf("value appeared in argv: %v", c.args)
		}
		if c.args[0] == "-i" && bytes.Contains(c.stdin, []byte(synthA)) {
			t.Fatal("stdin must carry an encoded form, not the raw value")
		}
	}
	_, err = k.Get(context.Background(), NewRef(BackendKeychain, "conn_x", "MISSING"))
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestKeychainLockedAndTimeoutAreActionable(t *testing.T) {
	ref := NewRef(BackendKeychain, "conn_x", "TOKEN")
	cases := []cliResult{
		{exit: 36, stderr: []byte("security: SecKeychainItemCopyContent: User interaction is not allowed. (-25308)\n")},
		{timedOut: true},
	}
	for _, res := range cases {
		res := res
		k := &KeychainStore{run: (&fakeRunner{reply: func([]string, []byte) cliResult { return res }}).run}
		_, err := k.Get(context.Background(), ref)
		if !IsLocked(err) || !strings.Contains(err.Error(), "unlock") {
			t.Fatalf("expected actionable locked error, got %v", err)
		}
		if ce := err.(*StoreError).Contract(); ce.Code != contracts.ErrUnavailable || ce.Details["availability"] != "locked_store" {
			t.Fatalf("unexpected contract error %+v", ce)
		}
		if AvailabilityState(err) != "locked_store" {
			t.Fatal("locked must map to locked_store availability")
		}
	}
}

func TestKeychainPutErrorRedactsEchoedValue(t *testing.T) {
	k := &KeychainStore{run: (&fakeRunner{reply: func(_ []string, stdin []byte) cliResult {
		// A misbehaving tool echoing its input must not leak through errors.
		return cliResult{exit: 1, stderr: append([]byte("add-generic-password: returned -25299 for input: "), stdin...)}
	}}).run}
	err := k.Put(context.Background(), NewRef(BackendKeychain, "conn_x", "TOKEN"), NewSecret(synthA))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), synthA) || strings.Contains(err.Error(), keychainValuePrefix) || strings.Contains(err.Error(), "U1lOVEhF") {
		t.Fatalf("error leaked value or its encoding: %v", err)
	}
	if !strings.Contains(err.Error(), "-25299") {
		t.Fatalf("error should keep the OSStatus code: %v", err)
	}
	big := NewSecret(strings.Repeat("y", MaxKeychainValue*2))
	if err := k.Put(context.Background(), NewRef(BackendKeychain, "conn_x", "BIG"), big); kindOf(err) != KindInvalid {
		t.Fatalf("oversized value must be refused before calling security: %v", err)
	}
}

func TestSecretToolStoreAndClassification(t *testing.T) {
	stored := map[string][]byte{}
	fr := &fakeRunner{reply: func(args []string, stdin []byte) cliResult {
		key := strings.Join(args[len(args)-4:], "|")
		switch args[0] {
		case "store":
			stored[key] = stdin
		case "lookup":
			v, ok := stored[key]
			if !ok {
				return cliResult{exit: 1}
			}
			return cliResult{stdout: v}
		case "clear":
			delete(stored, key)
		}
		return cliResult{}
	}}
	s := &SecretServiceStore{Tool: "/usr/bin/secret-tool", run: fr.run}
	ref := NewRef(BackendSecretService, "conn_y", "LINEAR_API_KEY")
	if err := s.Put(context.Background(), ref, NewSecret(synthB)); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(context.Background(), ref); err != nil || got.Reveal() != synthB {
		t.Fatalf("get = %v", err)
	}
	for _, c := range fr.calls {
		if strings.Contains(strings.Join(c.args, " "), synthB) {
			t.Fatalf("value in argv: %v", c.args)
		}
	}
	if err := s.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(context.Background(), ref); !IsNotFound(err) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
	for res, want := range map[*cliResult]ErrorKind{
		{exit: 1, stderr: []byte("secret-tool: Cannot get secret of a locked object\n")}:                                    KindLocked,
		{exit: 1, stderr: []byte("secret-tool: The name org.freedesktop.secrets was not provided by any .service files\n")}: KindUnavailable,
		{timedOut: true}: KindLocked,
	} {
		res := *res
		st := &SecretServiceStore{run: (&fakeRunner{reply: func([]string, []byte) cliResult { return res }}).run}
		_, err := st.Get(context.Background(), ref)
		if kindOf(err) != want || !strings.Contains(err.Error(), "Pyre will not fall back") && want == KindLocked {
			t.Fatalf("stderr %q => %v (want %s)", res.stderr, err, want)
		}
	}
}

func TestOpenNeverFallsBackToPlaintext(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	st, err := Open(Config{GOOS: "linux", LookPath: missing, FileDir: t.TempDir(), AllowFileStore: true})
	if st != nil || !IsUnavailable(err) {
		t.Fatalf("linux without secret-tool must be unavailable even when a file dir is configured: %v %v", st, err)
	}
	if !strings.Contains(err.Error(), "libsecret") || !strings.Contains(err.Error(), "never falls back") {
		t.Fatalf("error must be actionable: %v", err)
	}
	if _, err := Open(Config{Backend: BackendFile, FileDir: t.TempDir()}); kindOf(err) != KindRefused {
		t.Fatalf("file store without opt-in must be refused: %v", err)
	}
	found := func(name string) (string, error) { return "/usr/bin/" + name, nil }
	if st, err := Open(Config{GOOS: "darwin", LookPath: found}); err != nil || st.Backend() != BackendKeychain {
		t.Fatalf("darwin auto = %v %v", st, err)
	}
	if st, err := Open(Config{GOOS: "linux", LookPath: found}); err != nil || st.Backend() != BackendSecretService {
		t.Fatalf("linux auto = %v %v", st, err)
	}
}

func TestFileStorePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "creds")
	fs, err := NewFileStore(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	ref := NewRef(BackendFile, "conn_z", "TOKEN")
	if err := fs.Put(context.Background(), ref, NewSecret(synthA)); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(filepath.Join(dir, fileStoreName))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %o", st.Mode().Perm())
	}
	if got, err := fs.Get(context.Background(), ref); err != nil || got.Reveal() != synthA {
		t.Fatalf("get: %v", err)
	}
	os.Chmod(filepath.Join(dir, fileStoreName), 0o644)
	if _, err := fs.Get(context.Background(), ref); kindOf(err) != KindRefused || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("broad file mode must be refused with a fix: %v", err)
	}
	os.Chmod(filepath.Join(dir, fileStoreName), 0o600)
	os.Chmod(dir, 0o755)
	if _, err := fs.Get(context.Background(), ref); kindOf(err) != KindRefused {
		t.Fatalf("broad dir mode must be refused: %v", err)
	}
	os.Chmod(dir, 0o700)
	if err := fs.Delete(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Get(context.Background(), ref); !IsNotFound(err) {
		t.Fatalf("expected not found: %v", err)
	}
}

func TestResolverFailsClosedAndManifestHasNamesOnly(t *testing.T) {
	mem := NewMemoryStore()
	ctx := context.Background()
	ra, rb := NewRef(BackendMemory, "conn_a", "A_TOKEN"), NewRef(BackendMemory, "conn_b", "B_TOKEN")
	mem.Put(ctx, ra, NewSecret(synthA))
	mem.Put(ctx, rb, NewSecret(synthB))
	r := &Resolver{Stores: Stores{}.Add(mem)}
	env, m, err := r.Resolve(ctx, []LaunchGrant{{ConnectionID: "conn_a", EnvVar: "A_TOKEN", Ref: ra}, {ConnectionID: "conn_b", EnvVar: "B_TOKEN", Ref: rb}})
	if err != nil || env["A_TOKEN"] != synthA || env["B_TOKEN"] != synthB {
		t.Fatalf("resolve: %v", err)
	}
	mb, _ := json.Marshal(m)
	if strings.Contains(string(mb), synthA) || strings.Contains(string(mb), synthB) || strings.Join(m.EnvNames(), ",") != "A_TOKEN,B_TOKEN" {
		t.Fatalf("manifest leaked or wrong: %s", mb)
	}
	mem.SetLocked(true)
	env, _, err = r.Resolve(ctx, []LaunchGrant{{ConnectionID: "conn_a", EnvVar: "A_TOKEN", Ref: ra}})
	if env != nil || !IsLocked(err) || !strings.Contains(err.Error(), "conn_a") || !strings.Contains(err.Error(), "A_TOKEN") {
		t.Fatalf("locked store must fail closed with an actionable error: env=%v err=%v", env, err)
	}
	mem.SetLocked(false)
	for _, bad := range [][]LaunchGrant{
		{{ConnectionID: "conn_a", EnvVar: "PATH", Ref: ra}},
		{{ConnectionID: "conn_a", EnvVar: "PYRE_SESSION_TOKEN", Ref: ra}},
		{{ConnectionID: "conn_a", EnvVar: "X", Ref: ra}, {ConnectionID: "conn_b", EnvVar: "X", Ref: rb}},
		{{ConnectionID: "conn_c", EnvVar: "C_TOKEN", Ref: NewRef(BackendKeychain, "conn_c", "C_TOKEN")}}, // no keychain store configured
	} {
		if env, _, err := r.Resolve(ctx, bad); err == nil || env != nil {
			t.Fatalf("expected refusal for %+v", bad)
		} else if strings.Contains(err.Error(), synthA) || strings.Contains(err.Error(), synthB) {
			t.Fatalf("error leaked a value: %v", err)
		}
	}
}

func TestIndexPersistsNamesOnlyWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "node", "provisions.json")
	ix := OpenIndex(path)
	ref := NewRef(BackendMemory, "conn_a", "A_TOKEN")
	if err := ix.Upsert(Provision{ConnectionID: "conn_a", EnvVar: "A_TOKEN", Ref: ref.String(), ProvisionedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("index mode %o", st.Mode().Perm())
	}
	grants, missing, err := GrantsFor(ix, []string{"conn_a", "conn_b"})
	if err != nil || len(grants) != 1 || grants[0].Ref != ref || len(missing) != 1 || missing[0] != "conn_b" {
		t.Fatalf("grants=%+v missing=%v err=%v", grants, missing, err)
	}
	removed, err := ix.Remove("conn_a", "")
	if err != nil || len(removed) != 1 {
		t.Fatalf("remove: %v %v", removed, err)
	}
}

func TestSealedProvisioningIsSingleUseAndBound(t *testing.T) {
	ctx := context.Background()
	recipient := NewRecipient("node_b", nil)
	ref := NewRef(BackendMemory, "conn_a", "A_TOKEN")
	offer, err := recipient.NewOffer("conn_a", "A_TOKEN", ref, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Seal(offer, "node_a", NewSecret(synthA), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	wire, _ := json.Marshal(struct {
		O ProvisionOffer
		S SealedCredential
	}{offer, sealed})
	if bytes.Contains(wire, []byte(synthA)) {
		t.Fatal("offer/sealed wire form contains plaintext")
	}
	mem := NewMemoryStore()
	ix := OpenIndex("")
	// Redirected sender (AAD mismatch) fails and consumes the offer.
	bad := sealed
	bad.SenderNode = "node_evil"
	if _, err := recipient.Accept(ctx, bad, Stores{}.Add(mem), ix); err == nil {
		t.Fatal("tampered sender must fail")
	}
	if _, err := recipient.Accept(ctx, sealed, Stores{}.Add(mem), ix); !contracts.IsCode(err, contracts.ErrNotFound) {
		t.Fatalf("consumed offer must not be reusable: %v", err)
	}
	// Fresh offer: success then replay rejected.
	offer, _ = recipient.NewOffer("conn_a", "A_TOKEN", ref, time.Minute)
	sealed, _ = Seal(offer, "node_a", NewSecret(synthA), time.Now())
	prov, err := recipient.Accept(ctx, sealed, Stores{}.Add(mem), ix)
	if err != nil || prov.SourceNode != "node_a" || prov.EnvVar != "A_TOKEN" {
		t.Fatalf("accept: %+v %v", prov, err)
	}
	if got, _ := mem.Get(ctx, ref); got.Reveal() != synthA {
		t.Fatal("value not stored")
	}
	if _, err := recipient.Accept(ctx, sealed, Stores{}.Add(mem), ix); err == nil {
		t.Fatal("replay must fail")
	}
	// Expired offer.
	offer, _ = recipient.NewOffer("conn_a", "A_TOKEN", ref, time.Minute)
	if _, err := Seal(offer, "node_a", NewSecret(synthA), time.Now().Add(2*time.Minute)); !contracts.IsCode(err, contracts.ErrExpired) {
		t.Fatalf("expired offer must not seal: %v", err)
	}
	if recipient.Pending() != 1 {
		t.Fatalf("pending = %d", recipient.Pending())
	}
}

func TestRedaction(t *testing.T) {
	r := NewRedactor(NewSecret(synthA), NewSecret("abc")) // short values are ignored
	in := "auth failed for " + synthA + " using ghp_abcdefghijklmnopqrstuvwxyz0123 and Authorization: Bearer abcdefghijklmnop.qrs and token=sk_live_0123456789abcdef abc"
	out := r.String(in)
	for _, leak := range []string{synthA, "ghp_abcdefghijklmnopqrstuvwxyz0123", "abcdefghijklmnop.qrs", "sk_live_0123456789abcdef"} {
		if strings.Contains(out, leak) {
			t.Fatalf("redaction missed %q: %s", leak, out)
		}
	}
	if !strings.HasSuffix(out, " abc") {
		t.Fatalf("short known value must not be replaced: %s", out)
	}
	if !r.Contains([]byte("x" + synthA)) {
		t.Fatal("Contains")
	}
	if got := EnvNames(map[string]string{"B": synthB, "A": synthA}); strings.Join(got, ",") != "A,B" {
		t.Fatal(got)
	}
}
