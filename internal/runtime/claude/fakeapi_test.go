package claude

// A minimal Pyre API (WhoAmI, ListMessages with long-poll, GetMessage,
// AckMessage) served through the real httpapi handler on a unix socket, so
// the real `pyre mcp` inbox/ack tools and the real doorbell watcher talk to
// it exactly as they talk to pyred. Synthetic data only.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/cli"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/httpapi"
	"github.com/jhh3/pyre/internal/runtime/claudeplugin"
)

type fakeAPI struct {
	contracts.API // unimplemented methods are nil (calls fail)
	mu            sync.Mutex
	changed       chan struct{}
	msgs          []*contracts.MessageView
	session       string
	acks          map[string]time.Time
	lists         int
	gets          int

	srv  *http.Server
	sock string
}

func newFakeAPI(session string) *fakeAPI {
	return &fakeAPI{session: session, changed: make(chan struct{}), acks: map[string]time.Time{}}
}

func (f *fakeAPI) notify() {
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeAPI) post(id, from, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	f.msgs = append(f.msgs, &contracts.MessageView{
		Envelope: contracts.MessageEnvelope{SchemaVersion: 1, MessageID: id, OperationID: "op_" + id,
			Sender:    contracts.Principal{Kind: contracts.PrincipalSession, ID: from, Name: from},
			Recipient: contracts.Address{Kind: contracts.AddrSession, ID: f.session},
			Mode:      contracts.ModeQueue, Body: contracts.MessageBody{Text: text}, CreatedAt: now},
		Status: contracts.DeliveryStatus{Storage: contracts.StorageStored, StoredAt: &now, Runtime: contracts.DeliveryQueued, Ack: contracts.AckNone, UpdatedAt: now},
		Cursor: int64(len(f.msgs) + 1),
	})
	f.notify()
}

func (f *fakeAPI) ackedAt(id string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.acks[id]
	return t, ok
}

func (f *fakeAPI) WhoAmI(ctx context.Context) (contracts.Principal, error) {
	return contracts.Principal{Kind: contracts.PrincipalSession, ID: f.session}, nil
}

func (f *fakeAPI) ListMessages(ctx context.Context, req contracts.ListMessagesRequest) (contracts.Page[contracts.MessageView], error) {
	deadline := time.Now().Add(time.Duration(req.WaitSeconds) * time.Second)
	for {
		f.mu.Lock()
		f.lists++
		var items []contracts.MessageView
		for _, m := range f.msgs {
			if req.Address != nil && (req.Address.Kind != contracts.AddrSession || req.Address.ID != f.session) {
				continue
			}
			if m.Cursor <= req.AfterCursor || (req.Unacked && m.Status.Ack != contracts.AckNone) {
				continue
			}
			items = append(items, *m)
		}
		ch := f.changed
		f.mu.Unlock()
		if len(items) > 0 || req.WaitSeconds <= 0 || time.Now().After(deadline) {
			if items == nil {
				items = []contracts.MessageView{}
			}
			return contracts.Page[contracts.MessageView]{Items: items, SnapshotAt: time.Now().UTC()}, nil
		}
		select {
		case <-ch:
		case <-time.After(time.Until(deadline)):
		case <-ctx.Done():
			return contracts.Page[contracts.MessageView]{}, ctx.Err()
		}
	}
}

func (f *fakeAPI) GetMessage(ctx context.Context, req contracts.GetRequest) (contracts.MessageView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	for _, m := range f.msgs {
		if m.Envelope.MessageID == req.ID {
			return *m, nil
		}
	}
	return contracts.MessageView{}, contracts.Errorf(contracts.ErrNotFound, "no message %s", req.ID)
}

func (f *fakeAPI) AckMessage(ctx context.Context, req contracts.AckMessageRequest) (contracts.MessageView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.msgs {
		if m.Envelope.MessageID == req.MessageID {
			if m.Status.Ack == contracts.AckNone {
				m.Status.Ack, m.Status.AckRef = contracts.AckAcknowledged, "ack_"+req.MessageID
				f.acks[req.MessageID] = time.Now()
				f.notify()
			}
			return *m, nil
		}
	}
	return contracts.MessageView{}, contracts.Errorf(contracts.ErrNotFound, "no message %s", req.MessageID)
}

// serve starts (or restarts) the HTTP API on sock.
func (f *fakeAPI) serve(sock, token string) error {
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	h := httpapi.NewServer(f, httpapi.NewStaticTokens(map[string]contracts.Principal{
		token: {Kind: contracts.PrincipalSession, ID: f.session}}), httpapi.Options{})
	f.mu.Lock()
	f.srv, f.sock = &http.Server{Handler: h}, sock
	srv := f.srv
	f.mu.Unlock()
	go srv.Serve(ln)
	return nil
}

// down stops the API (simulates a pyred restart / outage).
func (f *fakeAPI) down() {
	f.mu.Lock()
	srv := f.srv
	f.mu.Unlock()
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
		_ = srv.Close()
	}
}

// pyreMCPMain (PYRE_TEST_ROLE=pyremcp) is the real `pyre mcp`.
func pyreMCPMain() int {
	return cli.NewApp().Main(context.Background(), []string{"mcp"})
}

// inboxWatchMain (PYRE_TEST_ROLE=inboxwatch) runs the same watcher as
// `pyred inbox-watch --run-dir DIR`, teeing notification lines to
// <run-dir>/doorbell-lines.log for measurement.
func inboxWatchMain(args []string) int {
	runDir := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--run-dir" {
			runDir = args[i+1]
		}
	}
	interval := 15 * time.Second
	if v, err := strconv.Atoi(os.Getenv("PYRE_TEST_DOORBELL_INTERVAL_MS")); err == nil && v > 0 {
		interval = time.Duration(v) * time.Millisecond
	}
	logf, _ := os.OpenFile(runDir+"/doorbell-watch.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	lines, _ := os.OpenFile(runDir+"/doorbell-lines.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	fmt.Fprintf(logf, "start pid=%d %s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano))
	claudeplugin.WritePID(runDir)
	conn, err := cli.Resolve(cli.ConnOptions{})
	if err != nil {
		fmt.Fprintln(logf, "resolve:", err)
		return 1
	}
	err = claudeplugin.Watch(context.Background(), claudeplugin.APISource{API: conn.Client, Session: os.Getenv("PYRE_SESSION_ID")},
		claudeplugin.WatchOptions{Out: io.MultiWriter(os.Stdout, &stampWriter{w: lines}), Log: logf,
			StateFile: runDir + "/doorbell.json", RunDir: runDir, MinInterval: interval})
	fmt.Fprintln(logf, "exit:", err)
	return 0
}

type stampWriter struct{ w io.Writer }

func (s *stampWriter) Write(p []byte) (int, error) {
	fmt.Fprintf(s.w, "%d %s", time.Now().UnixMilli(), p)
	return len(p), nil
}
