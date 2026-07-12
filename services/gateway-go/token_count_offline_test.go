package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestContextPackTokenizerUsesEmbeddedOfflineVocabulary(t *testing.T) {
	if os.Getenv("CONTEXTLATTICE_TOKENIZER_OFFLINE_TEST_CHILD") == "1" {
		result := contextPackCountTokens("hello world")
		if !result.TokenizerExact || result.Method != "tiktoken" || result.Encoding != defaultContextPackTokenizerEncoding {
			t.Fatalf("expected embedded tokenizer vocabulary, got %#v", result)
		}
		return
	}

	cacheDir := filepath.Join(t.TempDir(), "must-stay-empty")
	cmd := exec.Command(os.Args[0], "-test.run=^TestContextPackTokenizerUsesEmbeddedOfflineVocabulary$")
	cmd.Env = append(os.Environ(),
		"CONTEXTLATTICE_TOKENIZER_OFFLINE_TEST_CHILD=1",
		"CONTEXTLATTICE_TOKENIZER_ENCODING="+defaultContextPackTokenizerEncoding,
		"TIKTOKEN_CACHE_DIR="+cacheDir,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("offline tokenizer child failed: %v\n%s", err, output)
	}
	if entries, err := os.ReadDir(cacheDir); err == nil && len(entries) > 0 {
		t.Fatalf("embedded tokenizer unexpectedly wrote runtime cache entries: %v", entries)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("inspect tokenizer cache: %v", err)
	}
}
