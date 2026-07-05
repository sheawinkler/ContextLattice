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
	"contextlattice_agent_discover":                  "discover",
	"contextlattice_runner_quality":                  "runner-quality",
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
	case "discover":
		return c.cmdDiscover(args)
	case "runner-quality":
		return c.cmdRunnerQuality(args)
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
  adapter                        profiles/bootstrap/status/context-pack/checkpoint/handoff/state/event/complete helpers
  discover                       local agent process/profile/integration discovery
  runner-quality                 bounded adapter quality telemetry and advisor-only recommendations
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
		return c.emitUsage("contextlattice_adopt {status|doctor|proof|profiles|install|integrate} [options]")
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
	case "integrate":
		return c.cmdAdoptIntegrate(args)
	default:
		return fmt.Errorf("unknown contextlattice_adopt command %q", sub)
	}
}

type integrationTarget struct {
	file  string
	label string
}

var agentInstructionTargets = map[string]integrationTarget{
	"codex":        {file: "AGENTS.md", label: "Codex"},
	"claude-code":  {file: "CLAUDE.md", label: "Claude Code"},
	"opencode":     {file: "AGENTS.md", label: "OpenCode"},
	"hermes-agent": {file: "HERMES.md", label: "Hermes Agent"},
	"hermes-ultra": {file: "HERMES.md", label: "Hermes Ultra"},
	"pi":           {file: "PI.md", label: "Pi"},
	"droid":        {file: "DROID.md", label: "Droid"},
}

func (c *cli) cmdAdoptIntegrate(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"repo": "repo", "agents": "agents"}), mergeBoolFlags(commonBoolFlags(), map[string]string{"dry-run": "dry_run", "check": "check"}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_adopt integrate --repo . --agents codex,claude-code,opencode,hermes-agent,hermes-ultra,pi,droid [--check] --pretty")
	}
	repo := parsed.string("repo", ".")
	if repo == "" {
		repo = "."
	}
	absRepo, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	profiles := loadAgentProfiles()
	agents := splitCSV(parsed.string("agents", "codex,claude-code,opencode,hermes-agent,hermes-ultra,pi,droid"))
	if parsed.bool("check") {
		audit := auditRepoIntegration(absRepo, agents)
		if err := c.emit(audit, parsed.bool("pretty")); err != nil {
			return err
		}
		if !asBool(audit["ok"]) {
			return errors.New("repo integration check failed")
		}
		return nil
	}
	byFile := map[string][]string{}
	findings := []map[string]any{}
	for _, agent := range agents {
		target, ok := agentInstructionTargets[agent]
		if !ok {
			findings = append(findings, map[string]any{"reason": "unsupported_agent_instruction_target", "agent": agent})
			continue
		}
		byFile[target.file] = append(byFile[target.file], agent)
	}
	writes := []map[string]any{}
	for file, fileAgents := range byFile {
		path := filepath.Join(absRepo, file)
		block := renderAgentInstructionBlock(fileAgents, profiles, parsed.string("project", "contextlattice"))
		changed, err := upsertManagedBlock(path, block, parsed.bool("dry_run"))
		if err != nil {
			findings = append(findings, map[string]any{"reason": "write_failed", "path": path, "detail": err.Error()})
			continue
		}
		writes = append(writes, map[string]any{"path": path, "agents": fileAgents, "changed": changed, "dry_run": parsed.bool("dry_run")})
	}
	sort.Slice(writes, func(i, j int) bool { return firstString(writes[i]["path"]) < firstString(writes[j]["path"]) })
	ok := len(findings) == 0
	return c.emit(map[string]any{
		"ok":         ok,
		"schema_id":  "contextlattice_agent_repo_integration.v1",
		"repo":       absRepo,
		"agents":     agents,
		"writes":     writes,
		"findings":   findings,
		"next_steps": []string{"contextlattice_adopt status --pretty", "contextlattice_doctor --agents " + strings.Join(agents, ",") + " --skip-provider-smoke --pretty"},
	}, parsed.bool("pretty"))
}

func auditRepoIntegration(repo string, agents []string) map[string]any {
	const begin = "<!-- >>> contextlattice-agent-integration >>>"
	const end = "<!-- <<< contextlattice-agent-integration <<< -->"
	requiredSnippets := []string{
		"ContextLattice Agent Integration",
		"contextlattice_adopt install --pretty",
		"contextlattice_adopt status --pretty",
		"contextlattice_agent_adapter bootstrap",
		"contextlattice_agent_adapter context-pack",
		"contextlattice_checkpoint",
		"degraded-memory mode",
	}
	byFile := map[string][]string{}
	findings := []map[string]any{}
	for _, agent := range agents {
		target, ok := agentInstructionTargets[agent]
		if !ok {
			findings = append(findings, map[string]any{"reason": "unsupported_agent_instruction_target", "agent": agent})
			continue
		}
		byFile[target.file] = append(byFile[target.file], agent)
	}
	files := []map[string]any{}
	for file, fileAgents := range byFile {
		sort.Strings(fileAgents)
		path := filepath.Join(repo, file)
		fileFindings := []map[string]any{}
		data, err := os.ReadFile(path)
		exists := err == nil
		text := string(data)
		if err != nil {
			fileFindings = append(fileFindings, map[string]any{"reason": "instruction_file_missing", "path": path})
		} else {
			beginCount := strings.Count(text, begin)
			endCount := strings.Count(text, end)
			if beginCount != 1 {
				fileFindings = append(fileFindings, map[string]any{"reason": "managed_begin_marker_count", "path": path, "count": beginCount})
			}
			if endCount != 1 {
				fileFindings = append(fileFindings, map[string]any{"reason": "managed_end_marker_count", "path": path, "count": endCount})
			}
			if beginCount == 1 && endCount == 1 && strings.Index(text, begin) > strings.Index(text, end) {
				fileFindings = append(fileFindings, map[string]any{"reason": "managed_markers_reversed", "path": path})
			}
			for _, snippet := range requiredSnippets {
				if !strings.Contains(text, snippet) {
					fileFindings = append(fileFindings, map[string]any{"reason": "required_snippet_missing", "path": path, "snippet": snippet})
				}
			}
			for _, agent := range fileAgents {
				if !strings.Contains(text, "profile `"+agent+"`") {
					fileFindings = append(fileFindings, map[string]any{"reason": "agent_profile_entry_missing", "path": path, "agent": agent})
				}
			}
		}
		findings = append(findings, fileFindings...)
		files = append(files, map[string]any{"file": file, "path": path, "agents": fileAgents, "exists": exists, "ok": len(fileFindings) == 0, "findings": fileFindings})
	}
	sort.Slice(files, func(i, j int) bool { return firstString(files[i]["file"]) < firstString(files[j]["file"]) })
	return map[string]any{
		"ok":        len(findings) == 0,
		"schema_id": "contextlattice_agent_repo_integration_audit.v1",
		"repo":      repo,
		"agents":    agents,
		"files":     files,
		"findings":  findings,
	}
}

func renderAgentInstructionBlock(agents []string, profiles map[string]any, project string) string {
	sort.Strings(agents)
	var b strings.Builder
	b.WriteString("<!-- >>> contextlattice-agent-integration >>>\n")
	b.WriteString("# ContextLattice Agent Integration\n\n")
	b.WriteString("Use ContextLattice as the local memory and context layer for non-trivial work in this repo.\n\n")
	b.WriteString("Install or repair once per machine:\n\n")
	b.WriteString("```bash\n")
	b.WriteString("contextlattice_adopt install --pretty\n")
	b.WriteString("contextlattice_adopt status --pretty\n")
	b.WriteString("```\n\n")
	b.WriteString("Agent profiles for this repo:\n\n")
	for _, agent := range agents {
		profile := asMap(profiles[agent])
		target := agentInstructionTargets[agent]
		agentID := firstString(profile["agent_id"], strings.ReplaceAll(agent, "-", "_")+"_agent")
		topicPath := firstString(profile["topic_path"], "runbooks/"+agent+"-integration")
		b.WriteString(fmt.Sprintf("- %s: profile `%s`, agent id `%s`, topic `%s`.\n", target.label, agent, agentID, topicPath))
	}
	b.WriteString("\nBefore planning or coding, run a scoped bootstrap/context pack when CLI tools are available:\n\n")
	b.WriteString("```bash\n")
	b.WriteString(fmt.Sprintf("contextlattice_agent_adapter bootstrap --agent <profile> --project %s --pretty\n", shellQuote(project)))
	b.WriteString(fmt.Sprintf("contextlattice_agent_adapter context-pack --agent <profile> --project %s --pretty\n", shellQuote(project)))
	b.WriteString("```\n\n")
	b.WriteString("During long work, checkpoint decisions or blockers with `contextlattice_checkpoint` or `contextlattice_agent_adapter checkpoint`.\n")
	b.WriteString("For compaction or handoff, write a concise handoff. Post-compaction readback is bounded and opportunistic; do not paste large readbacks into the prompt unless the previous state is actually needed.\n")
	b.WriteString("If CLI tools are unavailable, use HTTP against `http://127.0.0.1:8075`: `POST /v1/agents/preflight`, `POST /memory/context-pack`, and `POST /memory/write`.\n")
	b.WriteString("If memory is unreachable or degraded, continue from local evidence and state `degraded-memory mode` explicitly.\n")
	b.WriteString("<!-- <<< contextlattice-agent-integration <<< -->\n")
	return b.String()
}

func upsertManagedBlock(path, block string, dryRun bool) (bool, error) {
	const begin = "<!-- >>> contextlattice-agent-integration >>>"
	const end = "<!-- <<< contextlattice-agent-integration <<< -->"
	currentRaw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	current := string(currentRaw)
	next := ""
	if start := strings.Index(current, begin); start >= 0 {
		if stopRel := strings.Index(current[start:], end); stopRel >= 0 {
			stop := start + stopRel + len(end)
			next = strings.TrimRight(current[:start], "\n") + "\n\n" + strings.TrimRight(block, "\n") + "\n" + strings.TrimLeft(current[stop:], "\n")
		}
	}
	if next == "" {
		if strings.TrimSpace(current) == "" {
			next = strings.TrimRight(block, "\n") + "\n"
		} else {
			next = strings.TrimRight(current, "\n") + "\n\n" + strings.TrimRight(block, "\n") + "\n"
		}
	}
	if next == current {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return false, err
	}
	return true, os.WriteFile(path, []byte(next), 0644)
}

func shellQuote(value string) string {
	if value == "" {
		return "contextlattice"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func (c *cli) adoptStatus(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"global-home": "global_home"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	globalHome := parsed.string("global_home", envString("CONTEXTLATTICE_GLOBAL_HOME", filepath.Join(homeDir(), ".contextlattice")))
	profiles := loadAgentProfiles()
	profileNames := make([]string, 0, len(profiles))
	for name := range profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	health, _, healthErr := c.requestJSON(context.Background(), http.MethodGet, "/health", nil, parsed.float("timeout", 10))
	boundary, _, boundaryErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/context-boundary", nil, parsed.float("timeout", 10))
	ownership, _, ownershipErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/native-ownership", nil, parsed.float("timeout", 10))
	runnerQuality, _, runnerQualityErr := c.requestJSON(context.Background(), http.MethodGet, "/telemetry/runner-quality?limit=200", nil, parsed.float("timeout", 10))
	installChecks := adoptionInstallChecks(globalHome)
	discovery := localAgentDiscoverySummary(globalHome, profileNames, "", 4)
	ok := healthErr == nil && boundaryErr == nil && ownershipErr == nil && asBool(health["ok"]) && asBool(boundary["ok"]) && asBool(ownership["ok"]) && asBool(installChecks["ok"])
	return c.emit(map[string]any{
		"ok":              ok,
		"schema_id":       "contextlattice_agent_adoption_status.v1",
		"native_cli":      true,
		"base_url":        c.baseURL,
		"global_home":     globalHome,
		"profile_count":   len(profileNames),
		"profiles":        profileNames,
		"health":          health,
		"contextBoundary": map[string]any{"ok": boundary["ok"], "violationCount": boundary["violationCount"], "boundedSurfaceCount": boundary["boundedSurfaceCount"]},
		"nativeOwnership": map[string]any{"ok": ownership["ok"], "violationCount": ownership["violationCount"], "nativeRouteCount": ownership["nativeRouteCount"], "pythonHotPathOwnership": ownership["pythonHotPathOwnership"]},
		"runner_quality":  dropEmpty(map[string]any{"ok": runnerQualityErr == nil, "summary": runnerQuality, "error": errString(runnerQualityErr)}),
		"install":         installChecks,
		"agent_discovery": discovery,
		"repair_command":  "scripts/install_global_agent_tools.sh --install-codex-hooks --no-shell-profile",
	}, parsed.bool("pretty"))
}

func adoptionInstallChecks(globalHome string) map[string]any {
	binDir := filepath.Join(globalHome, "bin")
	toolNames := []string{
		"contextlattice-agent-tools",
		"contextlattice_search",
		"contextlattice_pack",
		"contextlattice_write",
		"contextlattice_adopt",
		"contextlattice_doctor",
		"contextlattice_agent_adapter",
		"contextlattice_agent_discover",
		"contextlattice_runner_quality",
		"contextlattice_agent_session",
		"contextlattice_agent_start",
		"contextlattice_checkpoint",
		"contextlattice_pre_compaction_write",
		"contextlattice_post_compaction_read",
	}
	runtimeFiles := []string{
		filepath.Join(globalHome, "scripts", "agent", "_common.py"),
		filepath.Join(globalHome, "scripts", "agent", "audit-codex-session-store"),
		filepath.Join(globalHome, "scripts", "agent", "compaction-handoff-payload"),
		filepath.Join(globalHome, "scripts", "agent", "contextlattice-session"),
		filepath.Join(globalHome, "scripts", "agent_contracts.py"),
		filepath.Join(globalHome, "scripts", "agent_orchestration.py"),
		filepath.Join(globalHome, "scripts", "contextlattice_client.py"),
	}
	checks := []map[string]any{}
	ok := true
	for _, name := range toolNames {
		path := filepath.Join(binDir, name)
		installed := executableExists(path)
		ok = ok && installed
		checks = append(checks, map[string]any{"name": name, "path": path, "ok": installed, "kind": "tool"})
	}
	for _, path := range runtimeFiles {
		info, err := os.Stat(path)
		installed := err == nil && !info.IsDir()
		ok = ok && installed
		checks = append(checks, map[string]any{"name": strings.TrimPrefix(path, globalHome+string(os.PathSeparator)), "path": path, "ok": installed, "kind": "hook_runtime"})
	}
	return map[string]any{"ok": ok, "checks": checks}
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

func contextPackTokenBudgetStringFlags() map[string]string {
	return map[string]string{
		"agent-context-budget-tokens": "agent_context_budget_tokens",
		"model-context-window-tokens": "model_context_window_tokens",
		"reserved-response-tokens":    "reserved_response_tokens",
		"already-loaded-tokens":       "already_loaded_tokens",
		"target-context-pack-tokens":  "target_context_pack_tokens",
		"budget-tokens":               "target_context_pack_tokens",
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

func (a parsedArgs) has(name string) bool {
	if _, ok := a.values[name]; ok {
		return true
	}
	if _, ok := a.bools[name]; ok {
		return true
	}
	return false
}

func (a parsedArgs) boolString(name string) (bool, bool, error) {
	raw, ok := a.values[name]
	if !ok {
		return false, false, nil
	}
	switch strings.TrimSpace(strings.ToLower(raw)) {
	case "1", "true", "yes", "on":
		return true, true, nil
	case "0", "false", "no", "off":
		return false, true, nil
	default:
		return false, true, fmt.Errorf("--%s must be true or false", strings.ReplaceAll(name, "_", "-"))
	}
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

func currentGitRoot() string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = currentWorkingDir()
	if out, err := cmd.Output(); err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
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
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), contextPackTokenBudgetStringFlags(), map[string]string{
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
	addContextPackTokenBudgetArgs(payload, parsed)
	raw, err := c.requestWithRetries("/memory/context-pack", payload, parsed.float("timeout", 30), parsed.int("retries", 2), parsed.float("retry_delay", 1))
	if err != nil {
		if parsed.bool("soft") {
			return c.emit(failurePack(query, parsed.int("budget_chars", 10000), err), !parsed.bool("raw"))
		}
		return err
	}
	out := normalizePackOutput(raw, query, parsed.int("budget_chars", 10000))
	qualitySample := contextPackQualitySample(out)
	if len(qualitySample) == 0 {
		qualitySample = contextPackQualitySample(raw)
	}
	qualitySampleID := firstString(qualitySample["sample_id"])
	recordContextPackQualityPending(project, sessionID, query, agentID, qualitySample)
	if report := contextPackOutcomeReport(sessionID, qualitySampleID); len(report) > 0 {
		out["outcome_report"] = report
	}
	if err := c.emit(out, parsed.bool("pretty") || !parsed.bool("raw")); err != nil {
		return err
	}
	return nil
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
	return c.ensureSessionForAgent(project, objective, envString("CONTEXTLATTICE_AGENT", "agent-cli"), agentID, map[string]any{}, adapterProfile{}, timeout)
}

func (c *cli) ensureSessionForAgent(project, objective, agent, agentID string, ownership map[string]any, profile adapterProfile, timeout float64) string {
	if agent == "" {
		agent = envString("CONTEXTLATTICE_AGENT", "agent-cli")
	}
	if agentID == "" {
		agentID = envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", ""))
	}
	if len(ownership) == 0 {
		ownership = adapterOwnership(parsedArgs{})
	}
	state := buildAgentLifecycleState(parsedArgs{}, profile, "working")
	payload := map[string]any{
		"project":           project,
		"objective":         objective,
		"agent":             agent,
		"agent_id":          agentID,
		"repo":              ownership["repo"],
		"branch":            ownership["branch"],
		"worktree":          ownership["worktree"],
		"cwd":               ownership["cwd"],
		"task_id":           ownership["task_id"],
		"native_session_id": ownership["native_session_id"],
		"agent_state":       state,
		"tags":              []string{"auto-session", "context-pack", "go-native-cli"},
		"metadata": map[string]any{
			"tool":            "contextlattice_pack",
			"ownership":       ownership,
			"agent_state":     state,
			"state_authority": state["authority"],
		},
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
	pack := asMap(raw["context_pack"])
	if _, ok := raw["token_budget"]; !ok {
		if tokenBudget := asMap(firstMap(pack["token_budget"], pack["tokenBudget"])); len(tokenBudget) > 0 {
			raw["token_budget"] = tokenBudget
		}
	}
	if _, ok := raw["omitted_high_value_refs"]; !ok {
		if omitted := firstList(pack["omitted_high_value_refs"], pack["omittedHighValueRefs"]); len(omitted) > 0 {
			raw["omitted_high_value_refs"] = omitted
		}
	}
	raw["task_summary"] = query
	raw["context_budget_chars"] = budget
	raw["writeback_required"] = true
	return raw
}

func addContextPackTokenBudgetArgs(payload map[string]any, parsed parsedArgs) {
	for _, field := range []struct {
		key string
		arg string
	}{
		{"agent_context_budget_tokens", "agent_context_budget_tokens"},
		{"model_context_window_tokens", "model_context_window_tokens"},
		{"reserved_response_tokens", "reserved_response_tokens"},
		{"already_loaded_tokens", "already_loaded_tokens"},
		{"target_context_pack_tokens", "target_context_pack_tokens"},
	} {
		if value := parsed.int(field.arg, 0); value > 0 {
			payload[field.key] = value
		}
	}
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
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{"global-home": "global_home", "repo": "repo", "agents": "agents"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	globalHome := parsed.string("global_home", envString("CONTEXTLATTICE_GLOBAL_HOME", filepath.Join(homeDir(), ".contextlattice")))
	binDir := filepath.Join(globalHome, "bin")
	core := []string{"contextlattice_search", "contextlattice_pack", "contextlattice_write", "contextlattice_agent_session", "contextlattice_agent_discover", "contextlattice_runner_quality", "contextlattice_run_advisor", "contextlattice_agent_runtime_doctor", "contextlattice_strict_runtime_native_ownership", "contextlattice_context_boundary"}
	checks := []map[string]any{}
	for _, name := range core {
		path := filepath.Join(binDir, name)
		checks = append(checks, map[string]any{
			"name":              name,
			"ok":                executableExists(path),
			"path":              path,
			"go_native_wrapper": wrapperIsGoNative(path),
			"explanation":       "expected executable Go-native global wrapper in ContextLattice bin directory",
		})
	}
	health, _, healthErr := c.requestJSON(context.Background(), http.MethodGet, "/health", nil, parsed.float("timeout", 10))
	checks = append(checks, map[string]any{"name": "gateway_health", "ok": healthErr == nil && asBool(health["ok"]), "service": health["service"], "base_url": c.baseURL, "explanation": "gateway must be reachable before agents can report lifecycle or retrieve memory"})
	boundary, _, boundaryErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/context-boundary", nil, parsed.float("timeout", 10))
	checks = append(checks, map[string]any{"name": "context_boundary", "ok": boundaryErr == nil && asBool(boundary["ok"]) && asInt(boundary["violationCount"]) == 0, "boundedSurfaceCount": boundary["boundedSurfaceCount"], "violationCount": boundary["violationCount"], "explanation": "agent-facing context responses must stay within the bounded output contract"})
	ownership, _, ownershipErr := c.requestJSON(context.Background(), http.MethodGet, "/ops/native-ownership", nil, parsed.float("timeout", 10))
	hotPath := asMap(ownership["pythonHotPathOwnership"])
	checks = append(checks, map[string]any{"name": "native_ownership", "ok": ownershipErr == nil && asBool(ownership["ok"]) && asInt(hotPath["fallbacks"]) == 0, "nativeRouteCount": ownership["nativeRouteCount"], "python_fallbacks": hotPath["fallbacks"], "explanation": "default live agent hot paths should be owned by native routes and wrappers"})
	runnerQuality, _, runnerQualityErr := c.requestJSON(context.Background(), http.MethodGet, "/telemetry/runner-quality?limit=200", nil, parsed.float("timeout", 10))
	checks = append(checks, map[string]any{"name": "runner_quality", "ok": runnerQualityErr == nil && !explicitFalse(runnerQuality["ok"]), "telemetry": runnerQuality, "explanation": "runner quality is advisory telemetry for operator selection; it must not dispatch work automatically"})
	if repo := parsed.string("repo", ""); repo != "" {
		absRepo, err := filepath.Abs(repo)
		repoAudit := map[string]any{"ok": false, "schema_id": "contextlattice_agent_repo_integration_audit.v1", "repo": repo, "findings": []map[string]any{{"reason": "repo_path_invalid", "detail": errString(err)}}}
		if err == nil {
			repoAudit = auditRepoIntegration(absRepo, splitCSV(parsed.string("agents", "codex,claude-code,opencode,hermes-agent,hermes-ultra,pi,droid")))
		}
		checks = append(checks, map[string]any{"name": "repo_integration", "ok": asBool(repoAudit["ok"]), "repo": absRepo, "audit": repoAudit, "explanation": "repo-local instruction blocks tell non-hooked agents how to bootstrap, checkpoint, handoff, and report lifecycle state"})
	}
	discovery := localAgentDiscoverySummary(globalHome, splitCSV(parsed.string("agents", "")), parsed.string("repo", ""), 6)
	checks = append(checks, map[string]any{"name": "agent_discovery", "ok": asBool(discovery["ok"]), "discovery": discovery, "explanation": "best-effort process/profile discovery explains observable agent presence without depending on prompt compliance"})
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
		"ok":                      len(findings) == 0,
		"schema_id":               "contextlattice_native_agent_tools_doctor.v1",
		"checks":                  checks,
		"findings":                findings,
		"diagnostic_explanations": doctorExplanations(checks),
	}, parsed.bool("pretty"))
}

func (c *cli) cmdRunnerQuality(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"limit":      "limit",
		"task-class": "task_class",
	}), commonBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_runner_quality [--task-class scout] [--limit 500] --pretty")
	}
	c.applyBaseURL(parsed)
	values := url.Values{}
	values.Set("limit", strconv.Itoa(parsed.int("limit", 500)))
	if taskClass := parsed.string("task_class", ""); taskClass != "" {
		values.Set("task_class", taskClass)
	}
	raw, _, err := c.requestJSON(context.Background(), http.MethodGet, "/telemetry/runner-quality?"+values.Encode(), nil, parsed.float("timeout", 10))
	if err != nil {
		return err
	}
	return c.emit(raw, parsed.bool("pretty"))
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
	agents := splitCSV(parsed.string("agents", "codex,claude-code,opencode,hermes-agent,hermes-ultra,pi,droid,chatgpt-web,chatgpt-desktop,claude-web,claude-desktop"))
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
		return c.emitUsage("contextlattice_agent_adapter {profiles|bootstrap|status|context-pack|checkpoint|handoff|state|event|outcome|complete} [options]")
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
	case "state":
		return c.adapterState(args)
	case "event":
		return c.adapterEvent(args)
	case "outcome":
		return c.adapterOutcome(args)
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
	parsed := parseArgs(args, mergeStringFlags(adapterStringFlags(), map[string]string{"agent": "agent", "agent-id": "agent_id", "query": "query", "objective": "objective", "mission": "mission", "goal": "goal"}), commonBoolFlags())
	c.applyBaseURL(parsed)
	agent := parsed.string("agent", "codex")
	profile := resolveAdapterProfile(parsed)
	query := parsed.string("query", parsed.string("objective", agent+" preflight connectivity and retrieval"))
	payload := dropEmpty(map[string]any{
		"agent":             agent,
		"agent_id":          parsed.string("agent_id", ""),
		"project":           parsed.string("project", "contextlattice"),
		"topic_path":        parsed.string("topic_path", ""),
		"query":             query,
		"retrieval_mode":    parsed.string("mode", "balanced"),
		"mission":           parsed.string("mission", ""),
		"objective":         parsed.string("objective", query),
		"goal":              parsed.string("goal", ""),
		"task_id":           parsed.string("task_id", ""),
		"repo":              adapterOwnership(parsed)["repo"],
		"branch":            adapterOwnership(parsed)["branch"],
		"worktree":          adapterOwnership(parsed)["worktree"],
		"cwd":               adapterOwnership(parsed)["cwd"],
		"native_session_id": adapterOwnership(parsed)["native_session_id"],
		"agent_state":       buildAgentLifecycleState(parsed, profile, "working"),
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
	return c.emit(adapterResponse("bootstrap", ok, firstString(raw["agent"], agent), firstString(raw["agent_id"], payload["agent_id"]), parsed.string("project", "contextlattice"), sessionID, map[string]any{"preflight": compactPreflightForAdapter(raw), "agent_state": payload["agent_state"], "ownership": adapterOwnership(parsed)}, findings), parsed.bool("pretty"))
}

func adapterStringFlags() map[string]string {
	return mergeStringFlags(commonStringFlags(), contextPackTokenBudgetStringFlags(), map[string]string{
		"agent":                          "agent",
		"agent-id":                       "agent_id",
		"session-id":                     "session_id",
		"mission":                        "mission",
		"objective":                      "objective",
		"goal":                           "goal",
		"query":                          "query",
		"limit":                          "limit",
		"max-facts":                      "max_facts",
		"summary":                        "summary",
		"next-action":                    "next_action",
		"file":                           "file",
		"content":                        "content",
		"metadata-json":                  "metadata_json",
		"status":                         "status",
		"state":                          "state",
		"authority":                      "authority",
		"source":                         "source",
		"ttl-seconds":                    "ttl_seconds",
		"task-id":                        "task_id",
		"repo":                           "repo",
		"branch":                         "branch",
		"worktree":                       "worktree",
		"cwd":                            "cwd",
		"native-session-id":              "native_session_id",
		"needs-user":                     "needs_user",
		"blocked-by":                     "blocked_by",
		"context-pack-quality-sample-id": "context_pack_quality_sample_id",
		"sample-id":                      "context_pack_quality_sample_id",
		"first-pass-success":             "first_pass_success",
		"succeeded-first-pass":           "first_pass_success",
		"repair-required":                "repair_required",
		"retry-count":                    "retry_count",
		"retries":                        "retry_count",
		"followup-tokens":                "followup_tokens",
		"provider-prompt-tokens":         "provider_prompt_tokens",
		"provider-completion-tokens":     "provider_completion_tokens",
		"provider-total-tokens":          "provider_total_tokens",
		"provider-usage-json":            "provider_usage_json",
		"task-class":                     "task_class",
		"outcome-source":                 "outcome_source",
	})
}

func adapterBoolFlags() map[string]string {
	return mergeBoolFlags(commonBoolFlags(), map[string]string{
		"include-retrieval-debug":     "include_retrieval_debug",
		"stdin":                       "stdin",
		"strict":                      "strict",
		"report-context-pack-outcome": "report_context_pack_outcome",
	})
}

type adapterProfile struct {
	agent          string
	agentID        string
	topicPath      string
	query          string
	mode           string
	stateAuthority string
	processNames   []string
	profile        map[string]any
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
	stateAuthority := normalizeAgentStateAuthority(parsed.string("authority", firstString(profile["state_authority"], "self_report")))
	processNames := anyToStringList(firstList(profile["process_names"], profile["processNames"], profile["surfaces"]), 16)
	if len(processNames) == 0 {
		processNames = defaultAgentProcessNames(agent)
	}
	return adapterProfile{agent: agent, agentID: agentID, topicPath: topicPath, query: query, mode: mode, stateAuthority: stateAuthority, processNames: processNames, profile: profile}
}

func contextPackQualitySample(payload map[string]any) map[string]any {
	if quality := asMap(payload["context_pack_quality"]); len(quality) > 0 {
		return quality
	}
	pack := asMap(payload["context_pack"])
	if quality := asMap(firstMap(pack["context_pack_quality"], pack["contextPackQuality"])); len(quality) > 0 {
		return quality
	}
	return map[string]any{}
}

func contextPackQualitySampleID(payload map[string]any) string {
	return firstString(contextPackQualitySample(payload)["sample_id"])
}

func contextPackOutcomeReport(sessionID string, sampleID string) map[string]any {
	if strings.TrimSpace(sampleID) == "" {
		return map[string]any{}
	}
	return map[string]any{
		"schema_id": "contextlattice_context_pack_outcome_report.v1",
		"sample_id": sampleID,
		"endpoint":  "/telemetry/context-pack-quality/outcome",
		"command":   "contextlattice_agent_adapter outcome --session-id " + shellQuote(sessionID) + " --context-pack-quality-sample-id " + shellQuote(sampleID) + " --first-pass-success true --repair-required false --retry-count 0",
		"fields":    []any{"first_pass_success", "repair_required", "retry_count", "followup_tokens", "provider_prompt_tokens", "provider_completion_tokens", "provider_total_tokens"},
		"privacy":   "stores compact counters only; do not send prompts, completions, source text, or secrets",
	}
}

func recordContextPackQualityPending(project, sessionID, objective, agentID string, quality map[string]any) {
	sampleID := firstString(quality["sample_id"])
	if sampleID == "" || sessionID == "" {
		return
	}
	writeSessionStateWithExtras(project, sessionID, objective, agentID, map[string]any{
		"latest_context_pack_quality": map[string]any{
			"sample_id":     sampleID,
			"query_hash":    quality["query_hash"],
			"quality_score": quality["quality_score"],
			"captured_at":   firstString(quality["capturedAt"], quality["captured_at"], time.Now().UTC().Format(time.RFC3339)),
		},
	})
}

func resolvePendingContextPackQualitySampleID(parsed parsedArgs, project string) string {
	if sampleID := parsed.string("context_pack_quality_sample_id", ""); sampleID != "" {
		return sampleID
	}
	if sampleID := envString("CONTEXTLATTICE_CONTEXT_PACK_QUALITY_SAMPLE_ID", ""); sampleID != "" {
		return sampleID
	}
	state := readSessionState(project)
	quality := asMap(state["latest_context_pack_quality"])
	return firstString(quality["sample_id"])
}

func (c *cli) ensureAdapterSession(parsed parsedArgs, project, objective, agentID string) (string, error) {
	sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", ""))
	if sessionID != "" {
		return sessionID, nil
	}
	profile := resolveAdapterProfile(parsed)
	sessionID = c.ensureSessionForAgent(project, objective, profile.agent, agentID, adapterOwnership(parsed), profile, parsed.float("timeout", 30))
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
		"schema_id":                  "contextlattice_universal_agent_adapter.v1",
		"version":                    "2026-06-30",
		"required_phases":            []any{"preflight", "auto_session", "agent_state", "context_pack", "context_pack_outcome", "checkpoint", "handoff", "completion"},
		"preflight_route":            "/v1/agents/preflight",
		"event_route":                "/v1/agents/sessions/event",
		"context_pack_route":         "/memory/context-pack",
		"context_pack_outcome_route": "/telemetry/context-pack-quality/outcome",
		"checkpoint_route":           "/memory/write",
		"agent_lifecycle":            defaultAgentLifecycleContract(),
		"ownership_fields":           []any{"session_id", "agent", "agent_id", "task_id", "repo", "worktree", "branch", "cwd", "native_session_id"},
	}
}

func defaultAgentLifecycleContract() map[string]any {
	return map[string]any{
		"schema_id":          "contextlattice_agent_lifecycle_state.v1",
		"states":             []any{"idle", "working", "awaiting_user", "blocked", "done"},
		"authorities":        []any{"hook", "plugin", "self_report", "process_probe", "manual", "none"},
		"state_route":        "/v1/agents/sessions/event",
		"discovery_command":  "contextlattice_agent_discover --pretty",
		"adapter_command":    "contextlattice_agent_adapter state --state working --pretty",
		"separate_lifecycle": "agent_state is the semantic agent lifecycle; retrieval_lifecycle remains source-fetch progress only",
	}
}

func normalizeAgentLifecycleState(raw string) string {
	switch strings.TrimSpace(strings.ToLower(strings.ReplaceAll(raw, "-", "_"))) {
	case "idle", "ready", "standby":
		return "idle"
	case "working", "running", "active", "started", "busy":
		return "working"
	case "awaiting_user", "awaiting", "waiting", "paused", "needs_user", "need_user", "approval":
		return "awaiting_user"
	case "blocked", "stuck", "failed", "error":
		return "blocked"
	case "done", "completed", "complete", "succeeded", "success":
		return "done"
	default:
		return "working"
	}
}

func normalizeAgentStateAuthority(raw string) string {
	switch strings.TrimSpace(strings.ToLower(strings.ReplaceAll(raw, "-", "_"))) {
	case "hook", "plugin", "self_report", "process_probe", "manual", "none":
		return strings.TrimSpace(strings.ToLower(strings.ReplaceAll(raw, "-", "_")))
	case "process", "probe", "ps":
		return "process_probe"
	case "self", "agent", "agent_report":
		return "self_report"
	default:
		return "self_report"
	}
}

func sessionStatusForAgentState(state string) string {
	switch normalizeAgentLifecycleState(state) {
	case "awaiting_user":
		return "paused"
	case "blocked":
		return "blocked"
	case "done":
		return "completed"
	default:
		return "active"
	}
}

func buildAgentLifecycleState(parsed parsedArgs, profile adapterProfile, fallbackState string) map[string]any {
	state := normalizeAgentLifecycleState(parsed.string("state", fallbackState))
	authority := normalizeAgentStateAuthority(parsed.string("authority", profile.stateAuthority))
	if authority == "" {
		authority = "self_report"
	}
	ttl := parsed.int("ttl_seconds", 0)
	out := map[string]any{
		"schema_id":  "contextlattice_agent_lifecycle_state.v1",
		"state":      state,
		"authority":  authority,
		"source":     parsed.string("source", "contextlattice_agent_adapter"),
		"updated_at": time.Now().UTC().Format(time.RFC3339),
	}
	if ttl > 0 {
		out["ttl_seconds"] = ttl
		out["expires_at"] = time.Now().UTC().Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
	}
	for key, value := range adapterOwnership(parsed) {
		if firstString(value) != "" {
			out[key] = value
		}
	}
	if needsUser := parsed.string("needs_user", ""); needsUser != "" {
		out["needs_user"] = needsUser
	}
	if blockedBy := parsed.string("blocked_by", ""); blockedBy != "" {
		out["blocked_by"] = blockedBy
	}
	return out
}

func adapterOwnership(parsed parsedArgs) map[string]any {
	cwd := parsed.string("cwd", currentWorkingDir())
	worktree := parsed.string("worktree", gitValueInDir(cwd, "rev-parse", "--show-toplevel"))
	if worktree == "" {
		worktree = cwd
	}
	repo := parsed.string("repo", gitValueInDir(worktree, "config", "--get", "remote.origin.url"))
	branch := parsed.string("branch", gitValueInDir(worktree, "branch", "--show-current"))
	return dropEmpty(map[string]any{
		"task_id":           parsed.string("task_id", ""),
		"repo":              repo,
		"branch":            branch,
		"worktree":          worktree,
		"cwd":               cwd,
		"native_session_id": parsed.string("native_session_id", ""),
	})
}

func mergeAdapterMetadata(parsed parsedArgs, profile adapterProfile, fallbackState string) map[string]any {
	metadata := parseJSONObject(parsed.string("metadata_json", ""))
	state := buildAgentLifecycleState(parsed, profile, fallbackState)
	ownership := adapterOwnership(parsed)
	if len(state) > 0 {
		metadata["agent_state"] = state
	}
	if len(ownership) > 0 {
		metadata["ownership"] = ownership
	}
	metadata["state_authority"] = state["authority"]
	metadata["adapter"] = firstNonEmpty(firstString(metadata["adapter"]), "contextlattice-agent-adapter")
	metadata["go_native_cli"] = true
	return metadata
}

func defaultAgentProcessNames(agent string) []string {
	switch strings.TrimSpace(strings.ToLower(agent)) {
	case "codex":
		return []string{"codex"}
	case "claude-code":
		return []string{"claude"}
	case "opencode":
		return []string{"opencode"}
	case "hermes-agent":
		return []string{"hermes-agent", "hermes"}
	case "hermes-ultra":
		return []string{"hermes-ultra", "hermes-agent-ultra"}
	case "pi":
		return []string{"pi"}
	case "droid":
		return []string{"droid"}
	case "chatgpt-desktop":
		return []string{"ChatGPT", "chatgpt"}
	case "claude-desktop":
		return []string{"Claude", "claude"}
	default:
		return []string{agent}
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
		"task_id":                   parsed.string("task_id", ""),
		"agent_state":               buildAgentLifecycleState(parsed, profile, "working"),
		"ownership":                 adapterOwnership(parsed),
		"traffic_class":             "user",
		"native_cli_implementation": true,
	})
	addContextPackTokenBudgetArgs(request, parsed)
	contextPack, _, err := c.requestJSON(context.Background(), http.MethodPost, "/memory/context-pack", request, parsed.float("timeout", 30))
	if err != nil {
		return err
	}
	qualitySample := contextPackQualitySample(contextPack)
	qualitySampleID := firstString(qualitySample["sample_id"])
	recordContextPackQualityPending(project, sessionID, profile.query, profile.agentID, qualitySample)
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
			"adapter":                        "contextlattice-agent-adapter",
			"topic_path":                     request["topic_path"],
			"retrieval_mode":                 profile.mode,
			"contract_ok":                    len(findings) == 0,
			"go_native_cli":                  true,
			"context_pack_ref":               firstString(contextPack["schema_id"], "context_pack_response.v1"),
			"context_pack_quality_sample_id": qualitySampleID,
			"agent_state":                    request["agent_state"],
			"ownership":                      request["ownership"],
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
		"agent_state":    request["agent_state"],
		"ownership":      request["ownership"],
		"context_pack":   contextPack,
		"outcome_report": contextPackOutcomeReport(sessionID, qualitySampleID),
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
		"task_id":     parsed.string("task_id", ""),
		"repo":        adapterOwnership(parsed)["repo"],
		"branch":      adapterOwnership(parsed)["branch"],
		"worktree":    adapterOwnership(parsed)["worktree"],
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
			"agent_state":   buildAgentLifecycleState(parsed, profile, "working"),
			"ownership":     adapterOwnership(parsed),
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
		"agent_state": buildAgentLifecycleState(parsed, profile, "working"),
		"ownership":   adapterOwnership(parsed),
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
			"agent_state":   handoff["agent_state"],
			"ownership":     handoff["ownership"],
		},
	}, parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	ok := len(findings) == 0 && (len(event) == 0 || asBool(event["ok"]))
	return c.emit(adapterResponse("handoff", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{
		"handoff": handoff,
		"event":   event,
	}, findings), parsed.bool("pretty"))
}

func (c *cli) adapterState(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter state --state working --session-id <id> [--task-id <id>] [--needs-user text] [--blocked-by text] --pretty")
	}
	c.applyBaseURL(parsed)
	project := parsed.string("project", "contextlattice")
	profile := resolveAdapterProfile(parsed)
	state := buildAgentLifecycleState(parsed, profile, "working")
	sessionID, err := c.ensureAdapterSession(parsed, project, parsed.string("objective", "agent state "+firstString(state["state"])), profile.agentID)
	if err != nil {
		return err
	}
	summary := parsed.string("summary", "")
	if summary == "" {
		switch firstString(state["state"]) {
		case "awaiting_user":
			summary = firstNonEmpty(firstString(state["needs_user"]), "agent is awaiting user input")
		case "blocked":
			summary = firstNonEmpty(firstString(state["blocked_by"]), "agent is blocked")
		case "done":
			summary = "agent completed assigned work"
		case "idle":
			summary = "agent is idle"
		default:
			summary = "agent is working"
		}
	}
	metadata := mergeAdapterMetadata(parsed, profile, firstString(state["state"]))
	eventType := "agent.state." + firstString(state["state"])
	event, _, eventErr := c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", map[string]any{
		"session_id": sessionID,
		"agent":      profile.agent,
		"agent_id":   profile.agentID,
		"project":    project,
		"type":       eventType,
		"summary":    summary,
		"status":     sessionStatusForAgentState(firstString(state["state"])),
		"metadata":   metadata,
	}, parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	ok := len(findings) == 0 && asBool(event["ok"])
	if err := c.emit(adapterResponse("state", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{
		"agent_state": state,
		"ownership":   adapterOwnership(parsed),
		"event":       event,
	}, findings), parsed.bool("pretty")); err != nil {
		return err
	}
	return nil
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
		"status":     firstNonEmpty(parsed.string("status", ""), sessionStatusForAgentState(parsed.string("state", "working"))),
		"metadata":   mergeAdapterMetadata(parsed, profile, parsed.string("state", "working")),
	}), parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	ok := len(findings) == 0 && asBool(event["ok"])
	return c.emit(adapterResponse("event", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{"event": event}, findings), parsed.bool("pretty"))
}

func resolveOutcomeSessionID(parsed parsedArgs, project string) string {
	if sessionID := parsed.string("session_id", envString("CONTEXTLATTICE_SESSION_ID", "")); sessionID != "" {
		return sessionID
	}
	return firstString(readSessionState(project)["session_id"])
}

func buildContextPackOutcomePayload(parsed parsedArgs, project, sessionID string, profile adapterProfile, source string) (map[string]any, bool, error) {
	sampleID := resolvePendingContextPackQualitySampleID(parsed, project)
	if sampleID == "" {
		return nil, false, errors.New("context pack quality sample id is required; run contextlattice_agent_adapter context-pack first or pass --context-pack-quality-sample-id")
	}
	firstPass, firstPassPresent, err := parsed.boolString("first_pass_success")
	if err != nil {
		return nil, false, err
	}
	repairRequired, repairPresent, err := parsed.boolString("repair_required")
	if err != nil {
		return nil, false, err
	}
	providerUsage := parseJSONObject(parsed.string("provider_usage_json", ""))
	payload := dropEmpty(map[string]any{
		"sample_id":                      sampleID,
		"context_pack_quality_sample_id": sampleID,
		"session_id":                     sessionID,
		"agent":                          profile.agent,
		"agent_id":                       profile.agentID,
		"project":                        project,
		"task_class":                     parsed.string("task_class", "agent_workflow"),
		"outcome_source":                 parsed.string("outcome_source", source),
		"retry_count":                    parsed.int("retry_count", 0),
		"followup_tokens":                parsed.int("followup_tokens", 0),
		"provider_prompt_tokens":         parsed.int("provider_prompt_tokens", 0),
		"provider_completion_tokens":     parsed.int("provider_completion_tokens", 0),
		"provider_total_tokens":          parsed.int("provider_total_tokens", 0),
		"provider_usage":                 providerUsage,
	})
	if firstPassPresent {
		payload["first_pass_success"] = firstPass
	}
	if repairPresent {
		payload["repair_required"] = repairRequired
	}
	hasSignal := firstPassPresent ||
		repairPresent ||
		parsed.int("retry_count", 0) > 0 ||
		parsed.int("followup_tokens", 0) > 0 ||
		parsed.int("provider_prompt_tokens", 0) > 0 ||
		parsed.int("provider_completion_tokens", 0) > 0 ||
		parsed.int("provider_total_tokens", 0) > 0 ||
		len(providerUsage) > 0
	if !hasSignal {
		return nil, false, errors.New("outcome requires at least one explicit outcome or usage signal")
	}
	return payload, true, nil
}

func contextPackOutcomeRequested(parsed parsedArgs) bool {
	if parsed.bool("report_context_pack_outcome") {
		return true
	}
	for _, key := range []string{
		"context_pack_quality_sample_id",
		"first_pass_success",
		"repair_required",
		"retry_count",
		"followup_tokens",
		"provider_prompt_tokens",
		"provider_completion_tokens",
		"provider_total_tokens",
		"provider_usage_json",
	} {
		if parsed.has(key) {
			return true
		}
	}
	return false
}

func compactOutcomeMetadata(outcome map[string]any) map[string]any {
	return dropEmpty(map[string]any{
		"schema_id":                  firstString(outcome["schema_id"], "contextlattice_context_pack_outcome.v1"),
		"sample_id":                  outcome["sample_id"],
		"task_class":                 outcome["task_class"],
		"first_pass_success":         outcome["first_pass_success"],
		"repair_required":            outcome["repair_required"],
		"retry_count":                outcome["retry_count"],
		"observed_followup_tokens":   outcome["observed_followup_tokens"],
		"provider_prompt_tokens":     outcome["provider_prompt_tokens"],
		"provider_completion_tokens": outcome["provider_completion_tokens"],
		"provider_total_tokens":      outcome["provider_total_tokens"],
		"outcome_source":             outcome["outcome_source"],
	})
}

func (c *cli) postContextPackOutcome(parsed parsedArgs, project, sessionID string, profile adapterProfile, source string) (map[string]any, map[string]any, []map[string]any) {
	payload, _, err := buildContextPackOutcomePayload(parsed, project, sessionID, profile, source)
	if err != nil {
		return map[string]any{}, map[string]any{}, errorFinding(err)
	}
	raw, _, postErr := c.requestJSON(context.Background(), http.MethodPost, "/telemetry/context-pack-quality/outcome", payload, parsed.float("timeout", 10))
	findings := errorFinding(postErr)
	event := map[string]any{}
	outcome := asMap(raw["outcome"])
	if postErr == nil && sessionID != "" {
		var postEventErr error
		event, _, postEventErr = c.requestJSON(context.Background(), http.MethodPost, "/v1/agents/sessions/event", map[string]any{
			"session_id": sessionID,
			"agent":      profile.agent,
			"agent_id":   profile.agentID,
			"project":    project,
			"type":       "context_pack.outcome_reported",
			"summary":    "context pack outcome reported",
			"metadata": map[string]any{
				"adapter":       "contextlattice-agent-adapter",
				"go_native_cli": true,
				"outcome":       compactOutcomeMetadata(outcome),
			},
		}, parsed.float("timeout", 10))
		findings = append(findings, errorFinding(postEventErr)...)
	}
	return raw, event, findings
}

func (c *cli) adapterOutcome(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter outcome --session-id <id> --context-pack-quality-sample-id <id> --first-pass-success true|false --repair-required true|false [--retry-count n] [--provider-total-tokens n] --pretty")
	}
	c.applyBaseURL(parsed)
	project := parsed.string("project", "contextlattice")
	profile := resolveAdapterProfile(parsed)
	sessionID := resolveOutcomeSessionID(parsed, project)
	raw, event, findings := c.postContextPackOutcome(parsed, project, sessionID, profile, "adapter_outcome")
	ok := len(findings) == 0 && asBool(raw["ok"])
	if err := c.emit(adapterResponse("outcome", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{
		"outcome":   raw["outcome"],
		"telemetry": raw["telemetry"],
		"event":     event,
	}, findings), parsed.bool("pretty")); err != nil {
		return err
	}
	if !ok {
		return errors.New("context pack outcome report failed")
	}
	return nil
}

func (c *cli) adapterComplete(args []string) error {
	parsed := parseArgs(args, adapterStringFlags(), adapterBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_adapter complete --session-id <id> --summary '<result>' [--first-pass-success true|false --repair-required true|false --retry-count n] --pretty")
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
		"metadata":   mergeAdapterMetadata(parsed, profile, "done"),
	}, parsed.float("timeout", 10))
	findings := errorFinding(eventErr)
	outcomeResult := map[string]any{}
	outcomeEvent := map[string]any{}
	if contextPackOutcomeRequested(parsed) {
		var outcomeFindings []map[string]any
		outcomeResult, outcomeEvent, outcomeFindings = c.postContextPackOutcome(parsed, project, sessionID, profile, "adapter_complete")
		findings = append(findings, outcomeFindings...)
	}
	ok := len(findings) == 0 && asBool(event["ok"])
	if err := c.emit(adapterResponse("complete", ok, profile.agent, profile.agentID, project, sessionID, map[string]any{"event": event, "outcome": outcomeResult["outcome"], "outcome_event": outcomeEvent}, findings), parsed.bool("pretty")); err != nil {
		return err
	}
	return nil
}

func (c *cli) cmdDiscover(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"agents":      "agents",
		"repo":        "repo",
		"global-home": "global_home",
		"ps-fixture":  "ps_fixture",
		"limit":       "limit",
	}), commonBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_agent_discover --agents codex,claude-code --repo . --pretty")
	}
	globalHome := parsed.string("global_home", envString("CONTEXTLATTICE_GLOBAL_HOME", filepath.Join(homeDir(), ".contextlattice")))
	profiles := loadAgentProfilesFromGlobalHome(globalHome)
	names := splitCSV(parsed.string("agents", ""))
	if len(names) == 0 {
		for name := range profiles {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	psText, psErr := readProcessSnapshot(parsed.string("ps_fixture", ""), parsed.float("timeout", 2))
	repo := parsed.string("repo", "")
	absRepo := ""
	if repo != "" {
		if resolved, err := filepath.Abs(repo); err == nil {
			absRepo = resolved
		}
	}
	agents := []map[string]any{}
	findings := []map[string]any{}
	for _, name := range names {
		profile := asMap(profiles[name])
		resolved := adapterProfile{
			agent:          name,
			agentID:        firstString(profile["agent_id"], strings.ReplaceAll(name, "-", "_")+"_agent"),
			stateAuthority: normalizeAgentStateAuthority(firstString(profile["state_authority"], "self_report")),
			processNames:   anyToStringList(firstList(profile["process_names"], profile["processNames"], profile["surfaces"]), 16),
			profile:        profile,
		}
		if len(resolved.processNames) == 0 {
			resolved.processNames = defaultAgentProcessNames(name)
		}
		processes := discoverAgentProcesses(psText, resolved.processNames, parsed.int("limit", 8))
		stateValue := "idle"
		stateSource := "profile"
		if len(processes) > 0 {
			stateValue = "working"
			stateSource = "process_probe"
		}
		hook := agentHookEvidence(name)
		integration := map[string]any{
			"profile_present":        len(profile) > 0,
			"adapter_tool":           executableExists(filepath.Join(globalHome, "bin", "contextlattice_agent_adapter")),
			"discover_tool":          executableExists(filepath.Join(globalHome, "bin", "contextlattice_agent_discover")),
			"state_authority":        resolved.stateAuthority,
			"hook":                   hook,
			"repo_instruction_check": map[string]any{"ok": false, "reason": "repo_not_requested"},
		}
		if runner := runnerDiscoveryMetadata(name, absRepo); len(runner) > 0 {
			integration["runner"] = runner
			integration["install_hint"] = runner["install_hint"]
		}
		if absRepo != "" {
			audit := auditRepoIntegration(absRepo, []string{name})
			integration["repo_instruction_check"] = map[string]any{
				"ok":       asBool(audit["ok"]),
				"repo":     absRepo,
				"findings": audit["findings"],
			}
		}
		explanation := agentDiscoveryExplanation(name, stateValue, stateSource, processes, integration)
		agents = append(agents, map[string]any{
			"agent":            name,
			"agent_id":         resolved.agentID,
			"state_authority":  resolved.stateAuthority,
			"process_patterns": resolved.processNames,
			"agent_state": map[string]any{
				"schema_id": "contextlattice_agent_lifecycle_state.v1",
				"state":     stateValue,
				"authority": "process_probe",
				"source":    stateSource,
			},
			"process_count": len(processes),
			"processes":     processes,
			"integration":   integration,
			"explanation":   explanation,
		})
		if !asBool(integration["adapter_tool"]) {
			findings = append(findings, map[string]any{"reason": "adapter_tool_missing", "agent": name, "path": filepath.Join(globalHome, "bin", "contextlattice_agent_adapter")})
		}
	}
	return c.emit(map[string]any{
		"ok":                 len(findings) == 0,
		"schema_id":          "contextlattice_agent_discovery.v1",
		"lifecycle_contract": defaultAgentLifecycleContract(),
		"global_home":        globalHome,
		"repo":               absRepo,
		"process_probe": map[string]any{
			"ok":           psErr == nil,
			"source":       "ps",
			"best_effort":  true,
			"error":        errString(psErr),
			"privacy_note": "cwd/worktree evidence is best-effort and may be unavailable under OS privacy controls",
		},
		"agents": agents,
		"findings": append(findings, func() []map[string]any {
			if psErr == nil {
				return nil
			}
			return []map[string]any{{"reason": "process_snapshot_unavailable", "severity": "warning", "detail": truncate(psErr.Error(), 500)}}
		}()...),
	}, parsed.bool("pretty"))
}

func readProcessSnapshot(fixture string, timeout float64) (string, error) {
	if strings.TrimSpace(fixture) != "" {
		data, err := os.ReadFile(fixture)
		return string(data), err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(maxFloat(timeout, 1))*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,comm=,args=")
	raw, err := cmd.Output()
	return string(raw), err
}

func discoverAgentProcesses(psText string, patterns []string, limit int) []any {
	if limit < 1 {
		limit = 8
	}
	out := []any{}
	for _, raw := range strings.Split(psText, "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 3 {
			continue
		}
		pid := fields[0]
		ppid := fields[1]
		command := fields[2]
		args := ""
		if len(fields) > 3 {
			args = strings.Join(fields[3:], " ")
		}
		if agentProcessIgnored(command, args) {
			continue
		}
		if !agentProcessMatches(command, args, patterns) {
			continue
		}
		cwd := processCWD(pid)
		worktree := ""
		branch := ""
		repo := ""
		if cwd != "" {
			worktree = gitValueInDir(cwd, "rev-parse", "--show-toplevel")
			if worktree != "" {
				branch = gitValueInDir(worktree, "branch", "--show-current")
				repo = gitValueInDir(worktree, "config", "--get", "remote.origin.url")
			}
		}
		out = append(out, dropEmpty(map[string]any{
			"pid":      pid,
			"ppid":     ppid,
			"command":  command,
			"args":     truncate(args, 360),
			"cwd":      cwd,
			"worktree": worktree,
			"branch":   branch,
			"repo":     repo,
		}))
		if len(out) >= limit {
			break
		}
	}
	return out
}

func agentProcessIgnored(command, args string) bool {
	lower := strings.ToLower(command + " " + args)
	for _, pattern := range []string{
		"crashpad",
		"node_repl",
		"skycomputeruseclient",
		"kickbacks-codex-cli-agent",
		"contextlattice_agent_discover",
		"contextlattice-agent-tools discover",
		"go run ./cmd/contextlattice-agent-tools discover",
	} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

func agentProcessMatches(command, args string, patterns []string) bool {
	identities := agentProcessIdentities(command, args)
	if len(identities) == 0 {
		return false
	}
	for _, pattern := range patterns {
		p := normalizeProcessIdentity(pattern)
		if p == "" {
			continue
		}
		for _, identity := range identities {
			if identity == p {
				return true
			}
		}
	}
	return false
}

func agentProcessIdentities(command, args string) []string {
	identities := []string{}
	add := func(value string) {
		identity := normalizeProcessIdentity(value)
		if identity != "" {
			identities = append(identities, identity)
		}
	}
	add(command)
	tokens := strings.Fields(args)
	if len(tokens) == 0 {
		return uniqueStrings(identities)
	}
	add(tokens[0])
	identities = append(identities, executableTargetIdentities(tokens)...)
	return uniqueStrings(identities)
}

func executableTargetIdentities(tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	name := normalizeProcessIdentity(tokens[0])
	if name == "" {
		return nil
	}
	if isShellProcessName(name) {
		return nil
	}
	if isEnvProcessName(name) {
		return executableTargetIdentities(skipEnvArgs(tokens[1:]))
	}
	if isInterpreterProcessName(name) {
		return interpreterTargetIdentities(tokens[1:])
	}
	if isPackageRunnerProcessName(name) {
		return packageRunnerTargetIdentities(name, tokens[1:])
	}
	return []string{name}
}

func interpreterTargetIdentities(tokens []string) []string {
	for i := 0; i < len(tokens); i++ {
		token := strings.TrimSpace(tokens[i])
		if token == "" {
			continue
		}
		if token == "-m" && i+1 < len(tokens) {
			return moduleTargetIdentities(tokens[i+1])
		}
		if strings.HasPrefix(token, "-") {
			if interpreterOptionTakesValue(token) && i+1 < len(tokens) {
				i++
			}
			continue
		}
		return []string{normalizeProcessIdentity(token)}
	}
	return nil
}

func moduleTargetIdentities(module string) []string {
	module = strings.Trim(strings.TrimSpace(module), `"'`)
	module = strings.TrimSuffix(module, ":")
	module = strings.TrimSpace(module)
	if module == "" {
		return nil
	}
	parts := strings.Split(module, ".")
	first := ""
	if len(parts) > 0 {
		first = normalizeProcessIdentity(parts[0])
	}
	full := normalizeProcessIdentity(strings.ReplaceAll(module, ".", "-"))
	out := []string{full, first}
	for _, value := range []string{full, first} {
		if strings.HasSuffix(value, "-cli") {
			out = append(out, strings.TrimSuffix(value, "-cli"))
		}
	}
	return uniqueStrings(out)
}

func packageRunnerTargetIdentities(runner string, tokens []string) []string {
	if len(tokens) == 0 {
		return nil
	}
	i := 0
	if runner == "pipx" && i < len(tokens) && tokens[i] == "run" {
		i++
	}
	if (runner == "pnpm" || runner == "yarn") && i < len(tokens) && (tokens[i] == "dlx" || tokens[i] == "exec") {
		i++
	}
	for i < len(tokens) {
		token := strings.TrimSpace(tokens[i])
		if token == "" {
			i++
			continue
		}
		if token == "--" {
			i++
			continue
		}
		if strings.HasPrefix(token, "-") {
			if packageRunnerOptionTakesValue(token) && i+1 < len(tokens) {
				i += 2
			} else {
				i++
			}
			continue
		}
		return []string{normalizeProcessIdentity(token)}
	}
	return nil
}

func skipEnvArgs(tokens []string) []string {
	for len(tokens) > 0 {
		token := strings.TrimSpace(tokens[0])
		if token == "" {
			tokens = tokens[1:]
			continue
		}
		if token == "-S" && len(tokens) > 1 {
			return tokens[1:]
		}
		if strings.HasPrefix(token, "-") || strings.Contains(token, "=") {
			tokens = tokens[1:]
			continue
		}
		return tokens
	}
	return nil
}

func normalizeProcessIdentity(value string) string {
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	value = strings.TrimSuffix(value, ":")
	if value == "" {
		return ""
	}
	base := filepath.Base(value)
	base = strings.Trim(strings.TrimSpace(base), `"'`)
	base = strings.ToLower(base)
	base = strings.ReplaceAll(base, "_", "-")
	for _, suffix := range []string{".exe", ".cmd", ".bat", ".js", ".mjs", ".cjs", ".py", ".pyw", ".rb", ".pl", ".sh"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base
}

func isShellProcessName(name string) bool {
	switch name {
	case "sh", "bash", "zsh", "fish", "dash", "csh", "tcsh", "ksh", "pwsh", "powershell", "osascript":
		return true
	default:
		return false
	}
}

func isEnvProcessName(name string) bool {
	return name == "env" || name == "arch"
}

func isInterpreterProcessName(name string) bool {
	return name == "python" ||
		name == "pythonw" ||
		name == "pypy" ||
		name == "node" ||
		name == "nodejs" ||
		name == "deno" ||
		name == "bun" ||
		name == "ruby" ||
		name == "perl" ||
		strings.HasPrefix(name, "python") ||
		strings.HasPrefix(name, "pypy")
}

func isPackageRunnerProcessName(name string) bool {
	switch name {
	case "uvx", "pipx", "npx", "pnpm", "yarn", "bunx", "mise", "asdf":
		return true
	default:
		return false
	}
}

func interpreterOptionTakesValue(option string) bool {
	switch option {
	case "-c", "-W", "-X", "-Q":
		return true
	default:
		return strings.HasPrefix(option, "--check-hash-based-pycs")
	}
}

func packageRunnerOptionTakesValue(option string) bool {
	if strings.Contains(option, "=") {
		return false
	}
	switch option {
	case "--from", "--python", "--with", "--with-editable", "--index-url", "--extra-index-url", "--find-links", "--resolution", "--prerelease", "--exclude-newer", "--keyring-provider", "--config-setting", "--project", "--directory", "--package", "-p", "--package-manager":
		return true
	default:
		return false
	}
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func runnerDiscoveryMetadata(agent string, repo string) map[string]any {
	name := strings.TrimSpace(strings.ToLower(agent))
	var commands []string
	var hint string
	var adapterRel string
	switch name {
	case "pi":
		commands = []string{"pi"}
		hint = "brew install pi-coding-agent"
		adapterRel = filepath.Join("scripts", "agent_runners", "pi_runner.py")
	case "droid":
		commands = []string{"droid"}
		hint = "brew install --cask droid"
		adapterRel = filepath.Join("scripts", "agent_runners", "droid_runner.py")
	default:
		return map[string]any{}
	}
	detected := []string{}
	for _, command := range commands {
		if _, err := exec.LookPath(command); err == nil {
			detected = append(detected, command)
		}
	}
	adapter := runnerArtifactPath(repo, adapterRel)
	adapterPresent := executableExists(adapter)
	contractPresent := runnerCapabilityContractPresent(repo)
	commandState := "missing"
	if len(detected) > 0 {
		commandState = "detected"
	}
	return map[string]any{
		"profile":                    name,
		"commands":                   detected,
		"command_state":              commandState,
		"install_hint":               hint,
		"adapter":                    adapter,
		"adapter_present":            adapterPresent,
		"runner_capability_contract": contractPresent,
		"runner_ready":               len(detected) > 0 && adapterPresent && contractPresent,
		"required_for_quickstart":    false,
	}
}

func runnerContextLatticeRoots(repo string) []string {
	candidates := []string{currentGitRoot()}
	if strings.TrimSpace(repo) != "" {
		if abs, err := filepath.Abs(repo); err == nil {
			candidates = append(candidates, abs)
		}
	}
	candidates = append(candidates, repoRoot(), ".")
	seen := map[string]bool{}
	out := []string{}
	for _, candidate := range candidates {
		clean := strings.TrimSpace(candidate)
		if clean == "" {
			continue
		}
		if abs, err := filepath.Abs(clean); err == nil {
			clean = abs
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

func runnerArtifactPath(repo string, rel string) string {
	roots := runnerContextLatticeRoots(repo)
	for _, root := range roots {
		path := filepath.Join(root, rel)
		if executableExists(path) {
			return path
		}
	}
	if len(roots) > 0 {
		return filepath.Join(roots[0], rel)
	}
	return rel
}

func runnerCapabilityContractPresent(repo string) bool {
	for _, root := range runnerContextLatticeRoots(repo) {
		path := filepath.Join(root, "config", "agent_contracts", "agent_output_contracts.json")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parsed := map[string]any{}
		if json.Unmarshal(data, &parsed) != nil {
			continue
		}
		contracts := asMap(parsed["contracts"])
		if _, ok := contracts["runner_capability.v1"]; ok {
			return true
		}
	}
	return false
}

func processCWD(pid string) string {
	if pid == "" {
		return ""
	}
	if target, err := os.Readlink(filepath.Join("/proc", pid, "cwd")); err == nil {
		return target
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "lsof", "-a", "-p", pid, "-d", "cwd", "-Fn")
	raw, err := cmd.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "n/") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

func agentHookEvidence(agent string) map[string]any {
	switch strings.TrimSpace(strings.ToLower(agent)) {
	case "codex":
		path := filepath.Join(homeDir(), ".codex", "hooks.json")
		data, err := os.ReadFile(path)
		ok := err == nil && strings.Contains(string(data), "contextlattice_agent_start")
		reason := "codex_hooks_json_contains_contextlattice_agent_start"
		if err != nil {
			reason = "codex_hooks_json_unreadable_or_missing"
		} else if !ok {
			reason = "codex_hooks_json_missing_contextlattice_agent_start"
		}
		return map[string]any{"ok": ok, "path": path, "reason": reason}
	default:
		return map[string]any{"ok": false, "reason": "no_known_native_hook_probe_for_profile"}
	}
}

func agentDiscoveryExplanation(agent, state, stateSource string, processes []any, integration map[string]any) string {
	parts := []string{}
	if len(processes) > 0 {
		parts = append(parts, fmt.Sprintf("%s appears %s because %d matching process(es) were found by %s", agent, state, len(processes), stateSource))
	} else {
		parts = append(parts, fmt.Sprintf("%s has no matching process evidence, so discovery reports %s until hooks or self-reporting update state", agent, state))
	}
	if hook := asMap(integration["hook"]); asBool(hook["ok"]) {
		parts = append(parts, "native hook evidence is present")
	}
	if repo := asMap(integration["repo_instruction_check"]); asBool(repo["ok"]) {
		parts = append(parts, "repo instruction block is present")
	}
	if !asBool(integration["adapter_tool"]) {
		parts = append(parts, "global adapter tool is missing or not executable")
	}
	if runner := asMap(integration["runner"]); len(runner) > 0 {
		if asBool(runner["runner_ready"]) {
			parts = append(parts, "runner adapter is ready")
		} else if hint := firstString(runner["install_hint"]); hint != "" {
			parts = append(parts, "runner adapter is optional and not ready; install hint: "+hint)
		}
	}
	return strings.Join(parts, "; ") + "."
}

func localAgentDiscoverySummary(globalHome string, names []string, repo string, limit int) map[string]any {
	profiles := loadAgentProfilesFromGlobalHome(globalHome)
	if len(names) == 0 {
		for name := range profiles {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	psText, psErr := readProcessSnapshot("", 1.5)
	agents := []any{}
	for _, name := range names {
		profile := asMap(profiles[name])
		processNames := anyToStringList(firstList(profile["process_names"], profile["processNames"], profile["surfaces"]), 16)
		if len(processNames) == 0 {
			processNames = defaultAgentProcessNames(name)
		}
		processes := discoverAgentProcesses(psText, processNames, limit)
		state := "idle"
		source := "profile"
		if len(processes) > 0 {
			state = "working"
			source = "process_probe"
		}
		integration := map[string]any{
			"profile_present": len(profile) > 0,
			"adapter_tool":    executableExists(filepath.Join(globalHome, "bin", "contextlattice_agent_adapter")),
			"discover_tool":   executableExists(filepath.Join(globalHome, "bin", "contextlattice_agent_discover")),
			"hook":            agentHookEvidence(name),
		}
		if runner := runnerDiscoveryMetadata(name, repo); len(runner) > 0 {
			integration["runner"] = runner
			integration["install_hint"] = runner["install_hint"]
		}
		if strings.TrimSpace(repo) != "" {
			if absRepo, err := filepath.Abs(repo); err == nil {
				audit := auditRepoIntegration(absRepo, []string{name})
				integration["repo_instruction_check"] = map[string]any{"ok": asBool(audit["ok"]), "repo": absRepo, "findings": audit["findings"]}
			}
		}
		agents = append(agents, map[string]any{
			"agent":           name,
			"state":           state,
			"state_source":    source,
			"process_count":   len(processes),
			"state_authority": normalizeAgentStateAuthority(firstString(profile["state_authority"], "self_report")),
			"integration":     integration,
			"explanation":     agentDiscoveryExplanation(name, state, source, processes, integration),
		})
	}
	return map[string]any{
		"ok":        true,
		"schema_id": "contextlattice_agent_discovery_summary.v1",
		"agents":    agents,
		"process_probe": map[string]any{
			"ok":    psErr == nil,
			"error": errString(psErr),
		},
	}
}

func doctorExplanations(checks []map[string]any) []any {
	out := []any{}
	for _, check := range checks {
		name := firstString(check["name"])
		explanation := firstString(check["explanation"])
		if name == "" || explanation == "" {
			continue
		}
		state := "passed"
		if !asBool(check["ok"]) {
			state = "failed"
		}
		out = append(out, map[string]any{
			"check":       name,
			"status":      state,
			"explanation": explanation,
		})
	}
	return out
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
	return loadAgentProfilesFromGlobalHome(envString("CONTEXTLATTICE_GLOBAL_HOME", filepath.Join(homeDir(), ".contextlattice")))
}

func loadAgentProfilesFromGlobalHome(globalHome string) map[string]any {
	if strings.TrimSpace(globalHome) == "" {
		globalHome = filepath.Join(homeDir(), ".contextlattice")
	}
	paths := []string{
		filepath.Join(globalHome, "config", "agents", "agent_profiles.json"),
		filepath.Join(repoRoot(), "config", "agents", "agent_profiles.json"),
	}
	if currentRoot := currentGitRoot(); currentRoot != "" {
		paths = append(paths, filepath.Join(currentRoot, "config", "agents", "agent_profiles.json"))
	}
	profiles := map[string]any{}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		parsed := map[string]any{}
		if json.Unmarshal(data, &parsed) != nil {
			continue
		}
		for key, value := range asMap(parsed["profiles"]) {
			profiles[key] = value
		}
	}
	return profiles
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

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func sessionStatePath(project string) string {
	root := filepath.Join(homeDir(), ".contextlattice", "agent_runtime_sessions")
	keyRaw := project + "|" + currentWorkingDir()
	hash := sha256.Sum256([]byte(keyRaw))
	return filepath.Join(root, hex.EncodeToString(hash[:])[:16]+".json")
}

func writeSessionState(project, sessionID, objective, agentID string) {
	writeSessionStateWithExtras(project, sessionID, objective, agentID, nil)
}

func writeSessionStateWithExtras(project, sessionID, objective, agentID string, extras map[string]any) {
	if sessionID == "" {
		return
	}
	path := sessionStatePath(project)
	_ = os.MkdirAll(filepath.Dir(path), 0700)
	payload := readSessionState(project)
	payload["session_id"] = sessionID
	payload["project"] = project
	payload["agent_id"] = agentID
	if strings.TrimSpace(objective) != "" {
		payload["objective"] = objective
	}
	payload["source"] = "go-native-cli"
	for key, value := range extras {
		payload[key] = value
	}
	payload["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	raw, _ := json.Marshal(payload)
	_ = os.WriteFile(path, append(raw, '\n'), 0600)
}

func readSessionState(project string) map[string]any {
	path := sessionStatePath(project)
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	payload := map[string]any{}
	if json.Unmarshal(data, &payload) != nil {
		return map[string]any{}
	}
	return payload
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

func anyToStringList(value any, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	items := []string{}
	add := func(raw any) {
		if len(items) >= limit {
			return
		}
		text := firstString(raw)
		if text == "" {
			return
		}
		items = append(items, text)
	}
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			add(item)
		}
	case []string:
		for _, item := range typed {
			add(item)
		}
	case string:
		for _, item := range splitCSV(typed) {
			add(item)
		}
	default:
		add(value)
	}
	return items
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
	return gitValueInDir(repoRoot(), args...)
}

func gitValueInDir(dir string, args ...string) string {
	if strings.TrimSpace(dir) == "" {
		dir = currentWorkingDir()
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
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
