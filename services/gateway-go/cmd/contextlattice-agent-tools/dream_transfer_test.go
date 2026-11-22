package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestCLIDreamTransferRequiresExplicitScope(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_PROJECT", "ambient-project")
	for _, args := range [][]string{
		{"transfer", "--payload-file", "payload.json"},
		{"transfer", "--project", "destination"},
		{"transfer", "--project", "destination", "--payload-file", "--pretty"},
		{"transfer", "--project", "destination", "--payload-file", "payload.json", "--actor", "owner"},
		{"transfer", "--project", "destination", "--project=other", "--payload-file", "payload.json"},
		{"transfer", "--project", "destination", "--payload-file", "payload.json", "--payload-file", "other.json"},
		{"transfer", "--project", "destination::foreign", "--payload-file", "payload.json"},
		{"grant", "--project", "destination", "--payload-file", "payload.json"},
	} {
		if _, err := parseCLIDreamTransferArgs(args); err == nil {
			t.Errorf("accepted invalid args %q", args)
		}
	}
	args, err := parseCLIDreamTransferArgs([]string{"transfer", "--project=destination", "--payload-file", "payload.json", "--pretty"})
	if err != nil || args.project != "destination" || args.file != "payload.json" || args.help || !args.pretty {
		t.Fatalf("valid args: %+v, %v", args, err)
	}
}

func TestCLIDreamTransferPayloadBounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transfer.json")
	for _, tc := range []struct {
		name string
		json string
		ok   bool
	}{
		{"object", `{"schema_id":"dream_experience_transfer.v1","action":"freeze"}`, true},
		{"duplicate", `{"action":"freeze","action":"record_result"}`, false},
		{"case alias", `{"action":"freeze","Action":"record_result"}`, false},
		{"nested duplicate", `{"item":[{"value":"a","value":"b"}]}`, false},
		{"trailing", `{"action":"freeze"}{}`, false},
		{"null", "null", false},
		{"array", `[{}]`, false},
		{"invalid UTF-8", "{\"value\":\"\xff\"}", false},
		{"unpaired surrogate", `{"value":"\udc00"}`, false},
		{"oversized", `{"x":"` + strings.Repeat("x", cliDreamTransferMaxBytes) + `"}`, false},
		{"deep", `{"x":` + strings.Repeat("[", 25) + "0" + strings.Repeat("]", 25) + "}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.json), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCLIDreamTransferFile(path); (err == nil) != tc.ok {
				t.Fatalf("read error = %v, expected success = %v", err, tc.ok)
			}
		})
	}
	if _, err := readCLIDreamTransferFile(filepath.Dir(path)); err == nil {
		t.Fatal("directory accepted as a payload file")
	}
}

func TestCLIDreamTransferReceiptEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, project, destination, action, status, id, homeEffect string
		ok, persisted, valid                                       bool
	}{
		{"exact", "dest", "dest", "record_result", "confirmed", "dtx_exact", "unchanged", true, true, true},
		{"outer project", "other", "dest", "record_result", "confirmed", "dtx_exact", "unchanged", true, true, false},
		{"destination", "dest", "other", "record_result", "confirmed", "dtx_exact", "unchanged", true, true, false},
		{"wrong action", "dest", "dest", "freeze", "confirmed", "dtx_exact", "unchanged", true, true, false},
		{"prior gateway missing action", "dest", "dest", "", "confirmed", "dtx_exact", "unchanged", true, true, false},
		{"abstained", "dest", "dest", "record_result", "abstained", "dtx_exact", "unchanged", true, true, false},
		{"different decision", "dest", "dest", "record_result", "rejected", "dtx_exact", "unchanged", true, true, false},
		{"wrong id", "dest", "dest", "record_result", "confirmed", "dtx_another", "unchanged", true, true, false},
		{"source changed", "dest", "dest", "record_result", "confirmed", "dtx_exact", "changed", true, true, false},
		{"not ok", "dest", "dest", "record_result", "confirmed", "dtx_exact", "unchanged", false, true, false},
		{"not persisted", "dest", "dest", "record_result", "confirmed", "dtx_exact", "unchanged", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantID := "dtx_" + strings.Repeat("a", 32)
			gotID := wantID
			if tc.id != "dtx_exact" {
				gotID = "dtx_" + strings.Repeat("b", 32)
			}
			wire, err := json.Marshal(map[string]any{
				"project": tc.project, "mode": "dream_transfer", "ok": tc.ok, "persisted": tc.persisted,
				"dream_experience_transfer": map[string]any{
					"schema_id": cliDreamTransferSchema, "destination_project": tc.destination,
					"status": tc.status, "action": tc.action, "transfer_id": gotID, "home_project_effect": tc.homeEffect,
				},
			})
			var response map[string]json.RawMessage
			if err != nil || !decodeCLIDreamTransferJSON(wire, &response) {
				t.Fatalf("invalid fixture: %v", err)
			}
			if _, valid := cliDreamTransferReceiptEnvelope(response, "dest", "record_result", "confirmed", wantID); valid != tc.valid {
				t.Fatalf("receipt shape valid = %v, want %v", valid, tc.valid)
			}
		})
	}
}

func TestCLIDreamTransferTransportNeverFollowsRedirect(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != cliDreamTransferRoute || r.Method != http.MethodPost {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		var request struct {
			Project            string            `json:"project"`
			ExperienceTransfer map[string]string `json:"experience_transfer"`
			UseLLM             *bool             `json:"use_llm"`
			Persist            *bool             `json:"persist"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Project != "destination" ||
			request.ExperienceTransfer["action"] != "freeze" ||
			request.UseLLM == nil || *request.UseLLM || request.Persist == nil || *request.Persist {
			t.Errorf("wrong transfer envelope: %+v (%v)", request, err)
		}
		http.Redirect(w, r, "/unexpected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	c := &cli{baseURL: server.URL, client: server.Client()}
	if _, err := c.postCLIDreamTransfer("destination", map[string]json.RawMessage{"action": json.RawMessage(`"freeze"`)}, nil); err == nil {
		t.Fatal("redirect was treated as success")
	}
	if calls.Load() != 1 {
		t.Fatalf("requests = %d, want one", calls.Load())
	}
}

func TestCLIDreamTransferDispatchAndExactReceipt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only proof-file validation requires POSIX permissions")
	}
	dir := t.TempDir()
	path, proofPath := filepath.Join(dir, "transfer.json"), filepath.Join(dir, "proof")
	worker, proof := "synthetic-worker", strings.Repeat("b", 64)
	t.Setenv("CONTEXTLATTICE_WORKER_API_KEY", worker)
	t.Setenv(cliDreamTransferProofEnv, proofPath)
	if err := os.WriteFile(proofPath, []byte(proof+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := "dtx_" + strings.Repeat("a", 32)
	freeze := `{"schema_id":"dream_experience_transfer.v1","action":"freeze","grant_id":"grant-synthetic","source_passport_id":"passport-synthetic","experience_ref":"example","destination_project":"dest","mechanism":"batch","mismatch":"latency","prediction":"x\u003cy","falsifier":"no change","expires_at":"2030-01-01T00:00:00Z"}`
	result := `{"action":"record_result","result":"rejected","evidence_refs":["dest::control.json"],"transfer_id":"` + id + `"}`
	for _, tc := range []struct {
		name, payload, action, status string
		change                        func(map[string]any)
	}{
		{"freeze", freeze, "freeze", "frozen", nil},
		{"record result", result, "record_result", "rejected", nil},
		{"unbound", freeze, "freeze", "frozen", func(m map[string]any) { delete(m, "request_digest") }},
		{"wrong digest", result, "record_result", "rejected", func(m map[string]any) { m["request_digest"] = "sha256:" + strings.Repeat("f", 64) }},
		{"wrong write hash form", result, "record_result", "rejected", func(m map[string]any) { m["record_content_hash"] = "sha256:" + strings.Repeat("f", 64) }},
		{"projection not persisted", result, "record_result", "rejected", func(m map[string]any) { m["projection_persisted"] = false }},
		{"awaiting result is not receipt status", freeze, "freeze", "frozen", func(m map[string]any) { m["status"] = "awaiting_result" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.payload), 0600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var body map[string]any
				decoder := json.NewDecoder(r.Body)
				decoder.UseNumber()
				if err := decoder.Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				transfer, _ := body["experience_transfer"].(map[string]any)
				if r.Method != "POST" || r.URL.Path != "/memory/dream" || body["project"] != "dest" || transfer["action"] != tc.action ||
					r.Header.Get("X-Api-Key") != worker || r.Header.Get(cliDreamTransferProofHeader) != proof || transfer["schema_id"] != nil ||
					body["use_llm"] != false || body["persist"] != false {
					t.Error("unexpected transfer request")
				}
				canonical, err := json.Marshal(map[string]any{"project": body["project"], "experience_transfer": transfer})
				if err != nil {
					t.Errorf("marshal request: %v", err)
				}
				receipt := map[string]any{
					"schema_id": cliDreamTransferSchema, "destination_project": "dest", "action": tc.action, "status": tc.status,
					"transfer_id": id, "home_project_effect": "unchanged", "record_content_hash": strings.Repeat("c", 64),
					"request_digest": fmt.Sprintf("sha256:%x", sha256.Sum256(canonical)),
				}
				if tc.action == "record_result" {
					receipt["projection_persisted"] = true
				}
				if tc.change != nil {
					tc.change(receipt)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"ok": true, "persisted": true, "mode": "dream_transfer", "project": "dest",
					"dream_experience_transfer": receipt, "unexpected": "do-not-emit",
				})
			}))
			defer server.Close()
			var out bytes.Buffer
			c := &cli{baseURL: server.URL, apiKey: "synthetic-shared-key", client: server.Client(), stdout: &out}
			err := c.run([]string{"contextlattice", "dream", "transfer", "--project", "dest", "--payload-file", path})
			if calls.Load() != 1 || (err == nil) != (tc.change == nil) {
				t.Fatalf("requests = %d; run error = %v", calls.Load(), err)
			}
			if (err != nil && out.Len() != 0) || (err == nil && !strings.Contains(out.String(), id)) ||
				strings.Contains(out.String(), proof) || strings.Contains(out.String(), "do-not-emit") {
				t.Fatal("bad receipt output")
			}
		})
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	var out bytes.Buffer
	c := &cli{baseURL: server.URL, apiKey: "synthetic-shared-key", client: server.Client(), stdout: &out}
	args := []string{"contextlattice", "dream", "transfer", "--project", "dest", "--payload-file", path}
	for _, bad := range []string{"", filepath.Join(dir, "missing"), proofPath} {
		if bad == proofPath {
			if err := os.Chmod(proofPath, 0644); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv(cliDreamTransferProofEnv, bad)
		if err := c.run(args); err == nil || strings.Contains(err.Error(), proof) || calls.Load() != 0 || out.Len() != 0 {
			t.Fatal("unusable proof triggered a request or leaked a credential")
		}
	}
}
