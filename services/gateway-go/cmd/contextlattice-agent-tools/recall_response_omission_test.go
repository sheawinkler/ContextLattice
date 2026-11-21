package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecallResponseCLIOmissionBoundary(t *testing.T) {
	const item = "ref_item"
	const other = "ref_other"
	tests := []struct {
		name   string
		want   string
		mutate func(disclosure, row, binding map[string]any)
	}{
		{name: "bound"},
		{name: "clipped", mutate: func(disclosure, _, binding map[string]any) {
			disclosure["proof_union"] = []any{other}
			binding["proof_refs"] = []any{}
		}},
		{name: "false_digest", want: "invalid omission binding", mutate: func(_, _, binding map[string]any) {
			binding["binding_digest"] = "sha256:" + strings.Repeat("a", 64)
		}},
		{name: "legacy_shape", want: "unexpected nested field", mutate: func(_, _, binding map[string]any) {
			binding["union_digest"] = binding["binding_digest"]
		}},
		{name: "unknown_field", want: "unexpected nested field", mutate: func(_, _, binding map[string]any) {
			binding["extra"] = "unknown"
		}},
		{name: "forged_in_union", want: "invalid omission binding", mutate: func(_, _, binding map[string]any) {
			binding["proof_refs"] = []any{other}
		}},
		{name: "invented_ref", want: "invalid omission binding", mutate: func(_, _, binding map[string]any) {
			binding["proof_refs"] = []any{"ref_invented"}
		}},
		{name: "wrong_ref_type", want: "invalid omission binding", mutate: func(_, _, binding map[string]any) {
			binding["proof_refs"] = []any{map[string]any{}}
		}},
		{name: "null_proof_list", want: "invalid omission binding", mutate: func(_, _, binding map[string]any) {
			binding["proof_refs"] = nil
		}},
		{name: "unsupported_counterfactual", want: "invalid omission binding", mutate: func(_, row, _ map[string]any) {
			row["same_snapshot_counterfactual"] = map[string]any{"verified": true, "outcome": "no_loss_verified"}
		}},
		{name: "ledger_overflow", want: "invalid omission binding", mutate: func(disclosure, row, _ map[string]any) {
			disclosure["omission_ledger"] = []any{row, row, row}
		}},
		{name: "duplicate_receipt", want: "invalid omission binding", mutate: func(disclosure, row, _ map[string]any) {
			disclosure["omission_ledger"] = []any{row, row}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := failureRecallResponse()
			disclosure := asMap(response["disclosure"])
			disclosure["proof_union"] = []any{item, other}
			binding := map[string]any{"proof_refs": []any{item}}
			row := map[string]any{
				"item_ref": item, "item_type": "evidence", "reason": "response_budget_context", "protected": false,
				"evidence_binding": binding, "same_snapshot_counterfactual": map[string]any{"verified": false, "outcome": "not_verified"},
			}
			disclosure["omission_ledger"] = []any{row}
			if tc.mutate != nil {
				tc.mutate(disclosure, row, binding)
			}
			disclosure["union_digest"] = recallResponseCLIUnionDigest(disclosure)
			if _, exists := binding["binding_digest"]; !exists {
				binding["binding_digest"] = recallResponseCLIHashObject(map[string]any{
					"item_ref": item, "item_type": "evidence", "proof_refs": binding["proof_refs"],
					"union_digest": disclosure["union_digest"], "continuation_digest": disclosure["continuation_digest"],
				})
			}
			response["response_id"] = cliRecallResponseID(response)
			response["response_digest"] = cliRecallResponseDigest(response)
			body, err := json.Marshal(response)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			got, err := compactRecallResponse(response)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("expected %q rejection, got %v", tc.want, err)
				}
				return
			}
			out, _ := json.Marshal(got)
			if err != nil || string(out) != string(body) {
				t.Fatalf("valid omission was rejected or changed: %v", err)
			}
		})
	}
}

func TestRecallResponseCLICursorDoesNotChangeIdentityOrPermitRouteSubstitution(t *testing.T) {
	response := failureRecallResponse()
	disclosure := asMap(response["disclosure"])
	action := asMap(disclosure["continuation_action"])
	action["kind"] = "continue_snapshot"
	action["method"] = "POST"
	action["route"] = "/memory/recall/response"
	action["snapshot_semantics"] = "same_snapshot"
	action["request_contract"] = "token+scope+agent+request_digest"
	action["request_digest"] = "sha256:" + strings.Repeat("a", 64)
	action["token"] = "rrct_" + strings.Repeat("e", 48)
	action["page"] = 1
	body, _ := json.Marshal(response)
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if _, err := compactRecallResponse(response); err != nil {
		t.Fatalf("transport cursor changed semantic identity: %v", err)
	}
	asMap(asMap(response["disclosure"])["continuation_action"])["route"] = "/memory/claims"
	if _, err := compactRecallResponse(response); err == nil || !strings.Contains(err.Error(), "invalid continuation action") {
		t.Fatalf("unbound cursor route was accepted: %v", err)
	}
}
