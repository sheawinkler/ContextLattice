package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	frontierT2ProofHoldoutID        = "frontier_t2_proof_timeline_adversarial_v1"
	frontierT2ProofHoldoutSHA256    = "69a841d263ed5269851e43033605f3eadf49774801e76595e6c0d63005e3cc8b"
	frontierT2ProofBaselineSHA256   = "2aeea6290647b203eb2f8836c9188d190f17ff46cac8973385310fdbe5c0af34"
	frontierT2ProofTimelineSession  = "sess_frontier_t2_proof_holdout"
	frontierT2ProofTimelineTask     = "task_frontier_t2_proof_holdout"
	frontierT2ProofTimelineIdentity = "task_identity_frontier_t2_proof_holdout"
)

type frontierT2ProofHoldoutCase struct {
	CaseID    string         `json:"case_id"`
	Dimension string         `json:"dimension"`
	Fault     string         `json:"fault"`
	Expected  map[string]any `json:"expected"`
}

func TestProofTimelineStageForVerificationEvents(t *testing.T) {
	for _, eventType := range []string{"verify.completed", "verification.completed", "tests.verified"} {
		if got := proofTimelineStageForEvent(eventType); got != "verification" {
			t.Fatalf("proofTimelineStageForEvent(%q)=%q want verification", eventType, got)
		}
	}
}

func frontierT2ProofHoldoutCases(t *testing.T) []frontierT2ProofHoldoutCase {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t2-proof-timeline-holdout.v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read frozen proof-timeline holdout: %v", err)
	}
	digest := sha256.Sum256(raw)
	if got := hex.EncodeToString(digest[:]); got != frontierT2ProofHoldoutSHA256 {
		t.Fatalf("proof-timeline holdout sha256=%s want=%s", got, frontierT2ProofHoldoutSHA256)
	}
	fixture := struct {
		SchemaID                string                       `json:"schema_id"`
		HoldoutID               string                       `json:"holdout_id"`
		IndependentFromTraining bool                         `json:"independent_from_training"`
		Cases                   []frontierT2ProofHoldoutCase `json:"cases"`
	}{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode frozen proof-timeline holdout: %v", err)
	}
	if fixture.SchemaID != "frontier_t2_proof_timeline_holdout.v1" || fixture.HoldoutID != frontierT2ProofHoldoutID ||
		!fixture.IndependentFromTraining || len(fixture.Cases) != 15 {
		t.Fatalf("unexpected proof holdout identity: schema=%q holdout=%q independent=%t cases=%d", fixture.SchemaID, fixture.HoldoutID, fixture.IndependentFromTraining, len(fixture.Cases))
	}
	seen := map[string]struct{}{}
	for _, row := range fixture.Cases {
		if _, exists := seen[row.CaseID]; exists || strings.TrimSpace(row.CaseID) == "" {
			t.Fatalf("invalid or duplicate proof holdout case_id=%q", row.CaseID)
		}
		seen[row.CaseID] = struct{}{}
	}
	baselinePath := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t2-proof-timeline-baseline.v1.json")
	baselineRaw, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read frozen proof-timeline baseline: %v", err)
	}
	baselineDigest := sha256.Sum256(baselineRaw)
	if got := hex.EncodeToString(baselineDigest[:]); got != frontierT2ProofBaselineSHA256 {
		t.Fatalf("proof-timeline baseline sha256=%s want=%s", got, frontierT2ProofBaselineSHA256)
	}
	return fixture.Cases
}

func frontierT2ProofEvent(id, eventType, at string, metadata map[string]any) map[string]any {
	return map[string]any{
		"id": id, "session_id": frontierT2ProofTimelineSession, "project": "contextlattice",
		"type": eventType, "status": "active", "summary": eventType + " evidence", "created_at": at,
		"metadata": metadata,
	}
}

func frontierT2ProofBaseSnapshot(now time.Time) agentProofTimelineSnapshot {
	events := []map[string]any{
		frontierT2ProofEvent("evt_context", "context_pack.completed", now.Add(-6*time.Minute).Format(time.RFC3339Nano), map[string]any{"context_pack_quality_sample_id": "cpq_proof"}),
		frontierT2ProofEvent("evt_action", "tool.executed", now.Add(-5*time.Minute).Format(time.RFC3339Nano), map[string]any{"tool": "go_test"}),
		frontierT2ProofEvent("evt_correction", "decision.corrected", now.Add(-4*time.Minute).Format(time.RFC3339Nano), map[string]any{"reason": "evidence changed the implementation"}),
		frontierT2ProofEvent("evt_verification", "tests.verified", now.Add(-3*time.Minute).Format(time.RFC3339Nano), map[string]any{"passed": true}),
		frontierT2ProofEvent("evt_outcome", "context_pack.outcome_reported", now.Add(-2*time.Minute).Format(time.RFC3339Nano), map[string]any{"outcome": map[string]any{"sample_id": "cpq_proof"}}),
		frontierT2ProofEvent("evt_learning", "checkpoint.written", now.Add(-time.Minute).Format(time.RFC3339Nano), map[string]any{"checkpoint_id": "checkpoint_proof"}),
	}
	session := map[string]any{
		"id": frontierT2ProofTimelineSession, "project": "contextlattice", "agent": "codex", "agent_id": "codex_gpt5",
		"status": "completed", "objective": "prove the unified timeline", "task_id": frontierT2ProofTimelineTask,
		"task_identity_id": frontierT2ProofTimelineIdentity, "event_count": len(events),
		"updated_at": now.Format(time.RFC3339Nano),
	}
	anchors := map[string]any{"fixture": map[string]any{"sequence": 1, "digest": "sha256:fixture"}}
	return agentProofTimelineSnapshot{
		Session: session, Events: events, ContinuityIntegrity: true,
		QualitySamples: []map[string]any{{
			"schema_id": contextPackQualitySchemaID, "sample_id": "cpq_proof", "session_id": frontierT2ProofTimelineSession,
			"task_id": frontierT2ProofTimelineTask, "task_identity_id": frontierT2ProofTimelineIdentity, "project": "contextlattice",
			"capturedAt": now.Add(-6 * time.Minute).Format(time.RFC3339Nano), "quality_score": 92, "exact_prompt_tokens_saved": 640,
		}},
		QualityOutcomes: []map[string]any{{
			"schema_id": contextPackQualityOutcomeSchemaID, "outcome_id": "cpo_proof", "sample_id": "cpq_proof",
			"session_id": frontierT2ProofTimelineSession, "task_id": frontierT2ProofTimelineTask,
			"task_identity_id": frontierT2ProofTimelineIdentity, "project": "contextlattice",
			"capturedAt": now.Add(-2 * time.Minute).Format(time.RFC3339Nano), "outcome_class": "success", "retry_count": 0,
		}},
		TokenImpacts: []map[string]any{{
			"schema_id": "contextlattice_token_impact.v1", "sample_id": "cpq_proof", "session_id": frontierT2ProofTimelineSession,
			"task_id": frontierT2ProofTimelineTask, "task_identity_id": frontierT2ProofTimelineIdentity, "project": "contextlattice",
			"capturedAt": now.Add(-6 * time.Minute).Format(time.RFC3339Nano), "transport_tokens_exact": 420, "saved_tokens_estimate": 640,
		}},
		Availability: map[string]bool{
			"continuity": true, "temporal_claim": true, "context_pack_quality": true, "token_impact": true,
		},
		SourceAnchorsBefore: cloneAnyMap(anchors), SourceAnchorsAfter: cloneAnyMap(anchors),
	}
}

func frontierT2ProofGapCodes(proof map[string]any) map[string]struct{} {
	out := map[string]struct{}{}
	for _, raw := range contextPackAnyList(proof["gaps"]) {
		out[anyToString(anyMap(raw)["code"])] = struct{}{}
	}
	return out
}

func frontierT2ProofHasGap(proof map[string]any, code string) bool {
	_, ok := frontierT2ProofGapCodes(proof)[code]
	return ok
}

func frontierT2ProofCloneSnapshot(t testing.TB, source agentProofTimelineSnapshot) agentProofTimelineSnapshot {
	t.Helper()
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatalf("marshal proof snapshot: %v", err)
	}
	var out agentProofTimelineSnapshot
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("clone proof snapshot: %v", err)
	}
	return out
}

func frontierT2ProofRollbackResponse(t *testing.T, snapshot agentProofTimelineSnapshot) map[string]any {
	t.Helper()
	t.Setenv(agentProofTimelineFeatureEnv, "false")
	store := &agentSessionStore{
		maxKeep: 16, maxEvents: 32, idleTTL: 24 * time.Hour,
		sessions: map[string]map[string]any{frontierT2ProofTimelineSession: cloneAnyMap(snapshot.Session)},
		order:    []string{frontierT2ProofTimelineSession},
		events:   map[string][]map[string]any{frontierT2ProofTimelineSession: cloneMapSlice(snapshot.Events)},
	}
	server := &server{agentSessions: store}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/agents/sessions/"+frontierT2ProofTimelineSession+"/proof-timeline", nil)
	server.agentsSessionProofTimeline(recorder, request, frontierT2ProofTimelineSession)
	if recorder.Code != http.StatusOK {
		t.Fatalf("rollback proof endpoint status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode rollback response: %v", err)
	}
	return response
}

func TestFrontierT2ProofTimelineHoldout(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 30, 0, 0, time.UTC)
	cases := frontierT2ProofHoldoutCases(t)
	caseResults := make([]map[string]any, 0, len(cases))
	correct := 0
	for _, row := range cases {
		row := row
		passed := t.Run(row.CaseID, func(t *testing.T) {
			snapshot := frontierT2ProofBaseSnapshot(now)
			var proof map[string]any
			switch row.CaseID {
			case "complete_exact_identity_join":
				proof = buildAgentProofTimeline(snapshot, now)
				assertBoundaryContractPassed(t, agentProofTimelineContractID, proof)
				if !anyToBool(anyMap(proof["integrity"])["complete"]) || anyToString(anyMap(proof["integrity"])["status"]) != "verified" || len(contextPackAnyList(proof["missing_stages"])) != 0 {
					t.Fatalf("complete proof was not verified: %#v", proof)
				}
			case "out_of_order_arrival":
				for left, right := 0, len(snapshot.Events)-1; left < right; left, right = left+1, right-1 {
					snapshot.Events[left], snapshot.Events[right] = snapshot.Events[right], snapshot.Events[left]
				}
				proof = buildAgentProofTimeline(snapshot, now)
				previous := ""
				for _, raw := range contextPackAnyList(proof["timeline"]) {
					orderedAt := anyToString(anyMap(raw)["ordered_at"])
					if previous != "" && orderedAt < previous {
						t.Fatalf("timeline order regressed: %q < %q", orderedAt, previous)
					}
					previous = orderedAt
				}
			case "idempotent_retry_duplicate":
				snapshot.Events = append(snapshot.Events, cloneAnyMap(snapshot.Events[1]))
				proof = buildAgentProofTimeline(snapshot, now)
				metrics := anyMap(proof["metrics"])
				if anyToInt(metrics["duplicate_count"], 0) != 1 || anyToInt(metrics["conflict_count"], 0) != 0 || anyToFloat(metrics["event_link_fidelity"]) != 1 {
					t.Fatalf("idempotent duplicate handling failed: %#v", metrics)
				}
			case "conflicting_duplicate_identity":
				conflict := cloneAnyMap(snapshot.Events[1])
				conflict["summary"] = "different canonical source content"
				snapshot.Events = append(snapshot.Events, conflict)
				proof = buildAgentProofTimeline(snapshot, now)
				if anyToInt(anyMap(proof["metrics"])["conflict_count"], 0) != 1 || !frontierT2ProofHasGap(proof, "identity_collision") {
					t.Fatalf("identity collision was not explicit: %#v", proof)
				}
			case "partial_write_missing_verification":
				filtered := []map[string]any{}
				for _, event := range snapshot.Events {
					if anyToString(event["id"]) != "evt_verification" {
						filtered = append(filtered, event)
					}
				}
				snapshot.Events = filtered
				snapshot.Session["event_count"] = len(filtered)
				proof = buildAgentProofTimeline(snapshot, now)
				if !frontierT2ProofHasGap(proof, "stage_missing") || anyToString(anyMap(anyMap(proof["stages"])["verification"])["status"]) != "missing" {
					t.Fatalf("missing verification was not explicit: %#v", proof)
				}
			case "missing_session":
				proof = missingAgentProofTimeline(frontierT2ProofTimelineSession, now)
				if anyToString(anyMap(proof["integrity"])["status"]) != "unavailable" || !frontierT2ProofHasGap(proof, "session_missing") {
					t.Fatalf("missing session envelope mismatch: %#v", proof)
				}
			case "cross_task_event":
				foreign := frontierT2ProofEvent("evt_foreign", "tool.executed", now.Add(-30*time.Second).Format(time.RFC3339Nano), map[string]any{"ownership": map[string]any{"task_id": "foreign_task"}})
				snapshot.Events = append(snapshot.Events, foreign)
				proof = buildAgentProofTimeline(snapshot, now)
				if anyToInt(anyMap(proof["metrics"])["cross_scope_rejected"], 0) != 1 || !frontierT2ProofHasGap(proof, "cross_scope_rejected") {
					t.Fatalf("foreign task row was not rejected: %#v", proof)
				}
				for _, raw := range contextPackAnyList(proof["timeline"]) {
					if anyToString(anyMap(raw)["source_id"]) == "evt_foreign" {
						t.Fatal("foreign task event leaked into proof timeline")
					}
				}
			case "invalid_event_clock":
				entry := continuityLedgerEntry{
					SchemaID: continuityLedgerEntrySchemaID, Sequence: 1, EntryID: "continuity_invalid_clock", Kind: continuityLedgerKindObjectiveTransition,
					RecordedAt: now.Add(-90 * time.Second).Format(time.RFC3339Nano),
					Payload:    map[string]any{"session_id": frontierT2ProofTimelineSession, "project": "contextlattice", "transition_type": "progressed", "summary": "future clock", "occurred_at": now.Add(time.Hour).Format(time.RFC3339Nano)},
				}
				entry.EntryHash, _ = continuityEntryHash(entry)
				snapshot.ContinuityEntries = append(snapshot.ContinuityEntries, entry)
				proof = buildAgentProofTimeline(snapshot, now)
				if !frontierT2ProofHasGap(proof, "invalid_clock") || !anyToBool(anyMap(proof["integrity"])["ordering_uses_recorded_at"]) {
					t.Fatalf("invalid clock fallback mismatch: %#v", proof)
				}
			case "retention_truncated_session_tail":
				snapshot.Session["event_count"] = len(snapshot.Events) + 3
				proof = buildAgentProofTimeline(snapshot, now)
				if !frontierT2ProofHasGap(proof, "retention_truncated") {
					t.Fatalf("retention truncation was silent: %#v", proof)
				}
			case "corrupt_authoritative_row":
				snapshot.ContinuityIntegrity = false
				proof = buildAgentProofTimeline(snapshot, now)
				if anyToString(anyMap(proof["integrity"])["status"]) != "failed" || !frontierT2ProofHasGap(proof, "corrupt_rows") {
					t.Fatalf("corrupt source did not fail integrity: %#v", proof)
				}
			case "telemetry_without_exact_join_key":
				snapshot.TokenImpacts = append(snapshot.TokenImpacts, map[string]any{
					"impact_id": "legacy_unlinked_impact", "capturedAt": now.Format(time.RFC3339Nano),
					"transport_tokens_exact": 10, "saved_tokens_estimate": 10,
				})
				proof = buildAgentProofTimeline(snapshot, now)
				if anyToInt(anyMap(proof["metrics"])["missing_join_key_count"], 0) != 1 || !frontierT2ProofHasGap(proof, "missing_join_key") || anyToBool(anyMap(proof["integrity"])["timestamp_inference_used"]) {
					t.Fatalf("unlinked telemetry was inferred or hidden: %#v", proof)
				}
			case "claim_history_compacted":
				snapshot.Claims = append(snapshot.Claims, temporalClaim{
					SchemaID: temporalClaimContractID, ClaimID: "claim_proof", Project: "contextlattice", Statement: "the latest verified rule", Status: "active",
					ObservedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), SessionID: frontierT2ProofTimelineSession, Revision: 2,
					CreatedAt: now.Add(-2 * time.Minute).Format(time.RFC3339Nano), UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano),
				})
				proof = buildAgentProofTimeline(snapshot, now)
				if !frontierT2ProofHasGap(proof, "history_compacted") {
					t.Fatalf("claim history compaction was hidden: %#v", proof)
				}
				found := false
				for _, raw := range contextPackAnyList(proof["timeline"]) {
					if anyToString(anyMap(raw)["source_id"]) == "claim_proof:r2" {
						found = true
					}
				}
				if !found {
					t.Fatal("latest claim revision was not emitted")
				}
			case "recursive_sensitive_metadata":
				slackCanary := strings.Join([]string{"xoxb", "123456789012", "abcdefghijklmnopqrstuv"}, "-")
				metadata := anyMap(snapshot.Events[1]["metadata"])
				metadata["secret"] = "do-not-emit"
				metadata["raw_prompt"] = "private prompt"
				metadata["systemPrompt"] = "private system prompt"
				metadata["refresh_token"] = "refresh-do-not-emit"
				metadata["sessionToken"] = "session-do-not-emit"
				metadata["id_token"] = "id-do-not-emit"
				metadata["content"] = "raw model or prompt content must not emit"
				metadata["requestBody"] = "camel request body must not emit"
				metadata["responseBody"] = "camel response body must not emit"
				metadata["toolCalls"] = "camel tool calls must not emit"
				metadata["functionCall"] = "camel function call must not emit"
				metadata["rawContextlatticeJson"] = "camel raw json must not emit"
				metadata["privateKey"] = "-----BEGIN PRIVATE KEY----- camel-private-material"
				metadata["nested"] = map[string]any{
					"authorization": "Bearer abcdefghijklmnopqrstuvwxyz", "path": "/Users/example/private/file",
					"jwt":        "eyJabcdefghijk.abcdefghijklmnop.abcdefghijklmnop",
					"github_pat": "ghp_abcdefghijklmnopqrstuvwxyz123456",
					"slack":      slackCanary,
					"aws":        "AKIAABCDEFGHIJKLMNOP",
					"npm":        "npm_abcdefghijklmnopqrstuvwxyz123456",
					"hf":         "hf_abcdefghijklmnopqrstuvwxyz123456",
					"url":        "https://example.invalid/callback?refresh_token=opaquecredential123456",
					"paths":      []any{"/home/example/private/file", "/root/private/file", "/private/tmp/file", "/tmp/private/file", "/var/folders/example/file"},
				}
				deep := map[string]any{"value": "depth-secret-do-not-emit eyJabcdefghijk.abcdefghijklmnop.abcdefghijklmnop ghp_abcdefghijklmnopqrstuvwxyz123456"}
				for depth := 0; depth < 8; depth++ {
					deep = map[string]any{"ordinary": deep}
				}
				metadata["deep"] = deep
				proof = buildAgentProofTimeline(snapshot, now)
				raw, _ := json.Marshal(proof)
				serialized := string(raw)
				for _, forbidden := range []string{
					"do-not-emit", "private prompt", "private system prompt", "raw model or prompt content", "/Users/", "Bearer abc", "eyJabcdefghijk",
					"depth-secret-do-not-emit", "ghp_", "xoxb-", "AKIAABCDEFGHIJKLMNOP", "npm_abcdefghijklmnopqrstuvwxyz", "hf_abcdefghijklmnopqrstuvwxyz", "opaquecredential123456",
					"camel request body", "camel response body", "camel tool calls", "camel function call", "camel raw json", "camel-private-material",
					"/home/example", "/root/private", "/private/tmp", "/tmp/private", "/var/folders/example",
				} {
					if strings.Contains(serialized, forbidden) {
						t.Fatalf("recursive redaction leaked %q: %s", forbidden, serialized)
					}
				}
				if anyToInt(anyMap(proof["metrics"])["redaction_count"], 0) < 15 {
					t.Fatalf("recursive redaction failed: %s", serialized)
				}
			case "concurrent_snapshot_boundary":
				snapshot.SourceAnchorsAfter = map[string]any{"fixture": map[string]any{"sequence": 2, "digest": "sha256:advanced"}}
				proof = buildAgentProofTimeline(snapshot, now)
				if !frontierT2ProofHasGap(proof, "concurrent_snapshot") || anyToBool(anyMap(proof["source_anchors"])["stable"]) {
					t.Fatalf("concurrent source advance was hidden: %#v", proof)
				}
			case "rollback_to_existing_trace":
				proof = frontierT2ProofRollbackResponse(t, snapshot)
				if anyToString(proof["schema_id"]) != agentRunTraceContractID {
					t.Fatalf("feature rollback schema=%q want=%q", anyToString(proof["schema_id"]), agentRunTraceContractID)
				}
			default:
				t.Fatalf("unimplemented frozen proof holdout case %q", row.CaseID)
			}
			caseResults = append(caseResults, map[string]any{
				"case_id": row.CaseID, "dimension": row.Dimension, "fault": row.Fault,
				"schema_id": anyToString(proof["schema_id"]), "integrity_status": anyToString(anyMap(proof["integrity"])["status"]),
				"passed": true,
			})
		})
		if passed {
			correct++
		}
	}
	if correct != len(cases) {
		t.Fatalf("proof holdout accuracy=%d/%d", correct, len(cases))
	}

	const latencySamples = 100
	base := frontierT2ProofBaseSnapshot(now)
	durations := make([]time.Duration, 0, latencySamples)
	for index := 0; index < latencySamples; index++ {
		started := time.Now()
		proof := buildAgentProofTimeline(base, now)
		if anyToString(proof["schema_id"]) != agentProofTimelineContractID {
			t.Fatalf("latency sample %d returned schema=%q", index, anyToString(proof["schema_id"]))
		}
		durations = append(durations, time.Since(started))
	}
	p95 := frontierT2PercentileMillis(append([]time.Duration(nil), durations...), 0.95)
	strictLatency := frontierT2StrictLatencyGateEnabled()
	if strictLatency && p95 > 20 {
		t.Fatalf("proof projection p95 %.6fms exceeds 20ms gate", p95)
	}
	caseRaw, err := json.Marshal(caseResults)
	if err != nil {
		t.Fatalf("encode proof case results: %v", err)
	}
	caseDigest := sha256.Sum256(caseRaw)
	evidence := map[string]any{
		"schema_id": "frontier_t2_proof_timeline_eval.v1", "frontier_item": 24, "feature": "unified_proof_timeline",
		"tested_commit": os.Getenv("FRONTIER_T2_TESTED_COMMIT"), "holdout_id": frontierT2ProofHoldoutID,
		"baseline_fixture_sha256": frontierT2ProofBaselineSHA256, "holdout_fixture_sha256": frontierT2ProofHoldoutSHA256,
		"case_results_sha256": hex.EncodeToString(caseDigest[:]), "sample_count": len(cases), "correct_count": correct,
		"release_gates": map[string]any{
			"case_classification_accuracy": 1, "eligible_exact_link_coverage": 1, "ordering_fidelity": 1,
			"cross_scope_rejection_rate": 1, "redaction_failure_count": 0, "silent_gap_count": 0,
			"projection_p95_ms": p95, "projection_p95_ms_max": 20,
			"projection_latency_gate_enforced": strictLatency,
			"provider_calls":                   0, "external_network_calls": 0, "authoritative_ledger_mutations": 0,
		},
	}
	if path := strings.TrimSpace(os.Getenv("FRONTIER_T2_PROOF_TIMELINE_EVIDENCE_PATH")); path != "" {
		raw, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatalf("encode proof evidence: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create proof evidence directory: %v", err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write proof evidence: %v", err)
		}
	}
	evidenceRaw, _ := json.Marshal(evidence)
	t.Logf("frontier_t2_proof_timeline_eval=%s", evidenceRaw)
}

func TestAgentProofTimelineIdentityPersistsWithoutPromptContent(t *testing.T) {
	sample := map[string]any{
		"sample_id": "cpq_identity", "session_id": "sess_identity", "task_id": "task_identity",
		"task_identity_id": "task_identity_stable", "project": "contextlattice", "agent_id": "codex_gpt5",
		"baseline_tokens_estimate": 100, "packed_tokens_estimate": 40, "saved_tokens_estimate": 60,
		"raw_prompt": "must never persist",
	}
	entry := tokenImpactEntryFromSample(sample)
	for _, key := range []string{"sample_id", "session_id", "task_id", "task_identity_id", "project", "agent_id"} {
		if anyToString(entry[key]) != anyToString(sample[key]) {
			t.Fatalf("token impact identity %s=%q want=%q", key, anyToString(entry[key]), anyToString(sample[key]))
		}
	}
	if _, leaked := entry["raw_prompt"]; leaked {
		t.Fatal("token impact persisted raw prompt content")
	}
	outcome := contextPackQualityOutcomeFromSample(map[string]any{
		"sample_id": "cpq_identity", "session_id": "sess_identity", "task_identity_id": "task_identity_stable",
		"project": "contextlattice", "first_pass_success": true,
	})
	if anyToString(outcome["session_id"]) != "sess_identity" || anyToString(outcome["task_identity_id"]) != "task_identity_stable" {
		t.Fatalf("quality outcome lost proof identity: %#v", outcome)
	}
}

func TestMemoryContextPackDeltaRouteAccountsProofIdentityBeforeFinalization(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv(agentPacketDeltaFeatureEnv, "true")

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/retrieval/query" {
			http.NotFound(w, r)
			return
		}
		results := make([]any, 0, 8)
		for index := 0; index < 8; index++ {
			results = append(results, map[string]any{
				"project": "contextlattice", "file": fmt.Sprintf("notes/proof-%d.md", index), "source": "qdrant", "score": 0.99 - float64(index)*0.01,
				"summary": strings.Repeat(fmt.Sprintf("verified route evidence %d remains exact ", index), 12), "topic_path": "runbooks/frontier-30",
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results, "warnings": []any{}})
	}))
	defer backend.Close()

	server := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(server))
	defer gateway.Close()
	post := func(payload map[string]any) map[string]any {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.Post(gateway.URL+"/memory/context-pack", "application/json", strings.NewReader(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		decoded := map[string]any{}
		if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("context-pack route status=%d payload=%#v", response.StatusCode, decoded)
		}
		return decoded
	}
	request := map[string]any{
		"project": "contextlattice", "topic_path": "runbooks/frontier-30", "query": "prove route-level delta identity accounting",
		"session_id": "sess_route_delta_identity", "task_id": "task_route_delta_identity", "task_identity_id": "task_identity_route_delta_identity",
		"execution_lane_id": "lane_route_delta_identity", "agent_id": "codex_gpt5", "output_mode": agentPacketContractID,
		"target_context_pack_tokens": defaultAgentPacketTargetTokens, "hard_limit_tokens": defaultAgentPacketHardTokens,
		"_suppress_token_impact_recording": true,
	}
	base := post(request)
	if anyToString(base["schema_id"]) != agentPacketContractID {
		t.Fatalf("base route response is not an Agent Packet: %#v", base)
	}
	baseIdentity := anyMap(base["packet_identity"])
	request["packet_mode"] = "delta"
	request["base_packet"] = base
	request["base_packet_id"] = baseIdentity["packet_id"]
	request["base_digest"] = baseIdentity["transport_digest"]
	request["base_revision"] = baseIdentity["revision"]
	request["base_ack_cursor"] = baseIdentity["ack_cursor"]
	delta := post(request)
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		t.Fatalf("route did not emit an economical delta: %#v", delta)
	}
	impact := anyMap(delta["token_impact"])
	for key, expected := range map[string]string{
		"session_id": "sess_route_delta_identity", "task_id": "task_route_delta_identity",
		"task_identity_id": "task_identity_route_delta_identity", "execution_lane_id": "lane_route_delta_identity",
		"project": "contextlattice", "agent_id": "codex_gpt5",
	} {
		if actual := anyToString(impact[key]); actual != expected {
			t.Fatalf("route delta proof identity %s=%q want=%q", key, actual, expected)
		}
	}
	if anyToString(impact["sample_id"]) == "" {
		t.Fatal("route delta lost the context-pack quality sample identity")
	}
	wireCount := contextPackCountAnyTokens(delta).Tokens
	if anyToInt(anyMap(delta["token_budget"])["delta_wire_tokens_exact"], 0) != wireCount || anyToInt(impact["transport_tokens_exact"], 0) != wireCount {
		t.Fatalf("route delta token accounting drifted: budget=%#v impact=%#v wire=%d", delta["token_budget"], impact, wireCount)
	}
	rawDelta, _ := json.Marshal(delta)
	if anyToInt(anyMap(delta["format_contract"])["actual_json_bytes"], 0) != len(rawDelta) {
		t.Fatalf("route delta byte accounting drifted: reported=%d actual=%d", anyToInt(anyMap(delta["format_contract"])["actual_json_bytes"], 0), len(rawDelta))
	}
}

func TestAgentProofTimelineBoundsLargeEvidenceBeforeContractBoundary(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 0, 0, 0, time.UTC)
	snapshot := frontierT2ProofBaseSnapshot(now)
	for index := 0; index < 300; index++ {
		metadata := map[string]any{}
		for field := 0; field < 4; field++ {
			metadata[fmt.Sprintf("evidence_%d", field)] = strings.Repeat(fmt.Sprintf("wide-%03d-%d ", index, field), 160)
		}
		snapshot.Events = append(snapshot.Events, frontierT2ProofEvent(
			fmt.Sprintf("evt_wide_%03d", index), "tool.executed",
			now.Add(time.Duration(index)*time.Millisecond).Format(time.RFC3339Nano), metadata,
		))
	}
	snapshot.Session["event_count"] = len(snapshot.Events)
	proof := buildAgentProofTimeline(snapshot, now.Add(time.Minute))
	assertBoundaryContractPassed(t, agentProofTimelineContractID, proof)
	limits := agentBoundaryLimitsForContract(agentProofTimelineContractID)
	if size := jsonByteLen(proof); size > limits.MaxTotalJSONBytes {
		t.Fatalf("bounded proof bytes=%d max=%d", size, limits.MaxTotalJSONBytes)
	}
	metrics := anyMap(proof["metrics"])
	if anyToInt(metrics["evidence_compacted_count"], 0) == 0 || anyToInt(metrics["display_compacted_count"], 0) == 0 || anyToInt(metrics["projection_omitted_count"], 0) == 0 {
		t.Fatalf("large evidence was not explicitly compacted and omitted: %#v", metrics)
	}
	displayCompacted := false
	for _, raw := range contextPackAnyList(proof["timeline"]) {
		if anyToBool(anyMap(raw)["display_compacted"]) {
			displayCompacted = true
			break
		}
	}
	if !displayCompacted {
		t.Fatal("display compaction was counted but not surfaced on an emitted row")
	}
	if !frontierT2ProofHasGap(proof, "projection_truncated") || len(contextPackAnyList(proof["timeline"])) > maxAgentProofTimelineRows {
		t.Fatalf("large proof did not preserve explicit bounded projection semantics: %#v", proof["gaps"])
	}
}

func TestAgentProofTimelineConflictDigestPrecedesDisplayCompaction(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 10, 0, 0, time.UTC)
	snapshot := frontierT2ProofBaseSnapshot(now)
	prefix := strings.Repeat("same canonical prefix ", 80)
	left := frontierT2ProofEvent("evt_clipped_conflict", "tool.executed", now.Add(-30*time.Second).Format(time.RFC3339Nano), map[string]any{"detail": prefix + "left"})
	right := frontierT2ProofEvent("evt_clipped_conflict", "tool.executed", now.Add(-30*time.Second).Format(time.RFC3339Nano), map[string]any{"detail": prefix + "right"})
	snapshot.Events = append(snapshot.Events, left, right)
	snapshot.Session["event_count"] = len(snapshot.Events)

	proof := buildAgentProofTimeline(snapshot, now)
	metrics := anyMap(proof["metrics"])
	if anyToInt(metrics["conflict_count"], 0) != 1 || anyToInt(metrics["duplicate_count"], 0) != 0 {
		t.Fatalf("post-clip source difference was not retained as a conflict: %#v", metrics)
	}
	if !frontierT2ProofHasGap(proof, "identity_collision") || anyToInt(metrics["display_compacted_count"], 0) == 0 {
		t.Fatalf("conflict or display compaction was hidden: gaps=%#v metrics=%#v", proof["gaps"], metrics)
	}
}

func TestAgentProofTimelineScopedAnchorsIgnoreUnrelatedWrites(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 20, 0, 0, time.UTC)
	snapshot := frontierT2ProofBaseSnapshot(now)
	scope := proofTimelineScopeFromSession(snapshot.Session, snapshot.Events)
	telemetry := &tokenImpactTelemetry{samples: cloneMapSlice(snapshot.TokenImpacts), sampleCount: int64(len(snapshot.TokenImpacts))}

	before := telemetry.proofTimelineAnchor(scope)
	telemetry.mu.Lock()
	telemetry.samples = append(telemetry.samples, map[string]any{
		"sample_id": "cpq_foreign", "session_id": "sess_foreign", "task_id": "task_foreign",
		"project": "another-project", "capturedAt": now.Format(time.RFC3339Nano),
	})
	telemetry.mu.Unlock()
	afterUnrelated := telemetry.proofTimelineAnchor(scope)
	if proofTimelineDigest(before) != proofTimelineDigest(afterUnrelated) {
		t.Fatalf("unrelated telemetry write advanced a scoped anchor: before=%#v after=%#v", before, afterUnrelated)
	}
	snapshot.SourceAnchorsBefore = map[string]any{"token_impact": before}
	snapshot.SourceAnchorsAfter = map[string]any{"token_impact": afterUnrelated}
	if proof := buildAgentProofTimeline(snapshot, now); frontierT2ProofHasGap(proof, "concurrent_snapshot") {
		t.Fatalf("unrelated write produced a false concurrent snapshot: %#v", proof["gaps"])
	}

	telemetry.mu.Lock()
	telemetry.samples = append(telemetry.samples, map[string]any{
		"sample_id": "cpq_related_new", "session_id": frontierT2ProofTimelineSession,
		"task_id": frontierT2ProofTimelineTask, "project": "contextlattice", "capturedAt": now.Add(time.Second).Format(time.RFC3339Nano),
	})
	telemetry.mu.Unlock()
	afterRelated := telemetry.proofTimelineAnchor(scope)
	if proofTimelineDigest(before) == proofTimelineDigest(afterRelated) {
		t.Fatal("related telemetry write did not advance the scoped anchor")
	}
	snapshot.SourceAnchorsAfter = map[string]any{"token_impact": afterRelated}
	if proof := buildAgentProofTimeline(snapshot, now); !frontierT2ProofHasGap(proof, "concurrent_snapshot") {
		t.Fatalf("related write was not surfaced as concurrent: %#v", proof["gaps"])
	}
}

func TestAgentProofTimelineContinuityAndClaimReadsAreIndexedAndBounded(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 25, 0, 0, time.UTC)
	snapshot := frontierT2ProofBaseSnapshot(now)
	scope := proofTimelineScopeFromSession(snapshot.Session, snapshot.Events)
	continuity := &continuityStore{
		enabled: true, entries: []continuityLedgerEntry{}, proofIdentityIndex: map[string][]int{},
	}
	previousHash := ""
	for index := 0; index < maxAgentProofTimelineSourceRows+12; index++ {
		entry := continuityLedgerEntry{
			SchemaID: continuityLedgerEntrySchemaID, Sequence: uint64(index + 1), EntryID: fmt.Sprintf("continuity_bounded_%04d", index),
			Kind: continuityLedgerKindObjectiveTransition, RecordedAt: now.Add(time.Duration(index) * time.Millisecond).Format(time.RFC3339Nano),
			PreviousHash: previousHash,
			Payload: map[string]any{
				"session_id": frontierT2ProofTimelineSession, "task_id": frontierT2ProofTimelineTask,
				"project": "contextlattice", "transition_type": "progressed", "summary": "bounded exact identity row",
			},
		}
		entry.EntryHash, _ = continuityEntryHash(entry)
		continuity.entries = append(continuity.entries, entry)
		continuity.indexProofTimelineEntryLocked(entry, len(continuity.entries)-1)
		continuity.lastHash = entry.EntryHash
		previousHash = entry.EntryHash
	}
	rows, anchorBefore, valid, available, omitted := continuity.proofTimelineRows(scope)
	if !available || !valid || len(rows) != maxAgentProofTimelineSourceRows || omitted != 12 {
		t.Fatalf("bounded continuity projection mismatch: rows=%d omitted=%d valid=%t available=%t", len(rows), omitted, valid, available)
	}
	foreign := continuityLedgerEntry{
		SchemaID: continuityLedgerEntrySchemaID, Sequence: uint64(len(continuity.entries) + 1), EntryID: "continuity_foreign",
		Kind: continuityLedgerKindObjectiveTransition, RecordedAt: now.Add(time.Second).Format(time.RFC3339Nano), PreviousHash: continuity.lastHash,
		Payload: map[string]any{"session_id": "sess_foreign", "task_id": "task_foreign", "project": "another-project", "transition_type": "progressed"},
	}
	foreign.EntryHash, _ = continuityEntryHash(foreign)
	continuity.entries = append(continuity.entries, foreign)
	continuity.indexProofTimelineEntryLocked(foreign, len(continuity.entries)-1)
	continuity.lastHash = foreign.EntryHash
	if anchorAfter := continuity.proofTimelineAnchor(scope); proofTimelineDigest(anchorBefore) != proofTimelineDigest(anchorAfter) {
		t.Fatalf("unrelated continuity write advanced scoped anchor: before=%#v after=%#v", anchorBefore, anchorAfter)
	}

	claims := &temporalClaimStore{enabled: true, claims: map[string]temporalClaim{}, proofSessionIndex: map[string][]string{}}
	for index := 0; index < maxAgentProofTimelineSourceRows+7; index++ {
		claims.setClaimLocked(temporalClaim{
			ClaimID: fmt.Sprintf("claim_bounded_%04d", index), Project: "contextlattice", SessionID: frontierT2ProofTimelineSession,
			Revision: 1, UpdatedAt: now.Add(time.Duration(index) * time.Millisecond).Format(time.RFC3339Nano),
		})
	}
	claimRows, claimAnchorBefore, claimAvailable, claimOmitted := claims.proofTimelineRows(scope)
	if !claimAvailable || len(claimRows) != maxAgentProofTimelineSourceRows || claimOmitted == 0 {
		t.Fatalf("bounded claim projection mismatch: rows=%d omitted=%d available=%t", len(claimRows), claimOmitted, claimAvailable)
	}
	for index := 0; index < maxAgentProofTimelineSourceScans+17; index++ {
		claims.setClaimLocked(temporalClaim{
			ClaimID: fmt.Sprintf("claim_foreign_%04d", index), Project: "another-project", SessionID: frontierT2ProofTimelineSession,
			Revision: 1, UpdatedAt: now.Add(time.Duration(index) * time.Millisecond).Format(time.RFC3339Nano),
		})
	}
	if claimAnchorAfter := claims.proofTimelineAnchor(scope); proofTimelineDigest(claimAnchorBefore) != proofTimelineDigest(claimAnchorAfter) {
		t.Fatalf("same-session foreign-project claims advanced scoped anchor: before=%#v after=%#v", claimAnchorBefore, claimAnchorAfter)
	}

	snapshot.SourceOmitted = map[string]int{"continuity": omitted, "temporal_claim": claimOmitted}
	if proof := buildAgentProofTimeline(snapshot, now); !frontierT2ProofHasGap(proof, "source_scan_truncated") {
		t.Fatalf("bounded source omission was silent: %#v", proof["gaps"])
	}
}

func TestTemporalClaimProofIndexRetiresReplacedAndTrimmedClaims(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 27, 0, 0, time.UTC)
	store := &temporalClaimStore{
		enabled: true, maxClaims: 2, claims: map[string]temporalClaim{}, proofSessionIndex: map[string][]string{},
	}
	old := temporalClaim{ClaimID: "claim_move", Project: "project-a", SessionID: "session-a", UpdatedAt: now.Add(-3 * time.Minute).Format(time.RFC3339Nano)}
	store.setClaimLocked(old)
	moved := old
	moved.Project = "project-b"
	moved.SessionID = "session-b"
	moved.UpdatedAt = now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	store.setClaimLocked(moved)
	if _, exists := store.proofSessionIndex[temporalClaimProofIndexKey(old.Project, old.SessionID)]; exists {
		t.Fatal("replaced claim retained its stale proof index bucket")
	}
	store.setClaimLocked(temporalClaim{ClaimID: "claim_keep", Project: "project-b", SessionID: "session-keep", UpdatedAt: now.Add(-time.Minute).Format(time.RFC3339Nano)})
	store.setClaimLocked(temporalClaim{ClaimID: "claim_new", Project: "project-b", SessionID: "session-new", UpdatedAt: now.Format(time.RFC3339Nano)})
	store.trimLocked()
	if _, exists := store.claims[moved.ClaimID]; exists {
		t.Fatal("oldest claim was not trimmed")
	}
	if _, exists := store.proofSessionIndex[temporalClaimProofIndexKey(moved.Project, moved.SessionID)]; exists {
		t.Fatal("trimmed claim retained its proof index bucket")
	}
	if len(store.claims) != store.maxClaims || len(store.proofSessionIndex) != store.maxClaims {
		t.Fatalf("proof index retention mismatch: claims=%d buckets=%d", len(store.claims), len(store.proofSessionIndex))
	}
}

func TestAgentProofTimelineTelemetryUsesBoundedProofRingBeforeScopeFiltering(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 29, 0, 0, time.UTC)
	snapshot := frontierT2ProofBaseSnapshot(now)
	scope := proofTimelineScopeFromSession(snapshot.Session, snapshot.Events)
	quality := &contextPackQualityTelemetry{limit: 2, outcomeKeys: map[string]struct{}{}}
	tokens := &tokenImpactTelemetry{limit: 2}
	targetQuality := cloneAnyMap(snapshot.QualitySamples[0])
	targetOutcome := cloneAnyMap(snapshot.QualityOutcomes[0])
	targetToken := cloneAnyMap(snapshot.TokenImpacts[0])
	targetToken["baseline_tokens_estimate"] = 1000
	targetToken["packed_tokens_estimate"] = 400
	quality.applyQualityEntryLocked(targetQuality)
	if !quality.applyOutcomeEntryLocked(targetOutcome) {
		t.Fatal("target quality outcome was not retained")
	}
	tokens.applyEntryLocked(targetToken)
	qualityAnchorBefore := quality.proofTimelineAnchor(scope)
	tokenAnchorBefore := tokens.proofTimelineAnchor(scope)

	for index := 0; index < 117; index++ {
		identity := map[string]any{
			"project": "foreign-project", "session_id": frontierT2ProofTimelineSession,
			"task_id": fmt.Sprintf("foreign-task-%03d", index), "sample_id": fmt.Sprintf("foreign-sample-%03d", index),
			"capturedAt": now.Add(time.Duration(index) * time.Millisecond).Format(time.RFC3339Nano),
		}
		qualityRow := cloneAnyMap(identity)
		qualityRow["quality_score"] = 50
		quality.applyQualityEntryLocked(qualityRow)
		outcomeRow := cloneAnyMap(identity)
		outcomeRow["outcome_id"] = fmt.Sprintf("foreign-outcome-%03d", index)
		if !quality.applyOutcomeEntryLocked(outcomeRow) {
			t.Fatalf("foreign outcome %d was not recorded", index)
		}
		tokenRow := cloneAnyMap(identity)
		tokenRow["baseline_tokens_estimate"] = 1000
		tokenRow["packed_tokens_estimate"] = 500
		tokens.applyEntryLocked(tokenRow)
	}
	if len(quality.samples) != quality.limit || len(quality.outcomes) != quality.limit || len(tokens.samples) != tokens.limit {
		t.Fatalf("ordinary telemetry tail was not bounded: quality=%d outcomes=%d tokens=%d", len(quality.samples), len(quality.outcomes), len(tokens.samples))
	}
	qualitySamples, qualityOutcomes, qualityAnchorAfter, available, qualityOmitted := quality.proofTimelineRows(scope)
	if !available || qualityOmitted != 0 || len(qualitySamples) != 1 || len(qualityOutcomes) != 1 {
		t.Fatalf("scoped quality proof ring mismatch: samples=%d outcomes=%d omitted=%d available=%t", len(qualitySamples), len(qualityOutcomes), qualityOmitted, available)
	}
	if proofTimelineDigest(qualityAnchorBefore) != proofTimelineDigest(qualityAnchorAfter) {
		t.Fatalf("foreign quality flood advanced scoped anchor: before=%#v after=%#v", qualityAnchorBefore, qualityAnchorAfter)
	}
	tokenRows, tokenAnchorAfter, tokenAvailable, tokenOmitted := tokens.proofTimelineRows(scope)
	if !tokenAvailable || tokenOmitted != 0 || len(tokenRows) != 1 {
		t.Fatalf("scoped token proof ring mismatch: rows=%d omitted=%d available=%t", len(tokenRows), tokenOmitted, tokenAvailable)
	}
	if proofTimelineDigest(tokenAnchorBefore) != proofTimelineDigest(tokenAnchorAfter) {
		t.Fatalf("foreign token flood advanced scoped anchor: before=%#v after=%#v", tokenAnchorBefore, tokenAnchorAfter)
	}
}

func TestProofTimelineTelemetryRingTracksScopeSpecificOverflow(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 31, 0, 0, time.UTC)
	snapshot := frontierT2ProofBaseSnapshot(now)
	scope := proofTimelineScopeFromSession(snapshot.Session, snapshot.Events)
	foreign := func(index int) map[string]any {
		return map[string]any{
			"project": "foreign-project", "session_id": frontierT2ProofTimelineSession,
			"sample_id": fmt.Sprintf("foreign-overflow-%04d", index),
		}
	}

	foreignOverflow := &proofTimelineMapRing{}
	for index := 0; index <= maxAgentProofTimelineSourceScans; index++ {
		foreignOverflow.add(foreign(index))
	}
	foreignOverflow.add(cloneAnyMap(snapshot.TokenImpacts[0]))
	source, retentionOmitted := proofTimelineRingSource(foreignOverflow, nil, 0, scope)
	rows, projectionOmitted, _ := proofTimelineMapRowsLocked(source, scope)
	if retentionOmitted != 0 || projectionOmitted != 0 || len(rows) != 1 {
		t.Fatalf("foreign overflow degraded the target scope: rows=%d retention_omitted=%d projection_omitted=%d", len(rows), retentionOmitted, projectionOmitted)
	}

	targetOverflow := &proofTimelineMapRing{}
	targetOverflow.add(cloneAnyMap(snapshot.TokenImpacts[0]))
	for index := 0; index < maxAgentProofTimelineSourceScans; index++ {
		targetOverflow.add(foreign(index))
	}
	source, retentionOmitted = proofTimelineRingSource(targetOverflow, nil, 0, scope)
	rows, projectionOmitted, _ = proofTimelineMapRowsLocked(source, scope)
	if retentionOmitted == 0 || projectionOmitted != 0 || len(rows) != 0 {
		t.Fatalf("target-scope eviction was not explicit: rows=%d retention_omitted=%d projection_omitted=%d", len(rows), retentionOmitted, projectionOmitted)
	}
}

func TestAgentProofTimelineUnavailableContinuityDegradesWithoutClaimingCorruption(t *testing.T) {
	now := time.Date(2026, 7, 16, 5, 30, 0, 0, time.UTC)
	snapshot := frontierT2ProofBaseSnapshot(now)
	snapshot.Availability["continuity"] = false
	snapshot.ContinuityIntegrity = false
	proof := buildAgentProofTimeline(snapshot, now)
	if !anyToBool(proof["ok"]) || anyToString(anyMap(proof["integrity"])["status"]) != "degraded" {
		t.Fatalf("unavailable optional source should degrade without failing proof: %#v", proof["integrity"])
	}
	if !frontierT2ProofHasGap(proof, "source_unavailable") || frontierT2ProofHasGap(proof, "corrupt_rows") {
		t.Fatalf("source absence was mislabeled as corruption: %#v", proof["gaps"])
	}
}

func TestAgentProofTimelineRouteBuildsLiveReadModel(t *testing.T) {
	t.Setenv(agentProofTimelineFeatureEnv, "true")
	now := time.Now().UTC()
	snapshot := frontierT2ProofBaseSnapshot(now)
	store := &agentSessionStore{
		maxKeep: 16, maxEvents: 32, idleTTL: 24 * time.Hour,
		sessions: map[string]map[string]any{frontierT2ProofTimelineSession: cloneAnyMap(snapshot.Session)},
		order:    []string{frontierT2ProofTimelineSession},
		events:   map[string][]map[string]any{frontierT2ProofTimelineSession: cloneMapSlice(snapshot.Events)},
	}
	server := &server{
		agentSessions: store,
		continuity:    &continuityStore{enabled: true, entries: []continuityLedgerEntry{}},
		temporalClaims: &temporalClaimStore{
			enabled: true, claims: map[string]temporalClaim{},
		},
		contextPackQuality: &contextPackQualityTelemetry{
			samples: snapshot.QualitySamples, outcomes: snapshot.QualityOutcomes,
			sampleCount: int64(len(snapshot.QualitySamples)), outcomeCount: int64(len(snapshot.QualityOutcomes)),
		},
		tokenImpact: &tokenImpactTelemetry{samples: snapshot.TokenImpacts, sampleCount: int64(len(snapshot.TokenImpacts))},
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/agents/sessions/"+frontierT2ProofTimelineSession+"/proof-timeline", nil)
	server.agentsSessionProofTimeline(recorder, request, frontierT2ProofTimelineSession)
	if recorder.Code != http.StatusOK {
		t.Fatalf("proof route status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode proof route: %v", err)
	}
	assertBoundaryContractPassed(t, agentProofTimelineContractID, response)
	if anyToString(response["schema_id"]) != agentProofTimelineContractID || !anyToBool(anyMap(response["integrity"])["complete"]) {
		t.Fatalf("live read model mismatch: %#v", response)
	}
	if anyToInt(snapshot.Session["event_count"], 0) != anyToInt(store.sessions[frontierT2ProofTimelineSession]["event_count"], 0) {
		t.Fatal("proof route mutated the authoritative session ledger")
	}
}
