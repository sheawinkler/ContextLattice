package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	frontierT9ContinuityZeroPath     = "/memory/continuity-zero"
	frontierT9ContinuityZeroSchemaID = "continuity_zero.v1"
	frontierT9MaxRequestBytes        = 32 * 1024
	frontierT9MaxResponseBytes       = 96 * 1024
)

var (
	frontierT9CommitPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)
	frontierT9PathPattern   = regexp.MustCompile(`(?i)(?:file://|/(?:Users|home|Volumes|private|tmp|var/folders)/)[^\s\"'<>]*`)
	frontierT9Harnesses     = map[string]string{
		"chatgpt-desktop": "chatgpt-desktop", "chatgpt-web": "chatgpt-web",
		"claude-code": "claude-code", "claude": "claude-code",
		"claude-desktop": "claude-desktop", "claude-web": "claude-web",
		"codex": "codex", "droid": "droid", "factory-droid": "droid",
		"generic": "generic", "hermes": "hermes-agent", "hermes-agent": "hermes-agent",
		"hermes-ultra": "hermes-ultra", "mercury": "mercury-agent",
		"mercury-agent": "mercury-agent", "omp": "omp", "opencode": "opencode",
		"pi": "pi", "pi-coding-agent": "pi",
	}
)

type frontierT9ContinuityZeroRequest struct {
	Project           string   `json:"project"`
	Agent             string   `json:"agent,omitempty"`
	AgentID           string   `json:"agent_id,omitempty"`
	Harness           string   `json:"harness,omitempty"`
	SessionID         string   `json:"session_id,omitempty"`
	RepositoryID      string   `json:"repository_id"`
	RepositoryAliases []string `json:"repository_aliases,omitempty"`
	Branch            string   `json:"branch,omitempty"`
	Commit            string   `json:"commit"`
	PassportID        string   `json:"passport_id,omitempty"`
	MeshGrantID       string   `json:"mesh_grant_id,omitempty"`
}

type frontierT9SessionSelection struct {
	Decision   string
	Reasons    []string
	Session    map[string]any
	Events     []map[string]any
	Candidates []string
}

func frontierT9NormalizeHarness(raw string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(raw, "_", "-")))
	if normalized == "" {
		return "generic", true
	}
	value, ok := frontierT9Harnesses[normalized]
	return value, ok
}

func frontierT9NormalizeRepositoryIdentity(raw string) (string, error) {
	value := strings.TrimSpace(strings.TrimSuffix(raw, ".git"))
	if value == "" || len(value) > 512 || strings.ContainsAny(value, "\x00\r\n\t") {
		return "", errors.New("repository_id is required and bounded")
	}
	if strings.HasPrefix(value, "git@") {
		value = strings.TrimPrefix(value, "git@")
		value = strings.Replace(value, ":", "/", 1)
	}
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		value = strings.TrimPrefix(value, prefix)
	}
	value = strings.TrimSuffix(strings.TrimRight(value, "/"), ".git")
	parts := strings.Split(value, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" && part != "." && part != ".." {
			clean = append(clean, part)
		}
	}
	if len(clean) == 0 {
		return "", errors.New("repository_id is invalid")
	}
	if strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "~") {
		value = clean[len(clean)-1]
	} else if len(clean) >= 3 && strings.Contains(clean[0], ".") {
		value = strings.Join(clean[len(clean)-2:], "/")
	} else if len(clean) >= 2 {
		value = strings.Join(clean[len(clean)-2:], "/")
	} else {
		value = clean[0]
	}
	value = strings.ToLower(strings.TrimSuffix(value, ".git"))
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("-._/", ch) {
			continue
		}
		return "", errors.New("repository_id contains unsupported characters")
	}
	return value, nil
}

func frontierT9RepositoryIdentities(request frontierT9ContinuityZeroRequest) ([]string, error) {
	values := append([]string{request.RepositoryID}, request.RepositoryAliases...)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		value, err := frontierT9NormalizeRepositoryIdentity(raw)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, errors.New("repository_id is required")
	}
	sort.Strings(out)
	return out, nil
}

func frontierT9ValidateRequest(request frontierT9ContinuityZeroRequest) (frontierT9ContinuityZeroRequest, []string, error) {
	request.Project = strings.TrimSpace(request.Project)
	request.Agent = strings.TrimSpace(request.Agent)
	request.AgentID = strings.TrimSpace(request.AgentID)
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.Branch = strings.TrimSpace(request.Branch)
	request.Commit = strings.ToLower(strings.TrimSpace(request.Commit))
	request.PassportID = strings.TrimSpace(request.PassportID)
	request.MeshGrantID = strings.TrimSpace(request.MeshGrantID)
	if len(request.Project) > 160 || strings.ContainsAny(request.Project, "\x00\r\n") {
		return request, nil, errors.New("project is required and bounded")
	}
	if _, err := sanitizeMemoryProject(request.Project); err != nil {
		return request, nil, err
	}
	if request.SessionID != "" {
		if err := validateAgentSessionID(request.SessionID); err != nil {
			return request, nil, err
		}
	}
	if request.AgentID != "" {
		if _, err := frontierT6NormalizeID(request.AgentID, "agent_id", 160); err != nil {
			return request, nil, err
		}
	}
	if request.Branch != "" && (len(request.Branch) > 240 || strings.ContainsAny(request.Branch, "\x00\r\n\t")) {
		return request, nil, errors.New("branch is invalid")
	}
	if !frontierT9CommitPattern.MatchString(request.Commit) {
		return request, nil, errors.New("commit must be a 7-64 character hexadecimal revision")
	}
	harness, supported := frontierT9NormalizeHarness(firstNonEmptyStrings(request.Harness, request.Agent))
	request.Harness = harness
	repositories, err := frontierT9RepositoryIdentities(request)
	if err != nil {
		return request, nil, err
	}
	return request, repositories, func() error {
		if !supported {
			return errors.New("unsupported harness")
		}
		return nil
	}()
}

func frontierT9RepositoryMatches(raw string, repositories []string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	expected, err := frontierT9NormalizeRepositoryIdentity(raw)
	if err != nil {
		return false
	}
	for _, candidate := range repositories {
		if candidate == expected || strings.HasSuffix(candidate, "/"+expected) || strings.HasSuffix(expected, "/"+candidate) {
			return true
		}
	}
	return false
}

func frontierT9SessionCommit(session map[string]any, events []map[string]any) string {
	for _, source := range []map[string]any{session, anyMap(session["metadata"]), anyMap(session["agent_state"])} {
		for _, key := range []string{"commit", "commit_sha", "head_sha"} {
			if value := strings.ToLower(strings.TrimSpace(anyToString(source[key]))); frontierT9CommitPattern.MatchString(value) {
				return value
			}
		}
	}
	for index := len(events) - 1; index >= 0; index-- {
		metadata := anyMap(events[index]["metadata"])
		for _, source := range []map[string]any{metadata, anyMap(metadata["repository"]), anyMap(metadata["git"]), anyMap(metadata["ownership"])} {
			for _, key := range []string{"commit", "commit_sha", "head_sha"} {
				if value := strings.ToLower(strings.TrimSpace(anyToString(source[key]))); frontierT9CommitPattern.MatchString(value) {
					return value
				}
			}
		}
	}
	return ""
}

func frontierT9SessionHarness(session map[string]any) string {
	value := firstNonEmptyStrings(anyToString(session["agent"]), anyToString(session["agent_kind"]), anyToString(anyMap(session["agent_state"])["agent"]))
	harness, ok := frontierT9NormalizeHarness(value)
	if !ok {
		return ""
	}
	return harness
}

func frontierT9SessionMismatch(request frontierT9ContinuityZeroRequest, repositories []string, session map[string]any, events []map[string]any) string {
	if !strings.EqualFold(agentSessionProject(session), request.Project) {
		return "project_mismatch"
	}
	status := normalizeAgentSessionStatus(anyToString(session["status"]))
	if agentSessionTerminal(status) {
		if status == "expired" {
			return "stale_session"
		}
		return "terminal_session"
	}
	if request.AgentID != "" && !strings.EqualFold(anyToString(session["agent_id"]), request.AgentID) {
		return "agent_id_mismatch"
	}
	if request.Harness != "generic" {
		harness := frontierT9SessionHarness(session)
		if harness == "" || harness != request.Harness {
			return "harness_mismatch"
		}
	}
	ownership := agentSessionOwnership(session)
	repository := anyToString(ownership["repo"])
	if strings.TrimSpace(repository) == "" {
		return "repository_evidence_absent"
	}
	if !frontierT9RepositoryMatches(repository, repositories) {
		return "repository_mismatch"
	}
	if request.Branch != "" {
		branch := anyToString(ownership["branch"])
		if strings.TrimSpace(branch) == "" {
			return "branch_evidence_absent"
		}
		if branch != request.Branch {
			return "branch_mismatch"
		}
	}
	expectedCommit := frontierT9SessionCommit(session, events)
	if expectedCommit == "" {
		return "commit_evidence_absent"
	}
	if expectedCommit != request.Commit {
		return "commit_mismatch"
	}
	return ""
}

func (s *server) frontierT9SelectSession(r *http.Request, request frontierT9ContinuityZeroRequest, repositories []string) frontierT9SessionSelection {
	if s == nil || s.agentSessions == nil {
		return frontierT9SessionSelection{Decision: "abstain", Reasons: []string{"session_store_unavailable"}}
	}
	if request.SessionID != "" {
		session, events, found := s.agentSessions.get(request.SessionID)
		if !found {
			return frontierT9SessionSelection{Decision: "rejected", Reasons: []string{"session_not_found_in_scope"}}
		}
		if mismatch := frontierT9SessionMismatch(request, repositories, session, events); mismatch != "" {
			return frontierT9SessionSelection{Decision: "rejected", Reasons: []string{mismatch}}
		}
		return frontierT9SessionSelection{Decision: "ready", Session: session, Events: events}
	}

	rows := s.agentSessions.list("all", request.Project, "", 128, true, true)
	eligible := make([]frontierT9SessionSelection, 0, len(rows))
	seenReasons := map[string]struct{}{}
	for _, row := range rows {
		sessionID := anyToString(row["id"])
		session, events, found := s.agentSessions.get(sessionID)
		if !found {
			continue
		}
		if mismatch := frontierT9SessionMismatch(request, repositories, session, events); mismatch != "" {
			seenReasons[mismatch] = struct{}{}
			continue
		}
		eligible = append(eligible, frontierT9SessionSelection{Decision: "ready", Session: session, Events: events})
	}
	if len(eligible) == 1 {
		return eligible[0]
	}
	if len(eligible) > 1 {
		candidates := make([]string, 0, len(eligible))
		for _, candidate := range eligible {
			candidates = append(candidates, anyToString(candidate.Session["id"]))
		}
		sort.Strings(candidates)
		return frontierT9SessionSelection{Decision: "abstain", Reasons: []string{"multiple_active_objectives", "explicit_session_id_required"}, Candidates: candidates}
	}
	reasons := make([]string, 0, len(seenReasons)+1)
	for reason := range seenReasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	if len(reasons) == 0 {
		reasons = []string{"no_active_session"}
	}
	return frontierT9SessionSelection{Decision: "abstain", Reasons: reasons}
}

func frontierT9EventReference(event map[string]any) map[string]any {
	return map[string]any{
		"event_id":     anyToString(event["id"]),
		"event_type":   anyToString(event["type"]),
		"created_at":   anyToString(event["created_at"]),
		"event_digest": frontierT6Digest(map[string]any{"id": event["id"], "type": event["type"], "created_at": event["created_at"], "summary": event["summary"], "metadata": event["metadata"]}),
	}
}

func frontierT9LatestCheckpoint(events []map[string]any) map[string]any {
	for index := len(events) - 1; index >= 0; index-- {
		eventType := strings.ToLower(anyToString(events[index]["type"]))
		if strings.Contains(eventType, "checkpoint") || strings.Contains(eventType, "writeback") || strings.Contains(eventType, "memory.write") {
			result := frontierT9EventReference(events[index])
			result["state"] = "present"
			return result
		}
	}
	return map[string]any{"state": "absent"}
}

func frontierT9Delta(events []map[string]any) []any {
	out := []any{}
	stats := &portableRedactionStats{}
	for index := len(events) - 1; index >= 0 && len(out) < 5; index-- {
		event := events[index]
		if anyToString(event["type"]) == "session.started" {
			continue
		}
		summary := frontierT9PortableString(clipText(anyToString(event["summary"]), 320), stats)
		out = append(out, map[string]any{
			"event_id": anyToString(event["id"]), "event_type": anyToString(event["type"]),
			"created_at": anyToString(event["created_at"]), "summary": summary,
		})
	}
	return out
}

func frontierT9Scope(session map[string]any) frontierT6Scope {
	return frontierT6Scope{
		Project:   agentSessionProject(session),
		SessionID: anyToString(session["id"]), AgentID: anyToString(session["agent_id"]),
	}
}

func (s *server) frontierT9ProfileReference(session map[string]any, now time.Time) (map[string]any, string) {
	agentID := anyToString(session["agent_id"])
	if agentID == "" {
		return map[string]any{"state": "absent"}, "agent_profile_identity_absent"
	}
	scope := frontierT9Scope(session)
	if scope.WorkspaceID == "" && s != nil {
		if authorization, err := s.frontierT6OwnerAuthorization(nil, frontierT6AgentContextFeatureID, "resolve"); err == nil {
			scope.WorkspaceID = authorization.WorkspaceID
		}
	}
	var stored *frontierT6StoredAgentProfile
	if s != nil && s.frontierT6 != nil && s.frontierT6.enabled {
		if candidate, found, err := s.frontierT6.agentProfile(scope, agentID); err == nil && found {
			stored = &candidate
		}
	}
	resolved, err := frontierT6ResolveAgentContextProfile(frontierT6ProfileResolutionRequest{
		Scope: scope, AgentID: agentID, Stored: stored, Now: now,
	})
	if err != nil {
		return map[string]any{"state": "unavailable"}, "agent_profile_resolution_failed"
	}
	state := "generic_default"
	profileID := ""
	if stored != nil && resolved.StoredProfileUsed {
		state, profileID = "resolved", stored.ProfileID
	}
	return map[string]any{
		"state": state, "profile_id": profileID, "profile_digest": resolved.ProfileDigest,
		"stored_profile_used": resolved.StoredProfileUsed, "cold_start": resolved.ColdStart,
		"decision": resolved.Decision,
	}, ""
}

func (s *server) frontierT9PreparationReference(session map[string]any, profileDigest string, now time.Time) (map[string]any, string) {
	if s == nil || s.frontierT6 == nil || !s.frontierT6.enabled {
		return map[string]any{"state": "absent"}, "context_preparation_unavailable"
	}
	scope := frontierT9Scope(session)
	if scope.WorkspaceID == "" {
		if authorization, err := s.frontierT6OwnerAuthorization(nil, frontierT6ProactiveContextPrepFeatureID, "use"); err == nil {
			scope.WorkspaceID = authorization.WorkspaceID
		}
	}
	taskID := anyToString(session["task_id"])
	s.frontierT6.mu.RLock()
	defer s.frontierT6.mu.RUnlock()
	var selected *frontierT6ContextPrepRecord
	for _, candidate := range s.frontierT6.state.ContextPreps {
		if candidate.Status != "ready" || candidate.Artifact == nil || candidate.Scope != scope {
			continue
		}
		if taskID != "" && candidate.TaskID != "" && candidate.TaskID != taskID {
			continue
		}
		if profileDigest != "" && candidate.EffectiveProfileDigest != profileDigest {
			continue
		}
		expiresAt, ok := frontierT6ParseTime(candidate.ExpiresAt)
		artifactExpires, artifactOK := frontierT6ParseTime(candidate.Artifact.ExpiresAt)
		if !ok || !artifactOK || !now.Before(expiresAt) || !now.Before(artifactExpires) {
			continue
		}
		copyCandidate := candidate
		if selected == nil || copyCandidate.UpdatedAt > selected.UpdatedAt {
			selected = &copyCandidate
		}
	}
	if selected == nil {
		return map[string]any{"state": "absent"}, "context_preparation_absent"
	}
	return map[string]any{
		"state": "ready", "prep_id": selected.PrepID, "artifact_id": selected.Artifact.ArtifactID,
		"artifact_digest":           frontierT6Digest(selected.Artifact),
		"context_pack_digest":       selected.Artifact.ContextPackDigest,
		"retrieval_receipt_digest":  selected.Artifact.RetrievalReceiptDigest,
		"expires_at":                selected.Artifact.ExpiresAt,
		"consumption_state":         "not_consumed",
		"requires_explicit_cli_use": true,
	}, ""
}

func (s *server) frontierT9PassportReference(request frontierT9ContinuityZeroRequest, session map[string]any, now time.Time) (map[string]any, string, bool) {
	if s == nil || s.contextPassports == nil || !s.contextPassports.enabled {
		return map[string]any{"state": "absent"}, "context_passport_unavailable", false
	}
	var candidates []contextPassport
	s.contextPassports.mu.RLock()
	if request.PassportID != "" {
		if passport, exists := s.contextPassports.passports[request.PassportID]; exists {
			candidates = append(candidates, passport)
		}
	} else {
		for index := len(s.contextPassports.order) - 1; index >= 0; index-- {
			passport := s.contextPassports.passports[s.contextPassports.order[index]]
			if passport.Project != request.Project {
				continue
			}
			lineage := passport.Lineage
			if sessionID := anyToString(lineage["session_id"]); sessionID != "" && sessionID != anyToString(session["id"]) {
				continue
			}
			candidates = append(candidates, passport)
		}
	}
	s.contextPassports.mu.RUnlock()
	if len(candidates) == 0 {
		if request.PassportID != "" {
			return map[string]any{"state": "rejected"}, "context_passport_not_found", true
		}
		return map[string]any{"state": "absent"}, "context_passport_absent", false
	}
	passport := candidates[0]
	if findings := verifyContextPassport(passport, now, false); len(findings) > 0 {
		return map[string]any{"state": "rejected", "passport_id": passport.PassportID}, "context_passport_invalid_or_expired", true
	}
	if passport.Project != request.Project {
		return map[string]any{"state": "rejected", "passport_id": passport.PassportID}, "context_passport_project_mismatch", true
	}
	if branch := anyToString(passport.Lineage["branch"]); branch != "" && request.Branch != "" && branch != request.Branch {
		return map[string]any{"state": "rejected", "passport_id": passport.PassportID}, "context_passport_branch_mismatch", true
	}
	if commit := strings.ToLower(anyToString(passport.Lineage["commit"])); commit != "" && commit != request.Commit {
		return map[string]any{"state": "rejected", "passport_id": passport.PassportID}, "context_passport_commit_mismatch", true
	}
	return map[string]any{
		"state": "verified", "passport_id": passport.PassportID, "lineage_id": passport.LineageID,
		"revision": passport.Revision, "content_digest": passport.ContentDigest,
		"expires_at": passport.ExpiresAt, "signing_key_id": passport.Issuer.SigningKeyID,
	}, "", false
}

func (s *server) frontierT9GrantReference(request frontierT9ContinuityZeroRequest, now time.Time) (map[string]any, string, bool) {
	if request.MeshGrantID == "" {
		return map[string]any{"state": "not_requested"}, "", false
	}
	if s == nil || s.contextMesh == nil {
		return map[string]any{"state": "rejected"}, "context_grant_unavailable", true
	}
	grant, err := s.contextMesh.activeGrant(request.MeshGrantID, request.Project, now)
	if err != nil {
		return map[string]any{"state": "rejected", "grant_id": request.MeshGrantID}, "context_grant_revoked_or_invalid", true
	}
	return map[string]any{
		"state": "active", "grant_id": grant.GrantID, "grant_digest": frontierT6Digest(grant),
		"expires_at": grant.ExpiresAt,
	}, "", false
}

func frontierT9RepositoryReference(repositories []string, branch, commit string) map[string]any {
	digests := make([]any, 0, len(repositories))
	for _, repository := range repositories {
		digests = append(digests, frontierT6Digest(map[string]any{"repository_id": repository}))
	}
	return map[string]any{
		"repository_ref":     "repo_" + strings.TrimPrefix(anyToString(digests[0]), "sha256:")[:24],
		"repository_digests": digests, "branch": branch, "commit": commit,
		"identity_source": "explicit_cli_git_evidence", "local_path_transmitted": false,
	}
}

func frontierT9BaseResponse(request frontierT9ContinuityZeroRequest, repositories []string, decision string, reasons, candidates []string) map[string]any {
	candidateRefs := make([]any, 0, len(candidates))
	for _, id := range candidates {
		candidateRefs = append(candidateRefs, map[string]any{"session_id": id, "session_ref": "session_" + digestPrefix(id, 20)})
	}
	return map[string]any{
		"ok": decision == "ready", "schema_id": frontierT9ContinuityZeroSchemaID, "version": 1,
		"decision": decision, "reasons": reasons, "generated_at": nowUTCISO(),
		"project": request.Project, "agent_id": request.AgentID, "harness": request.Harness,
		"repository":         frontierT9RepositoryReference(repositories, request.Branch, request.Commit),
		"candidate_sessions": candidateRefs,
		"session":            map[string]any{"state": "unselected"}, "checkpoint": map[string]any{"state": "absent"},
		"effective_profile": map[string]any{"state": "absent"}, "preparation": map[string]any{"state": "absent"},
		"passport": map[string]any{"state": "absent"}, "context_grant": map[string]any{"state": "not_requested"},
		"continuity": map[string]any{"objective": "", "current_state": "", "delta": []any{}, "risks": reasons, "next_move": "Use ordinary bootstrap and context-pack, then select an explicit session if needed."},
		"safety": map[string]any{
			"advisory_only": true, "automatic_model_execution": false, "runner_dispatch": false,
			"filesystem_mutation": false, "network_calls": 0, "transport_performed": false,
			"hidden_session_creation": false, "local_path_returned": false, "requires_writeback": true,
		},
		"measurement": map[string]any{"tool_calls": 1, "model_calls": 0, "network_calls": 0},
	}
}

func (s *server) frontierT9BuildContinuityZero(r *http.Request, request frontierT9ContinuityZeroRequest, repositories []string, now time.Time) map[string]any {
	selection := s.frontierT9SelectSession(r, request, repositories)
	payload := frontierT9BaseResponse(request, repositories, selection.Decision, selection.Reasons, selection.Candidates)
	if selection.Decision != "ready" {
		return frontierT9FinalizeResponse(payload)
	}

	session, events := selection.Session, selection.Events
	if request.AgentID == "" {
		request.AgentID = anyToString(session["agent_id"])
		payload["agent_id"] = request.AgentID
	}
	if request.Harness == "generic" {
		if harness := frontierT9SessionHarness(session); harness != "" {
			request.Harness = harness
			payload["harness"] = harness
		}
	}
	packet := buildAgentSessionPacket(session, events, now)
	rollup := buildAgentSessionRollup(session, events, now)
	checkpoint := frontierT9LatestCheckpoint(events)
	profile, profileRisk := s.frontierT9ProfileReference(session, now)
	preparation, prepRisk := s.frontierT9PreparationReference(session, anyToString(profile["profile_digest"]), now)
	passport, passportRisk, passportReject := s.frontierT9PassportReference(request, session, now)
	grant, grantRisk, grantReject := s.frontierT9GrantReference(request, now)
	risks := []string{}
	for _, risk := range []string{profileRisk, prepRisk, passportRisk, grantRisk} {
		if risk != "" {
			risks = append(risks, risk)
		}
	}
	if anyToString(checkpoint["state"]) == "absent" {
		risks = append(risks, "checkpoint_absent")
	}
	risks = uniqueSortedStrings(risks)
	decision := "ready"
	if passportReject || grantReject {
		decision, payload["ok"] = "rejected", false
	}
	payload["decision"] = decision
	payload["reasons"] = risks
	payload["session"] = map[string]any{
		"state": "selected", "session_id": anyToString(session["id"]),
		"session_ref":   "session_" + digestPrefix(anyToString(session["id"]), 20),
		"status":        normalizeAgentSessionStatus(anyToString(session["status"])),
		"packet_digest": frontierT6Digest(packet), "rollup_digest": frontierT6Digest(rollup),
		"packet_schema_id": agentPacketContractID,
	}
	payload["checkpoint"] = checkpoint
	payload["effective_profile"] = profile
	payload["preparation"] = preparation
	payload["passport"] = passport
	payload["context_grant"] = grant
	objective := frontierT9PortableString(clipText(firstNonEmptyStrings(anyToString(rollup["objective"]), anyToString(rollup["goal"])), 1200), &portableRedactionStats{})
	nextMove := frontierT9PortableString(clipText(firstNonEmptyStrings(anyToString(rollup["next_action"]), "Verify current local state and execute the smallest evidence-backed next action."), 720), &portableRedactionStats{})
	payload["continuity"] = map[string]any{
		"objective": objective, "current_state": anyToString(rollup["objective_state"]),
		"delta": frontierT9Delta(events), "risks": risks, "next_move": nextMove,
		"agent_lifecycle": map[string]any{
			"state":      anyToString(anyMap(rollup["agent_lifecycle"])["state"]),
			"authority":  anyToString(anyMap(rollup["agent_lifecycle"])["authority"]),
			"source":     anyToString(anyMap(rollup["agent_lifecycle"])["source"]),
			"updated_at": anyToString(anyMap(rollup["agent_lifecycle"])["updated_at"]),
		},
	}
	return frontierT9FinalizeResponse(payload)
}

func frontierT9FinalizeResponse(payload map[string]any) map[string]any {
	redactions := &portableRedactionStats{}
	if sanitized, ok := frontierT9PortableValue(payload, 0, redactions).(map[string]any); ok {
		payload = sanitized
	}
	measurement := anyMap(payload["measurement"])
	measurement["redaction"] = map[string]any{
		"applied":     redactions.SecretKeys+redactions.Tokens+redactions.Paths > 0,
		"secret_keys": redactions.SecretKeys, "tokens": redactions.Tokens,
		"paths": redactions.Paths, "clipped": redactions.Clipped,
	}
	encoded, _ := json.Marshal(payload)
	measurement["manifest_bytes_before_contract"] = len(encoded)
	measurement["manifest_tokens_estimate"] = (len(encoded) + 3) / 4
	manifestMaterial := cloneAnyMap(payload)
	delete(manifestMaterial, "manifest_id")
	delete(manifestMaterial, "manifest_digest")
	delete(manifestMaterial, "format_contract")
	digest := frontierT6Digest(manifestMaterial)
	payload["manifest_digest"] = digest
	payload["manifest_id"] = "czero_" + strings.TrimPrefix(digest, "sha256:")[:24]
	return attachPayloadFormatContract(frontierT9ContinuityZeroSchemaID, payload, anyToString(payload["agent_id"]), "continuity_zero", frontierT9ContinuityZeroPath)
}

func frontierT9PortableValue(value any, depth int, stats *portableRedactionStats) any {
	if depth > 8 {
		stats.Clipped++
		return "[depth-clipped]"
	}
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(keys))
		for _, key := range keys {
			if portableSecretKey(key) {
				stats.SecretKeys++
				continue
			}
			out[key] = frontierT9PortableValue(typed[key], depth+1, stats)
		}
		return out
	case []any:
		limit := minInt(len(typed), 64)
		out := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			out = append(out, frontierT9PortableValue(item, depth+1, stats))
		}
		if len(typed) > limit {
			stats.Lists += len(typed) - limit
		}
		return out
	case []map[string]any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
		return frontierT9PortableValue(items, depth, stats)
	case []string:
		limit := minInt(len(typed), 64)
		items := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			items = append(items, frontierT9PortableValue(item, depth+1, stats))
		}
		if len(typed) > limit {
			stats.Lists += len(typed) - limit
		}
		return items
	case string:
		return frontierT9PortableString(typed, stats)
	case json.Number, float64, float32, int, int64, int32, uint, uint64, uint32, bool, nil:
		return typed
	default:
		return frontierT9PortableValue(anyToString(typed), depth, stats)
	}
}

func frontierT9PortableString(value string, stats *portableRedactionStats) string {
	redacted := frontierT9PathPattern.ReplaceAllString(value, "[local-path]")
	if redacted != value {
		stats.Paths++
	}
	return portableString(redacted, stats)
}

func frontierT9DecodeRequest(r *http.Request) (frontierT9ContinuityZeroRequest, error) {
	var request frontierT9ContinuityZeroRequest
	raw, err := io.ReadAll(io.LimitReader(r.Body, frontierT9MaxRequestBytes+1))
	if err != nil {
		return request, errors.New("read continuity-zero request failed")
	}
	if len(raw) > frontierT9MaxRequestBytes {
		return request, errors.New("continuity-zero request exceeds the input limit")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return request, errors.New("continuity-zero request body is required")
	}
	if err := strictJSONDecode(raw, &request); err != nil {
		return request, fmt.Errorf("invalid continuity-zero request: %w", err)
	}
	return request, nil
}

func (s *server) memoryContinuityZero(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	request, err := frontierT9DecodeRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_continuity_zero_request", "detail": err.Error()})
		return
	}
	request, repositories, validationErr := frontierT9ValidateRequest(request)
	if validationErr != nil && validationErr.Error() != "unsupported harness" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid_continuity_zero_request", "detail": validationErr.Error()})
		return
	}
	var payload map[string]any
	if validationErr != nil {
		payload = frontierT9BaseResponse(request, repositories, "rejected", []string{"unsupported_harness"}, nil)
		payload = frontierT9FinalizeResponse(payload)
	} else {
		payload = s.frontierT9BuildContinuityZero(r, request, repositories, time.Now().UTC())
	}
	encoded, _ := json.Marshal(payload)
	validation := anyMap(anyMap(payload["format_contract"])["validation"])
	if len(encoded) > frontierT9MaxResponseBytes || anyToString(validation["status"]) != "passed" {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"ok": false, "error": "continuity_zero_contract_failed",
			"detail": validation["errors"], "response_bytes": len(encoded),
		})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}
