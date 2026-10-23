package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/headless"
)

// Headless runs the caller executes itself (pyre run -- codex exec ...):
// the state machine is package headless; this file supplies Codex's
// CODEX_HOME, thread ids and the rollout reader. No app-server is involved:
// the transcript is read from the rollout file the command wrote.

// ExecAdapterName is the adapter name of headless pyre run runs of Codex.
const ExecAdapterName = "codex-exec"

// ExecAdapter is the headless (pyre run) adapter over a Codex adapter.
type ExecAdapter struct {
	*headless.Adapter
	base *Adapter
}

// NewExec returns the headless Codex adapter sharing base's probe and
// rollout lookup.
func NewExec(base *Adapter) *ExecAdapter {
	x := &ExecAdapter{base: base}
	x.Adapter = headless.New(headless.Spec{Name: ExecAdapterName, Harness: contracts.HarnessCodex, Base: execBase{base}, Child: "codex",
		NativeKind: "codex_thread", Home: execHome, ValidNative: threadIDRe.MatchString,
		FindNative:  func(st headless.State) string { return newRolloutSince(st.Home, st.Cwd, st.CreatedAt) },
		NativeKnown: x.writeMeta, Now: base.o.Now})
	return x
}

// threadIDRe matches a Codex thread id (a UUID).
var threadIDRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// execHome is a launch's CODEX_HOME: its profile, else the machine's own
// login.
func execHome(l contracts.ResolvedLaunch) string {
	if l.ProfileDir != "" {
		return l.ProfileDir
	}
	if h := l.Env["CODEX_HOME"]; h != "" {
		return h
	}
	return codexHome("")
}

// writeMeta writes the run meta the rollout readers use (run.json; no
// app-server socket: a headless run never had one).
func (x *ExecAdapter) writeMeta(runID, dir string, st headless.State, thread string) {
	m := runMeta{RunID: runID, ThreadID: thread, Home: st.Home, Cwd: st.Cwd, LaunchedAt: st.CreatedAt}
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = writeFileAtomic(filepath.Join(dir, "run.json"), b)
}

// execBase is the headless adapter's view of the Codex adapter: its probe
// and resume rules, and the rollout file as the transcript.
type execBase struct{ a *Adapter }

func (b execBase) Probe(ctx context.Context) (contracts.ProbeResult, error) { return b.a.Probe(ctx) }

func (b execBase) TranscriptFiles(h contracts.RunHandle) (string, []string) {
	return b.a.TranscriptFiles(h)
}

func (b execBase) ResumeSpec(ctx context.Context, prev contracts.Run, sameAccount bool) (contracts.ContinuationKind, *contracts.NativeRef, error) {
	return b.a.ResumeSpec(ctx, prev, sameAccount)
}

func (b execBase) ReadTranscript(ctx context.Context, h contracts.RunHandle, req contracts.ReadSessionRequest) (contracts.ReadSessionResponse, error) {
	resp := contracts.ReadSessionResponse{Freshness: contracts.Freshness{ObservedAt: b.a.o.Now(), Source: "node:" + b.a.o.NodeID}}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	var meta runMeta
	raw, err := os.ReadFile(filepath.Join(h.StateDir, "run.json"))
	if err != nil || json.Unmarshal(raw, &meta) != nil || meta.ThreadID == "" {
		resp.Freshness.Stale, resp.Freshness.Reason = true, "no Codex thread recorded for this headless run yet"
		resp.NextCursor = req.Cursor
		return resp, nil
	}
	sel, ok := rolloutTranscript(h.StateDir, meta, h.RunID, req, limit)
	resp.NextCursor = req.Cursor
	if !ok {
		resp.Freshness.Stale, resp.Freshness.Reason = true, "the rollout of thread "+meta.ThreadID+" was not found"
		return resp, nil
	}
	resp.Entries = sel
	if len(sel) > 0 {
		resp.NextCursor = fmt.Sprintf("%d", sel[len(sel)-1].Seq)
	}
	resp.Freshness.Reason = "read from the rollout file"
	return resp, nil
}

// newRolloutSince returns the thread id of the only thread created since t
// for cwd: a rollout under home's day folders (t's day and the day before,
// local time) whose session_meta names cwd and was written at or after t
// ("" when none or several). Only a new thread is looked up this way: a
// resumed run names its thread (ResumeFrom), and an older thread's rollout,
// even one appended to now (the caller's own session in the same folder),
// is never taken for the run's.
func newRolloutSince(home, cwd string, t time.Time) string {
	if home == "" {
		return ""
	}
	found := ""
	for _, d := range []int{0, -1} {
		day := t.Local().AddDate(0, 0, d)
		m, _ := filepath.Glob(filepath.Join(home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"), "rollout-*.jsonl"))
		for _, p := range m {
			fi, err := os.Stat(p)
			if err != nil || fi.ModTime().Before(t) {
				continue
			}
			id, mcwd, created := rolloutMeta(p)
			if id == "" || filepath.Clean(mcwd) != filepath.Clean(cwd) || created.Before(t) {
				continue
			}
			if found != "" && found != id {
				return ""
			}
			found = id
		}
	}
	return found
}

// rolloutMeta reads a rollout's first line (session_meta): the thread id,
// cwd and when the thread was created (zero when unknown).
func rolloutMeta(path string) (id, cwd string, created time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", created
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	line, err := r.ReadSlice('\n')
	if err != nil && len(line) == 0 {
		return "", "", created
	}
	var m struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			ID        string `json:"id"`
			Cwd       string `json:"cwd"`
			Timestamp string `json:"timestamp"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &m) != nil || m.Type != "session_meta" || !threadIDRe.MatchString(m.Payload.ID) {
		return "", "", created
	}
	for _, ts := range []string{m.Payload.Timestamp, m.Timestamp} {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			created = t
			break
		}
	}
	return m.Payload.ID, m.Payload.Cwd, created
}

// ResumeOwner tells where thread sid's rollout lives on this node, for a
// headless resume (pyre run -- codex exec resume ID): shared when an
// account profile reaches it through the accounts' shared sessions store
// (any account works), else the account whose own profile holds it, else
// defaultLogin when only the machine's own CODEX_HOME has it. All zero when
// not found (the run binds as usual).
func (a *Adapter) ResumeOwner(cwd, sid string) (shared bool, accountID string, defaultLogin bool) {
	if !threadIDRe.MatchString(sid) {
		return false, "", false
	}
	def := filepath.Clean(codexHome(""))
	for _, home := range a.transcriptHomes() {
		if !hasRollout(home, sid) {
			continue
		}
		isDef := filepath.Clean(home) == def
		acct := profileAccount(home)
		if fi, err := os.Lstat(filepath.Join(home, "sessions")); !isDef && (acct == "" || err == nil && fi.Mode()&os.ModeSymlink != 0) {
			return true, "", false // the accounts' shared store
		}
		switch {
		case isDef:
			defaultLogin = true
		case accountID == "":
			accountID = acct
		}
	}
	if accountID != "" {
		defaultLogin = false
	}
	return false, accountID, defaultLogin
}

// transcriptHomes are the CODEX_HOMEs searched for a rollout: the machine's
// own login, then Options.TranscriptHomes (globs).
func (a *Adapter) transcriptHomes() []string {
	homes := []string{codexHome("")}
	for _, h := range a.o.TranscriptHomes {
		if m, err := filepath.Glob(h); err == nil && len(m) > 0 {
			homes = append(homes, m...)
		} else {
			homes = append(homes, h)
		}
	}
	return homes
}

// hasRollout reports whether home holds thread sid's rollout (sessions day
// folders or archived_sessions).
func hasRollout(home, sid string) bool {
	if home == "" || !safeThreadID(sid) {
		return false
	}
	name := "rollout-*" + sid + ".jsonl"
	return globFirst([]string{filepath.Join(home, "sessions", "*", "*", "*", name), filepath.Join(home, "archived_sessions", name)}) != ""
}

// profileAccount returns the account id of a profile dir
// (<root>/<acct_...>/codex[-...]), "" otherwise.
func profileAccount(home string) string {
	acct := filepath.Base(filepath.Dir(filepath.Clean(home)))
	if strings.HasPrefix(acct, "acct_") {
		return acct
	}
	return ""
}
