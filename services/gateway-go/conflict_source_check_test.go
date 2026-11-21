package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestConflictSourceCurrentFencesExactSessionAndClaimRevisions(t *testing.T) {
	a := temporalClaim{ClaimID: "a", Project: "studio", TopicPath: "design", Subject: "proposal", Predicate: "state",
		Object: "submitted", Status: "active", Revision: 1, Contradicts: []string{"b"}}
	b := a
	b.ClaimID, b.Object, b.Contradicts = "b", "accepted", nil
	k, _ := scopedConflictCheck("studio", "design", []temporalClaim{a, b})
	s := &server{temporalClaims: &temporalClaimStore{enabled: true, claims: map[string]temporalClaim{"a": a, "b": b}},
		agentSessions: &agentSessionStore{sessions: map[string]map[string]any{"sess": {"id": "sess", "project": "studio", "agent_id": "reviewer",
			"task_id": "task", "scope_ownership": newAgentSessionPublicScopeOwnership("studio")}}}}
	call := func(task, session string, tool bool) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"conflict_source_check": k, "project": "studio", "topic_path": "design",
			"task_id": task, "session_id": session, "agent_id": "reviewer"})
		req := httptest.NewRequest(http.MethodPost, "/memory/claims/query", strings.NewReader(string(data)))
		w := httptest.NewRecorder()
		s.handleClaimQuery(w, req, tool)
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return w.Code, out
	}
	code, valid := call("task", "sess", false)
	if code != http.StatusOK || valid["current"] != true || valid["case_ref"] != k.Ref || len(valid) != 3 {
		t.Fatalf("current: %d %v", code, valid)
	}
	if code, denied := call("other", "sess", false); code != http.StatusNotFound || len(denied) != 1 {
		t.Fatalf("foreign task: %v", denied)
	}
	if code, denied := call("task", "absent", false); code != http.StatusNotFound || len(denied) != 1 {
		t.Fatalf("absent session: %v", denied)
	}
	if code, _ := call("task", "sess", true); code != http.StatusUnprocessableEntity {
		t.Fatal("native selector admitted by tool")
	}
	b.Revision++
	s.temporalClaims.claims["b"] = b
	if code, stale := call("task", "sess", false); code != http.StatusOK || stale["current"] != false || len(stale) != 3 {
		t.Fatalf("stale: %v", stale)
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
