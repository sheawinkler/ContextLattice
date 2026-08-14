package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	recallResponseContinuationSchema                 = "recall_response.continuation.v1"
	recallResponseContinuationTokenBytes             = 24
	recallResponseContinuationPageSize               = 128
	recallResponseContinuationMaximumItems           = 4096
	recallResponseContinuationTTL                    = 5 * time.Minute
	recallResponseContinuationMaximumRecords         = 64
	recallResponseContinuationMaximumStoredItems     = 8192
	recallResponseContinuationMaximumStoredBytes     = 2 * 1024 * 1024
	recallResponseContinuationMaximumRecordsPerAgent = 8
	recallResponseContinuationMaximumItemsPerAgent   = 4096
	recallResponseContinuationMaximumBytesPerAgent   = 1024 * 1024
)

type recallResponseContinuationRecord struct {
	AgentID                string
	Endpoint               string
	ScopeDigest            string
	SnapshotDigest         string
	RequestDigest          string
	SourceMembershipDigest string
	ContinuationRef        string
	Items                  []any
	Offset                 int
	Page                   int
	CreatedAt              time.Time
	ExpiresAt              time.Time
	StoredBytes            int
}

type recallResponseContinuationTelemetry struct {
	Admitted         uint64
	CapacityRejected uint64
	FairnessRejected uint64
	ExpiredEvicted   uint64
	InvalidRequests  uint64
	ResolvedPages    uint64
	Discarded        uint64
	CurrentRecords   int
	CurrentItems     int
	CurrentBytes     int
}

type recallResponseContinuationMembershipSet struct {
	All     []any
	Omitted []any
	Digest  string
}

func recallResponseTypedItemKey(itemType, itemRef string) string {
	return strings.TrimSpace(itemType) + "\x00" + strings.TrimSpace(itemRef)
}

func recallResponseSafeContinuationItemType(value, fallback string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if recallResponseOneOf(value, "evidence", "temporal", "proof", "conflict", "component") {
		return value
	}
	return fallback
}

func recallResponseContinuationTypedKeys(items []any) []any {
	keys := make([]any, 0, len(items))
	for _, raw := range items {
		row := anyMap(raw)
		keys = append(keys, anyToString(row["item_type"])+"|"+anyToString(row["item_ref"]))
	}
	return keys
}

func recallResponseValidContinuationToken(value string) bool {
	if !strings.HasPrefix(value, "rrct_") || len(value) != len("rrct_")+(2*recallResponseContinuationTokenBytes) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "rrct_"))
	return err == nil
}

func recallResponseNewContinuationToken() (string, bool) {
	raw := make([]byte, recallResponseContinuationTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", false
	}
	return "rrct_" + hex.EncodeToString(raw), true
}

func recallResponseContinuationAction(scopeDigest, requestDigest, endpoint, token string, page int) map[string]any {
	return map[string]any{
		"kind":               "continue_snapshot",
		"method":             http.MethodPost,
		"route":              endpoint,
		"snapshot_semantics": "same_snapshot",
		"request_contract":   "token+scope+agent+request_digest",
		"scope_digest":       scopeDigest,
		"request_digest":     requestDigest,
		"token":              token,
		"page":               page,
	}
}

func recallResponseContinuationRequestDigest(request map[string]any) string {
	requestMaterial := cloneJSONMap(request)
	delete(requestMaterial, "_suppress_token_impact_recording")
	delete(requestMaterial, "_suppress_final_token_impact_recording")
	delete(requestMaterial, "include_retrieval_debug")
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(requestMaterial))
}

func recallResponseTerminalContinuationAction(scopeDigest string) map[string]any {
	return map[string]any{
		"kind":               "terminal",
		"method":             "",
		"route":              "",
		"snapshot_semantics": "exhausted",
		"request_contract":   "none",
		"scope_digest":       scopeDigest,
		"request_digest":     "",
		"token":              "",
		"page":               0,
	}
}

func recallResponseSetContinuationAction(response map[string]any, action map[string]any) {
	disclosure := recallResponseDisclosure(response)
	disclosure["continuation_action"] = action
	disclosure["union_digest"] = recallResponseNonExclusionDigest(disclosure)
	for _, raw := range contextPackAnyList(disclosure["omission_ledger"]) {
		row := anyMap(raw)
		binding := anyMap(row["evidence_binding"])
		binding["binding_digest"] = recallResponseOmissionBindingDigest(disclosure, anyToString(row["item_ref"]), anyToString(row["item_type"]), contextPackAnyList(binding["proof_refs"]))
	}
	scope := anyMap(response["request_scope"])
	recallResponseBindControlReceipt(
		response,
		anyToString(scope["snapshot_digest"]),
		anyToString(scope["receipt_digest"]),
		anyToBool(scope["source_bound"]),
	)
	response["response_id"] = recallResponseIDForResponse(response)
	response["response_digest"] = recallResponseSemanticDigest(response)
}

func recallResponseContinuationMembership(
	composition, response map[string]any,
	policy validatedRecallResponsePolicyInput,
) (recallResponseContinuationMembershipSet, bool) {
	prepared, asOf := recallResponsePrepareTemporalInput(composition)
	_, asOfValid := recallResponseNormalizeAsOfWithValidity(firstNonEmptyStrings(anyToString(composition["as_of"]), anyToString(composition["asOf"])))
	scopeDigest := anyToString(anyMap(response["request_scope"])["scope_digest"])
	if !recallResponseValidDigest(scopeDigest) {
		return recallResponseContinuationMembershipSet{}, false
	}
	// A server-owned source snapshot that is invalid or incomplete proves that
	// some source membership is not traversable. Do not issue a cursor whose
	// terminal page would falsely close that omission; the caller receives the
	// explicit unavailable/fail-closed action instead.
	if snapshot := anyMap(composition["_recall_response_source_snapshot"]); len(snapshot) == 0 {
		pack := recallResponseCanonicalContextPack(composition)
		snapshot = anyMap(pack["_recall_response_source_snapshot"])
		if len(snapshot) == 0 && !policy.synthetic {
			return recallResponseContinuationMembershipSet{}, false
		}
		if len(snapshot) > 0 && (!recallResponseSourceSnapshotValidForInput(prepared, snapshot) || !anyToBool(snapshot["complete"])) {
			return recallResponseContinuationMembershipSet{}, false
		}
	} else if !recallResponseSourceSnapshotValidForInput(prepared, snapshot) || !anyToBool(snapshot["complete"]) {
		return recallResponseContinuationMembershipSet{}, false
	}
	disclosure := recallResponseDisclosure(response)
	seen := map[string]bool{}
	all := make([]any, 0)
	appendItem := func(itemType, refID, disposition string, protected bool) bool {
		refID = strings.TrimSpace(refID)
		key := recallResponseTypedItemKey(itemType, refID)
		if refID == "" || seen[key] {
			return true
		}
		if len(all) >= recallResponseContinuationMaximumItems {
			return false
		}
		seen[key] = true
		all = append(all, map[string]any{
			"item_type": itemType, "item_ref": refID,
			"disposition": disposition, "protected": protected,
		})
		return true
	}
	// Cursor membership starts from the complete original canonical source
	// snapshot. Every row crosses the same temporal and hard-exclusion policy as
	// the response projection at the requested as_of. Presentation clipping is
	// deliberately absent here: the owner retains the complete typed set and
	// only pages it on the wire.
	classes := recallResponseCanonicalSourceRows(composition)
	for _, class := range []string{"evidence", "temporal", "proof"} {
		for index, raw := range classes[class] {
			item := anyMap(raw)
			temporalAllowed := false
			if asOfValid {
				filtered, _ := recallResponseTemporalEvidenceAtOrBefore([]any{raw}, asOf)
				if len(filtered) == 1 {
					item = anyMap(filtered[0])
					temporalAllowed = true
				}
			}
			refID, _, eligible, confidenceValid, _, _, _, protected, _, _ := recallResponseEvidenceProjection(item, index, scopeDigest, policy)
			disposition := "relevant"
			if !temporalAllowed || !eligible || !confidenceValid {
				disposition = "hard_excluded"
				protected = false
			}
			if !appendItem(class, refID, disposition, protected) {
				return recallResponseContinuationMembershipSet{}, false
			}
		}
	}
	for index, raw := range classes["conflicts"] {
		if !appendItem("conflict", recallResponseConflictRef(raw, index, scopeDigest), "conflict", true) {
			return recallResponseContinuationMembershipSet{}, false
		}
	}
	// The proof union is a first-class typed membership class, including gap,
	// conflict, exclusion, and evidence receipts generated after source capture.
	// Recompute its complete bounded membership from the same response/input
	// artifacts instead of treating the presented proof sample as the source.
	rankedEvidence := recallResponseRelevantEvidenceRows(prepared)
	exclusionAccounting := recallResponseExclusionRefsWithAccounting(prepared, rankedEvidence, scopeDigest, policy)
	proofAccounting := recallResponseProofUnionRefsWithAccounting(
		response,
		contextPackAnyList(disclosure["evidence_union"]),
		recallResponseAnyStrings(exclusionAccounting.allRefs),
		recallResponseAllConflictRefs(prepared, scopeDigest),
	)
	protectedProof := map[string]bool{}
	for _, raw := range contextPackAnyList(response["conflicts"]) {
		protectedProof[anyToString(anyMap(raw)["conflict_id"])] = true
	}
	for _, raw := range contextPackAnyList(response["gaps"]) {
		protectedProof[recallResponseScopedOpaqueRef(scopeDigest, "gap", anyToString(anyMap(raw)["code"]))] = true
	}
	for _, refID := range proofAccounting.allRefs {
		if !appendItem("proof", refID, "relevant", protectedProof[refID] || recallResponseNonExclusionProtected(response, refID)) {
			return recallResponseContinuationMembershipSet{}, false
		}
	}
	for _, raw := range contextPackAnyList(disclosure["omission_ledger"]) {
		row := anyMap(raw)
		if anyToString(row["item_type"]) == "proof" && anyToString(row["reason"]) == "proof_union_clipped" &&
			!appendItem("proof", anyToString(row["item_ref"]), "relevant", anyToBool(row["protected"])) {
			return recallResponseContinuationMembershipSet{}, false
		}
	}
	for _, raw := range contextPackAnyList(disclosure["component_union"]) {
		row := anyMap(raw)
		if !appendItem("component", anyToString(row["component_ref"]), "relevant", anyToBool(row["protected"])) {
			return recallResponseContinuationMembershipSet{}, false
		}
	}
	// The public source-membership digest binds exact typed identities. Item
	// disposition and protection metadata remain owner-held and page-bound, but
	// cannot make the membership identity itself unstable.
	fullDigest := "sha256:" + sha256Hex(recallResponseCanonicalJSON(recallResponseContinuationTypedKeys(all)))
	visible := map[string]bool{}
	// Presentation evidence is the actual initial wire page. The disclosure
	// evidence_union is deliberately compact and may contain only the bounded
	// proof sample, so using it alone would re-page rows that were already
	// visible in the response body.
	for _, raw := range contextPackAnyList(response["evidence"]) {
		row := anyMap(raw)
		refID := anyToString(row["ref_id"])
		if refID == "" {
			continue
		}
		itemType := recallResponseSafeContinuationItemType(anyToString(row["item_type"]), "evidence")
		visible[recallResponseTypedItemKey(itemType, refID)] = true
	}
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		row := anyMap(raw)
		itemType := recallResponseSafeContinuationItemType(anyToString(row["item_type"]), "evidence")
		visible[recallResponseTypedItemKey(itemType, anyToString(row["ref_id"]))] = true
	}
	// exclusion_refs is retained for v1 compatibility but carries no item type.
	// Never let that untyped sample collapse cross-class membership: only the
	// typed evidence_union rows above establish visible source membership.
	for _, raw := range contextPackAnyList(disclosure["proof_union"]) {
		visible[recallResponseTypedItemKey("proof", anyToString(raw))] = true
	}
	for _, raw := range contextPackAnyList(response["conflicts"]) {
		visible[recallResponseTypedItemKey("conflict", anyToString(anyMap(raw)["conflict_id"]))] = true
	}
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		visible[recallResponseTypedItemKey("component", anyToString(anyMap(raw)["component_ref"]))] = true
	}
	omitted := make([]any, 0, len(all))
	for _, raw := range all {
		row := anyMap(raw)
		if !visible[recallResponseTypedItemKey(anyToString(row["item_type"]), anyToString(row["item_ref"]))] {
			omitted = append(omitted, raw)
		}
	}
	return recallResponseContinuationMembershipSet{All: all, Omitted: omitted, Digest: fullDigest}, true
}

func (s *server) recallResponseNow() time.Time {
	if s != nil && s.recallResponseContinuationNow != nil {
		return s.recallResponseContinuationNow().UTC()
	}
	return time.Now().UTC()
}

func recallResponseContinuationRecordBytes(record recallResponseContinuationRecord) int {
	material := map[string]any{
		"agent_id": record.AgentID, "endpoint": record.Endpoint,
		"scope_digest": record.ScopeDigest, "snapshot_digest": record.SnapshotDigest,
		"request_digest": record.RequestDigest, "source_membership_digest": record.SourceMembershipDigest,
		"continuation_ref": record.ContinuationRef, "items": record.Items,
		"created_at": record.CreatedAt.UTC().Format(time.RFC3339Nano),
		"expires_at": record.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}
	return len(recallResponseCanonicalJSON(material))
}

func (s *server) recountRecallResponseContinuationsLocked() {
	records, items, bytes := 0, 0, 0
	for token, record := range s.recallResponseContinuations {
		if !recallResponseValidContinuationToken(token) {
			continue
		}
		records++
		items += len(record.Items)
		storedBytes := record.StoredBytes
		if storedBytes <= 0 {
			storedBytes = recallResponseContinuationRecordBytes(record)
		}
		bytes += storedBytes
	}
	s.recallResponseContinuationStats.CurrentRecords = records
	s.recallResponseContinuationStats.CurrentItems = items
	s.recallResponseContinuationStats.CurrentBytes = bytes
}

func (s *server) pruneExpiredRecallResponseContinuationsLocked(now time.Time) {
	type expiredRecord struct {
		token     string
		expiresAt time.Time
		createdAt time.Time
	}
	expired := []expiredRecord{}
	for token, record := range s.recallResponseContinuations {
		if !now.Before(record.ExpiresAt) {
			expired = append(expired, expiredRecord{token: token, expiresAt: record.ExpiresAt, createdAt: record.CreatedAt})
		}
	}
	sort.Slice(expired, func(left, right int) bool {
		if !expired[left].expiresAt.Equal(expired[right].expiresAt) {
			return expired[left].expiresAt.Before(expired[right].expiresAt)
		}
		if !expired[left].createdAt.Equal(expired[right].createdAt) {
			return expired[left].createdAt.Before(expired[right].createdAt)
		}
		return expired[left].token < expired[right].token
	})
	for _, row := range expired {
		delete(s.recallResponseContinuations, row.token)
		s.recallResponseContinuationStats.ExpiredEvicted++
	}
	s.recountRecallResponseContinuationsLocked()
}

func (s *server) admitRecallResponseContinuationLocked(token string, record recallResponseContinuationRecord) bool {
	if s.recallResponseContinuations == nil {
		s.recallResponseContinuations = map[string]recallResponseContinuationRecord{}
	}
	if record.StoredBytes <= 0 {
		record.StoredBytes = recallResponseContinuationRecordBytes(record)
	}
	agentRecords, agentItems, agentBytes := 0, 0, 0
	for _, existing := range s.recallResponseContinuations {
		if existing.AgentID != record.AgentID {
			continue
		}
		agentRecords++
		agentItems += len(existing.Items)
		existingBytes := existing.StoredBytes
		if existingBytes <= 0 {
			existingBytes = recallResponseContinuationRecordBytes(existing)
		}
		agentBytes += existingBytes
	}
	if agentRecords+1 > recallResponseContinuationMaximumRecordsPerAgent ||
		agentItems+len(record.Items) > recallResponseContinuationMaximumItemsPerAgent ||
		agentBytes+record.StoredBytes > recallResponseContinuationMaximumBytesPerAgent {
		s.recallResponseContinuationStats.FairnessRejected++
		return false
	}
	s.recountRecallResponseContinuationsLocked()
	if s.recallResponseContinuationStats.CurrentRecords+1 > recallResponseContinuationMaximumRecords ||
		s.recallResponseContinuationStats.CurrentItems+len(record.Items) > recallResponseContinuationMaximumStoredItems ||
		s.recallResponseContinuationStats.CurrentBytes+record.StoredBytes > recallResponseContinuationMaximumStoredBytes {
		s.recallResponseContinuationStats.CapacityRejected++
		return false
	}
	if _, collision := s.recallResponseContinuations[token]; collision {
		s.recallResponseContinuationStats.CapacityRejected++
		return false
	}
	s.recallResponseContinuations[token] = record
	s.recallResponseContinuationStats.Admitted++
	s.recountRecallResponseContinuationsLocked()
	return true
}

func (s *server) recallResponseContinuationTelemetrySnapshot() recallResponseContinuationTelemetry {
	if s == nil {
		return recallResponseContinuationTelemetry{}
	}
	s.recallResponseContinuationMu.Lock()
	defer s.recallResponseContinuationMu.Unlock()
	s.recountRecallResponseContinuationsLocked()
	return s.recallResponseContinuationStats
}

func (s *server) discardRecallResponseContinuation(response map[string]any) {
	if s == nil || response == nil {
		return
	}
	token := anyToString(anyMap(recallResponseDisclosure(response)["continuation_action"])["token"])
	if !recallResponseValidContinuationToken(token) {
		return
	}
	s.recallResponseContinuationMu.Lock()
	if _, found := s.recallResponseContinuations[token]; found {
		delete(s.recallResponseContinuations, token)
		s.recallResponseContinuationStats.Discarded++
		s.recountRecallResponseContinuationsLocked()
	}
	s.recallResponseContinuationMu.Unlock()
}

func (s *server) installRecallResponseContinuation(
	response, composition, request map[string]any,
	policy validatedRecallResponsePolicyInput,
	agentID, endpoint string,
) {
	s.installRecallResponseContinuationWithFit(response, composition, request, policy, agentID, endpoint, true)
}

func (s *server) installRecallResponseContinuationWithFit(
	response, composition, request map[string]any,
	policy validatedRecallResponsePolicyInput,
	agentID, endpoint string,
	fitLegacyTransport bool,
) {
	if s == nil {
		return
	}
	scope := anyMap(response["request_scope"])
	scopeDigest := anyToString(scope["scope_digest"])
	snapshotDigest := anyToString(scope["snapshot_digest"])
	requestDigest := recallResponseContinuationRequestDigest(request)
	membership, ok := recallResponseContinuationMembership(composition, response, policy)
	if !ok || !recallResponseValidDigest(snapshotDigest) {
		recallResponseSetContinuationAction(response, recallResponseUnavailableContinuationAction(scopeDigest))
		return
	}
	if len(membership.Omitted) == 0 {
		recallResponseSetContinuationAction(response, recallResponseTerminalContinuationAction(scopeDigest))
		if fitLegacyTransport {
			recallResponseFitCandidateBudget(response)
		}
		return
	}
	token, ok := recallResponseNewContinuationToken()
	if !ok {
		return
	}
	recallResponseSetContinuationAction(response, recallResponseContinuationAction(scopeDigest, requestDigest, endpoint, token, 1))
	if fitLegacyTransport && !recallResponseFitCandidateBudget(response) {
		recallResponseSetContinuationAction(response, recallResponseUnavailableContinuationAction(scopeDigest))
		recallResponseFitCandidateBudget(response)
		return
	}
	membership, ok = recallResponseContinuationMembership(composition, response, policy)
	if !ok {
		recallResponseSetContinuationAction(response, recallResponseUnavailableContinuationAction(scopeDigest))
		return
	}
	if len(membership.Omitted) == 0 {
		recallResponseSetContinuationAction(response, recallResponseTerminalContinuationAction(scopeDigest))
		return
	}
	now := s.recallResponseNow()
	record := recallResponseContinuationRecord{
		AgentID: agentID, Endpoint: endpoint, ScopeDigest: scopeDigest,
		SnapshotDigest:         snapshotDigest,
		RequestDigest:          requestDigest,
		SourceMembershipDigest: membership.Digest,
		ContinuationRef:        anyToString(recallResponseDisclosure(response)["continuation_ref"]),
		Items:                  cloneJSONValue(membership.Omitted).([]any), Offset: 0, Page: 1,
		CreatedAt: now,
		ExpiresAt: now.Add(recallResponseContinuationTTL),
	}
	record.StoredBytes = recallResponseContinuationRecordBytes(record)
	s.recallResponseContinuationMu.Lock()
	s.pruneExpiredRecallResponseContinuationsLocked(now)
	admitted := s.admitRecallResponseContinuationLocked(token, record)
	s.recallResponseContinuationMu.Unlock()
	if !admitted {
		recallResponseSetContinuationAction(response, recallResponseUnavailableContinuationAction(scopeDigest))
		if fitLegacyTransport {
			recallResponseFitCandidateBudget(response)
		}
	}
}

// reconcileRecallResponseContinuation runs after transport fitting. The
// formatter may remove additional visible rows after the initial cursor was
// installed; the owner record must therefore be rebound to the exact final
// visible/omitted partition before the response is served.
func (s *server) reconcileRecallResponseContinuation(
	response, composition, request map[string]any,
	policy validatedRecallResponsePolicyInput,
	agentID, endpoint string,
) bool {
	if s == nil {
		return false
	}
	membership, ok := recallResponseContinuationMembership(composition, response, policy)
	if !ok {
		return false
	}
	disclosure := recallResponseDisclosure(response)
	action := anyMap(disclosure["continuation_action"])
	kind := anyToString(action["kind"])
	if kind == "terminal" && len(membership.Omitted) > 0 {
		s.installRecallResponseContinuation(response, composition, request, policy, agentID, endpoint)
		return anyToString(anyMap(disclosure["continuation_action"])["kind"]) != "terminal"
	}
	if kind != "continue_snapshot" {
		return false
	}
	token := anyToString(action["token"])
	now := s.recallResponseNow()
	s.recallResponseContinuationMu.Lock()
	s.pruneExpiredRecallResponseContinuationsLocked(now)
	record, found := s.recallResponseContinuations[token]
	if !found || record.AgentID != agentID || record.Endpoint != endpoint ||
		record.ScopeDigest != anyToString(action["scope_digest"]) || record.RequestDigest != anyToString(action["request_digest"]) {
		if found {
			delete(s.recallResponseContinuations, token)
		}
		s.recallResponseContinuationStats.InvalidRequests++
		s.recountRecallResponseContinuationsLocked()
		s.recallResponseContinuationMu.Unlock()
		recallResponseSetContinuationAction(response, recallResponseUnavailableContinuationAction(anyToString(action["scope_digest"])))
		recallResponseFitCandidateBudget(response)
		return true
	}
	if len(membership.Omitted) == 0 {
		delete(s.recallResponseContinuations, token)
		s.recountRecallResponseContinuationsLocked()
		s.recallResponseContinuationMu.Unlock()
		recallResponseSetContinuationAction(response, recallResponseTerminalContinuationAction(record.ScopeDigest))
		recallResponseFitCandidateBudget(response)
		return true
	}
	next := record
	next.Items = cloneJSONValue(membership.Omitted).([]any)
	next.Offset = 0
	next.Page = 1
	next.SourceMembershipDigest = membership.Digest
	next.ContinuationRef = anyToString(disclosure["continuation_ref"])
	next.StoredBytes = recallResponseContinuationRecordBytes(next)
	agentRecords, agentItems, agentBytes := 0, 0, 0
	globalRecords, globalItems, globalBytes := 0, 0, 0
	for existingToken, existing := range s.recallResponseContinuations {
		if existingToken == token {
			continue
		}
		existingBytes := existing.StoredBytes
		if existingBytes <= 0 {
			existingBytes = recallResponseContinuationRecordBytes(existing)
		}
		globalRecords++
		globalItems += len(existing.Items)
		globalBytes += existingBytes
		if existing.AgentID == next.AgentID {
			agentRecords++
			agentItems += len(existing.Items)
			agentBytes += existingBytes
		}
	}
	fairnessExceeded := agentRecords+1 > recallResponseContinuationMaximumRecordsPerAgent ||
		agentItems+len(next.Items) > recallResponseContinuationMaximumItemsPerAgent ||
		agentBytes+next.StoredBytes > recallResponseContinuationMaximumBytesPerAgent
	capacityExceeded := globalRecords+1 > recallResponseContinuationMaximumRecords ||
		globalItems+len(next.Items) > recallResponseContinuationMaximumStoredItems ||
		globalBytes+next.StoredBytes > recallResponseContinuationMaximumStoredBytes
	if fairnessExceeded || capacityExceeded {
		delete(s.recallResponseContinuations, token)
		if fairnessExceeded {
			s.recallResponseContinuationStats.FairnessRejected++
		} else {
			s.recallResponseContinuationStats.CapacityRejected++
		}
		s.recountRecallResponseContinuationsLocked()
		s.recallResponseContinuationMu.Unlock()
		recallResponseSetContinuationAction(response, recallResponseUnavailableContinuationAction(record.ScopeDigest))
		recallResponseFitCandidateBudget(response)
		return true
	}
	s.recallResponseContinuations[token] = next
	s.recountRecallResponseContinuationsLocked()
	s.recallResponseContinuationMu.Unlock()
	return false
}

func recallResponseContinuationRequest(payload map[string]any) (token, scopeDigest, requestDigest, agentID string, ok bool) {
	if !recallResponseExactFields(payload, []string{"continuation_token", "continuation_scope_digest", "continuation_request_digest", "agent_id"}) {
		return "", "", "", "", false
	}
	token = strings.TrimSpace(anyToString(payload["continuation_token"]))
	scopeDigest = strings.TrimSpace(anyToString(payload["continuation_scope_digest"]))
	requestDigest = strings.TrimSpace(anyToString(payload["continuation_request_digest"]))
	agentID = strings.TrimSpace(anyToString(payload["agent_id"]))
	return token, scopeDigest, requestDigest, agentID,
		recallResponseValidContinuationToken(token) && recallResponseValidDigest(scopeDigest) &&
			recallResponseValidDigest(requestDigest) && agentID != ""
}

func (s *server) resolveRecallResponseContinuation(payload map[string]any, endpoint string) (map[string]any, int) {
	token, scopeDigest, requestDigest, agentID, ok := recallResponseContinuationRequest(payload)
	if !ok || s == nil {
		if s != nil {
			s.recallResponseContinuationMu.Lock()
			s.recallResponseContinuationStats.InvalidRequests++
			s.recallResponseContinuationMu.Unlock()
		}
		return map[string]any{"error": "invalid continuation"}, http.StatusConflict
	}
	now := s.recallResponseNow()
	s.recallResponseContinuationMu.Lock()
	s.pruneExpiredRecallResponseContinuationsLocked(now)
	record, found := s.recallResponseContinuations[token]
	if !found || record.Endpoint != endpoint || record.AgentID != agentID ||
		record.ScopeDigest != scopeDigest || record.RequestDigest != requestDigest {
		s.recallResponseContinuationStats.InvalidRequests++
		s.recallResponseContinuationMu.Unlock()
		return map[string]any{"error": "invalid continuation"}, http.StatusConflict
	}
	end := minInt(record.Offset+recallResponseContinuationPageSize, len(record.Items))
	if record.Offset < 0 || record.Offset >= len(record.Items) || end <= record.Offset {
		s.recallResponseContinuationStats.InvalidRequests++
		s.recallResponseContinuationMu.Unlock()
		return map[string]any{"error": "continuation unavailable"}, http.StatusServiceUnavailable
	}
	pageItems := cloneJSONValue(record.Items[record.Offset:end]).([]any)
	exhausted := end == len(record.Items)
	nextAction := recallResponseTerminalContinuationAction(record.ScopeDigest)
	nextToken := ""
	next := record
	if !exhausted {
		var tokenOK bool
		nextToken, tokenOK = recallResponseNewContinuationToken()
		if !tokenOK {
			s.recallResponseContinuationMu.Unlock()
			return map[string]any{"error": "continuation unavailable"}, http.StatusServiceUnavailable
		}
		if _, collision := s.recallResponseContinuations[nextToken]; collision {
			s.recallResponseContinuationMu.Unlock()
			return map[string]any{"error": "continuation unavailable"}, http.StatusServiceUnavailable
		}
		next.Offset = end
		next.Page++
		nextAction = recallResponseContinuationAction(record.ScopeDigest, record.RequestDigest, endpoint, nextToken, next.Page)
	}
	pageMaterial := map[string]any{
		"snapshot_digest":          record.SnapshotDigest,
		"scope_digest":             record.ScopeDigest,
		"source_membership_digest": record.SourceMembershipDigest,
		"page":                     record.Page,
		"items":                    pageItems,
	}
	response := map[string]any{
		"schema_id":                recallResponseContinuationSchema,
		"snapshot_digest":          record.SnapshotDigest,
		"scope_digest":             record.ScopeDigest,
		"request_digest":           record.RequestDigest,
		"source_membership_digest": record.SourceMembershipDigest,
		"continuation_ref":         record.ContinuationRef,
		"page":                     record.Page,
		"item_count":               len(pageItems),
		"items":                    pageItems,
		"page_digest":              "sha256:" + sha256Hex(recallResponseCanonicalJSON(pageMaterial)),
		"expires_at":               record.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"exhausted":                exhausted,
		"continuation_action":      nextAction,
	}
	if !validateRecallResponseContinuationPage(response) {
		s.recallResponseContinuationMu.Unlock()
		return map[string]any{"error": "continuation unavailable"}, http.StatusServiceUnavailable
	}
	delete(s.recallResponseContinuations, token)
	if !exhausted {
		s.recallResponseContinuations[nextToken] = next
	}
	s.recallResponseContinuationStats.ResolvedPages++
	s.recountRecallResponseContinuationsLocked()
	s.recallResponseContinuationMu.Unlock()
	return response, http.StatusOK
}

func validateRecallResponseContinuationPage(response map[string]any) bool {
	if !recallResponseExactFields(response, []string{
		"schema_id", "snapshot_digest", "scope_digest", "request_digest", "source_membership_digest",
		"continuation_ref", "page", "item_count", "items", "page_digest", "expires_at", "exhausted", "continuation_action",
	}) || anyToString(response["schema_id"]) != recallResponseContinuationSchema ||
		!recallResponseValidDigest(anyToString(response["snapshot_digest"])) ||
		!recallResponseValidDigest(anyToString(response["scope_digest"])) ||
		!recallResponseValidDigest(anyToString(response["request_digest"])) ||
		!recallResponseValidDigest(anyToString(response["source_membership_digest"])) ||
		!recallResponseValidDigest(anyToString(response["page_digest"])) ||
		!recallResponseExactOpaqueID(anyToString(response["continuation_ref"]), "ref_continuation_") ||
		anyToInt(response["page"], 0) <= 0 {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, anyToString(response["expires_at"])); err != nil {
		return false
	}
	items := contextPackAnyList(response["items"])
	if len(items) == 0 || len(items) > recallResponseContinuationPageSize || anyToInt(response["item_count"], -1) != len(items) {
		return false
	}
	seen := map[string]bool{}
	for _, raw := range items {
		row := anyMap(raw)
		itemType := anyToString(row["item_type"])
		itemRef := anyToString(row["item_ref"])
		if !recallResponseExactFields(row, []string{"item_type", "item_ref", "disposition", "protected"}) ||
			!recallResponseOneOf(itemType, "evidence", "temporal", "proof", "conflict", "component") || strings.TrimSpace(itemRef) == "" ||
			!recallResponseOneOf(anyToString(row["disposition"]), "relevant", "hard_excluded", "conflict") || seen[itemType+"\x00"+itemRef] {
			return false
		}
		seen[itemType+"\x00"+itemRef] = true
	}
	pageMaterial := map[string]any{
		"snapshot_digest":          response["snapshot_digest"],
		"scope_digest":             response["scope_digest"],
		"source_membership_digest": response["source_membership_digest"],
		"page":                     response["page"],
		"items":                    items,
	}
	if anyToString(response["page_digest"]) != "sha256:"+sha256Hex(recallResponseCanonicalJSON(pageMaterial)) {
		return false
	}
	action := anyMap(response["continuation_action"])
	fakeResponse := map[string]any{
		"request_scope":  map[string]any{"scope_digest": response["scope_digest"]},
		"request_digest": response["request_digest"],
	}
	if !recallResponseContinuationActionValid(fakeResponse, action) {
		return false
	}
	return anyToBool(response["exhausted"]) == (anyToString(action["kind"]) == "terminal")
}
