package resilience

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/internal/storage/sqlitex"
	"github.com/jhh3/pyre/internal/transport"
	"github.com/jhh3/pyre/tests/resilience/stack"

	"testing"
)

// TestMain lets the test binary double as a node daemon stand-in for the
// resource budget (PYRE_RESILIENCE_ROLE=node), so node overhead is measured
// in its own process.
func TestMain(m *testing.M) {
	switch os.Getenv(stack.RoleEnv) {
	case "node":
		os.Exit(nodeRole())
	case stack.RoleDaemon:
		os.Exit(stack.RunTestDaemon(os.Args[1:]))
	}
	code := m.Run()
	stack.Cleanup()
	os.Exit(code)
}

// nodeRole runs a NodeLink and answers line commands on stdin:
//
//	enqueue N   -> "ok <ms>"          (N durable reports)
//	outbox      -> "outbox <n>"
//	connected   -> "connected <bool>"
//	logs        -> "logs <lines> <bytes>"
//	quit
func nodeRole() int {
	ring := diagnostics.NewRing(1000, 1<<20, diagnostics.Redact)
	db, err := sqlitex.Open(os.Getenv("NODE_DB"))
	if err != nil {
		fmt.Println("error", err)
		return 1
	}
	defer db.Close()
	link, err := transport.NewNodeLink(transport.NodeLinkConfig{
		HubURL: os.Getenv("HUB_URL"), NodeID: os.Getenv("NODE_ID"), Token: os.Getenv("NODE_TOKEN"), DB: db,
		Logger: slog.New(slog.NewTextHandler(ring, &slog.HandlerOptions{Level: slog.LevelInfo})),
		Hello: func(context.Context) (contracts.NodeHello, error) {
			return contracts.NodeHello{Hostname: "budget-node", Platform: "linux", Arch: "amd64", PyredVersion: "budget"}, nil
		},
		Handler: transport.CommandHandlerFunc(func(_ context.Context, cmd contracts.NodeCommand) (contracts.CommandResult, error) {
			return contracts.CommandResult{Outcome: contracts.OutcomeSucceeded, Result: []byte(`{"ok":true}`)}, nil
		}),
		Backoff: transport.BackoffConfig{Initial: 50 * time.Millisecond, Max: time.Second},
	})
	if err != nil {
		fmt.Println("error", err)
		return 1
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = link.Run(ctx) }()
	defer func() { cancel(); <-done }()
	in := bufio.NewScanner(os.Stdin)
	next := 0
	for in.Scan() {
		f := strings.Fields(in.Text())
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "enqueue":
			n, _ := strconv.Atoi(f[1])
			start := time.Now()
			for i := 0; i < n; i++ {
				if _, err := link.Enqueue(ctx, contracts.ReportEvent, map[string]any{"kind": "budget.tick", "i": next}); err != nil {
					fmt.Println("error", err)
					break
				}
				next++
			}
			fmt.Println("ok", time.Since(start).Milliseconds())
		case "outbox":
			n, _ := link.OutboxLen(ctx)
			fmt.Println("outbox", n)
		case "connected":
			fmt.Println("connected", link.Connected())
		case "logs":
			st := ring.Stats()
			fmt.Println("logs", st.TotalLines, st.TotalBytes)
		case "quit":
			return 0
		}
	}
	return 0
}
