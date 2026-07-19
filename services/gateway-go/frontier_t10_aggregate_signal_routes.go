package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

const (
	frontierT10MaximumRequestBytes  = 2 * 1024 * 1024
	frontierT10MaximumResponseBytes = 128 * 1024
)

type frontierT10AggregateRequest struct {
	Operation         string                             `json:"operation"`
	Metric            string                             `json:"metric,omitempty"`
	Source            string                             `json:"source,omitempty"`
	Value             *float64                           `json:"value,omitempty"`
	Category          string                             `json:"category,omitempty"`
	CohortWindow      string                             `json:"cohort_window,omitempty"`
	ContributionNonce string                             `json:"contribution_nonce,omitempty"`
	OptIn             bool                               `json:"opt_in,omitempty"`
	Epsilon           float64                            `json:"epsilon,omitempty"`
	Delta             float64                            `json:"delta,omitempty"`
	ExpiryWeeks       int                                `json:"expiry_weeks,omitempty"`
	Confirm           bool                               `json:"confirm,omitempty"`
	Contributions     []frontierT10AggregateContribution `json:"contributions,omitempty"`
}

var frontierT10ForbiddenPayloadFields = map[string]struct{}{
	"api_key": {}, "authorization": {}, "content": {}, "credential": {},
	"embedding": {}, "embeddings": {}, "exact_timestamp": {}, "file_path": {},
	"installation_id": {}, "local_path": {}, "messages": {}, "password": {},
	"private_key": {}, "project": {}, "project_name": {}, "prompt": {}, "prompts": {},
	"raw_content": {}, "raw_memory": {}, "raw_prompt": {}, "raw_text": {},
	"secret": {}, "secrets": {}, "stable_installation_id": {}, "timestamp": {},
}

func frontierT10ForbiddenPayloadPath(value any, path string) string {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(key), "-", "_"))
			next := key
			if path != "" {
				next = path + "." + key
			}
			if _, forbidden := frontierT10ForbiddenPayloadFields[normalized]; forbidden {
				return next
			}
			if found := frontierT10ForbiddenPayloadPath(child, next); found != "" {
				return found
			}
		}
	case []any:
		for index, child := range typed {
			if found := frontierT10ForbiddenPayloadPath(child, fmt.Sprintf("%s[%d]", path, index)); found != "" {
				return found
			}
		}
	}
	return ""
}

func frontierT10DecodeRequest(r *http.Request) (frontierT10AggregateRequest, error) {
	var request frontierT10AggregateRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, frontierT10MaximumRequestBytes+1))
	if err != nil {
		return request, errors.New("read Aggregate Signal request failed")
	}
	if len(raw) > frontierT10MaximumRequestBytes {
		return request, errors.New("Aggregate Signal request exceeds the input limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return request, errors.New("Aggregate Signal request body is required")
	}
	var untrusted any
	if err := json.Unmarshal(raw, &untrusted); err != nil {
		return request, errors.New("Aggregate Signal request is invalid JSON")
	}
	if path := frontierT10ForbiddenPayloadPath(untrusted, ""); path != "" {
		return request, fmt.Errorf("privacy-forbidden field %s", path)
	}
	if err := strictJSONDecode(raw, &request); err != nil {
		return request, fmt.Errorf("invalid Aggregate Signal request: %w", err)
	}
	return request, nil
}

func (s *server) frontierT10SourceStatistic(request frontierT10AggregateRequest) (*float64, string, error) {
	metric, spec, err := frontierT10NormalizeMetric(request.Metric)
	if err != nil {
		return nil, "", err
	}
	source, err := frontierT10NormalizeSource(request.Source)
	if err != nil {
		return nil, "", err
	}
	if source == "manual" {
		if spec.Kind == "numeric" {
			return request.Value, "", nil
		}
		return nil, request.Category, nil
	}
	var statistics map[string]any
	switch source {
	case "context_pack_quality":
		if s != nil && s.contextPackQuality != nil {
			statistics = s.contextPackQuality.aggregateSignalSufficientStatistics()
		}
	case "context_policy":
		if s != nil && s.contextPolicy != nil {
			statistics = s.contextPolicy.aggregateSignalSufficientStatistics()
		}
	case "context_mesh":
		if s != nil && s.contextMesh != nil {
			statistics = s.contextMesh.aggregateSignalSufficientStatistics(time.Now().UTC())
		}
	}
	raw, exists := statistics[metric]
	if !exists || raw == nil {
		return nil, "", errors.New("selected local source has no eligible statistic for metric")
	}
	if spec.Kind == "numeric" {
		value, ok := contextPolicyNumber(raw)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, "", errors.New("selected local source statistic is not finite")
		}
		return &value, "", nil
	}
	return nil, anyToString(raw), nil
}

func frontierT10PreviewCommitments(metric, window, nonce string) (string, string) {
	unit := frontierT6Digest(map[string]any{"scope": "unbound_preview", "metric": metric, "cohort_window": window})
	nonceDigest := frontierT6Digest(map[string]any{"scope": "unbound_preview", "metric": metric, "cohort_window": window, "nonce_present": strings.TrimSpace(nonce) != ""})
	return unit, nonceDigest
}

func (s *server) frontierT10BuildContribution(request frontierT10AggregateRequest, persist bool, now time.Time) (frontierT10AggregateContribution, bool, error) {
	metric, _, err := frontierT10NormalizeMetric(request.Metric)
	if err != nil {
		return frontierT10AggregateContribution{}, false, err
	}
	source, err := frontierT10NormalizeSource(request.Source)
	if err != nil {
		return frontierT10AggregateContribution{}, false, err
	}
	window, start, err := frontierT10ValidateWindow(request.CohortWindow, now, persist)
	if err != nil {
		return frontierT10AggregateContribution{}, false, err
	}
	value, category, err := s.frontierT10SourceStatistic(request)
	if err != nil {
		return frontierT10AggregateContribution{}, false, err
	}
	expiresOn := frontierT10Date(start.AddDate(0, 0, frontierT10ContributionExpiryWeeks*7))
	unitCommitment, nonceDigest := frontierT10PreviewCommitments(metric, window, request.ContributionNonce)
	if persist {
		if !request.OptIn {
			return frontierT10AggregateContribution{}, false, errors.New("queue requires explicit opt_in=true")
		}
		nonce := strings.TrimSpace(request.ContributionNonce)
		if len(nonce) < 16 || len(nonce) > 256 || strings.ContainsAny(nonce, "\x00\r\n") {
			return frontierT10AggregateContribution{}, false, errors.New("queue requires a bounded contribution_nonce")
		}
		if s == nil || s.aggregateSignal == nil || !s.aggregateSignal.enabled {
			return frontierT10AggregateContribution{}, false, errors.New("Aggregate Signal store is unavailable")
		}
		unitCommitment, nonceDigest, err = s.aggregateSignal.localCommitments(metric, window, nonce)
		if err != nil {
			return frontierT10AggregateContribution{}, false, err
		}
	}
	contribution, err := frontierT10BuildContribution(metric, source, window, unitCommitment, nonceDigest, value, category, expiresOn)
	if err != nil {
		return frontierT10AggregateContribution{}, false, err
	}
	if !persist {
		return contribution, false, nil
	}
	return s.aggregateSignal.queue(contribution, now)
}

func frontierT10ContractPassed(payload map[string]any) bool {
	return anyToString(anyMap(anyMap(payload["format_contract"])["validation"])["status"]) == "passed"
}

func frontierT10WritePayload(w http.ResponseWriter, status int, payload map[string]any) {
	raw, _ := json.Marshal(payload)
	if len(raw) > frontierT10MaximumResponseBytes || !frontierT10ContractPassed(payload) {
		validation := anyMap(anyMap(payload["format_contract"])["validation"])
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "error": "aggregate_signal_contract_failed", "response_bytes": len(raw),
			"detail": validation["errors"],
		})
		return
	}
	writeJSON(w, status, payload)
}

func frontierT10WriteRequestError(w http.ResponseWriter, err error) {
	status, code := http.StatusUnprocessableEntity, "aggregate_signal_request_rejected"
	if errors.Is(err, errFrontierT10Replay) || errors.Is(err, errFrontierT10Differencing) {
		status, code = http.StatusConflict, "aggregate_signal_replay_or_differencing_rejected"
	} else if errors.Is(err, errFrontierT10Budget) {
		status, code = http.StatusTooManyRequests, "aggregate_signal_privacy_budget_exhausted"
	} else if strings.Contains(strings.ToLower(err.Error()), "store is unavailable") || strings.Contains(strings.ToLower(err.Error()), "persist") {
		status, code = http.StatusServiceUnavailable, "aggregate_signal_store_unavailable"
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": code, "detail": clipText(err.Error(), 300)})
}

func (s *server) frontierT10Report(request frontierT10AggregateRequest, now time.Time) (map[string]any, error) {
	metric, spec, err := frontierT10NormalizeMetric(request.Metric)
	if err != nil {
		return nil, err
	}
	window, _, err := frontierT10ValidateWindow(request.CohortWindow, now, false)
	if err != nil {
		return nil, err
	}
	contributions := append([]frontierT10AggregateContribution(nil), request.Contributions...)
	if len(contributions) == 0 && s != nil && s.aggregateSignal != nil {
		contributions = s.aggregateSignal.localContributions(metric, window, now)
	}
	if _, err := frontierT10ValidateReleaseInputs(metric, window, contributions, now); err != nil {
		return nil, err
	}
	if len(contributions) < frontierT10MinimumCohort {
		return frontierT10SuppressedReport(metric, spec.Kind, window, s.aggregateSignal.accountingSnapshot(now), now), nil
	}
	epsilon := request.Epsilon
	if epsilon == 0 {
		epsilon = frontierT10DefaultReleaseEpsilon
	}
	delta := request.Delta
	if delta == 0 {
		delta = frontierT10DefaultReleaseDelta
	}
	if s == nil || s.aggregateSignal == nil {
		return nil, errors.New("Aggregate Signal store is unavailable")
	}
	record, replayed, err := s.aggregateSignal.release(metric, window, contributions, epsilon, delta, request.ExpiryWeeks, now)
	if err != nil {
		return nil, err
	}
	return frontierT10ReportResponse(record, replayed), nil
}

func (s *server) memoryAggregateSignal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	now := time.Now().UTC()
	if r.Method == http.MethodGet {
		frontierT10WritePayload(w, http.StatusOK, s.aggregateSignal.statusPayload("status", now))
		return
	}
	request, err := frontierT10DecodeRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_aggregate_signal_request", "detail": clipText(err.Error(), 300)})
		return
	}
	operation := strings.ToLower(strings.TrimSpace(request.Operation))
	switch operation {
	case "status":
		frontierT10WritePayload(w, http.StatusOK, s.aggregateSignal.statusPayload(operation, now))
	case "preview", "queue":
		contribution, replayed, err := s.frontierT10BuildContribution(request, operation == "queue", now)
		if err != nil {
			frontierT10WriteRequestError(w, err)
			return
		}
		decision := "preview"
		if operation == "queue" {
			decision = "queued"
		}
		frontierT10WritePayload(w, http.StatusOK, frontierT10ContributionResponse(operation, decision, contribution, operation == "queue", replayed))
	case "report":
		payload, err := s.frontierT10Report(request, now)
		if err != nil {
			frontierT10WriteRequestError(w, err)
			return
		}
		frontierT10WritePayload(w, http.StatusOK, payload)
	case "opt-out":
		if !request.Confirm {
			frontierT10WriteRequestError(w, errors.New("opt-out requires confirm=true"))
			return
		}
		if s.aggregateSignal == nil {
			frontierT10WriteRequestError(w, errors.New("Aggregate Signal store is unavailable"))
			return
		}
		receipt, deleted, err := s.aggregateSignal.optOut(now)
		if err != nil {
			frontierT10WriteRequestError(w, err)
			return
		}
		payload := s.aggregateSignal.statusPayload(operation, now)
		payload["opt_out"] = map[string]any{
			"receipt": receipt, "queued_contributions_deleted": deleted,
			"released_aggregate_subtraction_claimed": false, "future_contribution_stopped": true,
		}
		payload = attachPayloadFormatContract(frontierT10AccountantContractID, payload, "", "aggregate_signal", frontierT10AggregatePath)
		frontierT10WritePayload(w, http.StatusOK, payload)
	default:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "unsupported_aggregate_signal_operation"})
	}
}
