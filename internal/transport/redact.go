package transport

import (
	"context"
	"log/slog"
	"regexp"
)

// secretPattern matches any Pyre-prefixed secret (see the Prefix* constants)
// and bearer header values.
var (
	secretPattern = regexp.MustCompile(`py[cnstep]_[A-Za-z0-9_\-]{12,}`)
	bearerPattern = regexp.MustCompile(`(?i)(bearer\s+)[^\s"',;]+`)
)

// Redact replaces every Pyre secret and bearer value in s with a marker that
// keeps only the non-secret prefix (e.g. "pyn_[REDACTED]"). Apply it to any
// string that might carry a credential before logging or returning it.
func Redact(s string) string {
	s = bearerPattern.ReplaceAllString(s, "${1}[REDACTED]")
	return secretPattern.ReplaceAllStringFunc(s, func(m string) string {
		return m[:4] + "[REDACTED]"
	})
}

// RedactingHandler wraps a slog.Handler and passes every message and string
// or error attribute through Redact. Package loggers are wrapped with it, so
// a secret accidentally included in an error never reaches the log sink.
type RedactingHandler struct{ inner slog.Handler }

// NewRedactingLogger returns a logger whose output is redacted. A nil
// handler yields a logger that discards everything.
func NewRedactingLogger(h slog.Handler) *slog.Logger {
	if h == nil {
		h = slog.DiscardHandler
	}
	return slog.New(&RedactingHandler{inner: h})
}

func redactingLogger(l *slog.Logger) *slog.Logger {
	if l == nil {
		return NewRedactingLogger(nil)
	}
	if _, ok := l.Handler().(*RedactingHandler); ok {
		return l
	}
	return NewRedactingLogger(l.Handler())
}

func (h *RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h *RedactingHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, nr)
}

func (h *RedactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = redactAttr(a)
	}
	return &RedactingHandler{inner: h.inner.WithAttrs(out)}
}

func (h *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{inner: h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		g := v.Group()
		out := make([]any, len(g))
		for i, ga := range g {
			out[i] = redactAttr(ga)
		}
		return slog.Group(a.Key, out...)
	case slog.KindAny:
		if err, ok := v.Any().(error); ok {
			return slog.String(a.Key, Redact(err.Error()))
		}
		return slog.String(a.Key, Redact(v.String()))
	}
	return slog.Attr{Key: a.Key, Value: v}
}
