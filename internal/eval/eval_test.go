package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/policy"
)

func TestLoadDataset_SkipsBlankAndCommentLines(t *testing.T) {
	const data = `# a comment
{"id":"a","tool":"send_email","intent_id":"i1","args":{"to":"x"}}

{"id":"b","tool":"send_email","intent_id":"i1","args":{"to":"y"}}
`
	dir := t.TempDir()
	path := filepath.Join(dir, "ds.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	recs, err := LoadDataset(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(recs) != 2 {
		t.Errorf("got %d, want 2", len(recs))
	}
}

func TestLoadDataset_RejectsMissingFields(t *testing.T) {
	const data = `{"id":"a","tool":"send_email"}`
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadDataset(path); err == nil {
		t.Errorf("expected error for missing intent_id")
	}
}

func TestBuildPairs_OnlyWithinSameTool(t *testing.T) {
	records := []Record{
		{ID: "a", Tool: "send_email", IntentID: "i1"},
		{ID: "b", Tool: "send_email", IntentID: "i1"},
		{ID: "c", Tool: "send_email", IntentID: "i2"},
		{ID: "d", Tool: "charge_card", IntentID: "i1"},
	}
	pairs := BuildPairs(records)
	// 3 same-tool pairs in send_email; 1 in charge_card (singleton) -> 0; total 3
	if len(pairs) != 3 {
		t.Errorf("expected 3 pairs, got %d", len(pairs))
	}
	for _, p := range pairs {
		if p.A.Tool != p.B.Tool {
			t.Errorf("pair spans tools: %v / %v", p.A.Tool, p.B.Tool)
		}
	}
}

func TestRunner_ScoreAndEvaluate_SmallCorpus(t *testing.T) {
	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {
			Mode:              policy.ModeStrict,
			FingerprintFields: []string{"to", "body"},
			Normalize: map[string][]string{
				"to": {"lowercase", "trim"},
			},
		},
	}}
	emb, _ := embed.NewHashingTFIDF(384, 4)
	runner := NewRunner(cfg, emb)

	records := []Record{
		{ID: "a", Tool: "send_email", IntentID: "i-alice", Args: map[string]any{"to": "alice@example.com", "body": "Hello Alice, here is the report."}},
		{ID: "b", Tool: "send_email", IntentID: "i-alice", Args: map[string]any{"to": " ALICE@example.com ", "body": "Hello Alice, here is the report."}},
		{ID: "c", Tool: "send_email", IntentID: "i-bob", Args: map[string]any{"to": "bob@example.com", "body": "Hello Bob, here is the report."}},
	}
	pairs := BuildPairs(records)
	if len(pairs) != 3 {
		t.Fatalf("expected 3 pairs, got %d", len(pairs))
	}

	metrics, err := runner.Evaluate(pairs)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	best := BestThreshold(metrics)
	got, ok := best["send_email"]
	if !ok {
		t.Fatalf("no metrics for send_email")
	}
	// Threshold tied at multiple points; we picked the highest. At least
	// verify there are no negative counts and the matrix is consistent.
	if got.TP+got.FP+got.TN+got.FN != len(pairs) {
		t.Errorf("matrix does not sum to pair count")
	}
	if got.Precision() < 0 || got.Recall() < 0 {
		t.Errorf("negative ratios")
	}
}

func TestBestThreshold_TieBreaksOnHigherThreshold(t *testing.T) {
	// Two thresholds with equal F1: the higher one wins (more conservative
	// is safer for side-effecting tools).
	metrics := []Metrics{
		{Tool: "t", Threshold: 0.70, TP: 1, FP: 1, TN: 1, FN: 0},
		{Tool: "t", Threshold: 0.90, TP: 1, FP: 1, TN: 1, FN: 0},
	}
	best := BestThreshold(metrics)
	if best["t"].Threshold != 0.90 {
		t.Errorf("got threshold %v, want 0.90", best["t"].Threshold)
	}
}

func TestMetrics_Ratios_EdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		m     Metrics
		prec  float64
		rec   float64
		fpr   float64
		f1    float64
	}{
		{"all zero", Metrics{}, 0, 0, 0, 0},
		{"perfect", Metrics{TP: 10, TN: 10}, 1.0, 1.0, 0, 1.0},
		{"only false positives", Metrics{FP: 5, TN: 5}, 0, 0, 0.5, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.m.Precision() != c.prec || c.m.Recall() != c.rec || c.m.FalsePositiveRate() != c.fpr || c.m.F1() != c.f1 {
				t.Errorf("got p=%v r=%v fpr=%v f1=%v; want p=%v r=%v fpr=%v f1=%v",
					c.m.Precision(), c.m.Recall(), c.m.FalsePositiveRate(), c.m.F1(),
					c.prec, c.rec, c.fpr, c.f1)
			}
		})
	}
}

func TestPairCategory_ClassifiesCorrectly(t *testing.T) {
	cases := []struct {
		a, b Record
		want string
	}{
		{Record{Category: "baseline"}, Record{Category: "whitespace-casing"}, "whitespace-casing"},
		{Record{Category: "paraphrase"}, Record{Category: "paraphrase"}, "paraphrase"},
		{Record{Category: ""}, Record{Category: ""}, "different-intent-uncategorized"},
		{Record{Category: "x"}, Record{Category: "y"}, "x+y"},
	}
	for _, c := range cases {
		// Force different intent_ids so the uncategorized case yields the
		// "different-intent" label.
		c.a.IntentID = "i1"
		c.b.IntentID = "i2"
		got := pairCategory(c.a, c.b)
		if got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

func TestLoadDataset_DatasetFile_IsValid(t *testing.T) {
	// Smoke test that the curated dataset file parses cleanly so a
	// malformed line in the eval corpus fails CI before we report
	// stale results.
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	path := filepath.Join(repoRoot, "docs", "eval", "dataset.jsonl")
	if _, err := os.Stat(path); err != nil {
		t.Skip("dataset file not present; skipping")
	}
	recs, err := LoadDataset(path)
	if err != nil {
		t.Fatalf("load curated dataset: %v", err)
	}
	if len(recs) < 30 {
		t.Errorf("dataset has only %d records; expected at least 30 for a credible eval", len(recs))
	}
	// Every record must have a tool, intent_id, and at least one arg.
	for _, r := range recs {
		if strings.TrimSpace(r.Tool) == "" || strings.TrimSpace(r.IntentID) == "" {
			t.Errorf("record %q missing tool or intent_id", r.ID)
		}
		if len(r.Args) == 0 {
			t.Errorf("record %q has empty args", r.ID)
		}
	}
}
