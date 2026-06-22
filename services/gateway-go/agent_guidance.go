package main

import (
	"fmt"
	"sort"
	"strings"
)

type agentEvidenceGuidanceInput struct {
	Query          string
	Surface        string
	SourceCoverage map[string]any
	RankedEvidence []any
	Patterns       []any
	MaxThemes      int
	MaxRiskMarkers int
	MaxLinks       int
	MaxHints       int
}

func buildAgentEvidenceGuidance(input agentEvidenceGuidanceInput) map[string]any {
	maxThemes := clampInt(input.MaxThemes, 1, 8)
	maxRiskMarkers := clampInt(input.MaxRiskMarkers, 1, 8)
	maxLinks := clampInt(input.MaxLinks, 0, 8)
	maxHints := clampInt(input.MaxHints, 1, 10)

	themes := agentGuidanceThemes(input.RankedEvidence, input.Patterns, maxThemes)
	riskMarkers := agentGuidanceRiskMarkers(input.RankedEvidence, input.Patterns, maxRiskMarkers)
	candidateLinks := agentGuidanceCandidateLinks(input.RankedEvidence, themes, maxLinks)
	missingEvidence := agentGuidanceMissingEvidence(input.Query, input.SourceCoverage, len(input.RankedEvidence), len(input.Patterns))
	promptHints := agentGuidancePromptHints(input.Query, themes, riskMarkers, candidateLinks, missingEvidence, maxHints)

	return map[string]any{
		"schema_id":        "contextlattice_agent_guidance.v1",
		"source":           "deterministic_evidence_analysis",
		"authoritative":    false,
		"surface":          strings.TrimSpace(input.Surface),
		"intended_use":     "attention_scaffolding_for_agent_or_llm_prompting_not_final_claims",
		"not_dream_mode":   true,
		"themes":           themes,
		"risk_markers":     riskMarkers,
		"candidate_links":  candidateLinks,
		"missing_evidence": missingEvidence,
		"prompt_hints":     promptHints,
		"controls": map[string]any{
			"max_themes":       maxThemes,
			"max_risk_markers": maxRiskMarkers,
			"max_links":        maxLinks,
			"max_hints":        maxHints,
			"requires_llm":     false,
			"valid_as_dream":   false,
		},
	}
}

func agentGuidanceThemes(rankedEvidence []any, patterns []any, maxItems int) []any {
	type themeScore struct {
		Theme       string
		Score       float64
		Count       int
		EvidenceIDs []any
		Files       []any
		Sources     []any
	}
	scores := map[string]*themeScore{}
	addSignal := func(theme string, score float64, evidenceID string, fileName string, source string) {
		theme = agentGuidanceTheme(theme, fileName, source)
		if theme == "" {
			return
		}
		key := strings.ToLower(theme)
		row := scores[key]
		if row == nil {
			row = &themeScore{Theme: theme}
			scores[key] = row
		}
		row.Score += score
		row.Count += 1
		if evidenceID != "" && len(row.EvidenceIDs) < 4 {
			row.EvidenceIDs = appendUniqueAnyString(row.EvidenceIDs, evidenceID, 4)
		}
		if fileName != "" && len(row.Files) < 4 {
			row.Files = appendUniqueAnyString(row.Files, fileName, 4)
		}
		if source != "" && len(row.Sources) < 4 {
			row.Sources = appendUniqueAnyString(row.Sources, source, 4)
		}
	}
	for _, raw := range rankedEvidence {
		row := anyMap(raw)
		text := firstNonEmptyStrings(anyToString(row["text"]), anyToString(row["summary"]), anyToString(row["claim"]))
		rank := anyToInt(row["rank"], 0)
		score := anyToFloat(row["score"])
		if score == 0 && rank > 0 {
			score = float64(100 - minInt(rank, 80))
		}
		if score == 0 {
			score = 50
		}
		addSignal(text, score, agentGuidanceEvidenceID(row), anyToString(row["file"]), anyToString(row["source"]))
	}
	for _, raw := range patterns {
		row := anyMap(raw)
		addSignal(firstNonEmptyStrings(anyToString(row["category"]), anyToString(row["id"]), anyToString(row["agent_guidance"])), 72, anyToString(row["id"]), "", "review")
		for _, evidence := range contextPackAnyList(row["evidence"]) {
			addSignal(anyToString(evidence), 58, anyToString(row["id"]), "", "review")
		}
	}
	rows := make([]*themeScore, 0, len(scores))
	for _, row := range scores {
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score == rows[j].Score {
			return rows[i].Theme < rows[j].Theme
		}
		return rows[i].Score > rows[j].Score
	})
	limit := minInt(maxItems, len(rows))
	out := make([]any, 0, limit)
	for idx := 0; idx < limit; idx++ {
		row := rows[idx]
		out = append(out, map[string]any{
			"theme":        clipUTF8Bytes(row.Theme, 120),
			"rank":         idx + 1,
			"score":        roundFloat(row.Score, 3),
			"signal_count": row.Count,
			"evidence_ids": row.EvidenceIDs,
			"files":        row.Files,
			"sources":      row.Sources,
		})
	}
	return out
}

func agentGuidanceRiskMarkers(rankedEvidence []any, patterns []any, maxItems int) []any {
	type riskScore struct {
		Marker      string
		Count       int
		Score       float64
		EvidenceIDs []any
		Examples    []any
	}
	markers := map[string]string{
		"overflow":               "provider/context overflow risk",
		"context overflow":       "context overflow risk",
		"context length":         "context length risk",
		"context budget":         "context budget risk",
		"array_above_max_length": "provider array limit risk",
		"timeout":                "timeout or slow-source risk",
		"failed":                 "recent failure signal",
		"failure":                "recent failure signal",
		"blocked":                "blocked execution signal",
		"degraded":               "degraded source or runtime signal",
		"rollback":               "rollback or revert risk",
		"regression":             "regression risk",
		"stale":                  "stale-context risk",
		"secret":                 "secret-handling risk",
		"unsafe":                 "safety or trust boundary risk",
		"unavailable":            "unavailable dependency risk",
	}
	scores := map[string]*riskScore{}
	addRisk := func(marker string, description string, evidenceID string, example string, weight float64) {
		marker = strings.TrimSpace(strings.ToLower(marker))
		if marker == "" {
			return
		}
		row := scores[marker]
		if row == nil {
			row = &riskScore{Marker: description}
			scores[marker] = row
		}
		row.Count += 1
		row.Score += weight
		if evidenceID != "" {
			row.EvidenceIDs = appendUniqueAnyString(row.EvidenceIDs, evidenceID, 4)
		}
		if example != "" && len(row.Examples) < 3 {
			row.Examples = appendUniqueAnyString(row.Examples, clipUTF8Bytes(example, 180), 3)
		}
	}
	for _, raw := range rankedEvidence {
		row := anyMap(raw)
		text := strings.ToLower(strings.Join([]string{
			anyToString(row["kind"]),
			anyToString(row["reason"]),
			anyToString(row["text"]),
			anyToString(row["summary"]),
		}, " "))
		for marker, description := range markers {
			if strings.Contains(text, marker) {
				addRisk(marker, description, agentGuidanceEvidenceID(row), anyToString(row["text"]), 80+anyToFloat(row["score"])/10)
			}
		}
	}
	for _, raw := range patterns {
		row := anyMap(raw)
		severity := strings.TrimSpace(anyToString(row["severity"]))
		if severityRank(severity) >= severityRank("medium") {
			addRisk(anyToString(row["category"]), "review pattern: "+anyToString(row["category"]), anyToString(row["id"]), anyToString(row["agent_guidance"]), float64(severityRank(severity))*55)
		}
	}
	rows := make([]*riskScore, 0, len(scores))
	for _, row := range scores {
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Score == rows[j].Score {
			return rows[i].Marker < rows[j].Marker
		}
		return rows[i].Score > rows[j].Score
	})
	limit := minInt(maxItems, len(rows))
	out := make([]any, 0, limit)
	for idx := 0; idx < limit; idx++ {
		row := rows[idx]
		severity := "low"
		if row.Count >= 3 || row.Score >= 180 {
			severity = "high"
		} else if row.Count >= 2 || row.Score >= 105 {
			severity = "medium"
		}
		out = append(out, map[string]any{
			"marker":       clipUTF8Bytes(row.Marker, 140),
			"severity":     severity,
			"signal_count": row.Count,
			"evidence_ids": row.EvidenceIDs,
			"examples":     row.Examples,
			"prompt_hint":  clipUTF8Bytes("Before acting, verify the cited evidence that triggered "+row.Marker+".", 260),
		})
	}
	return out
}

func agentGuidanceCandidateLinks(rankedEvidence []any, themes []any, maxItems int) []any {
	if maxItems <= 0 || len(rankedEvidence) < 2 {
		return []any{}
	}
	themeByEvidence := map[string]string{}
	for _, raw := range themes {
		row := anyMap(raw)
		theme := anyToString(row["theme"])
		for _, evidenceID := range contextPackAnyList(row["evidence_ids"]) {
			themeByEvidence[anyToString(evidenceID)] = theme
		}
	}
	limit := minInt(maxItems, len(rankedEvidence)-1)
	out := make([]any, 0, limit)
	for idx := 0; idx < len(rankedEvidence)-1 && len(out) < limit; idx++ {
		left := anyMap(rankedEvidence[idx])
		right := anyMap(rankedEvidence[idx+1])
		leftID := agentGuidanceEvidenceID(left)
		rightID := agentGuidanceEvidenceID(right)
		leftTheme := firstNonEmptyStrings(themeByEvidence[leftID], agentGuidanceTheme(anyToString(left["text"]), anyToString(left["file"]), anyToString(left["source"])))
		rightTheme := firstNonEmptyStrings(themeByEvidence[rightID], agentGuidanceTheme(anyToString(right["text"]), anyToString(right["file"]), anyToString(right["source"])))
		if leftTheme == "" || rightTheme == "" || strings.EqualFold(leftTheme, rightTheme) {
			continue
		}
		out = append(out, map[string]any{
			"id":              fmt.Sprintf("link_%d", len(out)+1),
			"relationship":    "candidate_attention_link",
			"authoritative":   false,
			"left_evidence":   leftID,
			"right_evidence":  rightID,
			"left_theme":      clipUTF8Bytes(leftTheme, 120),
			"right_theme":     clipUTF8Bytes(rightTheme, 120),
			"reason":          clipUTF8Bytes("Adjacent ranked evidence may be useful to inspect together before synthesis.", 220),
			"prompt_hint":     clipUTF8Bytes("Consider whether "+leftTheme+" constrains or validates "+rightTheme+"; do not treat the link as a proven relationship without inspection.", 360),
			"valid_as_claim":  false,
			"valid_as_dream":  false,
			"requires_review": true,
		})
	}
	return out
}

func agentGuidanceMissingEvidence(query string, sourceCoverage map[string]any, evidenceCount int, patternCount int) []any {
	out := []any{}
	if evidenceCount == 0 && patternCount == 0 {
		out = append(out, map[string]any{
			"type":        "thin_context",
			"detail":      "No ranked evidence or review patterns were available for deterministic guidance.",
			"next_action": "Run a broader context pack or inspect current files before making claims.",
		})
	}
	if sourceCoverage == nil {
		return out
	}
	if !anyToBool(sourceCoverage["complete"]) {
		pending := anyToStringList(firstNonEmptyAny(sourceCoverage["pending"], sourceCoverage["warming"]), 6)
		failed := anyToStringList(firstNonEmptyAny(sourceCoverage["failed"], sourceCoverage["timed_out"]), 6)
		detail := "Source coverage is incomplete."
		if len(pending) > 0 {
			detail = "Pending or warming sources: " + strings.Join(pending, ", ")
		} else if len(failed) > 0 {
			detail = "Failed or timed-out sources: " + strings.Join(failed, ", ")
		}
		out = append(out, map[string]any{
			"type":        "incomplete_source_coverage",
			"detail":      clipUTF8Bytes(detail, 260),
			"next_action": "Treat guidance as partial and retry slow sources or inspect cited files before final decisions.",
		})
	}
	if strings.TrimSpace(query) == "" {
		out = append(out, map[string]any{
			"type":        "missing_query",
			"detail":      "No explicit task query was present for guidance ranking.",
			"next_action": "Attach the current objective/query to the next context-pack or preflight request.",
		})
	}
	return out
}

func agentGuidancePromptHints(query string, themes []any, riskMarkers []any, candidateLinks []any, missingEvidence []any, maxItems int) []any {
	out := []any{}
	if len(missingEvidence) > 0 {
		out = append(out, "Resolve missing or incomplete evidence before making final claims.")
	}
	if len(riskMarkers) > 0 {
		row := anyMap(riskMarkers[0])
		out = append(out, "Check risk marker first: "+anyToString(row["marker"])+".")
	}
	if len(candidateLinks) > 0 {
		row := anyMap(candidateLinks[0])
		out = append(out, anyToString(row["prompt_hint"]))
	}
	if len(themes) > 0 {
		row := anyMap(themes[0])
		out = append(out, "Use the highest-ranked theme as an attention anchor: "+anyToString(row["theme"])+".")
	}
	if strings.TrimSpace(query) != "" {
		out = append(out, "Keep guidance subordinate to the actual task: "+clipUTF8Bytes(query, 180)+".")
	}
	out = append(out, "These hints are deterministic scaffolding, not final claims or Dream Mode output.")
	if len(out) > maxItems {
		out = out[:maxItems]
	}
	return out
}

func agentGuidanceTheme(text string, fileName string, source string) string {
	terms := queryTerms(strings.ToLower(text), 5)
	filtered := make([]string, 0, len(terms))
	stop := map[string]bool{
		"the": true, "and": true, "for": true, "with": true, "that": true, "this": true, "from": true, "into": true,
		"contextlattice": true, "memory": true, "agent": true, "agents": true, "mode": true, "response": true,
	}
	for _, term := range terms {
		term = strings.TrimSpace(strings.ToLower(term))
		if len(term) < 4 || stop[term] {
			continue
		}
		filtered = append(filtered, term)
		if len(filtered) >= 3 {
			break
		}
	}
	if len(filtered) > 0 {
		return strings.Join(filtered, " ")
	}
	if strings.TrimSpace(fileName) != "" {
		return strings.TrimSpace(fileName)
	}
	if strings.TrimSpace(source) != "" {
		return strings.TrimSpace(source)
	}
	return ""
}

func agentGuidanceEvidenceID(row map[string]any) string {
	if row == nil {
		return ""
	}
	if id := strings.TrimSpace(anyToString(row["id"])); id != "" {
		return id
	}
	if rank := anyToInt(row["rank"], 0); rank > 0 {
		return fmt.Sprintf("rank_%d", rank)
	}
	parts := []string{}
	for _, key := range []string{"kind", "file", "source"} {
		if value := strings.TrimSpace(anyToString(row[key])); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, ":")
}

func appendUniqueAnyString(items []any, value string, maxItems int) []any {
	value = strings.TrimSpace(value)
	if value == "" {
		return items
	}
	for _, item := range items {
		if strings.EqualFold(anyToString(item), value) {
			return items
		}
	}
	if len(items) >= maxItems {
		return items
	}
	return append(items, clipUTF8Bytes(value, 180))
}

func firstNonEmptyAny(values ...any) any {
	for _, value := range values {
		switch typed := value.(type) {
		case nil:
			continue
		case string:
			if strings.TrimSpace(typed) != "" {
				return typed
			}
		case []any:
			if len(typed) > 0 {
				return typed
			}
		default:
			return value
		}
	}
	return nil
}
