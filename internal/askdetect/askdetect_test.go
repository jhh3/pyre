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
		{"descriptive your input", "Risk: this only changes how computer control on Windows tells your input apart from Vortex's.", false, ""},
		{"conditional status", "A third, final review is running. If it's clean, I'll merge.", false, ""},
		{"want your input", "I want your input on the rollout order before Friday.", true, "I want your input on the rollout order before Friday."},
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
		{"PR 41 is green. Please approve it.", true},
		{"Merged #12; it reports the next version for your approval.", false},
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

// Statements about asks made earlier or to come, status lines and reports
// of approvals already filed are not asks; real requests are, with or
// without a "?" (iss_…: a triage coordinator's
// status turns kept being sent back with the ask nudge).
func TestOwnerQuestionStatementsAndRequests(t *testing.T) {
	// The turn that was nudged first (synthetic names and paths).
	report := "The fix for VOY-1100, the slides editor losing edits, is up as PR #3765 (https://example.test/pull/3765). It's pushed and CI is green, but it isn't merged yet.\n\n" +
		"- **Cause:** after one refused save, the editor kept treating every later save as done.\n" +
		"- **Why it's not merged yet:** the second review found 5 serious issues. The worker is running one more review.\n" +
		"- **Your approval:** it adds two screens users will see, the close dialog and a \"kept both versions\" notice. Screenshots are in `~/Downloads/voy-1100/`.\n\n" +
		"When the review is clean I'll send the PR to you for approval."
	notOwner := []string{
		report,
		"Nothing is waiting on you yet. When the third review is clean, I'll file the #3765 approval in your Pyre inbox.",
		"The VOY-1100 worker is still running. Pyre reported its turn as finished when the session hit its length limit, but it picked straight back up. It's fixing what the third review found and rewriting the save check in full, then it reports the new version for your approval.",
		"Filed the #3765 approval in your Pyre inbox.",
		"I'll ask for your approval once the review is clean.",
		"The worker will need your sign-off after the third review.",
		"Your decision from Tuesday is applied: the old dashboard is gone.",
		"Approved by the second reviewer; merging now.",
		"Let me know if anything needs your approval.",
		"Let me know when you want the next batch.",
		"Please note: the build cache was cleared.",
		// A third thing's requirement, not the session asking.
		"That's your personal media drive (the Hawaii footage, films, DaVinci), and writing to its top level needs your password.",
		"The drive needs your password.",
		"Writing there requires your login.",
		"That repo needs your SSO.",
		"Mounting it needs your sudo password.",
		"The NAS share needs your login before anything can write to it.",
	}
	for _, msg := range notOwner {
		if q, ok := OwnerQuestion(msg); ok {
			t.Errorf("OwnerQuestion(%.70q) = %q, want none", msg, q)
		}
	}
	owner := []struct{ msg, q string }{
		{"Should I merge it?", "Should I merge it?"},
		{"Can you run aws login?", "Can you run aws login?"},
		{"Approve the PR when you get a chance.", "Approve the PR when you get a chance."},
		{"Tests pass. Want me to open the PR now?", "Tests pass. Want me to open the PR now?"},
		{"PR 41 is ready.\n\n- **Next:** please approve PR 41 before 5 PM.", "Next: please approve PR 41"},
		{"Please run aws login in your terminal.", "Please run aws login"},
		{"Let me know which layout you prefer.", "Let me know which layout"},
		{"Can you re-auth the GitHub CLI when you're back.", "Can you re-auth"},
		{"Merged the docs. Confirm the new pricing copy before I publish.", "Merged the docs."},
		{"I'd like your approval on the new close dialog.", "I'd like your approval"},
		{"Your approval is needed on the close dialog.", "Your approval is needed"},
		{"The release is waiting for your approval.", "The release is waiting"},
		{"I need your approval to merge PR 41.", "I need your approval"},
		{"This needs your approval before I merge.", "This needs your approval"},
		{"Waiting for your sign-off on the copy.", "Waiting for your sign-off"},
		{"I need your sudo password to write to the drive.", "I need your sudo password"},
		{"The deploy needs your final sign-off.", "The deploy needs your final sign-off"},
	}
	for _, c := range owner {
		if q, ok := OwnerQuestion(c.msg); !ok {
			t.Errorf("OwnerQuestion(%q) = none, want a hit", c.msg)
		} else if !strings.HasPrefix(q, c.q) {
			t.Errorf("OwnerQuestion(%q) = %q, want prefix %q", c.msg, q, c.q)
		}
		if QuestionKey(c.msg) == "" {
			t.Errorf("QuestionKey(%q) = none", c.msg)
		}
	}
}

// A question the message reports (another session asked it, or it was
// already handled) is not an ask, and a message that says nothing else
// needs the owner asks only with a direct question (owner report
// 2026-10-30: a lead's bug report describing a hidden yes/no question was
// sent back with the ask nudge).
func TestOwnerQuestionReported(t *testing.T) {
	report := "New Pyre bug, and one of your decisions got hidden by it. A fix session is working on it.\n" +
		"- **What happened:**\n" +
		"  - At about 5:01 PM the \"Vortex phone remote control research\" session asked you a yes/no question: should the production cloud image move?\n" +
		"  - Your next chat message to that session was about something else …\n" +
		"  - Pyre counted that message as your answer, so the question disappeared from your inbox even though you never decided it.\n" +
		"- **Your hidden question:** I'll tell you whether it's back in your inbox when the fix lands. Nothing else needs you."
	notOwner := []string{
		report,
		"At 5:01 PM the research session asked you a yes/no question: should the production cloud image move? It's back in your inbox now.",
		"Earlier the deploy lead asked: should we roll back the canary? I answered from your recorded decision; it rolled back at 6:12 PM.",
		"The billing lead is asking whether we should raise the Team price? That ask is in your Pyre inbox.",
		"The research session's question — should the production image move? — was answered by your 4 PM reply.",
		"Should the production cloud image move? — asked by the research session, already in your inbox.",
		"Fix is merged. Should the image move? That one is the research session's, and it is handling it. Nothing else needs you.",
		"- **Status:** the review is green. Which region wins is the lead's to decide.\n- Nothing needs you.",
	}
	for _, msg := range notOwner {
		if q, ok := OwnerQuestion(msg); ok {
			t.Errorf("OwnerQuestion(%.70q) = %q, want none", msg, q)
		}
		if k := QuestionKey(msg); k != "" {
			t.Errorf("QuestionKey(%.70q) = %q, want none", msg, k)
		}
	}
	owner := []struct{ msg, q string }{
		{"Nothing else needs you. Should I merge it?", "Nothing else needs you. Should I merge it?"},
		{"The flaky test is fixed. Want me to retry the deploy? Nothing else needs you.", "The flaky test is fixed."},
		{report + "\n\nShould I merge it?", "Should I merge it?"},
		{report + "\n\nWant me to retry?", "Want me to retry?"},
		{"Nothing else needs you.\n\n- **Next:** should I merge PR 41?", "Next: should I merge PR 41?"},
		{"The research session asked you: should the image move? Should I answer it with your default?", "The research session asked you"},
		{"I asked you earlier: should I merge PR 41?", "I asked you earlier"},
		{"Should I keep asking if the endpoint is down?", "Should I keep asking"},
		{"Nothing else needs you, but please approve PR 41 before 5 PM.", "Nothing else needs you, but please approve"},
	}
	for _, c := range owner {
		if q, ok := OwnerQuestion(c.msg); !ok {
			t.Errorf("OwnerQuestion(%.70q) = none, want a hit", c.msg)
		} else if !strings.HasPrefix(q, c.q) {
			t.Errorf("OwnerQuestion(%.70q) = %q, want prefix %q", c.msg, q, c.q)
		}
		if QuestionKey(c.msg) == "" {
			t.Errorf("QuestionKey(%.70q) = none", c.msg)
		}
	}
}

// The Stop hook's stricter check: a status line that only says something
// waits on the owner (already flagged with flag_attention) is no decision
// (iss_…); questions, requests and waits on a
// choice still are.
func TestDecisionAsk(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{
		{"issue status line", "Updated the project page (revision 81): Codex fix in flight, Stripe login still waiting on you.", false},
		{"issue login waiting", "Merged PR 41 and restarted the worker. Stripe login is still waiting on you; everything else is moving.", false},
		{"issue needs you", "Project page updated (revision 82). Two lanes running, the Stripe login on server needs you, the rest is on track.", false},

		{"question", "Tests pass on the branch.\n\nWant me to open the PR now?", true},
		{"should I", "Both layouts render. Should I ship the dark one?", true},
		{"request", "The branch is ready. Please approve PR 41 when you get a chance.", true},
		{"wait on decisions", "Draft is in reports/q4.md.\n\nThe report is still waiting on your 3 decisions: the target customer, the GPU credit rate, and the supplier list.", true},
		{"wait on go-ahead", "PR 41 merge is waiting on your go-ahead.", true},
		{"wait then pick", "Both mockups are up. Waiting on you to pick a layout.", true},
		{"your call", "Both layouts work. Your call on which one ships.", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, ok := DecisionAsk(c.msg)
			if ok != c.want {
				t.Fatalf("DecisionAsk(%q) = %v (%q), want %v", c.msg, ok, q, c.want)
			}
		})
	}
	// OwnerQuestion (the hub's waiting item) still sees the plain wait.
	if _, ok := OwnerQuestion(cases[0].msg); !ok {
		t.Fatalf("OwnerQuestion lost the plain wait")
	}
}
