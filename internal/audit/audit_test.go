package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpen_WritesJSONLLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	w, err := Open(path, 8)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	w.Write(Record{
		Tool: "send_email", Mode: "strict", Decision: "replay",
		Match: "exact", Similarity: 1.0, Hash: "abc", Protocol: "http",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		t.Fatalf("no line written")
	}
	var got Record
	if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, sc.Text())
	}
	if got.Tool != "send_email" || got.Decision != "replay" || got.Hash != "abc" {
		t.Errorf("record mismatch: %+v", got)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("timestamp was not stamped")
	}
}

func TestOpen_AppendsToExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")

	for i := 0; i < 2; i++ {
		w, err := Open(path, 8)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		w.Write(Record{Tool: "t", Hash: "h", Decision: "forward"})
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = w.Close(ctx)
		cancel()
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := 0
	for _, c := range b {
		if c == '\n' {
			lines++
		}
	}
	if lines != 2 {
		t.Errorf("expected 2 lines after append, got %d\nraw: %s", lines, b)
	}
}

func TestOpen_RejectsEmptyPath(t *testing.T) {
	if _, err := Open("", 8); err == nil {
		t.Errorf("expected error for empty path")
	}
}

func TestOpen_RejectsMissingParent(t *testing.T) {
	if _, err := Open("/no/such/dir/audit.log", 8); err == nil {
		t.Errorf("expected error for missing parent dir")
	}
}

func TestWriteAfterCloseIsNoop(t *testing.T) {
	w := NewDiscard()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// must not panic or block
	w.Write(Record{Tool: "t"})
}

func TestNewDiscard_AcceptsRecords(t *testing.T) {
	w := NewDiscard()
	for i := 0; i < 100; i++ {
		w.Write(Record{Tool: "t", Hash: "x", Decision: "forward"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := w.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestClose_IsIdempotent(t *testing.T) {
	w := NewDiscard()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = w.Close(ctx)
	if err := w.Close(ctx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
