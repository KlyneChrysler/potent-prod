package normalizer

import (
	"strings"
	"unicode"
)

type Transform func(string) string

var registry = map[string]Transform{
	"trim":               strings.TrimSpace,
	"lowercase":          strings.ToLower,
	"uppercase":          strings.ToUpper,
	"collapse_whitespace": collapseWhitespace,
}

func Apply(value string, transforms []string) string {
	out := value
	for _, name := range transforms {
		if fn, ok := registry[name]; ok {
			out = fn(out)
		}
	}
	return out
}

func collapseWhitespace(s string) string {
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteRune(' ')
			}
			prevSpace = true
			continue
		}
		b.WriteRune(r)
		prevSpace = false
	}
	return strings.TrimSpace(b.String())
}
