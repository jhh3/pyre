package envfile

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := "\xef\xbb\xbf# synthetic test file\n" +
		"\n" +
		"AGENT_BRIDGE_TOKEN=synthetic-not-a-secret\n" +
		"export AGENT_INTAKE_URL=http://127.0.0.1:8091\n" +
		"TRACK_INTAKE_WORKTREE = /tmp/worktree  # comment\n" +
		"ANTHROPIC_API_KEY=\n" +
		"SINGLE='a # b \\n'\n" +
		"DOUBLE=\"x\\ny \\\"q\\\" # kept\" # dropped\n" +
		"HASH=a#b\r\n" +
		"EMPTY=\n"
	v, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"AGENT_BRIDGE_TOKEN":    "synthetic-not-a-secret",
		"AGENT_INTAKE_URL":      "http://127.0.0.1:8091",
		"TRACK_INTAKE_WORKTREE": "/tmp/worktree",
		"SINGLE":                `a # b \n`,
		"DOUBLE":                "x\ny \"q\" # kept",
		"HASH":                  "a#b",
		"EMPTY":                 "",
	}
	if !reflect.DeepEqual(v.Set, want) {
		t.Fatalf("set = %#v", v.Set)
	}
	if !reflect.DeepEqual(v.Unset, []string{"ANTHROPIC_API_KEY"}) {
		t.Fatalf("unset = %v", v.Unset)
	}
	red := strings.Join(v.Redacted(), " ")
	if strings.Contains(red, "synthetic-not-a-secret") || !strings.Contains(red, "AGENT_BRIDGE_TOKEN=<redacted>") ||
		!strings.Contains(red, "ANTHROPIC_API_KEY (unset)") || !strings.Contains(red, "EMPTY=") {
		t.Fatalf("redacted = %s", red)
	}
}

func TestParseRefusesAndNeverEchoesValues(t *testing.T) {
	const secret = "synthetic-secret-value"
	for _, tc := range []struct{ line, want string }{
		{"PYRE_SESSION_TOKEN=" + secret, "PYRE_SESSION_TOKEN is set by Pyre"},
		{"PYRE_X=", "PYRE_X is set by Pyre"},
		{"HOME=" + secret, "HOME is set by Pyre"},
		{"PATH=", "PATH is set by Pyre"},
		{"CLAUDE_CONFIG_DIR=" + secret, "CLAUDE_CONFIG_DIR is set by Pyre"},
		{"ANTHROPIC_API_KEY=" + secret, "ANTHROPIC_API_KEY is a login"},
		{"ANTHROPIC_BASE_URL=" + secret, "ANTHROPIC_BASE_URL is a login"},
		{"CLAUDE_CODE_OAUTH_TOKEN=" + secret, "CLAUDE_CODE_OAUTH_TOKEN is a login"},
		{"CODEX_HOME=" + secret, "CODEX_HOME is a login"},
		{"OPENAI_API_KEY=" + secret, "OPENAI_API_KEY is a login"},
		// Case variants (Windows names are case-insensitive).
		{"anthropic_api_key=" + secret, "anthropic_api_key is a login"},
		{"Path=C:\\evil", "Path is set by Pyre"},
		{"pyre_session_token=" + secret, "pyre_session_token is set by Pyre"},
		{"claude_config_dir=" + secret, "claude_config_dir is set by Pyre"},
		{"Codex_Home=" + secret, "Codex_Home is a login"},
		// Windows homes, tmux and Pyre's private temp dir.
		{"USERPROFILE=C:\\Users\\x", "USERPROFILE is set by Pyre"},
		{"HOMEDRIVE=C:", "HOMEDRIVE is set by Pyre"},
		{"HOMEPATH=\\Users\\x", "HOMEPATH is set by Pyre"},
		{"APPDATA=" + secret, "APPDATA is set by Pyre"},
		{"LocalAppData=" + secret, "LocalAppData is set by Pyre"},
		{"TMUX=/tmp/x,1,0", "TMUX is set by Pyre"},
		{"TMUX_PANE=%1", "TMUX_PANE is set by Pyre"},
		{"CLAUDE_CODE_TMPDIR=/tmp", "CLAUDE_CODE_TMPDIR is set by Pyre"},
		// The shared login list (contracts.IsLoginEnv).
		{"CLAUDE_CODE_USE_FOUNDRY=1", "CLAUDE_CODE_USE_FOUNDRY is a login"},
		{"CLAUDE_CODE_OAUTH_REFRESH_TOKEN=" + secret, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN is a login"},
		{"GOOGLE_APPLICATION_CREDENTIALS=/x.json", "GOOGLE_APPLICATION_CREDENTIALS is a login"},
		{"GOOGLE_GENAI_USE_VERTEXAI=true", "GOOGLE_GENAI_USE_VERTEXAI is a login"},
		{"GEMINI_MODEL=" + secret, "GEMINI_MODEL is a login"},
		{secret, "line 1: not KEY=VALUE"},
		{"1BAD=" + secret, "line 1: not KEY=VALUE"},
		{"Q=\"" + secret, "unterminated double quote"},
		{"Q='" + secret + "' junk", "after the closing quote"},
		{"A=1\nA=" + secret, "A is already set on line 1"},
	} {
		_, err := Parse(strings.NewReader(tc.line + "\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.line, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%q: error echoes the value: %v", tc.line, err)
		}
	}
	// Empty auth keys unset; controlled keys stay refused even empty.
	v, err := Parse(strings.NewReader("ANTHROPIC_API_KEY=\nCLAUDE_CODE_OAUTH_TOKEN=''\nOPENAI_API_KEY=\"\"\nCODEX_HOME=\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Set) != 0 || len(v.Unset) != 4 {
		t.Fatalf("vars = %+v", v)
	}
	// Escalation-shaped keys stay allowed: whoever can write the file is
	// already the node user (agreed in review).
	if v, err := Parse(strings.NewReader("NODE_OPTIONS=--max-old-space-size=4096\nLD_PRELOAD=/x.so\nNODE_EXTRA_CA_CERTS=/ca.pem\n")); err != nil || len(v.Set) != 3 {
		t.Fatalf("ordinary keys refused: %+v %v", v, err)
	}
	if _, err := Parse(strings.NewReader(strings.Repeat("A", MaxSize+1))); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestReadChecksFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits; Windows checks the ACL (fileperm)")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "aide.env")
	if err := os.WriteFile(p, []byte("K=synthetic\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := Read(p); err != nil || v.Set["K"] != "synthetic" {
		t.Fatalf("read = %+v, %v", v, err)
	}
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o620, 0o602, 0o666} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(p); err == nil || !strings.Contains(err.Error(), "accessible to other users") || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("mode %o: err = %v", mode, err)
		}
	}
	_ = os.Chmod(p, 0o600)
	for _, bad := range []string{"aide.env", "/dev/fd/3", "/dev/stdin", "/proc/self/environ", "/sys/kernel/x", "//server/share/x.env"} {
		if _, err := Read(bad); err == nil || !strings.Contains(err.Error(), "refused") && !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("%s: %v", bad, err)
		}
	}
	link := filepath.Join(dir, "link.env")
	if err := os.Symlink(p, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link); err == nil {
		t.Fatal("symlink accepted")
	}
	if _, err := Read(dir); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := Read(filepath.Join(dir, "missing.env")); err == nil {
		t.Fatal("missing file accepted")
	}
}
