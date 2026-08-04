package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func outcomeIntelligenceCandidateRef(seed string) string {
	return "rtc_" + sha256Hex(seed)[:24]
}

// outcomeIntelligenceLargeQualitySample keeps retention tests within the
// production identifier contract while still creating a materially large,
// privacy-safe ledger row.
func outcomeIntelligenceLargeQualitySample(sampleID, seed string) map[string]any {
	candidates := make([]any, 0, contextPackSelectionReceiptLimit)
	for index := 0; index < contextPackSelectionReceiptLimit; index++ {
		candidates = append(candidates, map[string]any{
			"candidate_ref":   outcomeIntelligenceCandidateRef(seed + strconv.Itoa(index)),
			"selection_state": "selected", "ordinal": index + 1, "evidence_kind": "decision",
		})
	}
	return map[string]any{
		"sample_id": sampleID, "project": "contextlattice", "quality_score": 1,
		"selection_receipt": map[string]any{"candidates": candidates},
	}
}

func outcomeIntelligenceLargeScalarQualitySample(sampleID string) map[string]any {
	return map[string]any{
		"sample_id": sampleID, "project": "contextlattice", "quality_score": 1,
		"session_id": strings.Repeat("s", 120), "task_id": strings.Repeat("t", 160),
		"task_identity_id": strings.Repeat("i", 160), "execution_lane_id": strings.Repeat("l", 160),
		"agent_id": strings.Repeat("a", 160), "policy_id": strings.Repeat("p", 160),
		"task_class": strings.Repeat("c", 80), "retrieval_intent": strings.Repeat("r", 80),
	}
}

func TestContextPackQualitySelectionReceiptIsOpaqueBoundedAndDurable(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-regression-fixtures.ndjson"))

	selectedRef := outcomeIntelligenceCandidateRef("selected")
	omittedRef := outcomeIntelligenceCandidateRef("omitted")
	secret := "never-persist-this-source-body-or-project-path"
	sample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "opaque receipt test", Project: "contextlattice", TopicPath: "private/topic",
		RankedEvidence: []any{map[string]any{
			"candidate_id": selectedRef, "kind": "decision", "rank": 3,
			"summary": secret, "file": "/private/should-not-cross", "text": secret,
		}},
		OmittedHighValueRefs: []any{map[string]any{
			"candidate_id": omittedRef, "kind": "risk", "summary": secret, "file": "/private/also-not",
		}},
	})
	receipt := anyMap(sample["selection_receipt"])
	candidates := parseRows(receipt["candidates"])
	if anyToString(receipt["schema_id"]) != contextPackSelectionReceiptSchemaID || len(candidates) != 2 {
		t.Fatalf("expected two opaque selection candidates, got %#v", receipt)
	}
	if anyToString(candidates[0]["candidate_ref"]) != selectedRef || anyToString(candidates[0]["selection_state"]) != "selected" || anyToInt(candidates[0]["ordinal"], 0) != 3 {
		t.Fatalf("selected candidate receipt lost exact opaque receipt data: %#v", candidates[0])
	}
	if anyToString(candidates[1]["candidate_ref"]) != omittedRef || anyToString(candidates[1]["selection_state"]) != "omitted" {
		t.Fatalf("omitted candidate receipt was not retained: %#v", candidates[1])
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "private/topic", "/private/should-not-cross", "\"summary\"", "\"text\"", "\"file\""} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("selection receipt leaked %q: %s", forbidden, encoded)
		}
	}

	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(sample)
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read durable quality ledger: %v", err)
	}
	if !strings.Contains(string(raw), selectedRef) || !strings.Contains(string(raw), omittedRef) {
		t.Fatalf("quality ledger did not persist selection receipt: %s", raw)
	}
	for _, forbidden := range []string{secret, "/private/should-not-cross"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("durable selection receipt leaked %q: %s", forbidden, raw)
		}
	}
}

func TestContextPackQualityTelemetryNeverPersistsPrivateTopicOrOutcomePayloads(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	fixturePath := filepath.Join(t.TempDir(), "context-pack-regression-fixtures.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", fixturePath)

	privateTopic := "private/client-omega/roadmap?secret=do-not-store"
	privatePayload := "/private/client-omega/query=do-not-store"
	compactSecret := "sk_live_customer_omega_7f3a9c"
	sample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "do not persist this private query", Project: "contextlattice", TopicPath: privateTopic,
		RankedEvidence: []any{map[string]any{"candidate_id": outcomeIntelligenceCandidateRef("topic-private"), "kind": "decision"}},
	})
	second := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "independent query", Project: "contextlattice", TopicPath: privateTopic,
	})
	wantTopicRef := contextPackQualityTopicRef("contextlattice", privateTopic)
	if got := anyToString(sample["topic_ref"]); got != wantTopicRef || anyToString(second["topic_ref"]) != wantTopicRef || !utilitySHA256DigestValid(got) {
		t.Fatalf("topic ref was not stable opaque identity: first=%q second=%q want=%q", got, anyToString(second["topic_ref"]), wantTopicRef)
	}
	if _, leaked := sample["topic_path"]; leaked {
		t.Fatalf("context-pack quality sample retained raw topic path: %#v", sample)
	}

	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(sample)
	qualityRaw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read quality ledger: %v", err)
	}
	if strings.Contains(string(qualityRaw), privateTopic) || strings.Contains(string(qualityRaw), privatePayload) {
		t.Fatalf("quality ledger leaked private sample data: %s", qualityRaw)
	}

	digest := "sha256:" + sha256Hex("privacy-outcome-evidence")
	payload := map[string]any{
		"outcome_id": "outcome_privacy", "sample_id": sample["sample_id"], "project": "contextlattice",
		"first_pass_success": true, "topic_path": privateTopic, "query": privatePayload, "content": privatePayload,
		"utility": map[string]any{
			"value": 5, "unit": "unit_" + compactSecret, "verification_event_id": "evt_" + compactSecret,
			"evidence_digest": digest, "verification_passed": true, "verifier_kind": "deterministic_test",
			"verifier_id": "verifier_" + compactSecret, "raw_query": privatePayload,
		},
		"economics": map[string]any{"latency_ms": 17, "raw_query": privatePayload},
		"pairing": map[string]any{
			"pair_id": "pair_" + compactSecret, "matching_method": "exact_holdout", "task_match_digest": digest,
			"assignment_digest": digest, "leakage_free": true, "source_path": privatePayload,
			"model": "model_" + compactSecret, "runner": "runner_" + compactSecret, "harness": "harness_" + compactSecret,
			"experiment_id": "experiment_" + compactSecret, "context_reconstruction_contract": "contract_" + compactSecret,
		},
		"regression_case": map[string]any{
			"query": privatePayload, "project": "contextlattice", "topic_path": privateTopic,
			"expected_files": []string{privatePayload}, "negative_files": []string{privatePayload},
		},
		"regression_partition": "train", "traffic_class": "user",
		"stability": map[string]any{"stable": true, "run_count": 2, "result_digests": []string{digest, digest}},
		"evidence_attribution": []any{map[string]any{
			"entity_type": "file", "file": privatePayload, "attribution_method": "counterfactual", "verification_evidence_digest": digest,
			"issuer": "issuer_" + compactSecret, "producer_agent_id": "producer_" + compactSecret, "verifier_id": "verifier_" + compactSecret,
		}},
	}
	entry := contextPackQualityOutcomeFromSample(payload)
	utility := anyMap(entry["utility"])
	pairing := anyMap(entry["pairing"])
	if !utilitySHA256DigestValid(anyToString(utility["unit"])) || !utilitySHA256DigestValid(anyToString(utility["verification_event_id"])) ||
		!utilitySHA256DigestValid(anyToString(utility["verifier_id"])) || anyToInt(anyMap(entry["economics"])["latency_ms"], 0) != 17 ||
		!utilitySHA256DigestValid(anyToString(pairing["pair_id"])) || !utilitySHA256DigestValid(anyToString(pairing["model"])) ||
		!utilitySHA256DigestValid(anyToString(pairing["runner"])) || !utilitySHA256DigestValid(anyToString(pairing["harness"])) ||
		!utilitySHA256DigestValid(anyToString(entry["regression_case_ref"])) {
		t.Fatalf("strict outcome normalization lost required measured fields: %#v", entry)
	}
	for _, value := range []string{anyToString(utility["unit"]), anyToString(utility["verification_event_id"]), anyToString(utility["verifier_id"]), anyToString(pairing["pair_id"])} {
		if strings.Contains(value, compactSecret) {
			t.Fatalf("compact reporter secret was retained rather than converted to an opaque ref: %#v", entry)
		}
	}
	if _, leaked := anyMap(entry["utility"])["raw_query"]; leaked {
		t.Fatalf("utility raw field crossed quality boundary: %#v", entry)
	}
	if _, leaked := anyMap(entry["pairing"])["source_path"]; leaked {
		t.Fatalf("pairing raw path crossed quality boundary: %#v", entry)
	}
	if _, leaked := entry["regression_case"]; leaked {
		t.Fatalf("raw regression fixture crossed quality boundary: %#v", entry)
	}

	s := &server{contextPackQuality: telemetry}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(body)))
	if response.Code != http.StatusOK {
		t.Fatalf("record private outcome: status=%d body=%s", response.Code, response.Body.String())
	}
	fixtureRaw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read private regression fixture store: %v", err)
	}
	if strings.Count(string(fixtureRaw), `"fixture_ref"`) != 1 || !strings.Contains(string(fixtureRaw), privateTopic) {
		t.Fatalf("fixture sidecar did not retain exactly one private canonical fixture: %s", fixtureRaw)
	}
	qualityRaw, err = os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read quality ledger after outcome: %v", err)
	}
	snapshotRaw, err := json.Marshal(telemetry.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"ledger": qualityRaw, "snapshot": snapshotRaw, "response": response.Body.Bytes()} {
		if strings.Contains(string(raw), privateTopic) || strings.Contains(string(raw), privatePayload) || strings.Contains(string(raw), compactSecret) || strings.Contains(string(raw), "topic_path") {
			t.Fatalf("%s leaked private quality data: %s", name, raw)
		}
	}
	snapshotSamples := parseRows(anyMap(telemetry.snapshot())["samples"])
	if len(snapshotSamples) != 1 || anyToString(snapshotSamples[0]["topic_ref"]) != wantTopicRef {
		t.Fatalf("snapshot lost opaque topic grouping: %#v", snapshotSamples)
	}
}

func contextPackTopicMigrationRows() []map[string]any {
	return []map[string]any{
		{
			"schema_id": contextPackQualitySchemaID, "version": 1, "sample_id": "cpq_migrate_alpha",
			"project": "contextlattice", "topic_path": "private/customer-alpha/roadmap",
			"selection_receipt": map[string]any{"schema_id": contextPackSelectionReceiptSchemaID, "version": 1,
				"candidates": []any{map[string]any{"candidate_ref": outcomeIntelligenceCandidateRef("migration-alpha"), "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision"}},
			},
		},
		{
			"schema_id": contextPackQualityOutcomeSchemaID, "version": 1, "outcome_id": "outcome_migrate_alpha",
			"capturedAt": "2026-08-04T00:00:00Z", "sample_id": "cpq_migrate_alpha", "task_id": "", "project": "contextlattice",
			"task_class": "", "retrieval_intent": "", "first_pass_success": true, "repair_required": false,
			"retry_count": 0, "observed_followup_tokens": 0, "outcome_source": "adapter_complete", "outcome_class": "success",
			"context_attribution": "unknown", "calibration_eligible": true, "topic_path": "private/customer-alpha/outcome",
		},
		{
			"schema_id": contextPackQualitySchemaID, "version": 1, "sample_id": "cpq_migrate_beta",
			"project": "contextlattice", "topic_path": "",
			"selection_receipt": map[string]any{"schema_id": contextPackSelectionReceiptSchemaID, "version": 1,
				"candidates": []any{map[string]any{"candidate_ref": outcomeIntelligenceCandidateRef("migration-beta"), "selection_state": "omitted", "ordinal": 1, "evidence_kind": "risk"}},
			},
		},
	}
}

func contextPackCanonicalJSON(t *testing.T, row map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeContextPackTopicMigrationRows(t *testing.T, path string, rows []map[string]any) []byte {
	t.Helper()
	content := make([]byte, 0)
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, encoded...)
		content = append(content, '\n')
	}
	if err := writeOwnerOnlyDurableAtomicFile(path, content, false); err != nil {
		t.Fatalf("write legacy quality ledger: %v", err)
	}
	return content
}

func contextPackTopicMigrationLedger(path string) *contextPackQualityLedger {
	return &contextPackQualityLedger{
		enabled: true, path: path, maxBytes: 4 * 1024 * 1024, maxSamples: 100,
		writeFile: writeOwnerOnlyDurableAtomicFile,
	}
}

func TestContextPackQualityTopicPrivacyMigrationIsAtomicAndOneWay(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-regression-fixtures.ndjson"))

	legacyRows := contextPackTopicMigrationRows()
	writeContextPackTopicMigrationRows(t, ledgerPath, legacyRows)
	ledger := contextPackTopicMigrationLedger(ledgerPath)
	writes := 0
	ledger.writeFile = func(path string, content []byte, dedicatedParent bool) error {
		writes++
		return writeOwnerOnlyDurableAtomicFile(path, content, dedicatedParent)
	}
	telemetry := newContextPackQualityTelemetryWithLedger(20, ledger)
	if len(telemetry.snapshot()["samples"].([]any)) != 2 || len(telemetry.snapshot()["outcomes"].([]any)) != 1 {
		t.Fatalf("migrated telemetry did not preserve valid rows: %#v", telemetry.snapshot())
	}
	migratedSnapshot := telemetry.snapshot()
	migratedOutcomes := parseRows(migratedSnapshot["outcomes"])
	if len(migratedOutcomes) != 1 || anyToString(migratedOutcomes[0]["quality_sample_admission"]) != "legacy_ineligible" || anyToBool(migratedOutcomes[0]["calibration_eligible"]) {
		t.Fatalf("migrated legacy outcome was not explicitly observable-only: %#v", migratedSnapshot)
	}
	wantOutcomeSourceRef := contextPackQualityOpaqueReporterRef("contextlattice", "outcome_source", "adapter_complete", 80)
	if anyToString(migratedOutcomes[0]["outcome_source"]) != wantOutcomeSourceRef {
		t.Fatalf("legacy outcome reporter was not replaced with its project-bound ref: %#v", migratedOutcomes[0])
	}
	if got := anyToInt(migratedSnapshot["outcome_sample_count"], 0); got != 1 {
		t.Fatalf("migrated legacy outcome was not observable: %#v", migratedSnapshot)
	}
	for name, got := range map[string]int{
		"calibration outcome count":  anyToInt(migratedSnapshot["calibration_outcome_sample_count"], 0),
		"provider usage count":       anyToInt(migratedSnapshot["observed_provider_usage_count"], 0),
		"provider prompt tokens":     anyToInt(migratedSnapshot["observed_provider_prompt_tokens"], 0),
		"provider completion tokens": anyToInt(migratedSnapshot["observed_provider_completion_tokens"], 0),
		"provider total tokens":      anyToInt(migratedSnapshot["observed_provider_total_tokens"], 0),
	} {
		if got != 0 {
			t.Fatalf("migrated legacy outcome affected %s: %#v", name, migratedSnapshot)
		}
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, privateTopic := range []string{"private/customer-alpha/roadmap", "private/customer-alpha/outcome", "adapter_complete", "\"topic_path\""} {
		if strings.Contains(string(raw), privateTopic) {
			t.Fatalf("migrated ledger retained private topic material %q", privateTopic)
		}
	}
	migratedRows, parseErrors, err := ledger.readRowsUnlocked()
	if err != nil || parseErrors != 0 || len(migratedRows) != len(legacyRows) {
		t.Fatalf("migration changed durable row cardinality: rows=%d parse_errors=%d err=%v", len(migratedRows), parseErrors, err)
	}
	for index, expected := range legacyRows {
		got := migratedRows[index]
		if anyToString(got["schema_id"]) != anyToString(expected["schema_id"]) {
			t.Fatalf("migration changed row order at %d: got=%#v want=%#v", index, got, expected)
		}
		want := cloneJSONMap(expected)
		topicPath := anyToString(want["topic_path"])
		delete(want, "topic_path")
		if topicPath == "" {
			delete(want, "topic_ref")
		} else {
			want["topic_ref"] = contextPackQualityTopicRef(anyToString(want["project"]), topicPath)
		}
		if anyToString(want["schema_id"]) == contextPackQualityOutcomeSchemaID {
			want["outcome_source"] = wantOutcomeSourceRef
		}
		if contextPackCanonicalJSON(t, got) != contextPackCanonicalJSON(t, want) {
			t.Fatalf("migration did not preserve quality receipt/payload at %d: got=%#v want=%#v", index, got, want)
		}
	}
	writesBeforeIdempotentCheck := writes
	if err := telemetry.migratePersistedTopicPrivacy(); err != nil {
		t.Fatalf("idempotent migration retry: %v", err)
	}
	if writes != writesBeforeIdempotentCheck {
		t.Fatalf("already-migrated ledger rewrote on retry: before=%d after=%d", writesBeforeIdempotentCheck, writes)
	}

	// Historical outcome reporters also migrate when no topic row happens to be
	// present; the live ledger must not rely on an unrelated topic rewrite to
	// cross the privacy boundary.
	sourceOnlyPath := filepath.Join(t.TempDir(), "context-pack-quality-source-only.ndjson")
	sourceOnlyRow := cloneJSONMap(legacyRows[1])
	delete(sourceOnlyRow, "topic_path")
	writeContextPackTopicMigrationRows(t, sourceOnlyPath, []map[string]any{sourceOnlyRow})
	sourceOnly := newContextPackQualityTelemetryWithLedger(20, contextPackTopicMigrationLedger(sourceOnlyPath))
	sourceOnlyRaw, err := os.ReadFile(sourceOnlyPath)
	if err != nil || strings.Contains(string(sourceOnlyRaw), "adapter_complete") || !strings.Contains(string(sourceOnlyRaw), wantOutcomeSourceRef) || !contextPackQualityLedgerAvailable(sourceOnly.ledger) {
		t.Fatalf("source-only reporter migration failed: err=%v snapshot=%#v", err, sourceOnly.snapshot())
	}
	sourceOnlySnapshot := sourceOnly.snapshot()
	sourceOnlyOutcomes := parseRows(sourceOnlySnapshot["outcomes"])
	if len(sourceOnlyOutcomes) != 1 || anyToString(sourceOnlyOutcomes[0]["quality_sample_admission"]) != "legacy_ineligible" || anyToBool(sourceOnlyOutcomes[0]["calibration_eligible"]) {
		t.Fatalf("source-only migration granted legacy outcome credit: %#v", sourceOnlySnapshot)
	}
	for name, got := range map[string]int{
		"calibration outcomes": anyToInt(sourceOnlySnapshot["calibration_outcome_sample_count"], 0),
		"provider usage":       anyToInt(sourceOnlySnapshot["observed_provider_usage_count"], 0),
		"provider tokens":      anyToInt(sourceOnlySnapshot["observed_provider_total_tokens"], 0),
	} {
		if got != 0 {
			t.Fatalf("source-only migration affected %s: %#v", name, sourceOnlySnapshot)
		}
	}

	// Already-safe reporter forms are not migration triggers. Exercise the
	// migration method directly so the constructor's independent durability
	// acknowledgement rewrite cannot obscure an unnecessary privacy rewrite.
	for name, source := range map[string]string{
		"agent-report": "agent_report",
		"opaque-ref":   "sha256:" + sha256Hex("already-opaque-reporter"),
	} {
		t.Run("accepted-source-"+name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
			row := cloneJSONMap(sourceOnlyRow)
			row["outcome_source"] = source
			original := writeContextPackTopicMigrationRows(t, path, []map[string]any{row})
			ledger := contextPackTopicMigrationLedger(path)
			writes := 0
			ledger.writeFile = func(string, []byte, bool) error {
				writes++
				return nil
			}
			telemetry := &contextPackQualityTelemetry{ledger: ledger}
			if err := telemetry.migratePersistedTopicPrivacy(); err != nil {
				t.Fatalf("accepted reporter failed migration preflight: %v", err)
			}
			raw, err := os.ReadFile(path)
			if err != nil || writes != 0 || !bytes.Equal(raw, original) {
				t.Fatalf("accepted reporter triggered rewrite: writes=%d err=%v", writes, err)
			}
		})
	}

	// A pre-commit failure must retain the original unsafe bytes and refuse to
	// load them into a public snapshot rather than attempting a partial rewrite.
	precommitPath := filepath.Join(t.TempDir(), "context-pack-quality-precommit.ndjson")
	precommitOriginal := writeContextPackTopicMigrationRows(t, precommitPath, legacyRows)
	precommitLedger := contextPackTopicMigrationLedger(precommitPath)
	precommitLedger.writeFile = func(string, []byte, bool) error { return errors.New("injected precommit migration failure") }
	precommit := newContextPackQualityTelemetryWithLedger(20, precommitLedger)
	if len(precommit.samples) != 0 || len(precommit.outcomes) != 0 || precommitLedger.lastError != "io_error" || contextPackQualityLedgerAvailable(precommitLedger) {
		t.Fatalf("precommit migration failure did not fail closed: samples=%#v outcomes=%#v ledger=%#v", precommit.samples, precommit.outcomes, precommitLedger)
	}
	precommitRaw, err := os.ReadFile(precommitPath)
	if err != nil || !bytes.Equal(precommitRaw, precommitOriginal) {
		t.Fatalf("precommit migration failure replaced source bytes: err=%v", err)
	}
	precommit.recordQuality(map[string]any{"sample_id": "cpq_blocked_after_migration", "project": "contextlattice", "quality_score": 1})
	if afterBlockedWrite, readErr := os.ReadFile(precommitPath); readErr != nil || !bytes.Equal(afterBlockedWrite, precommitOriginal) {
		t.Fatalf("failed migration ledger accepted a later append: err=%v", readErr)
	}

	// A post-rename acknowledgement failure has unknown durability. The current
	// process remains fail-closed; the next healthy startup re-acknowledges only
	// the already-redacted bytes and restores the same ordered rows.
	committedPath := filepath.Join(t.TempDir(), "context-pack-quality-committed.ndjson")
	writeContextPackTopicMigrationRows(t, committedPath, legacyRows)
	committedLedger := contextPackTopicMigrationLedger(committedPath)
	committedLedger.writeFile = func(path string, content []byte, dedicatedParent bool) error {
		if err := writeOwnerOnlyDurableAtomicFile(path, content, dedicatedParent); err != nil {
			return err
		}
		return &ownerOnlyAtomicWriteError{Operation: "injected post-rename migration failure", Committed: true, Err: errors.New("directory sync unavailable")}
	}
	committed := newContextPackQualityTelemetryWithLedger(20, committedLedger)
	if len(committed.samples) != 0 || len(committed.outcomes) != 0 {
		t.Fatalf("committed migration uncertainty loaded public rows: %#v", committed.snapshot())
	}
	committedRaw, err := os.ReadFile(committedPath)
	if err != nil || strings.Contains(string(committedRaw), "private/customer-alpha/roadmap") || strings.Contains(string(committedRaw), "adapter_complete") || strings.Contains(string(committedRaw), "\"topic_path\"") {
		t.Fatalf("committed migration did not leave only redacted bytes: err=%v raw=%s", err, committedRaw)
	}
	restarted := newContextPackQualityTelemetryWithLedger(20, contextPackTopicMigrationLedger(committedPath))
	if len(restarted.samples) != 2 || len(restarted.outcomes) != 1 {
		t.Fatalf("healthy restart did not load redacted migration rows: %#v", restarted.snapshot())
	}

	// Any malformed legacy topic is a stop condition: no row can be silently
	// dropped or replaced while its intended opaque identity is ambiguous.
	malformedPath := filepath.Join(t.TempDir(), "context-pack-quality-malformed.ndjson")
	malformedRows := contextPackTopicMigrationRows()
	malformedRows[0]["topic_path"] = 7
	malformedOriginal := writeContextPackTopicMigrationRows(t, malformedPath, malformedRows)
	malformed := newContextPackQualityTelemetryWithLedger(20, contextPackTopicMigrationLedger(malformedPath))
	malformedRaw, err := os.ReadFile(malformedPath)
	if err != nil || !bytes.Equal(malformedRaw, malformedOriginal) || len(malformed.samples) != 0 || len(malformed.outcomes) != 0 {
		t.Fatalf("malformed topic migration was not atomic/fail-closed: err=%v snapshot=%#v", err, malformed.snapshot())
	}
	if malformed.ledger.lastError != "privacy_migration_failed" {
		t.Fatalf("malformed topic migration reported wrong error class: %q", malformed.ledger.lastError)
	}

	// A whole-file migration may not silently discard valid future-schema rows
	// that the current runtime projection does not yet understand.
	unknownSchemaPath := filepath.Join(t.TempDir(), "context-pack-quality-unknown-schema.ndjson")
	unknownSchemaRows := contextPackTopicMigrationRows()
	unknownSchemaRow := map[string]any{
		"schema_id": "contextlattice_future_quality.v2", "version": 2, "opaque_state": "retained",
	}
	unknownSchemaRows = append([]map[string]any{unknownSchemaRows[0], unknownSchemaRow}, unknownSchemaRows[1:]...)
	unknownSchemaOriginal := writeContextPackTopicMigrationRows(t, unknownSchemaPath, unknownSchemaRows)
	unknownSchema := newContextPackQualityTelemetryWithLedger(20, contextPackTopicMigrationLedger(unknownSchemaPath))
	unknownSchemaRaw, err := os.ReadFile(unknownSchemaPath)
	if err != nil || !bytes.Equal(unknownSchemaRaw, unknownSchemaOriginal) || contextPackQualityLedgerAvailable(unknownSchema.ledger) || unknownSchema.ledger.lastError != "privacy_migration_failed" {
		t.Fatalf("unknown schema was not retained byte-for-byte behind fail-closed migration: err=%v snapshot=%#v", err, unknownSchema.snapshot())
	}

	unsafeOutcomePath := filepath.Join(t.TempDir(), "context-pack-quality-unsafe-outcome.ndjson")
	unsafeRows := contextPackTopicMigrationRows()
	unsafeRows[1]["utility"] = map[string]any{"value": 4, "unit": "acceptance_points", "raw_query": "/private/unsafe-outcome-query"}
	unsafeOriginal := writeContextPackTopicMigrationRows(t, unsafeOutcomePath, unsafeRows)
	unsafe := newContextPackQualityTelemetryWithLedger(20, contextPackTopicMigrationLedger(unsafeOutcomePath))
	unsafeRaw, err := os.ReadFile(unsafeOutcomePath)
	if err != nil || !bytes.Equal(unsafeRaw, unsafeOriginal) || len(unsafe.samples) != 0 || len(unsafe.outcomes) != 0 || contextPackQualityLedgerAvailable(unsafe.ledger) {
		t.Fatalf("unsafe outcome migration did not latch fail-closed: err=%v snapshot=%#v", err, unsafe.snapshot())
	}
	assertLegacyOutcomeRejected := func(name string, mutate func(map[string]any)) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "context-pack-quality-unsafe-"+name+".ndjson")
		rows := contextPackTopicMigrationRows()
		mutate(rows[1])
		original := writeContextPackTopicMigrationRows(t, path, rows)
		telemetry := newContextPackQualityTelemetryWithLedger(20, contextPackTopicMigrationLedger(path))
		raw, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(raw, original) || len(telemetry.samples) != 0 || len(telemetry.outcomes) != 0 || contextPackQualityLedgerAvailable(telemetry.ledger) || telemetry.ledger.lastError != "privacy_migration_failed" {
			t.Fatalf("unsafe legacy outcome %s was not rejected without replacement: err=%v snapshot=%#v", name, readErr, telemetry.snapshot())
		}
	}
	assertLegacyOutcomeRejected("compact-identifier", func(row map[string]any) {
		row["utility"] = map[string]any{"value": 4, "unit": "sk_live_customer_omega_7f3a9c"}
	})
	assertLegacyOutcomeRejected("unknown-attribution", func(row map[string]any) {
		row["evidence_attribution"] = []any{map[string]any{
			"entity_type": "file", "entity_id": "sha256:" + sha256Hex("legacy-file"), "entity_ref": "sha256:" + sha256Hex("legacy-file-ref"),
			"attribution_method": "counterfactual", "summary": "private customer finding",
		}}
	})
	assertLegacyOutcomeRejected("noncandidate-candidate-ref", func(row map[string]any) {
		row["evidence_attribution"] = []any{map[string]any{
			"entity_type": "file", "entity_id": "sha256:" + sha256Hex("legacy-file"), "entity_ref": "sha256:" + sha256Hex("legacy-file-ref"),
			"candidate_ref": "sk_live_customer_omega_7f3a9c", "attribution_method": "counterfactual", "role": "support",
		}}
	})
	assertLegacyOutcomeRejected("top-level-content", func(row map[string]any) {
		row["raw_content"] = "private customer content"
	})
	assertLegacyOutcomeRejected("top-level-summary", func(row map[string]any) {
		row["summary"] = "sk_live_customer_omega_7f3a9c"
	})
	assertLegacyOutcomeRejected("top-level-notes", func(row map[string]any) {
		row["notes"] = "sk_live_customer_omega_7f3a9c"
	})
	assertLegacyOutcomeRejected("top-level-result", func(row map[string]any) {
		row["result"] = "sk_live_customer_omega_7f3a9c"
	})
	assertLegacyOutcomeRejected("economics-summary", func(row map[string]any) {
		row["economics"] = map[string]any{
			"latency_ms": 12, "summary": "sk_live_customer_omega_7f3a9c",
		}
	})
	assertLegacyOutcomeRejected("candidate-attempts-notes", func(row map[string]any) {
		row["candidate_attribution_attempts"] = map[string]any{
			"received": 1, "invalid_ref": 0, "notes": "sk_live_customer_omega_7f3a9c",
		}
	})
	assertLegacyOutcomeRejected("attribution-binding-summary", func(row map[string]any) {
		row["attribution_binding"] = map[string]any{
			"summary": "sk_live_customer_omega_7f3a9c",
		}
	})
	assertLegacyOutcomeRejected("attribution-binding-receipt-map", func(row map[string]any) {
		row["attribution_binding"] = map[string]any{
			"schema_id": contextPackOutcomeBindingSchemaID, "version": 1, "sample_id_present": false,
			"candidate_attribution_received": 0, "candidate_attribution_bound": 0, "candidate_attribution_rejected": 0,
			"legacy_unbound_count": 0, "selection_receipt_id": map[string]any{"notes": "sk_live_customer_omega_7f3a9c"},
			"selection_receipt_digest": "", "exclusions": map[string]any{},
		}
	})
	assertLegacyOutcomeRejected("stability-notes", func(row map[string]any) {
		row["stability"] = map[string]any{
			"stable": true, "run_count": 1, "external_state": false,
			"notes": "sk_live_customer_omega_7f3a9c",
		}
	})
	assertLegacyOutcomeRejected("invalid-captured-at", func(row map[string]any) {
		row["capturedAt"] = "not-a-rfc3339-timestamp"
	})
	assertLegacyOutcomeRejected("string-counter", func(row map[string]any) {
		row["retry_count"] = "1"
	})
	assertLegacyOutcomeRejected("wrong-bool", func(row map[string]any) {
		row["first_pass_success"] = "true"
	})
	assertLegacyOutcomeRejected("invalid-policy-enum", func(row map[string]any) {
		row["policy_arm"] = "exploit"
	})
	assertLegacyOutcomeRejected("legacy-captured-at-alias", func(row map[string]any) {
		row["captured_at"] = "2026-08-04T00:00:00Z"
	})
	assertLegacyOutcomeRejected("admitted-raw-reporter", func(row map[string]any) {
		row["quality_sample_admission"] = contextPackOutcomeAdmissionSchemaID
		row["quality_sample_admission_ref"] = "sha256:" + sha256Hex("admitted-raw-reporter")
	})
	assertLegacyOutcomeRejected("unscoped-raw-reporter", func(row map[string]any) {
		row["project"] = ""
	})
	assertLegacyOutcomeRejected("raw-reporter-wrong-type", func(row map[string]any) {
		row["outcome_source"] = 7
	})
	assertLegacyOutcomeRejected("raw-reporter-empty", func(row map[string]any) {
		row["outcome_source"] = ""
	})
	assertLegacyOutcomeRejected("raw-reporter-noncanonical", func(row map[string]any) {
		row["outcome_source"] = "adapter complete"
	})
	assertLegacyOutcomeRejected("raw-reporter-over-limit", func(row map[string]any) {
		row["outcome_source"] = strings.Repeat("a", 81)
	})
}

func TestContextPackQualityLegacyOutcomeScalarValidationRejectsNonFiniteUtility(t *testing.T) {
	base := contextPackTopicMigrationRows()[1]
	delete(base, "topic_path")
	base["outcome_source"] = "agent_report"
	for name, value := range map[string]any{
		"nan":       math.NaN(),
		"infinity":  math.Inf(1),
		"overbound": 1_000_000.000001,
	} {
		t.Run(name, func(t *testing.T) {
			row := cloneJSONMap(base)
			row["utility"] = map[string]any{"value": value}
			if !contextPackQualityLegacyOutcomeIdentifiersUnsafe(row) {
				t.Fatalf("non-finite or out-of-bounds utility value was accepted: %T", value)
			}
		})
	}
	for name, mutate := range map[string]func(map[string]any){
		"utility-wrong-bool": func(row map[string]any) { row["utility"] = map[string]any{"verification_passed": "true"} },
		"pairing-wrong-bool": func(row map[string]any) { row["pairing"] = map[string]any{"leakage_free": "true"} },
	} {
		t.Run(name, func(t *testing.T) {
			row := cloneJSONMap(base)
			mutate(row)
			if !contextPackQualityLegacyOutcomeIdentifiersUnsafe(row) {
				t.Fatalf("wrong boolean scalar was accepted")
			}
		})
	}
}

func TestContextPackQualityLegacyEmptyProjectIsObservableOnly(t *testing.T) {
	legacy := contextPackTopicMigrationRows()[1]
	delete(legacy, "topic_path")
	legacy["project"] = ""
	legacy["outcome_source"] = "agent_report"
	if contextPackQualityLegacyOutcomeIdentifiersUnsafe(legacy) {
		t.Fatalf("unadmitted observable-only legacy outcome with an empty project was rejected: %#v", legacy)
	}
	admitted := cloneJSONMap(legacy)
	admitted["quality_sample_admission"] = contextPackOutcomeAdmissionSchemaID
	admitted["quality_sample_admission_ref"] = "sha256:" + sha256Hex("admitted-empty-project")
	if !contextPackQualityLegacyOutcomeIdentifiersUnsafe(admitted) {
		t.Fatalf("admitted outcome with an empty project was accepted: %#v", admitted)
	}
	creditBearing := cloneJSONMap(legacy)
	creditBearing["utility"] = map[string]any{"value": 1}
	if !contextPackQualityLegacyOutcomeIdentifiersUnsafe(creditBearing) {
		t.Fatalf("credit-bearing outcome with an empty project was accepted: %#v", creditBearing)
	}
}

func TestContextPackRegressionFixtureStoreMaterializesEmptyAndPreservesExisting(t *testing.T) {
	t.Run("materializes-empty-owner-only-file", func(t *testing.T) {
		root := t.TempDir()
		qualityPath := filepath.Join(root, "quality.ndjson")
		fixturePath := filepath.Join(root, "fixtures.ndjson")
		t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
		t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", qualityPath)
		t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED", "true")
		t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", fixturePath)

		telemetry := newContextPackQualityTelemetry(20)
		info, err := os.Stat(fixturePath)
		if err != nil {
			t.Fatalf("missing fixture sidecar: %v", err)
		}
		if info.Size() != 0 {
			t.Fatalf("fixture sidecar not empty: size=%d", info.Size())
		}
		assertMode(t, fixturePath, ownerOnlyFileMode)
		status := contextPackRegressionFixtureOperationalStatus(telemetry.regressionFixtures)
		if !anyToBool(status["configured"]) || !anyToBool(status["enabled"]) || !anyToBool(status["healthy"]) || anyToString(status["error_code"]) != "" || anyToInt(status["fixture_count"], -1) != 0 || anyToInt(status["bytes"], -1) != 0 {
			t.Fatalf("materialized sidecar status unhealthy: %#v", status)
		}
	})

	t.Run("preserves-existing-fixtures", func(t *testing.T) {
		root := t.TempDir()
		qualityPath := filepath.Join(root, "quality.ndjson")
		fixturePath := filepath.Join(root, "fixtures.ndjson")
		t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
		t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", qualityPath)
		t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED", "true")
		t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", fixturePath)

		fixture, ref := contextPackQualityNormalizedRegressionFixture(map[string]any{
			"query": "private existing fixture", "project": "contextlattice", "expected_files": []string{"notes/expected.md"},
		})
		row, err := json.Marshal(map[string]any{
			"schema_id": contextPackRegressionFixtureSchemaID, "version": 1, "fixture_ref": ref, "fixture": fixture,
		})
		if err != nil {
			t.Fatal(err)
		}
		original := append(row, '\n')
		if err := writeOwnerOnlyDurableAtomicFile(fixturePath, original, false); err != nil {
			t.Fatal(err)
		}
		telemetry := newContextPackQualityTelemetry(20)
		after, err := os.ReadFile(fixturePath)
		if err != nil || !bytes.Equal(after, original) {
			t.Fatalf("existing fixture sidecar was replaced: err=%v", err)
		}
		status := contextPackRegressionFixtureOperationalStatus(telemetry.regressionFixtures)
		if !anyToBool(status["healthy"]) || anyToInt(status["fixture_count"], 0) != 1 {
			t.Fatalf("existing fixture sidecar did not reload: %#v", status)
		}
	})

	for name, prepareInvalidPath := range map[string]func(*testing.T, string){
		"symlink": func(t *testing.T, fixturePath string) {
			targetPath := filepath.Join(t.TempDir(), "target.ndjson")
			original := []byte("private-target-sentinel\n")
			if err := os.WriteFile(targetPath, original, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(targetPath, fixturePath); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			t.Cleanup(func() {
				after, err := os.ReadFile(targetPath)
				if err != nil || !bytes.Equal(after, original) {
					t.Errorf("symlink target changed: err=%v", err)
				}
				assertMode(t, targetPath, 0o644)
			})
		},
		"directory": func(t *testing.T, fixturePath string) {
			if err := os.Mkdir(fixturePath, 0o700); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run("rejects-"+name+"-path", func(t *testing.T) {
			root := t.TempDir()
			qualityPath := filepath.Join(root, "quality.ndjson")
			fixturePath := filepath.Join(root, "fixtures.ndjson")
			prepareInvalidPath(t, fixturePath)
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
			t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", qualityPath)
			t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED", "true")
			t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", fixturePath)

			telemetry := newContextPackQualityTelemetry(20)
			if telemetry.regressionFixtures == nil || telemetry.regressionFixtures.enabled || telemetry.regressionFixtures.lastError == "" {
				t.Fatalf("invalid sidecar path did not fail closed: %#v", telemetry.regressionFixtures)
			}
			info, err := os.Lstat(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			if name == "symlink" && info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("symlink path was replaced: mode=%v", info.Mode())
			}
			if name == "directory" && !info.IsDir() {
				t.Fatalf("directory path was replaced: mode=%v", info.Mode())
			}
		})
	}
}

func TestContextPackRegressionFixtureSidecarAdmitsOnlyAfterQualityAndRepairsByReplay(t *testing.T) {
	newFixture := func() map[string]any {
		return map[string]any{
			"query": "private regression query", "project": "contextlattice", "topic_path": "private/regression-topic",
			"expected_files": []string{"notes/expected.md"}, "negative_files": []string{"notes/negative.md"},
		}
	}
	newServer := func(t *testing.T) (*server, string, string) {
		t.Helper()
		ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
		fixturePath := filepath.Join(t.TempDir(), "context-pack-regression-fixtures.ndjson")
		t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
		t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
		t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", fixturePath)
		telemetry := newContextPackQualityTelemetry(20)
		telemetry.recordQuality(map[string]any{"sample_id": "cpq_fixture", "project": "contextlattice", "task_class": "coding"})
		return &server{contextPackQuality: telemetry}, ledgerPath, fixturePath
	}
	post := func(t *testing.T, s *server, payload map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
		return response
	}
	sidecarRows := func(t *testing.T, path string) int {
		t.Helper()
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Count(strings.TrimSpace(string(raw)), "\"fixture_ref\"")
	}

	// A second outcome for one authoritative quality sample is rejected before
	// the raw sidecar write, so it cannot leave an orphan fixture behind.
	s, _, fixturePath := newServer(t)
	if response := post(t, s, map[string]any{"outcome_id": "outcome_plain", "sample_id": "cpq_fixture", "project": "contextlattice", "first_pass_success": true}); response.Code != http.StatusOK {
		t.Fatalf("seed authoritative outcome: %d %s", response.Code, response.Body.String())
	}
	if response := post(t, s, map[string]any{"outcome_id": "outcome_conflict", "sample_id": "cpq_fixture", "project": "contextlattice", "first_pass_success": true, "regression_case": newFixture()}); response.Code != http.StatusConflict {
		t.Fatalf("quality conflict status=%d body=%s", response.Code, response.Body.String())
	}
	if got := sidecarRows(t, fixturePath); got != 0 {
		t.Fatalf("quality conflict wrote raw fixture rows=%d", got)
	}

	// Candidate receipt failure is likewise decided before any fixture write.
	s, _, fixturePath = newServer(t)
	s.contextPackQuality.recordQuality(map[string]any{"sample_id": "cpq_receipt", "project": "contextlattice", "selection_receipt": map[string]any{
		"candidates": []any{map[string]any{"candidate_ref": outcomeIntelligenceCandidateRef("receipted"), "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision"}},
	}})
	if response := post(t, s, map[string]any{
		"outcome_id": "outcome_receipt_rejected", "sample_id": "cpq_receipt", "project": "contextlattice", "first_pass_success": true, "regression_case": newFixture(),
		"evidence_attribution": []any{map[string]any{"entity_type": "candidate", "candidate_ref": outcomeIntelligenceCandidateRef("not-receipted"), "attribution_method": "counterfactual"}},
	}); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("receipt rejection status=%d body=%s", response.Code, response.Body.String())
	}
	if got := sidecarRows(t, fixturePath); got != 0 {
		t.Fatalf("receipt rejection wrote raw fixture rows=%d", got)
	}

	// Sidecar failure comes after quality admission: only the opaque ref is
	// durable, no private fixture bytes exist, and an exact replay repairs it.
	s, ledgerPath, fixturePath := newServer(t)
	s.contextPackQuality.regressionFixtures.writeFile = func(string, []byte, bool) error { return errors.New("injected sidecar failure") }
	payload := map[string]any{"outcome_id": "outcome_fixture_repair", "sample_id": "cpq_fixture", "project": "contextlattice", "first_pass_success": true, "regression_case": newFixture(), "regression_partition": "train", "traffic_class": "user"}
	if response := post(t, s, payload); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("sidecar failure status=%d body=%s", response.Code, response.Body.String())
	}
	qualityRaw, err := os.ReadFile(ledgerPath)
	if err != nil || !strings.Contains(string(qualityRaw), "regression_case_ref") || strings.Contains(string(qualityRaw), "private regression query") {
		t.Fatalf("sidecar failure did not leave ref-only quality outcome: err=%v raw=%s", err, qualityRaw)
	}
	if got := sidecarRows(t, fixturePath); got != 0 {
		t.Fatalf("failed sidecar write left private rows=%d", got)
	}
	s.contextPackQuality.regressionFixtures.writeFile = writeOwnerOnlyDurableAtomicFile
	if response := post(t, s, payload); response.Code != http.StatusOK {
		t.Fatalf("exact replay did not repair fixture sidecar: %d %s", response.Code, response.Body.String())
	}
	if got := sidecarRows(t, fixturePath); got != 1 {
		t.Fatalf("repaired sidecar rows=%d, want 1", got)
	}
	rows := s.contextPackQuality.derivedRegressionSourceRows(10)
	if len(rows) != 1 || len(anyMap(anyMap(rows[0])["regression_case"])) == 0 {
		t.Fatalf("owner-only fixture did not rejoin exact quality ref: %#v", rows)
	}
	restarted := newContextPackQualityTelemetry(20)
	restartedRows := restarted.derivedRegressionSourceRows(10)
	if len(restartedRows) != 1 || len(anyMap(anyMap(restartedRows[0])["regression_case"])) == 0 {
		t.Fatalf("sidecar fixture did not survive restart exact-ref join: %#v", restartedRows)
	}
}

func TestContextPackRegressionFixtureOperationalStatusIsBoundedAndMetadataOnly(t *testing.T) {
	qualityPath := filepath.Join(t.TempDir(), "quality.ndjson")
	fixturePath := filepath.Join(t.TempDir(), "fixtures.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", qualityPath)
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", fixturePath)
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_BYTES", "1")
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_FIXTURES", "1")

	telemetry := newContextPackQualityTelemetry(20)
	if _, _, err := telemetry.recordRegressionFixtureDetailed(map[string]any{
		"query": "owner-only status fixture", "project": "contextlattice", "topic_path": "private/status",
		"expected_files": []string{"notes/expected.md"}, "negative_files": []string{"notes/negative.md"},
	}); err != nil {
		t.Fatalf("record owner-only regression fixture: %v", err)
	}
	storage := anyMap(telemetry.snapshot()["storage"])
	status := anyMap(storage["regression_fixture_sidecar"])
	if !anyToBool(status["configured"]) || !anyToBool(status["enabled"]) || !anyToBool(status["healthy"]) ||
		anyToInt(status["fixture_count"], 0) != 1 || anyToInt64(status["bytes"], 0) <= 0 ||
		anyToInt64(status["max_bytes"], 0) != 64*1024 || anyToInt(status["max_fixtures"], 0) != 20 ||
		anyToString(status["error_code"]) != "" {
		t.Fatalf("unexpected bounded fixture status: %#v", status)
	}
	allowed := map[string]struct{}{
		"configured": {}, "enabled": {}, "healthy": {}, "fixture_count": {}, "bytes": {},
		"max_bytes": {}, "max_fixtures": {}, "error_code": {},
	}
	for key := range status {
		if _, ok := allowed[key]; !ok {
			t.Fatalf("fixture operational status exposed non-operational field %q: %#v", key, status)
		}
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{fixturePath, "owner-only status fixture", "private/status", "notes/expected.md"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("fixture operational status leaked %q: %s", forbidden, encoded)
		}
	}

	disabled := anyMap(defaultContextPackQualityTelemetrySnapshot(nil, nil)["storage"])["regression_fixture_sidecar"]
	if disabledStatus := anyMap(disabled); anyToBool(disabledStatus["configured"]) || anyToBool(disabledStatus["enabled"]) || anyToBool(disabledStatus["healthy"]) ||
		anyToInt(disabledStatus["fixture_count"], 0) != 0 || anyToInt64(disabledStatus["bytes"], 0) != 0 || anyToString(disabledStatus["error_code"]) != "" {
		t.Fatalf("default fixture operational status was not disabled and empty: %#v", disabledStatus)
	}

	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED", "false")
	disabledTelemetry := newContextPackQualityTelemetry(20)
	disabledStatus := anyMap(anyMap(disabledTelemetry.snapshot()["storage"])["regression_fixture_sidecar"])
	if !anyToBool(disabledStatus["configured"]) || anyToBool(disabledStatus["enabled"]) || anyToBool(disabledStatus["healthy"]) ||
		anyToInt(disabledStatus["fixture_count"], 0) != 0 || anyToInt64(disabledStatus["max_bytes"], 0) != 64*1024 ||
		anyToInt(disabledStatus["max_fixtures"], 0) != 20 || anyToString(disabledStatus["error_code"]) != "" {
		t.Fatalf("disabled fixture configuration exposed incorrect safe status: %#v", disabledStatus)
	}
}

func TestContextPackCompiledEvidencePersistsReceiptAndBindsOutcome(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	allocation := contextPackRankedEvidence("verified release gate", map[string]any{
		"relevant_decisions": []any{map[string]any{
			"text":    "The verified release gate requires a local validation receipt.",
			"project": "contextlattice", "file": "notes/release.md", "source": "qdrant",
		}},
	}, contextPackTokenBudget{})
	if len(allocation.RankedEvidence) == 0 {
		t.Fatalf("normal context-pack compilation produced no ranked evidence: %#v", allocation)
	}
	sample := buildContextPackQualitySample(contextPackQualitySampleInput{
		Query: "verified release gate", Project: "contextlattice", TopicPath: "runbooks/release",
		RankedEvidence: allocation.RankedEvidence, OmittedSelectionRefs: allocation.OmittedSelectionRefs,
	})
	receipt := anyMap(sample["selection_receipt"])
	candidates := parseRows(receipt["candidates"])
	if len(candidates) == 0 {
		t.Fatalf("compiled evidence did not reach the durable selection receipt: sample=%#v allocation=%#v", sample, allocation)
	}
	candidateRef := anyToString(candidates[0]["candidate_ref"])
	if contextPackOpaqueCandidateRef(candidateRef) == "" || anyToString(candidates[0]["selection_state"]) != "selected" {
		t.Fatalf("compiled selection receipt lost its opaque selected candidate: %#v", candidates[0])
	}

	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(sample)
	s := &server{contextPackQuality: telemetry}
	entry := contextPackQualityOutcomeFromSample(map[string]any{
		"outcome_id": "outcome_compiled_receipt", "sample_id": sample["sample_id"], "project": "contextlattice",
		"first_pass_success": true, "verification_passed": true,
		"verification_evidence_digest": "sha256:" + sha256Hex("compiled-receipt-proof"),
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": candidateRef, "attribution_method": "counterfactual",
		}},
	})
	bound := s.bindContextPackQualityOutcomeAttributions(entry)
	boundCandidates := parseRows(bound["evidence_attribution"])
	if len(boundCandidates) != 1 || anyToString(boundCandidates[0]["result_level_credit"]) != "selection_receipt_bound" {
		t.Fatalf("compiled candidate did not bind through the persisted receipt: %#v", bound)
	}
}

func TestContextPackOutcomeCandidateAttributionRequiresExactReceiptAndProject(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	selectedRef := outcomeIntelligenceCandidateRef("selected")
	omittedRef := outcomeIntelligenceCandidateRef("omitted")
	forgedRef := outcomeIntelligenceCandidateRef("forged")
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{
		"sample_id": "cpq_receipt", "project": "contextlattice",
		"selection_receipt": map[string]any{
			"schema_id": contextPackSelectionReceiptSchemaID, "version": 1,
			"candidates": []any{
				map[string]any{"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision"},
				map[string]any{"candidate_ref": omittedRef, "selection_state": "omitted", "ordinal": 1, "evidence_kind": "risk"},
			},
		},
	})
	s := &server{contextPackQuality: telemetry}
	entry := contextPackQualityOutcomeFromSample(map[string]any{
		"outcome_id": "outcome_receipt", "sample_id": "cpq_receipt", "project": "contextlattice",
		"first_pass_success": true, "verification_passed": true,
		"verification_evidence_digest": "sha256:" + sha256Hex("verified"),
		"evidence_attribution": []any{
			map[string]any{"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual", "summary": "never copy raw"},
			map[string]any{"entity_type": "candidate", "candidate_ref": omittedRef, "attribution_method": "counterfactual"},
			map[string]any{"entity_type": "candidate", "candidate_ref": forgedRef, "attribution_method": "counterfactual"},
			map[string]any{"entity_type": "source", "entity_id": "qdrant", "attribution_method": "counterfactual"},
		},
	})
	bound := s.bindContextPackQualityOutcomeAttributions(entry)
	attribution := parseRows(bound["evidence_attribution"])
	if len(attribution) != 3 {
		t.Fatalf("expected selected, omitted, and legacy attributions only, got %#v", attribution)
	}
	if anyToString(attribution[0]["result_level_credit"]) != "selection_receipt_bound" || anyToString(attribution[0]["selection_state"]) != "selected" {
		t.Fatalf("selected candidate was not receipt-bound: %#v", attribution[0])
	}
	if anyToString(attribution[1]["result_level_credit"]) != "selection_receipt_bound" || anyToString(attribution[1]["selection_state"]) != "omitted" {
		t.Fatalf("omitted candidate was not receipt-bound: %#v", attribution[1])
	}
	if anyToString(attribution[2]["result_level_credit"]) != "unbound_legacy" {
		t.Fatalf("legacy source attribution was not labelled unbound: %#v", attribution[2])
	}
	binding := anyMap(bound["attribution_binding"])
	if anyToInt(binding["candidate_attribution_bound"], 0) != 2 || anyToInt(anyMap(binding["exclusions"])["candidate_not_receipted"], 0) != 1 {
		t.Fatalf("forged candidate was not explicitly excluded: %#v", binding)
	}
	if anyToString(binding["selection_receipt_id"]) == "" || !strings.HasPrefix(anyToString(binding["selection_receipt_digest"]), "sha256:") {
		t.Fatalf("exact receipt set was not carried into attribution binding: %#v", binding)
	}

	crossProject := cloneJSONMap(entry)
	crossProject["project"] = "other-project"
	crossProject = s.bindContextPackQualityOutcomeAttributions(crossProject)
	for _, row := range parseRows(crossProject["evidence_attribution"]) {
		if anyToString(row["entity_type"]) == "candidate" {
			t.Fatalf("cross-project candidate received result-level credit: %#v", crossProject)
		}
	}
	if anyToInt(anyMap(anyMap(crossProject["attribution_binding"])["exclusions"])["candidate_project_mismatch"], 0) != 3 {
		t.Fatalf("cross-project exclusions are not explicit and bounded: %#v", crossProject)
	}

	caseNormalized := cloneJSONMap(entry)
	caseNormalized["project"] = "ContextLattice"
	caseNormalized = s.bindContextPackQualityOutcomeAttributions(caseNormalized)
	if got := anyToInt(anyMap(caseNormalized["attribution_binding"])["candidate_attribution_bound"], 0); got != 2 {
		t.Fatalf("case-normalized project scope rejected same project: %#v", caseNormalized)
	}
}

func TestContextPackOutcomeInvalidOnlyCandidateAttributionIsExplicitlyBounded(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "false")
	invalidRef := "not-an-opaque-candidate-ref-and-never-persist-this"
	entry := contextPackQualityOutcomeFromSample(map[string]any{
		"outcome_id": "outcome_invalid_candidate", "sample_id": "cpq_invalid", "project": "contextlattice", "first_pass_success": true,
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": invalidRef, "attribution_method": "counterfactual",
			"summary": "raw-candidate-text-must-not-cross", "trusted": true, "authorization": "never-copy",
		}},
	})
	bound := (&server{}).bindContextPackQualityOutcomeAttributions(entry)
	if len(parseRows(bound["evidence_attribution"])) != 0 {
		t.Fatalf("invalid candidate attribution was retained: %#v", bound)
	}
	binding := anyMap(bound["attribution_binding"])
	if anyToInt(binding["candidate_attribution_received"], 0) != 1 || anyToInt(binding["candidate_attribution_rejected"], 0) != 1 ||
		anyToInt(anyMap(binding["exclusions"])["candidate_ref_invalid"], 0) != 1 {
		t.Fatalf("invalid candidate attempt did not get a bounded explicit exclusion: %#v", bound)
	}
	encoded, err := json.Marshal(bound)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{invalidRef, "raw-candidate-text-must-not-cross", "\"trusted\"", "authorization"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("invalid candidate attempt leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestContextPackSelectionReceiptRecognizesEmittedEvidenceKinds(t *testing.T) {
	capabilityRef := outcomeIntelligenceCandidateRef("capability")
	graphRef := outcomeIntelligenceCandidateRef("graph-neighbor")
	receipt := contextPackSelectionReceipt([]any{
		map[string]any{"candidate_id": capabilityRef, "kind": "capability"},
		map[string]any{"candidate_id": graphRef, "kind": "graph_neighbor"},
	}, nil)
	candidates := parseRows(receipt["candidates"])
	if len(candidates) != 2 || anyToString(candidates[0]["evidence_kind"]) != "capability" || anyToString(candidates[1]["evidence_kind"]) != "graph_neighbor" {
		t.Fatalf("existing context-pack evidence kinds were downgraded to unknown: %#v", receipt)
	}
}

func TestEvidenceReputationCandidateCreditNeedsBoundReceiptAndVerification(t *testing.T) {
	rows := []map[string]any{}
	candidateRef := outcomeIntelligenceCandidateRef("reputation")
	for index := 1; index <= 5; index++ {
		row := evidenceReputationTestOutcome(index, true, []string{"verifier-a", "verifier-b"}[index%2])
		digest := anyToString(row["verification_evidence_digest"])
		row["evidence_attribution"] = []any{map[string]any{
			"entity_type": "candidate", "entity_id": candidateRef, "candidate_ref": candidateRef,
			"result_level_credit": "selection_receipt_bound", "selection_state": "selected", "attribution_method": "counterfactual",
			"issuer": anyToString(row["verifier_id"]), "producer_agent_id": "retrieval_agent", "verifier_id": anyToString(row["verifier_id"]),
			"verification_evidence_digest": digest,
		}}
		row["candidate_utility_verification"] = map[string]any{
			"outcome_id": row["outcome_id"], "sample_id": row["sample_id"],
			"independently_verified": true, "verification_status": "verified",
			"evidence_digest": digest, "verifier_id": row["verifier_id"],
		}
		rows = append(rows, row)
	}
	unverified := cloneJSONMap(rows[0])
	unverified["outcome_id"] = "unverified-candidate"
	unverified["verification_passed"] = false
	rows = append(rows, unverified)
	unbound := cloneJSONMap(rows[0])
	unbound["outcome_id"] = "unbound-candidate"
	unbound["verification_evidence_digest"] = "sha256:" + sha256Hex("unbound")
	unbound["evidence_attribution"] = []any{map[string]any{
		"entity_type": "candidate", "entity_id": outcomeIntelligenceCandidateRef("unbound"), "attribution_method": "counterfactual",
		"issuer": "verifier-b", "producer_agent_id": "retrieval_agent", "verifier_id": "verifier-b",
		"verification_evidence_digest": unbound["verification_evidence_digest"],
	}}
	rows = append(rows, unbound)
	omitted := cloneJSONMap(rows[0])
	omitted["outcome_id"] = "omitted-candidate"
	omitted["verification_evidence_digest"] = "sha256:" + sha256Hex("omitted")
	omittedAttribution := parseRows(omitted["evidence_attribution"])[0]
	omittedAttribution["entity_id"] = outcomeIntelligenceCandidateRef("omitted-reputation")
	omittedAttribution["candidate_ref"] = omittedAttribution["entity_id"]
	omittedAttribution["selection_state"] = "omitted"
	omittedAttribution["verification_evidence_digest"] = omitted["verification_evidence_digest"]
	omitted["evidence_attribution"] = []any{omittedAttribution}
	omitted["candidate_utility_verification"] = map[string]any{
		"outcome_id": omitted["outcome_id"], "sample_id": omitted["sample_id"],
		"independently_verified": true, "verification_status": "verified",
		"evidence_digest": omitted["verification_evidence_digest"], "verifier_id": omitted["verifier_id"],
	}
	rows = append(rows, omitted)
	legacy := cloneJSONMap(rows[0])
	legacy["outcome_id"] = "legacy-source"
	legacy["verification_evidence_digest"] = "sha256:" + sha256Hex("legacy")
	legacy["evidence_attribution"] = []any{map[string]any{
		"entity_type": "source", "entity_id": "legacy-source", "attribution_method": "counterfactual",
		"issuer": "verifier-a", "producer_agent_id": "retrieval_agent", "verifier_id": "verifier-a",
		"verification_evidence_digest": legacy["verification_evidence_digest"],
	}}
	rows = append(rows, legacy)

	projection := buildEvidenceReputation(rows, evidenceReputationOptions{
		Project: "contextlattice", TaskClass: "coding", AsOf: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC), MinimumSamples: 5, MaxEntries: 10,
	})
	entries := parseRows(projection["entries"])
	if len(entries) != 2 {
		t.Fatalf("expected bound candidate and compatible legacy source only, got %#v", projection)
	}
	for _, row := range entries {
		if anyToString(row["entity_type"]) == "candidate" {
			if anyToString(row["result_level_credit"]) != "selection_receipt_bound" || anyToBool(anyMap(row["bounded_influence"])["applied"]) {
				t.Fatalf("candidate reputation escaped receipt/advisory boundary: %#v", row)
			}
		} else if anyToString(row["result_level_credit"]) != "unbound_legacy" {
			t.Fatalf("legacy source compatibility must remain explicitly unbound: %#v", row)
		}
	}
	exclusions := anyMap(anyMap(projection["summary"])["exclusions"])
	if anyToInt(exclusions["verification_missing"], 0) != 1 || anyToInt(exclusions["candidate_unbound_to_selection_receipt"], 0) != 1 || anyToInt(exclusions["candidate_not_selected"], 0) != 1 {
		t.Fatalf("candidate verification and binding exclusions are missing: %#v", exclusions)
	}
}

func TestContextPackOutcomeRouteBindsCandidateBeforeResponseAndPersistence(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	selectedRef := outcomeIntelligenceCandidateRef("route-selected")
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{
		"sample_id": "cpq_route", "project": "contextlattice",
		"selection_receipt": map[string]any{
			"candidates": []any{map[string]any{
				"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
			}},
		},
	})
	s := &server{contextPackQuality: telemetry}
	secret := "outcome-attribution-must-not-echo-raw-content"
	reporterCapturedAt := "2001-02-03T04:05:06Z"
	forgedGatewayReceipt := "1999-01-01T00:00:00Z"
	payload, err := json.Marshal(map[string]any{
		"outcome_id": "outcome_route", "sample_id": "cpq_route", "project": "contextlattice", "first_pass_success": true,
		"captured_at": reporterCapturedAt, "gateway_received_at": forgedGatewayReceipt,
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual", "summary": secret,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload))
	s.telemetryContextPackQualityOutcomeRoute(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("outcome route rejected receipt-bound attribution: status=%d body=%s", response.Code, response.Body.String())
	}
	result, err := parseJSONMap(response.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	stored := anyMap(result["outcome"])
	attrs := parseRows(stored["evidence_attribution"])
	if len(attrs) != 1 || anyToString(attrs[0]["result_level_credit"]) != "selection_receipt_bound" {
		t.Fatalf("handler did not use bound entry for response and storage: %#v", result)
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("handler response copied raw attribution content: %s", response.Body.String())
	}
	if got := anyToString(stored["gateway_received_at"]); got == "" || got == forgedGatewayReceipt || got == reporterCapturedAt {
		t.Fatalf("gateway receipt timestamp trusted reporter input: %#v", stored)
	}
	if _, err := time.Parse(time.RFC3339Nano, anyToString(stored["gateway_received_at"])); err != nil {
		t.Fatalf("gateway receipt timestamp is not normalized RFC3339: %v", err)
	}
	persisted := telemetry.outcomeSourceRows(10)
	if len(persisted) != 1 || anyToString(parseRows(persisted[0]["evidence_attribution"])[0]["result_level_credit"]) != "selection_receipt_bound" || anyToString(persisted[0]["gateway_received_at"]) != anyToString(stored["gateway_received_at"]) {
		t.Fatalf("handler persisted a different, unbound outcome: %#v", persisted)
	}
}

func TestContextPackCandidateOutcomeRouteFailsClosedWithoutDurableReceipt(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "false")
	selectedRef := outcomeIntelligenceCandidateRef("durability-disabled")
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{
		"sample_id": "cpq_durability_disabled", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	})
	s := &server{
		contextPackQuality: telemetry,
		utility:            &utilityTelemetry{limit: 20, observations: []map[string]any{}, byOutcome: map[string]int{}},
	}
	payload, err := json.Marshal(map[string]any{
		"outcome_id": "outcome_durability_disabled", "sample_id": "cpq_durability_disabled", "project": "contextlattice",
		"first_pass_success":   true,
		"evidence_attribution": []any{map[string]any{"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
	if got, want := response.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("status = %d, want %d: %s", got, want, response.Body.String())
	}
	if strings.Contains(response.Body.String(), selectedRef) || strings.Contains(response.Body.String(), "cpq_durability_disabled") {
		t.Fatalf("durability rejection exposed candidate receipt data: %s", response.Body.String())
	}
	if rows := telemetry.outcomeSourceRows(10); len(rows) != 0 {
		t.Fatalf("undurable candidate entered proof outcome ring: %#v", rows)
	}
	if got := len(s.utility.observations); got != 0 {
		t.Fatalf("undurable candidate reached Utility reconciliation: %d observations", got)
	}
}

func TestContextPackCandidateOutcomeWriteFailureRecoversWithoutDuplicateAppend(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	selectedRef := outcomeIntelligenceCandidateRef("durability-recovery")
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{
		"sample_id": "cpq_durability_recovery", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	})
	s := &server{
		contextPackQuality: telemetry,
		utility:            &utilityTelemetry{limit: 20, observations: []map[string]any{}, byOutcome: map[string]int{}},
	}
	payload, err := json.Marshal(map[string]any{
		"outcome_id": "outcome_durability_recovery", "sample_id": "cpq_durability_recovery", "project": "contextlattice",
		"first_pass_success":   true,
		"evidence_attribution": []any{map[string]any{"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ledger := telemetry.ledger
	ledger.mu.Lock()
	ledger.path = t.TempDir() // openOwnerOnlyAppend rejects a directory.
	ledger.mu.Unlock()
	failed := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(failed, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
	if got, want := failed.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("write failure status = %d, want %d: %s", got, want, failed.Body.String())
	}
	if len(telemetry.outcomeSourceRows(10)) != 0 || len(s.utility.observations) != 0 {
		t.Fatalf("failed candidate append reached in-memory proof or Utility: outcomes=%#v utility=%#v", telemetry.outcomeSourceRows(10), s.utility.observations)
	}
	failedStorage := contextPackQualityLedgerPublicStatus(ledger)
	if anyToString(failedStorage["last_error"]) == "" {
		t.Fatalf("failed append did not latch latest write state: %#v", failedStorage)
	}

	ledger.mu.Lock()
	ledger.path = ledgerPath
	ledger.mu.Unlock()
	recovered := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(recovered, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
	if got, want := recovered.Code, http.StatusOK; got != want {
		t.Fatalf("recovery status = %d, want %d: %s", got, want, recovered.Body.String())
	}
	recoveredStorage := contextPackQualityLedgerPublicStatus(ledger)
	if anyToString(recoveredStorage["last_error"]) != "" {
		t.Fatalf("successful retry did not clear the current ledger failure: %#v", recoveredStorage)
	}
	if got := len(telemetry.outcomeSourceRows(10)); got != 1 {
		t.Fatalf("recovery outcome count = %d, want 1", got)
	}
}

func TestContextPackCandidateOutcomeCommittedPruneFailureRequiresDurabilityReack(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	selectedRef := outcomeIntelligenceCandidateRef("committed-prune-reack")
	telemetry := newContextPackQualityTelemetry(20)
	// Retention walks newest-first. This intentionally oversized oldest row
	// forces compaction while leaving the receipt and candidate row retainable.
	telemetry.recordQuality(outcomeIntelligenceLargeQualitySample("old-retention-row", "committed-prune-filler"))
	receiptSample := map[string]any{
		"sample_id": "cpq_committed_prune", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	}
	telemetry.recordQuality(receiptSample)
	s := &server{
		contextPackQuality: telemetry,
		utility:            &utilityTelemetry{limit: 20, observations: []map[string]any{}, byOutcome: map[string]int{}},
	}
	payload, err := json.Marshal(map[string]any{
		"outcome_id": "outcome_committed_prune", "sample_id": "cpq_committed_prune", "project": "contextlattice",
		"first_pass_success": true, "captured_at": "2026-08-04T00:00:00Z",
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	preview := contextPackQualityOutcomeFromSample(map[string]any{
		"outcome_id": "outcome_committed_prune", "sample_id": "cpq_committed_prune", "project": "contextlattice",
		"first_pass_success": true, "captured_at": "2026-08-04T00:00:00Z",
		"gateway_received_at": "2026-08-04T00:00:00Z",
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual",
		}},
	})
	preview = s.bindContextPackQualityOutcomeAttributions(preview)
	preview, err = bindContextPackQualityOutcomeSample(preview, contextPackQualityEntryFromSample(receiptSample))
	if err != nil {
		t.Fatalf("bind preview to authoritative quality sample: %v", err)
	}
	if !contextPackOutcomeHasReceiptBoundCandidate(preview) {
		t.Fatalf("candidate preview did not bind to durable selection receipt: %#v", preview)
	}
	receiptRaw, err := json.Marshal(contextPackQualityEntryFromSample(receiptSample))
	if err != nil {
		t.Fatal(err)
	}
	previewRaw, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	ledger := telemetry.ledger
	ledger.mu.Lock()
	// Preserve the exact durable quality sample plus its bound outcome after
	// compaction. The oversized oldest row still forces the committed rewrite.
	ledger.maxBytes = int64(len(receiptRaw) + len(previewRaw) + 1024)
	failAcknowledgement := true
	writeCalls := 0
	ledger.writeFile = func(path string, content []byte, dedicatedParent bool) error {
		writeCalls++
		if err := writeOwnerOnlyDurableAtomicFile(path, content, dedicatedParent); err != nil {
			return err
		}
		if failAcknowledgement {
			return &ownerOnlyAtomicWriteError{
				Operation: "synthetic context-pack prune directory sync",
				Committed: true,
				Err:       errors.New("durability acknowledgement unavailable"),
			}
		}
		return nil
	}
	ledger.mu.Unlock()

	first := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(first, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
	if got, want := first.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("committed prune failure status = %d, want %d: %s", got, want, first.Body.String())
	}
	if rows := telemetry.outcomeSourceRows(10); len(rows) != 0 || len(s.utility.observations) != 0 {
		t.Fatalf("committed prune uncertainty entered proof or Utility: outcomes=%#v utility=%#v", rows, s.utility.observations)
	}
	ledger.mu.Lock()
	uncertain := ledger.durabilityUnacknowledged
	ledger.mu.Unlock()
	if !uncertain || writeCalls != 1 {
		t.Fatalf("committed prune state was not latched exactly once: uncertain=%t writes=%d", uncertain, writeCalls)
	}
	if raw, readErr := os.ReadFile(ledgerPath); readErr != nil ||
		!strings.Contains(string(raw), "outcome_committed_prune") ||
		!strings.Contains(string(raw), `"sample_id":"cpq_committed_prune"`) ||
		!strings.Contains(string(raw), `"selection_receipt"`) {
		t.Fatalf("forced prune did not retain the exact candidate/receipt pair: err=%v raw=%q", readErr, raw)
	}

	// A readable row is insufficient: while the durable writer still cannot
	// acknowledge replacement, the retry remains outside proof and Utility.
	second := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(second, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
	if got, want := second.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("unacknowledged retry status = %d, want %d: %s", got, want, second.Body.String())
	}
	if rows := telemetry.outcomeSourceRows(10); len(rows) != 0 || len(s.utility.observations) != 0 || writeCalls != 1 {
		t.Fatalf("readable unacknowledged replay entered proof or Utility: outcomes=%#v utility=%#v writes=%d", rows, s.utility.observations, writeCalls)
	}

	ledger.mu.Lock()
	failAcknowledgement = false
	ledger.mu.Unlock()
	if err := ledger.acknowledgeDurability(); err != nil {
		t.Fatalf("re-acknowledge committed prune ledger: %v", err)
	}
	recovered := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(recovered, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
	if got, want := recovered.Code, http.StatusOK; got != want {
		t.Fatalf("durability re-acknowledgement status = %d, want %d: %s", got, want, recovered.Body.String())
	}
	result, err := parseJSONMap(recovered.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if anyToBool(result["recorded"]) || !anyToBool(result["duplicate"]) {
		t.Fatalf("re-acknowledged canonical outcome must remain an idempotent replay: %#v", result)
	}
	rows := telemetry.outcomeSourceRows(10)
	if len(rows) != 1 || anyToString(rows[0]["outcome_id"]) != "outcome_committed_prune" || len(s.utility.observations) != 1 {
		t.Fatalf("re-acknowledged canonical row did not reach proof before Utility: outcomes=%#v utility=%#v", rows, s.utility.observations)
	}
	ledger.mu.Lock()
	uncertain = ledger.durabilityUnacknowledged
	ledger.mu.Unlock()
	if uncertain || writeCalls != 2 {
		t.Fatalf("successful acknowledgement did not clear uncertainty: uncertain=%t writes=%d", uncertain, writeCalls)
	}
}

func TestContextPackPruneDropsCandidateCreditWhenItsReceiptIsNoLongerRetained(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	selectedRef := outcomeIntelligenceCandidateRef("pruned-receipt-exclusion")
	telemetry := newContextPackQualityTelemetry(20)
	receiptSample := map[string]any{
		"sample_id": "cpq_pruned_receipt", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	}
	telemetry.recordQuality(receiptSample)
	s := &server{contextPackQuality: telemetry}
	entry := contextPackQualityOutcomeFromSample(map[string]any{
		"outcome_id": "outcome_pruned_receipt", "sample_id": "cpq_pruned_receipt", "project": "contextlattice",
		"first_pass_success": true, "captured_at": "2026-08-04T00:00:00Z", "gateway_received_at": "2026-08-04T00:00:00Z",
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual",
		}},
	})
	entry = s.bindContextPackQualityOutcomeAttributions(entry)
	canonicalReceiptSample, found, err := telemetry.durableQualitySampleForOutcome("cpq_pruned_receipt")
	if err != nil || !found {
		t.Fatalf("resolve authoritative receipt sample: found=%t err=%v", found, err)
	}
	entry, err = bindContextPackQualityOutcomeSample(entry, canonicalReceiptSample)
	if err != nil {
		t.Fatalf("bind prune outcome to authoritative quality sample: %v", err)
	}
	if !contextPackOutcomeHasReceiptBoundCandidate(entry) {
		t.Fatalf("candidate did not bind before bounded retention test: %#v", entry)
	}
	if recorded, err := s.recordContextPackQualityOutcomeDurably(entry); err != nil || !recorded {
		t.Fatalf("record candidate before receipt-drop compaction: recorded=%t err=%v", recorded, err)
	}

	// Arrange the post-outcome row to survive with the candidate while the
	// oldest receipt is dropped. The exact durable lookup must override the
	// still-populated in-memory receipt index.
	sacrificialSample := outcomeIntelligenceLargeScalarQualitySample("newer-retention-row")
	receiptRaw, err := json.Marshal(canonicalReceiptSample)
	if err != nil {
		t.Fatal(err)
	}
	entryRaw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	sacrificialRaw, err := json.Marshal(contextPackQualityEntryFromSample(sacrificialSample))
	if err != nil {
		t.Fatal(err)
	}
	if len(receiptRaw) <= 128 {
		t.Fatalf("receipt fixture is too small to force selective compaction: %d", len(receiptRaw))
	}
	ledger := telemetry.ledger
	ledger.mu.Lock()
	ledger.maxBytes = int64(len(entryRaw) + len(sacrificialRaw) + 128)
	ledger.mu.Unlock()
	telemetry.recordQuality(sacrificialSample)

	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "outcome_pruned_receipt") || strings.Contains(string(raw), `"selection_receipt"`) {
		t.Fatalf("fixture did not retain candidate while dropping its receipt: %s", raw)
	}
	if rows := telemetry.outcomeSourceRows(10); len(rows) != 1 || anyToString(rows[0]["outcome_id"]) != "outcome_pruned_receipt" {
		t.Fatalf("candidate proof fixture was not resident before durable filtering: %#v", rows)
	}
	if rows, binding := telemetry.receiptDurableOutcomeRows(10); len(rows) != 0 || anyToInt(binding["missing_receipt_outcome_count"], 0) != 1 {
		t.Fatalf("stale in-memory receipt index blessed a pruned candidate: rows=%#v binding=%#v", rows, binding)
	}
	reputation := s.evidenceReputationSnapshot("contextlattice", "", 2, 10)
	if got := anyToInt(anyMap(reputation["summary"])["input_row_count"], -1); got != 0 {
		t.Fatalf("evidence reputation consumed candidate without its retained receipt: %#v", reputation)
	}
}

func TestContextPackRestartRequiresDurabilityReackBeforeLoadingCandidateProof(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	selectedRef := outcomeIntelligenceCandidateRef("restart-durability-reack")
	telemetry := newContextPackQualityTelemetry(20)
	receiptSample := map[string]any{
		"sample_id": "cpq_restart_reack", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	}
	telemetry.recordQuality(receiptSample)
	original := &server{contextPackQuality: telemetry}
	initialPayload := map[string]any{
		"outcome_id": "outcome_restart_reack", "sample_id": "cpq_restart_reack", "project": "contextlattice",
		"first_pass_success": true, "captured_at": "2026-08-04T00:00:00Z",
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual",
		}},
	}
	initialRaw, err := json.Marshal(initialPayload)
	if err != nil {
		t.Fatal(err)
	}
	initialResponse := httptest.NewRecorder()
	original.telemetryContextPackQualityOutcomeRoute(initialResponse, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(initialRaw)))
	if initialResponse.Code != http.StatusOK {
		t.Fatalf("record restart fixture candidate: status=%d body=%s", initialResponse.Code, initialResponse.Body.String())
	}
	if raw, err := os.ReadFile(ledgerPath); err != nil || !strings.Contains(string(raw), "outcome_restart_reack") {
		t.Fatalf("restart fixture did not persist readable candidate bytes: err=%v raw=%q", err, raw)
	}

	// Simulate a new process: its fresh ledger has no in-memory uncertainty
	// latch, but its startup rewrite reports a post-rename acknowledgement
	// failure. It must not rebuild candidate proof merely from readable bytes.
	restartedLedger := &contextPackQualityLedger{
		enabled: true, path: ledgerPath, maxBytes: telemetry.ledger.maxBytes, maxSamples: telemetry.ledger.maxSamples,
	}
	writeCalls := 0
	failAcknowledgement := true
	restartedLedger.writeFile = func(path string, content []byte, dedicatedParent bool) error {
		writeCalls++
		if err := writeOwnerOnlyDurableAtomicFile(path, content, dedicatedParent); err != nil {
			return err
		}
		if failAcknowledgement {
			return &ownerOnlyAtomicWriteError{
				Operation: "synthetic restart directory sync", Committed: true,
				Err: errors.New("durability acknowledgement unavailable"),
			}
		}
		return nil
	}
	restarted := newContextPackQualityTelemetryWithLedger(20, restartedLedger)
	if rows := restarted.outcomeSourceRows(10); len(rows) != 0 {
		t.Fatalf("restart loaded candidate proof without acknowledged rewrite: %#v", rows)
	}
	restartedLedger.mu.Lock()
	uncertain := restartedLedger.durabilityUnacknowledged
	restartedLedger.mu.Unlock()
	if !uncertain || writeCalls != 1 {
		t.Fatalf("restart durability failure was not latched: uncertain=%t writes=%d", uncertain, writeCalls)
	}

	failAcknowledgement = false
	if err := restartedLedger.acknowledgeDurability(); err != nil {
		t.Fatalf("re-acknowledge restart quality ledger: %v", err)
	}
	restarted.loadPersistedRows()
	recovered := &server{
		contextPackQuality: restarted,
		utility:            &utilityTelemetry{limit: 20, observations: []map[string]any{}, byOutcome: map[string]int{}},
	}
	payload, err := json.Marshal(map[string]any{
		"outcome_id": "outcome_restart_reack", "sample_id": "cpq_restart_reack", "project": "contextlattice",
		"first_pass_success": true, "captured_at": "2026-08-04T00:00:00Z",
		"evidence_attribution": []any{map[string]any{
			"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	recovered.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
	if got, want := response.Code, http.StatusOK; got != want {
		t.Fatalf("re-acknowledged restart replay status = %d, want %d: %s", got, want, response.Body.String())
	}
	if rows := restarted.outcomeSourceRows(10); len(rows) != 1 || anyToString(rows[0]["outcome_id"]) != "outcome_restart_reack" || len(recovered.utility.observations) != 1 {
		t.Fatalf("re-acknowledged restart did not admit canonical proof before Utility: outcomes=%#v utility=%#v", rows, recovered.utility.observations)
	}
	restartedLedger.mu.Lock()
	uncertain = restartedLedger.durabilityUnacknowledged
	restartedLedger.mu.Unlock()
	if uncertain || writeCalls != 2 {
		t.Fatalf("restart re-acknowledgement did not clear uncertainty: uncertain=%t writes=%d", uncertain, writeCalls)
	}
}

func TestContextPackCandidateOutcomeConcurrentReplayAppendsOnce(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	selectedRef := outcomeIntelligenceCandidateRef("concurrent-replay")
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{
		"sample_id": "cpq_concurrent_replay", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	})
	s := &server{contextPackQuality: telemetry}
	payload, err := json.Marshal(map[string]any{
		"outcome_id": "outcome_concurrent_replay", "sample_id": "cpq_concurrent_replay", "project": "contextlattice",
		"first_pass_success":   true,
		"evidence_attribution": []any{map[string]any{"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	const workers = 12
	var wait sync.WaitGroup
	errors := make(chan string, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response := httptest.NewRecorder()
			s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(payload)))
			if response.Code != http.StatusOK {
				errors <- response.Body.String()
			}
		}()
	}
	wait.Wait()
	close(errors)
	for response := range errors {
		t.Fatalf("concurrent replay failed: %s", response)
	}
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	outcomeRows := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		row, err := parseJSONMap([]byte(line))
		if err == nil && anyToString(row["outcome_id"]) == "outcome_concurrent_replay" {
			outcomeRows++
		}
	}
	if got, want := outcomeRows, 1; got != want {
		t.Fatalf("durable concurrent outcome rows = %d, want %d", got, want)
	}
}

func TestContextPackCandidateOutcomeRetryIsChronologyInsensitiveButClaimStrict(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	selectedRef := outcomeIntelligenceCandidateRef("retry-logical-claim")
	telemetry := newContextPackQualityTelemetry(20)
	telemetry.recordQuality(map[string]any{
		"sample_id": "cpq_retry_logical_claim", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	})
	s := &server{contextPackQuality: telemetry}
	payload := map[string]any{
		"outcome_id": "outcome_retry_logical_claim", "sample_id": "cpq_retry_logical_claim", "project": "contextlattice",
		"first_pass_success":   true,
		"utility":              map[string]any{"value": 4, "unit": "acceptance_points", "verification_passed": true},
		"evidence_attribution": []any{map[string]any{"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual"}},
	}
	post := func(body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
		return response
	}
	if response := post(payload); response.Code != http.StatusOK {
		t.Fatalf("first candidate outcome status=%d body=%s", response.Code, response.Body.String())
	}
	if response := post(cloneAnyMap(payload)); response.Code != http.StatusOK {
		t.Fatalf("candidate retry changed only gateway chronology but was not idempotent: status=%d body=%s", response.Code, response.Body.String())
	}
	changedUtility := cloneAnyMap(payload)
	anyMap(changedUtility["utility"])["value"] = 9
	if response := post(changedUtility); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "utility_outcome_conflict") {
		t.Fatalf("changed candidate utility was accepted: status=%d body=%s", response.Code, response.Body.String())
	}
	changedOutcome := cloneAnyMap(payload)
	changedOutcome["repair_required"] = true
	if response := post(changedOutcome); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "utility_outcome_conflict") {
		t.Fatalf("changed candidate outcome claim was accepted: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestContextPackCandidateOutcomeReplaySurvivesEvictionAndRestartBeyondLedgerWindow(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "context-pack-quality.ndjson")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", ledgerPath)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_MAX_SAMPLES", "20")
	selectedRef := outcomeIntelligenceCandidateRef("replay-eviction-restart")
	payload := map[string]any{
		"outcome_id": "outcome_replay_eviction_restart", "sample_id": "cpq_replay_eviction_restart", "project": "contextlattice",
		"first_pass_success":   true,
		"evidence_attribution": []any{map[string]any{"entity_type": "candidate", "candidate_ref": selectedRef, "attribution_method": "counterfactual"}},
	}
	post := func(s *server) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
		return response
	}

	telemetry := newContextPackQualityTelemetry(1)
	telemetry.recordQuality(map[string]any{
		"sample_id": "cpq_replay_eviction_restart", "project": "contextlattice",
		"selection_receipt": map[string]any{"candidates": []any{map[string]any{
			"candidate_ref": selectedRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
		}}},
	})
	s := &server{contextPackQuality: telemetry}
	if response := post(s); response.Code != http.StatusOK {
		t.Fatalf("first durable candidate outcome status=%d body=%s", response.Code, response.Body.String())
	}
	telemetry.recordOutcome(map[string]any{
		"outcome_id": "outcome_eviction_trigger", "sample_id": "cpq_eviction_trigger", "project": "contextlattice", "first_pass_success": true,
	})
	telemetry.mu.Lock()
	_, resident := telemetry.outcomeEntryLocked("outcome_replay_eviction_restart")
	telemetry.mu.Unlock()
	if resident {
		t.Fatal("candidate outcome did not leave the bounded in-memory outcome window")
	}
	if response := post(s); response.Code != http.StatusOK {
		t.Fatalf("evicted candidate replay status=%d body=%s", response.Code, response.Body.String())
	}

	for index := 0; index < 20; index++ {
		telemetry.recordQuality(map[string]any{
			"sample_id": "cpq_filler_" + strconv.Itoa(index), "project": "contextlattice", "quality_score": index,
		})
	}
	restarted := newContextPackQualityTelemetry(1)
	restarted.mu.Lock()
	_, candidateLoaded := restarted.outcomeKeys["outcome_replay_eviction_restart"]
	_, receiptLoaded := restarted.durableReceiptSamples["cpq_replay_eviction_restart"]
	restarted.mu.Unlock()
	if candidateLoaded || receiptLoaded {
		t.Fatalf("test did not push candidate evidence outside the bounded startup window: outcome=%t receipt=%t", candidateLoaded, receiptLoaded)
	}
	if response := post(&server{contextPackQuality: restarted}); response.Code != http.StatusOK {
		t.Fatalf("restarted durable candidate replay status=%d body=%s", response.Code, response.Body.String())
	}

	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		row, err := parseJSONMap([]byte(line))
		if err == nil && anyToString(row["outcome_id"]) == "outcome_replay_eviction_restart" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("durable candidate replay appended %d outcome rows, want 1", count)
	}
}

func TestEvidenceReputationSnapshotAdmitsCandidateOnlyAfterUtilityReconciliation(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "context-pack-quality.ndjson"))
	t.Setenv("GO_TOKEN_IMPACT_LEDGER_ENABLED", "false")
	t.Setenv("GO_UTILITY_LEDGER_ENABLED", "false")
	candidateRef := outcomeIntelligenceCandidateRef("utility-reconciled")
	outcome, quality, impact, events := utilityTestFixture("outcome_candidate_verified", "cpq_candidate_verified", "session_candidate_verified", "coding", "contextlattice", 4, 300, nil)
	quality["selection_receipt"] = map[string]any{"candidates": []any{map[string]any{
		"candidate_ref": candidateRef, "selection_state": "selected", "ordinal": 1, "evidence_kind": "decision",
	}}}
	qualityTelemetry := newContextPackQualityTelemetry(20)
	qualityTelemetry.recordQuality(quality)
	impactTelemetry := newTokenImpactTelemetry(20)
	impact["baseline_tokens_estimate"] = 600
	impact["packed_tokens_estimate"] = 300
	impact["saved_tokens_estimate"] = 300
	impact["transport_inclusive"] = true
	impact["transport_tokens_exact"] = 500
	impactTelemetry.record(impact)
	sessions := &agentSessionStore{
		sessions: map[string]map[string]any{anyToString(outcome["session_id"]): {"id": anyToString(outcome["session_id"]), "agent_id": "codex_test"}},
		events:   map[string][]map[string]any{anyToString(outcome["session_id"]): events},
	}
	s := &server{
		contextPackQuality: qualityTelemetry,
		tokenImpact:        impactTelemetry,
		utility:            &utilityTelemetry{limit: 20, observations: []map[string]any{}, byOutcome: map[string]int{}},
		agentSessions:      sessions,
	}
	digest := anyToString(anyMap(outcome["utility"])["evidence_digest"])
	outcome["first_pass_success"] = true
	outcome["evidence_attribution"] = []any{map[string]any{
		"entity_type": "candidate", "candidate_ref": candidateRef, "attribution_method": "counterfactual",
		"issuer": "go_holdout", "producer_agent_id": "codex_test", "verifier_id": "go_holdout",
		"verification_evidence_digest": digest,
	}}
	raw, err := json.Marshal(outcome)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.telemetryContextPackQualityOutcomeRoute(response, httptest.NewRequest(http.MethodPost, "/telemetry/context-pack-quality/outcome", bytes.NewReader(raw)))
	if response.Code != http.StatusOK {
		t.Fatalf("record route-to-session utility outcome: status=%d body=%s", response.Code, response.Body.String())
	}
	verified, ok := s.utility.observation(anyToString(outcome["outcome_id"]))
	if !ok || anyToString(verified["status"]) != "verified_exact" || !anyToBool(anyMap(verified["utility"])["independently_verified"]) {
		t.Fatalf("opaque quality utility did not join its raw session proof: %#v", verified)
	}
	if !utilitySHA256DigestValid(anyToString(anyMap(verified["utility"])["unit"])) || !utilitySHA256DigestValid(anyToString(anyMap(verified["utility"])["verification_event_id"])) || !utilitySHA256DigestValid(anyToString(anyMap(verified["utility"])["verifier_id"])) {
		t.Fatalf("public Utility observation retained reporter identifiers: %#v", verified)
	}
	if reconciliation := s.recordUtilitySessionEvent(sessions.sessions[anyToString(outcome["session_id"])], events[0]); anyToString(reconciliation["status"]) != "reconciled" {
		t.Fatalf("session verification event did not reconcile opaque Utility identity: %#v", reconciliation)
	}
	projection := s.evidenceReputationSnapshot("contextlattice", "coding", 2, 10)
	entries := parseRows(projection["entries"])
	if len(entries) != 1 || anyToString(entries[0]["entity_type"]) != "candidate" || anyToString(entries[0]["result_level_credit"]) != "selection_receipt_bound" {
		t.Fatalf("reconciled utility evidence did not unlock only advisory candidate reputation: %#v", projection)
	}
	if anyToBool(anyMap(entries[0]["bounded_influence"])["applied"]) || anyToBool(entries[0]["calibrated"]) {
		t.Fatalf("single verified candidate result affected ranking or skipped calibration: %#v", entries[0])
	}
}
