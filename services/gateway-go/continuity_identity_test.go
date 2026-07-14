package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestContinuityStore(t *testing.T) *continuityStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "continuity.ndjson")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH", path)
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC", "false")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_MAX_BYTES", "1048576")
	store, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("new continuity store: %v", err)
	}
	t.Cleanup(store.close)
	return store
}

func continuityIdentityID(t *testing.T, result map[string]any) string {
	t.Helper()
	id := strings.TrimSpace(anyToString(result["task_identity_id"]))
	if id == "" {
		t.Fatalf("missing task identity id: %#v", result)
	}
	return id
}

func TestContinuityLedgerDefaultsToPersistentMemoryRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CONTEXTLATTICE_CONTINUITY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH", "")
	t.Setenv("GO_MEMORY_STORE_ROOT", root)
	want := filepath.Join(root, "_contextlattice", "continuity_ledger.ndjson")
	if got := continuityLedgerPath(); got != want {
		t.Fatalf("continuity ledger escaped persistent memory root: got=%s want=%s", got, want)
	}
	store, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("initialize continuity store under memory root: %v", err)
	}
	if store.path != want {
		t.Fatalf("continuity store path mismatch: got=%s want=%s", store.path, want)
	}
}

func TestContinuityReconciliationExactFirstAndSemanticAbstention(t *testing.T) {
	store := newTestContinuityStore(t)
	if _, err := store.reconcile(map[string]any{
		"project": "contextlattice", "taskIdentityId": "task_missing_camel_only",
	}, false); !errors.Is(err, errContinuityIdentityMissing) {
		t.Fatalf("camel-only explicit identity did not use exact validation: %v", err)
	}
	base := map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "T1",
		"objective": "Implement durable task identity reconciliation", "branch": "main", "agent_id": "codex_test",
	}
	created, err := store.reconcile(base, true)
	if err != nil {
		t.Fatalf("create identity: %v", err)
	}
	if anyToString(created["match_mode"]) != "created" {
		t.Fatalf("expected created identity, got %#v", created)
	}
	identityID := continuityIdentityID(t, created)
	mainLane := anyToString(created["execution_lane_id"])
	if mainLane == "" {
		t.Fatalf("expected execution lane: %#v", created)
	}
	beforeScopeEntries := len(store.entries)
	for _, scoped := range []map[string]any{
		{"project": "other-project", "repo": "contextlattice", "task_identity_id": identityID},
		{"project": "contextlattice", "repo": "other-repo", "task_identity_id": identityID},
	} {
		if _, err := store.reconcile(scoped, false); err == nil || !strings.Contains(err.Error(), "scope") {
			t.Fatalf("explicit identity crossed project/repo scope: payload=%#v err=%v", scoped, err)
		}
	}
	if len(store.entries) != beforeScopeEntries {
		t.Fatalf("rejected explicit scope mismatch mutated ledger")
	}

	exactTask, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "T1",
		"objective": "Unrelated wording cannot outrank exact task id", "branch": "main", "agent_id": "codex_test",
	}, true)
	if err != nil || anyToString(exactTask["match_mode"]) != "exact_task_id" || continuityIdentityID(t, exactTask) != identityID {
		t.Fatalf("exact task id did not win: result=%#v err=%v", exactTask, err)
	}

	exactObjective, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "T1-renamed",
		"objective": "Implement durable task identity reconciliation", "branch": "release", "agent_id": "codex_test",
	}, true)
	if err != nil || anyToString(exactObjective["match_mode"]) != "exact_objective" || continuityIdentityID(t, exactObjective) != identityID {
		t.Fatalf("exact objective did not reconcile: result=%#v err=%v", exactObjective, err)
	}
	if releaseLane := anyToString(exactObjective["execution_lane_id"]); releaseLane == "" || releaseLane == mainLane {
		t.Fatalf("execution lane must remain separate from task identity: main=%s release=%s", mainLane, releaseLane)
	}

	semantic, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "T1-semantic",
		"objective": "Implement durable task identity reconciliation safely", "branch": "main", "agent_id": "codex_test",
	}, true)
	if err != nil {
		t.Fatalf("semantic candidate: %v", err)
	}
	if anyToString(semantic["match_mode"]) != "semantic_candidate" || !anyToBool(semantic["abstained"]) || !anyToBool(semantic["requires_confirmation"]) {
		t.Fatalf("semantic candidate must abstain: %#v", semantic)
	}
	if anyToString(semantic["task_identity_id"]) != "" || anyToBool(semantic["semantic_auto_merge"]) {
		t.Fatalf("semantic candidate was silently bound: %#v", semantic)
	}
	if len(store.taskIdentities) != 1 {
		t.Fatalf("semantic advisory mutated identities: %d", len(store.taskIdentities))
	}
	beforeEntries := len(store.entries)
	if _, err := store.reconcile(map[string]any{
		"project": "contextlattice", "objective": "reject malformed execution lane",
		"execution_lane_id": "lane with spaces",
	}, true); err == nil || !strings.Contains(err.Error(), "execution_lane_id") {
		t.Fatalf("invalid execution lane was not rejected: %v", err)
	}
	if len(store.entries) != beforeEntries {
		t.Fatalf("invalid execution lane mutated ledger")
	}
}

func TestObjectiveLinksRequireKnownSameProjectTaskIdentity(t *testing.T) {
	store := newTestContinuityStore(t)
	created, err := store.reconcile(map[string]any{
		"project": "alpha", "repo": "alpha-repo", "task_id": "alpha-task", "objective": "Scoped task linkage",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	identityID := continuityIdentityID(t, created)
	beforeEntries := len(store.entries)
	for _, payload := range []map[string]any{
		{"project": "alpha", "objective_id": "obj_unknown_task", "transition_type": "created", "actor": "codex", "task_identity_id": "task_missing"},
		{"project": "beta", "objective_id": "obj_cross_project", "transition_type": "created", "actor": "codex", "task_identity_id": identityID},
		{"project": "alpha", "objective_id": "obj_unknown_decision", "transition_type": "decision_changed", "actor": "codex", "decision_change_id": "dc_missing"},
	} {
		if _, err := store.recordObjectiveTransition(payload); err == nil ||
			(!strings.Contains(err.Error(), "task_identity_id") && !strings.Contains(err.Error(), "decision_change_id")) {
			t.Fatalf("invalid task linkage was accepted: payload=%#v err=%v", payload, err)
		}
	}
	if len(store.entries) != beforeEntries {
		t.Fatalf("rejected task linkage mutated ledger")
	}
	transition, err := store.recordObjectiveTransition(map[string]any{
		"project": "alpha", "objective_id": "obj_scoped_task", "transition_type": "created", "actor": "codex", "task_identity_id": identityID,
	})
	if err != nil || transition.TaskIdentityID != identityID {
		t.Fatalf("valid task linkage failed: transition=%#v err=%v", transition, err)
	}
}

func TestContinuityManualSplitMergeReceiptsAndCapacity(t *testing.T) {
	store := newTestContinuityStore(t)
	created, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "root",
		"objective": "Ship continuity identity", "branch": "main", "agent_id": "codex_test",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	rootID := continuityIdentityID(t, created)
	split, err := store.splitTaskIdentity(map[string]any{
		"source_task_identity_id": rootID, "task_id": "split-task", "objective": "Ship continuity identity",
		"actor": "operator", "reason": "separate independent acceptance lane", "branch": "release",
	})
	if err != nil {
		t.Fatalf("split identity: %v", err)
	}
	splitID := continuityIdentityID(t, split)
	if splitID == rootID || anyToString(split["match_mode"]) != "manual_split" {
		t.Fatalf("invalid split result: %#v", split)
	}
	if findings := validateAgentContractPayload(taskIdentityReceiptContractID, anyMap(split["receipt"])); len(findings) != 0 {
		t.Fatalf("split receipt violates nested contract: %#v", findings)
	}
	ambiguous, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "objective": "Ship continuity identity",
	}, true)
	if err != nil || anyToString(ambiguous["match_mode"]) != "ambiguous_exact" || !anyToBool(ambiguous["abstained"]) {
		t.Fatalf("split identities must force exact ambiguity: result=%#v err=%v", ambiguous, err)
	}
	merged, err := store.mergeTaskIdentities(map[string]any{
		"target_task_identity_id": rootID, "source_task_identity_ids": []any{splitID},
		"actor": "operator", "reason": "human confirmed both branches are one task",
	})
	if err != nil {
		t.Fatalf("merge identity: %v", err)
	}
	if anyToString(merged["match_mode"]) != "manual_merge" || store.taskIdentities[splitID].MergedInto != rootID {
		t.Fatalf("invalid merge result: %#v", merged)
	}
	if findings := validateAgentContractPayload(taskIdentityReceiptContractID, anyMap(merged["receipt"])); len(findings) != 0 {
		t.Fatalf("merge receipt violates nested contract: %#v", findings)
	}
	alias, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "split-task",
	}, false)
	if err != nil || continuityIdentityID(t, alias) != rootID {
		t.Fatalf("merged alias did not redirect: result=%#v err=%v", alias, err)
	}
	if len(store.entries) != 3 {
		t.Fatalf("expected immutable create/split/merge receipts, entries=%d", len(store.entries))
	}
	afterMerge, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "objective": "Ship continuity identity",
	}, false)
	if err != nil || anyToString(afterMerge["match_mode"]) != "exact_objective" || continuityIdentityID(t, afterMerge) != rootID {
		t.Fatalf("merged objective index did not converge on target: result=%#v err=%v", afterMerge, err)
	}
	reloaded := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries,
		entries: []continuityLedgerEntry{}, taskIdentities: map[string]taskIdentityRecord{}, taskAliases: map[string]string{},
	}
	if err := reloaded.load(); err != nil {
		t.Fatalf("reload merged identity ledger: %v", err)
	}
	reloadedAlias, err := reloaded.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "objective": "Ship continuity identity",
	}, false)
	if err != nil || anyToString(reloadedAlias["match_mode"]) != "exact_objective" || continuityIdentityID(t, reloadedAlias) != rootID {
		t.Fatalf("reloaded merged objective alias did not redirect: result=%#v err=%v", reloadedAlias, err)
	}
	for index, entry := range store.entries {
		if entry.Kind != continuityLedgerKindTaskIdentity || entry.EntryHash == "" {
			t.Fatalf("entry %d is not a hashed identity receipt: %#v", index, entry)
		}
		if index > 0 && entry.PreviousHash != store.entries[index-1].EntryHash {
			t.Fatalf("entry %d breaks hash chain", index)
		}
	}

	store.maxEntries = len(store.entries)
	beforeBytes := store.fileBytes
	if _, err := store.splitTaskIdentity(map[string]any{
		"source_task_identity_id": rootID, "objective": "capacity rejection", "actor": "operator", "reason": "test capacity",
	}); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("expected capacity rejection, got %v", err)
	}
	if len(store.entries) != 3 || store.fileBytes != beforeBytes {
		t.Fatalf("capacity rejection mutated ledger")
	}
}

func TestContinuityLedgerEnforcesSingleWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "continuity.ndjson")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH", path)
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC", "false")
	first, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("start first writer: %v", err)
	}
	defer first.close()
	second, err := newContinuityStoreFromEnv()
	if second != nil {
		second.close()
	}
	if !errors.Is(err, errContinuityLedgerLocked) {
		t.Fatalf("second writer was not rejected by the ledger lock: %v", err)
	}
	first.close()
	third, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("released writer lock was not reusable: %v", err)
	}
	third.close()
}

func TestContinuityIdempotencySurvivesAmbiguousPostPersistFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "continuity.ndjson")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH", path)
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC", "false")
	store, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	failOnce := true
	store.afterPersistHook = func() error {
		if failOnce {
			failOnce = false
			return errors.New("simulated response ambiguity after durable append")
		}
		return nil
	}
	payload := map[string]any{
		"project": "contextlattice", "objective_id": "obj_ambiguous_retry", "objective": "Retry durable transition",
		"transition_type": "started", "actor": "codex", "idempotency_key": "ambiguous-objective-retry",
	}
	if _, err := store.recordObjectiveTransition(payload); err == nil || !continuityCommitUnknown(err) || !strings.Contains(err.Error(), "disabled until restart and verification") {
		t.Fatalf("expected ambiguous post-persist failure, got %v", err)
	}
	store.close()
	restarted, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("reopen after ambiguous append: %v", err)
	}
	defer restarted.close()
	replayed, err := restarted.recordObjectiveTransition(payload)
	if err != nil || !replayed.idempotentReplay {
		t.Fatalf("durably persisted retry was not idempotent: transition=%#v err=%v", replayed, err)
	}
	if len(restarted.entries) != 1 || len(restarted.objectiveTransitions) != 1 {
		t.Fatalf("ambiguous retry duplicated ledger state: %#v", restarted.snapshot())
	}
	restarted.afterPersistHook = func() error { return errors.New("simulated decision response ambiguity") }
	decisionPayload := map[string]any{
		"project": "contextlattice", "objective_id": "obj_ambiguous_retry", "idempotency_key": "ambiguous-decision-retry",
		"before_decision": "Permit retry duplication", "after_decision": "Require idempotent retry",
		"confidence_before": 0.3, "confidence_after": 0.9, "trigger_evidence": []any{"eval:ambiguous-retry"},
		"actor": "codex", "rationale": "Durable append must be safe to retry.", "reason_code": "ambiguous_retry",
	}
	if _, _, err := restarted.recordDecisionChange(decisionPayload); err == nil || !continuityCommitUnknown(err) || !strings.Contains(err.Error(), "disabled until restart and verification") {
		t.Fatalf("expected ambiguous decision post-persist failure, got %v", err)
	}
	restarted.close()
	final, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("reopen after ambiguous decision append: %v", err)
	}
	defer final.close()
	change, transition, err := final.recordDecisionChange(decisionPayload)
	if err != nil || !change.idempotentReplay || !transition.idempotentReplay {
		t.Fatalf("durably persisted decision retry was not idempotent: change=%#v transition=%#v err=%v", change, transition, err)
	}
	if len(final.entries) != 2 || len(final.decisionChanges) != 1 || len(final.objectiveTransitions) != 2 {
		t.Fatalf("ambiguous decision retry duplicated ledger state: %#v", final.snapshot())
	}
}

func TestTaskIdentitySplitRetrySurvivesAmbiguousResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "continuity.ndjson")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH", path)
	t.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC", "false")
	store, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "root",
		"objective": "Ship durable continuity",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"source_task_identity_id": continuityIdentityID(t, root), "task_id": "independent-child",
		"objective": "Ship independent child", "actor": "operator", "reason": "verified independent scope",
		"idempotency_key": "split-ambiguous-retry",
	}
	store.afterPersistHook = func() error { return errors.New("simulated response ambiguity") }
	if _, err := store.splitTaskIdentity(payload); err == nil || !continuityCommitUnknown(err) || !strings.Contains(err.Error(), "disabled until restart and verification") {
		t.Fatalf("expected ambiguous split response, got %v", err)
	}
	store.close()

	restarted, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatalf("restart after durable split: %v", err)
	}
	defer restarted.close()
	replayed, err := restarted.splitTaskIdentity(payload)
	if err != nil || !anyToBool(replayed["idempotent_replay"]) || anyToBool(replayed["recorded"]) {
		t.Fatalf("split retry was not idempotent: result=%#v err=%v", replayed, err)
	}
	if len(restarted.entries) != 2 || len(restarted.taskIdentities) != 2 {
		t.Fatalf("split retry duplicated durable state: %#v", restarted.snapshot())
	}
	conflict := cloneJSONMap(payload)
	conflict["reason"] = "conflicting retry content"
	if _, err := restarted.splitTaskIdentity(conflict); err == nil || !strings.Contains(err.Error(), "idempotency_key") {
		t.Fatalf("conflicting split retry was accepted: %v", err)
	}
}

func TestTaskIdentityChainedMergesPreserveAllObjectiveAliases(t *testing.T) {
	store := newTestContinuityStore(t)
	create := func(taskID string, objective string) string {
		t.Helper()
		result, err := store.reconcile(map[string]any{
			"project": "contextlattice", "repo": "contextlattice", "task_id": taskID, "objective": objective,
		}, true)
		if err != nil {
			t.Fatalf("create %s: %v", taskID, err)
		}
		return continuityIdentityID(t, result)
	}
	sourceID := create("task-source", "Original source objective")
	middleID := create("task-middle", "Intermediate objective")
	targetID := create("task-target", "Canonical target objective")
	merge := func(target string, source string, key string) {
		t.Helper()
		payload := map[string]any{
			"target_task_identity_id": target, "source_task_identity_ids": []any{source},
			"actor": "operator", "reason": "verified common deliverable", "idempotency_key": key,
		}
		if _, err := store.mergeTaskIdentities(payload); err != nil {
			t.Fatalf("merge %s into %s: %v", source, target, err)
		}
		replayed, err := store.mergeTaskIdentities(payload)
		if err != nil || !anyToBool(replayed["idempotent_replay"]) {
			t.Fatalf("merge retry was not idempotent: result=%#v err=%v", replayed, err)
		}
	}
	merge(middleID, sourceID, "merge-source-middle")
	merge(targetID, middleID, "merge-middle-target")

	assertAliases := func(candidate *continuityStore) {
		t.Helper()
		for _, objective := range []string{"Original source objective", "Intermediate objective", "Canonical target objective"} {
			result, err := candidate.reconcile(map[string]any{
				"project": "contextlattice", "repo": "contextlattice", "objective": objective,
			}, false)
			if err != nil || continuityIdentityID(t, result) != targetID {
				t.Fatalf("objective alias %q did not converge on final target: result=%#v err=%v", objective, result, err)
			}
		}
	}
	assertAliases(store)
	reloaded := &continuityStore{enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries, entries: []continuityLedgerEntry{}}
	if err := reloaded.load(); err != nil {
		t.Fatalf("reload chained merge ledger: %v", err)
	}
	assertAliases(reloaded)
}

func TestContinuityRejectsSecretBearingRecordsBeforePersistence(t *testing.T) {
	store := newTestContinuityStore(t)
	identity, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "secret-proof", "objective": "Reject secrets",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	before := len(store.entries)
	for name, run := range map[string]func() error{
		"objective metadata": func() error {
			_, err := store.recordObjectiveTransition(map[string]any{
				"project": "contextlattice", "objective_id": "obj_secret", "objective": "Reject secret metadata",
				"transition_type": "created", "actor": "codex", "metadata": map[string]any{"api_key": "forbidden"},
			})
			return err
		},
		"decision verification": func() error {
			_, _, err := store.recordDecisionChange(map[string]any{
				"project": "contextlattice", "objective_id": "obj_secret", "before_decision": "A", "after_decision": "B",
				"confidence_before": 0.2, "confidence_after": 0.8, "trigger_evidence": []any{"eval:secret"},
				"actor": "codex", "rationale": "Secret rejection proof.", "reason_code": "secret_proof",
				"verification": map[string]any{"access_token": "forbidden"},
			})
			return err
		},
		"identity split": func() error {
			_, err := store.splitTaskIdentity(map[string]any{
				"source_task_identity_id": continuityIdentityID(t, identity), "objective": "Secret split",
				"actor": "operator", "reason": "reject", "metadata": map[string]any{"password": "forbidden"},
			})
			return err
		},
	} {
		if err := run(); err == nil || !strings.Contains(err.Error(), "secret-bearing") {
			t.Fatalf("%s did not reject secret-bearing input: %v", name, err)
		}
	}
	if len(store.entries) != before {
		t.Fatalf("secret rejection mutated ledger: before=%d after=%d", before, len(store.entries))
	}
}

func TestContinuityLedgerPersistenceFailureDisablesFurtherWrites(t *testing.T) {
	store := newTestContinuityStore(t)
	store.path = t.TempDir()

	_, err := store.reconcile(map[string]any{
		"project": "contextlattice", "objective": "prove fail closed persistence",
	}, true)
	if err == nil || !strings.Contains(err.Error(), "disabled until restart and verification") {
		t.Fatalf("expected fail-closed persistence error, got %v", err)
	}
	status := store.snapshot()
	if anyToBool(status["enabled"]) || anyToBool(status["ready"]) {
		t.Fatalf("persistence failure left ledger enabled: %#v", status)
	}
	if !strings.Contains(anyToString(status["last_error"]), "open continuity ledger for append") {
		t.Fatalf("missing actionable persistence error: %#v", status)
	}
}

func TestContinuityMergedDerivedIdentityRedirectsAndSplitAliasesCannotCollide(t *testing.T) {
	store := newTestContinuityStore(t)
	target, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "target-task",
		"objective": "Ship continuity identity",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "source-task",
		"objective": "Audit storage boundary",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	targetID := continuityIdentityID(t, target)
	sourceID := continuityIdentityID(t, source)
	if _, err := store.mergeTaskIdentities(map[string]any{
		"target_task_identity_id": targetID, "source_task_identity_ids": []any{sourceID},
		"actor": "operator", "reason": "verified shared deliverable",
	}); err != nil {
		t.Fatal(err)
	}
	redirected, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "objective": "Audit storage boundary",
	}, true)
	if err != nil || anyToString(redirected["match_mode"]) != "exact_objective" || continuityIdentityID(t, redirected) != targetID {
		t.Fatalf("merged deterministic identity did not redirect: result=%#v err=%v", redirected, err)
	}
	reloaded := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries,
		entries: []continuityLedgerEntry{}, taskIdentities: map[string]taskIdentityRecord{}, taskAliases: map[string]string{},
	}
	if err := reloaded.load(); err != nil {
		t.Fatalf("reload merged source objective: %v", err)
	}
	reloadedRedirect, err := reloaded.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "objective": "Audit storage boundary",
	}, false)
	if err != nil || anyToString(reloadedRedirect["match_mode"]) != "exact_objective" || continuityIdentityID(t, reloadedRedirect) != targetID {
		t.Fatalf("replayed merge lost source objective alias: result=%#v err=%v", reloadedRedirect, err)
	}

	beforeEntries := len(store.entries)
	if _, err := store.splitTaskIdentity(map[string]any{
		"source_task_identity_id": targetID, "task_id": "target-task", "objective": "Independent split",
		"actor": "operator", "reason": "test alias collision",
	}); err == nil || !strings.Contains(err.Error(), "already belongs") {
		t.Fatalf("expected split alias collision rejection, got %v", err)
	}
	if len(store.entries) != beforeEntries {
		t.Fatalf("rejected split alias collision mutated ledger")
	}
}

func TestContinuityLedgerReloadAndTamperFailClosed(t *testing.T) {
	store := newTestContinuityStore(t)
	result, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "reload",
		"objective": "Reload immutable continuity", "agent_id": "codex_test",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	identityID := continuityIdentityID(t, result)
	reloaded := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries,
		entries: []continuityLedgerEntry{}, taskIdentities: map[string]taskIdentityRecord{}, taskAliases: map[string]string{},
	}
	if err := reloaded.load(); err != nil {
		t.Fatalf("reload continuity ledger: %v", err)
	}
	if _, ok := reloaded.taskIdentities[identityID]; !ok || reloaded.lastHash != store.lastHash {
		t.Fatalf("reloaded state mismatch: %#v", reloaded.snapshot())
	}
	reloadedMatch, err := reloaded.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "objective": "Reload immutable continuity",
	}, false)
	if err != nil || anyToString(reloadedMatch["match_mode"]) != "exact_objective" || continuityIdentityID(t, reloadedMatch) != identityID {
		t.Fatalf("reloaded objective index mismatch: result=%#v err=%v", reloadedMatch, err)
	}
	if info, err := os.Stat(store.path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("continuity ledger must be owner-only: info=%v err=%v", info, err)
	}

	raw, err := os.ReadFile(store.path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, append(append([]byte{}, raw...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	blankLine := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries,
		entries: []continuityLedgerEntry{}, taskIdentities: map[string]taskIdentityRecord{}, taskAliases: map[string]string{},
	}
	if err := blankLine.load(); err == nil || !strings.Contains(err.Error(), "empty committed line") {
		t.Fatalf("blank committed ledger line did not fail closed: %v", err)
	}
	if err := os.WriteFile(store.path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var entry map[string]any
	if err := json.Unmarshal(raw[:len(raw)-1], &entry); err != nil {
		t.Fatal(err)
	}
	entry["entry_hash"] = strings.Repeat("0", 64)
	tampered, _ := json.Marshal(entry)
	if err := os.WriteFile(store.path, append(tampered, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	broken := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries,
		entries: []continuityLedgerEntry{}, taskIdentities: map[string]taskIdentityRecord{}, taskAliases: map[string]string{},
	}
	if err := broken.load(); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered ledger did not fail closed: %v", err)
	}
}

func TestContinuityTailRecoveryAndLosslessCompaction(t *testing.T) {
	store := newTestContinuityStore(t)
	created, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "tail-recovery",
		"objective": "Recover only an uncommitted ledger tail",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	identityID := continuityIdentityID(t, created)
	originalHash := store.lastHash
	file, err := os.OpenFile(store.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	partial := []byte("{\"schema_id\":\"continuity_ledger_entry.v1\",\"sequence\":2")
	if _, err := file.Write(partial); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries, fsync: false,
		entries: []continuityLedgerEntry{},
	}
	if err := reloaded.load(); err != nil {
		t.Fatalf("recover torn continuity tail: %v", err)
	}
	if reloaded.tailRecoveryCount != 1 || reloaded.tailRecoveryBytes != int64(len(partial)) {
		t.Fatalf("torn-tail recovery accounting mismatch: %#v", reloaded.snapshot())
	}
	if reloaded.lastHash != originalHash || len(reloaded.entries) != 1 {
		t.Fatalf("torn-tail recovery changed committed entries: %#v", reloaded.snapshot())
	}
	if _, ok := reloaded.taskIdentities[identityID]; !ok {
		t.Fatalf("torn-tail recovery lost committed task identity")
	}
	beforeCompact := reloaded.fileBytes
	compaction, err := reloaded.compact()
	if err != nil {
		t.Fatalf("lossless continuity compaction: %v", err)
	}
	if !anyToBool(compaction["lossless"]) || reloaded.fileBytes > beforeCompact || reloaded.lastHash != originalHash {
		t.Fatalf("compaction grew or changed the verified ledger: %#v", compaction)
	}
	if reloaded.compactionCount != 1 {
		t.Fatalf("compaction count mismatch: %#v", reloaded.snapshot())
	}
	if _, err := reloaded.recordObjectiveTransition(map[string]any{
		"project": "contextlattice", "objective_id": "obj_tail_recovery", "objective": "Recovery remains appendable",
		"transition_type": "created", "actor": "codex",
	}); err != nil {
		t.Fatalf("append after tail recovery and compaction: %v", err)
	}
	verified := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries, fsync: false,
		entries: []continuityLedgerEntry{},
	}
	if err := verified.load(); err != nil || len(verified.entries) != 2 || len(verified.objectiveTransitions) != 1 {
		t.Fatalf("post-compaction readback failed: entries=%d transitions=%d err=%v", len(verified.entries), len(verified.objectiveTransitions), err)
	}
}

func TestObjectiveGraphAsOfAndDecisionProvenance(t *testing.T) {
	store := newTestContinuityStore(t)
	identity, err := store.reconcile(map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "task-t1", "objective": "Ship T1 task identity",
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	taskIdentityID := continuityIdentityID(t, identity)
	now := time.Now().UTC()
	eventTimes := []string{
		now.Add(-4 * time.Hour).Format(time.RFC3339Nano),
		now.Add(-3 * time.Hour).Format(time.RFC3339Nano),
		now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		now.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	payloads := []map[string]any{
		{"project": "contextlattice", "objective_id": "obj_t1", "objective": "Ship T1", "transition_type": "created", "actor": "codex", "occurred_at": eventTimes[0]},
		{"project": "contextlattice", "objective_id": "obj_t1", "objective": "Ship T1", "transition_type": "started", "actor": "codex", "occurred_at": eventTimes[1], "task_identity_id": taskIdentityID, "execution_lane_id": "lane_main"},
		{"project": "contextlattice", "objective_id": "obj_t1", "objective": "Ship T1", "transition_type": "blocked", "actor": "codex", "occurred_at": eventTimes[2], "summary": "holdout failed"},
	}
	recordedTransitions := make([]objectiveTransition, 0, len(payloads))
	for index, payload := range payloads {
		transition, err := store.recordObjectiveTransition(payload)
		if err != nil {
			t.Fatalf("record transition: %v", err)
		}
		recordedTransitions = append(recordedTransitions, transition)
		if index < len(payloads)-1 {
			time.Sleep(time.Millisecond)
		}
	}
	asOf, err := time.Parse(time.RFC3339Nano, recordedTransitions[1].RecordedAt)
	if err != nil {
		t.Fatal(err)
	}
	graph := store.objectiveGraph("contextlattice", "obj_t1", asOf, true, 100)
	nodes, ok := graph["nodes"].([]objectiveGraphNode)
	if !ok || len(nodes) != 1 || nodes[0].Status != "active" || anyToInt(graph["transition_count"], 0) != 2 {
		t.Fatalf("as-of graph reconstructed wrong state: %#v", graph)
	}

	change, transition, err := store.recordDecisionChange(map[string]any{
		"project": "contextlattice", "objective_id": "obj_t1", "objective": "Ship T1",
		"before_decision": "Auto-merge semantic candidates", "after_decision": "Require explicit confirmation",
		"confidence_before": 0.4, "confidence_after": 0.9,
		"trigger_evidence": []any{map[string]any{"ref_id": "eval:ambiguity-holdout", "kind": "eval"}},
		"alternatives":     []any{"Raise the threshold only"}, "actor": "codex", "rationale": "Ambiguous candidates caused unsafe reuse.",
		"reason_code": "ambiguity_holdout_failed", "verification": map[string]any{"status": "verified", "method": "holdout", "checker": "go_test"},
		"occurred_at": eventTimes[3], "task_identity_id": taskIdentityID, "execution_lane_id": "lane_main",
	})
	if err != nil {
		t.Fatalf("record decision change: %v", err)
	}
	if change.ConfidenceDelta != 0.5 || transition.TransitionType != "decision_changed" || transition.DecisionChangeID != change.DecisionChangeID {
		t.Fatalf("decision provenance mismatch: change=%#v transition=%#v", change, transition)
	}
	latest := store.objectiveGraph("contextlattice", "obj_t1", time.Time{}, true, 100)
	latestNodes, nodesOK := latest["nodes"].([]objectiveGraphNode)
	latestEdges, edgesOK := latest["edges"].([]objectiveGraphEdge)
	if !nodesOK || !edgesOK || len(latestNodes) != 1 || latestNodes[0].Status != "blocked" || len(latestEdges) == 0 {
		t.Fatalf("decision transition missing from graph: %#v", latest)
	}
	beforeEntries := len(store.entries)
	if _, _, err := store.recordDecisionChange(map[string]any{
		"project": "contextlattice", "objective_id": "obj_t1", "before_decision": "A", "after_decision": "B",
		"confidence_before": 0.1, "confidence_after": 0.2, "trigger_evidence": []any{"ref"},
		"actor": "codex", "rationale": "bounded", "reason_code": "new_evidence", "chain_of_thought": "private trace",
	}); err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("hidden reasoning was not rejected: %v", err)
	}
	if len(store.entries) != beforeEntries {
		t.Fatalf("rejected decision change mutated ledger")
	}

	reloaded := &continuityStore{
		enabled: true, path: store.path, maxBytes: store.maxBytes, maxEntries: store.maxEntries,
		entries: []continuityLedgerEntry{}, taskIdentities: map[string]taskIdentityRecord{}, taskAliases: map[string]string{},
	}
	if err := reloaded.load(); err != nil {
		t.Fatalf("reload multi-entry continuity ledger: %v", err)
	}
	if len(reloaded.entries) != len(store.entries) || len(reloaded.objectiveTransitions) != 4 || len(reloaded.decisionChanges) != 1 {
		t.Fatalf("multi-entry replay mismatch: %#v", reloaded.snapshot())
	}
	if reloaded.lastPersistedAt == "" {
		t.Fatalf("multi-entry replay lost persistence timestamp")
	}
}

func TestObjectiveAndDecisionSameEffectiveTimeUseLedgerOrder(t *testing.T) {
	store := newTestContinuityStore(t)
	effectiveAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	for _, payload := range []map[string]any{
		{"project": "contextlattice", "objective_id": "obj_same_time", "objective": "Deterministic replay", "transition_id": "ot_z_first", "transition_type": "blocked", "actor": "codex", "occurred_at": effectiveAt},
		{"project": "contextlattice", "objective_id": "obj_same_time", "objective": "Deterministic replay", "transition_id": "ot_a_second", "transition_type": "completed", "actor": "codex", "occurred_at": effectiveAt},
	} {
		if _, err := store.recordObjectiveTransition(payload); err != nil {
			t.Fatal(err)
		}
	}
	graph := store.objectiveGraph("contextlattice", "obj_same_time", time.Time{}, true, 10)
	nodes, ok := graph["nodes"].([]objectiveGraphNode)
	if !ok || len(nodes) != 1 || nodes[0].Status != "completed" {
		t.Fatalf("same-effective-time objective replay ignored ledger order: %#v", graph)
	}

	decisionPayload := func(changeID string, before string, after string) map[string]any {
		return map[string]any{
			"project": "contextlattice", "objective_id": "obj_same_time", "decision_change_id": changeID,
			"before_decision": before, "after_decision": after, "confidence_before": 0.4, "confidence_after": 0.8,
			"trigger_evidence": []any{"eval:same-effective-time"}, "actor": "codex",
			"rationale": "Ledger order resolves equal effective timestamps.", "reason_code": "ledger_order", "occurred_at": effectiveAt,
		}
	}
	if _, _, err := store.recordDecisionChange(decisionPayload("dc_a_first", "A", "B")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.recordDecisionChange(decisionPayload("dc_z_second", "B", "C")); err != nil {
		t.Fatal(err)
	}
	changes := store.queryDecisionChanges("contextlattice", "obj_same_time", time.Time{}, 10)
	if len(changes) != 2 || changes[0].DecisionChangeID != "dc_z_second" {
		t.Fatalf("same-effective-time decision query ignored ledger order: %#v", changes)
	}
}

func TestObjectiveAndDecisionAsOfRequireKnowledgeAndEventTime(t *testing.T) {
	store := newTestContinuityStore(t)
	now := time.Now().UTC()
	backdated := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	historicalCutoff := now.Add(-time.Hour)
	future := now.Add(time.Hour).Format(time.RFC3339Nano)
	futureCutoff := now.Add(2 * time.Hour)

	late, err := store.recordObjectiveTransition(map[string]any{
		"project": "contextlattice", "objective_id": "obj_late", "objective": "Late arriving evidence",
		"transition_type": "created", "actor": "codex", "occurred_at": backdated,
	})
	if err != nil || late.RecordedAt == "" {
		t.Fatalf("record backdated transition: transition=%#v err=%v", late, err)
	}
	if graph := store.objectiveGraph("contextlattice", "obj_late", historicalCutoff, true, 100); anyToInt(graph["transition_count"], 0) != 0 {
		t.Fatalf("backdated late arrival appeared before knowledge time: %#v", graph)
	} else if anyToInt(graph["node_count"], -1) != 0 || len(graph["nodes"].([]objectiveGraphNode)) != 0 {
		t.Fatalf("historical explicit query fabricated a phantom objective node: %#v", graph)
	}
	if graph := store.objectiveGraph("contextlattice", "obj_late", time.Time{}, true, 100); anyToInt(graph["transition_count"], 0) != 1 {
		t.Fatalf("current graph omitted known backdated event: %#v", graph)
	}

	if _, err := store.recordObjectiveTransition(map[string]any{
		"project": "contextlattice", "objective_id": "obj_future", "objective": "Future effective event",
		"transition_type": "created", "actor": "codex", "occurred_at": future,
	}); err != nil {
		t.Fatal(err)
	}
	if graph := store.objectiveGraph("contextlattice", "obj_future", time.Time{}, true, 100); anyToInt(graph["transition_count"], 0) != 0 {
		t.Fatalf("default-now graph included future event: %#v", graph)
	}
	if graph := store.objectiveGraph("contextlattice", "obj_future", futureCutoff, true, 100); anyToInt(graph["transition_count"], 0) != 1 {
		t.Fatalf("future as-of graph omitted effective event: %#v", graph)
	}

	decisionPayload := func(objectiveID string, occurredAt string, changeID string) map[string]any {
		return map[string]any{
			"project": "contextlattice", "objective_id": objectiveID, "decision_change_id": changeID,
			"before_decision": "Use old evidence", "after_decision": "Use verified evidence",
			"confidence_before": 0.3, "confidence_after": 0.9,
			"trigger_evidence": []any{map[string]any{"ref_id": "eval:as-of", "kind": "eval"}},
			"actor":            "codex", "rationale": "The verified evidence changed the decision.",
			"reason_code": "verified_evidence", "verification": map[string]any{"status": "verified", "method": "go_test"},
			"occurred_at": occurredAt,
		}
	}
	lateDecision, _, err := store.recordDecisionChange(decisionPayload("obj_late_decision", backdated, "dc_late"))
	if err != nil || lateDecision.RecordedAt == "" {
		t.Fatalf("record backdated decision: decision=%#v err=%v", lateDecision, err)
	}
	if rows := store.queryDecisionChanges("contextlattice", "obj_late_decision", historicalCutoff, 10); len(rows) != 0 {
		t.Fatalf("backdated decision appeared before knowledge time: %#v", rows)
	}
	if rows := store.queryDecisionChanges("contextlattice", "obj_late_decision", time.Time{}, 10); len(rows) != 1 {
		t.Fatalf("current decision query omitted known backdated event: %#v", rows)
	}
	if _, _, err := store.recordDecisionChange(decisionPayload("obj_future_decision", future, "dc_future")); err != nil {
		t.Fatal(err)
	}
	if rows := store.queryDecisionChanges("contextlattice", "obj_future_decision", time.Time{}, 10); len(rows) != 0 {
		t.Fatalf("default-now decision query included future event: %#v", rows)
	}
}

func TestObjectiveGraphIsProjectScopedAndLinksOutcomesAndCheckpoints(t *testing.T) {
	store := newTestContinuityStore(t)
	for _, payload := range []map[string]any{
		{
			"project": "alpha", "objective_id": "obj_shared", "objective": "Alpha objective",
			"transition_id": "ot_alpha_shared", "transition_type": "started", "actor": "codex",
			"outcome_id": "out_alpha", "checkpoint_id": "checkpoint_alpha",
		},
		{
			"project": "beta", "objective_id": "obj_shared", "objective": "Beta objective",
			"transition_id": "ot_beta_shared", "transition_type": "blocked", "actor": "codex",
		},
	} {
		if _, err := store.recordObjectiveTransition(payload); err != nil {
			t.Fatalf("record project-scoped transition: %v", err)
		}
	}
	graph := store.objectiveGraph("alpha", "obj_shared", time.Time{}, true, 100)
	nodes := graph["nodes"].([]objectiveGraphNode)
	if len(nodes) != 1 || nodes[0].Project != "alpha" || nodes[0].Objective != "Alpha objective" || nodes[0].Status != "active" {
		t.Fatalf("same objective id fused across projects: %#v", graph)
	}
	if len(nodes[0].OutcomeIDs) != 1 || nodes[0].OutcomeIDs[0] != "out_alpha" ||
		len(nodes[0].CheckpointIDs) != 1 || nodes[0].CheckpointIDs[0] != "checkpoint_alpha" {
		t.Fatalf("objective node omitted outcome/checkpoint links: %#v", nodes[0])
	}
	edgeTypes := map[string]bool{}
	for _, edge := range graph["edges"].([]objectiveGraphEdge) {
		edgeTypes[edge.Type] = true
	}
	if !edgeTypes["outcome_link"] || !edgeTypes["checkpoint_link"] {
		t.Fatalf("objective graph omitted typed outcome/checkpoint edges: %#v", graph["edges"])
	}

	gateway := httptest.NewServer(buildNativeMux(&server{continuity: store}))
	defer gateway.Close()
	status, payload := getAgentSessionJSON(t, gateway.URL+"/memory/objectives/graph?objective_id=obj_shared")
	if status != http.StatusUnprocessableEntity || anyToString(payload["error"]) != "invalid_objective_graph_query" {
		t.Fatalf("unscoped graph query was accepted: status=%d payload=%#v", status, payload)
	}
}

func TestObjectiveAndDecisionChronologyParsesFractionalRFC3339(t *testing.T) {
	store := newTestContinuityStore(t)
	later := "2026-07-14T12:00:00.1Z"
	earlier := "2026-07-14T12:00:00Z"
	for _, payload := range []map[string]any{
		{"project": "contextlattice", "objective_id": "obj_fractional", "transition_id": "ot_fractional_later", "transition_type": "completed", "actor": "codex", "occurred_at": later},
		{"project": "contextlattice", "objective_id": "obj_fractional", "transition_id": "ot_whole_earlier", "transition_type": "blocked", "actor": "codex", "occurred_at": earlier},
	} {
		if _, err := store.recordObjectiveTransition(payload); err != nil {
			t.Fatal(err)
		}
	}
	graph := store.objectiveGraph("contextlattice", "obj_fractional", time.Now().UTC().Add(24*time.Hour), true, 10)
	nodes := graph["nodes"].([]objectiveGraphNode)
	if len(nodes) != 1 || nodes[0].Status != "completed" {
		t.Fatalf("fractional RFC3339 objective chronology used lexical order: %#v", graph)
	}
	decisionPayload := func(id string, occurredAt string, before string, after string) map[string]any {
		return map[string]any{
			"project": "contextlattice", "objective_id": "obj_fractional", "decision_change_id": id,
			"before_decision": before, "after_decision": after, "confidence_before": 0.2, "confidence_after": 0.8,
			"trigger_evidence": []any{"eval:fractional-time"}, "actor": "codex", "rationale": "Chronology proof.",
			"reason_code": "chronology_proof", "occurred_at": occurredAt,
		}
	}
	if _, _, err := store.recordDecisionChange(decisionPayload("dc_fractional_later", later, "A", "B")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.recordDecisionChange(decisionPayload("dc_whole_earlier", earlier, "B", "C")); err != nil {
		t.Fatal(err)
	}
	changes := store.queryDecisionChanges("contextlattice", "obj_fractional", time.Now().UTC().Add(24*time.Hour), 10)
	if len(changes) != 2 || changes[0].DecisionChangeID != "dc_fractional_later" {
		t.Fatalf("fractional RFC3339 decision chronology used lexical order: %#v", changes)
	}
}

func TestDecisionChangeIDsAreUniqueAndAtomic(t *testing.T) {
	store := newTestContinuityStore(t)
	payload := map[string]any{
		"project": "contextlattice", "objective_id": "obj_decision_unique", "objective": "Keep decision provenance immutable",
		"decision_change_id": "dc_unique", "before_decision": "Permit ambiguous reuse", "after_decision": "Require explicit confirmation",
		"confidence_before": 0.3, "confidence_after": 0.95,
		"trigger_evidence": []any{map[string]any{"ref_id": "eval:semantic-ambiguity", "kind": "eval"}},
		"actor":            "codex", "rationale": "The ambiguity holdout requires an explicit operator decision.",
		"reason_code": "ambiguity_holdout", "verification": map[string]any{"status": "verified", "method": "go_test"},
	}
	if _, _, err := store.recordDecisionChange(payload); err != nil {
		t.Fatalf("record initial decision change: %v", err)
	}
	if len(store.entries) != 1 || store.entries[0].Kind != continuityLedgerKindDecisionBundle ||
		len(store.decisionChanges) != 1 || len(store.objectiveTransitions) != 1 {
		t.Fatalf("decision change was not persisted as one atomic bundle: %#v", store.snapshot())
	}
	beforeEntries := len(store.entries)
	replayed, _, err := store.recordDecisionChange(payload)
	if err != nil || !replayed.idempotentReplay {
		t.Fatalf("expected idempotent decision replay, decision=%#v err=%v", replayed, err)
	}
	if len(store.entries) != beforeEntries {
		t.Fatalf("idempotent decision replay mutated ledger")
	}
	conflict := cloneJSONMap(payload)
	conflict["after_decision"] = "Silently merge ambiguous candidates"
	if _, _, err := store.recordDecisionChange(conflict); err == nil || !strings.Contains(err.Error(), "idempotency_key") {
		t.Fatalf("expected conflicting decision replay rejection, got %v", err)
	}
}

func TestDecisionChangeSeparatesMeaningChangesFromProvenanceRestatements(t *testing.T) {
	store := newTestContinuityStore(t)
	base := map[string]any{
		"project": "contextlattice", "objective_id": "obj_decision_meaning",
		"confidence_before": 0.8, "confidence_after": 0.8,
		"trigger_evidence": []any{"eval:decision-meaning"}, "actor": "codex",
		"rationale": "Only material conclusion or confidence changes belong here.", "reason_code": "meaning_holdout",
	}
	for name, pair := range map[string][2]string{
		"wording-only":          {"Require explicit confirmation", "require explicit confirmation."},
		"evidence-only-restate": {"Require explicit confirmation", "Require explicit confirmation"},
	} {
		payload := cloneJSONMap(base)
		payload["decision_change_id"] = "dc_" + strings.ReplaceAll(name, "-", "_")
		payload["before_decision"] = pair[0]
		payload["after_decision"] = pair[1]
		if _, _, err := store.recordDecisionChange(payload); err == nil || !strings.Contains(err.Error(), "ordinary provenance") {
			t.Fatalf("%s restatement was recorded as a decision change: %v", name, err)
		}
	}
	confidenceOnly := cloneJSONMap(base)
	confidenceOnly["decision_change_id"] = "dc_confidence_only"
	confidenceOnly["before_decision"] = "Require explicit confirmation"
	confidenceOnly["after_decision"] = "Require explicit confirmation"
	confidenceOnly["confidence_after"] = 0.95
	change, _, err := store.recordDecisionChange(confidenceOnly)
	if err != nil {
		t.Fatalf("confidence-only material revision was rejected: %v", err)
	}
	if change.Before.DecisionID != change.After.DecisionID || change.ConfidenceDelta != 0.15 {
		t.Fatalf("confidence revision did not retain stable decision identity: %#v", change)
	}
}

func TestDecisionChangeQueryPaginatesWithoutOverlap(t *testing.T) {
	store := newTestContinuityStore(t)
	baseTime := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < 7; index++ {
		if _, _, err := store.recordDecisionChange(map[string]any{
			"project": "contextlattice", "objective_id": "obj_paginated", "decision_change_id": fmt.Sprintf("dc_page_%02d", index),
			"before_decision": fmt.Sprintf("Decision %d", index), "after_decision": fmt.Sprintf("Decision %d", index+1),
			"confidence_before": 0.2, "confidence_after": 0.8,
			"trigger_evidence": []any{"eval:pagination"}, "actor": "codex", "rationale": "Pagination proof.",
			"reason_code": "pagination_proof", "occurred_at": baseTime.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("seed decision %d: %v", index, err)
		}
	}
	first, err := store.queryDecisionChangesPage("contextlattice", "obj_paginated", time.Time{}, 3, "")
	if err != nil || len(first.Rows) != 3 || first.MatchedCount != 7 || !first.MatchedCountExact || first.NextCursor == "" {
		t.Fatalf("invalid first page: result=%#v err=%v", first, err)
	}
	second, err := store.queryDecisionChangesPage("contextlattice", "obj_paginated", time.Time{}, 3, first.NextCursor)
	if err != nil || len(second.Rows) != 3 || second.MatchedCount != 4 || second.NextCursor == "" {
		t.Fatalf("invalid second page: result=%#v err=%v", second, err)
	}
	third, err := store.queryDecisionChangesPage("contextlattice", "obj_paginated", time.Time{}, 3, second.NextCursor)
	if err != nil || len(third.Rows) != 1 || third.MatchedCount != 1 || third.NextCursor != "" {
		t.Fatalf("invalid final page: result=%#v err=%v", third, err)
	}
	seen := map[string]bool{}
	for _, page := range [][]decisionChange{first.Rows, second.Rows, third.Rows} {
		for _, change := range page {
			if seen[change.DecisionChangeID] {
				t.Fatalf("decision appeared on multiple cursor pages: %s", change.DecisionChangeID)
			}
			seen[change.DecisionChangeID] = true
		}
	}
	if len(seen) != 7 {
		t.Fatalf("pagination omitted decisions: %#v", seen)
	}
	if _, err := store.queryDecisionChangesPage("contextlattice", "obj_paginated", time.Time{}, 3, "not-a-cursor"); err == nil {
		t.Fatal("invalid cursor was accepted")
	}
	if _, err := store.queryDecisionChangesPage("other-project", "obj_paginated", time.Time{}, 3, first.NextCursor); err == nil {
		t.Fatal("cursor was accepted across project scope")
	}
	if _, err := store.queryDecisionChangesPage("contextlattice", "other-objective", time.Time{}, 3, first.NextCursor); err == nil {
		t.Fatal("cursor was accepted across objective scope")
	}
	if _, err := store.queryDecisionChangesPage("contextlattice", "obj_paginated", first.AsOf.Add(-time.Second), 3, first.NextCursor); err == nil {
		t.Fatal("cursor was accepted with a different as_of instant")
	}
	gateway := httptest.NewServer(buildNativeMux(&server{continuity: store}))
	defer gateway.Close()
	status, response := getAgentSessionJSON(t, gateway.URL+"/memory/decision-changes?project=contextlattice&objective_id=obj_paginated&limit=3")
	if status != http.StatusOK || anyToInt(response["change_count"], -1) != 3 ||
		anyToInt(response["total_change_count"], -1) != 7 || anyToInt(response["omitted_count"], -1) != 4 ||
		!anyToBool(response["total_count_exact"]) || anyToBool(response["complete"]) || !anyToBool(response["query_truncated"]) ||
		anyToString(response["next_cursor"]) == "" {
		t.Fatalf("HTTP decision pagination metadata is not truthful: status=%d payload=%#v", status, response)
	}
	status, continued := getAgentSessionJSON(t, gateway.URL+"/memory/decision-changes?project=contextlattice&objective_id=obj_paginated&limit=3&cursor="+anyToString(response["next_cursor"]))
	if status != http.StatusOK || anyToInt(continued["change_count"], -1) != 3 || anyToString(continued["as_of"]) != anyToString(response["as_of"]) {
		t.Fatalf("HTTP cursor did not preserve its frozen default as_of: status=%d payload=%#v", status, continued)
	}
}

func TestDecisionChangeQueryBoundaryReconcilesReturnedCounts(t *testing.T) {
	store := newTestContinuityStore(t)
	store.maxBytes = 8 * 1024 * 1024
	largeEvidence := make([]any, 0, 32)
	for index := 0; index < 32; index++ {
		largeEvidence = append(largeEvidence, map[string]any{
			"ref_id": fmt.Sprintf("eval:boundary:%02d:%s", index, strings.Repeat("x", 760)), "kind": "eval",
		})
	}
	baseTime := time.Now().UTC().Add(-time.Hour)
	for index := 0; index < 24; index++ {
		if _, _, err := store.recordDecisionChange(map[string]any{
			"project": "contextlattice", "objective_id": "obj_decision_boundary", "decision_change_id": fmt.Sprintf("dc_boundary_%02d", index),
			"before_decision": fmt.Sprintf("Boundary decision %d", index), "after_decision": fmt.Sprintf("Boundary decision %d", index+1),
			"confidence_before": 0.2, "confidence_after": 0.8, "trigger_evidence": largeEvidence,
			"actor": "codex", "rationale": strings.Repeat("bounded rationale ", 100), "reason_code": "boundary_proof",
			"occurred_at": baseTime.Add(time.Duration(index) * time.Minute).Format(time.RFC3339Nano),
		}); err != nil {
			t.Fatalf("seed boundary decision %d: %v", index, err)
		}
	}
	gateway := httptest.NewServer(buildNativeMux(&server{continuity: store}))
	defer gateway.Close()
	resp, err := http.Get(gateway.URL + "/memory/decision-changes?project=contextlattice&objective_id=obj_decision_boundary&limit=500")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decision boundary status=%d body=%s", resp.StatusCode, string(raw))
	}
	if len(strings.TrimSpace(string(raw))) > 400000 {
		t.Fatalf("decision query escaped contract byte ceiling: %d", len(raw))
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	changes := contextPackAnyList(payload["changes"])
	if anyToInt(payload["change_count"], -1) != len(changes) || anyToInt(payload["total_change_count"], -1) != 24 ||
		anyToInt(payload["omitted_count"], -1) < 24-len(changes) || !anyToBool(payload["boundary_compacted"]) ||
		anyToBool(payload["complete"]) || !anyToBool(payload["query_truncated"]) {
		t.Fatalf("decision boundary metadata is stale: %#v", payload)
	}
	formatContract := anyMap(payload["format_contract"])
	if !anyToBool(formatContract["contract_valid"]) || !anyToBool(formatContract["truncated"]) {
		t.Fatalf("decision boundary format contract is not valid and explicit: %#v", formatContract)
	}
	if len(changes) > 0 && len(changes) < 24 {
		wantCursor := anyToString(anyMap(changes[len(changes)-1])["page_cursor"])
		if wantCursor == "" || anyToString(payload["next_cursor"]) != wantCursor {
			t.Fatalf("decision boundary cursor does not resume after last returned row: %#v", payload)
		}
	}
}

func TestHistoryWorkIsHardBounded(t *testing.T) {
	store := newTestContinuityStore(t)
	const rows = 100001
	occurredAt := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
	store.mu.Lock()
	for index := 0; index < rows; index++ {
		sequence := uint64(index + 1)
		store.applyObjectiveTransitionValidatedLocked(objectiveTransition{
			SchemaID: objectiveTransitionContractID, TransitionID: fmt.Sprintf("ot_bound_%06d", index),
			ObjectiveID: "obj_hard_bound", Project: "contextlattice", Objective: "Bound history work",
			TransitionType: "progressed", ToStatus: "active", Actor: "test", Summary: "bounded",
			IdempotencyKey: fmt.Sprintf("objective-bound-%06d", index), OccurredAt: occurredAt, RecordedAt: occurredAt,
			ledgerSequence: sequence,
		})
		decisionObjectiveID := "obj_hard_bound"
		if index == 0 {
			decisionObjectiveID = "obj_sparse_target"
		}
		store.applyDecisionChangeValidatedLocked(decisionChange{
			SchemaID: decisionChangeContractID, DecisionChangeID: fmt.Sprintf("dc_bound_%06d", index),
			Project: "contextlattice", ObjectiveID: decisionObjectiveID, IdempotencyKey: fmt.Sprintf("decision-bound-%06d", index),
			Before: decisionRef{DecisionID: "decision_old", Summary: "old"}, After: decisionRef{DecisionID: "decision_new", Summary: "new"},
			OccurredAt: occurredAt, RecordedAt: occurredAt, ledgerSequence: sequence,
		})
	}
	store.mu.Unlock()
	graph := store.objectiveGraph("contextlattice", "obj_hard_bound", time.Time{}, true, 1)
	if anyToInt(graph["replay_inspection_count"], -1) != objectiveGraphMaxReplayInspections ||
		!anyToBool(graph["replay_truncated"]) || anyToBool(graph["transition_count_exact"]) || anyToBool(graph["complete"]) {
		t.Fatalf("objective replay work escaped hard bound: %#v", graph)
	}
	query, err := store.queryDecisionChangesPage("contextlattice", "obj_hard_bound", time.Time{}, 1, "")
	if err != nil || query.InspectionCount != decisionChangeMaxQueryInspections || query.MatchedCountExact || len(query.Rows) != 1 || query.NextCursor == "" {
		t.Fatalf("decision query work escaped hard bound: result=%#v err=%v", query, err)
	}
	sparseFirst, err := store.queryDecisionChangesPage("contextlattice", "obj_sparse_target", time.Time{}, 1, "")
	if err != nil || sparseFirst.InspectionCount != decisionChangeMaxQueryInspections || sparseFirst.MatchedCountExact || len(sparseFirst.Rows) != 0 || sparseFirst.NextCursor == "" {
		t.Fatalf("sparse decision query did not expose bounded continuation: result=%#v err=%v", sparseFirst, err)
	}
	sparseSecond, err := store.queryDecisionChangesPage("contextlattice", "obj_sparse_target", time.Time{}, 1, sparseFirst.NextCursor)
	if err != nil || !sparseSecond.MatchedCountExact || len(sparseSecond.Rows) != 1 || sparseSecond.Rows[0].ObjectiveID != "obj_sparse_target" {
		t.Fatalf("sparse continuation could not reach older decision: result=%#v err=%v", sparseSecond, err)
	}
}

func TestObjectiveTransitionIDsStatusesAndGraphTraversalAreBounded(t *testing.T) {
	store := newTestContinuityStore(t)
	base := map[string]any{
		"project": "contextlattice", "objective_id": "obj_duplicate", "objective": "Reject duplicate transitions",
		"transition_id": "ot_duplicate", "transition_type": "created", "actor": "codex",
	}
	if _, err := store.recordObjectiveTransition(base); err != nil {
		t.Fatal(err)
	}
	beforeEntries := len(store.entries)
	replayed, err := store.recordObjectiveTransition(base)
	if err != nil || !replayed.idempotentReplay {
		t.Fatalf("expected idempotent transition replay, transition=%#v err=%v", replayed, err)
	}
	conflict := cloneJSONMap(base)
	conflict["summary"] = "conflicting retry"
	if _, err := store.recordObjectiveTransition(conflict); err == nil || !strings.Contains(err.Error(), "idempotency_key") {
		t.Fatalf("expected conflicting transition replay rejection, got %v", err)
	}
	for field, value := range map[string]string{"from_status": "invented", "to_status": "unknown"} {
		payload := map[string]any{
			"project": "contextlattice", "objective_id": "obj_status", "objective": "Validate status",
			"transition_type": "progressed", "actor": "codex", field: value,
		}
		if _, err := store.recordObjectiveTransition(payload); err == nil || !strings.Contains(err.Error(), field) {
			t.Fatalf("expected %s rejection, got %v", field, err)
		}
	}
	if len(store.entries) != beforeEntries {
		t.Fatalf("rejected transition mutated ledger")
	}

	for index := 0; index < 100; index++ {
		payload := map[string]any{
			"project": "contextlattice", "objective_id": fmt.Sprintf("obj_chain_%03d", index),
			"objective": fmt.Sprintf("Chain objective %03d", index), "transition_type": "created", "actor": "codex",
		}
		if index < 99 {
			payload["depends_on"] = []any{fmt.Sprintf("obj_chain_%03d", index+1)}
		}
		if _, err := store.recordObjectiveTransition(payload); err != nil {
			t.Fatalf("record chain transition %d: %v", index, err)
		}
	}
	if _, err := store.recordObjectiveTransition(map[string]any{
		"project": "contextlattice", "objective_id": "obj_unrelated", "objective": "Unrelated objective",
		"transition_type": "created", "actor": "codex",
	}); err != nil {
		t.Fatal(err)
	}
	graph := store.objectiveGraph("contextlattice", "obj_chain_000", time.Time{}, false, 500)
	if anyToInt(graph["node_count"], 0) != 100 || anyToInt(graph["transition_count"], 0) != 100 {
		t.Fatalf("linear connected traversal included wrong component: %#v", graph)
	}
	if !anyToBool(graph["complete"]) || anyToBool(graph["graph_truncated"]) {
		t.Fatalf("complete connected traversal reported truncation: %#v", graph)
	}
	bounded := store.objectiveGraph("contextlattice", "obj_chain_000", time.Time{}, true, 25)
	if anyToInt(bounded["node_count"], 0) != 25 || !anyToBool(bounded["traversal_truncated"]) ||
		!anyToBool(bounded["graph_truncated"]) || anyToBool(bounded["complete"]) {
		t.Fatalf("bounded connected traversal did not expose truncation: %#v", bounded)
	}
}

func TestObjectiveGraphCapsEveryReturnedList(t *testing.T) {
	store := newTestContinuityStore(t)
	for index := 0; index < 24; index++ {
		if _, err := store.recordObjectiveTransition(map[string]any{
			"project": "contextlattice", "objective_id": fmt.Sprintf("obj_global_%03d", index),
			"objective": fmt.Sprintf("Global objective %03d", index), "transition_type": "created", "actor": "codex",
		}); err != nil {
			t.Fatalf("record global objective %d: %v", index, err)
		}
	}
	global := store.objectiveGraph("contextlattice", "", time.Time{}, true, 5)
	globalNodes, nodesOK := global["nodes"].([]objectiveGraphNode)
	if !nodesOK || len(globalNodes) != 5 || anyToInt(global["node_count"], 0) != 5 ||
		!anyToBool(global["traversal_truncated"]) || !anyToBool(global["graph_truncated"]) || anyToBool(global["complete"]) {
		t.Fatalf("global node limit was not enforced: %#v", global)
	}

	for index := 0; index < 40; index++ {
		transitionType := "progressed"
		if index == 0 {
			transitionType = "created"
		}
		if _, err := store.recordObjectiveTransition(map[string]any{
			"project": "contextlattice", "objective_id": "obj_dense", "objective": "Dense bounded objective",
			"transition_type": transitionType, "actor": "codex",
			"session_id": fmt.Sprintf("session_dense_%03d", index),
		}); err != nil {
			t.Fatalf("record dense transition %d: %v", index, err)
		}
	}
	dense := store.objectiveGraph("contextlattice", "obj_dense", time.Time{}, true, 7)
	denseNodes, denseNodesOK := dense["nodes"].([]objectiveGraphNode)
	denseEdges, denseEdgesOK := dense["edges"].([]objectiveGraphEdge)
	denseTransitions, denseTransitionsOK := dense["transitions"].([]objectiveTransition)
	if !denseNodesOK || !denseEdgesOK || !denseTransitionsOK ||
		len(denseNodes) > 7 || len(denseEdges) != 7 || len(denseTransitions) != 7 {
		t.Fatalf("dense graph returned an unbounded list: %#v", dense)
	}
	if anyToInt(dense["limit"], 0) != 7 || anyToInt(dense["transition_count"], 0) != 40 ||
		anyToInt(dense["transition_omitted_count"], 0) != 33 || !anyToBool(dense["edge_truncated"]) ||
		!anyToBool(dense["transition_truncated"]) || !anyToBool(dense["transitions_included"]) ||
		!anyToBool(dense["graph_truncated"]) || anyToBool(dense["complete"]) {
		t.Fatalf("dense graph omission metadata is incorrect: %#v", dense)
	}
}

func TestObjectiveGraphHTTPBoundaryCompactionPreservesHonestMetadata(t *testing.T) {
	store := newTestContinuityStore(t)
	store.maxBytes = 16 << 20
	largeText := strings.Repeat("bounded-objective-evidence-", 190)
	for index := 0; index < 110; index++ {
		payload := map[string]any{
			"project": "contextlattice", "objective_id": fmt.Sprintf("obj_boundary_%03d", index),
			"objective": largeText, "summary": largeText, "transition_type": "created", "actor": "contract_boundary_test",
		}
		if index < 109 {
			payload["depends_on"] = []any{fmt.Sprintf("obj_boundary_%03d", index+1)}
		}
		if _, err := store.recordObjectiveTransition(payload); err != nil {
			t.Fatalf("record oversized graph transition %d: %v", index, err)
		}
	}

	gateway := httptest.NewServer(buildNativeMux(&server{continuity: store}))
	defer gateway.Close()
	resp, err := http.Get(gateway.URL + "/memory/objectives/graph?project=contextlattice&objective_id=obj_boundary_000&limit=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("objective graph status=%d body=%s", resp.StatusCode, string(raw))
	}
	if actual := len(strings.TrimSpace(string(raw))); actual > 500000 {
		t.Fatalf("objective graph escaped contract byte ceiling: %d", actual)
	}
	graph := map[string]any{}
	if err := json.Unmarshal(raw, &graph); err != nil {
		t.Fatal(err)
	}
	formatContract := anyMap(graph["format_contract"])
	if anyToString(anyMap(formatContract["validation"])["status"]) != "passed" || !anyToBool(formatContract["contract_valid"]) {
		t.Fatalf("compacted graph failed contract validation: %#v", formatContract)
	}
	nodes := contextPackAnyList(graph["nodes"])
	edges := contextPackAnyList(graph["edges"])
	transitions := contextPackAnyList(graph["transitions"])
	if len(nodes) >= 110 || len(transitions) >= 110 || !anyToBool(graph["boundary_compacted"]) ||
		!anyToBool(graph["graph_truncated"]) || anyToBool(graph["complete"]) || !anyToBool(formatContract["truncated"]) {
		t.Fatalf("oversized graph did not expose boundary compaction: %#v", graph)
	}
	if anyToInt(graph["node_count"], -1) != len(nodes) || anyToInt(graph["edge_count"], -1) != len(edges) ||
		anyToInt(graph["transition_count"], 0) != 110 ||
		anyToInt(graph["transition_omitted_count"], -1) != 110-len(transitions) ||
		!anyToBool(graph["traversal_truncated"]) || !anyToBool(graph["edge_truncated"]) || !anyToBool(graph["transition_truncated"]) {
		t.Fatalf("boundary-compacted graph counts are stale: %#v", graph)
	}
}

func TestObjectiveGraphBoundaryReportsInnerNodeLinkClipping(t *testing.T) {
	payload := map[string]any{
		"nodes": []any{map[string]any{
			"objective_id": "obj_links", "task_identity_ids": []any{"task_a"},
			"execution_lane_ids": []any{}, "session_ids": []any{"sess_a"},
			"decision_change_ids": []any{}, "outcome_ids": []any{}, "checkpoint_ids": []any{},
		}},
		"edges": []any{}, "transitions": []any{}, "node_count": 1, "edge_count": 0,
		"transition_count": 0, "transition_omitted_count": 0, "transitions_included": true,
		"node_link_truncated": false, "boundary_compacted": false, "complete": true,
	}
	stats := agentBoundaryStats{ObjectiveGraphNodeLinksBefore: 3, ListsClipped: 1}
	reconcileObjectiveGraphBoundaryMetadata(payload, &stats)
	if !anyToBool(payload["node_link_truncated"]) || !anyToBool(payload["boundary_compacted"]) || anyToBool(payload["complete"]) {
		t.Fatalf("inner node-link clipping was not reported honestly: %#v", payload)
	}
}

func TestContinuityEndpointsReturnUnavailableWithoutStore(t *testing.T) {
	gateway := httptest.NewServer(buildNativeMux(&server{}))
	defer gateway.Close()
	for _, target := range []string{
		gateway.URL + "/memory/objectives/graph?project=contextlattice",
		gateway.URL + "/memory/decision-changes?project=contextlattice",
	} {
		status, payload := getAgentSessionJSON(t, target)
		if status != http.StatusServiceUnavailable || anyToString(payload["error"]) != "continuity_ledger_unavailable" {
			t.Fatalf("nil continuity store did not fail available endpoint safely: target=%s status=%d payload=%#v", target, status, payload)
		}
	}
	status, payload := postAgentSessionJSON(t, gateway.URL+"/memory/objectives/transition", `{"project":"contextlattice"}`)
	if status != http.StatusServiceUnavailable || anyToString(payload["error"]) != "continuity_ledger_unavailable" {
		t.Fatalf("nil continuity transition endpoint panicked or returned wrong status: status=%d payload=%#v", status, payload)
	}
}

func TestAgentSessionContinuitySeparatesTaskIdentityFromExecutionLane(t *testing.T) {
	continuity := newTestContinuityStore(t)
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "sessions.json"))
	sessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(buildNativeMux(&server{agentSessions: sessions, continuity: continuity}))
	defer gateway.Close()

	request := func(branch string) map[string]any {
		status, payload := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
			"ensure":true,"agent":"codex","agent_id":"codex_test","project":"contextlattice","repo":"contextlattice",
			"task_id":"T1","objective":"Ship continuity identity","branch":"`+branch+`"
		}`)
		if status != 200 {
			t.Fatalf("session start %s status=%d payload=%#v", branch, status, payload)
		}
		return payload
	}
	main := request("main")
	release := request("release")
	mainAgain := request("main")
	mainSession := anyMap(main["session"])
	releaseSession := anyMap(release["session"])
	if anyToString(mainSession["task_identity_id"]) == "" || anyToString(mainSession["task_identity_id"]) != anyToString(releaseSession["task_identity_id"]) {
		t.Fatalf("same task did not retain stable identity: main=%#v release=%#v", mainSession, releaseSession)
	}
	if anyToString(mainSession["execution_lane_id"]) == anyToString(releaseSession["execution_lane_id"]) {
		t.Fatalf("different branches shared an execution lane")
	}
	if anyToString(mainSession["id"]) == anyToString(releaseSession["id"]) || anyToBool(release["reused"]) {
		t.Fatalf("different execution lane reused session: %#v", release)
	}
	if !anyToBool(mainAgain["reused"]) || anyToString(anyMap(mainAgain["session"])["id"]) != anyToString(mainSession["id"]) {
		t.Fatalf("same task and lane did not reuse session: %#v", mainAgain)
	}
	beforeScopeSessions := len(sessions.sessions)
	status, scopeMismatch := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"ensure":true,"agent":"codex","agent_id":"codex_test","project":"other-project","repo":"contextlattice",
		"task_identity_id":"`+anyToString(mainSession["task_identity_id"])+`","objective":"Reject cross-scope identity","branch":"scope-mismatch"
	}`)
	if status != 422 || anyToString(scopeMismatch["error"]) != "invalid_agent_session_continuity" {
		t.Fatalf("cross-scope session identity status=%d payload=%#v", status, scopeMismatch)
	}
	if len(sessions.sessions) != beforeScopeSessions {
		t.Fatalf("cross-scope continuity identity created a session")
	}
	status, missing := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"ensure":true,"agent":"codex","agent_id":"codex_test","project":"contextlattice","repo":"contextlattice",
		"task_identity_id":"task_missing","objective":"Do not bind a missing explicit identity","branch":"missing"
	}`)
	if status != http.StatusUnprocessableEntity || anyToString(missing["error"]) != "invalid_agent_session_continuity" {
		t.Fatalf("missing explicit identity did not fail closed: status=%d payload=%#v", status, missing)
	}
	status, missingCamel := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"ensure":true,"agent":"codex","agent_id":"codex_test","project":"contextlattice","repo":"contextlattice",
		"taskIdentityId":"task_missing_camel","objective":"Do not bypass explicit identity validation","branch":"missing-camel"
	}`)
	if status != http.StatusUnprocessableEntity || anyToString(missingCamel["error"]) != "invalid_agent_session_continuity" {
		t.Fatalf("camel-case explicit identity bypassed validation: status=%d payload=%#v", status, missingCamel)
	}
	if len(sessions.sessions) != beforeScopeSessions {
		t.Fatalf("missing explicit identity created or reused a session")
	}
	beforeSessions := len(sessions.sessions)
	status, invalid := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", `{
		"ensure":true,"agent":"codex","agent_id":"codex_test","project":"contextlattice",
		"objective":"Reject malformed execution lane","execution_lane_id":"lane with spaces"
	}`)
	if status != 422 || anyToString(invalid["error"]) != "invalid_agent_session_continuity" {
		t.Fatalf("invalid execution lane status=%d payload=%#v", status, invalid)
	}
	if len(sessions.sessions) != beforeSessions {
		t.Fatalf("invalid continuity input created a session")
	}
}

func TestAgentSessionReuseCannotOverrideResolvedIdentity(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "sessions.json"))
	sessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	first, reused, err := sessions.startOrReuse(map[string]any{
		"ensure": true, "session_id": "session_alpha", "reuse_key": "shared-selector", "agent": "codex",
		"project": "alpha", "repo": "repo", "task_id": "task-alpha", "task_identity_id": "identity-alpha", "execution_lane_id": "lane-alpha",
	})
	if err != nil || reused {
		t.Fatalf("create first session: session=%#v reused=%v err=%v", first, reused, err)
	}
	second, reused, err := sessions.startOrReuse(map[string]any{
		"ensure": true, "reuse_key": "shared-selector", "agent": "codex",
		"project_name": "alpha", "repo": "repo", "taskId": "task-beta", "taskIdentityId": "identity-beta", "executionLaneId": "lane-beta",
	})
	if err != nil || reused || anyToString(second["id"]) == anyToString(first["id"]) {
		t.Fatalf("reuse key overrode resolved task/lane identity: first=%#v second=%#v reused=%v err=%v", first, second, reused, err)
	}
	_, _, err = sessions.startOrReuse(map[string]any{
		"ensure": true, "session_id": "session_alpha", "agent": "codex",
		"project": "beta", "repo": "repo", "task_identity_id": "identity-beta", "execution_lane_id": "lane-beta",
	})
	if !errors.Is(err, errAgentSessionReuseConflict) {
		t.Fatalf("explicit session id accepted incompatible identity: %v", err)
	}
	caseVariant, reused, err := sessions.startOrReuse(map[string]any{
		"ensure": true, "reuse_key": "shared-selector", "agent": "codex",
		"project": "alpha", "repo": "repo", "task_id": "task-alpha", "task_identity_id": "Identity-alpha", "execution_lane_id": "lane-alpha",
	})
	if err != nil || reused || anyToString(caseVariant["id"]) == anyToString(first["id"]) {
		t.Fatalf("case-distinct opaque identity reused an existing session: session=%#v reused=%v err=%v", caseVariant, reused, err)
	}
}

func TestAgentSessionEventsCannotEstablishOrRebindOwnership(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "sessions.json"))
	sessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessions.startOrReuse(map[string]any{
		"session_id": "sess-owned", "agent": "codex", "project": "contextlattice", "repo": "repo",
		"task_id": "task-a", "task_identity_id": "Identity-A", "execution_lane_id": "Lane-A", "branch": "main",
	}); err != nil {
		t.Fatalf("seed owned session: %v", err)
	}
	gateway := httptest.NewServer(buildNativeMux(&server{agentSessions: sessions}))
	defer gateway.Close()
	for name, body := range map[string]string{
		"top-level rebind":        `{"session_id":"sess-owned","type":"progress","taskIdentityId":"Identity-B"}`,
		"nested ownership rebind": `{"session_id":"sess-owned","type":"progress","metadata":{"ownership":{"execution_lane_id":"Lane-B"}}}`,
		"event-first ownership":   `{"session_id":"sess-event-first","type":"progress","metadata":{"agent_state":{"task_identity_id":"Identity-A"}}}`,
	} {
		status, payload := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", body)
		if status != http.StatusConflict || anyToString(payload["error"]) != "agent_session_ownership_conflict" {
			t.Fatalf("%s was not rejected: status=%d payload=%#v", name, status, payload)
		}
	}
	status, matched := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/event", `{
		"session_id":"sess-owned","type":"progress","metadata":{"ownership":{"task_identity_id":"Identity-A","execution_lane_id":"Lane-A"}}
	}`)
	if status != http.StatusOK || anyToString(anyMap(matched["session"])["task_identity_id"]) != "Identity-A" {
		t.Fatalf("matching ownership event was rejected or mutated: status=%d payload=%#v", status, matched)
	}
	if _, _, ok := sessions.get("sess-event-first"); ok {
		t.Fatal("rejected event-first ownership created a session")
	}
}

func TestAgentSessionExplicitStartCannotRebindOwnership(t *testing.T) {
	t.Setenv("GO_AGENT_SESSIONS_PATH", filepath.Join(t.TempDir(), "sessions.json"))
	sessions, err := newAgentSessionStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	original, reused, err := sessions.startOrReuse(map[string]any{
		"session_id": "sess-owned-start", "agent": "codex", "agent_id": "codex-primary",
		"project": "contextlattice", "repo": "repo", "task_identity_id": "Identity-A",
		"execution_lane_id": "Lane-A", "native_session_id": "native-A",
	})
	if err != nil || reused {
		t.Fatalf("seed owned session: session=%#v reused=%v err=%v", original, reused, err)
	}
	_, _, err = sessions.startOrReuse(map[string]any{
		"session_id": "sess-owned-start", "agent": "codex", "agent_id": "codex-other",
		"project": "contextlattice", "repo": "repo", "task_identity_id": "Identity-A",
		"execution_lane_id": "Lane-A", "native_session_id": "native-B",
	})
	if !errors.Is(err, errAgentSessionReuseConflict) {
		t.Fatalf("duplicate explicit start rebound ownership instead of conflicting: %v", err)
	}
	_, _, err = sessions.startOrReuse(map[string]any{
		"ensure": true, "session_id": "sess-owned-start", "agent": "codex", "agent_id": "codex-primary",
		"project": "contextlattice", "repo": "repo", "task_identity_id": "Identity-A",
		"execution_lane_id": "Lane-A", "native_session_id": "native-B",
	})
	if !errors.Is(err, errAgentSessionReuseConflict) {
		t.Fatalf("ensure handed an existing session to a different native owner: %v", err)
	}
	after, _, ok := sessions.get("sess-owned-start")
	if !ok || anyToString(after["agent_id"]) != "codex-primary" || anyToString(after["native_session_id"]) != "native-A" {
		t.Fatalf("conflicting explicit start mutated ownership: before=%#v after=%#v", original, after)
	}
	_, _, err = sessions.startOrReuse(map[string]any{
		"ensure": true, "session_id": "sess-owned-start", "agent": "codex", "agent_id": "codex-primary",
		"project": "contextlattice", "repo": "repo", "task_identity_id": "Identity-A",
		"execution_lane_id": "Lane-A", "native_session_id": "native-A",
	})
	if err != nil {
		t.Fatalf("matching ensure should reuse immutable ownership: %v", err)
	}
}

func TestAgentSessionExplicitIdentityFailsClosedWhenContinuityUnavailable(t *testing.T) {
	newSessions := func(path string) *agentSessionStore {
		t.Helper()
		t.Setenv("GO_AGENT_SESSIONS_PATH", path)
		store, err := newAgentSessionStoreFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	request := `{
		"ensure":true,"agent":"codex","project":"contextlattice","repo":"contextlattice",
		"task_identity_id":"task_verified_elsewhere","objective":"Do not forge continuity"
	}`
	unavailableSessions := newSessions(filepath.Join(t.TempDir(), "unavailable-sessions.json"))
	unavailable := &continuityStore{enabled: false, lastError: "continuity ledger hash mismatch"}
	gateway := httptest.NewServer(buildNativeMux(&server{agentSessions: unavailableSessions, continuity: unavailable}))
	status, payload := postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", request)
	gateway.Close()
	if status != http.StatusServiceUnavailable || anyToString(payload["error"]) != "agent_session_continuity_unavailable" || len(unavailableSessions.sessions) != 0 {
		t.Fatalf("initialization failure allowed explicit identity: status=%d payload=%#v sessions=%d", status, payload, len(unavailableSessions.sessions))
	}

	disabledSessions := newSessions(filepath.Join(t.TempDir(), "disabled-sessions.json"))
	disabled := &continuityStore{enabled: false}
	gateway = httptest.NewServer(buildNativeMux(&server{agentSessions: disabledSessions, continuity: disabled}))
	status, payload = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", request)
	gateway.Close()
	if status != http.StatusOK || len(disabledSessions.sessions) != 1 || anyToString(anyMap(payload["session"])["task_identity_id"]) != "" {
		t.Fatalf("intentional disable did not strip unverifiable identity safely: status=%d payload=%#v", status, payload)
	}

	continuity := newTestContinuityStore(t)
	continuity.path = t.TempDir()
	if _, err := continuity.reconcile(map[string]any{
		"project": "contextlattice", "objective": "Force a verified persistence failure before explicit reuse",
	}, true); err == nil || continuity.enabled {
		t.Fatalf("test fixture did not disable continuity after append failure: err=%v status=%#v", err, continuity.snapshot())
	}
	runtimeFailureSessions := newSessions(filepath.Join(t.TempDir(), "runtime-failure-sessions.json"))
	gateway = httptest.NewServer(buildNativeMux(&server{agentSessions: runtimeFailureSessions, continuity: continuity}))
	status, payload = postAgentSessionJSON(t, gateway.URL+"/v1/agents/sessions/start", request)
	gateway.Close()
	if status != http.StatusServiceUnavailable || anyToString(payload["error"]) != "agent_session_continuity_unavailable" || len(runtimeFailureSessions.sessions) != 0 {
		t.Fatalf("runtime persistence failure allowed explicit identity: status=%d payload=%#v sessions=%d", status, payload, len(runtimeFailureSessions.sessions))
	}
}

func TestContinuityHTTPRoutesEmitValidContracts(t *testing.T) {
	continuity := newTestContinuityStore(t)
	gateway := httptest.NewServer(buildNativeMux(&server{continuity: continuity}))
	defer gateway.Close()
	assertContract := func(payload map[string]any, schemaID string) {
		t.Helper()
		contract := anyMap(payload["format_contract"])
		if anyToString(contract["schema_id"]) != schemaID || !anyToBool(contract["contract_valid"]) {
			t.Fatalf("invalid %s contract: %#v", schemaID, payload)
		}
	}

	status, reconciled := postAgentSessionJSON(t, gateway.URL+"/memory/continuity/reconcile", `{
		"project":"contextlattice","repo":"contextlattice","task_id":"T1","objective":"Ship continuity identity","branch":"main","agent_id":"codex_test"
	}`)
	if status != 200 {
		t.Fatalf("reconcile status=%d payload=%#v", status, reconciled)
	}
	assertContract(reconciled, taskIdentityReconciliationContractID)
	assertContract(anyMap(reconciled["receipt"]), taskIdentityReceiptContractID)
	unsafeReconciliation := cloneJSONMap(reconciled)
	unsafeReconciliation["exact_first"] = false
	unsafeReconciliation["semantic_auto_merge"] = true
	if findings := validateAgentContractPayload(taskIdentityReconciliationContractID, unsafeReconciliation); len(findings) < 2 {
		t.Fatalf("task identity contract certified unsafe merge semantics: %#v", findings)
	}
	taskIdentityID := continuityIdentityID(t, reconciled)

	status, transition := postAgentSessionJSON(t, gateway.URL+"/memory/objectives/transition", `{
		"project":"contextlattice","objective_id":"obj_t1","objective":"Ship T1","transition_type":"started","idempotency_key":"http_objective_t1","actor":"codex_test","task_identity_id":"`+taskIdentityID+`"
	}`)
	if status != 200 {
		t.Fatalf("transition status=%d payload=%#v", status, transition)
	}
	assertContract(transition, objectiveTransitionContractID)

	status, changed := postAgentSessionJSON(t, gateway.URL+"/memory/decision-changes", `{
		"project":"contextlattice","objective_id":"obj_t1","idempotency_key":"http_decision_t1","before_decision":"Auto merge","after_decision":"Require confirmation",
		"confidence_before":0.4,"confidence_after":0.9,"trigger_evidence":[{"ref_id":"eval:ambiguity","kind":"eval"}],
		"actor":"codex_test","rationale":"The ambiguity holdout failed.","reason_code":"ambiguity_failed",
		"verification":{"status":"verified","method":"go_test","checker":"continuity_identity_test"}
	}`)
	if status != 200 {
		t.Fatalf("decision status=%d payload=%#v", status, changed)
	}
	assertContract(changed, decisionChangeContractID)
	incompleteDecision := cloneJSONMap(changed)
	incompleteDecision["objective_transition"] = map[string]any{}
	if findings := validateAgentContractPayload(decisionChangeContractID, incompleteDecision); len(findings) == 0 {
		t.Fatal("decision contract accepted an empty linked objective transition")
	}

	status, graph := getAgentSessionJSON(t, gateway.URL+"/memory/objectives/graph?project=contextlattice&objective_id=obj_t1")
	if status != 200 {
		t.Fatalf("graph status=%d payload=%#v", status, graph)
	}
	assertContract(graph, objectiveGraphContractID)
	status, decisions := getAgentSessionJSON(t, gateway.URL+"/memory/decision-changes?project=contextlattice&objective_id=obj_t1")
	if status != 200 {
		t.Fatalf("decision query status=%d payload=%#v", status, decisions)
	}
	assertContract(decisions, decisionChangeQueryContractID)
	status, compacted := postAgentSessionJSON(t, gateway.URL+"/memory/continuity/reconcile", `{
		"operation":"compact","actor":"codex_test","reason":"prove lossless operator compaction"
	}`)
	if status != 200 || !anyToBool(anyMap(compacted["compaction"])["lossless"]) {
		t.Fatalf("continuity compaction status=%d payload=%#v", status, compacted)
	}
	assertContract(compacted, taskIdentityReconciliationContractID)
}

func benchmarkContinuityStore(b *testing.B) *continuityStore {
	b.Helper()
	b.Setenv("CONTEXTLATTICE_CONTINUITY_ENABLED", "true")
	b.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_PATH", filepath.Join(b.TempDir(), "continuity.ndjson"))
	b.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_FSYNC", "false")
	b.Setenv("CONTEXTLATTICE_CONTINUITY_LEDGER_MAX_BYTES", "67108864")
	store, err := newContinuityStoreFromEnv()
	if err != nil {
		b.Fatalf("new continuity benchmark store: %v", err)
	}
	return store
}

func benchmarkContinuityIdentities(b *testing.B, count int) *continuityStore {
	b.Helper()
	store := benchmarkContinuityStore(b)
	for index := 0; index < count; index++ {
		_, err := store.reconcile(map[string]any{
			"project": "contextlattice", "repo": "contextlattice",
			"task_id":   fmt.Sprintf("bench-%04d", index),
			"objective": fmt.Sprintf("continuity benchmark task %04d unique", index),
		}, true)
		if err != nil {
			b.Fatalf("seed identity %d: %v", index, err)
		}
	}
	return store
}

func BenchmarkContinuityExactTaskID1000(b *testing.B) {
	store := benchmarkContinuityIdentities(b, 1000)
	payload := map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "bench-0500",
		"objective": "wording does not matter on an exact external task id",
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := store.reconcile(payload, false)
		if err != nil || anyToString(result["match_mode"]) != "exact_task_id" {
			b.Fatalf("exact reconciliation failed: result=%#v err=%v", result, err)
		}
	}
}

func BenchmarkContinuityExactObjective1000(b *testing.B) {
	store := benchmarkContinuityIdentities(b, 1000)
	payload := map[string]any{
		"project": "contextlattice", "repo": "contextlattice",
		"objective": "continuity benchmark task 0500 unique",
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := store.reconcile(payload, false)
		if err != nil || anyToString(result["match_mode"]) != "exact_objective" {
			b.Fatalf("exact objective reconciliation failed: result=%#v err=%v", result, err)
		}
	}
}

func BenchmarkContinuitySemanticAdvisory1000(b *testing.B) {
	store := benchmarkContinuityIdentities(b, 1000)
	payload := map[string]any{
		"project": "contextlattice", "repo": "contextlattice", "task_id": "new-semantic-id",
		"objective": "continuity benchmark task 0500 unique safely",
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := store.reconcile(payload, false)
		if err != nil || anyToString(result["match_mode"]) != "semantic_candidate" || !anyToBool(result["abstained"]) {
			b.Fatalf("semantic advisory failed: result=%#v err=%v", result, err)
		}
	}
}

func BenchmarkObjectiveGraphReplay1000(b *testing.B) {
	store := benchmarkContinuityStore(b)
	objectiveID := "objective_benchmark"
	store.objectiveTransitions = make([]objectiveTransition, 0, 1000)
	store.objectiveTransitionIndex = map[string][]int{}
	store.objectiveRelationIndex = map[string][]objectiveGraphRelationRef{}
	for index := 0; index < 1000; index++ {
		store.applyObjectiveTransitionValidatedLocked(objectiveTransition{
			SchemaID: objectiveTransitionContractID, TransitionID: fmt.Sprintf("ot_bench_%04d", index),
			ObjectiveID: objectiveID, Project: "contextlattice", Objective: "benchmark longitudinal objective",
			TransitionType: "progressed", ToStatus: "active", Actor: "benchmark",
			Summary: fmt.Sprintf("verified step %04d", index), OccurredAt: time.Unix(int64(index), 0).UTC().Format(time.RFC3339Nano),
		})
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result := store.objectiveGraph("contextlattice", objectiveID, time.Time{}, true, 1000)
		if anyToInt(result["transition_count"], 0) != 1000 || anyToInt(result["node_count"], 0) != 1 {
			b.Fatalf("objective graph replay failed: %#v", result)
		}
	}
}
