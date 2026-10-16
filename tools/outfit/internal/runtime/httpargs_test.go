package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/outfit/internal/kit"
)

func TestRawPathOnlyNamesSegmentsBelowTheBase(t *testing.T) {
	base, _ := url.Parse("https://api.example.test/api/v10/")
	accepted := map[string]string{
		"/users/@me": "/users/@me",
		"/channels/1/messages/2/reactions/👍/@me":        "/channels/1/messages/2/reactions/%F0%9F%91%8D/@me",
		"/channels/1/messages/2/reactions/%F0%9F%91%8D": "/channels/1/messages/2/reactions/%F0%9F%91%8D",
		"/emoji/name:123": "/emoji/name:123",
	}
	for raw, want := range accepted {
		if got, err := rawPath(base, raw); err != nil || got != want {
			t.Errorf("rawPath(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"users", "//host/x", "http://host/x", "/a/../b", "/a/./b", "/a/%2E%2e", "/a/b%2Fc", "/a/b%5Cc", "/a?b=1", "/a#b", "/a\\b", "/a/", "/a b", "/a/%0a", "/api/v10/users", "/a/%zz"} {
		if got, err := rawPath(base, raw); err == nil {
			t.Errorf("rawPath(%q) = %q, want rejection", raw, got)
		}
	}
}

func TestRawPathOperationKeepsOriginAndAppliesMethodQueryBodyAndHeader(t *testing.T) {
	var seen *http.Request
	var body string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen, body = r, string(raw)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	mode := kit.Mode{Executor: kit.Executor{Kind: "http", BaseURL: server.URL + "/v1/", PrivateNet: true}}
	operation := kit.NormalizeOperation(kit.Operation{Name: "call", Class: "destructive", HTTP: &kit.HTTPOperation{
		Path: "/", MethodArgument: "method", Methods: []string{"POST", "DELETE"}, PathArgument: "path", QueryArgument: "query", BodyArgument: "body",
		ArgumentHeaders: []kit.ArgumentHeader{{Name: "X-Reason", Argument: "reason", Encoding: "url"}},
	}})
	arguments := map[string]string{"method": "post", "path": "/a/@b", "query": `{"tag":["x","y"],"n":12345678901234567890}`, "body": `[1,{"id":"2"}]`, "reason": "a b/c"}
	if _, err := (Executor{HTTPClient: server.Client()}).Run(context.Background(), mode, operation, arguments, nil); err != nil {
		t.Fatal(err)
	}
	if seen.Method != http.MethodPost || seen.URL.EscapedPath() != "/v1/a/@b" || seen.URL.RawQuery != "n=12345678901234567890&tag=x&tag=y" || seen.Header.Get("X-Reason") != "a%20b%2Fc" || body != `[1,{"id":"2"}]` {
		t.Fatalf("request = %s %s?%s reason=%q body=%s", seen.Method, seen.URL.EscapedPath(), seen.URL.RawQuery, seen.Header.Get("X-Reason"), body)
	}
	for _, bad := range []map[string]string{{"method": "PATCH", "path": "/a"}, {"method": "POST", "path": "/a", "query": `{"x":{"y":1}}`}, {"method": "POST", "path": "/a", "body": "nope"}} {
		if _, err := (Executor{}).Plan(mode, operation, bad); err == nil {
			t.Errorf("plan accepted %v", bad)
		}
	}
}

func TestRelativeTimeAcceptsMinutesDurationsTimestampsAndClear(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for raw, want := range map[string]string{
		"60": "2026-10-09T13:00:00Z", "90m": "2026-10-09T13:30:00Z", "7d": "2026-10-16T12:00:00Z",
		"2026-10-10T08:00:00-07:00": "2026-10-10T15:00:00Z", "0": "null", "none": "null",
	} {
		if got, err := relativeTime(raw, now); err != nil || got != want {
			t.Errorf("relativeTime(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"soon", "-5", "-1h"} {
		if _, err := relativeTime(raw, now); err == nil {
			t.Errorf("relativeTime(%q) accepted", raw)
		}
	}
}

func TestItemTemplatesWrapPlainStringItems(t *testing.T) {
	operation := kit.Operation{HTTP: &kit.HTTPOperation{Method: "POST", Path: "/m", BodyValues: map[string]any{"poll": map[string]any{"answers": "{{arg.answers}}"}}, ItemTemplates: map[string]any{"answers": map[string]any{"poll_media": map[string]any{"text": "{{item}}"}}}}}
	payload := httpPayload(operation, map[string]string{"answers": `["A",{"poll_media":{"text":"B"}}]`}, nil)
	raw, _ := json.Marshal(payload)
	encoded := strings.TrimSpace(string(raw))
	if encoded != `{"poll":{"answers":[{"poll_media":{"text":"A"}},{"poll_media":{"text":"B"}}]}}` {
		t.Fatalf("payload = %s", encoded)
	}
}
