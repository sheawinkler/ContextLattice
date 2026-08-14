package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type storageGovernancePolicy struct {
	enabled              bool
	diskRoot             string
	warnUsedRatio        float64
	highUsedRatio        float64
	minFreeBytes         uint64
	retentionWarnFactor  float64
	retentionHighFactor  float64
	taskDBCompactEnabled bool
}

func loadStorageGovernancePolicy() storageGovernancePolicy {
	root := strings.TrimSpace(os.Getenv("ORCH_STORAGE_GOVERNANCE_DISK_ROOT"))
	if root == "" {
		root = "."
	}
	minFreeGB := envFloat("ORCH_STORAGE_GOVERNANCE_MIN_FREE_GB", 40)
	if minFreeGB < 0 {
		minFreeGB = 0
	}
	policy := storageGovernancePolicy{
		enabled:              envBool("ORCH_STORAGE_GOVERNANCE_ENABLED", true),
		diskRoot:             root,
		warnUsedRatio:        envFloat("ORCH_STORAGE_GOVERNANCE_WARN_USED_RATIO", 0.85),
		highUsedRatio:        envFloat("ORCH_STORAGE_GOVERNANCE_HIGH_USED_RATIO", 0.92),
		minFreeBytes:         uint64(minFreeGB * 1024 * 1024 * 1024),
		retentionWarnFactor:  envFloat("ORCH_STORAGE_GOVERNANCE_RETENTION_MULTIPLIER_WARN", 1.5),
		retentionHighFactor:  envFloat("ORCH_STORAGE_GOVERNANCE_RETENTION_MULTIPLIER_HIGH", 2.5),
		taskDBCompactEnabled: envBool("ORCH_STORAGE_GOVERNANCE_TASK_DB_COMPACT_ENABLED", true),
	}
	if policy.warnUsedRatio <= 0 || policy.warnUsedRatio >= 1 {
		policy.warnUsedRatio = 0.85
	}
	if policy.highUsedRatio <= 0 || policy.highUsedRatio >= 1 {
		policy.highUsedRatio = 0.92
	}
	if policy.highUsedRatio < policy.warnUsedRatio {
		policy.highUsedRatio = policy.warnUsedRatio
	}
	if policy.retentionWarnFactor < 1.0 {
		policy.retentionWarnFactor = 1.0
	}
	if policy.retentionHighFactor < policy.retentionWarnFactor {
		policy.retentionHighFactor = policy.retentionWarnFactor
	}
	return policy
}

func defaultTrackedPaths() map[string]string {
	dataDir := filepath.Join(".data", "orchestrator")
	return map[string]string{
		"task_db":                   resolveStoragePath("TASK_DB_PATH", filepath.Join(dataDir, "agent_tasks.db")),
		"topic_index":               resolveStoragePath("TOPIC_INDEX_PATH", filepath.Join(dataDir, "topic_index.json")),
		"memory_write_history":      resolveStoragePath("MEMORY_WRITE_HISTORY_PATH", filepath.Join(dataDir, "memory_write_history.ndjson")),
		"trading_history":           resolveStoragePath("TRADING_HISTORY_PATH", filepath.Join(dataDir, "trading_metrics.ndjson")),
		"strategy_history":          resolveStoragePath("STRATEGY_HISTORY_PATH", filepath.Join(dataDir, "strategy_metrics.ndjson")),
		"signal_history":            resolveStoragePath("SIGNAL_HISTORY_PATH", filepath.Join(dataDir, "solana_signals.ndjson")),
		"override_history":          resolveStoragePath("OVERRIDE_HISTORY_PATH", filepath.Join(dataDir, "solana_overrides.ndjson")),
		"recall_monitor":            resolveStoragePath("RECALL_MONITOR_PATH", filepath.Join(dataDir, "recall_monitor.ndjson")),
		"memory_bank_cleanup_state": resolveStoragePath("ORCH_MEMORY_BANK_TELEMETRY_CLEANUP_STATE_PATH", filepath.Join(dataDir, "memory_bank_telemetry_cleanup_state.json")),
		"fanout_payload_blobs":      resolveStoragePath("FANOUT_OUTBOX_PAYLOAD_BLOB_DIR", filepath.Join(dataDir, "fanout_payload_blobs")),
		"mongo_raw_content_blobs":   resolveStoragePath("ORCH_MONGO_RAW_CONTENT_BLOB_DIR", filepath.Join(dataDir, "mongo_raw_content_blobs")),
		"continuation_outbox":       resolveStoragePath("GO_RETRIEVAL_CONTINUATION_DURABLE_DIR", filepath.Join(dataDir, "continuation_outbox")),
	}
}

func sourceLaneRow(s *server, source string) map[string]any {
	status, owner, detail := s.strictRuntimeLaneStatus(source)
	return map[string]any{
		"source": source,
		"status": status,
		"owner":  owner,
		"detail": detail,
	}
}

func memoryTopologyCluster(id string, role string, active []string, available []string, partitionKeys []string, notes []string) map[string]any {
	return map[string]any{
		"id":               id,
		"role":             role,
		"active_sources":   active,
		"available_fabric": available,
		"partition_keys":   partitionKeys,
		"notes":            notes,
	}
}

func memoryTopologyPolicyPayload(s *server, memoryPolicy memoryStorePolicy) map[string]any {
	rustBackendPolicy := defaultRustBackendPolicy()
	baseDefaultSources := []string{sourceTopicRollup, sourceQdrant}
	coreBoundarySurfaces := []string{
		"context_pack",
		"preflight",
		"policy_context_package",
		"memory_writes",
		"checkpoints",
		"review",
		"agent_guidance",
		"memory_edges",
		"graph_neighbors",
		"topic_rollups",
	}
	localInferenceConnectors := []string{"mlx", "llama-cpp", "ollama", "lmstudio", "vllm", "openai-compatible"}
	onboardingConnectors := []string{"obsidian_import_export", "source_backfill", "browser_context_ingest"}
	writePartitionKeys := []string{
		"project",
		"topic_path",
		"session_id",
		"agent_id",
		"data_class",
		"lifecycle",
		"content_hash",
		"object_id",
		"created_at",
		"horizon_days",
	}
	sourceLanes := []map[string]any{
		sourceLaneRow(s, sourceTopicRollup),
		sourceLaneRow(s, sourceQdrant),
		sourceLaneRow(s, sourceWeaviate),
		sourceLaneRow(s, sourcePgvector),
		sourceLaneRow(s, sourceMongoRaw),
		sourceLaneRow(s, sourceLetta),
		sourceLaneRow(s, sourceMemoryBank),
		sourceLaneRow(s, sourceMindsdb),
	}
	return map[string]any{
		"schema_id":             "contextlattice_memory_topology.v1",
		"default_app_profile":   "base_default",
		"base_default_hot_path": baseDefaultSources,
		"active_retrieval_policy": map[string]any{
			"default_sources":                s.retrieval.defaultSources,
			"fast_sources":                   s.retrieval.fastSources,
			"slow_sources":                   s.retrieval.slowSources,
			"sync_fallback_sources":          s.retrieval.syncFallbackSources,
			"fail_open_continuation_sources": mapKeysSorted(s.retrieval.failOpenContinuationSources),
			"rust_quality_fallback_sources":  s.retrieval.rustQualityFallbackSources,
			"rust_backend_policy":            rustBackendPolicy,
			"staged_fetch_enabled":           s.retrieval.enabled,
			"deep_blocking":                  s.retrieval.deepBlocking,
		},
		"partitioning": map[string]any{
			"default_write_partition_keys": writePartitionKeys,
			"default_read_cluster_keys": []string{
				"retrieval_mode",
				"retrieval_intent",
				"topic_prefix",
				"session_rollup",
				"memory_edge_relation",
				"source_lane",
				"traffic_class",
			},
			"topic_tree": map[string]any{
				"strategy":         "prefix rollups over topic_path",
				"history_index":    memoryPolicy.rollupUseHistoryIndex,
				"cache_ttl_secs":   memoryPolicy.rollupCacheTTL.Seconds(),
				"hot_horizon_days": envInt("GO_MEMORY_STORE_HOT_INDEX_MAX_AGE_DAYS", 0),
			},
			"graph_edges": map[string]any{
				"strategy":           "bounded local edge log with neighbor expansion",
				"edge_store_ref":     ownerOnlyStoreRef("memory_edges"),
				"max_edges":          memoryPolicy.maxEdges,
				"max_edge_neighbors": memoryPolicy.maxEdgeNeighbors,
				"relations":          []string{"same_topic", "references", "same_session", "same_agent", "inferred_related"},
			},
			"content_addressing": map[string]any{
				"enabled":           memoryPolicy.contentAddressed,
				"content_store_ref": ownerOnlyStoreRef("memory_content_blobs"),
				"content_link_mode": memoryPolicy.contentLinkMode,
				"history_store_ref": ownerOnlyStoreRef("memory_history"),
			},
		},
		"clusters": []map[string]any{
			memoryTopologyCluster(
				"base_default",
				"lowest-overhead local agent recall path",
				baseDefaultSources,
				[]string{sourceTopicRollup, sourceQdrant, sourceMongoRaw},
				[]string{"project", "topic_path", "created_at", "content_hash"},
				[]string{"This is the base app default, not the full backend fabric."},
			),
			memoryTopologyCluster(
				"vector_semantic",
				"parallel semantic recall and vector fanout",
				[]string{sourceQdrant},
				[]string{sourceQdrant, sourceWeaviate, sourcePgvector},
				[]string{"project", "topic_path", "content_hash", "embedding_dimension"},
				[]string{"Qdrant is first-class by default; pgvector and Weaviate remain first-class full/operator lanes when enabled."},
			),
			memoryTopologyCluster(
				"raw_audit",
				"durable write truth, raw telemetry, and cold backpointers",
				[]string{sourceMongoRaw},
				[]string{sourceMongoRaw, "memory_write_history", "content_blobs", "telemetry_spool"},
				[]string{"project", "session_id", "agent_id", "data_class", "created_at"},
				[]string{"Raw lanes stay separate from compact prompt surfaces so bounded context stays clean."},
			),
			memoryTopologyCluster(
				"graph_relationships",
				"typed relationships for neighbor recall and shared-memory interpretation",
				[]string{"memory_edges"},
				[]string{"memory_edges", "neighbors", "same_topic", "same_session", "references", "inferred_related"},
				[]string{"source_id", "target_id", "relation", "project", "topic_path"},
				[]string{"Graph edges are bounded and local; they add relational structure without requiring a heavyweight graph database."},
			),
			memoryTopologyCluster(
				"deep_recall",
				"slower recall lanes that enrich work without blocking the first answer",
				[]string{sourceLetta, sourceMemoryBank},
				[]string{sourceLetta, sourceMemoryBank, sourceMindsdb, "mcp_hub"},
				[]string{"project", "agent_id", "session_id", "topic_path"},
				[]string{"Deep lanes should normally warm asynchronously or run under explicit deep mode."},
			),
			memoryTopologyCluster(
				"lexical_acceleration",
				"Rust and search-index accelerators for memory-bank breadth",
				[]string{"memory-bank-spike-rs"},
				[]string{"memory-bank-spike-rs", "meilisearch", "quickwit_spike", "tantivy_lexical", "lancedb_spike", "trieve_spike", "helixdb_spike", "icm_spike", "shodh_spike", "memvid_spike", "surrealdb_spike"},
				[]string{"project", "topic_path", "file", "term_posting"},
				[]string{"Spike/adaptor lanes stay profile-gated unless promoted by measured quality and tail-latency evidence."},
			),
			memoryTopologyCluster(
				"agent_runtime",
				"coordination state for sessions, handoffs, prompt packages, and Skills Index discovery",
				[]string{"agent_sessions", "skills_index"},
				[]string{"agent_sessions", "agent_contracts", "skills_index", "context_packages", "compaction_handoffs"},
				[]string{"session_id", "agent_id", "project", "objective", "topic_path"},
				[]string{"This cluster lets any agent repackage prior work into a bounded reference packet."},
			),
			memoryTopologyCluster(
				"inference_support",
				"local model/runtime selection for synthesis, dream mode, and embeddings",
				[]string{"fastembed"},
				[]string{"fastembed", "mlx", "llama-cpp", "ollama", "lmstudio", "vllm", "openai_compatible"},
				[]string{"runtime", "model", "capability", "resource_class"},
				[]string{"Inference lanes support synthesis; they are not memory truth stores."},
			),
			memoryTopologyCluster(
				"observability",
				"health, quality, retention, and trace surfaces",
				[]string{"storage_governance", "retrieval_telemetry"},
				[]string{"storage_governance", "retrieval_telemetry", "memory_graph_quality", "langfuse", "storage_ledger"},
				[]string{"service", "source", "project", "captured_at"},
				[]string{"Observability is intentionally optional in local runs when disk pressure matters."},
			),
		},
		"deployment_profiles": map[string]any{
			"hosted_lite": map[string]any{
				"hot_path":          []string{sourceTopicRollup},
				"core_surfaces":     coreBoundarySurfaces,
				"excluded_services": []string{sourceQdrant, sourcePgvector, sourceMemoryBank, sourceLetta, "cozo", "observability", "local_llm_runtimes"},
				"notes": []string{
					"Single-container/container-constrained surfaces keep the retrieval contract small and avoid nested service assumptions.",
					"Memory edges remain core when backed by the gateway memory store rather than an external graph database.",
				},
			},
			"local_lite": map[string]any{
				"hot_path":                          baseDefaultSources,
				"default_sources":                   []string{sourceQdrant, sourceMongoRaw, sourceTopicRollup},
				"core_surfaces":                     append(append([]string{}, coreBoundarySurfaces...), sourceQdrant, sourceMongoRaw, "fastembed"),
				"connector_surfaces":                onboardingConnectors,
				"connector_only_inference_runtimes": localInferenceConnectors,
				"excluded_default_sources":          []string{sourcePgvector, sourceMemoryBank, sourceLetta, sourceMindsdb, "cozo", "observability", "premium_behavior_pack"},
				"optional":                          []string{sourceMemoryBank, "meilisearch", "spike_adapters"},
			},
			"full": map[string]any{
				"hot_path":                          []string{sourceTopicRollup, sourceQdrant, sourcePgvector},
				"default_sources":                   []string{sourceQdrant, sourcePgvector, sourceMongoRaw, sourceTopicRollup, sourceMemoryBank, "meilisearch"},
				"core_surfaces":                     append(append([]string{}, coreBoundarySurfaces...), sourceQdrant, sourcePgvector, sourceMongoRaw, "fastembed"),
				"connector_surfaces":                append(append([]string{}, onboardingConnectors...), sourceLetta, sourceWeaviate, "honcho_style_external_memory", "mcp_qdrant"),
				"connector_only_inference_runtimes": localInferenceConnectors,
				"deep":                              []string{sourceMongoRaw, sourceLetta, sourceMemoryBank, sourceMindsdb},
				"optional":                          []string{"observability", "local_llm_runtimes", "adapter_lab"},
			},
			"paid_local": map[string]any{
				"hot_path":         []string{sourceTopicRollup, sourceQdrant, sourcePgvector},
				"default_sources":  []string{sourceQdrant, sourcePgvector, sourceMongoRaw, sourceTopicRollup, sourceMemoryBank, "meilisearch"},
				"core_surfaces":    append(append([]string{}, coreBoundarySurfaces...), sourceQdrant, sourcePgvector, sourceMongoRaw, "fastembed", "premium_policy_packs"),
				"premium_surfaces": []string{"premium_behavior_pack", "premium_runtime_policy", "premium_policy_packs", "operator_runbooks", "paid_entitlement_gates"},
				"agent_policy": map[string]any{
					"premium_behavior_required": true,
					"policy_mode":               "paid_runtime_policy",
					"applies_to":                []string{"codex", "claude-code", "gemini-cli", "opencode", "shell-env", "contextlattice-hooks-env"},
					"public_contents":           false,
				},
				"connector_surfaces":                append(append([]string{}, onboardingConnectors...), sourceLetta, sourceWeaviate, "honcho_style_external_memory", "mcp_qdrant"),
				"connector_only_inference_runtimes": localInferenceConnectors,
				"deep":                              []string{sourceMongoRaw, sourceLetta, sourceMemoryBank, sourceMindsdb},
				"optional":                          []string{"observability", "local_llm_runtimes", "adapter_lab"},
			},
			"ultra_dev": map[string]any{
				"hot_path":      []string{sourceTopicRollup, sourceQdrant, sourceWeaviate, sourcePgvector},
				"core_surfaces": append(append([]string{}, coreBoundarySurfaces...), sourceQdrant, sourcePgvector, sourceMongoRaw, "fastembed", "premium_policy_packs", "premium_behavior_pack"),
				"deep":          []string{sourceMongoRaw, sourceLetta, sourceMemoryBank, sourceMindsdb},
				"lab":           []string{"lancedb_spike", "trieve_spike", "helixdb_spike", "icm_spike", "shodh_spike", "memvid_spike", "surrealdb_spike"},
			},
		},
		"source_lanes": sourceLanes,
		"recommendation": map[string]any{
			"default":    "Keep topic_rollups + qdrant as the base default app path.",
			"lite":       "Keep memory edges core, but treat llama.cpp and other local LLM runtimes as connector-only in Lite.",
			"full":       "Use pgvector as a first-class Full/Paid vector lane beside Qdrant; keep Weaviate, Letta, and Honcho-style providers as connector lanes unless explicitly configured.",
			"paid":       "Keep paid behavior policy behind entitlement boundaries without publishing non-public implementation details into public Lite.",
			"connectors": "Use Obsidian as an import/export onboarding connector, not as a replacement for the native dashboard or memory store.",
			"agents":     "Agents should consume the CLI or HTTP context-package surface instead of choosing stores directly.",
		},
	}
}

func fileOrDirSize(path string, maxFiles int) (int64, bool, error) {
	if strings.TrimSpace(path) == "" {
		return 0, false, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if !info.IsDir() {
		return info.Size(), false, nil
	}
	total := int64(0)
	scanned := 0
	truncated := false
	err = filepath.WalkDir(path, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		scanned += 1
		if maxFiles > 0 && scanned > maxFiles {
			truncated = true
			return filepath.SkipDir
		}
		item, statErr := entry.Info()
		if statErr != nil {
			return nil
		}
		total += item.Size()
		return nil
	})
	if err != nil && !errors.Is(err, filepath.SkipDir) {
		return total, truncated, err
	}
	return total, truncated, nil
}

func collectTrackedStorage(paths map[string]string, maxFiles int) map[string]any {
	rows := map[string]any{}
	total := int64(0)
	for key, path := range paths {
		sizeBytes, truncated, err := fileOrDirSize(path, maxFiles)
		exists := false
		if strings.TrimSpace(path) != "" {
			if _, statErr := os.Stat(path); statErr == nil {
				exists = true
			}
		}
		row := map[string]any{
			"artifact_ref": ownerOnlyStoreRef("tracked_" + key),
			"bytes":        sizeBytes,
			"bytesHuman":   humanizeBytes(sizeBytes),
			"exists":       exists,
		}
		if truncated {
			row["truncated"] = true
		}
		if err != nil {
			row["error"] = "storage_stat_error"
		}
		rows[key] = row
		if sizeBytes > 0 {
			total += sizeBytes
		}
	}
	rows["_total"] = map[string]any{
		"bytes":      total,
		"bytesHuman": humanizeBytes(total),
	}
	return rows
}

func diskUsageSnapshot(root string) (map[string]any, error) {
	cleanRoot := strings.TrimSpace(root)
	if cleanRoot == "" {
		cleanRoot = "."
	}
	total, free, storageDriver, err := diskUsageBytes(cleanRoot)
	if err != nil {
		return nil, err
	}
	used := total - free
	usedRatio := 0.0
	if total > 0 {
		usedRatio = float64(used) / float64(total)
	}
	return map[string]any{
		"totalBytes":    total,
		"freeBytes":     free,
		"usedBytes":     used,
		"usedRatio":     usedRatio,
		"totalHuman":    humanizeBytes(int64(total)),
		"freeHuman":     humanizeBytes(int64(free)),
		"usedHuman":     humanizeBytes(int64(used)),
		"capturedAt":    time.Now().UTC().Format(time.RFC3339),
		"platform":      runtimePlatform(),
		"storageDriver": storageDriver,
	}, nil
}

func pressureBand(usedRatio float64, freeBytes uint64, policy storageGovernancePolicy) string {
	if usedRatio >= policy.highUsedRatio {
		return "high"
	}
	if policy.minFreeBytes > 0 && freeBytes <= policy.minFreeBytes {
		return "high"
	}
	if usedRatio >= policy.warnUsedRatio {
		return "warn"
	}
	return "healthy"
}

func runtimePlatform() string {
	return runtime.GOOS
}

func humanizeBytes(value int64) string {
	if value < 0 {
		return "0 B"
	}
	units := []string{"B", "KB", "MB", "GB", "TB"}
	size := float64(value)
	unit := 0
	for size >= 1024.0 && unit < len(units)-1 {
		size /= 1024.0
		unit += 1
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", int64(size), units[unit])
	}
	return fmt.Sprintf("%.2f %s", size, units[unit])
}

func defaultStorageLedgerPath() string {
	return resolveStoragePath("ORCH_STORAGE_LEDGER_PATH", filepath.Join(".data", "orchestrator", "storage_ledger.ndjson"))
}

func parseStorageLedgerTime(raw string) (time.Time, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return ts.UTC(), true
	}
	if ts, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return ts.UTC(), true
	}
	return time.Time{}, false
}

func readStorageLedgerEntries(path string, limit int, since *time.Time, maxLineBytes int) ([]map[string]any, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	rows := make([]map[string]any, 0, limit)
	parseErrors := 0
	scanner := bufio.NewScanner(file)
	if maxLineBytes < 64*1024 {
		maxLineBytes = 64 * 1024
	}
	scanner.Buffer(make([]byte, 0, 128*1024), maxLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		row := map[string]any{}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			parseErrors += 1
			continue
		}
		if since != nil {
			capturedRaw := anyToString(row["captured_at"])
			if capturedRaw == "" {
				capturedRaw = anyToString(row["timestamp"])
			}
			capturedAt, ok := parseStorageLedgerTime(capturedRaw)
			if !ok || capturedAt.Before(*since) {
				continue
			}
		}
		if len(rows) < limit {
			rows = append(rows, row)
			continue
		}
		copy(rows, rows[1:])
		rows[len(rows)-1] = row
	}
	if err := scanner.Err(); err != nil {
		return rows, parseErrors, err
	}
	return rows, parseErrors, nil
}

func (s *server) storageTelemetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}
	policy := loadStorageGovernancePolicy()
	disk, diskErr := diskUsageSnapshot(policy.diskRoot)
	diskStatus := "ok"
	if diskErr != nil {
		disk = map[string]any{"error": "storage_stat_error"}
		diskStatus = "error"
	}
	pressure := "unknown"
	if diskErr == nil {
		pressure = pressureBand(
			anyToFloat64(disk["usedRatio"], 0.0),
			uint64(anyToInt64(disk["freeBytes"], 0)),
			policy,
		)
	}
	telemetrySummary := map[string]any{"enabled": false}
	if s.telemetrySink != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		summary, err := s.telemetrySink.summary(ctx)
		if err != nil {
			log.Printf("storage telemetry sink summary failed: %v", err)
			telemetrySummary = map[string]any{"enabled": s.telemetrySink.enabled, "error": "telemetry_summary_error"}
		} else {
			telemetrySummary = summary
		}
	}
	memoryPolicy := loadMemoryStorePolicy()
	tracked := collectTrackedStorage(defaultTrackedPaths(), 200000)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"capturedAt": time.Now().UTC().Format(time.RFC3339),
		"storageGovernance": map[string]any{
			"enabled":                 policy.enabled,
			"warnUsedRatio":           policy.warnUsedRatio,
			"highUsedRatio":           policy.highUsedRatio,
			"minFreeBytes":            policy.minFreeBytes,
			"retentionWarnMultiplier": policy.retentionWarnFactor,
			"retentionHighMultiplier": policy.retentionHighFactor,
			"taskDbCompactEnabled":    policy.taskDBCompactEnabled,
			"pressureBand":            pressure,
		},
		"dataClasses": map[string]any{
			"learning_memory": map[string]any{
				"retention":      "indefinite",
				"storage":        "go_memory_store + content_blobs",
				"deletionPolicy": "never_auto_delete",
			},
			"rollups": map[string]any{
				"retention":      "indefinite",
				"storage":        "topic rollup graph + history index",
				"deletionPolicy": "never_auto_delete",
			},
			"telemetry": map[string]any{
				"retention_days_hot": envInt("GO_TELEMETRY_RETENTION_DAYS", 75),
				"storage":            "mongo telemetry + compressed blobs",
				"cold_policy":        "content-addressed compressed blobs + pointer refs",
			},
			"ephemeral_state": map[string]any{
				"retention_days_hot": envInt("GO_TELEMETRY_RETENTION_DAYS", 75),
				"storage":            "telemetry lane (isolated)",
				"cold_policy":        "compressed blob refs",
			},
		},
		"memoryTopology":   memoryTopologyPolicyPayload(s, memoryPolicy),
		"gatewayState":     gatewayStateInventoryPayload(),
		"disk":             disk,
		"diskStatus":       diskStatus,
		"trackedArtifacts": tracked,
		"telemetrySink":    telemetrySummary,
	})
}

func (s *server) storageTelemetryLedger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if _, ok := s.prepareAuthorizedHeaders(w, r); !ok {
		return
	}

	ledgerPath := defaultStorageLedgerPath()
	defaultLimit := clampInt(envInt("ORCH_STORAGE_LEDGER_READ_LIMIT_DEFAULT", 168), 1, 5000)
	maxLimit := envInt("ORCH_STORAGE_LEDGER_READ_LIMIT_MAX", 5000)
	if maxLimit < 1 {
		maxLimit = 5000
	}
	maxLimit = clampInt(maxLimit, 1, 20000)
	maxLineBytes := envInt("ORCH_STORAGE_LEDGER_LINE_MAX_BYTES", 2*1024*1024)

	limit := defaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid limit query param"})
			return
		}
		limit = parsed
	}
	limit = clampInt(limit, 1, maxLimit)

	sinceRaw := strings.TrimSpace(r.URL.Query().Get("since"))
	var since *time.Time
	if sinceRaw != "" {
		parsed, ok := parseStorageLedgerTime(sinceRaw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid since query param; expected RFC3339 timestamp"})
			return
		}
		since = &parsed
	}

	rows, parseErrors, err := readStorageLedgerEntries(ledgerPath, limit, since, maxLineBytes)
	exists := true
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			exists = false
			rows = []map[string]any{}
		} else {
			log.Printf("storage ledger read failed: %v", err)
			writeJSON(w, http.StatusOK, map[string]any{
				"ok":         false,
				"capturedAt": time.Now().UTC().Format(time.RFC3339),
				"store_ref":  ownerOnlyStoreRef("storage_ledger"),
				"exists":     true,
				"error":      "storage_read_error",
				"count":      0,
				"rows":       []map[string]any{},
			})
			return
		}
	}

	payload := map[string]any{
		"ok":         true,
		"capturedAt": time.Now().UTC().Format(time.RFC3339),
		"store_ref":  ownerOnlyStoreRef("storage_ledger"),
		"exists":     exists,
		"count":      len(rows),
		"limit":      limit,
		"since":      sinceRaw,
		"rows":       rows,
	}
	if parseErrors > 0 {
		payload["parseErrors"] = parseErrors
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *server) storageMaintenanceRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if !s.writeAuthorizedRequest(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Invalid API key"})
		return
	}

	policy := loadStorageGovernancePolicy()
	beforeDisk, beforeErr := diskUsageSnapshot(policy.diskRoot)
	taskResults := map[string]any{}
	errorsList := []string{}

	if s.telemetrySink == nil || !s.telemetrySink.enabled {
		taskResults["telemetry_blob_gc"] = map[string]any{
			"enabled": false,
			"skipped": true,
			"reason":  "telemetry sink disabled",
		}
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		result, err := s.telemetrySink.runBlobGCOnce(ctx)
		if err != nil {
			taskResults["telemetry_blob_gc"] = map[string]any{"enabled": true, "ok": false, "error": err.Error()}
			errorsList = append(errorsList, "telemetry_blob_gc: "+err.Error())
		} else {
			taskResults["telemetry_blob_gc"] = map[string]any{"enabled": true, "ok": true, "result": result}
		}
	}
	afterDisk, afterErr := diskUsageSnapshot(policy.diskRoot)
	if beforeErr != nil {
		errorsList = append(errorsList, "disk_before: "+beforeErr.Error())
	}
	if afterErr != nil {
		errorsList = append(errorsList, "disk_after: "+afterErr.Error())
	}

	ok := len(errorsList) == 0
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          ok,
		"performedAt": time.Now().UTC().Format(time.RFC3339),
		"pressureBand": func() string {
			if afterErr == nil {
				return pressureBand(anyToFloat64(afterDisk["usedRatio"], 0.0), uint64(anyToInt64(afterDisk["freeBytes"], 0)), policy)
			}
			if beforeErr == nil {
				return pressureBand(anyToFloat64(beforeDisk["usedRatio"], 0.0), uint64(anyToInt64(beforeDisk["freeBytes"], 0)), policy)
			}
			return "unknown"
		}(),
		"disk": map[string]any{
			"before": beforeDisk,
			"after":  afterDisk,
		},
		"tasks":            taskResults,
		"trackedArtifacts": collectTrackedStorage(defaultTrackedPaths(), 200000),
		"errors":           errorsList,
	})
}

func anyToFloat64(value any, fallback float64) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case uint64:
		return float64(typed)
	case json.Number:
		parsed, err := typed.Float64()
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func anyToInt64(value any, fallback int64) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int32:
		return int64(typed)
	case int64:
		return typed
	case uint:
		return int64(typed)
	case uint32:
		return int64(typed)
	case uint64:
		if typed > uint64(^uint64(0)>>1) {
			return fallback
		}
		return int64(typed)
	case float64:
		return int64(typed)
	case float32:
		return int64(typed)
	default:
		return fallback
	}
}
