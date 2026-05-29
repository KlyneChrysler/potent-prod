package tracing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpanContext_HeaderRoundTrip(t *testing.T) {
	c := SpanContext{
		TraceID: "4bf92f3577b34da6a3ce929d0e0e4736",
		SpanID:  "00f067aa0ba902b7",
		Flags:   FlagSampled,
	}
	header := c.Header()
	want := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if header != want {
		t.Errorf("Header() = %q, want %q", header, want)
	}
	parsed, ok := Parse(header)
	if !ok {
		t.Fatalf("Parse failed on our own output")
	}
	if parsed != c {
		t.Errorf("round trip differs: %+v vs %+v", parsed, c)
	}
}

func TestSpanContext_Valid(t *testing.T) {
	cases := []struct {
		name string
		c    SpanContext
		want bool
	}{
		{"empty", SpanContext{}, false},
		{"all zero trace", SpanContext{TraceID: "00000000000000000000000000000000", SpanID: "00f067aa0ba902b7"}, false},
		{"all zero span", SpanContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "0000000000000000"}, false},
		{"wrong trace length", SpanContext{TraceID: "abcd", SpanID: "00f067aa0ba902b7"}, false},
		{"wrong span length", SpanContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "ab"}, false},
		{"non hex", SpanContext{TraceID: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", SpanID: "00f067aa0ba902b7"}, false},
		{"valid", SpanContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: FlagSampled}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.c.Valid(); got != c.want {
				t.Errorf("Valid = %v, want %v", got, c.want)
			}
		})
	}
}

func TestParse_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7", // missing flags
		"01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", // unsupported version
		"00-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz-00f067aa0ba902b7-01", // non hex
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz", // bad flags
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			if _, ok := Parse(in); ok {
				t.Errorf("Parse should have rejected %q", in)
			}
		})
	}
}

func TestNew_AndNewChild(t *testing.T) {
	parent, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !parent.Valid() {
		t.Errorf("New produced invalid context: %+v", parent)
	}
	if parent.Flags&FlagSampled == 0 {
		t.Errorf("New should default to sampled")
	}
	child, err := NewChild(parent)
	if err != nil {
		t.Fatalf("NewChild: %v", err)
	}
	if child.TraceID != parent.TraceID {
		t.Errorf("child trace_id = %s, want parent %s", child.TraceID, parent.TraceID)
	}
	if child.SpanID == parent.SpanID {
		t.Errorf("child span_id must differ from parent; both are %s", child.SpanID)
	}
}

func TestGenerate_PropagatesRandomError(t *testing.T) {
	_, err := generate(func([]byte) (int, error) { return 0, errors.New("no entropy") })
	if err == nil {
		t.Errorf("expected error from broken random source")
	}
}

func TestGenerate_AvoidsAllZero(t *testing.T) {
	// First call returns all zero; second call returns ones. The generator
	// must reject the first and retry.
	calls := 0
	readFn := func(b []byte) (int, error) {
		calls++
		if calls == 1 || calls == 3 {
			// zero trace then zero span before any non-zero hits
			for i := range b {
				b[i] = 0
			}
			return len(b), nil
		}
		for i := range b {
			b[i] = 0x7f
		}
		return len(b), nil
	}
	c, err := generate(readFn)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !c.Valid() {
		t.Errorf("generate emitted invalid ctx: %+v", c)
	}
}

func TestMiddleware_ExtractsAndStoresContext(t *testing.T) {
	inbound := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	var got SpanContext
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, ok := FromContext(r.Context())
		if !ok {
			t.Errorf("middleware did not attach span context")
		}
		got = sc
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set(HeaderName, inbound)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if got.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("inbound trace lost: got %q", got.TraceID)
	}
	if got.SpanID == "00f067aa0ba902b7" {
		t.Errorf("middleware must produce a child span; reused parent")
	}
	if w.Result().Header.Get(HeaderName) == "" {
		t.Errorf("middleware should echo traceparent on the response")
	}
}

func TestMiddleware_GeneratesWhenMissing(t *testing.T) {
	var got SpanContext
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if !got.Valid() {
		t.Errorf("middleware did not generate a valid ctx: %+v", got)
	}
}

func TestInjectInto_SetsHeader(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://upstream", nil)
	c := SpanContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: FlagSampled}
	InjectInto(r, c)
	if r.Header.Get(HeaderName) != c.Header() {
		t.Errorf("InjectInto did not set %s correctly", HeaderName)
	}
}

func TestInjectInto_IgnoresInvalid(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://upstream", nil)
	InjectInto(r, SpanContext{})
	if r.Header.Get(HeaderName) != "" {
		t.Errorf("InjectInto should not set a header for an invalid ctx")
	}
}

func TestSlogHandler_AddsTraceAttrs(t *testing.T) {
	var buf bytes.Buffer
	inner := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	logger := slog.New(NewSlogHandler(inner))

	c := SpanContext{TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", SpanID: "00f067aa0ba902b7", Flags: FlagSampled}
	ctx := WithContext(context.Background(), c)

	logger.InfoContext(ctx, "hello")
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("decode log: %v\nraw: %s", err, buf.String())
	}
	if parsed["trace_id"] != c.TraceID {
		t.Errorf("trace_id = %v, want %s", parsed["trace_id"], c.TraceID)
	}
	if parsed["span_id"] != c.SpanID {
		t.Errorf("span_id = %v, want %s", parsed["span_id"], c.SpanID)
	}
}

func TestSlogHandler_NoCtxNoAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewSlogHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	logger.Info("no ctx")
	if strings.Contains(buf.String(), "trace_id") {
		t.Errorf("trace_id appeared without a ctx: %s", buf.String())
	}
}

func TestSlogHandler_DelegatesWithAttrsAndGroup(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(NewSlogHandler(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	child := logger.With("k", "v").WithGroup("g")
	child.Info("hi")
	if !strings.Contains(buf.String(), `"k":"v"`) {
		t.Errorf("With() attrs lost: %s", buf.String())
	}
}
