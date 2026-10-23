package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/osutil"
	"github.com/jhh3/pyre/internal/usage"
)

// agy usage: agy has no isolated profiles (its login is the machine's own,
// never a Pyre account), but it reports its own quota. `agy -p "/usage"
// --output-format json` answers from the quota service without starting an
// agent turn, spending quota or leaving a conversation behind (agy 1.1.11+
// changelog), as one bucket per model group and window. The node observer
// starts the read at most once per agyUsageEvery, in the background so it
// never delays Claude and Codex observation, and only where:
//   - agy is installed and signed in (where the agy adapter offers it),
//   - its version is at least agyUsageMinVersion (older agy sends
//     "/usage" to the model as a prompt), and
//   - agy was used in the last agyUsageRecent (history or a conversation
//     changed): each agy start may run its background auto-updater (agy
//     1.4.2 offers no switch for it: AGY_CLI_DISABLE_AUTO_UPDATE, found in
//     the binary, did not stop it), so a machine where nobody uses agy is
//     never updated by Pyre's reads.
//
// The read runs in a scratch directory with its log there (--log-file, so
// agy's own cli.log and log/ are untouched), a minimal environment (no API
// keys or auth overrides), its own process
// group killed on timeout and a bounded wait for lingering descendants.
// Only the parsed windows are kept: the output is never logged or stored.
// A reply that reports a conversation or a turn disables the read for that
// agy version (logged once). The reading is tied to the login it was taken
// for and travels in the node's harness inventory (HarnessInfo.Usage).

const (
	agyUsageEvery      = 5 * time.Minute
	agyUsageTimeout    = 20 * time.Second
	agyUsageWaitDelay  = 5 * time.Second
	agyUsageRecent     = 7 * 24 * time.Hour
	agyUsageSource     = "agy:/usage"
	agyUsageMinVersion = "1.1.11"
)

type agyUsageState struct {
	mu       sync.Mutex
	tried    time.Time
	inflight bool
	reading  *contracts.HarnessUsage
	login    string // identity of the login the reading is for (email or sub)
	// disabled is the agy version whose reply showed a turn or a
	// conversation: no reads with it.
	disabled string
}

// agyUsageResponse decodes the parts of agy's print-mode JSON that carry
// the quota. remaining_fraction is the unused share of a bucket.
type agyUsageResponse struct {
	Status         string `json:"status"`
	ConversationID string `json:"conversation_id"`
	NumTurns       int    `json:"num_turns"`
	Command        *struct {
		Name string `json:"name"`
		Data struct {
			Groups []struct {
				Name    string `json:"name"`
				Buckets []struct {
					Window            string   `json:"window"`
					RemainingFraction *float64 `json:"remaining_fraction"`
					ResetTime         string   `json:"reset_time"`
				} `json:"buckets"`
			} `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// ParseAgyUsage maps agy's /usage buckets to Pyre's windows: "5h" to
// five_hour and "weekly" to seven_day. Each model group (Gemini, Claude
// and GPT, Argon) has its own buckets, and which group a session draws on
// depends on its model (and, for Argon, on the plan), so each window is
// the most used bucket across the groups: the binding limit, never an
// understatement.
func ParseAgyUsage(raw []byte) ([]contracts.UsageWindow, error) {
	var r agyUsageResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, errors.New("agy usage: output is not JSON")
	}
	if r.Command == nil || r.Command.Name != "usage" || (r.Status != "" && r.Status != "SUCCESS") {
		return nil, errors.New("agy usage: no usage payload")
	}
	byName := map[string]*contracts.UsageWindow{}
	var order []string
	for _, g := range r.Command.Data.Groups {
		for _, b := range g.Buckets {
			name := ""
			switch b.Window {
			case "5h":
				name = usage.FiveHour
			case "weekly":
				name = usage.Weekly
			}
			if name == "" || b.RemainingFraction == nil {
				continue
			}
			used := clampPct(math.Round((1-*b.RemainingFraction)*10000) / 100) // 0.01 %
			w := byName[name]
			if w == nil {
				byName[name] = &contracts.UsageWindow{Name: name, UsedPercent: used, ResetAt: parseTimeLoose(b.ResetTime)}
				order = append(order, name)
				continue
			}
			if used > w.UsedPercent {
				w.UsedPercent, w.ResetAt = used, parseTimeLoose(b.ResetTime)
			}
		}
	}
	if len(order) == 0 {
		return nil, errors.New("agy usage: no 5h or weekly bucket")
	}
	out := make([]contracts.UsageWindow, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, nil
}

// errAgyTurn: the reply reports a turn or a conversation (not the
// read-only slash command answer).
var errAgyTurn = errors.New("agy usage: reply started a turn or a conversation")

// parseAgyReply is ParseAgyUsage plus the turn tripwire.
func parseAgyReply(raw []byte) ([]contracts.UsageWindow, error) {
	var r agyUsageResponse
	if err := json.Unmarshal(raw, &r); err == nil && (r.ConversationID != "" || r.NumTurns > 0) {
		return nil, errAgyTurn
	}
	return ParseAgyUsage(raw)
}

// versionAtLeast compares dotted numeric versions ("1.4.2" >= "1.1.11").
func versionAtLeast(v, min string) bool {
	a, b := strings.Split(v, "."), strings.Split(min, ".")
	for i := 0; i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x, _ = strconv.Atoi(a[i])
		}
		y, _ = strconv.Atoi(b[i])
		if x != y {
			return x > y
		}
	}
	return true
}

var agyVersionRe = regexp.MustCompile(`\d+\.\d+\.\d+`)

// agyEnv is the read's environment: the basics only, so an API key or an
// auth override in pyred's environment never switches agy's auth mode.
func agyEnv() []string {
	env := []string{"NO_COLOR=1"}
	for _, k := range []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// agyUsedSince reports whether agy was used after t: its prompt history or
// a conversation file changed (the /usage read itself writes neither).
func agyUsedSince(dir string, t time.Time) bool {
	if fi, err := os.Stat(filepath.Join(dir, "history.jsonl")); err == nil && fi.ModTime().After(t) {
		return true
	}
	es, _ := os.ReadDir(filepath.Join(dir, "conversations"))
	for _, e := range es {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(t) {
			return true
		}
	}
	return false
}

// agyCmd builds one agy invocation in dir: its own process group, killed
// whole on ctx's end, with a bounded wait for descendants holding stdout.
func agyCmd(ctx context.Context, dir, bin string, args ...string) *exec.Cmd {
	cmd := execCommand(ctx, bin, args...)
	cmd.Dir, cmd.Env = dir, agyEnv()
	osutil.NewProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = osutil.KillGroup(cmd.Process.Pid, true)
		}
		return nil
	}
	cmd.WaitDelay = agyUsageWaitDelay
	return cmd
}

// agyBin is the agy executable where agy is offered (installed and signed
// in, not Windows), else "".
func (s *Service) agyBin() string {
	goos := s.cfg.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if goos == "windows" || s.cfg.UserAgyDir == "" {
		return ""
	}
	name := s.cfg.AgyBin
	if name == "" {
		name = "agy"
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	if _, err := os.Stat(filepath.Join(s.cfg.UserAgyDir, "antigravity-oauth-token")); err != nil {
		return "" // not signed in: agy is not offered here
	}
	return bin
}

// kickAgyUsage starts a due agy read in the background (the node
// observer's pass): at most one at a time, never delaying the pass.
func (s *Service) kickAgyUsage(ctx context.Context) {
	s.agy.mu.Lock()
	if s.agy.inflight {
		s.agy.mu.Unlock()
		return
	}
	s.agy.inflight = true
	s.agy.mu.Unlock()
	go func() {
		defer func() {
			s.agy.mu.Lock()
			s.agy.inflight = false
			s.agy.mu.Unlock()
		}()
		s.observeAgyUsage(ctx)
	}()
}

// observeAgyUsage reads agy's quota when due.
func (s *Service) observeAgyUsage(ctx context.Context) {
	now := s.now()
	s.agy.mu.Lock()
	due := s.agy.tried.IsZero() || now.Sub(s.agy.tried) >= agyUsageEvery
	if due {
		s.agy.tried = now
	}
	s.agy.mu.Unlock()
	if !due {
		return
	}
	bin := s.agyBin()
	login := agyOwnLogin(s.cfg.UserAgyDir)
	if bin == "" || login == nil {
		s.agy.mu.Lock()
		s.agy.reading = nil // signed out or uninstalled: no reading
		s.agy.mu.Unlock()
		return
	}
	if !agyUsedSince(s.cfg.UserAgyDir, now.Add(-agyUsageRecent)) {
		return // not used lately: leave agy (and its updater) alone
	}
	dir, err := os.MkdirTemp("", "pyre-agy-usage-")
	if err != nil {
		return
	}
	defer os.RemoveAll(dir)
	cctx, cancel := context.WithTimeout(ctx, agyUsageTimeout)
	defer cancel()
	vout, err := agyCmd(cctx, dir, bin, "--version").Output()
	if err != nil {
		return
	}
	ver := agyVersionRe.FindString(string(vout))
	s.agy.mu.Lock()
	off := ver != "" && ver == s.agy.disabled
	s.agy.mu.Unlock()
	if ver == "" || off || !versionAtLeast(ver, agyUsageMinVersion) {
		return
	}
	out, err := agyCmd(cctx, dir, bin, "-p", "/usage", "--output-format", "json", "--print-timeout", "15s",
		"--log-file", filepath.Join(dir, "agy.log")).Output()
	if err != nil {
		s.log.Info("accounts: agy usage not read", "err", redact(err.Error()))
		return
	}
	ws, err := parseAgyReply(out)
	if errors.Is(err, errAgyTurn) {
		s.agy.mu.Lock()
		s.agy.disabled = ver
		s.agy.mu.Unlock()
		s.log.Warn("accounts: agy usage reads disabled for this agy version: /usage started a turn or a conversation", "version", ver)
		return
	}
	if err != nil {
		s.log.Info("accounts: agy usage not read", "err", err.Error())
		return
	}
	s.agy.mu.Lock()
	s.agy.reading = &contracts.HarnessUsage{Windows: ws, ObservedAt: s.now(), Source: agyUsageSource}
	s.agy.login = loginKey(login)
	s.agy.mu.Unlock()
}

func loginKey(l *contracts.HarnessLogin) string {
	if l == nil {
		return ""
	}
	if l.ProviderRef != "" {
		return "sub:" + l.ProviderRef
	}
	return "email:" + strings.ToLower(l.Email)
}

// OwnUsage is the last reading of the machine's own login for a harness
// whose login Pyre does not manage (agy), nil when none or when agy is now
// signed in as another identity. A failed read keeps the previous reading,
// which then ages (clients show it stale).
func (s *Service) OwnUsage(h contracts.Harness) *contracts.HarnessUsage {
	if h != contracts.HarnessAgy {
		return nil
	}
	cur := loginKey(agyOwnLogin(s.cfg.UserAgyDir))
	s.agy.mu.Lock()
	defer s.agy.mu.Unlock()
	if s.agy.reading == nil || cur == "" || cur != s.agy.login {
		return nil
	}
	cp := *s.agy.reading
	cp.Windows = append([]contracts.UsageWindow(nil), cp.Windows...)
	return &cp
}
