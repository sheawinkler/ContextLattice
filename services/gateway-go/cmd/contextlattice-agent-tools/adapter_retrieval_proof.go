package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	adapterMemoryTrustAssessmentContractID  = "memory_trust_assessment.v1"
	adapterRetrievalDecisionTraceContractID = "retrieval_decision_trace.v1"
	adapterMemoryTrustAssessmentPath        = "$.memory_trust_assessment"
	adapterRetrievalDecisionTracePath       = "$.retrieval_decision_trace"
)

func adapterProofInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > math.MaxInt64 {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		raw := typed.String()
		if raw == "" || strings.TrimSpace(raw) != raw || strings.ContainsAny(raw, ".eE") || !json.Valid([]byte(raw)) {
			return 0, false
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func adapterProofList(value any) ([]any, bool) {
	items, ok := value.([]any)
	return items, ok
}

func adapterProofFormatContractValid(proof map[string]any, schemaID string) bool {
	formatContract, ok := proof["format_contract"].(map[string]any)
	if !ok {
		return false
	}
	contract := adapterContractDefinition(schemaID)
	contractVersion, versionOK := adapterProofInt64(formatContract["contract_version"])
	registryVersion, registryOK := adapterProofInt64(formatContract["registry_version"])
	maximumTotal, maximumTotalOK := adapterProofInt64(formatContract["max_total_json_bytes"])
	maximumString, maximumStringOK := adapterProofInt64(formatContract["max_string_bytes"])
	maximumList, maximumListOK := adapterProofInt64(formatContract["max_list_items"])
	actual, actualOK := adapterProofInt64(formatContract["actual_json_bytes"])
	expectedVersion := int64(asInt(contract["contract_version"]))
	encoded, encodedErr := json.Marshal(proof)
	validation, validationOK := formatContract["validation"].(map[string]any)
	errorsValue, errorsOK := adapterProofList(validation["errors"])
	return len(contract) > 0 && expectedVersion > 0 && versionOK && registryOK && validationOK && errorsOK &&
		firstString(formatContract["registry_id"]) == generatedAgentContractRegistryID &&
		registryVersion == int64(generatedAgentContractRegistryVersion) &&
		firstString(formatContract["schema_id"]) == schemaID && contractVersion == expectedVersion &&
		firstString(formatContract["required_output_mode"]) == firstString(contract["required_output_mode"]) &&
		firstString(formatContract["validator"]) == "contextlattice.boundary.v1" &&
		maximumTotalOK && maximumTotal == int64(asInt(contract["max_total_json_bytes"])) &&
		maximumStringOK && maximumString == int64(asInt(contract["max_string_bytes"])) &&
		maximumListOK && maximumList == int64(asInt(contract["max_list_items"])) &&
		actualOK && actual > 0 && actual <= maximumTotal && encodedErr == nil && actual == int64(len(encoded)) &&
		formatContract["contract_valid"] == true && firstString(validation["status"]) == "passed" && len(errorsValue) == 0
}

func adapterProofExactID(value any, prefix string) bool {
	text, ok := value.(string)
	if !ok || !strings.HasPrefix(text, prefix) || len(text) != len(prefix)+24 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(text, prefix))
	return err == nil && strings.ToLower(text) == text
}

func adapterProofDigest(value any) bool {
	text, ok := value.(string)
	if !ok || !strings.HasPrefix(text, "sha256:") || len(text) != len("sha256:")+64 || strings.ToLower(text) != text {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(text, "sha256:"))
	return err == nil
}

func adapterProofTraceID(value any) (string, bool) {
	text, ok := value.(string)
	if !ok || !adapterProofExactID(text, "rdt_") {
		return "", false
	}
	return text, true
}

func adapterRetrievalProofCountsValid(proof map[string]any, schemaID string, reference bool) bool {
	fields := []string{}
	if !reference {
		fields = append(fields, "version")
	}
	switch schemaID {
	case adapterMemoryTrustAssessmentContractID:
		fields = append(fields, "assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count")
		if !reference {
			fields = append(fields, "input_candidate_count", "processed_candidate_count")
		}
	case adapterRetrievalDecisionTraceContractID:
		fields = append(fields, "candidate_count", "decision_count", "input_truncated_count")
		if !reference {
			fields = append(fields, "processed_candidate_count")
		}
	default:
		return false
	}
	for _, field := range fields {
		count, ok := adapterProofInt64(proof[field])
		if !ok || count < 0 {
			return false
		}
	}
	if reference {
		return true
	}
	version, ok := adapterProofInt64(proof["version"])
	if !ok || version != 1 {
		return false
	}
	boundary, ok := proof["input_boundary"].(map[string]any)
	if !ok {
		return false
	}
	maximumCandidates, maximumOK := adapterProofInt64(boundary["maximum_candidates"])
	omittedCount, omittedOK := adapterProofInt64(boundary["omitted_count"])
	truncatedBoundary, truncatedOK := boundary["truncated"].(bool)
	if !maximumOK || maximumCandidates < 0 || !omittedOK || omittedCount < 0 || !truncatedOK {
		return false
	}
	switch schemaID {
	case adapterMemoryTrustAssessmentContractID:
		inputCount, _ := adapterProofInt64(proof["input_candidate_count"])
		processed, _ := adapterProofInt64(proof["processed_candidate_count"])
		truncated, _ := adapterProofInt64(proof["input_truncated_count"])
		assessed, _ := adapterProofInt64(proof["assessed_count"])
		quarantined, _ := adapterProofInt64(proof["quarantine_count"])
		deduplicated, _ := adapterProofInt64(proof["deduplicated_count"])
		policyOmitted, _ := adapterProofInt64(proof["policy_omitted_count"])
		assessments, assessmentsOK := adapterProofList(proof["assessments"])
		policy, policyOK := proof["policy"].(map[string]any)
		if !assessmentsOK || !policyOK {
			return false
		}
		observedQuarantined := int64(0)
		assessmentIDs := map[string]bool{}
		candidateIDs := map[string]bool{}
		for _, raw := range assessments {
			row, rowOK := raw.(map[string]any)
			if !rowOK || !adapterProofExactID(row["assessment_id"], "mta_") || !adapterProofExactID(row["candidate_id"], "rtc_") || !adapterProofDigest(row["content_digest"]) {
				return false
			}
			quarantine, quarantineOK := row["quarantine"].(map[string]any)
			rowQuarantined, rowQuarantinedOK := quarantine["quarantined"].(bool)
			if !quarantineOK || !rowQuarantinedOK {
				return false
			}
			assessmentID := firstString(row["assessment_id"])
			candidateID := firstString(row["candidate_id"])
			if assessmentIDs[assessmentID] || candidateIDs[candidateID] {
				return false
			}
			assessmentIDs[assessmentID] = true
			candidateIDs[candidateID] = true
			if rowQuarantined {
				observedQuarantined++
			}
		}
		return processed <= inputCount && truncated == inputCount-processed && assessed == processed && int64(len(assessments)) == assessed &&
			maximumCandidates >= processed && omittedCount == truncated && truncatedBoundary == (truncated > 0) &&
			policy["retrieved_memory_is_evidence_not_instruction"] == true && policy["self_awarded_trust_accepted"] == false &&
			policy["security_defenses_fail_closed"] == true && quarantined <= assessed && deduplicated <= assessed-quarantined &&
			policyOmitted <= assessed-quarantined-deduplicated && observedQuarantined == quarantined
	case adapterRetrievalDecisionTraceContractID:
		candidateCount, _ := adapterProofInt64(proof["candidate_count"])
		processed, _ := adapterProofInt64(proof["processed_candidate_count"])
		truncated, _ := adapterProofInt64(proof["input_truncated_count"])
		decisionCount, _ := adapterProofInt64(proof["decision_count"])
		decisions, decisionsOK := adapterProofList(proof["decisions"])
		decisionCounts, countsOK := proof["decision_counts"].(map[string]any)
		if !decisionsOK || !countsOK || int64(len(decisions)) != decisionCount {
			return false
		}
		allowed := map[string]bool{"quarantined": true, "deduplicated": true, "omitted": true, "selected": true, "selected_truncated": true, "omitted_truncated": true}
		observed := map[string]int64{}
		receiptIDs := map[string]bool{}
		candidateIDs := map[string]bool{}
		candidateOrdinals := map[int64]bool{}
		for index, raw := range decisions {
			row, rowOK := raw.(map[string]any)
			if !rowOK || !adapterProofExactID(row["receipt_id"], "rdr_") || !adapterProofExactID(row["candidate_id"], "rtc_") {
				return false
			}
			decision, decisionOK := row["decision"].(string)
			ordinal, ordinalOK := adapterProofInt64(row["candidate_ordinal"])
			order, orderOK := adapterProofInt64(row["decision_order"])
			if !decisionOK || !allowed[decision] || !ordinalOK || ordinal < 1 || ordinal > processed || !orderOK || order != int64(index+1) {
				return false
			}
			receiptID := firstString(row["receipt_id"])
			candidateID := firstString(row["candidate_id"])
			if receiptIDs[receiptID] || candidateIDs[candidateID] || candidateOrdinals[ordinal] {
				return false
			}
			receiptIDs[receiptID] = true
			candidateIDs[candidateID] = true
			candidateOrdinals[ordinal] = true
			observed[decision]++
		}
		if len(decisionCounts) != len(observed) {
			return false
		}
		for category, value := range decisionCounts {
			count, countOK := adapterProofInt64(value)
			if !allowed[category] || !countOK || count < 0 || observed[category] != count {
				return false
			}
		}
		coverage, coverageOK := proof["coverage_complete"].(bool)
		redaction, redactionOK := proof["redaction"].(map[string]any)
		return coverageOK && processed <= candidateCount && truncated == candidateCount-processed && decisionCount == processed &&
			maximumCandidates >= processed && omittedCount == truncated && truncatedBoundary == (truncated > 0) && coverage == (truncated == 0) &&
			redactionOK && redaction["raw_candidate_text_included"] == false && redaction["secret_values_included"] == false
	}
	return false
}

func adapterRetrievalProofPairValid(assessment, trace map[string]any) bool {
	assessmentAvailable, assessmentHasAvailability := assessment["available"].(bool)
	traceAvailable, traceHasAvailability := trace["available"].(bool)
	assessmentUnavailable := assessmentHasAvailability && !assessmentAvailable
	traceUnavailable := traceHasAvailability && !traceAvailable
	if assessmentUnavailable || traceUnavailable {
		return assessmentUnavailable && traceUnavailable
	}
	assessed, assessedOK := adapterProofInt64(assessment["assessed_count"])
	quarantined, quarantinedOK := adapterProofInt64(assessment["quarantine_count"])
	deduplicated, deduplicatedOK := adapterProofInt64(assessment["deduplicated_count"])
	policyOmitted, policyOmittedOK := adapterProofInt64(assessment["policy_omitted_count"])
	assessmentTruncated, assessmentTruncatedOK := adapterProofInt64(assessment["input_truncated_count"])
	candidateCount, candidateOK := adapterProofInt64(trace["candidate_count"])
	decisionCount, decisionOK := adapterProofInt64(trace["decision_count"])
	traceTruncated, traceTruncatedOK := adapterProofInt64(trace["input_truncated_count"])
	coverage, coverageOK := trace["coverage_complete"].(bool)
	if !assessedOK || !quarantinedOK || !deduplicatedOK || !policyOmittedOK || !assessmentTruncatedOK || !candidateOK || !decisionOK || !traceTruncatedOK || !coverageOK ||
		assessed < 0 || quarantined < 0 || deduplicated < 0 || policyOmitted < 0 || assessmentTruncated < 0 || candidateCount < 0 || decisionCount < 0 || traceTruncated < 0 ||
		assessed != decisionCount || assessmentTruncated != traceTruncated || decisionCount > math.MaxInt64-traceTruncated || candidateCount != decisionCount+traceTruncated ||
		coverage != (traceTruncated == 0) || quarantined > assessed || deduplicated > assessed-quarantined || policyOmitted > assessed-quarantined-deduplicated {
		return false
	}
	assessments, assessmentsOK := adapterProofList(assessment["assessments"])
	decisions, decisionsOK := adapterProofList(trace["decisions"])
	if !assessmentsOK || !decisionsOK {
		return true
	}
	inputCount, inputOK := adapterProofInt64(assessment["input_candidate_count"])
	assessmentProcessed, assessmentProcessedOK := adapterProofInt64(assessment["processed_candidate_count"])
	traceProcessed, traceProcessedOK := adapterProofInt64(trace["processed_candidate_count"])
	if !inputOK || !assessmentProcessedOK || !traceProcessedOK || inputCount != candidateCount || assessmentProcessed != traceProcessed || decisionCount != assessmentProcessed {
		return false
	}
	assessmentCandidates := map[string]int{}
	quarantinedCandidates := map[string]int{}
	for _, raw := range assessments {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		candidateID, candidateOK := row["candidate_id"].(string)
		quarantine, quarantineOK := row["quarantine"].(map[string]any)
		rowQuarantined, rowQuarantinedOK := quarantine["quarantined"].(bool)
		if !candidateOK || !quarantineOK || !rowQuarantinedOK {
			return false
		}
		assessmentCandidates[candidateID]++
		if rowQuarantined {
			quarantinedCandidates[candidateID]++
		}
	}
	decisionCandidates := map[string]int{}
	traceQuarantinedCandidates := map[string]int{}
	for _, raw := range decisions {
		row, ok := raw.(map[string]any)
		if !ok {
			return false
		}
		candidateID, candidateOK := row["candidate_id"].(string)
		decision, decisionOK := row["decision"].(string)
		if !candidateOK || !decisionOK {
			return false
		}
		decisionCandidates[candidateID]++
		if decision == "quarantined" {
			traceQuarantinedCandidates[candidateID]++
		}
	}
	if !reflect.DeepEqual(assessmentCandidates, decisionCandidates) || !reflect.DeepEqual(quarantinedCandidates, traceQuarantinedCandidates) {
		return false
	}
	decisionCounts, ok := trace["decision_counts"].(map[string]any)
	if !ok {
		return false
	}
	traceQuarantined, traceQuarantinedOK := adapterProofInt64(decisionCounts["quarantined"])
	traceDeduplicated, traceDeduplicatedOK := adapterProofInt64(decisionCounts["deduplicated"])
	omitted, omittedOK := adapterProofInt64(decisionCounts["omitted"])
	omittedTruncated, omittedTruncatedOK := adapterProofInt64(decisionCounts["omitted_truncated"])
	if !traceQuarantinedOK {
		traceQuarantined = 0
	}
	if !traceDeduplicatedOK {
		traceDeduplicated = 0
	}
	if !omittedOK {
		omitted = 0
	}
	if !omittedTruncatedOK {
		omittedTruncated = 0
	}
	policyCovered := omitted >= policyOmitted || (omitted < policyOmitted && omittedTruncated >= policyOmitted-omitted)
	return traceQuarantined == quarantined && traceDeduplicated == deduplicated && policyCovered
}

func adapterUnavailableRetrievalProof(schemaID string) map[string]any {
	path := adapterRetrievalDecisionTracePath
	if schemaID == adapterMemoryTrustAssessmentContractID {
		path = adapterMemoryTrustAssessmentPath
	}
	return map[string]any{
		"schema_id": schemaID, "canonical_path": path, "available": false,
		"reason": "a same-origin retrieval proof pair was not available before the adapter boundary",
	}
}

func adapterProjectedRetrievalProofValid(proof map[string]any, schemaID, path string) bool {
	available, availableOK := proof["available"].(bool)
	bounded, boundedOK := proof["bounded_projection"].(bool)
	if !availableOK || !available || !boundedOK || !bounded || firstString(proof["canonical_path"]) != path || !adapterProofDigest(proof["canonical_digest"]) {
		return false
	}
	countFields := []string{"candidate_count", "decision_count", "input_truncated_count"}
	expected := map[string]bool{"schema_id": true, "canonical_path": true, "available": true, "bounded_projection": true, "canonical_digest": true, "candidate_count": true, "decision_count": true, "input_truncated_count": true, "trace_id": true, "coverage_complete": true}
	if schemaID == adapterMemoryTrustAssessmentContractID {
		countFields = []string{"assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count"}
		expected = map[string]bool{"schema_id": true, "canonical_path": true, "available": true, "bounded_projection": true, "canonical_digest": true, "assessed_count": true, "quarantine_count": true, "deduplicated_count": true, "policy_omitted_count": true, "input_truncated_count": true}
	}
	for _, field := range countFields {
		count, ok := adapterProofInt64(proof[field])
		if !ok || count < 0 {
			return false
		}
	}
	if schemaID == adapterRetrievalDecisionTraceContractID {
		if _, ok := proof["coverage_complete"].(bool); !ok {
			return false
		}
		traceID, traceIDOK := proof["trace_id"].(string)
		omitted, omittedPresent := proof["trace_id_omitted"].(bool)
		if omittedPresent {
			if !omitted || !traceIDOK || traceID != "" {
				return false
			}
			expected["trace_id_omitted"] = true
		} else if !traceIDOK {
			return false
		} else if _, ok := adapterProofTraceID(traceID); !ok {
			return false
		}
	}
	if len(proof) != len(expected) {
		return false
	}
	for field := range proof {
		if !expected[field] {
			return false
		}
	}
	return true
}

func adapterCanonicalRetrievalProof(value any, schemaID string, allowReference bool) map[string]any {
	proof, ok := value.(map[string]any)
	if !ok || firstString(proof["schema_id"]) != schemaID {
		return nil
	}
	listField := "decisions"
	path := adapterRetrievalDecisionTracePath
	if schemaID == adapterMemoryTrustAssessmentContractID {
		listField = "assessments"
		path = adapterMemoryTrustAssessmentPath
	}
	if listValue, exists := proof[listField]; exists {
		items, itemsOK := adapterProofList(listValue)
		countField := "decision_count"
		if schemaID == adapterMemoryTrustAssessmentContractID {
			countField = "assessed_count"
		}
		count, countOK := adapterProofInt64(proof[countField])
		if !itemsOK || !countOK || count != int64(len(items)) || proof["ok"] != true || proof["bounded"] != true ||
			!adapterProofFormatContractValid(proof, schemaID) || len(adapterContractFindings(schemaID, proof)) != 0 ||
			!adapterRetrievalProofCountsValid(proof, schemaID, false) {
			return nil
		}
		return proof
	}
	if available, exists := proof["available"].(bool); exists && !available && firstString(proof["canonical_path"]) == path {
		return adapterUnavailableRetrievalProof(schemaID)
	}
	if adapterProjectedRetrievalProofValid(proof, schemaID, path) {
		return proof
	}
	if !allowReference || firstString(proof["canonical_path"]) != path || !adapterRetrievalProofCountsValid(proof, schemaID, true) {
		return nil
	}
	expected := map[string]bool{"schema_id": true, "canonical_path": true, "assessed_count": true, "quarantine_count": true, "deduplicated_count": true, "policy_omitted_count": true, "input_truncated_count": true}
	if schemaID == adapterRetrievalDecisionTraceContractID {
		expected = map[string]bool{"schema_id": true, "canonical_path": true, "trace_id": true, "candidate_count": true, "decision_count": true, "input_truncated_count": true, "coverage_complete": true}
		if _, ok := adapterProofTraceID(proof["trace_id"]); !ok {
			return nil
		}
		if _, ok := proof["coverage_complete"].(bool); !ok {
			return nil
		}
	}
	if len(proof) != len(expected) {
		return nil
	}
	for field := range proof {
		if !expected[field] {
			return nil
		}
	}
	return proof
}

func adapterRetrievalProofCanonicalJSON(value any) (string, error) {
	var out strings.Builder
	if err := writeAdapterRetrievalProofCanonicalJSON(&out, value, 0); err != nil {
		return "", err
	}
	return out.String(), nil
}

func writeAdapterRetrievalProofCanonicalJSON(out *strings.Builder, value any, depth int) error {
	if depth > 64 {
		return errors.New("retrieval proof JSON exceeds maximum depth")
	}
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		if !utf8.ValidString(typed) {
			return errors.New("retrieval proof contains invalid UTF-8")
		}
		writeAdapterRetrievalProofJSONString(out, typed)
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			if !utf8.ValidString(key) {
				return errors.New("retrieval proof key contains invalid UTF-8")
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				out.WriteByte(',')
			}
			writeAdapterRetrievalProofJSONString(out, key)
			out.WriteByte(':')
			if err := writeAdapterRetrievalProofCanonicalJSON(out, typed[key], depth+1); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	case []any:
		out.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				out.WriteByte(',')
			}
			if err := writeAdapterRetrievalProofCanonicalJSON(out, item, depth+1); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case int:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int8:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int16:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int32:
		out.WriteString(strconv.FormatInt(int64(typed), 10))
	case int64:
		out.WriteString(strconv.FormatInt(typed, 10))
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return errors.New("retrieval proof integer exceeds signed int64")
		}
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint8:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint16:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint32:
		out.WriteString(strconv.FormatUint(uint64(typed), 10))
	case uint64:
		if typed > math.MaxInt64 {
			return errors.New("retrieval proof integer exceeds signed int64")
		}
		out.WriteString(strconv.FormatUint(typed, 10))
	case float32:
		formatted, err := adapterProofPythonFloatJSON(float64(typed))
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	case float64:
		formatted, err := adapterProofPythonFloatJSON(typed)
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	case json.Number:
		raw := typed.String()
		if raw == "" || strings.TrimSpace(raw) != raw || !json.Valid([]byte(raw)) {
			return errors.New("invalid JSON number")
		}
		if !strings.ContainsAny(raw, ".eE") {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return err
			}
			out.WriteString(strconv.FormatInt(parsed, 10))
			return nil
		}
		number, err := strconv.ParseFloat(raw, 64)
		if err != nil && !(errors.Is(err, strconv.ErrRange) && number == 0) {
			return err
		}
		formatted, err := adapterProofPythonFloatJSON(number)
		if err != nil {
			return err
		}
		out.WriteString(formatted)
	default:
		return fmt.Errorf("unsupported retrieval proof JSON type %T", value)
	}
	return nil
}

func writeAdapterRetrievalProofJSONString(out *strings.Builder, value string) {
	out.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if char < 0x20 {
				_, _ = fmt.Fprintf(out, `\u%04x`, char)
			} else {
				out.WriteRune(char)
			}
		}
	}
	out.WriteByte('"')
}

func adapterProofPythonFloatJSON(value float64) (string, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "", errors.New("nonfinite retrieval proof number")
	}
	abs := math.Abs(value)
	if abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		return strconv.FormatFloat(value, 'e', -1, 64), nil
	}
	formatted := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(formatted, ".") {
		formatted += ".0"
	}
	return formatted, nil
}

func adapterProjectRetrievalProof(proof map[string]any, schemaID string) map[string]any {
	listField := "decisions"
	path := adapterRetrievalDecisionTracePath
	countFields := []string{"candidate_count", "decision_count", "input_truncated_count"}
	if schemaID == adapterMemoryTrustAssessmentContractID {
		listField = "assessments"
		path = adapterMemoryTrustAssessmentPath
		countFields = []string{"assessed_count", "quarantine_count", "deduplicated_count", "policy_omitted_count", "input_truncated_count"}
	}
	if _, full := proof[listField]; !full {
		return proof
	}
	canonical, err := adapterRetrievalProofCanonicalJSON(proof)
	if err != nil {
		return adapterUnavailableRetrievalProof(schemaID)
	}
	digest := sha256.Sum256([]byte(canonical))
	projection := map[string]any{
		"schema_id": schemaID, "canonical_path": path, "available": true,
		"bounded_projection": true, "canonical_digest": "sha256:" + hex.EncodeToString(digest[:]),
	}
	for _, field := range countFields {
		count, ok := adapterProofInt64(proof[field])
		if !ok || count < 0 {
			return adapterUnavailableRetrievalProof(schemaID)
		}
		projection[field] = count
	}
	if schemaID == adapterRetrievalDecisionTraceContractID {
		if traceID, ok := adapterProofTraceID(proof["trace_id"]); ok {
			projection["trace_id"] = traceID
		} else {
			projection["trace_id"] = ""
			projection["trace_id_omitted"] = true
		}
		coverage, ok := proof["coverage_complete"].(bool)
		if !ok {
			return adapterUnavailableRetrievalProof(schemaID)
		}
		projection["coverage_complete"] = coverage
	}
	return projection
}

func adapterPrepareContextPackRetrievalProofs(payload map[string]any) bool {
	contextPack, _ := payload["context_pack"].(map[string]any)
	compiler, _ := payload["context_compiler"].(map[string]any)
	if len(compiler) == 0 && len(contextPack) > 0 {
		compiler, _ = contextPack["context_compiler"].(map[string]any)
	}
	assessment := map[string]any{}
	trace := map[string]any{}
	selectedOrigin := false
	for _, origin := range []struct {
		owner          map[string]any
		allowReference bool
	}{{payload, true}, {contextPack, false}, {compiler, false}} {
		if len(origin.owner) == 0 {
			continue
		}
		_, assessmentPresent := origin.owner["memory_trust_assessment"]
		_, tracePresent := origin.owner["retrieval_decision_trace"]
		if !assessmentPresent && !tracePresent {
			continue
		}
		selectedOrigin = true
		candidateAssessment := adapterCanonicalRetrievalProof(origin.owner["memory_trust_assessment"], adapterMemoryTrustAssessmentContractID, origin.allowReference)
		candidateTrace := adapterCanonicalRetrievalProof(origin.owner["retrieval_decision_trace"], adapterRetrievalDecisionTraceContractID, origin.allowReference)
		if len(candidateAssessment) > 0 && len(candidateTrace) > 0 && adapterRetrievalProofPairValid(candidateAssessment, candidateTrace) {
			assessment, trace = candidateAssessment, candidateTrace
		}
		break
	}
	if !selectedOrigin || len(assessment) == 0 || len(trace) == 0 {
		assessment = adapterUnavailableRetrievalProof(adapterMemoryTrustAssessmentContractID)
		trace = adapterUnavailableRetrievalProof(adapterRetrievalDecisionTraceContractID)
	} else {
		assessment = adapterProjectRetrievalProof(assessment, adapterMemoryTrustAssessmentContractID)
		trace = adapterProjectRetrievalProof(trace, adapterRetrievalDecisionTraceContractID)
		if !adapterRetrievalProofPairValid(assessment, trace) {
			assessment = adapterUnavailableRetrievalProof(adapterMemoryTrustAssessmentContractID)
			trace = adapterUnavailableRetrievalProof(adapterRetrievalDecisionTraceContractID)
		}
	}
	payload["memory_trust_assessment"] = assessment
	payload["retrieval_decision_trace"] = trace
	if len(contextPack) > 0 {
		contextPack["memory_trust_assessment"] = assessment
		contextPack["retrieval_decision_trace"] = trace
		payload["context_pack"] = contextPack
	}
	if len(compiler) > 0 {
		compiler["memory_trust_assessment"] = assessment
		compiler["retrieval_decision_trace"] = trace
		payload["context_compiler"] = compiler
		if len(contextPack) > 0 {
			contextPack["context_compiler"] = compiler
		}
	}
	formatContract, ok := payload["format_contract"].(map[string]any)
	if !ok {
		return false
	}
	for attempts := 0; attempts < 12; attempts++ {
		raw, err := json.Marshal(payload)
		if err != nil {
			return false
		}
		actual, actualOK := adapterProofInt64(formatContract["actual_json_bytes"])
		if actualOK && actual == int64(len(raw)) {
			return true
		}
		formatContract["actual_json_bytes"] = len(raw)
	}
	return false
}
