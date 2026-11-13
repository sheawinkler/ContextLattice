package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"sort"
	"strings"
)

func cliDecisionTyped(payload map[string]any, names ...string) bool {
	for key := range payload {
		for _, name := range names {
			if strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

func (c *cli) requireDecisionContinuity(timeout float64) error {
	result, _, err := c.requestJSON(context.Background(), http.MethodGet, "/ops/capabilities", nil, min(timeout, 10))
	capability := asMap(result["decisionContinuity"])
	// Runtime readiness is deliberately separate: historical retries must still
	// work when an evidence owner is temporarily unavailable.
	if err != nil || !writeDerivationIntegerEquals(capability["version"], 1) || capability["manual_binding"] != true {
		return errors.New("gateway does not confirm decision continuity v1; upgrade before submitting typed decision intent; no write attempted")
	}
	return nil
}

func cliDecisionJSONEqual(left, right any) bool {
	canonical := func(value any) any {
		raw, _ := json.Marshal(value)
		var result any
		_ = json.Unmarshal(raw, &result)
		object := asMap(result)
		if object["schema_id"] == "decision_applicability.v1" {
			for _, raw := range asList(object["inputs"]) {
				input := asMap(raw)
				if hash, ok := input["target_content_hash"].(string); ok {
					input["target_content_hash"] = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(hash)), "sha256:")
				}
			}
		}
		for _, key := range []string{"inputs", "assumptions"} {
			if rows, ok := object[key].([]any); ok {
				sort.Slice(rows, func(i, j int) bool {
					left, _ := json.Marshal(rows[i])
					right, _ := json.Marshal(rows[j])
					return string(left) < string(right)
				})
			}
		}
		return result
	}
	return reflect.DeepEqual(canonical(left), canonical(right))
}

func normalizeSessionFileAliases(payload map[string]any, start bool) error {
	groups := map[string][]string{
		"project": {"project_name"}, "session_id": {"sessionId"}, "agent_id": {"agentId"},
		"task_id": {"taskId"}, "task_identity_id": {"taskIdentityId"}, "execution_lane_id": {"executionLaneId"},
		"native_session_id": {"nativeSessionId"}, "reuse_key": {"reuseKey"}, "agent_kind": {"agentKind", "runner"},
	}
	if start {
		groups["session_id"] = append(groups["session_id"], "id")
	}
	for canonical, aliases := range groups {
		value, present := payload[canonical]
		for _, alias := range aliases {
			if other, exists := payload[alias]; exists {
				if present && !cliDecisionJSONEqual(value, other) {
					return errors.New("conflicting outer session aliases for " + canonical + "; no write attempted")
				}
				value, present = other, true
			}
		}
		if present {
			payload[canonical] = value
		}
		for _, alias := range aliases {
			delete(payload, alias)
		}
	}
	return nil
}

func cliDecisionBindingReceipt(selected, received map[string]any) bool {
	if len(selected) != 8 || len(received) != 9 || selected["schema_id"] != "session_decision_binding.v1" || !cliDecisionHexDigest(firstString(received["record_digest"])) {
		return false
	}
	for key, value := range selected {
		if !cliDecisionJSONEqual(value, received[key]) {
			return false
		}
	}
	return true
}

func cliDecisionHexDigest(value string) bool {
	_, err := hex.DecodeString(value)
	return err == nil && len(value) == 64 && value == strings.ToLower(value)
}

func cliDecisionStartReceipt(payload, response map[string]any) bool {
	session := asMap(response["session"])
	baseline := asMap(session["decision_baseline"])
	return response["ok"] == true && firstString(session["id"]) != "" &&
		cliDecisionBindingReceipt(asMap(payload["decision_binding"]), asMap(session["decision_binding"])) &&
		baseline["schema_id"] == "session_decision_baseline.v1" && asInt(baseline["revision"]) >= 1 &&
		firstString(baseline["incarnation"]) != "" && cliDecisionHexDigest(firstString(baseline["binding_digest"]))
}

func cliDecisionAckReceipt(payload, response map[string]any) bool {
	event := asMap(response["event"])
	metadata := asMap(event["metadata"])
	baseline := asMap(metadata["decision_baseline"])
	ack := asMap(payload["decision_ack"])
	expected := asMap(ack["expected_baseline"])
	return response["ok"] == true && event["type"] == "decision.bound" && event["id"] == payload["event_id"] &&
		event["session_id"] == payload["session_id"] &&
		cliDecisionBindingReceipt(asMap(payload["decision_binding"]), asMap(metadata["decision_binding"])) &&
		baseline["schema_id"] == "session_decision_baseline.v1" && cliDecisionJSONEqual(baseline["revision"], ack["sequence"]) &&
		baseline["incarnation"] == expected["incarnation"] && cliDecisionHexDigest(firstString(baseline["binding_digest"]))
}

// File values survive defaults, including explicit nulls that the server must
// reject rather than silently downgrade to legacy behavior.
func overlaySessionDefaults(payload map[string]any, defaults map[string]any, parsed parsedArgs) {
	for key, value := range defaults {
		if parsed.has(key) {
			payload[key] = value
		} else if _, exists := payload[key]; !exists {
			if cleaned := dropEmpty(map[string]any{key: value}); len(cleaned) > 0 {
				payload[key] = value
			}
		}
	}
}
