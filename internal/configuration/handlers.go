package configuration

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jhh3/pyre/internal/contracts"
	"github.com/jhh3/pyre/internal/extensions"
)

// Handler is the node action handler signature used by the daemon's
// CmdAction dispatcher.
type Handler func(ctx context.Context, params json.RawMessage) (any, error)

// Handlers returns the env.* node action handlers.
func (m *Manager) Handlers() map[string]Handler {
	return map[string]Handler{
		contracts.ActionEnvPlan: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p contracts.EnvPlanParams
			if err := decodeParams(raw, &p); err != nil {
				return nil, err
			}
			return m.Plan(ctx, p)
		},
		contracts.ActionEnvApply: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p contracts.EnvApplyParams
			if err := decodeParams(raw, &p); err != nil {
				return nil, err
			}
			return m.Apply(ctx, p)
		},
		contracts.ActionEnvDrift: func(ctx context.Context, raw json.RawMessage) (any, error) {
			return m.Drift(ctx)
		},
		contracts.ActionEnvRollback: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p contracts.EnvRollbackParams
			if err := decodeParams(raw, &p); err != nil {
				return nil, err
			}
			return m.Rollback(ctx, p)
		},
		contracts.ActionEnvResolve: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p contracts.EnvPlanParams
			if err := decodeParams(raw, &p); err != nil {
				return nil, err
			}
			return m.Preview(ctx, p)
		},
		contracts.ActionEnvProfileSet: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p ProfileSetParams
			if err := decodeParams(raw, &p); err != nil {
				return nil, err
			}
			return m.SetProfile(ctx, p)
		},
		contracts.ActionEnvProfileGet: func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p ProfileGetParams
			if err := decodeParams(raw, &p); err != nil {
				return nil, err
			}
			return m.GetProfile(ctx, p)
		},
	}
}

// ProfileSetParams is the env.profile.set request: the desired managed
// profile for the global scope, or for a project when ProjectID is set.
// Profiles never carry secrets (Validate refuses credential-like values).
type ProfileSetParams struct {
	ProjectID string  `json:"project_id,omitempty"`
	Profile   Profile `json:"profile"`
}

// ProfileSetResult reports the stored desired profile and the plan that
// would apply it. The desired profile is pending: new runs and config homes
// change only when that plan (or a later one) is applied with env.apply.
type ProfileSetResult struct {
	Scope         string               `json:"scope"`
	Name          string               `json:"name"`
	ProfileDigest contracts.Digest     `json:"profile_digest"`
	Plan          contracts.ChangePlan `json:"plan"`
}

// ProfileGetParams selects a profile ("" name = "default").
type ProfileGetParams struct {
	ProjectID string `json:"project_id,omitempty"`
	Name      string `json:"name,omitempty"`
}

// ProfileGetResult shows the active profile (what new runs use), a pending
// desired profile if one is waiting for apply, and the scope's current
// applied generation.
type ProfileGetResult struct {
	Scope             string           `json:"scope"`
	Name              string           `json:"name"`
	Present           bool             `json:"present"` // an active profile exists
	Profile           *Profile         `json:"profile,omitempty"`
	ProfileDigest     contracts.Digest `json:"profile_digest,omitempty"`
	Pending           bool             `json:"pending"`
	Desired           *Profile         `json:"desired,omitempty"`
	DesiredDigest     contracts.Digest `json:"desired_digest,omitempty"`
	AppliedGeneration string           `json:"applied_generation,omitempty"`
}

func profileScope(projectID string) string {
	if projectID == "" {
		return "global"
	}
	return "project:" + projectID
}

func profileDigest(p *Profile) contracts.Digest {
	if p == nil {
		return ""
	}
	return extensions.DigestBytes(prettyJSON(p))
}

// SetProfile validates and stores the desired (pending) profile, then plans
// it. Running and new runs are unaffected until the plan is applied.
func (m *Manager) SetProfile(ctx context.Context, p ProfileSetParams) (ProfileSetResult, error) {
	if p.Profile.Name == "" {
		p.Profile.Name = "default"
	}
	m.mu.Lock()
	err := m.desired.Save(p.ProjectID, p.Profile)
	m.mu.Unlock()
	if err != nil {
		return ProfileSetResult{}, err
	}
	stored, err := m.desired.Load(p.ProjectID, p.Profile.Name)
	if err != nil {
		return ProfileSetResult{}, err
	}
	scope := profileScope(p.ProjectID)
	plan, err := m.Plan(ctx, contracts.EnvPlanParams{Scope: scope, Profile: p.Profile.Name})
	if err != nil {
		return ProfileSetResult{}, err
	}
	return ProfileSetResult{Scope: scope, Name: p.Profile.Name, ProfileDigest: profileDigest(stored), Plan: plan}, nil
}

// GetProfile returns the active and pending profiles of a scope.
func (m *Manager) GetProfile(ctx context.Context, p ProfileGetParams) (ProfileGetResult, error) {
	name := orDefault(p.Name, "default")
	m.mu.Lock()
	defer m.mu.Unlock()
	active, err := m.profiles.Load(p.ProjectID, name)
	if err != nil {
		return ProfileGetResult{}, err
	}
	desired, err := m.desired.Load(p.ProjectID, name)
	if err != nil {
		return ProfileGetResult{}, err
	}
	res := ProfileGetResult{Scope: profileScope(p.ProjectID), Name: name, Present: active != nil, Profile: active, ProfileDigest: profileDigest(active)}
	if desired != nil && profileDigest(desired) != res.ProfileDigest {
		res.Pending, res.Desired, res.DesiredDigest = true, desired, profileDigest(desired)
	}
	if rec := m.currentRecordFor(res.Scope); rec != nil {
		res.AppliedGeneration = rec.Generation.GenerationID
	}
	return res, nil
}

// decodeParams strictly decodes action params; empty input means {}.
func decodeParams(raw json.RawMessage, v any) error {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "invalid params: %v", err)
	}
	return nil
}

// AdoptionPreview inventories the default homes (plus a project's dirs) and
// reports what adoption would reference. Nothing is moved or written.
func (m *Manager) AdoptionPreview(ctx context.Context, projectID string) (extensions.Preview, error) {
	h := m.opt.Homes
	if projectID != "" && m.opt.ProjectDirs != nil {
		h.ProjectDirs = m.opt.ProjectDirs(ctx, projectID)
	}
	inv, err := extensions.Scan(ctx, h, extensions.ScanOptions{Managed: m.opt.Managed})
	if err != nil {
		return extensions.Preview{}, err
	}
	return extensions.AdoptionPreview(inv), nil
}
