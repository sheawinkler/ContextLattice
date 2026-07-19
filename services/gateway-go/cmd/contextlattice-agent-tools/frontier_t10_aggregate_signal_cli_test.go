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

func frontierT10CLIPassedResponse(schemaID string) map[string]any {
	return map[string]any{
		"ok": true, "schema_id": schemaID,
		"format_contract": map[string]any{"validation": map[string]any{"status": "passed"}},
	}
}

func TestFrontierT10AggregateSignalCLIIsCanonicalAndBuildsBoundedPreview(t *testing.T) {
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != frontierT10CLIAggregatePath {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(frontierT10CLIPassedResponse(frontierT10CLIContributionContractID))
	}))
	defer gateway.Close()
	var stdout bytes.Buffer
	c := newCLI(&stdout, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice_aggregate_signal", "preview", "--metric", "repair_rate",
		"--source", "manual", "--value", "0.2", "--cohort-window", "2026-W29", "--raw",
	}); err != nil {
		t.Fatal(err)
	}
	if nativeToolNames["contextlattice_aggregate_signal"] != "aggregate-signal" {
		t.Fatalf("canonical Aggregate Signal basename is missing: %#v", nativeToolNames)
	}
	if firstString(captured["operation"]) != "preview" || firstString(captured["metric"]) != "repair_rate" || captured["value"] != 0.2 {
		t.Fatalf("CLI did not preserve bounded preview input: %#v", captured)
	}
}

func TestFrontierT10AggregateSignalCLIQueueRequiresExplicitFlagsOnly(t *testing.T) {
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(frontierT10CLIPassedResponse(frontierT10CLIContributionContractID))
	}))
	defer gateway.Close()
	c := newCLI(ioDiscard{}, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{
		"contextlattice", "aggregate-signal", "queue", "--metric", "repair_rate", "--value", "0.1",
		"--nonce", "0123456789abcdef", "--opt-in", "--raw",
	}); err != nil {
		t.Fatal(err)
	}
	if !asBool(captured["opt_in"]) || firstString(captured["contribution_nonce"]) != "0123456789abcdef" {
		t.Fatalf("queue consent or nonce was not explicit: %#v", captured)
	}
	if _, exists := captured["project"]; exists {
		t.Fatalf("CLI introduced forbidden project scope: %#v", captured)
	}
}

func TestFrontierT10AggregateSignalCLIGovernanceUsesOwnerOnlyPayloadAndPaidRoute(t *testing.T) {
	payloadPath := filepath.Join(t.TempDir(), "governance.json")
	if err := os.WriteFile(payloadPath, []byte(`{"operation":"status"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var captured map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != frontierT10CLIGovernancePath {
			t.Fatalf("governance used wrong route: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&captured)
		_ = json.NewEncoder(w).Encode(frontierT10CLIPassedResponse(frontierT10CLIGovernanceContractID))
	}))
	defer gateway.Close()
	c := newCLI(ioDiscard{}, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "aggregate-signal", "governance", "--payload-file", payloadPath, "--raw"}); err != nil {
		t.Fatal(err)
	}
	if firstString(captured["operation"]) != "status" {
		t.Fatalf("governance operation was rewritten: %#v", captured)
	}
	unsafePath := filepath.Join(t.TempDir(), "unsafe.json")
	if err := os.WriteFile(unsafePath, []byte(`{"operation":"status"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafePath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := frontierT10ReadAggregateInput(unsafePath); err == nil {
		t.Fatal("Aggregate Signal CLI accepted a non-owner-only payload file")
	}
}

func TestFrontierT10AggregateSignalCLIRejectsUnpassedContract(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "schema_id": frontierT10CLIAccountantContractID,
			"format_contract": map[string]any{"validation": map[string]any{"status": "failed"}},
		})
	}))
	defer gateway.Close()
	c := newCLI(ioDiscard{}, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "aggregate-signal", "status", "--raw"}); err == nil {
		t.Fatal("Aggregate Signal CLI accepted an unpassed response contract")
	}
}

func TestFrontierT10AggregateSignalCLIWritesOwnerOnlyArtifact(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(frontierT10CLIPassedResponse(frontierT10CLIAccountantContractID))
	}))
	defer gateway.Close()
	output := filepath.Join(t.TempDir(), "aggregate-status.json")
	c := newCLI(ioDiscard{}, ioDiscard{})
	c.baseURL = gateway.URL
	if err := c.run([]string{"contextlattice", "aggregate-signal", "status", "--output", output, "--raw"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Aggregate Signal artifact is not owner-only: info=%v err=%v", info, err)
	}
}
