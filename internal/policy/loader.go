package policy

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Load reads a policy YAML file from disk and returns a validated Config.
//
// The loader intentionally implements a tiny YAML subset (top-level keys,
// nested maps, scalar values, simple inline lists like [trim, lowercase])
// rather than pulling in a YAML dependency for Week 1. The official YAML
// parser will replace this once external deps are introduced.
func Load(path string) (*Config, error) {
	// path is supplied by the operator via -policy flag, not by untrusted input.
	b, err := os.ReadFile(path) // #nosec G304
	if err != nil {
		return nil, fmt.Errorf("read policy %q: %w", path, err)
	}
	cfg, err := parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("parse policy %q: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate policy %q: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the config for obviously broken values.
func (c *Config) Validate() error {
	if c == nil {
		return errors.New("policy config is nil")
	}
	for name, p := range c.Tools {
		switch p.Mode {
		case "", ModeStrict, ModeCache, ModeLogOnly, ModeOff:
		default:
			return fmt.Errorf("tool %q: unknown mode %q", name, p.Mode)
		}
		if p.SemanticThreshold < 0 || p.SemanticThreshold > 1 {
			return fmt.Errorf("tool %q: semantic_threshold must be in [0,1], got %f", name, p.SemanticThreshold)
		}
	}
	return nil
}

// parse implements the minimal YAML subset described on Load.
func parse(src string) (*Config, error) {
	lines := strings.Split(src, "\n")
	cfg := &Config{Tools: map[string]ToolPolicy{}}

	var stack []stackFrame
	currentTool := ""

	for i, raw := range lines {
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := leadingSpaces(line)
		trim := strings.TrimSpace(line)

		// pop stack while indent <= top frame indent
		for len(stack) > 0 && indent <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}

		switch {
		case indent == 0 && trim == "defaults:":
			stack = append(stack, stackFrame{indent: indent, kind: "defaults"})
		case indent == 0 && trim == "tools:":
			stack = append(stack, stackFrame{indent: indent, kind: "tools"})
		case len(stack) > 0 && stack[len(stack)-1].kind == "tools" && strings.HasSuffix(trim, ":"):
			currentTool = strings.TrimSuffix(trim, ":")
			cfg.Tools[currentTool] = ToolPolicy{}
			stack = append(stack, stackFrame{indent: indent, kind: "tool", tool: currentTool})
		case len(stack) > 0 && (stack[len(stack)-1].kind == "defaults" || stack[len(stack)-1].kind == "tool"):
			if err := applyScalarOrOpenBlock(cfg, &stack, trim, indent); err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
		case len(stack) > 0 && stack[len(stack)-1].kind == "normalize":
			field, val, ok := splitKV(trim)
			if !ok {
				return nil, fmt.Errorf("line %d: expected key: value in normalize block", i+1)
			}
			tp := cfg.Tools[stack[len(stack)-1].tool]
			if tp.Normalize == nil {
				tp.Normalize = map[string][]string{}
			}
			tp.Normalize[field] = parseInlineList(val)
			cfg.Tools[stack[len(stack)-1].tool] = tp
		case len(stack) > 0 && stack[len(stack)-1].kind == "rate_limit":
			field, val, ok := splitKV(trim)
			if !ok {
				return nil, fmt.Errorf("line %d: expected key: value in rate_limit block", i+1)
			}
			tool := stack[len(stack)-1].tool
			tp := cfg.Tools[tool]
			switch field {
			case "rps":
				f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
				if err != nil {
					return nil, fmt.Errorf("line %d: rate_limit.rps: %w", i+1, err)
				}
				tp.RateLimit.RPS = f
			case "burst":
				n, err := strconv.Atoi(strings.TrimSpace(val))
				if err != nil {
					return nil, fmt.Errorf("line %d: rate_limit.burst: %w", i+1, err)
				}
				tp.RateLimit.Burst = n
			default:
				return nil, fmt.Errorf("line %d: unknown rate_limit key %q", i+1, field)
			}
			cfg.Tools[tool] = tp
		default:
			return nil, fmt.Errorf("line %d: unexpected token %q", i+1, trim)
		}
	}

	return cfg, nil
}

func applyScalarOrOpenBlock(cfg *Config, stack *[]stackFrame, trim string, indent int) error {
	top := (*stack)[len(*stack)-1]

	if trim == "normalize:" {
		*stack = append(*stack, stackFrame{indent: indent, kind: "normalize", tool: top.tool})
		return nil
	}
	if trim == "rate_limit:" {
		*stack = append(*stack, stackFrame{indent: indent, kind: "rate_limit", tool: top.tool})
		return nil
	}

	key, val, ok := splitKV(trim)
	if !ok {
		return fmt.Errorf("expected key: value, got %q", trim)
	}

	tp := selectPolicy(cfg, top)

	switch key {
	case "mode":
		tp.Mode = Mode(strings.TrimSpace(val))
	case "ttl":
		d, err := parseDuration(strings.TrimSpace(val))
		if err != nil {
			return fmt.Errorf("ttl: %w", err)
		}
		tp.TTL = d
	case "semantic_threshold":
		f, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return fmt.Errorf("semantic_threshold: %w", err)
		}
		tp.SemanticThreshold = f
	case "require_human_confirm_on_replay":
		tp.RequireHumanConfirmOnReplay = strings.TrimSpace(val) == "true"
	case "fingerprint_fields":
		tp.FingerprintFields = parseInlineList(val)
	case "allowed_callers":
		tp.AllowedCallers = parseInlineList(val)
	case "replay_strategy":
		v := strings.TrimSpace(val)
		switch ReplayStrategy(v) {
		case ReplayCachedResponse, ReplaySynthesizedAck, "":
			tp.ReplayStrategy = ReplayStrategy(v)
		default:
			return fmt.Errorf("replay_strategy: unknown value %q (want cached_response or synthesized_ack)", v)
		}
	case "synthesized_response":
		tp.SynthesizedResponse = strings.Trim(strings.TrimSpace(val), `"'`)
	case "redact_request_body":
		tp.RedactRequestBody = strings.TrimSpace(val) == "true"
	default:
		return fmt.Errorf("unknown key %q", key)
	}

	writePolicy(cfg, top, tp)
	return nil
}

func selectPolicy(cfg *Config, frame stackFrame) ToolPolicy {
	if frame.kind == "defaults" {
		return cfg.Defaults
	}
	return cfg.Tools[frame.tool]
}

func writePolicy(cfg *Config, frame stackFrame, p ToolPolicy) {
	if frame.kind == "defaults" {
		cfg.Defaults = p
		return
	}
	cfg.Tools[frame.tool] = p
}

// stackFrame represents an open block in the YAML parser.
// kind is one of: "defaults", "tools", "tool", "normalize", "rate_limit".
type stackFrame struct {
	indent int
	kind   string
	tool   string
}

func stripComment(s string) string {
	if i := strings.Index(s, "#"); i >= 0 {
		return s[:i]
	}
	return s
}

func leadingSpaces(s string) int {
	n := 0
	for _, r := range s {
		if r == ' ' {
			n++
			continue
		}
		break
	}
	return n
}

func splitKV(s string) (string, string, bool) {
	idx := strings.Index(s, ":")
	if idx < 0 {
		return "", "", false
	}
	return strings.TrimSpace(s[:idx]), strings.TrimSpace(s[idx+1:]), true
}

// parseDuration extends time.ParseDuration with "d" (24h) and "w" (168h) suffixes.
func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, errors.New("empty duration")
	}
	last := s[len(s)-1]
	if last == 'd' || last == 'w' {
		nStr := s[:len(s)-1]
		n, err := strconv.Atoi(nStr)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}
		mult := 24 * time.Hour
		if last == 'w' {
			mult = 7 * 24 * time.Hour
		}
		return time.Duration(n) * mult, nil
	}
	return time.ParseDuration(s)
}

func parseInlineList(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return []string{s}
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
