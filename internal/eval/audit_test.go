package eval

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/potent/potent/internal/audit"
)

func TestLoadAuditLog_SkipsMalformedLines(t *testing.T) {
	const data = `{"tool":"send_email","decision":"forward","hash":"h1"}
this is not json
{"tool":"send_email","decision":"replay","hash":"h1"}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadAuditLog(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d records, want 2 (the malformed line must be skipped)", len(got))
	}
}

func TestSummarize_AggregatesAcrossTools(t *testing.T) {
	records := []audit.Record{
		{Timestamp: time.Now(), Tool: "send_email", Decision: "forward", Hash: "h1"},
		{Timestamp: time.Now(), Tool: "send_email", Decision: "replay", Hash: "h1"},
		{Timestamp: time.Now(), Tool: "send_email", Decision: "replay", Hash: "h1"},
		{Timestamp: time.Now(), Tool: "charge_card", Decision: "forward", Hash: "h2"},
	}
	s := Summarize(records)
	if s.Records != 4 {
		t.Errorf("records = %d, want 4", s.Records)
	}
	se := s.Tools["send_email"]
	if se.Records != 3 {
		t.Errorf("send_email records = %d, want 3", se.Records)
	}
	if se.WouldDecisions["replay"] != 2 {
		t.Errorf("send_email replays = %d, want 2", se.WouldDecisions["replay"])
	}
	if se.WouldReplayRate < 0.66 || se.WouldReplayRate > 0.67 {
		t.Errorf("send_email replay rate = %v, want ~0.667", se.WouldReplayRate)
	}
	if se.UniqueHashes != 1 {
		t.Errorf("send_email unique hashes = %d, want 1", se.UniqueHashes)
	}
	if se.TopRepeatedHash != "h1" || se.TopRepeatedCount != 3 {
		t.Errorf("send_email top = %q (%d), want h1 (3)", se.TopRepeatedHash, se.TopRepeatedCount)
	}
}

func TestSummarize_PrefersWouldDecisionWhenSet(t *testing.T) {
	// In a shadow run, Decision is always "forward" but WouldDecision
	// carries the policy view. Summarize should report the WouldDecision
	// distribution.
	records := []audit.Record{
		{Tool: "send_email", Decision: "forward", WouldDecision: "replay", Hash: "h1"},
		{Tool: "send_email", Decision: "forward", WouldDecision: "replay", Hash: "h1"},
		{Tool: "send_email", Decision: "forward", WouldDecision: "forward", Hash: "h2"},
	}
	s := Summarize(records)
	se := s.Tools["send_email"]
	if se.WouldDecisions["replay"] != 2 || se.WouldDecisions["forward"] != 1 {
		t.Errorf("got would_decisions = %+v, want replay=2 forward=1", se.WouldDecisions)
	}
	if se.WouldReplayRate < 0.66 || se.WouldReplayRate > 0.67 {
		t.Errorf("would_replay_rate = %v, want ~0.667", se.WouldReplayRate)
	}
}

func TestSummarize_SortedTools(t *testing.T) {
	records := []audit.Record{
		{Tool: "zeta"}, {Tool: "alpha"}, {Tool: "mu"},
	}
	s := Summarize(records)
	got := s.SortedTools()
	want := []string{"alpha", "mu", "zeta"}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("got[%d] = %q, want %q", i, got[i], w)
		}
	}
}
