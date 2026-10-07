package mock

import (
	"context"
	"sort"
	"strings"
	"sync"

	c "github.com/jhh3/pyre/internal/contracts"
)

// Account tokens in the mock: the token's presence and dates only. The
// token bytes are dropped on arrival (the mock never needs them).

var _ c.AccountTokenAPI = (*Backend)(nil)

var (
	mockTokMu sync.Mutex
	mockToks  = map[*Backend]map[string]c.AccountTokenStatus{}
)

func (b *Backend) SetAccountToken(ctx context.Context, req c.SetAccountTokenRequest) (c.AccountTokenStatus, error) {
	tok := req.Token
	req.Token = "" // never part of the idempotency digest
	return mutate(b, ctx, c.OpSetAccountToken, req.Mutation, req, func(p c.Principal) (c.AccountTokenStatus, error) {
		if err := requireOwner(p); err != nil {
			return c.AccountTokenStatus{}, err
		}
		a, ok := b.accounts[req.AccountID]
		if !ok {
			return c.AccountTokenStatus{}, notFound(c.KindAccount, req.AccountID)
		}
		if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
			return c.AccountTokenStatus{}, c.Errorf(c.ErrInvalid, "token is required (one line)")
		}
		if a.Account.RotaRef == "" { // as the hub: a rota login keeps its lease
			a.Account.PerMachineIndependent = true
		}
		now := b.now()
		mockTokMu.Lock()
		defer mockTokMu.Unlock()
		m := mockToks[b]
		if m == nil {
			m = map[string]c.AccountTokenStatus{}
			mockToks[b] = m
		}
		st := m[req.AccountID]
		if !st.Set {
			st.AddedAt = now
		}
		st.AccountID, st.Provider, st.Set, st.UpdatedAt, st.Nodes = req.AccountID, a.Account.Provider, true, now, req.Nodes
		m[req.AccountID] = st
		return st, nil
	})
}

func (b *Backend) RemoveAccountToken(ctx context.Context, req c.RemoveAccountTokenRequest) (c.AccountTokenStatus, error) {
	return mutate(b, ctx, c.OpRemoveAccountToken, req.Mutation, req, func(p c.Principal) (c.AccountTokenStatus, error) {
		if err := requireOwner(p); err != nil {
			return c.AccountTokenStatus{}, err
		}
		a, ok := b.accounts[req.AccountID]
		if !ok {
			return c.AccountTokenStatus{}, notFound(c.KindAccount, req.AccountID)
		}
		mockTokMu.Lock()
		delete(mockToks[b], req.AccountID)
		mockTokMu.Unlock()
		return c.AccountTokenStatus{AccountID: req.AccountID, Provider: a.Account.Provider}, nil
	})
}

func (b *Backend) ListAccountTokens(ctx context.Context, req c.ListAccountTokensRequest) ([]c.AccountTokenStatus, error) {
	if _, err := b.principal(ctx); err != nil {
		return nil, err
	}
	mockTokMu.Lock()
	defer mockTokMu.Unlock()
	out := []c.AccountTokenStatus{}
	for id, st := range mockToks[b] {
		if req.AccountID == "" || req.AccountID == id {
			out = append(out, st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}
