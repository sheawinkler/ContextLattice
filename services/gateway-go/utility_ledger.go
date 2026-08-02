package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	utilityObservationContractID = "utility_observation.v1"
	utilityLedgerContractID      = "utility_ledger.v1"
	utilityAnalyticsContractID   = "utility_analytics.v1"
	utilityPolicyContractID      = "utility_policy_evaluation.v1"

	utilityTelemetryPath = "/telemetry/utility"
	utilityAnalyticsPath = "/telemetry/utility/analytics"
	utilityPolicyPath    = "/telemetry/utility/policy/evaluate"
)

var utilityVerifierKinds = map[string]struct{}{
	"artifact_verifier":  {},
	"contract_validator": {},
	"deterministic_test": {},
	"external_evaluator": {},
	"human_review":       {},
}

var utilityExactMatchingMethods = map[string]struct{}{
	"exact_holdout":      {},
	"paired_replay":      {},
	"randomized_control": {},
}

var errUtilityOutcomeConflict = errors.New("utility outcome id conflicts with an existing source claim")
var errUtilityPersistenceUnavailable = errors.New("utility ledger persistence is unavailable")
var errUtilityObservationUnavailable = errors.New("utility observation is unavailable for reconciliation")

type utilityTelemetry struct {
	mu           sync.Mutex
	limit        int
	store        *utilityLedgerStore
	observations []map[string]any
	byOutcome    map[string]int
}

type utilityLedgerStore struct {
	mu           sync.Mutex
	configured   bool
	enabled      bool
	path         string
	maxBytes     int64
	maxSamples   int
	parseErrors  int
	writeErrors  int
	loadedRows   int
	physicalRows int
	latest       map[string]map[string]any
	writeFile    func(string, []byte, bool) error
	unlock       func()
	lastWriteAt  string
	lastError    string
}

type utilityQuery struct {
	Project     string
	TaskClass   string
	UtilityUnit string
	From        time.Time
	To          time.Time
	Limit       int
}

type utilityPairMetric struct {
	PairID             string
	TaskClass          string
	UtilityUnit        string
	ControlOutcomeID   string
	TreatmentOutcomeID string
	UtilityGain        float64
	ModelVisibleTokens int
	GainPer1K          float64
	CapturedAt         string
}

func newUtilityTelemetry(limit int) *utilityTelemetry {
	if limit <= 0 {
		limit = 500
	}
	t := &utilityTelemetry{
		limit:        limit,
		store:        newUtilityLedgerStoreFromEnv(),
		observations: make([]map[string]any, 0, limit),
		byOutcome:    map[string]int{},
	}
	t.loadPersistedRows()
	return t
}

func newUtilityLedgerStoreFromEnv() *utilityLedgerStore {
	enabled := envBool("GO_UTILITY_LEDGER_ENABLED", true)
	path := utilityLedgerPath()
	if strings.TrimSpace(path) == "" {
		enabled = false
	}
	store := &utilityLedgerStore{
		configured: enabled,
		enabled:    enabled,
		path:       path,
		maxBytes:   int64(clampInt(envInt("GO_UTILITY_LEDGER_MAX_BYTES", 4*1024*1024), 64*1024, 128*1024*1024)),
		maxSamples: clampInt(envInt("GO_UTILITY_LEDGER_MAX_SAMPLES", 5000), 20, 100000),
		latest:     map[string]map[string]any{},
		writeFile:  writeOwnerOnlyDurableAtomicFile,
	}
	if enabled {
		dedicatedParent := strings.TrimSpace(os.Getenv("GO_UTILITY_LEDGER_PATH")) == ""
		if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
			store.enabled = false
			store.lastError = utilityLedgerErrorCode(err)
		} else if unlock, err := lockOwnerOnlyMigration(path + ".lock"); err != nil {
			store.enabled = false
			store.lastError = utilityLedgerErrorCode(err)
		} else {
			store.unlock = unlock
		}
	}
	return store
}

func utilityLedgerPath() string {
	return resolveStoragePath("GO_UTILITY_LEDGER_PATH", filepath.Join(".data", "orchestrator", "utility_ledger.ndjson"))
}

func (t *utilityTelemetry) loadPersistedRows() {
	if t == nil || t.store == nil || !t.store.enabled {
		return
	}
	rows, parseErrors, err := t.store.readRows()
	if err != nil {
		t.store.setError(err)
		return
	}
	t.mu.Lock()
	for _, row := range rows {
		t.applyLocked(row)
	}
	t.mu.Unlock()
	t.store.mu.Lock()
	t.store.loadedRows = len(rows)
	t.store.parseErrors = parseErrors
	t.store.mu.Unlock()
}

func (t *utilityTelemetry) applyLocked(row map[string]any) bool {
	if t == nil || len(row) == 0 || anyToString(row["schema_id"]) != utilityObservationContractID {
		return false
	}
	outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
	if outcomeID == "" {
		return false
	}
	row = cloneAnyMap(row)
	if index, exists := t.byOutcome[outcomeID]; exists && index >= 0 && index < len(t.observations) {
		if anyToInt(row["revision"], 1) < anyToInt(t.observations[index]["revision"], 1) {
			return false
		}
		t.observations[index] = row
		return false
	}
	t.byOutcome[outcomeID] = len(t.observations)
	t.observations = append(t.observations, row)
	if len(t.observations) > t.limit {
		t.observations = append([]map[string]any{}, t.observations[len(t.observations)-t.limit:]...)
		t.reindexLocked()
	}
	return true
}

func (t *utilityTelemetry) reindexLocked() {
	t.byOutcome = make(map[string]int, len(t.observations))
	for index, row := range t.observations {
		if outcomeID := strings.TrimSpace(anyToString(row["outcome_id"])); outcomeID != "" {
			t.byOutcome[outcomeID] = index
		}
	}
}

func (t *utilityTelemetry) record(row map[string]any) (map[string]any, bool, error) {
	if t == nil || len(row) == 0 {
		return nil, false, nil
	}
	if t.store != nil {
		configured, enabled := t.store.availability()
		if !configured {
			return nil, false, nil
		}
		if !enabled {
			return nil, false, errUtilityPersistenceUnavailable
		}
	}
	outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
	t.mu.Lock()
	defer t.mu.Unlock()
	if index, exists := t.byOutcome[outcomeID]; exists && index >= 0 && index < len(t.observations) {
		existing := cloneAnyMap(t.observations[index])
		existingDigest := strings.TrimSpace(anyToString(existing["source_claim_digest"]))
		candidateDigest := strings.TrimSpace(anyToString(row["source_claim_digest"]))
		if !utilitySHA256DigestValid(existingDigest) || !utilitySHA256DigestValid(candidateDigest) || existingDigest != candidateDigest {
			return existing, false, errUtilityOutcomeConflict
		}
		return existing, false, nil
	}
	if t.store != nil {
		if existing, exists := t.store.observation(outcomeID); exists {
			existingDigest := strings.TrimSpace(anyToString(existing["source_claim_digest"]))
			candidateDigest := strings.TrimSpace(anyToString(row["source_claim_digest"]))
			if !utilitySHA256DigestValid(existingDigest) || !utilitySHA256DigestValid(candidateDigest) || existingDigest != candidateDigest {
				return existing, false, errUtilityOutcomeConflict
			}
			return existing, false, nil
		}
		persisted, wrote, err := t.store.append(row)
		if err != nil {
			t.store.setError(err)
			if errors.Is(err, errUtilityOutcomeConflict) {
				return nil, false, errUtilityOutcomeConflict
			}
			return nil, false, errUtilityPersistenceUnavailable
		}
		if len(persisted) == 0 {
			return nil, false, errUtilityPersistenceUnavailable
		}
		t.applyLocked(persisted)
		return cloneAnyMap(persisted), wrote, nil
	}
	recorded := t.applyLocked(row)
	stored := cloneAnyMap(row)
	return stored, recorded, nil
}

func (t *utilityTelemetry) update(row map[string]any) (map[string]any, error) {
	if t == nil || len(row) == 0 {
		return nil, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
	index := -1
	storedIndex, exists := t.byOutcome[outcomeID]
	base := map[string]any{}
	if exists && storedIndex >= 0 && storedIndex < len(t.observations) {
		index = storedIndex
		base = t.observations[storedIndex]
	} else if t.store != nil {
		base, exists = t.store.observation(outcomeID)
	}
	if !exists || len(base) == 0 {
		return nil, errUtilityObservationUnavailable
	}
	row = cloneAnyMap(row)
	row["revision"] = anyToInt(base["revision"], 1) + 1
	row["updated_at"] = nowUTCISO()
	row["observation_digest"] = utilityObservationDigest(row)
	if t.store != nil {
		configured, enabled := t.store.availability()
		if !configured || !enabled {
			return nil, errUtilityPersistenceUnavailable
		}
		persisted, _, err := t.store.append(row)
		if err != nil {
			t.store.setError(err)
			return nil, errUtilityPersistenceUnavailable
		}
		if len(persisted) == 0 {
			return nil, errUtilityPersistenceUnavailable
		}
		row = persisted
	}
	if index >= 0 && index < len(t.observations) {
		t.observations[index] = row
	} else {
		t.applyLocked(row)
	}
	return cloneAnyMap(row), nil
}

func (t *utilityTelemetry) observation(outcomeID string) (map[string]any, bool) {
	if t == nil {
		return nil, false
	}
	t.mu.Lock()
	index, ok := t.byOutcome[strings.TrimSpace(outcomeID)]
	if ok && index >= 0 && index < len(t.observations) {
		row := cloneAnyMap(t.observations[index])
		t.mu.Unlock()
		return row, true
	}
	t.mu.Unlock()
	if t.store != nil {
		return t.store.observation(outcomeID)
	}
	return nil, false
}

func normalizeUtilityOutcomeClaim(sample map[string]any) map[string]any {
	source := anyMap(firstNonEmptyAny(sample["utility"], sample["verified_utility"]))
	out := map[string]any{}
	copyString := func(key string, limit int, values ...any) {
		if value := strings.TrimSpace(anyToString(firstPresentAny(values...))); value != "" {
			out[key] = clipText(value, limit)
		}
	}
	if value, ok := utilityNumberPresent(source, sample, "value", "utility_value", "verified_utility_value"); ok {
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > 1_000_000 {
			out["value_invalid"] = true
		} else {
			out["value"] = utilityRound(value, 6)
		}
	}
	copyString("unit", 80, source["unit"], sample["utility_unit"])
	copyString("verification_event_id", 128, source["verification_event_id"], sample["verification_event_id"])
	copyString("evidence_digest", 96, source["evidence_digest"], sample["verification_evidence_digest"], sample["evidence_digest"])
	copyString("verifier_kind", 80, source["verifier_kind"], sample["verifier_kind"])
	copyString("verifier_id", 160, source["verifier_id"], sample["verifier_id"])
	if raw, present := utilityFirstPresent(source, sample, "verification_passed"); present {
		out["verification_passed"] = anyToBool(raw)
	}
	out["independently_verified"] = false
	out["verification_status"] = "unverified"
	return out
}

func normalizeUtilityEconomics(sample map[string]any) map[string]any {
	source := anyMap(sample["economics"])
	out := map[string]any{}
	for _, field := range []struct {
		key     string
		aliases []string
		max     int
	}{
		{key: "latency_ms", aliases: []string{"latency_ms", "duration_ms"}, max: 86_400_000},
		{key: "cost_microusd", aliases: []string{"cost_microusd"}, max: 2_000_000_000},
		{key: "tool_calls", aliases: []string{"tool_calls", "tool_call_count"}, max: 1_000_000},
		{key: "failures", aliases: []string{"failures", "failure_count"}, max: 1_000_000},
	} {
		values := make([]any, 0, len(field.aliases)*2)
		for _, alias := range field.aliases {
			values = append(values, source[alias], sample[alias])
		}
		if raw, present := firstPresentValue(values...); present {
			out[field.key] = clampInt(anyToInt(raw, 0), 0, field.max)
		}
	}
	return out
}

func normalizeUtilityPairing(sample map[string]any) map[string]any {
	source := anyMap(firstNonEmptyAny(sample["pairing"], sample["matched_control"]))
	out := map[string]any{}
	copyString := func(key string, limit int, values ...any) {
		if value := strings.TrimSpace(anyToString(firstPresentAny(values...))); value != "" {
			out[key] = clipText(value, limit)
		}
	}
	copyString("pair_id", 160, source["pair_id"], sample["pair_id"])
	arm := strings.ToLower(strings.TrimSpace(anyToString(firstPresentAny(source["arm"], sample["pair_arm"], sample["arm"]))))
	if arm == "candidate" || arm == "canary" || arm == "treatment" {
		arm = "treatment"
	}
	if arm == "control" || arm == "treatment" {
		out["arm"] = arm
	}
	copyString("matched_control_outcome_id", 200, source["matched_control_outcome_id"], sample["matched_control_outcome_id"])
	copyString("task_match_digest", 96, source["task_match_digest"], sample["task_match_digest"])
	copyString("matching_method", 80, source["matching_method"], sample["matching_method"])
	copyString("experiment_id", 160, source["experiment_id"], sample["experiment_id"])
	copyString("assignment_digest", 96, source["assignment_digest"], sample["assignment_digest"])
	copyString("model", 160, source["model"], sample["pair_model"])
	copyString("runner", 160, source["runner"], sample["pair_runner"])
	copyString("harness", 160, source["harness"], sample["pair_harness"])
	copyString("context_reconstruction_contract", 160, source["context_reconstruction_contract"], sample["context_reconstruction_contract"])
	if raw, present := utilityFirstPresent(source, sample, "leakage_free"); present {
		out["leakage_free"] = anyToBool(raw)
	}
	return out
}

func utilityFirstPresent(nested, root map[string]any, key string) (any, bool) {
	return firstPresentValue(nested[key], root[key])
}

func utilityNumberPresent(nested, root map[string]any, keys ...string) (float64, bool) {
	values := make([]any, 0, len(keys)*2)
	for _, key := range keys {
		values = append(values, nested[key], root[key])
	}
	raw, present := firstPresentValue(values...)
	if !present {
		return 0, false
	}
	return anyToFloat(raw), true
}

func firstPresentValue(values ...any) (any, bool) {
	for _, value := range values {
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		return value, true
	}
	return nil, false
}

func utilityRound(value float64, places int) float64 {
	pow := math.Pow10(maxInt(0, places))
	return math.Round(value*pow) / pow
}

func utilitySHA256DigestValid(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func utilityObservationDigest(row map[string]any) string {
	copyRow := cloneAnyMap(row)
	delete(copyRow, "observation_digest")
	raw, err := json.Marshal(copyRow)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

func utilitySourceClaimDigest(outcome map[string]any) string {
	claim := cloneAnyMap(outcome)
	for _, field := range []string{"capturedAt", "captured_at", "updated_at", "revision", "observation_digest", "source_claim_digest"} {
		delete(claim, field)
	}
	raw, err := json.Marshal(claim)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(raw))
}

func utilityIdentityMismatch(source, candidate map[string]any) bool {
	for _, key := range []string{"sample_id", "session_id", "task_id", "task_identity_id", "execution_lane_id", "project", "agent_id"} {
		left := strings.TrimSpace(anyToString(source[key]))
		right := strings.TrimSpace(anyToString(candidate[key]))
		if left == "" || right == "" {
			continue
		}
		if key == "project" {
			if !strings.EqualFold(left, right) {
				return true
			}
			continue
		}
		if left != right {
			return true
		}
	}
	return false
}

func utilityExactJoinExclusions(outcome, quality, impact map[string]any) []string {
	reasons := []string{}
	for _, key := range []string{"sample_id", "session_id", "project", "agent_id"} {
		for _, source := range []map[string]any{outcome, quality, impact} {
			if strings.TrimSpace(anyToString(source[key])) == "" {
				reasons = append(reasons, "source_join_key_missing_"+key)
				break
			}
		}
	}
	return utilityUniqueStrings(reasons)
}

func utilityVerificationEvent(events []map[string]any, eventID string) map[string]any {
	eventID = strings.TrimSpace(eventID)
	if eventID == "" {
		return nil
	}
	for _, event := range events {
		if anyToString(event["id"]) == eventID {
			return cloneAnyMap(event)
		}
	}
	return nil
}

func utilityVerifyClaim(row map[string]any, event map[string]any) (map[string]any, string) {
	claim := cloneAnyMap(anyMap(row["utility"]))
	claim["independently_verified"] = false
	claim["verification_status"] = "unverified"
	eventID := strings.TrimSpace(anyToString(claim["verification_event_id"]))
	if eventID == "" {
		return claim, "verification_event_not_declared"
	}
	if len(event) == 0 {
		return claim, "verification_event_not_found"
	}
	if anyToString(event["id"]) != eventID || proofTimelineStageForEvent(anyToString(event["type"])) != "verification" {
		return claim, "verification_event_stage_mismatch"
	}
	metadata := anyMap(event["metadata"])
	proof := anyMap(firstNonEmptyAny(metadata["utility_verification"], metadata["utilityVerification"]))
	if len(proof) == 0 {
		return claim, "verification_evidence_missing"
	}
	if outcomeID := strings.TrimSpace(anyToString(proof["outcome_id"])); outcomeID == "" || outcomeID != anyToString(row["outcome_id"]) {
		return claim, "verification_outcome_mismatch"
	}
	if sampleID := strings.TrimSpace(anyToString(proof["sample_id"])); sampleID == "" || sampleID != anyToString(row["sample_id"]) {
		return claim, "verification_sample_mismatch"
	}
	if sessionID := strings.TrimSpace(anyToString(row["session_id"])); sessionID == "" || anyToString(event["session_id"]) != sessionID {
		return claim, "verification_session_mismatch"
	}
	value, valuePresent := firstPresentValue(proof["utility_value"], proof["value"])
	claimValue, claimValuePresent := firstPresentValue(claim["value"])
	if !valuePresent || !claimValuePresent || math.Abs(anyToFloat(value)-anyToFloat(claimValue)) > 0.000001 {
		return claim, "verification_utility_mismatch"
	}
	unit := strings.TrimSpace(anyToString(firstPresentAny(proof["utility_unit"], proof["unit"])))
	if unit == "" || unit != anyToString(claim["unit"]) {
		return claim, "verification_unit_mismatch"
	}
	digest := strings.TrimSpace(anyToString(firstPresentAny(proof["evidence_digest"], proof["verification_evidence_digest"])))
	if !utilitySHA256DigestValid(digest) || digest != anyToString(claim["evidence_digest"]) {
		return claim, "verification_digest_mismatch"
	}
	verifierKind := strings.ToLower(strings.TrimSpace(anyToString(proof["verifier_kind"])))
	if _, allowed := utilityVerifierKinds[verifierKind]; !allowed || verifierKind != strings.ToLower(anyToString(claim["verifier_kind"])) {
		return claim, "verification_verifier_invalid"
	}
	verifierID := strings.TrimSpace(anyToString(proof["verifier_id"]))
	if verifierID == "" || verifierID != anyToString(claim["verifier_id"]) {
		return claim, "verification_verifier_mismatch"
	}
	eventVerifierID := strings.TrimSpace(anyToString(event["agent_id"]))
	if eventVerifierID == "" || !strings.EqualFold(eventVerifierID, verifierID) {
		return claim, "verification_verifier_identity_mismatch"
	}
	if agentID := strings.TrimSpace(anyToString(row["agent_id"])); agentID != "" && strings.EqualFold(agentID, verifierID) {
		return claim, "verification_not_independent"
	}
	proofPassed, proofPassedPresent := firstPresentValue(proof["verification_passed"], proof["passed"])
	claimPassed, claimPassedPresent := firstPresentValue(claim["verification_passed"])
	if !proofPassedPresent || !claimPassedPresent || anyToBool(proofPassed) != anyToBool(claimPassed) {
		return claim, "verification_result_mismatch"
	}
	if !anyToBool(proofPassed) {
		return claim, "verification_failed"
	}
	claim["independently_verified"] = true
	claim["verification_status"] = "verified"
	claim["verified_at"] = firstNonEmptyStrings(anyToString(event["created_at"]), nowUTCISO())
	claim["evidence_digest"] = digest
	claim["verifier_kind"] = verifierKind
	claim["verifier_id"] = verifierID
	claim["verification_actor_id"] = eventVerifierID
	return claim, ""
}

func utilityRefreshEligibility(row map[string]any, exclusions []string) map[string]any {
	exclusions = utilityUniqueStrings(exclusions)
	denominator := anyMap(row["denominator"])
	claim := anyMap(row["utility"])
	if !anyToBool(denominator["model_visible_context_tokens_exact"]) || anyToInt(denominator["model_visible_context_tokens"], 0) <= 0 {
		exclusions = append(exclusions, "model_visible_context_tokens_not_exact")
	}
	if _, present := claim["value"]; !present || anyToBool(claim["value_invalid"]) {
		exclusions = append(exclusions, "utility_value_missing_or_invalid")
	}
	if strings.TrimSpace(anyToString(claim["unit"])) == "" {
		exclusions = append(exclusions, "utility_unit_missing")
	}
	if !anyToBool(claim["independently_verified"]) {
		exclusions = append(exclusions, "utility_not_independently_verified")
	}
	exclusions = utilityUniqueStrings(exclusions)
	modelVisible := anyToInt(denominator["model_visible_context_tokens"], 0)
	observedEligible := len(exclusions) == 0 && modelVisible > 0
	eligibility := map[string]any{
		"observed_yield_eligible": observedEligible,
		"causal_gain_eligible":    false,
		"exclusion_reasons":       utilityStringsToAny(exclusions),
		"causal_exclusion_reasons": []any{
			"matched_control_not_evaluated",
		},
	}
	row["eligibility"] = eligibility
	if observedEligible {
		row["observed_utility_per_1k_model_visible_tokens"] = utilityRound(anyToFloat(claim["value"])*1000/float64(modelVisible), 6)
		row["status"] = "verified_exact"
	} else {
		delete(row, "observed_utility_per_1k_model_visible_tokens")
		row["status"] = "excluded"
	}
	return row
}

func utilityUniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func utilityStringsToAny(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func buildUtilityObservation(outcome, quality, impact map[string]any, events []map[string]any) map[string]any {
	now := nowUTCISO()
	outcomeID := clipText(strings.TrimSpace(anyToString(outcome["outcome_id"])), 200)
	sampleID := clipText(strings.TrimSpace(anyToString(outcome["sample_id"])), 200)
	identity := proofTimelineIdentityFromMaps(quality, impact, outcome)
	identity["sample_id"] = sampleID
	exclusions := []string{}
	if len(quality) == 0 {
		exclusions = append(exclusions, "quality_sample_missing")
	}
	if len(impact) == 0 {
		exclusions = append(exclusions, "token_impact_missing")
	}
	if utilityIdentityMismatch(quality, impact) || utilityIdentityMismatch(quality, outcome) || utilityIdentityMismatch(impact, outcome) {
		exclusions = append(exclusions, "source_identity_mismatch")
	}
	exclusions = append(exclusions, utilityExactJoinExclusions(outcome, quality, impact)...)
	tokenizerExact := anyToBool(impact["tokenizer_exact"])
	wireTokens := anyToInt(firstPresentAny(impact["wire_tokens_exact"], impact["transport_tokens_exact"]), 0)
	modelVisible := anyToInt(impact["model_visible_context_tokens_exact"], 0)
	denominator := map[string]any{
		"wire_tokens":                         maxInt(0, wireTokens),
		"wire_tokens_exact":                   tokenizerExact && wireTokens > 0,
		"model_visible_context_tokens":        maxInt(0, modelVisible),
		"model_visible_context_tokens_exact":  tokenizerExact && modelVisible > 0,
		"provider_prompt_tokens_observed":     anyToInt(outcome["provider_prompt_tokens"], 0),
		"provider_completion_tokens_observed": anyToInt(outcome["provider_completion_tokens"], 0),
		"provider_total_tokens_observed":      anyToInt(outcome["provider_total_tokens"], 0),
		"provider_usage_observed":             anyToInt(outcome["provider_total_tokens"], 0) > 0,
		"tokenizer_encoding":                  anyToString(impact["tokenizer_encoding"]),
	}
	claim := normalizeUtilityOutcomeClaim(outcome)
	event := utilityVerificationEvent(events, anyToString(claim["verification_event_id"]))
	verifiedClaim, verificationReason := utilityVerifyClaim(map[string]any{
		"outcome_id": outcomeID, "sample_id": sampleID, "session_id": anyToString(identity["session_id"]),
		"agent_id": anyToString(identity["agent_id"]), "utility": claim,
	}, event)
	if verificationReason != "" {
		exclusions = append(exclusions, verificationReason)
	}
	row := map[string]any{
		"schema_id":           utilityObservationContractID,
		"version":             1,
		"revision":            1,
		"observation_id":      "util_" + sha256Hex(outcomeID + "\x00" + sampleID)[:24],
		"source_claim_digest": utilitySourceClaimDigest(outcome),
		"outcome_id":          outcomeID,
		"sample_id":           sampleID,
		"captured_at":         firstNonEmptyStrings(anyToString(outcome["capturedAt"]), anyToString(outcome["captured_at"]), now),
		"updated_at":          now,
		"task_class":          firstNonEmptyStrings(clipText(anyToString(outcome["task_class"]), 80), "unclassified"),
		"policy_id":           clipText(anyToString(outcome["policy_id"]), 160),
		"policy_arm":          clipText(anyToString(outcome["policy_arm"]), 40),
		"policy_phase":        clipText(anyToString(outcome["policy_phase"]), 40),
		"quality": map[string]any{
			"sample_present": len(quality) > 0,
			"quality_score":  anyToInt(quality["quality_score"], 0),
			"confidence":     anyToString(quality["confidence"]),
		},
		"denominator":       denominator,
		"utility":           verifiedClaim,
		"economics":         normalizeUtilityEconomics(outcome),
		"pairing":           normalizeUtilityPairing(outcome),
		"measurement_limit": "Observed yield requires independently verified utility and exact model-visible ContextLattice tokens. Causal gain is computed only from leakage-free exact matched controls.",
	}
	copyProofTimelineIdentity(row, identity)
	row = utilityRefreshEligibility(row, exclusions)
	row["observation_digest"] = utilityObservationDigest(row)
	return row
}

func (s *server) recordUtilityOutcome(outcome map[string]any) (map[string]any, bool, error) {
	if s == nil || s.utility == nil || len(outcome) == 0 {
		return nil, false, nil
	}
	sampleID := strings.TrimSpace(anyToString(outcome["sample_id"]))
	quality, _ := s.contextPackQuality.sampleForUtility(sampleID)
	impact, _ := s.tokenImpact.sampleForUtility(sampleID)
	events := []map[string]any{}
	if sessionID := strings.TrimSpace(firstNonEmptyStrings(anyToString(outcome["session_id"]), anyToString(quality["session_id"]), anyToString(impact["session_id"]))); sessionID != "" && s.agentSessions != nil {
		_, rows, ok := s.agentSessions.get(sessionID)
		if ok {
			events = rows
		}
	}
	return s.utility.record(buildUtilityObservation(outcome, quality, impact, events))
}

func (s *server) recordUtilitySessionEvent(session, event map[string]any) map[string]any {
	if s == nil || s.utility == nil || len(event) == 0 || proofTimelineStageForEvent(anyToString(event["type"])) != "verification" {
		return nil
	}
	metadata := anyMap(event["metadata"])
	proof := anyMap(firstNonEmptyAny(metadata["utility_verification"], metadata["utilityVerification"]))
	outcomeID := strings.TrimSpace(anyToString(proof["outcome_id"]))
	if outcomeID == "" {
		return nil
	}
	row, ok := s.utility.observation(outcomeID)
	if !ok {
		if s.utility.store != nil {
			configured, enabled := s.utility.store.availability()
			if configured && !enabled {
				return map[string]any{"ok": false, "status": "persistence_unavailable", "outcome_id": outcomeID}
			}
		}
		return map[string]any{"ok": true, "status": "pending_or_not_applicable", "outcome_id": outcomeID}
	}
	if anyToString(row["session_id"]) != anyToString(session["id"]) || anyToString(anyMap(row["utility"])["verification_event_id"]) != anyToString(event["id"]) {
		return map[string]any{"ok": true, "status": "pending_or_not_applicable", "outcome_id": outcomeID}
	}
	claim, reason := utilityVerifyClaim(row, event)
	row["utility"] = claim
	exclusions := []string{}
	for _, raw := range contextPackAnyList(anyMap(row["eligibility"])["exclusion_reasons"]) {
		value := anyToString(raw)
		if strings.HasPrefix(value, "verification_") || value == "utility_not_independently_verified" {
			continue
		}
		exclusions = append(exclusions, value)
	}
	if reason != "" {
		exclusions = append(exclusions, reason)
	}
	row = utilityRefreshEligibility(row, exclusions)
	updated, err := s.utility.update(row)
	if err != nil || len(updated) == 0 {
		status := "reconciliation_unavailable"
		if errors.Is(err, errUtilityPersistenceUnavailable) {
			status = "persistence_unavailable"
		} else if errors.Is(err, errUtilityObservationUnavailable) {
			status = "observation_unavailable"
		}
		return map[string]any{"ok": false, "status": status, "outcome_id": outcomeID}
	}
	return map[string]any{
		"ok": true, "status": "reconciled", "outcome_id": outcomeID,
		"revision": anyToInt(updated["revision"], 0), "verification_status": anyToString(anyMap(updated["utility"])["verification_status"]),
	}
}

func (t *utilityTelemetry) rows(query utilityQuery) []map[string]any {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := make([]map[string]any, 0, len(t.observations))
	for _, row := range t.observations {
		if query.Project != "" && !strings.EqualFold(anyToString(row["project"]), query.Project) {
			continue
		}
		if query.TaskClass != "" && !strings.EqualFold(anyToString(row["task_class"]), query.TaskClass) {
			continue
		}
		if query.UtilityUnit != "" && !strings.EqualFold(anyToString(anyMap(row["utility"])["unit"]), query.UtilityUnit) {
			continue
		}
		captured, err := time.Parse(time.RFC3339Nano, anyToString(row["captured_at"]))
		if err == nil {
			if !query.From.IsZero() && captured.Before(query.From) {
				continue
			}
			if !query.To.IsZero() && captured.After(query.To) {
				continue
			}
		}
		rows = append(rows, cloneAnyMap(row))
	}
	return rows
}

func utilityPairProjection(rows []map[string]any) ([]map[string]any, []utilityPairMetric, map[string]int) {
	projected := make([]map[string]any, len(rows))
	byOutcome := map[string]map[string]any{}
	for index, row := range rows {
		projected[index] = cloneAnyMap(row)
		byOutcome[anyToString(row["outcome_id"])] = projected[index]
	}
	sort.SliceStable(projected, func(i, j int) bool {
		left, right := anyToString(projected[i]["captured_at"]), anyToString(projected[j]["captured_at"])
		if left == right {
			return anyToString(projected[i]["outcome_id"]) < anyToString(projected[j]["outcome_id"])
		}
		return left < right
	})
	usedControls := map[string]struct{}{}
	pairs := []utilityPairMetric{}
	exclusions := map[string]int{}
	for _, treatment := range projected {
		pairing := cloneAnyMap(anyMap(treatment["pairing"]))
		if anyToString(pairing["arm"]) != "treatment" {
			continue
		}
		reasons := []string{}
		controlID := strings.TrimSpace(anyToString(pairing["matched_control_outcome_id"]))
		control := byOutcome[controlID]
		if controlID == "" {
			reasons = append(reasons, "matched_control_not_declared")
		} else if len(control) == 0 {
			reasons = append(reasons, "matched_control_not_found")
		}
		if len(control) > 0 {
			controlPairing := anyMap(control["pairing"])
			if anyToString(controlPairing["arm"]) != "control" {
				reasons = append(reasons, "matched_row_not_control")
			}
			if _, reused := usedControls[controlID]; reused {
				reasons = append(reasons, "matched_control_reused")
			}
			if anyToString(pairing["pair_id"]) == "" || anyToString(pairing["pair_id"]) != anyToString(controlPairing["pair_id"]) {
				reasons = append(reasons, "pair_id_mismatch")
			}
			if !utilitySHA256DigestValid(anyToString(pairing["task_match_digest"])) || anyToString(pairing["task_match_digest"]) != anyToString(controlPairing["task_match_digest"]) {
				reasons = append(reasons, "task_match_digest_mismatch")
			}
			if !anyToBool(pairing["leakage_free"]) || !anyToBool(controlPairing["leakage_free"]) {
				reasons = append(reasons, "treatment_leakage_unproven")
			}
			matchingMethod := strings.ToLower(strings.TrimSpace(anyToString(pairing["matching_method"])))
			if _, allowed := utilityExactMatchingMethods[matchingMethod]; !allowed || matchingMethod != strings.ToLower(strings.TrimSpace(anyToString(controlPairing["matching_method"]))) {
				reasons = append(reasons, "matching_method_invalid_or_mismatched")
			}
			assignmentDigest := anyToString(pairing["assignment_digest"])
			if !utilitySHA256DigestValid(assignmentDigest) || assignmentDigest != anyToString(controlPairing["assignment_digest"]) {
				reasons = append(reasons, "assignment_digest_missing_or_mismatched")
			}
			for _, field := range []string{"experiment_id", "model", "runner", "harness", "context_reconstruction_contract"} {
				treatmentValue := strings.TrimSpace(anyToString(pairing[field]))
				controlValue := strings.TrimSpace(anyToString(controlPairing[field]))
				if treatmentValue == "" || controlValue == "" {
					reasons = append(reasons, "matched_context_incomplete")
				} else if treatmentValue != controlValue {
					reasons = append(reasons, field+"_mismatch")
				}
			}
			if anyToString(treatment["task_class"]) == "" || anyToString(treatment["task_class"]) != anyToString(control["task_class"]) {
				reasons = append(reasons, "task_class_mismatch")
			}
			if anyToString(treatment["project"]) != anyToString(control["project"]) {
				reasons = append(reasons, "project_mismatch")
			}
			if anyToString(treatment["session_id"]) == "" || anyToString(treatment["session_id"]) == anyToString(control["session_id"]) {
				reasons = append(reasons, "session_separation_missing")
			}
			if !anyToBool(anyMap(treatment["eligibility"])["observed_yield_eligible"]) || !anyToBool(anyMap(control["eligibility"])["observed_yield_eligible"]) {
				reasons = append(reasons, "matched_observation_ineligible")
			}
			if anyToString(anyMap(treatment["utility"])["unit"]) != anyToString(anyMap(control["utility"])["unit"]) {
				reasons = append(reasons, "utility_unit_mismatch")
			}
			treatmentDenominator := anyMap(treatment["denominator"])
			controlDenominator := anyMap(control["denominator"])
			treatmentTokens := anyToInt(treatmentDenominator["model_visible_context_tokens"], 0)
			controlTokens := anyToInt(controlDenominator["model_visible_context_tokens"], 0)
			if !anyToBool(treatmentDenominator["model_visible_context_tokens_exact"]) || !anyToBool(controlDenominator["model_visible_context_tokens_exact"]) || treatmentTokens <= 0 || controlTokens <= 0 {
				reasons = append(reasons, "matched_denominator_not_exact")
			} else if treatmentTokens != controlTokens {
				reasons = append(reasons, "model_visible_context_tokens_mismatch")
			}
			treatmentEncoding := strings.TrimSpace(anyToString(treatmentDenominator["tokenizer_encoding"]))
			controlEncoding := strings.TrimSpace(anyToString(controlDenominator["tokenizer_encoding"]))
			if treatmentEncoding == "" || controlEncoding == "" || treatmentEncoding != controlEncoding {
				reasons = append(reasons, "tokenizer_encoding_missing_or_mismatched")
			}
		}
		reasons = utilityUniqueStrings(reasons)
		eligibility := cloneAnyMap(anyMap(treatment["eligibility"]))
		eligibility["causal_gain_eligible"] = len(reasons) == 0
		eligibility["causal_exclusion_reasons"] = utilityStringsToAny(reasons)
		treatment["eligibility"] = eligibility
		for _, reason := range reasons {
			exclusions[reason]++
		}
		if len(reasons) > 0 {
			continue
		}
		usedControls[controlID] = struct{}{}
		gain := anyToFloat(anyMap(treatment["utility"])["value"]) - anyToFloat(anyMap(control["utility"])["value"])
		tokens := anyToInt(anyMap(treatment["denominator"])["model_visible_context_tokens"], 0)
		metric := utilityPairMetric{
			PairID:             anyToString(pairing["pair_id"]),
			TaskClass:          anyToString(treatment["task_class"]),
			UtilityUnit:        anyToString(anyMap(treatment["utility"])["unit"]),
			ControlOutcomeID:   controlID,
			TreatmentOutcomeID: anyToString(treatment["outcome_id"]),
			UtilityGain:        utilityRound(gain, 6),
			ModelVisibleTokens: tokens,
			GainPer1K:          utilityRound(gain*1000/float64(tokens), 6),
			CapturedAt:         anyToString(treatment["captured_at"]),
		}
		pairing["causal_utility_gain"] = metric.UtilityGain
		pairing["causal_utility_gain_per_1k_model_visible_tokens"] = metric.GainPer1K
		pairing["causal_denominator_basis"] = "equal_control_and_treatment_exact_model_visible_context_tokens"
		pairing["causal_model_visible_context_tokens"] = metric.ModelVisibleTokens
		treatment["pairing"] = pairing
		pairs = append(pairs, metric)
	}
	return projected, pairs, exclusions
}

func utilityWeightedInterval(pairs []utilityPairMetric) map[string]any {
	if len(pairs) == 0 {
		return map[string]any{"status": "unavailable", "confidence": 0.95, "method": "none", "low": nil, "high": nil}
	}
	units := map[string]struct{}{}
	for _, pair := range pairs {
		units[pair.UtilityUnit] = struct{}{}
	}
	if len(units) != 1 {
		return map[string]any{"status": "mixed_utility_units", "confidence": 0.95, "method": "none", "low": nil, "high": nil}
	}
	totalWeight, totalWeighted, totalWeightSquared := 0.0, 0.0, 0.0
	for _, pair := range pairs {
		weight := float64(pair.ModelVisibleTokens)
		totalWeight += weight
		totalWeightSquared += weight * weight
		totalWeighted += pair.GainPer1K * weight
	}
	mean := totalWeighted / totalWeight
	if len(pairs) < 2 || totalWeightSquared == 0 {
		return map[string]any{"status": "insufficient_pairs", "confidence": 0.95, "method": "single_pair_no_interval", "point": utilityRound(mean, 6), "low": nil, "high": nil}
	}
	weightedSquaredError := 0.0
	for _, pair := range pairs {
		weight := float64(pair.ModelVisibleTokens)
		delta := pair.GainPer1K - mean
		weightedSquaredError += weight * delta * delta
	}
	effectiveN := totalWeight * totalWeight / totalWeightSquared
	varianceDenominator := totalWeight - totalWeightSquared/totalWeight
	if effectiveN <= 1 || varianceDenominator <= 0 {
		return map[string]any{"status": "insufficient_effective_sample", "confidence": 0.95, "method": "weighted_student_t_95", "point": utilityRound(mean, 6), "low": nil, "high": nil, "effective_n": utilityRound(effectiveN, 3)}
	}
	variance := weightedSquaredError / varianceDenominator
	degreesOfFreedom := maxInt(1, int(math.Floor(effectiveN))-1)
	margin := utilityStudentTCritical95(degreesOfFreedom) * math.Sqrt(variance/effectiveN)
	return map[string]any{
		"status": "available", "confidence": 0.95, "method": "weighted_student_t_95",
		"point": utilityRound(mean, 6), "low": utilityRound(mean-margin, 6), "high": utilityRound(mean+margin, 6),
		"effective_n": utilityRound(effectiveN, 3), "degrees_of_freedom": degreesOfFreedom,
	}
}

func utilityStudentTCritical95(degreesOfFreedom int) float64 {
	critical := [...]float64{
		0, 12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262,
		2.228, 2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093,
		2.086, 2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042,
	}
	if degreesOfFreedom < 1 {
		return critical[1]
	}
	if degreesOfFreedom >= len(critical) {
		return 1.96
	}
	return critical[degreesOfFreedom]
}

func utilityAggregate(rows []map[string]any, pairs []utilityPairMetric, pairExclusions map[string]int) map[string]any {
	observationCount := len(rows)
	verifiedCount, exactCount := 0, 0
	totalUtility := 0.0
	totalModelVisible, totalWire, totalProvider := int64(0), int64(0), int64(0)
	totalLatency, latencyCount := int64(0), int64(0)
	totalCost, totalToolCalls, totalFailures, failedObservations := int64(0), int64(0), int64(0), int64(0)
	exclusions := map[string]int{}
	utilityUnits := map[string]struct{}{}
	for _, row := range rows {
		eligibility := anyMap(row["eligibility"])
		for _, raw := range contextPackAnyList(eligibility["exclusion_reasons"]) {
			exclusions[anyToString(raw)]++
		}
		denominator := anyMap(row["denominator"])
		if anyToBool(denominator["wire_tokens_exact"]) {
			totalWire += int64(anyToInt(denominator["wire_tokens"], 0))
		}
		if anyToInt(denominator["provider_total_tokens_observed"], 0) > 0 {
			totalProvider += int64(anyToInt(denominator["provider_total_tokens_observed"], 0))
		}
		if anyToBool(anyMap(row["utility"])["independently_verified"]) {
			verifiedCount++
		}
		if anyToBool(eligibility["observed_yield_eligible"]) {
			exactCount++
			totalUtility += anyToFloat(anyMap(row["utility"])["value"])
			totalModelVisible += int64(anyToInt(denominator["model_visible_context_tokens"], 0))
			utilityUnits[anyToString(anyMap(row["utility"])["unit"])] = struct{}{}
		}
		economics := anyMap(row["economics"])
		if latency := anyToInt(economics["latency_ms"], 0); latency > 0 {
			totalLatency += int64(latency)
			latencyCount++
		}
		totalCost += int64(anyToInt(economics["cost_microusd"], 0))
		totalToolCalls += int64(anyToInt(economics["tool_calls"], 0))
		failures := anyToInt(economics["failures"], 0)
		totalFailures += int64(failures)
		if failures > 0 {
			failedObservations++
		}
	}
	observedYield := any(nil)
	if totalModelVisible > 0 && len(utilityUnits) == 1 {
		observedYield = utilityRound(totalUtility*1000/float64(totalModelVisible), 6)
	}
	pairGain, pairTokens := 0.0, 0
	for _, pair := range pairs {
		pairGain += pair.UtilityGain
		pairTokens += pair.ModelVisibleTokens
	}
	causalPoint := any(nil)
	if pairTokens > 0 && len(utilityUnits) == 1 {
		causalPoint = utilityRound(pairGain*1000/float64(pairTokens), 6)
	}
	averageLatency := any(nil)
	if latencyCount > 0 {
		averageLatency = utilityRound(float64(totalLatency)/float64(latencyCount), 3)
	}
	verifiedUtilitySum := any(nil)
	if len(utilityUnits) == 1 {
		verifiedUtilitySum = utilityRound(totalUtility, 6)
	}
	unitNames := make([]string, 0, len(utilityUnits))
	for unit := range utilityUnits {
		unitNames = append(unitNames, unit)
	}
	sort.Strings(unitNames)
	claimStatus := utilityClaimStatus(len(pairs))
	if len(utilityUnits) > 1 {
		claimStatus = "mixed_utility_units"
	}
	return map[string]any{
		"observation_count":                               observationCount,
		"independently_verified_count":                    verifiedCount,
		"observed_yield_eligible_count":                   exactCount,
		"causal_pair_count":                               len(pairs),
		"verified_utility_sum":                            verifiedUtilitySum,
		"utility_units":                                   utilityStringsToAny(unitNames),
		"utility_unit_count":                              len(unitNames),
		"observed_utility_per_1k_model_visible_tokens":    observedYield,
		"causal_utility_gain_per_1k_model_visible_tokens": causalPoint,
		"causal_interval":                                 utilityWeightedInterval(pairs),
		"claim_status":                                    claimStatus,
		"denominators": map[string]any{
			"wire_tokens_exact":                  totalWire,
			"model_visible_context_tokens_exact": totalModelVisible,
			"provider_total_tokens_observed":     totalProvider,
		},
		"economics": map[string]any{
			"latency_sample_count":     latencyCount,
			"average_latency_ms":       averageLatency,
			"total_cost_microusd":      totalCost,
			"tool_calls":               totalToolCalls,
			"failures":                 totalFailures,
			"failed_observation_count": failedObservations,
		},
		"observation_exclusions": utilityIntMap(exclusions),
		"causal_exclusions":      utilityIntMap(pairExclusions),
	}
}

func utilityClaimStatus(pairCount int) string {
	switch {
	case pairCount >= 2:
		return "causal_estimate"
	case pairCount == 1:
		return "paired_observation_without_interval"
	default:
		return "observed_yield_only"
	}
}

func utilityIntMap(values map[string]int) map[string]any {
	out := map[string]any{}
	for key, value := range values {
		out[key] = value
	}
	return out
}

func utilityTaskClassSummaries(rows []map[string]any, pairs []utilityPairMetric) []any {
	byClass := map[string][]map[string]any{}
	pairsByClass := map[string][]utilityPairMetric{}
	for _, row := range rows {
		key := firstNonEmptyStrings(anyToString(row["task_class"]), "unclassified")
		byClass[key] = append(byClass[key], row)
	}
	for _, pair := range pairs {
		pairsByClass[pair.TaskClass] = append(pairsByClass[pair.TaskClass], pair)
	}
	keys := make([]string, 0, len(byClass))
	for key := range byClass {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]any, 0, len(keys))
	for _, key := range keys {
		summary := utilityAggregate(byClass[key], pairsByClass[key], map[string]int{})
		summary["task_class"] = key
		out = append(out, summary)
	}
	return out
}

func utilityStorageStatus(store *utilityLedgerStore) map[string]any {
	status := map[string]any{"enabled": false, "durability": "disabled"}
	if store == nil {
		return status
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	status["configured"] = store.configured
	status["enabled"] = store.enabled
	if store.enabled {
		status["durability"] = "bounded_fsync_ndjson"
	} else if store.configured {
		status["durability"] = "unavailable"
	}
	status["max_bytes"] = store.maxBytes
	status["max_samples"] = store.maxSamples
	status["loaded_rows"] = store.loadedRows
	status["physical_rows"] = store.physicalRows
	status["parse_errors"] = store.parseErrors
	status["write_errors"] = store.writeErrors
	status["last_write_at"] = store.lastWriteAt
	status["last_error"] = store.lastError
	return status
}

func (t *utilityTelemetry) snapshot(query utilityQuery) map[string]any {
	rows := t.rows(query)
	projected, pairs, pairExclusions := utilityPairProjection(rows)
	summary := utilityAggregate(projected, pairs, pairExclusions)
	limit := query.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = clampInt(limit, 1, 100)
	start := maxInt(0, len(projected)-limit)
	visible := make([]any, 0, len(projected)-start)
	for _, row := range projected[start:] {
		visible = append(visible, row)
	}
	payload := map[string]any{
		"ok":         true,
		"schema_id":  utilityLedgerContractID,
		"version":    1,
		"updated_at": nowUTCISO(),
		"claim_posture": map[string]any{
			"observed_yield":  "requires independently verified utility and exact model-visible ContextLattice tokens",
			"causal_gain":     "requires an exact leakage-free matched control with identical task digest, class, project, and utility unit",
			"modeled_savings": "reported separately and never promoted into verified utility",
		},
		"filters": map[string]any{
			"project": query.Project, "task_class": query.TaskClass, "utility_unit": query.UtilityUnit,
			"from": utilityTimeString(query.From), "to": utilityTimeString(query.To),
		},
		"summary":           summary,
		"task_classes":      utilityTaskClassSummaries(projected, pairs),
		"observations":      visible,
		"storage":           utilityStorageStatus(t.store),
		"measurement_limit": "Wire, model-visible ContextLattice, and observed provider-total tokens are separate denominators. No provider usage, cost, causal effect, or verification is inferred when evidence is absent.",
	}
	return attachPayloadFormatContract(utilityLedgerContractID, payload, "", "utility_ledger", utilityTelemetryPath)
}

func utilityTimeString(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func utilityQueryFromRequest(r *http.Request, maximumLimit int) (utilityQuery, error) {
	query := utilityQuery{Limit: 20}
	if r == nil {
		return query, nil
	}
	query.Project = strings.TrimSpace(r.URL.Query().Get("project"))
	query.TaskClass = strings.TrimSpace(r.URL.Query().Get("task_class"))
	query.UtilityUnit = strings.TrimSpace(r.URL.Query().Get("utility_unit"))
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 {
			return query, errors.New("limit must be a positive integer")
		}
		query.Limit = clampInt(value, 1, maximumLimit)
	}
	for key, target := range map[string]*time.Time{"from": &query.From, "to": &query.To} {
		raw := strings.TrimSpace(r.URL.Query().Get(key))
		if raw == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			return query, errors.New(key + " must be RFC3339")
		}
		*target = parsed.UTC()
	}
	if !query.From.IsZero() && !query.To.IsZero() && query.From.After(query.To) {
		return query, errors.New("from must not be after to")
	}
	return query, nil
}

func (s *server) telemetryUtilityRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	query, err := utilityQueryFromRequest(r, 100)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_utility_query", "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.utility.snapshot(query))
}

func (s *server) telemetryUtilityAnalyticsRoute(w http.ResponseWriter, r *http.Request) {
	optionalFrontierT3UtilityAnalyticsRoute(s, w, r)
}

func (s *server) telemetryUtilityPolicyRoute(w http.ResponseWriter, r *http.Request) {
	optionalFrontierT3UtilityPolicyRoute(s, w, r)
}

func (l *utilityLedgerStore) availability() (bool, bool) {
	if l == nil {
		return false, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.configured, l.enabled
}

func (l *utilityLedgerStore) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	unlock := l.unlock
	l.unlock = nil
	l.enabled = false
	l.mu.Unlock()
	if unlock != nil {
		unlock()
	}
}

func (l *utilityLedgerStore) observation(outcomeID string) (map[string]any, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.latest[strings.TrimSpace(outcomeID)]
	if !ok {
		return nil, false
	}
	return cloneAnyMap(row), true
}

func (l *utilityLedgerStore) setLatestRowsLocked(rows []map[string]any) {
	l.latest = make(map[string]map[string]any, len(rows))
	for _, row := range rows {
		if outcomeID := strings.TrimSpace(anyToString(row["outcome_id"])); outcomeID != "" {
			l.latest[outcomeID] = cloneAnyMap(row)
		}
	}
}

func (l *utilityLedgerStore) readRows() ([]map[string]any, int, error) {
	if l == nil {
		return nil, 0, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled || l.path == "" {
		return nil, 0, nil
	}
	rows, parseErrors, physicalRows, err := l.readRowsUnlocked()
	if err != nil {
		return nil, parseErrors, l.failClosedLocked(err)
	}
	stat, statErr := os.Stat(l.path)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, parseErrors, l.failClosedLocked(statErr)
	}
	needsRewrite := parseErrors > 0 || physicalRows > l.maxSamples || len(rows) > l.maxSamples || (statErr == nil && stat.Size() > l.maxBytes)
	if needsRewrite {
		rows, err = l.writeBoundedRowsLocked(rows)
		if err != nil {
			return nil, parseErrors, l.failWriteLocked(err)
		}
		physicalRows = len(rows)
	}
	l.loadedRows = len(rows)
	l.physicalRows = physicalRows
	l.setLatestRowsLocked(rows)
	return rows, parseErrors, nil
}

func (l *utilityLedgerStore) append(row map[string]any) (map[string]any, bool, error) {
	if l == nil {
		return nil, false, errUtilityPersistenceUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled || l.path == "" {
		return nil, false, errUtilityPersistenceUnavailable
	}
	raw, err := json.Marshal(row)
	if err != nil {
		l.writeErrors++
		return nil, false, err
	}
	if int64(len(raw)+1) > l.maxBytes {
		l.writeErrors++
		return nil, false, errors.New("utility observation exceeds configured ledger byte limit")
	}
	outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
	claimDigest := strings.TrimSpace(anyToString(row["source_claim_digest"]))
	if outcomeID == "" || !utilitySHA256DigestValid(claimDigest) {
		l.writeErrors++
		return nil, false, errors.New("utility observation has invalid durable identity")
	}
	if existing, exists := l.latest[outcomeID]; exists {
		if anyToString(existing["source_claim_digest"]) != claimDigest {
			l.writeErrors++
			return cloneAnyMap(existing), false, errUtilityOutcomeConflict
		}
		if anyToInt(row["revision"], 1) <= anyToInt(existing["revision"], 1) {
			return cloneAnyMap(existing), false, nil
		}
	}
	// A durable atomic replacement keeps the canonical ledger either wholly old
	// or wholly new. A short append cannot strand an unparseable source claim
	// whose identity would be lost on restart.
	rows, parseErrors, _, err := l.readRowsUnlocked()
	if err != nil {
		return nil, false, l.failWriteLocked(err)
	}
	l.parseErrors += parseErrors
	l.setLatestRowsLocked(rows)
	if existing, exists := l.latest[outcomeID]; exists {
		if anyToString(existing["source_claim_digest"]) != claimDigest {
			l.writeErrors++
			return cloneAnyMap(existing), false, errUtilityOutcomeConflict
		}
		if anyToInt(row["revision"], 1) <= anyToInt(existing["revision"], 1) && parseErrors == 0 {
			return cloneAnyMap(existing), false, nil
		}
	}
	existing, existed := l.latest[outcomeID]
	wroteClaim := !existed || anyToInt(row["revision"], 1) > anyToInt(existing["revision"], 1)
	rows, err = utilityUpsertLedgerRow(rows, row)
	if err != nil {
		l.writeErrors++
		return nil, false, err
	}
	kept, err := l.writeBoundedRowsLocked(rows)
	if err != nil {
		return nil, false, l.failWriteLocked(err)
	}
	l.lastWriteAt = nowUTCISO()
	l.lastError = ""
	l.loadedRows = len(kept)
	l.physicalRows = len(kept)
	persisted, exists := l.latest[outcomeID]
	if !exists {
		return nil, false, l.failWriteLocked(errors.New("utility observation was not retained by bounded persistence"))
	}
	return cloneAnyMap(persisted), wroteClaim, nil
}

func (l *utilityLedgerStore) failClosedLocked(err error) error {
	if l == nil || err == nil {
		return err
	}
	l.enabled = false
	l.lastError = utilityLedgerErrorCode(err)
	return err
}

func (l *utilityLedgerStore) failWriteLocked(err error) error {
	if l == nil || err == nil {
		return err
	}
	l.writeErrors++
	return l.failClosedLocked(err)
}

func utilityUpsertLedgerRow(rows []map[string]any, row map[string]any) ([]map[string]any, error) {
	outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
	for index := range rows {
		if anyToString(rows[index]["outcome_id"]) == outcomeID {
			if anyToString(rows[index]["source_claim_digest"]) != anyToString(row["source_claim_digest"]) {
				return rows, errUtilityOutcomeConflict
			}
			if anyToInt(row["revision"], 1) <= anyToInt(rows[index]["revision"], 1) {
				return rows, nil
			}
			rows = append(rows[:index], rows[index+1:]...)
			return append(rows, cloneAnyMap(row)), nil
		}
	}
	return append(rows, cloneAnyMap(row)), nil
}

func (l *utilityLedgerStore) writeBoundedRowsLocked(rows []map[string]any) ([]map[string]any, error) {
	if len(rows) > l.maxSamples {
		rows = rows[len(rows)-l.maxSamples:]
	}
	type encodedLedgerRow struct {
		row map[string]any
		raw []byte
	}
	encoded := make([]encodedLedgerRow, 0, len(rows))
	total := int64(0)
	for index := len(rows) - 1; index >= 0; index-- {
		raw, err := json.Marshal(rows[index])
		if err != nil {
			continue
		}
		lineBytes := int64(len(raw) + 1)
		if lineBytes > l.maxBytes {
			continue
		}
		if total+lineBytes > l.maxBytes {
			break
		}
		encoded = append(encoded, encodedLedgerRow{row: cloneAnyMap(rows[index]), raw: raw})
		total += lineBytes
	}
	for left, right := 0, len(encoded)-1; left < right; left, right = left+1, right-1 {
		encoded[left], encoded[right] = encoded[right], encoded[left]
	}
	content := []byte{}
	kept := make([]map[string]any, 0, len(encoded))
	for _, item := range encoded {
		content = append(content, item.raw...)
		content = append(content, '\n')
		kept = append(kept, item.row)
	}
	dedicatedParent := strings.TrimSpace(os.Getenv("GO_UTILITY_LEDGER_PATH")) == ""
	writeFile := l.writeFile
	if writeFile == nil {
		writeFile = writeOwnerOnlyDurableAtomicFile
	}
	if err := writeFile(l.path, content, dedicatedParent); err != nil {
		return nil, err
	}
	l.loadedRows = len(kept)
	l.physicalRows = len(kept)
	l.setLatestRowsLocked(kept)
	return kept, nil
}

func (l *utilityLedgerStore) readRowsUnlocked() ([]map[string]any, int, int, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, 0, nil
		}
		return nil, 0, 0, err
	}
	defer file.Close()
	latest := map[string]map[string]any{}
	order := []string{}
	parseErrors := 0
	physicalRows := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		physicalRows++
		row := map[string]any{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		if anyToString(row["schema_id"]) != utilityObservationContractID {
			continue
		}
		outcomeID := strings.TrimSpace(anyToString(row["outcome_id"]))
		if outcomeID == "" {
			parseErrors++
			continue
		}
		claimDigest := strings.TrimSpace(anyToString(row["source_claim_digest"]))
		if !utilitySHA256DigestValid(claimDigest) {
			parseErrors++
			continue
		}
		if existing, exists := latest[outcomeID]; !exists {
			order = append(order, outcomeID)
		} else {
			if anyToString(existing["source_claim_digest"]) != claimDigest {
				parseErrors++
				continue
			}
			if anyToInt(row["revision"], 1) <= anyToInt(existing["revision"], 1) {
				continue
			}
		}
		latest[outcomeID] = row
	}
	rows := make([]map[string]any, 0, len(order))
	for _, outcomeID := range order {
		rows = append(rows, latest[outcomeID])
	}
	return rows, parseErrors, physicalRows, scanner.Err()
}

func (l *utilityLedgerStore) setError(err error) {
	if l == nil || err == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastError = utilityLedgerErrorCode(err)
}

func utilityLedgerErrorCode(err error) string {
	if errors.Is(err, errOwnerOnlyMigrationLocked) {
		return "single_writer_lock_unavailable"
	}
	return tokenImpactLedgerErrorCode(err)
}
