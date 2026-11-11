package skillresources_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"

	sr "github.com/contextlattice/gateway-go/internal/skillresources"
)

var skill = []byte("---\nname: bounded-check\n---\nReview before use.\r\n")

func resource(name string, content []byte) sr.Resource {
	return sr.Resource{Path: name, SizeBytes: len(content), SHA256: sr.SHA256(content), Content: content}
}

func fixture() sr.Bundle {
	return sr.Bundle{
		Manifest: sr.Manifest{
			SchemaID: sr.SchemaID, Version: sr.Version, SkillSHA256: sr.SHA256(skill),
			Applicability: sr.Applicability{
				Conditions: []string{"safe condition", "operator reviewed"},
				Exclusions: []string{}, FailureModes: []string{"unsupported runtime"},
			},
			Environment: sr.Environment{OS: "linux", Architecture: "amd64", FingerprintSHA256: sr.SHA256([]byte("fixture environment"))},
			Rollback:    sr.Rollback{IdentitySHA256: sr.SHA256([]byte("fixture rollback")), Steps: []string{"discard generated report"}},
			Entrypoint: sr.Entrypoint{
				Path: "scripts/check.mjs", Arguments: []string{},
				Interpreter: sr.Interpreter{Source: "installed", Name: "node", Version: "24.22.2", Arguments: []string{}},
			},
		},
		Resources: []sr.Resource{
			resource("scripts/check.mjs", []byte("console.log('fixture');\n")),
			resource("references/notes.txt", []byte("reviewed fixture\r\n")),
		},
	}
}

func wire(t *testing.T, bundle sr.Bundle) []byte {
	t.Helper()
	raw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func digest(t *testing.T, bundle sr.Bundle, markdown []byte) string {
	t.Helper()
	value, err := sr.Digest(bundle, markdown)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestRoundTripExactBytesAndNoMutation(t *testing.T) {
	bundle := fixture()
	content := []byte("\xef\xbb\xbf leading\t\r\n\nnull:\x00; unicode:\xc3\xa9; literal:\\uD800\n")
	bundle.Resources[0] = resource(bundle.Resources[0].Path, content)
	before := wire(t, bundle)
	canonical, err := sr.CanonicalJSON(bundle, skill)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := sr.Decode(canonical, skill)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded.Resources[1].Content, content) {
		t.Fatal("resource bytes changed")
	}
	if !bytes.Equal(wire(t, bundle), before) {
		t.Fatal("canonicalization mutated caller memory")
	}
	again, err := sr.CanonicalJSON(decoded, skill)
	if err != nil || !bytes.Equal(again, canonical) || digest(t, decoded, skill) != sr.SHA256(canonical) {
		t.Fatal("canonicalization is not idempotent or digest does not bind wire bytes")
	}
}

func TestInvalidPaths(t *testing.T) {
	paths := []string{
		"", "/scripts/check.mjs", "C:/scripts/check.mjs", "//host/scripts/a", "scripts\\a", "scripts/a\\b",
		"scripts/../a", "scripts/./a", "scripts//a", "scripts/", "scripts/a/", "scripts/a.", "scripts/.hidden",
		"scripts/a b", "scripts/a\tb", "scripts/a\nb", "scripts/a\x00b", "scripts/a\x7fb", "scripts/a\u0085b",
		"scripts/a\u202eb", "scripts/\u00e9", "scripts/%2e%2e/a", "scripts/a:b", "scripts/a?b", "scripts/a*b",
		"Scripts/a", "other/a", "SKILL.md", "scripts/SKILL.md", "references/skill.MD", "scripts/.contextlattice-source.json",
		"scripts/con", "scripts/CON.txt", "scripts/prn", "scripts/AUX", "scripts/nul.json", "scripts/com1.js", "references/Lpt9.txt",
		"scripts/" + strings.Repeat("a", 65), "scripts/" + strings.Repeat("a/", 120) + "x",
	}
	for i, name := range paths {
		t.Run(fmt.Sprintf("case_%02d", i), func(t *testing.T) {
			bundle := fixture()
			bundle.Resources[1].Path = name
			if err := sr.Validate(bundle, skill); !errors.Is(err, sr.ErrPath) {
				t.Fatalf("wanted path rejection, got %v", err)
			}
		})
	}
	for _, name := range []string{"scripts/check-v1.2_test.mjs", "references/a/b/C.txt", "references/com10.txt"} {
		bundle := fixture()
		bundle.Resources[1].Path = name
		if err := sr.Validate(bundle, skill); err != nil {
			t.Fatalf("valid canonical path rejected: %v", err)
		}
	}
}

func TestPathCollisions(t *testing.T) {
	cases := [][]string{
		{"scripts/check.mjs", "scripts/check.mjs"},
		{"scripts/check.mjs", "scripts/CHECK.mjs"},
		{"scripts/check.mjs", "scripts/check.mjs/child"},
		{"scripts/check.mjs", "scripts/a", "scripts/a-b", "scripts/a/b"},
		{"scripts/check.mjs", "references/Dir/a", "references/dir/b"},
	}
	for i, names := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			bundle := fixture()
			bundle.Resources = nil
			for _, name := range names {
				bundle.Resources = append(bundle.Resources, resource(name, []byte("x")))
			}
			if err := sr.Validate(bundle, skill); !errors.Is(err, sr.ErrPath) {
				t.Fatalf("collision admitted: %v", err)
			}
		})
	}
}

func TestValidationAdversaries(t *testing.T) {
	cases := []struct {
		name string
		edit func(*sr.Bundle)
	}{
		{"schema", func(b *sr.Bundle) { b.Manifest.SchemaID = "other" }},
		{"version", func(b *sr.Bundle) { b.Manifest.Version++ }},
		{"skill digest", func(b *sr.Bundle) { b.Manifest.SkillSHA256 = sr.SHA256([]byte("other")) }},
		{"uppercase digest", func(b *sr.Bundle) { b.Resources[0].SHA256 = strings.ToUpper(b.Resources[0].SHA256) }},
		{"missing digest", func(b *sr.Bundle) { b.Resources[0].SHA256 = "" }},
		{"stale bytes", func(b *sr.Bundle) { b.Resources[0].Content[0] ^= 1 }},
		{"size mismatch", func(b *sr.Bundle) { b.Resources[0].SizeBytes++ }},
		{"negative size", func(b *sr.Bundle) { b.Resources[0].SizeBytes = -1 }},
		{"nil content", func(b *sr.Bundle) { b.Resources[0] = resource(b.Resources[0].Path, nil) }},
		{"invalid utf8", func(b *sr.Bundle) { b.Resources[0] = resource(b.Resources[0].Path, []byte{0xff}) }},
		{"nil conditions", func(b *sr.Bundle) { b.Manifest.Applicability.Conditions = nil }},
		{"empty conditions", func(b *sr.Bundle) { b.Manifest.Applicability.Conditions = []string{} }},
		{"blank condition", func(b *sr.Bundle) { b.Manifest.Applicability.Conditions = []string{" "} }},
		{"null empty list", func(b *sr.Bundle) { b.Manifest.Applicability.Exclusions = nil }},
		{"metadata count", func(b *sr.Bundle) { b.Manifest.Rollback.Steps = strings.Split(strings.Repeat("x,", 8)+"x", ",") }},
		{"metadata bytes", func(b *sr.Bundle) { b.Manifest.Rollback.Steps = []string{strings.Repeat("x", sr.MaxMetadataBytes+1)} }},
		{"metadata control", func(b *sr.Bundle) { b.Manifest.Rollback.Steps = []string{"x\ny"} }},
		{"metadata bidi", func(b *sr.Bundle) { b.Manifest.Rollback.Steps = []string{"x\u202ey"} }},
		{"metadata utf8", func(b *sr.Bundle) { b.Manifest.Rollback.Steps = []string{string([]byte{0xff})} }},
		{"environment", func(b *sr.Bundle) { b.Manifest.Environment.OS = " linux" }},
		{"fingerprint", func(b *sr.Bundle) { b.Manifest.Environment.FingerprintSHA256 = "unbound" }},
		{"rollback", func(b *sr.Bundle) { b.Manifest.Rollback.IdentitySHA256 = "unbound" }},
		{"missing entrypoint", func(b *sr.Bundle) { b.Manifest.Entrypoint.Path = "scripts/missing.mjs" }},
		{"reference entrypoint", func(b *sr.Bundle) { b.Manifest.Entrypoint.Path = "references/notes.txt" }},
		{"entrypoint case", func(b *sr.Bundle) { b.Manifest.Entrypoint.Path = "scripts/CHECK.mjs" }},
		{"download runtime", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Source = "download" }},
		{"runtime path", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Name = "/usr/bin/node" }},
		{"runtime shell", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Name = "node;true" }},
		{"runtime alias", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Version = "latest" }},
		{"runtime range", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Version = "24.*" }},
		{"runtime comparator", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Version = ">=24" }},
		{"null arguments", func(b *sr.Bundle) { b.Manifest.Entrypoint.Arguments = nil }},
		{"argument control", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Arguments = []string{"\x00"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := fixture()
			tc.edit(&bundle)
			if err := sr.Validate(bundle, skill); err == nil {
				t.Fatal("invalid bundle admitted")
			}
			if _, err := sr.CanonicalJSON(bundle, skill); err == nil {
				t.Fatal("invalid bundle encoded")
			}
			if value, err := sr.Digest(bundle, skill); err == nil || value != "" {
				t.Fatal("invalid bundle received an identity")
			}
		})
	}
}

func TestLimits(t *testing.T) {
	bundle := fixture()
	bundle.Resources = nil
	for i := 0; i < sr.MaxResources; i++ {
		bundle.Resources = append(bundle.Resources, resource(fmt.Sprintf("scripts/check%d.mjs", i), bytes.Repeat([]byte("x"), sr.MaxResourceBytes)))
	}
	bundle.Manifest.Entrypoint.Path = bundle.Resources[0].Path
	if err := sr.Validate(bundle, skill); err != nil {
		t.Fatal("exact maximum resources rejected:", err)
	}
	if _, err := sr.Decode(wire(t, bundle), skill); err != nil {
		t.Fatal("exact maximum wire rejected:", err)
	}
	bundle.Resources[0] = resource(bundle.Resources[0].Path, bytes.Repeat([]byte("x"), sr.MaxResourceBytes+1))
	if err := sr.Validate(bundle, skill); !errors.Is(err, sr.ErrLimit) {
		t.Fatal("oversized resource admitted")
	}
	bundle = fixture()
	for len(bundle.Resources) <= sr.MaxResources {
		bundle.Resources = append(bundle.Resources, resource(fmt.Sprintf("references/n%d", len(bundle.Resources)), []byte{}))
	}
	if err := sr.Validate(bundle, skill); !errors.Is(err, sr.ErrLimit) {
		t.Fatal("excess resource count admitted")
	}
	bundle.Resources = []sr.Resource{}
	if err := sr.Validate(bundle, skill); !errors.Is(err, sr.ErrLimit) {
		t.Fatal("empty resource set admitted")
	}
	bundle = fixture()
	for _, size := range []int{sr.MaxSkillBytes, sr.MaxSkillBytes + 1, 0} {
		markdown := bytes.Repeat([]byte("x"), size)
		bundle.Manifest.SkillSHA256 = sr.SHA256(markdown)
		err := sr.Validate(bundle, markdown)
		if (size == sr.MaxSkillBytes) != (err == nil) {
			t.Fatal("incorrect Markdown boundary:", size, err)
		}
	}
	bundle.Manifest.SkillSHA256 = sr.SHA256([]byte{0xff})
	if err := sr.Validate(bundle, []byte{0xff}); !errors.Is(err, sr.ErrContent) {
		t.Fatal("invalid Markdown UTF-8 admitted")
	}
	raw := wire(t, fixture())
	atLimit := append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), sr.MaxJSONBytes-len(raw))...)
	if _, err := sr.Decode(atLimit, skill); err != nil {
		t.Fatal("exact JSON byte limit rejected:", err)
	}
	if _, err := sr.Decode(append(atLimit, ' '), skill); !errors.Is(err, sr.ErrLimit) {
		t.Fatal("oversized JSON admitted")
	}
}

func TestAllBehaviorFieldsBindIdentity(t *testing.T) {
	baseline := digest(t, fixture(), skill)
	cases := []struct {
		name string
		edit func(*sr.Bundle)
	}{
		{"condition", func(b *sr.Bundle) { b.Manifest.Applicability.Conditions[0] += "!" }},
		{"condition order", func(b *sr.Bundle) {
			b.Manifest.Applicability.Conditions[0], b.Manifest.Applicability.Conditions[1] = b.Manifest.Applicability.Conditions[1], b.Manifest.Applicability.Conditions[0]
		}},
		{"exclusion", func(b *sr.Bundle) { b.Manifest.Applicability.Exclusions = []string{"production"} }},
		{"failure mode", func(b *sr.Bundle) { b.Manifest.Applicability.FailureModes[0] += "!" }},
		{"os", func(b *sr.Bundle) { b.Manifest.Environment.OS = "darwin" }},
		{"architecture", func(b *sr.Bundle) { b.Manifest.Environment.Architecture = "arm64" }},
		{"environment identity", func(b *sr.Bundle) { b.Manifest.Environment.FingerprintSHA256 = sr.SHA256([]byte("changed")) }},
		{"rollback identity", func(b *sr.Bundle) { b.Manifest.Rollback.IdentitySHA256 = sr.SHA256([]byte("changed")) }},
		{"rollback steps", func(b *sr.Bundle) { b.Manifest.Rollback.Steps[0] += "!" }},
		{"entrypoint", func(b *sr.Bundle) {
			b.Resources[1].Path = "scripts/other.mjs"
			b.Manifest.Entrypoint.Path = b.Resources[1].Path
		}},
		{"runtime name", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Name = "nodejs" }},
		{"runtime version", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Version = "24.22.3" }},
		{"runtime args", func(b *sr.Bundle) { b.Manifest.Entrypoint.Interpreter.Arguments = []string{"--no-warnings"} }},
		{"script args", func(b *sr.Bundle) { b.Manifest.Entrypoint.Arguments = []string{"", "--check"} }},
		{"path", func(b *sr.Bundle) { b.Resources[1].Path = "references/renamed.txt" }},
		{"one byte", func(b *sr.Bundle) {
			b.Resources[1].Content[0] ^= 1
			b.Resources[1].SHA256 = sr.SHA256(b.Resources[1].Content)
		}},
		{"line endings", func(b *sr.Bundle) {
			b.Resources[1] = resource(b.Resources[1].Path, bytes.ReplaceAll(b.Resources[1].Content, []byte("\r\n"), []byte("\n")))
		}},
		{"removed reference", func(b *sr.Bundle) { b.Resources = b.Resources[:1] }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := fixture()
			tc.edit(&bundle)
			if digest(t, bundle, skill) == baseline {
				t.Fatal("behavior change did not change identity")
			}
		})
	}
	bundle := fixture()
	changedSkill := append(append([]byte{}, skill...), '\n')
	bundle.Manifest.SkillSHA256 = sr.SHA256(changedSkill)
	if digest(t, bundle, changedSkill) == baseline {
		t.Fatal("Markdown change did not change identity")
	}
	if _, err := sr.Digest(bundle, skill); !errors.Is(err, sr.ErrDigest) {
		t.Fatal("Markdown digest not checked against supplied bytes")
	}
}

func TestResourcePermutationMetamorphic(t *testing.T) {
	rng := rand.New(rand.NewSource(905))
	for trial := 0; trial < 64; trial++ {
		bundle := fixture()
		for i := 0; i < 2; i++ {
			bundle.Resources = append(bundle.Resources, resource(fmt.Sprintf("references/extra%d.txt", i), []byte(fmt.Sprintf("seed:%d:%d\r\n", trial, rng.Int63()))))
		}
		want := digest(t, bundle, skill)
		for iteration := 0; iteration < 12; iteration++ {
			rng.Shuffle(len(bundle.Resources), func(i, j int) { bundle.Resources[i], bundle.Resources[j] = bundle.Resources[j], bundle.Resources[i] })
			before := wire(t, bundle)
			if digest(t, bundle, skill) != want || !bytes.Equal(wire(t, bundle), before) {
				t.Fatal("resource permutation changed identity or input")
			}
			pretty, err := json.MarshalIndent(bundle, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := sr.Decode(pretty, skill)
			if err != nil || digest(t, decoded, skill) != want {
				t.Fatal("wire whitespace changed identity", err)
			}
		}
	}
}

func TestConcurrentReadOnlyUse(t *testing.T) {
	bundle := fixture()
	want := digest(t, bundle, skill)
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for j := 0; j < 10; j++ {
				got, err := sr.Digest(bundle, skill)
				if err != nil || got != want {
					t.Error("concurrent read changed digest", err)
				}
			}
		}()
	}
	workers.Wait()
}

func TestDecodeFailureReturnsZeroValue(t *testing.T) {
	for _, raw := range [][]byte{[]byte("null"), []byte("{}"), []byte("[]"), append(wire(t, fixture()), '\n', '{', '}')} {
		bundle, err := sr.Decode(raw, skill)
		if err == nil || !reflect.DeepEqual(bundle, sr.Bundle{}) {
			t.Fatal("failure exposed a partial bundle")
		}
	}
}
