package policy

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_ExampleConfig(t *testing.T) {
	yaml := `
defaults:
  mode: log_only
  ttl: 1h
  semantic_threshold: 0.9

tools:
  send_email:
    mode: strict
    ttl: 24h
    normalize:
      to: [lowercase, trim]
      subject: [trim, collapse_whitespace]
    fingerprint_fields: [to, subject, body]
    semantic_threshold: 0.94

  delete_user:
    mode: strict
    ttl: 168h
    require_human_confirm_on_replay: true
    fingerprint_fields: [user_id]

  log_event:
    mode: off
`
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write tmp: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Defaults.Mode != ModeLogOnly {
		t.Errorf("default mode = %v", cfg.Defaults.Mode)
	}
	if cfg.Defaults.TTL != time.Hour {
		t.Errorf("default ttl = %v", cfg.Defaults.TTL)
	}

	send := cfg.For("send_email")
	if send.Mode != ModeStrict {
		t.Errorf("send_email mode = %v", send.Mode)
	}
	if send.TTL != 24*time.Hour {
		t.Errorf("send_email ttl = %v", send.TTL)
	}
	if got := send.Normalize["to"]; len(got) != 2 || got[0] != "lowercase" || got[1] != "trim" {
		t.Errorf("send_email normalize[to] = %v", got)
	}
	if got := send.FingerprintFields; len(got) != 3 {
		t.Errorf("send_email fingerprint_fields = %v", got)
	}

	del := cfg.For("delete_user")
	if !del.RequireHumanConfirmOnReplay {
		t.Errorf("delete_user should require human confirm")
	}

	if cfg.For("log_event").Mode != ModeOff {
		t.Errorf("log_event mode = %v", cfg.For("log_event").Mode)
	}
}

func TestLoad_ParsesRateLimitAndAllowedCallers(t *testing.T) {
	yaml := `
tools:
  send_email:
    mode: strict
    ttl: 24h
    rate_limit:
      rps: 10
      burst: 5
    allowed_callers: [team-eng, team-finance]
`
	dir := t.TempDir()
	path := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	send := cfg.For("send_email")
	if send.RateLimit.RPS != 10 || send.RateLimit.Burst != 5 {
		t.Errorf("rate_limit = %+v, want {10, 5}", send.RateLimit)
	}
	if len(send.AllowedCallers) != 2 || send.AllowedCallers[0] != "team-eng" {
		t.Errorf("allowed_callers = %v", send.AllowedCallers)
	}
}

func TestParseDuration_Extensions(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
	}{
		{"1h", time.Hour},
		{"30m", 30 * time.Minute},
		{"7d", 7 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
	}
	for _, tt := range tests {
		got, err := parseDuration(tt.in)
		if err != nil {
			t.Errorf("parseDuration(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestLoad_FileMissing(t *testing.T) {
	if _, err := Load("/no/such/file"); err == nil {
		t.Errorf("expected error for missing file")
	}
}

func TestValidate_RejectsUnknownMode(t *testing.T) {
	c := &Config{Tools: map[string]ToolPolicy{
		"bad": {Mode: Mode("nonsense")},
	}}
	if err := c.Validate(); err == nil {
		t.Errorf("expected unknown-mode error")
	}
}

func TestValidate_RejectsThresholdOutOfRange(t *testing.T) {
	c := &Config{Tools: map[string]ToolPolicy{
		"bad": {Mode: ModeStrict, SemanticThreshold: 1.5},
	}}
	if err := c.Validate(); err == nil {
		t.Errorf("expected threshold-range error")
	}
}
