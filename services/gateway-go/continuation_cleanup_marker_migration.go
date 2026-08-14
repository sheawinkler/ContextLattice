package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const (
	// The active pointer is the only mutable control record. Plans, receipts,
	// and rollback records are immutable content-addressed siblings, so one
	// queue can perform repeated monotonic extensions without overwriting old
	// custody evidence.
	continuationEvaluationCleanupMarkerMigrationActivePointerFile    = ".cap-migration-active.json"
	continuationEvaluationCleanupMarkerMigrationPlanPrefix           = ".cap-migration-plan-"
	continuationEvaluationCleanupMarkerMigrationReceiptPrefix        = ".cap-migration-receipt-"
	continuationEvaluationCleanupMarkerMigrationOwnerLockFile        = ".cap-migration-owner.lock"
	continuationEvaluationCleanupMarkerMigrationPlanFile             = ".cap-migration-plan.json"    // legacy read-only migration
	continuationEvaluationCleanupMarkerMigrationReceiptFile          = ".cap-migration-receipt.json" // legacy read-only migration
	continuationEvaluationCleanupMarkerMigrationSchemaID             = "contextlattice_evaluation_cleanup_marker_cap_migration.v1"
	continuationEvaluationCleanupMarkerMigrationVersion              = 1
	continuationEvaluationCleanupMarkerMigrationAction               = "extend_evaluation_cleanup_marker_index_caps"
	continuationEvaluationCleanupMarkerMigrationAuthority            = "gateway-go"
	continuationEvaluationCleanupMarkerMigrationNativeOwner          = "gateway-go"
	continuationEvaluationCleanupMarkerMigrationAuthorization        = "gateway-go-operator"
	continuationEvaluationCleanupMarkerMigrationStatePrepared        = "prepared"
	continuationEvaluationCleanupMarkerMigrationStateCommitted       = "committed"
	continuationEvaluationCleanupMarkerMigrationStateRolledBack      = "rolled_back"
	continuationEvaluationCleanupMarkerMigrationStatePendingRecovery = "pending_recovery"
	continuationEvaluationCleanupMarkerMigrationRollbackPrefix       = ".cap-migration-rollback-"
	// These are the absolute operational bounds for an explicitly authorized
	// extension. They are deliberately larger than the default cap but remain
	// finite, auditable, and fail-closed before the storage tier is exhausted.
	continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount = 1000000
	continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes = int64(512 * 1024 * 1024)
	evaluationCleanupMarkerMigrationCapabilityEnv            = "CONTEXTLATTICE_EVALUATION_CLEANUP_MIGRATION_CAPABILITY"
	evaluationCleanupMarkerMigrationCapabilityHeader         = "X-ContextLattice-Evaluation-Cleanup-Capability"
	evaluationCleanupMarkerMigrationPrincipalHeader          = "X-ContextLattice-Operator-Principal"
	evaluationCleanupMarkerMigrationWorkspaceHeader          = "X-ContextLattice-Workspace-ID"
)

// continuationEvaluationCleanupMarkerCapMigrationRequest is accepted only by
// the native gateway-go owner. The authenticated native route validates and
// converts its closed JSON contract into this typed request; no queue path is
// accepted from callers.
type continuationEvaluationCleanupMarkerCapMigrationRequest struct {
	NewMaxCount            int
	NewMaxBytes            int64
	OperatorRef            string
	Authorization          string
	NativeOwner            string
	Reason                 string
	ExpectedGeneration     int64
	AuthenticatedPrincipal string
	WorkspaceID            string
}

func stableContinuationDurableErrorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, os.ErrNotExist) {
		return "marker_migration_record_missing"
	}
	if errors.Is(err, errContinuationDurableFileOversized) {
		return "marker_migration_record_oversized"
	}
	var atomicErr *ownerOnlyAtomicWriteError
	if errors.As(err, &atomicErr) {
		operation := strings.ToLower(strings.TrimSpace(atomicErr.Operation))
		operation = strings.NewReplacer(" ", "_", "/", "_", "-", "_").Replace(operation)
		if operation != "" {
			return "marker_migration_atomic_" + operation
		}
	}
	return "marker_migration_rejected"
}

func continuationEvaluationCleanupMarkerMigrationPlanRecordFile(planDigest string) string {
	return continuationEvaluationCleanupMarkerMigrationPlanPrefix + migrationDigestFileComponent(planDigest) + ".json"
}

func continuationEvaluationCleanupMarkerMigrationReceiptRecordFile(planDigest string) string {
	return continuationEvaluationCleanupMarkerMigrationReceiptPrefix + migrationDigestFileComponent(planDigest) + ".json"
}

func migrationDigestFileComponent(digest string) string {
	digest = strings.TrimSpace(strings.TrimPrefix(digest, "sha256:"))
	if len(digest) > 64 {
		digest = digest[:64]
	}
	if digest == "" {
		return "missing"
	}
	return digest
}

func continuationEvaluationCleanupMarkerMigrationActivePointerDigest(record map[string]any) string {
	return continuationEvaluationCleanupMarkerMigrationDigest(record, "digest")
}

const continuationEvaluationCleanupMarkerMigrationMaxEpoch = int64(^uint64(0) >> 1)

func nextEvaluationCleanupMarkerMigrationEpoch(current int64) (int64, error) {
	if current < 0 || current >= continuationEvaluationCleanupMarkerMigrationMaxEpoch {
		return 0, errors.New("marker migration epoch exhausted")
	}
	return current + 1, nil
}

func continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(pointer map[string]any) (int64, bool) {
	if pointer == nil {
		return 0, false
	}
	if raw, present := pointer["target_generation"]; present {
		return continuationEvaluationCleanupStrictInt64(raw)
	}
	// Pointers written before the epoch/target split selected the same
	// generation that they exposed, except for a rolled-back pointer. The
	// latter is resolved from its immutable rollback record during reconciliation
	// and is deliberately not guessed here.
	if anyToString(pointer["state"]) != continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		return continuationEvaluationCleanupStrictInt64(pointer["generation"])
	}
	return 0, false
}

func continuationEvaluationCleanupMarkerMigrationPointerTargetGenerationValue(pointer map[string]any) int64 {
	target, ok := continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(pointer)
	if !ok {
		return 0
	}
	return target
}

func continuationEvaluationCleanupMarkerMigrationPlanGeneration(plan map[string]any, field string) (int64, bool) {
	if plan == nil {
		return 0, false
	}
	return continuationEvaluationCleanupStrictInt64(plan[field])
}

func continuationEvaluationCleanupMarkerMigrationPlanPreviousTargetGeneration(plan map[string]any) (int64, bool) {
	if plan == nil {
		return 0, false
	}
	if raw, present := plan["previous_target_generation"]; present {
		return continuationEvaluationCleanupStrictInt64(raw)
	}
	// Plans written before the epoch/target split used previous_generation for
	// both values. This fallback is safe because it is read-only compatibility;
	// every new plan writes the separated target field.
	return continuationEvaluationCleanupStrictInt64(plan["previous_generation"])
}

func continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(value string) bool {
	value = strings.TrimSpace(value)
	return continuationEvaluationCleanupMarkerMigrationOpaqueFieldValid(value, 128)
}

// Migration custody is a public receipt boundary.  These fields are opaque
// operator identifiers, not free-form paths, credentials, or diagnostics.
// Keep the grammar deliberately small so a durable/public record cannot carry
// a private path, control byte, or configured capability back to a caller.
func continuationEvaluationCleanupMarkerMigrationOpaqueFieldValid(value string, maxBytes int) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxBytes || strings.Contains(value, "..") || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) && r != ' ' {
			return false
		}
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:-", r)) {
			return false
		}
	}
	return true
}

func continuationEvaluationCleanupMarkerMigrationReasonValid(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 256 || strings.Contains(value, "..") || strings.ContainsAny(value, "/\\") {
		return false
	}
	configured := strings.TrimSpace(os.Getenv(evaluationCleanupMarkerMigrationCapabilityEnv))
	if configured != "" && strings.Contains(value, configured) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
		// Reason evidence is intentionally a small public-safe grammar rather
		// than an arbitrary diagnostic string.  This prevents a caller from
		// smuggling shell paths, markup, or private provider text into a durable
		// receipt even when it avoids the keyword checks below.
		if r > unicode.MaxASCII || !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(" ._,:;+-()", r)) {
			return false
		}
	}
	lower := strings.ToLower(value)
	for _, forbidden := range []string{"authorization", "bearer", "password", "secret", "token", "private key", "capability"} {
		if strings.Contains(lower, forbidden) {
			return false
		}
	}
	return true
}

func continuationEvaluationCleanupMarkerMigrationCapabilityFromRequest(r *http.Request) (string, string, bool) {
	configured := strings.TrimSpace(os.Getenv(evaluationCleanupMarkerMigrationCapabilityEnv))
	provided := ""
	principal := ""
	workspace := ""
	if r != nil {
		provided = strings.TrimSpace(r.Header.Get(evaluationCleanupMarkerMigrationCapabilityHeader))
		principal = strings.TrimSpace(r.Header.Get(evaluationCleanupMarkerMigrationPrincipalHeader))
		workspace = strings.TrimSpace(r.Header.Get(evaluationCleanupMarkerMigrationWorkspaceHeader))
	}
	if configured == "" || !secureTokenEqual(provided, configured) || !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(principal) || !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(workspace) {
		return "", "", false
	}
	return principal, workspace, true
}

// opsEvaluationCleanupMarkerCapMigration is the authenticated native control
// surface for cap extension/recovery. The durable plan and receipt never
// expose the queue path; authorization is bound to the mandatory
// constant-time capability plus the authenticated principal/workspace. The
// owner/authorization fields in the closed JSON body are schema labels, not
// caller-forgeable authority.
func (s *server) opsEvaluationCleanupMarkerCapMigration(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	principal, workspace, authorized := continuationEvaluationCleanupMarkerMigrationCapabilityFromRequest(r)
	if !authorized {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "operator capability, principal, and workspace authorization required"})
		return
	}
	if s == nil || s.continuationDurable == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "continuation durable queue is unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var payload struct {
		Operation          string `json:"operation"`
		NewMaxCount        int    `json:"new_max_marker_count,omitempty"`
		NewMaxBytes        int64  `json:"new_max_marker_bytes,omitempty"`
		OperatorRef        string `json:"operator_ref"`
		Authorization      string `json:"authorization"`
		NativeOwner        string `json:"native_owner"`
		Reason             string `json:"reason"`
		PlanDigest         string `json:"plan_digest,omitempty"`
		ExpectedGeneration *int64 `json:"expected_generation"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid migration request"})
		return
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid migration request"})
		return
	}
	if payload.Authorization != continuationEvaluationCleanupMarkerMigrationAuthorization || payload.NativeOwner != continuationEvaluationCleanupMarkerMigrationNativeOwner {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "native owner authorization is invalid"})
		return
	}
	if strings.TrimSpace(payload.Operation) == "extend" && payload.ExpectedGeneration == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "expected_generation is required for compare-and-swap"})
		return
	}
	switch strings.TrimSpace(payload.Operation) {
	case "extend":
		receipt, err := s.continuationDurable.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
			NewMaxCount: payload.NewMaxCount, NewMaxBytes: payload.NewMaxBytes, OperatorRef: payload.OperatorRef,
			Authorization: payload.Authorization, NativeOwner: payload.NativeOwner, Reason: payload.Reason,
			ExpectedGeneration: *payload.ExpectedGeneration, AuthenticatedPrincipal: principal, WorkspaceID: workspace,
		})
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "marker cap migration was not committed", "detail": stableContinuationDurableErrorCode(err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "operation": "extend", "receipt": receipt})
	case "rollback":
		if err := s.continuationDurable.rollbackEvaluationCleanupMarkerIndexCaps(payload.OperatorRef, payload.PlanDigest, principal, workspace); err != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": "marker cap migration rollback was not committed", "detail": stableContinuationDurableErrorCode(err)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "operation": "rollback", "plan_digest": strings.TrimSpace(payload.PlanDigest)})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "operation must be extend or rollback"})
	}
}

type continuationEvaluationCleanupMarkerInventory struct {
	Count  int
	Bytes  int64
	Digest string
	Items  []any
}

func continuationEvaluationCleanupMarkerInventoryDigest(items []any, count int, bytes int64) string {
	return continuationEvaluationCleanupDigest(map[string]any{
		"marker_count": count,
		"marker_bytes": bytes,
		"items":        items,
	})
}

func continuationEvaluationCleanupMarkerMigrationDigest(record map[string]any, field string) string {
	copyRecord := cloneAnyMap(record)
	delete(copyRecord, field)
	return continuationEvaluationCleanupDigest(copyRecord)
}

func continuationEvaluationCleanupMarkerRawDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (q *continuationDurableQueue) inventoryEvaluationCleanupMarkersLocked() (continuationEvaluationCleanupMarkerInventory, error) {
	var inventory continuationEvaluationCleanupMarkerInventory
	entries, overflow, err := readEvaluationCleanupMarkerTreeBounded(q.dir, continuationEvaluationCleanupIndexDirectory, 256+1, q.evaluationCleanupMarkerCountLimitLocked()+1, continuationEvaluationCleanupMarkerMaxBytes, nil, nil)
	if errors.Is(err, os.ErrNotExist) {
		inventory.Digest = continuationEvaluationCleanupMarkerInventoryDigest([]any{}, 0, 0)
		return inventory, nil
	}
	if err != nil {
		return inventory, err
	}
	if overflow {
		return inventory, errors.New("evaluation cleanup marker migration inventory exceeds shard bound")
	}
	items := make([]any, 0)
	seen := map[string]struct{}{}
	for _, shardEntry := range entries {
		if evaluationCleanupMarkerIndexControlEntry(shardEntry.name) || evaluationCleanupMarkerTemporaryEntry(shardEntry.name) {
			continue
		}
		if len(shardEntry.name) != 2 || strings.Trim(shardEntry.name, "0123456789abcdefABCDEF") != "" || !shardEntry.isDir {
			return inventory, fmt.Errorf("evaluation cleanup marker migration found invalid shard %q", shardEntry.name)
		}
		if shardEntry.overflow {
			return inventory, errors.New("evaluation cleanup marker migration inventory exceeds marker cap")
		}
		for _, markerEntry := range shardEntry.entries {
			if evaluationCleanupMarkerTemporaryEntry(markerEntry.name) {
				continue
			}
			if markerEntry.isDir || markerEntry.name == "" {
				return inventory, fmt.Errorf("evaluation cleanup marker migration found invalid marker %q", markerEntry.name)
			}
			raw := markerEntry.raw
			marker, decodeErr := continuationEvaluationCleanupDecodeMarker(raw)
			if decodeErr != nil {
				return inventory, decodeErr
			}
			ref, refOK := continuationEvaluationCleanupStrictString(marker["job_ref"])
			if !refOK {
				return inventory, errors.New("evaluation cleanup marker migration found an unbound marker")
			}
			ref = strings.ToLower(strings.TrimSpace(ref))
			expectedIndex, expectedShard, expectedName, componentsOK := continuationEvaluationCleanupMarkerComponents(ref)
			if !componentsOK || expectedIndex != continuationEvaluationCleanupIndexDirectory || expectedShard != shardEntry.name || expectedName != markerEntry.name {
				return inventory, errors.New("evaluation cleanup marker migration identity/path mismatch")
			}
			if _, validateErr := continuationEvaluationCleanupValidateMarker(marker, ref, "", ""); validateErr != nil {
				return inventory, validateErr
			}
			if _, exists := seen[ref]; exists {
				return inventory, errors.New("evaluation cleanup marker migration found duplicate marker identity")
			}
			seen[ref] = struct{}{}
			inventory.Count++
			inventory.Bytes += int64(len(raw))
			items = append(items, map[string]any{
				"ref":    ref,
				"bytes":  len(raw),
				"digest": continuationEvaluationCleanupMarkerRawDigest(raw),
			})
			if inventory.Count > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || inventory.Bytes > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes {
				return inventory, errors.New("evaluation cleanup marker migration inventory exceeds absolute bound")
			}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		left, _ := items[i].(map[string]any)
		right, _ := items[j].(map[string]any)
		return anyToString(left["ref"]) < anyToString(right["ref"])
	})
	if len(items) == 0 {
		items = []any{}
	}
	inventory.Items = items
	inventory.Digest = continuationEvaluationCleanupMarkerInventoryDigest(items, inventory.Count, inventory.Bytes)
	return inventory, nil
}

func (q *continuationDurableQueue) readMarkerMigrationRecordLocked(name string) (map[string]any, error) {
	raw, err := readEvaluationCleanupMarkerFileBounded(q.dir, continuationEvaluationCleanupIndexDirectory, "", name, continuationEvaluationCleanupMarkerMaxBytes)
	if err != nil {
		return nil, err
	}
	value := map[string]any{}
	if err := decodeStrictJSONMap(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func continuationEvaluationCleanupMarkerMigrationRollbackFile(planDigest string) string {
	return continuationEvaluationCleanupMarkerMigrationRollbackPrefix + migrationDigestFileComponent(planDigest) + ".json"
}

func evaluationCleanupMarkerIndexControlEntry(name string) bool {
	name = strings.TrimSpace(name)
	return name == continuationEvaluationCleanupMarkerIndexFile ||
		name == continuationEvaluationCleanupMarkerMigrationActivePointerFile ||
		name == continuationEvaluationCleanupMarkerMigrationPlanFile ||
		name == continuationEvaluationCleanupMarkerMigrationReceiptFile ||
		strings.HasPrefix(name, continuationEvaluationCleanupMarkerMigrationPlanPrefix) ||
		strings.HasPrefix(name, continuationEvaluationCleanupMarkerMigrationReceiptPrefix) ||
		strings.HasPrefix(name, continuationEvaluationCleanupMarkerMigrationRollbackPrefix)
}

func evaluationCleanupMarkerTemporaryEntry(name string) bool {
	name = strings.TrimSpace(name)
	return strings.HasPrefix(name, ".") && strings.Contains(name, ".tmp-")
}

func decodeStrictJSONMap(raw []byte, target *map[string]any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("control record has trailing values")
		}
		return err
	}
	return nil
}

func (q *continuationDurableQueue) writeImmutableMarkerMigrationRecordLocked(name string, record map[string]any) error {
	if _, err := q.readMarkerMigrationRecordLocked(name); err == nil {
		return fmt.Errorf("marker migration record %s already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := q.ensureEvaluationCleanupMarkerDirectoriesLocked(filepath.Join(q.dir, continuationEvaluationCleanupIndexDirectory)); err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	writer := q.evaluationCleanupMarkerWriter
	if writer == nil {
		writer = writeEvaluationCleanupMarkerDurable
	}
	return writer(q.dir, continuationEvaluationCleanupIndexDirectory, "", name, append(raw, '\n'))
}

func (q *continuationDurableQueue) writeActiveMarkerMigrationPointerLocked(pointer map[string]any) error {
	if q == nil {
		return errors.New("nil continuation durable queue")
	}
	pointer = cloneAnyMap(pointer)
	delete(pointer, "digest")
	pointer["schema_id"] = continuationEvaluationCleanupMarkerMigrationSchemaID
	pointer["version"] = continuationEvaluationCleanupMarkerMigrationVersion
	pointer["authority"] = continuationEvaluationCleanupMarkerMigrationAuthority
	pointer["action"] = continuationEvaluationCleanupMarkerMigrationAction
	pointer["updated_at"] = nowUTCISO()
	pointer["digest"] = continuationEvaluationCleanupMarkerMigrationActivePointerDigest(pointer)
	raw, err := json.Marshal(pointer)
	if err != nil {
		return err
	}
	if int64(len(raw)) > continuationEvaluationCleanupMarkerMaxBytes {
		return errContinuationDurableFileOversized
	}
	if err := q.ensureEvaluationCleanupMarkerDirectoriesLocked(filepath.Join(q.dir, continuationEvaluationCleanupIndexDirectory)); err != nil {
		return err
	}
	writer := q.evaluationCleanupMarkerWriter
	if writer == nil {
		writer = writeEvaluationCleanupMarkerDurable
	}
	return writer(q.dir, continuationEvaluationCleanupIndexDirectory, "", continuationEvaluationCleanupMarkerMigrationActivePointerFile, append(raw, '\n'))
}

func (q *continuationDurableQueue) readActiveMarkerMigrationPointerLocked() (map[string]any, error) {
	pointer, err := q.readMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationActivePointerFile)
	if err != nil {
		return nil, err
	}
	digest, ok := pointer["digest"].(string)
	if !ok || digest == "" || continuationEvaluationCleanupMarkerMigrationActivePointerDigest(pointer) != digest {
		return nil, errors.New("marker migration active pointer digest is invalid")
	}
	if anyToString(pointer["schema_id"]) != continuationEvaluationCleanupMarkerMigrationSchemaID || anyToInt(pointer["version"], 0) != continuationEvaluationCleanupMarkerMigrationVersion || anyToString(pointer["authority"]) != continuationEvaluationCleanupMarkerMigrationAuthority || anyToString(pointer["action"]) != continuationEvaluationCleanupMarkerMigrationAction {
		return nil, errors.New("marker migration active pointer schema is invalid")
	}
	if !continuationEvaluationCleanupMarkerMigrationPointerShapeValid(pointer) {
		return nil, errors.New("marker migration active pointer shape is invalid")
	}
	return pointer, nil
}

// writeAndConfirmMarkerMigrationPointerLocked treats an atomic-write error as
// ambiguous until the descriptor-bound read observes the exact intended
// pointer. This is the publication boundary for cap/state changes: callers
// must not expose a staged migration or rollback merely because a rename or
// directory flush returned an error after possibly committing.
func (q *continuationDurableQueue) writeAndConfirmMarkerMigrationPointerLocked(pointer map[string]any) error {
	if err := q.writeActiveMarkerMigrationPointerLocked(pointer); err == nil {
		return nil
	} else {
		if readback, readErr := q.readActiveMarkerMigrationPointerLocked(); readErr == nil && continuationEvaluationCleanupMarkerMigrationPointerMatches(readback, pointer) {
			return nil
		} else {
			return err
		}
	}
}

func continuationEvaluationCleanupMarkerMigrationPointerMatches(actual, expected map[string]any) bool {
	if actual == nil || expected == nil {
		return false
	}
	if !continuationEvaluationCleanupMarkerMigrationPointerShapeValid(actual) || !continuationEvaluationCleanupMarkerMigrationPointerShapeValid(expected) {
		return false
	}
	state := anyToString(expected["state"])
	fields := []string{"state", "plan_digest", "receipt_digest", "authenticated_principal", "workspace_id", "generation", "target_generation", "max_marker_count", "max_marker_bytes"}
	if state == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		fields = append(fields, "rollback_plan_digest", "rollback_digest")
	}
	for _, field := range fields {
		switch field {
		case "generation", "target_generation":
			actualValue, actualOK := continuationEvaluationCleanupStrictInt64(actual[field])
			expectedValue, expectedOK := continuationEvaluationCleanupStrictInt64(expected[field])
			if !actualOK || !expectedOK || actualValue != expectedValue {
				return false
			}
		case "max_marker_count", "max_marker_bytes":
			if anyToInt(actual[field], -1) != anyToInt(expected[field], -2) {
				return false
			}
		default:
			if anyToString(actual[field]) != anyToString(expected[field]) {
				return false
			}
		}
	}
	return true
}

func continuationEvaluationCleanupMarkerMigrationPointerShapeValid(pointer map[string]any) bool {
	if pointer == nil {
		return false
	}
	state := anyToString(pointer["state"])
	if state != continuationEvaluationCleanupMarkerMigrationStatePrepared && state != continuationEvaluationCleanupMarkerMigrationStateCommitted && state != continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		return false
	}
	common := []string{"state", "generation", "plan_digest", "receipt_digest", "max_marker_count", "max_marker_bytes", "authenticated_principal", "workspace_id"}
	for _, field := range common {
		if _, ok := pointer[field]; !ok {
			return false
		}
	}
	if state == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		if strings.TrimSpace(anyToString(pointer["rollback_plan_digest"])) == "" || strings.TrimSpace(anyToString(pointer["rollback_digest"])) == "" {
			return false
		}
	} else if _, ok := pointer["rollback_plan_digest"]; ok {
		return false
	} else if _, ok := pointer["rollback_digest"]; ok {
		return false
	}
	allowed := map[string]struct{}{
		"schema_id": {}, "version": {}, "authority": {}, "action": {}, "updated_at": {}, "digest": {},
		"target_generation": {},
	}
	for _, field := range common {
		allowed[field] = struct{}{}
	}
	if state == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		allowed["rollback_plan_digest"] = struct{}{}
		allowed["rollback_digest"] = struct{}{}
	}
	for field := range pointer {
		if _, ok := allowed[field]; !ok {
			return false
		}
	}
	generationValue, generationOK := continuationEvaluationCleanupStrictInt64(pointer["generation"])
	maxCount, countOK := continuationEvaluationCleanupStrictInt(pointer["max_marker_count"])
	maxBytes, bytesOK := continuationEvaluationCleanupStrictInt(pointer["max_marker_bytes"])
	if !generationOK || !countOK || !bytesOK || generationValue < 0 || maxCount <= 0 || maxCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || maxBytes <= 0 || int64(maxBytes) > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes {
		return false
	}
	if target, targetOK := continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(pointer); targetOK {
		if target < 0 || target > generationValue || (state != continuationEvaluationCleanupMarkerMigrationStateRolledBack && target != generationValue) {
			return false
		}
	}
	if !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(anyToString(pointer["authenticated_principal"])) || !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(anyToString(pointer["workspace_id"])) {
		return false
	}
	planDigest := strings.TrimSpace(anyToString(pointer["plan_digest"]))
	// Generation zero is the pre-migration baseline and therefore has no
	// content-addressed plan or receipt to bind.  Every later generation must
	// carry a real digest; allowing an empty digest there would make rollback
	// pointers ambiguous and permit stale-generation ABA.
	if planDigest == "" {
		if state != continuationEvaluationCleanupMarkerMigrationStateRolledBack {
			return false
		}
	} else if !utilitySHA256DigestValid(planDigest) {
		return false
	}
	receiptDigest := strings.TrimSpace(anyToString(pointer["receipt_digest"]))
	if receiptDigest != "" && !utilitySHA256DigestValid(receiptDigest) {
		return false
	}
	if state == continuationEvaluationCleanupMarkerMigrationStateCommitted && receiptDigest == "" {
		return false
	}
	if state == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		if target, targetOK := continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(pointer); targetOK && target == 0 && receiptDigest != "" {
			return false
		}
	}
	if state == continuationEvaluationCleanupMarkerMigrationStateRolledBack && (!utilitySHA256DigestValid(anyToString(pointer["rollback_plan_digest"])) || !utilitySHA256DigestValid(anyToString(pointer["rollback_digest"]))) {
		return false
	}
	return true
}

func (q *continuationDurableQueue) migrationPlanAndReceiptLocked(planDigest string) (map[string]any, map[string]any, error) {
	planDigest = strings.TrimSpace(planDigest)
	if planDigest == "" {
		return nil, nil, errors.New("marker migration plan digest is missing")
	}
	plan, err := q.readMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationPlanRecordFile(planDigest))
	if err != nil {
		return nil, nil, err
	}
	if anyToString(plan["plan_digest"]) != planDigest || continuationEvaluationCleanupMarkerMigrationDigest(plan, "plan_digest") != planDigest {
		return nil, nil, errors.New("marker migration plan binding or digest is invalid")
	}
	receipt, receiptErr := q.readMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationReceiptRecordFile(planDigest))
	if errors.Is(receiptErr, os.ErrNotExist) {
		return plan, nil, nil
	}
	if receiptErr != nil {
		return nil, nil, receiptErr
	}
	receiptDigest := anyToString(receipt["receipt_digest"])
	if anyToString(receipt["plan_digest"]) != planDigest || receiptDigest == "" || continuationEvaluationCleanupMarkerMigrationDigest(receipt, "receipt_digest") != receiptDigest {
		return nil, nil, errors.New("marker migration receipt binding or digest is invalid")
	}
	return plan, receipt, nil
}

func (q *continuationDurableQueue) migrationRequestValid(request continuationEvaluationCleanupMarkerCapMigrationRequest) error {
	if request.NewMaxCount <= q.evaluationCleanupMarkerCountLimitLocked() || request.NewMaxCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount {
		return errors.New("marker migration count extension is outside the explicit bound")
	}
	if request.NewMaxBytes <= q.evaluationCleanupMarkerByteLimitLocked() || request.NewMaxBytes > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes {
		return errors.New("marker migration byte extension is outside the explicit bound")
	}
	if !continuationEvaluationCleanupMarkerMigrationOpaqueFieldValid(request.OperatorRef, 128) || !continuationEvaluationCleanupMarkerMigrationReasonValid(request.Reason) {
		return errors.New("marker migration operator reference and reason are required")
	}
	if request.Authorization != continuationEvaluationCleanupMarkerMigrationAuthorization || request.NativeOwner != continuationEvaluationCleanupMarkerMigrationNativeOwner {
		return errors.New("marker migration authorization is not native-owner authorized")
	}
	if !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(request.AuthenticatedPrincipal) || !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(request.WorkspaceID) {
		return errors.New("marker migration authenticated principal and workspace are required")
	}
	if request.ExpectedGeneration < 0 {
		return errors.New("marker migration generation precondition is invalid")
	}
	return nil
}

func (q *continuationDurableQueue) migrateEvaluationCleanupMarkerIndexCaps(request continuationEvaluationCleanupMarkerCapMigrationRequest) (map[string]any, error) {
	if q == nil {
		return nil, errors.New("nil continuation durable queue")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	unlockOwner, lockErr := acquireEvaluationCleanupMarkerMigrationOwnerLock(q.dir)
	if lockErr != nil {
		return nil, lockErr
	}
	defer unlockOwner()
	// A second queue handle can have a different in-memory epoch. Reconcile
	// from the durable pointer while holding the owner lock before validating
	// the caller's compare-and-swap token.
	if err := q.reconcileEvaluationCleanupMarkerMigrationLocked(); err != nil {
		return nil, err
	}
	if err := q.migrationRequestValid(request); err != nil {
		return nil, err
	}
	if q.evaluationCleanupMarkerState != continuationEvaluationCleanupMarkerIndexStateReady {
		return nil, errors.New("marker migration requires a ready exact-once index")
	}
	if q.evaluationCleanupMarkerMigrationState == continuationEvaluationCleanupMarkerMigrationStatePendingRecovery {
		return nil, errors.New("marker migration has unresolved recovery custody")
	}
	if request.ExpectedGeneration != q.evaluationCleanupMarkerMigrationGeneration {
		return nil, errors.New("marker migration generation compare-and-swap failed")
	}
	newEpoch, epochErr := nextEvaluationCleanupMarkerMigrationEpoch(q.evaluationCleanupMarkerMigrationGeneration)
	if epochErr != nil {
		return nil, epochErr
	}
	previousTargetGeneration := q.evaluationCleanupMarkerMigrationTargetGeneration
	inventory, err := q.inventoryEvaluationCleanupMarkersLocked()
	if err != nil {
		return nil, err
	}
	plan := map[string]any{
		"schema_id":                  continuationEvaluationCleanupMarkerMigrationSchemaID,
		"version":                    continuationEvaluationCleanupMarkerMigrationVersion,
		"authority":                  continuationEvaluationCleanupMarkerMigrationAuthority,
		"action":                     continuationEvaluationCleanupMarkerMigrationAction,
		"state":                      continuationEvaluationCleanupMarkerMigrationStatePrepared,
		"native_owner":               continuationEvaluationCleanupMarkerMigrationNativeOwner,
		"authorization":              continuationEvaluationCleanupMarkerMigrationAuthorization,
		"operator_ref":               strings.TrimSpace(request.OperatorRef),
		"reason":                     strings.TrimSpace(request.Reason),
		"old_max_marker_count":       q.evaluationCleanupMarkerCountLimitLocked(),
		"old_max_marker_bytes":       q.evaluationCleanupMarkerByteLimitLocked(),
		"new_max_marker_count":       request.NewMaxCount,
		"new_max_marker_bytes":       request.NewMaxBytes,
		"inventory_count":            inventory.Count,
		"inventory_bytes":            inventory.Bytes,
		"inventory_digest":           inventory.Digest,
		"generation":                 newEpoch,
		"previous_generation":        q.evaluationCleanupMarkerMigrationGeneration,
		"previous_target_generation": previousTargetGeneration,
		"previous_plan_digest":       q.evaluationCleanupMarkerMigrationPlanDigest,
		"previous_receipt_digest":    q.evaluationCleanupMarkerMigrationReceiptDigest,
		"authenticated_principal":    strings.TrimSpace(request.AuthenticatedPrincipal),
		"workspace_id":               strings.TrimSpace(request.WorkspaceID),
		"created_at":                 nowUTCISO(),
	}
	plan["plan_digest"] = continuationEvaluationCleanupMarkerMigrationDigest(plan, "plan_digest")
	planDigest := anyToString(plan["plan_digest"])
	if err := q.writeImmutableMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationPlanRecordFile(planDigest), plan); err != nil {
		return nil, err
	}
	preparedPointer := map[string]any{
		"state":      continuationEvaluationCleanupMarkerMigrationStatePrepared,
		"generation": plan["generation"], "plan_digest": planDigest, "receipt_digest": "",
		"target_generation": plan["generation"],
		"max_marker_count":  plan["old_max_marker_count"], "max_marker_bytes": plan["old_max_marker_bytes"],
		"authenticated_principal": plan["authenticated_principal"], "workspace_id": plan["workspace_id"],
	}
	if err := q.writeActiveMarkerMigrationPointerLocked(preparedPointer); err != nil {
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		return nil, err
	}
	postInventory, postInventoryErr := q.inventoryEvaluationCleanupMarkersLocked()
	if postInventoryErr != nil {
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		return nil, postInventoryErr
	}
	if postInventory.Count != inventory.Count || postInventory.Bytes != inventory.Bytes || postInventory.Digest != inventory.Digest {
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		return nil, errors.New("marker migration inventory changed while preparing plan")
	}
	// The receipt is a second immutable record. If process death happens before
	// it is durable, the prepared plan remains for explicit rollback; nothing is
	// silently deleted or treated as an extension.
	receipt := map[string]any{
		"schema_id":               continuationEvaluationCleanupMarkerMigrationSchemaID,
		"version":                 continuationEvaluationCleanupMarkerMigrationVersion,
		"authority":               continuationEvaluationCleanupMarkerMigrationAuthority,
		"action":                  continuationEvaluationCleanupMarkerMigrationAction,
		"state":                   continuationEvaluationCleanupMarkerMigrationStateCommitted,
		"native_owner":            continuationEvaluationCleanupMarkerMigrationNativeOwner,
		"authorization":           continuationEvaluationCleanupMarkerMigrationAuthorization,
		"operator_ref":            strings.TrimSpace(request.OperatorRef),
		"reason":                  strings.TrimSpace(request.Reason),
		"plan_digest":             plan["plan_digest"],
		"old_max_marker_count":    plan["old_max_marker_count"],
		"old_max_marker_bytes":    plan["old_max_marker_bytes"],
		"new_max_marker_count":    plan["new_max_marker_count"],
		"new_max_marker_bytes":    plan["new_max_marker_bytes"],
		"inventory_count":         inventory.Count,
		"inventory_bytes":         inventory.Bytes,
		"inventory_digest":        inventory.Digest,
		"generation":              plan["generation"],
		"authenticated_principal": plan["authenticated_principal"],
		"workspace_id":            plan["workspace_id"],
		"committed_at":            nowUTCISO(),
	}
	receipt["receipt_digest"] = continuationEvaluationCleanupMarkerMigrationDigest(receipt, "receipt_digest")
	if err := q.writeImmutableMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationReceiptRecordFile(planDigest), receipt); err != nil {
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		q.lastError = err.Error()
		return nil, err
	}
	newGeneration := newEpoch
	receiptDigest := anyToString(receipt["receipt_digest"])
	committedPointer := map[string]any{
		"state":      continuationEvaluationCleanupMarkerMigrationStateCommitted,
		"generation": newGeneration, "plan_digest": planDigest, "receipt_digest": receiptDigest,
		"target_generation": newGeneration,
		"max_marker_count":  request.NewMaxCount, "max_marker_bytes": request.NewMaxBytes,
		"authenticated_principal": request.AuthenticatedPrincipal, "workspace_id": request.WorkspaceID,
	}
	if err := q.writeAndConfirmMarkerMigrationPointerLocked(committedPointer); err != nil {
		// The prepared pointer and immutable receipt remain durable custody, but
		// the new caps are not published in this process until the committed
		// pointer is observed. A restart can reconcile the same evidence.
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		q.evaluationCleanupMarkerMigrationReceiptDigest = ""
		q.lastError = err.Error()
		return receipt, err
	}
	q.evaluationCleanupMarkerMaxCount = request.NewMaxCount
	q.evaluationCleanupMarkerMaxBytes = request.NewMaxBytes
	q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStateCommitted
	q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
	q.evaluationCleanupMarkerMigrationReceiptDigest = receiptDigest
	q.evaluationCleanupMarkerMigrationGeneration = newGeneration
	q.evaluationCleanupMarkerMigrationTargetGeneration = newGeneration
	if err := q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, q.evaluationCleanupMarkerPendingRef, q.evaluationCleanupMarkerPendingBytes); err != nil {
		q.lastError = err.Error()
		return receipt, err
	}
	return receipt, nil
}

// continuationEvaluationCleanupMarkerMigrationRollbackRecordValid validates
// the immutable rollback record at the same custody boundary used during
// restart reconciliation.  In particular, an existing record is never an
// idempotency shortcut: its digest, target generation/receipts, operator
// binding, and old-cap shape must all still agree with the active plan.
func continuationEvaluationCleanupMarkerMigrationRollbackRecordValid(record map[string]any, planDigest string, plan, pointer map[string]any, oldCount int, oldBytes int64, operatorRef, principal, workspace string) error {
	if record == nil || anyToString(record["schema_id"]) != continuationEvaluationCleanupMarkerMigrationSchemaID || anyToInt(record["version"], 0) != continuationEvaluationCleanupMarkerMigrationVersion || anyToString(record["authority"]) != continuationEvaluationCleanupMarkerMigrationAuthority || anyToString(record["action"]) != continuationEvaluationCleanupMarkerMigrationAction || anyToString(record["state"]) != continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		return errors.New("marker migration rollback receipt schema or state is invalid")
	}
	if anyToString(record["native_owner"]) != continuationEvaluationCleanupMarkerMigrationNativeOwner || anyToString(record["authorization"]) != continuationEvaluationCleanupMarkerMigrationAuthorization || anyToString(record["plan_digest"]) != strings.TrimSpace(planDigest) || anyToString(record["authenticated_principal"]) != strings.TrimSpace(principal) || anyToString(record["workspace_id"]) != strings.TrimSpace(workspace) || anyToString(record["operator_ref"]) != strings.TrimSpace(operatorRef) {
		return errors.New("marker migration rollback receipt binding is invalid")
	}
	rollbackDigest := anyToString(record["rollback_digest"])
	if rollbackDigest == "" || continuationEvaluationCleanupMarkerMigrationDigest(record, "rollback_digest") != rollbackDigest {
		return errors.New("marker migration rollback receipt digest is invalid")
	}
	if anyToString(pointer["state"]) == continuationEvaluationCleanupMarkerMigrationStateRolledBack && anyToString(pointer["rollback_digest"]) != rollbackDigest {
		return errors.New("marker migration rollback pointer digest does not bind the rollback receipt")
	}
	if !continuationEvaluationCleanupMarkerMigrationOpaqueFieldValid(anyToString(record["operator_ref"]), 128) || !continuationEvaluationCleanupMarkerMigrationReasonValid(anyToString(record["reason"])) && anyToString(record["reason"]) != "" {
		return errors.New("marker migration rollback receipt public fields are invalid")
	}
	if plan == nil || pointer == nil {
		return errors.New("marker migration rollback custody is incomplete")
	}
	targetGeneration, targetGenerationOK := continuationEvaluationCleanupStrictInt64(record["target_generation"])
	previousTargetGeneration, previousTargetGenerationOK := continuationEvaluationCleanupMarkerMigrationPlanPreviousTargetGeneration(plan)
	if !targetGenerationOK || !previousTargetGenerationOK || targetGeneration != previousTargetGeneration || anyToString(record["target_plan_digest"]) != anyToString(plan["previous_plan_digest"]) || anyToString(record["target_receipt_digest"]) != anyToString(plan["previous_receipt_digest"]) {
		return errors.New("marker migration rollback target binding is invalid")
	}
	if rawRollbackGeneration, present := record["rollback_generation"]; present && anyToString(pointer["state"]) == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		recordedRollbackGeneration, rollbackGenerationOK := continuationEvaluationCleanupStrictInt64(rawRollbackGeneration)
		pointerGeneration, pointerGenerationOK := continuationEvaluationCleanupStrictInt64(pointer["generation"])
		if !rollbackGenerationOK || !pointerGenerationOK || recordedRollbackGeneration != pointerGeneration {
			return errors.New("marker migration rollback epoch binding is invalid")
		}
	}
	recordedCount, countOK := continuationEvaluationCleanupStrictInt(record["old_max_marker_count"])
	recordedBytes, bytesOK := continuationEvaluationCleanupStrictInt(record["old_max_marker_bytes"])
	if !countOK || !bytesOK || recordedCount != oldCount || int64(recordedBytes) != oldBytes {
		return errors.New("marker migration rollback cap binding is invalid")
	}
	if anyToString(pointer["state"]) == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		pointerGeneration, pointerGenerationOK := continuationEvaluationCleanupStrictInt64(pointer["generation"])
		planGeneration, planGenerationOK := continuationEvaluationCleanupMarkerMigrationPlanGeneration(plan, "generation")
		pointerTargetGeneration, pointerTargetGenerationOK := continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(pointer)
		if !pointerTargetGenerationOK {
			if _, legacyPointer := pointer["target_generation"]; !legacyPointer {
				pointerTargetGeneration, pointerTargetGenerationOK = targetGeneration, true
			}
		}
		if !pointerGenerationOK || !planGenerationOK || !pointerTargetGenerationOK || pointerTargetGeneration != targetGeneration || anyToString(pointer["plan_digest"]) != anyToString(plan["previous_plan_digest"]) || anyToString(pointer["receipt_digest"]) != anyToString(plan["previous_receipt_digest"]) {
			return errors.New("marker migration rollback active pointer target binding is invalid")
		}
		if _, hasTargetGeneration := pointer["target_generation"]; hasTargetGeneration {
			if pointerGeneration <= planGeneration {
				return errors.New("marker migration rollback epoch did not advance")
			}
		} else if previousGeneration, previousGenerationOK := continuationEvaluationCleanupStrictInt64(plan["previous_generation"]); !previousGenerationOK || pointerGeneration != previousGeneration {
			// Read-only compatibility for a pre-split pointer. Reconciliation
			// immediately consumes a fresh epoch before accepting it as current.
			return errors.New("legacy marker migration rollback pointer generation is invalid")
		}
	} else if anyToInt(pointer["generation"], -1) != anyToInt(plan["generation"], -2) || anyToString(pointer["plan_digest"]) != strings.TrimSpace(planDigest) {
		return errors.New("marker migration rollback active pointer binding is invalid")
	}
	// Committed-extension rollback records carry an exact pre/post inventory
	// proof. Prepared-plan rollback has no inventory yet and is valid only when
	// those fields are absent, avoiding an ambiguous partial proof.
	hasPre := record["pre_inventory_count"] != nil || record["pre_inventory_bytes"] != nil || record["pre_inventory_digest"] != nil
	hasPost := record["post_inventory_count"] != nil || record["post_inventory_bytes"] != nil || record["post_inventory_digest"] != nil
	if hasPre || hasPost {
		for _, prefix := range []string{"pre_inventory", "post_inventory"} {
			count, countOK := continuationEvaluationCleanupStrictInt(record[prefix+"_count"])
			bytes, bytesOK := continuationEvaluationCleanupStrictInt(record[prefix+"_bytes"])
			digest := strings.TrimSpace(anyToString(record[prefix+"_digest"]))
			if !countOK || !bytesOK || count < 0 || int64(bytes) < 0 || digest == "" || !utilitySHA256DigestValid(digest) {
				return errors.New("marker migration rollback inventory proof is invalid")
			}
		}
		preCount := anyToInt(record["pre_inventory_count"], -1)
		preBytes := int64(anyToInt(record["pre_inventory_bytes"], -1))
		if preCount != anyToInt(record["post_inventory_count"], -2) || preBytes != int64(anyToInt(record["post_inventory_bytes"], -2)) || anyToString(record["pre_inventory_digest"]) != anyToString(record["post_inventory_digest"]) {
			return errors.New("marker migration rollback inventory changed")
		}
	}
	return nil
}

func (q *continuationDurableQueue) rollbackEvaluationCleanupMarkerIndexCaps(operatorRef, planDigest, principal, workspace string) error {
	if q == nil {
		return errors.New("nil continuation durable queue")
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	unlockOwner, lockErr := acquireEvaluationCleanupMarkerMigrationOwnerLock(q.dir)
	if lockErr != nil {
		return lockErr
	}
	defer unlockOwner()
	if strings.TrimSpace(operatorRef) == "" || strings.TrimSpace(planDigest) == "" || !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(principal) || !continuationEvaluationCleanupMarkerMigrationRecordPrincipalValid(workspace) {
		return errors.New("marker migration rollback authorization is incomplete")
	}
	if !continuationEvaluationCleanupMarkerMigrationOpaqueFieldValid(operatorRef, 128) {
		return errors.New("marker migration rollback operator reference is not a safe opaque identifier")
	}
	pointer, err := q.readActiveMarkerMigrationPointerLocked()
	if err != nil {
		return err
	}
	if anyToString(pointer["plan_digest"]) != strings.TrimSpace(planDigest) || anyToString(pointer["state"]) == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		return errors.New("marker migration rollback plan binding is invalid")
	}
	plan, baseReceipt, err := q.migrationPlanAndReceiptLocked(planDigest)
	if err != nil {
		return err
	}
	if anyToString(plan["operator_ref"]) != strings.TrimSpace(operatorRef) || anyToString(plan["authenticated_principal"]) != strings.TrimSpace(principal) || anyToString(plan["workspace_id"]) != strings.TrimSpace(workspace) {
		return errors.New("marker migration rollback principal or workspace binding is invalid")
	}
	oldCount, countOK := continuationEvaluationCleanupStrictInt(plan["old_max_marker_count"])
	oldBytes, bytesOK := continuationEvaluationCleanupStrictInt(plan["old_max_marker_bytes"])
	if !countOK || !bytesOK || oldCount <= 0 || oldCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || int64(oldBytes) <= 0 || int64(oldBytes) > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes {
		return errors.New("marker migration rollback old cap is invalid")
	}
	currentEpoch, currentEpochOK := continuationEvaluationCleanupStrictInt64(pointer["generation"])
	planEpoch, planEpochOK := continuationEvaluationCleanupMarkerMigrationPlanGeneration(plan, "generation")
	if !currentEpochOK || !planEpochOK || currentEpoch != planEpoch {
		return errors.New("marker migration rollback current epoch binding is invalid")
	}
	rollbackEpoch, rollbackEpochErr := nextEvaluationCleanupMarkerMigrationEpoch(currentEpoch)
	if rollbackEpochErr != nil {
		return rollbackEpochErr
	}
	targetGeneration, targetGenerationOK := continuationEvaluationCleanupMarkerMigrationPlanPreviousTargetGeneration(plan)
	if !targetGenerationOK {
		return errors.New("marker migration rollback target generation is invalid")
	}
	rollbackName := continuationEvaluationCleanupMarkerMigrationRollbackFile(planDigest)
	if existing, existingErr := q.readMarkerMigrationRecordLocked(rollbackName); existingErr == nil {
		if err := continuationEvaluationCleanupMarkerMigrationRollbackRecordValid(existing, planDigest, plan, pointer, oldCount, int64(oldBytes), operatorRef, principal, workspace); err != nil {
			return err
		}
		rollbackGeneration := rollbackEpoch
		rollbackPlanDigest := anyToString(plan["previous_plan_digest"])
		rollbackReceiptDigest := anyToString(plan["previous_receipt_digest"])
		rollbackPointer := map[string]any{
			"state": continuationEvaluationCleanupMarkerMigrationStateRolledBack, "generation": rollbackGeneration,
			"target_generation": targetGeneration,
			"plan_digest":       rollbackPlanDigest, "receipt_digest": rollbackReceiptDigest,
			"rollback_plan_digest": planDigest, "rollback_digest": existing["rollback_digest"], "max_marker_count": oldCount, "max_marker_bytes": oldBytes,
			"authenticated_principal": principal, "workspace_id": workspace,
		}
		if !continuationEvaluationCleanupMarkerMigrationPointerShapeValid(rollbackPointer) {
			return errors.New("marker migration rollback pointer shape is invalid")
		}
		if err := q.writeAndConfirmMarkerMigrationPointerLocked(rollbackPointer); err != nil {
			return err
		}
		q.evaluationCleanupMarkerMaxCount = oldCount
		q.evaluationCleanupMarkerMaxBytes = int64(oldBytes)
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStateRolledBack
		q.evaluationCleanupMarkerMigrationGeneration = rollbackGeneration
		q.evaluationCleanupMarkerMigrationTargetGeneration = targetGeneration
		q.evaluationCleanupMarkerMigrationPlanDigest = rollbackPlanDigest
		q.evaluationCleanupMarkerMigrationReceiptDigest = rollbackReceiptDigest
		return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, q.evaluationCleanupMarkerPendingRef, q.evaluationCleanupMarkerPendingBytes)
	} else if !errors.Is(existingErr, os.ErrNotExist) {
		return existingErr
	}
	if baseReceipt != nil && anyToString(baseReceipt["state"]) == continuationEvaluationCleanupMarkerMigrationStateCommitted {
		// A committed extension is rollback-safe only when the exact current
		// inventory still fits the recorded old cap. The rollback record is a
		// third immutable receipt; the original plan and commit receipt remain
		// untouched for audit and replay proof.
		inventory, inventoryErr := q.inventoryEvaluationCleanupMarkersLocked()
		if inventoryErr != nil {
			return inventoryErr
		}
		if inventory.Count > oldCount || inventory.Bytes > int64(oldBytes) {
			return errors.New("marker migration rollback refused because current inventory exceeds old cap")
		}
		postInventory, postErr := q.inventoryEvaluationCleanupMarkersLocked()
		if postErr != nil {
			return postErr
		}
		if postInventory.Count != inventory.Count || postInventory.Bytes != inventory.Bytes || postInventory.Digest != inventory.Digest {
			return errors.New("marker migration rollback inventory changed during authorization")
		}
		rollback := map[string]any{
			"schema_id":               continuationEvaluationCleanupMarkerMigrationSchemaID,
			"version":                 continuationEvaluationCleanupMarkerMigrationVersion,
			"authority":               continuationEvaluationCleanupMarkerMigrationAuthority,
			"action":                  continuationEvaluationCleanupMarkerMigrationAction,
			"state":                   continuationEvaluationCleanupMarkerMigrationStateRolledBack,
			"native_owner":            continuationEvaluationCleanupMarkerMigrationNativeOwner,
			"authorization":           continuationEvaluationCleanupMarkerMigrationAuthorization,
			"operator_ref":            strings.TrimSpace(operatorRef),
			"plan_digest":             strings.TrimSpace(planDigest),
			"previous_receipt_digest": anyToString(baseReceipt["receipt_digest"]),
			"old_max_marker_count":    oldCount,
			"old_max_marker_bytes":    oldBytes,
			"pre_inventory_count":     inventory.Count,
			"pre_inventory_bytes":     inventory.Bytes,
			"pre_inventory_digest":    inventory.Digest,
			"post_inventory_count":    postInventory.Count,
			"post_inventory_bytes":    postInventory.Bytes,
			"post_inventory_digest":   postInventory.Digest,
			"authenticated_principal": strings.TrimSpace(principal),
			"workspace_id":            strings.TrimSpace(workspace),
			"target_generation":       targetGeneration,
			"rollback_generation":     rollbackEpoch,
			"target_plan_digest":      plan["previous_plan_digest"],
			"target_receipt_digest":   plan["previous_receipt_digest"],
			"rolled_back_at":          nowUTCISO(),
		}
		rollback["rollback_digest"] = continuationEvaluationCleanupMarkerMigrationDigest(rollback, "rollback_digest")
		if err := q.writeImmutableMarkerMigrationRecordLocked(rollbackName, rollback); err != nil {
			return err
		}
		rollbackGeneration := rollbackEpoch
		rollbackPlanDigest := anyToString(plan["previous_plan_digest"])
		rollbackPointer := map[string]any{
			"state": continuationEvaluationCleanupMarkerMigrationStateRolledBack, "generation": rollbackGeneration,
			"target_generation": targetGeneration,
			"plan_digest":       rollbackPlanDigest, "receipt_digest": plan["previous_receipt_digest"],
			"rollback_plan_digest": planDigest, "rollback_digest": rollback["rollback_digest"], "max_marker_count": oldCount, "max_marker_bytes": oldBytes,
			"authenticated_principal": principal, "workspace_id": workspace,
		}
		if !continuationEvaluationCleanupMarkerMigrationPointerShapeValid(rollbackPointer) {
			return errors.New("marker migration rollback pointer shape is invalid")
		}
		if err := q.writeAndConfirmMarkerMigrationPointerLocked(rollbackPointer); err != nil {
			return err
		}
		q.evaluationCleanupMarkerMaxCount = oldCount
		q.evaluationCleanupMarkerMaxBytes = int64(oldBytes)
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStateRolledBack
		q.evaluationCleanupMarkerMigrationGeneration = rollbackGeneration
		q.evaluationCleanupMarkerMigrationTargetGeneration = targetGeneration
		q.evaluationCleanupMarkerMigrationPlanDigest = rollbackPlanDigest
		// The active pointer's receipt_digest names the target generation's
		// receipt.  The rollback receipt has its own rollback_digest field and is
		// retained as audit custody, but must not masquerade as that target receipt
		// in the in-memory active-generation binding.
		q.evaluationCleanupMarkerMigrationReceiptDigest = anyToString(plan["previous_receipt_digest"])
		return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, q.evaluationCleanupMarkerPendingRef, q.evaluationCleanupMarkerPendingBytes)
	}
	// A prepared plan has no committed receipt. The rollback record is still
	// immutable evidence and the active pointer moves back to the prior
	// generation; no extension is ever inferred from a missing receipt.
	receipt := map[string]any{
		"schema_id":               continuationEvaluationCleanupMarkerMigrationSchemaID,
		"version":                 continuationEvaluationCleanupMarkerMigrationVersion,
		"authority":               continuationEvaluationCleanupMarkerMigrationAuthority,
		"action":                  continuationEvaluationCleanupMarkerMigrationAction,
		"state":                   continuationEvaluationCleanupMarkerMigrationStateRolledBack,
		"native_owner":            continuationEvaluationCleanupMarkerMigrationNativeOwner,
		"authorization":           continuationEvaluationCleanupMarkerMigrationAuthorization,
		"operator_ref":            strings.TrimSpace(operatorRef),
		"plan_digest":             strings.TrimSpace(planDigest),
		"old_max_marker_count":    plan["old_max_marker_count"],
		"old_max_marker_bytes":    plan["old_max_marker_bytes"],
		"authenticated_principal": strings.TrimSpace(principal),
		"workspace_id":            strings.TrimSpace(workspace),
		"target_generation":       targetGeneration,
		"rollback_generation":     rollbackEpoch,
		"target_plan_digest":      plan["previous_plan_digest"],
		"target_receipt_digest":   plan["previous_receipt_digest"],
		"rolled_back_at":          nowUTCISO(),
	}
	receipt["rollback_digest"] = continuationEvaluationCleanupMarkerMigrationDigest(receipt, "rollback_digest")
	if err := q.writeImmutableMarkerMigrationRecordLocked(rollbackName, receipt); err != nil {
		return err
	}
	rollbackGeneration := rollbackEpoch
	rollbackPlanDigest := anyToString(plan["previous_plan_digest"])
	rollbackPointer := map[string]any{
		"state": continuationEvaluationCleanupMarkerMigrationStateRolledBack, "generation": rollbackGeneration,
		"target_generation": targetGeneration,
		"plan_digest":       rollbackPlanDigest, "receipt_digest": plan["previous_receipt_digest"],
		"rollback_plan_digest": planDigest, "rollback_digest": receipt["rollback_digest"], "max_marker_count": oldCount, "max_marker_bytes": oldBytes,
		"authenticated_principal": principal, "workspace_id": workspace,
	}
	if !continuationEvaluationCleanupMarkerMigrationPointerShapeValid(rollbackPointer) {
		return errors.New("marker migration rollback pointer shape is invalid")
	}
	if err := q.writeAndConfirmMarkerMigrationPointerLocked(rollbackPointer); err != nil {
		return err
	}
	q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStateRolledBack
	q.evaluationCleanupMarkerMigrationGeneration = rollbackGeneration
	q.evaluationCleanupMarkerMigrationTargetGeneration = targetGeneration
	q.evaluationCleanupMarkerMigrationPlanDigest = rollbackPlanDigest
	q.evaluationCleanupMarkerMigrationReceiptDigest = anyToString(plan["previous_receipt_digest"])
	return q.writeEvaluationCleanupMarkerIndexLocked(q.evaluationCleanupMarkerState, q.evaluationCleanupMarkerPendingRef, q.evaluationCleanupMarkerPendingBytes)
}

func (q *continuationDurableQueue) reconcileEvaluationCleanupMarkerMigrationLocked() error {
	pointer, pointerErr := q.readActiveMarkerMigrationPointerLocked()
	if errors.Is(pointerErr, os.ErrNotExist) {
		// Read-only compatibility for a pre-pointer installation. New writes
		// never use these fixed names; an ambiguous legacy record fails closed.
		legacyPlan, legacyErr := q.readMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationPlanFile)
		if errors.Is(legacyErr, os.ErrNotExist) {
			return nil
		}
		if legacyErr != nil {
			return legacyErr
		}
		legacyDigest := anyToString(legacyPlan["plan_digest"])
		if legacyDigest == "" || continuationEvaluationCleanupMarkerMigrationDigest(legacyPlan, "plan_digest") != legacyDigest {
			return errors.New("legacy marker migration plan digest is invalid")
		}
		legacyReceipt, receiptErr := q.readMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationReceiptFile)
		if errors.Is(receiptErr, os.ErrNotExist) {
			q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
			q.evaluationCleanupMarkerMigrationPlanDigest = legacyDigest
			return nil
		}
		if receiptErr != nil {
			return receiptErr
		}
		if anyToString(legacyReceipt["plan_digest"]) != legacyDigest || anyToString(legacyReceipt["receipt_digest"]) == "" || continuationEvaluationCleanupMarkerMigrationDigest(legacyReceipt, "receipt_digest") != anyToString(legacyReceipt["receipt_digest"]) {
			return errors.New("legacy marker migration receipt binding or digest is invalid")
		}
		newCount, countOK := continuationEvaluationCleanupStrictInt(legacyReceipt["new_max_marker_count"])
		newBytes, bytesOK := continuationEvaluationCleanupStrictInt(legacyReceipt["new_max_marker_bytes"])
		if !countOK || !bytesOK || newCount <= 0 || newCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || int64(newBytes) <= 0 || int64(newBytes) > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes {
			return errors.New("legacy marker migration receipt cap is invalid")
		}
		q.evaluationCleanupMarkerMaxCount = newCount
		q.evaluationCleanupMarkerMaxBytes = int64(newBytes)
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStateCommitted
		q.evaluationCleanupMarkerMigrationPlanDigest = legacyDigest
		q.evaluationCleanupMarkerMigrationReceiptDigest = anyToString(legacyReceipt["receipt_digest"])
		q.evaluationCleanupMarkerMigrationGeneration = 1
		q.evaluationCleanupMarkerMigrationTargetGeneration = 1
		return nil
	}
	if pointerErr != nil {
		return pointerErr
	}
	state := anyToString(pointer["state"])
	planDigest := anyToString(pointer["plan_digest"])
	if state == continuationEvaluationCleanupMarkerMigrationStateRolledBack {
		rollbackPlanDigest := anyToString(pointer["rollback_plan_digest"])
		rollbackDigest := anyToString(pointer["rollback_digest"])
		if rollbackPlanDigest == "" || rollbackDigest == "" {
			return errors.New("marker migration rollback pointer custody is incomplete")
		}
		rollbackPlan, _, rollbackPlanErr := q.migrationPlanAndReceiptLocked(rollbackPlanDigest)
		if rollbackPlanErr != nil {
			return rollbackPlanErr
		}
		rollback, rollbackErr := q.readMarkerMigrationRecordLocked(continuationEvaluationCleanupMarkerMigrationRollbackFile(rollbackPlanDigest))
		if rollbackErr != nil {
			return rollbackErr
		}
		maxCount, countOK := continuationEvaluationCleanupStrictInt(pointer["max_marker_count"])
		maxBytes, bytesOK := continuationEvaluationCleanupStrictInt(pointer["max_marker_bytes"])
		if !countOK || !bytesOK || maxCount <= 0 || maxCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || maxBytes <= 0 || int64(maxBytes) > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes {
			return errors.New("marker migration rollback pointer cap is invalid")
		}
		if err := continuationEvaluationCleanupMarkerMigrationRollbackRecordValid(rollback, rollbackPlanDigest, rollbackPlan, pointer, maxCount, int64(maxBytes), anyToString(rollback["operator_ref"]), anyToString(pointer["authenticated_principal"]), anyToString(pointer["workspace_id"])); err != nil {
			return err
		}
		targetGeneration, targetGenerationOK := continuationEvaluationCleanupStrictInt64(rollback["target_generation"])
		if !targetGenerationOK {
			return errors.New("marker migration rollback record target generation is invalid")
		}
		pointerGeneration, pointerGenerationOK := continuationEvaluationCleanupStrictInt64(pointer["generation"])
		planGeneration, planGenerationOK := continuationEvaluationCleanupMarkerMigrationPlanGeneration(rollbackPlan, "generation")
		if !pointerGenerationOK || !planGenerationOK {
			return errors.New("marker migration rollback epoch is invalid")
		}
		if _, hasTargetGeneration := pointer["target_generation"]; !hasTargetGeneration || pointerGeneration <= planGeneration {
			// Older rollback pointers reused previous_generation, which made a
			// later extension able to reuse an expected CAS token. Consume a fresh
			// epoch while preserving the immutable rollback record and its digest.
			baseEpoch := pointerGeneration
			if planGeneration > baseEpoch {
				baseEpoch = planGeneration
			}
			upgradedEpoch, epochErr := nextEvaluationCleanupMarkerMigrationEpoch(baseEpoch)
			if epochErr != nil {
				return epochErr
			}
			upgradedPointer := cloneAnyMap(pointer)
			upgradedPointer["generation"] = upgradedEpoch
			upgradedPointer["target_generation"] = targetGeneration
			if err := q.writeAndConfirmMarkerMigrationPointerLocked(upgradedPointer); err != nil {
				return err
			}
			readback, readErr := q.readActiveMarkerMigrationPointerLocked()
			if readErr != nil {
				return readErr
			}
			pointer = readback
			if err := continuationEvaluationCleanupMarkerMigrationRollbackRecordValid(rollback, rollbackPlanDigest, rollbackPlan, pointer, maxCount, int64(maxBytes), anyToString(rollback["operator_ref"]), anyToString(pointer["authenticated_principal"]), anyToString(pointer["workspace_id"])); err != nil {
				return err
			}
		}
		q.evaluationCleanupMarkerMaxCount, q.evaluationCleanupMarkerMaxBytes = maxCount, int64(maxBytes)
		q.evaluationCleanupMarkerMigrationState = state
		q.evaluationCleanupMarkerMigrationGeneration, _ = continuationEvaluationCleanupStrictInt64(pointer["generation"])
		q.evaluationCleanupMarkerMigrationTargetGeneration = targetGeneration
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		q.evaluationCleanupMarkerMigrationReceiptDigest = anyToString(pointer["receipt_digest"])
		return nil
	}
	if planDigest == "" {
		return errors.New("marker migration active pointer plan digest is missing")
	}
	plan, receipt, err := q.migrationPlanAndReceiptLocked(planDigest)
	if err != nil {
		return err
	}
	planGeneration, planGenerationOK := continuationEvaluationCleanupMarkerMigrationPlanGeneration(plan, "generation")
	pointerGeneration, pointerGenerationOK := continuationEvaluationCleanupStrictInt64(pointer["generation"])
	if !planGenerationOK || !pointerGenerationOK || planGeneration != pointerGeneration || anyToString(plan["authenticated_principal"]) != anyToString(pointer["authenticated_principal"]) || anyToString(plan["workspace_id"]) != anyToString(pointer["workspace_id"]) {
		return errors.New("marker migration active pointer plan binding is invalid")
	}
	if targetGeneration, targetGenerationOK := continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(pointer); !targetGenerationOK || targetGeneration != planGeneration {
		if _, legacyPointer := pointer["target_generation"]; !legacyPointer {
			upgradedPointer := cloneAnyMap(pointer)
			upgradedPointer["target_generation"] = planGeneration
			if err := q.writeAndConfirmMarkerMigrationPointerLocked(upgradedPointer); err != nil {
				return err
			}
			pointer, err = q.readActiveMarkerMigrationPointerLocked()
			if err != nil {
				return err
			}
		} else {
			return errors.New("marker migration active pointer target generation is invalid")
		}
	} else if _, legacyPointer := pointer["target_generation"]; !legacyPointer {
		upgradedPointer := cloneAnyMap(pointer)
		upgradedPointer["target_generation"] = planGeneration
		if err := q.writeAndConfirmMarkerMigrationPointerLocked(upgradedPointer); err != nil {
			return err
		}
		pointer, err = q.readActiveMarkerMigrationPointerLocked()
		if err != nil {
			return err
		}
	}
	targetGeneration, targetGenerationOK := continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(pointer)
	if !targetGenerationOK {
		return errors.New("marker migration active pointer target generation is invalid")
	}
	if receipt == nil {
		if state != continuationEvaluationCleanupMarkerMigrationStatePrepared {
			return errors.New("marker migration active pointer is missing its receipt")
		}
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		q.evaluationCleanupMarkerMigrationReceiptDigest = ""
		q.evaluationCleanupMarkerMigrationGeneration = planGeneration
		q.evaluationCleanupMarkerMigrationTargetGeneration = targetGeneration
		return nil
	}
	if anyToString(receipt["state"]) != continuationEvaluationCleanupMarkerMigrationStateCommitted {
		return errors.New("marker migration receipt state is invalid")
	}
	if state != continuationEvaluationCleanupMarkerMigrationStatePrepared && state != continuationEvaluationCleanupMarkerMigrationStateCommitted {
		return errors.New("marker migration active pointer state is invalid")
	}
	newCount, countOK := continuationEvaluationCleanupStrictInt(receipt["new_max_marker_count"])
	newBytes, bytesOK := continuationEvaluationCleanupStrictInt(receipt["new_max_marker_bytes"])
	if !countOK || !bytesOK || newCount <= 0 || newCount > continuationEvaluationCleanupMarkerIndexAbsoluteMaxCount || int64(newBytes) <= 0 || int64(newBytes) > continuationEvaluationCleanupMarkerIndexAbsoluteMaxBytes {
		return errors.New("marker migration receipt cap is invalid")
	}
	if anyToString(receipt["receipt_digest"]) == "" || (state == continuationEvaluationCleanupMarkerMigrationStateCommitted && anyToString(receipt["receipt_digest"]) != anyToString(pointer["receipt_digest"])) {
		return errors.New("marker migration committed receipt binding is invalid")
	}
	if state == continuationEvaluationCleanupMarkerMigrationStatePrepared {
		// Receipt durability won the crash window.  The committed pointer must be
		// durably confirmed before any cap or migration state becomes visible.
		committedPointer := map[string]any{
			"state": continuationEvaluationCleanupMarkerMigrationStateCommitted, "generation": plan["generation"], "plan_digest": planDigest,
			"target_generation": plan["generation"],
			"receipt_digest":    receipt["receipt_digest"], "max_marker_count": newCount, "max_marker_bytes": newBytes,
			"authenticated_principal": plan["authenticated_principal"], "workspace_id": plan["workspace_id"],
		}
		if err := q.writeAndConfirmMarkerMigrationPointerLocked(committedPointer); err != nil {
			q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
			q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
			q.evaluationCleanupMarkerMigrationReceiptDigest = ""
			q.lastError = err.Error()
			return err
		}
		readback, readbackErr := q.readActiveMarkerMigrationPointerLocked()
		if readbackErr != nil || !continuationEvaluationCleanupMarkerMigrationPointerMatches(readback, committedPointer) {
			if readbackErr == nil {
				readbackErr = errors.New("marker migration committed pointer readback mismatch")
			}
			q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStatePendingRecovery
			q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
			q.evaluationCleanupMarkerMigrationReceiptDigest = ""
			q.lastError = readbackErr.Error()
			return readbackErr
		}
		state = continuationEvaluationCleanupMarkerMigrationStateCommitted
		pointer = readback
	}
	if state == continuationEvaluationCleanupMarkerMigrationStateCommitted {
		pointerGeneration, pointerGenerationOK := continuationEvaluationCleanupStrictInt64(pointer["generation"])
		if !pointerGenerationOK || anyToString(pointer["plan_digest"]) != planDigest || pointerGeneration != planGeneration || continuationEvaluationCleanupMarkerMigrationPointerTargetGenerationValue(pointer) != planGeneration || anyToString(pointer["authenticated_principal"]) != anyToString(plan["authenticated_principal"]) || anyToString(pointer["workspace_id"]) != anyToString(plan["workspace_id"]) || anyToInt(pointer["max_marker_count"], -1) != newCount || anyToInt(pointer["max_marker_bytes"], -1) != newBytes {
			return errors.New("marker migration committed pointer binding is invalid")
		}
		q.evaluationCleanupMarkerMaxCount, q.evaluationCleanupMarkerMaxBytes = newCount, int64(newBytes)
		q.evaluationCleanupMarkerMigrationState = continuationEvaluationCleanupMarkerMigrationStateCommitted
		q.evaluationCleanupMarkerMigrationPlanDigest = planDigest
		q.evaluationCleanupMarkerMigrationReceiptDigest = anyToString(receipt["receipt_digest"])
		q.evaluationCleanupMarkerMigrationGeneration = planGeneration
		q.evaluationCleanupMarkerMigrationTargetGeneration = planGeneration
		return nil
	}
	return errors.New("marker migration active pointer state is invalid")
}
