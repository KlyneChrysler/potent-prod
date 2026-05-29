// Package tracing implements just enough of the W3C Trace Context
// specification (https://www.w3.org/TR/trace-context/) for potent to
// correlate inbound and outbound HTTP traffic with whatever distributed-
// tracing backend the operator runs (Datadog, Honeycomb, Jaeger, Tempo,
// etc.). We do not bring in the full opentelemetry-go SDK because the
// dependency surface is large and the operator's choice of exporter is
// project-policy; a four-field header plus a slog attribute handler is
// enough to let the operator's existing tracing stack do the correlation.
//
// When the OTEL SDK becomes a project-policy decision, this package can
// be deleted in favor of the SDK-emitted spans; the W3C header shape and
// audit-log field name stay stable so downstream consumers do not break.
package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
)

// Version is the only W3C trace-context version potent emits. The spec
// (section 3.2.2.1) permits older receivers to ignore unknown versions
// but the field is fixed for forward compatibility.
const Version = "00"

// FlagSampled is the only flag bit defined by the W3C spec today
// (section 3.2.2.4). When set the operator's collector should keep the
// span; when unset it may drop it. potent propagates whatever the
// inbound caller chose and defaults to sampled on synthesized contexts
// so a self-originated request still shows up in the tracing backend.
const FlagSampled byte = 0x01

// SpanContext is the minimal carrier the W3C header serializes. We hold
// the bytes in hex form so they go straight into headers and audit
// records without re-encoding on every emission.
type SpanContext struct {
	TraceID string // 32 hex chars (16 bytes)
	SpanID  string // 16 hex chars (8 bytes)
	Flags   byte   // sample bit; other bits are reserved
}

// Header renders the W3C traceparent value for outbound propagation.
func (c SpanContext) Header() string {
	return fmt.Sprintf("%s-%s-%s-%02x", Version, c.TraceID, c.SpanID, c.Flags)
}

// Valid reports whether the context is well-formed enough to propagate.
// An empty SpanContext and one with the all-zero trace or span id are
// invalid per the spec.
func (c SpanContext) Valid() bool {
	if len(c.TraceID) != 32 || len(c.SpanID) != 16 {
		return false
	}
	if c.TraceID == "00000000000000000000000000000000" || c.SpanID == "0000000000000000" {
		return false
	}
	for _, r := range c.TraceID + c.SpanID {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// Parse reads an inbound W3C traceparent header value. The lowercase
// requirement on hex chars is from the spec; we strip leading/trailing
// whitespace because middleware sometimes injects it. Returns an empty
// SpanContext + ok=false on any malformed input so callers can fall back
// to generating a fresh context.
func Parse(header string) (SpanContext, bool) {
	parts := strings.Split(strings.TrimSpace(header), "-")
	if len(parts) != 4 {
		return SpanContext{}, false
	}
	if parts[0] != Version {
		return SpanContext{}, false
	}
	flagsBytes, err := hex.DecodeString(parts[3])
	if err != nil || len(flagsBytes) != 1 {
		return SpanContext{}, false
	}
	c := SpanContext{TraceID: parts[1], SpanID: parts[2], Flags: flagsBytes[0]}
	if !c.Valid() {
		return SpanContext{}, false
	}
	return c, true
}

// New generates a fresh SpanContext using crypto/rand for the trace and
// span ids. Defaults to sampled.
func New() (SpanContext, error) {
	return generate(rand.Read)
}

// NewChild keeps the inbound trace-id (so backends see one trace across
// the whole request) and generates a fresh span-id for this hop.
func NewChild(parent SpanContext) (SpanContext, error) {
	span, err := randomSpanID(rand.Read)
	if err != nil {
		return SpanContext{}, err
	}
	return SpanContext{TraceID: parent.TraceID, SpanID: span, Flags: parent.Flags}, nil
}

// generate is split out so tests can inject a deterministic reader.
func generate(readFn func([]byte) (int, error)) (SpanContext, error) {
	trace := make([]byte, 16)
	span := make([]byte, 8)
	if _, err := readFn(trace); err != nil {
		return SpanContext{}, fmt.Errorf("trace id: %w", err)
	}
	if _, err := readFn(span); err != nil {
		return SpanContext{}, fmt.Errorf("span id: %w", err)
	}
	// Guard against the (vanishingly unlikely) all-zero result, which the
	// spec marks invalid; the loop is here so we never propagate it.
	for isAllZero(trace) {
		if _, err := readFn(trace); err != nil {
			return SpanContext{}, fmt.Errorf("trace id retry: %w", err)
		}
	}
	for isAllZero(span) {
		if _, err := readFn(span); err != nil {
			return SpanContext{}, fmt.Errorf("span id retry: %w", err)
		}
	}
	return SpanContext{
		TraceID: hex.EncodeToString(trace),
		SpanID:  hex.EncodeToString(span),
		Flags:   FlagSampled,
	}, nil
}

func randomSpanID(readFn func([]byte) (int, error)) (string, error) {
	b := make([]byte, 8)
	if _, err := readFn(b); err != nil {
		return "", err
	}
	for isAllZero(b) {
		if _, err := readFn(b); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(b), nil
}

func isAllZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
