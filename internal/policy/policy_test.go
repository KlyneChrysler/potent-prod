package policy

import (
	"testing"
	"time"
)

func TestFor_UsesDefaultsWhenToolMissing(t *testing.T) {
	c := &Config{
		Defaults: ToolPolicy{Mode: ModeLogOnly, TTL: time.Hour, SemanticThreshold: 0.9},
	}
	got := c.For("unknown_tool")
	if got.Mode != ModeLogOnly {
		t.Errorf("expected default mode, got %v", got.Mode)
	}
	if got.TTL != time.Hour {
		t.Errorf("expected default TTL, got %v", got.TTL)
	}
}

func TestFor_PreservesToolSpecificOverrides(t *testing.T) {
	c := &Config{
		Defaults: ToolPolicy{Mode: ModeLogOnly, TTL: time.Hour, SemanticThreshold: 0.9},
		Tools: map[string]ToolPolicy{
			"send_email": {Mode: ModeStrict, TTL: 24 * time.Hour, SemanticThreshold: 0.94},
		},
	}
	got := c.For("send_email")
	if got.Mode != ModeStrict {
		t.Errorf("expected strict, got %v", got.Mode)
	}
	if got.TTL != 24*time.Hour {
		t.Errorf("expected 24h, got %v", got.TTL)
	}
	if got.SemanticThreshold != 0.94 {
		t.Errorf("expected 0.94, got %v", got.SemanticThreshold)
	}
}

func TestFor_MergesDefaultsForUnsetFields(t *testing.T) {
	c := &Config{
		Defaults: ToolPolicy{Mode: ModeLogOnly, TTL: time.Hour, SemanticThreshold: 0.9},
		Tools: map[string]ToolPolicy{
			"partial": {Mode: ModeStrict}, // TTL + threshold unset
		},
	}
	got := c.For("partial")
	if got.Mode != ModeStrict {
		t.Errorf("expected strict, got %v", got.Mode)
	}
	if got.TTL != time.Hour {
		t.Errorf("expected default TTL fallback, got %v", got.TTL)
	}
	if got.SemanticThreshold != 0.9 {
		t.Errorf("expected default threshold fallback, got %v", got.SemanticThreshold)
	}
}

func TestModes_StringValues(t *testing.T) {
	tests := []struct {
		mode Mode
		want string
	}{
		{ModeStrict, "strict"},
		{ModeCache, "cache"},
		{ModeLogOnly, "log_only"},
		{ModeOff, "off"},
	}
	for _, tt := range tests {
		if string(tt.mode) != tt.want {
			t.Errorf("Mode %v = %q, want %q", tt.mode, string(tt.mode), tt.want)
		}
	}
}
