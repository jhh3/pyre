package main

import (
	"context"
	"time"

	"github.com/jhh3/pyre/internal/accounts"
	"github.com/jhh3/pyre/internal/contracts"
)

// hubTokens implements accounts.TokenSource over the node-credential hub
// client (accounts.token.fetch). The value is returned to the accounts
// service, which keeps it in memory only.
type hubTokens struct {
	hub contracts.AccountTokenFetcher
}

func (t hubTokens) FetchAccountToken(ctx context.Context, accountID string) (string, time.Time, error) {
	f, err := t.hub.FetchAccountToken(ctx, contracts.FetchAccountTokenRequest{AccountID: accountID})
	if err != nil {
		return "", time.Time{}, err
	}
	return f.Token, f.UpdatedAt, nil
}

// tokenSource returns nil without a hub link (token accounts are then
// never launchable on this node).
func tokenSource(hub contracts.AccountTokenFetcher) accounts.TokenSource {
	if hub == nil {
		return nil
	}
	return hubTokens{hub: hub}
}
