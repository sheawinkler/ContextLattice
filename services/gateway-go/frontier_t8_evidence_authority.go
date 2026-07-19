package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const frontierT8MaxAuthoritativeEvidenceRefs = frontierT8MaxReceipts * frontierT8MaxEvidenceRefs

type frontierT8EvidenceClaim struct {
	Path               string
	Ref                map[string]any
	ExpectedVerifiedAt time.Time
}

func frontierT8BindServerClock(input map[string]any, now time.Time) map[string]any {
	bound := cloneJSONMap(input)
	bound["as_of"] = now.UTC().Format(time.RFC3339Nano)
	return bound
}

func frontierT8CollectEvidenceClaims(value any, path string, inherited time.Time, out *[]frontierT8EvidenceClaim) error {
	switch typed := value.(type) {
	case map[string]any:
		verifiedAt := inherited
		if raw, exists := typed["verified_at"]; exists {
			parsed, _, err := frontierT8Timestamp(raw, path+".verified_at")
			if err != nil {
				return err
			}
			verifiedAt = parsed
		}
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			raw := typed[key]
			childPath := path + "." + key
			if key == "evidence_refs" {
				for index, item := range contextPackAnyList(raw) {
					if len(*out) >= frontierT8MaxAuthoritativeEvidenceRefs {
						return fmt.Errorf("authoritative evidence exceeds %d refs", frontierT8MaxAuthoritativeEvidenceRefs)
					}
					ref := anyMap(item)
					if len(ref) == 0 {
						return fmt.Errorf("%s[%d] must be an object", childPath, index)
					}
					*out = append(*out, frontierT8EvidenceClaim{Path: fmt.Sprintf("%s[%d]", childPath, index), Ref: ref, ExpectedVerifiedAt: verifiedAt})
				}
				continue
			}
			if err := frontierT8CollectEvidenceClaims(raw, childPath, verifiedAt, out); err != nil {
				return err
			}
		}
	case []any:
		for index, item := range typed {
			if err := frontierT8CollectEvidenceClaims(item, fmt.Sprintf("%s[%d]", path, index), inherited, out); err != nil {
				return err
			}
		}
	case []map[string]any:
		for index, item := range typed {
			if err := frontierT8CollectEvidenceClaims(item, fmt.Sprintf("%s[%d]", path, index), inherited, out); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *server) frontierT8ResolveEvidenceAuthority(input, candidate map[string]any, now time.Time) (map[string]any, error) {
	if s == nil || s.utility == nil || s.utility.store == nil || s.agentSessions == nil {
		return nil, errors.New("authoritative utility and session ledgers are required")
	}
	configured, enabled := s.utility.store.availability()
	if !configured || !enabled {
		return nil, errors.New("authoritative utility ledger persistence is unavailable")
	}
	project := strings.TrimSpace(anyToString(candidate["project"]))
	if project == "" {
		return nil, errors.New("candidate project is required for evidence authority")
	}
	claims := make([]frontierT8EvidenceClaim, 0, 16)
	if err := frontierT8CollectEvidenceClaims(input, "input", time.Time{}, &claims); err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return nil, errors.New("authoritative evidence refs are required")
	}
	window := anyMap(candidate["review_window"])
	var windowStart, windowEnd time.Time
	if len(window) > 0 {
		windowStart, _, _ = frontierT8Timestamp(window["start_at"], "candidate.review_window.start_at")
		windowEnd, _, _ = frontierT8Timestamp(window["end_at"], "candidate.review_window.end_at")
	}
	resolved := make([]any, 0, len(claims))
	for _, claim := range claims {
		row, err := s.frontierT8ResolveEvidenceClaim(project, claim, now, windowStart, windowEnd)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", claim.Path, err)
		}
		resolved = append(resolved, row)
	}
	resolutionRaw, _ := json.Marshal(resolved)
	return map[string]any{
		"authoritative_evidence_resolved": true,
		"authority":                       "utility_ledger+agent_session_ledger",
		"persistence_required":            true,
		"resolved_at":                     now.UTC().Format(time.RFC3339Nano),
		"resolved_ref_count":              len(resolved),
		"resolution_digest":               "sha256:" + sha256Hex(string(resolutionRaw)),
	}, nil
}

func (s *server) frontierT8ResolveEvidenceClaim(project string, claim frontierT8EvidenceClaim, now, windowStart, windowEnd time.Time) (map[string]any, error) {
	refID := strings.TrimSpace(anyToString(claim.Ref["ref_id"]))
	digest := strings.ToLower(strings.TrimSpace(anyToString(claim.Ref["digest"])))
	verificationID := strings.TrimSpace(anyToString(claim.Ref["verification_id"]))
	producerID := strings.TrimSpace(anyToString(claim.Ref["producer_id"]))
	verifierID := strings.TrimSpace(anyToString(claim.Ref["verifier_id"]))
	kind := strings.ToLower(strings.TrimSpace(anyToString(claim.Ref["kind"])))
	if refID == "" || digest == "" || verificationID == "" || producerID == "" || verifierID == "" || kind == "" {
		return nil, errors.New("evidence authority keys are incomplete")
	}
	observation, exists := s.utility.observation(refID)
	if !exists || len(observation) == 0 {
		return nil, fmt.Errorf("utility outcome %q was not found", refID)
	}
	if !strings.EqualFold(anyToString(observation["project"]), project) {
		return nil, errors.New("utility outcome project does not match the candidate")
	}
	if !strings.EqualFold(anyToString(observation["agent_id"]), producerID) {
		return nil, errors.New("utility outcome producer does not match the evidence ref")
	}
	utility := anyMap(observation["utility"])
	if !anyToBool(utility["independently_verified"]) || anyToString(utility["verification_status"]) != "verified" {
		return nil, errors.New("utility outcome is not independently verified")
	}
	if anyToString(utility["evidence_digest"]) != digest || anyToString(utility["verification_event_id"]) != verificationID {
		return nil, errors.New("utility outcome digest or verification identity does not match")
	}
	if !strings.EqualFold(anyToString(utility["verifier_id"]), verifierID) || !strings.EqualFold(anyToString(utility["verifier_kind"]), kind) {
		return nil, errors.New("utility outcome verifier does not match the evidence ref")
	}
	sessionID := strings.TrimSpace(anyToString(observation["session_id"]))
	if sessionID == "" {
		return nil, errors.New("utility outcome is not bound to a session")
	}
	session, events, exists := s.agentSessions.get(sessionID)
	if !exists || len(session) == 0 {
		return nil, errors.New("verification session was not found")
	}
	if !strings.EqualFold(anyToString(session["project"]), project) {
		return nil, errors.New("verification session project does not match the candidate")
	}
	event := utilityVerificationEvent(events, verificationID)
	verifiedClaim, reason := utilityVerifyClaim(observation, event)
	if reason != "" || !anyToBool(verifiedClaim["independently_verified"]) {
		return nil, fmt.Errorf("verification event failed authoritative reconciliation: %s", firstNonEmptyStrings(reason, "unverified"))
	}
	eventAt, err := time.Parse(time.RFC3339Nano, anyToString(event["created_at"]))
	if err != nil {
		return nil, errors.New("verification event has an invalid timestamp")
	}
	eventAt = eventAt.UTC()
	if eventAt.After(now.UTC().Add(time.Minute)) {
		return nil, errors.New("verification event is in the future")
	}
	if !claim.ExpectedVerifiedAt.IsZero() && !eventAt.Equal(claim.ExpectedVerifiedAt.UTC()) {
		return nil, errors.New("verification event timestamp does not match the workflow receipt")
	}
	if !windowStart.IsZero() && (eventAt.Before(windowStart) || !eventAt.Before(windowEnd)) {
		return nil, errors.New("verification event falls outside the retirement review window")
	}
	return map[string]any{
		"ref_id": refID, "digest": digest, "verification_id": verificationID,
		"session_id": sessionID, "producer_id": producerID, "verifier_id": verifierID,
		"verified_at": eventAt.Format(time.RFC3339Nano),
	}, nil
}

func frontierT8AttachEvidenceAuthority(candidate, authority map[string]any) {
	provenance := anyMap(candidate["provenance"])
	provenance["evidence_resolved"] = true
	if _, exists := provenance["independent_verification"]; exists {
		provenance["independent_verification"] = true
	}
	if _, exists := provenance["independently_verified"]; exists {
		provenance["independently_verified"] = true
	}
	provenance["authoritative_evidence_resolved"] = true
	provenance["evidence_authority"] = authority
	candidate["provenance"] = provenance
}
