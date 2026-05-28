package embed

import (
	"math"
	"testing"
)

func mustEmbed(t *testing.T, e Embedder, text string) []float32 {
	t.Helper()
	v, err := e.Embed(text)
	if err != nil {
		t.Fatalf("Embed(%q): %v", text, err)
	}
	return v
}

func TestHashingTFIDF_Deterministic(t *testing.T) {
	e, _ := NewHashingTFIDF(384, 4)
	v1 := mustEmbed(t, e, "send email to alice")
	v2 := mustEmbed(t, e, "send email to alice")
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("embedding not deterministic at index %d", i)
		}
	}
}

func TestHashingTFIDF_NormalizedDuplicatesAreClose(t *testing.T) {
	e, _ := NewHashingTFIDF(384, 4)
	cases := [][2]string{
		{"send email to alice@example.com", "Send  Email to ALICE@Example.com"},
		{"hello world", "  hello   world  "},
		{"q3 report attached", "q3 REPORT attached"},
	}
	for _, c := range cases {
		a := mustEmbed(t, e, c[0])
		b := mustEmbed(t, e, c[1])
		sim, err := Cosine(a, b)
		if err != nil {
			t.Fatalf("cosine: %v", err)
		}
		if sim < 0.95 {
			t.Errorf("expected near-duplicate cosine ≥ 0.95 for %q vs %q, got %v", c[0], c[1], sim)
		}
	}
}

func TestHashingTFIDF_UnrelatedStringsAreFar(t *testing.T) {
	e, _ := NewHashingTFIDF(384, 4)
	cases := [][2]string{
		{"send email to alice", "delete user account 42"},
		{"hello world", "transfer money to bob"},
		{"q3 financial report", "buy groceries milk eggs"},
	}
	for _, c := range cases {
		a := mustEmbed(t, e, c[0])
		b := mustEmbed(t, e, c[1])
		sim, _ := Cosine(a, b)
		if sim > 0.5 {
			t.Errorf("expected unrelated cosine ≤ 0.5 for %q vs %q, got %v", c[0], c[1], sim)
		}
	}
}

func TestHashingTFIDF_IsL2Normalized(t *testing.T) {
	e, _ := NewHashingTFIDF(384, 4)
	v := mustEmbed(t, e, "send email to alice")
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	norm := math.Sqrt(sum)
	if math.Abs(norm-1.0) > 1e-5 {
		t.Errorf("expected L2 norm ≈ 1, got %v", norm)
	}
}

func TestHashingTFIDF_DimMatches(t *testing.T) {
	e, _ := NewHashingTFIDF(256, 3)
	v, _ := e.Embed("hello")
	if len(v) != 256 {
		t.Errorf("expected dim 256, got %d", len(v))
	}
	if e.Dim() != 256 {
		t.Errorf("Dim() = %d, want 256", e.Dim())
	}
}

func TestHashingTFIDF_EmptyInputErrors(t *testing.T) {
	e, _ := NewHashingTFIDF(384, 4)
	if _, err := e.Embed(""); err == nil {
		t.Errorf("expected error for empty input")
	}
	if _, err := e.Embed("   "); err == nil {
		t.Errorf("expected error for whitespace-only input")
	}
}

func TestNewHashingTFIDF_RejectsBadParams(t *testing.T) {
	if _, err := NewHashingTFIDF(0, 4); err == nil {
		t.Errorf("expected error for dim=0")
	}
	if _, err := NewHashingTFIDF(384, 1); err == nil {
		t.Errorf("expected error for n=1")
	}
	if _, err := NewHashingTFIDF(384, 7); err == nil {
		t.Errorf("expected error for n=7")
	}
}

func TestCosine_DimMismatch(t *testing.T) {
	a := []float32{1, 0}
	b := []float32{1, 0, 0}
	if _, err := Cosine(a, b); err == nil {
		t.Errorf("expected dim mismatch error")
	}
}

func TestCosine_IdenticalIsOne(t *testing.T) {
	a := []float32{1, 0, 0}
	sim, _ := Cosine(a, a)
	if math.Abs(float64(sim)-1.0) > 1e-6 {
		t.Errorf("identical cosine = %v, want 1", sim)
	}
}

func TestCosine_OrthogonalIsZero(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	sim, _ := Cosine(a, b)
	if math.Abs(float64(sim)) > 1e-6 {
		t.Errorf("orthogonal cosine = %v, want 0", sim)
	}
}

func TestEmbed_ShortInputDoesNotPanic(t *testing.T) {
	e, _ := NewHashingTFIDF(384, 4)
	v, err := e.Embed("hi")
	if err != nil {
		t.Fatalf("short input: %v", err)
	}
	if len(v) != 384 {
		t.Errorf("dim = %d, want 384", len(v))
	}
}
