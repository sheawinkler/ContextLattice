package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDreamTransferNativeOperatorAndCaller(t *testing.T) {
	binary := os.Getenv("CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY")
	if binary == "" || (runtime.GOOS != "darwin" && runtime.GOOS != "linux") {
		t.Skip("requires a source-built native CLI and qualified local file custody")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("expected absolute native binary")
	}
	sender, observation, hash := dreamSourceFromNativeWriteAndExport(t, binary)
	for _, verdict := range []string{"confirmed", "rejected", "withdrawn"} {
		t.Run(verdict, func(t *testing.T) {
			t.Setenv("CONTEXTLATTICE_WORKER_API_KEY", "dedicated-destination-worker")
			t.Setenv("GO_TOOL_CALLS_ROLE_SPLIT_AUTO", "true")
			t.Setenv("GO_TOOL_CALLS_WORKER_ALLOW_ALL", "false")
			t.Setenv("GO_TOOL_CALLS_WORKER_ALLOWLIST", "capability_map,ops_queue_status,dream_transfer")
			t.Setenv("GO_TOOL_CALLS_WORKER_DENYLIST", "memory_write_batch,feedback_submit")
			s := &server{memoryStore: newDerivedMemoryTestStore(t), writePolicy: loadWriteIngressPolicy(), strictNoPythonRuntime: true,
				orchestratorAPIKey: "dedicated-destination-service"}
			s.toolCalls = loadToolCallPolicy(s.orchestratorAPIKey)
			if s.toolCallAllowedForRole("dream", "worker") || !s.toolCallAllowedForRole("dream_transfer", "worker") {
				t.Fatal("fixture granted ordinary Dream instead of only manual transfer")
			}
			origin, err := json.Marshal(observation)
			if err != nil {
				t.Fatal(err)
			}
			// Replace the destination with a distinct identity and an empty durable
			// store. Only a real encrypted, reconciled export can populate it.
			destinationHome := t.TempDir()
			openDestination := func() {
				s.contextPassports = newTestPassportStore(t, destinationHome)
				s.contextMesh = newTestMeshStore(t, destinationHome, s.contextPassports)
			}
			openDestination()
			export, err := sender.contextMesh.createGrant(map[string]any{
				"recipient_id": "test-destination", "recipient": s.contextMesh.identity.MeshRecipient,
				"project": observation.Project, "ttl_secs": 3600,
			})
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := sender.createMeshEnvelope(observation, []string{export.GrantID}, "")
			if err != nil {
				t.Fatal(err)
			}
			imported, code := s.reconcileMeshEnvelope(envelope, true)
			if code != http.StatusOK || !anyToBool(imported["applied"]) {
				t.Fatalf("real mesh import refused: status=%d", code)
			}
			ops, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(ops, 0700); err != nil {
				t.Fatal(err)
			}
			grantFile, proofFile := filepath.Join(ops, "grant"), filepath.Join(ops, "proof")
			keyFile, requestFile := filepath.Join(ops, "worker-key"), filepath.Join(ops, "request")
			if err := os.WriteFile(keyFile, []byte(s.toolCalls.workerKey), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv(dreamOwnerFileEnv, grantFile)
			run := func(want bool, env []string, args ...string) map[string]any {
				t.Helper()
				ctx, stop := context.WithTimeout(context.Background(), 12*time.Second)
				defer stop()
				cmd := exec.CommandContext(ctx, binary, args...)
				cmd.Dir = ops
				cmd.Env = append([]string{"HOME=" + ops, "PATH=/usr/bin:/bin", "TMPDIR=" + ops}, env...)
				out, err := cmd.CombinedOutput()
				if len(out) > 65536 || bytes.Contains(out, []byte(s.toolCalls.workerKey)) {
					t.Fatal("oversized output or worker credential disclosure")
				}
				if proof, e := os.ReadFile(proofFile); e == nil && len(bytes.TrimSpace(proof)) > 0 && bytes.Contains(out, bytes.TrimSpace(proof)) {
					t.Fatal("transfer credential disclosure")
				}
				if want != (err == nil) {
					t.Fatalf("native %s expected success=%t (exit=%v): %.240s", args[1], want, err, out)
				}
				var result map[string]any
				if want && json.Unmarshal(out, &result) != nil {
					t.Fatal("native operation did not emit valid JSON")
				}
				return result
			}
			dest := "destination-project"
			var operatorRequests atomic.Int32
			tripwire := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				operatorRequests.Add(1)
				w.WriteHeader(http.StatusForbidden)
			}))
			defer tripwire.Close()
			auth := run(true, []string{"CONTEXTLATTICE_DREAM_WORKER_KEY_FILE=" + keyFile,
				"CONTEXTLATTICE_ORCHESTRATOR_URL=" + tripwire.URL}, "dream", "authorize-local",
				"--source", observation.Project, "--destination", dest, "--passport", observation.PassportID,
				"--experience", "exp:cache-key", "--source-digest", hash, "--grant-file", grantFile, "--proof-file", proofFile, "--ttl", "10m")
			id := anyToString(auth["grant_id"])
			if operatorRequests.Load() != 0 || !dreamOwnerGrantID(id) {
				t.Fatal("invalid operator receipt")
			}
			gateway := httptest.NewServer(http.HandlerFunc(s.memoryDream))
			defer func() { gateway.Close() }()
			send := func(want bool, payload map[string]any, proof string) map[string]any {
				t.Helper()
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(requestFile, raw, 0600); err != nil {
					t.Fatal(err)
				}
				reply := run(want, []string{"CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL,
					"CONTEXTLATTICE_WORKER_API_KEY=" + s.toolCalls.workerKey, "CONTEXTLATTICE_DREAM_TRANSFER_PROOF_FILE=" + proof},
					"dream", "transfer", "--project", dest, "--payload-file", requestFile, "--raw")
				return anyMap(reply["dream_experience_transfer"])
			}
			writeObservation := func(name string) string {
				t.Helper()
				if _, _, err := s.memoryStore.put(normalizedWrite{project: dest, fileName: name, topicPath: "evidence",
					content: "destination check: " + verdict}); err != nil {
					t.Fatal(err)
				}
				return dest + "::" + name
			}
			old := writeObservation("evidence/before.txt")
			freeze := anyMap(dreamTransferFreezeInput(observation, id, time.Now().UTC().Add(5*time.Minute))["experience_transfer"])
			wrong := filepath.Join(ops, "different-proof")
			if err := os.WriteFile(wrong, []byte(strings.Repeat("a", 64)), 0600); err != nil {
				t.Fatal(err)
			}
			send(false, freeze, wrong)
			if len(s.contextMesh.dreamTransferReceipts) != 0 {
				t.Fatal("wrong caller left an authority or freeze receipt")
			}
			frozen := send(true, freeze, proofFile)
			transferID := anyToString(frozen["transfer_id"])
			if frozen["status"] != "frozen" || transferID == "" {
				t.Fatal("caller did not receive the frozen prediction")
			}
			gateway.Close()
			openDestination()
			s.memoryStore, err = newMemoryStoreFromEnv()
			if err != nil {
				t.Fatal(err)
			}
			gateway = httptest.NewServer(http.HandlerFunc(s.memoryDream))
			result := map[string]any{"action": "record_result", "transfer_id": transferID, "result": verdict, "evidence_refs": []string{old}}
			if verdict == "withdrawn" {
				result["result"] = "rejected"
			}
			send(false, result, proofFile)
			newRef := writeObservation("evidence/after.txt")
			result["evidence_refs"] = []string{newRef}
			send(false, result, wrong)
			if verdict == "withdrawn" {
				if err := os.Remove(grantFile); err != nil {
					t.Fatal(err)
				}
				send(false, result, proofFile)
				if _, found := s.contextMesh.dreamTransferReceipt("result", transferID); found {
					t.Fatal("withdrawn grant left a terminal result")
				}
			} else {
				reply := send(true, result, proofFile)
				if reply["status"] != verdict || reply["home_project_effect"] != "unchanged" {
					t.Fatal("caller did not receive its exact destination-only result")
				}
				send(false, result, proofFile)
				record, _, ok := s.readDreamExperienceTransfer(context.Background(), dest, transferID)
				if !ok || record.Status != verdict || len(record.ResultEvidence) != 1 || record.ResultEvidence[0]["event_id"] == "" {
					t.Fatal("signed result and durable original evidence did not agree")
				}
			}
			home, found := sender.contextPassports.get(observation.PassportID)
			after, err := json.Marshal(home)
			if !found || err != nil || !bytes.Equal(origin, after) || len(s.memoryStore.recentItems(observation.Project, "", 100, 0)) != 0 {
				t.Fatal("testing the destination changed the home observation")
			}
		})
	}
}

func dreamSourceFromNativeWriteAndExport(t *testing.T, binary string) (*server, contextPassport, string) {
	t.Helper()
	t.Setenv("GO_WRITE_QDRANT_FANOUT_MODE", "disabled")
	t.Setenv("GO_WRITE_PGVECTOR_FANOUT_MODE", "disabled")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	source := &server{memoryStore: newDerivedMemoryTestStore(t), writePolicy: loadWriteIngressPolicy(), strictNoPythonRuntime: true,
		orchestratorAPIKey: "isolated-source-service", contextPassports: newTestPassportStore(t, root)}
	source.contextMesh = newTestMeshStore(t, root, source.contextPassports)
	mux := http.NewServeMux()
	mux.HandleFunc("/memory/write", source.memoryWrite)
	mux.HandleFunc("/memory/context-passport/export", source.memoryContextPassportExport)
	host := httptest.NewServer(mux)
	defer host.Close()
	call := func(args ...string) map[string]any {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = root
		cmd.Env = []string{"HOME=" + root, "TMPDIR=" + root, "PATH=/usr/bin:/bin", "CONTEXTLATTICE_ORCHESTRATOR_URL=" + host.URL,
			"CONTEXTLATTICE_ORCHESTRATOR_API_KEY=" + source.orchestratorAPIKey}
		data, err := cmd.CombinedOutput()
		if bytes.Contains(data, []byte(source.orchestratorAPIKey)) {
			t.Fatal("CLI exposed the source credential")
		}
		var reply map[string]any
		if err != nil || json.Unmarshal(data, &reply) != nil || !anyToBool(reply["ok"]) {
			t.Fatalf("native source %s failed: %v %.240s", args[0], err, data)
		}
		return reply
	}
	const project, file = "home-project", "observations/cache-key.json"
	const original = `{"schema_id":"dream_source_observation.v1","experience_ref":"exp:cache-key","status":"confirmed","mechanism":"cache by stable key","observed_result":"Repeated lookup avoided a duplicate fetch."}`
	written := call("write", "--project", project, "--topic-path", "bench/cache", "--file", file, "--content", original, "--raw")
	event, digest := anyToString(written["event_id"]), anyToString(written["content_hash"])
	if event == "" || !isHexDigest(digest) {
		t.Fatal("native source write has no committed revision identity")
	}
	triple := map[string]any{"memory_id": project + "::" + file, "event_id": event, "content_hash": digest}
	save := func(name string, v any) string {
		t.Helper()
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	target := save("export-request.json", map[string]any{"project": project, "transfer_observation": triple})
	dreamSourceExportFixtureGrant(t, project, project+"::"+file, event, digest)
	response := call("passport-export", "--project", project, "--payload-file", target, "--raw")
	bound := anyMap(response["transfer_source"])
	passport, found := source.contextPassports.get(anyToString(bound["source_passport_id"]))
	_, wantHash, valid := dreamTransferConfirmedSourceClaim(passport, "exp:cache-key")
	if !found || !valid || bound["source_record_hash"] != wantHash || bound["experience_ref"] != "exp:cache-key" ||
		len(verifyContextPassport(passport, time.Now().UTC(), true)) != 0 {
		t.Fatal("exported source receipt is not bound to the real signed observation")
	}
	return source, passport, wantHash
}
