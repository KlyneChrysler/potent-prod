package embed

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPEmbedder_SimpleShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Input != "hello" {
			t.Errorf("got input %q, want hello", req.Input)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{0.6, 0.8, 0}})
	}))
	defer srv.Close()

	e, err := NewHTTPEmbedder(HTTPEmbedderOptions{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	v, err := e.Embed("hello")
	if err != nil {
		t.Fatal(err)
	}
	// 0.6, 0.8, 0 has norm 1.0 already; expect unchanged
	if len(v) != 3 || v[0] != 0.6 || v[1] != 0.8 || v[2] != 0 {
		t.Errorf("unexpected vector %v", v)
	}
	if e.Dim() != 3 {
		t.Errorf("dim discovery failed: %d", e.Dim())
	}
}

func TestHTTPEmbedder_OpenAIShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{3, 4}}},
		})
	}))
	defer srv.Close()
	e, _ := NewHTTPEmbedder(HTTPEmbedderOptions{URL: srv.URL})
	v, err := e.Embed("x")
	if err != nil {
		t.Fatal(err)
	}
	// 3, 4 -> norm 5 -> 0.6, 0.8
	if v[0] < 0.59 || v[0] > 0.61 || v[1] < 0.79 || v[1] > 0.81 {
		t.Errorf("normalised vector wrong: %v", v)
	}
}

func TestHTTPEmbedder_SendsHeaders(t *testing.T) {
	gotAuth := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1, 0}})
	}))
	defer srv.Close()
	e, _ := NewHTTPEmbedder(HTTPEmbedderOptions{
		URL:     srv.URL,
		Headers: map[string]string{"Authorization": "Bearer sk-test"},
	})
	_, _ = e.Embed("x")
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization header = %q", gotAuth)
	}
}

func TestHTTPEmbedder_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()
	e, _ := NewHTTPEmbedder(HTTPEmbedderOptions{URL: srv.URL})
	_, err := e.Embed("x")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Errorf("expected 429 error, got %v", err)
	}
}

func TestHTTPEmbedder_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	e, _ := NewHTTPEmbedder(HTTPEmbedderOptions{URL: srv.URL})
	_, err := e.Embed("x")
	if err == nil || !strings.Contains(err.Error(), "no embedding") {
		t.Errorf("expected no-embedding error, got %v", err)
	}
}

func TestHTTPEmbedder_DimMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1, 0, 0}})
	}))
	defer srv.Close()
	e, _ := NewHTTPEmbedder(HTTPEmbedderOptions{URL: srv.URL, Dim: 5})
	_, err := e.Embed("x")
	if err == nil || !strings.Contains(err.Error(), "dim mismatch") {
		t.Errorf("expected dim mismatch error, got %v", err)
	}
}

func TestHTTPEmbedder_EmptyTextReturnsZeroVector(t *testing.T) {
	e, _ := NewHTTPEmbedder(HTTPEmbedderOptions{URL: "http://unreachable.test", Dim: 4})
	v, err := e.Embed("")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 4 {
		t.Errorf("len=%d, want 4", len(v))
	}
	for _, x := range v {
		if x != 0 {
			t.Errorf("non-zero element: %v", v)
		}
	}
}

func TestNewHTTPEmbedder_RequiresURL(t *testing.T) {
	if _, err := NewHTTPEmbedder(HTTPEmbedderOptions{}); err == nil {
		t.Errorf("expected error for missing URL")
	}
}

func TestHTTPEmbedder_NormalisesNonUnit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{100, 0, 0}})
	}))
	defer srv.Close()
	e, _ := NewHTTPEmbedder(HTTPEmbedderOptions{URL: srv.URL})
	v, err := e.Embed("x")
	if err != nil {
		t.Fatal(err)
	}
	if abs32(v[0]-1.0) > 1e-4 {
		t.Errorf("expected normalised vector, got %v", v)
	}
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}
