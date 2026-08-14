package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func legacyEvaluationContinuationJob(id string) *continuationDurableJob {
	now := time.Now().UTC().Add(-time.Minute)
	request := map[string]any{
		"project": "contextlattice", "query": "legacy evaluation holdout", "traffic_class": "evaluation_holdout",
		"session_id": "evaluation-session", "agent_id": "evaluation-agent",
	}
	return &continuationDurableJob{
		SchemaVersion: continuationDurableSchemaVersion, ID: id, Source: sourceLetta, Reason: "legacy-evaluation-timeout",
		StreamToken: "legacy-evaluation-token", Fingerprint: continuationDurableFingerprint(sourceLetta, request), BaseRequest: request,
		CreatedAt: now, UpdatedAt: now, DueAt: now,
	}
}

func assertEvaluationContinuationSideEffectsAbsent(t *testing.T, s *server, token string) {
	t.Helper()
	if s == nil {
		t.Fatal("nil test server")
	}
	if s.continuationDurable != nil && s.continuationDurable.snapshot().Pending != 0 {
		t.Fatalf("evaluation holdout left durable continuation work: %#v", s.continuationDurable.snapshot())
	}
	s.continuationMu.Lock()
	history := len(s.continuationHistory[token])
	_, scoped := s.continuationSessionScopes[token]
	s.continuationMu.Unlock()
	if history != 0 || scoped {
		t.Fatalf("evaluation holdout mutated continuation event/scope state: history=%d scoped=%v", history, scoped)
	}
	if s.continuationSem != nil && len(s.continuationSem) != 0 {
		t.Fatalf("evaluation holdout acquired continuation semaphore: len=%d", len(s.continuationSem))
	}
}

func continuationEvaluationCleanupPendingCount(value any) int {
	pending, _ := continuationEvaluationCleanupPendingJobs(value)
	return len(pending)
}

func writeLegacyEvaluationCleanupReceipt(t *testing.T, q *continuationDurableQueue, receipt map[string]any) {
	t.Helper()
	receipt["digest"] = continuationEvaluationCleanupDigest(receipt)
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatalf("marshal legacy cleanup receipt: %v", err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(filepath.Join(q.dir, continuationEvaluationCleanupReceiptFile), append(raw, '\n'), true); err != nil {
		t.Fatalf("write legacy cleanup receipt: %v", err)
	}
}

func legacyEvaluationCleanupReceiptForJobs(jobs ...*continuationDurableJob) map[string]any {
	refs := make([]string, 0, len(jobs))
	for _, job := range jobs {
		refs = append(refs, continuationEvaluationCleanupJobRef(job))
	}
	return map[string]any{
		"schema_id": continuationEvaluationCleanupSchemaID, "version": continuationEvaluationCleanupVersion,
		"authority": "gateway-go", "action": "remove_stale_evaluation_continuation", "phase": "restart_load",
		"traffic_class": "evaluation_holdout", "side_effects_suppressed": true,
		"detected_jobs": len(jobs), "removed_jobs": len(jobs), "removal_failures": 0,
		"job_refs": refs, "job_refs_truncated": false, "cumulative_removed_jobs": len(jobs),
		"captured_at": nowUTCISO(),
	}
}

func TestEvaluationHoldoutContinuationSchedulingAndDurableFallbackAreSuppressed(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	token := "evaluation-side-effects"
	request := map[string]any{"project": "contextlattice", "query": "evaluation holdout", "traffic_class": "evaluation_holdout"}
	state, status, detail := s.scheduleOrDeferContinuation(http.Header{}, request, sourceTopicRollup, "timeout", token)
	if state != "suppressed" || status != "evaluation_side_effects_suppressed" || !anyToBool(detail["side_effects_suppressed"]) {
		t.Fatalf("evaluation continuation was not fail-closed: state=%q status=%q detail=%#v", state, status, detail)
	}
	assertEvaluationContinuationSideEffectsAbsent(t, s, token)
	// The event sink itself also rejects an explicitly marked evaluation event,
	// protecting callers that bypass the scheduler helper.
	s.publishContinuationEvent(token, map[string]any{"event": "queued", "traffic_class": "evaluation_holdout"})
	assertEvaluationContinuationSideEffectsAbsent(t, s, token)
}

func TestContinuationDurableRestartCleansLegacyEvaluationJobWithBoundedReceipt(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	job := legacyEvaluationContinuationJob("cont_legacy_restart_eval")
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(s.continuationDurable.dir, job.ID+".json")
	if err := os.WriteFile(jobPath, raw, 0o600); err != nil {
		t.Fatalf("write legacy evaluation job: %v", err)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 || !anyToBool(snapshot.EvaluationCleanup["side_effects_suppressed"]) || len(anyToStringSlice(snapshot.EvaluationCleanup["job_refs"])) != 1 {
		t.Fatalf("restart did not clean legacy evaluation job into a bounded receipt: %#v", snapshot)
	}
	if _, err := os.Stat(jobPath); !os.IsNotExist(err) {
		t.Fatalf("legacy evaluation job remained after guarded restart cleanup: %v", err)
	}
	receiptPath := filepath.Join(restarted.dir, continuationEvaluationCleanupReceiptFile)
	receiptRaw, err := os.ReadFile(receiptPath)
	if err != nil {
		t.Fatalf("read durable cleanup receipt: %v", err)
	}
	receipt := map[string]any{}
	if err := json.Unmarshal(receiptRaw, &receipt); err != nil || anyToString(receipt["digest"]) != continuationEvaluationCleanupDigest(receipt) {
		t.Fatalf("cleanup receipt is not durable and self-verifying: receipt=%#v err=%v", receipt, err)
	}
}

func TestContinuationDurableEvaluationCleanupRetainsJobWhenReceiptWriteFails(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	job := legacyEvaluationContinuationJob("cont_receipt_failure_eval")
	q := s.continuationDurable
	q.mu.Lock()
	if err := q.writeJobLocked(job); err != nil {
		q.mu.Unlock()
		t.Fatalf("write receipt-failure fixture: %v", err)
	}
	q.jobs[job.ID] = job
	q.fingerprintIndex[job.Fingerprint] = job.ID
	q.mu.Unlock()
	q.evaluationCleanupWriter = func(string, []byte, bool) error {
		return errors.New("injected evaluation cleanup receipt failure")
	}
	cleaned, err := q.cleanupEvaluationJob(job.ID, "drain")
	if !cleaned || err == nil {
		t.Fatalf("receipt failure was not surfaced while retaining cleanup job: cleaned=%v err=%v", cleaned, err)
	}
	if q.snapshot().Pending != 1 {
		t.Fatalf("receipt failure removed in-memory job before durable receipt: %#v", q.snapshot())
	}
	jobPath := filepath.Join(q.dir, job.ID+".json")
	if _, statErr := os.Stat(jobPath); statErr != nil {
		t.Fatalf("receipt failure removed durable job before retry: %v", statErr)
	}

	// A fresh queue models a process restart. The production writer is restored,
	// so the retained job must be receipt-bound and then removed without any
	// continuation scheduling or steering side effect.
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 {
		t.Fatalf("restart did not retry retained cleanup durably: %#v", snapshot)
	}
	if _, statErr := os.Stat(jobPath); !os.IsNotExist(statErr) {
		t.Fatalf("restart cleanup did not remove receipt-bound stale job: %v", statErr)
	}
}

func TestContinuationDurableDeleteFailurePreservesPendingCustodyAcrossJobsAndRestart(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	first := legacyEvaluationContinuationJob("cont_delete_failure_first")
	second := legacyEvaluationContinuationJob("cont_delete_failure_second")
	q := s.continuationDurable
	q.mu.Lock()
	for _, job := range []*continuationDurableJob{first, second} {
		if err := q.writeJobLocked(job); err != nil {
			q.mu.Unlock()
			t.Fatalf("write delete-failure fixture: %v", err)
		}
		q.jobs[job.ID] = job
		q.fingerprintIndex[job.Fingerprint] = job.ID
	}
	q.mu.Unlock()
	firstPath := filepath.Join(q.dir, first.ID+".json")
	q.evaluationCleanupDeleter = func(path string) error {
		if path == firstPath {
			return errors.New("injected evaluation cleanup delete failure")
		}
		return os.Remove(path)
	}
	if cleaned, err := q.cleanupEvaluationJob(first.ID, "drain"); !cleaned || err == nil {
		t.Fatalf("delete failure was not retained as pending custody: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 2 || snapshot.EvaluationCleanupTotal != 0 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || anyToString(snapshot.EvaluationCleanup["cleanup_state"]) != continuationEvaluationCleanupStatePending || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 1 {
		t.Fatalf("delete failure falsely committed or lost pending custody: %#v", snapshot)
	}
	if _, err := os.Stat(firstPath); err != nil {
		t.Fatalf("delete failure removed durable job before retry: %v", err)
	}

	if cleaned, err := q.cleanupEvaluationJob(second.ID, "drain"); !cleaned || err != nil {
		t.Fatalf("later cleanup did not complete independently while retaining first custody: cleaned=%v err=%v", cleaned, err)
	}
	snapshot := q.snapshot()
	if snapshot.Pending != 1 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 1 {
		t.Fatalf("later cleanup overwrote unresolved first custody: %#v", snapshot)
	}
	if _, err := os.Stat(filepath.Join(q.dir, second.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("later cleanup did not unlink second job: %v", err)
	}

	q.evaluationCleanupDeleter = os.Remove
	if cleaned, err := q.cleanupEvaluationJob(first.ID, "drain"); !cleaned || err != nil {
		t.Fatalf("retry did not complete retained first cleanup: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 2 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 0 {
		t.Fatalf("retry did not produce exact-once cumulative cleanup: %#v", snapshot)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 2 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 0 {
		t.Fatalf("restart double-counted or resurrected completed custody: %#v", snapshot)
	}
}

func TestContinuationDurableCrashAfterUnlinkReconcilesFinalReceiptExactlyOnce(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	job := legacyEvaluationContinuationJob("cont_final_receipt_failure_eval")
	q := s.continuationDurable
	q.mu.Lock()
	if err := q.writeJobLocked(job); err != nil {
		q.mu.Unlock()
		t.Fatalf("write final-receipt fixture: %v", err)
	}
	q.jobs[job.ID] = job
	q.fingerprintIndex[job.Fingerprint] = job.ID
	q.mu.Unlock()
	q.evaluationCleanupWriter = func(path string, raw []byte, strict bool) error {
		receipt := map[string]any{}
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return err
		}
		if anyToString(receipt["cleanup_state"]) == continuationEvaluationCleanupStateCompleted {
			return errors.New("injected final cleanup receipt failure")
		}
		return writeOwnerOnlyDurableAtomicFile(path, raw, strict)
	}
	cleaned, err := q.cleanupEvaluationJob(job.ID, "drain")
	if !cleaned || err == nil {
		t.Fatalf("final receipt failure was not surfaced after unlink: cleaned=%v err=%v", cleaned, err)
	}
	jobPath := filepath.Join(q.dir, job.ID+".json")
	if _, statErr := os.Stat(jobPath); !os.IsNotExist(statErr) {
		t.Fatalf("crash window did not unlink durable job: %v", statErr)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 0 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || anyToString(snapshot.EvaluationCleanup["cleanup_state"]) != continuationEvaluationCleanupStatePending || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 1 {
		t.Fatalf("crash window did not retain truthful pending receipt: %#v", snapshot)
	}

	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 || anyToString(snapshot.EvaluationCleanup["cleanup_state"]) != continuationEvaluationCleanupStateCompleted || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 0 {
		t.Fatalf("restart did not reconcile unlink-before-final receipt exactly once: %#v", snapshot)
	}
	restartedAgain := newContinuationDurableQueue(s.retrieval)
	if snapshot := restartedAgain.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 {
		t.Fatalf("replay double-counted reconciled cleanup: %#v", snapshot)
	}
}

func TestContinuationDurableLegacyCompletedReceiptMigratesExistingJobAfterDeleteFailure(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	job := legacyEvaluationContinuationJob("cont_legacy_completed_existing")
	q := newContinuationDurableQueue(s.retrieval)
	jobPath := filepath.Join(q.dir, job.ID+".json")
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobPath, raw, 0o600); err != nil {
		t.Fatalf("write legacy completed job: %v", err)
	}
	writeLegacyEvaluationCleanupReceipt(t, q, legacyEvaluationCleanupReceiptForJobs(job))
	q.evaluationCleanupDeleter = func(string) error {
		return errors.New("injected legacy completed delete failure")
	}
	if err := q.loadFromDisk(); err == nil {
		t.Fatal("legacy completed receipt hid delete failure")
	}
	snapshot := q.snapshot()
	if snapshot.Pending != 1 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 1 {
		t.Fatalf("legacy completed row was not migrated into truthful pending custody: %#v", snapshot)
	}
	if _, err := os.Stat(jobPath); err != nil {
		t.Fatalf("legacy completed job was removed despite injected delete failure: %v", err)
	}
	q.evaluationCleanupDeleter = os.Remove
	if cleaned, err := q.cleanupEvaluationJob(job.ID, "restart_load"); !cleaned || err != nil {
		t.Fatalf("legacy pending custody did not retry deletion: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 0 {
		t.Fatalf("legacy migration counted removal twice or left custody pending: %#v", snapshot)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 0 {
		t.Fatalf("legacy migration restart changed exact-once cumulative evidence: %#v", snapshot)
	}
	if _, err := os.Stat(jobPath); !os.IsNotExist(err) {
		t.Fatalf("legacy migrated job reappeared after restart: %v", err)
	}
}

func TestContinuationDurableLegacyFailedReceiptRecountsAfterSuccessfulRemoval(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	job := legacyEvaluationContinuationJob("cont_legacy_failed_recount")
	q := newContinuationDurableQueue(s.retrieval)
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	jobPath := filepath.Join(q.dir, job.ID+".json")
	if err := os.WriteFile(jobPath, raw, 0o600); err != nil {
		t.Fatalf("write legacy failed job: %v", err)
	}
	receipt := legacyEvaluationCleanupReceiptForJobs(job)
	receipt["removed_jobs"] = 0
	receipt["removal_failures"] = 1
	receipt["cumulative_removed_jobs"] = 0
	writeLegacyEvaluationCleanupReceipt(t, q, receipt)
	q.evaluationCleanupDeleter = func(string) error {
		return errors.New("injected legacy failed cleanup delete failure")
	}
	if err := q.loadFromDisk(); err == nil {
		t.Fatal("legacy failed receipt hid delete failure")
	}
	snapshot := q.snapshot()
	if snapshot.Pending != 1 || snapshot.EvaluationCleanupTotal != 0 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 1 {
		t.Fatalf("legacy failed row was not retained as uncounted pending custody: %#v", snapshot)
	}
	pending, valid := continuationEvaluationCleanupPendingJobs(snapshot.EvaluationCleanup["pending_jobs"])
	if !valid || len(pending) != 1 || pending[0].AlreadyCounted {
		t.Fatalf("legacy failed row was incorrectly migrated as already counted: valid=%v pending=%#v", valid, pending)
	}
	q.evaluationCleanupDeleter = os.Remove
	if cleaned, err := q.cleanupEvaluationJob(job.ID, "restart_load"); !cleaned || err != nil {
		t.Fatalf("legacy failed row did not recount after successful removal: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 {
		t.Fatalf("successful removal did not increment legacy failed row exactly once: %#v", snapshot)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 {
		t.Fatalf("legacy failed row replayed or lost its successful count after restart: %#v", snapshot)
	}
}

func TestContinuationDurableLegacyFailureOverridesNonzeroRemovedCount(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	job := legacyEvaluationContinuationJob("cont_legacy_ambiguous_recount")
	q := newContinuationDurableQueue(s.retrieval)
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
		t.Fatalf("write legacy ambiguous job: %v", err)
	}
	receipt := legacyEvaluationCleanupReceiptForJobs(job)
	receipt["removed_jobs"] = 1
	receipt["removal_failures"] = 1
	receipt["cumulative_removed_jobs"] = 1
	writeLegacyEvaluationCleanupReceipt(t, q, receipt)
	q.evaluationCleanupDeleter = func(string) error {
		return errors.New("injected legacy ambiguous cleanup delete failure")
	}
	if err := q.loadFromDisk(); err == nil {
		t.Fatal("legacy ambiguous receipt hid delete failure")
	}
	snapshot := q.snapshot()
	if snapshot.Pending != 1 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 {
		t.Fatalf("legacy failure did not remain truthful despite nonzero removed count: %#v", snapshot)
	}
	pending, valid := continuationEvaluationCleanupPendingJobs(snapshot.EvaluationCleanup["pending_jobs"])
	if !valid || len(pending) != 1 || pending[0].AlreadyCounted {
		t.Fatalf("legacy ambiguous row was incorrectly migrated as already counted: valid=%v pending=%#v", valid, pending)
	}
	q.evaluationCleanupDeleter = os.Remove
	if cleaned, err := q.cleanupEvaluationJob(job.ID, "restart_load"); !cleaned || err != nil {
		t.Fatalf("legacy ambiguous row did not count after successful removal: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 2 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 {
		t.Fatalf("legacy ambiguous row did not increment exactly once: %#v", snapshot)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 2 {
		t.Fatalf("legacy ambiguous row replayed after restart: %#v", snapshot)
	}
}

func TestContinuationDurableLegacyAggregateProofRejectsAmbiguousRows(t *testing.T) {
	testCases := []struct {
		name   string
		mutate func(map[string]any)
		count  int
	}{
		{name: "missing_detected_jobs", mutate: func(receipt map[string]any) {
			delete(receipt, "detected_jobs")
		}, count: 1},
		{name: "invalid_integer_type", mutate: func(receipt map[string]any) {
			receipt["removed_jobs"] = "1"
		}, count: 1},
		{name: "truncated_refs", mutate: func(receipt map[string]any) {
			receipt["job_refs_truncated"] = true
		}, count: 1},
		{name: "incomplete_multi_ref_aggregate", mutate: func(receipt map[string]any) {
			receipt["detected_jobs"] = 1
		}, count: 2},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			s := newContinuationDurableTestServer(t)
			q := newContinuationDurableQueue(s.retrieval)
			jobs := make([]*continuationDurableJob, 0, testCase.count)
			for index := 0; index < testCase.count; index++ {
				job := legacyEvaluationContinuationJob(fmt.Sprintf("cont_legacy_proof_%s_%d", testCase.name, index))
				jobs = append(jobs, job)
				raw, err := json.Marshal(job)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
					t.Fatalf("write ambiguous legacy job: %v", err)
				}
			}
			receipt := legacyEvaluationCleanupReceiptForJobs(jobs...)
			testCase.mutate(receipt)
			writeLegacyEvaluationCleanupReceipt(t, q, receipt)
			q.evaluationCleanupDeleter = func(string) error {
				return errors.New("injected ambiguous legacy cleanup delete failure")
			}
			if err := q.loadFromDisk(); err == nil {
				t.Fatal("ambiguous legacy receipt hid pending cleanup failure")
			}
			before := q.snapshot()
			if before.Pending != testCase.count || continuationEvaluationCleanupPendingCount(before.EvaluationCleanup["pending_jobs"]) != testCase.count {
				t.Fatalf("ambiguous legacy rows were not retained as pending custody: %#v", before)
			}
			pending, valid := continuationEvaluationCleanupPendingJobs(before.EvaluationCleanup["pending_jobs"])
			if !valid || len(pending) != testCase.count {
				t.Fatalf("ambiguous legacy custody was malformed: valid=%v pending=%#v", valid, pending)
			}
			for _, item := range pending {
				if item.AlreadyCounted {
					t.Fatalf("ambiguous legacy ref was incorrectly trusted: %#v", item)
				}
			}
			q.evaluationCleanupDeleter = os.Remove
			for _, job := range jobs {
				if cleaned, err := q.cleanupEvaluationJob(job.ID, "legacy_retry"); !cleaned || err != nil {
					t.Fatalf("ambiguous legacy row did not complete exactly once: cleaned=%v err=%v", cleaned, err)
				}
			}
			after := q.snapshot()
			if after.Pending != 0 || after.EvaluationCleanupTotal != before.EvaluationCleanupTotal+testCase.count {
				t.Fatalf("ambiguous legacy rows did not increment exactly once: before=%#v after=%#v", before, after)
			}
			restarted := newContinuationDurableQueue(s.retrieval)
			if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != after.EvaluationCleanupTotal {
				t.Fatalf("ambiguous legacy rows replayed after restart: %#v", snapshot)
			}
		})
	}
}

func TestContinuationDurableMarkerAncestorSyncFailureRecoversOnRestart(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprintf("ancestor_sync_%d", failAt), func(t *testing.T) {
			s := newContinuationDurableTestServer(t)
			q := newContinuationDurableQueue(s.retrieval)
			job := legacyEvaluationContinuationJob(fmt.Sprintf("cont_marker_ancestor_sync_%d", failAt))
			raw, err := json.Marshal(job)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
				t.Fatal(err)
			}
			syncCalls := 0
			q.evaluationCleanupDirectorySync = func(path string) error {
				syncCalls++
				if syncCalls == failAt {
					return fmt.Errorf("injected ancestor sync failure %d", failAt)
				}
				return syncOwnerOnlyDirectory(path)
			}
			if err := q.loadFromDisk(); err == nil {
				t.Fatal("ancestor sync failure was hidden")
			}
			failed := q.snapshot()
			if failed.Pending != 0 || failed.EvaluationCleanupTotal != 0 || continuationEvaluationCleanupPendingCount(failed.EvaluationCleanup["pending_jobs"]) != 1 {
				t.Fatalf("failed marker transaction lost pending custody: %#v", failed)
			}
			if _, err := os.Stat(q.evaluationCleanupMarkerPathLocked(job)); !os.IsNotExist(err) {
				t.Fatalf("marker became visible before ancestor durability completed: %v", err)
			}
			restarted := newContinuationDurableQueue(s.retrieval)
			recovered := restarted.snapshot()
			if recovered.Pending != 0 || recovered.EvaluationCleanupTotal != 1 || recovered.EvaluationCleanupMarkerIndex["state"] != continuationEvaluationCleanupMarkerIndexStateReady || anyToInt(recovered.EvaluationCleanupMarkerIndex["marker_count"], 0) != 1 {
				t.Fatalf("restart did not reconcile ancestor crash window exactly once: %#v", recovered)
			}
			if _, err := os.Stat(restarted.evaluationCleanupMarkerPathLocked(job)); err != nil {
				t.Fatalf("recovered marker missing: %v", err)
			}
		})
	}
}

func TestContinuationDurableMarkerDescriptorReadRejectsReplacementAndSymlink(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	job := legacyEvaluationContinuationJob("cont_marker_descriptor_binding")
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := q.loadFromDisk(); err != nil {
		t.Fatalf("initial marker creation failed: %v", err)
	}
	markerPath := q.evaluationCleanupMarkerPathLocked(job)
	original, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	_, _, markerErr := q.loadEvaluationCleanupMarkerLocked(job)
	q.mu.Unlock()
	if markerErr != nil {
		t.Fatalf("initial descriptor-bound marker read failed: %v", markerErr)
	}
	if err := os.WriteFile(markerPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	_, _, markerErr = q.loadEvaluationCleanupMarkerLocked(job)
	q.mu.Unlock()
	if markerErr == nil {
		t.Fatal("replaced marker was accepted")
	}
	if err := os.WriteFile(markerPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	external := filepath.Join(q.dir, "external-marker.json")
	if err := os.WriteFile(external, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, markerPath); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	_, _, markerErr = q.loadEvaluationCleanupMarkerLocked(job)
	q.mu.Unlock()
	if markerErr == nil {
		t.Fatal("marker symlink was accepted")
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, original, 0o600); err != nil {
		t.Fatal(err)
	}
	shardPath := filepath.Dir(markerPath)
	shardName := filepath.Base(shardPath)
	indexPath := filepath.Dir(shardPath)
	externalShard := filepath.Join(q.dir, "external-shard")
	if err := os.Mkdir(externalShard, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shardPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(externalShard, filepath.Base(markerPath)), original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalShard, filepath.Join(indexPath, shardName)); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	_, _, markerErr = q.loadEvaluationCleanupMarkerLocked(job)
	q.mu.Unlock()
	if markerErr == nil {
		t.Fatal("marker ancestor symlink was accepted")
	}
}

func TestContinuationDurableMarkerIndexLimitFailsClosedBeforeUnlink(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	q.evaluationCleanupMarkerMaxCount = 1
	jobs := []*continuationDurableJob{
		legacyEvaluationContinuationJob("cont_marker_limit_a"),
		legacyEvaluationContinuationJob("cont_marker_limit_b"),
	}
	for _, job := range jobs {
		raw, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.loadFromDisk(); err == nil {
		t.Fatal("marker index limit did not fail closed")
	}
	snapshot := q.snapshot()
	if snapshot.Pending != 1 || snapshot.EvaluationCleanupTotal != 1 || snapshot.EvaluationCleanupMarkerIndex["state"] != continuationEvaluationCleanupMarkerIndexStateLimit || anyToInt(snapshot.EvaluationCleanupMarkerIndex["marker_count"], 0) != 1 {
		t.Fatalf("marker limit telemetry or custody was incorrect: %#v", snapshot)
	}
	if _, err := os.Stat(filepath.Join(q.dir, jobs[1].ID+".json")); err != nil {
		t.Fatalf("marker limit failed to retain uncommitted job: %v", err)
	}
}

func TestContinuationDurableMarkerCapMigrationCommitRestartAndRollback(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	job := legacyEvaluationContinuationJob("cont_marker_cap_migration")
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := q.loadFromDisk(); err != nil {
		t.Fatalf("initial marker setup failed: %v", err)
	}
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	receipt, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 64, NewMaxBytes: oldBytes + 1024*1024,
		OperatorRef: "operator-migration-test", Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization,
		NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner, Reason: "verified storage extension",
		AuthenticatedPrincipal: "test-operator", WorkspaceID: "test-workspace",
	})
	if err != nil {
		t.Fatalf("cap migration failed: %v", err)
	}
	planDigest := anyToString(receipt["plan_digest"])
	if planDigest == "" || anyToString(receipt["state"]) != continuationEvaluationCleanupMarkerMigrationStateCommitted {
		t.Fatalf("migration receipt was not committed: %#v", receipt)
	}
	if q.snapshot().EvaluationCleanupTotal != 1 || q.evaluationCleanupMarkerMaxCount != oldCount+64 {
		t.Fatalf("migration changed exact-once custody: %#v", q.snapshot())
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if restarted.evaluationCleanupMarkerMaxCount != oldCount+64 || restarted.snapshot().EvaluationCleanupTotal != 1 {
		t.Fatalf("committed cap migration did not replay across restart: %#v", restarted.snapshot())
	}
	if err := restarted.rollbackEvaluationCleanupMarkerIndexCaps("operator-migration-test", planDigest, "test-operator", "test-workspace"); err != nil {
		t.Fatalf("safe rollback failed: %v", err)
	}
	if restarted.evaluationCleanupMarkerMaxCount != oldCount || restarted.snapshot().EvaluationCleanupTotal != 1 {
		t.Fatalf("rollback changed exact-once custody or cap: %#v", restarted.snapshot())
	}
	if restarted.evaluationCleanupMarkerMigrationReceiptDigest != "" {
		t.Fatalf("generation-zero rollback retained rollback receipt as active receipt: %q", restarted.evaluationCleanupMarkerMigrationReceiptDigest)
	}
	rolledBack := newContinuationDurableQueue(s.retrieval)
	if rolledBack.evaluationCleanupMarkerMaxCount != oldCount || rolledBack.snapshot().EvaluationCleanupTotal != 1 {
		t.Fatalf("rollback did not survive restart: %#v", rolledBack.snapshot())
	}
}

func TestContinuationDurableMarkerCapMigrationPreparedPlanRollsBackAfterReceiptFailure(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	job := legacyEvaluationContinuationJob("cont_marker_cap_migration_receipt_fault")
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := q.loadFromDisk(); err != nil {
		t.Fatalf("initial marker setup failed: %v", err)
	}
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	q.evaluationCleanupMarkerWriter = func(root, index, shard, name string, content []byte) error {
		if strings.HasPrefix(name, continuationEvaluationCleanupMarkerMigrationReceiptPrefix) {
			return errors.New("injected migration receipt failure")
		}
		return writeEvaluationCleanupMarkerDurable(root, index, shard, name, content)
	}
	_, err = q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 8, NewMaxBytes: oldBytes + 1024,
		OperatorRef: "operator-migration-fault", Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization,
		NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner, Reason: "fault proof",
		AuthenticatedPrincipal: "test-operator", WorkspaceID: "test-workspace",
	})
	if err == nil {
		t.Fatal("receipt failure was hidden")
	}
	planRaw, err := readEvaluationCleanupMarkerFileBounded(q.dir, continuationEvaluationCleanupIndexDirectory, "", continuationEvaluationCleanupMarkerMigrationPlanRecordFile(q.evaluationCleanupMarkerMigrationPlanDigest), continuationEvaluationCleanupMarkerMaxBytes)
	if err != nil {
		t.Fatalf("prepared plan was not retained: %v", err)
	}
	plan := map[string]any{}
	if err := decodeStrictJSONMap(planRaw, &plan); err != nil {
		t.Fatal(err)
	}
	q.evaluationCleanupMarkerWriter = writeEvaluationCleanupMarkerDurable
	if err := q.rollbackEvaluationCleanupMarkerIndexCaps("operator-migration-fault", anyToString(plan["plan_digest"]), "test-operator", "test-workspace"); err != nil {
		t.Fatalf("prepared migration rollback failed: %v", err)
	}
	if q.evaluationCleanupMarkerMaxCount != oldCount || q.snapshot().EvaluationCleanupTotal != 1 {
		t.Fatalf("prepared rollback changed cap or exact-once custody: %#v", q.snapshot())
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if restarted.evaluationCleanupMarkerMaxCount != oldCount || restarted.snapshot().EvaluationCleanupTotal != 1 {
		t.Fatalf("prepared rollback did not survive restart: %#v", restarted.snapshot())
	}
}

func TestContinuationDurableMarkerCapMigrationRepeatsWithGenerationCAS(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	request := func(count int, bytes int64, expected int64, operator string) map[string]any {
		t.Helper()
		receipt, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
			NewMaxCount: count, NewMaxBytes: bytes, OperatorRef: operator,
			Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization,
			NativeOwner:   continuationEvaluationCleanupMarkerMigrationNativeOwner, Reason: "repeat extension proof",
			ExpectedGeneration: expected, AuthenticatedPrincipal: "repeat-operator", WorkspaceID: "repeat-workspace",
		})
		if err != nil {
			t.Fatalf("repeated migration failed: %v", err)
		}
		return receipt
	}
	first := request(oldCount+4, oldBytes+4096, 0, "repeat-1")
	if anyToInt(first["generation"], 0) != 1 {
		t.Fatalf("first migration generation was not one: %#v", first)
	}
	second := request(oldCount+8, oldBytes+8192, 1, "repeat-2")
	if anyToInt(second["generation"], 0) != 2 || q.evaluationCleanupMarkerMigrationGeneration != 2 {
		t.Fatalf("second migration did not advance generation: %#v", second)
	}
	if _, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 12, NewMaxBytes: oldBytes + 12288, OperatorRef: "stale-cas",
		Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
		Reason: "stale generation", ExpectedGeneration: 1, AuthenticatedPrincipal: "repeat-operator", WorkspaceID: "repeat-workspace",
	}); err == nil {
		t.Fatal("stale migration generation was accepted")
	}
	currentPlan := q.evaluationCleanupMarkerMigrationPlanDigest
	if err := q.rollbackEvaluationCleanupMarkerIndexCaps("repeat-2", currentPlan, "repeat-operator", "repeat-workspace"); err != nil {
		t.Fatalf("repeated migration rollback failed: %v", err)
	}
	if q.evaluationCleanupMarkerMaxCount != oldCount+4 || q.evaluationCleanupMarkerMigrationGeneration != 3 || q.evaluationCleanupMarkerMigrationTargetGeneration != 1 {
		t.Fatalf("rollback did not restore prior target with a fresh epoch: cap=%d generation=%d target=%d", q.evaluationCleanupMarkerMaxCount, q.evaluationCleanupMarkerMigrationGeneration, q.evaluationCleanupMarkerMigrationTargetGeneration)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if restarted.evaluationCleanupMarkerMaxCount != oldCount+4 || restarted.evaluationCleanupMarkerMigrationGeneration != 3 || restarted.evaluationCleanupMarkerMigrationTargetGeneration != 1 {
		t.Fatalf("repeated migration rollback was not durable: cap=%d generation=%d target=%d", restarted.evaluationCleanupMarkerMaxCount, restarted.evaluationCleanupMarkerMigrationGeneration, restarted.evaluationCleanupMarkerMigrationTargetGeneration)
	}
	if _, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 12, NewMaxBytes: oldBytes + 12288, OperatorRef: "stale-after-rollback",
		Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
		Reason: "stale rollback generation", ExpectedGeneration: 1, AuthenticatedPrincipal: "repeat-operator", WorkspaceID: "repeat-workspace",
	}); err == nil {
		t.Fatal("stale pre-rollback generation was accepted after rollback")
	}
	if _, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 12, NewMaxBytes: oldBytes + 12288, OperatorRef: "post-rollback-extension",
		Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
		Reason: "fresh rollback generation", ExpectedGeneration: 3, AuthenticatedPrincipal: "repeat-operator", WorkspaceID: "repeat-workspace",
	}); err != nil {
		t.Fatalf("fresh post-rollback generation was rejected: %v", err)
	}
}

func TestContinuationDurableMarkerCapMigrationIndependentHandlesUseDurableCAS(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	first := newContinuationDurableQueue(s.retrieval)
	second := newContinuationDurableQueue(s.retrieval)
	oldCount := first.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := first.evaluationCleanupMarkerByteLimitLocked()
	request := func(q *continuationDurableQueue, operator string) error {
		_, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
			NewMaxCount: oldCount + 4, NewMaxBytes: oldBytes + 4096, OperatorRef: operator,
			Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
			Reason: "independent handle CAS proof", ExpectedGeneration: 0, AuthenticatedPrincipal: "independent-operator", WorkspaceID: "independent-workspace",
		})
		return err
	}
	if err := request(first, "independent-first"); err != nil {
		t.Fatalf("first independent handle migration failed: %v", err)
	}
	if err := request(second, "independent-stale"); err == nil {
		t.Fatal("second independent handle reused a stale in-memory generation")
	}
}

func TestContinuationDurableMarkerCapMigrationRejectsRehashedRollbackPointerTamper(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := s.continuationDurable
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	receipt, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 4, NewMaxBytes: oldBytes + 4096, OperatorRef: "tamper-operator",
		Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
		Reason: "rollback pointer binding proof", ExpectedGeneration: 0, AuthenticatedPrincipal: "tamper-operator", WorkspaceID: "tamper-workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.rollbackEvaluationCleanupMarkerIndexCaps("tamper-operator", anyToString(receipt["plan_digest"]), "tamper-operator", "tamper-workspace"); err != nil {
		t.Fatal(err)
	}
	pointer, err := q.readActiveMarkerMigrationPointerLocked()
	if err != nil {
		t.Fatal(err)
	}
	pointer["rollback_digest"] = "sha256:" + strings.Repeat("0", 64)
	delete(pointer, "digest")
	pointer["digest"] = continuationEvaluationCleanupMarkerMigrationActivePointerDigest(pointer)
	raw, err := json.Marshal(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEvaluationCleanupMarkerDurable(q.dir, continuationEvaluationCleanupIndexDirectory, "", continuationEvaluationCleanupMarkerMigrationActivePointerFile, append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if err := restarted.loadFromDisk(); err == nil || !strings.Contains(err.Error(), "does not bind the rollback receipt") {
		t.Fatalf("recomputed active pointer digest did not expose rollback-record tamper: %v", err)
	}
}

func TestContinuationDurableMarkerCapMigrationUpgradesLegacyPointerTarget(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := s.continuationDurable
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	receipt, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 4, NewMaxBytes: oldBytes + 4096, OperatorRef: "legacy-pointer-operator",
		Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
		Reason: "legacy pointer target proof", ExpectedGeneration: 0, AuthenticatedPrincipal: "legacy-pointer-operator", WorkspaceID: "legacy-pointer-workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	pointer, err := q.readActiveMarkerMigrationPointerLocked()
	if err != nil {
		t.Fatal(err)
	}
	delete(pointer, "target_generation")
	delete(pointer, "digest")
	pointer["digest"] = continuationEvaluationCleanupMarkerMigrationActivePointerDigest(pointer)
	raw, err := json.Marshal(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEvaluationCleanupMarkerDurable(q.dir, continuationEvaluationCleanupIndexDirectory, "", continuationEvaluationCleanupMarkerMigrationActivePointerFile, append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	upgraded, err := restarted.readActiveMarkerMigrationPointerLocked()
	if err != nil {
		t.Fatal(err)
	}
	if target, ok := continuationEvaluationCleanupMarkerMigrationPointerTargetGeneration(upgraded); !ok || target != 1 || restarted.evaluationCleanupMarkerMigrationGeneration != 1 || anyToString(receipt["plan_digest"]) != restarted.evaluationCleanupMarkerMigrationPlanDigest {
		t.Fatalf("legacy committed pointer was not upgraded safely: pointer=%#v generation=%d target=%d", upgraded, restarted.evaluationCleanupMarkerMigrationGeneration, restarted.evaluationCleanupMarkerMigrationTargetGeneration)
	}
}

func TestContinuationDurableMarkerCapMigrationUpgradesLegacyRollbackEpoch(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := s.continuationDurable
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	receipt, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 4, NewMaxBytes: oldBytes + 4096, OperatorRef: "legacy-rollback-operator",
		Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
		Reason: "legacy rollback epoch proof", ExpectedGeneration: 0, AuthenticatedPrincipal: "legacy-rollback-operator", WorkspaceID: "legacy-rollback-workspace",
	})
	if err != nil {
		t.Fatal(err)
	}
	planDigest := anyToString(receipt["plan_digest"])
	if err := q.rollbackEvaluationCleanupMarkerIndexCaps("legacy-rollback-operator", planDigest, "legacy-rollback-operator", "legacy-rollback-workspace"); err != nil {
		t.Fatal(err)
	}
	rollbackName := continuationEvaluationCleanupMarkerMigrationRollbackFile(planDigest)
	rollback, err := q.readMarkerMigrationRecordLocked(rollbackName)
	if err != nil {
		t.Fatal(err)
	}
	delete(rollback, "rollback_generation")
	delete(rollback, "rollback_digest")
	rollback["rollback_digest"] = continuationEvaluationCleanupMarkerMigrationDigest(rollback, "rollback_digest")
	rollbackRaw, err := json.Marshal(rollback)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEvaluationCleanupMarkerDurable(q.dir, continuationEvaluationCleanupIndexDirectory, "", rollbackName, append(rollbackRaw, '\n')); err != nil {
		t.Fatal(err)
	}
	pointer, err := q.readActiveMarkerMigrationPointerLocked()
	if err != nil {
		t.Fatal(err)
	}
	delete(pointer, "target_generation")
	pointer["generation"] = 0
	pointer["rollback_digest"] = rollback["rollback_digest"]
	delete(pointer, "digest")
	pointer["digest"] = continuationEvaluationCleanupMarkerMigrationActivePointerDigest(pointer)
	pointerRaw, err := json.Marshal(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeEvaluationCleanupMarkerDurable(q.dir, continuationEvaluationCleanupIndexDirectory, "", continuationEvaluationCleanupMarkerMigrationActivePointerFile, append(pointerRaw, '\n')); err != nil {
		t.Fatal(err)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if restarted.evaluationCleanupMarkerMigrationGeneration != 2 || restarted.evaluationCleanupMarkerMigrationTargetGeneration != 0 || restarted.evaluationCleanupMarkerMaxCount != oldCount {
		t.Fatalf("legacy rollback pointer did not consume a fresh epoch: generation=%d target=%d cap=%d", restarted.evaluationCleanupMarkerMigrationGeneration, restarted.evaluationCleanupMarkerMigrationTargetGeneration, restarted.evaluationCleanupMarkerMaxCount)
	}
}

func TestContinuationDurableMarkerCapMigrationCrossProcessCAS(t *testing.T) {
	if os.Getenv("CONTEXTLATTICE_MARKER_MIGRATION_LOCK_HOLDER") == "1" {
		unlock, err := acquireEvaluationCleanupMarkerMigrationOwnerLock(os.Getenv("CONTEXTLATTICE_MARKER_MIGRATION_DIR"))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer unlock()
		if err := os.WriteFile(os.Getenv("CONTEXTLATTICE_MARKER_MIGRATION_READY"), []byte("ready\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(os.Getenv("CONTEXTLATTICE_MARKER_MIGRATION_RELEASE")); err == nil {
				os.Exit(0)
			}
			if time.Now().After(deadline) {
				fmt.Fprintln(os.Stderr, "owner lock holder timed out")
				os.Exit(1)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if os.Getenv("CONTEXTLATTICE_MARKER_MIGRATION_SUBPROCESS") == "1" {
		dir := os.Getenv("CONTEXTLATTICE_MARKER_MIGRATION_DIR")
		policy := retrievalPolicy{
			continuationDurableEnabled:         true,
			continuationDurableDir:             dir,
			continuationDurableMaxPending:      64,
			continuationDurableMaxPendingBySrc: 2,
			continuationDurableMaxAttempts:     4,
			continuationDurableDrainBatch:      8,
			continuationDurablePollInterval:    250 * time.Millisecond,
			continuationDurableRetryBase:       500 * time.Millisecond,
			continuationDurableRetryMax:        5 * time.Second,
		}
		q := newContinuationDurableQueue(policy)
		oldCount := q.evaluationCleanupMarkerCountLimitLocked()
		oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
		_, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
			NewMaxCount: oldCount + 4, NewMaxBytes: oldBytes + 4096, OperatorRef: "subprocess-operator",
			Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization, NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner,
			Reason: "cross process CAS proof", ExpectedGeneration: 0, AuthenticatedPrincipal: "subprocess-operator", WorkspaceID: "subprocess-workspace",
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix subprocess proof uses the platform owner lock")
	}
	s := newContinuationDurableTestServer(t)
	command := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestContinuationDurableMarkerCapMigrationCrossProcessCAS$", "-test.v")
		cmd.Env = append(os.Environ(), "CONTEXTLATTICE_MARKER_MIGRATION_SUBPROCESS=1", "CONTEXTLATTICE_MARKER_MIGRATION_DIR="+s.continuationDurable.dir)
		return cmd
	}
	holderReady := filepath.Join(t.TempDir(), "owner-ready")
	holderRelease := filepath.Join(t.TempDir(), "owner-release")
	holder := exec.Command(os.Args[0], "-test.run", "^TestContinuationDurableMarkerCapMigrationCrossProcessCAS$", "-test.v")
	holder.Env = append(os.Environ(),
		"CONTEXTLATTICE_MARKER_MIGRATION_LOCK_HOLDER=1",
		"CONTEXTLATTICE_MARKER_MIGRATION_DIR="+s.continuationDurable.dir,
		"CONTEXTLATTICE_MARKER_MIGRATION_READY="+holderReady,
		"CONTEXTLATTICE_MARKER_MIGRATION_RELEASE="+holderRelease,
	)
	var holderOutput bytes.Buffer
	holder.Stdout, holder.Stderr = &holderOutput, &holderOutput
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	readyDeadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(holderReady); err == nil {
			break
		}
		if time.Now().After(readyDeadline) {
			_ = holder.Process.Kill()
			_ = holder.Wait()
			t.Fatalf("owner lock holder did not become ready: %s", holderOutput.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	blocked := command()
	var blockedOutput bytes.Buffer
	blocked.Stdout, blocked.Stderr = &blockedOutput, &blockedOutput
	if err := blocked.Run(); err == nil || !strings.Contains(blockedOutput.String(), "owner-only migration worker already active") {
		_ = holder.Process.Kill()
		_ = holder.Wait()
		t.Fatalf("independent process crossed the owner lock: err=%v output=%s", err, blockedOutput.String())
	}
	if err := os.WriteFile(holderRelease, []byte("release\n"), 0o600); err != nil {
		_ = holder.Process.Kill()
		_ = holder.Wait()
		t.Fatal(err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("owner lock holder failed: %v %s", err, holderOutput.String())
	}
	allowed := command()
	var allowedOutput bytes.Buffer
	allowed.Stdout, allowed.Stderr = &allowedOutput, &allowedOutput
	if err := allowed.Run(); err != nil {
		t.Fatalf("process did not acquire owner lock after release: %v %s", err, allowedOutput.String())
	}

	s = newContinuationDurableTestServer(t)
	command = func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestContinuationDurableMarkerCapMigrationCrossProcessCAS$", "-test.v")
		cmd.Env = append(os.Environ(), "CONTEXTLATTICE_MARKER_MIGRATION_SUBPROCESS=1", "CONTEXTLATTICE_MARKER_MIGRATION_DIR="+s.continuationDurable.dir)
		return cmd
	}
	first, second := command(), command()
	var firstOutput, secondOutput bytes.Buffer
	first.Stdout, first.Stderr = &firstOutput, &firstOutput
	second.Stdout, second.Stderr = &secondOutput, &secondOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	if err := second.Start(); err != nil {
		_ = first.Process.Kill()
		_ = first.Wait()
		t.Fatal(err)
	}
	firstErr, secondErr := first.Wait(), second.Wait()
	successes := 0
	if firstErr == nil {
		successes++
	}
	if secondErr == nil {
		successes++
	}
	if successes != 1 {
		t.Fatalf("independent subprocesses did not produce exactly one durable CAS winner: first=%v %s second=%v %s", firstErr, firstOutput.String(), secondErr, secondOutput.String())
	}
}

func TestContinuationDurableMarkerCapMigrationPointerFailureStagesInMemoryState(t *testing.T) {
	t.Run("fail-before-commit-restart-reconciles", func(t *testing.T) {
		s := newContinuationDurableTestServer(t)
		q := newContinuationDurableQueue(s.retrieval)
		oldCount := q.evaluationCleanupMarkerCountLimitLocked()
		oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
		newCount := oldCount + 10
		newBytes := oldBytes + 16384
		failed := false
		q.evaluationCleanupMarkerWriter = func(root, index, shard, name string, content []byte) error {
			if name == continuationEvaluationCleanupMarkerMigrationActivePointerFile && bytes.Contains(content, []byte(`"state":"committed"`)) && !failed {
				failed = true
				return errors.New("injected committed pointer failure before durable write")
			}
			return writeEvaluationCleanupMarkerDurable(root, index, shard, name, content)
		}
		_, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
			NewMaxCount: newCount, NewMaxBytes: newBytes, OperatorRef: "pointer-before", Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization,
			NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner, Reason: "pointer failure proof", AuthenticatedPrincipal: "pointer-operator", WorkspaceID: "pointer-workspace",
		})
		if err == nil || !failed {
			t.Fatalf("expected committed pointer failure, err=%v failed=%v", err, failed)
		}
		if q.evaluationCleanupMarkerMaxCount != oldCount || q.evaluationCleanupMarkerMaxBytes != oldBytes || q.evaluationCleanupMarkerMigrationState != continuationEvaluationCleanupMarkerMigrationStatePendingRecovery {
			t.Fatalf("failed commit leaked staged in-memory caps/state: cap=%d bytes=%d state=%q", q.evaluationCleanupMarkerMaxCount, q.evaluationCleanupMarkerMaxBytes, q.evaluationCleanupMarkerMigrationState)
		}
		restarted := newContinuationDurableQueue(s.retrieval)
		if restarted.evaluationCleanupMarkerMaxCount != newCount || restarted.evaluationCleanupMarkerMaxBytes != newBytes || restarted.evaluationCleanupMarkerMigrationState != continuationEvaluationCleanupMarkerMigrationStateCommitted {
			t.Fatalf("restart did not reconcile durable receipt: cap=%d bytes=%d state=%q", restarted.evaluationCleanupMarkerMaxCount, restarted.evaluationCleanupMarkerMaxBytes, restarted.evaluationCleanupMarkerMigrationState)
		}
	})

	t.Run("fail-after-commit-readback-publishes", func(t *testing.T) {
		s := newContinuationDurableTestServer(t)
		q := newContinuationDurableQueue(s.retrieval)
		oldCount := q.evaluationCleanupMarkerCountLimitLocked()
		oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
		newCount := oldCount + 11
		newBytes := oldBytes + 32768
		failed := false
		q.evaluationCleanupMarkerWriter = func(root, index, shard, name string, content []byte) error {
			if name == continuationEvaluationCleanupMarkerMigrationActivePointerFile && bytes.Contains(content, []byte(`"state":"committed"`)) && !failed {
				if err := writeEvaluationCleanupMarkerDurable(root, index, shard, name, content); err != nil {
					return err
				}
				failed = true
				return errors.New("injected committed pointer failure after durable write")
			}
			return writeEvaluationCleanupMarkerDurable(root, index, shard, name, content)
		}
		_, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
			NewMaxCount: newCount, NewMaxBytes: newBytes, OperatorRef: "pointer-after", Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization,
			NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner, Reason: "pointer readback proof", AuthenticatedPrincipal: "pointer-operator", WorkspaceID: "pointer-workspace",
		})
		if err != nil || !failed {
			t.Fatalf("expected exact committed pointer readback, err=%v failed=%v", err, failed)
		}
		if q.evaluationCleanupMarkerMaxCount != newCount || q.evaluationCleanupMarkerMaxBytes != newBytes || q.evaluationCleanupMarkerMigrationState != continuationEvaluationCleanupMarkerMigrationStateCommitted {
			t.Fatalf("exact committed pointer did not publish authoritative state: cap=%d bytes=%d state=%q", q.evaluationCleanupMarkerMaxCount, q.evaluationCleanupMarkerMaxBytes, q.evaluationCleanupMarkerMigrationState)
		}
		restarted := newContinuationDurableQueue(s.retrieval)
		if restarted.evaluationCleanupMarkerMaxCount != newCount || restarted.evaluationCleanupMarkerMaxBytes != newBytes || restarted.evaluationCleanupMarkerMigrationState != continuationEvaluationCleanupMarkerMigrationStateCommitted {
			t.Fatalf("restart lost exact committed pointer: cap=%d bytes=%d state=%q", restarted.evaluationCleanupMarkerMaxCount, restarted.evaluationCleanupMarkerMaxBytes, restarted.evaluationCleanupMarkerMigrationState)
		}
	})
}

func TestContinuationDurableMarkerCapMigrationRollbackPointerFailureDoesNotPublish(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	newCount := oldCount + 12
	newBytes := oldBytes + 65536
	receipt, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: newCount, NewMaxBytes: newBytes, OperatorRef: "rollback-pointer", Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization,
		NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner, Reason: "rollback pointer proof", AuthenticatedPrincipal: "pointer-operator", WorkspaceID: "pointer-workspace",
	})
	if err != nil {
		t.Fatalf("initial migration failed: %v", err)
	}
	planDigest := anyToString(receipt["plan_digest"])
	failed := false
	q.evaluationCleanupMarkerWriter = func(root, index, shard, name string, content []byte) error {
		if name == continuationEvaluationCleanupMarkerMigrationActivePointerFile && bytes.Contains(content, []byte(`"state":"rolled_back"`)) && !failed {
			failed = true
			return errors.New("injected rollback pointer failure before durable write")
		}
		return writeEvaluationCleanupMarkerDurable(root, index, shard, name, content)
	}
	if err := q.rollbackEvaluationCleanupMarkerIndexCaps("rollback-pointer", planDigest, "pointer-operator", "pointer-workspace"); err == nil || !failed {
		t.Fatalf("expected rollback pointer failure, err=%v failed=%v", err, failed)
	}
	if q.evaluationCleanupMarkerMaxCount != newCount || q.evaluationCleanupMarkerMigrationState != continuationEvaluationCleanupMarkerMigrationStateCommitted {
		t.Fatalf("failed rollback leaked in-memory rollback state: cap=%d state=%q", q.evaluationCleanupMarkerMaxCount, q.evaluationCleanupMarkerMigrationState)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if restarted.evaluationCleanupMarkerMaxCount != newCount || restarted.evaluationCleanupMarkerMigrationState != continuationEvaluationCleanupMarkerMigrationStateCommitted {
		t.Fatalf("failed rollback changed durable authority: cap=%d state=%q", restarted.evaluationCleanupMarkerMaxCount, restarted.evaluationCleanupMarkerMigrationState)
	}
}

func TestContinuationDurableMarkerCapMigrationRestartsAfterManifestFailure(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	oldCount := q.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := q.evaluationCleanupMarkerByteLimitLocked()
	manifestFailures := 0
	q.evaluationCleanupMarkerWriter = func(root, index, shard, name string, content []byte) error {
		if name == continuationEvaluationCleanupMarkerIndexFile {
			manifestFailures++
			return errors.New("injected final migration manifest failure")
		}
		return writeEvaluationCleanupMarkerDurable(root, index, shard, name, content)
	}
	receipt, err := q.migrateEvaluationCleanupMarkerIndexCaps(continuationEvaluationCleanupMarkerCapMigrationRequest{
		NewMaxCount: oldCount + 12, NewMaxBytes: oldBytes + 8192,
		OperatorRef: "operator-migration-manifest-fault", Authorization: continuationEvaluationCleanupMarkerMigrationAuthorization,
		NativeOwner: continuationEvaluationCleanupMarkerMigrationNativeOwner, Reason: "manifest crash-window proof",
		AuthenticatedPrincipal: "test-operator", WorkspaceID: "test-workspace",
	})
	if err == nil || manifestFailures == 0 || anyToString(receipt["state"]) != continuationEvaluationCleanupMarkerMigrationStateCommitted {
		t.Fatalf("final manifest failure was not reported after durable receipt: err=%v failures=%d receipt=%#v", err, manifestFailures, receipt)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if restarted.evaluationCleanupMarkerMaxCount != oldCount+12 || restarted.snapshot().EvaluationCleanupMarkerIndex["cap_migration_state"] != continuationEvaluationCleanupMarkerMigrationStateCommitted {
		t.Fatalf("restart did not recover committed cap after manifest crash window: %#v", restarted.snapshot())
	}
}

func TestContinuationDurableMarkerCapMigrationNativeRouteClosedContract(t *testing.T) {
	t.Setenv(evaluationCleanupMarkerMigrationCapabilityEnv, "test-capability-secret")
	s := newContinuationDurableTestServer(t)
	oldCount := s.continuationDurable.evaluationCleanupMarkerCountLimitLocked()
	oldBytes := s.continuationDurable.evaluationCleanupMarkerByteLimitLocked()
	requestBody := map[string]any{
		"operation": "extend", "new_max_marker_count": oldCount + 4, "new_max_marker_bytes": oldBytes + 4096,
		"operator_ref": "route-migration-test", "authorization": continuationEvaluationCleanupMarkerMigrationAuthorization,
		"native_owner": continuationEvaluationCleanupMarkerMigrationNativeOwner, "reason": "route contract proof",
		"expected_generation": 0,
	}
	handler := buildNativeMux(s)
	unauthorizedRequest := httptest.NewRequest(http.MethodPost, "/ops/evaluation-cleanup/marker-cap-migration", bytes.NewReader([]byte(`{"operation":"extend"}`)))
	unauthorizedRequest.Header.Set("X-ContextLattice-Native-Owner", continuationEvaluationCleanupMarkerMigrationAuthorization)
	unauthorizedRecorder := httptest.NewRecorder()
	s.opsEvaluationCleanupMarkerCapMigration(unauthorizedRecorder, unauthorizedRequest)
	if unauthorizedRecorder.Code != http.StatusForbidden {
		t.Fatalf("native migration route accepted missing owner authorization: status=%d body=%s", unauthorizedRecorder.Code, unauthorizedRecorder.Body.String())
	}
	raw, err := json.Marshal(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/ops/evaluation-cleanup/marker-cap-migration", bytes.NewReader(raw))
	req.Header.Set(evaluationCleanupMarkerMigrationCapabilityHeader, "test-capability-secret")
	req.Header.Set(evaluationCleanupMarkerMigrationPrincipalHeader, "route-test-operator")
	req.Header.Set(evaluationCleanupMarkerMigrationWorkspaceHeader, "route-test-workspace")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("native migration route rejected authorized extension: status=%d body=%s queue=%#v", recorder.Code, recorder.Body.String(), s.continuationDurable.snapshot())
	}
	unknownRequest := httptest.NewRequest(http.MethodPost, "/ops/evaluation-cleanup/marker-cap-migration", bytes.NewReader([]byte(`{"operation":"extend","expected_generation":1,"unknown":true}`)))
	unknownRequest.Header.Set(evaluationCleanupMarkerMigrationCapabilityHeader, "test-capability-secret")
	unknownRequest.Header.Set(evaluationCleanupMarkerMigrationPrincipalHeader, "route-test-operator")
	unknownRequest.Header.Set(evaluationCleanupMarkerMigrationWorkspaceHeader, "route-test-workspace")
	unknownRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unknownRecorder, unknownRequest)
	if unknownRecorder.Code != http.StatusBadRequest {
		t.Fatalf("native migration route accepted unknown fields: status=%d body=%s", unknownRecorder.Code, unknownRecorder.Body.String())
	}
	scalarRequest := httptest.NewRequest(http.MethodPost, "/ops/evaluation-cleanup/marker-cap-migration", bytes.NewReader([]byte(`[]`)))
	scalarRequest.Header.Set(evaluationCleanupMarkerMigrationCapabilityHeader, "test-capability-secret")
	scalarRequest.Header.Set(evaluationCleanupMarkerMigrationPrincipalHeader, "route-test-operator")
	scalarRequest.Header.Set(evaluationCleanupMarkerMigrationWorkspaceHeader, "route-test-workspace")
	scalarRecorder := httptest.NewRecorder()
	handler.ServeHTTP(scalarRecorder, scalarRequest)
	if scalarRecorder.Code != http.StatusBadRequest {
		t.Fatalf("native migration route accepted scalar payload: status=%d body=%s", scalarRecorder.Code, scalarRecorder.Body.String())
	}
	planDigest := s.continuationDurable.evaluationCleanupMarkerMigrationPlanDigest
	rollbackBody := map[string]any{
		"operation": "rollback", "operator_ref": "route-migration-test", "plan_digest": planDigest,
		"authorization": continuationEvaluationCleanupMarkerMigrationAuthorization, "native_owner": continuationEvaluationCleanupMarkerMigrationNativeOwner,
		"reason": "route rollback proof",
	}
	rollbackRaw, err := json.Marshal(rollbackBody)
	if err != nil {
		t.Fatal(err)
	}
	rollbackRequest := httptest.NewRequest(http.MethodPost, "/ops/evaluation-cleanup/marker-cap-migration", bytes.NewReader(rollbackRaw))
	rollbackRequest.Header.Set(evaluationCleanupMarkerMigrationCapabilityHeader, "test-capability-secret")
	rollbackRequest.Header.Set(evaluationCleanupMarkerMigrationPrincipalHeader, "route-test-operator")
	rollbackRequest.Header.Set(evaluationCleanupMarkerMigrationWorkspaceHeader, "route-test-workspace")
	rollbackRecorder := httptest.NewRecorder()
	handler.ServeHTTP(rollbackRecorder, rollbackRequest)
	if rollbackRecorder.Code != http.StatusOK || s.continuationDurable.evaluationCleanupMarkerMaxCount != oldCount {
		t.Fatalf("native migration route did not authorize safe rollback: status=%d body=%s cap=%d", rollbackRecorder.Code, rollbackRecorder.Body.String(), s.continuationDurable.evaluationCleanupMarkerMaxCount)
	}
}

func TestContinuationDurableMarkerCapMigrationPublicFieldsAreClosed(t *testing.T) {
	validReason := "operator-authorized marker index recovery"
	if !continuationEvaluationCleanupMarkerMigrationReasonValid(validReason) {
		t.Fatal("bounded public reason was rejected")
	}
	for _, reason := range []string{
		"/private/queue/path",
		"provider secret: test-capability-secret",
		"private\ncontrol",
		"private\u2028text",
		"<redacted>\u0000",
	} {
		if continuationEvaluationCleanupMarkerMigrationReasonValid(reason) {
			t.Fatalf("private or control-bearing migration reason was accepted: %q", reason)
		}
	}
	for _, value := range []string{"/tmp/operator", "operator secret", "operator\tref", "..", "workspace/private"} {
		if continuationEvaluationCleanupMarkerMigrationOpaqueFieldValid(value, 128) {
			t.Fatalf("private migration identifier was accepted: %q", value)
		}
	}
}

func TestContinuationDurableMarkerRebuildReadsSameHandleAcrossReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows junction runtime is unavailable in this Unix test process")
	}
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	job := legacyEvaluationContinuationJob("cont_marker_rebuild_same_handle")
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := q.loadFromDisk(); err != nil {
		t.Fatalf("initial marker setup failed: %v", err)
	}
	markerPath := q.evaluationCleanupMarkerPathLocked(job)
	shardPath := filepath.Dir(markerPath)
	indexPath := filepath.Dir(shardPath)
	if err := os.Remove(filepath.Join(indexPath, continuationEvaluationCleanupMarkerIndexFile)); err != nil {
		t.Fatal(err)
	}
	restored := make(chan struct{})
	q.evaluationCleanupMarkerRebuildHook = func(stage string) error {
		backup := shardPath + ".aba-original"
		attacker := shardPath + ".aba-attacker"
		attackerSaved := shardPath + ".aba-attacker-saved"
		switch stage {
		case "after_directory_enumeration:" + filepath.Base(shardPath):
			if err := os.Rename(shardPath, backup); err != nil {
				return err
			}
			if err := os.Mkdir(attacker, 0o700); err != nil {
				_ = os.Rename(backup, shardPath)
				return err
			}
			attackerMarker := filepath.Join(attacker, filepath.Base(markerPath))
			if err := os.WriteFile(attackerMarker, []byte(`{"job_ref":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","removed_jobs":1}`), 0o600); err != nil {
				_ = os.RemoveAll(attacker)
				_ = os.Rename(backup, shardPath)
				return err
			}
		case "after_marker_read:" + filepath.Base(shardPath):
			// Restore the original pathname only after the child read. A
			// pathname-reopen implementation can therefore read the attacker
			// marker and then observe the original inode during its final check.
			if err := os.Rename(attacker, attackerSaved); err != nil {
				return err
			}
			if err := os.Rename(backup, shardPath); err != nil {
				return err
			}
			close(restored)
		}
		return nil
	}
	if err := q.loadFromDisk(); err != nil {
		_ = os.RemoveAll(shardPath + ".aba-attacker")
		_ = os.RemoveAll(shardPath + ".aba-attacker-saved")
		if _, statErr := os.Stat(shardPath + ".aba-original"); statErr == nil {
			_ = os.Rename(shardPath+".aba-original", shardPath)
		}
		t.Fatalf("same-handle rebuild was redirected by replacement: %v", err)
	}
	select {
	case <-restored:
	default:
		t.Fatal("replacement hook was not exercised")
	}
	if err := os.RemoveAll(shardPath + ".aba-attacker"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(shardPath + ".aba-attacker-saved"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.loadEvaluationCleanupMarkerLocked(job); err != nil {
		t.Fatalf("rebuild did not preserve original marker after ABA replacement: %v", err)
	}
}

func TestContinuationDurableMarkerWriteRejectsAncestorSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows runtime reparse write fixture is covered by the Windows-only test")
	}
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	index := continuationEvaluationCleanupIndexDirectory
	shard := "bb"
	indexPath := filepath.Join(q.dir, index)
	shardPath := filepath.Join(indexPath, shard)
	if err := os.MkdirAll(shardPath, 0o700); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(q.dir, "external-write-target")
	if err := os.Mkdir(external, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(shardPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, shardPath); err != nil {
		t.Fatal(err)
	}
	marker := "sha256:" + strings.Repeat("c", 64)
	if err := writeEvaluationCleanupMarkerDurable(q.dir, index, shard, marker, []byte("must not redirect\n")); err == nil {
		t.Fatal("descriptor-relative marker writer accepted an ancestor symlink")
	}
	if _, err := os.Stat(filepath.Join(external, marker)); !os.IsNotExist(err) {
		t.Fatalf("ancestor symlink redirected marker write: %v", err)
	}
	// The queue-root path itself is also opened component-by-component. A
	// symlinked ancestor must fail before index/shard creation, even when the
	// final root directory is otherwise a valid owner-only tree.
	container := t.TempDir()
	realParent := filepath.Join(container, "real-parent")
	realRoot := filepath.Join(realParent, "root")
	if err := os.MkdirAll(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasedParent := filepath.Join(container, "aliased-parent")
	if err := os.Symlink(realParent, aliasedParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	aliasedRoot := filepath.Join(aliasedParent, "root")
	if err := writeEvaluationCleanupMarkerDurable(aliasedRoot, index, shard, marker, []byte("must not redirect root\n")); err == nil {
		t.Fatal("descriptor-relative marker writer accepted a symlinked root ancestor")
	}
	if _, err := os.Stat(filepath.Join(realRoot, index, shard, marker)); !os.IsNotExist(err) {
		t.Fatalf("symlinked root ancestor redirected marker write: %v", err)
	}
}

func TestContinuationDurableCompletedReferenceDedupesWithUnrelatedPendingJobs(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	first := legacyEvaluationContinuationJob("cont_legacy_pending_first")
	completed := legacyEvaluationContinuationJob("cont_legacy_completed_second")
	q := newContinuationDurableQueue(s.retrieval)
	for _, job := range []*continuationDurableJob{first, completed} {
		raw, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
			t.Fatalf("write multi-job legacy fixture: %v", err)
		}
	}
	receipt := legacyEvaluationCleanupReceiptForJobs(completed)
	writeLegacyEvaluationCleanupReceipt(t, q, receipt)
	firstPath := filepath.Join(q.dir, first.ID+".json")
	q.evaluationCleanupDeleter = func(path string) error {
		if path == firstPath {
			return errors.New("injected unrelated pending delete failure")
		}
		return os.Remove(path)
	}
	if err := q.loadFromDisk(); err == nil {
		t.Fatal("multi-job legacy load hid unresolved pending delete")
	}
	if snapshot := q.snapshot(); snapshot.Pending != 1 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 1 {
		t.Fatalf("legacy completed reference or unrelated pending custody was lost: %#v", snapshot)
	}
	if _, err := os.Stat(filepath.Join(q.dir, completed.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("completed reference row was not unlinked: %v", err)
	}
	// Reintroduce the already-completed row while the unrelated first cleanup is
	// still pending. It must be removed without incrementing cumulative evidence.
	completedRaw, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, completed.ID+".json"), completedRaw, 0o600); err != nil {
		t.Fatalf("reintroduce completed row: %v", err)
	}
	q.mu.Lock()
	q.jobs[completed.ID] = completed
	q.fingerprintIndex[completed.Fingerprint] = completed.ID
	q.mu.Unlock()
	if cleaned, err := q.cleanupEvaluationJob(completed.ID, "drain"); !cleaned || err != nil {
		t.Fatalf("reappeared completed row was not deduped: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 1 || snapshot.EvaluationCleanupTotal != 1 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 0 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 1 {
		t.Fatalf("reappeared completed row incremented or overwrote unresolved custody: %#v", snapshot)
	}

	q.evaluationCleanupDeleter = os.Remove
	if cleaned, err := q.cleanupEvaluationJob(first.ID, "drain"); !cleaned || err != nil {
		t.Fatalf("unresolved first custody did not retry: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 2 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 0 {
		t.Fatalf("multi-job cleanup did not count only newly removed custody: %#v", snapshot)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != 2 || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 || continuationEvaluationCleanupPendingCount(snapshot.EvaluationCleanup["pending_jobs"]) != 0 {
		t.Fatalf("multi-job legacy restart violated exact-once completed reference: %#v", snapshot)
	}
}

func TestContinuationDurableCleanupIndexDedupeSurvivesBoundedReceiptWindow(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := newContinuationDurableQueue(s.retrieval)
	const cleanupCount = continuationEvaluationCleanupMaxPending + 44
	jobs := make([]*continuationDurableJob, 0, cleanupCount)
	for index := 0; index < cleanupCount; index++ {
		job := legacyEvaluationContinuationJob(fmt.Sprintf("cont_index_window_%03d", index))
		jobs = append(jobs, job)
		raw, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(q.dir, job.ID+".json"), raw, 0o600); err != nil {
			t.Fatalf("write cleanup-index job %d: %v", index, err)
		}
		q.mu.Lock()
		q.jobs[job.ID] = job
		q.fingerprintIndex[job.Fingerprint] = job.ID
		q.mu.Unlock()
		if cleaned, err := q.cleanupEvaluationJob(job.ID, "drain"); !cleaned || err != nil {
			t.Fatalf("cleanup-index job %d did not complete: cleaned=%v err=%v", index, cleaned, err)
		}
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != cleanupCount || len(anyToStringSlice(snapshot.EvaluationCleanup["completed_job_refs"])) > continuationEvaluationCleanupMaxPending {
		t.Fatalf("cleanup index run did not retain bounded receipt state and cumulative count: %#v", snapshot)
	}
	receiptRefs := map[string]struct{}{}
	for _, ref := range anyToStringSlice(q.snapshot().EvaluationCleanup["completed_job_refs"]) {
		receiptRefs[ref] = struct{}{}
	}
	var replayJob *continuationDurableJob
	for _, job := range jobs {
		if _, exists := receiptRefs[continuationEvaluationCleanupJobRef(job)]; !exists {
			replayJob = job
			break
		}
	}
	if replayJob == nil {
		t.Fatal("bounded receipt window unexpectedly retained every cleanup identity")
	}
	markerPath := q.evaluationCleanupMarkerPathLocked(replayJob)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("evicted completed identity was not retained in durable cleanup index: %v", err)
	}
	reintroduce := func(queue *continuationDurableQueue, job *continuationDurableJob) {
		t.Helper()
		raw, err := json.Marshal(job)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(queue.dir, job.ID+".json"), raw, 0o600); err != nil {
			t.Fatalf("reintroduce cleanup-index job: %v", err)
		}
		queue.mu.Lock()
		queue.jobs[job.ID] = job
		queue.fingerprintIndex[job.Fingerprint] = job.ID
		queue.mu.Unlock()
	}
	reintroduce(q, replayJob)
	if cleaned, err := q.cleanupEvaluationJob(replayJob.ID, "replay"); !cleaned || err != nil {
		t.Fatalf("evicted cleanup identity was not deduped from durable index: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := q.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != cleanupCount {
		t.Fatalf("oldest cleanup replay changed cumulative evidence: %#v", snapshot)
	}
	restarted := newContinuationDurableQueue(s.retrieval)
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != cleanupCount {
		t.Fatalf("cleanup index restart changed cumulative evidence: %#v", snapshot)
	}
	reintroduce(restarted, replayJob)
	if cleaned, err := restarted.cleanupEvaluationJob(replayJob.ID, "restart_replay"); !cleaned || err != nil {
		t.Fatalf("evicted cleanup identity was not deduped after restart: cleaned=%v err=%v", cleaned, err)
	}
	if snapshot := restarted.snapshot(); snapshot.Pending != 0 || snapshot.EvaluationCleanupTotal != cleanupCount {
		t.Fatalf("oldest cleanup restart replay double-counted: %#v", snapshot)
	}
}

func TestContinuationDurableStartupBoundsAndQuarantinesOversizedAndExcessFiles(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	q := s.continuationDurable
	q.loadMaxFiles = 3
	q.loadMaxBytes = 4096
	q.jobMaxBytes = 1024
	q.receiptMaxBytes = 128
	normal := &continuationDurableJob{
		SchemaVersion: continuationDurableSchemaVersion,
		ID:            "cont_bounded_normal",
		Source:        sourceLetta,
		Reason:        "bounded-startup",
		BaseRequest:   map[string]any{"project": "contextlattice", "query": "bounded startup"},
		CreatedAt:     time.Now().UTC(), UpdatedAt: time.Now().UTC(), DueAt: time.Now().UTC(),
	}
	normalRaw, err := json.Marshal(normal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, "a-normal.json"), normalRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	secondNormal := *normal
	secondNormal.ID = "cont_bounded_normal_two"
	secondNormalRaw, err := json.Marshal(&secondNormal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, "b-normal.json"), secondNormalRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, "c-oversized.json"), []byte(strings.Repeat("x", 2048)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, "d-excess.json"), normalRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := q.loadFromDisk(); err == nil {
		t.Fatal("bounded startup accepted oversized/excess durable entries")
	}
	quarantineDir := filepath.Join(q.dir, continuationDurableQuarantineDirectory)
	quarantined, err := os.ReadDir(quarantineDir)
	if err != nil || len(quarantined) < 2 {
		t.Fatalf("oversized and excess entries were not deterministically quarantined: count=%d err=%v", len(quarantined), err)
	}
	if q.snapshot().Pending != 2 {
		t.Fatalf("bounded startup did not retain only the bounded normal jobs: %#v", q.snapshot())
	}
}

func TestContinuationDurableDrainCleansLegacyEvaluationBeforeSchedulingOrSteering(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	job := legacyEvaluationContinuationJob("cont_legacy_drain_eval")
	q := s.continuationDurable
	q.mu.Lock()
	if err := q.writeJobLocked(job); err != nil {
		q.mu.Unlock()
		t.Fatalf("write drain fixture: %v", err)
	}
	q.jobs[job.ID] = job
	q.fingerprintIndex[job.Fingerprint] = job.ID
	q.mu.Unlock()
	s.drainContinuationDurableQueue()
	assertEvaluationContinuationSideEffectsAbsent(t, s, job.StreamToken)
	snapshot := q.snapshot()
	if snapshot.EvaluationCleanupTotal != 1 || anyToString(snapshot.EvaluationCleanup["phase"]) != "drain" || anyToInt(snapshot.EvaluationCleanup["removed_jobs"], 0) != 1 {
		t.Fatalf("drain did not emit the bounded cleanup receipt: %#v", snapshot)
	}
	if len(s.continuationSteeringState) != 0 || len(s.continuationSessionScopes) != 0 {
		t.Fatalf("evaluation cleanup mutated steering/session state: steering=%#v scopes=%#v", s.continuationSteeringState, s.continuationSessionScopes)
	}
}

func TestEvaluationHoldoutRunSourceBatchSuppressesTopicRollupTimeoutAndError(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	request := map[string]any{"query": "evaluation topic rollup fault", "traffic_class": "evaluation_holdout", "project": "contextlattice"}
	for _, testCase := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "error", ctx: context.Background()},
		{name: "timeout", ctx: func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Nanosecond))
			cancel()
			return ctx
		}()},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			token := "evaluation-topic-rollup-" + testCase.name
			output := s.runSourceBatch(testCase.ctx, http.Header{}, request, []string{sourceTopicRollup}, "balanced", "fault-"+testCase.name, true, true, true, false, map[string]struct{}{}, token)
			if len(output.continuationSources) != 0 || len(output.continuationUnavailable) != 0 {
				t.Fatalf("evaluation topic-rollup fault escaped into continuation state: %#v", output)
			}
			assertEvaluationContinuationSideEffectsAbsent(t, s, token)
		})
	}
}

func newContinuationDurableTestServer(t *testing.T) *server {
	t.Helper()
	policy := retrievalPolicy{
		continuationEventHistory:         16,
		continuationEventTTL:             5 * time.Minute,
		continuationMaxInflight:          1,
		continuationMaxInflightPerSource: 1,
		continuationMaxInflightOverrides: map[string]int{
			sourceLetta: 1,
		},
		continuationSourceCooldown:         0,
		continuationSourceCooldownBySrc:    map[string]time.Duration{},
		continuationTimeoutDefault:         2 * time.Second,
		continuationSheddingEnabled:        false,
		continuationDurableEnabled:         true,
		continuationDurableDir:             t.TempDir(),
		continuationDurableMaxPending:      64,
		continuationDurableMaxPendingBySrc: 2,
		continuationDurableDrainBatch:      8,
		continuationDurablePollInterval:    250 * time.Millisecond,
		continuationDurableRetryBase:       500 * time.Millisecond,
		continuationDurableRetryMax:        5 * time.Second,
		continuationDurableMaxAttempts:     4,
	}
	s := &server{
		retrieval:                       policy,
		client:                          &http.Client{Timeout: 250 * time.Millisecond},
		continuationSem:                 make(chan struct{}, 1),
		continuationInFlight:            map[string]int{},
		continuationInFlightStarted:     map[string][]time.Time{},
		continuationRetrying:            map[string]int{},
		continuationSourceCooldownUntil: map[string]time.Time{},
		continuationSubscribers:         map[string][]chan map[string]any{},
		continuationHistory:             map[string][]map[string]any{},
		continuationExpiry:              map[string]time.Time{},
		lettaAgentBySession:             map[string]string{},
		lettaAgentVerifiedAt:            map[string]time.Time{},
	}
	s.continuationDurable = newContinuationDurableQueue(policy)
	return s
}

func TestScheduleOrDeferContinuationQueuesDurablyUnderPressure(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	// Saturate in-flight semaphore so continuation scheduling is forced to defer.
	s.continuationSem <- struct{}{}
	token := "durable-token-pressure"

	state, status, _ := s.scheduleOrDeferContinuation(
		http.Header{},
		map[string]any{
			"project": "algotraderv2_rust",
			"query":   "letta warm request",
		},
		sourceLetta,
		"timeout",
		token,
	)
	if state != "deferred" {
		t.Fatalf("expected deferred continuation state, got state=%q status=%q", state, status)
	}
	if status != "durable_queued" {
		t.Fatalf("expected durable_queued status, got %q", status)
	}
	durable := s.continuationDurable.snapshot()
	if durable.Pending != 1 {
		t.Fatalf("expected one durable pending continuation job, got %#v", durable)
	}
	queue := s.continuationQueueSnapshot()
	if queue.DurablePending != 1 {
		t.Fatalf("expected durable pending reflected in queue snapshot, got %#v", queue)
	}
	s.continuationMu.Lock()
	events := s.continuationHistory[token]
	s.continuationMu.Unlock()
	if len(events) == 0 {
		t.Fatalf("expected continuation history event for deferred queueing")
	}
	last := events[len(events)-1]
	if strings.TrimSpace(strings.ToLower(anyToString(last["status"]))) != "durable_queued" {
		t.Fatalf("expected durable_queued continuation event, got %#v", last)
	}
}

func TestContinuationDurableQueueCapsPendingPerSource(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	s.continuationSem <- struct{}{}
	for i := 0; i < 5; i++ {
		state, _, _ := s.scheduleOrDeferContinuation(
			http.Header{},
			map[string]any{
				"project": "algotraderv2_rust",
				"query":   "letta warm request " + string(rune('a'+i)),
			},
			sourceLetta,
			"timeout",
			"durable-token-cap",
		)
		if i < 2 && state != "deferred" {
			t.Fatalf("expected first two jobs to defer, i=%d state=%q", i, state)
		}
	}
	durable := s.continuationDurable.snapshot()
	if durable.BySource[sourceLetta] != 2 {
		t.Fatalf("expected per-source cap of 2 durable Letta jobs, got %#v", durable)
	}
	if durable.Pending != 2 {
		t.Fatalf("expected total pending capped at 2, got %#v", durable)
	}
}

func TestDrainContinuationDurableQueueSchedulesAndClearsJob(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	s.continuationSem <- struct{}{}
	token := "durable-token-drain"
	state, _, _ := s.scheduleOrDeferContinuation(
		http.Header{},
		map[string]any{
			"project": "algotraderv2_rust",
			"query":   "warm letta cache",
		},
		sourceLetta,
		"timeout",
		token,
	)
	if state != "deferred" {
		t.Fatalf("expected deferred state before draining, got %q", state)
	}
	<-s.continuationSem
	s.drainContinuationDurableQueue()

	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.continuationDurable.snapshot().Pending == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected durable continuation queue to drain to zero, snapshot=%#v", s.continuationDurable.snapshot())
}

func TestScheduleOrDeferContinuationReportsUnavailableWhenDurableDisabled(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	s.continuationDurable.enabled = false
	s.continuationSem <- struct{}{}
	state, status, detail := s.scheduleOrDeferContinuation(
		http.Header{},
		map[string]any{
			"project": "algotraderv2_rust",
			"query":   "unavailable continuation",
		},
		sourceLetta,
		"timeout",
		"durable-token-disabled",
	)
	if state != "unavailable" {
		t.Fatalf("expected unavailable state when durable queue disabled, got %q", state)
	}
	if strings.TrimSpace(status) == "" {
		t.Fatalf("expected schedule status for unavailable continuation")
	}
	if strings.TrimSpace(anyToString(detail["durable_error"])) == "" {
		t.Fatalf("expected durable_error detail when durable queue disabled, got %#v", detail)
	}
}
