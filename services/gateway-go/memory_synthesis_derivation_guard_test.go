package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSynthesisFinalDerivationGuardRejectsOwnerCorrection(t *testing.T) {
	for _, version := range []string{synthesisPackContractID, synthesisPackV2ContractID} {
		for _, mode := range []string{"full", "agent_packet"} {
			for _, mutation := range []string{"stable", "changed", "unrelated"} {
				t.Run(version+"/"+mode+"/"+mutation, func(t *testing.T) {
					t.Setenv("GO_RETRIEVAL_NATIVE_QDRANT_ENABLED", "false")
					t.Setenv("ORCH_RETRIEVAL_SOURCES", sourceQdrant)
					t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", sourceQdrant)
					t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
					var rows []map[string]any
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/v1/retrieval/query" {
							t.Errorf("unexpected backend path: %s", r.URL.Path)
							http.NotFound(w, r)
							return
						}
						writeJSON(w, http.StatusOK, map[string]any{"results": rows, "warnings": []any{}})
					}))
					defer backend.Close()
					s := newTestServer(t, backend.URL)
					store := newDerivedMemoryTestStore(t)
					s.memoryStore = store
					input := seedMemoryGraphRepairDoc(t, store, "alpha", "notes/input.md", "pilot/derivation", "needle original input", "agent", "session")
					derived := putDerivedMemory(t, store, "notes/derived.md", input)
					benign := seedMemoryGraphRepairDoc(t, store, "alpha", "notes/benign.md", "pilot/derivation", "needle benign evidence", "agent", "session")
					unrelated := seedMemoryGraphRepairDoc(t, store, "alpha", "notes/unrelated.md", "other/topic", "unrelated original input", "agent", "session")
					rows = []map[string]any{derivedMemoryRow(derived), derivedMemoryRow(benign)}
					request := map[string]any{"project": "alpha", "query": "needle", "limit": 8, "max_facts": 16, "token_budget": 16000}
					if mode == "agent_packet" {
						request["output_mode"] = mode
						request["target_context_pack_tokens"] = defaultAgentPacketHardTokens
						request["hard_limit_tokens"] = defaultAgentPacketHardTokens
					}
					assertOutput := func(response map[string]any, status int, err error) {
						t.Helper()
						wantSchema := version
						evidence := contextPackAnyList(anyMap(response["context_pack"])["ranked_evidence"])
						if mode == "agent_packet" {
							wantSchema = agentPacketContractID
							evidence = contextPackAnyList(response["evidence"])
						}
						if err != nil || status != http.StatusOK || !anyToBool(response["ok"]) || response["schema_id"] != wantSchema {
							t.Fatalf("valid synthesis refused: status=%d err=%v schema=%v", status, err, response["schema_id"])
						}
						wire, marshalErr := json.Marshal(evidence)
						if marshalErr != nil {
							t.Fatal(marshalErr)
						}
						for _, entry := range []memoryStoreEntry{derived, benign} {
							if !bytes.Contains(wire, []byte(entry.FileName)) || !bytes.Contains(wire, []byte(entry.Summary)) {
								t.Fatalf("valid evidence lost for %s: %s", entry.FileName, wire)
							}
						}
					}
					// Exercise the unchanged entrypoints before installing this request's seam.
					if version == synthesisPackContractID {
						assertOutput(s.buildSynthesisPackResponse(context.Background(), http.Header{}, request, "/memory/synthesis-pack"))
					} else {
						assertOutput(s.buildSynthesisPackV2Response(context.Background(), http.Header{}, request, "/memory/synthesis-pack/v2"))
					}
					ctx, guard := withMemoryDerivationGuard(context.Background(), store)
					calls := 0
					options := synthesisResponseBuildOptions{beforeFinalGuard: func(observedVersion string) {
						calls++
						if observedVersion != version || calls != 1 {
							t.Fatalf("wrong final boundary: version=%s calls=%d", observedVersion, calls)
						}
						guard.mu.Lock()
						roots := append([]memoryStoreEntry(nil), guard.roots...)
						guard.mu.Unlock()
						captured := false
						for _, root := range roots {
							captured = captured || root.Project == derived.Project && root.FileName == derived.FileName && root.EventID == derived.EventID && root.ContentHash == derived.ContentHash
						}
						if !captured || guard.verify(ctx) != nil {
							t.Fatal("final boundary did not retain a valid real derivation owner")
						}
						if mutation == "stable" {
							return
						}
						target := input
						if mutation == "unrelated" {
							target = unrelated
						}
						corrected := seedMemoryGraphRepairDoc(t, store, target.Project, target.FileName, target.TopicPath, "corrected owner during synthesis", "agent", "session")
						if corrected.EventID == target.EventID || corrected.ContentHash == target.ContentHash {
							t.Fatal("correction did not change the real event and content hash")
						}
					}}
					var response map[string]any
					var status int
					var err error
					if version == synthesisPackContractID {
						response, status, err = s.buildSynthesisPackResponseWithOptions(ctx, http.Header{}, request, "/memory/synthesis-pack", options)
					} else {
						response, status, err = s.buildSynthesisPackV2ResponseWithOptions(ctx, http.Header{}, request, "/memory/synthesis-pack/v2", options)
					}
					if calls != 1 {
						t.Fatalf("request did not reach its final synthesis boundary: calls=%d err=%v", calls, err)
					}
					if mutation == "changed" {
						if !errors.Is(err, errMemoryDerivationUnavailable) || status != http.StatusServiceUnavailable || response != nil {
							t.Fatalf("stale synthesis escaped: status=%d err=%v response_present=%v", status, err, response != nil)
						}
						filtered, _, suppressed := s.filterDerivedMemoryRows(context.Background(), rows, false)
						if suppressed != 1 || len(filtered) != 1 || filtered[0]["file"] != benign.FileName {
							t.Fatalf("correction caused collateral evidence loss: suppressed=%d rows=%v", suppressed, filtered)
						}
					} else {
						assertOutput(response, status, err)
					}
					for _, entry := range []memoryStoreEntry{derived, benign} {
						current, ok := store.currentEntry(entry.Project, entry.FileName)
						if !ok || current.EventID != entry.EventID || current.ContentHash != entry.ContentHash {
							t.Fatalf("synthesis altered unrelated retained record %s", entry.FileName)
						}
					}
				})
			}
		}
	}
}
