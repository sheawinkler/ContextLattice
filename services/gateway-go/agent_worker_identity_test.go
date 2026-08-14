package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestWorkerIdentityPythonProducerToGatewayHTTPAckRestartCAS(t *testing.T) {
	first, second := identityTestLedgers(t)
	authority := identityTestAuthority("python-principal", "python-workspace", "python-instance")
	if _, err := first.registerWorkerIdentity(context.Background(), "python-occupier", authority.WorkspaceID, "python-http-worker", "occupier-instance"); err != nil {
		t.Fatalf("seed canonical worker identity: %v", err)
	}
	key := "python-worker-http-test-key"
	server := &server{taskLedger: first, orchestratorAPIKey: key}
	var capturedAck []byte
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/agents/workers/identity/ack") {
			capturedAck, _ = io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(capturedAck))
		}
		buildMux(server).ServeHTTP(w, r)
	}))
	defer gateway.Close()
	stateRoot, err := os.MkdirTemp(os.Getenv("TMPDIR"), "worker-identity-http-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateRoot)
	python := `import json, os, sys
from scripts import task_agent_worker as worker
state = worker._load_or_create_worker_state("python-http-worker", dispatcher_id="python-dispatcher", worker_instance="python-instance")
state = worker._register_worker_identity(os.environ["TEST_GATEWAY_URL"], state)
claim = worker._claim_next_task(os.environ["TEST_GATEWAY_URL"], "python-http-worker", state=state)
safe_state = dict(state)
safe_state.pop("worker_instance_credential", None)
print(json.dumps({"state": safe_state, "claim": claim}, sort_keys=True))
`
	command := exec.Command("python3", "-c", python)
	command.Dir = filepath.Join("..", "..")
	command.Env = append(os.Environ(),
		"TEST_GATEWAY_URL="+gateway.URL,
		"CONTEXTLATTICE_ORCHESTRATOR_API_KEY="+key,
		"CONTEXTLATTICE_WORKER_API_KEY="+key,
		"TASK_WORKER_PRINCIPAL="+authority.PrincipalID,
		"TASK_WORKER_WORKSPACE="+authority.WorkspaceID,
		"TASK_AGENT_WORKER_STATE_ROOT="+stateRoot,
		"PYTHONDONTWRITEBYTECODE=1",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("real Python producer -> Gateway HTTP consumer failed: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode Python producer result: %v (%s)", err, strings.TrimSpace(string(output)))
	}
	if !anyToBool(anyMap(result["claim"])["task"] == nil) {
		t.Fatalf("Python producer did not complete acknowledged no-task claim: %#v", result)
	}
	stateFiles, err := filepath.Glob(filepath.Join(stateRoot, "worker_identity.dispatcher-*.json"))
	if err != nil || len(stateFiles) != 1 {
		t.Fatalf("read Python worker state path: files=%v err=%v", stateFiles, err)
	}
	stateRaw, err := os.ReadFile(stateFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	var persistedState map[string]any
	if err := json.Unmarshal(stateRaw, &persistedState); err != nil {
		t.Fatal(err)
	}
	credential := anyToString(persistedState["worker_instance_credential"])
	if len(credential) != workerInstanceCredentialBytes*2 {
		t.Fatalf("Python worker did not persist its client credential owner-only: %q", credential)
	}
	var delivered map[string]any
	if err := json.Unmarshal(capturedAck, &delivered); err != nil {
		t.Fatalf("decode captured Python ACK: %v", err)
	}
	nested := anyMap(delivered["identity_update"])
	update, err := first.workerIdentityUpdateByID(context.Background(), anyToString(nested["update_id"]))
	if err != nil || update.UpdateID == "" {
		t.Fatalf("read Python worker identity update: update=%#v err=%v", update, err)
	}
	if anyToString(nested["state"]) != agentWorkerIdentityStateDelivered || anyToBool(nested["ack_required"]) != true {
		t.Fatalf("Python ACK did not carry exact delivered receipt: %#v", nested)
	}
	state, err := first.workerIdentityUpdateState(context.Background(), update.UpdateID)
	if err != nil || state != agentWorkerIdentityStateAcknowledged {
		t.Fatalf("Gateway did not commit Python ACK: state=%q err=%v", state, err)
	}
	stored, err := first.workerIdentityUpdateByID(context.Background(), update.UpdateID)
	if err != nil || stored.AckReceiptPayloadVersion != workerIdentityAckReceiptPayloadVersionExact || !workerIdentityReceiptSnapshotMatches(nested, stored.AckReceiptPayloadJSON) {
		t.Fatalf("Gateway did not retain exact ACK receipt snapshot: update=%#v err=%v", stored, err)
	}
	// Re-submit the exact Python-produced request after reopening the SQLite
	// authority. The second consumer must return an idempotent replay and must
	// not mutate the committed receipt.
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("reopen Gateway ledger: %v", err)
	}
	defer restarted.close()
	server.taskLedger = restarted
	replayRequest, err := http.NewRequest(http.MethodPost, gateway.URL+"/agents/workers/identity/ack", bytes.NewReader(capturedAck))
	if err != nil {
		t.Fatal(err)
	}
	replayRequest.Header.Set("Content-Type", "application/json")
	replayRequest.Header.Set("X-API-Key", key)
	replayRequest.Header.Set(workerInstanceCredentialHeader, credential)
	replayRequest.Header.Set("X-Worker-Instance-ID", authority.WorkerInstanceID)
	replayResponse, err := http.DefaultClient.Do(replayRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer replayResponse.Body.Close()
	replayBody, _ := io.ReadAll(replayResponse.Body)
	if replayResponse.StatusCode != http.StatusOK {
		t.Fatalf("exact ACK replay after restart failed: status=%d body=%s", replayResponse.StatusCode, replayBody)
	}
	var replay map[string]any
	if err := json.Unmarshal(replayBody, &replay); err != nil {
		t.Fatal(err)
	}
	if !anyToBool(replay["idempotent_replay"]) || anyToString(anyMap(replay["identity_update"])["state"]) != agentWorkerIdentityStateAcknowledged {
		t.Fatalf("restart replay was not an acknowledged idempotent response: %#v", replay)
	}
}

func TestWorkerIdentityDefaultLauncherSharedRootAllocatesAndAcknowledgesDistinctInstances(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	key := "default-launcher-worker-key"
	server := &server{taskLedger: ledger, orchestratorAPIKey: key}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buildMux(server).ServeHTTP(w, r)
	}))
	defer gateway.Close()
	stateRoot, err := os.MkdirTemp(os.Getenv("TMPDIR"), "worker-identity-launcher-state-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(stateRoot)
	worktreeRoot, err := os.MkdirTemp(os.Getenv("TMPDIR"), "worker-identity-launcher-worktree-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(worktreeRoot)

	filteredEnv := make([]string, 0, len(os.Environ()))
	blockedEnv := map[string]struct{}{
		"TASK_AGENT_DISPATCHER_ID": {}, "TASK_WORKER_DISPATCHER_ID": {},
		"CONTEXTLATTICE_TASK_WORKER_DISPATCHER_ID": {}, "TASK_WORKER_INSTANCE": {},
	}
	for _, item := range os.Environ() {
		name := strings.SplitN(item, "=", 2)[0]
		if _, blocked := blockedEnv[name]; !blocked {
			filteredEnv = append(filteredEnv, item)
		}
	}
	filteredEnv = append(filteredEnv,
		"CONTEXTLATTICE_ORCHESTRATOR_URL="+gateway.URL,
		"CONTEXTLATTICE_ORCHESTRATOR_API_KEY="+key,
		"CONTEXTLATTICE_WORKER_API_KEY="+key,
		"TASK_AGENT_WORKER_STATE_ROOT="+stateRoot,
		"CONTEXTLATTICE_TASK_WORKTREE_ROOT="+worktreeRoot,
		"TASK_WORKER=shared-default-worker",
		"TASK_WORKER_PRINCIPAL=default-launcher-principal",
		"TASK_WORKER_WORKSPACE=default-launcher-workspace",
		"TASK_MODEL_PROVIDER=auto",
		"TASK_MODEL=qwen3.5:9b",
		"PYTHONDONTWRITEBYTECODE=1",
	)
	repoRoot := filepath.Join("..", "..")
	launcherExecutable := ""
	launcherArgs := []string(nil)
	if zshPath, lookErr := exec.LookPath("zsh"); lookErr == nil {
		launcherExecutable = zshPath
		launcherArgs = []string{"-f", "scripts/launch_task_agent.sh", "--once"}
	} else {
		pythonPath, pythonErr := exec.LookPath("python3")
		if pythonErr != nil {
			t.Fatalf("default launcher test requires zsh or python3: zsh=%v python3=%v", lookErr, pythonErr)
		}
		// The zsh launcher is a thin argv boundary whose final action is this
		// exact Python worker invocation. Exercise that same lifecycle directly
		// on minimal Linux runners that do not install zsh.
		launcherExecutable = pythonPath
		launcherArgs = []string{"scripts/task_agent_worker.py", "--once"}
	}
	runLauncher := func() ([]byte, error) {
		command := exec.Command(launcherExecutable, launcherArgs...)
		command.Dir = repoRoot
		command.Env = filteredEnv
		return command.CombinedOutput()
	}

	// Keep one requested ID occupied so the concurrent launches exercise the
	// real server-issued canonical update and Python ACK path. They must still
	// receive distinct instance/canonical identities.
	seedAuthority := identityTestAuthority("default-launcher-seed", "default-launcher-workspace", "default-launcher-seed-instance")
	_, seedIdentity := registerIdentityForTest(t, ledger, seedAuthority, "shared-default-worker")
	launches := make(chan struct {
		output []byte
		err    error
	}, 2)
	var launchGroup sync.WaitGroup
	for index := 0; index < 2; index++ {
		launchGroup.Add(1)
		go func() {
			defer launchGroup.Done()
			output, runErr := runLauncher()
			launches <- struct {
				output []byte
				err    error
			}{output: output, err: runErr}
		}()
	}
	launchGroup.Wait()
	close(launches)
	for result := range launches {
		if result.err != nil {
			t.Fatalf("simultaneous default launcher did not complete real register/claim lifecycle: %v (%s)", result.err, strings.TrimSpace(string(result.output)))
		}
	}
	retireIdentityForTest(t, ledger, seedAuthority, seedIdentity)

	// A clean unkeyed launcher retires its server identity before releasing the
	// local slot. More than the historical 128-slot bound must therefore keep
	// working against one real SQLite Gateway without resurrecting a prior
	// instance.
	const sequentialLaunchCount = 130
	for index := 0; index < sequentialLaunchCount; index++ {
		output, runErr := runLauncher()
		if runErr != nil {
			t.Fatalf("sequential default launcher %d did not complete real retirement lifecycle: %v (%s)", index, runErr, strings.TrimSpace(string(output)))
		}
	}
	rows, err := ledger.db.Query(`SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE principal_id=? AND workspace_id=? AND requested_worker_id=? ORDER BY worker_instance_id`, "default-launcher-principal", "default-launcher-workspace", "shared-default-worker")
	if err != nil {
		t.Fatal(err)
	}
	identities := make([]agentWorkerIdentityRecord, 0, sequentialLaunchCount+2)
	for rows.Next() {
		identity, scanErr := scanWorkerIdentity(rows)
		if scanErr != nil {
			_ = rows.Close()
			t.Fatal(scanErr)
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(identities) != sequentialLaunchCount+2 {
		t.Fatalf("default launcher did not retain every distinct server identity: got=%d want=%d", len(identities), sequentialLaunchCount+2)
	}
	instances := map[string]struct{}{}
	canonicals := map[string]struct{}{}
	for _, identity := range identities {
		instances[identity.WorkerInstanceID] = struct{}{}
		canonicals[identity.CanonicalWorkerID] = struct{}{}
		if identity.Status != "closed" || identity.ClosedAt == "" || !strings.HasPrefix(identity.CanonicalWorkerID, "closed-") {
			t.Fatalf("default launcher did not durably close and tombstone every identity: %#v", identity)
		}
		if identity.AcknowledgedGeneration != identity.IdentityUpdateGeneration {
			t.Fatalf("default launcher left an identity update unacknowledged: %#v", identity)
		}
	}
	if len(instances) != sequentialLaunchCount+2 || len(canonicals) != sequentialLaunchCount+2 {
		t.Fatalf("default launcher merged or reused worker identity: identities=%#v", identities)
	}
	var acknowledgedUpdates int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identity_updates WHERE state=?`, agentWorkerIdentityStateAcknowledged).Scan(&acknowledgedUpdates); err != nil {
		t.Fatal(err)
	}
	if acknowledgedUpdates != 2 {
		t.Fatalf("expected two simultaneous collision updates to be durably acknowledged, got %d", acknowledgedUpdates)
	}
	var retirementCount, closedCount int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identity_retirements WHERE workspace_id=? AND principal_id=?`, "default-launcher-workspace", "default-launcher-principal").Scan(&retirementCount); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE workspace_id=? AND principal_id=? AND status='closed'`, "default-launcher-workspace", "default-launcher-principal").Scan(&closedCount); err != nil {
		t.Fatal(err)
	}
	if retirementCount != sequentialLaunchCount+2 || closedCount != sequentialLaunchCount+2 {
		t.Fatalf("server retirement did not persist every closed identity: retirements=%d closed=%d", retirementCount, closedCount)
	}
	stateFiles, err := filepath.Glob(filepath.Join(stateRoot, "worker_identity*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(stateFiles) != 1 {
		t.Fatalf("default launcher did not retain a clean retirement marker for the reusable slot: %v", stateFiles)
	}
	retiredRaw, err := os.ReadFile(stateFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	var retiredState map[string]any
	if err := json.Unmarshal(retiredRaw, &retiredState); err != nil {
		t.Fatal(err)
	}
	if !anyToBool(retiredState["retired"]) {
		t.Fatalf("default launcher did not write an explicit clean retirement marker: %#v", retiredState)
	}

	// The requested canonical is now free, but none of the old tombstoned
	// instances may be resurrected. A new instance may reclaim the requested
	// canonical only through a fresh active registration.
	reclaimAuthority := identityTestAuthority("default-launcher-principal", "default-launcher-workspace", "post-retirement-instance")
	response, err := ledger.registerWorkerIdentity(context.Background(), reclaimAuthority.PrincipalID, reclaimAuthority.WorkspaceID, "shared-default-worker", reclaimAuthority.WorkerInstanceID)
	if err != nil || anyToString(anyMap(response["identity"])["canonical_worker_id"]) != "shared-default-worker" {
		t.Fatalf("fresh identity did not reclaim the released requested canonical: response=%#v err=%v", response, err)
	}
	reclaimed, err := ledger.workerIdentityByAuthority(context.Background(), reclaimAuthority)
	if err != nil || reclaimed.Status != "active" || reclaimed.CanonicalWorkerID != "shared-default-worker" {
		t.Fatalf("fresh identity was not active on the released canonical: identity=%#v err=%v", reclaimed, err)
	}
	if _, err := ledger.registerWorkerIdentity(context.Background(), reclaimAuthority.PrincipalID, reclaimAuthority.WorkspaceID, "shared-default-worker", identities[0].WorkerInstanceID); err == nil {
		t.Fatal("closed identity was resurrected by re-registering its old instance")
	}
	receipt := retireIdentityForTest(t, ledger, reclaimAuthority, reclaimed)
	replay := retireIdentityForTest(t, ledger, reclaimAuthority, reclaimed)
	if !anyToBool(replay["idempotent_replay"]) || anyToString(replay["closed_identity_digest"]) != anyToString(receipt["closed_identity_digest"]) || anyToString(replay["closed_status"]) != "closed" {
		t.Fatalf("retirement receipt replay did not bind the exact closed tombstone: first=%#v replay=%#v", receipt, replay)
	}
	if err := ledger.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart ledger after committed retirement: %v", err)
	}
	defer restarted.close()
	restartedReplay := retireIdentityForTest(t, restarted, reclaimAuthority, reclaimed)
	if !anyToBool(restartedReplay["idempotent_replay"]) || anyToString(restartedReplay["closed_identity_digest"]) != anyToString(receipt["closed_identity_digest"]) {
		t.Fatalf("restart retirement replay did not verify the durable tombstone: %#v", restartedReplay)
	}
}

func TestWorkerIdentityRetirementRejectsQueuedCanonicalAssignmentAndTampering(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	authority := identityTestAuthority("retirement-queue-principal", "retirement-queue-workspace", "retirement-queue-instance")
	_, identity := registerIdentityForTest(t, ledger, authority, "retirement-queue-worker")
	manifest := testAgentTaskManifest("retirement-queue-task", "retirement-queue-project", "retirement-queue-owner", "retirement-queue-session")
	manifest["workspace_id"] = authority.WorkspaceID
	manifest["metadata"] = map[string]any{"worker": identity.CanonicalWorkerID}
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit queued canonical assignment: %v", err)
	}
	request := workerIdentityRetirementPayloadForTest(identity)
	if _, err := ledger.retireWorkerIdentity(ctx, request, authority); err == nil || !strings.Contains(err.Error(), "nonterminal") {
		t.Fatalf("queued canonical assignment did not block retirement: %v", err)
	}
	var status, canonical, digest string
	if err := ledger.db.QueryRow(`SELECT status,canonical_worker_id,identity_digest FROM task_ledger_worker_identities WHERE identity_id=?`, identity.IdentityID).Scan(&status, &canonical, &digest); err != nil {
		t.Fatal(err)
	}
	if status != "active" || canonical != identity.CanonicalWorkerID || digest != identity.IdentityDigest {
		t.Fatalf("queued retirement failure mutated identity state: status=%q canonical=%q digest=%q", status, canonical, digest)
	}
	var retirementRows int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identity_retirements WHERE identity_id=?`, identity.IdentityID).Scan(&retirementRows); err != nil {
		t.Fatal(err)
	}
	if retirementRows != 0 {
		t.Fatalf("queued retirement failure wrote a retirement receipt: %d", retirementRows)
	}

	// A same-requested-ID process receives a distinct canonical identity and
	// cannot inherit the old queued row whose assignment has no instance key.
	newAuthority := identityTestAuthority(authority.PrincipalID, authority.WorkspaceID, "retirement-queue-new-instance")
	newResponse, err := ledger.registerWorkerIdentity(ctx, newAuthority.PrincipalID, newAuthority.WorkspaceID, identity.RequestedWorkerID, newAuthority.WorkerInstanceID)
	if err != nil {
		t.Fatalf("register replacement requested worker: %v", err)
	}
	if anyToBool(newResponse["identity_update_required"]) {
		update, readErr := ledger.readWorkerIdentityUpdate(ctx, newAuthority, "")
		if readErr != nil {
			t.Fatalf("read replacement identity update: %v", readErr)
		}
		acknowledgeIdentityForTest(t, ledger, newAuthority, update.UpdateID)
	}
	newIdentity, err := ledger.workerIdentityByAuthority(ctx, newAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if newIdentity.CanonicalWorkerID == identity.CanonicalWorkerID {
		t.Fatalf("replacement identity inherited the queued canonical assignment: %#v", newIdentity)
	}
	claim, err := ledger.claimTaskWithIdentity(ctx, newIdentity.CanonicalWorkerID, newAuthority.WorkerInstanceID, newAuthority.WorkspaceID, "", newIdentity.IdentityUpdateGeneration)
	if err != nil {
		t.Fatalf("replacement identity claim failed closed: %v", err)
	}
	if claim != nil {
		t.Fatalf("replacement identity inherited the old queued assignment: %#v", claim)
	}

	// Stale, copied, and foreign authority material cannot close the identity
	// or create a tombstone row.
	stale := workerIdentityRetirementPayloadForTest(identity)
	stale["worker_identity_update_generation"] = identity.IdentityUpdateGeneration + 1
	stale["acknowledged_generation"] = identity.AcknowledgedGeneration + 1
	if _, err := ledger.retireWorkerIdentity(ctx, stale, authority); err == nil {
		t.Fatal("stale retirement generation was accepted")
	}
	foreign := workerIdentityRetirementPayloadForTest(identity)
	foreignAuthority := identityTestAuthority("foreign-principal", authority.WorkspaceID, authority.WorkerInstanceID)
	if _, err := ledger.retireWorkerIdentity(ctx, foreign, foreignAuthority); err == nil {
		t.Fatal("foreign retirement authority was accepted")
	}
	copyRequest := workerIdentityRetirementPayloadForTest(identity)
	copyRequest["retirement_digest"] = agentTaskDigest(map[string]any{"copied": true})
	if _, err := ledger.retireWorkerIdentity(ctx, copyRequest, authority); err == nil {
		t.Fatal("tampered copied retirement receipt was accepted")
	}
	if err := ledger.db.QueryRow(`SELECT status FROM task_ledger_worker_identities WHERE identity_id=?`, identity.IdentityID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "active" {
		t.Fatalf("tampered retirement changed identity status: %q", status)
	}
}

func TestWorkerIdentityRetirementRejectsUnkeyedAttemptDownstreamAuthority(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	authority := identityTestAuthority("retirement-unkeyed-principal", "retirement-unkeyed-workspace", "retirement-unkeyed-instance")
	_, identity := registerIdentityForTest(t, ledger, authority, "retirement-unkeyed-worker")

	manifest := testAgentTaskManifest("retirement-unkeyed-task", "retirement-unkeyed-project", "retirement-unkeyed-reviewer", "retirement-unkeyed-session")
	manifest["workspace_id"] = authority.WorkspaceID
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit unkeyed task: %v", err)
	}
	var storedClaimWorker string
	if err := ledger.db.QueryRowContext(ctx, `SELECT claim_worker_id FROM task_ledger_tasks WHERE id=?`, "retirement-unkeyed-task").Scan(&storedClaimWorker); err != nil {
		t.Fatal(err)
	}
	if storedClaimWorker != "" {
		t.Fatalf("unkeyed task unexpectedly acquired a routing identity: %q", storedClaimWorker)
	}
	claim, err := ledger.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, authority.WorkerInstanceID, authority.WorkspaceID, "", identity.IdentityUpdateGeneration)
	if err != nil || claim == nil {
		t.Fatalf("claim exact unkeyed task: claim=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	if fence.WorkerID != identity.CanonicalWorkerID || fence.WorkerInstanceID != authority.WorkerInstanceID {
		t.Fatalf("claim did not retain exact worker authority: %#v", fence)
	}
	if _, err := ledger.heartbeat(ctx, fence); err != nil {
		t.Fatalf("start unkeyed attempt: %v", err)
	}
	exitCode := 0
	if _, err := ledger.observe(ctx, fence, "succeeded", &exitCode, map[string]any{"source": "worker-identity-retirement"}); err != nil {
		t.Fatalf("observe unkeyed attempt: %v", err)
	}

	resultID := "result-retirement-unkeyed"
	publicationID := "publication-retirement-unkeyed"
	workspaceRef := "workspace-ref-retirement-unkeyed"
	publication, err := ledger.stagePublication(ctx, fence, map[string]any{
		"publication_id": publicationID, "idempotency_key": "task-result:" + resultID, "runner_exit_required": true,
		"result": map[string]any{
			"result_id": resultID, "summary": "unkeyed result", "output": "pending downstream reconciliation",
			"context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
			"workspace":         map[string]any{"workspace_ref": workspaceRef},
			"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, workspaceRef)},
		},
	})
	if err != nil {
		t.Fatalf("stage unkeyed publication: %v", err)
	}
	var deliveryID, resultDigest string
	if err := ledger.db.QueryRowContext(ctx, `SELECT delivery_id FROM task_ledger_deliveries WHERE publication_id=?`, publicationID).Scan(&deliveryID); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRowContext(ctx, `SELECT digest FROM task_ledger_results WHERE result_id=?`, resultID).Scan(&resultDigest); err != nil {
		t.Fatal(err)
	}
	if anyToString(publication["publication_id"]) != publicationID || resultDigest == "" {
		t.Fatalf("staged publication lost exact downstream identifiers: publication=%#v digest=%q", publication, resultDigest)
	}

	// Build the real downstream chain through the ledger APIs. The direct
	// status updates below model each exact reconciliation boundary in
	// isolation; they never mutate the worker identity or its retirement row.
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='execution_succeeded',updated_at=? WHERE id=?`, agentTaskNow(), fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.claimReview(ctx, fence.TaskID, resultID, deliveryID, "retirement-unkeyed-reviewer"); err != nil {
		t.Fatalf("create reviewer claim: %v", err)
	}
	if _, err := ledger.review(ctx, fence.TaskID, resultID, "retirement-unkeyed-reviewer", "block", "pending downstream work", ""); err != nil {
		t.Fatalf("create pending review action: %v", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='integrated',updated_at=? WHERE id=?`, agentTaskNow(), fence.TaskID); err != nil {
		t.Fatal(err)
	}

	retirement := workerIdentityRetirementPayloadForTest(identity)
	if _, err := ledger.retireWorkerIdentity(ctx, retirement, authority); err == nil || !strings.Contains(err.Error(), "attempt") || !strings.Contains(err.Error(), "execution_observed") {
		t.Fatalf("execution-observed exact attempt did not block retirement: %v", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_attempts SET status='completed',completed_at=? WHERE attempt_id=?`, agentTaskNow(), fence.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.retireWorkerIdentity(ctx, retirement, authority); err == nil || !strings.Contains(err.Error(), "result") || !strings.Contains(err.Error(), "publication_pending") {
		t.Fatalf("pending result did not block retirement after terminal attempt: %v", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_results SET status='result_published' WHERE result_id=?`, resultID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.retireWorkerIdentity(ctx, retirement, authority); err == nil || !strings.Contains(err.Error(), "publication") || !strings.Contains(err.Error(), "writeback_pending") {
		t.Fatalf("pending publication did not block unkeyed retirement: %v", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_publications SET status='committed',writeback_status='committed',updated_at=? WHERE publication_id=?`, agentTaskNow(), publicationID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='integrated',updated_at=? WHERE id=?`, agentTaskNow(), fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.retireWorkerIdentity(ctx, retirement, authority); err == nil || !strings.Contains(err.Error(), "delivery") || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending delivery did not block unkeyed retirement: %v", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_deliveries SET status='delivered',updated_at=? WHERE delivery_id=?`, agentTaskNow(), deliveryID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.retireWorkerIdentity(ctx, retirement, authority); err == nil || !strings.Contains(err.Error(), "reviewer claim") {
		t.Fatalf("active reviewer claim did not block retirement: %v", err)
	}
	// The owner closes the durable claim through the terminal review API. The
	// claim row is retained for audit and the review/claim transition commits
	// atomically; no DELETE is a valid reconciliation.
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='review_pending',updated_at=? WHERE id=?`, agentTaskNow(), fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.review(ctx, fence.TaskID, resultID, "retirement-unkeyed-reviewer", "accept", "terminal downstream review", ""); err != nil {
		t.Fatalf("complete terminal review and claim custody: %v", err)
	}
	var claimStatus, reviewStatus string
	if err := ledger.db.QueryRowContext(ctx, `SELECT c.status,r.status FROM task_ledger_reviewer_claims c JOIN task_ledger_reviews r ON r.result_id=c.result_id AND r.task_id=c.task_id WHERE c.result_id=?`, resultID).Scan(&claimStatus, &reviewStatus); err != nil {
		t.Fatal(err)
	}
	if claimStatus != "accepted_for_integration" || reviewStatus != "accepted_for_integration" {
		t.Fatalf("terminal review did not close the exact durable claim: claim=%q review=%q", claimStatus, reviewStatus)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='integrated',updated_at=? WHERE id=?`, agentTaskNow(), fence.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.db.ExecContext(ctx, `INSERT INTO task_ledger_integrations(integration_id,result_id,task_id,action,status,actor,digest,created_at) VALUES(?,?,?,?,?,?,?,?)`, "integration-retirement-unkeyed", resultID, fence.TaskID, "test_pending", "pending", "retirement-unkeyed-reviewer", resultDigest, agentTaskNow()); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.retireWorkerIdentity(ctx, retirement, authority); err == nil || !strings.Contains(err.Error(), "integration") || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending integration did not block retirement: %v", err)
	}
	if _, err := ledger.db.ExecContext(ctx, `UPDATE task_ledger_integrations SET status='unintegrated' WHERE integration_id=?`, "integration-retirement-unkeyed"); err != nil {
		t.Fatal(err)
	}
	cleanupReceipt := map[string]any{
		"schema_id": agentTaskCleanupReceiptID, "receipt_id": "cleanup-receipt-retirement-unkeyed",
		"authority": "task-execution-worker", "state": "cleaned", "cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, workspaceRef),
		"workspace_ref": workspaceRef, "publication_id": publicationID, "result_id": resultID,
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"generation": fence.Generation, "worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID,
	}
	cleanupReceipt["receipt_digest"] = agentTaskDigest(cleanupReceipt)
	if _, err := ledger.acknowledgeCleanup(ctx, fence.TaskID, fence.AttemptID, map[string]any{"cleanup_receipt": cleanupReceipt}); err != nil {
		t.Fatalf("record exact durable cleanup receipt: %v", err)
	}
	if receipt, err := ledger.retireWorkerIdentity(ctx, retirement, authority); err != nil || !anyToBool(receipt["retired"]) || anyToString(receipt["closed_status"]) != "closed" {
		t.Fatalf("exact downstream reconciliation did not permit retirement: receipt=%#v err=%v", receipt, err)
	}
}

func identityTestLedgers(t *testing.T) (*agentTaskDeliveryLedger, *agentTaskDeliveryLedger) {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "agent_tasks.sqlite3")
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", dbPath)
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	first, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("open first task ledger: %v", err)
	}
	second, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		_ = first.close()
		t.Fatalf("open second task ledger: %v", err)
	}
	t.Cleanup(func() {
		_ = second.close()
		_ = first.close()
	})
	return first, second
}

func identityTestAuthority(principal, workspace, instance string) agentWorkerIdentityAuthority {
	return agentWorkerIdentityAuthority{PrincipalID: principal, WorkspaceID: workspace, WorkerInstanceID: instance}
}

func identityTestAckPayload(update agentWorkerIdentityUpdateRecord, authority agentWorkerIdentityAuthority) map[string]any {
	canonicalAuthority, err := normalizeWorkerIdentityAuthority(authority.PrincipalID, authority.WorkspaceID, authority.WorkerInstanceID)
	if err != nil {
		panic(err)
	}
	return agentTaskContractPayload(agentWorkerIdentityAckContractID, map[string]any{
		"schema_id": agentWorkerIdentityAckContractID, "contract_version": 1,
		"update_id": update.UpdateID, "identity_id": update.IdentityID,
		"principal_id": canonicalAuthority.PrincipalID, "workspace_id": canonicalAuthority.WorkspaceID, "worker_instance_id": canonicalAuthority.WorkerInstanceID,
		"requested_worker_id": update.RequestedWorkerID, "old_worker_id": update.OldWorkerID,
		"canonical_worker_id": update.CanonicalWorkerID, "new_worker_id": update.NewWorkerID,
		"worker_identity_update_generation": update.IdentityUpdateGeneration,
		"update_digest":                     update.UpdateDigest, "receipt_digest": update.ReceiptDigest,
		"acknowledged": true, "ack_receipt_digest": workerIdentityAckReceiptDigest(update, canonicalAuthority),
		"idempotent_replay": false,
		"identity_update":   update.payload(),
	})
}

func registerIdentityForTest(t *testing.T, ledger *agentTaskDeliveryLedger, authority agentWorkerIdentityAuthority, requested string) (map[string]any, agentWorkerIdentityRecord) {
	t.Helper()
	response, err := ledger.registerWorkerIdentity(context.Background(), authority.PrincipalID, authority.WorkspaceID, requested, authority.WorkerInstanceID)
	if err != nil {
		t.Fatalf("register worker identity: %v", err)
	}
	identity, err := ledger.workerIdentityByAuthority(context.Background(), authority)
	if err != nil {
		t.Fatalf("read registered worker identity: %v", err)
	}
	return response, identity
}

func acknowledgeIdentityForTest(t *testing.T, ledger *agentTaskDeliveryLedger, authority agentWorkerIdentityAuthority, updateID string) agentWorkerIdentityUpdateRecord {
	t.Helper()
	update, err := ledger.readWorkerIdentityUpdate(context.Background(), authority, updateID)
	if err != nil {
		t.Fatalf("read worker identity update: %v", err)
	}
	if update.UpdateID == "" {
		t.Fatal("worker identity update was empty")
	}
	if _, err := ledger.acknowledgeWorkerIdentityUpdate(context.Background(), identityTestAckPayload(update, authority), authority); err != nil {
		t.Fatalf("acknowledge worker identity update: %v", err)
	}
	return update
}

func retireIdentityForTest(t *testing.T, ledger *agentTaskDeliveryLedger, authority agentWorkerIdentityAuthority, identity agentWorkerIdentityRecord) map[string]any {
	t.Helper()
	receipt, err := ledger.retireWorkerIdentity(context.Background(), workerIdentityRetirementPayloadForTest(identity), authority)
	if err != nil {
		t.Fatalf("retire worker identity: %v", err)
	}
	return receipt
}

func workerIdentityRetirementPayloadForTest(identity agentWorkerIdentityRecord) map[string]any {
	return agentTaskContractPayload(agentWorkerIdentityRetireContractID, map[string]any{
		"schema_id": agentWorkerIdentityRetireContractID, "contract_version": 1,
		"identity_id": identity.IdentityID, "principal_id": identity.PrincipalID, "workspace_id": identity.WorkspaceID,
		"requested_worker_id": identity.RequestedWorkerID, "canonical_worker_id": identity.CanonicalWorkerID, "worker_instance_id": identity.WorkerInstanceID,
		"worker_identity_update_generation": identity.IdentityUpdateGeneration, "acknowledged_generation": identity.AcknowledgedGeneration,
		"identity_digest": identity.IdentityDigest, "retirement_digest": workerIdentityRetirementDigest(identity), "retired": true,
	})
}

func TestWorkerIdentityConcurrentRegistrationRetainsFirstAndReplays(t *testing.T) {
	first, second := identityTestLedgers(t)
	ctx := context.Background()
	authorities := []agentWorkerIdentityAuthority{
		identityTestAuthority("principal-a", "workspace-shared", "instance-a"),
		identityTestAuthority("principal-b", "workspace-shared", "instance-b"),
	}
	type result struct {
		response map[string]any
		err      error
	}
	results := make(chan result, 2)
	var group sync.WaitGroup
	for index, ledger := range []*agentTaskDeliveryLedger{first, second} {
		group.Add(1)
		go func(index int, ledger *agentTaskDeliveryLedger) {
			defer group.Done()
			response, err := ledger.registerWorkerIdentity(ctx, authorities[index].PrincipalID, authorities[index].WorkspaceID, "shared-worker", authorities[index].WorkerInstanceID)
			results <- result{response: response, err: err}
		}(index, ledger)
	}
	group.Wait()
	close(results)

	identities := make([]map[string]any, 0, 2)
	for item := range results {
		if item.err != nil {
			t.Fatalf("concurrent registration failed: %v", item.err)
		}
		identities = append(identities, anyMap(item.response["identity"]))
	}
	if len(identities) != 2 {
		t.Fatalf("expected two registrations, got %d", len(identities))
	}
	canonical := map[string]bool{}
	requestedCount := 0
	for _, identity := range identities {
		requested := anyToString(identity["requested_worker_id"])
		current := anyToString(identity["canonical_worker_id"])
		if requested != "shared-worker" || current == "" {
			t.Fatalf("incomplete identity response: %#v", identity)
		}
		canonical[current] = true
		if current == requested {
			requestedCount++
		}
	}
	if len(canonical) != 2 || requestedCount != 1 {
		t.Fatalf("collision did not produce one retained and one deterministic distinct canonical: %#v", identities)
	}

	replay, err := first.registerWorkerIdentity(ctx, authorities[0].PrincipalID, authorities[0].WorkspaceID, "shared-worker", authorities[0].WorkerInstanceID)
	if err != nil {
		t.Fatalf("registration replay failed: %v", err)
	}
	if !anyToBool(replay["idempotent_replay"]) {
		t.Fatalf("registration replay was not marked idempotent: %#v", replay)
	}
	if anyToString(anyMap(replay["identity"])["canonical_worker_id"]) == "" {
		t.Fatal("registration replay lost canonical identity")
	}
}

func TestWorkerIdentityCollisionCanonicalClaimExecutePublishProof(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	occupierAuthority := identityTestAuthority("collision-proof-occupier", "collision-proof-workspace", "collision-proof-occupier-instance")
	registerIdentityForTest(t, ledger, occupierAuthority, "collision-proof-worker")
	authority := identityTestAuthority("collision-proof-owner", "collision-proof-workspace", "collision-proof-owner-instance")
	response, identity := registerIdentityForTest(t, ledger, authority, "collision-proof-worker")
	if !anyToBool(response["identity_update_required"]) || identity.CanonicalWorkerID == identity.RequestedWorkerID {
		t.Fatalf("collision did not produce a pending canonical update: response=%#v identity=%#v", response, identity)
	}
	update := acknowledgeIdentityForTest(t, ledger, authority, anyToString(anyMap(response["identity_update"])["update_id"]))
	if update.IdentityUpdateGeneration != identity.IdentityUpdateGeneration || update.CanonicalWorkerID != identity.CanonicalWorkerID {
		t.Fatalf("acknowledged collision update changed canonical binding: update=%#v identity=%#v", update, identity)
	}
	identity, err := ledger.workerIdentityByAuthority(ctx, authority)
	if err != nil {
		t.Fatal(err)
	}
	manifest := testAgentTaskManifest("collision-proof-task", "collision-proof-project", "collision-proof-reviewer", "collision-proof-session")
	manifest["workspace_id"] = authority.WorkspaceID
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit collision proof task: %v", err)
	}
	claim, err := ledger.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, authority.WorkerInstanceID, authority.WorkspaceID, "", identity.IdentityUpdateGeneration)
	if err != nil || claim == nil {
		t.Fatalf("canonical collision claim failed: claim=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	fence.WorkerIdentityUpdateGeneration = identity.IdentityUpdateGeneration
	claimIdentityGeneration := anyToInt(anyMap(claim["lease"])["worker_identity_update_generation"], -1)
	if fence.WorkerID != identity.CanonicalWorkerID || fence.WorkerInstanceID != authority.WorkerInstanceID || claimIdentityGeneration != identity.IdentityUpdateGeneration {
		t.Fatalf("canonical claim fence was not server-bound: fence=%#v claim_generation=%d identity=%#v", fence, claimIdentityGeneration, identity)
	}
	if _, err := ledger.heartbeat(ctx, fence); err != nil {
		t.Fatalf("canonical collision execution start failed: %v", err)
	}
	exitCode := 0
	if _, err := ledger.observe(ctx, fence, "succeeded", &exitCode, map[string]any{"proof": "collision-canonical"}); err != nil {
		t.Fatalf("canonical collision execution observation failed: %v", err)
	}
	publication, err := ledger.stagePublication(ctx, fence, map[string]any{
		"publication_id": "publication-collision-proof", "idempotency_key": "task-result:collision-proof-result", "runner_exit_required": true,
		"result": map[string]any{
			"result_id": "collision-proof-result", "summary": "collision canonical proof", "output": "canonical execution published",
			"context_pack_hash": anyToString(anyMap(claim["attempt"])["context_pack_hash"]),
			"workspace":         map[string]any{"workspace_ref": "workspace-ref-collision-proof"},
			"cleanup":           map[string]any{"cleanup_id": agentTaskCleanupID(fence.TaskID, fence.AttemptID, "workspace-ref-collision-proof")},
		},
	})
	if err != nil {
		t.Fatalf("canonical collision publication failed: %v", err)
	}
	if anyToString(publication["publication_id"]) != "publication-collision-proof" {
		t.Fatalf("canonical publication response lost publication identity: %#v", publication)
	}
	var storedWorker, storedInstance string
	var storedGeneration int
	if err := ledger.db.QueryRowContext(ctx, `SELECT a.worker_id,a.worker_instance_id,a.worker_identity_update_generation FROM task_ledger_attempts a WHERE a.attempt_id=?`, fence.AttemptID).Scan(&storedWorker, &storedInstance, &storedGeneration); err != nil {
		t.Fatal(err)
	}
	if storedWorker != identity.CanonicalWorkerID || storedInstance != authority.WorkerInstanceID || storedGeneration != identity.IdentityUpdateGeneration {
		t.Fatalf("durable execution evidence did not retain canonical identity generation: worker=%q instance=%q generation=%d identity=%#v", storedWorker, storedInstance, storedGeneration, identity)
	}
	var publicationWorker, publicationInstance string
	if err := ledger.db.QueryRowContext(ctx, `SELECT a.worker_id,a.worker_instance_id FROM task_ledger_publications p JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id WHERE p.publication_id=?`, "publication-collision-proof").Scan(&publicationWorker, &publicationInstance); err != nil {
		t.Fatal(err)
	}
	if publicationWorker != identity.CanonicalWorkerID || publicationInstance != authority.WorkerInstanceID {
		t.Fatalf("durable publication evidence did not retain canonical identity: worker=%q instance=%q", publicationWorker, publicationInstance)
	}
}

func TestWorkerIdentityRetirementClaimTwoHandleInterleaving(t *testing.T) {
	first, second := identityTestLedgers(t)
	ctx := context.Background()
	authority := identityTestAuthority("interleave-principal", "interleave-workspace", "interleave-instance")
	_, identity := registerIdentityForTest(t, first, authority, "interleave-worker")
	manifest := testAgentTaskManifest("interleave-task", "interleave-project", "interleave-reviewer", "interleave-session")
	manifest["workspace_id"] = authority.WorkspaceID
	if _, err := first.submit(ctx, manifest); err != nil {
		t.Fatalf("submit interleaving task: %v", err)
	}
	retirement := workerIdentityRetirementPayloadForTest(identity)
	type outcome struct {
		claim     map[string]any
		claimErr  error
		receipt   map[string]any
		retireErr error
	}
	started := make(chan struct{}, 2)
	start := make(chan struct{})
	results := make(chan outcome, 2)
	go func() {
		started <- struct{}{}
		<-start
		claim, err := first.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, authority.WorkerInstanceID, authority.WorkspaceID, "", identity.IdentityUpdateGeneration)
		results <- outcome{claim: claim, claimErr: err}
	}()
	go func() {
		started <- struct{}{}
		<-start
		receipt, err := second.retireWorkerIdentity(ctx, retirement, authority)
		results <- outcome{receipt: receipt, retireErr: err}
	}()
	<-started
	<-started
	close(start)
	var claimResult, retireResult outcome
	for range 2 {
		result := <-results
		if result.claimErr != nil || result.claim != nil {
			claimResult = result
		} else {
			retireResult = result
		}
	}
	if claimResult.claimErr == nil && claimResult.claim == nil {
		t.Fatal("interleaving claim returned neither a claim nor an error")
	}
	if retireResult.retireErr == nil && !anyToBool(retireResult.receipt["retired"]) {
		t.Fatalf("interleaving retirement returned an invalid receipt: %#v", retireResult.receipt)
	}
	if retireResult.retireErr == nil {
		if claimResult.claimErr == nil || claimResult.claim != nil || !strings.Contains(strings.ToLower(claimResult.claimErr.Error()), "closed") {
			t.Fatalf("retire-before-claim interleaving allowed stale custody: claim=%#v err=%v receipt=%#v", claimResult.claim, claimResult.claimErr, retireResult.receipt)
		}
	} else if claimResult.claimErr == nil && claimResult.claim != nil {
		if !strings.Contains(strings.ToLower(retireResult.retireErr.Error()), "task") && !strings.Contains(strings.ToLower(retireResult.retireErr.Error()), "attempt") {
			t.Fatalf("claim-before-retire interleaving did not block retirement on active custody: %v", retireResult.retireErr)
		}
		var attempts int
		if err := first.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_attempts WHERE task_id=?`, "interleave-task").Scan(&attempts); err != nil {
			t.Fatal(err)
		}
		if attempts != 1 {
			t.Fatalf("claim-before-retire interleaving created unexpected attempt count: %d", attempts)
		}
	} else {
		t.Fatalf("interleaving produced two failures: claim=%v retirement=%v", claimResult.claimErr, retireResult.retireErr)
	}
}

func TestWorkerIdentityRegistrationRejectsNonPublicLeaseIDsBeforeDurableWrite(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	authority := identityTestAuthority("grammar-principal", "grammar-workspace", "grammar-instance")
	var before int
	if err := ledger.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_worker_identities`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, requested := range []string{"bad/worker", "~worker", "é-worker", ""} {
		if _, err := ledger.registerWorkerIdentity(ctx, authority.PrincipalID, authority.WorkspaceID, requested, authority.WorkerInstanceID); err == nil {
			t.Fatalf("non-public requested worker ID %q was accepted", requested)
		}
	}
	var after int
	if err := ledger.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_worker_identities`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("invalid requested worker IDs changed durable identity count: before=%d after=%d", before, after)
	}
}

func TestWorkerIdentityMixedCaseWorkspaceCollisionReplayAndMigrationConflict(t *testing.T) {
	first, second := identityTestLedgers(t)
	ctx := context.Background()
	authorities := []agentWorkerIdentityAuthority{
		identityTestAuthority("mixed-principal-a", "Workspace-X", "mixed-instance-a"),
		identityTestAuthority("mixed-principal-b", "workspace-x", "mixed-instance-b"),
	}
	type registrationResult struct {
		response map[string]any
		err      error
	}
	results := make(chan registrationResult, 2)
	var group sync.WaitGroup
	for index, ledger := range []*agentTaskDeliveryLedger{first, second} {
		group.Add(1)
		go func(index int, ledger *agentTaskDeliveryLedger) {
			defer group.Done()
			response, err := ledger.registerWorkerIdentity(ctx, authorities[index].PrincipalID, authorities[index].WorkspaceID, "mixed-worker", authorities[index].WorkerInstanceID)
			results <- registrationResult{response: response, err: err}
		}(index, ledger)
	}
	group.Wait()
	close(results)
	var collisionAuthority agentWorkerIdentityAuthority
	canonicals := map[string]struct{}{}
	retained := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("mixed-case concurrent registration failed: %v", result.err)
		}
		identity := anyMap(result.response["identity"])
		if anyToString(identity["workspace_id"]) != "workspace-x" {
			t.Fatalf("workspace was not canonicalized before storage/readback: %#v", identity)
		}
		canonical := anyToString(identity["canonical_worker_id"])
		canonicals[canonical] = struct{}{}
		if canonical == "mixed-worker" {
			retained++
		} else {
			for _, authority := range authorities {
				if authority.WorkerInstanceID == anyToString(identity["worker_instance_id"]) {
					collisionAuthority = authority
				}
			}
		}
	}
	if len(canonicals) != 2 || retained != 1 || collisionAuthority.WorkerInstanceID == "" {
		t.Fatalf("mixed-case workspace did not retain one ID and allocate one collision ID: canonicals=%#v authority=%#v", canonicals, collisionAuthority)
	}
	update, err := first.readWorkerIdentityUpdate(ctx, collisionAuthority, "")
	if err != nil || update.UpdateID == "" {
		t.Fatalf("mixed-case collision update was not readable through normalized authority: update=%#v err=%v", update, err)
	}
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, collisionAuthority), collisionAuthority); err != nil {
		t.Fatalf("mixed-case collision acknowledgement failed: %v", err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart mixed-case ledger: %v", err)
	}
	defer restarted.close()
	replay, err := restarted.registerWorkerIdentity(ctx, collisionAuthority.PrincipalID, "WORKSPACE-X", "mixed-worker", collisionAuthority.WorkerInstanceID)
	if err != nil || !anyToBool(replay["idempotent_replay"]) || anyToBool(replay["identity_update_required"]) {
		t.Fatalf("mixed-case restart/replay was not durable: response=%#v err=%v", replay, err)
	}

	migrationLedger := restarted
	seed := agentWorkerIdentityRecord{
		IdentityID: "case-variant-identity", PrincipalID: "migration-principal", WorkspaceID: "Workspace-Migration",
		RequestedWorkerID: "migration-worker", CanonicalWorkerID: "migration-worker", WorkerInstanceID: "case-variant-instance",
		Status: "active", CreatedAt: agentTaskNow(), UpdatedAt: agentTaskNow(),
	}
	seed.RequestedIDDigest = workerIdentityRequestedDigest(seed.RequestedWorkerID)
	seed.IdentityDigest = workerIdentityRecordDigest(seed)
	if _, err := migrationLedger.db.Exec(`INSERT INTO task_ledger_worker_identities(identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,worker_identity_update_generation,acknowledged_generation,requested_id_digest,identity_digest,status,created_at,updated_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, seed.IdentityID, seed.PrincipalID, seed.WorkspaceID, seed.RequestedWorkerID, seed.CanonicalWorkerID, seed.WorkerInstanceID, 0, 0, seed.RequestedIDDigest, seed.IdentityDigest, seed.Status, seed.CreatedAt, seed.UpdatedAt, ""); err != nil {
		t.Fatalf("seed case-variant migration row: %v", err)
	}
	var before, after int
	if err := migrationLedger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE lower(workspace_id)=lower(?)`, "workspace-migration").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := migrationLedger.registerWorkerIdentity(ctx, "new-principal", "workspace-migration", "migration-worker", "new-instance"); err == nil {
		t.Fatal("case-variant migration conflict was silently merged")
	}
	if err := migrationLedger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE lower(workspace_id)=lower(?)`, "workspace-migration").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("case-variant migration rejection mutated identity rows: before=%d after=%d", before, after)
	}
}

func TestWorkerIdentityReadAckReplayTamperAndTwoHandleCAS(t *testing.T) {
	first, second := identityTestLedgers(t)
	ctx := context.Background()
	authority := identityTestAuthority("principal-b", "workspace-ack", "instance-b")
	_, identity := registerIdentityForTest(t, first, identityTestAuthority("principal-a", "workspace-ack", "instance-a"), "ack-worker")
	if identity.IdentityUpdateGeneration != 0 {
		t.Fatalf("first registration unexpectedly collided: %#v", identity)
	}
	_, identity = registerIdentityForTest(t, second, authority, "ack-worker")
	if identity.IdentityUpdateGeneration != 1 {
		t.Fatalf("second registration did not receive generation one: %#v", identity)
	}
	update, err := second.readWorkerIdentityUpdate(ctx, authority, "")
	if err != nil {
		t.Fatalf("read pending update: %v", err)
	}
	if update.State != agentWorkerIdentityStateDelivered || update.DeliveryAttempts != 1 {
		t.Fatalf("readback did not durably deliver once: %#v", update)
	}
	retry, err := first.readWorkerIdentityUpdate(ctx, authority, update.UpdateID)
	if err != nil {
		t.Fatalf("readback retry: %v", err)
	}
	if retry.DeliveryAttempts != update.DeliveryAttempts || retry.ReceiptDigest != update.ReceiptDigest {
		t.Fatalf("readback retry changed immutable delivery evidence: first=%#v retry=%#v", update, retry)
	}
	if _, err := first.readWorkerIdentityUpdate(ctx, identityTestAuthority("other-principal", authority.WorkspaceID, authority.WorkerInstanceID), update.UpdateID); err == nil {
		t.Fatal("cross-principal update read was accepted")
	}
	if _, err := first.readWorkerIdentityUpdate(ctx, identityTestAuthority(authority.PrincipalID, "other-workspace", authority.WorkerInstanceID), update.UpdateID); err == nil {
		t.Fatal("cross-workspace update read was accepted")
	}
	if _, err := first.readWorkerIdentityUpdate(ctx, identityTestAuthority(authority.PrincipalID, authority.WorkspaceID, "other-instance"), update.UpdateID); err == nil {
		t.Fatal("cross-instance update read was accepted")
	}
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, identityTestAuthority("other-principal", authority.WorkspaceID, authority.WorkerInstanceID)), identityTestAuthority("other-principal", authority.WorkspaceID, authority.WorkerInstanceID)); err == nil {
		t.Fatal("cross-principal acknowledgement was accepted")
	}
	wrongReceipt := identityTestAckPayload(update, authority)
	wrongReceipt["ack_receipt_digest"] = update.ReceiptDigest
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, wrongReceipt, authority); err == nil {
		t.Fatal("server update receipt was accepted as acknowledgement receipt")
	}
	wrongIdentity := identityTestAckPayload(update, authority)
	wrongIdentity["identity_id"] = "foreign-identity"
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, wrongIdentity, authority); err == nil {
		t.Fatal("acknowledgement with a foreign identity binding was accepted")
	}
	wrongDigest := identityTestAckPayload(update, authority)
	wrongDigest["update_digest"] = "sha256:" + strings.Repeat("0", 64)
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, wrongDigest, authority); err == nil {
		t.Fatal("acknowledgement with a foreign update digest was accepted")
	}
	nestedTamper := identityTestAckPayload(update, authority)
	nestedTamper["identity_update"] = cloneAnyMap(anyMap(nestedTamper["identity_update"]))
	anyMap(nestedTamper["identity_update"])["canonical_worker_id"] = "tampered"
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, nestedTamper, authority); err == nil {
		t.Fatal("acknowledgement with a tampered nested update was accepted")
	}
	malformedCases := []struct {
		name   string
		path   string
		value  any
		nested bool
	}{
		{name: "nested state type", path: "state", value: 1, nested: true},
		{name: "nested delivery attempts type", path: "delivery_attempts", value: "one", nested: true},
		{name: "nested timestamp type", path: "created_at", value: 1, nested: true},
		{name: "nested acknowledgement flag type", path: "ack_required", value: "true", nested: true},
		{name: "nested instance type", path: "worker_instance_id", value: 1, nested: true},
		{name: "nested principal type", path: "principal_id", value: 1, nested: true},
		{name: "top instance type", path: "worker_instance_id", value: 1},
		{name: "top principal type", path: "principal_id", value: 1},
		{name: "top workspace type", path: "workspace_id", value: 1},
	}
	for _, malformed := range malformedCases {
		candidate := identityTestAckPayload(update, authority)
		if malformed.nested {
			candidate["identity_update"] = cloneAnyMap(anyMap(candidate["identity_update"]))
			anyMap(candidate["identity_update"])[malformed.path] = malformed.value
		} else {
			candidate[malformed.path] = malformed.value
		}
		if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, candidate, authority); err == nil {
			t.Fatalf("malformed acknowledgement %s was accepted", malformed.name)
		}
		state, stateErr := first.workerIdentityUpdateState(ctx, update.UpdateID)
		if stateErr != nil || state != agentWorkerIdentityStateDelivered {
			t.Fatalf("malformed acknowledgement %s changed durable update state: state=%q err=%v", malformed.name, state, stateErr)
		}
		current, identityErr := first.workerIdentityByAuthority(ctx, authority)
		if identityErr != nil || current.AcknowledgedGeneration != 0 {
			t.Fatalf("malformed acknowledgement %s changed durable identity generation: identity=%#v err=%v", malformed.name, current, identityErr)
		}
	}
	validTamperCases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "state", mutate: func(nested map[string]any) {
			nested["state"] = agentWorkerIdentityStatePending
			nested["ack_required"] = true
		}},
		{name: "delivery attempts", mutate: func(nested map[string]any) {
			nested["delivery_attempts"] = anyToInt(nested["delivery_attempts"], 0) + 1
		}},
		{name: "created timestamp", mutate: func(nested map[string]any) { nested["created_at"] = "2099-01-01T00:00:00Z" }},
		{name: "delivered timestamp", mutate: func(nested map[string]any) { nested["delivered_at"] = "2099-01-01T00:00:00Z" }},
		{name: "format contract", mutate: func(nested map[string]any) {
			nested["format_contract"] = cloneAnyMap(anyMap(nested["format_contract"]))
			anyMap(nested["format_contract"])["schema_id"] = "tampered.contract.v1"
		}},
	}
	for _, tamper := range validTamperCases {
		candidate := identityTestAckPayload(update, authority)
		candidate["identity_update"] = cloneAnyMap(anyMap(candidate["identity_update"]))
		tamper.mutate(anyMap(candidate["identity_update"]))
		if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, candidate, authority); err == nil {
			t.Fatalf("valid-type nested receipt tamper %s was accepted", tamper.name)
		}
		state, stateErr := first.workerIdentityUpdateState(ctx, update.UpdateID)
		if stateErr != nil || state != agentWorkerIdentityStateDelivered {
			t.Fatalf("valid-type nested receipt tamper %s changed durable state: state=%q err=%v", tamper.name, state, stateErr)
		}
	}
	unknownField := identityTestAckPayload(update, authority)
	unknownField["unexpected"] = true
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, unknownField, authority); err == nil {
		t.Fatal("acknowledgement with an unknown field was accepted")
	}
	falseAcknowledgement := identityTestAckPayload(update, authority)
	falseAcknowledgement["acknowledged"] = false
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, falseAcknowledgement, authority); err == nil {
		t.Fatal("acknowledgement with acknowledged=false was accepted")
	}
	stale := identityTestAckPayload(update, authority)
	stale["worker_identity_update_generation"] = update.IdentityUpdateGeneration + 1
	if _, err := first.acknowledgeWorkerIdentityUpdate(ctx, stale, authority); err == nil {
		t.Fatal("stale generation acknowledgement was accepted")
	}

	ackResults := make(chan error, 2)
	var group sync.WaitGroup
	for _, ledger := range []*agentTaskDeliveryLedger{first, second} {
		group.Add(1)
		go func(ledger *agentTaskDeliveryLedger) {
			defer group.Done()
			_, ackErr := ledger.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, authority), authority)
			ackResults <- ackErr
		}(ledger)
	}
	group.Wait()
	close(ackResults)
	for ackErr := range ackResults {
		if ackErr != nil {
			t.Fatalf("concurrent exact acknowledgement failed: %v", ackErr)
		}
	}
	state, err := first.workerIdentityUpdateState(ctx, update.UpdateID)
	if err != nil || state != agentWorkerIdentityStateAcknowledged {
		t.Fatalf("acknowledgement state was not durable: state=%q err=%v", state, err)
	}
	replay, err := first.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(update, authority), authority)
	if err != nil || !anyToBool(replay["idempotent_replay"]) {
		t.Fatalf("exact acknowledgement replay was not idempotent: response=%#v err=%v", replay, err)
	}
	identity, err = first.workerIdentityByAuthority(ctx, authority)
	if err != nil || identity.AcknowledgedGeneration != identity.IdentityUpdateGeneration {
		t.Fatalf("identity generation CAS did not converge: identity=%#v err=%v", identity, err)
	}
}

func TestWorkerIdentityRestartPreservesCanonicalAndAcknowledgement(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "agent_tasks.sqlite3")
	t.Setenv("GO_AGENT_TASK_LEDGER_PATH", dbPath)
	t.Setenv("GO_AGENT_TASK_ARTIFACT_DIR", filepath.Join(root, "artifacts"))
	t.Setenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH", "")
	first, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	authorityA := identityTestAuthority("restart-a", "restart-workspace", "restart-instance-a")
	registerIdentityForTest(t, first, authorityA, "restart-worker")
	authorityB := identityTestAuthority("restart-b", "restart-workspace", "restart-instance-b")
	_, identity := registerIdentityForTest(t, first, authorityB, "restart-worker")
	update, err := first.readWorkerIdentityUpdate(context.Background(), authorityB, "")
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeIdentityForTest(t, first, authorityB, update.UpdateID)
	canonical := identity.CanonicalWorkerID
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("restart ledger: %v", err)
	}
	defer restarted.close()
	replay, err := restarted.registerWorkerIdentity(context.Background(), authorityB.PrincipalID, authorityB.WorkspaceID, "restart-worker", authorityB.WorkerInstanceID)
	if err != nil {
		t.Fatalf("restart registration replay: %v", err)
	}
	if !anyToBool(replay["idempotent_replay"]) || anyToString(anyMap(replay["identity"])["canonical_worker_id"]) != canonical || anyToBool(replay["identity_update_required"]) {
		t.Fatalf("restart replay lost durable identity acknowledgement: %#v", replay)
	}
	state, err := restarted.workerIdentityUpdateState(context.Background(), update.UpdateID)
	if err != nil || state != agentWorkerIdentityStateAcknowledged {
		t.Fatalf("restart replay changed update state: state=%q err=%v", state, err)
	}
}

func TestWorkerIdentityPendingClaimGateAndLeaseGenerationImmutability(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	firstAuthority := identityTestAuthority("owner-a", "workspace-claim", "instance-a")
	registerIdentityForTest(t, ledger, firstAuthority, "claim-worker")
	secondAuthority := identityTestAuthority("owner-b", "workspace-claim", "instance-b")
	_, secondIdentity := registerIdentityForTest(t, ledger, secondAuthority, "claim-worker")
	if secondIdentity.IdentityUpdateGeneration != 1 {
		t.Fatalf("expected pending collision identity: %#v", secondIdentity)
	}
	if _, err := ledger.resolveWorkerIdentityForClaim(ctx, secondAuthority.PrincipalID, secondAuthority.WorkspaceID, secondIdentity.RequestedWorkerID, secondAuthority.WorkerInstanceID); !errors.Is(err, errWorkerIdentityUpdatePending) {
		t.Fatalf("pending identity did not block claim: %v", err)
	}
	update, err := ledger.readWorkerIdentityUpdate(ctx, secondAuthority, "")
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeIdentityForTest(t, ledger, secondAuthority, update.UpdateID)
	secondIdentity, err = ledger.resolveWorkerIdentityForClaim(ctx, secondAuthority.PrincipalID, secondAuthority.WorkspaceID, secondIdentity.RequestedWorkerID, secondAuthority.WorkerInstanceID)
	if err != nil {
		t.Fatalf("claim remained blocked after exact acknowledgement: %v", err)
	}
	manifest := testAgentTaskManifest("identity-claim-task", "claim-project", "owner-b", "identity-claim-session")
	manifest["workspace_id"] = secondAuthority.WorkspaceID
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit generic task: %v", err)
	}
	claim, err := ledger.claimTaskWithIdentity(ctx, secondIdentity.CanonicalWorkerID, secondIdentity.WorkerInstanceID, secondIdentity.WorkspaceID, "", secondIdentity.IdentityUpdateGeneration)
	if err != nil || claim == nil {
		t.Fatalf("claim after acknowledgement failed: claim=%#v err=%v", claim, err)
	}
	attempt := anyMap(claim["attempt"])
	lease := anyMap(claim["lease"])
	if anyToString(lease["worker_id"]) != secondIdentity.CanonicalWorkerID || anyToInt(attempt["worker_identity_update_generation"], -1) != 1 {
		t.Fatalf("claim did not use canonical identity and generation: lease=%#v attempt=%#v", lease, attempt)
	}
	if findings := validateAgentContractPayload(agentTaskLeaseContractID, lease); len(findings) != 0 {
		t.Fatalf("claim lease did not satisfy the closed lease contract: %#v", findings)
	}
	for _, field := range []string{"worker_id", "worker_instance_id", "worker_identity_update_generation", "generation", "assignment_generation", "lease_generation"} {
		candidate := cloneAnyMap(lease)
		delete(candidate, field)
		if findings := validateAgentContractPayload(agentTaskLeaseContractID, candidate); len(findings) == 0 {
			t.Fatalf("lease contract accepted a claim missing %s", field)
		}
	}
	taskID := anyToString(anyMap(claim["task"])["task_id"])
	events, err := ledger.taskEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("read immutable claim events: %v", err)
	}
	foundLeaseEvent := false
	for _, event := range events {
		if anyToString(event["status"]) != "leased" {
			continue
		}
		foundLeaseEvent = true
		if anyToInt(anyMap(event["metadata"])["worker_identity_update_generation"], -1) != secondIdentity.IdentityUpdateGeneration {
			t.Fatalf("immutable claim event omitted identity-update generation: %#v", event)
		}
	}
	if !foundLeaseEvent {
		t.Fatal("immutable claim event was not persisted")
	}
	if _, err := ledger.registerWorkerIdentity(ctx, secondAuthority.PrincipalID, secondAuthority.WorkspaceID, "claim-worker", secondAuthority.WorkerInstanceID); err != nil {
		t.Fatalf("identity replay changed active lease: %v", err)
	}
	if anyToInt(anyMap(claim["attempt"])["worker_identity_update_generation"], -1) != 1 {
		t.Fatal("identity replay rewrote the returned active lease")
	}

	legacyAuthority := identityTestAuthority("owner-c", "workspace-claim", "instance-c")
	legacyManifest := testAgentTaskManifest("identity-legacy-task", "legacy-project", "owner-c", "identity-legacy-session")
	legacyManifest["workspace_id"] = legacyAuthority.WorkspaceID
	if _, err := ledger.submit(ctx, legacyManifest); err != nil {
		t.Fatalf("submit generation zero task: %v", err)
	}
	legacyClaim, err := ledger.claimTask(ctx, "legacy-worker", legacyAuthority.WorkerInstanceID, legacyAuthority.WorkspaceID, "")
	if err != nil || legacyClaim == nil {
		t.Fatalf("generation zero claim failed: claim=%#v err=%v", legacyClaim, err)
	}
	_, legacyIdentity := registerIdentityForTest(t, ledger, legacyAuthority, "legacy-worker")
	if legacyIdentity.IdentityUpdateGeneration != 0 || legacyIdentity.CanonicalWorkerID != "legacy-worker" {
		t.Fatalf("same-ID registration did not retain the existing generation-zero identity: %#v", legacyIdentity)
	}
	legacyFence := testAgentTaskFenceFromClaim(t, legacyClaim)
	if anyToInt(anyMap(legacyClaim["attempt"])["worker_identity_update_generation"], -1) != 0 {
		t.Fatal("generation zero lease was rewritten")
	}
	if _, err := ledger.heartbeat(ctx, legacyFence); err != nil {
		t.Fatalf("generation zero active lease was invalidated by registration: %v", err)
	}
}

func TestWorkerIdentityCanonicalDBCollisionRetryAndAssignmentShapes(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	workspace := "workspace-collision-retry"
	requested := "retry-worker"
	occupied := boundedCanonicalWorkerID(requested, "instance-retry", 0)
	now := agentTaskNow()
	occupiedIdentity := agentWorkerIdentityRecord{
		IdentityID: "occupied-identity", PrincipalID: "occupier", WorkspaceID: workspace,
		RequestedWorkerID: "other-worker", CanonicalWorkerID: occupied, WorkerInstanceID: "occupier-instance",
		IdentityUpdateGeneration: 0, AcknowledgedGeneration: 0, RequestedIDDigest: workerIdentityRequestedDigest("other-worker"),
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	occupiedIdentity.IdentityDigest = workerIdentityRecordDigest(occupiedIdentity)
	if _, err := ledger.db.Exec(`INSERT INTO task_ledger_worker_identities(identity_id,principal_id,workspace_id,requested_worker_id,canonical_worker_id,worker_instance_id,worker_identity_update_generation,acknowledged_generation,requested_id_digest,identity_digest,status,created_at,updated_at,closed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, occupiedIdentity.IdentityID, occupiedIdentity.PrincipalID, occupiedIdentity.WorkspaceID, occupiedIdentity.RequestedWorkerID, occupiedIdentity.CanonicalWorkerID, occupiedIdentity.WorkerInstanceID, occupiedIdentity.IdentityUpdateGeneration, occupiedIdentity.AcknowledgedGeneration, occupiedIdentity.RequestedIDDigest, occupiedIdentity.IdentityDigest, occupiedIdentity.Status, occupiedIdentity.CreatedAt, occupiedIdentity.UpdatedAt, occupiedIdentity.ClosedAt); err != nil {
		t.Fatalf("seed canonical collision: %v", err)
	}
	registerIdentityForTest(t, ledger, identityTestAuthority("first-principal", workspace, "first-instance"), requested)
	authority := identityTestAuthority("retry-principal", workspace, "instance-retry")
	_, identity := registerIdentityForTest(t, ledger, authority, requested)
	if identity.CanonicalWorkerID != boundedCanonicalWorkerID(requested, authority.WorkerInstanceID, 1) {
		t.Fatalf("database canonical collision did not retry deterministically: got=%q", identity.CanonicalWorkerID)
	}
	if identity.CanonicalWorkerID == occupied {
		t.Fatal("database canonical collision reused an occupied ID")
	}

	firstAuthority := identityTestAuthority("target-principal", workspace, "target-instance")
	_, firstIdentity := registerIdentityForTest(t, ledger, firstAuthority, "target-worker")
	targeted := testAgentTaskManifest("targeted-identity-task", "targeted-project", "target-principal", "targeted-session")
	targeted["workspace_id"] = workspace
	targeted["metadata"] = map[string]any{"worker": firstIdentity.CanonicalWorkerID}
	if _, err := ledger.submit(ctx, targeted); err != nil {
		t.Fatalf("submit targeted task: %v", err)
	}
	if claim, err := ledger.claimTaskWithIdentity(ctx, firstIdentity.CanonicalWorkerID, firstAuthority.WorkerInstanceID, workspace, "", 0); err != nil || claim == nil {
		t.Fatalf("targeted task was not claimable by its canonical worker: claim=%#v err=%v", claim, err)
	}
	generic := testAgentTaskManifest("generic-identity-task", "generic-project", "target-principal", "generic-session")
	generic["workspace_id"] = workspace
	if _, err := ledger.submit(ctx, generic); err != nil {
		t.Fatalf("submit generic task: %v", err)
	}
	otherAuthority := identityTestAuthority("generic-principal", workspace, "generic-instance")
	_, otherIdentity := registerIdentityForTest(t, ledger, otherAuthority, "other-worker")
	if claim, err := ledger.claimTaskWithIdentity(ctx, otherIdentity.CanonicalWorkerID, otherAuthority.WorkerInstanceID, workspace, "", 0); err != nil || claim == nil {
		t.Fatalf("generic task was excluded by request shape: claim=%#v err=%v", claim, err)
	}
	if strings.TrimSpace(otherIdentity.CanonicalWorkerID) == "" {
		t.Fatal("generic identity canonical was empty")
	}
}

func TestWorkerIdentityCanonicalUTF8BoundAndPrincipalRebindFailsClosed(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	// Public lease IDs use the shared ASCII grammar. Exercise the byte bound
	// with the longest valid requested ID; collision canonicalization must
	// truncate it before appending the server-issued suffix without creating an
	// invalid UTF-8 or out-of-bound durable ID.
	requested := strings.Repeat("a", agentWorkerIdentityCanonicalMaxBytes)
	firstAuthority := identityTestAuthority("Principal-Exact", "utf8-workspace", "utf8-first")
	registerIdentityForTest(t, ledger, firstAuthority, requested)
	secondAuthority := identityTestAuthority("Principal-Second", "utf8-workspace", "utf8-second")
	_, second := registerIdentityForTest(t, ledger, secondAuthority, requested)
	if second.CanonicalWorkerID == requested || !utf8.ValidString(second.CanonicalWorkerID) || strings.ContainsRune(second.CanonicalWorkerID, '\ufffd') || len([]byte(second.CanonicalWorkerID)) > agentWorkerIdentityCanonicalMaxBytes {
		t.Fatalf("collision canonical worker ID was not bounded and rune-safe: %q bytes=%d", second.CanonicalWorkerID, len([]byte(second.CanonicalWorkerID)))
	}
	if _, err := ledger.registerWorkerIdentity(ctx, "principal-exact", firstAuthority.WorkspaceID, requested, firstAuthority.WorkerInstanceID); err == nil {
		t.Fatal("principal case variant silently rebound the existing worker instance")
	}
	identityBefore, err := ledger.workerIdentityByAuthority(ctx, firstAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if identityBefore.PrincipalID != firstAuthority.PrincipalID || identityBefore.WorkerInstanceID != firstAuthority.WorkerInstanceID {
		t.Fatalf("principal rebind rejection did not preserve identity authority: %#v", identityBefore)
	}
	var before int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.registerWorkerIdentity(ctx, firstAuthority.PrincipalID, "other-utf8-workspace", requested, firstAuthority.WorkerInstanceID); err == nil {
		t.Fatal("worker instance silently rebound to a different workspace")
	}
	var after, foreign int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities WHERE workspace_id=?`, "other-utf8-workspace").Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if after != before || foreign != 0 {
		t.Fatalf("workspace rebind mutated identity authority: before=%d after=%d foreign=%d", before, after, foreign)
	}
}

func TestWorkerIdentityLegacyAcknowledgementReceiptBackfillAndTamperFailClosed(t *testing.T) {
	first, second := identityTestLedgers(t)
	ctx := context.Background()
	authority := identityTestAuthority("legacy-ack-principal", "legacy-ack-workspace", "legacy-ack-instance")
	registerIdentityForTest(t, first, identityTestAuthority("legacy-ack-occupier", authority.WorkspaceID, "legacy-ack-occupier-instance"), "legacy-ack-worker")
	registerIdentityForTest(t, first, authority, "legacy-ack-worker")
	update, err := first.readWorkerIdentityUpdate(ctx, authority, "")
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeIdentityForTest(t, first, authority, update.UpdateID)
	if _, err := first.db.Exec(`UPDATE task_ledger_worker_identity_updates SET ack_receipt_payload_version=0 WHERE update_id=?`, update.UpdateID); err != nil {
		t.Fatal(err)
	}
	if err := first.close(); err != nil {
		t.Fatal(err)
	}
	if err := second.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("valid legacy acknowledgement did not backfill: %v", err)
	}
	stored, err := restarted.workerIdentityUpdateByID(ctx, update.UpdateID)
	if err != nil || stored.AckReceiptPayloadVersion != workerIdentityAckReceiptPayloadVersionExact || stored.AckReceiptPayloadJSON == "" {
		t.Fatalf("legacy acknowledgement backfill was not versioned and durable: update=%#v err=%v", stored, err)
	}
	exactReplay := stored
	exactReplay.State = agentWorkerIdentityStateDelivered
	exactReplay.AcknowledgedAt = ""
	exactReplay.UpdatedAt = exactReplay.DeliveredAt
	if _, err := restarted.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(exactReplay, authority), authority); err != nil {
		t.Fatalf("backfilled acknowledgement did not accept exact replay: %v", err)
	}
	if err := restarted.close(); err != nil {
		t.Fatal(err)
	}

	// A pre-v6 row without any snapshot receives an explicit legacy
	// reconciliation version. Its deterministic delivered receipt remains the
	// only replay accepted after restart.
	legacy, legacySecond := identityTestLedgers(t)
	legacyAuthority := identityTestAuthority("legacy-empty-principal", "legacy-empty-workspace", "legacy-empty-instance")
	registerIdentityForTest(t, legacy, identityTestAuthority("legacy-empty-occupier", legacyAuthority.WorkspaceID, "legacy-empty-occupier-instance"), "legacy-empty-worker")
	registerIdentityForTest(t, legacy, legacyAuthority, "legacy-empty-worker")
	legacyUpdate := acknowledgeIdentityForTest(t, legacy, legacyAuthority, "")
	if _, err := legacy.db.Exec(`UPDATE task_ledger_worker_identity_updates SET ack_receipt_payload_json='',ack_receipt_payload_version=0 WHERE update_id=?`, legacyUpdate.UpdateID); err != nil {
		t.Fatal(err)
	}
	if err := legacy.close(); err != nil {
		t.Fatal(err)
	}
	if err := legacySecond.close(); err != nil {
		t.Fatal(err)
	}
	legacyRestarted, err := newAgentTaskDeliveryLedgerFromEnv()
	if err != nil {
		t.Fatalf("empty legacy acknowledgement did not receive a closed reconciliation receipt: %v", err)
	}
	legacyStored, err := legacyRestarted.workerIdentityUpdateByID(ctx, legacyUpdate.UpdateID)
	if err != nil || legacyStored.AckReceiptPayloadVersion != workerIdentityAckReceiptPayloadVersionLegacyReconciled {
		t.Fatalf("empty legacy acknowledgement was not explicitly versioned: update=%#v err=%v", legacyStored, err)
	}
	legacyReplay := legacyStored
	legacyReplay.State = agentWorkerIdentityStateDelivered
	legacyReplay.AcknowledgedAt = ""
	legacyReplay.UpdatedAt = legacyReplay.DeliveredAt
	if _, err := legacyRestarted.acknowledgeWorkerIdentityUpdate(ctx, identityTestAckPayload(legacyReplay, legacyAuthority), legacyAuthority); err != nil {
		t.Fatalf("legacy reconciliation receipt did not accept deterministic replay: %v", err)
	}
	if err := legacyRestarted.close(); err != nil {
		t.Fatal(err)
	}

	// A second database with tampered legacy evidence must fail initialization;
	// migration must never invent a receipt for an ambiguous row.
	tampered, _ := identityTestLedgers(t)
	tamperedAuthority := identityTestAuthority("legacy-tampered-principal", "legacy-tampered-workspace", "legacy-tampered-instance")
	registerIdentityForTest(t, tampered, identityTestAuthority("legacy-tampered-occupier", tamperedAuthority.WorkspaceID, "legacy-tampered-occupier-instance"), "legacy-tampered-worker")
	registerIdentityForTest(t, tampered, tamperedAuthority, "legacy-tampered-worker")
	tamperedUpdate, err := tampered.readWorkerIdentityUpdate(ctx, tamperedAuthority, "")
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeIdentityForTest(t, tampered, tamperedAuthority, tamperedUpdate.UpdateID)
	if _, err := tampered.db.Exec(`UPDATE task_ledger_worker_identity_updates SET ack_receipt_digest=?,ack_receipt_payload_json='',ack_receipt_payload_version=0 WHERE update_id=?`, "sha256:"+strings.Repeat("0", 64), tamperedUpdate.UpdateID); err != nil {
		t.Fatal(err)
	}
	if err := tampered.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgentTaskDeliveryLedgerFromEnv(); err == nil {
		t.Fatal("tampered legacy acknowledgement was silently backfilled")
	}
}

func TestWorkerIdentityNativeRoutesBindAuthorityAndClosedAcknowledgement(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	server := &server{taskLedger: ledger}
	firstAuth := agentTaskRouteAuth{Principal: "route-principal-a", Workspace: "route-workspace", Signed: true}
	secondAuth := agentTaskRouteAuth{Principal: "route-principal-b", Workspace: "route-workspace", Signed: true}

	call := func(auth agentTaskRouteAuth, method, path string, payload map[string]any) (int, map[string]any) {
		t.Helper()
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(method, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		requestPayload, parseErr := parseJSONMap(body)
		if parseErr != nil {
			t.Fatalf("parse identity route request: %v", parseErr)
		}
		server.handleAgentWorkerIdentityRoute(recorder, request, auth, requestPayload)
		var response map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode identity route response: status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
		}
		return recorder.Code, response
	}

	var identitiesBefore int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities`).Scan(&identitiesBefore); err != nil {
		t.Fatal(err)
	}
	for _, malformed := range []struct {
		field string
		value any
	}{
		{field: "requested_worker_id", value: 1},
		{field: "worker_instance_id", value: false},
		{field: "workspace_id", value: 1.0},
		{field: "worker_identity_update_generation", value: true},
		{field: "worker_identity_update_generation", value: "0"},
	} {
		malformedPayload := map[string]any{"requested_worker_id": "strict-route-worker", "worker_instance_id": "strict-route-instance"}
		malformedPayload[malformed.field] = malformed.value
		status, _ := call(firstAuth, http.MethodPost, "/agents/workers/register", malformedPayload)
		if status == http.StatusOK {
			t.Fatalf("malformed registration field %s was accepted", malformed.field)
		}
	}
	var identitiesAfter int
	if err := ledger.db.QueryRow(`SELECT COUNT(*) FROM task_ledger_worker_identities`).Scan(&identitiesAfter); err != nil {
		t.Fatal(err)
	}
	if identitiesBefore != identitiesAfter {
		t.Fatalf("malformed registration matrix changed durable identity count: before=%d after=%d", identitiesBefore, identitiesAfter)
	}

	status, registration := call(firstAuth, http.MethodPost, "/agents/workers/register", map[string]any{
		"requested_worker_id": "route-worker", "worker_instance_id": "route-instance-a",
	})
	if status != http.StatusOK || anyToString(anyMap(registration["identity"])["canonical_worker_id"]) != "route-worker" {
		t.Fatalf("first route registration failed: status=%d response=%#v", status, registration)
	}
	status, collision := call(secondAuth, http.MethodPost, "/agents/workers/register", map[string]any{
		"requested_worker_id": "route-worker", "worker_instance_id": "route-instance-b",
	})
	if status != http.StatusOK || !anyToBool(collision["identity_update_required"]) {
		t.Fatalf("collision route did not push update: status=%d response=%#v", status, collision)
	}
	update := anyMap(collision["identity_update"])
	if anyToString(update["update_id"]) == "" || anyToString(update["receipt_digest"]) == anyToString(update["ack_receipt_digest"]) {
		t.Fatalf("route exposed a missing or non-distinct acknowledgement digest: %#v", update)
	}
	status, readback := call(secondAuth, http.MethodGet, "/agents/workers/identity/"+anyToString(update["update_id"]), map[string]any{
		"worker_instance_id": "route-instance-b",
	})
	if status != http.StatusOK || anyToString(anyMap(readback["identity_update"])["update_id"]) != anyToString(update["update_id"]) {
		t.Fatalf("exact update readback failed: status=%d response=%#v", status, readback)
	}
	status, _ = call(secondAuth, http.MethodGet, "/agents/workers/identity/"+anyToString(update["update_id"]), map[string]any{
		"update_id": "different-update", "worker_instance_id": "route-instance-b",
	})
	if status == http.StatusOK {
		t.Fatal("conflicting update readback aliases were accepted")
	}
	ack := map[string]any{
		"schema_id": agentWorkerIdentityAckContractID, "contract_version": 1,
		"update_id": update["update_id"], "identity_id": update["identity_id"],
		"principal_id": secondAuth.Principal, "workspace_id": secondAuth.Workspace, "worker_instance_id": "route-instance-b",
		"old_worker_id": update["old_worker_id"], "requested_worker_id": update["requested_worker_id"],
		"canonical_worker_id": update["canonical_worker_id"], "new_worker_id": update["new_worker_id"],
		"worker_identity_update_generation": update["worker_identity_update_generation"],
		"update_digest":                     update["update_digest"], "receipt_digest": update["receipt_digest"],
		"ack_receipt_digest": update["ack_receipt_digest"], "acknowledged": true, "idempotent_replay": false,
		"identity_update": cloneAnyMap(update),
	}
	ack = agentTaskContractPayload(agentWorkerIdentityAckContractID, ack)
	for _, field := range []string{"principal_id", "workspace_id", "worker_instance_id"} {
		malformed := cloneAnyMap(ack)
		malformed[field] = 1
		status, _ = call(secondAuth, http.MethodPost, "/agents/workers/identity/ack", malformed)
		if status == http.StatusOK {
			t.Fatalf("route accepted malformed acknowledgement authority field %s", field)
		}
	}
	state, stateErr := ledger.workerIdentityUpdateState(context.Background(), anyToString(update["update_id"]))
	if stateErr != nil || state != agentWorkerIdentityStateDelivered {
		t.Fatalf("malformed route acknowledgement changed durable state: state=%q err=%v", state, stateErr)
	}
	status, acknowledged := call(secondAuth, http.MethodPost, "/agents/workers/identity/ack", ack)
	if status != http.StatusOK || anyToBool(acknowledged["acknowledged"]) != true || anyToString(acknowledged["ack_receipt_digest"]) != anyToString(ack["ack_receipt_digest"]) {
		t.Fatalf("exact route acknowledgement failed: status=%d response=%#v", status, acknowledged)
	}
	status, _ = call(firstAuth, http.MethodGet, "/agents/workers/identity/"+anyToString(update["update_id"]), map[string]any{
		"worker_instance_id": "route-instance-b",
	})
	if status == http.StatusOK {
		t.Fatal("cross-principal route read was accepted")
	}
	status, _ = call(firstAuth, http.MethodGet, "/agents/workers/identity", map[string]any{
		"worker_instance_id": "unregistered-instance",
	})
	if status == http.StatusOK {
		t.Fatal("unregistered identity read was accepted")
	}
	secondIdentity, err := ledger.workerIdentityByAuthority(context.Background(), identityTestAuthority(secondAuth.Principal, secondAuth.Workspace, "route-instance-b"))
	if err != nil {
		t.Fatal(err)
	}
	retirement := workerIdentityRetirementPayloadForTest(secondIdentity)
	status, retirementReceipt := call(secondAuth, http.MethodPost, "/agents/workers/identity/retire", retirement)
	if status != http.StatusOK || !anyToBool(retirementReceipt["retired"]) {
		t.Fatalf("route retirement did not commit exact closed receipt: status=%d receipt=%#v", status, retirementReceipt)
	}
	readback = cloneAnyMap(retirement)
	readback["retirement_receipt_digest"] = retirementReceipt["retirement_receipt_digest"]
	status, readReceipt := call(secondAuth, http.MethodGet, "/agents/workers/identity/retire", readback)
	if status != http.StatusOK || !anyToBool(readReceipt["idempotent_replay"]) || anyToString(readReceipt["retirement_receipt_digest"]) != anyToString(retirementReceipt["retirement_receipt_digest"]) {
		t.Fatalf("route retirement readback did not return exact server receipt: status=%d receipt=%#v", status, readReceipt)
	}
	status, _ = call(firstAuth, http.MethodPost, "/agents/workers/register", map[string]any{
		"requested_worker_id": "route-worker", "worker_instance_id": "route-instance-c", "canonical_worker_id": "caller-chosen",
	})
	if status == http.StatusOK {
		t.Fatal("caller-supplied canonical worker ID was accepted")
	}
}

func TestWorkerIdentitySignedFenceRequiresMappedCanonicalAndExactZeroOrOneGeneration(t *testing.T) {
	ledger, _ := identityTestLedgers(t)
	ctx := context.Background()
	legacyOccupant := identityTestAuthority("legacy-occupant", "fence-workspace", "legacy-occupant-instance")
	registerIdentityForTest(t, ledger, legacyOccupant, "legacy-collision-worker")
	legacyAuthority := identityTestAuthority("legacy-owner", "fence-workspace", "legacy-instance")
	legacyManifest := testAgentTaskManifest("identity-legacy-fence-task", "legacy-fence-project", "legacy-owner", "identity-legacy-fence-session")
	legacyManifest["workspace_id"] = legacyAuthority.WorkspaceID
	if _, err := ledger.submit(ctx, legacyManifest); err != nil {
		t.Fatalf("submit legacy fence task: %v", err)
	}
	legacyClaim, err := ledger.claimTask(ctx, "legacy-collision-worker", legacyAuthority.WorkerInstanceID, legacyAuthority.WorkspaceID, "")
	if err != nil || legacyClaim == nil {
		t.Fatalf("claim pre-registration generation-zero lease: claim=%#v err=%v", legacyClaim, err)
	}
	_, legacyIdentity := registerIdentityForTest(t, ledger, legacyAuthority, "legacy-collision-worker")
	if legacyIdentity.IdentityUpdateGeneration != 1 || legacyIdentity.CanonicalWorkerID == legacyIdentity.RequestedWorkerID {
		t.Fatalf("legacy collision did not create a generation-one canonical identity: %#v", legacyIdentity)
	}
	legacyFence := testAgentTaskFenceFromClaim(t, legacyClaim)
	legacyAuth := agentTaskRouteAuth{Principal: legacyAuthority.PrincipalID, Workspace: legacyAuthority.WorkspaceID, Signed: true}
	server := &server{taskLedger: ledger}
	if err := server.authorizeAgentTaskFence(ctx, &legacyAuth, legacyFence); err != nil {
		t.Fatalf("persisted pre-registration generation-zero lease was rejected: %v", err)
	}
	if _, err := ledger.heartbeat(ctx, legacyFence); err != nil {
		t.Fatalf("legacy generation-zero heartbeat was rejected: %v", err)
	}
	forgedCanonical := legacyFence
	forgedCanonical.WorkerID = legacyIdentity.CanonicalWorkerID
	if err := server.authorizeAgentTaskFence(ctx, &legacyAuth, forgedCanonical); err == nil {
		t.Fatal("forged generation-zero canonical fence was accepted")
	}
	forgedAttempt := legacyFence
	forgedAttempt.AttemptID = "forged-attempt"
	if err := server.authorizeAgentTaskFence(ctx, &legacyAuth, forgedAttempt); err == nil {
		t.Fatal("generation-zero fence without a persisted attempt was accepted")
	}

	firstAuthority := identityTestAuthority("fence-owner-a", "fence-workspace", "fence-instance-a")
	registerIdentityForTest(t, ledger, firstAuthority, "fence-worker")
	secondAuthority := identityTestAuthority("fence-owner-b", "fence-workspace", "fence-instance-b")
	_, identity := registerIdentityForTest(t, ledger, secondAuthority, "fence-worker")
	update, err := ledger.readWorkerIdentityUpdate(ctx, secondAuthority, "")
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeIdentityForTest(t, ledger, secondAuthority, update.UpdateID)
	manifest := testAgentTaskManifest("identity-fence-task", "fence-project", "fence-owner-b", "identity-fence-session")
	manifest["workspace_id"] = secondAuthority.WorkspaceID
	if _, err := ledger.submit(ctx, manifest); err != nil {
		t.Fatalf("submit fence task: %v", err)
	}
	claim, err := ledger.claimTaskWithIdentity(ctx, identity.CanonicalWorkerID, identity.WorkerInstanceID, identity.WorkspaceID, "", identity.IdentityUpdateGeneration)
	if err != nil || claim == nil {
		t.Fatalf("claim fence task: claim=%#v err=%v", claim, err)
	}
	fence := testAgentTaskFenceFromClaim(t, claim)
	fence.WorkerIdentityUpdateGeneration = anyToInt(anyMap(claim["lease"])["worker_identity_update_generation"], -1)
	auth := agentTaskRouteAuth{Principal: secondAuthority.PrincipalID, Workspace: secondAuthority.WorkspaceID, Signed: true}
	if err := server.authorizeAgentTaskFence(ctx, &auth, fence); err != nil {
		t.Fatalf("mapped canonical fence was rejected: %v", err)
	}
	routeRequest := httptest.NewRequest(http.MethodPost, "/agents/tasks/identity-fence-task/heartbeat", nil)
	routeRequest.Header.Set("X-Worker-Instance-ID", secondAuthority.WorkerInstanceID)
	server.taskSignedRouteAuth = func(*http.Request) (agentTaskRouteAuth, bool, error) {
		return agentTaskRouteAuth{Principal: secondAuthority.PrincipalID, Workspace: secondAuthority.WorkspaceID, Role: "worker", Signed: true, RequestBound: true}, true, nil
	}
	boundAuth, err := server.authenticateAgentTaskRoute(routeRequest, "worker")
	if err != nil || boundAuth.WorkerInstanceID != secondAuthority.WorkerInstanceID {
		t.Fatalf("signed route did not retain the explicit worker-instance authority boundary: auth=%#v err=%v", boundAuth, err)
	}
	foreignInstanceAuth := boundAuth
	foreignInstanceFence := fence
	foreignInstanceFence.WorkerInstanceID = "same-principal-foreign-instance"
	if err := server.authorizeAgentTaskFence(ctx, &foreignInstanceAuth, foreignInstanceFence); err == nil || !strings.Contains(err.Error(), "foreign-instance") {
		t.Fatalf("same-principal foreign-instance fence was not rejected: err=%v", err)
	}
	omittedGeneration := fence
	omittedGeneration.WorkerIdentityUpdateGeneration = 0
	if err := server.authorizeAgentTaskFence(ctx, &auth, omittedGeneration); err == nil {
		t.Fatal("generation-one fence omission was accepted")
	}
	wrongCanonical := fence
	wrongCanonical.WorkerID = secondAuthority.PrincipalID
	if err := server.authorizeAgentTaskFence(ctx, &auth, wrongCanonical); err == nil {
		t.Fatal("principal-as-canonical fence fallback was accepted")
	}
}

func TestWorkerIdentityFenceIngressRejectsMalformedRawTypesAndConflictingAliases(t *testing.T) {
	base := map[string]any{
		"attempt_id": "attempt-1", "lease_id": "lease-1", "worker_id": "worker-1", "worker_instance_id": "instance-1",
		"generation": 1, "worker_identity_update_generation": 0,
	}
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "worker id number", mutate: func(payload map[string]any) { payload["worker_id"] = 1 }},
		{name: "instance object", mutate: func(payload map[string]any) { payload["worker_instance_id"] = map[string]any{} }},
		{name: "generation float", mutate: func(payload map[string]any) { payload["generation"] = 1.0 }},
		{name: "generation numeric string", mutate: func(payload map[string]any) { payload["generation"] = "1" }},
		{name: "identity generation bool", mutate: func(payload map[string]any) { payload["worker_identity_update_generation"] = true }},
		{name: "worker aliases conflict", mutate: func(payload map[string]any) { payload["worker"] = "other-worker" }},
		{name: "nested fence conflict", mutate: func(payload map[string]any) { payload["fence"] = map[string]any{"attempt_id": "attempt-2"} }},
	}
	for _, testCase := range cases {
		payload := cloneAnyMap(base)
		testCase.mutate(payload)
		if _, err := agentTaskFenceFromRequest("task-1", payload); err == nil {
			t.Fatalf("malformed fence case %s was accepted", testCase.name)
		}
	}
}
