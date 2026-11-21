package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

// Missing, historical, differently scoped and unselected opponents are not
// research pointers. Returning their IDs could disclose a source the caller did not see.
func scopedConflictCheck(project, topic string, rows []temporalClaim) (decisionresearch.Case, bool) {
	byID := make(map[string]temporalClaim, len(rows))
	now := time.Now().UTC()
	for _, row := range rows {
		if frontierT5CurrentClaimExclusion(project, row, now) != "" ||
			(topic != "" && row.TopicPath != topic && !strings.HasPrefix(row.TopicPath, topic+"/")) {
			continue
		}
		byID[row.ClaimID] = row
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var selected decisionresearch.Case
	for _, id := range ids {
		left := byID[id]
		for _, target := range left.Contradicts {
			right, ok := byID[target]
			if !ok || right.TopicPath != left.TopicPath || left.Branch != right.Branch ||
				left.Subject == "" || left.Predicate == "" || left.Subject != right.Subject ||
				left.Predicate != right.Predicate || left.Object == "" || right.Object == "" || left.Object == right.Object {
				continue
			}
			c, ok := decisionresearch.New(project, left.TopicPath, left.Branch,
				decisionresearch.Claim{ID: id, Revision: left.Revision},
				decisionresearch.Claim{ID: target, Revision: right.Revision})
			if ok && (selected.Ref == "" || c.Ref < selected.Ref) {
				selected = c
			}
		}
	}
	return selected, selected.Ref != ""
}

const conflictCheckGuidance = "Before acting on the disputed premise, check one authorized original that distinguishes the two claim revisions. Record what it settles, or which evidence or owner is missing. Continue work that is safe under either claim. This is not permission to act."

// The existing read-only claim-query owner can recheck an exact case without
// disclosing either claim's text or treating a vanished opponent as a winner.
func (s *server) recheckConflictSource(w http.ResponseWriter, r *http.Request, payload map[string]any, raw any) {
	var check decisionresearch.Case
	data, err := json.Marshal(raw)
	project, topic := anyToString(payload["project"]), anyToString(payload["topic_path"])
	if err != nil || len(data) > 2048 || json.Unmarshal(data, &check) != nil || !check.ValidFor(project, topic) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "error": "invalid_conflict_source_check"})
		return
	}
	unlock := s.lockOptionalFrontierT1ProjectBoundary()
	defer unlock()
	session, task, actor := anyToString(payload["session_id"]), anyToString(payload["task_id"]), anyToString(payload["agent_id"])
	if s.agentSessions == nil || session == "" || task == "" || actor == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session not found"})
		return
	}
	row, _, exists := s.agentSessions.get(session)
	if !exists || agentSessionProject(row) != project || row["task_id"] != task || row["agent_id"] != actor ||
		row["scope_ownership"] == nil || validatePublicAgentSessionScope(row) != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "agent session not found"})
		return
	}
	rows := frontierT5Claims(s.temporalClaims, project, []string{check.Claims[0].ID, check.Claims[1].ID}, "", time.Now().UTC())
	current, ok := scopedConflictCheck(project, topic, rows)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "case_ref": check.Ref, "current": ok && current == check})
}
