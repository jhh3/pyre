package tuitest

import (
	"context"

	"github.com/jhh3/pyre/internal/contracts"
)

// The test wrappers pass search.query through to the wrapped API when it
// offers it (contracts.SearchAPI is not part of contracts.API).

func searchVia(api contracts.API, ctx context.Context, req contracts.SearchRequest) (contracts.SearchResponse, error) {
	if s, ok := api.(contracts.SearchAPI); ok {
		return s.Search(ctx, req)
	}
	return contracts.SearchResponse{}, contracts.Errorf(contracts.ErrUnsupported, "search not offered")
}

func (e *Embeddable) Search(ctx context.Context, req contracts.SearchRequest) (contracts.SearchResponse, error) {
	return searchVia(e.API, ctx, req)
}

func (r *Recorder) Search(ctx context.Context, req contracts.SearchRequest) (contracts.SearchResponse, error) {
	r.rec(contracts.OpSearch, "", req)
	return searchVia(r.API, ctx, req)
}
