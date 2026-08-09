package main

import "strings"

const recallResponseMaxFacetLabels = 4

var recallResponseConsequenceRank = map[string]int{
	"informational":       1,
	"decision_supporting": 2,
	"executable":          3,
	"sensitive":           4,
	"high_stakes":         5,
}

// recallResponseFacets derives the five independent routing facets. Request
// text and task metadata are signals only: evidence and authority continue to
// come exclusively from the server-produced retrieval projection.
func recallResponseFacets(
	input map[string]any,
	rankedEvidence []any,
	sourceCoverage map[string]any,
	retrievalIntent string,
	evidenceCount, conflictCount, gapCount int,
	asOf string,
) map[string]any {
	query := strings.ToLower(strings.Join(strings.Fields(anyToString(input["query"])), " "))
	taskClass := recallResponseSafeTaskClass(anyToString(input["task_class"]))

	jobs := []string{}
	objects := []string{}
	addJob := func(value string) { jobs = appendRecallResponseString(jobs, value, recallResponseMaxFacetLabels) }
	addObject := func(value string) { objects = appendRecallResponseString(objects, value, recallResponseMaxFacetLabels) }

	actionSignal := taskClass == "action" || recallResponseContainsAny(query, "take action", "execute", "perform the action", "use the tool", "action preparation", "advisory action")
	procedureSignal := taskClass == "procedure" || recallResponseContainsAny(query, "how to", "procedure", "runbook", "steps")
	switch retrievalIntent {
	case "decision":
		addJob("verify")
		addJob("explain")
		addObject("decision")
	case "procedure":
		if taskClass == "action" {
			addJob("act")
		} else {
			addJob("apply")
		}
		addObject("procedure")
	case "status", "exact", "bounded":
		addJob("look_up")
	case "proof":
		addJob("verify")
	case "synthesis", "advisory":
		addJob("explain")
		addJob("compare")
	}

	switch taskClass {
	case "status", "fact":
		addJob("look_up")
		addObject("fact")
	case "decision":
		addJob("explain")
		addObject("decision")
	case "continuation":
		addJob("continue")
		addObject("project_state")
	case "procedure":
		addJob("apply")
		addObject("procedure")
	case "action":
		addJob("act")
		addObject("procedure")
	case "timeline":
		addJob("reconstruct")
		addObject("event")
	case "preference":
		addJob("look_up")
		addObject("preference")
	}

	if recallResponseContainsAny(query, "continue", "resume", "next step", "checkpoint", "left off") {
		addJob("continue")
		addObject("project_state")
	}
	if recallResponseContainsAny(query, "why", "rationale", "reason", "explain") {
		addJob("explain")
		addObject("decision")
	}
	if recallResponseContainsAny(query, "compare", "synthesize", "across") {
		addJob("compare")
	}
	if recallResponseContainsAny(query, "when", "timeline", "history", "before", "after") {
		addJob("reconstruct")
		addObject("event")
	}
	if procedureSignal && !actionSignal {
		addJob("apply")
		addObject("procedure")
	}
	if actionSignal {
		addJob("act")
		addObject("procedure")
	}
	if recallResponseContainsAny(query, "preference", "constraint", "must", "never") {
		addObject("constraint")
	}
	if recallResponseContainsAny(query, "conflict", "competing claim", "supersession", "supersede") {
		addObject("conflict")
	}
	if recallResponseContainsAny(query, "bounded absence", "did not happen", "does not exist", "not found", "whether") &&
		recallResponseContainsAny(query, "exist", "happen", "found", "absence") {
		addObject("negative")
	}
	// Execution-like language mixed with another unsupported intent is
	// ambiguous unless the caller supplied the dedicated normalized action or
	// procedure class. Preserve the informational labels, but do not guess an
	// apply/act job from free text.
	otherIntentSignal := recallResponseContainsAny(query,
		"continue", "resume", "next step", "checkpoint", "left off",
		"why", "rationale", "reason", "explain", "compare", "synthesize", "across",
		"when", "timeline", "history", "before", "after",
	)
	if taskClass == "general" && retrievalIntent != "procedure" && (actionSignal || procedureSignal) && otherIntentSignal {
		filtered := jobs[:0]
		for _, job := range jobs {
			if job != "apply" && job != "act" {
				filtered = append(filtered, job)
			}
		}
		jobs = filtered
		addJob("verify")
	}
	for _, raw := range rankedEvidence {
		switch strings.ToLower(strings.TrimSpace(anyToString(anyMap(raw)["kind"]))) {
		case "fact", "event", "decision", "preference", "constraint", "procedure", "policy", "relationship", "identity":
			addObject(strings.ToLower(strings.TrimSpace(anyToString(anyMap(raw)["kind"]))))
		}
	}
	if len(jobs) == 0 {
		addJob("look_up")
	}
	if len(objects) == 0 {
		addObject("durable_memory")
	}
	if evidenceCount > 0 {
		addObject("proof_receipt")
	}
	if evidenceCount == 0 {
		addJob("verify")
	}

	evidenceState := "absent"
	if evidenceCount > 0 {
		evidenceState = "clean"
	}
	if !anyToBool(sourceCoverage["complete"]) {
		evidenceState = "degraded"
	} else if evidenceCount > 0 && gapCount > 0 {
		evidenceState = "sparse"
	}
	if conflictCount > 0 {
		evidenceState = "conflicting"
	} else if anyToBool(input["_snapshot_revision_changed"]) {
		evidenceState = "degraded"
	}
	if evidenceCount == 0 {
		for _, raw := range rankedEvidence {
			status, _ := recallResponseEvidenceStatus(anyMap(raw))
			if status == "quarantined" {
				evidenceState = "quarantined"
				break
			}
			if status == "superseded" || status == "retracted" {
				evidenceState = "superseded"
			}
		}
	}

	temporalState := "current_or_unknown"
	if asOf != recallResponseLatestAsOf {
		temporalState = "historical"
	} else if recallResponseContainsAny(query, "timeline", "history", "before", "after", "changed") {
		temporalState = "changed_over_time"
	}

	consequence := "informational"
	if retrievalIntent == "decision" {
		consequence = "decision_supporting"
	} else if retrievalIntent == "procedure" || taskClass == "action" || actionSignal {
		consequence = "executable"
	}
	if actionSignal || procedureSignal {
		consequence = recallResponseStricterConsequence(consequence, "executable")
	}
	// Caller signals may only increase caution. They cannot make a response
	// more actionable or strengthen its evidence state.
	for _, hint := range []string{
		anyToString(input["consequence_hint"]),
		anyToString(anyMap(input["classification"])["consequence"]),
	} {
		consequence = recallResponseStricterConsequence(consequence, hint)
	}

	return map[string]any{
		"jobs":           recallResponseAnyStrings(jobs),
		"memory_objects": recallResponseAnyStrings(objects),
		"temporal_state": temporalState,
		"evidence_state": evidenceState,
		"consequence":    consequence,
	}
}

func recallResponseDerivedPosture(facets map[string]any, evidenceCount, conflictCount, gapCount int) string {
	if evidenceCount == 0 || anyToString(facets["evidence_state"]) == "absent" || anyToString(facets["evidence_state"]) == "quarantined" {
		return "abstain"
	}
	if conflictCount > 0 || gapCount > 0 || anyToString(facets["evidence_state"]) != "clean" ||
		recallResponseConsequenceRank[anyToString(facets["consequence"])] >= recallResponseConsequenceRank["executable"] {
		return "verify_before_action"
	}
	return "answer_with_proof"
}

func recallResponseLegacyClassification(facets map[string]any, posture string) map[string]any {
	return map[string]any{
		"jobs":           cloneJSONValue(facets["jobs"]),
		"objects":        cloneJSONValue(facets["memory_objects"]),
		"temporal_mode":  facets["temporal_state"],
		"evidence_state": facets["evidence_state"],
		"consequence":    facets["consequence"],
		"posture":        posture,
		"facets":         cloneJSONMap(facets),
	}
}

func recallResponseSafeTaskClass(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "status", "fact", "decision", "continuation", "procedure", "action", "timeline", "preference", "general":
		return value
	default:
		return "general"
	}
}

func recallResponseContainsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func appendRecallResponseString(values []string, value string, limit int) []string {
	if value == "" || containsString(values, value) || (limit > 0 && len(values) >= limit) {
		return values
	}
	return append(values, value)
}

func recallResponseAnyStrings(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}
