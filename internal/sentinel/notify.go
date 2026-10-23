package sentinel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/fileperm"
)

// TokenEnv overrides SlackConfig.BotTokenFile (e.g. under doppler run).
const TokenEnv = "PYRE_SENTINEL_SLACK_TOKEN"

// SlackNotifier sends a Slack DM to the owner: through a bot token
// (conversations.open + chat.postMessage, which notifies him), falling back
// to agent-slack (posts as the owner himself: a record, usually silent).
type SlackNotifier struct {
	Cfg  SlackConfig
	HTTP *http.Client

	mu       sync.Mutex
	channel  string
	recorded map[string]bool // message keys already copied through agent-slack
}

func (n *SlackNotifier) httpClient() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

// token reads the bot token from the environment or the 0600 token file.
func (n *SlackNotifier) token() (string, error) {
	if v := strings.TrimSpace(os.Getenv(TokenEnv)); v != "" {
		return v, nil
	}
	path := n.Cfg.BotTokenFile
	if path == "" {
		return "", errors.New("no bot_token_file configured")
	}
	f, err := fileperm.OpenNoFollow(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	if why, bad := fileperm.Exposed(path, fi); bad {
		return "", fmt.Errorf("%s is accessible to other users (%s); chmod 600 it", path, why)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return tok, nil
}

// errUnknownOutcome: the request may have reached Slack (a timeout after
// sending). Falling back to agent-slack then could post the alert twice.
var errUnknownOutcome = errors.New("delivery outcome unknown")

func (n *SlackNotifier) api(ctx context.Context, token, method string, body any) (map[string]any, error) {
	out, _, err := n.call(ctx, token, method, body)
	return out, err
}

func (n *SlackNotifier) call(ctx context.Context, token, method string, body any) (map[string]any, http.Header, error) {
	base := n.Cfg.APIBase
	if base == "" {
		base = "https://slack.com/api"
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/"+method, bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := n.httpClient().Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("slack %s: %s: %w", method, shortErr(err), errUnknownOutcome)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, resp.Header, fmt.Errorf("slack %s: HTTP %d", method, resp.StatusCode)
	}
	if ok, _ := out["ok"].(bool); !ok {
		e, _ := out["error"].(string)
		return nil, resp.Header, fmt.Errorf("slack %s: %s", method, e)
	}
	return out, resp.Header, nil
}

// Check verifies that the bot can DM: the token is readable and private,
// auth.test accepts it, and it carries chat:write and im:write. It sends
// nothing.
func (n *SlackNotifier) Check(ctx context.Context) error {
	tok, err := n.token()
	if err != nil {
		return err
	}
	_, hdr, err := n.call(ctx, tok, "auth.test", map[string]any{})
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, sc := range strings.Split(hdr.Get("X-Oauth-Scopes"), ",") {
		have[strings.TrimSpace(sc)] = true
	}
	var missing []string
	for _, need := range []string{"chat:write", "im:write"} {
		if !have[need] {
			missing = append(missing, need)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("slack bot token lacks %s", strings.Join(missing, ", "))
	}
	return nil
}

func (n *SlackNotifier) sendBot(ctx context.Context, text string) error {
	tok, err := n.token()
	if err != nil {
		return err
	}
	n.mu.Lock()
	ch := n.channel
	n.mu.Unlock()
	if ch == "" {
		out, err := n.api(ctx, tok, "conversations.open", map[string]any{"users": n.Cfg.UserID})
		if err != nil {
			return err
		}
		c, _ := out["channel"].(map[string]any)
		ch, _ = c["id"].(string)
		if ch == "" {
			return errors.New("slack conversations.open: no channel id")
		}
		n.mu.Lock()
		n.channel = ch
		n.mu.Unlock()
	}
	_, err = n.api(ctx, tok, "chat.postMessage", map[string]any{"channel": ch, "text": text, "unfurl_links": false, "unfurl_media": false})
	return err
}

func (n *SlackNotifier) agentSlack() string {
	bin := n.Cfg.AgentSlack
	if bin == "" {
		return ""
	}
	if strings.ContainsRune(bin, os.PathSeparator) {
		return expandHome(bin)
	}
	if p, err := exec.LookPath(bin); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, ".local", "bin", bin); fileExists(p) {
		return p
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (n *SlackNotifier) sendAgentSlack(ctx context.Context, text string) error {
	bin := n.agentSlack()
	if bin == "" {
		return errors.New("agent-slack fallback not available")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "message", "send", n.Cfg.UserID, text).CombinedOutput()
	if err != nil {
		return fmt.Errorf("agent-slack: %v: %s", err, lastLine(string(out)))
	}
	return nil
}

// Send implements Notifier. Only a bot DM counts as delivered: it notifies
// the owner. When the bot fails, the message is recorded once per key
// (incident and kind, not the text, which changes every round) through
// agent-slack (the owner's own account, silent) and Send still returns the
// bot error, so the caller keeps retrying the bot and the sentinel reports
// it cannot deliver.
func (n *SlackNotifier) Send(ctx context.Context, key, text string) error {
	berr := n.sendBot(ctx, text)
	if berr == nil {
		return nil
	}
	if errors.Is(berr, errUnknownOutcome) {
		return berr // may have been posted: no agent-slack copy now
	}
	n.mu.Lock()
	done := n.recorded[key]
	n.mu.Unlock()
	if done {
		return fmt.Errorf("bot: %w (already recorded through agent-slack)", berr)
	}
	if aerr := n.sendAgentSlack(ctx, text); aerr != nil {
		return fmt.Errorf("bot: %w; %v", berr, aerr)
	}
	n.mu.Lock()
	if n.recorded == nil {
		n.recorded = map[string]bool{}
	}
	if len(n.recorded) > 256 {
		clear(n.recorded)
	}
	n.recorded[key] = true
	n.mu.Unlock()
	return fmt.Errorf("bot: %w (recorded through agent-slack, which does not notify)", berr)
}
