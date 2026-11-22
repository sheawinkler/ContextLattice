package main

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestHTTPWriteIndexesOnlyCompleteCallerFacts(t *testing.T) {
	raw := map[string]any{"agent_id": "hermes_agent", "tags": []string{"note"}, "caller_attribution": map[string]any{
		"source": "caller_reported", "harness": "hermes", "model_known": true, "model": "lab-2.0-maple", "reasoning_effort": "low", "execution_label": "forged",
	}}
	m := normalizeWriteMetadata(raw)
	if m.agentID != "hermes_agent" || !slices.Contains(m.tags, "execution:hermes-lab-maple-2.0-low") || slices.Contains(m.tags, "execution:forged") {
		t.Fatal(m)
	}
	delete(raw["caller_attribution"].(map[string]any), "reasoning_effort")
	if m := normalizeWriteMetadata(raw); !slices.Equal(m.tags, []string{"note"}) {
		t.Fatal(m)
	}
}

func TestProviderAttributionSurvivesHTTPWrite(t *testing.T) {
	s := newTestServer(t, "http://127.0.0.1:1")
	s.orchestratorAPIKey = ""
	s.memoryStore = newExactReadTestStore(t)
	r := httptest.NewRequest(http.MethodPost, "/memory/write", strings.NewReader(`{"projectName":"alpha","fileName":"notes/observed.md","content":"scope probe","agent_id":"hermes_agent","caller_attribution":{"source":"caller_reported","harness":"hermes","model_known":true,"model":"lab-2.0-maple","reasoning_effort":"low"}}`))
	w := httptest.NewRecorder()
	s.memoryWrite(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("write status: %d", w.Code)
	}
	entry, ok := s.memoryStore.currentEntry("alpha", "notes/observed.md")
	if !ok || entry.AgentID != "hermes_agent" || !slices.Contains(entry.Tags, "execution:hermes-lab-maple-2.0-low") {
		t.Fatalf("stored owner/tags: %v %q %v", ok, entry.AgentID, entry.Tags)
	}
}
