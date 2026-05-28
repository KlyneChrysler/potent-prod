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
