package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Putter is the minimal AWS S3 surface this package depends on. It is
// satisfied by *s3.Client and any test fake. Keeping the interface this
// narrow makes the sink trivial to unit-test without spinning up AWS.
type S3Putter interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// S3SinkOptions configures the S3 audit sink.
type S3SinkOptions struct {
	// Bucket is the destination S3 bucket. Required.
	Bucket string
	// Prefix is prepended to every key (e.g. "potent/audit"). Trailing
	// slashes are normalised.
	Prefix string
	// FlushInterval is the longest a record may sit in memory before being
	// uploaded. Defaults to 5 minutes if zero.
	FlushInterval time.Duration
	// FlushBytes triggers an upload when buffered records exceed this
	// many bytes. Defaults to 5 MiB if zero.
	FlushBytes int
	// Capacity sizes the inbound channel. Defaults to 1024.
	Capacity int
	// Client overrides the default S3 client (loaded from environment).
	// Test code should always set this.
	Client S3Putter
	// Now overrides the clock for tests.
	Now func() time.Time
	// Logger receives structured upload errors. Defaults to slog.Default().
	Logger *slog.Logger
}

// ParseS3URL parses an s3://bucket[/prefix] URL into its components.
func ParseS3URL(raw string) (bucket, prefix string, err error) {
	if !strings.HasPrefix(raw, "s3://") {
		return "", "", fmt.Errorf("audit: s3 url must start with s3:// (got %q)", raw)
	}
	rest := strings.TrimPrefix(raw, "s3://")
	if rest == "" {
		return "", "", errors.New("audit: s3 url has no bucket")
	}
	parts := strings.SplitN(rest, "/", 2)
	bucket = parts[0]
	if bucket == "" {
		return "", "", errors.New("audit: s3 url has empty bucket")
	}
	if len(parts) == 2 {
		prefix = parts[1]
	}
	return bucket, strings.TrimSuffix(prefix, "/"), nil
}

// NewS3Sink returns an audit sink that batches records in memory and
// uploads them to S3 either every opts.FlushInterval or once the buffer
// crosses opts.FlushBytes, whichever comes first.
//
// Keys are Hive-partitioned (year=/month=/day=/hour=) so Athena, BigQuery
// External, Trino, and Spark all auto-discover the partitions without
// extra work.
func NewS3Sink(ctx context.Context, opts S3SinkOptions) (Sink, error) {
	if opts.Bucket == "" {
		return nil, errors.New("audit: s3 bucket is required")
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = 5 * time.Minute
	}
	if opts.FlushBytes <= 0 {
		opts.FlushBytes = 5 * 1024 * 1024
	}
	if opts.Capacity <= 0 {
		opts.Capacity = 1024
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Client == nil {
		cfg, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			return nil, fmt.Errorf("audit: load aws config: %w", err)
		}
		opts.Client = s3.NewFromConfig(cfg)
	}
	s := &s3Sink{
		opts:   opts,
		prefix: strings.TrimSuffix(opts.Prefix, "/"),
		ch:     make(chan Record, opts.Capacity),
		closed: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.run()
	return s, nil
}

type s3Sink struct {
	opts   S3SinkOptions
	prefix string
	ch     chan Record
	wg     sync.WaitGroup
	closed chan struct{}
	once   sync.Once
}

func (s *s3Sink) Write(r Record) error {
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

func (s *s3Sink) Close(ctx context.Context) error {
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

func (s *s3Sink) run() {
	defer s.wg.Done()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	var firstAt time.Time
	tick := time.NewTicker(s.opts.FlushInterval)
	defer tick.Stop()

	flush := func() {
		if buf.Len() == 0 {
			return
		}
		key := s.partitionKey(firstAt)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err := s.opts.Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:      aws.String(s.opts.Bucket),
			Key:         aws.String(key),
			Body:        bytes.NewReader(buf.Bytes()),
			ContentType: aws.String("application/x-ndjson"),
		})
		if err != nil {
			s.opts.Logger.Error("audit s3 put failed",
				"bucket", s.opts.Bucket, "key", key, "bytes", buf.Len(), "err", err)
		}
		buf.Reset()
		firstAt = time.Time{}
	}
	defer flush()

	for {
		select {
		case r, ok := <-s.ch:
			if !ok {
				return
			}
			if firstAt.IsZero() {
				firstAt = s.opts.Now()
			}
			if err := enc.Encode(r); err != nil {
				s.opts.Logger.Error("audit s3 encode failed", "err", err)
				continue
			}
			if buf.Len() >= s.opts.FlushBytes {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

// partitionKey returns a Hive-style S3 key, e.g.
// "<prefix>/year=2026/month=05/day=30/hour=14/audit-20260530T143205Z.jsonl".
func (s *s3Sink) partitionKey(t time.Time) string {
	if t.IsZero() {
		t = s.opts.Now()
	}
	u := t.UTC()
	stamp := u.Format("20060102T150405Z")
	key := fmt.Sprintf("year=%04d/month=%02d/day=%02d/hour=%02d/audit-%s.jsonl",
		u.Year(), int(u.Month()), u.Day(), u.Hour(), stamp)
	if s.prefix != "" {
		key = s.prefix + "/" + key
	}
	return key
}
