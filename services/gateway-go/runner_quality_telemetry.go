package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	runnerQualitySampleSchemaID    = "runner_quality_sample.v1"
	runnerQualityTelemetrySchemaID = "contextlattice_runner_quality_telemetry.v1"
)

func runnerQualityLedgerPath() string {
	if explicit := strings.TrimSpace(os.Getenv("GO_RUNNER_QUALITY_LEDGER_PATH")); explicit != "" {
		return filepath.Clean(explicit)
	}
	if explicit := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_PATH")); explicit != "" {
		return filepath.Clean(explicit)
	}
	if explicit := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_RUNNER_QUALITY_LEDGER")); explicit != "" {
		return filepath.Clean(explicit)
	}
	root := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_ROOT"))
	if root == "" {
		root = strings.TrimSpace(os.Getenv("MEMORY_BANK_ROOT"))
	}
	if root != "" {
		return filepath.Clean(filepath.Join(root, "_contextlattice", "runner_quality_ledger.ndjson"))
	}
	return filepath.Clean(filepath.Join(".data", "orchestrator", "runner_quality_ledger.ndjson"))
}

func (s *server) telemetryRunnerQualityRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method_not_allowed"})
		return
	}
	limit := clampInt(envInt("GO_RUNNER_QUALITY_TELEMETRY_LIMIT", 500), 20, 20000)
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		limit = clampInt(anyToInt(raw, limit), 1, 20000)
	}
	taskClass := normalizeRunnerQualityTaskClass(r.URL.Query().Get("task_class"))
	rows, storage := readRunnerQualityLedgerRows(limit)
	payload := summarizeRunnerQualityRows(rows, anyToInt(storage["parse_errors"], 0), taskClass)
	payload["storage"] = storage
	writeJSON(w, http.StatusOK, payload)
}

func readRunnerQualityLedgerRows(limit int) ([]map[string]any, map[string]any) {
	path := runnerQualityLedgerPath()
	storage := map[string]any{
		"enabled":      envBool("GO_RUNNER_QUALITY_TELEMETRY_ENABLED", true),
		"durability":   "bounded_ndjson",
		"exists":       false,
		"bytes":        0,
		"loaded_rows":  0,
		"parse_errors": 0,
		"last_error":   "",
	}
	if !envBool("GO_RUNNER_QUALITY_TELEMETRY_ENABLED", true) || strings.TrimSpace(path) == "" {
		storage["enabled"] = false
		storage["durability"] = "disabled"
		return nil, storage
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, storage
		}
		storage["last_error"] = "stat_failed"
		return nil, storage
	}
	storage["exists"] = true
	storage["bytes"] = info.Size()
	file, err := os.Open(path)
	if err != nil {
		storage["last_error"] = "open_failed"
		return nil, storage
	}
	defer file.Close()
	rows := []map[string]any{}
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		row := map[string]any{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors++
			continue
		}
		if anyToString(row["schema_id"]) != runnerQualitySampleSchemaID {
			continue
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		storage["last_error"] = "scan_failed"
	}
	if limit > 0 && len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	storage["loaded_rows"] = len(rows)
	storage["parse_errors"] = parseErrors
	return rows, storage
}

func normalizeRunnerQualityTaskClass(value string) string {
	text := strings.TrimSpace(strings.ToLower(value))
	if text == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range text {
		valid := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '.' || r == ':' || r == '/' || r == '-'
		if valid {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-_.:/")
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func runnerQualityRowTaskClass(row map[string]any) string {
	if taskClass := normalizeRunnerQualityTaskClass(anyToString(row["task_class"])); taskClass != "" {
		return taskClass
	}
	metadata := anyMap(row["metadata"])
	if taskClass := normalizeRunnerQualityTaskClass(anyToString(metadata["task_class"])); taskClass != "" {
		return taskClass
	}
	return "general"
}

func summarizeRunnerQualityRows(rows []map[string]any, parseErrors int, taskClass string) map[string]any {
	allRows := rows
	if taskClass != "" {
		filtered := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if runnerQualityRowTaskClass(row) == taskClass {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	statusCounts := map[string]int{}
	taskClassCounts := map[string]int{}
	runnerRows := map[string][]map[string]any{}
	for _, row := range allRows {
		taskClassCounts[runnerQualityRowTaskClass(row)]++
	}
	for _, row := range rows {
		status := anyToString(row["status"])
		if status == "" {
			status = "unknown"
		}
		statusCounts[status]++
		runner := anyToString(row["runner"])
		if runner == "" {
			runner = "unknown"
		}
		runnerRows[runner] = append(runnerRows[runner], row)
	}
	byRunner := map[string]any{}
	runnerNames := make([]string, 0, len(runnerRows))
	for runner := range runnerRows {
		runnerNames = append(runnerNames, runner)
	}
	sort.Strings(runnerNames)
	for _, runner := range runnerNames {
		items := runnerRows[runner]
		total := len(items)
		successes := 0
		blocked := 0
		failed := 0
		durationTotal := 0.0
		qualityTotal := 0
		qualityCount := 0
		exactSaved := 0
		modeledAvoided := 0
		for _, row := range items {
			status := anyToString(row["status"])
			if anyToBool(row["ok"]) && status == "succeeded" {
				successes++
			} else if runnerQualityBlockedStatus(status) {
				blocked++
			} else {
				failed++
			}
			durationTotal += anyToFloat(row["duration_secs"])
			quality := anyMap(row["context_pack_quality"])
			qualityScore := anyToInt(quality["quality_score"], 0)
			if qualityScore > 0 {
				qualityTotal += qualityScore
				qualityCount++
			}
			exactSaved += anyToInt(quality["exact_prompt_tokens_saved"], 0)
			modeledAvoided += anyToInt(quality["modeled_inference_tokens_avoided"], 0)
		}
		avgQuality := 0.0
		if qualityCount > 0 {
			avgQuality = roundFloat(float64(qualityTotal)/float64(qualityCount), 1)
		}
		byRunner[runner] = map[string]any{
			"sample_count":                     total,
			"success_count":                    successes,
			"blocked_count":                    blocked,
			"failed_count":                     failed,
			"success_rate":                     runnerQualityRate(successes, total),
			"blocked_rate":                     runnerQualityRate(blocked, total),
			"failure_rate":                     runnerQualityRate(failed, total),
			"average_duration_secs":            runnerQualityRateFloat(durationTotal, total),
			"average_context_quality_score":    avgQuality,
			"exact_prompt_tokens_saved":        exactSaved,
			"modeled_inference_tokens_avoided": modeledAvoided,
		}
	}
	return map[string]any{
		"schema_id":          runnerQualityTelemetrySchemaID,
		"updated_at":         nowUTCISO(),
		"sample_count":       len(rows),
		"total_sample_count": len(allRows),
		"filtered":           taskClass != "",
		"task_class":         firstNonEmptyStrings(taskClass, "all"),
		"parse_errors":       parseErrors,
		"by_status":          statusCounts,
		"by_task_class":      taskClassCounts,
		"by_runner":          byRunner,
		"recommendations":    runnerQualityRecommendations(byRunner, len(rows), firstNonEmptyStrings(taskClass, "all")),
		"measurement_limit":  "Aggregates bounded runner_quality_sample.v1 rows only. No raw prompts, completions, stdout, stderr, or secrets are read into telemetry.",
		"source":             "/telemetry/runner-quality",
	}
}

func runnerQualityBlockedStatus(status string) bool {
	switch status {
	case "blocked", "missing_binary", "invalid_task", "timed_out", "skipped":
		return true
	default:
		return false
	}
}

func runnerQualityRate(n int, total int) float64 {
	if total <= 0 {
		return 0
	}
	return roundFloat(float64(n)/float64(total), 3)
}

func runnerQualityRateFloat(value float64, total int) float64 {
	if total <= 0 {
		return 0
	}
	return roundFloat(value/float64(total), 3)
}

const runnerQualityMinimumSamplesPerRunner = 5

func runnerQualityRecommendations(byRunner map[string]any, sampleCount int, taskClass string) map[string]any {
	type candidate struct {
		Payload map[string]any
		Score   float64
		Count   int
		Runner  string
	}
	candidates := []candidate{}
	for runner, raw := range byRunner {
		metrics := anyMap(raw)
		total := anyToInt(metrics["sample_count"], 0)
		if total <= 0 {
			continue
		}
		successRate := anyToFloat(metrics["success_rate"])
		blockedRate := anyToFloat(metrics["blocked_rate"])
		failureRate := anyToFloat(metrics["failure_rate"])
		quality := anyToFloat(metrics["average_context_quality_score"])
		duration := anyToFloat(metrics["average_duration_secs"])
		score := (successRate * 100.0) - (blockedRate * 24.0) - (failureRate * 38.0) + minFloat(quality, 100.0)*0.08
		if duration > 0 {
			score -= minFloat(duration, 300.0) * 0.015
		}
		payload := map[string]any{
			"runner":                  runner,
			"score":                   roundFloat(score, 3),
			"sample_count":            total,
			"success_rate":            successRate,
			"blocked_rate":            blockedRate,
			"failure_rate":            failureRate,
			"average_duration_secs":   duration,
			"recommendation_eligible": total >= runnerQualityMinimumSamplesPerRunner,
			"eligibility":             "eligible",
			"samples_remaining":       maxInt(0, runnerQualityMinimumSamplesPerRunner-total),
			"reason":                  runnerQualityReason(successRate, blockedRate, failureRate, total),
		}
		if total < runnerQualityMinimumSamplesPerRunner {
			payload["eligibility"] = "insufficient_samples"
		}
		candidates = append(candidates, candidate{Payload: payload, Score: score, Count: total, Runner: runner})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].Count != candidates[j].Count {
			return candidates[i].Count > candidates[j].Count
		}
		return candidates[i].Runner < candidates[j].Runner
	})
	items := make([]any, 0, minInt(len(candidates), 5))
	eligibleCount := 0
	topRunner := ""
	var topEligiblePayload map[string]any
	for _, candidate := range candidates {
		if candidate.Count >= runnerQualityMinimumSamplesPerRunner {
			eligibleCount++
			if topRunner == "" {
				topRunner = candidate.Runner
				topEligiblePayload = candidate.Payload
			}
		}
		if len(items) < 5 {
			items = append(items, candidate.Payload)
		}
	}
	if topEligiblePayload != nil {
		topVisible := false
		for _, raw := range items {
			if anyToString(anyMap(raw)["runner"]) == topRunner {
				topVisible = true
				break
			}
		}
		if !topVisible {
			if len(items) < 5 {
				items = append(items, topEligiblePayload)
			} else {
				items[len(items)-1] = topEligiblePayload
			}
		}
	}
	confidence := "high"
	if eligibleCount == 0 {
		confidence = "insufficient_samples"
	} else if eligibleCount < 2 {
		confidence = "low"
	} else if sampleCount < 30 {
		confidence = "medium"
	}
	return map[string]any{
		"mode":                       "advisor_only",
		"basis":                      "observed_bounded_runner_quality_samples",
		"task_class":                 taskClass,
		"minimum_samples_per_runner": runnerQualityMinimumSamplesPerRunner,
		"confidence":                 confidence,
		"top_runner":                 topRunner,
		"candidates":                 items,
		"guardrails": []any{
			"Never dispatch or mutate automatically from this summary.",
			"Compare only similar task_class samples before preferring a runner.",
			"missing_binary, auth, or blocked statuses are readiness signals, not proof that a runner is low quality.",
			"Use operator judgment and task constraints before selecting Pi, Droid, Codex, or another adapter.",
		},
	}
}

func runnerQualityReason(successRate float64, blockedRate float64, failureRate float64, total int) string {
	return strings.TrimSpace(
		strconvPercent(successRate) + " success, " +
			strconvPercent(blockedRate) + " blocked, " +
			strconvPercent(failureRate) + " failed across " + anyToString(total) + " comparable sample(s)",
	)
}

func strconvPercent(value float64) string {
	return strconv.Itoa(int(value*100+0.5)) + "%"
}
