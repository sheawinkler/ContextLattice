package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("mode for %s = %04o, want %04o", path, got, want)
	}
}

func TestOwnerOnlyMigrationStateReadRejectsOversizedArtifact(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	path := ownerOnlyStatePath(root)
	if err := os.MkdirAll(filepath.Dir(path), ownerOnlyDirectoryMode); err != nil {
		t.Fatalf("create migration state directory: %v", err)
	}
	raw := bytes.Repeat([]byte("x"), int(ownerOnlyMigrationStateMaxBytes)+1)
	if err := os.WriteFile(path, raw, ownerOnlyFileMode); err != nil {
		t.Fatalf("write oversized migration state: %v", err)
	}
	if _, err := loadOwnerOnlyMigrationState(path); !errors.Is(err, errMemoryEdgeLogOversized) {
		t.Fatalf("oversized migration state was not rejected by its cap: %v", err)
	}
}

func TestOwnerOnlyMigrationIsBoundedResumableAndIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.MkdirAll(filepath.Join(root, "project", "topic"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(root, "foo", "inside.md"),
		filepath.Join(root, "foo-bar", "inside.md"),
		filepath.Join(root, "project", "one.md"),
		filepath.Join(root, "project", "topic", "two.md"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("private"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	batchSizes := []int{}
	report, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{
		batchLimit: 1,
		readChunk:  2,
		afterBatch: func(entries int) {
			batchSizes = append(batchSizes, entries)
		},
	})
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	if !report.OK || !report.Complete || report.ProcessedEntries < 1 {
		t.Fatalf("unexpected migration report: %+v", report)
	}
	if len(batchSizes) < 2 {
		t.Fatalf("expected multiple bounded batches, got %v", batchSizes)
	}
	for _, entries := range batchSizes {
		if entries > 1 {
			t.Fatalf("batch processed %d entries, want at most 1", entries)
		}
	}
	if err := migrateOwnerOnlyStore(root); err != nil {
		t.Fatalf("idempotent completed migration failed: %v", err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, filepath.Join(root, "foo"), 0o700)
	assertMode(t, filepath.Join(root, "foo", "inside.md"), 0o600)
	assertMode(t, filepath.Join(root, "foo-bar"), 0o700)
	assertMode(t, filepath.Join(root, "foo-bar", "inside.md"), 0o600)
	assertMode(t, filepath.Join(root, "project"), 0o700)
	assertMode(t, filepath.Join(root, "project", "topic"), 0o700)
	assertMode(t, filepath.Join(root, "project", "one.md"), 0o600)
	assertMode(t, filepath.Join(root, "project", "topic", "two.md"), 0o600)
	assertMode(t, ownerOnlyStatePath(root), 0o600)
	assertMode(t, outside, 0o644)
}

func TestOwnerOnlyMigrationStreamsLargeFlatDirectoryInBoundedBatches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	flat := filepath.Join(root, "flat")
	if err := os.MkdirAll(flat, 0o755); err != nil {
		t.Fatal(err)
	}
	const files = 4097
	for index := 0; index < files; index++ {
		path := filepath.Join(flat, formatOwnerOnlyTestFileName(index))
		if err := os.WriteFile(path, []byte("private"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	batchSizes := []int{}
	report, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{
		batchLimit: ownerOnlyMigrationBatchMax,
		readChunk:  7,
		afterBatch: func(entries int) {
			batchSizes = append(batchSizes, entries)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || report.ProcessedEntries < files {
		t.Fatalf("flat migration incomplete: %+v", report)
	}
	if len(batchSizes) < 5 {
		t.Fatalf("expected at least five batches, got %v", batchSizes)
	}
	for _, entries := range batchSizes {
		if entries < 1 || entries > ownerOnlyMigrationBatchMax {
			t.Fatalf("invalid batch size %d", entries)
		}
	}
	assertMode(t, filepath.Join(flat, formatOwnerOnlyTestFileName(files-1)), 0o600)
}

func formatOwnerOnlyTestFileName(index int) string {
	return fmt.Sprintf("entry-%08d.txt", index)
}

func TestOwnerOnlyMigrationResumesAfterInterruptionByRecheckingModes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 12; index++ {
		if err := os.WriteFile(filepath.Join(root, formatOwnerOnlyTestFileName(index)), []byte("private"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seen := 0
	interrupted := errors.New("injected interruption")
	first, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{
		batchLimit: 3,
		readChunk:  2,
		beforeEntry: func(_ string) error {
			seen++
			if seen == 6 {
				return interrupted
			}
			return nil
		},
	})
	if !errors.Is(err, interrupted) {
		t.Fatalf("expected injected interruption, got report=%+v err=%v", first, err)
	}
	if first.Complete || first.ProcessedEntries == 0 {
		t.Fatalf("expected durable partial progress: %+v", first)
	}

	second, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{batchLimit: 3, readChunk: 2})
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if !second.Complete || !second.Resumed {
		t.Fatalf("expected completed resumed report: %+v", second)
	}
	for index := 0; index < 12; index++ {
		assertMode(t, filepath.Join(root, formatOwnerOnlyTestFileName(index)), 0o600)
	}
}

func TestOwnerOnlyMigrationLimitCannotExceedProgramBudget(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_MAX_ENTRIES", "999999")
	if got := ownerOnlyMigrationLimit(); got != ownerOnlyMigrationBatchMax {
		t.Fatalf("migration limit = %d, want %d", got, ownerOnlyMigrationBatchMax)
	}
}

func TestOwnerOnlyMigrationInvalidatesLegacyCompleteReceipt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	statePath := ownerOnlyStatePath(root)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"schema_id":"contextlattice_owner_only_store.v1","complete":true,"cursor":"foo/inside.md","updated_at":"2026-07-13T00:00:00Z"}`)
	if err := os.WriteFile(statePath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	filePath := filepath.Join(root, "foo-bar", "memory.md")
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filePath, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := migrateOwnerOnlyStoreWithOptions(root, ownerOnlyMigrationOptions{batchLimit: 2, readChunk: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Complete || !report.Resumed {
		t.Fatalf("legacy receipt was not reverified: %+v", report)
	}
	assertMode(t, filePath, 0o600)
	state, err := loadOwnerOnlyMigrationState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaID != ownerOnlySchemaID || !state.Complete {
		t.Fatalf("legacy receipt was not upgraded: %+v", state)
	}
}

func TestOwnerOnlyMigrationCommandEmitsOpaqueStructuredReport(t *testing.T) {
	root := filepath.Join(t.TempDir(), "private-store")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.md"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	handled, code := runOwnerOnlyMigrationCommand(
		[]string{"gateway-go", "owner-only-migrate", "--root", root},
		stdout,
		stderr,
	)
	if !handled || code != 0 {
		t.Fatalf("command failed: handled=%t code=%d stderr=%q", handled, code, stderr.String())
	}
	if strings.Contains(stdout.String(), root) {
		t.Fatalf("structured report leaked local root: %s", stdout.String())
	}
	var report ownerOnlyMigrationReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.SchemaID != ownerOnlyMigrationReportSchemaID || !report.OK || !report.Complete || report.StoreRef == "" {
		t.Fatalf("unexpected command report: %+v", report)
	}
	if err := os.Chmod(filepath.Join(root, "memory.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	handled, code = runOwnerOnlyMigrationCommand(
		[]string{"gateway-go", "owner-only-migrate", "--root", root, "--force"},
		stdout,
		stderr,
	)
	if !handled || code != 0 {
		t.Fatalf("forced command failed: handled=%t code=%d stderr=%q", handled, code, stderr.String())
	}
	assertMode(t, filepath.Join(root, "memory.md"), 0o600)
}

func TestOwnerOnlyMigrationRejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := filepath.Join(t.TempDir(), "store")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := migrateOwnerOnlyStore(root); err == nil {
		t.Fatal("expected escaping symlink to fail closed")
	}
}

func TestOwnerOnlyMigrationRejectsNestedEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	root := filepath.Join(t.TempDir(), "store")
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "redirect")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "redirect"), filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	if err := migrateOwnerOnlyStore(root); err == nil {
		t.Fatal("expected nested escaping symlink to fail closed")
	}
}

func TestEnforceOwnerOnlyPermissionsRejectsSymlinkWithoutMutatingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires additional Windows privileges")
	}
	target := filepath.Join(t.TempDir(), "target.txt")
	link := filepath.Join(t.TempDir(), "link.txt")
	if err := os.WriteFile(target, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := enforceOwnerOnlyPermissions(link, 0o600); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
	assertMode(t, target, 0o644)
}

func TestOwnerOnlyAtomicAndAppendWrites(t *testing.T) {
	root := filepath.Join(t.TempDir(), "store")
	atomicPath := filepath.Join(root, "atomic.json")
	appendPath := filepath.Join(root, "events.ndjson")
	if err := writeOwnerOnlyAtomicFile(atomicPath, []byte("{}"), true); err != nil {
		t.Fatal(err)
	}
	if err := writeOwnerOnlyAtomicFile(atomicPath, []byte("{\"version\":2}"), true); err != nil {
		t.Fatalf("replace atomic file: %v", err)
	}
	atomicRaw, err := os.ReadFile(atomicPath)
	if err != nil {
		t.Fatalf("read replaced atomic file: %v", err)
	}
	if string(atomicRaw) != "{\"version\":2}" {
		t.Fatalf("replaced atomic file = %q", atomicRaw)
	}
	durablePath := filepath.Join(root, "durable.json")
	if err := writeOwnerOnlyDurableAtomicFile(durablePath, []byte("{\"version\":1}"), true); err != nil {
		t.Fatalf("write durable file: %v", err)
	}
	if err := writeOwnerOnlyDurableAtomicFile(durablePath, []byte("{\"version\":2}"), true); err != nil {
		t.Fatalf("replace durable file: %v", err)
	}
	durableRaw, err := os.ReadFile(durablePath)
	if err != nil {
		t.Fatalf("read replaced durable file: %v", err)
	}
	if string(durableRaw) != "{\"version\":2}" {
		t.Fatalf("replaced durable file = %q", durableRaw)
	}
	file, err := openOwnerOnlyAppend(appendPath, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{}\n"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	assertMode(t, root, 0o700)
	assertMode(t, atomicPath, 0o600)
	assertMode(t, durablePath, 0o600)
	assertMode(t, appendPath, 0o600)
}

func TestOwnerOnlyExplicitFileDoesNotTightenOperatorParent(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "operator-owned")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "runtime.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareOwnerOnlyFile(path, false); err != nil {
		t.Fatal(err)
	}
	assertMode(t, parent, 0o755)
	assertMode(t, path, 0o600)
}
