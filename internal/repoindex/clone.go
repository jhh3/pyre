package repoindex

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
)

// Clone-on-demand (W-newsession): a machine that lacks a chosen repository
// clones it into its primary repo root, laid out like the repositories
// already there. An existing directory is never overwritten: the same
// repository there is reused, anything else is refused.

// ReadRepo reads a main checkout's metadata (ok is false when path is not
// a main checkout).
func ReadRepo(path string) (contracts.Repo, bool) {
	if st, err := os.Stat(filepath.Join(path, ".git")); err != nil || !st.IsDir() {
		return contracts.Repo{}, false
	}
	return readRepo(path)
}

// GitHubRemote turns "owner/name" into its https clone URL; anything else
// is returned unchanged.
func GitHubRemote(spec string) string {
	if IsGitHubSpec(spec) {
		return "https://github.com/" + strings.TrimSuffix(spec, ".git") + ".git"
	}
	return spec
}

// CloneDest picks where a machine clones remote: the root holding the
// most repositories (else the first existing root, else the first root);
// <root>/<owner>/<name> when most of that root's hosted repositories sit
// in a directory named after their owner, else <root>/<name>.
func CloneDest(idx contracts.NodeRepoIndex, roots []string, remote string) (string, error) {
	remote = GitHubRemote(remote)
	full := FullNameOf(remote)
	name := strings.TrimSuffix(filepath.Base(strings.TrimSuffix(NormalizeRemote(remote), "/")), ".git")
	if full != "" {
		_, name, _ = strings.Cut(full, "/")
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || !strings.Contains(NormalizeRemote(remote), "/") {
		return "", contracts.Errorf(contracts.ErrInvalid, "cannot tell the repository name from %q", remote)
	}
	cands := idx.Roots
	if len(cands) == 0 {
		cands = roots
	}
	count := map[string]int{}
	for _, r := range idx.Repos {
		for _, root := range cands {
			root = filepath.Clean(ExpandHome(root))
			if strings.HasPrefix(r.Path, root+string(filepath.Separator)) {
				count[root]++
				break
			}
		}
	}
	root, best := "", 0
	for _, c := range cands {
		c = filepath.Clean(ExpandHome(c))
		if count[c] > best {
			root, best = c, count[c]
		}
	}
	all := append(append([]string{}, cands...), roots...)
	for _, c := range all {
		if root != "" {
			break
		}
		c = filepath.Clean(ExpandHome(c))
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			root = c
		}
	}
	if root == "" && len(all) > 0 {
		root = filepath.Clean(ExpandHome(all[0]))
	}
	if root == "" || !filepath.IsAbs(root) {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, "code")
	}
	// Layout: owner directories, or flat?
	byOwner, flat := 0, 0
	for _, r := range idx.Repos {
		rel, err := filepath.Rel(root, r.Path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		fn := FullNameOf(r.RemoteURL)
		switch {
		case len(parts) == 1:
			flat++
		case len(parts) == 2 && fn != "" && strings.EqualFold(strings.Split(fn, "/")[0], parts[0]):
			byOwner++
		}
	}
	if byOwner > flat && full != "" {
		owner, _, _ := strings.Cut(full, "/")
		return filepath.Join(root, owner, name), nil
	}
	return filepath.Join(root, name), nil
}

// CloneOptions configure Clone on a node.
type CloneOptions struct {
	Index contracts.NodeRepoIndex // the node's current index (layout, URL style)
	Roots []string                // configured roots (when the index has none)
	GH    string                  // gh CLI path ("" = git only)
	Now   func() time.Time
}

// Clone clones p.Remote on this machine. It never overwrites: a
// destination holding the same repository is returned as is (Cloned
// false), any other existing destination is refused. The clone goes to a
// hidden sibling first and is renamed into place, so a failure leaves
// nothing behind.
func Clone(ctx context.Context, p contracts.RepoCloneParams, o CloneOptions) (contracts.RepoCloneResult, error) {
	remote := strings.TrimSpace(p.Remote)
	if remote == "" || strings.HasPrefix(remote, "-") {
		return contracts.RepoCloneResult{}, contracts.Errorf(contracts.ErrInvalid, "repo.clone needs a remote (clone URL or owner/name)")
	}
	if IsPathSpec(remote) {
		return contracts.RepoCloneResult{}, contracts.Errorf(contracts.ErrInvalid, "clone a remote, not a local path (%s)", remote)
	}
	remote = GitHubRemote(remote)
	remote = StripCredentials(remote)
	home, _ := os.UserHomeDir()
	dest := strings.TrimSpace(p.Dest)
	if dest == "" {
		d, err := CloneDest(o.Index, o.Roots, remote)
		if err != nil {
			return contracts.RepoCloneResult{}, err
		}
		dest = d
	} else {
		dest = filepath.Clean(ExpandHome(dest))
		if !filepath.IsAbs(dest) || home == "" || !strings.HasPrefix(dest, filepath.Clean(home)+string(filepath.Separator)) {
			return contracts.RepoCloneResult{}, contracts.Errorf(contracts.ErrInvalid, "clone destination %s must be inside this machine's home", dest)
		}
	}
	res := contracts.RepoCloneResult{Path: dest, Remote: remote}
	want := NormalizeRemote(remote)
	if st, err := os.Lstat(dest); err == nil {
		if !st.IsDir() {
			return res, contracts.Errorf(contracts.ErrConflict, "%s already exists and is not a directory; nothing was changed", dest)
		}
		r, ok := ReadRepo(dest)
		switch {
		case ok && NormalizeRemote(r.RemoteURL) == want:
			res.Repo = r
			return res, nil // already here: reuse
		case ok && r.RemoteURL != "":
			return res, contracts.Errorf(contracts.ErrConflict, "%s already exists and is a checkout of %s, not %s; nothing was changed (move it, or clone elsewhere)", dest, r.RemoteURL, remote)
		case ok:
			return res, contracts.Errorf(contracts.ErrConflict, "%s already exists and is a git repository without a remote; nothing was changed", dest)
		}
		return res, contracts.Errorf(contracts.ErrConflict, "%s already exists and is not a checkout of %s; nothing was changed", dest, remote)
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return res, contracts.Errorf(contracts.ErrInvalid, "create %s: %v", parent, err)
	}
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	tmp := filepath.Join(parent, ".pyre-clone-"+filepath.Base(dest)+"-"+hex.EncodeToString(rb[:]))
	defer os.RemoveAll(tmp) // gone after the rename; cleans up a failure
	if err := runClone(ctx, o, remote, tmp); err != nil {
		return res, err
	}
	if _, err := os.Lstat(dest); err == nil {
		return res, contracts.Errorf(contracts.ErrConflict, "%s appeared while cloning; nothing was changed", dest)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return res, contracts.Errorf(contracts.ErrInternal, "move the clone into %s: %v", dest, err)
	}
	r, ok := ReadRepo(dest)
	if !ok {
		return res, contracts.Errorf(contracts.ErrInternal, "cloned %s but cannot read it", dest)
	}
	res.Repo, res.Cloned = r, true
	return res, nil
}

// runClone clones with the machine's own git credentials (ssh agent, git
// credential helpers, or gh). A GitHub repository, in whatever form the
// remote names it (another machine's ssh alias included), is cloned the
// way this machine reaches GitHub: ssh when its other GitHub checkouts
// do, else gh when it is installed (its own git_protocol), else https
// (gh's credential helper or any other). The remote as given is used
// only for other hosts.
func runClone(ctx context.Context, o CloneOptions, remote, dir string) error {
	name, args := cloneCommand(o, remote, dir)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "LC_ALL=C")
	cmd.Stdin = nil
	var errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &errb, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if len(msg) > 600 {
			msg = msg[len(msg)-600:]
		}
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() != nil {
			return contracts.Errorf(contracts.ErrUnavailable, "clone of %s did not finish in time: %s", remote, msg)
		}
		return contracts.Errorf(contracts.ErrUnavailable, "clone of %s failed (this machine's git credentials: ssh key, credential helper or gh auth): %s", remote, msg)
	}
	return nil
}

// cloneCommand is the command runClone runs (see there for the choice).
func cloneCommand(o CloneOptions, remote, dir string) (string, []string) {
	full := ""
	if strings.HasPrefix(NormalizeRemote(remote), "github.com/") {
		full = FullNameOf(remote)
	}
	switch {
	case full != "" && prefersSSH(o.Index):
		return "git", []string{"clone", "--quiet", "--", "git@github.com:" + full + ".git", dir}
	case full != "" && o.GH != "":
		return o.GH, []string{"repo", "clone", full, dir, "--", "--quiet"}
	case full != "":
		return "git", []string{"clone", "--quiet", "--", "https://github.com/" + full + ".git", dir}
	}
	return "git", []string{"clone", "--quiet", "--", remote, dir}
}

// prefersSSH: most of the machine's GitHub checkouts use ssh remotes.
func prefersSSH(idx contracts.NodeRepoIndex) bool {
	ssh, https := 0, 0
	for _, r := range idx.Repos {
		if !strings.HasPrefix(NormalizeRemote(r.RemoteURL), "github.com/") {
			continue
		}
		if strings.HasPrefix(r.RemoteURL, "https://") {
			https++
		} else {
			ssh++
		}
	}
	return ssh > https
}

// ActionInvoker is the part of contracts.API CloneVia needs.
type ActionInvoker interface {
	InvokeNodeAction(ctx context.Context, req contracts.NodeActionRequest) (contracts.NodeActionResult, error)
}

// CloneVia runs repo.clone on a node through the hub and waits for it
// (bounded by ctx): a clone outlives one request, so a pending or
// delivered outcome is asked again with the same operation id, which
// never starts a second clone.
func CloneVia(ctx context.Context, api ActionInvoker, nodeID string, p contracts.RepoCloneParams, opID string) (contracts.RepoCloneResult, error) {
	var out contracts.RepoCloneResult
	raw, _ := json.Marshal(p)
	for {
		res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: opID},
			NodeID: nodeID, Action: contracts.ActionRepoClone, Params: raw, TimeoutSeconds: 60})
		if err != nil {
			if contracts.IsCode(err, contracts.ErrNotFound) && strings.Contains(err.Error(), "unknown action") {
				return out, contracts.Errorf(contracts.ErrUnsupported, "this machine's pyred cannot clone repositories yet (upgrade it)")
			}
			return out, err
		}
		if res.Error != nil {
			return out, res.Error
		}
		switch res.Outcome {
		case contracts.OutcomeSucceeded:
			err := json.Unmarshal(res.Result, &out)
			return out, err
		case contracts.OutcomePending, contracts.OutcomeDelivered:
			select {
			case <-ctx.Done():
				return out, contracts.Errorf(contracts.ErrOutcomeIndeterminate, "clone still running on the machine (command %s)", res.CommandID)
			case <-time.After(time.Second):
			}
			continue
		case contracts.OutcomeUnsupported:
			return out, contracts.Errorf(contracts.ErrUnsupported, "this machine's pyred cannot clone repositories yet (upgrade it)")
		}
		return out, contracts.Errorf(contracts.ErrInternal, "repo.clone: %s", res.Outcome)
	}
}

// CreateVia runs worktree.create on a node through the hub and waits for
// it (bounded by ctx). It fetches origin first, so like CloneVia a pending
// or delivered outcome is asked again with the same operation id (an older
// pyred answers at once).
func CreateVia(ctx context.Context, api ActionInvoker, nodeID string, p contracts.WorktreeCreateParams, opID string) (contracts.WorktreeCreateResult, error) {
	var out contracts.WorktreeCreateResult
	raw, _ := json.Marshal(p)
	for {
		res, err := api.InvokeNodeAction(ctx, contracts.NodeActionRequest{Mutation: contracts.Mutation{OperationID: opID},
			NodeID: nodeID, Action: contracts.ActionWorktreeCreate, Params: raw, TimeoutSeconds: 60})
		if err != nil {
			return out, err
		}
		if res.Error != nil {
			return out, res.Error
		}
		switch res.Outcome {
		case contracts.OutcomeSucceeded:
			err := json.Unmarshal(res.Result, &out)
			return out, err
		case contracts.OutcomePending, contracts.OutcomeDelivered:
			select {
			case <-ctx.Done():
				return out, contracts.Errorf(contracts.ErrOutcomeIndeterminate, "worktree creation still running on the machine (command %s)", res.CommandID)
			case <-time.After(time.Second):
			}
			continue
		case contracts.OutcomeUnsupported:
			return out, contracts.Errorf(contracts.ErrUnsupported, "worktree.create is not supported on this machine")
		}
		return out, contracts.Errorf(contracts.ErrInternal, "worktree.create: %s", res.Outcome)
	}
}

// DescribeClone is a one-line summary for logs and confirmations.
func DescribeClone(remote, node string) string {
	name := FullNameOf(remote)
	if name == "" {
		name = remote
	}
	return fmt.Sprintf("cloning %s on %s (not there yet)", name, node)
}
