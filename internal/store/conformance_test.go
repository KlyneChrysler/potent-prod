package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// conformanceSuite runs the contract every Store implementation must satisfy.
// It is parameterized over a factory so we can drive both Memory and Bolt
// through the same scenarios.
func conformanceSuite(t *testing.T, newStore func(clock func() time.Time) (Store, func())) {
	t.Helper()

	now := time.Unix(1_700_000_000, 0)
	clock := now
	clockFn := func() time.Time { return clock }

	t.Run("PutAndGet", func(t *testing.T) {
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		in := Entry{Tool: "t", Hash: "h", Response: []byte("ok"), StatusCode: 200, TTL: time.Hour, CreatedAt: now}
		if err := s.Put(ctx, in); err != nil {
			t.Fatalf("Put: %v", err)
		}
		got, err := s.Get(ctx, "t", "h")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got.Tool != "t" || string(got.Response) != "ok" || got.StatusCode != 200 {
			t.Errorf("entry mismatch: %+v", got)
		}
	})

	t.Run("GetMissing", func(t *testing.T) {
		s, cleanup := newStore(clockFn)
		defer cleanup()
		_, err := s.Get(context.Background(), "x", "y")
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("Expired", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		_ = s.Put(ctx, Entry{Tool: "t", Hash: "h", TTL: time.Minute, CreatedAt: now})
		clock = now.Add(2 * time.Minute)
		if _, err := s.Get(ctx, "t", "h"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected expired to be ErrNotFound, got %v", err)
		}
	})

	t.Run("ZeroTTL", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		_ = s.Put(ctx, Entry{Tool: "t", Hash: "h", TTL: 0, CreatedAt: now})
		clock = now.Add(365 * 24 * time.Hour)
		if _, err := s.Get(ctx, "t", "h"); err != nil {
			t.Errorf("zero TTL should never expire, got %v", err)
		}
	})

	t.Run("PutRequiresKeys", func(t *testing.T) {
		s, cleanup := newStore(clockFn)
		defer cleanup()
		if err := s.Put(context.Background(), Entry{Hash: "h"}); err == nil {
			t.Errorf("expected error for missing Tool")
		}
	})

	t.Run("IncrementReplay", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		_ = s.Put(ctx, Entry{Tool: "t", Hash: "h", TTL: time.Hour, CreatedAt: now})
		_ = s.IncrementReplay(ctx, "t", "h")
		_ = s.IncrementReplay(ctx, "t", "h")
		got, _ := s.Get(ctx, "t", "h")
		if got.ReplayCount != 2 {
			t.Errorf("expected ReplayCount=2, got %d", got.ReplayCount)
		}
	})

	t.Run("IncrementReplayMissing", func(t *testing.T) {
		s, cleanup := newStore(clockFn)
		defer cleanup()
		if err := s.IncrementReplay(context.Background(), "t", "h"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("ContextCancelled", func(t *testing.T) {
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := s.Get(ctx, "a", "b"); err == nil {
			t.Errorf("expected ctx error on Get")
		}
		if err := s.Put(ctx, Entry{Tool: "t", Hash: "h", TTL: time.Hour, CreatedAt: now}); err == nil {
			t.Errorf("expected ctx error on Put")
		}
	})

	t.Run("ScanByTool", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		_ = s.Put(ctx, Entry{Tool: "a", Hash: "1", TTL: time.Hour, CreatedAt: now})
		_ = s.Put(ctx, Entry{Tool: "a", Hash: "2", TTL: time.Hour, CreatedAt: now})
		_ = s.Put(ctx, Entry{Tool: "b", Hash: "1", TTL: time.Hour, CreatedAt: now})

		var seen []string
		err := s.Scan(ctx, "a", func(e Entry) bool {
			seen = append(seen, e.Hash)
			return true
		})
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if len(seen) != 2 {
			t.Errorf("expected 2 entries for tool 'a', got %d (%v)", len(seen), seen)
		}
	})

	t.Run("ScanSkipsExpired", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		_ = s.Put(ctx, Entry{Tool: "t", Hash: "live", TTL: time.Hour, CreatedAt: now})
		_ = s.Put(ctx, Entry{Tool: "t", Hash: "dead", TTL: time.Minute, CreatedAt: now})
		clock = now.Add(5 * time.Minute)

		count := 0
		_ = s.Scan(ctx, "t", func(e Entry) bool {
			if e.Hash == "dead" {
				t.Errorf("expired entry surfaced in Scan")
			}
			count++
			return true
		})
		if count != 1 {
			t.Errorf("expected 1 live entry, got %d", count)
		}
	})

	t.Run("ScanEarlyStop", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		for i := 0; i < 5; i++ {
			_ = s.Put(ctx, Entry{Tool: "t", Hash: string(rune('a' + i)), TTL: time.Hour, CreatedAt: now})
		}
		count := 0
		_ = s.Scan(ctx, "t", func(_ Entry) bool {
			count++
			return count < 2
		})
		if count != 2 {
			t.Errorf("expected early stop after 2, got %d", count)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		_ = s.Put(ctx, Entry{Tool: "t", Hash: "h", TTL: time.Hour, CreatedAt: now})
		if err := s.Delete(ctx, "t", "h"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := s.Get(ctx, "t", "h"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected entry gone after Delete, got %v", err)
		}
	})

	t.Run("DeleteMissing", func(t *testing.T) {
		s, cleanup := newStore(clockFn)
		defer cleanup()
		if err := s.Delete(context.Background(), "t", "h"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("EmbeddingRoundTrip", func(t *testing.T) {
		clock = now
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		emb := []float32{0.1, 0.2, 0.3, 0.4}
		_ = s.Put(ctx, Entry{Tool: "t", Hash: "h", Embedding: emb, TTL: time.Hour, CreatedAt: now})
		got, err := s.Get(ctx, "t", "h")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got.Embedding) != 4 {
			t.Fatalf("embedding len = %d, want 4", len(got.Embedding))
		}
		for i := range emb {
			if got.Embedding[i] != emb[i] {
				t.Errorf("embedding[%d] = %v, want %v", i, got.Embedding[i], emb[i])
			}
		}
	})

	t.Run("Concurrent", func(t *testing.T) {
		s, cleanup := newStore(clockFn)
		defer cleanup()
		ctx := context.Background()
		var wg sync.WaitGroup
		for i := 0; i < 30; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				e := Entry{Tool: "t", Hash: string(rune('a' + i%26)), TTL: time.Hour, CreatedAt: now}
				_ = s.Put(ctx, e)
				_, _ = s.Get(ctx, e.Tool, e.Hash)
			}(i)
		}
		wg.Wait()
	})
}

func TestMemory_Conformance(t *testing.T) {
	conformanceSuite(t, func(clock func() time.Time) (Store, func()) {
		return NewMemory(clock), func() {}
	})
}

func TestBolt_Conformance(t *testing.T) {
	conformanceSuite(t, func(clock func() time.Time) (Store, func()) {
		dir := t.TempDir()
		path := filepath.Join(dir, "test.db")
		b, err := OpenBolt(path, clock)
		if err != nil {
			t.Fatalf("OpenBolt: %v", err)
		}
		return b, func() { _ = b.Close() }
	})
}

func TestBolt_PersistsAcrossOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }

	b1, err := OpenBolt(path, clock)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := b1.Put(context.Background(), Entry{Tool: "t", Hash: "h", Response: []byte("hello"), TTL: time.Hour, CreatedAt: now}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_ = b1.Close()

	b2, err := OpenBolt(path, clock)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer b2.Close()

	got, err := b2.Get(context.Background(), "t", "h")
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if string(got.Response) != "hello" {
		t.Errorf("expected persisted response 'hello', got %q", got.Response)
	}
}

func TestBolt_OpenInvalidPath(t *testing.T) {
	_, err := OpenBolt("/no/such/dir/test.db", nil)
	if err == nil {
		t.Errorf("expected error for invalid path")
	}
}

func TestBolt_CompactRemovesExpiredEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compact.db")
	now := time.Unix(1_700_000_000, 0)
	clock := now
	b, err := OpenBolt(path, func() time.Time { return clock })
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer b.Close()
	ctx := context.Background()

	// seed 5 entries; 3 expire in 1 minute, 2 live for an hour
	for i := 0; i < 3; i++ {
		_ = b.Put(ctx, Entry{Tool: "t", Hash: string(rune('a' + i)), TTL: time.Minute, CreatedAt: now})
	}
	_ = b.Put(ctx, Entry{Tool: "t", Hash: "live1", TTL: time.Hour, CreatedAt: now})
	_ = b.Put(ctx, Entry{Tool: "t", Hash: "live2", TTL: time.Hour, CreatedAt: now})

	clock = now.Add(2 * time.Minute)
	removed, err := b.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if removed != 3 {
		t.Errorf("removed = %d, want 3", removed)
	}

	// the two live entries must still be there
	for _, h := range []string{"live1", "live2"} {
		if _, err := b.Get(ctx, "t", h); err != nil {
			t.Errorf("live entry %q gone after Compact: %v", h, err)
		}
	}
	// expired ones must be gone
	for i := 0; i < 3; i++ {
		if _, err := b.Get(ctx, "t", string(rune('a'+i))); !errors.Is(err, ErrNotFound) {
			t.Errorf("expired entry should be deleted, got %v", err)
		}
	}
}

func TestBolt_CompactIsNoOpWhenNothingExpired(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	b, _ := OpenBolt(filepath.Join(dir, "no_op.db"), func() time.Time { return now })
	defer b.Close()
	ctx := context.Background()

	_ = b.Put(ctx, Entry{Tool: "t", Hash: "x", TTL: time.Hour, CreatedAt: now})

	removed, err := b.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if removed != 0 {
		t.Errorf("expected 0 removals, got %d", removed)
	}
}

func TestBolt_RunCompactorRespectsContext(t *testing.T) {
	dir := t.TempDir()
	b, _ := OpenBolt(filepath.Join(dir, "ticker.db"), nil)
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	called := make(chan struct{}, 4)
	go b.RunCompactor(ctx, 10*time.Millisecond, func(int, error) {
		select {
		case called <- struct{}{}:
		default:
		}
	})

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatalf("compactor never ran")
	}
	cancel()
	// give the goroutine a moment to exit cleanly
	time.Sleep(30 * time.Millisecond)
}
