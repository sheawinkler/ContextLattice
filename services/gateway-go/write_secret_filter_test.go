package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func syntheticAssignedSecret() string {
	return "api_key=" + strings.Repeat("A1", 12)
}

func TestWriteSecretFilterModesAndFalsePositiveCorpus(t *testing.T) {
	secret := syntheticAssignedSecret()
	tests := []struct {
		name             string
		mode             string
		content          string
		raw              map[string]any
		wantError        bool
		wantFindings     bool
		wantContent      string
		wantRawRedaction bool
	}{
		{
			name:         "redacts assigned secret",
			mode:         "redact",
			content:      "credential follows: " + secret,
			wantFindings: true,
			wantContent:  "credential follows: " + writeSecretRedaction,
		},
		{
			name:         "blocks assigned secret",
			mode:         "block",
			content:      "credential follows: " + secret,
			wantError:    true,
			wantFindings: true,
		},
		{
			name:         "explicit allow preserves value",
			mode:         "allow",
			content:      "credential follows: " + secret,
			wantFindings: true,
			wantContent:  "credential follows: " + secret,
		},
		{
			name:        "ordinary security prose remains intact",
			mode:        "redact",
			content:     "Document password rotation, token budgets, API key naming, and secret-management policy.",
			wantContent: "Document password rotation, token budgets, API key naming, and secret-management policy.",
		},
		{
			name:    "non-secret numeric token metadata remains intact",
			mode:    "redact",
			content: "Token accounting is ordinary product telemetry.",
			raw: map[string]any{
				"token":        2048,
				"token_budget": 4096,
			},
			wantContent: "Token accounting is ordinary product telemetry.",
		},
		{
			name:    "recursive sensitive key is redacted",
			mode:    "redact",
			content: "Structured metadata.",
			raw: map[string]any{
				"metadata": map[string]any{"client_secret": strings.Repeat("z", 24)},
			},
			wantFindings:     true,
			wantContent:      "Structured metadata.",
			wantRawRedaction: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("SECRETS_STORAGE_MODE", test.mode)
			raw := map[string]any{
				"projectName": "contextlattice",
				"fileName":    "notes/security-filter.md",
				"content":     test.content,
				"topicPath":   "runbooks/security",
			}
			for key, value := range test.raw {
				raw[key] = value
			}
			secured, result, err := secureNormalizedWrite(normalizedWrite{
				project:   "contextlattice",
				fileName:  "notes/security-filter.md",
				content:   test.content,
				topicPath: "runbooks/security",
				raw:       raw,
			})
			if (err != nil) != test.wantError {
				t.Fatalf("error=%v wantError=%t result=%#v", err, test.wantError, result)
			}
			if (result.Findings > 0) != test.wantFindings {
				t.Fatalf("findings=%d wantFindings=%t", result.Findings, test.wantFindings)
			}
			if test.wantError {
				return
			}
			if secured.content != test.wantContent {
				t.Fatalf("content=%q want=%q", secured.content, test.wantContent)
			}
			if test.wantRawRedaction {
				metadata := anyMap(secured.raw["metadata"])
				if anyToString(metadata["client_secret"]) != writeSecretRedaction {
					t.Fatalf("sensitive structured value was not redacted: %#v", metadata)
				}
			}
		})
	}
}

func TestWriteIngressRedactsBeforeBackendFanout(t *testing.T) {
	t.Setenv("SECRETS_STORAGE_MODE", "redact")
	secret := syntheticAssignedSecret()
	var captured map[string]any
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode backend payload: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "warnings": []any{}})
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	requestBody := map[string]any{
		"projectName": "contextlattice",
		"fileName":    "notes/redaction-proof.md",
		"content":     "do not persist " + secret,
		"topicPath":   "runbooks/security",
		"metadata":    map[string]any{"access_token": strings.Repeat("q", 24)},
	}
	raw, _ := json.Marshal(requestBody)
	resp, err := http.Post(gateway.URL+"/memory/write", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("memory write: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	serialized, _ := json.Marshal(captured)
	if strings.Contains(string(serialized), secret) || strings.Contains(string(serialized), strings.Repeat("q", 24)) {
		t.Fatal("backend fanout received an unredacted value")
	}
	if !strings.Contains(anyToString(captured["content"]), writeSecretRedaction) {
		t.Fatalf("content was not redacted before fanout: %#v", captured)
	}
	if anyToInt(anyMap(response["secret_filter"])["redactions"], 0) < 2 {
		t.Fatalf("response omitted redaction evidence: %#v", response)
	}
	if s.writeSecretRedactions.Load() < 2 {
		t.Fatalf("redaction metric was not recorded: %d", s.writeSecretRedactions.Load())
	}
}

func TestWriteBatchBlockModeIsAtomic(t *testing.T) {
	t.Setenv("SECRETS_STORAGE_MODE", "block")
	var backendCalls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}))
	defer backend.Close()

	s := newTestServer(t, backend.URL)
	gateway := httptest.NewServer(buildMux(s))
	defer gateway.Close()

	requestBody := map[string]any{"items": []any{
		map[string]any{
			"projectName": "contextlattice",
			"fileName":    "notes/safe.md",
			"content":     "safe content",
			"topicPath":   "runbooks/security",
		},
		map[string]any{
			"projectName": "contextlattice",
			"fileName":    "notes/blocked.md",
			"content":     syntheticAssignedSecret(),
			"topicPath":   "runbooks/security",
		},
	}}
	raw, _ := json.Marshal(requestBody)
	resp, err := http.Post(gateway.URL+"/memory/write/batch", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("memory batch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d want=%d", resp.StatusCode, http.StatusUnprocessableEntity)
	}
	if backendCalls.Load() != 0 {
		t.Fatalf("blocked batch partially fanned out: calls=%d", backendCalls.Load())
	}
	var response map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if anyToString(response["error"]) != "potential_secret_detected" || anyToInt(response["index"], -1) != 1 {
		t.Fatalf("unexpected block response: %#v", response)
	}
	if s.writeSecretBlocked.Load() != 1 {
		t.Fatalf("blocked metric=%d", s.writeSecretBlocked.Load())
	}
}

func TestMemoryTelemetryExposesSecretFilterWithoutValues(t *testing.T) {
	t.Setenv("SECRETS_STORAGE_MODE", "redact")
	s := newTestServer(t, "")
	s.recordWriteSecretFilter(writeSecretFilterResult{
		Mode:       "redact",
		Findings:   3,
		Redactions: 2,
	}, true)
	payload := s.telemetryMemoryPayload()
	secretFilter := anyMap(payload["secretFilter"])
	if anyToString(secretFilter["mode"]) != "redact" ||
		anyToInt(secretFilter["findings"], 0) != 3 ||
		anyToInt(secretFilter["redactions"], 0) != 2 ||
		anyToInt(secretFilter["blocked"], 0) != 1 {
		t.Fatalf("unexpected secret filter telemetry: %#v", secretFilter)
	}
	serialized, err := json.Marshal(secretFilter)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), syntheticAssignedSecret()) {
		t.Fatal("secret filter telemetry exposed a matched value")
	}
}

func BenchmarkWriteSecretFilter(b *testing.B) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "safe", content: "Durable context with ordinary security policy prose and no credentials."},
		{name: "redact", content: "credential follows: " + syntheticAssignedSecret()},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			b.Setenv("SECRETS_STORAGE_MODE", "redact")
			item := normalizedWrite{
				project:   "contextlattice",
				fileName:  "notes/filter-benchmark.md",
				content:   test.content,
				topicPath: "runbooks/security",
				raw: map[string]any{
					"projectName": "contextlattice",
					"fileName":    "notes/filter-benchmark.md",
					"content":     test.content,
					"topicPath":   "runbooks/security",
					"metadata":    map[string]any{"source": "benchmark"},
				},
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, _, err := secureNormalizedWrite(item); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
