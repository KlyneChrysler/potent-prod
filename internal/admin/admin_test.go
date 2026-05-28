package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/potent/potent/internal/store"
)

func seedStore(t *testing.T) store.Store {
	t.Helper()
	s := store.NewMemory(nil)
	ctx := context.Background()
	base := time.Now()
	for i := 0; i < 3; i++ {
		_ = s.Put(ctx, store.Entry{
			Tool: "send_email", Hash: string(rune('a' + i)),
			StatusCode: 200, TTL: time.Hour,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	_ = s.Put(ctx, store.Entry{Tool: "other", Hash: "z", TTL: time.Hour, CreatedAt: base})
	return s
}

func TestHandler_ListFingerprintsByTool(t *testing.T) {
	s := seedStore(t)
	srv := httptest.NewServer(Handler(s, s))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/fingerprints?tool=send_email")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got []FingerprintView
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("expected 3 entries, got %d", len(got))
	}
}

func TestHandler_ListRequiresTool(t *testing.T) {
	s := seedStore(t)
	srv := httptest.NewServer(Handler(s, s))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/fingerprints")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
}

func TestHandler_StatsAggregates(t *testing.T) {
	s := seedStore(t)
	srv := httptest.NewServer(Handler(s, s))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/stats?tool=send_email")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var stats Stats
	_ = json.NewDecoder(resp.Body).Decode(&stats)
	ts := stats.Tools["send_email"]
	if ts.Entries != 3 {
		t.Errorf("entries = %d, want 3", ts.Entries)
	}
	if ts.OldestEntry.After(ts.NewestEntry) {
		t.Errorf("oldest > newest")
	}
}

func TestHandler_DeleteRemovesEntry(t *testing.T) {
	s := seedStore(t)
	srv := httptest.NewServer(Handler(s, s))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/fingerprints/send_email/a", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body=%s", resp.StatusCode, body)
	}

	if _, err := s.Get(context.Background(), "send_email", "a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("entry not deleted: %v", err)
	}
}

func TestHandler_DeleteBadPath(t *testing.T) {
	s := seedStore(t)
	srv := httptest.NewServer(Handler(s, s))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/fingerprints/onlyone", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestHandler_DeleteWithoutEraserReturns501(t *testing.T) {
	s := seedStore(t)
	srv := httptest.NewServer(Handler(s, nil)) // no eraser
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/fingerprints/send_email/a", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestHandler_FingerprintsRejectsPost(t *testing.T) {
	s := seedStore(t)
	srv := httptest.NewServer(Handler(s, s))
	defer srv.Close()
	resp, _ := http.Post(srv.URL+"/fingerprints", "application/json", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestSplitToolHash(t *testing.T) {
	cases := []struct {
		path    string
		tool    string
		hash    string
		ok      bool
	}{
		{"/fingerprints/send_email/abc", "send_email", "abc", true},
		{"/fingerprints/scoped/name/abc", "scoped/name", "abc", true},
		{"/fingerprints/onlyone", "", "", false},
		{"/fingerprints/", "", "", false},
		{"/something", "", "", false},
	}
	for _, c := range cases {
		tool, hash, ok := splitToolHash(c.path)
		if ok != c.ok || tool != c.tool || hash != c.hash {
			t.Errorf("split(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.path, tool, hash, ok, c.tool, c.hash, c.ok)
		}
	}
}
