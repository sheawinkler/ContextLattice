package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCLIErrorOutputPreservesHTTPStatusWithoutBackendData(t *testing.T) {
	const secret = "private-diagnostic-canary"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"ok":false,"error":%q}`, secret)
	}))
	defer source.Close()
	c := newCLI(&bytes.Buffer{}, &bytes.Buffer{})
	c.baseURL = source.URL
	_, _, err := c.requestJSON(context.Background(), http.MethodPost, "/proof/"+secret+"?credential="+secret, map[string]any{}, 5)
	if err == nil {
		t.Fatal("expected HTTP failure")
	}
	var output bytes.Buffer
	writeCLIError(&output, fmt.Errorf("wrapped %s: %w", secret, err))
	var payload struct {
		OK       bool           `json:"ok"`
		Error    string         `json:"error"`
		Evidence map[string]any `json:"error_evidence"`
	}
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.OK || payload.Error != "ContextLattice gateway request failed" || payload.Evidence["status_code"] != float64(503) {
		t.Fatalf("HTTP evidence missing: %#v", payload)
	}
	if payload.Evidence["retryable"] != false || payload.Evidence["wrote_request"] != true || payload.Evidence["got_connection"] != true {
		t.Fatalf("delivery evidence changed: %#v", payload.Evidence)
	}
	if _, ok := payload.Evidence["path"]; ok || strings.Contains(output.String(), secret) || strings.Contains(output.String(), source.URL) {
		t.Fatalf("private request data leaked: %s", output.String())
	}
	if output.Len() > 1024 {
		t.Fatal("request failure output is not bounded")
	}
}

func TestCLIErrorOutputPreservesTransportClassification(t *testing.T) {
	const privateValue = "secret-method-or-cause"
	cause := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New(privateValue)}
	err := cliRequestFailure(privateValue, "/"+privateValue, 0, cause, false, false)
	var output bytes.Buffer
	writeCLIError(&output, err)
	var payload map[string]any
	if err := json.Unmarshal(output.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	evidence := payload["error_evidence"].(map[string]any)
	for _, field := range []string{"pre_delivery", "connection_failed", "retryable"} {
		if evidence[field] != true {
			t.Fatalf("lost %s: %#v", field, evidence)
		}
	}
	if evidence["method"] != "unknown" || evidence["status_code"] != float64(0) || strings.Contains(output.String(), privateValue) {
		t.Fatalf("unsafe transport evidence: %s", output.String())
	}
}

func TestCLIErrorOutputKeepsNonTransportUsageErrors(t *testing.T) {
	var output bytes.Buffer
	writeCLIError(&output, errors.New("invalid option"))
	if output.String() != "invalid option\n" {
		t.Fatalf("usage error changed: %q", output.String())
	}
}
