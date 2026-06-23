package main

import "testing"

func TestContextBoundaryIncludesAsyncInboxDrain(t *testing.T) {
	var found map[string]any
	for _, surface := range contextBoundaryRequiredSurfaces() {
		if surface.Path != "contextlattice_async_inbox_drain" {
			continue
		}
		found = map[string]any{
			"name":       surface.Name,
			"contract":   surface.ContractID,
			"json_bytes": surface.LocalMaxJSONBytes,
			"string":     surface.LocalMaxString,
			"items":      surface.LocalMaxListItems,
			"owner":      surface.RuntimeOwner,
		}
		if surface.Surface != "agent_cli" {
			t.Fatalf("async inbox drain should be agent_cli surface: %#v", surface)
		}
		if surface.RuntimeOwner != sourceOwnerGoNative {
			t.Fatalf("async inbox drain should be go-native owned: %#v", surface)
		}
		if surface.LocalMaxJSONBytes <= 0 || surface.LocalMaxString <= 0 || surface.LocalMaxListItems <= 0 {
			t.Fatalf("async inbox drain missing local output budget: %#v", surface)
		}
	}
	if found == nil {
		t.Fatalf("async inbox drain boundary surface missing")
	}
}
