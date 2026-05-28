package normalizer

import "testing"

func TestApply(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		transforms []string
		want       string
	}{
		{"empty transforms returns input", "Hello", nil, "Hello"},
		{"trim removes surrounding whitespace", "  hi  ", []string{"trim"}, "hi"},
		{"lowercase converts to lower", "ABC", []string{"lowercase"}, "abc"},
		{"uppercase converts to upper", "abc", []string{"uppercase"}, "ABC"},
		{"collapse_whitespace collapses runs", "a   b\t\tc", []string{"collapse_whitespace"}, "a b c"},
		{"transforms apply in order", "  Hello World  ", []string{"trim", "lowercase"}, "hello world"},
		{"unknown transform is ignored", "abc", []string{"does_not_exist"}, "abc"},
		{"collapse trims edges too", "   a    b   ", []string{"collapse_whitespace"}, "a b"},
		{"unicode whitespace handled", "a  b", []string{"collapse_whitespace"}, "a b"},
		{"empty string passes through", "", []string{"trim", "lowercase"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Apply(tt.value, tt.transforms)
			if got != tt.want {
				t.Errorf("Apply(%q, %v) = %q, want %q", tt.value, tt.transforms, got, tt.want)
			}
		})
	}
}

func TestApply_TransformOrderMatters(t *testing.T) {
	// uppercase then lowercase != lowercase then uppercase result, but both end at lowercase
	got1 := Apply("Hello", []string{"uppercase", "lowercase"})
	got2 := Apply("Hello", []string{"lowercase", "uppercase"})
	if got1 != "hello" {
		t.Errorf("upper->lower got %q", got1)
	}
	if got2 != "HELLO" {
		t.Errorf("lower->upper got %q", got2)
	}
}
