package notifications

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

type fixedClock struct{ t time.Time }

func (c *fixedClock) Now() time.Time { return c.t }

// fakeGoogle is a fake OAuth2 token endpoint plus a fake FCM HTTP v1
// endpoint. It verifies the JWT assertion against the service account's
// public key and the bearer token on sends.
type fakeGoogle struct {
	t   *testing.T
	pub *rsa.PublicKey
	srv *httptest.Server

	mu         sync.Mutex
	tokens     int
	bodies     [][]byte
	paths      []string
	sendStatus []int // per-send status queue; empty = 200
	sendBodies []string
	retryAfter string
	expireAll  bool // respond 401 once to force a token refresh
}

func newFakeGoogle(t *testing.T, pub *rsa.PublicKey) *fakeGoogle {
	f := &fakeGoogle{t: t, pub: pub}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", f.token)
	mux.HandleFunc("/v1/projects/", f.send)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGoogle) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	if r.Form.Get("grant_type") != jwtBearerGrant {
		f.t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
	}
	parts := strings.Split(r.Form.Get("assertion"), ".")
	if len(parts) != 3 {
		http.Error(w, `{"error":"invalid_grant"}`, 400)
		return
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(f.pub, crypto.SHA256, sum[:], sig); err != nil {
		f.t.Errorf("JWT signature invalid: %v", err)
		http.Error(w, `{"error":"invalid_grant"}`, 400)
		return
	}
	var hdr map[string]string
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(hb, &hdr)
	if hdr["alg"] != "RS256" || hdr["kid"] != "synthetic-key-id" {
		f.t.Errorf("JWT header = %v", hdr)
	}
	var claims map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(cb, &claims)
	if claims["iss"] != "pyre-synthetic@example.invalid" || claims["scope"] != fcmScope || claims["aud"] != f.srv.URL+"/token" {
		f.t.Errorf("JWT claims = %v", claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat != 3600 {
		f.t.Errorf("JWT lifetime = %v", exp-iat)
	}
	f.mu.Lock()
	f.tokens++
	n := f.tokens
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-access-" + string(rune('0'+n)), "expires_in": 3600, "token_type": "Bearer"})
}

func (f *fakeGoogle) send(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	want := "Bearer synthetic-access-" + string(rune('0'+f.tokens))
	if f.expireAll {
		f.expireAll = false
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`)
		return
	}
	if r.Header.Get("Authorization") != want {
		f.t.Errorf("authorization = %q want %q", r.Header.Get("Authorization"), want)
	}
	f.bodies = append(f.bodies, b)
	f.paths = append(f.paths, r.URL.Path)
	if len(f.sendStatus) > 0 {
		st, body := f.sendStatus[0], f.sendBodies[0]
		f.sendStatus, f.sendBodies = f.sendStatus[1:], f.sendBodies[1:]
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
		return
	}
	_, _ = io.WriteString(w, `{"name":"projects/pyre-synthetic/messages/1"}`)
}

func (f *fakeGoogle) queue(status int, body string) {
	f.mu.Lock()
	f.sendStatus = append(f.sendStatus, status)
	f.sendBodies = append(f.sendBodies, body)
	f.mu.Unlock()
}

// synthServiceAccount writes an obviously synthetic service-account file.
func synthServiceAccount(t *testing.T, tokenURI string) (string, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	sa := map[string]string{
		"type": "service_account", "project_id": "pyre-synthetic", "private_key_id": "synthetic-key-id",
		"private_key": string(pemKey), "client_email": "pyre-synthetic@example.invalid", "token_uri": tokenURI,
	}
	b, _ := json.Marshal(sa)
	path := filepath.Join(t.TempDir(), "synthetic-service-account.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, key
}

func newTestFCM(t *testing.T) (*FCM, *fakeGoogle, *fixedClock) {
	t.Helper()
	path, key := synthServiceAccount(t, "http://placeholder.invalid/token")
	g := newFakeGoogle(t, &key.PublicKey)
	clk := &fixedClock{t: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}
	f, err := NewFCM(FCMConfig{ServiceAccountFile: path, Endpoint: g.srv.URL, TokenURL: g.srv.URL + "/token", Clock: clk, HTTPClient: g.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return f, g, clk
}

var testMsg = Message{AttentionID: "att_synth-1", Kind: contracts.AttnApproval, Severity: "urgent", DeepLink: "pyre://approval/apr_synth-1",
	GroupKey: "approval:apr_synth-1", Title: GenericTitle(contracts.AttnApproval), Action: ActionNotify}

func TestFCMSendPayloadIsMinimalDataOnly(t *testing.T) {
	f, g, _ := newTestFCM(t)
	res := f.Deliver(context.Background(), testMsg, []Target{{Provider: ProviderFCM, ClientID: "cli_synth", DeviceToken: "synthetic-device-token"}})
	if len(res) != 1 || res[0].Err != nil {
		t.Fatalf("deliver: %+v", res)
	}
	if g.paths[0] != "/v1/projects/pyre-synthetic/messages:send" {
		t.Fatalf("path = %s", g.paths[0])
	}
	var body struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(g.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	for k := range body.Message {
		if k != "token" && k != "data" && k != "android" {
			t.Errorf("unexpected message field %q (must be data-only, no notification block)", k)
		}
	}
	var data map[string]string
	_ = json.Unmarshal(body.Message["data"], &data)
	want := map[string]string{"attention_id": "att_synth-1", "kind": "approval", "severity": "urgent", "deep_link": "pyre://approval/apr_synth-1",
		"group_key": "approval:apr_synth-1", "title": "Pyre: approval needed", "action": "notify"}
	if len(data) != len(want) {
		t.Fatalf("data keys = %v", data)
	}
	for k, v := range want {
		if data[k] != v {
			t.Errorf("data[%s] = %q want %q", k, data[k], v)
		}
	}
	var android fcmAndroid
	_ = json.Unmarshal(body.Message["android"], &android)
	if android.Priority != "HIGH" || android.CollapseKey != "approval:apr_synth-1" || android.TTL != "3600s" {
		t.Fatalf("android = %+v", android)
	}
}

func TestFCMTokenCachingAndRefresh(t *testing.T) {
	f, g, clk := newTestFCM(t)
	tgt := []Target{{DeviceToken: "synthetic-device-token"}}
	for i := 0; i < 3; i++ {
		if r := f.Deliver(context.Background(), testMsg, tgt); r[0].Err != nil {
			t.Fatal(r[0].Err)
		}
	}
	if f.TokenFetches() != 1 {
		t.Fatalf("token fetched %d times; want cached", f.TokenFetches())
	}
	clk.t = clk.t.Add(59*time.Minute + 30*time.Second) // inside the 1-minute refresh margin
	if r := f.Deliver(context.Background(), testMsg, tgt); r[0].Err != nil {
		t.Fatal(r[0].Err)
	}
	if f.TokenFetches() != 2 {
		t.Fatalf("expired token not refreshed: %d", f.TokenFetches())
	}
	// A 401 invalidates the cached token and retries once.
	g.mu.Lock()
	g.expireAll = true
	g.mu.Unlock()
	if r := f.Deliver(context.Background(), testMsg, tgt); r[0].Err != nil {
		t.Fatalf("401 refresh: %v", r[0].Err)
	}
	if f.TokenFetches() != 3 {
		t.Fatalf("401 did not refresh: %d", f.TokenFetches())
	}
}

func TestFCMErrorClassification(t *testing.T) {
	f, g, _ := newTestFCM(t)
	tgt := []Target{{DeviceToken: "synthetic-device-token"}}
	g.queue(404, `{"error":{"code":404,"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`)
	if r := f.Deliver(context.Background(), testMsg, tgt)[0]; !r.Unregistered || r.Retryable {
		t.Fatalf("unregistered: %+v", r)
	}
	g.queue(403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`)
	if r := f.Deliver(context.Background(), testMsg, tgt)[0]; !r.Unregistered {
		t.Fatalf("sender mismatch: %+v", r)
	}
	g.retryAfter = "7"
	g.queue(503, `{"error":{"code":503,"status":"UNAVAILABLE","details":[{"errorCode":"UNAVAILABLE"}]}}`)
	if r := f.Deliver(context.Background(), testMsg, tgt)[0]; !r.Retryable || r.RetryAfter != 7*time.Second || r.Unregistered {
		t.Fatalf("unavailable: %+v", r)
	}
	g.retryAfter = ""
	g.queue(400, `{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"errorCode":"INVALID_ARGUMENT"}]}}`)
	r := f.Deliver(context.Background(), testMsg, tgt)[0]
	if r.Err == nil || r.Retryable || r.Unregistered {
		t.Fatalf("invalid argument: %+v", r)
	}
	if strings.Contains(r.Err.Error(), "synthetic-device-token") || strings.Contains(r.Err.Error(), "synthetic-access") {
		t.Fatalf("error leaks token: %v", r.Err)
	}
}

func TestFCMConfigValidation(t *testing.T) {
	path, _ := synthServiceAccount(t, "https://oauth2.example.invalid/token")
	if _, err := NewFCM(FCMConfig{ServiceAccountFile: path}); err != nil {
		t.Fatalf("valid config: %v", err)
	}
	if _, err := NewFCM(FCMConfig{ServiceAccountFile: path, Endpoint: "http://fcm.example.invalid"}); err == nil {
		t.Fatal("plain-http non-loopback endpoint accepted")
	}
	if _, err := NewFCM(FCMConfig{}); err == nil {
		t.Fatal("missing service account accepted")
	}
	if _, err := NewFCM(FCMConfig{ServiceAccountJSON: []byte(`{"type":"authorized_user"}`)}); err == nil {
		t.Fatal("non service-account accepted")
	}
	if _, err := NewFCM(FCMConfig{ServiceAccountJSON: []byte(`{"type":"service_account","project_id":"p","client_email":"e","private_key":"nope"}`)}); err == nil {
		t.Fatal("bad key accepted")
	}
}

func TestFCMTokenEndpointFailureIsRetryable(t *testing.T) {
	path, _ := synthServiceAccount(t, "http://127.0.0.1:1/token") // nothing listens on port 1
	f, err := NewFCM(FCMConfig{ServiceAccountFile: path, Endpoint: "http://127.0.0.1:1", HTTPClient: &http.Client{Timeout: 2 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	r := f.Deliver(context.Background(), testMsg, []Target{{DeviceToken: "synthetic-device-token"}})[0]
	if r.Err == nil || !r.Retryable {
		t.Fatalf("unreachable token endpoint: %+v", r)
	}
}
