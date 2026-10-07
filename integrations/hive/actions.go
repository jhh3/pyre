package hive

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/jhh3/pyre/internal/contracts"
)

// ActionHandler is the node action handler shape the daemon registers.
type ActionHandler func(ctx context.Context, params json.RawMessage) (any, error)

// GetParams is the memory.get payload ({"id": ...}).
type GetParams struct {
	ID string `json:"id"`
}

// Handlers returns the memory.* node action handlers keyed by action name.
func (c *Client) Handlers() map[string]ActionHandler {
	return map[string]ActionHandler{
		contracts.ActionMemorySearch: c.SearchAction,
		contracts.ActionMemoryGet:    c.GetAction,
		contracts.ActionMemoryStatus: c.StatusAction,
	}
}

func decode(params json.RawMessage, v any) error {
	if len(bytes.TrimSpace(params)) == 0 || string(bytes.TrimSpace(params)) == "null" {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return contracts.Errorf(contracts.ErrInvalid, "invalid params: %v", err)
	}
	return nil
}

// SearchAction handles memory.search: MemorySearchParams -> MemorySearchResult.
func (c *Client) SearchAction(ctx context.Context, params json.RawMessage) (any, error) {
	var p contracts.MemorySearchParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return c.Search(ctx, p)
}

// GetAction handles memory.get: {id} -> MemoryHit.
func (c *Client) GetAction(ctx context.Context, params json.RawMessage) (any, error) {
	var p GetParams
	if err := decode(params, &p); err != nil {
		return nil, err
	}
	return c.Get(ctx, p.ID)
}

// StatusAction handles memory.status: {} -> MemoryStatus. It never fails;
// an absent Hive is reported as backend "unavailable".
func (c *Client) StatusAction(ctx context.Context, _ json.RawMessage) (any, error) {
	return c.Refresh(ctx).Memory, nil
}
