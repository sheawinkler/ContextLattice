package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHostedDeploymentRequiresRetrievalReadinessContract(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, composePath := range []string{"docker-compose.yml", "docker-compose.lite.yml"} {
		raw, err := os.ReadFile(filepath.Join(root, composePath))
		if err != nil {
			t.Fatalf("read %s: %v", composePath, err)
		}
		gatewayBlock := composeServiceBlock(string(raw), "gateway-go")
		healthcheck, ok := activeConfigValue(gatewayBlock, "test")
		if !ok || !strings.Contains(healthcheck, "/readyz") || strings.Contains(healthcheck, "/healthz") {
			t.Fatalf("%s gateway healthcheck=%q, want active /readyz gate", composePath, healthcheck)
		}
		for key, want := range map[string]string{
			"ORCH_QDRANT_AUTO_CREATE_ON_STARTUP": "${ORCH_QDRANT_AUTO_CREATE_ON_STARTUP:-true}",
			"ORCH_QDRANT_EMBED_DIM":              "${ORCH_QDRANT_EMBED_DIM:-384}",
		} {
			if got, ok := activeConfigValue(gatewayBlock, key); !ok || got != want {
				t.Fatalf("%s gateway %s=%q, want %q", composePath, key, got, want)
			}
		}
	}
	for _, envPath := range []string{".env.example", filepath.Join("config", "env", "strict_runtime.env")} {
		envRaw, err := os.ReadFile(filepath.Join(root, envPath))
		if err != nil {
			t.Fatalf("read %s: %v", envPath, err)
		}
		for _, required := range []string{
			"ORCH_QDRANT_AUTO_CREATE_ON_STARTUP=true",
			"ORCH_QDRANT_EMBED_DIM=384",
		} {
			if !strings.Contains(string(envRaw), required) {
				t.Fatalf("%s is missing %q", envPath, required)
			}
		}
	}

	smokeRaw, err := os.ReadFile(filepath.Join(root, "scripts", "smoke_hosted_split.sh"))
	if err != nil {
		t.Fatalf("read hosted smoke script: %v", err)
	}
	smoke := string(smokeRaw)
	if !strings.Contains(smoke, "command -v jq") {
		t.Fatal("hosted smoke script must fail closed when jq is unavailable")
	}
	if strings.Contains(smoke, "contextlattice-smoke-body.$$") || !strings.Contains(smoke, "mktemp") {
		t.Fatal("hosted smoke script must use mktemp for its response body")
	}
	readyBlock := activeShellFunctionBlock(smoke, "check_ready_json")
	if !strings.Contains(readyBlock, `jq -e 'type == "object" and .ready == true'`) || strings.Contains(readyBlock, "grep") {
		t.Fatalf("hosted readiness function lacks an exact jq top-level predicate:\n%s", readyBlock)
	}
	canaryBlock := activeShellFunctionBlock(smoke, "check_retrieval_canary")
	for _, required := range []string{
		`"sources":["qdrant"]`,
		`and .degraded == false`,
		`and (.result_state == "ready" or .result_state == "empty")`,
		`and .retrieval_debug.source_owners.qdrant == "go_native"`,
		`and (.retrieval_debug.source_errors.qdrant? == null)`,
		`and ((.warnings? // []) | type == "array")`,
		`and ((.warnings? // []) | all(.[]; type == "string" and (startswith("qdrant collection dimension probe failed;") | not)))`,
	} {
		if !strings.Contains(canaryBlock, required) {
			t.Fatalf("hosted retrieval canary lacks active assertion %q:\n%s", required, canaryBlock)
		}
	}
	if strings.Contains(canaryBlock, "grep") {
		t.Fatal("hosted retrieval canary must parse JSON rather than substring-match it")
	}
}

func TestHostedSmokeExecutesRetrievalReadinessContract(t *testing.T) {
	for _, command := range []string{"bash", "curl", "jq"} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("hosted smoke dependency %s unavailable: %v", command, err)
		}
	}
	root := filepath.Join("..", "..")
	script := filepath.Join(root, "scripts", "smoke_hosted_split.sh")
	validReady := `{"ready":true}`
	validCanary := `{"degraded":false,"result_state":"empty","warnings":[],"retrieval_debug":{"source_owners":{"qdrant":"go_native"},"source_errors":{"qdrant":null}}}`
	tests := []struct {
		name        string
		readyBody   string
		canaryBody  string
		wantSuccess bool
	}{
		{name: "valid", readyBody: validReady, canaryBody: validCanary, wantSuccess: true},
		{
			name:        "authoritative stale-copy suppression warning",
			readyBody:   validReady,
			canaryBody:  strings.Replace(validCanary, `"warnings":[]`, `"warnings":["qdrant: qdrant authoritative memory state suppressed 5 fallback result(s)"]`, 1),
			wantSuccess: true,
		},
		{name: "nested readiness lookalike", readyBody: `{"nested":{"ready":true}}`, canaryBody: validCanary},
		{name: "malformed readiness", readyBody: `{`, canaryBody: validCanary},
		{name: "degraded retrieval", readyBody: validReady, canaryBody: strings.Replace(validCanary, `"degraded":false`, `"degraded":true`, 1)},
		{name: "fallback owner", readyBody: validReady, canaryBody: strings.Replace(validCanary, `"go_native"`, `"python_fallback"`, 1)},
		{name: "source error", readyBody: validReady, canaryBody: strings.Replace(validCanary, `"qdrant":null`, `"qdrant":"collection missing"`, 1)},
		{name: "dimension fallback warning", readyBody: validReady, canaryBody: strings.Replace(validCanary, `"warnings":[]`, `"warnings":["qdrant collection dimension probe failed; using default embed dim: status=404"]`, 1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/":
					_, _ = w.Write([]byte("ContextLattice"))
				case "/console":
					w.WriteHeader(http.StatusOK)
				case "/readyz":
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.readyBody))
				case "/memory/search":
					if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
						w.WriteHeader(http.StatusMethodNotAllowed)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(tc.canaryBody))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			cmd := exec.Command("bash", script, "--local", "--app-url", server.URL, "--api-url", server.URL)
			cmd.Env = append(os.Environ(), "TIMEOUT_SECS=2")
			output, err := cmd.CombinedOutput()
			if tc.wantSuccess && err != nil {
				t.Fatalf("smoke rejected valid fixtures: %v\n%s", err, output)
			}
			if !tc.wantSuccess && err == nil {
				t.Fatalf("smoke accepted invalid fixtures:\n%s", output)
			}
		})
	}
}

func composeServiceBlock(compose string, service string) string {
	marker := "  " + service + ":"
	lines := strings.Split(compose, "\n")
	inService := false
	block := make([]string, 0)
	for _, line := range lines {
		if line == marker {
			inService = true
			continue
		}
		if !inService {
			continue
		}
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			break
		}
		block = append(block, line)
	}
	return strings.Join(block, "\n")
}

func activeConfigValue(block string, key string) (string, bool) {
	prefix := key + ":"
	for _, line := range strings.Split(block, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix)), true
	}
	return "", false
}

func activeShellFunctionBlock(script string, name string) string {
	marker := name + "() {"
	lines := strings.Split(script, "\n")
	inFunction := false
	active := make([]string, 0)
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inFunction {
			if trimmed == marker {
				inFunction = true
			}
			continue
		}
		if trimmed == "}" {
			break
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		active = append(active, trimmed)
	}
	return strings.Join(active, "\n")
}
