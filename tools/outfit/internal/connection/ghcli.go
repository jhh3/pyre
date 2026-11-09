package connection

import "strings"

// ghReadCommands lists, for the GitHub CLI (generated from gh 2.101.0's
// --help), each read-only command and the flags it may carry. A trailing
// "=" marks a flag that takes a value. -w/--web (opens a browser), auth
// status --show-token (prints the token), and every flag not listed here
// need --write, as does every command not listed here.
const ghReadCommands = `
pr view: -c --comments -q= --jq= --json= -t= --template= -R= --repo=
pr list: --app= -a= --assignee= -A= --author= -B= --base= -d --draft -H= --head= -q= --jq= --json= -l= --label= -L= --limit= -S= --search= -s= --state= -t= --template= -R= --repo=
pr diff: --allow-escape-sequences --color= -e= --exclude= --name-only --patch -R= --repo=
pr checks: --fail-fast -i= --interval= -q= --jq= --json= --required -t= --template= --watch -R= --repo=
pr status: -c --conflict-status -q= --jq= --json= -t= --template= -R= --repo=
issue view: -c --comments -q= --jq= --json= -t= --template= -R= --repo=
issue list: --app= -a= --assignee= -A= --author= -q= --jq= --json= -l= --label= -L= --limit= --mention= -m= --milestone= -S= --search= -s= --state= -t= --template= --type= -R= --repo=
issue status: -q= --jq= --json= -t= --template= -R= --repo=
run view: -a= --attempt= --exit-status -j= --job= -q= --jq= --json= --log --log-failed -t= --template= -v --verbose -R= --repo=
run list: -a --all -b= --branch= -c= --commit= --created= -e= --event= -q= --jq= --json= -L= --limit= -s= --status= -t= --template= -u= --user= -w= --workflow= -R= --repo=
run watch: --compact --exit-status -i= --interval= -R= --repo=
repo view: -b= --branch= -q= --jq= --json= -t= --template=
repo list: --archived --fork -q= --jq= --json= -l= --language= -L= --limit= --no-archived --source -t= --template= --topic= --visibility=
release view: -q= --jq= --json= -t= --template= -R= --repo=
release list: --exclude-drafts --exclude-pre-releases -q= --jq= --json= -L= --limit= -O= --order= -t= --template= -R= --repo=
workflow view: -r= --ref= -y --yaml -R= --repo=
workflow list: -a --all -q= --jq= --json= -L= --limit= -t= --template= -R= --repo=
search repos: --archived --created= --followers= --forks= --good-first-issues= --help-wanted-issues= --include-forks= -q= --jq= --json= --language= --license= -L= --limit= --match= --number-topics= --order= --owner= --size= --sort= --stars= -t= --template= --topic= --updated= --visibility=
search issues: --app= --archived --assignee= --author= --closed= --commenter= --comments= --created= --include-prs --interactions= --involves= -q= --jq= --json= --label= --language= -L= --limit= --locked --match= --mentions= --milestone= --no-assignee --no-label --no-milestone --no-project --order= --owner= --project= --reactions= -R= --repo= --search-type= --sort= --state= --team-mentions= -t= --template= --updated= --visibility=
search prs: --app= --archived --assignee= --author= -B= --base= --checks= --closed= --commenter= --comments= --created= --draft -H= --head= --interactions= --involves= -q= --jq= --json= --label= --language= -L= --limit= --locked --match= --mentions= --merged --merged-at= --milestone= --no-assignee --no-label --no-milestone --no-project --order= --owner= --project= --reactions= -R= --repo= --review= --review-requested= --reviewed-by= --sort= --state= --team-mentions= -t= --template= --updated= --visibility=
search code: --extension= --filename= -q= --jq= --json= --language= -L= --limit= --match= --owner= -R= --repo= --size= -t= --template=
search commits: --author= --author-date= --author-email= --author-name= --committer= --committer-date= --committer-email= --committer-name= --hash= -q= --jq= --json= -L= --limit= --merge --order= --owner= --parent= -R= --repo= --sort= -t= --template= --tree= --visibility=
label list: -q= --jq= --json= -L= --limit= --order= -S= --search= --sort= -t= --template= -R= --repo=
gist view: --allow-escape-sequences -f= --filename= --files -r --raw
gist list: --filter= --include-content -L= --limit= --public --secret
status: -e= --exclude= -o= --org=
browse: -n --no-browser -a --actions -b= --branch= -p --projects -r --releases -s --settings -w --wiki -R= --repo=
auth status: -a --active -h= --hostname= --jq= --json= --template=
cache list: -q= --jq= --json= -k= --key= -L= --limit= -O= --order= -r= --ref= -S= --sort= -t= --template= -R= --repo=
ruleset list: -L= --limit= -o= --org= -p --parents -R= --repo=
ruleset view: -o= --org= -p --parents -R= --repo=
secret list: -a= --app= -e= --env= -q= --jq= --json= -o= --org= -t= --template= -u --user -R= --repo=
variable list: -e= --env= -q= --jq= --json= -o= --org= -t= --template= -R= --repo=
variable get: -e= --env= -q= --jq= --json= -o= --org= -t= --template= -R= --repo=
org list: -L= --limit=
`

// ghReadFlags maps each read command to its allowed flags; the value is
// true when the flag takes a value.
var ghReadFlags = parseGHReadCommands(ghReadCommands)

func parseGHReadCommands(table string) map[string]map[string]bool {
	result := map[string]map[string]bool{}
	for _, line := range strings.Split(table, "\n") {
		command, flags, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		result[command] = map[string]bool{}
		for _, flag := range strings.Fields(flags) {
			name, value := strings.CutSuffix(flag, "=")
			result[command][name] = value
		}
	}
	return result
}

// ghHelpCommands are gh's built-in top-level commands, for which a trailing
// --help only prints help. Extensions and commands that hand their
// arguments to another program (copilot, preview, agent-task, skill) are
// left out: their --help is not gh's.
var ghHelpCommands = map[string]bool{"alias": true, "api": true, "attestation": true, "auth": true, "browse": true, "cache": true, "codespace": true, "completion": true, "config": true, "discussion": true, "extension": true, "gist": true, "gpg-key": true, "issue": true, "label": true, "licenses": true, "org": true, "pr": true, "project": true, "release": true, "repo": true, "ruleset": true, "run": true, "search": true, "secret": true, "ssh-key": true, "status": true, "variable": true, "workflow": true}

// ghReadOnly reports whether argv for the GitHub CLI provably cannot mutate
// state. It allows only:
//   - `api` calls that ghAPIReadOnly accepts;
//   - the commands in ghReadCommands (`ls` for `list`), with only their
//     listed flags, written after the subcommand; `browse` only with
//     -n/--no-browser;
//   - help and version.
//
// Everything else is a write, deny by default: other subcommands (pr
// create/merge/comment/checkout, issue edit, run rerun, repo clone, ...),
// -w/--web, an unknown or misplaced flag (including -R/--repo before the
// subcommand), `--`, a short flag group, a missing flag value, and a
// positional URL outside https://github.com/.
func ghReadOnly(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	if argv[0] == "api" {
		return ghAPIReadOnly(argv)
	}
	if ghHelpOnly(argv) {
		return true
	}
	command, rest := ghReadCommand(argv)
	flags, ok := ghReadFlags[command]
	if !ok {
		return false
	}
	noBrowser := false
	for index := 0; index < len(rest); index++ {
		arg := rest[index]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			// A URL argument makes gh talk to its host; only github.com.
			if strings.Contains(arg, "://") && !strings.HasPrefix(arg, "https://github.com/") {
				return false
			}
			continue
		}
		name, value, hasValue := splitGHFlag(arg)
		takesValue, allowed := flags[name]
		if !allowed {
			return false
		}
		if !takesValue {
			// --draft=false is a boolean spelling; -cv is a group of short
			// flags, which is not accepted.
			if hasValue && (!strings.HasPrefix(arg, "--") || (value != "true" && value != "false")) {
				return false
			}
			noBrowser = noBrowser || ((name == "-n" || name == "--no-browser") && value != "false")
			continue
		}
		// Like gh's flag parser, a value flag takes the next word even when
		// it starts with "-".
		if !hasValue {
			if index+1 >= len(rest) {
				return false
			}
			index++
		}
	}
	return command != "browse" || noBrowser
}

// ghReadCommand splits argv into the command path a ghReadCommands entry
// would name (one or two words, with `ls` read as `list`) and the rest.
func ghReadCommand(argv []string) (string, []string) {
	if _, ok := ghReadFlags[argv[0]]; ok {
		return argv[0], argv[1:]
	}
	if len(argv) < 2 {
		return "", nil
	}
	subcommand := argv[1]
	if subcommand == "ls" {
		subcommand = "list"
	}
	return argv[0] + " " + subcommand, argv[2:]
}

// ghHelpOnly accepts `help ...`, `version`, `--version`, a bare --help/-h,
// and a built-in command ending in --help/-h that cannot be the value of a
// preceding flag (`--body --help` sets a body).
func ghHelpOnly(argv []string) bool {
	switch {
	case argv[0] == "help":
		return true
	case len(argv) == 1:
		return argv[0] == "version" || argv[0] == "--version" || argv[0] == "--help" || argv[0] == "-h"
	case !ghHelpCommands[argv[0]]:
		return false
	}
	last := argv[len(argv)-1]
	if last != "--help" && last != "-h" {
		return false
	}
	return !strings.HasPrefix(argv[len(argv)-2], "-")
}
