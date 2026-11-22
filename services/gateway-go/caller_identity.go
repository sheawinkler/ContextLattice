package main

import (
	"reflect"

	"github.com/contextlattice/gateway-go/internal/calleridentity"
)

// Context session telemetry records the invoked caller's model, not the model
// or attribution retained by a previous invocation of this session.
func contextCallerAttribution(request map[string]any, agentID string) map[string]any {
	raw := anyMap(request["caller_attribution"])
	if raw["source"] != "caller_reported" {
		return nil
	}
	read := func(k string) string { s, _ := raw[k].(string); return s }
	model := read("model")
	if raw["model_known"] != true {
		model = ""
	}
	f := calleridentity.Facts{Harness: read("harness"), Model: model, Origin: read("model_origin"),
		Family: read("model_family"), Version: read("model_version"), Effort: read("reasoning_effort"),
		Observation: read("observation")}
	return f.Metadata(agentID)
}

// Refresh a reused row without modifying ownership, extending its TTL, or
// rewriting the observation recorded at session start. Skip identical facts.
func (s *agentSessionStore) persistReuseFactsLocked(existing, candidate, payload map[string]any, scopeChanged bool) error {
	id := anyToString(existing["id"])
	facts := contextCallerAttribution(anyMap(payload["metadata"]), anyToString(existing["agent_id"]))
	changed := facts != nil && !reflect.DeepEqual(existing["execution_identity"], facts)
	if !scopeChanged && !changed {
		return nil
	}
	if changed {
		candidate["execution_identity"] = facts
	}
	s.sessions[id] = candidate
	if err := s.persistLocked(); err != nil {
		if !ownerOnlyAtomicWriteCommitted(err) {
			s.sessions[id] = existing
		}
		return err
	}
	return nil
}
