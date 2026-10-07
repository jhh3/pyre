package accounts

import (
	"context"
	"encoding/json"

	"github.com/jhh3/pyre/internal/contracts"
)

// ActionHandler is the node action handler shape used by the daemon.
type ActionHandler func(ctx context.Context, params json.RawMessage) (any, error)

// Handlers returns the account.* node actions. account.inject_limit is
// registered always but refuses outside test mode.
func (s *Service) Handlers() map[string]ActionHandler {
	m := map[string]ActionHandler{
		contracts.ActionAccountList:        s.handleList,
		contracts.ActionAccountSelect:      s.handleSelect,
		contracts.ActionAccountObserve:     s.handleObserve,
		contracts.ActionAccountInjectLimit: s.handleInjectLimit,
		ActionAccountRegister: func(ctx context.Context, params json.RawMessage) (any, error) {
			var p RegisterParams
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return s.Register(ctx, p)
		},
		ActionAccountCheckouts: func(ctx context.Context, _ json.RawMessage) (any, error) {
			return s.Checkouts(ctx)
		},
		ActionAccountCheckin: func(ctx context.Context, params json.RawMessage) (any, error) {
			var p struct {
				AccountID string `json:"account_id"`
			}
			if err := decode(params, &p); err != nil {
				return nil, err
			}
			return s.Checkin(ctx, p.AccountID)
		},
	}
	for k, h := range s.deviceLoginHandlers() { // devlogin.go
		m[k] = h
	}
	return m
}

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "invalid params: %v", err)
	}
	return nil
}

func (s *Service) handleList(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.ListAccountsRequest
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return s.List(ctx, p.Provider)
}

func (s *Service) handleSelect(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.AccountSelectParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return s.Select(ctx, p)
}

func (s *Service) handleObserve(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Provider contracts.Provider `json:"provider,omitempty"`
		Force    bool               `json:"force,omitempty"` // probe token accounts now
	}
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	observe := s.ObserveDue
	if p.Force {
		observe = s.ObserveNow
	}
	obs := observe(ctx, p.Provider)
	if obs == nil {
		obs = []contracts.AccountObservation{}
	}
	return obs, nil
}

func (s *Service) handleInjectLimit(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.InjectLimitParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return s.InjectLimit(ctx, p)
}

// List returns the node-local account view.
func (s *Service) List(ctx context.Context, provider contracts.Provider) ([]contracts.AccountView, error) {
	accts, err := listAccounts(ctx, s.db, provider)
	if err != nil {
		return nil, err
	}
	out := make([]contracts.AccountView, 0, len(accts))
	for _, a := range accts {
		v := contracts.AccountView{Account: a, Grants: []contracts.DeviceGrant{}, Assignments: []contracts.AccountAssignment{}}
		if v.Latest, err = getObservation(ctx, s.db, a.AccountID); err != nil {
			return nil, err
		}
		gs, err := s.listGrants(ctx, s.db, a.AccountID)
		if err != nil {
			return nil, err
		}
		for _, g := range gs {
			v.Grants = append(v.Grants, g.DeviceGrant)
		}
		as, err := s.activeAssignments(ctx, s.db, a.AccountID)
		if err != nil {
			return nil, err
		}
		for _, x := range as {
			v.Assignments = append(v.Assignments, x.AccountAssignment)
		}
		out = append(out, v)
	}
	return out, nil
}
