package main

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/contextlattice/gateway-go/internal/calleridentity"
)

var threadUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Only the active, exactly identified thread may supply missing facts.
// Query bounded metadata, not transcripts, settings, or another user's history.
func codexThreadFacts(id string) (string, string) {
	if !threadUUID.MatchString(id) {
		return "", ""
	}
	root := firstNonEmpty(os.Getenv("CODEX_SQLITE_HOME"), os.Getenv("CODEX_HOME"))
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", ""
		}
		root = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(root) {
		return "", ""
	}
	// Prefer the latest numbered schema, never try older rows on a miss.
	paths, _ := fs.Glob(os.DirFS(root), "state_*.sqlite")
	best, db := -1, ""
	for _, path := range paths {
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "state_"), ".sqlite"))
		if err == nil && n >= 0 && n > best {
			best, db = n, filepath.Join(root, path)
		}
	}
	if db == "" {
		return "", ""
	}
	bin, err := exec.LookPath("sqlite3")
	if err != nil || !filepath.IsAbs(bin) {
		return "", ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	q := "SELECT model,coalesce(reasoning_effort,'') FROM threads WHERE id='" + id +
		"' AND archived=0 AND length(model) BETWEEN 1 AND 120 AND length(coalesce(reasoning_effort,''))<=16 LIMIT 1"
	raw, err := exec.CommandContext(ctx, bin, "-readonly", "-batch", "-init", os.DevNull, "-noheader", "-separator", "\t", db, q).Output()
	if err != nil || len(raw) > 150 {
		return "", ""
	}
	model, effort, ok := strings.Cut(strings.TrimRight(string(raw), "\r\n"), "\t")
	if !ok || calleridentity.Model(model) == "" {
		return "", ""
	}
	return model, effort
}

func invocationFacts(parsed parsedArgs, agent, rawModel string) calleridentity.Facts {
	boundHarness := envString("CONTEXTLATTICE_HARNESS", envString("CONTEXTLATTICE_AGENT", ""))
	requested := parsed.string("harness", parsed.string("agent", boundHarness))
	h := calleridentity.Harness(firstNonEmpty(requested, agent))
	thread := os.Getenv("CODEX_THREAD_ID")
	nativeID := parsed.string("native_session_id", os.Getenv("CONTEXTLATTICE_NATIVE_SESSION_ID"))
	foreignOwner := parsed.string("agent_id", "") != "" && parsed.string("agent_id", "") != os.Getenv("CONTEXTLATTICE_AGENT_ID")
	if requested == "" && agent == "agent-cli" && !foreignOwner && threadUUID.MatchString(thread) {
		h = "codex"
	}
	envModel := envString("CONTEXTLATTICE_MODEL", envString("CONTEXTLATTICE_AGENT_MODEL", ""))
	explicitModel := parsed.string("model", "")
	foreign := boundHarness != "" && calleridentity.Harness(boundHarness) != h
	if foreign && explicitModel == "" {
		rawModel = ""
	}
	model, observation := rawModel, "invocation"
	effort := parsed.string("reasoning_effort", "")
	observed := false
	if h == "codex" && (nativeID == "" || nativeID == thread) && (explicitModel == "" || effort == "") {
		if m, e := codexThreadFacts(thread); m != "" && (rawModel == "" || m == rawModel || explicitModel == "" && rawModel == envModel) {
			model, observation, observed = m, "codex_thread_index", true
			if effort == "" {
				effort = e
			}
		}
	}
	bound := !foreign && (envModel == "" || envModel == model)
	value := func(flag, env string) string {
		if bound {
			return parsed.string(flag, os.Getenv(env))
		}
		return parsed.string(flag, "")
	}
	if effort == "" && !observed {
		effort = value("reasoning_effort", "CONTEXTLATTICE_REASONING_EFFORT")
	}
	return calleridentity.Facts{Harness: h, Model: calleridentity.Model(model), Effort: effort, Observation: observation,
		Origin: value("model_origin", "CONTEXTLATTICE_MODEL_ORIGIN"), Family: value("model_family", "CONTEXTLATTICE_MODEL_FAMILY"), Version: value("model_version", "CONTEXTLATTICE_MODEL_VERSION")}
}

func (c *cli) cmdIdentity(args []string) error {
	p := parseArgs(args, commonStringFlags(), commonBoolFlags())
	if p.bool("help") {
		return c.emitUsage("contextlattice identity [--agent harness] [--agent-id owner] [--model slug] [--reasoning-effort level] [--raw]")
	}
	caller := resolveNativeCaller(p, loadAgentProfiles())
	return c.emit(map[string]any{"ok": true, "agent_id": caller.agentID, "caller_attribution": caller.metadata()}, !p.bool("raw"))
}
