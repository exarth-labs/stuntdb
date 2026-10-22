package redact

import (
	"context"
	"fmt"
	"log/slog"
)

// Handler wraps h so that every log record is redacted before h sees it:
// the message, and every attribute that holds text (strings, errors,
// Stringers and other values, in groups too). Attribute keys are kept, as
// are numbers, booleans, times and durations, which hold no text.
func (r *Redactor) Handler(h slog.Handler) slog.Handler {
	return &handler{r: r, h: h}
}

type handler struct {
	r *Redactor
	h slog.Handler
}

func (x *handler) Enabled(ctx context.Context, level slog.Level) bool {
	return x.h.Enabled(ctx, level)
}

func (x *handler) Handle(ctx context.Context, rec slog.Record) error {
	clean := slog.NewRecord(rec.Time, rec.Level, x.r.String(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		clean.AddAttrs(x.attr(a))
		return true
	})
	return x.h.Handle(ctx, clean)
}

func (x *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		clean[i] = x.attr(a)
	}
	return &handler{r: x.r, h: x.h.WithAttrs(clean)}
}

func (x *handler) WithGroup(name string) slog.Handler {
	return &handler{r: x.r, h: x.h.WithGroup(name)}
}

func (x *handler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve() // a LogValuer, such as value.Value, shows only what it chooses
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, x.r.String(v.String()))
	case slog.KindGroup:
		group := v.Group()
		clean := make([]slog.Attr, len(group))
		for i, g := range group {
			clean[i] = x.attr(g)
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(clean...)}
	case slog.KindAny:
		switch val := v.Any().(type) {
		case error:
			return slog.String(a.Key, x.r.String(val.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, x.r.String(val.String()))
		default:
			return slog.String(a.Key, x.r.String(fmt.Sprint(val)))
		}
	default:
		return slog.Attr{Key: a.Key, Value: v}
	}
}
