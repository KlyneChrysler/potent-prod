// Package store defines the persistence port for cached tool-call fingerprints
// and provides an in-memory adapter suitable for tests and Week 1 development.
//
// The Store interface intentionally exposes only the operations the proxy
// needs. Real adapters (BoltDB, Redis) will satisfy the same contract.
package store

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotFound is returned by Store.Get when no entry exists for the key.
var ErrNotFound = errors.New("store: not found")

// Entry is the cached result of a previously executed tool call.
//
// It is immutable from the store's perspective: callers must treat returned
// entries as read-only and construct a new Entry to update.
type Entry struct {
	Tool        string
	Hash        string
	Request     []byte
	Response    []byte
	StatusCode  int
	CreatedAt   time.Time
	ReplayCount int
	TTL         time.Duration

	// Embedding is the semantic vector of the request intent. nil means
	// the entry was written before semantic indexing was enabled or the
	// embedder is disabled for this tool.
	Embedding []float32
}

// Expired reports whether the entry has aged past its TTL relative to now.
// A zero TTL means the entry never expires.
func (e Entry) Expired(now time.Time) bool {
	if e.TTL == 0 {
		return false
	}
	return now.Sub(e.CreatedAt) > e.TTL
}

// Store is the persistence port used by the proxy. Implementations must be
// safe for concurrent use.
type Store interface {
	Get(ctx context.Context, tool, hash string) (Entry, error)
	Put(ctx context.Context, e Entry) error
	IncrementReplay(ctx context.Context, tool, hash string) error
	// Scan iterates non-expired entries for the given tool. The visitor
	// returns false to stop iteration. Order is implementation-defined.
	Scan(ctx context.Context, tool string, visit func(Entry) bool) error
}

// Memory is an in-memory Store suitable for tests and local development.
// It is safe for concurrent use.
type Memory struct {
	mu      sync.RWMutex
	entries map[string]Entry
	now     func() time.Time
}

// NewMemory returns an empty in-memory Store. The clock function is used for
// expiry checks; pass time.Now in production code.
func NewMemory(clock func() time.Time) *Memory {
	if clock == nil {
		clock = time.Now
	}
	return &Memory{
		entries: make(map[string]Entry),
		now:     clock,
	}
}

func key(tool, hash string) string { return tool + "\x00" + hash }

// Get returns the cached entry for (tool, hash). Expired entries are
// reported as ErrNotFound and lazily removed.
func (m *Memory) Get(ctx context.Context, tool, hash string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	m.mu.RLock()
	e, ok := m.entries[key(tool, hash)]
	m.mu.RUnlock()
	if !ok {
		return Entry{}, ErrNotFound
	}
	if e.Expired(m.now()) {
		m.mu.Lock()
		delete(m.entries, key(tool, hash))
		m.mu.Unlock()
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Put stores the entry, overwriting any existing entry for the same key.
func (m *Memory) Put(ctx context.Context, e Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.Tool == "" || e.Hash == "" {
		return errors.New("store: Entry.Tool and Entry.Hash are required")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = m.now()
	}
	m.mu.Lock()
	m.entries[key(e.Tool, e.Hash)] = e
	m.mu.Unlock()
	return nil
}

// Scan iterates non-expired entries for the given tool.
func (m *Memory) Scan(ctx context.Context, tool string, visit func(Entry) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := m.now()
	m.mu.RLock()
	defer m.mu.RUnlock()
	prefix := tool + "\x00"
	for k, e := range m.entries {
		if !startsWith(k, prefix) {
			continue
		}
		if e.Expired(now) {
			continue
		}
		if !visit(e) {
			return nil
		}
	}
	return nil
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

// IncrementReplay bumps the replay counter for an existing entry. Returns
// ErrNotFound if the entry is missing.
func (m *Memory) IncrementReplay(ctx context.Context, tool, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key(tool, hash)]
	if !ok {
		return ErrNotFound
	}
	e.ReplayCount++
	m.entries[key(tool, hash)] = e
	return nil
}
