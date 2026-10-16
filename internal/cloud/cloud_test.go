package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Synthetic secrets: obviously fake, and asserted never to reach argv.
const (
	fakeEnrollSecret = "pye_SYNTHETIC-enroll-secret-0000"
	fakeTSKey        = "tskey-auth-SYNTHETIC-0000"
	fakeSigners      = "pyre-release namespaces=\"pyre-release\" ssh-ed25519 AAAASYNTHETICKEY pyre-release\n"
)

// fakeGcloud records every invocation and answers from a tiny state model.
type fakeGcloud struct {
	mu        sync.Mutex
	calls     [][]string
	instances map[string]*gceInstance
	firewall  bool
	families  map[string]bool
	disks     []Disk
	files     map[string]string // metadata-from-file contents captured at call time
	onCreate  func()
	resetPW   string
}

func newFake() *fakeGcloud {
	return &fakeGcloud{instances: map[string]*gceInstance{}, families: map[string]bool{}, files: map[string]string{}}
}

func flagVal(args []string, name string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, name+"="); ok {
			return v
		}
	}
	return ""
}

func notFound() error { return &GcloudError{Msg: "ERROR: (gcloud) The resource was not found"} }

func (f *fakeGcloud) Run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	if v := flagVal(args, "--metadata-from-file"); v != "" {
		for _, kv := range strings.Split(v, ",") {
			k, p, _ := strings.Cut(kv, "=")
			b, _ := os.ReadFile(p)
			f.files[k] = string(b)
		}
	}
	j := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(j, "config get-value"):
		return []byte("(unset)\n"), nil
	case strings.HasPrefix(j, "compute firewall-rules describe"):
		if f.firewall {
			return []byte("{}"), nil
		}
		return nil, notFound()
	case strings.HasPrefix(j, "compute firewall-rules create"):
		f.firewall = true
		return nil, nil
	case strings.HasPrefix(j, "compute images describe-from-family"):
		if f.families[args[3]] {
			return []byte("{}"), nil
		}
		return nil, notFound()
	case strings.HasPrefix(j, "compute images create"):
		return nil, nil
	case strings.HasPrefix(j, "compute instances describe"):
		gi := f.instances[args[3]]
		if gi == nil {
			return nil, notFound()
		}
		return json.Marshal(gi)
	case strings.HasPrefix(j, "compute instances list"):
		var all []gceInstance
		for _, gi := range f.instances {
			all = append(all, *gi)
		}
		return json.Marshal(all)
	case strings.HasPrefix(j, "compute instances create"):
		gi := &gceInstance{Name: args[3], Zone: "zones/" + flagVal(args, "--zone"), MachineType: "machineTypes/" + flagVal(args, "--machine-type"),
			Status: "RUNNING", Labels: map[string]string{}, LastStartTimestamp: time.Now().UTC().Format(time.RFC3339)}
		for _, kv := range strings.Split(flagVal(args, "--labels"), ",") {
			k, v, _ := strings.Cut(kv, "=")
			gi.Labels[k] = v
		}
		if strings.HasPrefix(args[3], "pyre-bake-") {
			gi.Status = "TERMINATED" // the builder powers itself off
		}
		f.instances[args[3]] = gi
		if f.onCreate != nil {
			f.onCreate()
		}
		return []byte("[]"), nil
	case strings.HasPrefix(j, "compute instances stop"):
		f.instances[args[3]].Status = "TERMINATED"
		return nil, nil
	case strings.HasPrefix(j, "compute instances start"):
		f.instances[args[3]].Status = "RUNNING"
		return nil, nil
	case strings.HasPrefix(j, "compute instances delete"):
		delete(f.instances, args[3])
		return nil, nil
	case strings.HasPrefix(j, "compute instances add-metadata"), strings.HasPrefix(j, "compute instances remove-metadata"),
		strings.HasPrefix(j, "compute disks add-labels"), strings.HasPrefix(j, "compute disks delete"):
		return nil, nil
	case strings.HasPrefix(j, "compute disks list"):
		return json.Marshal(f.disks)
	case strings.HasPrefix(j, "compute reset-windows-password"):
		return json.Marshal(map[string]string{"username": flagVal(args, "--user"), "password": f.resetPW, "ip_address": "203.0.113.9"})
	}
	return nil, errors.New("fake gcloud: unexpected " + j)
}

func (f *fakeGcloud) called(prefix string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeGcloud) assertNoSecretsInArgv(t *testing.T, secrets ...string) {
	t.Helper()
	for _, c := range f.calls {
		j := strings.Join(c, " ")
		for _, s := range secrets {
			if s != "" && strings.Contains(j, s) {
				t.Fatalf("secret in gcloud argv: %s", j)
			}
		}
	}
}

type fakeHub struct {
	mu        sync.Mutex
	nodes     map[string]*contracts.Node
	sessions  []contracts.SessionView
	worktrees map[string]contracts.SessionWorktrees
	revoked   []string
	tickets   int
}

func (h *fakeHub) CreateEnrollment(_ context.Context, name string, ttl time.Duration) (contracts.EnrollmentTicket, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tickets++
	if ttl <= 0 || ttl > time.Hour {
		return contracts.EnrollmentTicket{}, errors.New("bad ttl")
	}
	h.nodes[name] = &contracts.Node{NodeID: "node_synth-" + name, Name: name, Connection: contracts.NodeConnection{State: contracts.ConnNever}}
	return contracts.EnrollmentTicket{NodeID: "node_synth-" + name, Secret: fakeEnrollSecret, HubURL: "https://hub.lan.invalid:7443"}, nil
}

func (h *fakeHub) FindNode(_ context.Context, name string) (*contracts.Node, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := h.nodes[name]
	if n == nil {
		return nil, nil
	}
	cp := *n
	return &cp, nil
}

func (h *fakeHub) connect(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nodes[name].Connection.State = contracts.ConnConnected
}

func (h *fakeHub) NodeSessions(_ context.Context, nodeID string) ([]contracts.SessionView, error) {
	var out []contracts.SessionView
	for _, s := range h.sessions {
		if s.Run != nil && s.Run.NodeID == nodeID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (h *fakeHub) SessionWorktrees(_ context.Context, sv contracts.SessionView, _ bool) (contracts.SessionWorktrees, error) {
	if sw, ok := h.worktrees[sv.Session.SessionID]; ok {
		return sw, nil
	}
	return contracts.SessionWorktrees{}, contracts.Errorf(contracts.ErrNotFound, "no report")
}

func (h *fakeHub) RevokeNode(_ context.Context, nodeID, _ string) error {
	h.revoked = append(h.revoked, nodeID)
	return nil
}

type fakeKeys struct {
	minted, revoked []string
}

func (k *fakeKeys) Mint(context.Context, string) (AuthKey, error) {
	k.minted = append(k.minted, "k1")
	return AuthKey{ID: "kSYNTH1", Key: fakeTSKey}, nil
}
func (k *fakeKeys) Revoke(_ context.Context, id string) error {
	k.revoked = append(k.revoked, id)
	return nil
}

func newManager(t *testing.T) (*Manager, *fakeGcloud, *fakeHub, *fakeKeys) {
	g := newFake()
	h := &fakeHub{nodes: map[string]*contracts.Node{}, worktrees: map[string]contracts.SessionWorktrees{}}
	k := &fakeKeys{}
	m := &Manager{GCP: &GCP{Run: g, Project: "synthetic-project"}, Hub: h, Keys: k, Zone: "zz-test1-a",
		HubPin: strings.Repeat("ab", 32), AllowedSigners: fakeSigners, Out: io.Discard, Poll: time.Millisecond,
		TempDir: t.TempDir(), Cache: &Cache{Path: filepath.Join(t.TempDir(), CacheFileName)},
		Cfg: Config{HubURL: "https://hub.synthetic-tailnet.ts.net:7443",
			Release: ReleaseConfig{URL: "gs://synthetic-bucket/pyre/0.0.0-test/", Version: "0.0.0-test"}}}
	return m, g, h, k
}

func TestCreateLinuxKeepsSecretsOutOfArgvAndCleansUp(t *testing.T) {
	m, g, h, k := newManager(t)
	g.onCreate = func() { go h.connect("dev1") }
	res, err := m.Create(context.Background(), CreateOptions{Name: "dev1"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Connected || res.NodeID != "node_synth-dev1" || res.Instance.MachineType != DefaultMachineType {
		t.Fatalf("result: %+v", res)
	}
	g.assertNoSecretsInArgv(t, fakeEnrollSecret, fakeTSKey)
	cr := g.called("compute instances create")
	if len(cr) != 1 {
		t.Fatalf("creates: %v", cr)
	}
	args := cr[0]
	for _, want := range []string{"--labels=pyre-managed=true,pyre-node=dev1,pyre-os=linux", "--tags=pyre-node",
		"--image-family=" + DefaultBaseImage, "--image-project=" + DefaultImageProject, "--boot-disk-size=200GB",
		"--scopes=https://www.googleapis.com/auth/devstorage.read_only", "--project=synthetic-project", "--zone=zz-test1-a"} {
		if !contains(args, want) {
			t.Errorf("create args miss %s: %v", want, args)
		}
	}
	if g.files[MetaEnrollSecret] != fakeEnrollSecret || g.files[MetaTSAuthKey] != fakeTSKey {
		t.Fatalf("secrets not passed by file: %v", g.files)
	}
	if !strings.Contains(g.files[MetaStartupScript], "pyre-bootstrap") {
		t.Fatal("startup script not passed")
	}
	var bc BootConfig
	if err := json.Unmarshal([]byte(g.files[MetaConfig]), &bc); err != nil {
		t.Fatal(err)
	}
	if bc.NodeID != "node_synth-dev1" || bc.HubURL != m.Cfg.HubURL || bc.HubPin != m.HubPin || bc.AllowedSigners != fakeSigners || bc.TSHostname != "pyre-dev1" || !bc.TSSSH {
		t.Fatalf("boot config: %+v", bc)
	}
	if strings.Contains(g.files[MetaConfig], fakeEnrollSecret) || strings.Contains(g.files[MetaConfig], fakeTSKey) {
		t.Fatal("pyre-config holds a secret")
	}
	if len(g.called("compute firewall-rules create")) != 1 {
		t.Fatal("deny-ingress firewall rule not ensured")
	}
	fw := strings.Join(g.called("compute firewall-rules create")[0], " ")
	if !strings.Contains(fw, "--action=DENY") || !strings.Contains(fw, "--target-tags=pyre-node") {
		t.Fatalf("firewall rule: %s", fw)
	}
	rm := g.called("compute instances remove-metadata")
	if len(rm) != 1 || !contains(rm[0], "--keys=pyre-enroll-secret,pyre-ts-authkey") {
		t.Fatalf("secrets not removed from metadata: %v", rm)
	}
	if len(k.revoked) != 0 {
		t.Fatalf("key revoked after the node joined (logs an ephemeral device out): %v", k.revoked)
	}
	if len(g.called("compute disks add-labels pyre-dev1")) != 1 {
		t.Fatal("boot disk not labelled")
	}
	// The private secret dir is gone.
	if ents, _ := os.ReadDir(m.TempDir); len(ents) != 0 {
		t.Fatalf("secret files left behind: %v", ents)
	}
	if got := m.Cache.Load()["dev1"]; got.Status != "RUNNING" || got.Cloud != "gcp" {
		t.Fatalf("cache: %+v", got)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestCreateRefusals(t *testing.T) {
	m, g, h, _ := newManager(t)
	ctx := context.Background()
	if _, err := m.Create(ctx, CreateOptions{Name: "Bad_Name"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("bad name: %v", err)
	}
	h.nodes["taken"] = &contracts.Node{NodeID: "node_x", Name: "taken"}
	if _, err := m.Create(ctx, CreateOptions{Name: "taken"}); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("taken: %v", err)
	}
	m2, _, _, _ := newManager(t)
	m2.Cfg.HubURL = ""
	if _, err := m2.Create(ctx, CreateOptions{Name: "x1"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("no hub url: %v", err)
	}
	m3, _, _, _ := newManager(t)
	m3.AllowedSigners = ""
	if _, err := m3.Create(ctx, CreateOptions{Name: "x2"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("no release key: %v", err)
	}
	m4, _, _, _ := newManager(t)
	m4.Keys = nil
	if _, err := m4.Create(ctx, CreateOptions{Name: "x3"}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("no tailscale key: %v", err)
	}
	if _, err := m.Create(ctx, CreateOptions{Name: "x4", GPU: "nvidia-l4:1", MachineType: "e2-standard-8", TypeExplicit: true}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("l4 on e2: %v", err)
	}
	if len(g.called("compute instances create")) != 0 || h.tickets != 0 {
		t.Fatal("a refused create created something")
	}
}

func TestCreateTimeoutStillRemovesSecrets(t *testing.T) {
	m, g, _, _ := newManager(t)
	_, err := m.Create(context.Background(), CreateOptions{Name: "slow", Wait: 30 * time.Millisecond})
	if !contracts.IsCode(err, contracts.ErrOutcomeIndeterminate) {
		t.Fatalf("want indeterminate, got %v", err)
	}
	if len(g.called("compute instances remove-metadata")) != 1 {
		t.Fatal("secrets left in metadata after a timeout")
	}
}

func TestGPUShapes(t *testing.T) {
	cases := []struct {
		o          CreateOptions
		mt, accel  string
		shouldFail bool
	}{
		{CreateOptions{GPU: "nvidia-l4:1"}, "g2-standard-8", "", false},
		{CreateOptions{GPU: "nvidia-l4:2"}, "g2-standard-24", "", false},
		{CreateOptions{GPU: "nvidia-l4:1", MachineType: "g2-standard-4", TypeExplicit: true}, "g2-standard-4", "", false},
		{CreateOptions{GPU: "nvidia-tesla-t4:1"}, "n1-standard-8", "type=nvidia-tesla-t4,count=1", false},
		{CreateOptions{GPU: "nvidia-l4:3"}, "", "", true},
		{CreateOptions{}, "e2-standard-8", "", false},
	}
	for _, tc := range cases {
		mt, accel, err := resolveShape(tc.o, "e2-standard-8")
		if (err != nil) != tc.shouldFail || mt != tc.mt || accel != tc.accel {
			t.Errorf("%+v: got %q %q %v", tc.o, mt, accel, err)
		}
	}
	s := CreateSpec{Name: "pyre-g", Zone: "z", MachineType: "g2-standard-8", GPU: true, Spot: true}
	a := strings.Join(s.CreateArgs(), " ")
	for _, want := range []string{"--maintenance-policy=TERMINATE", "--provisioning-model=SPOT", "--instance-termination-action=STOP", "--no-service-account", "--no-scopes"} {
		if !strings.Contains(a, want) {
			t.Errorf("spec args miss %s: %s", want, a)
		}
	}
}

func TestCreateWindowsUsesPowerShellAndOpensNoPort(t *testing.T) {
	m, g, h, _ := newManager(t)
	g.onCreate = func() { go h.connect("win1") }
	res, err := m.Create(context.Background(), CreateOptions{Name: "win1", OS: OSWindows, RDP: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.OS != OSWindows || res.RunningUSD <= EstimateCost(DefaultMachineType, "", false, 200, "", nil).Hourly(true) {
		t.Fatalf("windows estimate should include the license: %+v", res)
	}
	args := g.called("compute instances create")[0]
	if !contains(args, "--image-family="+DefaultWindowsImage) || !contains(args, "--image-project="+DefaultWindowsImageProject) {
		t.Fatalf("windows image: %v", args)
	}
	if g.files[MetaWindowsStartup] == "" || g.files[MetaStartupScript] != "" {
		t.Fatalf("windows must use %s only: %v", MetaWindowsStartup, keys(g.files))
	}
	var bc BootConfig
	_ = json.Unmarshal([]byte(g.files[MetaConfig]), &bc)
	if !bc.RDP || bc.OS != OSWindows || bc.TSSSH {
		t.Fatalf("boot config: %+v", bc)
	}
	for _, c := range g.calls {
		j := strings.Join(c, " ")
		if strings.Contains(j, "3389") || strings.Contains(j, "--action=ALLOW") {
			t.Fatalf("an inbound port was opened: %s", j)
		}
	}
	g.assertNoSecretsInArgv(t, fakeEnrollSecret, fakeTSKey)
	if _, err := m.Create(context.Background(), CreateOptions{Name: "lin2", RDP: true}); !contracts.IsCode(err, contracts.ErrInvalid) {
		t.Fatalf("--rdp on linux: %v", err)
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestBakedFamilyPreferred(t *testing.T) {
	m, g, h, _ := newManager(t)
	g.families[DefaultBakedFamily] = true
	g.onCreate = func() { go h.connect("fast") }
	if _, err := m.Create(context.Background(), CreateOptions{Name: "fast"}); err != nil {
		t.Fatal(err)
	}
	args := g.called("compute instances create")[0]
	if !contains(args, "--image-family="+DefaultBakedFamily) || !contains(args, "--image-project=synthetic-project") {
		t.Fatalf("baked family not used: %v", args)
	}
}

func liveSession(id, node string, st contracts.RunState) contracts.SessionView {
	return contracts.SessionView{Session: contracts.Session{SessionID: id, Title: "synthetic " + id},
		Run: &contracts.Run{RunID: "run_" + id, SessionID: id, NodeID: node, State: st}}
}

func TestDeleteSafety(t *testing.T) {
	m, g, h, _ := newManager(t)
	ctx := context.Background()
	g.onCreate = func() { go h.connect("del1") }
	if _, err := m.Create(ctx, CreateOptions{Name: "del1"}); err != nil {
		t.Fatal(err)
	}
	nid := "node_synth-del1"
	h.sessions = []contracts.SessionView{liveSession("ses_a", nid, contracts.RunRunning), liveSession("ses_b", nid, contracts.RunExited)}
	h.worktrees["ses_b"] = contracts.SessionWorktrees{Repos: []contracts.RepoWorktrees{{Name: "r", Path: "/synthetic/r", Worktrees: []contracts.Worktree{
		{Path: "/synthetic/r", Main: true},
		{Path: "/synthetic/r/.wt/a", Branch: "feat-a", CreatedInSession: true, Dirty: true},
		{Path: "/synthetic/r/.wt/b", Branch: "feat-b", CreatedInSession: true},
		{Path: "/synthetic/r/.wt/c", Branch: "feat-c", CreatedInSession: true, Pushed: true},
	}}}}
	_, err := m.Delete(ctx, "del1", false)
	if !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("delete must refuse: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"live run: session ses_a", "uncommitted changes: /synthetic/r/.wt/a", "branch feat-b neither merged nor pushed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal misses %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "feat-c") {
		t.Errorf("a pushed branch is safe: %s", msg)
	}
	if len(g.called("compute instances delete")) != 0 || len(h.revoked) != 0 {
		t.Fatal("refused delete deleted something")
	}
	if _, err := m.Stop(ctx, "del1", false); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("stop with a live run must refuse: %v", err)
	}
	g.disks = []Disk{{Name: "pyre-del1-extra", Zone: "zz-test1-a"}}
	res, err := m.Delete(ctx, "del1", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Instance != "pyre-del1" || len(res.Forced) != 3 || res.RevokedNode != nid || len(res.Disks) != 1 {
		t.Fatalf("forced delete: %+v", res)
	}
	if d := g.called("compute instances delete")[0]; !contains(d, "--delete-disks=all") {
		t.Fatalf("delete args: %v", d)
	}
	if _, ok := m.Cache.Load()["del1"]; ok {
		t.Fatal("cache entry kept")
	}
}

func TestDeleteRefusesForeignInstance(t *testing.T) {
	m, g, _, _ := newManager(t)
	g.instances["pyre-other"] = &gceInstance{Name: "pyre-other", Status: "RUNNING", Labels: map[string]string{"team": "x"}}
	if _, err := m.Delete(context.Background(), "other", true); !contracts.IsCode(err, contracts.ErrConflict) {
		t.Fatalf("unmanaged instance: %v", err)
	}
}

func TestStopStartRekeys(t *testing.T) {
	m, g, h, k := newManager(t)
	ctx := context.Background()
	g.onCreate = func() { go h.connect("ss1") }
	if _, err := m.Create(ctx, CreateOptions{Name: "ss1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Stop(ctx, "ss1", false); err != nil {
		t.Fatal(err)
	}
	if g.instances["pyre-ss1"].Status != "TERMINATED" {
		t.Fatal("not stopped")
	}
	g.files = map[string]string{}
	h.nodes["ss1"].Connection.State = contracts.ConnDisconnected
	go func() { time.Sleep(5 * time.Millisecond); h.connect("ss1") }()
	if _, err := m.Start(ctx, "ss1", time.Second, false); err != nil {
		t.Fatal(err)
	}
	if len(k.minted) != 2 || g.files[MetaTSAuthKey] != fakeTSKey {
		t.Fatalf("start must mint a fresh key and pass it by file: minted=%v files=%v", k.minted, keys(g.files))
	}
	if len(g.called("compute instances remove-metadata")) != 2 {
		t.Fatal("start left the key in metadata")
	}
	g.assertNoSecretsInArgv(t, fakeEnrollSecret, fakeTSKey)
}

func TestListJoinsHubAndEstimates(t *testing.T) {
	m, g, _, _ := newManager(t)
	g.instances["pyre-a"] = &gceInstance{Name: "pyre-a", Zone: "zones/zz-test1-a", MachineType: "x/e2-standard-8", Status: "RUNNING",
		Labels: map[string]string{LabelManaged: "true", LabelNode: "a"}, LastStartTimestamp: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)}
	g.instances["pyre-b"] = &gceInstance{Name: "pyre-b", Zone: "zones/zz-test1-a", MachineType: "x/e2-standard-8", Status: "TERMINATED",
		Labels: map[string]string{LabelManaged: "true", LabelNode: "b"}}
	g.instances["pyre-a"].Disks = append(g.instances["pyre-a"].Disks, struct {
		Boot       bool            `json:"boot"`
		DiskSizeGb json.RawMessage `json:"diskSizeGb"`
		Source     string          `json:"source"`
	}{Boot: true, DiskSizeGb: json.RawMessage(`"200"`)})
	ns, err := m.List(context.Background(), []contracts.Node{{NodeID: "node_a", Name: "a", Connection: contracts.NodeConnection{State: contracts.ConnConnected}, Resources: contracts.NodeResources{ActiveRuns: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ns) != 2 || ns[0].Name != "a" || ns[0].Connection != "connected" || ns[0].ActiveRuns != 2 || ns[0].UptimeHours < 1.9 {
		t.Fatalf("list: %+v", ns)
	}
	if ns[0].HourlyUSD < 0.25 || ns[0].HourlyUSD > 0.35 || ns[1].HourlyUSD >= ns[0].HourlyUSD || ns[1].Connection != "not enrolled" {
		t.Fatalf("estimates: running %.3f stopped %.3f", ns[0].HourlyUSD, ns[1].HourlyUSD)
	}
	if d := CacheReader(m.Cache.Path)("a"); !strings.Contains(d, "e2-standard-8") || !strings.Contains(d, "running") {
		t.Fatalf("cache describe: %q", d)
	}
}

func TestPricing(t *testing.T) {
	e := EstimateCost("e2-standard-8", "", false, 200, "pd-balanced", nil)
	if !e.Known || e.ComputeHourly < 0.26 || e.ComputeHourly > 0.28 {
		t.Fatalf("e2-standard-8: %+v", e)
	}
	if s := EstimateCost("e2-standard-8", "", true, 0, "", nil); s.ComputeHourly > e.ComputeHourly/2 {
		t.Fatalf("spot: %+v", s)
	}
	if g := EstimateCost("g2-standard-8", "nvidia-l4:1", false, 0, "", nil); g.ComputeHourly < 0.8 || g.ComputeHourly > 0.9 {
		t.Fatalf("g2 bundles its GPU: %+v", g)
	}
	if n := EstimateCost("n1-standard-8", "nvidia-tesla-t4:1", false, 0, "", nil); n.ComputeHourly < 0.6 {
		t.Fatalf("n1+t4: %+v", n)
	}
	if u := EstimateCost("zz-weird-1", "", false, 0, "", nil); u.Known {
		t.Fatal("unknown type must be flagged")
	}
	if o := EstimateCost("e2-standard-8", "", false, 0, "", map[string]float64{"e2-standard-8": 1}); o.ComputeHourly != 1 {
		t.Fatal("override ignored")
	}
}

func TestOAuthMinterRequestsSingleUseEphemeralTaggedKey(t *testing.T) {
	var body map[string]any
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v2/oauth/token":
			_ = r.ParseForm()
			if r.Form.Get("client_secret") != "SYNTHETIC-secret" {
				w.WriteHeader(401)
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"SYNTHETIC-access"}`))
		case r.URL.Path == "/api/v2/tailnet/-/keys" && r.Method == http.MethodPost:
			if r.Header.Get("Authorization") != "Bearer SYNTHETIC-access" {
				w.WriteHeader(401)
				return
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"id":"kSYNTH","key":"` + fakeTSKey + `"}`))
		case r.Method == http.MethodDelete:
			deleted = r.URL.Path
		}
	}))
	defer srv.Close()
	o := &OAuthMinter{ClientID: "cid", ClientSecret: "SYNTHETIC-secret", Tag: "tag:pyre-node", TTL: 10 * time.Minute, BaseURL: srv.URL}
	k, err := o.Mint(context.Background(), "pyre node dev1")
	if err != nil || k.Key != fakeTSKey || k.ID != "kSYNTH" {
		t.Fatalf("mint: %+v %v", k, err)
	}
	create := body["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	if create["reusable"] != false || create["ephemeral"] != true || create["preauthorized"] != true || body["expirySeconds"].(float64) != 600 {
		t.Fatalf("key request: %v", body)
	}
	if tags := create["tags"].([]any); len(tags) != 1 || tags[0] != "tag:pyre-node" {
		t.Fatalf("tags: %v", tags)
	}
	if body["description"] != "pyre-node-dev1" {
		t.Fatalf("description: %v", body["description"])
	}
	if err := o.Revoke(context.Background(), "kSYNTH"); err != nil || deleted != "/api/v2/tailnet/-/keys/kSYNTH" {
		t.Fatalf("revoke: %v %s", err, deleted)
	}
}

func TestMinterFromConfig(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if k, err := MinterFromConfig(TailscaleConfig{}, get); k != nil || err != nil {
		t.Fatalf("none configured: %v %v", k, err)
	}
	env[EnvTailscaleAuthKey] = fakeTSKey
	k, _ := MinterFromConfig(TailscaleConfig{}, get)
	if _, ok := k.(*StaticKey); !ok {
		t.Fatalf("static: %T", k)
	}
	if _, err := k.Mint(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Mint(context.Background(), ""); err == nil {
		t.Fatal("a static key is single use")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "secret")
	_ = os.WriteFile(p, []byte("SYNTHETIC\n"), 0o644)
	if _, err := MinterFromConfig(TailscaleConfig{OAuthClientID: "x", OAuthClientSecretFile: p}, get); err == nil {
		t.Fatal("a world-readable secret file must be refused")
	}
	_ = os.Chmod(p, 0o600)
	k, err := MinterFromConfig(TailscaleConfig{OAuthClientID: "x", OAuthClientSecretFile: p}, get)
	if o, ok := k.(*OAuthMinter); err != nil || !ok || o.ClientSecret != "SYNTHETIC" {
		t.Fatalf("oauth: %T %v", k, err)
	}
}

type fakeTS struct{ out string }

func (f fakeTS) Run(context.Context, ...string) ([]byte, error) { return []byte(f.out), nil }

func TestRDPResetHandsPasswordToAutologonAndShowsItOnce(t *testing.T) {
	m, g, h, _ := newManager(t)
	ctx := context.Background()
	g.onCreate = func() { go h.connect("win2") }
	if _, err := m.Create(ctx, CreateOptions{Name: "win2", OS: OSWindows, RDP: true}); err != nil {
		t.Fatal(err)
	}
	g.resetPW = "SYNTHETIC-Pw-123"
	g.files = map[string]string{}
	ts := fakeTS{out: `{"Peer":{"k1":{"HostName":"pyre-win2","DNSName":"pyre-win2.synthetic.ts.net.","TailscaleIPs":["100.64.0.9","fd7a:115c:a1e0::9"],"Online":true}}}`}
	info, err := m.RDP(ctx, ts, "win2", "", true, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if info.Address != "100.64.0.9" || info.TailnetName != "pyre-win2.synthetic.ts.net" || info.Password != "SYNTHETIC-Pw-123" || info.User != DefaultUser {
		t.Fatalf("info: %+v", info)
	}
	if strings.Contains(info.RDPFile, "SYNTHETIC-Pw") || !strings.Contains(info.RDPFile, "full address:s:100.64.0.9") {
		t.Fatalf("rdp file: %q", info.RDPFile)
	}
	if g.files[MetaAutologonPassword] != "SYNTHETIC-Pw-123" {
		t.Fatal("auto-logon handoff missing")
	}
	rms := g.called("compute instances remove-metadata")
	if last := rms[len(rms)-1]; !contains(last, "--keys="+MetaAutologonPassword) {
		t.Fatalf("handoff key not removed: %v", last)
	}
	g.assertNoSecretsInArgv(t, "SYNTHETIC-Pw-123")
	// Linux nodes have no RDP.
	g.onCreate = func() { go h.connect("lin3") }
	if _, err := m.Create(ctx, CreateOptions{Name: "lin3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RDP(ctx, ts, "lin3", "", false, 0); !contracts.IsCode(err, contracts.ErrUnsupported) {
		t.Fatalf("rdp to linux: %v", err)
	}
}

func TestBakeUsesBuilderWithoutSecrets(t *testing.T) {
	m, g, h, _ := newManager(t)
	res, err := m.Bake(context.Background(), BakeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Family != DefaultBakedFamily || !strings.HasPrefix(res.Builder, "pyre-bake-") {
		t.Fatalf("bake: %+v", res)
	}
	if g.files[MetaEnrollSecret] != "" || g.files[MetaTSAuthKey] != "" || h.tickets != 0 {
		t.Fatal("bake must carry no secret and mint no ticket")
	}
	var bc BootConfig
	_ = json.Unmarshal([]byte(g.files[MetaConfig]), &bc)
	if bc.Mode != "bake" || bc.NodeID != "" {
		t.Fatalf("bake config: %+v", bc)
	}
	if len(g.called("compute images create")) != 1 || len(g.called("compute instances delete "+res.Builder)) != 1 {
		t.Fatal("image not created or builder not deleted")
	}
}

func TestBootScripts(t *testing.T) {
	if _, err := exec.LookPath("bash"); err == nil {
		dir := t.TempDir()
		p := filepath.Join(dir, "startup.sh")
		_ = os.WriteFile(p, []byte(StartupScript), 0o600)
		if out, err := exec.Command("bash", "-n", p).CombinedOutput(); err != nil {
			t.Fatalf("startup.sh syntax: %v\n%s", err, out)
		}
	}
	for name, s := range map[string]string{"startup.sh": StartupScript, "startup.ps1": WindowsStartupScript} {
		if strings.Contains(s, "set -x") || strings.Contains(s, "Set-PSDebug") {
			t.Errorf("%s enables tracing (would print secrets)", name)
		}
		for _, key := range []string{MetaEnrollSecret, MetaTSAuthKey} {
			if !strings.Contains(s, key) {
				t.Errorf("%s does not read %s", name, key)
			}
		}
	}
	// Secrets are read into files, never echoed or put on a command line.
	if !strings.Contains(StartupScript, `md_secret pyre-ts-authkey "$RUN/ts.key"`) || !strings.Contains(StartupScript, `--auth-key="file:$RUN/ts.key"`) ||
		!strings.Contains(StartupScript, `--secret-stdin`) || !strings.Contains(StartupScript, "ssh-keygen -Y verify") {
		t.Error("startup.sh secret/verification handling changed")
	}
	if !strings.Contains(WindowsStartupScript, "--auth-key=file:$keyFile") || !strings.Contains(WindowsStartupScript, "-Y verify") ||
		!strings.Contains(WindowsStartupScript, "100.64.0.0/10") || !strings.Contains(WindowsStartupScript, "AutoAdminLogon") {
		t.Error("startup.ps1 secret/verification/RDP handling changed")
	}
	// Windows: SYSTEM never runs install.ps1 (a per-user install); it stages
	// for the dev user and a one-shot logon task installs as that user.
	ps := WindowsStartupScript
	for _, bad := range []string{"-NoEnroll", "-User $User", "| & powershell.exe", "-Service none"} {
		if strings.Contains(ps, bad) {
			t.Errorf("startup.ps1 still contains %q (install.ps1 has no such mode; SYSTEM must not install Pyre)", bad)
		}
	}
	for _, want := range []string{
		// staging dir: dev user + SYSTEM only
		`$Stage = 'C:\ProgramData\pyre-install'`,
		`& icacls.exe $Stage /inheritance:r /grant:r "*$($userSid):(OI)(CI)F" '*S-1-5-18:(OI)(CI)F'`,
		// ticket: metadata -> SYSTEM-only file -> staged file, never argv
		`MdSecret 'pyre-enroll-secret' $tf`,
		`[IO.File]::WriteAllText($ticketFile, $ticket, $utf8)`,
		// one-shot logon task as the dev user, interactive, not elevated
		`$TaskName = 'pyre-first-install'`,
		`New-ScheduledTaskPrincipal -UserId $uid -LogonType Interactive -RunLevel Limited`,
		`New-ScheduledTaskTrigger -AtLogOn -User $uid`,
		`Register-ScheduledTask -TaskName $TaskName -Principal $pFirst -Trigger $tFirst`,
		`(A;;FA;;;$userSid)`,
		// the task: install.ps1 -EnrollTicketFile, ticket deleted in finally, log in the user's state dir, self-unregister
		`$a += @('-EnrollTicketFile', $ticket)`,
		"} finally {\n  # The ticket is single-use: gone once install.ps1 returned, success or failure.\n  Remove-Item -Force -LiteralPath $ticket",
		`$log = Join-Path $state 'first-install.log'`,
		`Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction Stop`,
		`'-Service', 'node', '-Restart'`,
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("startup.ps1 lacks %q", want)
		}
	}
	// Bake: no install, no task, no ticket.
	bake := ps[strings.Index(ps, "if ($Mode -eq 'bake') {"):]
	bake = bake[:strings.Index(bake, "exit 0")]
	for _, bad := range []string{"install.ps1", "Register-ScheduledTask", "enroll"} {
		if strings.Contains(bake, bad) {
			t.Errorf("startup.ps1 bake branch contains %q", bad)
		}
	}
	if pwsh, err := exec.LookPath("pwsh"); err == nil {
		dir := t.TempDir()
		p := filepath.Join(dir, "startup.ps1")
		_ = os.WriteFile(p, []byte(WindowsStartupScript), 0o600)
		cmd := exec.Command(pwsh, "-NoProfile", "-Command", "$e=$null; [void][System.Management.Automation.Language.Parser]::ParseFile($args[0], [ref]$null, [ref]$e); if ($e.Count) { $e | % { $_.ToString() }; exit 1 }", p)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("startup.ps1 parse: %v\n%s", err, out)
		}
	}
}
