package gatewaystate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const RootEnv = "CONTEXTLATTICE_GATEWAY_STATE_ROOT"

var legacyDefaultPrefixes = []string{
	filepath.Join(".data", "orchestrator"),
	filepath.Join("services", "orchestrator", "data"),
}

type RootResolution struct {
	Path        string `json:"path"`
	Source      string `json:"source"`
	SourceEnv   string `json:"source_env,omitempty"`
	StorageTier string `json:"storage_tier"`
	Warning     string `json:"warning,omitempty"`
}

type PathResolution struct {
	Path        string `json:"path"`
	Source      string `json:"source"`
	SourceEnv   string `json:"source_env,omitempty"`
	StorageTier string `json:"storage_tier"`
	Override    bool   `json:"override"`
}

type EntryInput struct {
	ID               string
	Path             string
	Source           string
	SourceEnv        string
	StorageTier      string
	Kind             string
	PersistenceClass string
}

func firstEnv(names ...string) (string, string) {
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value, name
		}
	}
	return "", ""
}

func absoluteClean(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("path is empty")
	}
	cleaned := filepath.Clean(raw)
	if filepath.IsAbs(cleaned) {
		return cleaned, nil
	}
	return filepath.Abs(cleaned)
}

func ResolveRoot() RootResolution {
	if raw, name := firstEnv(RootEnv); raw != "" {
		resolved := filepath.Clean(raw)
		warning := ""
		if !filepath.IsAbs(resolved) {
			warning = RootEnv + " must be an absolute path"
		}
		return RootResolution{Path: resolved, Source: "explicit_state_root", SourceEnv: name, StorageTier: "operator_configured", Warning: warning}
	}
	if raw, name := firstEnv("GO_MEMORY_STORE_ROOT", "MEMORY_BANK_ROOT"); raw != "" {
		resolved, err := absoluteClean(filepath.Join(raw, "_contextlattice"))
		warning := ""
		if err != nil {
			resolved = filepath.Clean(filepath.Join(raw, "_contextlattice"))
			warning = err.Error()
		}
		return RootResolution{Path: resolved, Source: "memory_store_compatibility", SourceEnv: name, StorageTier: "memory_store", Warning: warning}
	}
	if raw, name := firstEnv("CONTEXTLATTICE_GLOBAL_HOME"); raw != "" {
		resolved, err := absoluteClean(filepath.Join(raw, "state", "gateway"))
		warning := ""
		if err != nil {
			resolved = filepath.Clean(filepath.Join(raw, "state", "gateway"))
			warning = err.Error()
		}
		return RootResolution{Path: resolved, Source: "global_home", SourceEnv: name, StorageTier: "local_user", Warning: warning}
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return RootResolution{
			Path:        filepath.Clean(filepath.Join(home, ".contextlattice", "state", "gateway")),
			Source:      "user_home_default",
			StorageTier: "local_user",
		}
	}
	fallback := filepath.Join(os.TempDir(), "contextlattice", "state", "gateway")
	resolved, err := absoluteClean(fallback)
	warning := "user home unavailable; temporary fallback is not a durable production state tier"
	if err != nil {
		resolved = filepath.Clean(fallback)
		warning += ": " + err.Error()
	}
	return RootResolution{Path: resolved, Source: "temporary_fallback", StorageTier: "temporary", Warning: warning}
}

func DefaultRelative(fallback string) string {
	cleaned := filepath.Clean(strings.TrimSpace(fallback))
	if cleaned == "." || cleaned == "" {
		return ""
	}
	if filepath.IsAbs(cleaned) {
		return cleaned
	}
	for _, prefix := range legacyDefaultPrefixes {
		prefix = filepath.Clean(prefix)
		if cleaned == prefix {
			return ""
		}
		if rel, err := filepath.Rel(prefix, cleaned); err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return rel
		}
	}
	return cleaned
}

func ResolvePath(envNames []string, fallback string) PathResolution {
	if raw, name := firstEnv(envNames...); raw != "" {
		return PathResolution{
			Path:        filepath.Clean(raw),
			Source:      "surface_override",
			SourceEnv:   name,
			StorageTier: "explicit_override",
			Override:    true,
		}
	}
	root := ResolveRoot()
	relative := DefaultRelative(fallback)
	resolved := root.Path
	if relative != "" {
		if filepath.IsAbs(relative) {
			resolved = filepath.Clean(relative)
		} else {
			resolved = filepath.Clean(filepath.Join(root.Path, relative))
		}
	}
	return PathResolution{
		Path:        resolved,
		Source:      "state_root",
		SourceEnv:   root.SourceEnv,
		StorageTier: root.StorageTier,
		Override:    false,
	}
}

func EnsureRoot() (RootResolution, error) {
	root := ResolveRoot()
	if strings.TrimSpace(root.Path) == "" {
		return root, errors.New("gateway state root is empty")
	}
	if !filepath.IsAbs(root.Path) {
		return root, fmt.Errorf("gateway state root is not absolute: %s", root.Path)
	}
	if err := os.MkdirAll(root.Path, 0o700); err != nil {
		return root, fmt.Errorf("create gateway state root: %w", err)
	}
	info, err := os.Stat(root.Path)
	if err != nil {
		return root, fmt.Errorf("stat gateway state root: %w", err)
	}
	if !info.IsDir() {
		return root, fmt.Errorf("gateway state root is not a directory: %s", root.Path)
	}
	return root, nil
}

func failureClass(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, os.ErrPermission) {
		return "permission_denied"
	}
	return "path_error"
}

func nearestExistingParent(path string) (string, os.FileInfo, error) {
	candidate := filepath.Clean(path)
	for {
		info, err := os.Stat(candidate)
		if err == nil {
			return candidate, info, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return candidate, nil, err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return candidate, nil, err
		}
		candidate = parent
	}
}

func inspectPath(rawPath string, kind string) map[string]any {
	absPath, err := absoluteClean(rawPath)
	if err != nil {
		return map[string]any{
			"path": rawPath, "exists": false, "writable": false, "traversable": false,
			"error": err.Error(), "failure_class": failureClass(err), "check": "non_mutating",
		}
	}
	result := map[string]any{
		"path": absPath, "exists": false, "writable": false, "traversable": false,
		"error": "", "failure_class": "", "check": "non_mutating",
	}
	info, statErr := os.Stat(absPath)
	if statErr == nil {
		result["exists"] = true
		result["actual_kind"] = "file"
		if info.IsDir() {
			result["actual_kind"] = "directory"
		}
		opened, openErr := os.Open(absPath)
		if openErr == nil {
			result["traversable"] = true
			_ = opened.Close()
		} else {
			result["error"] = openErr.Error()
			result["failure_class"] = failureClass(openErr)
		}
		if info.Mode().IsRegular() {
			writer, writeErr := os.OpenFile(absPath, os.O_WRONLY, 0)
			if writeErr == nil {
				result["writable"] = true
				_ = writer.Close()
			} else if result["error"] == "" {
				result["error"] = writeErr.Error()
				result["failure_class"] = failureClass(writeErr)
			}
		} else if info.IsDir() {
			writable, traversable, writeErr, traverseErr := probeDirectoryAccess(absPath)
			result["writable"] = writable
			result["traversable"] = traversable
			if traverseErr != nil {
				result["error"] = traverseErr.Error()
				result["failure_class"] = failureClass(traverseErr)
			} else if writeErr != nil {
				result["error"] = writeErr.Error()
				result["failure_class"] = failureClass(writeErr)
			}
		}
		if kind == "directory" && !info.IsDir() && result["error"] == "" {
			result["error"] = "resolved directory path is an existing non-directory"
			result["failure_class"] = "path_error"
		}
		if kind == "file" && info.IsDir() && result["error"] == "" {
			result["error"] = "resolved file path is an existing directory"
			result["failure_class"] = "path_error"
		}
		return result
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		result["error"] = statErr.Error()
		result["failure_class"] = failureClass(statErr)
		return result
	}
	parent, parentInfo, parentErr := nearestExistingParent(filepath.Dir(absPath))
	result["nearest_existing_parent"] = parent
	if parentErr != nil {
		result["error"] = parentErr.Error()
		result["failure_class"] = failureClass(parentErr)
		return result
	}
	if parentInfo != nil && parentInfo.IsDir() {
		writable, traversable, writeErr, traverseErr := probeDirectoryAccess(parent)
		result["writable"] = writable
		result["traversable"] = traversable
		if traverseErr != nil {
			result["error"] = traverseErr.Error()
			result["failure_class"] = failureClass(traverseErr)
		} else if writeErr != nil {
			result["error"] = writeErr.Error()
			result["failure_class"] = failureClass(writeErr)
		}
	}
	return result
}

func Inventory(entries []EntryInput) map[string]any {
	root := ResolveRoot()
	rootProbe := inspectPath(root.Path, "directory")
	rows := make([]map[string]any, 0, len(entries))
	allResolved := filepath.IsAbs(root.Path)
	unhealthyEntries := 0
	for _, entry := range entries {
		probe := inspectPath(entry.Path, entry.Kind)
		resolvedPath, _ := probe["path"].(string)
		if !filepath.IsAbs(resolvedPath) {
			allResolved = false
		}
		writable, _ := probe["writable"].(bool)
		traversable, _ := probe["traversable"].(bool)
		probeError, _ := probe["error"].(string)
		if !filepath.IsAbs(resolvedPath) || !writable || !traversable || probeError != "" {
			unhealthyEntries++
		}
		row := map[string]any{
			"id": entry.ID, "path": probe["path"], "kind": entry.Kind,
			"persistence_class": entry.PersistenceClass, "storage_tier": entry.StorageTier,
			"override_source": entry.Source, "override_env": entry.SourceEnv,
			"exists": probe["exists"], "writable": probe["writable"], "traversable": probe["traversable"],
			"check": probe["check"], "error": probe["error"], "failure_class": probe["failure_class"],
		}
		if parent := probe["nearest_existing_parent"]; parent != nil {
			row["nearest_existing_parent"] = parent
		}
		rows = append(rows, row)
	}
	rootWritable, _ := rootProbe["writable"].(bool)
	rootTraversable, _ := rootProbe["traversable"].(bool)
	return map[string]any{
		"ok":          allResolved && rootWritable && rootTraversable && root.Source != "temporary_fallback" && unhealthyEntries == 0,
		"schema_id":   "contextlattice_gateway_state_inventory.v1",
		"captured_at": time.Now().UTC().Format(time.RFC3339Nano),
		"root": map[string]any{
			"path": rootProbe["path"], "source": root.Source, "source_env": root.SourceEnv,
			"storage_tier": root.StorageTier, "warning": root.Warning, "exists": rootProbe["exists"],
			"writable": rootProbe["writable"], "traversable": rootProbe["traversable"],
			"error": rootProbe["error"], "failure_class": rootProbe["failure_class"],
		},
		"entry_count":       len(rows),
		"unhealthy_entries": unhealthyEntries,
		"entries":           rows,
		"permission_policy": "permission_denied is reported only for an exact OS permission error; missing or mismatched paths remain path/configuration evidence",
	}
}
