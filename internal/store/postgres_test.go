package store

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

// Postgres integration tests run only when POTENT_TEST_PG_DSN is set to a
// reachable database, e.g.
//
//	POTENT_TEST_PG_DSN="postgres://potent:potent@127.0.0.1:5432/potent_test?sslmode=disable" \
//	    go test ./internal/store/ -run TestPostgres
//
// In CI we run a postgres service container alongside the job. Locally,
// `docker run -p 5432:5432 -e POSTGRES_PASSWORD=potent postgres:16` is enough.

func TestPostgres_Conformance(t *testing.T) {
	dsn := os.Getenv("POTENT_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("POTENT_TEST_PG_DSN not set; skipping Postgres conformance suite")
	}
	conformanceSuite(t, func(clock func() time.Time) (Store, func()) {
		s, err := OpenPostgres(context.Background(), dsn, clock)
		if err != nil {
			t.Fatalf("OpenPostgres: %v", err)
		}
		// Each test gets a fresh table; truncate is faster than dropping.
		_, err = s.pool.Exec(context.Background(), `TRUNCATE potent_entries`)
		if err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return s, func() { s.Close() }
	})
}

func TestPostgres_CompactRemovesExpired(t *testing.T) {
	dsn := os.Getenv("POTENT_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("POTENT_TEST_PG_DSN not set")
	}
	now := time.Unix(1_700_000_000, 0)
	clock := now
	clockFn := func() time.Time { return clock }
	s, err := OpenPostgres(context.Background(), dsn, clockFn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	_, _ = s.pool.Exec(ctx, `TRUNCATE potent_entries`)

	// one expired, one fresh, one immortal
	_ = s.Put(ctx, Entry{Tool: "t", Hash: "h_expired", StatusCode: 200, TTL: time.Hour, CreatedAt: now.Add(-2 * time.Hour)})
	_ = s.Put(ctx, Entry{Tool: "t", Hash: "h_fresh", StatusCode: 200, TTL: time.Hour, CreatedAt: now})
	_ = s.Put(ctx, Entry{Tool: "t", Hash: "h_immortal", StatusCode: 200, TTL: 0, CreatedAt: now.Add(-100 * time.Hour)})

	removed, err := s.Compact(ctx)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := s.Get(ctx, "t", "h_expired"); err != ErrNotFound {
		t.Errorf("expired entry should be gone, got %v", err)
	}
	if _, err := s.Get(ctx, "t", "h_fresh"); err != nil {
		t.Errorf("fresh entry should survive: %v", err)
	}
	if _, err := s.Get(ctx, "t", "h_immortal"); err != nil {
		t.Errorf("immortal entry should survive: %v", err)
	}
}

func TestPostgres_OpenInvalidDSN(t *testing.T) {
	_, err := OpenPostgres(context.Background(), "not a real dsn", nil)
	if err == nil {
		t.Errorf("expected error for invalid DSN")
	}
}

func TestEncodeDecodeEmbedding_Roundtrip(t *testing.T) {
	cases := [][]float32{
		nil,
		{},
		{0},
		{1, -1, 0.5, math.MaxFloat32, math.SmallestNonzeroFloat32},
		make([]float32, 384), // typical embedding dimension
	}
	for i, in := range cases {
		out := decodeEmbedding(encodeEmbedding(in))
		if len(in) == 0 {
			if out != nil {
				t.Errorf("case %d: empty in -> %v, want nil", i, out)
			}
			continue
		}
		if len(out) != len(in) {
			t.Errorf("case %d: len %d != %d", i, len(out), len(in))
			continue
		}
		for j := range in {
			if out[j] != in[j] {
				t.Errorf("case %d index %d: %v != %v", i, j, out[j], in[j])
			}
		}
	}
}

func TestDecodeEmbedding_RejectsMalformed(t *testing.T) {
	// odd byte counts are not valid float32 arrays
	if got := decodeEmbedding([]byte{1, 2, 3}); got != nil {
		t.Errorf("decode of 3-byte input should be nil, got %v", got)
	}
	if got := decodeEmbedding([]byte{1, 2, 3, 4, 5}); got != nil {
		t.Errorf("decode of 5-byte input should be nil, got %v", got)
	}
}
