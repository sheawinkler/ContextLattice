package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func frontierT6TestDigest(label string) string {
	return frontierT6OpaqueDigest("frontier-t6-test", label)
}

func frontierT6TestProvenance(now time.Time, generation, authorization string, freshFor time.Duration) frontierT6Provenance {
	return frontierT6Provenance{
		Source: "test_ledger", SourceID: "ledger_1", SourceGeneration: generation,
		ContentDigest: frontierT6TestDigest("content-" + generation), AuthorizationDigest: authorization,
		ObservedAt: now.UTC().Format(time.RFC3339Nano), FreshUntil: now.Add(freshFor).UTC().Format(time.RFC3339Nano),
	}
}

func frontierT6TestStore(t *testing.T, limits frontierT6StoreLimits) (*frontierT6AgentFitStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "frontier-t6.json")
	store, err := newFrontierT6AgentFitStore(path, limits)
	if err != nil {
		t.Fatalf("create Frontier T6 test store: %v", err)
	}
	t.Cleanup(store.close)
	return store, path
}

func frontierT6TestScope() frontierT6Scope {
	return frontierT6Scope{WorkspaceID: "workspace_t6", Project: "project_t6", SessionID: "session_t6", AgentID: "agent_t6"}
}

func frontierT6TestPublish(scope frontierT6Scope, now time.Time, key string) frontierT6SteeringPublishRequest {
	authorization := frontierT6TestDigest("authorization")
	return frontierT6SteeringPublishRequest{
		Scope: scope, Kind: "context_ready", Message: "Verified context is ready for the next safe boundary.",
		SuggestedAction: "Inspect the receipt before using the prepared context.", InjectionBoundary: "after_tool",
		DedupeKey: key, TTLSeconds: 1800,
		Provenance: frontierT6TestProvenance(now, "generation_1", authorization, 2*time.Hour),
	}
}

func frontierT6PushCapabilities() frontierT6HarnessCapabilities {
	return frontierT6HarnessCapabilities{
		HarnessID: "test_harness", Transport: "sse", SupportsSSE: true, SupportsEventIDs: true,
		SupportsResume: true, SupportsAck: true, InjectionBoundaries: []string{"after_tool", "idle"},
		MaxEventBytes: 4096,
	}
}

func TestFrontierT6PushDeliveryAckRetryReplayAndRestart(t *testing.T) {
	now := time.Date(2026, time.July, 18, 15, 0, 0, 0, time.UTC)
	scope := frontierT6TestScope()
	store, path := frontierT6TestStore(t, frontierT6StoreLimits{MaxEvents: 8, MaxDeliveries: 16})

	first, deduplicated, err := store.publishSteering(frontierT6TestPublish(scope, now, "first"), now)
	if err != nil || deduplicated {
		t.Fatalf("publish first steering event: deduplicated=%v err=%v", deduplicated, err)
	}
	duplicate, deduplicated, err := store.publishSteering(frontierT6TestPublish(scope, now, "first"), now.Add(time.Second))
	if err != nil || !deduplicated || duplicate.EventID != first.EventID {
		t.Fatalf("deduplicate steering event: event=%#v deduplicated=%v err=%v", duplicate, deduplicated, err)
	}

	fallback, err := store.claimSteering(scope, "subscriber_1", frontierT6HarnessCapabilities{HarnessID: "poll_only"}, "", now.Add(2*time.Second), 8)
	if err != nil || fallback.PushNative || fallback.DeliveryMode != "bounded_pull_replay" || len(fallback.Events) != 1 || fallback.FallbackReason == "" {
		t.Fatalf("unsupported harness did not receive honest pull fallback: batch=%#v err=%v", fallback, err)
	}
	if len(store.state.SteeringDeliveries) != 0 {
		t.Fatalf("pull replay incorrectly created push delivery state: %#v", store.state.SteeringDeliveries)
	}

	claimed, err := store.claimSteering(scope, "subscriber_1", frontierT6PushCapabilities(), "", now.Add(3*time.Second), 8)
	if err != nil || !claimed.PushNative || claimed.DeliveryMode != "sse_push_adapter_claim" || len(claimed.Deliveries) != 1 || claimed.ExecutionPerformed {
		t.Fatalf("claim native push delivery: batch=%#v err=%v", claimed, err)
	}
	item := claimed.Deliveries[0]
	if err := store.recordSteeringDelivered(item.DeliveryID, item.ClaimToken, now.Add(4*time.Second)); err != nil {
		t.Fatalf("record SSE write: %v", err)
	}
	if _, err := store.acknowledgeSteering(scope, "subscriber_1", item.DeliveryID, "wrong_event", now.Add(5*time.Second)); err == nil {
		t.Fatal("mismatched event acknowledgement was accepted")
	}
	ackCursor, err := store.acknowledgeSteering(scope, "subscriber_1", item.DeliveryID, first.EventID, now.Add(5*time.Second))
	if err != nil || ackCursor != first.Cursor {
		t.Fatalf("acknowledge delivery: cursor=%q err=%v", ackCursor, err)
	}
	reclaimed, err := store.claimSteering(scope, "subscriber_1", frontierT6PushCapabilities(), "", now.Add(6*time.Second), 8)
	if err != nil || len(reclaimed.Deliveries) != 0 {
		t.Fatalf("acknowledged delivery was replayed as push: batch=%#v err=%v", reclaimed, err)
	}

	secondRequest := frontierT6TestPublish(scope, now, "second")
	secondRequest.Message = "A second evidence receipt is available."
	second, _, err := store.publishSteering(secondRequest, now.Add(7*time.Second))
	if err != nil {
		t.Fatalf("publish retry event: %v", err)
	}
	retryClaim, err := store.claimSteering(scope, "subscriber_1", frontierT6PushCapabilities(), first.Cursor, now.Add(8*time.Second), 8)
	if err != nil || len(retryClaim.Deliveries) != 1 || retryClaim.Deliveries[0].Event.EventID != second.EventID {
		t.Fatalf("claim retry event: batch=%#v err=%v", retryClaim, err)
	}
	retryItem := retryClaim.Deliveries[0]
	if err := store.failSteeringDelivery(retryItem.DeliveryID, retryItem.ClaimToken, "backpressure", now.Add(9*time.Second)); err != nil {
		t.Fatalf("record retryable delivery failure: %v", err)
	}
	early, err := store.claimSteering(scope, "subscriber_1", frontierT6PushCapabilities(), first.Cursor, now.Add(10*time.Second), 8)
	if err != nil || len(early.Deliveries) != 0 {
		t.Fatalf("delivery retried before durable backoff: batch=%#v err=%v", early, err)
	}

	store.close()
	reopened, err := newFrontierT6AgentFitStore(path, frontierT6StoreLimits{MaxEvents: 8, MaxDeliveries: 16})
	if err != nil {
		t.Fatalf("reopen Frontier T6 store: %v", err)
	}
	t.Cleanup(reopened.close)
	retried, err := reopened.claimSteering(scope, "subscriber_1", frontierT6PushCapabilities(), first.Cursor, now.Add(12*time.Second), 8)
	if err != nil || len(retried.Deliveries) != 1 {
		t.Fatalf("durable retry did not survive restart: batch=%#v err=%v", retried, err)
	}
	if delivery := reopened.state.SteeringDeliveries[frontierT6DeliveryKey(frontierT6ScopeDigest(scope), frontierT6OpaqueDigest("frontier-t6-subscriber", "subscriber_1"), second.EventID)]; delivery.Attempts != 2 {
		t.Fatalf("retry attempt count was not durable: %#v", delivery)
	}

	for i := 0; i < 10; i++ {
		request := frontierT6TestPublish(scope, now, "bounded_"+string(rune('a'+i)))
		request.Message = "Bounded replay event " + string(rune('a'+i))
		if _, _, err := reopened.publishSteering(request, now.Add(time.Duration(20+i)*time.Second)); err != nil {
			t.Fatalf("publish bounded replay event %d: %v", i, err)
		}
	}
	batch, err := reopened.replaySteering(scope, first.Cursor, now.Add(time.Minute), 8)
	if !errors.Is(err, errFrontierT6CursorExpired) || !batch.CursorExpired || batch.ReplayFloor == frontierT6Cursor(0) {
		t.Fatalf("bounded replay did not reject evicted cursor: batch=%#v err=%v", batch, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("Frontier T6 state is not owner-only: mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func frontierT6TestSelectionCandidate(now time.Time, id string, samples, successes, failures int, quality float64) frontierT6SelectionCandidate {
	return frontierT6SelectionCandidate{
		CandidateID: id, Readiness: "ready", Capabilities: []string{"go", "local_repo"}, ContextWindowTokens: 32768,
		Evidence: frontierT6SelectionEvidence{
			TaskClass: "go_implementation", Verified: true, SampleCount: samples,
			SuccessCount: successes, FailureCount: failures, QualityScore: quality, Confidence: 0.9,
			CostKnown: true, CostMicrosPer1K: 1200, LatencyKnown: true, LatencyMillisP50: 800,
			Provenance: frontierT6TestProvenance(now, "selection_"+id, frontierT6TestDigest("selection-auth"), time.Hour),
		},
	}
}

func TestFrontierT6SelectionAbstainsUntilEvidenceFloorAndRanksDeterministically(t *testing.T) {
	now := time.Date(2026, time.July, 18, 16, 0, 0, 0, time.UTC)
	request := frontierT6SelectionRequest{
		TaskClass: "go_implementation", Now: now,
		Constraints: frontierT6SelectionConstraints{RequiredCapabilities: []string{"go"}, MinimumContextTokens: 8192, MinimumSamples: 5},
		Candidates:  []frontierT6SelectionCandidate{frontierT6TestSelectionCandidate(now, "runner_cold", 4, 4, 0, 99)},
	}
	cold, err := frontierT6AdviseRunnerSelection(request)
	if err != nil || cold.Decision != "abstain" || cold.SelectedID != "" || !cold.AdvisoryOnly || cold.ActivationAllowed || cold.ExecutionPerformed || cold.NetworkCalls != 0 {
		t.Fatalf("cold-start selection did not abstain safely: receipt=%#v err=%v", cold, err)
	}
	if len(cold.Candidates) != 1 || cold.Candidates[0].Eligible || !frontierT6Contains(cold.Candidates[0].Reasons, "minimum_sample_floor_not_met") {
		t.Fatalf("sample-floor evidence was not explicit: %#v", cold.Candidates)
	}

	best := frontierT6TestSelectionCandidate(now, "runner_a", 12, 11, 1, 92)
	second := frontierT6TestSelectionCandidate(now, "runner_b", 12, 9, 2, 80)
	incompatible := frontierT6TestSelectionCandidate(now, "runner_c", 40, 40, 0, 100)
	incompatible.Capabilities = []string{"python"}
	request.Candidates = []frontierT6SelectionCandidate{second, incompatible, best}
	receipt, err := frontierT6AdviseRunnerSelection(request)
	if err != nil || receipt.Decision != "recommend" || receipt.SelectedID != "runner_a" || receipt.Candidates[0].CandidateID != "runner_a" {
		t.Fatalf("deterministic runner selection failed: receipt=%#v err=%v", receipt, err)
	}
	if receipt.ConstraintsDigest == "" || receipt.ReceiptID == "" || receipt.SchemaID != frontierT6RunnerSelectionSchemaID {
		t.Fatalf("selection receipt omitted provenance contract: %#v", receipt)
	}
	for _, candidate := range receipt.Candidates {
		if candidate.CandidateID == "runner_c" && (candidate.Eligible || !strings.Contains(strings.Join(candidate.Reasons, ","), "required_capability_missing")) {
			t.Fatalf("capability mismatch became eligible: %#v", candidate)
		}
	}
	modelReceipt, err := frontierT6AdviseModelSelection(request)
	if err != nil || modelReceipt.SchemaID != frontierT6ModelSelectionSchemaID || modelReceipt.Kind != "model" || modelReceipt.SelectedID != "runner_a" {
		t.Fatalf("model receipt did not preserve the same evidence gates: receipt=%#v err=%v", modelReceipt, err)
	}

	request.Now = now.Add(2 * time.Hour)
	stale, err := frontierT6AdviseRunnerSelection(request)
	if err != nil || stale.Decision != "abstain" || stale.SelectedID != "" {
		t.Fatalf("stale selection evidence did not abstain: receipt=%#v err=%v", stale, err)
	}
}

func TestFrontierT6ProfilePrecedencePreservesExplicitConstraints(t *testing.T) {
	now := time.Date(2026, time.July, 18, 17, 0, 0, 0, time.UTC)
	store, _ := frontierT6TestStore(t, frontierT6StoreLimits{})
	scope := frontierT6Scope{WorkspaceID: "workspace_t6", Project: "project_t6"}
	stored, err := store.configureAgentProfile(scope, "agent_t6", map[string]any{
		"model_context_window_tokens": 4096, "context_budget_tokens": 1800,
		"reserved_response_tokens": 500, "required_tools": []any{"rg"},
		"push_mode": "preferred", "allowed_sources": []any{"postgres"},
	}, frontierT6TestProvenance(now, "profile_1", frontierT6TestDigest("profile-auth"), time.Hour), now)
	if err != nil {
		t.Fatalf("configure stored profile: %v", err)
	}
	resolution, err := frontierT6ResolveAgentContextProfile(frontierT6ProfileResolutionRequest{
		Scope: scope, AgentID: "agent_t6", Stored: &stored, Now: now,
		ExplicitFields: map[string]any{"context_budget_tokens": 2200, "required_tools": []any{"rg", "go"}},
		Capabilities: frontierT6AgentCapabilities{
			Declared: true, AgentFamily: "generic_worker", ContextWindowTokens: 2500,
			Tools: []string{"rg"}, OutputFormats: []string{"text"}, PushSupported: false,
			AuthorizedSources: []string{"postgres"},
		},
	})
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	if resolution.Decision != "abstain" || anyToInt(resolution.EffectiveProfile["context_budget_tokens"], 0) != 2200 {
		t.Fatalf("explicit context budget was silently reduced: %#v", resolution)
	}
	if got := frontierT6NormalizeStringList(anyToStringSlice(resolution.EffectiveProfile["required_tools"]), 64); !frontierT6Contains(got, "go") || !frontierT6Contains(got, "rg") {
		t.Fatalf("explicit required tools were silently reduced: %#v", resolution.EffectiveProfile["required_tools"])
	}
	if resolution.FieldSources["context_budget_tokens"] != "explicit_request_or_cli" || anyToString(resolution.EffectiveProfile["push_mode"]) != "pull_only" {
		t.Fatalf("profile precedence or safe push fallback is wrong: %#v", resolution)
	}
	joinedConflicts := strings.Join(resolution.Conflicts, ",")
	if !strings.Contains(joinedConflicts, "explicit_context_budget_constraints") || !strings.Contains(joinedConflicts, "explicit_required_tools_unsupported") {
		t.Fatalf("profile conflicts did not explain abstention: %#v", resolution.Conflicts)
	}

	unknown, err := frontierT6ResolveAgentContextProfile(frontierT6ProfileResolutionRequest{
		Scope: scope, AgentID: "unknown_agent", Capabilities: frontierT6AgentCapabilities{}, Now: now,
	})
	if err != nil || unknown.Decision != "fallback" || !unknown.UnknownAgent || anyToString(unknown.EffectiveProfile["agent_family"]) != "generic" || strings.Contains(strings.ToLower(anyToString(unknown.EffectiveProfile["agent_family"])), "codex") {
		t.Fatalf("unknown agent silently inherited a named-agent profile: resolution=%#v err=%v", unknown, err)
	}
}

func TestFrontierT6ProfileStoreIsExactScopedBoundedAndDurable(t *testing.T) {
	now := time.Date(2026, time.July, 18, 17, 30, 0, 0, time.UTC)
	store, path := frontierT6TestStore(t, frontierT6StoreLimits{MaxProfiles: 1})
	scope := frontierT6Scope{WorkspaceID: "workspace_one", Project: "project_one"}
	profile, err := store.configureAgentProfile(scope, "agent_one", map[string]any{"role": "reviewer"}, frontierT6TestProvenance(now, "profile_generation", frontierT6TestDigest("profile-scope-auth"), time.Hour), now)
	if err != nil {
		t.Fatalf("configure scoped profile: %v", err)
	}
	if _, err := store.configureAgentProfile(scope, "agent_two", map[string]any{"role": "builder"}, frontierT6TestProvenance(now, "profile_generation", frontierT6TestDigest("profile-scope-auth"), time.Hour), now); err == nil {
		t.Fatal("bounded profile store accepted a second exact profile")
	}
	if _, exists, err := store.agentProfile(frontierT6Scope{WorkspaceID: "workspace_two", Project: "project_one"}, "agent_one"); err != nil || exists {
		t.Fatalf("profile leaked across workspace scope: exists=%v err=%v", exists, err)
	}
	store.close()
	reopened, err := newFrontierT6AgentFitStore(path, frontierT6StoreLimits{MaxProfiles: 1})
	if err != nil {
		t.Fatalf("reopen profile store: %v", err)
	}
	t.Cleanup(reopened.close)
	loaded, exists, err := reopened.agentProfile(scope, "agent_one")
	if err != nil || !exists || loaded.ProfileDigest != profile.ProfileDigest {
		t.Fatalf("exact profile did not survive durable restart: loaded=%#v exists=%v err=%v", loaded, exists, err)
	}
}

func frontierT6TestPrepRequest(scope frontierT6Scope, now time.Time, taskID, action string, approved bool) frontierT6ContextPrepRequest {
	authorization := frontierT6TestDigest("prep-authorization")
	profileDigest := frontierT6TestDigest("effective-profile")
	return frontierT6ContextPrepRequest{
		Scope: scope, TaskID: taskID, NextActionClass: action, PredictionConfidence: 0.9,
		MinimumConfidence: 0.8, EffectiveProfileDigest: profileDigest, SourceGeneration: "source_generation_1", TTLSeconds: 900,
		Approval: frontierT6ContextPrepApproval{
			Approved: approved, ApprovalID: "approval_1", ScopeDigest: frontierT6ScopeDigest(scope),
			AuthorizationDigest: authorization, ExpiresAt: now.Add(2 * time.Hour).Format(time.RFC3339Nano),
		},
		Provenance: frontierT6TestProvenance(now, "source_generation_1", authorization, 2*time.Hour),
	}
}

func frontierT6TestPrepArtifact(prep frontierT6ContextPrepRecord, now time.Time) frontierT6ContextPrepArtifact {
	expiresAt := now.Add(10 * time.Minute)
	return frontierT6ContextPrepArtifact{
		SchemaID: frontierT6ContextPrepArtifactID, Version: 1,
		ContextPackDigest: frontierT6TestDigest("context-pack"), RetrievalReceiptDigest: frontierT6TestDigest("retrieval-receipt"),
		EffectiveProfileDigest: prep.EffectiveProfileDigest, SourceGeneration: prep.SourceGeneration,
		AuthorizationDigest: prep.AuthorizationDigest, CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: expiresAt.Format(time.RFC3339Nano),
		EvidenceRefs: []frontierT6ContextPrepEvidenceRef{{
			SourceID: "source_1", SourceGeneration: prep.SourceGeneration, ContentDigest: frontierT6TestDigest("evidence-ref"),
			AuthorizationDigest: prep.AuthorizationDigest, FreshUntil: now.Add(time.Hour).Format(time.RFC3339Nano),
		}},
	}
}

func TestFrontierT6ContextPrepIsOptInDeduplicatedExternalAndFresh(t *testing.T) {
	now := time.Date(2026, time.July, 18, 18, 0, 0, 0, time.UTC)
	store, _ := frontierT6TestStore(t, frontierT6StoreLimits{MaxPreps: 8})
	scope := frontierT6Scope{WorkspaceID: "workspace_t6", Project: "project_t6"}

	unapproved := frontierT6TestPrepRequest(scope, now, "task_one", "run_tests", false)
	result, err := store.scheduleContextPrep(unapproved, now)
	if err != nil || result.Decision != "abstain" || len(result.Reasons) != 1 || result.ExecutionPerformed || len(store.state.ContextPreps) != 0 {
		t.Fatalf("unapproved proactive preparation mutated state: result=%#v err=%v state=%#v", result, err, store.state.ContextPreps)
	}

	request := frontierT6TestPrepRequest(scope, now, "task_one", "run_tests", true)
	result, err = store.scheduleContextPrep(request, now)
	if err != nil || result.Decision != "scheduled" || result.Prep == nil || result.ExecutionOwner != "external_cli_worker" || result.ExecutionPerformed {
		t.Fatalf("schedule approved context prep: result=%#v err=%v", result, err)
	}
	prep := *result.Prep
	duplicate, err := store.scheduleContextPrep(request, now.Add(time.Second))
	if err != nil || !duplicate.Deduplicated || duplicate.Prep == nil || duplicate.Prep.PrepID != prep.PrepID || len(store.state.ContextPreps) != 1 {
		t.Fatalf("context prep was not deterministically deduplicated: result=%#v err=%v", duplicate, err)
	}

	claim, found, err := store.claimContextPrep(scope, prep.PrepID, "cli_worker_1", now.Add(2*time.Second))
	if err != nil || !found || claim.ClaimToken == "" || claim.GatewayExecutionPerformed || claim.ExecutionOwner != "external_cli_worker" {
		t.Fatalf("external worker claim failed: claim=%#v found=%v err=%v", claim, found, err)
	}
	completed, err := store.completeContextPrep(prep.PrepID, claim.ClaimToken, frontierT6TestPrepArtifact(claim.Prep, now.Add(3*time.Second)), now.Add(3*time.Second))
	if err != nil || completed.Status != "ready" || completed.Artifact == nil {
		t.Fatalf("complete context prep: prep=%#v err=%v", completed, err)
	}
	use := store.useContextPrep(scope, prep.PrepID, prep.TaskID, prep.EffectiveProfileDigest, prep.SourceGeneration, prep.AuthorizationDigest, now.Add(4*time.Second))
	if !use.Eligible || use.Artifact == nil || use.InjectionPerformed || !use.RequiresExplicitCLIUse {
		t.Fatalf("fresh context prep was not exposed as explicit-use-only: %#v", use)
	}
	stale := store.useContextPrep(scope, prep.PrepID, prep.TaskID, prep.EffectiveProfileDigest, "source_generation_2", prep.AuthorizationDigest, now.Add(4*time.Second))
	if stale.Eligible || stale.Artifact != nil || !frontierT6Contains(stale.Reasons, "source_generation_changed") {
		t.Fatalf("stale source generation exposed prepared context: %#v", stale)
	}
	unauthorized := store.useContextPrep(scope, prep.PrepID, prep.TaskID, prep.EffectiveProfileDigest, prep.SourceGeneration, frontierT6TestDigest("new-authorization"), now.Add(4*time.Second))
	if unauthorized.Eligible || unauthorized.Artifact != nil || !frontierT6Contains(unauthorized.Reasons, "authorization_changed") {
		t.Fatalf("authorization drift exposed prepared context: %#v", unauthorized)
	}
	pivoted := store.useContextPrep(scope, prep.PrepID, "task_two", prep.EffectiveProfileDigest, prep.SourceGeneration, prep.AuthorizationDigest, now.Add(4*time.Second))
	if pivoted.Eligible || pivoted.Artifact != nil || !frontierT6Contains(pivoted.Reasons, "task_pivot_detected") {
		t.Fatalf("task pivot exposed prepared context: %#v", pivoted)
	}
}

func TestFrontierT6ContextPrepRetryIsDurableAndBounded(t *testing.T) {
	now := time.Date(2026, time.July, 18, 18, 30, 0, 0, time.UTC)
	store, path := frontierT6TestStore(t, frontierT6StoreLimits{MaxPreps: 2})
	scope := frontierT6Scope{WorkspaceID: "workspace_t6", Project: "project_t6"}
	result, err := store.scheduleContextPrep(frontierT6TestPrepRequest(scope, now, "task_retry", "compile", true), now)
	if err != nil || result.Prep == nil {
		t.Fatalf("schedule retry prep: result=%#v err=%v", result, err)
	}
	claim, found, err := store.claimContextPrep(scope, result.Prep.PrepID, "worker_retry", now.Add(time.Second))
	if err != nil || !found {
		t.Fatalf("claim retry prep: claim=%#v found=%v err=%v", claim, found, err)
	}
	if err := store.failContextPrep(claim.Prep.PrepID, claim.ClaimToken, "resource_pressure", true, now.Add(2*time.Second)); err != nil {
		t.Fatalf("record prep retry: %v", err)
	}
	if _, found, err := store.claimContextPrep(scope, claim.Prep.PrepID, "worker_retry", now.Add(3*time.Second)); err != nil || found {
		t.Fatalf("prep retry ignored durable backoff: found=%v err=%v", found, err)
	}
	store.close()
	reopened, err := newFrontierT6AgentFitStore(path, frontierT6StoreLimits{MaxPreps: 2})
	if err != nil {
		t.Fatalf("reopen prep store: %v", err)
	}
	t.Cleanup(reopened.close)
	retry, found, err := reopened.claimContextPrep(scope, claim.Prep.PrepID, "worker_retry", now.Add(8*time.Second))
	if err != nil || !found || retry.Prep.Attempts != 2 {
		t.Fatalf("prep retry did not survive restart: claim=%#v found=%v err=%v", retry, found, err)
	}
	badArtifact := frontierT6TestPrepArtifact(retry.Prep, now.Add(9*time.Second))
	badArtifact.EvidenceRefs[0].AuthorizationDigest = frontierT6TestDigest("wrong-authorization")
	if _, err := reopened.completeContextPrep(retry.Prep.PrepID, retry.ClaimToken, badArtifact, now.Add(9*time.Second)); err == nil {
		t.Fatal("unauthorized artifact evidence was accepted")
	}
}

func TestFrontierT6HandlersRequireCoordinatorAuthorizationAndStayAdvisory(t *testing.T) {
	now := time.Date(2026, time.July, 18, 19, 0, 0, 0, time.UTC)
	request := frontierT6SelectionHTTPRequest{
		Kind: "runner",
		Request: frontierT6SelectionRequest{
			TaskClass:   "go_implementation",
			Constraints: frontierT6SelectionConstraints{MinimumSamples: 5},
			Candidates:  []frontierT6SelectionCandidate{frontierT6TestSelectionCandidate(now, "runner_a", 10, 9, 1, 90)},
		},
	}
	raw, _ := json.Marshal(request)
	unwired := httptest.NewRecorder()
	frontierT6AgentFitHandlers{Now: func() time.Time { return now }}.Selection(unwired, httptest.NewRequest(http.MethodPost, "/unwired", bytes.NewReader(raw)))
	if unwired.Code != http.StatusServiceUnavailable || !strings.Contains(unwired.Body.String(), "frontier_t6_authorization_unwired") {
		t.Fatalf("unwired T6 handler did not fail closed: status=%d body=%s", unwired.Code, unwired.Body.String())
	}

	authorization := frontierT6TestDigest("handler-authorization")
	handlers := frontierT6AgentFitHandlers{
		Now: func() time.Time { return now },
		Authorize: func(_ *http.Request, featureID, operation string) (frontierT6RequestAuthorization, error) {
			if featureID != frontierT6RunnerSelectionFeatureID || operation != "advise" {
				t.Fatalf("unexpected authorization request: feature=%s operation=%s", featureID, operation)
			}
			return frontierT6RequestAuthorization{Authorized: true, WorkspaceID: "workspace_t6", AuthorizationDigest: authorization}, nil
		},
	}
	recorder := httptest.NewRecorder()
	handlers.Selection(recorder, httptest.NewRequest(http.MethodPost, "/selection", bytes.NewReader(raw)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized selection handler failed: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	response := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode selection response: %v", err)
	}
	if !anyToBool(response["advisory_only"]) || anyToBool(response["activation_allowed"]) || anyToBool(response["execution_performed"]) || anyToInt(response["network_calls"], -1) != 0 {
		t.Fatalf("selection handler crossed advisory boundary: %#v", response)
	}
}
