// Command demo runs the Pyre TUI for manual review, either against the
// in-process deterministic mock (default; synthetic fixture data, labelled
// mock) or against a hub/mockhub over HTTP:
//
//	go run ./internal/tui/demo                       # in-process mock
//	go run ./internal/tui/demo -chaos                # plus node flaps and event bursts
//	go run ./internal/tui/demo -hub http://127.0.0.1:7788 -token dev-token
//
// The Sessions pane embeds a synthetic in-memory terminal per session. The
// full-screen attach (A) does not exec anything: it shows the descriptor
// the CLI would run and waits for Enter, so the suspend/resume path can be
// reviewed safely.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/internal/tui"
	"github.com/jhh3/pyre/internal/tui/tuitest"
)

func main() {
	hub := flag.String("hub", "", "hub URL (default: in-process mock)")
	token := flag.String("token", os.Getenv("PYRE_TOKEN"), "hub bearer token")
	pin := flag.String("pin", "", "hub TLS certificate SHA-256 pin")
	chaos := flag.Bool("chaos", false, "mock only: random run-state changes, event bursts and node flaps")
	noColor := flag.Bool("no-color", false, "monochrome output")
	extra := flag.String("fixtures", "", "mock only: an extra fixture directory merged after the defaults")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	opts := tui.Options{NoColor: *noColor, Attach: fakeAttach, Edit: tui.DefaultEdit}
	var api contracts.API
	if *hub != "" {
		cl := client.New(*hub, *token, client.Options{CertSHA256: *pin, UserAgent: "pyre-tui-demo"})
		if err := cl.Err(); err != nil {
			fmt.Fprintln(os.Stderr, "demo:", err)
			os.Exit(2)
		}
		api = cl
		opts.Watch = cl.WatchEvents
	} else {
		owner := contracts.Principal{Kind: contracts.PrincipalOwner, ID: "cli_synth-laptop", Name: "owner (synthetic)"}
		clk := mock.NewOffsetClock(mock.FixtureEpoch)
		var dirs []string
		if *extra != "" {
			dirs = append(mock.DefaultFixtureDirs(), *extra)
		}
		b, err := mock.NewE(mock.Options{Clock: clk, DefaultPrincipal: &owner, FixtureDirs: dirs})
		if err != nil {
			fmt.Fprintln(os.Stderr, "demo:", err)
			os.Exit(2)
		}
		go b.Run(ctx, 2*time.Second)
		if *chaos {
			go chaosLoop(ctx, b)
		}
		// The embedded terminal pane shows a synthetic Claude-like session
		// (in-memory; no tmux, no command runs).
		api = tuitest.NewEmbeddable(b)
		opts.StartTerm = tuitest.FakeTermStarter
		opts.Clock = clk
		opts.Watch = tui.PollWatcher(b)
	}
	if err := tui.Run(ctx, api, opts); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "demo:", err)
		os.Exit(1)
	}
}

func fakeAttach(d contracts.AttachDescriptor) error {
	fmt.Print("\x1b[2J\x1b[H")
	fmt.Println("Pyre demo: the TUI is suspended, exactly as during a real attach.")
	fmt.Println()
	fmt.Printf("  run        %s on %s\n", d.RunID, d.NodeID)
	fmt.Printf("  mechanism  %s (read-only: %v)\n", d.Mechanism, d.ReadOnly)
	if len(d.Command) > 0 {
		fmt.Printf("  command    %s\n", strings.Join(d.Command, " "))
	}
	if d.URL != "" {
		fmt.Printf("  url        %s\n", d.URL)
	}
	if d.Notes != "" {
		fmt.Printf("  notes      %s\n", d.Notes)
	}
	fmt.Println()
	fmt.Print("Press Enter to detach and return to the same list and selection… ")
	_, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return err
}

// chaosLoop exercises the robustness paths: bursts of observations,
// approvals appearing and being answered natively, and a node flapping.
func chaosLoop(ctx context.Context, b *mock.Backend) {
	r := rand.New(rand.NewSource(1))
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	states := []contracts.RunState{contracts.RunRunning, contracts.RunWaitingInput, contracts.RunWaitingBackground}
	flap := false
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		page, err := b.ListSessions(ctx, contracts.ListSessionsRequest{})
		if err != nil {
			continue
		}
		var live []contracts.SessionView
		for _, s := range page.Items {
			if s.Run != nil && !s.Run.State.Terminal() && s.Run.State != contracts.RunUnknown {
				live = append(live, s)
			}
		}
		for j := 0; j < 50 && len(live) > 0; j++ { // a burst
			s := live[r.Intn(len(live))]
			_ = b.SetRunState(s.Run.RunID, states[r.Intn(len(states))], "chaos burst (demo)")
		}
		if i%4 == 1 && len(live) > 0 {
			s := live[r.Intn(len(live))]
			if ap, err := b.RequestApproval(s.Run.RunID, mock.ApprovalSpec{Title: "Synthetic chaos request", Body: "demo approval"}); err == nil {
				go func(id string) {
					time.Sleep(20 * time.Second)
					_, _ = b.ResolveApprovalNatively(id, "allow") // withdrawn if still pending
				}(ap.ApprovalID)
			}
		}
		if i%5 == 4 {
			flap = !flap
			_ = b.SetNodeConnected("node_synth-linux", !flap)
		}
	}
}
