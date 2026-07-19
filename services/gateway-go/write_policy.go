package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	dataClassLearningMemory     = "learning_memory"
	dataClassRuntimeStateMirror = "runtime_state_mirror"
)

type writeIngressPolicy struct {
	enabled                    bool
	strictRequiredFields       bool
	telemetryIsolationEnabled  bool
	telemetryTopicPrefixes     []string
	telemetryFilePatterns      []string
	telemetryMarkers           []string
	durableMemoryTopicPrefixes []string
	durableMemoryFilePatterns  []string
	durableMemoryFileProjects  []string
	fanoutExcludeTargets       []string
	batchConcurrency           int
}

type normalizedWrite struct {
	project        string
	fileName       string
	content        string
	topicPath      string
	agentID        string
	sessionID      string
	tags           []string
	createdAt      string
	lifecycle      string
	storageTier    string
	dataClass      string
	itemID         string
	idempotencyKey string
	raw            map[string]any
}

func loadWriteIngressPolicy() writeIngressPolicy {
	policy := writeIngressPolicy{
		enabled:                   envBool("GO_WRITE_INGRESS_ENABLED", true),
		strictRequiredFields:      envBool("GO_WRITE_STRICT_REQUIRED_FIELDS", true),
		telemetryIsolationEnabled: envBool("GO_WRITE_TELEMETRY_ISOLATION_ENABLED", true),
		telemetryTopicPrefixes: csvLowerListEnv(
			"GO_WRITE_TELEMETRY_TOPIC_PREFIXES",
			"telemetry,metrics,signals,overrides,perf,tmp,state,states,snapshots,health,stats,allocations,system_state",
		),
		telemetryFilePatterns: csvLowerListEnv(
			"GO_WRITE_TELEMETRY_FILE_PATTERNS",
			"index__*.json,*_agg-latest.json,*_agg-*.json,*__agg-*.json,telemetry__*.json,*__state__*.json,*__stats__*.json,*__snapshots__*.json,*__health__*.json,*__allocations__*.json",
		),
		telemetryMarkers: csvLowerListEnv(
			"GO_WRITE_TELEMETRY_MARKERS",
			"telemetry,metrics,__state__,__stats__,__snapshots__,__health__,__allocations__,_agg-,queue__",
		),
		durableMemoryTopicPrefixes: csvLowerListEnv(
			"GO_WRITE_DURABLE_MEMORY_TOPIC_PREFIXES",
			"decisions,decision,discoveries,discovery,learnings,learning,ideas,notes,runbooks,architecture,projects,knowledge,handoffs,checkpoints,analysis,findings,plans,objectives,missions,skills",
		),
		durableMemoryFilePatterns: csvLowerListEnv(
			"GO_WRITE_DURABLE_MEMORY_FILE_PATTERNS",
			"",
		),
		durableMemoryFileProjects: csvLowerListEnv(
			"GO_WRITE_DURABLE_MEMORY_FILE_PROJECT_PATTERNS",
			"",
		),
		fanoutExcludeTargets: normalizeFanoutTargets(
			csvLowerListEnv("GO_WRITE_FANOUT_EXCLUDE_TARGETS", "mindsdb"),
		),
		batchConcurrency: envInt("GO_WRITE_BATCH_CONCURRENCY", 4),
	}
	if policy.batchConcurrency < 1 {
		policy.batchConcurrency = 1
	}
	if policy.batchConcurrency > 16 {
		policy.batchConcurrency = 16
	}
	return policy
}

func normalizeFanoutTargets(raw []string) []string {
	allowed := map[string]struct{}{
		"mongo_raw":         {},
		"qdrant":            {},
		"postgres_pgvector": {},
		"langfuse":          {},
		"mindsdb":           {},
		"letta":             {},
	}
	rows := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, value := range raw {
		target := strings.TrimSpace(strings.ToLower(value))
		if target == "" {
			continue
		}
		if _, ok := allowed[target]; !ok {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		rows = append(rows, target)
	}
	return rows
}

func csvLowerListEnv(name string, fallback string) []string {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		raw = fallback
	}
	parts := strings.Split(raw, ",")
	rows := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		normalized := strings.TrimSpace(strings.ToLower(part))
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		rows = append(rows, normalized)
	}
	return rows
}

func normalizeWritePayload(path string, payload map[string]any) (normalizedWrite, error) {
	item := normalizedWrite{raw: payload}
	switch path {
	case "/memory/write":
		item.project = strings.TrimSpace(anyToString(payload["projectName"]))
		item.fileName = strings.TrimSpace(anyToString(payload["fileName"]))
		item.content = strings.TrimSpace(anyToString(payload["content"]))
		item.topicPath = strings.TrimSpace(anyToString(payload["topicPath"]))
	case "/v1/memory/put":
		rawItem, ok := payload["item"].(map[string]any)
		if !ok {
			return normalizedWrite{}, errors.New("item is required")
		}
		item.raw = rawItem
		item.project = strings.TrimSpace(anyToString(rawItem["project"]))
		item.fileName = strings.TrimSpace(anyToString(rawItem["file_name"]))
		item.content = strings.TrimSpace(anyToString(rawItem["content"]))
		item.topicPath = strings.TrimSpace(anyToString(rawItem["topic_path"]))
	default:
		return normalizedWrite{}, fmt.Errorf("unsupported write path: %s", path)
	}
	meta := normalizeWriteMetadata(item.raw)
	item.agentID = meta.agentID
	item.sessionID = meta.sessionID
	item.tags = meta.tags
	item.createdAt = meta.createdAt
	item.lifecycle = meta.lifecycle
	return item, nil
}

func normalizeWriteBatchPayload(path string, payload map[string]any) ([]normalizedWrite, error) {
	rawItems, ok := payload["items"]
	if !ok {
		return nil, errors.New("items is required")
	}
	list, ok := asAnySlice(rawItems)
	if !ok || len(list) == 0 {
		return nil, errors.New("items must contain at least one write")
	}
	rows := make([]normalizedWrite, 0, len(list))
	for idx, raw := range list {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("item %d must be an object", idx)
		}
		item := normalizedWrite{raw: itemMap}
		switch path {
		case "/memory/write/batch":
			item.project = strings.TrimSpace(anyToString(itemMap["projectName"]))
			item.fileName = strings.TrimSpace(anyToString(itemMap["fileName"]))
			item.content = strings.TrimSpace(anyToString(itemMap["content"]))
			item.topicPath = strings.TrimSpace(anyToString(itemMap["topicPath"]))
			item.itemID = strings.TrimSpace(anyToString(itemMap["itemId"]))
			item.idempotencyKey = strings.TrimSpace(anyToString(itemMap["idempotencyKey"]))
		case "/tools/memory_write_batch":
			item.project = strings.TrimSpace(anyToString(itemMap["projectName"]))
			item.fileName = strings.TrimSpace(anyToString(itemMap["fileName"]))
			item.content = strings.TrimSpace(anyToString(itemMap["content"]))
			item.topicPath = strings.TrimSpace(anyToString(itemMap["topicPath"]))
			item.itemID = strings.TrimSpace(anyToString(itemMap["itemId"]))
			item.idempotencyKey = strings.TrimSpace(anyToString(itemMap["idempotencyKey"]))
		case "/v1/memory/batch-put":
			item.project = strings.TrimSpace(anyToString(itemMap["project"]))
			item.fileName = strings.TrimSpace(anyToString(itemMap["file_name"]))
			item.content = strings.TrimSpace(anyToString(itemMap["content"]))
			item.topicPath = strings.TrimSpace(anyToString(itemMap["topic_path"]))
		default:
			return nil, fmt.Errorf("unsupported batch path: %s", path)
		}
		meta := normalizeWriteMetadata(item.raw)
		item.agentID = meta.agentID
		item.sessionID = meta.sessionID
		item.tags = meta.tags
		item.createdAt = meta.createdAt
		item.lifecycle = meta.lifecycle
		rows = append(rows, item)
	}
	return rows, nil
}

func asAnySlice(value any) ([]any, bool) {
	if typed, ok := value.([]any); ok {
		return typed, true
	}
	if typed, ok := value.([]map[string]any); ok {
		rows := make([]any, 0, len(typed))
		for _, row := range typed {
			rows = append(rows, row)
		}
		return rows, true
	}
	return nil, false
}

func (p writeIngressPolicy) validateWrite(item normalizedWrite) error {
	if !p.strictRequiredFields {
		return nil
	}
	if !validRequiredValue(item.project) {
		return errors.New("project must be non-empty")
	}
	if !validRequiredValue(item.fileName) {
		return errors.New("fileName must be non-empty")
	}
	if !validRequiredValue(item.content) {
		return errors.New("content must be non-empty")
	}
	return nil
}

func validRequiredValue(value string) bool {
	normalized := strings.TrimSpace(value)
	if normalized == "" {
		return false
	}
	switch strings.ToLower(normalized) {
	case "null", "none", "undefined":
		return false
	default:
		return true
	}
}

func (p writeIngressPolicy) isTelemetryLike(item normalizedWrite) bool {
	if !p.telemetryIsolationEnabled {
		return false
	}
	if item.dataClass == dataClassRuntimeStateMirror {
		return false
	}
	if p.isDurableMemoryFile(item) {
		return false
	}
	normalizedTopic := normalizeTopic(item.topicPath)
	hardTelemetryFile := p.isHardTelemetryFile(item.fileName)
	if normalizedTopic != "" && p.isDurableMemoryTopic(normalizedTopic) && !hardTelemetryFile {
		return false
	}
	if normalizedTopic != "" {
		for _, prefix := range p.telemetryTopicPrefixes {
			if normalizedTopic == prefix || strings.HasPrefix(normalizedTopic, prefix+"/") {
				return true
			}
		}
	}
	lowerFile := strings.ToLower(strings.TrimSpace(item.fileName))
	if lowerFile != "" {
		if hardTelemetryFile {
			return true
		}
		for _, marker := range p.telemetryMarkers {
			if marker != "" && strings.Contains(lowerFile, marker) {
				return true
			}
		}
	}
	lowerTopic := strings.ToLower(normalizedTopic)
	for _, marker := range p.telemetryMarkers {
		if marker != "" && strings.Contains(lowerTopic, marker) {
			return true
		}
	}
	return false
}

func (p writeIngressPolicy) isDurableMemoryFile(item normalizedWrite) bool {
	lowerFile := strings.ToLower(strings.TrimSpace(item.fileName))
	if lowerFile == "" {
		return false
	}
	project := strings.ToLower(strings.TrimSpace(item.project))
	projectMatched := false
	for _, pattern := range p.durableMemoryFileProjects {
		if pattern != "" && (project == pattern || globMatches(pattern, project)) {
			projectMatched = true
			break
		}
	}
	if !projectMatched {
		return false
	}
	for _, pattern := range p.durableMemoryFilePatterns {
		if globMatches(pattern, lowerFile) {
			return true
		}
	}
	return false
}

func (p writeIngressPolicy) classifyWrite(item normalizedWrite) normalizedWrite {
	item.dataClass = dataClassLearningMemory
	if p.isDurableMemoryFile(item) {
		item.dataClass = dataClassRuntimeStateMirror
	}
	return item
}

func (s *server) classifyWrite(item normalizedWrite) normalizedWrite {
	item = s.writePolicy.classifyWrite(item)
	if s.memoryStore != nil && s.memoryStore.isExactStatePath(item.project, item.fileName) {
		item.dataClass = dataClassRuntimeStateMirror
	}
	return item
}

func (p writeIngressPolicy) fanoutExcludeTargetsFor(item normalizedWrite) []string {
	rows := append([]string{}, p.fanoutExcludeTargets...)
	if item.dataClass == dataClassRuntimeStateMirror {
		rows = append(rows, "mongo_raw", "qdrant", "postgres_pgvector", "mindsdb", "letta")
	}
	return normalizeFanoutTargets(rows)
}

func (p writeIngressPolicy) isDurableMemoryTopic(normalizedTopic string) bool {
	for _, prefix := range p.durableMemoryTopicPrefixes {
		if prefix != "" && (normalizedTopic == prefix || strings.HasPrefix(normalizedTopic, prefix+"/")) {
			return true
		}
	}
	return false
}

func (p writeIngressPolicy) isHardTelemetryFile(fileName string) bool {
	lowerFile := strings.ToLower(strings.TrimSpace(fileName))
	if lowerFile == "" {
		return false
	}
	for _, pattern := range p.telemetryFilePatterns {
		if globMatches(pattern, lowerFile) {
			return true
		}
	}
	return false
}

func normalizeTopic(value string) string {
	normalized := strings.Trim(strings.ToLower(strings.TrimSpace(value)), "/")
	normalized = strings.ReplaceAll(normalized, "//", "/")
	return normalized
}

func globMatches(pattern string, candidate string) bool {
	pattern = strings.TrimSpace(strings.ToLower(pattern))
	candidate = strings.TrimSpace(strings.ToLower(candidate))
	if pattern == "" || candidate == "" {
		return false
	}
	if ok, _ := filepath.Match(pattern, candidate); ok {
		return true
	}
	base := filepath.Base(candidate)
	if ok, _ := filepath.Match(pattern, base); ok {
		return true
	}
	return false
}

func buildForwardPayload(path string, item normalizedWrite, fanoutExcludeTargets []string) map[string]any {
	switch path {
	case "/memory/write":
		forward := map[string]any{
			"projectName": item.project,
			"fileName":    item.fileName,
			"content":     item.content,
			"topicPath":   item.topicPath,
		}
		if len(fanoutExcludeTargets) > 0 {
			forward["fanoutExcludeTargets"] = append([]string{}, fanoutExcludeTargets...)
		}
		if item.agentID != "" {
			forward["agent_id"] = item.agentID
		}
		if item.sessionID != "" {
			forward["session_id"] = item.sessionID
		}
		if len(item.tags) > 0 {
			forward["tags"] = append([]string{}, item.tags...)
		}
		if item.createdAt != "" {
			forward["created_at"] = item.createdAt
		}
		return forward
	case "/v1/memory/put":
		rawItem := map[string]any{
			"project":    item.project,
			"file_name":  item.fileName,
			"content":    item.content,
			"topic_path": item.topicPath,
		}
		if len(fanoutExcludeTargets) > 0 {
			rawItem["fanout_exclude_targets"] = append([]string{}, fanoutExcludeTargets...)
		}
		if item.agentID != "" {
			rawItem["agent_id"] = item.agentID
		}
		if item.sessionID != "" {
			rawItem["session_id"] = item.sessionID
		}
		if len(item.tags) > 0 {
			rawItem["tags"] = append([]string{}, item.tags...)
		}
		if item.createdAt != "" {
			rawItem["created_at"] = item.createdAt
		}
		return map[string]any{
			"item": rawItem,
		}
	default:
		return map[string]any{}
	}
}
