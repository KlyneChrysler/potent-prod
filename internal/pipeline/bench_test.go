package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/store"
)

// BenchmarkApply_ExactReplay measures the cache-hit hot path:
// fingerprint compute + store lookup + audit emit, with no upstream call.
// This is the dominant case in production once the cache is warm.
func BenchmarkApply_ExactReplay(b *testing.B) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to", "subject", "body"}},
	}}
	pl := New(cfg, store.NewMemory(nil))
	body := []byte(`{"to":"alice@example.com","subject":"hello","body":"world"}`)
	forward := func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"ok":true}`), nil
	}
	// warm the cache
	_, _ = pl.Apply(context.Background(), "send_email", "", body, forward)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = pl.Apply(context.Background(), "send_email", "", body, forward)
	}
}

// BenchmarkApply_ForwardMiss measures the cold path: fingerprint + miss
// + forward + cache write. Forward is a no-op closure so the benchmark
// isolates potent's overhead from real upstream latency.
func BenchmarkApply_ForwardMiss(b *testing.B) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to", "subject"}},
	}}
	st := store.NewMemory(nil)
	pl := New(cfg, st)
	forward := func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"ok":true}`), nil
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		// distinct payload per iter so every call is a miss
		body := []byte(fmt.Sprintf(`{"to":"a%d@x","subject":"s"}`, i))
		_, _ = pl.Apply(context.Background(), "send_email", "", body, forward)
	}
}

// BenchmarkAnalyse isolates the pure-function pipeline core (normalize +
// canonical-json + sha256). Useful for tracking regressions in the
// fingerprint hot loop independent of store and audit.
func BenchmarkAnalyse(b *testing.B) {
	pol := policy.ToolPolicy{
		FingerprintFields: []string{"to", "subject", "body"},
		Normalize: map[string][]string{
			"to":      {"lowercase", "trim"},
			"subject": {"trim", "collapse_whitespace"},
		},
	}
	body := []byte(`{"to":"  ALICE@EXAMPLE.COM  ","subject":"  hello   world  ","body":"some text"}`)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = analyse(body, pol)
	}
}
