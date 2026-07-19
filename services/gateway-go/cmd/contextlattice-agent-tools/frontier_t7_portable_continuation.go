package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const frontierT7PortableContinuationMaxInputBytes = 8 << 20

const (
	frontierT7PortableContinuationGrantsPath    = "/memory/portable-continuation/grants"
	frontierT7PortableContinuationImportsPath   = "/memory/portable-continuation/imports"
	frontierT7PortableContinuationManifestsPath = "/memory/portable-continuation/manifests"
	frontierT7PortableContinuationStatusPath    = "/telemetry/portable-continuation"
)

type frontierT7PortableContinuationSpec struct {
	method          string
	path            string
	wireOperation   string
	acceptsEnvelope bool
}

var frontierT7PortableContinuationOperations = map[string]frontierT7PortableContinuationSpec{
	"grant-create":       {method: http.MethodPost, path: frontierT7PortableContinuationGrantsPath, wireOperation: "create"},
	"grant-authorize":    {method: http.MethodPost, path: frontierT7PortableContinuationGrantsPath, wireOperation: "authorize"},
	"grant-revoke":       {method: http.MethodPost, path: frontierT7PortableContinuationGrantsPath, wireOperation: "revoke"},
	"import-plan":        {method: http.MethodPost, path: frontierT7PortableContinuationImportsPath, wireOperation: "plan"},
	"import-commit":      {method: http.MethodPost, path: frontierT7PortableContinuationImportsPath, wireOperation: "commit"},
	"manifest-create":    {method: http.MethodPost, path: frontierT7PortableContinuationManifestsPath, wireOperation: "create"},
	"manifest-reconcile": {method: http.MethodPost, path: frontierT7PortableContinuationManifestsPath, wireOperation: "reconcile", acceptsEnvelope: true},
	"status":             {method: http.MethodGet, path: frontierT7PortableContinuationStatusPath},
}

func frontierT7PortableContinuationUsage() string {
	return "contextlattice_agent_tools portable-continuation {grant-create|grant-authorize|grant-revoke|import-plan|import-commit|manifest-create|manifest-reconcile} --payload-file request.json [manifest-reconcile: --envelope-file envelope.json] [--output result.json] [--pretty|--raw]\ncontextlattice_agent_tools portable-continuation status [--output result.json] [--pretty|--raw]"
}

func (c *cli) cmdPortableContinuation(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"payload-file":  "payload_file",
		"envelope-file": "envelope_file",
		"output":        "output",
	}), commonBoolFlags())
	if parsed.bool("help") || len(parsed.pos) == 0 {
		return c.emitUsage(frontierT7PortableContinuationUsage())
	}
	if len(parsed.pos) != 1 {
		return errors.New("portable-continuation accepts exactly one operation")
	}

	operation := strings.ToLower(strings.TrimSpace(parsed.pos[0]))
	spec, ok := frontierT7PortableContinuationOperations[operation]
	if !ok {
		return errors.New("unknown portable-continuation operation")
	}
	c.applyBaseURL(parsed)

	if spec.method == http.MethodGet {
		if parsed.has("payload_file") || parsed.has("envelope_file") {
			return errors.New("status does not accept payload or envelope files")
		}
		result, status, err := c.requestJSON(context.Background(), spec.method, spec.path, nil, parsed.float("timeout", 15))
		if err != nil {
			return frontierT7PortableContinuationRequestError(operation, status)
		}
		return c.emitPortableContinuationResult(operation, result, parsed)
	}

	payloadPath := parsed.string("payload_file", "")
	if payloadPath == "" {
		return errors.New("--payload-file is required for portable-continuation POST operations")
	}
	payloadValue, err := frontierT7ReadPortableContinuationJSON(payloadPath, "payload", true)
	if err != nil {
		return err
	}
	payload := payloadValue.(map[string]any)
	if err := frontierT7SetPortableContinuationOperation(payload, spec.wireOperation); err != nil {
		return err
	}

	envelopePath := parsed.string("envelope_file", "")
	if parsed.has("envelope_file") {
		if !spec.acceptsEnvelope {
			return errors.New("--envelope-file is only supported by manifest-reconcile")
		}
		if envelopePath == "" {
			return errors.New("--envelope-file is required when supplied")
		}
		envelope, err := frontierT7ReadPortableContinuationJSON(envelopePath, "envelope", true)
		if err != nil {
			return err
		}
		if existing, exists := payload["envelope"]; exists && existing != nil {
			return errors.New("payload envelope conflicts with --envelope-file")
		}
		payload["envelope"] = envelope
	}

	result, status, err := c.requestJSON(context.Background(), spec.method, spec.path, payload, parsed.float("timeout", 30))
	if err != nil {
		return frontierT7PortableContinuationRequestError(operation, status)
	}
	return c.emitPortableContinuationResult(operation, result, parsed)
}

func frontierT7SetPortableContinuationOperation(payload map[string]any, expected string) error {
	if supplied, exists := payload["operation"]; exists {
		value, ok := supplied.(string)
		if !ok || strings.ToLower(strings.TrimSpace(value)) != expected {
			return errors.New("payload operation conflicts with the selected portable-continuation operation")
		}
	}
	payload["operation"] = expected
	return nil
}

func frontierT7ReadPortableContinuationJSON(path, label string, objectOnly bool) (any, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("--%s file is required", label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("open portable-continuation %s file failed", label)
	}
	if info.Mode()&os.ModeSymlink != 0 || !privateConfigFileModeAllowed(info.Mode(), runtime.GOOS) {
		return nil, fmt.Errorf("portable-continuation %s file must be a regular owner-only file", label)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open portable-continuation %s file failed", label)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || openedInfo.Mode()&os.ModeSymlink != 0 || !privateConfigFileModeAllowed(openedInfo.Mode(), runtime.GOOS) {
		return nil, fmt.Errorf("portable-continuation %s file must be a regular owner-only file", label)
	}
	raw, err := io.ReadAll(io.LimitReader(file, frontierT7PortableContinuationMaxInputBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read portable-continuation %s file failed", label)
	}
	if len(raw) > frontierT7PortableContinuationMaxInputBytes {
		return nil, fmt.Errorf("portable-continuation %s file exceeds the input limit", label)
	}

	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode portable-continuation %s file failed", label)
	}
	if objectOnly {
		object, ok := value.(map[string]any)
		if !ok || object == nil {
			return nil, fmt.Errorf("portable-continuation %s file must contain a JSON object", label)
		}
		return object, nil
	}
	if value == nil {
		return nil, fmt.Errorf("portable-continuation %s file must contain JSON", label)
	}
	return value, nil
}

func frontierT7PortableContinuationRequestError(operation string, status int) error {
	if status > 0 {
		return fmt.Errorf("portable-continuation %s request failed with status %d", operation, status)
	}
	return fmt.Errorf("portable-continuation %s request failed", operation)
}

func (c *cli) emitPortableContinuationResult(operation string, result map[string]any, parsed parsedArgs) error {
	safe := frontierT7SanitizePortableContinuationOutput(result)
	if outputPath := parsed.string("output", ""); outputPath != "" {
		if err := writePrivateJSONArtifact(outputPath, safe); err != nil {
			return errors.New("write portable-continuation output failed")
		}
		safe = map[string]any{
			"ok":               true,
			"operation":        operation,
			"artifact_written": true,
			"artifact_kind":    "portable_continuation_response.v1",
		}
	}
	return c.emit(safe, parsed.bool("pretty") || !parsed.bool("raw"))
}

func frontierT7SanitizePortableContinuationOutput(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(typed))
		for key, nested := range typed {
			if frontierT7PortableContinuationSensitiveKey(key) {
				clean[key] = "[REDACTED]"
				continue
			}
			clean[key] = frontierT7SanitizePortableContinuationOutput(nested)
		}
		return clean
	case []any:
		clean := make([]any, len(typed))
		for index, nested := range typed {
			clean[index] = frontierT7SanitizePortableContinuationOutput(nested)
		}
		return clean
	case string:
		if frontierT7LooksLikeLocalPortableContinuationPath(typed) {
			return "[REDACTED_PATH]"
		}
		return typed
	default:
		return typed
	}
}

func frontierT7PortableContinuationSensitiveKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(key, "-", "_")))
	switch normalized {
	case "envelope", "plaintext", "api_key", "apikey", "access_token", "token", "secret", "password", "credential", "private_key", "authorization", "bearer", "runtime_license", "license_key", "entitlement_key", "claim_token":
		return true
	}
	for _, suffix := range []string{"_api_key", "_access_token", "_private_key", "_runtime_license", "_license_key", "_entitlement_key", "_claim_token"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func frontierT7LooksLikeLocalPortableContinuationPath(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "file://") {
		return true
	}
	if !filepath.IsAbs(value) {
		return false
	}
	for _, prefix := range []string{"/memory/", "/telemetry/", "/ops/", "/v1/"} {
		if strings.HasPrefix(value, prefix) {
			return false
		}
	}
	return true
}
