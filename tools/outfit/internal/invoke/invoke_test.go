package invoke

import (
	"context"
	"testing"

	"github.com/jhh3/outfit/internal/kit"
	"github.com/jhh3/outfit/internal/runtime"
)

func TestRunAutoPaginatesStandardCursorWithinBounds(t *testing.T) {
	operation := kit.NormalizeOperation(kit.Operation{
		Name: "list_items", Class: "read",
		Input: map[string]any{"properties": map[string]any{"cursor": map[string]any{"type": "string"}}},
		HTTP:  &kit.HTTPOperation{Method: "GET", Path: "/items", Query: []string{"cursor"}},
	})
	calls := 0
	envelope, err := Run(context.Background(), Metadata{Connection: "acme", Connector: "acme", Mode: "rest"}, operation, nil, Options{All: true, MaxPages: 5, MaxItems: 10}, func(_ context.Context, arguments map[string]string) (runtime.Result, error) {
		calls++
		if calls == 1 {
			if arguments["cursor"] != "" {
				t.Fatalf("first cursor = %q", arguments["cursor"])
			}
			return runtime.Result{Status: 200, Data: map[string]any{"items": []any{"a", "b"}, "next_cursor": "next"}}, nil
		}
		if arguments["cursor"] != "next" {
			t.Fatalf("second cursor = %q", arguments["cursor"])
		}
		return runtime.Result{Status: 200, Data: map[string]any{"items": []any{"c"}}}, nil
	})
	if err != nil || !envelope.OK || envelope.Meta.Pages != 2 || envelope.Meta.Items != 3 || calls != 2 {
		t.Fatalf("envelope=%#v calls=%d err=%v", envelope, calls, err)
	}
}

func TestRunStopsAndReportsRepeatedCursor(t *testing.T) {
	operation := kit.NormalizeOperation(kit.Operation{Name: "list_items", Class: "read", Input: map[string]any{"properties": map[string]any{"cursor": map[string]any{"type": "string"}}}, HTTP: &kit.HTTPOperation{Method: "GET", Path: "/items", Query: []string{"cursor"}}})
	envelope, err := Run(context.Background(), Metadata{}, operation, nil, Options{All: true}, func(_ context.Context, _ map[string]string) (runtime.Result, error) {
		return runtime.Result{Status: 200, Data: map[string]any{"next_cursor": "same"}}, nil
	})
	if err == nil || envelope.Error == nil || envelope.Error.Code != "pagination_stalled" || envelope.Meta.Pages != 2 {
		t.Fatalf("envelope=%#v err=%v", envelope, err)
	}
}

func TestFriendlyOperationNameSupportsConnectorPrefixedTools(t *testing.T) {
	if got := FriendlyOperationName("slack", "slack_read_thread"); got != "read-thread" {
		t.Fatalf("friendly name = %q", got)
	}
	if got := FriendlyOperationName("github", "get_pull_request"); got != "get-pull-request" {
		t.Fatalf("friendly name = %q", got)
	}
}
