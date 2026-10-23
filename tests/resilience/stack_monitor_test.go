//go:build !windows

package resilience

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/supervisor"
	"github.com/jhh3/pyre/tests/resilience/stack"
)

// TestStackMonitorWakesOnChange (W6): `pyre monitor add` starts a real
// shell session on the real pyred; its loop checks a file every minute and
// messages the target session only when the file's content changes, as the
// monitor session.
func TestStackMonitorWakesOnChange(t *testing.T) {
	skipShort(t)
	needTmux(t)
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	// The run's PATH starts with pyred's directory: put the CLI next to it.
	cli := filepath.Join(filepath.Dir(bins.Pyred), "pyre")
	build := exec.Command("go", "build", "-o", cli, "./cmd/pyre")
	build.Dir, build.Env = stack.ModuleRoot(), append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build pyre: %v\n%s", err, out)
	}
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "mon")
	tm := supervisor.Tmux{Socket: stack.TmuxSocket(node.State), OwnedRoot: node.State}
	t.Cleanup(func() { _ = tm.Kill(context.Background()) })
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	target := startSession(t, cl, node.NodeID, contracts.HarnessShell, dir, "target")
	watched := filepath.Join(dir, "watched")
	if err := os.WriteFile(watched, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cli, "--json", "--hub", hub.URL, "--pin", hub.Pin, "monitor", "add", "--node", "mon", "--session", target.Session.SessionID,
		"--every", "1m", "--cmd", "cat "+watched, "--title", "watched file")
	cmd.Env = append(os.Environ(), "PYRE_TOKEN="+hub.Owner.Token, "PYRE_CONFIG="+filepath.Join(dir, "client.json"), "PYRE_SESSION_TOKEN=", "PYRE_SESSION_ID=", "PYRE_NODE_ID=", "PYRE_SOCKET=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("monitor add: %v\n%s", err, out)
	}
	var mon contracts.SessionView
	stack.WaitFor(t, 30*time.Second, "monitor running", func() bool {
		page, err := cl.ListSessions(context.Background(), contracts.ListSessionsRequest{Harness: contracts.HarnessShell, IncludeMonitors: true})
		if err != nil {
			return false
		}
		for _, s := range page.Items {
			if s.Session.Title == "monitor: watched file" && s.Run != nil && s.Run.State == contracts.RunRunning {
				mon = s
				return true
			}
		}
		return false
	})
	inbox := func() []contracts.MessageView {
		page, _ := cl.ListMessages(context.Background(), contracts.ListMessagesRequest{Box: "inbox",
			Address: &contracts.Address{Kind: contracts.AddrSession, ID: target.Session.SessionID}})
		return page.Items
	}
	time.Sleep(3 * time.Second) // the baseline check runs at start
	if msgs := inbox(); len(msgs) != 0 {
		t.Fatalf("woken before any change: %+v", msgs)
	}
	if err := os.WriteFile(watched, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got contracts.MessageView
	screen := func() string {
		rs, _ := cl.ReadSession(context.Background(), contracts.ReadSessionRequest{SessionID: mon.Session.SessionID})
		return rs.Screen
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("monitor screen:\n%s", screen())
		}
	})
	stack.WaitFor(t, 90*time.Second, "wake message", func() bool {
		msgs := inbox()
		if len(msgs) > 0 {
			got = msgs[0]
		}
		return len(msgs) > 0
	})
	if got.Envelope.Sender.ID != mon.Session.SessionID || !strings.Contains(got.Envelope.Body.Text, "two") || !strings.Contains(got.Envelope.Body.Text, "previous: one") {
		rs, _ := cl.ReadSession(context.Background(), contracts.ReadSessionRequest{SessionID: mon.Session.SessionID})
		t.Fatalf("wake: %+v\nmonitor screen:\n%s", got.Envelope, rs.Screen)
	}
	time.Sleep(65 * time.Second) // another check, unchanged: no second wake
	if msgs := inbox(); len(msgs) != 1 {
		t.Fatalf("%d messages after an unchanged check", len(msgs))
	}
	// Archiving the target ends the monitor at its next check.
	yes := true
	if _, err := cl.UpdateSession(stack.OwnerCtx(t), contracts.UpdateSessionRequest{SessionID: target.Session.SessionID, Archived: &yes, WhenDone: true}); err != nil {
		t.Fatal(err)
	}
	stack.WaitFor(t, 120*time.Second, "monitor exits after its target is archived", func() bool {
		sv, err := cl.GetSession(context.Background(), contracts.GetRequest{ID: mon.Session.SessionID})
		return err == nil && sv.Run != nil && sv.Run.State.Terminal()
	})
	evidence(t, "monitor %s woke %s once on change (%q)", mon.Session.SessionID, target.Session.SessionID, got.Envelope.Body.Text)
}
