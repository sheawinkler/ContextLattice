package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func requestMemoryV1Get(t *testing.T, s *server, id, key string, wantStatus int) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/memory/get?memory_id="+url.QueryEscape(id), nil)
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	res := httptest.NewRecorder()
	s.memoryV1Get(res, req)
	if res.Code != wantStatus {
		t.Fatalf("GET %s: got %d, want %d: %s", id, res.Code, wantStatus, res.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode GET %s: %v", id, err)
	}
	return payload
}

func TestMemoryV1GetExactRevision(t *testing.T) {
	store := newExactReadTestStore(t)
	s := &server{memoryStore: store, orchestratorAPIKey: "test-key"}
	item := normalizedWrite{project: "alpha", fileName: "notes/revision.md", content: "first"}
	writeAndCheck := func() memoryStoreEntry {
		t.Helper()
		entry, _, err := store.put(item)
		if err != nil {
			t.Fatal(err)
		}
		row := requestMemoryV1Get(t, s, "alpha::notes//revision.md", "test-key", http.StatusOK)["memory"].(map[string]any)
		assertExactReadBytes(t, row, canonicalMemoryContent(item.content))
		if row["file"] != item.fileName || row["topic_path"] != entry.TopicPath ||
			row["object_id"] != entry.ObjectID || row["event_id"] != entry.EventID || row["event_recorded_at"] != entry.CreatedAt {
			t.Fatalf("GET did not return identity of committed entry %#v: %#v", entry, row)
		}
		return entry
	}

	initial := writeAndCheck()
	item.topicPath = "research/custom"
	retopiced := writeAndCheck()
	item.content = "second"
	rewritten := writeAndCheck()
	item.topicPath, item.content = "", "first"
	revisited := writeAndCheck()
	if initial.TopicPath != "notes" || retopiced.TopicPath != "research/custom" ||
		initial.ContentHash != retopiced.ContentHash || initial.ObjectID == retopiced.ObjectID ||
		rewritten.ContentHash == retopiced.ContentHash || revisited.ObjectID != initial.ObjectID ||
		revisited.EventID == initial.EventID {
		t.Fatalf("fixture did not exercise default, same-byte retopicing, rewrite and A-B-A")
	}

	requestMemoryV1Get(t, s, "alpha::notes/revision.md", "wrong", http.StatusUnauthorized)
	requestMemoryV1Get(t, s, "alpha::notes/missing.md", "test-key", http.StatusNotFound)
	reopened, err := newMemoryStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	s.memoryStore = reopened
	row := requestMemoryV1Get(t, s, "alpha/notes/revision.md", "test-key", http.StatusOK)["memory"].(map[string]any)
	assertExactReadBytes(t, row, "first\n")
	if row["topic_path"] != revisited.TopicPath || row["object_id"] != revisited.ObjectID || row["event_id"] != revisited.EventID {
		t.Fatalf("replay did not preserve the latest revision: %#v", row)
	}
}

func TestMemoryV1GetAuthorizedLegacyFallback(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/memory/get" ||
			r.URL.Query().Get("memory_id") != "alpha::notes/revision.md" || r.Header.Get("X-Api-Key") != "test-key" {
			t.Errorf("incorrect backend method, path, memory ID or auth")
		}
		writeJSON(w, http.StatusOK, map[string]any{"memory": map[string]any{
			"file_name": "notes/revision.md", "content": "legacy\n",
		}})
	}))
	defer backend.Close()
	s := &server{backendURL: backend.URL, client: backend.Client(), orchestratorAPIKey: "test-key"}

	requestMemoryV1Get(t, s, "alpha::notes/revision.md", "wrong", http.StatusUnauthorized)
	newNative := newExactReadTestStore(t)
	s.memoryStore = newNative
	requestMemoryV1Get(t, s, "alpha::notes/revision.md", "test-key", http.StatusNotFound)
	s.memoryStore = nil
	payload := requestMemoryV1Get(t, s, "alpha::notes/revision.md", "test-key", http.StatusOK)
	row := payload["memory"].(map[string]any)
	assertExactReadUnknown(t, row)
	if got := calls.Load(); got != 1 || row["content"] != "legacy\n" || row["file_name"] != "notes/revision.md" {
		t.Fatalf("expected one authorized backend read with the legacy response shape: calls=%d row=%#v", got, row)
	}
}
