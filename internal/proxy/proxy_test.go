package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/pipeline"
	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/store"
)

func makeUpstream(t *testing.T, callCount *int32, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newProxy(t *testing.T, cfg *policy.Config, upstreamURL string, opts ...pipeline.Option) (*Proxy, store.Store) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	st := store.NewMemory(nil)
	pl := pipeline.New(cfg, st, opts...)
	return New(pl, u, nil), st
}

func newProxyWithEmbedder(t *testing.T, cfg *policy.Config, upstreamURL string) (*Proxy, store.Store) {
	t.Helper()
	emb, _ := embed.NewHashingTFIDF(384, 4)
	return newProxy(t, cfg, upstreamURL, pipeline.WithEmbedder(emb))
}

func doPost(t *testing.T, h http.Handler, tool, body string) *http.Response {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	r.Header.Set("Content-Type", "application/json")
	if tool != "" {
		r.Header.Set(ToolHeader, tool)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func TestProxy_StrictModeReplaysOnDuplicate(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true,"id":"abc"}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"send_email": {
				Mode:              policy.ModeStrict,
				TTL:               time.Hour,
				FingerprintFields: []string{"to", "subject"},
			},
		},
	}
	p, _ := newProxy(t, cfg, upstream.URL)

	body := `{"to":"a@b.com","subject":"Hi","body":"hello"}`
	resp1 := doPost(t, p, "send_email", body)
	if resp1.Header.Get("X-Potent-Status") != "fresh" {
		t.Errorf("first call X-Potent-Status = %q", resp1.Header.Get("X-Potent-Status"))
	}
	resp2 := doPost(t, p, "send_email", body)
	if resp2.Header.Get("X-Potent-Status") != "replayed" {
		t.Errorf("replay X-Potent-Status = %q", resp2.Header.Get("X-Potent-Status"))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream call count = %d, want 1", got)
	}
}

func TestProxy_NormalizationCausesDedup(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"send_email": {
				Mode: policy.ModeStrict,
				TTL:  time.Hour,
				Normalize: map[string][]string{
					"to":      {"lowercase", "trim"},
					"subject": {"trim", "collapse_whitespace"},
				},
				FingerprintFields: []string{"to", "subject"},
			},
		},
	}
	p, _ := newProxy(t, cfg, upstream.URL)
	_ = doPost(t, p, "send_email", `{"to":"Alice@Example.com","subject":"Q3 report"}`)
	resp := doPost(t, p, "send_email", `{"to":" alice@example.com ","subject":"  Q3   report  "}`)
	if resp.Header.Get("X-Potent-Status") != "replayed" {
		t.Errorf("expected replay, got %q", resp.Header.Get("X-Potent-Status"))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1", got)
	}
}

func TestProxy_LogOnlyAlwaysForwards(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{Defaults: policy.ToolPolicy{Mode: policy.ModeLogOnly, TTL: time.Hour}}
	p, _ := newProxy(t, cfg, upstream.URL)
	_ = doPost(t, p, "any_tool", `{"x":1}`)
	_ = doPost(t, p, "any_tool", `{"x":1}`)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("log_only forwards, got calls = %d", got)
	}
}

func TestProxy_OffModeBypasses(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"noisy": {Mode: policy.ModeOff}}}
	p, _ := newProxy(t, cfg, upstream.URL)
	_ = doPost(t, p, "noisy", `{"x":1}`)
	_ = doPost(t, p, "noisy", `{"x":1}`)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("off forwards every call, got %d", got)
	}
}

func TestProxy_MissingToolHeaderPassesThrough(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{Defaults: policy.ToolPolicy{Mode: policy.ModeStrict, TTL: time.Hour}}
	p, _ := newProxy(t, cfg, upstream.URL)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"x":1}`))
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected upstream call, got %d", got)
	}
}

func TestProxy_RequireHumanConfirmBlocks(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"delete_user": {
				Mode:                        policy.ModeStrict,
				TTL:                         time.Hour,
				FingerprintFields:           []string{"user_id"},
				RequireHumanConfirmOnReplay: true,
			},
		},
	}
	p, _ := newProxy(t, cfg, upstream.URL)
	_ = doPost(t, p, "delete_user", `{"user_id":"42"}`)
	resp := doPost(t, p, "delete_user", `{"user_id":"42"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestProxy_InvalidJSONReturns500(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"send_email": {Mode: policy.ModeStrict, TTL: time.Hour}}}
	p, _ := newProxy(t, cfg, "http://upstream.invalid")
	resp := doPost(t, p, "send_email", `not json`)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("expected 500 for invalid JSON, got %d", resp.StatusCode)
	}
}

func TestProxy_CacheModeReplays(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"results":["a","b"]}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"search_web": {Mode: policy.ModeCache, TTL: 5 * time.Minute, FingerprintFields: []string{"query"}},
		},
	}
	p, _ := newProxy(t, cfg, upstream.URL)
	resp1 := doPost(t, p, "search_web", `{"query":"go programming"}`)
	resp2 := doPost(t, p, "search_web", `{"query":"go programming"}`)
	if resp2.Header.Get("X-Potent-Status") != "replayed" {
		t.Errorf("expected cache replay, got %q", resp2.Header.Get("X-Potent-Status"))
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("cache should hit upstream once")
	}
	b1, _ := io.ReadAll(resp1.Body)
	b2, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(b1, b2) {
		t.Errorf("replay body differs")
	}
}

func TestProxy_RequiresBearerWhenTokenSet(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	u, _ := url.Parse("http://upstream.invalid")
	pl := pipeline.New(cfg, store.NewMemory(nil))
	h := New(pl, u, nil, WithAPIToken("s3cret")).Handler()

	// no header
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"to":"a"}`))
	r.Header.Set(ToolHeader, "send_email")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", w.Result().StatusCode)
	}

	// wrong token
	r = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"to":"a"}`))
	r.Header.Set(ToolHeader, "send_email")
	r.Header.Set("Authorization", "Bearer nope")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", w.Result().StatusCode)
	}
}

func TestProxy_PerToolACL_RejectsForbiddenCaller(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {
			Mode:              policy.ModeStrict,
			TTL:               time.Hour,
			FingerprintFields: []string{"to"},
			AllowedCallers:    []string{"team-eng"},
		},
	}}
	u, _ := url.Parse(upstream.URL)
	pl := pipeline.New(cfg, store.NewMemory(nil))
	p := New(pl, u, nil, WithAPITokensFile(map[string]string{
		"tok-eng":       "team-eng",
		"tok-marketing": "team-marketing",
	}))
	h := p.Handler()

	body := `{"to":"alice@example.com","subject":"Q3","body":"hi"}`

	// team-marketing must be rejected
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	r.Header.Set(ToolHeader, "send_email")
	r.Header.Set("Authorization", "Bearer tok-marketing")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusForbidden {
		t.Errorf("forbidden caller: status = %d, want 403", w.Result().StatusCode)
	}

	// team-eng must pass through
	r = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	r.Header.Set(ToolHeader, "send_email")
	r.Header.Set("Authorization", "Bearer tok-eng")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusOK {
		t.Errorf("allowed caller: status = %d, want 200", w.Result().StatusCode)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("upstream calls = %d, want exactly 1", atomic.LoadInt32(&calls))
	}
}

func TestProxy_RateLimitReturns429(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {
			Mode:              policy.ModeStrict,
			TTL:               time.Hour,
			FingerprintFields: []string{"to"},
			RateLimit:         policy.RateLimit{RPS: 1, Burst: 1},
		},
	}}
	u, _ := url.Parse(upstream.URL)
	pl := pipeline.New(cfg, store.NewMemory(nil))
	p := New(pl, u, nil)

	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"to":"a@b.com"}`))
	r.Header.Set(ToolHeader, "send_email")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Result().StatusCode != 200 {
		t.Fatalf("first should pass, got %d", w.Result().StatusCode)
	}

	// second different intent so cache does not absorb
	r = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"to":"b@b.com"}`))
	r.Header.Set(ToolHeader, "send_email")
	w = httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusTooManyRequests {
		t.Errorf("second should rate-limit, got %d", w.Result().StatusCode)
	}
	if w.Result().Header.Get("Retry-After") == "" {
		t.Errorf("missing Retry-After header on 429")
	}
}

func TestProxy_RejectsOversizedBody(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	u, _ := url.Parse(upstream.URL)
	pl := pipeline.New(cfg, store.NewMemory(nil))
	p := New(pl, u, nil, WithMaxBodyBytes(64)) // tiny cap for the test

	huge := bytes.Repeat([]byte("a"), 256)
	body := append([]byte(`{"to":"alice@example.com","filler":"`), huge...)
	body = append(body, '"', '}')

	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	r.Header.Set(ToolHeader, "send_email")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)

	if w.Result().StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413, got %d", w.Result().StatusCode)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("upstream should not have been called for oversized body")
	}
}

func TestProxy_SemanticReplay(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true,"id":"msg-1"}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"send_email": {
				Mode:              policy.ModeStrict,
				TTL:               time.Hour,
				FingerprintFields: []string{"to", "subject", "body"},
				SemanticThreshold: 0.9,
			},
		},
	}
	p, _ := newProxyWithEmbedder(t, cfg, upstream.URL)
	_ = doPost(t, p, "send_email", `{"to":"alice@example.com","subject":"Q3 report","body":"Please find attached the Q3 financial report for your review."}`)
	resp := doPost(t, p, "send_email", `{"to":"alice@example.com","subject":"Q3 report","body":"Please find attached the Q3 financial report for review."}`)
	if resp.Header.Get("X-Potent-Match") != "semantic" {
		t.Errorf("expected semantic match, got %q", resp.Header.Get("X-Potent-Match"))
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("semantic should replay, got upstream calls = %d", atomic.LoadInt32(&calls))
	}
}
