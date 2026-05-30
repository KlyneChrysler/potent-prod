// Package audit writes a structured record of every policy decision to one
// or more sinks. Operators wire records to a local file for compliance,
// ship them to object storage for long-term retention, or both at once.
//
// The public surface is intentionally small: a Record schema, a Sink
// interface, and a Writer that fans a record out to every configured sink.
// Each sink owns its own buffering and shutdown semantics so the request
// path applies one channel send per sink.
package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one audit entry. JSON tags are stable; treat them as a public
// schema once a release is tagged.
type Record struct {
	Timestamp  time.Time `json:"ts"`
	Tool       string    `json:"tool"`
	Mode       string    `json:"mode"`
	Decision   string    `json:"decision"`
	Match      string    `json:"match,omitempty"`
	Similarity float32   `json:"similarity,omitempty"`
	Hash       string    `json:"hash"`
	Protocol   string    `json:"protocol,omitempty"`

	// Shadow is true when the proxy was running in shadow mode at the time
	// of this decision. Decision is the action that was actually taken
	// (always "forward" in shadow); WouldDecision is what the policy would
	// have chosen if shadow had been off.
	Shadow        bool   `json:"shadow,omitempty"`
	WouldDecision string `json:"would_decision,omitempty"`

	// TraceID is the W3C trace-context trace-id under which the decision
	// was made, when known. Carrying it through audit makes it possible
	// for operators to correlate a particular replay/block decision with
	// the rest of the request span in Datadog/Honeycomb/Jaeger.
	TraceID string `json:"trace_id,omitempty"`
}

// Sink consumes audit records. Implementations own their own buffering,
// flush cadence, and error reporting. Write must not block on slow I/O;
// it should enqueue and return. Close must drain pending records and
// return once they have been flushed or ctx expires.
type Sink interface {
	Write(Record) error
	Close(ctx context.Context) error
}

// Writer fans a record out to every configured sink. Construct via
// NewWriter, Open (convenience for a single file sink), or NewDiscard.
type Writer struct {
	sinks  []Sink
	closed chan struct{}
	once   sync.Once
	now    func() time.Time
}

// NewWriter returns a Writer that dispatches each record to every sink in
// order. A Writer with no sinks discards records.
func NewWriter(sinks ...Sink) *Writer {
	return &Writer{
		sinks:  sinks,
		closed: make(chan struct{}),
		now:    time.Now,
	}
}

// Open returns a Writer with a single file sink appending JSON-encoded
// records to path. Kept for backwards compatibility with v0.1.0 .. v0.1.13.
func Open(path string, capacity int) (*Writer, error) {
	s, err := NewFileSink(path, capacity)
	if err != nil {
		return nil, err
	}
	return NewWriter(s), nil
}

// NewDiscard returns a Writer with no sinks. Useful for tests and
// configurations where auditing is disabled but callers still want a
// non-nil Writer to avoid nil checks.
func NewDiscard() *Writer { return NewWriter() }

// Write enqueues a record on every sink. Errors from sinks are not
// propagated to the caller; sinks are responsible for their own error
// reporting (typically via logs or metrics). After Close, Write is a
// no-op.
func (w *Writer) Write(r Record) {
	select {
	case <-w.closed:
		return
	default:
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = w.now()
	}
	for _, s := range w.sinks {
		_ = s.Write(r)
	}
}

// Close shuts every sink down. Sinks are closed in registration order so
// operators see a deterministic shutdown sequence in logs.
func (w *Writer) Close(ctx context.Context) error {
	w.once.Do(func() { close(w.closed) })
	var firstErr error
	for _, s := range w.sinks {
		if err := s.Close(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ---------- file sink ----------

// fileSink writes one JSON record per line to an os.File via a single
// background goroutine fed by a bounded channel.
type fileSink struct {
	ch     chan Record
	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

// NewFileSink opens path for append and starts a writer goroutine. The
// file is created with 0600 perms if absent; the parent directory must
// already exist. capacity sizes the internal channel.
func NewFileSink(path string, capacity int) (Sink, error) {
	if path == "" {
		return nil, errors.New("audit: file sink path is required")
	}
	if capacity <= 0 {
		capacity = 1024
	}
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("audit: parent dir: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("audit: open %q: %w", path, err)
	}
	return newFileSinkWriter(f, capacity), nil
}

func newFileSinkWriter(dst io.Writer, capacity int) *fileSink {
	if capacity <= 0 {
		capacity = 1
	}
	s := &fileSink{
		ch:     make(chan Record, capacity),
		closed: make(chan struct{}),
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		enc := json.NewEncoder(dst)
		for r := range s.ch {
			_ = enc.Encode(r)
		}
		if c, ok := dst.(io.Closer); ok {
			_ = c.Close()
		}
	}()
	return s
}

func (s *fileSink) Write(r Record) error {
	select {
	case <-s.closed:
		return errSinkClosed
	default:
	}
	select {
	case s.ch <- r:
		return nil
	case <-s.closed:
		return errSinkClosed
	}
}

func (s *fileSink) Close(ctx context.Context) error {
	s.once.Do(func() {
		close(s.closed)
		close(s.ch)
	})
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errSinkClosed = errors.New("audit: sink closed")
