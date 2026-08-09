package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type frontierT6HoldoutCase struct {
	ID       string         `json:"id"`
	Item     int            `json:"item"`
	Surface  string         `json:"surface"`
	Scenario string         `json:"scenario"`
	Expected map[string]any `json:"expected"`
}

type frontierT6HoldoutResult struct {
	Decision   string
	SelectedID string
}

func frontierT6HoldoutFixture(t *testing.T) ([]frontierT6HoldoutCase, string) {
	t.Helper()
	fixturePath := filepath.Join("..", "..", "docs", "evals", "fixtures", "frontier-t6-agent-fit-holdout.v1.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read frozen Frontier T6 holdout: %v", err)
	}
	fixture := struct {
		SchemaID string                  `json:"schema_id"`
		Cases    []frontierT6HoldoutCase `json:"cases"`
	}{}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode frozen Frontier T6 holdout: %v", err)
	}
	if fixture.SchemaID != "frontier_t6_agent_fit_holdout.v1" || len(fixture.Cases) != 20 {
		t.Fatalf("unexpected frozen Frontier T6 holdout: schema=%q cases=%d", fixture.SchemaID, len(fixture.Cases))
	}
	digest := sha256.Sum256(raw)
	return fixture.Cases, hex.EncodeToString(digest[:])
}

func frontierT6HoldoutSelection(t *testing.T, scenario string, now time.Time) frontierT6HoldoutResult {
	t.Helper()
	constraints := frontierT6SelectionConstraints{
		RequiredCapabilities: []string{"go"}, MinimumContextTokens: 8192, MinimumSamples: 5,
	}
	candidates := []frontierT6SelectionCandidate{frontierT6TestSelectionCandidate(now, "runner_candidate", 12, 11, 1, 92)}
	switch scenario {
	case "cold_start":
		candidates[0] = frontierT6TestSelectionCandidate(now, "runner_cold", 4, 4, 0, 99)
	case "provider_outage":
		candidates[0].Readiness = "unavailable"
	case "capability_mismatch":
		candidates[0].Capabilities = []string{"python"}
	case "cost_spike":
		constraints.MaximumCostMicrosPer1K = 1000
	case "stable_rank":
		candidates = []frontierT6SelectionCandidate{
			frontierT6TestSelectionCandidate(now, "runner_b", 12, 9, 2, 80),
			frontierT6TestSelectionCandidate(now, "runner_a", 12, 11, 1, 92),
		}
	default:
		t.Fatalf("unsupported Frontier T6 selection scenario %q", scenario)
	}
	receipt, err := frontierT6AdviseRunnerSelection(frontierT6SelectionRequest{
		TaskClass: "go_implementation", Constraints: constraints, Candidates: candidates, Now: now,
	})
	if err != nil {
		t.Fatalf("Frontier T6 selection scenario %s: %v", scenario, err)
	}
	if !receipt.AdvisoryOnly || receipt.ActivationAllowed || receipt.ExecutionPerformed || receipt.NetworkCalls != 0 {
		t.Fatalf("Frontier T6 selection crossed its advisory boundary: %#v", receipt)
	}
	return frontierT6HoldoutResult{Decision: receipt.Decision, SelectedID: receipt.SelectedID}
}

func frontierT6HoldoutProfile(t *testing.T, scenario string, now time.Time) frontierT6HoldoutResult {
	t.Helper()
	scope := frontierT6Scope{WorkspaceID: "holdout_workspace", Project: "holdout_project"}
	request := frontierT6ProfileResolutionRequest{
		Scope: scope, AgentID: "holdout_agent", Now: now,
		Capabilities: frontierT6AgentCapabilities{
			Declared: true, AgentFamily: "generic_worker", ContextWindowTokens: 4096,
			Tools: []string{"rg", "go"}, OutputFormats: []string{"text"},
			AuthorizedSources: []string{"postgres"},
		},
	}
	switch scenario {
	case "unknown_agent":
		request.AgentID = "unknown_agent"
		request.Capabilities = frontierT6AgentCapabilities{}
	case "explicit_overflow":
		request.ExplicitFields = map[string]any{
			"model_context_window_tokens": 4096, "reserved_response_tokens": 1024, "context_budget_tokens": 3500,
		}
	case "tool_mismatch":
		request.ExplicitFields = map[string]any{"required_tools": []string{"rg", "unsupported_tool"}}
	case "low_context":
		request.Capabilities.ContextWindowTokens = 1600
	case "stored_precedence":
		store, _ := frontierT6TestStore(t, frontierT6StoreLimits{})
		stored, err := store.configureAgentProfile(
			scope,
			request.AgentID,
			map[string]any{"role": "reviewer", "context_budget_tokens": 768},
			frontierT6TestProvenance(now, "stored_profile", frontierT6TestDigest("profile-auth"), time.Hour),
			now,
		)
		if err != nil {
			t.Fatalf("configure Frontier T6 stored profile: %v", err)
		}
		request.Stored = &stored
		request.ExplicitFields = map[string]any{"role": "builder"}
	default:
		t.Fatalf("unsupported Frontier T6 profile scenario %q", scenario)
	}
	resolution, err := frontierT6ResolveAgentContextProfile(request)
	if err != nil {
		t.Fatalf("Frontier T6 profile scenario %s: %v", scenario, err)
	}
	decision := resolution.Decision
	if scenario == "low_context" {
		if resolution.Decision != "ready" || len(resolution.Adjustments) == 0 {
			t.Fatalf("low-context profile was not safely adapted: %#v", resolution)
		}
		decision = "adapted"
	}
	if scenario == "stored_precedence" {
		if resolution.Decision != "ready" || resolution.FieldSources["role"] != "explicit_request_or_cli" || anyToString(resolution.EffectiveProfile["role"]) != "builder" {
			t.Fatalf("explicit profile did not precede stored defaults: %#v", resolution)
		}
		decision = "adapted"
	}
	if resolution.AutomaticExecution {
		t.Fatalf("Frontier T6 profile resolution crossed its advisory boundary: %#v", resolution)
	}
	return frontierT6HoldoutResult{Decision: decision}
}

func frontierT6HoldoutReadyPrep(t *testing.T, now time.Time) (*frontierT6AgentFitStore, frontierT6Scope, frontierT6ContextPrepRecord) {
	t.Helper()
	store, _ := frontierT6TestStore(t, frontierT6StoreLimits{MaxPreps: 8})
	scope := frontierT6Scope{WorkspaceID: "holdout_workspace", Project: "holdout_project", SessionID: "holdout_session", AgentID: "holdout_agent"}
	request := frontierT6TestPrepRequest(scope, now, "task_holdout", "run_tests", true)
	scheduled, err := store.scheduleContextPrep(request, now)
	if err != nil || scheduled.Decision != "scheduled" || scheduled.Prep == nil {
		t.Fatalf("schedule Frontier T6 holdout prep: result=%#v err=%v", scheduled, err)
	}
	claim, found, err := store.claimContextPrep(scope, scheduled.Prep.PrepID, "holdout_worker", now.Add(time.Second))
	if err != nil || !found {
		t.Fatalf("claim Frontier T6 holdout prep: claim=%#v found=%v err=%v", claim, found, err)
	}
	completed, err := store.completeContextPrep(
		scope,
		claim.Prep.PrepID,
		claim.ClaimToken,
		frontierT6TestPrepArtifact(claim.Prep, now.Add(2*time.Second)),
		now.Add(2*time.Second),
	)
	if err != nil || completed.Status != "ready" || completed.Artifact == nil {
		t.Fatalf("complete Frontier T6 holdout prep: prep=%#v err=%v", completed, err)
	}
	return store, scope, completed
}

func frontierT6HoldoutPrep(t *testing.T, scenario string, now time.Time) frontierT6HoldoutResult {
	t.Helper()
	if scenario == "unapproved" || scenario == "low_confidence" {
		store, _ := frontierT6TestStore(t, frontierT6StoreLimits{MaxPreps: 8})
		scope := frontierT6Scope{WorkspaceID: "holdout_workspace", Project: "holdout_project", SessionID: "holdout_session", AgentID: "holdout_agent"}
		request := frontierT6TestPrepRequest(scope, now, "task_holdout", "run_tests", scenario != "unapproved")
		if scenario == "low_confidence" {
			request.PredictionConfidence = 0.5
		}
		result, err := store.scheduleContextPrep(request, now)
		if err != nil || result.Decision != "abstain" || result.ExecutionPerformed || len(store.state.ContextPreps) != 0 {
			t.Fatalf("inert Frontier T6 prep scenario %s mutated state: result=%#v err=%v", scenario, result, err)
		}
		return frontierT6HoldoutResult{Decision: result.Decision}
	}

	store, scope, prep := frontierT6HoldoutReadyPrep(t, now)
	taskID := prep.TaskID
	profileDigest := prep.EffectiveProfileDigest
	sourceGeneration := prep.SourceGeneration
	authorizationDigest := prep.AuthorizationDigest
	expectedReason := ""
	switch scenario {
	case "task_pivot":
		taskID = "different_task"
		expectedReason = "task_pivot_detected"
	case "stale_generation":
		sourceGeneration = "different_generation"
		expectedReason = "source_generation_changed"
	case "authorization_change":
		authorizationDigest = frontierT6TestDigest("different-authorization")
		expectedReason = "authorization_changed"
	default:
		t.Fatalf("unsupported Frontier T6 preparation scenario %q", scenario)
	}
	use, err := store.useContextPrep(scope, prep.PrepID, taskID, profileDigest, sourceGeneration, authorizationDigest, now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("use Frontier T6 holdout preparation: %v", err)
	}
	if use.Eligible || use.InjectionPerformed || !use.RequiresExplicitCLIUse || !frontierT6Contains(use.Reasons, expectedReason) {
		t.Fatalf("stale Frontier T6 preparation was not rejected: scenario=%s result=%#v", scenario, use)
	}
	return frontierT6HoldoutResult{Decision: "rejected"}
}

func frontierT6HoldoutSteering(t *testing.T, scenario string, now time.Time) frontierT6HoldoutResult {
	t.Helper()
	scope := frontierT6TestScope()
	limits := frontierT6StoreLimits{MaxEvents: 8, MaxDeliveries: 16}
	store, statePath := frontierT6TestStore(t, limits)
	first, _, err := store.publishSteering(frontierT6TestPublish(scope, now, "first"), now)
	if err != nil {
		t.Fatalf("publish Frontier T6 holdout steering: %v", err)
	}
	switch scenario {
	case "unsupported_harness":
		batch, err := store.claimSteering(scope, "pull_subscriber", frontierT6HarnessCapabilities{HarnessID: "poll_only"}, "", now.Add(time.Second), 8)
		if err != nil || batch.PushNative || batch.DeliveryMode != "bounded_pull_replay" || batch.FallbackReason == "" {
			t.Fatalf("unsupported harness did not receive bounded pull fallback: batch=%#v err=%v", batch, err)
		}
		return frontierT6HoldoutResult{Decision: "fallback"}
	case "duplicate_publish":
		duplicate, deduplicated, err := store.publishSteering(frontierT6TestPublish(scope, now, "first"), now.Add(time.Second))
		if err != nil || !deduplicated || duplicate.EventID != first.EventID {
			t.Fatalf("duplicate steering event was not deduplicated: event=%#v deduplicated=%v err=%v", duplicate, deduplicated, err)
		}
		return frontierT6HoldoutResult{Decision: "deduplicated"}
	case "expired_cursor":
		for index := 0; index < 10; index++ {
			request := frontierT6TestPublish(scope, now, "evict_"+string(rune('a'+index)))
			request.Message = "bounded holdout event " + string(rune('a'+index))
			if _, _, err := store.publishSteering(request, now.Add(time.Duration(index+1)*time.Second)); err != nil {
				t.Fatalf("publish bounded steering event %d: %v", index, err)
			}
		}
		batch, err := store.replaySteering(scope, first.Cursor, now.Add(time.Minute), 8)
		if !errors.Is(err, errFrontierT6CursorExpired) || !batch.CursorExpired {
			t.Fatalf("evicted steering cursor was not rejected: batch=%#v err=%v", batch, err)
		}
		return frontierT6HoldoutResult{Decision: "cursor_expired"}
	case "backpressure_retry":
		claim, err := store.claimSteering(scope, "push_subscriber", frontierT6PushCapabilities(), "", now.Add(time.Second), 8)
		if err != nil || len(claim.Deliveries) != 1 {
			t.Fatalf("claim steering delivery for retry: batch=%#v err=%v", claim, err)
		}
		item := claim.Deliveries[0]
		if err := store.failSteeringDelivery(item.DeliveryID, item.ClaimToken, "backpressure", now.Add(2*time.Second)); err != nil {
			t.Fatalf("record steering backpressure: %v", err)
		}
		retried, err := store.claimSteering(scope, "push_subscriber", frontierT6PushCapabilities(), "", now.Add(5*time.Second), 8)
		if err != nil || len(retried.Deliveries) != 1 || retried.Deliveries[0].Event.EventID != first.EventID {
			t.Fatalf("retry did not become claimable after backoff: batch=%#v err=%v", retried, err)
		}
		return frontierT6HoldoutResult{Decision: "retried"}
	case "restart_durable":
		store.close()
		reopened, err := newFrontierT6AgentFitStore(statePath, limits)
		if err != nil {
			t.Fatalf("reopen Frontier T6 steering state: %v", err)
		}
		defer reopened.close()
		batch, err := reopened.replaySteering(scope, "", now.Add(time.Second), 8)
		if err != nil || len(batch.Events) != 1 || batch.Events[0].EventID != first.EventID {
			t.Fatalf("steering event did not survive restart: batch=%#v err=%v", batch, err)
		}
		return frontierT6HoldoutResult{Decision: "durable"}
	default:
		t.Fatalf("unsupported Frontier T6 steering scenario %q", scenario)
	}
	return frontierT6HoldoutResult{}
}

func TestFrontierT6AgentFitHoldout(t *testing.T) {
	started := time.Now()
	now := time.Date(2026, time.July, 19, 0, 0, 0, 0, time.UTC)
	cases, fixtureSHA256 := frontierT6HoldoutFixture(t)
	seen := map[string]struct{}{}
	itemCounts := map[int]int{}
	passed := 0
	for _, testCase := range cases {
		if strings.TrimSpace(testCase.ID) == "" {
			t.Fatal("Frontier T6 holdout case ID is empty")
		}
		if _, duplicate := seen[testCase.ID]; duplicate {
			t.Fatalf("duplicate Frontier T6 holdout case ID %q", testCase.ID)
		}
		seen[testCase.ID] = struct{}{}
		itemCounts[testCase.Item]++
		var result frontierT6HoldoutResult
		switch testCase.Surface {
		case "steering":
			result = frontierT6HoldoutSteering(t, testCase.Scenario, now)
		case "selection":
			result = frontierT6HoldoutSelection(t, testCase.Scenario, now)
		case "profile":
			result = frontierT6HoldoutProfile(t, testCase.Scenario, now)
		case "context_prep":
			result = frontierT6HoldoutPrep(t, testCase.Scenario, now)
		default:
			t.Fatalf("%s: unsupported Frontier T6 holdout surface %q", testCase.ID, testCase.Surface)
		}
		if expected := anyToString(testCase.Expected["decision"]); result.Decision != expected {
			t.Fatalf("%s: decision=%q want=%q", testCase.ID, result.Decision, expected)
		}
		if expected := anyToString(testCase.Expected["selected_id"]); expected != "" && result.SelectedID != expected {
			t.Fatalf("%s: selected_id=%q want=%q", testCase.ID, result.SelectedID, expected)
		}
		passed++
	}
	for _, item := range []int{5, 23, 11, 12} {
		if itemCounts[item] != 5 {
			t.Fatalf("Frontier T6 holdout item %d has %d cases, want 5", item, itemCounts[item])
		}
	}
	if output := strings.TrimSpace(os.Getenv("FRONTIER_T6_AGENT_FIT_EVIDENCE_PATH")); output != "" {
		evidence := map[string]any{
			"schema_id": "frontier_t6_agent_fit_eval.v1", "version": 1,
			"tested_commit": os.Getenv("FRONTIER_T6_TESTED_COMMIT"), "tested_tree": os.Getenv("FRONTIER_T6_TESTED_TREE"),
			"holdout_schema_id": "frontier_t6_agent_fit_holdout.v1", "holdout_fixture_sha256": fixtureSHA256,
			"case_count": len(cases), "passed_count": passed, "failed_count": len(cases) - passed,
			"item_case_counts": itemCounts, "duration_ms": roundFloat(float64(time.Since(started).Microseconds())/1000, 6),
			"provider_calls": 0, "local_inference_calls": 0, "external_network_calls": 0,
			"gateway_execution_calls": 0, "automatic_runtime_activation": false, "prompt_injection_performed": false,
		}
		encoded, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			t.Fatalf("encode Frontier T6 holdout evidence: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
			t.Fatalf("create Frontier T6 evidence directory: %v", err)
		}
		if err := os.WriteFile(output, append(encoded, '\n'), 0o600); err != nil {
			t.Fatalf("write Frontier T6 holdout evidence: %v", err)
		}
	}
}
