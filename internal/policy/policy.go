package policy

import "time"

type Mode string

const (
	ModeStrict  Mode = "strict"
	ModeCache   Mode = "cache"
	ModeLogOnly Mode = "log_only"
	ModeOff     Mode = "off"
)

type ToolPolicy struct {
	Mode                        Mode                `yaml:"mode"`
	TTL                         time.Duration       `yaml:"ttl"`
	Normalize                   map[string][]string `yaml:"normalize"`
	FingerprintFields           []string            `yaml:"fingerprint_fields"`
	SemanticThreshold           float64             `yaml:"semantic_threshold"`
	RequireHumanConfirmOnReplay bool                `yaml:"require_human_confirm_on_replay"`

	// RateLimit caps the rate at which tool calls reach the upstream from
	// this potent instance. The check fires before the cache lookup so a
	// buggy agent in a retry loop cannot saturate the gateway even with
	// requests that would have been deduped. Zero (RPS=0) disables.
	RateLimit RateLimit `yaml:"rate_limit"`

	// AllowedCallers, when non-empty, restricts which authenticated callers
	// may invoke this tool. The caller-id (set by auth middleware via the
	// tokens file) must appear in this list or the request is rejected
	// with 403. An empty list (the default) means any authenticated caller
	// may invoke the tool, which matches single-token deployments.
	AllowedCallers []string `yaml:"allowed_callers"`
}

// AllowsCaller reports whether the named caller may invoke this tool. An
// empty AllowedCallers list means every authenticated caller is permitted
// (the simple, single-tenant default).
func (p ToolPolicy) AllowsCaller(caller string) bool {
	if len(p.AllowedCallers) == 0 {
		return true
	}
	for _, c := range p.AllowedCallers {
		if c == caller {
			return true
		}
	}
	return false
}

// RateLimit is the token-bucket configuration for a single tool. RPS is
// the steady-state rate; Burst is the maximum allowance for a brief spike
// (defaults to RPS rounded up when unset).
type RateLimit struct {
	RPS   float64 `yaml:"rps"`
	Burst int     `yaml:"burst"`
}

type Config struct {
	Defaults ToolPolicy            `yaml:"defaults"`
	Tools    map[string]ToolPolicy `yaml:"tools"`
}

func (c *Config) For(tool string) ToolPolicy {
	if p, ok := c.Tools[tool]; ok {
		return p.mergeDefaults(c.Defaults)
	}
	return c.Defaults
}

func (p ToolPolicy) mergeDefaults(d ToolPolicy) ToolPolicy {
	if p.Mode == "" {
		p.Mode = d.Mode
	}
	if p.TTL == 0 {
		p.TTL = d.TTL
	}
	if p.SemanticThreshold == 0 {
		p.SemanticThreshold = d.SemanticThreshold
	}
	return p
}
