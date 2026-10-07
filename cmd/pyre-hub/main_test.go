package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/hub/assemble"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, &out, &errb)
	return code, out.String(), errb.String()
}

func readCred(t *testing.T, dir string) ownerCredential {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, OwnerCredentialFile))
	if err != nil {
		t.Fatal(err)
	}
	var c ownerCredential
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestInitAndUsage(t *testing.T) {
	if code, _, _ := runCmd(t); code != 2 {
		t.Fatalf("no args: %d", code)
	}
	if code, _, _ := runCmd(t, "serve"); code != 2 {
		t.Fatal("serve without --data should be a usage error")
	}
	dir := filepath.Join(t.TempDir(), "hub")
	code, out, errs := runCmd(t, "init", "--data", dir, "--hub-url", "https://hub.synth.invalid:7443")
	if code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	cred := readCred(t, dir)
	if !strings.HasPrefix(cred.ClientID, "cli_") || cred.Token == "" || cred.HubCertSHA256 == "" {
		t.Fatalf("credential: %+v", cred)
	}
	if strings.Contains(out, cred.Token) || strings.Contains(errs, cred.Token) {
		t.Fatal("token printed")
	}
	st, _ := os.Stat(filepath.Join(dir, OwnerCredentialFile))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode %v", st.Mode().Perm())
	}
	if code, _, _ := runCmd(t, "init", "--data", dir); code != 1 {
		t.Fatal("second init should refuse")
	}
}

func freeAddr(t *testing.T) string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestServeOverTLSWithClient(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	if code, _, errs := runCmd(t, "init", "--data", dir); code != 0 {
		t.Fatal(errs)
	}
	cred := readCred(t, dir)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() { done <- run(ctx, []string{"serve", "--data", dir, "--listen", addr}, io.Discard, &errb) }()
	cl := client.New("https://"+addr, cred.Token, client.Options{CertSHA256: cred.HubCertSHA256, MaxAttempts: 1, Timeout: 2 * time.Second})
	var hello contracts.HelloResponse
	var err error
	for i := 0; i < 100; i++ {
		hello, err = cl.Hello(context.Background(), contracts.HelloRequest{Protocol: contracts.ProtocolVersion, Client: "pyre-test/0"})
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("hello over TLS: %v (%s)", err, errb.String())
	}
	if hello.Principal.ID != cred.ClientID || hello.Principal.Kind != contracts.PrincipalOwner || hello.Mock {
		t.Fatalf("hello: %+v", hello)
	}
	// Composed domains answer through the same server.
	p, err := cl.CreateProject(context.Background(), contracts.CreateProjectRequest{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)}, Name: "served"})
	if err != nil || p.ProjectID == "" {
		t.Fatalf("project via serve: %v", err)
	}
	if _, err := cl.ListNodes(context.Background(), contracts.ListNodesRequest{}); err != nil {
		t.Fatalf("fleet via serve: %v", err)
	}
	bad := client.New("https://"+addr, "pyc_not-a-real-token", client.Options{CertSHA256: cred.HubCertSHA256, MaxAttempts: 1})
	if _, err := bad.WhoAmI(context.Background()); !contracts.IsCode(err, contracts.ErrUnauthorized) {
		t.Fatalf("bad token: %v", err)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit %d: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	if strings.Contains(errb.String(), cred.Token) {
		t.Fatal("token logged")
	}
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "hub")
	if code, _, errs := runCmd(t, "init", "--data", dir); code != 0 {
		t.Fatal(errs)
	}
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-backup"})
	st, err := assemble.Open(context.Background(), assemble.Config{DataDir: dir, Logger: quietLogger(io.Discard)})
	if err != nil {
		t.Fatal(err)
	}
	hubID := st.Hub.HubID()
	data := []byte("synthetic artifact bytes for backup")
	sum := sha256.Sum256(data)
	dg := contracts.Digest("sha256:" + hex.EncodeToString(sum[:]))
	up, err := st.Hub.BeginUpload(ctx, contracts.BeginUploadRequest{Mutation: contracts.Mutation{OperationID: "op_up"}, Title: "report", Filename: "r.txt", MediaType: "text/plain", Size: int64(len(data)), Digest: dg})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Hub.UploadChunk(ctx, up.UploadID, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	rec, err := st.Hub.FinalizeUpload(ctx, contracts.FinalizeUploadRequest{Mutation: contracts.Mutation{OperationID: "op_fin"}, UploadID: up.UploadID})
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.Hub.SendMessage(ctx, contracts.SendMessageRequest{Mutation: contracts.Mutation{OperationID: "op_msg"}, Recipient: contracts.Address{Kind: contracts.AddrOwner}, Body: contracts.MessageBody{Text: "synthetic note"}})
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := st.Hub.ListEvents(ctx, contracts.ListEventsRequest{})
	st.Close()

	archive := filepath.Join(base, "backup.tar")
	if code, out, errs := runCmd(t, "backup", "--data", dir, "--out", archive, "--unencrypted"); code != 0 || !strings.Contains(out, hubID) || !strings.Contains(errs, "UNENCRYPTED") {
		t.Fatalf("backup: %d %s %s", code, out, errs)
	}
	if fi, _ := os.Stat(archive); fi.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode %v", fi.Mode().Perm())
	}
	if code, _, _ := runCmd(t, "backup", "--data", dir, "--out", archive, "--unencrypted"); code == 0 {
		t.Fatal("backup overwrote an existing archive")
	}
	restored := filepath.Join(base, "restored")
	if code, out, errs := runCmd(t, "restore", "--in", archive, "--data", restored); code != 0 {
		t.Fatalf("restore: %d %s %s", code, out, errs)
	}
	if code, _, _ := runCmd(t, "restore", "--in", archive, "--data", restored); code == 0 {
		t.Fatal("restore into a non-empty directory succeeded")
	}
	st2, err := assemble.Open(context.Background(), assemble.Config{DataDir: restored, Logger: quietLogger(io.Discard)})
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if st2.Hub.HubID() != hubID {
		t.Fatal("hub identity not restored")
	}
	rc, ver, err := st2.Hub.OpenContent(ctx, rec.Version.VersionID)
	if err != nil {
		t.Fatalf("artifact after restore: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) || ver.Digest != dg {
		t.Fatal("artifact bytes differ after restore")
	}
	if _, err := st2.Hub.GetMessage(ctx, contracts.GetRequest{ID: m.Envelope.MessageID}); err != nil {
		t.Fatalf("message after restore: %v", err)
	}
	evs2, _ := st2.Hub.ListEvents(ctx, contracts.ListEventsRequest{})
	if evs2.HeadCursor < evs.HeadCursor {
		t.Fatalf("event cursor regressed: %d < %d", evs2.HeadCursor, evs.HeadCursor)
	}
	if readCred(t, dir).HubCertSHA256 != st2.Cert.Fingerprint {
		t.Fatal("TLS identity (pin) not restored")
	}

	// Tampering is detected and nothing is restored.
	raw, _ := os.ReadFile(archive)
	idx := bytes.Index(raw, data)
	if idx < 0 {
		t.Fatal("blob not found in archive")
	}
	raw[idx] ^= 0xff
	bad := filepath.Join(base, "tampered.tar")
	os.WriteFile(bad, raw, 0o600)
	target := filepath.Join(base, "restored-bad")
	code, _, errs := runCmd(t, "restore", "--in", bad, "--data", target)
	if code == 0 || !strings.Contains(errs, "verification") {
		t.Fatalf("tampered restore: %d %s", code, errs)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("partial restore left a data directory")
	}
}

func TestDerivePreviewURL(t *testing.T) {
	for _, tc := range []struct{ hub, listen, want string }{
		{"https://hub.synth.invalid:7443", ":8444", "https://hub.synth.invalid:8444"},
		{"https://10.0.0.5:7443/", "0.0.0.0:9000", "https://10.0.0.5:9000"},
		{"https://[fd00::1]:7443", ":8444", "https://[fd00::1]:8444"},
		{"", ":8444", ""},
		{"https://hub.synth.invalid", "bad", ""},
	} {
		if got := derivePreviewURL(tc.hub, tc.listen); got != tc.want {
			t.Errorf("derivePreviewURL(%q, %q) = %q, want %q", tc.hub, tc.listen, got, tc.want)
		}
	}
}

func isAgeFile(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(raw, []byte("age-encryption.org/v1\n")) || bytes.Contains(raw, []byte("PRIVATE KEY")) || bytes.Contains(raw, []byte("MANIFEST.json")) {
		t.Fatalf("%s is not an age-encrypted archive", path)
	}
}

// Backups are encrypted by default (age passphrase); restore detects
// encryption and needs the passphrase.
func TestEncryptedBackupRestore(t *testing.T) {
	scryptLogN = 10
	defer func() { scryptLogN = 0 }()
	base := t.TempDir()
	dir := filepath.Join(base, "hub")
	if code, _, errs := runCmd(t, "init", "--data", dir); code != 0 {
		t.Fatal(errs)
	}
	archive := filepath.Join(base, "enc.tar.age")
	t.Setenv(passphraseEnv, "")
	if code, _, errs := runCmd(t, "backup", "--data", dir, "--out", archive); code == 0 || !strings.Contains(errs, "--passphrase-file") || !strings.Contains(errs, "--recipient") {
		t.Fatalf("backup without passphrase: %d %s", code, errs)
	}
	pass := filepath.Join(base, "pass")
	os.WriteFile(pass, []byte("synthetic passphrase\n"), 0o600)
	if code, _, errs := runCmd(t, "backup", "--data", dir, "--out", archive, "--passphrase-file", pass, "--unencrypted"); code == 0 {
		t.Fatalf("passphrase with --unencrypted accepted: %s", errs)
	}
	if code, out, errs := runCmd(t, "backup", "--data", dir, "--out", archive, "--passphrase-file", pass, "--verify"); code != 0 || !strings.Contains(out, "encrypted: age, passphrase") || !strings.Contains(out, "verified") {
		t.Fatalf("encrypted backup: %d %s %s", code, out, errs)
	}
	isAgeFile(t, archive)
	target := filepath.Join(base, "restored")
	if code, _, errs := runCmd(t, "restore", "--in", archive, "--data", target); code == 0 || !strings.Contains(errs, "encrypted") || !strings.Contains(errs, "passphrase") {
		t.Fatalf("restore without passphrase: %d %s", code, errs)
	}
	wrong := filepath.Join(base, "wrong")
	os.WriteFile(wrong, []byte("not it"), 0o600)
	if code, _, errs := runCmd(t, "restore", "--in", archive, "--data", target, "--passphrase-file", wrong); code == 0 || !strings.Contains(errs, "wrong passphrase") {
		t.Fatalf("restore with wrong passphrase: %d %s", code, errs)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("failed restore left a data directory")
	}
	t.Setenv(passphraseEnv, "synthetic passphrase")
	if code, out, errs := runCmd(t, "restore", "--in", archive, "--verify-only"); code != 0 || !strings.Contains(out, "verified") {
		t.Fatalf("verify-only: %d %s %s", code, out, errs)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("--verify-only wrote a data directory")
	}
	if code, out, errs := runCmd(t, "restore", "--in", archive, "--data", target); code != 0 || !strings.Contains(out, "age") {
		t.Fatalf("restore via env: %d %s %s", code, out, errs)
	}
	for _, f := range []string{"hub.db", "tls"} {
		if _, err := os.Stat(filepath.Join(target, f)); err != nil {
			t.Fatalf("restored data lacks %s: %v", f, err)
		}
	}
	// With the passphrase in the environment, recipients are ambiguous.
	x, _ := age.GenerateX25519Identity()
	if code, _, errs := runCmd(t, "backup", "--data", dir, "--out", archive+"2", "--recipient", x.Recipient().String()); code != 2 || !strings.Contains(errs, "not both") {
		t.Fatalf("env passphrase + recipient: %d %s", code, errs)
	}
}

// Recipient mode: X25519 and post-quantum hybrid, repeatable flags and a
// recipients file, identity files on restore, streaming to stdout refused
// with --verify.
func TestRecipientBackupRestore(t *testing.T) {
	t.Setenv(passphraseEnv, "")
	base := t.TempDir()
	dir := filepath.Join(base, "hub")
	if code, _, errs := runCmd(t, "init", "--data", dir); code != 0 {
		t.Fatal(errs)
	}
	x1, _ := age.GenerateX25519Identity()
	x2, _ := age.GenerateX25519Identity()
	h1, _ := age.GenerateHybridIdentity()
	h2, _ := age.GenerateHybridIdentity()
	write := func(name, s string) string {
		p := filepath.Join(base, name)
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	rfile := write("recipients.txt", "# synthetic\n"+x2.Recipient().String()+"\n")
	k1 := write("x1.key", x1.String()+"\n")
	k2 := write("x2.key", x2.String()+"\n")
	kh2 := write("h2.key", "# synthetic pq key\n"+h2.String()+"\n")
	other, _ := age.GenerateX25519Identity()
	kother := write("other.key", other.String()+"\n")

	arch := filepath.Join(base, "x.tar.age")
	if code, out, errs := runCmd(t, "backup", "--data", dir, "--out", arch, "--recipient", x1.Recipient().String(), "--recipients-file", rfile, "--verify"); code != 0 || !strings.Contains(out, "2 recipient") || !strings.Contains(out, "verified") {
		t.Fatalf("x25519 backup: %d %s %s", code, out, errs)
	}
	isAgeFile(t, arch)
	for i, k := range []string{k1, k2} {
		target := filepath.Join(base, "rx", string(rune('a'+i)))
		if code, out, errs := runCmd(t, "restore", "--in", arch, "--data", target, "--identity", k); code != 0 {
			t.Fatalf("restore with %s: %d %s %s", k, code, out, errs)
		}
	}
	if code, _, errs := runCmd(t, "restore", "--in", arch, "--data", filepath.Join(base, "rx", "no")); code == 0 || !strings.Contains(errs, "--identity") || !strings.Contains(errs, "X25519") {
		t.Fatalf("restore without identity: %d %s", code, errs)
	}
	if code, _, errs := runCmd(t, "restore", "--in", arch, "--data", filepath.Join(base, "rx", "bad"), "--identity", kother); code == 0 || !strings.Contains(errs, "none of the supplied keys") {
		t.Fatalf("restore with wrong identity: %d %s", code, errs)
	}

	pq := filepath.Join(base, "pq.tar.age")
	if code, out, errs := runCmd(t, "backup", "--data", dir, "--out", pq, "--recipient", h1.Recipient().String(), "--recipient", h2.Recipient().String()); code != 0 || !strings.Contains(out, "post-quantum") {
		t.Fatalf("hybrid backup: %d %s %s", code, out, errs)
	}
	if code, out, errs := runCmd(t, "restore", "--in", pq, "--data", filepath.Join(base, "rpq"), "--identity", kh2); code != 0 {
		t.Fatalf("hybrid restore: %d %s %s", code, out, errs)
	}
	if code, _, errs := runCmd(t, "backup", "--data", dir, "--out", filepath.Join(base, "mixed.tar.age"), "--recipient", h1.Recipient().String(), "--recipient", x1.Recipient().String()); code == 0 || !strings.Contains(errs, "post-quantum") {
		t.Fatalf("mixed recipients accepted: %d %s", code, errs)
	}
	if code, _, errs := runCmd(t, "backup", "--data", dir, "--out", "-", "--recipient", x1.Recipient().String(), "--verify"); code != 2 || !strings.Contains(errs, "--verify") {
		t.Fatalf("--verify with stdout: %d %s", code, errs)
	}
	if code, _, errs := runCmd(t, "backup", "--data", dir, "--out", filepath.Join(base, "bad.tar.age"), "--recipient", x1.String()); code != 2 || !strings.Contains(errs, "PRIVATE") {
		t.Fatalf("private key as recipient: %d %s", code, errs)
	}
}

// A20 soak: stopping the hub took the full 10 s shutdown timeout (fleet
// scripts SIGKILLed it at 5 s) because http.Server.Shutdown waited for
// in-flight long-polls (inbox waits of up to 60 s, e.g. the Claude
// doorbell's). Shutdown now cancels in-flight requests after a short grace.
func TestServeStopsPromptlyWithLongPollInFlight(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	if code, _, errs := runCmd(t, "init", "--data", dir); code != 0 {
		t.Fatal(errs)
	}
	cred := readCred(t, dir)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() { done <- run(ctx, []string{"serve", "--data", dir, "--listen", addr}, io.Discard, &errb) }()
	cl := client.New("https://"+addr, cred.Token, client.Options{CertSHA256: cred.HubCertSHA256, MaxAttempts: 1, Timeout: 90 * time.Second})
	var err error
	for i := 0; i < 100; i++ {
		if _, err = cl.WhoAmI(context.Background()); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("hub not up: %v (%s)", err, errb.String())
	}
	polled := make(chan struct{})
	go func() {
		close(polled)
		_, _ = cl.ListMessages(context.Background(), contracts.ListMessagesRequest{WaitSeconds: 60})
	}()
	<-polled
	time.Sleep(300 * time.Millisecond) // the long-poll is now waiting in the hub
	t0 := time.Now()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exit %d: %s", code, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
	if d := time.Since(t0); d > 4*time.Second {
		t.Fatalf("serve took %v to stop with a long-poll in flight (want < 4s)", d)
	}
}

func TestInitNextStepsUseHubURLPortAndCLICommands(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hub")
	code, out, errs := runCmd(t, "init", "--data", dir, "--hub-url", "https://127.0.0.1:18760", "--tls-host", "127.0.0.1")
	if code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	cred := readCred(t, dir)
	for _, want := range []string{
		"--listen :18760 --hub-url https://127.0.0.1:18760",
		"pyre login --hub https://127.0.0.1:18760 --pin " + cred.HubCertSHA256 + " --token -",
		"pyre pair-client --name", "pyre enroll-node --name",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("next steps lack %q:\n%s", want, out)
		}
	}
	for _, stale := range []string{":7443", "clients.pair operation", "nodes.enroll operation"} {
		if strings.Contains(out, stale) {
			t.Errorf("next steps still say %q", stale)
		}
	}
	for in, want := range map[string]string{"": ":7443", "https://h.invalid": ":443", "http://h.invalid": ":80", "https://[fd00::1]:9443/": ":9443"} {
		if got := listenFromURL(in); got != want {
			t.Errorf("listenFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}
