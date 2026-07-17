package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextLatticeBuildIdentityBindsSourceWithoutTimestampDrift(t *testing.T) {
	originalVersion, originalChannel := contextLatticeBuildVersion, contextLatticeBuildChannel
	originalCommit, originalTree := contextLatticeSourceCommit, contextLatticeSourceTree
	originalNonce := contextLatticeProcessBootNonce
	t.Cleanup(func() {
		contextLatticeBuildVersion, contextLatticeBuildChannel = originalVersion, originalChannel
		contextLatticeSourceCommit, contextLatticeSourceTree = originalCommit, originalTree
		contextLatticeProcessBootNonce = originalNonce
	})
	contextLatticeBuildVersion = "3.18.0"
	contextLatticeBuildChannel = "release"
	contextLatticeSourceCommit = "0123456789012345678901234567890123456789"
	contextLatticeSourceTree = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	contextLatticeProcessBootNonce = "0123456789abcdef0123456789abcdef"

	identity := contextLatticeBuildIdentity()
	if anyToString(identity["schema_id"]) != "contextlattice_build_identity.v1" ||
		anyToString(identity["version"]) != "3.18.0" ||
		anyToString(identity["channel"]) != "release" ||
		anyToString(identity["source_commit"]) != contextLatticeSourceCommit ||
		anyToString(identity["source_tree"]) != contextLatticeSourceTree ||
		anyToString(identity["boot_nonce"]) != contextLatticeProcessBootNonce ||
		!anyToBool(identity["source_bound"]) {
		t.Fatalf("unexpected build identity: %#v", identity)
	}
	if _, present := identity["built_at"]; present {
		t.Fatalf("build identity contains nondeterministic timestamp: %#v", identity)
	}
}

func TestContextLatticeBootNonceGenerationIsDeterministicForInjectedEntropy(t *testing.T) {
	nonce, err := newContextLatticeBootNonce(bytes.NewReader(bytes.Repeat([]byte{0xab}, contextLatticeBootNonceBytes)))
	if err != nil {
		t.Fatalf("generate boot nonce: %v", err)
	}
	if expected := strings.Repeat("ab", contextLatticeBootNonceBytes); nonce != expected {
		t.Fatalf("boot nonce=%q want=%q", nonce, expected)
	}
	if _, err := newContextLatticeBootNonce(strings.NewReader("short")); err == nil {
		t.Fatal("short entropy source unexpectedly generated a boot nonce")
	}
}

func TestContextLatticeProcessBootNonceIsStableAndValid(t *testing.T) {
	first := anyToString(contextLatticeBuildIdentity()["boot_nonce"])
	second := anyToString(contextLatticeBuildIdentity()["boot_nonce"])
	if first == "" || first != second {
		t.Fatalf("process boot nonce is not stable: first=%q second=%q", first, second)
	}
	decoded, err := hex.DecodeString(first)
	if err != nil || len(decoded) != contextLatticeBootNonceBytes {
		t.Fatalf("process boot nonce is not %d bytes of lowercase hex: nonce=%q err=%v", contextLatticeBootNonceBytes, first, err)
	}
	if first != strings.ToLower(first) {
		t.Fatalf("process boot nonce is not lowercase: %q", first)
	}
}

func TestContextLatticeAuditPayloadsReuseHealthBuildIdentity(t *testing.T) {
	expected := contextLatticeBuildIdentity()
	healthRecorder := httptest.NewRecorder()
	healthRequest := httptest.NewRequest("GET", "/health", nil)
	newTestServer(t, "http://127.0.0.1:9").health(healthRecorder, healthRequest)
	var healthPayload map[string]any
	if err := json.Unmarshal(healthRecorder.Body.Bytes(), &healthPayload); err != nil {
		t.Fatalf("decode health payload: %v", err)
	}
	healthzRecorder := httptest.NewRecorder()
	healthzRequest := httptest.NewRequest("GET", "/healthz", nil)
	(&server{
		strictNoPythonRuntime: true,
		memoryStore: &memoryStore{
			migration: newOwnerOnlyMigrationRuntime("", false),
		},
	}).healthz(healthzRecorder, healthzRequest)
	var healthzPayload map[string]any
	if err := json.Unmarshal(healthzRecorder.Body.Bytes(), &healthzPayload); err != nil {
		t.Fatalf("decode healthz payload: %v", err)
	}
	payloads := map[string]map[string]any{
		"health":           anyMap(healthPayload["build"]),
		"healthz":          anyMap(healthzPayload["build"]),
		"context_boundary": anyMap(contextBoundaryPayload()["build"]),
		"native_ownership": anyMap((&server{}).nativeOwnershipPayload()["build"]),
	}
	fields := []string{"schema_id", "version", "channel", "source_commit", "source_tree", "source_bound", "boot_nonce"}
	for name, identity := range payloads {
		for _, field := range fields {
			if anyToString(identity[field]) != anyToString(expected[field]) {
				t.Fatalf("%s build identity field %s=%v want=%v", name, field, identity[field], expected[field])
			}
		}
	}
}

func TestContextLatticeBuildIdentityRequiresFullGitObjectIDs(t *testing.T) {
	originalCommit, originalTree := contextLatticeSourceCommit, contextLatticeSourceTree
	t.Cleanup(func() {
		contextLatticeSourceCommit, contextLatticeSourceTree = originalCommit, originalTree
	})

	testCases := []struct {
		name   string
		commit string
		tree   string
		bound  bool
	}{
		{name: "sha1", commit: strings.Repeat("a", 40), tree: strings.Repeat("b", 40), bound: true},
		{name: "sha256", commit: strings.Repeat("c", 64), tree: strings.Repeat("d", 64), bound: true},
		{name: "unknown", commit: "unknown", tree: "unknown"},
		{name: "short", commit: "foo", tree: "bar"},
		{name: "uppercase", commit: strings.Repeat("A", 40), tree: strings.Repeat("b", 40)},
		{name: "non hexadecimal", commit: strings.Repeat("g", 40), tree: strings.Repeat("b", 40)},
		{name: "mixed lengths", commit: strings.Repeat("a", 40), tree: strings.Repeat("b", 39)},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			contextLatticeSourceCommit, contextLatticeSourceTree = testCase.commit, testCase.tree
			if got := anyToBool(contextLatticeBuildIdentity()["source_bound"]); got != testCase.bound {
				t.Fatalf("source_bound=%v want=%v identity=%#v", got, testCase.bound, contextLatticeBuildIdentity())
			}
		})
	}
}
