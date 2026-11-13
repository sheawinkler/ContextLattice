package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestDecisionApplicabilityRejectsForgedRequestSelection(t *testing.T) {
	for _, field := range []string{"request_task_identity_id", "Request_Task_Identity_ID"} {
		t.Run(field, func(t *testing.T) {
			store, owner, payload := decisionApplicabilityFixture(t)
			payload[field] = payload["task_identity_id"]
			before, _ := os.ReadFile(store.path)
			if _, _, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner)); err == nil {
				t.Fatal("caller supplied server-owned request selection")
			}
			after, _ := os.ReadFile(store.path)
			if !bytes.Equal(before, after) {
				t.Fatal("forged selection changed ledger")
			}
		})
	}
}

func TestDecisionApplicabilityRetryRejectsDifferentOriginalTask(t *testing.T) {
	store, owner, payload := decisionApplicabilityFixture(t)
	original, _, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner))
	if err != nil {
		t.Fatal(err)
	}
	target := decisionMergeTarget(t, store, "alternate")
	mergeDecisionTasks(t, store, original.TaskIdentityID, target)
	changed := cloneJSONMap(payload)
	changed["task_identity_id"] = target
	before, _ := os.ReadFile(store.path)
	if _, _, err := store.recordDecisionChangeWithOwner(changed, decisionOwnerValidator(owner)); err == nil {
		t.Fatal("retry accepted a different original selection despite equivalent current alias")
	}
	after, _ := os.ReadFile(store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting retry changed history")
	}
}

func TestDecisionApplicabilityConcurrentAppend(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "equivalent", true: "conflicting"}[conflict], func(t *testing.T) {
			store, owner, payload := decisionApplicabilityFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			result := make(chan error, 1)
			go func() {
				_, _, err := store.recordDecisionChangeWithOwner(payload, func(change decisionChange) error {
					close(entered)
					<-release
					return decisionOwnerValidator(owner)(change)
				})
				result <- err
			}()
			<-entered
			second := cloneJSONMap(payload)
			if conflict {
				anyMap(anyMap(second["applicability"])["scope"])["branch"] = "review"
			}
			winner, _, err := store.recordDecisionChangeWithOwner(second, decisionOwnerValidator(owner))
			close(release)
			loser := <-result
			if err != nil || (loser != nil) != conflict || len(store.decisionChanges) != 1 || len(store.objectiveTransitions) != 1 {
				t.Fatalf("concurrent append did not preserve one atomic winner: winner=%v loser=%v", err, loser)
			}
			if winner.Applicability.Scope.Branch != anyToString(anyMap(anyMap(second["applicability"])["scope"])["branch"]) {
				t.Fatal("winner lost its original scope")
			}
		})
	}
}

func TestDecisionApplicabilityScopeDriftDuringOwnerCheck(t *testing.T) {
	store, owner, payload := decisionApplicabilityFixture(t)
	target := decisionMergeTarget(t, store, "drift")
	_, _, err := store.recordDecisionChangeWithOwner(payload, func(change decisionChange) error {
		if err := decisionOwnerValidator(owner)(change); err != nil {
			return err
		}
		mergeDecisionTasks(t, store, change.TaskIdentityID, target)
		return nil
	})
	if err == nil || len(store.decisionChanges) != 0 || len(store.objectiveTransitions) != 0 {
		t.Fatal("new append rebound to a task that changed during owner observation")
	}
}

func TestDecisionApplicabilityCommitUnknownAndCompaction(t *testing.T) {
	store, owner, payload := decisionApplicabilityFixture(t)
	store.afterPersistHook = func() error { return errors.New("simulated lost typed response") }
	if _, _, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner)); !continuityCommitUnknown(err) {
		t.Fatalf("expected durable ambiguous response: %v", err)
	}
	store.close()
	reopened, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	if _, err := reopened.compact(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(reopened.path)
	change, transition, err := reopened.recordDecisionChange(payload)
	after, _ := os.ReadFile(reopened.path)
	if err != nil || !change.idempotentReplay || !transition.idempotentReplay || !bytes.Equal(before, after) ||
		len(reopened.decisionChanges) != 1 || change.Applicability == nil || change.RequestTaskIdentityID != payload["task_identity_id"] {
		t.Fatalf("typed durable retry/compaction lost original request or required owner: %v", err)
	}
}

func TestDecisionApplicabilityAssumptionUsesExactInputHandle(t *testing.T) {
	_, _, payload := decisionApplicabilityFixture(t)
	row := anyMap(contextPackAnyList(anyMap(payload["applicability"])["assumptions"])[0])
	row["input_id"] = strings.ToUpper(anyToString(row["input_id"]))
	if _, _, err := normalizeDecisionChange(payload); err == nil {
		t.Fatal("noncanonical assumption input handle accepted")
	}
}

func TestDecisionApplicabilityOwnerCustodyMatrix(t *testing.T) {
	for _, mode := range []string{"valid_nested", "stale_leaf", "missing_receipt", "exact_leaf", "excluded_leaf", "no_fence", "owner_card", "owner_journal", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			store, owner, payload := decisionApplicabilityFixture(t)
			leaf, _ := owner.currentEntry("alpha", "notes/input.md")
			derived := putDerivedMemory(t, owner, "notes/decision-input.md", leaf)
			app := anyMap(payload["applicability"])
			app["inputs"] = []any{map[string]any{"memory_id": "alpha::" + derived.FileName,
				"target_event_id": derived.EventID, "target_content_hash": derived.ContentHash}}
			anyMap(contextPackAnyList(app["assumptions"])[0])["input_id"] = "alpha::" + derived.FileName
			ctx := context.Background()
			switch mode {
			case "stale_leaf":
				seedMemoryGraphRepairDoc(t, owner, "alpha", leaf.FileName, leaf.TopicPath, "Corrected leaf.", "test", "session")
			case "missing_receipt":
				if err := os.Remove(owner.memoryReferenceReceiptPath(derived.ReferenceTransactionID)); err != nil {
					t.Fatal(err)
				}
			case "exact_leaf":
				if err := owner.registerExactStatePath("alpha", leaf.FileName); err != nil {
					t.Fatal(err)
				}
			case "excluded_leaf":
				owner.policy.graphExcludeLowValue = true
				owner.policy.graphExcludeFilePatterns = append(owner.policy.graphExcludeFilePatterns, leaf.FileName)
			case "no_fence":
				owner.policy.edgePath = ""
			case "owner_card":
				if err := writeOwnerOnlyDurableAtomicFile(owner.currentStateGenerationCardPath("alpha"), []byte(`{"schema_id":"wrong"}`), true); err != nil {
					t.Fatal(err)
				}
			case "owner_journal":
				if err := writeOwnerOnlyDurableAtomicFile(owner.currentStateTransactionPath(), []byte(`{}`), true); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before, _ := os.ReadFile(store.path)
			_, _, err := store.recordDecisionChangeWithOwner(payload, func(change decisionChange) error {
				return owner.validateDecisionApplicabilityOwner(ctx, change.Applicability)
			})
			after, _ := os.ReadFile(store.path)
			if mode == "valid_nested" {
				if err != nil || len(store.decisionChanges) != 1 {
					t.Fatalf("valid nested custody refused: %v", err)
				}
			} else if err == nil || !bytes.Equal(before, after) || len(store.decisionChanges) != 0 {
				t.Fatal("invalid custody appended decision provenance")
			}
		})
	}
}

func TestDecisionApplicabilityStoredRequestPresence(t *testing.T) {
	_, _, payload := decisionApplicabilityFixture(t)
	for _, raw := range []map[string]any{
		{"request_task_identity_id": nil},
		{"request_task_identity_id": payload["task_identity_id"]},
		{"applicability": payload["applicability"]},
		{"applicability": payload["applicability"], "request_task_identity_id": nil},
		{"applicability": payload["applicability"], "Request_Task_Identity_ID": payload["task_identity_id"]},
	} {
		if err := validateStoredDecisionApplicability(raw); err == nil {
			t.Fatal("malformed stored request presence treated as legacy or valid typed provenance")
		}
	}
	store, _, _ := decisionApplicabilityFixture(t)
	err := store.applyEntryLocked(continuityLedgerEntry{Kind: continuityLedgerKindDecisionBundle, Payload: map[string]any{
		"schema_id": continuityDecisionBundleSchemaID, "Decision_Change": map[string]any{"Applicability": nil},
	}})
	if err == nil || !strings.Contains(err.Error(), "canonical decision_change") {
		t.Fatal("bundle alias bypassed raw typed validation")
	}
}
