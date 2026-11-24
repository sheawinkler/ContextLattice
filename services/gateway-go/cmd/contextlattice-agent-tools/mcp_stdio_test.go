package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const mcpTestInit = `{"jsonrpc":"2.0","method":"initialize","id":"start","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`
const mcpTestReady = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
const mcpTestCall = `{"jsonrpc":"2.0","method":"tools/call","id":3,"params":{"name":"contextlattice_context","arguments":{"query":"example"}}}`

func TestMCPBinaryHandshake(t *testing.T) {
	bin := os.Getenv("MCP_TEST_BINARY")
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "contextlattice")
		build := exec.Command("go", "build", "-p", "2", "-trimpath", "-o", bin, ".")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build: %v: %s", err, out)
		}
	}
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body mcpObject
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.Method != "POST" || r.URL.RequestURI() != "/memory/search" || body["project"] != "demo" || body["query"] != "example" || body["limit"] != float64(mcpRowLimit) || body["fetch_content"] != false || r.Header.Get("X-Api-Key") != "fixture" || r.Header.Get("X-ContextLattice-Plan") != "" {
			t.Error("unexpected gateway request")
		}
		io.WriteString(w, `{"project":"demo","results":[{"project":"other","summary":"wrong"},{"summary":"untagged"},{"Project":"demo","summary":"case alias"},{"project":{"name":"demo"},"summary":"nested tag"},{"project":"DEMO","summary":"case mismatch"},{"project":"demo","summary":"kept","file":"private"}],"debug":"hidden"}`)
	}))
	defer gateway.Close()
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "mcp", "--project", "demo")
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "CONTEXTLATTICE_CONFIG_HOME=" + home, "CONTEXTLATTICE_PROJECT=other", "CONTEXTLATTICE_PLAN=fixture", "CONTEXTLATTICE_CLI_OUTPUT=pretty", "CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL, "CONTEXTLATTICE_ORCHESTRATOR_API_KEY=fixture"}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(stdout)
	send := func(msg string) {
		t.Helper()
		if _, err := fmt.Fprintln(stdin, msg); err != nil {
			t.Fatal(err)
		}
	}
	read := func(id any) mcpObject {
		t.Helper()
		line, err := r.ReadBytes('\n')
		var rpc mcpObject
		if err != nil || json.Unmarshal(line, &rpc) != nil || rpc["jsonrpc"] != "2.0" || rpc["id"] != id {
			t.Fatal("missing or unexpected JSON-RPC response")
		}
		result, ok := rpc["result"].(map[string]any)
		if !ok {
			t.Fatal("expected success")
		}
		return result
	}
	send(mcpTestInit)
	init := read("start")
	if init["protocolVersion"] != "2025-06-18" || len(init["capabilities"].(mcpObject)) != 1 {
		t.Fatal("negotiation")
	}
	send(mcpTestReady)
	send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	read(float64(1))
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := read(float64(2))["tools"].([]any)
	if len(list) != 1 || list[0].(mcpObject)["name"] != mcpTool {
		t.Fatal("tool discovery")
	}
	send(mcpTestCall)
	result := read(float64(3))
	encoded, _ := json.Marshal(result)
	if string(encoded) != `{"content":[{"text":"kept","type":"text"}]}` {
		t.Fatal("row projection or project filter")
	}
	stdin.Close()
	if _, err = r.ReadByte(); err != io.EOF {
		t.Fatal("extra stdout")
	}
	if err = cmd.Wait(); err != nil || stderr.Len() != 0 || calls.Load() != 1 {
		t.Fatalf("exit=%v stderr bytes=%d calls=%d", err, stderr.Len(), calls.Load())
	}
	if entries, err := os.ReadDir(home); err != nil || len(entries) != 0 {
		t.Fatal("profile changed")
	}
}

func TestMCPRejectLaunchAndFraming(t *testing.T) {
	c := cli{stdin: strings.NewReader(""), stdout: io.Discard, baseURL: "http://localhost"}
	for _, args := range [][]string{nil, {"--project", ""}, {"--project", "-x"}, {"--project", "../x"}, {"--project", "a/b"}, {"--project", "a b"}, {"--project", strings.Repeat("x", 121)}, {"--project", "x", "--project", "y"}, {"--project=x"}, {"--project", "x", "--base-url", "example"}} {
		if c.cmdMCPStdio(args) == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	for _, origin := range []string{"http://remote.invalid", "https://name@localhost", "https://localhost?", "https://localhost/path"} {
		c.baseURL = origin
		if c.cmdMCPStdio([]string{"--project", "x"}) == nil {
			t.Fatal("invalid origin")
		}
	}
	c.baseURL = "http://localhost"
	for _, line := range []string{mcpTestInit, strings.Repeat(" ", mcpFrameLimit+1) + "\n" + mcpTestCall + "\n"} {
		var out bytes.Buffer
		c.stdin = strings.NewReader(line)
		c.stdout = &out
		if c.cmdMCPStdio([]string{"--project", "x"}) == nil || out.Len() != 0 {
			t.Fatal("did not fail closed on frame")
		}
	}
}

func TestMCPWireBoundaries(t *testing.T) {
	for _, v := range []string{"2024-11-05", "2025-03-26", "2025-06-18", mcpLatestHandshake, "unsupported"} {
		s := mcpSession{}
		got := s.frame([]byte(strings.Replace(mcpTestInit, "2025-06-18", v, 1))).(mcpObject)["result"].(mcpObject)["protocolVersion"]
		want := v
		if v == "unsupported" {
			want = mcpLatestHandshake
		}
		if got != want {
			t.Fatal("version", v)
		}
		if s.frame([]byte(mcpTestCall)).(mcpObject)["error"] == nil {
			t.Fatal("called without initialized notification")
		}
		if s.frame([]byte(mcpTestReady)) != nil || !s.ready {
			t.Fatal("initialized notification")
		}
		batch := s.frame([]byte(`[{"jsonrpc":"2.0","method":"ping","id":5},{"jsonrpc":"2.0","method":"notifications/cancelled"}]`))
		if v == "2025-03-26" {
			if len(batch.([]mcpObject)) != 1 {
				t.Fatal("batch reply")
			}
		} else if batch.(mcpObject)["error"] == nil {
			t.Fatal("unexpected batch support")
		}
	}
	// No gateway client: an accidentally accepted call would fail this test.
	s := mcpSession{version: mcpLatestHandshake, ready: true}
	for _, line := range []string{
		`{}`, `[]`, mcpTestInit, `{"jsonrpc":"2.0","id":null,"method":"ping"}`, `{"jsonrpc":"2.0","id":1.5,"method":"ping"}`,
		mcpTestCall + " {}", strings.Replace(mcpTestCall, `"query":"example"`, `"query":"x","query":"y"`, 1),
		strings.Replace(mcpTestCall, "example", `\ud800`, 1), strings.Replace(mcpTestCall, "example", "\xff", 1),
		strings.Replace(mcpTestCall, `"query":"example"`, `"Query":"x"`, 1),
		strings.Replace(mcpTestCall, `"query":"example"`, `"project":"other","query":"x"`, 1),
		strings.Replace(mcpTestCall, `"name":`, `"endpoint":"http://elsewhere","name":`, 1),
		strings.Replace(mcpTestCall, "contextlattice_context", "write", 1), strings.Replace(mcpTestCall, "tools/call", "write", 1),
		strings.Replace(mcpTestCall, "example", " ", 1), strings.Replace(mcpTestCall, "example", strings.Repeat("x", mcpQueryLimit+1), 1),
		strings.Replace(mcpTestCall, `"example"`, `{"x":"y"}`, 1), strings.Replace(mcpTestCall, "example", `\n`, 1),
	} {
		if reply := s.frame([]byte(line)).(mcpObject); reply["error"] == nil {
			t.Fatal("accepted invalid request")
		}
	}
	if s.frame([]byte(strings.Replace(mcpTestCall, `"id":3,`, "", 1))) != nil {
		t.Fatal("answered tool notification")
	}
}

func TestMCPGatewayBounds(t *testing.T) {
	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer sink.Close()
	for _, tc := range []struct {
		name, body string
		status     int
		fail       bool
	}{
		{"redirect", "", 307, true}, {"http_error", "private", 403, true}, {"too_large", strings.Repeat("x", mcpGatewayLimit+1), 200, true},
		{"duplicate_project", `{"results":[{"project":"other","project":"demo","summary":"private"}]}`, 200, true},
		{"wrong_shape", `{"results":null}`, 200, true}, {"empty", `{"results":[]}`, 200, false},
		{"declared_failure", `{"ok":false,"results":[{"project":"demo","summary":"private"}]}`, 200, true},
		{"contradictory_scope", `{"project":"other","results":[{"project":"demo","summary":"private"}]}`, 200, true},
		{"cap_results", `{"results":[` + strings.TrimSuffix(strings.Repeat(`{"project":"demo","summary":"`+strings.Repeat("\u754c", 500)+`"},`, 7), ",") + `]}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", sink.URL)
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer g.Close()
			s := mcpSession{client: &cli{client: g.Client(), baseURL: g.URL, apiKey: "fixture"}, project: "demo"}
			reply := s.search("example")
			if (reply["isError"] == true) != tc.fail {
				t.Fatal("wrong error status")
			}
			content := reply["content"].([]any)
			if tc.name == "cap_results" && len(content) != mcpRowLimit {
				t.Fatal("row limit")
			}
			for _, block := range content {
				v := block.(mcpObject)["text"].(string)
				if len(v) > mcpSummaryLimit || !utf8.ValidString(v) || strings.Contains(v, "private") {
					t.Fatal("invalid or unbounded projection")
				}
			}
		})
	}
	if hits.Load() != 0 {
		t.Fatal("followed redirect")
	}
}
