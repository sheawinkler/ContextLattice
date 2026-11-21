package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/contextlattice/gateway-go/internal/calleridentity"
)

type nativeCaller struct {
	agent, agentID, model, nativeSessionID string
	explicit                               bool
}

func resolveNativeCaller(parsed parsedArgs, profiles map[string]any) nativeCaller {
	agent := parsed.string("agent", envString("CONTEXTLATTICE_AGENT", ""))
	agentID := parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", "")))
	explicit := agent != "" || agentID != ""
	if agent == "" && agentID != "" {
		// A unique configured alias may label a known actor, but cannot replace
		// its ID or supply the model currently serving this invocation.
		for name, raw := range profiles {
			if firstString(asMap(raw)["agent_id"]) != agentID {
				continue
			}
			if agent != "" {
				agent = ""
				break
			}
			agent = name
		}
	}
	agent = firstNonEmpty(agent, "agent-cli")
	agentID = firstNonEmpty(agentID, firstString(asMap(profiles[agent])["agent_id"]), agent)
	model := parsed.string("model", envString("CONTEXTLATTICE_MODEL", envString("CONTEXTLATTICE_AGENT_MODEL", "")))
	return nativeCaller{agent: agent, agentID: agentID, model: calleridentity.Model(model), nativeSessionID: parsed.string("native_session_id", envString("CONTEXTLATTICE_NATIVE_SESSION_ID", "")), explicit: explicit}
}

func (caller nativeCaller) metadata() map[string]any {
	return calleridentity.Metadata(caller.agentID, caller.model)
}

func (caller nativeCaller) writeTags(base ...string) []string {
	// Distinguish different callers even when neither reports a model.
	tags := base
	if tag := calleridentity.CallerTag(caller.agentID); tag != "" {
		tags = append(tags, tag)
	}
	if tag := calleridentity.Tag(caller.agentID, caller.model); tag != "" {
		tags = append(tags, tag)
	}
	return tags
}

func (caller nativeCaller) sessionStatePath(project string) string {
	legacy := sessionStatePath(project)
	if legacy == "" {
		return ""
	}
	encoded, _ := json.Marshal([]string{
		strings.ToLower(project), currentWorkingDir(), caller.agentID, caller.nativeSessionID,
	})
	hash := sha256.Sum256(encoded)
	return filepath.Join(filepath.Dir(legacy), "caller-"+hex.EncodeToString(hash[:16])+".json")
}

func (caller nativeCaller) readSessionState(project string) map[string]any {
	path := caller.sessionStatePath(project)
	state := readSessionStateFile(path)
	if state["agent_id"] == caller.agentID && strings.EqualFold(firstString(state["project"]), project) {
		return state
	}
	// Read only a matching legacy pointer, never the legacy multi-session
	// outcomes. Those could have been left by a different actor in this cwd.
	legacy := readSessionState(project)
	if caller.agentID == "" || caller.nativeSessionID != "" ||
		legacy["agent_id"] != caller.agentID || !strings.EqualFold(firstString(legacy["project"]), project) {
		return map[string]any{}
	}
	delete(legacy, "pending_context_pack_quality_by_session")
	delete(legacy, "latest_context_pack_quality")
	return legacy
}

func (caller nativeCaller) writeSessionState(project, sessionID, objective string, extras map[string]any) {
	path := caller.sessionStatePath(project)
	if sessionID == "" || path == "" {
		return
	}
	state := caller.readSessionState(project)
	state["session_id"] = sessionID
	state["agent_id"] = caller.agentID
	state["project"] = project
	state["source"] = "go-native-cli"
	state["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	if objective != "" {
		state["objective"] = objective
	}
	for key, value := range extras {
		state[key] = value
	}
	_ = writePrivateJSONArtifact(path, state)
}

func (c *cli) checkCallerSession(project, sessionID string, caller nativeCaller, timeout float64) error {
	raw, _, err := c.requestJSONForValidation(context.Background(), http.MethodGet,
		"/v1/agents/sessions/"+url.PathEscape(sessionID), nil, minFloat(timeout, 5))
	row := asMap(raw["session"])
	// The durable actor ID is authoritative; an earlier CLI may have used a
	// different display harness for the same explicit actor.
	if err != nil || raw["ok"] != true ||
		!agentSessionAuthorityMatches(row, sessionID, project, "", caller.agentID) ||
		!liveResumeSession(row, project, caller.agentID) {
		return errors.New("selected session is not active for this caller and project")
	}
	if nativeID := caller.nativeSessionID; nativeID != "" && row["native_session_id"] != nativeID {
		return errors.New("selected session does not match the caller's native session")
	}
	return nil
}

func (c *cli) lifecycleCallerSession(parsed parsedArgs, project, objective string, profile adapterProfile) (string, error) {
	caller := profile.caller
	selected := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	if selected != "" {
		if caller.explicit || caller.nativeSessionID != "" {
			if err := c.checkCallerSession(project, selected, caller, parsed.float("timeout", 10)); err != nil {
				return "", err
			}
		}
		return selected, nil
	}
	if cached := firstString(caller.readSessionState(project)["session_id"]); cached != "" {
		if err := c.checkCallerSession(project, cached, caller, parsed.float("timeout", 10)); err != nil {
			return "", err
		}
		return cached, nil
	}
	ownership := adapterOwnership(parsed)
	ownership["native_session_id"] = caller.nativeSessionID
	id := c.ensureSessionForAgent(project, objective, caller.agent, caller.agentID, ownership, profile, parsed.float("timeout", 30))
	if id == "" && parsed.bool("strict") {
		return "", errors.New("failed to create ContextLattice agent session")
	}
	return id, nil
}
