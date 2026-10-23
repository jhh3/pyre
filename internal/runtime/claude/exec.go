package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/runtime/headless"
)

// Headless runs the caller executes itself (pyre run -- claude -p ...):
// the state machine is package headless; this file supplies Claude's
// config home, session ids and transcript readers.

// ExecAdapterName is the adapter name of headless pyre run runs.
const ExecAdapterName = "claude-exec"

// ExecAdapter is the headless (pyre run) adapter over a Claude adapter.
type ExecAdapter struct {
	*headless.Adapter
	base *Adapter
}

// NewExec returns the headless adapter sharing base's transcript readers.
func NewExec(base *Adapter) *ExecAdapter {
	x := &ExecAdapter{base: base}
	x.Adapter = headless.New(headless.Spec{Name: ExecAdapterName, Harness: contracts.HarnessClaude, Base: base, Child: "claude",
		NativeKind: "claude_session", Home: execHome, ValidNative: uuidRe.MatchString,
		FindNative:  func(st headless.State) string { return newTranscriptSince(st.Home, st.Cwd, st.CreatedAt) },
		NativeKnown: x.writeMeta, Now: base.o.Now})
	return x
}

// execHome is a launch's CLAUDE_CONFIG_DIR: its profile, else the
// machine's own login.
func execHome(l contracts.ResolvedLaunch) string {
	if l.ProfileDir != "" {
		return l.ProfileDir
	}
	if cfg := l.Env["CLAUDE_CONFIG_DIR"]; cfg != "" {
		return cfg
	}
	return defaultConfigDir()
}

// writeMeta writes the Claude run meta the transcript readers use.
func (x *ExecAdapter) writeMeta(runID, dir string, st headless.State, sid string) {
	m := runMeta{RunID: runID, SessionUUID: sid, ConfigDir: st.Home, ConfigMech: "claude_config_dir", Cwd: st.Cwd,
		Resumed: st.ResumeFrom != "", LaunchedAt: st.CreatedAt}
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = writeFileAtomic(filepath.Join(dir, fileRun), b)
	x.base.mu.Lock()
	delete(x.base.runs, runID) // reload the meta on the next read
	x.base.mu.Unlock()
}

// newTranscriptSince returns the session id of the only transcript in the
// cwd's project directory written since t ("" when none or several: a
// shared store may hold other runs' transcripts from the same directory).
func newTranscriptSince(configDir, cwd string, t time.Time) string {
	dir := filepath.Dir(guessTranscriptPath(configDir, cwd, "x"))
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range ents {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if !ok || !uuidRe.MatchString(id) {
			continue
		}
		if fi, err := e.Info(); err == nil && !fi.ModTime().Before(t) {
			if found != "" {
				return ""
			}
			found = id
		}
	}
	return found
}

// ConversationOwner tells where conversation sid lives on this node, for a
// headless resume (pyre run -- claude --resume ID): Shared when an account
// profile reaches it through the shared transcript store (any account
// works), else the account whose own profile holds it, else Default when
// only the default login (~/.claude) has it. Zero when not found.
type ConversationOwner struct {
	Shared    bool
	AccountID string
	Default   bool
}

// ConversationOwner searches the configured homes for sid.
func (a *Adapter) ConversationOwner(cwd, sid string) ConversationOwner {
	if !uuidRe.MatchString(sid) {
		return ConversationOwner{}
	}
	var own ConversationOwner
	def := defaultConfigDir()
	for _, home := range a.transcriptHomes() {
		if findTranscript([]string{home}, cwd, sid) == "" {
			continue
		}
		isDef := filepath.Clean(home) == filepath.Clean(def)
		acct := profileAccount(home)
		if fi, err := os.Lstat(filepath.Join(home, "projects")); !isDef && (acct == "" || err == nil && fi.Mode()&os.ModeSymlink != 0) {
			return ConversationOwner{Shared: true} // the accounts' shared store
		}
		switch {
		case isDef:
			own.Default = true
		case own.AccountID == "":
			own.AccountID = acct
		}
	}
	if own.AccountID != "" {
		own.Default = false
	}
	return own
}

// ResumeOwner is ConversationOwner for the daemon (headless resumes).
func (a *Adapter) ResumeOwner(cwd, sid string) (shared bool, accountID string, defaultLogin bool) {
	o := a.ConversationOwner(cwd, sid)
	return o.Shared, o.AccountID, o.Default
}

// profileAccount returns the account id of a profile dir
// (<root>/<acct_...>/claude[-token]), "" otherwise.
func profileAccount(home string) string {
	acct := filepath.Base(filepath.Dir(filepath.Clean(home)))
	if strings.HasPrefix(acct, "acct_") {
		return acct
	}
	return ""
}
