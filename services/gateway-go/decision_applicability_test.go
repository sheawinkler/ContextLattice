package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func decisionApplicabilityFixture(t *testing.T) (*continuityStore, *memoryStore, map[string]any) {
	t.Helper()
	store := newTestContinuityStore(t)
	owner := newDerivedMemoryTestStore(t)
	identity, err := store.reconcile(map[string]any{"project": "alpha", "repo": "alpha-repo", "task_id": "release",
		"objective": "Verify the release"}, true)
	if err != nil {
		t.Fatal(err)
	}
	task := continuityIdentityID(t, identity)
	input := seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Acceptance requires verified deployment.", "test", "session")
	return store, owner, map[string]any{
		"project": "alpha", "objective_id": "obj_release", "task_identity_id": task, "idempotency_key": "typed-decision",
		"before": "Keep the old deployment", "after": "Recompute the deployment", "confidence_before": 0.4, "confidence_after": 0.8,
		"trigger_evidence": []any{"alpha::notes/input.md"}, "actor": "test", "rationale": "The pinned acceptance owner changed.", "reason_code": "source_changed",
		"applicability": map[string]any{"schema_id": decisionApplicabilitySchemaID,
			"scope":       map[string]any{"project": "alpha", "objective_id": "obj_release", "task_identity_id": task, "repo": "alpha-repo", "branch": "main"},
			"inputs":      []any{map[string]any{"memory_id": "alpha::notes/input.md", "target_event_id": input.EventID, "target_content_hash": input.ContentHash}},
			"assumptions": []any{map[string]any{"assumption_id": "acceptance", "input_id": "alpha::notes/input.md", "role": "acceptance_criteria"}}},
	}
}

func decisionOwnerValidator(owner *memoryStore) func(decisionChange) error {
	return func(change decisionChange) error {
		return owner.validateDecisionApplicabilityOwner(context.Background(), change.Applicability)
	}
}

func TestDecisionApplicabilityStrictShapeAndLegacy(t *testing.T) {
	_, _, payload := decisionApplicabilityFixture(t)
	change, _, err := normalizeDecisionChange(payload)
	if err != nil || change.Applicability == nil {
		t.Fatalf("valid explicit contract lost: %v", err)
	}
	mutations := map[string]func(map[string]any){
		"null":                func(p map[string]any) { p["applicability"] = nil },
		"unknown":             func(p map[string]any) { anyMap(p["applicability"])["verified"] = true },
		"no assumptions":      func(p map[string]any) { delete(anyMap(p["applicability"]), "assumptions") },
		"foreign scope":       func(p map[string]any) { anyMap(anyMap(p["applicability"])["scope"])["project"] = "beta" },
		"unknown scope field": func(p map[string]any) { anyMap(anyMap(p["applicability"])["scope"])["trusted"] = true },
		"missing event": func(p map[string]any) {
			anyMap(contextPackAnyList(anyMap(p["applicability"])["inputs"])[0])["target_event_id"] = ""
		},
		"bad hash": func(p map[string]any) {
			anyMap(contextPackAnyList(anyMap(p["applicability"])["inputs"])[0])["target_content_hash"] = "bad"
		},
		"unbound assumption": func(p map[string]any) {
			anyMap(contextPackAnyList(anyMap(p["applicability"])["assumptions"])[0])["input_id"] = "alpha::notes/other.md"
		},
		"duplicate input": func(p map[string]any) {
			a := anyMap(p["applicability"])
			a["inputs"] = append(contextPackAnyList(a["inputs"]), contextPackAnyList(a["inputs"])[0])
		},
		"oversized": func(p map[string]any) {
			anyMap(anyMap(p["applicability"])["scope"])["branch"] = strings.Repeat("x", 8193)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			p := cloneJSONMap(payload)
			mutate(p)
			if _, _, err := normalizeDecisionChange(p); err == nil {
				t.Fatal("malformed typed applicability accepted")
			}
		})
	}
	legacy := cloneJSONMap(payload)
	delete(legacy, "applicability")
	old, _, err := normalizeDecisionChange(legacy)
	encoded, _ := json.Marshal(old)
	if err != nil || old.Applicability != nil || strings.Contains(string(encoded), "applicability") {
		t.Fatal("omitted legacy shape changed")
	}
}

func TestDecisionApplicabilityOriginalPinsAndHistoricalRetry(t *testing.T) {
	store, owner, payload := decisionApplicabilityFixture(t)
	if _, _, err := store.recordDecisionChange(payload); !errors.Is(err, errDecisionApplicabilityOwner) {
		t.Fatal("typed store append bypassed owner validation")
	}
	change, _, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(store.path)
	seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Corrected acceptance requires a different deployment.", "test", "session")
	called := false
	retry, _, err := store.recordDecisionChangeWithOwner(payload, func(decisionChange) error { called = true; return errDecisionApplicabilityOwner })
	after, _ := os.ReadFile(store.path)
	if err != nil || called || !retry.idempotentReplay || !reflect.DeepEqual(retry.Applicability, change.Applicability) || string(before) != string(after) {
		t.Fatal("historical retry revalidated, rebound or appended", err)
	}
	newDecision := cloneJSONMap(payload)
	newDecision["idempotency_key"] = "new-decision-with-stale-pins"
	if _, _, err := store.recordDecisionChangeWithOwner(newDecision, decisionOwnerValidator(owner)); err == nil {
		t.Fatal("new decision accepted stale original pins")
	}
	conflict := cloneJSONMap(payload)
	anyMap(anyMap(conflict["applicability"])["scope"])["branch"] = "other"
	if _, _, err := store.recordDecisionChangeWithOwner(conflict, decisionOwnerValidator(owner)); err == nil {
		t.Fatal("idempotency key accepted different applicability")
	}
}

func TestDecisionApplicabilityOwnerCheckReleasesContinuityLockAndKeepsObservedPins(t *testing.T) {
	store, owner, payload := decisionApplicabilityFixture(t)
	change, _, err := store.recordDecisionChangeWithOwner(payload, func(change decisionChange) error {
		if !store.mu.TryLock() {
			return errors.New("continuity lock held during owner check")
		}
		store.mu.Unlock()
		if err := decisionOwnerValidator(owner)(change); err != nil {
			return err
		}
		seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Corrected after observation.", "test", "session")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := owner.currentEntry("alpha", "notes/input.md")
	if change.Applicability.Inputs[0].TargetEventID == current.EventID {
		t.Fatal("append refreshed the computation's original event pin")
	}
}

func TestDecisionApplicabilityRestartRetainsHistoryWithoutOwnerReads(t *testing.T) {
	store, owner, payload := decisionApplicabilityFixture(t)
	change, _, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner))
	if err != nil {
		t.Fatal(err)
	}
	seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "New owner version.", "test", "session")
	store.close()
	reopened, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	retry, _, err := reopened.recordDecisionChange(payload)
	if err != nil || !retry.idempotentReplay || !reflect.DeepEqual(retry.Applicability, change.Applicability) {
		t.Fatal("restart erased immutable applicability or required fresh owner approval", err)
	}
}

func TestDecisionApplicabilityDoesNotSilentlyDropTypedInput(t *testing.T) {
	payload := map[string]any{
		"project": "alpha", "objective_id": "obj_release", "idempotency_key": "typed-input",
		"before": "Keep the old deployment", "after": "Recompute the deployment",
		"confidence_before": 0.4, "confidence_after": 0.8,
		"trigger_evidence": []any{"alpha::notes/input.md"}, "actor": "test",
		"rationale": "The input changed.", "reason_code": "source_changed",
		"applicability": map[string]any{"schema_id": "unsupported"},
	}
	change, _, err := normalizeDecisionChange(payload)
	if err != nil {
		return
	}
	raw, _ := json.Marshal(change)
	if !strings.Contains(string(raw), `"applicability"`) {
		t.Fatal("explicit typed applicability was silently discarded instead of validated")
	}
}
