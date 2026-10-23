package connection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/jhh3/outfit/internal/builtin"
	"github.com/jhh3/outfit/internal/kit"
	cwruntime "github.com/jhh3/outfit/internal/runtime"
	"github.com/jhh3/outfit/internal/state"
	"github.com/jhh3/outfit/internal/vault"
)

// Obviously synthetic keys: they match Stripe's key shapes and nothing else.
const (
	syntheticTestKey = "rk_test_SYNTHETICOUTFITTESTKEY0000"
	syntheticLiveKey = "rk_" + "live_SYNTHETICOUTFITLIVEKEY0000"
)

func builtinStripe(t *testing.T) kit.Package {
	t.Helper()
	packages, err := builtin.All()
	if err != nil {
		t.Fatal(err)
	}
	for _, loaded := range packages {
		if loaded.Package.Key == "stripe" {
			return loaded.Package
		}
	}
	t.Fatal("built-in stripe package not found")
	return kit.Package{}
}

func stripeTestService(t *testing.T, pkg kit.Package, executor cwruntime.Executor, resolve func(kit.ToolRequirement) (string, error)) Service {
	t.Helper()
	t.Setenv("OUTFIT_VAULT_BACKEND", "file")
	ctx := context.Background()
	root := t.TempDir()
	paths := state.Paths{Root: root, Database: root + "/outfit.db", Packages: root + "/packages", Skills: root + "/skills", Locks: root + "/locks", Vault: root + "/vault"}
	store, err := state.Open(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	raw, _ := json.Marshal(pkg)
	if err := store.InstallPackage(ctx, kit.Loaded{Package: pkg, Raw: raw}, "trusted"); err != nil {
		t.Fatal(err)
	}
	secret, err := vault.Open(paths)
	if err != nil {
		t.Fatal(err)
	}
	return Service{State: store, Vault: secret, Executor: executor, ResolveTool: resolve}
}

// writeFakeStripeCLI writes a stand-in for the pinned Stripe CLI that, like
// the real one, prints Stripe's error object and exits 0 for a missing
// object. It reports which key reached it (test, live, or none) and whether
// --live was passed, and logs each call.
func writeFakeStripeCLI(t *testing.T) (binary, log string) {
	t.Helper()
	directory := t.TempDir()
	binary, log = filepath.Join(directory, "stripe"), filepath.Join(directory, "calls.log")
	body := `#!/bin/sh
key=none
case "$STRIPE_API_KEY" in rk_test_*) key=test;; rk_live_*) key=live;; esac
live=false
for arg in "$@"; do [ "$arg" = "--live" ] && live=true; done
echo "$key $*" >> '` + log + `'
case "$*" in *sub_missing*) printf '{"error":{"code":"resource_missing","doc_url":"https://stripe.com/docs/error-codes/resource-missing","message":"No such subscription: '"'"'sub_missing'"'"'","param":"id","type":"invalid_request_error"}}'; exit 0;; esac
printf '{"id":"acct_SYNTHETIC","object":"account","key":"%s","live_flag":%s}' "$key" "$live"
`
	if err := os.WriteFile(binary, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary, log
}

func readCalls(t *testing.T, log string) []string {
	t.Helper()
	raw, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestStripeCLILiveReadsUseTheLiveKeyWithoutWrite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper")
	}
	t.Setenv("STRIPE_API_KEY", "")
	binary, log := writeFakeStripeCLI(t)
	service := stripeTestService(t, builtinStripe(t), cwruntime.Executor{}, func(kit.ToolRequirement) (string, error) { return binary, nil })
	ctx := context.Background()
	if _, err := service.Configure(ctx, "stripe", "stripe", "cli", map[string]string{"STRIPE_API_KEY": syntheticTestKey, "STRIPE_LIVE_API_KEY": syntheticLiveKey}); err != nil {
		t.Fatal(err)
	}
	// Setup verifies the default key and then the stored live key.
	if calls := readCalls(t, log); strings.Join(calls, "|") != "test get /v1/account|live get /v1/account --live" {
		t.Fatalf("verification calls = %q", calls)
	}

	read := func(arguments map[string]string) map[string]any {
		t.Helper()
		result, err := service.Run(ctx, "stripe", "subscriptions_get", arguments, false)
		if err != nil {
			t.Fatalf("subscriptions_get %v: %v", arguments, err)
		}
		data, _ := result.Data.(map[string]any)
		return data
	}
	if data := read(map[string]string{"subscription_id": "sub_SYNTHETIC1", "live": "true"}); data["key"] != "live" || data["live_flag"] != true {
		t.Fatalf("live read = %#v", data)
	}
	if data := read(map[string]string{"subscription_id": "sub_SYNTHETIC1"}); data["key"] != "test" || data["live_flag"] != false {
		t.Fatalf("default read = %#v", data)
	}
	if data := read(map[string]string{"subscription_id": "sub_SYNTHETIC1", "live": "false"}); data["key"] != "test" || data["live_flag"] != false {
		t.Fatalf("explicit test read = %#v", data)
	}

	// A typed mutation still needs --write in live mode.
	if _, err := service.Run(ctx, "stripe", "subscriptions_cancel", map[string]string{"subscription_id": "sub_SYNTHETIC1"}, false); err == nil {
		t.Fatal("destructive typed operation ran without --write")
	}

	// Raw reads run without --write and pick the key from --live.
	for _, argv := range [][]string{{"subscriptions", "retrieve", "sub_SYNTHETIC1", "--live"}, {"get", "/v1/subscriptions/sub_SYNTHETIC1", "--live"}} {
		result, err := service.ExecCLI(ctx, "stripe", argv, false)
		if err != nil {
			t.Fatalf("exec %q: %v", argv, err)
		}
		if !strings.Contains(string(result.Raw), `"key":"live"`) {
			t.Fatalf("exec %q used %s", argv, result.Raw)
		}
	}
	if result, err := service.ExecCLI(ctx, "stripe", []string{"get", "/v1/subscriptions/sub_SYNTHETIC1"}, false); err != nil || !strings.Contains(string(result.Raw), `"key":"test"`) {
		t.Fatalf("test-mode exec = %s, %v", result.Raw, err)
	}
	before := len(readCalls(t, log))
	for _, argv := range [][]string{{"post", "/v1/subscriptions/sub_SYNTHETIC1", "-d", "cancel_at_period_end=true", "--live"}, {"subscriptions", "cancel", "sub_SYNTHETIC1", "--live"}, {"trigger", "payment_intent.succeeded"}} {
		if _, err := service.ExecCLI(ctx, "stripe", argv, false); err == nil || !strings.Contains(err.Error(), "may mutate") {
			t.Fatalf("exec %q without --write: %v", argv, err)
		}
	}
	if after := len(readCalls(t, log)); after != before {
		t.Fatalf("a refused mutation reached the CLI (%d calls)", after-before)
	}
}

func TestStripeCLIErrorObjectIsAFailedCall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper")
	}
	binary, _ := writeFakeStripeCLI(t)
	service := stripeTestService(t, builtinStripe(t), cwruntime.Executor{}, func(kit.ToolRequirement) (string, error) { return binary, nil })
	ctx := context.Background()
	if _, err := service.Configure(ctx, "stripe", "stripe", "cli", map[string]string{"STRIPE_API_KEY": syntheticTestKey}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(ctx, "stripe", "subscriptions_get", map[string]string{"subscription_id": "sub_missing"}, false)
	var failure *cwruntime.ProviderError
	if !errors.As(err, &failure) || failure.Status != http.StatusNotFound || failure.Code != "resource_missing" || result.Status != http.StatusNotFound {
		t.Fatalf("missing subscription = %#v status %d, %v", failure, result.Status, err)
	}
	if err.Error() != "upstream returned 404 Not Found: No such subscription: 'sub_missing' (resource_missing)" {
		t.Fatalf("error text = %q", err)
	}
	if _, err := service.ExecCLI(ctx, "stripe", []string{"get", "/v1/subscriptions/sub_missing"}, false); !errors.As(err, &failure) || failure.Code != "resource_missing" {
		t.Fatalf("raw missing subscription = %v", err)
	}
}

func TestStripeLiveReadWithoutALiveKeyFailsInsteadOfReadingTestData(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper")
	}
	binary, log := writeFakeStripeCLI(t)
	service := stripeTestService(t, builtinStripe(t), cwruntime.Executor{}, func(kit.ToolRequirement) (string, error) { return binary, nil })
	ctx := context.Background()
	if _, err := service.Configure(ctx, "stripe", "stripe", "cli", map[string]string{"STRIPE_API_KEY": syntheticTestKey}); err != nil {
		t.Fatal(err)
	}
	before := len(readCalls(t, log))
	_, err := service.Run(ctx, "stripe", "subscriptions_get", map[string]string{"subscription_id": "sub_SYNTHETIC1", "live": "true"}, false)
	if err == nil || !strings.Contains(err.Error(), "STRIPE_LIVE_API_KEY") {
		t.Fatalf("live read without a live key = %v", err)
	}
	if _, err := service.ExecCLI(ctx, "stripe", []string{"get", "/v1/account", "--live"}, false); err == nil || !strings.Contains(err.Error(), "STRIPE_LIVE_API_KEY") {
		t.Fatalf("raw live read without a live key = %v", err)
	}
	if after := len(readCalls(t, log)); after != before {
		t.Fatalf("a live read without a live key reached the CLI (%d calls)", after-before)
	}
}

func TestStripeNativeLoginLiveReadPassesLiveFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell helper")
	}
	t.Setenv("STRIPE_API_KEY", "")
	binary, _ := writeFakeStripeCLI(t)
	service := stripeTestService(t, builtinStripe(t), cwruntime.Executor{}, func(kit.ToolRequirement) (string, error) { return binary, nil })
	ctx := context.Background()
	// Native login keeps its keys in the Stripe CLI profile; --live makes
	// the CLI pick the profile's live key.
	if _, err := service.ConfigureWithMethod(ctx, "stripe", "stripe", "cli", "native-login", map[string]string{}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Run(ctx, "stripe", "customers_get", map[string]string{"customer_id": "cus_SYNTHETIC1", "live": "true"}, false)
	data, _ := result.Data.(map[string]any)
	if err != nil || data["live_flag"] != true || data["key"] != "none" {
		t.Fatalf("native live read = %#v, %v", data, err)
	}
}

func TestStripeRESTLiveReadsAndErrors(t *testing.T) {
	var mutex sync.Mutex
	var seen []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mutex.Lock()
		seen = append(seen, auth[:8]+" "+r.URL.Path)
		mutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case !strings.Contains(auth, "SYNTHETIC"):
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid API Key provided","type":"invalid_request_error"}}`)
		case strings.HasSuffix(r.URL.Path, "/sub_missing"):
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"code":"resource_missing","message":"No such subscription: 'sub_missing'","param":"id","type":"invalid_request_error"}}`)
		default:
			fmt.Fprintf(w, `{"id":"acct_SYNTHETIC","object":"account","path":%q}`, r.URL.Path)
		}
	}))
	defer server.Close()
	pkg := builtinStripe(t)
	for index := range pkg.Modes {
		if pkg.Modes[index].ID == "rest" {
			pkg.Modes[index].Executor.BaseURL = server.URL + "/v1/"
			pkg.Modes[index].Executor.PrivateNet = true
		}
	}
	service := stripeTestService(t, pkg, cwruntime.Executor{HTTPClient: server.Client()}, nil)
	ctx := context.Background()
	if _, err := service.Configure(ctx, "bad-live", "stripe", "rest", map[string]string{"STRIPE_API_KEY": syntheticTestKey, "STRIPE_LIVE_API_KEY": "rk_live_WRONGKEY"}); err == nil || !strings.Contains(err.Error(), "verify account_get with --live") {
		t.Fatalf("a rejected live key was accepted: %v", err)
	}
	if _, err := service.Configure(ctx, "stripe", "stripe", "rest", map[string]string{"STRIPE_API_KEY": syntheticTestKey, "STRIPE_LIVE_API_KEY": syntheticLiveKey}); err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	seen = nil
	mutex.Unlock()
	if _, err := service.Run(ctx, "stripe", "subscriptions_get", map[string]string{"subscription_id": "sub_SYNTHETIC1", "live": "true"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(ctx, "stripe", "subscriptions_get", map[string]string{"subscription_id": "sub_SYNTHETIC1"}, false); err != nil {
		t.Fatal(err)
	}
	_, err := service.Run(ctx, "stripe", "subscriptions_get", map[string]string{"subscription_id": "sub_missing", "live": "true"}, false)
	var failure *cwruntime.ProviderError
	if !errors.As(err, &failure) || failure.Status != http.StatusNotFound || failure.Code != "resource_missing" || err.Error() != "upstream returned 404 Not Found: No such subscription: 'sub_missing' (resource_missing)" {
		t.Fatalf("REST missing subscription = %v", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if got := strings.Join(seen, "|"); got != "rk_live_ /v1/subscriptions/sub_SYNTHETIC1|rk_test_ /v1/subscriptions/sub_SYNTHETIC1|rk_live_ /v1/subscriptions/sub_missing" {
		t.Fatalf("REST keys = %s", got)
	}
}
