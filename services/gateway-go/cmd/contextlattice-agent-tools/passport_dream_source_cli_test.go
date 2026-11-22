package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const cliDreamSourceTestKey = "synthetic-source-operator"
const cliDreamSourceTestProject = "source"
const cliDreamSourceTestMemoryID = "source::observations/lesson.json"
const cliDreamSourceTestEvent = "native-source-event"

func cliDreamSourceArgs(t *testing.T) ([]string, string) {
	t.Helper()
	dir := t.TempDir()
	payload := filepath.Join(dir, "source.json")
	raw := fmt.Sprintf(`{"project":%q,"transfer_observation":{"memory_id":%q,"event_id":%q,"content_hash":%q}}`,
		cliDreamSourceTestProject, cliDreamSourceTestMemoryID, cliDreamSourceTestEvent, strings.Repeat("a", 64))
	if err := os.WriteFile(payload, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(dir, "passport.json")
	return []string{"contextlattice", "passport-export", "--payload-file", payload, "--output", artifact, "--raw"}, artifact
}

func TestCLIDreamSourcePassportDoesNotFollow307(t *testing.T) {
	var first, second atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		second.Add(1)
	}))
	defer target.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/memory/context-passport/export" || r.Header.Get("X-Api-Key") != cliDreamSourceTestKey {
			t.Error("unexpected source export request")
		}
		http.Redirect(w, r, target.URL+"/unexpected", http.StatusTemporaryRedirect)
	}))
	defer gateway.Close()
	args, artifact := cliDreamSourceArgs(t)
	var out bytes.Buffer
	c := &cli{baseURL: gateway.URL, apiKey: cliDreamSourceTestKey, client: gateway.Client(), stdout: &out}
	err := c.run(args)
	if err == nil || first.Load() != 1 || second.Load() != 0 || out.Len() != 0 ||
		strings.Contains(err.Error(), cliDreamSourceTestKey) || strings.Contains(err.Error(), target.URL) {
		t.Fatalf("redirect was not rejected silently: first=%d second=%d", first.Load(), second.Load())
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatal("redirect created a passport artifact")
	}
}

func cliDreamSourceSignedFixture(t *testing.T, event string) map[string]any {
	t.Helper()
	buildJSON := func(value any) []byte {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	claim := map[string]any{
		"kind": "conditional_experience", "status": "confirmed", "experience_id": "exp:lesson",
		"mechanism": "stable key", "observed_result": "fewer duplicates", "evidence_refs": []any{"proof-one"},
	}
	p := cliDreamSourcePassport{cliDreamSourcePassportPayload: cliDreamSourcePassportPayload{
		SchemaID: cliDreamSourcePassportSchema, Version: 1, LineageID: "lineage_fixture", Project: cliDreamSourceTestProject, Revision: 1,
		Scope: map[string]any{"project": cliDreamSourceTestProject}, Objective: map[string]any{},
		Claims: []map[string]any{claim}, Evidence: []map[string]any{{
			"evidence_id": "proof-one", "kind": "source_memory_revision", "memory_id": cliDreamSourceTestMemoryID,
			"event_id": event, "content_hash": strings.Repeat("a", 64),
		}},
		Lineage: map[string]any{"source_schema": "dream_source_observation.v1"},
		Replay:  map[string]any{},
	}}
	signing := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	p.Issuer.InstanceID = "synthetic-source"
	p.Issuer.SigningPublicKey = base64.RawStdEncoding.EncodeToString(signing.Public().(ed25519.PublicKey))
	keyID := sha256.Sum256([]byte(p.Issuer.SigningPublicKey))
	p.Issuer.SigningKeyID = fmt.Sprintf("ed25519:%x", keyID[:12])
	unsigned := buildJSON(p.cliDreamSourcePassportPayload)
	hash := sha256.Sum256(unsigned)
	p.ContentDigest, p.PassportID = fmt.Sprintf("sha256:%x", hash), fmt.Sprintf("passport_%x", hash[:12])
	p.Signature.Algorithm, p.Signature.KeyID = "Ed25519", p.Issuer.SigningKeyID
	signed := []byte(fmt.Sprintf(`{"passport_id":%q,"content_digest":%q,"payload":%s}`, p.PassportID, p.ContentDigest, unsigned))
	p.Signature.Value = base64.RawStdEncoding.EncodeToString(ed25519.Sign(signing, signed))
	var passport map[string]any
	if err := json.Unmarshal(buildJSON(p), &passport); err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"ok": true, "recorded": true, "schema_id": cliDreamSourcePassportSchema, "passport": passport,
		"reconciliation": map[string]any{"recorded": true, "passport_id": p.PassportID, "lineage_id": p.LineageID, "revision": p.Revision},
		"transfer_source": map[string]any{
			"source_passport_id": p.PassportID, "experience_ref": "exp:lesson",
			"source_record_hash": fmt.Sprintf("sha256:%x", sha256.Sum256(buildJSON(claim))),
		},
	}
}

func TestCLIDreamSourcePassportRejectsForged200BeforeArtifact(t *testing.T) {
	for _, tc := range []struct {
		name   string
		event  string
		change func(map[string]any)
		ok     bool
	}{
		{"bound", cliDreamSourceTestEvent, nil, true},
		{"not recorded", cliDreamSourceTestEvent, func(m map[string]any) { m["recorded"] = false }, false},
		{"reconciliation not recorded", cliDreamSourceTestEvent, func(m map[string]any) {
			m["reconciliation"].(map[string]any)["recorded"] = false
		}, false},
		{"signed but different revision", "foreign-event", nil, false},
		{"forged passport", cliDreamSourceTestEvent, func(m map[string]any) {
			m["passport"].(map[string]any)["signature"].(map[string]any)["value"] = "forged"
		}, false},
		{"mismatched claim hash", cliDreamSourceTestEvent, func(m map[string]any) {
			m["transfer_source"].(map[string]any)["source_record_hash"] = "sha256:" + strings.Repeat("c", 64)
		}, false},
		{"mismatched passport id", cliDreamSourceTestEvent, func(m map[string]any) {
			m["transfer_source"].(map[string]any)["source_passport_id"] = "passport_foreign"
		}, false},
		{"mismatched project", cliDreamSourceTestEvent, func(m map[string]any) {
			m["passport"].(map[string]any)["project"] = "foreign"
		}, false},
		{"bare success", cliDreamSourceTestEvent, func(m map[string]any) {
			delete(m, "passport")
			delete(m, "transfer_source")
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := cliDreamSourceSignedFixture(t, tc.event)
			if tc.change != nil {
				tc.change(response)
				response["diagnostic"] = "do-not-disclose-response"
			}
			var calls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/memory/context-passport/export" || r.Header.Get("X-Api-Key") != cliDreamSourceTestKey {
					t.Error("unexpected request")
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer gateway.Close()
			args, artifact := cliDreamSourceArgs(t)
			var out bytes.Buffer
			c := &cli{baseURL: gateway.URL, apiKey: cliDreamSourceTestKey, client: gateway.Client(), stdout: &out, stderr: io.Discard}
			err := c.run(args)
			if (err == nil) != tc.ok || calls.Load() != 1 {
				t.Fatalf("unexpected source result: confirmed=%t requests=%d", err == nil, calls.Load())
			}
			if err != nil {
				if out.Len() != 0 || strings.Contains(err.Error(), cliDreamSourceTestKey) ||
					strings.Contains(err.Error(), "do-not-disclose-response") {
					t.Fatal("rejected response disclosed credentials or body")
				}
				if _, err := os.Stat(artifact); !os.IsNotExist(err) {
					t.Fatal("rejected response created an artifact")
				}
			} else if raw, err := os.ReadFile(artifact); err != nil || !bytes.Contains(raw, []byte("source_memory_revision")) {
				t.Fatal("validated response did not create its passport")
			}
		})
	}
}
