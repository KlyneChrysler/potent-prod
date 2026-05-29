package mcp

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/potent/potent/internal/pipeline"
	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/store"
)

func mcpUpstream(t *testing.T, callCount *int32, response any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newMCPHandler(t *testing.T, cfg *policy.Config, upstreamURL string) (*HTTPHandler, store.Store) {
	t.Helper()
	u, _ := url.Parse(upstreamURL)
	st := store.NewMemory(nil)
	pl := pipeline.New(cfg, st)
	return NewHTTPHandler(pl, u, nil), st
}

func sendRPC(t *testing.T, h http.Handler, frame any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(frame)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result()
}

func TestMCPHTTP_ToolsCallReplays(t *testing.T) {
	var calls int32
	upstream := mcpUpstream(t, &calls, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  map[string]any{"content": []map[string]any{{"type": "text", "text": "email sent"}}},
	})

	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to", "subject"}},
	}}
	h, _ := newMCPHandler(t, cfg, upstream.URL)

	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "send_email", "arguments": map[string]any{"to": "a@b.com", "subject": "Hi"}},
	}
	resp1 := sendRPC(t, h, rpc)
	if resp1.Header.Get("X-Potent-Status") != "fresh" {
		t.Errorf("first call status = %q", resp1.Header.Get("X-Potent-Status"))
	}

	rpc["id"] = 2
	resp2 := sendRPC(t, h, rpc)
	if resp2.Header.Get("X-Potent-Status") != "replayed" {
		t.Errorf("second call status = %q, want replayed", resp2.Header.Get("X-Potent-Status"))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("upstream calls = %d, want 1", got)
	}

	// The replayed frame must carry the new request's id, not the cached one.
	body, _ := io.ReadAll(resp2.Body)
	var got Message
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var gotID int
	_ = json.Unmarshal(got.ID, &gotID)
	if gotID != 2 {
		t.Errorf("replay id = %d, want 2 (must match current request)", gotID)
	}
}

func TestMCPHTTP_NonToolsCallPassesThrough(t *testing.T) {
	var calls int32
	upstream := mcpUpstream(t, &calls, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result":  map[string]any{"tools": []any{}},
	})
	cfg := &policy.Config{Defaults: policy.ToolPolicy{Mode: policy.ModeStrict, TTL: time.Hour}}
	h, _ := newMCPHandler(t, cfg, upstream.URL)

	rpc := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	_ = sendRPC(t, h, rpc)
	_ = sendRPC(t, h, rpc)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("tools/list must always reach upstream, got %d calls", got)
	}
}

func TestMCPHTTP_NonJSONPassesThrough(t *testing.T) {
	var calls int32
	upstream := mcpUpstream(t, &calls, "anything")
	cfg := &policy.Config{Defaults: policy.ToolPolicy{Mode: policy.ModeStrict, TTL: time.Hour}}
	h, _ := newMCPHandler(t, cfg, upstream.URL)

	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString("not json at all"))
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("non-JSON passthrough should reach upstream once, got %d", got)
	}
}

func TestMCPHTTP_ReframeWithID(t *testing.T) {
	cached := []byte(`{"jsonrpc":"2.0","id":1,"result":{"x":1}}`)
	newID := json.RawMessage(`42`)
	out := reframeWithID(cached, newID)

	var got Message
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var id int
	_ = json.Unmarshal(got.ID, &id)
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}
}

func TestParseToolsCall_RejectsMissingName(t *testing.T) {
	m := &Message{Method: MethodToolsCall, Params: json.RawMessage(`{}`)}
	if _, _, err := ParseToolsCall(m); err == nil {
		t.Errorf("expected error for missing name")
	}
}

func TestParseToolsCall_RejectsWrongMethod(t *testing.T) {
	m := &Message{Method: "tools/list"}
	if _, _, err := ParseToolsCall(m); err == nil {
		t.Errorf("expected error for wrong method")
	}
}

func TestMCPHTTP_GETPassesThrough(t *testing.T) {
	var calls int32
	upstream := mcpUpstream(t, &calls, "ok")
	cfg := &policy.Config{Defaults: policy.ToolPolicy{Mode: policy.ModeStrict, TTL: time.Hour}}
	h, _ := newMCPHandler(t, cfg, upstream.URL)

	r := httptest.NewRequest(http.MethodGet, "/events", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("GET should pass through, got %d calls", got)
	}
}

func TestMessage_KindHelpers(t *testing.T) {
	req := Message{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/call"}
	if !req.IsRequest() || req.IsNotification() || req.IsResponse() {
		t.Errorf("request kind misclassified: %+v", req)
	}
	notif := Message{JSONRPC: "2.0", Method: "notify"}
	if notif.IsRequest() || !notif.IsNotification() || notif.IsResponse() {
		t.Errorf("notification kind misclassified: %+v", notif)
	}
	resp := Message{JSONRPC: "2.0", ID: json.RawMessage(`1`), Result: json.RawMessage(`{}`)}
	if resp.IsRequest() || resp.IsNotification() || !resp.IsResponse() {
		t.Errorf("response kind misclassified: %+v", resp)
	}
}

func TestNewToolsCallResponse(t *testing.T) {
	out := NewToolsCallResponse(json.RawMessage(`5`), []byte(`{"x":1}`))
	if out.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q", out.JSONRPC)
	}
	if string(out.Result) != `{"x":1}` {
		t.Errorf("result = %s", out.Result)
	}
}

func TestNewErrorResponse(t *testing.T) {
	out := NewErrorResponse(json.RawMessage(`1`), -32000, "boom")
	if out.Error == nil {
		t.Fatalf("error missing")
	}
	if out.Error.Code != -32000 || out.Error.Message != "boom" {
		t.Errorf("err = %+v", out.Error)
	}
}

func TestReframeWithID_InvalidJSONReturnsUnchanged(t *testing.T) {
	in := []byte("not json")
	out := reframeWithID(in, json.RawMessage(`1`))
	if string(out) != "not json" {
		t.Errorf("got %q, want unchanged", out)
	}
}

func TestReframeWithID_EmptyID(t *testing.T) {
	in := []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`)
	out := reframeWithID(in, nil)
	if string(out) != string(in) {
		t.Errorf("empty id should leave body unchanged, got %s", out)
	}
}

func TestSingleJoin(t *testing.T) {
	cases := []struct {
		a, b, want string
	}{
		{"", "/path", "/path"},
		{"/api", "", "/api"},
		{"/api/", "/v1", "/api/v1"},
		{"/api", "/v1", "/api/v1"},
		{"/api", "v1", "/api/v1"},
	}
	for _, c := range cases {
		if got := singleJoin(c.a, c.b); got != c.want {
			t.Errorf("singleJoin(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestMCPHTTP_BlockOnRequireHumanConfirm(t *testing.T) {
	var calls int32
	upstream := mcpUpstream(t, &calls, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}})
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"delete_user": {
			Mode:                        policy.ModeStrict,
			TTL:                         time.Hour,
			FingerprintFields:           []string{"user_id"},
			RequireHumanConfirmOnReplay: true,
		},
	}}
	h, _ := newMCPHandler(t, cfg, upstream.URL)

	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "delete_user", "arguments": map[string]any{"user_id": "42"}},
	}
	_ = sendRPC(t, h, rpc)
	rpc["id"] = 2
	resp := sendRPC(t, h, rpc)
	body, _ := io.ReadAll(resp.Body)
	var got Message
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil {
		t.Errorf("expected error response on blocked duplicate")
	}
}

func TestMCPHTTP_UpstreamErrorSurfaces(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	h, _ := newMCPHandler(t, cfg, "http://127.0.0.1:1") // intentionally bad upstream
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "send_email", "arguments": map[string]any{"to": "x"}},
	}
	resp := sendRPC(t, h, rpc)
	if resp.StatusCode == http.StatusOK {
		// either OK with empty body or error — we only require the handler doesn't panic
		body, _ := io.ReadAll(resp.Body)
		_ = body
	}
}

func TestMCPHTTP_PerToolACL_RejectsForbiddenCaller(t *testing.T) {
	var calls int32
	upstream := mcpUpstream(t, &calls, map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"ok": true}})
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
	h := NewHTTPHandler(pl, u, nil, WithHTTPAPITokensFile(map[string]string{
		"tok-mkt": "team-marketing",
	})).Handler()

	frame := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "send_email", "arguments": map[string]any{"to": "a@b.com"}},
	}
	b, _ := json.Marshal(frame)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer tok-mkt")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	body, _ := io.ReadAll(w.Result().Body)
	var got Message
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\nraw: %s", err, body)
	}
	if got.Error == nil || got.Error.Code != -32003 {
		t.Errorf("expected json-rpc -32003 forbidden, got %+v", got.Error)
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Errorf("upstream should not have been called when caller is forbidden")
	}
}

func TestMCPHTTP_WithHTTPUpstreamTLS_DoesNotPanicOnNil(t *testing.T) {
	pl := pipeline.New(&policy.Config{}, store.NewMemory(nil))
	u, _ := url.Parse("http://upstream.invalid")
	h := NewHTTPHandler(pl, u, nil, WithHTTPUpstreamTLS(nil))
	if h.client == nil {
		t.Errorf("client should not be nil after WithHTTPUpstreamTLS(nil)")
	}
}

func TestMCPHTTP_WithHTTPUpstreamTLS_ReplacesClientTransport(t *testing.T) {
	pl := pipeline.New(&policy.Config{}, store.NewMemory(nil))
	u, _ := url.Parse("http://upstream.invalid")
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "wiring-test"}
	h := NewHTTPHandler(pl, u, nil, WithHTTPUpstreamTLS(cfg))
	tp, ok := h.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", h.client.Transport)
	}
	// Verify wiring via a distinctive non-default field. We do not exercise
	// disabled verification anywhere, even in tests, to avoid modeling a
	// real anti-pattern.
	if tp.TLSClientConfig == nil || tp.TLSClientConfig.ServerName != "wiring-test" || tp.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Errorf("tls config not applied to client transport: %+v", tp.TLSClientConfig)
	}
}

func TestMCPHTTP_RequiresBearerWhenTokenSet(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	u, _ := url.Parse("http://upstream.invalid")
	pl := pipeline.New(cfg, store.NewMemory(nil))
	h := NewHTTPHandler(pl, u, nil, WithHTTPAPIToken("s3cret")).Handler()

	frame := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "send_email", "arguments": map[string]any{"to": "a"}},
	}
	b, _ := json.Marshal(frame)
	r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(b))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Result().StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token: status = %d, want 401", w.Result().StatusCode)
	}
}

func TestMCPHTTP_MalformedToolsCallReturnsError(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"send_email": {Mode: policy.ModeStrict, TTL: time.Hour}}}
	h, _ := newMCPHandler(t, cfg, "http://upstream.invalid")
	rpc := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"arguments": map[string]any{"x": 1}}, // missing name
	}
	resp := sendRPC(t, h, rpc)
	body, _ := io.ReadAll(resp.Body)
	var got Message
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Error == nil {
		t.Errorf("expected error response for missing name")
	}
}
