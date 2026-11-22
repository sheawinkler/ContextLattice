package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func dreamTransferOwnerTestArgs(dir string) []string {
	return []string{
		"--source", "source-project", "--destination", "destination-project",
		"--passport", "passport_example", "--experience", "experience:example",
		"--source-digest", "sha256:" + strings.Repeat("a", 64),
		"--grant-file", filepath.Join(dir, "grant.json"), "--proof-file", filepath.Join(dir, "proof"),
		"--ttl", "15m",
	}
}

func TestDreamTransferAuthorityParserRejectsInvalidScope(t *testing.T) {
	good := dreamTransferOwnerTestArgs(t.TempDir())
	if _, err := parseDreamTransferAuthority(good); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		make func([]string) []string
	}{
		{"same scope", func(a []string) []string { a[3] = a[1]; return a }},
		{"case alias", func(a []string) []string { a[3] = strings.ToUpper(a[1]); return a }},
		{"source traversal", func(a []string) []string { a[1] = "../other"; return a }},
		{"missing destination", func(a []string) []string { return append(a[:2], a[4:]...) }},
		{"duplicate consent", func(a []string) []string { a[12] = "--source"; return a }},
		{"wrong digest", func(a []string) []string { a[9] = "sha256:" + strings.Repeat("A", 64); return a }},
		{"unbounded experience", func(a []string) []string { a[7] = strings.Repeat("x", 257); return a }},
		{"wrong ttl", func(a []string) []string { a[15] = "15m1ns"; return a }},
		{"no ttl", func(a []string) []string { a[15] = "0s"; return a }},
		{"same output", func(a []string) []string { a[13] = a[11]; return a }},
		{"relative output", func(a []string) []string { a[13] = "proof"; return a }},
		{"credential flag", func(a []string) []string { a[12] = "--api-key"; return a }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDreamTransferAuthority(tc.make(append([]string(nil), good...))); err == nil {
				t.Fatal("invalid arguments were accepted")
			}
		})
	}
}

func TestDreamTransferAuthorityUnsupportedDoesNotReadEnvironment(t *testing.T) {
	if dreamTransferAuthoritySupported {
		return
	}
	var stdout, stderr bytes.Buffer
	exit := runDreamTransferAuthority(nil, &stdout, &stderr, func(string) string {
		t.Fatal("unsupported platform read configuration")
		return ""
	})
	if exit == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "only on Darwin and Linux") {
		t.Fatal("unsupported platform did not fail closed")
	}
}
