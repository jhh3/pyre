package askdetect

import (
	"strings"
	"testing"
)

// Endings paraphrased from live leads on 2026-10-14 (synthetic names).
func TestOwnerQuestion(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
		q    string // expected returned line prefix (when want)
	}{
		{"decisions waiting", "Draft is in reports/q4.md.\n\nThe report is still waiting on your 3 decisions: the target customer, the GPU credit rate, and whether to start supplier talks.", true, "The report is still waiting on your 3 decisions"},
		{"should I", "Mixer stems are rendered.\n\nStill waiting on you: should I build the voice-match score in the DAW?", true, "Still waiting on you"},
		{"numbered questions", "Two things need a yes from you:\n\n1. Licence: may the AI filter worker accept NVIDIA's free model licence on your Hugging Face account?\n2. Spend: we are at $112 of paid generation. Should the lead stop new paid generation once it passes $150?", true, "Licence: may the AI"},
		{"numbered questions only", "Status: filter worker idle.\n\n1. Licence: may the AI filter worker accept NVIDIA's free model licence on your Hugging Face account?\n2. Spend: Should the lead stop new paid generation once it passes $150?", true, "Licence: may the AI filter worker accept"},
		{"bullet approvals", "Prototype build 7 runs end to end.\n\nThe prototype is still waiting on you:\n- Approve about $1.3k.\n- Book a sound editor for about 3 hours.", true, "The prototype is still waiting on you:"},
		{"asked earlier", "Supplier list refreshed (14 vendors).\n\nI asked earlier whether to send Ravi a short note … That's still open.", true, "I asked earlier whether to send Ravi"},
		{"want me to", "Tests pass on the branch.\n\nWant me to open the PR now?", true, "Want me to open the PR now?"},
		{"your call", "Both layouts work. **Your call** on which one ships.", true, "Both layouts work."},
		{"question then default", "Should I switch the nightly job to the cheaper region? (default: yes, tomorrow 9 AM)", true, "Should I switch"},

		{"nothing needs you", "Nothing needs you. The dashboard found no new problems and the nightly sync finished at 03:10.", false, ""},
		{"nothing else waiting", "All three PRs merged. There is nothing else waiting on you today.", false, ""},
		{"status update", "Finished the migration: 41 tables copied, row counts match, the old cluster is read-only until Friday.\n\nNext: I'll watch the error rate overnight.", false, ""},
		{"rhetorical", "Why did the import fail? The CSV had a BOM. Fixed and re-run; 2,310 rows loaded.", false, ""},
		{"code block question", "Added the prompt to the CLI:\n\n```go\nfmt.Println(\"Should I overwrite the file? [y/N]\")\n```\n\nShipped in v0.4.", false, ""},
		{"blockquote question", "You wrote:\n\n> Should I keep the old dashboard? Do you want both?\n\nI kept both and linked them from the home page.", false, ""},
		{"inline code question", "The flag `--ask \"should I retry?\"` now defaults to off.", false, ""},
		{"url query", "Report: https://example.test/r?id=7 is live.", false, ""},
		{"dont need your input", "I don't need your input on this one; it's reversible.", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, ok := OwnerQuestion(c.msg)
			if ok != c.want {
				t.Fatalf("OwnerQuestion(%q) = %v (%q), want %v", c.msg, ok, q, c.want)
			}
			if ok && !strings.HasPrefix(q, c.q) {
				t.Fatalf("question = %q, want prefix %q", q, c.q)
			}
		})
	}
}

// Only the tail counts: a question answered earlier in a long report does
// not trigger.
func TestOwnerQuestionTail(t *testing.T) {
	msg := "Should I use Postgres? I checked and yes, Postgres.\n" + strings.Repeat("Loaded batch and verified counts.\n", 60)
	if q, ok := OwnerQuestion(msg); ok {
		t.Fatalf("question outside the tail counted: %q", q)
	}
	if q, ok := OwnerQuestion(strings.Repeat("x", 300) + "\n" + "Which region should I use?"); !ok || q != "Which region should I use?" {
		t.Fatalf("got %q %v", q, ok)
	}
	long := "Should I " + strings.Repeat("really ", 60) + "do it?"
	if q, ok := OwnerQuestion(long); !ok || !strings.HasSuffix(q, "…") || len(q) > maxQuestion+len("…") {
		t.Fatalf("long question not bounded: %q", q)
	}
}
