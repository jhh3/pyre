package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/mock"
)

// ---- attach ----

type attachAPI struct {
	c.API
	t        *testing.T
	desc     c.AttachDescriptor
	out      string
	conflict bool
	mu       sync.Mutex
	calls    []string
}

func (f *attachAPI) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, s)
}

func (f *attachAPI) GetSession(ctx context.Context, req c.GetRequest) (c.SessionView, error) {
	return c.SessionView{Session: c.Session{SessionID: req.ID}}, nil
}

func (f *attachAPI) AttachSession(ctx context.Context, req c.AttachRequest) (c.AttachDescriptor, error) {
	f.record("attach readonly=" + map[bool]string{true: "yes", false: "no"}[req.ReadOnly])
	d := f.desc
	d.ReadOnly = req.ReadOnly
	return d, nil
}

func (f *attachAPI) AcquireLease(ctx context.Context, req c.LeaseRequest) (c.InputLease, error) {
	if f.conflict {
		return c.InputLease{}, c.Errorf(c.ErrConflict, "run input is leased by someone else")
	}
	if _, err := os.Stat(f.out); err == nil {
		f.t.Errorf("lease acquired after the attach command ran")
	}
	f.record("acquire " + req.RunID)
	return c.InputLease{LeaseID: "lease_test", RunID: req.RunID, ExpiresAt: time.Now().Add(time.Minute)}, nil
}

func (f *attachAPI) ReleaseLease(ctx context.Context, req c.LeaseReleaseRequest) (c.InputLease, error) {
	if _, err := os.Stat(f.out); err != nil {
		f.t.Errorf("lease released before the attach command ran")
	}
	f.record("release " + req.LeaseID)
	return c.InputLease{LeaseID: req.LeaseID}, nil
}

func (f *attachAPI) ReadSession(ctx context.Context, req c.ReadSessionRequest) (c.ReadSessionResponse, error) {
	return c.ReadSessionResponse{Screen: "synthetic screen"}, nil
}

func TestAttachExecsDescriptorCommandUnderLease(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-attach")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$2\" > \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "attached.txt")
	newAPI := func() *attachAPI {
		return &attachAPI{t: t, out: out, desc: c.AttachDescriptor{RunID: "run_x", NodeID: "node_x", Mechanism: c.AttachLocalTmux,
			Command: []string{script, out, "attached-ok"}, Notes: "detach with Ctrl-b d"}}
	}
	e := newEnv(t, nil)

	api := newAPI()
	r := run(t, e, &App{API: api}, "attach", "ses_x")
	expectCode(t, r, 0)
	b, err := os.ReadFile(out)
	if err != nil || strings.TrimSpace(string(b)) != "attached-ok" {
		t.Fatalf("descriptor command did not run with its args: %q %v\n%s", b, err, r)
	}
	if strings.Join(api.calls, ",") != "attach readonly=no,acquire run_x,release lease_test" {
		t.Fatalf("lease protocol: %v", api.calls)
	}

	// Lease held by someone else: exit 3, command not run.
	os.Remove(out)
	api = newAPI()
	api.conflict = true
	r = run(t, e, &App{API: api}, "attach", "ses_x")
	expectCode(t, r, ExitConflict)
	if _, err := os.Stat(out); err == nil {
		t.Fatal("attach command ran without the lease")
	}
	if !strings.Contains(r.stderr, "--takeover") {
		t.Fatalf("should suggest --read-only/--takeover: %s", r)
	}

	// Read-only native attach takes no lease.
	api = newAPI()
	r = run(t, e, &App{API: api}, "attach", "ses_x", "--read-only")
	expectCode(t, r, 0)
	for _, call := range api.calls {
		if strings.HasPrefix(call, "acquire") {
			t.Fatalf("read-only attach acquired a lease: %v", api.calls)
		}
	}

	// --print / --json: the descriptor, no exec.
	os.Remove(out)
	r = run(t, e, &App{API: newAPI()}, "--json", "attach", "ses_x")
	expectCode(t, r, 0)
	if d := mustJSON[c.AttachDescriptor](t, r); d.Mechanism != c.AttachLocalTmux {
		t.Fatalf("descriptor: %+v", d)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("--json attach must not exec")
	}

	// A failing attach command still releases the lease and reports failure.
	api = newAPI()
	api.desc.Command = []string{"/bin/sh", "-c", "echo x > \"$0\"; exit 3", out}
	r = run(t, e, &App{API: api}, "attach", "ses_x")
	expectCode(t, r, ExitError)
	if api.calls[len(api.calls)-1] != "release lease_test" {
		t.Fatalf("lease not released after failure: %v", api.calls)
	}

	// Web terminal: a hint, never the ticket.
	api = newAPI()
	api.desc = c.AttachDescriptor{RunID: "run_x", Mechanism: c.AttachWebTerminal, URL: "https://hub.synthetic/v1/terminal", Ticket: "pyt_secret-ticket-synthetic"}
	r = run(t, e, &App{API: api}, "attach", "ses_x")
	expectCode(t, r, 0)
	if !strings.Contains(r.stdout, "https://hub.synthetic/v1/terminal") || strings.Contains(r.stdout+r.stderr, "pyt_secret") {
		t.Fatalf("web terminal hint: %s", r)
	}
}

// ---- documents ----

func TestDocEditConflictKeepsDraft(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h).with("EDITOR", "fake-editor --wait", "VISUAL", "")
	other := c.WithPrincipal(context.Background(), c.Principal{Kind: c.PrincipalSession, ID: "ses_synth-a"})

	// Success path: the editor changes the text; the proposal commits; the draft is removed.
	app := &App{Exec: func(argv []string) error {
		if len(argv) != 3 || argv[0] != "fake-editor" || argv[1] != "--wait" {
			t.Errorf("editor argv %q", argv)
		}
		return appendFile(argv[2], "edited once\n")
	}}
	r := run(t, e, app, "--json", "doc", "edit", "brief", "--project", "prj_synth-outfit")
	expectCode(t, r, 0)
	res := mustJSON[c.DocumentWithRevision](t, r)
	if !strings.HasSuffix(res.Revision.Content, "edited once\n") {
		t.Fatalf("edit not applied: %q", res.Revision.Content)
	}
	if files, _ := filepath.Glob(filepath.Join(e[EnvStateDir], "drafts", "*")); len(files) != 0 {
		t.Fatalf("draft not cleaned up after success: %v", files)
	}

	// Conflict path: someone else commits while the editor is open.
	var draft string
	app = &App{Exec: func(argv []string) error {
		draft = argv[len(argv)-1]
		head, err := h.b.GetDocument(other, c.GetDocumentRequest{DocumentID: res.Document.DocumentID})
		if err != nil {
			return err
		}
		if _, err := h.b.ProposeRevision(other, c.ProposeRevisionRequest{Mutation: c.Mutation{OperationID: "op_concurrent"}, DocumentID: head.Document.DocumentID, BaseRevisionID: head.Revision.RevisionID, Content: "concurrent edit\n"}); err != nil {
			return err
		}
		return appendFile(draft, "my precious edit\n")
	}}
	r = run(t, e, app, "doc", "edit", res.Document.DocumentID)
	expectCode(t, r, ExitConflict)
	b, err := os.ReadFile(draft)
	if err != nil || !strings.Contains(string(b), "my precious edit") {
		t.Fatalf("draft must be kept on conflict: %v %q", err, b)
	}
	if !strings.Contains(r.stdout, "NOT applied") || !strings.Contains(r.stdout, draft) || !strings.Contains(r.stdout, "prp_") {
		t.Fatalf("conflict output must name the proposal and draft: %s", r)
	}
	// The hub preserved the proposal with our text.
	props, err := h.b.ListProposals(other, c.ListProposalsRequest{DocumentID: res.Document.DocumentID, States: []c.ProposalState{c.ProposalConflicted}})
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, p := range props.Items {
		kept = kept || strings.Contains(p.Content, "my precious edit")
	}
	if !kept {
		t.Fatal("conflicting edit not preserved as a proposal")
	}
	head, _ := h.b.GetDocument(other, c.GetDocumentRequest{DocumentID: res.Document.DocumentID})
	if head.Revision.Content != "concurrent edit\n" {
		t.Fatalf("the other writer's revision must remain head: %q", head.Revision.Content)
	}

	// Non-interactive (agent) path with an explicit stale base: JSON conflict body + exit 3.
	f := filepath.Join(t.TempDir(), "new.md")
	os.WriteFile(f, []byte("agent text\n"), 0o600)
	r = run(t, e, nil, "--json", "doc", "edit", res.Document.DocumentID, "--file", f, "--base", res.Revision.RevisionID)
	expectCode(t, r, ExitConflict)
	if out := mustJSON[DocEditResult](t, r); out.ProposalID == "" || out.CurrentHead != head.Revision.RevisionID {
		t.Fatalf("doc edit conflict json: %+v", out)
	}
}

func appendFile(p, s string) error {
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(s)
	return err
}

// ---- publish ----

type tamperAPI struct {
	c.API
	mode string // "reject" | "wrong-digest"
}

func (f tamperAPI) FinalizeUpload(ctx context.Context, req c.FinalizeUploadRequest) (c.PublishReceipt, error) {
	if f.mode == "reject" {
		return c.PublishReceipt{}, c.Errorf(c.ErrConflict, "uploaded bytes do not match the declared digest")
	}
	rec, err := f.API.FinalizeUpload(ctx, req)
	rec.Version.Digest = c.Digest("sha256:" + strings.Repeat("0", 64))
	return rec, err
}

func TestPublishReceiptAndDigestMismatch(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h).with(EnvSessionID, "ses_synth-b", EnvRunID, "run_synth-b-1", EnvNodeID, "node_synth-linux")
	file := filepath.Join(t.TempDir(), "report.md")
	data := []byte("# synthetic report\n")
	os.WriteFile(file, data, 0o600)
	sum := sha256.Sum256(data)

	r := run(t, e, nil, "--json", "publish", file, "--title", "Report", "--review")
	expectCode(t, r, 0)
	rec := mustJSON[c.PublishReceipt](t, r)
	if string(rec.Version.Digest) != "sha256:"+hex.EncodeToString(sum[:]) || rec.Version.Size != int64(len(data)) {
		t.Fatalf("receipt digest: %+v", rec.Version)
	}
	if !rec.Version.ReviewRequested || rec.Version.Provenance.SessionID != "ses_synth-b" || rec.Version.Provenance.RunID != "run_synth-b-1" {
		t.Fatalf("review flag / provenance: %+v", rec.Version)
	}

	// The published bytes come back exactly, verified.
	got := filepath.Join(t.TempDir(), "got.md")
	r = run(t, e, nil, "artifact", "get", rec.Version.VersionID, "-o", got)
	expectCode(t, r, 0)
	if b, _ := os.ReadFile(got); string(b) != string(data) {
		t.Fatalf("downloaded bytes differ: %q", b)
	}
	// ...and from the verified cache when the hub is down.
	got2 := filepath.Join(t.TempDir(), "got2.md")
	r = run(t, e.with(EnvHub, "http://127.0.0.1:1"), nil, "artifact", "get", rec.Version.VersionID, "-o", got2)
	expectCode(t, r, 0)
	if b, _ := os.ReadFile(got2); string(b) != string(data) || !strings.Contains(r.stderr, "local cache") {
		t.Fatalf("offline cache read: %q %s", b, r)
	}

	// Hub rejects the bytes: nothing reported as published.
	for _, mode := range []string{"reject", "wrong-digest"} {
		b := mock.New(mock.Options{DefaultPrincipal: &testOwner})
		r = run(t, e, &App{API: tamperAPI{API: b, mode: mode}}, "publish", file)
		if r.code == 0 || strings.Contains(r.stdout, "published") {
			t.Fatalf("%s: must not report published: %s", mode, r)
		}
		if !strings.Contains(r.stderr, "not published") {
			t.Fatalf("%s: should say the file remains local: %s", mode, r)
		}
		r = run(t, e, &App{API: tamperAPI{API: b, mode: mode}}, "--json", "publish", file)
		if r.code == 0 || r.stdout != "" {
			t.Fatalf("%s --json: stdout must be empty on failure: %s", mode, r)
		}
	}
}

// ---- watch ----

func TestWatchNotifiesNewAttention(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	notified := make(chan string, 4)
	app := &App{Notify: func(title, body string) error { notified <- title + ": " + body; return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan result, 1)
	go func() {
		var out, errb strings.Builder
		app.Stdout, app.Stderr, app.Getenv, app.Stdin = &syncWriter{w: &out}, &syncWriter{w: &errb}, e.get, strings.NewReader("")
		code := app.Main(ctx, []string{"watch"})
		done <- result{stdout: out.String(), stderr: errb.String(), code: code}
	}()
	// Keep raising until the watcher (which starts at the head) sees one.
	deadline := time.After(15 * time.Second)
	for i := 0; ; i++ {
		if _, err := h.b.RequestApproval("run_synth-b-1", mock.ApprovalSpec{Title: "Synthetic approval " + itoa(i), Options: []c.ApprovalOption{{ID: "allow", Label: "Allow"}}}); err != nil {
			t.Fatal(err)
		}
		select {
		case msg := <-notified:
			if !strings.Contains(msg, "Synthetic approval") {
				t.Fatalf("notification: %q", msg)
			}
			cancel()
			r := <-done
			expectCode(t, r, 0)
			if !strings.Contains(r.stdout, "attention.changed") {
				t.Fatalf("event stream output: %s", r)
			}
			return
		case <-time.After(300 * time.Millisecond):
		case <-deadline:
			t.Fatal("no desktop notification for a new attention item")
		}
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *strings.Builder
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func TestReadFollowStreamsNewEntries(t *testing.T) {
	h := newHub(t)
	e := newEnv(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out strings.Builder
	sw := &syncWriter{w: &out}
	app := &App{Stdout: sw, Stderr: &syncWriter{w: &strings.Builder{}}, Getenv: e.get, Stdin: strings.NewReader(""), PollInterval: 20 * time.Millisecond}
	done := make(chan int, 1)
	go func() { done <- app.Main(ctx, []string{"--json", "read", "ses_synth-b", "--tail", "2", "--follow"}) }()
	time.Sleep(200 * time.Millisecond)
	owner := c.WithPrincipal(context.Background(), testOwner)
	m, err := h.b.SendMessage(owner, c.SendMessageRequest{Mutation: c.Mutation{OperationID: "op_follow"}, Recipient: c.Address{Kind: c.AddrSession, ID: "ses_synth-b"}, Body: c.MessageBody{Text: "follow me"}})
	if err != nil {
		t.Fatal(err)
	}
	h.b.Settle(0)
	deadline := time.Now().Add(10 * time.Second)
	for {
		sw.mu.Lock()
		s := out.String()
		sw.mu.Unlock()
		if strings.Contains(s, m.Envelope.MessageID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("follow never showed the delivered message:\n%s", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("follow exit %d", code)
	}
	// Each line is one ReadSessionResponse (NDJSON).
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var resp c.ReadSessionResponse
		if err := json.Unmarshal([]byte(line), &resp); err != nil {
			t.Fatalf("follow --json line is not a ReadSessionResponse: %q", line)
		}
	}
}
