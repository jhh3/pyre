package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/buildinfo"
	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
)

func TestVersionJSON(t *testing.T) {
	code, out, _ := runCmd(t, "version", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var v struct {
		Version  string `json:"version"`
		Protocol int    `json:"protocol"`
		Min      int    `json:"min_protocol"`
		Go       string `json:"go"`
		OS       string `json:"os"`
		Arch     string `json:"arch"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Version != buildinfo.Version || v.Protocol != contracts.ProtocolVersion || v.Min != contracts.MinProtocolVersion || v.Go == "" || v.OS == "" {
		t.Fatalf("version --json: %q %v", out, err)
	}
	if code, out, _ := runCmd(t, "version"); code != 0 || !strings.Contains(out, "pyre-hub ") {
		t.Fatalf("version: %d %q", code, out)
	}
}

// synthServiceAccount writes an obviously synthetic Google service account.
func synthServiceAccount(t *testing.T, dir string) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	b, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "synthetic-project", "client_email": "synthetic@synthetic-project.iam.gserviceaccount.invalid",
		"private_key": string(pemKey), "token_uri": "https://oauth2.synthetic.invalid/token"})
	p := filepath.Join(dir, "synthetic-fcm.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// serve --fcm-service-account enables push registration (FCM configured);
// --log-file writes a size-rotated log instead of stderr.
func TestServeFCMAndLogFile(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "hub")
	if code, _, errs := runCmd(t, "init", "--data", dir); code != 0 {
		t.Fatal(errs)
	}
	// A broken service account fails fast, before serving.
	bad := filepath.Join(base, "bad.json")
	os.WriteFile(bad, []byte(`{"type":"user"}`), 0o600)
	if code, _, errs := runCmd(t, "serve", "--data", dir, "--listen", freeAddr(t), "--fcm-service-account", bad); code == 0 || !strings.Contains(errs, "fcm") {
		t.Fatalf("bad service account: %d %s", code, errs)
	}
	cred := readCred(t, dir)
	addr := freeAddr(t)
	logPath := filepath.Join(base, "logs", "hub.log")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var errb bytes.Buffer
	go func() {
		done <- run(ctx, []string{"serve", "--data", dir, "--listen", addr, "--fcm-service-account", synthServiceAccount(t, base),
			"--log-file", logPath, "--log-max-bytes", "4096"}, io.Discard, &errb)
	}()
	cl := client.New("https://"+addr, cred.Token, client.Options{CertSHA256: cred.HubCertSHA256, MaxAttempts: 1, Timeout: 2 * time.Second})
	var err error
	for i := 0; i < 100; i++ {
		if _, err = cl.Hello(context.Background(), contracts.HelloRequest{Protocol: contracts.ProtocolVersion}); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("hello: %v (%s)", err, errb.String())
	}
	res, err := cl.RegisterPush(context.Background(), contracts.PushRegistration{Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		Provider: "fcm", DeviceToken: "synthetic-device-token"})
	if err != nil || !res.Enabled {
		t.Fatalf("push registration with FCM configured: %+v %v", res, err)
	}
	for i := 0; i < 40; i++ { // enough requests to rotate a 4 KiB log
		_, _ = cl.WhoAmI(context.Background())
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
	b, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(b)+readOr(logPath+".1"), "pyre-hub") {
		t.Fatalf("log file: %v %q", err, b)
	}
	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("log not rotated: %v", err)
	}
	if st, _ := os.Stat(logPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("log mode %v", st.Mode().Perm())
	}
	if strings.Contains(string(b), cred.Token) || strings.Contains(string(b), "synthetic-device-token") {
		t.Fatal("secret in hub log")
	}
}

func readOr(p string) string { b, _ := os.ReadFile(p); return string(b) }
