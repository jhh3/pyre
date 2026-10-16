package linkcontext

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDiscordCreatePollBuildsPollBodyFromPlainAnswers(t *testing.T) {
	fake, service := newDiscordHarness(t)
	arguments := map[string]string{"channel": "234567890123456789", "question": "2026", "answers": `["Friday",{"poll_media":{"text":"Saturday","emoji":{"name":"✅"}}}]`, "duration": "48", "allow_multiselect": "true"}
	if _, err := service.Run(context.Background(), "vortex-discord", "create_poll", arguments, false); err == nil {
		t.Fatal("create_poll ran without explicit write intent")
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "create_poll", arguments, true); err != nil {
		t.Fatal(err)
	}
	request := fake.last(t)
	if request.Method != http.MethodPost || request.EscapedPath != "/channels/234567890123456789/messages" {
		t.Fatalf("create_poll request = %#v", request)
	}
	poll, _ := request.Body["poll"].(map[string]any)
	question, _ := poll["question"].(map[string]any)
	answers, _ := poll["answers"].([]any)
	if question["text"] != "2026" || poll["duration"] != json.Number("48") || poll["allow_multiselect"] != true || poll["layout_type"] != json.Number("1") || len(answers) != 2 {
		t.Fatalf("poll body = %#v", poll)
	}
	first, _ := answers[0].(map[string]any)
	media, _ := first["poll_media"].(map[string]any)
	second, _ := answers[1].(map[string]any)
	secondMedia, _ := second["poll_media"].(map[string]any)
	if media["text"] != "Friday" || secondMedia["text"] != "Saturday" || secondMedia["emoji"] == nil {
		t.Fatalf("poll answers = %#v", answers)
	}
	mentions, _ := request.Body["allowed_mentions"].(map[string]any)
	if parse, ok := mentions["parse"].([]any); !ok || len(parse) != 0 {
		t.Fatalf("create_poll allowed_mentions = %#v", request.Body)
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "create_poll", map[string]string{"channel": "234567890123456789", "question": "Q", "answers": `["A","B"]`}, true); err != nil {
		t.Fatal(err)
	}
	if poll, _ := fake.last(t).Body["poll"].(map[string]any); poll["duration"] != nil || poll["allow_multiselect"] != nil {
		t.Fatalf("omitted poll options must use Discord defaults: %#v", poll)
	}
}

func TestDiscordPollVotersEndPollAndResultsStayInMessage(t *testing.T) {
	fake, service := newDiscordHarness(t)
	if _, err := service.Run(context.Background(), "vortex-discord", "poll_voters", map[string]string{"channel": "234567890123456789", "message": "345678901234568000", "answer": "2", "limit": "100"}, false); err != nil {
		t.Fatal(err)
	}
	if request := fake.last(t); request.Method != http.MethodGet || request.EscapedPath != "/channels/234567890123456789/polls/345678901234568000/answers/2" || request.RawQuery != "limit=100" {
		t.Fatalf("poll_voters request = %#v", request)
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "end_poll", map[string]string{"channel": "234567890123456789", "message": "345678901234568000"}, false); err == nil {
		t.Fatal("end_poll ran without explicit write intent")
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "end_poll", map[string]string{"channel": "234567890123456789", "message": "345678901234568000"}, true); err != nil {
		t.Fatal(err)
	}
	if request := fake.last(t); request.Method != http.MethodPost || request.EscapedPath != "/channels/234567890123456789/polls/345678901234568000/expire" {
		t.Fatalf("end_poll request = %#v", request)
	}
	result, err := service.Run(context.Background(), "vortex-discord", "message", map[string]string{"channel": "234567890123456789", "message": "345678901234568000"}, false)
	if err != nil {
		t.Fatal(err)
	}
	message, _ := result.Data.(map[string]any)
	poll, _ := message["poll"].(map[string]any)
	results, _ := poll["results"].(map[string]any)
	counts, _ := results["answer_counts"].([]any)
	if results["is_finalized"] != true || len(counts) != 2 {
		t.Fatalf("message dropped poll results: %#v", message)
	}
}

func TestDiscordTimeoutSendsRelativeTimeAndEncodedReason(t *testing.T) {
	fake, service := newDiscordHarness(t)
	started := time.Now()
	arguments := map[string]string{"guild": "123456789012345678", "user": "456789012345678901", "until": "60", "reason": "Spam — repeated links: see #general"}
	if _, err := service.Run(context.Background(), "vortex-discord", "timeout_member", arguments, true); err != nil {
		t.Fatal(err)
	}
	request := fake.last(t)
	if request.Method != http.MethodPatch || request.EscapedPath != "/guilds/123456789012345678/members/456789012345678901" {
		t.Fatalf("timeout request = %#v", request)
	}
	if request.AuditReason != "Spam%20%E2%80%94%20repeated%20links:%20see%20%23general" {
		t.Fatalf("X-Audit-Log-Reason = %q", request.AuditReason)
	}
	until, err := time.Parse(time.RFC3339, request.Body["communication_disabled_until"].(string))
	if err != nil || until.Before(started.Add(59*time.Minute)) || until.After(time.Now().Add(61*time.Minute)) {
		t.Fatalf("communication_disabled_until = %#v (%v)", request.Body["communication_disabled_until"], err)
	}

	if _, err := service.Run(context.Background(), "vortex-discord", "timeout_member", map[string]string{"guild": "123456789012345678", "user": "456789012345678901", "until": "0"}, true); err != nil {
		t.Fatal(err)
	}
	request = fake.last(t)
	if value, present := request.Body["communication_disabled_until"]; !present || value != nil || request.HasReason {
		t.Fatalf("clearing timeout body = %#v reason=%v", request.Body, request.HasReason)
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "timeout_member", map[string]string{"guild": "123456789012345678", "user": "456789012345678901", "until": "soon"}, true); err == nil {
		t.Fatal("an unparseable timeout reached Discord")
	}
}

func TestDiscordAddMemberRoleUsesRolePath(t *testing.T) {
	fake, service := newDiscordHarness(t)
	if _, err := service.Run(context.Background(), "vortex-discord", "add_member_role", map[string]string{"guild": "123456789012345678", "user": "456789012345678901", "role": "567890123456789012", "reason": "Verified"}, true); err != nil {
		t.Fatal(err)
	}
	if request := fake.last(t); request.Method != http.MethodPut || request.EscapedPath != "/guilds/123456789012345678/members/456789012345678901/roles/567890123456789012" || request.AuditReason != "Verified" {
		t.Fatalf("add_member_role request = %#v", request)
	}
}

func TestDiscordAPIGetValidatesPathAndKeepsTheFixedOrigin(t *testing.T) {
	fake, service := newDiscordHarness(t)
	for _, path := range []string{
		"http://evil.example/api/v10/users/@me", "https://evil.example/x", "//evil.example/users/@me", "evil.example/users",
		"/guilds/../users/@me", "/guilds/%2e%2e/users", "/guilds//emojis", "/guilds/1/emojis?limit=1", "/guilds/1#x",
		"/guilds/a%2Fb", "/guilds\\1", "/api/v10/users/@me", "/guilds/ 1", "/guilds/%zz", "",
	} {
		if _, err := service.Run(context.Background(), "vortex-discord", "api_get", map[string]string{"path": path}, false); err == nil {
			t.Errorf("api_get accepted unsafe path %q", path)
		}
	}
	if len(fake.requests) != 0 {
		t.Fatalf("an unsafe path reached the network: %#v", fake.requests)
	}

	result, err := service.Run(context.Background(), "vortex-discord", "api_get", map[string]string{"path": "/guilds/123456789012345678/emojis", "query": `{"limit":10,"after":"345678901234567890"}`}, false)
	if err != nil {
		t.Fatal(err)
	}
	request := fake.last(t)
	if request.Method != http.MethodGet || request.EscapedPath != "/guilds/123456789012345678/emojis" || request.RawQuery != "after=345678901234567890&limit=10" || request.Authorization != "Bot synthetic-bot-token" {
		t.Fatalf("api_get request = %#v", request)
	}
	if list, _ := result.Data.([]any); len(list) != 1 {
		t.Fatalf("api_get result = %#v", result.Data)
	}

	// An "@host" segment is only ever a path segment on the fixed origin.
	plan, err := service.Plan(context.Background(), "vortex-discord", "api_get", map[string]string{"path": "/@evil.example/users"}, false)
	if err != nil {
		t.Fatal(err)
	}
	firstHost := fake.requests[0].Host
	if !strings.HasSuffix(plan.Runtime.Destination, firstHost) || plan.Runtime.Path != "/api/v10/@evil.example/users" || plan.Runtime.Method != http.MethodGet {
		t.Fatalf("api_get plan = %#v (host %s)", plan.Runtime, firstHost)
	}
}

func TestDiscordAPICallRequiresWriteAndSendsMethodBodyQueryAndReason(t *testing.T) {
	fake, service := newDiscordHarness(t)
	arguments := map[string]string{"method": "PATCH", "path": "/guilds/123456789012345678/members/@me", "query": "dry=1", "body": `{"nick":"Helper","roles":["567890123456789012"]}`, "reason": "Owner asked"}
	if _, err := service.Run(context.Background(), "vortex-discord", "api_call", arguments, false); err == nil {
		t.Fatal("api_call ran without explicit write intent")
	}
	if len(fake.requests) != 0 {
		t.Fatalf("api_call reached Discord without --write: %#v", fake.requests)
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "api_call", arguments, true); err != nil {
		t.Fatal(err)
	}
	request := fake.last(t)
	if request.Method != http.MethodPatch || request.EscapedPath != "/guilds/123456789012345678/members/@me" || request.RawQuery != "dry=1" || request.AuditReason != "Owner%20asked" {
		t.Fatalf("api_call request = %#v", request)
	}
	if roles, _ := request.Body["roles"].([]any); request.Body["nick"] != "Helper" || len(roles) != 1 || roles[0] != "567890123456789012" {
		t.Fatalf("api_call body = %#v", request.Body)
	}
	for _, bad := range []map[string]string{
		{"method": "GET", "path": "/users/@me"},
		{"method": "POST", "path": "https://evil.example/api/v10/channels/1/messages", "body": `{}`},
		{"method": "DELETE", "path": "/channels/../../x"},
		{"method": "POST", "path": "/channels/1/messages", "body": `{"content":`},
	} {
		before := len(fake.requests)
		if _, err := service.Run(context.Background(), "vortex-discord", "api_call", bad, true); err == nil || len(fake.requests) != before {
			t.Errorf("api_call accepted %v (err=%v)", bad, err)
		}
	}
	plan, err := service.Plan(context.Background(), "vortex-discord", "api_call", map[string]string{"method": "delete", "path": "/channels/234567890123456789/messages/345678901234567899"}, true)
	if err != nil || plan.Runtime.Method != http.MethodDelete || plan.Runtime.Body != nil || plan.Runtime.Class != "destructive" {
		t.Fatalf("api_call delete plan = %#v %v", plan.Runtime, err)
	}
}
