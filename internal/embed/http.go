package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// HTTPEmbedder calls out to an external embedding service so operators
// can plug in any transformer model without bundling ONNX, libtorch, or
// CUDA into the potent binary. The contract is intentionally minimal so
// it fits sentence-transformers HTTP servers, ollama's /api/embeddings,
// the OpenAI embeddings API, or any 30-line FastAPI shim.
//
// Request:  POST <URL> with {"input": "<text>"} and optional headers
// Response: 200 with {"embedding": [<float32>, ...]} or
//           {"data": [{"embedding": [...]}]} (OpenAI-compatible)
//
// The embedder is L2-normalised once on the client side so backends that
// don't normalise (raw transformer hidden states) still produce cosine
// similarities in [-1, 1].
type HTTPEmbedder struct {
	url     string
	dim     int
	client  *http.Client
	headers map[string]string
	now     func() time.Time

	mu sync.Mutex // guards dim discovery
}

// HTTPEmbedderOptions configures the HTTP embedder.
type HTTPEmbedderOptions struct {
	// URL is the endpoint to POST to. Required.
	URL string
	// Dim is the expected vector dimension. If 0, it is discovered on
	// the first call by inspecting the response length.
	Dim int
	// Timeout caps a single embedding request. Defaults to 10s.
	Timeout time.Duration
	// Headers are sent on every request (e.g. {"Authorization": "Bearer ..."}).
	Headers map[string]string
	// Client overrides the underlying *http.Client for tests.
	Client *http.Client
}

// NewHTTPEmbedder constructs an HTTPEmbedder. URL is required; everything
// else has sensible defaults.
func NewHTTPEmbedder(opts HTTPEmbedderOptions) (*HTTPEmbedder, error) {
	if opts.URL == "" {
		return nil, errors.New("embed: HTTP embedder URL is required")
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 10 * time.Second
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: opts.Timeout}
	}
	return &HTTPEmbedder{
		url:     opts.URL,
		dim:     opts.Dim,
		client:  client,
		headers: opts.Headers,
		now:     time.Now,
	}, nil
}

// Dim returns the embedding dimension. Returns 0 if it has not yet been
// discovered (no embedding call has succeeded).
func (e *HTTPEmbedder) Dim() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.dim
}

type embedReq struct {
	Input string `json:"input"`
}

// embedResp accepts both the simple {"embedding": [...]} shape and the
// OpenAI-compatible {"data": [{"embedding": [...]}]} shape.
type embedResp struct {
	Embedding []float32 `json:"embedding,omitempty"`
	Data      []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data,omitempty"`
}

// Embed POSTs the text to the configured URL and returns an L2-normalised
// vector. Empty text returns a zero vector of the configured dim (matches
// HashingTFIDF semantics).
func (e *HTTPEmbedder) Embed(text string) ([]float32, error) {
	if text == "" {
		return make([]float32, e.Dim()), nil
	}
	body, err := json.Marshal(embedReq{Input: text})
	if err != nil {
		return nil, fmt.Errorf("embed: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), e.client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: post %s: %w", e.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embed: %s returned %d: %s", e.url, resp.StatusCode, preview)
	}
	var parsed embedResp
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("embed: decode response: %w", err)
	}
	vec := parsed.Embedding
	if len(vec) == 0 && len(parsed.Data) > 0 {
		vec = parsed.Data[0].Embedding
	}
	if len(vec) == 0 {
		return nil, errors.New("embed: response had no embedding field")
	}

	e.mu.Lock()
	switch {
	case e.dim == 0:
		e.dim = len(vec)
	case e.dim != len(vec):
		e.mu.Unlock()
		return nil, fmt.Errorf("embed: dim mismatch: got %d, configured %d", len(vec), e.dim)
	}
	e.mu.Unlock()

	l2Normalize(vec)
	return vec, nil
}
