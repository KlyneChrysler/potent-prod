package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres is a PostgreSQL-backed Store. Designed for HA deployments where
// multiple potent instances share a single source of truth so a node going
// away does not lose the cache. Safe for concurrent use (pgxpool handles
// connection pooling internally).
//
// Schema (created on first open):
//
//	CREATE TABLE potent_entries (
//	    tool         text        NOT NULL,
//	    hash         text        NOT NULL,
//	    request      bytea,
//	    response     bytea,
//	    status_code  int         NOT NULL,
//	    created_at   timestamptz NOT NULL,
//	    ttl_ms       bigint      NOT NULL,
//	    replay_count int         NOT NULL DEFAULT 0,
//	    embedding    bytea,
//	    PRIMARY KEY (tool, hash)
//	);
//	CREATE INDEX potent_entries_tool_created ON potent_entries (tool, created_at);
//
// TTL is enforced lazily on Get (matching the Memory and Bolt adapters) and
// reclaimed in bulk by Compact / RunCompactor. The embedding is stored as
// IEEE-754 little-endian float32 bytes; the column is bytea instead of a
// vector type so the package has no pgvector dependency.
type Postgres struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// OpenPostgres connects to the database at dsn (a postgres:// URL or a
// libpq connection string), creates the schema if missing, and returns a
// Store. Pass time.Now for clock in production.
func OpenPostgres(ctx context.Context, dsn string, clock func() time.Time) (*Postgres, error) {
	if clock == nil {
		clock = time.Now
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	p := &Postgres{pool: pool, now: clock}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

// Close releases pooled connections.
func (p *Postgres) Close() { p.pool.Close() }

const createTableSQL = `
CREATE TABLE IF NOT EXISTS potent_entries (
    tool         text        NOT NULL,
    hash         text        NOT NULL,
    request      bytea,
    response     bytea,
    status_code  int         NOT NULL,
    created_at   timestamptz NOT NULL,
    ttl_ms       bigint      NOT NULL,
    replay_count int         NOT NULL DEFAULT 0,
    embedding    bytea,
    PRIMARY KEY (tool, hash)
);
CREATE INDEX IF NOT EXISTS potent_entries_tool_created
    ON potent_entries (tool, created_at);
`

func (p *Postgres) migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, createTableSQL); err != nil {
		return fmt.Errorf("postgres: migrate: %w", err)
	}
	return nil
}

// Get returns the cached entry. Expired entries are reported as ErrNotFound
// and deleted asynchronously so the read path stays single-roundtrip.
func (p *Postgres) Get(ctx context.Context, tool, hash string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	const q = `
		SELECT request, response, status_code, created_at, ttl_ms, replay_count, embedding
		FROM potent_entries
		WHERE tool = $1 AND hash = $2
	`
	row := p.pool.QueryRow(ctx, q, tool, hash)
	var (
		req, resp, emb []byte
		status         int
		createdAt      time.Time
		ttlMs          int64
		replays        int
	)
	if err := row.Scan(&req, &resp, &status, &createdAt, &ttlMs, &replays, &emb); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Entry{}, ErrNotFound
		}
		return Entry{}, fmt.Errorf("postgres: get: %w", err)
	}
	e := Entry{
		Tool:        tool,
		Hash:        hash,
		Request:     req,
		Response:    resp,
		StatusCode:  status,
		CreatedAt:   createdAt,
		TTL:         time.Duration(ttlMs) * time.Millisecond,
		ReplayCount: replays,
		Embedding:   decodeEmbedding(emb),
	}
	if e.Expired(p.now()) {
		// Best-effort async delete; do not block the read path. Intentionally
		// uses context.Background() (not the caller's ctx) so the cleanup
		// completes even if the caller's request is already returning
		// ErrNotFound. The 2s timeout caps the orphan goroutine lifetime.
		go func() { // #nosec G118 -- intentional background ctx; see comment above
			delCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _ = p.pool.Exec(delCtx, `DELETE FROM potent_entries WHERE tool = $1 AND hash = $2`, tool, hash)
		}()
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Put inserts or replaces the entry. Two potent instances racing on the
// same fingerprint converge on the most recent writer because the upsert
// uses LWW (last-write-wins); for correctness this is fine because the
// fingerprint hash already proves the requests are equivalent.
func (p *Postgres) Put(ctx context.Context, e Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.Tool == "" || e.Hash == "" {
		return errors.New("store: Entry.Tool and Entry.Hash are required")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = p.now()
	}
	const q = `
		INSERT INTO potent_entries
			(tool, hash, request, response, status_code, created_at, ttl_ms, replay_count, embedding)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (tool, hash) DO UPDATE SET
			request      = EXCLUDED.request,
			response     = EXCLUDED.response,
			status_code  = EXCLUDED.status_code,
			created_at   = EXCLUDED.created_at,
			ttl_ms       = EXCLUDED.ttl_ms,
			replay_count = EXCLUDED.replay_count,
			embedding    = EXCLUDED.embedding
	`
	_, err := p.pool.Exec(ctx, q,
		e.Tool, e.Hash, e.Request, e.Response, e.StatusCode,
		e.CreatedAt, e.TTL.Milliseconds(), e.ReplayCount, encodeEmbedding(e.Embedding),
	)
	if err != nil {
		return fmt.Errorf("postgres: put: %w", err)
	}
	return nil
}

// Delete removes a fingerprint. Returns ErrNotFound when absent so the
// admin API can render 404 idempotently.
func (p *Postgres) Delete(ctx context.Context, tool, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM potent_entries WHERE tool = $1 AND hash = $2`, tool, hash)
	if err != nil {
		return fmt.Errorf("postgres: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Scan iterates non-expired entries for the given tool. Uses a server-side
// cursor (pgx batches rows) so memory stays bounded for large tools.
func (p *Postgres) Scan(ctx context.Context, tool string, visit func(Entry) bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	const q = `
		SELECT hash, request, response, status_code, created_at, ttl_ms, replay_count, embedding
		FROM potent_entries
		WHERE tool = $1
		ORDER BY created_at DESC
	`
	rows, err := p.pool.Query(ctx, q, tool)
	if err != nil {
		return fmt.Errorf("postgres: scan: %w", err)
	}
	defer rows.Close()
	now := p.now()
	for rows.Next() {
		var (
			hash           string
			req, resp, emb []byte
			status         int
			createdAt      time.Time
			ttlMs          int64
			replays        int
		)
		if err := rows.Scan(&hash, &req, &resp, &status, &createdAt, &ttlMs, &replays, &emb); err != nil {
			return fmt.Errorf("postgres: scan row: %w", err)
		}
		e := Entry{
			Tool:        tool,
			Hash:        hash,
			Request:     req,
			Response:    resp,
			StatusCode:  status,
			CreatedAt:   createdAt,
			TTL:         time.Duration(ttlMs) * time.Millisecond,
			ReplayCount: replays,
			Embedding:   decodeEmbedding(emb),
		}
		if e.Expired(now) {
			continue
		}
		if !visit(e) {
			return nil
		}
	}
	return rows.Err()
}

// IncrementReplay bumps the replay counter atomically using a single
// UPDATE; no read-modify-write race.
func (p *Postgres) IncrementReplay(ctx context.Context, tool, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tag, err := p.pool.Exec(ctx,
		`UPDATE potent_entries SET replay_count = replay_count + 1 WHERE tool = $1 AND hash = $2`,
		tool, hash)
	if err != nil {
		return fmt.Errorf("postgres: increment_replay: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Compact deletes every expired entry in one statement. Returns the number
// of rows removed. Use RunCompactor to schedule periodic compaction.
//
// Server-side TTL evaluation: expression compares NOW() against the row's
// (created_at + ttl). Rows with ttl_ms = 0 are immortal and never deleted.
func (p *Postgres) Compact(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tag, err := p.pool.Exec(ctx, `
		DELETE FROM potent_entries
		WHERE ttl_ms > 0
		  AND created_at + (ttl_ms * INTERVAL '1 millisecond') < $1
	`, p.now())
	if err != nil {
		return 0, fmt.Errorf("postgres: compact: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RunCompactor runs Compact at the given interval until ctx is cancelled.
// Matches the Bolt adapter so the cmd/potent wiring is identical regardless
// of backend.
func (p *Postgres) RunCompactor(ctx context.Context, interval time.Duration, onResult func(removed int, err error)) {
	if interval <= 0 {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			n, err := p.Compact(ctx)
			if onResult != nil {
				onResult(n, err)
			}
		}
	}
}

// encodeEmbedding packs a []float32 as little-endian IEEE-754 bytes.
// Returns nil for empty input so the column stores NULL.
func encodeEmbedding(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// decodeEmbedding reverses encodeEmbedding. Malformed input (length not a
// multiple of 4) returns nil; the entry stays usable, just without the
// semantic vector.
func decodeEmbedding(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}
