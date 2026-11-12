package main

import (
	"bytes"
	"encoding/json"
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
