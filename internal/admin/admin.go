// Package admin exposes operator-facing HTTP endpoints for inspecting and
// invalidating the fingerprint cache.
//
// The admin server runs on its own port — never the user-facing proxy
// port — so it can be bound to localhost or fronted by separate auth
// without affecting tool-call traffic.
package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/potent/potent/internal/auth"
	"github.com/potent/potent/internal/store"
)

// Inspector is the subset of store.Store that the admin API needs. Defining
// it here keeps admin from depending on Scan/Get/IncrementReplay surface
// not exposed yet for moderation.
type Inspector interface {
	Scan(ctx context.Context, tool string, visit func(store.Entry) bool) error
}

// Eraser deletes a single fingerprint. The Store interface doesn't expose
// delete yet; admin gates that to its own narrower port to avoid bloating
// the persistence contract until a second caller appears.
type Eraser interface {
	Delete(ctx context.Context, tool, hash string) error
}

// Handler returns an http.Handler that serves admin endpoints. If er is
// nil, DELETE is disabled and the endpoint returns 501 Not Implemented.
//
// The token, when non-empty, gates every endpoint: requests must carry
// "Authorization: Bearer <token>" with a constant-time match. Construct a
// Handler with an empty token only for tests; cmd/potent refuses to start
// the admin server without one.
func Handler(in Inspector, er Eraser, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		stats := computeStats(r.Context(), in, r.URL.Query().Get("tool"))
		writeJSON(w, http.StatusOK, stats)
	})
	mux.HandleFunc("/fingerprints", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			listFingerprints(w, r, in)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/fingerprints/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if er == nil {
			http.Error(w, "delete not configured", http.StatusNotImplemented)
			return
		}
		tool, hash, ok := splitToolHash(r.URL.Path)
		if !ok {
			http.Error(w, "expected /fingerprints/{tool}/{hash}", http.StatusBadRequest)
			return
		}
		if err := er.Delete(r.Context(), tool, hash); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return auth.RequireBearer(token, "potent-admin", mux)
}

// Stats summarizes cache state per tool.
type Stats struct {
	Tools map[string]ToolStats `json:"tools"`
}

// ToolStats aggregates one tool's entries.
type ToolStats struct {
	Entries      int       `json:"entries"`
	Replays      int       `json:"replays"`
	OldestEntry  time.Time `json:"oldest_entry,omitempty"`
	NewestEntry  time.Time `json:"newest_entry,omitempty"`
}

func computeStats(ctx context.Context, in Inspector, tool string) Stats {
	out := Stats{Tools: map[string]ToolStats{}}
	if tool == "" {
		return out
	}
	var stats ToolStats
	_ = in.Scan(ctx, tool, func(e store.Entry) bool {
		stats.Entries++
		stats.Replays += e.ReplayCount
		if stats.OldestEntry.IsZero() || e.CreatedAt.Before(stats.OldestEntry) {
			stats.OldestEntry = e.CreatedAt
		}
		if e.CreatedAt.After(stats.NewestEntry) {
			stats.NewestEntry = e.CreatedAt
		}
		return true
	})
	out.Tools[tool] = stats
	return out
}

// FingerprintView is a redacted view of an Entry suitable for /fingerprints
// listings. Raw request/response bytes are never returned over admin — only
// metadata, so admin can't be used to exfiltrate cached payloads.
type FingerprintView struct {
	Tool        string    `json:"tool"`
	Hash        string    `json:"hash"`
	StatusCode  int       `json:"status_code"`
	CreatedAt   time.Time `json:"created_at"`
	ReplayCount int       `json:"replay_count"`
	TTLSeconds  int64     `json:"ttl_seconds"`
}

func listFingerprints(w http.ResponseWriter, r *http.Request, in Inspector) {
	tool := r.URL.Query().Get("tool")
	if tool == "" {
		http.Error(w, "tool query param is required", http.StatusBadRequest)
		return
	}
	out := []FingerprintView{}
	err := in.Scan(r.Context(), tool, func(e store.Entry) bool {
		out = append(out, FingerprintView{
			Tool:        e.Tool,
			Hash:        e.Hash,
			StatusCode:  e.StatusCode,
			CreatedAt:   e.CreatedAt,
			ReplayCount: e.ReplayCount,
			TTLSeconds:  int64(e.TTL.Seconds()),
		})
		return true
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func splitToolHash(path string) (string, string, bool) {
	// path looks like "/fingerprints/{tool}/{hash}"
	const prefix = "/fingerprints/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	idx := strings.LastIndex(rest, "/")
	if idx <= 0 || idx == len(rest)-1 {
		return "", "", false
	}
	return rest[:idx], rest[idx+1:], true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
