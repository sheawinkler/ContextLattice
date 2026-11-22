package main

import "net/http"

func publicCorrectionSessionMatches(row map[string]any, project, actor string) bool {
	owner := firstNonEmptyStrings(anyToString(row["agent_id"]), anyToString(row["agent"]))
	scope, bound := row["scope_ownership"]
	return (!bound || anyMap(scope)["project"] == project) && len(row) != 0 && project != "" && agentSessionProject(row) == project &&
		owner != "" && owner == actor && validatePublicAgentSessionScope(row) == nil
}

func (s *server) publicCorrectionScopeAllowed(project, session, actor string) bool {
	if session == "" {
		return true
	}
	if s.agentSessions == nil || validateAgentSessionID(session) != nil {
		return false
	}
	row, _, found := s.agentSessions.get(session)
	return found && publicCorrectionSessionMatches(row, project, actor)
}

func writeCorrectionSessionNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "agent session not found"})
}
