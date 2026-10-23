package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/refs"
)

// Node-started headless runs (W-pyrerun-node): `pyre run` on a machine
// whose CLI has no hub login asks this node to record the run. The node
// starts it in the hub with its own credential, which the hub allows only
// for exactly this shape (hub nodestart.go): a claude or codex session on this node
// with the exec claim argument, tagged pyre run / report:none /
// archive:when-done, never a parent, prompt or report. The CLI then claims
// the binding as for any headless run (PathLocalExecClaim).
//
// Who may ask: the local socket is 0600 in the 0700 state directory, so
// only pyred's own user (and root) can connect; on Linux and macOS this
// endpoint also checks the connecting process's uid (peercred_*.go). That
// user can already read this node's account profiles and its credential,
// so the endpoint grants nothing new: it starts nothing and only lets a
// headless claude or codex the caller runs itself use one of this node's
// accounts.

// PathLocalExecStart records a headless run with the node's credential
// (POST ExecStartRequest -> ExecStartResponse).
const PathLocalExecStart = "/v1/local/exec/start"

// ExecStartRequest asks the node to record a headless run.
type ExecStartRequest struct {
	// ClaimSHA256 is the hex SHA-256 of the claim secret only the caller
	// knows (the run's ExecArg).
	ClaimSHA256 string `json:"claim_sha256"`
	// Resume is the conversation the caller's command resumes (ExecResumeArg).
	Resume string `json:"resume,omitempty"`
	// Harness is claude (the default) or codex.
	Harness contracts.Harness `json:"harness,omitempty"`
	Title   string            `json:"title,omitempty"`
	Cwd     string            `json:"cwd"`
	// Project is a project ID or name, resolved in the hub with the node's
	// (read-only) access; empty: none.
	Project string `json:"project,omitempty"`
	// Account is an account ID, label or rota name; empty: the node's
	// selector picks the best eligible account.
	Account string `json:"account,omitempty"`
}

// ExecStartResponse names the recorded session and run.
type ExecStartResponse struct {
	SessionID string  `json:"session_id"`
	RunID     string  `json:"run_id"`
	ProjectID *string `json:"project_id,omitempty"`
	AccountID string  `json:"account_id,omitempty"`
}

// maxExecTitle bounds a node-started session's title (bytes).
const maxExecTitle = 200

// canStartExec reports whether this node can record headless runs itself.
func (d *Daemon) canStartExec() bool {
	return d.cfg.NodeToken != "" && d.cfg.Upstream != nil && len(d.cfg.ExecAdapters) > 0
}

func (d *Daemon) serveExecStart(w http.ResponseWriter, r *http.Request) {
	if err := checkPeer(r.Context()); err != nil {
		writeLocalErr(w, err)
		return
	}
	var req ExecStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeLocalErr(w, contracts.Errorf(contracts.ErrInvalid, "exec start: %v", err))
		return
	}
	res, err := d.startExec(r.Context(), req)
	if err != nil {
		writeLocalErr(w, err)
		return
	}
	writeLocalJSON(w, http.StatusOK, res)
}

// startExec validates req and records the run in the hub as this node.
func (d *Daemon) startExec(ctx context.Context, req ExecStartRequest) (ExecStartResponse, error) {
	var out ExecStartResponse
	if req.Harness == "" {
		req.Harness = contracts.HarnessClaude
	}
	switch {
	case !d.canStartExec():
		return out, contracts.Errorf(contracts.ErrUnavailable, "this node has no hub credential to record headless runs with (not enrolled)")
	case req.Harness != contracts.HarnessClaude && req.Harness != contracts.HarnessCodex:
		return out, contracts.Errorf(contracts.ErrUnsupported, "headless runs started by the node are claude or codex (got %s)", req.Harness)
	case d.execAdapterFor(req.Harness) == nil:
		return out, contracts.Errorf(contracts.ErrUnsupported, "headless runs of %s are not supported on this node", req.Harness)
	case !hexSHA256(req.ClaimSHA256):
		return out, contracts.Errorf(contracts.ErrInvalid, "claim_sha256 must be a hex SHA-256")
	case req.Cwd == "" || !filepath.IsAbs(req.Cwd):
		return out, contracts.Errorf(contracts.ErrInvalid, "cwd must be an absolute path")
	case len(req.Title) > maxExecTitle || !utf8.ValidString(req.Title):
		return out, contracts.Errorf(contracts.ErrInvalid, "title must be valid UTF-8 of at most %d bytes", maxExecTitle)
	case strings.HasPrefix(req.Resume, "-") || strings.ContainsAny(req.Resume, " \t\r\n="):
		return out, contracts.Errorf(contracts.ErrInvalid, "resume must be a conversation id")
	case d.hubUnreachable():
		return out, contracts.Errorf(contracts.ErrUnavailable, "this node is not connected to the hub")
	}
	api, err := d.upstream(d.cfg.NodeToken)
	if err != nil {
		return out, err
	}
	hctx := d.upstreamCtx(ctx)
	// The CLI's own resolution (internal/refs): the same --project and
	// --account resolve the same way with or without a hub login.
	if out.ProjectID, err = refs.ProjectPtr(hctx, api, req.Project); err != nil {
		return out, err
	}
	if out.ProjectID != nil && *out.ProjectID == "" {
		out.ProjectID = nil // "none": no parent to inherit a project from
	}
	policy := contracts.AccountPolicy{Mode: contracts.AccountEligible}
	if strings.TrimSpace(req.Account) != "" {
		id, err := refs.AccountID(hctx, api, req.Account, contracts.ProviderFor(req.Harness))
		if err != nil {
			return out, err
		}
		policy = contracts.AccountPolicy{Mode: contracts.AccountExplicit, AccountID: id}
	}
	args := []string{ExecArg + "=" + strings.ToLower(req.ClaimSHA256)}
	if req.Resume != "" {
		args = append(args, ExecResumeArg+"="+req.Resume)
	}
	no := false
	res, err := api.StartSession(hctx, contracts.StartSessionRequest{
		Mutation: contracts.Mutation{OperationID: contracts.NewID(contracts.PrefixOperation)},
		NodeID:   d.nodeID, Harness: req.Harness, Title: strings.TrimSpace(req.Title), Cwd: req.Cwd, ProjectID: out.ProjectID,
		AccountPolicy: policy, Args: args, ReportToParent: &no,
		Tags: []string{contracts.TagPyreRun, contracts.TagReportNone, contracts.TagArchiveWhenDone},
	})
	if err != nil {
		return out, err
	}
	if res.Session.Run == nil {
		return out, contracts.Errorf(contracts.ErrUnavailable, "the hub recorded no run")
	}
	out.SessionID, out.RunID, out.AccountID = res.Session.Session.SessionID, res.Session.Run.RunID, policy.AccountID
	d.cfg.Logger.Info("headless run recorded with the node credential", "session", out.SessionID, "run", out.RunID, "project", ptrStr(out.ProjectID))
	return out, nil
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
