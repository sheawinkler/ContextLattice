package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/contextlattice/gateway-go/internal/skillresources"
)

const skillBundleInspectUsage = "contextlattice_skills_index bundle-inspect --skill-file SKILL.md --bundle-file bundle.json [--expect-digest sha256:...] [--pretty]"

var (
	errSkillBundleArgs     = errors.New("skill bundle inspect: invalid arguments")
	errSkillBundleFile     = errors.New("skill bundle inspect: cannot read stable bounded regular file")
	errSkillBundleBlocked  = errors.New("skill bundle inspect: blocked content")
	errSkillBundleOutput   = errors.New("skill bundle inspect: output failed")
	errSkillBundlePlatform = errors.New("skill bundle inspect: native snapshot unavailable on this platform")
)

// Preflight the shared permissive parser: this offline command accepts no
// positional, duplicate, network, execution or approval options.
func parseSkillBundleInspectArgs(args []string) (parsedArgs, error) {
	stringsFlags := map[string]string{"skill-file": "skill_file", "bundle-file": "bundle_file", "expect-digest": "expect_digest"}
	if len(args) > 7 {
		return parsedArgs{}, errSkillBundleArgs
	}
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		name, value, equal := strings.Cut(args[i], "=")
		if seen[name] {
			return parsedArgs{}, errSkillBundleArgs
		}
		seen[name] = true
		if name == "--pretty" && !equal {
			continue
		}
		if !strings.HasPrefix(name, "--") || stringsFlags[strings.TrimPrefix(name, "--")] == "" {
			return parsedArgs{}, errSkillBundleArgs
		}
		if !equal {
			i++
			if i == len(args) {
				return parsedArgs{}, errSkillBundleArgs
			}
			value = args[i]
		}
		if value == "" || len(value) > 4096 || strings.HasPrefix(value, "-") || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return parsedArgs{}, errSkillBundleArgs
		}
	}
	parsed := parseArgs(args, stringsFlags, map[string]string{"pretty": "pretty"})
	if parsed.values["skill_file"] == "" || parsed.values["bundle_file"] == "" || len(parsed.pos) != 0 {
		return parsedArgs{}, errSkillBundleArgs
	}
	if value, present := parsed.values["expect_digest"]; present {
		if len(value) != 71 || !strings.HasPrefix(value, "sha256:") || strings.Trim(value[7:], "0123456789abcdef") != "" {
			return parsedArgs{}, errSkillBundleArgs
		}
	}
	return parsed, nil
}

type skillBundlePathIdentity struct {
	name string
	info os.FileInfo
}

type skillBundleSnapshot struct {
	file       *os.File
	identities []skillBundlePathIdentity
	limit      int
}

func openSkillBundleSnapshot(name string, limit int) (*skillBundleSnapshot, error) {
	// These are the native platforms whose nonblocking regular-file semantics
	// this adapter supports. Other targets retain the CLI but fail closed.
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return nil, errSkillBundlePlatform
	}
	if limit <= 0 || name == "" || len(name) > 4096 || strings.ContainsRune(name, 0) {
		return nil, errSkillBundleFile
	}
	abs, err := filepath.Abs(name)
	if err != nil {
		return nil, errSkillBundleFile
	}
	components := strings.Split(strings.TrimPrefix(abs, string(filepath.Separator)), string(filepath.Separator))
	snapshot := &skillBundleSnapshot{limit: limit}
	current := string(filepath.Separator)
	for i, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errSkillBundleFile
		}
		if i == len(components)-1 {
			if !info.Mode().IsRegular() || info.Size() > int64(limit) {
				return nil, errSkillBundleFile
			}
		} else if !info.IsDir() {
			return nil, errSkillBundleFile
		}
		snapshot.identities = append(snapshot.identities, skillBundlePathIdentity{current, info})
	}
	if err := snapshot.open(); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (s *skillBundleSnapshot) open() error {
	// OpenRoot is used only for the fixed filesystem root, never a user leaf
	// that could become a FIFO. Root.OpenFile uses descriptor-relative directory
	// traversal; NONBLOCK also prevents a leaf swapped to a FIFO blocking open.
	root, err := os.OpenRoot(string(filepath.Separator))
	if err != nil {
		return errSkillBundleFile
	}
	defer root.Close()
	name := s.identities[len(s.identities)-1].name
	s.file, err = root.OpenFile(strings.TrimPrefix(name, string(filepath.Separator)), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errSkillBundleFile
	}
	if err := s.verify(); err != nil {
		s.file.Close()
		return err
	}
	return nil
}

func sameSkillBundleFile(before, after os.FileInfo) bool {
	return os.SameFile(before, after) && before.Mode() == after.Mode() && before.Size() == after.Size() && before.ModTime().Equal(after.ModTime())
}

func (s *skillBundleSnapshot) verify() error {
	leaf := s.identities[len(s.identities)-1].info
	opened, err := s.file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !sameSkillBundleFile(leaf, opened) {
		return errSkillBundleFile
	}
	for i, identity := range s.identities {
		now, err := os.Lstat(identity.name)
		if err != nil || now.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity.info, now) || identity.info.Mode() != now.Mode() {
			return errSkillBundleFile
		}
		if i == len(s.identities)-1 && !sameSkillBundleFile(identity.info, now) {
			return errSkillBundleFile
		}
	}
	return nil
}

func (s *skillBundleSnapshot) read() ([]byte, error) {
	if err := s.verify(); err != nil {
		return nil, err
	}
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, errSkillBundleFile
	}
	raw, err := io.ReadAll(io.LimitReader(s.file, int64(s.limit)+1))
	if err != nil || len(raw) > s.limit || int64(len(raw)) != s.identities[len(s.identities)-1].info.Size() {
		return nil, errSkillBundleFile
	}
	if err := s.verify(); err != nil {
		return nil, err
	}
	return raw, nil
}

func (s *skillBundleSnapshot) confirm(raw []byte) error {
	again, err := s.read()
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, again) {
		return errSkillBundleFile
	}
	return nil
}

// Only fixed rule identifiers leave this scanner. Scan every admitted UTF-8
// byte sequence, including NUL-containing data and non-script extensions.
func reviewSkillBundle(bundle skillresources.Bundle, skill []byte) ([]string, error) {
	m := bundle.Manifest
	text := []string{m.SchemaID, m.SkillSHA256, m.Environment.OS, m.Environment.Architecture, m.Environment.FingerprintSHA256,
		m.Rollback.IdentitySHA256, m.Entrypoint.Path, m.Entrypoint.Interpreter.Source, m.Entrypoint.Interpreter.Name, m.Entrypoint.Interpreter.Version}
	for _, list := range [][]string{m.Applicability.Conditions, m.Applicability.Exclusions, m.Applicability.FailureModes, m.Rollback.Steps, m.Entrypoint.Arguments, m.Entrypoint.Interpreter.Arguments} {
		text = append(text, list...)
	}
	content := [][]byte{skill}
	for _, resource := range bundle.Resources {
		content = append(content, resource.Content)
		text = append(text, resource.Path, resource.SHA256)
	}
	for _, value := range text {
		content = append(content, []byte(value))
	}
	found := map[string]bool{}
	for _, raw := range content {
		for _, rule := range skillSourceBlockedRules {
			if rule.pattern.Match(raw) {
				return nil, errSkillBundleBlocked
			}
		}
		for _, rule := range skillSourceReviewRules {
			if rule.pattern.Match(raw) {
				found[rule.id] = true
			}
		}
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (c *cli) skillsIndexBundleInspect(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		if err := c.emitUsage(skillBundleInspectUsage); err != nil {
			return errSkillBundleOutput
		}
		return nil
	}
	parsed, err := parseSkillBundleInspectArgs(args)
	if err != nil {
		return err
	}
	skillFile, err := openSkillBundleSnapshot(parsed.values["skill_file"], skillresources.MaxSkillBytes)
	if err != nil {
		return err
	}
	defer skillFile.file.Close()
	bundleFile, err := openSkillBundleSnapshot(parsed.values["bundle_file"], skillresources.MaxJSONBytes)
	if err != nil {
		return err
	}
	defer bundleFile.file.Close()
	skill, err := skillFile.read()
	if err != nil {
		return err
	}
	wire, err := bundleFile.read()
	if err != nil {
		return err
	}
	bundle, err := skillresources.Decode(wire, skill)
	if err != nil {
		return err
	}
	digest, err := skillresources.Digest(bundle, skill)
	if err != nil {
		return err
	}
	if expected := parsed.values["expect_digest"]; expected != "" && expected != digest {
		return skillresources.ErrDigest
	}
	reviewIDs, err := reviewSkillBundle(bundle, skill)
	if err != nil {
		return err
	}
	if err := skillFile.confirm(skill); err != nil {
		return err
	}
	if err := bundleFile.confirm(wire); err != nil {
		return err
	}
	if err := skillFile.verify(); err != nil {
		return err
	}
	resourceBytes := 0
	resourceDigests := make([]string, 0, len(bundle.Resources))
	for _, resource := range bundle.Resources {
		resourceBytes += resource.SizeBytes
		resourceDigests = append(resourceDigests, resource.SHA256)
	}
	sort.Strings(resourceDigests)
	if err := c.emit(map[string]any{
		"schema_id": "contextlattice.skill_bundle_inspection.v1", "valid_manifest": true,
		"bundle_digest": digest, "skill_sha256": bundle.Manifest.SkillSHA256, "bundle_file_sha256": skillresources.SHA256(wire),
		"resource_digests": resourceDigests, "resource_count": len(bundle.Resources), "resource_bytes": resourceBytes,
		"skill_bytes": len(skill), "bundle_file_bytes": len(wire), "review_rule_ids": reviewIDs,
		"review_required": true, "active": false, "execution_authorized": false,
		"independent_verification": false, "runtime_environment_verified": false,
	}, parsed.bool("pretty")); err != nil {
		return errSkillBundleOutput
	}
	return nil
}
