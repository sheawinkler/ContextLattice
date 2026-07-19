package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	frontierT7PassportDiffViewSchemaID   = "context_passport_diff_view.v1"
	frontierT7CollaborativeGrantSchemaID = "collaborative_context_grant.v1"
	frontierT7GrantDecisionSchemaID      = "collaborative_context_grant_decision.v1"
	frontierT7ProvenanceSchemaID         = "provenance.v1"
	frontierT7ImportPlanSchemaID         = "import_plan.v1"
	frontierT7ImportReceiptSchemaID      = "import_receipt.v1"
	frontierT7ContinuationSchemaID       = "context_continuation_manifest.v1"
	frontierT7ReconciliationSchemaID     = "context_continuation_reconciliation.v1"

	frontierT7MaxDiffRows    = 32
	frontierT7MaxGrantItems  = 32
	frontierT7MaxImportRows  = 256
	frontierT7MaxBatchRows   = 32
	frontierT7MaxObligations = 64
	frontierT7MaxSubjectAge  = 24 * time.Hour
)

func frontierT7Digest(value any) string {
	encoded, _ := json.Marshal(value)
	return "sha256:" + sha256Hex(string(encoded))
}

func frontierT7ValidDigest(value string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func frontierT7SafeID(value, field string, maxLen int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxLen || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("%s must be a bounded single-line identifier", field)
	}
	return value, nil
}

func frontierT7SortedUnique(values []string, limit int) ([]string, error) {
	if len(values) > limit {
		return nil, fmt.Errorf("list exceeds %d items", limit)
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || len(value) > 160 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, errors.New("list values must be bounded single-line identifiers")
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func frontierT7StringSubset(child, parent []string) bool {
	allowed := map[string]struct{}{}
	for _, value := range parent {
		allowed[strings.ToLower(strings.TrimSpace(value))] = struct{}{}
	}
	for _, value := range child {
		if _, exists := allowed[strings.ToLower(strings.TrimSpace(value))]; !exists {
			return false
		}
	}
	return true
}

func frontierT7RedactionCount(stats *portableRedactionStats) int {
	if stats == nil {
		return 0
	}
	return stats.SecretKeys + stats.Tokens + stats.Paths + stats.Clipped + stats.Lists
}

func frontierT7DiffRows(baseRows, targetRows []map[string]any, stats *portableRedactionStats) []map[string]any {
	base := passportRowIndex(baseRows)
	target := passportRowIndex(targetRows)
	ids := make([]string, 0, len(base)+len(target))
	seen := map[string]struct{}{}
	for id := range base {
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range target {
		if _, exists := seen[id]; exists {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]map[string]any, 0, minInt(len(ids), frontierT7MaxDiffRows))
	for _, id := range ids {
		before, beforeExists := base[id]
		after, afterExists := target[id]
		change := "changed"
		switch {
		case !beforeExists:
			change = "added"
		case !afterExists:
			change = "removed"
		case jsonValuesEqual(before, after):
			continue
		}
		if len(out) >= frontierT7MaxDiffRows {
			break
		}
		redactionsBefore := frontierT7RedactionCount(stats)
		row := map[string]any{
			"portable_id": id, "change": change,
			"before": portableMap(before, stats), "after": portableMap(after, stats),
			"redaction_applied": frontierT7RedactionCount(stats) > redactionsBefore,
		}
		out = append(out, row)
	}
	return out
}

func buildFrontierT7PassportDiffView(base, target contextPassport) (map[string]any, error) {
	findings := append(verifyContextPassport(base, time.Now().UTC(), false), verifyContextPassport(target, time.Now().UTC(), false)...)
	if len(findings) > 0 {
		return nil, fmt.Errorf("passport verification failed: %s", strings.Join(uniqueSortedStrings(findings), ","))
	}
	structural := buildPassportDiff(base, target)
	stats := &portableRedactionStats{}
	claimRows := frontierT7DiffRows(base.Claims, target.Claims, stats)
	evidenceRows := frontierT7DiffRows(base.Evidence, target.Evidence, stats)
	lineageValid := anyToBool(structural["same_lineage"]) && anyToBool(structural["parent_link_valid"])
	summary := []string{
		fmt.Sprintf("Passport revision %d -> %d.", base.Revision, target.Revision),
		fmt.Sprintf("Claims changed: %d; evidence changed: %d.", len(claimRows), len(evidenceRows)),
	}
	if !lineageValid {
		summary = append(summary, "Lineage continuity requires review.")
	}
	return map[string]any{
		"schema_id": frontierT7PassportDiffViewSchemaID, "version": 1,
		"base_passport_id": base.PassportID, "target_passport_id": target.PassportID,
		"lineage_id": target.LineageID, "lineage_valid": lineageValid,
		"structural_diff": structural, "summary": summary,
		"objective": map[string]any{"before": portableMap(base.Objective, stats), "after": portableMap(target.Objective, stats)},
		"scope":     map[string]any{"before": portableMap(base.Scope, stats), "after": portableMap(target.Scope, stats)},
		"claims":    claimRows, "evidence": evidenceRows,
		"provenance": map[string]any{
			"base_digest": base.ContentDigest, "target_digest": target.ContentDigest,
			"issuer_key_id": target.Issuer.SigningKeyID, "deterministic": true,
		},
		"redaction": map[string]any{"applied": stats.SecretKeys+stats.Tokens+stats.Paths > 0, "secret_keys": stats.SecretKeys, "tokens": stats.Tokens, "paths": stats.Paths},
		"bounded":   true, "persisted": false, "network_calls": 0, "model_calls": 0,
	}, nil
}

type frontierT7GrantSubject struct {
	SubjectID      string   `json:"subject_id"`
	Roles          []string `json:"roles"`
	WorkspaceID    string   `json:"workspace_id"`
	SnapshotDigest string   `json:"snapshot_digest"`
	ObservedAt     string   `json:"observed_at"`
}

type frontierT7CollaborativeGrant struct {
	SchemaID         string                   `json:"schema_id"`
	Version          int                      `json:"version"`
	GrantID          string                   `json:"grant_id"`
	Subject          frontierT7GrantSubject   `json:"subject"`
	Project          string                   `json:"project"`
	Topics           []string                 `json:"topics"`
	DataClasses      []string                 `json:"data_classes"`
	Actions          []string                 `json:"actions"`
	Purpose          string                   `json:"purpose"`
	UsageLimit       int                      `json:"usage_limit"`
	ParentGrantID    string                   `json:"parent_grant_id,omitempty"`
	AncestorGrantIDs []string                 `json:"ancestor_grant_ids,omitempty"`
	DelegationDepth  int                      `json:"delegation_depth"`
	Approvers        []string                 `json:"approvers"`
	KeyEpoch         int                      `json:"key_epoch"`
	RecipientKeyID   string                   `json:"recipient_key_id"`
	NotBefore        string                   `json:"not_before"`
	ExpiresAt        string                   `json:"expires_at"`
	CreatedAt        string                   `json:"created_at"`
	Issuer           contextPassportIssuer    `json:"issuer"`
	GrantDigest      string                   `json:"grant_digest"`
	Signature        contextPassportSignature `json:"signature"`
}

type frontierT7GrantUnsigned struct {
	SchemaID         string                 `json:"schema_id"`
	Version          int                    `json:"version"`
	GrantID          string                 `json:"grant_id"`
	Subject          frontierT7GrantSubject `json:"subject"`
	Project          string                 `json:"project"`
	Topics           []string               `json:"topics"`
	DataClasses      []string               `json:"data_classes"`
	Actions          []string               `json:"actions"`
	Purpose          string                 `json:"purpose"`
	UsageLimit       int                    `json:"usage_limit"`
	ParentGrantID    string                 `json:"parent_grant_id,omitempty"`
	AncestorGrantIDs []string               `json:"ancestor_grant_ids,omitempty"`
	DelegationDepth  int                    `json:"delegation_depth"`
	Approvers        []string               `json:"approvers"`
	KeyEpoch         int                    `json:"key_epoch"`
	RecipientKeyID   string                 `json:"recipient_key_id"`
	NotBefore        string                 `json:"not_before"`
	ExpiresAt        string                 `json:"expires_at"`
	CreatedAt        string                 `json:"created_at"`
	Issuer           contextPassportIssuer  `json:"issuer"`
}

func frontierT7GrantUnsignedValue(grant frontierT7CollaborativeGrant) frontierT7GrantUnsigned {
	return frontierT7GrantUnsigned{
		SchemaID: grant.SchemaID, Version: grant.Version, GrantID: grant.GrantID, Subject: grant.Subject,
		Project: grant.Project, Topics: grant.Topics, DataClasses: grant.DataClasses, Actions: grant.Actions,
		Purpose: grant.Purpose, UsageLimit: grant.UsageLimit, ParentGrantID: grant.ParentGrantID,
		AncestorGrantIDs: grant.AncestorGrantIDs,
		DelegationDepth:  grant.DelegationDepth, Approvers: grant.Approvers, KeyEpoch: grant.KeyEpoch,
		RecipientKeyID: grant.RecipientKeyID, NotBefore: grant.NotBefore, ExpiresAt: grant.ExpiresAt,
		CreatedAt: grant.CreatedAt, Issuer: grant.Issuer,
	}
}

func frontierT7GrantAncestorIDs(grant frontierT7CollaborativeGrant) ([]string, bool) {
	if grant.DelegationDepth == 0 {
		return []string{}, grant.ParentGrantID == "" && len(grant.AncestorGrantIDs) == 0
	}
	// Pre-release depth-one grants did not carry the explicit ancestry vector.
	// Their signed parent id remains sufficient for the single-hop case.
	if len(grant.AncestorGrantIDs) == 0 {
		if grant.DelegationDepth == 1 && grant.ParentGrantID != "" {
			return []string{grant.ParentGrantID}, true
		}
		return nil, false
	}
	if len(grant.AncestorGrantIDs) != grant.DelegationDepth || grant.AncestorGrantIDs[len(grant.AncestorGrantIDs)-1] != grant.ParentGrantID {
		return nil, false
	}
	seen := make(map[string]struct{}, len(grant.AncestorGrantIDs))
	for _, ancestorID := range grant.AncestorGrantIDs {
		if _, err := frontierT7SafeID(ancestorID, "ancestor_grant_id", 200); err != nil {
			return nil, false
		}
		if _, exists := seen[ancestorID]; exists || ancestorID == grant.GrantID {
			return nil, false
		}
		seen[ancestorID] = struct{}{}
	}
	return append([]string(nil), grant.AncestorGrantIDs...), true
}

func frontierT7SubjectFresh(observedAt string, now time.Time) bool {
	observed, err := time.Parse(time.RFC3339Nano, observedAt)
	if err != nil || now.IsZero() {
		return false
	}
	return !observed.After(now.Add(2*time.Minute)) && observed.After(now.Add(-frontierT7MaxSubjectAge))
}

type frontierT7GrantCreateRequest struct {
	Subject         frontierT7GrantSubject
	Project         string
	Topics          []string
	DataClasses     []string
	Actions         []string
	Purpose         string
	UsageLimit      int
	Parent          *frontierT7CollaborativeGrant
	DelegationDepth int
	Approvers       []string
	KeyEpoch        int
	RecipientKeyID  string
	NotBefore       time.Time
	ExpiresAt       time.Time
}

func frontierT7VerifyGrant(grant frontierT7CollaborativeGrant) []string {
	findings := []string{}
	if grant.SchemaID != frontierT7CollaborativeGrantSchemaID || grant.Version != 1 {
		findings = append(findings, "unsupported_grant")
	}
	unsigned := frontierT7GrantUnsignedValue(grant)
	if grant.GrantDigest != frontierT7Digest(unsigned) {
		findings = append(findings, "grant_digest_mismatch")
	}
	if !verifySignedBytes(struct {
		GrantDigest string                  `json:"grant_digest"`
		Grant       frontierT7GrantUnsigned `json:"grant"`
	}{grant.GrantDigest, unsigned}, grant.Signature, grant.Issuer) {
		findings = append(findings, "grant_signature_invalid")
	}
	if !frontierT7ValidDigest(grant.Subject.SnapshotDigest) {
		findings = append(findings, "subject_snapshot_invalid")
	}
	if _, err := frontierT7SafeID(grant.Subject.SubjectID, "subject_id", 160); err != nil {
		findings = append(findings, "subject_id_invalid")
	}
	if _, err := frontierT7SafeID(grant.Subject.WorkspaceID, "workspace_id", 160); err != nil {
		findings = append(findings, "subject_workspace_invalid")
	}
	if _, err := time.Parse(time.RFC3339Nano, grant.Subject.ObservedAt); err != nil {
		findings = append(findings, "subject_observed_at_invalid")
	}
	if normalized, err := frontierT7SortedUnique(grant.Subject.Roles, frontierT7MaxGrantItems); err != nil || len(normalized) == 0 || !jsonValuesEqual(normalized, grant.Subject.Roles) {
		findings = append(findings, "subject_roles_invalid")
	}
	if project, err := sanitizeMemoryProject(grant.Project); err != nil || project != grant.Project {
		findings = append(findings, "project_invalid")
	}
	for label, values := range map[string][]string{"topics": grant.Topics, "data_classes": grant.DataClasses, "actions": grant.Actions, "approvers": grant.Approvers} {
		normalized, err := frontierT7SortedUnique(values, frontierT7MaxGrantItems)
		if err != nil || len(normalized) == 0 || !jsonValuesEqual(normalized, values) {
			findings = append(findings, label+"_invalid")
		}
	}
	if _, err := frontierT7SafeID(grant.Purpose, "purpose", 240); err != nil {
		findings = append(findings, "purpose_invalid")
	}
	if _, err := frontierT7SafeID(grant.RecipientKeyID, "recipient_key_id", 200); err != nil {
		findings = append(findings, "recipient_invalid")
	}
	created, createdErr := time.Parse(time.RFC3339Nano, grant.CreatedAt)
	notBefore, beforeErr := time.Parse(time.RFC3339Nano, grant.NotBefore)
	expires, expiresErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if createdErr != nil || beforeErr != nil || expiresErr != nil || expires.Before(notBefore) || expires.Equal(notBefore) || expires.Sub(notBefore) > 30*24*time.Hour || created.After(expires) {
		findings = append(findings, "grant_time_bounds_invalid")
	}
	if grant.UsageLimit < 1 || grant.UsageLimit > 100000 || grant.KeyEpoch < 1 || grant.DelegationDepth < 0 || grant.DelegationDepth > 4 {
		findings = append(findings, "grant_bounds_invalid")
	}
	if (grant.DelegationDepth == 0) != (grant.ParentGrantID == "") {
		findings = append(findings, "parent_depth_invalid")
	}
	if _, valid := frontierT7GrantAncestorIDs(grant); !valid {
		findings = append(findings, "ancestor_chain_invalid")
	}
	return uniqueSortedStrings(findings)
}

func frontierT7CreateCollaborativeGrant(keys *contextIdentityKeys, request frontierT7GrantCreateRequest, now time.Time) (frontierT7CollaborativeGrant, error) {
	if keys == nil {
		return frontierT7CollaborativeGrant{}, errors.New("signing identity is required")
	}
	project, err := sanitizeMemoryProject(request.Project)
	if err != nil {
		return frontierT7CollaborativeGrant{}, err
	}
	request.Subject.SubjectID, err = frontierT7SafeID(request.Subject.SubjectID, "subject_id", 160)
	if err != nil || !frontierT7ValidDigest(request.Subject.SnapshotDigest) {
		return frontierT7CollaborativeGrant{}, errors.New("a valid subject snapshot is required")
	}
	request.Subject.WorkspaceID, err = frontierT7SafeID(request.Subject.WorkspaceID, "workspace_id", 160)
	if err != nil {
		return frontierT7CollaborativeGrant{}, err
	}
	observedAt, observedErr := time.Parse(time.RFC3339Nano, request.Subject.ObservedAt)
	if observedErr != nil || !frontierT7SubjectFresh(request.Subject.ObservedAt, now) {
		return frontierT7CollaborativeGrant{}, errors.New("subject observed_at is invalid")
	}
	request.Subject.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
	request.Subject.Roles, err = frontierT7SortedUnique(request.Subject.Roles, frontierT7MaxGrantItems)
	if err != nil || len(request.Subject.Roles) == 0 {
		return frontierT7CollaborativeGrant{}, errors.New("subject roles are required")
	}
	topics, err := frontierT7SortedUnique(request.Topics, frontierT7MaxGrantItems)
	if err != nil || len(topics) == 0 {
		return frontierT7CollaborativeGrant{}, errors.New("one or more topics are required")
	}
	dataClasses, err := frontierT7SortedUnique(request.DataClasses, frontierT7MaxGrantItems)
	if err != nil || len(dataClasses) == 0 {
		return frontierT7CollaborativeGrant{}, errors.New("one or more data classes are required")
	}
	actions, err := frontierT7SortedUnique(request.Actions, frontierT7MaxGrantItems)
	if err != nil || len(actions) == 0 {
		return frontierT7CollaborativeGrant{}, errors.New("one or more grant actions are required")
	}
	approvers, err := frontierT7SortedUnique(request.Approvers, frontierT7MaxGrantItems)
	if err != nil || len(approvers) == 0 {
		return frontierT7CollaborativeGrant{}, errors.New("one or more approvers are required")
	}
	purpose, err := frontierT7SafeID(request.Purpose, "purpose", 240)
	if err != nil {
		return frontierT7CollaborativeGrant{}, err
	}
	recipient, err := frontierT7SafeID(request.RecipientKeyID, "recipient_key_id", 200)
	if err != nil {
		return frontierT7CollaborativeGrant{}, err
	}
	if request.UsageLimit < 1 || request.UsageLimit > 100000 || request.KeyEpoch < 1 || request.DelegationDepth < 0 || request.DelegationDepth > 4 {
		return frontierT7CollaborativeGrant{}, errors.New("grant bounds are invalid")
	}
	if request.NotBefore.IsZero() {
		request.NotBefore = now
	}
	if !request.ExpiresAt.After(now) || !request.ExpiresAt.After(request.NotBefore) || request.ExpiresAt.Sub(request.NotBefore) > 30*24*time.Hour {
		return frontierT7CollaborativeGrant{}, errors.New("grant expiry must be within 30 days")
	}
	parentID := ""
	ancestorGrantIDs := []string{}
	if request.Parent != nil {
		parent := *request.Parent
		if findings := frontierT7VerifyGrant(parent); len(findings) > 0 {
			return frontierT7CollaborativeGrant{}, fmt.Errorf("parent grant invalid: %s", strings.Join(findings, ","))
		}
		parentNotBefore := mustParseFrontierT7Time(parent.NotBefore)
		if parent.Project != project || !frontierT7StringSubset(topics, parent.Topics) || !frontierT7StringSubset(dataClasses, parent.DataClasses) || !frontierT7StringSubset(actions, parent.Actions) ||
			!jsonValuesEqual(request.Subject, parent.Subject) || purpose != parent.Purpose || recipient != parent.RecipientKeyID || request.KeyEpoch != parent.KeyEpoch ||
			!frontierT7StringSubset(approvers, parent.Approvers) || request.NotBefore.Before(parentNotBefore) || request.UsageLimit > parent.UsageLimit ||
			request.DelegationDepth != parent.DelegationDepth+1 || request.DelegationDepth > 4 || !request.ExpiresAt.Before(mustParseFrontierT7Time(parent.ExpiresAt).Add(time.Nanosecond)) {
			return frontierT7CollaborativeGrant{}, errors.New("delegated grant exceeds parent authority")
		}
		if !frontierT7StringSubset([]string{"delegate"}, parent.Actions) {
			return frontierT7CollaborativeGrant{}, errors.New("parent grant does not permit delegation")
		}
		parentAncestors, valid := frontierT7GrantAncestorIDs(parent)
		if !valid {
			return frontierT7CollaborativeGrant{}, errors.New("parent grant ancestry is invalid")
		}
		ancestorGrantIDs = append(parentAncestors, parent.GrantID)
		if len(ancestorGrantIDs) != request.DelegationDepth {
			return frontierT7CollaborativeGrant{}, errors.New("delegation ancestry does not match depth")
		}
		parentID = parent.GrantID
	}
	issuer := contextPassportIssuer{InstanceID: keys.InstanceID, SigningKeyID: keys.SigningKeyID, SigningPublicKey: keys.SigningPublicKey}
	createdAt := now.UTC().Format(time.RFC3339Nano)
	seed := map[string]any{
		"subject": request.Subject, "project": project, "topics": topics, "classes": dataClasses, "actions": actions,
		"purpose": purpose, "usage_limit": request.UsageLimit, "parent": parentID, "ancestors": ancestorGrantIDs, "delegation_depth": request.DelegationDepth,
		"approvers": approvers, "recipient": recipient, "epoch": request.KeyEpoch,
		"not_before": request.NotBefore.UTC().Format(time.RFC3339Nano), "expires": request.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"created_at": createdAt, "issuer_instance_id": issuer.InstanceID, "issuer_key_id": issuer.SigningKeyID,
	}
	grant := frontierT7CollaborativeGrant{
		SchemaID: frontierT7CollaborativeGrantSchemaID, Version: 1, GrantID: "ctxgrant_" + strings.TrimPrefix(frontierT7Digest(seed), "sha256:")[:24],
		Subject: request.Subject, Project: project, Topics: topics, DataClasses: dataClasses, Actions: actions,
		Purpose: purpose, UsageLimit: request.UsageLimit, ParentGrantID: parentID, AncestorGrantIDs: ancestorGrantIDs, DelegationDepth: request.DelegationDepth,
		Approvers: approvers, KeyEpoch: request.KeyEpoch, RecipientKeyID: recipient,
		NotBefore: request.NotBefore.UTC().Format(time.RFC3339Nano), ExpiresAt: request.ExpiresAt.UTC().Format(time.RFC3339Nano),
		CreatedAt: createdAt, Issuer: issuer,
	}
	unsigned := frontierT7GrantUnsignedValue(grant)
	grant.GrantDigest = frontierT7Digest(unsigned)
	grant.Signature, err = signBytesWithIdentity(struct {
		GrantDigest string                  `json:"grant_digest"`
		Grant       frontierT7GrantUnsigned `json:"grant"`
	}{grant.GrantDigest, unsigned}, keys)
	return grant, err
}

func mustParseFrontierT7Time(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

type frontierT7GrantUseRequest struct {
	Project               string
	Topic                 string
	DataClass             string
	Action                string
	Purpose               string
	RecipientKeyID        string
	SubjectSnapshotDigest string
	KeyEpoch              int
	UsageCount            int
	RevokedGrantIDs       map[string]struct{}
	Now                   time.Time
	ClockSkew             time.Duration
}

type frontierT7GrantDecision struct {
	SchemaID           string   `json:"schema_id"`
	GrantID            string   `json:"grant_id"`
	GrantDigest        string   `json:"grant_digest"`
	Allowed            bool     `json:"allowed"`
	Reasons            []string `json:"reasons"`
	CapabilityDigest   string   `json:"capability_digest"`
	RemainingUses      int      `json:"remaining_uses"`
	NetworkCalls       int      `json:"network_calls"`
	ExecutionPerformed bool     `json:"execution_performed"`
}

func frontierT7AuthorizeGrant(grant frontierT7CollaborativeGrant, request frontierT7GrantUseRequest) frontierT7GrantDecision {
	reasons := frontierT7VerifyGrant(grant)
	if request.Now.IsZero() {
		request.Now = time.Now().UTC()
	}
	if request.ClockSkew <= 0 || request.ClockSkew > 2*time.Minute {
		request.ClockSkew = 2 * time.Minute
	}
	notBefore, beforeErr := time.Parse(time.RFC3339Nano, grant.NotBefore)
	expires, expiresErr := time.Parse(time.RFC3339Nano, grant.ExpiresAt)
	if beforeErr != nil || expiresErr != nil || request.Now.Add(request.ClockSkew).Before(notBefore) || !request.Now.Add(-request.ClockSkew).Before(expires) {
		reasons = append(reasons, "grant_time_window_invalid")
	}
	if !frontierT7SubjectFresh(grant.Subject.ObservedAt, request.Now) {
		reasons = append(reasons, "stale_subject")
	}
	if _, revoked := request.RevokedGrantIDs[grant.GrantID]; revoked {
		reasons = append(reasons, "grant_revoked")
	}
	if ancestors, valid := frontierT7GrantAncestorIDs(grant); !valid {
		reasons = append(reasons, "ancestor_chain_invalid")
	} else {
		for _, ancestorID := range ancestors {
			if _, revoked := request.RevokedGrantIDs[ancestorID]; revoked {
				reasons = append(reasons, "ancestor_grant_revoked")
				break
			}
		}
	}
	if grant.Project != request.Project {
		reasons = append(reasons, "project_scope_mismatch")
	}
	if !frontierT7StringSubset([]string{strings.ToLower(strings.TrimSpace(request.Topic))}, grant.Topics) {
		reasons = append(reasons, "topic_scope_mismatch")
	}
	if !frontierT7StringSubset([]string{strings.ToLower(strings.TrimSpace(request.DataClass))}, grant.DataClasses) {
		reasons = append(reasons, "data_class_escalation")
	}
	if !frontierT7StringSubset([]string{strings.ToLower(strings.TrimSpace(request.Action))}, grant.Actions) {
		reasons = append(reasons, "action_escalation")
	}
	if request.Purpose != grant.Purpose {
		reasons = append(reasons, "purpose_mismatch")
	}
	if request.RecipientKeyID != grant.RecipientKeyID {
		reasons = append(reasons, "recipient_mismatch")
	}
	if request.SubjectSnapshotDigest != grant.Subject.SnapshotDigest {
		reasons = append(reasons, "subject_snapshot_changed")
	}
	if request.KeyEpoch != grant.KeyEpoch {
		reasons = append(reasons, "key_epoch_mismatch")
	}
	if request.UsageCount < 0 || request.UsageCount >= grant.UsageLimit {
		reasons = append(reasons, "usage_limit_exhausted")
	}
	reasons = uniqueSortedStrings(reasons)
	return frontierT7GrantDecision{
		SchemaID: frontierT7GrantDecisionSchemaID, GrantID: grant.GrantID, GrantDigest: grant.GrantDigest, Allowed: len(reasons) == 0, Reasons: reasons,
		CapabilityDigest: frontierT7Digest(map[string]any{"project": request.Project, "topic": request.Topic, "data_class": request.DataClass, "action": request.Action, "purpose": request.Purpose}),
		RemainingUses:    maxInt(0, grant.UsageLimit-request.UsageCount), NetworkCalls: 0, ExecutionPerformed: false,
	}
}

type frontierT7Provenance struct {
	SchemaID               string         `json:"schema_id"`
	SourceAlias            string         `json:"source_alias"`
	RelativeLocator        string         `json:"relative_locator"`
	SourceDigest           string         `json:"source_digest"`
	ContentDigest          string         `json:"content_digest"`
	ImporterDigest         string         `json:"importer_digest"`
	ConfigDigest           string         `json:"config_digest"`
	TransformationManifest map[string]any `json:"transformation_manifest"`
	RedactionManifest      map[string]any `json:"redaction_manifest"`
	Classification         string         `json:"classification"`
	License                string         `json:"license"`
	Consent                bool           `json:"consent"`
	DeletionLocator        string         `json:"deletion_locator"`
	InstructionShaped      bool           `json:"instruction_shaped"`
}

type frontierT7ImportRecord struct {
	Provenance frontierT7Provenance `json:"provenance"`
	TopicPath  string               `json:"topic_path"`
	Symlink    bool                 `json:"symlink"`
	Binary     bool                 `json:"binary"`
}

type frontierT7ImportMapping struct {
	SourceKey     string               `json:"source_key"`
	MemoryID      string               `json:"memory_id"`
	ContentDigest string               `json:"content_digest"`
	TopicPath     string               `json:"topic_path"`
	Provenance    frontierT7Provenance `json:"provenance"`
	Action        string               `json:"action"`
}

type frontierT7ImportPlan struct {
	SchemaID              string                    `json:"schema_id"`
	Version               int                       `json:"version"`
	PlanID                string                    `json:"plan_id"`
	Project               string                    `json:"project"`
	BatchSize             int                       `json:"batch_size"`
	BatchCount            int                       `json:"batch_count"`
	Mappings              []frontierT7ImportMapping `json:"mappings"`
	PlanDigest            string                    `json:"plan_digest"`
	Warnings              []string                  `json:"warnings"`
	AtomicBatches         bool                      `json:"atomic_batches"`
	Resumable             bool                      `json:"resumable"`
	ExecutionOwner        string                    `json:"execution_owner"`
	ExecutionPerformed    bool                      `json:"execution_performed"`
	NetworkCalls          int                       `json:"network_calls"`
	OrdinaryMemoryMutated bool                      `json:"ordinary_memory_mutated"`
}

func frontierT7RelativeLocator(value string) (string, error) {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	clean := path.Clean(value)
	containsControl := strings.IndexFunc(value, unicode.IsControl) >= 0
	if value == "" || containsControl || path.IsAbs(value) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") || len(clean) > 1024 {
		return "", errors.New("locator must be a bounded relative path")
	}
	return clean, nil
}

func frontierT7ImportBatchCount(plan frontierT7ImportPlan) (int, error) {
	if plan.BatchSize < 1 || plan.BatchSize > frontierT7MaxBatchRows || len(plan.Mappings) < 1 || len(plan.Mappings) > frontierT7MaxImportRows {
		return 0, errors.New("import plan batch bounds are invalid")
	}
	return (len(plan.Mappings) + plan.BatchSize - 1) / plan.BatchSize, nil
}

func frontierT7BuildImportPlan(project string, records []frontierT7ImportRecord, existing map[string]string, batchSize int) (frontierT7ImportPlan, error) {
	project, err := sanitizeMemoryProject(project)
	if err != nil {
		return frontierT7ImportPlan{}, err
	}
	if len(records) < 1 || len(records) > frontierT7MaxImportRows {
		return frontierT7ImportPlan{}, errors.New("import plan requires one to 256 records")
	}
	batchSize = clampInt(batchSize, 1, frontierT7MaxBatchRows)
	mappings := make([]frontierT7ImportMapping, 0, len(records))
	warnings := []string{}
	seen := map[string]string{}
	for _, record := range records {
		if record.Symlink || record.Binary {
			return frontierT7ImportPlan{}, errors.New("symlinks and binary records require an explicit external adapter policy")
		}
		provenance := record.Provenance
		if provenance.SchemaID != frontierT7ProvenanceSchemaID || !provenance.Consent || strings.TrimSpace(provenance.License) == "" ||
			!frontierT7ValidDigest(provenance.SourceDigest) || !frontierT7ValidDigest(provenance.ContentDigest) || !frontierT7ValidDigest(provenance.ImporterDigest) || !frontierT7ValidDigest(provenance.ConfigDigest) {
			return frontierT7ImportPlan{}, errors.New("record provenance is incomplete or invalid")
		}
		alias, safeErr := frontierT7SafeID(strings.ToLower(provenance.SourceAlias), "source_alias", 120)
		if safeErr != nil {
			return frontierT7ImportPlan{}, safeErr
		}
		locator, safeErr := frontierT7RelativeLocator(provenance.RelativeLocator)
		if safeErr != nil {
			return frontierT7ImportPlan{}, safeErr
		}
		deletionLocator, safeErr := frontierT7RelativeLocator(provenance.DeletionLocator)
		if safeErr != nil {
			return frontierT7ImportPlan{}, fmt.Errorf("deletion locator: %w", safeErr)
		}
		if len(provenance.TransformationManifest) == 0 || len(provenance.RedactionManifest) == 0 {
			return frontierT7ImportPlan{}, errors.New("transformation and redaction manifests are required")
		}
		topicPath, safeErr := frontierT7RelativeLocator(record.TopicPath)
		if safeErr != nil {
			return frontierT7ImportPlan{}, fmt.Errorf("topic path: %w", safeErr)
		}
		classification, safeErr := frontierT7SafeID(provenance.Classification, "classification", 120)
		if safeErr != nil {
			return frontierT7ImportPlan{}, safeErr
		}
		license, safeErr := frontierT7SafeID(provenance.License, "license", 240)
		if safeErr != nil {
			return frontierT7ImportPlan{}, safeErr
		}
		redactionStats := &portableRedactionStats{}
		transformationManifest := portableMap(provenance.TransformationManifest, redactionStats)
		redactionManifest := portableMap(provenance.RedactionManifest, redactionStats)
		if len(transformationManifest) == 0 || len(redactionManifest) == 0 {
			return frontierT7ImportPlan{}, errors.New("provenance manifests become empty after secret redaction")
		}
		provenance.SourceAlias = alias
		provenance.RelativeLocator = locator
		provenance.DeletionLocator = deletionLocator
		provenance.TransformationManifest = transformationManifest
		provenance.RedactionManifest = redactionManifest
		provenance.Classification = classification
		provenance.License = license
		sourceKey := frontierT7Digest(map[string]any{"source_alias": alias, "relative_locator": locator, "source_digest": provenance.SourceDigest})
		if prior, exists := seen[sourceKey]; exists {
			if prior != provenance.ContentDigest {
				return frontierT7ImportPlan{}, errors.New("duplicate source locator has conflicting content")
			}
			warnings = append(warnings, "duplicate_source_record_collapsed")
			continue
		}
		seen[sourceKey] = provenance.ContentDigest
		action := "import"
		if prior := strings.TrimSpace(existing[sourceKey]); prior != "" {
			if prior != provenance.ContentDigest {
				return frontierT7ImportPlan{}, errors.New("existing source mapping conflicts with new content")
			}
			action = "skip_duplicate"
		}
		if provenance.InstructionShaped {
			warnings = append(warnings, "instruction_shaped_content_is_untrusted_data")
		}
		memoryID := "memimp_" + strings.TrimPrefix(frontierT7Digest(map[string]any{"project": project, "source": sourceKey, "content": provenance.ContentDigest}), "sha256:")[:24]
		mappings = append(mappings, frontierT7ImportMapping{
			SourceKey: sourceKey, MemoryID: memoryID, ContentDigest: provenance.ContentDigest,
			TopicPath: topicPath, Provenance: provenance, Action: action,
		})
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].SourceKey < mappings[j].SourceKey })
	unsigned := map[string]any{"project": project, "batch_size": batchSize, "mappings": mappings}
	planDigest := frontierT7Digest(unsigned)
	return frontierT7ImportPlan{
		SchemaID: frontierT7ImportPlanSchemaID, Version: 1, PlanID: "importplan_" + strings.TrimPrefix(planDigest, "sha256:")[:24],
		Project: project, BatchSize: batchSize, BatchCount: (len(mappings) + batchSize - 1) / batchSize,
		Mappings: mappings, PlanDigest: planDigest, Warnings: uniqueSortedStrings(warnings), AtomicBatches: true, Resumable: true,
		ExecutionOwner: "external_import_worker", ExecutionPerformed: false, NetworkCalls: 0, OrdinaryMemoryMutated: false,
	}, nil
}

type frontierT7ImportReceipt struct {
	SchemaID                string                    `json:"schema_id"`
	Version                 int                       `json:"version"`
	ReceiptID               string                    `json:"receipt_id"`
	PlanID                  string                    `json:"plan_id"`
	PlanDigest              string                    `json:"plan_digest"`
	BatchIndex              int                       `json:"batch_index"`
	BatchCount              int                       `json:"batch_count"`
	Status                  string                    `json:"status"`
	Mappings                []frontierT7ImportMapping `json:"mappings"`
	ResumeBatchIndex        int                       `json:"resume_batch_index"`
	ExternalExecutionDigest string                    `json:"external_execution_digest"`
	ExecutionOwner          string                    `json:"execution_owner"`
	RecordedAt              string                    `json:"recorded_at"`
	ReceiptDigest           string                    `json:"receipt_digest"`
	Atomic                  bool                      `json:"atomic"`
	GatewayMutatedMemory    bool                      `json:"gateway_mutated_memory"`
	NetworkCalls            int                       `json:"network_calls"`
}

func frontierT7ImportReceiptDigest(receipt frontierT7ImportReceipt) string {
	return frontierT7Digest(map[string]any{
		"schema_id": receipt.SchemaID, "version": receipt.Version, "receipt_id": receipt.ReceiptID,
		"plan_id": receipt.PlanID, "plan_digest": receipt.PlanDigest, "batch_index": receipt.BatchIndex,
		"batch_count": receipt.BatchCount, "status": receipt.Status, "mappings": receipt.Mappings,
		"resume_batch_index": receipt.ResumeBatchIndex, "external_execution_digest": receipt.ExternalExecutionDigest,
		"execution_owner": receipt.ExecutionOwner, "recorded_at": receipt.RecordedAt,
		"atomic": receipt.Atomic, "gateway_mutated_memory": receipt.GatewayMutatedMemory, "network_calls": receipt.NetworkCalls,
	})
}

func frontierT7ValidImportReceipt(receipt frontierT7ImportReceipt, plan frontierT7ImportPlan, expectedBatch int) bool {
	expectedBatchCount, err := frontierT7ImportBatchCount(plan)
	if err != nil || plan.BatchCount != expectedBatchCount {
		return false
	}
	return receipt.SchemaID == frontierT7ImportReceiptSchemaID && receipt.Version == 1 && receipt.PlanID == plan.PlanID &&
		receipt.PlanDigest == plan.PlanDigest && receipt.BatchIndex == expectedBatch && receipt.BatchCount == expectedBatchCount &&
		receipt.Status == "committed" && frontierT7ValidDigest(receipt.ExternalExecutionDigest) && receipt.ExecutionOwner == "external_import_worker" &&
		receipt.Atomic && !receipt.GatewayMutatedMemory && receipt.NetworkCalls == 0 &&
		receipt.ResumeBatchIndex == expectedBatch+1 && receipt.ReceiptDigest == frontierT7ImportReceiptDigest(receipt)
}

func frontierT7ValidateImportPlan(plan frontierT7ImportPlan) error {
	expectedBatchCount, batchErr := frontierT7ImportBatchCount(plan)
	if batchErr != nil || plan.SchemaID != frontierT7ImportPlanSchemaID || plan.Version != 1 || plan.BatchCount != expectedBatchCount ||
		plan.PlanDigest != frontierT7Digest(map[string]any{"project": plan.Project, "batch_size": plan.BatchSize, "mappings": plan.Mappings}) ||
		plan.PlanID != "importplan_"+strings.TrimPrefix(plan.PlanDigest, "sha256:")[:24] || !plan.AtomicBatches || !plan.Resumable || plan.ExecutionPerformed || plan.NetworkCalls != 0 || plan.OrdinaryMemoryMutated || plan.ExecutionOwner != "external_import_worker" {
		return errors.New("import plan digest mismatch")
	}
	return nil
}

func frontierT7CommitImportBatch(plan frontierT7ImportPlan, batchIndex int, prior map[int]frontierT7ImportReceipt, externalExecutionDigest string, now time.Time) (frontierT7ImportReceipt, error) {
	if err := frontierT7ValidateImportPlan(plan); err != nil {
		return frontierT7ImportReceipt{}, err
	}
	expectedBatchCount, _ := frontierT7ImportBatchCount(plan)
	if batchIndex < 0 || batchIndex >= expectedBatchCount {
		return frontierT7ImportReceipt{}, errors.New("batch index is outside the plan")
	}
	if !frontierT7ValidDigest(externalExecutionDigest) {
		return frontierT7ImportReceipt{}, errors.New("external execution digest is required")
	}
	for index := 0; index < batchIndex; index++ {
		if receipt, exists := prior[index]; !exists || !frontierT7ValidImportReceipt(receipt, plan, index) {
			return frontierT7ImportReceipt{}, errors.New("prior batch receipt is missing or invalid")
		}
	}
	if existing, exists := prior[batchIndex]; exists {
		if !frontierT7ValidImportReceipt(existing, plan, batchIndex) || existing.ExternalExecutionDigest != externalExecutionDigest {
			return frontierT7ImportReceipt{}, errors.New("conflicting batch receipt")
		}
		return existing, nil
	}
	start := batchIndex * plan.BatchSize
	end := minInt(len(plan.Mappings), start+plan.BatchSize)
	rows := append([]frontierT7ImportMapping(nil), plan.Mappings[start:end]...)
	receiptID := "impreceipt_" + strings.TrimPrefix(frontierT7Digest(map[string]any{"plan": plan.PlanDigest, "batch": batchIndex, "rows": rows}), "sha256:")[:24]
	receipt := frontierT7ImportReceipt{
		SchemaID: frontierT7ImportReceiptSchemaID, Version: 1, ReceiptID: receiptID, PlanID: plan.PlanID, PlanDigest: plan.PlanDigest,
		BatchIndex: batchIndex, BatchCount: expectedBatchCount, Status: "committed", Mappings: rows, ResumeBatchIndex: batchIndex + 1,
		ExternalExecutionDigest: externalExecutionDigest, ExecutionOwner: "external_import_worker",
		RecordedAt: now.UTC().Format(time.RFC3339Nano), Atomic: true, GatewayMutatedMemory: false, NetworkCalls: 0,
	}
	receipt.ReceiptDigest = frontierT7ImportReceiptDigest(receipt)
	return receipt, nil
}

type frontierT7ContinuationManifest struct {
	SchemaID                    string                   `json:"schema_id"`
	Version                     int                      `json:"version"`
	ManifestID                  string                   `json:"manifest_id"`
	Project                     string                   `json:"project"`
	PassportID                  string                   `json:"passport_id"`
	PassportDigest              string                   `json:"passport_digest"`
	LineageDigest               string                   `json:"lineage_digest"`
	CheckpointDigest            string                   `json:"checkpoint_digest"`
	LifecycleReceiptDigest      string                   `json:"lifecycle_receipt_digest"`
	UnresolvedObligationDigests []string                 `json:"unresolved_obligation_digests"`
	RepositoryConstraintDigest  string                   `json:"repository_constraint_digest"`
	DestinationSessionDigest    string                   `json:"destination_session_digest"`
	RecipientKeyID              string                   `json:"recipient_key_id"`
	GrantID                     string                   `json:"grant_id"`
	GrantDigest                 string                   `json:"grant_digest"`
	Transport                   string                   `json:"transport"`
	CreatedAt                   string                   `json:"created_at"`
	ExpiresAt                   string                   `json:"expires_at"`
	Issuer                      contextPassportIssuer    `json:"issuer"`
	ManifestDigest              string                   `json:"manifest_digest"`
	Signature                   contextPassportSignature `json:"signature"`
}

type frontierT7ContinuationUnsigned struct {
	SchemaID                    string                `json:"schema_id"`
	Version                     int                   `json:"version"`
	ManifestID                  string                `json:"manifest_id"`
	Project                     string                `json:"project"`
	PassportID                  string                `json:"passport_id"`
	PassportDigest              string                `json:"passport_digest"`
	LineageDigest               string                `json:"lineage_digest"`
	CheckpointDigest            string                `json:"checkpoint_digest"`
	LifecycleReceiptDigest      string                `json:"lifecycle_receipt_digest"`
	UnresolvedObligationDigests []string              `json:"unresolved_obligation_digests"`
	RepositoryConstraintDigest  string                `json:"repository_constraint_digest"`
	DestinationSessionDigest    string                `json:"destination_session_digest"`
	RecipientKeyID              string                `json:"recipient_key_id"`
	GrantID                     string                `json:"grant_id"`
	GrantDigest                 string                `json:"grant_digest"`
	Transport                   string                `json:"transport"`
	CreatedAt                   string                `json:"created_at"`
	ExpiresAt                   string                `json:"expires_at"`
	Issuer                      contextPassportIssuer `json:"issuer"`
}

func frontierT7ContinuationUnsignedValue(manifest frontierT7ContinuationManifest) frontierT7ContinuationUnsigned {
	return frontierT7ContinuationUnsigned{
		SchemaID: manifest.SchemaID, Version: manifest.Version, ManifestID: manifest.ManifestID, Project: manifest.Project,
		PassportID: manifest.PassportID, PassportDigest: manifest.PassportDigest, LineageDigest: manifest.LineageDigest,
		CheckpointDigest: manifest.CheckpointDigest, LifecycleReceiptDigest: manifest.LifecycleReceiptDigest,
		UnresolvedObligationDigests: manifest.UnresolvedObligationDigests, RepositoryConstraintDigest: manifest.RepositoryConstraintDigest,
		DestinationSessionDigest: manifest.DestinationSessionDigest, RecipientKeyID: manifest.RecipientKeyID,
		GrantID: manifest.GrantID, GrantDigest: manifest.GrantDigest, Transport: manifest.Transport,
		CreatedAt: manifest.CreatedAt, ExpiresAt: manifest.ExpiresAt, Issuer: manifest.Issuer,
	}
}

type frontierT7ContinuationRequest struct {
	Project                     string
	PassportID                  string
	PassportDigest              string
	LineageDigest               string
	CheckpointDigest            string
	LifecycleReceiptDigest      string
	UnresolvedObligationDigests []string
	RepositoryConstraintDigest  string
	DestinationSessionDigest    string
	RecipientKeyID              string
	Grant                       frontierT7CollaborativeGrant
	Transport                   string
	ExpiresAt                   time.Time
}

func frontierT7CreateContinuationManifest(keys *contextIdentityKeys, request frontierT7ContinuationRequest, now time.Time) (frontierT7ContinuationManifest, error) {
	if keys == nil {
		return frontierT7ContinuationManifest{}, errors.New("signing identity is required")
	}
	project, err := sanitizeMemoryProject(request.Project)
	if err != nil {
		return frontierT7ContinuationManifest{}, err
	}
	request.PassportID, err = frontierT7SafeID(request.PassportID, "passport_id", 200)
	if err != nil {
		return frontierT7ContinuationManifest{}, err
	}
	digests := append([]string{request.PassportDigest, request.LineageDigest, request.CheckpointDigest, request.LifecycleReceiptDigest, request.RepositoryConstraintDigest, request.DestinationSessionDigest, request.Grant.GrantDigest}, request.UnresolvedObligationDigests...)
	if len(request.UnresolvedObligationDigests) > frontierT7MaxObligations {
		return frontierT7ContinuationManifest{}, errors.New("too many unresolved obligations")
	}
	for _, digest := range digests {
		if !frontierT7ValidDigest(digest) {
			return frontierT7ContinuationManifest{}, errors.New("continuation references require SHA-256 digests")
		}
	}
	grantExpiry, grantExpiryErr := time.Parse(time.RFC3339Nano, request.Grant.ExpiresAt)
	if findings := frontierT7VerifyGrant(request.Grant); len(findings) > 0 || grantExpiryErr != nil || !now.Before(grantExpiry) || request.Grant.Project != project || request.Grant.RecipientKeyID != request.RecipientKeyID {
		return frontierT7ContinuationManifest{}, errors.New("continuation grant is invalid or out of scope")
	}
	transport, err := frontierT7SafeID(strings.ToLower(request.Transport), "transport", 80)
	if err != nil {
		return frontierT7ContinuationManifest{}, err
	}
	if !request.ExpiresAt.After(now) || request.ExpiresAt.Sub(now) > 24*time.Hour {
		return frontierT7ContinuationManifest{}, errors.New("continuation manifest expiry must be within 24 hours")
	}
	if request.ExpiresAt.After(grantExpiry) {
		request.ExpiresAt = grantExpiry
	}
	obligations := append([]string(nil), request.UnresolvedObligationDigests...)
	sort.Strings(obligations)
	issuer := contextPassportIssuer{InstanceID: keys.InstanceID, SigningKeyID: keys.SigningKeyID, SigningPublicKey: keys.SigningPublicKey}
	seed := map[string]any{"project": project, "passport": request.PassportDigest, "checkpoint": request.CheckpointDigest, "destination": request.DestinationSessionDigest, "recipient": request.RecipientKeyID, "grant": request.Grant.GrantDigest, "created": now.UTC().Format(time.RFC3339Nano)}
	manifest := frontierT7ContinuationManifest{
		SchemaID: frontierT7ContinuationSchemaID, Version: 1, ManifestID: "contmanifest_" + strings.TrimPrefix(frontierT7Digest(seed), "sha256:")[:24],
		Project: project, PassportID: request.PassportID, PassportDigest: request.PassportDigest, LineageDigest: request.LineageDigest,
		CheckpointDigest: request.CheckpointDigest, LifecycleReceiptDigest: request.LifecycleReceiptDigest,
		UnresolvedObligationDigests: obligations, RepositoryConstraintDigest: request.RepositoryConstraintDigest,
		DestinationSessionDigest: request.DestinationSessionDigest, RecipientKeyID: request.RecipientKeyID,
		GrantID: request.Grant.GrantID, GrantDigest: request.Grant.GrantDigest, Transport: transport,
		CreatedAt: now.UTC().Format(time.RFC3339Nano), ExpiresAt: request.ExpiresAt.UTC().Format(time.RFC3339Nano), Issuer: issuer,
	}
	unsigned := frontierT7ContinuationUnsignedValue(manifest)
	manifest.ManifestDigest = frontierT7Digest(unsigned)
	manifest.Signature, err = signBytesWithIdentity(struct {
		ManifestDigest string                         `json:"manifest_digest"`
		Manifest       frontierT7ContinuationUnsigned `json:"manifest"`
	}{manifest.ManifestDigest, unsigned}, keys)
	return manifest, err
}

type frontierT7ContinuationReconciliation struct {
	SchemaID                       string   `json:"schema_id"`
	ManifestID                     string   `json:"manifest_id"`
	Accepted                       bool     `json:"accepted"`
	DryRun                         bool     `json:"dry_run"`
	Findings                       []string `json:"findings"`
	Action                         string   `json:"action"`
	Transport                      string   `json:"transport"`
	TransportOwnedByContextLattice bool     `json:"transport_owned_by_contextlattice"`
	TransportExecuted              bool     `json:"transport_executed"`
	PrivateKeyExported             bool     `json:"private_key_exported"`
	OrdinaryMemoryMutated          bool     `json:"ordinary_memory_mutated"`
	GatewayExecutionPerformed      bool     `json:"gateway_execution_performed"`
	ModelExecutionPerformed        bool     `json:"model_execution_performed"`
	SubprocessExecutionPerformed   bool     `json:"subprocess_execution_performed"`
	NetworkCalls                   int      `json:"network_calls"`
}

type frontierT7ContinuationAuthorization struct {
	Topic                 string
	DataClass             string
	Purpose               string
	SubjectSnapshotDigest string
	KeyEpoch              int
	UsageCount            int
	RevokedGrantIDs       map[string]struct{}
}

type frontierT7ReplayGuard interface {
	checkAndRecord(manifestID, manifestDigest string) (string, bool, error)
}

type frontierT7MemoryReplayGuard struct {
	mu   sync.Mutex
	seen map[string]string
}

func newFrontierT7MemoryReplayGuard() *frontierT7MemoryReplayGuard {
	return &frontierT7MemoryReplayGuard{seen: map[string]string{}}
}

func (g *frontierT7MemoryReplayGuard) checkAndRecord(manifestID, manifestDigest string) (string, bool, error) {
	if g == nil {
		return "", false, errors.New("replay guard unavailable")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen == nil {
		g.seen = map[string]string{}
	}
	previous, exists := g.seen[manifestID]
	if !exists {
		g.seen[manifestID] = manifestDigest
	}
	return previous, exists, nil
}

func frontierT7ReconcileContinuation(manifest frontierT7ContinuationManifest, grant frontierT7CollaborativeGrant, now time.Time, localRecipientKeyID, expectedLineageDigest string, authorization frontierT7ContinuationAuthorization, replayGuard frontierT7ReplayGuard) frontierT7ContinuationReconciliation {
	findings := []string{}
	if manifest.SchemaID != frontierT7ContinuationSchemaID || manifest.Version != 1 {
		findings = append(findings, "unsupported_manifest")
	}
	unsigned := frontierT7ContinuationUnsignedValue(manifest)
	if manifest.ManifestDigest != frontierT7Digest(unsigned) {
		findings = append(findings, "manifest_digest_mismatch")
	}
	if !verifySignedBytes(struct {
		ManifestDigest string                         `json:"manifest_digest"`
		Manifest       frontierT7ContinuationUnsigned `json:"manifest"`
	}{manifest.ManifestDigest, unsigned}, manifest.Signature, manifest.Issuer) {
		findings = append(findings, "manifest_signature_invalid")
	}
	created, createdErr := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
	expires, expiresErr := time.Parse(time.RFC3339Nano, manifest.ExpiresAt)
	if createdErr != nil || expiresErr != nil || !expires.After(created) || !now.Before(expires) {
		findings = append(findings, "manifest_expired_or_invalid")
	}
	if manifest.RecipientKeyID != localRecipientKeyID {
		findings = append(findings, "wrong_recipient")
	}
	if manifest.LineageDigest != expectedLineageDigest {
		findings = append(findings, "divergent_lineage")
	}
	grantDecision := frontierT7AuthorizeGrant(grant, frontierT7GrantUseRequest{
		Project: manifest.Project, Topic: authorization.Topic, DataClass: authorization.DataClass, Action: "continue",
		Purpose: authorization.Purpose, RecipientKeyID: manifest.RecipientKeyID,
		SubjectSnapshotDigest: authorization.SubjectSnapshotDigest, KeyEpoch: authorization.KeyEpoch,
		UsageCount: authorization.UsageCount, RevokedGrantIDs: authorization.RevokedGrantIDs, Now: now,
	})
	if grant.SchemaID != frontierT7CollaborativeGrantSchemaID || !grantDecision.Allowed || grantDecision.SchemaID != frontierT7GrantDecisionSchemaID || grantDecision.GrantID != manifest.GrantID || grantDecision.GrantDigest != manifest.GrantDigest {
		findings = append(findings, "grant_denied_or_mismatched")
	}
	findings = uniqueSortedStrings(findings)
	if len(findings) == 0 {
		if replayGuard == nil {
			findings = append(findings, "replay_guard_unavailable")
		} else if previousDigest, exists, err := replayGuard.checkAndRecord(manifest.ManifestID, manifest.ManifestDigest); err != nil {
			findings = append(findings, "replay_guard_unavailable")
		} else if exists {
			if previousDigest == manifest.ManifestDigest {
				findings = append(findings, "manifest_replay")
			} else {
				findings = append(findings, "manifest_id_collision")
			}
		}
	}
	findings = uniqueSortedStrings(findings)
	action := "reject"
	if len(findings) == 0 {
		action = "dry_run_import_and_continue"
	}
	return frontierT7ContinuationReconciliation{
		SchemaID: frontierT7ReconciliationSchemaID, ManifestID: manifest.ManifestID, Accepted: len(findings) == 0,
		DryRun: true, Findings: findings, Action: action, Transport: manifest.Transport,
		TransportOwnedByContextLattice: false, TransportExecuted: false, PrivateKeyExported: false,
		OrdinaryMemoryMutated: false, GatewayExecutionPerformed: false, ModelExecutionPerformed: false,
		SubprocessExecutionPerformed: false, NetworkCalls: 0,
	}
}
