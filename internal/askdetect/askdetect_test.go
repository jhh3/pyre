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

// The key is the asking sentence, so idle reports that repeat one request
// in other words around it give the same key, and a new request another.
func TestQuestionKey(t *testing.T) {
	a := "Nothing new from me… I'm still waiting on your yes to publish the synthetic voice post. The GPU update you have already seen."
	b := "Nothing new from me: the cost report was routine. I'm still waiting on your yes to publish the synthetic voice post."
	c := "Done with the render. Should I publish the synthetic voice post now?"
	ka, kb, kc := QuestionKey(a), QuestionKey(b), QuestionKey(c)
	if ka == "" || ka != kb {
		t.Fatalf("repeat: %q vs %q", ka, kb)
	}
	if ka != "i m still waiting on your yes to publish the synthetic voice post" {
		t.Fatalf("key %q", ka)
	}
	if kc == "" || kc == ka {
		t.Fatalf("new question: %q", kc)
	}
	if k := QuestionKey("All done, nothing needs you."); k != "" {
		t.Fatalf("no question: %q", k)
	}
}

// The key keeps a waiting sentence's subject (no punctuation before the
// phrase) and covers every asking sentence, so a new question next to a
// repeated status line is not a repeat.
func TestQuestionKeySubjectAndAllSentences(t *testing.T) {
	pairs := [][2]string{
		{"PR 41 merge is waiting on your go-ahead for the release", "Bucket deletion is waiting on your go-ahead for the release"},
		{"Deleting prod-db backups needs your approval before the window closes", "Rotating the Stripe key needs your approval before the window closes"},
	}
	for _, p := range pairs {
		a, b := QuestionKey(p[0]), QuestionKey(p[1])
		if a == "" || b == "" || a == b {
			t.Fatalf("distinct requests share a key: %q / %q", a, b)
		}
	}
	status := "Status: still waiting on your go-ahead to publish the launch post."
	with := status + "\n\nNew: the CDN cert expires tomorrow. Should I renew it with the new account?"
	if a, b := QuestionKey(status), QuestionKey(with); a == "" || a == b || !strings.Contains(b, a) {
		t.Fatalf("a new question beside a repeated line: %q / %q", a, b)
	}
}

func TestMailAsks(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{"status?", true},
		{"No rush, should I merge PR 41?", true},
		{"Quick question: should I merge PR 41?\n" + strings.Repeat("Loaded batch and verified counts.\n", 80), true},
		{"Done. Waiting for your review.", true},
		{"See https://example.test/x?y=1 for the run. Merged.", false},
		{"Merged #12. Nothing needs you.", false},
		{"```\nwhy?\n```\nMerged.", false},
	} {
		if got := MailAsks(c.msg); got != c.want {
			t.Errorf("MailAsks(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}

// A waiting phrase the message quotes or uses to describe something is not
// the writer waiting (owner report 2026-10-19: a release report's line
// 'A session repeating "still waiting on you" no longer brings its flag
// back' raised a waiting item on the lead that wrote it).
func TestOwnerQuestionQuotedAndDescriptive(t *testing.T) {
	// The turn that raised att_01M508879YXEM73B331DHT00AX (synthetic SHA).
	release := "The inbox fix is live: rc83 is on all five machines, each one is connected, and your inbox is empty (0 unread, 0 flags).\n\n" +
		"- **What changed:**\n" +
		"  - An ask's message now clears when the ask closes.\n" +
		"  - Status updates stay out of your inbox unless they ask you something or sound urgent.\n" +
		"  - Opening a message marks it read everywhere.\n" +
		"  - A session repeating \"still waiting on you\" no longer brings its flag back.\n" +
		"- **Build:** made from main at 0000000 (https://example.test/pull/25). I checked its checksums, release signature and source commit.\n" +
		"- **Risk:** this is a hub change to what reaches your inbox, on every client. If it misbehaves, each machine can go back with `upgrade.sh --rollback`."
	notOwner := []string{
		release,
		"- **Waiting flags:** a session repeating the same \"still waiting on you\" no longer brings its flag back.",
		"A session repeating \"still waiting on you\" no longer brings its flag back.",
		"A session repeating “still waiting on you” no longer brings its flag back.",
		"A session repeating 'still waiting on you' no longer brings its flag back.",
		"A session repeating still waiting on you no longer brings its flag back.",
		"The \"waiting on you\" check uses the same detector.",
		"Sessions waiting on you now show a badge in the TUI.",
		"Waiting on you items no longer reopen on a repeat.",
		"The needs you flag clears when you type into the session.",
		"The web inbox lists a session when it needs your answer.",
		"If a lead is waiting on your go-ahead, the phone gets a push.",
		"Fixed the prompt \"Should I overwrite the file?\" in the CLI.",
		"It now treats ‘needs your approval’ as a request.",
		// Review of PR #26: a condition or a description mentioning the
		// writer is not the writer waiting.
		"I'll keep going and ping you when I need your input.",
		"Let me know if anything needs your approval.",
		"We now flag sessions waiting on you in the inbox.",
		"Our sessions waiting on you now get a badge.",
	}
	for _, msg := range notOwner {
		if q, ok := OwnerQuestion(msg); ok {
			t.Errorf("OwnerQuestion(%.70q) = %q, want none", msg, q)
		}
		if k := QuestionKey(msg); k != "" {
			t.Errorf("QuestionKey(%.70q) = %q, want none", msg, k)
		}
	}
	owner := []string{
		"I'm waiting on you to pick the launch date.",
		"I’m still waiting on your yes to publish the synthetic post.",
		"Rendered. Waiting on your go-ahead to publish.",
		"The merge is waiting on your decision about the schema.",
		"PR 41 is blocked on you: the release key.",
		"We need your answer on the synthetic pricing before Friday.",
		"Everything else is done; the deploy needs your sign-off.",
		"Still waiting on you: the \"beta\" label or the \"preview\" one.",
		"When you're back, the deploy is waiting on your go-ahead.",
		"Status: still waiting on your go-ahead to publish the launch post.",
		// Review of PR #26: -ing nouns and the writer's own items.
		"Final wording needs your approval.",
		"Onboarding needs your input before I continue.",
		"The new branding needs your sign-off.",
		"Rendering needs your call on the codec.",
		"One item needs your decision: merging PR 41.",
		"Open items awaiting your decision:",
		"Several items need your decision:",
		"The standing items awaiting your word: two signups.",
		"I have been waiting on your answer since Monday.",
	}
	for _, msg := range owner {
		if q, ok := OwnerQuestion(msg); !ok {
			t.Errorf("OwnerQuestion(%q) = none, want a hit", msg)
		} else if strings.Contains(q, "quote") {
			t.Errorf("returned line %q lost its quotes", q)
		}
	}
	// Mail keeps erring towards the inbox: a quoted phrase still counts there.
	if !MailAsks("A session repeating \"still waiting on you\" no longer brings its flag back.") {
		t.Error("MailAsks no longer sees a waiting phrase")
	}
}

func TestUnquote(t *testing.T) {
	for in, want := range map[string]string{
		`say "hi" now`:                                  `say quote now`,
		`don't stop 'here' ok`:                          `don't stop quote ok`,
		`it’s ‘fine’, isn’t it`:                         `it’s quote, isn’t it`,
		`a 12" screen`:                                  `a 12" screen`,
		`a 12" screen; I'm waiting on you for the "go"`: `a 12" screen; I'm waiting on you for the quote`,
		`"Should I merge?" asked`:                       `quote asked`,
	} {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%q) = %q, want %q", in, got, want)
		}
	}
}
