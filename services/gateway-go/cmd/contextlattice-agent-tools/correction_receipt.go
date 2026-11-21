package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func (c *cli) checkCorrectionSession(project, sessionID string, caller nativeCaller, timeout float64) error {
	raw, _, err := c.requestJSONForValidation(context.Background(), http.MethodGet,
		"/v1/agents/sessions/"+url.PathEscape(sessionID), nil, minFloat(timeout, 5))
	row := asMap(raw["session"])
	var actor string
	if row != nil {
		actor = firstString(row["agent_id"], row["agent"])
	}
	// Corrections are allowed as post-terminal observations, but never against
	// a different project, actor, or native session.
	if err != nil || !correctionSessionPublic(row, project) || raw["ok"] != true || !agentSessionAuthorityMatches(row, sessionID, project, "", "") || row["project"] != project ||
		actor != caller.agentID || (caller.nativeSessionID != "" && row["native_session_id"] != caller.nativeSessionID) {
		return errors.New("selected session is not available for this correction caller and project")
	}
	return nil
}

func correctionFeedbackReceiptMatches(receipt, sent map[string]any) bool {
	row := asMap(receipt["feedback"])
	if receipt["ok"] != true || receipt["recorded"] != true || firstString(row["id"]) == "" ||
		row["source"] != "agent" || row["content"] != sent["content"] || row["project"] != sent["project"] {
		return false
	}
	stored, expected := asMap(row["metadata"]), asMap(sent["metadata"])
	for _, field := range []string{"schema_id", "agent_id", "session_id", "category", "factual", "target_claim_id", "operation_id"} {
		if stored[field] != expected[field] {
			return false
		}
	}
	return true
}

func correctionClaimReceiptMatches(claim, sent map[string]any, content, category, target string) bool {
	provenance := asMap(claim["provenance"])
	if claim["schema_id"] != "temporal_claim.v1" || firstString(claim["claim_id"]) == "" ||
		asInt(claim["revision"]) != 1 || claim["project"] != sent["project"] ||
		claim["agent_id"] != sent["agent_id"] || firstString(claim["session_id"]) != firstString(sent["session_id"]) ||
		claim["statement"] != content || claim["subject"] != sent["subject"] || claim["object"] != sent["object"] ||
		claim["predicate"] != strings.ReplaceAll(strings.ToLower(firstString(sent["predicate"])), " ", "_") ||
		provenance["source"] != "correction_reporter_assertion" || firstString(provenance["source_id"]) != target ||
		len(asList(claim["supersedes"])) != 0 {
		return false
	}
	if requested := firstString(sent["claim_id"]); requested != "" && claim["claim_id"] != requested {
		return false
	}
	against := asList(claim["contradicts"])
	if category == "wrong" {
		return len(against) == 1 && against[0] == target
	}
	return len(against) == 0
}

func correctionSessionPublic(row map[string]any, project string) bool {
	raw, exists := row["scope_ownership"]
	if !exists {
		return true // A legacy unbound session still has an exact project and actor.
	}
	scope := asMap(raw)
	return scope["schema_id"] == "agent_session_scope_ownership.v1" && scope["kind"] == "public_unbound" && scope["project"] == project
}
