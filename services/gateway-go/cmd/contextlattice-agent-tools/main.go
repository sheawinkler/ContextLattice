package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "http://127.0.0.1:8075"

var nativeToolNames = map[string]string{
	"contextlattice_search":                          "search",
	"contextlattice_pack":                            "pack",
	"contextlattice_write":                           "write",
	"contextlattice_agent_session":                   "session",
	"contextlattice_agent_trace":                     "trace",
	"contextlattice_adopt":                           "adopt",
	"contextlattice_run_advisor":                     "run-advisor",
	"contextlattice_agent_runtime_doctor":            "runtime-doctor",
	"contextlattice_doctor":                          "runtime-doctor",
	"contextlattice_context_boundary":                "context-boundary",
	"contextlattice_strict_runtime_native_ownership": "strict-runtime-native-ownership",
	"contextlattice_agent_runtime_proof":             "runtime-proof",
	"contextlattice_agent_adoption_proof":            "adoption-proof",
	"contextlattice_agent_adapter":                   "adapter",
	"contextlattice_memory_topology":                 "memory-topology",
	"contextlattice_skills_index":                    "skills-index",
}

type cli struct {
	baseURL string
	apiKey  string
	client  *http.Client
	stdout  io.Writer
	stderr  io.Writer
}

type parsedArgs struct {
	values map[string]string
	bools  map[string]bool
	pos    []string
}

func main() {
	c := newCLI(os.Stdout, os.Stderr)
	if err := c.run(os.Args); err != nil {
		fmt.Fprintln(c.stderr, err.Error())
		os.Exit(1)
	}
}

func newCLI(stdout, stderr io.Writer) *cli {
	return &cli{
		baseURL: baseURLFromEnv(),
		apiKey:  apiKeyFromEnv(),
		client:  &http.Client{},
		stdout:  stdout,
		stderr:  stderr,
	}
}

func (c *cli) run(argv []string) error {
	if len(argv) == 0 {
		return errors.New("missing argv")
	}
	name := filepath.Base(argv[0])
	command := nativeToolNames[name]
	args := argv[1:]
	if command == "" {
		if len(args) == 0 {
			return c.usage()
		}
		command = strings.TrimSpace(args[0])
		args = args[1:]
	}
	switch command {
	case "search":
		return c.cmdSearch(args)
	case "write":
		return c.cmdWrite(args)
	case "pack":
		return c.cmdPack(args)
	case "session":
		return c.cmdSession(args)
	case "trace":
		return c.cmdTrace(args)
	case "run-advisor":
		return c.cmdRunAdvisor(args)
	case "runtime-doctor":
		return c.cmdRuntimeDoctor(args)
	case "context-boundary":
		return c.cmdContextBoundary(args)
	case "strict-runtime-native-ownership":
		return c.cmdStrictRuntimeNativeOwnership(args)
	case "runtime-proof":
		return c.cmdRuntimeProof(args)
	case "adoption-proof":
		return c.cmdAdoptionProof(args)
	case "adapter":
		return c.cmdAdapter(args)
	case "adopt":
		return c.cmdAdopt(args)
	case "memory-topology":
		return c.cmdMemoryTopology(args)
	case "skills-index":
		return c.cmdSkillsIndex(args)
	case "-h", "--help", "help":
		return c.usage()
	default:
		return fmt.Errorf("unknown contextlattice agent tool command %q", command)
	}
}

func (c *cli) usage() error {
	_, err := fmt.Fprintln(c.stdout, `ContextLattice native agent tools

Usage:
  contextlattice-agent-tools <command> [args]

Native commands:
  search                         lifecycle-aware memory search
  pack                           bounded context package
  write                          memory write/checkpoint
  session                        agent session lifecycle, rollup, context package, trace, runtime
  trace                          alias for session trace
  run-advisor                    render or emit run advisor
  context-boundary               audit /ops/context-boundary
  strict-runtime-native-ownership audit /ops/native-ownership
  runtime-doctor                 audit local native helper install and gateway health
  runtime-proof                  compact live runtime proof
  adoption-proof                 compact profile preflight proof matrix
  adapter                        profiles/bootstrap/status/context-pack/checkpoint/handoff/event/complete helpers
  adopt                          status/doctor/proof compatibility front door
  memory-topology                audit /telemetry/storage memory topology
  skills-index                   active Skills Index search/reindex helper

The same binary is intended to be symlinked or wrapped as contextlattice_search,
contextlattice_pack, contextlattice_write, contextlattice_agent_session, and
other contextlattice_* commands.`)
	return err
}

func (c *cli) cmdAdopt(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		return c.emitUsage("contextlattice_adopt {status|doctor|proof|profiles|install} [options]")
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "status":
		return c.adoptStatus(args)
	case "doctor":
		return c.cmdRuntimeDoctor(args)
	case "proof":
		return c.cmdAdoptionProof(args)
	case "profiles":
		return c.adapterProfiles(args)
	case "install":
		parsed := parseArgs(args, commonStringFlags(), commonBoolFlags())
		return c.emit(map[string]any{
			"ok":             true,
			"schema_id":      "contextlattice_agent_adoption_status.v1",
			"native_cli":     true,
			"install_status": "already_managed_by_scripts_install_global_agent_tools",
			"message":        "Run scripts/install_global_agent_tools.sh from the checkout to refresh Go-native global tools.",
		}, parsed.bool("pretty"))
	default:
		return fmt.Errorf("unknown contextlattice_adopt command %q", sub)
	}
}

func (c *cli) adoptStatus(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"global-home": "global_home"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	profiles := loadAgentProfiles()
	profileNames := make([]string, 0, len(profiles))
	for name := range profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	health, _, healthErr := c.requestJSON(context.Background(), http.MethodGet, "/health", nil, parsed.float("timeout", 10))
	boundary, _, boundaryErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/context-boundary", nil, parsed.float("timeout", 10))
	ownership, _, ownershipErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/native-ownership", nil, parsed.float("timeout", 10))
	ok := healthErr == nil && boundaryErr == nil && ownershipErr == nil && asBool(health["ok"]) && asBool(boundary["ok"]) && asBool(ownership["ok"])
	return c.emit(map[string]any{
		"ok":              ok,
		"schema_id":       "contextlattice_agent_adoption_status.v1",
		"native_cli":      true,
		"base_url":        c.baseURL,
		"profile_count":   len(profileNames),
		"profiles":        profileNames,
		"health":          health,
		"contextBoundary": map[string]any{"ok": boundary["ok"], "violationCount": boundary["violationCount"], "boundedSurfaceCount": boundary["boundedSurfaceCount"]},
		"nativeOwnership": map[string]any{"ok": ownership["ok"], "violationCount": ownership["violationCount"], "nativeRouteCount": ownership["nativeRouteCount"], "pythonHotPathOwnership": ownership["pythonHotPathOwnership"]},
	}, parsed.bool("pretty"))
}

func parseArgs(args []string, stringNames map[string]string, boolNames map[string]string) parsedArgs {
	out := parsedArgs{values: map[string]string{}, bools: map[string]bool{}, pos: []string{}}
	for i := 0; i < len(args); i++ {
		token := args[i]
		if token == "--" {
			out.pos = append(out.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(token, "-") || token == "-" {
			out.pos = append(out.pos, token)
			continue
		}
		name := strings.TrimLeft(token, "-")
		value := ""
		if idx := strings.Index(name, "="); idx >= 0 {
			value = name[idx+1:]
			name = name[:idx]
		}
		if canonical, ok := boolNames[name]; ok {
			out.bools[canonical] = true
			continue
		}
		canonical, ok := stringNames[name]
		if !ok {
			out.pos = append(out.pos, token)
			continue
		}
		if value == "" && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			value = args[i]
		}
		out.values[canonical] = value
	}
	return out
}

func commonStringFlags() map[string]string {
	return map[string]string{
		"project":    "project",
		"p":          "project",
		"topic-path": "topic_path",
		"t":          "topic_path",
		"mode":       "mode",
		"m":          "mode",
		"timeout":    "timeout",
		"base-url":   "base_url",
	}
}

func commonBoolFlags() map[string]string {
	return map[string]string{
		"h":      "help",
		"help":   "help",
		"pretty": "pretty",
		"raw":    "raw",
		"json":   "json",
	}
}

func mergeStringFlags(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func mergeBoolFlags(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func (a parsedArgs) string(name, fallback string) string {
	if value, ok := a.values[name]; ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func (a parsedArgs) int(name string, fallback int) int {
	if raw := strings.TrimSpace(a.values[name]); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil {
			return value
		}
	}
	return fallback
}

func (a parsedArgs) float(name string, fallback float64) float64 {
	if raw := strings.TrimSpace(a.values[name]); raw != "" {
		if value, err := strconv.ParseFloat(raw, 64); err == nil {
			return value
		}
	}
	return fallback
}

func (a parsedArgs) bool(name string) bool {
	return a.bools[name]
}

func (c *cli) applyBaseURL(args parsedArgs) {
	if raw := strings.TrimSpace(args.values["base_url"]); raw != "" {
		c.baseURL = strings.TrimRight(raw, "/")
	}
}

func baseURLFromEnv() string {
	for _, name := range []string{"CONTEXTLATTICE_ORCHESTRATOR_URL", "MEMMCP_ORCHESTRATOR_URL"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return defaultBaseURL
}

func apiKeyFromEnv() string {
	if value := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY")); value != "" {
		return value
	}
	for _, path := range []string{filepath.Join(repoRoot(), ".env"), ".env"} {
		if value := readEnvValue(path, "CONTEXTLATTICE_ORCHESTRATOR_API_KEY"); value != "" {
			return value
		}
	}
	return ""
}

func readEnvValue(path, key string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	prefix := key + "="
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, prefix)), `"'`)
	}
	return ""
}

func repoRoot() string {
	if value := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_REPO_ROOT")); value != "" {
		return value
	}
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return "."
}

func (c *cli) requestJSON(ctx context.Context, method, path string, payload any, timeout float64) (map[string]any, int, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(maxFloat(timeout, 1))*time.Second)
	defer cancel()
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(raw)
	}
	target := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		target = c.baseURL + path
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, 0, err
	}
	if payload != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	parsed := map[string]any{}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return nil, resp.StatusCode, fmt.Errorf("decode %s: %w: %s", path, err, string(raw[:minInt(len(raw), 1000)]))
		}
	}
	if resp.StatusCode >= 400 {
		return parsed, resp.StatusCode, fmt.Errorf("%s %s returned status=%d payload=%s", method, path, resp.StatusCode, compactJSON(parsed))
	}
	return parsed, resp.StatusCode, nil
}

func (c *cli) emit(payload any, pretty bool) error {
	var raw []byte
	var err error
	if pretty {
		raw, err = json.MarshalIndent(payload, "", "  ")
	} else {
		raw, err = json.Marshal(payload)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(c.stdout, string(raw))
	return err
}

func compactJSON(payload any) string {
	raw, _ := json.Marshal(payload)
	if len(raw) > 1200 {
		raw = raw[:1200]
	}
	return string(raw)
}

func (c *cli) cmdSearch(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"query":    "query",
		"q":        "query",
		"limit":    "limit",
		"l":        "limit",
		"agent-id": "agent_id",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{"wait": "wait"}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_search [--query text] [--project p] [--topic-path t] [--mode fast|balanced|deep] [--limit n] [--wait] [--raw]")
	}
	c.applyBaseURL(parsed)
	query := parsed.string("query", "")
	if query == "" && len(parsed.pos) > 0 {
		query = strings.Join(parsed.pos, " ")
	}
	if strings.TrimSpace(query) == "" {
		return errors.New("query is required")
	}
	mode := parsed.string("mode", envString("CONTEXTLATTICE_RETRIEVAL_MODE", "balanced"))
	payload := map[string]any{
		"query":                   query,
		"project":                 parsed.string("project", envString("CONTEXTLATTICE_PROJECT", "contextlattice")),
		"topic_path":              emptyToNil(parsed.string("topic_path", "")),
		"limit":                   maxInt(parsed.int("limit", 10), 1),
		"fetch_content":           false,
		"retrieval_mode":          mode,
		"include_grounding":       true,
		"include_retrieval_debug": false,
		"agent_id":                parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", "codex_gpt5"))),
	}
	if strings.EqualFold(mode, "deep") {
		payload["deep_async"] = true
	}
	initial, _, err := c.requestJSON(context.Background(), http.MethodPost, "/memory/search", payload, parsed.float("timeout", 30))
	if err != nil {
		return err
	}
	output := lifecycleSearchOutput(query, mode, initial)
	if parsed.bool("wait") {
		output = c.waitForSearchContinuation(output, parsed.float("timeout", 30))
	}
	return c.emit(output, !parsed.bool("raw"))
}

func lifecycleSearchOutput(query, mode string, initial map[string]any) map[string]any {
	continuation := asMap(initial["continuation_async"])
	token := firstString(continuation["token"], initial["token"], initial["job_id"])
	pollURL := firstString(continuation["poll_url"], initial["continuation_poll_url"], initial["job_poll_url"], initial["poll_url"])
	eventsURL := firstString(continuation["events_url"], initial["continuation_events_url"], initial["events_url"])
	return map[string]any{
		"ok":                 true,
		"query":              query,
		"project":            initial["project"],
		"retrieval_mode":     mode,
		"results":            asList(initial["results"]),
		"lifecycle":          lifecycleSummary(initial),
		"async":              token != "" || len(continuation) > 0 || asBool(initial["async"]),
		"continuation_async": continuation,
		"token":              token,
		"poll_url":           pollURL,
		"events_url":         eventsURL,
		"warnings":           asList(initial["warnings"]),
		"initial_response":   initial,
		"final_response":     nil,
	}
}

func (c *cli) waitForSearchContinuation(output map[string]any, timeout float64) map[string]any {
	pollURL := firstString(output["poll_url"])
	if pollURL == "" {
		return output
	}
	deadline := time.Now().Add(time.Duration(maxFloat(timeout, 5)) * time.Second)
	for time.Now().Before(deadline) {
		payload, _, err := c.requestJSON(context.Background(), http.MethodGet, ensureSlash(pollURL), nil, 5)
		if err == nil {
			output["final_response"] = payload
			status := strings.ToLower(firstString(payload["status"], asMap(payload["retrieval_progress"])["status"]))
			if status == "completed" || status == "succeeded" || status == "failed" {
				if results := asList(payload["results"]); len(results) > 0 {
					output["results"] = results
				}
				output["lifecycle"] = lifecycleSummary(payload)
				return output
			}
		}
		time.Sleep(1500 * time.Millisecond)
	}
	return output
}

func lifecycleSummary(payload map[string]any) map[string]any {
	lifecycle := asMap(payload["retrieval_lifecycle"])
	sources := asMap(lifecycle["sources"])
	sourceSummary := asMap(payload["source_summary"])
	return map[string]any{
		"status":          firstNonEmptyLower(lifecycle["status"], payload["status"], "unknown"),
		"result_state":    firstNonEmptyLower(lifecycle["result_state"], payload["result_state"]),
		"returned_now":    asList(sources["returned_now"]),
		"pending":         firstList(sources["pending"], sourceSummary["pending_sources"]),
		"failed":          firstList(sources["failed"], sourceSummary["failed_sources"]),
		"timed_out":       firstList(sources["timed_out"], sourceSummary["timed_out_sources"]),
		"budget_exceeded": firstList(sources["budget_exceeded"], sourceSummary["budget_exceeded_sources"]),
		"next_actions":    asList(lifecycle["next_actions"]),
	}
}

func (c *cli) cmdWrite(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"file":         "file",
		"f":            "file",
		"content":      "content",
		"c":            "content",
		"content-file": "content_file",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{"stdin": "stdin"}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_write --file notes/x.md (--content text|--content-file path|--stdin) [--project p] [--topic-path t]")
	}
	c.applyBaseURL(parsed)
	fileName := parsed.string("file", "")
	if fileName == "" {
		return errors.New("--file is required")
	}
	content, err := resolveContent(parsed)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"projectName": parsed.string("project", envString("CONTEXTLATTICE_PROJECT", "contextlattice")),
		"fileName":    fileName,
		"content":     content,
	}
	if topic := parsed.string("topic_path", ""); topic != "" {
		payload["topicPath"] = topic
	}
	result, _, err := c.requestJSON(context.Background(), http.MethodPost, "/memory/write", payload, parsed.float("timeout", 30))
	if err != nil {
		return err
	}
	return c.emit(result, !parsed.bool("raw"))
}

func resolveContent(parsed parsedArgs) (string, error) {
	if path := parsed.string("content_file", ""); path != "" {
		data, err := os.ReadFile(path)
		return string(data), err
	}
	if parsed.bool("stdin") {
		data, err := io.ReadAll(os.Stdin)
		return string(data), err
	}
	if content := parsed.string("content", ""); content != "" {
		return content, nil
	}
	return "", errors.New("content is required: use --content, --content-file, or --stdin")
}

func (c *cli) cmdPack(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"budget-chars": "budget_chars",
		"limit":        "limit",
		"max-facts":    "max_facts",
		"agent-id":     "agent_id",
		"session-id":   "session_id",
		"retries":      "retries",
		"retry-delay":  "retry_delay",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{
		"blocking":        "blocking",
		"nonblocking":     "nonblocking",
		"soft":            "soft",
		"strict":          "strict",
		"auto-session":    "auto_session",
		"no-auto-session": "no_auto_session",
	}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_pack '<task>' [--project p] [--topic-path t] [--mode balanced] [--pretty]")
	}
	c.applyBaseURL(parsed)
	query := strings.TrimSpace(strings.Join(parsed.pos, " "))
	if query == "" {
		return errors.New("query is required")
	}
	project := parsed.string("project", "contextlattice")
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	agentID := parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", "")))
	if sessionID == "" && !parsed.bool("no_auto_session") && !autoSessionDisabled() {
		sessionID = c.ensureSession(project, query, agentID, parsed.float("timeout", 30))
	}
	blocking := parsed.bool("blocking") && !parsed.bool("nonblocking")
	payload := map[string]any{
		"query":                     query,
		"project":                   project,
		"projectName":               project,
		"topic_path":                emptyToNil(parsed.string("topic_path", "")),
		"topicPath":                 emptyToNil(parsed.string("topic_path", "")),
		"retrieval_mode":            parsed.string("mode", "balanced"),
		"include_grounding":         true,
		"include_retrieval_debug":   true,
		"combined_sources":          true,
		"wait_for_slow_sources":     blocking,
		"sync_slow_sources":         blocking,
		"limit":                     parsed.int("limit", 12),
		"max_facts":                 parsed.int("max_facts", 24),
		"agent_id":                  emptyToNil(agentID),
		"session_id":                emptyToNil(sessionID),
		"native_cli_implementation": true,
	}
	raw, err := c.requestWithRetries("/memory/context-pack", payload, parsed.float("timeout", 30), parsed.int("retries", 2), parsed.float("retry_delay", 1))
	if err != nil {
		if parsed.bool("soft") {
			return c.emit(failurePack(query, parsed.int("budget_chars", 10000), err), !parsed.bool("raw"))
		}
		return err
	}
	out := normalizePackOutput(raw, query, parsed.int("budget_chars", 10000))
	return c.emit(out, parsed.bool("pretty") || !parsed.bool("raw"))
}

func (c *cli) requestWithRetries(path string, payload any, timeout float64, retries int, delay float64) (map[string]any, error) {
	var last error
	for attempt := 0; attempt <= maxInt(retries, 0); attempt++ {
		result, _, err := c.requestJSON(context.Background(), http.MethodPost, path, payload, timeout)
		if err == nil {
			return result, nil
		}
		last = err
		if attempt < retries {
			time.Sleep(time.Duration(delay*float64(attempt+1)) * time.Second)
		}
	}
	return nil, last
}

func (c *cli) ensureSession(project, objective, agentID string, timeout float64) string {
	payload := map[string]any{
		"project":   project,
		"objective": objective,
		"agent":     envString("CONTEXTLATTICE_AGENT", "agent-cli"),
		"agent_id":  agentID,
		"repo":      gitValue("config", "--get", "remote.origin.url"),
		"branch":    gitValue("branch", "--show-current"),
		"cwd":       currentWorkingDir(),
		"tags":      []string{"auto-session", "context-pack", "go-native-cli"},
		"metadata":  map[string]any{"tool": "contextlattice_pack"},
	}
	raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/start", payload, minFloat(timeout, 10))
	if err != nil {
		return ""
	}
	session := asMap(raw["session"])
	id := firstString(session["id"], raw["session_id"])
	if id != "" {
		writeSessionState(project, id, objective, agentID)
	}
	return id
}

func normalizePackOutput(raw map[string]any, query string, budget int) map[string]any {
	if _, ok := raw["task_summary"]; ok {
		return raw
	}
	if _, ok := raw["context_pack"]; !ok {
		return raw
	}
	raw["task_summary"] = query
	raw["context_budget_chars"] = budget
	raw["writeback_required"] = true
	return raw
}

func failurePack(query string, budget int, err error) map[string]any {
	return map[string]any{
		"ok":                   false,
		"task_summary":         query,
		"context_budget_chars": budget,
		"status":               "failed_after_retries",
		"structured_failure":   true,
		"error":                truncate(err.Error(), 1000),
		"context_pack": map[string]any{
			"query":              query,
			"facts":              []any{},
			"results":            []any{},
			"source_coverage":    map[string]any{"configured": []any{}, "returned": []any{}, "complete": false},
			"prompt_sections":    map[string]any{"objective": query, "task": query, "next_action": "Recover ContextLattice availability, then rerun context package compilation."},
			"ranked_evidence":    []any{},
			"context_compiler":   map[string]any{"schema_id": "contextlattice_context_compiler.v1", "strategy": "failure_payload"},
			"writeback_required": true,
		},
	}
}

func (c *cli) cmdSession(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		return c.emitUsage("contextlattice_agent_session {start|ensure|event|complete|fail|status|rollup|context-package|trace|list|runtime|watch} [options]")
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "start", "ensure":
		return c.sessionStart(sub, args)
	case "event", "complete", "fail":
		return c.sessionEvent(sub, args)
	case "status", "rollup", "context-package", "trace":
		return c.sessionGet(sub, args)
	case "list":
		return c.sessionList(args)
	case "runtime":
		return c.sessionRuntime(args)
	case "watch":
		return c.sessionWatch(args)
	default:
		return fmt.Errorf("unknown contextlattice_agent_session command %q", sub)
	}
}

func sessionCommonFlags() (map[string]string, map[string]string) {
	return mergeStringFlags(commonStringFlags(), map[string]string{
		"session-id":    "session_id",
		"agent":         "agent",
		"agent-id":      "agent_id",
		"mission":       "mission",
		"goal":          "goal",
		"repo":          "repo",
		"branch":        "branch",
		"cwd":           "cwd",
		"tag":           "tag",
		"metadata-json": "metadata_json",
		"summary":       "summary",
		"status":        "status",
		"limit":         "limit",
	}), commonBoolFlags()
}

func (c *cli) sessionStart(kind string, args []string) error {
	stringsFlags, bools := sessionCommonFlags()
	parsed := parseArgs(args, stringsFlags, mergeBoolFlags(bools, map[string]string{"persist": "persist", "strict": "strict"}))
	c.applyBaseURL(parsed)
	objective := strings.TrimSpace(strings.Join(parsed.pos, " "))
	if objective == "" {
		return errors.New("objective is required")
	}
	payload := map[string]any{
		"session_id": parsed.string("session_id", ""),
		"agent":      parsed.string("agent", envString("CONTEXTLATTICE_AGENT", "")),
		"agent_id":   parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", "")),
		"project":    parsed.string("project", "contextlattice"),
		"objective":  objective,
		"mission":    parsed.string("mission", ""),
		"goal":       parsed.string("goal", ""),
		"repo":       parsed.string("repo", gitValue("config", "--get", "remote.origin.url")),
		"branch":     parsed.string("branch", gitValue("branch", "--show-current")),
		"cwd":        parsed.string("cwd", currentWorkingDir()),
		"tags":       []string{"go-native-cli", kind},
		"metadata":   parseJSONObject(parsed.string("metadata_json", "")),
	}
	payload = dropEmpty(payload)
	raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/start", payload, parsed.float("timeout", 10))
	if err != nil {
		if kind == "ensure" && !parsed.bool("strict") {
			return c.emit(map[string]any{"ok": false, "session_id": "", "error": truncate(err.Error(), 500)}, parsed.bool("pretty"))
		}
		return err
	}
	session := asMap(raw["session"])
	if id := firstString(session["id"], raw["session_id"]); id != "" {
		raw["export"] = "export CONTEXTLATTICE_SESSION_ID=" + id
		writeSessionState(parsed.string("project", "contextlattice"), id, objective, parsed.string("agent_id", ""))
	}
	return c.emit(raw, parsed.bool("pretty"))
}

func (c *cli) sessionEvent(kind string, args []string) error {
	stringsFlags, bools := sessionCommonFlags()
	parsed := parseArgs(args, stringsFlags, bools)
	c.applyBaseURL(parsed)
	eventType := "session." + kind
	if kind == "event" {
		if len(parsed.pos) == 0 {
			return errors.New("event type is required")
		}
		eventType = parsed.pos[0]
	}
	status := parsed.string("status", "")
	if kind == "complete" {
		status = firstNonEmpty(status, "completed")
	}
	if kind == "fail" {
		status = firstNonEmpty(status, "failed")
	}
	payload := dropEmpty(map[string]any{
		"session_id": parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", "")),
		"agent":      parsed.string("agent", envString("CONTEXTLATTICE_AGENT", "")),
		"agent_id":   parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", "")),
		"project":    parsed.string("project", "contextlattice"),
		"type":       eventType,
		"summary":    parsed.string("summary", strings.Join(parsed.pos[1:], " ")),
		"status":     status,
		"metadata":   parseJSONObject(parsed.string("metadata_json", "")),
	})
	raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", payload, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	return c.emit(raw, parsed.bool("pretty"))
}

func (c *cli) sessionGet(kind string, args []string) error {
	stringsFlags, bools := sessionCommonFlags()
	parsed := parseArgs(args, stringsFlags, bools)
	c.applyBaseURL(parsed)
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	if sessionID == "" && len(parsed.pos) > 0 {
		sessionID = parsed.pos[0]
	}
	if sessionID == "" {
		return errors.New("session id is required")
	}
	suffix := ""
	if kind != "status" {
		suffix = "/" + kind
	}
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/v1/agents/sessions/"+url.PathEscape(sessionID)+suffix, nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	return c.emit(raw, parsed.bool("pretty"))
}

func (c *cli) sessionList(args []string) error {
	stringsFlags, bools := sessionCommonFlags()
	parsed := parseArgs(args, stringsFlags, bools)
	c.applyBaseURL(parsed)
	values := url.Values{}
	values.Set("limit", strconv.Itoa(parsed.int("limit", 20)))
	for _, key := range []string{"status", "project", "agent"} {
		if value := parsed.string(key, ""); value != "" {
			values.Set(key, value)
		}
	}
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/v1/agents/sessions?"+values.Encode(), nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	return c.emit(raw, parsed.bool("pretty"))
}

func (c *cli) sessionRuntime(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"limit": "limit"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/telemetry/agents/runtime?limit="+strconv.Itoa(parsed.int("limit", 16)), nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	return c.emit(raw, parsed.bool("pretty"))
}

func (c *cli) sessionWatch(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"session-id":         "session_id",
		"continuation-token": "continuation_token",
		"interval":           "interval",
		"max-seconds":        "max_seconds",
		"history":            "history",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{"once": "once", "jsonl": "jsonl", "all-events": "all_events"}))
	c.applyBaseURL(parsed)
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	token := parsed.string("continuation_token", "")
	if sessionID == "" && token == "" {
		return errors.New("session id or --continuation-token is required")
	}
	if token != "" {
		raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/memory/search/continuations/"+url.PathEscape(token)+"?include_result=false", nil, parsed.float("timeout", 10))
		if err != nil {
			return err
		}
		if parsed.bool("jsonl") {
			return c.emit(raw, false)
		}
		return c.emit(raw, parsed.bool("pretty"))
	}
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/v1/agents/sessions/"+url.PathEscape(sessionID)+"/events", nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	return c.emit(raw, parsed.bool("pretty") || !parsed.bool("jsonl"))
}

func (c *cli) cmdTrace(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"session-id": "session_id",
		"output":     "output",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{
		"json":     "json",
		"tree":     "tree",
		"markdown": "markdown",
	}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_trace --session-id <id> [--tree|--markdown|--json] [--pretty] [--output path]")
	}
	c.applyBaseURL(parsed)
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	if sessionID == "" && len(parsed.pos) > 0 {
		sessionID = parsed.pos[0]
	}
	if sessionID == "" {
		return errors.New("session id is required")
	}
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/v1/agents/sessions/"+url.PathEscape(sessionID)+"/trace", nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	var body string
	if parsed.bool("json") {
		out, err := marshalString(raw, parsed.bool("pretty"))
		if err != nil {
			return err
		}
		body = out
	} else if parsed.bool("markdown") {
		body = renderTraceMarkdown(raw)
	} else {
		body = renderTraceTree(raw)
	}
	return c.writeText(parsed.string("output", ""), body)
}

func (c *cli) cmdRunAdvisor(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"session-id": "session_id",
		"limit":      "limit",
		"max-facts":  "max_facts",
		"agent-id":   "agent_id",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{"blocking": "blocking"}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_run_advisor '<task>' --pretty OR --session-id <id> --json")
	}
	c.applyBaseURL(parsed)
	query := strings.TrimSpace(strings.Join(parsed.pos, " "))
	var advisor map[string]any
	if query != "" {
		payload := map[string]any{
			"query":                   query,
			"project":                 parsed.string("project", "contextlattice"),
			"projectName":             parsed.string("project", "contextlattice"),
			"topic_path":              emptyToNil(parsed.string("topic_path", "")),
			"topicPath":               emptyToNil(parsed.string("topic_path", "")),
			"retrieval_mode":          parsed.string("mode", "balanced"),
			"include_grounding":       true,
			"include_retrieval_debug": true,
			"combined_sources":        true,
			"wait_for_slow_sources":   parsed.bool("blocking"),
			"sync_slow_sources":       parsed.bool("blocking"),
			"limit":                   parsed.int("limit", 12),
			"max_facts":               parsed.int("max_facts", 24),
			"agent_id":                emptyToNil(parsed.string("agent_id", envString("CONTEXTLATTICE_AGENT_ID", ""))),
		}
		raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/memory/context-pack", payload, parsed.float("timeout", 30))
		if err != nil {
			return err
		}
		advisor = firstMap(raw["run_advisor"], asMap(raw["context_pack"])["run_advisor"])
	} else {
		sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
		if sessionID == "" {
			return errors.New("query or session id is required")
		}
		raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/v1/agents/sessions/"+url.PathEscape(sessionID)+"/trace", nil, parsed.float("timeout", 10))
		if err != nil {
			return err
		}
		advisor = firstMap(raw["run_advisor"], asMap(raw["run_shaping"])["run_advisor"])
	}
	if advisor == nil {
		advisor = map[string]any{"ok": false, "error": "run_advisor_missing"}
	}
	if parsed.bool("json") || parsed.bool("pretty") {
		return c.emit(advisor, parsed.bool("pretty"))
	}
	_, err := fmt.Fprint(c.stdout, renderAdvisor(advisor))
	return err
}

func renderAdvisor(advisor map[string]any) string {
	promptQuality := asMap(advisor["prompt_quality"])
	continuation := asMap(advisor["continuation"])
	objective := asMap(advisor["objective_coherence"])
	graph := asMap(advisor["graph_quality"])
	retrieval := asMap(advisor["retrieval_advice"])
	lines := []string{
		"ContextLattice Run Advisor",
		"posture: " + firstString(advisor["posture"], "unknown"),
		"prompt quality: " + firstString(promptQuality["score"], "0") + " | " + firstString(promptQuality["state"], "unknown"),
		"objective coherence: " + firstString(objective["score"], "0") + " | " + firstString(objective["status"], "unknown"),
		"continuation: " + firstString(continuation["status"], "unknown") + " | pending " + strings.Join(stringList(continuation["pending_sources"], 8), ", "),
		"retrieval: " + firstString(retrieval["recommended_mode"], "balanced") + " via " + firstString(retrieval["recommended_surface"], "cli_for_local_agents"),
		"graph: " + firstString(graph["status"], "not_sampled") + " | " + truncate(firstString(graph["recommendation"]), 220),
		"repair: " + truncate(firstString(continuation["repair_instruction"], objective["repair_instruction"]), 240),
		"follow-up: " + truncate(firstString(continuation["agent_followup_command"]), 260),
		"",
		"next actions:",
	}
	actions := asList(advisor["next_actions"])
	if len(actions) == 0 {
		lines = append(lines, "  - none")
	}
	for _, item := range actions[:minInt(len(actions), 5)] {
		row := asMap(item)
		lines = append(lines, "  - "+firstString(row["label"], "action")+": "+truncate(firstString(row["command"]), 220))
		if reason := firstString(row["reason"]); reason != "" {
			lines = append(lines, "    "+truncate(reason, 220))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

func (c *cli) cmdContextBoundary(args []string) error {
	parsed := parseArgs(args, commonStringFlags(), commonBoolFlags())
	c.applyBaseURL(parsed)
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/ops/context-boundary", nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	result := auditContextBoundary(raw)
	return c.emit(result, parsed.bool("pretty"))
}

func (c *cli) cmdStrictRuntimeNativeOwnership(args []string) error {
	parsed := parseArgs(args, commonStringFlags(), commonBoolFlags())
	c.applyBaseURL(parsed)
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/ops/native-ownership", nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	result := auditNativeOwnership(raw)
	return c.emit(result, parsed.bool("pretty"))
}

func auditContextBoundary(payload map[string]any) map[string]any {
	required := []string{
		"/memory/context-pack", "/tools/context_pack", "/v1/agents/preflight", "/v1/codex/preflight",
		"policy_context_package", "scripts/agent/contextlattice-pack", "scripts/agent/compaction-handoff-payload",
		"scripts/agent_hooks/contextlattice_pre_compaction_write.sh", "scripts/agent_hooks/contextlattice_post_compaction_read.sh",
	}
	requiredFields := []string{"contract_valid", "truncated", "omitted_counts", "actual_json_bytes", "max_total_json_bytes", "max_string_bytes", "max_list_items"}
	findings := []map[string]any{}
	if firstString(payload["schema_id"]) != "contextlattice_context_boundary.v1" {
		findings = append(findings, map[string]any{"reason": "schema_id_mismatch", "actual": payload["schema_id"]})
	}
	if !asBool(payload["ok"]) {
		findings = append(findings, map[string]any{"reason": "context_boundary_not_ok", "status": payload["status"]})
	}
	if asInt(payload["violationCount"]) != 0 {
		findings = append(findings, map[string]any{"reason": "boundary_violations_present", "count": payload["violationCount"]})
	}
	routes := map[string]map[string]any{}
	for _, item := range asList(payload["routes"]) {
		row := asMap(item)
		if path := firstString(row["path"]); path != "" {
			routes[path] = row
		}
	}
	for _, path := range required {
		row, ok := routes[path]
		if !ok {
			findings = append(findings, map[string]any{"reason": "required_boundaries_missing", "path": path})
			continue
		}
		if !asBool(row["bounded"]) {
			findings = append(findings, map[string]any{"reason": "required_boundary_not_bounded", "path": path})
		}
		fields := stringSet(row["metadata_fields"])
		for _, field := range requiredFields {
			if !fields[field] {
				findings = append(findings, map[string]any{"reason": "metadata_field_missing", "path": path, "field": field})
			}
		}
	}
	return map[string]any{
		"ok":                   len(findings) == 0,
		"schema_id":            "contextlattice_context_boundary_audit.v1",
		"source_schema_id":     payload["schema_id"],
		"status":               payload["status"],
		"registry_id":          payload["registry_id"],
		"registry_version":     payload["registry_version"],
		"requiredSurfaceCount": payload["requiredSurfaceCount"],
		"boundedSurfaceCount":  payload["boundedSurfaceCount"],
		"violationCount":       payload["violationCount"],
		"checkedRequiredPaths": required,
		"findings":             findings,
		"raw":                  payload,
	}
}

func auditNativeOwnership(payload map[string]any) map[string]any {
	required := []string{"/health", "/status", "/migration/runtime", "/ops/context-boundary", "/ops/native-ownership", "/memory/context-pack", "/tools/context_pack", "/v1/agents/preflight", "/v1/codex/preflight", "/telemetry/sidecar-health", "/telemetry/strategies", "/telemetry/strategies/history"}
	findings := []map[string]any{}
	if firstString(payload["schema_id"]) != "strict_runtime_native_ownership.v1" {
		findings = append(findings, map[string]any{"reason": "schema_id_mismatch", "actual": payload["schema_id"]})
	}
	if !asBool(payload["ok"]) {
		findings = append(findings, map[string]any{"reason": "native_ownership_not_ok", "status": payload["status"]})
	}
	if asInt(payload["violationCount"]) != 0 {
		findings = append(findings, map[string]any{"reason": "route_violations_present", "count": payload["violationCount"]})
	}
	ownership := asMap(payload["pythonHotPathOwnership"])
	if asInt(ownership["fallbacks"]) != 0 {
		findings = append(findings, map[string]any{"reason": "python_hot_path_fallbacks_present", "count": ownership["fallbacks"], "byPath": ownership["byPath"]})
	}
	routes := map[string]map[string]any{}
	for _, item := range asList(payload["routes"]) {
		row := asMap(item)
		if path := firstString(row["path"]); path != "" {
			routes[path] = row
		}
	}
	for _, path := range required {
		row, ok := routes[path]
		if !ok {
			findings = append(findings, map[string]any{"reason": "required_routes_missing", "path": path})
			continue
		}
		owner := firstString(row["owner"])
		status := firstString(row["status"])
		if (owner != "go_native" && owner != "rust_native") || status != "native" || !asBool(row["strictRuntimeCompatible"]) {
			findings = append(findings, map[string]any{"reason": "required_route_not_native", "path": path, "owner": owner, "status": status})
		}
	}
	return map[string]any{
		"ok":                     len(findings) == 0,
		"schema_id":              "contextlattice_strict_runtime_native_ownership_audit.v1",
		"source_schema_id":       payload["schema_id"],
		"status":                 payload["status"],
		"strictNoPython":         payload["strictNoPython"],
		"routeOwnerClass":        payload["routeOwnerClass"],
		"requiredRouteCount":     payload["requiredRouteCount"],
		"nativeRouteCount":       payload["nativeRouteCount"],
		"violationCount":         payload["violationCount"],
		"pythonHotPathOwnership": ownership,
		"checkedRequiredPaths":   required,
		"findings":               findings,
		"raw":                    payload,
	}
}

func (c *cli) cmdRuntimeDoctor(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"global-home": "global_home"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	globalHome := parsed.string("global_home", envString("CONTEXTLATTICE_GLOBAL_HOME", filepath.Join(homeDir(), ".contextlattice")))
	binDir := filepath.Join(globalHome, "bin")
	core := []string{"contextlattice_search", "contextlattice_pack", "contextlattice_write", "contextlattice_agent_session", "contextlattice_run_advisor", "contextlattice_agent_runtime_doctor", "contextlattice_strict_runtime_native_ownership", "contextlattice_context_boundary"}
	checks := []map[string]any{}
	for _, name := range core {
		path := filepath.Join(binDir, name)
		checks = append(checks, map[string]any{
			"name":              name,
			"ok":                executableExists(path),
			"path":              path,
			"go_native_wrapper": wrapperIsGoNative(path),
		})
	}
	health, _, healthErr := c.requestJSON(context.Background(), http.MethodGet, "/health", nil, parsed.float("timeout", 10))
	checks = append(checks, map[string]any{"name": "gateway_health", "ok": healthErr == nil && asBool(health["ok"]), "service": health["service"], "base_url": c.baseURL})
	boundary, _, boundaryErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/context-boundary", nil, parsed.float("timeout", 10))
	checks = append(checks, map[string]any{"name": "context_boundary", "ok": boundaryErr == nil && asBool(boundary["ok"]) && asInt(boundary["violationCount"]) == 0, "boundedSurfaceCount": boundary["boundedSurfaceCount"], "violationCount": boundary["violationCount"]})
	ownership, _, ownershipErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/native-ownership", nil, parsed.float("timeout", 10))
	hotPath := asMap(ownership["pythonHotPathOwnership"])
	checks = append(checks, map[string]any{"name": "native_ownership", "ok": ownershipErr == nil && asBool(ownership["ok"]) && asInt(hotPath["fallbacks"]) == 0, "nativeRouteCount": ownership["nativeRouteCount"], "python_fallbacks": hotPath["fallbacks"]})
	findings := []map[string]any{}
	for _, check := range checks {
		if !asBool(check["ok"]) {
			findings = append(findings, map[string]any{"reason": "check_failed", "check": check["name"], "detail": check})
		}
		if strings.HasPrefix(firstString(check["name"]), "contextlattice_") && !asBool(check["go_native_wrapper"]) {
			findings = append(findings, map[string]any{"reason": "core_wrapper_not_go_native", "tool": check["name"], "path": check["path"]})
		}
	}
	return c.emit(map[string]any{
		"ok":        len(findings) == 0,
		"schema_id": "contextlattice_native_agent_tools_doctor.v1",
		"checks":    checks,
		"findings":  findings,
	}, parsed.bool("pretty"))
}

func (c *cli) cmdRuntimeProof(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"agent": "agent", "agent-id": "agent_id"}), commonBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_runtime_proof --pretty")
	}
	c.applyBaseURL(parsed)
	phases := []map[string]any{}
	health, _, healthErr := c.requestJSON(context.Background(), http.MethodGet, "/health", nil, parsed.float("timeout", 10))
	phases = append(phases, map[string]any{"name": "health", "ok": healthErr == nil && asBool(health["ok"]), "evidence": health})
	boundary, _, boundaryErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/context-boundary", nil, parsed.float("timeout", 10))
	phases = append(phases, map[string]any{"name": "context_boundary", "ok": boundaryErr == nil && asBool(boundary["ok"]) && asInt(boundary["violationCount"]) == 0, "evidence": boundary})
	ownership, _, ownershipErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/native-ownership", nil, parsed.float("timeout", 10))
	phases = append(phases, map[string]any{"name": "native_ownership", "ok": ownershipErr == nil && asBool(ownership["ok"]), "evidence": ownership})
	runtime, _, runtimeErr := c.requestJSON(context.Background(), http.MethodGet, "/telemetry/agents/runtime?limit=8", nil, parsed.float("timeout", 10))
	phases = append(phases, map[string]any{"name": "agent_runtime", "ok": runtimeErr == nil, "evidence": runtime})
	ok := true
	for _, phase := range phases {
		ok = ok && asBool(phase["ok"])
	}
	return c.emit(map[string]any{"ok": ok, "schema_id": "contextlattice_agent_runtime_proof.v1", "native_cli": true, "phase_count": len(phases), "proof_pack": map[string]any{"phases": phases}}, parsed.bool("pretty"))
}

func (c *cli) cmdAdoptionProof(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"agents": "agents"}), mergeBoolFlags(commonBoolFlags(), map[string]string{"skip-provider-smoke": "skip_provider_smoke", "progress": "progress"}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adoption_proof --agents codex,claude-code --pretty")
	}
	c.applyBaseURL(parsed)
	agents := splitCSV(parsed.string("agents", "codex,claude-code,opencode,hermes-agent,chatgpt-web,chatgpt-desktop,claude-web,claude-desktop"))
	cases := []map[string]any{}
	for _, agent := range agents {
		payload := map[string]any{"agent": agent, "project": parsed.string("project", "contextlattice"), "retrieval_mode": parsed.string("mode", "fast")}
		raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/preflight", payload, parsed.float("timeout", 30))
		format := asMap(raw["format_contracts"])
		validation := asMap(format["validation"])
		cases = append(cases, map[string]any{
			"agent":      agent,
			"ok":         err == nil && asBool(raw["ok"]) && firstString(validation["status"]) == "passed",
			"session_id": raw["session_id"],
			"validation": validation["status"],
			"findings":   errorFinding(err),
		})
	}
	ok := true
	for _, row := range cases {
		ok = ok && asBool(row["ok"])
	}
	return c.emit(map[string]any{"ok": ok, "schema_id": "contextlattice_agent_adoption_proof_matrix.v1", "native_cli": true, "case_count": len(cases), "cases": cases}, parsed.bool("pretty"))
}

func (c *cli) cmdAdapter(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		return c.emitUsage("contextlattice_agent_adapter {profiles|bootstrap|status|context-pack|checkpoint|handoff|event|complete} [options]")
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "profiles":
		return c.adapterProfiles(args)
	case "bootstrap":
		return c.adapterBootstrap(args)
	case "status":
		return c.sessionGet("status", args)
	case "context-pack":
		return c.adapterContextPack(args)
	case "checkpoint":
		return c.adapterCheckpoint(args)
	case "handoff":
		return c.adapterHandoff(args)
	case "event":
		return c.adapterEvent(args)
	case "complete":
		return c.adapterComplete(args)
	default:
		return fmt.Errorf("unknown contextlattice_agent_adapter command %q", sub)
	}
}

func (c *cli) adapterProfiles(args []string) error {
	parsed := parseArgs(args, commonStringFlags(), commonBoolFlags())
	profiles := loadAgentProfiles()
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return c.emit(map[string]any{"ok": true, "schema_id": "contextlattice_universal_agent_adapter_profiles.v1", "result": map[string]any{"profiles": profiles, "profile_names": names}}, parsed.bool("pretty"))
}

func (c *cli) adapterBootstrap(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"agent": "agent", "agent-id": "agent_id", "query": "query", "objective": "objective", "mission": "mission", "goal": "goal"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	agent := parsed.string("agent", "codex")
	query := parsed.string("query", parsed.string("objective", agent+" preflight connectivity and retrieval"))
	payload := dropEmpty(map[string]any{
		"agent":          agent,
		"agent_id":       parsed.string("agent_id", ""),
		"project":        parsed.string("project", "contextlattice"),
		"topic_path":     parsed.string("topic_path", ""),
		"query":          query,
		"retrieval_mode": parsed.string("mode", "balanced"),
		"mission":        parsed.string("mission", ""),
		"objective":      parsed.string("objective", query),
		"goal":           parsed.string("goal", ""),
	})
	raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/preflight", payload, parsed.float("timeout", 45))
	findings := errorFinding(err)
	sessionID := firstString(raw["session_id"], asMap(asMap(raw["agent_runtime"])["session"])["id"])
	if err == nil && sessionID == "" {
		findings = append(findings, map[string]any{"reason": "missing_session_id"})
	}
	ok := err == nil && !explicitFalse(raw["ok"]) && len(findings) == 0
	if sessionID != "" {
		writeSessionState(parsed.string("project", "contextlattice"), sessionID, query, parsed.string("agent_id", ""))
	}
	return c.emit(adapterResponse("bootstrap", ok, firstString(raw["agent"], agent), firstString(raw["agent_id"], payload["agent_id"]), parsed.string("project", "contextlattice"), sessionID, map[string]any{"preflight": compactPreflightForAdapter(raw)}, findings), parsed.bool("pretty"))
}

func adapterStringFlags() map[string]string {
	return mergeStringFlags(commonStringFlags(), map[string]string{
		"agent":         "agent",
		"agent-id":      "agent_id",
		"session-id":    "session_id",
		"mission":       "mission",
		"objective":     "objective",
		"goal":          "goal",
		"query":         "query",
		"limit":         "limit",
		"max-facts":     "max_facts",
		"summary":       "summary",
		"next-action":   "next_action",
		"file":          "file",
		"content":       "content",
		"metadata-json": "metadata_json",
		"status":        "status",
	})
}

func adapterBoolFlags() map[string]string {
	return mergeBoolFlags(commonBoolFlags(), map[string]string{
		"include-retrieval-debug": "include_retrieval_debug",
		"stdin":                   "stdin",
		"strict":                  "strict",
	})
}

type adapterProfile struct {
	agent     string
	agentID   string
	topicPath string
	query     string
	mode      string
	profile   map[string]any
}

func resolveAdapterProfile(parsed parsedArgs) adapterProfile {
	agent := parsed.string("agent", "codex")
	profile := asMap(loadAgentProfiles()[agent])
	agentID := parsed.string("agent_id", firstString(profile["agent_id"], envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", ""))))
	topicPath := parsed.string("topic_path", firstString(profile["topic_path"]))
	query := parsed.string("query", parsed.string("objective", firstString(profile["query"], agent+" preflight connectivity and retrieval")))
	mode := strings.ToLower(parsed.string("mode", firstString(profile["retrieval_mode"], "balanced")))
	if mode == "" {
		mode = "balanced"
	}
	return adapterProfile{agent: agent, agentID: agentID, topicPath: topicPath, query: query, mode: mode, profile: profile}
}

func (c *cli) ensureAdapterSession(parsed parsedArgs, project, objective, agentID string) (string, error) {
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	if sessionID != "" {
		return sessionID, nil
	}
	sessionID = c.ensureSession(project, objective, agentID, parsed.float("timeout", 30))
	if sessionID == "" && parsed.bool("strict") {
		return "", errors.New("failed to create ContextLattice agent session")
	}
	return sessionID, nil
}

func adapterResponse(command string, ok bool, agent, agentID, project, sessionID string, result map[string]any, findings []map[string]any) map[string]any {
	status := "passed"
	if !ok || len(findings) > 0 {
		status = "failed"
	}
	return map[string]any{
		"ok":               ok && len(findings) == 0,
		"schema_id":        "universal_agent_adapter_response.v1",
		"command":          command,
		"agent":            agent,
		"agent_id":         agentID,
		"project":          project,
		"session_id":       sessionID,
		"adapter_contract": defaultAdapterContract(),
		"result":           result,
		"findings":         findings,
		"format_contract": map[string]any{
			"schema_id": "universal_agent_adapter_response.v1",
			"validation": map[string]any{
				"status": status,
			},
		},
	}
}

func compactPreflightForAdapter(raw map[string]any) map[string]any {
	objectiveRuntime := asMap(raw["objective_runtime"])
	objectiveValidation := asMap(asMap(objectiveRuntime["format_contract"])["validation"])
	policy := asMap(raw["policy_context_package"])
	policyValidation := asMap(asMap(policy["format_contract"])["validation"])
	contextPack := asMap(raw["context_pack"])
	agentRuntime := asMap(raw["agent_runtime"])
	session := asMap(agentRuntime["session"])
	return map[string]any{
		"ok":                   raw["ok"],
		"service":              raw["service"],
		"agent":                raw["agent"],
		"agent_id":             raw["agent_id"],
		"project":              raw["project"],
		"query":                raw["query"],
		"topic_path":           raw["topic_path"],
		"retrieval_mode":       raw["retrieval_mode"],
		"session_id":           firstString(raw["session_id"], session["id"]),
		"objective_state":      objectiveRuntime["objective_state"],
		"next_action":          objectiveRuntime["next_action"],
		"objective_validation": objectiveValidation["status"],
		"policy_validation":    policyValidation["status"],
		"context_status":       firstNonEmpty(firstString(raw["context_status"]), firstString(contextPack["status"])),
		"mission_status":       raw["mission_context_status"],
		"skills_index":         compactSkillsIndex(asMap(raw["skills_index"])),
		"agent_runtime": map[string]any{
			"session_id":          firstString(session["id"], raw["session_id"]),
			"last_event_type":     session["last_event_type"],
			"memory_contribution": agentRuntime["memory_contribution"],
		},
		"format_contracts": raw["format_contracts"],
		"raw_omitted":      true,
	}
}

func compactSkillsIndex(payload map[string]any) map[string]any {
	top := []any{}
	for _, item := range asList(payload["results"])[:minInt(len(asList(payload["results"])), 5)] {
		row := asMap(item)
		top = append(top, map[string]any{
			"name":   row["name"],
			"source": row["source"],
			"path":   row["path"],
			"score":  row["score"],
		})
	}
	return map[string]any{
		"ok":            payload["ok"],
		"index":         payload["index"],
		"returned":      payload["returned"],
		"total_matches": payload["total_matches"],
		"top":           top,
	}
}

func defaultAdapterContract() map[string]any {
	return map[string]any{
		"schema_id":          "contextlattice_universal_agent_adapter.v1",
		"version":            "2026-06-05",
		"required_phases":    []any{"preflight", "auto_session", "context_pack", "checkpoint", "handoff", "completion"},
		"preflight_route":    "/v1/agents/preflight",
		"event_route":        "/v1/agents/sessions/event",
		"context_pack_route": "/memory/context-pack",
		"checkpoint_route":   "/memory/write",
	}
}

func (c *cli) adapterContextPack(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter context-pack --agent codex --project contextlattice --session-id <id> --pretty")
	}
	c.applyBaseURL(parsed)
	project := parsed.string("project", "contextlattice")
	profile := resolveAdapterProfile(parsed)
	sessionID, err := c.ensureAdapterSession(parsed, project, parsed.string("objective", profile.query), profile.agentID)
	if err != nil {
		return err
	}
	request := dropEmpty(map[string]any{
		"query":                     profile.query,
		"project":                   project,
		"projectName":               project,
		"topic_path":                parsed.string("topic_path", profile.topicPath),
		"topicPath":                 parsed.string("topic_path", profile.topicPath),
		"retrieval_mode":            profile.mode,
		"limit":                     parsed.int("limit", 6),
		"max_facts":                 parsed.int("max_facts", 8),
		"include_grounding":         true,
		"include_retrieval_debug":   parsed.bool("include_retrieval_debug"),
		"agent_id":                  profile.agentID,
		"session_id":                sessionID,
		"traffic_class":             "user",
		"native_cli_implementation": true,
	})
	contextPack, _, err := c.requestJSON(context.Background(), http.MethodPost, "/memory/context-pack", request, parsed.float("timeout", 30))
	if err != nil {
		return err
	}
	findings := []map[string]any{}
	if validation := contractValidationStatus(contextPack); validation != "" && validation != "passed" {
		findings = append(findings, map[string]any{"reason": "context_pack_validation_not_passed", "validation": validation})
	}
	if containsProviderOverflowPayload(contextPack) {
		findings = append(findings, map[string]any{"reason": "context_pack_provider_overflow_phrase_leaked"})
	}
	event, _, eventErr := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", map[string]any{
		"session_id": sessionID,
		"agent":      profile.agent,
		"agent_id":   profile.agentID,
		"project":    project,
		"type":       "context_pack.completed",
		"summary":    "context pack completed for " + truncate(profile.query, 160),
		"metadata": map[string]any{
			"adapter":          "contextlattice-agent-adapter",
			"topic_path":       request["topic_path"],
			"retrieval_mode":   profile.mode,
			"contract_ok":      len(findings) == 0,
			"go_native_cli":    true,
			"context_pack_ref": firstString(contextPack["schema_id"], "context_pack_response.v1"),
		},
	}, parsed.float("timeout", 10))
	if eventErr != nil {
		findings = append(findings, map[string]any{"reason": "context_pack_event_failed", "detail": truncate(eventErr.Error(), 500)})
	}
	ok := len(findings) == 0 && (len(event) == 0 || asBool(event["ok"]))
	return c.emit(adapterResponse("context-pack", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{
		"profile":        profile.profile,
		"topic_path":     request["topic_path"],
		"query":          profile.query,
		"retrieval_mode": profile.mode,
		"context_pack":   contextPack,
		"event":          event,
	}, findings), parsed.bool("pretty"))
}

func (c *cli) adapterCheckpoint(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter checkpoint --session-id <id> --content '<summary>' [--file notes/agent-adapters/checkpoint.md] --pretty")
	}
	c.applyBaseURL(parsed)
	project := parsed.string("project", "contextlattice")
	profile := resolveAdapterProfile(parsed)
	sessionID, err := c.ensureAdapterSession(parsed, project, parsed.string("objective", profile.query), profile.agentID)
	if err != nil {
		return err
	}
	content := parsed.string("content", "")
	if parsed.bool("stdin") {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		content = string(data)
	}
	if content == "" && len(parsed.pos) > 0 {
		content = strings.Join(parsed.pos, " ")
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return errors.New("checkpoint content is required")
	}
	topicPath := parsed.string("topic_path", profile.topicPath)
	if topicPath == "" {
		topicPath = "agent/checkpoints"
	}
	fileName := parsed.string("file", "notes/agent-adapters/checkpoint.md")
	write, _, err := c.requestJSON(context.Background(), http.MethodPost, "/memory/write", map[string]any{
		"projectName": project,
		"fileName":    fileName,
		"topicPath":   topicPath,
		"content":     content,
		"agent_id":    profile.agentID,
		"session_id":  sessionID,
		"tags":        []any{"agent-writeback", "checkpoint", "universal-agent-adapter", "go-native-cli"},
	}, parsed.float("timeout", 30))
	if err != nil {
		return err
	}
	event, _, eventErr := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", map[string]any{
		"session_id": sessionID,
		"agent":      profile.agent,
		"agent_id":   profile.agentID,
		"project":    project,
		"type":       "writeback.completed",
		"summary":    truncate(content, 240),
		"metadata": map[string]any{
			"adapter":       "contextlattice-agent-adapter",
			"file":          fileName,
			"topic_path":    topicPath,
			"go_native_cli": true,
		},
	}, parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	ok := len(findings) == 0 && asBool(write["ok"])
	return c.emit(adapterResponse("checkpoint", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{
		"writeback": map[string]any{
			"ok":         asBool(write["ok"]),
			"kind":       "checkpoint",
			"project":    project,
			"file":       fileName,
			"topic_path": topicPath,
			"session_id": sessionID,
			"write":      write,
			"event":      event,
		},
	}, findings), parsed.bool("pretty"))
}

func (c *cli) adapterHandoff(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter handoff --session-id <id> --summary '<state>' [--next-action '<step>'] --pretty")
	}
	c.applyBaseURL(parsed)
	project := parsed.string("project", "contextlattice")
	profile := resolveAdapterProfile(parsed)
	summary := parsed.string("summary", strings.Join(parsed.pos, " "))
	if strings.TrimSpace(summary) == "" {
		summary = "universal agent adapter handoff"
	}
	sessionID, err := c.ensureAdapterSession(parsed, project, parsed.string("objective", summary), profile.agentID)
	if err != nil {
		return err
	}
	handoff := map[string]any{
		"ok":          true,
		"schema_id":   "contextlattice_adapter_handoff.v1",
		"project":     project,
		"session_id":  sessionID,
		"agent":       profile.agent,
		"agent_id":    profile.agentID,
		"summary":     truncate(summary, 4000),
		"objective":   parsed.string("objective", ""),
		"next_action": parsed.string("next_action", ""),
		"created_at":  time.Now().UTC().Format(time.RFC3339),
	}
	event, _, eventErr := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", map[string]any{
		"session_id": sessionID,
		"agent":      profile.agent,
		"agent_id":   profile.agentID,
		"project":    project,
		"type":       "handoff.created",
		"summary":    truncate(summary, 240),
		"metadata": map[string]any{
			"adapter":       "contextlattice-agent-adapter",
			"handoff":       handoff,
			"handoff_ok":    true,
			"go_native_cli": true,
		},
	}, parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	ok := len(findings) == 0 && (len(event) == 0 || asBool(event["ok"]))
	return c.emit(adapterResponse("handoff", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{
		"handoff": handoff,
		"event":   event,
	}, findings), parsed.bool("pretty"))
}

func (c *cli) adapterEvent(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter event <event.type> --session-id <id> [--summary text] --pretty")
	}
	if len(parsed.pos) == 0 {
		return errors.New("event type is required")
	}
	c.applyBaseURL(parsed)
	project := parsed.string("project", "contextlattice")
	profile := resolveAdapterProfile(parsed)
	sessionID, err := c.ensureAdapterSession(parsed, project, parsed.string("objective", parsed.pos[0]), profile.agentID)
	if err != nil {
		return err
	}
	summary := parsed.string("summary", strings.Join(parsed.pos[1:], " "))
	event, _, eventErr := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", dropEmpty(map[string]any{
		"session_id": sessionID,
		"agent":      profile.agent,
		"agent_id":   profile.agentID,
		"project":    project,
		"type":       parsed.pos[0],
		"summary":    summary,
		"status":     parsed.string("status", ""),
		"metadata":   parseJSONObject(parsed.string("metadata_json", "")),
	}), parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	ok := len(findings) == 0 && asBool(event["ok"])
	return c.emit(adapterResponse("event", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{"event": event}, findings), parsed.bool("pretty"))
}

func (c *cli) adapterComplete(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter complete --session-id <id> --summary '<result>' --pretty")
	}
	c.applyBaseURL(parsed)
	project := parsed.string("project", "contextlattice")
	profile := resolveAdapterProfile(parsed)
	summary := parsed.string("summary", strings.Join(parsed.pos, " "))
	if summary == "" {
		summary = "session completed"
	}
	sessionID, err := c.ensureAdapterSession(parsed, project, parsed.string("objective", summary), profile.agentID)
	if err != nil {
		return err
	}
	event, _, eventErr := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", map[string]any{
		"session_id": sessionID,
		"agent":      profile.agent,
		"agent_id":   profile.agentID,
		"project":    project,
		"type":       "session.completed",
		"summary":    summary,
		"status":     "completed",
		"metadata": map[string]any{
			"adapter":       "contextlattice-agent-adapter",
			"go_native_cli": true,
		},
	}, parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	ok := len(findings) == 0 && asBool(event["ok"])
	return c.emit(adapterResponse("complete", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{"event": event}, findings), parsed.bool("pretty"))
}

func (c *cli) cmdMemoryTopology(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"profile": "profile"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/telemetry/storage", nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	topology := asMap(raw["memoryTopology"])
	checks := []map[string]any{
		{"name": "topology_schema", "ok": firstString(topology["schema_id"]) == "contextlattice_memory_topology.v1", "schema": topology["schema_id"]},
		{"name": "base_default_hot_path", "ok": len(asList(topology["base_default_hot_path"])) > 0, "sources": topology["base_default_hot_path"]},
	}
	ok := true
	for _, check := range checks {
		ok = ok && asBool(check["ok"])
	}
	return c.emit(map[string]any{"ok": ok, "schema_id": "contextlattice_memory_topology_audit.v1", "checks": checks, "topology": topology}, parsed.bool("pretty"))
}

func (c *cli) cmdSkillsIndex(args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		return c.emitUsage("contextlattice_skills_index {search|reindex} [options]")
	}
	sub := args[0]
	args = args[1:]
	switch sub {
	case "search":
		return c.skillsIndexSearch(args)
	case "reindex":
		return c.skillsIndexReindex(args)
	default:
		return fmt.Errorf("unknown contextlattice_skills_index command %q", sub)
	}
}

func (c *cli) skillsIndexSearch(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"query":     "query",
		"q":         "query",
		"limit":     "limit",
		"min-score": "min_score",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{"show-terms": "show_terms"}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_skills_index search '<query>' [--limit n] [--min-score score] [--show-terms] [--pretty]")
	}
	c.applyBaseURL(parsed)
	query := parsed.string("query", strings.Join(parsed.pos, " "))
	if strings.TrimSpace(query) == "" {
		return errors.New("query is required")
	}
	payload := map[string]any{
		"query":      query,
		"limit":      parsed.int("limit", 10),
		"json":       true,
		"show_terms": parsed.bool("show_terms"),
	}
	if minScore := parsed.string("min_score", ""); minScore != "" {
		payload["min_score"] = minScore
	}
	raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/tools/skills_index_search", payload, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	if err := c.emit(raw, parsed.bool("pretty")); err != nil {
		return err
	}
	if explicitFalse(raw["ok"]) || firstString(raw["error"]) != "" {
		return fmt.Errorf("skills index search failed: %s", truncate(firstString(raw["error"], raw["detail"], "not ok"), 240))
	}
	return nil
}

func (c *cli) skillsIndexReindex(args []string) error {
	parsed := parseArgs(args, commonStringFlags(), commonBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_skills_index reindex [--pretty]")
	}
	c.applyBaseURL(parsed)
	raw, _, err := c.requestJSON(context.Background(), http.MethodPost, "/tools/skills_index_reindex", map[string]any{}, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	if err := c.emit(raw, parsed.bool("pretty")); err != nil {
		return err
	}
	if explicitFalse(raw["ok"]) || firstString(raw["error"]) != "" {
		return fmt.Errorf("skills index reindex failed: %s", truncate(firstString(raw["error"], raw["detail"], "not ok"), 240))
	}
	return nil
}

func marshalString(payload any, pretty bool) (string, error) {
	var raw []byte
	var err error
	if pretty {
		raw, err = json.MarshalIndent(payload, "", "  ")
	} else {
		raw, err = json.Marshal(payload)
	}
	if err != nil {
		return "", err
	}
	return string(raw) + "\n", nil
}

func (c *cli) writeText(path, body string) error {
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if strings.TrimSpace(path) != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(body), 0644)
	}
	_, err := fmt.Fprint(c.stdout, body)
	return err
}

func renderTraceTree(trace map[string]any) string {
	session := asMap(trace["session"])
	runShaping := asMap(trace["run_shaping"])
	contextPack := asMap(runShaping["context"])
	skills := asMap(runShaping["skills"])
	sources := asMap(runShaping["sources"])
	agentInbox := asMap(runShaping["agent_inbox"])
	latestSteering := asMap(agentInbox["latest"])
	graph := asMap(runShaping["graph"])
	handoffs := asMap(runShaping["handoffs"])
	checkpoints := asMap(runShaping["checkpoints"])
	advisor := firstMap(trace["run_advisor"], runShaping["run_advisor"])
	promptQuality := asMap(advisor["prompt_quality"])
	continuation := asMap(advisor["continuation"])
	objective := asMap(runShaping["objective"])
	objectiveHierarchy := firstMap(session["objective_hierarchy"], objective["hierarchy"])
	objectiveLineage := firstMap(session["objective_lineage"], objective["lineage"])
	objectiveProject := asMap(objectiveHierarchy["project"])
	objectiveTopic := asMap(objectiveHierarchy["topic"])
	objectiveSession := asMap(objectiveHierarchy["session"])
	objectiveDrift := asMap(objectiveLineage["drift"])
	phaseCounts := asMap(trace["phase_counts"])
	validation := firstString(asMap(asMap(trace["format_contract"])["validation"])["status"], "unknown")

	lines := []string{
		"ContextLattice Run Trace",
		"session " + firstString(session["id"], "unknown") + " | " + firstString(session["agent"], session["agent_id"], "agent") + " | " + firstString(session["status"], "unknown") + " | confidence " + firstString(session["confidence"], "0"),
		"objective: " + truncate(firstString(session["objective"]), 220),
		"next action: " + truncate(firstString(session["next_action"]), 220),
		"contract: " + firstString(trace["schema_id"], "agent_run_trace.v1") + " | validation " + validation,
		"",
		"objective lineage:",
		"  - project primary: " + truncate(firstString(objectiveProject["primary_objective"]), 220),
		"  - topic: " + truncate(firstString(objectiveTopic["topic_path"]), 140) + " | " + truncate(firstString(objectiveTopic["objective"]), 220),
		"  - session: " + truncate(firstString(objectiveSession["session_id"]), 120) + " | " + truncate(firstString(objectiveSession["objective"]), 220),
		"  - drift: " + firstString(objectiveDrift["status"], "unknown") + " | project-topic " + firstString(objectiveDrift["project_to_topic"], "unknown") + " | topic-session " + firstString(objectiveDrift["topic_to_session"], "unknown"),
		"",
		"run advisor:",
		"  - posture: " + firstString(advisor["posture"], "unknown"),
		"  - prompt quality: " + firstString(promptQuality["score"], "0") + " | " + firstString(promptQuality["state"], "unknown"),
		"  - continuation: " + firstString(continuation["status"], "unknown") + " | pending " + csvAny(continuation["pending_sources"], 8),
		"  - repair: " + truncate(firstString(continuation["repair_instruction"]), 220),
		"  - follow-up: " + truncate(firstString(continuation["agent_followup_command"]), 220),
		"",
		"agent steering:",
		"  - latest: " + truncate(firstString(latestSteering["message"]), 260),
		"  - action: " + truncate(firstString(latestSteering["suggested_action"]), 260),
		"",
		"phases:",
	}
	if len(phaseCounts) == 0 {
		lines = append(lines, "  - none captured")
	} else {
		keys := make([]string, 0, len(phaseCounts))
		for key := range phaseCounts {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			lines = append(lines, "  - "+key+": "+firstString(phaseCounts[key], "0"))
		}
	}
	lines = append(lines,
		"",
		"context:",
		"  - validation: "+firstString(contextPack["validation"], "unknown"),
		"  - prompt ready: "+strings.ToLower(firstString(contextPack["prompt_ready"], "false")),
		"  - reference prompt chars: "+firstString(contextPack["reference_prompt_chars"], "0"),
		"  - surface: "+firstString(contextPack["recommended_surface"], "agent_reference_prompt"),
		"",
		"skills that may be helpful for this work:",
	)
	skillItems := asList(skills["items"])
	if len(skillItems) == 0 {
		lines = append(lines, "  - none captured; search: "+truncate(firstString(skills["lookup_command"]), 180))
	} else {
		for _, item := range skillItems[:minInt(len(skillItems), 8)] {
			row := asMap(item)
			source := firstString(row["source"])
			suffix := ""
			if source != "" {
				suffix = " (" + source + ")"
			}
			score := firstString(row["score"])
			if score != "" && score != "0" {
				score = " score " + score
			} else {
				score = ""
			}
			lines = append(lines, "  - "+truncate(firstString(row["name"]), 120)+suffix+score)
		}
	}
	lines = append(lines,
		"",
		"sources:",
		"  - returned: "+csvAny(sources["returned_sources"], 12),
		"  - pending: "+csvAny(sources["pending_sources"], 12),
		"  - failed: "+csvAny(sources["failed_sources"], 8),
		"",
		"run shaping:",
		"  - graph touches: "+firstString(graph["touches"], "0"),
		"  - handoffs: "+firstString(handoffs["count"], "0"),
		"  - checkpoints: "+firstString(checkpoints["count"], "0"),
		"",
		"timeline:",
	)
	timeline := asList(trace["timeline"])
	if len(timeline) == 0 {
		lines = append(lines, "  - none captured")
	} else {
		start := maxInt(0, len(timeline)-16)
		for _, item := range timeline[start:] {
			row := asMap(item)
			lines = append(lines, "  - "+strings.Join([]string{
				firstString(row["phase"], "other"),
				firstString(row["type"], "event"),
				firstString(row["status"], "unknown"),
				truncate(firstString(row["summary"]), 160),
			}, " | "))
		}
	}
	runCard := asMap(trace["run_card"])
	lines = append(lines,
		"",
		"exports:",
		"  - json: "+truncate(firstString(runCard["json_endpoint"]), 180),
		"  - tree: "+truncate(firstString(runCard["cli_tree"]), 180),
		"  - markdown: "+truncate(firstString(runCard["cli_markdown"]), 180),
	)
	return strings.Join(lines, "\n") + "\n"
}

func renderTraceMarkdown(trace map[string]any) string {
	if markdown := firstString(asMap(trace["run_card"])["markdown"]); markdown != "" {
		return strings.TrimRight(markdown, "\n") + "\n"
	}
	return "# ContextLattice Agent Run Card\n\nNo run card was returned.\n"
}

func csvAny(value any, limit int) string {
	items := []string{}
	for _, item := range asList(value)[:minInt(len(asList(value)), limit)] {
		if text := firstString(item); text != "" {
			items = append(items, text)
		}
	}
	if len(items) == 0 {
		return "none"
	}
	return strings.Join(items, ", ")
}

func explicitFalse(value any) bool {
	_, present := value.(bool)
	return present && !asBool(value)
}

func contractValidationStatus(payload map[string]any) string {
	return strings.ToLower(firstString(asMap(asMap(payload["format_contract"])["validation"])["status"]))
}

func containsProviderOverflowPayload(payload any) bool {
	raw, _ := json.Marshal(payload)
	text := strings.ToLower(string(raw))
	for _, pattern := range []string{"array_above_max_length", "context length exceeded", "maximum context length", "max context length", "input array is too long", "oversized input"} {
		if strings.Contains(text, pattern) {
			return true
		}
	}
	return false
}

func loadAgentProfiles() map[string]any {
	paths := []string{
		filepath.Join(repoRoot(), "config", "agents", "agent_profiles.json"),
		filepath.Join(envString("CONTEXTLATTICE_GLOBAL_HOME", filepath.Join(homeDir(), ".contextlattice")), "config", "agents", "agent_profiles.json"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parsed := map[string]any{}
		if json.Unmarshal(data, &parsed) == nil {
			return asMap(parsed["profiles"])
		}
	}
	return map[string]any{}
}

func executableExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0111 != 0
}

func wrapperIsGoNative(path string) bool {
	target, err := filepath.EvalSymlinks(path)
	if err == nil && strings.Contains(filepath.Base(target), "contextlattice-agent-tools") {
		return true
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	text := string(data)
	return strings.Contains(text, "contextlattice-agent-tools") && !strings.Contains(text, "PYTHON_BIN") && !strings.Contains(text, "python")
}

func errorFinding(err error) []map[string]any {
	if err == nil {
		return []map[string]any{}
	}
	return []map[string]any{{"reason": "request_failed", "detail": truncate(err.Error(), 500)}}
}

func writeSessionState(project, sessionID, objective, agentID string) {
	if sessionID == "" {
		return
	}
	root := filepath.Join(homeDir(), ".contextlattice", "agent_runtime_sessions")
	_ = os.MkdirAll(root, 0700)
	keyRaw := project + "|" + currentWorkingDir()
	hash := sha256.Sum256([]byte(keyRaw))
	path := filepath.Join(root, hex.EncodeToString(hash[:])[:16]+".json")
	payload := map[string]any{"session_id": sessionID, "project": project, "agent_id": agentID, "objective": objective, "source": "go-native-cli", "updated_at": time.Now().UTC().Format(time.RFC3339)}
	raw, _ := json.Marshal(payload)
	_ = os.WriteFile(path, append(raw, '\n'), 0600)
}

func parseJSONObject(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func dropEmpty(in map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range in {
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) == "" {
				continue
			}
		case nil:
			continue
		case map[string]any:
			if len(v) == 0 {
				continue
			}
		}
		out[key] = value
	}
	return out
}

func (c *cli) emitUsage(text string) error {
	_, err := fmt.Fprintln(c.stdout, text)
	return err
}

func asMap(value any) map[string]any {
	if row, ok := value.(map[string]any); ok {
		return row
	}
	return map[string]any{}
}

func firstMap(values ...any) map[string]any {
	for _, value := range values {
		row := asMap(value)
		if len(row) > 0 {
			return row
		}
	}
	return nil
}

func asList(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return []any{}
}

func firstList(values ...any) []any {
	for _, value := range values {
		items := asList(value)
		if len(items) > 0 {
			return items
		}
	}
	return []any{}
}

func stringList(value any, limit int) []string {
	items := asList(value)
	out := []string{}
	for _, item := range items[:minInt(len(items), limit)] {
		if text := firstString(item); text != "" {
			out = append(out, text)
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

func asBool(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(v, "true") || v == "1"
	default:
		return false
	}
}

func asInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		i, _ := strconv.Atoi(v)
		return i
	default:
		return 0
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		switch v := value.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case fmt.Stringer:
			if strings.TrimSpace(v.String()) != "" {
				return strings.TrimSpace(v.String())
			}
		case nil:
		default:
			text := fmt.Sprint(v)
			if strings.TrimSpace(text) != "" && text != "<nil>" {
				return strings.TrimSpace(text)
			}
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyLower(values ...any) string {
	return strings.ToLower(firstString(values...))
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func emptyToNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func ensureSlash(path string) string {
	if strings.HasPrefix(path, "/") || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return "/" + path
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 3 {
		return value[:limit]
	}
	return strings.TrimSpace(value[:limit-3]) + "..."
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}

func currentWorkingDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return ""
}

func gitValue(args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoRoot()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func autoSessionDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CONTEXTLATTICE_AUTO_SESSION_DISABLED"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func stringSet(value any) map[string]bool {
	out := map[string]bool{}
	for _, item := range asList(value) {
		out[firstString(item)] = true
	}
	return out
}

func splitCSV(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if item := strings.TrimSpace(part); item != "" {
			out = append(out, item)
		}
	}
	return out
}

func init() {
	flag.CommandLine.SetOutput(io.Discard)
}
