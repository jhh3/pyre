package hive

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Options tune a Client. Zero values select the defaults.
type Options struct {
	// StatusTTL caches Detect results (default 30s).
	StatusTTL time.Duration
	// StagingRoot holds one empty directory per project from which bridge
	// notes are written, so Hive scopes them to a stable repo_key
	// "dir:<project_id>@<sha8>" (default ~/.local/state/pyre/hive-notes).
	StagingRoot string
	// ProjectRepoKeys optionally maps a Pyre project to additional Hive
	// repo_keys (e.g. "owner/repo" of its bound repositories) used to rank
	// project material first.
	ProjectRepoKeys func(projectID string) []string
	SnippetBytes    int // per-hit snippet cap (default 300)
	GetBytes        int // memory.get body cap (default 8 KiB)
	ResultBytes     int // whole search response cap (default 64 KiB)
	// BackgroundSlots bounds concurrent background Hive subprocesses
	// (transcript capture and the note bridge together; default 1).
	// Interactive calls (search, get, launch context) never take a slot,
	// so they never wait behind background work.
	BackgroundSlots int
}

// bgKey marks a context whose Hive subprocesses are background work.
type bgKey struct{}

// Background returns ctx marked as background work: Hive subprocesses run
// under it wait for one of the client's BackgroundSlots and run at lowered
// CPU priority (BackgroundNice). The Capturer and the Bridge's Run loop
// mark their contexts; interactive callers do not.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, bgKey{}, true)
}

// IsBackground reports whether ctx was marked by Background.
func IsBackground(ctx context.Context) bool {
	v, _ := ctx.Value(bgKey{}).(bool)
	return v
}

// BackgroundStats describes the client's background subprocess lane.
type BackgroundStats struct {
	Slots       int `json:"slots"`
	Running     int `json:"running"`
	Waiting     int `json:"waiting"`
	MaxRunning  int `json:"max_running"`
	Completed   int `json:"completed"`
	TotalWaited time.Duration
}

// Client is the node-local Hive adapter. Safe for concurrent use.
type Client struct {
	env  Env
	opts Options

	mu       sync.Mutex
	status   Status
	statusAt time.Time
	pending  func() int
	natives  NativeResolver

	bgSem   chan struct{}
	bgMu    sync.Mutex
	bgStats BackgroundStats
}

// acquireBackground waits for a background slot (or ctx) and returns its
// release function.
func (c *Client) acquireBackground(ctx context.Context) (func(), error) {
	start := time.Now()
	c.bgMu.Lock()
	c.bgStats.Waiting++
	c.bgMu.Unlock()
	var err error
	select {
	case c.bgSem <- struct{}{}:
	case <-ctx.Done():
		err = ctx.Err()
	}
	c.bgMu.Lock()
	defer c.bgMu.Unlock()
	c.bgStats.Waiting--
	if err != nil {
		return nil, err
	}
	c.bgStats.TotalWaited += time.Since(start)
	c.bgStats.Running++
	c.bgStats.MaxRunning = max(c.bgStats.MaxRunning, c.bgStats.Running)
	return func() {
		c.bgMu.Lock()
		c.bgStats.Running--
		c.bgStats.Completed++
		c.bgMu.Unlock()
		<-c.bgSem
	}, nil
}

// BackgroundStats returns a snapshot of the background lane.
func (c *Client) BackgroundStats() BackgroundStats {
	c.bgMu.Lock()
	defer c.bgMu.Unlock()
	s := c.bgStats
	s.Slots = cap(c.bgSem)
	return s
}

// runCmd runs one subprocess; background contexts take a slot and mark the
// command for lowered priority.
func (c *Client) runCmd(ctx context.Context, cmd Cmd) (Result, error) {
	if IsBackground(ctx) {
		release, err := c.acquireBackground(ctx)
		if err != nil {
			return Result{}, err
		}
		defer release()
		cmd.Background = true
	}
	return c.env.runner().Run(ctx, cmd)
}

// NativeSession is the Pyre session and run that own a harness-native
// session (a Claude session UUID or Codex thread ID) on this node.
type NativeSession struct {
	SessionID string
	RunID     string
	ProjectID string
}

// NativeResolver maps a harness-native session ID to the Pyre session that
// ran it on this node (ok=false for sessions Pyre did not launch).
type NativeResolver func(ctx context.Context, nativeID string) (NativeSession, bool)

// SetNativeResolver lets the daemon attribute Hive-captured transcripts to
// the Pyre session that produced them: the hit's SessionRef becomes the Pyre
// session ID (as for hub notes), its provenance names the Pyre session and
// run, and the session's project scopes the hit.
func (c *Client) SetNativeResolver(f NativeResolver) {
	c.mu.Lock()
	c.natives = f
	c.mu.Unlock()
}

// attribute applies the native resolver to a transcript hit captured from a
// Pyre-launched harness. Hits of sessions Pyre did not launch keep Hive's
// native session reference.
func (c *Client) attribute(ctx context.Context, h *contracts.MemoryHit, localMachine string) {
	c.mu.Lock()
	f := c.natives
	c.mu.Unlock()
	if f == nil || h.Kind != "transcript" || h.SessionRef == "" {
		return
	}
	ns, ok := f(ctx, h.SessionRef)
	if !ok || ns.SessionID == "" {
		return
	}
	h.SessionRef = ns.SessionID
	if localMachine != "" && !strings.Contains(h.Source, " machine=") {
		// The run was launched on this node, so its transcript was captured
		// here (e.g. Pyre's own capture of a Codex rollout, which leaves no
		// Hive session-ledger row to name the machine).
		h.Source = strings.Replace(h.Source, "hive", "hive machine="+localMachine, 1)
	}
	h.Source += " pyre_session=" + ns.SessionID
	if ns.RunID != "" {
		h.Source += " pyre_run=" + ns.RunID
	}
	if h.ProjectID == "" && ns.ProjectID != "" {
		h.ProjectID = ns.ProjectID
	}
}

// New returns a Client for env.
func New(env Env, opts Options) *Client {
	if opts.StatusTTL <= 0 {
		opts.StatusTTL = 30 * time.Second
	}
	if opts.StagingRoot == "" {
		opts.StagingRoot = filepath.Join(env.Home(), ".local", "state", "pyre", "hive-notes")
	}
	if opts.SnippetBytes <= 0 {
		opts.SnippetBytes = 300
	}
	if opts.GetBytes <= 0 {
		opts.GetBytes = 8 << 10
	}
	if opts.ResultBytes <= 0 {
		opts.ResultBytes = 64 << 10
	}
	if opts.BackgroundSlots <= 0 {
		opts.BackgroundSlots = 1
	}
	return &Client{env: env, opts: opts, bgSem: make(chan struct{}, opts.BackgroundSlots)}
}

// SetPendingFunc lets the daemon add the bridge's backlog to
// MemoryStatus.PendingItems.
func (c *Client) SetPendingFunc(f func() int) {
	c.mu.Lock()
	c.pending = f
	c.mu.Unlock()
}

// Status returns a cached Detect result (refreshed after StatusTTL).
func (c *Client) Status(ctx context.Context) Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.env.clock().Now()
	if c.statusAt.IsZero() || now.Sub(c.statusAt) >= c.opts.StatusTTL {
		c.status = Detect(ctx, c.env)
		c.statusAt = now
	}
	st := c.status
	if c.pending != nil {
		st.Memory.PendingItems += c.pending()
	}
	// The index write time is re-read on every call (two stats) so search
	// freshness and memory.status reflect the newest write, e.g. a note the
	// bridge just published, rather than the cached detection's.
	if st.Mode != ModeUnavailable {
		if db := c.env.DBPath(); db != "" { // live: a fresh store may appear after detection
			if t := latestMtime(db, db+"-wal"); t != nil {
				st.Memory.LastIndexAt = t
			}
		}
	}
	return st
}

// Refresh forces a new Detect.
func (c *Client) Refresh(ctx context.Context) Status {
	c.mu.Lock()
	c.statusAt = time.Time{}
	c.mu.Unlock()
	return c.Status(ctx)
}

func (c *Client) freshness(st Status, src, extraReason string, forceStale bool) contracts.Freshness {
	f := contracts.Freshness{ObservedAt: st.CheckedAt, Source: src, Stale: st.Stale || forceStale}
	if st.Memory.LastIndexAt != nil {
		f.ObservedAt = *st.Memory.LastIndexAt
	}
	var reasons []string
	if extraReason != "" {
		reasons = append(reasons, extraReason)
	}
	if st.StaleReason != "" {
		reasons = append(reasons, st.StaleReason)
	}
	f.Reason = strings.Join(reasons, "; ")
	return f
}

func unavailable(st Status) error {
	return contracts.Errorf(contracts.ErrUnavailable, "hive memory unavailable on this node: %s", st.Memory.Detail).
		WithDetail("backend", st.Memory.Backend)
}

// Search runs Hive's ranked retrieval and maps the hits.
func (c *Client) Search(ctx context.Context, p contracts.MemorySearchParams) (contracts.MemorySearchResult, error) {
	q := strings.TrimSpace(p.Query)
	if q == "" {
		return contracts.MemorySearchResult{}, contracts.Errorf(contracts.ErrInvalid, "memory.search: query is required")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	st := c.Status(ctx)
	var (
		entries []entry
		err     error
		fresh   contracts.Freshness
	)
	switch st.Mode {
	case ModeCLI:
		entries, err = c.cliSearch(ctx, q, limit+1)
		fresh = c.freshness(st, "hive:cli", "", false)
		if err != nil {
			cliErr := err
			entries, err = indexSearch(ctx, st.DBPath, q, "", "", limit+1)
			if err != nil {
				return contracts.MemorySearchResult{}, contracts.Errorf(contracts.ErrUnavailable,
					"hive search failed (%v) and the local index is unreadable (%v)", cliErr, err)
			}
			fresh = c.freshness(st, "hive:index-ro", "hive CLI failed ("+cliErr.Error()+"); served from the read-only local index (lexical ranking only)", true)
		}
	case ModeIndex:
		entries, err = indexSearch(ctx, st.DBPath, q, "", "", limit+1)
		if err != nil {
			return contracts.MemorySearchResult{}, contracts.Errorf(contracts.ErrUnavailable, "hive local index search failed: %v", err)
		}
		fresh = c.freshness(st, "hive:index-ro", "hive CLI unavailable; served from the read-only local index (lexical ranking only)", true)
	default:
		return contracts.MemorySearchResult{}, unavailable(st)
	}

	res := contracts.MemorySearchResult{Hits: []contracts.MemoryHit{}, Freshness: fresh}
	if len(entries) > limit {
		entries = entries[:limit]
		res.Truncated = true
	}
	prov := c.provenance(ctx, st, entries)
	keys := c.projectKeys(p.ProjectID)
	type ranked struct {
		hit  contracts.MemoryHit
		inPj bool
	}
	rs := make([]ranked, 0, len(entries))
	for _, e := range entries {
		var pv *sessionProv
		if v, ok := prov[e.UID]; ok {
			pv = &v
		}
		h := toHit(e, pv, st.Machine, c.opts.SnippetBytes)
		c.attribute(ctx, &h, st.Machine)
		inPj := false
		if p.ProjectID != "" && (h.ProjectID == p.ProjectID || keys[e.RepoKey]) {
			inPj = true
			h.ProjectID = p.ProjectID
		}
		rs = append(rs, ranked{h, inPj})
	}
	// Project material first; Hive's rank order otherwise preserved. Other
	// hits stay visible: projects are optional and scoping is a preference.
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].inPj && !rs[j].inPj })
	total := 0
	for _, r := range rs {
		b, _ := json.Marshal(r.hit)
		if total+len(b) > c.opts.ResultBytes {
			res.Truncated = true
			break
		}
		total += len(b)
		res.Hits = append(res.Hits, r.hit)
	}
	return res, nil
}

// Get returns one memory item with a bounded body.
func (c *Client) Get(ctx context.Context, id string) (contracts.MemoryHit, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.HasPrefix(id, "-") || len(id) > 200 || strings.ContainsAny(id, "\x00\n") {
		return contracts.MemoryHit{}, contracts.Errorf(contracts.ErrInvalid, "memory.get: invalid id")
	}
	st := c.Status(ctx)
	var (
		e     entry
		found bool
		err   error
	)
	switch st.Mode {
	case ModeCLI:
		e, found, err = c.cliShow(ctx, id)
		if err != nil {
			e, found, err = indexGet(ctx, st.DBPath, id)
		}
	case ModeIndex:
		e, found, err = indexGet(ctx, st.DBPath, id)
	default:
		return contracts.MemoryHit{}, unavailable(st)
	}
	if err != nil {
		return contracts.MemoryHit{}, contracts.Errorf(contracts.ErrUnavailable, "hive get failed: %v", err)
	}
	if !found {
		return contracts.MemoryHit{}, contracts.Errorf(contracts.ErrNotFound, "no hive memory %q", id)
	}
	prov := c.provenance(ctx, st, []entry{e})
	var pv *sessionProv
	if v, ok := prov[e.UID]; ok {
		pv = &v
	}
	h := toHit(e, pv, st.Machine, c.opts.SnippetBytes)
	c.attribute(ctx, &h, st.Machine)
	h.Snippet = detailText(e, c.opts.GetBytes)
	return h, nil
}

func (c *Client) provenance(ctx context.Context, st Status, entries []entry) map[string]sessionProv {
	var ids []string
	for _, e := range entries {
		if e.Origin == "live" && e.UID != "" {
			ids = append(ids, e.UID)
		}
	}
	if len(ids) == 0 || !fileExists(st.DBPath) {
		return nil
	}
	return lookupSessions(ctx, st.DBPath, ids)
}

func (c *Client) projectKeys(projectID string) map[string]bool {
	out := map[string]bool{}
	if projectID == "" {
		return out
	}
	out[c.ProjectRepoKey(projectID)] = true
	if c.opts.ProjectRepoKeys != nil {
		for _, k := range c.opts.ProjectRepoKeys(projectID) {
			out[k] = true
		}
	}
	return out
}

// run invokes the discovered launcher.
func (c *Client) run(ctx context.Context, timeout time.Duration, dir string, args ...string) (Result, error) {
	// Callers have just consulted Status; reuse its launcher rather than
	// re-detecting for every subprocess.
	c.mu.Lock()
	bin := c.status.Bin
	c.mu.Unlock()
	if bin == "" {
		bin = c.Status(ctx).Bin
	}
	if bin == "" {
		return Result{}, errors.New("hive CLI not found")
	}
	return c.runCmd(ctx, Cmd{Path: bin, Args: args, Env: c.env.childEnv(), Dir: dir,
		Timeout: timeout, MaxOutput: c.env.maxOutput()})
}

// cliSearch: `hive search --json --all-repos --limit N -- <query>`. hive-mem
// exits 1 with "no results" on stderr for an empty result.
func (c *Client) cliSearch(ctx context.Context, q string, limit int) ([]entry, error) {
	res, err := c.run(ctx, c.env.queryTimeout(), "", "search", "--json", "--all-repos", "--limit", strconv.Itoa(limit), "--", q)
	if err != nil {
		return nil, err
	}
	return parseListOutput(res)
}

func parseListOutput(res Result) ([]entry, error) {
	switch {
	case res.ExitCode == 1 && len(bytes.TrimSpace(res.Stdout)) == 0:
		return nil, nil
	case res.ExitCode != 0:
		return nil, fmt.Errorf("hive exited %d: %s", res.ExitCode, oneLine(res.Stderr))
	}
	var out []entry
	sc := bufio.NewScanner(bytes.NewReader(res.Stdout))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e entry
		if json.Unmarshal(line, &e) == nil && e.UID != "" {
			out = append(out, e)
		}
	}
	return out, nil
}

func (c *Client) cliShow(ctx context.Context, id string) (entry, bool, error) {
	res, err := c.run(ctx, c.env.queryTimeout(), "", "show", "--json", id)
	if err != nil {
		return entry{}, false, err
	}
	if res.ExitCode == 1 {
		return entry{}, false, nil
	}
	if res.ExitCode != 0 {
		return entry{}, false, fmt.Errorf("hive show exited %d: %s", res.ExitCode, oneLine(res.Stderr))
	}
	var e entry
	if err := json.Unmarshal(bytes.TrimSpace(res.Stdout), &e); err != nil || e.UID == "" {
		return entry{}, false, fmt.Errorf("hive show: unparseable output")
	}
	return e, true, nil
}

// ProjectRepoKey is the Hive repo_key that bridge notes for projectID carry:
// hive-mem's key for a non-git directory, "dir:<basename>@<sha256(realpath)[:8]>".
// An empty projectID maps to the shared "standalone" directory.
func (c *Client) ProjectRepoKey(projectID string) string {
	dir := c.stagingDir(projectID)
	anchor := dir
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		anchor = r
	}
	sum := sha256.Sum256([]byte(anchor))
	return "dir:" + filepath.Base(anchor) + "@" + hex.EncodeToString(sum[:])[:8]
}

func (c *Client) stagingDir(projectID string) string {
	name := "standalone"
	if projectID != "" && contracts.ValidID(projectID, contracts.PrefixProject) {
		name = projectID
	}
	return filepath.Join(c.opts.StagingRoot, name)
}

// ErrWrittenUnverified means `hive note` succeeded but the marker was not
// found afterwards. The bridge records that the write happened so it never
// writes the same note twice.
var ErrWrittenUnverified = errors.New("hive note written but not yet found by marker")

// maxNoteBytes keeps the whole text inside hive-mem's outcome cap (4000
// bytes) so the marker and footer are never cut by Hive.
const maxNoteBytes = 3800

// NoteText renders a hub note as the text handed to `hive note`.
func NoteText(n contracts.Note) string {
	_, mk := noteKindFor(n.Kind)
	m := marker{NoteID: n.NoteID, Kind: mk, Promoted: n.PromotedTo != ""}
	if n.ProjectID != nil && contracts.ValidID(*n.ProjectID, contracts.PrefixProject) {
		m.ProjectID = *n.ProjectID
	}
	head := formatMarker(m) + " " + truncBytes(flatten(n.Title), 140)
	foot := fmt.Sprintf("— pyre %s %s", mk, n.NoteID)
	if m.ProjectID != "" {
		foot += " · project " + m.ProjectID
	}
	if n.SessionID != "" {
		foot += " · session " + n.SessionID
	}
	if !n.CreatedAt.IsZero() {
		foot += " · created " + n.CreatedAt.UTC().Format(time.RFC3339)
	}
	if n.Provenance.NodeID != "" {
		foot += " · node " + n.Provenance.NodeID
	}
	if n.PromotedTo != "" {
		foot += " · promoted to " + n.PromotedTo
	}
	body := strings.TrimSpace(n.Body)
	room := maxNoteBytes - len(head) - len(foot) - 4
	if len(body) > room {
		body = truncBytes(body, room-60) + " [truncated; full text in the Pyre hub note]"
	}
	if body == "" {
		return head + "\n\n" + foot
	}
	return head + "\n\n" + body + "\n\n" + foot
}

// PublishNote writes a hub note through `hive note` and returns the Hive uid.
// It does not check for an existing copy; the Bridge calls FindNote first.
func (c *Client) PublishNote(ctx context.Context, n contracts.Note) (string, error) {
	if !contracts.ValidID(n.NoteID, contracts.PrefixNote) {
		return "", contracts.Errorf(contracts.ErrInvalid, "hive bridge: invalid note id %q", n.NoteID)
	}
	st := c.Status(ctx)
	if st.Mode != ModeCLI {
		return "", unavailable(st)
	}
	var pid string
	if n.ProjectID != nil {
		pid = *n.ProjectID
	}
	dir := c.stagingDir(pid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("hive bridge staging dir: %w", err)
	}
	hk, _ := noteKindFor(n.Kind)
	res, err := c.run(ctx, c.env.writeTimeout(), dir, "note", "--kind", hk, "--", NoteText(n))
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("hive note exited %d: %s", res.ExitCode, oneLine(res.Stderr))
	}
	ref, found, err := c.FindNote(ctx, n.NoteID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrWrittenUnverified, err)
	}
	if !found {
		return "", ErrWrittenUnverified
	}
	return ref, nil
}

// FindNote looks a hub note up in Hive by its marker; the earliest matching
// entry wins so the answer is stable even if a duplicate ever exists.
func (c *Client) FindNote(ctx context.Context, noteID string) (string, bool, error) {
	st := c.Status(ctx)
	var (
		entries []entry
		err     error
	)
	switch st.Mode {
	case ModeCLI:
		entries, err = c.cliSearch(ctx, noteID, 20)
	case ModeIndex:
		entries, err = indexSearch(ctx, st.DBPath, noteID, "", "", 20)
	default:
		return "", false, unavailable(st)
	}
	if err != nil {
		return "", false, err
	}
	best := entry{}
	for _, e := range entries {
		if m, ok := parseMarker(e.Title); ok && m.NoteID == noteID {
			if best.UID == "" || e.TS < best.TS || (e.TS == best.TS && e.UID < best.UID) {
				best = e
			}
		}
	}
	return best.UID, best.UID != "", nil
}

// Available reports whether notes can be published now.
func (c *Client) Available(ctx context.Context) (bool, string) {
	st := c.Status(ctx)
	return st.Mode == ModeCLI, st.Memory.Detail
}

// Present reports Hive installed on this node, even if it cannot be written
// right now (degraded index-only mode).
func (c *Client) Present(ctx context.Context) bool {
	return c.Status(ctx).Mode != ModeUnavailable
}
