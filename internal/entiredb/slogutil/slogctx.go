package slogutil

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/trace"
)

type slogctxKey struct{}

type CtxHandler struct {
	slog.Handler
}

func AddToCtx(ctx context.Context, attr slog.Attr) context.Context {
	if v := ctx.Value(slogctxKey{}); v != nil {
		if attrs, ok := v.([]slog.Attr); ok {
			return context.WithValue(ctx, slogctxKey{}, append(attrs, attr))
		}
	}
	return context.WithValue(ctx, slogctxKey{}, []slog.Attr{attr})
}

func CopyAttrs(dst, src context.Context) context.Context {
	return context.WithValue(dst, slogctxKey{}, src.Value(slogctxKey{}))
}

func NewCtxHandler(h slog.Handler) *CtxHandler {
	return &CtxHandler{Handler: h}
}

func (h *CtxHandler) Handle(ctx context.Context, r slog.Record) error {
	if v := ctx.Value(slogctxKey{}); v != nil {
		if attrs, ok := v.([]slog.Attr); ok {
			r.AddAttrs(attrs...)
		}
	}
	for _, m := range baggage.FromContext(ctx).Members() {
		r.AddAttrs(slog.String(m.Key(), m.Value()))
	}
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}
