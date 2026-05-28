// Package proxy implements the HTTP reverse-proxy adapter that enforces
// per-tool idempotency policies on inbound tool calls.
//
// The proxy reads X-Potent-Tool from the request, looks up the matching
// policy, normalizes JSON-body fields, computes an exact-match fingerprint,
// optionally evaluates a semantic fallback over an Embedder, and either
// replays a cached response or forwards to the upstream tool server.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/fingerprint"
	"github.com/potent/potent/internal/metrics"
	"github.com/potent/potent/internal/normalizer"
	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/store"
)

// ToolHeader is the request header the client sets to tell the proxy which
// tool is being invoked. The header determines which policy applies.
const ToolHeader = "X-Potent-Tool"

// Decision is the outcome of evaluating a tool call against its policy.
type Decision int

const (
	// DecisionForward means the proxy forwards the call upstream as-is.
	DecisionForward Decision = iota
	// DecisionReplay means the proxy serves a cached response without
	// hitting the upstream tool server.
	DecisionReplay
	// DecisionBlock means the proxy refuses the call (strict mode, no
	// cached response, but human confirmation required).
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

// Proxy is the HTTP handler that enforces idempotency policies. It composes
// the policy config with a Store adapter and an upstream reverse proxy.
type Proxy struct {
	policies *policy.Config
	store    store.Store
	upstream *httputil.ReverseProxy
	logger   *slog.Logger
	metrics  *metrics.Metrics
	embedder embed.Embedder
	now      func() time.Time
}

// Option configures a Proxy at construction time.
type Option func(*Proxy)

// WithMetrics attaches a metrics collector. Without it, the proxy still
// operates correctly but emits no Prometheus samples.
func WithMetrics(m *metrics.Metrics) Option {
	return func(p *Proxy) { p.metrics = m }
}

// WithClock overrides time.Now (used for testing TTL stamping).
func WithClock(clock func() time.Time) Option {
	return func(p *Proxy) { p.now = clock }
}

// WithEmbedder enables the semantic-replay tier. When the embedder is
// non-nil and a tool's policy sets a SemanticThreshold > 0, an exact-hash
// miss falls through to cosine search over cached entries for the tool.
func WithEmbedder(e embed.Embedder) Option {
	return func(p *Proxy) { p.embedder = e }
}

// New constructs a Proxy that forwards uncached requests to upstream.
func New(policies *policy.Config, st store.Store, upstream *url.URL, logger *slog.Logger, opts ...Option) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Proxy{
		policies: policies,
		store:    st,
		upstream: httputil.NewSingleHostReverseProxy(upstream),
		logger:   logger,
		now:      time.Now,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

func (p *Proxy) recordDecision(tool string, mode policy.Mode, decision Decision) {
	if p.metrics == nil {
		return
	}
	p.metrics.Decisions.WithLabelValues(tool, string(mode), decision.String()).Inc()
}

func (p *Proxy) recordStoreError(op string) {
	if p.metrics == nil {
		return
	}
	p.metrics.StoreErrors.WithLabelValues(op).Inc()
}

func (p *Proxy) recordUpstreamLatency(tool string, seconds float64) {
	if p.metrics == nil {
		return
	}
	p.metrics.UpstreamLatency.WithLabelValues(tool).Observe(seconds)
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tool := r.Header.Get(ToolHeader)
	if tool == "" {
		// Without a tool header we cannot apply policy; pass through.
		p.upstream.ServeHTTP(w, r)
		return
	}

	pol := p.policies.For(tool)
	if pol.Mode == policy.ModeOff {
		p.upstream.ServeHTTP(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.fail(w, http.StatusBadRequest, "read body", err)
		return
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	normalized, hash, intent, err := analyseRequest(body, pol)
	if err != nil {
		p.fail(w, http.StatusBadRequest, "fingerprint request", err)
		return
	}
	_ = normalized // reserved for future request canonicalization

	decision, entry, match := p.decide(r.Context(), tool, hash, pol, intent)

	p.logger.Info("policy decision",
		"tool", tool,
		"hash", hash,
		"mode", string(pol.Mode),
		"decision", decision.String(),
		"match", match.kind,
		"similarity", match.similarity,
	)
	p.recordDecision(tool, pol.Mode, decision)

	switch decision {
	case DecisionReplay:
		p.replay(r.Context(), w, tool, entry, match)
	case DecisionBlock:
		http.Error(w, "duplicate request blocked by policy", http.StatusConflict)
	default:
		p.forwardAndCache(w, r, tool, hash, pol, intent)
	}
}

// matchInfo carries why a Replay decision fired so we can surface it in
// response headers, logs, and metrics without smuggling state through the
// Decision enum.
type matchInfo struct {
	kind       string // "exact" | "semantic" | ""
	similarity float32
}

func (p *Proxy) decide(ctx context.Context, tool, hash string, pol policy.ToolPolicy, intent string) (Decision, store.Entry, matchInfo) {
	switch pol.Mode {
	case policy.ModeOff:
		return DecisionForward, store.Entry{}, matchInfo{}
	case policy.ModeLogOnly:
		_, _ = p.store.Get(ctx, tool, hash)
		return DecisionForward, store.Entry{}, matchInfo{}
	case policy.ModeStrict, policy.ModeCache:
		entry, err := p.store.Get(ctx, tool, hash)
		if err == nil {
			if pol.Mode == policy.ModeStrict && pol.RequireHumanConfirmOnReplay {
				return DecisionBlock, entry, matchInfo{kind: "exact", similarity: 1.0}
			}
			return DecisionReplay, entry, matchInfo{kind: "exact", similarity: 1.0}
		}
		if !errors.Is(err, store.ErrNotFound) {
			p.recordStoreError("get")
			p.logger.Warn("store get failed", "err", err)
			return DecisionForward, store.Entry{}, matchInfo{}
		}
		// Exact miss: fall through to semantic tier when configured.
		if p.embedder == nil || pol.SemanticThreshold <= 0 || intent == "" {
			return DecisionForward, store.Entry{}, matchInfo{}
		}
		match, score, ok := p.semanticSearch(ctx, tool, intent, pol.SemanticThreshold)
		if !ok {
			return DecisionForward, store.Entry{}, matchInfo{}
		}
		if pol.Mode == policy.ModeStrict && pol.RequireHumanConfirmOnReplay {
			return DecisionBlock, match, matchInfo{kind: "semantic", similarity: score}
		}
		return DecisionReplay, match, matchInfo{kind: "semantic", similarity: score}
	}
	return DecisionForward, store.Entry{}, matchInfo{}
}

// semanticSearch scans cached entries for the tool and returns the best
// match above threshold. Brute force is fine while per-tool entry counts
// stay small; an ANN index slots in behind the same call site later.
func (p *Proxy) semanticSearch(ctx context.Context, tool, intent string, threshold float64) (store.Entry, float32, bool) {
	queryVec, err := p.embedder.Embed(intent)
	if err != nil {
		p.logger.Warn("embed query failed", "err", err)
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
		p.logger.Warn("store scan failed", "err", err)
		return store.Entry{}, 0, false
	}
	return best, bestScore, found
}

func (p *Proxy) replay(ctx context.Context, w http.ResponseWriter, tool string, e store.Entry, match matchInfo) {
	if err := p.store.IncrementReplay(ctx, tool, e.Hash); err != nil {
		p.logger.Warn("increment replay failed", "err", err)
		p.recordStoreError("increment_replay")
	}
	w.Header().Set("X-Potent-Status", "replayed")
	w.Header().Set("X-Potent-Hash", e.Hash)
	if match.kind != "" {
		w.Header().Set("X-Potent-Match", match.kind)
	}
	if match.similarity > 0 {
		w.Header().Set("X-Potent-Similarity", strconv.FormatFloat(float64(match.similarity), 'f', 4, 32))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(e.Response)
}

func (p *Proxy) forwardAndCache(w http.ResponseWriter, r *http.Request, tool, hash string, pol policy.ToolPolicy, intent string) {
	rec := &recordingWriter{ResponseWriter: w, body: &bytes.Buffer{}, status: http.StatusOK}
	rec.Header().Set("X-Potent-Status", "fresh")
	rec.Header().Set("X-Potent-Hash", hash)

	start := p.now()
	p.upstream.ServeHTTP(rec, r)
	p.recordUpstreamLatency(tool, p.now().Sub(start).Seconds())

	if pol.Mode == policy.ModeStrict || pol.Mode == policy.ModeCache {
		if rec.status >= 200 && rec.status < 300 {
			req, _ := io.ReadAll(r.Body)
			var emb []float32
			if p.embedder != nil && pol.SemanticThreshold > 0 && intent != "" {
				if v, err := p.embedder.Embed(intent); err == nil {
					emb = v
				} else {
					p.logger.Warn("embed cache failed", "err", err)
				}
			}
			err := p.store.Put(r.Context(), store.Entry{
				Tool:       tool,
				Hash:       hash,
				Request:    req,
				Response:   rec.body.Bytes(),
				StatusCode: rec.status,
				CreatedAt:  p.now(),
				TTL:        pol.TTL,
				Embedding:  emb,
			})
			if err != nil {
				p.logger.Warn("store put failed", "err", err)
				p.recordStoreError("put")
			}
		}
	}
}

func (p *Proxy) fail(w http.ResponseWriter, code int, msg string, err error) {
	p.logger.Error(msg, "err", err)
	http.Error(w, msg, code)
}

// recordingWriter is an http.ResponseWriter that captures the upstream
// response body and status for caching, while still streaming to the client.
type recordingWriter struct {
	http.ResponseWriter
	body   *bytes.Buffer
	status int
}

func (rw *recordingWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingWriter) Write(b []byte) (int, error) {
	rw.body.Write(b)
	return rw.ResponseWriter.Write(b)
}

// analyseRequest normalizes the request body, computes an exact-match hash,
// and builds a deterministic "intent string" suitable for embedding. The
// intent string concatenates the policy-selected fields' string values in a
// stable order, producing the same input on retries with reordered JSON.
func analyseRequest(body []byte, pol policy.ToolPolicy) (map[string]any, string, string, error) {
	var raw map[string]any
	if len(body) == 0 {
		hash, err := fingerprint.Exact(map[string]any{})
		return nil, hash, "", err
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, "", "", fmt.Errorf("body must be a JSON object: %w", err)
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
		return nil, "", "", err
	}
	return raw, hash, buildIntent(selected), nil
}

// buildIntent renders a stable, embeddable string from the policy-selected
// fields. Keys are sorted so reordered JSON yields the same intent text.
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
