package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecallResponseLifecycleMarkersCannotSupportOrPrepareAction(t *testing.T) {
	toolRef := "sha256:" + strings.Repeat("a", 64)
	parameterRef := "sha256:" + strings.Repeat("b", 64)
	base := map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("1", 24),
		"kind":         "runbook",
		"status":       "current",
		"confidence":   0.98,
		"action_evidence": map[string]any{
			"tool_ref": toolRef,
			"parameter_bindings": []any{map[string]any{
				"parameter_ref": parameterRef, "value_state": "resolved", "required": true,
			}},
		},
	}
	markers := []struct {
		name string
		set  func(map[string]any)
	}{
		{name: "retired_lifecycle", set: func(row map[string]any) { row["lifecycle"] = "retired" }},
		{name: "retired_status_mixed_with_current", set: func(row map[string]any) { row["status"] = "retired" }},
		{name: "forgotten_flag", set: func(row map[string]any) { row["forgotten"] = true }},
		{name: "test_flag", set: func(row map[string]any) { row["is_test"] = true }},
		{name: "test_lifecycle_mixed_with_current", set: func(row map[string]any) {
			row["lifecycle"] = "test"
			row["status"] = "current"
		}},
		{name: "stale_freshness_mixed_with_current", set: func(row map[string]any) {
			row["freshness"] = "stale"
			row["status"] = "current"
		}},
		{name: "unknown_state_mixed_with_current", set: func(row map[string]any) {
			row["state"] = "unknown"
			row["status"] = "current"
		}},
		{name: "nested_unknown_state_mixed_with_current", set: func(row map[string]any) {
			row["recall_metadata"] = map[string]any{"temporal": map[string]any{"state": "unknown"}}
			row["status"] = "current"
		}},
		{name: "nested_forgotten_marker", set: func(row map[string]any) {
			row["recall_metadata"] = map[string]any{"temporal": map[string]any{"forgotten": true}}
		}},
		{name: "mixed_test_and_forgotten_markers", set: func(row map[string]any) {
			row["is_test"] = true
			row["forgotten"] = true
		}},
	}
	for _, marker := range markers {
		t.Run(marker.name, func(t *testing.T) {
			row := cloneJSONMap(base)
			marker.set(row)
			if status, eligible := recallResponseEvidenceStatus(row); eligible {
				t.Fatalf("hard lifecycle marker became supporting status=%q row=%#v", status, row)
			}

			source := map[string]any{
				"retrieval_intent": "action",
				"source_coverage":  map[string]any{"complete": true},
				"context_pack":     map[string]any{"ranked_evidence": []any{row}},
			}
			if recallResponseActionProjectionAllowed(source) {
				t.Fatal("hard lifecycle marker became an action witness")
			}
			payload := recallResponseActionPayload(
				"memory_to_action",
				map[string]any{},
				[]string{"rtc_" + strings.Repeat("1", 24)},
				source,
			)
			if got := anyToString(payload["intended_tool_ref"]); got != "unresolved_tool" {
				t.Fatalf("hard lifecycle marker prepared a tool %q: %#v", got, payload)
			}
		})
	}
}

func TestRecallResponseUnknownLifecycleIsExcludedButTraversable(t *testing.T) {
	currentRef := "rtc_" + strings.Repeat("3", 24)
	unknownRef := "rtc_" + strings.Repeat("4", 24)
	unknownTool := "sha256:" + strings.Repeat("d", 64)
	rows := []any{
		map[string]any{
			"candidate_id": currentRef, "kind": "fact", "status": "current", "state": "current",
			"confidence": 0.91, "source": "qdrant", "summary": "known support",
		},
		map[string]any{
			"candidate_id": unknownRef, "kind": "runbook", "status": "current", "state": "unknown",
			"confidence": 0.99, "source": "qdrant", "summary": "unknown lifecycle action",
			"action_evidence": map[string]any{
				"tool_ref": unknownTool,
				"parameter_bindings": []any{map[string]any{
					"parameter_ref": "sha256:" + strings.Repeat("e", 64), "value_state": "resolved", "required": true,
				}},
			},
		},
	}
	pack := buildContextPackPayload("known and unknown lifecycle", map[string]any{"results": rows}, 2, 2)
	input := recallResponseTestInput(false)
	input["query"] = "what is current and what remains unknown"
	input["task_class"] = "timeline"
	input["retrieval_intent"] = "action"
	input["context_pack"] = pack
	input["source_coverage"] = map[string]any{"complete": true}
	response := composeRecallResponse(input)
	if got := contextPackAnyList(response["evidence"]); len(got) != 1 || anyToString(anyMap(got[0])["ref_id"]) != currentRef {
		t.Fatalf("unknown lifecycle displaced or joined current support: %#v", got)
	}
	if !recallResponseDisclosureRefs(response, "exclusion_refs")[unknownRef] {
		t.Fatalf("unknown lifecycle identity was hidden instead of disclosed as excluded: %#v", response["disclosure"])
	}
	if strings.Contains(recallResponseCanonicalJSON(response), unknownTool) {
		t.Fatalf("unknown lifecycle action witness crossed response policy: %#v", response)
	}
	periods := recallResponseUnknownPeriods(
		response, []string{"ref_gap_" + strings.Repeat("5", 24)}, input,
	)
	if len(periods) != 1 || anyToString(anyMap(periods[0])["reason"]) != "transition_boundary_unproven" {
		t.Fatalf("unknown lifecycle did not remain typed unknown-period evidence: %#v", periods)
	}
	membership, ok := recallResponseContinuationMembership(input, response, recallResponseProductionPolicyInput())
	if !ok {
		t.Fatal("unknown lifecycle source membership was not terminally traversable")
	}
	hardExcluded := map[string]bool{}
	for _, raw := range membership.All {
		row := anyMap(raw)
		if anyToString(row["disposition"]) == "hard_excluded" {
			hardExcluded[anyToString(row["item_ref"])] = true
		}
	}
	if !hardExcluded[unknownRef] {
		t.Fatalf("unknown lifecycle source membership was not typed hard-excluded: %#v", membership.All)
	}
	selectedOrProof := map[string]bool{}
	for _, raw := range contextPackAnyList(response["evidence"]) {
		selectedOrProof[anyToString(anyMap(raw)["ref_id"])] = true
	}
	for _, raw := range contextPackAnyList(anyMap(anyMap(response["answer"])["proof_spine"])["proof_refs"]) {
		selectedOrProof[anyToString(raw)] = true
	}
	if selectedOrProof[unknownRef] && hardExcluded[unknownRef] {
		t.Fatal("one unknown record was both support/proof and hard-excluded")
	}

	allUnknown := cloneJSONMap(input)
	allUnknownPack := buildContextPackPayload("all unknown lifecycle", map[string]any{"results": []any{rows[1]}}, 1, 1)
	allUnknown["context_pack"] = allUnknownPack
	if got := composeRecallResponse(allUnknown); len(contextPackAnyList(got["evidence"])) != 0 {
		t.Fatalf("all-unknown input produced support: %#v", got["evidence"])
	}
	if recallResponseActionProjectionAllowed(allUnknown) {
		t.Fatal("all-unknown input became action-eligible")
	}
}

func TestRecallResponseRouteUnknownLifecycleCannotSupportOrPrepareAction(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "false")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	currentRef := "rtc_" + strings.Repeat("6", 24)
	unknownRef := "rtc_" + strings.Repeat("7", 24)
	unknownTool := "sha256:" + strings.Repeat("8", 64)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/retrieval/query":
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
				map[string]any{
					"project": "contextlattice", "file": "notes/recall/known.md", "topic_path": "recall/unknown-lifecycle",
					"source": "qdrant", "score": 0.91, "summary": "known", "candidate_id": currentRef, "status": "current", "state": "current",
				},
				map[string]any{
					"project": "contextlattice", "file": "notes/recall/unknown.md", "topic_path": "recall/unknown-lifecycle",
					"source": "qdrant", "score": 0.99, "summary": "unknown action",
					"candidate_id": unknownRef, "status": "current", "state": "unknown",
					"action_evidence": map[string]any{"tool_ref": unknownTool},
				},
			}, "warnings": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	t.Cleanup(gateway.Close)
	resp, payload, raw := recallResponseRouteRequest(
		t, http.MethodPost, gateway.URL+memoryRecallResponsePath,
		`{"query":"mixed unknown lifecycle action","retrieval_intent":"action","agent_id":"p1-unknown-route"}`, nil,
	)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown lifecycle route failed: status=%d body=%s", resp.StatusCode, raw)
	}
	foundCurrent := false
	for _, evidence := range contextPackAnyList(payload["evidence"]) {
		refID := anyToString(anyMap(evidence)["ref_id"])
		if refID == currentRef {
			foundCurrent = true
		}
		if refID == unknownRef {
			t.Fatalf("route selected unknown lifecycle support: %#v", payload["evidence"])
		}
	}
	if !foundCurrent {
		t.Fatalf("route regression did not retain the known half of the mixed case: %#v", payload["evidence"])
	}
	if !recallResponseDisclosureRefs(payload, "exclusion_refs")[unknownRef] || strings.Contains(recallResponseCanonicalJSON(payload), unknownTool) {
		t.Fatalf("route hid the unknown exclusion or projected its action: %#v", payload)
	}
	action := anyMap(anyMap(payload["disclosure"])["continuation_action"])
	foundHardExcluded := false
	for anyToString(action["kind"]) == "continue_snapshot" {
		body, err := json.Marshal(map[string]any{
			"continuation_token": action["token"], "continuation_scope_digest": action["scope_digest"],
			"continuation_request_digest": action["request_digest"], "agent_id": "p1-unknown-route",
		})
		if err != nil {
			t.Fatal(err)
		}
		pageResp, page, pageRaw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, string(body), nil)
		if pageResp.StatusCode != http.StatusOK || !validateRecallResponseContinuationPage(page) {
			t.Fatalf("unknown lifecycle continuation failed: status=%d body=%s", pageResp.StatusCode, pageRaw)
		}
		for _, rawItem := range contextPackAnyList(page["items"]) {
			item := anyMap(rawItem)
			if anyToString(item["item_ref"]) == unknownRef && anyToString(item["disposition"]) == "hard_excluded" {
				foundHardExcluded = true
			}
		}
		action = anyMap(page["continuation_action"])
	}
	if !foundHardExcluded || anyToString(action["kind"]) != "terminal" {
		t.Fatalf("unknown lifecycle membership was not terminally traversed as hard-excluded: found=%v action=%#v", foundHardExcluded, action)
	}
}

func TestRecallResponseTestLifecycleCannotProveRetirementOrNegativeHistory(t *testing.T) {
	row := map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("7", 24),
		"kind":         "fact",
		"status":       "retired",
		"lifecycle":    "test",
		"confidence":   0.99,
	}
	source := map[string]any{
		"source_coverage": map[string]any{"complete": true, "returned": []any{"qdrant"}},
		"context_pack":    map[string]any{"ranked_evidence": []any{row}},
	}
	if recallResponseTemporalHasRetirement(source) {
		t.Fatal("test-only lifecycle became retirement or selective-forgetting proof")
	}
	if periods := recallResponseUnknownPeriods(recallResponseTestInput(false), []string{"rtc_" + strings.Repeat("7", 24)}, source); len(periods) != 0 {
		t.Fatalf("test-only lifecycle created an unsupported historical gap: %#v", periods)
	}
	mixed := cloneJSONMap(row)
	mixed["is_test"] = true
	mixed["forgotten"] = true
	mixedSource := cloneJSONMap(source)
	anyMap(mixedSource["context_pack"])["ranked_evidence"] = []any{mixed}
	if recallResponseTemporalHasRetirement(mixedSource) {
		t.Fatal("mixed test and forgotten fixture became retirement or selective-forgetting proof")
	}
	responseInput := recallResponseTestInput(false)
	responseInput["source_coverage"] = source["source_coverage"]
	responseInput["context_pack"] = source["context_pack"]
	response := composeRecallResponse(responseInput)
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		if anyToString(anyMap(raw)["kind"]) == "conflict_supersession" {
			t.Fatal("test-only lifecycle created a retirement conflict module")
		}
	}
}

func TestRecallResponseIncludeTestMemoryCannotReintroduceSupportingEvidence(t *testing.T) {
	testRow := map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("2", 24),
		"kind":         "fact",
		"status":       "current",
		"lifecycle":    "test",
		"confidence":   0.99,
		"summary":      "test-only retrieval row",
	}
	request := recallResponseRequestPayload(map[string]any{
		"query":               "test memory",
		"include_test_memory": true,
	})
	if !anyToBool(request["include_test_memory"]) {
		t.Fatal("request normalizer dropped the explicit retrieval opt-in")
	}
	pack := buildContextPackPayload("test memory", map[string]any{
		"results": []any{testRow},
	}, 1, 1)
	input := recallResponseTestInput(false)
	input["retrieval_intent"] = "decision"
	input["task_class"] = "general"
	input["context_pack"] = pack
	response := composeRecallResponse(input)
	if got := len(contextPackAnyList(response["evidence"])); got != 0 {
		t.Fatalf("include_test_memory promoted test retrieval into support: %#v", response["evidence"])
	}
	for _, raw := range contextPackAnyList(anyMap(response["answer"])["components"]) {
		module := anyMap(raw)
		if anyToString(module["kind"]) != "memory_to_action" {
			continue
		}
		if got := anyToString(anyMap(module["payload"])["intended_tool_ref"]); got != "unresolved_tool" {
			t.Fatalf("include_test_memory promoted a test action witness: %#v", module)
		}
	}
}

func TestRecallResponseRouteIncludeTestMemoryCannotProduceSupport(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "false")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/retrieval/query":
			_, _ = w.Write([]byte(`{"results":[{"project":"contextlattice","source":"qdrant","score":0.99,"summary":"test-only result","topic_path":"test/recall","candidate_id":"rtc_aaaaaaaaaaaaaaaaaaaaaaaa","status":"current","recall_metadata":{"temporal":{"forgotten":true}}}],"warnings":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	t.Cleanup(gateway.Close)
	resp, payload, raw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, `{"query":"test retrieval","include_test_memory":true,"agent_id":"p1-test-route"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test-memory route failed: status=%d body=%s", resp.StatusCode, raw)
	}
	if got := len(contextPackAnyList(payload["evidence"])); got != 0 {
		t.Fatalf("route include_test_memory promoted a test row into support: %#v", payload["evidence"])
	}
}

func TestRecallResponseRouteRetainsTwoSourcePreClipMembership(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "mindsdb,qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "mindsdb,qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("QDRANT_URL", "")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("GO_MINDSDB_SQL_URL", "")
	t.Setenv("MINDSDB_SQL_URL", "")
	t.Setenv("MINDSDB_URL", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "false")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	paths := []string{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/retrieval/query":
			body, _ := io.ReadAll(r.Body)
			request := map[string]any{}
			_ = json.Unmarshal(body, &request)
			sources := anyToStringSlice(anyMap(request["request"])["sources"])
			source := "mindsdb"
			if len(sources) > 0 {
				source = sources[0]
			}
			rows := make([]map[string]any, 0, 10)
			base := 0
			if source == "qdrant" {
				base = 10
			}
			for index := 0; index < 10; index++ {
				rows = append(rows, map[string]any{
					"project":      "contextlattice",
					"file":         fmt.Sprintf("notes/recall/%s-%d.md", source, index+1),
					"source":       source,
					"score":        0.99 - float64(index)/1000,
					"summary":      fmt.Sprintf("%s source row %d", source, index+1),
					"candidate_id": fmt.Sprintf("rtc_%024x", base+index+1),
					"status":       "current",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": rows, "warnings": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	t.Cleanup(gateway.Close)
	initialResp, initial, raw := recallResponseRouteRequest(
		t, http.MethodPost, gateway.URL+memoryRecallResponsePath,
		`{"query":"two source bounded recall","limit":1,"sources":["mindsdb","qdrant"],"agent_id":"p1-two-source"}`,
		nil,
	)
	if initialResp.StatusCode != http.StatusOK {
		t.Fatalf("two-source route failed: status=%d body=%s", initialResp.StatusCode, raw)
	}
	disclosure := anyMap(initial["disclosure"])
	if anyToInt(anyMap(disclosure["source_counts"])["evidence"], 0) != 20 ||
		anyToBool(disclosure["source_truncated"]) ||
		anyToInt(anyMap(disclosure["union_counts"])["evidence"], 0) != 20 {
		t.Fatalf("two-source route did not preserve exact disjoint source accounting: disclosure=%#v", disclosure)
	}
	seen := map[string]bool{}
	seenCount := map[string]int{}
	for _, rawEvidence := range contextPackAnyList(initial["evidence"]) {
		if ref := anyToString(anyMap(rawEvidence)["ref_id"]); strings.HasPrefix(ref, "rtc_") {
			seen[ref] = true
			seenCount[ref]++
		}
	}
	action := anyMap(anyMap(initial["disclosure"])["continuation_action"])
	pages := 0
	for anyToString(action["kind"]) == "continue_snapshot" {
		pages++
		if pages > 8 {
			t.Fatal("two-source continuation did not make bounded progress")
		}
		body, err := json.Marshal(map[string]any{
			"continuation_token":          action["token"],
			"continuation_scope_digest":   action["scope_digest"],
			"continuation_request_digest": action["request_digest"],
			"agent_id":                    "p1-two-source",
		})
		if err != nil {
			t.Fatal(err)
		}
		pageResp, page, pageRaw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, string(body), nil)
		if pageResp.StatusCode != http.StatusOK || !validateRecallResponseContinuationPage(page) {
			t.Fatalf("two-source continuation failed: status=%d body=%s", pageResp.StatusCode, pageRaw)
		}
		for _, rawItem := range contextPackAnyList(page["items"]) {
			item := anyMap(rawItem)
			if anyToString(item["item_type"]) == "evidence" {
				if ref := anyToString(item["item_ref"]); strings.HasPrefix(ref, "rtc_") {
					seen[ref] = true
					seenCount[ref]++
				}
			}
		}
		action = anyMap(page["continuation_action"])
	}
	if anyToString(action["kind"]) != "terminal" {
		t.Fatalf("two-source initial response terminated silently: action=%#v initial=%#v paths=%v", action, initial, paths)
	}
	for index := 1; index <= 20; index++ {
		ref := fmt.Sprintf("rtc_%024x", index)
		if !seen[ref] {
			t.Fatalf("safe pre-clip source identity was neither initially represented nor traversable: %s seen=%v action=%#v initial=%#v paths=%v", ref, seen, action, initial, paths)
		}
		if seenCount[ref] != 1 {
			t.Fatalf("safe pre-clip source identity was not represented exactly once: %s count=%d", ref, seenCount[ref])
		}
	}
	retrievalCalls := 0
	for _, path := range paths {
		if path == "/v1/retrieval/query" {
			retrievalCalls++
		}
	}
	if retrievalCalls != 2 {
		t.Fatalf("continuation repeated retrieval or failed to fan out exactly once per source: paths=%v", paths)
	}
}

func TestRecallResponseRouteRetainsOver128PreClipMembership(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "mindsdb,qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "mindsdb,qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("QDRANT_URL", "")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("GO_MINDSDB_SQL_URL", "")
	t.Setenv("MINDSDB_SQL_URL", "")
	t.Setenv("MINDSDB_URL", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "false")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	paths := []string{}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/retrieval/query":
			body, _ := io.ReadAll(r.Body)
			request := map[string]any{}
			_ = json.Unmarshal(body, &request)
			sources := anyToStringSlice(anyMap(request["request"])["sources"])
			source := "mindsdb"
			base := 0
			if len(sources) > 0 {
				source = sources[0]
			}
			if source == "qdrant" {
				base = 65
			}
			rows := make([]map[string]any, 0, 65)
			for index := 0; index < 65; index++ {
				ordinal := base + index + 1
				rows = append(rows, map[string]any{
					"project":      "contextlattice",
					"file":         fmt.Sprintf("notes/recall/%s-%d.md", source, ordinal),
					"source":       source,
					"score":        0.99 - float64(index)/1000,
					"summary":      fmt.Sprintf("%s over128 source row %d", source, ordinal),
					"topic_path":   "recall/over128",
					"candidate_id": fmt.Sprintf("rtc_%024x", ordinal),
					"status":       "current",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": rows, "warnings": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	t.Cleanup(gateway.Close)
	initialResp, initial, raw := recallResponseRouteRequest(
		t, http.MethodPost, gateway.URL+memoryRecallResponsePath,
		`{"query":"over128 bounded recall","limit":1,"sources":["mindsdb","qdrant"],"agent_id":"p1-over128"}`,
		nil,
	)
	if initialResp.StatusCode != http.StatusOK {
		t.Fatalf("over128 route failed: status=%d body=%s", initialResp.StatusCode, raw)
	}
	disclosure := anyMap(initial["disclosure"])
	if anyToInt(anyMap(disclosure["source_counts"])["evidence"], 0) != 130 ||
		anyToInt(anyMap(disclosure["union_counts"])["evidence"], 0) <= 0 ||
		anyToInt(anyMap(disclosure["omitted_counts"])["source"], 0) != 2 {
		t.Fatalf("over128 route lost complete pre-clip source custody: disclosure=%#v", disclosure)
	}
	seenCount := map[string]int{}
	for _, rawEvidence := range contextPackAnyList(initial["evidence"]) {
		ref := anyToString(anyMap(rawEvidence)["ref_id"])
		if strings.HasPrefix(ref, "rtc_") {
			seenCount[ref]++
		}
	}
	action := anyMap(disclosure["continuation_action"])
	pages := 0
	for anyToString(action["kind"]) == "continue_snapshot" {
		pages++
		if pages > 8 {
			t.Fatal("over128 continuation did not make bounded progress")
		}
		body, err := json.Marshal(map[string]any{
			"continuation_token":          action["token"],
			"continuation_scope_digest":   action["scope_digest"],
			"continuation_request_digest": action["request_digest"],
			"agent_id":                    "p1-over128",
		})
		if err != nil {
			t.Fatal(err)
		}
		pageResp, page, pageRaw := recallResponseRouteRequest(t, http.MethodPost, gateway.URL+memoryRecallResponsePath, string(body), nil)
		if pageResp.StatusCode != http.StatusOK || !validateRecallResponseContinuationPage(page) {
			t.Fatalf("over128 continuation failed: status=%d body=%s", pageResp.StatusCode, pageRaw)
		}
		for _, rawItem := range contextPackAnyList(page["items"]) {
			item := anyMap(rawItem)
			if anyToString(item["item_type"]) == "evidence" {
				ref := anyToString(item["item_ref"])
				if strings.HasPrefix(ref, "rtc_") {
					seenCount[ref]++
				}
			}
		}
		action = anyMap(page["continuation_action"])
	}
	if anyToString(action["kind"]) != "terminal" {
		t.Fatalf("over128 response did not reach terminal continuation: action=%#v", action)
	}
	for ordinal := 1; ordinal <= 130; ordinal++ {
		ref := fmt.Sprintf("rtc_%024x", ordinal)
		if seenCount[ref] != 1 {
			t.Fatalf("over128 source identity was not traversable exactly once: %s count=%d", ref, seenCount[ref])
		}
	}
	retrievalCalls := 0
	for _, path := range paths {
		if path == "/v1/retrieval/query" {
			retrievalCalls++
		}
	}
	if retrievalCalls != 2 {
		t.Fatalf("over128 continuation repeated retrieval or failed to fan out exactly once per source: paths=%v", paths)
	}
}

func TestRecallResponseRouteSourceInputOverflowFailsClosed(t *testing.T) {
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "true")
	t.Setenv("ORCH_RETRIEVAL_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_FAST_SOURCES", "qdrant")
	t.Setenv("ORCH_RETRIEVAL_SLOW_SOURCES", "")
	t.Setenv("QDRANT_URL", "")
	t.Setenv("QDRANT_LOCAL_URL", "")
	t.Setenv("GO_CONTEXT_PACK_GRAPH_NEIGHBORS_ENABLED", "false")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_ENABLED", "true")
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	const candidateCount = recallResponseContinuationMaximumItems + 1
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/v1/retrieval/query":
			rows := make([]map[string]any, 0, candidateCount)
			for index := 0; index < candidateCount; index++ {
				ordinal := index + 1
				rows = append(rows, map[string]any{
					"project":      "contextlattice",
					"file":         fmt.Sprintf("notes/recall/overflow-%d.md", ordinal),
					"source":       "qdrant",
					"score":        0.99 - float64(index)/100000,
					"summary":      fmt.Sprintf("overflow source row %d", ordinal),
					"candidate_id": fmt.Sprintf("rtc_%024x", ordinal),
					"status":       "current",
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": rows, "warnings": []any{}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(backend.Close)
	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	t.Cleanup(gateway.Close)
	resp, payload, raw := recallResponseRouteRequest(
		t, http.MethodPost, gateway.URL+memoryRecallResponsePath,
		`{"query":"source input overflow","limit":1,"sources":["qdrant"],"agent_id":"p1-source-overflow"}`,
		nil,
	)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("source-input overflow route failed: status=%d body=%s", resp.StatusCode, raw)
	}
	disclosure := anyMap(payload["disclosure"])
	sourceCounts := anyMap(disclosure["source_counts"])
	omittedCounts := anyMap(disclosure["omitted_counts"])
	if anyToInt(sourceCounts["evidence"], 0) != candidateCount ||
		anyToInt(omittedCounts["source"], 0) != candidateCount-recallResponseMaxSourceCapture ||
		!anyToBool(disclosure["source_truncated"]) || !anyToBool(disclosure["union_truncated"]) {
		t.Fatalf("source-input overflow did not retain exact bounded custody accounting: disclosure=%#v", disclosure)
	}
	action := anyMap(disclosure["continuation_action"])
	if anyToString(action["kind"]) != "owner_cursor_unavailable" || anyToString(action["token"]) != "" ||
		!recallResponseContinuationActionValid(payload, action) {
		t.Fatalf("source-input overflow issued a terminal or traversable cursor instead of failing closed: action=%#v disclosure=%#v", action, disclosure)
	}
	if anyToInt(anyMap(disclosure["union_counts"])["evidence"], 0) >= candidateCount {
		t.Fatalf("source-input overflow disclosed all candidates as a complete supporting union: disclosure=%#v", disclosure)
	}
	searchRequest := map[string]any{
		"query":   "source input overflow",
		"limit":   1,
		"sources": []any{"qdrant"},
	}
	serverCaptured, status, err := s.executeRetrieval(
		withRecallResponseServerObservationCapture(context.Background()),
		http.Header{}, searchRequest, true,
	)
	if err != nil || status != http.StatusOK {
		t.Fatalf("server-owned source envelope retrieval failed: status=%d err=%v", status, err)
	}
	sourceInput := anyMap(serverCaptured[recallResponseSourceInputKey])
	if anyToInt(sourceInput["candidate_count"], 0) != candidateCount ||
		anyToInt(sourceInput["captured_count"], 0) != recallResponseMaxSourceInputCapture ||
		anyToInt(sourceInput["omitted_count"], 0) != 1 || anyToBool(sourceInput["complete"]) ||
		!recallResponseValidDigest(anyToString(sourceInput["membership_digest"])) ||
		!recallResponseValidDigest(anyToString(sourceInput["captured_membership_digest"])) {
		t.Fatalf("server-owned source envelope did not retain exact overflow custody: %#v", sourceInput)
	}
	if rawRetrieval, _, err := s.executeRetrieval(context.Background(), http.Header{}, searchRequest, true); err != nil {
		t.Fatalf("raw retrieval privacy probe failed: %v", err)
	} else if _, present := rawRetrieval[recallResponseSourceInputKey]; present {
		t.Fatal("raw retrieval route exposed the compiler-only source envelope")
	}
}

func TestRecallResponseHistoricalCarrierFilteringKeepsFutureOutOfSupportAndAction(t *testing.T) {
	priorRef := "rtc_" + strings.Repeat("1", 24)
	futureRef := "rtc_" + strings.Repeat("2", 24)
	futureTool := "sha256:" + strings.Repeat("f", 64)
	rows := []any{
		map[string]any{
			"project": "contextlattice", "file": "notes/recall/prior.md", "source": "qdrant",
			"candidate_id": priorRef, "kind": "fact", "status": "current", "confidence": 0.91,
			"occurred_at": "2026-01-01T15:00:00Z", "summary": "historical prior row",
		},
		map[string]any{
			"project": "contextlattice", "file": "notes/recall/future.md", "source": "qdrant",
			"candidate_id": futureRef, "kind": "fact", "status": "current", "confidence": 0.99,
			"occurred_at": "2026-01-01T17:00:00Z", "summary": "future action row",
			"action_evidence": map[string]any{
				"tool_ref": futureTool,
				"parameter_bindings": []any{map[string]any{
					"parameter_ref": "sha256:" + strings.Repeat("e", 64),
					"value_state":   "resolved", "required": true,
				}},
			},
		},
	}
	pack := buildContextPackPayload("historical carrier filtering", map[string]any{"results": rows}, 1, 1)
	input := recallResponseTestInput(false)
	input["query"] = "what action was available at the historical boundary"
	input["retrieval_intent"] = "action"
	input["as_of"] = "2026-01-01T16:00:00Z"
	input["context_pack"] = pack
	input["source_coverage"] = map[string]any{"complete": true}
	response := composeRecallResponse(input)
	disclosure := recallResponseDisclosure(response)
	if got := len(contextPackAnyList(response["evidence"])); got != 1 ||
		anyToString(anyMap(contextPackAnyList(response["evidence"])[0])["ref_id"]) != priorRef {
		t.Fatalf("historical filtering did not retain exactly the prior supporting row: evidence=%#v", response["evidence"])
	}
	if refs := recallResponseDisclosureRefs(response, "exclusion_refs"); !refs[futureRef] {
		t.Fatalf("future carrier identity was not retained as an opaque temporal exclusion: disclosure=%#v", disclosure)
	}
	if strings.Contains(recallResponseCanonicalJSON(response), futureTool) {
		t.Fatalf("future carrier action witness crossed the historical boundary: response=%#v", response)
	}
	if anyToInt(anyMap(disclosure["source_counts"])["evidence"], 0) != 2 ||
		anyToInt(anyMap(disclosure["omitted_counts"])["source"], 0) != 1 ||
		anyToBool(disclosure["source_truncated"]) {
		t.Fatalf("historical completeness was not separated from temporal exclusion: disclosure=%#v", disclosure)
	}
	if snapshot := anyMap(pack["_recall_response_source_snapshot"]); !anyToBool(snapshot["complete"]) {
		t.Fatalf("historical fixture did not start from complete server custody: %#v", snapshot)
	}
	prepared, _ := recallResponsePrepareTemporalInput(input)
	snapshot := recallResponseSourceSnapshotForInput(prepared)
	receipt := anyMap(prepared[recallResponseTemporalPartitionKey])
	if !recallResponseSourceSnapshotValidForInput(prepared, snapshot) ||
		anyToInt(anyMap(receipt["source"])["original_count"], 0) != 2 ||
		anyToInt(anyMap(receipt["source"])["retained_count"], 0) != 1 ||
		anyToInt(anyMap(receipt["source"])["excluded_count"], 0) != 1 ||
		anyToString(receipt["receipt_digest"]) != recallResponseTemporalPartitionReceiptDigest(receipt) {
		t.Fatalf("historical source partition was not closed and recomputable: receipt=%#v snapshot=%#v", receipt, snapshot)
	}
}

func TestRecallResponseHistoricalTemporalPartitionReceiptRejectsTampering(t *testing.T) {
	rows := []any{
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("1", 24), "status": "current", "confidence": 0.9, "occurred_at": "2026-01-01T12:00:00Z", "summary": "retained one"},
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("2", 24), "status": "current", "confidence": 0.9, "occurred_at": "2026-01-01T13:00:00Z", "summary": "retained two"},
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("3", 24), "status": "current", "confidence": 0.9, "occurred_at": "2026-01-01T17:00:00Z", "summary": "excluded future source"},
	}
	graphRows := []any{
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("a", 24), "status": "current", "confidence": 0.8, "occurred_at": "2026-01-01T14:00:00Z", "summary": "retained graph"},
		map[string]any{"candidate_id": "rtc_" + strings.Repeat("b", 24), "status": "current", "confidence": 0.8, "occurred_at": "2026-01-01T18:00:00Z", "summary": "excluded future graph"},
	}
	pack := buildContextPackPayload("historical custody receipt", map[string]any{"results": rows}, 2, 2)
	pack["graph_neighbors"] = cloneJSONValue(graphRows)
	recallResponseRefreshSourceCarrier(pack, map[string]any{
		"status": "sampled", "signals": map[string]any{"candidate_count": 2, "hydration_failure_count": 0},
	})
	input := recallResponseTestInput(false)
	input["as_of"] = "2026-01-01T16:00:00Z"
	input["context_pack"] = pack
	input["source_coverage"] = map[string]any{"complete": true}
	prepared, _ := recallResponsePrepareTemporalInput(input)
	snapshot := recallResponseSourceSnapshotForInput(prepared)
	if !recallResponseSourceSnapshotValidForInput(prepared, snapshot) {
		t.Fatalf("legitimate historical source/graph partition was rejected: prepared=%#v snapshot=%#v", prepared, snapshot)
	}
	receipt := anyMap(prepared[recallResponseTemporalPartitionKey])
	sourcePartition := anyMap(receipt["source"])
	graphPartition := anyMap(receipt["graph"])
	if anyToInt(sourcePartition["original_count"], 0) != 3 || anyToInt(sourcePartition["retained_count"], 0) != 2 || anyToInt(sourcePartition["excluded_count"], 0) != 1 ||
		anyToInt(graphPartition["original_count"], 0) != 2 || anyToInt(graphPartition["retained_count"], 0) != 1 || anyToInt(graphPartition["excluded_count"], 0) != 1 ||
		anyToString(sourcePartition["original_membership_digest"]) != anyToString(snapshot["captured_membership_digest"]) ||
		anyToString(graphPartition["original_membership_digest"]) != anyToString(snapshot["graph_membership_digest"]) {
		t.Fatalf("source and graph custody were not exact and disjoint: receipt=%#v snapshot=%#v", receipt, snapshot)
	}

	restamp := func(candidate map[string]any) {
		candidateReceipt := anyMap(candidate[recallResponseTemporalPartitionKey])
		candidateReceipt["receipt_digest"] = recallResponseTemporalPartitionReceiptDigest(candidateReceipt)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "drop_retained_source", mutate: func(candidate map[string]any) {
			candidatePack := recallResponseCanonicalContextPack(candidate)
			candidatePack["_recall_response_source_rows"] = contextPackAnyList(candidatePack["_recall_response_source_rows"])[1:]
		}},
		{name: "replace_retained_source_content", mutate: func(candidate map[string]any) {
			candidatePack := recallResponseCanonicalContextPack(candidate)
			carrier := contextPackAnyList(candidatePack["_recall_response_source_rows"])
			anyMap(carrier[0])["summary"] = "replacement with same identity"
		}},
		{name: "reorder_retained_source", mutate: func(candidate map[string]any) {
			candidatePack := recallResponseCanonicalContextPack(candidate)
			carrier := contextPackAnyList(candidatePack["_recall_response_source_rows"])
			carrier[0], carrier[1] = carrier[1], carrier[0]
		}},
		{name: "duplicate_retained_source", mutate: func(candidate map[string]any) {
			candidatePack := recallResponseCanonicalContextPack(candidate)
			carrier := contextPackAnyList(candidatePack["_recall_response_source_rows"])
			carrier[1] = cloneJSONValue(carrier[0])
		}},
		{name: "wrong_as_of_with_recomputed_receipt", mutate: func(candidate map[string]any) {
			anyMap(candidate[recallResponseTemporalPartitionKey])["as_of"] = "2026-01-01T15:00:00Z"
			restamp(candidate)
		}},
		{name: "wrong_count_with_recomputed_receipt", mutate: func(candidate map[string]any) {
			candidateReceipt := anyMap(candidate[recallResponseTemporalPartitionKey])
			anyMap(candidateReceipt["source"])["retained_count"] = 1
			restamp(candidate)
		}},
		{name: "reordered_partition_entries_with_recomputed_receipt", mutate: func(candidate map[string]any) {
			candidateReceipt := anyMap(candidate[recallResponseTemporalPartitionKey])
			entries := contextPackAnyList(anyMap(candidateReceipt["source"])["entries"])
			entries[0], entries[1] = entries[1], entries[0]
			restamp(candidate)
		}},
		{name: "tampered_receipt_digest", mutate: func(candidate map[string]any) {
			anyMap(candidate[recallResponseTemporalPartitionKey])["receipt_digest"] = "sha256:" + strings.Repeat("f", 64)
		}},
		{name: "replace_retained_graph_content", mutate: func(candidate map[string]any) {
			candidatePack := recallResponseCanonicalContextPack(candidate)
			anyMap(contextPackAnyList(candidatePack[recallResponseGraphRowsKey])[0])["summary"] = "tampered graph"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := cloneJSONMap(prepared)
			tc.mutate(candidate)
			if recallResponseSourceSnapshotValidForInput(candidate, recallResponseSourceSnapshotForInput(candidate)) {
				t.Fatalf("tampered historical custody was accepted: %#v", candidate[recallResponseTemporalPartitionKey])
			}
		})
	}

	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("c", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("d", 64)
	response := composeRecallResponseWithPolicy(input, policy)
	if !anyToBool(anyMap(response["request_scope"])["source_bound"]) {
		t.Fatalf("valid historical custody did not retain source authority: %#v", response["request_scope"])
	}
	membership, ok := recallResponseContinuationMembership(input, response, policy)
	if !ok {
		t.Fatal("valid historical source/graph partition did not produce traversable membership")
	}
	hardExcluded := map[string]bool{}
	for _, raw := range membership.All {
		row := anyMap(raw)
		if anyToString(row["disposition"]) == "hard_excluded" {
			hardExcluded[anyToString(row["item_ref"])] = true
		}
	}
	if !hardExcluded["rtc_"+strings.Repeat("3", 24)] || !hardExcluded["rtc_"+strings.Repeat("b", 24)] {
		t.Fatalf("future source/graph identities were not retained as hard-excluded membership: %#v", membership.All)
	}
}

func TestRecallResponseSourceCarrierRetainsPreClipAndGraphMembership(t *testing.T) {
	rows := make([]any, 0, 20)
	for index := 0; index < 20; index++ {
		rows = append(rows, map[string]any{
			"candidate_id": fmt.Sprintf("rtc_%024x", index+1),
			"kind":         "fact",
			"status":       "current",
			"confidence":   0.75 + float64(index)/100,
			"source":       "qdrant",
			"summary":      fmt.Sprintf("safe source row %d", index+1),
		})
	}
	pack := buildContextPackPayload("twenty source rows", map[string]any{"results": rows}, 1, 1)
	if got := len(contextPackAnyList(pack["results"])); got != 1 {
		t.Fatalf("presentation limit was not retained: %d", got)
	}
	carrier := contextPackAnyList(pack["_recall_response_source_rows"])
	if got := len(carrier); got != 20 {
		t.Fatalf("pre-clip source carrier lost rows: %d", got)
	}
	snapshot := anyMap(pack["_recall_response_source_snapshot"])
	if anyToInt(snapshot["source_candidate_count"], 0) != 20 ||
		anyToInt(snapshot["source_captured_count"], 0) != 20 ||
		anyToString(snapshot["captured_membership_digest"]) != recallResponseSourceIdentityDigest(carrier) ||
		anyToString(snapshot["source_membership_digest"]) != recallResponseSourceIdentityDigest(carrier) ||
		!anyToBool(snapshot["complete"]) ||
		!recallResponseValidDigest(anyToString(snapshot["coverage_digest"])) ||
		!recallResponseValidDigest(anyToString(snapshot["source_membership_digest"])) {
		t.Fatalf("pre-clip source snapshot was not complete and digested: %#v", snapshot)
	}

	graphRow := map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("f", 24),
		"memory_id":    "mem_graph_target",
		"kind":         "graph_neighbor",
		"status":       "current",
		"confidence":   0.91,
		"source":       "memory_graph",
	}
	pack["graph_neighbors"] = []any{graphRow}
	recallResponseRefreshSourceCarrier(pack, map[string]any{
		"status": "sampled",
		"signals": map[string]any{
			"candidate_count":         1,
			"hydration_failure_count": 0,
		},
	})
	carrier = contextPackAnyList(pack["_recall_response_source_rows"])
	if got := len(carrier); got != 20 {
		t.Fatalf("graph rows contaminated retrieval source carrier: %d", got)
	}
	graphCarrier := contextPackAnyList(pack[recallResponseGraphRowsKey])
	if got := len(graphCarrier); got != 1 {
		t.Fatalf("graph carrier did not retain the graph membership separately: %d", got)
	}
	snapshot = anyMap(pack["_recall_response_source_snapshot"])
	if anyToInt(snapshot["source_candidate_count"], 0) != 20 ||
		anyToInt(snapshot["source_captured_count"], 0) != 20 ||
		anyToInt(snapshot["graph_candidate_count"], 0) != 1 ||
		anyToInt(snapshot["graph_captured_count"], 0) != 1 ||
		anyToInt(snapshot["graph_omitted_count"], -1) != 0 ||
		anyToInt(snapshot["source_omitted_count"], -1) != 0 ||
		anyToInt(snapshot["source_captured_count"], 0) > anyToInt(snapshot["source_candidate_count"], 0) ||
		anyToInt(snapshot["graph_captured_count"], 0) > anyToInt(snapshot["graph_candidate_count"], 0) ||
		anyToString(snapshot["graph_membership_digest"]) != recallResponseSourceIdentityDigest(graphCarrier) ||
		!anyToBool(snapshot["complete"]) ||
		!recallResponseValidDigest(anyToString(snapshot["coverage_digest"])) {
		t.Fatalf("graph/source coverage was not complete and digested: %#v", snapshot)
	}

	transport := projectContextPackForTransport(pack)
	if _, present := transport["_recall_response_source_rows"]; present {
		t.Fatal("private source carrier crossed the context-pack transport boundary")
	}
	if _, present := transport["_recall_response_source_snapshot"]; present {
		t.Fatal("private source snapshot crossed the context-pack transport boundary")
	}
	if _, present := transport[recallResponseGraphRowsKey]; present {
		t.Fatal("private graph carrier crossed the context-pack transport boundary")
	}

	input := recallResponseTestInput(false)
	input["context_pack"] = pack
	input["source_coverage"] = map[string]any{"complete": true}
	response := composeRecallResponse(input)
	disclosure := recallResponseDisclosure(response)
	if got := anyToInt(anyMap(disclosure["source_counts"])["evidence"], 0); got != 21 {
		t.Fatalf("response source accounting lost pre-clip/graph rows: %d", got)
	}
	if anyToBool(disclosure["source_truncated"]) {
		t.Fatalf("complete pre-clip/graph custody was marked incomplete: %#v", disclosure)
	}
	if anyToInt(anyMap(disclosure["omitted_counts"])["evidence"], 0) == 0 {
		t.Fatal("presentation clipping did not leave a traversable evidence omission")
	}
}

func TestRecallResponseIncompleteSourceCarrierIsExplicitlyBounded(t *testing.T) {
	allRows := make([]any, 0, recallResponseMaxSourceCapture+1)
	for index := 0; index <= recallResponseMaxSourceCapture; index++ {
		allRows = append(allRows, map[string]any{
			"candidate_id": fmt.Sprintf("rtc_%024x", index+1),
			"kind":         "fact",
			"status":       "current",
			"confidence":   0.8,
		})
	}
	pack := map[string]any{
		"ranked_evidence":              allRows[:1],
		"_recall_response_source_rows": allRows[:recallResponseMaxSourceCapture],
		"_recall_response_source_snapshot": recallResponseBuildSourceSnapshot(
			allRows, allRows[:recallResponseMaxSourceCapture], nil, false, nil,
		),
	}
	input := recallResponseTestInput(false)
	input["context_pack"] = pack
	input["source_coverage"] = map[string]any{"complete": true}
	response := composeRecallResponse(input)
	disclosure := recallResponseDisclosure(response)
	if !anyToBool(disclosure["source_truncated"]) || !anyToBool(disclosure["union_truncated"]) {
		t.Fatalf("incomplete source custody was not surfaced: %#v", disclosure)
	}
	if anyToInt(anyMap(disclosure["source_counts"])["evidence"], 0) != recallResponseMaxSourceCapture+1 ||
		anyToInt(anyMap(disclosure["omitted_counts"])["source"], 0) < 1 {
		t.Fatalf("incomplete source membership was not counted: %#v", disclosure)
	}
}

func TestRecallResponseInvalidSourceSnapshotCannotClaimCompleteness(t *testing.T) {
	pack := buildContextPackPayload("invalid source snapshot", map[string]any{
		"results": []any{map[string]any{
			"candidate_id": "rtc_" + strings.Repeat("9", 24),
			"kind":         "fact",
			"status":       "current",
			"confidence":   0.9,
		}},
	}, 1, 1)
	snapshot := anyMap(pack["_recall_response_source_snapshot"])
	snapshot["source_candidate_count"] = 100
	input := recallResponseTestInput(false)
	input["context_pack"] = pack
	input["source_coverage"] = map[string]any{"complete": true}
	response := composeRecallResponse(input)
	disclosure := recallResponseDisclosure(response)
	if !anyToBool(disclosure["source_truncated"]) || anyToInt(anyMap(disclosure["source_counts"])["evidence"], 0) != 1 {
		t.Fatalf("tampered source snapshot was trusted: %#v", disclosure)
	}
}

func TestRecallResponseUnreceiptedSourceCannotClaimBoundOrContinue(t *testing.T) {
	input := recallResponseTestInput(false)
	input["as_of"] = recallResponseLatestAsOf
	input["context_pack"] = map[string]any{"ranked_evidence": []any{map[string]any{
		"candidate_id": "rtc_" + strings.Repeat("a", 24),
		"kind":         "fact",
		"status":       "current",
		"confidence":   0.91,
		"source":       "server-evidence",
		"observed_at":  "2026-01-01T00:00:00Z",
	}}}
	policy := recallResponseProductionPolicyInput()
	policy.sourceBound = true
	policy.snapshotDigest = "sha256:" + strings.Repeat("b", 64)
	policy.receiptDigest = "sha256:" + strings.Repeat("c", 64)
	policy.evidenceBindings = recallResponseValidatedEvidenceBindings(input, "server_receipt", nil)

	response := composeRecallResponseWithPolicy(input, policy)
	if anyToBool(anyMap(response["request_scope"])["source_bound"]) {
		t.Fatal("formatted policy digests promoted an unreceipted production source")
	}
	s := newTestServer(t, "http://127.0.0.1:1")
	request := recallResponseRequestPayload(map[string]any{
		"query": input["query"], "project": input["project"], "topic_path": input["topic_path"],
		"agent_id": input["agent_id"], "retrieval_mode": input["retrieval_mode"],
	})
	s.installRecallResponseContinuation(response, input, request, policy, "agent-unreceipted", memoryRecallResponsePath)
	action := anyMap(recallResponseDisclosure(response)["continuation_action"])
	if anyToString(action["kind"]) != "owner_cursor_unavailable" {
		t.Fatalf("unreceipted production source issued a terminal or live cursor: %#v", action)
	}
}
