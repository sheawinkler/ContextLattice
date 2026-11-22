package main

import (
	"slices"
	"testing"
)

func TestHTTPWriteIndexesOnlyCompleteCallerFacts(t *testing.T) {
	raw := map[string]any{"agent_id": "hermes_agent", "tags": []string{"note"}, "caller_attribution": map[string]any{
		"source": "caller_reported", "harness": "hermes", "model_known": true, "model": "lab-2.0-maple", "reasoning_effort": "low", "execution_label": "forged",
	}}
	m := normalizeWriteMetadata(raw)
	if m.agentID != "hermes_agent" || !slices.Contains(m.tags, "execution:hermes-lab-maple-2.0-low") || slices.Contains(m.tags, "execution:forged") {
		t.Fatal(m)
	}
	delete(raw["caller_attribution"].(map[string]any), "reasoning_effort")
	if m := normalizeWriteMetadata(raw); !slices.Equal(m.tags, []string{"note"}) {
		t.Fatal(m)
	}
}
