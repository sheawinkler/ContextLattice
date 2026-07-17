package main

import (
	"crypto/rand"
	"encoding/hex"
	"io"
	"strings"
)

const contextLatticeBootNonceBytes = 16

var (
	contextLatticeBuildVersion     = "development"
	contextLatticeBuildChannel     = "local"
	contextLatticeSourceCommit     = "unknown"
	contextLatticeSourceTree       = "unknown"
	contextLatticeProcessBootNonce = mustContextLatticeBootNonce(rand.Reader)
)

func newContextLatticeBootNonce(reader io.Reader) (string, error) {
	raw := make([]byte, contextLatticeBootNonceBytes)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func mustContextLatticeBootNonce(reader io.Reader) string {
	nonce, err := newContextLatticeBootNonce(reader)
	if err != nil {
		panic("generate ContextLattice process boot nonce: " + err.Error())
	}
	return nonce
}

func normalizedBuildIdentityValue(value string, fallback string) string {
	if normalized := strings.TrimSpace(value); normalized != "" {
		return normalized
	}
	return fallback
}

func contextLatticeBuildIdentity() map[string]any {
	commit := normalizedBuildIdentityValue(contextLatticeSourceCommit, "unknown")
	tree := normalizedBuildIdentityValue(contextLatticeSourceTree, "unknown")
	return map[string]any{
		"schema_id":     "contextlattice_build_identity.v1",
		"version":       normalizedBuildIdentityValue(contextLatticeBuildVersion, "development"),
		"channel":       normalizedBuildIdentityValue(contextLatticeBuildChannel, "local"),
		"source_commit": commit,
		"source_tree":   tree,
		"source_bound":  contextLatticeGitObjectID(commit) && contextLatticeGitObjectID(tree),
		"boot_nonce":    normalizedBuildIdentityValue(contextLatticeProcessBootNonce, "unknown"),
	}
}

func contextLatticeGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
