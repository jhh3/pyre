package credentials

import (
	"strings"
	"testing"
)

// Every credential shape RedactPatterns knows is removed, wherever it sits
// (prefixed env names, glued to identifiers, inside URLs), and ordinary
// text is kept. All values are synthetic.
func TestRedactPatternsShapes(t *testing.T) {
	for _, tc := range []struct{ in, leak, keep string }{
		{"ANTHROPIC_API_KEY=sk-ant-api03-SYNTHETICsyntheticSYNTHETIC00", "SYNTHETICsynthetic", "ANTHROPIC_API_KEY="},
		{"export GITHUB_TOKEN=synthetic0123456789", "synthetic0123456789", "GITHUB_TOKEN="},
		{"OPENAI_API_KEY: synthetic-value-0123", "synthetic-value-0123", "OPENAI_API_KEY:"},
		{`{"client_secret": "synthetic-secret-99"}`, "synthetic-secret-99", "client_secret"},
		{"DB_PASSWORD=hunter2synthetic", "hunter2synthetic", "DB_PASSWORD="},
		{"access_token=synthetic-access-1234", "synthetic-access-1234", "access_token="},
		{"SLACK_BOT_TOKEN: synthetic-bot-1234", "synthetic-bot-1234", "SLACK_BOT_TOKEN:"},
		{"AWS_SECRET_ACCESS_KEY=syntheticAWSsecret00", "syntheticAWSsecret00", "AWS_SECRET_ACCESS_KEY="},
		{"token: synthetic-note-secret", "synthetic-note-secret", "token:"},
		{"(sk-ant-api03-SYNTHETICsyntheticSYNTHETIC0123)", "SYNTHETICsynthetic", "("},
		{"key sk-ant-api03-SYNTHETICsyntheticSYNTHETIC0123 used", "SYNTHETICsyntheticSYNTHETIC0123", " used"},
		{"bare sk-proj-SYNTHETICsyntheticSYNTHETIC0123", "SYNTHETICsyntheticSYNTHETIC0123", "bare "},
		{"clone https://synthuser:synthpass123@git.example.invalid/r.git", "synthpass123", "@git.example.invalid/r.git"},
		{"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAAsynthetic\n-----END OPENSSH PRIVATE KEY-----\nafter", "b3BlbnNzaC1rZXktdjEAAAAAsynthetic", "after"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIEsynthetictruncated", "MIIEsynthetic", ""},
		{"slack xox" + "b-0000000000-SYNTHETICsynthetic", "SYNTHETICsynthetic", "slack "},
		{"slack xox" + "p-0000000000-SYNTHETICsynthetic", "SYNTHETICsynthetic", "slack "},
		{"gh MY_ghp_SYNTHETICsyntheticSYNTHETIC1234", "SYNTHETICsyntheticSYNTHETIC1234", "gh "},
		{"gho_SYNTHETICsyntheticSYNTHETIC1234", "SYNTHETICsyntheticSYNTHETIC1234", ""},
		{"github_pat_11SYNTHETIC_syntheticSYNTHETIC1234", "syntheticSYNTHETIC1234", ""},
		{"aws AKIASYNTHETIC0000000 ok", "AKIASYNTHETIC0000000", " ok"},
		{"Authorization: Bearer synthetic.bearer.value123", "synthetic.bearer.value123", "Bearer "},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzeW50aGV0aWMifQ.c3ludGhldGljc2lnbmF0dXJl", "eyJzdWIiOiJzeW50aGV0aWMifQ", "jwt "},
	} {
		out := RedactPatterns(tc.in)
		if strings.Contains(out, tc.leak) || !strings.Contains(out, Redacted) || !strings.Contains(out, tc.keep) {
			t.Errorf("%q -> %q (leak %q, keep %q)", tc.in, out, tc.leak, tc.keep)
		}
	}
	for _, plain := range []string{
		"send_message to ses_01JSYNTH0000000000000000AB reported delivered after 3s",
		"see https://example.invalid/synth/pull/1 and /home/synth/work",
		"the disk is full: 97% used on /dev/sda1",
		"fix/disk-space-cleanup-worktree-branch",
		"ask-owner-topic-routing-fix and task-runner-cleanup-scheduler, risk-assessment-for-the-rollout-plan",
		"topic_key: inbox-asks-dedupe",
		`{"idempotency_key": "op_01JSYNTH0000000000000000AB"}`,
		"sort_key=last_seen_at",
		"monkey: bananas",
		"input_tokens: 123456",
		"credentials: /home/synth/.config/gh/hosts.yml",
	} {
		if out := RedactPatterns(plain); out != plain {
			t.Errorf("ordinary text changed: %q -> %q", plain, out)
		}
	}
}
