package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
)

const (
	frontierT10CLIAggregatePath          = "/memory/aggregate-signal"
	frontierT10CLIGovernancePath         = "/memory/aggregate-signal/governance"
	frontierT10CLIContributionContractID = "aggregate_contribution.v1"
	frontierT10CLIReportContractID       = "aggregate_report.v1"
	frontierT10CLIAccountantContractID   = "privacy_accountant.v1"
	frontierT10CLIGovernanceContractID   = "frontier_t10_aggregate_governance.v1"
	frontierT10CLIMaxInputBytes          = 2 * 1024 * 1024
)

func frontierT10AggregateSignalUsage() string {
	return "contextlattice aggregate-signal {preview|queue|report|status|opt-out|governance} " +
		"[--metric name] [--source manual|context_pack_quality|context_policy|context_mesh] " +
		"[--value number|--category bucket] [--cohort-window YYYY-Www] [--nonce value] " +
		"[--opt-in] [--epsilon number] [--delta number] [--expiry-weeks n] [--confirm] " +
		"[--payload-file request.json] [--output result.json] [--pretty|--raw]"
}

func frontierT10ReadAggregateInput(path string) (map[string]any, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("open Aggregate Signal payload file failed")
	}
	if info.Mode()&os.ModeSymlink != 0 || !frontierT7OwnerOnlyFileModeAllowed(info.Mode(), runtime.GOOS) {
		return nil, errors.New("Aggregate Signal payload file must be a regular owner-only file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open Aggregate Signal payload file failed")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || openedInfo.Mode()&os.ModeSymlink != 0 || !frontierT7OwnerOnlyFileModeAllowed(openedInfo.Mode(), runtime.GOOS) {
		return nil, errors.New("Aggregate Signal payload file must be a regular owner-only file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, frontierT10CLIMaxInputBytes+1))
	if err != nil {
		return nil, errors.New("read Aggregate Signal payload file failed")
	}
	if len(raw) > frontierT10CLIMaxInputBytes {
		return nil, errors.New("Aggregate Signal payload file exceeds the input limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	payload := map[string]any{}
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, errors.New("Aggregate Signal payload file must contain one JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("Aggregate Signal payload file must contain one JSON object")
	}
	return payload, nil
}

func frontierT10AggregateSignalContractPassed(result map[string]any, expected string) bool {
	if firstString(result["schema_id"]) != expected {
		return false
	}
	return firstString(asMap(asMap(result["format_contract"])["validation"])["status"]) == "passed"
}

func frontierT10AggregateExpectedContract(operation string) string {
	switch operation {
	case "preview", "queue":
		return frontierT10CLIContributionContractID
	case "report":
		return frontierT10CLIReportContractID
	case "status", "opt-out":
		return frontierT10CLIAccountantContractID
	case "governance":
		return frontierT10CLIGovernanceContractID
	default:
		return ""
	}
}

func (c *cli) cmdAggregateSignal(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		return c.emitUsage(frontierT10AggregateSignalUsage())
	}
	operation := strings.ToLower(strings.TrimSpace(args[0]))
	if frontierT10AggregateExpectedContract(operation) == "" {
		return errors.New("unknown Aggregate Signal operation")
	}
	parsed := parseArgs(args[1:], mergeStringFlags(commonStringFlags(), map[string]string{
		"metric": "metric", "source": "source", "value": "value", "category": "category",
		"cohort-window": "cohort_window", "window": "cohort_window", "nonce": "nonce",
		"contribution-nonce": "nonce", "epsilon": "epsilon", "delta": "delta",
		"expiry-weeks": "expiry_weeks", "payload-file": "payload_file", "output": "output",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{
		"opt-in": "opt_in", "confirm": "confirm",
	}))
	if parsed.bool("help") {
		return c.emitUsage(frontierT10AggregateSignalUsage())
	}
	if len(parsed.pos) > 0 {
		return errors.New("Aggregate Signal received unexpected positional arguments")
	}
	c.applyBaseURL(parsed)
	payload := map[string]any{}
	if path := parsed.string("payload_file", ""); path != "" {
		loaded, err := frontierT10ReadAggregateInput(path)
		if err != nil {
			return err
		}
		payload = loaded
	}
	path := frontierT10CLIAggregatePath
	if operation == "governance" {
		path = frontierT10CLIGovernancePath
		if strings.TrimSpace(firstString(payload["operation"])) == "" {
			return errors.New("Aggregate Signal governance payload requires operation")
		}
	} else {
		payload["operation"] = operation
	}
	for _, field := range []string{"metric", "source", "category", "cohort_window"} {
		if value := parsed.string(field, ""); value != "" {
			payload[field] = value
		}
	}
	if nonce := parsed.string("nonce", ""); nonce != "" {
		payload["contribution_nonce"] = nonce
	}
	for _, field := range []string{"value", "epsilon", "delta"} {
		if raw := parsed.string(field, ""); raw != "" {
			value, err := strconv.ParseFloat(raw, 64)
			if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
				return fmt.Errorf("--%s must be a finite number", strings.ReplaceAll(field, "_", "-"))
			}
			payload[field] = value
		}
	}
	if raw := parsed.string("expiry_weeks", ""); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return errors.New("--expiry-weeks must be an integer")
		}
		payload["expiry_weeks"] = value
	}
	if parsed.bool("opt_in") {
		payload["opt_in"] = true
	}
	if parsed.bool("confirm") {
		payload["confirm"] = true
	}
	result, status, err := c.requestJSON(context.Background(), http.MethodPost, path, payload, parsed.float("timeout", 20))
	if err != nil {
		if status > 0 {
			return fmt.Errorf("Aggregate Signal %s request failed with status %d", operation, status)
		}
		return fmt.Errorf("Aggregate Signal %s request failed", operation)
	}
	if !frontierT10AggregateSignalContractPassed(result, frontierT10AggregateExpectedContract(operation)) {
		return errors.New("Aggregate Signal response failed its public contract")
	}
	if output := parsed.string("output", ""); output != "" {
		if err := writePrivateJSONArtifact(output, result); err != nil {
			return errors.New("write Aggregate Signal owner-only artifact failed")
		}
		return c.emit(map[string]any{
			"ok": true, "operation": operation, "artifact_written": true,
			"artifact_kind": firstString(result["schema_id"]),
		}, parsed.bool("pretty") || !parsed.bool("raw"))
	}
	return c.emit(result, parsed.bool("pretty") || !parsed.bool("raw"))
}
