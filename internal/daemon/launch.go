package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/jhh3/pyre/internal/stablepath"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/agentguide"
	"github.com/jhh3/pyre/internal/contracts"
)

// expandHome expands a leading "~/".
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// validateDir requires an absolute existing directory.
func validateDir(p string) (string, error) {
	if p == "" {
		return "", contracts.Errorf(contracts.ErrInvalid, "working directory is required")
	}
	p = expandHome(p)
	if !filepath.IsAbs(p) {
		return "", contracts.Errorf(contracts.ErrInvalid, "working directory %q must be absolute", p)
	}
	p = filepath.Clean(p)
	st, err := os.Stat(p)
	if err != nil {
		return "", contracts.Errorf(contracts.ErrInvalid, "working directory %q does not exist on this node", p)
	}
	if !st.IsDir() {
		return "", contracts.Errorf(contracts.ErrInvalid, "working directory %q is not a directory", p)
	}
	return p, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), contracts.Errorf(contracts.ErrInvalid, "git %s: %s", args[0], strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

var branchSafe = regexp.MustCompile(`[^A-Za-z0-9._/-]+`)

// resolveCheckout returns the working directory and checkout binding for a
// launch. Worktree mode runs `git worktree add`, which never modifies the
// source checkout's working tree or index, so dirty repositories are safe;
// no stash/checkout/reset/--force is ever used. Re-running for the same
// run (restart mid-launch) reuses an existing worktree at the same path.
func (d *Daemon) resolveCheckout(ctx context.Context, cmd contracts.StartRunCommand) (string, *contracts.CheckoutBinding, error) {
	req := cmd.Checkout
	if req == nil {
		cwd, err := validateDir(cmd.Cwd)
		if err != nil {
			return "", nil, err
		}
		return cwd, nil, nil
	}
	switch req.Mode {
	case contracts.CheckoutShared, contracts.CheckoutPlain, "":
		p := req.Path
		if p == "" {
			p = cmd.Cwd
		}
		cwd, err := validateDir(p)
		if err != nil {
			return "", nil, err
		}
		mode := req.Mode
		if mode == "" {
			mode = contracts.CheckoutPlain
		}
		b := &contracts.CheckoutBinding{NodeID: d.nodeID, Path: cwd, RepoID: req.RepoID, Mode: mode, Branch: req.Branch}
		if br, err := git(ctx, cwd, "rev-parse", "--abbrev-ref", "HEAD"); err == nil && b.Branch == "" {
			b.Branch = br
		}
		return cwd, b, nil
	case contracts.CheckoutWorktree:
	default:
		return "", nil, contracts.Errorf(contracts.ErrInvalid, "unknown checkout mode %q", req.Mode)
	}
	src, err := validateDir(cmd.Cwd)
	if err != nil {
		return "", nil, contracts.Errorf(contracts.ErrInvalid, "worktree checkout needs the source repository as cwd: %v", err)
	}
	top, err := git(ctx, src, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, contracts.Errorf(contracts.ErrInvalid, "%s is not inside a git repository", src)
	}
	short := strings.ToLower(cmd.RunID)
	if i := strings.IndexByte(short, '_'); i >= 0 {
		short = short[i+1:]
	}
	if len(short) > 10 {
		short = short[len(short)-10:]
	}
	branch := req.Branch
	if branch == "" {
		branch = "pyre/" + short
	}
	if branchSafe.MatchString(branch) || strings.Contains(branch, "..") || strings.HasPrefix(branch, "-") {
		return "", nil, contracts.Errorf(contracts.ErrInvalid, "invalid branch name %q", branch)
	}
	path := req.Path
	if path == "" {
		name := strings.NewReplacer("/", "-").Replace(branch)
		path = filepath.Join(filepath.Dir(top), filepath.Base(top)+"-worktrees", name)
	}
	path = expandHome(path)
	if !filepath.IsAbs(path) {
		return "", nil, contracts.Errorf(contracts.ErrInvalid, "worktree path %q must be absolute", path)
	}
	path = filepath.Clean(path)
	binding := &contracts.CheckoutBinding{NodeID: d.nodeID, Path: path, RepoID: req.RepoID, Mode: contracts.CheckoutWorktree, Branch: branch}
	if st, err := os.Stat(path); err == nil {
		if !st.IsDir() {
			return "", nil, contracts.Errorf(contracts.ErrConflict, "worktree path %s exists and is not a directory", path)
		}
		// Reuse only if it is already a worktree of this repository on the
		// requested branch.
		common1, e1 := git(ctx, path, "rev-parse", "--git-common-dir")
		common2, e2 := git(ctx, top, "rev-parse", "--git-common-dir")
		br, e3 := git(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
		if e1 == nil && e2 == nil && e3 == nil && absGit(path, common1) == absGit(top, common2) && br == branch {
			return path, binding, nil
		}
		return "", nil, contracts.Errorf(contracts.ErrConflict, "worktree path %s already exists and is not a worktree of %s on %s", path, top, branch)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", nil, err
	}
	if _, err := git(ctx, top, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err = git(ctx, top, "worktree", "add", path, branch)
		if err != nil {
			return "", nil, err
		}
	} else {
		if _, err := git(ctx, top, "worktree", "add", "-b", branch, path, "HEAD"); err != nil {
			return "", nil, err
		}
	}
	return path, binding, nil
}

func absGit(base, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// pyreBinDir is the directory added to PATH for agent tools.
func (d *Daemon) pyreBinDir() string {
	if d.cfg.PyreBinDir != "" {
		return d.cfg.PyreBinDir
	}
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			exe = r
		}
		return filepath.Dir(stablepath.Of(exe))
	}
	return ""
}

// runEnv builds the Pyre additions to a run's environment. The session
// token travels only in this map (never logged or reported). The tmux
// adapters hand the whole map to the pane process through a private 0600
// file read by the pane launcher (internal/runtime/tmux/execenv.go), so
// neither the token nor any other value here (overlay or connection
// secrets) appears on a command line or in the tmux session environment.
func (d *Daemon) runEnv(base map[string]string, cmd contracts.StartRunCommand, runDir string) map[string]string {
	env := map[string]string{}
	for k, v := range base {
		env[k] = v
	}
	env["PYRE_SESSION_ID"] = cmd.Session.SessionID
	env["PYRE_RUN_ID"] = cmd.RunID
	env["PYRE_NODE_ID"] = d.nodeID
	env["PYRE_SOCKET"] = d.SocketPath()
	env["PYRE_RUN_DIR"] = runDir
	if cmd.Session.ProjectID != nil && *cmd.Session.ProjectID != "" {
		env["PYRE_PROJECT_ID"] = *cmd.Session.ProjectID
	}
	if parent := cmd.Session.Lineage.ParentSessionID; parent != "" { // W-childnotify
		env[agentguide.EnvParentSessionID] = parent
		if slices.Contains(cmd.Session.Tags, contracts.TagReportParent) {
			env[agentguide.EnvReportToParent] = "1"
		}
	}
	if cmd.SessionToken != "" {
		env["PYRE_SESSION_TOKEN"] = cmd.SessionToken
	}
	path := env["PATH"]
	if path == "" {
		path = os.Getenv("PATH")
	}
	if bin := d.pyreBinDir(); bin != "" && !strings.HasPrefix(path, bin+string(os.PathListSeparator)) {
		path = bin + string(os.PathListSeparator) + path
	}
	env["PATH"] = path
	if d.cfg.PyreCLI != "" {
		env["PYRE_CLI"] = d.cfg.PyreCLI
	}
	return env
}

// agentMCPArgs returns the --mcp-config arguments that give a Claude run the
// Pyre agent tools, unless args already carry an --mcp-config (the
// environment overlay registers the same server) or no MCP command is
// configured. The server inherits the run's PYRE_SOCKET/PYRE_SESSION_TOKEN,
// so its tools act as the session. The file holds no secrets.
func (d *Daemon) agentMCPArgs(args []string, runDir string) ([]string, error) {
	if len(d.cfg.AgentMCP) == 0 {
		return nil, nil
	}
	for _, a := range args {
		if a == "--mcp-config" || strings.HasPrefix(a, "--mcp-config=") {
			return nil, nil
		}
	}
	spec := map[string]any{"mcpServers": map[string]any{"pyre": map[string]any{
		"type": "stdio", "command": d.cfg.AgentMCP[0], "args": d.cfg.AgentMCP[1:]}}}
	b, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, err
	}
	p := filepath.Join(runDir, "pyre-mcp.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return nil, err
	}
	// "=" form: --mcp-config is variadic and would otherwise swallow the
	// initial prompt the adapter appends after the args.
	return []string{"--mcp-config=" + p}, nil
}
