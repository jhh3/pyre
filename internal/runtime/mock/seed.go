package mock

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	c "github.com/jhh3/pyre/internal/contracts"
)

// DefaultFixtureDirs returns tests/fixtures/contracts and tests/fixtures/mock
// of the source tree this package was built from, when they exist (tests,
// `go run`, and binaries built in a checkout). Otherwise nil.
func DefaultFixtureDirs() []string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return nil
	}
	root := filepath.Join(filepath.Dir(file), "..", "..", "..")
	var out []string
	for _, d := range []string{"contracts", "mock"} {
		p := filepath.Join(root, "tests", "fixtures", d)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// seed merges one bundle; records with an existing ID are replaced.
func (b *Backend) seed(f *c.FixtureBundle) {
	for _, n := range f.Nodes {
		n := clone(n)
		b.nodes[n.NodeID] = &n
	}
	for _, sv := range f.Sessions {
		sv := clone(sv)
		s := sv.Session
		b.sessions[s.SessionID] = &s
		if sv.Run != nil {
			r := *sv.Run
			b.runs[r.RunID] = &r
		}
	}
	for _, r := range f.Runs {
		r := clone(r)
		b.runs[r.RunID] = &r
	}
	for _, m := range f.Messages {
		m := clone(m)
		b.messages[m.Envelope.MessageID] = &m
	}
	for _, a := range f.Approvals {
		a := clone(a)
		b.approvals[a.ApprovalID] = &a
	}
	for _, l := range f.Leases {
		l := clone(l)
		b.leases[l.LeaseID] = &l
	}
	for _, p := range f.Projects {
		p := clone(p)
		b.projects[p.ProjectID] = &p
	}
	for _, d := range f.Documents {
		d := clone(d)
		b.documents[d.DocumentID] = &d
	}
	for _, r := range f.Revisions {
		r := clone(r)
		r.Size = int64(len(r.Content))
		r.Digest = sha256Digest([]byte(r.Content))
		b.revisions[r.RevisionID] = &r
	}
	for _, p := range f.Proposals {
		p := clone(p)
		b.proposals[p.ProposalID] = &p
	}
	for _, a := range f.Artifacts {
		a := clone(a)
		b.artifacts[a.ArtifactID] = &a
	}
	for id, content := range f.Contents {
		b.contents[id] = []byte(content)
	}
	for _, v := range f.Versions {
		v := clone(v)
		b.versions[v.VersionID] = &v
	}
	for _, cm := range f.Comments {
		cm := clone(cm)
		b.comments[cm.CommentID] = &cm
	}
	for _, n := range f.Notes {
		n := clone(n)
		b.notes[n.NoteID] = &n
	}
	for _, a := range f.Accounts {
		a := clone(a)
		b.accounts[a.Account.AccountID] = &a
	}
	for _, o := range f.Observations {
		if av, ok := b.accounts[o.AccountID]; ok {
			o := clone(o)
			if av.Latest == nil || !o.ObservedAt.Before(av.Latest.ObservedAt) {
				av.Latest = &o
			}
		}
	}
	b.fallbacks = append(b.fallbacks, clone(f.Fallbacks)...)
	for _, a := range f.Attention {
		a := clone(a)
		b.attention[a.AttentionID] = &a
	}
	b.events = append(b.events, clone(f.Events)...)
	for _, e := range f.Environments {
		e := clone(e)
		b.envs[e.NodeID] = append(b.envs[e.NodeID], e)
	}
	if len(f.Tools) > 0 {
		// Fixture tools are not node-scoped; attach them to every node of the
		// bundle that has an environment, else to the first node seen.
		for _, e := range f.Environments {
			b.tools[e.NodeID] = clone(f.Tools)
		}
		if len(f.Environments) == 0 {
			b.tools["*"] = clone(f.Tools)
		}
	}
	b.drift = append(b.drift, clone(f.Drift)...)
	for _, p := range f.Plans {
		b.plans[p.PlanID] = clone(p)
	}
	for _, cn := range f.Connections {
		cn := clone(cn)
		b.connections[cn.ConnectionID] = &cn
	}
}

// finishSeed normalizes merged fixture data into a consistent live state.
func (b *Backend) finishSeed() {
	// Versions: recompute digest/size from inline content so downloads are
	// verifiable; versions without content get synthetic placeholder bytes.
	for id, v := range b.versions {
		data, ok := b.contents[id]
		if !ok {
			data = []byte("synthetic mock content for " + id + "\n")
			b.contents[id] = data
		}
		v.Size = int64(len(data))
		v.Digest = sha256Digest(data)
		if v.ContentURL == "" {
			v.ContentURL = "/v1/content/" + id
		}
	}
	// Events: renumber cursors from 1 in original order (hub-assigned values
	// are local to each fixture).
	sort.SliceStable(b.events, func(i, j int) bool { return b.events[i].Cursor < b.events[j].Cursor })
	seen := map[string]bool{}
	evs := b.events[:0]
	for _, e := range b.events {
		if seen[e.EventID] {
			continue
		}
		seen[e.EventID] = true
		b.eventHead++
		e.Cursor = b.eventHead
		evs = append(evs, e)
	}
	b.events = evs
	// Messages: unique, monotonic inbox cursors in fixture order.
	ids := sortedKeys(b.messages)
	sort.SliceStable(ids, func(i, j int) bool { return b.messages[ids[i]].Cursor < b.messages[ids[j]].Cursor })
	for _, id := range ids {
		b.msgSeq++
		b.messages[id].Cursor = b.msgSeq
	}
	// Leases: a run's embedded lease is the fixture's initial state and wins;
	// a run without one picks up its most recent listed lease.
	for _, r := range b.runs {
		if r.Lease != nil {
			l := *r.Lease
			b.leases[l.LeaseID] = &l
		}
	}
	embedded := map[string]bool{}
	for _, r := range b.runs {
		embedded[r.RunID] = r.Lease != nil
	}
	for _, id := range sortedKeys(b.leases) {
		l := b.leases[id]
		r, ok := b.runs[l.RunID]
		if !ok || embedded[r.RunID] {
			continue
		}
		if r.Lease == nil || l.AcquiredAt.After(r.Lease.AcquiredAt) {
			lc := *l
			r.Lease = &lc
		}
	}
	for _, r := range b.runs {
		// Label every observation as mock so it can never pass for runtime evidence.
		if !strings.HasPrefix(r.Freshness.Source, "mock") {
			r.Freshness.Source = "mock:" + r.Freshness.Source
		}
		if _, ok := b.transcripts[r.RunID]; !ok {
			b.transcripts[r.RunID] = b.syntheticTranscript(r)
		}
	}
	// Default owner clients, so client lists are not empty.
	for _, ci := range []c.ClientInfo{
		{ClientID: "cli_synth-laptop", Name: "owner laptop (synthetic)", Kind: "cli", CreatedAt: FixtureEpoch.Add(-240 * time.Hour)},
		{ClientID: "cli_synth-android", Name: "owner phone (synthetic)", Kind: "android", CreatedAt: FixtureEpoch.Add(-200 * time.Hour)},
	} {
		if _, ok := b.clients[ci.ClientID]; !ok {
			ci := ci
			b.clients[ci.ClientID] = &ci
		}
	}
}

func (b *Backend) syntheticTranscript(r *c.Run) []c.TranscriptEntry {
	title := r.SessionID
	if s, ok := b.sessions[r.SessionID]; ok && s.Title != "" {
		title = s.Title
	}
	at := r.StartedAt
	return []c.TranscriptEntry{
		{Seq: 1, RunID: r.RunID, At: at, Kind: "system", Text: "[mock] synthetic transcript; not a real runtime"},
		{Seq: 2, RunID: r.RunID, At: at, Kind: "user", Text: "Synthetic task: " + title},
		{Seq: 3, RunID: r.RunID, At: at, Kind: "assistant", Text: "Working on it (synthetic)."},
	}
}

func sha256Digest(b []byte) c.Digest {
	s := sha256.Sum256(b)
	return c.Digest("sha256:" + hex.EncodeToString(s[:]))
}
