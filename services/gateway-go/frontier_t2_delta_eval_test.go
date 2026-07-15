package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const frontierT2DeltaHoldoutID = "frontier_t2_delta_packet_adversarial_v1"

type frontierT2DeltaHoldoutCase struct {
	CaseID    string         `json:"case_id"`
	Dimension string         `json:"dimension"`
	Phase     string         `json:"phase"`
	Mutation  map[string]any `json:"mutation"`
	Fault     string         `json:"fault"`
	Expected  map[string]any `json:"expected"`
}

func frontierT2DeltaHoldoutCases(t *testing.T) []frontierT2DeltaHoldoutCase {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t2-delta-packet-holdout.v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frozen T2 delta holdout: %v", err)
	}
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != frontierT2PacketHoldoutSHA256 {
		t.Fatalf("T2 delta holdout sha256=%s want=%s", got, frontierT2PacketHoldoutSHA256)
	}
	fixture := struct {
		SchemaID                string                       `json:"schema_id"`
		HoldoutID               string                       `json:"holdout_id"`
		IndependentFromTraining bool                         `json:"independent_from_training"`
		Cases                   []frontierT2DeltaHoldoutCase `json:"cases"`
	}{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode frozen T2 delta holdout: %v", err)
	}
	if fixture.SchemaID != "frontier_t2_delta_packet_holdout.v1" ||
		fixture.HoldoutID != frontierT2DeltaHoldoutID ||
		!fixture.IndependentFromTraining || len(fixture.Cases) != 19 {
		t.Fatalf("unexpected T2 delta holdout identity: schema=%q holdout=%q independent=%t cases=%d", fixture.SchemaID, fixture.HoldoutID, fixture.IndependentFromTraining, len(fixture.Cases))
	}
	seen := map[string]struct{}{}
	for _, row := range fixture.Cases {
		if strings.TrimSpace(row.CaseID) == "" {
			t.Fatal("T2 delta holdout contains an empty case_id")
		}
		if _, exists := seen[row.CaseID]; exists {
			t.Fatalf("T2 delta holdout contains duplicate case_id=%q", row.CaseID)
		}
		seen[row.CaseID] = struct{}{}
	}
	return fixture.Cases
}

func frontierT2HoldoutTargetPacket(t testing.TB, row frontierT2DeltaHoldoutCase, request map[string]any) map[string]any {
	t.Helper()
	response := frontierT2CloneMap(t, frontierT2PacketResponse())
	kind := anyToString(row.Mutation["kind"])
	switch kind {
	case "none", "replace_prompt", "replace_fields", "replace_all_model_visible_fields",
		"tamper_operation_value_after_result_digest", "reverse_operation_order", "apply_delta_to_other_trusted_base":
	case "replace_evidence_text":
		contextPack := anyMap(response["context_pack"])
		evidence := contextPackAnyList(contextPack["ranked_evidence"])
		index := anyToInt(row.Mutation["index"], -1)
		if index < 0 || index >= len(evidence) {
			t.Fatalf("case %s evidence index=%d is out of range", row.CaseID, index)
		}
		anyMap(evidence[index])["text"] = anyToString(row.Mutation["value"])
		contextPack["ranked_evidence"] = evidence
		response["context_pack"] = contextPack
	case "add_next_action":
		advisor := anyMap(response["run_advisor"])
		actions := contextPackAnyList(advisor["next_actions"])
		actions = append(actions, map[string]any{
			"label":   anyToString(row.Mutation["label"]),
			"command": "contextlattice context 'release proof' --project contextlattice",
			"reason":  anyToString(row.Mutation["reason"]),
		})
		advisor["next_actions"] = actions
		response["run_advisor"] = advisor
	case "remove_field":
		if path := anyToString(row.Mutation["path"]); path != "/warnings" {
			t.Fatalf("case %s requested unsupported holdout removal %q", row.CaseID, path)
		}
		delete(response, "warnings")
	case "set_scope_field":
		field := anyToString(row.Mutation["field"])
		value := anyToString(row.Mutation["value"])
		if field != "task_id" && field != "project" {
			t.Fatalf("case %s requested unsupported scope field %q", row.CaseID, field)
		}
		response[field] = value
		request[field] = value
		if field == "project" {
			contextPack := anyMap(response["context_pack"])
			contextPack[field] = value
			response["context_pack"] = contextPack
		}
	default:
		t.Fatalf("case %s has unsupported mutation kind %q", row.CaseID, kind)
	}

	packet := buildAgentPacket(response, request, "synthesis_pack_v2")
	switch kind {
	case "replace_prompt":
		packet["prompt"] = anyToString(row.Mutation["value"])
	case "replace_fields":
		packet["prompt"] = "Continue from the reconstructed packet only after verifying its digest."
		packet["decision_gate"] = map[string]any{
			"decision": "verify",
			"refusal":  false,
			"reasons":  []any{"The release proof must match the reconstructed packet digest."},
			"policy":   "verify packet lineage before material action",
		}
		packet["next_actions"] = []any{
			map[string]any{
				"label":   "verify_packet_lineage",
				"command": "contextlattice_packet_reconstruct --proof",
				"reason":  "Bind the next action to the trusted packet base.",
			},
		}
	case "replace_all_model_visible_fields":
		frontierT2MakePacketEconomicallyIneligible(packet)
	}
	return packet
}

func frontierT2MakePacketEconomicallyIneligible(packet map[string]any) {
	packet["query"] = "x"
	packet["prompt"] = "x"
	packet["evidence"] = []any{}
	packet["provenance"] = map[string]any{"source_count": 0, "sources": []any{}, "citation_count": 0}
	packet["uncertainty"] = map[string]any{
		"status": "insufficient_evidence", "evidence_alignment": 0.0,
		"source_complete": false, "reasons": []any{"x"},
	}
	packet["decision_gate"] = map[string]any{
		"decision": "abstain", "refusal": true, "reasons": []any{"x"}, "policy": "x",
	}
	packet["next_actions"] = []any{}
	packet["continuation"] = map[string]any{
		"status": "none", "result_state": "", "token": "", "pending_sources": []any{},
	}
	packet["outcome"] = map[string]any{"sample_id": "", "session_id": anyToString(packet["session_id"]), "command": ""}
	packet["writeback_required"] = false
	delete(packet, "warnings")
}

func frontierT2ApplyHoldoutFault(t testing.TB, row frontierT2DeltaHoldoutCase, request map[string]any, now time.Time) {
	t.Helper()
	switch row.Fault {
	case "", "none", "tamper_delta_operation", "reorder_delta_operations", "swap_reconstruction_base":
	case "omit_base":
		delete(request, "base_packet")
	case "tamper_base_content_after_digest":
		anyMap(request["base_packet"])["prompt"] = "tampered after sealing"
	case "mismatch_requested_base_digest":
		request["base_digest"] = agentPacketPlaceholderDigest()
	case "mismatch_requested_base_revision":
		request["base_revision"] = 99
	case "expire_base_identity":
		identity := anyMap(anyMap(request["base_packet"])["packet_identity"])
		identity["issued_at"] = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
		identity["expires_at"] = now.Add(-time.Hour).Format(time.RFC3339Nano)
		identity["ack_cursor"] = agentPacketAckCursor(identity)
		request["base_ack_cursor"] = identity["ack_cursor"]
	case "tamper_base_packet_id":
		anyMap(anyMap(request["base_packet"])["packet_identity"])["packet_id"] = "packet_000000000000000000000000"
	case "tamper_ack_cursor":
		anyMap(anyMap(request["base_packet"])["packet_identity"])["ack_cursor"] = "ack_00000000000000000000000000000000"
	case "disable_delta_feature":
		// The caller owns the process-scoped feature flag through t.Setenv.
	default:
		t.Fatalf("case %s has unsupported fault %q", row.CaseID, row.Fault)
	}
}

func frontierT2HoldoutMultiOperationDelta(t testing.TB, base map[string]any, now time.Time) map[string]any {
	t.Helper()
	targetResponse := frontierT2ChangedResponse(t)
	delete(targetResponse, "warnings")
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(targetResponse, request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID || len(contextPackAnyList(delta["operations"])) < 2 {
		t.Fatalf("holdout requires a multi-operation delta, got %#v", delta)
	}
	return delta
}

func frontierT2MutationPaths(delta map[string]any) []string {
	paths := make([]string, 0, len(contextPackAnyList(delta["operations"])))
	for _, raw := range contextPackAnyList(delta["operations"]) {
		paths = append(paths, anyToString(anyMap(raw)["path"]))
	}
	return paths
}

func frontierT2PercentileMillis(samples []time.Duration, percentile float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	index := int(math.Ceil(percentile*float64(len(samples)))) - 1
	index = clampInt(index, 0, len(samples)-1)
	return float64(samples[index]) / float64(time.Millisecond)
}

func TestFrontierT2DeltaPacketHoldout(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	cases := frontierT2DeltaHoldoutCases(t)
	caseResults := make([]map[string]any, 0, len(cases))
	correctCount := 0
	usefulDeltaCount := 0
	verifiedReconstructionCount := 0
	fullFallbackCount := 0
	exactFullTokens := 0
	exactDeltaTokens := 0
	exactTokensSaved := 0
	unsafeDeltaOnInvalidBaseCount := 0
	corruptReconstructionCount := 0

	for _, row := range cases {
		row := row
		passed := t.Run(row.CaseID, func(t *testing.T) {
			t.Setenv(agentPacketDeltaFeatureEnv, "true")
			base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
			expectedSchema := anyToString(row.Expected["response_schema"])
			result := map[string]any{
				"case_id": row.CaseID, "dimension": row.Dimension, "phase": firstNonEmptyStrings(row.Phase, "negotiate"),
				"expected_schema": expectedSchema,
			}

			if row.Phase == "reconstruct" {
				delta := frontierT2HoldoutMultiOperationDelta(t, base, now)
				reconstructionBase := base
				switch row.Fault {
				case "tamper_delta_operation":
					delta = frontierT2CloneMap(t, delta)
					operations := contextPackAnyList(delta["operations"])
					mutated := false
					for _, raw := range operations {
						operation := anyMap(raw)
						if anyToString(operation["op"]) != "remove" {
							operation["value"] = "tampered operation value"
							mutated = true
							break
						}
					}
					if !mutated {
						t.Fatal("tamper holdout requires an operation with a value")
					}
					delta["operations"] = operations
				case "reorder_delta_operations":
					delta = frontierT2CloneMap(t, delta)
					operations := contextPackAnyList(delta["operations"])
					for left, right := 0, len(operations)-1; left < right; left, right = left+1, right-1 {
						operations[left], operations[right] = operations[right], operations[left]
					}
					delta["operations"] = operations
				case "swap_reconstruction_base":
					otherResponse := frontierT2PacketResponse()
					anyMap(contextPackAnyList(anyMap(otherResponse["context_pack"])["ranked_evidence"])[0])["text"] = "Different trusted base packet."
					reconstructionBase = frontierT2BuildPacket(t, otherResponse, frontierT2PacketRequest(), now)
				default:
					t.Fatalf("reconstruction case has unsupported fault %q", row.Fault)
				}
				response, status := agentPacketReconstructionResponse(reconstructionBase, delta, now.Add(time.Minute))
				assertBoundaryContractPassed(t, agentPacketReconstructionContractID, response)
				expectedError := anyToString(row.Expected["error"])
				if anyToString(response["schema_id"]) != expectedSchema || anyToBool(response["ok"]) ||
					anyToBool(response["verified"]) || anyToString(response["error"]) != expectedError || status != http.StatusUnprocessableEntity {
					corruptReconstructionCount++
					t.Fatalf("reconstruction rejection mismatch: status=%d expected_error=%q response=%#v", status, expectedError, response)
				}
				result["observed_schema"] = anyToString(response["schema_id"])
				result["observed_error"] = anyToString(response["error"])
				result["verified"] = false
				result["passed"] = true
				caseResults = append(caseResults, result)
				return
			}

			request := frontierT2DeltaRequest(t, base)
			if row.Fault == "disable_delta_feature" {
				t.Setenv(agentPacketDeltaFeatureEnv, "false")
			}
			targetPacket := frontierT2HoldoutTargetPacket(t, row, request)
			frontierT2ApplyHoldoutFault(t, row, request, now)
			response := finalizeAgentPacketForRequestAt(targetPacket, request, now.Add(time.Minute))
			observedSchema := anyToString(response["schema_id"])
			if observedSchema != expectedSchema {
				if expectedSchema == agentPacketContractID && observedSchema == agentPacketDeltaContractID {
					unsafeDeltaOnInvalidBaseCount++
				}
				t.Fatalf("response schema=%q want=%q payload=%#v", observedSchema, expectedSchema, response)
			}
			result["observed_schema"] = observedSchema

			switch observedSchema {
			case agentPacketDeltaContractID:
				assertBoundaryContractPassed(t, agentPacketDeltaContractID, response)
				budget := anyMap(response["token_budget"])
				fullTokens := anyToInt(budget["full_packet_tokens_exact"], 0)
				deltaTokens := anyToInt(budget["delta_wire_tokens_exact"], 0)
				tokensSaved := anyToInt(budget["tokens_saved_exact"], 0)
				minimumSaved := anyToInt(row.Expected["minimum_tokens_saved"], 0)
				if !anyToBool(budget["delta_smaller_than_full"]) || deltaTokens >= fullTokens || tokensSaved != fullTokens-deltaTokens || tokensSaved < minimumSaved {
					t.Fatalf("delta economics failed: minimum=%d budget=%#v", minimumSaved, budget)
				}
				operations := contextPackAnyList(response["operations"])
				if expectedCount, exists := row.Expected["operation_count"]; exists && len(operations) != anyToInt(expectedCount, -1) {
					t.Fatalf("operation count=%d want=%d", len(operations), anyToInt(expectedCount, -1))
				}
				if tombstone := anyToString(row.Expected["required_tombstone"]); tombstone != "" && !containsString(anyToStringList(response["tombstones"], maxAgentPacketDeltaOperations), tombstone) {
					t.Fatalf("required tombstone %q missing from %#v", tombstone, response["tombstones"])
				}
				paths := frontierT2MutationPaths(response)
				if anyToBool(row.Expected["operation_paths_sorted"]) && !sort.StringsAreSorted(paths) {
					t.Fatalf("operation paths are not canonical: %#v", paths)
				}
				reconstructed, err := reconstructAgentPacket(base, response, now.Add(time.Minute), true)
				if err != nil {
					t.Fatalf("verified holdout delta did not reconstruct: %v", err)
				}
				identity := anyMap(reconstructed["packet_identity"])
				if anyToString(identity["transport_digest"]) != anyToString(response["result_digest"]) ||
					anyToInt(anyMap(reconstructed["token_budget"])["actual_tokens"], 0) != contextPackCountAnyTokens(reconstructed).Tokens {
					t.Fatalf("reconstructed packet fidelity failed: identity=%#v budget=%#v", identity, reconstructed["token_budget"])
				}
				usefulDeltaCount++
				verifiedReconstructionCount++
				exactFullTokens += fullTokens
				exactDeltaTokens += deltaTokens
				exactTokensSaved += tokensSaved
				result["operation_count"] = len(operations)
				result["full_packet_tokens_exact"] = fullTokens
				result["delta_wire_tokens_exact"] = deltaTokens
				result["tokens_saved_exact"] = tokensSaved
				result["verified"] = true
			case agentPacketContractID:
				assertBoundaryContractPassed(t, agentPacketContractID, response)
				fallback := anyMap(response["delta_fallback"])
				expectedReason := anyToString(row.Expected["fallback_reason"])
				if anyToString(fallback["reason"]) != expectedReason || !anyToBool(fallback["full_packet_verified"]) {
					t.Fatalf("fallback mismatch: reason=%q want=%q payload=%#v", anyToString(fallback["reason"]), expectedReason, fallback)
				}
				if _, reason := validateAgentPacketSelf(response, now.Add(2*time.Minute), true); reason != "" {
					t.Fatalf("full fallback failed self-verification: %s", reason)
				}
				fullFallbackCount++
				result["fallback_reason"] = anyToString(fallback["reason"])
				result["verified"] = true
			default:
				t.Fatalf("unexpected holdout response schema %q", observedSchema)
			}
			result["passed"] = true
			caseResults = append(caseResults, result)
		})
		if passed {
			correctCount++
		}
	}

	if correctCount != len(cases) || unsafeDeltaOnInvalidBaseCount != 0 || corruptReconstructionCount != 0 {
		t.Fatalf("T2 holdout gate failed: correct=%d/%d unsafe_delta=%d corrupt_reconstruction=%d", correctCount, len(cases), unsafeDeltaOnInvalidBaseCount, corruptReconstructionCount)
	}
	if usefulDeltaCount == 0 || verifiedReconstructionCount != usefulDeltaCount {
		t.Fatalf("T2 reconstruction fidelity failed: verified=%d useful_deltas=%d", verifiedReconstructionCount, usefulDeltaCount)
	}
	reconstructionDigestFidelity := float64(verifiedReconstructionCount) / float64(usefulDeltaCount)

	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	deltaRequest := frontierT2DeltaRequest(t, base)
	target := finalizeAgentPacketWithIdentity(
		buildAgentPacket(frontierT2ChangedResponse(t), deltaRequest, "synthesis_pack_v2"),
		anyMap(base["packet_identity"]), deltaRequest, now.Add(time.Minute),
	)
	if _, err := buildAgentPacketDelta(base, target, now.Add(time.Minute)); err != nil {
		t.Fatalf("warm T2 holdout projection: %v", err)
	}
	latencySamples := make([]time.Duration, 0, 30)
	for index := 0; index < cap(latencySamples); index++ {
		started := time.Now()
		if _, err := buildAgentPacketDelta(base, target, now.Add(time.Minute)); err != nil {
			t.Fatalf("T2 holdout projection sample %d: %v", index, err)
		}
		latencySamples = append(latencySamples, time.Since(started))
	}
	p50 := frontierT2PercentileMillis(append([]time.Duration(nil), latencySamples...), 0.50)
	p95 := frontierT2PercentileMillis(append([]time.Duration(nil), latencySamples...), 0.95)
	p99 := frontierT2PercentileMillis(append([]time.Duration(nil), latencySamples...), 0.99)
	if p95 > 20 {
		t.Fatalf("T2 holdout projection p95 %.6fms exceeds 20ms gate", p95)
	}

	caseRaw, err := json.Marshal(caseResults)
	if err != nil {
		t.Fatalf("encode deterministic T2 case results: %v", err)
	}
	caseDigest := sha256.Sum256(caseRaw)
	evidence := map[string]any{
		"schema_id":                     "frontier_t2_delta_packet_eval.v1",
		"tested_commit":                 os.Getenv("FRONTIER_T2_TESTED_COMMIT"),
		"frontier_item":                 2,
		"feature":                       "delta_agent_packets",
		"holdout_id":                    frontierT2DeltaHoldoutID,
		"baseline_fixture_sha256":       frontierT2PacketBaselineSHA256,
		"holdout_fixture_sha256":        frontierT2PacketHoldoutSHA256,
		"case_results_sha256":           hex.EncodeToString(caseDigest[:]),
		"sample_count":                  len(cases),
		"correct_count":                 correctCount,
		"useful_delta_count":            usefulDeltaCount,
		"verified_reconstruction_count": verifiedReconstructionCount,
		"full_fallback_count":           fullFallbackCount,
		"release_gates": map[string]any{
			"reconstruction_digest_fidelity":         reconstructionDigestFidelity,
			"corrupt_reconstruction_count":           corruptReconstructionCount,
			"unsafe_delta_on_invalid_base_count":     unsafeDeltaOnInvalidBaseCount,
			"delta_only_when_smaller":                true,
			"full_fallback_always_contract_valid":    true,
			"synchronous_projection_p95_ms_max":      20,
			"synchronous_projection_p95_ms_observed": p95,
		},
		"token_accounting": map[string]any{
			"tokenizer_exact":          true,
			"tokenizer_method":         "tiktoken",
			"tokenizer_encoding":       defaultContextPackTokenizerEncoding,
			"full_packet_tokens_exact": exactFullTokens,
			"delta_wire_tokens_exact":  exactDeltaTokens,
			"tokens_saved_exact":       exactTokensSaved,
			"measurement_scope":        "serialized agent_packet.v1 and agent_packet_delta.v1 transport JSON only; no provider-token or inference-avoidance claim",
		},
		"latency": map[string]any{
			"sample_count": len(latencySamples), "p50_ms": p50, "p95_ms": p95, "p99_ms": p99,
		},
		"cost": map[string]any{
			"provider_calls": 0, "local_inference_calls": 0, "external_network_calls": 0,
		},
		"privacy": map[string]any{
			"raw_prompts_recorded": false, "source_text_recorded": false, "personal_paths_recorded": false,
		},
		"cases": caseResults,
	}
	raw, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatalf("encode T2 delta release evidence: %v", err)
	}
	raw = append(raw, '\n')
	if output := os.Getenv("FRONTIER_T2_ITEM2_EVAL_OUTPUT"); output != "" {
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			t.Fatalf("create T2 delta evidence directory: %v", err)
		}
		if err := os.WriteFile(output, raw, 0o600); err != nil {
			t.Fatalf("write T2 delta evidence: %v", err)
		}
	}
	digest := sha256.Sum256(raw)
	t.Logf(
		"t2_delta_eval_sha256=%s cases=%d useful_deltas=%d fallbacks=%d tokens_saved=%d p95_ms=%.6f",
		hex.EncodeToString(digest[:]), len(cases), usefulDeltaCount, fullFallbackCount, exactTokensSaved, p95,
	)
}
