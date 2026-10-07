package credentials

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// LaunchGrant asks the launch wrapper to place one provisioned credential in a
// run's environment under EnvVar. It carries a reference, not a value.
type LaunchGrant struct {
	ConnectionID string `json:"connection_id"`
	EnvVar       string `json:"env_var"`
	Ref          Ref    `json:"ref"`
}

// ManifestEntry describes one provisioned variable by NAME. Manifests are safe
// to persist with a run, show to the owner, and include in diagnostics.
type ManifestEntry struct {
	ConnectionID string `json:"connection_id"`
	EnvVar       string `json:"env_var"`
	Ref          string `json:"ref"`
	Backend      string `json:"backend"`
}

// Manifest lists what a launch provisioned, names only.
type Manifest struct {
	Entries []ManifestEntry `json:"entries"`
}

// EnvNames returns the provisioned variable names.
func (m Manifest) EnvNames() []string {
	out := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.EnvVar)
	}
	sort.Strings(out)
	return out
}

var envName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

// reservedEnv may never be overwritten by a connection grant.
var reservedEnv = map[string]bool{"PATH": true, "HOME": true, "SHELL": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "PWD": true, "TERM": true, "IFS": true}

// ValidateEnvName enforces a conservative environment variable name.
func ValidateEnvName(name string) error {
	if !envName.MatchString(name) {
		return fmt.Errorf("credentials: invalid environment variable name %q", name)
	}
	if reservedEnv[name] || strings.HasPrefix(name, "PYRE_") || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return fmt.Errorf("credentials: environment variable %q is reserved", name)
	}
	return nil
}

// Resolver turns launch grants into environment values at launch time. The
// values exist only in the returned map (destined for ResolvedLaunch.Env,
// which is never serialized); the Manifest carries names only.
type Resolver struct {
	Stores Stores
}

// Resolve reads every grant's value. It fails closed: any invalid, missing,
// locked, or unavailable credential returns an error naming the connection,
// variable, and reference (never a value) and no environment at all.
func (r *Resolver) Resolve(ctx context.Context, grants []LaunchGrant) (map[string]string, Manifest, error) {
	env := make(map[string]string, len(grants))
	var m Manifest
	seen := map[string]string{}
	for _, g := range grants {
		if err := ValidateEnvName(g.EnvVar); err != nil {
			return nil, Manifest{}, fmt.Errorf("credentials: connection %s: %w", g.ConnectionID, err)
		}
		if other, dup := seen[g.EnvVar]; dup {
			return nil, Manifest{}, fmt.Errorf("credentials: connections %s and %s both provision %s; choose distinct variable names", other, g.ConnectionID, g.EnvVar)
		}
		seen[g.EnvVar] = g.ConnectionID
		st, err := r.Stores.For(g.Ref)
		if err != nil {
			return nil, Manifest{}, fmt.Errorf("credentials: connection %s variable %s: %w", g.ConnectionID, g.EnvVar, err)
		}
		v, err := st.Get(ctx, g.Ref)
		if err != nil {
			return nil, Manifest{}, fmt.Errorf("credentials: connection %s variable %s: %w", g.ConnectionID, g.EnvVar, err)
		}
		env[g.EnvVar] = v.Reveal()
		m.Entries = append(m.Entries, ManifestEntry{ConnectionID: g.ConnectionID, EnvVar: g.EnvVar, Ref: g.Ref.String(), Backend: g.Ref.Backend})
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].EnvVar < m.Entries[j].EnvVar })
	return env, m, nil
}

// GrantsFor builds launch grants for connectionIDs from the provisioning
// index. Connections without a local provision are returned in missing.
func GrantsFor(ix *Index, connectionIDs []string) (grants []LaunchGrant, missing []string, err error) {
	for _, id := range connectionIDs {
		ps, err := ix.ForConnection(id)
		if err != nil {
			return nil, nil, err
		}
		if len(ps) == 0 {
			missing = append(missing, id)
			continue
		}
		for _, p := range ps {
			ref, err := p.ParsedRef()
			if err != nil {
				return nil, nil, fmt.Errorf("credentials: connection %s: %w", id, err)
			}
			grants = append(grants, LaunchGrant{ConnectionID: id, EnvVar: p.EnvVar, Ref: ref})
		}
	}
	return grants, missing, nil
}
