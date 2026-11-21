package main

import "github.com/contextlattice/gateway-go/internal/calleridentity"

// Context session telemetry records the invoked caller's model, not the model
// or attribution retained by a previous invocation of this session.
func contextCallerAttribution(request map[string]any, agentID string) map[string]any {
	raw := anyMap(request["caller_attribution"])
	if raw["source"] != "caller_reported" {
		return nil
	}
	model, _ := raw["model"].(string)
	if raw["model_known"] != true {
		model = ""
	}
	return calleridentity.Metadata(agentID, model)
}
