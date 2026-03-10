package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

type server struct {
	backendURL string
	client     *http.Client
}

func envDurationSeconds(name string, fallback float64) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return time.Duration(fallback * float64(time.Second))
	}
	secs, err := strconv.ParseFloat(raw, 64)
	if err != nil || secs <= 0 {
		return time.Duration(fallback * float64(time.Second))
	}
	return time.Duration(secs * float64(time.Second))
}

func newServer() *server {
	backendURL := strings.TrimRight(strings.TrimSpace(os.Getenv("BACKEND_URL")), "/")
	if backendURL == "" {
		backendURL = "http://contextlattice-orchestrator:8075"
	}
	timeout := envDurationSeconds("GATEWAY_PROXY_TIMEOUT_SECS", 95)
	return &server{
		backendURL: backendURL,
		client:     &http.Client{Timeout: timeout},
	}
}

func isProxyPath(path string) bool {
	switch path {
	case "/v1/retrieval/query",
		"/v1/retrieval/query-with-grounding",
		"/v1/retrieval/batch-query",
		"/v1/retrieval/health",
		"/v1/memory/put",
		"/v1/memory/update",
		"/v1/memory/get",
		"/v1/memory/neighbors",
		"/v1/memory/batch-put",
		"/migration/runtime":
		return true
	default:
		return false
	}
}

func (s *server) copyHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Host") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request) {
	if !isProxyPath(r.URL.Path) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	targetURL := s.backendURL + r.URL.Path
	if query := r.URL.RawQuery; query != "" {
		targetURL += "?" + query
	}

	var bodyBytes []byte
	if r.Body != nil {
		defer r.Body.Close()
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "failed to read request body"})
			return
		}
		bodyBytes = raw
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "failed to build proxy request"})
		return
	}
	s.copyHeaders(req.Header, r.Header)
	if req.Header.Get("X-Forwarded-For") == "" {
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
	req.Header.Set("X-ContextLattice-Gateway", "gateway-go")

	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":      "backend unavailable",
			"detail":     err.Error(),
			"backendUrl": s.backendURL,
		})
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "failed to read backend response"})
		return
	}

	for key, values := range resp.Header {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
}

func (s *server) backendHealthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.backendURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

func (s *server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"service":       "gateway-go",
		"backendUrl":    s.backendURL,
		"backendHealth": s.backendHealthy(ctx),
	})
}

func (s *server) info(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"description": "ContextLattice gateway-go retrieval/memory proxy",
		"backendUrl":  s.backendURL,
	})
}

func buildMux(s *server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/info", s.info)
	// Retrieval + memory engine API (go-first ingress, python fallback backend).
	mux.HandleFunc("/v1/retrieval/query", s.proxy)
	mux.HandleFunc("/v1/retrieval/query-with-grounding", s.proxy)
	mux.HandleFunc("/v1/retrieval/batch-query", s.proxy)
	mux.HandleFunc("/v1/retrieval/health", s.proxy)
	mux.HandleFunc("/v1/memory/put", s.proxy)
	mux.HandleFunc("/v1/memory/update", s.proxy)
	mux.HandleFunc("/v1/memory/get", s.proxy)
	mux.HandleFunc("/v1/memory/neighbors", s.proxy)
	mux.HandleFunc("/v1/memory/batch-put", s.proxy)
	mux.HandleFunc("/migration/runtime", s.proxy)
	return mux
}

func main() {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8091"
	}
	srv := newServer()
	mux := buildMux(srv)
	log.Printf("gateway-go listening on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatal(err)
	}
}
