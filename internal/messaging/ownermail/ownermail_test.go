package ownermail

import (
	"strings"
	"testing"

	"github.com/jhh3/pyre/internal/contracts"
)

func TestUpdate(t *testing.T) {
	owner := contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}
	session := contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synthetic"}
	system := contracts.Principal{Kind: contracts.PrincipalSystem, ID: "cleanup"}
	env := func(text string) contracts.MessageEnvelope {
		return contracts.MessageEnvelope{Sender: session, Recipient: owner, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: text}}
	}
	cases := []struct {
		name    string
		env     func() contracts.MessageEnvelope
		toOwner bool
		want    bool
	}{
		{"status report", func() contracts.MessageEnvelope { return env("Merged #12 and deployed at 1:15pm PT. Nothing urgent.") }, false, true},
		{"correction", func() contracts.MessageEnvelope {
			return env("Ignore my previous message: the coordinator already covered it.")
		}, false, true},
		{"negated", func() contracts.MessageEnvelope { return env("Digest for Monday. Nothing needs you today.") }, false, true},
		{"question", func() contracts.MessageEnvelope { return env("The fix is ready. Should I merge it now?") }, false, false},
		{"no-reply label does not outrank a question", func() contracts.MessageEnvelope {
			return env("FYI (no reply needed): deploy done. Separately, should I delete the old bucket?")
		}, false, false},
		{"bare fyi question", func() contracts.MessageEnvelope { return env("FYI: prod is down. Should I roll back?") }, false, false},
		{"correction with a new question", func() contracts.MessageEnvelope {
			return env("Ignore my previous question. Should I deploy to staging or prod instead?")
		}, false, false},
		{"no action phrase, then a question", func() contracts.MessageEnvelope {
			return env("CI green, no action needed on the flake. Should I merge?")
		}, false, false},
		{"info mode with request ack", func() contracts.MessageEnvelope {
			e := env("Prod keys rotated; update the Android build.")
			e.Mode, e.RequestAck = contracts.ModeInfo, true
			return e
		}, false, false},
		{"decision phrase", func() contracts.MessageEnvelope { return env("PR #13377 is ready for your call: CI green.") }, false, false},
		{"request ack", func() contracts.MessageEnvelope { e := env("Heads-up: prod is down."); e.RequestAck = true; return e }, false, false},
		{"reply to owner", func() contracts.MessageEnvelope { e := env("On it."); e.InReplyTo = "msg_synthetic"; return e }, true, false},
		{"reply to a session", func() contracts.MessageEnvelope { e := env("Done."); e.InReplyTo = "msg_synthetic"; return e }, false, true},
		{"info mode question", func() contracts.MessageEnvelope {
			e := env("Should I merge it?")
			e.Mode = contracts.ModeInfo
			return e
		}, false, false},
		{"ask carrier", func() contracts.MessageEnvelope {
			e := env("[pyre ask] Merge?")
			e.ThreadID = contracts.NewID(contracts.PrefixAsk)
			return e
		}, false, false},
		{"pyre queue mail", func() contracts.MessageEnvelope {
			e := env("[pyre cleanup] failed on server")
			e.Sender = system
			return e
		}, false, false},
		{"pyre info mail", func() contracts.MessageEnvelope {
			e := env("[pyre cleanup] archived 3 sessions")
			e.Sender, e.Mode = system, contracts.ModeInfo
			return e
		}, false, true},
		{"owner note to self", func() contracts.MessageEnvelope {
			e := env("remember the milk")
			e.Sender = contracts.Principal{Kind: contracts.PrincipalOwner, ID: "owner"}
			return e
		}, false, false},
		{"to a session", func() contracts.MessageEnvelope {
			e := env("fyi")
			e.Recipient = contracts.Address{Kind: contracts.AddrSession, ID: "ses_other"}
			return e
		}, false, false},
	}
	for _, tc := range cases {
		toOwner := tc.toOwner
		if got := Update(tc.env(), func() bool { return toOwner }); got != tc.want {
			t.Errorf("%s: Update = %v, want %v", tc.name, got, tc.want)
		}
	}
	if !Question(env("Which branch should I use?")) || Question(env("Merged.")) {
		t.Fatal("Question")
	}
}

// The review's battery (PR #25): requests, urgency and incidents written
// without a "should I …?" stay in the inbox; so does any question, however
// softened or deep in the message; plain reports are updates.
func TestUpdateFailsTowardTheInbox(t *testing.T) {
	report := strings.Repeat("Loaded the synthetic batch and verified the counts. ", 40)
	cases := []struct {
		text   string
		update bool
	}{
		{"URGENT: prod is down. The API returns 500 for all requests since 10:02.", false},
		{"Prod is down — the hub on server crashed and sessions cannot connect.", false},
		{"Security incident: credentials leaked in the public repo.", false},
		{"Action required: rotate the leaked AWS key now.", false},
		{"Heads up: I will drop the production table in 10 minutes unless you stop me.", false},
		{"Please approve the deploy to production.", false},
		{"Please review PR 41 and merge it when you can.", false},
		{"Let me know if you want changes.", false},
		{"LMK which option you prefer.", false},
		{"I need the Stripe API key to continue.", false},
		{"Blocked: need a new GitHub token from you.", false},
		{"Can you run pyre login on server", false},
		{"I'll go ahead with option B unless you object.", false},
		{"Nothing urgent, but should I merge PR 41?", false},
		{"No rush, should I merge PR 41?", false},
		{"Not urgent, do you want me to deploy?", false},
		{"Quick question: should I merge PR 41?\n" + report, false},
		{"status?", false},
		{"Thoughts?", false},
		{"Ship it?", false},
		{"OK?", false},
		{"Should I merge?", false},
		{"Waiting on your go-ahead.", false},
		{"Done… Waiting for your review.", false},
		// Plain reports stay updates.
		{"Merged #12 and deployed at 1:15pm PT. Nothing urgent.", true},
		{"Synthetic digest for Monday: 15 reports, 10 new issues, 8 PRs merged.", true},
		{"Ignore my previous message: the coordinator already covered it.", true},
		{"Wraparound fix run done, no user impact. Scripts are in the synthetic repo.", true},
	}
	for _, c := range cases {
		e := contracts.MessageEnvelope{Sender: contracts.Principal{Kind: contracts.PrincipalSession, ID: "ses_synthetic"},
			Recipient: contracts.Address{Kind: contracts.AddrOwner, ID: "owner"}, Mode: contracts.ModeQueue, Body: contracts.MessageBody{Text: c.text}}
		if got := Update(e, func() bool { return false }); got != c.update {
			t.Errorf("Update(%.60q) = %v, want %v", c.text, got, c.update)
		}
	}
}
