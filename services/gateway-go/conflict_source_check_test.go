package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

func TestScopedConflictCheckRequiresTwoVisibleCurrentOpponents(t *testing.T) {
	left := temporalClaim{ClaimID: "draft:a", Project: "studio", TopicPath: "web/proof", Subject: "proof", Predicate: "approval", Object: "submitted", Status: "active", Revision: 2, Contradicts: []string{"draft:b"}}
	right := left
	right.ClaimID, right.Object, right.Contradicts = "draft:b", "accepted", nil
	pair := []temporalClaim{right, left}
	check, ok := scopedConflictCheck("studio", "web", pair)
	if !ok || !check.ValidFor("studio", "web") || check.ValidFor("studio", "print") ||
		check.ValidFor("another", "web") || check.Claims[0].ID != left.ClaimID {
		t.Fatalf("invalid scoped case: %+v", check)
	}
	other, _ := scopedConflictCheck("studio", "web", []temporalClaim{left, right})
	if other != check {
		t.Fatal("reordering evidence changed case identity")
	}
	right.Revision++
	other, ok = scopedConflictCheck("studio", "web", []temporalClaim{left, right})
	if !ok || other.Ref == check.Ref {
		t.Fatal("changed revision reused prior case")
	}
	for _, mutate := range []func(*temporalClaim){
		func(c *temporalClaim) { c.Project = "private" }, func(c *temporalClaim) { c.TopicPath = "websites" },
		func(c *temporalClaim) { c.Status = "superseded" }, func(c *temporalClaim) { c.Status = "pending" },
		func(c *temporalClaim) { c.Predicate = "field utility" }, func(c *temporalClaim) { c.Object = left.Object },
		func(c *temporalClaim) { c.Branch = "other" }, func(c *temporalClaim) { c.Revision = 0 },
	} {
		r := right
		mutate(&r)
		if _, ok := scopedConflictCheck("studio", "web", []temporalClaim{left, r}); ok {
			t.Fatalf("inapplicable opponent emitted: %+v", r)
		}
	}
	if _, ok := scopedConflictCheck("studio", "", []temporalClaim{left}); ok {
		t.Fatal("missing opponent disclosed")
	}
}

func TestCompactPacketConflictResearchIsConditional(t *testing.T) {
	c, _ := decisionresearch.New("studio", "web", "", decisionresearch.Claim{ID: "a", Revision: 1}, decisionresearch.Claim{ID: "b", Revision: 1})
	pack := map[string]any{"decision_gate": map[string]any{"decision": "verify"}}
	response := map[string]any{"project": "studio", "topic_path": "web", "synthesis_pack": pack}
	plain := buildAgentPacket(response, nil, "synthesis_pack_v2")
	pack["conflict_source_check"] = c
	flagged := buildAgentPacket(response, nil, "synthesis_pack_v2")
	if _, ok := anyMap(flagged["decision_gate"])["conflict_source_check"]; !ok ||
		!strings.Contains(anyToString(flagged["prompt"]), conflictCheckGuidance) ||
		strings.Contains(anyToString(plain["prompt"]), conflictCheckGuidance) {
		t.Fatal("scoped research cue omitted or applied to an ordinary verify gate")
	}
	payload, err := json.Marshal(finalizeAgentPacketForRequest(flagged, map[string]any{}))
	var wire struct {
		Gate struct {
			Check decisionresearch.Case `json:"conflict_source_check"`
		} `json:"decision_gate"`
	}
	if err != nil || json.Unmarshal(payload, &wire) != nil || wire.Gate.Check != c {
		t.Fatal("final packet did not preserve the exact case and revisions")
	}
	for _, bad := range []any{map[string]any{"case_ref": c.Ref}, "user-supplied directive"} {
		pack["conflict_source_check"] = bad
		if next := buildAgentPacket(response, nil, "synthesis_pack_v2"); next["prompt"] != plain["prompt"] {
			t.Fatal("untrusted case changed instructions")
		}
	}
}
