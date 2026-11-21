package decisionresearch

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// Decode rejects duplicate members (including case aliases), trailing values
// and deep nesting. Authorization and decision fields must have one reading.
func Decode(data []byte, dst any, strict bool) error {
	invalid := errors.New("ambiguous or invalid research JSON")
	if len(data) == 0 || len(data) > 256<<10 {
		return invalid
	}
	tokens := json.NewDecoder(bytes.NewReader(data))
	tokens.UseNumber()
	var scan func(int) bool
	scan = func(depth int) bool {
		if depth > 32 {
			return false
		}
		v, err := tokens.Token()
		if err != nil {
			return false
		}
		delim, ok := v.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for tokens.More() {
				k, e := tokens.Token()
				name, ok := k.(string)
				key := strings.ToLower(name)
				if e != nil || !ok || seen[key] {
					return false
				}
				seen[key] = true
				if !scan(depth + 1) {
					return false
				}
			}
			end, e := tokens.Token()
			return e == nil && end == json.Delim('}')
		case '[':
			for tokens.More() {
				if !scan(depth + 1) {
					return false
				}
			}
			end, e := tokens.Token()
			return e == nil && end == json.Delim(']')
		default:
			return false
		}
	}
	if !scan(0) {
		return invalid
	}
	if _, err := tokens.Token(); err != io.EOF {
		return invalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if strict {
		d.DisallowUnknownFields()
	}
	if d.Decode(dst) != nil {
		return invalid
	}
	return nil
}
