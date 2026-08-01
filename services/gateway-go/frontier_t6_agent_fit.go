package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	frontierT6AgentFitContractID = "frontier_t6_agent_fit.v1"

	frontierT6AsyncSteeringFeatureID        = "frontier_async_steering_delivery"
	frontierT6RunnerSelectionFeatureID      = "frontier_runner_selection_activation"
	frontierT6AgentContextFeatureID         = "frontier_agent_context_automation"
	frontierT6ProactiveContextPrepFeatureID = "frontier_proactive_context_preparation"

	frontierT6SteeringEventSchemaID    = "async_steering_event.v1"
	frontierT6SteeringDeliverySchemaID = "async_steering_delivery.v1"
	frontierT6SteeringStreamItemID     = "async_steering_stream_item.v1"
	frontierT6RunnerSelectionSchemaID  = "runner_selection.v1"
	frontierT6ModelSelectionSchemaID   = "model_selection.v1"
	frontierT6ContextProfileSchemaID   = "agent_context_profile.v1"
	frontierT6ContextPrepSchemaID      = "context_prep.v1"
	frontierT6ContextPrepArtifactID    = "context_prep_artifact.v1"
	frontierT6StateSchemaID            = "frontier_t6_agent_fit_state.v1"
	frontierT6StatusSchemaID           = "frontier_t6_agent_fit_status.v1"

	frontierT6StatePathEnv = "CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_PATH"
	frontierT6EnabledEnv   = "CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_ENABLED"

	frontierT6DefaultStatePath     = ".data/orchestrator/frontier_t6_agent_fit.json"
	frontierT6DefaultMaxBytes      = 8 * 1024 * 1024
	frontierT6MaximumMaxBytes      = 64 * 1024 * 1024
	frontierT6DefaultMaxEvents     = 256
	frontierT6MaximumMaxEvents     = 4096
	frontierT6DefaultMaxDeliveries = 1024
	frontierT6MaximumDeliveries    = 16384
	frontierT6DefaultMaxProfiles   = 256
	frontierT6MaximumProfiles      = 4096
	frontierT6DefaultMaxPreps      = 256
	frontierT6MaximumPreps         = 4096

	frontierT6SteeringMaxAttempts  = 5
	frontierT6PrepMaxAttempts      = 3
	frontierT6SelectionSampleFloor = 5
)

var (
	errFrontierT6CursorExpired = errors.New("Frontier T6 replay cursor is outside the bounded replay window")
	errFrontierT6ClaimStale    = errors.New("Frontier T6 delivery claim is stale")
)

type frontierT6Scope struct {
	WorkspaceID string `json:"workspace_id"`
	Project     string `json:"project"`
	SessionID   string `json:"session_id,omitempty"`
	AgentID     string `json:"agent_id,omitempty"`
}

type frontierT6Provenance struct {
	Source              string `json:"source"`
	SourceID            string `json:"source_id"`
	SourceGeneration    string `json:"source_generation"`
	ContentDigest       string `json:"content_digest"`
	AuthorizationDigest string `json:"authorization_digest"`
	ObservedAt          string `json:"observed_at"`
	FreshUntil          string `json:"fresh_until"`
}

type frontierT6StoreLimits struct {
	MaxBytes      int
	MaxEvents     int
	MaxDeliveries int
	MaxProfiles   int
	MaxPreps      int
}

type frontierT6SteeringPublishRequest struct {
	Scope             frontierT6Scope      `json:"scope"`
	Kind              string               `json:"kind"`
	Message           string               `json:"message"`
	SuggestedAction   string               `json:"suggested_action"`
	InjectionBoundary string               `json:"injection_boundary"`
	DedupeKey         string               `json:"dedupe_key,omitempty"`
	TTLSeconds        int                  `json:"ttl_seconds"`
	Provenance        frontierT6Provenance `json:"provenance"`
}

type frontierT6SteeringEvent struct {
	SchemaID          string               `json:"schema_id"`
	Version           int                  `json:"version"`
	Sequence          uint64               `json:"sequence"`
	Cursor            string               `json:"cursor"`
	EventID           string               `json:"event_id"`
	Scope             frontierT6Scope      `json:"scope"`
	Kind              string               `json:"kind"`
	Message           string               `json:"message"`
	SuggestedAction   string               `json:"suggested_action"`
	InjectionBoundary string               `json:"injection_boundary"`
	Fingerprint       string               `json:"fingerprint"`
	Provenance        frontierT6Provenance `json:"provenance"`
	CreatedAt         string               `json:"created_at"`
	ExpiresAt         string               `json:"expires_at"`
	PreviousHash      string               `json:"previous_hash"`
	EventHash         string               `json:"event_hash"`
}

type frontierT6HarnessCapabilities struct {
	HarnessID           string   `json:"harness_id"`
	Transport           string   `json:"transport"`
	SupportsSSE         bool     `json:"supports_sse"`
	SupportsEventIDs    bool     `json:"supports_event_ids"`
	SupportsResume      bool     `json:"supports_resume"`
	SupportsAck         bool     `json:"supports_ack"`
	InjectionBoundaries []string `json:"injection_boundaries"`
	MaxEventBytes       int      `json:"max_event_bytes"`
}

type frontierT6SteeringDelivery struct {
	SchemaID         string `json:"schema_id"`
	Version          int    `json:"version"`
	DeliveryID       string `json:"delivery_id"`
	EventID          string `json:"event_id"`
	EventSequence    uint64 `json:"event_sequence"`
	ScopeDigest      string `json:"scope_digest"`
	SubscriberDigest string `json:"subscriber_digest"`
	HarnessID        string `json:"harness_id"`
	Boundary         string `json:"boundary"`
	Status           string `json:"status"`
	Attempts         int    `json:"attempts"`
	ClaimDigest      string `json:"claim_digest,omitempty"`
	LeaseExpiresAt   string `json:"lease_expires_at,omitempty"`
	NextAttemptAt    string `json:"next_attempt_at,omitempty"`
	LastReasonDigest string `json:"last_reason_digest,omitempty"`
	DeliveredAt      string `json:"delivered_at,omitempty"`
	AcknowledgedAt   string `json:"acknowledged_at,omitempty"`
	AckCursor        string `json:"ack_cursor,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type frontierT6PushItem struct {
	DeliveryID string                  `json:"delivery_id"`
	ClaimToken string                  `json:"claim_token"`
	Event      frontierT6SteeringEvent `json:"event"`
}

type frontierT6SteeringBatch struct {
	SchemaID           string                    `json:"schema_id"`
	DeliveryMode       string                    `json:"delivery_mode"`
	PushNative         bool                      `json:"push_native"`
	FallbackReason     string                    `json:"fallback_reason,omitempty"`
	CursorExpired      bool                      `json:"cursor_expired"`
	RequestedCursor    string                    `json:"requested_cursor,omitempty"`
	NextCursor         string                    `json:"next_cursor"`
	ReplayFloor        string                    `json:"replay_floor"`
	Events             []frontierT6SteeringEvent `json:"events,omitempty"`
	Deliveries         []frontierT6PushItem      `json:"deliveries,omitempty"`
	RecommendedSurface string                    `json:"recommended_surface"`
	NetworkCalls       int                       `json:"network_calls"`
	ExecutionPerformed bool                      `json:"execution_performed"`
}

type frontierT6SelectionEvidence struct {
	TaskClass        string               `json:"task_class"`
	Verified         bool                 `json:"verified"`
	SampleCount      int                  `json:"sample_count"`
	SuccessCount     int                  `json:"success_count"`
	FailureCount     int                  `json:"failure_count"`
	BlockedCount     int                  `json:"blocked_count"`
	QualityScore     float64              `json:"quality_score"`
	Confidence       float64              `json:"confidence"`
	CostKnown        bool                 `json:"cost_known"`
	CostMicrosPer1K  int64                `json:"cost_micros_per_1k"`
	LatencyKnown     bool                 `json:"latency_known"`
	LatencyMillisP50 int64                `json:"latency_millis_p50"`
	Provenance       frontierT6Provenance `json:"provenance"`
}

type frontierT6SelectionCandidate struct {
	CandidateID         string                      `json:"candidate_id"`
	Readiness           string                      `json:"readiness"`
	Capabilities        []string                    `json:"capabilities"`
	ContextWindowTokens int                         `json:"context_window_tokens"`
	Evidence            frontierT6SelectionEvidence `json:"evidence"`
}

type frontierT6SelectionConstraints struct {
	RequiredCapabilities   []string `json:"required_capabilities"`
	AllowedCandidateIDs    []string `json:"allowed_candidate_ids,omitempty"`
	MinimumContextTokens   int      `json:"minimum_context_tokens"`
	MaximumCostMicrosPer1K int64    `json:"maximum_cost_micros_per_1k"`
	MaximumLatencyMillis   int64    `json:"maximum_latency_millis"`
	MinimumSamples         int      `json:"minimum_samples"`
}

type frontierT6SelectionRequest struct {
	TaskClass   string                         `json:"task_class"`
	Candidates  []frontierT6SelectionCandidate `json:"candidates"`
	Constraints frontierT6SelectionConstraints `json:"constraints"`
	Now         time.Time                      `json:"-"`
}

type frontierT6SelectionCandidateReceipt struct {
	CandidateID      string   `json:"candidate_id"`
	Eligible         bool     `json:"eligible"`
	ScoreBasisPoints int      `json:"score_basis_points"`
	SampleCount      int      `json:"sample_count"`
	EvidenceDigest   string   `json:"evidence_digest"`
	Reasons          []string `json:"reasons"`
}

type frontierT6SelectionReceipt struct {
	SchemaID           string                                `json:"schema_id"`
	Version            int                                   `json:"version"`
	ReceiptID          string                                `json:"receipt_id"`
	Kind               string                                `json:"kind"`
	TaskClass          string                                `json:"task_class"`
	Decision           string                                `json:"decision"`
	SelectedID         string                                `json:"selected_id,omitempty"`
	Confidence         string                                `json:"confidence"`
	SampleFloor        int                                   `json:"sample_floor"`
	ConstraintsDigest  string                                `json:"constraints_digest"`
	Candidates         []frontierT6SelectionCandidateReceipt `json:"candidates"`
	Reasons            []string                              `json:"reasons"`
	RecommendedSurface string                                `json:"recommended_surface"`
	AdvisoryOnly       bool                                  `json:"advisory_only"`
	ActivationAllowed  bool                                  `json:"activation_allowed"`
	ExecutionPerformed bool                                  `json:"execution_performed"`
	NetworkCalls       int                                   `json:"network_calls"`
	CreatedAt          string                                `json:"created_at"`
}

type frontierT6AgentCapabilities struct {
	Declared            bool     `json:"declared"`
	AgentFamily         string   `json:"agent_family,omitempty"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty"`
	Tools               []string `json:"tools,omitempty"`
	OutputFormats       []string `json:"output_formats,omitempty"`
	PushSupported       bool     `json:"push_supported"`
	InjectionBoundaries []string `json:"injection_boundaries,omitempty"`
	RunnerCapabilities  []string `json:"runner_capabilities,omitempty"`
	AuthorizedSources   []string `json:"authorized_sources,omitempty"`
}

type frontierT6StoredAgentProfile struct {
	SchemaID      string               `json:"schema_id"`
	Version       int                  `json:"version"`
	ProfileID     string               `json:"profile_id"`
	Scope         frontierT6Scope      `json:"scope"`
	AgentID       string               `json:"agent_id"`
	Fields        map[string]any       `json:"fields"`
	Provenance    frontierT6Provenance `json:"provenance"`
	ProfileDigest string               `json:"profile_digest"`
	UpdatedAt     string               `json:"updated_at"`
}

type frontierT6ProfileResolutionRequest struct {
	Scope          frontierT6Scope               `json:"scope"`
	AgentID        string                        `json:"agent_id"`
	Stored         *frontierT6StoredAgentProfile `json:"stored,omitempty"`
	ExplicitFields map[string]any                `json:"explicit_fields,omitempty"`
	Capabilities   frontierT6AgentCapabilities   `json:"capabilities"`
	Now            time.Time                     `json:"-"`
}

type frontierT6ProfileResolution struct {
	SchemaID           string            `json:"schema_id"`
	Version            int               `json:"version"`
	Decision           string            `json:"decision"`
	AgentID            string            `json:"agent_id"`
	UnknownAgent       bool              `json:"unknown_agent"`
	ColdStart          bool              `json:"cold_start"`
	StoredProfileUsed  bool              `json:"stored_profile_used"`
	EffectiveProfile   map[string]any    `json:"effective_profile"`
	FieldSources       map[string]string `json:"field_sources"`
	ExplicitFields     []string          `json:"explicit_fields"`
	Adjustments        []string          `json:"adjustments"`
	Conflicts          []string          `json:"conflicts"`
	ProfileDigest      string            `json:"profile_digest"`
	RecommendedSurface string            `json:"recommended_surface"`
	AutomaticExecution bool              `json:"automatic_execution"`
}

type frontierT6ContextPrepApproval struct {
	Approved            bool   `json:"approved"`
	ApprovalID          string `json:"approval_id"`
	ScopeDigest         string `json:"scope_digest"`
	AuthorizationDigest string `json:"authorization_digest"`
	ExpiresAt           string `json:"expires_at"`
}

type frontierT6ContextPrepRequest struct {
	Scope                  frontierT6Scope               `json:"scope"`
	TaskID                 string                        `json:"task_id"`
	NextActionClass        string                        `json:"next_action_class"`
	PredictionConfidence   float64                       `json:"prediction_confidence"`
	MinimumConfidence      float64                       `json:"minimum_confidence"`
	EffectiveProfileDigest string                        `json:"effective_profile_digest"`
	SourceGeneration       string                        `json:"source_generation"`
	TTLSeconds             int                           `json:"ttl_seconds"`
	Approval               frontierT6ContextPrepApproval `json:"approval"`
	Provenance             frontierT6Provenance          `json:"provenance"`
}

type frontierT6ContextPrepEvidenceRef struct {
	SourceID            string `json:"source_id"`
	SourceGeneration    string `json:"source_generation"`
	ContentDigest       string `json:"content_digest"`
	AuthorizationDigest string `json:"authorization_digest"`
	FreshUntil          string `json:"fresh_until"`
}

type frontierT6ContextPrepArtifact struct {
	SchemaID               string                             `json:"schema_id"`
	Version                int                                `json:"version"`
	ArtifactID             string                             `json:"artifact_id"`
	ContextPackDigest      string                             `json:"context_pack_digest"`
	RetrievalReceiptDigest string                             `json:"retrieval_receipt_digest"`
	EffectiveProfileDigest string                             `json:"effective_profile_digest"`
	SourceGeneration       string                             `json:"source_generation"`
	AuthorizationDigest    string                             `json:"authorization_digest"`
	EvidenceRefs           []frontierT6ContextPrepEvidenceRef `json:"evidence_refs"`
	CreatedAt              string                             `json:"created_at"`
	ExpiresAt              string                             `json:"expires_at"`
}

type frontierT6ContextPrepRecord struct {
	SchemaID               string                         `json:"schema_id"`
	Version                int                            `json:"version"`
	PrepID                 string                         `json:"prep_id"`
	DedupeKey              string                         `json:"dedupe_key"`
	Scope                  frontierT6Scope                `json:"scope"`
	TaskID                 string                         `json:"task_id"`
	NextActionClass        string                         `json:"next_action_class"`
	PredictionConfidence   float64                        `json:"prediction_confidence"`
	EffectiveProfileDigest string                         `json:"effective_profile_digest"`
	SourceGeneration       string                         `json:"source_generation"`
	AuthorizationDigest    string                         `json:"authorization_digest"`
	ApprovalDigest         string                         `json:"approval_digest"`
	Provenance             frontierT6Provenance           `json:"provenance"`
	Status                 string                         `json:"status"`
	Attempts               int                            `json:"attempts"`
	WorkerDigest           string                         `json:"worker_digest,omitempty"`
	ClaimDigest            string                         `json:"claim_digest,omitempty"`
	LeaseExpiresAt         string                         `json:"lease_expires_at,omitempty"`
	NextAttemptAt          string                         `json:"next_attempt_at,omitempty"`
	LastReasonDigest       string                         `json:"last_reason_digest,omitempty"`
	Artifact               *frontierT6ContextPrepArtifact `json:"artifact,omitempty"`
	CreatedAt              string                         `json:"created_at"`
	UpdatedAt              string                         `json:"updated_at"`
	ExpiresAt              string                         `json:"expires_at"`
}

type frontierT6ContextPrepScheduleResult struct {
	Decision           string                       `json:"decision"`
	Reasons            []string                     `json:"reasons"`
	Deduplicated       bool                         `json:"deduplicated"`
	Prep               *frontierT6ContextPrepRecord `json:"prep,omitempty"`
	ExecutionOwner     string                       `json:"execution_owner"`
	ExecutionPerformed bool                         `json:"execution_performed"`
	NetworkCalls       int                          `json:"network_calls"`
}

type frontierT6ContextPrepClaim struct {
	Prep                      frontierT6ContextPrepRecord `json:"prep"`
	ClaimToken                string                      `json:"claim_token"`
	ExecutionOwner            string                      `json:"execution_owner"`
	GatewayExecutionPerformed bool                        `json:"gateway_execution_performed"`
}

type frontierT6ContextPrepUse struct {
	Eligible               bool                           `json:"eligible"`
	Reasons                []string                       `json:"reasons"`
	Artifact               *frontierT6ContextPrepArtifact `json:"artifact,omitempty"`
	InjectionPerformed     bool                           `json:"injection_performed"`
	RequiresExplicitCLIUse bool                           `json:"requires_explicit_cli_use"`
}

type frontierT6AgentFitState struct {
	SchemaID               string                                  `json:"schema_id"`
	Version                int                                     `json:"version"`
	SteeringEvents         []frontierT6SteeringEvent               `json:"steering_events"`
	SteeringAnchorSequence uint64                                  `json:"steering_anchor_sequence"`
	SteeringAnchorHash     string                                  `json:"steering_anchor_hash,omitempty"`
	LastSteeringSequence   uint64                                  `json:"last_steering_sequence"`
	SteeringDeliveries     map[string]frontierT6SteeringDelivery   `json:"steering_deliveries"`
	Profiles               map[string]frontierT6StoredAgentProfile `json:"profiles"`
	ContextPreps           map[string]frontierT6ContextPrepRecord  `json:"context_preps"`
	UpdatedAt              string                                  `json:"updated_at"`
	StateHash              string                                  `json:"state_hash"`
}

type frontierT6AgentFitStore struct {
	mu              sync.RWMutex
	enabled         bool
	path            string
	dedicatedParent bool
	limits          frontierT6StoreLimits
	state           frontierT6AgentFitState
	fileBytes       int64
	lastErrorCode   string
	unlock          func()
}

func frontierT6DefaultLimits() frontierT6StoreLimits {
	return frontierT6StoreLimits{
		MaxBytes: frontierT6DefaultMaxBytes, MaxEvents: frontierT6DefaultMaxEvents,
		MaxDeliveries: frontierT6DefaultMaxDeliveries, MaxProfiles: frontierT6DefaultMaxProfiles,
		MaxPreps: frontierT6DefaultMaxPreps,
	}
}

func frontierT6NormalizeLimits(limits frontierT6StoreLimits) frontierT6StoreLimits {
	defaults := frontierT6DefaultLimits()
	if limits.MaxBytes <= 0 {
		limits.MaxBytes = defaults.MaxBytes
	}
	if limits.MaxEvents <= 0 {
		limits.MaxEvents = defaults.MaxEvents
	}
	if limits.MaxDeliveries <= 0 {
		limits.MaxDeliveries = defaults.MaxDeliveries
	}
	if limits.MaxProfiles <= 0 {
		limits.MaxProfiles = defaults.MaxProfiles
	}
	if limits.MaxPreps <= 0 {
		limits.MaxPreps = defaults.MaxPreps
	}
	limits.MaxBytes = clampInt(limits.MaxBytes, 64*1024, frontierT6MaximumMaxBytes)
	limits.MaxEvents = clampInt(limits.MaxEvents, 8, frontierT6MaximumMaxEvents)
	limits.MaxDeliveries = clampInt(limits.MaxDeliveries, 8, frontierT6MaximumDeliveries)
	limits.MaxProfiles = clampInt(limits.MaxProfiles, 1, frontierT6MaximumProfiles)
	limits.MaxPreps = clampInt(limits.MaxPreps, 1, frontierT6MaximumPreps)
	return limits
}

func emptyFrontierT6AgentFitState() frontierT6AgentFitState {
	return frontierT6AgentFitState{
		SchemaID: frontierT6StateSchemaID, Version: 1,
		SteeringEvents:     []frontierT6SteeringEvent{},
		SteeringDeliveries: map[string]frontierT6SteeringDelivery{},
		Profiles:           map[string]frontierT6StoredAgentProfile{},
		ContextPreps:       map[string]frontierT6ContextPrepRecord{},
	}
}

func frontierT6StatePath() string {
	return resolveStoragePath(frontierT6StatePathEnv, frontierT6DefaultStatePath)
}

func newFrontierT6AgentFitStore(path string, limits frontierT6StoreLimits) (*frontierT6AgentFitStore, error) {
	return newFrontierT6AgentFitStoreWithParent(path, limits, false)
}

func newFrontierT6AgentFitStoreWithParent(path string, limits frontierT6StoreLimits, dedicatedParent bool) (*frontierT6AgentFitStore, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return nil, errors.New("Frontier T6 state path is required")
	}
	store := &frontierT6AgentFitStore{
		enabled: true, path: path, dedicatedParent: dedicatedParent,
		limits: frontierT6NormalizeLimits(limits), state: emptyFrontierT6AgentFitState(),
	}
	if err := prepareOwnerOnlyFile(store.path, dedicatedParent); err != nil {
		return nil, fmt.Errorf("prepare Frontier T6 state: %w", err)
	}
	unlock, err := lockOwnerOnlyMigration(store.path + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock Frontier T6 state: %w", err)
	}
	store.unlock = unlock
	if err := store.load(); err != nil {
		store.close()
		return nil, err
	}
	return store, nil
}

func newFrontierT6AgentFitStoreFromEnv() (*frontierT6AgentFitStore, error) {
	if !envBool(frontierT6EnabledEnv, true) {
		return &frontierT6AgentFitStore{enabled: false, state: emptyFrontierT6AgentFitState(), limits: frontierT6DefaultLimits()}, nil
	}
	limits := frontierT6StoreLimits{
		MaxBytes:      envInt("CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_MAX_BYTES", frontierT6DefaultMaxBytes),
		MaxEvents:     envInt("CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_MAX_EVENTS", frontierT6DefaultMaxEvents),
		MaxDeliveries: envInt("CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_MAX_DELIVERIES", frontierT6DefaultMaxDeliveries),
		MaxProfiles:   envInt("CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_MAX_PROFILES", frontierT6DefaultMaxProfiles),
		MaxPreps:      envInt("CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_MAX_PREPS", frontierT6DefaultMaxPreps),
	}
	dedicatedParent := strings.TrimSpace(os.Getenv(frontierT6StatePathEnv)) == ""
	return newFrontierT6AgentFitStoreWithParent(frontierT6StatePath(), limits, dedicatedParent)
}

func (s *frontierT6AgentFitStore) close() {
	if s != nil && s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
}

func frontierT6Digest(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func frontierT6OpaqueDigest(label, value string) string {
	sum := sha256.Sum256([]byte(label + "\x00" + strings.TrimSpace(value)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func frontierT6ValidDigest(value string) bool {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func frontierT6StateHash(state frontierT6AgentFitState) string {
	state.StateHash = ""
	return frontierT6Digest(state)
}

func frontierT6SteeringEventHash(event frontierT6SteeringEvent) string {
	event.EventHash = ""
	return frontierT6Digest(event)
}

func (s *frontierT6AgentFitStore) load() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s.saveLocked(time.Now().UTC())
	}
	if err != nil {
		return fmt.Errorf("stat Frontier T6 state: %w", err)
	}
	if info.Size() == 0 {
		return s.saveLocked(time.Now().UTC())
	}
	if info.Size() > int64(s.limits.MaxBytes) {
		return errors.New("Frontier T6 state exceeds the bounded maximum")
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("read Frontier T6 state: %w", err)
	}
	state := emptyFrontierT6AgentFitState()
	if err := json.Unmarshal(raw, &state); err != nil {
		return fmt.Errorf("decode Frontier T6 state: %w", err)
	}
	if err := frontierT6ValidateState(state, s.limits); err != nil {
		return err
	}
	s.state = state
	s.fileBytes = info.Size()
	return nil
}

func frontierT6ValidateState(state frontierT6AgentFitState, limits frontierT6StoreLimits) error {
	if state.SchemaID != frontierT6StateSchemaID || state.Version != 1 || state.SteeringDeliveries == nil || state.Profiles == nil || state.ContextPreps == nil {
		return errors.New("Frontier T6 state schema is invalid")
	}
	if len(state.SteeringEvents) > limits.MaxEvents || len(state.SteeringDeliveries) > limits.MaxDeliveries || len(state.Profiles) > limits.MaxProfiles || len(state.ContextPreps) > limits.MaxPreps {
		return errors.New("Frontier T6 state exceeds configured entry bounds")
	}
	previous := state.SteeringAnchorHash
	sequence := state.SteeringAnchorSequence
	for _, event := range state.SteeringEvents {
		if event.SchemaID != frontierT6SteeringEventSchemaID || event.Version != 1 || event.Sequence <= sequence || event.PreviousHash != previous || event.EventID == "" || event.Cursor != frontierT6Cursor(event.Sequence) || frontierT6SteeringEventHash(event) != event.EventHash {
			return errors.New("Frontier T6 steering event chain is invalid")
		}
		sequence, previous = event.Sequence, event.EventHash
	}
	if sequence > state.LastSteeringSequence || frontierT6StateHash(state) != state.StateHash {
		return errors.New("Frontier T6 state integrity is invalid")
	}
	return nil
}

func (s *frontierT6AgentFitStore) saveLocked(now time.Time) error {
	s.trimLocked(now)
	s.state.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	s.state.StateHash = frontierT6StateHash(s.state)
	raw, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	if len(raw)+1 > s.limits.MaxBytes {
		return errors.New("Frontier T6 state exceeds the bounded maximum")
	}
	content := append(raw, '\n')
	if err := writeOwnerOnlyDurableAtomicFile(s.path, content, s.dedicatedParent); err != nil {
		return err
	}
	s.fileBytes = int64(len(content))
	s.lastErrorCode = ""
	return nil
}

func (s *frontierT6AgentFitStore) mutate(now time.Time, apply func() error) error {
	if s == nil || !s.enabled {
		return errors.New("Frontier T6 Agent Fit store is disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	if err := apply(); err != nil {
		return err
	}
	if err := s.saveLocked(now); err != nil {
		if ownerOnlyAtomicWriteCommitted(err) {
			s.enabled = false
			s.lastErrorCode = "commit_unknown"
			return fmt.Errorf("Frontier T6 commit outcome is unknown: %w", err)
		}
		_ = json.Unmarshal(before, &s.state)
		s.lastErrorCode = "storage_unavailable"
		return err
	}
	return nil
}

func (s *frontierT6AgentFitStore) trimLocked(now time.Time) {
	for len(s.state.SteeringEvents) > 0 {
		first := s.state.SteeringEvents[0]
		expiresAt, ok := frontierT6ParseTime(first.ExpiresAt)
		if len(s.state.SteeringEvents) <= s.limits.MaxEvents && (!ok || now.Before(expiresAt)) {
			break
		}
		s.state.SteeringAnchorSequence = first.Sequence
		s.state.SteeringAnchorHash = first.EventHash
		s.state.SteeringEvents = s.state.SteeringEvents[1:]
	}
	retainedEvents := map[string]struct{}{}
	for _, event := range s.state.SteeringEvents {
		retainedEvents[event.EventID] = struct{}{}
	}
	for key, delivery := range s.state.SteeringDeliveries {
		if _, ok := retainedEvents[delivery.EventID]; !ok {
			delete(s.state.SteeringDeliveries, key)
		}
	}
	frontierT6TrimTerminalDeliveries(s.state.SteeringDeliveries, s.limits.MaxDeliveries)
	for key, prep := range s.state.ContextPreps {
		expiresAt, ok := frontierT6ParseTime(prep.ExpiresAt)
		if ok && !now.Before(expiresAt) && prep.Status != "expired" {
			prep.Status = "expired"
			prep.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
			s.state.ContextPreps[key] = prep
		}
	}
	frontierT6TrimTerminalPreps(s.state.ContextPreps, s.limits.MaxPreps)
}

func frontierT6TrimTerminalDeliveries(deliveries map[string]frontierT6SteeringDelivery, limit int) {
	if len(deliveries) <= limit {
		return
	}
	keys := make([]string, 0, len(deliveries))
	for key, delivery := range deliveries {
		if delivery.Status == "acknowledged" || delivery.Status == "exhausted" || delivery.Status == "revoked" {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := deliveries[keys[i]], deliveries[keys[j]]
		if left.UpdatedAt == right.UpdatedAt {
			return keys[i] < keys[j]
		}
		return left.UpdatedAt < right.UpdatedAt
	})
	for _, key := range keys {
		if len(deliveries) <= limit {
			break
		}
		delete(deliveries, key)
	}
}

func frontierT6TrimTerminalPreps(preps map[string]frontierT6ContextPrepRecord, limit int) {
	if len(preps) <= limit {
		return
	}
	keys := make([]string, 0, len(preps))
	for key, prep := range preps {
		switch prep.Status {
		case "consumed", "failed", "expired", "canceled":
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		left, right := preps[keys[i]], preps[keys[j]]
		if left.UpdatedAt == right.UpdatedAt {
			return keys[i] < keys[j]
		}
		return left.UpdatedAt < right.UpdatedAt
	})
	for _, key := range keys {
		if len(preps) <= limit {
			break
		}
		delete(preps, key)
	}
}

func frontierT6NormalizeID(value, field string, maxLen int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLen || strings.Contains(value, "..") || strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("%s must be a bounded machine-safe identifier", field)
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._:-/@", r) {
			continue
		}
		return "", fmt.Errorf("%s must be a bounded machine-safe identifier", field)
	}
	return value, nil
}

func frontierT6NormalizeScope(scope frontierT6Scope, requireSession bool) (frontierT6Scope, error) {
	var err error
	if scope.WorkspaceID, err = frontierT6NormalizeID(scope.WorkspaceID, "workspace_id", 160); err != nil {
		return frontierT6Scope{}, err
	}
	if scope.Project, err = frontierT6NormalizeID(scope.Project, "project", 160); err != nil {
		return frontierT6Scope{}, err
	}
	if requireSession {
		if scope.SessionID, err = frontierT6NormalizeID(scope.SessionID, "session_id", 192); err != nil {
			return frontierT6Scope{}, err
		}
	}
	if scope.SessionID != "" && !requireSession {
		if scope.SessionID, err = frontierT6NormalizeID(scope.SessionID, "session_id", 192); err != nil {
			return frontierT6Scope{}, err
		}
	}
	if scope.AgentID != "" {
		if scope.AgentID, err = frontierT6NormalizeID(scope.AgentID, "agent_id", 160); err != nil {
			return frontierT6Scope{}, err
		}
	}
	return scope, nil
}

func frontierT6ScopeDigest(scope frontierT6Scope) string {
	return frontierT6Digest(scope)
}

func frontierT6ParseTime(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	return parsed.UTC(), err == nil
}

func frontierT6ValidateProvenance(provenance frontierT6Provenance, now, mustCover time.Time) error {
	if _, err := frontierT6NormalizeID(provenance.Source, "provenance.source", 80); err != nil {
		return err
	}
	if _, err := frontierT6NormalizeID(provenance.SourceID, "provenance.source_id", 192); err != nil {
		return err
	}
	if _, err := frontierT6NormalizeID(provenance.SourceGeneration, "provenance.source_generation", 192); err != nil {
		return err
	}
	if !frontierT6ValidDigest(provenance.ContentDigest) || !frontierT6ValidDigest(provenance.AuthorizationDigest) {
		return errors.New("provenance requires content and authorization SHA-256 digests")
	}
	observedAt, observedOK := frontierT6ParseTime(provenance.ObservedAt)
	freshUntil, freshOK := frontierT6ParseTime(provenance.FreshUntil)
	if !observedOK || !freshOK || freshUntil.Before(observedAt) || observedAt.After(now.Add(5*time.Minute)) || !now.Before(freshUntil) {
		return errors.New("provenance freshness window is invalid or stale")
	}
	if !mustCover.IsZero() && freshUntil.Before(mustCover) {
		return errors.New("provenance does not cover the requested lifetime")
	}
	return nil
}

func frontierT6CanonicalProvenance(provenance frontierT6Provenance) frontierT6Provenance {
	provenance.Source = strings.TrimSpace(provenance.Source)
	provenance.SourceID = strings.TrimSpace(provenance.SourceID)
	provenance.SourceGeneration = strings.TrimSpace(provenance.SourceGeneration)
	provenance.ContentDigest = strings.ToLower(strings.TrimSpace(provenance.ContentDigest))
	provenance.AuthorizationDigest = strings.ToLower(strings.TrimSpace(provenance.AuthorizationDigest))
	if parsed, ok := frontierT6ParseTime(provenance.ObservedAt); ok {
		provenance.ObservedAt = parsed.Format(time.RFC3339Nano)
	}
	if parsed, ok := frontierT6ParseTime(provenance.FreshUntil); ok {
		provenance.FreshUntil = parsed.Format(time.RFC3339Nano)
	}
	return provenance
}

func frontierT6SafeLine(value, field string, maxLen int, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if len(value) > maxLen || strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("%s must be a bounded single line", field)
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"-----begin private key", "authorization: bearer", "x-api-key:", "ghp_", "sk-proj-", "/users/", "/volumes/", `c:\users\`} {
		if strings.Contains(lower, marker) {
			return "", fmt.Errorf("%s contains disallowed secret or private-path material", field)
		}
	}
	return value, nil
}

func frontierT6SafeBoundary(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "-", "_")))
	if value == "" {
		value = "after_tool"
	}
	switch value {
	case "after_tool", "before_model_call", "idle":
		return value, nil
	default:
		return "", errors.New("injection_boundary must be after_tool, before_model_call, or idle")
	}
}

func frontierT6Cursor(sequence uint64) string {
	return "ft6c_" + strconv.FormatUint(sequence, 10)
}

func frontierT6ParseCursor(cursor string) (uint64, error) {
	cursor = strings.TrimSpace(cursor)
	if cursor == "" {
		return 0, nil
	}
	if !strings.HasPrefix(cursor, "ft6c_") {
		return 0, errors.New("Frontier T6 cursor is invalid")
	}
	sequence, err := strconv.ParseUint(strings.TrimPrefix(cursor, "ft6c_"), 10, 64)
	if err != nil {
		return 0, errors.New("Frontier T6 cursor is invalid")
	}
	return sequence, nil
}

func (s *frontierT6AgentFitStore) publishSteering(request frontierT6SteeringPublishRequest, now time.Time) (frontierT6SteeringEvent, bool, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	scope, err := frontierT6NormalizeScope(request.Scope, true)
	if err != nil {
		return frontierT6SteeringEvent{}, false, err
	}
	kind, err := frontierT6NormalizeID(request.Kind, "kind", 80)
	if err != nil {
		return frontierT6SteeringEvent{}, false, err
	}
	message, err := frontierT6SafeLine(request.Message, "message", 720, true)
	if err != nil {
		return frontierT6SteeringEvent{}, false, err
	}
	suggestedAction, err := frontierT6SafeLine(request.SuggestedAction, "suggested_action", 480, false)
	if err != nil {
		return frontierT6SteeringEvent{}, false, err
	}
	boundary, err := frontierT6SafeBoundary(request.InjectionBoundary)
	if err != nil {
		return frontierT6SteeringEvent{}, false, err
	}
	if request.DedupeKey != "" {
		if _, err := frontierT6NormalizeID(request.DedupeKey, "dedupe_key", 160); err != nil {
			return frontierT6SteeringEvent{}, false, err
		}
	}
	ttl := clampInt(request.TTLSeconds, 30, 24*60*60)
	if request.TTLSeconds <= 0 {
		ttl = 15 * 60
	}
	expiresAt := now.Add(time.Duration(ttl) * time.Second)
	request.Provenance = frontierT6CanonicalProvenance(request.Provenance)
	if err := frontierT6ValidateProvenance(request.Provenance, now, expiresAt); err != nil {
		return frontierT6SteeringEvent{}, false, err
	}
	fingerprint := frontierT6Digest(map[string]any{
		"scope": scope, "kind": kind, "message": message, "suggested_action": suggestedAction,
		"boundary": boundary, "dedupe_key": request.DedupeKey, "provenance": request.Provenance,
	})
	var result frontierT6SteeringEvent
	deduplicated := false
	err = s.mutate(now, func() error {
		for i := len(s.state.SteeringEvents) - 1; i >= 0; i-- {
			event := s.state.SteeringEvents[i]
			expires, ok := frontierT6ParseTime(event.ExpiresAt)
			if event.Fingerprint == fingerprint && ok && now.Before(expires) {
				result, deduplicated = event, true
				return nil
			}
		}
		s.state.LastSteeringSequence++
		sequence := s.state.LastSteeringSequence
		previous := s.state.SteeringAnchorHash
		if len(s.state.SteeringEvents) > 0 {
			previous = s.state.SteeringEvents[len(s.state.SteeringEvents)-1].EventHash
		}
		result = frontierT6SteeringEvent{
			SchemaID: frontierT6SteeringEventSchemaID, Version: 1, Sequence: sequence,
			Cursor: frontierT6Cursor(sequence), Scope: scope, Kind: kind, Message: message,
			SuggestedAction: suggestedAction, InjectionBoundary: boundary, Fingerprint: fingerprint,
			Provenance: request.Provenance, CreatedAt: now.UTC().Format(time.RFC3339Nano),
			ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano), PreviousHash: previous,
		}
		result.EventID = "ft6e_" + strings.TrimPrefix(frontierT6Digest(map[string]any{"sequence": sequence, "fingerprint": fingerprint, "created_at": result.CreatedAt}), "sha256:")[:24]
		result.EventHash = frontierT6SteeringEventHash(result)
		s.state.SteeringEvents = append(s.state.SteeringEvents, result)
		return nil
	})
	return result, deduplicated, err
}

func frontierT6ScopeEqual(left, right frontierT6Scope) bool {
	return left.WorkspaceID == right.WorkspaceID && left.Project == right.Project && left.SessionID == right.SessionID && left.AgentID == right.AgentID
}

func (s *frontierT6AgentFitStore) replaySteering(scope frontierT6Scope, cursor string, now time.Time, limit int) (frontierT6SteeringBatch, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	normalized, err := frontierT6NormalizeScope(scope, true)
	if err != nil {
		return frontierT6SteeringBatch{}, err
	}
	sequence, err := frontierT6ParseCursor(cursor)
	if err != nil {
		return frontierT6SteeringBatch{}, err
	}
	limit = clampInt(limit, 1, 128)
	batch := frontierT6SteeringBatch{
		SchemaID: frontierT6AgentFitContractID, DeliveryMode: "bounded_pull_replay",
		RequestedCursor: cursor, Events: []frontierT6SteeringEvent{}, NetworkCalls: 0,
		ExecutionPerformed: false, RecommendedSurface: "primary_cli",
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	batch.ReplayFloor = frontierT6Cursor(s.state.SteeringAnchorSequence)
	batch.NextCursor = frontierT6Cursor(sequence)
	if cursor != "" && sequence < s.state.SteeringAnchorSequence {
		batch.CursorExpired = true
		batch.FallbackReason = "bounded_replay_cursor_expired"
		return batch, errFrontierT6CursorExpired
	}
	if sequence > s.state.LastSteeringSequence {
		return batch, errors.New("Frontier T6 cursor is ahead of the durable stream")
	}
	for _, event := range s.state.SteeringEvents {
		if event.Sequence <= sequence || !frontierT6ScopeEqual(event.Scope, normalized) {
			continue
		}
		expiresAt, ok := frontierT6ParseTime(event.ExpiresAt)
		if !ok || !now.Before(expiresAt) {
			continue
		}
		batch.Events = append(batch.Events, event)
		batch.NextCursor = event.Cursor
		if len(batch.Events) >= limit {
			break
		}
	}
	return batch, nil
}

func frontierT6NormalizeCapabilities(capabilities frontierT6HarnessCapabilities, boundary string) (frontierT6HarnessCapabilities, string, []string) {
	reasons := []string{}
	capabilities.HarnessID = strings.ToLower(strings.TrimSpace(capabilities.HarnessID))
	capabilities.Transport = strings.ToLower(strings.TrimSpace(capabilities.Transport))
	capabilities.InjectionBoundaries = frontierT6NormalizeStringList(capabilities.InjectionBoundaries, 8)
	if capabilities.HarnessID == "" {
		reasons = append(reasons, "harness_identity_missing")
	} else if _, err := frontierT6NormalizeID(capabilities.HarnessID, "harness_id", 160); err != nil {
		reasons = append(reasons, "harness_identity_invalid")
	}
	if capabilities.Transport != "sse" || !capabilities.SupportsSSE {
		reasons = append(reasons, "sse_transport_unsupported")
	}
	if !capabilities.SupportsEventIDs {
		reasons = append(reasons, "event_ids_unsupported")
	}
	if !capabilities.SupportsResume {
		reasons = append(reasons, "resume_cursor_unsupported")
	}
	if !capabilities.SupportsAck {
		reasons = append(reasons, "delivery_ack_unsupported")
	}
	boundary, _ = frontierT6SafeBoundary(boundary)
	if !frontierT6Contains(capabilities.InjectionBoundaries, boundary) {
		reasons = append(reasons, "safe_injection_boundary_unsupported")
	}
	if capabilities.MaxEventBytes > 0 && capabilities.MaxEventBytes < 1024 {
		reasons = append(reasons, "event_size_capability_too_small")
	}
	return capabilities, boundary, frontierT6UniqueStrings(reasons)
}

func frontierT6DeliveryKey(scopeDigest, subscriberDigest, eventID string) string {
	return frontierT6Digest(map[string]any{"scope": scopeDigest, "subscriber": subscriberDigest, "event": eventID})
}

func (s *frontierT6AgentFitStore) claimSteering(scope frontierT6Scope, subscriberID string, capabilities frontierT6HarnessCapabilities, cursor string, now time.Time, limit int) (frontierT6SteeringBatch, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if _, err := frontierT6NormalizeID(subscriberID, "subscriber_id", 192); err != nil {
		return frontierT6SteeringBatch{}, err
	}
	batch, replayErr := s.replaySteering(scope, cursor, now, limit)
	if replayErr != nil {
		return batch, replayErr
	}
	firstBoundary := "after_tool"
	if len(batch.Events) > 0 {
		firstBoundary = batch.Events[0].InjectionBoundary
	}
	capabilities, _, reasons := frontierT6NormalizeCapabilities(capabilities, firstBoundary)
	for index, event := range batch.Events {
		if index > 0 {
			_, _, eventReasons := frontierT6NormalizeCapabilities(capabilities, event.InjectionBoundary)
			reasons = append(reasons, eventReasons...)
		}
		if capabilities.MaxEventBytes > 0 {
			raw, _ := json.Marshal(event)
			if len(raw) > capabilities.MaxEventBytes {
				reasons = append(reasons, "event_exceeds_harness_size_capability")
			}
		}
	}
	reasons = frontierT6UniqueStrings(reasons)
	if len(reasons) > 0 {
		batch.FallbackReason = strings.Join(reasons, ",")
		batch.DeliveryMode = "bounded_pull_replay"
		batch.PushNative = false
		return batch, nil
	}
	normalizedScope, _ := frontierT6NormalizeScope(scope, true)
	scopeDigest := frontierT6ScopeDigest(normalizedScope)
	subscriberDigest := frontierT6OpaqueDigest("frontier-t6-subscriber", subscriberID)
	deliveries := []frontierT6PushItem{}
	err := s.mutate(now, func() error {
		frontierT6TrimTerminalDeliveries(s.state.SteeringDeliveries, s.limits.MaxDeliveries-1)
		for _, event := range batch.Events {
			boundary := event.InjectionBoundary
			key := frontierT6DeliveryKey(scopeDigest, subscriberDigest, event.EventID)
			delivery, exists := s.state.SteeringDeliveries[key]
			if exists && (delivery.Status == "acknowledged" || delivery.Status == "revoked" || delivery.Status == "exhausted") {
				continue
			}
			if exists {
				nextAttempt, nextOK := frontierT6ParseTime(delivery.NextAttemptAt)
				leaseExpires, leaseOK := frontierT6ParseTime(delivery.LeaseExpiresAt)
				if delivery.Status == "inflight" && leaseOK && now.Before(leaseExpires) {
					continue
				}
				if nextOK && now.Before(nextAttempt) {
					continue
				}
			} else if len(s.state.SteeringDeliveries) >= s.limits.MaxDeliveries {
				return errors.New("Frontier T6 delivery store is full with unacknowledged deliveries")
			}
			if delivery.Attempts >= frontierT6SteeringMaxAttempts {
				delivery.Status = "exhausted"
				delivery.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
				s.state.SteeringDeliveries[key] = delivery
				continue
			}
			if !exists {
				delivery = frontierT6SteeringDelivery{
					SchemaID: frontierT6SteeringDeliverySchemaID, Version: 1,
					DeliveryID: "ft6d_" + strings.TrimPrefix(key, "sha256:")[:24], EventID: event.EventID,
					EventSequence: event.Sequence, ScopeDigest: scopeDigest, SubscriberDigest: subscriberDigest,
					HarnessID: capabilities.HarnessID, Boundary: boundary,
					CreatedAt: now.UTC().Format(time.RFC3339Nano),
				}
			}
			delivery.Attempts++
			delivery.Status = "inflight"
			delivery.HarnessID = capabilities.HarnessID
			delivery.Boundary = boundary
			delivery.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
			delivery.LeaseExpiresAt = now.Add(30 * time.Second).UTC().Format(time.RFC3339Nano)
			delivery.NextAttemptAt = delivery.LeaseExpiresAt
			claimToken := "ft6claim_" + strings.TrimPrefix(frontierT6Digest(map[string]any{"delivery": delivery.DeliveryID, "attempt": delivery.Attempts, "lease": delivery.LeaseExpiresAt}), "sha256:")[:32]
			delivery.ClaimDigest = frontierT6OpaqueDigest("frontier-t6-delivery-claim", claimToken)
			s.state.SteeringDeliveries[key] = delivery
			deliveries = append(deliveries, frontierT6PushItem{DeliveryID: delivery.DeliveryID, ClaimToken: claimToken, Event: event})
		}
		return nil
	})
	if err != nil {
		return frontierT6SteeringBatch{}, err
	}
	batch.DeliveryMode = "sse_push_adapter_claim"
	batch.PushNative = true
	batch.Events = nil
	batch.Deliveries = deliveries
	return batch, nil
}

func (s *frontierT6AgentFitStore) findDeliveryLocked(deliveryID string) (string, frontierT6SteeringDelivery, bool) {
	for key, delivery := range s.state.SteeringDeliveries {
		if delivery.DeliveryID == deliveryID {
			return key, delivery, true
		}
	}
	return "", frontierT6SteeringDelivery{}, false
}

func frontierT6ClaimMatches(claimToken, claimDigest string) bool {
	return claimToken != "" && claimDigest == frontierT6OpaqueDigest("frontier-t6-delivery-claim", claimToken)
}

func (s *frontierT6AgentFitStore) recordSteeringDelivered(deliveryID, claimToken string, now time.Time) error {
	return s.mutate(now, func() error {
		key, delivery, exists := s.findDeliveryLocked(deliveryID)
		if !exists {
			return errors.New("Frontier T6 delivery was not found")
		}
		if delivery.Status == "acknowledged" {
			return nil
		}
		leaseExpires, leaseOK := frontierT6ParseTime(delivery.LeaseExpiresAt)
		if delivery.Status != "inflight" || !frontierT6ClaimMatches(claimToken, delivery.ClaimDigest) || !leaseOK || !now.Before(leaseExpires) {
			return errFrontierT6ClaimStale
		}
		delivery.Status = "delivered_unacknowledged"
		delivery.DeliveredAt = now.UTC().Format(time.RFC3339Nano)
		delivery.UpdatedAt = delivery.DeliveredAt
		delivery.ClaimDigest = ""
		delivery.LeaseExpiresAt = ""
		delivery.NextAttemptAt = now.Add(frontierT6SteeringRetryDelay(delivery.Attempts)).UTC().Format(time.RFC3339Nano)
		s.state.SteeringDeliveries[key] = delivery
		return nil
	})
}

func frontierT6SteeringRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 6 {
		attempt = 6
	}
	return time.Duration(1<<(attempt-1)) * 2 * time.Second
}

func (s *frontierT6AgentFitStore) failSteeringDelivery(deliveryID, claimToken, reasonCode string, now time.Time) error {
	reasonCode, err := frontierT6NormalizeID(reasonCode, "reason_code", 80)
	if err != nil {
		return err
	}
	return s.mutate(now, func() error {
		key, delivery, exists := s.findDeliveryLocked(deliveryID)
		if !exists {
			return errors.New("Frontier T6 delivery was not found")
		}
		leaseExpires, leaseOK := frontierT6ParseTime(delivery.LeaseExpiresAt)
		if delivery.Status != "inflight" || !frontierT6ClaimMatches(claimToken, delivery.ClaimDigest) || !leaseOK || !now.Before(leaseExpires) {
			return errFrontierT6ClaimStale
		}
		delivery.ClaimDigest = ""
		delivery.LeaseExpiresAt = ""
		delivery.LastReasonDigest = frontierT6OpaqueDigest("frontier-t6-delivery-reason", reasonCode)
		delivery.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		if delivery.Attempts >= frontierT6SteeringMaxAttempts {
			delivery.Status = "exhausted"
			delivery.NextAttemptAt = ""
		} else {
			delivery.Status = "retry_pending"
			delivery.NextAttemptAt = now.Add(frontierT6SteeringRetryDelay(delivery.Attempts)).UTC().Format(time.RFC3339Nano)
		}
		s.state.SteeringDeliveries[key] = delivery
		return nil
	})
}

func (s *frontierT6AgentFitStore) acknowledgeSteering(scope frontierT6Scope, subscriberID, deliveryID, eventID string, now time.Time) (string, error) {
	normalized, err := frontierT6NormalizeScope(scope, true)
	if err != nil {
		return "", err
	}
	if _, err := frontierT6NormalizeID(subscriberID, "subscriber_id", 192); err != nil {
		return "", err
	}
	scopeDigest := frontierT6ScopeDigest(normalized)
	subscriberDigest := frontierT6OpaqueDigest("frontier-t6-subscriber", subscriberID)
	ackCursor := ""
	err = s.mutate(now, func() error {
		key, delivery, exists := s.findDeliveryLocked(deliveryID)
		if !exists {
			return errors.New("Frontier T6 delivery was not found")
		}
		if delivery.ScopeDigest != scopeDigest || delivery.SubscriberDigest != subscriberDigest || delivery.EventID != eventID {
			return errors.New("Frontier T6 acknowledgement scope does not match the delivery")
		}
		if delivery.Status == "acknowledged" {
			ackCursor = delivery.AckCursor
			return nil
		}
		if delivery.Status != "delivered_unacknowledged" && delivery.Status != "inflight" {
			return errors.New("Frontier T6 delivery is not acknowledgement-eligible")
		}
		delivery.Status = "acknowledged"
		delivery.AcknowledgedAt = now.UTC().Format(time.RFC3339Nano)
		delivery.UpdatedAt = delivery.AcknowledgedAt
		delivery.ClaimDigest, delivery.LeaseExpiresAt, delivery.NextAttemptAt = "", "", ""
		delivery.AckCursor = frontierT6Cursor(delivery.EventSequence)
		ackCursor = delivery.AckCursor
		s.state.SteeringDeliveries[key] = delivery
		return nil
	})
	return ackCursor, err
}

func frontierT6AdviseRunnerSelection(request frontierT6SelectionRequest) (frontierT6SelectionReceipt, error) {
	return frontierT6AdviseSelection("runner", frontierT6RunnerSelectionSchemaID, request)
}

func frontierT6AdviseModelSelection(request frontierT6SelectionRequest) (frontierT6SelectionReceipt, error) {
	return frontierT6AdviseSelection("model", frontierT6ModelSelectionSchemaID, request)
}

func frontierT6AdviseSelection(kind, schemaID string, request frontierT6SelectionRequest) (frontierT6SelectionReceipt, error) {
	now := request.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	taskClass := normalizeRunnerQualityTaskClass(request.TaskClass)
	if taskClass == "" {
		return frontierT6SelectionReceipt{}, errors.New("task_class is required")
	}
	constraints := request.Constraints
	if constraints.MinimumSamples <= 0 {
		constraints.MinimumSamples = frontierT6SelectionSampleFloor
	}
	if constraints.MinimumSamples > 100000 || constraints.MinimumContextTokens < 0 || constraints.MaximumCostMicrosPer1K < 0 || constraints.MaximumLatencyMillis < 0 {
		return frontierT6SelectionReceipt{}, errors.New("selection constraints are outside bounded ranges")
	}
	constraints.RequiredCapabilities = frontierT6NormalizeStringList(constraints.RequiredCapabilities, 32)
	constraints.AllowedCandidateIDs = frontierT6NormalizeStringList(constraints.AllowedCandidateIDs, 128)
	seen := map[string]struct{}{}
	rows := make([]frontierT6SelectionCandidateReceipt, 0, len(request.Candidates))
	for _, candidate := range request.Candidates {
		candidateID, err := frontierT6NormalizeID(strings.ToLower(candidate.CandidateID), "candidate_id", 160)
		if err != nil {
			return frontierT6SelectionReceipt{}, err
		}
		if _, duplicate := seen[candidateID]; duplicate {
			return frontierT6SelectionReceipt{}, errors.New("selection candidate IDs must be unique")
		}
		seen[candidateID] = struct{}{}
		reasons := []string{}
		evidence := candidate.Evidence
		evidence.Provenance = frontierT6CanonicalProvenance(evidence.Provenance)
		if strings.ToLower(strings.TrimSpace(candidate.Readiness)) != "ready" {
			reasons = append(reasons, "candidate_not_ready")
		}
		if len(constraints.AllowedCandidateIDs) > 0 && !frontierT6Contains(constraints.AllowedCandidateIDs, candidateID) {
			reasons = append(reasons, "candidate_not_explicitly_allowed")
		}
		capabilities := frontierT6NormalizeStringList(candidate.Capabilities, 64)
		for _, required := range constraints.RequiredCapabilities {
			if !frontierT6Contains(capabilities, required) {
				reasons = append(reasons, "required_capability_missing:"+required)
			}
		}
		if candidate.ContextWindowTokens <= 0 || candidate.ContextWindowTokens < constraints.MinimumContextTokens {
			reasons = append(reasons, "context_window_constraint_not_met")
		}
		if !evidence.Verified {
			reasons = append(reasons, "task_evidence_unverified")
		}
		if normalizeRunnerQualityTaskClass(evidence.TaskClass) != taskClass {
			reasons = append(reasons, "task_class_evidence_mismatch")
		}
		if evidence.SampleCount < constraints.MinimumSamples {
			reasons = append(reasons, "minimum_sample_floor_not_met")
		}
		if evidence.SampleCount < 0 || evidence.SuccessCount < 0 || evidence.FailureCount < 0 || evidence.BlockedCount < 0 || evidence.SuccessCount+evidence.FailureCount+evidence.BlockedCount > evidence.SampleCount {
			reasons = append(reasons, "sample_accounting_invalid")
		}
		if math.IsNaN(evidence.QualityScore) || math.IsInf(evidence.QualityScore, 0) || evidence.QualityScore < 0 || evidence.QualityScore > 100 || math.IsNaN(evidence.Confidence) || math.IsInf(evidence.Confidence, 0) || evidence.Confidence < 0 || evidence.Confidence > 1 {
			reasons = append(reasons, "quality_or_confidence_invalid")
		}
		if !evidence.CostKnown || evidence.CostMicrosPer1K < 0 {
			reasons = append(reasons, "cost_evidence_missing")
		} else if constraints.MaximumCostMicrosPer1K > 0 && evidence.CostMicrosPer1K > constraints.MaximumCostMicrosPer1K {
			reasons = append(reasons, "cost_constraint_exceeded")
		}
		if !evidence.LatencyKnown || evidence.LatencyMillisP50 < 0 {
			reasons = append(reasons, "latency_evidence_missing")
		} else if constraints.MaximumLatencyMillis > 0 && evidence.LatencyMillisP50 > constraints.MaximumLatencyMillis {
			reasons = append(reasons, "latency_constraint_exceeded")
		}
		if err := frontierT6ValidateProvenance(evidence.Provenance, now, time.Time{}); err != nil {
			reasons = append(reasons, "evidence_stale_or_provenance_invalid")
		}
		reasons = frontierT6UniqueStrings(reasons)
		total := maxInt(evidence.SampleCount, 1)
		successBP := evidence.SuccessCount * 10000 / total
		failureBP := evidence.FailureCount * 10000 / total
		blockedBP := evidence.BlockedCount * 10000 / total
		score := int(math.Round(evidence.QualityScore*35)) + successBP*35/100 + int(math.Round(evidence.Confidence*2000)) - failureBP*20/100 - blockedBP*10/100
		if evidence.LatencyKnown {
			score -= int(frontierT6MinInt64(evidence.LatencyMillisP50, 60000) / 20)
		}
		if evidence.CostKnown {
			score -= int(frontierT6MinInt64(evidence.CostMicrosPer1K, 10000000) / 1000)
		}
		rows = append(rows, frontierT6SelectionCandidateReceipt{
			CandidateID: candidateID, Eligible: len(reasons) == 0, ScoreBasisPoints: score,
			SampleCount: evidence.SampleCount, EvidenceDigest: evidence.Provenance.ContentDigest,
			Reasons: reasons,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Eligible != rows[j].Eligible {
			return rows[i].Eligible
		}
		if rows[i].ScoreBasisPoints != rows[j].ScoreBasisPoints {
			return rows[i].ScoreBasisPoints > rows[j].ScoreBasisPoints
		}
		if rows[i].SampleCount != rows[j].SampleCount {
			return rows[i].SampleCount > rows[j].SampleCount
		}
		return rows[i].CandidateID < rows[j].CandidateID
	})
	receipt := frontierT6SelectionReceipt{
		SchemaID: schemaID, Version: 1, Kind: kind, TaskClass: taskClass,
		Decision: "abstain", Confidence: "insufficient_evidence", SampleFloor: constraints.MinimumSamples,
		ConstraintsDigest: frontierT6Digest(constraints), Candidates: rows, Reasons: []string{}, AdvisoryOnly: true,
		RecommendedSurface: "cli_advisor",
		ActivationAllowed:  false, ExecutionPerformed: false, NetworkCalls: 0,
		CreatedAt: now.Format(time.RFC3339Nano),
	}
	eligible := 0
	for _, row := range rows {
		if row.Eligible {
			eligible++
		}
	}
	if eligible > 0 {
		receipt.Decision = "recommend"
		receipt.SelectedID = rows[0].CandidateID
		receipt.Confidence = "low"
		if eligible > 1 && rows[0].SampleCount >= constraints.MinimumSamples*2 {
			receipt.Confidence = "medium"
		}
		if eligible > 1 && rows[0].SampleCount >= constraints.MinimumSamples*5 && rows[0].ScoreBasisPoints-rows[1].ScoreBasisPoints >= 500 {
			receipt.Confidence = "high"
		}
	} else if len(rows) == 0 {
		receipt.Reasons = []string{"no_candidates_declared"}
	} else {
		receipt.Reasons = []string{"no_candidate_satisfies_readiness_capability_and_evidence_gates"}
	}
	receipt.ReceiptID = "ft6sel_" + strings.TrimPrefix(frontierT6Digest(map[string]any{
		"schema": schemaID, "task_class": taskClass, "constraints": constraints, "candidates": rows, "created_at": receipt.CreatedAt,
	}), "sha256:")[:24]
	return receipt, nil
}

func frontierT6MinInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func frontierT6NormalizeStringList(values []string, limit int) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "-", "_")))
		if value == "" || len(value) > 160 {
			continue
		}
		if _, err := frontierT6NormalizeID(value, "list value", 160); err != nil {
			continue
		}
		set[value] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func frontierT6Contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func frontierT6UniqueStrings(values []string) []string {
	set := map[string]struct{}{}
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

var frontierT6ProfileFieldNames = map[string]struct{}{
	"agent_family": {}, "model_context_window_tokens": {}, "context_budget_tokens": {},
	"reserved_response_tokens": {}, "max_evidence_items": {}, "max_source_age_seconds": {},
	"role": {}, "required_tools": {}, "output_format": {}, "push_mode": {},
	"runner_capabilities": {}, "allowed_sources": {},
}

func frontierT6GenericProfileFields() map[string]any {
	return map[string]any{
		"agent_family": "generic", "model_context_window_tokens": 4096,
		"context_budget_tokens": 1024, "reserved_response_tokens": 1024,
		"max_evidence_items": 8, "max_source_age_seconds": 3600,
		"role": "general", "required_tools": []string{}, "output_format": "text",
		"push_mode": "pull_only", "runner_capabilities": []string{}, "allowed_sources": []string{},
	}
}

func frontierT6NormalizeProfileFields(fields map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for key, raw := range fields {
		if _, ok := frontierT6ProfileFieldNames[key]; !ok {
			return nil, fmt.Errorf("unsupported agent context profile field %q", key)
		}
		switch key {
		case "model_context_window_tokens":
			value := anyToInt(raw, 0)
			if value < 512 || value > 10000000 {
				return nil, errors.New("model_context_window_tokens is outside its bounded range")
			}
			out[key] = value
		case "context_budget_tokens":
			value := anyToInt(raw, 0)
			if value < 128 || value > 1000000 {
				return nil, errors.New("context_budget_tokens is outside its bounded range")
			}
			out[key] = value
		case "reserved_response_tokens":
			value := anyToInt(raw, -1)
			if value < 0 || value > 1000000 {
				return nil, errors.New("reserved_response_tokens is outside its bounded range")
			}
			out[key] = value
		case "max_evidence_items":
			value := anyToInt(raw, 0)
			if value < 1 || value > 128 {
				return nil, errors.New("max_evidence_items is outside its bounded range")
			}
			out[key] = value
		case "max_source_age_seconds":
			value := anyToInt(raw, 0)
			if value < 1 || value > 365*24*60*60 {
				return nil, errors.New("max_source_age_seconds is outside its bounded range")
			}
			out[key] = value
		case "required_tools", "runner_capabilities", "allowed_sources":
			values, err := frontierT6NormalizeProfileList(raw, key)
			if err != nil {
				return nil, err
			}
			out[key] = values
		case "output_format":
			value := strings.ToLower(strings.TrimSpace(anyToString(raw)))
			if !frontierT6Contains([]string{"text", "markdown", "json", "structured_json"}, value) {
				return nil, errors.New("output_format is unsupported")
			}
			out[key] = value
		case "push_mode":
			value := strings.ToLower(strings.TrimSpace(anyToString(raw)))
			if !frontierT6Contains([]string{"pull_only", "preferred", "required"}, value) {
				return nil, errors.New("push_mode is unsupported")
			}
			out[key] = value
		case "agent_family", "role":
			value, err := frontierT6NormalizeID(strings.ToLower(anyToString(raw)), key, 80)
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
	}
	return out, nil
}

func frontierT6NormalizeProfileList(raw any, field string) ([]string, error) {
	values := []string{}
	switch typed := raw.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		if len(typed) > 64 {
			return nil, fmt.Errorf("%s exceeds the bounded item limit", field)
		}
		for _, value := range typed {
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s must contain strings only", field)
			}
			values = append(values, text)
		}
	case nil:
		return []string{}, nil
	default:
		return nil, fmt.Errorf("%s must be an array of strings", field)
	}
	if len(values) > 64 {
		return nil, fmt.Errorf("%s exceeds the bounded item limit", field)
	}
	for _, value := range values {
		normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(value, "-", "_")))
		if normalized == "" {
			return nil, fmt.Errorf("%s contains an empty constraint", field)
		}
		if _, err := frontierT6NormalizeID(normalized, field, 160); err != nil {
			return nil, err
		}
	}
	return frontierT6NormalizeStringList(values, 64), nil
}

func frontierT6ProfileKey(scope frontierT6Scope, agentID string) string {
	return frontierT6Digest(map[string]any{"workspace_id": scope.WorkspaceID, "project": scope.Project, "agent_id": agentID})
}

func frontierT6StoredProfileDigest(profile frontierT6StoredAgentProfile) string {
	profile.ProfileDigest = ""
	profile.UpdatedAt = ""
	return frontierT6Digest(profile)
}

func (s *frontierT6AgentFitStore) configureAgentProfile(scope frontierT6Scope, agentID string, fields map[string]any, provenance frontierT6Provenance, now time.Time) (frontierT6StoredAgentProfile, error) {
	normalizedScope, err := frontierT6NormalizeScope(scope, false)
	if err != nil {
		return frontierT6StoredAgentProfile{}, err
	}
	agentID, err = frontierT6NormalizeID(agentID, "agent_id", 160)
	if err != nil {
		return frontierT6StoredAgentProfile{}, err
	}
	normalizedFields, err := frontierT6NormalizeProfileFields(fields)
	if err != nil {
		return frontierT6StoredAgentProfile{}, err
	}
	if len(normalizedFields) == 0 {
		return frontierT6StoredAgentProfile{}, errors.New("agent context profile requires at least one declared field")
	}
	provenance = frontierT6CanonicalProvenance(provenance)
	if err := frontierT6ValidateProvenance(provenance, now, time.Time{}); err != nil {
		return frontierT6StoredAgentProfile{}, err
	}
	key := frontierT6ProfileKey(normalizedScope, agentID)
	profile := frontierT6StoredAgentProfile{
		SchemaID: frontierT6ContextProfileSchemaID, Version: 1,
		ProfileID: "ft6p_" + strings.TrimPrefix(key, "sha256:")[:24], Scope: normalizedScope,
		AgentID: agentID, Fields: normalizedFields, Provenance: provenance,
		UpdatedAt: now.UTC().Format(time.RFC3339Nano),
	}
	profile.ProfileDigest = frontierT6StoredProfileDigest(profile)
	err = s.mutate(now, func() error {
		if _, exists := s.state.Profiles[key]; !exists && len(s.state.Profiles) >= s.limits.MaxProfiles {
			return errors.New("Frontier T6 profile store is full")
		}
		s.state.Profiles[key] = profile
		return nil
	})
	return profile, err
}

func (s *frontierT6AgentFitStore) agentProfile(scope frontierT6Scope, agentID string) (frontierT6StoredAgentProfile, bool, error) {
	normalizedScope, err := frontierT6NormalizeScope(scope, false)
	if err != nil {
		return frontierT6StoredAgentProfile{}, false, err
	}
	agentID, err = frontierT6NormalizeID(agentID, "agent_id", 160)
	if err != nil {
		return frontierT6StoredAgentProfile{}, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	profile, exists := s.state.Profiles[frontierT6ProfileKey(normalizedScope, agentID)]
	return profile, exists, nil
}

func frontierT6ResolveAgentContextProfile(request frontierT6ProfileResolutionRequest) (frontierT6ProfileResolution, error) {
	now := request.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	scope, err := frontierT6NormalizeScope(request.Scope, false)
	if err != nil {
		return frontierT6ProfileResolution{}, err
	}
	agentID, err := frontierT6NormalizeID(request.AgentID, "agent_id", 160)
	if err != nil {
		return frontierT6ProfileResolution{}, err
	}
	explicit, err := frontierT6NormalizeProfileFields(request.ExplicitFields)
	if err != nil {
		return frontierT6ProfileResolution{}, err
	}
	effective := frontierT6GenericProfileFields()
	sources := map[string]string{}
	for key := range effective {
		sources[key] = "generic_default"
	}
	storedUsed := false
	conflicts := []string{}
	if request.Stored != nil {
		stored := *request.Stored
		if stored.SchemaID != frontierT6ContextProfileSchemaID || stored.Version != 1 || stored.AgentID != agentID || stored.ProfileDigest != frontierT6StoredProfileDigest(stored) || stored.Scope.WorkspaceID != scope.WorkspaceID || stored.Scope.Project != scope.Project {
			conflicts = append(conflicts, "stored_profile_identity_or_integrity_invalid")
		} else if err := frontierT6ValidateProvenance(stored.Provenance, now, time.Time{}); err != nil {
			conflicts = append(conflicts, "stored_profile_provenance_stale")
		} else {
			storedFields, normalizeErr := frontierT6NormalizeProfileFields(stored.Fields)
			if normalizeErr != nil {
				conflicts = append(conflicts, "stored_profile_fields_invalid")
			} else {
				for key, value := range storedFields {
					effective[key] = value
					sources[key] = "stored_agent_default"
				}
				storedUsed = true
			}
		}
	}
	explicitNames := make([]string, 0, len(explicit))
	for key, value := range explicit {
		effective[key] = value
		sources[key] = "explicit_request_or_cli"
		explicitNames = append(explicitNames, key)
	}
	sort.Strings(explicitNames)
	isExplicit := func(field string) bool {
		_, ok := explicit[field]
		return ok
	}
	capabilities := request.Capabilities
	capabilities.Tools = frontierT6NormalizeStringList(capabilities.Tools, 64)
	capabilities.OutputFormats = frontierT6NormalizeStringList(capabilities.OutputFormats, 16)
	capabilities.InjectionBoundaries = frontierT6NormalizeStringList(capabilities.InjectionBoundaries, 8)
	capabilities.RunnerCapabilities = frontierT6NormalizeStringList(capabilities.RunnerCapabilities, 64)
	capabilities.AuthorizedSources = frontierT6NormalizeStringList(capabilities.AuthorizedSources, 64)
	adjustments := []string{}
	if capabilities.AgentFamily != "" && !isExplicit("agent_family") {
		family, normalizeErr := frontierT6NormalizeID(strings.ToLower(capabilities.AgentFamily), "agent_family", 80)
		if normalizeErr != nil {
			conflicts = append(conflicts, "declared_agent_family_invalid")
		} else {
			effective["agent_family"] = family
			sources["agent_family"] = "declared_capability"
		}
	}
	window := anyToInt(effective["model_context_window_tokens"], 4096)
	if capabilities.ContextWindowTokens > 0 && window > capabilities.ContextWindowTokens {
		if isExplicit("model_context_window_tokens") {
			conflicts = append(conflicts, "explicit_model_context_window_exceeds_declared_capability")
		} else {
			effective["model_context_window_tokens"] = capabilities.ContextWindowTokens
			window = capabilities.ContextWindowTokens
			adjustments = append(adjustments, "model_context_window_reduced_to_declared_capability")
		}
	}
	reserved := anyToInt(effective["reserved_response_tokens"], 1024)
	budget := anyToInt(effective["context_budget_tokens"], 1024)
	available := window - reserved
	if available < 128 {
		conflicts = append(conflicts, "declared_window_cannot_satisfy_reserved_response_constraint")
	} else if budget > available {
		if isExplicit("context_budget_tokens") || isExplicit("reserved_response_tokens") || isExplicit("model_context_window_tokens") {
			conflicts = append(conflicts, "explicit_context_budget_constraints_exceed_declared_window")
		} else {
			effective["context_budget_tokens"] = available
			budget = available
			adjustments = append(adjustments, "context_budget_reduced_to_fit_declared_window")
		}
	}
	maxEvidence := anyToInt(effective["max_evidence_items"], 8)
	maxByBudget := maxInt(1, budget/96)
	if maxEvidence > maxByBudget {
		if isExplicit("max_evidence_items") {
			conflicts = append(conflicts, "explicit_evidence_item_constraint_exceeds_context_budget")
		} else {
			effective["max_evidence_items"] = maxByBudget
			adjustments = append(adjustments, "evidence_item_limit_reduced_to_context_budget")
		}
	}
	frontierT6AdaptProfileList(effective, "required_tools", capabilities.Tools, capabilities.Declared, isExplicit("required_tools"), &adjustments, &conflicts)
	frontierT6AdaptProfileList(effective, "runner_capabilities", capabilities.RunnerCapabilities, capabilities.Declared, isExplicit("runner_capabilities"), &adjustments, &conflicts)
	frontierT6AdaptProfileList(effective, "allowed_sources", capabilities.AuthorizedSources, capabilities.Declared, isExplicit("allowed_sources"), &adjustments, &conflicts)
	if capabilities.Declared {
		outputFormat := anyToString(effective["output_format"])
		if len(capabilities.OutputFormats) > 0 && !frontierT6Contains(capabilities.OutputFormats, outputFormat) {
			if isExplicit("output_format") {
				conflicts = append(conflicts, "explicit_output_format_unsupported")
			} else {
				effective["output_format"] = capabilities.OutputFormats[0]
				adjustments = append(adjustments, "output_format_changed_to_declared_capability")
			}
		}
		pushMode := anyToString(effective["push_mode"])
		if pushMode != "pull_only" && (!capabilities.PushSupported || len(capabilities.InjectionBoundaries) == 0) {
			if isExplicit("push_mode") {
				conflicts = append(conflicts, "explicit_push_constraint_unsupported")
			} else {
				effective["push_mode"] = "pull_only"
				adjustments = append(adjustments, "push_mode_fell_back_to_pull_only")
			}
		}
	}
	unknownAgent := !storedUsed && capabilities.AgentFamily == "" && !isExplicit("agent_family")
	decision := "ready"
	if len(conflicts) > 0 {
		decision = "abstain"
	} else if unknownAgent || !capabilities.Declared {
		decision = "fallback"
	}
	effective["schema_id"] = frontierT6ContextProfileSchemaID
	effective["version"] = 1
	effective["agent_id"] = agentID
	effective["unknown_agent"] = unknownAgent
	effective["fallback_safe"] = len(conflicts) == 0
	profileDigest := frontierT6Digest(map[string]any{"scope": scope, "agent_id": agentID, "profile": effective, "field_sources": sources})
	effective["profile_digest"] = profileDigest
	return frontierT6ProfileResolution{
		SchemaID: frontierT6ContextProfileSchemaID, Version: 1, Decision: decision, AgentID: agentID,
		UnknownAgent: unknownAgent, ColdStart: unknownAgent || !capabilities.Declared,
		StoredProfileUsed: storedUsed, EffectiveProfile: effective, FieldSources: sources,
		ExplicitFields: explicitNames, Adjustments: frontierT6UniqueStrings(adjustments),
		Conflicts: frontierT6UniqueStrings(conflicts), ProfileDigest: profileDigest,
		AutomaticExecution: false, RecommendedSurface: "cli_context_package",
	}, nil
}

func frontierT6AdaptProfileList(effective map[string]any, field string, available []string, capabilitiesDeclared, explicit bool, adjustments, conflicts *[]string) {
	required := frontierT6NormalizeStringList(anyToStringSlice(effective[field]), 64)
	if len(required) == 0 || !capabilitiesDeclared {
		return
	}
	missing := []string{}
	retained := []string{}
	for _, value := range required {
		if frontierT6Contains(available, value) {
			retained = append(retained, value)
		} else {
			missing = append(missing, value)
		}
	}
	if len(missing) == 0 {
		return
	}
	if explicit {
		*conflicts = append(*conflicts, "explicit_"+field+"_unsupported:"+strings.Join(missing, ","))
		return
	}
	effective[field] = retained
	*adjustments = append(*adjustments, field+"_reduced_to_declared_capabilities")
}

func frontierT6PrepDedupeKey(request frontierT6ContextPrepRequest) string {
	return frontierT6Digest(map[string]any{
		"scope": request.Scope, "task_id": request.TaskID, "next_action_class": request.NextActionClass,
		"profile": request.EffectiveProfileDigest, "source_generation": request.SourceGeneration,
		"authorization": request.Approval.AuthorizationDigest,
	})
}

func (s *frontierT6AgentFitStore) scheduleContextPrep(request frontierT6ContextPrepRequest, now time.Time) (frontierT6ContextPrepScheduleResult, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	result := frontierT6ContextPrepScheduleResult{
		Decision: "abstain", Reasons: []string{}, ExecutionOwner: "external_cli_worker",
		ExecutionPerformed: false, NetworkCalls: 0,
	}
	scope, err := frontierT6NormalizeScope(request.Scope, false)
	if err != nil {
		return result, err
	}
	request.Scope = scope
	request.TaskID, err = frontierT6NormalizeID(request.TaskID, "task_id", 160)
	if err != nil {
		return result, err
	}
	request.NextActionClass, err = frontierT6NormalizeID(request.NextActionClass, "next_action_class", 120)
	if err != nil {
		return result, err
	}
	request.SourceGeneration, err = frontierT6NormalizeID(request.SourceGeneration, "source_generation", 192)
	if err != nil {
		return result, err
	}
	request.EffectiveProfileDigest = strings.ToLower(strings.TrimSpace(request.EffectiveProfileDigest))
	request.Approval.ScopeDigest = strings.ToLower(strings.TrimSpace(request.Approval.ScopeDigest))
	request.Approval.AuthorizationDigest = strings.ToLower(strings.TrimSpace(request.Approval.AuthorizationDigest))
	request.Provenance = frontierT6CanonicalProvenance(request.Provenance)
	if !frontierT6ValidDigest(request.EffectiveProfileDigest) {
		return result, errors.New("effective_profile_digest must be a SHA-256 digest")
	}
	minimumConfidence := request.MinimumConfidence
	if minimumConfidence == 0 {
		minimumConfidence = 0.75
	}
	if math.IsNaN(request.PredictionConfidence) || math.IsInf(request.PredictionConfidence, 0) || request.PredictionConfidence < 0 || request.PredictionConfidence > 1 || minimumConfidence < 0.5 || minimumConfidence > 1 {
		return result, errors.New("context preparation confidence is outside its bounded range")
	}
	if !request.Approval.Approved {
		result.Reasons = []string{"explicit_opt_in_approval_required"}
		return result, nil
	}
	if request.PredictionConfidence < minimumConfidence {
		result.Reasons = []string{"prediction_confidence_below_declared_floor"}
		return result, nil
	}
	request.Approval.ApprovalID, err = frontierT6NormalizeID(request.Approval.ApprovalID, "approval_id", 160)
	if err != nil {
		return result, err
	}
	if request.Approval.ScopeDigest != frontierT6ScopeDigest(scope) || !frontierT6ValidDigest(request.Approval.AuthorizationDigest) {
		return result, errors.New("context preparation approval scope or authorization is invalid")
	}
	ttl := clampInt(request.TTLSeconds, 30, 60*60)
	if request.TTLSeconds <= 0 {
		ttl = 15 * 60
	}
	expiresAt := now.Add(time.Duration(ttl) * time.Second)
	approvalExpires, approvalOK := frontierT6ParseTime(request.Approval.ExpiresAt)
	if !approvalOK || approvalExpires.Before(expiresAt) {
		return result, errors.New("context preparation approval does not cover the requested TTL")
	}
	if request.Provenance.SourceGeneration != request.SourceGeneration || request.Provenance.AuthorizationDigest != request.Approval.AuthorizationDigest {
		return result, errors.New("context preparation provenance does not match source generation and authorization")
	}
	if err := frontierT6ValidateProvenance(request.Provenance, now, expiresAt); err != nil {
		return result, err
	}
	dedupeKey := frontierT6PrepDedupeKey(request)
	var prep frontierT6ContextPrepRecord
	deduplicated := false
	err = s.mutate(now, func() error {
		for _, existing := range s.state.ContextPreps {
			existingExpires, ok := frontierT6ParseTime(existing.ExpiresAt)
			if existing.DedupeKey == dedupeKey && ok && now.Before(existingExpires) && existing.Status != "failed" && existing.Status != "canceled" && existing.Status != "expired" {
				prep, deduplicated = existing, true
				return nil
			}
		}
		frontierT6TrimTerminalPreps(s.state.ContextPreps, s.limits.MaxPreps-1)
		if len(s.state.ContextPreps) >= s.limits.MaxPreps {
			return errors.New("Frontier T6 context preparation store is full")
		}
		createdAt := now.UTC().Format(time.RFC3339Nano)
		prep = frontierT6ContextPrepRecord{
			SchemaID: frontierT6ContextPrepSchemaID, Version: 1,
			PrepID:    "ft6prep_" + strings.TrimPrefix(frontierT6Digest(map[string]any{"dedupe": dedupeKey, "created_at": createdAt}), "sha256:")[:24],
			DedupeKey: dedupeKey, Scope: scope, TaskID: request.TaskID, NextActionClass: request.NextActionClass,
			PredictionConfidence: request.PredictionConfidence, EffectiveProfileDigest: request.EffectiveProfileDigest,
			SourceGeneration: request.SourceGeneration, AuthorizationDigest: request.Approval.AuthorizationDigest,
			ApprovalDigest: frontierT6Digest(request.Approval), Provenance: request.Provenance,
			Status: "queued", Attempts: 0, NextAttemptAt: createdAt,
			CreatedAt: createdAt, UpdatedAt: createdAt, ExpiresAt: expiresAt.UTC().Format(time.RFC3339Nano),
		}
		s.state.ContextPreps[prep.PrepID] = prep
		return nil
	})
	if err != nil {
		return result, err
	}
	result.Decision = "scheduled"
	result.Deduplicated = deduplicated
	result.Prep = &prep
	return result, nil
}

func frontierT6PrepClaimMatches(token, digest string) bool {
	return token != "" && digest == frontierT6OpaqueDigest("frontier-t6-prep-claim", token)
}

func (s *frontierT6AgentFitStore) claimContextPrep(scope frontierT6Scope, prepID, workerID string, now time.Time) (frontierT6ContextPrepClaim, bool, error) {
	normalizedScope, err := frontierT6NormalizeScope(scope, false)
	if err != nil {
		return frontierT6ContextPrepClaim{}, false, err
	}
	if prepID != "" {
		if _, err := frontierT6NormalizeID(prepID, "prep_id", 160); err != nil {
			return frontierT6ContextPrepClaim{}, false, err
		}
	}
	if _, err := frontierT6NormalizeID(workerID, "worker_id", 160); err != nil {
		return frontierT6ContextPrepClaim{}, false, err
	}
	var claim frontierT6ContextPrepClaim
	found := false
	err = s.mutate(now, func() error {
		ids := make([]string, 0, len(s.state.ContextPreps))
		for id := range s.state.ContextPreps {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			left, right := s.state.ContextPreps[ids[i]], s.state.ContextPreps[ids[j]]
			if left.NextAttemptAt == right.NextAttemptAt {
				return ids[i] < ids[j]
			}
			return left.NextAttemptAt < right.NextAttemptAt
		})
		for _, id := range ids {
			prep := s.state.ContextPreps[id]
			if prepID != "" && prep.PrepID != prepID {
				continue
			}
			if prep.Scope.WorkspaceID != normalizedScope.WorkspaceID || prep.Scope.Project != normalizedScope.Project {
				continue
			}
			expiresAt, expiresOK := frontierT6ParseTime(prep.ExpiresAt)
			if !expiresOK || !now.Before(expiresAt) {
				continue
			}
			leaseExpires, leaseOK := frontierT6ParseTime(prep.LeaseExpiresAt)
			if prep.Status == "preparing" && leaseOK && now.Before(leaseExpires) {
				continue
			}
			nextAttempt, nextOK := frontierT6ParseTime(prep.NextAttemptAt)
			if nextOK && now.Before(nextAttempt) {
				continue
			}
			if prep.Status != "queued" && prep.Status != "retry_pending" && prep.Status != "preparing" {
				continue
			}
			if prep.Attempts >= frontierT6PrepMaxAttempts {
				prep.Status = "failed"
				prep.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
				s.state.ContextPreps[id] = prep
				continue
			}
			prep.Attempts++
			prep.Status = "preparing"
			prep.WorkerDigest = frontierT6OpaqueDigest("frontier-t6-prep-worker", workerID)
			prep.LeaseExpiresAt = now.Add(60 * time.Second).UTC().Format(time.RFC3339Nano)
			prep.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
			token := "ft6prepclaim_" + strings.TrimPrefix(frontierT6Digest(map[string]any{"prep": prep.PrepID, "attempt": prep.Attempts, "lease": prep.LeaseExpiresAt}), "sha256:")[:32]
			prep.ClaimDigest = frontierT6OpaqueDigest("frontier-t6-prep-claim", token)
			s.state.ContextPreps[id] = prep
			claim = frontierT6ContextPrepClaim{Prep: prep, ClaimToken: token, ExecutionOwner: "external_cli_worker", GatewayExecutionPerformed: false}
			found = true
			break
		}
		return nil
	})
	return claim, found, err
}

func frontierT6ValidatePrepArtifact(prep frontierT6ContextPrepRecord, artifact frontierT6ContextPrepArtifact, now time.Time) (frontierT6ContextPrepArtifact, error) {
	if artifact.SchemaID == "" {
		artifact.SchemaID = frontierT6ContextPrepArtifactID
	}
	if artifact.Version == 0 {
		artifact.Version = 1
	}
	artifact.ContextPackDigest = strings.ToLower(strings.TrimSpace(artifact.ContextPackDigest))
	artifact.RetrievalReceiptDigest = strings.ToLower(strings.TrimSpace(artifact.RetrievalReceiptDigest))
	artifact.EffectiveProfileDigest = strings.ToLower(strings.TrimSpace(artifact.EffectiveProfileDigest))
	artifact.SourceGeneration = strings.TrimSpace(artifact.SourceGeneration)
	artifact.AuthorizationDigest = strings.ToLower(strings.TrimSpace(artifact.AuthorizationDigest))
	if artifact.SchemaID != frontierT6ContextPrepArtifactID || artifact.Version != 1 || !frontierT6ValidDigest(artifact.ContextPackDigest) || !frontierT6ValidDigest(artifact.RetrievalReceiptDigest) {
		return frontierT6ContextPrepArtifact{}, errors.New("context preparation artifact contract or evidence digests are invalid")
	}
	if artifact.EffectiveProfileDigest != prep.EffectiveProfileDigest || artifact.SourceGeneration != prep.SourceGeneration || artifact.AuthorizationDigest != prep.AuthorizationDigest {
		return frontierT6ContextPrepArtifact{}, errors.New("context preparation artifact scope evidence does not match the preparation")
	}
	prepExpires, _ := frontierT6ParseTime(prep.ExpiresAt)
	prepCreated, prepCreatedOK := frontierT6ParseTime(prep.CreatedAt)
	artifactCreated, artifactCreatedOK := frontierT6ParseTime(artifact.CreatedAt)
	if artifact.CreatedAt == "" {
		artifact.CreatedAt = now.UTC().Format(time.RFC3339Nano)
		artifactCreated, artifactCreatedOK = now.UTC(), true
	}
	artifactExpires, expiresOK := frontierT6ParseTime(artifact.ExpiresAt)
	if !prepCreatedOK || !artifactCreatedOK || artifactCreated.Before(prepCreated) || artifactCreated.After(now.Add(5*time.Minute)) || !expiresOK || !artifactExpires.After(artifactCreated) || artifactExpires.After(prepExpires) || !now.Before(artifactExpires) {
		return frontierT6ContextPrepArtifact{}, errors.New("context preparation artifact expiry is invalid")
	}
	if len(artifact.EvidenceRefs) == 0 || len(artifact.EvidenceRefs) > 32 {
		return frontierT6ContextPrepArtifact{}, errors.New("context preparation artifact evidence references are outside bounds")
	}
	for index := range artifact.EvidenceRefs {
		ref := artifact.EvidenceRefs[index]
		ref.SourceID = strings.TrimSpace(ref.SourceID)
		ref.SourceGeneration = strings.TrimSpace(ref.SourceGeneration)
		ref.ContentDigest = strings.ToLower(strings.TrimSpace(ref.ContentDigest))
		ref.AuthorizationDigest = strings.ToLower(strings.TrimSpace(ref.AuthorizationDigest))
		if _, err := frontierT6NormalizeID(ref.SourceID, "artifact source_id", 192); err != nil || ref.SourceGeneration != prep.SourceGeneration || ref.AuthorizationDigest != prep.AuthorizationDigest || !frontierT6ValidDigest(ref.ContentDigest) {
			return frontierT6ContextPrepArtifact{}, errors.New("context preparation artifact contains stale or unauthorized evidence")
		}
		freshUntil, ok := frontierT6ParseTime(ref.FreshUntil)
		if !ok || freshUntil.Before(artifactExpires) {
			return frontierT6ContextPrepArtifact{}, errors.New("context preparation artifact evidence is not fresh for its lifetime")
		}
		ref.FreshUntil = freshUntil.Format(time.RFC3339Nano)
		artifact.EvidenceRefs[index] = ref
	}
	if artifact.ArtifactID == "" {
		artifact.ArtifactID = "ft6artifact_" + strings.TrimPrefix(frontierT6Digest(map[string]any{"prep": prep.PrepID, "pack": artifact.ContextPackDigest, "receipt": artifact.RetrievalReceiptDigest}), "sha256:")[:24]
	} else if _, err := frontierT6NormalizeID(artifact.ArtifactID, "artifact_id", 160); err != nil {
		return frontierT6ContextPrepArtifact{}, err
	}
	return artifact, nil
}

func (s *frontierT6AgentFitStore) completeContextPrep(prepID, claimToken string, artifact frontierT6ContextPrepArtifact, now time.Time) (frontierT6ContextPrepRecord, error) {
	var completed frontierT6ContextPrepRecord
	err := s.mutate(now, func() error {
		prep, exists := s.state.ContextPreps[prepID]
		if !exists {
			return errors.New("Frontier T6 context preparation was not found")
		}
		leaseExpires, leaseOK := frontierT6ParseTime(prep.LeaseExpiresAt)
		if prep.Status != "preparing" || !frontierT6PrepClaimMatches(claimToken, prep.ClaimDigest) || !leaseOK || !now.Before(leaseExpires) {
			return errFrontierT6ClaimStale
		}
		normalized, err := frontierT6ValidatePrepArtifact(prep, artifact, now)
		if err != nil {
			return err
		}
		prep.Artifact = &normalized
		prep.Status = "ready"
		prep.ClaimDigest, prep.LeaseExpiresAt, prep.NextAttemptAt = "", "", ""
		prep.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		s.state.ContextPreps[prepID] = prep
		completed = prep
		return nil
	})
	return completed, err
}

func (s *frontierT6AgentFitStore) failContextPrep(prepID, claimToken, reasonCode string, retryable bool, now time.Time) error {
	reasonCode, err := frontierT6NormalizeID(reasonCode, "reason_code", 80)
	if err != nil {
		return err
	}
	return s.mutate(now, func() error {
		prep, exists := s.state.ContextPreps[prepID]
		if !exists {
			return errors.New("Frontier T6 context preparation was not found")
		}
		leaseExpires, leaseOK := frontierT6ParseTime(prep.LeaseExpiresAt)
		if prep.Status != "preparing" || !frontierT6PrepClaimMatches(claimToken, prep.ClaimDigest) || !leaseOK || !now.Before(leaseExpires) {
			return errFrontierT6ClaimStale
		}
		prep.ClaimDigest, prep.LeaseExpiresAt = "", ""
		prep.LastReasonDigest = frontierT6OpaqueDigest("frontier-t6-prep-reason", reasonCode)
		prep.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		if retryable && prep.Attempts < frontierT6PrepMaxAttempts {
			prep.Status = "retry_pending"
			prep.NextAttemptAt = now.Add(time.Duration(1<<uint(prep.Attempts-1)) * 5 * time.Second).UTC().Format(time.RFC3339Nano)
		} else {
			prep.Status = "failed"
			prep.NextAttemptAt = ""
		}
		s.state.ContextPreps[prepID] = prep
		return nil
	})
}

func (s *frontierT6AgentFitStore) useContextPrep(scope frontierT6Scope, prepID, taskID, profileDigest, sourceGeneration, authorizationDigest string, now time.Time) frontierT6ContextPrepUse {
	result := frontierT6ContextPrepUse{Eligible: false, Reasons: []string{}, InjectionPerformed: false, RequiresExplicitCLIUse: true}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	normalizedScope, err := frontierT6NormalizeScope(scope, false)
	if err != nil {
		result.Reasons = []string{"scope_invalid"}
		return result
	}
	s.mu.RLock()
	prep, exists := s.state.ContextPreps[prepID]
	s.mu.RUnlock()
	if !exists || prep.Scope.WorkspaceID != normalizedScope.WorkspaceID || prep.Scope.Project != normalizedScope.Project {
		result.Reasons = []string{"preparation_not_found_in_scope"}
		return result
	}
	if prep.Status != "ready" || prep.Artifact == nil {
		result.Reasons = []string{"preparation_not_ready"}
		return result
	}
	expiresAt, expiresOK := frontierT6ParseTime(prep.ExpiresAt)
	if !expiresOK || !now.Before(expiresAt) {
		result.Reasons = []string{"preparation_expired"}
		return result
	}
	artifactExpires, artifactExpiresOK := frontierT6ParseTime(prep.Artifact.ExpiresAt)
	if !artifactExpiresOK || !now.Before(artifactExpires) {
		result.Reasons = []string{"prepared_artifact_expired"}
		return result
	}
	if prep.TaskID != taskID {
		result.Reasons = append(result.Reasons, "task_pivot_detected")
	}
	if prep.EffectiveProfileDigest != profileDigest {
		result.Reasons = append(result.Reasons, "effective_profile_changed")
	}
	if prep.SourceGeneration != sourceGeneration {
		result.Reasons = append(result.Reasons, "source_generation_changed")
	}
	if prep.AuthorizationDigest != authorizationDigest {
		result.Reasons = append(result.Reasons, "authorization_changed")
	}
	for _, ref := range prep.Artifact.EvidenceRefs {
		freshUntil, ok := frontierT6ParseTime(ref.FreshUntil)
		if !ok || !now.Before(freshUntil) || ref.SourceGeneration != sourceGeneration || ref.AuthorizationDigest != authorizationDigest {
			result.Reasons = append(result.Reasons, "artifact_contains_stale_or_unauthorized_evidence")
			break
		}
	}
	result.Reasons = frontierT6UniqueStrings(result.Reasons)
	if len(result.Reasons) == 0 {
		artifact := *prep.Artifact
		result.Eligible = true
		result.Artifact = &artifact
	}
	return result
}

type frontierT6RequestAuthorization struct {
	Authorized          bool
	WorkspaceID         string
	SubjectID           string
	AuthorizationDigest string
	AllowActivation     bool
	AllowPersistence    bool
	AllowWorker         bool
}

type frontierT6AuthorizeFunc func(r *http.Request, featureID, operation string) (frontierT6RequestAuthorization, error)

type frontierT6AgentFitHandlers struct {
	Store     *frontierT6AgentFitStore
	Authorize frontierT6AuthorizeFunc
	Now       func() time.Time
}

func (h frontierT6AgentFitHandlers) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

func (h frontierT6AgentFitHandlers) authorize(w http.ResponseWriter, r *http.Request, featureID, operation string) (frontierT6RequestAuthorization, bool) {
	if h.Authorize == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_authorization_unwired"})
		return frontierT6RequestAuthorization{}, false
	}
	auth, err := h.Authorize(r, featureID, operation)
	if err != nil || !auth.Authorized {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "frontier_t6_authorization_required"})
		return frontierT6RequestAuthorization{}, false
	}
	workspaceID, err := frontierT6NormalizeID(auth.WorkspaceID, "authorized workspace_id", 160)
	if err != nil || !frontierT6ValidDigest(auth.AuthorizationDigest) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_authorization_invalid"})
		return frontierT6RequestAuthorization{}, false
	}
	auth.WorkspaceID = workspaceID
	auth.AuthorizationDigest = strings.ToLower(strings.TrimSpace(auth.AuthorizationDigest))
	return auth, true
}

func frontierT6DecodeBody(r *http.Request, target any) error {
	if r == nil || r.Body == nil {
		return errors.New("request body is required")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 256*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}

func frontierT6BindAuthorizedScope(scope frontierT6Scope, auth frontierT6RequestAuthorization) (frontierT6Scope, error) {
	if scope.WorkspaceID != "" && scope.WorkspaceID != auth.WorkspaceID {
		return frontierT6Scope{}, errors.New("workspace override is forbidden")
	}
	scope.WorkspaceID = auth.WorkspaceID
	return scope, nil
}

func frontierT6WriteHandlerError(w http.ResponseWriter, err error) {
	status := http.StatusUnprocessableEntity
	code := "frontier_t6_operation_rejected"
	if errors.Is(err, errFrontierT6CursorExpired) {
		status, code = http.StatusConflict, "replay_cursor_expired"
	} else if errors.Is(err, errFrontierT6ClaimStale) {
		status, code = http.StatusConflict, "delivery_claim_stale"
	} else if strings.Contains(strings.ToLower(err.Error()), "store is full") || strings.Contains(strings.ToLower(err.Error()), "disabled") || strings.Contains(strings.ToLower(err.Error()), "commit outcome") {
		status, code = http.StatusServiceUnavailable, "frontier_t6_store_unavailable"
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": code})
}

type frontierT6SteeringHTTPRequest struct {
	Operation    string                            `json:"operation"`
	Scope        frontierT6Scope                   `json:"scope"`
	Publish      *frontierT6SteeringPublishRequest `json:"publish,omitempty"`
	SubscriberID string                            `json:"subscriber_id,omitempty"`
	Capabilities frontierT6HarnessCapabilities     `json:"capabilities,omitempty"`
	Cursor       string                            `json:"cursor,omitempty"`
	Limit        int                               `json:"limit,omitempty"`
	DeliveryID   string                            `json:"delivery_id,omitempty"`
	EventID      string                            `json:"event_id,omitempty"`
	ClaimToken   string                            `json:"claim_token,omitempty"`
	ReasonCode   string                            `json:"reason_code,omitempty"`
}

func (h frontierT6AgentFitHandlers) Steering(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_store_unavailable"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	request := frontierT6SteeringHTTPRequest{}
	if err := frontierT6DecodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	auth, ok := h.authorize(w, r, frontierT6AsyncSteeringFeatureID, operation)
	if !ok {
		return
	}
	scope, err := frontierT6BindAuthorizedScope(request.Scope, auth)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "workspace_override_forbidden"})
		return
	}
	if operation != "replay" && !auth.AllowActivation {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{"ok": false, "error": "push_activation_not_entitled", "pull_fallback_available": true})
		return
	}
	now := h.now()
	var response any
	switch operation {
	case "publish":
		if request.Publish == nil {
			err = errors.New("publish payload is required")
			break
		}
		request.Publish.Scope = scope
		request.Publish.Provenance.AuthorizationDigest = auth.AuthorizationDigest
		var event frontierT6SteeringEvent
		var deduplicated bool
		event, deduplicated, err = h.Store.publishSteering(*request.Publish, now)
		response = map[string]any{"event": event, "deduplicated": deduplicated, "push_performed": false}
	case "replay":
		response, err = h.Store.replaySteering(scope, request.Cursor, now, request.Limit)
	case "claim":
		response, err = h.Store.claimSteering(scope, request.SubscriberID, request.Capabilities, request.Cursor, now, request.Limit)
	case "delivered":
		err = h.Store.recordSteeringDelivered(request.DeliveryID, request.ClaimToken, now)
		response = map[string]any{"delivery_recorded": err == nil, "consumption_acknowledged": false}
	case "fail":
		err = h.Store.failSteeringDelivery(request.DeliveryID, request.ClaimToken, request.ReasonCode, now)
		response = map[string]any{"retry_recorded": err == nil}
	case "acknowledge", "ack":
		var cursor string
		cursor, err = h.Store.acknowledgeSteering(scope, request.SubscriberID, request.DeliveryID, request.EventID, now)
		response = map[string]any{"acknowledged": err == nil, "ack_cursor": cursor}
	default:
		err = errors.New("unsupported steering operation")
	}
	if err != nil {
		frontierT6WriteHandlerError(w, err)
		return
	}
	payload := map[string]any{"ok": true, "schema_id": frontierT6AgentFitContractID, "operation": operation, "result": response, "network_calls": 0, "automatic_model_execution": false}
	writeJSON(w, http.StatusOK, frontierT6AttachFormatContract(frontierT6AgentFitContractID, payload, "agent_http"))
}

type frontierT6SelectionHTTPRequest struct {
	Kind    string                     `json:"kind"`
	Request frontierT6SelectionRequest `json:"request"`
}

func (h frontierT6AgentFitHandlers) Selection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	request := frontierT6SelectionHTTPRequest{}
	if err := frontierT6DecodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	if _, ok := h.authorize(w, r, frontierT6RunnerSelectionFeatureID, "advise"); !ok {
		return
	}
	request.Request.Now = h.now()
	var receipt frontierT6SelectionReceipt
	var err error
	switch strings.ToLower(strings.TrimSpace(request.Kind)) {
	case "runner":
		receipt, err = frontierT6AdviseRunnerSelection(request.Request)
	case "model":
		receipt, err = frontierT6AdviseModelSelection(request.Request)
	default:
		err = errors.New("selection kind must be runner or model")
	}
	if err != nil {
		frontierT6WriteHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, frontierT6AttachFormatContract(receipt.SchemaID, receipt, "agent_http"))
}

type frontierT6ProfileHTTPRequest struct {
	Operation      string                      `json:"operation"`
	Scope          frontierT6Scope             `json:"scope"`
	AgentID        string                      `json:"agent_id"`
	Fields         map[string]any              `json:"fields,omitempty"`
	Provenance     frontierT6Provenance        `json:"provenance,omitempty"`
	ExplicitFields map[string]any              `json:"explicit_fields,omitempty"`
	Capabilities   frontierT6AgentCapabilities `json:"capabilities,omitempty"`
}

func (h frontierT6AgentFitHandlers) Profile(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_store_unavailable"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	request := frontierT6ProfileHTTPRequest{}
	if err := frontierT6DecodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	auth, ok := h.authorize(w, r, frontierT6AgentContextFeatureID, operation)
	if !ok {
		return
	}
	scope, err := frontierT6BindAuthorizedScope(request.Scope, auth)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "workspace_override_forbidden"})
		return
	}
	now := h.now()
	var response any
	switch operation {
	case "configure":
		if !auth.AllowPersistence {
			writeJSON(w, http.StatusPaymentRequired, map[string]any{"ok": false, "error": "persistent_profile_not_entitled", "explicit_profile_available": true})
			return
		}
		request.Provenance.AuthorizationDigest = auth.AuthorizationDigest
		response, err = h.Store.configureAgentProfile(scope, request.AgentID, request.Fields, request.Provenance, now)
	case "resolve":
		stored, exists, lookupErr := h.Store.agentProfile(scope, request.AgentID)
		if lookupErr != nil {
			err = lookupErr
			break
		}
		var storedPtr *frontierT6StoredAgentProfile
		if exists {
			storedPtr = &stored
		}
		response, err = frontierT6ResolveAgentContextProfile(frontierT6ProfileResolutionRequest{
			Scope: scope, AgentID: request.AgentID, Stored: storedPtr, ExplicitFields: request.ExplicitFields,
			Capabilities: request.Capabilities, Now: now,
		})
	default:
		err = errors.New("unsupported profile operation")
	}
	if err != nil {
		frontierT6WriteHandlerError(w, err)
		return
	}
	payload := map[string]any{
		"ok": true, "schema_id": frontierT6AgentFitContractID, "operation": operation,
		"result_schema_id": frontierT6ContextProfileSchemaID, "result": response,
		"network_calls": 0, "automatic_model_execution": false,
	}
	writeJSON(w, http.StatusOK, frontierT6AttachFormatContract(frontierT6AgentFitContractID, payload, "agent_http"))
}

type frontierT6PrepHTTPRequest struct {
	Operation              string                        `json:"operation"`
	Scope                  frontierT6Scope               `json:"scope"`
	Schedule               *frontierT6ContextPrepRequest `json:"schedule,omitempty"`
	PrepID                 string                        `json:"prep_id,omitempty"`
	WorkerID               string                        `json:"worker_id,omitempty"`
	ClaimToken             string                        `json:"claim_token,omitempty"`
	Artifact               frontierT6ContextPrepArtifact `json:"artifact,omitempty"`
	ReasonCode             string                        `json:"reason_code,omitempty"`
	Retryable              bool                          `json:"retryable,omitempty"`
	TaskID                 string                        `json:"task_id,omitempty"`
	EffectiveProfileDigest string                        `json:"effective_profile_digest,omitempty"`
	SourceGeneration       string                        `json:"source_generation,omitempty"`
}

func (h frontierT6AgentFitHandlers) ContextPrep(w http.ResponseWriter, r *http.Request) {
	if h.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "frontier_t6_store_unavailable"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	request := frontierT6PrepHTTPRequest{}
	if err := frontierT6DecodeBody(r, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json"})
		return
	}
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	auth, ok := h.authorize(w, r, frontierT6ProactiveContextPrepFeatureID, operation)
	if !ok {
		return
	}
	scope, err := frontierT6BindAuthorizedScope(request.Scope, auth)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "workspace_override_forbidden"})
		return
	}
	if operation != "use" && !auth.AllowActivation {
		writeJSON(w, http.StatusPaymentRequired, map[string]any{"ok": false, "error": "background_prep_not_entitled", "reactive_preparation_available": true})
		return
	}
	if (operation == "claim" || operation == "complete" || operation == "fail") && !auth.AllowWorker {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "external_worker_authorization_required"})
		return
	}
	now := h.now()
	var response any
	switch operation {
	case "schedule":
		if request.Schedule == nil {
			err = errors.New("schedule payload is required")
			break
		}
		request.Schedule.Scope = scope
		request.Schedule.Approval.AuthorizationDigest = auth.AuthorizationDigest
		request.Schedule.Provenance.AuthorizationDigest = auth.AuthorizationDigest
		response, err = h.Store.scheduleContextPrep(*request.Schedule, now)
	case "claim":
		var found bool
		response, found, err = h.Store.claimContextPrep(scope, request.PrepID, request.WorkerID, now)
		if err == nil && !found {
			response = map[string]any{"claimed": false, "execution_owner": "external_cli_worker"}
		}
	case "complete":
		response, err = h.Store.completeContextPrep(request.PrepID, request.ClaimToken, request.Artifact, now)
	case "fail":
		err = h.Store.failContextPrep(request.PrepID, request.ClaimToken, request.ReasonCode, request.Retryable, now)
		response = map[string]any{"failure_recorded": err == nil}
	case "use":
		response = h.Store.useContextPrep(scope, request.PrepID, request.TaskID, request.EffectiveProfileDigest, request.SourceGeneration, auth.AuthorizationDigest, now)
	default:
		err = errors.New("unsupported context preparation operation")
	}
	if err != nil {
		frontierT6WriteHandlerError(w, err)
		return
	}
	payload := map[string]any{
		"ok": true, "schema_id": frontierT6AgentFitContractID, "operation": operation,
		"result_schema_id": frontierT6ContextPrepSchemaID, "result": response,
		"network_calls": 0, "automatic_model_execution": false,
	}
	writeJSON(w, http.StatusOK, frontierT6AttachFormatContract(frontierT6AgentFitContractID, payload, "agent_http"))
}
