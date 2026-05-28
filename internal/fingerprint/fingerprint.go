package fingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Exact returns a deterministic hash of the canonicalized JSON value.
// Canonicalization: object keys sorted recursively.
func Exact(v any) (string, error) {
	canon, err := canonicalize(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalize(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				buf = append(buf, ',')
			}
			kb, _ := json.Marshal(k)
			buf = append(buf, kb...)
			buf = append(buf, ':')
			cb, err := canonicalize(x[k])
			if err != nil {
				return nil, err
			}
			buf = append(buf, cb...)
		}
		buf = append(buf, '}')
		return buf, nil
	case []any:
		buf := []byte{'['}
		for i, item := range x {
			if i > 0 {
				buf = append(buf, ',')
			}
			cb, err := canonicalize(item)
			if err != nil {
				return nil, err
			}
			buf = append(buf, cb...)
		}
		buf = append(buf, ']')
		return buf, nil
	default:
		return json.Marshal(x)
	}
}
