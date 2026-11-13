package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func decisionMergeTarget(t *testing.T, store *continuityStore, suffix string) string {
	t.Helper()
	result, err := store.reconcile(map[string]any{"project": "alpha", "repo": "alpha-repo", "task_id": "target-" + suffix}, true)
	if err != nil {
		t.Fatal(err)
	}
	return continuityIdentityID(t, result)
}

func mergeDecisionTasks(t *testing.T, store *continuityStore, from, to string) {
	t.Helper()
	if from == to {
		t.Fatal("merge fixture did not create independent identities")
	}
	_, err := store.mergeTaskIdentities(map[string]any{"target_task_identity_id": to, "source_task_identity_ids": []any{from},
		"actor": "operator", "reason": "Verified fixture identity merge"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDecisionApplicabilityHistoricalRetrySurvivesIdentityMerges(t *testing.T) {
	for _, premerge := range []bool{false, true} {
		for _, count := range []int{1, 2} {
			t.Run(fmt.Sprintf("premerge_%t_later_merges_%d", premerge, count), func(t *testing.T) {
				store, owner, payload := decisionApplicabilityFixture(t)
				current := anyToString(payload["task_identity_id"])
				if premerge {
					target := decisionMergeTarget(t, store, "before")
					mergeDecisionTasks(t, store, current, target)
					current = target
				}
				original, transition, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner))
				if err != nil {
					t.Fatal(err)
				}
				for index := 0; index < count; index++ {
					target := decisionMergeTarget(t, store, fmt.Sprintf("later-%d", index))
					mergeDecisionTasks(t, store, current, target)
					current = target
				}
				seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/input.md", "pilot/decision", "Owner changed after append.", "test", "session")
				assertRetry := func(s *continuityStore) {
					before, _ := os.ReadFile(s.path)
					called := false
					got, companion, err := s.recordDecisionChangeWithOwner(payload, func(decisionChange) error { called = true; return errDecisionApplicabilityOwner })
					after, _ := os.ReadFile(s.path)
					if err != nil || called || !got.idempotentReplay || got.TaskIdentityID != original.TaskIdentityID ||
						got.DecisionChangeID != original.DecisionChangeID || companion.TransitionID != transition.TransitionID ||
						!reflect.DeepEqual(got.Applicability, original.Applicability) || !bytes.Equal(before, after) {
						t.Fatalf("historical retry rebound, rechecked, mutated or conflicted (reopened=%t): %v", s != store, err)
					}
					conflict := cloneJSONMap(payload)
					anyMap(anyMap(conflict["applicability"])["scope"])["branch"] = "changed"
					if _, _, err := s.recordDecisionChangeWithOwner(conflict, decisionOwnerValidator(owner)); err == nil {
						t.Fatal("changed applicability accepted under the original idempotency key")
					}
				}
				assertRetry(store)
				store.close()
				reopened, err := newContinuityStoreFromEnv()
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.close()
				assertRetry(reopened)
			})
		}
	}
}

func decisionCaseAliasMutations() map[string]func(map[string]any) {
	return map[string]func(map[string]any){
		"outer_valid": func(p map[string]any) { p["Applicability"] = p["applicability"]; delete(p, "applicability") },
		"outer_null":  func(p map[string]any) { p["APPLICABILITY"] = nil; delete(p, "applicability") },
		"mixed_outer": func(p map[string]any) { p["Applicability"] = nil },
		"scope":       func(p map[string]any) { a := anyMap(p["applicability"]); a["Scope"] = a["scope"]; delete(a, "scope") },
		"inputs": func(p map[string]any) {
			a := anyMap(p["applicability"])
			a["Inputs"] = a["inputs"]
			delete(a, "inputs")
		},
		"assumptions": func(p map[string]any) {
			a := anyMap(p["applicability"])
			a["Assumptions"] = a["assumptions"]
			delete(a, "assumptions")
		},
		"scope_field": func(p map[string]any) {
			s := anyMap(anyMap(p["applicability"])["scope"])
			s["Branch"] = s["branch"]
			delete(s, "branch")
		},
		"input_field": func(p map[string]any) {
			i := anyMap(contextPackAnyList(anyMap(p["applicability"])["inputs"])[0])
			i["TARGET_EVENT_ID"] = i["target_event_id"]
			delete(i, "target_event_id")
		},
		"assumption_field": func(p map[string]any) {
			a := anyMap(contextPackAnyList(anyMap(p["applicability"])["assumptions"])[0])
			a["Role"] = a["role"]
			delete(a, "role")
		},
	}
}

func TestDecisionApplicabilityRejectsCaseAliasesBeforeAppend(t *testing.T) {
	for name, mutate := range decisionCaseAliasMutations() {
		t.Run(name, func(t *testing.T) {
			store, _, payload := decisionApplicabilityFixture(t)
			mutate(payload)
			before, _ := os.ReadFile(store.path)
			if _, _, err := store.recordDecisionChange(payload); err == nil {
				t.Fatal("alias bypassed strict typed presence and became legacy")
			}
			after, _ := os.ReadFile(store.path)
			if !bytes.Equal(before, after) {
				t.Fatal("invalid optional-field spelling mutated the ledger")
			}
			if _, _, err := normalizeDecisionChange(payload); err == nil {
				t.Fatal("case alias accepted by the structural normalizer")
			}
		})
	}
}

func TestDecisionApplicabilityRejectsHashCorrectAliasReplay(t *testing.T) {
	for _, standalone := range []bool{false, true} {
		for name, mutate := range decisionCaseAliasMutations() {
			t.Run(fmt.Sprintf("standalone_%t_%s", standalone, name), func(t *testing.T) {
				store, owner, payload := decisionApplicabilityFixture(t)
				if _, _, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner)); err != nil {
					t.Fatal(err)
				}
				store.close()
				raw, err := os.ReadFile(store.path)
				if err != nil {
					t.Fatal(err)
				}
				var rewritten bytes.Buffer
				lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
				for index, line := range lines {
					var entry continuityLedgerEntry
					if err := json.Unmarshal([]byte(line), &entry); err != nil {
						t.Fatal(err)
					}
					originalHash, err := continuityEntryHash(entry)
					if err != nil || originalHash != entry.EntryHash {
						t.Fatal("fixture was not initially hash-correct")
					}
					if index == len(lines)-1 {
						decision := anyMap(entry.Payload["decision_change"])
						mutate(decision)
						if standalone {
							entry.Kind, entry.Payload = continuityLedgerKindDecisionChange, decision
						}
						entry.EntryHash, err = continuityEntryHash(entry)
						if err != nil {
							t.Fatal(err)
						}
					}
					if err := json.NewEncoder(&rewritten).Encode(entry); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(store.path, rewritten.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
				reopened, err := newContinuityStoreFromEnv()
				if reopened != nil {
					reopened.close()
				}
				if err == nil {
					t.Fatal("hash-correct alias/malformed presence silently became valid legacy replay")
				}
			})
		}
	}
}
