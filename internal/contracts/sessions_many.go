package contracts

import "context"

// OpGetManySessions (W-delta, additive, optional): fetch several sessions
// by id in one call, so a client that learned from events which sessions
// changed can refresh just those rows instead of re-listing everything.
// Same auth and visibility as sessions.get. Archived sessions are returned
// (the client decides whether to show them); unknown ids are reported in
// Missing. At most MaxGetManySessions ids per call (more is invalid).
// Not part of API: callers type-assert SessionBatchAPI; a hub without it
// answers 404 (the client reports ErrUnsupported) and does not advertise
// the hub feature FeatureSessionsGetMany in Hello.
const OpGetManySessions = "sessions.get_many"

// FeatureSessionsGetMany is the Hello feature advertising OpGetManySessions.
const FeatureSessionsGetMany = "sessions.get_many"

// MaxGetManySessions bounds GetManySessionsRequest.IDs.
const MaxGetManySessions = 200

type GetManySessionsRequest struct {
	IDs []string `json:"ids"`
}

type GetManySessionsResponse struct {
	Items   []SessionView `json:"items"`
	Missing []string      `json:"missing"`
}

// SessionBatchAPI is implemented by hubs (and the client, the node-local
// proxy and the mock) that offer OpGetManySessions.
type SessionBatchAPI interface {
	GetManySessions(ctx context.Context, req GetManySessionsRequest) (GetManySessionsResponse, error)
}
