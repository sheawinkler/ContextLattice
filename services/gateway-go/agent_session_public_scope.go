package main

import (
	"errors"
	"strings"
	"time"
)

// Public sessions may retain legacy unbound rows, never reinterpret a retained
// workspace owner as public merely because this edition cannot authorize it.
func validatePublicAgentSessionScope(row map[string]any) error {
	for key := range row {
		folded := strings.NewReplacer("_", "", "-", "", ".", "").Replace(strings.ToLower(key))
		if folded == "scopeownership" && key != "scope_ownership" {
			return errAgentSessionOwnershipConflict
		}
	}
	raw, exists := row["scope_ownership"]
	if !exists {
		return nil
	}
	scope, ok := raw.(map[string]any)
	if !ok || len(scope) != 4 {
		return errAgentSessionOwnershipConflict
	}
	for _, key := range []string{"schema_id", "kind", "project", "established_at"} {
		if _, ok := scope[key].(string); !ok {
			return errAgentSessionOwnershipConflict
		}
	}
	if scope["schema_id"] != "agent_session_scope_ownership.v1" || scope["kind"] != "public_unbound" ||
		!strings.EqualFold(scope["project"].(string), agentSessionProject(row)) {
		return errAgentSessionOwnershipConflict
	}
	if _, err := time.Parse(time.RFC3339Nano, scope["established_at"].(string)); err != nil {
		return errAgentSessionOwnershipConflict
	}
	return nil
}

func establishAgentSessionScopeOwnership(row map[string]any, requested map[string]any) (bool, error) {
	if err := validatePublicAgentSessionScope(row); err != nil {
		return false, err
	}
	if len(requested) == 0 {
		return false, nil
	}
	candidate := cloneAnyMap(row)
	candidate["scope_ownership"] = requested
	if err := validatePublicAgentSessionScope(candidate); err != nil {
		return false, err
	}
	if _, present := row["scope_ownership"]; present {
		return false, nil
	}
	if len(row) == 0 {
		return false, errors.New("agent session row missing")
	}
	row["scope_ownership"] = cloneAnyMap(requested)
	return true, nil
}
