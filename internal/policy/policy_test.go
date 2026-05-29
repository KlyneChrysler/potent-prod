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

func TestToolPolicy_AllowsCaller(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		caller  string
		want    bool
	}{
		{"empty list permits any caller", nil, "team-eng", true},
		{"empty list permits anonymous", nil, "", true},
		{"explicit allow matches", []string{"team-eng"}, "team-eng", true},
		{"explicit allow rejects others", []string{"team-eng"}, "team-marketing", false},
		{"empty caller rejected by non-empty list", []string{"team-eng"}, "", false},
		{"multiple entries one match", []string{"team-eng", "team-finance"}, "team-finance", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ToolPolicy{AllowedCallers: c.allowed}
			if got := p.AllowsCaller(c.caller); got != c.want {
				t.Errorf("AllowsCaller(%q) with %v = %v, want %v", c.caller, c.allowed, got, c.want)
			}
		})
	}
}

func TestToolPolicy_SynthesizeReplay(t *testing.T) {
	cases := []struct {
		name string
		tmpl string
		tool string
		hash string
		want string
	}{
		{"empty template uses default", "", "send_email", "abc123", `{"replayed":true,"hash":"abc123"}`},
		{"hash expansion", `{"id":"$hash"}`, "send_email", "deadbeef", `{"id":"deadbeef"}`},
		{"tool expansion", `{"tool":"$tool","hash":"$hash"}`, "charge_card", "h1", `{"tool":"charge_card","hash":"h1"}`},
		{"no expansion tokens", `{"static":true}`, "x", "y", `{"static":true}`},
		{"lone dollar sign untouched", `{"price":"$10"}`, "x", "y", `{"price":"$10"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := ToolPolicy{SynthesizedResponse: c.tmpl}
			got := string(p.SynthesizeReplay(c.tool, c.hash))
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
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
