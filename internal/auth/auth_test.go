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

func TestRequireBearer_DefaultRealm(t *testing.T) {
	h := RequireBearer("secret", "", http.HandlerFunc(ok))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := w.Result().Header.Get("WWW-Authenticate"); !strings.Contains(got, `realm="potent"`) {
		t.Errorf("default realm should be potent, got %q", got)
	}
}
