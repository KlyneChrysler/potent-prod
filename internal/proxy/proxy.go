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
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"github.com/potent/potent/internal/pipeline"
)

// ToolHeader is the request header the client sets to tell the proxy which
// tool is being invoked.
const ToolHeader = "X-Potent-Tool"

// Proxy is an http.Handler that runs requests through the idempotency
// pipeline before forwarding to upstream.
type Proxy struct {
	pipeline *pipeline.Pipeline
	upstream *httputil.ReverseProxy
	upstrURL *url.URL
	logger   *slog.Logger
}

// New constructs an HTTP-mode proxy.
func New(pl *pipeline.Pipeline, upstream *url.URL, logger *slog.Logger) *Proxy {
	if logger == nil {
		logger = slog.Default()
	}
	return &Proxy{
		pipeline: pl,
		upstream: httputil.NewSingleHostReverseProxy(upstream),
		upstrURL: upstream,
		logger:   logger,
	}
}

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tool := r.Header.Get(ToolHeader)
	if tool == "" {
		p.upstream.ServeHTTP(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
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

	res, err := p.pipeline.Apply(r.Context(), tool, body, forward)
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
