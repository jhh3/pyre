package connection

import (
	"regexp"
	"strings"
	"testing"

	"github.com/jhh3/outfit/internal/builtin"
	"github.com/jhh3/outfit/internal/kit"
)

func TestGHReadOnlyClassification(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		read bool
	}{
		// The reported call: statusCheckRollup has no typed operation.
		{"pr view json", []string{"pr", "view", "3420", "-R", "synthetic-org/synthetic-repo", "--json", "body,headRefOid,statusCheckRollup"}, true},
		{"pr view url", []string{"pr", "view", "https://github.com/synthetic-org/synthetic-repo/pull/1", "--comments"}, true},
		{"pr view jq template", []string{"pr", "view", "1", "--json=title", "-q", ".title", "--template", "{{.title}}"}, true},
		{"pr view attached repo", []string{"pr", "view", "1", "-Rsynthetic-org/synthetic-repo", "--repo=synthetic-org/synthetic-repo"}, true},
		{"pr list", []string{"pr", "list", "--state", "merged", "--limit", "5", "-A", "synthetic-user", "--draft"}, true},
		{"pr ls", []string{"pr", "ls", "-L5"}, true},
		{"pr list draft=false", []string{"pr", "list", "--draft=false"}, true},
		{"pr diff", []string{"pr", "diff", "1", "--name-only", "--color", "never"}, true},
		{"pr checks watch", []string{"pr", "checks", "1", "--watch", "--fail-fast", "-i", "5", "--required"}, true},
		{"pr status", []string{"pr", "status", "-c"}, true},
		{"issue view", []string{"issue", "view", "7", "--comments", "--json", "body"}, true},
		{"issue list", []string{"issue", "list", "--label", "bug", "--state", "all", "-S", "label:bug"}, true},
		{"issue status", []string{"issue", "status"}, true},
		{"run view log", []string{"run", "view", "123", "--log-failed", "-j", "456"}, true},
		{"run list workflow", []string{"run", "list", "-w", "ci.yml", "-b", "main", "-c", "abc123"}, true},
		{"run watch", []string{"run", "watch", "123", "--exit-status", "--compact"}, true},
		{"repo view", []string{"repo", "view", "synthetic-org/synthetic-repo", "--json", "defaultBranchRef"}, true},
		{"repo list", []string{"repo", "list", "synthetic-org", "--limit", "100", "--no-archived"}, true},
		{"release view", []string{"release", "view", "v1.0.0", "-R", "synthetic-org/synthetic-repo"}, true},
		{"release list", []string{"release", "list", "--exclude-drafts"}, true},
		{"workflow view", []string{"workflow", "view", "ci.yml", "--yaml", "-r", "main"}, true},
		{"workflow list", []string{"workflow", "list", "--all"}, true},
		{"search prs", []string{"search", "prs", "--repo", "synthetic-org/synthetic-repo", "--state", "open", "is:draft", "--checks=failure"}, true},
		{"search code", []string{"search", "code", "func main", "--language", "go", "-L", "10"}, true},
		{"search commits", []string{"search", "commits", "--author", "synthetic-user", "--merge"}, true},
		{"label list", []string{"label", "list", "--search", "bug"}, true},
		{"gist view", []string{"gist", "view", "0123abcd", "--raw", "--files"}, true},
		{"gist list", []string{"gist", "list", "--public"}, true},
		{"status", []string{"status", "-o", "synthetic-org"}, true},
		{"browse no browser", []string{"browse", "123", "--no-browser"}, true},
		{"browse -n settings", []string{"browse", "-n", "--settings", "-R", "synthetic-org/synthetic-repo"}, true},
		{"auth status", []string{"auth", "status", "--active"}, true},
		{"secret list", []string{"secret", "list", "--env", "prod"}, true},
		{"value flag takes a dash word", []string{"pr", "list", "--search", "--web"}, true},
		{"help", []string{"help", "pr", "merge"}, true},
		{"trailing help on a write", []string{"pr", "merge", "1", "--help"}, true},
		{"bare help", []string{"--help"}, true},
		{"version", []string{"version"}, true},
		{"--version", []string{"--version"}, true},
		{"api still on its own rules", []string{"api", "repos/OWNER/REPO"}, true},

		{"pr create", []string{"pr", "create", "--fill"}, false},
		{"pr merge", []string{"pr", "merge", "1", "--squash"}, false},
		{"pr close", []string{"pr", "close", "1"}, false},
		{"pr comment", []string{"pr", "comment", "1", "--body", "x"}, false},
		{"pr review", []string{"pr", "review", "1", "--approve"}, false},
		{"pr edit", []string{"pr", "edit", "1", "--title", "x"}, false},
		{"pr ready", []string{"pr", "ready", "1"}, false},
		{"pr checkout", []string{"pr", "checkout", "1"}, false},
		{"co alias", []string{"co", "1"}, false},
		{"issue create", []string{"issue", "create", "--title", "x"}, false},
		{"issue close", []string{"issue", "close", "1"}, false},
		{"issue comment ending in a read verb", []string{"issue", "comment", "1", "--body", "list"}, false},
		{"issue edit", []string{"issue", "edit", "1", "--add-label", "status"}, false},
		{"run rerun", []string{"run", "rerun", "123"}, false},
		{"run cancel", []string{"run", "cancel", "123"}, false},
		{"run download", []string{"run", "download", "123"}, false},
		{"repo create", []string{"repo", "create", "x"}, false},
		{"repo clone", []string{"repo", "clone", "synthetic-org/synthetic-repo"}, false},
		{"repo fork", []string{"repo", "fork"}, false},
		{"repo delete", []string{"repo", "delete", "x", "--yes"}, false},
		{"release create", []string{"release", "create", "v1"}, false},
		{"release download", []string{"release", "download", "v1"}, false},
		{"workflow run", []string{"workflow", "run", "ci.yml"}, false},
		{"workflow enable", []string{"workflow", "enable", "ci.yml"}, false},
		{"label create", []string{"label", "create", "list"}, false},
		{"gist create", []string{"gist", "create", "file"}, false},
		{"search no subcommand", []string{"search"}, false},
		{"pr view web", []string{"pr", "view", "1", "--web"}, false},
		{"pr view -w", []string{"pr", "view", "1", "-w"}, false},
		{"pr list web=true", []string{"pr", "list", "--web=true"}, false},
		{"search web", []string{"search", "prs", "x", "-w"}, false},
		{"repo view web", []string{"repo", "view", "-w"}, false},
		{"browse opens a browser", []string{"browse", "123"}, false},
		{"browse no-browser=false", []string{"browse", "--no-browser=false"}, false},
		{"browse commit optional value", []string{"browse", "-n", "-c"}, false},
		{"auth status show token", []string{"auth", "status", "--show-token"}, false},
		{"auth status -t", []string{"auth", "status", "-t"}, false},
		{"auth token", []string{"auth", "token"}, false},
		{"global repo flag before subcommand", []string{"-R", "synthetic-org/synthetic-repo", "pr", "view", "1"}, false},
		{"repo flag between command words", []string{"pr", "-R", "synthetic-org/synthetic-repo", "view", "1"}, false},
		{"global hostname flag", []string{"--hostname", "github.example", "pr", "list"}, false},
		{"unknown flag", []string{"pr", "view", "1", "--frobnicate"}, false},
		{"flag from another command", []string{"pr", "view", "1", "--state", "open"}, false},
		{"short flag group", []string{"pr", "view", "1", "-cw"}, false},
		{"bool flag with value", []string{"pr", "list", "--draft=yes"}, false},
		{"missing flag value", []string{"pr", "view", "1", "--json"}, false},
		{"double dash", []string{"pr", "view", "--", "1"}, false},
		{"non-github URL", []string{"pr", "view", "https://github.example/synthetic-org/synthetic-repo/pull/1"}, false},
		{"help as a flag value", []string{"pr", "comment", "1", "--body", "--help"}, false},
		{"extension help", []string{"copilot", "--help"}, false},
		{"empty", []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cliReadOnly("gh", tc.argv); got != tc.read {
				t.Fatalf("cliReadOnly(gh, %q) = %v, want %v", tc.argv, got, tc.read)
			}
		})
	}
}

func TestGHRawClassificationMatchesTypedOperations(t *testing.T) {
	// Running a typed GitHub CLI operation's own argv through `outfit exec`
	// must give the same answer as its declared class.
	for _, line := range strings.Split(ghReadCommands, "\n") {
		if line != "" && !strings.Contains(line, ":") {
			t.Fatalf("malformed table line %q", line)
		}
	}
	packages, err := builtin.All()
	if err != nil {
		t.Fatal(err)
	}
	var mode kit.Mode
	for _, loaded := range packages {
		if loaded.Package.Key == "github" {
			mode, _ = kit.FindMode(loaded.Package, "cli")
		}
	}
	if len(mode.Operations) == 0 {
		t.Fatal("github cli mode not found")
	}
	placeholder := regexp.MustCompile(`\{\{arg\.[a-z_]+\}\}`)
	for _, operation := range mode.Operations {
		argv := []string{}
		for _, arg := range operation.CLI.Args {
			argv = append(argv, placeholder.ReplaceAllString(arg, "SYNTHETIC"))
		}
		want := operation.Class == "read"
		if operation.Name == "search_code" {
			// gh api with -f is a write to the raw classifier even with an
			// explicit GET (outfit#8); the typed operation is a read.
			want = false
		}
		if got := cliReadOnly("gh", argv); got != want {
			t.Errorf("%s (%s): raw %q read = %v, want %v", operation.Name, operation.Class, argv, got, want)
		}
	}
}
