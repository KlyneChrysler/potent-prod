// Package audit writes a structured record of every policy decision to a
// JSONL stream. Operators wire this to a local file for compliance, ship
// it to S3 / Loki / Splunk, or discard it entirely.
//
// Records are buffered through a bounded channel and written by a single
// background goroutine to keep request-path overhead at one channel send.
// On graceful shutdown the writer drains the channel before returning.
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
}

// Writer accepts records on a buffered channel. Construct via Open or
// NewDiscard; never instantiate directly.
type Writer struct {
	ch     chan Record
	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
	now    func() time.Time
}

// Open returns a Writer that appends JSON-encoded records to path, one per
// line. The file is created with 0600 perms if absent; parent directories
// must already exist. capacity sizes the internal channel — sends block
// when full so the request path applies backpressure rather than losing
// records.
func Open(path string, capacity int) (*Writer, error) {
	if capacity <= 0 {
		capacity = 1024
	}
	if path == "" {
		return nil, errors.New("audit: path is required")
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
	w := newWriter(capacity)
	w.startWriter(f)
	return w, nil
}

// NewDiscard returns a Writer that drops all records. Useful for tests and
// configurations where auditing is disabled but callers still want a
// non-nil Writer to avoid nil checks.
func NewDiscard() *Writer {
	w := newWriter(0)
	w.startWriter(io.Discard)
	return w
}

func newWriter(capacity int) *Writer {
	if capacity <= 0 {
		capacity = 1
	}
	return &Writer{
		ch:     make(chan Record, capacity),
		closed: make(chan struct{}),
		now:    time.Now,
	}
}

func (w *Writer) startWriter(dst io.Writer) {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		enc := json.NewEncoder(dst)
		for r := range w.ch {
			_ = enc.Encode(r)
		}
		if c, ok := dst.(io.Closer); ok {
			_ = c.Close()
		}
	}()
}

// Write enqueues a record. The send blocks when the channel is full so
// backpressure propagates rather than silently dropping audit events.
// After Close, Write becomes a no-op.
func (w *Writer) Write(r Record) {
	select {
	case <-w.closed:
		return
	default:
	}
	if r.Timestamp.IsZero() {
		r.Timestamp = w.now()
	}
	select {
	case w.ch <- r:
	case <-w.closed:
	}
}

// Close drains the channel and waits for the writer goroutine to exit.
// Safe to call multiple times.
func (w *Writer) Close(ctx context.Context) error {
	w.once.Do(func() {
		close(w.closed)
		close(w.ch)
	})
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
