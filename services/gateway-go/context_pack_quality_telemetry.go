package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	contextPackQualitySchemaID           = "contextlattice_context_pack_quality.v1"
	contextPackQualityOutcomeSchemaID    = "contextlattice_context_pack_outcome.v1"
	contextPackQualityTelemetrySchemaID  = "contextlattice_context_pack_quality_telemetry.v1"
	contextPackSelectionReceiptSchemaID  = "contextlattice_context_pack_selection_receipt.v1"
	contextPackOutcomeBindingSchemaID    = "contextlattice_context_pack_outcome_attribution_binding.v1"
	contextPackRegressionFixtureSchemaID = "contextlattice_context_pack_regression_fixture.v1"
	contextPackOutcomeAdmissionSchemaID  = "contextlattice_context_pack_outcome_admission.v1"
	contextPackSelectionReceiptLimit     = 24
	contextPackCandidateAttemptLimit     = 64
	// Outcome counters are observational calibration inputs, not a bulk
	// accounting interface. Keep each claim bounded so one authenticated
	// report cannot dominate a bounded aggregate or overflow its counters.
	contextPackOutcomeMaxRetryCount              = int64(50)
	contextPackOutcomeMaxFollowupTokens          = int64(10_000_000)
	contextPackOutcomeMaxProviderComponentTokens = int64(10_000_000)
	contextPackOutcomeMaxProviderTotalTokens     = int64(20_000_000)
)

type contextPackQualityTelemetry struct {
	mu                            sync.Mutex
	outcomeAdmissionMu            sync.Mutex
	limit                         int
	ledger                        *contextPackQualityLedger
	regressionFixtures            *contextPackRegressionFixtureStore
	samples                       []map[string]any
	outcomes                      []map[string]any
	proofSamples                  proofTimelineMapRing
	proofOutcomes                 proofTimelineMapRing
	outcomeKeys                   map[string]struct{}
	durableReceiptSamples         map[string]string
	sampleCount                   int64
	outcomeCount                  int64
	calibrationOutcomeCount       int64
	exactTokenSamples             int64
	totalQualityScore             int64
	totalExactPromptSaved         int64
	totalModeledInferenceAvoided  int64
	totalModeledExtraCallsMilli   int64
	firstPassSuccessCount         int64
	repairRequiredCount           int64
	totalRetryCount               int64
	totalObservedFollowupTokens   int64
	providerUsageCount            int64
	totalProviderPromptTokens     int64
	totalProviderCompletionTokens int64
	totalProviderTotalTokens      int64
	lastSampleAt                  string
	lastOutcomeAt                 string
}

type contextPackQualityLedger struct {
	mu                       sync.Mutex
	enabled                  bool
	path                     string
	maxBytes                 int64
	maxSamples               int
	parseErrors              int
	writeErrors              int
	pruneErrors              int
	loadedRows               int
	lastWriteAt              string
	lastError                string
	lastPruneError           string
	durabilityUnacknowledged bool
	writeFile                func(string, []byte, bool) error
}

// contextPackRegressionFixtureStore is intentionally separate from public
// quality telemetry. It is the single owner-only durable copy of an explicit
// regression fixture; quality rows retain only its exact opaque reference.
type contextPackRegressionFixtureStore struct {
	mu              sync.Mutex
	enabled         bool
	path            string
	dedicatedParent bool
	maxBytes        int64
	maxFixtures     int
	order           []string
	fixtures        map[string]map[string]any
	parseErrors     int
	writeErrors     int
	lastError       string
	writeFile       func(string, []byte, bool) error
}

var (
	errContextPackReceiptLedgerUnavailable = errors.New("context-pack receipt ledger durability is unavailable")
	errContextPackOutcomeInvalidNumeric    = errors.New("context-pack outcome has an invalid numeric claim")
	errContextPackOutcomeSampleConflict    = errors.New("context-pack sample already has a different authoritative outcome")
	errContextPackPrivacyMigrationRejected = errors.New("context-pack quality privacy migration rejected")
)

func newContextPackQualityTelemetry(limit int) *contextPackQualityTelemetry {
	return newContextPackQualityTelemetryWithLedger(limit, newContextPackQualityLedgerFromEnv())
}

// newContextPackQualityTelemetryWithLedger keeps startup durability testable
// without weakening the production constructor's owner-only ledger boundary.
func newContextPackQualityTelemetryWithLedger(limit int, ledger *contextPackQualityLedger) *contextPackQualityTelemetry {
	if limit <= 0 {
		limit = 100
	}
	t := &contextPackQualityTelemetry{
		limit:                 limit,
		ledger:                ledger,
		samples:               make([]map[string]any, 0, limit),
		outcomes:              make([]map[string]any, 0, limit),
		outcomeKeys:           make(map[string]struct{}),
		durableReceiptSamples: make(map[string]string),
	}
	if t.ledger != nil && t.ledger.enabled {
		if err := t.migratePersistedTopicPrivacy(); err != nil {
			t.ledger.failClosedPrivacyMigration(err)
			// Do not load a failed migration's rows: the public snapshot must
			// fail closed rather than re-expose legacy topic paths.
			return t
		}
		if err := t.ledger.acknowledgeStartupDurability(); err != nil {
			t.ledger.setError(err)
		}
	}
	// Explicit regression fixtures are meaningful only beside an acknowledged
	// quality ledger. Do not create a shared/default raw-fixture store for a
	// disabled, failed, or unacknowledged quality boundary.
	if contextPackQualityLedgerAvailable(t.ledger) {
		t.regressionFixtures = newContextPackRegressionFixtureStoreFromQualityLedger(t.ledger)
	}
	t.loadPersistedRows()
	return t
}

// migratePersistedTopicPrivacy is a one-way, owner-only telemetry schema
// migration. It replaces top-level quality topic paths and legacy observable-
// only reporter labels with deterministic project-bound refs while preserving
// every row's order, receipt, and outcome semantics. Canonical memory remains
// the sole owner of topic labels and raw reporter-selected identifiers.
func (t *contextPackQualityTelemetry) migratePersistedTopicPrivacy() error {
	if t == nil || t.ledger == nil || !t.ledger.enabled || strings.TrimSpace(t.ledger.path) == "" {
		return nil
	}
	t.ledger.mu.Lock()
	defer t.ledger.mu.Unlock()
	rows, parseErrors, err := t.ledger.readRowsForPrivacyMigrationUnlocked()
	if err != nil {
		return err
	}
	if parseErrors > 0 {
		// Rewriting from a partial parser result would silently drop a durable
		// row. Preserve the original file and make the public projection fail
		// closed until a human has repaired the malformed ledger.
		return fmt.Errorf("%w: malformed ledger rows", errContextPackPrivacyMigrationRejected)
	}
	needsMigration := false
	for _, row := range rows {
		privacySafeRow := row
		if anyToString(row["schema_id"]) == contextPackQualityOutcomeSchemaID {
			var reporterMigrated bool
			privacySafeRow, reporterMigrated, err = contextPackQualityMigrateLegacyOutcomeReporter(row)
			if err != nil {
				return err
			}
			needsMigration = needsMigration || reporterMigrated
		}
		if anyToString(privacySafeRow["schema_id"]) == contextPackQualityOutcomeSchemaID && contextPackQualityOutcomeNeedsFailClosedPrivacyStop(privacySafeRow) {
			// Outcome rows can contain nested free-form result claims. Rewriting
			// them through the current normalizer would alter durable semantics
			// while leaving the original private bytes on disk, so treat any such
			// legacy row as an explicit stop condition instead.
			return fmt.Errorf("%w: unsafe legacy outcome row", errContextPackPrivacyMigrationRejected)
		}
		if schemaID := anyToString(row["schema_id"]); schemaID == contextPackQualitySchemaID || schemaID == contextPackQualityOutcomeSchemaID {
			if _, present := row["topic_path"]; present {
				needsMigration = true
			}
		}
	}
	if !needsMigration {
		return nil
	}
	content := make([]byte, 0)
	for _, original := range rows {
		row := cloneJSONMap(original)
		if anyToString(row["schema_id"]) == contextPackQualityOutcomeSchemaID {
			row, _, err = contextPackQualityMigrateLegacyOutcomeReporter(row)
			if err != nil {
				return err
			}
		}
		if schemaID := anyToString(row["schema_id"]); schemaID == contextPackQualitySchemaID || schemaID == contextPackQualityOutcomeSchemaID {
			if rawTopic, present := row["topic_path"]; present {
				topicPath, ok := rawTopic.(string)
				if !ok {
					return fmt.Errorf("%w: non-string topic_path", errContextPackPrivacyMigrationRejected)
				}
				project := contextPackQualityIdentifier(row["project"], 160)
				if project == "" {
					return fmt.Errorf("%w: unresolvable project identity", errContextPackPrivacyMigrationRejected)
				}
				delete(row, "topic_path")
				// Empty legacy paths have no topic grouping semantics. Drop the raw
				// key without inventing a shared sentinel ref; this is the known
				// shape of older quality rows.
				if strings.TrimSpace(topicPath) == "" {
					delete(row, "topic_ref")
				} else if ref := contextPackQualityTopicRef(project, topicPath); ref != "" {
					row["topic_ref"] = ref
				} else {
					return fmt.Errorf("%w: topic_ref derivation failed", errContextPackPrivacyMigrationRejected)
				}
			}
		}
		encoded, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			return marshalErr
		}
		content = append(content, encoded...)
		content = append(content, '\n')
	}
	dedicatedParent := strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH")) == ""
	return t.ledger.writeContentDurablyLocked(content, dedicatedParent)
}

func newContextPackRegressionFixtureStoreFromQualityLedger(qualityLedger *contextPackQualityLedger) *contextPackRegressionFixtureStore {
	enabled := qualityLedger != nil && qualityLedger.enabled && strings.TrimSpace(qualityLedger.path) != "" &&
		envBool("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_ENABLED", true)
	qualityLedgerPath := ""
	if qualityLedger != nil {
		qualityLedgerPath = qualityLedger.path
	}
	path := contextPackRegressionFixtureLedgerPath(qualityLedgerPath)
	if strings.TrimSpace(path) == "" {
		enabled = false
	}
	store := &contextPackRegressionFixtureStore{
		enabled:         enabled,
		path:            path,
		dedicatedParent: strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH")) == "" && strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH")) == "",
		maxBytes:        int64(clampInt(envInt("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_BYTES", 2*1024*1024), 64*1024, 64*1024*1024)),
		maxFixtures:     clampInt(envInt("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_MAX_FIXTURES", 1000), 20, 20000),
		fixtures:        map[string]map[string]any{},
		writeFile:       writeOwnerOnlyDurableAtomicFile,
	}
	if !enabled {
		return store
	}
	if err := createOwnerOnlyDurableEmptyFileIfMissing(path, store.dedicatedParent); err != nil {
		store.enabled = false
		store.lastError = tokenImpactLedgerErrorCode(err)
		return store
	}
	if err := store.load(); err != nil {
		store.enabled = false
		store.order = nil
		store.fixtures = map[string]map[string]any{}
		store.lastError = tokenImpactLedgerErrorCode(err)
	}
	return store
}

func (s *contextPackRegressionFixtureStore) load() error {
	if s == nil || !s.enabled || strings.TrimSpace(s.path) == "" {
		return nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer file.Close()
	order := make([]string, 0)
	fixtures := map[string]map[string]any{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 512*1024)
	loadedBytes := int64(0)
	for scanner.Scan() {
		if len(order) >= s.maxFixtures || loadedBytes+int64(len(scanner.Bytes()))+1 > s.maxBytes {
			return errors.New("context-pack regression fixture ledger exceeds bounded load capacity")
		}
		loadedBytes += int64(len(scanner.Bytes())) + 1
		row, parseErr := parseJSONMap(scanner.Bytes())
		if parseErr != nil || anyToString(row["schema_id"]) != contextPackRegressionFixtureSchemaID {
			return errors.New("context-pack regression fixture ledger contains an invalid row")
		}
		fixture, ref := contextPackQualityNormalizedRegressionFixture(anyMap(row["fixture"]))
		if fixture == nil || !utilitySHA256DigestValid(anyToString(row["fixture_ref"])) || anyToString(row["fixture_ref"]) != ref {
			return errors.New("context-pack regression fixture ledger contains an invalid fixture ref")
		}
		if _, exists := fixtures[ref]; exists {
			return errors.New("context-pack regression fixture ledger contains a duplicate fixture ref")
		}
		fixtures[ref] = fixture
		order = append(order, ref)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.order = order
	s.fixtures = fixtures
	s.parseErrors = 0
	s.mu.Unlock()
	return nil
}

func contextPackQualityNormalizedRegressionFixture(raw map[string]any) (map[string]any, string) {
	fixture, _ := normalizeDerivedRegressionFixture(raw)
	if len(fixture) == 0 {
		return nil, ""
	}
	encoded, err := json.Marshal(fixture)
	if err != nil {
		return nil, ""
	}
	return fixture, "sha256:" + sha256Hex(string(encoded))
}

func (s *contextPackRegressionFixtureStore) recordDetailed(raw map[string]any) (string, bool, error) {
	fixture, ref := contextPackQualityNormalizedRegressionFixture(raw)
	if fixture == nil || ref == "" {
		return "", false, nil
	}
	if s == nil || !s.enabled || strings.TrimSpace(s.path) == "" {
		return "", false, errors.New("context-pack regression fixture persistence is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.fixtures[ref]; exists {
		return ref, false, nil
	}
	previousOrder := append([]string{}, s.order...)
	previousFixtures := s.fixtures
	nextFixtures := make(map[string]map[string]any, len(previousFixtures)+1)
	for key, value := range previousFixtures {
		nextFixtures[key] = value
	}
	nextFixtures[ref] = cloneJSONMap(fixture)
	nextOrder := append(append([]string{}, previousOrder...), ref)
	content, trimmedOrder, trimmedFixtures, err := contextPackRegressionFixtureContent(nextOrder, nextFixtures, s.maxBytes, s.maxFixtures)
	if err != nil {
		s.writeErrors++
		s.lastError = tokenImpactLedgerErrorCode(err)
		return "", false, err
	}
	writeFile := s.writeFile
	if writeFile == nil {
		writeFile = writeOwnerOnlyDurableAtomicFile
	}
	if err := writeFile(s.path, content, s.dedicatedParent); err != nil {
		s.writeErrors++
		s.lastError = tokenImpactLedgerErrorCode(err)
		return "", false, err
	}
	s.order = trimmedOrder
	s.fixtures = trimmedFixtures
	s.lastError = ""
	return ref, true, nil
}

func contextPackRegressionFixtureContent(order []string, fixtures map[string]map[string]any, maxBytes int64, maxFixtures int) ([]byte, []string, map[string]map[string]any, error) {
	order = append([]string{}, order...)
	fixtures = cloneContextPackRegressionFixtures(fixtures)
	if len(order) > maxFixtures {
		return nil, nil, nil, errors.New("context-pack regression fixture ledger exceeds bounded fixture capacity")
	}
	content := make([]byte, 0)
	for _, ref := range order {
		fixture := fixtures[ref]
		encoded, err := json.Marshal(map[string]any{
			"schema_id": contextPackRegressionFixtureSchemaID, "version": 1, "fixture_ref": ref, "fixture": fixture,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		content = append(content, encoded...)
		content = append(content, '\n')
	}
	if int64(len(content)) > maxBytes {
		return nil, nil, nil, errors.New("context-pack regression fixture ledger exceeds bounded byte capacity")
	}
	return content, order, fixtures, nil
}

func cloneContextPackRegressionFixtures(input map[string]map[string]any) map[string]map[string]any {
	out := make(map[string]map[string]any, len(input))
	for key, value := range input {
		out[key] = cloneJSONMap(value)
	}
	return out
}

func (s *contextPackRegressionFixtureStore) fixture(ref string) (map[string]any, bool) {
	if s == nil || !utilitySHA256DigestValid(ref) {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fixture, found := s.fixtures[ref]
	return cloneJSONMap(fixture), found
}

func newContextPackQualityLedgerFromEnv() *contextPackQualityLedger {
	enabled := envBool("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", true)
	path := contextPackQualityLedgerPath()
	if strings.TrimSpace(path) == "" {
		enabled = false
	}
	maxBytes := int64(clampInt(envInt("GO_CONTEXT_PACK_QUALITY_LEDGER_MAX_BYTES", 2*1024*1024), 64*1024, 64*1024*1024))
	maxSamples := clampInt(envInt("GO_CONTEXT_PACK_QUALITY_LEDGER_MAX_SAMPLES", 1000), 20, 20000)
	ledger := &contextPackQualityLedger{
		enabled:    enabled,
		path:       path,
		maxBytes:   maxBytes,
		maxSamples: maxSamples,
		writeFile:  writeOwnerOnlyDurableAtomicFile,
	}
	if enabled {
		dedicatedParent := strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH")) == ""
		if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
			ledger.enabled = false
			ledger.lastError = err.Error()
		}
	}
	return ledger
}

func contextPackQualityLedgerPath() string {
	return resolveStoragePath("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(".data", "orchestrator", "context_pack_quality_ledger.ndjson"))
}

func (s *server) recordContextPackQuality(sample map[string]any) {
	if s == nil || s.contextPackQuality == nil || len(sample) == 0 {
		return
	}
	s.contextPackQuality.recordQuality(sample)
}

func (s *server) recordContextPackQualityOutcome(sample map[string]any) bool {
	recorded, _ := s.recordContextPackQualityOutcomeDurably(sample)
	return recorded
}

// recordContextPackQualityOutcomeDurably exposes the candidate receipt boundary
// to HTTP ingress. Receipt-bound candidate outcomes may only enter the local
// proof ring after their quality-ledger append has acknowledged durability.
func (s *server) recordContextPackQualityOutcomeDurably(sample map[string]any) (bool, error) {
	if s == nil || s.contextPackQuality == nil || len(sample) == 0 {
		return false, errContextPackReceiptLedgerUnavailable
	}
	if anyToString(sample["schema_id"]) == contextPackQualityOutcomeSchemaID {
		return s.contextPackQuality.recordOutcomeEntryDurably(sample)
	}
	return s.contextPackQuality.recordOutcomeDurably(sample)
}

func (s *server) contextPackQualityTelemetrySnapshot() map[string]any {
	if s == nil || s.contextPackQuality == nil {
		return defaultContextPackQualityTelemetrySnapshot(nil, nil)
	}
	return s.contextPackQuality.snapshot()
}

// aggregateSignalSufficientStatistics exposes only clipped scalar inputs for
// the opt-in Aggregate Signal boundary; sample rows and scope labels stay local.
func (t *contextPackQualityTelemetry) aggregateSignalSufficientStatistics() map[string]any {
	if t == nil {
		return map[string]any{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	statistics := map[string]any{
		"exact_prompt_tokens_saved":        t.totalExactPromptSaved,
		"modeled_inference_tokens_avoided": t.totalModeledInferenceAvoided,
		"provider_total_tokens":            t.totalProviderTotalTokens,
	}
	grade := "modeled_counterfactual"
	if t.calibrationOutcomeCount > 0 {
		grade = "outcome_seeded"
	}
	if t.calibrationOutcomeCount >= 20 {
		grade = "outcome_adjusted"
	}
	statistics["calibration_grade"] = grade
	if t.sampleCount > 0 {
		statistics["context_quality_score"] = roundFloat(float64(t.totalQualityScore)/float64(t.sampleCount), 6)
		statistics["contribution_coverage"] = roundFloat(math.Min(1, float64(t.calibrationOutcomeCount)/float64(t.sampleCount)), 6)
	}
	if t.calibrationOutcomeCount > 0 {
		statistics["first_pass_success_rate"] = roundFloat(float64(t.firstPassSuccessCount)/float64(t.calibrationOutcomeCount), 6)
		statistics["repair_rate"] = roundFloat(float64(t.repairRequiredCount)/float64(t.calibrationOutcomeCount), 6)
		statistics["average_retry_count"] = roundFloat(float64(t.totalRetryCount)/float64(t.calibrationOutcomeCount), 6)
	}
	return statistics
}

func defaultContextPackQualityTelemetrySnapshot(ledger *contextPackQualityLedger, regressionFixtures *contextPackRegressionFixtureStore) map[string]any {
	return map[string]any{
		"schema_id":                              contextPackQualityTelemetrySchemaID,
		"version":                                1,
		"updatedAt":                              nowUTCISO(),
		"sample_count":                           0,
		"outcome_sample_count":                   0,
		"calibration_outcome_sample_count":       0,
		"confidence":                             "low",
		"calibration_grade":                      "modeled_counterfactual",
		"average_quality_score":                  0,
		"exact_prompt_tokens_saved":              0,
		"modeled_inference_tokens_avoided":       0,
		"modeled_extra_calls_avoided":            0,
		"observed_first_pass_success_rate":       nil,
		"observed_repair_rate":                   nil,
		"observed_average_retry_count":           nil,
		"observed_followup_tokens":               0,
		"observed_provider_usage_count":          0,
		"observed_provider_prompt_tokens":        0,
		"observed_provider_completion_tokens":    0,
		"observed_provider_total_tokens":         0,
		"observed_average_provider_total_tokens": nil,
		"measurement_limit":                      contextPackQualityMeasurementLimit(false),
		"source":                                 "/telemetry/context-pack-quality",
		"storage":                                contextPackQualityStoragePublicStatus(ledger, regressionFixtures),
		"samples":                                []any{},
		"outcomes":                               []any{},
	}
}

func contextPackQualityMeasurementLimit(hasOutcomes bool) string {
	if hasOutcomes {
		return "Exact prompt-token savings are measured from context-pack token counts; modeled inference avoidance is confidence-banded and calibrated by bounded outcome rows."
	}
	return "Exact prompt-token savings are measured from context-pack token counts; inference avoidance is a confidence-banded counterfactual model until outcome rows are posted."
}

func (t *contextPackQualityTelemetry) loadPersistedRows() {
	if t == nil || !contextPackQualityLedgerAvailable(t.ledger) {
		return
	}
	rows, parseErrors, err := t.ledger.readRows()
	if err != nil {
		t.ledger.setError(err)
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	deferredDurableOutcomes := make([]map[string]any, 0)
	for _, row := range rows {
		if contextPackQualityRowNeedsPrivacyMigration(row) {
			switch anyToString(row["schema_id"]) {
			case contextPackQualitySchemaID:
				row = contextPackQualityEntryFromSample(row)
			case contextPackQualityOutcomeSchemaID:
				// Defense in depth for a file modified between migration preflight
				// and loading: never make an unsafe durable outcome visible through
				// an in-memory-only normalization escape hatch.
				t.ledger.failClosedPrivacyMigration(fmt.Errorf("%w: ledger changed to an unsafe legacy outcome row during load", errContextPackPrivacyMigrationRejected))
				return
			}
		}
		switch anyToString(row["schema_id"]) {
		case contextPackQualitySchemaID:
			t.applyQualityEntryLocked(row)
			t.markDurableReceiptSampleLocked(row)
		case contextPackQualityOutcomeSchemaID:
			if !contextPackOutcomeHasAuthoritativeSampleAdmission(row) {
				// Rows written before server-bound sample admission remain visible for
				// forensic continuity, but can never calibrate counts or provider use.
				row["quality_sample_admission"] = "legacy_ineligible"
				delete(row, "quality_sample_admission_ref")
				row["calibration_eligible"] = false
			}
			if contextPackOutcomeHasReceiptBoundCandidate(row) || contextPackOutcomeHasAuthoritativeSampleAdmission(row) {
				deferredDurableOutcomes = append(deferredDurableOutcomes, row)
				continue
			}
			t.applyOutcomeEntryLocked(row)
		}
	}
	for _, row := range deferredDurableOutcomes {
		if t.candidateOutcomeReceiptDurableLocked(row) && t.authoritativeOutcomeSampleDurableLocked(row) {
			t.applyOutcomeEntryLocked(row)
		}
	}
	t.ledger.loadedRows = len(rows)
	t.ledger.parseErrors = parseErrors
}

func contextPackQualityRowNeedsPrivacyMigration(row map[string]any) bool {
	switch anyToString(row["schema_id"]) {
	case contextPackQualitySchemaID:
		if _, present := row["topic_path"]; present {
			return true
		}
		return contextPackQualityQueryHash(row["query_hash"]) != anyToString(row["query_hash"]) ||
			contextPackQualityConfidence(row["confidence"]) != anyToString(row["confidence"]) ||
			contextPackQualityCalibrationGrade(row["calibration_grade"]) != anyToString(row["calibration_grade"]) ||
			contextPackQualityCounterfactualBaseline(row["counterfactual_baseline"]) != anyToString(row["counterfactual_baseline"])
	case contextPackQualityOutcomeSchemaID:
		for _, key := range []string{
			"topic_path", "regression_case", "verified_utility", "matched_control", "utility_value", "verified_utility_value",
			"utility_unit", "verification_event_id", "evidence_digest", "verifier_kind", "latency_ms", "duration_ms",
			"cost_microusd", "tool_calls", "tool_call_count", "failures", "failure_count", "pair_id", "pair_arm",
			"arm", "matched_control_outcome_id", "task_match_digest", "matching_method", "leakage_free",
		} {
			if _, present := row[key]; present {
				return true
			}
		}
		for _, nested := range []map[string]any{anyMap(row["utility"]), anyMap(row["pairing"]), anyMap(row["economics"])} {
			for key, value := range nested {
				if strings.Contains(strings.ToLower(key), "query") || strings.Contains(strings.ToLower(key), "content") || strings.Contains(strings.ToLower(key), "path") ||
					contextPackQualityUnsafeReporterString(value) {
					return true
				}
			}
		}
		return contextPackQualityLegacyOutcomeIdentifiersUnsafe(row)
	}
	return false
}

// A top-level topic_path is the one legacy outcome field with a defined,
// lossless one-way migration. All other outdated outcome shapes are rejected
// rather than in-memory-sanitized while private durable bytes remain behind.
func contextPackQualityOutcomeNeedsFailClosedPrivacyStop(row map[string]any) bool {
	if anyToString(row["schema_id"]) != contextPackQualityOutcomeSchemaID {
		return false
	}
	withoutTopic := cloneJSONMap(row)
	delete(withoutTopic, "topic_path")
	return contextPackQualityRowNeedsPrivacyMigration(withoutTopic)
}

// contextPackQualityMigrateLegacyOutcomeReporter performs the sole additional
// lossy migration allowed for old outcome rows: a reporter-selected source is
// converted with the same project/field-bound one-way function used by current
// ingress. Only observable-only rows qualify. Credit-bearing or admitted rows
// with a non-canonical reporter remain an explicit fail-closed condition.
func contextPackQualityMigrateLegacyOutcomeReporter(row map[string]any) (map[string]any, bool, error) {
	if anyToString(row["schema_id"]) != contextPackQualityOutcomeSchemaID {
		return row, false, nil
	}
	raw, present := row["outcome_source"]
	if !present {
		return row, false, nil
	}
	source, ok := raw.(string)
	if !ok || source == "" || source == "agent_report" || utilitySHA256DigestValid(source) {
		return row, false, nil
	}
	if !contextPackQualityLegacyOutcomeObservableOnly(row) || contextPackQualityIdentifier(source, 80) != source {
		return nil, false, fmt.Errorf("%w: unsafe legacy outcome reporter", errContextPackPrivacyMigrationRejected)
	}
	project, ok := row["project"].(string)
	if !ok || project == "" || contextPackQualityIdentifier(project, 160) != project {
		return nil, false, fmt.Errorf("%w: unscoped legacy outcome reporter", errContextPackPrivacyMigrationRejected)
	}
	ref := contextPackQualityOpaqueReporterRef(project, "outcome_source", source, 80)
	if !utilitySHA256DigestValid(ref) {
		return nil, false, fmt.Errorf("%w: legacy outcome reporter ref derivation failed", errContextPackPrivacyMigrationRejected)
	}
	migrated := cloneJSONMap(row)
	migrated["outcome_source"] = ref
	return migrated, true, nil
}

func contextPackQualityLegacyOutcomeObservableOnly(row map[string]any) bool {
	if _, present := row["quality_sample_admission"]; present {
		return false
	}
	if _, present := row["quality_sample_admission_ref"]; present {
		return false
	}
	for _, field := range []string{
		"utility", "pairing", "evidence_attribution", "candidate_attribution_attempts", "attribution_binding", "regression_case_ref",
		"verification_evidence_digest", "verifier_id", "verification_passed",
	} {
		if _, present := row[field]; present {
			return false
		}
	}
	return true
}

// Legacy outcome rows have no trustworthy private sidecar binding. Accept only
// the exact opaque/enum scalar shapes produced by current ingress; a compact
// credential or customer identifier is otherwise indistinguishable from a
// benign identifier and must fail closed instead of reaching a public snapshot.
func contextPackQualityLegacyOutcomeIdentifiersUnsafe(row map[string]any) bool {
	fullRef := func(value any) bool {
		text, ok := value.(string)
		return ok && utilitySHA256DigestValid(text)
	}
	allowedOutcomeKeys := map[string]struct{}{
		"schema_id": {}, "version": {}, "capturedAt": {}, "gateway_received_at": {},
		"outcome_id": {}, "sample_id": {}, "task_id": {}, "project": {}, "task_class": {}, "retrieval_intent": {},
		"session_id": {}, "task_identity_id": {}, "execution_lane_id": {}, "agent_id": {},
		"first_pass_success": {}, "repair_required": {}, "retry_count": {}, "observed_followup_tokens": {},
		"outcome_source": {}, "outcome_class": {}, "context_attribution": {}, "calibration_eligible": {},
		"policy_id": {}, "policy_arm": {}, "policy_phase": {},
		"provider_prompt_tokens": {}, "provider_completion_tokens": {}, "provider_total_tokens": {},
		"utility": {}, "economics": {}, "pairing": {}, "evidence_attribution": {}, "candidate_attribution_attempts": {},
		"verification_evidence_digest": {}, "verifier_id": {}, "verification_passed": {},
		"regression_case_ref": {}, "regression_partition": {}, "traffic_class": {}, "synthetic": {}, "stability": {},
		"topic_path": {}, "topic_ref": {}, "quality_sample_admission": {}, "quality_sample_admission_ref": {}, "attribution_binding": {},
	}
	for key := range row {
		if _, allowed := allowedOutcomeKeys[key]; !allowed {
			return true
		}
	}
	for key := range row {
		lower := strings.ToLower(strings.TrimSpace(key))
		if strings.Contains(lower, "query") || strings.Contains(lower, "content") || strings.Contains(lower, "path") ||
			(strings.Contains(lower, "source") && lower != "outcome_source") {
			return true
		}
	}
	mapHasOnly := func(values map[string]any, allowed []string) bool {
		allowedSet := make(map[string]struct{}, len(allowed))
		for _, key := range allowed {
			allowedSet[key] = struct{}{}
		}
		for key := range values {
			if _, ok := allowedSet[key]; !ok {
				return false
			}
		}
		return true
	}
	optionalMap := func(field string) (map[string]any, bool) {
		raw, present := row[field]
		if !present {
			return nil, true
		}
		values, ok := raw.(map[string]any)
		return values, ok
	}
	optionalList := func(field string) ([]any, bool) {
		raw, present := row[field]
		if !present {
			return nil, true
		}
		values, ok := raw.([]any)
		return values, ok
	}
	exactBool := func(value any) bool {
		_, ok := value.(bool)
		return ok
	}
	finiteUtilityValue := func(value any) (float64, bool) {
		var number float64
		switch typed := value.(type) {
		case int:
			number = float64(typed)
		case int8:
			number = float64(typed)
		case int16:
			number = float64(typed)
		case int32:
			number = float64(typed)
		case int64:
			number = float64(typed)
		case uint:
			number = float64(typed)
		case uint8:
			number = float64(typed)
		case uint16:
			number = float64(typed)
		case uint32:
			number = float64(typed)
		case uint64:
			number = float64(typed)
		case float32:
			number = float64(typed)
		case float64:
			number = typed
		case json.Number:
			parsed, err := typed.Float64()
			if err != nil {
				return 0, false
			}
			number = parsed
		default:
			return 0, false
		}
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Abs(number) > 1_000_000 || math.Abs(utilityRound(number, 6)-number) > 1e-9 {
			return 0, false
		}
		return number, true
	}
	canonicalIdentifier := func(value any, limit int, allowEmpty bool) bool {
		text, ok := value.(string)
		if !ok {
			return false
		}
		if text == "" {
			return allowEmpty
		}
		return contextPackQualityIdentifier(text, limit) == text
	}
	canonicalLowerIdentifier := func(value any, limit int, allowEmpty bool) bool {
		text, ok := value.(string)
		if !ok {
			return false
		}
		if text == "" {
			return allowEmpty
		}
		return contextPackQualityIdentifier(strings.ToLower(text), limit) == text
	}
	canonicalTimestamp := func(value any) bool {
		text, ok := value.(string)
		if !ok || text == "" {
			return false
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		return err == nil && parsed.UTC().Format(time.RFC3339Nano) == text
	}
	admission, admissionPresent := row["quality_sample_admission"]
	admissionRef, admissionRefPresent := row["quality_sample_admission_ref"]
	// An old direct ledger row can be useful as an observable-only forensic
	// record even when its original project scope was absent. It must not carry
	// any field that can join Utility, regression, or evidence/candidate credit;
	// authoritative rows always require an explicit, nonempty project scope.
	legacyObservableOnly := contextPackQualityLegacyOutcomeObservableOnly(row)
	if schemaID, ok := row["schema_id"].(string); !ok || schemaID != contextPackQualityOutcomeSchemaID {
		return true
	}
	version, versionOK := contextPackOutcomeBoundedCount(row["version"], 1)
	if !versionOK || version != 1 || !canonicalTimestamp(row["capturedAt"]) {
		return true
	}
	if receivedAt, present := row["gateway_received_at"]; present && !canonicalTimestamp(receivedAt) {
		return true
	}
	for _, field := range []struct {
		key        string
		limit      int
		allowEmpty bool
		lower      bool
		required   bool
	}{
		{key: "outcome_id", limit: 200, required: true},
		{key: "sample_id", limit: 200, required: true},
		{key: "task_id", limit: 160, allowEmpty: true},
		{key: "task_class", limit: 80, allowEmpty: true, lower: true},
		{key: "retrieval_intent", limit: 80, allowEmpty: true, lower: true},
	} {
		value, present := row[field.key]
		if !present {
			if field.required {
				return true
			}
			continue
		}
		if (field.lower && !canonicalLowerIdentifier(value, field.limit, field.allowEmpty)) || (!field.lower && !canonicalIdentifier(value, field.limit, field.allowEmpty)) {
			return true
		}
	}
	project, projectPresent := row["project"]
	if !projectPresent || !canonicalIdentifier(project, 160, legacyObservableOnly) {
		return true
	}
	for _, field := range []struct {
		key   string
		limit int
	}{
		{key: "session_id", limit: maxAgentSessionIDLength},
		{key: "task_identity_id", limit: 160},
		{key: "execution_lane_id", limit: 160},
		{key: "agent_id", limit: 160},
		{key: "policy_id", limit: 160},
	} {
		if value, present := row[field.key]; present && !canonicalIdentifier(value, field.limit, false) {
			return true
		}
	}
	for _, field := range []string{"first_pass_success", "repair_required", "calibration_eligible"} {
		value, present := row[field]
		if !present || !exactBool(value) {
			return true
		}
	}
	retryCount, retryCountOK := contextPackOutcomeBoundedCount(row["retry_count"], contextPackOutcomeMaxRetryCount)
	followupTokens, followupTokensOK := contextPackOutcomeBoundedCount(row["observed_followup_tokens"], contextPackOutcomeMaxFollowupTokens)
	if !retryCountOK || !followupTokensOK {
		return true
	}
	_ = retryCount
	_ = followupTokens
	outcomeSource, outcomeSourceOK := row["outcome_source"].(string)
	if !outcomeSourceOK || (outcomeSource != "agent_report" && !fullRef(outcomeSource)) {
		return true
	}
	if !canonicalLowerIdentifier(row["outcome_class"], 80, false) || !canonicalLowerIdentifier(row["context_attribution"], 80, false) {
		return true
	}
	if policyArm, present := row["policy_arm"]; present {
		arm, ok := policyArm.(string)
		if !ok || (arm != "control" && arm != "candidate" && arm != "shadow" && arm != "canary") {
			return true
		}
	}
	if policyPhase, present := row["policy_phase"]; present {
		phase, ok := policyPhase.(string)
		if !ok {
			return true
		}
		if _, allowed := contextPolicyPhases[phase]; !allowed {
			return true
		}
	}
	providerCounts := map[string]int64{}
	providerPresent := map[string]bool{}
	for _, field := range []struct {
		key string
		max int64
	}{
		{key: "provider_prompt_tokens", max: contextPackOutcomeMaxProviderComponentTokens},
		{key: "provider_completion_tokens", max: contextPackOutcomeMaxProviderComponentTokens},
		{key: "provider_total_tokens", max: contextPackOutcomeMaxProviderTotalTokens},
	} {
		if value, present := row[field.key]; present {
			count, valid := contextPackOutcomeBoundedCount(value, field.max)
			if !valid || count == 0 {
				return true
			}
			providerCounts[field.key] = count
			providerPresent[field.key] = true
		}
	}
	if (providerPresent["provider_prompt_tokens"] || providerPresent["provider_completion_tokens"]) && !providerPresent["provider_total_tokens"] {
		return true
	}
	if providerPresent["provider_total_tokens"] &&
		(providerCounts["provider_total_tokens"] < providerCounts["provider_prompt_tokens"] ||
			providerCounts["provider_total_tokens"] < providerCounts["provider_completion_tokens"] ||
			(providerPresent["provider_prompt_tokens"] && providerPresent["provider_completion_tokens"] && providerCounts["provider_total_tokens"] != providerCounts["provider_prompt_tokens"]+providerCounts["provider_completion_tokens"])) {
		return true
	}
	if value, present := row["topic_ref"]; present && !fullRef(value) {
		return true
	}
	if value, present := row["regression_case_ref"]; present && !fullRef(value) {
		return true
	}
	if value, present := row["regression_partition"]; present {
		partition, ok := value.(string)
		if !ok || (partition != "train" && partition != "holdout") {
			return true
		}
	}
	if value, present := row["traffic_class"]; present {
		trafficClass, ok := value.(string)
		if !ok || (trafficClass != "user" && trafficClass != "synthetic") {
			return true
		}
	}
	if value, present := row["synthetic"]; present && !exactBool(value) {
		return true
	}
	if admissionPresent != admissionRefPresent || (admissionPresent && (admission != contextPackOutcomeAdmissionSchemaID || !fullRef(admissionRef))) {
		return true
	}
	if value, present := row["verifier_id"]; present && !fullRef(value) {
		return true
	}
	if value, present := row["verification_evidence_digest"]; present && !fullRef(value) {
		return true
	}
	utility, utilityOK := optionalMap("utility")
	if !utilityOK || utility != nil && len(utility) == 0 {
		return true
	}
	if !mapHasOnly(utility, []string{"value", "unit", "verification_event_id", "evidence_digest", "verifier_kind", "verifier_id", "verification_passed"}) {
		return true
	}
	for _, field := range []string{"unit", "verification_event_id", "verifier_id"} {
		if value, present := utility[field]; present && !fullRef(value) {
			return true
		}
	}
	if value, present := utility["evidence_digest"]; present && !fullRef(value) {
		return true
	}
	if value, present := utility["verifier_kind"]; present {
		kind, ok := value.(string)
		if !ok || strings.ToLower(kind) != kind {
			return true
		}
		if _, allowed := utilityVerifierKinds[kind]; !allowed {
			return true
		}
	}
	if value, present := utility["value"]; present {
		if _, valid := finiteUtilityValue(value); !valid {
			return true
		}
	}
	if value, present := utility["verification_passed"]; present && !exactBool(value) {
		return true
	}
	economics, economicsOK := optionalMap("economics")
	if !economicsOK || economics != nil && len(economics) == 0 {
		return true
	}
	if !mapHasOnly(economics, []string{"latency_ms", "cost_microusd", "tool_calls", "failures"}) {
		return true
	}
	for _, field := range []string{"latency_ms", "cost_microusd", "tool_calls", "failures"} {
		if value, present := economics[field]; present {
			if _, ok := contextPackOutcomeBoundedCount(value, map[string]int64{
				"latency_ms": 86_400_000, "cost_microusd": 2_000_000_000, "tool_calls": 1_000_000, "failures": 1_000_000,
			}[field]); !ok {
				return true
			}
		}
	}
	pairing, pairingOK := optionalMap("pairing")
	if !pairingOK || pairing != nil && len(pairing) == 0 {
		return true
	}
	if !mapHasOnly(pairing, []string{"pair_id", "matched_control_outcome_id", "experiment_id", "model", "runner", "harness", "context_reconstruction_contract", "arm", "matching_method", "task_match_digest", "assignment_digest", "leakage_free"}) {
		return true
	}
	for _, field := range []string{"pair_id", "matched_control_outcome_id", "experiment_id", "model", "runner", "harness", "context_reconstruction_contract"} {
		if value, present := pairing[field]; present && !fullRef(value) {
			return true
		}
	}
	for _, field := range []string{"task_match_digest", "assignment_digest"} {
		if value, present := pairing[field]; present && !fullRef(value) {
			return true
		}
	}
	if value, present := pairing["matching_method"]; present {
		method, ok := value.(string)
		if !ok || strings.ToLower(method) != method {
			return true
		}
		if _, allowed := utilityExactMatchingMethods[method]; !allowed {
			return true
		}
	}
	if value, present := pairing["arm"]; present && value != "control" && value != "treatment" {
		return true
	}
	if value, present := pairing["leakage_free"]; present && !exactBool(value) {
		return true
	}
	evidenceAttribution, evidenceAttributionOK := optionalList("evidence_attribution")
	if !evidenceAttributionOK || len(evidenceAttribution) > evidenceReputationMaxAttributions {
		return true
	}
	for _, raw := range evidenceAttribution {
		attribution, attributionOK := raw.(map[string]any)
		if !attributionOK {
			return true
		}
		if !mapHasOnly(attribution, []string{"entity_type", "entity_id", "entity_ref", "candidate_ref", "attribution_method", "role", "issuer", "producer_agent_id", "verifier_id", "issuer_subject_ref", "producer_agent_id_subject_ref", "verifier_id_subject_ref", "entity_subject_ref", "verification_evidence_digest", "selection_state", "selection_ordinal", "evidence_role", "evidence_kind", "result_level_credit"}) {
			return true
		}
		entityType, entityTypeOK := attribution["entity_type"].(string)
		method, methodOK := attribution["attribution_method"].(string)
		role, roleOK := attribution["role"].(string)
		if !entityTypeOK || !methodOK || !roleOK ||
			!containsString([]string{"candidate", "source", "file", "agent", "memory"}, entityType) ||
			!containsString([]string{"explicit_verified", "counterfactual", "leave_one_out", "citation_loss"}, method) ||
			(role != "support" && role != "opposition") {
			return true
		}
		entityID, entityIDOK := attribution["entity_id"].(string)
		if !entityIDOK {
			return true
		}
		if entityType == "candidate" {
			candidateRef, candidateRefPresent := attribution["candidate_ref"]
			if !candidateRefPresent {
				candidateRef = entityID
			}
			candidateRefText, candidateRefOK := candidateRef.(string)
			if !candidateRefOK || contextPackOpaqueCandidateRef(candidateRefText) == "" {
				return true
			}
			if candidateRefPresent && candidateRefText != entityID {
				return true
			}
		} else {
			if _, present := attribution["candidate_ref"]; present || !fullRef(entityID) {
				return true
			}
		}
		if value, present := attribution["entity_ref"]; present && !fullRef(value) {
			return true
		}
		for _, field := range []string{"issuer", "producer_agent_id", "verifier_id", "issuer_subject_ref", "producer_agent_id_subject_ref", "verifier_id_subject_ref", "entity_subject_ref"} {
			if value, present := attribution[field]; present && !fullRef(value) {
				return true
			}
		}
		if value, present := attribution["verification_evidence_digest"]; present && !fullRef(value) {
			return true
		}
		if value, present := attribution["selection_state"]; present {
			state, ok := value.(string)
			if !ok || (state != "selected" && state != "omitted") {
				return true
			}
		}
		if value, present := attribution["selection_ordinal"]; present {
			ordinal, valid := contextPackOutcomeBoundedCount(value, 10_000)
			if !valid || ordinal == 0 {
				return true
			}
		}
		if value, present := attribution["evidence_role"]; present {
			evidenceRole, ok := value.(string)
			if !ok || (evidenceRole != "support" && evidenceRole != "opposition") {
				return true
			}
		}
		if value, present := attribution["evidence_kind"]; present {
			evidenceKind, ok := value.(string)
			if !ok || !containsString([]string{"decision", "risk", "check", "runbook", "capability", "fact", "graph_neighbor", "memory", "source", "unknown"}, evidenceKind) {
				return true
			}
		}
		if value, present := attribution["result_level_credit"]; present {
			credit, ok := value.(string)
			if !ok || (credit != "selection_receipt_bound" && credit != "unbound_legacy") {
				return true
			}
		}
	}
	attempts, attemptsOK := optionalMap("candidate_attribution_attempts")
	if !attemptsOK {
		return true
	}
	if attempts != nil {
		if !mapHasOnly(attempts, []string{"received", "invalid_ref"}) {
			return true
		}
		receivedRaw, receivedPresent := attempts["received"]
		invalidRaw, invalidPresent := attempts["invalid_ref"]
		received, receivedOK := contextPackOutcomeBoundedCount(receivedRaw, contextPackCandidateAttemptLimit)
		invalid, invalidOK := contextPackOutcomeBoundedCount(invalidRaw, contextPackCandidateAttemptLimit)
		if !receivedPresent || !invalidPresent || !receivedOK || !invalidOK || invalid > received {
			return true
		}
	}
	binding, bindingOK := optionalMap("attribution_binding")
	if !bindingOK {
		return true
	}
	if binding != nil {
		bindingSchemaID, bindingSchemaIDOK := binding["schema_id"].(string)
		if !mapHasOnly(binding, []string{"schema_id", "version", "sample_id_present", "candidate_attribution_received", "candidate_attribution_bound", "candidate_attribution_rejected", "legacy_unbound_count", "selection_receipt_id", "selection_receipt_digest", "exclusions"}) ||
			!bindingSchemaIDOK || bindingSchemaID != contextPackOutcomeBindingSchemaID {
			return true
		}
		version, versionOK := contextPackOutcomeBoundedCount(binding["version"], 1)
		if !versionOK || version != 1 || !exactBool(binding["sample_id_present"]) {
			return true
		}
		counts := map[string]int64{}
		for _, field := range []string{"candidate_attribution_received", "candidate_attribution_bound", "candidate_attribution_rejected", "legacy_unbound_count"} {
			count, valid := contextPackOutcomeBoundedCount(binding[field], evidenceReputationMaxAttributions)
			if !valid {
				return true
			}
			counts[field] = count
		}
		if counts["candidate_attribution_received"] > contextPackCandidateAttemptLimit ||
			counts["candidate_attribution_bound"] > counts["candidate_attribution_received"] ||
			counts["candidate_attribution_rejected"] > counts["candidate_attribution_received"] ||
			counts["candidate_attribution_bound"]+counts["candidate_attribution_rejected"] != counts["candidate_attribution_received"] {
			return true
		}
		receiptIDRaw, receiptIDPresent := binding["selection_receipt_id"]
		receiptDigestRaw, receiptDigestPresent := binding["selection_receipt_digest"]
		if !receiptIDPresent || !receiptDigestPresent {
			return true
		}
		receiptID, receiptIDOK := receiptIDRaw.(string)
		receiptDigest, receiptDigestOK := receiptDigestRaw.(string)
		if !receiptIDOK || !receiptDigestOK {
			return true
		}
		if receiptID != "" && !contextPackQualitySelectionReceiptIDValid(receiptID) {
			return true
		}
		if receiptDigest != "" && !fullRef(receiptDigest) {
			return true
		}
		exclusions, exclusionsOK := binding["exclusions"].(map[string]any)
		if !exclusionsOK || !mapHasOnly(exclusions, []string{"candidate_ref_invalid", "candidate_duplicate", "candidate_receipt_missing", "candidate_receipt_not_durable", "candidate_project_scope_missing", "candidate_project_mismatch", "candidate_not_receipted"}) {
			return true
		}
		for _, value := range exclusions {
			count, valid := contextPackOutcomeBoundedCount(value, contextPackCandidateAttemptLimit)
			if !valid || count == 0 {
				return true
			}
		}
	}
	stability, stabilityOK := optionalMap("stability")
	if !stabilityOK {
		return true
	}
	if stability != nil {
		if !mapHasOnly(stability, []string{"stable", "run_count", "external_state", "result_digests"}) ||
			!exactBool(stability["stable"]) || !exactBool(stability["external_state"]) {
			return true
		}
		runCount, runCountOK := contextPackOutcomeBoundedCount(stability["run_count"], 20)
		if !runCountOK {
			return true
		}
		_ = runCount
		if rawDigests, present := stability["result_digests"]; present {
			digests, ok := rawDigests.([]any)
			if !ok || len(digests) > 20 {
				return true
			}
			for _, digest := range digests {
				if !fullRef(digest) {
					return true
				}
			}
		}
	}
	return false
}

func contextPackQualitySelectionReceiptIDValid(value string) bool {
	if len(value) != len("cpr_")+24 || !strings.HasPrefix(value, "cpr_") {
		return false
	}
	for _, character := range value[len("cpr_"):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}

func contextPackQualityUnsafeReporterString(value any) bool {
	text := strings.TrimSpace(anyToString(value))
	return strings.ContainsAny(text, "/\\?=") || strings.ContainsAny(text, " \t\r\n")
}

func (t *contextPackQualityTelemetry) recordRegressionFixtureDetailed(raw map[string]any) (string, bool, error) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if t == nil || t.regressionFixtures == nil {
		return "", false, errors.New("context-pack regression fixture persistence is unavailable")
	}
	return t.regressionFixtures.recordDetailed(raw)
}

func (t *contextPackQualityTelemetry) recordQuality(sample map[string]any) {
	if t == nil {
		return
	}
	entry := contextPackQualityEntryFromSample(sample)
	if len(entry) == 0 {
		return
	}
	if receipt := contextPackSelectionReceiptFromSample(entry["selection_receipt"]); len(receipt) > 0 {
		entry["selection_receipt"] = receipt
		if contextPackQualityLedgerAvailable(t.ledger) {
			if err := t.ledger.append(entry); err == nil {
				t.mu.Lock()
				t.applyQualityEntryLocked(entry)
				t.markDurableReceiptSampleLocked(entry)
				t.mu.Unlock()
				return
			} else {
				t.ledger.setError(err)
			}
		}
		// A receipt that was not durably acknowledged must never become an
		// in-memory candidate-binding source. Keep ordinary quality telemetry
		// observable, but remove the unusable receipt before recording it.
		delete(entry, "selection_receipt")
		t.mu.Lock()
		t.applyQualityEntryLocked(entry)
		t.mu.Unlock()
		return
	}
	t.mu.Lock()
	t.applyQualityEntryLocked(entry)
	t.mu.Unlock()
	if t.ledger != nil && t.ledger.enabled {
		if err := t.ledger.append(entry); err != nil {
			t.ledger.setError(err)
		}
	}
}

func (t *contextPackQualityTelemetry) recordOutcome(sample map[string]any) bool {
	recorded, _ := t.recordOutcomeDurably(sample)
	return recorded
}

func (t *contextPackQualityTelemetry) recordOutcomeDurably(sample map[string]any) (bool, error) {
	if t == nil {
		return false, nil
	}
	entry := contextPackQualityOutcomeFromSample(sample)
	if len(entry) == 0 {
		return false, nil
	}
	return t.recordOutcomeEntryDurably(entry)
}

// recordOutcomeEntry persists an already-normalized outcome. The HTTP ingress
// binds result attribution before both the quality and Utility Ledger paths;
// re-normalizing here would erase that verified binding.
func (t *contextPackQualityTelemetry) recordOutcomeEntry(entry map[string]any) bool {
	recorded, _ := t.recordOutcomeEntryDurably(entry)
	return recorded
}

func (t *contextPackQualityTelemetry) recordOutcomeEntryDurably(entry map[string]any) (bool, error) {
	if t == nil || anyToString(entry["schema_id"]) != contextPackQualityOutcomeSchemaID {
		return false, nil
	}
	entry = cloneJSONMap(entry)
	t.mu.Lock()
	defer t.mu.Unlock()

	outcomeKey := contextPackQualityOutcomeKey(entry)
	candidateReceiptBound := contextPackOutcomeHasReceiptBoundCandidate(entry)
	requiresDurableAdmission := contextPackOutcomeRequiresDurableAdmission(entry)
	if requiresDurableAdmission {
		if t.ledger == nil {
			return false, errContextPackReceiptLedgerUnavailable
		}
		// A post-rename acknowledgement failure can leave readable bytes behind
		// without proving their durable replacement. Re-acknowledge the complete
		// bounded ledger before a retry may promote such an outcome.
		if err := t.ledger.acknowledgeDurability(); err != nil {
			t.ledger.setError(err)
			return false, err
		}
	}
	// Read the complete ledger when it is enabled. Startup intentionally keeps a
	// bounded projection, but any retained outcome_id must still have one stable
	// logical claim across eviction and restart. The ledger's durable row wins
	// over a bounded resident copy.
	existing, found, durableErr := t.durableOutcomeEntryLocked(outcomeKey)
	if durableErr != nil {
		if errors.Is(durableErr, errUtilityOutcomeConflict) {
			return false, durableErr
		}
		if requiresDurableAdmission {
			return false, errContextPackReceiptLedgerUnavailable
		}
	}
	if !found {
		existing, found = t.outcomeEntryLocked(outcomeKey)
	}
	if found {
		if contextPackOutcomeLogicalClaimDigest(existing) != contextPackOutcomeLogicalClaimDigest(entry) {
			return false, errUtilityOutcomeConflict
		}
		if requiresDurableAdmission {
			if !contextPackOutcomeRequiresDurableAdmission(existing) {
				return false, errContextPackReceiptLedgerUnavailable
			}
		}
		if candidateReceiptBound {
			if !contextPackOutcomeHasReceiptBoundCandidate(existing) || !t.candidateOutcomeReceiptDurableLocked(existing) {
				return false, errContextPackReceiptLedgerUnavailable
			}
		}
		if contextPackOutcomeHasAuthoritativeSampleAdmission(existing) && !t.authoritativeOutcomeSampleDurableLocked(existing) {
			return false, errContextPackReceiptLedgerUnavailable
		}
		_, admitted := t.outcomeKeys[outcomeKey]
		if (candidateReceiptBound || contextPackOutcomeHasAuthoritativeSampleAdmission(existing)) && !admitted {
			// A prior append may have committed bytes but failed its final
			// acknowledgement. Once all durable admission proofs re-check, admit
			// the canonical row before Utility reconciliation.
			_ = t.applyOutcomeEntryLocked(existing)
		}
		return false, nil
	}
	if _, exists := t.outcomeKeys[outcomeKey]; exists {
		// Retention may have discarded a legacy non-candidate entry when its
		// ledger is disabled or no longer retains it. Preserve that legacy
		// behavior; receipt-bound candidates cannot make that weaker claim.
		if requiresDurableAdmission {
			return false, errContextPackReceiptLedgerUnavailable
		}
		return false, nil
	}
	if requiresDurableAdmission {
		// The quality mutex intentionally remains held while the ledger append
		// runs. No path holds the ledger mutex before acquiring this mutex, so
		// this serializes duplicate candidate POSTs without a lock inversion.
		if !contextPackQualityLedgerAvailable(t.ledger) {
			return false, errContextPackReceiptLedgerUnavailable
		}
		if err := t.ledger.append(entry); err != nil {
			t.ledger.setError(err)
			return false, err
		}
		if candidateReceiptBound {
			// Compaction may retain this newest row while dropping its older receipt.
			// Re-read the exact retained binding before the outcome can enter proof.
			if !t.candidateOutcomeReceiptDurableLocked(entry) {
				return false, errContextPackReceiptLedgerUnavailable
			}
		}
		if contextPackOutcomeHasAuthoritativeSampleAdmission(entry) && !t.authoritativeOutcomeSampleDurableLocked(entry) {
			return false, errContextPackReceiptLedgerUnavailable
		}
		return t.applyOutcomeEntryLocked(entry), nil
	}

	recorded := t.applyOutcomeEntryLocked(entry)
	if !recorded {
		return false, nil
	}
	if t.ledger != nil && t.ledger.enabled {
		if err := t.ledger.append(entry); err != nil {
			t.ledger.setError(err)
		}
	}
	return true, nil
}

// contextPackOutcomeLogicalClaimDigest deliberately shares the Utility
// Ledger's normalized claim identity. gateway_received_at is an authoritative
// HTTP receipt time, rather than reporter claim content, so it must not turn a
// retry into a conflict. All logical outcome fields, including utility and
// receipt binding, remain part of the identity.
func contextPackOutcomeLogicalClaimDigest(entry map[string]any) string {
	return utilitySourceClaimDigest(entry)
}

// durableOutcomeEntryLocked resolves an outcome from the complete quality
// ledger while t.mu is held. readRows() is intentionally bounded for startup,
// but replay safety must be able to find a still-retained row past that window.
func (t *contextPackQualityTelemetry) durableOutcomeEntryLocked(outcomeID string) (map[string]any, bool, error) {
	if t == nil || strings.TrimSpace(outcomeID) == "" || !contextPackQualityLedgerAvailable(t.ledger) {
		return nil, false, nil
	}
	// A directory can never be an NDJSON ledger. Leave its failure to append(),
	// which records the durable-write error consistently with the rest of the
	// quality boundary instead of misclassifying a new candidate as a read-only
	// replay lookup failure.
	if info, err := os.Stat(t.ledger.path); err == nil && !info.Mode().IsRegular() {
		return nil, false, nil
	}
	t.ledger.mu.Lock()
	rows, _, err := t.ledger.readRowsUnlocked()
	t.ledger.mu.Unlock()
	if err != nil {
		t.ledger.setError(err)
		return nil, false, err
	}
	var found map[string]any
	for _, row := range rows {
		if anyToString(row["schema_id"]) != contextPackQualityOutcomeSchemaID || anyToString(row["outcome_id"]) != outcomeID {
			continue
		}
		if found != nil && contextPackOutcomeLogicalClaimDigest(found) != contextPackOutcomeLogicalClaimDigest(row) {
			return cloneJSONMap(found), true, errUtilityOutcomeConflict
		}
		found = cloneJSONMap(row)
	}
	if found == nil {
		return nil, false, nil
	}
	return found, true, nil
}

func (t *contextPackQualityTelemetry) outcomeEntryLocked(outcomeID string) (map[string]any, bool) {
	if t == nil {
		return nil, false
	}
	for index := len(t.outcomes) - 1; index >= 0; index-- {
		if anyToString(t.outcomes[index]["outcome_id"]) == outcomeID {
			return t.outcomes[index], true
		}
	}
	return nil, false
}

func contextPackQualityOutcomeKey(entry map[string]any) string {
	outcomeKey := strings.TrimSpace(anyToString(entry["outcome_id"]))
	if outcomeKey != "" {
		return outcomeKey
	}
	outcomeKey = "cpo_" + sha256Hex(anyToString(entry["sample_id"]) + "\x00" + anyToString(entry["capturedAt"]))[:24]
	entry["outcome_id"] = outcomeKey
	return outcomeKey
}

func contextPackOutcomeHasReceiptBoundCandidate(entry map[string]any) bool {
	for _, raw := range contextPackAnyList(entry["evidence_attribution"]) {
		attribution := anyMap(raw)
		if anyToString(attribution["entity_type"]) == "candidate" &&
			anyToString(attribution["result_level_credit"]) == "selection_receipt_bound" {
			return true
		}
	}
	return false
}

// Regression fixtures can be reconstructed only from their owner-only sidecar,
// so an outcome that references one is admitted under the same acknowledged
// quality-ledger boundary as a receipt-bound candidate.
func contextPackOutcomeRequiresDurableAdmission(entry map[string]any) bool {
	return contextPackOutcomeHasReceiptBoundCandidate(entry) || utilitySHA256DigestValid(anyToString(entry["regression_case_ref"])) || contextPackOutcomeHasAuthoritativeSampleAdmission(entry)
}

func contextPackQualityLedgerAvailable(ledger *contextPackQualityLedger) bool {
	if ledger == nil {
		return false
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	return ledger.enabled && !ledger.durabilityUnacknowledged && strings.TrimSpace(ledger.path) != ""
}

func (t *contextPackQualityTelemetry) outcomeForUtility(outcomeID string) (map[string]any, bool) {
	if t == nil || strings.TrimSpace(outcomeID) == "" {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for index := len(t.outcomes) - 1; index >= 0; index-- {
		if anyToString(t.outcomes[index]["outcome_id"]) == outcomeID {
			return cloneJSONMap(t.outcomes[index]), true
		}
	}
	existing, found, err := t.durableOutcomeEntryLocked(outcomeID)
	if err != nil {
		return nil, false
	}
	return existing, found
}

func contextPackQualityEntryFromSample(sample map[string]any) map[string]any {
	sampleID := contextPackQualityIdentifier(sample["sample_id"], 200)
	if sampleID == "" {
		return nil
	}
	project := contextPackQualityIdentifier(sample["project"], 160)
	qualityScore := clampInt(anyToInt(sample["quality_score"], 0), 0, 100)
	exactSaved := anyToInt(sample["exact_prompt_tokens_saved"], 0)
	if exactSaved < 0 {
		exactSaved = 0
	}
	modeledAvoided := anyToInt(sample["modeled_inference_tokens_avoided"], 0)
	if modeledAvoided < 0 {
		modeledAvoided = 0
	}
	modeledCalls := anyToFloat(sample["modeled_extra_calls_avoided"])
	if modeledCalls < 0 {
		modeledCalls = 0
	}
	entry := map[string]any{
		"schema_id":                          contextPackQualitySchemaID,
		"version":                            1,
		"capturedAt":                         contextPackQualityTimestamp(firstPresentAny(sample["capturedAt"], sample["captured_at"])),
		"sample_id":                          sampleID,
		"query_hash":                         contextPackQualityQueryHash(sample["query_hash"]),
		"project":                            project,
		"task_class":                         contextPackQualityIdentifier(strings.ToLower(anyToString(sample["task_class"])), 80),
		"retrieval_intent":                   contextPackQualityIdentifier(strings.ToLower(anyToString(sample["retrieval_intent"])), 80),
		"quality_score":                      qualityScore,
		"confidence":                         contextPackQualityConfidence(sample["confidence"]),
		"calibration_grade":                  contextPackQualityCalibrationGrade(sample["calibration_grade"]),
		"exact_prompt_tokens_saved":          exactSaved,
		"modeled_inference_tokens_avoided":   modeledAvoided,
		"modeled_extra_calls_avoided":        roundFloat(modeledCalls, 3),
		"counterfactual_baseline":            contextPackQualityCounterfactualBaseline(sample["counterfactual_baseline"]),
		"ranked_evidence_count":              anyToInt(sample["ranked_evidence_count"], 0),
		"high_impact_evidence_count":         anyToInt(sample["high_impact_evidence_count"], 0),
		"omitted_high_value_count":           anyToInt(sample["omitted_high_value_count"], 0),
		"returned_source_count":              anyToInt(sample["returned_source_count"], 0),
		"warning_count":                      anyToInt(sample["warning_count"], 0),
		"tokenizer_exact":                    anyToBool(sample["tokenizer_exact"]),
		"wire_tokens_exact":                  anyToInt(firstPresentAny(sample["wire_tokens_exact"], sample["transport_tokens_exact"]), 0),
		"model_visible_context_tokens_exact": anyToInt(sample["model_visible_context_tokens_exact"], 0),
		"token_budget_active":                anyToBool(sample["token_budget_active"]),
		"source_coverage_complete":           anyToBool(sample["source_coverage_complete"]),
		"graph_context_used":                 anyToBool(sample["graph_context_used"]),
		"model_call_token_basis":             anyToInt(sample["model_call_token_basis"], 0),
		"raw_retry_probability_estimate":     roundFloat(anyToFloat(sample["raw_retry_probability_estimate"]), 3),
		"packed_retry_probability_estimate":  roundFloat(anyToFloat(sample["packed_retry_probability_estimate"]), 3),
	}
	if topicRef := contextPackQualityTopicRefFromSample(sample, project); topicRef != "" {
		entry["topic_ref"] = topicRef
	}
	copyContextPackQualityProofIdentity(entry, sample)
	if receipt := contextPackSelectionReceiptFromSample(sample["selection_receipt"]); len(receipt) > 0 {
		entry["selection_receipt"] = receipt
	}
	if encoding := contextPackQualityIdentifier(sample["tokenizer_encoding"], 80); encoding != "" {
		entry["tokenizer_encoding"] = encoding
	}
	return entry
}

// contextPackQualityIdentifier is intentionally stricter than a clipped
// string: quality and outcome rows are durable/public telemetry, so identifiers
// cannot carry paths, prompts, queries, or arbitrary reporter prose.
func contextPackQualityIdentifier(value any, limit int) string {
	text := strings.TrimSpace(anyToString(value))
	if text == "" || len(text) > limit {
		return ""
	}
	for _, character := range text {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' || character == '.' || character == ':' {
			continue
		}
		return ""
	}
	return text
}

func contextPackQualityTimestamp(value any) string {
	text := strings.TrimSpace(anyToString(value))
	if text != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UTC().Format(time.RFC3339Nano)
		}
	}
	return nowUTCISO()
}

func contextPackQualityQueryHash(value any) string {
	text := strings.ToLower(strings.TrimSpace(anyToString(value)))
	if len(text) != 16 && !utilitySHA256DigestValid(text) && len(text) != 64 {
		return ""
	}
	if strings.HasPrefix(text, "sha256:") {
		return text
	}
	for _, character := range text {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return ""
		}
	}
	return text
}

func contextPackQualityConfidence(value any) string {
	switch strings.ToLower(strings.TrimSpace(anyToString(value))) {
	case "medium", "high":
		return strings.ToLower(strings.TrimSpace(anyToString(value)))
	default:
		return "low"
	}
}

func contextPackQualityCalibrationGrade(value any) string {
	switch strings.ToLower(strings.TrimSpace(anyToString(value))) {
	case "outcome_seeded", "outcome_adjusted", "tokenizer_exact":
		return strings.ToLower(strings.TrimSpace(anyToString(value)))
	default:
		return "modeled_counterfactual"
	}
}

func contextPackQualityCounterfactualBaseline(value any) string {
	switch strings.ToLower(strings.TrimSpace(anyToString(value))) {
	case "retrieval_replay", "paired_replay":
		return strings.ToLower(strings.TrimSpace(anyToString(value)))
	default:
		return "raw_candidate_replay"
	}
}

// contextPackQualityTopicRef preserves deterministic topic grouping without
// persisting or returning the underlying topic path. The project is already a
// retained scope field, but binding it in the ref also prevents cross-project
// topic correlation through a shared path digest.
func contextPackQualityTopicRef(project, topicPath string) string {
	project = strings.ToLower(strings.TrimSpace(project))
	topicPath = strings.Trim(strings.TrimSpace(topicPath), "/")
	if topicPath == "" {
		return ""
	}
	return "sha256:" + sha256Hex("context-pack-quality-topic.v1\x00"+project+"\x00"+topicPath)
}

// contextPackQualityOpaqueReporterRef turns every reporter-chosen identifier
// admitted into public/durable outcome telemetry into a project- and
// field-bound full digest. Identifier syntax checks alone are not a secrecy
// boundary: compact credentials and customer names often look identifier-safe.
func contextPackQualityOpaqueReporterRef(project, field string, value any, limit int) string {
	project = strings.ToLower(contextPackQualityIdentifier(project, 160))
	field = strings.ToLower(contextPackQualityIdentifier(field, 96))
	identifier := contextPackQualityIdentifier(value, limit)
	if project == "" || field == "" || identifier == "" {
		return ""
	}
	return "sha256:" + sha256Hex("context-pack-quality-reporter.v1\x00"+project+"\x00"+field+"\x00"+identifier)
}

func contextPackQualityTopicRefFromSample(sample map[string]any, project string) string {
	if topicPath := strings.TrimSpace(anyToString(sample["topic_path"])); topicPath != "" {
		return contextPackQualityTopicRef(project, topicPath)
	}
	if ref := strings.TrimSpace(anyToString(sample["topic_ref"])); utilitySHA256DigestValid(ref) {
		return strings.ToLower(ref)
	}
	return ""
}

func copyContextPackQualityProofIdentity(dst, source map[string]any) {
	if dst == nil || source == nil {
		return
	}
	for _, alias := range []struct {
		canonical string
		keys      []string
		limit     int
	}{
		{canonical: "sample_id", keys: []string{"sample_id", "context_pack_quality_sample_id"}, limit: 200},
		{canonical: "session_id", keys: []string{"session_id", "sessionId"}, limit: maxAgentSessionIDLength},
		{canonical: "task_id", keys: []string{"task_id", "taskId"}, limit: 160},
		{canonical: "task_identity_id", keys: []string{"task_identity_id", "taskIdentityId"}, limit: 160},
		{canonical: "execution_lane_id", keys: []string{"execution_lane_id", "executionLaneId"}, limit: 160},
		{canonical: "agent_id", keys: []string{"agent_id", "agentId"}, limit: 160},
	} {
		if strings.TrimSpace(anyToString(dst[alias.canonical])) != "" {
			continue
		}
		for _, key := range alias.keys {
			if value := contextPackQualityIdentifier(source[key], alias.limit); value != "" {
				dst[alias.canonical] = value
				break
			}
		}
	}
}

// contextPackCandidateAttributionAttempts is an ingress-only counter. It
// retains no reporter value, but lets the binding boundary account for invalid
// candidate references that normalized attribution intentionally discards.
func contextPackCandidateAttributionAttempts(sample map[string]any) map[string]any {
	rows := parseRows(firstPresentAny(sample["evidence_attribution"], sample["evidence_attributions"]))
	if len(rows) > contextPackCandidateAttemptLimit {
		rows = rows[:contextPackCandidateAttemptLimit]
	}
	received := 0
	invalidRef := 0
	for _, row := range rows {
		if !strings.EqualFold(strings.TrimSpace(anyToString(row["entity_type"])), "candidate") {
			continue
		}
		received++
		if contextPackOpaqueCandidateRef(firstPresentAny(row["candidate_ref"], row["entity_id"])) == "" {
			invalidRef++
		}
	}
	if received == 0 {
		return nil
	}
	return map[string]any{
		"received":    received,
		"invalid_ref": invalidRef,
	}
}

func contextPackOpaqueCandidateRef(value any) string {
	ref := strings.TrimSpace(anyToString(value))
	if isSearchIntelligenceFullSHA256Ref(ref) {
		return strings.ToLower(ref)
	}
	if len(ref) != len("rtc_")+24 || !strings.HasPrefix(ref, "rtc_") {
		return ""
	}
	for _, character := range ref[len("rtc_"):] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return ""
		}
	}
	return ref
}

func contextPackSelectionEvidenceKind(value any) string {
	switch strings.ToLower(strings.TrimSpace(anyToString(value))) {
	case "decision", "risk", "check", "runbook", "capability", "fact", "graph_neighbor", "memory", "source":
		return strings.ToLower(strings.TrimSpace(anyToString(value)))
	default:
		return "unknown"
	}
}

func contextPackSelectionEvidenceRole(value any) string {
	if strings.EqualFold(strings.TrimSpace(anyToString(value)), "opposition") {
		return "opposition"
	}
	return "support"
}

func contextPackSelectionReceiptCandidate(row map[string]any, state string, fallbackOrdinal int) map[string]any {
	if state != "selected" && state != "omitted" {
		return nil
	}
	ref := contextPackOpaqueCandidateRef(firstPresentAny(row["candidate_ref"], row["candidate_id"], row["entity_id"]))
	if ref == "" {
		return nil
	}
	ordinal := anyToInt(firstPresentAny(row["ordinal"], row["rank"]), fallbackOrdinal)
	if ordinal <= 0 {
		ordinal = fallbackOrdinal
	}
	return map[string]any{
		"candidate_ref":   ref,
		"selection_state": state,
		"ordinal":         clampInt(ordinal, 1, 10_000),
		"evidence_role":   contextPackSelectionEvidenceRole(row["role"]),
		"evidence_kind":   contextPackSelectionEvidenceKind(firstPresentAny(row["evidence_kind"], row["kind"])),
	}
}

func contextPackSelectionReceiptFromCandidates(candidates []map[string]any) map[string]any {
	if len(candidates) > contextPackSelectionReceiptLimit {
		candidates = candidates[:contextPackSelectionReceiptLimit]
	}
	selected := 0
	omitted := 0
	rows := make([]any, 0, len(candidates))
	for _, candidate := range candidates {
		if anyToString(candidate["selection_state"]) == "selected" {
			selected++
		} else {
			omitted++
		}
		rows = append(rows, candidate)
	}
	receiptDigestParts := make([]string, 0, len(candidates)+1)
	receiptDigestParts = append(receiptDigestParts, contextPackSelectionReceiptSchemaID)
	for _, candidate := range candidates {
		receiptDigestParts = append(receiptDigestParts, strings.Join([]string{
			anyToString(candidate["candidate_ref"]), anyToString(candidate["selection_state"]),
			anyToString(candidate["ordinal"]), anyToString(candidate["evidence_role"]), anyToString(candidate["evidence_kind"]),
		}, "\x00"))
	}
	digest := sha256Hex(strings.Join(receiptDigestParts, "\x00"))
	return map[string]any{
		"schema_id":       contextPackSelectionReceiptSchemaID,
		"version":         1,
		"receipt_id":      "cpr_" + digest[:24],
		"receipt_digest":  "sha256:" + digest,
		"candidate_limit": contextPackSelectionReceiptLimit,
		"candidate_count": len(rows),
		"selected_count":  selected,
		"omitted_count":   omitted,
		"candidates":      rows,
	}
}

// contextPackSelectionReceipt records selection facts only. It deliberately
// excludes candidate text, summaries, source locations, and query data.
func contextPackSelectionReceipt(ranked, omitted any) map[string]any {
	candidates := make([]map[string]any, 0, contextPackSelectionReceiptLimit)
	seen := map[string]struct{}{}
	appendRows := func(rows []map[string]any, state string) {
		for index, row := range rows {
			if len(candidates) >= contextPackSelectionReceiptLimit {
				return
			}
			candidate := contextPackSelectionReceiptCandidate(row, state, index+1)
			if len(candidate) == 0 {
				continue
			}
			ref := anyToString(candidate["candidate_ref"])
			if _, duplicate := seen[ref]; duplicate {
				continue
			}
			seen[ref] = struct{}{}
			candidates = append(candidates, candidate)
		}
	}
	appendRows(parseRows(ranked), "selected")
	appendRows(parseRows(omitted), "omitted")
	return contextPackSelectionReceiptFromCandidates(candidates)
}

// contextPackSelectionReceiptFromSample is the durable ledger boundary. Even
// internally generated receipts are re-parsed to prevent a future caller from
// accidentally adding raw retrieval content to the quality ledger.
func contextPackSelectionReceiptFromSample(value any) map[string]any {
	raw := anyMap(value)
	if len(raw) == 0 {
		return nil
	}
	candidates := make([]map[string]any, 0, contextPackSelectionReceiptLimit)
	seen := map[string]struct{}{}
	for index, row := range parseRows(raw["candidates"]) {
		if len(candidates) >= contextPackSelectionReceiptLimit {
			break
		}
		state := strings.ToLower(strings.TrimSpace(anyToString(row["selection_state"])))
		candidate := contextPackSelectionReceiptCandidate(row, state, index+1)
		if len(candidate) == 0 {
			continue
		}
		ref := anyToString(candidate["candidate_ref"])
		if _, duplicate := seen[ref]; duplicate {
			continue
		}
		seen[ref] = struct{}{}
		candidates = append(candidates, candidate)
	}
	return contextPackSelectionReceiptFromCandidates(candidates)
}

// contextPackOutcomeBoundedCount accepts only an exact, finite, non-negative
// integer. HTTP ingress decodes JSON with UseNumber so a native JSON number
// retains its exact spelling until this boundary. The integer cases preserve
// internal call sites without accepting reporter strings, booleans, or casts.
func contextPackOutcomeBoundedCount(value any, maximum int64) (int64, bool) {
	var count int64
	switch typed := value.(type) {
	case int:
		count = int64(typed)
	case int8:
		count = int64(typed)
	case int16:
		count = int64(typed)
	case int32:
		count = int64(typed)
	case int64:
		count = typed
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		count = int64(typed)
	case uint8:
		count = int64(typed)
	case uint16:
		count = int64(typed)
	case uint32:
		count = int64(typed)
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		count = int64(typed)
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed || typed < 0 || typed > float64(maximum) {
			return 0, false
		}
		count = int64(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		count = parsed
	default:
		return 0, false
	}
	if count < 0 || count > maximum {
		return 0, false
	}
	return count, true
}

// contextPackOutcomeCountClaim reconciles synonymous count fields. If a
// reporter supplies multiple aliases, every supplied value must be a strict
// bounded integer and all values must agree; silent precedence would make a
// forged claim appear valid in a replay.
func contextPackOutcomeCountClaim(maximum int64, values ...any) (int64, bool, error) {
	var count int64
	present := false
	for _, raw := range values {
		if raw == nil {
			continue
		}
		parsed, valid := contextPackOutcomeBoundedCount(raw, maximum)
		if !valid {
			return 0, false, errContextPackOutcomeInvalidNumeric
		}
		if present && count != parsed {
			return 0, false, errContextPackOutcomeInvalidNumeric
		}
		count = parsed
		present = true
	}
	return count, present, nil
}

func contextPackOutcomeProviderUsageMaps(sample map[string]any) []map[string]any {
	return []map[string]any{
		anyMap(sample["provider_usage"]),
		anyMap(sample["usage"]),
		anyMap(sample["token_usage"]),
	}
}

func contextPackOutcomeSaturatingAdd(total *int64, value int64) {
	if total == nil || value <= 0 {
		return
	}
	// Legacy ledgers may contain a previously-overflowed negative aggregate.
	// Never let that state wrap again or make a new report reduce a count.
	if *total < 0 {
		*total = 0
	}
	if *total >= math.MaxInt64-value {
		*total = math.MaxInt64
		return
	}
	*total += value
}

func contextPackQualityOutcomeFromSample(sample map[string]any) map[string]any {
	entry, _ := contextPackQualityOutcomeFromSampleChecked(sample)
	return entry
}

func contextPackQualityOutcomeFromSampleChecked(sample map[string]any) (map[string]any, error) {
	sampleID := contextPackQualityIdentifier(firstNonEmptyStrings(anyToString(sample["sample_id"]), anyToString(sample["context_pack_quality_sample_id"])), 200)
	taskID := contextPackQualityIdentifier(firstNonEmptyStrings(anyToString(sample["task_id"]), anyToString(sample["taskId"])), 160)
	firstPassRaw, firstPassPresent := contextPackOutcomeFirstPresent(sample, "first_pass_success", "succeeded_first_pass", "success_first_pass")
	repairRaw, repairPresent := contextPackOutcomeFirstPresent(sample, "repair_required", "needed_repair", "repair")
	retryCount, _, err := contextPackOutcomeCountClaim(contextPackOutcomeMaxRetryCount, sample["retry_count"], sample["retries"])
	if err != nil {
		return nil, err
	}
	followupTokens, _, err := contextPackOutcomeCountClaim(contextPackOutcomeMaxFollowupTokens,
		sample["followup_tokens"], sample["actual_followup_tokens"], sample["repair_tokens"], sample["observed_followup_tokens"])
	if err != nil {
		return nil, err
	}
	providerUsage := contextPackOutcomeProviderUsageMaps(sample)
	providerPromptTokens, promptPresent, err := contextPackOutcomeCountClaim(contextPackOutcomeMaxProviderComponentTokens,
		sample["provider_prompt_tokens"], sample["prompt_tokens"], sample["input_tokens"],
		providerUsage[0]["prompt_tokens"], providerUsage[0]["input_tokens"],
		providerUsage[1]["prompt_tokens"], providerUsage[1]["input_tokens"],
		providerUsage[2]["prompt_tokens"], providerUsage[2]["input_tokens"],
	)
	if err != nil {
		return nil, err
	}
	providerCompletionTokens, completionPresent, err := contextPackOutcomeCountClaim(contextPackOutcomeMaxProviderComponentTokens,
		sample["provider_completion_tokens"], sample["completion_tokens"], sample["output_tokens"],
		providerUsage[0]["completion_tokens"], providerUsage[0]["output_tokens"],
		providerUsage[1]["completion_tokens"], providerUsage[1]["output_tokens"],
		providerUsage[2]["completion_tokens"], providerUsage[2]["output_tokens"],
	)
	if err != nil {
		return nil, err
	}
	providerTotalTokens, totalPresent, err := contextPackOutcomeCountClaim(contextPackOutcomeMaxProviderTotalTokens,
		sample["provider_total_tokens"], sample["total_tokens"],
		providerUsage[0]["total_tokens"], providerUsage[1]["total_tokens"], providerUsage[2]["total_tokens"],
	)
	if err != nil {
		return nil, err
	}
	providerComponents := providerPromptTokens + providerCompletionTokens
	if totalPresent {
		if (promptPresent && providerTotalTokens < providerPromptTokens) ||
			(completionPresent && providerTotalTokens < providerCompletionTokens) ||
			(promptPresent && completionPresent && providerTotalTokens != providerComponents) {
			return nil, errContextPackOutcomeInvalidNumeric
		}
	} else if promptPresent || completionPresent {
		providerTotalTokens = providerComponents
	}
	project := contextPackQualityIdentifier(sample["project"], 160)
	outcomeSource := "agent_report"
	if rawOutcomeSource := contextPackQualityIdentifier(sample["outcome_source"], 80); rawOutcomeSource != "" && rawOutcomeSource != "agent_report" {
		if ref := contextPackQualityOpaqueReporterRef(project, "outcome_source", rawOutcomeSource, 80); ref != "" {
			outcomeSource = ref
		}
	}
	calibrationRaw, calibrationPresent := contextPackOutcomeFirstPresent(sample, "calibration_eligible")
	calibrationEligible := true
	if calibrationPresent {
		calibrationEligible = anyToBool(calibrationRaw)
	}
	outcomeClass := contextPackQualityIdentifier(strings.ToLower(anyToString(sample["outcome_class"])), 80)
	if outcomeClass == "" {
		if anyToBool(firstPassRaw) {
			outcomeClass = "success"
		} else if anyToBool(repairRaw) || retryCount > 0 {
			outcomeClass = "repair_required"
		} else {
			outcomeClass = "unspecified"
		}
	}
	attribution := firstNonEmptyStrings(contextPackQualityIdentifier(strings.ToLower(anyToString(sample["context_attribution"])), 80), "unknown")
	outcomeID := contextPackQualityIdentifier(sample["outcome_id"], 200)
	if outcomeID == "" {
		seed := strings.Join([]string{
			sampleID,
			taskID,
			outcomeSource,
			outcomeClass,
			anyToString(anyToBool(firstPassRaw)),
			anyToString(anyToBool(repairRaw)),
			anyToString(retryCount),
			anyToString(followupTokens),
		}, "\x00")
		outcomeID = "cpo_" + sha256Hex(seed)[:24]
	}
	entry := map[string]any{
		"schema_id":                contextPackQualityOutcomeSchemaID,
		"version":                  1,
		"capturedAt":               contextPackQualityTimestamp(firstPresentAny(sample["capturedAt"], sample["captured_at"])),
		"outcome_id":               outcomeID,
		"sample_id":                sampleID,
		"task_id":                  taskID,
		"project":                  project,
		"task_class":               contextPackQualityIdentifier(strings.ToLower(anyToString(sample["task_class"])), 80),
		"retrieval_intent":         contextPackQualityIdentifier(strings.ToLower(anyToString(sample["retrieval_intent"])), 80),
		"first_pass_success":       anyToBool(firstPassRaw),
		"repair_required":          anyToBool(repairRaw),
		"retry_count":              retryCount,
		"observed_followup_tokens": followupTokens,
		"outcome_source":           outcomeSource,
		"outcome_class":            outcomeClass,
		"context_attribution":      attribution,
		"calibration_eligible":     calibrationEligible,
	}
	if attempts := contextPackCandidateAttributionAttempts(sample); len(attempts) > 0 {
		entry["candidate_attribution_attempts"] = attempts
	}
	copyContextPackQualityProofIdentity(entry, sample)
	if policyID := contextPackQualityIdentifier(sample["policy_id"], 160); policyID != "" {
		entry["policy_id"] = policyID
	}
	if policyArm := contextPackQualityIdentifier(strings.ToLower(anyToString(sample["policy_arm"])), 80); policyArm != "" {
		switch policyArm {
		case "control", "candidate", "shadow", "canary":
			entry["policy_arm"] = policyArm
		}
	}
	if policyPhase := contextPackQualityIdentifier(strings.ToLower(anyToString(sample["policy_phase"])), 80); policyPhase != "" {
		if _, ok := contextPolicyPhases[policyPhase]; ok {
			entry["policy_phase"] = policyPhase
		}
	}
	if providerPromptTokens > 0 {
		entry["provider_prompt_tokens"] = providerPromptTokens
	}
	if providerCompletionTokens > 0 {
		entry["provider_completion_tokens"] = providerCompletionTokens
	}
	if providerTotalTokens > 0 {
		entry["provider_total_tokens"] = providerTotalTokens
	}
	if utility := contextPackQualityUtilityClaim(sample, anyToString(entry["project"])); len(utility) > 0 {
		entry["utility"] = utility
		// These exact, validated aliases are retained only because the existing
		// attribution binder consumes them as the outcome-wide verifier proof.
		if digest := anyToString(utility["evidence_digest"]); digest != "" {
			entry["verification_evidence_digest"] = digest
		}
		if verifierID := anyToString(utility["verifier_id"]); verifierID != "" {
			entry["verifier_id"] = verifierID
		}
		if value, present := utility["verification_passed"]; present {
			entry["verification_passed"] = value
		}
	}
	if economics := normalizeUtilityEconomics(sample); len(economics) > 0 {
		entry["economics"] = economics
	}
	if pairing := contextPackQualityPairing(sample, anyToString(entry["project"])); len(pairing) > 0 {
		entry["pairing"] = pairing
	}
	if attribution := contextPackQualityEvidenceAttributions(sample, anyToString(entry["project"])); len(attribution) > 0 {
		entry["evidence_attribution"] = attribution
	}
	for key, value := range contextPackQualityDerivedRegressionLedgerFields(sample) {
		entry[key] = value
	}
	utilityPresent := len(anyMap(entry["utility"])) > 0 || len(anyMap(entry["verified_utility"])) > 0
	if !utilityPresent {
		_, utilityPresent = firstPresentValue(entry["utility_value"], entry["verified_utility_value"])
	}
	if retryCount == 0 && followupTokens == 0 && providerTotalTokens == 0 && !firstPassPresent && !repairPresent && !utilityPresent {
		return nil, nil
	}
	return entry, nil
}

// contextPackQualityUtilityClaim is the durable/public utility ingress
// boundary. Utility is evaluated later by the independent ledger, so this
// preserves only canonical machine fields rather than reporter-provided maps.
func contextPackQualityUtilityClaim(sample map[string]any, project string) map[string]any {
	source := anyMap(firstNonEmptyAny(sample["utility"], sample["verified_utility"]))
	out := map[string]any{}
	if value, present := utilityNumberPresent(source, sample, "value", "utility_value", "verified_utility_value"); present &&
		!math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 1_000_000 {
		out["value"] = utilityRound(value, 6)
	}
	if unit := contextPackQualityOpaqueReporterRef(project, "utility_unit", firstPresentAny(source["unit"], sample["utility_unit"]), 80); unit != "" {
		out["unit"] = unit
	}
	if eventID := contextPackQualityOpaqueReporterRef(project, "verification_event_id", firstPresentAny(source["verification_event_id"], sample["verification_event_id"]), 128); eventID != "" {
		out["verification_event_id"] = eventID
	}
	if digest := strings.TrimSpace(anyToString(firstPresentAny(source["evidence_digest"], sample["verification_evidence_digest"], sample["evidence_digest"]))); utilitySHA256DigestValid(digest) {
		out["evidence_digest"] = strings.ToLower(digest)
	}
	if kind := strings.ToLower(contextPackQualityIdentifier(firstPresentAny(source["verifier_kind"], sample["verifier_kind"]), 80)); kind != "" {
		if _, allowed := utilityVerifierKinds[kind]; allowed {
			out["verifier_kind"] = kind
		}
	}
	if verifierID := contextPackQualityOpaqueReporterRef(project, "verifier_id", firstPresentAny(source["verifier_id"], sample["verifier_id"]), 160); verifierID != "" {
		out["verifier_id"] = verifierID
	}
	if raw, present := utilityFirstPresent(source, sample, "verification_passed"); present {
		out["verification_passed"] = anyToBool(raw)
	}
	return out
}

func contextPackQualityPairing(sample map[string]any, project string) map[string]any {
	source := anyMap(firstNonEmptyAny(sample["pairing"], sample["matched_control"]))
	out := map[string]any{}
	for _, field := range []struct {
		key    string
		limit  int
		values []any
	}{
		{key: "pair_id", limit: 160, values: []any{source["pair_id"], sample["pair_id"]}},
		{key: "matched_control_outcome_id", limit: 200, values: []any{source["matched_control_outcome_id"], sample["matched_control_outcome_id"]}},
		{key: "experiment_id", limit: 160, values: []any{source["experiment_id"], sample["experiment_id"]}},
		{key: "model", limit: 160, values: []any{source["model"], sample["pair_model"]}},
		{key: "runner", limit: 160, values: []any{source["runner"], sample["pair_runner"]}},
		{key: "harness", limit: 160, values: []any{source["harness"], sample["pair_harness"]}},
		{key: "context_reconstruction_contract", limit: 160, values: []any{source["context_reconstruction_contract"], sample["context_reconstruction_contract"]}},
	} {
		if value := contextPackQualityOpaqueReporterRef(project, field.key, firstPresentAny(field.values...), field.limit); value != "" {
			out[field.key] = value
		}
	}
	if arm := strings.ToLower(contextPackQualityIdentifier(firstPresentAny(source["arm"], sample["pair_arm"], sample["arm"]), 40)); arm != "" {
		if arm == "candidate" || arm == "canary" || arm == "treatment" {
			arm = "treatment"
		}
		if arm == "control" || arm == "treatment" {
			out["arm"] = arm
		}
	}
	if method := strings.ToLower(contextPackQualityIdentifier(firstPresentAny(source["matching_method"], sample["matching_method"]), 80)); method != "" {
		if _, allowed := utilityExactMatchingMethods[method]; allowed {
			out["matching_method"] = method
		}
	}
	for _, field := range []struct {
		key    string
		values []any
	}{
		{key: "task_match_digest", values: []any{source["task_match_digest"], sample["task_match_digest"]}},
		{key: "assignment_digest", values: []any{source["assignment_digest"], sample["assignment_digest"]}},
	} {
		if digest := strings.TrimSpace(anyToString(firstPresentAny(field.values...))); utilitySHA256DigestValid(digest) {
			out[field.key] = strings.ToLower(digest)
		}
	}
	if raw, present := utilityFirstPresent(source, sample, "leakage_free"); present {
		out["leakage_free"] = anyToBool(raw)
	}
	return out
}

// contextPackQualityEvidenceAttributions keeps attribution grouping useful
// while replacing non-candidate entity labels with full opaque refs. This
// prevents file, source, or memory paths from crossing the outcome boundary.
func contextPackQualityEvidenceAttributions(sample map[string]any, project string) []any {
	normalized := normalizeEvidenceAttributions(sample)
	out := make([]any, 0, len(normalized))
	for _, raw := range normalized {
		row := anyMap(raw)
		entityType := strings.ToLower(anyToString(row["entity_type"]))
		if !containsString([]string{"candidate", "source", "file", "agent", "memory"}, entityType) {
			continue
		}
		method := strings.ToLower(anyToString(row["attribution_method"]))
		if !containsString([]string{"explicit_verified", "counterfactual", "leave_one_out", "citation_loss"}, method) {
			continue
		}
		role := "support"
		if strings.EqualFold(anyToString(row["role"]), "opposition") {
			role = "opposition"
		}
		rawEntityID := anyToString(row["entity_id"])
		entityID := rawEntityID
		if entityType == "candidate" {
			entityID = contextPackOpaqueCandidateRef(firstPresentAny(row["candidate_ref"], entityID))
			if entityID == "" {
				continue
			}
		} else {
			if entityID == "" {
				continue
			}
			entityID = "sha256:" + sha256Hex("context-pack-quality-attribution.v1\x00"+entityType+"\x00"+entityID)
		}
		clean := map[string]any{
			"entity_type":        entityType,
			"entity_id":          entityID,
			"entity_ref":         "sha256:" + sha256Hex("context-pack-quality-attribution-ref.v1\x00"+entityType+"\x00"+entityID),
			"attribution_method": method,
			"role":               role,
		}
		if entityType == "candidate" {
			clean["candidate_ref"] = entityID
		}
		if entityType == "agent" {
			if subjectRef := contextPackQualityOpaqueReporterRef(project, "evidence_identity", rawEntityID, 160); subjectRef != "" {
				clean["entity_subject_ref"] = subjectRef
			}
		}
		for _, field := range []string{"issuer", "producer_agent_id", "verifier_id"} {
			if value := contextPackQualityOpaqueReporterRef(project, field, row[field], 160); value != "" {
				clean[field] = value
				if subjectRef := contextPackQualityOpaqueReporterRef(project, "evidence_identity", row[field], 160); subjectRef != "" {
					clean[field+"_subject_ref"] = subjectRef
				}
			}
		}
		if digest := strings.TrimSpace(anyToString(row["verification_evidence_digest"])); utilitySHA256DigestValid(digest) {
			clean["verification_evidence_digest"] = strings.ToLower(digest)
		}
		out = append(out, clean)
	}
	return out
}

// Context-pack outcome telemetry is not the regression-fixture store. A
// fixture may contain a query, topic, expected paths, and source excerpts, so
// this boundary retains only its opaque identity and the scalar admission
// evidence. The offline evaluator therefore rejects it as non-reconstructable
// instead of rehydrating private fixture material from telemetry.
func contextPackQualityDerivedRegressionLedgerFields(sample map[string]any) map[string]any {
	fields := derivedRegressionLedgerFields(sample)
	out := map[string]any{}
	if fixture := anyMap(fields["regression_case"]); len(fixture) > 0 {
		if raw, err := json.Marshal(fixture); err == nil {
			out["regression_case_ref"] = "sha256:" + sha256Hex(string(raw))
		}
	}
	if partition := strings.ToLower(contextPackQualityIdentifier(fields["regression_partition"], 20)); partition == "train" || partition == "holdout" {
		out["regression_partition"] = partition
	}
	if trafficClass := strings.ToLower(contextPackQualityIdentifier(fields["traffic_class"], 40)); trafficClass == "user" || trafficClass == "synthetic" {
		out["traffic_class"] = trafficClass
	}
	if raw, present := fields["synthetic"]; present {
		out["synthetic"] = anyToBool(raw)
	}
	if stability := anyMap(fields["stability"]); len(stability) > 0 {
		clean := map[string]any{
			"stable":         anyToBool(stability["stable"]),
			"run_count":      clampInt(anyToInt(stability["run_count"], 0), 0, 20),
			"external_state": anyToBool(stability["external_state"]),
		}
		digests := make([]any, 0, 20)
		for _, raw := range anyToStringList(stability["result_digests"], 20) {
			if utilitySHA256DigestValid(raw) {
				digests = append(digests, strings.ToLower(raw))
			}
		}
		if len(digests) > 0 {
			clean["result_digests"] = digests
		}
		out["stability"] = clean
	}
	return out
}

func (t *contextPackQualityTelemetry) outcomeSourceRows(limit int) []map[string]any {
	if t == nil {
		return nil
	}
	limit = clampInt(limit, 1, evidenceReputationMaxRows)
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := t.proofOutcomes.ordered()
	if len(rows) == 0 {
		rows = t.outcomes
	}
	start := maxInt(0, len(rows)-limit)
	out := make([]map[string]any, 0, len(rows)-start)
	for _, row := range rows[start:] {
		out = append(out, cloneJSONMap(row))
	}
	return out
}

func (t *contextPackQualityTelemetry) derivedRegressionSourceRows(limit int) []map[string]any {
	rows := t.outcomeSourceRows(clampInt(limit, 1, derivedRegressionMaxRows))
	if t == nil || t.regressionFixtures == nil {
		return rows
	}
	for _, row := range rows {
		ref := strings.TrimSpace(anyToString(row["regression_case_ref"]))
		fixture, found := t.regressionFixtures.fixture(ref)
		if !found {
			continue
		}
		row["regression_case"] = fixture
	}
	return rows
}

func (t *contextPackQualityTelemetry) sampleForUtility(sampleID string) (map[string]any, bool) {
	if t == nil || strings.TrimSpace(sampleID) == "" {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	rows := t.proofSamples.ordered()
	if len(rows) == 0 {
		rows = t.samples
	}
	for index := len(rows) - 1; index >= 0; index-- {
		if anyToString(rows[index]["sample_id"]) == sampleID {
			return cloneAnyMap(rows[index]), true
		}
	}
	return nil, false
}

// durableReceiptSampleForUtility resolves the exact retained ledger receipt.
// The in-memory index only supplies the expected digest: bounded compaction
// can remove an older receipt after it was first acknowledged, so cached rows
// must never bless a candidate on their own.
func (t *contextPackQualityTelemetry) durableReceiptSampleForUtility(sampleID string) (map[string]any, bool) {
	if t == nil || strings.TrimSpace(sampleID) == "" {
		return nil, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	wantDigest := strings.TrimSpace(t.durableReceiptSamples[sampleID])
	return t.durableReceiptSampleFromLedgerLocked(sampleID, wantDigest)
}

// durableQualitySampleForOutcome resolves the complete, server-recorded
// quality row used to bind public outcome claims. Unlike selection receipts,
// every admissible outcome needs this row, and conflicting duplicate sample
// identities fail closed rather than accepting the newest row by accident.
func (t *contextPackQualityTelemetry) durableQualitySampleForOutcome(sampleID string) (map[string]any, bool, error) {
	if t == nil || strings.TrimSpace(sampleID) == "" || !contextPackQualityLedgerAvailable(t.ledger) {
		return nil, false, nil
	}
	t.ledger.mu.Lock()
	rows, _, err := t.ledger.readRowsUnlocked()
	t.ledger.mu.Unlock()
	if err != nil {
		t.ledger.setError(err)
		return nil, false, err
	}
	var found map[string]any
	for _, row := range rows {
		if anyToString(row["schema_id"]) != contextPackQualitySchemaID || anyToString(row["sample_id"]) != sampleID {
			continue
		}
		canonical := contextPackQualityEntryFromSample(row)
		if len(canonical) == 0 {
			continue
		}
		if found != nil && contextPackQualitySampleAdmissionRef(found) != contextPackQualitySampleAdmissionRef(canonical) {
			return nil, false, errContextPackOutcomeSampleConflict
		}
		found = canonical
	}
	if found == nil {
		return nil, false, nil
	}
	return cloneJSONMap(found), true, nil
}

func contextPackQualitySampleAdmissionRef(sample map[string]any) string {
	canonical := contextPackQualityEntryFromSample(sample)
	if len(canonical) == 0 || strings.TrimSpace(anyToString(canonical["sample_id"])) == "" {
		return ""
	}
	project, ok := canonical["project"].(string)
	if !ok || project == "" || contextPackQualityIdentifier(project, 160) != project {
		// An unscoped quality sample can remain observable, but it cannot be
		// the authoritative receipt for an outcome that affects calibration,
		// provider, Utility, or candidate evidence.
		return ""
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	return "sha256:" + sha256Hex(string(encoded))
}

func contextPackOutcomeHasAuthoritativeSampleAdmission(entry map[string]any) bool {
	return anyToString(entry["quality_sample_admission"]) == contextPackOutcomeAdmissionSchemaID &&
		utilitySHA256DigestValid(anyToString(entry["quality_sample_admission_ref"]))
}

// bindContextPackQualityOutcomeSample replaces reporter identity with the
// canonical server-recorded quality sample. A nonempty reporter identity must
// agree exactly (case-insensitively only for project scope); otherwise a known
// sample could be reused to poison another task or session's calibration.
func bindContextPackQualityOutcomeSample(entry, sample map[string]any) (map[string]any, error) {
	if len(entry) == 0 || len(sample) == 0 || anyToString(entry["sample_id"]) == "" || anyToString(entry["sample_id"]) != anyToString(sample["sample_id"]) {
		return nil, errContextPackOutcomeSampleConflict
	}
	bound := cloneJSONMap(entry)
	for _, field := range []string{
		"project", "task_class", "retrieval_intent", "task_id", "session_id",
		"task_identity_id", "execution_lane_id", "agent_id",
	} {
		reported := strings.TrimSpace(anyToString(bound[field]))
		canonical := strings.TrimSpace(anyToString(sample[field]))
		if reported != "" {
			matches := reported == canonical
			if field == "project" {
				matches = strings.EqualFold(reported, canonical)
			}
			if canonical == "" || !matches {
				return nil, errContextPackOutcomeSampleConflict
			}
		}
		if canonical == "" {
			delete(bound, field)
		} else {
			bound[field] = canonical
		}
	}
	ref := contextPackQualitySampleAdmissionRef(sample)
	if ref == "" {
		return nil, errContextPackOutcomeSampleConflict
	}
	bound["quality_sample_admission"] = contextPackOutcomeAdmissionSchemaID
	bound["quality_sample_admission_ref"] = ref
	return bound, nil
}

// authoritativeOutcomeForSample reads complete durable rows so bounded in-memory
// retention cannot permit a second public outcome for the same quality sample.
func (t *contextPackQualityTelemetry) authoritativeOutcomeForSample(sampleID string) (map[string]any, bool, error) {
	if t == nil || strings.TrimSpace(sampleID) == "" || !contextPackQualityLedgerAvailable(t.ledger) {
		return nil, false, nil
	}
	t.ledger.mu.Lock()
	rows, _, err := t.ledger.readRowsUnlocked()
	t.ledger.mu.Unlock()
	if err != nil {
		t.ledger.setError(err)
		return nil, false, err
	}
	var found map[string]any
	for _, row := range rows {
		if anyToString(row["schema_id"]) != contextPackQualityOutcomeSchemaID || anyToString(row["sample_id"]) != sampleID || !contextPackOutcomeHasAuthoritativeSampleAdmission(row) {
			continue
		}
		if found != nil && anyToString(found["outcome_id"]) != anyToString(row["outcome_id"]) {
			return nil, false, errContextPackOutcomeSampleConflict
		}
		if found != nil && contextPackOutcomeLogicalClaimDigest(found) != contextPackOutcomeLogicalClaimDigest(row) {
			return nil, false, errContextPackOutcomeSampleConflict
		}
		found = cloneJSONMap(row)
	}
	if found == nil {
		return nil, false, nil
	}
	return found, true, nil
}

// authoritativeOutcomeSampleDurableLocked proves that the exact sample used at
// public ingress still survives ledger retention. A syntactically valid digest
// alone is not evidence: applying an orphaned outcome would let a pruned sample
// continue to influence calibration and provider aggregates.
func (t *contextPackQualityTelemetry) authoritativeOutcomeSampleDurableLocked(entry map[string]any) bool {
	if !contextPackOutcomeHasAuthoritativeSampleAdmission(entry) {
		return true
	}
	sample, found, err := t.durableQualitySampleForOutcome(anyToString(entry["sample_id"]))
	return err == nil && found && contextPackQualitySampleAdmissionRef(sample) == anyToString(entry["quality_sample_admission_ref"])
}

// durableReceiptSampleFromLedgerLocked finds a retained receipt outside the
// bounded in-memory timeline. It only returns the exact canonical receipt row
// from the quality ledger, so durable replay never promotes reporter input.
func (t *contextPackQualityTelemetry) durableReceiptSampleFromLedgerLocked(sampleID, wantDigest string) (map[string]any, bool) {
	if t == nil || strings.TrimSpace(sampleID) == "" || !contextPackQualityLedgerAvailable(t.ledger) {
		return nil, false
	}
	t.ledger.mu.Lock()
	rows, _, err := t.ledger.readRowsUnlocked()
	t.ledger.mu.Unlock()
	if err != nil {
		t.ledger.setError(err)
		return nil, false
	}
	for index := len(rows) - 1; index >= 0; index-- {
		row := rows[index]
		if anyToString(row["schema_id"]) != contextPackQualitySchemaID || anyToString(row["sample_id"]) != sampleID {
			continue
		}
		receipt := contextPackSelectionReceiptFromSample(row["selection_receipt"])
		digest := strings.TrimSpace(anyToString(receipt["receipt_digest"]))
		if digest == "" || (wantDigest != "" && digest != wantDigest) {
			continue
		}
		t.markDurableReceiptSampleLocked(row)
		return cloneAnyMap(row), true
	}
	return nil, false
}

func (t *contextPackQualityTelemetry) markDurableReceiptSampleLocked(entry map[string]any) {
	if t == nil {
		return
	}
	sampleID := strings.TrimSpace(anyToString(entry["sample_id"]))
	receipt := contextPackSelectionReceiptFromSample(entry["selection_receipt"])
	digest := strings.TrimSpace(anyToString(receipt["receipt_digest"]))
	if sampleID == "" || digest == "" {
		return
	}
	if t.durableReceiptSamples == nil {
		t.durableReceiptSamples = make(map[string]string)
	}
	t.durableReceiptSamples[sampleID] = digest
}

func (t *contextPackQualityTelemetry) candidateOutcomeReceiptDurableLocked(entry map[string]any) bool {
	if t == nil || !contextPackOutcomeHasReceiptBoundCandidate(entry) {
		return true
	}
	if !contextPackQualityLedgerAvailable(t.ledger) {
		return false
	}
	sampleID := strings.TrimSpace(anyToString(entry["sample_id"]))
	expectedDigest := strings.TrimSpace(anyToString(anyMap(entry["attribution_binding"])["selection_receipt_digest"]))
	if sampleID == "" || expectedDigest == "" {
		return false
	}
	_, durable := t.durableReceiptSampleFromLedgerLocked(sampleID, expectedDigest)
	return durable
}

func (t *contextPackQualityTelemetry) receiptDurableOutcomeRows(limit int) ([]map[string]any, map[string]any) {
	rows := t.outcomeSourceRows(limit)
	accepted := make([]map[string]any, 0, len(rows))
	missing := 0
	t.mu.Lock()
	for _, row := range rows {
		if !t.candidateOutcomeReceiptDurableLocked(row) {
			missing++
			continue
		}
		accepted = append(accepted, cloneJSONMap(row))
	}
	t.mu.Unlock()
	return accepted, map[string]any{
		"pass":                          missing == 0,
		"receipt_bound_outcome_count":   len(rows) - missing,
		"missing_receipt_outcome_count": missing,
	}
}

func contextPackOutcomeFirstPresent(sample map[string]any, keys ...string) (any, bool) {
	for _, key := range keys {
		value, ok := sample[key]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		return value, true
	}
	return nil, false
}

func (t *contextPackQualityTelemetry) applyQualityEntryLocked(entry map[string]any) {
	qualityScore := clampInt(anyToInt(entry["quality_score"], 0), 0, 100)
	t.sampleCount++
	t.totalQualityScore += int64(qualityScore)
	t.totalExactPromptSaved += int64(anyToInt(entry["exact_prompt_tokens_saved"], 0))
	t.totalModeledInferenceAvoided += int64(anyToInt(entry["modeled_inference_tokens_avoided"], 0))
	t.totalModeledExtraCallsMilli += int64(math.Round(anyToFloat(entry["modeled_extra_calls_avoided"]) * 1000))
	if anyToBool(entry["tokenizer_exact"]) {
		t.exactTokenSamples++
	}
	t.lastSampleAt = firstNonEmptyStrings(anyToString(entry["capturedAt"]), nowUTCISO())
	entry["capturedAt"] = t.lastSampleAt
	stored := cloneMap(entry)
	t.samples = append(t.samples, stored)
	t.proofSamples.add(stored)
	if len(t.samples) > t.limit {
		t.samples = append([]map[string]any{}, t.samples[len(t.samples)-t.limit:]...)
	}
}

func (t *contextPackQualityTelemetry) applyOutcomeEntryLocked(entry map[string]any) bool {
	if t.outcomeKeys == nil {
		t.outcomeKeys = make(map[string]struct{})
	}
	outcomeKey := strings.TrimSpace(anyToString(entry["outcome_id"]))
	if outcomeKey == "" {
		outcomeKey = "cpo_" + sha256Hex(anyToString(entry["sample_id"]) + "\x00" + anyToString(entry["capturedAt"]))[:24]
		entry["outcome_id"] = outcomeKey
	}
	if _, exists := t.outcomeKeys[outcomeKey]; exists {
		return false
	}
	t.outcomeKeys[outcomeKey] = struct{}{}
	contextPackOutcomeSaturatingAdd(&t.outcomeCount, 1)
	authoritativeAdmission := contextPackOutcomeHasAuthoritativeSampleAdmission(entry)
	calibrationEligible := true
	if raw, present := entry["calibration_eligible"]; present {
		calibrationEligible = anyToBool(raw)
	}
	calibrationEligible = calibrationEligible && authoritativeAdmission
	entry["calibration_eligible"] = calibrationEligible
	if calibrationEligible {
		contextPackOutcomeSaturatingAdd(&t.calibrationOutcomeCount, 1)
		if anyToBool(entry["first_pass_success"]) {
			contextPackOutcomeSaturatingAdd(&t.firstPassSuccessCount, 1)
		}
		if anyToBool(entry["repair_required"]) {
			contextPackOutcomeSaturatingAdd(&t.repairRequiredCount, 1)
		}
		if retryCount, valid := contextPackOutcomeBoundedCount(entry["retry_count"], contextPackOutcomeMaxRetryCount); valid {
			contextPackOutcomeSaturatingAdd(&t.totalRetryCount, retryCount)
		}
		if followupTokens, valid := contextPackOutcomeBoundedCount(entry["observed_followup_tokens"], contextPackOutcomeMaxFollowupTokens); valid {
			contextPackOutcomeSaturatingAdd(&t.totalObservedFollowupTokens, followupTokens)
		}
	}
	providerPromptTokens, promptValid := contextPackOutcomeBoundedCount(entry["provider_prompt_tokens"], contextPackOutcomeMaxProviderComponentTokens)
	providerCompletionTokens, completionValid := contextPackOutcomeBoundedCount(entry["provider_completion_tokens"], contextPackOutcomeMaxProviderComponentTokens)
	providerTotalTokens, totalValid := contextPackOutcomeBoundedCount(entry["provider_total_tokens"], contextPackOutcomeMaxProviderTotalTokens)
	if !promptValid {
		providerPromptTokens = 0
	}
	if !completionValid {
		providerCompletionTokens = 0
	}
	if !totalValid {
		providerTotalTokens = 0
	}
	if authoritativeAdmission && (providerPromptTokens > 0 || providerCompletionTokens > 0 || providerTotalTokens > 0) {
		contextPackOutcomeSaturatingAdd(&t.providerUsageCount, 1)
		contextPackOutcomeSaturatingAdd(&t.totalProviderPromptTokens, providerPromptTokens)
		contextPackOutcomeSaturatingAdd(&t.totalProviderCompletionTokens, providerCompletionTokens)
		contextPackOutcomeSaturatingAdd(&t.totalProviderTotalTokens, providerTotalTokens)
	}
	t.lastOutcomeAt = firstNonEmptyStrings(anyToString(entry["capturedAt"]), nowUTCISO())
	entry["capturedAt"] = t.lastOutcomeAt
	stored := cloneMap(entry)
	t.outcomes = append(t.outcomes, stored)
	t.proofOutcomes.add(stored)
	if len(t.outcomes) > t.limit {
		t.outcomes = append([]map[string]any{}, t.outcomes[len(t.outcomes)-t.limit:]...)
	}
	return true
}

func (t *contextPackQualityTelemetry) snapshot() map[string]any {
	if t == nil {
		return defaultContextPackQualityTelemetrySnapshot(nil, nil)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sampleCount == 0 && t.outcomeCount == 0 {
		return defaultContextPackQualityTelemetrySnapshot(t.ledger, t.regressionFixtures)
	}
	samples := make([]any, 0, minInt(len(t.samples), 20))
	start := maxInt(0, len(t.samples)-20)
	for _, sample := range t.samples[start:] {
		samples = append(samples, cloneMap(sample))
	}
	outcomes := make([]any, 0, minInt(len(t.outcomes), 20))
	outcomeStart := maxInt(0, len(t.outcomes)-20)
	for _, outcome := range t.outcomes[outcomeStart:] {
		outcomes = append(outcomes, cloneMap(outcome))
	}

	avgQuality := int64(0)
	if t.sampleCount > 0 {
		avgQuality = t.totalQualityScore / t.sampleCount
	}
	modeledCalls := 0.0
	if t.sampleCount > 0 {
		modeledCalls = roundFloat(float64(t.totalModeledExtraCallsMilli)/1000, 3)
	}
	confidence := "low"
	calibration := "modeled_counterfactual"
	if t.calibrationOutcomeCount > 0 {
		calibration = "outcome_seeded"
	}
	if t.calibrationOutcomeCount >= 20 {
		confidence = "high"
		calibration = "outcome_adjusted"
	} else if t.calibrationOutcomeCount >= 5 || t.sampleCount >= 10 {
		confidence = "medium"
	}
	var firstPassRate any
	var repairRate any
	var avgRetries any
	var avgProviderTotal any
	if t.calibrationOutcomeCount > 0 {
		firstPassRate = roundFloat(float64(t.firstPassSuccessCount)/float64(t.calibrationOutcomeCount), 3)
		repairRate = roundFloat(float64(t.repairRequiredCount)/float64(t.calibrationOutcomeCount), 3)
		avgRetries = roundFloat(float64(t.totalRetryCount)/float64(t.calibrationOutcomeCount), 3)
	}
	if t.providerUsageCount > 0 {
		avgProviderTotal = roundFloat(float64(t.totalProviderTotalTokens)/float64(t.providerUsageCount), 3)
	}

	return map[string]any{
		"schema_id":                              contextPackQualityTelemetrySchemaID,
		"version":                                1,
		"updatedAt":                              nowUTCISO(),
		"sample_count":                           t.sampleCount,
		"outcome_sample_count":                   t.outcomeCount,
		"calibration_outcome_sample_count":       t.calibrationOutcomeCount,
		"exact_token_sample_count":               t.exactTokenSamples,
		"confidence":                             confidence,
		"calibration_grade":                      calibration,
		"average_quality_score":                  avgQuality,
		"exact_prompt_tokens_saved":              t.totalExactPromptSaved,
		"modeled_inference_tokens_avoided":       t.totalModeledInferenceAvoided,
		"modeled_extra_calls_avoided":            modeledCalls,
		"observed_first_pass_success_rate":       firstPassRate,
		"observed_repair_rate":                   repairRate,
		"observed_average_retry_count":           avgRetries,
		"observed_followup_tokens":               t.totalObservedFollowupTokens,
		"observed_provider_usage_count":          t.providerUsageCount,
		"observed_provider_prompt_tokens":        t.totalProviderPromptTokens,
		"observed_provider_completion_tokens":    t.totalProviderCompletionTokens,
		"observed_provider_total_tokens":         t.totalProviderTotalTokens,
		"observed_average_provider_total_tokens": avgProviderTotal,
		"last_sample_at":                         t.lastSampleAt,
		"last_outcome_at":                        t.lastOutcomeAt,
		"measurement_limit":                      contextPackQualityMeasurementLimit(t.calibrationOutcomeCount > 0),
		"source":                                 "/telemetry/context-pack-quality",
		"basis":                                  contextPackQualityBasis(),
		"storage":                                contextPackQualityStoragePublicStatus(t.ledger, t.regressionFixtures),
		"samples":                                samples,
		"outcomes":                               outcomes,
	}
}

func contextPackQualityBasis() []any {
	return []any{
		"exact context_pack.token_impact prompt-token delta",
		"ranked evidence count and high-impact evidence mix",
		"source coverage completeness and warning pressure",
		"bounded counterfactual retry probability model",
		"optional posted outcome rows and provider usage counters for calibration",
	}
}

func contextPackQualityLedgerPublicStatus(ledger *contextPackQualityLedger) map[string]any {
	status := map[string]any{"enabled": false, "durability": "disabled"}
	if ledger == nil {
		return status
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	status["enabled"] = ledger.enabled
	if ledger.enabled {
		status["durability"] = "bounded_ndjson"
	}
	status["max_bytes"] = ledger.maxBytes
	status["max_samples"] = ledger.maxSamples
	status["loaded_rows"] = ledger.loadedRows
	status["parse_errors"] = ledger.parseErrors
	status["write_errors"] = ledger.writeErrors
	status["prune_errors"] = ledger.pruneErrors
	status["last_write_at"] = ledger.lastWriteAt
	status["last_error"] = ledger.lastError
	status["last_prune_error"] = ledger.lastPruneError
	return status
}

func contextPackQualityStoragePublicStatus(ledger *contextPackQualityLedger, regressionFixtures *contextPackRegressionFixtureStore) map[string]any {
	status := contextPackQualityLedgerPublicStatus(ledger)
	status["regression_fixture_sidecar"] = contextPackRegressionFixtureOperationalStatus(regressionFixtures)
	return status
}

// contextPackRegressionFixtureOperationalStatus intentionally exposes only
// operational metadata. Its path and fixture content remain owner-only.
func contextPackRegressionFixtureOperationalStatus(store *contextPackRegressionFixtureStore) map[string]any {
	status := map[string]any{
		"configured":    false,
		"enabled":       false,
		"healthy":       false,
		"fixture_count": 0,
		"bytes":         int64(0),
		"max_bytes":     int64(0),
		"max_fixtures":  0,
		"error_code":    "",
	}
	if store == nil {
		return status
	}
	store.mu.Lock()
	configured := strings.TrimSpace(store.path) != ""
	enabled := store.enabled
	path := store.path
	lastError := store.lastError
	status["configured"] = configured
	status["enabled"] = enabled
	status["fixture_count"] = len(store.fixtures)
	status["max_bytes"] = store.maxBytes
	status["max_fixtures"] = store.maxFixtures
	store.mu.Unlock()

	if configured && enabled {
		if info, err := os.Stat(path); err == nil {
			status["bytes"] = info.Size()
		} else if lastError == "" {
			lastError = tokenImpactLedgerErrorCode(err)
		}
	}
	status["error_code"] = lastError
	status["healthy"] = enabled && lastError == ""
	return status
}

func (l *contextPackQualityLedger) readRows() ([]map[string]any, int, error) {
	if l == nil || !l.enabled || l.path == "" {
		return nil, 0, nil
	}
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()
	rows := make([]map[string]any, 0, l.maxSamples)
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		schemaID := anyToString(row["schema_id"])
		if schemaID != contextPackQualitySchemaID && schemaID != contextPackQualityOutcomeSchemaID {
			continue
		}
		rows = append(rows, row)
		if len(rows) > l.maxSamples {
			rows = append([]map[string]any{}, rows[len(rows)-l.maxSamples:]...)
		}
	}
	if err := scanner.Err(); err != nil {
		return rows, parseErrors, err
	}
	return rows, parseErrors, nil
}

func (l *contextPackQualityLedger) append(entry map[string]any) error {
	if l == nil {
		return errContextPackReceiptLedgerUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled || strings.TrimSpace(l.path) == "" {
		return errContextPackReceiptLedgerUnavailable
	}
	if err := l.acknowledgeDurabilityLocked(); err != nil {
		return err
	}
	file, err := openOwnerOnlyAppend(l.path, false)
	if err != nil {
		l.writeErrors++
		return err
	}
	encoded, err := json.Marshal(entry)
	if err == nil {
		_, err = file.Write(append(encoded, '\n'))
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		l.writeErrors++
		return err
	}
	l.lastWriteAt = nowUTCISO()
	l.lastError = ""
	if stat, statErr := os.Stat(l.path); statErr == nil && stat.Size() > l.maxBytes {
		if pruneErr := l.pruneLocked(); pruneErr != nil {
			l.pruneErrors++
			l.lastPruneError = tokenImpactLedgerErrorCode(pruneErr)
			// A pre-replacement retention failure leaves the fsync-acknowledged
			// append intact, so it remains telemetry-only. Once replacement has
			// happened, however, a missing acknowledgement means the candidate
			// must not enter proof or Utility state.
			if ownerOnlyAtomicWriteCommitted(pruneErr) {
				l.durabilityUnacknowledged = true
				l.lastError = tokenImpactLedgerErrorCode(pruneErr)
				return pruneErr
			}
		} else {
			l.lastPruneError = ""
		}
	}
	return nil
}

func (l *contextPackQualityLedger) pruneLocked() error {
	rows, _, err := l.readRowsUnlocked()
	if err != nil {
		return err
	}
	if len(rows) > l.maxSamples {
		rows = rows[len(rows)-l.maxSamples:]
	}
	encodedRows := make([][]byte, 0, len(rows))
	total := int64(0)
	for i := len(rows) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(rows[i])
		if err != nil {
			continue
		}
		lineBytes := int64(len(encoded) + 1)
		if len(encodedRows) > 0 && total+lineBytes > l.maxBytes {
			break
		}
		encodedRows = append(encodedRows, encoded)
		total += lineBytes
	}
	for i, j := 0, len(encodedRows)-1; i < j; i, j = i+1, j-1 {
		encodedRows[i], encodedRows[j] = encodedRows[j], encodedRows[i]
	}
	content := make([]byte, 0, total)
	for _, row := range encodedRows {
		content = append(content, row...)
		content = append(content, '\n')
	}
	dedicatedParent := strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH")) == ""
	return l.writeContentDurablyLocked(content, dedicatedParent)
}

// rewriteCurrentRowsDurablyLocked acknowledges retained bytes without applying
// a new retention policy. Startup must not erase outcomes merely because its
// bounded in-memory projection is smaller than the physical ledger history.
func (l *contextPackQualityLedger) rewriteCurrentRowsDurablyLocked() error {
	rows, _, err := l.readRowsUnlocked()
	if err != nil {
		return err
	}
	content := make([]byte, 0)
	for _, row := range rows {
		encoded, err := json.Marshal(row)
		if err != nil {
			continue
		}
		content = append(content, encoded...)
		content = append(content, '\n')
	}
	dedicatedParent := strings.TrimSpace(os.Getenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH")) == ""
	return l.writeContentDurablyLocked(content, dedicatedParent)
}

func (l *contextPackQualityLedger) writeContentDurablyLocked(content []byte, dedicatedParent bool) error {
	writeFile := l.writeFile
	if writeFile == nil {
		writeFile = writeOwnerOnlyDurableAtomicFile
	}
	return writeFile(l.path, content, dedicatedParent)
}

// acknowledgeDurability retries the durable atomic rewrite only after a
// committed-but-unacknowledged replacement. It never treats readable bytes as
// sufficient acknowledgement.
func (l *contextPackQualityLedger) acknowledgeDurability() error {
	if l == nil {
		return errContextPackReceiptLedgerUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled || strings.TrimSpace(l.path) == "" {
		return errContextPackReceiptLedgerUnavailable
	}
	return l.acknowledgeDurabilityLocked()
}

// acknowledgeStartupDurability treats every non-empty persisted ledger as an
// unacknowledged replacement until it has passed a fresh durable atomic rewrite.
// A prior process may have failed after rename but before its directory sync;
// readable rows alone are not sufficient to rebuild candidate proof state.
func (l *contextPackQualityLedger) acknowledgeStartupDurability() error {
	if l == nil {
		return errContextPackReceiptLedgerUnavailable
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.enabled || strings.TrimSpace(l.path) == "" {
		return errContextPackReceiptLedgerUnavailable
	}
	l.durabilityUnacknowledged = true
	info, err := os.Stat(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			l.durabilityUnacknowledged = false
			return nil
		}
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("context-pack quality ledger is not a regular file")
	}
	if info.Size() == 0 {
		l.durabilityUnacknowledged = false
		return nil
	}
	if err := l.rewriteCurrentRowsDurablyLocked(); err != nil {
		l.pruneErrors++
		l.lastPruneError = tokenImpactLedgerErrorCode(err)
		l.lastError = tokenImpactLedgerErrorCode(err)
		return err
	}
	l.durabilityUnacknowledged = false
	l.lastPruneError = ""
	l.lastError = ""
	l.lastWriteAt = nowUTCISO()
	return nil
}

func (l *contextPackQualityLedger) acknowledgeDurabilityLocked() error {
	if l == nil || !l.durabilityUnacknowledged {
		return nil
	}
	if err := l.pruneLocked(); err != nil {
		l.pruneErrors++
		l.lastPruneError = tokenImpactLedgerErrorCode(err)
		if ownerOnlyAtomicWriteCommitted(err) {
			l.lastError = tokenImpactLedgerErrorCode(err)
		}
		return err
	}
	l.durabilityUnacknowledged = false
	l.lastPruneError = ""
	l.lastError = ""
	l.lastWriteAt = nowUTCISO()
	return nil
}

func (l *contextPackQualityLedger) readRowsUnlocked() ([]map[string]any, int, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()
	rows := []map[string]any{}
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		schemaID := anyToString(row["schema_id"])
		if schemaID == contextPackQualitySchemaID || schemaID == contextPackQualityOutcomeSchemaID {
			rows = append(rows, row)
		}
	}
	return rows, parseErrors, scanner.Err()
}

// readRowsForPrivacyMigrationUnlocked is intentionally stricter than the
// runtime projection reader. A migration rewrites the whole file, so silently
// skipping a valid but unknown schema would turn an additive reader behavior
// into durable data loss. Unknown or malformed rows therefore stop the rewrite
// and leave the original bytes untouched.
func (l *contextPackQualityLedger) readRowsForPrivacyMigrationUnlocked() ([]map[string]any, int, error) {
	file, err := os.Open(l.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer file.Close()
	rows := []map[string]any{}
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 256*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		switch anyToString(row["schema_id"]) {
		case contextPackQualitySchemaID, contextPackQualityOutcomeSchemaID:
			rows = append(rows, row)
		default:
			return nil, parseErrors, fmt.Errorf("%w: unrecognized ledger schema", errContextPackPrivacyMigrationRejected)
		}
	}
	return rows, parseErrors, scanner.Err()
}

func (l *contextPackQualityLedger) setError(err error) {
	if l == nil || err == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastError = tokenImpactLedgerErrorCode(err)
}

// failClosedPrivacyMigration prevents a constructor that refused unsafe legacy
// bytes from later appending to the same ledger in memory. A healthy process
// must construct a fresh ledger and acknowledge the redacted file first.
func (l *contextPackQualityLedger) failClosedPrivacyMigration(err error) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.enabled = false
	l.durabilityUnacknowledged = true
	if err != nil {
		if errors.Is(err, errContextPackPrivacyMigrationRejected) {
			// This status is intentionally categorical: it identifies a schema or
			// privacy stop without exposing the rejected row or field value.
			l.lastError = "privacy_migration_failed"
		} else {
			l.lastError = tokenImpactLedgerErrorCode(err)
		}
	}
}

func buildContextPackQualitySample(input contextPackQualitySampleInput) map[string]any {
	queryHash := sha256Hex(input.Query)
	sampleSeed := strings.Join([]string{
		nowUTCISO(),
		queryHash,
		anyToString(input.TokenImpact["saved_tokens_estimate"]),
		anyToString(input.TokenImpact["packed_tokens_estimate"]),
	}, "\x00")
	ranked := contextPackAnyList(input.RankedEvidence)
	omitted := contextPackAnyList(input.OmittedHighValueRefs)
	omittedSelectionRefs := contextPackAnyList(input.OmittedSelectionRefs)
	if len(omittedSelectionRefs) == 0 {
		omittedSelectionRefs = omitted
	}
	coverage := input.SourceCoverage
	highImpactCount := 0
	for _, raw := range ranked {
		switch anyToString(anyMap(raw)["kind"]) {
		case "decision", "risk", "check", "runbook":
			highImpactCount++
		}
	}
	warningCount := len(input.Warnings)
	returnedSources := len(anyToStringList(coverage["returned"], 64))
	tokenizerExact := anyToBool(input.TokenImpact["tokenizer_exact"])
	tokenBudgetActive := anyToBool(anyMap(input.Compiled["token_budget"])["active"])
	graphUsed := anyToBool(input.GraphQuality["used"])
	coverageComplete := anyToBool(coverage["complete"])
	exactPromptSaved := anyToInt(input.TokenImpact["saved_tokens_estimate"], 0)
	packedTokens := anyToInt(input.TokenImpact["packed_tokens_estimate"], 0)
	qualityScore := contextPackQualityScore(contextPackQualitySignals{
		RankedEvidenceCount:    len(ranked),
		HighImpactCount:        highImpactCount,
		OmittedHighValueCount:  len(omitted),
		ReturnedSourceCount:    returnedSources,
		WarningCount:           warningCount,
		TokenizerExact:         tokenizerExact,
		TokenBudgetActive:      tokenBudgetActive,
		SourceCoverageComplete: coverageComplete,
		GraphUsed:              graphUsed,
		ExactPromptSaved:       exactPromptSaved,
	})
	retryModel := contextPackQualityRetryModel(qualityScore, highImpactCount, returnedSources, warningCount, packedTokens)
	confidence := "low"
	if qualityScore >= 80 && tokenizerExact && warningCount == 0 {
		confidence = "medium"
	}
	project := contextPackQualityIdentifier(input.Project, 160)
	sample := map[string]any{
		"schema_id":                          contextPackQualitySchemaID,
		"version":                            1,
		"capturedAt":                         nowUTCISO(),
		"sample_id":                          "cpq_" + sha256Hex(sampleSeed)[:24],
		"query_hash":                         queryHash[:16],
		"project":                            project,
		"task_class":                         contextPackQualityIdentifier(strings.ToLower(input.TaskClass), 80),
		"retrieval_intent":                   contextPackQualityIdentifier(strings.ToLower(input.RetrievalIntent), 80),
		"quality_score":                      qualityScore,
		"confidence":                         confidence,
		"calibration_grade":                  "modeled_counterfactual",
		"exact_prompt_tokens_saved":          exactPromptSaved,
		"modeled_inference_tokens_avoided":   retryModel.ModeledInferenceTokensAvoided,
		"modeled_extra_calls_avoided":        retryModel.ExtraCallsAvoided,
		"counterfactual_baseline":            "raw_candidate_replay",
		"ranked_evidence_count":              len(ranked),
		"high_impact_evidence_count":         highImpactCount,
		"omitted_high_value_count":           len(omitted),
		"returned_source_count":              returnedSources,
		"warning_count":                      warningCount,
		"tokenizer_exact":                    tokenizerExact,
		"tokenizer_encoding":                 contextPackQualityIdentifier(input.TokenImpact["tokenizer_encoding"], 80),
		"model_visible_context_tokens_exact": anyToInt(input.TokenImpact["model_visible_context_tokens_exact"], 0),
		"token_budget_active":                tokenBudgetActive,
		"source_coverage_complete":           coverageComplete,
		"graph_context_used":                 graphUsed,
		"model_call_token_basis":             retryModel.ModelCallTokenBasis,
		"raw_retry_probability_estimate":     retryModel.RawRetryProbability,
		"packed_retry_probability_estimate":  retryModel.PackedRetryProbability,
		"measurement_limit":                  contextPackQualityMeasurementLimit(false),
		"selection_receipt":                  contextPackSelectionReceipt(ranked, omittedSelectionRefs),
	}
	if topicRef := contextPackQualityTopicRef(project, input.TopicPath); topicRef != "" {
		sample["topic_ref"] = topicRef
	}
	copyContextPackQualityProofIdentity(sample, map[string]any{
		"session_id":        input.SessionID,
		"task_id":           input.TaskID,
		"task_identity_id":  input.TaskIdentityID,
		"execution_lane_id": input.ExecutionLaneID,
		"agent_id":          input.AgentID,
	})
	return sample
}

func (s *server) bindContextPackQualityOutcomeAttributions(entry map[string]any) map[string]any {
	bound := cloneJSONMap(entry)
	if len(bound) == 0 {
		return bound
	}
	attempts := anyMap(bound["candidate_attribution_attempts"])
	delete(bound, "candidate_attribution_attempts")
	candidateReceived := clampInt(anyToInt(attempts["received"], 0), 0, contextPackCandidateAttemptLimit)
	candidateInvalidRef := clampInt(anyToInt(attempts["invalid_ref"], 0), 0, candidateReceived)
	// contextPackQualityOutcomeFromSample already performs the sole
	// reporter-to-telemetry normalization. Keep its cloned opaque rows here;
	// routing them through the generic portable normalizer again would discard
	// the safe subject refs used to detect self-attribution.
	attributions := contextPackAnyList(bound["evidence_attribution"])
	if len(attributions) == 0 && candidateReceived == 0 {
		return bound
	}

	project := strings.TrimSpace(anyToString(bound["project"]))
	sampleID := strings.TrimSpace(anyToString(bound["sample_id"]))
	sample := map[string]any(nil)
	receiptDurable := false
	if s != nil && s.contextPackQuality != nil {
		sample, receiptDurable = s.contextPackQuality.durableReceiptSampleForUtility(sampleID)
	}
	sampleProject := strings.TrimSpace(anyToString(sample["project"]))
	receipt := contextPackSelectionReceiptFromSample(sample["selection_receipt"])
	receiptCandidates := map[string]map[string]any{}
	for _, candidate := range parseRows(receipt["candidates"]) {
		receiptCandidates[anyToString(candidate["candidate_ref"])] = candidate
	}

	exclusions := map[string]int{}
	if candidateInvalidRef > 0 {
		exclusions["candidate_ref_invalid"] = candidateInvalidRef
	}
	accepted := make([]any, 0, len(attributions))
	seenCandidates := map[string]struct{}{}
	candidateBound := 0
	parsedCandidates := 0
	legacyUnbound := 0
	for _, raw := range attributions {
		attribution := cloneAnyMap(anyMap(raw))
		entityType := anyToString(attribution["entity_type"])
		if entityType != "candidate" {
			attribution["result_level_credit"] = "unbound_legacy"
			accepted = append(accepted, attribution)
			legacyUnbound++
			continue
		}
		parsedCandidates++
		if candidateReceived < parsedCandidates {
			candidateReceived = parsedCandidates
		}
		ref := contextPackOpaqueCandidateRef(firstPresentAny(attribution["candidate_ref"], attribution["entity_id"]))
		if ref == "" {
			exclusions["candidate_ref_invalid"]++
			continue
		}
		if _, duplicate := seenCandidates[ref]; duplicate {
			exclusions["candidate_duplicate"]++
			continue
		}
		seenCandidates[ref] = struct{}{}
		if !receiptDurable {
			exclusions["candidate_receipt_not_durable"]++
			continue
		}
		if len(sample) == 0 || len(receiptCandidates) == 0 {
			exclusions["candidate_receipt_missing"]++
			continue
		}
		if project == "" || sampleProject == "" {
			exclusions["candidate_project_scope_missing"]++
			continue
		}
		if !strings.EqualFold(project, sampleProject) {
			exclusions["candidate_project_mismatch"]++
			continue
		}
		receiptCandidate, receipted := receiptCandidates[ref]
		if !receipted {
			exclusions["candidate_not_receipted"]++
			continue
		}
		attribution["entity_id"] = ref
		attribution["candidate_ref"] = ref
		attribution["selection_state"] = anyToString(receiptCandidate["selection_state"])
		attribution["selection_ordinal"] = anyToInt(receiptCandidate["ordinal"], 0)
		attribution["evidence_role"] = anyToString(receiptCandidate["evidence_role"])
		attribution["evidence_kind"] = anyToString(receiptCandidate["evidence_kind"])
		attribution["result_level_credit"] = "selection_receipt_bound"
		accepted = append(accepted, attribution)
		candidateBound++
	}
	if len(accepted) > 0 {
		bound["evidence_attribution"] = accepted
	} else {
		delete(bound, "evidence_attribution")
	}
	receiptID := anyToString(receipt["receipt_id"])
	receiptDigest := anyToString(receipt["receipt_digest"])
	bound["attribution_binding"] = map[string]any{
		"schema_id":                      contextPackOutcomeBindingSchemaID,
		"version":                        1,
		"sample_id_present":              sampleID != "",
		"candidate_attribution_received": candidateReceived,
		"candidate_attribution_bound":    candidateBound,
		"candidate_attribution_rejected": candidateReceived - candidateBound,
		"legacy_unbound_count":           legacyUnbound,
		"selection_receipt_id":           receiptID,
		"selection_receipt_digest":       receiptDigest,
		"exclusions":                     contextPackOutcomeAttributionExclusions(exclusions),
	}
	return bound
}

func contextPackOutcomeAttributionExclusions(exclusions map[string]int) map[string]any {
	known := []string{
		"candidate_ref_invalid", "candidate_duplicate", "candidate_receipt_missing",
		"candidate_receipt_not_durable",
		"candidate_project_scope_missing", "candidate_project_mismatch", "candidate_not_receipted",
	}
	bounded := map[string]any{}
	for _, reason := range known {
		if count := exclusions[reason]; count > 0 {
			bounded[reason] = count
		}
	}
	return bounded
}

type contextPackQualitySampleInput struct {
	Query                string
	Project              string
	TopicPath            string
	TaskClass            string
	RetrievalIntent      string
	SessionID            string
	TaskID               string
	TaskIdentityID       string
	ExecutionLaneID      string
	AgentID              string
	TokenImpact          map[string]any
	Compiled             map[string]any
	SourceCoverage       map[string]any
	GraphQuality         map[string]any
	RankedEvidence       any
	OmittedHighValueRefs any
	OmittedSelectionRefs any
	Warnings             []string
}

type contextPackQualitySignals struct {
	RankedEvidenceCount    int
	HighImpactCount        int
	OmittedHighValueCount  int
	ReturnedSourceCount    int
	WarningCount           int
	TokenizerExact         bool
	TokenBudgetActive      bool
	SourceCoverageComplete bool
	GraphUsed              bool
	ExactPromptSaved       int
}

func contextPackQualityScore(signals contextPackQualitySignals) int {
	score := 35
	if signals.ExactPromptSaved > 0 {
		score += 10
	}
	if signals.TokenizerExact {
		score += 8
	}
	if signals.TokenBudgetActive {
		score += 8
	}
	if signals.SourceCoverageComplete {
		score += 7
	}
	if signals.GraphUsed {
		score += 4
	}
	score += minInt(signals.RankedEvidenceCount*3, 18)
	score += minInt(signals.HighImpactCount*5, 15)
	score += minInt(signals.ReturnedSourceCount*2, 8)
	score -= minInt(signals.OmittedHighValueCount*2, 8)
	score -= minInt(signals.WarningCount*4, 16)
	return clampInt(score, 0, 100)
}

type contextPackQualityRetryEstimate struct {
	RawRetryProbability           float64
	PackedRetryProbability        float64
	ExtraCallsAvoided             float64
	ModelCallTokenBasis           int
	ModeledInferenceTokensAvoided int
}

func contextPackQualityRetryModel(qualityScore int, highImpactCount int, returnedSources int, warningCount int, packedTokens int) contextPackQualityRetryEstimate {
	rawProb := 0.34
	packedProb := rawProb -
		float64(qualityScore)*0.0022 -
		float64(minInt(highImpactCount, 5))*0.012 -
		float64(minInt(returnedSources, 5))*0.004 +
		float64(minInt(warningCount, 6))*0.025
	packedProb = clampFloat(packedProb, 0.05, rawProb)
	extraCalls := roundFloat(math.Max(0, rawProb-packedProb), 3)
	tokenBasis := maxInt(packedTokens+1200, 4000)
	return contextPackQualityRetryEstimate{
		RawRetryProbability:           roundFloat(rawProb, 3),
		PackedRetryProbability:        roundFloat(packedProb, 3),
		ExtraCallsAvoided:             extraCalls,
		ModelCallTokenBasis:           tokenBasis,
		ModeledInferenceTokensAvoided: int(math.Round(extraCalls * float64(tokenBasis))),
	}
}

func (s *server) telemetryContextPackQualityRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.contextPackQualityTelemetrySnapshot())
}

func parseContextPackQualityOutcomeJSON(body []byte) (map[string]any, error) {
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return payload, nil
}

func (s *server) telemetryContextPackQualityOutcomeRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	bodyBytes, err := readRequestBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
		return
	}
	payload, err := parseContextPackQualityOutcomeJSON(bodyBytes)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return
	}
	entry, err := contextPackQualityOutcomeFromSampleChecked(payload)
	if errors.Is(err, errContextPackOutcomeInvalidNumeric) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid outcome numeric claim"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid outcome payload"})
		return
	}
	if len(entry) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "empty outcome payload"})
		return
	}
	if s == nil || s.contextPackQuality == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "quality_sample_admission_unavailable",
		})
		return
	}
	// One serialized admission covers sample lookup, the optional fixture sidecar,
	// and quality append. It prevents concurrent unique outcome IDs from racing
	// through the same durable sample.
	s.contextPackQuality.outcomeAdmissionMu.Lock()
	defer s.contextPackQuality.outcomeAdmissionMu.Unlock()
	if !contextPackQualityLedgerAvailable(s.contextPackQuality.ledger) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "quality_sample_admission_unavailable",
		})
		return
	}
	canonicalSample, sampleFound, sampleErr := s.contextPackQuality.durableQualitySampleForOutcome(anyToString(entry["sample_id"]))
	if errors.Is(sampleErr, errContextPackOutcomeSampleConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "quality_sample_conflict"})
		return
	}
	if sampleErr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "quality_sample_admission_unavailable"})
		return
	}
	if !sampleFound {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "unknown_quality_sample"})
		return
	}
	entry, err = bindContextPackQualityOutcomeSample(entry, canonicalSample)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "quality_sample_identity_mismatch"})
		return
	}
	if existing, found, existingErr := s.contextPackQuality.authoritativeOutcomeForSample(anyToString(entry["sample_id"])); existingErr != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "quality_sample_outcome_conflict"})
		return
	} else if found && anyToString(existing["outcome_id"]) != anyToString(entry["outcome_id"]) {
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "quality_sample_outcome_conflict"})
		return
	}
	rawRegressionFixture := anyMap(payload["regression_case"])
	if len(rawRegressionFixture) > 0 {
		_, regressionFixtureRef := contextPackQualityNormalizedRegressionFixture(rawRegressionFixture)
		if regressionFixtureRef == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"ok": false, "error": "invalid_regression_fixture",
			})
			return
		}
		entry["regression_case_ref"] = regressionFixtureRef
	} else {
		delete(entry, "regression_case_ref")
	}
	// This is deliberately set after decoding and normalization: reporter time
	// remains observable as capturedAt, but can never control receipt chronology.
	entry["gateway_received_at"] = nowUTCISO()
	candidateAttempted := contextPackOutcomeHasCandidateAttributionAttempt(entry)
	prebindingDurableAdmission := contextPackOutcomeRequiresDurableAdmission(entry)
	if (candidateAttempted || prebindingDurableAdmission) && s != nil && s.contextPackQuality != nil && s.contextPackQuality.ledger != nil {
		// If a previous bounded rewrite replaced bytes but could not acknowledge
		// its directory sync, a retry must repair that exact durability boundary
		// before the readable receipt can be rebound to a candidate outcome.
		if err := s.contextPackQuality.ledger.acknowledgeDurability(); err != nil {
			s.contextPackQuality.ledger.setError(err)
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok": false, "error": "receipt_ledger_durability_unavailable",
				"detail": "candidate outcome receipt durability is unavailable",
			})
			return
		}
	}
	entry = s.bindContextPackQualityOutcomeAttributions(entry)
	candidateReceiptBound := contextPackOutcomeHasReceiptBoundCandidate(entry)
	requiresDurableAdmission := contextPackOutcomeRequiresDurableAdmission(entry)
	if candidateAttempted && !candidateReceiptBound {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "receipt_ledger_durability_unavailable",
			"detail": "candidate outcome receipt durability is unavailable",
		})
		return
	}
	recorded, qualityErr := s.recordContextPackQualityOutcomeDurably(entry)
	if errors.Is(qualityErr, errUtilityOutcomeConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "utility_outcome_conflict",
			"detail":   "outcome_id is already bound to a different logical source claim",
			"recorded": false, "utility_recorded": false,
		})
		return
	}
	if requiresDurableAdmission && qualityErr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "receipt_ledger_durability_unavailable",
			"detail": "candidate outcome receipt durability is unavailable",
		})
		return
	}
	canonical := entry
	if existing, found := s.contextPackQuality.outcomeForUtility(anyToString(entry["outcome_id"])); found {
		canonical = existing
	}
	// Persist raw fixture material only after its opaque reference has passed
	// durable quality admission. A crash or sidecar failure can leave at most a
	// harmless dangling digest; a replay repairs it. Rejections never create an
	// orphan raw fixture row.
	if len(rawRegressionFixture) > 0 {
		persistedRef, _, fixtureErr := s.contextPackQuality.recordRegressionFixtureDetailed(rawRegressionFixture)
		if fixtureErr != nil || persistedRef == "" || persistedRef != anyToString(canonical["regression_case_ref"]) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok": false, "error": "regression_fixture_persistence_unavailable",
				"detail":   "the authoritative outcome was accepted, but its private regression fixture is not durably available",
				"recorded": recorded,
			})
			return
		}
	}
	utilityObservation, utilityRecorded, utilityErr := s.recordUtilityOutcome(canonical)
	var utilityStore *utilityLedgerStore
	if s != nil && s.utility != nil {
		utilityStore = s.utility.store
	}
	if errors.Is(utilityErr, errUtilityOutcomeConflict) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "utility_outcome_conflict",
			"detail":   "outcome_id is already bound to a different utility source claim",
			"recorded": recorded, "utility_recorded": false, "utility_observation": utilityObservation,
		})
		return
	}
	if errors.Is(utilityErr, errUtilityPersistenceUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "error": "utility_persistence_unavailable",
			"detail":   "the authoritative outcome was accepted, but the derived Utility Ledger did not acknowledge durable persistence",
			"recorded": recorded, "utility_recorded": false,
			"utility_storage": utilityStorageStatus(utilityStore),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "recorded": recorded, "duplicate": !recorded, "outcome": canonical,
		"utility_recorded": utilityRecorded, "utility_observation": utilityObservation,
		"utility_storage": utilityStorageStatus(utilityStore),
		"telemetry":       s.contextPackQualityTelemetrySnapshot(),
	})
}

func contextPackOutcomeHasCandidateAttributionAttempt(entry map[string]any) bool {
	if anyToInt(anyMap(entry["candidate_attribution_attempts"])["received"], 0) > 0 {
		return true
	}
	for _, raw := range contextPackAnyList(entry["evidence_attribution"]) {
		if anyToString(anyMap(raw)["entity_type"]) == "candidate" {
			return true
		}
	}
	return false
}
