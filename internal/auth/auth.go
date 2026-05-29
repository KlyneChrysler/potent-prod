// Package auth provides bearer-token middleware shared by the proxy port
// and the admin port. A single implementation keeps the constant-time
// comparison, the WWW-Authenticate challenge, and the empty-token escape
// hatch identical everywhere.
package auth

import (
	"crypto/subtle"
	"net/http"
)

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
