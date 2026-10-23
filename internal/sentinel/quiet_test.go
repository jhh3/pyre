package sentinel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real fleet (Oct 22): server, devbox and laptop at home,
// office-box1 and office-box2 at the office.
var (
	fleetOrder = []string{"devbox", "office-box2", "laptop", "office-box1", server}
	homeHosts  = []string{server, "devbox", "laptop"}
	office     = []string{"office-box1", "office-box2"}
	withSites  = func(c *Config) {
		c.Sites = map[string][]string{"home": homeHosts, "office": office}
	}
)

func (w *world) count(sub string) int {
	n := 0
	for _, m := range w.sent {
		if strings.Contains(m.text, sub) {
			n++
		}
	}
	return n
}

// at reports the fake clock in "15:04" UTC, for failure messages.
func (w *world) at() string { return w.clock.Now().UTC().Format("15:04") }

// A problem that starts inside a maintenance window gets its full grace
// after the window ends: no alert during it, none right at its end, and the
// alert once the grace has passed after it.
func TestMaintenanceSuppressesThenExpiryRestoresAlert(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(1) // 7:31
	w.maint[server] = &Window{Until: w.clock.Now().Add(10 * time.Minute), Reason: "upgrade.sh from v0.1.0-rc99"}
	w.unitsDown[server] = []string{"pyre-web (activating)"}
	w.steps(18) // to 7:49: the window ended at 7:41, the last quiet tick was 7:40
	if len(w.sent) != 0 {
		t.Fatalf("alerted inside the window or before its grace after it (now %s):\n%s", w.at(), w.dump())
	}
	w.steps(2) // 7:51: 10 min of DEGRADED after the window
	got := w.about(server)
	if len(got) != 1 || !strings.Contains(got[0].text, "server is DEGRADED since") || !strings.Contains(got[0].text, "pyre-web (activating)") {
		t.Fatalf("want one DEGRADED alert after the window's grace:\n%s", w.dump())
	}

	// A window ended early (upgrade.sh clears its own after a healthy
	// restart): the grace counts from the clear.
	w2 := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w2.steps(1) // 7:31
	w2.maint[server] = &Window{Until: w2.clock.Now().Add(10 * time.Minute)}
	w2.hubUp[server] = false
	w2.steps(3) // 7:34
	w2.maint[server] = nil
	w2.steps(2) // 7:36: the cache is dropped at the first probe without the file (7:35)
	if len(w2.sent) != 0 {
		t.Fatalf("alerted right after the window was cleared:\n%s", w2.dump())
	}
	w2.steps(3) // 7:39: DOWN held 3 min after the last quiet tick (7:34)
	if got := w2.about(server); len(got) != 1 || !strings.Contains(got[0].text, "server is DOWN since") {
		t.Fatalf("want the DOWN alert 3 min after the window was cleared:\n%s", w2.dump())
	}
}

// partitionWorld: the home uplink is congested. The office NUCs cannot
// probe server or laptop (hub and ssh time out) and those cannot probe the
// office, but sentinel state still gets through devbox (the port
// forward), as on Oct 22.
func partitionWorld(t *testing.T) *world {
	w := newFleet(t, fleetOrder, withSites, func(c *Config) { c.Targets[0].NoRecovery = true })
	return w
}

func (w *world) setLink(down bool) {
	for _, o := range office {
		for _, h := range []string{server, "laptop"} {
			w.blocked[[2]string{o, h}] = down
			w.blocked[[2]string{h, o}] = down
		}
	}
}

// A partition (some watchers fail, the target's own LAN says it is fine) is
// one link message and one recovery, with no DOWN, no reminders and no
// flapping notices, however often the link comes and goes.
func TestPartitionIsOneLinkMessageNotDown(t *testing.T) {
	w := partitionWorld(t)
	w.steps(2)
	for range 6 { // flapping: down 7 min, back 3 min, for an hour
		w.setLink(true)
		w.steps(7)
		w.setLink(false)
		w.steps(3)
	}
	w.setLink(true)
	w.steps(60) // and then down for another hour
	t.Logf("messages:\n%s", w.dump())
	if n := w.count("link looks down"); n != 1 {
		t.Fatalf("%d link messages, want 1:\n%s", n, w.dump())
	}
	for _, bad := range []string{"DOWN", "DEGRADED", "reminder", "again", "recovered"} {
		if n := w.count(bad); n != 0 {
			t.Errorf("%d messages with %q during a partition:\n%s", n, bad, w.dump())
		}
	}
	m := w.sent[0]
	if m.from != "devbox" {
		t.Errorf("link message from %s, want the leader", m.from)
	}
	for _, want := range []string{
		"Pyre: The office ↔ home link looks down since",
		"office-box1, office-box2 can't reach laptop and server",
		"laptop, server can't reach office-box1 and office-box2",
		"devbox are fine",
	} {
		if !strings.Contains(m.text, want) {
			t.Errorf("link message lacks %q: %q", want, m.text)
		}
	}
	if l := w.engines["devbox"].State().Timers[server].Level; l != LevelOK {
		t.Errorf("server evaluated %s during the partition, want OK (its own LAN reaches it)", l)
	}

	w.setLink(false)
	w.steps(29)
	if n := w.count("link recovered"); n != 0 {
		t.Fatalf("recovered before 30 min stable:\n%s", w.dump())
	}
	w.steps(3)
	if n := w.count("link recovered"); n != 1 || len(w.sent) != 2 {
		t.Fatalf("want exactly the link message and one recovery:\n%s", w.dump())
	}
	if !strings.Contains(w.sent[1].text, "Pyre: The home ↔ office link recovered at") {
		t.Errorf("recovered text: %q", w.sent[1].text)
	}
	for _, name := range fleetOrder {
		if st := w.engines[name].State(); len(st.Links) != 0 {
			t.Errorf("%s still has link incidents %v", name, st.Links)
		}
	}
}

// Without sites the rule is "which watchers succeed": two cannot reach
// server, one can, so it is a link, not DOWN.
func TestPartitionWithoutSitesIsALink(t *testing.T) {
	w := newWorld(t, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	w.blocked[[2]string{"office-box2", server}] = true
	w.blocked[[2]string{"laptop", server}] = true
	w.steps(20)
	if n := w.count("DOWN"); n != 0 {
		t.Fatalf("DOWN while devbox reaches server:\n%s", w.dump())
	}
	if n := w.count("Pyre: A network link looks down since"); n != 1 || len(w.sent) != 1 {
		t.Fatalf("want one link message:\n%s", w.dump())
	}
	if !strings.Contains(w.sent[0].text, "laptop, office-box2 can't reach server; server and devbox are fine.") {
		t.Errorf("text: %q", w.sent[0].text)
	}
}

// Every watcher fails, the same-LAN ones included: a real outage, DOWN as
// before (with sites configured and the target's own sentinel gone).
func TestRealOutageStillAlertsDownWithSites(t *testing.T) {
	w := newFleet(t, fleetOrder, withSites, func(c *Config) { c.Targets[0].NoRecovery = true })
	w.steps(2)
	w.hostUp[server], w.running[server] = false, false // power cut
	w.steps(8)
	got := w.about(server)
	if len(got) != 1 || !strings.Contains(got[0].text, "Pyre: server is DOWN since") ||
		!strings.Contains(got[0].text, "confirmed by devbox, laptop, office-box1, office-box2") {
		t.Fatalf("want one DOWN alert confirmed by every watcher:\n%s", w.dump())
	}
	if n := w.count("link"); n != 0 {
		t.Errorf("a real outage was called a link problem:\n%s", w.dump())
	}

	// The hub dies on a host every watcher still reaches over ssh: DOWN.
	w2 := newFleet(t, fleetOrder, withSites, func(c *Config) { c.Targets[0].NoRecovery = true })
	w2.steps(2)
	w2.hubUp[server] = false
	w2.steps(6)
	if got := w2.about(server); len(got) != 1 || !strings.Contains(got[0].text, "Pyre: server is DOWN since") {
		t.Fatalf("hub down:\n%s", w2.dump())
	}

	// At the start of an outage a watcher's last good probe (from before it)
	// is no evidence of a partition: the office NUCs see it first, home a
	// tick later, and it is still DOWN, not a link.
	w3 := newFleet(t, fleetOrder, withSites, func(c *Config) { c.Targets[0].NoRecovery = true })
	w3.steps(2)
	w3.hostUp[server], w3.running[server] = false, false
	w3.steps(10)
	if n := w3.count("link"); n != 0 || w3.count("server is DOWN since") != 1 {
		t.Fatalf("outage onset:\n%s", w3.dump())
	}
}

func withPublic(c *Config) {
	c.Targets[0].NoRecovery = true
	c.Targets[0].Public = []Endpoint{{URL: "https://pyre-example.ngrok.app", Label: "pyre-web"}}
}

// Public URLs failing from the watchers only (an ngrok edge problem on
// their side) while server's own check of them passes: no alert at all. When
// server's own check fails too, one alert after 15 minutes.
func TestPublicURLOnlyNeedsSelfCheckAnd15Minutes(t *testing.T) {
	w := newWorld(t, withPublic)
	w.steps(2)
	for _, from := range []string{"devbox", "office-box2", "laptop"} {
		w.publicFail[[2]string{from, server}] = "EOF"
	}
	w.steps(60)
	if len(w.sent) != 0 {
		t.Fatalf("alerted on a public URL only the watchers fail on:\n%s", w.dump())
	}
	if st := w.engines[server].State(); len(st.SelfPublic) != 1 || !st.SelfPublic[0].OK {
		t.Fatalf("server self-check %+v", st.SelfPublic)
	}

	w2 := newWorld(t, withPublic)
	w2.steps(2) // 7:32
	for _, from := range []string{"devbox", "office-box2", "laptop", server} {
		w2.publicFail[[2]string{from, server}] = "EOF"
	}
	w2.steps(14) // 7:46: 14 min
	if len(w2.sent) != 0 {
		t.Fatalf("public-URL-only alerted before 15 min (now %s):\n%s", w2.at(), w2.dump())
	}
	w2.steps(3)
	got := w2.about(server)
	if len(got) != 1 {
		t.Fatalf("want one alert by 17 min:\n%s", w2.dump())
	}
	for _, want := range []string{"server is DEGRADED since", "public URL down: https://pyre-example.ngrok.app (EOF)", "server's own check fails too: https://pyre-example.ngrok.app (EOF)."} {
		if !strings.Contains(got[0].text, want) {
			t.Errorf("alert lacks %q: %q", want, got[0].text)
		}
	}
	w2.steps(5)
	if len(w2.sent) != 1 {
		t.Errorf("more than one message:\n%s", w2.dump())
	}
}

func TestSitesConfig(t *testing.T) {
	c := &Config{Self: "a", Sentinels: []SentinelRef{{Name: "a"}, {Name: "b"}}, Slack: SlackConfig{UserID: "U0"},
		Sites: map[string][]string{"home": {"a"}, "office": {"a", "b"}}}
	if err := c.Normalize(); err == nil || !strings.Contains(err.Error(), `host "a" is in sites`) {
		t.Fatalf("a host in two sites: %v", err)
	}
	c.Sites = map[string][]string{"home": {"a"}, "office": {"b"}}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	if c.site("b") != "office" || c.site("x") != "" || c.Timing.PublicGrace.D() != 15*time.Minute {
		t.Errorf("site(b)=%q site(x)=%q public_grace=%s", c.site("b"), c.site("x"), c.Timing.PublicGrace.D())
	}
	if key, label := c.linkKey("a", []string{"b"}); key != "home~office" || label != "office ↔ home" {
		t.Errorf("link %q %q", key, label)
	}
}

// maintenanceScript writes the window a sentinel reads and removes it.
func TestRemoteMaintenanceScript(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC)
	w := &Window{Until: now.Add(15 * time.Minute), Reason: "it's 'quoted' $HOME `x`", Set: now}
	run := func(w *Window) string {
		s, err := maintenanceScript(w)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("/bin/sh", "-s")
		cmd.Stdin, cmd.Env = strings.NewReader(s), []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	if out := run(w); !strings.Contains(out, "@@ok") {
		t.Fatalf("no ok: %q", out)
	}
	got, err := ReadMaintenance(filepath.Join(home, remoteMaintenanceFile))
	if err != nil || got == nil || !got.Until.Equal(w.Until) || got.Reason != w.Reason {
		t.Fatalf("window %+v, %v", got, err)
	}
	run(nil)
	if got, err := ReadMaintenance(filepath.Join(home, remoteMaintenanceFile)); got != nil || err != nil {
		t.Fatalf("after clear: %+v, %v", got, err)
	}
}
