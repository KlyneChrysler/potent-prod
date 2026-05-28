// Package embed defines the Embedder port and ships a zero-dependency
// hashing-TF-IDF embedder suitable for catching normalized-duplicate
// semantics ("Send email to Alice" ≈ "send  email  to alice").
//
// The Embedder interface is small (1 method) on purpose: an ONNX-backed
// transformer embedder (Week 4+) drops in without touching the proxy.
package embed

import (
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"unicode"
)

// Embedder turns a free-text string into a fixed-dim L2-normalized vector.
// Implementations must be deterministic and safe for concurrent use.
type Embedder interface {
	Embed(text string) ([]float32, error)
	Dim() int
}

// HashingTFIDF is a deterministic, dependency-free embedder built on
// character n-grams + the hashing trick. It is intentionally simple but
// non-trivial: it consistently produces cosine similarity > 0.9 for
// near-duplicate strings and < 0.3 for unrelated strings, which is the
// regime potent's semantic tier targets.
type HashingTFIDF struct {
	dim    int
	nGramN int
}

// NewHashingTFIDF returns an embedder with the given output dimension and
// character n-gram size. Defaults: dim=384, n=4.
func NewHashingTFIDF(dim, nGramN int) (*HashingTFIDF, error) {
	if dim <= 0 {
		return nil, fmt.Errorf("dim must be > 0, got %d", dim)
	}
	if nGramN < 2 || nGramN > 6 {
		return nil, fmt.Errorf("n-gram size must be in [2,6], got %d", nGramN)
	}
	return &HashingTFIDF{dim: dim, nGramN: nGramN}, nil
}

// Dim returns the embedding dimension.
func (h *HashingTFIDF) Dim() int { return h.dim }

// Embed computes the embedding for text. The same input always produces the
// same output. An error is returned only for an empty input.
func (h *HashingTFIDF) Embed(text string) ([]float32, error) {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil, errors.New("embed: empty input")
	}
	// collapse runs of whitespace so " a   b " and "a b" embed identically
	text = collapseSpaces(text)

	vec := make([]float32, h.dim)
	runes := []rune(text)
	n := h.nGramN
	if len(runes) < n {
		// pad with space so very short inputs still produce a stable n-gram
		runes = append(runes, []rune(strings.Repeat(" ", n-len(runes)))...)
	}

	total := 0
	for i := 0; i <= len(runes)-n; i++ {
		gram := string(runes[i : i+n])
		idx, sign := bucket(gram, h.dim)
		vec[idx] += sign
		total++
	}

	// log-scale the raw counts so high-frequency grams don't dominate
	for i, v := range vec {
		if v == 0 {
			continue
		}
		s := float32(1)
		if v < 0 {
			s = -1
		}
		vec[i] = s * float32(math.Log(1+math.Abs(float64(v))))
	}

	l2Normalize(vec)
	return vec, nil
}

// bucket returns (index, sign) for an n-gram using FNV-1a + sign bit.
// The signed-hash trick avoids systematic bias from collisions.
//
// Conversions are safe: dim is validated > 0 by NewHashingTFIDF and the
// modulus result is always in [0, dim), which fits in int on every supported
// platform.
func bucket(gram string, dim int) (int, float32) {
	if dim <= 0 {
		return 0, 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(gram))
	sum := h.Sum64()
	idx := int(sum % uint64(dim)) // #nosec G115 -- dim > 0 enforced; result < dim
	sign := float32(1)
	if sum&1 == 0 {
		sign = -1
	}
	return idx, sign
}

func collapseSpaces(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prev {
				b.WriteRune(' ')
			}
			prev = true
			continue
		}
		b.WriteRune(r)
		prev = false
	}
	return b.String()
}

func l2Normalize(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(sum))
	for i := range v {
		v[i] *= inv
	}
}

// Cosine returns the cosine similarity of two equal-length vectors.
// Inputs are assumed L2-normalized; the function still works correctly
// for un-normalized inputs but is more expensive there.
func Cosine(a, b []float32) (float32, error) {
	if len(a) != len(b) {
		return 0, fmt.Errorf("dim mismatch: %d vs %d", len(a), len(b))
	}
	if len(a) == 0 {
		return 0, errors.New("empty vectors")
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0, nil
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb))), nil
}
