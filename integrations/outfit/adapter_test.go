package outfit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhh3/pyre/integrations/outfit/fakeoutfit"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/credentials"
)

func TestMain(m *testing.M) {
	fakeoutfit.RunIfRequested()
	os.Exit(m.Run())
}

const synthSecret = "SYNTHETIC-outfit-held-secret-55aa"

func baseState() fakeoutfit.State {
	verified := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return fakeoutfit.State{
		Version:  "0.10.7+replacefix",
		Revision: "c8a3dd7+working",
		Connections: []fakeoutfit.Connection{
			{ID: "con_1", Name: "github-work", Connector: "github", Mode: "rest", Method: "managed-token", Custody: "outfit", Status: "active", Env: []string{"GH_TOKEN", "GITHUB_TOKEN"}, Secret: synthSecret, VerifiedAt: &verified,
				Operations: map[string]json.RawMessage{"get_issue": json.RawMessage(`{"title":"synthetic issue"}`), "big": json.RawMessage(`"BIG:"`)}, WriteOps: []string{"create_issue"}},
			{ID: "con_2", Name: "linear_me", Connector: "linear", Mode: "mcp", Method: "oauth", Custody: "oauth", Status: "active"},
		},
		Tools:  []fakeoutfit.Tool{{ID: "gh", Version: "2.80.0", Platform: "darwin-arm64", Path: "/opt/outfit/tools/gh", SHA256: "deadbeef", Current: true}},
		Skills: []fakeoutfit.Skill{{ID: "github-pr", Connection: "github-work", Connector: "github", Host: "claude", Kind: "generated", Source: "connector", Ref: "abc123", Digest: "feed", Path: "/home/u/.claude/skills/github-pr"}},
	}
}

type fake struct {
	t         *testing.T
	state     string
	log       string
	adapter   *Adapter
	stateData fakeoutfit.State
}

func newFake(t *testing.T, st fakeoutfit.State, mod func(*Options)) *fake {
	t.Helper()
	dir := t.TempDir()
	f := &fake{t: t, state: filepath.Join(dir, "state.json"), log: filepath.Join(dir, "calls.log"), stateData: st}
	if err := fakeoutfit.Save(f.state, st); err != nil {
		t.Fatal(err)
	}
	opt := Options{Binary: os.Args[0], Home: filepath.Join(dir, "outfit-home"), ExtraEnv: fakeoutfit.Env(f.state, f.log)}
	if mod != nil {
		mod(&opt)
	}
	f.adapter = New(opt)
	return f
}

func (f *fake) update(fn func(*fakeoutfit.State)) {
	fn(&f.stateData)
	if err := fakeoutfit.Save(f.state, f.stateData); err != nil {
		f.t.Fatal(err)
	}
}

type call struct {
	Argv       []string `json:"argv"`
	StdinBytes int      `json:"stdin_bytes"`
	PyreToken  bool     `json:"env_has_pyre_token"`
}

func (f *fake) calls() []call {
	file, err := os.Open(f.log)
	if err != nil {
		return nil
	}
	defer file.Close()
	var out []call
	sc := bufio.NewScanner(file)
	for sc.Scan() {
		var c call
		json.Unmarshal(sc.Bytes(), &c)
		out = append(out, c)
	}
	return out
}

func TestDetect(t *testing.T) {
	f := newFake(t, baseState(), nil)
	st := f.adapter.Detect(context.Background())
	if !st.Present || st.Version != "0.10.7+replacefix" || st.Revision != "c8a3dd7+working" || !st.Dirty || st.Health != "ok" {
		t.Fatalf("detect = %+v", st)
	}
	if ic := st.Integration(); !ic.Present || ic.Version != st.Version {
		t.Fatalf("integration = %+v", ic)
	}
	absent := New(Options{Binary: "definitely-not-outfit-xyz"}).Detect(context.Background())
	if absent.Present || !strings.HasPrefix(absent.Health, "absent") {
		t.Fatalf("absent = %+v", absent)
	}
}

func TestConnectionsFromInventoryAreMetadataOnly(t *testing.T) {
	f := newFake(t, baseState(), nil)
	list, err := f.adapter.Connections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "github-work" || list[0].Custody != "outfit" || strings.Join(list[0].Environment, ",") != "GH_TOKEN,GITHUB_TOKEN" {
		t.Fatalf("list = %+v", list)
	}
	raw, _ := json.Marshal(list)
	if strings.Contains(string(raw), synthSecret) {
		t.Fatal("connection metadata contains a secret")
	}
	c := list[0].Contract("node_mac", time.Now())
	if c.ConnectionID != "conn_github-work" || c.Service != "github" || c.Availability[0].State != "verified" || c.Availability[0].Mode != ModeRemoteInvoke {
		t.Fatalf("contract = %+v", c)
	}
	if id := ConnectionIDFor("linear_me"); !contracts.ValidID(id, contracts.PrefixConnection) {
		t.Fatalf("invalid id %q", id)
	} else if name, ok := NameForConnectionID(id); !ok || name != "linear_me" {
		t.Fatalf("inverse = %q", name)
	}
}

func TestConnectionsFallBackToLsAndRememberIt(t *testing.T) {
	st := baseState()
	st.NoInventory = true
	f := newFake(t, st, nil)
	for i := 0; i < 2; i++ {
		list, err := f.adapter.Connections(context.Background())
		if err != nil || len(list) != 2 {
			t.Fatalf("list = %+v %v", list, err)
		}
		if av := list[0].Contract("n", time.Now()).Availability[0]; av.State != "unknown" {
			t.Fatalf("ls fallback cannot claim verification: %+v", av)
		}
	}
	inv := 0
	for _, c := range f.calls() {
		if c.Argv[0] == "inventory" {
			inv++
		}
	}
	if inv != 1 {
		t.Fatalf("inventory probed %d times; want 1", inv)
	}
}

func TestCheckClassifiesAndRedacts(t *testing.T) {
	f := newFake(t, baseState(), nil)
	ctx := context.Background()
	if c, err := f.adapter.Check(ctx, "github-work"); err != nil || c.State != "verified" {
		t.Fatalf("check = %+v %v", c, err)
	}
	if c, _ := f.adapter.Check(ctx, "nope"); c.State != "missing" || c.Detail != `connection "nope" is not configured in Outfit on this node` {
		t.Fatalf("missing = %+v", c)
	}
	f.update(func(s *fakeoutfit.State) { s.Connections[0].Expired = true; s.Leak = true })
	c, err := f.adapter.Check(ctx, "github-work")
	if err != nil || c.State != "expired" || strings.Contains(c.Detail, synthSecret) {
		t.Fatalf("expired/redaction = %+v %v", c, err)
	}
	f.update(func(s *fakeoutfit.State) { s.Connections[0].Expired = false; s.Locked = true })
	if c, _ := f.adapter.Check(ctx, "github-work"); c.State != "locked_store" {
		t.Fatalf("locked = %+v", c)
	}
	if _, err := f.adapter.Check(ctx, "--help"); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("flag-like names must be rejected: %v", err)
	}
}

func TestInvokeArgsOnStdinBoundedAndClassified(t *testing.T) {
	t.Setenv("PYRE_SESSION_TOKEN", "SYNTHETIC-session-token")
	f := newFake(t, baseState(), nil)
	ctx := context.Background()
	res, err := f.adapter.Invoke(ctx, InvokeRequest{Connection: "github-work", Operation: "get_issue", Args: map[string]string{"owner": "acme", "number": "SYNTHETIC-arg-value-42"}})
	if err != nil || !res.OK || res.Truncated {
		t.Fatalf("invoke = %+v %v", res, err)
	}
	if !strings.Contains(string(res.Envelope), "synthetic issue") || !strings.Contains(string(res.Envelope), "SYNTHETIC-arg-value-42") {
		t.Fatalf("envelope = %s", res.Envelope)
	}
	for _, c := range f.calls() {
		if strings.Contains(strings.Join(c.Argv, " "), "SYNTHETIC-arg-value-42") {
			t.Fatalf("argument value reached argv: %v", c.Argv)
		}
		if c.PyreToken {
			t.Fatal("Pyre session token leaked into the outfit environment")
		}
		if c.Argv[0] == "run" && c.StdinBytes == 0 {
			t.Fatal("arguments must be passed on stdin")
		}
	}
	res, err = f.adapter.Invoke(ctx, InvokeRequest{Connection: "github-work", Operation: "create_issue"})
	if err != nil || res.OK || !contracts.IsCode(res.Err(), contracts.ErrForbidden) {
		t.Fatalf("write must be refused: %+v %v", res, err)
	}
	res, _ = f.adapter.Invoke(ctx, InvokeRequest{Connection: "github-work", Operation: "nope"})
	if !contracts.IsCode(res.Err(), contracts.ErrNotFound) {
		t.Fatalf("unknown op: %v", res.Err())
	}
	res, err = f.adapter.Invoke(ctx, InvokeRequest{Connection: "github-work", Operation: "big"})
	if err != nil || !res.Truncated || strings.Contains(string(res.Envelope), "outfit-result-fake") {
		t.Fatalf("spill must be truncated and pathless: %+v %v %s", res, err, res.Envelope)
	}
	if _, err := os.Stat(filepath.Join(os.TempDir(), "outfit-result-fake.json")); !os.IsNotExist(err) {
		t.Fatal("spill file was not removed")
	}
	f.update(func(s *fakeoutfit.State) { s.Connections[0].Expired = true; s.Leak = true })
	res, _ = f.adapter.Invoke(ctx, InvokeRequest{Connection: "github-work", Operation: "get_issue"})
	if e := res.Err(); e == nil || strings.Contains(e.Error(), synthSecret) || strings.Contains(string(res.Envelope), synthSecret) && res.Failure != nil && strings.Contains(res.Failure.Message, synthSecret) {
		t.Fatalf("failure leaked a secret: %v", e)
	}
	for _, bad := range []InvokeRequest{{Connection: "-x", Operation: "a"}, {Connection: "a", Operation: "--write"}, {Connection: "a", Operation: "b", Args: map[string]string{"--write": "1"}}} {
		if _, err := f.adapter.Invoke(ctx, bad); !contracts.IsCode(err, contracts.ErrInvalid) {
			t.Fatalf("expected invalid for %+v: %v", bad, err)
		}
	}
}

func TestInvokeTimeoutIsIndeterminate(t *testing.T) {
	st := baseState()
	st.Connections[0].SleepMillis = 3000
	f := newFake(t, st, func(o *Options) { o.InvokeTimeout = 300 * time.Millisecond })
	start := time.Now()
	_, err := f.adapter.Invoke(context.Background(), InvokeRequest{Connection: "github-work", Operation: "get_issue"})
	if !contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) {
		t.Fatalf("timeout = %v", err)
	}
	if time.Since(start) > 2500*time.Millisecond {
		t.Fatal("timeout did not stop the child promptly")
	}
}

func TestInventoryBothSources(t *testing.T) {
	for _, noInv := range []bool{false, true} {
		st := baseState()
		st.NoInventory = noInv
		f := newFake(t, st, nil)
		inv, err := f.adapter.Inventory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.Tools) != 1 || inv.Tools[0].Tool != "gh" || inv.Tools[0].State != "active" || inv.Tools[0].Path != "/opt/outfit/tools/gh" {
			t.Fatalf("noInv=%v tools = %+v", noInv, inv.Tools)
		}
		if len(inv.Skills) != 1 || inv.Skills[0].Name != "github-pr" || inv.Skills[0].Harness != contracts.HarnessClaude || inv.Skills[0].Version != "abc123" {
			t.Fatalf("noInv=%v skills = %+v", noInv, inv.Skills)
		}
		if want := map[bool]string{false: "inventory", true: "text"}[noInv]; inv.Source != want {
			t.Fatalf("source = %s", inv.Source)
		}
	}
}

func TestPlanSession(t *testing.T) {
	now := time.Now()
	c := contracts.Connection{ConnectionID: "conn_github-work", Name: "github-work", Service: "github", Modes: []string{ModeLocalCredential, ModeRemoteInvoke},
		Availability: []contracts.ConnectionAvailability{
			{NodeID: "node_server", State: "verified", Mode: ModeRemoteInvoke, VerifiedAt: now},
			{NodeID: "node_mac", State: "verified", Mode: ModeRemoteInvoke, VerifiedAt: now.Add(-time.Hour)},
			{NodeID: "node_old", State: "expired", Mode: ModeRemoteInvoke, VerifiedAt: now.Add(time.Hour)},
		}}
	ref := credentials.NewRef(credentials.BackendKeychain, c.ConnectionID, "GH_TOKEN")
	p, err := PlanSession(c, LocalFacts{Provisions: []credentials.Provision{{ConnectionID: c.ConnectionID, EnvVar: "GH_TOKEN", Ref: ref.String()}}}, "node_mac")
	if err != nil || p.Mode != ModeLocalCredential || len(p.Grants) != 1 || p.Grants[0].Ref != ref || !strings.Contains(p.Guide, "$GH_TOKEN") {
		t.Fatalf("local plan = %+v %v", p, err)
	}
	p, err = PlanSession(c, LocalFacts{OutfitHasIt: true}, "node_mac")
	if err != nil || p.Mode != ModeRemoteInvoke || p.NodeID != "node_mac" {
		t.Fatalf("prefer local outfit = %+v %v", p, err)
	}
	p, err = PlanSession(c, LocalFacts{}, "node_other")
	if err != nil || p.NodeID != "node_server" {
		t.Fatalf("most recent verified = %+v %v", p, err)
	}
	c.Availability = c.Availability[2:]
	if _, err := PlanSession(c, LocalFacts{}, "node_other"); !contracts.IsCode(err, contracts.ErrUnavailable) {
		t.Fatalf("unverified must be unavailable: %v", err)
	}
}
