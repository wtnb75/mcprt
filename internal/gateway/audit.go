package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
)

// defaultMaskKeyPatterns are matched case-insensitively as substrings
// against argument key names: covers apikey/api_key/access_key/private_key
// (key), authorization (auth), password/passwd (pass), credential (cred),
// token.
var defaultMaskKeyPatterns = []string{"key", "auth", "pass", "cred", "token"}

// maskArguments returns a copy of v with any object key matching (case-
// insensitively, by substring) one of defaultMaskKeyPatterns or extraKeys
// replaced with "***". v is either json.RawMessage (tool arguments) or
// map[string]string (prompt arguments); both are normalized to a walkable
// any tree first. A v of neither type, or malformed JSON, falls back to a
// string representation rather than panicking or dropping the field.
func maskArguments(v any, extraKeys []string) any {
	switch t := v.(type) {
	case json.RawMessage:
		var parsed any
		if err := json.Unmarshal(t, &parsed); err != nil {
			return string(t)
		}
		return maskValue(parsed, extraKeys)
	case map[string]string:
		m := make(map[string]any, len(t))
		for k, val := range t {
			m[k] = val
		}
		return maskValue(m, extraKeys)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// maskValue walks a JSON-shaped any tree (the output of json.Unmarshal into
// `any`, or the map maskArguments builds for prompt arguments), replacing
// every object value whose key matches shouldMask.
func maskValue(v any, extraKeys []string) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if shouldMask(k, extraKeys) {
				out[k] = "***"
				continue
			}
			out[k] = maskValue(val, extraKeys)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = maskValue(val, extraKeys)
		}
		return out
	default:
		return t
	}
}

// shouldMask reports whether key matches a default or extra mask pattern,
// case-insensitively, by substring.
func shouldMask(key string, extraKeys []string) bool {
	lower := strings.ToLower(key)
	for _, p := range defaultMaskKeyPatterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	for _, p := range extraKeys {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
