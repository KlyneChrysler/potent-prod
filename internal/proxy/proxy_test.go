package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/potent/potent/internal/embed"
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

func newProxy(t *testing.T, cfg *policy.Config, upstreamURL string) (*Proxy, store.Store) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	st := store.NewMemory(nil)
	return New(cfg, st, u, nil), st
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
		Defaults: policy.ToolPolicy{Mode: policy.ModeLogOnly, TTL: time.Hour},
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
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first call status = %d", resp1.StatusCode)
	}
	if got := resp1.Header.Get("X-Potent-Status"); got != "fresh" {
		t.Errorf("first call X-Potent-Status = %q, want fresh", got)
	}

	resp2 := doPost(t, p, "send_email", body)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("replay status = %d", resp2.StatusCode)
	}
	if got := resp2.Header.Get("X-Potent-Status"); got != "replayed" {
		t.Errorf("replay X-Potent-Status = %q, want replayed", got)
	}

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream call count = %d, want 1 (second should be replayed)", got)
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

	a := `{"to":"Alice@Example.com","subject":"Q3 report"}`
	b := `{"to":" alice@example.com ","subject":"  Q3   report  "}`

	_ = doPost(t, p, "send_email", a)
	resp := doPost(t, p, "send_email", b)
	if resp.Header.Get("X-Potent-Status") != "replayed" {
		t.Errorf("expected normalized duplicate to replay, got %q", resp.Header.Get("X-Potent-Status"))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1", got)
	}
}

func TestProxy_LogOnlyModeAlwaysForwards(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{
		Defaults: policy.ToolPolicy{Mode: policy.ModeLogOnly, TTL: time.Hour},
	}
	p, _ := newProxy(t, cfg, upstream.URL)

	body := `{"x":1}`
	_ = doPost(t, p, "any_tool", body)
	_ = doPost(t, p, "any_tool", body)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("log_only should always forward, got calls = %d", got)
	}
}

func TestProxy_OffModeBypassesPipeline(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"noisy_tool": {Mode: policy.ModeOff},
		},
	}
	p, _ := newProxy(t, cfg, upstream.URL)

	body := `{"x":1}`
	_ = doPost(t, p, "noisy_tool", body)
	_ = doPost(t, p, "noisy_tool", body)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("off mode should forward every call, got %d", got)
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
		t.Errorf("expected upstream to receive call, got %d", got)
	}
}

func TestProxy_StrictRequireHumanConfirmBlocksReplay(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true,"id":"u1"}`)
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

	body := `{"user_id":"42"}`
	_ = doPost(t, p, "delete_user", body)
	resp := doPost(t, p, "delete_user", body)

	if resp.StatusCode != http.StatusConflict {
		t.Errorf("expected 409, got %d", resp.StatusCode)
	}
}

func TestProxy_InvalidJSONReturns400(t *testing.T) {
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"send_email": {Mode: policy.ModeStrict, TTL: time.Hour},
		},
	}
	p, _ := newProxy(t, cfg, "http://upstream.invalid")

	resp := doPost(t, p, "send_email", `not json`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid JSON, got %d", resp.StatusCode)
	}
}

func TestProxy_CacheModeReplays(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"results":["a","b"]}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"search_web": {
				Mode:              policy.ModeCache,
				TTL:               5 * time.Minute,
				FingerprintFields: []string{"query"},
			},
		},
	}
	p, _ := newProxy(t, cfg, upstream.URL)

	body := `{"query":"go programming"}`
	resp1 := doPost(t, p, "search_web", body)
	resp2 := doPost(t, p, "search_web", body)

	if resp2.Header.Get("X-Potent-Status") != "replayed" {
		t.Errorf("expected cached replay, got %q", resp2.Header.Get("X-Potent-Status"))
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("cache mode should hit upstream once")
	}

	b1, _ := io.ReadAll(resp1.Body)
	b2, _ := io.ReadAll(resp2.Body)
	if !bytes.Equal(b1, b2) {
		t.Errorf("replay body differs: %s vs %s", b1, b2)
	}

	// sanity: the cached response is still valid JSON
	var dummy any
	if err := json.Unmarshal(b2, &dummy); err != nil {
		t.Errorf("cached body not valid JSON: %v", err)
	}
}

func newProxyWithEmbedder(t *testing.T, cfg *policy.Config, upstreamURL string) (*Proxy, store.Store) {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	st := store.NewMemory(nil)
	emb, err := embed.NewHashingTFIDF(384, 4)
	if err != nil {
		t.Fatalf("embedder: %v", err)
	}
	return New(cfg, st, u, nil, WithEmbedder(emb)), st
}

func TestProxy_SemanticReplayOnNearDuplicate(t *testing.T) {
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

	// First call seeds the cache.
	first := `{"to":"alice@example.com","subject":"Q3 report","body":"Please find attached the Q3 financial report for review."}`
	resp1 := doPost(t, p, "send_email", first)
	if resp1.Header.Get("X-Potent-Status") != "fresh" {
		t.Fatalf("first call must be fresh, got %q", resp1.Header.Get("X-Potent-Status"))
	}

	// Near-duplicate: same recipient/subject, body with minor edits.
	// Exact hash differs (body wording changed), but semantic match should fire.
	near := `{"to":"alice@example.com","subject":"Q3 report","body":"Please find attached the Q3 financial report for your review."}`
	resp2 := doPost(t, p, "send_email", near)

	if resp2.Header.Get("X-Potent-Status") != "replayed" {
		t.Errorf("expected semantic replay, got status %q", resp2.Header.Get("X-Potent-Status"))
	}
	if resp2.Header.Get("X-Potent-Match") != "semantic" {
		t.Errorf("expected match=semantic, got %q", resp2.Header.Get("X-Potent-Match"))
	}
	if got := resp2.Header.Get("X-Potent-Similarity"); got == "" {
		t.Errorf("expected X-Potent-Similarity to be set")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected upstream calls=1 (semantic replayed), got %d", got)
	}
}

func TestProxy_SemanticDoesNotReplayUnrelated(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
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

	_ = doPost(t, p, "send_email", `{"to":"alice@example.com","subject":"Q3 report","body":"financial summary attached"}`)
	resp := doPost(t, p, "send_email", `{"to":"bob@somewhere.org","subject":"office party invite","body":"come join us Friday for drinks"}`)

	if resp.Header.Get("X-Potent-Status") == "replayed" {
		t.Errorf("unrelated request must not replay")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 upstream calls, got %d", got)
	}
}

func TestProxy_SemanticDisabledWhenThresholdZero(t *testing.T) {
	var calls int32
	upstream := makeUpstream(t, &calls, `{"ok":true}`)
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"send_email": {
				Mode:              policy.ModeStrict,
				TTL:               time.Hour,
				FingerprintFields: []string{"to", "subject", "body"},
				SemanticThreshold: 0, // disabled
			},
		},
	}
	p, _ := newProxyWithEmbedder(t, cfg, upstream.URL)

	_ = doPost(t, p, "send_email", `{"to":"alice@example.com","subject":"Q3 report","body":"financial summary attached"}`)
	resp := doPost(t, p, "send_email", `{"to":"alice@example.com","subject":"Q3 report","body":"financial summary attached please"}`)

	if resp.Header.Get("X-Potent-Status") == "replayed" {
		t.Errorf("semantic disabled, must not replay near-duplicate")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("expected 2 upstream calls, got %d", got)
	}
}

func TestProxy_ContextPropagatesToStore(t *testing.T) {
	cfg := &policy.Config{
		Tools: map[string]policy.ToolPolicy{
			"t": {Mode: policy.ModeStrict, TTL: time.Hour},
		},
	}
	p, _ := newProxy(t, cfg, "http://upstream.invalid")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"x":1}`)).WithContext(ctx)
	r.Header.Set(ToolHeader, "t")
	w := httptest.NewRecorder()
	// should not panic; behavior is allowed to be either error or pass-through-failure
	p.ServeHTTP(w, r)
}
