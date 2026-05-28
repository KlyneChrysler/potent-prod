package fingerprint

import "testing"

func TestExact_DeterministicSameInput(t *testing.T) {
	v := map[string]any{"to": "a@b.com", "subject": "Hi"}
	h1, err := Exact(v)
	if err != nil {
		t.Fatalf("Exact returned err: %v", err)
	}
	h2, err := Exact(v)
	if err != nil {
		t.Fatalf("Exact returned err: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hash not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex sha256, got len %d", len(h1))
	}
}

func TestExact_KeyOrderInsensitive(t *testing.T) {
	a := map[string]any{"to": "x", "subject": "y", "body": "z"}
	b := map[string]any{"body": "z", "to": "x", "subject": "y"}
	ha, _ := Exact(a)
	hb, _ := Exact(b)
	if ha != hb {
		t.Errorf("key order changed hash: %s vs %s", ha, hb)
	}
}

func TestExact_NestedKeyOrderInsensitive(t *testing.T) {
	a := map[string]any{"outer": map[string]any{"k1": 1, "k2": 2}}
	b := map[string]any{"outer": map[string]any{"k2": 2, "k1": 1}}
	ha, _ := Exact(a)
	hb, _ := Exact(b)
	if ha != hb {
		t.Errorf("nested key order changed hash: %s vs %s", ha, hb)
	}
}

func TestExact_ArrayOrderSensitive(t *testing.T) {
	a := []any{"x", "y", "z"}
	b := []any{"z", "y", "x"}
	ha, _ := Exact(a)
	hb, _ := Exact(b)
	if ha == hb {
		t.Errorf("array order should change hash but didn't")
	}
}

func TestExact_DifferentValuesDifferentHashes(t *testing.T) {
	a := map[string]any{"amount": 100}
	b := map[string]any{"amount": 101}
	ha, _ := Exact(a)
	hb, _ := Exact(b)
	if ha == hb {
		t.Errorf("different values produced same hash")
	}
}

func TestExact_StringVsNumber(t *testing.T) {
	a := map[string]any{"x": "1"}
	b := map[string]any{"x": 1}
	ha, _ := Exact(a)
	hb, _ := Exact(b)
	if ha == hb {
		t.Errorf("string vs number must differ")
	}
}

func TestExact_NilAndEmpty(t *testing.T) {
	hNil, err := Exact(nil)
	if err != nil {
		t.Fatalf("nil err: %v", err)
	}
	hEmpty, err := Exact(map[string]any{})
	if err != nil {
		t.Fatalf("empty err: %v", err)
	}
	if hNil == hEmpty {
		t.Errorf("nil and empty map should differ")
	}
}

func TestExact_UnicodeValues(t *testing.T) {
	v := map[string]any{"name": "café ☕"}
	h, err := Exact(v)
	if err != nil {
		t.Fatalf("unicode err: %v", err)
	}
	if len(h) != 64 {
		t.Errorf("expected 64-char hash, got %d", len(h))
	}
}
