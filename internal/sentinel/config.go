package sentinel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/fileperm"
	"github.com/jhh3/pyre/internal/transport"
)

// Config is the sentinel configuration file (default
// ~/.config/pyre/sentinel.json, mode 0600). Each machine has its own copy:
// Self names the machine and every ssh alias is resolved by that machine's
// ~/.ssh/config.
type Config struct {
	// Self is this sentinel's name; it must appear in Sentinels.
	Self string `json:"self"`
	// Sentinels in rank order: the first live one is the leader, the only
	// sentinel that alerts and recovers.
	Sentinels []SentinelRef `json:"sentinels"`
	// Targets are the hosts this sentinel probes. Every other sentinel host
	// is added as a target (ssh reachability, user manager) when not listed.
	Targets []Target      `json:"targets"`
	Network NetworkConfig `json:"network"`
	Slack   SlackConfig   `json:"slack"`
	Timing  Timing        `json:"timing"`
	// SSHIdentity is an optional private key for the sentinel's ssh (passed
	// as -i): a dedicated passphrase-less key, since a service has no ssh
	// agent. Install its public key on each target with "restrict,".
	SSHIdentity string `json:"ssh_identity,omitempty"`
	// Sites groups hosts by network location ({"home": ["server", "laptop"],
	// "office": ["office-box1"]}). Optional: it names the link in a
	// partition message and lets a target's own self-report count as
	// "healthy" evidence when only watchers at another site fail.
	Sites map[string][]string `json:"sites,omitempty"`
	// StateDir holds state.json and peers/ (default
	// ~/.local/state/pyre/sentinel on every platform, so peers can read it
	// over ssh at a fixed path).
	StateDir string `json:"state_dir,omitempty"`
}

// SentinelRef names one sentinel and how this machine reaches it.
type SentinelRef struct {
	Name string `json:"name"`
	// SSH aliases tried in order (e.g. a LAN alias, then a port-forward).
	SSH []string `json:"ssh,omitempty"`
	// Via names a target this machine's only path to the sentinel runs
	// through (a ProxyJump host). While that target is DOWN, the sentinel
	// being unreadable is expected and not reported as missing.
	Via string `json:"via,omitempty"`
}

// Target is one watched host.
type Target struct {
	Name string `json:"name"`
	// SSH aliases tried in order; the first that connects is used.
	SSH []string `json:"ssh,omitempty"`
	// Hub /healthz endpoints (unauthenticated). Any one passing means the
	// hub is up. HubPin, when set, is the hub certificate's SHA-256
	// fingerprint; without it /healthz skips certificate verification.
	Hub    []Endpoint `json:"hub,omitempty"`
	HubPin string     `json:"hub_pin,omitempty"`
	// Units checked with `systemctl --user is-active` (Linux) or
	// `launchctl list` (macOS); any not active is DEGRADED.
	Units []string `json:"units,omitempty"`
	// TCP "host:port" checks run on the target over ssh (nc -z, or bash
	// /dev/tcp); a closed port is DEGRADED.
	TCP []string `json:"tcp,omitempty"`
	// Public HTTPS endpoints probed from the sentinel. A response below 500
	// without an ngrok error header counts as up.
	Public []Endpoint `json:"public,omitempty"`
	// NoRecovery disables the ssh-login recovery for this target.
	NoRecovery bool `json:"no_recovery,omitempty"`
	// Via names the target this machine's only path to this one runs
	// through (a ProxyJump host). While Via is DOWN, failing to reach this
	// target is expected: the vote is "unknown", not "down". A sentinel's
	// implicit target inherits the sentinel's Via.
	Via string `json:"via,omitempty"`

	auto bool // added from Sentinels, not listed in Targets
}

// Endpoint is a URL with a short label used in alerts ("LAN", "Tailscale").
type Endpoint struct {
	URL   string `json:"url"`
	Label string `json:"label,omitempty"`
}

func (e Endpoint) name() string {
	if e.Label != "" {
		return e.Label
	}
	return e.URL
}

// NetworkConfig is the self network check. The sentinel votes "unknown" for
// every target unless its default gateway or one public address answers.
type NetworkConfig struct {
	// Public "host:port" addresses dialled over TCP (default 1.1.1.1:443,
	// 8.8.8.8:53).
	Public []string `json:"public,omitempty"`
	// NoGateway skips the default-gateway dial.
	NoGateway bool `json:"no_gateway,omitempty"`
}

// SlackConfig is the alert transport: a Slack DM to UserID.
type SlackConfig struct {
	// UserID is the Slack member ID alerts are sent to (U...).
	UserID string `json:"user_id"`
	// BotTokenFile is a 0600 file holding a bot token (xoxb-...) with
	// chat:write and im:write. A bot DM notifies; a message from the owner's
	// own account to his self-DM does not. PYRE_SENTINEL_SLACK_TOKEN
	// overrides the file (doppler run).
	BotTokenFile string `json:"bot_token_file,omitempty"`
	// AgentSlack is the agent-slack executable used when the bot token is
	// missing or fails ("" disables it, "agent-slack" finds it on PATH or in
	// ~/.local/bin). It posts as the owner, so it is a written record more
	// than a notification.
	AgentSlack string `json:"agent_slack,omitempty"`
	// APIBase overrides https://slack.com/api (tests).
	APIBase string `json:"api_base,omitempty"`
}

// Timing holds the intervals; every field has a default.
type Timing struct {
	Interval      Duration   `json:"interval,omitempty"`       // 60s
	DownGrace     Duration   `json:"down_grace,omitempty"`     // 3m
	DegradedGrace Duration   `json:"degraded_grace,omitempty"` // 10m
	Stale         Duration   `json:"stale,omitempty"`          // 3m: a peer state older than this is missing
	RecoverGrace  Duration   `json:"recover_grace,omitempty"`  // 2m clear before "recovered"
	RecoveryRetry Duration   `json:"recovery_retry,omitempty"` // 4m between recovery attempts
	Reminders     []Duration `json:"reminders,omitempty"`      // 30m, 2h after the first alert
	// PublicGrace is how long a problem that is only public URLs failing
	// must hold before it alerts (and then only when the target's own
	// check of the URL fails too, when it reports one).
	PublicGrace         Duration `json:"public_grace,omitempty"`          // 15m
	MaxRecoveryAttempts int      `json:"max_recovery_attempts,omitempty"` // 2
}

// Duration is a time.Duration written as a Go duration string ("90s").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration: want a string like \"90s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// D returns the time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// DefaultConfigPath is ~/.config/pyre/sentinel.json.
func DefaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "pyre", "sentinel.json")
}

// DefaultStateDir is ~/.local/state/pyre/sentinel (all platforms).
func DefaultStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "pyre", "sentinel")
}

// Remote paths, relative to the remote $HOME; peers read and write them over
// ssh, so they are the same on every platform.
const (
	remoteStateFile       = ".local/state/pyre/sentinel/state.json"
	remotePeersDir        = ".local/state/pyre/sentinel/peers"
	remoteMaintenanceFile = ".local/state/pyre/maintenance.json"
)

// LoadConfig reads and validates the config at path. The file must not be
// readable by other users.
func LoadConfig(path string) (*Config, error) {
	f, err := fileperm.OpenNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if why, bad := fileperm.Exposed(path, fi); bad {
		return nil, fmt.Errorf("%s is accessible to other users (%s); chmod 600 it", path, why)
	}
	var c Config
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Normalize(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// safeWord is what may be interpolated into the remote probe script: unit
// names, host:port pairs, sentinel names.
var safeWord = regexp.MustCompile(`^[A-Za-z0-9@._:\[\]-]+$`)

// Normalize applies defaults, adds the implicit sentinel targets and
// validates the config.
func (c *Config) Normalize() error {
	if c.Self == "" {
		return errors.New("self is required")
	}
	if len(c.Sentinels) == 0 {
		return errors.New("sentinels is required (rank order)")
	}
	seen := map[string]bool{}
	selfFound := false
	for _, s := range c.Sentinels {
		if !safeWord.MatchString(s.Name) || strings.HasPrefix(s.Name, "-") {
			return fmt.Errorf("sentinel name %q: use letters, digits, '.', '_', '-'", s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("sentinel %q listed twice", s.Name)
		}
		seen[s.Name] = true
		if s.Name == c.Self {
			selfFound = true
		}
	}
	if !selfFound {
		return fmt.Errorf("self %q is not in sentinels", c.Self)
	}
	targets := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		if !safeWord.MatchString(t.Name) {
			return fmt.Errorf("target name %q: use letters, digits, '.', '_', '-'", t.Name)
		}
		if targets[t.Name] {
			return fmt.Errorf("target %q listed twice", t.Name)
		}
		targets[t.Name] = true
		if len(t.SSH) == 0 {
			if r, ok := c.sentinel(t.Name); ok && len(r.SSH) > 0 {
				t.SSH = r.SSH
			} else {
				t.SSH = []string{t.Name}
			}
		}
		for _, w := range append(append([]string{}, t.Units...), t.TCP...) {
			if !safeWord.MatchString(w) || strings.HasPrefix(w, "-") {
				return fmt.Errorf("target %s: %q is not a plain unit name or host:port", t.Name, w)
			}
		}
		for _, hp := range t.TCP {
			if i := strings.LastIndex(hp, ":"); i <= 0 || i == len(hp)-1 {
				return fmt.Errorf("target %s: tcp %q is not host:port", t.Name, hp)
			}
		}
		if t.HubPin != "" {
			if _, err := transport.NormalizeFingerprint(t.HubPin); err != nil {
				return fmt.Errorf("target %s: hub_pin: %v", t.Name, err)
			}
		}
		for _, e := range append(append([]Endpoint{}, t.Hub...), t.Public...) {
			if !strings.HasPrefix(e.URL, "https://") && !strings.HasPrefix(e.URL, "http://") {
				return fmt.Errorf("target %s: %q is not an http(s) URL", t.Name, e.URL)
			}
		}
	}
	for _, s := range c.Sentinels {
		if s.Via != "" && !targets[s.Via] && !seen[s.Via] {
			return fmt.Errorf("sentinel %s: via %q is not a target", s.Name, s.Via)
		}
		if s.Name == c.Self || targets[s.Name] {
			continue
		}
		ssh := s.SSH
		if len(ssh) == 0 {
			ssh = []string{s.Name}
		}
		c.Targets = append(c.Targets, Target{Name: s.Name, SSH: ssh, Via: s.Via, auto: true})
	}
	for _, t := range c.Targets {
		if t.Via != "" && !targets[t.Via] && !seen[t.Via] {
			return fmt.Errorf("target %s: via %q is not a target", t.Name, t.Via)
		}
		for _, a := range t.SSH {
			if strings.HasPrefix(a, "-") || !safeWord.MatchString(a) {
				return fmt.Errorf("target %s: ssh alias %q", t.Name, a)
			}
		}
	}
	inSite := map[string]string{}
	for _, site := range sortedKeys(c.Sites) {
		if !safeWord.MatchString(site) {
			return fmt.Errorf("site name %q: use letters, digits, '.', '_', '-'", site)
		}
		for _, h := range c.Sites[site] {
			if prev, ok := inSite[h]; ok {
				return fmt.Errorf("host %q is in sites %q and %q", h, prev, site)
			}
			inSite[h] = site
		}
	}
	if c.Slack.UserID == "" {
		return errors.New("slack.user_id is required")
	}
	c.Slack.BotTokenFile = expandHome(c.Slack.BotTokenFile)
	c.SSHIdentity = expandHome(c.SSHIdentity)
	if c.StateDir == "" {
		c.StateDir = DefaultStateDir()
	}
	c.StateDir = expandHome(c.StateDir)
	if len(c.Network.Public) == 0 {
		c.Network.Public = []string{"1.1.1.1:443", "8.8.8.8:53"}
	}
	tm := &c.Timing
	def := func(d *Duration, v time.Duration) {
		if *d <= 0 {
			*d = Duration(v)
		}
	}
	def(&tm.Interval, time.Minute)
	def(&tm.DownGrace, 3*time.Minute)
	def(&tm.DegradedGrace, 10*time.Minute)
	def(&tm.Stale, 3*time.Minute)
	def(&tm.RecoverGrace, 2*time.Minute)
	def(&tm.RecoveryRetry, 4*time.Minute)
	def(&tm.PublicGrace, 15*time.Minute)
	if tm.Reminders == nil {
		tm.Reminders = []Duration{Duration(30 * time.Minute), Duration(2 * time.Hour)}
	}
	if tm.MaxRecoveryAttempts <= 0 {
		tm.MaxRecoveryAttempts = 2
	}
	return nil
}

func (c *Config) sentinel(name string) (SentinelRef, bool) {
	for _, s := range c.Sentinels {
		if s.Name == name {
			return s, true
		}
	}
	return SentinelRef{}, false
}

// rank is the 1-based position of name in Sentinels (0 if absent).
func (c *Config) rank(name string) int {
	for i, s := range c.Sentinels {
		if s.Name == name {
			return i + 1
		}
	}
	return 0
}

// target returns the configured target by name.
func (c *Config) target(name string) (Target, bool) {
	for _, t := range c.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// site is the configured site of host ("" when none).
func (c *Config) site(host string) string {
	for _, name := range sortedKeys(c.Sites) {
		for _, h := range c.Sites[name] {
			if h == host {
				return name
			}
		}
	}
	return ""
}

// watched are the targets this sentinel probes: every target but itself (a
// sentinel never votes on its own host).
func (c *Config) watched() []Target {
	var out []Target
	for _, t := range c.Targets {
		if t.Name != c.Self {
			out = append(out, t)
		}
	}
	return out
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
