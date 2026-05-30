package audit

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type fakePutter struct {
	mu      sync.Mutex
	puts    []putCall
	err     error
	gotBody []byte
}

type putCall struct {
	bucket string
	key    string
	body   []byte
}

func (f *fakePutter) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	body, _ := io.ReadAll(in.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.puts = append(f.puts, putCall{bucket: *in.Bucket, key: *in.Key, body: body})
	f.gotBody = body
	return &s3.PutObjectOutput{}, nil
}

func (f *fakePutter) calls() []putCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]putCall, len(f.puts))
	copy(out, f.puts)
	return out
}

func TestParseS3URL(t *testing.T) {
	cases := []struct {
		in            string
		bucket, prefix string
		wantErr       bool
	}{
		{"s3://my-bucket", "my-bucket", "", false},
		{"s3://my-bucket/audit", "my-bucket", "audit", false},
		{"s3://my-bucket/a/b/c/", "my-bucket", "a/b/c", false},
		{"s3://", "", "", true},
		{"my-bucket", "", "", true},
		{"s3:///prefix", "", "", true},
	}
	for _, c := range cases {
		b, p, err := ParseS3URL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: want error, got bucket=%q prefix=%q", c.in, b, p)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected err %v", c.in, err)
			continue
		}
		if b != c.bucket || p != c.prefix {
			t.Errorf("%q: bucket=%q prefix=%q, want %q %q", c.in, b, p, c.bucket, c.prefix)
		}
	}
}

func TestNewS3Sink_RequiresBucket(t *testing.T) {
	_, err := NewS3Sink(context.Background(), S3SinkOptions{Client: &fakePutter{}})
	if err == nil {
		t.Fatalf("expected error for empty bucket")
	}
}

func TestS3Sink_FlushOnClose(t *testing.T) {
	fake := &fakePutter{}
	now := time.Date(2026, 5, 30, 14, 32, 5, 0, time.UTC)
	s, err := NewS3Sink(context.Background(), S3SinkOptions{
		Bucket:        "b",
		Prefix:        "potent/audit",
		FlushInterval: time.Hour,
		FlushBytes:    1 << 20,
		Client:        fake,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewS3Sink: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Write(Record{Tool: "t", Hash: "h", Decision: "replay", Timestamp: now}); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := fake.calls()
	if len(calls) != 1 {
		t.Fatalf("want 1 put, got %d", len(calls))
	}
	c := calls[0]
	if c.bucket != "b" {
		t.Errorf("bucket=%q", c.bucket)
	}
	wantKey := "potent/audit/year=2026/month=05/day=30/hour=14/audit-20260530T143205Z.jsonl"
	if c.key != wantKey {
		t.Errorf("key=%q want %q", c.key, wantKey)
	}
	if lines := strings.Count(string(c.body), "\n"); lines != 3 {
		t.Errorf("want 3 lines, got %d: %s", lines, c.body)
	}
}

func TestS3Sink_FlushOnByteThreshold(t *testing.T) {
	fake := &fakePutter{}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s, err := NewS3Sink(context.Background(), S3SinkOptions{
		Bucket:        "b",
		FlushInterval: time.Hour,
		FlushBytes:    200, // tiny threshold to force mid-stream flush
		Client:        fake,
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewS3Sink: %v", err)
	}
	for i := 0; i < 20; i++ {
		_ = s.Write(Record{Tool: "send_email", Hash: "abcdef", Decision: "replay", Timestamp: now})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Close(ctx)
	if got := len(fake.calls()); got < 2 {
		t.Errorf("expected multiple flushes, got %d", got)
	}
}

func TestS3Sink_NoFlushWhenEmpty(t *testing.T) {
	fake := &fakePutter{}
	s, err := NewS3Sink(context.Background(), S3SinkOptions{
		Bucket: "b", FlushInterval: 10 * time.Millisecond, Client: fake,
	})
	if err != nil {
		t.Fatalf("NewS3Sink: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Close(ctx)
	if got := len(fake.calls()); got != 0 {
		t.Errorf("expected zero puts for empty buffer, got %d", got)
	}
}

func TestS3Sink_PutErrorIsLoggedNotPanicked(t *testing.T) {
	fake := &fakePutter{err: errors.New("boom")}
	s, err := NewS3Sink(context.Background(), S3SinkOptions{
		Bucket: "b", FlushInterval: time.Hour, Client: fake,
	})
	if err != nil {
		t.Fatalf("NewS3Sink: %v", err)
	}
	_ = s.Write(Record{Tool: "t", Hash: "h", Decision: "replay"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.Close(ctx)
	// no assertion beyond "did not panic"; the error path was exercised.
}

func TestS3Sink_WriteAfterCloseReturnsError(t *testing.T) {
	s, err := NewS3Sink(context.Background(), S3SinkOptions{
		Bucket: "b", Client: &fakePutter{},
	})
	if err != nil {
		t.Fatalf("NewS3Sink: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.Close(ctx)
	if err := s.Write(Record{Tool: "t"}); err == nil {
		t.Errorf("expected error writing after close")
	}
}

func TestWriter_FansOutToMultipleSinks(t *testing.T) {
	a := &countingSink{}
	b := &countingSink{}
	w := NewWriter(a, b)
	for i := 0; i < 5; i++ {
		w.Write(Record{Tool: "t", Hash: "h", Decision: "forward"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = w.Close(ctx)
	if a.n != 5 || b.n != 5 {
		t.Errorf("fanout counts: a=%d b=%d", a.n, b.n)
	}
}

type countingSink struct {
	mu sync.Mutex
	n  int
}

func (c *countingSink) Write(Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}
func (c *countingSink) Close(context.Context) error { return nil }
