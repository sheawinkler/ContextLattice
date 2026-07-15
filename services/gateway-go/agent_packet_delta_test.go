package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

const (
	frontierT2PacketBaselineSHA256 = "fd9b445567674a5253a8b6a29a8689383be9574c1cd6f38b216e903fe78198a8"
	frontierT2PacketHoldoutSHA256  = "34ee2e3d47e703b79d735b5b4c8bd1ba36c7165b09275a723c5a94c2d4997cc2"
)

func frontierT2PacketRequest() map[string]any {
	return map[string]any{
		"output_mode":                agentPacketContractID,
		"target_context_pack_tokens": defaultAgentPacketTargetTokens,
		"hard_limit_tokens":          defaultAgentPacketHardTokens,
		"project":                    "contextlattice",
		"topic_path":                 "runbooks/frontier-30",
		"session_id":                 "sess_frontier_t2_holdout",
		"agent_id":                   "codex_gpt5",
		"task_id":                    "task_frontier_t2_holdout",
		"task_identity_id":           "task_identity_frontier_t2_holdout",
		"execution_lane_id":          "lane_frontier_t2_holdout",
		"packet_ttl_seconds":         defaultAgentPacketTTLSeconds,
	}
}

func frontierT2PacketResponse() map[string]any {
	evidence := make([]any, 0, 8)
	for index := 0; index < 8; index++ {
		marker := string(rune('A' + index))
		evidence = append(evidence, map[string]any{
			"kind":       "finding",
			"text":       "Evidence row " + marker + ". " + strings.Repeat("Verified ContextLattice packet evidence remains stable across this holdout row. ", 5),
			"score":      0.99 - float64(index)*0.01,
			"project":    "contextlattice",
			"topic_path": "runbooks/frontier-30",
			"source":     "holdout_fixture",
			"citation":   "fixture:frontier-t2:" + string(rune('a'+index)),
		})
	}
	return map[string]any{
		"ok":         true,
		"query":      "ship the verified Frontier T2 packet protocol",
		"project":    "contextlattice",
		"topic_path": "runbooks/frontier-30",
		"session_id": "sess_frontier_t2_holdout",
		"agent_id":   "codex_gpt5",
		"source_coverage": map[string]any{
			"complete": true,
			"returned": []any{"holdout_fixture", "agent_session_ledger"},
		},
		"context_pack": map[string]any{
			"query":           "ship the verified Frontier T2 packet protocol",
			"project":         "contextlattice",
			"topic_path":      "runbooks/frontier-30",
			"ranked_evidence": evidence,
			"files_to_read":   []any{"services/gateway-go/agent_packet_delta.go"},
		},
		"context_pack_quality": map[string]any{"sample_id": "cpq_frontier_t2_holdout"},
		"run_advisor": map[string]any{
			"next_actions": []any{
				map[string]any{"label": "run_holdout", "command": "go test ./...", "reason": "Verify reconstruction before release."},
			},
		},
		"token_impact": map[string]any{
			"baseline_tokens_estimate":        9000,
			"compiled_prompt_tokens_estimate": 1800,
		},
		"warnings":           []any{"Synthetic holdout warning that can be removed as a tombstone."},
		"writeback_required": true,
		"task_id":            "task_frontier_t2_holdout",
		"task_identity_id":   "task_identity_frontier_t2_holdout",
		"execution_lane_id":  "lane_frontier_t2_holdout",
	}
}

func frontierT2BuildPacket(t testing.TB, response map[string]any, request map[string]any, now time.Time) map[string]any {
	t.Helper()
	packet := finalizeAgentPacketWithIdentity(buildAgentPacket(response, request, "synthesis_pack_v2"), nil, request, now)
	if findings := validateAgentContractPayload(agentPacketContractID, packet); len(findings) > 0 {
		t.Fatalf("expected %s validation passed, got %#v", agentPacketContractID, findings)
	}
	return packet
}

func frontierT2CloneMap(t testing.TB, value map[string]any) map[string]any {
	t.Helper()
	cloned, err := deepCloneAgentPacketMap(value)
	if err != nil {
		t.Fatalf("clone packet: %v", err)
	}
	return cloned
}

func frontierT2DeltaRequest(t testing.TB, base map[string]any) map[string]any {
	t.Helper()
	request := frontierT2PacketRequest()
	request["packet_mode"] = "delta"
	request["base_packet"] = frontierT2CloneMap(t, base)
	identity := anyMap(base["packet_identity"])
	request["base_packet_id"] = identity["packet_id"]
	request["base_digest"] = identity["transport_digest"]
	request["base_revision"] = identity["revision"]
	request["base_ack_cursor"] = identity["ack_cursor"]
	return request
}

func frontierT2ChangedResponse(t testing.TB) map[string]any {
	t.Helper()
	response := frontierT2CloneMap(t, frontierT2PacketResponse())
	response["writeback_required"] = false
	return response
}

func TestFrontierT2PacketFixturesAreFrozen(t *testing.T) {
	fixtures := map[string]string{
		"frontier-t2-packet-baseline.v1.json":      frontierT2PacketBaselineSHA256,
		"frontier-t2-delta-packet-holdout.v1.json": frontierT2PacketHoldoutSHA256,
	}
	for name, expected := range fixtures {
		path := filepath.Join("..", "..", "docs", "evals", "fixtures", name)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read frozen fixture %s: %v", name, err)
		}
		digest := sha256.Sum256(raw)
		if actual := hex.EncodeToString(digest[:]); actual != expected {
			t.Fatalf("fixture %s sha256=%s want=%s", name, actual, expected)
		}
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode frozen fixture %s: %v", name, err)
		}
		if !anyToBool(payload["frozen_before_tuning"]) && !anyToBool(payload["independent_from_training"]) {
			t.Fatalf("fixture %s is not marked frozen/independent", name)
		}
	}
}

func TestAgentPacketIdentityIsDeterministicAndTokenExact(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	request := frontierT2PacketRequest()
	first := frontierT2BuildPacket(t, frontierT2PacketResponse(), request, now)
	second := frontierT2BuildPacket(t, frontierT2PacketResponse(), request, now)
	if agentPacketIdentitySummary(first) != agentPacketIdentitySummary(second) {
		t.Fatalf("same packet input produced different identity: first=%s second=%s", agentPacketIdentitySummary(first), agentPacketIdentitySummary(second))
	}
	identity, reason := validateAgentPacketSelf(first, now.Add(time.Minute), true)
	if reason != "" || len(identity) == 0 {
		t.Fatalf("packet self-verification failed: reason=%s identity=%#v", reason, identity)
	}
	actual := contextPackCountAnyTokens(first).Tokens
	if reported := anyToInt(anyMap(first["token_budget"])["actual_tokens"], 0); reported != actual {
		t.Fatalf("packet token accounting drifted: reported=%d actual=%d", reported, actual)
	}
}

func TestAgentPacketIdentityAccountingConvergesAfterWireRoundTrip(t *testing.T) {
	baseTime := time.Date(2026, 7, 15, 20, 36, 50, 0, time.UTC)
	tests := []struct {
		surface     string
		nanoseconds int
	}{
		{surface: "context_pack", nanoseconds: 0},
		{surface: "synthesis_pack", nanoseconds: 1},
		{surface: "synthesis_pack_v2", nanoseconds: 10},
		{surface: "tools_context_pack", nanoseconds: 123},
		{surface: "tools_synthesis_pack", nanoseconds: 119852215},
		{surface: "tools_synthesis_pack_v2", nanoseconds: 999999999},
	}
	for _, test := range tests {
		t.Run(test.surface, func(t *testing.T) {
			now := baseTime.Add(time.Duration(test.nanoseconds))
			request := frontierT2PacketRequest()
			packet := finalizeAgentPacketWithIdentity(buildAgentPacket(frontierT2PacketResponse(), request, test.surface), nil, request, now)
			raw, err := json.Marshal(packet)
			if err != nil {
				t.Fatal(err)
			}
			var retained map[string]any
			if err := json.Unmarshal(raw, &retained); err != nil {
				t.Fatal(err)
			}
			if _, reason := validateAgentPacketSelf(retained, now.Add(time.Minute), true); reason != "" {
				t.Fatalf("wire-retained packet did not self-verify: reason=%s budget=%#v impact=%#v", reason, retained["token_budget"], retained["token_impact"])
			}
		})
	}
}

func TestAgentPacketDeltaRoundTripSavesExactWireTokens(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	request := frontierT2DeltaRequest(t, base)
	target := finalizeAgentPacketWithIdentity(
		buildAgentPacket(frontierT2ChangedResponse(t), request, "synthesis_pack_v2"),
		anyMap(base["packet_identity"]),
		request,
		now.Add(time.Minute),
	)
	directDelta, err := buildAgentPacketDelta(base, target, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("build delta: code=%s err=%v", reconstructionErrorCode(err), err)
	}
	if budget := anyMap(directDelta["token_budget"]); !anyToBool(budget["delta_smaller_than_full"]) {
		t.Fatalf("direct delta is not economical: %#v", budget)
	}
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(frontierT2ChangedResponse(t), request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("expected delta response, got %#v", delta)
	}
	assertBoundaryContractPassed(t, agentPacketDeltaContractID, delta)
	budget := anyMap(delta["token_budget"])
	if !anyToBool(budget["delta_smaller_than_full"]) || anyToInt(budget["tokens_saved_exact"], 0) <= 0 {
		t.Fatalf("delta did not prove exact savings: %#v", budget)
	}
	if anyToInt(budget["delta_wire_tokens_exact"], 0) != contextPackCountAnyTokens(delta).Tokens {
		t.Fatalf("delta wire-token count is not transport exact: %#v", budget)
	}
	rawDelta, err := json.Marshal(delta)
	if err != nil {
		t.Fatalf("marshal delta for byte accounting: %v", err)
	}
	if reported := anyToInt(anyMap(delta["format_contract"])["actual_json_bytes"], 0); reported != len(rawDelta) {
		t.Fatalf("delta JSON-byte accounting drifted: reported=%d actual=%d", reported, len(rawDelta))
	}
	incrementalTokens := contextPackCountAnyTokens(map[string]any{
		"operations": delta["operations"],
		"tombstones": delta["tombstones"],
	}).Tokens
	if reported := anyToInt(budget["incremental_model_visible_tokens_exact"], 0); reported != incrementalTokens || reported >= anyToInt(budget["delta_wire_tokens_exact"], 0) {
		t.Fatalf("incremental token count is not exact or separated from the wire envelope: reported=%d expected=%d budget=%#v", reported, incrementalTokens, budget)
	}
	reconstructed, err := reconstructAgentPacket(base, delta, now.Add(time.Minute), true)
	if err != nil {
		t.Fatalf("reconstruct delta: %v", err)
	}
	identity := anyMap(reconstructed["packet_identity"])
	if anyToString(identity["transport_digest"]) != anyToString(delta["result_digest"]) || anyToString(identity["model_visible_digest"]) != anyToString(delta["model_visible_digest"]) {
		t.Fatalf("reconstruction digest drift: identity=%#v delta=%#v", identity, delta)
	}
	if findings := validateAgentContractPayload(agentPacketContractID, reconstructed); len(findings) > 0 {
		t.Fatalf("reconstructed packet contract failed: %#v", findings)
	}
}

func TestAgentPacketDeltaUsesExplicitTombstones(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	baseResponse := frontierT2PacketResponse()
	base := frontierT2BuildPacket(t, baseResponse, frontierT2PacketRequest(), now)
	targetResponse := frontierT2CloneMap(t, baseResponse)
	delete(targetResponse, "warnings")
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(targetResponse, request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("expected tombstone delta, got fallback=%#v", delta["delta_fallback"])
	}
	if tombstones := contextPackAnyList(delta["tombstones"]); len(tombstones) != 1 || anyToString(tombstones[0]) != "/warnings" {
		t.Fatalf("expected warnings tombstone, got %#v", tombstones)
	}
	reconstructed, err := reconstructAgentPacket(base, delta, now.Add(time.Minute), true)
	if err != nil {
		t.Fatalf("reconstruct tombstone delta: %v", err)
	}
	if _, exists := reconstructed["warnings"]; exists {
		t.Fatalf("tombstoned warnings survived reconstruction: %#v", reconstructed["warnings"])
	}
}

func TestAgentPacketDeltaUsesNestedEvidenceOperation(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	baseResponse := frontierT2PacketResponse()
	base := frontierT2BuildPacket(t, baseResponse, frontierT2PacketRequest(), now)
	targetResponse := frontierT2CloneMap(t, baseResponse)
	contextPack := anyMap(targetResponse["context_pack"])
	evidence := contextPackAnyList(contextPack["ranked_evidence"])
	anyMap(evidence[3])["text"] = "The packet reconstruction digest was independently verified after one evidence correction."
	contextPack["ranked_evidence"] = evidence
	targetResponse["context_pack"] = contextPack
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(targetResponse, request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("nested evidence change did not produce an economical delta: %#v", delta["delta_fallback"])
	}
	paths := []string{}
	for _, raw := range contextPackAnyList(delta["operations"]) {
		paths = append(paths, anyToString(anyMap(raw)["path"]))
	}
	if !containsString(paths, "/evidence/3/text") {
		t.Fatalf("nested evidence path missing: %#v", paths)
	}
	if _, err := reconstructAgentPacket(base, delta, now.Add(time.Minute), true); err != nil {
		t.Fatalf("reconstruct nested evidence delta: %v", err)
	}
}

func TestAgentPacketDeltaAppendsOneNextActionSafely(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	baseResponse := frontierT2PacketResponse()
	base := frontierT2BuildPacket(t, baseResponse, frontierT2PacketRequest(), now)
	targetResponse := frontierT2CloneMap(t, baseResponse)
	advisor := anyMap(targetResponse["run_advisor"])
	nextActions := contextPackAnyList(advisor["next_actions"])
	nextActions = append(nextActions, map[string]any{
		"label":   "inspect_release_proof",
		"command": "contextlattice context 'release proof' --project contextlattice",
		"reason":  "Verify the immutable release artifact before mutation.",
	})
	advisor["next_actions"] = nextActions
	targetResponse["run_advisor"] = advisor
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(targetResponse, request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("next-action append did not produce an economical delta: %#v", delta["delta_fallback"])
	}
	operations := contextPackAnyList(delta["operations"])
	if len(operations) != 1 || anyToString(anyMap(operations[0])["op"]) != "add" || anyToString(anyMap(operations[0])["path"]) != "/next_actions/1" {
		t.Fatalf("next-action append was not represented as one canonical add: %#v", operations)
	}
	if _, err := reconstructAgentPacket(base, delta, now.Add(time.Minute), true); err != nil {
		t.Fatalf("reconstruct next-action delta: %v", err)
	}
}

func TestAgentPacketUnchangedDeltaCarriesNoOperations(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	baseResponse := frontierT2PacketResponse()
	base := frontierT2BuildPacket(t, baseResponse, frontierT2PacketRequest(), now)
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(baseResponse, request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID || len(contextPackAnyList(delta["operations"])) != 0 {
		t.Fatalf("unchanged packet did not return a zero-op delta: %#v", delta)
	}
	baseIdentity := anyMap(base["packet_identity"])
	resultIdentity := anyMap(delta["result_identity"])
	if anyToInt(resultIdentity["revision"], 0) != anyToInt(baseIdentity["revision"], 0)+1 ||
		anyToString(resultIdentity["base_packet_id"]) != anyToString(baseIdentity["packet_id"]) ||
		anyToString(resultIdentity["base_digest"]) != anyToString(baseIdentity["transport_digest"]) {
		t.Fatalf("unchanged delta did not advance a parent-bound identity: base=%#v result=%#v", baseIdentity, resultIdentity)
	}
	if _, err := reconstructAgentPacket(base, delta, now.Add(time.Minute), true); err != nil {
		t.Fatalf("unchanged delta did not reconstruct: %v", err)
	}
}

func TestAgentPacketDeltaInvalidBasesFallBackSafely(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	target := frontierT2ChangedResponse(t)
	tests := []struct {
		name   string
		reason string
		mutate func(map[string]any)
	}{
		{name: "missing", reason: "base_packet_missing", mutate: func(request map[string]any) { delete(request, "base_packet") }},
		{name: "legacy packet without identity", reason: "base_identity_missing", mutate: func(request map[string]any) {
			delete(anyMap(request["base_packet"]), "packet_identity")
		}},
		{name: "tampered content", reason: "base_digest_mismatch", mutate: func(request map[string]any) { anyMap(request["base_packet"])["prompt"] = "tampered after sealing" }},
		{name: "packet id", reason: "base_packet_id_mismatch", mutate: func(request map[string]any) {
			anyMap(anyMap(request["base_packet"])["packet_identity"])["packet_id"] = "packet_000000000000000000000000"
		}},
		{name: "ack cursor", reason: "base_ack_cursor_mismatch", mutate: func(request map[string]any) {
			anyMap(anyMap(request["base_packet"])["packet_identity"])["ack_cursor"] = "ack_00000000000000000000000000000000"
		}},
		{name: "ack parent binding", reason: "base_ack_cursor_mismatch", mutate: func(request map[string]any) {
			anyMap(anyMap(request["base_packet"])["packet_identity"])["base_packet_id"] = "packet_unacknowledged_parent"
		}},
		{name: "ack issuance binding", reason: "base_ack_cursor_mismatch", mutate: func(request map[string]any) {
			anyMap(anyMap(request["base_packet"])["packet_identity"])["issued_at"] = now.Add(-time.Minute).Format(time.RFC3339Nano)
		}},
		{name: "transport accounting", reason: "base_accounting_mismatch", mutate: func(request map[string]any) {
			packet := anyMap(request["base_packet"])
			budget := anyMap(packet["token_budget"])
			budget["actual_tokens"] = anyToInt(budget["actual_tokens"], 0) + 1
		}},
		{name: "token impact accounting", reason: "base_accounting_mismatch", mutate: func(request map[string]any) {
			packet := anyMap(request["base_packet"])
			impact := anyMap(packet["token_impact"])
			impact["transport_tokens_exact"] = anyToInt(impact["transport_tokens_exact"], 0) + 1
		}},
		{name: "ttl exceeds maximum", reason: "base_validity_window_invalid", mutate: func(request map[string]any) {
			identity := anyMap(anyMap(request["base_packet"])["packet_identity"])
			identity["expires_at"] = now.Add(8 * 24 * time.Hour).Format(time.RFC3339Nano)
			identity["ack_cursor"] = agentPacketAckCursor(identity)
			request["base_ack_cursor"] = identity["ack_cursor"]
		}},
		{name: "requested digest", reason: "base_request_digest_mismatch", mutate: func(request map[string]any) { request["base_digest"] = agentPacketPlaceholderDigest() }},
		{name: "requested revision", reason: "base_revision_mismatch", mutate: func(request map[string]any) { request["base_revision"] = 99 }},
		{name: "requested ack", reason: "base_ack_cursor_mismatch", mutate: func(request map[string]any) { request["base_ack_cursor"] = "ack_00000000000000000000000000000000" }},
		{name: "scope", reason: "base_scope_mismatch", mutate: func(request map[string]any) { request["task_id"] = "task_other_objective" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := frontierT2DeltaRequest(t, base)
			test.mutate(request)
			targetResponse := frontierT2CloneMap(t, target)
			if test.name == "scope" {
				targetResponse["task_id"] = "task_other_objective"
			}
			response := finalizeAgentPacketForRequestAt(buildAgentPacket(targetResponse, request, "synthesis_pack_v2"), request, now.Add(time.Minute))
			if anyToString(response["schema_id"]) != agentPacketContractID {
				t.Fatalf("invalid base emitted delta: %#v", response)
			}
			fallback := anyMap(response["delta_fallback"])
			if anyToString(fallback["reason"]) != test.reason || !anyToBool(fallback["full_packet_verified"]) {
				t.Fatalf("fallback reason=%q want=%q payload=%#v", anyToString(fallback["reason"]), test.reason, fallback)
			}
			assertBoundaryContractPassed(t, agentPacketContractID, response)
			if _, reason := validateAgentPacketSelf(response, now.Add(2*time.Minute), true); reason != "" {
				t.Fatalf("fallback packet did not self-verify: %s", reason)
			}
		})
	}
}

func TestAgentPacketDeltaExpiredBaseFallsBack(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	request := frontierT2DeltaRequest(t, base)
	identity := anyMap(anyMap(request["base_packet"])["packet_identity"])
	identity["issued_at"] = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	identity["expires_at"] = now.Add(-time.Hour).Format(time.RFC3339Nano)
	identity["ack_cursor"] = agentPacketAckCursor(identity)
	request["base_ack_cursor"] = identity["ack_cursor"]
	response := finalizeAgentPacketForRequestAt(buildAgentPacket(frontierT2ChangedResponse(t), request, "synthesis_pack_v2"), request, now)
	if reason := anyToString(anyMap(response["delta_fallback"])["reason"]); reason != "base_expired" {
		t.Fatalf("expired base fallback reason=%q payload=%#v", reason, response)
	}
}

func TestAgentPacketDeltaRollbackFlagReturnsVerifiedFullPacket(t *testing.T) {
	t.Setenv(agentPacketDeltaFeatureEnv, "false")
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	request := frontierT2DeltaRequest(t, base)
	response := finalizeAgentPacketForRequestAt(buildAgentPacket(frontierT2ChangedResponse(t), request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(response["schema_id"]) != agentPacketContractID || anyToString(anyMap(response["delta_fallback"])["reason"]) != "delta_disabled" {
		t.Fatalf("rollback flag did not force a full packet: %#v", response)
	}
	if _, reason := validateAgentPacketSelf(response, now.Add(2*time.Minute), true); reason != "" {
		t.Fatalf("rollback full packet failed verification: %s", reason)
	}
}

func TestAgentPacketReconstructionRejectsTamperReorderAndWrongBase(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	baseResponse := frontierT2PacketResponse()
	base := frontierT2BuildPacket(t, baseResponse, frontierT2PacketRequest(), now)
	targetResponse := frontierT2ChangedResponse(t)
	delete(targetResponse, "warnings")
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(targetResponse, request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID || len(contextPackAnyList(delta["operations"])) < 2 {
		t.Fatalf("test requires a multi-operation delta: %#v", delta)
	}

	tampered := frontierT2CloneMap(t, delta)
	operations := contextPackAnyList(tampered["operations"])
	tamperedAppliedValue := false
	for _, raw := range operations {
		operation := anyMap(raw)
		if anyToString(operation["op"]) == "remove" {
			continue
		}
		operation["value"] = "tampered operation value"
		tamperedAppliedValue = true
		break
	}
	if !tamperedAppliedValue {
		t.Fatal("test requires an operation with an applied value")
	}
	tampered["operations"] = operations
	if _, err := reconstructAgentPacket(base, tampered, now.Add(time.Minute), false); reconstructionErrorCode(err) != "result_digest_mismatch" {
		t.Fatalf("tampered operation error=%v code=%s", err, reconstructionErrorCode(err))
	}
	if response, status := agentPacketReconstructionResponse(base, tampered, now.Add(time.Minute)); status != http.StatusUnprocessableEntity || anyToInt(response["operations_applied"], -1) != 0 {
		t.Fatalf("failed reconstruction reported applied operations: status=%d response=%#v", status, response)
	}

	tamperedAccounting := frontierT2CloneMap(t, delta)
	accounting := anyMap(tamperedAccounting["result_accounting"])
	anyMap(accounting["token_budget"])["target_tokens"] = 777
	if _, err := reconstructAgentPacket(base, tamperedAccounting, now.Add(time.Minute), false); reconstructionErrorCode(err) != "result_accounting_mismatch" {
		t.Fatalf("tampered accounting error=%v code=%s", err, reconstructionErrorCode(err))
	}

	tamperedAccountingReceipt := frontierT2CloneMap(t, delta)
	receipt := anyMap(tamperedAccountingReceipt["result_accounting"])
	receiptBudget := anyMap(receipt["token_budget"])
	receiptBudget["actual_tokens"] = anyToInt(receiptBudget["actual_tokens"], 0) + 1
	if _, err := reconstructAgentPacket(base, tamperedAccountingReceipt, now.Add(time.Minute), false); reconstructionErrorCode(err) != "result_accounting_mismatch" {
		t.Fatalf("tampered finalized accounting receipt error=%v code=%s", err, reconstructionErrorCode(err))
	}

	reordered := frontierT2CloneMap(t, delta)
	operations = contextPackAnyList(reordered["operations"])
	for left, right := 0, len(operations)-1; left < right; left, right = left+1, right-1 {
		operations[left], operations[right] = operations[right], operations[left]
	}
	reordered["operations"] = operations
	if _, err := reconstructAgentPacket(base, reordered, now.Add(time.Minute), false); reconstructionErrorCode(err) != "operation_sequence_invalid" {
		t.Fatalf("reordered operation error=%v code=%s", err, reconstructionErrorCode(err))
	}

	otherResponse := frontierT2PacketResponse()
	anyMap(contextPackAnyList(anyMap(otherResponse["context_pack"])["ranked_evidence"])[0])["text"] = "Different trusted base packet."
	otherBase := frontierT2BuildPacket(t, otherResponse, frontierT2PacketRequest(), now)
	if _, err := reconstructAgentPacket(otherBase, delta, now.Add(time.Minute), false); reconstructionErrorCode(err) != "delta_base_mismatch" {
		t.Fatalf("wrong base error=%v code=%s", err, reconstructionErrorCode(err))
	}
}

func TestAgentPacketReconstructionRejectsFalseParentAndExpiredResult(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(frontierT2ChangedResponse(t), request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("test requires delta response: %#v", delta["delta_fallback"])
	}

	falseParent := frontierT2CloneMap(t, delta)
	falseParentIdentity := anyMap(falseParent["result_identity"])
	falseParentIdentity["base_packet_id"] = "packet_false_parent"
	falseParentIdentity["ack_cursor"] = agentPacketAckCursor(falseParentIdentity)
	falseParent["result_identity"] = falseParentIdentity
	if _, err := reconstructAgentPacket(base, falseParent, now.Add(time.Minute), false); reconstructionErrorCode(err) != "result_identity_mismatch" {
		t.Fatalf("false result parent error=%v code=%s", err, reconstructionErrorCode(err))
	}

	expiringRequest := frontierT2DeltaRequest(t, base)
	expiringRequest["packet_ttl_seconds"] = 60
	expiringDelta := finalizeAgentPacketForRequestAt(buildAgentPacket(frontierT2ChangedResponse(t), expiringRequest, "synthesis_pack_v2"), expiringRequest, now.Add(time.Minute))
	if anyToString(expiringDelta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("test requires expiring delta response: %#v", expiringDelta["delta_fallback"])
	}
	if _, err := reconstructAgentPacket(base, expiringDelta, now.Add(3*time.Minute), true); reconstructionErrorCode(err) != "result_identity_mismatch" {
		t.Fatalf("expired result error=%v code=%s", err, reconstructionErrorCode(err))
	}
}

func TestAgentPacketDeltaRequiresExactTokenizer(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_TOKENIZER_ENCODING", "frontier_t2_missing_tokenizer_encoding")
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	request := frontierT2DeltaRequest(t, base)
	response := finalizeAgentPacketForRequestAt(buildAgentPacket(frontierT2ChangedResponse(t), request, "synthesis_pack_v2"), request, now.Add(time.Minute))
	if anyToString(response["schema_id"]) != agentPacketContractID || anyToString(anyMap(response["delta_fallback"])["reason"]) != "base_tokenizer_inexact" {
		t.Fatalf("inexact tokenizer emitted an exact delta claim: %#v", response)
	}
}

func TestAgentPacketDeltaOperationsMustBeCanonical(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	delta := frontierT2HoldoutMultiOperationDelta(t, base, now)
	tests := []struct {
		name   string
		code   string
		mutate func(map[string]any)
	}{
		{name: "string sequence", code: "operation_sequence_invalid", mutate: func(candidate map[string]any) {
			anyMap(contextPackAnyList(candidate["operations"])[0])["sequence"] = "1"
		}},
		{name: "extra member", code: "operation_kind_invalid", mutate: func(candidate map[string]any) {
			anyMap(contextPackAnyList(candidate["operations"])[0])["unexpected"] = true
		}},
		{name: "overlapping paths", code: "operation_path_overlap", mutate: func(candidate map[string]any) {
			candidate["operations"] = []any{
				map[string]any{"sequence": 1, "op": "replace", "path": "/evidence", "value": []any{}},
				map[string]any{"sequence": 2, "op": "replace", "path": "/evidence/0", "value": map[string]any{}},
			}
			candidate["tombstones"] = []any{}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := frontierT2CloneMap(t, delta)
			test.mutate(candidate)
			if _, err := reconstructAgentPacket(base, candidate, now.Add(time.Minute), false); reconstructionErrorCode(err) != test.code {
				t.Fatalf("canonical operation error=%v code=%s want=%s", err, reconstructionErrorCode(err), test.code)
			}
			found := false
			for _, finding := range validateAgentContractPayload(agentPacketDeltaContractID, candidate) {
				if anyToString(finding["code"]) == test.code {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("boundary validator omitted %s: %#v", test.code, validateAgentContractPayload(agentPacketDeltaContractID, candidate))
			}
		})
	}
}

func TestAgentPacketEndpointForSurface(t *testing.T) {
	tests := map[string]string{
		"context_pack":            "/memory/context-pack",
		"synthesis_pack":          "/memory/synthesis-pack",
		"synthesis_pack_v2":       "/memory/synthesis-pack/v2",
		"tools_context_pack":      "/tools/context_pack",
		"tools_synthesis_pack":    "/tools/synthesis_pack",
		"tools_synthesis_pack_v2": "/tools/synthesis_pack_v2",
	}
	for surface, endpoint := range tests {
		if actual := agentPacketEndpointForSurface(surface); actual != endpoint {
			t.Fatalf("surface=%s endpoint=%s want=%s", surface, actual, endpoint)
		}
	}
}

func TestAgentPacketReconstructionHTTPRouteReturnsVerifiedContract(t *testing.T) {
	now := time.Now().UTC().Add(-time.Minute)
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), frontierT2PacketRequest(), now)
	request := frontierT2DeltaRequest(t, base)
	delta := finalizeAgentPacketForRequestAt(buildAgentPacket(frontierT2ChangedResponse(t), request, "synthesis_pack_v2"), request, now.Add(time.Second))
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("test requires delta response: %#v", delta["delta_fallback"])
	}
	body, err := json.Marshal(map[string]any{"base_packet": base, "delta": delta})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	httpRequest := httptest.NewRequest(http.MethodPost, agentPacketReconstructionRoute, bytes.NewReader(body))
	(&server{}).memoryAgentPacketReconstruct(recorder, httpRequest)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reconstruction route status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode reconstruction route response: %v", err)
	}
	if anyToString(response["schema_id"]) != agentPacketReconstructionContractID || !anyToBool(response["verified"]) || anyToString(anyMap(response["packet"])["schema_id"]) != agentPacketContractID {
		t.Fatalf("route did not return verified reconstruction: %#v", response)
	}
	assertBoundaryContractPassed(t, agentPacketReconstructionContractID, response)
}

func TestAgentPacketReconstructionHTTPRouteBoundsRequestBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		agentPacketReconstructionRoute,
		bytes.NewReader(bytes.Repeat([]byte("x"), maxAgentPacketReconstructionBody+1)),
	)
	(&server{}).memoryAgentPacketReconstruct(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "invalid_json") {
		t.Fatalf("oversized reconstruction request was not bounded: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAgentPacketDeltaToolSurfacesPreserveWireContracts(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	results := make([]any, 0, 8)
	for index := 0; index < 8; index++ {
		marker := string(rune('a' + index))
		results = append(results, map[string]any{
			"project": "contextlattice", "file": "notes/packet-" + marker + ".md", "source": "qdrant",
			"score": 0.99 - float64(index)*0.01,
			"summary": "Agent Packet delta tool surface evidence " + marker + ". " +
				strings.Repeat("This deterministic evidence makes the retained full packet materially larger than its incremental update. ", 4),
			"topic_path": "runbooks/frontier-30",
		})
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/retrieval/query" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results, "warnings": []any{}})
	}))
	defer backend.Close()

	gateway := httptest.NewServer(buildMux(newTestServer(t, backend.URL)))
	defer gateway.Close()
	routes := map[string]string{
		"/tools/context_pack":      "tools_context_pack",
		"/tools/synthesis_pack":    "tools_synthesis_pack",
		"/tools/synthesis_pack_v2": "tools_synthesis_pack_v2",
	}
	for route, expectedSurface := range routes {
		route := route
		expectedSurface := expectedSurface
		t.Run(route, func(t *testing.T) {
			request := map[string]any{
				"query": "verify Agent Packet tool delta", "project": "contextlattice",
				"topic_path": "runbooks/frontier-30", "output_mode": agentPacketContractID,
				"agent_id": "codex_gpt5", "task_id": "task_tool_delta",
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			base := postJSONForTest(t, gateway.URL+route, string(raw))
			if anyToString(base["schema_id"]) != agentPacketContractID || anyToString(base["surface"]) != expectedSurface || base["tool"] != nil {
				t.Fatalf("tool full packet wire envelope drifted: %#v", base)
			}
			assertBoundaryContractPassed(t, agentPacketContractID, base)
			if _, reason := validateAgentPacketSelf(base, time.Now().UTC(), true); reason != "" {
				t.Fatalf("tool full packet is not a valid retained delta base: reason=%s budget=%#v impact=%#v", reason, base["token_budget"], base["token_impact"])
			}
			identity := anyMap(base["packet_identity"])
			request["packet_mode"] = "delta"
			request["base_packet"] = base
			request["base_packet_id"] = identity["packet_id"]
			request["base_digest"] = identity["transport_digest"]
			request["base_revision"] = identity["revision"]
			request["base_ack_cursor"] = identity["ack_cursor"]
			raw, err = json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			delta := postJSONForTest(t, gateway.URL+route, string(raw))
			if anyToString(delta["schema_id"]) != agentPacketDeltaContractID || anyToString(delta["surface"]) != expectedSurface || delta["tool"] != nil {
				t.Fatalf("tool delta wire envelope drifted: %#v", delta)
			}
			assertBoundaryContractPassed(t, agentPacketDeltaContractID, delta)
			if _, err := reconstructAgentPacket(base, delta, time.Now().UTC(), true); err != nil {
				t.Fatalf("tool delta did not reconstruct: %v", err)
			}
		})
	}
}

func TestFrontierT2AgentPacketProjectionLatencyGate(t *testing.T) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	request := frontierT2PacketRequest()
	base := frontierT2BuildPacket(t, frontierT2PacketResponse(), request, now)
	deltaRequest := frontierT2DeltaRequest(t, base)
	target := finalizeAgentPacketWithIdentity(
		buildAgentPacket(frontierT2ChangedResponse(t), deltaRequest, "synthesis_pack_v2"),
		anyMap(base["packet_identity"]),
		deltaRequest,
		now.Add(time.Minute),
	)
	baseIdentity, reason := validateAgentPacketSelf(base, now.Add(time.Minute), true)
	if reason != "" {
		t.Fatalf("validate latency-gate base: %s", reason)
	}
	if _, err := buildAgentPacketDeltaFromValidatedBase(base, baseIdentity, target, now.Add(time.Minute)); err != nil {
		t.Fatalf("warm delta projection: %v", err)
	}
	durations := make([]time.Duration, 0, 30)
	for index := 0; index < 30; index++ {
		started := time.Now()
		if _, err := buildAgentPacketDeltaFromValidatedBase(base, baseIdentity, target, now.Add(time.Minute)); err != nil {
			t.Fatalf("delta projection sample %d: %v", index, err)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[int(float64(len(durations)-1)*0.95)]
	if p95 > 20*time.Millisecond {
		t.Fatalf("delta projection p95=%s exceeds 20ms gate", p95)
	}
	t.Logf("delta_projection_samples=%d p95_ms=%.6f", len(durations), float64(p95)/float64(time.Millisecond))
}

func reconstructionErrorCode(err error) string {
	var target *agentPacketReconstructionError
	if errors.As(err, &target) {
		return target.Code
	}
	return ""
}

func BenchmarkAgentPacketDeltaProjection(b *testing.B) {
	now := time.Date(2026, 7, 15, 18, 30, 0, 0, time.UTC)
	request := frontierT2PacketRequest()
	base := finalizeAgentPacketWithIdentity(buildAgentPacket(frontierT2PacketResponse(), request, "synthesis_pack_v2"), nil, request, now)
	deltaRequest := frontierT2DeltaRequest(b, base)
	target := finalizeAgentPacketWithIdentity(
		buildAgentPacket(frontierT2ChangedResponse(b), deltaRequest, "synthesis_pack_v2"),
		anyMap(base["packet_identity"]),
		deltaRequest,
		now.Add(time.Minute),
	)
	baseIdentity, reason := validateAgentPacketSelf(base, now.Add(time.Minute), true)
	if reason != "" {
		b.Fatalf("validate benchmark base: %s", reason)
	}
	if _, err := buildAgentPacketDeltaFromValidatedBase(base, baseIdentity, target, now.Add(time.Minute)); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := buildAgentPacketDeltaFromValidatedBase(base, baseIdentity, target, now.Add(time.Minute)); err != nil {
			b.Fatal(err)
		}
	}
}
