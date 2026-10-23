package daemon

import (
	"context"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/envfile"
)

// EnvFileArg names a session's environment file on this node (pyre start
// --env-file PATH, MCP start_session env_file): dotenv KEY=VALUE lines
// (internal/envfile) applied to the harness process. Carried in
// StartRunCommand.Args like PermissionsArg and removed before the adapter
// sees the args. Only the path is recorded on the run (adapterDataEnvFile);
// the node reads the file at every launch, so a resume and an account
// switch (a resume on another account) relaunch with its current values,
// which never leave the node.
//
// On a resume, EnvFileArg=PATH replaces the session's file and
// EnvFileArg=none (EnvFileNone) drops it (pyre resume --env-file).
const EnvFileArg = contracts.StartArgEnvFile

// EnvFileNone on a resume removes the session's environment file.
const EnvFileNone = "none"

// adapterDataEnvFile records the path of a run's environment file (never
// its values).
const adapterDataEnvFile = "pyre.env_file"

// splitEnvFileArg removes every EnvFileArg from args and returns the last
// requested path ("" = none).
func splitEnvFileArg(args []string) ([]string, string, error) {
	var out []string
	path := ""
	for i := 0; i < len(args); i++ {
		name, val, has := strings.Cut(args[i], "=")
		if name != EnvFileArg {
			out = append(out, args[i])
			continue
		}
		if !has {
			if i+1 >= len(args) {
				return nil, "", contracts.Errorf(contracts.ErrInvalid, "%s needs a path", EnvFileArg)
			}
			i++
			val = args[i]
		}
		path = strings.TrimSpace(val)
		if path == "" {
			return nil, "", contracts.Errorf(contracts.ErrInvalid, "%s needs a path", EnvFileArg)
		}
	}
	return out, path, nil
}

// runEnvFile decides a run's environment file: the launch's own (none:
// no file), else, on a resume, the one the run it continues recorded (prev,
// or this node's copy of that run when prev carries no adapter data). It
// reads the file now: a file that is missing, not private to this node's
// user or that sets what Pyre controls refuses the launch before anything
// starts, naming the recorded path and how to recover.
func (d *Daemon) runEnvFile(ctx context.Context, req string, prev *contracts.Run) (string, envfile.Vars, error) {
	path, inherited := req, false
	if req == EnvFileNone {
		if prev == nil {
			return "", envfile.Vars{}, contracts.Errorf(contracts.ErrInvalid, "%s=%s only applies to a resume", EnvFileArg, EnvFileNone)
		}
		return "", envfile.Vars{}, nil
	}
	if path == "" && prev != nil {
		path, inherited = prev.AdapterData[adapterDataEnvFile], true
		if path == "" && len(prev.AdapterData) == 0 && prev.RunID != "" {
			if rec, ok, err := d.st.run(ctx, prev.RunID); err == nil && ok {
				path = rec.Run.AdapterData[adapterDataEnvFile]
			}
		}
	}
	if path == "" {
		return "", envfile.Vars{}, nil
	}
	v, err := envfile.Read(path)
	if err != nil {
		if inherited {
			return "", envfile.Vars{}, contracts.Errorf(contracts.ErrInvalid,
				"the session's env file (recorded on run %s): %v; fix the file, or resume with `pyre resume --env-file PATH` (another file) or `--env-file none` (no file)", prev.RunID, err)
		}
		return "", envfile.Vars{}, contracts.Errorf(contracts.ErrInvalid, "%v", err)
	}
	return path, v, nil
}

// applyEnvFile adds a session's environment file to the launch
// environment. Pyre's own variables (environment profile, connections,
// account binding) win over the file; an unset (an empty auth key) removes
// an inherited variable unless Pyre sets it. It returns the variables
// applied, the unset list to use and the keys Pyre's own values kept.
//
// Names compare with envfile.FoldKey (case-insensitive on Windows), so
// Path in the file never sits beside Pyre's PATH.
func applyEnvFile(env map[string]string, unset []string, v envfile.Vars) (applied map[string]string, outUnset, kept []string) {
	return applyEnvFileFold(env, unset, v, envfile.FoldKey)
}

func applyEnvFileFold(env map[string]string, unset []string, v envfile.Vars, fold func(string) string) (applied map[string]string, outUnset, kept []string) {
	own := make(map[string]bool, len(env))
	for k := range env {
		own[fold(k)] = true
	}
	outUnset = append([]string(nil), unset...)
	unsetting := make(map[string]bool, len(unset))
	for _, k := range unset {
		unsetting[fold(k)] = true
	}
	for k, val := range v.Set {
		if own[fold(k)] {
			kept = append(kept, k)
			continue
		}
		if applied == nil {
			applied = map[string]string{}
		}
		env[k] = val
		applied[k] = val
	}
	for _, k := range v.Unset {
		if own[fold(k)] {
			kept = append(kept, k)
			continue
		}
		if !unsetting[fold(k)] {
			unsetting[fold(k)] = true
			outUnset = append(outUnset, k)
		}
	}
	return applied, outUnset, kept
}
