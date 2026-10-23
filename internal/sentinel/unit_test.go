package sentinel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMessagesArePacificAndSelfContained(t *testing.T) {
	// 07:35 UTC on Oct 20 2026 is 12:35 AM PDT.
	since := time.Date(2026, 10, 20, 7, 35, 0, 0, time.UTC)
	now := time.Date(2026, 10, 20, 7, 38, 0, 0, time.UTC)
	o := &Observation{
		Hub:     []Check{{Label: "LAN"}, {Label: "Tailscale"}},
		SSH:     SSHOK,
		Manager: ManagerAbsent,
	}
	c := Cond{Level: LevelDown, DownBy: []string{"devbox", "office-box2"}}
	att := &RecoveryAttempt{At: now}
	got := alertText("server", LevelDown, since, c, o, recoveryLine(Target{Name: "server"}, o, att, 1, 2, now), now)
	want := "Pyre: server is DOWN since 12:35 AM PT (confirmed by devbox, office-box2). Hub unreachable on LAN and Tailscale; ssh works, user manager not running. Auto-recovery: ssh login (attempt 1 of 2) at 12:38 AM PT."
	if got != want {
		t.Errorf("alert\n got %q\nwant %q", got, want)
	}

	// Winter: PST, and another day gets a date.
	jan := time.Date(2026, 1, 5, 17, 4, 0, 0, time.UTC) // 9:04 AM PST
	if got := clockPT(jan, jan); got != "9:04 AM PT" {
		t.Errorf("PST: %q", got)
	}
	if got := clockPT(jan, jan.Add(48*time.Hour)); got != "Jan 5, 9:04 AM PT" {
		t.Errorf("other day: %q", got)
	}
	inc := &Incident{ID: "server-20261020T0735Z", Target: "server", Level: LevelDown, Since: since, Recovery: []RecoveryAttempt{{At: now}}}
	if got := recoveredText(inc, now.Add(3*time.Minute)); got != "Pyre: server recovered at 12:41 AM PT (DOWN since 12:35 AM PT, 6 min). Auto-recovery: 1 ssh login(s), last at 12:38 AM PT. Incident server-20261020T0735Z." {
		t.Errorf("recovered: %q", got)
	}
	if got := reminderText("server", LevelDown, since, c, o, since.Add(2*time.Hour+5*time.Minute)); !strings.HasPrefix(got, "Pyre reminder: server is still DOWN (since 12:35 AM PT, 2 h 5 min). Hub unreachable") {
		t.Errorf("reminder: %q", got)
	}
	u := Cond{Level: LevelUnconfirmed, DownBy: []string{"office-box2"}, Missing: []string{"devbox", "laptop"}}
	if got := alertText("server", LevelUnconfirmed, since, u, &Observation{SSH: SSHUnreachable, SSHDetail: "Connection timed out"}, "", now); got != "Pyre: server may be DOWN since 12:35 AM PT (unconfirmed: only office-box2 can check; devbox, laptop not reporting). ssh fails (Connection timed out)." {
		t.Errorf("unconfirmed: %q", got)
	}
	if got := restartText("server", parseUnixStamp("@1792505198"), time.Time{}, 0, time.Unix(1792505198, 0)); !strings.HasPrefix(got, "Pyre: server's systemd user manager restarted at 7:06 AM PT.") {
		t.Errorf("restart: %q", got)
	}
}

func TestClassify(t *testing.T) {
	hubDown := []Check{{Label: "LAN"}, {Label: "Tailscale"}}
	hubUp := []Check{{Label: "LAN", OK: true}, {Label: "Tailscale"}}
	for _, tc := range []struct {
		name string
		o    Observation
		want Status
	}{
		{"healthy", Observation{Hub: hubUp, SSH: SSHOK, Manager: ManagerRunning}, StatusUp},
		{"one hub path is enough", Observation{Hub: hubUp, SSH: SSHOK}, StatusUp},
		{"hub down", Observation{Hub: hubDown, SSH: SSHOK, Manager: ManagerRunning}, StatusDown},
		{"manager absent", Observation{Hub: hubUp, SSH: SSHOK, Manager: ManagerAbsent}, StatusDown},
		{"host gone", Observation{Hub: hubDown, SSH: SSHUnreachable}, StatusDown},
		{"no hub, ssh gone", Observation{SSH: SSHUnreachable}, StatusDown},
		{"ssh gone, hub up", Observation{Hub: hubUp, SSH: SSHUnreachable}, StatusDegraded},
		{"key refused, no hub", Observation{SSH: SSHAuth}, StatusUnknown},
		{"key refused, hub up", Observation{Hub: hubUp, SSH: SSHAuth}, StatusUp},
		{"unit down", Observation{Hub: hubUp, SSH: SSHOK, UnitsDown: []string{"db-tunnel-prod (inactive)"}}, StatusDegraded},
		{"port closed", Observation{SSH: SSHOK, TCPDown: []string{"127.0.0.1:5431"}}, StatusDegraded},
		{"tunnel offline", Observation{SSH: SSHOK, Public: []Check{{URL: "https://x.ngrok.app", Detail: "ngrok ERR_NGROK_3200"}}}, StatusDegraded},
		{"mac host", Observation{SSH: SSHOK, Manager: ManagerNA}, StatusUp},
	} {
		if got := classify(tc.o).Status; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestEvaluateQuorum(t *testing.T) {
	v := func(s Status) *Vote { return &Vote{Status: s} }
	for _, tc := range []struct {
		name    string
		votes   map[string]*Vote
		missing bool
		want    Level
	}{
		{"two down", map[string]*Vote{"a": v(StatusDown), "b": v(StatusDown), "c": v(StatusUp)}, false, LevelDown},
		{"one of three", map[string]*Vote{"a": v(StatusDown), "b": v(StatusUp), "c": v(StatusUp)}, false, LevelOK},
		{"one of two", map[string]*Vote{"a": v(StatusDown), "b": v(StatusUp)}, false, LevelOK},
		{"lone live", map[string]*Vote{"a": v(StatusDown), "b": v(StatusUnknown)}, false, LevelUnconfirmed},
		{"target's own vote ignored", map[string]*Vote{"a": v(StatusDown), "t": v(StatusDown)}, false, LevelUnconfirmed},
		{"degraded needs two", map[string]*Vote{"a": v(StatusDegraded), "b": v(StatusUp)}, false, LevelOK},
		{"degraded two", map[string]*Vote{"a": v(StatusDegraded), "b": v(StatusDown), "c": v(StatusUp)}, false, LevelDegraded},
		{"degraded lone", map[string]*Vote{"a": v(StatusDegraded)}, false, LevelDegraded},
		{"sentinel missing", map[string]*Vote{"a": v(StatusUp)}, true, LevelDegraded},
		{"nobody", map[string]*Vote{"a": v(StatusUnknown)}, false, LevelOK},
	} {
		if got := evaluate("t", tc.votes, nil, tc.missing).Level; got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestParseProbeAndManager(t *testing.T) {
	out := "@@os Linux\n@@manager\noffline\n@@managerstart\n\n@@units\npyre-hub inactive\npyred active\ndb-tunnel-prod failed\n@@tcp\n127.0.0.1:5431 closed\n@@maintenance\n{\"until\":\"2026-10-20T08:00:00Z\",\"reason\":\"upgrade.sh\"}\n\n@@sentinel\n{\"sentinel\":\"server\"}\n@@end\n"
	p := parseProbe(out)
	if !p.complete || p.os != "Linux" || managerState(p.os, p.managerRaw) != ManagerAbsent {
		t.Fatalf("%+v", p)
	}
	if strings.Join(p.unitsDown, ",") != "pyre-hub (inactive),db-tunnel-prod (failed)" || strings.Join(p.tcpDown, ",") != "127.0.0.1:5431" {
		t.Errorf("units %v tcp %v", p.unitsDown, p.tcpDown)
	}
	w, err := parseWindow(p.maintenance)
	if err != nil || w.Reason != "upgrade.sh" {
		t.Errorf("window %v %v", w, err)
	}
	if string(p.sentinel) != `{"sentinel":"server"}` {
		t.Errorf("sentinel %q", p.sentinel)
	}
	if parseProbe("@@os Linux\n@@manager\n").complete {
		t.Error("a cut-off probe is complete")
	}
	for raw, want := range map[string]string{"running": ManagerRunning, "degraded": ManagerRunning, "offline": ManagerAbsent, "Failed to connect to bus: No medium found": ManagerAbsent} {
		if got := managerState("Linux", raw); got != want {
			t.Errorf("%q: %s", raw, got)
		}
	}
	if managerState("Darwin", "") != ManagerNA {
		t.Error("darwin")
	}
}

func TestClassifySSH(t *testing.T) {
	for stderr, want := range map[string]string{
		"ssh: connect to host 192.168.1.200 port 22: Connection timed out":  SSHUnreachable,
		"ssh: connect to host server port 22: No route to host":             SSHUnreachable,
		"ssh: Could not resolve hostname server: Name or service not known": SSHUnreachable,
		"Host key verification failed.":                                     SSHAuth,
		"jhh3@server: Permission denied (publickey).":                       SSHAuth,
		"something odd": SSHError,
	} {
		if got, _ := classifySSH(stderr, nil); got != want {
			t.Errorf("%q: %s, want %s", stderr, got, want)
		}
	}
	if _, d := classifySSH("ssh: connect to host 192.168.1.200 port 22: Connection timed out", nil); d != "Connection timed out" {
		t.Errorf("detail %q", d)
	}
}

func TestGatewayFromProcRoute(t *testing.T) {
	route := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"eth0\t0001A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"
	if got := gatewayFromProcRoute(route); got != "192.168.1.1" {
		t.Errorf("gateway %q", got)
	}
}

// TestProbeScriptRuns runs the generated script under /bin/sh with a fake
// uname (so no systemctl or launchctl is touched) and a temporary HOME: it
// must parse, read the maintenance and sentinel files, and push our state.
func TestProbeScriptRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh")
	}
	home, bin := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "uname"), []byte("#!/bin/sh\necho Plan9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []struct{ path, body string }{
		{remoteMaintenanceFile, `{"until":"2099-01-01T00:00:00Z","reason":"test"}`},
		{remoteStateFile, `{"sentinel":"peer","updated_at":"2026-10-20T07:00:00Z"}`},
	} {
		p := filepath.Join(home, f.path)
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		if err := os.WriteFile(p, []byte(f.body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	push := []byte(`{"sentinel":"devbox","note":"it's \"quoted\" $HOME` + "`x`" + `"}`)
	script := probeScript(Target{Name: "peer", TCP: []string{"127.0.0.1:1"}}, "devbox", push)
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"HOME=" + home, "PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	p := parseProbe(string(out))
	if !p.complete || p.os != "Plan9" {
		t.Fatalf("parse: %+v\n%s", p, out)
	}
	if w, _ := parseWindow(p.maintenance); w == nil || w.Reason != "test" {
		t.Errorf("maintenance %q", p.maintenance)
	}
	if !strings.Contains(string(p.sentinel), `"sentinel":"peer"`) {
		t.Errorf("sentinel %q", p.sentinel)
	}
	if len(p.tcpDown) != 1 {
		t.Errorf("tcp %v", p.tcpDown)
	}
	got, err := os.ReadFile(filepath.Join(home, remotePeersDir, "devbox.json"))
	if err != nil || strings.TrimSpace(string(got)) != string(push) {
		t.Errorf("pushed %q (%v)", got, err)
	}
	if fi, err := os.Stat(filepath.Join(home, remotePeersDir, "devbox.json")); err == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("pushed file mode %v", fi.Mode().Perm())
	}
	// No sentinel state dir on the target: nothing is created there.
	home2 := t.TempDir()
	cmd = exec.Command("/bin/sh", "-s")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"HOME=" + home2, "PATH=" + bin + ":/usr/bin:/bin"}
	if out, err := cmd.Output(); err != nil || !parseProbe(string(out)).complete {
		t.Fatalf("%v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home2, ".local")); !os.IsNotExist(err) {
		t.Errorf("probe created files on a host without a sentinel")
	}
}

func TestConfigNormalize(t *testing.T) {
	c := &Config{
		Self:      "devbox",
		Sentinels: []SentinelRef{{Name: "devbox"}, {Name: "office-box2", SSH: []string{"office-box2", "office-box2-via-server"}, Via: "server"}, {Name: "server"}},
		Targets:   []Target{{Name: "server", Units: []string{"pyre-hub"}}},
		Slack:     SlackConfig{UserID: "U1"},
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tg := range c.watched() {
		names = append(names, tg.Name)
	}
	if strings.Join(names, ",") != "server,office-box2" {
		t.Errorf("watched %v", names)
	}
	n2, _ := c.target("office-box2")
	if n2.Via != "server" || len(n2.SSH) != 2 {
		t.Errorf("implicit target %+v", n2)
	}
	if c.Timing.DownGrace.D() != 3*time.Minute || c.Timing.DegradedGrace.D() != 10*time.Minute || c.Timing.MaxRecoveryAttempts != 2 {
		t.Errorf("defaults %+v", c.Timing)
	}
	bad := *c
	bad.Targets = []Target{{Name: "server", Units: []string{"pyre-hub; rm -rf ~"}}}
	if err := bad.Normalize(); err == nil {
		t.Error("accepted a unit name with shell syntax")
	}
	bad = Config{Self: "x", Sentinels: []SentinelRef{{Name: "y"}}, Slack: SlackConfig{UserID: "U"}}
	if err := bad.Normalize(); err == nil {
		t.Error("accepted self outside sentinels")
	}
}

func TestLoadConfigRefusesReadableFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits")
	}
	p := filepath.Join(t.TempDir(), "sentinel.json")
	b, _ := json.Marshal(Config{Self: "a", Sentinels: []SentinelRef{{Name: "a"}}, Slack: SlackConfig{UserID: "U"}})
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("0644 config: %v", err)
	}
	_ = os.Chmod(p, 0o600)
	if _, err := LoadConfig(p); err != nil {
		t.Errorf("0600 config: %v", err)
	}
}

func TestMaintenanceWriteRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pyre", "maintenance.json")
	now := time.Date(2026, 10, 20, 16, 0, 0, 0, time.UTC)
	if _, err := WriteMaintenance(p, now, 10*time.Minute, "upgrade.sh"); err != nil {
		t.Fatal(err)
	}
	w, err := ReadMaintenance(p)
	if err != nil || !w.Active(now.Add(9*time.Minute)) || w.Active(now.Add(10*time.Minute)) || w.Reason != "upgrade.sh" {
		t.Errorf("%+v %v", w, err)
	}
	if err := ClearMaintenance(p); err != nil {
		t.Fatal(err)
	}
	if w, _ := ReadMaintenance(p); w != nil {
		t.Errorf("cleared: %+v", w)
	}
}

func TestSlackBotDM(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer xoxb-synthetic-test-token" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.URL.Path+" "+string(b))
		switch r.URL.Path {
		case "/conversations.open":
			io.WriteString(w, `{"ok":true,"channel":{"id":"D0SYNTH"}}`)
		case "/chat.postMessage":
			io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("xoxb-synthetic-test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(TokenEnv, "")
	n := &SlackNotifier{Cfg: SlackConfig{UserID: "U0SYNTH", BotTokenFile: tok, APIBase: srv.URL}}
	for range 2 {
		if err := n.Send(context.Background(), "test", "Pyre sentinel test — ignore"); err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 3 || !strings.Contains(calls[0], `"users":"U0SYNTH"`) || !strings.Contains(calls[1], `"channel":"D0SYNTH"`) || !strings.Contains(calls[1], "Pyre sentinel test") {
		t.Errorf("calls %v", calls)
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tok, 0o644)
		n2 := &SlackNotifier{Cfg: SlackConfig{UserID: "U0SYNTH", BotTokenFile: tok, APIBase: srv.URL}}
		if err := n2.Send(context.Background(), "k", "x"); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("readable token file: %v", err)
		}
	}
}

func TestSlackFallsBackToAgentSlack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	fake := filepath.Join(dir, "agent-slack")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s|' \"$@\" > '"+log+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(TokenEnv, "")
	n := &SlackNotifier{Cfg: SlackConfig{UserID: "U0SYNTH", AgentSlack: fake}}
	// The bot cannot send: agent-slack records the text (it does not notify
	// the owner), and Send still fails so the caller retries the bot.
	if err := n.Send(context.Background(), "server-20261020T0735Z/alert/DOWN/0", "hello at 12:38 AM PT"); err == nil || !strings.Contains(err.Error(), "recorded through agent-slack") {
		t.Fatalf("Send: %v", err)
	}
	if b, _ := os.ReadFile(log); string(b) != "message|send|U0SYNTH|hello at 12:38 AM PT|" {
		t.Errorf("agent-slack args %q", b)
	}
	_ = os.Remove(log)
	// The retry next round has new times in its text but the same key.
	if err := n.Send(context.Background(), "server-20261020T0735Z/alert/DOWN/0", "hello at 12:39 AM PT"); err == nil {
		t.Fatal("retry reported success")
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("the same message was recorded through agent-slack twice")
	}
	// Another kind of message for the incident is recorded.
	_ = n.Send(context.Background(), "server-20261020T0735Z/reminder/0/0", "reminder")
	if _, err := os.Stat(log); err != nil {
		t.Error("a different message kind was not recorded")
	}
}

func TestSlackUnknownOutcomeDoesNotFallBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/conversations.open" {
			io.WriteString(w, `{"ok":true,"channel":{"id":"D0SYNTH"}}`)
			return
		}
		time.Sleep(300 * time.Millisecond) // accepted, but the answer is late
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	fake := filepath.Join(dir, "agent-slack")
	_ = os.WriteFile(fake, []byte("#!/bin/sh\ntouch '"+log+"'\n"), 0o755)
	t.Setenv(TokenEnv, "xoxb-synthetic-test-token")
	n := &SlackNotifier{Cfg: SlackConfig{UserID: "U0SYNTH", APIBase: srv.URL, AgentSlack: fake}, HTTP: &http.Client{Timeout: 100 * time.Millisecond}}
	if err := n.Send(context.Background(), "k", "x"); err == nil {
		t.Fatal("a timed-out post reported success")
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("fell back to agent-slack after a post whose outcome is unknown")
	}
}

// TestProbeThatHangsIsNotHostOff: ssh connected (the script printed @@os)
// but never finished: "connected, probe hung", DEGRADED, not "host off".
func TestProbeThatHangsIsNotHostOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh script")
	}
	fake := filepath.Join(t.TempDir(), "ssh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '@@os Linux\\n@@now 1792505198\\n@@manager\\n'\necho 'Timeout, server not responding.' >&2\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &SSHProber{Self: "devbox", SSHBin: fake}
	o := p.Probe(context.Background(), Target{Name: "server", SSH: []string{"server"}}, nil)
	if o.SSH != SSHHung {
		t.Fatalf("ssh %q (%s), want hung", o.SSH, o.SSHDetail)
	}
	v := classify(o)
	if v.Status != StatusDegraded {
		t.Errorf("vote %s, want degraded", v.Status)
	}
	if got := describe(&o, Cond{}); !strings.Contains(got, "ssh connects but the probe hung") {
		t.Errorf("describe %q", got)
	}
	if got := recoveryLine(Target{Name: "server"}, &o, nil, 0, 2, time.Now()); !strings.Contains(got, "systemd is not answering") {
		t.Errorf("recovery line %q", got)
	}
}

// TestProbeScriptBoundsSystemctl: a systemctl that does not answer is cut
// off by `timeout 5` and reported as "timeout" (fake timeout and systemctl
// on PATH; nothing real is called).
func TestProbeScriptBoundsSystemctl(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh")
	}
	bin := t.TempDir()
	for name, body := range map[string]string{
		"uname":     "#!/bin/sh\necho Linux\n",
		"timeout":   "#!/bin/sh\nexit 124\n", // as if the wrapped command hung for 5 s
		"systemctl": "#!/bin/sh\nsleep 30\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Stdin = strings.NewReader(probeScript(Target{Name: "server", Units: []string{"pyre-hub"}}, "x", nil))
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	p := parseProbe(string(out))
	if !p.complete || managerState(p.os, p.managerRaw) != ManagerTimeout || len(p.unitsDown) != 1 || p.unitsDown[0] != "pyre-hub (timeout)" {
		t.Fatalf("%+v\n%s", p, out)
	}
	if p.remoteNow.IsZero() {
		t.Error("no @@now")
	}
	if v := classify(Observation{SSH: SSHOK, OS: "Linux", Manager: ManagerTimeout}); v.Status != StatusDegraded {
		t.Errorf("manager timeout vote %s", v.Status)
	}
}

func TestTimerHysteresisAndGap(t *testing.T) {
	tm := &Timer{}
	t0 := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	tg := Timing{DownGrace: Duration(3 * time.Minute), DegradedGrace: Duration(10 * time.Minute)}
	tm.update(LevelDown, at(0), 2*time.Minute, 3*time.Minute)
	tm.update(LevelOK, at(1), 2*time.Minute, 3*time.Minute) // one good tick
	tm.update(LevelDown, at(2), 2*time.Minute, 3*time.Minute)
	if got := tm.confirmed(at(3), tg); got != LevelDown {
		t.Errorf("one good tick reset the grace: %s", got)
	}
	tm.update(LevelOK, at(4), 2*time.Minute, 3*time.Minute)
	tm.update(LevelOK, at(5), 2*time.Minute, 3*time.Minute)
	tm.update(LevelOK, at(6), 2*time.Minute, 3*time.Minute)
	if !tm.DownSince.IsZero() || tm.confirmed(at(6), tg) != LevelOK {
		t.Errorf("not cleared after 2 min OK: %+v", tm)
	}
	// A long gap (abstaining or stopped): the old DOWN start is not reused.
	tm.update(LevelDown, at(7), 2*time.Minute, 3*time.Minute)
	tm.update(LevelDown, at(30), 2*time.Minute, 3*time.Minute)
	if !tm.DownSince.Equal(at(30)) || tm.confirmed(at(31), tg) != LevelOK {
		t.Errorf("gap did not restart the timer: %+v", tm)
	}
}

func TestPeerAgeUsesOneClock(t *testing.T) {
	now := time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)
	run := func(stamp, remoteNow time.Time) []string {
		cfg := &Config{Self: "a", Sentinels: []SentinelRef{{Name: "a"}, {Name: "b"}}, Slack: SlackConfig{UserID: "U"}, StateDir: t.TempDir()}
		if err := cfg.Normalize(); err != nil {
			t.Fatal(err)
		}
		peer, _ := json.Marshal(&State{Version: 1, Sentinel: "b", UpdatedAt: stamp, NetworkOK: true})
		e := &Engine{Cfg: cfg, Clock: &fakeClock{t: now}, Notifier: &nopNotifier{}, Logf: func(string, ...any) {}, DryRun: true,
			Prober: &stubProber{o: Observation{SSH: SSHOK, OS: "Linux", Manager: ManagerRunning, PeerState: peer, RemoteNow: remoteNow}}}
		if err := e.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		return e.State().Live
	}
	// b's clock runs 10 min ahead: its fresh state is still live.
	if live := run(now.Add(10*time.Minute), now.Add(10*time.Minute+30*time.Second)); strings.Join(live, ",") != "a,b" {
		t.Errorf("skewed but fresh peer: live %v", live)
	}
	// A stamp in the future of the peer's own clock is not trusted.
	if live := run(now.Add(10*time.Minute), now); strings.Join(live, ",") != "a" {
		t.Errorf("future-stamped peer: live %v", live)
	}
	// Old on its own clock: stale even though our clock says otherwise.
	if live := run(now.Add(-5*time.Minute), now.Add(30*time.Second)); strings.Join(live, ",") != "a" {
		t.Errorf("stale peer: live %v", live)
	}
}

type stubProber struct{ o Observation }

func (p *stubProber) Network(context.Context) (bool, string) { return true, "ok" }
func (p *stubProber) Probe(context.Context, Target, []byte) Observation {
	return p.o
}

type nopNotifier struct{}

func (nopNotifier) Send(context.Context, string, string) error { return nil }

func TestHubPinValidated(t *testing.T) {
	c := &Config{Self: "a", Sentinels: []SentinelRef{{Name: "a"}}, Slack: SlackConfig{UserID: "U"},
		Targets: []Target{{Name: "server", Hub: []Endpoint{{URL: "https://h:7443"}}, HubPin: "not-a-fingerprint"}}}
	if err := c.Normalize(); err == nil || !strings.Contains(err.Error(), "hub_pin") {
		t.Errorf("malformed pin: %v", err)
	}
}
