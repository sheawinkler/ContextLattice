package main

import "testing"

func TestWritePolicyTelemetryByTopic(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled: true,
		telemetryTopicPrefixes:    []string{"telemetry", "state"},
		telemetryFilePatterns:     []string{"telemetry__*.json"},
		telemetryMarkers:          []string{"__state__"},
	}
	item := normalizedWrite{project: "p", fileName: "notes.md", content: "x", topicPath: "telemetry/runtime"}
	if !policy.isTelemetryLike(item) {
		t.Fatalf("expected telemetry classification by topic")
	}
}

func TestWritePolicyTelemetryByFilePattern(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled: true,
		telemetryTopicPrefixes:    []string{"telemetry"},
		telemetryFilePatterns:     []string{"telemetry__*.json"},
		telemetryMarkers:          []string{"__state__"},
	}
	item := normalizedWrite{project: "p", fileName: "telemetry__agg-latest.json", content: "x", topicPath: "runbooks"}
	if !policy.isTelemetryLike(item) {
		t.Fatalf("expected telemetry classification by filename")
	}
}

func TestWritePolicyNonTelemetry(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled: true,
		telemetryTopicPrefixes:    []string{"telemetry"},
		telemetryFilePatterns:     []string{"telemetry__*.json"},
		telemetryMarkers:          []string{"__state__"},
	}
	item := normalizedWrite{project: "p", fileName: "architecture/plan.md", content: "design", topicPath: "learning/architecture"}
	if policy.isTelemetryLike(item) {
		t.Fatalf("expected non-telemetry classification")
	}
}

func TestWritePolicyRequiredValidation(t *testing.T) {
	policy := writeIngressPolicy{strictRequiredFields: true}
	if err := policy.validateWrite(normalizedWrite{project: " ", fileName: "x", content: "x"}); err == nil {
		t.Fatalf("expected project validation error")
	}
	if err := policy.validateWrite(normalizedWrite{project: "p", fileName: "", content: "x"}); err == nil {
		t.Fatalf("expected file validation error")
	}
	if err := policy.validateWrite(normalizedWrite{project: "p", fileName: "x", content: "null"}); err == nil {
		t.Fatalf("expected content validation error for placeholder")
	}
	if err := policy.validateWrite(normalizedWrite{project: "p", fileName: "x", content: "ok"}); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestNormalizeWritePayloadV1Put(t *testing.T) {
	item, err := normalizeWritePayload("/v1/memory/put", map[string]any{
		"item": map[string]any{
			"project":   "proj",
			"file_name": "file.md",
			"content":   "hello",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.project != "proj" || item.fileName != "file.md" || item.content != "hello" {
		t.Fatalf("unexpected normalized payload: %+v", item)
	}
}

func TestBuildForwardPayloadIncludesFanoutExcludes(t *testing.T) {
	item := normalizedWrite{
		project:   "proj",
		fileName:  "notes.md",
		content:   "hello",
		topicPath: "runbooks/x",
	}
	excludes := []string{"mindsdb"}

	writePayload := buildForwardPayload("/memory/write", item, excludes)
	gotTargets, ok := writePayload["fanoutExcludeTargets"].([]string)
	if !ok || len(gotTargets) != 1 || gotTargets[0] != "mindsdb" {
		t.Fatalf("expected fanoutExcludeTargets on /memory/write payload, got %#v", writePayload["fanoutExcludeTargets"])
	}

	v1Payload := buildForwardPayload("/v1/memory/put", item, excludes)
	rawItem, ok := v1Payload["item"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested item payload for /v1/memory/put")
	}
	rawTargets, ok := rawItem["fanout_exclude_targets"].([]string)
	if !ok || len(rawTargets) != 1 || rawTargets[0] != "mindsdb" {
		t.Fatalf("expected fanout_exclude_targets on /v1/memory/put payload, got %#v", rawItem["fanout_exclude_targets"])
	}
}

func TestNormalizeWritePayloadCanonicalMetadata(t *testing.T) {
	item, err := normalizeWritePayload("/memory/write", map[string]any{
		"projectName": "contextlattice",
		"fileName":    "notes/a.md",
		"content":     "alpha",
		"topicPath":   "runbooks/testing",
		"metadata": map[string]any{
			"agent_id":   "codex_gpt5",
			"session_id": "sess-1",
			"tags":       []string{"lane:go", "runtime:v4"},
			"created_at": "2026-04-01T00:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.agentID != "codex_gpt5" {
		t.Fatalf("expected canonical agent_id, got %q", item.agentID)
	}
	if item.sessionID != "sess-1" {
		t.Fatalf("expected canonical session_id, got %q", item.sessionID)
	}
	if len(item.tags) != 2 {
		t.Fatalf("expected canonical tags, got %#v", item.tags)
	}
	if item.createdAt != "2026-04-01T00:00:00Z" {
		t.Fatalf("expected canonical created_at, got %q", item.createdAt)
	}
}

func TestBuildForwardPayloadIncludesCanonicalMetadata(t *testing.T) {
	item := normalizedWrite{
		project:   "proj",
		fileName:  "notes.md",
		content:   "hello",
		topicPath: "runbooks/x",
		agentID:   "codex_gpt5",
		sessionID: "session-abc",
		tags:      []string{"runtime:v4", "lane:mongo_raw"},
		createdAt: "2026-04-01T01:02:03Z",
	}

	writePayload := buildForwardPayload("/memory/write", item, nil)
	if got := anyToString(writePayload["agent_id"]); got != item.agentID {
		t.Fatalf("expected /memory/write agent_id=%q, got %q", item.agentID, got)
	}
	if got := anyToString(writePayload["session_id"]); got != item.sessionID {
		t.Fatalf("expected /memory/write session_id=%q, got %q", item.sessionID, got)
	}
	if got := anyToString(writePayload["created_at"]); got != item.createdAt {
		t.Fatalf("expected /memory/write created_at=%q, got %q", item.createdAt, got)
	}
	if tags := anyToStringSlice(writePayload["tags"]); len(tags) != 2 {
		t.Fatalf("expected /memory/write tags, got %#v", writePayload["tags"])
	}

	v1Payload := buildForwardPayload("/v1/memory/put", item, nil)
	rawItem, ok := v1Payload["item"].(map[string]any)
	if !ok {
		t.Fatalf("expected nested item payload for /v1/memory/put")
	}
	if got := anyToString(rawItem["agent_id"]); got != item.agentID {
		t.Fatalf("expected /v1/memory/put agent_id=%q, got %q", item.agentID, got)
	}
	if got := anyToString(rawItem["session_id"]); got != item.sessionID {
		t.Fatalf("expected /v1/memory/put session_id=%q, got %q", item.sessionID, got)
	}
	if got := anyToString(rawItem["created_at"]); got != item.createdAt {
		t.Fatalf("expected /v1/memory/put created_at=%q, got %q", item.createdAt, got)
	}
	if tags := anyToStringSlice(rawItem["tags"]); len(tags) != 2 {
		t.Fatalf("expected /v1/memory/put tags, got %#v", rawItem["tags"])
	}
}

func TestNormalizeFanoutTargets(t *testing.T) {
	normalized := normalizeFanoutTargets([]string{" mindsdb ", "qdrant", "invalid_target", "mindsdb"})
	if len(normalized) != 2 {
		t.Fatalf("expected deduped targets, got %#v", normalized)
	}
	if normalized[0] != "mindsdb" || normalized[1] != "qdrant" {
		t.Fatalf("unexpected normalized target order/content: %#v", normalized)
	}
}
