package tracing

import (
	"context"
	"net/http"
)

// HeaderName is the canonical W3C name for the trace-context header
// (spec section 3.2.1).
const HeaderName = "Traceparent"

type ctxKey struct{}

// FromContext returns the SpanContext attached by the middleware or
// (empty, false) when the context has no trace.
func FromContext(ctx context.Context) (SpanContext, bool) {
	v, ok := ctx.Value(ctxKey{}).(SpanContext)
	return v, ok
}

// WithContext returns a derived context carrying c. Typically called by
// the middleware; downstream handlers use FromContext.
func WithContext(ctx context.Context, c SpanContext) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// Middleware is an http.Handler middleware that resolves a SpanContext
// for every inbound request and stores it in the request context.
//
// If the inbound request carries a valid `traceparent` header the trace
// id is preserved (so the operator's tracing backend sees one trace
// across the agent + potent + upstream hops) and a fresh span id is
// generated for this hop. If the header is missing or malformed a brand
// new SpanContext is created, marked sampled so a self-originated
// request still shows up in the backend.
//
// The middleware never fails the request; if the random source errors
// the request proceeds without a trace and a warning fires on the next
// log line.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			sc  SpanContext
			err error
		)
		if parent, ok := Parse(r.Header.Get(HeaderName)); ok {
			sc, err = NewChild(parent)
		} else {
			sc, err = New()
		}
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		// Echo the chosen context back on the response so the caller can
		// correlate the response with its original trace without rereading
		// the spec.
		w.Header().Set(HeaderName, sc.Header())
		next.ServeHTTP(w, r.WithContext(WithContext(r.Context(), sc)))
	})
}

// InjectInto sets the W3C traceparent header on req using c. Outbound
// HTTP clients should call this before sending so the next hop sees a
// well-formed parent context.
func InjectInto(req *http.Request, c SpanContext) {
	if !c.Valid() {
		return
	}
	req.Header.Set(HeaderName, c.Header())
}
