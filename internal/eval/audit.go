package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/potent/potent/internal/audit"
)

// AuditSummary aggregates a shadow-mode audit log into the numbers an
// operator needs to decide whether to flip the policy on. Per-tool
// breakdowns include the would-be decision distribution, the unique
// fingerprint count (a proxy for cardinality), and the top hashes by
// repeat count (potential cache savings).
type AuditSummary struct {
	Records         int                    `json:"records"`
	Tools           map[string]*ToolReport `json:"tools"`
}

// ToolReport is one tool's section of the summary.
type ToolReport struct {
	Records          int            `json:"records"`
	WouldDecisions   map[string]int `json:"would_decisions"`
	UniqueHashes     int            `json:"unique_hashes"`
	TopRepeatedHash  string         `json:"top_repeated_hash,omitempty"`
	TopRepeatedCount int            `json:"top_repeated_count,omitempty"`
	WouldReplayRate  float64        `json:"would_replay_rate"`
}

// LoadAuditLog parses a jsonl audit file produced by potent (typically
// with -shadow-mode enabled). Lines that fail to parse as a Record are
// skipped silently so a partially-rotated file still summarizes.
func LoadAuditLog(path string) ([]audit.Record, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	return readAuditLog(f)
}

func readAuditLog(r io.Reader) ([]audit.Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var out []audit.Record
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var rec audit.Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			continue
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// Summarize collapses an audit-log slice into per-tool reports.
// WouldReplayRate is the fraction of the tool's records whose
// WouldDecision is "replay" (or, for non-shadow logs, whose Decision is
// "replay"). That fraction is the headline number for an operator deciding
// whether the cache pays for itself.
func Summarize(records []audit.Record) AuditSummary {
	out := AuditSummary{Records: len(records), Tools: map[string]*ToolReport{}}
	hashCounts := map[string]map[string]int{}
	for _, r := range records {
		tr, ok := out.Tools[r.Tool]
		if !ok {
			tr = &ToolReport{WouldDecisions: map[string]int{}}
			out.Tools[r.Tool] = tr
			hashCounts[r.Tool] = map[string]int{}
		}
		tr.Records++
		// In shadow runs the would-be decision is in WouldDecision; in
		// production runs it is the Decision itself.
		dec := r.WouldDecision
		if dec == "" {
			dec = r.Decision
		}
		tr.WouldDecisions[dec]++
		if r.Hash != "" {
			hashCounts[r.Tool][r.Hash]++
		}
	}
	for tool, tr := range out.Tools {
		var top string
		var topN int
		for h, n := range hashCounts[tool] {
			if n > topN {
				top, topN = h, n
			}
		}
		tr.UniqueHashes = len(hashCounts[tool])
		tr.TopRepeatedHash = top
		tr.TopRepeatedCount = topN
		if tr.Records > 0 {
			tr.WouldReplayRate = float64(tr.WouldDecisions["replay"]) / float64(tr.Records)
		}
	}
	return out
}

// SortedTools returns tool names sorted alphabetically; helps stabilize
// human-readable output.
func (s AuditSummary) SortedTools() []string {
	out := make([]string, 0, len(s.Tools))
	for k := range s.Tools {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
