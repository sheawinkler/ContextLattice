package calleridentity

import "testing"

func TestExecutionLabels(t *testing.T) {
	for _, tc := range []struct {
		name         string
		facts        Facts
		want, status string
	}{
		{"version first", Facts{Harness: "codex", Model: "gpt-6.2-astra", Effort: "max"}, "codex-gpt-astra-6.2-max", "complete"},
		{"family first", Facts{Harness: "hermes-agent", Model: "anthropic/claude-opus-5-5", Effort: "xhigh"}, "hermes-claude-opus-5.5-xhigh", "complete"},
		{"versioned snapshot", Facts{Harness: "hermes-ultra", Model: "claude-3-5-sonnet-20241022", Effort: "low"}, "hermes-ultra-claude-sonnet-3.5-low", "complete"},
		{"opaque deployment", Facts{Harness: "pi", Model: "deployment42", Origin: "lab", Family: "birch", Version: "2.7", Effort: "medium"}, "pi-lab-birch-2.7-medium", "complete"},
		{"missing effort", Facts{Harness: "codex", Model: "gpt-6.2-sol"}, "", "partial"},
		{"no family", Facts{Harness: "codex", Model: "gpt-5.4", Effort: "high"}, "", "partial"},
		{"bad effort", Facts{Harness: "codex", Model: "gpt-6.2-sol", Effort: "high/secret"}, "", "partial"},
		{"unknown harness", Facts{Harness: "agent-cli", Model: "gpt-6.2-sol", Effort: "high"}, "", "partial"},
		{"inconsistent raw and reported", Facts{Harness: "pi", Model: "lab-birch-2", Family: "ash", Effort: "max"}, "", "conflicting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.facts.Metadata("stable-owner")
			got, _ := m["execution_label"].(string)
			if got != tc.want || m["execution_status"] != tc.status {
				t.Fatalf("unexpected facts: %#v", m)
			}
		})
	}
}
