// Package skillresources validates and hashes inactive skill resource bundles.
// It performs no execution, filesystem access, network access, or activation.
package skillresources

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	SchemaID              = "skill_resource_bundle.v1"
	Version               = 1
	MaxResources          = 4
	MaxResourceBytes      = 8 * 1024
	MaxTotalResourceBytes = 32 * 1024
	MaxSkillBytes         = 128 * 1024
	MaxJSONBytes          = 128 * 1024
	MaxJSONDepth          = 8
	MaxPathBytes          = 240
	MaxMetadataItems      = 8
	MaxMetadataBytes      = 256
)

// Error values are deliberately content-free, including for malformed JSON.
var (
	ErrJSON       = errors.New("skillresources: invalid JSON contract")
	ErrLimit      = errors.New("skillresources: size, count, or depth limit exceeded")
	ErrManifest   = errors.New("skillresources: invalid manifest")
	ErrPath       = errors.New("skillresources: invalid or colliding resource path")
	ErrContent    = errors.New("skillresources: invalid resource content")
	ErrDigest     = errors.New("skillresources: digest mismatch or invalid digest")
	ErrEntrypoint = errors.New("skillresources: invalid entrypoint or interpreter")
)

// Bundle carries exact resource bytes; JSON encodes Content as canonical base64.
// SKILL.md bytes are supplied separately to every validating API.
type Bundle struct {
	Manifest  Manifest   `json:"manifest"`
	Resources []Resource `json:"resources"`
}

type Manifest struct {
	SchemaID      string        `json:"schema_id"`
	Version       int           `json:"version"`
	SkillSHA256   string        `json:"skill_sha256"`
	Applicability Applicability `json:"applicability"`
	Environment   Environment   `json:"environment"`
	Rollback      Rollback      `json:"rollback"`
	Entrypoint    Entrypoint    `json:"entrypoint"`
}

// All lists are ordered, required and non-null, even when empty is permitted.
type Applicability struct {
	Conditions   []string `json:"conditions"`
	Exclusions   []string `json:"exclusions"`
	FailureModes []string `json:"failure_modes"`
}

// FingerprintSHA256 is an identity claim, not proof of the current host state.
type Environment struct {
	OS                string `json:"os"`
	Architecture      string `json:"architecture"`
	FingerprintSHA256 string `json:"fingerprint_sha256"`
}

// IdentitySHA256 identifies a separately reviewed rollback artifact. The codec
// does not retrieve that artifact or certify that the steps reverse side effects.
type Rollback struct {
	IdentitySHA256 string   `json:"identity_sha256"`
	Steps          []string `json:"steps"`
}

type Entrypoint struct {
	Path        string      `json:"path"`
	Interpreter Interpreter `json:"interpreter"`
	Arguments   []string    `json:"arguments"`
}

// Source must be "installed". Name and Version identify an operator-selected
// runtime; this package neither resolves it nor authorizes any argument.
type Interpreter struct {
	Source    string   `json:"source"`
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Arguments []string `json:"arguments"`
}

type Resource struct {
	Path      string `json:"path"`
	SizeBytes int    `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	Content   []byte `json:"content"`
}

// SHA256 returns the canonical lowercase, prefixed digest of exact bytes.
func SHA256(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// Validate verifies a bundle against the exact SKILL.md bytes. It does not
// normalize or mutate input. Callers must not concurrently mutate its slices.
func Validate(bundle Bundle, skillMarkdown []byte) error {
	manifest := bundle.Manifest
	if manifest.SchemaID != SchemaID || manifest.Version != Version {
		return ErrManifest
	}
	if len(skillMarkdown) == 0 || len(skillMarkdown) > MaxSkillBytes {
		return ErrLimit
	}
	if !utf8.Valid(skillMarkdown) {
		return ErrContent
	}
	if !validDigest(manifest.SkillSHA256) || SHA256(skillMarkdown) != manifest.SkillSHA256 {
		return ErrDigest
	}
	if !validList(manifest.Applicability.Conditions, false, false) ||
		!validList(manifest.Applicability.Exclusions, true, false) ||
		!validList(manifest.Applicability.FailureModes, true, false) ||
		!validList(manifest.Rollback.Steps, false, false) {
		return ErrManifest
	}
	if !validName(manifest.Environment.OS) || !validName(manifest.Environment.Architecture) {
		return ErrManifest
	}
	if !validDigest(manifest.Environment.FingerprintSHA256) || !validDigest(manifest.Rollback.IdentitySHA256) {
		return ErrDigest
	}
	entry := manifest.Entrypoint
	interpreter := entry.Interpreter
	if !validPath(entry.Path) || !strings.HasPrefix(entry.Path, "scripts/") ||
		interpreter.Source != "installed" || !validName(interpreter.Name) ||
		!validRuntimeVersion(interpreter.Version) ||
		!validList(interpreter.Arguments, true, true) || !validList(entry.Arguments, true, true) {
		return ErrEntrypoint
	}
	if len(bundle.Resources) == 0 || len(bundle.Resources) > MaxResources {
		return ErrLimit
	}
	paths := make([]string, 0, len(bundle.Resources))
	components := make(map[string]string)
	total, foundEntrypoint := 0, false
	for _, resource := range bundle.Resources {
		if !validPath(resource.Path) {
			return ErrPath
		}
		paths = append(paths, strings.ToLower(resource.Path))
		parts := strings.Split(resource.Path, "/")
		for i := range parts {
			prefix := strings.Join(parts[:i+1], "/")
			folded := strings.ToLower(prefix)
			if previous, exists := components[folded]; exists && previous != prefix {
				return ErrPath
			}
			components[folded] = prefix
		}
		if resource.Content == nil || len(resource.Content) > MaxResourceBytes {
			return ErrLimit
		}
		total += len(resource.Content)
		if total > MaxTotalResourceBytes {
			return ErrLimit
		}
		if resource.SizeBytes != len(resource.Content) || !utf8.Valid(resource.Content) {
			return ErrContent
		}
		if !validDigest(resource.SHA256) || SHA256(resource.Content) != resource.SHA256 {
			return ErrDigest
		}
		if resource.Path == entry.Path {
			foundEntrypoint = true
		}
	}
	for i := range paths {
		for j := 0; j < i; j++ {
			if paths[i] == paths[j] || strings.HasPrefix(paths[i], paths[j]+"/") || strings.HasPrefix(paths[j], paths[i]+"/") {
				return ErrPath
			}
		}
	}
	if !foundEntrypoint {
		return ErrEntrypoint
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, c := range value[7:] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validName(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validRuntimeVersion(value string) bool {
	if len(value) == 0 || len(value) > 64 || value[0] < '0' || value[0] > '9' {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '+') {
			return false
		}
	}
	return true
}

func validList(values []string, allowEmpty, allowEmptyItem bool) bool {
	if values == nil || len(values) > MaxMetadataItems || !allowEmpty && len(values) == 0 {
		return false
	}
	for _, value := range values {
		if len(value) > MaxMetadataBytes || !utf8.ValidString(value) || !allowEmptyItem && strings.TrimSpace(value) == "" {
			return false
		}
		for _, c := range value {
			if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
				return false
			}
		}
	}
	return true
}

func validPath(value string) bool {
	if len(value) > MaxPathBytes || !(strings.HasPrefix(value, "scripts/") || strings.HasPrefix(value, "references/")) {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if len(segment) == 0 || len(segment) > 64 || segment == "." || segment == ".." || strings.HasSuffix(segment, ".") {
			return false
		}
		for i, c := range segment {
			alphanumeric := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
			if !alphanumeric && (i == 0 || c != '.' && c != '_' && c != '-') {
				return false
			}
		}
		base := strings.ToLower(strings.SplitN(segment, ".", 2)[0])
		if base == "con" || base == "prn" || base == "aux" || base == "nul" ||
			len(base) == 4 && (strings.HasPrefix(base, "com") || strings.HasPrefix(base, "lpt")) && base[3] >= '1' && base[3] <= '9' {
			return false
		}
		if strings.EqualFold(segment, "SKILL.md") {
			return false
		}
	}
	return true
}
