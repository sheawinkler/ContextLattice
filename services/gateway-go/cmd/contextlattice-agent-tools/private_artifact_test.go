package main

import (
	"strings"
	"testing"
)

func TestValidatePrivateArtifactWindowsLeaf(t *testing.T) {
	for _, valid := range []string{"private.json", "proof-ledger.ndjson", "com10.json", "context lattice.txt"} {
		if err := validatePrivateArtifactWindowsLeaf(valid); err != nil {
			t.Fatalf("valid Windows private artifact leaf %q was rejected: %v", valid, err)
		}
	}

	for _, invalid := range []string{
		"", ".", "..", "private.json:stream", "private?.json", "private*.json", "private.json.", "private.json ",
		"CON", "con.txt", "PRN.json", "AUX", "NUL.ndjson", "CLOCK$", "CONIN$.json", "CONOUT$",
		"COM1", "com9.log", "COM\u00b9.txt", "LPT1", "lpt9.json", "LPT\u00b2.log", "private\x01.json", "private\x7f.json",
	} {
		t.Run(strings.ReplaceAll(invalid, "/", "_"), func(t *testing.T) {
			if err := validatePrivateArtifactWindowsLeaf(invalid); err == nil {
				t.Fatalf("unsafe Windows private artifact leaf %q was accepted", invalid)
			}
		})
	}
}

func TestValidatePrivateArtifactWindowsComponentsRejectsParentBeforeMutation(t *testing.T) {
	if err := validatePrivateArtifactWindowsComponents([]string{"safe", "nested", "private.json"}); err != nil {
		t.Fatalf("safe Windows target components were rejected: %v", err)
	}
	for _, components := range [][]string{
		{"new-parent", "bad:name", "private.json"},
		{"new-parent", "CONIN$", "private.json"},
		{"new-parent", "private\x7f", "private.json"},
	} {
		if err := validatePrivateArtifactWindowsComponents(components); err == nil {
			t.Fatalf("unsafe Windows target components were accepted: %#v", components)
		}
	}
}

func TestValidatePrivateArtifactWindowsNamespace(t *testing.T) {
	for _, valid := range []string{`C:\safe\private.json`, `\\server\share\private.json`, `relative\private.json`} {
		if err := validatePrivateArtifactWindowsNamespace(valid); err != nil {
			t.Fatalf("ordinary Windows path %q was rejected: %v", valid, err)
		}
	}
	for _, invalid := range []string{
		`\\.\GLOBALROOT\Device\HarddiskVolumeShadowCopy1\private.json`,
		`\\?\C:\safe\private.json`,
		`\??\C:\safe\private.json`,
		`//?/C:/safe/private.json`,
	} {
		if err := validatePrivateArtifactWindowsNamespace(invalid); err == nil {
			t.Fatalf("Windows device namespace %q was accepted", invalid)
		}
	}
}
