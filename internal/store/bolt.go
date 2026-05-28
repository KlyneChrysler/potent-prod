package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"
)

// bucketName is the single top-level bucket; per-tool nesting happens inside.
var bucketName = []byte("entries")

// Bolt is a BoltDB-backed Store. Safe for concurrent use (bbolt handles
// reader/writer coordination internally).
type Bolt struct {
	db  *bolt.DB
	now func() time.Time
}

// OpenBolt opens (or creates) a BoltDB file at path. The clock function is
// used for expiry checks; pass time.Now in production.
func OpenBolt(path string, clock func() time.Time) (*Bolt, error) {
	if clock == nil {
		clock = time.Now
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bolt %q: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucketName)
		return e
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init bucket: %w", err)
	}
	return &Bolt{db: db, now: clock}, nil
}

// Close releases the underlying database file.
func (b *Bolt) Close() error { return b.db.Close() }

// Get returns the cached entry. Expired entries are reported as ErrNotFound
// and lazily deleted.
func (b *Bolt) Get(ctx context.Context, tool, hash string) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	var e Entry
	k := []byte(key(tool, hash))
	err := b.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketName).Get(k)
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &e)
	})
	if err != nil {
		return Entry{}, err
	}
	if e.Expired(b.now()) {
		_ = b.db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket(bucketName).Delete(k)
		})
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Put persists the entry, overwriting any existing entry for the same key.
func (b *Bolt) Put(ctx context.Context, e Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.Tool == "" || e.Hash == "" {
		return errors.New("store: Entry.Tool and Entry.Hash are required")
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = b.now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}
	return b.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketName).Put([]byte(key(e.Tool, e.Hash)), data)
	})
}

// IncrementReplay reads, mutates, and writes back the entry atomically inside
// a single bbolt transaction.
func (b *Bolt) IncrementReplay(ctx context.Context, tool, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	k := []byte(key(tool, hash))
	return b.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket(bucketName)
		v := bkt.Get(k)
		if v == nil {
			return ErrNotFound
		}
		var e Entry
		if err := json.Unmarshal(v, &e); err != nil {
			return fmt.Errorf("unmarshal entry: %w", err)
		}
		e.ReplayCount++
		data, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("marshal entry: %w", err)
		}
		return bkt.Put(k, data)
	})
}
