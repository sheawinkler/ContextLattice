package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWriteRequiresExplicitSuccessfulReceipt(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ok   bool
	}{
		{"confirmed", `{"ok":true}`, true},
		{"rejected", `{"ok":false}`, false},
		{"missing", `{}`, false},
		{"string", `{"ok":"true"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer gateway.Close()
			var out, errOut bytes.Buffer
			c := newCLI(&out, &errOut)
			c.baseURL = gateway.URL
			err := c.cmdWrite([]string{"--file", "notes/a.md", "--content", "body", "--session-id", "", "--agent-id", ""})
			if (err == nil) != tc.ok {
				t.Fatalf("receipt acceptance=%v expected=%v err=%v", err == nil, tc.ok, err)
			}
		})
	}
}

func TestWriteForwardsExplicitStructuredGraphClaims(t *testing.T) {
	var payload map[string]any
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/memory/write" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer gateway.Close()
	var out, errOut bytes.Buffer
	c := newCLI(&out, &errOut)
	c.baseURL = gateway.URL
	if err := c.cmdWrite([]string{
		"--file", "notes/decision.md", "--project", "alpha", "--content", "A sourced decision.",
		"--references-json", `["alpha::notes/source.md"]`,
		"--relations-json", `[{"target":"alpha::notes/evidence.md","relation":"supports"}]`,
	}); err != nil {
		t.Fatal(err)
	}
	refs, ok := payload["references"].([]any)
	if !ok || len(refs) != 1 || refs[0] != "alpha::notes/source.md" {
		t.Fatalf("reference claims were lost: %#v", payload)
	}
	relations, ok := payload["relations"].([]any)
	if !ok || len(relations) != 1 || asMap(relations[0])["relation"] != "supports" {
		t.Fatalf("typed relation claims were lost: %#v", payload)
	}
}

func TestWriteGraphClaimBoundsAndSyntaxFailBeforeNetwork(t *testing.T) {
	for _, raw := range []string{
		"", "null", `{}`, `[`, `[] {}`, `[] null`,
		`["` + strings.Repeat("x", 32768) + `"]`,
		`[` + strings.Repeat(`"alpha::notes/a.md",`, 32) + `"alpha::notes/a.md"]`,
	} {
		var out, errOut bytes.Buffer
		c := newCLI(&out, &errOut)
		c.baseURL = "http://127.0.0.1:1"
		err := c.cmdWrite([]string{
			"--file", "notes/a.md", "--content", "body", "--references-json", raw,
		})
		if err == nil || !strings.HasPrefix(err.Error(), "--references-json:") {
			t.Fatalf("invalid claims were not rejected locally: input_bytes=%d err=%v", len(raw), err)
		}
	}
	claims, err := parseWriteGraphClaims(`[]`)
	if err != nil || claims == nil || len(claims) != 0 {
		t.Fatalf("explicit empty claims must survive as an empty array: %#v %v", claims, err)
	}
}

func TestWriteDerivationPreflightBlocksUnsupportedGatewaysBeforeMutation(t *testing.T) {
	for _, capability := range []map[string]any{nil, {"version": 1, "available": false}, {"version": 2, "available": true}, {"version": "1", "available": true}, {"version": 1.5, "available": true}} {
		t.Run(firstString(capability["version"])+"-"+fmt.Sprint(capability["available"]), func(t *testing.T) {
			writes := 0
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					writes++
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"derivedMemory": capability})
			}))
			defer gateway.Close()
			var out, errOut bytes.Buffer
			c := newCLI(&out, &errOut)
			c.baseURL = gateway.URL
			err := c.cmdWrite([]string{"--file", "notes/derived.md", "--content", "derived", "--session-id", "", "--agent-id", "", "--relations-json", `[{"target_id":"alpha::notes/input.md","relation":"derived_from","target_event_id":"event","target_content_hash":"` + strings.Repeat("a", 64) + `"}]`})
			if err == nil || writes != 0 || out.Len() != 0 {
				t.Fatalf("unsupported gateway mutated state or printed success: writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestWriteDerivationRequiresReadyCapabilityAndPostWriteCustodyReceipt(t *testing.T) {
	valid := map[string]any{"version": 1, "registered": true, "scope": "same_project", "input_count": 1, "transaction_id": "ref_tx_" + strings.Repeat("a", 32)}
	for _, mode := range []string{"valid", "missing", "version_string", "count_fractional", "count_wrong", "registered_string", "transaction_invalid"} {
		t.Run(mode, func(t *testing.T) {
			receipt := map[string]any{}
			for key, value := range valid {
				receipt[key] = value
			}
			switch mode {
			case "missing":
				receipt = nil
			case "version_string":
				receipt["version"] = "1"
			case "count_fractional":
				receipt["input_count"] = 1.5
			case "count_wrong":
				receipt["input_count"] = 2
			case "registered_string":
				receipt["registered"] = "true"
			case "transaction_invalid":
				receipt["transaction_id"] = "ref_tx_" + strings.Repeat("z", 32)
			}
			writes, preflights := 0, 0
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet && r.URL.Path == "/ops/capabilities" {
					preflights++
					_ = json.NewEncoder(w).Encode(map[string]any{"derivedMemory": map[string]any{"version": 1, "available": true}})
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/memory/write" {
					t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
				}
				writes++
				var payload map[string]any
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if firstString(asMap(asList(payload["relations"])[0])["target_event_id"]) != "input-event" {
					t.Error("input pin lost")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "derivation_custody": receipt})
			}))
			defer gateway.Close()
			var out, errOut bytes.Buffer
			c := newCLI(&out, &errOut)
			c.baseURL = gateway.URL
			err := c.cmdWrite([]string{"--file", "notes/derived.md", "--content", "derived", "--session-id", "", "--agent-id", "", "--relations-json", `[{"target_id":"alpha::notes/input.md","relation":"DERIVED-FROM","target_event_id":"input-event","target_content_hash":"` + strings.Repeat("a", 64) + `"}]`})
			if (err == nil) != (mode == "valid") || writes != 1 || preflights != 1 {
				t.Fatalf("incorrect receipt confirmation or unsafe retry: mode=%s writes=%d preflights=%d err=%v", mode, writes, preflights, err)
			}
			if mode != "valid" && out.Len() != 0 {
				t.Fatal("unconfirmed protection printed a success payload")
			}
		})
	}
}
