package main

import (
	"testing"
	"time"
)

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

func TestWritePolicyDurableTopicShieldsTelemetryWords(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled:  true,
		telemetryTopicPrefixes:     []string{"telemetry", "metrics"},
		telemetryFilePatterns:      []string{"telemetry__*.json", "*_agg-latest.json"},
		telemetryMarkers:           []string{"telemetry", "metrics", "__state__"},
		durableMemoryTopicPrefixes: []string{"decisions", "learnings", "ideas"},
	}
	item := normalizedWrite{
		project:   "project",
		fileName:  "notes/codex/runtime-metrics-decision.md",
		content:   "decision: runtime metrics are evidence, not a raw telemetry payload",
		topicPath: "decisions/runtime-metrics",
	}
	if policy.isTelemetryLike(item) {
		t.Fatalf("expected durable topic to preserve agent memory despite telemetry words")
	}
}

func TestWritePolicyDurableTopicDoesNotShieldRawTelemetryArtifact(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled:  true,
		telemetryTopicPrefixes:     []string{"telemetry", "metrics"},
		telemetryFilePatterns:      []string{"telemetry__*.json", "*_agg-latest.json"},
		telemetryMarkers:           []string{"telemetry", "metrics", "__state__"},
		durableMemoryTopicPrefixes: []string{"decisions", "learnings", "ideas"},
	}
	item := normalizedWrite{
		project:   "project",
		fileName:  "telemetry__agg-latest.json",
		content:   `{"latency_ms":12,"event":"heartbeat"}`,
		topicPath: "decisions/runtime-metrics",
	}
	if !policy.isTelemetryLike(item) {
		t.Fatalf("expected raw telemetry artifact filename to remain telemetry-like")
	}
}

func TestWritePolicyDurableRuntimeStateFileOverridesTelemetryPattern(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled: true,
		telemetryFilePatterns:     []string{"*__state__*.json"},
		telemetryMarkers:          []string{"__state__"},
		durableMemoryFilePatterns: []string{"runtime__state__*.json"},
		durableMemoryFileProjects: []string{"project-*"},
	}
	item := normalizedWrite{
		project:  "project-alpha",
		fileName: "runtime__state__latest.json",
		content:  `{"generation":21,"positions":[]}`,
	}
	if policy.isTelemetryLike(item) {
		t.Fatal("expected configured runtime state snapshot to remain durable memory")
	}
}

func TestWritePolicyUnlistedRuntimeStateRemainsTelemetry(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled: true,
		telemetryFilePatterns:     []string{"*__state__*.json"},
		telemetryMarkers:          []string{"__state__"},
		durableMemoryFilePatterns: []string{"runtime__state__*.json"},
		durableMemoryFileProjects: []string{"project-*"},
	}
	item := normalizedWrite{
		project:  "project-alpha",
		fileName: "discovery__state__latest.json",
		content:  `{"round":1}`,
	}
	if !policy.isTelemetryLike(item) {
		t.Fatal("expected unlisted runtime state to remain telemetry")
	}
}

func TestWritePolicyDurableRuntimeStateRequiresProjectAndFileMatch(t *testing.T) {
	policy := writeIngressPolicy{
		telemetryIsolationEnabled: true,
		telemetryFilePatterns:     []string{"*__state__*.json"},
		telemetryMarkers:          []string{"__state__"},
		durableMemoryFilePatterns: []string{"runtime__state__*.json"},
		durableMemoryFileProjects: []string{"project-*"},
	}
	item := normalizedWrite{
		project:  "unrelated-project",
		fileName: "runtime__state__latest.json",
		content:  `{"generation":21}`,
	}
	if !policy.isTelemetryLike(item) {
		t.Fatal("expected a matching filename outside the configured project scope to remain telemetry")
	}
	classified := policy.classifyWrite(item)
	if classified.dataClass != dataClassLearningMemory {
		t.Fatalf("unexpected data class outside project scope: %q", classified.dataClass)
	}
}

func TestWritePolicyRuntimeStateFallbackExcludesHistoricalAndVectorSinks(t *testing.T) {
	policy := writeIngressPolicy{
		fanoutExcludeTargets:      []string{"mindsdb"},
		durableMemoryFilePatterns: []string{"runtime__state__*.json"},
		durableMemoryFileProjects: []string{"project-*"},
	}
	item := policy.classifyWrite(normalizedWrite{
		project:  "project-alpha",
		fileName: "runtime__state__latest.json",
		content:  `{"generation":2}`,
	})
	if item.dataClass != dataClassRuntimeStateMirror {
		t.Fatalf("unexpected data class: %q", item.dataClass)
	}
	targets := policy.fanoutExcludeTargetsFor(item)
	for _, target := range []string{"mongo_raw", "qdrant", "postgres_pgvector", "mindsdb", "letta"} {
		found := false
		for _, actual := range targets {
			if actual == target {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("runtime state fallback did not exclude %q: %#v", target, targets)
		}
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
			"tags":       []string{"lane:go", "runtime:go"},
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

func TestNormalizeWritePayloadDefaultsCanonicalMetadata(t *testing.T) {
	t.Setenv("GO_WRITE_DEFAULT_AGENT_ID", "gateway-test-agent")
	t.Setenv("GO_WRITE_DEFAULT_SESSION_ID", "gateway-test-session")
	t.Setenv("GO_WRITE_DEFAULT_TAGS", "source:gateway-go,kind:test")

	item, err := normalizeWritePayload("/memory/write", map[string]any{
		"projectName": "contextlattice",
		"fileName":    "notes/a.md",
		"content":     "alpha",
		"topicPath":   "runbooks/testing",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.agentID != "gateway-test-agent" {
		t.Fatalf("expected default agent_id, got %q", item.agentID)
	}
	if item.sessionID != "gateway-test-session" {
		t.Fatalf("expected default session_id, got %q", item.sessionID)
	}
	if len(item.tags) != 2 {
		t.Fatalf("expected default tags, got %#v", item.tags)
	}
	if _, err := time.Parse(time.RFC3339Nano, item.createdAt); err != nil {
		t.Fatalf("expected generated RFC3339 created_at, got %q: %v", item.createdAt, err)
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
		tags:      []string{"runtime:go", "lane:mongo_raw"},
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
