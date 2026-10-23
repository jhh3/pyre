package daemon

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/harnessargs"
)

// Extra harness arguments (pyre start --harness-arg / `-- ARGS`, MCP
// start_session harness_args) arrive in StartRunCommand.Args after Pyre's
// own --pyre-* arguments are removed. The node validates them against the
// harness's allowlist (internal/harnessargs) before anything starts and
// records them on the run, so a resume and an account switch (a resume on
// another account) relaunch with the same arguments.

// adapterDataHarnessArgs records a run's extra harness arguments: a JSON
// array in canonical "--flag=value" form (nonsecret; shown in session
// details).
const adapterDataHarnessArgs = "pyre.harness_args"

// runHarnessArgs decides a run's extra harness arguments: the launch's own,
// validated; else, on a resume, those of the run it continues; else none.
// Shell and mock arguments are their command line and pass unchanged.
func runHarnessArgs(harness contracts.Harness, args []string, prev *contracts.Run) ([]string, error) {
	if harnessargs.Passthrough(harness) {
		return args, nil
	}
	if len(args) == 0 && prev != nil && prev.Harness == harness {
		args = recordedHarnessArgs(*prev)
	}
	out, err := harnessargs.Normalize(harness, args)
	if err != nil {
		return nil, err
	}
	for _, a := range out {
		if v, ok := strings.CutPrefix(a, "--settings="); ok {
			if err := checkSessionSettings(v); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// checkSessionSettings refuses a session's --settings file that cannot be
// read or that sets what Pyre controls (harnessargs.SettingsKeysRefused,
// permissions.defaultMode): checked on the node, before anything starts.
func checkSessionSettings(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "--settings %s: %v", path, err)
	}
	if len(b) > 1<<20 {
		return contracts.Errorf(contracts.ErrInvalid, "--settings %s is larger than 1 MiB", path)
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) != nil {
		return contracts.Errorf(contracts.ErrInvalid, "--settings %s is not a JSON object", path)
	}
	for _, k := range harnessargs.SettingsKeysRefused {
		if _, ok := m[k]; ok {
			return contracts.Errorf(contracts.ErrInvalid, "--settings %s sets %q, which Pyre controls (the account binding, permissions or hooks)", path, k)
		}
	}
	var perms struct {
		DefaultMode *string `json:"defaultMode"`
	}
	if raw, ok := m["permissions"]; ok && json.Unmarshal(raw, &perms) == nil && perms.DefaultMode != nil {
		return contracts.Errorf(contracts.ErrInvalid, "--settings %s sets permissions.defaultMode; use --permissions", path)
	}
	return nil
}

// recordedHarnessArgs returns the extra harness arguments recorded on r.
func recordedHarnessArgs(r contracts.Run) []string {
	raw := r.AdapterData[adapterDataHarnessArgs]
	if raw == "" {
		return nil
	}
	var args []string
	if json.Unmarshal([]byte(raw), &args) != nil {
		return nil
	}
	return args
}

// adapterDataPermissionsCustom says which extra arguments can widen what
// the run may do beyond its recorded permission mode (settings that allow
// tools or add hooks, plugins with hooks, agent definitions), so session
// details never present "ask" as the whole story.
const adapterDataPermissionsCustom = "pyre.permissions_custom"

// recordHarnessArgs stores args on the run's adapter data (none: nothing).
func recordHarnessArgs(r *contracts.Run, args []string) {
	if len(args) == 0 || harnessargs.Passthrough(r.Harness) {
		return
	}
	b, _ := json.Marshal(args)
	r.AdapterData[adapterDataHarnessArgs] = string(b)
	var custom []string
	for _, f := range []string{"--settings", "--plugin-dir", "--agents", "--agent", "--mcp-config"} {
		if harnessargs.Has(args, f) {
			custom = append(custom, f)
		}
	}
	if len(custom) > 0 {
		r.AdapterData[adapterDataPermissionsCustom] = strings.Join(custom, " ")
	}
}
