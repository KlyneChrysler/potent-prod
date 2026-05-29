// Command potent-eval is the offline thesis-validation harness. It reads
// a labelled jsonl dataset of tool calls, runs each pair through the same
// normalizer + fingerprint + embedder pipeline that the proxy uses at
// request time, and reports precision/recall at every candidate
// semantic_threshold so operators (and CI) can tune the policy from
// data instead of guess. The product's claim ("semantic intent dedup
// catches real LLM retries reliably and safely") is unfalsifiable
// without this measurement; here is the falsifier.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/potent/potent/internal/embed"
	"github.com/potent/potent/internal/eval"
	"github.com/potent/potent/internal/policy"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "potent-eval: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	datasetPath := flag.String("dataset", "docs/eval/dataset.jsonl", "labelled jsonl dataset")
	policyPath := flag.String("policy", "docs/eval/policy.yaml", "policy yaml the eval should mirror")
	embedDim := flag.Int("embed-dim", 384, "embedder dimension")
	embedN := flag.Int("embed-ngram", 4, "char n-gram size")
	jsonOut := flag.Bool("json", false, "emit a single json document instead of human tables")
	verbose := flag.Bool("v", false, "include the full threshold sweep (slow for large datasets)")
	fromAudit := flag.String("from-audit", "", "summarize a potent audit-log jsonl file instead of running the threshold sweep (use with -shadow-mode captures)")
	flag.Parse()

	if *fromAudit != "" {
		return summarizeAudit(*fromAudit, *jsonOut)
	}

	records, err := eval.LoadDataset(*datasetPath)
	if err != nil {
		return fmt.Errorf("load dataset: %w", err)
	}
	if len(records) == 0 {
		return fmt.Errorf("dataset %q is empty", *datasetPath)
	}

	cfg, err := policy.Load(*policyPath)
	if err != nil {
		return fmt.Errorf("load policy: %w", err)
	}

	emb, err := embed.NewHashingTFIDF(*embedDim, *embedN)
	if err != nil {
		return fmt.Errorf("embedder: %w", err)
	}

	runner := eval.NewRunner(cfg, emb)
	pairs := eval.BuildPairs(records)
	if len(pairs) == 0 {
		return fmt.Errorf("dataset has no same-tool pairs to evaluate")
	}
	metrics, err := runner.Evaluate(pairs)
	if err != nil {
		return fmt.Errorf("evaluate: %w", err)
	}
	best := eval.BestThreshold(metrics)

	if *jsonOut {
		return emitJSON(records, pairs, metrics, best, *verbose)
	}
	return emitHuman(records, pairs, metrics, best, *verbose)
}

type jsonReport struct {
	Records int                       `json:"records"`
	Pairs   int                       `json:"pairs"`
	Best    map[string]thresholdEntry `json:"best_threshold_per_tool"`
	Sweep   []eval.Metrics            `json:"sweep,omitempty"`
}

type thresholdEntry struct {
	Threshold float64 `json:"threshold"`
	Precision float64 `json:"precision"`
	Recall    float64 `json:"recall"`
	FPR       float64 `json:"fpr"`
	F1        float64 `json:"f1"`
	TP        int     `json:"tp"`
	FP        int     `json:"fp"`
	TN        int     `json:"tn"`
	FN        int     `json:"fn"`
}

func emitJSON(records []eval.Record, pairs []eval.Pair, metrics []eval.Metrics, best map[string]eval.Metrics, verbose bool) error {
	out := jsonReport{Records: len(records), Pairs: len(pairs), Best: map[string]thresholdEntry{}}
	for tool, m := range best {
		out.Best[tool] = thresholdEntry{
			Threshold: m.Threshold, Precision: m.Precision(), Recall: m.Recall(),
			FPR: m.FalsePositiveRate(), F1: m.F1(),
			TP: m.TP, FP: m.FP, TN: m.TN, FN: m.FN,
		}
	}
	if verbose {
		out.Sweep = metrics
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func emitHuman(records []eval.Record, pairs []eval.Pair, metrics []eval.Metrics, best map[string]eval.Metrics, verbose bool) error {
	fmt.Printf("dataset: %d records, %d same-tool pairs\n\n", len(records), len(pairs))

	// Best threshold per tool.
	fmt.Println("best F1 per tool")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "tool\tthreshold\tprecision\trecall\tFPR\tF1\tTP/FP/TN/FN")
	tools := sortedToolNames(best)
	for _, tool := range tools {
		m := best[tool]
		fmt.Fprintf(tw, "%s\t%.2f\t%.3f\t%.3f\t%.3f\t%.3f\t%d/%d/%d/%d\n",
			tool, m.Threshold, m.Precision(), m.Recall(), m.FalsePositiveRate(), m.F1(),
			m.TP, m.FP, m.TN, m.FN)
	}
	tw.Flush()
	fmt.Println()

	// Coarse PR curve every 0.05 step.
	fmt.Println("precision/recall curve (every 0.05 step)")
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "tool\tthreshold\tprecision\trecall\tFPR\tF1")
	for _, tool := range tools {
		for _, m := range metrics {
			if m.Tool != tool {
				continue
			}
			rounded := int(m.Threshold*100 + 0.5)
			if rounded%5 != 0 {
				continue
			}
			fmt.Fprintf(tw, "%s\t%.2f\t%.3f\t%.3f\t%.3f\t%.3f\n",
				tool, m.Threshold, m.Precision(), m.Recall(), m.FalsePositiveRate(), m.F1())
		}
		fmt.Fprintln(tw)
	}
	tw.Flush()

	if verbose {
		fmt.Println("\nfull sweep")
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "tool\tthreshold\tprecision\trecall\tFPR\tF1\tTP/FP/TN/FN")
		for _, m := range metrics {
			fmt.Fprintf(tw, "%s\t%.2f\t%.3f\t%.3f\t%.3f\t%.3f\t%d/%d/%d/%d\n",
				m.Tool, m.Threshold, m.Precision(), m.Recall(), m.FalsePositiveRate(), m.F1(),
				m.TP, m.FP, m.TN, m.FN)
		}
		tw.Flush()
	}
	return nil
}

// summarizeAudit reads a shadow-mode audit log and renders the per-tool
// would-decision distribution and the top repeated fingerprint. Operators
// run potent with -shadow-mode -audit-log /path/to/audit.log for a
// representative window, then feed the file here.
func summarizeAudit(path string, asJSON bool) error {
	records, err := eval.LoadAuditLog(path)
	if err != nil {
		return fmt.Errorf("load audit log: %w", err)
	}
	if len(records) == 0 {
		return fmt.Errorf("audit log %q is empty", path)
	}
	summary := eval.Summarize(records)
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}
	fmt.Printf("audit log: %d records, %d tools\n\n", summary.Records, len(summary.Tools))
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "tool\trecords\twould_replay_rate\tunique_hashes\ttop_hash_count\tdecisions")
	for _, tool := range summary.SortedTools() {
		tr := summary.Tools[tool]
		decKeys := make([]string, 0, len(tr.WouldDecisions))
		for k := range tr.WouldDecisions {
			decKeys = append(decKeys, k)
		}
		sort.Strings(decKeys)
		parts := make([]string, 0, len(decKeys))
		for _, k := range decKeys {
			parts = append(parts, fmt.Sprintf("%s=%d", k, tr.WouldDecisions[k]))
		}
		fmt.Fprintf(tw, "%s\t%d\t%.3f\t%d\t%d\t%s\n",
			tool, tr.Records, tr.WouldReplayRate, tr.UniqueHashes, tr.TopRepeatedCount,
			joinKV(parts))
	}
	tw.Flush()
	return nil
}

func joinKV(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func sortedToolNames(m map[string]eval.Metrics) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
