package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func hardeningObservedAgentTask(t *testing.T, ledger *agentTaskDeliveryLedger, taskID, project, owner, sessionID string) (agentTaskFence, map[string]any) {
	t.Helper()
	if _, err := ledger.submit(context.Background(), testAgentTaskManifest(taskID, project, owner, sessionID)); err != nil {
		t.Fatalf("submit observed task: %v", err)
	}
	claim, err := ledger.claimNext(context.Background(), "worker-"+taskID, "instance-"+taskID, "")
	if err != nil || claim == nil {
		t.Fatalf("claim observed task: row=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatalf("heartbeat observed task: %v", err)
	}
	exitCode := 0
	if _, err := ledger.observe(context.Background(), fence, "succeeded", &exitCode, map[string]any{"source": "hardening-test"}); err != nil {
		t.Fatalf("observe task: %v", err)
	}
	return fence, claim
}

func hardeningPublicationRequest(fence agentTaskFence, claim map[string]any, suffix string, artifacts []any) map[string]any {
	workspaceRef := "workspace-ref-" + suffix
	return map[string]any{
		"publication_id": "publication-" + suffix, "idempotency_key": "publication-idempotency:" + suffix,
		"runner_exit_required": true,
		"result": map[string]any{
			"result_id": "result-" + suffix, "summary": "bounded hardening result", "output": "verified output",
			"context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
			"workspace":         map[string]any{"workspace_ref": workspaceRef},
			"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, workspaceRef)},
		},
		"artifacts": artifacts,
	}
}

func testAgentTaskCount(t *testing.T, ledger *agentTaskDeliveryLedger, table string) int {
	t.Helper()
	allowed := map[string]bool{
		"task_ledger_tasks": true, "task_ledger_events": true, "task_ledger_artifacts": true,
		"task_ledger_results": true, "task_ledger_publications": true, "task_ledger_reviews": true,
		"task_ledger_blocking_answers": true, "task_ledger_approvals": true, "task_ledger_integrations": true,
		"task_ledger_migration_receipts": true, "task_ledger_cleanup_receipts": true,
		"task_ledger_worker_identity_retirements": true,
	}
	if !allowed[table] {
		t.Fatalf("test requested unsafe table name %q", table)
	}
	var count int
	if err := ledger.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

func TestAgentTaskRecursiveSecretBoundaryRejectsEveryMutationBeforePersistence(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz123456"
	manifest := testAgentTaskManifest("secret-manifest", "secret-project", "secret-owner", "sess_secret")
	manifest["metadata"] = map[string]any{"nested": []any{map[string]any{"credential": secret}}}
	if _, err := ledger.submit(context.Background(), manifest); err == nil || !strings.Contains(err.Error(), "canonical Gateway secret boundary") {
		t.Fatalf("nested manifest secret was accepted: %v", err)
	}
	if count := testAgentTaskCount(t, ledger, "task_ledger_tasks"); count != 0 {
		t.Fatalf("rejected manifest left %d task rows", count)
	}

	clean := testAgentTaskManifest("secret-boundaries", "secret-project", "secret-owner", "sess_secret_boundaries")
	if _, err := ledger.submit(context.Background(), clean); err != nil {
		t.Fatal(err)
	}
	claim, err := ledger.claimNext(context.Background(), "secret-worker", "secret-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("claim: %#v %v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := ledger.heartbeat(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	eventsBefore := testAgentTaskCount(t, ledger, "task_ledger_events")
	if _, err := ledger.observe(context.Background(), fence, "succeeded", nil, map[string]any{"runtime": []any{map[string]any{"client_secret": secret}}}); err == nil {
		t.Fatal("nested runner observation secret was accepted")
	}
	if events := testAgentTaskCount(t, ledger, "task_ledger_events"); events != eventsBefore {
		t.Fatalf("rejected observation changed event rows: before=%d after=%d", eventsBefore, events)
	}
	attempt, err := ledger.attempt(context.Background(), fence.AttemptID)
	if err != nil || anyToString(attempt["status"]) != "running" {
		t.Fatalf("rejected observation mutated attempt: %#v %v", attempt, err)
	}

	exitCode := 0
	if _, err := ledger.observe(context.Background(), fence, "succeeded", &exitCode, nil); err != nil {
		t.Fatal(err)
	}
	content := "clean artifact must not survive a secret result"
	digest := agentTaskBytesDigest([]byte(content))
	contentPath, err := ledger.artifactPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	request := hardeningPublicationRequest(fence, claim, "secret-result", []any{map[string]any{"name": "clean.txt", "content": content, "digest": digest}})
	anyMap(request["result"])["runtime_metadata"] = map[string]any{"layers": []any{map[string]any{"api_key": secret}}}
	if _, err := ledger.stagePublication(context.Background(), fence, request); err == nil {
		t.Fatal("nested result secret was accepted")
	}
	if _, err := os.Lstat(contentPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected result left artifact blob: %v", err)
	}
	for _, table := range []string{"task_ledger_artifacts", "task_ledger_results", "task_ledger_publications"} {
		if count := testAgentTaskCount(t, ledger, table); count != 0 {
			t.Fatalf("rejected result left %d rows in %s", count, table)
		}
	}

	mutationChecks := []struct {
		name  string
		table string
		call  func() error
	}{
		{"review", "task_ledger_reviews", func() error {
			_, err := ledger.review(context.Background(), "task", "result", "owner", "accept", "Bearer "+secret, "")
			return err
		}},
		{"blocking_answer", "task_ledger_blocking_answers", func() error {
			_, err := ledger.answerBlockingQuestion(context.Background(), "task", "result", "delivery", "owner", "password="+secret, "attempt")
			return err
		}},
		{"approval", "task_ledger_approvals", func() error {
			_, err := ledger.createApproval(context.Background(), map[string]any{"task_id": "task", "attempt_id": "attempt", "approver": "owner", "target": map[string]any{"private_key": secret}})
			return err
		}},
		{"integration", "task_ledger_integrations", func() error {
			_, err := ledger.integrate(context.Background(), map[string]any{"task_id": "task", "result_id": "result", "actor": "owner", "action": "open_pr", "execution_receipt": map[string]any{"token": secret}})
			return err
		}},
		{"delivery", "task_ledger_events", func() error {
			_, err := ledger.deliver(context.Background(), "delivery", "failed", "Authorization: Bearer "+secret)
			return err
		}},
		{"finalize", "task_ledger_events", func() error {
			_, err := ledger.finalizePublication(context.Background(), "publication", "failed", "", "client_secret="+secret)
			return err
		}},
	}
	for _, check := range mutationChecks {
		before := testAgentTaskCount(t, ledger, check.table)
		if err := check.call(); err == nil || !strings.Contains(err.Error(), "canonical Gateway secret boundary") {
			t.Fatalf("%s secret was not rejected at canonical boundary: %v", check.name, err)
		}
		if after := testAgentTaskCount(t, ledger, check.table); after != before {
			t.Fatalf("%s rejection changed %s rows: before=%d after=%d", check.name, check.table, before, after)
		}
	}
}

func TestAgentTaskArtifactPreparationIsOutsideTransactionAndLateBindingIsExact(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	first, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	fence, claim := hardeningObservedAgentTask(t, first, "artifact-short-tx", "artifact-project", "artifact-owner", "sess_artifact_short_tx")
	content := "same-size-original"
	digest := agentTaskBytesDigest([]byte(content))
	path, err := first.artifactPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	first.artifactStageHook = func(stage string) error {
		switch stage {
		case "after_artifact_writes":
			return second.setMeta(context.Background(), "artifact_prepare_outside_tx", "proved")
		case "before_publication_commit":
			if err := os.Rename(path, path+".displaced"); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("same-size-foreign!"), 0o600)
		}
		return nil
	}
	_, err = first.stagePublication(context.Background(), fence, hardeningPublicationRequest(fence, claim, "artifact-short-tx", []any{map[string]any{"artifact_id": "artifact-short-tx", "name": "proof.txt", "content": content, "digest": digest, "size_bytes": len(content)}}))
	if err == nil || (!strings.Contains(err.Error(), "canonical path") && !strings.Contains(err.Error(), "digest verification") && !strings.Contains(err.Error(), "digest-verified descriptor changed")) {
		t.Fatalf("same-size path replacement was committed: %v", err)
	}
	if count := testAgentTaskCount(t, first, "task_ledger_artifacts"); count != 0 {
		t.Fatalf("failed final descriptor binding left %d artifact rows", count)
	}
	if count := testAgentTaskCount(t, first, "task_ledger_results"); count != 0 {
		t.Fatalf("failed final descriptor binding left %d result rows", count)
	}
	var proof string
	if err := first.db.QueryRowContext(context.Background(), `SELECT value FROM task_ledger_meta WHERE key='artifact_prepare_outside_tx'`).Scan(&proof); err != nil || proof != "proved" {
		t.Fatalf("second handle could not write during artifact preparation: proof=%q err=%v", proof, err)
	}
}

func TestAgentTaskArtifactLateInPlaceMutationInvalidatesPinnedDigestBinding(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	fence, claim := hardeningObservedAgentTask(t, ledger, "artifact-in-place", "artifact-project", "artifact-owner", "sess_artifact_in_place")
	content := "immutable-original"
	digest := agentTaskBytesDigest([]byte(content))
	path, err := ledger.artifactPath(digest)
	if err != nil {
		t.Fatal(err)
	}
	ledger.artifactStageHook = func(stage string) error {
		if stage != "before_publication_commit" {
			return nil
		}
		return os.WriteFile(path, []byte("immutable-foreignX"), 0o600)
	}
	_, err = ledger.stagePublication(context.Background(), fence, hardeningPublicationRequest(fence, claim, "artifact-in-place", []any{
		map[string]any{"artifact_id": "artifact-in-place", "name": "proof.txt", "content": content, "digest": digest, "size_bytes": len(content)},
	}))
	if err == nil || !strings.Contains(err.Error(), "digest-verified descriptor changed") {
		t.Fatalf("same-inode same-size mutation was committed: %v", err)
	}
	if count := testAgentTaskCount(t, ledger, "task_ledger_artifacts"); count != 0 {
		t.Fatalf("in-place mutation left %d artifact ledger rows", count)
	}
	if count := testAgentTaskCount(t, ledger, "task_ledger_results"); count != 0 {
		t.Fatalf("in-place mutation left %d result ledger rows", count)
	}
}

func TestAgentTaskArtifactReadsRejectSymlinkAndSameSizeReplacement(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(string, []byte) error
	}{
		{"symlink", func(path string, foreign []byte) error {
			target := filepath.Join(filepath.Dir(filepath.Dir(path)), "foreign-target")
			if err := os.WriteFile(target, foreign, 0o600); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(target, path)
		}},
		{"same_size", func(path string, foreign []byte) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.WriteFile(path, foreign, 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ledger := testAgentTaskLedger(t)
			content := []byte("immutable-original")
			_, publication := testAgentTaskStagePublication(t, ledger, "artifact-read-"+test.name, "artifact-read-project", "artifact-reader", "sess_artifact_read_"+test.name, []any{map[string]any{"artifact_id": "artifact-read-" + test.name, "name": "proof.txt", "content": string(content)}})
			artifact := anyMap(anySlice(anyMap(publication["result"])["artifacts"])[0])
			var path string
			if err := ledger.db.QueryRowContext(context.Background(), `SELECT content_path FROM task_ledger_artifacts WHERE artifact_id=?`, anyToString(artifact["artifact_id"])).Scan(&path); err != nil {
				t.Fatal(err)
			}
			foreign := []byte("immutable-foreignX")
			if len(foreign) != len(content) {
				t.Fatal("test fixture must preserve size")
			}
			if err := test.replace(path, foreign); err != nil {
				t.Fatal(err)
			}
			if file, _, err := ledger.artifactFile(context.Background(), anyToString(artifact["artifact_id"]), "artifact-reader"); err == nil {
				_ = file.Close()
				t.Fatal("foreign artifact bytes passed immutable read verification")
			}
		})
	}
}

func TestAgentTaskArtifactReconciliationSerializesWithCrossHandleWriter(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	writer, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer writer.close()
	reconciler, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.close()
	fence, claim := hardeningObservedAgentTask(t, writer, "artifact-lock-race", "artifact-lock-project", "artifact-lock-owner", "sess_artifact_lock")
	written := make(chan struct{})
	release := make(chan struct{})
	writer.artifactStageHook = func(stage string) error {
		if stage == "after_artifact_writes" {
			close(written)
			<-release
		}
		return nil
	}
	publicationDone := make(chan error, 1)
	go func() {
		_, stageErr := writer.stagePublication(context.Background(), fence, hardeningPublicationRequest(fence, claim, "artifact-lock-race", []any{map[string]any{"artifact_id": "artifact-lock-race", "name": "proof.txt", "content": "writer-owned-clean-content"}}))
		publicationDone <- stageErr
	}()
	<-written
	beforeLock := make(chan struct{})
	afterLock := make(chan struct{})
	reconciler.artifactReconcileHook = func(stage string) {
		if stage == "before_lock" {
			close(beforeLock)
		} else if stage == "after_lock" {
			close(afterLock)
		}
	}
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- reconciler.reconcileArtifactOrphans(context.Background()) }()
	<-beforeLock
	select {
	case <-afterLock:
		t.Fatal("orphan reconciliation crossed an active writer namespace lease")
	default:
	}
	close(release)
	if err := <-publicationDone; err != nil {
		t.Fatalf("writer publication: %v", err)
	}
	if err := <-reconcileDone; err != nil {
		t.Fatalf("post-writer reconciliation: %v", err)
	}
	if count := testAgentTaskCount(t, writer, "task_ledger_artifacts"); count != 1 {
		t.Fatalf("reconciliation raced committed writer: artifact rows=%d", count)
	}
}

func TestAgentTaskMigrationIsAllowlistedScannedAndConvergesAcrossHandles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "server-owned-agent-tasks.json")
	arbitrary := filepath.Join(root, "caller-selected.json")
	if err := os.WriteFile(source, []byte(`{"tasks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arbitrary, []byte(`{"tasks":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GO_AGENT_TASKS_PATH", source)
	t.Setenv("GO_AGENT_TASK_MIGRATION_SOURCE_PATHS", "")
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	first, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	if _, err := first.migration(context.Background(), map[string]any{"phase": "import", "source_path": arbitrary}); err == nil || !strings.Contains(err.Error(), "allowlisted") {
		t.Fatalf("caller-selected migration source was accepted: %v", err)
	}
	secretDocument := map[string]any{"tasks": []any{map[string]any{"task_id": "secret-import", "metadata": map[string]any{"client_secret": "ghp_" + "abcdefghijklmnopqrstuvwxyz123456"}}}}
	secretJSON, err := json.Marshal(secretDocument)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, secretJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := first.migration(context.Background(), map[string]any{"phase": "import", "source_path": source}); err == nil || !strings.Contains(err.Error(), "canonical Gateway secret boundary") {
		t.Fatalf("secret legacy source was accepted: %v", err)
	}
	if count := testAgentTaskCount(t, first, "task_ledger_tasks"); count != 0 {
		t.Fatalf("secret migration left %d tasks", count)
	}
	if count := testAgentTaskCount(t, first, "task_ledger_migration_receipts"); count != 0 {
		t.Fatalf("secret migration left %d receipts", count)
	}

	manifest := testAgentTaskManifest("migrated-race-task", "migration-project", "migration-owner", "sess_migration")
	manifest["id"] = manifest["task_id"]
	cleanJSON, err := json.Marshal(map[string]any{"tasks": []any{manifest}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, cleanJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	for _, ledger := range []*agentTaskDeliveryLedger{first, second} {
		wg.Add(1)
		go func(ledger *agentTaskDeliveryLedger) {
			defer wg.Done()
			_, migrationErr := ledger.migration(context.Background(), map[string]any{"phase": "import", "source_path": source})
			errorsCh <- migrationErr
		}(ledger)
	}
	wg.Wait()
	close(errorsCh)
	for migrationErr := range errorsCh {
		if migrationErr != nil {
			t.Fatalf("two-handle migration did not converge: %v", migrationErr)
		}
	}
	if count := testAgentTaskCount(t, first, "task_ledger_tasks"); count != 1 {
		t.Fatalf("two-handle import produced %d task rows", count)
	}
	if count := testAgentTaskCount(t, first, "task_ledger_migration_receipts"); count != 1 {
		t.Fatalf("two-handle import produced %d migration receipts", count)
	}
}

func TestAgentTaskAuthorizedClaimProgressesPastFiveHundredStaleGovernanceRows(t *testing.T) {
	ledger := testAgentTaskLedger(t)
	const staleRows = 520
	for index := 0; index < staleRows; index++ {
		manifest := testAgentTaskManifest(fmt.Sprintf("stale-governance-%03d", index), fmt.Sprintf("stale-project-%03d", index), "claim-owner", fmt.Sprintf("sess_stale_%03d", index))
		manifest["priority"] = 1000
		if _, err := ledger.submit(context.Background(), manifest); err != nil {
			t.Fatalf("submit stale row %d: %v", index, err)
		}
	}
	validLow := testAgentTaskManifest("valid-low", "claim-valid-project", "claim-owner", "sess_valid_low")
	validLow["priority"] = 10
	validHigh := testAgentTaskManifest("valid-high", "claim-valid-project", "claim-owner", "sess_valid_high")
	validHigh["priority"] = 20
	for _, manifest := range []map[string]any{validLow, validHigh} {
		if _, err := ledger.submit(context.Background(), manifest); err != nil {
			t.Fatal(err)
		}
	}
	server, _, _ := testAgentTaskServerWithMemory(t, ledger, "claim-valid-project", "claim-owner", "sess_valid_high")
	claim, err := server.claimNextAuthorizedAgentTask(context.Background(), "claim-worker", "claim-instance", "")
	if err != nil || claim == nil {
		t.Fatalf("claim after %d stale governance rows: row=%#v err=%v", staleRows, claim, err)
	}
	if claimedID := anyToString(anyMap(claim["task"])["task_id"]); claimedID != "valid-high" {
		t.Fatalf("claim cursor lost eligible priority: got %q want valid-high", claimedID)
	}
}

func TestAgentTaskSchemaMigrationSerializesAcrossConcurrentHandles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	const handles = 6
	ledgers := make(chan *agentTaskDeliveryLedger, handles)
	errorsCh := make(chan error, handles)
	var wg sync.WaitGroup
	for index := 0; index < handles; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ledger, err := newAgentTaskDeliveryLedgerFromEnv()
			if err != nil {
				errorsCh <- err
				return
			}
			ledgers <- ledger
		}()
	}
	wg.Wait()
	close(ledgers)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent schema initialization: %v", err)
	}
	opened := make([]*agentTaskDeliveryLedger, 0, handles)
	for ledger := range ledgers {
		opened = append(opened, ledger)
	}
	defer func() {
		for _, ledger := range opened {
			_ = ledger.close()
		}
	}()
	if len(opened) != handles {
		t.Fatalf("opened %d ledgers, want %d", len(opened), handles)
	}
	var schemaVersion int
	if err := opened[0].db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&schemaVersion); err != nil || schemaVersion != agentTaskLedgerSchemaVersion {
		t.Fatalf("serialized schema version=%d err=%v", schemaVersion, err)
	}
	if count := testAgentTaskCount(t, opened[0], "task_ledger_cleanup_receipts"); count != 0 {
		t.Fatalf("new cleanup evidence table unexpectedly nonempty: %d", count)
	}
}

func TestAgentTaskExecutionRetryIsFencedCrashSafeAndDeadLetters(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", filepath.Join(root, "agent_tasks.sqlite3"))
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	t.Setenv("GO_AGENT_TASK_MAX_EXECUTION_ATTEMPTS", "2")
	first, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	manifest := testAgentTaskManifest("execution-retry", "retry-project", "retry-owner", "sess_retry")
	if _, err := first.submit(context.Background(), manifest); err != nil {
		t.Fatal(err)
	}
	claim, err := first.claimNext(context.Background(), "retry-worker", "retry-instance-1", "")
	if err != nil || claim == nil {
		t.Fatalf("first claim: %#v %v", claim, err)
	}
	firstFence := testAgentTaskFenceFromClaim(t, claim)
	if _, err := first.heartbeat(context.Background(), firstFence); err != nil {
		t.Fatal(err)
	}
	first.executionRetryHook = func(stage string) error {
		if stage == "after_attempt_observation" {
			return errors.New("injected observation crash")
		}
		return nil
	}
	exitCode := 17
	if _, err := first.observe(context.Background(), firstFence, "failed", &exitCode, map[string]any{"fault": "first"}); err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("observation crash hook did not fire: %v", err)
	}
	attempt, err := first.attempt(context.Background(), firstFence.AttemptID)
	if err != nil || anyToString(attempt["status"]) != "running" || anyToString(attempt["observation_digest"]) != "" {
		t.Fatalf("crashed observation leaked partial attempt mutation: %#v %v", attempt, err)
	}

	second, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	failed, err := second.observe(context.Background(), firstFence, "failed", &exitCode, map[string]any{"fault": "first"})
	if err != nil || anyToString(failed["failure_disposition"]) != "retry_queued" || anyToString(anyMap(failed["task"])["status"]) != "queued" {
		t.Fatalf("restart did not requeue failed attempt: %#v %v", failed, err)
	}
	replay, err := second.observe(context.Background(), firstFence, "failed", &exitCode, map[string]any{"fault": "first"})
	if err != nil || !anyToBool(replay["idempotent_replay"]) {
		t.Fatalf("failed observation did not replay idempotently: %#v %v", replay, err)
	}
	secondClaim, err := second.claimNext(context.Background(), "retry-worker", "retry-instance-2", "")
	if err != nil || secondClaim == nil {
		t.Fatalf("second retry claim: %#v %v", secondClaim, err)
	}
	secondFence := testAgentTaskFenceFromClaim(t, secondClaim)
	if secondFence.AttemptID == firstFence.AttemptID || secondFence.Generation != firstFence.Generation+1 {
		t.Fatalf("retry reused stale attempt generation: first=%#v second=%#v", firstFence, secondFence)
	}
	if _, err := second.heartbeat(context.Background(), secondFence); err != nil {
		t.Fatal(err)
	}
	dead, err := second.observe(context.Background(), secondFence, "failed", &exitCode, map[string]any{"fault": "second"})
	if err != nil || anyToString(dead["failure_disposition"]) != "execution_dead_letter" || anyToString(anyMap(dead["task"])["status"]) != "dead_letter" {
		t.Fatalf("retry budget did not dead-letter: %#v %v", dead, err)
	}
	third, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer third.close()
	if next, err := third.claimNext(context.Background(), "retry-worker", "retry-instance-3", ""); err != nil || next != nil {
		t.Fatalf("dead-letter task was claimable after restart: %#v %v", next, err)
	}
}

func TestGatewayStartupCancellationAndPanicsAlwaysPublishTerminalResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	release := make(chan struct{})
	lateClosed := make(chan struct{})
	activated := atomic.Bool{}
	terminal := startGatewayRuntime(ctx, func(context.Context) *server {
		close(started)
		<-release
		workerContext, workerCancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			<-workerContext.Done()
			close(done)
			close(lateClosed)
		}()
		return &server{taskRecoveryCancel: workerCancel, taskRecoveryDone: done}
	}, func(*server) { activated.Store(true) })
	<-started
	cancel()
	result := <-terminal
	if !errors.Is(result.err, context.Canceled) || result.server != nil || activated.Load() {
		t.Fatalf("startup cancellation result=%#v activated=%v", result, activated.Load())
	}
	close(release)
	<-lateClosed

	panicResult := <-startGatewayRuntime(context.Background(), func(context.Context) *server {
		panic("constructor fault")
	}, func(*server) {})
	if panicResult.err == nil || !strings.Contains(panicResult.err.Error(), "startup panic") {
		t.Fatalf("startup panic did not publish a terminal error: %#v", panicResult)
	}
	activationResult := <-startGatewayRuntime(context.Background(), func(context.Context) *server { return &server{} }, func(*server) {
		panic("activation fault")
	})
	if activationResult.err == nil || !strings.Contains(activationResult.err.Error(), "activation panic") {
		t.Fatalf("activation panic did not publish a terminal error: %#v", activationResult)
	}
}

func TestAgentTaskCryptoIDsAndStorageCollisionRetry(t *testing.T) {
	seen := map[string]bool{}
	for index := 0; index < 256; index++ {
		id, err := newAgentTaskID("proof")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(id, "proof_") || len(strings.TrimPrefix(id, "proof_")) != 32 || seen[id] {
			t.Fatalf("cryptographic ID is malformed or duplicated: %q", id)
		}
		seen[id] = true
	}
	ledger := testAgentTaskLedger(t)
	collision := testAgentTaskManifest("task_collision", "id-project", "id-owner", "sess_id_collision")
	if _, err := ledger.submit(context.Background(), collision); err != nil {
		t.Fatal(err)
	}
	var taskCalls atomic.Int32
	ledger.idGenerator = func(prefix string) (string, error) {
		if prefix == "task" {
			if taskCalls.Add(1) == 1 {
				return "task_collision", nil
			}
			return "task_after_collision", nil
		}
		return newAgentTaskID(prefix)
	}
	generated := testAgentTaskManifest("", "id-project", "id-owner", "sess_id_retry")
	generated["idempotency_key"] = "idempotency-generated-after-collision"
	row, err := ledger.submit(context.Background(), generated)
	if err != nil || anyToString(row["task_id"]) != "task_after_collision" || taskCalls.Load() < 2 {
		t.Fatalf("storage collision did not retry with a new ID: row=%#v calls=%d err=%v", row, taskCalls.Load(), err)
	}
	ledger.idGenerator = func(prefix string) (string, error) { return "task_collision", nil }
	exhausted := testAgentTaskManifest("", "id-project", "id-owner", "sess_id_exhausted")
	exhausted["idempotency_key"] = "idempotency-exhausted-collision"
	if _, err := ledger.submit(context.Background(), exhausted); err == nil || !errors.Is(err, errAgentTaskIDCollision) {
		t.Fatalf("exhausted ID collision did not fail closed: %v", err)
	}
}
