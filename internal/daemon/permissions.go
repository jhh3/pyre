package daemon

import (
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Run permission modes (pyred --permissions, pyre start --permissions).
const (
	// PermissionsAsk keeps the harness's own approval prompts (default);
	// Pyre's approval broker relays them.
	PermissionsAsk = "ask"
	// PermissionsBypass runs the harness without approval prompts or
	// sandbox: Claude --dangerously-skip-permissions, Codex
	// approval_policy=never + sandbox_mode=danger-full-access.
	PermissionsBypass = "bypass"
)

// PermissionsArg is the per-launch override carried in StartRunCommand.Args
// ("--pyre-permissions=ask|bypass"). The daemon removes it before the
// adapter sees the args, so it never reaches the harness.
const PermissionsArg = "--pyre-permissions"

// PermissionsEnv tells the adapter the run's permission mode
// (ResolvedLaunch.Env; adapters apply their native mechanism).
const PermissionsEnv = "PYRE_PERMISSIONS"

// adapterDataPermissions records the mode on the run (nonsecret).
const adapterDataPermissions = "pyre.permissions"

// ValidPermissions reports whether p is a known mode.
func ValidPermissions(p string) bool { return p == PermissionsAsk || p == PermissionsBypass }

// splitPermissionsArg removes every PermissionsArg from args and returns the
// last requested mode ("" = none requested).
func splitPermissionsArg(args []string) ([]string, string, error) {
	var out []string
	mode := ""
	for i := 0; i < len(args); i++ {
		name, val, has := strings.Cut(args[i], "=")
		if name != PermissionsArg {
			out = append(out, args[i])
			continue
		}
		if !has {
			if i+1 >= len(args) {
				return nil, "", contracts.Errorf(contracts.ErrInvalid, "%s needs ask or bypass", PermissionsArg)
			}
			i++
			val = args[i]
		}
		if !ValidPermissions(val) {
			return nil, "", contracts.Errorf(contracts.ErrInvalid, "permissions %q: want ask or bypass", val)
		}
		mode = val
	}
	return out, mode, nil
}

// runPermissions decides a run's permission mode: an explicit per-launch
// request, else the mode of the run a resume continues, else the node
// default. Shell runs have no permission prompts and always report ask.
func (d *Daemon) runPermissions(harness contracts.Harness, requested string, prev *contracts.Run) string {
	if harness == contracts.HarnessShell {
		return PermissionsAsk
	}
	if requested != "" {
		return requested
	}
	if prev != nil && ValidPermissions(prev.AdapterData[adapterDataPermissions]) {
		return prev.AdapterData[adapterDataPermissions]
	}
	if ValidPermissions(d.cfg.Permissions) {
		return d.cfg.Permissions
	}
	return PermissionsAsk
}
