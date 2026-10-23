package daemon

import (
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
)

// Per-launch model selection (pyre start --model / --effort, MCP
// start_session model / effort), carried in StartRunCommand.Args like
// PermissionsArg and removed before the adapter sees the args. The adapter
// receives them as ResolvedLaunch.Model/Effort and applies its harness's
// native mechanism. Nothing requested: the harness's own default, exactly
// as before.
const (
	ModelArg  = "--pyre-model"
	EffortArg = "--pyre-effort"
)

// Run AdapterData keys recording the chosen model and effort (nonsecret;
// shown in session details, carried over by resume).
const (
	adapterDataModel  = "pyre.model"
	adapterDataEffort = "pyre.effort"
)

// splitModelArgs removes every ModelArg/EffortArg from args and returns
// the last requested values ("" = none requested).
func splitModelArgs(args []string) (rest []string, model, effort string, err error) {
	for i := 0; i < len(args); i++ {
		name, val, has := strings.Cut(args[i], "=")
		if name != ModelArg && name != EffortArg {
			rest = append(rest, args[i])
			continue
		}
		if !has {
			if i+1 >= len(args) {
				return nil, "", "", contracts.Errorf(contracts.ErrInvalid, "%s needs a value", name)
			}
			i++
			val = args[i]
		}
		val = strings.TrimSpace(val)
		if name == ModelArg {
			if !contracts.ValidModelName(val) {
				return nil, "", "", contracts.Errorf(contracts.ErrInvalid, "model %q is not a model name", val)
			}
			model = val
		} else {
			if !contracts.ValidEffort(val) {
				return nil, "", "", contracts.Errorf(contracts.ErrInvalid, "effort %q is not an effort level (low, medium, high, ...)", val)
			}
			effort = val
		}
	}
	return rest, model, effort, nil
}

// runModel decides a run's model and effort: an explicit per-launch
// request, else what the run a resume continues used (a resume keeps the
// model), else none (the harness default). Shell runs have no model.
func runModel(harness contracts.Harness, model, effort string, prev *contracts.Run) (string, string) {
	if harness == contracts.HarnessShell {
		return "", ""
	}
	if prev != nil && prev.Harness == harness {
		if model == "" {
			model = prev.AdapterData[adapterDataModel]
		}
		if effort == "" {
			effort = prev.AdapterData[adapterDataEffort]
		}
	}
	return model, effort
}
