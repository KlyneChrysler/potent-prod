package pipeline

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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

	res1, err := pl.Apply(context.Background(), "send_email", "", []byte(`{"to":"a@b.com"}`), forward)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if res1.Decision != DecisionForward {
		t.Errorf("first decision = %v, want Forward", res1.Decision)
	}

	res2, err := pl.Apply(context.Background(), "send_email", "", []byte(`{"to":"a@b.com"}`), forward)
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
	_, _ = pl.Apply(context.Background(), "t", "", []byte(`{"x":1}`), forward)
	_, _ = pl.Apply(context.Background(), "t", "", []byte(`{"x":1}`), forward)
	if calls != 2 {
		t.Errorf("off mode should forward both, got %d", calls)
	}
}

func TestApply_InvalidJSONErrors(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{"t": {Mode: policy.ModeStrict, TTL: time.Hour}}}
	pl := New(cfg, store.NewMemory(nil))
	_, err := pl.Apply(context.Background(), "t", "", []byte("not json"), func(ctx context.Context) (int, []byte, error) {
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

	_, _ = pl.Apply(context.Background(), "email", "", []byte(first), func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"id":1}`), nil
	})
	res, err := pl.Apply(context.Background(), "email", "", []byte(near), func(ctx context.Context) (int, []byte, error) {
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
	_, _ = pl.Apply(context.Background(), "delete_user", "", body, func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"deleted":true}`), nil
	})
	res, err := pl.Apply(context.Background(), "delete_user", "", body, func(ctx context.Context) (int, []byte, error) {
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
	res, intent, err := pl.Lookup(context.Background(), "t", "", []byte(`{"x":1}`))
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
	res, _, err := pl.Lookup(context.Background(), "t", "", []byte(`{"x":1}`))
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
	if _, _, err := pl.Lookup(context.Background(), "t", "", []byte("not json")); err == nil {
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

	res, _, _ := pl.Lookup(context.Background(), "t", "", body)
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
	res, _, _ := pl.Lookup(context.Background(), "t", "", []byte(`{"x":1}`))
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
	_, err := pl.Apply(context.Background(), "t", "", []byte(`{"x":1}`), func(ctx context.Context) (int, []byte, error) {
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
	_, _ = pl.Apply(context.Background(), "t", "", []byte(`{"x":1}`), forward)
	_, _ = pl.Apply(context.Background(), "t", "", []byte(`{"x":1}`), forward)
	if calls != 2 {
		t.Errorf("log_only must forward both, got %d", calls)
	}
}

func TestBuildIntent_HandlesScalarTypes(t *testing.T) {
	pl := New(&policy.Config{Tools: map[string]policy.ToolPolicy{
		"t": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"s", "n", "b", "z"}},
	}}, store.NewMemory(nil))
	// Exercise writeScalar across all branches.
	_, _ = pl.Apply(context.Background(), "t", "", []byte(`{"s":"x","n":3.14,"b":true,"z":null}`), func(ctx context.Context) (int, []byte, error) {
		return 200, []byte("ok"), nil
	})
}

func TestApply_ForbidsCallerNotInAllowList(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {
			Mode:              policy.ModeStrict,
			TTL:               time.Hour,
			FingerprintFields: []string{"to"},
			AllowedCallers:    []string{"team-eng", "team-finance"},
		},
	}}
	pl := New(cfg, store.NewMemory(nil))
	forward := func(ctx context.Context) (int, []byte, error) {
		t.Errorf("forward should not be called when caller is forbidden")
		return 0, nil, nil
	}

	r, err := pl.Apply(context.Background(), "send_email", "team-marketing", []byte(`{"to":"a@b.com"}`), forward)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if r.Decision != DecisionForbidden || r.StatusCode != 403 {
		t.Errorf("got %v/%d, want Forbidden/403", r.Decision, r.StatusCode)
	}
}

func TestApply_AllowsCallerInAllowList(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {
			Mode:              policy.ModeStrict,
			TTL:               time.Hour,
			FingerprintFields: []string{"to"},
			AllowedCallers:    []string{"team-eng"},
		},
	}}
	pl := New(cfg, store.NewMemory(nil))
	calls := 0
	forward := func(ctx context.Context) (int, []byte, error) {
		calls++
		return 200, []byte("{}"), nil
	}
	r, err := pl.Apply(context.Background(), "send_email", "team-eng", []byte(`{"to":"a@b.com"}`), forward)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if r.Decision != DecisionForward {
		t.Errorf("allowed caller should forward, got %v", r.Decision)
	}
	if calls != 1 {
		t.Errorf("forward not called")
	}
}

func TestApply_EmptyAllowListMeansAnyCaller(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	pl := New(cfg, store.NewMemory(nil))
	forward := func(ctx context.Context) (int, []byte, error) { return 200, []byte("{}"), nil }

	// no caller (anonymous, single-token mode)
	r, _ := pl.Apply(context.Background(), "send_email", "", []byte(`{"to":"a@b.com"}`), forward)
	if r.Decision != DecisionForward {
		t.Errorf("anonymous caller with empty allow list should forward, got %v", r.Decision)
	}
	// arbitrary caller (multi-tenant with no per-tool gate)
	r, _ = pl.Apply(context.Background(), "send_email", "team-marketing", []byte(`{"to":"b@b.com"}`), forward)
	if r.Decision != DecisionForward {
		t.Errorf("arbitrary caller with empty allow list should forward, got %v", r.Decision)
	}
}

func TestDecision_ForbiddenString(t *testing.T) {
	if DecisionForbidden.String() != "forbidden" {
		t.Errorf("got %q", DecisionForbidden.String())
	}
}

func TestApply_RateLimitReturns429WhenExceeded(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {
			Mode:              policy.ModeStrict,
			TTL:               time.Hour,
			FingerprintFields: []string{"to"},
			RateLimit:         policy.RateLimit{RPS: 2, Burst: 1},
		},
	}}
	pl := New(cfg, store.NewMemory(nil))
	forward := func(ctx context.Context) (int, []byte, error) {
		return 200, []byte(`{"ok":true}`), nil
	}

	// burst is 1, so the second back-to-back call exceeds the limit
	body1 := []byte(`{"to":"a@b.com"}`)
	body2 := []byte(`{"to":"b@b.com"}`) // different intent so the cache does not absorb it
	r1, err := pl.Apply(context.Background(), "send_email", "", body1, forward)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if r1.Decision != DecisionForward {
		t.Errorf("first call should pass through, got %v", r1.Decision)
	}
	r2, err := pl.Apply(context.Background(), "send_email", "", body2, forward)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if r2.Decision != DecisionRateLimited {
		t.Errorf("second call should be rate-limited, got %v (status %d)", r2.Decision, r2.StatusCode)
	}
	if r2.StatusCode != 429 {
		t.Errorf("rate-limited status = %d, want 429", r2.StatusCode)
	}
}

func TestApply_RateLimitZeroDisablesCheck(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	pl := New(cfg, store.NewMemory(nil))
	calls := 0
	forward := func(ctx context.Context) (int, []byte, error) {
		calls++
		return 200, []byte("{}"), nil
	}
	for i := 0; i < 100; i++ {
		body := []byte(fmt.Sprintf(`{"to":"caller-%d@b.com"}`, i))
		_, _ = pl.Apply(context.Background(), "send_email", "", body, forward)
	}
	if calls != 100 {
		t.Errorf("RPS=0 should disable rate limiting; got %d forwards out of 100", calls)
	}
}

func TestDecision_RateLimitedString(t *testing.T) {
	if DecisionRateLimited.String() != "rate_limited" {
		t.Errorf("DecisionRateLimited.String() = %q, want rate_limited", DecisionRateLimited.String())
	}
}

func TestApply_LeaderPanicDoesNotDeadlockWaiters(t *testing.T) {
	// If the leader's forward closure panics, every coalesced sibling waiting
	// on its done channel must receive a clean error response instead of
	// hanging forever or seeing a zero Result.
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	pl := New(cfg, store.NewMemory(nil))

	release := make(chan struct{})
	var leaderEntered int32
	forward := func(ctx context.Context) (int, []byte, error) {
		if atomic.AddInt32(&leaderEntered, 1) == 1 {
			<-release
			panic("simulated forward panic")
		}
		t.Errorf("siblings should not call forward; the leader's panic should propagate")
		return 0, nil, nil
	}

	body := []byte(`{"to":"a@b.com"}`)
	type outcome struct {
		res Result
		err error
	}
	results := make([]outcome, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results[i] = outcome{err: fmt.Errorf("propagated panic: %v", r)}
				}
			}()
			res, err := pl.Apply(context.Background(), "send_email", "", body, forward)
			results[i] = outcome{res: res, err: err}
		}(i)
	}

	time.Sleep(50 * time.Millisecond) // let siblings queue as waiters
	close(release)
	wg.Wait()

	// At least one of the three goroutines should report an error (the leader's
	// panic or a "leader failed" error to waiters). None should silently
	// return a zero Result with no signal.
	failures := 0
	for i, o := range results {
		if o.err != nil {
			failures++
			continue
		}
		if o.res.StatusCode == 0 && len(o.res.Body) == 0 {
			t.Errorf("result[%d] is a silent zero Result: callers cannot tell forward failed", i)
		}
	}
	if failures == 0 {
		t.Errorf("expected at least one goroutine to surface the leader's panic, got none")
	}
}

func TestApply_CoalescesConcurrentDuplicates(t *testing.T) {
	// Three concurrent Apply calls with the same body and tool should result
	// in exactly one forward() invocation; the other two should receive the
	// leader's response without their own forward closures being called.
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	pl := New(cfg, store.NewMemory(nil))

	var forwardCalls int32
	// gate the leader's forward so all three Apply calls overlap in flight
	release := make(chan struct{})
	forward := func(ctx context.Context) (int, []byte, error) {
		n := atomic.AddInt32(&forwardCalls, 1)
		if n == 1 {
			<-release // block leader until siblings have arrived
		}
		return 200, []byte(`{"id":"x"}`), nil
	}

	body := []byte(`{"to":"a@b.com"}`)
	results := make([]Result, 3)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, _ := pl.Apply(context.Background(), "send_email", "", body, forward)
			results[i] = res
		}(i)
	}

	// give siblings a moment to enter Apply and find the leader's inflight
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&forwardCalls); got != 1 {
		t.Errorf("forward called %d times, want 1", got)
	}
	for i, r := range results {
		if r.StatusCode != 200 {
			t.Errorf("result[%d] status = %d", i, r.StatusCode)
		}
		if string(r.Body) != `{"id":"x"}` {
			t.Errorf("result[%d] body = %s", i, r.Body)
		}
	}
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
