//go:build !windows

package resilience

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/client"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/diagnostics"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/mock"
	"github.com/jhh3/pyre/tests/resilience/refhub"
	"github.com/jhh3/pyre/tests/resilience/stack"
)

// Proposed explicit limits (A20). Measured values and environment are in
// BUDGETS.md; limits leave headroom for slower CI hardware. The test fails
// when a measurement exceeds its limit.
var budgetLimits = map[string]float64{
	"node_idle_cpu_pct":           1.0,   // % of one core, connected, 15s heartbeat
	"node_idle_rss_mb":            64,    // NodeLink + SQLite, no runtimes
	"node_burst_peak_rss_mb":      128,   // during a 10k-report burst
	"node_cpu_us_per_report":      1000,  // node CPU time per durable report (enqueue + send + prune)
	"hub_cpu_us_per_report":       1000,  // hub-process CPU per committed report (includes test polling)
	"report_throughput_min_per_s": 500,   // node outbox -> hub commit
	"outage_drain_5k_s":           20,    // 5000 queued reports after a hub restart
	"command_latency_p99_ms":      500,   // queue -> result committed, 200 commands
	"event_latency_p99_ms":        250,   // paced 500 events/s through httpapi stream + client.WatchEvents
	"event_burst_min_per_s":       2000,  // 5000 events as fast as possible
	"node_log_bytes_per_report":   1,     // durable reports must not log per item at Info
	"node_idle_log_bytes_per_min": 2048,  // connected idle
	"hub_log_bytes_per_report":    1,     // hub NodeServer logs per report at Info
	"node_db_bytes_per_report":    300.0, // outbox row footprint is reclaimed after ack (WAL bound checked separately)
}

type budgetResult struct {
	Name  string  `json:"name"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	Limit float64 `json:"limit,omitempty"`
	Note  string  `json:"note,omitempty"`
}

type budget struct {
	t       *testing.T
	results []budgetResult
}

func (b *budget) add(name string, v float64, unit, note string) {
	r := budgetResult{Name: name, Value: math.Round(v*100) / 100, Unit: unit, Note: note}
	if lim, ok := budgetLimits[name]; ok {
		r.Limit = lim
	}
	b.results = append(b.results, r)
}

func (b *budget) check() {
	for _, r := range b.results {
		if r.Limit == 0 {
			continue
		}
		over := r.Value > r.Limit
		if strings.HasSuffix(r.Name, "_min_per_s") {
			over = r.Value < r.Limit
		}
		if over {
			b.t.Errorf("BUDGET EXCEEDED %s = %v %s (limit %v)", r.Name, r.Value, r.Unit, r.Limit)
		}
	}
}

func (b *budget) report() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "| metric | value | unit | limit | note |\n|---|---|---|---|---|\n")
	for _, r := range b.results {
		lim := ""
		if r.Limit != 0 {
			lim = strconv.FormatFloat(r.Limit, 'f', -1, 64)
		}
		fmt.Fprintf(&sb, "| %s | %v | %s | %s | %s |\n", r.Name, r.Value, r.Unit, lim, r.Note)
	}
	return sb.String()
}

// nodeProc is the node role subprocess.
type nodeProc struct {
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Scanner
	mu  sync.Mutex
}

func startNodeProc(t *testing.T, hubURL, nodeID, token, dbPath string) *nodeProc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "PYRE_RESILIENCE_ROLE=node", "HUB_URL="+hubURL, "NODE_ID="+nodeID, "NODE_TOKEN="+token, "NODE_DB="+dbPath)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	p := &nodeProc{cmd: cmd, in: in, out: bufio.NewScanner(out)}
	t.Cleanup(func() {
		_, _ = io.WriteString(in, "quit\n")
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	return p
}

func (p *nodeProc) ask(t *testing.T, line string) []string {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := io.WriteString(p.in, line+"\n"); err != nil {
		t.Fatal(err)
	}
	if !p.out.Scan() {
		t.Fatalf("node process gave no answer to %q: %v", line, p.out.Err())
	}
	f := strings.Fields(p.out.Text())
	if len(f) > 0 && f[0] == "error" {
		t.Fatalf("node process: %s", p.out.Text())
	}
	return f
}

func (p *nodeProc) num(t *testing.T, line string, idx int) int64 {
	f := p.ask(t, line)
	if len(f) <= idx {
		t.Fatalf("short answer %v", f)
	}
	n, _ := strconv.ParseInt(f[idx], 10, 64)
	return n
}

func countReports(t *testing.T, hub *refhub.RefHub, nodeID string) int {
	var n int
	if err := hub.DB().QueryRow(`SELECT COUNT(*) FROM refhub_reports WHERE node_id=? AND kind=?`, nodeID, string(contracts.ReportEvent)).Scan(&n); err != nil {
		return -1
	}
	return n
}

func percentile(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	i := int(math.Ceil(p*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func newInfoLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// TestResourceBudget measures A20 resource envelopes. Opt-in:
//
//	PYRE_RESOURCE=1 go test ./tests/resilience -run TestResourceBudget -v
//
// PYRE_RESOURCE_OUT=<file> also writes the results as JSON.
func TestResourceBudget(t *testing.T) {
	if os.Getenv("PYRE_RESOURCE") != "1" {
		t.Skip("set PYRE_RESOURCE=1 to run the resource budget measurement (~60s)")
	}
	b := &budget{t: t}
	ctx := ctxT(t)
	hubLogs := diagnostics.NewRing(1000, 1<<20, nil)
	hub, err := refhub.New(refhub.Options{Dir: t.TempDir(), HeartbeatInterval: 15 * time.Second,
		Logger: newInfoLogger(hubLogs)})
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	nodeID, tok, err := hub.EnrollNode(ctx, "budget-node")
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "node.db")
	np := startNodeProc(t, hub.URL(), nodeID, tok, dbPath)
	pid := np.cmd.Process.Pid
	waitFor(t, 15*time.Second, "node subprocess connected", func() bool { return hub.Connected(nodeID) })

	// ---- idle ----
	time.Sleep(2 * time.Second) // let startup settle
	logs0 := np.num(t, "logs", 2)
	s0, err := diagnostics.SampleProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	idle := 20 * time.Second
	time.Sleep(idle)
	s1, _ := diagnostics.SampleProcess(pid)
	logs1 := np.num(t, "logs", 2)
	b.add("node_idle_cpu_pct", diagnostics.CPUPercent(s0, s1), "% core", fmt.Sprintf("%v window, heartbeat 15s", idle))
	b.add("node_idle_rss_mb", s1.RSSMB(), "MiB", "node subprocess (NodeLink + SQLite)")
	b.add("node_idle_log_bytes_per_min", float64(logs1-logs0)/idle.Minutes(), "B/min", "Info level")

	// ---- report burst ----
	const burst = 10000
	hubLogs.Reset()
	nodeLogs0 := np.num(t, "logs", 2)
	var peakRSS float64
	var peakOutbox int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if s, err := diagnostics.SampleProcess(pid); err == nil && s.RSSMB() > peakRSS {
					peakRSS = s.RSSMB()
				}
			}
		}
	}()
	bs0, _ := diagnostics.SampleProcess(pid)
	hs0, _ := diagnostics.SampleSelf()
	start := time.Now()
	enqMs := np.num(t, fmt.Sprintf("enqueue %d", burst), 1)
	for {
		if n := np.num(t, "outbox", 1); n > peakOutbox {
			peakOutbox = n
		}
		if countReports(t, hub, nodeID) >= burst {
			break
		}
		if time.Since(start) > 60*time.Second {
			t.Fatal("burst did not drain in 60s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	drain := time.Since(start)
	bs1, _ := diagnostics.SampleProcess(pid)
	hs1, _ := diagnostics.SampleSelf()
	close(stop)
	wg.Wait()
	nodeLogs1 := np.num(t, "logs", 2)
	b.add("report_throughput_min_per_s", burst/drain.Seconds(), "reports/s", fmt.Sprintf("%d reports enqueued in %dms, committed in %v", burst, enqMs, drain.Round(time.Millisecond)))
	b.add("node_burst_cpu_pct", diagnostics.CPUPercent(bs0, bs1), "% core", "average over the burst window (bounded work, informational)")
	b.add("node_cpu_us_per_report", float64((bs1.CPU-bs0.CPU).Microseconds())/burst, "µs/report", "ps TIME resolution 10ms")
	b.add("hub_cpu_us_per_report", float64((hs1.CPU-hs0.CPU).Microseconds())/burst, "µs/report", "getrusage of the hub+test process")
	b.add("node_burst_peak_rss_mb", peakRSS, "MiB", "sampled every 100ms")
	b.add("node_peak_outbox", float64(peakOutbox), "reports", "unacked during burst (MaxOutbox default 100000)")
	b.add("node_log_bytes_per_report", float64(nodeLogs1-nodeLogs0)/burst, "B/report", "")
	b.add("hub_log_bytes_per_report", float64(hubLogs.Stats().TotalBytes)/burst, "B/report", "NodeServer at Info")
	waitFor(t, 10*time.Second, "outbox empty", func() bool { return np.num(t, "outbox", 1) == 0 })
	if st, err := os.Stat(dbPath); err == nil {
		wal, _ := os.Stat(dbPath + "-wal")
		var walSize int64
		if wal != nil {
			walSize = wal.Size()
		}
		b.add("node_db_bytes_per_report", float64(st.Size())/burst, "B/report", fmt.Sprintf("db %d B + wal %d B after %d acked reports", st.Size(), walSize, burst))
	}

	// ---- commands ----
	const ncmd = 200
	var lat []time.Duration
	issued := map[string]time.Time{}
	for i := 0; i < ncmd; i++ {
		c, err := hub.QueueCommand(ctx, nodeID, contracts.CmdAction, map[string]int{"i": i}, nil)
		if err != nil {
			t.Fatal(err)
		}
		issued[c.CommandID] = time.Now()
	}
	seen := map[string]bool{}
	deadline := time.Now().Add(30 * time.Second)
	for len(seen) < ncmd && time.Now().Before(deadline) {
		res, _ := hub.CommandResults(ctx, nodeID)
		now := time.Now()
		for id := range res {
			if !seen[id] {
				seen[id] = true
				lat = append(lat, now.Sub(issued[id]))
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(seen) < ncmd {
		t.Fatalf("only %d/%d command results", len(seen), ncmd)
	}
	b.add("command_latency_p50_ms", ms(percentile(lat, 0.5)), "ms", fmt.Sprintf("%d commands queued at once (5ms poll resolution)", ncmd))
	b.add("command_latency_p99_ms", ms(percentile(lat, 0.99)), "ms", "")

	// ---- outage drain ----
	hub.Kill()
	np.num(t, "enqueue 5000", 1)
	outboxDown := np.num(t, "outbox", 1)
	if err := hub.Restart(); err != nil {
		t.Fatal(err)
	}
	ostart := time.Now()
	waitFor(t, 60*time.Second, "outage drain", func() bool { return countReports(t, hub, nodeID) >= burst+5000 && np.num(t, "outbox", 1) == 0 })
	b.add("outage_drain_5k_s", time.Since(ostart).Seconds(), "s", fmt.Sprintf("outbox held %d while hub down; includes reconnect backoff", outboxDown))

	// ---- event stream ----
	measureEventStream(t, b)

	t.Logf("environment: %s/%s, %d CPUs, %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	t.Logf("\n%s", b.report())
	if out := os.Getenv("PYRE_RESOURCE_OUT"); out != "" {
		blob, _ := json.MarshalIndent(map[string]any{
			"goos": runtime.GOOS, "goarch": runtime.GOARCH, "cpus": runtime.NumCPU(), "go": runtime.Version(),
			"at": time.Now().UTC().Format(time.RFC3339), "results": b.results,
		}, "", "  ")
		if err := os.WriteFile(out, blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b.check()
}

// eventLog is a minimal in-memory event source with long-poll semantics,
// standing in for the hub's events table behind httpapi's event stream.
type eventLog struct {
	mu     sync.Mutex
	events []contracts.Event
	wake   chan struct{}
}

func newEventLog() *eventLog { return &eventLog{wake: make(chan struct{})} }

func (l *eventLog) append(payload []byte) {
	l.mu.Lock()
	ev := contracts.Event{EventID: contracts.NewID(contracts.PrefixEvent), Cursor: int64(len(l.events) + 1), Origin: "hub",
		Kind: contracts.EvNoteCreated, PayloadVersion: 1, Payload: payload, OccurredAt: time.Now().UTC(), IngestedAt: time.Now().UTC()}
	l.events = append(l.events, ev)
	close(l.wake)
	l.wake = make(chan struct{})
	l.mu.Unlock()
}

func (l *eventLog) ListEvents(ctx context.Context, req contracts.ListEventsRequest) (contracts.ListEventsResponse, error) {
	limit := req.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	timer := time.NewTimer(time.Duration(req.WaitSeconds) * time.Second)
	defer timer.Stop()
	for {
		l.mu.Lock()
		var out []contracts.Event
		for i := int(req.AfterCursor); i < len(l.events) && len(out) < limit; i++ {
			out = append(out, l.events[i])
		}
		head := int64(len(l.events))
		wake := l.wake
		l.mu.Unlock()
		if len(out) > 0 || req.WaitSeconds == 0 {
			next := req.AfterCursor
			if len(out) > 0 {
				next = out[len(out)-1].Cursor
			}
			return contracts.ListEventsResponse{Events: out, NextCursor: next, HeadCursor: head}, nil
		}
		select {
		case <-ctx.Done():
			return contracts.ListEventsResponse{}, ctx.Err()
		case <-timer.C:
			return contracts.ListEventsResponse{NextCursor: req.AfterCursor, HeadCursor: head}, nil
		case <-wake:
		}
	}
}

type eventsAPI struct {
	base
	*eventLog
}

func measureEventStream(t *testing.T, b *budget) {
	log := newEventLog()
	api := &eventsAPI{base{mock.New(mock.Options{Empty: true})}, log}
	auth := httpapi.NewStaticTokens(map[string]contracts.Principal{ownerToken: ownerP})
	srv := httptest.NewServer(httpapi.NewServer(api, auth, httpapi.Options{StreamWait: 5 * time.Second}))
	defer srv.Close()
	cl := client.New(srv.URL, ownerToken, client.Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type rx struct {
		sent int64
		got  time.Time
	}
	recv := make(chan rx, 10000)
	go func() {
		_ = cl.WatchEvents(ctx, 0, func(e contracts.Event) error {
			var p struct {
				Sent int64 `json:"sent"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			recv <- rx{p.Sent, time.Now()}
			return nil
		})
	}()
	time.Sleep(300 * time.Millisecond)
	collect := func(n int, timeout time.Duration) []time.Duration {
		var out []time.Duration
		deadline := time.After(timeout)
		for len(out) < n {
			select {
			case r := <-recv:
				out = append(out, r.got.Sub(time.Unix(0, r.sent)))
			case <-deadline:
				t.Fatalf("received %d/%d events", len(out), n)
			}
		}
		return out
	}
	payload := func() []byte { return []byte(`{"sent":` + strconv.FormatInt(time.Now().UnixNano(), 10) + `}`) }

	// Paced: 500 events at 2ms intervals.
	go func() {
		for i := 0; i < 500; i++ {
			log.append(payload())
			time.Sleep(2 * time.Millisecond)
		}
	}()
	paced := collect(500, 30*time.Second)
	b.add("event_latency_p50_ms", ms(percentile(paced, 0.5)), "ms", "paced 500 ev/s, httpapi stream -> client.WatchEvents")
	b.add("event_latency_p99_ms", ms(percentile(paced, 0.99)), "ms", "")

	// Burst: 5000 events as fast as possible.
	s0, _ := diagnostics.SampleSelf()
	start := time.Now()
	go func() {
		for i := 0; i < 5000; i++ {
			log.append(payload())
		}
	}()
	burst := collect(5000, 60*time.Second)
	took := time.Since(start)
	s1, _ := diagnostics.SampleSelf()
	b.add("event_burst_min_per_s", 5000/took.Seconds(), "events/s", fmt.Sprintf("5000 events in %v", took.Round(time.Millisecond)))
	b.add("event_burst_latency_p99_ms", ms(percentile(burst, 0.99)), "ms", "includes queueing behind the burst")
	b.add("event_burst_cpu_pct", diagnostics.CPUPercent(s0, s1), "% core", "server+client in one process")
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// Limits for the real processes (pyre-hub, pyred). See BUDGETS.md.
var stackBudgetLimits = map[string]float64{
	"pyred_idle_cpu_pct":          1.0,
	"pyred_idle_rss_mb":           96,
	"pyred_idle20_cpu_pct":        3.0, // 20 live mock runs being observed
	"hub_idle_cpu_pct":            1.0,
	"hub_idle_rss_mb":             128,
	"pyred_burst_peak_rss_mb":     192,
	"hub_burst_peak_rss_mb":       256,
	"msg_accept_latency_p99_ms":   2000, // owner send -> runtime accepted event (mock runtime)
	"msg_send_min_per_s":          50,   // hub stores sends (RPC returns), 8 concurrent clients
	"hub_cpu_ms_per_message":      50,   // idle-adjusted hub CPU per delivered message
	"pyred_cpu_ms_per_message":    50,   // idle-adjusted pyred CPU per delivered message
	"hub_log_bytes_per_message":   400,
	"pyred_log_bytes_per_message": 400,
}

// TestResourceBudgetStack measures the real pyre-hub and pyred processes
// (pyred --dev-mock runtime, so agent processes are excluded by design):
// idle, idle with 20 live runs, and a 500-message burst across 20 sessions.
//
//	PYRE_RESOURCE=1 go test ./tests/resilience -run TestResourceBudgetStack -v
func TestResourceBudgetStack(t *testing.T) {
	if os.Getenv("PYRE_RESOURCE") != "1" {
		t.Skip("set PYRE_RESOURCE=1 to run the real-process resource budget (~90s)")
	}
	for k, v := range stackBudgetLimits {
		budgetLimits[k] = v
	}
	b := &budget{t: t}
	dir := stack.ShortDir(t)
	bins := stack.Build(t)
	hub := stack.StartHub(t, bins, dir)
	node := stack.EnrollNode(t, hub, dir, "budget")
	node.Extra = []string{"--dev-mock"}
	node.Start(t)
	node.WaitConnected(t, 30*time.Second)
	cl := hub.Client()
	hp, np := hub.Proc.PID(), node.Proc.PID()

	idle := func(prefix string, d time.Duration) {
		h0, _ := diagnostics.SampleProcess(hp)
		n0, _ := diagnostics.SampleProcess(np)
		time.Sleep(d)
		h1, _ := diagnostics.SampleProcess(hp)
		n1, _ := diagnostics.SampleProcess(np)
		b.add(prefix+"pyred_idle_cpu_pct", diagnostics.CPUPercent(n0, n1), "% core", fmt.Sprintf("%v window", d))
		b.add(prefix+"hub_idle_cpu_pct", diagnostics.CPUPercent(h0, h1), "% core", fmt.Sprintf("%v window", d))
		b.add(prefix+"pyred_idle_rss_mb", n1.RSSMB(), "MiB", "")
		b.add(prefix+"hub_idle_rss_mb", h1.RSSMB(), "MiB", "")
	}
	time.Sleep(3 * time.Second)
	idle("", 30*time.Second)

	// 20 live mock runs.
	var sids []string
	for i := 0; i < 20; i++ {
		res, err := cl.StartSession(stack.OwnerCtx(t), contracts.StartSessionRequest{NodeID: node.NodeID, Harness: contracts.HarnessMock, Cwd: dir, Title: fmt.Sprintf("budget-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		sids = append(sids, res.Session.Session.SessionID)
	}
	stack.WaitFor(t, 60*time.Second, "20 runs live", func() bool {
		p, err := cl.ListSessions(context.Background(), contracts.ListSessionsRequest{PageRequest: contracts.PageRequest{Limit: 100}})
		if err != nil {
			return false
		}
		live := 0
		for _, s := range p.Items {
			if s.Run != nil && s.Run.State == contracts.RunWaitingInput {
				live++
			}
		}
		return live == 20
	})
	n0, _ := diagnostics.SampleProcess(np)
	h0, _ := diagnostics.SampleProcess(hp)
	time.Sleep(20 * time.Second)
	n1, _ := diagnostics.SampleProcess(np)
	h1, _ := diagnostics.SampleProcess(hp)
	b.add("pyred_idle20_cpu_pct", diagnostics.CPUPercent(n0, n1), "% core", "20 live mock runs, 20s")
	b.add("hub_idle20_cpu_pct", diagnostics.CPUPercent(h0, h1), "% core", "20 live mock runs, 20s")
	b.add("pyred_idle20_rss_mb", n1.RSSMB(), "MiB", "")

	// Burst: 500 messages from the owner across the 20 sessions.
	hubLog0, nodeLog0 := int64(len(hub.Proc.Log())), int64(len(node.Proc.Log()))+dirSize(filepath.Join(node.State, "logs"))
	accepted := map[string]time.Time{}
	var amu sync.Mutex
	wctx, wcancel := context.WithCancel(context.Background())
	defer wcancel()
	head, err := cl.ListEvents(context.Background(), contracts.ListEventsRequest{AfterCursor: 0, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = cl.WatchEventsFiltered(wctx, contracts.ListEventsRequest{AfterCursor: head.HeadCursor, Kinds: []contracts.EventKind{contracts.EvMessageDelivery}}, func(e contracts.Event) error {
			var mv contracts.MessageView
			if json.Unmarshal(e.Payload, &mv) == nil && mv.Status.Runtime == contracts.DeliveryAccepted {
				amu.Lock()
				if _, ok := accepted[mv.Envelope.MessageID]; !ok {
					accepted[mv.Envelope.MessageID] = time.Now()
				}
				amu.Unlock()
			}
			return nil
		})
	}()
	time.Sleep(500 * time.Millisecond)
	// Idle-recipient latency: one message per idle session.
	var idleLat []time.Duration
	for _, sid := range sids {
		t0 := time.Now()
		m, err := cl.SendMessage(context.Background(), contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sid}, Body: contracts.MessageBody{Text: "synthetic latency probe"}})
		if err != nil {
			t.Fatal(err)
		}
		var at time.Time
		stack.WaitFor(t, 30*time.Second, "probe accepted", func() bool {
			amu.Lock()
			defer amu.Unlock()
			at = accepted[m.Envelope.MessageID]
			return !at.IsZero()
		})
		idleLat = append(idleLat, at.Sub(t0))
	}
	b.add("msg_accept_latency_p50_ms", ms(percentile(idleLat, 0.5)), "ms", "owner send -> message.delivery accepted event, idle recipient (20 probes)")
	b.add("msg_accept_latency_p99_ms", ms(percentile(idleLat, 0.99)), "ms", "")
	amu.Lock()
	accepted = map[string]time.Time{}
	amu.Unlock()
	time.Sleep(5 * time.Second) // let the probed runs go idle again
	// Each mock run is busy after accepting input until the daemon's next
	// observation (Poll.Interval 2s), so per-session delivery is serialized
	// at the poll cadence; 5 messages per session keeps the run short.
	const nmsg = 40
	sent := make(map[string]time.Time, nmsg)
	var smu sync.Mutex
	var peakN, peakH float64
	stop := make(chan struct{})
	var swg sync.WaitGroup
	swg.Add(1)
	go func() {
		defer swg.Done()
		tk := time.NewTicker(100 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tk.C:
				if s, err := diagnostics.SampleProcess(np); err == nil && s.RSSMB() > peakN {
					peakN = s.RSSMB()
				}
				if s, err := diagnostics.SampleProcess(hp); err == nil && s.RSSMB() > peakH {
					peakH = s.RSSMB()
				}
			}
		}
	}()
	bn0, _ := diagnostics.SampleProcess(np)
	bh0, _ := diagnostics.SampleProcess(hp)
	start := time.Now()
	var wg sync.WaitGroup
	work := make(chan int)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				t0 := time.Now()
				m, err := cl.SendMessage(context.Background(), contracts.SendMessageRequest{Recipient: contracts.Address{Kind: contracts.AddrSession, ID: sids[i%len(sids)]},
					Body: contracts.MessageBody{Text: fmt.Sprintf("synthetic budget message %d", i)}})
				if err != nil {
					t.Errorf("send %d: %v", i, err)
					continue
				}
				smu.Lock()
				sent[m.Envelope.MessageID] = t0
				smu.Unlock()
			}
		}()
	}
	for i := 0; i < nmsg; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	sendDone := time.Since(start)
	deadline := time.Now().Add(150 * time.Second)
	for time.Now().Before(deadline) {
		amu.Lock()
		n := len(accepted)
		amu.Unlock()
		if n >= nmsg {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	took := time.Since(start)
	amu.Lock()
	nacc := len(accepted)
	amu.Unlock()
	if nacc < nmsg {
		t.Errorf("only %d/%d messages runtime-accepted within 150s", nacc, nmsg)
	}
	bn1, _ := diagnostics.SampleProcess(np)
	bh1, _ := diagnostics.SampleProcess(hp)
	close(stop)
	swg.Wait()
	var lat []time.Duration
	amu.Lock()
	for id, at := range accepted {
		if s, ok := sent[id]; ok {
			lat = append(lat, at.Sub(s))
		}
	}
	amu.Unlock()
	b.add("msg_accept_per_s", float64(nacc)/took.Seconds(), "msgs/s", fmt.Sprintf("%d sends (8 concurrent) in %v; %d runtime-accepted after %v (20 sessions; mock runs busy until next 2s poll)", nmsg, sendDone.Round(time.Millisecond), nacc, took.Round(time.Millisecond)))
	b.add("msg_send_min_per_s", nmsg/sendDone.Seconds(), "msgs/s", "hub stored (send RPC returns), 8 concurrent clients")
	b.add("msg_burst_accept_latency_p99_ms", ms(percentile(lat, 0.99)), "ms", "includes per-session serialization behind busy mock runs")
	b.add("pyred_burst_cpu_s", (bn1.CPU - bn0.CPU).Seconds(), "CPU s", fmt.Sprintf("%d messages over %v", nmsg, took.Round(time.Second)))
	b.add("hub_burst_cpu_s", (bh1.CPU - bh0.CPU).Seconds(), "CPU s", fmt.Sprintf("%d messages over %v", nmsg, took.Round(time.Second)))
	nIdle := (n1.CPU - n0.CPU).Seconds() / 20 // CPU s per wall s with 20 idle runs
	hIdle := (h1.CPU - h0.CPU).Seconds() / 20
	b.add("pyred_cpu_ms_per_message", 1000*math.Max(0, (bn1.CPU-bn0.CPU).Seconds()-nIdle*took.Seconds())/nmsg, "ms CPU/msg", "burst CPU minus the 20-run idle rate over the same window")
	b.add("hub_cpu_ms_per_message", 1000*math.Max(0, (bh1.CPU-bh0.CPU).Seconds()-hIdle*took.Seconds())/nmsg, "ms CPU/msg", "burst CPU minus the 20-run idle rate (includes event-stream watcher load)")
	b.add("pyred_burst_peak_rss_mb", peakN, "MiB", "")
	b.add("hub_burst_peak_rss_mb", peakH, "MiB", "")
	time.Sleep(2 * time.Second)
	hubLog1, nodeLog1 := int64(len(hub.Proc.Log())), int64(len(node.Proc.Log()))+dirSize(filepath.Join(node.State, "logs"))
	b.add("hub_log_bytes_per_message", float64(hubLog1-hubLog0)/nmsg, "B/msg", "pyre-hub stderr at default level")
	b.add("pyred_log_bytes_per_message", float64(nodeLog1-nodeLog0)/nmsg, "B/msg", "pyred info log (rotating file; stderr carries only warnings)")
	b.add("hub_data_dir_mb", float64(dirSize(hub.Data))/(1<<20), "MiB", "after 20 sessions + 60 messages")
	b.add("pyred_state_dir_mb", float64(dirSize(node.State))/(1<<20), "MiB", "")
	idle("post_", 10*time.Second)

	t.Logf("environment: %s/%s, %d CPUs, %s", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version())
	t.Logf("\n%s", b.report())
	if out := os.Getenv("PYRE_RESOURCE_OUT"); out != "" {
		blob, _ := json.MarshalIndent(map[string]any{"goos": runtime.GOOS, "goarch": runtime.GOARCH, "cpus": runtime.NumCPU(), "go": runtime.Version(),
			"at": time.Now().UTC().Format(time.RFC3339), "results": b.results}, "", "  ")
		_ = os.WriteFile(out, blob, 0o644)
	}
	b.check()
}
