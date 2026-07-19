package main

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFrontierT8ReusableSkillCandidateIsVerifiedBoundedAndInactive(t *testing.T) {
	payload := frontierT8TestSkillCandidatePayload()
	before := frontierT8TestJSON(t, payload)
	first, err := frontierT8ReusableSkillCandidate(payload)
	if err != nil {
		t.Fatalf("build reusable skill candidate: %v", err)
	}
	second, err := frontierT8ReusableSkillCandidate(payload)
	if err != nil {
		t.Fatalf("repeat reusable skill candidate: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("candidate is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if after := frontierT8TestJSON(t, payload); before != after {
		t.Fatal("advisory kernel mutated its input")
	}
	if anyToString(first["schema_id"]) != frontierT8ReusableSkillCandidateSchemaID || anyToString(first["status"]) != "review_required" {
		t.Fatalf("unexpected candidate identity: %#v", first)
	}
	if anyToString(anyMap(first["export"])["state"]) != "inactive" || anyToBool(anyMap(first["export"])["activation_allowed"]) {
		t.Fatalf("candidate export must remain inactive: %#v", first["export"])
	}
	provenance := anyMap(first["provenance"])
	for _, key := range []string{"hash_chains_verified", "evidence_resolved", "independent_verification", "training_holdout_separated", "fixture_environment_separated"} {
		if !anyToBool(provenance[key]) {
			t.Fatalf("provenance %s=false: %#v", key, provenance)
		}
	}
	economics := anyMap(first["economics"])
	if !anyToBool(economics["exact"]) || anyToInt(economics["receipt_count"], 0) != 6 || anyToInt(economics["execution_count"], 0) != 6 {
		t.Fatalf("unexpected exact economics: %#v", economics)
	}
	if anyToInt(economics["input_tokens"], 0) != 606 || anyToInt(economics["output_tokens"], 0) != 126 {
		t.Fatalf("token economics were not exactly aggregated: %#v", economics)
	}
	handoff := anyMap(first["skill_foundry_handoff"])
	if anyToString(handoff["target_contract"]) != skillDraftContractID || anyToBool(handoff["automatic_submit"]) {
		t.Fatalf("candidate must extend Foundry through a manual handoff: %#v", handoff)
	}
	safety := anyMap(first["safety"])
	if !anyToBool(safety["advisory_only"]) || anyToInt(safety["filesystem_mutations"], -1) != 0 || anyToInt(safety["provider_calls"], -1) != 0 {
		t.Fatalf("unexpected safety envelope: %#v", safety)
	}
	encoded := frontierT8TestJSON(t, first)
	if len(encoded) > frontierT8MaxOutputBytes {
		t.Fatalf("candidate bytes=%d limit=%d", len(encoded), frontierT8MaxOutputBytes)
	}
}

func TestFrontierT8ReusableSkillCandidateRejectsAdversarialHoldouts(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(map[string]any)
	}{
		{
			name: "superficially similar workflow",
			want: "superficially similar",
			mutate: func(payload map[string]any) {
				holdouts := frontierT8TestReceiptRows(payload, "holdout_receipts")
				holdouts[0]["steps"] = []any{"Compile the candidate.", "Run only a visual smoke check."}
				frontierT8TestRechain(holdouts)
			},
		},
		{
			name: "one off success",
			want: "at least 3",
			mutate: func(payload map[string]any) {
				payload["training_receipts"] = contextPackAnyList(payload["training_receipts"])[:1]
			},
		},
		{
			name: "hidden manual step",
			want: "manual workflow step",
			mutate: func(payload map[string]any) {
				training := frontierT8TestReceiptRows(payload, "training_receipts")
				training[0]["hidden_manual_steps"] = []any{"Approve an unrecorded dialog."}
				frontierT8TestRechain(training)
			},
		},
		{
			name: "stale verification command",
			want: "stale",
			mutate: func(payload map[string]any) {
				training := frontierT8TestReceiptRows(payload, "training_receipts")
				training[0]["verified_at"] = "2025-01-01T00:00:00Z"
				frontierT8TestRechain(training)
			},
		},
		{
			name: "unresolved evidence",
			want: "unresolved",
			mutate: func(payload map[string]any) {
				training := frontierT8TestReceiptRows(payload, "training_receipts")
				ref := anyMap(contextPackAnyList(training[0]["evidence_refs"])[0])
				ref["resolved"] = false
				frontierT8TestRechain(training)
			},
		},
		{
			name: "training holdout evidence leakage",
			want: "training/holdout leakage",
			mutate: func(payload map[string]any) {
				training := frontierT8TestReceiptRows(payload, "training_receipts")
				holdouts := frontierT8TestReceiptRows(payload, "holdout_receipts")
				trainRef := anyMap(contextPackAnyList(training[0]["evidence_refs"])[0])
				holdRef := anyMap(contextPackAnyList(holdouts[0]["evidence_refs"])[0])
				for _, key := range []string{"ref_id", "digest", "resolved_digest"} {
					holdRef[key] = trainRef[key]
				}
				frontierT8TestRechain(holdouts)
			},
		},
		{
			name: "fixture environment leakage",
			want: "fixture_id",
			mutate: func(payload map[string]any) {
				training := frontierT8TestReceiptRows(payload, "training_receipts")
				holdouts := frontierT8TestReceiptRows(payload, "holdout_receipts")
				holdouts[0]["fixture_id"] = training[0]["fixture_id"]
				frontierT8TestRechain(holdouts)
			},
		},
		{
			name: "secret bearing material",
			want: "secret-bearing",
			mutate: func(payload map[string]any) {
				payload["description"] = "Use credential sk-supersecretmaterial1234567890 during verification."
			},
		},
		{
			name: "unsafe workflow material",
			want: "unsafe workflow material",
			mutate: func(payload map[string]any) {
				training := frontierT8TestReceiptRows(payload, "training_receipts")
				training[0]["steps"] = []any{"rm -rf the workspace before rebuilding."}
				frontierT8TestRechain(training)
			},
		},
		{
			name: "broken hash link",
			want: "breaks the hash chain",
			mutate: func(payload map[string]any) {
				training := frontierT8TestReceiptRows(payload, "training_receipts")
				training[1]["previous_receipt_digest"] = "sha256:" + strings.Repeat("f", 64)
				training[1]["receipt_digest"] = frontierT8ReceiptDigest(training[1])
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := frontierT8TestCloneMap(t, frontierT8TestSkillCandidatePayload())
			test.mutate(payload)
			_, err := frontierT8ReusableSkillCandidate(payload)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.want)) {
				t.Fatalf("error=%v want substring %q", err, test.want)
			}
		})
	}
}

func TestFrontierT8SkillRetirementCandidateIsSeparateAndReviewOnly(t *testing.T) {
	payload := frontierT8TestRetirementPayload()
	before := frontierT8TestJSON(t, payload)
	first, err := frontierT8SkillRetirementCandidate(payload)
	if err != nil {
		t.Fatalf("build retirement candidate: %v", err)
	}
	second, err := frontierT8SkillRetirementCandidate(payload)
	if err != nil {
		t.Fatalf("repeat retirement candidate: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("retirement candidate is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if after := frontierT8TestJSON(t, payload); before != after {
		t.Fatal("retirement advisory mutated its input")
	}
	if anyToString(first["schema_id"]) != frontierT8SkillRetirementCandidateSchemaID || anyToString(first["status"]) != "candidate" {
		t.Fatalf("unexpected retirement candidate: %#v", first)
	}
	if anyToBool(first["terminal_retirement"]) || anyToBool(first["mutation_performed"]) || anyToBool(first["automatic"]) {
		t.Fatalf("retirement candidate crossed the terminal boundary: %#v", first)
	}
	approval := anyMap(first["approval"])
	if !anyToBool(approval["required"]) || !anyToBool(approval["explicit"]) || anyToBool(approval["approved"]) || anyToString(approval["state"]) != "pending" {
		t.Fatalf("explicit approval boundary missing: %#v", approval)
	}
	signals := anyMap(first["signals"])
	for _, key := range []string{"efficacy_decay", "staleness", "regressions", "dependency_change"} {
		if !anyToBool(anyMap(signals[key])["detected"]) {
			t.Fatalf("expected %s retirement signal: %#v", key, signals[key])
		}
	}
	measurement := anyMap(first["measurement"])
	if anyToInt(measurement["network_calls"], -1) != 10 || anyToInt(measurement["model_calls"], -1) != 4 || anyToInt(measurement["execution_count"], -1) != 50 {
		t.Fatalf("missing exact observation counts: %#v", measurement)
	}
}

func TestFrontierT8SkillRetirementCandidateProtectsFalseRetirements(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		mutate func(map[string]any)
	}{
		{
			name: "seasonal skill",
			want: "seasonal_evidence_window_incomplete",
			mutate: func(payload map[string]any) {
				payload["seasonality"] = map[string]any{"seasonal": true, "full_observation_cycle": false, "season_id": "tax-season"}
			},
		},
		{
			name:   "rare high value skill",
			want:   "rare_high_value_skill",
			mutate: func(payload map[string]any) { payload["rare_high_value"] = true },
		},
		{
			name: "temporary provider failure",
			want: "temporary_provider_failure_dominates_regressions",
			mutate: func(payload map[string]any) {
				anyMap(payload["metrics"])["temporary_provider_failure_count"] = 3
			},
		},
		{
			name: "narrower replacement",
			want: "replacement_coverage_is_narrower_or_unverified",
			mutate: func(payload map[string]any) {
				anyMap(payload["replacement"])["coverage_ratio"] = 0.60
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := frontierT8TestCloneMap(t, frontierT8TestRetirementPayload())
			test.mutate(payload)
			candidate, err := frontierT8SkillRetirementCandidate(payload)
			if err != nil {
				t.Fatalf("build protected candidate: %v", err)
			}
			if anyToString(candidate["status"]) != "protected" || anyToString(candidate["recommendation"]) != "retain_pending_protection_review" {
				t.Fatalf("false retirement was not protected: %#v", candidate)
			}
			if !frontierT8TestContains(candidate["protections"], test.want) {
				t.Fatalf("protections=%#v want %q", candidate["protections"], test.want)
			}
		})
	}
}

func TestFrontierT8SkillRetirementCandidateRejectsLowUseAloneAndMutationIntent(t *testing.T) {
	payload := frontierT8TestRetirementPayload()
	metrics := anyMap(payload["metrics"])
	metrics["baseline_verified_success_rate"] = 0.95
	metrics["current_verified_success_rate"] = 0.95
	metrics["use_count"] = 0
	metrics["verified_regression_count"] = 0
	payload["last_verified_at"] = "2026-07-17T00:00:00Z"
	payload["dependency_change"] = map[string]any{"detected": false, "severity": "none"}
	payload["replacement"] = map[string]any{}
	candidate, err := frontierT8SkillRetirementCandidate(payload)
	if err != nil {
		t.Fatalf("build low-use candidate: %v", err)
	}
	if anyToString(candidate["status"]) != "insufficient_evidence" || anyToString(candidate["recommendation"]) != "retain_and_remeasure" {
		t.Fatalf("low use alone must not retire a skill: %#v", candidate)
	}

	mutating := frontierT8TestCloneMap(t, frontierT8TestRetirementPayload())
	mutating["retire"] = true
	if _, err := frontierT8SkillRetirementCandidate(mutating); err == nil || !strings.Contains(err.Error(), "mutation intent") {
		t.Fatalf("expected mutation intent rejection, got %v", err)
	}
}

func TestFrontierT8PureAdvisoryKernelHasNoExecutionSurface(t *testing.T) {
	raw, err := os.ReadFile("frontier_t8_verified_skill_evolution.go")
	if err != nil {
		t.Fatalf("read T8 kernel: %v", err)
	}
	source := string(raw)
	for _, forbidden := range []string{
		`"os/exec"`, `"net/http"`, `"os"`, "exec.Command", "time.Now(",
		"memoryStore", "writeMemory", "os.WriteFile", "os.Create", "providerClient",
	} {
		if strings.Contains(source, forbidden) {
			t.Fatalf("pure advisory kernel contains forbidden execution surface %q", forbidden)
		}
	}
}

func frontierT8TestSkillCandidatePayload() map[string]any {
	training := frontierT8TestReceiptChain("training", 3)
	holdouts := frontierT8TestReceiptChain("holdout", 3)
	return map[string]any{
		"project": "contextlattice", "name": "verified-release-gate",
		"description": "Use when a bounded release gate has independently verified receipts.",
		"as_of":       "2026-07-18T00:00:00Z", "max_verification_age_days": 30,
		"minimum_training_receipts": 3, "minimum_holdout_receipts": 3,
		"training_receipts": training, "holdout_receipts": holdouts,
	}
}

func frontierT8TestReceiptChain(partition string, count int) []any {
	rows := make([]map[string]any, 0, count)
	for index := 0; index < count; index++ {
		tag := partition + "-" + anyToString(index+1)
		command := "go test ./services/gateway-go -run ^TestVerifiedReleaseGate$ -count=1"
		row := map[string]any{
			"schema_id": "workflow_receipt.v1", "receipt_id": "receipt-" + tag,
			"workflow_id": "verified-release-gate", "partition": partition,
			"fixture_id": "fixture-" + tag, "environment_id": "environment-" + tag,
			"producer_id": "producer-" + tag, "verifier_id": "verifier-" + tag,
			"success": true, "verification_passed": true, "checks_passed": true,
			"verified_at": "2026-07-17T12:00:00Z", "verification_command": command,
			"verification_command_digest": frontierT8CommandDigest(command),
			"steps":                       []any{"Compile the candidate.", "Run the bounded release selector."},
			"checks":                      []any{"Require an exact green selector result."},
			"prerequisites":               []any{"Use a clean isolated worktree."},
			"rollback":                    []any{"Discard only the candidate branch after review."},
			"side_effects":                []any{"Writes bounded compiler and test cache entries."},
			"platform_constraints":        []any{"Go 1.26 or later on a supported platform."},
			"evidence_refs":               []any{frontierT8TestEvidence("evidence-"+tag, "producer-"+tag, "verifier-"+tag)},
			"cost": map[string]any{
				"input_tokens": 100 + index, "output_tokens": 20 + index,
				"tool_calls": 2, "provider_cost_micros": 0,
			},
			"latency_ms": 1000 + index*100, "network_calls": 0,
			"model_calls": 0, "execution_count": 1,
		}
		rows = append(rows, row)
	}
	frontierT8TestRechain(rows)
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	return out
}

func frontierT8TestEvidence(tag, producer, verifier string) map[string]any {
	digest := "sha256:" + sha256Hex("frontier-t8-"+tag)
	return map[string]any{
		"ref_id": tag, "kind": "deterministic_test", "digest": digest,
		"resolved_digest": digest, "resolved": true, "verification_passed": true,
		"producer_id": producer, "verifier_id": verifier, "verification_id": "verification-" + tag,
	}
}

func frontierT8TestRechain(rows []map[string]any) {
	previous := "genesis"
	for _, row := range rows {
		row["previous_receipt_digest"] = previous
		row["receipt_digest"] = frontierT8ReceiptDigest(row)
		previous = anyToString(row["receipt_digest"])
	}
}

func frontierT8TestReceiptRows(payload map[string]any, key string) []map[string]any {
	items := contextPackAnyList(payload[key])
	rows := make([]map[string]any, 0, len(items))
	for _, item := range items {
		rows = append(rows, anyMap(item))
	}
	return rows
}

func frontierT8TestRetirementPayload() map[string]any {
	return map[string]any{
		"project": "contextlattice", "skill_id": "skill-release-gate", "name": "release-gate", "skill_version": 3,
		"as_of": "2026-07-18T00:00:00Z", "last_verified_at": "2026-03-01T00:00:00Z",
		"review_window": map[string]any{"start_at": "2026-07-01T00:00:00Z", "end_at": "2026-08-01T00:00:00Z"},
		"metrics": map[string]any{
			"baseline_verified_success_rate": 0.95, "current_verified_success_rate": 0.60,
			"baseline_sample_count": 50, "current_sample_count": 50, "use_count": 12,
			"verified_regression_count": 5, "temporary_provider_failure_count": 0,
			"network_calls": 10, "model_calls": 4, "execution_count": 50,
			"total_cost_micros": 12345, "total_latency_ms": 100000,
		},
		"evidence_refs":   []any{frontierT8TestEvidence("retirement-window", "skill-telemetry", "retirement-reviewer")},
		"security_change": map[string]any{"detected": false, "severity": "none"},
		"dependency_change": map[string]any{
			"detected": true, "severity": "high", "summary": "A required dependency is unsupported.",
			"evidence_refs": []any{frontierT8TestEvidence("dependency-change", "dependency-inventory", "dependency-reviewer")},
		},
		"replacement": map[string]any{
			"present": true, "skill_id": "skill-release-gate-v4", "verified": true,
			"coverage_ratio": 1.0, "coverage_basis": "All verified task classes in the review window.",
			"evidence_refs": []any{frontierT8TestEvidence("replacement-coverage", "replacement-evaluator", "replacement-reviewer")},
		},
		"impact": map[string]any{
			"severity": "high", "summary": "Failed release gates delay verified delivery.",
			"affected_workflows": 4, "value_per_use_micros": 500000, "user_visible": true,
		},
		"seasonality":     map[string]any{"seasonal": false, "full_observation_cycle": true, "season_id": "continuous"},
		"rare_high_value": false,
	}
}

func frontierT8TestCloneMap(t *testing.T, input map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal clone: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatalf("unmarshal clone: %v", err)
	}
	return output
}

func frontierT8TestJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}

func frontierT8TestContains(raw any, want string) bool {
	for _, item := range contextPackAnyList(raw) {
		if anyToString(item) == want {
			return true
		}
	}
	return false
}
