package pipeline

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/metrics"
	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/store"
)

func metricsTestRegistry(t *testing.T) *metrics.Metrics {
	t.Helper()
	return metrics.New(prometheus.NewRegistry())
}

func TestApply_StrictReplaysExactDuplicate(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	pl := New(cfg, store.NewMemory(nil))

	calls := 0
	forward := func(ctx context.Context) (int, []byte, error) {
		calls++
		return 200, []byte(`{"id":"x"}`), nil
	}

	res1, err := pl.Apply(context.Background(), "send_email", []byte(`{"to":"a@b.com"}`), forward)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if res1.Decision != DecisionForward {
		t.Errorf("first decision = %v, want Forward", res1.Decision)
	}

	res2, err := pl.Apply(context.Background(), "send_email", []byte(`{"to":"a@b.com"}`), forward)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res2.Decision != DecisionReplay {
		t.Errorf("second decision = %v, want Replay", res2.Decision)
	}
	if res2.Match.Kind != "exact" {
		t.Errorf("expected exact match, got %q", res2.Match.Kind)
	}
	if calls != 1 {
		t.Errorf("forward called %d times, want 1", calls)
	}
}

func TestApply_OffModeAlwaysForwards(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"t": {Mode: policy.ModeOff}}}
	pl := New(cfg, store.NewMemory(nil))
	calls := 0
	forward := func(ctx context.Context) (int, []byte, error) {
		calls++
		return 200, []byte("ok"), nil
	}
	_, _ = pl.Apply(context.Background(), "t", []byte(`{"x":1}`), forward)
	_, _ = pl.Apply(context.Background(), "t", []byte(`{"x":1}`), forward)
	if calls != 2 {
		t.Errorf("off mode should forward both, got %d", calls)
	}
}

func TestApply_InvalidJSONErrors(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"t": {Mode: policy.ModeStrict, TTL: time.Hour}}}
	pl := New(cfg, store.NewMemory(nil))
	_, err := pl.Apply(context.Background(), "t", []byte("not json"), func(ctx context.Context) (int, []byte, error) {
		return 200, nil, nil
	})
	if err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestApply_SemanticReplay(t *testing.T) {
	emb, _ := embed.NewHashingTFIDF(384, 4)
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"email": {
			Mode:              policy.ModeStrict,
			TTL:               time.Hour,
			FingerprintFields: []string{"to", "subject", "body"},
			SemanticThreshold: 0.9,
		},
	}}
	pl := New(cfg, store.NewMemory(nil), WithEmbedder(emb))

	first := `{"to":"alice@example.com","subject":"Q3","body":"Please find attached the Q3 financial report for your review."}`
	near := `{"to":"alice@example.com","subject":"Q3","body":"Please find attached the Q3 financial report for review."}`

	_, _ = pl.Apply(context.Background(), "email", []byte(first), func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"id":1}`), nil
	})
	res, err := pl.Apply(context.Background(), "email", []byte(near), func(ctx context.Context) (int, []byte, error) {
		t.Errorf("forward should not be called for semantic replay")
		return 200, nil, nil
	})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Decision != DecisionReplay || res.Match.Kind != "semantic" {
		t.Errorf("got decision=%v match=%q, want replay/semantic", res.Decision, res.Match.Kind)
	}
}

func TestApply_BlockOnRequireHumanConfirm(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"delete_user": {
			Mode:                        policy.ModeStrict,
			TTL:                         time.Hour,
			FingerprintFields:           []string{"user_id"},
			RequireHumanConfirmOnReplay: true,
		},
	}}
	pl := New(cfg, store.NewMemory(nil))
	body := []byte(`{"user_id":"42"}`)
	_, _ = pl.Apply(context.Background(), "delete_user", body, func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"deleted":true}`), nil
	})
	res, err := pl.Apply(context.Background(), "delete_user", body, func(ctx context.Context) (int, []byte, error) {
		t.Errorf("forward should not be called when blocking")
		return 200, nil, nil
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Decision != DecisionBlock {
		t.Errorf("decision = %v, want Block", res.Decision)
	}
}

func TestLookup_ReturnsForwardOnMiss(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"x"}},
	}}
	pl := New(cfg, store.NewMemory(nil))
	res, intent, err := pl.Lookup(context.Background(), "t", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Decision != DecisionForward {
		t.Errorf("decision = %v, want Forward", res.Decision)
	}
	if res.Hash == "" {
		t.Errorf("hash empty")
	}
	if intent == "" {
		t.Errorf("intent empty")
	}
}

func TestLookup_OffMode(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"t": {Mode: policy.ModeOff}}}
	pl := New(cfg, store.NewMemory(nil))
	res, _, err := pl.Lookup(context.Background(), "t", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if res.Decision != DecisionForward {
		t.Errorf("decision = %v, want Forward", res.Decision)
	}
}

func TestLookup_InvalidJSON(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"t": {Mode: policy.ModeStrict, TTL: time.Hour}}}
	pl := New(cfg, store.NewMemory(nil))
	if _, _, err := pl.Lookup(context.Background(), "t", []byte("not json")); err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestCache_StoresSuccessResponse(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"x"}},
	}}
	st := store.NewMemory(nil)
	pl := New(cfg, st)
	body := []byte(`{"x":1}`)
	if err := pl.Cache(context.Background(), "t", body, 200, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("Cache: %v", err)
	}

	res, _, _ := pl.Lookup(context.Background(), "t", body)
	if res.Decision != DecisionReplay {
		t.Errorf("expected cached entry replays, got %v", res.Decision)
	}
}

func TestCache_SkipsNon2xx(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"x"}},
	}}
	pl := New(cfg, store.NewMemory(nil))
	if err := pl.Cache(context.Background(), "t", []byte(`{"x":1}`), 500, []byte(`fail`)); err != nil {
		t.Fatalf("Cache: %v", err)
	}
	res, _, _ := pl.Lookup(context.Background(), "t", []byte(`{"x":1}`))
	if res.Decision != DecisionForward {
		t.Errorf("5xx should not be cached, got %v", res.Decision)
	}
}

func TestCache_SkipsOffMode(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"t": {Mode: policy.ModeOff}}}
	pl := New(cfg, store.NewMemory(nil))
	if err := pl.Cache(context.Background(), "t", []byte(`{"x":1}`), 200, []byte(`{}`)); err != nil {
		t.Fatalf("Cache: %v", err)
	}
}

func TestWithMetricsOption(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"x"}},
	}}
	m := metricsTestRegistry(t)
	pl := New(cfg, store.NewMemory(nil), WithMetrics(m))
	_, err := pl.Apply(context.Background(), "t", []byte(`{"x":1}`), func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"ok":true}`), nil
	})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestWithClockOption(t *testing.T) {
	fixed := time.Unix(1_700_000_000, 0)
	cfg := &policy.Config{}
	pl := New(cfg, store.NewMemory(nil), WithClock(func() time.Time { return fixed }))
	if pl.now().Unix() != fixed.Unix() {
		t.Errorf("WithClock did not apply")
	}
}

func TestCache_EmptyBodyOK(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour},
	}}
	pl := New(cfg, store.NewMemory(nil))
	if err := pl.Cache(context.Background(), "t", []byte{}, 200, []byte(`{}`)); err != nil {
		t.Errorf("empty body cache failed: %v", err)
	}
}

func TestCache_InvalidJSONReturnsError(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour},
	}}
	pl := New(cfg, store.NewMemory(nil))
	if err := pl.Cache(context.Background(), "t", []byte("not json"), 200, []byte(`{}`)); err == nil {
		t.Errorf("expected error for invalid JSON")
	}
}

func TestApply_LogOnlyForwardsButChecksCache(t *testing.T) {
	cfg := &policy.Config{Defaults: policy.ToolPolicy{Mode: policy.ModeLogOnly, TTL: time.Hour}}
	pl := New(cfg, store.NewMemory(nil))
	calls := 0
	forward := func(ctx context.Context) (int, []byte, error) {
		calls++
		return 200, []byte("ok"), nil
	}
	_, _ = pl.Apply(context.Background(), "t", []byte(`{"x":1}`), forward)
	_, _ = pl.Apply(context.Background(), "t", []byte(`{"x":1}`), forward)
	if calls != 2 {
		t.Errorf("log_only must forward both, got %d", calls)
	}
}

func TestBuildIntent_HandlesScalarTypes(t *testing.T) {
	pl := New(&policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"s", "n", "b", "z"}},
	}}, store.NewMemory(nil))
	// Exercise writeScalar across all branches.
	_, _ = pl.Apply(context.Background(), "t", []byte(`{"s":"x","n":3.14,"b":true,"z":null}`), func(ctx context.Context) (int, []byte, error) {
		return 200, []byte("ok"), nil
	})
}

func TestDecision_String(t *testing.T) {
	cases := []struct {
		d    Decision
		want string
	}{
		{DecisionForward, "forward"},
		{DecisionReplay, "replay"},
		{DecisionBlock, "block"},
		{Decision(99), "unknown"},
	}
	for _, c := range cases {
		if c.d.String() != c.want {
			t.Errorf("Decision(%d).String() = %q, want %q", c.d, c.d.String(), c.want)
		}
	}
}
