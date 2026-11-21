// Package calleridentity formats bounded, self-reported model attribution.
package calleridentity

import "strings"

func Model(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "", "unknown", "default", "auto", "none", "null", "n/a":
		return ""
	}
	if !label(value, 120) {
		return ""
	}
	return value
}

func Tag(agentID, model string) string {
	model = Model(model)
	if model == "" || !label(agentID, 120) {
		return ""
	}
	return agentID + "-" + model
}

func CallerTag(agentID string) string {
	if !label(agentID, 120) {
		return ""
	}
	return "caller:" + agentID
}

func Metadata(agentID, model string) map[string]any {
	model = Model(model)
	value := map[string]any{"source": "caller_reported", "model_known": model != ""}
	if model != "" {
		value["model"] = model
	}
	if tag := Tag(agentID, model); tag != "" {
		value["tag"] = tag
	}
	return value
}

func label(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.HasPrefix(value, "/") || strings.Contains(value, ":/") {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' ||
			ch >= '0' && ch <= '9' || strings.ContainsRune("-_./:", ch)) {
			return false
		}
	}
	return true
}
