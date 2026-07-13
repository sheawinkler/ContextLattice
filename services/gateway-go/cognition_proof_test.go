package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestTemporalClaimGraphPersistsSupersession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.ndjson")
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH", path)
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_FSYNC", "false")
	store, err := newTemporalClaimStoreFromEnv()
	if err != nil {
		t.Fatalf("create claim store: %v", err)
	}
	oldClaim, err := store.upsert(map[string]any{
		"project": "contextlattice", "subject": "release", "predicate": "current_version", "object": "3.11.2",
		"statement": "The current release is 3.11.2.", "confidence": 0.98,
		"support": []any{map[string]any{"ref_id": "release:v3.11.2", "kind": "release"}},
	})
	if err != nil {
		t.Fatalf("write old claim: %v", err)
	}
	newClaim, err := store.upsert(map[string]any{
		"project": "contextlattice", "subject": "release", "predicate": "current_version", "object": "3.12.0",
		"statement": "The current release is 3.12.0.", "confidence": 0.99,
		"supersedes": []any{oldClaim.ClaimID}, "branch": "main", "commit": "abc123",
		"verification": map[string]any{"status": "verified", "method": "release tag"},
	})
	if err != nil {
		t.Fatalf("write new claim: %v", err)
	}
	if newClaim.ClaimID == oldClaim.ClaimID {
		t.Fatalf("expected object change to create a distinct claim id")
	}
	rows := store.query(temporalClaimQuery{Project: "contextlattice", Query: "release current version", Limit: 10, IncludeSuperseded: true})
	if len(rows) != 2 {
		t.Fatalf("expected two temporal claims, got %#v", rows)
	}
	foundSuperseded := false
	for _, row := range rows {
		if row.ClaimID == oldClaim.ClaimID && row.Status == "superseded" {
			foundSuperseded = true
		}
	}
	if !foundSuperseded {
		t.Fatalf("expected old claim to be superseded, got %#v", rows)
	}
	reloaded, err := newTemporalClaimStoreFromEnv()
	if err != nil {
		t.Fatalf("reload claim store: %v", err)
	}
	if anyToInt(reloaded.snapshot()["claim_count"], 0) != 2 {
		t.Fatalf("expected two persisted claims, got %#v", reloaded.snapshot())
	}
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), newClaim.ClaimID) {
		t.Fatalf("expected append-only persisted claim, err=%v raw=%s", err, string(raw))
	}
}

func TestTemporalClaimGraphRejectsInvalidStatusWithoutMutation(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH", filepath.Join(t.TempDir(), "claims.ndjson"))
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_FSYNC", "false")
	store, err := newTemporalClaimStoreFromEnv()
	if err != nil {
		t.Fatalf("create claim store: %v", err)
	}
	if _, err := store.upsert(map[string]any{
		"project": "contextlattice", "subject": "release", "predicate": "state", "object": "ready", "status": "maybe",
	}); err == nil {
		t.Fatalf("expected invalid status to fail")
	}
	if anyToInt(store.snapshot()["claim_count"], -1) != 0 {
		t.Fatalf("invalid claim mutated store: %#v", store.snapshot())
	}
}

func TestTemporalClaimGraphHidesRetractedClaimsUnlessExplicitlyRequested(t *testing.T) {
	store := &temporalClaimStore{enabled: true, maxClaims: 10, claims: map[string]temporalClaim{}}
	for _, claim := range []temporalClaim{
		{ClaimID: "claim_active", Project: "contextlattice", Subject: "release", Predicate: "state", Object: "ready", Status: "active", Confidence: 0.9},
		{ClaimID: "claim_retracted", Project: "contextlattice", Subject: "release", Predicate: "state", Object: "withdrawn", Status: "retracted", Confidence: 0.9},
	} {
		claim.searchText = temporalClaimSearchText(claim)
		store.claims[claim.ClaimID] = claim
	}

	rows := store.query(temporalClaimQuery{Project: "contextlattice", Limit: 10})
	if len(rows) != 1 || rows[0].ClaimID != "claim_active" {
		t.Fatalf("default query must exclude retracted claims, got %#v", rows)
	}
	retracted := store.query(temporalClaimQuery{Project: "contextlattice", Status: "retracted", Limit: 10})
	if len(retracted) != 1 || retracted[0].ClaimID != "claim_retracted" {
		t.Fatalf("explicit retracted query must return retracted claims, got %#v", retracted)
	}
}

func TestTemporalClaimRoutesReturnValidContracts(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH", filepath.Join(t.TempDir(), "claims.ndjson"))
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_FSYNC", "false")
	s := newTestServer(t, "http://127.0.0.1:1")
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	writePayload := postJSONForTest(t, gateway.URL+"/memory/claims", `{
  "project":"contextlattice",
  "subject":"gateway",
  "predicate":"health_state",
  "object":"healthy",
  "statement":"The gateway health endpoint is healthy.",
  "support":[{"ref_id":"health:/health","kind":"runtime","excerpt":"ok=true"}],
  "verification":{"status":"verified","method":"HTTP 200"}
}`)
	assertBoundaryContractPassed(t, temporalClaimContractID, writePayload)
	claim := anyMap(writePayload["claim"])
	if anyToString(claim["contradiction_state"]) != "clear" {
		t.Fatalf("expected clear claim, got %#v", claim)
	}

	queryPayload := postJSONForTest(t, gateway.URL+"/tools/claim_query", `{"project":"contextlattice","query":"gateway health","limit":10}`)
	assertBoundaryContractPassed(t, temporalClaimQueryContractID, queryPayload)
	if anyToInt(queryPayload["claim_count"], 0) != 1 || anyToString(queryPayload["tool"]) != "claim_query" {
		t.Fatalf("unexpected claim query payload: %#v", queryPayload)
	}
}

func TestAdaptiveRetrievalPlannerIsDeterministicAdvisorOnly(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	plan := s.buildAdaptiveRetrievalPlan(map[string]any{
		"project": "contextlattice", "query": "debug a cross-project retrieval regression",
		"token_budget": 5000,
	})
	if anyToString(plan["schema_id"]) != retrievalPlanContractID || anyToString(plan["mode"]) != "advisor" {
		t.Fatalf("unexpected retrieval plan identity: %#v", plan)
	}
	if anyToString(plan["activation_state"]) != "shadow_only" || anyToBool(anyMap(plan["calibration"])["activation_eligible"]) {
		t.Fatalf("planner must not activate policy: %#v", plan["calibration"])
	}
	if anyToString(plan["task_phase"]) != "debug" || !anyToBool(anyMap(plan["expansion"])["memory_graph"]) {
		t.Fatalf("expected debug graph plan: %#v", plan)
	}
	if anyToInt(plan["token_budget"], 0) != 5000 || len(contextPackAnyList(plan["source_plan"])) == 0 {
		t.Fatalf("expected bounded source and token plan: %#v", plan)
	}
}

func TestProofClaimsExcludeFindingsWithoutEvidenceIdentity(t *testing.T) {
	claims, excluded := proofClaimsFromSynthesis("contextlattice", []any{
		map[string]any{"kind": "decision", "text": "unsupported inference", "source": "qdrant"},
		map[string]any{"kind": "decision", "text": "file-backed decision", "file": "notes/proof.md", "source": "qdrant"},
	}, nil)
	if excluded != 1 || len(claims) != 1 {
		t.Fatalf("expected one unsupported finding excluded, excluded=%d claims=%#v", excluded, claims)
	}
	claim := anyMap(claims[0])
	if len(contextPackAnyList(claim["support"])) != 1 {
		t.Fatalf("expected one bounded file evidence reference, got %#v", claim)
	}
}

func TestProofClaimsKeepContestedClaimAsSupportAndAttachActualOpposition(t *testing.T) {
	supporting := temporalClaim{
		ClaimID: "claim_support", Subject: "release", Predicate: "state", Object: "ready",
		Statement: "The release is ready.", Status: "active", Confidence: 0.9,
		Contradicts: []string{"claim_opposition"},
	}
	opposing := temporalClaim{
		ClaimID: "claim_opposition", Subject: "release", Predicate: "state", Object: "blocked",
		Statement: "The release is blocked.", Status: "active", Confidence: 0.7,
	}
	supporting.searchText = temporalClaimSearchText(supporting)
	opposing.searchText = temporalClaimSearchText(opposing)
	claims, excluded := proofClaimsFromSynthesis("contextlattice", []any{
		map[string]any{"kind": "decision", "text": "The release is ready.", "file": "notes/release.md"},
	}, []temporalClaim{supporting, opposing})
	if excluded != 0 || len(claims) != 1 {
		t.Fatalf("unexpected proof claims: excluded=%d claims=%#v", excluded, claims)
	}
	claim := anyMap(claims[0])
	if anyToString(claim["proof_status"]) != "contested" {
		t.Fatalf("expected contested proof status, got %#v", claim)
	}
	if !proofReferenceListContains(claim["support"], "claim_support") || !proofReferenceListContains(claim["opposition"], "claim_opposition") {
		t.Fatalf("expected supporting and opposing claims on correct sides, got %#v", claim)
	}
}

func proofReferenceListContains(raw any, refID string) bool {
	for _, item := range contextPackAnyList(raw) {
		if anyToString(anyMap(item)["ref_id"]) == refID {
			return true
		}
	}
	return false
}

func TestProofCarryingSynthesisV2ExcludesUnsupportedFindings(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_ENABLED", "true")
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH", filepath.Join(t.TempDir(), "claims.ndjson"))
	t.Setenv("CONTEXTLATTICE_TEMPORAL_CLAIMS_FSYNC", "false")
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/retrieval/query" {
			_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","file":"notes/proof.md","source":"qdrant","score":0.94,"summary":"decision: proof carrying synthesis excludes uncited claims and preserves temporal contradictions","topic_path":"cognition/proof","timestamp":"2026-07-11T00:00:00Z"}],"warnings":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer backend.Close()
	s := newTestServer(t, backend.URL)
	_, err := s.temporalClaims.upsert(map[string]any{
		"project": "contextlattice", "subject": "proof carrying synthesis", "predicate": "behavior", "object": "excludes uncited claims",
		"statement": "Proof carrying synthesis excludes uncited claims.", "confidence": 0.95,
		"verification": map[string]any{"status": "verified", "method": "contract test"},
		"support":      []any{map[string]any{"ref_id": "notes/proof.md", "kind": "memory"}},
	})
	if err != nil {
		t.Fatalf("seed temporal claim: %v", err)
	}
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	for _, path := range []string{"/memory/synthesis-pack/v2", "/tools/synthesis_pack_v2"} {
		t.Run(path, func(t *testing.T) {
			payload := postJSONForTest(t, gateway.URL+path, `{"project":"contextlattice","query":"verify proof carrying synthesis","topic_path":"cognition/proof","limit":8,"retrieval_mode":"balanced"}`)
			assertBoundaryContractPassed(t, synthesisPackV2ContractID, payload)
			assertBoundaryJSONUnderLimit(t, synthesisPackV2ContractID, payload)
			pack := anyMap(payload["synthesis_pack"])
			claims := contextPackAnyList(pack["proof_claims"])
			if len(claims) == 0 {
				t.Fatalf("expected proof claims, got %#v", pack)
			}
			for _, raw := range claims {
				claim := anyMap(raw)
				if len(contextPackAnyList(claim["support"])) == 0 || anyToString(claim["proof_status"]) == "unsupported" {
					t.Fatalf("proof claim lacks support: %#v", claim)
				}
			}
			trace := anyMap(pack["synthesis_trace"])
			if anyToBool(trace["llm_used"]) || anyToString(trace["mode"]) != "deterministic_proof_v2" {
				t.Fatalf("expected deterministic no-LLM trace: %#v", trace)
			}
			plan := anyMap(payload["retrieval_plan"])
			if anyToString(plan["activation_state"]) != "shadow_only" {
				t.Fatalf("expected advisor-only retrieval plan: %#v", plan)
			}
		})
	}
}

func postJSONForTest(t *testing.T, url string, body string) map[string]any {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("post %s expected 200, got %d body=%s", url, resp.StatusCode, string(raw))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return payload
}

func BenchmarkTemporalClaimQuery1000(b *testing.B) {
	store := &temporalClaimStore{enabled: true, maxClaims: 2000, claims: map[string]temporalClaim{}}
	for i := 0; i < 1000; i++ {
		id := "claim_bench_" + sha256Hex(strconv.Itoa(i))[:24]
		claim := temporalClaim{
			ClaimID: id, Project: "contextlattice", Subject: "retrieval planner",
			Predicate: "benchmark", Object: "bounded query", Statement: "The retrieval planner uses bounded evidence obligations.",
			Status: "active", Confidence: 0.8, UpdatedAt: "2026-07-11T00:00:00Z",
		}
		claim.searchText = temporalClaimSearchText(claim)
		store.claims[id] = claim
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = store.query(temporalClaimQuery{Project: "contextlattice", Query: "retrieval planner evidence", Limit: 32})
	}
}

func BenchmarkAdaptiveRetrievalPlanner(b *testing.B) {
	s := &server{
		retrieval: retrievalPolicy{
			defaultSources:   []string{sourceTopicRollup, sourceQdrant},
			fastSources:      []string{sourceTopicRollup, sourceQdrant},
			slowSources:      []string{sourceLetta, sourceMemoryBank},
			protectedSources: map[string]struct{}{sourceTopicRollup: {}},
		},
		adaptiveBySource:   map[string]*adaptiveSourceStats{},
		contextPackQuality: newContextPackQualityTelemetry(10),
	}
	payload := map[string]any{"project": "contextlattice", "query": "verify cross-project cognition proof", "token_budget": 5000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.buildAdaptiveRetrievalPlan(payload)
	}
}

func BenchmarkProofClaims32(b *testing.B) {
	findings := make([]any, 0, 32)
	for i := 0; i < 32; i++ {
		findings = append(findings, map[string]any{
			"kind": "decision", "text": "Proof synthesis preserves evidence and temporal state.",
			"file": "notes/proof.md", "source": "qdrant", "confidence": 0.82,
		})
	}
	temporal := []temporalClaim{{
		ClaimID: "claim_benchmark", Project: "contextlattice", Subject: "proof synthesis",
		Predicate: "preserves", Object: "evidence", Statement: "Proof synthesis preserves evidence and temporal state.",
		Status: "active", Confidence: 0.9, Verification: map[string]any{"status": "verified"},
	}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = proofClaimsFromSynthesis("contextlattice", findings, temporal)
	}
}
