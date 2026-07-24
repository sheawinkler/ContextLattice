package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	skillSourceSchemaID          = "contextlattice.skill_source.v1"
	skillSourceProvider          = "vercel_skills_cli"
	skillSourceManifestName      = ".contextlattice-source.json"
	skillSourceMaxCommandBytes   = 1 << 20
	skillSourceMaxFileBytes      = 4 << 20
	skillSourceMaxTotalBytes     = 16 << 20
	skillSourceMaxFiles          = 256
	skillSourceMaxRepositoryWalk = 20_000
	skillSourceMaxFindings       = 100
	defaultSkillSourceNPXPackage = "skills@1.5.9"
)

var (
	skillSourceSegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	skillSourceRefPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	skillSourceNPXPattern     = regexp.MustCompile(`^skills@[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?$`)
	skillSourceANSI           = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	skillSourceDiscoveryLine  = regexp.MustCompile(`(?m)([A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*@[A-Za-z0-9][A-Za-z0-9._-]*)[ \t]+([0-9][0-9.,]*[KkMm]?\+?)[ \t]+installs?`)
	skillSourceBlockedRules   = []skillSourceRule{
		{id: "embedded_private_key", severity: "blocked", pattern: regexp.MustCompile(`-----BEGIN (?:[A-Z0-9 ]+ )?PRIVATE KEY-----`)},
		{id: "embedded_aws_access_key", severity: "blocked", pattern: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
		{id: "embedded_openai_token", severity: "blocked", pattern: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
		{id: "embedded_github_token", severity: "blocked", pattern: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`)},
		{id: "embedded_slack_token", severity: "blocked", pattern: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`)},
		{id: "embedded_jwt", severity: "blocked", pattern: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
	}
	skillSourceReviewRules = []skillSourceRule{
		{id: "instruction_override", severity: "review_required", pattern: regexp.MustCompile(`(?i)ignore[^\n]{0,80}(previous|prior|system|developer)[^\n]{0,40}instructions?`)},
		{id: "network_pipe_to_shell", severity: "review_required", pattern: regexp.MustCompile(`(?i)\b(curl|wget)\b[^\n]{0,240}\|\s*(sh|bash|zsh)\b`)},
		{id: "recursive_destructive_command", severity: "review_required", pattern: regexp.MustCompile(`(?i)\brm\s+-[A-Za-z]*r[A-Za-z]*f|\brm\s+-[A-Za-z]*f[A-Za-z]*r`)},
		{id: "recursive_permission_change", severity: "review_required", pattern: regexp.MustCompile(`(?i)\b(chmod|chown)\b[^\n]{0,80}\s-R\b|\b(chmod|chown)\s+-R\b`)},
		{id: "privilege_escalation", severity: "review_required", pattern: regexp.MustCompile(`(?i)(^|[^A-Za-z0-9_])sudo([^A-Za-z0-9_]|$)`)},
		{id: "credential_store_access", severity: "review_required", pattern: regexp.MustCompile(`(?i)(~/?\.ssh|/etc/passwd|find-generic-password|login\.keychain|credentials?\.(json|toml|yaml|yml))`)},
		{id: "credential_exfiltration", severity: "review_required", pattern: regexp.MustCompile(`(?i)(curl|wget)[^\n]{0,160}(\$[A-Z_]*(TOKEN|KEY|SECRET|PASSWORD)|process\.env\.[A-Z_]*(TOKEN|KEY|SECRET|PASSWORD))`)},
	}
)

type skillSourcePackage struct {
	Owner string
	Repo  string
	Skill string
}

func (p skillSourcePackage) String() string {
	return p.Owner + "/" + p.Repo + "@" + p.Skill
}

func (p skillSourcePackage) repositoryURL() string {
	return "https://github.com/" + p.Owner + "/" + p.Repo + ".git"
}

func (p skillSourcePackage) directoryName() string {
	return p.Owner + "--" + p.Repo + "--" + p.Skill
}

type skillSourceRule struct {
	id       string
	severity string
	pattern  *regexp.Regexp
}

type skillSourceFinding struct {
	RuleID   string `json:"rule_id"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
}

type skillSourceScan struct {
	Status     string               `json:"status"`
	FileCount  int                  `json:"file_count"`
	TotalBytes int64                `json:"total_bytes"`
	Findings   []skillSourceFinding `json:"findings"`
}

type skillSourceManifest struct {
	SchemaID                string          `json:"schema_id"`
	Provider                string          `json:"provider"`
	Package                 string          `json:"package"`
	Repository              string          `json:"repository"`
	Ref                     string          `json:"ref"`
	Commit                  string          `json:"commit"`
	SkillPath               string          `json:"skill_path"`
	ContentSHA256           string          `json:"content_sha256"`
	Scan                    skillSourceScan `json:"scan"`
	StagedAt                string          `json:"staged_at"`
	CheckedAt               string          `json:"checked_at"`
	PromotedAt              string          `json:"promoted_at,omitempty"`
	PromotionAcceptedReview bool            `json:"promotion_accepted_review,omitempty"`
}

type skillSourceFile struct {
	RelativePath string
	Mode         os.FileMode
	Content      []byte
}

type skillSourceDiscoveryResult struct {
	Package      string `json:"package"`
	InstallCount int64  `json:"install_count"`
	Installs     string `json:"installs"`
	URL          string `json:"url"`
}

type skillSourceCommandResult struct {
	stdout     string
	stderr     string
	durationMS int64
}

type skillSourceCloneFunc func(context.Context, skillSourcePackage, string, string) (string, error)

type cappedSkillSourceBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *cappedSkillSourceBuffer) Write(raw []byte) (int, error) {
	accepted := len(raw)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(raw) > remaining {
			raw = raw[:remaining]
		}
		_, _ = b.buffer.Write(raw)
	}
	return accepted, nil
}

func (b *cappedSkillSourceBuffer) String() string {
	return b.buffer.String()
}

func parseSkillSourcePackage(raw string) (skillSourcePackage, error) {
	raw = strings.TrimSpace(raw)
	ownerRepo, skill, found := strings.Cut(raw, "@")
	if !found || strings.Contains(skill, "@") {
		return skillSourcePackage{}, errors.New("package must use owner/repo@skill")
	}
	owner, repo, found := strings.Cut(ownerRepo, "/")
	if !found || strings.Contains(repo, "/") {
		return skillSourcePackage{}, errors.New("package must use owner/repo@skill")
	}
	parts := []string{owner, repo, skill}
	for _, part := range parts {
		if part == "." || part == ".." || !skillSourceSegmentPattern.MatchString(part) {
			return skillSourcePackage{}, errors.New("package contains an invalid owner, repository, or skill name")
		}
	}
	return skillSourcePackage{Owner: owner, Repo: repo, Skill: skill}, nil
}

func parseSkillSourceInstallCount(raw string) int64 {
	value := strings.TrimSuffix(strings.TrimSpace(raw), "+")
	multiplier := float64(1)
	if strings.HasSuffix(strings.ToLower(value), "k") {
		multiplier = 1_000
		value = value[:len(value)-1]
	} else if strings.HasSuffix(strings.ToLower(value), "m") {
		multiplier = 1_000_000
		value = value[:len(value)-1]
	}
	value = strings.ReplaceAll(value, ",", "")
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || parsed < 0 {
		return 0
	}
	return int64(parsed * multiplier)
}

func parseSkillSourceDiscovery(raw string, limit int) []skillSourceDiscoveryResult {
	if limit < 1 {
		limit = 10
	}
	cleaned := skillSourceANSI.ReplaceAllString(raw, "")
	matches := skillSourceDiscoveryLine.FindAllStringSubmatch(cleaned, -1)
	seen := map[string]struct{}{}
	results := make([]skillSourceDiscoveryResult, 0, minInt(len(matches), limit))
	for _, match := range matches {
		if len(match) != 3 {
			continue
		}
		pkg, err := parseSkillSourcePackage(match[1])
		if err != nil {
			continue
		}
		if _, ok := seen[pkg.String()]; ok {
			continue
		}
		seen[pkg.String()] = struct{}{}
		results = append(results, skillSourceDiscoveryResult{
			Package:      pkg.String(),
			InstallCount: parseSkillSourceInstallCount(match[2]),
			Installs:     match[2],
			URL:          "https://skills.sh/" + pkg.Owner + "/" + pkg.Repo + "/" + pkg.Skill,
		})
		if len(results) == limit {
			break
		}
	}
	return results
}

func runSkillSourceCommand(ctx context.Context, directory string, environment []string, name string, args ...string) (skillSourceCommandResult, error) {
	resolved, err := exec.LookPath(name)
	if err != nil {
		return skillSourceCommandResult{}, fmt.Errorf("%s is required: %w", name, err)
	}
	command := exec.CommandContext(ctx, resolved, args...)
	command.Dir = directory
	command.Env = append(os.Environ(), environment...)
	stdout := &cappedSkillSourceBuffer{limit: skillSourceMaxCommandBytes}
	stderr := &cappedSkillSourceBuffer{limit: skillSourceMaxCommandBytes}
	command.Stdout = stdout
	command.Stderr = stderr
	started := time.Now()
	err = command.Run()
	result := skillSourceCommandResult{
		stdout:     strings.TrimSpace(stdout.String()),
		stderr:     strings.TrimSpace(stderr.String()),
		durationMS: time.Since(started).Milliseconds(),
	}
	if err != nil {
		detail := truncate(result.stderr, 400)
		if detail == "" {
			detail = truncate(result.stdout, 400)
		}
		if detail == "" {
			detail = err.Error()
		}
		return result, errors.New(detail)
	}
	return result, nil
}

func discoverSkillSources(ctx context.Context, query string, limit int) (map[string]any, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("query is required")
	}
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	npxPackage, err := skillSourceNPXPackage()
	if err != nil {
		return nil, err
	}
	commandResult, err := runSkillSourceCommand(
		ctx,
		"",
		[]string{"CI=1", "NO_COLOR=1", "FORCE_COLOR=0", "DISABLE_TELEMETRY=1", "DO_NOT_TRACK=1"},
		"npx",
		"--yes",
		npxPackage,
		"find",
		query,
	)
	if err != nil {
		return nil, fmt.Errorf("Vercel skills discovery failed: %w", err)
	}
	results := parseSkillSourceDiscovery(commandResult.stdout, limit)
	return map[string]any{
		"ok":          true,
		"schema_id":   "contextlattice.skill_source_discovery.v1",
		"provider":    skillSourceProvider,
		"cli_package": npxPackage,
		"query":       query,
		"returned":    len(results),
		"results":     results,
		"duration_ms": commandResult.durationMS,
		"next_step":   "stage a package into quarantine; discovery never installs or activates a skill",
	}, nil
}

func skillSourceNPXPackage() (string, error) {
	value := envString("CONTEXTLATTICE_SKILLS_NPX_PACKAGE", defaultSkillSourceNPXPackage)
	if !skillSourceNPXPattern.MatchString(value) {
		return "", errors.New("CONTEXTLATTICE_SKILLS_NPX_PACKAGE must pin skills@<semver>")
	}
	return value, nil
}

func expandSkillSourcePath(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			raw = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
		}
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return ""
	}
	return filepath.Clean(absolute)
}

func skillSourceQuarantineRoot() (string, error) {
	raw := firstString(
		os.Getenv("CONTEXTLATTICE_SKILLS_QUARANTINE_ROOT"),
		os.Getenv("ORCH_SKILLS_QUARANTINE_HOST_ROOT_DIR"),
		os.Getenv("CODEX_SKILLS_QUARANTINE_ROOT"),
	)
	if strings.TrimSpace(raw) == "" {
		raw = filepath.Join(homeDir(), ".codex", "skills_quarantine")
	}
	root := expandSkillSourcePath(raw)
	if root == "" || root == string(filepath.Separator) {
		return "", errors.New("skills quarantine root must resolve to a bounded absolute path")
	}
	return root, nil
}

func skillSourceActiveRoot() (string, error) {
	raw := firstString(
		os.Getenv("CONTEXTLATTICE_SKILLS_ACTIVE_ROOT"),
		os.Getenv("ORCH_SKILLS_INDEX_HOST_ACTIVE_ROOT_DIR"),
	)
	if strings.TrimSpace(raw) == "" {
		raw = filepath.Join(homeDir(), ".codex", "skills")
	}
	root := expandSkillSourcePath(raw)
	if root == "" || root == string(filepath.Separator) {
		return "", errors.New("skills active root must resolve to a bounded absolute path")
	}
	return root, nil
}

func cloneSkillSourceRepository(ctx context.Context, pkg skillSourcePackage, ref string, destination string) (string, error) {
	args := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.file.allow=never",
		"clone",
		"--depth", "1",
		"--filter=blob:none",
		"--single-branch",
	}
	if strings.TrimSpace(ref) != "" && !strings.EqualFold(strings.TrimSpace(ref), "HEAD") {
		if !validSkillSourceRef(ref) {
			return "", errors.New("ref must be a simple branch or tag name")
		}
		args = append(args, "--branch", ref)
	}
	args = append(args, pkg.repositoryURL(), destination)
	if _, err := runSkillSourceCommand(
		ctx,
		"",
		[]string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0"},
		"git",
		args...,
	); err != nil {
		return "", fmt.Errorf("clone skill repository: %w", err)
	}
	commitResult, err := runSkillSourceCommand(
		ctx,
		destination,
		[]string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_ATTR_NOSYSTEM=1", "GIT_OPTIONAL_LOCKS=0"},
		"git",
		"rev-parse",
		"HEAD",
	)
	if err != nil {
		return "", fmt.Errorf("resolve staged commit: %w", err)
	}
	commit := strings.TrimSpace(commitResult.stdout)
	if len(commit) != 40 && len(commit) != 64 {
		return "", errors.New("staged repository returned an invalid commit identity")
	}
	return commit, nil
}

func validSkillSourceRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	if !skillSourceRefPattern.MatchString(ref) ||
		strings.HasPrefix(ref, "-") ||
		strings.HasPrefix(ref, ".") ||
		strings.HasSuffix(ref, ".") ||
		strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".lock") ||
		strings.Contains(ref, "..") ||
		strings.Contains(ref, "//") ||
		strings.Contains(ref, "@{") {
		return false
	}
	for _, component := range strings.Split(ref, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") {
			return false
		}
	}
	return true
}

func normalizeSkillSourceName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastDash := false
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			out.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func skillSourceFrontmatterValue(text string, key string) string {
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	end := strings.Index(text[3:], "\n---")
	if end < 0 {
		return ""
	}
	header := text[3 : 3+end]
	prefix := strings.ToLower(key) + ":"
	for _, line := range strings.Split(header, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) {
			return strings.Trim(strings.TrimSpace(trimmed[len(prefix):]), `"'`)
		}
	}
	return ""
}

func locateSkillSourceDirectory(repositoryRoot string, skillName string) (string, string, error) {
	type candidate struct {
		path     string
		relative string
		score    int
	}
	walked := 0
	candidates := []candidate{}
	err := filepath.WalkDir(repositoryRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		walked++
		if walked > skillSourceMaxRepositoryWalk {
			return errors.New("repository traversal exceeded the bounded entry limit")
		}
		if entry.IsDir() {
			switch entry.Name() {
			case ".git", "node_modules", ".next", "dist", "target":
				if path != repositoryRoot {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Name() != "SKILL.md" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() > skillSourceMaxFileBytes {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		relative, err := filepath.Rel(repositoryRoot, filepath.Dir(path))
		if err != nil {
			return nil
		}
		frontmatterName := skillSourceFrontmatterValue(string(raw), "name")
		score := 0
		if strings.EqualFold(strings.TrimSpace(frontmatterName), skillName) {
			score = 100
		} else if normalizeSkillSourceName(frontmatterName) == normalizeSkillSourceName(skillName) && frontmatterName != "" {
			score = 90
		} else if strings.EqualFold(filepath.Base(filepath.Dir(path)), skillName) {
			score = 80
		} else if normalizeSkillSourceName(filepath.Base(filepath.Dir(path))) == normalizeSkillSourceName(skillName) {
			score = 70
		}
		if score > 0 {
			candidates = append(candidates, candidate{path: filepath.Dir(path), relative: filepath.ToSlash(relative), score: score})
		}
		return nil
	})
	if err != nil {
		return "", "", err
	}
	if len(candidates) == 0 {
		return "", "", fmt.Errorf("repository does not contain a uniquely matching SKILL.md for %q", skillName)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].relative < candidates[j].relative
		}
		return candidates[i].score > candidates[j].score
	})
	if len(candidates) > 1 && candidates[0].score == candidates[1].score {
		return "", "", fmt.Errorf("repository contains multiple equally matching SKILL.md files for %q", skillName)
	}
	return candidates[0].path, candidates[0].relative, nil
}

func skillSourceTextFile(relative string, raw []byte) bool {
	if bytes.IndexByte(raw, 0) >= 0 {
		return false
	}
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".md", ".txt", ".sh", ".zsh", ".bash", ".py", ".rb", ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx", ".json", ".yaml", ".yml", ".toml", ".go", ".rs", ".ps1":
		return true
	default:
		return filepath.Base(relative) == "SKILL.md"
	}
}

func skillSourceExecutableFile(relative string, mode os.FileMode) bool {
	if mode.Perm()&0o111 != 0 {
		return true
	}
	switch strings.ToLower(filepath.Ext(relative)) {
	case ".sh", ".zsh", ".bash", ".py", ".rb", ".js", ".mjs", ".cjs", ".ps1":
		return true
	default:
		return false
	}
}

func skillSourceLine(raw []byte, offset int) int {
	if offset < 0 {
		return 0
	}
	return bytes.Count(raw[:minInt(offset, len(raw))], []byte("\n")) + 1
}

func inspectSkillSourceDirectory(root string) ([]skillSourceFile, skillSourceScan, string, error) {
	files := []skillSourceFile{}
	findings := []skillSourceFinding{}
	var totalBytes int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill source contains a rejected symlink at %s", relative)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("skill source contains a rejected special file at %s", relative)
		}
		if filepath.Base(relative) == skillSourceManifestName {
			return nil
		}
		if len(files) >= skillSourceMaxFiles {
			return errors.New("skill source exceeds the bounded file-count limit")
		}
		if info.Size() > skillSourceMaxFileBytes {
			return fmt.Errorf("skill source file exceeds the bounded byte limit at %s", relative)
		}
		totalBytes += info.Size()
		if totalBytes > skillSourceMaxTotalBytes {
			return errors.New("skill source exceeds the bounded total-byte limit")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files = append(files, skillSourceFile{RelativePath: relative, Mode: info.Mode(), Content: raw})
		if !skillSourceTextFile(relative, raw) {
			return nil
		}
		for _, rule := range append(skillSourceBlockedRules, skillSourceReviewRules...) {
			for _, location := range rule.pattern.FindAllIndex(raw, -1) {
				if len(findings) >= skillSourceMaxFindings {
					break
				}
				findings = append(findings, skillSourceFinding{
					RuleID:   rule.id,
					Severity: rule.severity,
					Path:     relative,
					Line:     skillSourceLine(raw, location[0]),
				})
			}
		}
		if skillSourceExecutableFile(relative, info.Mode()) && len(findings) < skillSourceMaxFindings {
			findings = append(findings, skillSourceFinding{
				RuleID:   "executable_skill_content",
				Severity: "review_required",
				Path:     relative,
			})
		}
		return nil
	})
	if err != nil {
		return nil, skillSourceScan{}, "", err
	}
	if len(files) == 0 {
		return nil, skillSourceScan{}, "", errors.New("skill source contains no regular files")
	}
	hasSkill := false
	for _, file := range files {
		if file.RelativePath == "SKILL.md" {
			hasSkill = true
			break
		}
	}
	if !hasSkill {
		return nil, skillSourceScan{}, "", errors.New("skill source root is missing SKILL.md")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].RelativePath < files[j].RelativePath })
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity == "blocked"
		}
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		if findings[i].Line != findings[j].Line {
			return findings[i].Line < findings[j].Line
		}
		return findings[i].RuleID < findings[j].RuleID
	})
	status := "pass"
	for _, finding := range findings {
		if finding.Severity == "blocked" {
			status = "blocked"
			break
		}
		status = "review_required"
	}
	digest := sha256.New()
	for _, file := range files {
		_, _ = io.WriteString(digest, file.RelativePath)
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write(file.Content)
		_, _ = digest.Write([]byte{0})
	}
	return files, skillSourceScan{
		Status:     status,
		FileCount:  len(files),
		TotalBytes: totalBytes,
		Findings:   findings,
	}, "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

func writeSkillSourceTree(root string, files []skillSourceFile, manifest skillSourceManifest) error {
	for _, file := range files {
		target := filepath.Join(root, filepath.FromSlash(file.RelativePath))
		if !pathWithinSkillSourceRoot(root, target) {
			return errors.New("skill source contains an unsafe relative path")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		mode := (file.Mode.Perm() & 0o555) | 0o600
		if err := os.WriteFile(target, file.Content, mode); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(filepath.Join(root, skillSourceManifestName), raw, 0o600)
}

func pathWithinSkillSourceRoot(root string, target string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(target))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func ensureSkillSourceDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("skill source root must be a regular directory")
	}
	return nil
}

func replaceSkillSourceTree(parent string, target string, files []skillSourceFile, manifest skillSourceManifest, preserveBackup bool) (string, error) {
	if !pathWithinSkillSourceRoot(parent, target) || filepath.Clean(parent) == filepath.Clean(target) {
		return "", errors.New("refusing an unbounded skill source target")
	}
	if err := ensureSkillSourceDirectory(parent); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".contextlattice-stage-*")
	if err != nil {
		return "", err
	}
	stagingOwned := true
	defer func() {
		if stagingOwned {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := writeSkillSourceTree(staging, files, manifest); err != nil {
		return "", err
	}
	var backup string
	if info, err := os.Lstat(target); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", errors.New("existing skill target must be a regular directory")
		}
		backupParent := filepath.Join(parent, ".contextlattice-backups")
		if err := ensureSkillSourceDirectory(backupParent); err != nil {
			return "", err
		}
		backup = filepath.Join(backupParent, filepath.Base(target)+"-"+time.Now().UTC().Format("20060102T150405.000000000Z"))
		if err := os.Rename(target, backup); err != nil {
			return "", fmt.Errorf("preserve existing skill target: %w", err)
		}
	}
	if err := os.Rename(staging, target); err != nil {
		if backup != "" {
			if restoreErr := os.Rename(backup, target); restoreErr != nil {
				return "", fmt.Errorf("publish staged skill tree: %v; restore prior target: %w", err, restoreErr)
			}
		}
		return "", fmt.Errorf("publish staged skill tree: %w", err)
	}
	stagingOwned = false
	if backup != "" && !preserveBackup {
		_ = os.RemoveAll(backup)
		backup = ""
	}
	return backup, nil
}

func stageSkillSourceFromCheckout(pkg skillSourcePackage, ref string, commit string, checkoutRoot string, quarantineRoot string, now time.Time) (map[string]any, error) {
	skillDirectory, relativeSkillPath, err := locateSkillSourceDirectory(checkoutRoot, pkg.Skill)
	if err != nil {
		return nil, err
	}
	files, scan, digest, err := inspectSkillSourceDirectory(skillDirectory)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(ref) == "" {
		ref = "HEAD"
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	manifest := skillSourceManifest{
		SchemaID:      skillSourceSchemaID,
		Provider:      skillSourceProvider,
		Package:       pkg.String(),
		Repository:    pkg.repositoryURL(),
		Ref:           ref,
		Commit:        commit,
		SkillPath:     relativeSkillPath,
		ContentSHA256: digest,
		Scan:          scan,
		StagedAt:      stamp,
		CheckedAt:     stamp,
	}
	providerRoot := filepath.Join(quarantineRoot, "vercel")
	target := filepath.Join(providerRoot, pkg.directoryName())
	if _, err := replaceSkillSourceTree(providerRoot, target, files, manifest, false); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":              true,
		"schema_id":       skillSourceSchemaID,
		"action":          "stage",
		"package":         pkg.String(),
		"provider":        skillSourceProvider,
		"repository":      pkg.repositoryURL(),
		"ref":             ref,
		"commit":          commit,
		"skill_path":      relativeSkillPath,
		"content_sha256":  digest,
		"scan":            scan,
		"quarantine_path": target,
		"active":          false,
		"next_step":       "review scan findings, then run promote with explicit approval",
	}, nil
}

func stageSkillSource(ctx context.Context, pkg skillSourcePackage, ref string, quarantineRoot string, now time.Time, clone skillSourceCloneFunc) (map[string]any, error) {
	temporary, err := os.MkdirTemp("", "contextlattice-skills-source-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	checkout := filepath.Join(temporary, "repository")
	commit, err := clone(ctx, pkg, ref, checkout)
	if err != nil {
		return nil, err
	}
	return stageSkillSourceFromCheckout(pkg, ref, commit, checkout, quarantineRoot, now)
}

func readSkillSourceManifest(path string) (skillSourceManifest, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return skillSourceManifest{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 128<<10 {
		return skillSourceManifest{}, errors.New("skill source manifest is not a bounded regular file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return skillSourceManifest{}, err
	}
	var manifest skillSourceManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return skillSourceManifest{}, err
	}
	if manifest.SchemaID != skillSourceSchemaID || manifest.Provider != skillSourceProvider {
		return skillSourceManifest{}, errors.New("skill source manifest has an unsupported schema or provider")
	}
	pkg, err := parseSkillSourcePackage(manifest.Package)
	if err != nil {
		return skillSourceManifest{}, err
	}
	if manifest.Repository != pkg.repositoryURL() {
		return skillSourceManifest{}, errors.New("skill source manifest repository does not match its package")
	}
	if manifest.Ref != "HEAD" && !validSkillSourceRef(manifest.Ref) {
		return skillSourceManifest{}, errors.New("skill source manifest contains an invalid ref")
	}
	if (len(manifest.Commit) != 40 && len(manifest.Commit) != 64) || !isSkillSourceHex(manifest.Commit) {
		return skillSourceManifest{}, errors.New("skill source manifest contains an invalid commit identity")
	}
	if !strings.HasPrefix(manifest.ContentSHA256, "sha256:") || len(manifest.ContentSHA256) != len("sha256:")+64 || !isSkillSourceHex(strings.TrimPrefix(manifest.ContentSHA256, "sha256:")) {
		return skillSourceManifest{}, errors.New("skill source manifest contains an invalid content digest")
	}
	return manifest, nil
}

func isSkillSourceHex(value string) bool {
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return value != ""
}

func listSkillSourceManifests(quarantineRoot string) ([]skillSourceManifest, error) {
	providerRoot := filepath.Join(quarantineRoot, "vercel")
	entries, err := os.ReadDir(providerRoot)
	if os.IsNotExist(err) {
		return []skillSourceManifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	manifests := []skillSourceManifest{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		manifest, err := readSkillSourceManifest(filepath.Join(providerRoot, entry.Name(), skillSourceManifestName))
		if err != nil {
			continue
		}
		manifests = append(manifests, manifest)
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Package < manifests[j].Package })
	return manifests, nil
}

func skillSourceRefreshDue(manifest skillSourceManifest, now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		return true
	}
	checked, err := time.Parse(time.RFC3339Nano, manifest.CheckedAt)
	if err != nil {
		return true
	}
	return !now.Before(checked.Add(interval))
}

func promoteSkillSource(pkg skillSourcePackage, quarantineRoot string, activeRoot string, acceptReview bool, replace bool, now time.Time) (map[string]any, error) {
	candidate := filepath.Join(quarantineRoot, "vercel", pkg.directoryName())
	manifest, err := readSkillSourceManifest(filepath.Join(candidate, skillSourceManifestName))
	if err != nil {
		return nil, fmt.Errorf("read quarantined source manifest: %w", err)
	}
	if manifest.Package != pkg.String() {
		return nil, errors.New("quarantined source manifest package does not match the requested package")
	}
	files, scan, digest, err := inspectSkillSourceDirectory(candidate)
	if err != nil {
		return nil, err
	}
	if digest != manifest.ContentSHA256 {
		return nil, errors.New("quarantined source content changed after its recorded scan")
	}
	if scan.Status == "blocked" {
		return nil, errors.New("blocked skill source cannot be promoted")
	}
	if scan.Status == "review_required" && !acceptReview {
		return nil, errors.New("skill source requires --accept-review before promotion")
	}
	if err := os.MkdirAll(activeRoot, 0o700); err != nil {
		return nil, err
	}
	target := filepath.Join(activeRoot, pkg.Skill)
	if _, err := os.Lstat(target); err == nil && !replace {
		return nil, errors.New("active skill already exists; use --replace after reviewing the collision")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	manifest.PromotedAt = now.UTC().Format(time.RFC3339Nano)
	manifest.PromotionAcceptedReview = scan.Status == "review_required"
	backup, err := replaceSkillSourceTree(activeRoot, target, files, manifest, true)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":               true,
		"schema_id":        skillSourceSchemaID,
		"action":           "promote",
		"package":          pkg.String(),
		"content_sha256":   digest,
		"scan":             scan,
		"active_path":      target,
		"backup_path":      backup,
		"active":           true,
		"index_visibility": "immediate_live_scan",
	}, nil
}

func (c *cli) skillsIndexDiscover(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"query": "query",
		"q":     "query",
		"limit": "limit",
	}), commonBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_skills_index discover '<query>' [--limit 10] [--timeout 30] [--pretty]")
	}
	query := parsed.string("query", strings.Join(parsed.pos, " "))
	timeout := parsed.float("timeout", 30)
	if timeout < 1 {
		timeout = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	result, err := discoverSkillSources(ctx, query, parsed.int("limit", 10))
	if err != nil {
		return err
	}
	return c.emit(result, parsed.bool("pretty"))
}

func (c *cli) skillsIndexStage(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"package": "package",
		"ref":     "ref",
	}), commonBoolFlags())
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_skills_index stage owner/repo@skill [--ref branch-or-tag] [--timeout 120] [--pretty]")
	}
	rawPackage := parsed.string("package", "")
	if rawPackage == "" && len(parsed.pos) > 0 {
		rawPackage = parsed.pos[0]
	}
	pkg, err := parseSkillSourcePackage(rawPackage)
	if err != nil {
		return err
	}
	root, err := skillSourceQuarantineRoot()
	if err != nil {
		return err
	}
	timeout := parsed.float("timeout", 120)
	if timeout < 1 {
		timeout = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	result, err := stageSkillSource(ctx, pkg, parsed.string("ref", ""), root, time.Now(), cloneSkillSourceRepository)
	if err != nil {
		return err
	}
	return c.emit(result, parsed.bool("pretty"))
}

func (c *cli) skillsIndexRefresh(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"interval-hours": "interval_hours",
		"limit":          "limit",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{"due": "due"}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_skills_index refresh [owner/repo@skill ...] [--due] [--interval-hours 24] [--limit 20] [--timeout 300] [--pretty]")
	}
	root, err := skillSourceQuarantineRoot()
	if err != nil {
		return err
	}
	manifests, err := listSkillSourceManifests(root)
	if err != nil {
		return err
	}
	selected := map[string]struct{}{}
	for _, raw := range parsed.pos {
		pkg, err := parseSkillSourcePackage(raw)
		if err != nil {
			return err
		}
		selected[pkg.String()] = struct{}{}
	}
	intervalHours := parsed.float("interval_hours", envFloat("CONTEXTLATTICE_SKILLS_REFRESH_INTERVAL_HOURS", 24))
	if intervalHours < 0 {
		return errors.New("--interval-hours must be non-negative")
	}
	limit := parsed.int("limit", 20)
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	if len(selected) > limit {
		return errors.New("requested packages exceed the bounded --limit")
	}
	timeout := parsed.float("timeout", 300)
	if timeout < 1 {
		timeout = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	now := time.Now()
	items := []map[string]any{}
	matched := map[string]bool{}
	attempted := 0
	processed := 0
	failures := 0
	changed := 0
	skipped := 0
	for _, manifest := range manifests {
		if len(selected) > 0 {
			if _, ok := selected[manifest.Package]; !ok {
				continue
			}
			matched[manifest.Package] = true
		}
		if processed >= limit {
			break
		}
		processed++
		if parsed.bool("due") && !skillSourceRefreshDue(manifest, now, time.Duration(intervalHours*float64(time.Hour))) {
			items = append(items, map[string]any{"package": manifest.Package, "status": "not_due", "checked_at": manifest.CheckedAt})
			skipped++
			continue
		}
		attempted++
		pkg, _ := parseSkillSourcePackage(manifest.Package)
		result, refreshErr := stageSkillSource(ctx, pkg, manifest.Ref, root, now, cloneSkillSourceRepository)
		if refreshErr != nil {
			items = append(items, map[string]any{"package": manifest.Package, "status": "failed", "error": truncate(refreshErr.Error(), 400)})
			failures++
			continue
		}
		wasChanged := firstString(result["content_sha256"]) != manifest.ContentSHA256 || firstString(result["commit"]) != manifest.Commit
		status := "unchanged"
		if wasChanged {
			status = "changed_in_quarantine"
			changed++
		}
		items = append(items, map[string]any{
			"package":        manifest.Package,
			"status":         status,
			"commit":         result["commit"],
			"content_sha256": result["content_sha256"],
			"scan":           result["scan"],
		})
	}
	for requested := range selected {
		if !matched[requested] {
			items = append(items, map[string]any{"package": requested, "status": "not_registered"})
			failures++
		}
	}
	result := map[string]any{
		"ok":             failures == 0,
		"schema_id":      "contextlattice.skill_source_refresh.v1",
		"provider":       skillSourceProvider,
		"attempted":      attempted,
		"changed":        changed,
		"skipped":        skipped,
		"failures":       failures,
		"items":          items,
		"active_mutated": false,
		"next_step":      "review changed quarantined sources and promote explicitly",
	}
	if err := c.emit(result, parsed.bool("pretty")); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("skills source refresh completed with %d failure(s)", failures)
	}
	return nil
}

func (c *cli) skillsIndexPromote(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"package": "package",
	}), mergeBoolFlags(commonBoolFlags(), map[string]string{
		"yes":           "yes",
		"replace":       "replace",
		"accept-review": "accept_review",
	}))
	if parsed.bool("help") {
		return c.emitUsage("contextlattice_skills_index promote owner/repo@skill --yes [--accept-review] [--replace] [--pretty]")
	}
	if !parsed.bool("yes") {
		return errors.New("promotion requires --yes after reviewing the quarantined source and scan")
	}
	rawPackage := parsed.string("package", "")
	if rawPackage == "" && len(parsed.pos) > 0 {
		rawPackage = parsed.pos[0]
	}
	pkg, err := parseSkillSourcePackage(rawPackage)
	if err != nil {
		return err
	}
	quarantineRoot, err := skillSourceQuarantineRoot()
	if err != nil {
		return err
	}
	activeRoot, err := skillSourceActiveRoot()
	if err != nil {
		return err
	}
	result, err := promoteSkillSource(pkg, quarantineRoot, activeRoot, parsed.bool("accept_review"), parsed.bool("replace"), time.Now())
	if err != nil {
		return err
	}
	return c.emit(result, parsed.bool("pretty"))
}
