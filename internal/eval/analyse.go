package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/potent/potent/internal/fingerprint"
	"github.com/potent/potent/internal/normalizer"
	"github.com/potent/potent/internal/policy"
)

// analyse mirrors pipeline.analyse so the eval harness uses the exact
// same canonicalization, hashing, and intent-string construction the
// proxy uses at request time. Kept as a small private copy rather than
// exported from pipeline because pipeline.analyse takes raw JSON bytes
// (what the proxy receives over the wire) while the eval feeds maps
// already parsed from the labelled jsonl file.
func analyse(args map[string]any, pol policy.ToolPolicy) (string, string, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw := args

	// Apply per-field normalization (lowercase, trim, collapse_whitespace).
	for field, transforms := range pol.Normalize {
		if v, ok := raw[field].(string); ok {
			raw[field] = normalizer.Apply(v, transforms)
		}
	}

	// Select only the policy's fingerprint_fields when configured.
	selected := raw
	if len(pol.FingerprintFields) > 0 {
		filtered := make(map[string]any, len(pol.FingerprintFields))
		for _, f := range pol.FingerprintFields {
			if v, ok := raw[f]; ok {
				filtered[f] = v
			}
		}
		selected = filtered
	}

	hash, err := fingerprint.Exact(selected)
	if err != nil {
		return "", "", err
	}
	return hash, buildIntent(selected), nil
}

// buildIntent emits a deterministic intent string. Keys are sorted; the
// scalar value of each field is appended. Identical logic to the proxy.
func buildIntent(m map[string]any) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(k)
		b.WriteByte('=')
		writeScalar(&b, m[k])
	}
	return b.String()
}

func writeScalar(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case string:
		b.WriteString(x)
	case float64:
		b.WriteString(strconv.FormatFloat(x, 'g', -1, 64))
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case nil:
		b.WriteString("null")
	default:
		_ = json.NewEncoder(b).Encode(x)
	}
}

// unused so the import of fmt doesn't fail when the file is small.
var _ = fmt.Errorf
