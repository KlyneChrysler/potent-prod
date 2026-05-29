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
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/potent/potent/internal/audit"
	"github.com/potent/potent/internal/embed"
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
)

func (d Decision) String() string {
	switch d {
	case DecisionForward:
		return "forward"
	case DecisionReplay:
		return "replay"
	case DecisionBlock:
		return "block"
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

	// inflightMu guards inflight; only Apply mutates the map.
	inflightMu sync.Mutex
	// inflight coalesces concurrent Apply calls with the same fingerprint
	// hash so a single forward() invocation serves every concurrent
	// duplicate. Without coalescing, three concurrent identical tool calls
	// would all reach upstream, defeating the whole point of idempotency.
	inflight map[string]*applyInflight
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
	}
	for _, opt := range opts {
		opt(pl)
	}
	return pl
}

// Option configures the Pipeline.
type Option func(*Pipeline)

func WithEmbedder(e embed.Embedder) Option  { return func(p *Pipeline) { p.embedder = e } }
func WithMetrics(m *metrics.Metrics) Option { return func(p *Pipeline) { p.metrics = m } }
func WithClock(c func() time.Time) Option   { return func(p *Pipeline) { p.now = c } }
func WithAudit(a *audit.Writer) Option      { return func(p *Pipeline) { p.audit = a } }

// Lookup evaluates policy + cache without calling upstream. Returns the
// decision (Replay/Block/Forward), the cached entry if applicable, and the
// hash/intent the caller needs to later store a fresh response.
//
// Forward callers (HTTP, MCP-HTTP) typically use Apply; bidirectional
// transports (MCP stdio) split lookup from caching because the response
// arrives on a separate pipe after the forwarded request.
func (p *Pipeline) Lookup(ctx context.Context, tool string, body []byte) (Result, string, error) {
	pol := p.policies.For(tool)
	if pol.Mode == policy.ModeOff {
		return Result{Decision: DecisionForward}, "", nil
	}

	hash, intent, err := analyse(body, pol)
	if err != nil {
		return Result{}, "", fmt.Errorf("analyse: %w", err)
	}

	decision, entry, match := p.decide(ctx, tool, hash, pol, intent)
	p.recordDecision(tool, pol.Mode, decision)
	p.writeAudit(tool, string(pol.Mode), decision, match, hash, entry.Hash)

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
			Body:       entry.Response,
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
	if err := p.store.Put(ctx, store.Entry{
		Tool:       tool,
		Hash:       hash,
		Request:    body,
		Response:   response,
		StatusCode: status,
		CreatedAt:  p.now(),
		TTL:        pol.TTL,
		Embedding:  emb,
	}); err != nil {
		p.recordStoreError("put")
		return fmt.Errorf("store put: %w", err)
	}
	return nil
}

// Apply runs the idempotency pipeline for a tool call. Body is the raw
// JSON arguments object; forward is invoked only when the policy requires
// reaching the upstream.
func (p *Pipeline) Apply(ctx context.Context, tool string, body []byte, forward Forwarder) (Result, error) {
	pol := p.policies.For(tool)
	if pol.Mode == policy.ModeOff {
		status, resp, err := forward(ctx)
		return Result{Decision: DecisionForward, StatusCode: status, Body: resp}, err
	}

	hash, intent, err := analyse(body, pol)
	if err != nil {
		return Result{}, fmt.Errorf("analyse: %w", err)
	}

	decision, entry, match := p.decide(ctx, tool, hash, pol, intent)
	p.recordDecision(tool, pol.Mode, decision)
	p.writeAudit(tool, string(pol.Mode), decision, match, hash, entry.Hash)

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
			Body:       entry.Response,
		}, nil
	case DecisionBlock:
		return Result{Decision: DecisionBlock, Match: match, Hash: entry.Hash, StatusCode: 409}, nil
	default:
		// Coalesce concurrent duplicates so only one of N siblings actually
		// calls forward(). Strict and cache modes participate; off and
		// log_only fall through because they always forward by design.
		if pol.Mode == policy.ModeStrict || pol.Mode == policy.ModeCache {
			if res, leader, leaderHandle := p.acquireLeader(hash); !leader {
				return p.waitForLeader(ctx, res), nil
			} else {
				defer p.releaseLeader(hash, leaderHandle)
				return p.doForward(ctx, tool, hash, intent, pol, body, forward, leaderHandle)
			}
		}
		return p.doForward(ctx, tool, hash, intent, pol, body, forward, nil)
	}
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
func (p *Pipeline) waitForLeader(ctx context.Context, leader *applyInflight) Result {
	select {
	case <-leader.done:
		// Borrow the leader's body/status. Mark as Replay so callers can
		// distinguish a coalesced response from a fresh forward.
		r := leader.result
		r.Decision = DecisionReplay
		r.Match = Match{Kind: "exact", Similarity: 1.0}
		return r
	case <-ctx.Done():
		return Result{Decision: DecisionForward}
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
		perr := p.store.Put(ctx, store.Entry{
			Tool:       tool,
			Hash:       hash,
			Request:    body,
			Response:   resp,
			StatusCode: status,
			CreatedAt:  p.now(),
			TTL:        pol.TTL,
			Embedding:  emb,
		})
		if perr != nil {
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

func (p *Pipeline) writeAudit(tool, mode string, d Decision, match Match, freshHash, entryHash string) {
	if p.audit == nil {
		return
	}
	h := entryHash
	if h == "" {
		h = freshHash
	}
	p.audit.Write(audit.Record{
		Tool:       tool,
		Mode:       mode,
		Decision:   d.String(),
		Match:      match.Kind,
		Similarity: match.Similarity,
		Hash:       h,
	})
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
