package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func frontierT8CLIWritePayload(t *testing.T, payload map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "payload.json")
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFrontierT8SkillEvolutionCLIUsesSingleCanonicalRouteFamily(t *testing.T) {
	tests := []struct {
		operation       string
		wireOperation   string
		contractID      string
		explicitHandoff bool
	}{
		{operation: "reusable-candidate", wireOperation: "derive_reusable_candidate", contractID: "reusable_skill_candidate.v1"},
		{operation: "foundry-handoff", wireOperation: "handoff_reusable_candidate", contractID: "reusable_skill_candidate.v1", explicitHandoff: true},
		{operation: "retirement-candidate", wireOperation: "derive_retirement_candidate", contractID: "skill_retirement_candidate.v1"},
	}
	for _, test := range tests {
		t.Run(test.operation, func(t *testing.T) {
			var captured map[string]any
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != frontierT8CLISkillEvolutionPath {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true, "schema_id": test.contractID, "operation": test.wireOperation,
					"candidate": map[string]any{"schema_id": test.contractID},
					"format_contract": map[string]any{
						"schema_id":  test.contractID,
						"validation": map[string]any{"status": "passed"},
					},
				})
			}))
			defer gateway.Close()

			payloadPath := frontierT8CLIWritePayload(t, map[string]any{"name": "bounded-candidate"})
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			if err := c.run([]string{"contextlattice_agent_tools", "skill-evolution", test.operation, "--payload-file", payloadPath, "--raw"}); err != nil {
				t.Fatalf("run CLI: %v", err)
			}
			if captured["operation"] != test.wireOperation {
				t.Fatalf("wire operation=%#v want %q", captured["operation"], test.wireOperation)
			}
			if asMap(captured["input"])["name"] != "bounded-candidate" {
				t.Fatalf("input was not preserved: %#v", captured)
			}
			if asBool(captured["explicit_handoff"]) != test.explicitHandoff {
				t.Fatalf("explicit_handoff=%#v want %t", captured["explicit_handoff"], test.explicitHandoff)
			}
			if bytes.Contains(stdout.Bytes(), []byte(payloadPath)) {
				t.Fatalf("CLI leaked its local input path: %s", stdout.String())
			}
			var output map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &output); err != nil || firstString(output["schema_id"]) != test.contractID {
				t.Fatalf("unexpected CLI output=%s err=%v", stdout.String(), err)
			}
		})
	}
	if _, exists := nativeToolNames["contextlattice_skill_evolution"]; exists {
		t.Fatal("skill evolution must not add a global wrapper alias")
	}
}

func TestFrontierT8SkillEvolutionCLIRejectsUnpassedContractAndUnsafeInputFile(t *testing.T) {
	payloadPath := frontierT8CLIWritePayload(t, map[string]any{"name": "bounded-candidate"})
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "schema_id": "reusable_skill_candidate.v1",
			"format_contract": map[string]any{"validation": map[string]any{"status": "failed"}},
		})
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice_agent_tools", "skill-evolution", "reusable-candidate", "--payload-file", payloadPath, "--raw"}); err == nil {
		t.Fatal("CLI accepted a response with failed format_contract validation")
	}

	unsafePath := filepath.Join(t.TempDir(), "unsafe.json")
	if err := os.WriteFile(unsafePath, []byte(`{"name":"bounded-candidate"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := frontierT8ReadSkillEvolutionInput(unsafePath); err == nil {
		t.Fatal("CLI accepted a non-owner-only payload file")
	}
}
