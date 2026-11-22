package main

import (
	"strings"
	"time"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

const searchConflictEvidenceClaimLimit = 128

func visibleSearchEvidenceRefs(project, topic string, raw any) map[string]map[string]struct{} {
	visible := map[string]map[string]struct{}{}
	var rows []any
	switch value := raw.(type) {
	case []any:
		rows = value
	case []map[string]any:
		rows = make([]any, len(value))
		for i := range value {
			rows[i] = value[i]
		}
	default:
		return visible
	}
	for _, value := range rows {
		row := anyMap(value)
		rowProject := strings.TrimSpace(anyToString(row["project"]))
		file := strings.TrimSpace(anyToString(row["file"]))
		if rowProject != project || file == "" {
			continue
		}
		rowTopic := strings.Trim(strings.TrimSpace(anyToString(row["topic_path"])), "/")
		if rowTopic == "" || (rowTopic != topic && !strings.HasPrefix(rowTopic, topic+"/")) {
			continue
		}
		memoryID := project + "::" + file
		if reportedID := strings.TrimSpace(anyToString(row["memory_id"])); reportedID != "" && reportedID != memoryID {
			continue
		}
		digest := normalizeProjectedContentHash(anyToString(row["content_hash"]))
		if !isHexDigest(digest) {
			continue
		}
		if visible[digest] == nil {
			visible[digest] = map[string]struct{}{}
		}
		visible[digest][memoryID] = struct{}{}
	}
	return visible
}

func claimHasVisibleCurrentEvidence(claim temporalClaim, visible map[string]map[string]struct{},
	issuers []temporalClaimEvidenceIssuer, now time.Time) bool {
	for _, refs := range [][]temporalClaimEvidence{claim.Support, claim.Opposition} {
		for _, ref := range refs {
			digest := normalizeProjectedContentHash(ref.ContentHash)
			if !isHexDigest(digest) {
				continue
			}
			if _, ok := visible[digest][strings.TrimSpace(ref.MemoryID)]; !ok {
				continue
			}
			if ref.Admission != nil && temporalClaimCurrentEvidenceAuthority(claim, ref, issuers, now) == temporalClaimInactiveAttestation {
				continue
			}
			return true
		}
	}
	return false
}

func scopedSearchConflictSourceCheck(store *temporalClaimStore, project, topic string, rawResults any) (decisionresearch.Case, bool) {
	project = strings.TrimSpace(project)
	if project == "" {
		return decisionresearch.Case{}, false
	}
	project, err := sanitizeMemoryProject(project)
	if err != nil {
		return decisionresearch.Case{}, false
	}
	topic, ok := normalizeBrowserTopicPath(topic)
	if !ok || strings.TrimSpace(topic) == "" {
		return decisionresearch.Case{}, false
	}
	topic = strings.Trim(topic, "/")
	visible := visibleSearchEvidenceRefs(project, topic, rawResults)
	if len(visible) == 0 {
		return decisionresearch.Case{}, false
	}
	claims, complete := store.visibleEvidenceClaims(project, topic, visible, searchConflictEvidenceClaimLimit)
	if !complete || len(claims) < 2 {
		return decisionresearch.Case{}, false
	}
	issuers := configuredTemporalClaimEvidenceIssuers()
	now := time.Now().UTC()
	matched := make([]temporalClaim, 0, len(claims))
	for _, claim := range claims {
		if claimHasVisibleCurrentEvidence(claim, visible, issuers, now) {
			matched = append(matched, claim)
		}
	}
	return scopedConflictCheck(project, topic, matched)
}
