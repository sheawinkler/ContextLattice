package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
)

// orchestrator-go intentionally owns no task state. Gateway Go's SQLite-WAL
// ledger is the sole live task authority; these historical endpoints remain
// fail-closed so older clients receive an exact migration target instead of
// silently creating a second in-memory queue.
type server struct{}

func newServer() *server { return &server{} }

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *server) taskAuthorityMoved(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusGone, map[string]any{
		"ok":                    false,
		"error":                 "legacy_orchestrator_task_authority_disabled",
		"authoritative_backend": "gateway-go-sqlite-wal",
		"authoritative_route":   "/agents/tasks",
	})
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"authoritative":         false,
		"mutations_enabled":     false,
		"authoritative_backend": "gateway-go-sqlite-wal",
		"totals":                map[string]any{"tasks": 0, "by_status": map[string]int{}},
	})
}

func (s *server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"service":               "orchestrator-go",
		"task_authority":        false,
		"authoritative_backend": "gateway-go-sqlite-wal",
	})
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/tasks/submit", s.taskAuthorityMoved)
	mux.HandleFunc("/v1/tasks/claim", s.taskAuthorityMoved)
	mux.HandleFunc("/v1/tasks/status", s.taskAuthorityMoved)
	mux.HandleFunc("/v1/tasks/retry", s.taskAuthorityMoved)
	mux.HandleFunc("/v1/tasks/metrics", s.metrics)
	return mux
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}
	srv := newServer()

	log.Printf("orchestrator-go listening on :%s task_authority=disabled", port)
	if err := http.ListenAndServe(":"+port, srv.handler()); err != nil {
		log.Fatal(err)
	}
}
