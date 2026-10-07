package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jhh3/pyre/internal/autotitle"
	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/daemon/daemontest"
)

func titleSuggestions(n *node) []autotitle.Suggestion {
	var out []autotitle.Suggestion
	for _, r := range n.rep.Reports(contracts.ReportEvent) {
		var ev contracts.Event
		if json.Unmarshal(r.Payload, &ev) != nil || ev.Kind != autotitle.EventTitleSuggested {
			continue
		}
		var s autotitle.Suggestion
		_ = json.Unmarshal(ev.Payload, &s)
		out = append(out, s)
	}
	return out
}

// An automatically titled run reports a title from its first prompt after
// the first turn, then Claude's own title when it appears; an untagged
// (person-titled) session reports nothing.
func TestAutoTitleSuggestionsFromTranscript(t *testing.T) {
	w := daemontest.NewWorld()
	ad := &filedAdapter{Adapter: daemontest.New(w, contracts.HarnessClaude), files: map[string][]string{}}
	dir := shortDir(t)
	tr := filepath.Join(t.TempDir(), "synthetic-session.jsonl")
	writeLines(t, tr, userLine("rotate the synthetic webhook secret and redeploy"))
	ad.files["run_t1"] = []string{tr}
	ad.files["run_t2"] = []string{tr}
	n := startNode(t, dir, Config{Adapters: []contracts.RuntimeAdapter{ad}})
	sc := startCmd("ses_t1", "run_t1", contracts.HarnessClaude, t.TempDir())
	sc.Session.Tags = []string{autotitle.TagAuto}
	n.start(sc)
	waitFor(t, "first-prompt title", func() bool { return len(titleSuggestions(n)) == 1 })
	if s := titleSuggestions(n)[0]; s.Title != "Rotate the synthetic webhook secret" || s.Source != autotitle.SourcePrompt || s.RunID != "run_t1" {
		t.Fatalf("suggestion %+v", s)
	}

	// Claude writes its own title after a turn: the next idle edge reports it.
	writeLines(t, tr, `{"type":"ai-title","aiTitle":"Synthetic webhook secret rotation","sessionId":"synthetic"}`)
	w.Mutate("run_t1", func(p *daemontest.Proc) { p.State = contracts.RunRunning })
	n.d.PollNow(context.Background(), "run_t1")
	w.Mutate("run_t1", func(p *daemontest.Proc) { p.State = contracts.RunWaitingInput })
	n.d.PollNow(context.Background(), "run_t1")
	waitFor(t, "native title", func() bool { return len(titleSuggestions(n)) == 2 })
	if s := titleSuggestions(n)[1]; s.Title != "Synthetic webhook secret rotation" || s.Source != autotitle.SourceNative {
		t.Fatalf("suggestion %+v", s)
	}

	// The node suggests for every non-shell run, including ones launched
	// without title:auto: tags can change on the hub after launch, so the hub
	// alone applies or drops a suggestion (TestAutoTitlePrecedence).

}
