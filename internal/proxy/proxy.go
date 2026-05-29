// Package proxy implements the HTTP reverse-proxy adapter that enforces
// per-tool idempotency policies on inbound tool calls.
//
// The handler reads X-Potent-Tool from the request, hands the body and a
// forwarder closure to the pipeline, and writes the verdict back as a
// regular HTTP response with X-Potent-* headers.
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/potent/potent/internal/auth"
	"github.com/potent/potent/internal/pipeline"
)

// ToolHeader is the request header the client sets to tell the proxy which
// tool is being invoked.
const ToolHeader = "X-Potent-Tool"

// DefaultMaxBodyBytes caps a single tool-call request body. Tool call bodies
// are arguments JSON; 1 MiB is well above realistic usage and well below
// memory-exhaustion territory.
const DefaultMaxBodyBytes int64 = 1 << 20

// Proxy is an http.Handler that runs requests through the idempotency
// pipeline before forwarding to upstream.
type Proxy struct {
	pipeline     *pipeline.Pipeline
	upstream     *httputil.ReverseProxy
	upstrURL     *url.URL
	logger       *slog.Logger
	maxBodyBytes int64

	// Single-token auth (POTENT_API_TOKEN). When set, every caller is
	// treated as anonymous and per-tool ACLs cannot enforce caller
	// identity. Used by simple single-tenant deployments.
	apiToken string

	// Multi-tenant auth: map of token -> caller-id. When non-empty, takes
	// precedence over apiToken; the matched caller-id is attached to the
	// request context so the pipeline can enforce per-tool ACLs and the
	// audit log can record who made the call.
	apiTokens map[string]string

	// upstreamTLS, when non-nil, is applied to the reverse-proxy transport
	// so the connection to upstream uses a custom CA pool, client cert,
	// or insecure-skip-verify per operator policy.
	upstreamTLS *tls.Config
}

// Option configures a Proxy at construction time.
type Option func(*Proxy)

// WithMaxBodyBytes caps the size of inbound tool-call request bodies.
// Pass 0 to disable the cap (not recommended).
func WithMaxBodyBytes(n int64) Option {
	return func(p *Proxy) { p.maxBodyBytes = n }
}

// WithAPIToken gates every request behind a constant-time bearer-token
// check. An empty token disables auth. cmd/potent enforces the token at
// startup when the proxy listens on a non-loopback address.
func WithAPIToken(token string) Option {
	return func(p *Proxy) { p.apiToken = token }
}

// WithAPITokensFile installs a multi-tenant token map. Each request's
// bearer is looked up against the map and the matching caller-id is
// attached to the request context for ACL enforcement and audit logging.
// When supplied with a non-empty map, takes precedence over WithAPIToken.
func WithAPITokensFile(tokens map[string]string) Option {
	return func(p *Proxy) { p.apiTokens = tokens }
}

// WithUpstreamTLS swaps the reverse-proxy transport with one configured
// from cfg. Pass nil for the default (system trust store, no client cert).
func WithUpstreamTLS(cfg *tls.Config) Option {
	return func(p *Proxy) { p.upstreamTLS = cfg }
}

// New constructs an HTTP-mode proxy.
func New(pl *pipeline.Pipeline, upstream *url.URL, logger *slog.Logger, opts ...Option) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	p := &Proxy{
		pipeline:     pl,
		upstream:     httputil.NewSingleHostReverseProxy(upstream),
		upstrURL:     upstream,
		logger:       logger,
		maxBodyBytes: DefaultMaxBodyBytes,
	}
	for _, opt := range opts {
		opt(p)
	}
	// Apply upstream TLS to the reverse-proxy transport if configured.
	// The default transport carries CGO-DNS, dual-stack dial, etc., so we
	// clone it and only override TLSClientConfig.
	if p.upstreamTLS != nil {
		tp := http.DefaultTransport.(*http.Transport).Clone()
		tp.TLSClientConfig = p.upstreamTLS
		p.upstream.Transport = tp
	}
	return p
}

// Handler returns the proxy wrapped with bearer-token auth when an API
// token is configured. Mount this rather than the bare Proxy on the
// public-facing ServeMux so the token check runs ahead of every request.
// Multi-tenant (apiTokens) takes precedence over single-token (apiToken).
func (p *Proxy) Handler() http.Handler {
	if len(p.apiTokens) > 0 {
		return auth.RequireBearerCallers(p.apiTokens, "potent", p)
	}
	return auth.RequireBearer(p.apiToken, "potent", p)
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tool := r.Header.Get(ToolHeader)
	if tool == "" {
		p.upstream.ServeHTTP(w, r)
		return
	}

	reader := io.Reader(r.Body)
	if p.maxBodyBytes > 0 {
		reader = http.MaxBytesReader(w, r.Body, p.maxBodyBytes)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, "read body", http.StatusRequestEntityTooLarge)
		return
	}
	_ = r.Body.Close()

	forward := func(ctx context.Context) (int, []byte, error) {
		req := r.Clone(ctx)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		rec := &recorder{header: http.Header{}, body: &bytes.Buffer{}, status: http.StatusOK}
		p.upstream.ServeHTTP(rec, req)
		return rec.status, rec.body.Bytes(), nil
	}

	caller := auth.CallerFromContext(r.Context())
	res, err := p.pipeline.Apply(r.Context(), tool, caller, body, forward)
	if err != nil {
		p.logger.Warn("pipeline apply", "err", err, "tool", tool)
		http.Error(w, "pipeline error", http.StatusInternalServerError)
		return
	}

	p.logger.Info("policy decision",
		"tool", tool,
		"hash", res.Hash,
		"decision", res.Decision.String(),
		"match", res.Match.Kind,
		"similarity", res.Match.Similarity,
	)

	w.Header().Set("X-Potent-Hash", res.Hash)
	switch res.Decision {
	case pipeline.DecisionForbidden:
		http.Error(w, "tool not allowed for caller", http.StatusForbidden)
		return
	case pipeline.DecisionRateLimited:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	case pipeline.DecisionReplay:
		w.Header().Set("X-Potent-Status", "replayed")
		if res.Match.Kind != "" {
			w.Header().Set("X-Potent-Match", res.Match.Kind)
		}
		if res.Match.Similarity > 0 {
			w.Header().Set("X-Potent-Similarity", strconv.FormatFloat(float64(res.Match.Similarity), 'f', 4, 32))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(res.Body)
	case pipeline.DecisionBlock:
		http.Error(w, "duplicate request blocked by policy", http.StatusConflict)
	default:
		w.Header().Set("X-Potent-Status", "fresh")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(res.Body)
	}
}

// recorder is a minimal http.ResponseWriter used to capture the upstream
// response so it can flow through the pipeline result.
type recorder struct {
	header http.Header
	body   *bytes.Buffer
	status int
}

func (r *recorder) Header() http.Header        { return r.header }
func (r *recorder) WriteHeader(code int)       { r.status = code }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
