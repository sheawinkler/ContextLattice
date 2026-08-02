package gatewaystate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const MigrationSchemaID = "contextlattice_gateway_state_migration.v1"

type MigrationFile struct {
	RelativePath    string `json:"relative_path"`
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
	Bytes           int64  `json:"bytes"`
	SHA256          string `json:"sha256"`
	Action          string `json:"action"`
	Applied         bool   `json:"applied"`
	RolledBack      bool   `json:"rolled_back"`
}

type MigrationBlocker struct {
	Reason string `json:"reason"`
	Path   string `json:"path,omitempty"`
	Detail string `json:"detail"`
}

type MigrationManifest struct {
	OK                    bool               `json:"ok"`
	SchemaID              string             `json:"schema_id"`
	MigrationID           string             `json:"migration_id"`
	Status                string             `json:"status"`
	DryRun                bool               `json:"dry_run"`
	LegacyRoot            string             `json:"legacy_root"`
	LegacyKind            string             `json:"legacy_kind"`
	StateRoot             string             `json:"state_root"`
	BackupRoot            string             `json:"backup_root"`
	ManifestPath          string             `json:"manifest_path"`
	CapturedAt            string             `json:"captured_at"`
	AppliedAt             string             `json:"applied_at,omitempty"`
	RolledBackAt          string             `json:"rolled_back_at,omitempty"`
	PlanDigest            string             `json:"plan_digest"`
	FileCount             int                `json:"file_count"`
	CopyCount             int                `json:"copy_count"`
	AlreadyPresentCount   int                `json:"already_present_count"`
	AppliedCopyCount      int                `json:"applied_copy_count"`
	RollbackRemovedCount  int                `json:"rollback_removed_count"`
	RequiredBytes         uint64             `json:"required_bytes"`
	AvailableBytes        uint64             `json:"available_bytes"`
	FreeSpaceProbe        string             `json:"free_space_probe"`
	Files                 []MigrationFile    `json:"files"`
	Blockers              []MigrationBlocker `json:"blockers"`
	Failure               string             `json:"failure,omitempty"`
	DuplicatePolicy       string             `json:"duplicate_policy"`
	BackupPolicy          string             `json:"backup_policy"`
	IntentionalBackupCopy string             `json:"intentional_backup_copy,omitempty"`
}

type AvailableBytesFunc func(path string) (uint64, string, error)

type MigrationOptions struct {
	LegacyRoot     string
	StateRoot      string
	Apply          bool
	Confirm        bool
	Now            func() time.Time
	AvailableBytes AvailableBytesFunc
	BeforeCopy     func(MigrationFile) error
	BeforeCommit   func() error
}

func migrationNow(options MigrationOptions) time.Time {
	if options.Now != nil {
		return options.Now().UTC()
	}
	return time.Now().UTC()
}

func migrationAbsolutePath(raw string) (string, error) {
	return migrationAbsoluteField("path", strings.TrimSpace(raw))
}

func pathContains(parent string, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func migrationAbsoluteField(label string, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%s must be an absolute path", label)
	}
	cleaned := filepath.Clean(raw)
	if cleaned != raw {
		return "", fmt.Errorf("%s must be a clean absolute path", label)
	}
	return cleaned, nil
}

func validateMigrationRelativePath(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" || filepath.IsAbs(raw) {
		return "", errors.New("migration relative path must be non-empty and relative")
	}
	cleaned := filepath.Clean(raw)
	if cleaned != raw || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", errors.New("migration relative path is not cleanly bounded")
	}
	firstComponent := strings.Split(cleaned, string(filepath.Separator))[0]
	if strings.EqualFold(firstComponent, ".migrations") {
		return "", errors.New(".migrations is reserved for ContextLattice migration receipts")
	}
	return cleaned, nil
}

func validateNoSymlinkWithin(root string, target string) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if !pathContains(root, target) {
		return fmt.Errorf("path %s escaped root %s", target, root)
	}
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	components := []string{}
	if relative != "." {
		components = strings.Split(relative, string(filepath.Separator))
	}
	current := root
	for index := -1; index < len(components); index++ {
		if index >= 0 {
			current = filepath.Join(current, components[index])
		}
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return nil
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink path component is not allowed: %s", current)
		}
		if index < len(components)-1 && !info.IsDir() {
			return fmt.Errorf("non-directory path component: %s", current)
		}
	}
	return nil
}

func validateRegularMigrationFile(root string, path string) error {
	if err := validateNoSymlinkWithin(root, path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("migration path must be a regular non-symlink file")
	}
	return nil
}

func migrationDigest(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	bytesWritten, err := io.Copy(hash, file)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), bytesWritten, nil
}

func addMigrationBlocker(manifest *MigrationManifest, reason string, path string, detail string) {
	manifest.Blockers = append(manifest.Blockers, MigrationBlocker{Reason: reason, Path: path, Detail: detail})
}

func migrationSeedDigest(legacyRoot string, legacyKind string, stateRoot string, files []MigrationFile) string {
	type planFile struct {
		RelativePath    string `json:"relative_path"`
		SourcePath      string `json:"source_path"`
		DestinationPath string `json:"destination_path"`
		Bytes           int64  `json:"bytes"`
		SHA256          string `json:"sha256"`
		Action          string `json:"action"`
	}
	planFiles := make([]planFile, 0, len(files))
	for _, file := range files {
		planFiles = append(planFiles, planFile{
			RelativePath: file.RelativePath, SourcePath: file.SourcePath,
			DestinationPath: file.DestinationPath, Bytes: file.Bytes,
			SHA256: file.SHA256, Action: file.Action,
		})
	}
	seed := struct {
		LegacyRoot string     `json:"legacy_root"`
		LegacyKind string     `json:"legacy_kind"`
		StateRoot  string     `json:"state_root"`
		Files      []planFile `json:"files"`
	}{LegacyRoot: legacyRoot, LegacyKind: legacyKind, StateRoot: stateRoot, Files: planFiles}
	encoded, _ := json.Marshal(seed)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func nearestMigrationSpaceRoot(path string) (string, error) {
	candidate := filepath.Clean(path)
	for {
		info, err := os.Stat(candidate)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("nearest existing state-root ancestor is not a directory: %s", candidate)
			}
			return candidate, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", err
		}
		candidate = parent
	}
}

func appendPlannedMigrationFile(manifest *MigrationManifest, legacyRoot string, stateRoot string, sourcePath string, rawRelativePath string) {
	relativePath, err := validateMigrationRelativePath(rawRelativePath)
	if err != nil {
		reason := "legacy_path_escape"
		if strings.Contains(err.Error(), ".migrations") {
			reason = "reserved_destination_namespace"
		}
		addMigrationBlocker(manifest, reason, sourcePath, err.Error())
		return
	}
	if err := validateRegularMigrationFile(legacyRoot, sourcePath); err != nil {
		addMigrationBlocker(manifest, "unsupported_legacy_entry", sourcePath, err.Error())
		return
	}
	digest, size, err := migrationDigest(sourcePath)
	if err != nil {
		addMigrationBlocker(manifest, "legacy_digest_failed", sourcePath, err.Error())
		return
	}
	destinationPath := filepath.Clean(filepath.Join(stateRoot, relativePath))
	if !pathContains(stateRoot, destinationPath) {
		addMigrationBlocker(manifest, "destination_path_escape", destinationPath, "resolved destination escaped the canonical state root")
		return
	}
	if err := validateNoSymlinkWithin(stateRoot, destinationPath); err != nil {
		addMigrationBlocker(manifest, "destination_symlink_path", destinationPath, err.Error())
		return
	}
	file := MigrationFile{
		RelativePath: relativePath, SourcePath: sourcePath, DestinationPath: destinationPath,
		Bytes: size, SHA256: digest, Action: "copy",
	}
	destinationInfo, destinationErr := os.Lstat(destinationPath)
	switch {
	case destinationErr == nil && destinationInfo.Mode().IsRegular():
		destinationDigest, destinationSize, destinationDigestErr := migrationDigest(destinationPath)
		if destinationDigestErr != nil {
			addMigrationBlocker(manifest, "destination_digest_failed", destinationPath, destinationDigestErr.Error())
		} else if destinationDigest == digest && destinationSize == size {
			file.Action = "already_present"
		} else {
			addMigrationBlocker(manifest, "destination_conflict", destinationPath, "destination exists with different content")
		}
	case destinationErr == nil:
		addMigrationBlocker(manifest, "destination_type_conflict", destinationPath, "destination exists and is not a regular file")
	case !errors.Is(destinationErr, os.ErrNotExist):
		addMigrationBlocker(manifest, "destination_stat_failed", destinationPath, destinationErr.Error())
	}
	manifest.Files = append(manifest.Files, file)
}

func PlanMigration(options MigrationOptions) (MigrationManifest, error) {
	now := migrationNow(options)
	manifest := MigrationManifest{
		SchemaID:        MigrationSchemaID,
		Status:          "planning",
		DryRun:          true,
		CapturedAt:      now.Format(time.RFC3339Nano),
		Files:           []MigrationFile{},
		Blockers:        []MigrationBlocker{},
		DuplicatePolicy: "same-path same-SHA256 files are reused; different content blocks before mutation",
		BackupPolicy:    "successful apply renames the selected legacy root to one timestamped rollback backup; the backup is intentional duplication until explicit rollback or operator retirement",
	}
	legacyRoot, err := migrationAbsolutePath(options.LegacyRoot)
	if err != nil {
		return manifest, fmt.Errorf("resolve legacy root: %w", err)
	}
	stateRootRaw := strings.TrimSpace(options.StateRoot)
	if stateRootRaw == "" {
		stateRootRaw = ResolveRoot().Path
	}
	stateRoot, err := migrationAbsolutePath(stateRootRaw)
	if err != nil {
		return manifest, fmt.Errorf("resolve state root: %w", err)
	}
	manifest.LegacyRoot = legacyRoot
	manifest.StateRoot = stateRoot

	rootsOverlap := pathContains(legacyRoot, stateRoot) || pathContains(stateRoot, legacyRoot)
	if rootsOverlap {
		addMigrationBlocker(&manifest, "root_overlap", stateRoot, "legacy root and canonical state root must not contain one another")
	}
	if stateInfo, stateErr := os.Lstat(stateRoot); stateErr == nil {
		switch {
		case stateInfo.Mode()&os.ModeSymlink != 0:
			addMigrationBlocker(&manifest, "state_root_symlink", stateRoot, "canonical state root must be an explicit directory, not a symlink")
		case !stateInfo.IsDir():
			addMigrationBlocker(&manifest, "state_root_not_directory", stateRoot, "canonical state root must be a directory")
		}
	} else if !errors.Is(stateErr, os.ErrNotExist) {
		addMigrationBlocker(&manifest, "state_root_stat_failed", stateRoot, stateErr.Error())
	}

	legacyInfo, legacyErr := os.Lstat(legacyRoot)
	if legacyErr != nil {
		addMigrationBlocker(&manifest, "legacy_root_unavailable", legacyRoot, legacyErr.Error())
	} else if legacyInfo.Mode()&os.ModeSymlink != 0 {
		addMigrationBlocker(&manifest, "legacy_root_symlink", legacyRoot, "legacy root must be an explicit file or directory, not a symlink")
	} else if legacyInfo.Mode().IsRegular() {
		manifest.LegacyKind = "file"
		if !rootsOverlap {
			appendPlannedMigrationFile(&manifest, legacyRoot, stateRoot, legacyRoot, filepath.Base(legacyRoot))
		}
	} else if legacyInfo.IsDir() {
		manifest.LegacyKind = "directory"
		if !rootsOverlap {
			walkErr := filepath.WalkDir(legacyRoot, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if sourcePath == legacyRoot || entry.IsDir() {
					return nil
				}
				relativePath, relErr := filepath.Rel(legacyRoot, sourcePath)
				if relErr != nil {
					addMigrationBlocker(&manifest, "legacy_path_escape", sourcePath, "legacy entry could not be bounded beneath the selected root")
					return nil
				}
				appendPlannedMigrationFile(&manifest, legacyRoot, stateRoot, sourcePath, relativePath)
				return nil
			})
			if walkErr != nil {
				addMigrationBlocker(&manifest, "legacy_walk_failed", legacyRoot, walkErr.Error())
			}
		}
	} else {
		addMigrationBlocker(&manifest, "legacy_root_unsupported", legacyRoot, "legacy root must be a regular file or directory")
	}

	sort.Slice(manifest.Files, func(i int, j int) bool {
		return manifest.Files[i].RelativePath < manifest.Files[j].RelativePath
	})
	for _, file := range manifest.Files {
		switch file.Action {
		case "copy":
			manifest.CopyCount++
			manifest.RequiredBytes += uint64(file.Bytes)
		case "already_present":
			manifest.AlreadyPresentCount++
		}
	}
	manifest.FileCount = len(manifest.Files)
	manifest.PlanDigest = migrationSeedDigest(legacyRoot, manifest.LegacyKind, stateRoot, manifest.Files)
	timestamp := now.Format("20060102T150405Z")
	manifest.MigrationID = "gateway-state-" + timestamp + "-" + manifest.PlanDigest[:12]
	manifest.BackupRoot = legacyRoot + ".contextlattice-backup-" + manifest.MigrationID
	manifest.ManifestPath = filepath.Join(stateRoot, ".migrations", manifest.MigrationID+".json")
	if _, backupErr := os.Lstat(manifest.BackupRoot); backupErr == nil {
		addMigrationBlocker(&manifest, "backup_conflict", manifest.BackupRoot, "planned rollback backup already exists")
	} else if !errors.Is(backupErr, os.ErrNotExist) {
		addMigrationBlocker(&manifest, "backup_stat_failed", manifest.BackupRoot, backupErr.Error())
	}

	spaceRoot, spaceRootErr := nearestMigrationSpaceRoot(stateRoot)
	if spaceRootErr != nil {
		addMigrationBlocker(&manifest, "free_space_root_unavailable", stateRoot, spaceRootErr.Error())
	} else {
		probe := options.AvailableBytes
		if probe == nil {
			probe = diskAvailableBytes
		}
		available, probeName, probeErr := probe(spaceRoot)
		manifest.AvailableBytes = available
		manifest.FreeSpaceProbe = probeName
		if probeErr != nil {
			addMigrationBlocker(&manifest, "free_space_probe_failed", spaceRoot, probeErr.Error())
		} else if manifest.RequiredBytes > available {
			addMigrationBlocker(&manifest, "insufficient_free_space", stateRoot, fmt.Sprintf("required_bytes=%d available_bytes=%d", manifest.RequiredBytes, available))
		}
	}

	manifest.OK = len(manifest.Blockers) == 0
	switch {
	case !manifest.OK:
		manifest.Status = "blocked"
	case manifest.FileCount == 0:
		manifest.Status = "nothing_to_migrate"
	default:
		manifest.Status = "dry_run_ready"
	}
	return manifest, nil
}

func validateMigrationManifestBinding(manifest MigrationManifest, manifestPath string) error {
	legacyRoot, err := migrationAbsoluteField("legacy_root", manifest.LegacyRoot)
	if err != nil {
		return err
	}
	stateRoot, err := migrationAbsoluteField("state_root", manifest.StateRoot)
	if err != nil {
		return err
	}
	backupRoot, err := migrationAbsoluteField("backup_root", manifest.BackupRoot)
	if err != nil {
		return err
	}
	manifestPath, err = migrationAbsoluteField("manifest_path", manifestPath)
	if err != nil {
		return err
	}
	if pathContains(legacyRoot, stateRoot) || pathContains(stateRoot, legacyRoot) {
		return errors.New("manifest roots overlap")
	}
	if manifest.LegacyKind != "directory" && manifest.LegacyKind != "file" {
		return fmt.Errorf("manifest legacy_kind is invalid: %q", manifest.LegacyKind)
	}
	if manifest.LegacyKind == "file" && len(manifest.Files) != 1 {
		return errors.New("file-root migration manifest must contain exactly one file")
	}
	if strings.TrimSpace(manifest.MigrationID) == "" || filepath.Base(manifest.MigrationID) != manifest.MigrationID || !strings.HasPrefix(manifest.MigrationID, "gateway-state-") {
		return errors.New("manifest migration_id is invalid")
	}
	expectedManifestPath := filepath.Join(stateRoot, ".migrations", manifest.MigrationID+".json")
	if manifestPath != expectedManifestPath {
		return fmt.Errorf("manifest path is not bound to state root: got=%s expected=%s", manifestPath, expectedManifestPath)
	}
	expectedBackupRoot := legacyRoot + ".contextlattice-backup-" + manifest.MigrationID
	if backupRoot != expectedBackupRoot {
		return fmt.Errorf("backup root is not bound to migration id: got=%s expected=%s", backupRoot, expectedBackupRoot)
	}
	if err := validateNoSymlinkWithin(stateRoot, manifestPath); err != nil {
		return fmt.Errorf("manifest path validation: %w", err)
	}
	copyCount := 0
	alreadyPresentCount := 0
	appliedCopyCount := 0
	requiredBytes := uint64(0)
	for index, file := range manifest.Files {
		relativePath, relativeErr := validateMigrationRelativePath(file.RelativePath)
		if relativeErr != nil {
			return fmt.Errorf("manifest file %d relative path: %w", index, relativeErr)
		}
		expectedSource := filepath.Join(legacyRoot, relativePath)
		if manifest.LegacyKind == "file" {
			if relativePath != filepath.Base(legacyRoot) {
				return fmt.Errorf("manifest file %d name is not bound to the legacy file root", index)
			}
			expectedSource = legacyRoot
		}
		expectedDestination := filepath.Join(stateRoot, relativePath)
		if file.SourcePath != expectedSource || file.DestinationPath != expectedDestination {
			return fmt.Errorf("manifest file %d paths are not root-bound", index)
		}
		if file.Bytes < 0 {
			return fmt.Errorf("manifest file %d has a negative byte count", index)
		}
		decodedDigest, digestErr := hex.DecodeString(file.SHA256)
		if digestErr != nil || len(decodedDigest) != sha256.Size || strings.ToLower(file.SHA256) != file.SHA256 {
			return fmt.Errorf("manifest file %d has an invalid sha256", index)
		}
		switch file.Action {
		case "copy":
			copyCount++
			requiredBytes += uint64(file.Bytes)
			if file.Applied {
				appliedCopyCount++
			}
		case "already_present":
			alreadyPresentCount++
		default:
			return fmt.Errorf("manifest file %d has unsupported action %q", index, file.Action)
		}
	}
	if manifest.FileCount != len(manifest.Files) || manifest.CopyCount != copyCount || manifest.AlreadyPresentCount != alreadyPresentCount || manifest.AppliedCopyCount != appliedCopyCount || manifest.RequiredBytes != requiredBytes {
		return errors.New("manifest counters do not match the bound file plan")
	}
	expectedDigest := migrationSeedDigest(legacyRoot, manifest.LegacyKind, stateRoot, manifest.Files)
	if manifest.PlanDigest != expectedDigest || !strings.HasSuffix(manifest.MigrationID, "-"+expectedDigest[:12]) {
		return errors.New("manifest plan digest or migration id suffix does not match the bound file plan")
	}
	if (manifest.Status == "prepared" || manifest.Status == "completed") && appliedCopyCount != copyCount {
		return errors.New("rollback-eligible manifest does not mark every copied file as applied")
	}
	return nil
}

func verifyPreparedMigration(manifest MigrationManifest) error {
	for _, file := range manifest.Files {
		if err := validateRegularMigrationFile(manifest.LegacyRoot, file.SourcePath); err != nil {
			return fmt.Errorf("source %s: %w", file.RelativePath, err)
		}
		sourceDigest, sourceBytes, err := migrationDigest(file.SourcePath)
		if err != nil || sourceDigest != file.SHA256 || sourceBytes != file.Bytes {
			if err != nil {
				return fmt.Errorf("source %s changed after planning: %w", file.RelativePath, err)
			}
			return fmt.Errorf("source %s changed after planning", file.RelativePath)
		}
		if err := validateRegularMigrationFile(manifest.StateRoot, file.DestinationPath); err != nil {
			return fmt.Errorf("destination %s: %w", file.RelativePath, err)
		}
		destinationDigest, destinationBytes, err := migrationDigest(file.DestinationPath)
		if err != nil || destinationDigest != file.SHA256 || destinationBytes != file.Bytes {
			if err != nil {
				return fmt.Errorf("destination %s changed after planning: %w", file.RelativePath, err)
			}
			return fmt.Errorf("destination %s changed after planning", file.RelativePath)
		}
	}
	return nil
}

func writeMigrationManifest(path string, manifest MigrationManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".gateway-state-manifest-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := replaceFileAtomic(temporaryPath, path); err != nil {
		return err
	}
	return nil
}

func copyMigrationFileAtomic(file MigrationFile, stateRoot string) error {
	sourceInfo, err := os.Lstat(file.SourcePath)
	if err != nil {
		return err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
		return errors.New("source is no longer a regular non-symlink file")
	}
	if err := os.MkdirAll(filepath.Dir(file.DestinationPath), 0o700); err != nil {
		return err
	}
	if err := validateNoSymlinkWithin(stateRoot, filepath.Dir(file.DestinationPath)); err != nil {
		return err
	}
	if _, err := os.Lstat(file.DestinationPath); err == nil {
		return errors.New("destination appeared after migration planning")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	source, err := os.Open(file.SourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	temporary, err := os.CreateTemp(filepath.Dir(file.DestinationPath), ".gateway-state-copy-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(sourceInfo.Mode().Perm()); err != nil {
		_ = temporary.Close()
		return err
	}
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), source)
	if err != nil {
		_ = temporary.Close()
		return err
	}
	if written != file.Bytes || hex.EncodeToString(hash.Sum(nil)) != file.SHA256 {
		_ = temporary.Close()
		return errors.New("source changed after migration planning")
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, file.DestinationPath); err != nil {
		return err
	}
	return nil
}

func cleanupCreatedMigrationFiles(files []MigrationFile, stateRoot string) error {
	var failures []string
	for index := len(files) - 1; index >= 0; index-- {
		file := files[index]
		if file.Action != "copy" || !file.Applied {
			continue
		}
		digest, size, err := migrationDigest(file.DestinationPath)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", file.DestinationPath, err))
			continue
		}
		if digest != file.SHA256 || size != file.Bytes {
			failures = append(failures, file.DestinationPath+": destination changed; refused cleanup")
			continue
		}
		if err := os.Remove(file.DestinationPath); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", file.DestinationPath, err))
			continue
		}
		removeEmptyMigrationParents(filepath.Dir(file.DestinationPath), stateRoot)
	}
	if len(failures) > 0 {
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}

func removeEmptyMigrationParents(start string, stop string) {
	candidate := filepath.Clean(start)
	stop = filepath.Clean(stop)
	for candidate != stop && pathContains(stop, candidate) {
		if err := os.Remove(candidate); err != nil {
			return
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return
		}
		candidate = parent
	}
}

func ExecuteMigration(options MigrationOptions) (MigrationManifest, error) {
	manifest, err := PlanMigration(options)
	if err != nil {
		return manifest, err
	}
	if !options.Apply {
		return manifest, nil
	}
	manifest.DryRun = false
	if !options.Confirm {
		manifest.OK = false
		manifest.Status = "confirmation_required"
		return manifest, errors.New("migration apply requires both --apply and --yes")
	}
	if !manifest.OK {
		return manifest, errors.New("migration is blocked; resolve every reported blocker before apply")
	}
	if manifest.FileCount == 0 {
		manifest.Status = "nothing_to_migrate"
		return manifest, nil
	}
	if err := os.MkdirAll(manifest.StateRoot, 0o700); err != nil {
		manifest.OK = false
		manifest.Status = "failed"
		manifest.Failure = err.Error()
		return manifest, fmt.Errorf("create canonical state root: %w", err)
	}
	if err := validateNoSymlinkWithin(manifest.StateRoot, manifest.StateRoot); err != nil {
		manifest.OK = false
		manifest.Status = "failed"
		manifest.Failure = err.Error()
		return manifest, fmt.Errorf("validate canonical state root: %w", err)
	}
	for index := range manifest.Files {
		file := manifest.Files[index]
		if file.Action != "copy" {
			continue
		}
		if options.BeforeCopy != nil {
			if err := options.BeforeCopy(file); err != nil {
				manifest.OK = false
				manifest.Status = "failed"
				manifest.Failure = err.Error()
				cleanupErr := cleanupCreatedMigrationFiles(manifest.Files, manifest.StateRoot)
				if cleanupErr != nil {
					manifest.Failure += "; cleanup: " + cleanupErr.Error()
				}
				return manifest, fmt.Errorf("migration copy injection failed: %w", err)
			}
		}
		if err := copyMigrationFileAtomic(file, manifest.StateRoot); err != nil {
			manifest.OK = false
			manifest.Status = "failed"
			manifest.Failure = fmt.Sprintf("copy %s: %v", file.RelativePath, err)
			cleanupErr := cleanupCreatedMigrationFiles(manifest.Files, manifest.StateRoot)
			if cleanupErr != nil {
				manifest.Failure += "; cleanup: " + cleanupErr.Error()
			}
			return manifest, errors.New(manifest.Failure)
		}
		manifest.Files[index].Applied = true
		manifest.AppliedCopyCount++
	}
	if options.BeforeCommit != nil {
		if err := options.BeforeCommit(); err != nil {
			manifest.OK = false
			manifest.Status = "failed"
			manifest.Failure = "pre-commit hook: " + err.Error()
			cleanupErr := cleanupCreatedMigrationFiles(manifest.Files, manifest.StateRoot)
			if cleanupErr != nil {
				manifest.Failure += "; cleanup: " + cleanupErr.Error()
			}
			return manifest, errors.New(manifest.Failure)
		}
	}
	if err := verifyPreparedMigration(manifest); err != nil {
		manifest.OK = false
		manifest.Status = "failed"
		manifest.Failure = "pre-commit validation: " + err.Error()
		cleanupErr := cleanupCreatedMigrationFiles(manifest.Files, manifest.StateRoot)
		if cleanupErr != nil {
			manifest.Failure += "; cleanup: " + cleanupErr.Error()
		}
		return manifest, errors.New(manifest.Failure)
	}
	manifest.Status = "prepared"
	if err := writeMigrationManifest(manifest.ManifestPath, manifest); err != nil {
		manifest.OK = false
		manifest.Status = "failed"
		manifest.Failure = "write prepared manifest: " + err.Error()
		cleanupErr := cleanupCreatedMigrationFiles(manifest.Files, manifest.StateRoot)
		if cleanupErr != nil {
			manifest.Failure += "; cleanup: " + cleanupErr.Error()
		}
		return manifest, errors.New(manifest.Failure)
	}
	if err := os.Rename(manifest.LegacyRoot, manifest.BackupRoot); err != nil {
		manifest.OK = false
		manifest.Status = "failed"
		manifest.Failure = "create rollback backup: " + err.Error()
		cleanupErr := cleanupCreatedMigrationFiles(manifest.Files, manifest.StateRoot)
		if cleanupErr != nil {
			manifest.Failure += "; cleanup: " + cleanupErr.Error()
		}
		_ = writeMigrationManifest(manifest.ManifestPath, manifest)
		return manifest, errors.New(manifest.Failure)
	}
	manifest.OK = true
	manifest.Status = "completed"
	manifest.AppliedAt = migrationNow(options).Format(time.RFC3339Nano)
	manifest.IntentionalBackupCopy = manifest.BackupRoot
	if err := writeMigrationManifest(manifest.ManifestPath, manifest); err != nil {
		return manifest, fmt.Errorf("migration completed but final manifest update failed: %w", err)
	}
	return manifest, nil
}

func readMigrationManifest(path string) (MigrationManifest, error) {
	manifest := MigrationManifest{}
	resolvedPath, err := migrationAbsoluteField("manifest_path", strings.TrimSpace(path))
	if err != nil {
		return manifest, err
	}
	info, err := os.Lstat(resolvedPath)
	if err != nil {
		return manifest, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return manifest, errors.New("migration manifest must be a regular non-symlink file")
	}
	encoded, err := os.ReadFile(resolvedPath)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return manifest, err
	}
	if manifest.SchemaID != MigrationSchemaID {
		return manifest, fmt.Errorf("unsupported migration manifest schema: %s", manifest.SchemaID)
	}
	manifest.ManifestPath = resolvedPath
	if err := validateMigrationManifestBinding(manifest, resolvedPath); err != nil {
		return manifest, fmt.Errorf("invalid migration manifest: %w", err)
	}
	return manifest, nil
}

func RollbackMigration(manifestPath string, confirm bool) (MigrationManifest, error) {
	resolvedManifestPath, err := migrationAbsolutePath(manifestPath)
	if err != nil {
		return MigrationManifest{}, fmt.Errorf("resolve manifest path: %w", err)
	}
	manifest, err := readMigrationManifest(resolvedManifestPath)
	if err != nil {
		return manifest, err
	}
	if !confirm {
		manifest.OK = false
		manifest.Status = "rollback_confirmation_required"
		return manifest, errors.New("migration rollback requires --yes")
	}
	if manifest.Status != "completed" && manifest.Status != "prepared" {
		return manifest, fmt.Errorf("migration status %q is not rollback-eligible", manifest.Status)
	}
	rollbackBlockers := []MigrationBlocker{}
	backupInfo, backupErr := os.Lstat(manifest.BackupRoot)
	if backupErr != nil {
		rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "backup_unavailable", Path: manifest.BackupRoot, Detail: backupErr.Error()})
	} else if backupInfo.Mode()&os.ModeSymlink != 0 ||
		(manifest.LegacyKind == "directory" && !backupInfo.IsDir()) ||
		(manifest.LegacyKind == "file" && !backupInfo.Mode().IsRegular()) {
		rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "backup_invalid", Path: manifest.BackupRoot, Detail: "rollback backup type must match the non-symlink legacy root"})
	}
	if _, legacyErr := os.Lstat(manifest.LegacyRoot); legacyErr == nil {
		rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "legacy_root_conflict", Path: manifest.LegacyRoot, Detail: "legacy root already exists"})
	} else if !errors.Is(legacyErr, os.ErrNotExist) {
		rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "legacy_root_stat_failed", Path: manifest.LegacyRoot, Detail: legacyErr.Error()})
	}
	for _, file := range manifest.Files {
		backupFile := filepath.Join(manifest.BackupRoot, file.RelativePath)
		if manifest.LegacyKind == "file" {
			backupFile = manifest.BackupRoot
		}
		if validationErr := validateRegularMigrationFile(manifest.BackupRoot, backupFile); validationErr != nil {
			rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "backup_path_invalid", Path: backupFile, Detail: validationErr.Error()})
			continue
		}
		backupDigest, backupSize, backupDigestErr := migrationDigest(backupFile)
		if backupDigestErr != nil || backupDigest != file.SHA256 || backupSize != file.Bytes {
			detail := "backup file digest does not match migration manifest"
			if backupDigestErr != nil {
				detail = backupDigestErr.Error()
			}
			rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "backup_digest_mismatch", Path: backupFile, Detail: detail})
		}
		if file.Action != "copy" {
			continue
		}
		if validationErr := validateRegularMigrationFile(manifest.StateRoot, file.DestinationPath); validationErr != nil {
			rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "destination_path_invalid", Path: file.DestinationPath, Detail: validationErr.Error()})
			continue
		}
		destinationDigest, destinationSize, destinationDigestErr := migrationDigest(file.DestinationPath)
		if destinationDigestErr != nil || destinationDigest != file.SHA256 || destinationSize != file.Bytes {
			detail := "canonical destination changed after migration; rollback refuses to remove it"
			if destinationDigestErr != nil {
				detail = destinationDigestErr.Error()
			}
			rollbackBlockers = append(rollbackBlockers, MigrationBlocker{Reason: "destination_changed", Path: file.DestinationPath, Detail: detail})
		}
	}
	if len(rollbackBlockers) > 0 {
		manifest.OK = false
		manifest.Status = "rollback_blocked"
		manifest.Blockers = rollbackBlockers
		return manifest, errors.New("rollback blocked; no files were changed")
	}
	if err := os.Rename(manifest.BackupRoot, manifest.LegacyRoot); err != nil {
		manifest.OK = false
		manifest.Status = "rollback_failed"
		manifest.Failure = err.Error()
		return manifest, fmt.Errorf("restore legacy root: %w", err)
	}
	for index := range manifest.Files {
		file := manifest.Files[index]
		if file.Action != "copy" {
			continue
		}
		digest, size, digestErr := migrationDigest(file.DestinationPath)
		if digestErr != nil || digest != file.SHA256 || size != file.Bytes {
			manifest.OK = false
			manifest.Status = "rollback_partial"
			manifest.Failure = "canonical destination changed during rollback; legacy root was restored but destination was preserved"
			_ = writeMigrationManifest(manifest.ManifestPath, manifest)
			return manifest, errors.New(manifest.Failure)
		}
		if err := os.Remove(file.DestinationPath); err != nil {
			manifest.OK = false
			manifest.Status = "rollback_partial"
			manifest.Failure = fmt.Sprintf("legacy root restored but canonical destination removal failed: %v", err)
			_ = writeMigrationManifest(manifest.ManifestPath, manifest)
			return manifest, errors.New(manifest.Failure)
		}
		manifest.Files[index].RolledBack = true
		manifest.RollbackRemovedCount++
		removeEmptyMigrationParents(filepath.Dir(file.DestinationPath), manifest.StateRoot)
	}
	manifest.OK = true
	manifest.Status = "rolled_back"
	manifest.RolledBackAt = time.Now().UTC().Format(time.RFC3339Nano)
	manifest.IntentionalBackupCopy = ""
	if err := writeMigrationManifest(manifest.ManifestPath, manifest); err != nil {
		return manifest, fmt.Errorf("rollback completed but manifest update failed: %w", err)
	}
	return manifest, nil
}
