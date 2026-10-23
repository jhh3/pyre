package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }

// world is a fake fleet: hosts, their health, who can reach whom, and the
// sentinel engines running on some of them. No network, no ssh.
type world struct {
	t       *testing.T
	clock   *fakeClock
	order   []string
	engines map[string]*Engine
	running map[string]bool // sentinel process alive

	hostUp       map[string]bool // reachable at all (ssh + hub ports)
	hubUp        map[string]bool
	manager      map[string]string
	managerStart map[string]int64
	unitsDown    map[string][]string
	netOK        map[string]bool
	blocked      map[[2]string]bool   // [from, to]
	via          map[[2]string]string // [from, to] -> the host the path runs through
	sshBroken    map[string]bool      // sshd down, the rest (hub) up
	botBroken    map[string]bool      // that sentinel's Slack bot token is rejected
	maint        map[string]*Window
	publicFail   map[[2]string]string // [from, to] -> why its public URLs fail from there
	fixOnRecover bool

	sent     []msg
	recovers []string
}

type msg struct{ from, text string }

const server = "server"

func newWorld(t *testing.T, mutate ...func(*Config)) *world {
	return newFleet(t, []string{"devbox", "office-box2", "laptop", server}, mutate...)
}

// newFleet is newWorld with these sentinels, in rank order.
func newFleet(t *testing.T, order []string, mutate ...func(*Config)) *world {
	w := &world{
		t: t, clock: &fakeClock{t: time.Date(2026, 10, 20, 7, 30, 0, 0, time.UTC)},
		order:   order,
		engines: map[string]*Engine{}, running: map[string]bool{},
		hostUp: map[string]bool{}, hubUp: map[string]bool{}, manager: map[string]string{},
		managerStart: map[string]int64{}, unitsDown: map[string][]string{}, netOK: map[string]bool{},
		blocked: map[[2]string]bool{}, via: map[[2]string]string{}, maint: map[string]*Window{},
		sshBroken: map[string]bool{}, botBroken: map[string]bool{}, publicFail: map[[2]string]string{},
	}
	root := t.TempDir()
	for _, name := range w.order {
		w.hostUp[name], w.hubUp[name], w.manager[name], w.netOK[name] = true, true, ManagerRunning, true
		w.managerStart[name] = 1792300000
		var refs []SentinelRef
		for _, n := range order {
			refs = append(refs, SentinelRef{Name: n})
		}
		cfg := &Config{
			Self:      name,
			Sentinels: refs,
			Targets: []Target{{
				Name: server,
				Hub: []Endpoint{
					{URL: "https://192.168.1.200:7443", Label: "LAN"},
					{URL: "https://100.64.0.163:7443", Label: "Tailscale"},
				},
				Units: []string{"pyre-hub", "pyred"},
			}},
			Slack:    SlackConfig{UserID: "U0TEST"},
			StateDir: filepath.Join(root, name),
		}
		for _, m := range mutate {
			m(cfg)
		}
		if err := cfg.Normalize(); err != nil {
			t.Fatal(err)
		}
		e := &Engine{Cfg: cfg, Clock: w.clock, Logf: func(string, ...any) {}}
		e.Prober = &fakeProber{w: w, self: name}
		e.Recoverer = &fakeRecoverer{w: w, self: name}
		e.Notifier = &fakeNotifier{w: w, self: name}
		w.engines[name] = e
		w.running[name] = true
	}
	return w
}

type fakeProber struct {
	w    *world
	self string
}

func (p *fakeProber) Network(context.Context) (bool, string) {
	if p.w.netOK[p.self] {
		return true, "gateway 192.168.1.1"
	}
	return false, "unreachable: gateway 192.168.1.1, 1.1.1.1:443"
}

func (p *fakeProber) Probe(_ context.Context, t Target, push []byte) Observation {
	w := p.w
	o := Observation{CheckedAt: w.clock.Now(), RemoteNow: w.clock.Now()}
	reach := w.hostUp[t.Name] && !w.blocked[[2]string{p.self, t.Name}]
	if hop := w.via[[2]string{p.self, t.Name}]; hop != "" && (!w.hostUp[hop] || w.sshBroken[hop]) {
		reach = false
	}
	for _, h := range t.Hub {
		c := Check{Label: h.name(), URL: h.URL, OK: reach && w.hubUp[t.Name]}
		if !c.OK {
			c.Detail = "timeout"
		}
		o.Hub = append(o.Hub, c)
	}
	for _, e := range t.Public { // through the internet, not the path to the host
		c := Check{Label: e.name(), URL: e.URL, OK: true}
		if why := w.publicFail[[2]string{p.self, t.Name}]; why != "" {
			c.OK, c.Detail = false, why
		}
		o.Public = append(o.Public, c)
	}
	if !reach || w.sshBroken[t.Name] {
		o.SSH, o.SSHDetail = SSHUnreachable, "Connection timed out"
		return o
	}
	o.SSH, o.OS = SSHOK, "Linux"
	o.Manager = w.manager[t.Name]
	if o.Manager == ManagerAbsent {
		o.ManagerRaw = "offline"
	} else {
		o.ManagerStart = "@" + strconv.FormatInt(w.managerStart[t.Name], 10)
	}
	o.UnitsDown = w.unitsDown[t.Name]
	o.Maintenance = w.maint[t.Name]
	if pe := w.engines[t.Name]; pe != nil {
		if st := pe.State(); st != nil && !st.UpdatedAt.IsZero() {
			o.PeerState, _ = json.Marshal(st)
		}
		if len(push) > 0 && w.running[t.Name] {
			dir := filepath.Join(pe.Cfg.StateDir, "peers")
			_ = os.MkdirAll(dir, 0o700)
			f := filepath.Join(dir, p.self+".json")
			_ = os.WriteFile(f, push, 0o600)
			_ = os.Chtimes(f, w.clock.Now(), w.clock.Now())
		}
	}
	return o
}

// CheckPublic is the sentinel's check of its own host's public URLs.
func (p *fakeProber) CheckPublic(_ context.Context, eps []Endpoint) []Check {
	var out []Check
	for _, e := range eps {
		c := Check{Label: e.name(), URL: e.URL, OK: true}
		if why := p.w.publicFail[[2]string{p.self, p.self}]; why != "" {
			c.OK, c.Detail = false, why
		}
		out = append(out, c)
	}
	return out
}

type fakeRecoverer struct {
	w    *world
	self string
}

func (r *fakeRecoverer) Recover(_ context.Context, t Target) (string, error) {
	r.w.recovers = append(r.w.recovers, r.self+"->"+t.Name)
	if r.w.fixOnRecover {
		r.w.manager[t.Name] = ManagerRunning
		r.w.hubUp[t.Name] = true
		r.w.managerStart[t.Name] = r.w.clock.Now().Unix()
		return "user manager running", nil
	}
	return "user manager offline", nil
}

type fakeNotifier struct {
	w    *world
	self string
}

func (n *fakeNotifier) Send(_ context.Context, _, text string) error {
	if n.w.botBroken[n.self] {
		return errors.New("slack chat.postMessage: invalid_auth")
	}
	n.w.sent = append(n.w.sent, msg{n.self, text})
	return nil
}

func (n *fakeNotifier) Check(context.Context) error {
	if n.w.botBroken[n.self] {
		return errors.New("slack auth.test: invalid_auth")
	}
	return nil
}

// step advances the clock one minute and ticks every running sentinel.
func (w *world) step() {
	w.clock.t = w.clock.t.Add(time.Minute)
	for _, name := range w.order {
		if !w.running[name] {
			continue
		}
		if err := w.engines[name].Tick(context.Background()); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *world) steps(n int) {
	for range n {
		w.step()
	}
}

// about returns the messages mentioning target.
func (w *world) about(target string) []msg {
	var out []msg
	for _, m := range w.sent {
		if strings.Contains(m.text, " "+target+" ") || strings.Contains(m.text, " "+target+"'s ") {
			out = append(out, m)
		}
	}
	return out
}

func (w *world) dump() string {
	var b strings.Builder
	for _, m := range w.sent {
		b.WriteString(m.from + ": " + m.text + "\n")
	}
	return b.String()
}

func TestDownNeedsGraceThenOneAlertRecoveryAndRecovered(t *testing.T) {
	w := newWorld(t)
	w.fixOnRecover = true
	w.steps(2) // healthy warm-up: every sentinel has a fresh state
	w.manager[server], w.hubUp[server] = ManagerAbsent, false
	downAt := w.clock.Now().Add(time.Minute)
	// The leader sees its own down vote at once and its peers' a tick later
	// (each reads the others' last state): quorum after one tick, then the
	// 3 minute grace.
	w.steps(4)
	if len(w.sent) != 0 {
		t.Fatalf("alerted before the 3 min grace:\n%s", w.dump())
	}
	w.step() // DOWN has held 3 minutes
	if len(w.sent) != 1 {
		t.Fatalf("want one alert, got:\n%s", w.dump())
	}
	m := w.sent[0]
	if m.from != "devbox" {
		t.Errorf("alert from %s, want the leader devbox", m.from)
	}
	want := "Pyre: server is DOWN since " + clockPT(downAt, w.clock.Now()) + " (confirmed by devbox, laptop, office-box2). Hub unreachable on LAN and Tailscale; ssh works, user manager not running (is-system-running: offline). Auto-recovery: ssh login (attempt 1 of 2) at " + clockPT(w.clock.Now(), w.clock.Now()) + ": user manager running."
	if m.text != want {
		t.Errorf("alert:\n got %q\nwant %q", m.text, want)
	}
	if len(w.recovers) != 1 || w.recovers[0] != "devbox->server" {
		t.Fatalf("recoveries %v, want one by the leader", w.recovers)
	}
	w.steps(10)
	if got := w.about(server); len(got) != 2 || !strings.HasPrefix(got[1].text, "Pyre: server recovered at ") {
		t.Fatalf("want alert + one recovered (no restart notice inside the incident), got:\n%s", w.dump())
	}
	if len(w.recovers) != 1 {
		t.Errorf("recoveries %v", w.recovers)
	}
	st := w.engines["office-box2"].State()
	if len(st.Incidents) != 0 || len(st.Closed) != 1 || !st.Closed[0].RecoveredSent {
		t.Errorf("non-leader mirror: incidents %v closed %v", st.Incidents, st.Closed)
	}
}

func TestOneSentinelsViewIsNotEnough(t *testing.T) {
	w := newWorld(t)
	w.blocked[[2]string{"office-box2", server}] = true // its Tailscale path broke
	w.steps(30)
	if len(w.sent) != 0 {
		t.Fatalf("a single sentinel's down vote alerted:\n%s", w.dump())
	}
	if l := w.engines["devbox"].State().Timers[server].Level; l != LevelOK {
		t.Errorf("level %s, want OK (1 down, 2 up)", l)
	}
}

func TestLoneLiveSentinelRaisesUnconfirmed(t *testing.T) {
	w := newWorld(t)
	w.steps(1)
	w.running["office-box2"], w.running["laptop"] = false, false
	w.hostUp[server] = false
	w.steps(4) // peers go stale after 3 min; DOWN then needs its own 3 min
	w.steps(3)
	got := w.about(server)
	if len(got) == 0 {
		t.Fatalf("no alert:\n%s", w.dump())
	}
	if !strings.Contains(got[0].text, "Pyre: server may be DOWN since") || !strings.Contains(got[0].text, "(unconfirmed: only devbox can check; office-box2, laptop not reporting)") {
		t.Errorf("unconfirmed alert: %q", got[0].text)
	}
	if !strings.Contains(got[0].text, "ssh fails (Connection timed out)") {
		t.Errorf("detail: %q", got[0].text)
	}
	for _, m := range got {
		if strings.Contains(m.text, "is DOWN since") {
			t.Errorf("a lone sentinel confirmed DOWN: %q", m.text)
		}
	}
	if len(w.recovers) != 0 {
		t.Errorf("recovered on an unconfirmed down: %v", w.recovers)
	}
}

func TestOwnNetworkFailureAbstainsAndHandsOverLeadership(t *testing.T) {
	w := newWorld(t)
	w.steps(1)
	w.netOK["devbox"] = false
	w.step()
	st := w.engines["devbox"].State()
	if v := st.Votes[server]; v.Status != StatusUnknown {
		t.Errorf("vote %s, want unknown", v.Status)
	}
	if got := w.engines["laptop"].State().Leader; got != "office-box2" {
		t.Errorf("leader %q, want office-box2 while devbox is offline", got)
	}
	w.steps(15)
	if got := w.about(server); len(got) != 0 {
		t.Errorf("alerts about a healthy server:\n%s", w.dump())
	}
}

func TestLeaderFailoverDoesNotRepeatAlerts(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	w.hubUp[server] = false
	w.unitsDown[server] = []string{"pyre-hub (failed)"}
	w.steps(5)
	if got := w.about(server); len(got) != 1 || got[0].from != "devbox" {
		t.Fatalf("first alert:\n%s", w.dump())
	}
	w.running["devbox"] = false // the leader dies
	w.steps(10)
	if got := w.about(server); len(got) != 1 {
		t.Fatalf("the new leader repeated the alert:\n%s", w.dump())
	}
	if l := w.engines["laptop"].State().Leader; l != "office-box2" {
		t.Fatalf("leader %q", l)
	}
	w.steps(20) // 30 min after the first alert
	got := w.about(server)
	if len(got) != 2 || got[1].from != "office-box2" || !strings.HasPrefix(got[1].text, "Pyre reminder: server is still DOWN (since ") {
		t.Fatalf("want one reminder from the new leader:\n%s", w.dump())
	}
	w.steps(15)
	if got := w.about(server); len(got) != 2 {
		t.Errorf("extra messages:\n%s", w.dump())
	}
	// The dead sentinel's host is reachable, so it is DEGRADED (sentinel
	// missing), alerted once.
	hn := w.about("devbox")
	if len(hn) == 0 || !strings.Contains(hn[0].text, "Pyre: devbox is DEGRADED since") || !strings.Contains(hn[0].text, "its sentinel is not reporting") {
		t.Errorf("sentinel-missing alert:\n%s", w.dump())
	}
}

func TestMaintenanceSuppressesEvenWhenHostVanishes(t *testing.T) {
	w := newWorld(t)
	w.maint[server] = &Window{Until: w.clock.Now().Add(20 * time.Minute), Reason: "upgrade.sh"}
	w.steps(2) // sentinels read and cache the window
	w.hostUp[server] = false
	w.steps(15)
	if len(w.sent) != 0 || len(w.recovers) != 0 {
		t.Fatalf("acted during maintenance:\n%s", w.dump())
	}
	w.steps(5) // window over, still down
	got := w.about(server)
	if len(got) != 1 || !strings.Contains(got[0].text, "server is DOWN since") {
		t.Fatalf("want the DOWN alert once the window ends:\n%s", w.dump())
	}
	if !strings.Contains(got[0].text, "Auto-recovery: none possible (ssh fails") {
		t.Errorf("recovery line: %q", got[0].text)
	}
}

func TestDegradedGraceAndRecovered(t *testing.T) {
	w := newWorld(t)
	w.steps(1)
	w.unitsDown[server] = []string{"db-tunnel-prod (inactive)"}
	w.steps(11)
	if len(w.sent) != 0 {
		t.Fatalf("degraded alerted before 10 min:\n%s", w.dump())
	}
	w.step()
	got := w.about(server)
	if len(got) != 1 || !strings.Contains(got[0].text, "server is DEGRADED since") ||
		!strings.Contains(got[0].text, "(seen by devbox, laptop, office-box2)") ||
		!strings.Contains(got[0].text, "not active: db-tunnel-prod (inactive)") {
		t.Fatalf("degraded alert:\n%s", w.dump())
	}
	if len(w.recovers) != 0 {
		t.Errorf("recovery on degraded: %v", w.recovers)
	}
	w.unitsDown[server] = nil
	w.steps(4)
	if got := w.about(server); len(got) != 2 || !strings.Contains(got[1].text, "recovered") {
		t.Fatalf("recovered:\n%s", w.dump())
	}
}

func TestRecoveryAtMostTwiceThenManualHelp(t *testing.T) {
	w := newWorld(t)
	w.steps(2)
	w.manager[server], w.hubUp[server] = ManagerAbsent, false
	w.steps(60)
	if len(w.recovers) != 2 {
		t.Fatalf("recoveries %v, want exactly 2", w.recovers)
	}
	got := w.dump()
	for _, want := range []string{"Pyre: server is DOWN since", "Pyre: server is still DOWN. Auto-recovery: ssh login (attempt 2 of 2)", "Pyre: server is still DOWN after 2 auto-recovery attempts", "Pyre reminder: server is still DOWN"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, w.dump())
		}
	}
	if n := len(w.about(server)); n != 4 {
		t.Errorf("%d messages, want 4 (alert, attempt 2, manual help, 30 min reminder):\n%s", n, w.dump())
	}
}

func TestRestartNoticeOnce(t *testing.T) {
	w := newWorld(t)
	w.steps(3)
	w.managerStart[server] = w.clock.Now().Unix() // restarted between probes (Restart=always or a probe login)
	w.steps(5)
	got := w.about(server)
	if len(got) != 1 || got[0].from != "devbox" || !strings.HasPrefix(got[0].text, "Pyre: server's systemd user manager restarted at ") {
		t.Fatalf("restart notice:\n%s", w.dump())
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	w.hubUp[server] = false
	w.steps(5)
	if len(w.about(server)) != 1 {
		t.Fatalf("%s", w.dump())
	}
	// Restart the leader process: a fresh Engine on the same state dir.
	old := w.engines["devbox"]
	e := &Engine{Cfg: old.Cfg, Clock: w.clock, Prober: old.Prober, Recoverer: old.Recoverer, Notifier: old.Notifier, Logf: old.Logf}
	w.engines["devbox"] = e
	w.steps(5)
	if got := w.about(server); len(got) != 1 {
		t.Errorf("restarted leader repeated the alert:\n%s", w.dump())
	}
}

func TestViaSuppressesExpectedSilence(t *testing.T) {
	// The real topology: the home sentinels reach office-box2 only through
	// server (ProxyJump); office-box2 reaches them through port forwards.
	w := newWorld(t, func(c *Config) {
		if c.Self == "devbox" || c.Self == "laptop" {
			for i := range c.Sentinels {
				if c.Sentinels[i].Name == "office-box2" {
					c.Sentinels[i].Via = server
				}
			}
		}
	})
	w.via[[2]string{"devbox", "office-box2"}] = server
	w.via[[2]string{"laptop", "office-box2"}] = server
	w.steps(2)
	w.hostUp[server] = false
	w.steps(15)
	for _, m := range w.sent {
		if strings.Contains(m.text, "office-box2 is") {
			t.Errorf("office-box2 reported although its only path is through the DOWN server: %q", m.text)
		}
		if m.from != "devbox" {
			t.Errorf("message from %s, want only the leader: %q", m.from, m.text)
		}
	}
	if got := w.about(server); len(got) != 1 || !strings.Contains(got[0].text, "server is DOWN since") {
		t.Errorf("server alert:\n%s", w.dump())
	}
	if l := w.engines["office-box2"].State().Leader; l != "devbox" {
		t.Errorf("office-box2 sees leader %q, want devbox (it pushes to and pulls from it directly)", l)
	}
}

func TestPushedStateMakesOneWayPeersLive(t *testing.T) {
	w := newWorld(t)
	// devbox cannot reach office-box2, but office-box2 reaches
	// devbox (a port forward) and pushes its state there.
	w.blocked[[2]string{"devbox", "office-box2"}] = true
	w.steps(3)
	live := w.engines["devbox"].State().Live
	if strings.Join(live, ",") != "devbox,office-box2,laptop,server" {
		t.Errorf("live %v", live)
	}
}

func TestLeadersOwnHostIsHandledByTheNextSentinel(t *testing.T) {
	w := newWorld(t)
	w.steps(2)
	// devbox (the leader) is really down: its host and its sentinel.
	w.hostUp["devbox"], w.running["devbox"] = false, false
	w.steps(9) // its last state goes stale (3 min), then quorum and the 3 min grace
	got := w.about("devbox")
	if len(got) != 1 || got[0].from != "office-box2" || !strings.Contains(got[0].text, "Pyre: devbox is DOWN since") {
		t.Fatalf("want one DOWN alert about the leader's host from office-box2:\n%s", w.dump())
	}
	w.hostUp["devbox"], w.running["devbox"] = true, true
	w.steps(5)
	if l := w.engines["devbox"].State().Leader; l != "devbox" {
		t.Fatalf("leader %q after it came back", l)
	}
	got = w.about("devbox")
	if len(got) != 2 || got[1].from != "office-box2" || !strings.Contains(got[1].text, "devbox recovered") {
		t.Fatalf("want one recovered from office-box2 (it still acts for devbox's host):\n%s", w.dump())
	}
	for _, m := range w.sent {
		if m.from == "devbox" && strings.Contains(m.text, "devbox") {
			t.Errorf("a sentinel acted on its own host: %q", m.text)
		}
	}
}

func TestReportingSentinelIsNotDownOverAPathProblem(t *testing.T) {
	w := newWorld(t)
	w.steps(2)
	// Nobody can ssh to devbox, but its sentinel keeps pushing fresh
	// state: a path problem, DEGRADED at most, never DOWN.
	for _, from := range []string{"office-box2", "laptop", server} {
		w.blocked[[2]string{from, "devbox"}] = true
	}
	w.steps(15)
	got := w.about("devbox")
	if len(got) != 1 || got[0].from != "office-box2" || !strings.Contains(got[0].text, "devbox is DEGRADED since") {
		t.Fatalf("want one DEGRADED alert from office-box2:\n%s", w.dump())
	}
	for _, m := range w.sent {
		if strings.Contains(m.text, "DOWN") {
			t.Errorf("DOWN over a path problem: %q", m.text)
		}
	}
	if v := w.engines["office-box2"].State().Votes["devbox"]; v.Status != StatusDegraded || !strings.Contains(v.Reasons[0], "no ssh path from office-box2") {
		t.Errorf("vote %+v", v)
	}
}

func TestAbstainingVotesDoNotResolve(t *testing.T) {
	// T is DOWN and alerted; then the only path to it (via server) breaks too:
	// every vote on T becomes unknown. That is not "recovered".
	w := newWorld(t, func(c *Config) {
		c.Targets[0].NoRecovery = true
		c.Targets = append(c.Targets, Target{Name: "lab", Hub: []Endpoint{{URL: "https://lab:7443", Label: "LAN"}}, Via: server})
	})
	w.order = append(w.order, "lab")
	w.hostUp["lab"], w.hubUp["lab"], w.manager["lab"], w.netOK["lab"] = true, true, ManagerRunning, true
	w.steps(2)
	w.hubUp["lab"] = false
	w.steps(5)
	if got := w.about("lab"); len(got) != 1 || !strings.Contains(got[0].text, "lab is DOWN since") {
		t.Fatalf("lab alert:\n%s", w.dump())
	}
	w.hostUp[server] = false
	for _, from := range w.order {
		w.via[[2]string{from, "lab"}] = server
	}
	w.steps(20)
	for _, m := range w.about("lab") {
		if strings.Contains(m.text, "recovered") {
			t.Fatalf("abstaining votes resolved the incident:\n%s", w.dump())
		}
	}
	if inc := w.engines["devbox"].State().Incidents["lab"]; inc == nil || !inc.Resolved.IsZero() {
		t.Errorf("lab incident %+v, want still open", inc)
	}
}

func TestReturningLeaderDropsAnIncidentAPeerClosed(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	w.hubUp[server] = false
	w.steps(5)
	if len(w.about(server)) != 1 {
		t.Fatalf("%s", w.dump())
	}
	w.running["devbox"] = false // leader away; office-box2 takes over
	w.steps(5)
	w.hubUp[server] = true // server recovers while devbox is away
	w.steps(4)
	if got := w.about(server); len(got) != 2 || got[1].from != "office-box2" || !strings.Contains(got[1].text, "recovered") {
		t.Fatalf("recovered from the stand-in leader:\n%s", w.dump())
	}
	w.running["devbox"] = true // back, with its stale open incident
	w.steps(5)
	if got := w.about(server); len(got) != 2 {
		t.Errorf("the returning leader repeated a message:\n%s", w.dump())
	}
	if inc := w.engines["devbox"].State().Incidents[server]; inc != nil {
		t.Errorf("returning leader kept incident %+v", inc)
	}
}

func TestUndeliverableSentinelIsSkippedAsActor(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.botBroken["devbox"] = true // its token was revoked
	w.steps(2)
	if st := w.engines["devbox"].State(); st.NotifyErr == "" {
		t.Fatal("token check failure not recorded")
	}
	w.hubUp[server] = false
	w.steps(6)
	got := w.about(server)
	if len(got) != 1 || got[0].from != "office-box2" || !strings.Contains(got[0].text, "server is DOWN since") {
		t.Fatalf("want the alert from office-box2 (devbox cannot deliver):\n%s", w.dump())
	}
	// Once devbox can deliver again (next token check), it takes over
	// without repeating the alert.
	w.botBroken["devbox"] = false
	w.engines["devbox"].lastCheck = time.Time{}
	w.steps(3)
	if got := w.about(server); len(got) != 1 {
		t.Errorf("repeated after the actor changed back:\n%s", w.dump())
	}
	if inc := w.engines["devbox"].State().Incidents[server]; inc == nil || inc.Owner != "devbox" || !alerted(inc) {
		t.Errorf("devbox incident %+v", inc)
	}
}

func TestFailedSendIsRetriedNotMarkedSent(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	w.hubUp[server] = false
	w.steps(4)
	w.botBroken["devbox"] = true // breaks right before the alert
	w.step()
	if inc := w.engines["devbox"].State().Incidents[server]; inc == nil || alerted(inc) {
		t.Fatalf("a failed send was marked sent: %+v", inc)
	}
	w.step() // office-box2 now acts (devbox reported it cannot deliver)
	if got := w.about(server); len(got) != 1 || got[0].from != "office-box2" {
		t.Fatalf("want the alert delivered by office-box2:\n%s", w.dump())
	}
}

func TestCrashLoopingHubStillAlerts(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	for i := range 12 { // down two minutes, up one, repeatedly
		w.hubUp[server] = i%3 == 2
		w.step()
	}
	if got := w.about(server); len(got) == 0 || !strings.Contains(got[0].text, "server is DOWN since") {
		t.Fatalf("a crash-looping hub never alerted:\n%s", w.dump())
	}
}

func TestViaFailingThisTickSuppressesThePath(t *testing.T) {
	w := newWorld(t, func(c *Config) {
		if c.Self == "devbox" || c.Self == "laptop" {
			for i := range c.Sentinels {
				if c.Sentinels[i].Name == "office-box2" {
					c.Sentinels[i].Via = server
				}
			}
		}
	})
	w.via[[2]string{"devbox", "office-box2"}] = server
	w.via[[2]string{"laptop", "office-box2"}] = server
	w.running["office-box2"] = false // and it cannot push either
	w.steps(2)
	w.sshBroken[server] = true // server's sshd stops; its hub stays up (DEGRADED, not DOWN)
	w.steps(15)
	for _, m := range w.sent {
		if strings.Contains(m.text, "office-box2 is") {
			t.Errorf("office-box2 reported over the broken jump host: %q", m.text)
		}
	}
}

func TestRestartNoticeRateLimited(t *testing.T) {
	w := newWorld(t)
	w.steps(3)
	for range 70 { // linger off: every probe login restarts the manager
		w.managerStart[server] = w.clock.Now().Unix()
		w.step()
	}
	got := w.about(server)
	if len(got) != 2 {
		t.Fatalf("want 2 restart notices in 70 min (one per hour), got:\n%s", w.dump())
	}
	if !strings.Contains(got[1].text, "more time(s) in the past hour") {
		t.Errorf("second notice does not count the muted restarts: %q", got[1].text)
	}
}

func TestFlappingTargetIsOneIncident(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	for range 12 { // down 6 min, up 4 min, for two hours
		w.hubUp[server] = false
		w.steps(6)
		w.hubUp[server] = true
		w.steps(4)
	}
	got := w.about(server)
	t.Logf("flapping messages:\n%s", w.dump())
	// first alert, recovered, failing again, reminders at 30 min and 2 h.
	if len(got) > 5 {
		t.Fatalf("%d messages for one flapping problem:\n%s", len(got), w.dump())
	}
	if !strings.Contains(w.dump(), "again since") {
		t.Errorf("no flapping message:\n%s", w.dump())
	}
	w.steps(35) // stable for 30 min: one "recovered"
	if n := strings.Count(w.dump(), "server recovered"); n != 2 {
		t.Errorf("%d recovered messages, want 2 (before and after flapping):\n%s", n, w.dump())
	}
}

func TestNoReminderOrManualHelpOnceTheTargetIsBack(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	w.hubUp[server] = false
	w.steps(5) // first alert at 7:37; the 30 min reminder is due at 8:07
	if len(w.about(server)) != 1 {
		t.Fatalf("%s", w.dump())
	}
	w.steps(28)            // to 8:05
	w.hubUp[server] = true // back at 8:06, inside the 2 min clear window at 8:07
	w.steps(5)
	got := w.about(server)
	if len(got) != 2 || !strings.Contains(got[1].text, "recovered") {
		t.Errorf("want alert + recovered, no reminder while already up:\n%s", w.dump())
	}

	// Manual help: two failed attempts, then the host comes back just before
	// the "needs manual help" message would go out.
	w2 := newWorld(t)
	w2.steps(2)
	w2.manager[server], w2.hubUp[server] = ManagerAbsent, false
	after := -1
	for i := range 40 {
		w2.step()
		if len(w2.recovers) == 2 && after < 0 {
			after = i
		}
		// Back 2 min after attempt 2: manual help is due at +4 min, when the
		// confirmed level is still DOWN (2 min clear window) but the round
		// itself is good.
		if after >= 0 && i == after+2 {
			w2.manager[server], w2.hubUp[server] = ManagerRunning, true
		}
	}
	if strings.Contains(w2.dump(), "Needs manual help") {
		t.Errorf("manual help sent while the host was back:\n%s", w2.dump())
	}
}

func TestGossipCarriesDeliveryStatus(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	// office-box2 and devbox cannot reach each other at all; each
	// knows the other only through laptop and server. devbox's bot
	// token is revoked.
	w.blocked[[2]string{"office-box2", "devbox"}] = true
	w.blocked[[2]string{"devbox", "office-box2"}] = true
	w.botBroken["devbox"] = true
	w.steps(3)
	w.hubUp[server] = false
	w.steps(8)
	got := w.about(server)
	if len(got) != 1 || got[0].from != "office-box2" {
		t.Fatalf("want exactly one alert, from office-box2 (the gossip-only leader cannot deliver):\n%s", w.dump())
	}
}

func TestGossipedLivenessExpires(t *testing.T) {
	w := newWorld(t)
	w.blocked[[2]string{"office-box2", "devbox"}] = true
	w.blocked[[2]string{"devbox", "office-box2"}] = true
	w.steps(3)
	if l := w.engines["office-box2"].State().Leader; l != "devbox" {
		t.Fatalf("leader through gossip %q", l)
	}
	w.running["devbox"] = false
	n := 0
	for ; n < 10 && w.engines["office-box2"].State().Leader == "devbox"; n++ {
		w.step()
	}
	// Direct staleness is 3 min; gossip must not stretch it much further.
	if n > 4 {
		t.Errorf("a dead leader stayed known through gossip for %d min", n)
	}
}
