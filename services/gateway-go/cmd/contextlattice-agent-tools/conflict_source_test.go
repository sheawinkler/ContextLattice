package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

func TestHostConflictChecksNewOriginalAndReportsOnlyItsDisposition(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("owner-only research grant admission is not enabled on Windows")
	}
	for _, tc := range []struct{ name, value, want, claim string }{
		{"original_a", "submitted", "supported_for_scope", "draft"}, {"original_b", "accepted", "supported_for_scope", "final"},
		{"withheld", "", "source_unavailable", ""}, {"unrecognized", "in-review", "still_contested", ""},
		{"revoked", "accepted", "not_authorized", ""}, {"stale_claim", "accepted", "case_unavailable_or_changed", ""},
		{"ambiguous", "accepted", "source_unavailable", ""}, {"changed_bytes", "accepted", "source_unavailable", ""},
		{"wrong_identity", "accepted", "source_unavailable", ""}, {"as_known_then", "accepted", "not_known_at_cutoff", ""},
		{"wrong_attempt", "accepted", "not_authorized", ""}, {"event_failed", "accepted", "handoff_unconfirmed", ""},
		{"bare_ack", "accepted", "handoff_unconfirmed", ""}, {"foreign_ack", "accepted", "handoff_unconfirmed", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "operator")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "grant.json")
			t.Setenv(grantEnv, path)
			t.Setenv("CONTEXTLATTICE_ATTEMPT_ID", "attempt1")
			k, _ := decisionresearch.New("studio", "design", "", decisionresearch.Claim{ID: "draft", Revision: 1}, decisionresearch.Claim{ID: "final", Revision: 2})
			reads, checks, events := 0, 0, 0
			var eventBody string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/memory/claims/query":
					checks++
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "case_ref": k.Ref, "current": tc.name != "stale_claim" || checks == 1})
				case "/v1/memory/get":
					reads++
					if r.Method != "GET" || r.URL.Query().Get("memory_id") != "studio::design/proof.json" {
						t.Error("unexpected source or verb")
					}
					if tc.name == "revoked" {
						if err := os.Remove(path); err != nil {
							t.Error(err)
						}
					}
					if tc.value == "" {
						http.NotFound(w, r)
						return
					}
					content := "{\"stage\":" + strconvQuote(tc.value) + ",\"effective_at\":\"2026-01-01T00:00:00Z\",\"note\":\"NEVER FORWARD ORIGINAL\"}"
					if tc.name == "ambiguous" {
						content = strings.Replace(content, "{\"stage\":", "{\"stage\":\"other\",\"stage\":", 1)
					}
					h := sha256.Sum256([]byte(content))
					digest := hex.EncodeToString(h[:])
					if tc.name == "changed_bytes" {
						digest = strings.Repeat("a", 64)
					}
					identityHash := sha256.Sum256([]byte("studio|design/proof.json|design|" + digest))
					oid := "obj_" + hex.EncodeToString(identityHash[:12])
					if tc.name == "wrong_identity" {
						oid = "obj2"
					}
					json.NewEncoder(w).Encode(map[string]any{"source": "go_memory_store", "memory_id": "studio::design/proof.json", "memory": map[string]any{"project": "studio", "topic_path": "design", "file": "design/proof.json", "event_id": "evt1", "object_id": oid, "content_hash": digest, "updated_at": "2026-01-02T00:00:00Z", "content": content}})
				case "/v1/agents/sessions/event":
					events++
					body, _ := io.ReadAll(r.Body)
					eventBody = string(body)
					if tc.name == "event_failed" {
						http.Error(w, "injected", 503)
						return
					}
					var event map[string]any
					if err := json.Unmarshal(body, &event); err != nil {
						t.Error(err)
					}
					event["id"] = "evt_one"
					if tc.name == "bare_ack" {
						event = nil
					}
					if tc.name == "foreign_ack" {
						event["agent_id"] = "other"
					}
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "event": event})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()
			b := decisionresearch.Binding{Gateway: ts.URL, Project: "studio", Topic: "design", Session: "s1", Task: "t1", Attempt: "attempt1", Agent: "hermes"}
			g := decisionresearch.Grant{Schema: "conflict_source_grant.v1", Binding: b, CaseRef: k.Ref, Source: decisionresearch.Source{Kind: "memory_json", MemoryID: "studio::design/proof.json", Field: "stage"}, Values: map[string]string{"draft": "submitted", "final": "accepted"}, Expires: time.Now().Add(time.Hour)}
			if tc.name == "wrong_attempt" {
				g.Binding.Attempt = "someone-else"
			}
			g.Issued = time.Now().Add(-time.Minute)
			if tc.name == "as_known_then" {
				cutoff := time.Now().Add(-time.Hour)
				g.KnownBy = &cutoff
			}
			doc, _ := json.Marshal(g)
			if err := os.WriteFile(path, doc, 0600); err != nil {
				t.Fatal(err)
			}
			if read, _, err := readConflictGrant(path); err != nil || (tc.name != "wrong_attempt" && !read.Allows(k, b, time.Now())) {
				t.Fatalf("fixture grant not admitted: %v", err)
			}
			packet := map[string]any{"ok": true, "schema_id": agentPacketContractID, "project": "studio", "topic_path": "design", "session_id": "s1", "task_id": "t1", "agent_id": "hermes", "decision_gate": map[string]any{"decision": "verify", "conflict_source_check": k}}
			var stderr bytes.Buffer
			client := cli{baseURL: ts.URL, client: ts.Client(), stdout: io.Discard, stderr: &stderr}
			call := func() { client.observeContextConflict(packet, "studio", "design", "s1", "t1", "hermes") }
			call()
			var result decisionresearch.Result
			if err := json.Unmarshal(stderr.Bytes(), &result); err != nil || result.Status != tc.want || result.Claim != tc.claim {
				t.Fatalf("result=%s, err=%v", stderr.String(), err)
			}
			if strings.Contains(eventBody+stderr.String(), "NEVER FORWARD ORIGINAL") {
				t.Fatal("original escaped the host")
			}
			switch tc.name {
			case "wrong_attempt":
				if checks+events+reads != 0 {
					t.Fatal("ungranted work ran")
				}
			case "revoked", "stale_claim":
				if events != 0 || reads != 1 {
					t.Fatal("stale or revoked outcome was delivered")
				}
			default:
				if events != 1 || reads != 1 || checks != 2 || !strings.Contains(eventBody, `"attempt_id":"attempt1"`) {
					t.Fatalf("reads=%d checks=%d events=%d", reads, checks, events)
				}
				stderr.Reset()
				call()
				if reads != 1 || events != 1 || !strings.Contains(stderr.String(), "already_checked_for_revision") {
					t.Fatal("repeated a completed/inconclusive or unconfirmed check")
				}
			}
		})
	}
}

func strconvQuote(v string) string { b, _ := json.Marshal(v); return string(b) }

func TestConflictResearchRequiresPrivateHostFileAndAttestedPacket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unattended POSIX qualification")
	}
	base := t.TempDir()
	dir := filepath.Join(base, "owner")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "grant")
	if err := os.WriteFile(path, []byte("grant"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := readConflictPrivateFile(path, 32)
	if err != nil || string(data) != "grant" {
		t.Fatalf("private original: %v", err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := readConflictPrivateFile(filepath.Join(alias, "grant"), 32); err == nil {
		t.Fatal("followed parent alias")
	}
	missing := filepath.Join(base, "missing")
	if _, err := readConflictPrivateFile(filepath.Join(missing, "grant"), 32); err == nil {
		t.Fatal("missing directory admitted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("a read created a directory")
	}
	if err := os.Link(path, filepath.Join(dir, "copy")); err != nil {
		t.Fatal(err)
	}
	if _, err := readConflictPrivateFile(path, 32); err == nil {
		t.Fatal("admitted multiply linked grant")
	}
	fake := filepath.Join(dir, "packet")
	if err := os.WriteFile(fake, []byte(`{"project":"studio","schema_id":"agent_packet.v1","ok":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := cli{stdout: io.Discard, stderr: io.Discard}
	if err := c.policyConflictResearch(fake, "studio", false); err == nil {
		t.Fatal("missing producer attestation accepted")
	}
}
