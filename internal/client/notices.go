package client

import "context"

// NoticeSink receives one-line, human-readable notices about a call in
// progress: why it is retrying (a restarting hub or local pyred) and the
// progress of a long upload. The pyre CLI prints them on stderr.
type NoticeSink func(msg string)

type noticeSinkKey struct{}

// WithNotices returns ctx whose calls report notices to sink. A nil sink
// silences them (a full-screen UI that owns the terminal).
func WithNotices(ctx context.Context, sink NoticeSink) context.Context {
	return context.WithValue(ctx, noticeSinkKey{}, sink)
}

func notify(ctx context.Context, msg string) {
	if sink, _ := ctx.Value(noticeSinkKey{}).(NoticeSink); sink != nil {
		sink(msg)
	}
}
