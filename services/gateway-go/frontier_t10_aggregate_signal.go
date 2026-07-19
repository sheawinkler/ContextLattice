package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	frontierT10AggregatePath                  = "/memory/aggregate-signal"
	frontierT10ContributionContractID         = "aggregate_contribution.v1"
	frontierT10ReportContractID               = "aggregate_report.v1"
	frontierT10AccountantContractID           = "privacy_accountant.v1"
	frontierT10StateSchemaID                  = "frontier_t10_aggregate_signal_state.v1"
	frontierT10ReceiptSchemaID                = "frontier_t10_aggregate_receipt.v1"
	frontierT10EnabledEnv                     = "CONTEXTLATTICE_AGGREGATE_SIGNAL_ENABLED"
	frontierT10PathEnv                        = "CONTEXTLATTICE_AGGREGATE_SIGNAL_PATH"
	frontierT10MaxBytesEnv                    = "CONTEXTLATTICE_AGGREGATE_SIGNAL_MAX_BYTES"
	frontierT10MaxRecordsEnv                  = "CONTEXTLATTICE_AGGREGATE_SIGNAL_MAX_RECORDS"
	frontierT10DefaultPath                    = ".data/orchestrator/aggregate_signal_state.json"
	frontierT10DefaultMaxBytes                = 8 * 1024 * 1024
	frontierT10DefaultMaxRecords              = 10000
	frontierT10MaximumBytes                   = 64 * 1024 * 1024
	frontierT10MaximumRecords                 = 100000
	frontierT10MinimumCohort                  = 20
	frontierT10ReleaseEpsilonMax              = 0.25
	frontierT10RollingEpsilonMax              = 2.0
	frontierT10DeltaMax                       = 0.000001
	frontierT10DefaultReleaseEpsilon          = 0.25
	frontierT10DefaultReleaseDelta            = 0.0000001
	frontierT10DefaultExpiryWeeks             = 13
	frontierT10ContributionExpiryWeeks        = 2
	frontierT10RollingWindowDays              = 90
	frontierT10MaximumContributionsPerRequest = 4096
)

var (
	frontierT10DigestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	frontierT10WindowPattern   = regexp.MustCompile(`^([0-9]{4})-W([0-9]{2})$`)
	errFrontierT10Replay       = errors.New("one contribution per privacy unit, metric, and week")
	errFrontierT10Differencing = errors.New("a different release already exists for this metric and cohort window")
	errFrontierT10Budget       = errors.New("privacy budget exhausted")
)

type frontierT10MetricSpec struct {
	Kind       string
	Minimum    float64
	Maximum    float64
	Categories []string
}

var frontierT10MetricSpecs = map[string]frontierT10MetricSpec{
	"active_mesh_grant_count":                  {Kind: "numeric", Minimum: 0, Maximum: 10000},
	"average_retry_count":                      {Kind: "numeric", Minimum: 0, Maximum: 10},
	"calibration_grade":                        {Kind: "categorical", Categories: []string{"modeled_counterfactual", "outcome_seeded", "outcome_adjusted"}},
	"context_quality_score":                    {Kind: "numeric", Minimum: 0, Maximum: 100},
	"contribution_coverage":                    {Kind: "numeric", Minimum: 0, Maximum: 1},
	"exact_prompt_tokens_saved":                {Kind: "numeric", Minimum: 0, Maximum: 1000000},
	"first_pass_success_rate":                  {Kind: "numeric", Minimum: 0, Maximum: 1},
	"mesh_revocation_count":                    {Kind: "numeric", Minimum: 0, Maximum: 10000},
	"modeled_inference_tokens_avoided":         {Kind: "numeric", Minimum: 0, Maximum: 1000000},
	"outcome_class":                            {Kind: "categorical", Categories: []string{"success", "repair_required", "blocked", "failure", "other"}},
	"policy_candidate_count":                   {Kind: "numeric", Minimum: 0, Maximum: 10000},
	"policy_evaluation_count":                  {Kind: "numeric", Minimum: 0, Maximum: 10000},
	"policy_lift":                              {Kind: "numeric", Minimum: -1, Maximum: 1},
	"policy_promotion_rate":                    {Kind: "numeric", Minimum: 0, Maximum: 1},
	"provider_total_tokens":                    {Kind: "numeric", Minimum: 0, Maximum: 1000000},
	"reconstruction_test_failure_rate":         {Kind: "numeric", Minimum: 0, Maximum: 1},
	"repair_rate":                              {Kind: "numeric", Minimum: 0, Maximum: 1},
	"retrieval_mode":                           {Kind: "categorical", Categories: []string{"fast", "balanced", "deep"}},
	"sync_projection_latency_ms":               {Kind: "numeric", Minimum: 0, Maximum: 5000},
	"verified_utility_per_model_visible_token": {Kind: "numeric", Minimum: -1000, Maximum: 1000},
	"workload_class":                           {Kind: "categorical", Categories: []string{"coding", "research", "review", "planning", "operations", "other"}},
}

var frontierT10AllowedSources = map[string]struct{}{
	"manual": {}, "context_pack_quality": {}, "context_policy": {}, "context_mesh": {},
}

type frontierT10AggregateContribution struct {
	SchemaID            string         `json:"schema_id"`
	Version             int            `json:"version"`
	ContributionID      string         `json:"contribution_id"`
	Metric              string         `json:"metric"`
	StatisticType       string         `json:"statistic_type"`
	CohortWindow        string         `json:"cohort_window"`
	UnitEpochCommitment string         `json:"unit_epoch_commitment"`
	NonceDigest         string         `json:"nonce_digest"`
	Statistic           map[string]any `json:"statistic"`
	Clipping            map[string]any `json:"clipping"`
	Source              string         `json:"source"`
	ExpiresOn           string         `json:"expires_on"`
	ContributionDigest  string         `json:"contribution_digest"`
	FormatContract      map[string]any `json:"format_contract,omitempty"`
	Operation           string         `json:"operation,omitempty"`
	Decision            string         `json:"decision,omitempty"`
	OK                  bool           `json:"ok,omitempty"`
	Persisted           bool           `json:"persisted,omitempty"`
	Privacy             map[string]any `json:"privacy,omitempty"`
	Safety              map[string]any `json:"safety,omitempty"`
}

type frontierT10AccountEntry struct {
	ReceiptID     string  `json:"receipt_id"`
	ReportID      string  `json:"report_id"`
	Metric        string  `json:"metric"`
	CohortWindow  string  `json:"cohort_window"`
	ReleasedOn    string  `json:"released_on"`
	ExpiresOn     string  `json:"expires_on"`
	Epsilon       float64 `json:"epsilon"`
	Delta         float64 `json:"delta"`
	RequestDigest string  `json:"request_digest"`
}

type frontierT10LedgerReceipt struct {
	SchemaID      string `json:"schema_id"`
	ReceiptID     string `json:"receipt_id"`
	Operation     string `json:"operation"`
	Decision      string `json:"decision"`
	Metric        string `json:"metric,omitempty"`
	CohortWindow  string `json:"cohort_window,omitempty"`
	ReleaseWindow string `json:"release_window,omitempty"`
	RecordedOn    string `json:"recorded_on"`
	ExpiresOn     string `json:"expires_on"`
	RequestDigest string `json:"request_digest"`
	Idempotent    bool   `json:"idempotent"`
}

type frontierT10AggregateReportRecord struct {
	ReportID      string                   `json:"report_id"`
	Metric        string                   `json:"metric"`
	StatisticType string                   `json:"statistic_type"`
	CohortWindow  string                   `json:"cohort_window"`
	CohortSize    int                      `json:"cohort_size"`
	Estimate      map[string]any           `json:"estimate"`
	Suppression   map[string]any           `json:"suppression"`
	Accountant    map[string]any           `json:"accountant"`
	Receipt       frontierT10LedgerReceipt `json:"receipt"`
	RequestDigest string                   `json:"request_digest"`
}

type frontierT10AggregateState struct {
	SchemaID      string                                      `json:"schema_id"`
	Version       int                                         `json:"version"`
	OptedIn       bool                                        `json:"opted_in"`
	Generation    uint64                                      `json:"generation"`
	LocalSecret   string                                      `json:"local_secret"`
	Contributions map[string]frontierT10AggregateContribution `json:"contributions"`
	Reports       map[string]frontierT10AggregateReportRecord `json:"reports"`
	Accounting    []frontierT10AccountEntry                   `json:"accounting"`
	Receipts      []frontierT10LedgerReceipt                  `json:"receipts"`
	UpdatedOn     string                                      `json:"updated_on"`
	StateHash     string                                      `json:"state_hash"`
}

type frontierT10AggregateStore struct {
	mu              sync.Mutex
	enabled         bool
	path            string
	dedicatedParent bool
	maxBytes        int
	maxRecords      int
	state           frontierT10AggregateState
	fileBytes       int64
	lastErrorCode   string
}

func frontierT10RandomSecret() (string, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(secret), nil
}

func emptyFrontierT10AggregateState() (frontierT10AggregateState, error) {
	secret, err := frontierT10RandomSecret()
	if err != nil {
		return frontierT10AggregateState{}, err
	}
	state := frontierT10AggregateState{
		SchemaID: frontierT10StateSchemaID, Version: 1, LocalSecret: secret,
		Contributions: map[string]frontierT10AggregateContribution{},
		Reports:       map[string]frontierT10AggregateReportRecord{},
		Accounting:    []frontierT10AccountEntry{}, Receipts: []frontierT10LedgerReceipt{},
		UpdatedOn: frontierT10Date(time.Now().UTC()),
	}
	state.StateHash = frontierT10StateHash(state)
	return state, nil
}

func frontierT10StateHash(state frontierT10AggregateState) string {
	state.StateHash = ""
	return frontierT6Digest(state)
}

func frontierT10StatePath() string {
	if explicit := strings.TrimSpace(os.Getenv(frontierT10PathEnv)); explicit != "" {
		return filepath.Clean(explicit)
	}
	return resolveStoragePath(frontierT10PathEnv, frontierT10DefaultPath)
}

func newFrontierT10AggregateStoreFromEnv() (*frontierT10AggregateStore, error) {
	state, err := emptyFrontierT10AggregateState()
	if err != nil {
		return nil, err
	}
	store := &frontierT10AggregateStore{
		enabled: envBool(frontierT10EnabledEnv, true), path: frontierT10StatePath(),
		dedicatedParent: strings.TrimSpace(os.Getenv(frontierT10PathEnv)) == "",
		maxBytes:        clampInt(envInt(frontierT10MaxBytesEnv, frontierT10DefaultMaxBytes), 64*1024, frontierT10MaximumBytes),
		maxRecords:      clampInt(envInt(frontierT10MaxRecordsEnv, frontierT10DefaultMaxRecords), 100, frontierT10MaximumRecords),
		state:           state,
	}
	if !store.enabled || strings.TrimSpace(store.path) == "" {
		store.enabled = false
		return store, nil
	}
	if err := prepareOwnerOnlyFile(store.path, store.dedicatedParent); err != nil {
		return store, err
	}
	if err := store.load(); err != nil {
		return store, err
	}
	return store, nil
}

func (s *frontierT10AggregateStore) load() error {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) || len(strings.TrimSpace(string(raw))) == 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > s.maxBytes {
		return errors.New("Aggregate Signal state exceeds max bytes")
	}
	var state frontierT10AggregateState
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode Aggregate Signal state: %w", err)
	}
	if err := frontierT10ValidateState(state, s.maxRecords); err != nil {
		return err
	}
	s.state = state
	s.fileBytes = int64(len(raw))
	return nil
}

func frontierT10ValidateState(state frontierT10AggregateState, maxRecords int) error {
	if state.SchemaID != frontierT10StateSchemaID || state.Version != 1 {
		return errors.New("Aggregate Signal state schema is invalid")
	}
	secret, err := base64.RawURLEncoding.DecodeString(state.LocalSecret)
	if err != nil || len(secret) != 32 {
		return errors.New("Aggregate Signal local secret is invalid")
	}
	if state.Contributions == nil || state.Reports == nil || state.Accounting == nil || state.Receipts == nil {
		return errors.New("Aggregate Signal state collections are invalid")
	}
	if len(state.Contributions)+len(state.Reports)+len(state.Accounting)+len(state.Receipts) > maxRecords {
		return errors.New("Aggregate Signal state exceeds max records")
	}
	if state.StateHash != frontierT10StateHash(state) {
		return errors.New("Aggregate Signal state hash mismatch")
	}
	for _, contribution := range state.Contributions {
		if err := frontierT10ValidateContribution(contribution); err != nil {
			return fmt.Errorf("stored Aggregate Signal contribution is invalid: %w", err)
		}
	}
	return nil
}

func frontierT10CloneState(state frontierT10AggregateState) frontierT10AggregateState {
	raw, _ := json.Marshal(state)
	var cloned frontierT10AggregateState
	_ = json.Unmarshal(raw, &cloned)
	return cloned
}

func (s *frontierT10AggregateStore) saveLocked(now time.Time) error {
	s.pruneLocked(now)
	if len(s.state.Contributions)+len(s.state.Reports)+len(s.state.Accounting)+len(s.state.Receipts) > s.maxRecords {
		return errors.New("Aggregate Signal state exceeds max records")
	}
	s.state.UpdatedOn = frontierT10Date(now)
	s.state.StateHash = frontierT10StateHash(s.state)
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > s.maxBytes {
		return errors.New("Aggregate Signal state exceeds max bytes")
	}
	if err := writeOwnerOnlyDurableAtomicFile(s.path, raw, s.dedicatedParent); err != nil {
		return err
	}
	s.fileBytes = int64(len(raw))
	s.lastErrorCode = ""
	return nil
}

func (s *frontierT10AggregateStore) pruneLocked(now time.Time) {
	today := frontierT10Date(now)
	for key, contribution := range s.state.Contributions {
		if contribution.ExpiresOn != "" && contribution.ExpiresOn < today {
			delete(s.state.Contributions, key)
		}
	}
	for key, report := range s.state.Reports {
		if report.Receipt.ExpiresOn != "" && report.Receipt.ExpiresOn < today {
			delete(s.state.Reports, key)
		}
	}
	accounting := s.state.Accounting[:0]
	for _, entry := range s.state.Accounting {
		if entry.ExpiresOn == "" || entry.ExpiresOn >= today {
			accounting = append(accounting, entry)
		}
	}
	s.state.Accounting = accounting
	if len(s.state.Receipts) > 256 {
		s.state.Receipts = append([]frontierT10LedgerReceipt(nil), s.state.Receipts[len(s.state.Receipts)-256:]...)
	}
}

func frontierT10Date(now time.Time) string {
	return now.UTC().Format("2006-01-02")
}

func frontierT10Window(now time.Time) string {
	year, week := now.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func frontierT10WindowStart(window string) (time.Time, error) {
	matches := frontierT10WindowPattern.FindStringSubmatch(strings.TrimSpace(window))
	if len(matches) != 3 {
		return time.Time{}, errors.New("cohort_window must use ISO week format YYYY-Www")
	}
	year, _ := strconv.Atoi(matches[1])
	week, _ := strconv.Atoi(matches[2])
	if week < 1 || week > 53 {
		return time.Time{}, errors.New("cohort_window is invalid")
	}
	jan4 := time.Date(year, time.January, 4, 0, 0, 0, 0, time.UTC)
	weekday := int(jan4.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	start := jan4.AddDate(0, 0, -(weekday-1)+(week-1)*7)
	actualYear, actualWeek := start.ISOWeek()
	if actualYear != year || actualWeek != week {
		return time.Time{}, errors.New("cohort_window is invalid")
	}
	return start, nil
}

func frontierT10ValidateWindow(window string, now time.Time, currentOnly bool) (string, time.Time, error) {
	window = firstNonEmptyStrings(strings.TrimSpace(window), frontierT10Window(now))
	start, err := frontierT10WindowStart(window)
	if err != nil {
		return "", time.Time{}, err
	}
	currentStart, _ := frontierT10WindowStart(frontierT10Window(now))
	if start.After(currentStart) {
		return "", time.Time{}, errors.New("future cohort_window is not allowed")
	}
	if currentOnly && !start.Equal(currentStart) {
		return "", time.Time{}, errors.New("queued contributions must use the current cohort window")
	}
	if start.Before(currentStart.AddDate(0, 0, -7*frontierT10DefaultExpiryWeeks)) {
		return "", time.Time{}, errors.New("cohort_window is outside the retention horizon")
	}
	return window, start, nil
}

func frontierT10SecretBytes(encoded string) ([]byte, error) {
	secret, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(secret) != 32 {
		return nil, errors.New("Aggregate Signal local secret is invalid")
	}
	return secret, nil
}

func frontierT10HMAC(secret []byte, parts ...string) string {
	hash := hmac.New(sha256.New, secret)
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return "sha256:" + fmt.Sprintf("%x", hash.Sum(nil))
}

func frontierT10ContributionMaterial(contribution frontierT10AggregateContribution) map[string]any {
	return map[string]any{
		"schema_id": contribution.SchemaID, "version": contribution.Version,
		"metric": contribution.Metric, "statistic_type": contribution.StatisticType,
		"cohort_window": contribution.CohortWindow, "unit_epoch_commitment": contribution.UnitEpochCommitment,
		"nonce_digest": contribution.NonceDigest, "statistic": contribution.Statistic,
		"clipping": contribution.Clipping, "source": contribution.Source, "expires_on": contribution.ExpiresOn,
	}
}

func frontierT10ContributionDigest(contribution frontierT10AggregateContribution) string {
	return frontierT6Digest(frontierT10ContributionMaterial(contribution))
}

func frontierT10ContributionID(contribution frontierT10AggregateContribution) string {
	digest := strings.TrimPrefix(frontierT10ContributionDigest(contribution), "sha256:")
	return "agc_" + digest[:24]
}

func frontierT10NormalizeMetric(raw string) (string, frontierT10MetricSpec, error) {
	metric := strings.ToLower(strings.TrimSpace(raw))
	spec, ok := frontierT10MetricSpecs[metric]
	if !ok {
		return "", frontierT10MetricSpec{}, errors.New("metric is not allowlisted")
	}
	return metric, spec, nil
}

func frontierT10NormalizeSource(raw string) (string, error) {
	source := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(raw, "manual")))
	if _, ok := frontierT10AllowedSources[source]; !ok {
		return "", errors.New("source is not allowlisted")
	}
	return source, nil
}

func frontierT10Category(spec frontierT10MetricSpec, raw string) (string, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	for _, allowed := range spec.Categories {
		if value == allowed {
			return value, true
		}
	}
	return "", false
}

func frontierT10BuildContribution(metric, source, window, unitCommitment, nonceDigest string, value *float64, category string, expiresOn string) (frontierT10AggregateContribution, error) {
	metric, spec, err := frontierT10NormalizeMetric(metric)
	if err != nil {
		return frontierT10AggregateContribution{}, err
	}
	source, err = frontierT10NormalizeSource(source)
	if err != nil {
		return frontierT10AggregateContribution{}, err
	}
	contribution := frontierT10AggregateContribution{
		SchemaID: frontierT10ContributionContractID, Version: 1, Metric: metric,
		StatisticType: spec.Kind, CohortWindow: window, UnitEpochCommitment: unitCommitment,
		NonceDigest: nonceDigest, Source: source, ExpiresOn: expiresOn,
	}
	switch spec.Kind {
	case "numeric":
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return frontierT10AggregateContribution{}, errors.New("numeric metric requires one finite value")
		}
		clipped := math.Max(spec.Minimum, math.Min(spec.Maximum, *value))
		contribution.Statistic = map[string]any{"numeric_value": roundFloat(clipped, 8)}
		contribution.Clipping = map[string]any{
			"minimum": spec.Minimum, "maximum": spec.Maximum, "applied": clipped != *value,
		}
	case "categorical":
		category, ok := frontierT10Category(spec, category)
		if !ok {
			return frontierT10AggregateContribution{}, errors.New("category is not allowlisted for metric")
		}
		contribution.Statistic = map[string]any{"category": category}
		contribution.Clipping = map[string]any{"allowed_categories": append([]string(nil), spec.Categories...), "applied": false}
	default:
		return frontierT10AggregateContribution{}, errors.New("metric statistic type is invalid")
	}
	contribution.ContributionID = frontierT10ContributionID(contribution)
	contribution.ContributionDigest = frontierT10ContributionDigest(contribution)
	return contribution, nil
}

func frontierT10ExactMapKeys(value map[string]any, allowed ...string) bool {
	set := map[string]struct{}{}
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range value {
		if _, ok := set[key]; !ok {
			return false
		}
	}
	return true
}

func frontierT10ValidateContribution(contribution frontierT10AggregateContribution) error {
	metric, spec, err := frontierT10NormalizeMetric(contribution.Metric)
	if err != nil || metric != contribution.Metric {
		return errors.New("contribution metric is invalid")
	}
	if contribution.SchemaID != frontierT10ContributionContractID || contribution.Version != 1 || contribution.StatisticType != spec.Kind {
		return errors.New("contribution schema is invalid")
	}
	if _, err := frontierT10WindowStart(contribution.CohortWindow); err != nil {
		return err
	}
	if !frontierT10DigestPattern.MatchString(contribution.UnitEpochCommitment) || !frontierT10DigestPattern.MatchString(contribution.NonceDigest) {
		return errors.New("contribution epoch or nonce commitment is invalid")
	}
	if _, err := frontierT10NormalizeSource(contribution.Source); err != nil {
		return err
	}
	if _, err := time.Parse("2006-01-02", contribution.ExpiresOn); err != nil {
		return errors.New("contribution expiry is invalid")
	}
	if spec.Kind == "numeric" {
		if !frontierT10ExactMapKeys(contribution.Statistic, "numeric_value") || !frontierT10ExactMapKeys(contribution.Clipping, "minimum", "maximum", "applied") {
			return errors.New("numeric contribution fields are invalid")
		}
		value, ok := contextPolicyNumber(contribution.Statistic["numeric_value"])
		minimum, minOK := contextPolicyNumber(contribution.Clipping["minimum"])
		maximum, maxOK := contextPolicyNumber(contribution.Clipping["maximum"])
		if !ok || !minOK || !maxOK || value < spec.Minimum || value > spec.Maximum || minimum != spec.Minimum || maximum != spec.Maximum {
			return errors.New("numeric contribution bounds are invalid")
		}
	} else {
		if !frontierT10ExactMapKeys(contribution.Statistic, "category") || !frontierT10ExactMapKeys(contribution.Clipping, "allowed_categories", "applied") {
			return errors.New("categorical contribution fields are invalid")
		}
		if _, ok := frontierT10Category(spec, anyToString(contribution.Statistic["category"])); !ok {
			return errors.New("categorical contribution value is invalid")
		}
		actual := anyToStringSlice(contribution.Clipping["allowed_categories"])
		if strings.Join(actual, "\x00") != strings.Join(spec.Categories, "\x00") || anyToBool(contribution.Clipping["applied"]) {
			return errors.New("categorical contribution allowlist is invalid")
		}
	}
	if contribution.ContributionDigest != frontierT10ContributionDigest(contribution) || contribution.ContributionID != frontierT10ContributionID(contribution) {
		return errors.New("contribution digest is invalid")
	}
	return nil
}

func (s *frontierT10AggregateStore) localCommitments(metric, window, nonce string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	secret, err := frontierT10SecretBytes(s.state.LocalSecret)
	if err != nil {
		return "", "", err
	}
	return frontierT10HMAC(secret, "unit", metric, window), frontierT10HMAC(secret, "nonce", metric, window, nonce), nil
}

func frontierT10ContributionKey(metric, window string) string {
	return metric + "\x00" + window
}

func (s *frontierT10AggregateStore) queue(contribution frontierT10AggregateContribution, now time.Time) (frontierT10AggregateContribution, bool, error) {
	if s == nil || !s.enabled {
		return frontierT10AggregateContribution{}, false, errors.New("Aggregate Signal store is unavailable")
	}
	if err := frontierT10ValidateContribution(contribution); err != nil {
		return frontierT10AggregateContribution{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	secret, err := frontierT10SecretBytes(s.state.LocalSecret)
	if err != nil {
		return frontierT10AggregateContribution{}, false, err
	}
	if contribution.UnitEpochCommitment != frontierT10HMAC(secret, "unit", contribution.Metric, contribution.CohortWindow) {
		return frontierT10AggregateContribution{}, false, errors.New("contribution epoch commitment is stale")
	}
	key := frontierT10ContributionKey(contribution.Metric, contribution.CohortWindow)
	if _, released := s.state.Reports[frontierT10ReleaseKey(contribution.Metric, contribution.CohortWindow)]; released {
		return frontierT10AggregateContribution{}, false, errFrontierT10Replay
	}
	if existing, ok := s.state.Contributions[key]; ok {
		if existing.ContributionDigest == contribution.ContributionDigest && existing.NonceDigest == contribution.NonceDigest {
			return existing, true, nil
		}
		return frontierT10AggregateContribution{}, false, errFrontierT10Replay
	}
	previous := frontierT10CloneState(s.state)
	s.state.OptedIn = true
	s.state.Contributions[key] = contribution
	receiptDigest := frontierT6Digest(map[string]any{"operation": "queue", "contribution_digest": contribution.ContributionDigest})
	s.state.Receipts = append(s.state.Receipts, frontierT10LedgerReceipt{
		SchemaID: frontierT10ReceiptSchemaID, ReceiptID: "agr_" + strings.TrimPrefix(receiptDigest, "sha256:")[:24],
		Operation: "queue", Decision: "accepted", Metric: contribution.Metric, CohortWindow: contribution.CohortWindow,
		RecordedOn: frontierT10Date(now), ExpiresOn: contribution.ExpiresOn, RequestDigest: receiptDigest,
	})
	if err := s.saveLocked(now); err != nil {
		s.state = previous
		s.lastErrorCode = "persist_failed"
		return frontierT10AggregateContribution{}, false, err
	}
	return contribution, false, nil
}

func frontierT10ReleaseKey(metric, window string) string {
	return metric + "\x00" + window
}

func frontierT10RequestDigest(metric, window string, epsilon, delta float64, contributions []frontierT10AggregateContribution) string {
	ids := make([]string, 0, len(contributions))
	for _, contribution := range contributions {
		ids = append(ids, contribution.ContributionDigest)
	}
	sort.Strings(ids)
	return frontierT6Digest(map[string]any{
		"metric": metric, "cohort_window": window, "epsilon": epsilon, "delta": delta, "contribution_digests": ids,
	})
}

func frontierT10RollingComposition(entries []frontierT10AccountEntry, now time.Time) (float64, float64) {
	cutoff := now.UTC().AddDate(0, 0, -frontierT10RollingWindowDays)
	epsilon, delta := 0.0, 0.0
	for _, entry := range entries {
		released, err := time.Parse("2006-01-02", entry.ReleasedOn)
		if err == nil && !released.Before(cutoff) {
			epsilon += entry.Epsilon
			delta += entry.Delta
		}
	}
	return roundFloat(epsilon, 10), roundFloat(delta, 12)
}

func frontierT10AccountantSnapshot(entries []frontierT10AccountEntry, releaseEpsilon, releaseDelta float64, now time.Time) map[string]any {
	epsilon, delta := frontierT10RollingComposition(entries, now)
	return map[string]any{
		"schema_id":       frontierT10AccountantContractID,
		"release_epsilon": releaseEpsilon, "rolling_90_day_epsilon": epsilon,
		"delta": releaseDelta, "rolling_90_day_delta": delta,
		"budget_remaining": roundFloat(math.Max(0, frontierT10RollingEpsilonMax-epsilon), 10),
		"composition":      "basic_composition_v1", "window_days": frontierT10RollingWindowDays,
	}
}

func frontierT10RandomNormal() (float64, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return 0, err
	}
	u1 := (float64(binary.BigEndian.Uint64(raw[:8])) + 1) / (float64(^uint64(0)) + 2)
	u2 := (float64(binary.BigEndian.Uint64(raw[8:])) + 1) / (float64(^uint64(0)) + 2)
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2), nil
}

func frontierT10GaussianSigma(sensitivity, epsilon, delta float64) float64 {
	return sensitivity * math.Sqrt(2*math.Log(1.25/delta)) / epsilon
}

func frontierT10AggregateEstimate(spec frontierT10MetricSpec, contributions []frontierT10AggregateContribution, epsilon, delta float64) (map[string]any, map[string]any, error) {
	if spec.Kind == "numeric" {
		total := 0.0
		for _, contribution := range contributions {
			value, ok := contextPolicyNumber(contribution.Statistic["numeric_value"])
			if !ok {
				return nil, nil, errors.New("numeric contribution is invalid")
			}
			total += value
		}
		sensitivity := (spec.Maximum - spec.Minimum) / float64(len(contributions))
		sigma := frontierT10GaussianSigma(sensitivity, epsilon, delta)
		noise, err := frontierT10RandomNormal()
		if err != nil {
			return nil, nil, err
		}
		noisyMean := math.Max(spec.Minimum, math.Min(spec.Maximum, total/float64(len(contributions))+noise*sigma))
		return map[string]any{
			"mechanism": "gaussian_v1", "noisy_mean": roundFloat(noisyMean, 8),
			"noise_sigma": roundFloat(sigma, 8), "clipped_to_metric_bounds": true,
		}, map[string]any{"active": false, "rare_categories_suppressed": false}, nil
	}
	counts := map[string]int{}
	for _, contribution := range contributions {
		counts[anyToString(contribution.Statistic["category"])]++
	}
	noisy := map[string]any{}
	sigma := frontierT10GaussianSigma(1, epsilon, delta)
	suppressed := false
	for _, category := range spec.Categories {
		count := counts[category]
		if count < frontierT10MinimumCohort {
			if count > 0 {
				suppressed = true
			}
			continue
		}
		noise, err := frontierT10RandomNormal()
		if err != nil {
			return nil, nil, err
		}
		noisy[category] = roundFloat(math.Max(0, float64(count)+noise*sigma), 6)
	}
	return map[string]any{
		"mechanism": "gaussian_histogram_v1", "noisy_counts": noisy,
		"noise_sigma": roundFloat(sigma, 8), "rare_categories_suppressed": suppressed,
	}, map[string]any{"active": suppressed, "rare_categories_suppressed": suppressed}, nil
}

func frontierT10ValidateReleaseInputs(metric, window string, contributions []frontierT10AggregateContribution, now time.Time) (frontierT10MetricSpec, error) {
	metric, spec, err := frontierT10NormalizeMetric(metric)
	if err != nil {
		return frontierT10MetricSpec{}, err
	}
	if _, _, err := frontierT10ValidateWindow(window, now, false); err != nil {
		return frontierT10MetricSpec{}, err
	}
	if len(contributions) > frontierT10MaximumContributionsPerRequest {
		return frontierT10MetricSpec{}, errors.New("contribution batch exceeds the input limit")
	}
	seenUnits := map[string]struct{}{}
	seenContributions := map[string]struct{}{}
	for _, contribution := range contributions {
		if err := frontierT10ValidateContribution(contribution); err != nil {
			return frontierT10MetricSpec{}, err
		}
		if contribution.Metric != metric || contribution.CohortWindow != window || contribution.StatisticType != spec.Kind {
			return frontierT10MetricSpec{}, errors.New("contribution cohort does not match the release")
		}
		if _, exists := seenUnits[contribution.UnitEpochCommitment]; exists {
			return frontierT10MetricSpec{}, errFrontierT10Replay
		}
		if _, exists := seenContributions[contribution.ContributionDigest]; exists {
			return frontierT10MetricSpec{}, errFrontierT10Replay
		}
		seenUnits[contribution.UnitEpochCommitment] = struct{}{}
		seenContributions[contribution.ContributionDigest] = struct{}{}
	}
	return spec, nil
}

func (s *frontierT10AggregateStore) release(metric, window string, contributions []frontierT10AggregateContribution, epsilon, delta float64, expiryWeeks int, now time.Time) (frontierT10AggregateReportRecord, bool, error) {
	if s == nil || !s.enabled {
		return frontierT10AggregateReportRecord{}, false, errors.New("Aggregate Signal store is unavailable")
	}
	spec, err := frontierT10ValidateReleaseInputs(metric, window, contributions, now)
	if err != nil {
		return frontierT10AggregateReportRecord{}, false, err
	}
	if len(contributions) < frontierT10MinimumCohort {
		return frontierT10AggregateReportRecord{}, false, errors.New("cohort_suppressed")
	}
	if epsilon <= 0 || epsilon > frontierT10ReleaseEpsilonMax || delta <= 0 || delta > frontierT10DeltaMax {
		return frontierT10AggregateReportRecord{}, false, errors.New("privacy release parameters exceed the contract")
	}
	if expiryWeeks <= 0 {
		expiryWeeks = frontierT10DefaultExpiryWeeks
	}
	expiryWeeks = clampInt(expiryWeeks, 1, frontierT10DefaultExpiryWeeks)
	requestDigest := frontierT10RequestDigest(metric, window, epsilon, delta, contributions)
	key := frontierT10ReleaseKey(metric, window)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if existing, ok := s.state.Reports[key]; ok {
		if existing.RequestDigest != requestDigest {
			return frontierT10AggregateReportRecord{}, false, errFrontierT10Differencing
		}
		return existing, true, nil
	}
	rollingEpsilon, rollingDelta := frontierT10RollingComposition(s.state.Accounting, now)
	if rollingEpsilon+epsilon > frontierT10RollingEpsilonMax+1e-12 || rollingDelta+delta > frontierT10DeltaMax+1e-15 {
		return frontierT10AggregateReportRecord{}, false, errFrontierT10Budget
	}
	estimate, suppression, err := frontierT10AggregateEstimate(spec, contributions, epsilon, delta)
	if err != nil {
		return frontierT10AggregateReportRecord{}, false, err
	}
	reportDigest := frontierT6Digest(map[string]any{"request_digest": requestDigest, "estimate": estimate})
	reportID := "ags_" + strings.TrimPrefix(reportDigest, "sha256:")[:24]
	receiptID := "agr_" + strings.TrimPrefix(frontierT6Digest(map[string]any{"report_id": reportID, "request_digest": requestDigest}), "sha256:")[:24]
	releasedOn := frontierT10Date(now)
	expiresOn := frontierT10Date(now.AddDate(0, 0, expiryWeeks*7))
	receipt := frontierT10LedgerReceipt{
		SchemaID: frontierT10ReceiptSchemaID, ReceiptID: receiptID, Operation: "report",
		Decision: "released", Metric: metric, CohortWindow: window, RecordedOn: releasedOn,
		ReleaseWindow: frontierT10Window(now), ExpiresOn: expiresOn, RequestDigest: requestDigest,
	}
	entry := frontierT10AccountEntry{
		ReceiptID: receiptID, ReportID: reportID, Metric: metric, CohortWindow: window,
		ReleasedOn: releasedOn, ExpiresOn: frontierT10Date(now.AddDate(0, 0, frontierT10RollingWindowDays)),
		Epsilon: epsilon, Delta: delta, RequestDigest: requestDigest,
	}
	accountingAfter := append(append([]frontierT10AccountEntry(nil), s.state.Accounting...), entry)
	record := frontierT10AggregateReportRecord{
		ReportID: reportID, Metric: metric, StatisticType: spec.Kind, CohortWindow: window,
		CohortSize: len(contributions), Estimate: estimate, Suppression: suppression,
		Accountant: frontierT10AccountantSnapshot(accountingAfter, epsilon, delta, now),
		Receipt:    receipt, RequestDigest: requestDigest,
	}
	previous := frontierT10CloneState(s.state)
	s.state.Reports[key] = record
	s.state.Accounting = accountingAfter
	s.state.Receipts = append(s.state.Receipts, receipt)
	delete(s.state.Contributions, frontierT10ContributionKey(metric, window))
	if err := s.saveLocked(now); err != nil {
		s.state = previous
		s.lastErrorCode = "persist_failed"
		return frontierT10AggregateReportRecord{}, false, err
	}
	return record, false, nil
}

func (s *frontierT10AggregateStore) localContributions(metric, window string, now time.Time) []frontierT10AggregateContribution {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	contribution, ok := s.state.Contributions[frontierT10ContributionKey(metric, window)]
	if !ok {
		return nil
	}
	return []frontierT10AggregateContribution{contribution}
}

func (s *frontierT10AggregateStore) optOut(now time.Time) (frontierT10LedgerReceipt, int, error) {
	if s == nil || !s.enabled {
		return frontierT10LedgerReceipt{}, 0, errors.New("Aggregate Signal store is unavailable")
	}
	secret, err := frontierT10RandomSecret()
	if err != nil {
		return frontierT10LedgerReceipt{}, 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := frontierT10CloneState(s.state)
	deleted := len(s.state.Contributions)
	s.state.OptedIn = false
	s.state.Generation++
	s.state.LocalSecret = secret
	s.state.Contributions = map[string]frontierT10AggregateContribution{}
	digest := frontierT6Digest(map[string]any{"operation": "opt_out", "generation": s.state.Generation, "recorded_on": frontierT10Date(now)})
	receipt := frontierT10LedgerReceipt{
		SchemaID: frontierT10ReceiptSchemaID, ReceiptID: "agr_" + strings.TrimPrefix(digest, "sha256:")[:24],
		Operation: "opt_out", Decision: "future_contribution_stopped", RecordedOn: frontierT10Date(now),
		ExpiresOn: frontierT10Date(now.AddDate(0, 0, frontierT10DefaultExpiryWeeks*7)), RequestDigest: digest,
	}
	s.state.Receipts = append(s.state.Receipts, receipt)
	if err := s.saveLocked(now); err != nil {
		s.state = previous
		s.lastErrorCode = "persist_failed"
		return frontierT10LedgerReceipt{}, 0, err
	}
	return receipt, deleted, nil
}

func frontierT10RequiredReviews(status string) []any {
	reviews := []string{"membership_inference", "attribute_inference", "reconstruction", "malicious_client", "accountant_exhaustion", "utility_loss"}
	rows := make([]any, 0, len(reviews))
	for _, review := range reviews {
		rows = append(rows, map[string]any{"review": review, "status": status})
	}
	return rows
}

func frontierT10Safety() map[string]any {
	return map[string]any{
		"network_calls": 0, "bytes_sent": 0, "raw_memory_exported": false,
		"raw_prompt_exported": false, "stable_installation_id_exported": false,
		"external_transport_performed": false, "formal_privacy_claim": false,
		"model_execution": false, "runner_execution": false,
	}
}

func (s *frontierT10AggregateStore) statusPayload(operation string, now time.Time) map[string]any {
	if s == nil {
		return frontierT10AccountantResponse(operation, false, nil, nil, 0, 0, "store_unavailable", now)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	epsilon, delta := frontierT10RollingComposition(s.state.Accounting, now)
	receipts := make([]any, 0, minInt(len(s.state.Receipts), 64))
	start := maxInt(0, len(s.state.Receipts)-64)
	for _, receipt := range s.state.Receipts[start:] {
		receipts = append(receipts, receipt)
	}
	payload := frontierT10AccountantResponse(operation, s.state.OptedIn, receipts, map[string]any{
		"pending_contributions": len(s.state.Contributions), "released_reports": len(s.state.Reports),
		"generation": s.state.Generation,
	}, epsilon, delta, s.lastErrorCode, now)
	limits := anyMap(payload["limits"])
	limits["max_bytes"], limits["max_records"] = s.maxBytes, s.maxRecords
	return payload
}

func (s *frontierT10AggregateStore) accountingSnapshot(now time.Time) []frontierT10AccountEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	return append([]frontierT10AccountEntry(nil), s.state.Accounting...)
}

func frontierT10AccountantResponse(operation string, optedIn bool, receipts []any, queue map[string]any, epsilon, delta float64, lastError string, now time.Time) map[string]any {
	if receipts == nil {
		receipts = []any{}
	}
	if queue == nil {
		queue = map[string]any{"pending_contributions": 0, "released_reports": 0, "generation": 0}
	}
	payload := map[string]any{
		"ok": true, "schema_id": frontierT10AccountantContractID, "version": 1,
		"operation": operation, "opted_in": optedIn, "research_status": "research_only",
		"queue": queue,
		"composition": map[string]any{
			"rolling_90_day_epsilon": epsilon, "rolling_90_day_delta": delta,
			"epsilon_remaining": roundFloat(math.Max(0, frontierT10RollingEpsilonMax-epsilon), 10),
			"accounting":        "basic_composition_v1",
		},
		"limits": map[string]any{
			"minimum_cohort": frontierT10MinimumCohort, "release_epsilon_max": frontierT10ReleaseEpsilonMax,
			"rolling_90_day_epsilon_max": frontierT10RollingEpsilonMax, "delta_max": frontierT10DeltaMax,
			"max_bytes": frontierT10MaximumBytes, "max_records": frontierT10MaximumRecords,
		},
		"receipts": receipts,
		"promotion_gate": map[string]any{
			"passed": false, "status": "research_only", "required_reviews": frontierT10RequiredReviews("pending_independent_review"),
		},
		"measurement": map[string]any{"network_calls": 0, "bytes_sent": 0, "window": frontierT10Window(now)},
		"safety":      frontierT10Safety(),
	}
	if strings.TrimSpace(lastError) != "" {
		payload["warnings"] = []any{lastError}
	}
	return attachPayloadFormatContract(frontierT10AccountantContractID, payload, "", "aggregate_signal", frontierT10AggregatePath)
}

func frontierT10ContributionResponse(operation, decision string, contribution frontierT10AggregateContribution, persisted, replayed bool) map[string]any {
	payload := map[string]any{
		"ok": true, "schema_id": frontierT10ContributionContractID, "version": 1,
		"operation": operation, "decision": decision, "contribution_id": contribution.ContributionID,
		"metric": contribution.Metric, "statistic_type": contribution.StatisticType,
		"cohort_window": contribution.CohortWindow, "unit_epoch_commitment": contribution.UnitEpochCommitment,
		"nonce_digest": contribution.NonceDigest, "statistic": contribution.Statistic,
		"clipping": contribution.Clipping, "source": contribution.Source, "persisted": persisted,
		"expires_on": contribution.ExpiresOn, "contribution_digest": contribution.ContributionDigest,
		"privacy": map[string]any{
			"opt_in_required": true, "local_only": true, "one_per_unit_metric_week": true,
			"epsilon_consumed": 0.0, "delta_consumed": 0.0, "idempotent_replay": replayed,
		},
		"safety": frontierT10Safety(),
	}
	return attachPayloadFormatContract(frontierT10ContributionContractID, payload, "", "aggregate_signal", frontierT10AggregatePath)
}

func frontierT10SuppressedReport(metric, statisticType, window string, accounting []frontierT10AccountEntry, now time.Time) map[string]any {
	requestDigest := frontierT6Digest(map[string]any{"metric": metric, "cohort_window": window, "decision": "suppressed"})
	receipt := frontierT10LedgerReceipt{
		SchemaID: frontierT10ReceiptSchemaID, ReceiptID: "agr_" + strings.TrimPrefix(requestDigest, "sha256:")[:24],
		Operation: "report", Decision: "suppressed", Metric: metric, CohortWindow: window,
		ReleaseWindow: frontierT10Window(now), RecordedOn: frontierT10Date(now), ExpiresOn: frontierT10Date(now.AddDate(0, 0, 7)),
		RequestDigest: requestDigest,
	}
	payload := map[string]any{
		"ok": true, "schema_id": frontierT10ReportContractID, "version": 1,
		"operation": "report", "decision": "suppressed", "report_id": "",
		"metric": metric, "statistic_type": statisticType, "cohort_window": window,
		"cohort":             map[string]any{"minimum_size": frontierT10MinimumCohort, "exact_count_disclosed": false, "size_band": "below_threshold"},
		"estimate":           map[string]any{},
		"suppression":        map[string]any{"active": true, "reason": "minimum_cohort_not_met", "rare_categories_suppressed": true},
		"privacy_accountant": frontierT10AccountantSnapshot(accounting, 0, 0, now),
		"receipt":            receipt, "safety": frontierT10Safety(),
	}
	return attachPayloadFormatContract(frontierT10ReportContractID, payload, "", "aggregate_signal", frontierT10AggregatePath)
}

func frontierT10ReportResponse(record frontierT10AggregateReportRecord, replayed bool) map[string]any {
	receipt := record.Receipt
	receipt.Idempotent = replayed
	payload := map[string]any{
		"ok": true, "schema_id": frontierT10ReportContractID, "version": 1,
		"operation": "report", "decision": "released", "report_id": record.ReportID,
		"metric": record.Metric, "statistic_type": record.StatisticType, "cohort_window": record.CohortWindow,
		"cohort": map[string]any{
			"minimum_size": frontierT10MinimumCohort, "exact_count_disclosed": true,
			"eligible_count": record.CohortSize, "contribution_coverage": 1.0,
		},
		"estimate": record.Estimate, "suppression": record.Suppression,
		"privacy_accountant": record.Accountant, "receipt": receipt, "safety": frontierT10Safety(),
	}
	return attachPayloadFormatContract(frontierT10ReportContractID, payload, "", "aggregate_signal", frontierT10AggregatePath)
}
