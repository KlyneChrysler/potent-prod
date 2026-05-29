// Package eval drives offline measurement of potent's semantic-dedup
// quality. Operators (and CI) feed it a labelled dataset of tool calls;
// the runner exercises the same normalizer + fingerprint + embedder
// pipeline used at request time and reports precision/recall at every
// candidate threshold so the per-tool semantic_threshold can be tuned
// from data instead of guessed.
//
// The dataset format is jsonl, one tool call per line. The intent_id
// label is the ground truth: two records with the same (tool, intent_id)
// MUST dedup; two records with the same tool and different intent_ids
// MUST NOT.
package eval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/policy"
)

// Record is one labelled tool call in the eval set. ID identifies the
// record itself (for traceability when a pair surprises us); IntentID is
// the ground-truth equivalence class within a tool. Category is a free-text
// tag that describes how the variation differs from the canonical form
// (e.g. "whitespace", "reorder", "paraphrase", "adversarial-different-id");
// it shows up in the per-category breakdown so operators can see which
// kinds of retry the embedder is good or bad at catching.
type Record struct {
	ID       string                 `json:"id"`
	Tool     string                 `json:"tool"`
	IntentID string                 `json:"intent_id"`
	Category string                 `json:"category,omitempty"`
	Args     map[string]any         `json:"args"`
}

// Pair is the unit the eval reports on: two records from the same tool
// with a known ground truth (SameIntent reflects whether they share an
// intent_id, which means dedup should fire).
type Pair struct {
	A, B       Record
	SameIntent bool
	Category   string // pair category derived from B's record category
}

// Score holds the analytical artifacts for a single pair after the
// pipeline runs: the canonical hash for each side and the cosine
// similarity of the intent strings.
type Score struct {
	HashA, HashB   string
	ExactMatch     bool
	CosineSim      float32
}

// Metrics is the confusion matrix at one (tool, threshold) cut.
type Metrics struct {
	Tool       string
	Threshold  float64
	TP, FP, TN, FN int
}

// Precision is TP / (TP + FP). Zero when no positive predictions.
func (m Metrics) Precision() float64 {
	if m.TP+m.FP == 0 {
		return 0
	}
	return float64(m.TP) / float64(m.TP+m.FP)
}

// Recall is TP / (TP + FN). Zero when there are no real positives at all.
func (m Metrics) Recall() float64 {
	if m.TP+m.FN == 0 {
		return 0
	}
	return float64(m.TP) / float64(m.TP+m.FN)
}

// FalsePositiveRate is FP / (FP + TN). The fraction of distinct calls
// that would be wrongly blocked at this threshold.
func (m Metrics) FalsePositiveRate() float64 {
	if m.FP+m.TN == 0 {
		return 0
	}
	return float64(m.FP) / float64(m.FP+m.TN)
}

// F1 harmonic-means precision and recall.
func (m Metrics) F1() float64 {
	p, r := m.Precision(), m.Recall()
	if p+r == 0 {
		return 0
	}
	return 2 * p * r / (p + r)
}

// LoadDataset parses a jsonl file of Record. Lines that are blank or
// start with "#" are ignored so the file can carry inline comments.
func LoadDataset(path string) ([]Record, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied path
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	return readDataset(f)
}

func readDataset(r io.Reader) ([]Record, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var out []Record
	for line := 0; sc.Scan(); line++ {
		raw := sc.Bytes()
		if len(raw) == 0 || raw[0] == '#' {
			continue
		}
		var rec Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("line %d: %w", line+1, err)
		}
		if rec.Tool == "" || rec.IntentID == "" {
			return nil, fmt.Errorf("line %d: missing tool or intent_id", line+1)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// BuildPairs returns every unordered pair of records within the same
// tool. Same-tool pairs across different intent_ids are negative samples
// (SameIntent=false); pairs within the same intent_id are positive.
// Pairs across different tools are skipped because semantic_threshold is
// per-tool.
func BuildPairs(records []Record) []Pair {
	byTool := map[string][]int{}
	for i, r := range records {
		byTool[r.Tool] = append(byTool[r.Tool], i)
	}
	var pairs []Pair
	for _, idxs := range byTool {
		for i := 0; i < len(idxs); i++ {
			for j := i + 1; j < len(idxs); j++ {
				a, b := records[idxs[i]], records[idxs[j]]
				pairs = append(pairs, Pair{
					A:          a,
					B:          b,
					SameIntent: a.IntentID == b.IntentID,
					Category:   pairCategory(a, b),
				})
			}
		}
	}
	return pairs
}

func pairCategory(a, b Record) string {
	if a.Category == "" && b.Category == "" {
		if a.IntentID == b.IntentID {
			return "same-intent-uncategorized"
		}
		return "different-intent-uncategorized"
	}
	if a.Category == b.Category && a.Category != "" {
		return a.Category
	}
	if a.Category == "baseline" && b.Category != "" {
		return b.Category
	}
	if b.Category == "baseline" && a.Category != "" {
		return a.Category
	}
	return a.Category + "+" + b.Category
}

// Runner exercises the same code path the proxy uses for every pair.
// embedder is required; pol carries the per-tool normalize and
// fingerprint_fields config so the eval can't drift from production
// behavior.
type Runner struct {
	cfg      *policy.Config
	embedder embed.Embedder
}

// NewRunner constructs a Runner over the supplied policy and embedder.
func NewRunner(cfg *policy.Config, e embed.Embedder) *Runner {
	return &Runner{cfg: cfg, embedder: e}
}

// Score evaluates one pair through normalize + fingerprint + embed. The
// returned Score is independent of any threshold; thresholds are applied
// later in Evaluate.
func (r *Runner) Score(p Pair) (Score, error) {
	pol := r.cfg.For(p.A.Tool)
	hashA, intentA, err := analyse(p.A.Args, pol)
	if err != nil {
		return Score{}, fmt.Errorf("analyse A: %w", err)
	}
	hashB, intentB, err := analyse(p.B.Args, pol)
	if err != nil {
		return Score{}, fmt.Errorf("analyse B: %w", err)
	}
	var sim float32
	if intentA != "" && intentB != "" {
		va, err := r.embedder.Embed(intentA)
		if err != nil {
			return Score{}, fmt.Errorf("embed A: %w", err)
		}
		vb, err := r.embedder.Embed(intentB)
		if err != nil {
			return Score{}, fmt.Errorf("embed B: %w", err)
		}
		sim, err = embed.Cosine(va, vb)
		if err != nil {
			return Score{}, fmt.Errorf("cosine: %w", err)
		}
	}
	return Score{HashA: hashA, HashB: hashB, ExactMatch: hashA == hashB, CosineSim: sim}, nil
}

// Evaluate sweeps thresholds across [0.50, 0.99] in 0.01 steps and
// returns confusion-matrix Metrics at every cut. Each pair is treated
// as "dedup fires" when ExactMatch is true OR CosineSim >= threshold,
// which mirrors the production decide() logic.
func (r *Runner) Evaluate(pairs []Pair) ([]Metrics, error) {
	// Score every pair once.
	type scored struct {
		pair  Pair
		score Score
	}
	byTool := map[string][]scored{}
	for _, p := range pairs {
		s, err := r.Score(p)
		if err != nil {
			return nil, err
		}
		byTool[p.A.Tool] = append(byTool[p.A.Tool], scored{p, s})
	}
	// At each candidate threshold, compute the matrix per tool.
	thresholds := candidateThresholds()
	var all []Metrics
	for tool, items := range byTool {
		for _, th := range thresholds {
			var m Metrics
			m.Tool = tool
			m.Threshold = th
			for _, s := range items {
				fires := s.score.ExactMatch || float64(s.score.CosineSim) >= th
				switch {
				case s.pair.SameIntent && fires:
					m.TP++
				case s.pair.SameIntent && !fires:
					m.FN++
				case !s.pair.SameIntent && fires:
					m.FP++
				case !s.pair.SameIntent && !fires:
					m.TN++
				}
			}
			all = append(all, m)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Tool != all[j].Tool {
			return all[i].Tool < all[j].Tool
		}
		return all[i].Threshold < all[j].Threshold
	})
	return all, nil
}

// BestThreshold picks the threshold with the highest F1 score per tool.
// If multiple thresholds tie, the highest threshold wins (more
// conservative is safer for side-effecting tools).
func BestThreshold(metrics []Metrics) map[string]Metrics {
	best := map[string]Metrics{}
	for _, m := range metrics {
		cur, ok := best[m.Tool]
		switch {
		case !ok:
			best[m.Tool] = m
		case m.F1() > cur.F1():
			best[m.Tool] = m
		case m.F1() == cur.F1() && m.Threshold > cur.Threshold:
			best[m.Tool] = m
		}
	}
	return best
}

func candidateThresholds() []float64 {
	var out []float64
	for v := 50; v <= 99; v++ {
		out = append(out, float64(v)/100)
	}
	return out
}
