package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/potent/potent/internal/auth"
	"github.com/potent/potent/internal/pipeline"
)

// DefaultMaxBodyBytes caps a single JSON-RPC frame body. MCP frames are
// usually small; 1 MiB is well above realistic tools/call payloads and well
// below memory-exhaustion territory.
const DefaultMaxBodyBytes int64 = 1 << 20

// HTTPHandler is an MCP Streamable HTTP adapter. It accepts JSON-RPC frames
// over POST, intercepts tools/call to run them through the pipeline, and
// passes everything else (initialize, tools/list, resources/*, etc.)
// untouched to the upstream MCP server.
type HTTPHandler struct {
	pipeline     *pipeline.Pipeline
	upstream     *url.URL
	client       *http.Client
	logger       *slog.Logger
	maxBodyBytes int64
	apiToken     string
}

// HTTPOption configures an HTTPHandler at construction time.
type HTTPOption func(*HTTPHandler)

// WithHTTPMaxBodyBytes caps the size of inbound JSON-RPC frames. Pass 0 to
// disable the cap (not recommended).
func WithHTTPMaxBodyBytes(n int64) HTTPOption {
	return func(h *HTTPHandler) { h.maxBodyBytes = n }
}

// WithHTTPAPIToken gates every inbound request behind constant-time bearer
// token validation. An empty token disables auth.
func WithHTTPAPIToken(token string) HTTPOption {
	return func(h *HTTPHandler) { h.apiToken = token }
}

// NewHTTPHandler constructs the MCP HTTP adapter.
func NewHTTPHandler(pl *pipeline.Pipeline, upstream *url.URL, logger *slog.Logger, opts ...HTTPOption) *HTTPHandler {
	if logger == nil {
		logger = slog.Default()
	}
	h := &HTTPHandler{
		pipeline:     pl,
		upstream:     upstream,
		client:       &http.Client{},
		logger:       logger,
		maxBodyBytes: DefaultMaxBodyBytes,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Handler returns the MCP HTTP handler wrapped with bearer-token auth when
// an API token is configured. Mount this rather than the bare HTTPHandler.
func (h *HTTPHandler) Handler() http.Handler {
	return auth.RequireBearer(h.apiToken, "potent-mcp", h)
}

// ServeHTTP implements http.Handler.
func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// MCP Streamable HTTP also supports GET for SSE server→client streams.
		// For Week 4 we proxy GETs through transparently — interception
		// only applies to POST'd tools/call requests.
		h.passthrough(w, r, nil)
		return
	}

	reader := io.Reader(r.Body)
	if h.maxBodyBytes > 0 {
		reader = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		http.Error(w, "read body", http.StatusRequestEntityTooLarge)
		return
	}
	_ = r.Body.Close()

	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		// Not a parseable JSON-RPC frame; pass through.
		h.passthrough(w, r, body)
		return
	}

	if msg.Method != MethodToolsCall {
		h.passthrough(w, r, body)
		return
	}

	tool, args, err := ParseToolsCall(&msg)
	if err != nil {
		h.writeRPCError(w, msg.ID, -32602, "invalid tools/call params")
		return
	}

	forward := func(ctx context.Context) (int, []byte, error) {
		return h.forwardOnce(ctx, r, body)
	}

	res, err := h.pipeline.Apply(r.Context(), tool, args, forward)
	if err != nil {
		h.logger.Warn("pipeline apply", "err", err, "tool", tool)
		h.writeRPCError(w, msg.ID, -32603, "pipeline error")
		return
	}

	h.logger.Info("mcp tools/call",
		"tool", tool,
		"hash", res.Hash,
		"decision", res.Decision.String(),
		"match", res.Match.Kind,
	)

	w.Header().Set("X-Potent-Hash", res.Hash)
	w.Header().Set("Content-Type", "application/json")
	switch res.Decision {
	case pipeline.DecisionRateLimited:
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusOK)
		blocked := NewErrorResponse(msg.ID, -32005, "rate limit exceeded")
		_ = json.NewEncoder(w).Encode(blocked)
		return
	case pipeline.DecisionReplay:
		// Reshape the cached upstream response into a JSON-RPC frame so the
		// client cannot tell the cache hit from a fresh response besides the
		// X-Potent-Status header. The cached body is the original upstream
		// response: an entire JSON-RPC frame including the original id. We
		// substitute the current request's id so the client correlates.
		w.Header().Set("X-Potent-Status", "replayed")
		if res.Match.Kind != "" {
			w.Header().Set("X-Potent-Match", res.Match.Kind)
		}
		if res.Match.Similarity > 0 {
			w.Header().Set("X-Potent-Similarity", strconv.FormatFloat(float64(res.Match.Similarity), 'f', 4, 32))
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(reframeWithID(res.Body, msg.ID))
	case pipeline.DecisionBlock:
		w.WriteHeader(http.StatusOK)
		blocked := NewErrorResponse(msg.ID, -32000, "duplicate request blocked by policy")
		_ = json.NewEncoder(w).Encode(blocked)
	default:
		w.Header().Set("X-Potent-Status", "fresh")
		w.WriteHeader(res.StatusCode)
		_, _ = w.Write(res.Body)
	}
}

func (h *HTTPHandler) passthrough(w http.ResponseWriter, r *http.Request, body []byte) {
	target := *h.upstream
	target.Path = singleJoin(target.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery

	var reqBody io.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), reqBody)
	if err != nil {
		http.Error(w, "build request", http.StatusInternalServerError)
		return
	}
	copyHeaders(req.Header, r.Header)

	// Upstream URL host/scheme are operator-configured; only path/query are
	// appended from the inbound request (a proxy's expected behavior).
	resp, err := h.client.Do(req) // #nosec G107,G704 -- upstream host fixed at startup
	if err != nil {
		http.Error(w, "upstream", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// forwardOnce sends the original JSON-RPC frame upstream and returns its
// status + body for the pipeline to cache.
func (h *HTTPHandler) forwardOnce(ctx context.Context, r *http.Request, body []byte) (int, []byte, error) {
	target := *h.upstream
	target.Path = singleJoin(target.Path, r.URL.Path)
	target.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	copyHeaders(req.Header, r.Header)
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// Upstream URL host/scheme are operator-configured; only path/query are
	// appended from the inbound request (a proxy's expected behavior).
	resp, err := h.client.Do(req) // #nosec G107,G704 -- upstream host fixed at startup
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

func (h *HTTPHandler) writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(NewErrorResponse(id, code, msg))
}

// reframeWithID rewrites the "id" field of a cached JSON-RPC response so
// the current client correlates the replay with its own request id. If the
// cached body is not a valid JSON-RPC frame it is returned unchanged.
func reframeWithID(cached []byte, id json.RawMessage) []byte {
	if len(id) == 0 {
		return cached
	}
	var m Message
	if err := json.Unmarshal(cached, &m); err != nil {
		return cached
	}
	m.ID = id
	out, err := json.Marshal(m)
	if err != nil {
		return cached
	}
	return out
}

func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func singleJoin(a, b string) string {
	switch {
	case a == "" || a == "/":
		return b
	case b == "" || b == "/":
		return a
	case a[len(a)-1] == '/' && b[0] == '/':
		return a + b[1:]
	case a[len(a)-1] != '/' && b[0] != '/':
		return a + "/" + b
	default:
		return a + b
	}
}

