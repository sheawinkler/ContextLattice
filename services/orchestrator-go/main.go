package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Task struct {
	ID          string                 `json:"id"`
	Title       string                 `json:"title"`
	Project     string                 `json:"project,omitempty"`
	Agent       string                 `json:"agent,omitempty"`
	Priority    int                    `json:"priority"`
	Status      string                 `json:"status"`
	Payload     map[string]interface{} `json:"payload,omitempty"`
	RunAfter    string                 `json:"run_after,omitempty"`
	Attempts    int                    `json:"attempts"`
	MaxAttempts int                    `json:"max_attempts"`
	UpdatedAt   string                 `json:"updated_at"`
	CreatedAt   string                 `json:"created_at"`
}

type submitRequest struct {
	Title       string                 `json:"title"`
	Project     string                 `json:"project"`
	Agent       string                 `json:"agent"`
	Priority    int                    `json:"priority"`
	Payload     map[string]interface{} `json:"payload"`
	RunAfter    string                 `json:"run_after"`
	MaxAttempts int                    `json:"max_attempts"`
}

type claimRequest struct {
	WorkerID string `json:"worker_id"`
}

type statusRequest struct {
	TaskID   string                 `json:"task_id"`
	Status   string                 `json:"status"`
	Message  string                 `json:"message"`
	Metadata map[string]interface{} `json:"metadata"`
}

type retryRequest struct {
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
	Worker string `json:"worker"`
}

type server struct {
	mu        sync.Mutex
	tasks     map[string]*Task
	orderedID []string
}

func newServer() *server {
	return &server{
		tasks: make(map[string]*Task),
	}
}

func nowISO() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func newTaskID() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json", "detail": err.Error()})
		return false
	}
	return true
}

func (s *server) submitTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req submitRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "title is required"})
		return
	}
	if req.MaxAttempts <= 0 {
		req.MaxAttempts = 3
	}
	task := &Task{
		ID:          newTaskID(),
		Title:       req.Title,
		Project:     req.Project,
		Agent:       req.Agent,
		Priority:    req.Priority,
		Status:      "queued",
		Payload:     req.Payload,
		RunAfter:    req.RunAfter,
		Attempts:    0,
		MaxAttempts: req.MaxAttempts,
		CreatedAt:   nowISO(),
		UpdatedAt:   nowISO(),
	}

	s.mu.Lock()
	s.tasks[task.ID] = task
	s.orderedID = append(s.orderedID, task.ID)
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

func (s *server) claimTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req claimRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	worker := req.WorkerID
	if worker == "" {
		worker = "go-worker"
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	candidates := make([]*Task, 0, len(s.tasks))
	for _, task := range s.tasks {
		if task.Status == "queued" || task.Status == "approved" {
			candidates = append(candidates, task)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Priority == candidates[j].Priority {
			return candidates[i].CreatedAt < candidates[j].CreatedAt
		}
		return candidates[i].Priority > candidates[j].Priority
	})
	if len(candidates) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"task": nil})
		return
	}
	task := candidates[0]
	task.Status = "running"
	task.Attempts += 1
	task.UpdatedAt = nowISO()
	if task.Payload == nil {
		task.Payload = map[string]interface{}{}
	}
	task.Payload["claimed_by"] = worker
	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

func (s *server) updateTaskStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req statusRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.TaskID == "" || req.Status == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "task_id and status are required"})
		return
	}

	s.mu.Lock()
	task := s.tasks[req.TaskID]
	if task != nil {
		task.Status = req.Status
		task.UpdatedAt = nowISO()
	}
	s.mu.Unlock()

	if task == nil {
		writeJSON(w, http.StatusOK, map[string]any{"task": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

func (s *server) retryTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	var req retryRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	s.mu.Lock()
	task := s.tasks[req.TaskID]
	if task != nil {
		if task.Attempts >= task.MaxAttempts {
			task.Status = "failed"
		} else {
			task.Status = "queued"
		}
		task.UpdatedAt = nowISO()
	}
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"task": task})
}

func (s *server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	for _, task := range s.tasks {
		counts[task.Status] += 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true,
		"totals": map[string]any{
			"tasks":     len(s.tasks),
			"by_status": counts,
		},
	})
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "orchestrator-go"})
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8090"
	}
	mux := http.NewServeMux()
	srv := newServer()
	mux.HandleFunc("/healthz", srv.healthz)
	mux.HandleFunc("/v1/tasks/submit", srv.submitTask)
	mux.HandleFunc("/v1/tasks/claim", srv.claimTask)
	mux.HandleFunc("/v1/tasks/status", srv.updateTaskStatus)
	mux.HandleFunc("/v1/tasks/retry", srv.retryTask)
	mux.HandleFunc("/v1/tasks/metrics", srv.metrics)

	log.Printf("orchestrator-go listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
