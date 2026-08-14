package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type agentTaskRouteAuth struct {
	Principal                      string
	Role                           string
	Workspace                      string
	CanonicalWorkerID              string
	WorkerInstanceID               string
	WorkerInstanceCredential       string
	WorkerIdentityUpdateGeneration int
	Signed                         bool
	Service                        bool
	// RequestBound is set only by authenticateAgentTaskRoute. Direct
	// in-process ledger tests construct this value without an HTTP proof and
	// remain useful for migration compatibility; every external signed route
	// is proof-bound before it can inspect or mutate a fence.
	RequestBound bool
}

type agentTaskRouteAuthorizationContextKey struct{}

// withAgentTaskRouteAuthorization binds a trusted in-process authorization to
// one request context. Network callers cannot construct this value; public and
// paid middleware may use it only after validating their own credential.
func withAgentTaskRouteAuthorization(r *http.Request, auth agentTaskRouteAuth) *http.Request {
	if r == nil {
		return nil
	}
	auth.RequestBound = true
	return r.WithContext(context.WithValue(r.Context(), agentTaskRouteAuthorizationContextKey{}, auth))
}

func agentTaskRouteAuthorizationFromRequest(r *http.Request) (agentTaskRouteAuth, bool) {
	if r == nil {
		return agentTaskRouteAuth{}, false
	}
	auth, ok := r.Context().Value(agentTaskRouteAuthorizationContextKey{}).(agentTaskRouteAuth)
	return auth, ok && auth.RequestBound
}

func (s *server) authorizeAgentTaskProject(workspaceID, project string) error {
	workspaceID = strings.TrimSpace(workspaceID)
	project = strings.TrimSpace(project)
	if workspaceID == "" || project == "" {
		return errors.New("authenticated workspace and task project are required")
	}
	boundWorkspace, err := s.resolveAgentTaskProjectWorkspace(project)
	if err != nil {
		return err
	}
	if !strings.EqualFold(boundWorkspace, workspaceID) {
		return errors.New("task project is bound to a different workspace")
	}
	return nil
}

func (s *server) resolveAgentTaskProjectWorkspace(project string) (string, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		return "", errors.New("task project is required")
	}
	if s != nil && s.taskProjectWorkspace != nil {
		return s.taskProjectWorkspace(project)
	}
	return optionalAgentTaskProjectWorkspace(s, project)
}

func (a agentTaskRouteAuth) authorizeFence(fence agentTaskFence) error {
	expectedWorker := strings.TrimSpace(a.Principal)
	if a.Signed {
		if strings.TrimSpace(a.CanonicalWorkerID) == "" {
			return errors.New("authenticated worker identity mapping is required")
		}
		expectedWorker = strings.TrimSpace(a.CanonicalWorkerID)
	}
	if a.Signed && !strings.EqualFold(strings.TrimSpace(fence.WorkerID), expectedWorker) {
		return errors.New("worker fence identity does not match authenticated principal")
	}
	if a.Signed && strings.TrimSpace(a.WorkerInstanceID) != "" && fence.WorkerInstanceID != a.WorkerInstanceID {
		return errors.New("worker fence instance does not match authenticated worker instance")
	}
	if a.Signed && fence.WorkerIdentityUpdateGeneration != a.WorkerIdentityUpdateGeneration {
		return errors.New("worker fence identity update generation does not match authenticated worker identity")
	}
	return nil
}

func (s *server) authorizeAgentTaskFence(ctx context.Context, auth *agentTaskRouteAuth, fence agentTaskFence) error {
	if auth == nil {
		return errors.New("task route authorization is unavailable")
	}
	if auth.Signed && s != nil && s.taskLedger != nil {
		if auth.RequestBound {
			if strings.TrimSpace(auth.WorkerInstanceID) == "" {
				return errors.New("worker instance credential requires an explicit worker instance")
			}
		}
		if strings.TrimSpace(fence.WorkerInstanceID) == "" {
			return errors.New("worker_instance_id is required for an authenticated worker fence")
		}
		// Once a signed request is bound to an explicit worker instance, a
		// same-principal fence from another instance is foreign authority. Do
		// this comparison before looking up the fence instance; otherwise the
		// lookup would silently rebind the request to whichever instance the
		// caller copied into the fence.
		if auth.RequestBound && strings.TrimSpace(auth.WorkerInstanceID) != "" && fence.WorkerInstanceID != auth.WorkerInstanceID {
			return errors.New("worker identity foreign-instance fence rejected")
		}
		if auth.RequestBound && strings.TrimSpace(auth.WorkerInstanceCredential) == "" {
			return errors.New("worker instance credential is required")
		}
		identity, err := s.taskLedger.workerIdentityByAuthority(ctx, agentWorkerIdentityAuthority{PrincipalID: auth.Principal, WorkspaceID: auth.Workspace, WorkerInstanceID: fence.WorkerInstanceID})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errWorkerIdentityNotRegistered
			}
			return err
		}
		if identity.Status != "active" {
			return errors.New("worker identity is closed")
		}
		if auth.RequestBound {
			verifiedIdentity, credentialErr := s.taskLedger.verifyAndUpgradeWorkerInstanceCredential(ctx, identity, auth.WorkerInstanceCredential)
			if credentialErr != nil {
				return credentialErr
			}
			identity = verifiedIdentity
		}
		auth.CanonicalWorkerID = identity.CanonicalWorkerID
		if strings.TrimSpace(auth.WorkerInstanceID) == "" {
			auth.WorkerInstanceID = identity.WorkerInstanceID
		}
		auth.WorkerIdentityUpdateGeneration = identity.IdentityUpdateGeneration
	}
	authErr := auth.authorizeFence(fence)
	if authErr == nil {
		return nil
	} else if auth.Signed && s != nil && s.taskLedger != nil {
		// A lease issued before a collision registration carries the requested
		// ID and generation zero. It remains mutable only when every fence
		// field matches that persisted pre-registration attempt row.
		if identityGeneration := auth.WorkerIdentityUpdateGeneration; identityGeneration > 0 && fence.WorkerIdentityUpdateGeneration == 0 {
			identity, identityErr := s.taskLedger.workerIdentityByAuthority(ctx, agentWorkerIdentityAuthority{PrincipalID: auth.Principal, WorkspaceID: auth.Workspace, WorkerInstanceID: fence.WorkerInstanceID})
			if identityErr == nil && strings.EqualFold(fence.WorkerID, identity.RequestedWorkerID) {
				var taskID, leaseID, workerID, workerInstanceID string
				var generation, storedIdentityGeneration int
				lookupErr := s.taskLedger.db.QueryRowContext(ctx, `SELECT task_id,lease_id,generation,worker_id,worker_instance_id,worker_identity_update_generation FROM task_ledger_attempts WHERE attempt_id=?`, fence.AttemptID).Scan(&taskID, &leaseID, &generation, &workerID, &workerInstanceID, &storedIdentityGeneration)
				if lookupErr == nil && taskID == fence.TaskID && leaseID == fence.LeaseID && generation == fence.Generation && strings.EqualFold(workerID, fence.WorkerID) && workerInstanceID == fence.WorkerInstanceID && storedIdentityGeneration == 0 {
					legacy := *auth
					legacy.CanonicalWorkerID = fence.WorkerID
					legacy.WorkerIdentityUpdateGeneration = 0
					if legacyErr := legacy.authorizeFence(fence); legacyErr == nil {
						return nil
					}
				}
			}
		}
		return authErr
	}
	return authErr
}

func (s *server) authorizeAgentWorkerIdentityCredential(ctx context.Context, auth *agentTaskRouteAuth, authority agentWorkerIdentityAuthority) (agentWorkerIdentityRecord, error) {
	if auth == nil {
		return agentWorkerIdentityRecord{}, errors.New("task route authorization is unavailable")
	}
	if auth.Service || !auth.Signed || !auth.RequestBound {
		return agentWorkerIdentityRecord{}, nil
	}
	if strings.TrimSpace(auth.WorkerInstanceID) == "" || auth.WorkerInstanceID != authority.WorkerInstanceID {
		return agentWorkerIdentityRecord{}, errors.New("worker instance authority does not match the authenticated instance")
	}
	if strings.TrimSpace(auth.WorkerInstanceCredential) == "" {
		return agentWorkerIdentityRecord{}, errors.New("worker instance credential is required")
	}
	identity, err := s.taskLedger.workerIdentityByAuthority(ctx, authority)
	if errors.Is(err, sql.ErrNoRows) {
		return agentWorkerIdentityRecord{}, errWorkerIdentityNotRegistered
	}
	if err != nil {
		return agentWorkerIdentityRecord{}, err
	}
	identity, credentialErr := s.taskLedger.verifyAndUpgradeWorkerInstanceCredential(ctx, identity, auth.WorkerInstanceCredential)
	if credentialErr != nil {
		return agentWorkerIdentityRecord{}, credentialErr
	}
	if identity.Status != "active" {
		return agentWorkerIdentityRecord{}, errors.New("worker identity is closed")
	}
	auth.CanonicalWorkerID = identity.CanonicalWorkerID
	auth.WorkerInstanceID = identity.WorkerInstanceID
	auth.WorkerIdentityUpdateGeneration = identity.IdentityUpdateGeneration
	return identity, nil
}

func (s *server) authorizeTaskResource(ctx context.Context, taskID string, auth agentTaskRouteAuth) error {
	if s == nil || s.taskLedger == nil {
		return errors.New("authoritative task ledger unavailable")
	}
	task, err := s.taskLedger.queryTask(ctx, strings.TrimSpace(taskID))
	if err != nil {
		return err
	}
	boundWorkspace, err := s.resolveAgentTaskProjectWorkspace(anyToString(task["project"]))
	if err != nil {
		return err
	}
	if !strings.EqualFold(anyToString(task["workspace_id"]), boundWorkspace) {
		return errors.New("task project is outside its current workspace binding")
	}
	if auth.Service {
		return nil
	}
	if !strings.EqualFold(boundWorkspace, auth.Workspace) {
		return errors.New("task project is outside authenticated workspace")
	}
	if !agentTaskTaskAllowsPrincipal(task, auth.Principal) {
		return errors.New("task resource is not authorized for authenticated principal")
	}
	return nil
}

// authorizeTaskWorkerFence grants a signed worker access only to the exact
// server-owned attempt it currently holds. Workers are deliberately not task
// recipients, so recipient authorization cannot be reused for heartbeat,
// observation, cancellation, publication, or restart reconciliation.
func (s *server) authorizeTaskWorkerFence(ctx context.Context, fence agentTaskFence, auth agentTaskRouteAuth) error {
	if auth.Service {
		return s.authorizeTaskResource(ctx, fence.TaskID, auth)
	}
	if !auth.Signed {
		return errors.New("signed task worker is required")
	}
	if err := auth.authorizeFence(fence); err != nil {
		return err
	}
	task, err := s.taskLedger.queryTask(ctx, fence.TaskID)
	if err != nil {
		return err
	}
	boundWorkspace, err := s.resolveAgentTaskProjectWorkspace(anyToString(task["project"]))
	if err != nil {
		return err
	}
	if !strings.EqualFold(anyToString(task["workspace_id"]), boundWorkspace) || !strings.EqualFold(auth.Workspace, boundWorkspace) {
		return errors.New("task worker fence is outside authenticated workspace")
	}
	tx, err := s.taskLedger.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := s.taskLedger.fenceTx(ctx, tx, fence, false); err != nil {
		return err
	}
	var activeAttemptID string
	if err := tx.QueryRowContext(ctx, `SELECT active_attempt_id FROM task_ledger_tasks WHERE id=?`, fence.TaskID).Scan(&activeAttemptID); err != nil {
		return err
	}
	if strings.TrimSpace(activeAttemptID) != fence.AttemptID {
		return errors.New("stale_lease_fence: worker attempt is not the active task attempt")
	}
	return tx.Commit()
}

func (s *server) claimNextAuthorizedAgentTask(ctx context.Context, workerID, workerInstanceID, workspaceID string) (map[string]any, error) {
	if s == nil || s.taskLedger == nil {
		return nil, errors.New("authoritative task ledger unavailable")
	}
	if err := agentTaskValidateStructured(map[string]any{"worker_id": workerID, "worker_instance_id": workerInstanceID, "workspace_id": workspaceID}, "authorized task claim", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	cursor := agentTaskClaimCursor{}
	const windows = 8 // 8 * 128 proves progress beyond 500 stale governance rows.
	for window := 0; window < windows; window++ {
		candidates, next, err := s.taskLedger.claimCandidateWindow(ctx, workerID, workspaceID, cursor, 128)
		if err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			project := strings.TrimSpace(anyToString(candidate["project"]))
			boundWorkspace, bindingErr := s.resolveAgentTaskProjectWorkspace(project)
			if bindingErr != nil || !strings.EqualFold(boundWorkspace, anyToString(candidate["workspace_id"])) {
				continue
			}
			if strings.TrimSpace(workspaceID) != "" && !strings.EqualFold(boundWorkspace, workspaceID) {
				continue
			}
			claimed, claimErr := s.taskLedger.claimTask(ctx, workerID, workerInstanceID, workspaceID, anyToString(candidate["task_id"]))
			if claimErr != nil {
				return nil, claimErr
			}
			if claimed != nil {
				return claimed, nil
			}
		}
		if len(candidates) < 128 || !next.Set {
			return nil, nil
		}
		cursor = next
	}
	return nil, nil
}

func (s *server) claimNextAuthorizedAgentTaskWithIdentity(ctx context.Context, identity agentWorkerIdentityRecord) (map[string]any, error) {
	if s == nil || s.taskLedger == nil {
		return nil, errors.New("authoritative task ledger unavailable")
	}
	if identity.Status != "active" {
		return nil, errors.New("worker identity is closed")
	}
	if strings.TrimSpace(identity.WorkerInstanceCredentialVerifier) == "" {
		return nil, errors.New("worker instance credential is required; re-register a new instance")
	}
	if identity.IdentityUpdateGeneration != identity.AcknowledgedGeneration {
		return nil, errWorkerIdentityUpdatePending
	}
	workerID := identity.CanonicalWorkerID
	workerInstanceID := identity.WorkerInstanceID
	workspaceID := identity.WorkspaceID
	if err := agentTaskValidateStructured(map[string]any{"worker_id": workerID, "worker_instance_id": workerInstanceID, "workspace_id": workspaceID, "worker_identity_update_generation": identity.IdentityUpdateGeneration}, "authorized task claim", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	cursor := agentTaskClaimCursor{}
	const windows = 8
	for window := 0; window < windows; window++ {
		candidates, next, err := s.taskLedger.claimCandidateWindow(ctx, workerID, workspaceID, cursor, 128)
		if err != nil {
			return nil, err
		}
		for _, candidate := range candidates {
			project := strings.TrimSpace(anyToString(candidate["project"]))
			boundWorkspace, bindingErr := s.resolveAgentTaskProjectWorkspace(project)
			if bindingErr != nil || !strings.EqualFold(boundWorkspace, anyToString(candidate["workspace_id"])) || !strings.EqualFold(boundWorkspace, workspaceID) {
				continue
			}
			claimed, claimErr := s.taskLedger.claimTaskWithIdentity(ctx, workerID, workerInstanceID, workspaceID, anyToString(candidate["task_id"]), identity.IdentityUpdateGeneration)
			if claimErr != nil {
				return nil, claimErr
			}
			if claimed != nil {
				return claimed, nil
			}
		}
		if len(candidates) < 128 || !next.Set {
			return nil, nil
		}
		cursor = next
	}
	return nil, nil
}

func (s *server) authenticateAgentTaskRoute(r *http.Request, operation string) (agentTaskRouteAuth, error) {
	if s == nil || r == nil {
		return agentTaskRouteAuth{}, errors.New("task delivery authentication is unavailable")
	}
	operation = strings.TrimSpace(strings.ToLower(operation))
	auth := agentTaskRouteAuth{}
	credentialRoute := agentWorkerInstanceCredentialRoutePath(r.URL.Path)
	workerInstanceID := strings.TrimSpace(r.Header.Get("X-Worker-Instance-ID"))
	resolved, resolvedOK := agentTaskRouteAuthorizationFromRequest(r)
	if !resolvedOK {
		resolver := optionalAgentTaskSignedRouteAuthorization
		if s.taskSignedRouteAuth != nil {
			resolver = func(_ *server, request *http.Request) (agentTaskRouteAuth, bool, error) {
				return s.taskSignedRouteAuth(request)
			}
		}
		var err error
		resolved, resolvedOK, err = resolver(s, r)
		if err != nil {
			return agentTaskRouteAuth{}, err
		}
	}
	if resolvedOK {
		auth = resolved
		auth.WorkerInstanceID = workerInstanceID
		if credentialRoute {
			auth.WorkerInstanceCredential = r.Header.Get(workerInstanceCredentialHeader)
		}
		auth.Signed = auth.Principal != "" && auth.Workspace != ""
		auth.RequestBound = auth.Signed
	}
	if !auth.Signed {
		// A service operation is authenticated by the configured Gateway API
		// key. Caller-supplied principal/role headers never create this scope.
		provided, explicit := requestAPIKey(r)
		expected := strings.TrimSpace(s.orchestratorAPIKey)
		if expected != "" && explicit && len(provided) == len(expected) && secureTokenEqual(provided, expected) {
			credential := ""
			if credentialRoute {
				credential = r.Header.Get(workerInstanceCredentialHeader)
			}
			auth = agentTaskRouteAuth{Principal: "gateway-service", Role: "service", Service: true, WorkerInstanceID: workerInstanceID, WorkerInstanceCredential: credential, RequestBound: true}
		}
	}
	if auth.Principal == "" {
		return agentTaskRouteAuth{}, errors.New("authenticated task principal is required")
	}
	if auth.Signed && len([]byte(auth.WorkerInstanceCredential)) > workerInstanceCredentialMaxBytes {
		return agentTaskRouteAuth{}, errors.New("worker instance credential exceeds the bounded ingress size")
	}
	switch operation {
	case "reviewer", "recipient":
		if !auth.Signed && !(auth.Service && s.taskServiceOwnerLocalLifecycle) {
			return agentTaskRouteAuth{}, errors.New("signed task principal is required")
		}
	case "operator":
		if !auth.Service && auth.Role != "owner" && auth.Role != "admin" {
			return agentTaskRouteAuth{}, errors.New("operator capability is required")
		}
	case "worker":
		if !auth.Service && !auth.Signed {
			return agentTaskRouteAuth{}, errors.New("worker capability is required")
		}
	case "read", "submit":
		if !auth.Service && !auth.Signed {
			return agentTaskRouteAuth{}, errors.New("task project capability is required")
		}
	}
	return auth, nil
}

func (s *server) agentTaskServiceSessionPrincipal(sessionID, project string) (string, error) {
	sessionID = strings.TrimSpace(sessionID)
	project = strings.TrimSpace(project)
	if s == nil || s.agentSessions == nil || sessionID == "" || project == "" {
		return "", errors.New("owner-local task session authority is unavailable")
	}
	session, _, exists := s.agentSessions.get(sessionID)
	if !exists || !strings.EqualFold(agentSessionProject(session), project) || agentSessionTerminal(anyToString(session["status"])) {
		return "", errors.New("owner-local task session is unavailable or outside the task project")
	}
	principal := strings.TrimSpace(firstNonEmptyStrings(anyToString(session["agent_id"]), anyToString(session["agent"])))
	if principal == "" {
		return "", errors.New("owner-local task session principal is unavailable")
	}
	if err := agentTaskValidateText(principal, "owner-local task session principal", 2048); err != nil {
		return "", err
	}
	return principal, nil
}

func (s *server) bindAgentTaskServiceLifecycleManifest(manifest map[string]any, boundWorkspace string) (map[string]any, error) {
	manifest = cloneAnyMap(manifest)
	boundWorkspace = strings.TrimSpace(boundWorkspace)
	project := strings.TrimSpace(firstNonEmptyStrings(anyToString(manifest["project"]), anyToString(manifest["project_name"]), anyToString(manifest["projectName"])))
	for _, field := range []string{"project", "project_name", "projectName"} {
		value := strings.TrimSpace(anyToString(manifest[field]))
		if value != "" && !strings.EqualFold(value, project) {
			return nil, errors.New("service task project aliases conflict")
		}
	}
	manifest["project"] = project
	delete(manifest, "project_name")
	delete(manifest, "projectName")
	for _, field := range []string{"workspace_id", "workspaceId", "workspace"} {
		value := strings.TrimSpace(anyToString(manifest[field]))
		if value != "" && !strings.EqualFold(value, boundWorkspace) {
			return nil, errors.New("service task workspace aliases conflict with the project binding")
		}
	}
	manifest["workspace_id"] = boundWorkspace
	delete(manifest, "workspaceId")
	delete(manifest, "workspace")
	contextRequest := cloneAnyMap(anyMap(manifest["context_request"]))
	contextSessionID := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(contextRequest["session_id"]),
		anyToString(contextRequest["sessionId"]),
	))
	for _, field := range []string{"session_id", "sessionId"} {
		value := strings.TrimSpace(anyToString(contextRequest[field]))
		if value != "" && value != contextSessionID {
			return nil, errors.New("service task context session aliases conflict")
		}
	}
	contextRequest["session_id"] = contextSessionID
	delete(contextRequest, "sessionId")
	manifest["context_request"] = contextRequest
	principal, err := s.agentTaskServiceSessionPrincipal(contextSessionID, project)
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"review_owner", "reviewOwner", "canonical_reviewer", "canonicalReviewer", "reviewer", "requesting_agent_id", "requestingAgentId"} {
		value := strings.TrimSpace(anyToString(manifest[field]))
		if value != "" && !strings.EqualFold(value, principal) {
			return nil, errors.New("service task lifecycle identity is outside the owner-local workspace authority")
		}
	}
	recipients := []any{}
	hasReviewerSession := false
	seenRecipientPrincipals := map[string]bool{}
	if raw, present := manifest["recipients"]; present {
		rows, ok := raw.([]any)
		if !ok {
			return nil, errors.New("service task recipients must be a list")
		}
		for _, rawRow := range rows {
			row := cloneAnyMap(anyMap(rawRow))
			if len(row) == 0 {
				text, ok := rawRow.(string)
				if !ok || strings.TrimSpace(text) == "" {
					return nil, errors.New("service task recipient must be an object")
				}
				row = map[string]any{"principal_id": strings.TrimSpace(text)}
			}
			recipientSessionID := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["session_id"]), anyToString(row["sessionId"])))
			if recipientSessionID == "" {
				recipientSessionID = contextSessionID
			}
			for _, field := range []string{"session_id", "sessionId"} {
				value := strings.TrimSpace(anyToString(row[field]))
				if value != "" && value != recipientSessionID {
					return nil, errors.New("service task recipient session aliases conflict")
				}
			}
			recipientPrincipal, recipientErr := s.agentTaskServiceSessionPrincipal(recipientSessionID, project)
			if recipientErr != nil {
				return nil, recipientErr
			}
			recipientKey := strings.ToLower(recipientPrincipal)
			if seenRecipientPrincipals[recipientKey] {
				return nil, errors.New("service task recipients contain a duplicate canonical principal")
			}
			seenRecipientPrincipals[recipientKey] = true
			for _, field := range []string{"principal_id", "principalId", "principal", "id"} {
				value := strings.TrimSpace(anyToString(row[field]))
				if value != "" && !strings.EqualFold(value, recipientPrincipal) {
					return nil, errors.New("service task recipient is outside the owner-local workspace authority")
				}
			}
			for _, field := range []string{"project", "project_name", "projectName"} {
				value := strings.TrimSpace(anyToString(row[field]))
				if value != "" && !strings.EqualFold(value, project) {
					return nil, errors.New("service task recipient project does not match the task project")
				}
			}
			for _, field := range []string{"workspace_id", "workspaceId", "workspace"} {
				value := strings.TrimSpace(anyToString(row[field]))
				if value != "" && !strings.EqualFold(value, boundWorkspace) {
					return nil, errors.New("service task recipient workspace does not match the task workspace")
				}
			}
			row["principal_id"] = recipientPrincipal
			row["project"] = project
			row["session_id"] = recipientSessionID
			delete(row, "principal")
			delete(row, "principalId")
			delete(row, "id")
			delete(row, "sessionId")
			delete(row, "project_name")
			delete(row, "projectName")
			delete(row, "workspace_id")
			delete(row, "workspaceId")
			delete(row, "workspace")
			recipients = append(recipients, row)
			if strings.EqualFold(recipientPrincipal, principal) && recipientSessionID == contextSessionID {
				hasReviewerSession = true
			}
		}
	}
	if !hasReviewerSession {
		if seenRecipientPrincipals[strings.ToLower(principal)] {
			return nil, errors.New("service task recipients bind the reviewer principal to a different session")
		}
		recipients = append(recipients, map[string]any{
			"principal_id": principal,
			"role":         "reviewer",
			"project":      project,
			"observer":     false,
			"session_id":   contextSessionID,
		})
	}
	manifest["review_owner"] = principal
	manifest["requesting_agent_id"] = principal
	manifest["recipients"] = recipients
	delete(manifest, "canonical_reviewer")
	delete(manifest, "canonicalReviewer")
	delete(manifest, "reviewer")
	delete(manifest, "reviewOwner")
	delete(manifest, "requestingAgentId")
	return manifest, nil
}

func (s *server) agentTaskReviewerActor(ctx context.Context, taskID string, auth agentTaskRouteAuth) (string, error) {
	if !auth.Service {
		return auth.Principal, nil
	}
	if s == nil || s.taskLedger == nil || !s.taskServiceOwnerLocalLifecycle {
		return "", errors.New("owner-local task reviewer authority is unavailable")
	}
	task, err := s.taskLedger.queryTask(ctx, strings.TrimSpace(taskID))
	if err != nil {
		return "", err
	}
	if err := s.authorizeTaskResource(ctx, taskID, auth); err != nil {
		return "", err
	}
	reviewOwner := strings.TrimSpace(anyToString(task["review_owner"]))
	requestingAgentID := strings.TrimSpace(anyToString(task["requesting_agent_id"]))
	if reviewOwner == "" || !strings.EqualFold(reviewOwner, requestingAgentID) {
		return "", errors.New("owner-local task reviewer binding is invalid")
	}
	return reviewOwner, nil
}

func (s *server) agentTaskRecipientActor(ctx context.Context, taskID, deliveryID string, auth agentTaskRouteAuth) (string, error) {
	if !auth.Service {
		return auth.Principal, nil
	}
	if s == nil || s.taskLedger == nil || !s.taskServiceOwnerLocalLifecycle {
		return "", errors.New("owner-local task recipient authority is unavailable")
	}
	if err := s.authorizeTaskResource(ctx, taskID, auth); err != nil {
		return "", err
	}
	deliveries, err := s.taskLedger.deliveries(ctx, taskID, "")
	if err != nil {
		return "", err
	}
	for _, delivery := range deliveries {
		if anyToString(delivery["delivery_id"]) != strings.TrimSpace(deliveryID) {
			continue
		}
		recipient := anyMap(delivery["recipient"])
		recipientID := strings.TrimSpace(anyToString(delivery["recipient_id"]))
		if recipientID == "" || !strings.EqualFold(recipientID, anyToString(recipient["principal_id"])) {
			return "", errors.New("owner-local task recipient binding is invalid")
		}
		return recipientID, nil
	}
	return "", errors.New("task delivery record not found")
}

func agentTaskRouteOperation(path string, method string) string {
	path = canonicalAgentTaskPath(path)
	if isAgentWorkerIdentityPath(path) {
		return "worker"
	}
	if strings.Contains(path, "/artifacts/") || strings.HasSuffix(path, "/ack") || strings.HasSuffix(path, "/answer") {
		return "recipient"
	}
	if strings.HasSuffix(path, "/review") || strings.HasSuffix(path, "/review-claim") || strings.HasSuffix(path, "/approval") || strings.HasSuffix(path, "/integrate") || strings.HasSuffix(path, "/approve") {
		return "reviewer"
	}
	if strings.HasSuffix(path, "/recover-leases") || strings.HasSuffix(path, "/migrate") || strings.HasSuffix(path, "/runtime") || strings.HasSuffix(path, "/deadletter") || strings.HasSuffix(path, "/deliver") || strings.HasSuffix(path, "/finalize") || strings.HasSuffix(path, "/termination") {
		return "operator"
	}
	if strings.HasSuffix(path, "/heartbeat") || strings.HasSuffix(path, "/observe") || strings.HasSuffix(path, "/status") || strings.HasSuffix(path, "/cancel") || strings.HasSuffix(path, "/publish") || strings.HasSuffix(path, "/publication") || strings.HasSuffix(path, "/cleanup") || path == "/agents/tasks/next" {
		return "worker"
	}
	if method == http.MethodPost && path == "/agents/tasks" {
		return "submit"
	}
	return "read"
}

func isAgentWorkerIdentityPath(path string) bool {
	path = canonicalAgentTaskPath(path)
	for _, prefix := range []string{"/agents/workers/", "/agents/tasks/worker/", "/agents/tasks/workers/"} {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

func consistentAgentWorkerRouteValue(field string, caseInsensitive bool, values ...string) (string, error) {
	chosen := ""
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if chosen == "" {
			chosen = value
			continue
		}
		matches := value == chosen
		if caseInsensitive {
			matches = strings.EqualFold(value, chosen)
		}
		if !matches {
			return "", fmt.Errorf("service worker authority field %s was supplied inconsistently", field)
		}
	}
	return chosen, nil
}

func strictRouteStringField(payload map[string]any, field string) (string, bool, error) {
	raw, present := payload[field]
	if !present {
		return "", false, nil
	}
	value, ok := raw.(string)
	if !ok {
		return "", true, fmt.Errorf("worker identity field %s must be a string", field)
	}
	return strings.TrimSpace(value), true, nil
}

func validateStrictRouteStringFields(payload map[string]any, fields ...string) error {
	for _, field := range fields {
		if _, _, err := strictRouteStringField(payload, field); err != nil {
			return err
		}
	}
	return nil
}

func strictAgentTaskFenceRawValue(payload map[string]any, key string) (any, bool, error) {
	if payload == nil {
		return nil, false, nil
	}
	top, topPresent := payload[key]
	nestedRaw, nestedPresent := payload["fence"]
	if !nestedPresent {
		return top, topPresent, nil
	}
	nested, ok := nestedRaw.(map[string]any)
	if !ok {
		return nil, false, errors.New("task fence must be an object")
	}
	nestedValue, nestedKeyPresent := nested[key]
	if topPresent && nestedKeyPresent && !agentTaskCanonicalMapEqual(map[string]any{"value": top}, map[string]any{"value": nestedValue}) {
		return nil, false, fmt.Errorf("task fence field %s was supplied inconsistently", key)
	}
	if topPresent {
		return top, true, nil
	}
	return nestedValue, nestedKeyPresent, nil
}

func strictAgentTaskFenceString(payload map[string]any, field string, caseInsensitive bool, aliases ...string) (string, error) {
	values := make([]string, 0, 1+len(aliases))
	for _, key := range append([]string{field}, aliases...) {
		raw, present, err := strictAgentTaskFenceRawValue(payload, key)
		if err != nil {
			return "", err
		}
		if !present {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return "", fmt.Errorf("task fence field %s must be a string", key)
		}
		values = append(values, strings.TrimSpace(value))
	}
	return consistentAgentWorkerRouteValue(field, caseInsensitive, values...)
}

func strictAgentTaskFenceInteger(payload map[string]any, field string) (int, error) {
	raw, present, err := strictAgentTaskFenceRawValue(payload, field)
	if err != nil {
		return 0, err
	}
	if !present {
		return 0, nil
	}
	value, valid := strictWorkerIdentityInteger(raw)
	if !valid || value < 0 {
		return 0, fmt.Errorf("task fence field %s must be a nonnegative integer", field)
	}
	return value, nil
}

func canonicalAgentTaskPath(path string) string {
	path = strings.TrimSpace(path)
	if strings.HasPrefix(path, "/v1/") {
		path = strings.TrimPrefix(path, "/v1")
	}
	if path == "/v1" {
		return "/"
	}
	return path
}

func readAgentTaskPayload(r *http.Request) (map[string]any, error) {
	const maxTaskRequestBytes int64 = 32 * 1024 * 1024
	if r == nil || r.Body == nil {
		return map[string]any{}, nil
	}
	if r.ContentLength > maxTaskRequestBytes {
		return nil, errors.New("task delivery request exceeds bounded ingress size")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTaskRequestBytes+1))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxTaskRequestBytes {
		return nil, errors.New("task delivery request exceeds bounded ingress size")
	}
	return parseJSONMap(body)
}

func agentTaskPayloadValue(payload map[string]any, key string) any {
	if payload == nil {
		return nil
	}
	if value, ok := payload[key]; ok {
		return value
	}
	if nested := anyMap(payload["fence"]); nested != nil {
		return nested[key]
	}
	return nil
}

func agentTaskFenceFromRequest(taskID string, payload map[string]any) (agentTaskFence, error) {
	attemptID, err := strictAgentTaskFenceString(payload, "attempt_id", false)
	if err != nil {
		return agentTaskFence{}, err
	}
	leaseID, err := strictAgentTaskFenceString(payload, "lease_id", false)
	if err != nil {
		return agentTaskFence{}, err
	}
	workerID, err := strictAgentTaskFenceString(payload, "worker_id", true, "worker")
	if err != nil {
		return agentTaskFence{}, err
	}
	workerInstanceID, err := strictAgentTaskFenceString(payload, "worker_instance_id", false, "instance_id")
	if err != nil {
		return agentTaskFence{}, err
	}
	generation, err := strictAgentTaskFenceInteger(payload, "generation")
	if err != nil {
		return agentTaskFence{}, err
	}
	identityGeneration, err := strictAgentTaskFenceInteger(payload, "worker_identity_update_generation")
	if err != nil {
		return agentTaskFence{}, err
	}
	fence := agentTaskFence{
		TaskID:                         strings.TrimSpace(taskID),
		AttemptID:                      attemptID,
		LeaseID:                        leaseID,
		WorkerID:                       workerID,
		WorkerInstanceID:               workerInstanceID,
		Generation:                     generation,
		WorkerIdentityUpdateGeneration: identityGeneration,
	}
	if fence.TaskID == "" || fence.AttemptID == "" || fence.LeaseID == "" || fence.WorkerID == "" || fence.WorkerInstanceID == "" || fence.Generation <= 0 {
		return agentTaskFence{}, errors.New("complete task, attempt, lease, worker, worker instance, and generation fence is required")
	}
	return fence, nil
}

func agentTaskCleanupFenceFromRequest(taskID, expectedAttemptID string, payload map[string]any) (agentTaskFence, error) {
	fence, err := agentTaskFenceFromRequest(taskID, payload)
	if err != nil {
		return agentTaskFence{}, err
	}
	if expectedAttemptID != "" && fence.AttemptID != strings.TrimSpace(expectedAttemptID) {
		return agentTaskFence{}, errors.New("cleanup receipt attempt does not match the route")
	}
	receipt := anyMap(payload["cleanup_receipt"])
	if len(receipt) == 0 && anyToString(payload["schema_id"]) == agentTaskCleanupReceiptID {
		receipt = payload
	}
	if err := validateStrictRouteStringFields(receipt, "task_id", "attempt_id", "lease_id", "worker_id", "worker_instance_id"); err != nil {
		return agentTaskFence{}, err
	}
	if raw, present := receipt["generation"]; present {
		if value, valid := strictWorkerIdentityInteger(raw); !valid || value < 0 {
			return agentTaskFence{}, errors.New("cleanup receipt generation must be a nonnegative integer")
		}
	}
	for field, expected := range map[string]string{
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID,
	} {
		if anyToString(receipt[field]) != expected {
			return agentTaskFence{}, errors.New("cleanup receipt does not match the exact request fence")
		}
	}
	if anyToInt(receipt["generation"], 0) != fence.Generation {
		return agentTaskFence{}, errors.New("cleanup receipt does not match the exact request fence")
	}
	return fence, nil
}

func agentTaskPublicationFenceFromQuery(taskID, attemptID string, r *http.Request) (agentTaskFence, string, error) {
	if r == nil {
		return agentTaskFence{}, "", errors.New("publication reconciliation request is unavailable")
	}
	query := r.URL.Query()
	generation, generationValid := strictWorkerIdentityInteger(json.Number(query.Get("generation")))
	if !generationValid || generation < 0 {
		return agentTaskFence{}, "", errors.New("publication reconciliation generation must be a nonnegative integer")
	}
	fence := agentTaskFence{
		TaskID: strings.TrimSpace(taskID), AttemptID: strings.TrimSpace(attemptID),
		LeaseID: strings.TrimSpace(query.Get("lease_id")), WorkerID: strings.TrimSpace(query.Get("worker_id")),
		WorkerInstanceID: strings.TrimSpace(query.Get("worker_instance_id")), Generation: generation,
	}
	identityGenerationRaw := query.Get("worker_identity_update_generation")
	if identityGenerationRaw == "" {
		fence.WorkerIdentityUpdateGeneration = 0
	} else if identityGeneration, valid := strictWorkerIdentityInteger(json.Number(identityGenerationRaw)); !valid || identityGeneration < 0 {
		return agentTaskFence{}, "", errors.New("publication reconciliation identity generation must be a nonnegative integer")
	} else {
		fence.WorkerIdentityUpdateGeneration = identityGeneration
	}
	if fence.TaskID == "" || fence.AttemptID == "" || fence.LeaseID == "" || fence.WorkerID == "" || fence.WorkerInstanceID == "" || fence.Generation <= 0 {
		return agentTaskFence{}, "", errors.New("complete task, attempt, lease, worker, worker instance, and generation reconciliation fence is required")
	}
	for _, field := range []string{"assignment_generation", "lease_generation"} {
		if raw := strings.TrimSpace(query.Get(field)); raw != "" {
			copyGeneration, valid := strictWorkerIdentityInteger(json.Number(raw))
			if !valid || copyGeneration != generation {
				return agentTaskFence{}, "", errors.New("publication reconciliation generation copies do not match exactly")
			}
		}
	}
	idempotencyKey := strings.TrimSpace(query.Get("idempotency_key"))
	if idempotencyKey == "" {
		return agentTaskFence{}, "", errors.New("publication reconciliation idempotency_key is required")
	}
	return fence, idempotencyKey, nil
}

func agentTaskRouteErrorStatus(err error) int {
	if err == nil {
		return http.StatusInternalServerError
	}
	var migrationChallenge *workerIdentityCredentialMigrationChallengeError
	if errors.As(err, &migrationChallenge) {
		return http.StatusConflict
	}
	if errors.Is(err, errWorkerIdentityLegacyCredentialMigration) {
		return http.StatusConflict
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "not found"), strings.Contains(message, "no rows"):
		return http.StatusNotFound
	case strings.Contains(message, "legacy worker identity credential migration required"):
		return http.StatusConflict
	case strings.Contains(message, "authenticated task principal"), strings.Contains(message, "signed task principal"), strings.Contains(message, "worker instance credential"):
		return http.StatusUnauthorized
	case strings.Contains(message, "governance is unavailable"), strings.Contains(message, "unresolved commit"):
		return http.StatusServiceUnavailable
	case strings.Contains(message, "unauthorized"), strings.Contains(message, "not authorized"), strings.Contains(message, "canonical reviewer"), strings.Contains(message, "approval actor"), strings.Contains(message, "workspace"):
		return http.StatusForbidden
	case strings.Contains(message, "stale_lease_fence"), strings.Contains(message, "fence"), strings.Contains(message, "already used"):
		return http.StatusConflict
	case strings.Contains(message, "unavailable"), strings.Contains(message, "sqlite"), strings.Contains(message, "ledger"):
		return http.StatusServiceUnavailable
	default:
		return http.StatusUnprocessableEntity
	}
}

func writeAgentTaskRouteError(w http.ResponseWriter, err error) {
	status := agentTaskRouteErrorStatus(err)
	var migrationChallenge *workerIdentityCredentialMigrationChallengeError
	if errors.As(err, &migrationChallenge) {
		writeJSON(w, status, map[string]any{
			"ok": false, "error": "worker identity credential migration challenge required",
			"code": "worker_identity_credential_migration_required", "migration_challenge": migrationChallenge.Challenge,
		})
		return
	}
	if errors.Is(err, errWorkerIdentityLegacyCredentialMigration) {
		writeJSON(w, status, map[string]any{
			"ok": false, "error": "worker identity credential migration required",
			"code": "worker_identity_credential_migration_required",
		})
		return
	}
	message := "authoritative task ledger rejected the request"
	if status == http.StatusNotFound {
		message = "task delivery record not found"
	} else if status == http.StatusUnauthorized {
		message = "task delivery authentication failed"
	} else if status == http.StatusForbidden {
		message = "task delivery authorization failed"
	} else if status == http.StatusConflict {
		message = "task delivery fence or idempotency conflict"
	} else if status == http.StatusServiceUnavailable {
		message = "authoritative task ledger unavailable"
	}
	// Do not expose storage paths, SQL text, or other internal framing through
	// the public compatibility boundary. Stable status is the diagnostic API.
	writeJSON(w, status, map[string]any{"ok": false, "error": message})
}

func agentWorkerIdentityRouteKind(path string) (string, string, bool) {
	path = canonicalAgentTaskPath(path)
	switch path {
	case "/agents/workers/register", "/agents/tasks/worker/register", "/agents/tasks/workers/register":
		return "register", "", true
	case "/agents/workers/identity", "/agents/tasks/worker/identity", "/agents/tasks/workers/identity":
		return "read", "", true
	case "/agents/workers/identity/ack", "/agents/tasks/worker/identity/ack", "/agents/tasks/workers/identity/ack":
		return "ack", "", true
	case "/agents/workers/identity/retire", "/agents/tasks/worker/identity/retire", "/agents/tasks/workers/identity/retire":
		return "retire", "", true
	}
	for _, prefix := range []string{"/agents/workers/identity/", "/agents/tasks/worker/identity/", "/agents/tasks/workers/identity/"} {
		if strings.HasPrefix(path, prefix) {
			updateID := strings.Trim(strings.TrimPrefix(path, prefix), "/")
			if updateID != "" && !strings.Contains(updateID, "/") {
				return "read", updateID, true
			}
		}
	}
	return "", "", false
}

// agentWorkerInstanceCredentialRoutePath is deliberately narrower than the
// /agents surface. The credential header is consumed only by registration,
// worker-identity, exact worker-fence, and identity-bound task-claim routes;
// list/read/artifact/recipient/reviewer/publication-worker routes never enter
// the proof boundary and therefore never copy the header into auth state.
func agentWorkerInstanceCredentialRoutePath(path string) bool {
	path = canonicalAgentTaskPath(path)
	if _, _, ok := agentWorkerIdentityRouteKind(path); ok {
		return true
	}
	if path == "/agents/tasks/next" {
		return true
	}
	if !strings.HasPrefix(path, "/agents/tasks/") {
		return false
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, "/agents/tasks/"), "/"), "/")
	if len(parts) == 2 {
		switch strings.ToLower(strings.TrimSpace(parts[1])) {
		case "heartbeat", "observe", "status", "cancel", "publish", "publication", "cleanup":
			return true
		}
	}
	if len(parts) == 4 && strings.EqualFold(parts[1], "attempts") {
		switch strings.ToLower(strings.TrimSpace(parts[3])) {
		case "cleanup", "publication":
			return true
		}
	}
	return false
}

func (s *server) agentWorkerIdentityAuthorityFromRoute(auth agentTaskRouteAuth, r *http.Request, payload map[string]any) (agentWorkerIdentityAuthority, error) {
	principalID, _, err := strictRouteStringField(payload, "principal_id")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	principalAlias, _, err := strictRouteStringField(payload, "principal")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	workspaceID, _, err := strictRouteStringField(payload, "workspace_id")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	instanceID, _, err := strictRouteStringField(payload, "worker_instance_id")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	instanceAlias, _, err := strictRouteStringField(payload, "instance_id")
	if err != nil {
		return agentWorkerIdentityAuthority{}, err
	}
	principal := auth.Principal
	workspace := auth.Workspace
	if auth.Service {
		requireCallerAuthority := s == nil || s.taskServiceWorkerAuthority == nil
		principal, err = consistentAgentWorkerRouteValue("principal_id", requireCallerAuthority, principalID, principalAlias, r.URL.Query().Get("principal_id"), r.Header.Get("X-Worker-Principal"))
		if err != nil {
			return agentWorkerIdentityAuthority{}, err
		}
		workspace, err = consistentAgentWorkerRouteValue("workspace_id", requireCallerAuthority, workspaceID, r.URL.Query().Get("workspace_id"), r.Header.Get("X-Worker-Workspace"))
		if err != nil {
			return agentWorkerIdentityAuthority{}, err
		}
		if s != nil && s.taskServiceWorkerAuthority != nil {
			principal, workspace, err = s.taskServiceWorkerAuthority(principal, workspace)
			if err != nil {
				return agentWorkerIdentityAuthority{}, err
			}
		}
	} else {
		supplied, suppliedErr := consistentAgentWorkerRouteValue("principal_id", true, principalID, principalAlias)
		if suppliedErr != nil {
			return agentWorkerIdentityAuthority{}, suppliedErr
		}
		if supplied != "" && !strings.EqualFold(supplied, principal) {
			return agentWorkerIdentityAuthority{}, errors.New("signed worker cannot choose authenticated principal")
		}
		if workspaceID != "" && !strings.EqualFold(workspaceID, workspace) {
			return agentWorkerIdentityAuthority{}, errors.New("signed worker cannot choose authenticated workspace")
		}
	}
	instance, instanceErr := consistentAgentWorkerRouteValue("worker_instance_id", false, instanceID, instanceAlias, r.URL.Query().Get("worker_instance_id"), r.Header.Get("X-Worker-Instance-ID"))
	if instanceErr != nil {
		return agentWorkerIdentityAuthority{}, instanceErr
	}
	if auth.Signed && auth.RequestBound && (strings.TrimSpace(auth.WorkerInstanceID) == "" || instance != auth.WorkerInstanceID) {
		return agentWorkerIdentityAuthority{}, errors.New("signed worker cannot choose an unauthenticated worker instance")
	}
	return normalizeWorkerIdentityAuthority(principal, workspace, instance)
}

func (s *server) handleAgentWorkerIdentityRoute(w http.ResponseWriter, r *http.Request, auth agentTaskRouteAuth, payload map[string]any) {
	kind, pathUpdateID, ok := agentWorkerIdentityRouteKind(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "worker identity route not found"})
		return
	}
	if err := validateStrictRouteStringFields(payload, "principal_id", "principal", "workspace_id", "worker_instance_id", "instance_id", "requested_worker_id", "worker_id", "worker", "canonical_worker_id", "new_worker_id", "update_id", "identity_id", "identity_digest", "retirement_digest", "retirement_receipt_digest", "retirement_id"); err != nil {
		writeAgentTaskRouteError(w, err)
		return
	}
	if raw, present := payload["worker_identity_update_generation"]; present {
		generation, valid := strictWorkerIdentityInteger(raw)
		if !valid || generation < 0 {
			writeAgentTaskRouteError(w, errors.New("worker identity update generation must be a nonnegative integer"))
			return
		}
	}
	authority, err := s.agentWorkerIdentityAuthorityFromRoute(auth, r, payload)
	if err != nil {
		writeAgentTaskRouteError(w, err)
		return
	}
	ctx := r.Context()
	if kind != "register" {
		if _, credentialErr := s.authorizeAgentWorkerIdentityCredential(ctx, &auth, authority); credentialErr != nil {
			writeAgentTaskRouteError(w, credentialErr)
			return
		}
	}
	switch kind {
	case "register":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		if _, present := payload["worker_identity_update_generation"]; present {
			writeAgentTaskRouteError(w, errors.New("caller-supplied worker identity update generation is not authoritative"))
			return
		}
		requestedWorkerID, _, _ := strictRouteStringField(payload, "requested_worker_id")
		workerID, _, _ := strictRouteStringField(payload, "worker_id")
		workerAlias, _, _ := strictRouteStringField(payload, "worker")
		requested, requestedErr := consistentAgentWorkerRouteValue("requested_worker_id", true, requestedWorkerID, workerID, workerAlias, r.URL.Query().Get("worker"), r.Header.Get("X-Worker-ID"))
		if requestedErr != nil {
			writeAgentTaskRouteError(w, requestedErr)
			return
		}
		if _, present, _ := strictRouteStringField(payload, "canonical_worker_id"); present {
			writeAgentTaskRouteError(w, errors.New("caller-supplied canonical worker ID is not authoritative"))
			return
		}
		if _, present, _ := strictRouteStringField(payload, "new_worker_id"); present {
			writeAgentTaskRouteError(w, errors.New("caller-supplied canonical worker ID is not authoritative"))
			return
		}
		var response map[string]any
		var registerErr error
		if !auth.RequestBound {
			// This is the trusted in-process ledger/test compatibility surface.
			// Every HTTP request, including a service API-key request, is marked
			// RequestBound by authenticateAgentTaskRoute and takes the strict
			// client-credential path below; no external caller can select this
			// no-proof registration branch.
			response, registerErr = s.taskLedger.registerWorkerIdentity(ctx, authority.PrincipalID, authority.WorkspaceID, requested, authority.WorkerInstanceID)
		} else {
			response, registerErr = s.taskLedger.registerWorkerIdentity(ctx, authority.PrincipalID, authority.WorkspaceID, requested, authority.WorkerInstanceID, auth.WorkerInstanceCredential)
		}
		if registerErr != nil {
			writeAgentTaskRouteError(w, registerErr)
			return
		}
		identity := anyMap(response["identity"])
		registrationPayload := map[string]any{
			"schema_id": agentWorkerIdentityRegistrationContractID, "contract_version": 1,
			"principal_id": authority.PrincipalID, "workspace_id": authority.WorkspaceID,
			"requested_worker_id": anyToString(identity["requested_worker_id"]), "canonical_worker_id": anyToString(identity["canonical_worker_id"]),
			"worker_instance_id": authority.WorkerInstanceID, "worker_identity_update_generation": anyToInt(identity["worker_identity_update_generation"], 0),
			"identity": identity, "identity_update_required": anyToBool(response["identity_update_required"]),
			"idempotent_replay": anyToBool(response["idempotent_replay"]),
		}
		if update := anyMap(response["identity_update"]); len(update) != 0 {
			registrationPayload["identity_update"] = update
		}
		registration := agentTaskContractPayload(agentWorkerIdentityRegistrationContractID, registrationPayload)
		writeJSON(w, http.StatusOK, registration)
	case "read":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		updateIDValue, _, _ := strictRouteStringField(payload, "update_id")
		updateID, updateIDErr := consistentAgentWorkerRouteValue("update_id", false, pathUpdateID, updateIDValue, r.URL.Query().Get("update_id"))
		if updateIDErr != nil {
			writeAgentTaskRouteError(w, updateIDErr)
			return
		}
		update, readErr := s.taskLedger.readWorkerIdentityUpdate(ctx, authority, updateID)
		if readErr != nil {
			writeAgentTaskRouteError(w, readErr)
			return
		}
		if update.UpdateID == "" {
			writeJSON(w, http.StatusOK, map[string]any{"identity_update": nil, "identity_update_required": false, "authoritative_backend": "gateway-go-sqlite-wal"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"identity_update": update.payload(), "identity_update_required": update.State != agentWorkerIdentityStateAcknowledged, "authoritative_backend": "gateway-go-sqlite-wal"})
	case "ack":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		ackPayload := cloneAnyMap(payload)
		ackPayload["principal_id"] = authority.PrincipalID
		ackPayload["workspace_id"] = authority.WorkspaceID
		ackPayload["worker_instance_id"] = authority.WorkerInstanceID
		ack, ackErr := s.taskLedger.acknowledgeWorkerIdentityUpdate(ctx, ackPayload, authority)
		if ackErr != nil {
			writeAgentTaskRouteError(w, ackErr)
			return
		}
		ackResponse := agentTaskContractPayload(agentWorkerIdentityAckContractID, map[string]any{
			"schema_id": agentWorkerIdentityAckContractID, "contract_version": 1,
			"update_id": anyToString(anyMap(ack["identity_update"])["update_id"]), "identity_id": anyToString(anyMap(ack["identity_update"])["identity_id"]), "identity_update": ack["identity_update"],
			"principal_id": authority.PrincipalID, "workspace_id": authority.WorkspaceID, "worker_instance_id": authority.WorkerInstanceID,
			"old_worker_id": anyToString(anyMap(ack["identity_update"])["old_worker_id"]), "requested_worker_id": anyToString(anyMap(ack["identity_update"])["requested_worker_id"]),
			"canonical_worker_id": anyToString(anyMap(ack["identity_update"])["canonical_worker_id"]), "new_worker_id": anyToString(anyMap(ack["identity_update"])["new_worker_id"]),
			"worker_identity_update_generation": anyToInt(anyMap(ack["identity_update"])["worker_identity_update_generation"], 0),
			"update_digest":                     anyToString(anyMap(ack["identity_update"])["update_digest"]), "receipt_digest": anyToString(anyMap(ack["identity_update"])["receipt_digest"]),
			"ack_receipt_digest": anyToString(anyMap(ack["identity_update"])["ack_receipt_digest"]),
			"acknowledged":       true, "idempotent_replay": anyToBool(ack["idempotent_replay"]),
		})
		writeJSON(w, http.StatusOK, ackResponse)
	case "retire":
		if r.Method == http.MethodGet {
			readPayload := cloneAnyMap(payload)
			for _, field := range []string{"identity_id", "identity_digest", "retirement_digest", "retirement_receipt_digest", "requested_worker_id", "canonical_worker_id", "worker_instance_id"} {
				if strings.TrimSpace(anyToString(readPayload[field])) == "" {
					if value := strings.TrimSpace(r.URL.Query().Get(field)); value != "" {
						readPayload[field] = value
					}
				}
			}
			readPayload["principal_id"] = authority.PrincipalID
			readPayload["workspace_id"] = authority.WorkspaceID
			readPayload["worker_instance_id"] = authority.WorkerInstanceID
			receipt, readErr := s.taskLedger.readWorkerIdentityRetirement(ctx, readPayload, authority)
			if readErr != nil {
				writeAgentTaskRouteError(w, readErr)
				return
			}
			writeJSON(w, http.StatusOK, receipt)
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		retirePayload := cloneAnyMap(payload)
		retirePayload["principal_id"] = authority.PrincipalID
		retirePayload["workspace_id"] = authority.WorkspaceID
		retirePayload["worker_instance_id"] = authority.WorkerInstanceID
		receipt, retireErr := s.taskLedger.retireWorkerIdentity(ctx, retirePayload, authority)
		if retireErr != nil {
			writeAgentTaskRouteError(w, retireErr)
			return
		}
		writeJSON(w, http.StatusOK, receipt)
	}
}

func (s *server) handleAgentTaskCleanup(w http.ResponseWriter, r *http.Request, auth agentTaskRouteAuth, taskID, expectedAttemptID string, payload map[string]any) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	fence, fenceErr := agentTaskCleanupFenceFromRequest(taskID, expectedAttemptID, payload)
	if fenceErr != nil {
		writeAgentTaskRouteError(w, fenceErr)
		return
	}
	if fenceErr := s.authorizeAgentTaskFence(r.Context(), &auth, fence); fenceErr != nil {
		writeAgentTaskRouteError(w, fenceErr)
		return
	}
	if resourceErr := s.authorizeTaskWorkerFence(r.Context(), fence, auth); resourceErr != nil {
		writeAgentTaskRouteError(w, resourceErr)
		return
	}
	receipt, cleanupErr := s.taskLedger.acknowledgeCleanup(r.Context(), taskID, fence.AttemptID, payload)
	if cleanupErr != nil {
		writeAgentTaskRouteError(w, cleanupErr)
		return
	}
	writeJSON(w, http.StatusOK, receipt)
}

func (s *server) agentTaskDeliveryRoute(w http.ResponseWriter, r *http.Request) {
	if !methodAllowed(r.Method, http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodPut) {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	if s == nil || s.taskLedger == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "authoritative task ledger unavailable"})
		return
	}
	payload, err := readAgentTaskPayload(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid task delivery json"})
		return
	}
	path := canonicalAgentTaskPath(r.URL.Path)
	auth, authErr := s.authenticateAgentTaskRoute(r, agentTaskRouteOperation(path, r.Method))
	if authErr != nil {
		writeAgentTaskRouteError(w, authErr)
		return
	}
	ctx := r.Context()
	if isAgentWorkerIdentityPath(path) {
		s.handleAgentWorkerIdentityRoute(w, r, auth, payload)
		return
	}

	switch {
	case path == "/agents/tasks" && r.Method == http.MethodPost:
		manifest := cloneAnyMap(anyMap(payload["manifest"]))
		if len(manifest) == 0 {
			manifest = payload
		}
		project := strings.TrimSpace(anyToString(manifest["project"]))
		boundWorkspace, bindingErr := s.resolveAgentTaskProjectWorkspace(project)
		if bindingErr != nil {
			writeAgentTaskRouteError(w, bindingErr)
			return
		}
		requestedWorkspace := strings.TrimSpace(anyToString(manifest["workspace_id"]))
		if requestedWorkspace != "" && !strings.EqualFold(requestedWorkspace, boundWorkspace) {
			writeAgentTaskRouteError(w, errors.New("task project is outside its active workspace binding"))
			return
		}
		if !auth.Service {
			if err := s.authorizeAgentTaskProject(auth.Workspace, project); err != nil {
				writeAgentTaskRouteError(w, err)
				return
			}
			owner := strings.TrimSpace(anyToString(manifest["review_owner"]))
			if owner != "" && !strings.EqualFold(owner, auth.Principal) {
				writeAgentTaskRouteError(w, errors.New("task review owner must match authenticated principal"))
				return
			}
			manifest["review_owner"] = auth.Principal
			manifest["requesting_agent_id"] = auth.Principal
		} else if s.taskServiceOwnerLocalLifecycle {
			manifest, bindingErr = s.bindAgentTaskServiceLifecycleManifest(manifest, boundWorkspace)
			if bindingErr != nil {
				writeAgentTaskRouteError(w, bindingErr)
				return
			}
		} else if strings.TrimSpace(anyToString(manifest["requesting_agent_id"])) == "" {
			manifest["requesting_agent_id"] = "gateway-service"
		}
		manifest["workspace_id"] = boundWorkspace
		metadata := cloneAnyMap(anyMap(manifest["metadata"]))
		delete(metadata, "authorized_workspace_id")
		manifest["metadata"] = metadata
		task, submitErr := s.taskLedger.submit(ctx, manifest)
		if submitErr != nil {
			writeAgentTaskRouteError(w, submitErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"task": task, "authoritative_backend": "gateway-go-sqlite-wal"})
		return
	case path == "/agents/tasks" && r.Method == http.MethodGet:
		agentFilter := r.URL.Query().Get("agent")
		if !auth.Service {
			agentFilter = auth.Principal
		}
		tasks, listErr := s.taskLedger.list(ctx, r.URL.Query().Get("status"), r.URL.Query().Get("project"), agentFilter, anyToInt(r.URL.Query().Get("limit"), 50))
		if listErr != nil {
			writeAgentTaskRouteError(w, listErr)
			return
		}
		filtered := make([]map[string]any, 0, len(tasks))
		for _, task := range tasks {
			boundWorkspace, bindingErr := s.resolveAgentTaskProjectWorkspace(anyToString(task["project"]))
			if bindingErr != nil || !strings.EqualFold(anyToString(task["workspace_id"]), boundWorkspace) {
				continue
			}
			if !auth.Service && (!agentTaskTaskAllowsPrincipal(task, auth.Principal) || !strings.EqualFold(boundWorkspace, auth.Workspace)) {
				continue
			}
			filtered = append(filtered, task)
		}
		tasks = filtered
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "authoritative_backend": "gateway-go-sqlite-wal"})
		return
	case path == "/agents/tasks/next" && r.Method == http.MethodPost:
		if err := validateStrictRouteStringFields(payload, "requested_worker_id", "requested_worker", "canonical_worker_id", "worker_id", "worker", "worker_instance_id", "principal_id", "principal", "workspace_id"); err != nil {
			writeAgentTaskRouteError(w, err)
			return
		}
		requestedWorkerID, _, _ := strictRouteStringField(payload, "requested_worker_id")
		requestedWorkerAlias, _, _ := strictRouteStringField(payload, "requested_worker")
		canonicalWorkerID, _, _ := strictRouteStringField(payload, "canonical_worker_id")
		workerID, _, _ := strictRouteStringField(payload, "worker_id")
		workerAlias, _, _ := strictRouteStringField(payload, "worker")
		instanceID, _, _ := strictRouteStringField(payload, "worker_instance_id")
		principalID, _, _ := strictRouteStringField(payload, "principal_id")
		principalAlias, _, _ := strictRouteStringField(payload, "principal")
		workspaceID, _, _ := strictRouteStringField(payload, "workspace_id")
		requestedWorker, consistencyErr := consistentAgentWorkerRouteValue("requested_worker_id", true, requestedWorkerID, requestedWorkerAlias)
		if consistencyErr != nil {
			writeAgentTaskRouteError(w, consistencyErr)
			return
		}
		canonicalWorkerHint, consistencyErr := consistentAgentWorkerRouteValue("canonical_worker_id", true, canonicalWorkerID, workerID, workerAlias, r.URL.Query().Get("worker"), r.Header.Get("X-Worker-ID"))
		if consistencyErr != nil {
			writeAgentTaskRouteError(w, consistencyErr)
			return
		}
		instance, consistencyErr := consistentAgentWorkerRouteValue("worker_instance_id", false, instanceID, r.Header.Get("X-Worker-Instance-ID"))
		if consistencyErr != nil {
			writeAgentTaskRouteError(w, consistencyErr)
			return
		}
		if auth.Signed && auth.RequestBound && (strings.TrimSpace(auth.WorkerInstanceID) == "" || instance != auth.WorkerInstanceID) {
			writeAgentTaskRouteError(w, errors.New("signed worker cannot choose an unauthenticated worker instance"))
			return
		}
		principal := auth.Principal
		workspace := auth.Workspace
		requestedIdentityGeneration := -1
		if raw, present := payload["worker_identity_update_generation"]; present {
			parsed, valid := strictWorkerIdentityInteger(raw)
			if !valid || parsed < 0 {
				writeAgentTaskRouteError(w, errors.New("worker identity update generation must be a nonnegative integer"))
				return
			}
			requestedIdentityGeneration = parsed
		}
		if auth.Service {
			requireCallerAuthority := s == nil || s.taskServiceWorkerAuthority == nil
			principal, consistencyErr = consistentAgentWorkerRouteValue("principal_id", requireCallerAuthority, principalID, principalAlias, r.Header.Get("X-Worker-Principal"))
			if consistencyErr == nil {
				workspace, consistencyErr = consistentAgentWorkerRouteValue("workspace_id", requireCallerAuthority, workspaceID, r.Header.Get("X-Worker-Workspace"))
			}
			if consistencyErr == nil && s != nil && s.taskServiceWorkerAuthority != nil {
				principal, workspace, consistencyErr = s.taskServiceWorkerAuthority(principal, workspace)
			}
			if consistencyErr != nil {
				writeAgentTaskRouteError(w, consistencyErr)
				return
			}
		} else {
			suppliedPrincipal, principalErr := consistentAgentWorkerRouteValue("principal_id", true, principalID, principalAlias)
			if principalErr != nil || (suppliedPrincipal != "" && !strings.EqualFold(suppliedPrincipal, principal)) {
				if principalErr == nil {
					principalErr = errors.New("signed worker cannot choose authenticated principal")
				}
				writeAgentTaskRouteError(w, principalErr)
				return
			}
			if workspaceID != "" && !strings.EqualFold(workspaceID, workspace) {
				writeAgentTaskRouteError(w, errors.New("signed worker cannot choose authenticated workspace"))
				return
			}
		}
		if requestedWorker == "" && canonicalWorkerHint == "" {
			writeAgentTaskRouteError(w, errors.New("requested_worker_id is required"))
			return
		}
		if !auth.Service && auth.Signed && auth.RequestBound {
			if _, credentialErr := s.authorizeAgentWorkerIdentityCredential(ctx, &auth, agentWorkerIdentityAuthority{PrincipalID: principal, WorkspaceID: workspace, WorkerInstanceID: instance}); credentialErr != nil {
				writeAgentTaskRouteError(w, credentialErr)
				return
			}
		}
		identity, identityErr := s.taskLedger.workerIdentityByAuthority(ctx, agentWorkerIdentityAuthority{PrincipalID: principal, WorkspaceID: workspace, WorkerInstanceID: instance})
		if errors.Is(identityErr, sql.ErrNoRows) {
			identityErr = errWorkerIdentityNotRegistered
		}
		if identityErr != nil {
			writeAgentTaskRouteError(w, identityErr)
			return
		}
		if requestedIdentityGeneration >= 0 && requestedIdentityGeneration != identity.IdentityUpdateGeneration {
			writeAgentTaskRouteError(w, errors.New("worker identity update generation does not match the registered instance"))
			return
		}
		if requestedWorker == "" {
			requestedWorker = identity.RequestedWorkerID
		}
		if identity.RequestedWorkerID != strings.ToLower(requestedWorker) {
			writeAgentTaskRouteError(w, errors.New("requested worker ID does not match the registered instance"))
			return
		}
		if canonicalWorkerHint != "" && !strings.EqualFold(identity.CanonicalWorkerID, canonicalWorkerHint) {
			writeAgentTaskRouteError(w, errors.New("canonical worker ID does not match the registered instance"))
			return
		}
		if identity.IdentityUpdateGeneration > identity.AcknowledgedGeneration {
			update, updateErr := s.taskLedger.readWorkerIdentityUpdate(ctx, agentWorkerIdentityAuthority{PrincipalID: principal, WorkspaceID: workspace, WorkerInstanceID: instance}, "")
			if updateErr != nil {
				writeAgentTaskRouteError(w, updateErr)
				return
			}
			if update.UpdateID == "" {
				writeAgentTaskRouteError(w, errors.New("worker identity update is missing for the pending generation"))
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"task": nil, "identity_update": update.payload(), "identity_update_required": update.State != agentWorkerIdentityStateAcknowledged, "authoritative_backend": "gateway-go-sqlite-wal"})
			return
		}
		claimed, claimErr := s.claimNextAuthorizedAgentTaskWithIdentity(ctx, identity)
		if claimErr != nil {
			writeAgentTaskRouteError(w, claimErr)
			return
		}
		if claimed == nil {
			writeJSON(w, http.StatusOK, map[string]any{"task": nil, "authoritative_backend": "gateway-go-sqlite-wal"})
			return
		}
		writeJSON(w, http.StatusOK, claimed)
		return
	case path == "/agents/tasks/recover-leases" && r.Method == http.MethodPost:
		if !auth.Service {
			writeAgentTaskRouteError(w, errors.New("lease recovery is restricted to the Gateway operator"))
			return
		}
		recovered, recoverErr := s.taskLedger.recoverExpired(ctx, anyToInt(payload["limit"], anyToInt(r.URL.Query().Get("limit"), 200)))
		if recoverErr != nil {
			writeAgentTaskRouteError(w, recoverErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recovered": recovered, "authoritative_backend": "gateway-go-sqlite-wal"})
		return
	case path == "/agents/tasks/runtime" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"runtime": s.taskLedger.runtimeSnapshot(ctx)})
		return
	case path == "/agents/tasks/migrate" && r.Method == http.MethodPost:
		if !auth.Service {
			writeAgentTaskRouteError(w, errors.New("task migration is restricted to the Gateway operator"))
			return
		}
		if strings.EqualFold(strings.TrimSpace(anyToString(payload["phase"])), "worker_identity_rebind") {
			receipt, recoveryErr := s.taskLedger.rebindLegacyWorkerIdentityQueuedClaims(ctx, payload)
			if recoveryErr != nil {
				writeAgentTaskRouteError(w, recoveryErr)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"migration": receipt, "authoritative_backend": "gateway-go-sqlite-wal"})
			return
		}
		receipt, migrationErr := s.taskLedger.migration(ctx, payload)
		if migrationErr != nil {
			writeAgentTaskRouteError(w, migrationErr)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"migration": receipt})
		return
	case path == "/agents/tasks/deadletter" && r.Method == http.MethodGet:
		if !auth.Service {
			writeAgentTaskRouteError(w, errors.New("dead-letter inspection is restricted to the Gateway operator"))
			return
		}
		tasks, listErr := s.taskLedger.list(ctx, "dead_letter", r.URL.Query().Get("project"), r.URL.Query().Get("agent"), anyToInt(r.URL.Query().Get("limit"), 100))
		if listErr != nil {
			writeAgentTaskRouteError(w, listErr)
			return
		}
		filtered := make([]map[string]any, 0, len(tasks))
		for _, task := range tasks {
			boundWorkspace, bindingErr := s.resolveAgentTaskProjectWorkspace(anyToString(task["project"]))
			if bindingErr == nil && strings.EqualFold(anyToString(task["workspace_id"]), boundWorkspace) {
				filtered = append(filtered, task)
			}
		}
		tasks = filtered
		writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
		return
	}

	if strings.HasPrefix(path, "/agents/tasks/") {
		remainder := strings.Trim(strings.TrimPrefix(path, "/agents/tasks/"), "/")
		parts := strings.Split(remainder, "/")
		if len(parts) == 0 || strings.TrimSpace(parts[0]) == "" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "task route not found"})
			return
		}
		if parts[0] == "artifacts" && len(parts) == 2 && r.Method == http.MethodGet {
			artifact, artifactErr := s.taskLedger.artifact(ctx, parts[1], auth.Principal)
			if artifactErr != nil {
				writeAgentTaskRouteError(w, artifactErr)
				return
			}
			if !agentTaskArtifactAllowsAuth(ctx, s, artifact, auth) {
				writeAgentTaskRouteError(w, errors.New("artifact resource is outside authenticated workspace"))
				return
			}
			writeJSON(w, http.StatusOK, artifact)
			return
		}
		if parts[0] == "artifacts" && len(parts) == 3 && strings.EqualFold(parts[2], "content") && r.Method == http.MethodGet {
			artifact, lookupErr := s.taskLedger.artifact(ctx, parts[1], auth.Principal)
			if lookupErr != nil || !agentTaskArtifactAllowsAuth(ctx, s, artifact, auth) {
				writeAgentTaskRouteError(w, firstNonEmptyError(lookupErr, errors.New("artifact resource is outside authenticated workspace")))
				return
			}
			file, _, artifactErr := s.taskLedger.artifactFile(ctx, parts[1], auth.Principal)
			if artifactErr != nil {
				writeAgentTaskRouteError(w, artifactErr)
				return
			}
			defer file.Close()
			http.ServeContent(w, r, "agent-task-artifact", time.Time{}, file)
			return
		}
		taskID := strings.TrimSpace(parts[0])
		if len(parts) == 1 && r.Method == http.MethodGet {
			task, events, getErr := s.taskLedger.get(ctx, taskID)
			if getErr != nil {
				writeAgentTaskRouteError(w, getErr)
				return
			}
			if task == nil {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "task delivery record not found"})
				return
			}
			if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
				writeAgentTaskRouteError(w, resourceErr)
				return
			}
			deliveries, deliveryErr := s.taskLedger.deliveries(ctx, taskID, anyToString(task["result_id"]))
			if deliveryErr != nil {
				writeAgentTaskRouteError(w, deliveryErr)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"task": task, "events": events, "deliveries": deliveries})
			return
		}
		if len(parts) == 2 {
			suffix := strings.ToLower(strings.TrimSpace(parts[1]))
			switch suffix {
			case "heartbeat":
				if r.Method != http.MethodPost {
					writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
					return
				}
				fence, fenceErr := agentTaskFenceFromRequest(taskID, payload)
				if fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if fenceErr := s.authorizeAgentTaskFence(ctx, &auth, fence); fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if resourceErr := s.authorizeTaskWorkerFence(ctx, fence, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				attempt, heartbeatErr := s.taskLedger.heartbeat(ctx, fence)
				if heartbeatErr != nil {
					writeAgentTaskRouteError(w, heartbeatErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"attempt": attempt})
				return
			case "observe", "status":
				if r.Method != http.MethodPost {
					writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
					return
				}
				fence, fenceErr := agentTaskFenceFromRequest(taskID, payload)
				if fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if fenceErr := s.authorizeAgentTaskFence(ctx, &auth, fence); fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if resourceErr := s.authorizeTaskWorkerFence(ctx, fence, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				var exitCode *int
				if value, present := payload["exit_code"]; present && value != nil && strings.TrimSpace(anyToString(value)) != "" {
					value := anyToInt(payload["exit_code"], 0)
					exitCode = &value
				}
				observed, observeErr := s.taskLedger.observe(ctx, fence, anyToString(payload["runner_status"]), exitCode, anyMap(payload["metadata"]))
				if observeErr != nil {
					writeAgentTaskRouteError(w, observeErr)
					return
				}
				writeJSON(w, http.StatusOK, observed)
				return
			case "cancel":
				if r.Method != http.MethodPost {
					writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
					return
				}
				fence, fenceErr := agentTaskFenceFromRequest(taskID, payload)
				if fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if fenceErr := s.authorizeAgentTaskFence(ctx, &auth, fence); fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if resourceErr := s.authorizeTaskWorkerFence(ctx, fence, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				// A signed task worker cannot self-attest that its own process group is
				// gone. Until the execution-surface owner supplies that proof through
				// the Gateway operator path, cancellation fails closed to quarantine.
				terminationVerified := auth.Service && anyToBool(payload["termination_verified"])
				canceled, cancelErr := s.taskLedger.cancelAttempt(ctx, fence, terminationVerified, anyToString(payload["reason"]))
				if cancelErr != nil {
					writeAgentTaskRouteError(w, cancelErr)
					return
				}
				writeJSON(w, http.StatusOK, canceled)
				return
			case "publish", "publication":
				if r.Method != http.MethodPost {
					writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
					return
				}
				fence, fenceErr := agentTaskFenceFromRequest(taskID, payload)
				if fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if fenceErr := s.authorizeAgentTaskFence(ctx, &auth, fence); fenceErr != nil {
					writeAgentTaskRouteError(w, fenceErr)
					return
				}
				if resourceErr := s.authorizeTaskWorkerFence(ctx, fence, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				publication, publicationErr := s.taskLedger.stagePublication(ctx, fence, payload)
				if publicationErr != nil {
					writeAgentTaskRouteError(w, publicationErr)
					return
				}
				writeJSON(w, http.StatusOK, publication)
				return
			case "cleanup":
				s.handleAgentTaskCleanup(w, r, auth, taskID, "", payload)
				return
			case "finalize":
				if r.Method != http.MethodPost {
					writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
					return
				}
				publicationID := strings.TrimSpace(anyToString(payload["publication_id"]))
				if publicationID == "" {
					task, taskErr := s.taskLedger.queryTask(ctx, taskID)
					if taskErr != nil {
						writeAgentTaskRouteError(w, taskErr)
						return
					}
					publicationID = anyToString(task["publication_id"])
				}
				if !auth.Service {
					writeAgentTaskRouteError(w, errors.New("publication finalization is restricted to the Gateway publication worker"))
					return
				}
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				publication, finalizeErr := s.runTaskPublicationWorker(ctx, publicationID)
				if finalizeErr != nil {
					writeAgentTaskRouteError(w, finalizeErr)
					return
				}
				writeJSON(w, http.StatusOK, publication)
				return
			case "review", "review-claim", "answer", "approval", "integrate", "deliveries":
				// These actions use the explicit nested routes below.
			}
		}
		if len(parts) == 4 && strings.EqualFold(parts[1], "attempts") && strings.EqualFold(parts[3], "cleanup") {
			s.handleAgentTaskCleanup(w, r, auth, taskID, strings.TrimSpace(parts[2]), payload)
			return
		}
		if len(parts) == 4 && strings.EqualFold(parts[1], "attempts") && strings.EqualFold(parts[3], "termination") {
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
				return
			}
			if !auth.Service {
				writeAgentTaskRouteError(w, errors.New("quarantine termination resolution is restricted to the Gateway operator"))
				return
			}
			fence, fenceErr := agentTaskFenceFromRequest(taskID, payload)
			if fenceErr != nil {
				writeAgentTaskRouteError(w, fenceErr)
				return
			}
			if fence.AttemptID != strings.TrimSpace(parts[2]) {
				writeAgentTaskRouteError(w, errors.New("stale_lease_fence: termination resolution attempt does not match the route"))
				return
			}
			if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
				writeAgentTaskRouteError(w, resourceErr)
				return
			}
			resolution, resolutionErr := s.taskLedger.resolveQuarantinedAttempt(ctx, fence, anyToBool(payload["termination_verified"]), anyToString(payload["reason"]))
			if resolutionErr != nil {
				writeAgentTaskRouteError(w, resolutionErr)
				return
			}
			writeJSON(w, http.StatusOK, resolution)
			return
		}
		if len(parts) == 4 && strings.EqualFold(parts[1], "attempts") && strings.EqualFold(parts[3], "publication") {
			if r.Method != http.MethodGet {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
				return
			}
			fence, idempotencyKey, fenceErr := agentTaskPublicationFenceFromQuery(taskID, strings.TrimSpace(parts[2]), r)
			if fenceErr != nil {
				writeAgentTaskRouteError(w, fenceErr)
				return
			}
			if fenceErr := s.authorizeAgentTaskFence(ctx, &auth, fence); fenceErr != nil {
				writeAgentTaskRouteError(w, fenceErr)
				return
			}
			if resourceErr := s.authorizeTaskWorkerFence(ctx, fence, auth); resourceErr != nil {
				writeAgentTaskRouteError(w, resourceErr)
				return
			}
			publication, publicationErr := s.taskLedger.publicationForExactFence(ctx, fence, idempotencyKey)
			if publicationErr != nil {
				writeAgentTaskRouteError(w, publicationErr)
				return
			}
			writeJSON(w, http.StatusOK, publication)
			return
		}
		if len(parts) >= 3 && strings.EqualFold(parts[1], "deliveries") {
			deliveryID := strings.TrimSpace(parts[2])
			if len(parts) == 3 && r.Method == http.MethodGet {
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				deliveries, deliveryErr := s.taskLedger.deliveries(ctx, taskID, "")
				if deliveryErr != nil {
					writeAgentTaskRouteError(w, deliveryErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"deliveries": deliveries})
				return
			}
			if len(parts) == 4 && r.Method == http.MethodPost {
				action := strings.ToLower(strings.TrimSpace(parts[3]))
				var delivery map[string]any
				if action == "deliver" {
					if !auth.Service {
						writeAgentTaskRouteError(w, errors.New("delivery projection is restricted to the Gateway outbox worker"))
						return
					}
					if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
						writeAgentTaskRouteError(w, resourceErr)
						return
					}
					delivery, err = s.runTaskDeliveryOutbox(ctx, deliveryID)
				} else if action == "ack" || action == "acknowledge" {
					if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
						writeAgentTaskRouteError(w, resourceErr)
						return
					}
					actor, actorErr := s.agentTaskRecipientActor(ctx, taskID, deliveryID, auth)
					if actorErr != nil {
						writeAgentTaskRouteError(w, actorErr)
						return
					}
					delivery, err = s.taskLedger.acknowledgeDelivery(ctx, deliveryID, actor)
				} else {
					writeJSON(w, http.StatusNotFound, map[string]any{"error": "delivery action not found"})
					return
				}
				if err != nil {
					writeAgentTaskRouteError(w, err)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"delivery": delivery})
				return
			}
		}
		if len(parts) == 4 && strings.EqualFold(parts[1], "publications") && strings.EqualFold(parts[3], "finalize") && r.Method == http.MethodPost {
			if !auth.Service {
				writeAgentTaskRouteError(w, errors.New("publication finalization is restricted to the Gateway publication worker"))
				return
			}
			if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
				writeAgentTaskRouteError(w, resourceErr)
				return
			}
			publication, finalizeErr := s.runTaskPublicationWorker(ctx, parts[2])
			if finalizeErr != nil {
				writeAgentTaskRouteError(w, finalizeErr)
				return
			}
			writeJSON(w, http.StatusOK, publication)
			return
		}
		if len(parts) == 2 && r.Method == http.MethodPost {
			switch strings.ToLower(strings.TrimSpace(parts[1])) {
			case "approve":
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				actor, actorErr := s.agentTaskReviewerActor(ctx, taskID, auth)
				if actorErr != nil {
					writeAgentTaskRouteError(w, actorErr)
					return
				}
				task, approveErr := s.taskLedger.approveLegacy(ctx, taskID, actor, anyToString(payload["note"]))
				if approveErr != nil {
					writeAgentTaskRouteError(w, approveErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"task": task})
				return
			case "review":
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				actor, actorErr := s.agentTaskReviewerActor(ctx, taskID, auth)
				if actorErr != nil {
					writeAgentTaskRouteError(w, actorErr)
					return
				}
				payload["actor"] = actor
				payload["task_id"] = taskID
				decision := strings.TrimSpace(strings.ToLower(anyToString(payload["decision"])))
				sourceAttemptID := strings.TrimSpace(anyToString(payload["source_attempt_id"]))
				sourceGeneration := anyToInt(payload["source_generation"], 0)
				if decision == "request_changes" && (sourceAttemptID == "" || sourceGeneration <= 0) {
					writeAgentTaskRouteError(w, errors.New("request_changes requires source_attempt_id and positive source_generation"))
					return
				}
				review, reviewErr := s.taskLedger.reviewWithFence(ctx, taskID, anyToString(payload["result_id"]), actor, decision, anyToString(payload["reason"]), anyToString(payload["replacement_result_id"]), sourceAttemptID, sourceGeneration)
				if reviewErr != nil {
					writeAgentTaskRouteError(w, reviewErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"review": review})
				return
			case "review-claim":
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				actor, actorErr := s.agentTaskReviewerActor(ctx, taskID, auth)
				if actorErr != nil {
					writeAgentTaskRouteError(w, actorErr)
					return
				}
				claim, claimErr := s.taskLedger.claimReview(ctx, taskID, anyToString(payload["result_id"]), anyToString(payload["delivery_id"]), actor)
				if claimErr != nil {
					writeAgentTaskRouteError(w, claimErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"reviewer_claim": claim})
				return
			case "answer":
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				actor, actorErr := s.agentTaskRecipientActor(ctx, taskID, anyToString(payload["delivery_id"]), auth)
				if actorErr != nil {
					writeAgentTaskRouteError(w, actorErr)
					return
				}
				answer, answerErr := s.taskLedger.answerBlockingQuestion(ctx, taskID, anyToString(payload["result_id"]), anyToString(payload["delivery_id"]), actor, anyToString(payload["answer"]), anyToString(payload["source_attempt_id"]))
				if answerErr != nil {
					writeAgentTaskRouteError(w, answerErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"answer": answer})
				return
			case "approval":
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				actor, actorErr := s.agentTaskReviewerActor(ctx, taskID, auth)
				if actorErr != nil {
					writeAgentTaskRouteError(w, actorErr)
					return
				}
				payload["task_id"] = taskID
				payload["approver"] = actor
				approval, approvalErr := s.taskLedger.createApproval(ctx, payload)
				if approvalErr != nil {
					writeAgentTaskRouteError(w, approvalErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"approval": approval})
				return
			case "integrate":
				if resourceErr := s.authorizeTaskResource(ctx, taskID, auth); resourceErr != nil {
					writeAgentTaskRouteError(w, resourceErr)
					return
				}
				actor, actorErr := s.agentTaskReviewerActor(ctx, taskID, auth)
				if actorErr != nil {
					writeAgentTaskRouteError(w, actorErr)
					return
				}
				payload["task_id"] = taskID
				payload["actor"] = actor
				integration, integrationErr := s.taskLedger.integrate(ctx, payload)
				if integrationErr != nil {
					writeAgentTaskRouteError(w, integrationErr)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"integration": integration})
				return
			}
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "task delivery route not found"})
}

func firstNonEmptyError(primary error, fallback error) error {
	if primary != nil {
		return primary
	}
	return fallback
}

func agentTaskTaskAllowsPrincipal(task map[string]any, principal string) bool {
	principal = strings.TrimSpace(principal)
	if principal == "" || task == nil {
		return false
	}
	if strings.EqualFold(principal, anyToString(task["review_owner"])) {
		return true
	}
	for _, raw := range agentTaskRecipientRows(task) {
		if strings.EqualFold(principal, anyToString(raw["principal_id"])) {
			return true
		}
	}
	return false
}

func agentTaskArtifactAllowsAuth(ctx context.Context, s *server, artifactResponse map[string]any, auth agentTaskRouteAuth) bool {
	artifact := anyMap(artifactResponse["artifact"])
	if s == nil || s.taskLedger == nil || anyToString(artifact["task_id"]) == "" {
		return false
	}
	task, err := s.taskLedger.queryTask(ctx, anyToString(artifact["task_id"]))
	if err != nil {
		return false
	}
	boundWorkspace, bindingErr := s.resolveAgentTaskProjectWorkspace(anyToString(task["project"]))
	if bindingErr != nil || !strings.EqualFold(anyToString(task["workspace_id"]), boundWorkspace) {
		return false
	}
	if auth.Service {
		return true
	}
	return agentTaskTaskAllowsPrincipal(task, auth.Principal) && strings.EqualFold(boundWorkspace, auth.Workspace)
}
