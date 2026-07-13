package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ownerOnlyDirectoryMode = os.FileMode(0o700)
	ownerOnlyFileMode      = os.FileMode(0o600)
	ownerOnlySchemaID      = "contextlattice_owner_only_store.v1"
	ownerOnlyStateFile     = "owner_only_v1.json"
)

var errOwnerOnlyMigrationIncomplete = errors.New("owner-only store migration incomplete")

type ownerOnlyMigrationState struct {
	SchemaID  string `json:"schema_id"`
	Complete  bool   `json:"complete"`
	Cursor    string `json:"cursor,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

func ownerOnlyStoreRef(kind string) string {
	digest := sha256.Sum256([]byte("contextlattice-store:" + strings.TrimSpace(strings.ToLower(kind))))
	return "store_" + hex.EncodeToString(digest[:8])
}

func ensureOwnerOnlyDirectory(path string, tightenExisting bool) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return errors.New("owner-only directory path is empty")
	}
	info, err := os.Lstat(clean)
	created := false
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(clean, ownerOnlyDirectoryMode); err != nil {
			return err
		}
		created = true
		info, err = os.Lstat(clean)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("owner-only directory is not a real directory")
	}
	if !tightenExisting && !created {
		return nil
	}
	return enforceOwnerOnlyPermissions(clean, ownerOnlyDirectoryMode)
}

func ensureOwnerOnlyFile(path string) error {
	info, err := os.Lstat(filepath.Clean(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("owner-only file is not a real regular file")
	}
	return enforceOwnerOnlyPermissions(path, ownerOnlyFileMode)
}

func prepareOwnerOnlyFile(path string, dedicatedParent bool) error {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" || clean == "." {
		return errors.New("owner-only file path is empty")
	}
	if err := ensureOwnerOnlyDirectory(filepath.Dir(clean), dedicatedParent); err != nil {
		return err
	}
	return ensureOwnerOnlyFile(clean)
}

func openOwnerOnlyAppend(path string, dedicatedParent bool) (*os.File, error) {
	if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, ownerOnlyFileMode)
	if err != nil {
		return nil, err
	}
	if err := enforceOwnerOnlyPermissions(path, ownerOnlyFileMode); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func writeOwnerOnlyAtomicFile(path string, content []byte, dedicatedParent bool) error {
	if err := prepareOwnerOnlyFile(path, dedicatedParent); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(tmpPath)
	}
	if err := enforceOwnerOnlyPermissions(tmpPath, ownerOnlyFileMode); err != nil {
		cleanup()
		return err
	}
	if _, err := file.Write(content); err != nil {
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return ensureOwnerOnlyFile(path)
}

func ownerOnlyPathWithinRoot(root string, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel))
}

func validateOwnerOnlyInternalSymlink(root string, path string) error {
	target, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootResolved, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	targetResolved, err := filepath.EvalSymlinks(targetAbs)
	if err != nil {
		return err
	}
	if !ownerOnlyPathWithinRoot(rootResolved, targetResolved) {
		return errors.New("owner-only store symlink escapes declared root")
	}
	return nil
}

func ownerOnlyMigrationLimit() int {
	return clampInt(envInt("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_MAX_ENTRIES", 250000), 1, 1000000)
}

func ownerOnlyStatePath(root string) string {
	return filepath.Join(filepath.Clean(root), "_contextlattice", ownerOnlyStateFile)
}

func loadOwnerOnlyMigrationState(path string) (ownerOnlyMigrationState, error) {
	state := ownerOnlyMigrationState{SchemaID: ownerOnlySchemaID}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		return ownerOnlyMigrationState{}, errors.New("owner-only migration state is invalid")
	}
	if state.SchemaID != ownerOnlySchemaID {
		return ownerOnlyMigrationState{}, errors.New("owner-only migration state schema mismatch")
	}
	return state, nil
}

func persistOwnerOnlyMigrationState(path string, state ownerOnlyMigrationState) error {
	state.SchemaID = ownerOnlySchemaID
	state.UpdatedAt = nowUTCISO()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeOwnerOnlyAtomicFile(path, raw, true)
}

func migrateOwnerOnlyStore(root string) error {
	root = filepath.Clean(strings.TrimSpace(root))
	if root == "" || root == "." {
		return errors.New("owner-only store root is empty")
	}
	if err := ensureOwnerOnlyDirectory(root, true); err != nil {
		return err
	}
	statePath := ownerOnlyStatePath(root)
	if err := ensureOwnerOnlyDirectory(filepath.Dir(statePath), true); err != nil {
		return err
	}
	state, err := loadOwnerOnlyMigrationState(statePath)
	if err != nil {
		return err
	}
	if state.Complete {
		return ensureOwnerOnlyFile(statePath)
	}

	limit := ownerOnlyMigrationLimit()
	processed := 0
	lastCursor := state.Cursor
	stop := errors.New("owner-only migration batch limit reached")
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !ownerOnlyPathWithinRoot(root, path) {
			return errors.New("owner-only migration path escaped declared root")
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || filepath.Clean(path) == filepath.Clean(statePath) {
			return nil
		}
		if state.Cursor != "" && rel <= state.Cursor {
			return nil
		}
		if processed >= limit {
			return stop
		}
		info, infoErr := os.Lstat(path)
		if infoErr != nil {
			return infoErr
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			if err := validateOwnerOnlyInternalSymlink(root, path); err != nil {
				return err
			}
		case info.IsDir():
			if err := ensureOwnerOnlyDirectory(path, true); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			if err := ensureOwnerOnlyFile(path); err != nil {
				return err
			}
		default:
			return errors.New("owner-only store contains unsupported filesystem entry")
		}
		processed++
		lastCursor = rel
		return nil
	})
	if errors.Is(err, stop) {
		if persistErr := persistOwnerOnlyMigrationState(statePath, ownerOnlyMigrationState{Cursor: lastCursor}); persistErr != nil {
			return persistErr
		}
		return fmt.Errorf("%w: processed %d entries", errOwnerOnlyMigrationIncomplete, processed)
	}
	if err != nil {
		return err
	}
	return persistOwnerOnlyMigrationState(statePath, ownerOnlyMigrationState{Complete: true})
}
