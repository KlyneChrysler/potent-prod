// Package pipeline implements the protocol-agnostic core of potent: given a
// tool name and an arguments blob, decide whether to replay a cached
// response, forward to upstream and cache, or block.
//
// The HTTP and MCP adapters compose this Pipeline rather than reimplementing
// policy / fingerprint / semantic logic. The Forwarder closure hides the
// underlying transport so the pipeline is reused across HTTP reverse-proxy,
// MCP Streamable HTTP, and MCP stdio.
package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/potent/potent/internal/audit"
	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/tracing"
	"github.com/potent/potent/internal/fingerprint"
	"github.com/potent/potent/internal/metrics"
	"github.com/potent/potent/internal/normalizer"
	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/store"
)

// Decision is the outcome of evaluating a tool call against its policy.
type Decision int

const (
	DecisionForward Decision = iota
	DecisionReplay
	DecisionBlock
	DecisionRateLimited
	DecisionForbidden
)

func (d Decision) String() string {
	switch d {
	case DecisionForward:
		return "forward"
	case DecisionReplay:
		return "replay"
	case DecisionBlock:
		return "block"
	case DecisionRateLimited:
		return "rate_limited"
	case DecisionForbidden:
		return "forbidden"
	}
	return "unknown"
}

// Match describes why a Replay decision fired.
type Match struct {
	Kind       string // "exact" | "semantic" | ""
	Similarity float32
}

// Result is the pipeline's verdict and the response data to write back. For
// Forward decisions, Body is the fresh upstream response (already cached by
// the pipeline). For Replay, it's the cached response. For Block, the caller
// surfaces an error response.
type Result struct {
	Decision   Decision
	Match      Match
	Hash       string
	StatusCode int
	Body       []byte
}

// Forwarder executes the actual call to the upstream tool and returns its
// status code, response body, and an error. Implementations should respect
// the supplied context.
type Forwarder func(ctx context.Context) (int, []byte, error)

// Pipeline owns the policy + cache + embedder. Construct once per process
// and share across requests.
type Pipeline struct {
	policies *policy.Config
	store    store.Store
	embedder embed.Embedder
	metrics  *metrics.Metrics
	audit    *audit.Writer
	now      func() time.Time

	// shadow, when true, forces every Apply/Lookup decision to Forward
	// regardless of policy while still computing the would-be decision
	// and writing it to the audit log. Lets operators measure what the
	// pipeline would have done against real traffic before flipping the
	// policy on.
	shadow bool

	// inflightMu guards inflight; only Apply mutates the map.
	inflightMu sync.Mutex
	// inflight coalesces concurrent Apply calls with the same fingerprint
	// hash so a single forward() invocation serves every concurrent
	// duplicate. Without coalescing, three concurrent identical tool calls
	// would all reach upstream, defeating the whole point of idempotency.
	inflight map[string]*applyInflight

	// rlMu guards limiters; entries are created lazily on first Apply
	// for a tool that has a non-zero rate limit configured.
	rlMu     sync.Mutex
	limiters map[string]*rate.Limiter
}

// applyInflight is what concurrent siblings of a leader Apply call wait on.
// The leader fills in result + err before closing done; waiters read after.
type applyInflight struct {
	done   chan struct{}
	result Result
	err    error
}

// New constructs a Pipeline. Pass nil metrics or embedder to disable those
// concerns; the pipeline still operates correctly.
func New(p *policy.Config, st store.Store, opts ...Option) *Pipeline {
	pl := &Pipeline{
		policies: p,
		store:    st,
		now:      time.Now,
		inflight: make(map[string]*applyInflight),
		limiters: make(map[string]*rate.Limiter),
	}
	for _, opt := range opts {
		opt(pl)
	}
	return pl
}

// limiterFor returns the token-bucket limiter for tool, creating it lazily
// from policy on first use. Returns nil if rate limiting is disabled for
// the tool (RPS <= 0).
func (p *Pipeline) limiterFor(tool string, rl policy.RateLimit) *rate.Limiter {
	if rl.RPS <= 0 {
		return nil
	}
	p.rlMu.Lock()
	defer p.rlMu.Unlock()
	if l, ok := p.limiters[tool]; ok {
		return l
	}
	burst := rl.Burst
	if burst <= 0 {
		burst = int(math.Ceil(rl.RPS))
		if burst < 1 {
			burst = 1
		}
	}
	l := rate.NewLimiter(rate.Limit(rl.RPS), burst)
	p.limiters[tool] = l
	return l
}

// Option configures the Pipeline.
type Option func(*Pipeline)

func WithEmbedder(e embed.Embedder) Option  { return func(p *Pipeline) { p.embedder = e } }
func WithMetrics(m *metrics.Metrics) Option { return func(p *Pipeline) { p.metrics = m } }
func WithClock(c func() time.Time) Option   { return func(p *Pipeline) { p.now = c } }
func WithAudit(a *audit.Writer) Option      { return func(p *Pipeline) { p.audit = a } }

// WithShadow enables shadow mode. In shadow mode every tool call is
// forwarded to upstream (no replays, no blocks, no rate-limit
// rejections) but the audit log records what the policy would have
// done. Used by operators to calibrate semantic_threshold and ACLs
// against real traffic before flipping the policy on.
func WithShadow(on bool) Option { return func(p *Pipeline) { p.shadow = on } }

// Lookup evaluates policy + cache without calling upstream. Returns the
// decision (Replay/Block/Forward/RateLimited/Forbidden), the cached entry
// if applicable, and the hash/intent the caller needs to later store a
// fresh response. Caller carries the authenticated caller-id and gates
// per-tool ACLs; pass "" for anonymous mode.
//
// Forward callers (HTTP, MCP-HTTP) typically use Apply; bidirectional
// transports (MCP stdio) split lookup from caching because the response
// arrives on a separate pipe after the forwarded request.
func (p *Pipeline) Lookup(ctx context.Context, tool, caller string, body []byte) (Result, string, error) {
	pol := p.policies.For(tool)
	if pol.Mode == policy.ModeOff {
		return Result{Decision: DecisionForward}, "", nil
	}

	if !pol.AllowsCaller(caller) {
		p.recordDecision(tool, pol.Mode, DecisionForbidden)
		p.writeAudit(ctx, tool, string(pol.Mode), DecisionForbidden, Match{}, "", "")
		if p.shadow {
			return Result{Decision: DecisionForward}, "", nil
		}
		return Result{Decision: DecisionForbidden, StatusCode: 403}, "", nil
	}

	if l := p.limiterFor(tool, pol.RateLimit); l != nil && !l.Allow() {
		p.recordDecision(tool, pol.Mode, DecisionRateLimited)
		p.writeAudit(ctx, tool, string(pol.Mode), DecisionRateLimited, Match{}, "", "")
		if p.shadow {
			return Result{Decision: DecisionForward}, "", nil
		}
		return Result{Decision: DecisionRateLimited, StatusCode: 429}, "", nil
	}

	hash, intent, err := analyse(body, pol)
	if err != nil {
		return Result{}, "", fmt.Errorf("analyse: %w", err)
	}

	decision, entry, match := p.decide(ctx, tool, hash, pol, intent)
	p.recordDecision(tool, pol.Mode, decision)
	p.writeAudit(ctx, tool, string(pol.Mode), decision, match, hash, entry.Hash)

	// In shadow mode the mcp-stdio adapter must always forward to the child
	// server. The Cache call after the child responds will populate the
	// store so a future identical call surfaces as a would-replay.
	if p.shadow {
		return Result{Decision: DecisionForward, Hash: hash}, intent, nil
	}

	switch decision {
	case DecisionReplay:
		if err := p.store.IncrementReplay(ctx, tool, entry.Hash); err != nil {
			p.recordStoreError("increment_replay")
		}
		return Result{
			Decision:   DecisionReplay,
			Match:      match,
			Hash:       entry.Hash,
			StatusCode: entry.StatusCode,
			Body:       renderReplayBody(tool, entry, pol),
		}, intent, nil
	case DecisionBlock:
		return Result{Decision: DecisionBlock, Match: match, Hash: entry.Hash, StatusCode: 409}, intent, nil
	default:
		return Result{Decision: DecisionForward, Hash: hash}, intent, nil
	}
}

// Cache persists an upstream response keyed by the tool + body, computing
// the embedding when policy enables semantic matching. Intended to be
// called from bidirectional transports after a forwarded request returns
// a response on a separate pipe.
func (p *Pipeline) Cache(ctx context.Context, tool string, body []byte, status int, response []byte) error {
	pol := p.policies.For(tool)
	if pol.Mode != policy.ModeStrict && pol.Mode != policy.ModeCache {
		return nil
	}
	if status < 200 || status >= 300 {
		return nil
	}
	hash, intent, err := analyse(body, pol)
	if err != nil {
		return fmt.Errorf("analyse: %w", err)
	}
	var emb []float32
	if p.embedder != nil && pol.SemanticThreshold > 0 && intent != "" {
		if v, eerr := p.embedder.Embed(intent); eerr == nil {
			emb = v
		}
	}
	if err := p.store.Put(ctx, p.buildEntry(tool, hash, body, status, response, pol, emb)); err != nil {
		p.recordStoreError("put")
		return fmt.Errorf("store put: %w", err)
	}
	return nil
}

// Apply runs the idempotency pipeline for a tool call. Body is the raw
// JSON arguments object; forward is invoked only when the policy requires
// reaching the upstream. Caller is the authenticated caller-id (from the
// auth middleware) and is used to enforce per-tool ACLs and to label
// audit records; pass "" when running in anonymous/single-tenant mode.
func (p *Pipeline) Apply(ctx context.Context, tool, caller string, body []byte, forward Forwarder) (Result, error) {
	pol := p.policies.For(tool)
	if pol.Mode == policy.ModeOff {
		status, resp, err := forward(ctx)
		return Result{Decision: DecisionForward, StatusCode: status, Body: resp}, err
	}

	if !pol.AllowsCaller(caller) {
		p.recordDecision(tool, pol.Mode, DecisionForbidden)
		p.writeAudit(ctx, tool, string(pol.Mode), DecisionForbidden, Match{}, "", "")
		if p.shadow {
			return p.shadowForward(ctx, tool, "", "", pol, body, forward)
		}
		return Result{Decision: DecisionForbidden, StatusCode: 403}, nil
	}

	if l := p.limiterFor(tool, pol.RateLimit); l != nil && !l.Allow() {
		p.recordDecision(tool, pol.Mode, DecisionRateLimited)
		p.writeAudit(ctx, tool, string(pol.Mode), DecisionRateLimited, Match{}, "", "")
		if p.shadow {
			return p.shadowForward(ctx, tool, "", "", pol, body, forward)
		}
		return Result{Decision: DecisionRateLimited, StatusCode: 429}, nil
	}

	hash, intent, err := analyse(body, pol)
	if err != nil {
		return Result{}, fmt.Errorf("analyse: %w", err)
	}

	decision, entry, match := p.decide(ctx, tool, hash, pol, intent)
	p.recordDecision(tool, pol.Mode, decision)
	p.writeAudit(ctx, tool, string(pol.Mode), decision, match, hash, entry.Hash)

	// In shadow mode every decision becomes Forward on the wire while the
	// audit log keeps the policy's view. The cache still gets written so a
	// subsequent identical call would surface as a would-replay.
	if p.shadow {
		return p.shadowForward(ctx, tool, hash, intent, pol, body, forward)
	}

	switch decision {
	case DecisionReplay:
		if err := p.store.IncrementReplay(ctx, tool, entry.Hash); err != nil {
			p.recordStoreError("increment_replay")
		}
		return Result{
			Decision:   DecisionReplay,
			Match:      match,
			Hash:       entry.Hash,
			StatusCode: entry.StatusCode,
			Body:       renderReplayBody(tool, entry, pol),
		}, nil
	case DecisionBlock:
		return Result{Decision: DecisionBlock, Match: match, Hash: entry.Hash, StatusCode: 409}, nil
	default:
		// Coalesce concurrent duplicates so only one of N siblings actually
		// calls forward(). Strict and cache modes participate; off and
		// log_only fall through because they always forward by design.
		if pol.Mode == policy.ModeStrict || pol.Mode == policy.ModeCache {
			if res, leader, leaderHandle := p.acquireLeader(hash); !leader {
				return p.waitForLeader(ctx, res)
			} else {
				return p.runLeader(ctx, tool, hash, intent, pol, body, forward, leaderHandle)
			}
		}
		return p.doForward(ctx, tool, hash, intent, pol, body, forward, nil)
	}
}

// shadowForward forces a Forward response while still caching the upstream
// reply (so a subsequent identical call surfaces as a would-replay in the
// audit log). When the analyse stage produced no hash (because the call
// failed an earlier gate like the ACL or rate limit), the cache write is
// skipped; the audit record already noted what would have blocked.
func (p *Pipeline) shadowForward(ctx context.Context, tool, hash, intent string, pol policy.ToolPolicy, body []byte, forward Forwarder) (Result, error) {
	start := p.now()
	status, resp, err := forward(ctx)
	p.recordUpstreamLatency(tool, p.now().Sub(start).Seconds())
	if err != nil {
		return Result{Decision: DecisionForward, Hash: hash, StatusCode: status, Body: resp}, err
	}
	if hash != "" && (pol.Mode == policy.ModeStrict || pol.Mode == policy.ModeCache) && status >= 200 && status < 300 {
		var emb []float32
		if p.embedder != nil && pol.SemanticThreshold > 0 && intent != "" {
			if v, eerr := p.embedder.Embed(intent); eerr == nil {
				emb = v
			}
		}
		if perr := p.store.Put(ctx, p.buildEntry(tool, hash, body, status, resp, pol, emb)); perr != nil {
			p.recordStoreError("put")
		}
	}
	return Result{Decision: DecisionForward, Hash: hash, StatusCode: status, Body: resp}, nil
}

// buildEntry constructs the store.Entry to persist for a tool call. Two
// PII-aware policy hooks may rewrite the entry:
//
//   - RedactRequestBody drops the raw inbound bytes (the fingerprint hash
//     keeps the cache functional; only the admin debug view loses signal)
//   - ReplayStrategy == synthesized_ack drops the upstream response bytes
//     so a future replay returns the SynthesizedResponse template instead
//     of byte-for-byte forwarding the original (possibly sensitive) body
func (p *Pipeline) buildEntry(tool, hash string, body []byte, status int, resp []byte, pol policy.ToolPolicy, emb []float32) store.Entry {
	e := store.Entry{
		Tool:       tool,
		Hash:       hash,
		StatusCode: status,
		CreatedAt:  p.now(),
		TTL:        pol.TTL,
		Embedding:  emb,
	}
	if !pol.RedactRequestBody {
		e.Request = body
	}
	if pol.ReplayStrategy != policy.ReplaySynthesizedAck {
		e.Response = resp
	}
	return e
}

// renderReplayBody decides what bytes to return on a cache hit. When the
// stored entry has a response body, use it (the fast path; cached_response
// strategy). When the response is empty (synthesized_ack strategy or
// otherwise) render the configured template so a replay still returns
// something parseable.
func renderReplayBody(tool string, entry store.Entry, pol policy.ToolPolicy) []byte {
	if len(entry.Response) > 0 {
		return entry.Response
	}
	return pol.SynthesizeReplay(tool, entry.Hash)
}

// runLeader runs doForward with panic recovery. A panic in the forward
// closure is captured on the inflight handle so waiters surface it as
// an error instead of receiving a silent zero Result. The panic is
// re-raised at the end so the leader's own caller sees it.
func (p *Pipeline) runLeader(ctx context.Context, tool, hash, intent string, pol policy.ToolPolicy, body []byte, forward Forwarder, leaderHandle *applyInflight) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			leaderHandle.err = fmt.Errorf("forward panic: %v", r)
			p.releaseLeader(hash, leaderHandle)
			panic(r) // re-raise so the leader's own caller sees the panic too
		}
		p.releaseLeader(hash, leaderHandle)
	}()
	return p.doForward(ctx, tool, hash, intent, pol, body, forward, leaderHandle)
}

// acquireLeader registers an in-flight forward for the given hash. Returns
// (existing, false, nil) when a sibling is already in flight; (nil, true,
// handle) when the caller is the new leader and must call doForward.
func (p *Pipeline) acquireLeader(hash string) (*applyInflight, bool, *applyInflight) {
	p.inflightMu.Lock()
	defer p.inflightMu.Unlock()
	if existing, ok := p.inflight[hash]; ok {
		return existing, false, nil
	}
	h := &applyInflight{done: make(chan struct{})}
	p.inflight[hash] = h
	return nil, true, h
}

// releaseLeader removes the in-flight entry. Called from a defer by the
// leader so siblings that arrive after this point hit the cache instead.
func (p *Pipeline) releaseLeader(hash string, h *applyInflight) {
	p.inflightMu.Lock()
	if cur, ok := p.inflight[hash]; ok && cur == h {
		delete(p.inflight, hash)
	}
	p.inflightMu.Unlock()
	close(h.done)
}

// waitForLeader blocks until the leader's forward + cache write completes,
// then returns the leader's Result with the waiter's view of the decision.
// If the leader's forward closure failed (returned an error or panicked),
// that failure is surfaced to the waiter instead of a silent zero Result.
func (p *Pipeline) waitForLeader(ctx context.Context, leader *applyInflight) (Result, error) {
	select {
	case <-leader.done:
		if leader.err != nil {
			return Result{Decision: DecisionForward}, fmt.Errorf("leader failed: %w", leader.err)
		}
		// Borrow the leader's body/status. Mark as Replay so callers can
		// distinguish a coalesced response from a fresh forward.
		r := leader.result
		r.Decision = DecisionReplay
		r.Match = Match{Kind: "exact", Similarity: 1.0}
		return r, nil
	case <-ctx.Done():
		return Result{Decision: DecisionForward}, ctx.Err()
	}
}

// doForward calls the upstream forwarder and persists the response. When
// leaderHandle is non-nil, the result is stored on the handle for siblings
// waiting on done.
func (p *Pipeline) doForward(ctx context.Context, tool, hash, intent string, pol policy.ToolPolicy, body []byte, forward Forwarder, leaderHandle *applyInflight) (Result, error) {
	start := p.now()
	status, resp, err := forward(ctx)
	p.recordUpstreamLatency(tool, p.now().Sub(start).Seconds())
	if err != nil {
		res := Result{Decision: DecisionForward, Hash: hash, StatusCode: status, Body: resp}
		if leaderHandle != nil {
			leaderHandle.result = res
			leaderHandle.err = err
		}
		return res, err
	}
	if (pol.Mode == policy.ModeStrict || pol.Mode == policy.ModeCache) && status >= 200 && status < 300 {
		var emb []float32
		if p.embedder != nil && pol.SemanticThreshold > 0 && intent != "" {
			if v, eerr := p.embedder.Embed(intent); eerr == nil {
				emb = v
			}
		}
		if perr := p.store.Put(ctx, p.buildEntry(tool, hash, body, status, resp, pol, emb)); perr != nil {
			p.recordStoreError("put")
		}
	}
	res := Result{Decision: DecisionForward, Hash: hash, StatusCode: status, Body: resp}
	if leaderHandle != nil {
		leaderHandle.result = res
	}
	return res, nil
}

func (p *Pipeline) decide(ctx context.Context, tool, hash string, pol policy.ToolPolicy, intent string) (Decision, store.Entry, Match) {
	if pol.Mode == policy.ModeLogOnly {
		_, _ = p.store.Get(ctx, tool, hash)
		return DecisionForward, store.Entry{}, Match{}
	}
	if pol.Mode != policy.ModeStrict && pol.Mode != policy.ModeCache {
		return DecisionForward, store.Entry{}, Match{}
	}

	entry, err := p.store.Get(ctx, tool, hash)
	if err == nil {
		if pol.Mode == policy.ModeStrict && pol.RequireHumanConfirmOnReplay {
			return DecisionBlock, entry, Match{Kind: "exact", Similarity: 1.0}
		}
		return DecisionReplay, entry, Match{Kind: "exact", Similarity: 1.0}
	}
	if !errors.Is(err, store.ErrNotFound) {
		p.recordStoreError("get")
		return DecisionForward, store.Entry{}, Match{}
	}

	if p.embedder == nil || pol.SemanticThreshold <= 0 || intent == "" {
		return DecisionForward, store.Entry{}, Match{}
	}
	best, score, ok := p.semanticSearch(ctx, tool, intent, pol.SemanticThreshold)
	if !ok {
		return DecisionForward, store.Entry{}, Match{}
	}
	if pol.Mode == policy.ModeStrict && pol.RequireHumanConfirmOnReplay {
		return DecisionBlock, best, Match{Kind: "semantic", Similarity: score}
	}
	return DecisionReplay, best, Match{Kind: "semantic", Similarity: score}
}

func (p *Pipeline) semanticSearch(ctx context.Context, tool, intent string, threshold float64) (store.Entry, float32, bool) {
	queryVec, err := p.embedder.Embed(intent)
	if err != nil {
		return store.Entry{}, 0, false
	}
	var best store.Entry
	var bestScore float32
	found := false
	err = p.store.Scan(ctx, tool, func(e store.Entry) bool {
		if len(e.Embedding) == 0 {
			return true
		}
		score, cerr := embed.Cosine(queryVec, e.Embedding)
		if cerr != nil {
			return true
		}
		if score >= float32(threshold) && score > bestScore {
			best = e
			bestScore = score
			found = true
		}
		return true
	})
	if err != nil {
		p.recordStoreError("scan")
		return store.Entry{}, 0, false
	}
	return best, bestScore, found
}

func (p *Pipeline) recordDecision(tool string, mode policy.Mode, d Decision) {
	if p.metrics == nil {
		return
	}
	p.metrics.Decisions.WithLabelValues(tool, string(mode), d.String()).Inc()
}

func (p *Pipeline) recordStoreError(op string) {
	if p.metrics == nil {
		return
	}
	p.metrics.StoreErrors.WithLabelValues(op).Inc()
}

func (p *Pipeline) writeAudit(ctx context.Context, tool, mode string, d Decision, match Match, freshHash, entryHash string) {
	if p.audit == nil {
		return
	}
	h := entryHash
	if h == "" {
		h = freshHash
	}
	rec := audit.Record{
		Tool:       tool,
		Mode:       mode,
		Decision:   d.String(),
		Match:      match.Kind,
		Similarity: match.Similarity,
		Hash:       h,
	}
	if sc, ok := tracing.FromContext(ctx); ok && sc.Valid() {
		rec.TraceID = sc.TraceID
	}
	if p.shadow {
		// In shadow mode every call is forwarded regardless of policy. The
		// pipeline's choice (d) becomes WouldDecision; Decision reflects
		// what actually happened on the wire.
		rec.Shadow = true
		rec.WouldDecision = d.String()
		rec.Decision = DecisionForward.String()
	}
	p.audit.Write(rec)
}

func (p *Pipeline) recordUpstreamLatency(tool string, seconds float64) {
	if p.metrics == nil {
		return
	}
	p.metrics.UpstreamLatency.WithLabelValues(tool).Observe(seconds)
}

// analyse normalizes args and produces (hash, intent). body must be a JSON
// object; empty body is treated as an empty object.
func analyse(body []byte, pol policy.ToolPolicy) (string, string, error) {
	var raw map[string]any
	if len(body) == 0 {
		h, err := fingerprint.Exact(map[string]any{})
		return h, "", err
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", "", fmt.Errorf("body must be a JSON object: %w", err)
	}

	for field, transforms := range pol.Normalize {
		if v, ok := raw[field].(string); ok {
			raw[field] = normalizer.Apply(v, transforms)
		}
	}

	selected := raw
	if len(pol.FingerprintFields) > 0 {
		filtered := make(map[string]any, len(pol.FingerprintFields))
		for _, f := range pol.FingerprintFields {
			if v, ok := raw[f]; ok {
				filtered[f] = v
			}
		}
		selected = filtered
	}

	hash, err := fingerprint.Exact(selected)
	if err != nil {
		return "", "", err
	}
	return hash, buildIntent(selected), nil
}

func buildIntent(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b bytes.Buffer
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		writeScalar(&b, m[k])
	}
	return b.String()
}

func writeScalar(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case string:
		b.WriteString(x)
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case nil:
		b.WriteString("null")
	default:
		_ = json.NewEncoder(b).Encode(x)
	}
}
