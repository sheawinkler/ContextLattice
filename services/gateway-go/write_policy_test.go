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

func TestNormalizeFanoutTargets(t *testing.T) {
	normalized := normalizeFanoutTargets([]string{" mindsdb ", "qdrant", "invalid_target", "mindsdb"})
	if len(normalized) != 2 {
		t.Fatalf("expected deduped targets, got %#v", normalized)
	}
	if normalized[0] != "mindsdb" || normalized[1] != "qdrant" {
		t.Fatalf("unexpected normalized target order/content: %#v", normalized)
	}
}
