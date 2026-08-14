package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func recallResponseMixedContinuationInput() (map[string]any, validatedRecallResponsePolicyInput) {
	input := recallResponseTestInput(false)
	input["as_of"] = recallResponseLatestAsOf
	pack := map[string]any{}
	classes := map[string][]any{
		"ranked_evidence": {},
		"temporal_claims": {},
		"proof_claims":    {},
		"conflicts":       {},
	}
	for index := 0; index < 48; index++ {
		candidate := func(offset int) string { return "rtc_" + fmt.Sprintf("%024x", offset+index+1) }
		digest := func(label string) string { return "sha256:" + sha256Hex(fmt.Sprintf("%s-%d", label, index)) }
		evidence := map[string]any{
			"candidate_id": candidate(0), "kind": "decision", "confidence": 0.91,
			"source": "server-evidence", "content_digest": digest("evidence"), "status": "active",
			"observed_at": "2026-01-01T00:00:00Z",
		}
		if index == 0 {
			evidence["required_for_action"] = true
		} else {
			evidence["support"] = "context"
		}
		classes["ranked_evidence"] = append(classes["ranked_evidence"], evidence)
		temporalStatus := "active"
		if index%3 == 0 {
			temporalStatus = "revoked"
		}
		classes["temporal_claims"] = append(classes["temporal_claims"], map[string]any{
			"candidate_id": candidate(100), "kind": "temporal_claim", "confidence": 0.87,
			"source": "server-temporal", "content_digest": digest("temporal"), "status": temporalStatus,
			"support":     "context",
			"observed_at": "2026-01-02T00:00:00Z",
		})
		classes["proof_claims"] = append(classes["proof_claims"], map[string]any{
			"candidate_id": candidate(200), "kind": "proof_claim", "confidence": 0.84,
			"source": "server-proof", "content_digest": digest("proof"), "status": "selected",
			"support":     "context",
			"observed_at": "2026-01-03T00:00:00Z",
		})
		if index < 4 {
			classes["conflicts"] = append(classes["conflicts"], map[string]any{
				"conflict_id": "conflict-" + candidate(300), "kind": "contradiction", "status": "contested",
				"statement": fmt.Sprintf("opaque conflict %d", index),
			})
		}
	}
	for key, rows := range classes {
		pack[key] = rows
	}
	input["context_pack"] = recallResponseServerOwnedSourcePack(pack, classes["ranked_evidence"])
	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("a", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("b", 64)
	policy.evidenceBindings = recallResponseValidatedEvidenceBindings(input, "validated_policy", nil)
	return input, policy
}

func TestRecallResponseContinuationPagesMixedSnapshotWithoutLooping(t *testing.T) {
	input, policy := recallResponseMixedContinuationInput()
	baseNow := time.Date(2026, time.August, 10, 12, 0, 0, 0, time.UTC)
	now := baseNow
	s := newTestServer(t, "http://127.0.0.1:1")
	s.recallResponseContinuationNow = func() time.Time { return now }
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()
	request := recallResponseRequestPayload(map[string]any{
		"query": input["query"], "project": input["project"], "topic_path": input["topic_path"],
		"agent_id": input["agent_id"], "retrieval_mode": input["retrieval_mode"],
	})
	issue := func() (map[string]any, map[string]any) {
		response := composeRecallResponseWithPolicy(cloneJSONMap(input), policy)
		s.installRecallResponseContinuation(response, input, request, policy, "agent-alpha", memoryRecallResponsePath)
		compactBytes, compactTokens := recallResponseCompactBudget(response)
		if !validateRecallResponseU2(response) || compactBytes > recallResponseMaxCompactBytes || compactTokens > recallResponseMaxCompactTokens {
			t.Fatalf("cursor installation broke the closed response: bytes=%d tokens=%d", compactBytes, compactTokens)
		}
		action := anyMap(recallResponseDisclosure(response)["continuation_action"])
		if !recallResponseContinuationActionValid(response, action) || anyToString(action["kind"]) != "continue_snapshot" || anyToString(action["snapshot_semantics"]) != "same_snapshot" {
			t.Fatalf("owner did not issue a same-snapshot cursor: %#v", action)
		}
		return response, action
	}
	postAction := func(action map[string]any, scopeDigest, agentID string) (*http.Response, map[string]any, string) {
		body, err := json.Marshal(map[string]any{
			"continuation_token": action["token"], "continuation_scope_digest": scopeDigest,
			"continuation_request_digest": action["request_digest"], "agent_id": agentID,
		})
		if err != nil {
			t.Fatalf("marshal continuation request: %v", err)
		}
		return recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, string(body), nil)
	}

	initial, action := issue()
	visible := map[string]bool{}
	disclosure := recallResponseDisclosure(initial)
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		row := anyMap(raw)
		visible[recallResponseTypedItemKey(anyToString(row["item_type"]), anyToString(row["ref_id"]))] = true
	}
	if rows := contextPackAnyList(disclosure["evidence_union"]); len(rows) == 0 || !anyToBool(anyMap(rows[0])["protected"]) {
		t.Fatalf("source-bound protected evidence did not survive presentation pruning: %#v", rows)
	}
	for _, raw := range contextPackAnyList(disclosure["proof_union"]) {
		visible[recallResponseTypedItemKey("proof", anyToString(raw))] = true
	}
	for _, raw := range contextPackAnyList(initial["conflicts"]) {
		visible[recallResponseTypedItemKey("conflict", anyToString(anyMap(raw)["conflict_id"]))] = true
	}

	// A formatted token not owned by the server fails without consuming the
	// real cursor.
	tampered := cloneJSONMap(action)
	token := anyToString(tampered["token"])
	last := "0"
	if strings.HasSuffix(token, "0") {
		last = "1"
	}
	tampered["token"] = token[:len(token)-1] + last
	if resp, _, _ := postAction(tampered, anyToString(action["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("tampered cursor did not fail closed: %d", resp.StatusCode)
	}

	seen := map[string]bool{}
	types := map[string]bool{"conflict": len(contextPackAnyList(initial["conflicts"])) > 0}
	hardExcluded := false
	conflictSeen := len(contextPackAnyList(initial["conflicts"])) > 0
	current := action
	for expectedPage := 1; ; expectedPage++ {
		resp, page, raw := postAction(current, anyToString(action["scope_digest"]), "agent-alpha")
		if resp.StatusCode != http.StatusOK || !validateRecallResponseContinuationPage(page) {
			t.Fatalf("page %d failed: status=%d body=%s", expectedPage, resp.StatusCode, raw)
		}
		if anyToInt(page["page"], 0) != expectedPage || anyToString(page["snapshot_digest"]) != anyToString(anyMap(initial["request_scope"])["snapshot_digest"]) {
			t.Fatalf("page identity drifted: %#v", page)
		}
		for _, rawItem := range contextPackAnyList(page["items"]) {
			item := anyMap(rawItem)
			refID := anyToString(item["item_ref"])
			key := anyToString(item["item_type"]) + "\x00" + refID
			if visible[key] || seen[key] {
				t.Fatalf("continuation repeated initial or prior membership: %#v", item)
			}
			seen[key] = true
			types[anyToString(item["item_type"])] = true
			hardExcluded = hardExcluded || anyToString(item["disposition"]) == "hard_excluded"
			conflictSeen = conflictSeen || anyToString(item["disposition"]) == "conflict"
		}
		if anyToBool(page["exhausted"]) {
			if anyToString(anyMap(page["continuation_action"])["kind"]) != "terminal" || expectedPage < 2 {
				t.Fatalf("continuation did not end after additive pages: %#v", page)
			}
			break
		}
		current = anyMap(page["continuation_action"])
	}
	if len(seen) <= recallResponseContinuationPageSize || len(types) != 5 || !hardExcluded || !conflictSeen {
		t.Fatalf("mixed continuation lost a class or hard exclusion: seen=%d types=%v hard=%v conflict=%v", len(seen), types, hardExcluded, conflictSeen)
	}
	if resp, _, _ := postAction(action, anyToString(action["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("consumed cursor replay did not fail closed: %d", resp.StatusCode)
	}

	// Scope, identity, and expiry are independently bound to each one-use token.
	now = baseNow
	_, wrongScope := issue()
	if resp, _, _ := postAction(wrongScope, "sha256:"+strings.Repeat("c", 64), "agent-alpha"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong-scope cursor did not fail closed: %d", resp.StatusCode)
	}
	if resp, _, _ := postAction(wrongScope, anyToString(wrongScope["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong-scope request consumed a valid cursor: %d", resp.StatusCode)
	}
	_, wrongAgent := issue()
	if resp, _, _ := postAction(wrongAgent, anyToString(wrongAgent["scope_digest"]), "agent-beta"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong-agent cursor did not fail closed: %d", resp.StatusCode)
	}
	if resp, _, _ := postAction(wrongAgent, anyToString(wrongAgent["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong-agent request consumed a valid cursor: %d", resp.StatusCode)
	}
	_, wrongEndpoint := issue()
	foreignEndpointPayload := map[string]any{
		"continuation_token": wrongEndpoint["token"], "continuation_scope_digest": wrongEndpoint["scope_digest"],
		"continuation_request_digest": wrongEndpoint["request_digest"], "agent_id": "agent-alpha",
	}
	if _, status := s.resolveRecallResponseContinuation(foreignEndpointPayload, toolsRecallResponsePath); status != http.StatusConflict {
		t.Fatalf("wrong-endpoint cursor did not fail closed: %d", status)
	}
	if resp, _, _ := postAction(wrongEndpoint, anyToString(wrongEndpoint["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong-endpoint request consumed a valid cursor: %d", resp.StatusCode)
	}
	_, wrongRequest := issue()
	foreignRequest := cloneJSONMap(wrongRequest)
	foreignRequest["request_digest"] = "sha256:" + strings.Repeat("d", 64)
	if resp, _, _ := postAction(foreignRequest, anyToString(wrongRequest["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("wrong-request cursor did not fail closed: %d", resp.StatusCode)
	}
	if resp, _, _ := postAction(wrongRequest, anyToString(wrongRequest["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusOK {
		t.Fatalf("wrong-request binding consumed a valid cursor: %d", resp.StatusCode)
	}
	_, stale := issue()
	now = baseNow.Add(recallResponseContinuationTTL + time.Second)
	if resp, _, _ := postAction(stale, anyToString(stale["scope_digest"]), "agent-alpha"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("stale cursor did not fail closed: %d", resp.StatusCode)
	}
}

func recallResponseContinuationTestRecord(agent string, now time.Time, padding int) recallResponseContinuationRecord {
	itemRef := "rtc_" + strings.Repeat("a", 24)
	if padding > 0 {
		itemRef += strings.Repeat("x", padding)
	}
	record := recallResponseContinuationRecord{
		AgentID: agent, Endpoint: memoryRecallResponsePath,
		ScopeDigest: "sha256:" + strings.Repeat("1", 64), SnapshotDigest: "sha256:" + strings.Repeat("2", 64),
		RequestDigest: "sha256:" + strings.Repeat("3", 64), SourceMembershipDigest: "sha256:" + strings.Repeat("4", 64),
		ContinuationRef: "ref_continuation_" + strings.Repeat("5", 24),
		Items:           []any{map[string]any{"item_type": "evidence", "item_ref": itemRef, "disposition": "relevant", "protected": false}},
		Page:            1, CreatedAt: now, ExpiresAt: now.Add(recallResponseContinuationTTL),
	}
	record.StoredBytes = recallResponseContinuationRecordBytes(record)
	return record
}

func recallResponseContinuationTestToken(index int) string {
	return "rrct_" + fmt.Sprintf("%048x", index)
}

func recallResponseContinuationTestItemsRecord(agent string, now time.Time, count int) recallResponseContinuationRecord {
	record := recallResponseContinuationTestRecord(agent, now, 0)
	record.Items = make([]any, 0, count)
	for index := 0; index < count; index++ {
		record.Items = append(record.Items, map[string]any{
			"item_type": "evidence", "item_ref": "rtc_" + fmt.Sprintf("%024x", index+1),
			"disposition": "relevant", "protected": false,
		})
	}
	record.StoredBytes = recallResponseContinuationRecordBytes(record)
	return record
}

func TestRecallResponseContinuationStoreCapacityFairnessExpiryAndTelemetry(t *testing.T) {
	now := time.Date(2026, time.August, 10, 13, 0, 0, 0, time.UTC)
	fair := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	for index := 1; index <= recallResponseContinuationMaximumRecordsPerAgent; index++ {
		if !fair.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(index), recallResponseContinuationTestRecord("agent-a", now, 0)) {
			t.Fatalf("fairness store rejected admitted record %d", index)
		}
	}
	if fair.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(100), recallResponseContinuationTestRecord("agent-a", now, 0)) {
		t.Fatal("per-agent record bound admitted an unfair record")
	}
	if !fair.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(101), recallResponseContinuationTestRecord("agent-b", now, 0)) {
		t.Fatal("one agent's fairness bound blocked another agent")
	}
	if telemetry := fair.recallResponseContinuationTelemetrySnapshot(); telemetry.FairnessRejected != 1 {
		t.Fatalf("fairness rejection was not observable: %#v", telemetry)
	}

	itemFair := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	if !itemFair.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(1), recallResponseContinuationTestItemsRecord("item-agent", now, recallResponseContinuationMaximumItemsPerAgent)) {
		t.Fatal("per-agent item store rejected its exact bound")
	}
	if itemFair.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(2), recallResponseContinuationTestItemsRecord("item-agent", now, 1)) {
		t.Fatal("per-agent item bound admitted an unfair item")
	}
	if !itemFair.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(3), recallResponseContinuationTestItemsRecord("other-item-agent", now, 1)) {
		t.Fatal("per-agent item fairness blocked another agent")
	}

	agentBytes := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	if !agentBytes.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(1), recallResponseContinuationTestRecord("byte-agent", now, 700*1024)) {
		t.Fatal("per-agent byte store rejected a bounded record")
	}
	if agentBytes.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(2), recallResponseContinuationTestRecord("byte-agent", now, 700*1024)) {
		t.Fatal("per-agent byte bound admitted an unfair record")
	}

	capacity := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	index := 1
	for agent := 0; agent < recallResponseContinuationMaximumRecords/recallResponseContinuationMaximumRecordsPerAgent; agent++ {
		for recordIndex := 0; recordIndex < recallResponseContinuationMaximumRecordsPerAgent; recordIndex++ {
			if !capacity.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(index), recallResponseContinuationTestRecord(fmt.Sprintf("agent-%d", agent), now, 0)) {
				t.Fatalf("global store rejected record %d before its exact bound", index)
			}
			index++
		}
	}
	if capacity.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(index), recallResponseContinuationTestRecord("agent-extra", now, 0)) {
		t.Fatal("global record bound admitted an over-capacity record")
	}
	if telemetry := capacity.recallResponseContinuationTelemetrySnapshot(); telemetry.CapacityRejected != 1 || telemetry.CurrentRecords != recallResponseContinuationMaximumRecords {
		t.Fatalf("global capacity telemetry drifted: %#v", telemetry)
	}

	itemsStore := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	if !itemsStore.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(1), recallResponseContinuationTestItemsRecord("global-item-a", now, recallResponseContinuationMaximumItemsPerAgent)) ||
		!itemsStore.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(2), recallResponseContinuationTestItemsRecord("global-item-b", now, recallResponseContinuationMaximumItemsPerAgent)) {
		t.Fatal("global item store rejected its exact bound")
	}
	if itemsStore.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(3), recallResponseContinuationTestItemsRecord("global-item-c", now, 1)) {
		t.Fatal("global item bound admitted an over-capacity item")
	}

	bytesStore := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	for agent := 0; agent < 2; agent++ {
		if !bytesStore.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(agent+1), recallResponseContinuationTestRecord(fmt.Sprintf("byte-agent-%d", agent), now, 700*1024)) {
			t.Fatalf("byte store rejected bounded record %d", agent)
		}
	}
	if bytesStore.admitRecallResponseContinuationLocked(recallResponseContinuationTestToken(3), recallResponseContinuationTestRecord("byte-agent-2", now, 700*1024)) {
		t.Fatal("global byte bound admitted an over-capacity record")
	}

	expiry := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	expired := recallResponseContinuationTestRecord("expired-agent", now, 0)
	expired.ExpiresAt = now
	expired.StoredBytes = recallResponseContinuationRecordBytes(expired)
	expiry.recallResponseContinuations[recallResponseContinuationTestToken(1)] = expired
	expiry.pruneExpiredRecallResponseContinuationsLocked(now)
	if telemetry := expiry.recallResponseContinuationTelemetrySnapshot(); telemetry.ExpiredEvicted != 1 || telemetry.CurrentRecords != 0 {
		t.Fatalf("expiry eviction was not deterministic and observable: %#v", telemetry)
	}

	discard := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	discardToken := recallResponseContinuationTestToken(1)
	discard.recallResponseContinuations[discardToken] = recallResponseContinuationTestRecord("discard-agent", now, 0)
	discard.discardRecallResponseContinuation(map[string]any{"disclosure": map[string]any{
		"continuation_action": map[string]any{"token": discardToken},
	}})
	if telemetry := discard.recallResponseContinuationTelemetrySnapshot(); telemetry.Discarded != 1 || telemetry.CurrentRecords != 0 {
		t.Fatalf("discarded unserved cursor was not observable: %#v", telemetry)
	}
}

func TestRecallResponseContinuationVisibleMembershipUsesTypedKeys(t *testing.T) {
	input := recallResponseTestInput(false)
	shared := "rtc_" + strings.Repeat("e", 24)
	row := func(kind string) map[string]any {
		return map[string]any{
			"candidate_id": shared, "kind": kind, "confidence": 0.9, "status": "selected",
			"source": "server", "content_digest": "sha256:" + strings.Repeat("f", 64),
			"observed_at": "2026-01-01T00:00:00Z", "support": "context",
		}
	}
	rankedEvidence := []any{row("fact")}
	input["context_pack"] = recallResponseServerOwnedSourcePack(map[string]any{
		"ranked_evidence": rankedEvidence,
		"temporal_claims": []any{func() map[string]any {
			temporal := row("temporal_claim")
			temporal["status"] = "revoked"
			return temporal
		}()},
		"proof_claims": []any{row("proof_claim")},
	}, rankedEvidence)
	policy := recallResponseProductionPolicyInput()
	response := composeRecallResponseWithPolicy(cloneJSONMap(input), policy)
	disclosure := recallResponseDisclosure(response)
	typedEvidence := []any{}
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		if anyToString(anyMap(raw)["item_type"]) == "evidence" {
			typedEvidence = append(typedEvidence, raw)
			break
		}
	}
	disclosure["evidence_union"] = typedEvidence
	disclosure["proof_union"] = []any{}
	membership, ok := recallResponseContinuationMembership(input, response, policy)
	if !ok {
		t.Fatal("typed continuation membership could not be materialized")
	}
	all := map[string]bool{}
	omitted := map[string]bool{}
	for _, raw := range membership.All {
		item := anyMap(raw)
		all[recallResponseTypedItemKey(anyToString(item["item_type"]), anyToString(item["item_ref"]))] = true
	}
	for _, raw := range membership.Omitted {
		item := anyMap(raw)
		omitted[recallResponseTypedItemKey(anyToString(item["item_type"]), anyToString(item["item_ref"]))] = true
	}
	if !all[recallResponseTypedItemKey("evidence", shared)] || !all[recallResponseTypedItemKey("temporal", shared)] ||
		!all[recallResponseTypedItemKey("proof", shared)] || omitted[recallResponseTypedItemKey("evidence", shared)] ||
		!omitted[recallResponseTypedItemKey("temporal", shared)] || !omitted[recallResponseTypedItemKey("proof", shared)] {
		t.Fatalf("cross-class duplicate collapsed across typed visibility: all=%v omitted=%v", all, omitted)
	}
}

func TestRecallResponseContinuationMembershipAppliesCompleteHardPolicy(t *testing.T) {
	input := recallResponseTestInput(false)
	input["as_of"] = "2026-01-01T00:00:00Z"
	digest := func(index int) string { return fmt.Sprintf("sha256:%064x", index+1) }
	candidate := func(index int) string { return fmt.Sprintf("rtc_%024x", index+1) }
	row := func(index int) map[string]any {
		return map[string]any{
			"candidate_id": candidate(index), "kind": "fact", "confidence": 0.9,
			"status": "current", "source": "server", "source_ref": "server/ref/" + candidate(index),
			"content_digest": digest(index), "occurred_at": "2025-12-01T00:00:00Z",
		}
	}
	rows := []any{row(0), row(1), row(2), row(3), row(4)}
	stale := row(5)
	stale["freshness"] = "stale"
	rows = append(rows, stale)
	retired := row(6)
	retired["status"] = "retired"
	rows = append(rows, retired)
	revoked := row(7)
	revoked["status"] = "revoked"
	rows = append(rows, revoked)
	quarantined := row(8)
	quarantined["status"] = "quarantined"
	rows = append(rows, quarantined)
	future := row(9)
	future["occurred_at"] = "2027-01-01T00:00:00Z"
	rows = append(rows, future)
	missingCurrent := row(10)
	delete(missingCurrent, "candidate_id")
	missingCurrent["source_ref"] = "server/ref/missing-current"
	rows = append(rows, missingCurrent)
	missingFuture := row(11)
	delete(missingFuture, "candidate_id")
	missingFuture["source_ref"] = "server/ref/missing-future"
	missingFuture["occurred_at"] = "2027-02-01T00:00:00Z"
	rows = append(rows, missingFuture)
	input["context_pack"] = recallResponseServerOwnedSourcePack(
		map[string]any{"ranked_evidence": rows}, rows,
	)
	policy := recallResponseProductionPolicyInput()
	response := composeRecallResponseWithPolicy(cloneJSONMap(input), policy)
	membership, ok := recallResponseContinuationMembership(input, response, policy)
	if !ok {
		t.Fatal("complete server-owned continuation membership was rejected")
	}
	classes := recallResponseCanonicalSourceRows(input)
	if got, want := len(classes["evidence"]), len(rows); got != want {
		t.Fatalf("canonical source rows were clipped before continuation membership: got=%d want=%d", got, want)
	}
	hard := map[string]bool{}
	for _, raw := range []any{stale, retired, revoked, quarantined, future, missingFuture} {
		hard[recallResponseCanonicalSourceRef(anyMap(raw), "evidence")] = true
	}
	relevant := map[string]bool{}
	seen := map[string]bool{}
	for _, raw := range membership.All {
		item := anyMap(raw)
		if anyToString(item["item_type"]) != "evidence" {
			continue
		}
		ref := anyToString(item["item_ref"])
		seen[ref] = true
		if anyToString(item["disposition"]) == "relevant" {
			relevant[ref] = true
		}
		if hard[ref] && anyToString(item["disposition"]) == "relevant" {
			t.Fatalf("hard-excluded row was marked relevant: ref=%s item=%#v", ref, item)
		}
		if ref == "" {
			t.Fatal("continuation membership emitted an empty source identity")
		}
	}
	if len(seen) != len(rows) || len(relevant) < 6 {
		t.Fatalf("complete membership lost >4 or missing-ID source rows: seen=%d want=%d relevant=%d all=%#v", len(seen), len(rows), len(relevant), membership.All)
	}
	missingCurrentRef := recallResponseCanonicalSourceRef(missingCurrent, "evidence")
	if !seen[missingCurrentRef] || !relevant[missingCurrentRef] {
		t.Fatalf("eligible missing-ID row was not assigned a stable canonical membership identity: ref=%s all=%#v", missingCurrentRef, membership.All)
	}
	for ref := range hard {
		if !seen[ref] {
			t.Fatalf("hard-excluded source row was omitted from complete membership: ref=%s", ref)
		}
	}
	for _, raw := range contextPackAnyList(anyMap(response["disclosure"])["evidence_union"]) {
		ref := anyToString(anyMap(raw)["ref_id"])
		if hard[ref] {
			t.Fatalf("hard-excluded source row crossed the supporting evidence union: ref=%s", ref)
		}
	}

	// Exercise the owner-held cursor through to its terminal page. Wire pages
	// are bounded, while the complete typed membership remains in the record.
	s := &server{recallResponseContinuations: map[string]recallResponseContinuationRecord{}}
	s.recallResponseContinuationNow = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	request := recallResponseRequestPayload(input)
	s.installRecallResponseContinuationWithFit(response, input, request, policy, "adversarial-agent", memoryRecallResponsePath, false)
	action := anyMap(recallResponseDisclosure(response)["continuation_action"])
	if anyToString(action["kind"]) != "continue_snapshot" {
		t.Fatalf("complete hard-policy adversary did not receive a cursor: %#v", action)
	}
	record, found := s.recallResponseContinuations[anyToString(action["token"])]
	if !found || record.SourceMembershipDigest != membership.Digest || len(record.Items) != len(membership.Omitted) {
		t.Fatalf("cursor did not retain complete server membership: found=%v record=%#v omitted=%d", found, record, len(membership.Omitted))
	}
	traversed := map[string]bool{}
	current := action
	for pageNumber := 1; ; pageNumber++ {
		page, status := s.resolveRecallResponseContinuation(map[string]any{
			"continuation_token":          current["token"],
			"continuation_scope_digest":   current["scope_digest"],
			"continuation_request_digest": current["request_digest"],
			"agent_id":                    "adversarial-agent",
		}, memoryRecallResponsePath)
		if status != http.StatusOK || !validateRecallResponseContinuationPage(page) || anyToInt(page["page"], 0) != pageNumber {
			t.Fatalf("terminal adversarial traversal page %d invalid: status=%d page=%#v", pageNumber, status, page)
		}
		for _, raw := range contextPackAnyList(page["items"]) {
			item := anyMap(raw)
			key := recallResponseTypedItemKey(anyToString(item["item_type"]), anyToString(item["item_ref"]))
			if traversed[key] {
				t.Fatalf("terminal adversarial traversal repeated %s", key)
			}
			traversed[key] = true
		}
		if anyToBool(page["exhausted"]) {
			if anyToString(anyMap(page["continuation_action"])["kind"]) != "terminal" {
				t.Fatalf("terminal adversarial traversal did not return a terminal action: %#v", page)
			}
			break
		}
		current = anyMap(page["continuation_action"])
	}
	if len(traversed) != len(membership.Omitted) {
		t.Fatalf("terminal adversarial traversal lost membership: traversed=%d omitted=%d", len(traversed), len(membership.Omitted))
	}
}

func TestRecallResponseContinuationTokenIsTransportOnly(t *testing.T) {
	input := recallResponseTestInput(true)
	policy := recallResponseProductionPolicyInput()
	response := composeRecallResponseWithPolicy(input, policy)
	scopeDigest := anyToString(anyMap(response["request_scope"])["scope_digest"])
	requestDigest := recallResponseContinuationRequestDigest(recallResponseRequestPayload(input))
	action := recallResponseContinuationAction(scopeDigest, requestDigest, memoryRecallResponsePath, "rrct_"+strings.Repeat("a", recallResponseContinuationTokenBytes*2), 1)
	recallResponseSetContinuationAction(response, action)
	stable := map[string]any{
		"semantic_material": recallResponseStableIdentityMaterial(response),
		"response_id":       response["response_id"],
		"response_digest":   response["response_digest"],
		"semantic_digest":   recallResponseSemanticDigest(response),
		"union_digest":      anyMap(response["disclosure"])["union_digest"],
		"control_receipt":   anyMap(anyMap(response["disclosure"])["control_receipt"])["artifact_digest"],
		"omission_ledger":   anyMap(response["disclosure"])["omission_ledger"],
	}
	secondAction := recallResponseContinuationAction(scopeDigest, requestDigest, memoryRecallResponsePath, "rrct_"+strings.Repeat("b", recallResponseContinuationTokenBytes*2), 1)
	recallResponseSetContinuationAction(response, secondAction)
	second := map[string]any{
		"semantic_material": recallResponseStableIdentityMaterial(response),
		"response_id":       response["response_id"],
		"response_digest":   response["response_digest"],
		"semantic_digest":   recallResponseSemanticDigest(response),
		"union_digest":      anyMap(response["disclosure"])["union_digest"],
		"control_receipt":   anyMap(anyMap(response["disclosure"])["control_receipt"])["artifact_digest"],
		"omission_ledger":   anyMap(response["disclosure"])["omission_ledger"],
	}
	if recallResponseCanonicalJSON(stable) != recallResponseCanonicalJSON(second) {
		t.Fatalf("opaque transport cursor contaminated stable response identities:\nfirst=%s\nsecond=%s", recallResponseCanonicalJSON(stable), recallResponseCanonicalJSON(second))
	}
	if anyToString(anyMap(recallResponseDisclosure(response)["continuation_action"])["token"]) != anyToString(secondAction["token"]) {
		t.Fatal("transport cursor was not updated in the server-owned action")
	}
}
