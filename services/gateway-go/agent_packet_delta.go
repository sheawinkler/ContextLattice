package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	agentPacketIdentitySchemaID          = "agent_packet_identity.v1"
	agentPacketDeltaContractID           = agentPacketDeltaOutputContractID
	agentPacketReconstructionContractID  = agentPacketReconstructionOutputContractID
	agentPacketDeltaFeatureEnv           = "CONTEXTLATTICE_AGENT_PACKET_DELTA_ENABLED"
	defaultAgentPacketTTLSeconds         = 24 * 60 * 60
	maximumAgentPacketTTLSeconds         = 7 * 24 * 60 * 60
	agentPacketReconstructionRoute       = "/memory/agent-packet/reconstruct"
	agentPacketDigestPrefix              = "sha256:"
	agentPacketPacketIDPrefix            = "packet_"
	agentPacketLineageIDPrefix           = "packet_lineage_"
	agentPacketAcknowledgementIDPrefix   = "ack_"
	agentPacketIdentityAckVersion        = 1
	agentPacketPlaceholderDigestHexChars = 64
	maxAgentPacketDeltaOperations        = 64
	maxAgentPacketDeltaDepth             = 6
	maxAgentPacketReconstructionBody     = 128 << 10
)

var agentPacketModelVisibleFields = []string{
	"ok", "schema_id", "version", "surface", "query", "project", "topic_path",
	"session_id", "agent_id", "task_id", "task_identity_id", "execution_lane_id",
	"prompt", "evidence", "provenance", "uncertainty", "decision_gate", "next_actions",
	"continuation", "outcome", "writeback_required", "warnings", "session", "session_rollup",
}

type agentPacketBaseValidation struct {
	Packet   map[string]any
	Identity map[string]any
	Reason   string
}

type agentPacketReconstructionError struct {
	Code string
	Err  error
}

func (e *agentPacketReconstructionError) Error() string {
	if e == nil || e.Err == nil {
		return "agent packet reconstruction failed"
	}
	return e.Err.Error()
}

func (e *agentPacketReconstructionError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func agentPacketDeltaRequested(request map[string]any) bool {
	mode := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(
		anyToString(request["packet_mode"]),
		anyToString(request["packetMode"]),
		anyToString(request["delivery_mode"]),
		anyToString(request["deliveryMode"]),
	)))
	return mode == "delta" || mode == agentPacketDeltaContractID
}

func agentPacketDeltaEnabled() bool {
	return envBool(agentPacketDeltaFeatureEnv, true)
}

func agentPacketEndpointForSurface(surface string) string {
	switch strings.TrimSpace(surface) {
	case "context_pack":
		return "/memory/context-pack"
	case "synthesis_pack":
		return "/memory/synthesis-pack"
	case "synthesis_pack_v2":
		return "/memory/synthesis-pack/v2"
	case "tools_context_pack":
		return "/tools/context_pack"
	case "tools_synthesis_pack":
		return "/tools/synthesis_pack"
	case "tools_synthesis_pack_v2":
		return "/tools/synthesis_pack_v2"
	default:
		return "/memory/context-pack"
	}
}

func agentPacketTTL(request map[string]any) time.Duration {
	seconds := anyToInt(firstNonEmptyAny(
		request["packet_ttl_seconds"],
		request["packetTTLSeconds"],
	), envInt("CONTEXTLATTICE_AGENT_PACKET_TTL_SECONDS", defaultAgentPacketTTLSeconds))
	seconds = clampInt(seconds, 60, maximumAgentPacketTTLSeconds)
	return time.Duration(seconds) * time.Second
}

func deepCloneAgentPacketMap(input map[string]any) (map[string]any, error) {
	if input == nil {
		return map[string]any{}, nil
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func canonicalAgentPacketDigest(value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return agentPacketDigestPrefix + hex.EncodeToString(digest[:]), nil
}

func agentPacketModelVisibleProjection(packet map[string]any) map[string]any {
	out := make(map[string]any, len(agentPacketModelVisibleFields))
	for _, field := range agentPacketModelVisibleFields {
		if value, ok := packet[field]; ok {
			out[field] = value
		}
	}
	return out
}

func agentPacketTransportProjection(packet map[string]any) (map[string]any, error) {
	out, err := deepCloneAgentPacketMap(packet)
	if err != nil {
		return nil, err
	}
	delete(out, "format_contract")
	delete(out, "packet_identity")
	delete(out, "delta_fallback")
	// Accounting is recomputed after reconstruction and cannot participate in
	// the digest without creating a packet-id/token-count fixed-point cycle.
	delete(out, "token_budget")
	delete(out, "token_impact")
	return out, nil
}

func agentPacketAccountingProjection(packet map[string]any) map[string]any {
	tokenBudget := anyMap(packet["token_budget"])
	tokenImpact := anyMap(packet["token_impact"])
	budget := map[string]any{
		"target_tokens":     anyToInt(tokenBudget["target_tokens"], defaultAgentPacketTargetTokens),
		"hard_limit_tokens": anyToInt(tokenBudget["hard_limit_tokens"], defaultAgentPacketHardTokens),
	}
	for _, field := range []string{"requested_hard_limit_tokens", "hard_limit_adjusted", "adjustment_reason"} {
		if value, ok := tokenBudget[field]; ok {
			budget[field] = value
		}
	}
	impact := map[string]any{
		"baseline_tokens_estimate":        anyToInt(tokenImpact["baseline_tokens_estimate"], 0),
		"compiled_prompt_tokens_estimate": anyToInt(tokenImpact["compiled_prompt_tokens_estimate"], 0),
	}
	return map[string]any{
		"token_budget": budget,
		"token_impact": impact,
	}
}

func agentPacketAccountingDigest(packet map[string]any) (string, error) {
	return canonicalAgentPacketDigest(agentPacketAccountingProjection(packet))
}

func agentPacketFinalAccounting(packet map[string]any) (map[string]any, error) {
	accounting := map[string]any{
		"token_budget": cloneAnyMap(anyMap(packet["token_budget"])),
		"token_impact": cloneAnyMap(anyMap(packet["token_impact"])),
	}
	digest, err := canonicalAgentPacketDigest(accounting)
	if err != nil {
		return nil, err
	}
	accounting["digest"] = digest
	return accounting, nil
}

func agentPacketScopeProjection(packet map[string]any) map[string]any {
	return map[string]any{
		"schema_id":         agentPacketContractID,
		"surface":           strings.TrimSpace(anyToString(packet["surface"])),
		"project":           strings.TrimSpace(anyToString(packet["project"])),
		"topic_path":        strings.Trim(strings.TrimSpace(anyToString(packet["topic_path"])), "/"),
		"session_id":        strings.TrimSpace(anyToString(packet["session_id"])),
		"agent_id":          strings.TrimSpace(anyToString(packet["agent_id"])),
		"task_id":           strings.TrimSpace(anyToString(packet["task_id"])),
		"task_identity_id":  strings.TrimSpace(anyToString(packet["task_identity_id"])),
		"execution_lane_id": strings.TrimSpace(anyToString(packet["execution_lane_id"])),
	}
}

func agentPacketDigestParts(packet map[string]any) (modelVisibleDigest, transportDigest, scopeDigest string, err error) {
	modelVisibleDigest, err = canonicalAgentPacketDigest(agentPacketModelVisibleProjection(packet))
	if err != nil {
		return "", "", "", err
	}
	transport, transportErr := agentPacketTransportProjection(packet)
	if transportErr != nil {
		return "", "", "", transportErr
	}
	transportDigest, err = canonicalAgentPacketDigest(transport)
	if err != nil {
		return "", "", "", err
	}
	scopeDigest, err = canonicalAgentPacketDigest(agentPacketScopeProjection(packet))
	return modelVisibleDigest, transportDigest, scopeDigest, err
}

func agentPacketDigestSuffix(digest string, length int) string {
	value := strings.TrimPrefix(strings.TrimSpace(digest), agentPacketDigestPrefix)
	if length < 1 || len(value) <= length {
		return value
	}
	return value[:length]
}

func agentPacketAckCursor(identity map[string]any) string {
	// This cursor detects accidental drift in a caller-retained packet. It is a
	// self-verifying content digest, not an authentication signature.
	payload := map[string]any{
		"schema_id":            agentPacketIdentitySchemaID,
		"version":              anyToInt(identity["version"], 0),
		"ack_version":          anyToInt(identity["ack_version"], 0),
		"lineage_id":           anyToString(identity["lineage_id"]),
		"packet_id":            anyToString(identity["packet_id"]),
		"revision":             anyToInt(identity["revision"], 0),
		"base_packet_id":       anyToString(identity["base_packet_id"]),
		"base_digest":          anyToString(identity["base_digest"]),
		"model_visible_digest": anyToString(identity["model_visible_digest"]),
		"transport_digest":     anyToString(identity["transport_digest"]),
		"scope_digest":         anyToString(identity["scope_digest"]),
		"accounting_digest":    anyToString(identity["accounting_digest"]),
		"issued_at":            anyToString(identity["issued_at"]),
		"expires_at":           anyToString(identity["expires_at"]),
	}
	digest, err := canonicalAgentPacketDigest(payload)
	if err != nil {
		return ""
	}
	return agentPacketAcknowledgementIDPrefix + agentPacketDigestSuffix(digest, 32)
}

func agentPacketPlaceholderDigest() string {
	return agentPacketDigestPrefix + strings.Repeat("0", agentPacketPlaceholderDigestHexChars)
}

func agentPacketIdentitySeed(packet map[string]any, baseIdentity map[string]any, request map[string]any, now time.Time) map[string]any {
	now = now.UTC()
	issuedAt := now.Format(time.RFC3339Nano)
	expiresAt := now.Add(agentPacketTTL(request)).Format(time.RFC3339Nano)
	revision := 1
	lineageID := agentPacketLineageIDPrefix + strings.Repeat("0", 24)
	basePacketID := ""
	baseDigest := ""
	if len(baseIdentity) > 0 {
		revision = maxInt(1, anyToInt(baseIdentity["revision"], 0)+1)
		lineageID = anyToString(baseIdentity["lineage_id"])
		basePacketID = anyToString(baseIdentity["packet_id"])
		baseDigest = anyToString(baseIdentity["transport_digest"])
	} else if existing := anyMap(packet["packet_identity"]); anyToString(existing["schema_id"]) == agentPacketIdentitySchemaID {
		revision = maxInt(1, anyToInt(existing["revision"], 1))
		lineageID = firstNonEmptyStrings(anyToString(existing["lineage_id"]), lineageID)
		basePacketID = anyToString(existing["base_packet_id"])
		baseDigest = anyToString(existing["base_digest"])
		issuedAt = firstNonEmptyStrings(anyToString(existing["issued_at"]), issuedAt)
		expiresAt = firstNonEmptyStrings(anyToString(existing["expires_at"]), expiresAt)
	}
	return map[string]any{
		"schema_id":            agentPacketIdentitySchemaID,
		"version":              1,
		"ack_version":          agentPacketIdentityAckVersion,
		"lineage_id":           lineageID,
		"packet_id":            agentPacketPacketIDPrefix + strings.Repeat("0", 24),
		"revision":             revision,
		"base_packet_id":       basePacketID,
		"base_digest":          baseDigest,
		"model_visible_digest": agentPacketPlaceholderDigest(),
		"transport_digest":     agentPacketPlaceholderDigest(),
		"scope_digest":         agentPacketPlaceholderDigest(),
		"accounting_digest":    agentPacketPlaceholderDigest(),
		"issued_at":            issuedAt,
		"expires_at":           expiresAt,
		"ack_cursor":           agentPacketAcknowledgementIDPrefix + strings.Repeat("0", 32),
	}
}

func sealAgentPacketIdentity(packet map[string]any, seed map[string]any) error {
	modelVisibleDigest, transportDigest, scopeDigest, err := agentPacketDigestParts(packet)
	if err != nil {
		return err
	}
	accountingDigest, err := agentPacketAccountingDigest(packet)
	if err != nil {
		return err
	}
	identity := map[string]any{
		"schema_id":            agentPacketIdentitySchemaID,
		"version":              1,
		"ack_version":          agentPacketIdentityAckVersion,
		"lineage_id":           agentPacketLineageIDPrefix + agentPacketDigestSuffix(scopeDigest, 24),
		"packet_id":            agentPacketPacketIDPrefix + agentPacketDigestSuffix(transportDigest, 24),
		"revision":             maxInt(1, anyToInt(seed["revision"], 1)),
		"base_packet_id":       anyToString(seed["base_packet_id"]),
		"base_digest":          anyToString(seed["base_digest"]),
		"model_visible_digest": modelVisibleDigest,
		"transport_digest":     transportDigest,
		"scope_digest":         scopeDigest,
		"accounting_digest":    accountingDigest,
		"issued_at":            anyToString(seed["issued_at"]),
		"expires_at":           anyToString(seed["expires_at"]),
	}
	if inherited := strings.TrimSpace(anyToString(seed["lineage_id"])); inherited != "" && !strings.HasSuffix(inherited, strings.Repeat("0", 24)) {
		identity["lineage_id"] = inherited
	}
	identity["ack_cursor"] = agentPacketAckCursor(identity)
	packet["packet_identity"] = identity
	return nil
}

func finalizeAgentPacketWithIdentity(packet map[string]any, baseIdentity map[string]any, request map[string]any, now time.Time) map[string]any {
	seed := agentPacketIdentitySeed(packet, baseIdentity, request, now)
	packet["packet_identity"] = seed
	// Sealing changes fixed-width identity content, so accounting must converge
	// against the final sealed wire packet rather than the pre-seal placeholder.
	// Most packets settle in two passes; the bounded retries handle numeric
	// tokenization and format-byte fixed points exposed by different platforms.
	for pass := 0; pass < 6; pass++ {
		packet = finalizeAgentPacketCore(packet)
		if err := sealAgentPacketIdentity(packet, seed); err != nil {
			packet["warnings"] = append(contextPackAnyList(packet["warnings"]), "Packet identity could not be sealed; do not use this packet as a delta base.")
			break
		}
		packet = attachAgentPacketFormatContract(packet)
		if validateAgentPacketTransportAccounting(packet) == "" {
			return packet
		}
	}
	return attachAgentPacketFormatContract(packet)
}

func validateAgentPacketTransportAccounting(packet map[string]any) string {
	count := contextPackCountAnyTokens(packet)
	if !count.TokenizerExact {
		return "base_tokenizer_inexact"
	}
	budget := anyMap(packet["token_budget"])
	impact := anyMap(packet["token_impact"])
	actual := anyToInt(budget["actual_tokens"], 0)
	hardLimit := anyToInt(budget["hard_limit_tokens"], 0)
	baseline := anyToInt(impact["baseline_tokens_estimate"], 0)
	expectedNet := baseline - count.Tokens
	expectedSaved := maxInt(0, expectedNet)
	expectedRatio := roundFloat(float64(baseline)/float64(maxInt(count.Tokens, 1)), 3)
	budgetExact := anyToBool(budget["tokenizer_exact"]) || anyToString(budget["calibration_grade"]) == "tokenizer_exact"
	if actual != count.Tokens || hardLimit < 1 || anyToBool(budget["within_hard_limit"]) != (count.Tokens <= hardLimit) ||
		anyToString(budget["estimate_method"]) != count.Method || anyToString(budget["calibration_grade"]) != count.CalibrationGrade ||
		!budgetExact || anyToString(budget["tokenizer_encoding"]) != count.Encoding ||
		anyToInt(impact["packed_tokens_estimate"], 0) != count.Tokens || anyToInt(impact["transport_tokens_exact"], 0) != count.Tokens ||
		anyToInt(impact["saved_tokens_estimate"], -1) != expectedSaved || anyToInt(impact["net_token_delta"], 0) != expectedNet ||
		anyToFloat(impact["compression_ratio"]) != expectedRatio || !anyToBool(impact["transport_inclusive"]) ||
		anyToString(impact["estimate_method"]) != count.Method || anyToString(impact["calibration_grade"]) != count.CalibrationGrade ||
		!anyToBool(impact["tokenizer_exact"]) || anyToString(impact["tokenizer_encoding"]) != count.Encoding {
		return "base_accounting_mismatch"
	}
	if targetMet, exists := budget["target_met"]; exists && anyToBool(targetMet) != (count.Tokens <= anyToInt(budget["target_tokens"], 0)) {
		return "base_accounting_mismatch"
	}
	return ""
}

func validateAgentPacketSelf(packet map[string]any, now time.Time, checkExpiry bool) (map[string]any, string) {
	if len(packet) == 0 {
		return nil, "base_packet_missing"
	}
	if anyToString(packet["schema_id"]) != agentPacketContractID {
		return nil, "base_schema_mismatch"
	}
	if findings := validateAgentContractPayload(agentPacketContractID, packet); len(findings) > 0 {
		return nil, "base_contract_invalid"
	}
	identity := anyMap(packet["packet_identity"])
	for _, field := range []string{
		"schema_id", "version", "ack_version", "lineage_id", "packet_id", "revision", "base_packet_id", "base_digest",
		"model_visible_digest", "transport_digest", "scope_digest", "accounting_digest", "issued_at", "expires_at", "ack_cursor",
	} {
		if _, exists := identity[field]; !exists {
			return nil, "base_identity_missing"
		}
	}
	if anyToString(identity["schema_id"]) != agentPacketIdentitySchemaID || anyToInt(identity["version"], 0) != 1 ||
		anyToInt(identity["ack_version"], 0) != agentPacketIdentityAckVersion || anyToInt(identity["revision"], 0) < 1 {
		return nil, "base_identity_missing"
	}
	modelVisibleDigest, transportDigest, scopeDigest, err := agentPacketDigestParts(packet)
	if err != nil {
		return nil, "base_digest_unavailable"
	}
	if anyToString(identity["model_visible_digest"]) != modelVisibleDigest || anyToString(identity["transport_digest"]) != transportDigest {
		return nil, "base_digest_mismatch"
	}
	if anyToString(identity["scope_digest"]) != scopeDigest {
		return nil, "base_scope_digest_mismatch"
	}
	accountingDigest, accountingErr := agentPacketAccountingDigest(packet)
	if accountingErr != nil || anyToString(identity["accounting_digest"]) != accountingDigest {
		return nil, "base_accounting_digest_mismatch"
	}
	if anyToString(identity["packet_id"]) != agentPacketPacketIDPrefix+agentPacketDigestSuffix(transportDigest, 24) {
		return nil, "base_packet_id_mismatch"
	}
	if anyToString(identity["lineage_id"]) != agentPacketLineageIDPrefix+agentPacketDigestSuffix(scopeDigest, 24) {
		return nil, "base_lineage_id_mismatch"
	}
	if anyToString(identity["ack_cursor"]) != agentPacketAckCursor(identity) {
		return nil, "base_ack_cursor_mismatch"
	}
	issuedAt, issuedErr := time.Parse(time.RFC3339Nano, anyToString(identity["issued_at"]))
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, anyToString(identity["expires_at"]))
	now = now.UTC()
	if issuedErr != nil || expiresErr != nil || issuedAt.After(now) || !expiresAt.After(issuedAt) ||
		expiresAt.Sub(issuedAt) > time.Duration(maximumAgentPacketTTLSeconds)*time.Second {
		return nil, "base_validity_window_invalid"
	}
	if checkExpiry && !now.Before(expiresAt) {
		return nil, "base_expired"
	}
	if reason := validateAgentPacketTransportAccounting(packet); reason != "" {
		return nil, reason
	}
	return identity, ""
}

func agentPacketBaseFromRequest(request map[string]any) map[string]any {
	if base := anyMap(request["base_packet"]); len(base) > 0 {
		return base
	}
	if base := anyMap(request["basePacket"]); len(base) > 0 {
		return base
	}
	delta := anyMap(firstNonEmptyAny(request["packet_delta"], request["packetDelta"]))
	return anyMap(firstNonEmptyAny(delta["base_packet"], delta["basePacket"]))
}

func validateAgentPacketBase(request map[string]any, target map[string]any, now time.Time) agentPacketBaseValidation {
	base := agentPacketBaseFromRequest(request)
	identity, reason := validateAgentPacketSelf(base, now, true)
	if reason != "" {
		return agentPacketBaseValidation{Packet: base, Identity: identity, Reason: reason}
	}
	requestedPacketID := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["base_packet_id"]), anyToString(request["basePacketId"])))
	if requestedPacketID != "" && requestedPacketID != anyToString(identity["packet_id"]) {
		return agentPacketBaseValidation{Packet: base, Identity: identity, Reason: "base_request_packet_id_mismatch"}
	}
	requestedDigest := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["base_digest"]), anyToString(request["baseDigest"])))
	if requestedDigest != "" && requestedDigest != anyToString(identity["transport_digest"]) {
		return agentPacketBaseValidation{Packet: base, Identity: identity, Reason: "base_request_digest_mismatch"}
	}
	requestedRevision := anyToInt(firstNonEmptyAny(request["base_revision"], request["baseRevision"]), 0)
	if requestedRevision > 0 && requestedRevision != anyToInt(identity["revision"], 0) {
		return agentPacketBaseValidation{Packet: base, Identity: identity, Reason: "base_revision_mismatch"}
	}
	requestedAck := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["base_ack_cursor"]), anyToString(request["baseAckCursor"]), anyToString(request["ack_cursor"])))
	if requestedAck != "" && requestedAck != anyToString(identity["ack_cursor"]) {
		return agentPacketBaseValidation{Packet: base, Identity: identity, Reason: "base_ack_cursor_mismatch"}
	}
	targetScopeDigest, err := canonicalAgentPacketDigest(agentPacketScopeProjection(target))
	if err != nil || targetScopeDigest != anyToString(identity["scope_digest"]) {
		return agentPacketBaseValidation{Packet: base, Identity: identity, Reason: "base_scope_mismatch"}
	}
	return agentPacketBaseValidation{Packet: base, Identity: identity}
}

func agentPacketJSONEqual(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func escapeAgentPacketJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func parseAgentPacketJSONPointer(value string) ([]string, error) {
	if !strings.HasPrefix(value, "/") || len(value) > 512 {
		return nil, errors.New("invalid JSON Pointer path")
	}
	rawParts := strings.Split(strings.TrimPrefix(value, "/"), "/")
	if len(rawParts) == 0 || len(rawParts) > maxAgentPacketDeltaDepth+1 {
		return nil, errors.New("JSON Pointer depth exceeds the packet delta limit")
	}
	parts := make([]string, 0, len(rawParts))
	for _, raw := range rawParts {
		if raw == "" {
			return nil, errors.New("empty JSON Pointer segment")
		}
		for index := 0; index < len(raw); index++ {
			if raw[index] == '~' && (index+1 >= len(raw) || (raw[index+1] != '0' && raw[index+1] != '1')) {
				return nil, errors.New("invalid JSON Pointer escape")
			}
		}
		parts = append(parts, strings.ReplaceAll(strings.ReplaceAll(raw, "~1", "/"), "~0", "~"))
	}
	return parts, nil
}

func exactAgentPacketOperationSequence(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, typed > 0
	case int64:
		return int(typed), typed > 0 && typed <= int64(maxAgentPacketDeltaOperations)
	case float64:
		if typed < 1 || typed > maxAgentPacketDeltaOperations || typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil || parsed < 1 || parsed > int64(maxAgentPacketDeltaOperations) {
			return 0, false
		}
		return int(parsed), true
	default:
		return 0, false
	}
}

func agentPacketPathIsAncestor(left, right []string) bool {
	if len(left) >= len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func agentPacketDeltaOperationFindings(delta map[string]any) []map[string]any {
	finding := func(reason, code, path string) []map[string]any {
		return []map[string]any{{
			"reason": reason, "code": code, "path": path, "contract_id": agentPacketDeltaContractID,
		}}
	}
	operations, ok := delta["operations"].([]any)
	if !ok {
		return finding("operation_list_type_invalid", "operation_kind_invalid", "operations")
	}
	if len(operations) > maxAgentPacketDeltaOperations {
		return finding("operation_limit_exceeded", "operation_limit_exceeded", "operations")
	}
	previousPath := ""
	parsedPaths := make([][]string, 0, len(operations))
	expectedTombstones := make([]string, 0, len(operations))
	for index, raw := range operations {
		operation, ok := raw.(map[string]any)
		if !ok {
			return finding("operation_object_required", "operation_kind_invalid", fmt.Sprintf("operations.%d", index))
		}
		sequence, sequenceOK := exactAgentPacketOperationSequence(operation["sequence"])
		kind, kindOK := operation["op"].(string)
		path, pathOK := operation["path"].(string)
		if !sequenceOK || sequence != index+1 {
			return finding("operation_sequence_invalid", "operation_sequence_invalid", fmt.Sprintf("operations.%d.sequence", index))
		}
		if !kindOK || (kind != "add" && kind != "replace" && kind != "remove") {
			return finding("operation_kind_invalid", "operation_kind_invalid", fmt.Sprintf("operations.%d.op", index))
		}
		if !pathOK || path == "" || (previousPath != "" && path <= previousPath) {
			return finding("operation_sequence_invalid", "operation_sequence_invalid", fmt.Sprintf("operations.%d.path", index))
		}
		segments, pathErr := parseAgentPacketJSONPointer(path)
		if pathErr != nil || len(segments) == 0 || containsString([]string{"format_contract", "packet_identity", "delta_fallback", "token_budget", "token_impact"}, segments[0]) {
			return finding("operation_path_invalid", "operation_path_invalid", path)
		}
		for _, prior := range parsedPaths {
			if agentPacketPathIsAncestor(prior, segments) || agentPacketPathIsAncestor(segments, prior) {
				return finding("operation_paths_overlap", "operation_path_overlap", path)
			}
		}
		parsedPaths = append(parsedPaths, segments)
		previousPath = path

		allowedKeys := map[string]bool{"sequence": true, "op": true, "path": true}
		if kind == "remove" {
			allowedKeys["tombstone"] = true
			tombstone, tombstoneOK := operation["tombstone"].(bool)
			if !tombstoneOK || !tombstone {
				return finding("operation_tombstone_invalid", "operation_kind_invalid", path)
			}
			expectedTombstones = append(expectedTombstones, path)
		} else {
			allowedKeys["value"] = true
			if _, exists := operation["value"]; !exists {
				return finding("operation_value_missing", "operation_kind_invalid", path)
			}
		}
		if len(operation) != len(allowedKeys) {
			return finding("operation_keys_noncanonical", "operation_kind_invalid", path)
		}
		for key := range operation {
			if !allowedKeys[key] {
				return finding("operation_keys_noncanonical", "operation_kind_invalid", path+"."+key)
			}
		}
	}
	rawTombstones, ok := delta["tombstones"].([]any)
	if !ok || len(rawTombstones) != len(expectedTombstones) {
		return finding("tombstone_manifest_mismatch", "tombstone_manifest_mismatch", "tombstones")
	}
	for index, raw := range rawTombstones {
		value, ok := raw.(string)
		if !ok || value != expectedTombstones[index] {
			return finding("tombstone_manifest_mismatch", "tombstone_manifest_mismatch", fmt.Sprintf("tombstones.%d", index))
		}
	}
	return nil
}

func agentPacketDeltaOperation(kind, path string, value any) map[string]any {
	operation := map[string]any{"op": kind, "path": path}
	if kind == "remove" {
		operation["tombstone"] = true
	} else {
		operation["value"] = value
	}
	return operation
}

func diffAgentPacketValue(path string, base, target any, depth int) []map[string]any {
	if agentPacketJSONEqual(base, target) {
		return nil
	}
	if depth >= maxAgentPacketDeltaDepth {
		return []map[string]any{agentPacketDeltaOperation("replace", path, target)}
	}
	baseMap, baseIsMap := base.(map[string]any)
	targetMap, targetIsMap := target.(map[string]any)
	if baseIsMap && targetIsMap {
		keySet := map[string]struct{}{}
		for key := range baseMap {
			keySet[key] = struct{}{}
		}
		for key := range targetMap {
			keySet[key] = struct{}{}
		}
		keys := make([]string, 0, len(keySet))
		for key := range keySet {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		operations := []map[string]any{}
		for _, key := range keys {
			baseValue, baseExists := baseMap[key]
			targetValue, targetExists := targetMap[key]
			childPath := path + "/" + escapeAgentPacketJSONPointer(key)
			switch {
			case !targetExists:
				operations = append(operations, agentPacketDeltaOperation("remove", childPath, nil))
			case !baseExists:
				operations = append(operations, agentPacketDeltaOperation("add", childPath, targetValue))
			default:
				operations = append(operations, diffAgentPacketValue(childPath, baseValue, targetValue, depth+1)...)
			}
			if len(operations) > maxAgentPacketDeltaOperations {
				return []map[string]any{agentPacketDeltaOperation("replace", path, target)}
			}
		}
		return operations
	}
	baseList, baseIsList := base.([]any)
	targetList, targetIsList := target.([]any)
	if baseIsList && targetIsList {
		if len(baseList) == len(targetList) {
			operations := []map[string]any{}
			for index := range baseList {
				childPath := path + "/" + strconv.Itoa(index)
				operations = append(operations, diffAgentPacketValue(childPath, baseList[index], targetList[index], depth+1)...)
				if len(operations) > maxAgentPacketDeltaOperations {
					return []map[string]any{agentPacketDeltaOperation("replace", path, target)}
				}
			}
			return operations
		}
		if len(targetList) == len(baseList)+1 && agentPacketJSONEqual(baseList, targetList[:len(baseList)]) {
			return []map[string]any{agentPacketDeltaOperation("add", path+"/"+strconv.Itoa(len(baseList)), targetList[len(baseList)])}
		}
	}
	return []map[string]any{agentPacketDeltaOperation("replace", path, target)}
}

func buildAgentPacketOperations(base, target map[string]any) ([]any, []any, error) {
	baseProjection, err := agentPacketTransportProjection(base)
	if err != nil {
		return nil, nil, err
	}
	targetProjection, err := agentPacketTransportProjection(target)
	if err != nil {
		return nil, nil, err
	}
	keySet := map[string]struct{}{}
	for key := range baseProjection {
		keySet[key] = struct{}{}
	}
	for key := range targetProjection {
		keySet[key] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	operationMaps := []map[string]any{}
	for _, key := range keys {
		baseValue, baseExists := baseProjection[key]
		targetValue, targetExists := targetProjection[key]
		path := "/" + escapeAgentPacketJSONPointer(key)
		switch {
		case !targetExists:
			operationMaps = append(operationMaps, agentPacketDeltaOperation("remove", path, nil))
		case !baseExists:
			operationMaps = append(operationMaps, agentPacketDeltaOperation("add", path, targetValue))
		default:
			fieldOperations := diffAgentPacketValue(path, baseValue, targetValue, 1)
			if len(operationMaps)+len(fieldOperations) > maxAgentPacketDeltaOperations {
				fieldOperations = []map[string]any{agentPacketDeltaOperation("replace", path, targetValue)}
			}
			operationMaps = append(operationMaps, fieldOperations...)
		}
	}
	if len(operationMaps) > maxAgentPacketDeltaOperations {
		return nil, nil, fmt.Errorf("packet delta exceeds %d operations", maxAgentPacketDeltaOperations)
	}
	sort.Slice(operationMaps, func(i, j int) bool {
		return anyToString(operationMaps[i]["path"]) < anyToString(operationMaps[j]["path"])
	})
	operations := make([]any, 0, len(operationMaps))
	tombstones := []any{}
	for index, operation := range operationMaps {
		operation["sequence"] = index + 1
		operations = append(operations, operation)
		if anyToString(operation["op"]) == "remove" {
			tombstones = append(tombstones, operation["path"])
		}
	}
	return operations, tombstones, nil
}

func applyAgentPacketDeltaTokenMetadata(delta map[string]any, fullCount, modelVisibleCount, incrementalModelVisibleCount, deltaCount tokenCountResult) {
	allExact := fullCount.TokenizerExact && modelVisibleCount.TokenizerExact && incrementalModelVisibleCount.TokenizerExact && deltaCount.TokenizerExact
	net := fullCount.Tokens - deltaCount.Tokens
	saved := maxInt(0, net)
	if !allExact {
		net = 0
		saved = 0
	}
	ratio := 1.0
	if deltaCount.Tokens > 0 {
		ratio = roundFloat(float64(fullCount.Tokens)/float64(deltaCount.Tokens), 3)
	}
	delta["token_budget"] = map[string]any{
		"full_packet_tokens_exact":                 fullCount.Tokens,
		"delta_wire_tokens_exact":                  deltaCount.Tokens,
		"incremental_model_visible_tokens_exact":   incrementalModelVisibleCount.Tokens,
		"reconstructed_model_visible_tokens_exact": modelVisibleCount.Tokens,
		"tokens_saved_exact":                       saved,
		"reduction_fraction":                       roundFloat(float64(saved)/float64(maxInt(fullCount.Tokens, 1)), 6),
		"delta_smaller_than_full":                  allExact && deltaCount.Tokens < fullCount.Tokens,
		"equal_reconstructed_context":              true,
		"estimate_method":                          deltaCount.Method,
		"calibration_grade":                        deltaCount.CalibrationGrade,
		"tokenizer_exact":                          allExact,
		"tokenizer_encoding":                       deltaCount.Encoding,
	}
	delta["token_impact"] = map[string]any{
		"schema_id":                "contextlattice_token_impact.v1",
		"version":                  1,
		"scope":                    "agent_packet_delta_transport",
		"packed_kind":              "serialized_agent_packet_delta_json",
		"baseline_tokens_estimate": fullCount.Tokens,
		"packed_tokens_estimate":   deltaCount.Tokens,
		"transport_tokens_exact":   deltaCount.Tokens,
		"saved_tokens_estimate":    saved,
		"net_token_delta":          net,
		"compression_ratio":        ratio,
		"transport_inclusive":      true,
		"estimate_method":          deltaCount.Method,
		"calibration_grade":        deltaCount.CalibrationGrade,
		"tokenizer_exact":          allExact,
		"tokenizer_encoding":       deltaCount.Encoding,
		"measurement_limit":        "Delta wire tokens and reconstructed model-visible tokens are reported separately; no provider token or inference-avoidance claim is made.",
	}
	if !allExact {
		anyMap(delta["token_impact"])["measurement_limit"] = "Exact tokenizer accounting is unavailable; return the verified full Agent Packet."
	}
}

func stabilizeAgentPacketDeltaJSONBytes(delta map[string]any) {
	metadata := anyMap(delta["format_contract"])
	if len(metadata) == 0 {
		return
	}
	for pass := 0; pass < 4; pass++ {
		size := jsonByteLen(delta)
		if size > 0 && anyToInt(metadata["actual_json_bytes"], 0) == size {
			return
		}
		metadata["actual_json_bytes"] = size
	}
}

func agentPacketDeltaTokenMetadata(delta map[string]any, fullPacket map[string]any) map[string]any {
	fullBudget := anyMap(fullPacket["token_budget"])
	fullCount := tokenCountResult{
		Tokens:           anyToInt(fullBudget["actual_tokens"], 0),
		Method:           anyToString(fullBudget["estimate_method"]),
		CalibrationGrade: anyToString(fullBudget["calibration_grade"]),
		Encoding:         anyToString(fullBudget["tokenizer_encoding"]),
		TokenizerExact:   anyToBool(fullBudget["tokenizer_exact"]) || anyToString(fullBudget["calibration_grade"]) == "tokenizer_exact",
	}
	if fullCount.Tokens < 1 {
		fullCount = contextPackCountAnyTokens(fullPacket)
	}
	modelVisibleCount := contextPackCountAnyTokens(agentPacketModelVisibleProjection(fullPacket))
	incrementalModelVisibleCount := contextPackCountAnyTokens(map[string]any{
		"operations": delta["operations"],
		"tombstones": delta["tombstones"],
	})

	// Attach and enforce the boundary once with a complete metadata shape. The
	// convergence loop changes only internally generated numeric accounting
	// fields, so repeating full boundary validation would add latency without
	// changing the contract decision. buildAgentPacketDelta validates the final
	// payload and reconstructs it before any delta can be emitted.
	provisionalCount := fullCount
	provisionalCount.Tokens = maxInt(1, fullCount.Tokens/2)
	applyAgentPacketDeltaTokenMetadata(delta, fullCount, modelVisibleCount, incrementalModelVisibleCount, provisionalCount)
	delta = attachPayloadFormatContract(
		agentPacketDeltaContractID,
		delta,
		anyToString(delta["agent_id"]),
		"agent_packet_delta",
		agentPacketEndpointForSurface(anyToString(delta["surface"])),
	)

	for pass := 0; pass < 6; pass++ {
		stabilizeAgentPacketDeltaJSONBytes(delta)
		deltaCount := contextPackCountAnyTokens(delta)
		if reported := anyToInt(anyMap(delta["token_budget"])["delta_wire_tokens_exact"], 0); reported > 0 && reported == deltaCount.Tokens {
			return delta
		}
		applyAgentPacketDeltaTokenMetadata(delta, fullCount, modelVisibleCount, incrementalModelVisibleCount, deltaCount)
	}

	// A non-converging self-count is never eligible for delta delivery. The
	// caller falls back to the verified full packet rather than claiming an
	// inexact transport saving.
	budget := anyMap(delta["token_budget"])
	budget["delta_smaller_than_full"] = false
	budget["tokens_saved_exact"] = 0
	impact := anyMap(delta["token_impact"])
	impact["saved_tokens_estimate"] = 0
	impact["net_token_delta"] = 0
	impact["measurement_limit"] = "Delta transport accounting did not converge; return the verified full Agent Packet."
	stabilizeAgentPacketDeltaJSONBytes(delta)
	return delta
}

func buildAgentPacketDelta(base, target map[string]any, now time.Time) (map[string]any, error) {
	baseIdentity, reason := validateAgentPacketSelf(base, now, true)
	if reason != "" {
		return nil, reconstructionFailure("delta_base_invalid", "base packet rejected: %s", reason)
	}
	return buildAgentPacketDeltaFromValidatedBase(base, baseIdentity, target, now)
}

// buildAgentPacketDeltaFromValidatedBase is internal to a request whose base
// has already crossed validateAgentPacketSelf. It preserves the same verified
// reconstruction proof without repeating the expensive trust-boundary pass.
func buildAgentPacketDeltaFromValidatedBase(base, baseIdentity, target map[string]any, now time.Time) (map[string]any, error) {
	targetIdentity := anyMap(target["packet_identity"])
	operations, tombstones, err := buildAgentPacketOperations(base, target)
	if err != nil {
		return nil, err
	}
	resultAccounting, err := agentPacketFinalAccounting(target)
	if err != nil {
		return nil, err
	}
	delta := map[string]any{
		"ok":                   true,
		"schema_id":            agentPacketDeltaContractID,
		"version":              1,
		"surface":              anyToString(target["surface"]),
		"project":              anyToString(target["project"]),
		"session_id":           anyToString(target["session_id"]),
		"agent_id":             anyToString(target["agent_id"]),
		"task_id":              anyToString(target["task_id"]),
		"lineage_id":           anyToString(targetIdentity["lineage_id"]),
		"packet_id":            anyToString(targetIdentity["packet_id"]),
		"revision":             anyToInt(targetIdentity["revision"], 0),
		"base_packet_id":       anyToString(baseIdentity["packet_id"]),
		"base_revision":        anyToInt(baseIdentity["revision"], 0),
		"base_digest":          anyToString(baseIdentity["transport_digest"]),
		"result_digest":        anyToString(targetIdentity["transport_digest"]),
		"model_visible_digest": anyToString(targetIdentity["model_visible_digest"]),
		"scope_digest":         anyToString(targetIdentity["scope_digest"]),
		"operations":           operations,
		"tombstones":           tombstones,
		"ack_cursor":           anyToString(targetIdentity["ack_cursor"]),
		"result_identity":      targetIdentity,
		"result_accounting":    resultAccounting,
		"reconstruction": map[string]any{
			"verified":        true,
			"digest_match":    true,
			"operation_count": len(operations),
			"contract_id":     agentPacketReconstructionContractID,
		},
		"fallback": map[string]any{
			"used":   false,
			"reason": "",
		},
		"token_budget": map[string]any{},
		"token_impact": map[string]any{},
	}
	delta = agentPacketDeltaTokenMetadata(delta, target)
	if !anyToBool(anyMap(delta["token_budget"])["delta_smaller_than_full"]) {
		return delta, nil
	}
	if findings := validateAgentContractPayload(agentPacketDeltaContractID, delta); len(findings) > 0 {
		return nil, reconstructionFailure("delta_contract_invalid", "formatted delta contract validation failed: %v", findings)
	}
	result, reconstructionErr := reconstructAgentPacketFromValidatedBase(base, baseIdentity, delta, now, true)
	if reconstructionErr != nil {
		return nil, reconstructionErr
	}
	resultIdentity := anyMap(result["packet_identity"])
	if anyToString(resultIdentity["transport_digest"]) != anyToString(targetIdentity["transport_digest"]) ||
		anyToString(resultIdentity["model_visible_digest"]) != anyToString(targetIdentity["model_visible_digest"]) {
		return nil, errors.New("formatted delta reconstruction digest mismatch")
	}
	return delta, nil
}

func agentPacketFullFallback(packet map[string]any, reason string, baseIdentity map[string]any, request map[string]any, now time.Time) map[string]any {
	packet["delta_fallback"] = map[string]any{
		"requested":            true,
		"used":                 true,
		"reason":               clipText(reason, 120),
		"base_packet_id":       anyToString(baseIdentity["packet_id"]),
		"full_packet_verified": true,
		"rollback_env":         agentPacketDeltaFeatureEnv + "=false",
	}
	return finalizeAgentPacketWithIdentity(packet, nil, request, now)
}

func finalizeAgentPacketForRequest(packet map[string]any, request map[string]any) map[string]any {
	return finalizeAgentPacketForRequestAt(packet, request, time.Now().UTC())
}

func finalizeAgentPacketForRequestAt(packet map[string]any, request map[string]any, now time.Time) map[string]any {
	now = now.UTC()
	if !agentPacketDeltaRequested(request) {
		return finalizeAgentPacketWithIdentity(packet, nil, request, now)
	}
	if !agentPacketDeltaEnabled() {
		return agentPacketFullFallback(packet, "delta_disabled", nil, request, now)
	}
	baseValidation := validateAgentPacketBase(request, packet, now)
	if baseValidation.Reason != "" {
		return agentPacketFullFallback(packet, baseValidation.Reason, baseValidation.Identity, request, now)
	}
	target := finalizeAgentPacketWithIdentity(packet, baseValidation.Identity, request, now)
	delta, err := buildAgentPacketDeltaFromValidatedBase(baseValidation.Packet, baseValidation.Identity, target, now)
	if err != nil {
		return agentPacketFullFallback(target, "reconstruction_failed", baseValidation.Identity, request, now)
	}
	budget := anyMap(delta["token_budget"])
	if !anyToBool(budget["tokenizer_exact"]) {
		return agentPacketFullFallback(target, "delta_tokenizer_inexact", baseValidation.Identity, request, now)
	}
	if !anyToBool(budget["delta_smaller_than_full"]) {
		return agentPacketFullFallback(target, "delta_not_smaller", baseValidation.Identity, request, now)
	}
	return delta
}

func reconstructionFailure(code string, format string, args ...any) error {
	return &agentPacketReconstructionError{Code: code, Err: fmt.Errorf(format, args...)}
}

func cloneAgentPacketValue(value any) (any, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func applyAgentPacketOperation(node any, segments []string, operation map[string]any) (any, error) {
	if len(segments) == 0 {
		return nil, errors.New("operation path has no segments")
	}
	segment := segments[0]
	if len(segments) > 1 {
		switch typed := node.(type) {
		case map[string]any:
			child, exists := typed[segment]
			if !exists {
				return nil, fmt.Errorf("operation parent does not exist: %s", segment)
			}
			updated, err := applyAgentPacketOperation(child, segments[1:], operation)
			if err != nil {
				return nil, err
			}
			typed[segment] = updated
			return typed, nil
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return nil, fmt.Errorf("operation list parent index is invalid: %s", segment)
			}
			updated, err := applyAgentPacketOperation(typed[index], segments[1:], operation)
			if err != nil {
				return nil, err
			}
			typed[index] = updated
			return typed, nil
		default:
			return nil, fmt.Errorf("operation traverses a scalar at %s", segment)
		}
	}

	kind := anyToString(operation["op"])
	value, hasValue := operation["value"]
	switch typed := node.(type) {
	case map[string]any:
		_, exists := typed[segment]
		switch kind {
		case "add":
			if exists || !hasValue {
				return nil, fmt.Errorf("add requires an absent field and a value: %s", segment)
			}
			cloned, err := cloneAgentPacketValue(value)
			if err != nil {
				return nil, err
			}
			typed[segment] = cloned
		case "replace":
			if !exists || !hasValue {
				return nil, fmt.Errorf("replace requires an existing field and a value: %s", segment)
			}
			cloned, err := cloneAgentPacketValue(value)
			if err != nil {
				return nil, err
			}
			typed[segment] = cloned
		case "remove":
			if !exists || !anyToBool(operation["tombstone"]) {
				return nil, fmt.Errorf("remove requires an existing field and tombstone: %s", segment)
			}
			delete(typed, segment)
		default:
			return nil, fmt.Errorf("unsupported operation: %s", kind)
		}
		return typed, nil
	case []any:
		index, err := strconv.Atoi(segment)
		if err != nil || index < 0 {
			return nil, fmt.Errorf("operation list index is invalid: %s", segment)
		}
		switch kind {
		case "add":
			if index != len(typed) || !hasValue {
				return nil, fmt.Errorf("list add must append exactly one value: %s", segment)
			}
			cloned, err := cloneAgentPacketValue(value)
			if err != nil {
				return nil, err
			}
			return append(typed, cloned), nil
		case "replace":
			if index >= len(typed) || !hasValue {
				return nil, fmt.Errorf("list replace target is invalid: %s", segment)
			}
			cloned, err := cloneAgentPacketValue(value)
			if err != nil {
				return nil, err
			}
			typed[index] = cloned
			return typed, nil
		case "remove":
			if index != len(typed)-1 || !anyToBool(operation["tombstone"]) {
				return nil, fmt.Errorf("list remove must tombstone the final value: %s", segment)
			}
			return typed[:index], nil
		default:
			return nil, fmt.Errorf("unsupported operation: %s", kind)
		}
	default:
		return nil, fmt.Errorf("operation parent is not a container: %s", segment)
	}
}

func reconstructAgentPacket(base map[string]any, delta map[string]any, now time.Time, validateDeltaContract bool) (map[string]any, error) {
	baseIdentity, baseReason := validateAgentPacketSelf(base, now, true)
	if baseReason != "" {
		return nil, reconstructionFailure("delta_base_invalid", "base packet rejected: %s", baseReason)
	}
	return reconstructAgentPacketFromValidatedBase(base, baseIdentity, delta, now, validateDeltaContract)
}

func reconstructAgentPacketFromValidatedBase(base, baseIdentity, delta map[string]any, now time.Time, validateDeltaContract bool) (map[string]any, error) {
	if anyToString(delta["schema_id"]) != agentPacketDeltaContractID {
		return nil, reconstructionFailure("delta_schema_mismatch", "expected %s", agentPacketDeltaContractID)
	}
	if findings := agentPacketDeltaOperationFindings(delta); len(findings) > 0 {
		code := firstNonEmptyStrings(anyToString(findings[0]["code"]), "operation_contract_invalid")
		return nil, reconstructionFailure(code, "delta operation contract failed: %s", anyToString(findings[0]["reason"]))
	}
	if validateDeltaContract {
		if findings := validateAgentContractPayload(agentPacketDeltaContractID, delta); len(findings) > 0 {
			return nil, reconstructionFailure("delta_contract_invalid", "delta contract validation failed")
		}
	}
	if anyToString(delta["base_packet_id"]) != anyToString(baseIdentity["packet_id"]) ||
		anyToString(delta["base_digest"]) != anyToString(baseIdentity["transport_digest"]) ||
		anyToInt(delta["base_revision"], 0) != anyToInt(baseIdentity["revision"], 0) ||
		anyToString(delta["lineage_id"]) != anyToString(baseIdentity["lineage_id"]) {
		return nil, reconstructionFailure("delta_base_mismatch", "delta does not bind to the supplied base")
	}
	projection, err := agentPacketTransportProjection(base)
	if err != nil {
		return nil, reconstructionFailure("delta_base_invalid", "clone base projection: %v", err)
	}
	operations := contextPackAnyList(delta["operations"])
	if len(operations) > maxAgentPacketDeltaOperations {
		return nil, reconstructionFailure("operation_limit_exceeded", "delta has %d operations; limit is %d", len(operations), maxAgentPacketDeltaOperations)
	}
	previousPath := ""
	seenPaths := map[string]struct{}{}
	expectedTombstones := []string{}
	for index, raw := range operations {
		operation := anyMap(raw)
		sequence := anyToInt(operation["sequence"], 0)
		path := anyToString(operation["path"])
		if sequence != index+1 || (previousPath != "" && path <= previousPath) {
			return nil, reconstructionFailure("operation_sequence_invalid", "operation %d is not in canonical order", index+1)
		}
		if _, exists := seenPaths[path]; exists {
			return nil, reconstructionFailure("operation_sequence_invalid", "operation path is duplicated: %s", path)
		}
		seenPaths[path] = struct{}{}
		previousPath = path
		segments, pathErr := parseAgentPacketJSONPointer(path)
		if pathErr != nil || len(segments) == 0 || containsString([]string{"format_contract", "packet_identity", "delta_fallback", "token_budget", "token_impact"}, segments[0]) {
			return nil, reconstructionFailure("operation_path_invalid", "operation path is not allowed: %s", path)
		}
		if anyToString(operation["op"]) == "remove" {
			expectedTombstones = append(expectedTombstones, path)
		} else if anyToBool(operation["tombstone"]) {
			return nil, reconstructionFailure("operation_kind_invalid", "only remove operations may carry tombstones: %s", path)
		}
		updated, applyErr := applyAgentPacketOperation(projection, segments, operation)
		if applyErr != nil {
			return nil, reconstructionFailure("operation_precondition_failed", "operation %s failed: %v", path, applyErr)
		}
		projection = anyMap(updated)
	}
	providedTombstones := anyToStringList(delta["tombstones"], maxAgentPacketDeltaOperations)
	if !agentPacketJSONEqual(providedTombstones, expectedTombstones) {
		return nil, reconstructionFailure("tombstone_manifest_mismatch", "delta tombstone manifest does not match remove operations")
	}
	resultIdentity := anyMap(delta["result_identity"])
	if anyToString(resultIdentity["schema_id"]) != agentPacketIdentitySchemaID ||
		anyToInt(resultIdentity["version"], 0) != 1 ||
		anyToInt(resultIdentity["ack_version"], 0) != agentPacketIdentityAckVersion ||
		anyToString(resultIdentity["lineage_id"]) != anyToString(baseIdentity["lineage_id"]) ||
		anyToString(resultIdentity["base_packet_id"]) != anyToString(baseIdentity["packet_id"]) ||
		anyToString(resultIdentity["base_digest"]) != anyToString(baseIdentity["transport_digest"]) ||
		anyToInt(resultIdentity["revision"], 0) != anyToInt(baseIdentity["revision"], 0)+1 {
		return nil, reconstructionFailure("result_identity_mismatch", "result identity does not bind to the supplied base")
	}
	baseIssuedAt, baseIssuedErr := time.Parse(time.RFC3339Nano, anyToString(baseIdentity["issued_at"]))
	resultIssuedAt, resultIssuedErr := time.Parse(time.RFC3339Nano, anyToString(resultIdentity["issued_at"]))
	resultExpiresAt, resultExpiresErr := time.Parse(time.RFC3339Nano, anyToString(resultIdentity["expires_at"]))
	now = now.UTC()
	if baseIssuedErr != nil || resultIssuedErr != nil || resultExpiresErr != nil ||
		resultIssuedAt.Before(baseIssuedAt) || resultIssuedAt.After(now) || !resultExpiresAt.After(resultIssuedAt) ||
		resultExpiresAt.Sub(resultIssuedAt) > time.Duration(maximumAgentPacketTTLSeconds)*time.Second || !now.Before(resultExpiresAt) {
		return nil, reconstructionFailure("result_identity_mismatch", "result identity has an invalid issuance relationship")
	}
	resultAccounting := anyMap(delta["result_accounting"])
	resultBudget := anyMap(resultAccounting["token_budget"])
	resultImpact := anyMap(resultAccounting["token_impact"])
	if anyToInt(resultBudget["target_tokens"], 0) < 1 || anyToInt(resultBudget["hard_limit_tokens"], 0) < 1 || anyToInt(resultBudget["actual_tokens"], 0) < 1 || anyToInt(resultImpact["baseline_tokens_estimate"], -1) < 0 {
		return nil, reconstructionFailure("result_accounting_invalid", "delta is missing a valid accounting seed")
	}
	accountingPacket := map[string]any{"token_budget": resultBudget, "token_impact": resultImpact}
	accountingDigest, accountingErr := agentPacketAccountingDigest(accountingPacket)
	if accountingErr != nil || accountingDigest != anyToString(resultIdentity["accounting_digest"]) {
		return nil, reconstructionFailure("result_accounting_mismatch", "delta accounting seed does not match the result identity")
	}
	finalAccounting := cloneAnyMap(resultAccounting)
	delete(finalAccounting, "digest")
	finalAccountingDigest, finalAccountingErr := canonicalAgentPacketDigest(finalAccounting)
	if finalAccountingErr != nil || finalAccountingDigest != anyToString(resultAccounting["digest"]) {
		return nil, reconstructionFailure("result_accounting_mismatch", "delta finalized accounting receipt is invalid")
	}
	projection["packet_identity"] = resultIdentity
	projection["token_budget"] = cloneAnyMap(resultBudget)
	projection["token_impact"] = cloneAnyMap(resultImpact)
	modelVisibleDigest, transportDigest, scopeDigest, digestErr := agentPacketDigestParts(projection)
	if digestErr != nil {
		return nil, reconstructionFailure("result_digest_unavailable", "digest reconstructed packet: %v", digestErr)
	}
	if transportDigest != anyToString(delta["result_digest"]) || transportDigest != anyToString(resultIdentity["transport_digest"]) ||
		modelVisibleDigest != anyToString(delta["model_visible_digest"]) || modelVisibleDigest != anyToString(resultIdentity["model_visible_digest"]) {
		return nil, reconstructionFailure("result_digest_mismatch", "reconstructed packet digest does not match delta")
	}
	if scopeDigest != anyToString(delta["scope_digest"]) || scopeDigest != anyToString(resultIdentity["scope_digest"]) ||
		anyToString(resultIdentity["packet_id"]) != anyToString(delta["packet_id"]) ||
		anyToString(resultIdentity["packet_id"]) != agentPacketPacketIDPrefix+agentPacketDigestSuffix(transportDigest, 24) ||
		anyToInt(resultIdentity["revision"], 0) != anyToInt(delta["revision"], 0) ||
		anyToString(resultIdentity["ack_cursor"]) != anyToString(delta["ack_cursor"]) ||
		anyToString(resultIdentity["ack_cursor"]) != agentPacketAckCursor(resultIdentity) {
		return nil, reconstructionFailure("result_identity_mismatch", "reconstructed packet identity does not match delta")
	}
	projection = attachAgentPacketFormatContract(projection)
	if findings := validateAgentContractPayload(agentPacketContractID, projection); len(findings) > 0 {
		return nil, reconstructionFailure("result_contract_invalid", "reconstructed agent packet contract validation failed: %v", findings)
	}
	// Contract, identity, lineage, digests, validity, accounting receipt, and ACK
	// are all verified above. Recount only the final serialized transport here;
	// repeating the full trust-boundary pass would duplicate those checks.
	if reason := validateAgentPacketTransportAccounting(projection); reason != "" {
		return nil, reconstructionFailure("result_self_verification_failed", "reconstructed agent packet failed self-verification: %s", reason)
	}
	return projection, nil
}

func agentPacketReconstructionResponse(base map[string]any, delta map[string]any, now time.Time) (map[string]any, int) {
	packet, err := reconstructAgentPacket(base, delta, now, true)
	response := map[string]any{
		"ok":                 err == nil,
		"schema_id":          agentPacketReconstructionContractID,
		"version":            1,
		"verified":           err == nil,
		"base_packet_id":     anyToString(anyMap(base["packet_identity"])["packet_id"]),
		"packet_id":          anyToString(delta["packet_id"]),
		"result_digest":      anyToString(delta["result_digest"]),
		"operations_applied": 0,
		"packet":             map[string]any{},
		"warnings":           []any{},
		"error":              "",
	}
	status := http.StatusOK
	if err != nil {
		status = http.StatusUnprocessableEntity
		response["error"] = "reconstruction_failed"
		var reconstructionErr *agentPacketReconstructionError
		if errors.As(err, &reconstructionErr) {
			response["error"] = reconstructionErr.Code
		}
		response["warnings"] = []any{clipText(err.Error(), 500)}
	} else {
		response["packet"] = packet
		response["operations_applied"] = len(contextPackAnyList(delta["operations"]))
	}
	response = attachPayloadFormatContract(agentPacketReconstructionContractID, response, anyToString(delta["agent_id"]), "agent_packet_reconstruction", agentPacketReconstructionRoute)
	return response, status
}

func (s *server) memoryAgentPacketReconstruct(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAgentPacketReconstructionBody)
	payload, err := readOptionalJSONBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_json", "detail": clipText(err.Error(), 300)})
		return
	}
	base := anyMap(firstNonEmptyAny(payload["base_packet"], payload["basePacket"]))
	delta := anyMap(payload["delta"])
	response, status := agentPacketReconstructionResponse(base, delta, time.Now().UTC())
	writeJSON(w, status, response)
}

func agentPacketIdentitySummary(packet map[string]any) string {
	identity := anyMap(packet["packet_identity"])
	return strings.Join([]string{
		anyToString(identity["packet_id"]),
		strconv.Itoa(anyToInt(identity["revision"], 0)),
		anyToString(identity["transport_digest"]),
	}, "|")
}
