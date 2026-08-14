package main

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// The non-exclusion surface is deliberately nested under disclosure so the
// existing recall_response.v1 control fields remain wire-compatible.  It is
// an evidence/proof accounting surface, not a second answer channel.
const (
	recallResponseNonExclusionSchema = "recall_response.non_exclusion.v1"
	// The generated transport contract permits up to 32 rows, but the complete
	// response must also remain below 16KB/4K tokens. Exclusion receipts retain
	// the four-row temporal bound while evidence/proof samples display two rows
	// each; complete membership remains in counts, digests, and continuation.
	recallResponseMaxUnionRefs             = 4
	recallResponseMaxPresentedEvidenceRefs = 2
	recallResponseMaxPresentedProofRefs    = 2
	recallResponseMaxPresentedComponents   = 5
	recallResponseMaxOmissionLedger        = 2
	// Source accounting remains independently bounded at 128 rows per class so
	// a mixed >128-row snapshot is digested before presentation clipping. The
	// private retrieval bridge has the larger owner-cursor bound below: it must
	// retain every row it claims can be reached by a continuation, not merely a
	// digest of rows that were discarded before response compilation.
	recallResponseMaxSourceCapture        = 128
	recallResponseMaxSourceInputCapture   = recallResponseContinuationMaximumItems
	recallResponseSourceSnapshotSchema    = "recall_response.source_snapshot.v1"
	recallResponseSourceInputKey          = "_recall_response_source_input"
	recallResponseSourceInputSchema       = "recall_response.source_input.v1"
	recallResponseGraphRowsKey            = "_recall_response_graph_rows"
	recallResponseTemporalPartitionKey    = "_recall_response_temporal_partition_receipt"
	recallResponseTemporalPartitionSchema = "recall_response.temporal_partition_receipt.v1"
)

// recallResponseBuildSourceInput is installed only by executeRetrieval when
// the unexported context-pack compiler marker is present. It preserves the
// complete bounded server-owned source membership plus exact pre-clipping
// count and membership digest, so a retrieval limit cannot silently become the
// response policy's source boundary. When the retrieval attempt exceeds the
// owner bound, the envelope remains explicitly incomplete and continuation
// custody must fail closed.
func recallResponseBuildSourceInput(rows []map[string]any) map[string]any {
	allRows := make([]any, 0, len(rows))
	for _, row := range rows {
		allRows = append(allRows, row)
	}
	capturedRows := allRows
	if len(capturedRows) > recallResponseMaxSourceInputCapture {
		capturedRows = capturedRows[:recallResponseMaxSourceInputCapture]
	}
	return map[string]any{
		"schema_id":                  recallResponseSourceInputSchema,
		"bounded":                    true,
		"candidate_count":            len(allRows),
		"captured_count":             len(capturedRows),
		"omitted_count":              maxInt(len(allRows)-len(capturedRows), 0),
		"membership_digest":          recallResponseSourceIdentityDigest(allRows),
		"captured_membership_digest": recallResponseSourceIdentityDigest(capturedRows),
		"complete":                   len(allRows) == len(capturedRows),
		"rows":                       cloneJSONValue(capturedRows),
	}
}

func recallResponseSourceInputRows(value any) ([]map[string]any, bool) {
	input := anyMap(value)
	if anyToString(input["schema_id"]) != recallResponseSourceInputSchema || !anyToBool(input["bounded"]) {
		return nil, false
	}
	candidateCount := anyToInt(input["candidate_count"], -1)
	capturedCount := anyToInt(input["captured_count"], -1)
	omittedCount := anyToInt(input["omitted_count"], -1)
	if candidateCount < 0 || capturedCount < 0 || omittedCount < 0 ||
		capturedCount > candidateCount ||
		capturedCount > recallResponseMaxSourceInputCapture ||
		omittedCount != maxInt(candidateCount-capturedCount, 0) ||
		!recallResponseValidDigest(anyToString(input["membership_digest"])) ||
		!recallResponseValidDigest(anyToString(input["captured_membership_digest"])) {
		return nil, false
	}
	rows := parseRows(input["rows"])
	if len(rows) != capturedCount ||
		recallResponseSourceIdentityDigest(rowsToAny(rows)) != anyToString(input["captured_membership_digest"]) ||
		anyToBool(input["complete"]) != (candidateCount == capturedCount) {
		return nil, false
	}
	return rows, true
}

func rowsToAny(rows []map[string]any) []any {
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	return out
}

func recallResponseSourceIdentityDigest(rows []any) string {
	accumulator := recallResponseDigestAccumulator{}
	for index, raw := range rows {
		accumulator.add("evidence\x00" + recallResponseRawUnionIdentity(raw, "evidence", index))
	}
	return accumulator.digest()
}

func recallResponseSourceIdentityDigestFromRefs(refs []string) string {
	accumulator := recallResponseDigestAccumulator{}
	for _, ref := range refs {
		accumulator.add("evidence\x00" + ref)
	}
	return accumulator.digest()
}

func recallResponseSourceRowDigest(raw any) string {
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(raw))
}

func recallResponseSourceContentDigestFromRows(rows []any) string {
	digests := make([]any, 0, len(rows))
	for _, raw := range rows {
		digests = append(digests, recallResponseSourceRowDigest(raw))
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(digests))
}

func recallResponseSourceContentDigestFromDigests(digests []string) string {
	values := make([]any, 0, len(digests))
	for _, digest := range digests {
		values = append(values, digest)
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(values))
}

func recallResponseTemporalPartitionReceiptDigest(receipt map[string]any) string {
	material := cloneJSONMap(receipt)
	delete(material, "receipt_digest")
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseBuildTemporalPartitionClass(rows []any, asOf string, asOfValid bool) ([]any, map[string]any, int) {
	retained := make([]any, 0, len(rows))
	entries := make([]any, 0, len(rows))
	originalRefs := make([]string, 0, len(rows))
	originalDigests := make([]string, 0, len(rows))
	retainedRefs := []string{}
	retainedDigests := []string{}
	excludedRefs := []string{}
	excludedDigests := []string{}
	unverifiable := 0
	for index, raw := range rows {
		itemRef := recallResponseRawUnionIdentity(raw, "evidence", index)
		originalDigest := recallResponseSourceRowDigest(raw)
		originalRefs = append(originalRefs, itemRef)
		originalDigests = append(originalDigests, originalDigest)
		disposition := "excluded"
		retainedDigest := ""
		if asOfValid {
			filtered, rejected := recallResponseTemporalEvidenceAtOrBefore([]any{raw}, asOf)
			unverifiable += rejected
			if len(filtered) == 1 {
				projected := cloneJSONValue(filtered[0])
				retained = append(retained, projected)
				disposition = "retained"
				retainedDigest = recallResponseSourceRowDigest(projected)
				retainedRefs = append(retainedRefs, itemRef)
				retainedDigests = append(retainedDigests, retainedDigest)
			} else {
				excludedRefs = append(excludedRefs, itemRef)
				excludedDigests = append(excludedDigests, originalDigest)
			}
		} else {
			unverifiable++
			excludedRefs = append(excludedRefs, itemRef)
			excludedDigests = append(excludedDigests, originalDigest)
		}
		entries = append(entries, map[string]any{
			"ordinal": index, "item_ref": itemRef, "disposition": disposition,
			"original_row_digest": originalDigest, "retained_row_digest": retainedDigest,
		})
	}
	partition := map[string]any{
		"original_count": len(rows), "original_membership_digest": recallResponseSourceIdentityDigestFromRefs(originalRefs),
		"original_content_digest": recallResponseSourceContentDigestFromDigests(originalDigests),
		"retained_count":          len(retainedRefs), "retained_membership_digest": recallResponseSourceIdentityDigestFromRefs(retainedRefs),
		"retained_content_digest": recallResponseSourceContentDigestFromDigests(retainedDigests),
		"excluded_count":          len(excludedRefs), "excluded_membership_digest": recallResponseSourceIdentityDigestFromRefs(excludedRefs),
		"excluded_content_digest": recallResponseSourceContentDigestFromDigests(excludedDigests),
		"entries":                 entries,
	}
	return retained, partition, unverifiable
}

func recallResponseBuildTemporalPartitionReceipt(sourceRows, graphRows []any, asOf string, asOfValid bool) ([]any, []any, map[string]any, int) {
	retainedSource, sourcePartition, sourceUnverifiable := recallResponseBuildTemporalPartitionClass(sourceRows, asOf, asOfValid)
	retainedGraph, graphPartition, graphUnverifiable := recallResponseBuildTemporalPartitionClass(graphRows, asOf, asOfValid)
	receipt := map[string]any{
		"schema_id": recallResponseTemporalPartitionSchema, "bounded": true,
		"as_of": asOf, "as_of_valid": asOfValid,
		"source": sourcePartition, "graph": graphPartition,
	}
	receipt["receipt_digest"] = recallResponseTemporalPartitionReceiptDigest(receipt)
	return retainedSource, retainedGraph, receipt, sourceUnverifiable + graphUnverifiable
}

func recallResponseTemporalPartitionClassValid(carrier []any, partition map[string]any, expectedCount int, expectedMembershipDigest, expectedContentDigest string, maximum int) bool {
	if !recallResponseExactFields(partition, []string{
		"original_count", "original_membership_digest", "original_content_digest",
		"retained_count", "retained_membership_digest", "retained_content_digest",
		"excluded_count", "excluded_membership_digest", "excluded_content_digest", "entries",
	}) {
		return false
	}
	originalCount := anyToInt(partition["original_count"], -1)
	retainedCount := anyToInt(partition["retained_count"], -1)
	excludedCount := anyToInt(partition["excluded_count"], -1)
	if originalCount != expectedCount || originalCount < 0 || originalCount > maximum ||
		retainedCount < 0 || excludedCount < 0 || retainedCount+excludedCount != originalCount ||
		retainedCount != len(carrier) ||
		anyToString(partition["original_membership_digest"]) != expectedMembershipDigest ||
		anyToString(partition["original_content_digest"]) != expectedContentDigest {
		return false
	}
	for _, key := range []string{
		"original_membership_digest", "original_content_digest", "retained_membership_digest",
		"retained_content_digest", "excluded_membership_digest", "excluded_content_digest",
	} {
		if !recallResponseValidDigest(anyToString(partition[key])) {
			return false
		}
	}
	entries := contextPackAnyList(partition["entries"])
	if len(entries) != originalCount {
		return false
	}
	originalRefs := make([]string, 0, originalCount)
	originalDigests := make([]string, 0, originalCount)
	retainedRefs := []string{}
	retainedDigests := []string{}
	excludedRefs := []string{}
	excludedDigests := []string{}
	seen := map[string]bool{}
	retainedIndex := 0
	for index, raw := range entries {
		entry := anyMap(raw)
		if !recallResponseExactFields(entry, []string{"ordinal", "item_ref", "disposition", "original_row_digest", "retained_row_digest"}) ||
			anyToInt(entry["ordinal"], -1) != index {
			return false
		}
		itemRef := strings.TrimSpace(anyToString(entry["item_ref"]))
		originalDigest := anyToString(entry["original_row_digest"])
		retainedDigest := anyToString(entry["retained_row_digest"])
		if itemRef == "" || seen[itemRef] || !recallResponseValidDigest(originalDigest) {
			return false
		}
		seen[itemRef] = true
		originalRefs = append(originalRefs, itemRef)
		originalDigests = append(originalDigests, originalDigest)
		switch anyToString(entry["disposition"]) {
		case "retained":
			if !recallResponseValidDigest(retainedDigest) || retainedIndex >= len(carrier) ||
				recallResponseRawUnionIdentity(carrier[retainedIndex], "evidence", retainedIndex) != itemRef ||
				recallResponseSourceRowDigest(carrier[retainedIndex]) != retainedDigest {
				return false
			}
			retainedRefs = append(retainedRefs, itemRef)
			retainedDigests = append(retainedDigests, retainedDigest)
			retainedIndex++
		case "excluded":
			if retainedDigest != "" {
				return false
			}
			excludedRefs = append(excludedRefs, itemRef)
			excludedDigests = append(excludedDigests, originalDigest)
		default:
			return false
		}
	}
	return retainedIndex == len(carrier) &&
		recallResponseSourceIdentityDigestFromRefs(originalRefs) == anyToString(partition["original_membership_digest"]) &&
		recallResponseSourceContentDigestFromDigests(originalDigests) == anyToString(partition["original_content_digest"]) &&
		recallResponseSourceIdentityDigestFromRefs(retainedRefs) == anyToString(partition["retained_membership_digest"]) &&
		recallResponseSourceContentDigestFromDigests(retainedDigests) == anyToString(partition["retained_content_digest"]) &&
		recallResponseSourceIdentityDigestFromRefs(excludedRefs) == anyToString(partition["excluded_membership_digest"]) &&
		recallResponseSourceContentDigestFromDigests(excludedDigests) == anyToString(partition["excluded_content_digest"])
}

func recallResponseTemporalPartitionReceiptValid(input, snapshot map[string]any, sourceRows, graphRows []any) bool {
	receipt := anyMap(input[recallResponseTemporalPartitionKey])
	if !recallResponseExactFields(receipt, []string{"schema_id", "bounded", "as_of", "as_of_valid", "source", "graph", "receipt_digest"}) ||
		anyToString(receipt["schema_id"]) != recallResponseTemporalPartitionSchema || !anyToBool(receipt["bounded"]) ||
		!anyToBool(receipt["as_of_valid"]) || anyToString(receipt["receipt_digest"]) != recallResponseTemporalPartitionReceiptDigest(receipt) {
		return false
	}
	asOf, valid := recallResponseNormalizeAsOfWithValidity(firstNonEmptyStrings(anyToString(input["as_of"]), anyToString(input["asOf"])))
	if !valid || anyToString(receipt["as_of"]) != asOf {
		return false
	}
	if !recallResponseTemporalPartitionClassValid(
		sourceRows, anyMap(receipt["source"]), anyToInt(snapshot["source_captured_count"], -1),
		anyToString(snapshot["captured_membership_digest"]), anyToString(snapshot["captured_content_digest"]), recallResponseMaxSourceInputCapture,
	) || !recallResponseTemporalPartitionClassValid(
		graphRows, anyMap(receipt["graph"]), anyToInt(snapshot["graph_captured_count"], -1),
		anyToString(snapshot["graph_membership_digest"]), anyToString(snapshot["graph_content_digest"]), recallResponseMaxSourceCapture,
	) {
		return false
	}
	if asOf == recallResponseLatestAsOf {
		return anyToInt(anyMap(receipt["source"])["excluded_count"], -1) == 0 &&
			anyToInt(anyMap(receipt["graph"])["excluded_count"], -1) == 0
	}
	return true
}

func recallResponseSourceSnapshotDigest(snapshot map[string]any) string {
	material := cloneJSONMap(snapshot)
	delete(material, "coverage_digest")
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseSourceSnapshotValidForInput(input, snapshot map[string]any) bool {
	if snapshot == nil || anyToString(snapshot["schema_id"]) != recallResponseSourceSnapshotSchema || !anyToBool(snapshot["bounded"]) {
		return false
	}
	for _, key := range []string{"source_complete", "graph_complete", "complete"} {
		if _, ok := snapshot[key].(bool); !ok {
			return false
		}
	}
	for _, key := range []string{"source_membership_digest", "captured_membership_digest", "captured_content_digest", "graph_membership_digest", "graph_content_digest", "coverage_digest"} {
		if !recallResponseValidDigest(anyToString(snapshot[key])) {
			return false
		}
	}
	if anyToString(snapshot["coverage_digest"]) != recallResponseSourceSnapshotDigest(snapshot) {
		return false
	}
	sourceCandidateCount := anyToInt(snapshot["source_candidate_count"], -1)
	sourceCapturedCount := anyToInt(snapshot["source_captured_count"], -1)
	sourceOmittedCount := anyToInt(snapshot["source_omitted_count"], -1)
	graphCandidateCount := anyToInt(snapshot["graph_candidate_count"], -1)
	graphCapturedCount := anyToInt(snapshot["graph_captured_count"], -1)
	graphOmittedCount := anyToInt(snapshot["graph_omitted_count"], -1)
	if sourceCandidateCount < 0 || sourceCapturedCount < 0 || sourceOmittedCount < 0 ||
		graphCandidateCount < 0 || graphCapturedCount < 0 || graphOmittedCount < 0 ||
		sourceCapturedCount > sourceCandidateCount ||
		graphCapturedCount > graphCandidateCount ||
		sourceCapturedCount > recallResponseMaxSourceInputCapture || graphCapturedCount > recallResponseMaxSourceCapture ||
		sourceOmittedCount != maxInt(sourceCandidateCount-sourceCapturedCount, 0) ||
		graphOmittedCount != maxInt(graphCandidateCount-graphCapturedCount, 0) {
		return false
	}
	complete := anyToBool(snapshot["source_complete"]) && anyToBool(snapshot["graph_complete"]) && sourceOmittedCount == 0 && graphOmittedCount == 0
	if anyToBool(snapshot["complete"]) != complete {
		return false
	}
	pack := recallResponseCanonicalContextPack(input)
	rawCarrier, present := pack["_recall_response_source_rows"]
	if !present {
		return false
	}
	carrier := contextPackAnyList(rawCarrier)
	if len(carrier) > sourceCapturedCount {
		return false
	}
	graphRows := contextPackAnyList(pack[recallResponseGraphRowsKey])
	if graphCapturedCount == 0 && len(graphRows) == 0 {
		// A zero-row graph snapshot predates the private graph carrier. It is
		// safe to accept the empty alias, but non-empty graph custody must be
		// private and server-owned.
		graphRows = contextPackAnyList(pack["graph_neighbors"])
		if len(graphRows) == 0 {
			graphRows = contextPackAnyList(pack["graphNeighbors"])
		}
	}
	rawAsOf := firstNonEmptyStrings(anyToString(input["as_of"]), anyToString(input["asOf"]))
	asOf, asOfValid := recallResponseNormalizeAsOfWithValidity(rawAsOf)
	if !asOfValid || !recallResponseTemporalPartitionReceiptValid(input, snapshot, carrier, graphRows) {
		return false
	}
	// The temporal receipt proves a historical retained/excluded partition. The
	// latest snapshot has no temporal exclusion, so retain the stronger direct
	// equality check against both captured membership and row content.
	if asOf == recallResponseLatestAsOf &&
		(recallResponseSourceIdentityDigest(carrier) != anyToString(snapshot["captured_membership_digest"]) ||
			recallResponseSourceContentDigestFromRows(carrier) != anyToString(snapshot["captured_content_digest"]) ||
			recallResponseSourceIdentityDigest(graphRows) != anyToString(snapshot["graph_membership_digest"]) ||
			recallResponseSourceContentDigestFromRows(graphRows) != anyToString(snapshot["graph_content_digest"])) {
		return false
	}
	return true
}

func recallResponseGraphCoverage(graphRows []any, graphQuality map[string]any) (int, int, int, bool) {
	if len(graphQuality) == 0 {
		return len(graphRows), len(graphRows), 0, true
	}
	signals := anyMap(graphQuality["signals"])
	candidates := anyToInt(signals["candidate_count"], len(graphRows))
	// The server cannot have captured more graph rows than it observed as
	// candidates. A malformed or stale quality signal is repaired downward only
	// by preserving the stronger observed fact: captured rows are candidates.
	if candidates < len(graphRows) {
		candidates = len(graphRows)
	}
	failures := maxInt(anyToInt(signals["hydration_failure_count"], 0), 0)
	status := strings.ToLower(strings.TrimSpace(anyToString(graphQuality["status"])))
	complete := true
	if status == "sampled" {
		complete = candidates <= len(graphRows) && failures == 0
	}
	// Hydration failures make the graph snapshot incomplete, but they are not
	// additional membership rows. Keep the omission equation exact:
	// omitted = candidate - captured.
	omitted := maxInt(candidates-len(graphRows), 0)
	return candidates, len(graphRows), omitted, complete
}

func recallResponseBuildSourceSnapshot(allRows, capturedRows, graphRows []any, sourceComplete bool, graphQuality map[string]any) map[string]any {
	graphCandidates, graphCaptured, graphOmitted, graphComplete := recallResponseGraphCoverage(graphRows, graphQuality)
	snapshot := map[string]any{
		"schema_id":                  recallResponseSourceSnapshotSchema,
		"bounded":                    true,
		"source_candidate_count":     len(allRows),
		"source_captured_count":      len(capturedRows),
		"source_omitted_count":       maxInt(len(allRows)-len(capturedRows), 0),
		"source_membership_digest":   recallResponseSourceIdentityDigest(allRows),
		"captured_membership_digest": recallResponseSourceIdentityDigest(capturedRows),
		"captured_content_digest":    recallResponseSourceContentDigestFromRows(capturedRows),
		"graph_candidate_count":      graphCandidates,
		"graph_captured_count":       graphCaptured,
		"graph_omitted_count":        graphOmitted,
		"graph_membership_digest":    recallResponseSourceIdentityDigest(graphRows),
		"graph_content_digest":       recallResponseSourceContentDigestFromRows(graphRows),
		"source_complete":            sourceComplete && len(allRows) == len(capturedRows),
		"graph_complete":             graphComplete,
		"complete":                   sourceComplete && len(allRows) == len(capturedRows) && graphComplete,
	}
	snapshot["coverage_digest"] = recallResponseSourceSnapshotDigest(snapshot)
	return snapshot
}

func recallResponseRefreshSourceCarrier(contextPack map[string]any, graphQuality map[string]any) {
	if contextPack == nil {
		return
	}
	carrier := contextPackAnyList(contextPack["_recall_response_source_rows"])
	graphRows := contextPackAnyList(contextPack[recallResponseGraphRowsKey])
	if len(graphRows) == 0 {
		graphRows = contextPackAnyList(contextPack["graph_neighbors"])
		if len(graphRows) == 0 {
			graphRows = contextPackAnyList(contextPack["graphNeighbors"])
		}
	}
	// Retrieval-source and graph memberships are separate custody classes.
	// Graph rows may be merged into the response's evidence projection later,
	// but never into the retrieval-source carrier or its source counts.
	if len(carrier) > recallResponseMaxSourceInputCapture {
		carrier = carrier[:recallResponseMaxSourceInputCapture]
	}
	if len(graphRows) > recallResponseMaxSourceCapture {
		graphRows = graphRows[:recallResponseMaxSourceCapture]
	}
	contextPack["_recall_response_source_rows"] = cloneJSONValue(carrier)
	contextPack[recallResponseGraphRowsKey] = cloneJSONValue(graphRows)
	snapshot := anyMap(contextPack["_recall_response_source_snapshot"])
	if len(snapshot) == 0 {
		snapshot = recallResponseBuildSourceSnapshot(carrier, carrier, graphRows, true, graphQuality)
	}
	sourceCandidateCount := anyToInt(snapshot["source_candidate_count"], len(carrier))
	snapshot["source_candidate_count"] = sourceCandidateCount
	if strings.TrimSpace(anyToString(snapshot["source_membership_digest"])) == "" {
		snapshot["source_membership_digest"] = recallResponseSourceIdentityDigest(carrier)
	}
	snapshot["captured_membership_digest"] = recallResponseSourceIdentityDigest(carrier)
	snapshot["captured_content_digest"] = recallResponseSourceContentDigestFromRows(carrier)
	snapshot["source_captured_count"] = len(carrier)
	snapshot["source_omitted_count"] = maxInt(sourceCandidateCount-len(carrier), 0)
	graphCandidates, graphCaptured, graphOmitted, graphComplete := recallResponseGraphCoverage(graphRows, graphQuality)
	snapshot["graph_candidate_count"] = graphCandidates
	snapshot["graph_captured_count"] = graphCaptured
	snapshot["graph_omitted_count"] = graphOmitted
	snapshot["graph_membership_digest"] = recallResponseSourceIdentityDigest(graphRows)
	snapshot["graph_content_digest"] = recallResponseSourceContentDigestFromRows(graphRows)
	previousGraphComplete, hadGraphComplete := snapshot["graph_complete"].(bool)
	if hadGraphComplete {
		graphComplete = graphComplete && previousGraphComplete
	}
	snapshot["graph_complete"] = graphComplete
	sourceComplete := anyToBool(snapshot["source_complete"]) && sourceCandidateCount == len(carrier)
	snapshot["source_complete"] = sourceComplete
	snapshot["complete"] = sourceComplete && graphComplete
	snapshot["coverage_digest"] = recallResponseSourceSnapshotDigest(snapshot)
	contextPack["_recall_response_source_snapshot"] = snapshot
}

// recallResponseCanonicalContextPack selects one wire alias. A response must
// never count context_pack and contextPack (or their ranked-evidence aliases)
// as two independent source snapshots.
func recallResponseCanonicalContextPack(input map[string]any) map[string]any {
	if pack := anyMap(input["context_pack"]); len(pack) > 0 {
		return pack
	}
	return anyMap(input["contextPack"])
}

func recallResponseSourceSnapshotForInput(input map[string]any) map[string]any {
	if snapshot := anyMap(input["_recall_response_source_snapshot"]); len(snapshot) > 0 {
		return snapshot
	}
	return anyMap(recallResponseCanonicalContextPack(input)["_recall_response_source_snapshot"])
}

func recallResponseFirstRows(value map[string]any, keys ...string) []any {
	for _, key := range keys {
		if raw, present := value[key]; present {
			rows := contextPackAnyList(raw)
			if len(rows) > 0 || key == keys[len(keys)-1] {
				return rows
			}
		}
	}
	return nil
}

// recallResponseCanonicalSourceRows performs alias selection and deterministic
// within-class de-duplication before any bounded accounting. The rows are
// still policy-filtered by the projection; this helper only establishes the
// one source snapshot and the class order.
func recallResponseCanonicalSourceRows(input map[string]any) map[string][]any {
	pack := recallResponseCanonicalContextPack(input)
	classRows := map[string][]any{}
	classKeys := []struct {
		name     string
		packKeys []string
		inputKey string
	}{
		{name: "evidence", packKeys: []string{"ranked_evidence", "rankedEvidence"}, inputKey: "evidence"},
		{name: "temporal", packKeys: []string{"temporal_claims"}, inputKey: "temporal_claims"},
		{name: "proof", packKeys: []string{"proof_claims"}, inputKey: "proof_claims"},
		{name: "conflicts", packKeys: []string{"conflicts", "contradictions"}, inputKey: "conflicts"},
	}
	for _, class := range classKeys {
		rows := []any{}
		if class.name == "evidence" {
			// The compiler's ranked_evidence is a bounded presentation. A
			// server-owned source carrier, when present, is the complete retrieval
			// snapshot used for policy and continuation membership.
			rows = append(rows, contextPackAnyList(pack["_recall_response_source_rows"])...)
			// Graph evidence has its own custody accounting. It is combined only
			// at this policy projection boundary, never by mutating source counts.
			graphRows := contextPackAnyList(pack[recallResponseGraphRowsKey])
			if _, present := pack[recallResponseGraphRowsKey]; !present {
				graphRows = contextPackAnyList(pack["graph_neighbors"])
				if len(graphRows) == 0 {
					graphRows = contextPackAnyList(pack["graphNeighbors"])
				}
			}
			rows = append(rows, graphRows...)
		}
		if len(rows) == 0 {
			rows = recallResponseFirstRows(pack, class.packKeys...)
		}
		if len(rows) == 0 {
			rows = contextPackAnyList(input[class.inputKey])
		}
		seen := map[string]bool{}
		canonical := make([]any, 0, len(rows))
		for index, raw := range rows {
			identity := recallResponseRawUnionIdentity(raw, class.name, index)
			if seen[identity] {
				continue
			}
			seen[identity] = true
			item := cloneJSONMap(anyMap(raw))
			item["_recall_response_item_type"] = class.name
			if strings.TrimSpace(anyToString(item["kind"])) == "" {
				switch class.name {
				case "temporal":
					item["kind"] = "temporal_claim"
				case "proof":
					item["kind"] = "proof_claim"
				case "conflicts":
					item["kind"] = "contradiction"
				default:
					item["kind"] = "evidence"
				}
			}
			canonical = append(canonical, item)
		}
		classRows[class.name] = canonical
	}
	return classRows
}

func recallResponseRelevantEvidenceRows(input map[string]any) []any {
	classes := recallResponseCanonicalSourceRows(input)
	rows := make([]any, 0, len(classes["evidence"])+len(classes["temporal"])+len(classes["proof"]))
	for _, class := range []string{"evidence", "temporal", "proof"} {
		rows = append(rows, classes[class]...)
	}
	return rows
}

type recallResponseDigestAccumulator struct {
	hash  [32]byte
	value []byte
}

func (a *recallResponseDigestAccumulator) add(value string) {
	// Keep the accumulator deterministic without retaining an unbounded source
	// list. Each item is length-delimited by a NUL separator before hashing.
	a.value = append(a.value, 0)
	a.value = append(a.value, []byte(value)...)
	// Rehashing the bounded accumulator state avoids retaining source rows in
	// the continuation receipt. The source row itself remains outside output.
	a.hash = sha256.Sum256(a.value)
	a.value = a.hash[:]
}

func (a *recallResponseDigestAccumulator) digest() string {
	return "sha256:" + hex.EncodeToString(a.hash[:])
}

func recallResponseRawUnionIdentity(raw any, class string, index int) string {
	item := anyMap(raw)
	for _, key := range []string{"candidate_id", "ref_id", "memory_id", "record_ref", "claim_id", "conflict_id"} {
		value := strings.TrimSpace(anyToString(item[key]))
		if value == "" {
			continue
		}
		if safe := recallResponseSafeOpaqueID(value); safe != "" {
			return safe
		}
		return recallResponseScopedOpaqueRef("sha256:"+sha256Hex(class), class, key+"\x00"+value)
	}
	if digest := strings.TrimSpace(anyToString(item["content_digest"])); recallResponseValidDigest(digest) {
		return recallResponseScopedOpaqueRef("sha256:"+sha256Hex(class), class, "content_digest\x00"+digest)
	}
	// Distinct rows from one source must not collapse merely because they share
	// a backend name. The canonical row hash stays opaque and also de-duplicates
	// exact alias duplicates without retaining raw content in the output.
	_ = index
	return recallResponseScopedOpaqueRef("sha256:"+sha256Hex(class), class, recallResponseCanonicalJSON(item))
}

// recallResponseCanonicalSourceRef is the server-owned identity for one source
// row. Candidate IDs remain the exact source identity. Rows without a valid
// candidate ID use a class-bound canonical row identity, never a presentation
// index; that keeps a row's identity stable when an earlier future/stale row
// is removed by the as_of premise.
func recallResponseCanonicalSourceRef(item map[string]any, class string) string {
	if item == nil {
		return ""
	}
	for _, key := range []string{"candidate_id", "ref_id", "memory_id", "record_ref", "claim_id", "conflict_id"} {
		value := strings.TrimSpace(anyToString(item[key]))
		if value == "" {
			continue
		}
		if safe := recallResponseSafeOpaqueID(value); safe != "" {
			return safe
		}
	}
	identity := recallResponseRawUnionIdentity(item, class, 0)
	if identity == "" {
		return ""
	}
	return recallResponseScopedOpaqueRef(
		"sha256:"+sha256Hex("recall-response-source-identity"),
		"source",
		class+"\x00"+identity,
	)
}

func recallResponseEvidenceBindingKey(item map[string]any) string {
	candidateID := recallResponseSafeOpaqueID(anyToString(item["candidate_id"]))
	source := firstNonEmptyStrings(anyToString(item["source_ref"]), anyToString(item["source"]))
	digest := strings.TrimSpace(anyToString(item["content_digest"]))
	if candidateID == "" || source == "" || !recallResponseValidDigest(digest) {
		return ""
	}
	return "sha256:" + sha256Hex(candidateID+"\x00"+source+"\x00"+digest)
}

// recallResponseValidatedEvidenceBindings creates a typed, internal-only
// binding set. Public JSON cannot populate this map. allowedCandidateRefs is
// nil for a validated frozen policy snapshot; production callers pass the
// candidate identities admitted by a server-owned receipt.
func recallResponseValidatedEvidenceBindings(input map[string]any, authority string, allowedCandidateRefs map[string]bool) map[string]recallResponseValidatedEvidenceBinding {
	if authority != "server_receipt" && authority != "validated_policy" {
		return nil
	}
	out := map[string]recallResponseValidatedEvidenceBinding{}
	classes := recallResponseCanonicalSourceRows(input)
	for _, class := range []string{"evidence", "temporal", "proof"} {
		for _, raw := range classes[class] {
			item := anyMap(raw)
			candidateID := recallResponseSafeOpaqueID(anyToString(item["candidate_id"]))
			if allowedCandidateRefs != nil && !allowedCandidateRefs[candidateID] {
				continue
			}
			if key := recallResponseEvidenceBindingKey(item); key != "" {
				out[key] = recallResponseValidatedEvidenceBinding{authority: authority}
			}
		}
	}
	return out
}

func recallResponseEvidenceBindingAuthority(item map[string]any, policy validatedRecallResponsePolicyInput) (string, bool) {
	if !policy.sourceBound || !recallResponseValidDigest(policy.snapshotDigest) || !recallResponseValidDigest(policy.receiptDigest) {
		return "derived", false
	}
	key := recallResponseEvidenceBindingKey(item)
	binding, ok := policy.evidenceBindings[key]
	if key == "" || !ok || (binding.authority != "server_receipt" && binding.authority != "validated_policy") {
		return "derived", false
	}
	return binding.authority, true
}

func recallResponseCaptureSourceUnion(input map[string]any) map[string]any {
	union := map[string]any{
		"evidence":  []any{},
		"temporal":  []any{},
		"proof":     []any{},
		"conflicts": []any{},
	}
	counts := map[string]any{"evidence": 0, "temporal": 0, "proof": 0, "conflicts": 0}
	truncated := map[string]any{"evidence": false, "temporal": false, "proof": false, "conflicts": false}
	truncatedRefs := map[string]any{"evidence": []any{}, "temporal": []any{}, "proof": []any{}, "conflicts": []any{}}
	digests := map[string]*recallResponseDigestAccumulator{
		"evidence":  &recallResponseDigestAccumulator{},
		"temporal":  &recallResponseDigestAccumulator{},
		"proof":     &recallResponseDigestAccumulator{},
		"conflicts": &recallResponseDigestAccumulator{},
	}
	appendRows := func(key string, rows []any) {
		current := contextPackAnyList(union[key])
		for index, row := range rows {
			identity := recallResponseRawUnionIdentity(row, key, index)
			counts[key] = anyToInt(counts[key], 0) + 1
			digests[key].add(identity)
			if len(current) >= recallResponseMaxSourceCapture {
				truncated[key] = true
				refs := contextPackAnyList(truncatedRefs[key])
				if len(refs) < recallResponseMaxOmissionLedger {
					truncatedRefs[key] = append(refs, identity)
				}
				continue
			}
			current = append(current, cloneJSONValue(row))
		}
		union[key] = current
	}
	classes := recallResponseCanonicalSourceRows(input)
	for _, class := range []string{"evidence", "temporal", "proof", "conflicts"} {
		appendRows(class, classes[class])
	}
	digestMaterial := map[string]any{}
	for _, key := range []string{"evidence", "temporal", "proof", "conflicts"} {
		digestMaterial[key] = map[string]any{"count": counts[key], "identity_digest": digests[key].digest()}
	}
	snapshot := anyMap(input["_recall_response_source_snapshot"])
	if len(snapshot) == 0 {
		snapshot = anyMap(recallResponseCanonicalContextPack(input)["_recall_response_source_snapshot"])
	}
	snapshotValid := len(snapshot) == 0 || recallResponseSourceSnapshotValidForInput(input, snapshot)
	if len(snapshot) > 0 {
		// The snapshot count is the server's pre-presentation custody count. A
		// clipped carrier must remain visibly incomplete even when its retained
		// prefix happens to fit the per-class capture bound.
		if snapshotValid {
			if candidateCount := anyToInt(snapshot["source_candidate_count"], 0); candidateCount > anyToInt(counts["evidence"], 0) {
				counts["evidence"] = candidateCount
				digestMaterial["evidence"] = map[string]any{"count": candidateCount, "identity_digest": anyToString(snapshot["source_membership_digest"])}
			}
		}
		if !snapshotValid || !anyToBool(snapshot["complete"]) {
			truncated["evidence"] = true
		}
	}
	union["source_counts"] = counts
	union["source_truncated"] = anyToBool(truncated["evidence"]) || anyToBool(truncated["temporal"]) || anyToBool(truncated["proof"]) || anyToBool(truncated["conflicts"])
	union["source_identity_digests"] = digestMaterial
	union["source_truncated_refs"] = truncatedRefs
	union["source_continuation_digest"] = "sha256:" + sha256Hex(recallResponseCanonicalJSON(digestMaterial))
	if len(snapshot) > 0 {
		if snapshotValid {
			union["source_membership_digest"] = firstNonEmptyStrings(
				anyToString(snapshot["captured_membership_digest"]), anyToString(snapshot["source_membership_digest"]),
			)
			union["source_coverage_digest"] = anyToString(snapshot["coverage_digest"])
			union["source_snapshot_complete"] = anyToBool(snapshot["complete"])
		} else {
			union["source_membership_digest"] = recallResponseSourceIdentityDigest(recallResponseRelevantEvidenceRows(input))
			union["source_coverage_digest"] = "sha256:" + sha256Hex("recall-response-source-coverage-invalid\x00"+anyToString(union["source_membership_digest"]))
			union["source_snapshot_complete"] = false
		}
	} else {
		union["source_membership_digest"] = recallResponseSourceIdentityDigest(recallResponseRelevantEvidenceRows(input))
		union["source_coverage_digest"] = "sha256:" + sha256Hex("recall-response-source-coverage\x00"+anyToString(union["source_membership_digest"]))
		union["source_snapshot_complete"] = true
	}
	return union
}

func recallResponseTemporalExclusionRefs(input map[string]any, asOf string, asOfValid bool) []any {
	refs := []string{}
	add := func(raw any, index int) {
		item := anyMap(raw)
		ref := recallResponseCanonicalSourceRef(item, "temporal")
		if !containsString(refs, ref) && len(refs) < recallResponseMaxUnionRefs {
			refs = append(refs, ref)
		}
		_ = index
	}
	visit := func(rows []any) {
		if !asOfValid {
			for index, raw := range rows {
				add(raw, index)
			}
			return
		}
		filtered, _ := recallResponseTemporalEvidenceAtOrBefore(rows, asOf)
		retained := map[string]bool{}
		for _, raw := range filtered {
			identity := recallResponseCanonicalSourceRef(anyMap(raw), "temporal")
			retained[identity] = true
		}
		for index, raw := range rows {
			identity := recallResponseCanonicalSourceRef(anyMap(raw), "temporal")
			if !retained[identity] {
				add(raw, index)
			}
		}
	}
	classes := recallResponseCanonicalSourceRows(input)
	for _, class := range []string{"evidence", "temporal", "proof"} {
		visit(classes[class])
	}
	return recallResponseAnyStrings(refs)
}

func recallResponseSourceEvidence(input map[string]any, fallback []any) []any {
	// Source accounting is intentionally separate from the authoritative
	// projection. Returning the bounded private capture here could drop an
	// eligible row merely because earlier rejected rows consumed the capture
	// budget. The caller's prepared rows have already crossed temporal policy;
	// recallResponseEvidenceProjection applies owner/trust/forgetting eligibility
	// before any support row is emitted.
	_ = input
	return fallback
}

func recallResponseEvidenceProjection(item map[string]any, index int, scopeDigest string, policy validatedRecallResponsePolicyInput) (string, string, bool, bool, float64, string, string, bool, string, bool) {
	kind := recallResponseSafeKind(anyToString(item["kind"]))
	class := firstNonEmptyStrings(anyToString(item["_recall_response_item_type"]), "evidence")
	refID := recallResponseCanonicalSourceRef(item, class)
	if refID == "" {
		// Keep malformed/empty rows opaque and deterministic. This branch is
		// intentionally independent of the bounded presentation index.
		refID = recallResponseScopedOpaqueRef(scopeDigest, "evidence", recallResponseCanonicalJSON(item))
	}
	_ = index
	identity := firstNonEmptyStrings(
		anyToString(item["candidate_id"]), anyToString(item["ref_id"]),
		anyToString(item["memory_id"]), anyToString(item["content_digest"]),
		anyToString(item["citation"]), anyToString(item["source"]),
		anyToString(item["file"]), anyToString(item["text"]),
		refID,
	)
	status, eligible := recallResponseEvidenceStatus(item)
	confidence, confidenceValid := recallResponseEvidenceConfidence(item["confidence"])
	originalDigest := strings.TrimSpace(anyToString(item["content_digest"]))
	digest := originalDigest
	bindingAuthority, sourceBound := recallResponseEvidenceBindingAuthority(item, policy)
	if !recallResponseValidDigest(digest) {
		digest = "sha256:" + sha256Hex(firstNonEmptyStrings(anyToString(item["text"]), identity))
	}
	role := "support"
	if strings.EqualFold(strings.TrimSpace(anyToString(item["support"])), "context") {
		role = "context"
	}
	protected := recallResponseEvidenceProtected(item, status, eligible, confidence, confidenceValid, sourceBound)
	return refID, kind, eligible, confidenceValid, confidence, digest, role, protected, bindingAuthority, sourceBound
}

func recallResponseEvidenceProtected(item map[string]any, status string, eligible bool, confidence float64, confidenceValid bool, sourceBound bool) bool {
	if !eligible || !confidenceValid {
		return false
	}
	status = strings.ToLower(strings.TrimSpace(status))
	switch status {
	case "quarantined", "omitted", "stale", "superseded", "retracted", "revoked", "retired", "expired", "unknown", "invalid_confidence":
		return false
	}
	if strings.EqualFold(strings.TrimSpace(anyToString(item["support"])), "distractor") ||
		strings.EqualFold(strings.TrimSpace(anyToString(item["support"])), "non_support") ||
		strings.EqualFold(strings.TrimSpace(anyToString(item["support"])), "unsupported") {
		return false
	}
	// Confidence and formatted fields alone cannot make a row unsheddable. The
	// exact source/content tuple must be present in a typed validated policy
	// binding created outside caller-controlled JSON.
	if !sourceBound {
		return false
	}
	if len(anyMap(item["action"])) > 0 || len(anyMap(item["action_boundary"])) > 0 {
		return true
	}
	if anyToBool(item["required_for_action"]) || anyToBool(item["required_for_proof"]) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(anyToString(item["proof_status"])), "contested") ||
		strings.EqualFold(strings.TrimSpace(anyToString(item["proof_status"])), "required") {
		return true
	}
	return false
}

func recallResponseDisclosure(response map[string]any) map[string]any {
	disclosure := anyMap(response["disclosure"])
	if len(disclosure) == 0 {
		disclosure = map[string]any{}
		response["disclosure"] = disclosure
	}
	return disclosure
}

func recallResponseEvidenceUnionRows(input map[string]any, rankedEvidence []any, scopeDigest string, policy validatedRecallResponsePolicyInput) []any {
	rankedEvidence = recallResponseSourceEvidence(input, rankedEvidence)
	protectedRows := []any{}
	ordinaryRows := []any{}
	seen := map[string]bool{}
	for index, raw := range rankedEvidence {
		item := anyMap(raw)
		refID, kind, eligible, confidenceValid, confidence, contentDigest, role, protected, bindingAuthority, sourceBound := recallResponseEvidenceProjection(item, index, scopeDigest, policy)
		if !eligible || !confidenceValid {
			continue
		}
		itemType := recallResponseSafeContinuationItemType(anyToString(item["_recall_response_item_type"]), "evidence")
		typedKey := recallResponseTypedItemKey(itemType, refID)
		if seen[typedKey] {
			continue
		}
		seen[typedKey] = true
		row := map[string]any{
			"ref_id":     refID,
			"item_type":  itemType,
			"kind":       kind,
			"role":       role,
			"status":     recallResponseSafeStatus(anyToString(firstNonEmptyStrings(anyToString(item["status"]), anyToString(item["proof_status"]), anyToString(item["freshness"]), "selected"))),
			"confidence": roundFloat(confidence, 4),
			"protected":  protected,
			"evidence_binding": map[string]any{
				"source_ref":        recallResponseScopedOpaqueRef(scopeDigest, "source", firstNonEmptyStrings(anyToString(item["source_ref"]), anyToString(item["source"]))),
				"content_digest":    contentDigest,
				"binding_authority": bindingAuthority,
				"source_bound":      sourceBound,
			},
		}
		if protected {
			protectedRows = append(protectedRows, row)
		} else {
			ordinaryRows = append(ordinaryRows, row)
		}
	}
	rows := append(protectedRows, ordinaryRows...)
	if len(rows) > recallResponseMaxPresentedEvidenceRefs {
		rows = rows[:recallResponseMaxPresentedEvidenceRefs]
	}
	return rows
}

type recallResponseExclusionAccounting struct {
	refs              []any
	allRefs           []string
	clippedRefs       []string
	truncated         bool
	continuation      string
	eligibleCount     int
	eligibleRefs      []string
	eligibleProtected map[string]bool
	excludedCount     int
}

func recallResponseExclusionRefsWithAccounting(input map[string]any, rankedEvidence []any, scopeDigest string, policy validatedRecallResponsePolicyInput) recallResponseExclusionAccounting {
	rankedEvidence = recallResponseSourceEvidence(input, rankedEvidence)
	visibleRefs := []string{}
	allEligible := []string{}
	allExcluded := []string{}
	allTemporal := []string{}
	eligibleProtected := map[string]bool{}
	seen := map[string]bool{}
	for index, raw := range rankedEvidence {
		item := anyMap(raw)
		refID, _, eligible, confidenceValid, _, _, _, protected, _, _ := recallResponseEvidenceProjection(item, index, scopeDigest, policy)
		if eligible && confidenceValid {
			if !containsString(allEligible, refID) && len(allEligible) < recallResponseMaxSourceCapture {
				allEligible = append(allEligible, refID)
				eligibleProtected[refID] = protected
			}
			continue
		}
		if !containsString(allExcluded, refID) && len(allExcluded) < recallResponseMaxSourceCapture {
			allExcluded = append(allExcluded, refID)
		}
	}
	for _, raw := range contextPackAnyList(input["_recall_response_temporal_exclusion_refs"]) {
		refID := strings.TrimSpace(anyToString(raw))
		if refID != "" && !containsString(allTemporal, refID) && len(allTemporal) < recallResponseMaxSourceCapture {
			allTemporal = append(allTemporal, refID)
		}
	}
	ordered := append([]string{}, allEligible...)
	ordered = append(ordered, allExcluded...)
	ordered = append(ordered, allTemporal...)
	for _, refID := range ordered {
		if !seen[refID] && len(visibleRefs) < recallResponseMaxUnionRefs {
			seen[refID] = true
			if containsString(allExcluded, refID) || containsString(allTemporal, refID) {
				visibleRefs = append(visibleRefs, refID)
			}
		}
	}
	sourceUnion := anyMap(input["_recall_response_source_union"])
	sourceTruncated := anyToBool(sourceUnion["source_truncated"])
	sourceCounts := anyMap(sourceUnion["source_counts"])
	knownSourceCount := anyToInt(sourceCounts["evidence"], len(rankedEvidence))
	if knownSourceCount > len(rankedEvidence) {
		sourceTruncated = true
	}
	truncated := sourceTruncated || len(ordered) > recallResponseMaxUnionRefs
	clipped := []string{}
	for _, refID := range allExcluded {
		if !containsString(visibleRefs, refID) && len(clipped) < recallResponseMaxOmissionLedger {
			clipped = append(clipped, refID)
		}
	}
	for _, refID := range allTemporal {
		if !containsString(visibleRefs, refID) && !containsString(clipped, refID) && len(clipped) < recallResponseMaxOmissionLedger {
			clipped = append(clipped, refID)
		}
	}
	continuationMaterial := map[string]any{
		"ordered_refs":               ordered,
		"source_continuation_digest": anyToString(sourceUnion["source_continuation_digest"]),
		"source_counts":              sourceCounts,
	}
	return recallResponseExclusionAccounting{
		refs:              recallResponseAnyStrings(visibleRefs),
		allRefs:           ordered,
		clippedRefs:       clipped,
		truncated:         truncated,
		continuation:      "sha256:" + sha256Hex(recallResponseCanonicalJSON(continuationMaterial)),
		eligibleCount:     len(allEligible),
		eligibleRefs:      allEligible,
		eligibleProtected: eligibleProtected,
		excludedCount:     len(allExcluded) + len(allTemporal),
	}
}

func recallResponseExclusionRefs(input map[string]any, rankedEvidence []any, scopeDigest string) ([]any, bool, string) {
	accounting := recallResponseExclusionRefsWithAccounting(input, rankedEvidence, scopeDigest, recallResponseProductionPolicyInput())
	return accounting.refs, accounting.truncated, accounting.continuation
}

type recallResponseComponentAccounting struct {
	rows         []any
	allRefs      []string
	clippedRefs  []string
	protected    map[string]bool
	truncated    bool
	continuation string
}

func recallResponseComponentUnionRowsWithAccounting(components []any, limits ...int) recallResponseComponentAccounting {
	limit := recallResponseMaxModules
	if len(limits) > 0 && limits[0] > 0 && limits[0] < limit {
		limit = limits[0]
	}
	type componentCandidate struct {
		refID     string
		protected bool
		row       map[string]any
	}
	protectedRows := []componentCandidate{}
	ordinaryRows := []componentCandidate{}
	seen := map[string]bool{}
	allRefs := []string{}
	clippedRefs := []string{}
	protected := map[string]bool{}
	proofTruncated := false
	for index, raw := range components {
		component := anyMap(raw)
		refID := strings.TrimSpace(anyToString(component["component_ref"]))
		kind := strings.TrimSpace(anyToString(component["kind"]))
		if refID == "" || kind == "" || seen[refID] {
			continue
		}
		allRefs = append(allRefs, refID)
		componentProtected := index == 0 || recallResponseModuleSafety[kind]
		seen[refID] = true
		if len(contextPackAnyList(component["proof_refs"])) > recallResponseMaxProofRefs {
			proofTruncated = true
		}
		candidate := componentCandidate{refID: refID, protected: componentProtected, row: map[string]any{
			"component_ref": refID,
			"kind":          kind,
			"protected":     componentProtected,
		}}
		protected[refID] = componentProtected
		if componentProtected {
			protectedRows = append(protectedRows, candidate)
		} else {
			ordinaryRows = append(ordinaryRows, candidate)
		}
	}
	ordered := append(protectedRows, ordinaryRows...)
	rows := make([]any, 0, minInt(len(ordered), limit))
	for _, candidate := range ordered {
		if len(rows) < limit {
			rows = append(rows, candidate.row)
		} else if len(clippedRefs) < recallResponseMaxOmissionLedger {
			clippedRefs = append(clippedRefs, candidate.refID)
		}
	}
	truncated := len(allRefs) > limit || proofTruncated
	continuationMaterial := map[string]any{"component_refs": allRefs, "proof_truncated": proofTruncated}
	return recallResponseComponentAccounting{
		rows:         rows,
		allRefs:      allRefs,
		clippedRefs:  clippedRefs,
		protected:    protected,
		truncated:    truncated,
		continuation: "sha256:" + sha256Hex(recallResponseCanonicalJSON(continuationMaterial)),
	}
}

func recallResponseComponentUnionRows(components []any) []any {
	return recallResponseComponentUnionRowsWithAccounting(components).rows
}

type recallResponseProofAccounting struct {
	refs         []any
	allRefs      []string
	clippedRefs  []string
	truncated    bool
	continuation string
}

func recallResponseProofUnionRefsWithAccounting(response map[string]any, evidenceUnion []any, exclusionRefs []any, conflictRefs []string) recallResponseProofAccounting {
	allRefs := []string{}
	add := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref != "" && !containsString(allRefs, ref) {
			allRefs = append(allRefs, ref)
		}
	}
	for _, raw := range evidenceUnion {
		add(anyToString(anyMap(raw)["ref_id"]))
	}
	for _, ref := range conflictRefs {
		add(ref)
	}
	for _, raw := range contextPackAnyList(response["conflicts"]) {
		add(anyToString(anyMap(raw)["conflict_id"]))
	}
	scopeDigest := anyToString(anyMap(response["request_scope"])["scope_digest"])
	for _, raw := range contextPackAnyList(response["gaps"]) {
		add(recallResponseScopedOpaqueRef(scopeDigest, "gap", anyToString(anyMap(raw)["code"])))
	}
	for _, raw := range exclusionRefs {
		add(anyToString(raw))
	}
	visible := allRefs
	if len(visible) > recallResponseMaxPresentedProofRefs {
		visible = visible[:recallResponseMaxPresentedProofRefs]
	}
	clipped := []string{}
	if len(allRefs) > recallResponseMaxPresentedProofRefs {
		clipped = append(clipped, allRefs[recallResponseMaxPresentedProofRefs:minInt(len(allRefs), recallResponseMaxPresentedProofRefs+recallResponseMaxOmissionLedger)]...)
	}
	return recallResponseProofAccounting{
		refs:         recallResponseAnyStrings(visible),
		allRefs:      allRefs,
		clippedRefs:  clipped,
		truncated:    len(allRefs) > recallResponseMaxPresentedProofRefs,
		continuation: "sha256:" + sha256Hex(recallResponseCanonicalJSON(allRefs)),
	}
}

func recallResponseProofUnionRefs(response map[string]any, evidenceUnion []any) []any {
	return recallResponseProofUnionRefsWithAccounting(response, evidenceUnion, nil, nil).refs
}

func recallResponseProofIdentityDigest(refs []any) string {
	values := []string{}
	for _, raw := range refs {
		ref := strings.TrimSpace(anyToString(raw))
		if ref != "" && !containsString(values, ref) {
			values = append(values, ref)
		}
	}
	sort.Strings(values)
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(recallResponseAnyStrings(values)))
}

func recallResponseControlReceiptMaterial(snapshotDigest, receiptDigest, unionDigest, proofIdentityDigest string, sourceBound bool) map[string]any {
	return map[string]any{
		"snapshot_digest":       snapshotDigest,
		"receipt_digest":        receiptDigest,
		"union_digest":          unionDigest,
		"proof_identity_digest": proofIdentityDigest,
		"source_bound":          sourceBound,
	}
}

func recallResponseEnsureControlReceipt(response map[string]any, policy validatedRecallResponsePolicyInput, asOf string) {
	scope := anyMap(response["request_scope"])
	snapshotDigest := policy.snapshotDigest
	if !recallResponseValidDigest(snapshotDigest) {
		snapshotDigest = anyToString(scope["snapshot_digest"])
	}
	if !recallResponseValidDigest(snapshotDigest) {
		snapshotDigest = "sha256:" + sha256Hex("recall-response-control-snapshot\x00"+anyToString(scope["scope_digest"]))
	}
	receiptDigest := policy.receiptDigest
	if !recallResponseValidDigest(receiptDigest) {
		receiptDigest = anyToString(scope["receipt_digest"])
	}
	if !recallResponseValidDigest(receiptDigest) {
		receiptDigest = "sha256:" + sha256Hex("recall-response-control-receipt\x00"+anyToString(scope["scope_digest"]))
	}
	scope["as_of"] = asOf
	scope["source_bound"] = policy.sourceBound
	scope["snapshot_digest"] = snapshotDigest
	scope["receipt_digest"] = receiptDigest
	temporalPremiseDigest := anyToString(scope["temporal_premise_digest"])
	if !recallResponseValidDigest(temporalPremiseDigest) {
		temporalPremiseDigest = anyToString(recallResponseTemporalPremise(asOf, anyToString(anyMap(response["classification"])["facets"]))["digest"])
		scope["temporal_premise_digest"] = temporalPremiseDigest
	}
	recallResponseBindControlReceipt(response, snapshotDigest, receiptDigest, policy.sourceBound)
}

func recallResponseBindControlReceipt(response map[string]any, snapshotDigest, receiptDigest string, sourceBound bool) {
	disclosure := recallResponseDisclosure(response)
	unionDigest := anyToString(disclosure["union_digest"])
	// The control proof identity is the authoritative proof-union identity,
	// not the candidate's presentation spine. Layout compression may shorten
	// that spine, but it cannot change the same-snapshot proof/evidence union.
	proofDigest := recallResponseProofIdentityDigest(contextPackAnyList(disclosure["proof_union"]))
	material := recallResponseControlReceiptMaterial(snapshotDigest, receiptDigest, unionDigest, proofDigest, sourceBound)
	receipt := cloneJSONMap(material)
	receipt["artifact_digest"] = "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
	disclosure["control_union_digest"] = unionDigest
	disclosure["control_proof_identity_digest"] = proofDigest
	disclosure["control_receipt"] = receipt
	for _, raw := range contextPackAnyList(disclosure["omission_ledger"]) {
		row := anyMap(raw)
		counterfactual := anyMap(row["same_snapshot_counterfactual"])
		row["same_snapshot_counterfactual"] = recallResponseOmissionCounterfactual(response, anyToString(counterfactual["outcome"]), anyToBool(row["protected"]))
	}
}

func recallResponseControlReceiptDigest(response map[string]any) string {
	disclosure := recallResponseDisclosure(response)
	scope := anyMap(response["request_scope"])
	snapshotDigest := anyToString(scope["snapshot_digest"])
	if !recallResponseValidDigest(snapshotDigest) {
		snapshotDigest = "sha256:" + sha256Hex("recall-response-control-snapshot\x00"+anyToString(scope["scope_digest"]))
	}
	receiptDigest := anyToString(scope["receipt_digest"])
	if !recallResponseValidDigest(receiptDigest) {
		receiptDigest = "sha256:" + sha256Hex("recall-response-control-receipt\x00"+anyToString(scope["scope_digest"]))
	}
	scope["snapshot_digest"] = snapshotDigest
	scope["receipt_digest"] = receiptDigest
	recallResponseBindControlReceipt(response, snapshotDigest, receiptDigest, anyToBool(scope["source_bound"]))
	return anyToString(anyMap(disclosure["control_receipt"])["artifact_digest"])
}

func recallResponseNonExclusionDigest(disclosure map[string]any) string {
	material := map[string]any{
		"schema":              disclosure["union_schema"],
		"evidence_union":      disclosure["evidence_union"],
		"proof_union":         disclosure["proof_union"],
		"component_union":     disclosure["component_union"],
		"exclusion_refs":      disclosure["exclusion_refs"],
		"source_counts":       disclosure["source_counts"],
		"union_counts":        disclosure["union_counts"],
		"omitted_counts":      disclosure["omitted_counts"],
		"source_truncated":    disclosure["source_truncated"],
		"union_truncated":     disclosure["union_truncated"],
		"continuation_digest": disclosure["continuation_digest"],
		"ablation_witness":    disclosure["ablation_witness"],
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseOmissionCounterfactual(response map[string]any, outcome string, protected bool) map[string]any {
	disclosure := recallResponseDisclosure(response)
	controlReceipt := anyMap(disclosure["control_receipt"])
	controlUnionDigest := anyToString(controlReceipt["union_digest"])
	controlProofDigest := anyToString(controlReceipt["proof_identity_digest"])
	candidateProofDigest := recallResponseProofIdentityDigest(contextPackAnyList(disclosure["proof_union"]))
	scope := anyMap(response["request_scope"])
	sameSnapshot := recallResponseValidDigest(anyToString(controlReceipt["snapshot_digest"])) &&
		recallResponseValidDigest(anyToString(controlReceipt["receipt_digest"])) &&
		anyToBool(controlReceipt["source_bound"]) &&
		anyToBool(scope["source_bound"]) &&
		anyToString(controlReceipt["snapshot_digest"]) == anyToString(scope["snapshot_digest"]) &&
		anyToString(controlReceipt["receipt_digest"]) == anyToString(scope["receipt_digest"]) &&
		recallResponseValidDigest(anyToString(controlReceipt["artifact_digest"]))
	verified := !protected && outcome == "no_loss_verified" && !anyToBool(disclosure["union_truncated"]) && sameSnapshot && recallResponseEvidenceUnionSourceBound(disclosure) && recallResponseValidDigest(controlUnionDigest) && recallResponseValidDigest(controlProofDigest) && controlUnionDigest == anyToString(disclosure["union_digest"]) && controlProofDigest == candidateProofDigest
	if protected {
		verified = false
	}
	return map[string]any{
		"verified": verified,
		"outcome":  outcome,
	}
}

func recallResponseEvidenceUnionSourceBound(disclosure map[string]any) bool {
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		binding := anyMap(anyMap(raw)["evidence_binding"])
		if anyToBool(binding["source_bound"]) != true || !recallResponseOneOf(anyToString(binding["binding_authority"]), "server_receipt", "validated_policy") {
			return false
		}
	}
	return true
}

func recallResponseOmissionBindingDigest(disclosure map[string]any, itemRef, itemType string, proofRefs []any) string {
	material := map[string]any{
		"item_ref": itemRef, "item_type": itemType, "proof_refs": proofRefs,
		"union_digest": disclosure["union_digest"], "continuation_digest": disclosure["continuation_digest"],
	}
	return "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
}

func recallResponseOmissionBinding(response map[string]any, itemRef, itemType string) map[string]any {
	proofRefs := []any{}
	disclosure := recallResponseDisclosure(response)
	for _, raw := range contextPackAnyList(disclosure["proof_union"]) {
		if anyToString(raw) == itemRef {
			proofRefs = append(proofRefs, raw)
		}
	}
	for _, raw := range contextPackAnyList(disclosure["component_union"]) {
		row := anyMap(raw)
		if anyToString(row["component_ref"]) == itemRef {
			for _, proofRef := range contextPackAnyList(row["proof_refs"]) {
				if len(proofRefs) >= recallResponseMaxProofRefs {
					break
				}
				proofRefs = append(proofRefs, proofRef)
			}
		}
	}
	return map[string]any{
		"proof_refs":     proofRefs,
		"binding_digest": recallResponseOmissionBindingDigest(disclosure, itemRef, itemType, proofRefs),
	}
}

func recallResponseRecordOmission(response map[string]any, itemRef, itemType, reason string, protected bool, outcome string) {
	itemRef = strings.TrimSpace(itemRef)
	if itemRef == "" {
		return
	}
	disclosure := recallResponseDisclosure(response)
	ledger := contextPackAnyList(disclosure["omission_ledger"])
	for _, raw := range ledger {
		row := anyMap(raw)
		if anyToString(row["item_type"]) == itemType && anyToString(row["item_ref"]) == itemRef {
			if recallResponseOmissionPriority(reason, protected) > recallResponseOmissionPriority(anyToString(row["reason"]), anyToBool(row["protected"])) {
				if outcome == "" {
					outcome = "not_verified"
				}
				row["reason"] = reason
				row["protected"] = protected
				row["evidence_binding"] = recallResponseOmissionBinding(response, itemRef, itemType)
				row["same_snapshot_counterfactual"] = recallResponseOmissionCounterfactual(response, outcome, protected)
			}
			return
		}
	}
	if outcome == "" {
		outcome = "not_verified"
	}
	row := map[string]any{
		"item_ref":                     itemRef,
		"item_type":                    itemType,
		"reason":                       reason,
		"protected":                    protected,
		"evidence_binding":             recallResponseOmissionBinding(response, itemRef, itemType),
		"same_snapshot_counterfactual": recallResponseOmissionCounterfactual(response, outcome, protected),
	}
	if len(ledger) < recallResponseMaxOmissionLedger {
		ledger = append(ledger, row)
		disclosure["omission_ledger"] = ledger
		return
	}
	// Later presentation fitting must still name the exact item it removed.
	// When the bounded ledger is full, replace only a lower-priority receipt;
	// total membership and the displaced class remain bound by union counts,
	// the continuation digest, and the closed union digest.
	replacement := -1
	newPriority := recallResponseOmissionPriority(reason, protected)
	for index, raw := range ledger {
		existing := anyMap(raw)
		if anyToString(existing["reason"]) == reason && recallResponseOmissionPriority(anyToString(existing["reason"]), anyToBool(existing["protected"])) < newPriority {
			replacement = index
			break
		}
		if priority := recallResponseOmissionPriority(anyToString(existing["reason"]), anyToBool(existing["protected"])); priority < newPriority && (replacement < 0 || priority < recallResponseOmissionPriority(anyToString(anyMap(ledger[replacement])["reason"]), anyToBool(anyMap(ledger[replacement])["protected"]))) {
			replacement = index
		}
	}
	if replacement < 0 {
		return
	}
	ledger[replacement] = row
	disclosure["omission_ledger"] = ledger
}

func recallResponseOmissionPriority(reason string, protected bool) int {
	priority := map[string]int{
		"evidence_union_clipped":           10,
		"exclusion_union_clipped":          20,
		"source_capture_truncated":         90,
		"proof_union_clipped":              90,
		"component_union_clipped":          95,
		"presentation_component_omitted":   130,
		"presentation_evidence_omitted":    60,
		"response_budget_context":          110,
		"response_budget_secondary_module": 120,
		"synthetic_ablation":               150,
		"fail_closed_control":              200,
	}[reason]
	if priority == 0 {
		priority = 65
	}
	if protected {
		priority += 1000
	}
	return priority
}

func recallResponseRecordFailClosedWithdrawals(response map[string]any) {
	disclosure := recallResponseDisclosure(response)
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		row := anyMap(raw)
		recallResponseRecordOmission(response, anyToString(row["ref_id"]), "evidence", "fail_closed_control", anyToBool(row["protected"]), "fail_closed_control")
	}
	for _, raw := range contextPackAnyList(disclosure["component_union"]) {
		row := anyMap(raw)
		recallResponseRecordOmission(response, anyToString(row["component_ref"]), "component", "fail_closed_control", anyToBool(row["protected"]), "fail_closed_control")
	}
	// A protected row may have been clipped before the candidate failed
	// validation. Upgrade that existing receipt to an explicit fail-closed
	// outcome; protected evidence is never silently treated as shed.
	for _, raw := range contextPackAnyList(disclosure["omission_ledger"]) {
		row := anyMap(raw)
		if anyToBool(row["protected"]) {
			row["same_snapshot_counterfactual"] = recallResponseOmissionCounterfactual(response, "fail_closed_control", true)
		}
	}
}

func recallResponseAttachNonExclusion(response, input map[string]any, rankedEvidence []any, scopeDigest string, components []any, policy validatedRecallResponsePolicyInput) {
	disclosure := recallResponseDisclosure(response)
	sourceUnion := anyMap(input["_recall_response_source_union"])
	disclosure["union_schema"] = recallResponseNonExclusionSchema
	disclosure["evidence_union"] = recallResponseEvidenceUnionRows(input, rankedEvidence, scopeDigest, policy)
	if len(contextPackAnyList(response["conflicts"])) > 0 || len(contextPackAnyList(response["gaps"])) > 0 {
		rows := contextPackAnyList(disclosure["evidence_union"])
		if len(rows) > 1 && !anyToBool(anyMap(rows[1])["protected"]) {
			disclosure["evidence_union"] = rows[:1]
		}
	}
	componentAccounting := recallResponseComponentUnionRowsWithAccounting(components, recallResponseMaxPresentedComponents)
	disclosure["component_union"] = componentAccounting.rows
	exclusionAccounting := recallResponseExclusionRefsWithAccounting(input, rankedEvidence, scopeDigest, policy)
	disclosure["exclusion_refs"] = exclusionAccounting.refs
	proofAccounting := recallResponseProofUnionRefsWithAccounting(response, contextPackAnyList(disclosure["evidence_union"]), recallResponseAnyStrings(exclusionAccounting.allRefs), recallResponseAllConflictRefs(input, scopeDigest))
	disclosure["proof_union"] = proofAccounting.refs
	disclosure["source_counts"] = cloneJSONValue(sourceUnion["source_counts"])
	if len(anyMap(disclosure["source_counts"])) == 0 {
		disclosure["source_counts"] = map[string]any{"evidence": len(rankedEvidence), "temporal": 0, "proof": 0, "conflicts": 0}
	}
	disclosure["source_truncated"] = anyToBool(sourceUnion["source_truncated"])
	disclosure["union_counts"] = map[string]any{
		"evidence":   exclusionAccounting.eligibleCount,
		"exclusions": exclusionAccounting.excludedCount,
		"proof":      len(proofAccounting.allRefs),
		"components": len(componentAccounting.allRefs),
	}
	visibleEvidenceRefs := map[string]bool{}
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		visibleEvidenceRefs[anyToString(anyMap(raw)["ref_id"])] = true
	}
	sourceCounts := anyMap(disclosure["source_counts"])
	sourceOmitted := 0
	for _, class := range []string{"evidence", "temporal", "proof", "conflicts"} {
		sourceOmitted += maxInt(anyToInt(sourceCounts[class], 0)-len(contextPackAnyList(sourceUnion[class])), 0)
	}
	disclosure["omitted_counts"] = map[string]any{
		"source":     sourceOmitted,
		"evidence":   maxInt(len(exclusionAccounting.eligibleRefs)-len(visibleEvidenceRefs), 0),
		"exclusions": maxInt(exclusionAccounting.excludedCount-len(contextPackAnyList(exclusionAccounting.refs)), 0),
		"proof":      maxInt(len(proofAccounting.allRefs)-len(proofAccounting.refs), 0),
		"components": maxInt(len(componentAccounting.allRefs)-len(componentAccounting.rows), 0),
	}
	allUnionRefs := []string{}
	appendUnique := func(ref string) {
		ref = strings.TrimSpace(ref)
		if ref != "" && !containsString(allUnionRefs, ref) {
			allUnionRefs = append(allUnionRefs, ref)
		}
	}
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		appendUnique(anyToString(anyMap(raw)["ref_id"]))
	}
	for _, ref := range proofAccounting.allRefs {
		appendUnique(ref)
	}
	for _, ref := range componentAccounting.allRefs {
		appendUnique(ref)
	}
	for _, ref := range exclusionAccounting.allRefs {
		appendUnique(ref)
	}
	continuationMaterial := map[string]any{
		"ordered_union_refs":            allUnionRefs,
		"source_continuation_digest":    anyToString(sourceUnion["source_continuation_digest"]),
		"exclusion_continuation_digest": exclusionAccounting.continuation,
		"proof_continuation_digest":     proofAccounting.continuation,
		"component_continuation_digest": componentAccounting.continuation,
	}
	disclosure["union_truncated"] = anyToBool(disclosure["source_truncated"]) || exclusionAccounting.truncated || proofAccounting.truncated || componentAccounting.truncated
	disclosure["continuation_digest"] = "sha256:" + sha256Hex(recallResponseCanonicalJSON(continuationMaterial))
	disclosure["continuation_ref"] = recallResponseScopedOpaqueRef(scopeDigest, "continuation", anyToString(disclosure["continuation_digest"]))
	disclosure["continuation_action"] = recallResponseUnavailableContinuationAction(scopeDigest)
	disclosure["ablation_witness"] = recallResponseDefaultAblationWitness()
	disclosure["omission_ledger"] = []any{}
	disclosure["union_digest"] = recallResponseNonExclusionDigest(disclosure)
	scope := anyMap(response["request_scope"])
	snapshotDigest := anyToString(scope["snapshot_digest"])
	if !recallResponseValidDigest(snapshotDigest) {
		snapshotDigest = "sha256:" + sha256Hex("recall-response-control-snapshot\x00"+scopeDigest+"\x00"+anyToString(disclosure["union_digest"]))
	}
	receiptDigest := anyToString(scope["receipt_digest"])
	if !recallResponseValidDigest(receiptDigest) {
		receiptDigest = "sha256:" + sha256Hex("recall-response-control-receipt\x00"+scopeDigest+"\x00"+recallResponseCanonicalJSON(response["receipt_refs"]))
	}
	scope["snapshot_digest"] = snapshotDigest
	scope["receipt_digest"] = receiptDigest
	scope["source_bound"] = anyToBool(scope["source_bound"])
	asOf := firstNonEmptyStrings(anyToString(scope["as_of"]), anyToString(input["as_of"]), recallResponseLatestAsOf)
	scope["as_of"] = asOf
	temporalPremiseDigest := anyToString(scope["temporal_premise_digest"])
	if !recallResponseValidDigest(temporalPremiseDigest) {
		premise := recallResponseTemporalPremise(asOf, anyToString(anyMap(response["classification"])["facets"]))
		temporalPremiseDigest = anyToString(premise["digest"])
		scope["temporal_premise_digest"] = temporalPremiseDigest
	}
	recallResponseBindControlReceipt(response, snapshotDigest, receiptDigest, false)
	type omissionBatch struct {
		refs       []string
		itemType   string
		reason     string
		protected  func(string) bool
		firstIndex int
	}
	pendingOmissions := []omissionBatch{}
	recordOmissionRefs := func(refs []string, itemType, reason string, protected func(string) bool) {
		if len(refs) == 0 {
			return
		}
		// Reserve one bounded receipt for each clipping reason before filling the
		// remaining ledger slots. This keeps every clipped class explainable even
		// when many rows compete for the 32-row ledger.
		first := 0
		for index, ref := range refs {
			if protected != nil && protected(ref) {
				first = index
				break
			}
		}
		pendingOmissions = append(pendingOmissions, omissionBatch{refs: refs, itemType: itemType, reason: reason, protected: protected, firstIndex: first})
	}
	topLevelEvidenceRefs := map[string]bool{}
	for _, raw := range contextPackAnyList(response["evidence"]) {
		topLevelEvidenceRefs[anyToString(anyMap(raw)["ref_id"])] = true
	}
	presentationOmitted := []string{}
	for _, raw := range contextPackAnyList(disclosure["evidence_union"]) {
		ref := anyToString(anyMap(raw)["ref_id"])
		if ref != "" && !topLevelEvidenceRefs[ref] {
			presentationOmitted = append(presentationOmitted, ref)
		}
	}
	recordOmissionRefs(presentationOmitted, "evidence", "presentation_evidence_omitted", func(ref string) bool {
		return recallResponseNonExclusionProtected(response, ref)
	})
	for _, class := range []string{"evidence", "temporal", "proof", "conflicts"} {
		refs := []string{}
		for _, raw := range contextPackAnyList(anyMap(sourceUnion["source_truncated_refs"])[class]) {
			if ref := strings.TrimSpace(anyToString(raw)); ref != "" && !containsString(refs, ref) {
				refs = append(refs, ref)
			}
		}
		itemType := class
		if itemType == "conflicts" {
			itemType = "conflict"
		}
		recordOmissionRefs(refs, itemType, "source_capture_truncated", nil)
	}
	recordOmissionRefs(exclusionAccounting.clippedRefs, "evidence", "exclusion_union_clipped", nil)
	recordOmissionRefs(proofAccounting.clippedRefs, "proof", "proof_union_clipped", func(ref string) bool {
		return recallResponseNonExclusionProtected(response, ref)
	})
	recordOmissionRefs(componentAccounting.clippedRefs, "component", "component_union_clipped", func(ref string) bool {
		return componentAccounting.protected[ref]
	})
	evidenceClippedRefs := []string{}
	for _, ref := range exclusionAccounting.eligibleRefs {
		if !visibleEvidenceRefs[ref] {
			evidenceClippedRefs = append(evidenceClippedRefs, ref)
		}
	}
	recordOmissionRefs(evidenceClippedRefs, "evidence", "evidence_union_clipped", func(ref string) bool {
		return exclusionAccounting.eligibleProtected[ref]
	})
	// Reserve one receipt per omission class before filling remaining slots.
	// This keeps mixed >128-row continuations explainable even when one class
	// alone could consume the complete 32-row ledger.
	sort.SliceStable(pendingOmissions, func(left, right int) bool {
		leftRef := pendingOmissions[left].refs[pendingOmissions[left].firstIndex]
		rightRef := pendingOmissions[right].refs[pendingOmissions[right].firstIndex]
		leftProtected := pendingOmissions[left].protected != nil && pendingOmissions[left].protected(leftRef)
		rightProtected := pendingOmissions[right].protected != nil && pendingOmissions[right].protected(rightRef)
		return recallResponseOmissionPriority(pendingOmissions[left].reason, leftProtected) > recallResponseOmissionPriority(pendingOmissions[right].reason, rightProtected)
	})
	recordBatchOmission := func(batch omissionBatch, ref string) {
		protected := batch.protected != nil && batch.protected(ref)
		outcome := "not_verified"
		if protected {
			outcome = "fail_closed_control"
		}
		recallResponseRecordOmission(response, ref, batch.itemType, batch.reason, protected, outcome)
	}
	for _, batch := range pendingOmissions {
		ref := batch.refs[batch.firstIndex]
		recordBatchOmission(batch, ref)
	}
	for _, batch := range pendingOmissions {
		for index, ref := range batch.refs {
			if index == batch.firstIndex {
				continue
			}
			recordBatchOmission(batch, ref)
		}
	}
}

func recallResponseUnavailableContinuationAction(scopeDigest string) map[string]any {
	return map[string]any{
		"kind":               "owner_cursor_unavailable",
		"method":             "",
		"route":              "",
		"snapshot_semantics": "not_served",
		"request_contract":   "none",
		"scope_digest":       scopeDigest,
		"request_digest":     "",
		"token":              "",
		"page":               0,
	}
}

func recallResponseContinuationActionValid(response map[string]any, action map[string]any) bool {
	scope := anyMap(response["request_scope"])
	if !recallResponseExactFields(action, []string{
		"kind", "method", "route", "snapshot_semantics", "request_contract", "scope_digest", "request_digest", "token", "page",
	}) || anyToString(action["scope_digest"]) != anyToString(scope["scope_digest"]) ||
		!recallResponseValidDigest(anyToString(action["scope_digest"])) {
		return false
	}
	switch anyToString(action["kind"]) {
	case "continue_snapshot":
		return anyToString(action["method"]) == "POST" &&
			recallResponseOneOf(anyToString(action["route"]), memoryRecallResponsePath, toolsRecallResponsePath) &&
			anyToString(action["snapshot_semantics"]) == "same_snapshot" &&
			anyToString(action["request_contract"]) == "token+scope+agent+request_digest" &&
			recallResponseValidDigest(anyToString(action["request_digest"])) &&
			(strings.TrimSpace(anyToString(response["request_digest"])) == "" || anyToString(response["request_digest"]) == anyToString(action["request_digest"])) &&
			recallResponseValidContinuationToken(anyToString(action["token"])) && anyToInt(action["page"], 0) > 0
	case "terminal":
		return anyToString(action["method"]) == "" && anyToString(action["route"]) == "" &&
			anyToString(action["snapshot_semantics"]) == "exhausted" && anyToString(action["request_contract"]) == "none" &&
			anyToString(action["request_digest"]) == "" && anyToString(action["token"]) == "" && anyToInt(action["page"], -1) == 0
	case "owner_cursor_unavailable":
		return anyToString(action["method"]) == "" && anyToString(action["route"]) == "" &&
			anyToString(action["snapshot_semantics"]) == "not_served" && anyToString(action["request_contract"]) == "none" &&
			anyToString(action["request_digest"]) == "" && anyToString(action["token"]) == "" && anyToInt(action["page"], -1) == 0
	default:
		return false
	}
}

func recallResponseMergeComponentUnion(response map[string]any, components []any, ablations ...string) bool {
	disclosure := recallResponseDisclosure(response)
	ablation := ""
	if len(ablations) > 0 {
		ablation = strings.TrimSpace(ablations[0])
	}
	rows := contextPackAnyList(disclosure["component_union"])
	seen := map[string]bool{}
	for _, raw := range rows {
		seen[anyToString(anyMap(raw)["component_ref"])] = true
	}
	for _, raw := range components {
		row := anyMap(raw)
		refID := anyToString(row["component_ref"])
		if strings.TrimSpace(refID) == "" {
			continue
		}
		if !seen[refID] {
			// Component selection is a layout decision. A candidate may not add a
			// component outside the already materialized control union, because
			// doing so would make the candidate and control snapshots incomparable.
			return false
		}
	}
	selected := map[string]bool{}
	for _, raw := range components {
		selected[anyToString(anyMap(raw)["component_ref"])] = true
	}
	for _, raw := range rows {
		row := anyMap(raw)
		refID := anyToString(row["component_ref"])
		if selected[refID] {
			continue
		}
		if anyToBool(row["protected"]) {
			if anyToString(row["kind"]) != ablation {
				return false
			}
			recallResponseRecordOmission(response, refID, "component", "synthetic_ablation", true, "fail_closed_control")
			continue
		}
		recallResponseRecordOmission(response, refID, "component", "presentation_component_omitted", false, "not_verified")
	}
	return true
}

func recallResponseNonExclusionProtected(response map[string]any, refID string) bool {
	for _, raw := range contextPackAnyList(recallResponseDisclosure(response)["evidence_union"]) {
		if anyToString(anyMap(raw)["ref_id"]) == refID {
			return anyToBool(anyMap(raw)["protected"])
		}
	}
	for _, raw := range contextPackAnyList(recallResponseDisclosure(response)["component_union"]) {
		if anyToString(anyMap(raw)["component_ref"]) == refID {
			return anyToBool(anyMap(raw)["protected"])
		}
	}
	return false
}

// recallResponsePresentationClassClosedByContinuation permits a bounded
// presentation sample to yield its last per-row omission receipt only when
// the server has issued a same-snapshot cursor for that class. The complete
// owner-held typed membership, class counts, union digest, and continuation
// digest remain the proof of omission; an unavailable/local cursor never
// upgrades a missing row-level receipt.
func recallResponsePresentationClassClosedByContinuation(disclosure map[string]any, itemType string) bool {
	if disclosure == nil || !anyToBool(disclosure["union_truncated"]) {
		return false
	}
	action := anyMap(disclosure["continuation_action"])
	if anyToString(action["kind"]) != "continue_snapshot" || !recallResponseValidDigest(anyToString(disclosure["continuation_digest"])) {
		return false
	}
	omitted := anyMap(disclosure["omitted_counts"])
	return anyToInt(omitted[itemType], 0) > 0
}

func validateRecallResponseNonExclusion(response map[string]any) bool {
	disclosure := anyMap(response["disclosure"])
	controlFallback := recallResponseIsV1Control(response)
	if !recallResponseExactFields(disclosure, []string{
		"bounded", "raw_retrieval_included", "raw_prompt_included", "paths_included", "secrets_included",
		"inference_boundary", "omission_policy", "union_schema", "evidence_union", "proof_union",
		"component_union", "omission_ledger", "union_digest", "exclusion_refs", "union_truncated",
		"source_counts", "source_truncated", "union_counts", "omitted_counts", "continuation_digest", "continuation_ref", "continuation_action", "ablation_witness", "control_union_digest", "control_proof_identity_digest", "control_receipt",
	}) || anyToString(disclosure["union_schema"]) != recallResponseNonExclusionSchema || !recallResponseValidDigest(anyToString(disclosure["union_digest"])) || !recallResponseValidDigest(anyToString(disclosure["continuation_digest"])) || !recallResponseValidDigest(anyToString(disclosure["control_union_digest"])) || !recallResponseValidDigest(anyToString(disclosure["control_proof_identity_digest"])) || !recallResponseStringList(disclosure["exclusion_refs"]) || len(contextPackAnyList(disclosure["exclusion_refs"])) > recallResponseMaxUnionRefs {
		return false
	}
	if !recallResponseNonNegativeCountMap(disclosure["source_counts"], []string{"evidence", "temporal", "proof", "conflicts"}) || !recallResponseNonNegativeCountMap(disclosure["union_counts"], []string{"evidence", "exclusions", "proof", "components"}) || !recallResponseNonNegativeCountMap(disclosure["omitted_counts"], []string{"source", "evidence", "exclusions", "proof", "components"}) || !recallResponseExactOpaqueID(anyToString(disclosure["continuation_ref"]), "ref_continuation_") {
		return false
	}
	sourceTruncated, sourceTruncatedOK := disclosure["source_truncated"].(bool)
	unionTruncated, unionTruncatedOK := disclosure["union_truncated"].(bool)
	if !sourceTruncatedOK || !unionTruncatedOK || sourceTruncated && !unionTruncated {
		return false
	}
	controlReceipt := anyMap(disclosure["control_receipt"])
	if !recallResponseControlReceiptValid(response, controlReceipt) || anyToString(disclosure["control_union_digest"]) != anyToString(disclosure["union_digest"]) || anyToString(disclosure["control_proof_identity_digest"]) != anyToString(controlReceipt["proof_identity_digest"]) {
		return false
	}
	scope := anyMap(response["request_scope"])
	if anyToString(disclosure["continuation_ref"]) != recallResponseScopedOpaqueRef(anyToString(scope["scope_digest"]), "continuation", anyToString(disclosure["continuation_digest"])) {
		return false
	}
	if !recallResponseContinuationActionValid(response, anyMap(disclosure["continuation_action"])) {
		return false
	}
	if !recallResponseAblationWitnessValid(response, anyMap(disclosure["ablation_witness"])) {
		return false
	}
	evidenceUnion := contextPackAnyList(disclosure["evidence_union"])
	proofUnion := contextPackAnyList(disclosure["proof_union"])
	componentUnion := contextPackAnyList(disclosure["component_union"])
	ledger := contextPackAnyList(disclosure["omission_ledger"])
	if len(evidenceUnion) > recallResponseMaxUnionRefs || len(proofUnion) > recallResponseMaxUnionRefs || len(componentUnion) > recallResponseMaxModules || len(ledger) > recallResponseMaxOmissionLedger || !recallResponseStringList(disclosure["proof_union"]) {
		return false
	}
	evidenceSet := map[string]bool{}
	typedEvidenceSet := map[string]bool{}
	componentSet := map[string]bool{}
	visibleEvidenceSet := map[string]bool{}
	for _, raw := range contextPackAnyList(response["evidence"]) {
		if refID := strings.TrimSpace(anyToString(anyMap(raw)["ref_id"])); refID != "" {
			visibleEvidenceSet[refID] = true
		}
	}
	visibleComponentSet := map[string]bool{}
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		if refID := strings.TrimSpace(anyToString(anyMap(raw)["component_ref"])); refID != "" {
			visibleComponentSet[refID] = true
		}
	}
	ledgerByRef := map[string]map[string]any{}
	for _, raw := range ledger {
		row := anyMap(raw)
		ledgerByRef[anyToString(row["item_type"])+"\x00"+anyToString(row["item_ref"])] = row
	}
	exclusionSet := map[string]bool{}
	for _, raw := range contextPackAnyList(disclosure["exclusion_refs"]) {
		exclusionSet[anyToString(raw)] = true
	}
	for _, raw := range evidenceUnion {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"ref_id", "item_type", "kind", "role", "status", "confidence", "protected", "evidence_binding"}) || strings.TrimSpace(anyToString(row["ref_id"])) == "" || !recallResponseOneOf(anyToString(row["item_type"]), "evidence", "temporal", "proof") || !recallResponseEvidenceBindingValid(anyMap(row["evidence_binding"])) || !recallResponseOneOf(anyToString(row["role"]), "support", "context", "excluded") || !recallResponseValidDigest(anyToString(anyMap(row["evidence_binding"])["content_digest"])) || !anyToBool(row["protected"]) && anyToFloat(row["confidence"]) < 0 {
			return false
		}
		refID := anyToString(row["ref_id"])
		typedKey := recallResponseTypedItemKey(anyToString(row["item_type"]), refID)
		if typedEvidenceSet[typedKey] {
			return false
		}
		typedEvidenceSet[typedKey] = true
		evidenceSet[refID] = true
		if !containsString(recallResponseStringValues(proofUnion), refID) {
			return false
		}
		if !visibleEvidenceSet[refID] && !controlFallback {
			row := ledgerByRef["evidence\x00"+refID]
			if len(row) == 0 && !recallResponsePresentationClassClosedByContinuation(disclosure, "evidence") {
				return false
			}
		}
	}
	for _, raw := range componentUnion {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"component_ref", "kind", "protected"}) || strings.TrimSpace(anyToString(row["component_ref"])) == "" || !recallResponseModuleAllowed(anyToString(row["kind"])) {
			return false
		}
		refID := anyToString(row["component_ref"])
		if componentSet[refID] {
			return false
		}
		componentSet[refID] = true
		if !visibleComponentSet[refID] && !controlFallback {
			row := ledgerByRef["component\x00"+refID]
			if len(row) == 0 && !recallResponsePresentationClassClosedByContinuation(disclosure, "components") {
				classReceipt := false
				for _, raw := range ledger {
					omission := anyMap(raw)
					if anyToString(omission["item_type"]) == "component" && recallResponseOneOf(anyToString(omission["reason"]), "response_budget_secondary_module", "presentation_component_omitted", "synthetic_ablation") {
						classReceipt = true
						break
					}
				}
				if !classReceipt {
					return false
				}
			}
		}
	}
	// Exclusion refs are already materialized hard-exclusion receipts. They do
	// not need a duplicate omission row when the bounded proof sample clips the
	// same identity; total proof membership remains count/digest/continuation
	// bound below.
	for _, raw := range ledger {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"item_ref", "item_type", "reason", "protected", "evidence_binding", "same_snapshot_counterfactual"}) || strings.TrimSpace(anyToString(row["item_ref"])) == "" || !recallResponseOneOf(anyToString(row["item_type"]), "evidence", "temporal", "proof", "conflict", "component") || !recallResponseOmissionBindingValid(disclosure, row) || !recallResponseCounterfactualValid(response, row) {
			return false
		}
		refID := anyToString(row["item_ref"])
		reason := anyToString(row["reason"])
		if anyToString(row["item_type"]) == "evidence" && !evidenceSet[refID] && !exclusionSet[refID] && reason != "source_capture_truncated" && reason != "proof_union_clipped" && reason != "evidence_union_clipped" && reason != "exclusion_union_clipped" && reason != "presentation_evidence_omitted" && reason != "response_budget_context" || anyToString(row["item_type"]) == "component" && !componentSet[refID] && reason != "component_union_clipped" {
			return false
		}
		counterfactual := anyMap(row["same_snapshot_counterfactual"])
		verified := anyToBool(counterfactual["verified"])
		outcome := anyToString(counterfactual["outcome"])
		if verified != (outcome == "no_loss_verified") || !verified && outcome != "not_verified" && outcome != "fail_closed_control" {
			return false
		}
		if anyToBool(row["protected"]) && outcome != "fail_closed_control" {
			return false
		}
	}
	unionCounts := anyMap(disclosure["union_counts"])
	omittedCounts := anyMap(disclosure["omitted_counts"])
	if anyToInt(unionCounts["evidence"], -1) != len(evidenceUnion)+anyToInt(omittedCounts["evidence"], -1) ||
		anyToInt(unionCounts["exclusions"], -1) != len(contextPackAnyList(disclosure["exclusion_refs"]))+anyToInt(omittedCounts["exclusions"], -1) ||
		anyToInt(unionCounts["proof"], -1) != len(proofUnion)+anyToInt(omittedCounts["proof"], -1) ||
		anyToInt(unionCounts["components"], -1) != len(componentUnion)+anyToInt(omittedCounts["components"], -1) ||
		(sourceTruncated && anyToInt(omittedCounts["source"], 0) == 0) ||
		(anyToInt(omittedCounts["evidence"], 0) > 0 || anyToInt(omittedCounts["exclusions"], 0) > 0 || anyToInt(omittedCounts["proof"], 0) > 0 || anyToInt(omittedCounts["components"], 0) > 0) && !unionTruncated {
		return false
	}
	// Bounded class samples may yield their ledger slots to exact presentation
	// omissions. Full class membership is still closed by counts, union and
	// continuation digests; the owner-issued cursor resolves the omitted rows.
	return recallResponseNonExclusionDigest(disclosure) == anyToString(disclosure["union_digest"])
}

func recallResponseNonNegativeCountMap(value any, keys []string) bool {
	object := anyMap(value)
	if len(object) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, present := object[key]; !present || anyToInt(object[key], -1) < 0 {
			return false
		}
	}
	return true
}

func recallResponseEvidenceBindingValid(binding map[string]any) bool {
	authority := anyToString(binding["binding_authority"])
	sourceBound := anyToBool(binding["source_bound"])
	return recallResponseExactFields(binding, []string{"source_ref", "content_digest", "binding_authority", "source_bound"}) && strings.HasPrefix(anyToString(binding["source_ref"]), "ref_") && recallResponseValidDigest(anyToString(binding["content_digest"])) && recallResponseOneOf(authority, "server_receipt", "validated_policy", "derived") && sourceBound == (authority != "derived")
}

func recallResponseOmissionBindingValid(disclosure, row map[string]any) bool {
	binding := anyMap(row["evidence_binding"])
	proofRefs := contextPackAnyList(binding["proof_refs"])
	expected := recallResponseOmissionBindingDigest(disclosure, anyToString(row["item_ref"]), anyToString(row["item_type"]), proofRefs)
	return recallResponseExactFields(binding, []string{"proof_refs", "binding_digest"}) && recallResponseStringList(binding["proof_refs"]) && anyToString(binding["binding_digest"]) == expected
}

func recallResponseCounterfactualValid(response, row map[string]any) bool {
	counterfactual := anyMap(row["same_snapshot_counterfactual"])
	verified := anyToBool(counterfactual["verified"])
	outcome := anyToString(counterfactual["outcome"])
	expected := recallResponseOmissionCounterfactual(response, outcome, anyToBool(row["protected"]))
	return recallResponseExactFields(counterfactual, []string{"verified", "outcome"}) && recallResponseOneOf(outcome, "no_loss_verified", "not_verified", "fail_closed_control") && verified == (outcome == "no_loss_verified") && recallResponseCanonicalJSON(counterfactual) == recallResponseCanonicalJSON(expected)
}

func recallResponseControlReceiptValid(response map[string]any, receipt map[string]any) bool {
	if !recallResponseExactFields(receipt, []string{"snapshot_digest", "receipt_digest", "union_digest", "proof_identity_digest", "source_bound", "artifact_digest"}) ||
		!recallResponseBoolField(receipt, "source_bound") ||
		!recallResponseValidDigest(anyToString(receipt["snapshot_digest"])) || !recallResponseValidDigest(anyToString(receipt["receipt_digest"])) ||
		!recallResponseValidDigest(anyToString(receipt["union_digest"])) || !recallResponseValidDigest(anyToString(receipt["proof_identity_digest"])) || !recallResponseValidDigest(anyToString(receipt["artifact_digest"])) {
		return false
	}
	scope := anyMap(response["request_scope"])
	if !recallResponseBoolField(scope, "source_bound") || anyToString(receipt["snapshot_digest"]) != anyToString(scope["snapshot_digest"]) || anyToString(receipt["receipt_digest"]) != anyToString(scope["receipt_digest"]) || anyToBool(receipt["source_bound"]) != anyToBool(scope["source_bound"]) {
		return false
	}
	material := recallResponseControlReceiptMaterial(anyToString(receipt["snapshot_digest"]), anyToString(receipt["receipt_digest"]), anyToString(receipt["union_digest"]), anyToString(receipt["proof_identity_digest"]), anyToBool(receipt["source_bound"]))
	return "sha256:"+sha256Hex(recallResponseCanonicalJSON(material)) == anyToString(receipt["artifact_digest"])
}

func recallResponseBoolField(value map[string]any, key string) bool {
	_, ok := value[key].(bool)
	return ok
}
