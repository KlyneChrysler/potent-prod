package tracing

import (
	"context"
	"log/slog"
)

// SlogHandler wraps an inner slog.Handler and adds trace_id and span_id
// attributes to every record whose context carries a SpanContext. Records
// without a context-borne trace are written through unchanged, so the
// wrapper is safe to install globally on slog.SetDefault even when most
// of the process's logs originate outside an HTTP request.
type SlogHandler struct {
	inner slog.Handler
}

// NewSlogHandler returns a SlogHandler that delegates to inner.
func NewSlogHandler(inner slog.Handler) *SlogHandler {
	return &SlogHandler{inner: inner}
}

// Enabled delegates to the wrapped handler.
func (h *SlogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.inner.Enabled(ctx, level)
}

// Handle adds trace_id and span_id attributes from the context before
// passing the record through.
func (h *SlogHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc, ok := FromContext(ctx); ok && sc.Valid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID),
			slog.String("span_id", sc.SpanID),
		)
	}
	return h.inner.Handle(ctx, r)
}

// WithAttrs returns a new handler wrapping the inner handler's
// equivalent so adders chain correctly through structured group calls.
func (h *SlogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &SlogHandler{inner: h.inner.WithAttrs(attrs)}
}

// WithGroup mirrors WithAttrs for slog group calls.
func (h *SlogHandler) WithGroup(name string) slog.Handler {
	return &SlogHandler{inner: h.inner.WithGroup(name)}
}
