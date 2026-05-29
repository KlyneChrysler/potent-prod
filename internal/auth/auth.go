// Package auth provides bearer-token middleware shared by the proxy port
// and the admin port. A single implementation keeps the constant-time
// comparison, the WWW-Authenticate challenge, and the empty-token escape
// hatch identical everywhere.
//
// Two modes are supported:
//
//   - Single token. RequireBearer(token, realm, next) gates next behind one
//     shared secret. Used by the admin port and by simple proxy deployments
//     where every caller is trusted equally.
//
//   - Multi-token with caller identity. RequireBearerCallers(tokens, realm,
//     next) accepts a {token -> caller-id} map. The caller-id is written into
//     the request context via CallerKey so downstream handlers can enforce
//     per-tool ACLs and emit audit records that name the responsible caller.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
)

// CallerKey is the context key under which RequireBearerCallers stores the
// caller-id that matched the incoming token. Downstream handlers retrieve
// it via CallerFromContext.
type callerKey struct{}

// CallerFromContext returns the caller-id that the auth middleware attached
// to ctx, or the empty string if no caller is set (single-token mode or
// auth disabled).
func CallerFromContext(ctx context.Context) string {
	v, _ := ctx.Value(callerKey{}).(string)
	return v
}

// RequireBearer returns an http.Handler that gates next behind constant-time
// validation of an Authorization: Bearer <token> header. An empty token
// disables auth entirely; callers that should require a token at startup
// must enforce that at construction time (cmd/potent does this).
func RequireBearer(token, realm string, next http.Handler) http.Handler {
	if token == "" {
		return next
	}
	if realm == "" {
		realm = "potent"
	}
	expected := []byte("Bearer " + token)
	challenge := `Bearer realm="` + realm + `"`
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireBearerCallers returns an http.Handler that validates the inbound
// Authorization: Bearer header against tokens, a {token -> caller-id} map.
// On success, the matching caller-id is stored in the request context so
// downstream handlers can render it in audit records and enforce per-tool
// ACLs. An empty or nil map disables auth.
//
// Comparison is constant-time across every token in the map so an attacker
// cannot use response timing to learn whether a guessed token is partially
// correct or to enumerate valid tokens.
func RequireBearerCallers(tokens map[string]string, realm string, next http.Handler) http.Handler {
	if len(tokens) == 0 {
		return next
	}
	if realm == "" {
		realm = "potent"
	}
	challenge := `Bearer realm="` + realm + `"`

	// Precompute expected bearer headers once so the request path only
	// does string compares.
	type entry struct {
		expected []byte
		caller   string
	}
	entries := make([]entry, 0, len(tokens))
	for token, caller := range tokens {
		entries = append(entries, entry{
			expected: []byte("Bearer " + token),
			caller:   caller,
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		// Walk every entry even after a match to keep the wall-clock
		// runtime independent of which token matched (or whether any did).
		var matched string
		for _, e := range entries {
			if subtle.ConstantTimeCompare(got, e.expected) == 1 {
				matched = e.caller
			}
		}
		if matched == "" {
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, matched)))
	})
}
