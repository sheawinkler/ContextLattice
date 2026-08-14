package main

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSearchIntelligenceCandidateIdentityCollapsesTrueContentDuplicatesAndSeparatesPassages(t *testing.T) {
	sharedContent := "The verified release gate requires the local test receipt."
	left := searchIntelligenceCandidateIdentity(map[string]any{
		"content": sharedContent,
		"summary": "The verified release gate requires the local test receipt.",
		"project": "alpha", "file": "notes/release-a.md", "source": "qdrant",
	})
	right := searchIntelligenceCandidateIdentity(map[string]any{
		"content": sharedContent,
		"summary": "The verified release gate requires the local test receipt.",
		"project": "beta", "file": "archive/release-copy.md", "source": "mongo_raw",
	})
	otherPassage := searchIntelligenceCandidateIdentity(map[string]any{
		"content": sharedContent,
		"summary": "The release gate also requires an independent review receipt.",
		"project": "alpha", "file": "notes/release-a.md", "source": "qdrant",
	})

	if left.CandidateRef != right.CandidateRef || left.ContentRef != right.ContentRef || left.PassageRef != right.PassageRef {
		t.Fatalf("true cross-store duplicate did not collapse: left=%#v right=%#v", left, right)
	}
	if left.LocatorRef == right.LocatorRef {
		t.Fatalf("distinct locators were not retained as separate digest references: left=%#v right=%#v", left, right)
	}
	if left.CandidateRef == otherPassage.CandidateRef || left.PassageRef == otherPassage.PassageRef {
		t.Fatalf("distinct passages were not distinguished: left=%#v other=%#v", left, otherPassage)
	}
	for _, ref := range []string{left.CandidateRef, left.ContentRef, left.PassageRef, left.LocatorRef} {
		if !isSearchIntelligenceFullSHA256Ref(ref) {
			t.Fatalf("candidate identity did not use a full SHA-256 reference: %q", ref)
		}
	}
}

func TestMergeRowsKeepsDistinctPassagesFromOneFile(t *testing.T) {
	rows := map[string][]map[string]any{
		"qdrant": {
			{"content": "A release note with multiple decisions.", "summary": "first decision", "project": "alpha", "file": "notes/release.md", "score": 0.93},
			{"content": "A release note with multiple decisions.", "summary": "second decision", "project": "alpha", "file": "notes/release.md", "score": 0.91},
		},
	}
	merged := mergeRowsAll(rows)
	if len(merged) != 2 {
		t.Fatalf("same-file passages collapsed in the literal merge: %#v", merged)
	}
	if rowIdentity(merged[0]) == rowIdentity(merged[1]) {
		t.Fatalf("same-file passages share a pipeline identity: %#v", merged)
	}
}

func TestMergeRowsEqualScoreConflictIsDeterministicAndConservative(t *testing.T) {
	permissive := map[string]any{
		"content": "same retained record", "summary": "same retained record", "project": "alpha",
		"file": "notes/record.md", "chunk_id": "chunk-1", "source": "qdrant", "score": 0.88,
		"status": "current", "support": "direct", "action_evidence": map[string]any{
			"tool_ref": "sha256:" + strings.Repeat("a", 64),
		},
	}
	excluded := map[string]any{
		"content": "same retained record", "summary": "same retained record", "project": "alpha",
		"file": "notes/record.md", "chunk_id": "chunk-1", "source": "topic_rollups", "score": 0.88,
		"status": "retired", "lifecycle": "retired", "support": "distractor", "action_evidence": map[string]any{
			"tool_ref": "sha256:" + strings.Repeat("b", 64),
		},
	}
	if rowIdentity(permissive) != rowIdentity(excluded) {
		t.Fatalf("fixture rows did not share identity: permissive=%q excluded=%q", rowIdentity(permissive), rowIdentity(excluded))
	}
	left := map[string][]map[string]any{
		"qdrant":        {permissive},
		"topic_rollups": {excluded},
	}
	right := map[string][]map[string]any{
		"topic_rollups": {cloneMap(excluded)},
		"qdrant":        {cloneMap(permissive)},
	}
	first, err := json.Marshal(mergeRowsAll(left))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 20; attempt++ {
		merged := mergeRowsAll(right)
		serialized, marshalErr := json.Marshal(merged)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if string(serialized) != string(first) {
			t.Fatalf("equal-score duplicate merge changed with input order: first=%s attempt=%s", first, serialized)
		}
		if len(merged) != 1 || anyToString(merged[0]["status"]) != "retired" ||
			anyToString(merged[0]["support"]) != "distractor" || merged[0]["action_evidence"] != nil ||
			anyToString(merged[0]["merge_conflict"]) != "conservative_duplicate_exclusion" {
			t.Fatalf("permissive duplicate won conservative custody merge: %#v", merged)
		}
		if _, eligible := recallResponseEvidenceStatus(merged[0]); eligible {
			t.Fatalf("conservative duplicate remained selectable: %#v", merged[0])
		}
	}
	intelligenceLeft := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: left, AllMerged: mergeRowsAll(left), Literal: mergeRows(left, 1), ResultState: "ready",
	})
	intelligenceRight := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: right, AllMerged: mergeRowsAll(right), Literal: mergeRows(right, 1), ResultState: "ready",
	})
	leftBytes, err := json.Marshal(intelligenceLeft)
	if err != nil {
		t.Fatal(err)
	}
	rightBytes, err := json.Marshal(intelligenceRight)
	if err != nil {
		t.Fatal(err)
	}
	if string(leftBytes) != string(rightBytes) {
		t.Fatalf("equal-score duplicate changed response intelligence digest: left=%s right=%s", leftBytes, rightBytes)
	}
}

func TestMergeRowsUnequalScoreKeepsHighestScoreContentBaseAndConservativeSafety(t *testing.T) {
	highScore := map[string]any{
		"content": "same retained record", "summary": "same retained record",
		"project": "highest-score-project", "file": "highest/record.md", "chunk_id": "highest-chunk",
		"source": "z_high_score", "score": 0.97, "title": "highest-score-title",
		"author": "highest-score-author", "numeric_value": 97,
		"metadata": map[string]any{"winner": "highest-score"},
		"status":   "current", "support": "direct", "action_evidence": map[string]any{
			"tool_ref": "sha256:" + strings.Repeat("a", 64),
		},
	}
	lowerScoreExcluded := map[string]any{
		"content": "same retained record", "summary": "same retained record",
		"project": "lower-score-project", "file": "lower/record.md", "chunk_id": "lower-chunk",
		"source": "a_lower_score", "score": 0.42, "title": "lower-score-title",
		"author": "lower-score-author", "numeric_value": 42,
		"metadata": map[string]any{"winner": "lower-score"},
		"status":   "retired", "lifecycle": "retired", "support": "distractor", "action_evidence": map[string]any{
			"tool_ref": "sha256:" + strings.Repeat("b", 64),
		},
	}
	if rowIdentity(highScore) != rowIdentity(lowerScoreExcluded) {
		t.Fatalf("fixture rows did not share identity: high=%q low=%q", rowIdentity(highScore), rowIdentity(lowerScoreExcluded))
	}
	inputs := []map[string][]map[string]any{
		{"z_high_score": {highScore}, "a_lower_score": {lowerScoreExcluded}},
		{"a_lower_score": {cloneMap(lowerScoreExcluded)}, "z_high_score": {cloneMap(highScore)}},
	}
	var first []byte
	for index, input := range inputs {
		merged := mergeRowsAll(input)
		if len(merged) != 1 {
			t.Fatalf("input %d did not collapse the duplicate: %#v", index, merged)
		}
		row := merged[0]
		for field, want := range highScore {
			if mergeRowSafetyField(field) {
				continue
			}
			if !reflect.DeepEqual(row[field], want) {
				t.Fatalf("input %d field %q came from the lower-score base: got=%#v want=%#v row=%#v", index, field, row[field], want, row)
			}
		}
		if got := parseScore(row); got != 0.97 {
			t.Fatalf("input %d displayed score is not the content-base score: got=%v row=%#v", index, got, row)
		}
		if anyToString(row["status"]) != "retired" || anyToString(row["lifecycle"]) != "retired" ||
			anyToString(row["support"]) != "distractor" || row["action_evidence"] != nil ||
			anyToString(row["merge_conflict"]) != "conservative_duplicate_exclusion" {
			t.Fatalf("input %d did not retain the strongest safety overlay: %#v", index, row)
		}
		serialized, err := json.Marshal(merged)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = serialized
		} else if string(serialized) != string(first) {
			t.Fatalf("unequal-score duplicate merge changed with input order: first=%s next=%s", first, serialized)
		}
	}
}

func TestMergeRowsUnequalScoreKeepsHighestOrdinaryFieldsAndOverlaysEligibleActionSafety(t *testing.T) {
	highScore := map[string]any{
		"content": "same executable procedure", "summary": "same executable procedure",
		"project": "highest-score-project", "file": "highest/procedure.md", "chunk_id": "highest-chunk",
		"source": "z_high_score", "score": 0.96, "title": "highest-score-title",
		"status": "current", "support": "direct",
	}
	actionReceipt := map[string]any{
		"tool_ref": "sha256:" + strings.Repeat("c", 64),
		"parameter_bindings": []any{map[string]any{
			"parameter_ref": "sha256:" + strings.Repeat("d", 64),
			"required":      true, "sensitive": false, "value_state": "bound_redacted",
		}},
		"ordered_steps": []any{map[string]any{"step_ref": "sha256:" + strings.Repeat("e", 64)}},
	}
	lowerScoreAction := map[string]any{
		"content": "same executable procedure", "summary": "same executable procedure",
		"project": "lower-score-project", "file": "lower/procedure.md", "chunk_id": "lower-chunk",
		"source": "a_lower_action", "score": 0.41, "title": "lower-score-title",
		"status": "current", "support": "direct", "action_evidence": actionReceipt,
	}
	if rowIdentity(highScore) != rowIdentity(lowerScoreAction) {
		t.Fatalf("fixture rows did not share identity: high=%q low=%q", rowIdentity(highScore), rowIdentity(lowerScoreAction))
	}
	inputs := []map[string][]map[string]any{
		{"z_high_score": {highScore}, "a_lower_action": {lowerScoreAction}},
		{"a_lower_action": {cloneMap(lowerScoreAction)}, "z_high_score": {cloneMap(highScore)}},
	}
	for index, input := range inputs {
		merged := mergeRowsAll(input)
		if len(merged) != 1 {
			t.Fatalf("input %d did not collapse the duplicate: %#v", index, merged)
		}
		row := merged[0]
		for field, want := range highScore {
			if mergeRowSafetyField(field) {
				continue
			}
			if !reflect.DeepEqual(row[field], want) {
				t.Fatalf("input %d ordinary field %q did not come from the highest-score base: got=%#v want=%#v row=%#v", index, field, row[field], want, row)
			}
		}
		if anyToString(row["source"]) != "z_high_score" || parseScore(row) != 0.96 {
			t.Fatalf("input %d displayed source/score did not stay with the highest-score base: %#v", index, row)
		}
		if !reflect.DeepEqual(row["action_evidence"], actionReceipt) {
			t.Fatalf("input %d eligible lower-score action safety receipt was lost: %#v", index, row)
		}
		if _, eligible := recallResponseEvidenceStatus(row); !eligible {
			t.Fatalf("input %d eligible action receipt became non-supporting: %#v", index, row)
		}
	}
}

func TestMergeRowsActionOverlayRequiresWholePayloadAgreement(t *testing.T) {
	actionPayload := map[string]any{
		"tool_ref": "sha256:" + strings.Repeat("1", 64),
		"parameter_bindings": []any{map[string]any{
			"parameter_ref": "sha256:" + strings.Repeat("2", 64),
			"required":      true, "sensitive": false, "value_state": "bound_redacted",
		}},
		"ordered_steps":      []any{map[string]any{"step_ref": "sha256:" + strings.Repeat("3", 64)}},
		"refusal_conditions": []any{},
	}
	row := func(source string, score float64, field string, payload map[string]any) map[string]any {
		out := map[string]any{
			"content": "same closed action", "summary": "same closed action",
			"project": source + "-project", "file": source + "/action.md", "chunk_id": source + "-chunk",
			"source": source, "score": score, "title": source + "-title",
			"status": "current", "support": "direct",
		}
		if field != "" {
			out[field] = payload
		}
		return out
	}
	clonePayload := func() map[string]any { return cloneJSONMap(actionPayload) }
	toolConflict := clonePayload()
	toolConflict["tool_ref"] = "sha256:" + strings.Repeat("4", 64)
	parameterConflict := clonePayload()
	anyMap(contextPackAnyList(parameterConflict["parameter_bindings"])[0])["parameter_ref"] = "sha256:" + strings.Repeat("5", 64)
	stepsConflict := clonePayload()
	anyMap(contextPackAnyList(stepsConflict["ordered_steps"])[0])["step_ref"] = "sha256:" + strings.Repeat("6", 64)
	partialConflict := map[string]any{"tool_ref": actionPayload["tool_ref"]}
	nestedIdentical := row("nested_identical", 0.65, "action_evidence", clonePayload())
	nestedIdentical["recall_metadata"] = map[string]any{"action": clonePayload()}
	nestedConflict := row("nested_conflict", 0.65, "action_evidence", clonePayload())
	nestedConflict["recall_metadata"] = map[string]any{"action": toolConflict}
	nestedOnly := row("nested_only", 0.60, "", nil)
	nestedOnly["recall_metadata"] = map[string]any{"action": clonePayload()}

	tests := []struct {
		name       string
		candidates []map[string]any
		wantAction bool
		wantHard   bool
	}{
		{
			name: "identical complete aliases",
			candidates: []map[string]any{
				row("a_action", 0.70, "action_evidence", clonePayload()),
				row("b_action", 0.60, "structured_action", clonePayload()),
			},
			wantAction: true,
		},
		{
			name:       "identical nested and top-level aliases",
			candidates: []map[string]any{nestedIdentical},
			wantAction: true,
		},
		{
			name: "identical nested and top-level carriers",
			candidates: []map[string]any{
				row("top_level", 0.70, "action_evidence", clonePayload()),
				nestedOnly,
			},
			wantAction: true,
		},
		{
			name:       "conflicting nested and top-level aliases",
			candidates: []map[string]any{nestedConflict},
		},
		{
			name: "conflicting tool",
			candidates: []map[string]any{
				row("a_action", 0.70, "action_evidence", clonePayload()),
				row("b_action", 0.60, "action_evidence", toolConflict),
			},
		},
		{
			name: "conflicting parameter",
			candidates: []map[string]any{
				row("a_action", 0.70, "action_evidence", clonePayload()),
				row("b_action", 0.60, "action_evidence", parameterConflict),
			},
		},
		{
			name: "conflicting steps",
			candidates: []map[string]any{
				row("a_action", 0.70, "action_evidence", clonePayload()),
				row("b_action", 0.60, "action_evidence", stepsConflict),
			},
		},
		{
			name: "partial payload conflicts with complete payload",
			candidates: []map[string]any{
				row("a_action", 0.70, "action_evidence", clonePayload()),
				row("b_action", 0.60, "action_evidence", partialConflict),
			},
		},
		{
			name: "hard excluded duplicate suppresses agreed action",
			candidates: []map[string]any{
				row("a_action", 0.70, "action_evidence", clonePayload()),
				func() map[string]any {
					excluded := row("b_retired", 0.60, "", nil)
					excluded["status"] = "retired"
					excluded["lifecycle"] = "retired"
					excluded["support"] = "distractor"
					return excluded
				}(),
			},
			wantHard: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			highest := row("z_highest", 0.99, "", nil)
			input := map[string][]map[string]any{"z_highest": {highest}}
			for _, candidate := range test.candidates {
				source := anyToString(candidate["source"])
				input[source] = append(input[source], candidate)
			}
			merged := mergeRowsAll(input)
			if len(merged) != 1 {
				t.Fatalf("duplicate action rows did not collapse: %#v", merged)
			}
			got := merged[0]
			if anyToString(got["source"]) != "z_highest" || parseScore(got) != 0.99 || anyToString(got["title"]) != "z_highest-title" {
				t.Fatalf("action overlay replaced the highest-score ordinary/display winner: %#v", got)
			}
			if test.wantAction {
				if !reflect.DeepEqual(got["action_evidence"], actionPayload) {
					t.Fatalf("byte-equivalent complete action payloads were not retained: %#v", got)
				}
			} else if got["action_evidence"] != nil || got["structured_action"] != nil || got["action"] != nil {
				t.Fatalf("conflicting or excluded action payload did not fail closed: %#v", got)
			}
			if test.wantHard {
				if anyToString(got["status"]) != "retired" || anyToString(got["support"]) != "distractor" {
					t.Fatalf("hard exclusion did not retain lifecycle authority: %#v", got)
				}
			}
		})
	}
}

func TestMergeRowsLowerScoreNestedLifecycleAuthoritySurvivesAndSuppressesAction(t *testing.T) {
	actionPayload := map[string]any{
		"tool_ref": "sha256:" + strings.Repeat("a", 64),
		"parameter_bindings": []any{map[string]any{
			"parameter_ref": "sha256:" + strings.Repeat("b", 64),
			"required":      true, "sensitive": false, "value_state": "bound_redacted",
		}},
	}
	high := map[string]any{
		"content": "same lifecycle record", "summary": "same lifecycle record",
		"project": "high-project", "file": "high/record.md", "chunk_id": "high-chunk",
		"source": "z_high", "score": 0.99, "title": "highest ordinary content",
		"status": "current", "support": "direct", "action_evidence": actionPayload,
	}
	tests := []struct {
		name          string
		wantLifecycle string
		apply         func(map[string]any)
	}{
		{
			name:          "recall metadata temporal retirement",
			wantLifecycle: "retired",
			apply: func(row map[string]any) {
				row["recall_metadata"] = map[string]any{"temporal": map[string]any{"lifecycle": "retired"}}
			},
		},
		{
			name:          "recall metadata root forgotten flag",
			wantLifecycle: "forgotten",
			apply: func(row map[string]any) {
				row["recall_metadata"] = map[string]any{"forgotten": true}
			},
		},
		{
			name:          "temporal evidence forgotten flag",
			wantLifecycle: "forgotten",
			apply: func(row map[string]any) {
				row["temporal_evidence"] = map[string]any{"forgotten": true}
			},
		},
		{
			name:          "mixed optimistic and nested forgotten state",
			wantLifecycle: "forgotten",
			apply: func(row map[string]any) {
				row["recall_metadata"] = map[string]any{
					"state":    "current",
					"temporal": map[string]any{"state": "current", "forgotten": true},
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			low := cloneJSONMap(high)
			low["source"] = "a_low"
			low["score"] = 0.20
			low["title"] = "lower safety carrier"
			delete(low, "action_evidence")
			test.apply(low)
			if rowIdentity(high) != rowIdentity(low) {
				t.Fatalf("lifecycle fixture rows did not share identity: high=%q low=%q", rowIdentity(high), rowIdentity(low))
			}
			merged := mergeRowsAll(map[string][]map[string]any{
				"z_high": {cloneJSONMap(high)},
				"a_low":  {low},
			})
			if len(merged) != 1 {
				t.Fatalf("lifecycle duplicate did not collapse: %#v", merged)
			}
			row := merged[0]
			if anyToString(row["source"]) != "z_high" || parseScore(row) != 0.99 || anyToString(row["title"]) != "highest ordinary content" {
				t.Fatalf("nested lifecycle overlay replaced highest-score ordinary/display fields: %#v", row)
			}
			lifecycle := recallResponseCanonicalLifecycle(row)
			if !lifecycle.hard || lifecycle.canonical != test.wantLifecycle || anyToString(row["lifecycle"]) != test.wantLifecycle {
				t.Fatalf("nested lifecycle authority did not survive merge: lifecycle=%#v row=%#v", lifecycle, row)
			}
			if _, eligible := recallResponseEvidenceStatus(row); eligible {
				t.Fatalf("nested lifecycle row remained supporting: %#v", row)
			}
			if row["action_evidence"] != nil || row["structured_action"] != nil || row["action"] != nil ||
				len(anyMap(anyMap(row["recall_metadata"])["action"])) != 0 || len(recallResponseProjectActionMetadata(row)) != 0 {
				t.Fatalf("nested lifecycle row retained action authority: %#v", row)
			}
			source := map[string]any{
				"retrieval_intent": "action",
				"source_coverage":  map[string]any{"complete": true},
				"context_pack":     map[string]any{"ranked_evidence": []any{row}},
			}
			if recallResponseActionProjectionAllowed(source) || len(anyMap(recallResponseProjectEvidenceMetadata(row)["action"])) != 0 {
				t.Fatalf("nested lifecycle row remained projectable as action: %#v", row)
			}
			if _, err := json.Marshal(merged); err != nil {
				t.Fatalf("merged lifecycle output is not JSON-safe: %v", err)
			}
		})
	}
}

func TestRecallResponseActionProjectionRequiresExactNestedAliasAgreement(t *testing.T) {
	payload := map[string]any{
		"tool_ref": "sha256:" + strings.Repeat("c", 64),
		"parameter_bindings": []any{map[string]any{
			"parameter_ref": "sha256:" + strings.Repeat("d", 64),
			"required":      true, "sensitive": false, "value_state": "bound_redacted",
		}},
	}
	row := map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("a", 24),
		"status":       "current", "support": "direct", "confidence": 0.95,
		"action_evidence": cloneJSONMap(payload),
		"recall_metadata": map[string]any{"action": cloneJSONMap(payload)},
	}
	if len(recallResponseProjectActionMetadata(row)) == 0 || !recallResponseHasStructuredActionEvidence([]any{row}) {
		t.Fatalf("identical nested/top-level action aliases were rejected: %#v", row)
	}
	conflicting := cloneJSONMap(row)
	anyMap(anyMap(conflicting["recall_metadata"])["action"])["tool_ref"] = "sha256:" + strings.Repeat("e", 64)
	if len(recallResponseProjectActionMetadata(conflicting)) != 0 || recallResponseHasStructuredActionEvidence([]any{conflicting}) {
		t.Fatalf("conflicting nested/top-level action aliases remained projectable: %#v", conflicting)
	}
}

func TestMergeRowsRejectsEveryNonFiniteScoreBeforeOrderingAndJSON(t *testing.T) {
	types := []struct {
		name  string
		value any
	}{
		{name: "numeric_nan", value: math.NaN()},
		{name: "numeric_positive_infinity", value: math.Inf(1)},
		{name: "numeric_negative_infinity", value: math.Inf(-1)},
		{name: "numeric_float32_nan", value: float32(math.NaN())},
		{name: "numeric_float32_positive_infinity", value: float32(math.Inf(1))},
		{name: "numeric_float32_negative_infinity", value: float32(math.Inf(-1))},
		{name: "string_nan", value: "NaN"},
		{name: "string_positive_infinity", value: "+Inf"},
		{name: "string_negative_infinity", value: "-Inf"},
		{name: "string_lowercase_nan", value: "nan"},
		{name: "string_positive_infinity_word", value: "+Infinity"},
		{name: "string_negative_infinity_word", value: "-Infinity"},
		{name: "string_overflow_to_infinity", value: "1e10000"},
		{name: "json_number_nan", value: json.Number("NaN")},
		{name: "json_number_positive_infinity", value: json.Number("+Inf")},
		{name: "json_number_negative_infinity", value: json.Number("-Inf")},
		{name: "json_number_lowercase_nan", value: json.Number("nan")},
		{name: "json_number_positive_infinity_word", value: json.Number("+Infinity")},
		{name: "json_number_negative_infinity_word", value: json.Number("-Infinity")},
		{name: "json_number_overflow_to_infinity", value: json.Number("1e10000")},
	}
	aliases := []string{"score", "hybrid_score", "similarity", "confidence"}
	valid := map[string]any{
		"content": "finite score record", "summary": "finite score record",
		"source": "finite", "score": 0.75, "status": "current",
	}
	invalid := make([]map[string]any, 0, len(types)*len(aliases)+1)
	for _, alias := range aliases {
		for _, scoreType := range types {
			row := map[string]any{
				"content": "invalid " + alias + " " + scoreType.name,
				"summary": "invalid " + alias + " " + scoreType.name,
				"source":  alias + "_" + scoreType.name,
				"status":  "current",
				alias:     scoreType.value,
			}
			if score, ok := parseFiniteScore(row); ok || score != 0 || math.IsNaN(parseScore(row)) || math.IsInf(parseScore(row), 0) {
				t.Fatalf("non-finite %s/%s was accepted: score=%v valid=%t", alias, scoreType.name, score, ok)
			}
			invalid = append(invalid, row)
		}
	}
	hiddenAlias := cloneJSONMap(valid)
	hiddenAlias["source"] = "finite_primary_nonfinite_secondary"
	hiddenAlias["confidence"] = math.NaN()
	invalid = append(invalid, hiddenAlias)

	forward := append([]map[string]any{cloneJSONMap(valid)}, invalid...)
	reverse := make([]map[string]any, 0, len(forward))
	for index := len(invalid) - 1; index >= 0; index-- {
		reverse = append(reverse, invalid[index])
	}
	reverse = append(reverse, cloneJSONMap(valid))
	inputs := []map[string][]map[string]any{
		{"mixed": forward},
		{"mixed": reverse},
	}
	var first []byte
	for index, input := range inputs {
		merged := mergeRowsAll(input)
		if len(merged) != 1 || anyToString(merged[0]["source"]) != "finite" || parseScore(merged[0]) != 0.75 {
			t.Fatalf("input %d retained or ordered a non-finite row: %#v", index, merged)
		}
		serialized, err := json.Marshal(merged)
		if err != nil {
			t.Fatalf("input %d emitted non-JSON score material: %v", index, err)
		}
		if index == 0 {
			first = serialized
		} else if string(serialized) != string(first) {
			t.Fatalf("finite output changed with non-finite input order: first=%s next=%s", first, serialized)
		}
	}
}

func TestSearchIntelligenceIdentityPrefersObservedContentOverClaimedDigest(t *testing.T) {
	claimed := "sha256:" + strings.Repeat("d", 64)
	left := map[string]any{"content_ref": claimed, "content": "first full body", "summary": "shared summary"}
	right := map[string]any{"content_ref": claimed, "content": "second full body", "summary": "shared summary"}
	if searchIntelligenceCandidateIdentity(left).CandidateRef == searchIntelligenceCandidateIdentity(right).CandidateRef {
		t.Fatal("claimed content digest collapsed distinct server-observed content")
	}
}

func TestSearchIntelligenceSparseIdentityPreservesDistinctSameFileLocators(t *testing.T) {
	claimed := "sha256:" + strings.Repeat("a", 64)
	first := map[string]any{
		"content_ref": claimed, "project": "alpha", "file": "notes/release.md",
		"chunk_id": "chunk-1", "line_start": 10, "line_end": 20, "score": 0.9,
	}
	second := map[string]any{
		"content_ref": claimed, "project": "alpha", "file": "notes/release.md",
		"chunk_id": "chunk-2", "line_start": 21, "line_end": 30, "score": 0.8,
	}
	firstIdentity := searchIntelligenceCandidateIdentity(first)
	secondIdentity := searchIntelligenceCandidateIdentity(second)
	if firstIdentity.CandidateRef == secondIdentity.CandidateRef {
		t.Fatalf("sparse same-file passages collapsed despite distinct locators: first=%#v second=%#v", firstIdentity, secondIdentity)
	}
	merged := mergeRowsAll(map[string][]map[string]any{"qdrant": {first, second, cloneMap(first)}})
	if len(merged) != 2 {
		t.Fatalf("sparse locator identity did not preserve two chunks and collapse one exact replay: %#v", merged)
	}
}

func TestSearchIntelligenceShadowFusionIsBoundedAndDoesNotChangeLiteralOrdering(t *testing.T) {
	rowsBySource := map[string][]map[string]any{
		"qdrant": {
			{"content": "shared verified release evidence", "summary": "shared verified release evidence", "project": "alpha", "file": "notes/release.md", "source": "qdrant", "score": 0.61},
			{"content": "native winner", "summary": "native winner", "project": "alpha", "file": "notes/native.md", "source": "qdrant", "score": 0.99},
		},
		"topic_rollups": {
			{"content": "shared verified release evidence", "summary": "shared verified release evidence", "project": "archive", "file": "history/release.md", "source": "topic_rollups", "score": 0.54},
			{"content": "single-source history", "summary": "single-source history", "project": "archive", "file": "history/other.md", "source": "topic_rollups", "score": 0.52},
		},
	}
	allMerged := mergeRowsAll(rowsBySource)
	if len(allMerged) != 3 {
		t.Fatalf("true cross-store duplicate did not collapse in the literal pipeline: %#v", allMerged)
	}
	literal := truncateMergedRows(allMerged, 2)
	if got := anyToString(literal[0]["summary"]); got != "native winner" {
		t.Fatalf("fixture does not establish current native order: %#v", literal)
	}

	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource,
		AllMerged:    allMerged,
		Literal:      literal,
		ResultState:  "ready",
	})
	literalStatus := anyMap(intelligence["literal_results"])
	if anyToString(literalStatus["ordering"]) != "native_score_desc_preserved" || anyToInt(literalStatus["returned_count"], 0) != 2 {
		t.Fatalf("literal result status does not prove production order retention: %#v", literalStatus)
	}
	frontier := anyMap(intelligence["decision_frontier"])
	if anyToString(frontier["status"]) != "shadow_only" || anyToString(anyMap(frontier["fusion"])["method"]) != "weighted_reciprocal_rank_fusion" {
		t.Fatalf("shadow fusion contract missing: %#v", frontier)
	}
	candidates := contextPackAnyList(frontier["candidates"])
	if len(candidates) != 3 {
		t.Fatalf("expected cross-store duplicate collapse into three bounded candidates, got %#v", candidates)
	}
	first := anyMap(candidates[0])
	features := anyMap(first["features"])
	if anyToInt(features["source_count"], 0) != 2 || !containsAnyInList(contextPackAnyList(first["reasons"]), "cross_store_content_duplicate_collapsed") {
		t.Fatalf("shared evidence did not lead the shadow frontier: %#v", first)
	}
	raw, err := json.Marshal(intelligence)
	if err != nil {
		t.Fatalf("marshal search intelligence: %v", err)
	}
	for _, forbidden := range []string{"shared verified release evidence", "notes/release.md", "history/release.md"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("new receipt leaked raw content or path %q: %s", forbidden, raw)
		}
	}
	for i := 0; i < 5; i++ {
		rebuilt, err := json.Marshal(buildSearchIntelligence(searchIntelligenceInput{
			RowsBySource: rowsBySource,
			AllMerged:    allMerged,
			Literal:      literal,
			ResultState:  "ready",
		}))
		if err != nil {
			t.Fatalf("marshal rebuilt intelligence: %v", err)
		}
		if string(rebuilt) != string(raw) {
			t.Fatalf("weighted-RRF shadow receipt was not deterministic: first=%s rebuilt=%s", raw, rebuilt)
		}
	}
}

func TestSearchIntelligenceDecisionImpactRanksCurrentAlignedEvidenceWithoutSelfAwardingVerification(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	current := map[string]any{
		"content": "Release approval requires independent verification before rollout.",
		"summary": "current release verification evidence", "project": "alpha", "file": "notes/current.md",
		"source": "topic_rollups", "score": 0.61, "timestamp": asOf.Add(-24 * time.Hour).Format(time.RFC3339Nano),
		"projection_authority": "current_event", "source_owner": "go_native",
		"candidate_utility_verification": map[string]any{
			"independently_verified": true, "verification_status": "verified",
			"evidence_digest": "sha256:" + strings.Repeat("a", 64), "verifier_id": "verifier-a",
		},
		"memory_trust_assessment": map[string]any{
			"trust_label": "bounded", "quarantine": map[string]any{"quarantined": false},
			"provenance": map[string]any{"server_observed": true},
		},
	}
	stale := map[string]any{
		"content": "Release approval paused without supporting verification.",
		"summary": "stale unsupported release evidence", "project": "alpha", "file": "notes/stale.md",
		"source": "qdrant", "score": 0.99, "timestamp": asOf.AddDate(-2, 0, 0).Format(time.RFC3339Nano),
	}
	rowsBySource := map[string][]map[string]any{"qdrant": {stale}, "topic_rollups": {current}}
	allMerged := mergeRowsAll(rowsBySource)
	literal := truncateMergedRows(allMerged, 2)
	if anyToString(literal[0]["summary"]) != "stale unsupported release evidence" {
		t.Fatalf("fixture does not establish native ordering: %#v", literal)
	}

	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: literal, ResultState: "ready",
		Query: "release independent verification", RetrievalIntent: "release", AsOf: asOf,
	})
	frontier := anyMap(intelligence["decision_frontier"])
	candidates := contextPackAnyList(frontier["candidates"])
	if len(candidates) != 2 {
		t.Fatalf("candidate count=%d, want 2: %#v", len(candidates), frontier)
	}
	currentRef := searchIntelligenceCandidateIdentity(current).CandidateRef
	if got := anyToString(anyMap(anyMap(candidates[0])["refs"])["candidate_ref"]); got != currentRef {
		t.Fatalf("current verified evidence did not outrank stale unsupported evidence: got=%q want=%q frontier=%#v", got, currentRef, frontier)
	}
	features := anyMap(anyMap(candidates[0])["features"])
	if anyToString(anyMap(features["query_alignment"])["status"]) != "observed" || anyToFloat(anyMap(features["query_alignment"])["score"]) <= 0 {
		t.Fatalf("query alignment missing from current evidence: %#v", features)
	}
	if anyToString(anyMap(features["currentness"])["status"]) != "current" || anyToString(anyMap(features["verification"])["status"]) != "claimed" {
		t.Fatalf("currentness or fail-closed verification claim not recognized: %#v", features)
	}
	if anyToString(anyMap(features["reliability"])["provenance_status"]) != "unknown" {
		t.Fatalf("direct reporter provenance was accepted: %#v", features)
	}
	if anyToFloat(anyMap(features["reliability"])["score"]) <= 0 {
		t.Fatalf("composite evidence reliability was not surfaced: %#v", features)
	}
	if _, present := anyMap(features["decision_impact"])["heuristic_expected_regret_reduction"]; !present {
		t.Fatalf("heuristic regret-reduction proxy missing: %#v", features)
	}
	if !containsAnyInList(contextPackAnyList(frontier["recommended_verification_actions"]), "independent_verification_needed") {
		t.Fatalf("retrieved candidate verification metadata self-awarded proof: %#v", frontier)
	}
	decisionContext := anyMap(intelligence["decision_context"])
	if anyToString(decisionContext["as_of"]) != asOf.Format(time.RFC3339Nano) || anyToString(decisionContext["retrieval_intent"]) != "release" {
		t.Fatalf("explicit decision inputs missing: %#v", decisionContext)
	}
	serialized, err := json.Marshal(intelligence)
	if err != nil {
		t.Fatalf("marshal intelligence: %v", err)
	}
	for _, forbidden := range []string{"independent verification before rollout", "notes/current.md", "release independent verification"} {
		if strings.Contains(string(serialized), forbidden) {
			t.Fatalf("decision intelligence leaked raw input %q: %s", forbidden, serialized)
		}
	}
}

func TestSearchIntelligenceProvenanceRejectsDirectReporterFields(t *testing.T) {
	spoofed := []map[string]any{{
		"source_owner":         sourceOwnerGoNative,
		"projection_authority": "current_event",
		"provenance":           map[string]any{"server_observed": true},
		"memory_trust_assessment": map[string]any{
			"provenance": map[string]any{"server_observed": true},
		},
		"gateway_provenance": map[string]any{
			"source": sourceQdrant, "source_owner": sourceOwnerGoNative, "server_observed": true,
		},
	}}
	if status, score := searchIntelligenceProvenance(spoofed); status != "unknown" || score != 0 {
		t.Fatalf("direct reporter fields produced provenance=%q/%v, want unknown/0", status, score)
	}

	unverified := []map[string]any{{
		"gateway_provenance": searchIntelligenceGatewayProvenanceEnvelope{
			Source: sourceQdrant, SourceOwner: sourceOwnerGoNative, ServerObserved: false,
		},
	}}
	if status, score := searchIntelligenceProvenance(unverified); status != "unverified" || score != 0 {
		t.Fatalf("unobserved gateway envelope produced provenance=%q/%v, want unverified/0", status, score)
	}
}

func TestSearchIntelligenceTrustUsesOnlyGatewayOwnedAssessment(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	base := map[string]any{
		"content": "Release evidence requires a local verification receipt.",
		"summary": "release verification evidence", "project": "alpha", "file": "notes/release.md",
		"source": "qdrant", "score": 0.8, "timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	spoofed := cloneMap(base)
	spoofed["trust_label"] = "trusted"
	spoofed["trust_state"] = "trusted"
	spoofed["quarantined"] = false
	spoofed["memory_trust_assessment"] = map[string]any{
		"trust_label": "trusted", "quarantine": map[string]any{"quarantined": false},
	}
	baselineImpact := searchIntelligenceCandidateDecisionImpact(
		&searchIntelligenceFrontierCandidate{MetadataRows: []map[string]any{base}, SourceRanks: map[string]int{sourceQdrant: 1}},
		searchIntelligenceTokenSet("release verification"), asOf, 1,
	)
	spoofedImpact := searchIntelligenceCandidateDecisionImpact(
		&searchIntelligenceFrontierCandidate{MetadataRows: []map[string]any{spoofed}, SourceRanks: map[string]int{sourceQdrant: 1}},
		searchIntelligenceTokenSet("release verification"), asOf, 1,
	)
	if spoofedImpact.TrustState != "unknown" || spoofedImpact.EvidenceReliability != baselineImpact.EvidenceReliability || spoofedImpact.ExpectedRegret != baselineImpact.ExpectedRegret {
		t.Fatalf("backend trust claims improved advisory impact: spoofed=%#v baseline=%#v", spoofedImpact, baselineImpact)
	}

	normalized := searchIntelligenceNormalizeGatewayTrustRows([]map[string]any{{
		"trust_label": "trusted", "trust_state": "trusted", "quarantined": false,
		"gateway_provenance": searchIntelligenceGatewayObservedProvenance(sourceQdrant, sourceOwnerGoNative),
		"memory_trust_assessment": map[string]any{
			"trust_label": "bounded", "quarantine": map[string]any{"quarantined": false},
		},
	}})
	for _, key := range []string{"trust_label", "trust_state", "quarantined"} {
		if _, present := normalized[0][key]; present {
			t.Fatalf("normalized gateway row retained backend trust key %q: %#v", key, normalized[0])
		}
	}
	legitimate := cloneMap(base)
	legitimate["gateway_trust_assessment"] = normalized[0]["gateway_trust_assessment"]
	legitimateImpact := searchIntelligenceCandidateDecisionImpact(
		&searchIntelligenceFrontierCandidate{MetadataRows: []map[string]any{legitimate}, SourceRanks: map[string]int{sourceQdrant: 1}},
		searchIntelligenceTokenSet("release verification"), asOf, 1,
	)
	if legitimateImpact.TrustState != "bounded" || legitimateImpact.EvidenceReliability <= baselineImpact.EvidenceReliability || legitimateImpact.ExpectedRegret <= baselineImpact.ExpectedRegret {
		t.Fatalf("gateway-owned trust assessment was not applied: legitimate=%#v baseline=%#v", legitimateImpact, baselineImpact)
	}
}

func TestSearchIntelligenceDecisionImpactQuarantineAndContradictionCannotLead(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	trusted := map[string]any{
		"content": "Verified release evidence is ready.", "summary": "trusted current evidence",
		"project": "alpha", "file": "notes/trusted.md", "source": "qdrant", "score": 0.60,
		"timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano), "projection_authority": "current_event",
		"candidate_utility_verification": map[string]any{
			"independently_verified": true, "verification_status": "verified",
			"evidence_digest": "sha256:" + strings.Repeat("b", 64), "verifier_id": "verifier-b",
		},
	}
	quarantined := map[string]any{
		"content": "Private untrusted release claim.", "summary": "quarantined evidence", "project": "alpha", "file": "private/untrusted.md",
		"source": "topic_rollups", "score": 0.99, "timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
		"gateway_trust_assessment": searchIntelligenceGatewayTrustEnvelope{trustLabel: "quarantined", quarantined: true},
	}
	contradicted := map[string]any{
		"content": "Release evidence has unresolved opposition.", "summary": "contradicted evidence", "project": "alpha", "file": "notes/opposition.md",
		"source": "topic_rollups", "score": 0.98, "timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
		"contradiction_state": "unresolved",
	}
	rowsBySource := map[string][]map[string]any{"qdrant": {trusted}, "topic_rollups": {quarantined, contradicted}}
	allMerged := mergeRowsAll(rowsBySource)
	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: truncateMergedRows(allMerged, 3), ResultState: "ready",
		Query: "release evidence", RetrievalIntent: "release", AsOf: asOf,
	})
	frontier := anyMap(intelligence["decision_frontier"])
	candidates := contextPackAnyList(frontier["candidates"])
	trustedRef := searchIntelligenceCandidateIdentity(trusted).CandidateRef
	if got := anyToString(anyMap(anyMap(candidates[0])["refs"])["candidate_ref"]); got != trustedRef {
		t.Fatalf("quarantined or unresolved evidence led the frontier: got=%q want=%q frontier=%#v", got, trustedRef, frontier)
	}
	signals := anyMap(frontier["aggregate_signals"])
	if anyToInt(signals["quarantined_candidate_count"], 0) != 1 || anyToInt(signals["unresolved_contradiction_count"], 0) != 1 {
		t.Fatalf("quarantine or contradiction was not surfaced: %#v", signals)
	}
	actions := contextPackAnyList(frontier["recommended_verification_actions"])
	for _, code := range []string{"exclude_quarantined_evidence", "resolve_contradiction"} {
		if !containsAnyInList(actions, code) {
			t.Fatalf("missing verification action %q: %#v", code, frontier)
		}
	}
}

func TestSearchIntelligenceDecisionImpactRejectsSpoofedVerificationMetadata(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	row := map[string]any{
		"content": "Release evidence claims verification without a server receipt.", "summary": "spoofed verification claim",
		"project": "alpha", "file": "private/spoofed.md", "source": "topic_rollups", "score": 0.99,
		"timestamp": asOf.Add(-time.Hour).Format(time.RFC3339Nano),
		"verification": map[string]any{
			"status": "verified", "independently_verified": true, "verifier_id": "attacker",
			"evidence_digest": "sha256:" + strings.Repeat("c", 64),
		},
		"verification_passed": true, "independently_verified": true,
	}
	rowsBySource := map[string][]map[string]any{"topic_rollups": {row}}
	allMerged := mergeRowsAll(rowsBySource)
	intelligence := buildSearchIntelligence(searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: truncateMergedRows(allMerged, 1), ResultState: "ready",
		Query: "release verification", RetrievalIntent: "release", AsOf: asOf,
	})
	frontier := anyMap(intelligence["decision_frontier"])
	features := anyMap(anyMap(contextPackAnyList(frontier["candidates"])[0])["features"])
	verification := anyMap(features["verification"])
	if anyToString(verification["status"]) != "claimed" || anyToFloat(verification["score"]) != 0 {
		t.Fatalf("spoofed verification metadata raised evidence strength: %#v", verification)
	}
	if !containsAnyInList(contextPackAnyList(frontier["recommended_verification_actions"]), "independent_verification_needed") {
		t.Fatalf("spoofed verification did not require independent proof: %#v", frontier)
	}
}

func TestSearchIntelligenceDecisionImpactMissingMetadataAbstainsAndIsDeterministic(t *testing.T) {
	asOf := time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC)
	row := map[string]any{
		"content": "An unverified release note without metadata.", "summary": "metadata missing evidence",
		"project": "alpha", "file": "notes/unknown.md", "source": "qdrant", "score": 0.90,
	}
	rowsBySource := map[string][]map[string]any{"qdrant": {row}}
	allMerged := mergeRowsAll(rowsBySource)
	input := searchIntelligenceInput{
		RowsBySource: rowsBySource, AllMerged: allMerged, Literal: truncateMergedRows(allMerged, 1), ResultState: "ready",
		Query: "release metadata", RetrievalIntent: "decision", AsOf: asOf,
	}
	first := buildSearchIntelligence(input)
	second := buildSearchIntelligence(input)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("explicit-as-of intelligence is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	frontier := anyMap(first["decision_frontier"])
	if anyToString(frontier["recommendation_state"]) != "verify_before_action" {
		t.Fatalf("missing metadata did not abstain from an action recommendation: %#v", frontier)
	}
	features := anyMap(anyMap(contextPackAnyList(frontier["candidates"])[0])["features"])
	for _, feature := range []string{"currentness", "verification", "acquisition_cost"} {
		if anyToString(anyMap(features[feature])["status"]) != "unknown" {
			t.Fatalf("missing %s was treated as favorable: %#v", feature, features)
		}
	}
	if anyToString(anyMap(features["reliability"])["provenance_status"]) != "unknown" {
		t.Fatalf("missing provenance was treated as observed: %#v", features)
	}
	actions := contextPackAnyList(frontier["recommended_verification_actions"])
	for _, code := range []string{"independent_verification_needed", "timestamp_needed", "provenance_needed", "acquisition_cost_unknown"} {
		if !containsAnyInList(actions, code) {
			t.Fatalf("missing abstention action %q: %#v", code, frontier)
		}
	}
}
