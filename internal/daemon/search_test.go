package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

// filedAdapter is the fake adapter plus native transcript files.
type filedAdapter struct {
	*daemontest.Adapter
	mu    sync.Mutex
	files map[string][]string // run ID -> paths
	calls int
}

func (f *filedAdapter) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return "claude", f.files[h.RunID]
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
}

func userLine(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "timestamp": "2026-10-05T09:00:00Z", "message": map[string]any{"role": "user", "content": text}})
	return string(b)
}

func TestTranscriptSearchActionIndexesRunsIncrementally(t *testing.T) {
	w := daemontest.NewWorld()
	ad := &filedAdapter{Adapter: daemontest.New(w, contracts.HarnessClaude), files: map[string][]string{}}
	dir := shortDir(t)
	tr := filepath.Join(t.TempDir(), "synthetic-session.jsonl")
	writeLines(t, tr, userLine("SYNTHETIC: rotate the stripe webhook secret"))
	ad.files["run_s1"] = []string{tr}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ad}})
	n.start(startCmd("ses_s1", "run_s1", contracts.HarnessClaude, t.TempDir()))

	search := func(q string) contracts.TranscriptSearchResult {
		t.Helper()
		params, _ := json.Marshal(contracts.TranscriptSearchParams{Query: q, BudgetMS: 3000})
		res := n.do(command(contracts.CmdAction, contracts.NodeActionRequest{NodeID: "node_test", Action: contracts.ActionTranscriptSearch, Params: params}))
		if res.Error != nil || res.Outcome != contracts.OutcomeSucceeded || res.Interim {
			t.Fatalf("action result %+v", res)
		}
		var out contracts.TranscriptSearchResult
		if err := json.Unmarshal(res.Result, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	r := search("stripe webhook")
	if len(r.Hits) != 1 || r.Hits[0].SessionID != "ses_s1" || r.Hits[0].RunID != "run_s1" || r.Hits[0].Seq != 1 || r.Hits[0].NodeID != "node_test" {
		t.Fatalf("hits %+v", r)
	}
	if !strings.Contains(r.Hits[0].Snippet, contracts.SearchMarkOpen+"stripe"+contracts.SearchMarkClose) || r.Hits[0].Title != "test ses_s1" {
		t.Fatalf("snippet/title %+v", r.Hits[0])
	}
	// A new turn is found by the next query (catch-up inside the budget).
	writeLines(t, tr, userLine("now deploy the lighthouse service"))
	if r := search("lighthouse"); len(r.Hits) != 1 || r.Hits[0].Seq != 2 {
		t.Fatalf("incremental %+v", r)
	}
	// Indexed state lives in the node database and survives a restart.
	n = n.restart()
	if r := search("stripe"); len(r.Hits) != 1 {
		t.Fatalf("after restart %+v", r)
	}
	if !contains(n.d.Actions(), contracts.ActionTranscriptSearch) {
		t.Fatal("transcript.search not registered")
	}
}

func TestTurnSettledKicksIndexer(t *testing.T) {
	d := &Daemon{search: &searchState{kick: make(chan struct{}, 1)}}
	d.searchTurnSettled(contracts.RunRunning, contracts.RunRunning)
	d.searchTurnSettled(contracts.RunWaitingInput, contracts.RunRunning)
	select {
	case <-d.search.kick:
		t.Fatal("kicked without a settled turn")
	default:
	}
	d.searchTurnSettled(contracts.RunRunning, contracts.RunWaitingInput)
	d.searchTurnSettled(contracts.RunRunning, contracts.RunExited) // coalesced, never blocks
	select {
	case <-d.search.kick:
	default:
		t.Fatal("turn end did not kick the indexer")
	}
}

func TestSearchOfflineAnswersFromThisNode(t *testing.T) {
	w := daemontest.NewWorld()
	ad := &filedAdapter{Adapter: daemontest.New(w, contracts.HarnessClaude), files: map[string][]string{}}
	tr := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, tr, userLine("SYNTHETIC: offline lookup of the zeppelin config"))
	ad.files["run_o1"] = []string{tr}
	n := startNode(t, shortDir(t), Config{Adapters: []contracts.RuntimeAdapter{ad}}) // no hub configured
	n.start(startCmd("ses_o1", "run_o1", contracts.HarnessClaude, t.TempDir()))
	ctx := contracts.WithPrincipal(context.Background(), contracts.Principal{Kind: contracts.PrincipalOwner, ID: "local"})
	resp, err := n.d.LocalAPI().Search(ctx, contracts.SearchRequest{Query: "zeppelin"})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Hits) != 1 || resp.Hits[0].SessionID != "ses_o1" || len(resp.Nodes) != 1 || !strings.Contains(resp.Nodes[0].Detail, "hub unreachable") {
		t.Fatalf("offline search %+v", resp)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
