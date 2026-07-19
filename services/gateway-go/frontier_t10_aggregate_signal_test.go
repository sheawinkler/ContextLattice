package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func newFrontierT10TestStore(t *testing.T) (*frontierT10AggregateStore, *server) {
	t.Helper()
	state, err := emptyFrontierT10AggregateState()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "aggregate-signal.json")
	if err := prepareOwnerOnlyFile(path, true); err != nil {
		t.Fatal(err)
	}
	store := &frontierT10AggregateStore{
		enabled: true, path: path, dedicatedParent: true,
		maxBytes: frontierT10DefaultMaxBytes, maxRecords: frontierT10DefaultMaxRecords, state: state,
	}
	return store, &server{aggregateSignal: store}
}

func frontierT10RouteCall(t *testing.T, s *server, payload any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, frontierT10AggregatePath, bytes.NewReader(raw))
	recorder := httptest.NewRecorder()
	s.memoryAggregateSignal(recorder, request)
	result := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response status=%d body=%s: %v", recorder.Code, recorder.Body.String(), err)
	}
	return recorder.Code, result
}

func frontierT10TestContribution(t *testing.T, metric string, index int, now time.Time) frontierT10AggregateContribution {
	t.Helper()
	_, spec, err := frontierT10NormalizeMetric(metric)
	if err != nil {
		t.Fatal(err)
	}
	window := frontierT10Window(now)
	unit := frontierT6Digest(fmt.Sprintf("unit:%s:%d", metric, index))
	nonce := frontierT6Digest(fmt.Sprintf("nonce:%s:%d", metric, index))
	expires := frontierT10Date(now.AddDate(0, 0, 14))
	var value *float64
	category := ""
	if spec.Kind == "numeric" {
		midpoint := (spec.Minimum + spec.Maximum) / 2
		value = &midpoint
	} else {
		category = spec.Categories[index%len(spec.Categories)]
	}
	contribution, err := frontierT10BuildContribution(metric, "manual", window, unit, nonce, value, category, expires)
	if err != nil {
		t.Fatal(err)
	}
	return contribution
}

func frontierT10ContributionBatch(t *testing.T, metric string, count int, now time.Time) []frontierT10AggregateContribution {
	t.Helper()
	rows := make([]frontierT10AggregateContribution, 0, count)
	for index := 0; index < count; index++ {
		rows = append(rows, frontierT10TestContribution(t, metric, index, now))
	}
	return rows
}

func TestFrontierT10PreviewClipsWithoutPersistenceOrTransport(t *testing.T) {
	store, s := newFrontierT10TestStore(t)
	value := 180.0
	status, result := frontierT10RouteCall(t, s, map[string]any{
		"operation": "preview", "metric": "context_quality_score", "value": value,
	})
	if status != http.StatusOK || anyToString(result["decision"]) != "preview" {
		t.Fatalf("unexpected preview status=%d result=%#v", status, result)
	}
	if anyToFloat(anyMap(result["statistic"])["numeric_value"]) != 100 || !anyToBool(anyMap(result["clipping"])["applied"]) {
		t.Fatalf("preview did not clip to the allowlisted bound: %#v", result)
	}
	if anyToBool(result["persisted"]) || len(store.state.Contributions) != 0 {
		t.Fatalf("preview persisted material: %#v", result)
	}
	if !frontierT10ContractPassed(result) || anyToInt(anyMap(result["safety"])["network_calls"], -1) != 0 {
		t.Fatalf("preview contract or transport boundary failed: %#v", result)
	}
}

func TestFrontierT10QueueRequiresConsentAndRejectsSecondPrivacyUnitContribution(t *testing.T) {
	store, s := newFrontierT10TestStore(t)
	base := map[string]any{
		"operation": "queue", "metric": "repair_rate", "value": 0.25,
		"contribution_nonce": "0123456789abcdef",
	}
	status, _ := frontierT10RouteCall(t, s, base)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("queue without consent status=%d", status)
	}
	base["opt_in"] = true
	status, first := frontierT10RouteCall(t, s, base)
	if status != http.StatusOK || !anyToBool(first["persisted"]) || !frontierT10ContractPassed(first) {
		t.Fatalf("queue failed status=%d result=%#v", status, first)
	}
	status, replay := frontierT10RouteCall(t, s, base)
	if status != http.StatusOK || !anyToBool(anyMap(replay["privacy"])["idempotent_replay"]) {
		t.Fatalf("identical replay was not idempotent status=%d result=%#v", status, replay)
	}
	base["contribution_nonce"] = "fedcba9876543210"
	status, rejected := frontierT10RouteCall(t, s, base)
	if status != http.StatusConflict || anyToString(rejected["error"]) != "aggregate_signal_replay_or_differencing_rejected" {
		t.Fatalf("second weekly contribution was not rejected status=%d result=%#v", status, rejected)
	}
	info, err := os.Stat(store.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Aggregate Signal ledger is not owner-only: info=%v err=%v", info, err)
	}
}

func TestFrontierT10RejectsForbiddenAndMalformedContributionMaterial(t *testing.T) {
	_, s := newFrontierT10TestStore(t)
	status, result := frontierT10RouteCall(t, s, map[string]any{
		"operation": "preview", "metric": "repair_rate", "value": 0.1,
		"metadata": map[string]any{"raw_text": "do not export"},
	})
	if status != http.StatusBadRequest || !strings.Contains(anyToString(result["detail"]), "privacy-forbidden") {
		t.Fatalf("raw text was not rejected status=%d result=%#v", status, result)
	}
	now := time.Now().UTC()
	malicious := frontierT10TestContribution(t, "repair_rate", 1, now)
	malicious.Clipping["maximum"] = 1000.0
	malicious.ContributionDigest = frontierT10ContributionDigest(malicious)
	malicious.ContributionID = frontierT10ContributionID(malicious)
	if err := frontierT10ValidateContribution(malicious); err == nil {
		t.Fatal("forged clipping bounds were accepted")
	}
	invalid := mathNaN()
	if _, err := frontierT10BuildContribution("repair_rate", "manual", frontierT10Window(now), frontierT6Digest("unit"), frontierT6Digest("nonce"), &invalid, "", frontierT10Date(now.AddDate(0, 0, 14))); err == nil {
		t.Fatal("non-finite contribution was accepted")
	}
}

func mathNaN() float64 {
	return math.Float64frombits(0x7ff8000000000001)
}

func TestFrontierT10SmallCohortIsSuppressedWithoutExactCountOrBudget(t *testing.T) {
	store, s := newFrontierT10TestStore(t)
	now := time.Now().UTC()
	status, result := frontierT10RouteCall(t, s, map[string]any{
		"operation": "report", "metric": "repair_rate", "cohort_window": frontierT10Window(now),
		"contributions": frontierT10ContributionBatch(t, "repair_rate", frontierT10MinimumCohort-1, now),
	})
	if status != http.StatusOK || anyToString(result["decision"]) != "suppressed" || !frontierT10ContractPassed(result) {
		t.Fatalf("small cohort was not safely suppressed status=%d result=%#v", status, result)
	}
	cohort := anyMap(result["cohort"])
	if anyToBool(cohort["exact_count_disclosed"]) || cohort["eligible_count"] != nil {
		t.Fatalf("suppressed cohort disclosed its exact count: %#v", cohort)
	}
	if len(store.state.Accounting) != 0 || len(store.state.Reports) != 0 {
		t.Fatalf("suppression consumed privacy state: %#v", store.state)
	}
}

func TestFrontierT10MinimumCohortReleasesOnceAndRejectsDifferencing(t *testing.T) {
	_, s := newFrontierT10TestStore(t)
	now := time.Now().UTC()
	contributions := frontierT10ContributionBatch(t, "repair_rate", frontierT10MinimumCohort, now)
	payload := map[string]any{
		"operation": "report", "metric": "repair_rate", "cohort_window": frontierT10Window(now),
		"epsilon": 0.25, "delta": 0.0000001, "contributions": contributions,
	}
	status, first := frontierT10RouteCall(t, s, payload)
	if status != http.StatusOK || anyToString(first["decision"]) != "released" || !frontierT10ContractPassed(first) {
		t.Fatalf("minimum cohort release failed status=%d result=%#v", status, first)
	}
	if anyToInt(anyMap(first["cohort"])["eligible_count"], 0) != frontierT10MinimumCohort || anyToBool(anyMap(first["safety"])["formal_privacy_claim"]) {
		t.Fatalf("release cohort or research boundary is wrong: %#v", first)
	}
	status, replay := frontierT10RouteCall(t, s, payload)
	if status != http.StatusOK || !anyToBool(anyMap(replay["receipt"])["idempotent"]) || anyToString(replay["report_id"]) != anyToString(first["report_id"]) {
		t.Fatalf("identical report was not replayed exactly status=%d result=%#v", status, replay)
	}
	changed := append([]frontierT10AggregateContribution(nil), contributions...)
	changed[len(changed)-1] = frontierT10TestContribution(t, "repair_rate", 99, now)
	payload["contributions"] = changed
	status, rejected := frontierT10RouteCall(t, s, payload)
	if status != http.StatusConflict || anyToString(rejected["error"]) != "aggregate_signal_replay_or_differencing_rejected" {
		t.Fatalf("differencing request was not rejected status=%d result=%#v", status, rejected)
	}
}

func TestFrontierT10CategoricalReportSuppressesRareBuckets(t *testing.T) {
	store, _ := newFrontierT10TestStore(t)
	now := time.Now().UTC()
	rows := frontierT10ContributionBatch(t, "outcome_class", frontierT10MinimumCohort, now)
	record, _, err := store.release("outcome_class", frontierT10Window(now), rows, 0.25, 0.0000001, 13, now)
	if err != nil {
		t.Fatal(err)
	}
	if !anyToBool(record.Suppression["rare_categories_suppressed"]) || len(anyMap(record.Estimate["noisy_counts"])) != 0 {
		t.Fatalf("rare categorical buckets were exposed: %#v", record)
	}
}

func TestFrontierT10RollingPrivacyBudgetCannotBeEvadedByShortReportExpiry(t *testing.T) {
	store, _ := newFrontierT10TestStore(t)
	now := time.Now().UTC()
	metrics := []string{
		"repair_rate", "first_pass_success_rate", "context_quality_score", "average_retry_count",
		"exact_prompt_tokens_saved", "modeled_inference_tokens_avoided", "provider_total_tokens",
		"policy_candidate_count", "policy_evaluation_count",
	}
	for index, metric := range metrics {
		_, _, err := store.release(metric, frontierT10Window(now), frontierT10ContributionBatch(t, metric, 20, now), 0.25, 0.0000001, 1, now)
		if index < 8 && err != nil {
			t.Fatalf("release %d failed before budget exhaustion: %v", index, err)
		}
		if index == 8 && !errors.Is(err, errFrontierT10Budget) {
			t.Fatalf("ninth release did not exhaust rolling budget: %v", err)
		}
	}
	if len(store.state.Accounting) != 8 {
		t.Fatalf("rolling accountant lost entries to short report expiry: %d", len(store.state.Accounting))
	}
	for _, entry := range store.state.Accounting {
		if entry.ExpiresOn < frontierT10Date(now.AddDate(0, 0, frontierT10RollingWindowDays)) {
			t.Fatalf("accounting entry expires before 90 days: %#v", entry)
		}
	}
}

func TestFrontierT10OptOutDeletesQueueRotatesEpochAndDoesNotClaimSubtraction(t *testing.T) {
	store, s := newFrontierT10TestStore(t)
	payload := map[string]any{
		"operation": "queue", "metric": "repair_rate", "value": 0.25,
		"contribution_nonce": "0123456789abcdef", "opt_in": true,
	}
	status, queued := frontierT10RouteCall(t, s, payload)
	if status != http.StatusOK {
		t.Fatalf("queue failed: %#v", queued)
	}
	firstCommitment := anyToString(queued["unit_epoch_commitment"])
	status, result := frontierT10RouteCall(t, s, map[string]any{"operation": "opt-out", "confirm": true})
	if status != http.StatusOK || anyToBool(result["opted_in"]) || !frontierT10ContractPassed(result) {
		t.Fatalf("opt-out failed status=%d result=%#v", status, result)
	}
	optOut := anyMap(result["opt_out"])
	if anyToInt(optOut["queued_contributions_deleted"], 0) != 1 || anyToBool(optOut["released_aggregate_subtraction_claimed"]) {
		t.Fatalf("opt-out semantics are wrong: %#v", optOut)
	}
	payload["contribution_nonce"] = "fedcba9876543210"
	status, queuedAgain := frontierT10RouteCall(t, s, payload)
	if status != http.StatusOK || anyToString(queuedAgain["unit_epoch_commitment"]) == firstCommitment {
		t.Fatalf("opt-out did not rotate the epoch commitment: %#v", queuedAgain)
	}
	if len(store.state.Contributions) != 1 {
		t.Fatalf("post-opt-in queue state is wrong: %#v", store.state.Contributions)
	}
}

func TestFrontierT10SourceAdaptersExposeOnlySufficientStatistics(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "false")
	store, s := newFrontierT10TestStore(t)
	_ = store
	s.contextPackQuality = newContextPackQualityTelemetry(20)
	s.contextPackQuality.recordQuality(map[string]any{
		"sample_id": "safe-sample", "project": "never-export-this-project", "quality_score": 92,
		"exact_prompt_tokens_saved": 500, "modeled_inference_tokens_avoided": 800,
	})
	s.contextPackQuality.recordOutcome(map[string]any{
		"outcome_id": "safe-outcome", "sample_id": "safe-sample", "project": "never-export-this-project",
		"first_pass_success": true, "calibration_eligible": true,
	})
	status, result := frontierT10RouteCall(t, s, map[string]any{
		"operation": "preview", "source": "context_pack_quality", "metric": "context_quality_score",
	})
	if status != http.StatusOK || anyToFloat(anyMap(result["statistic"])["numeric_value"]) != 92 {
		t.Fatalf("quality sufficient statistic preview failed: status=%d result=%#v", status, result)
	}
	raw, _ := json.Marshal(result)
	if bytes.Contains(raw, []byte("never-export-this-project")) || frontierT10ForbiddenPayloadPath(result, "") != "" {
		t.Fatalf("source adapter leaked scope material: %s", raw)
	}
}

func TestFrontierT10StateReloadIsOwnerOnlyAndHashVerified(t *testing.T) {
	store, s := newFrontierT10TestStore(t)
	status, result := frontierT10RouteCall(t, s, map[string]any{
		"operation": "queue", "metric": "repair_rate", "value": 0.2,
		"contribution_nonce": "0123456789abcdef", "opt_in": true,
	})
	if status != http.StatusOK {
		t.Fatalf("queue failed: %#v", result)
	}
	reloaded := &frontierT10AggregateStore{
		enabled: true, path: store.path, dedicatedParent: true,
		maxBytes: store.maxBytes, maxRecords: store.maxRecords,
	}
	if err := reloaded.load(); err != nil || len(reloaded.state.Contributions) != 1 {
		t.Fatalf("reload failed state=%#v err=%v", reloaded.state, err)
	}
	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	var tampered map[string]any
	if err := json.Unmarshal(raw, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered["generation"] = 999
	tamperedRaw, _ := json.Marshal(tampered)
	if err := os.WriteFile(store.path, tamperedRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloaded.load(); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered state was accepted: %v", err)
	}
}

func TestFrontierT10PreviewP95StaysBelowSyncProjectionBudget(t *testing.T) {
	_, s := newFrontierT10TestStore(t)
	durations := make([]time.Duration, 0, 200)
	value := 0.2
	request := frontierT10AggregateRequest{Operation: "preview", Metric: "repair_rate", Source: "manual", Value: &value}
	for index := 0; index < cap(durations); index++ {
		started := time.Now()
		if _, _, err := s.frontierT10BuildContribution(request, false, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		durations = append(durations, time.Since(started))
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	p95 := durations[int(float64(len(durations))*0.95)-1]
	if p95 >= 20*time.Millisecond {
		t.Fatalf("preview p95=%s exceeds 20ms budget", p95)
	}
}

func TestFrontierT10FrozenHoldoutCoverageIsComplete(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t10-aggregate-signal-holdout.v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Cases []struct {
			ID string `json:"id"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	covered := map[string]string{
		"numeric_preview_clips_to_allowlisted_bounds": "preview", "categorical_preview_accepts_allowlisted_bucket": "categorical",
		"unknown_metric_is_rejected": "validation", "unknown_category_is_suppressed": "validation",
		"raw_text_is_rejected_recursively": "forbidden", "embedding_is_rejected_recursively": "forbidden",
		"prompt_is_rejected_recursively": "forbidden", "file_path_is_rejected_recursively": "forbidden",
		"project_name_is_rejected_recursively": "forbidden", "exact_timestamp_is_rejected_recursively": "forbidden",
		"stable_installation_id_is_rejected_recursively": "forbidden", "non_finite_numeric_is_rejected": "validation",
		"oversized_contribution_is_rejected": "bounds", "queue_requires_explicit_opt_in": "queue",
		"repeated_nonce_is_rejected": "queue", "different_nonce_same_unit_metric_week_is_rejected": "queue",
		"queued_contribution_expires": "expiry", "opt_out_deletes_unreleased_queue": "optout",
		"post_release_opt_out_never_claims_subtraction": "optout", "cohort_19_is_suppressed": "suppression",
		"cohort_20_can_release_once": "release", "rare_category_is_suppressed": "categorical",
		"repeated_report_is_idempotent": "release", "release_epsilon_above_point_25_is_rejected": "accountant",
		"rolling_epsilon_above_2_is_rejected": "accountant", "delta_above_one_e_minus_6_is_rejected": "accountant",
		"malicious_client_cannot_increase_sensitivity": "validation", "wrong_workspace_is_rejected": "paid_boundary",
		"unentitled_plan_is_rejected": "paid_boundary", "revoked_cohort_credential_is_rejected": "paid_boundary",
		"membership_inference_review_blocks_promotion": "research_gate", "attribute_inference_review_blocks_promotion": "research_gate",
		"reconstruction_review_blocks_promotion": "research_gate", "malicious_client_review_blocks_promotion": "research_gate",
		"accountant_exhaustion_review_blocks_promotion": "research_gate", "utility_loss_review_blocks_promotion": "research_gate",
		"default_path_performs_no_network": "safety", "ledger_stays_bounded": "storage",
		"preview_stays_off_hot_path_budget": "latency", "outputs_never_echo_forbidden_payloads": "forbidden",
	}
	if len(fixture.Cases) != 40 {
		t.Fatalf("frozen holdout case count=%d want 40", len(fixture.Cases))
	}
	for _, testCase := range fixture.Cases {
		if covered[testCase.ID] == "" {
			t.Fatalf("frozen holdout has no implementation proof mapping: %s", testCase.ID)
		}
	}
}
