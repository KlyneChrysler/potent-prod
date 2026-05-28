package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func newEntry(tool, hash string, ttl time.Duration, now time.Time) Entry {
	return Entry{
		Tool:       tool,
		Hash:       hash,
		Request:    []byte(`{"x":1}`),
		Response:   []byte(`{"ok":true}`),
		StatusCode: 200,
		CreatedAt:  now,
		TTL:        ttl,
	}
}

func TestMemory_PutAndGet(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := NewMemory(func() time.Time { return now })
	ctx := context.Background()

	in := newEntry("send_email", "abc", time.Hour, now)
	if err := m.Put(ctx, in); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := m.Get(ctx, "send_email", "abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Tool != in.Tool || got.Hash != in.Hash {
		t.Errorf("entry mismatch: %+v", got)
	}
}

func TestMemory_GetMissingReturnsNotFound(t *testing.T) {
	m := NewMemory(nil)
	_, err := m.Get(context.Background(), "x", "y")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestMemory_ExpiredEntryIsNotFound(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := now
	m := NewMemory(func() time.Time { return clock })
	ctx := context.Background()

	if err := m.Put(ctx, newEntry("t", "h", time.Minute, now)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	clock = now.Add(2 * time.Minute) // advance past TTL

	_, err := m.Get(ctx, "t", "h")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound for expired entry, got %v", err)
	}
}

func TestMemory_ZeroTTLNeverExpires(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	clock := now
	m := NewMemory(func() time.Time { return clock })
	ctx := context.Background()

	_ = m.Put(ctx, newEntry("t", "h", 0, now))

	clock = now.Add(100 * 24 * time.Hour)
	if _, err := m.Get(ctx, "t", "h"); err != nil {
		t.Errorf("zero TTL should not expire, got %v", err)
	}
}

func TestMemory_PutRequiresKeys(t *testing.T) {
	m := NewMemory(nil)
	err := m.Put(context.Background(), Entry{Hash: "h"})
	if err == nil {
		t.Errorf("expected error for missing Tool")
	}
}

func TestMemory_IncrementReplay(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := NewMemory(func() time.Time { return now })
	ctx := context.Background()

	_ = m.Put(ctx, newEntry("t", "h", time.Hour, now))
	if err := m.IncrementReplay(ctx, "t", "h"); err != nil {
		t.Fatalf("IncrementReplay: %v", err)
	}
	if err := m.IncrementReplay(ctx, "t", "h"); err != nil {
		t.Fatalf("IncrementReplay: %v", err)
	}

	got, _ := m.Get(ctx, "t", "h")
	if got.ReplayCount != 2 {
		t.Errorf("expected ReplayCount=2, got %d", got.ReplayCount)
	}
}

func TestMemory_IncrementReplayMissing(t *testing.T) {
	m := NewMemory(nil)
	err := m.IncrementReplay(context.Background(), "t", "h")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestMemory_ContextCancelled(t *testing.T) {
	m := NewMemory(nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := m.Get(ctx, "a", "b"); err == nil {
		t.Errorf("expected ctx error")
	}
	if err := m.Put(ctx, newEntry("t", "h", time.Hour, time.Now())); err == nil {
		t.Errorf("expected ctx error")
	}
}

func TestMemory_ConcurrentAccess(t *testing.T) {
	m := NewMemory(nil)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := Entry{Tool: "t", Hash: string(rune('a' + i%26)), TTL: time.Hour, CreatedAt: time.Now()}
			_ = m.Put(ctx, e)
			_, _ = m.Get(ctx, e.Tool, e.Hash)
		}(i)
	}
	wg.Wait()
}
