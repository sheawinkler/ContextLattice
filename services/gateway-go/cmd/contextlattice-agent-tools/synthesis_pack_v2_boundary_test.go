package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestFullSynthesisV2CommandRegisteredBoundary(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_SESSION_ID", "")
	t.Setenv("CONTEXTLATTICE_AGENT_CONTRACTS_PATH", "")
	full := synthesisV2BoundaryTestEnvelope(t)
	maximum := asInt(asMap(full["format_contract"])["max_total_json_bytes"])
	if maximum <= maxContextPackResponseJSONBytes {
		t.Fatal("fixture must exercise the synthesis-only byte range")
	}
	packet := testAgentPacketResponse("", "cpq_synthesis_compact_boundary", map[string]any{"surface": "synthesis_pack_v2"})
	stabilizeTestRegisteredEnvelope(packet)

	cases := []struct {
		name     string
		response string
		flags    []string
		want     bool
		mutate   func(map[string]any)
	}{
		{name: "full_exact_ceiling_falls_back_to_compact_json", want: true, flags: []string{"--full", "--pretty"}},
		{name: "debug", want: true, flags: []string{"--debug", "--raw"}},
		{name: "explicit_budget_counts_framing", flags: []string{"--budget-chars", strconv.Itoa(maximum), "--raw"}},
		{name: "explicit_budget_includes_framing", want: true, flags: []string{"--budget-chars", strconv.Itoa(maximum + 1), "--raw"}},
		{name: "other_schema", response: "packet", flags: []string{"--full"}},
		{name: "compact_unchanged", response: "packet", want: true, flags: []string{"--raw"}},
		{name: "full_envelope_not_accepted_for_compact", flags: []string{"--raw"}},
		{name: "wrong_synthesis_schema", flags: []string{"--full"}, mutate: func(response map[string]any) {
			asMap(response["synthesis_pack"])["schema_id"] = "synthesis_pack.v1"
			stabilizeTestRegisteredEnvelope(response)
		}},
		{name: "corrupt_attested_size", flags: []string{"--full"}, mutate: func(response map[string]any) {
			asMap(response["format_contract"])["actual_json_bytes"] = maximum - 1
		}},
		{name: "wrong_attestation_schema", flags: []string{"--full"}, mutate: func(response map[string]any) {
			asMap(response["format_contract"])["schema_id"] = "context_pack_response.v1"
			asMap(response["padding"])["last"] = ""
			stabilizeTestRegisteredEnvelope(response)
		}},
		{name: "over_contract_even_with_larger_explicit_budget", flags: []string{"--budget-chars", strconv.Itoa(maximum + 1000)}, mutate: func(response map[string]any) {
			padding := asMap(response["padding"])
			padding["last"] = firstString(padding["last"]) + "x"
			stabilizeTestRegisteredEnvelope(response)
		}},
	}

	fullJSON, _ := json.Marshal(full)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var response map[string]any
			if tc.response == "packet" {
				response = packet
			} else {
				if err := json.Unmarshal(fullJSON, &response); err != nil {
					t.Fatal(err)
				}
				if tc.mutate != nil {
					tc.mutate(response)
				}
			}
			wire, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			wire = append(wire, '\n')
			var requests int
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var request map[string]any
				if r.Method != http.MethodPost || r.URL.Path != "/memory/synthesis-pack/v2" || json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("unexpected synthesis request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				wantFull := tc.flags[0] != "--raw"
				if request["include_retrieval_debug"] != wantFull ||
					(wantFull && request["output_mode"] != nil) || (!wantFull && request["output_mode"] != agentPacketContractID) {
					t.Errorf("request mode drifted: full=%t", wantFull)
				}
				_, _ = w.Write(wire)
			}))
			defer gateway.Close()
			var stdout bytes.Buffer
			c := newCLI(&stdout, ioDiscard{})
			c.baseURL = gateway.URL
			args := append([]string{"contextlattice", "synthesis-pack-v2", "verify recorded findings", "--no-auto-session"}, tc.flags...)
			err = c.run(args)
			if requests != 1 {
				t.Fatalf("requests=%d, want one", requests)
			}
			if tc.want {
				if err != nil || !bytes.Equal(stdout.Bytes(), wire) {
					t.Fatalf("registered output changed or failed: err=%v, bytes=%d want=%d", err, stdout.Len(), len(wire))
				}
			} else if err == nil || stdout.Len() != 0 {
				t.Fatalf("registered boundary failed open: err=%v, bytes=%d", err, stdout.Len())
			}
		})
	}
}

func synthesisV2BoundaryTestEnvelope(t *testing.T) map[string]any {
	t.Helper()
	contract := adapterContractDefinition(synthesisPackV2ContractID)
	response := map[string]any{}
	for field, typ := range asMap(contract["field_types"]) {
		switch typ {
		case "string":
			response[field] = "sample"
		case "int":
			response[field] = 2
		case "object":
			response[field] = map[string]any{}
		case "bool":
			response[field] = true
		}
	}
	for path, fields := range asMap(contract["required_fields_by_path"]) {
		if path == "format_contract" {
			continue
		}
		object := response
		for _, part := range strings.Split(path, ".") {
			if _, ok := object[part].(map[string]any); !ok {
				object[part] = map[string]any{}
			}
			object = asMap(object[part])
		}
		for _, field := range adapterStringList(fields) {
			if _, exists := object[field]; !exists {
				object[field] = nil
			}
		}
	}
	response["schema_id"] = synthesisPackV2ContractID
	response["reference_prompt"] = "Synthesis Pack v2 test response"
	pack := asMap(response["synthesis_pack"])
	pack["schema_id"], pack["version"] = synthesisPackV2ContractID, 2
	padding := map[string]any{"last": ""}
	for i := 0; i < 24; i++ {
		padding[fmt.Sprintf("block_%02d", i)] = strings.Repeat("x", 7300)
	}
	response["padding"] = padding
	stamped, ok := adapterStampRegisteredEnvelope(synthesisPackV2ContractID, response)
	if !ok {
		t.Fatalf("fixture did not satisfy the registry: %v", adapterContractFindings(synthesisPackV2ContractID, response))
	}
	maximum := asInt(contract["max_total_json_bytes"])
	for range 5 {
		actual := asInt(asMap(stamped["format_contract"])["actual_json_bytes"])
		if actual == maximum {
			return stamped
		}
		amount := len(firstString(padding["last"])) + maximum - actual
		padding["last"] = strings.Repeat("x", amount)
		stamped, ok = adapterStampRegisteredEnvelope(synthesisPackV2ContractID, response)
		if !ok {
			t.Fatal("fixture could not fit the exact registry ceiling")
		}
	}
	t.Fatal("fixture could not stabilize at the exact registry ceiling")
	return nil
}
