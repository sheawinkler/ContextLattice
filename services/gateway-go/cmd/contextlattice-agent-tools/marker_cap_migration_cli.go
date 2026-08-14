package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// cmdMarkerCapMigration is the local operator invocation for the same closed
// authenticated route exposed by gateway-go. It never edits marker storage
// directly: the native gateway remains the sole owner of plans, receipts,
// inventory, rollback, and exact-once custody.
func (c *cli) cmdMarkerCapMigration(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"operation": "operation", "new-max-marker-count": "new_max_marker_count", "new-max-marker-bytes": "new_max_marker_bytes",
		"operator-ref": "operator_ref", "reason": "reason", "plan-digest": "plan_digest", "capability": "capability",
		"principal": "principal", "workspace-id": "workspace_id", "expected-generation": "expected_generation",
	}), commonBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice marker-cap-migration --operation extend|rollback --capability <secret> --principal <id> --workspace-id <id> [--new-max-marker-count n --new-max-marker-bytes n --expected-generation n | --plan-digest sha256:<hex>] [--operator-ref id --reason text] [--pretty]")
	}
	c.applyBaseURL(parsed)
	operation := strings.ToLower(strings.TrimSpace(parsed.string("operation", "")))
	capability := parsed.string("capability", envString("CONTEXTLATTICE_EVALUATION_CLEANUP_MIGRATION_CAPABILITY", ""))
	principal := parsed.string("principal", envString("CONTEXTLATTICE_OPERATOR_PRINCIPAL", ""))
	workspace := parsed.string("workspace_id", envString("CONTEXTLATTICE_WORKSPACE_ID", ""))
	operatorRef := parsed.string("operator_ref", envString("CONTEXTLATTICE_OPERATOR_REF", principal))
	reason := parsed.string("reason", "operator-authorized marker index recovery")
	if operation != "extend" && operation != "rollback" {
		return errors.New("--operation must be extend or rollback")
	}
	if strings.TrimSpace(capability) == "" || strings.TrimSpace(principal) == "" || strings.TrimSpace(workspace) == "" || strings.TrimSpace(operatorRef) == "" || strings.TrimSpace(reason) == "" {
		return errors.New("capability, principal, workspace, operator reference, and reason are required")
	}
	payload := map[string]any{
		"operation": operation, "operator_ref": operatorRef, "reason": reason,
		"authorization": "gateway-go-operator", "native_owner": "gateway-go",
	}
	if operation == "extend" {
		count := parsed.int("new_max_marker_count", 0)
		bytesRaw := parsed.string("new_max_marker_bytes", "")
		bytesLimit, err := strconv.ParseInt(strings.TrimSpace(bytesRaw), 10, 64)
		if count <= 0 || err != nil || bytesLimit <= 0 {
			return errors.New("extend requires positive --new-max-marker-count and --new-max-marker-bytes")
		}
		payload["new_max_marker_count"] = count
		payload["new_max_marker_bytes"] = bytesLimit
		generation := parsed.string("expected_generation", "")
		if generation == "" {
			return errors.New("extend requires --expected-generation for compare-and-swap")
		}
		value, parseErr := strconv.ParseInt(generation, 10, 64)
		if parseErr != nil || value < 0 {
			return errors.New("--expected-generation must be a non-negative integer")
		}
		payload["expected_generation"] = value
	} else {
		planDigest := strings.TrimSpace(parsed.string("plan_digest", ""))
		if planDigest == "" {
			return errors.New("rollback requires --plan-digest")
		}
		payload["plan_digest"] = planDigest
	}
	headers := make(http.Header)
	headers.Set("X-ContextLattice-Evaluation-Cleanup-Capability", capability)
	headers.Set("X-ContextLattice-Operator-Principal", principal)
	headers.Set("X-ContextLattice-Workspace-ID", workspace)
	result, status, _, err := c.requestJSONWithHeaders(context.Background(), http.MethodPost, "/ops/evaluation-cleanup/marker-cap-migration", payload, clientTimeoutFor(parsed), headers)
	if err != nil {
		return err
	}
	if status < http.StatusOK || status >= http.StatusMultipleChoices {
		return fmt.Errorf("marker cap migration rejected: status=%d code=%s", status, strings.TrimSpace(fmt.Sprint(result["detail"])))
	}
	return c.emit(result, parsed.bool("pretty") || !parsed.bool("raw"))
}
