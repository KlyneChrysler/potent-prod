// Package proxy implements the HTTP reverse-proxy adapter that enforces
// per-tool idempotency policies on inbound tool calls.
//
// The proxy reads X-Potent-Tool from the request, looks up the matching
// policy, normalizes JSON-body fields, computes an exact-match fingerprint
// (semantic matching is added in Week 3), and either replays a cached
// response or forwards to the upstream tool server.
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
	"time"

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

	hash, err := fingerprintRequest(body, pol)
	if err != nil {
		p.fail(w, http.StatusBadRequest, "fingerprint request", err)
		return
	}

	decision, entry, err := p.decide(r.Context(), tool, hash, pol)
	if err != nil {
		p.fail(w, http.StatusInternalServerError, "evaluate policy", err)
		return
	}

	p.logger.Info("policy decision",
		"tool", tool,
		"hash", hash,
		"mode", string(pol.Mode),
		"decision", decision.String(),
	)
	p.recordDecision(tool, pol.Mode, decision)

	switch decision {
	case DecisionReplay:
		p.replay(r.Context(), w, tool, hash, entry)
	case DecisionBlock:
		http.Error(w, "duplicate request blocked by policy", http.StatusConflict)
	default:
		p.forwardAndCache(w, r, tool, hash, pol)
	}
}

func (p *Proxy) decide(ctx context.Context, tool, hash string, pol policy.ToolPolicy) (Decision, store.Entry, error) {
	switch pol.Mode {
	case policy.ModeOff:
		return DecisionForward, store.Entry{}, nil
	case policy.ModeLogOnly:
		// Detect dupes for logs/metrics but never replay or block.
		_, _ = p.store.Get(ctx, tool, hash)
		return DecisionForward, store.Entry{}, nil
	case policy.ModeStrict, policy.ModeCache:
		entry, err := p.store.Get(ctx, tool, hash)
		if errors.Is(err, store.ErrNotFound) {
			return DecisionForward, store.Entry{}, nil
		}
		if err != nil {
			p.recordStoreError("get")
			return DecisionForward, store.Entry{}, fmt.Errorf("store get: %w", err)
		}
		if pol.Mode == policy.ModeStrict && pol.RequireHumanConfirmOnReplay {
			return DecisionBlock, entry, nil
		}
		return DecisionReplay, entry, nil
	}
	return DecisionForward, store.Entry{}, nil
}

func (p *Proxy) replay(ctx context.Context, w http.ResponseWriter, tool, hash string, e store.Entry) {
	if err := p.store.IncrementReplay(ctx, tool, hash); err != nil {
		p.logger.Warn("increment replay failed", "err", err)
		p.recordStoreError("increment_replay")
	}
	w.Header().Set("X-Potent-Status", "replayed")
	w.Header().Set("X-Potent-Hash", hash)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.StatusCode)
	_, _ = w.Write(e.Response)
}

func (p *Proxy) forwardAndCache(w http.ResponseWriter, r *http.Request, tool, hash string, pol policy.ToolPolicy) {
	rec := &recordingWriter{ResponseWriter: w, body: &bytes.Buffer{}, status: http.StatusOK}
	rec.Header().Set("X-Potent-Status", "fresh")
	rec.Header().Set("X-Potent-Hash", hash)

	start := p.now()
	p.upstream.ServeHTTP(rec, r)
	p.recordUpstreamLatency(tool, p.now().Sub(start).Seconds())

	if pol.Mode == policy.ModeStrict || pol.Mode == policy.ModeCache {
		if rec.status >= 200 && rec.status < 300 {
			req, _ := io.ReadAll(r.Body) // body already buffered above
			err := p.store.Put(r.Context(), store.Entry{
				Tool:       tool,
				Hash:       hash,
				Request:    req,
				Response:   rec.body.Bytes(),
				StatusCode: rec.status,
				CreatedAt:  p.now(),
				TTL:        pol.TTL,
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

// fingerprintRequest normalizes the request body and produces an exact-match
// hash. If the body is empty or not a JSON object, an error is returned.
func fingerprintRequest(body []byte, pol policy.ToolPolicy) (string, error) {
	var raw map[string]any
	if len(body) == 0 {
		return fingerprint.Exact(map[string]any{})
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("body must be a JSON object: %w", err)
	}

	for field, transforms := range pol.Normalize {
		if v, ok := raw[field].(string); ok {
			raw[field] = normalizer.Apply(v, transforms)
		}
	}

	if len(pol.FingerprintFields) > 0 {
		filtered := make(map[string]any, len(pol.FingerprintFields))
		for _, f := range pol.FingerprintFields {
			if v, ok := raw[f]; ok {
				filtered[f] = v
			}
		}
		return fingerprint.Exact(filtered)
	}
	return fingerprint.Exact(raw)
}
