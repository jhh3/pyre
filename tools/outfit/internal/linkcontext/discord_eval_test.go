package linkcontext

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jhh3/outfit/internal/connection"
	"github.com/jhh3/outfit/internal/runtime"
)

// discordRequest is what the fake Discord API observed for one call.
type discordRequest struct {
	Method, EscapedPath, RawQuery, Authorization, UserAgent, Host, AuditReason string
	HasReason                                                                  bool
	Body                                                                       map[string]any
}

// fakeDiscord is a synthetic Discord v10 API. It enforces the bot
// Authorization and the mandatory DiscordBot User-Agent on every request and
// can rate-limit a path a fixed number of times with Discord's 429 body.
type fakeDiscord struct {
	mu        sync.Mutex
	requests  []discordRequest
	throttles map[string]int
}

func (fake *fakeDiscord) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		var body map[string]any
		if len(raw) > 0 {
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.UseNumber()
			if err := decoder.Decode(&body); err != nil {
				t.Errorf("malformed JSON body %q: %v", raw, err)
			}
		}
		path := strings.TrimPrefix(request.URL.EscapedPath(), "/api/v10")
		fake.mu.Lock()
		_, hasReason := request.Header["X-Audit-Log-Reason"]
		fake.requests = append(fake.requests, discordRequest{Method: request.Method, EscapedPath: path, RawQuery: request.URL.RawQuery, Authorization: request.Header.Get("Authorization"), UserAgent: request.Header.Get("User-Agent"), Host: request.Host, AuditReason: request.Header.Get("X-Audit-Log-Reason"), HasReason: hasReason, Body: body})
		throttled := fake.throttles[path] > 0
		if throttled {
			fake.throttles[path]--
		}
		fake.mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		if request.Header.Get("Authorization") != "Bot synthetic-bot-token" || !strings.HasPrefix(request.Header.Get("User-Agent"), "DiscordBot (https://") {
			response.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(response, `{"message":"401: Unauthorized","code":0}`)
			return
		}
		if throttled {
			response.Header().Set("X-RateLimit-Limit", "5")
			response.Header().Set("X-RateLimit-Remaining", "0")
			response.Header().Set("X-RateLimit-Reset-After", "0.02")
			response.Header().Set("X-RateLimit-Scope", "user")
			response.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(response, `{"message":"You are being rate limited.","retry_after":0.02,"global":false}`)
			return
		}
		switch {
		case request.Method == http.MethodGet && path == "/users/@me":
			_, _ = io.WriteString(response, `{"id":"900000000000000001","username":"synthetic-helper","bot":true}`)
		case request.Method == http.MethodGet && path == "/guilds/123456789012345678/channels":
			_, _ = io.WriteString(response, `[{"id":"234567890123456789","name":"support","type":0,"position":1,"last_message_id":"345678901234567899","permission_overwrites":[{"id":"1","type":0}],"topic":"long topic"},{"id":"234567890123456790","name":"feedback","type":15,"parent_id":"111","last_message_id":null,"available_tags":[{"id":"678901234567890123","name":"bug"}]}]`)
		case request.Method == http.MethodGet && path == "/guilds/123456789012345678/threads/active":
			_, _ = io.WriteString(response, `{"threads":[{"id":"777777777777777777","name":"Login loop","type":11,"parent_id":"234567890123456789","last_message_id":"345678901234567950","message_count":4,"owner_id":"456789012345678901","thread_metadata":{"archived":false}}],"members":[{"id":"777777777777777777","user_id":"900000000000000001"}]}`)
		case request.Method == http.MethodGet && path == "/channels/234567890123456789/messages":
			_, _ = io.WriteString(response, `[{"id":"345678901234567899","content":"new question","author":{"id":"456789012345678901","username":"synthetic-user"}}]`)
		case request.Method == http.MethodPost && (path == "/channels/234567890123456789/messages" || path == "/channels/234567890123456790/threads"):
			_, _ = io.WriteString(response, `{"id":"345678901234568000","channel_id":"234567890123456789","content":"ok"}`)
		case request.Method == http.MethodGet && path == "/channels/234567890123456789/messages/345678901234568000":
			_, _ = io.WriteString(response, `{"id":"345678901234568000","type":0,"content":"","poll":{"question":{"text":"Which day?"},"answers":[{"answer_id":1,"poll_media":{"text":"Friday"}},{"answer_id":2,"poll_media":{"text":"Saturday"}}],"expiry":"2026-10-11T00:00:00+00:00","allow_multiselect":false,"layout_type":1,"results":{"is_finalized":true,"answer_counts":[{"id":1,"count":7,"me_voted":false},{"id":2,"count":3,"me_voted":false}]}}}`)
		case request.Method == http.MethodGet && path == "/channels/234567890123456789/polls/345678901234568000/answers/2":
			_, _ = io.WriteString(response, `{"users":[{"id":"456789012345678901","username":"synthetic-user"}]}`)
		case request.Method == http.MethodPost && path == "/channels/234567890123456789/polls/345678901234568000/expire":
			_, _ = io.WriteString(response, `{"id":"345678901234568000","poll":{"results":{"is_finalized":false}}}`)
		case request.Method == http.MethodPatch && strings.HasPrefix(path, "/guilds/123456789012345678/members/"):
			_, _ = io.WriteString(response, `{"user":{"id":"456789012345678901"},"communication_disabled_until":null}`)
		case request.Method == http.MethodGet && path == "/guilds/123456789012345678/emojis":
			_, _ = io.WriteString(response, `[{"id":"567890123456789012","name":"vortex"}]`)
		case (request.Method == http.MethodPut || request.Method == http.MethodDelete) && strings.Contains(path, "/roles/"):
			response.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPut && strings.HasSuffix(path, "/@me"):
			response.WriteHeader(http.StatusNoContent)
		default:
			response.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(response, `{"message":"Unknown fixture","code":10003}`)
		}
	})
}

func (fake *fakeDiscord) last(t *testing.T) discordRequest {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) == 0 {
		t.Fatal("fake Discord saw no request")
	}
	return fake.requests[len(fake.requests)-1]
}

func newDiscordHarness(t *testing.T) (*fakeDiscord, connection.Service) {
	t.Helper()
	fake := &fakeDiscord{throttles: map[string]int{}}
	server := httptest.NewTLSServer(fake.handler(t))
	t.Cleanup(server.Close)
	store := linkTestStore(t)
	vault := &evalVault{values: map[string]map[string]string{}}
	installEvalConnection(t, store, vault, "discord", "rest", "managed-token", "vortex-discord", server.URL+"/api/v10/", map[string]string{"TOKEN": "synthetic-bot-token"}, nil)
	return fake, connection.Service{State: store, Vault: vault, Executor: runtime.Executor{HTTPClient: server.Client()}}
}

func TestDiscordVerifiesWithBotAuthorizationAndUserAgent(t *testing.T) {
	fake, service := newDiscordHarness(t)
	result, err := service.Run(context.Background(), "vortex-discord", "me", nil, false)
	if err != nil || result.Status != http.StatusOK {
		t.Fatalf("me: %v %#v", err, result)
	}
	request := fake.last(t)
	if request.Authorization != "Bot synthetic-bot-token" || !strings.HasPrefix(request.UserAgent, "DiscordBot (https://github.com/jhh3/pyre, ") {
		t.Fatalf("headers = %#v", request)
	}
}

func TestDiscordMessagesReadSendsCursorQuery(t *testing.T) {
	fake, service := newDiscordHarness(t)
	if _, err := service.Run(context.Background(), "vortex-discord", "messages", map[string]string{"channel": "234567890123456789", "after": "345678901234567890", "limit": "100"}, false); err != nil {
		t.Fatal(err)
	}
	request := fake.last(t)
	if request.Method != http.MethodGet || request.EscapedPath != "/channels/234567890123456789/messages" || request.RawQuery != "after=345678901234567890&limit=100" {
		t.Fatalf("messages request = %#v", request)
	}
}

func TestDiscordWritesRequireExplicitIntent(t *testing.T) {
	fake, service := newDiscordHarness(t)
	if _, err := service.Run(context.Background(), "vortex-discord", "send_message", map[string]string{"channel": "234567890123456789", "content": "hi"}, false); err == nil {
		t.Fatal("send_message ran without explicit write intent")
	}
	if len(fake.requests) != 0 {
		t.Fatalf("a mutation reached Discord without --write: %#v", fake.requests)
	}
}

func TestDiscordSendMessageDefaultsToNoMentionsAndKeepsSnowflakes(t *testing.T) {
	fake, service := newDiscordHarness(t)
	arguments := map[string]string{
		"channel":           "234567890123456789",
		"content":           "42",
		"message_reference": `{"message_id":"345678901234567899","fail_if_not_exists":false}`,
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "send_message", arguments, true); err != nil {
		t.Fatal(err)
	}
	body := fake.last(t).Body
	if body["content"] != "42" {
		t.Fatalf("content was coerced: %#v", body["content"])
	}
	mentions, _ := body["allowed_mentions"].(map[string]any)
	if parse, ok := mentions["parse"].([]any); !ok || len(parse) != 0 || len(mentions) != 1 {
		t.Fatalf("allowed_mentions default = %#v", body["allowed_mentions"])
	}
	reference, _ := body["message_reference"].(map[string]any)
	if reference["message_id"] != "345678901234567899" || reference["fail_if_not_exists"] != false {
		t.Fatalf("message_reference = %#v", body["message_reference"])
	}
	if body["enforce_nonce"] != true {
		t.Fatalf("enforce_nonce = %#v", body["enforce_nonce"])
	}

	explicit := map[string]string{"channel": "234567890123456789", "content": "ping", "allowed_mentions": `{"users":["456789012345678901"]}`}
	if _, err := service.Run(context.Background(), "vortex-discord", "send_message", explicit, true); err != nil {
		t.Fatal(err)
	}
	mentions, _ = fake.last(t).Body["allowed_mentions"].(map[string]any)
	if users, _ := mentions["users"].([]any); len(users) != 1 || users[0] != "456789012345678901" || mentions["parse"] != nil {
		t.Fatalf("explicit allowed_mentions = %#v", mentions)
	}
}

func TestDiscordReplyBuildsMessageReferenceWithExactID(t *testing.T) {
	fake, service := newDiscordHarness(t)
	arguments := map[string]string{"channel": "234567890123456789", "message": "345678901234567899", "content": "Thanks! (automated helper)", "nonce": "r345678901234567899"}
	if _, err := service.Run(context.Background(), "vortex-discord", "reply", arguments, true); err != nil {
		t.Fatal(err)
	}
	request := fake.last(t)
	reference, _ := request.Body["message_reference"].(map[string]any)
	if request.Method != http.MethodPost || reference["message_id"] != "345678901234567899" || reference["fail_if_not_exists"] != false {
		t.Fatalf("reply request = %#v", request)
	}
	mentions, _ := request.Body["allowed_mentions"].(map[string]any)
	if parse, _ := mentions["parse"].([]any); len(parse) != 0 || mentions["replied_user"] != true {
		t.Fatalf("reply allowed_mentions = %#v", mentions)
	}
	if request.Body["nonce"] != "r345678901234567899" || request.Body["content"] != "Thanks! (automated helper)" {
		t.Fatalf("reply body = %#v", request.Body)
	}
}

func TestDiscordCreateForumPostSendsStarterMessageAndTags(t *testing.T) {
	fake, service := newDiscordHarness(t)
	arguments := map[string]string{"channel": "234567890123456790", "name": "2026", "content": "Crash on launch", "applied_tags": `["678901234567890123"]`, "auto_archive_duration": "1440"}
	if _, err := service.Run(context.Background(), "vortex-discord", "create_forum_post", arguments, true); err != nil {
		t.Fatal(err)
	}
	request := fake.last(t)
	body := request.Body
	if request.EscapedPath != "/channels/234567890123456790/threads" || body["name"] != "2026" || body["auto_archive_duration"] != json.Number("1440") {
		t.Fatalf("forum post request = %#v", request)
	}
	if tags, _ := body["applied_tags"].([]any); len(tags) != 1 || tags[0] != "678901234567890123" {
		t.Fatalf("applied_tags = %#v", body["applied_tags"])
	}
	message, _ := body["message"].(map[string]any)
	mentions, _ := message["allowed_mentions"].(map[string]any)
	if message["content"] != "Crash on launch" || mentions == nil || message["embeds"] != nil {
		t.Fatalf("starter message = %#v", body["message"])
	}
	if _, hasType := body["type"]; hasType {
		t.Fatalf("forum post must not send a thread type: %#v", body)
	}
}

func TestDiscordCreateThreadDefaultsToPublicThread(t *testing.T) {
	fake, service := newDiscordHarness(t)
	if _, err := service.Run(context.Background(), "vortex-discord", "create_thread", map[string]string{"channel": "234567890123456790", "name": "Help"}, true); err != nil {
		t.Fatal(err)
	}
	if body := fake.last(t).Body; body["type"] != json.Number("11") || body["name"] != "Help" {
		t.Fatalf("create_thread body = %#v", body)
	}
}

func TestDiscordReactionEmojiIsPathEncoded(t *testing.T) {
	fake, service := newDiscordHarness(t)
	if _, err := service.Run(context.Background(), "vortex-discord", "add_reaction", map[string]string{"channel": "234567890123456789", "message": "345678901234567899", "emoji": "👍"}, true); err != nil {
		t.Fatal(err)
	}
	if request := fake.last(t); request.Method != http.MethodPut || request.EscapedPath != "/channels/234567890123456789/messages/345678901234567899/reactions/%F0%9F%91%8D/@me" {
		t.Fatalf("unicode reaction request = %#v", request)
	}
	if _, err := service.Run(context.Background(), "vortex-discord", "add_reaction", map[string]string{"channel": "234567890123456789", "message": "345678901234567899", "emoji": "vortex:567890123456789012"}, true); err != nil {
		t.Fatal(err)
	}
	if request := fake.last(t); request.EscapedPath != "/channels/234567890123456789/messages/345678901234567899/reactions/vortex:567890123456789012/@me" {
		t.Fatalf("custom reaction request = %#v", request)
	}
}

func TestDiscordRetriesRateLimitedWriteAfterRetryAfter(t *testing.T) {
	fake, service := newDiscordHarness(t)
	fake.throttles["/channels/234567890123456789/messages"] = 2
	if _, err := service.Run(context.Background(), "vortex-discord", "send_message", map[string]string{"channel": "234567890123456789", "content": "after the limit"}, true); err != nil {
		t.Fatalf("rate-limited send did not recover: %v", err)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("requests = %d, want 2 throttled + 1 accepted", len(fake.requests))
	}
	for _, request := range fake.requests {
		if request.Body["content"] != "after the limit" {
			t.Fatalf("retry lost the body: %#v", request.Body)
		}
	}

	fake.requests = nil
	fake.throttles["/users/@me"] = 10
	if _, err := service.Run(context.Background(), "vortex-discord", "me", nil, false); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("persistent rate limit error = %v", err)
	}
	if len(fake.requests) != 4 {
		t.Fatalf("persistent 429 attempts = %d, want 1 + 3 bounded retries", len(fake.requests))
	}
}

func TestDiscordActivityReturnsCompactChannelsAndThreads(t *testing.T) {
	_, service := newDiscordHarness(t)
	result, err := service.Run(context.Background(), "vortex-discord", "activity", map[string]string{"guild": "123456789012345678"}, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := result.Data.(map[string]any)
	channels, _ := data["channels"].([]any)
	threads, _ := data["threads"].([]any)
	if len(channels) != 2 || len(threads) != 1 {
		t.Fatalf("activity = %#v", result.Data)
	}
	first, _ := channels[0].(map[string]any)
	if first["last_message_id"] != "345678901234567899" || first["permission_overwrites"] != nil || first["topic"] != nil {
		t.Fatalf("channel projection = %#v", first)
	}
	thread, _ := threads[0].(map[string]any)
	if thread["last_message_id"] != "345678901234567950" || thread["parent_id"] != "234567890123456789" || thread["thread_metadata"] != nil {
		t.Fatalf("thread projection = %#v", thread)
	}
	if !strings.Contains(string(result.Raw), `"threads"`) {
		t.Fatalf("raw activity = %s", result.Raw)
	}
}
