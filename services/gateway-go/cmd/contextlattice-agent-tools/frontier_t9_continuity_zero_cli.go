package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

const frontierT9CLIContinuityZeroPath = "/memory/continuity-zero"

func frontierT9ContinuityZeroUsage() string {
	return "contextlattice continuity-zero [--project name] [--agent name] [--agent-id id] [--session-id id] [--repo owner/repo] [--branch name] [--commit sha] [--passport-id id] [--mesh-grant-id id] [--cwd repo] [--output manifest.json] [--pretty|--raw]"
}

func frontierT9GitValue(cwd string, args ...string) string {
	argv := append([]string{"-C", cwd}, args...)
	command := exec.Command("git", argv...)
	command.Env = os.Environ()
	raw, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func frontierT9RepositoryBasename(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if filepath.Base(path) == ".git" {
		path = filepath.Dir(path)
	}
	return filepath.Base(path)
}

func frontierT9CLISafeRepositoryIdentity(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	remotePath := false
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		if strings.EqualFold(parsed.Scheme, "file") {
			return frontierT9RepositoryBasename(parsed.Path)
		}
		value = parsed.Path
		remotePath = true
	} else if at := strings.Index(value, "@"); at >= 0 {
		if colon := strings.Index(value[at+1:], ":"); colon >= 0 {
			value = value[at+1+colon+1:]
			remotePath = true
		}
	}
	value = strings.TrimSuffix(strings.TrimRight(strings.SplitN(strings.SplitN(value, "?", 2)[0], "#", 2)[0], "/"), ".git")
	if !remotePath && (filepath.IsAbs(value) || strings.HasPrefix(value, "~") || strings.Contains(value, "..")) {
		return frontierT9RepositoryBasename(value)
	}
	value = strings.ReplaceAll(value, "\\", "/")
	parts := make([]string, 0, 3)
	for _, part := range strings.Split(value, "/") {
		if part = strings.TrimSpace(part); part != "" && part != "." && part != ".." {
			parts = append(parts, part)
		}
	}
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return ""
}

func frontierT9UniqueCLIStrings(values ...string) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func frontierT9CLIGitIdentity(cwd, repositoryOverride, branchOverride, commitOverride string) (string, []string, string, string, error) {
	repository := frontierT9CLISafeRepositoryIdentity(repositoryOverride)
	origin := frontierT9CLISafeRepositoryIdentity(frontierT9GitValue(cwd, "config", "--get", "remote.origin.url"))
	top := frontierT9GitValue(cwd, "rev-parse", "--show-toplevel")
	common := frontierT9GitValue(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	aliases := frontierT9UniqueCLIStrings(origin, frontierT9RepositoryBasename(top), frontierT9RepositoryBasename(common))
	if repository == "" {
		if origin != "" {
			repository = origin
		} else if len(aliases) > 0 {
			repository = aliases[0]
		}
	}
	if repository == "" {
		return "", nil, "", "", errors.New("repository identity unavailable; use --repo")
	}
	filteredAliases := []string{}
	for _, alias := range aliases {
		if !strings.EqualFold(alias, repository) {
			filteredAliases = append(filteredAliases, alias)
		}
	}
	branch := strings.TrimSpace(branchOverride)
	if branch == "" {
		branch = frontierT9GitValue(cwd, "branch", "--show-current")
	}
	commit := strings.TrimSpace(commitOverride)
	if commit == "" {
		commit = frontierT9GitValue(cwd, "rev-parse", "HEAD")
	}
	if commit == "" {
		return "", nil, "", "", errors.New("commit identity unavailable; use --commit")
	}
	return repository, filteredAliases, branch, commit, nil
}

func frontierT9CLIProject(parsed parsedArgs, repository string) string {
	if project := parsed.string("project", ""); project != "" {
		return project
	}
	if project := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_PROJECT")); project != "" {
		return project
	}
	value := strings.TrimSuffix(strings.TrimRight(repository, "/"), ".git")
	if index := strings.LastIndexAny(value, "/:"); index >= 0 {
		value = value[index+1:]
	}
	return strings.TrimSpace(value)
}

func (c *cli) cmdContinuityZero(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"project": "project", "agent": "agent", "agent-id": "agent_id", "harness": "harness",
		"session-id": "session_id", "repo": "repo", "branch": "branch", "commit": "commit",
		"passport-id": "passport_id", "mesh-grant-id": "mesh_grant_id", "cwd": "cwd", "output": "output",
	}), commonBoolFlags())
	if parsed.bool("help") || len(parsed.pos) > 0 {
		return c.emitUsage(frontierT9ContinuityZeroUsage())
	}
	cwd := parsed.string("cwd", "")
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return errors.New("resolve current repository failed")
		}
	}
	repository, aliases, branch, commit, err := frontierT9CLIGitIdentity(
		cwd, parsed.string("repo", ""), parsed.string("branch", ""), parsed.string("commit", ""),
	)
	if err != nil {
		return err
	}
	project := frontierT9CLIProject(parsed, repository)
	if project == "" {
		return errors.New("project unavailable; use --project")
	}
	agent := firstNonEmpty(parsed.string("agent", ""), strings.TrimSpace(os.Getenv("CONTEXTLATTICE_AGENT")))
	agentID := firstNonEmpty(parsed.string("agent_id", ""), strings.TrimSpace(os.Getenv("CONTEXTLATTICE_AGENT_ID")), strings.TrimSpace(os.Getenv("MEMMCP_AGENT_ID")))
	sessionID := firstNonEmpty(parsed.string("session_id", ""), strings.TrimSpace(os.Getenv("CONTEXTLATTICE_SESSION_ID")))
	payload := map[string]any{
		"project": project, "agent": agent, "agent_id": agentID,
		"harness": firstNonEmpty(parsed.string("harness", ""), agent), "session_id": sessionID,
		"repository_id": repository, "repository_aliases": aliases, "branch": branch, "commit": commit,
	}
	for _, pair := range []struct{ flag, key string }{{"passport_id", "passport_id"}, {"mesh_grant_id", "mesh_grant_id"}} {
		if value := parsed.string(pair.flag, ""); value != "" {
			payload[pair.key] = value
		}
	}
	c.applyBaseURL(parsed)
	result, status, err := c.requestJSON(context.Background(), http.MethodPost, frontierT9CLIContinuityZeroPath, payload, parsed.float("timeout", 20))
	if err != nil {
		if status > 0 {
			return fmt.Errorf("continuity-zero request failed with status %d", status)
		}
		return errors.New("continuity-zero request failed")
	}
	if firstString(result["schema_id"]) != "continuity_zero.v1" || firstString(asMap(asMap(result["format_contract"])["validation"])["status"]) != "passed" {
		return errors.New("continuity-zero response failed its public contract")
	}
	if output := parsed.string("output", ""); output != "" {
		if err := writePrivateJSONArtifact(output, result); err != nil {
			return errors.New("write continuity-zero owner-only artifact failed")
		}
		result = map[string]any{
			"ok": true, "schema_id": "continuity_zero_cli_artifact.v1", "decision": result["decision"],
			"manifest_id": result["manifest_id"], "manifest_digest": result["manifest_digest"],
			"artifact_written": true, "artifact_kind": "continuity_zero.v1", "artifact_path": output,
		}
	}
	return c.emit(result, parsed.bool("pretty") || !parsed.bool("raw"))
}
