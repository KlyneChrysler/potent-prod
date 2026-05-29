package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func ok(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func TestRequireBearer_EmptyTokenPassesThrough(t *testing.T) {
	h := RequireBearer("", "", http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("empty token should pass through, got %d", w.Result().StatusCode)
	}
}

func TestRequireBearer_RejectsMissingHeader(t *testing.T) {
	h := RequireBearer("secret", "test", http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("missing header should 401, got %d", w.Result().StatusCode)
	}
	if !strings.Contains(w.Result().Header.Get("WWW-Authenticate"), `realm="test"`) {
		t.Errorf("missing WWW-Authenticate challenge: %q", w.Result().Header.Get("WWW-Authenticate"))
	}
}

func TestRequireBearer_RejectsWrongToken(t *testing.T) {
	h := RequireBearer("secret", "", http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token should 401, got %d", w.Result().StatusCode)
	}
}

func TestRequireBearer_AcceptsCorrect(t *testing.T) {
	h := RequireBearer("secret", "", http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("right token should 200, got %d", w.Result().StatusCode)
	}
}

func TestRequireBearerCallers_AttachesCallerToContext(t *testing.T) {
	var seen string
	h := RequireBearerCallers(
		map[string]string{"tok-A": "team-eng", "tok-B": "team-finance"},
		"",
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = CallerFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}),
	)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer tok-B")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status = %d", w.Result().StatusCode)
	}
	if seen != "team-finance" {
		t.Errorf("caller in ctx = %q, want team-finance", seen)
	}
}

func TestRequireBearerCallers_RejectsUnknownToken(t *testing.T) {
	h := RequireBearerCallers(
		map[string]string{"tok-A": "team-eng"}, "",
		http.HandlerFunc(ok),
	)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer nope")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("unknown token = %d, want 401", w.Result().StatusCode)
	}
}

func TestRequireBearerCallers_EmptyMapPassesThrough(t *testing.T) {
	h := RequireBearerCallers(nil, "", http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("empty map should pass, got %d", w.Result().StatusCode)
	}
}

func TestCallerFromContext_DefaultEmpty(t *testing.T) {
	if got := CallerFromContext(httptest.NewRequest(http.MethodGet, "/", nil).Context()); got != "" {
		t.Errorf("default caller should be empty, got %q", got)
	}
}

func TestRequireBearer_DefaultRealm(t *testing.T) {
	h := RequireBearer("secret", "", http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Result().Header.Get("WWW-Authenticate"); !strings.Contains(got, `realm="potent"`) {
		t.Errorf("default realm should be potent, got %q", got)
	}
}
