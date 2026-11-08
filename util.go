package agents

import "strings"

// ExtractJSONObject returns the first balanced JSON object found in raw.
// If no complete object is found, it returns raw unchanged.
func ExtractJSONObject(raw string) string {
	start := strings.Index(raw, "{")
	if start < 0 {
		return raw
	}
	depth, inString, escaped := 0, false, false
	for idx := start; idx < len(raw); idx++ {
		ch := raw[idx]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return raw[start : idx+1]
			}
		}
	}
	return raw
}

// ExtractJSONArray returns the first balanced JSON array found in raw.
// If no complete array is found, it returns raw unchanged.
func ExtractJSONArray(raw string) string {
	start := strings.Index(raw, "[")
	if start < 0 {
		return raw
	}
	depth, inString, escaped := 0, false, false
	for idx := start; idx < len(raw); idx++ {
		ch := raw[idx]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch ch {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return raw[start : idx+1]
			}
		}
	}
	return raw
}
