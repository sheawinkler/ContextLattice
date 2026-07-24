package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSkillSourceFixture(t *testing.T, repositoryRoot string, skillName string, body string) string {
	t.Helper()
	skillDirectory := filepath.Join(repositoryRoot, "skills", skillName)
	if err := os.MkdirAll(skillDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + skillName + "\ndescription: Test fixture.\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(skillDirectory, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return skillDirectory
}

func TestParseSkillSourcePackageRejectsTraversal(t *testing.T) {
	valid, err := parseSkillSourcePackage("vercel-labs/agent-skills@frontend-design")
	if err != nil {
		t.Fatalf("parse valid package: %v", err)
	}
	if valid.Owner != "vercel-labs" || valid.Repo != "agent-skills" || valid.Skill != "frontend-design" {
		t.Fatalf("unexpected parsed package: %#v", valid)
	}
	for _, raw := range []string{
		"vercel-labs/agent-skills",
		"../agent-skills@frontend-design",
		"vercel-labs/../../escape@frontend-design",
		"vercel-labs/agent-skills@../escape",
		"https://github.com/vercel-labs/agent-skills@frontend-design",
	} {
		if _, err := parseSkillSourcePackage(raw); err == nil {
			t.Fatalf("expected %q to be rejected", raw)
		}
	}
}

func TestSkillSourcePinsDiscoveryCLIAndValidatesRefs(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_SKILLS_NPX_PACKAGE", "")
	if got, err := skillSourceNPXPackage(); err != nil || got != defaultSkillSourceNPXPackage {
		t.Fatalf("default npx package=%q err=%v", got, err)
	}
	t.Setenv("CONTEXTLATTICE_SKILLS_NPX_PACKAGE", "other-package@9.9.9")
	if _, err := skillSourceNPXPackage(); err == nil {
		t.Fatal("expected non-skills package override to be rejected")
	}
	for _, ref := range []string{"main", "release/v4.1.0", "v4.1.0-rc.1"} {
		if !validSkillSourceRef(ref) {
			t.Fatalf("expected valid ref %q", ref)
		}
	}
	for _, ref := range []string{"../main", "-branch", "feature//unsafe", "refs/heads/main.lock", "feature/@{upstream}"} {
		if validSkillSourceRef(ref) {
			t.Fatalf("expected invalid ref %q", ref)
		}
	}
}

func TestParseSkillSourceDiscoveryNormalizesVercelOutput(t *testing.T) {
	raw := "\x1b[32mvercel-labs/agent-skills@frontend-design\x1b[0m 185K installs\n" +
		"└ https://skills.sh/vercel-labs/agent-skills/frontend-design\n" +
		"owner/repo@second-skill 1,234 installs\n" +
		"owner/repo@second-skill 1,234 installs\n"
	results := parseSkillSourceDiscovery(raw, 10)
	if len(results) != 2 {
		t.Fatalf("results=%#v", results)
	}
	if results[0].Package != "vercel-labs/agent-skills@frontend-design" || results[0].InstallCount != 185_000 {
		t.Fatalf("unexpected first result: %#v", results[0])
	}
	if results[1].URL != "https://skills.sh/owner/repo/second-skill" || results[1].InstallCount != 1_234 {
		t.Fatalf("unexpected second result: %#v", results[1])
	}
}

func TestSkillSourceStageAndPromoteSafeContent(t *testing.T) {
	repositoryRoot := t.TempDir()
	writeSkillSourceFixture(t, repositoryRoot, "safe-skill", "Use deterministic local checks.")
	pkg, err := parseSkillSourcePackage("owner/repo@safe-skill")
	if err != nil {
		t.Fatal(err)
	}
	quarantineRoot := t.TempDir()
	activeRoot := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	staged, err := stageSkillSourceFromCheckout(pkg, "main", strings.Repeat("a", 40), repositoryRoot, quarantineRoot, now)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	scan, ok := staged["scan"].(skillSourceScan)
	if !ok || scan.Status != "pass" || scan.FileCount != 1 {
		t.Fatalf("unexpected scan: %#v", staged["scan"])
	}
	candidate := filepath.Join(quarantineRoot, "vercel", pkg.directoryName())
	manifest, err := readSkillSourceManifest(filepath.Join(candidate, skillSourceManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.ContentSHA256 == "" || manifest.Package != pkg.String() || manifest.Scan.Status != "pass" {
		t.Fatalf("unexpected manifest: %#v", manifest)
	}
	promoted, err := promoteSkillSource(pkg, quarantineRoot, activeRoot, false, false, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if promoted["index_visibility"] != "immediate_live_scan" {
		t.Fatalf("unexpected promotion result: %#v", promoted)
	}
	if _, err := os.Stat(filepath.Join(activeRoot, pkg.Skill, "SKILL.md")); err != nil {
		t.Fatalf("active skill missing: %v", err)
	}
}

func TestSkillSourceScanBlocksSecretsWithoutEchoingThem(t *testing.T) {
	root := t.TempDir()
	secret := "sk-" + strings.Repeat("A", 32)
	writeSkillSourceFixture(t, root, "unsafe-skill", "Never store this credential: "+secret)
	skillRoot := filepath.Join(root, "skills", "unsafe-skill")
	_, scan, _, err := inspectSkillSourceDirectory(skillRoot)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Status != "blocked" {
		t.Fatalf("expected blocked scan, got %#v", scan)
	}
	raw := strings.Builder{}
	for _, finding := range scan.Findings {
		raw.WriteString(finding.RuleID)
		raw.WriteString(finding.Path)
	}
	if strings.Contains(raw.String(), secret) {
		t.Fatal("scan finding leaked matched secret content")
	}
}

func TestSkillSourceRejectsSymlinksAndManifestProvenanceTampering(t *testing.T) {
	root := t.TempDir()
	skillRoot := writeSkillSourceFixture(t, root, "bounded-skill", "Stay bounded.")
	if err := os.Symlink(filepath.Join(skillRoot, "SKILL.md"), filepath.Join(skillRoot, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := inspectSkillSourceDirectory(skillRoot); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}

	pkg, _ := parseSkillSourcePackage("owner/repo@bounded-skill")
	manifest := skillSourceManifest{
		SchemaID:      skillSourceSchemaID,
		Provider:      skillSourceProvider,
		Package:       pkg.String(),
		Repository:    "https://github.com/other/repo.git",
		Ref:           "main",
		Commit:        strings.Repeat("a", 40),
		ContentSHA256: "sha256:" + strings.Repeat("b", 64),
	}
	manifestPath := filepath.Join(t.TempDir(), skillSourceManifestName)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readSkillSourceManifest(manifestPath); err == nil || !strings.Contains(err.Error(), "repository") {
		t.Fatalf("expected provenance mismatch rejection, got %v", err)
	}
}

func TestSkillSourceReviewRequiresExplicitAcceptance(t *testing.T) {
	repositoryRoot := t.TempDir()
	skillDirectory := writeSkillSourceFixture(t, repositoryRoot, "review-skill", "Review bundled helpers before use.")
	if err := os.WriteFile(filepath.Join(skillDirectory, "helper.sh"), []byte("#!/bin/zsh\nprint -r -- ready\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pkg, _ := parseSkillSourcePackage("owner/repo@review-skill")
	quarantineRoot := t.TempDir()
	activeRoot := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	staged, err := stageSkillSourceFromCheckout(pkg, "main", strings.Repeat("b", 40), repositoryRoot, quarantineRoot, now)
	if err != nil {
		t.Fatal(err)
	}
	scan := staged["scan"].(skillSourceScan)
	if scan.Status != "review_required" {
		t.Fatalf("expected review_required, got %#v", scan)
	}
	if _, err := promoteSkillSource(pkg, quarantineRoot, activeRoot, false, false, now); err == nil || !strings.Contains(err.Error(), "--accept-review") {
		t.Fatalf("expected explicit review gate, got %v", err)
	}
	if _, err := promoteSkillSource(pkg, quarantineRoot, activeRoot, true, false, now); err != nil {
		t.Fatalf("promote accepted review: %v", err)
	}
}

func TestSkillSourcePromotionDetectsCandidateTampering(t *testing.T) {
	repositoryRoot := t.TempDir()
	writeSkillSourceFixture(t, repositoryRoot, "stable-skill", "Use stable evidence.")
	pkg, _ := parseSkillSourcePackage("owner/repo@stable-skill")
	quarantineRoot := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if _, err := stageSkillSourceFromCheckout(pkg, "main", strings.Repeat("c", 40), repositoryRoot, quarantineRoot, now); err != nil {
		t.Fatal(err)
	}
	candidateSkill := filepath.Join(quarantineRoot, "vercel", pkg.directoryName(), "SKILL.md")
	if err := os.WriteFile(candidateSkill, []byte("---\nname: stable-skill\ndescription: changed\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := promoteSkillSource(pkg, quarantineRoot, t.TempDir(), false, false, now); err == nil || !strings.Contains(err.Error(), "changed after") {
		t.Fatalf("expected tamper rejection, got %v", err)
	}
}

func TestSkillSourcePromotionPreservesReplacedSkill(t *testing.T) {
	repositoryRoot := t.TempDir()
	writeSkillSourceFixture(t, repositoryRoot, "replace-skill", "New version.")
	pkg, _ := parseSkillSourcePackage("owner/repo@replace-skill")
	quarantineRoot := t.TempDir()
	activeRoot := t.TempDir()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	if _, err := stageSkillSourceFromCheckout(pkg, "main", strings.Repeat("d", 40), repositoryRoot, quarantineRoot, now); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(activeRoot, pkg.Skill)
	if err := os.MkdirAll(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "SKILL.md"), []byte("old version"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := promoteSkillSource(pkg, quarantineRoot, activeRoot, false, false, now); err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("expected collision gate, got %v", err)
	}
	result, err := promoteSkillSource(pkg, quarantineRoot, activeRoot, false, true, now)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	backup, _ := result["backup_path"].(string)
	if backup == "" {
		t.Fatalf("expected recoverable backup: %#v", result)
	}
	old, err := os.ReadFile(filepath.Join(backup, "SKILL.md"))
	if err != nil || string(old) != "old version" {
		t.Fatalf("backup was not preserved: %q err=%v", string(old), err)
	}
}

func TestSkillSourceRefreshDueIsBoundedByRecordedCheck(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	manifest := skillSourceManifest{CheckedAt: now.Add(-23 * time.Hour).Format(time.RFC3339Nano)}
	if skillSourceRefreshDue(manifest, now, 24*time.Hour) {
		t.Fatal("23-hour-old source should not be due at a 24-hour interval")
	}
	manifest.CheckedAt = now.Add(-25 * time.Hour).Format(time.RFC3339Nano)
	if !skillSourceRefreshDue(manifest, now, 24*time.Hour) {
		t.Fatal("25-hour-old source should be due at a 24-hour interval")
	}
}
