package policy

import "time"

type Mode string

const (
	ModeStrict  Mode = "strict"
	ModeCache   Mode = "cache"
	ModeLogOnly Mode = "log_only"
	ModeOff     Mode = "off"
)

// ReplayStrategy decides how a duplicate tool call is satisfied.
type ReplayStrategy string

const (
	// ReplayCachedResponse stores the upstream's response body and replays
	// it byte-for-byte on the next duplicate. Default behavior; ideal when
	// the response is non-sensitive and the caller depends on its contents
	// (e.g. a search query that returns the first hit verbatim).
	ReplayCachedResponse ReplayStrategy = "cached_response"

	// ReplaySynthesizedAck never stores the upstream response body. On
	// replay, potent returns a generated acknowledgement (configurable via
	// SynthesizedResponse). Use this for side-effecting tools whose response
	// contains PII or other sensitive material (charge IDs, message IDs,
	// PHI) that should not sit in the cache. The caller learns that the
	// action was already done but not the original upstream return value.
	ReplaySynthesizedAck ReplayStrategy = "synthesized_ack"
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

	// ReplayStrategy controls how a duplicate tool call is satisfied. The
	// zero value is ReplayCachedResponse for backward compatibility.
	ReplayStrategy ReplayStrategy `yaml:"replay_strategy"`

	// SynthesizedResponse is the JSON body returned on a replay when
	// ReplayStrategy is ReplaySynthesizedAck. Two tokens are expanded at
	// replay time: "$hash" becomes the request fingerprint and "$tool"
	// becomes the tool name. When empty, the default '{"replayed":true,
	// "hash":"$hash"}' is used.
	SynthesizedResponse string `yaml:"synthesized_response"`

	// RedactRequestBody, when true, causes the cache to store nil instead
	// of the raw request bytes. The fingerprint (already a one-way hash)
	// is still recorded, so the cache works normally; only the bytes the
	// admin debug view exposes are dropped. Useful for tools whose
	// arguments include PII (customer SSN, etc.).
	RedactRequestBody bool `yaml:"redact_request_body"`
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

// SynthesizeReplay renders the SynthesizedResponse template for a
// duplicate call. Tokens "$hash" and "$tool" expand to their respective
// values. When the template is empty, a minimal default is produced. The
// caller is expected to use this only when ReplayStrategy is
// ReplaySynthesizedAck.
func (p ToolPolicy) SynthesizeReplay(tool, hash string) []byte {
	tmpl := p.SynthesizedResponse
	if tmpl == "" {
		tmpl = `{"replayed":true,"hash":"$hash"}`
	}
	out := make([]byte, 0, len(tmpl)+len(hash)+len(tool))
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] == '$' && i+5 <= len(tmpl) && tmpl[i:i+5] == "$hash" {
			out = append(out, hash...)
			i += 4
			continue
		}
		if tmpl[i] == '$' && i+5 <= len(tmpl) && tmpl[i:i+5] == "$tool" {
			out = append(out, tool...)
			i += 4
			continue
		}
		out = append(out, tmpl[i])
	}
	return out
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
