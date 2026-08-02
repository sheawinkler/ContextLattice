package gatewaystate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeMigrationTestFile(t *testing.T, path string, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir test file parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write test file: %v", err)
	}
}

func fixedMigrationOptions(legacyRoot string, stateRoot string) MigrationOptions {
	return MigrationOptions{
		LegacyRoot: legacyRoot,
		StateRoot:  stateRoot,
		Now: func() time.Time {
			return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		},
		AvailableBytes: func(string) (uint64, string, error) {
			return 1 << 30, "test", nil
		},
	}
}

func TestMigrationDryRunApplyDeduplicatesAndRollbackRestoresLegacyRoot(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "same.json"), "same\n")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "nested", "copy.json"), "copy\n")
	writeMigrationTestFile(t, filepath.Join(stateRoot, "same.json"), "same\n")

	options := fixedMigrationOptions(legacyRoot, stateRoot)
	plan, err := ExecuteMigration(options)
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if !plan.OK || plan.Status != "dry_run_ready" || plan.CopyCount != 1 || plan.AlreadyPresentCount != 1 {
		t.Fatalf("unexpected migration plan: %#v", plan)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "nested", "copy.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created destination: %v", err)
	}
	if _, err := os.Stat(plan.BackupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created backup: %v", err)
	}

	options.Apply = true
	options.Confirm = true
	completed, err := ExecuteMigration(options)
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	if !completed.OK || completed.Status != "completed" || completed.AppliedCopyCount != 1 {
		t.Fatalf("unexpected completed migration: %#v", completed)
	}
	if _, err := os.Stat(legacyRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy root was not moved to backup: %v", err)
	}
	if _, err := os.Stat(completed.BackupRoot); err != nil {
		t.Fatalf("rollback backup missing: %v", err)
	}
	if encoded, err := os.ReadFile(filepath.Join(stateRoot, "nested", "copy.json")); err != nil || string(encoded) != "copy\n" {
		t.Fatalf("copied destination mismatch: value=%q err=%v", encoded, err)
	}
	if _, err := os.Stat(completed.ManifestPath); err != nil {
		t.Fatalf("migration manifest missing: %v", err)
	}

	rolledBack, err := RollbackMigration(completed.ManifestPath, true)
	if err != nil {
		t.Fatalf("rollback migration: %v", err)
	}
	if !rolledBack.OK || rolledBack.Status != "rolled_back" || rolledBack.RollbackRemovedCount != 1 {
		t.Fatalf("unexpected rollback result: %#v", rolledBack)
	}
	if encoded, err := os.ReadFile(filepath.Join(legacyRoot, "nested", "copy.json")); err != nil || string(encoded) != "copy\n" {
		t.Fatalf("legacy backup was not restored: value=%q err=%v", encoded, err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "nested", "copy.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback preserved migration-created destination: %v", err)
	}
	if encoded, err := os.ReadFile(filepath.Join(stateRoot, "same.json")); err != nil || string(encoded) != "same\n" {
		t.Fatalf("rollback removed pre-existing deduplicated destination: value=%q err=%v", encoded, err)
	}
}

func TestMigrationRegularFileRootApplyAndRollback(t *testing.T) {
	base := t.TempDir()
	legacyFile := filepath.Join(base, "agent_sessions.json")
	stateRoot := filepath.Join(base, "state")
	writeMigrationTestFile(t, legacyFile, "session-ledger\n")

	options := fixedMigrationOptions(legacyFile, stateRoot)
	plan, err := ExecuteMigration(options)
	if err != nil {
		t.Fatalf("dry-run file migration: %v", err)
	}
	if !plan.OK || plan.Status != "dry_run_ready" || plan.LegacyKind != "file" || plan.FileCount != 1 || plan.CopyCount != 1 {
		t.Fatalf("unexpected file migration plan: %#v", plan)
	}
	if plan.Files[0].RelativePath != "agent_sessions.json" || plan.Files[0].SourcePath != legacyFile {
		t.Fatalf("file-root plan is not basename-bound: %#v", plan.Files[0])
	}

	options.Apply = true
	options.Confirm = true
	completed, err := ExecuteMigration(options)
	if err != nil {
		t.Fatalf("apply file migration: %v", err)
	}
	if !completed.OK || completed.Status != "completed" || completed.LegacyKind != "file" {
		t.Fatalf("unexpected completed file migration: %#v", completed)
	}
	if _, err := os.Stat(legacyFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy file was not moved to backup: %v", err)
	}
	if info, err := os.Lstat(completed.BackupRoot); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("file rollback backup is invalid: info=%#v err=%v", info, err)
	}
	destination := filepath.Join(stateRoot, "agent_sessions.json")
	if encoded, err := os.ReadFile(destination); err != nil || string(encoded) != "session-ledger\n" {
		t.Fatalf("file migration destination mismatch: value=%q err=%v", encoded, err)
	}

	rolledBack, err := RollbackMigration(completed.ManifestPath, true)
	if err != nil {
		t.Fatalf("rollback file migration: %v", err)
	}
	if !rolledBack.OK || rolledBack.Status != "rolled_back" || rolledBack.RollbackRemovedCount != 1 {
		t.Fatalf("unexpected file rollback result: %#v", rolledBack)
	}
	if encoded, err := os.ReadFile(legacyFile); err != nil || string(encoded) != "session-ledger\n" {
		t.Fatalf("legacy file was not restored: value=%q err=%v", encoded, err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file rollback preserved migration-created destination: %v", err)
	}
}

func TestMigrationConflictAndFreeSpaceBlockBeforeMutation(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "state.json"), "legacy\n")
	writeMigrationTestFile(t, filepath.Join(stateRoot, "state.json"), "different\n")
	options := fixedMigrationOptions(legacyRoot, stateRoot)
	plan, err := PlanMigration(options)
	if err != nil {
		t.Fatalf("plan conflict: %v", err)
	}
	if plan.OK || plan.Status != "blocked" {
		t.Fatalf("destination conflict did not block: %#v", plan)
	}
	options.Apply = true
	options.Confirm = true
	if _, err := ExecuteMigration(options); err == nil {
		t.Fatal("conflicting migration apply unexpectedly succeeded")
	}
	if encoded, err := os.ReadFile(filepath.Join(legacyRoot, "state.json")); err != nil || string(encoded) != "legacy\n" {
		t.Fatalf("blocked migration changed legacy file: value=%q err=%v", encoded, err)
	}

	stateRoot = filepath.Join(base, "empty-state")
	options = fixedMigrationOptions(legacyRoot, stateRoot)
	options.AvailableBytes = func(string) (uint64, string, error) { return 0, "test", nil }
	plan, err = PlanMigration(options)
	if err != nil {
		t.Fatalf("plan free-space block: %v", err)
	}
	if plan.OK || plan.Status != "blocked" || plan.RequiredBytes == 0 {
		t.Fatalf("insufficient free space did not block: %#v", plan)
	}
}

func TestMigrationInjectedCopyFailureLeavesLegacyAndRemovesCreatedFiles(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "a.json"), "a\n")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "b.json"), "b\n")
	options := fixedMigrationOptions(legacyRoot, stateRoot)
	options.Apply = true
	options.Confirm = true
	options.BeforeCopy = func(file MigrationFile) error {
		if file.RelativePath == "b.json" {
			return errors.New("injected copy failure")
		}
		return nil
	}
	result, err := ExecuteMigration(options)
	if err == nil || result.Status != "failed" {
		t.Fatalf("expected injected failure, result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(legacyRoot, "a.json")); err != nil {
		t.Fatalf("failure moved or removed legacy root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "a.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failure cleanup left copied destination: %v", err)
	}
	if _, err := os.Stat(result.BackupRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failure created rollback backup: %v", err)
	}
}

func TestRollbackRefusesChangedCanonicalDestination(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "state.json"), "original\n")
	options := fixedMigrationOptions(legacyRoot, stateRoot)
	options.Apply = true
	options.Confirm = true
	completed, err := ExecuteMigration(options)
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	writeMigrationTestFile(t, filepath.Join(stateRoot, "state.json"), "changed\n")
	result, err := RollbackMigration(completed.ManifestPath, true)
	if err == nil || result.Status != "rollback_blocked" {
		t.Fatalf("changed destination did not block rollback: result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(legacyRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("blocked rollback restored legacy root despite conflict: %v", err)
	}
	if _, err := os.Stat(completed.BackupRoot); err != nil {
		t.Fatalf("blocked rollback removed backup: %v", err)
	}
}

func TestMigrationRevalidatesDeduplicatedSourceBeforeCommit(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	legacyPath := filepath.Join(legacyRoot, "same.json")
	statePath := filepath.Join(stateRoot, "same.json")
	writeMigrationTestFile(t, legacyPath, "same\n")
	writeMigrationTestFile(t, statePath, "same\n")
	options := fixedMigrationOptions(legacyRoot, stateRoot)
	options.Apply = true
	options.Confirm = true
	options.BeforeCommit = func() error {
		return os.WriteFile(legacyPath, []byte("changed\n"), 0o600)
	}
	result, err := ExecuteMigration(options)
	if err == nil || result.Status != "failed" || !strings.Contains(result.Failure, "source same.json changed") {
		t.Fatalf("deduplicated source drift did not fail closed: result=%#v err=%v", result, err)
	}
	if encoded, readErr := os.ReadFile(legacyPath); readErr != nil || string(encoded) != "changed\n" {
		t.Fatalf("failed migration altered changed legacy source: value=%q err=%v", encoded, readErr)
	}
	if encoded, readErr := os.ReadFile(statePath); readErr != nil || string(encoded) != "same\n" {
		t.Fatalf("failed migration altered canonical destination: value=%q err=%v", encoded, readErr)
	}
	if _, statErr := os.Stat(result.BackupRoot); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed migration created rollback backup: %v", statErr)
	}
}

func TestMigrationBlocksReservedManifestNamespaceAndDestinationSymlink(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, ".migrations", "operator.json"), "reserved\n")
	plan, err := PlanMigration(fixedMigrationOptions(legacyRoot, stateRoot))
	if err != nil {
		t.Fatalf("plan reserved namespace: %v", err)
	}
	if plan.OK || !migrationHasBlocker(plan, "reserved_destination_namespace") {
		t.Fatalf("reserved migration namespace did not block: %#v", plan)
	}

	legacyRoot = filepath.Join(base, "legacy-symlink")
	stateRoot = filepath.Join(base, "state-symlink")
	externalRoot := filepath.Join(base, "external")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "linked", "state.json"), "value\n")
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatalf("mkdir state root: %v", err)
	}
	if err := os.MkdirAll(externalRoot, 0o700); err != nil {
		t.Fatalf("mkdir external root: %v", err)
	}
	if err := os.Symlink(externalRoot, filepath.Join(stateRoot, "linked")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	plan, err = PlanMigration(fixedMigrationOptions(legacyRoot, stateRoot))
	if err != nil {
		t.Fatalf("plan symlink destination: %v", err)
	}
	if plan.OK || !migrationHasBlocker(plan, "destination_symlink_path") {
		t.Fatalf("destination symlink did not block: %#v", plan)
	}
}

func TestRollbackRejectsTamperedManifestPathsBeforeMutation(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	stateRoot := filepath.Join(base, "state")
	writeMigrationTestFile(t, filepath.Join(legacyRoot, "state.json"), "original\n")
	options := fixedMigrationOptions(legacyRoot, stateRoot)
	options.Apply = true
	options.Confirm = true
	completed, err := ExecuteMigration(options)
	if err != nil {
		t.Fatalf("apply migration: %v", err)
	}
	victimPath := filepath.Join(base, "victim.json")
	writeMigrationTestFile(t, victimPath, "original\n")
	encoded, err := os.ReadFile(completed.ManifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	manifest := MigrationManifest{}
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	manifest.Files[0].DestinationPath = victimPath
	encoded, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode tampered manifest: %v", err)
	}
	if err := os.WriteFile(completed.ManifestPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write tampered manifest: %v", err)
	}
	if _, err := RollbackMigration(completed.ManifestPath, true); err == nil || !strings.Contains(err.Error(), "invalid migration manifest") {
		t.Fatalf("tampered manifest unexpectedly reached rollback: %v", err)
	}
	if encoded, err := os.ReadFile(victimPath); err != nil || string(encoded) != "original\n" {
		t.Fatalf("tampered rollback changed victim: value=%q err=%v", encoded, err)
	}
	if _, err := os.Stat(legacyRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tampered rollback restored legacy root: %v", err)
	}
	if _, err := os.Stat(completed.BackupRoot); err != nil {
		t.Fatalf("tampered rollback changed backup: %v", err)
	}
}

func migrationHasBlocker(manifest MigrationManifest, reason string) bool {
	for _, blocker := range manifest.Blockers {
		if blocker.Reason == reason {
			return true
		}
	}
	return false
}
