package main

import (
	"sort"
	"strings"
	"time"
)

// recallResponseTemporalEvidenceAtOrBefore applies event time, validity, and
// explicit status transitions before a row can become supporting evidence.
// Ambiguous historical status is excluded rather than reconstructed from a
// row's present-day status.
func recallResponseTemporalEvidenceAtOrBefore(items []any, asOf string) ([]any, int) {
	filtered, unverifiable := recallResponseEvidenceAtOrBeforeWithGaps(items, asOf)
	cutoff := recallResponseNowUTC()
	if asOf != recallResponseLatestAsOf {
		parsed, err := time.Parse(time.RFC3339Nano, asOf)
		if err != nil {
			return []any{}, len(items)
		}
		cutoff = parsed
	}
	out := make([]any, 0, len(filtered))
	for _, raw := range filtered {
		item := cloneJSONMap(anyMap(raw))
		if !recallResponseTemporalValidAt(item, cutoff) {
			unverifiable++
			continue
		}
		if asOf != recallResponseLatestAsOf {
			status, known := recallResponseTemporalStatusAt(item, cutoff)
			if !known {
				unverifiable++
				continue
			}
			item = recallResponseTemporalRowAt(item, cutoff, status)
		}
		out = append(out, item)
	}
	return out, unverifiable
}

func recallResponseTemporalRowAt(item map[string]any, cutoff time.Time, status string) map[string]any {
	item["status"] = status
	filterTransitions := func(value any) []any {
		out := []any{}
		for _, raw := range contextPackAnyList(value) {
			row := anyMap(raw)
			at, err := time.Parse(time.RFC3339Nano, firstNonEmptyStrings(
				anyToString(row["effective_at"]), anyToString(row["observed_at"]), anyToString(row["at"]),
			))
			if err == nil && !at.After(cutoff) {
				out = append(out, cloneJSONMap(row))
			}
		}
		return out
	}
	recallMetadata := cloneJSONMap(anyMap(item["recall_metadata"]))
	temporal := cloneJSONMap(anyMap(recallMetadata["temporal"]))
	if len(temporal) > 0 {
		temporal["status"] = status
		temporal["transitions"] = filterTransitions(temporal["transitions"])
		recallMetadata["temporal"] = temporal
		item["recall_metadata"] = recallMetadata
	} else if _, present := item["status_transitions"]; present {
		item["status_transitions"] = filterTransitions(item["status_transitions"])
	}
	return item
}

func recallResponseTemporalValidAt(item map[string]any, cutoff time.Time) bool {
	metadata := recallResponseTemporalMetadata(item)
	observed := firstNonEmptyStrings(
		anyToString(item["as_of"]), anyToString(item["valid_at"]), anyToString(item["occurred_at"]),
		anyToString(item["observed_at"]), anyToString(item["updated_at"]), anyToString(item["created_at"]),
		anyToString(item["timestamp"]),
	)
	if observed != "" {
		parsed, err := time.Parse(time.RFC3339Nano, observed)
		if err != nil {
			return false
		}
		if parsed.After(cutoff) {
			return false
		}
	}
	from := firstNonEmptyStrings(anyToString(metadata["valid_from"]), anyToString(item["valid_from"]))
	to := firstNonEmptyStrings(anyToString(metadata["valid_to"]), anyToString(item["valid_to"]))
	if from != "" {
		parsed, err := time.Parse(time.RFC3339Nano, from)
		if err != nil {
			return false
		}
		if parsed.After(cutoff) {
			return false
		}
	}
	if to != "" {
		parsed, err := time.Parse(time.RFC3339Nano, to)
		if err != nil {
			return false
		}
		if !parsed.After(cutoff) {
			return false
		}
	}
	return true
}

func recallResponseTemporalStatusAt(item map[string]any, cutoff time.Time) (string, bool) {
	metadata := recallResponseTemporalMetadata(item)
	transitions := contextPackAnyList(metadata["transitions"])
	if len(transitions) == 0 {
		transitions = contextPackAnyList(item["status_transitions"])
	}
	type transition struct {
		at     time.Time
		status string
	}
	rows := []transition{}
	for _, raw := range transitions {
		row := anyMap(raw)
		atValue := firstNonEmptyStrings(anyToString(row["effective_at"]), anyToString(row["observed_at"]), anyToString(row["at"]))
		at, err := time.Parse(time.RFC3339Nano, atValue)
		status := recallResponseSafeStatus(anyToString(row["status"]))
		if err != nil || status == "unknown" {
			return "unknown", false
		}
		rows = append(rows, transition{at: at, status: status})
	}
	if len(rows) > 0 {
		if !anyToBool(metadata["transition_history_complete"]) {
			return "unknown", false
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
		status := ""
		for index, row := range rows {
			if index > 0 && row.at.Equal(rows[index-1].at) && row.status != rows[index-1].status {
				return "unknown", false
			}
			if row.at.After(cutoff) {
				break
			}
			status = row.status
		}
		return status, status != ""
	}

	rawStatus := firstNonEmptyStrings(anyToString(metadata["status"]), anyToString(item["status"]), anyToString(item["proof_status"]))
	if strings.TrimSpace(rawStatus) == "" {
		status, eligible := recallResponseEvidenceStatus(item)
		return status, eligible
	}
	status := recallResponseSafeStatus(rawStatus)
	if status == "superseded" || status == "retracted" || status == "revoked" || status == "expired" {
		// A present-day retired label does not establish when retirement began.
		return "unknown", false
	}
	return status, status != "unknown"
}

func recallResponseTemporalMetadata(row map[string]any) map[string]any {
	metadata := anyMap(anyMap(row["recall_metadata"])["temporal"])
	if len(metadata) > 0 {
		return metadata
	}
	return row
}

func recallResponseTemporalRows(source map[string]any) []any {
	pack := anyMap(source["context_pack"])
	if len(pack) == 0 {
		pack = anyMap(source["contextPack"])
	}
	rows := []any{}
	rows = append(rows, contextPackAnyList(pack["ranked_evidence"])...)
	rows = append(rows, contextPackAnyList(pack["rankedEvidence"])...)
	rows = append(rows, contextPackAnyList(pack["temporal_claims"])...)
	rows = append(rows, contextPackAnyList(pack["proof_claims"])...)
	rows = append(rows, contextPackAnyList(source["evidence"])...)
	rows = append(rows, contextPackAnyList(source["temporal_claims"])...)
	rows = append(rows, contextPackAnyList(source["proof_claims"])...)
	return rows
}

func recallResponseTemporalHasRetirement(source map[string]any) bool {
	for _, raw := range recallResponseTemporalRows(source) {
		row := anyMap(raw)
		metadata := recallResponseTemporalMetadata(row)
		status := recallResponseSafeStatus(firstNonEmptyStrings(anyToString(metadata["status"]), anyToString(row["status"]), anyToString(row["proof_status"])))
		if status == "superseded" || status == "retracted" || status == "revoked" || status == "expired" ||
			anyToInt(metadata["supersedes_count"], len(contextPackAnyList(row["supersedes"]))) > 0 {
			return true
		}
	}
	return false
}

func recallResponseTimelinePayload(response map[string]any, refs []string, source map[string]any) map[string]any {
	ordering := "source_order"
	for _, raw := range recallResponseTemporalRows(source) {
		metadata := recallResponseTemporalMetadata(anyMap(raw))
		if len(contextPackAnyList(metadata["transitions"])) > 0 || len(contextPackAnyList(anyMap(raw)["status_transitions"])) > 0 {
			ordering = "explicit_status_transitions"
			break
		}
	}
	return map[string]any{
		"event_refs":        recallResponseAnyStrings(refs),
		"ordering":          ordering,
		"unknown_intervals": recallResponseUnknownPeriods(response, refs, source),
		"causal_claim_refs": recallResponseTemporalCausalRefs(refs, source),
	}
}

func recallResponseConflictPayload(response map[string]any, refs []string, source map[string]any) map[string]any {
	winner := recallResponseTemporalWinner(response, refs, source)
	status := "unresolved"
	reason := ""
	if winner != "" {
		status = "proven_superseded"
		reason = winner
	} else if len(refs) > 0 {
		reason = refs[0]
	}
	return map[string]any{
		"claim_refs":            recallResponseAnyStrings(refs),
		"winner_ref":            winner,
		"resolution_status":     status,
		"resolution_reason_ref": reason,
		"unknown_periods":       recallResponseUnknownPeriods(response, refs, source),
	}
}

func recallResponseTemporalWinner(response map[string]any, refs []string, source map[string]any) string {
	if !anyToBool(anyMap(response["state"])["source_complete"]) || len(contextPackAnyList(response["conflicts"])) > 0 || anyToBool(source["_snapshot_revision_changed"]) {
		return ""
	}
	proofSet := map[string]bool{}
	for _, ref := range refs {
		proofSet[ref] = true
	}
	rows := recallResponseTemporalRows(source)
	winner := ""
	for _, raw := range rows {
		row := anyMap(raw)
		metadata := recallResponseTemporalMetadata(row)
		status := recallResponseSafeStatus(firstNonEmptyStrings(anyToString(metadata["status"]), anyToString(row["status"]), anyToString(row["proof_status"])))
		if status != "active" && status != "current" && status != "selected" && status != "supported" {
			continue
		}
		rawTargets := recallResponseRawSupersessionTargets(row)
		if len(rawTargets) > recallResponseMaxProofRefs {
			return ""
		}
		targets := recallResponseSupersessionTargets(row)
		count := anyToInt(metadata["supersedes_count"], len(targets))
		complete := anyToBool(metadata["transition_history_complete"])
		if count == 0 {
			continue
		}
		if count != len(targets) || !complete || !recallResponseSupersessionTargetsProven(rows, targets) {
			return ""
		}
		ref := recallResponseProjectedRowRef(row, response)
		if ref == "" || !proofSet[ref] || (winner != "" && winner != ref) {
			return ""
		}
		winner = ref
	}
	return winner
}

func recallResponseSupersessionTargets(row map[string]any) []string {
	values := recallResponseRawSupersessionTargets(row)
	out := []string{}
	for _, value := range values {
		identity := strings.TrimSpace(anyToString(value))
		if identity == "" || containsString(out, identity) || len(out) >= recallResponseMaxProofRefs {
			continue
		}
		out = append(out, identity)
	}
	return out
}

func recallResponseRawSupersessionTargets(row map[string]any) []any {
	raw := anyMap(row["temporal_evidence"])
	values := contextPackAnyList(raw["supersedes"])
	if len(values) == 0 {
		values = contextPackAnyList(row["supersedes"])
	}
	return values
}

func recallResponseTemporalRowIdentity(row map[string]any) string {
	return strings.TrimSpace(firstNonEmptyStrings(
		anyToString(row["candidate_id"]), anyToString(row["ref_id"]),
		anyToString(row["claim_id"]), anyToString(row["memory_id"]),
	))
}

func recallResponseSupersessionTargetsProven(rows []any, targets []string) bool {
	if len(targets) == 0 {
		return false
	}
	for _, target := range targets {
		proven := false
		for _, raw := range rows {
			row := anyMap(raw)
			if recallResponseTemporalRowIdentity(row) != target {
				continue
			}
			metadata := recallResponseTemporalMetadata(row)
			status := recallResponseSafeStatus(firstNonEmptyStrings(anyToString(metadata["status"]), anyToString(row["status"]), anyToString(row["proof_status"])))
			if (status == "superseded" || status == "retracted" || status == "revoked" || status == "expired") &&
				anyToBool(metadata["transition_history_complete"]) {
				proven = true
				break
			}
		}
		if !proven {
			return false
		}
	}
	return true
}

func recallResponseProjectedRowRef(row, response map[string]any) string {
	identity := firstNonEmptyStrings(
		anyToString(row["candidate_id"]), anyToString(row["ref_id"]),
		anyToString(row["claim_id"]), anyToString(row["memory_id"]),
	)
	evidenceSet := map[string]bool{}
	for _, raw := range contextPackAnyList(response["evidence"]) {
		ref := anyToString(anyMap(raw)["ref_id"])
		evidenceSet[ref] = true
		if ref == identity {
			return ref
		}
	}
	if identity == "" {
		return ""
	}
	scopeDigest := anyToString(anyMap(response["request_scope"])["scope_digest"])
	for index := 0; index < recallResponseMaxEvidence; index++ {
		ref := recallResponseScopedOpaqueRef(scopeDigest, "evidence", identity+"\x00"+anyToString(index))
		if evidenceSet[ref] {
			return ref
		}
	}
	return ""
}

func recallResponseUnknownPeriods(response map[string]any, refs []string, source map[string]any) []any {
	if !recallResponseTemporalHasRetirement(source) || len(refs) == 0 {
		return []any{}
	}
	for _, raw := range recallResponseTemporalRows(source) {
		row := anyMap(raw)
		metadata := recallResponseTemporalMetadata(row)
		transitions := contextPackAnyList(metadata["transitions"])
		if len(transitions) == 0 {
			transitions = contextPackAnyList(row["status_transitions"])
		}
		if anyToBool(metadata["transition_history_complete"]) && len(transitions) > 0 {
			continue
		}
		return []any{map[string]any{
			"start": "unknown", "end": anyToString(anyMap(response["request_scope"])["as_of"]),
			"basis_ref": refs[0], "reason": "transition_boundary_unproven",
		}}
	}
	return []any{}
}

func recallResponseTemporalCausalRefs(refs []string, source map[string]any) []any {
	if !recallResponseTemporalHasRetirement(source) {
		return []any{}
	}
	return recallResponseAnyStrings(refs)
}

func recallResponseExplicitNegativeTerminal(source map[string]any) string {
	for _, raw := range recallResponseTemporalRows(source) {
		row := anyMap(raw)
		terminal := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(
			anyToString(anyMap(row["recall_metadata"])["negative_terminal"]),
			anyToString(row["negative_terminal"]), anyToString(row["event_status"]),
		)))
		if terminal == "did_not_happen" {
			return terminal
		}
	}
	return ""
}

func recallResponseNegativePayload(response map[string]any, refs []string, source map[string]any) map[string]any {
	receipt := recallResponseNegativeCoverageReceipt(response, refs, source)
	terminal := "unknown"
	negativeRef := recallResponseExplicitNegativeRef(response, refs, source)
	if negativeRef != "" && anyToBool(receipt["complete"]) {
		terminal = "did_not_happen"
	} else if recallResponseBoundedAbsence(response) && anyToBool(receipt["complete"]) {
		terminal = "not_found"
	}
	if terminal != "did_not_happen" {
		negativeRef = ""
	}
	return map[string]any{"terminal": terminal, "coverage_receipt": receipt, "negative_claim_ref": negativeRef}
}

func recallResponseExplicitNegativeRef(response map[string]any, refs []string, source map[string]any) string {
	for _, raw := range recallResponseTemporalRows(source) {
		row := anyMap(raw)
		terminal := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(
			anyToString(anyMap(row["recall_metadata"])["negative_terminal"]),
			anyToString(row["negative_terminal"]), anyToString(row["event_status"]),
		)))
		if terminal != "did_not_happen" {
			continue
		}
		ref := recallResponseProjectedRowRef(row, response)
		if ref != "" && containsString(refs, ref) {
			return ref
		}
	}
	return ""
}

func recallResponseBoundedAbsence(response map[string]any) bool {
	if len(contextPackAnyList(response["evidence"])) != 0 || len(contextPackAnyList(response["conflicts"])) != 0 {
		return false
	}
	for _, raw := range contextPackAnyList(response["gaps"]) {
		switch anyToString(anyMap(raw)["code"]) {
		case "no_bounded_evidence":
		default:
			return false
		}
	}
	return true
}

func recallResponseNegativeCoverageReceipt(response map[string]any, refs []string, source map[string]any) map[string]any {
	coverage := anyMap(source["source_coverage"])
	if len(coverage) == 0 {
		coverage = anyMap(source["sourceCoverage"])
	}
	scope := anyMap(response["request_scope"])
	returned := anyToStringList(coverage["returned"], 32)
	complete := anyToBool(coverage["complete"]) && len(returned) > 0 && !anyToBool(source["_snapshot_revision_changed"])
	for _, raw := range contextPackAnyList(response["gaps"]) {
		if anyToString(anyMap(raw)["code"]) != "no_bounded_evidence" {
			complete = false
		}
	}
	reason := "incomplete"
	if complete && recallResponseExplicitNegativeRef(response, refs, source) != "" {
		reason = "explicit_negative"
	} else if complete && recallResponseBoundedAbsence(response) {
		reason = "no_match"
	}
	pack := anyMap(source["context_pack"])
	proofRefs := append([]string(nil), refs...)
	sort.Strings(proofRefs)
	coverageBasis := map[string]any{
		"source_universe":  returned,
		"authorized_scope": []any{scope["owner_ref"], scope["workspace_ref"], scope["project_ref"]},
		"as_of":            scope["as_of"],
		"proof_refs":       recallResponseAnyStrings(proofRefs),
		"snapshot_digest":  scope["snapshot_digest"],
		"receipt_digest":   scope["receipt_digest"],
		"source_revisions": map[string]any{
			"vector": pack["source_revision_vector"], "start": pack["snapshot_revision_start"], "end": pack["snapshot_revision_end"],
		},
	}
	material := map[string]any{
		"basis_digest": "sha256:" + sha256Hex(recallResponseCanonicalJSON(coverageBasis)),
		"complete":     complete,
		"reason":       reason,
	}
	material["receipt_digest"] = "sha256:" + sha256Hex(recallResponseCanonicalJSON(material))
	return material
}

func recallResponseUnknownPeriodsValid(value any, proofSet map[string]bool, asOf string) bool {
	rows, ok := value.([]any)
	if !ok || len(rows) > recallResponseMaxProofRefs {
		return false
	}
	for _, raw := range rows {
		row := anyMap(raw)
		if !recallResponseExactFields(row, []string{"start", "end", "basis_ref", "reason"}) ||
			anyToString(row["start"]) != "unknown" || anyToString(row["end"]) != asOf ||
			!proofSet[anyToString(row["basis_ref"])] || anyToString(row["reason"]) != "transition_boundary_unproven" {
			return false
		}
	}
	return true
}

func recallResponseNegativePayloadValid(payload map[string]any, proofSet map[string]bool, scope map[string]any) bool {
	terminal := anyToString(payload["terminal"])
	if !recallResponseOneOf(terminal, "unknown", "not_found", "did_not_happen") {
		return false
	}
	claimRef, ok := payload["negative_claim_ref"].(string)
	if !ok || (claimRef != "" && !proofSet[claimRef]) {
		return false
	}
	receipt := anyMap(payload["coverage_receipt"])
	fields := []string{"basis_digest", "complete", "reason", "receipt_digest"}
	if !recallResponseExactFields(receipt, fields) ||
		!recallResponseValidDigest(anyToString(receipt["basis_digest"])) ||
		!recallResponseValidDigest(anyToString(receipt["receipt_digest"])) {
		return false
	}
	if _, ok := receipt["complete"].(bool); !ok {
		return false
	}
	digestMaterial := cloneJSONMap(receipt)
	delete(digestMaterial, "receipt_digest")
	if anyToString(receipt["receipt_digest"]) != "sha256:"+sha256Hex(recallResponseCanonicalJSON(digestMaterial)) {
		return false
	}
	complete := anyToBool(receipt["complete"])
	reason := anyToString(receipt["reason"])
	switch terminal {
	case "not_found":
		return complete && claimRef == "" && reason == "no_match"
	case "did_not_happen":
		return complete && claimRef != "" && reason == "explicit_negative"
	default:
		return claimRef == "" && reason == "incomplete"
	}
}
