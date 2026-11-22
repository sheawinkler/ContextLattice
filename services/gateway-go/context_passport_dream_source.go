package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	dreamSourceExportFileEnv         = "CONTEXTLATTICE_DREAM_SOURCE_EXPORT_FILE"
	dreamSourceExportAuthoritySchema = "dream_source_export_authority.v1"
	dreamSourceObservationSchema     = "dream_source_observation.v1"
	dreamSourceMaxBytes              = 4096
	dreamSourceMaxGrantTTL           = 15 * time.Minute
)

// Require an exact field set and reject duplicate keys before any coercion.
func dreamSourceObject(raw []byte, fields ...string) (map[string]json.RawMessage, bool) {
	if len(raw) == 0 || len(raw) > dreamSourceMaxBytes || !utf8.Valid(raw) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, false
	}
	allowed := make(map[string]bool, len(fields))
	for _, key := range fields {
		allowed[key] = true
	}
	values := make(map[string]json.RawMessage, len(fields))
	for d.More() {
		token, err := d.Token()
		key, isString := token.(string)
		if err != nil || !isString || !allowed[key] {
			return nil, false
		}
		delete(allowed, key)
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		values[key] = value
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') || len(allowed) != 0 {
		return nil, false
	}
	_, err = d.Token()
	return values, err == io.EOF
}

func dreamSourceText(fields map[string]json.RawMessage, key string, limit int) string {
	var value string
	if json.Unmarshal(fields[key], &value) != nil || len(value) > limit || strings.TrimSpace(value) != value ||
		strings.ContainsRune(value, utf8.RuneError) || strings.ContainsFunc(value, unicode.IsControl) {
		return ""
	}
	return value
}

type dreamSourceExportTarget struct {
	project, memoryID, fileName, eventID, contentHash string
}

func parseDreamSourceExportTarget(raw []byte) (dreamSourceExportTarget, bool) {
	var target dreamSourceExportTarget
	body, ok := dreamSourceObject(raw, "project", "transfer_observation")
	if !ok {
		return target, false
	}
	observation, ok := dreamSourceObject(body["transfer_observation"], "memory_id", "event_id", "content_hash")
	if !ok {
		return target, false
	}
	target.project = dreamSourceText(body, "project", 120)
	target.memoryID = dreamSourceText(observation, "memory_id", 1024)
	target.eventID = dreamSourceText(observation, "event_id", 160)
	target.contentHash = dreamSourceText(observation, "content_hash", 64)
	project, err := sanitizeMemoryProject(target.project)
	if err != nil || project != target.project || target.eventID == "" || !isHexDigest(target.contentHash) {
		return target, false
	}
	file, ok := strings.CutPrefix(target.memoryID, project+"::")
	clean, err := sanitizeMemoryFile(file)
	if !ok || err != nil || clean != file {
		return target, false
	}
	target.fileName = clean
	return target, true
}

func (target dreamSourceExportTarget) grantMatches(raw []byte, now time.Time) bool {
	f, ok := dreamSourceObject(raw, "schema_id", "project", "memory_id", "event_id", "content_hash", "expires_at")
	if !ok || dreamSourceText(f, "schema_id", 64) != dreamSourceExportAuthoritySchema ||
		dreamSourceText(f, "project", 120) != target.project ||
		dreamSourceText(f, "memory_id", 1024) != target.memoryID ||
		dreamSourceText(f, "event_id", 160) != target.eventID ||
		dreamSourceText(f, "content_hash", 64) != target.contentHash {
		return false
	}
	expires, err := time.Parse(time.RFC3339Nano, dreamSourceText(f, "expires_at", 64))
	return err == nil && expires.After(now) && expires.Sub(now) <= dreamSourceMaxGrantTTL
}

type dreamSourceObservation struct {
	experienceRef, mechanism, observedResult string
}

func (target dreamSourceExportTarget) readObservation(ctx context.Context, store *memoryStore) (dreamSourceObservation, bool) {
	var observation dreamSourceObservation
	if store == nil || !store.isEnabled() || ctx.Err() != nil {
		return observation, false
	}
	relative := filepath.Join(target.project, filepath.FromSlash(target.fileName))
	root, err := filepath.EvalSymlinks(store.policy.rootPath)
	if err != nil {
		return observation, false
	}
	parent := filepath.Join(root, filepath.Dir(relative))
	if resolved, err := filepath.EvalSymlinks(parent); err != nil || resolved != parent {
		return observation, false
	}
	path := filepath.Join(root, relative)
	raw, info, err := readOwnerOnlyBoundedFileWithInfo(path, dreamSourceMaxBytes)
	if err != nil || sha256Hex(string(raw)) != target.contentHash {
		return observation, false
	}
	entry, proven := store.exactReadEntry(ctx, target.project, target.fileName, target.contentHash, info)
	if !proven || entry.EventID != target.eventID {
		return observation, false
	}
	f, ok := dreamSourceObject(raw, "schema_id", "experience_ref", "status", "mechanism", "observed_result")
	if !ok || dreamSourceText(f, "schema_id", 64) != dreamSourceObservationSchema || dreamSourceText(f, "status", 16) != "confirmed" {
		return observation, false
	}
	observation.experienceRef = dreamSourceText(f, "experience_ref", 160)
	observation.mechanism = dreamSourceText(f, "mechanism", 1000)
	observation.observedResult = dreamSourceText(f, "observed_result", 1000)
	return observation, observation.experienceRef != "" && observation.mechanism != "" && observation.observedResult != ""
}

func (target dreamSourceExportTarget) buildPassport(store *contextPassportStore, observation dreamSourceObservation, now time.Time) (contextPassport, string, bool) {
	var empty contextPassport
	if store == nil || store.identity == nil {
		return empty, "", false
	}
	store.mu.RLock()
	enabled := store.enabled
	store.mu.RUnlock()
	if !enabled {
		return empty, "", false
	}
	expires, err := parsePassportExpiry(nil, now)
	if err != nil {
		return empty, "", false
	}
	stats := &portableRedactionStats{}
	evidenceID := "dream_source_" + digestPrefix(target.memoryID+"\x00"+target.eventID+"\x00"+target.contentHash, 24)
	evidence := portableRows([]any{map[string]any{
		"evidence_id": evidenceID, "kind": "source_memory_revision", "memory_id": target.memoryID,
		"event_id": target.eventID, "content_hash": target.contentHash,
	}}, 1, stats, "evidence_id")
	claims := portableRows([]any{map[string]any{
		"experience_id": observation.experienceRef, "kind": "conditional_experience", "status": "confirmed",
		"mechanism": observation.mechanism, "observed_result": observation.observedResult, "evidence_refs": []any{evidenceID},
	}}, 1, stats, "experience_id")
	passport := contextPassport{
		SchemaID: contextPassportContractID, Version: 1, Revision: 1, LineageID: "lineage_" + randomHex(12),
		Project: target.project, CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: expires.Format(time.RFC3339Nano),
		Issuer: contextPassportIssuer{
			InstanceID: store.identity.InstanceID, SigningKeyID: store.identity.SigningKeyID,
			SigningPublicKey: store.identity.SigningPublicKey,
		},
		Scope:     map[string]any{"project": target.project, "data_classes": []any{"learning_memory"}},
		Objective: map[string]any{}, Claims: claims, Evidence: evidence,
		Lineage: map[string]any{"source_schema": dreamSourceObservationSchema},
		Replay:  map[string]any{}, Capabilities: []string{contextPassportContractID},
		Redactions: map[string]any{
			"applied": stats.Tokens+stats.Paths+stats.Clipped > 0, "token_values_redacted": stats.Tokens,
			"local_paths_redacted": stats.Paths, "strings_clipped": stats.Clipped,
		},
	}
	if signAndBoundContextPassport(&passport, store.identity, store.maxItemBytes) != nil ||
		len(verifyContextPassport(passport, now, true)) != 0 || len(passport.Claims) != 1 || len(passport.Evidence) != 1 {
		return empty, "", false
	}
	claim, claimHash, ok := dreamTransferConfirmedSourceClaim(passport, observation.experienceRef)
	provenance := passport.Evidence[0]
	if !ok || claim["experience_id"] != observation.experienceRef ||
		provenance["memory_id"] != target.memoryID || provenance["event_id"] != target.eventID ||
		provenance["content_hash"] != target.contentHash {
		return empty, "", false
	}
	return passport, claimHash, true
}

func (s *server) memoryContextPassportTransferSourceExport(w http.ResponseWriter, r *http.Request, raw []byte) {
	unavailable := func() {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "source_export_unavailable"})
	}
	keys := r.Header.Values("X-Api-Key")
	if len(keys) != 1 || !secureTokenEqual(keys[0], s.orchestratorAPIKey) {
		unavailable()
		return
	}
	target, ok := parseDreamSourceExportTarget(raw)
	if !ok {
		unavailable()
		return
	}
	unlock := s.lockOptionalFrontierT1ProjectBoundary()
	defer unlock()
	if !optionalDreamTransferLocalProjects(s, target.project, target.project) {
		unavailable()
		return
	}
	path := os.Getenv(dreamSourceExportFileEnv)
	grant, stamp, ok := readDreamOwnerFile(path)
	if !ok || !target.grantMatches(grant, time.Now().UTC()) {
		unavailable()
		return
	}
	observation, ok := target.readObservation(r.Context(), s.memoryStore)
	if !ok {
		unavailable()
		return
	}
	passport, hash, ok := target.buildPassport(s.contextPassports, observation, time.Now().UTC())
	if !ok {
		unavailable()
		return
	}
	live, liveStamp, ok := readDreamOwnerFile(path)
	if !ok || stamp != liveStamp || !bytes.Equal(grant, live) || !target.grantMatches(live, time.Now().UTC()) || r.Context().Err() != nil {
		unavailable()
		return
	}
	reconciliation, err := s.contextPassports.record(passport)
	if err != nil || !reconciliation.Recorded {
		unavailable()
		return
	}
	response := map[string]any{
		"ok": true, "schema_id": contextPassportContractID, "passport": passport,
		"recorded": true, "reconciliation": reconciliation,
		"portable": true, "replayable": false, "diffable": true, "private_key_exported": false,
		"transfer_source": map[string]any{
			"source_passport_id": passport.PassportID, "experience_ref": observation.experienceRef, "source_record_hash": hash,
		},
	}
	writeJSON(w, http.StatusOK, attachPayloadFormatContract(contextPassportContractID, response, "", "context_passport_export", r.URL.Path))
}
