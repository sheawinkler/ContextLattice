package main

import (
	"log"
	"path/filepath"
	"strings"

	"github.com/contextlattice/gateway-go/internal/gatewaystate"
)

func resolveStoragePath(envName string, fallback string) string {
	envNames := []string{}
	if strings.TrimSpace(envName) != "" {
		envNames = append(envNames, envName)
	}
	return gatewaystate.ResolvePath(envNames, fallback).Path
}

func resolveStoragePathAliases(envNames []string, fallback string) string {
	return gatewaystate.ResolvePath(envNames, fallback).Path
}

func gatewayStateEntry(id, kind, persistenceClass string, envNames []string, fallback string) gatewaystate.EntryInput {
	resolved := gatewaystate.ResolvePath(envNames, fallback)
	return gatewaystate.EntryInput{
		ID: id, Path: resolved.Path, Source: resolved.Source, SourceEnv: resolved.SourceEnv,
		StorageTier: resolved.StorageTier, Kind: kind, PersistenceClass: persistenceClass,
	}
}

func gatewayStateInventoryEntries() []gatewaystate.EntryInput {
	entries := []gatewaystate.EntryInput{
		gatewayStateEntry("feedback_history", "file", "append_only_durable_file", []string{"FEEDBACK_HISTORY_PATH"}, defaultFeedbackHistoryRelPath),
		gatewayStateEntry("task_db", "file", "durable_file", []string{"TASK_DB_PATH"}, filepath.Join(".data", "orchestrator", "agent_tasks.db")),
		gatewayStateEntry("agent_sessions", "file", "durable_file", []string{"GO_AGENT_SESSIONS_PATH", "GO_AGENT_SESSION_LEDGER_PATH"}, defaultAgentSessionsPathRel),
		gatewayStateEntry("continuity_ledger", "file", "append_only_durable_file", []string{"CONTEXTLATTICE_CONTINUITY_LEDGER_PATH"}, defaultContinuityLedgerPathRel),
		gatewayStateEntry("continuation_outbox", "directory", "durable_queue", []string{"GO_RETRIEVAL_CONTINUATION_DURABLE_DIR"}, filepath.Join(".data", "orchestrator", "continuation_outbox")),
		gatewayStateEntry("agent_memory_profiles", "file", "durable_file", []string{"AGENT_MEMORY_PROFILE_PATH"}, filepath.Join("services", "orchestrator", "data", "agent_memory_profiles.json")),
		gatewayStateEntry("telemetry_spool", "file", "bounded_durable_spool", []string{"GO_TELEMETRY_SPOOL_PATH"}, filepath.Join("services", "orchestrator", "data", "telemetry_spool.ndjson")),
		gatewayStateEntry("recall_eval_cases", "file", "durable_file", []string{"ORCH_RECALL_EVAL_CASES_PATH"}, defaultRecallEvalCasesRelativePath),
		gatewayStateEntry("recall_monitor", "file", "append_only_durable_file", []string{"RECALL_MONITOR_PATH"}, filepath.Join(".data", "orchestrator", "recall_monitor.ndjson")),
		gatewayStateEntry("temporal_claims", "file", "append_only_durable_file", []string{"CONTEXTLATTICE_TEMPORAL_CLAIMS_PATH"}, filepath.Join(".data", "orchestrator", "temporal_claims.ndjson")),
		gatewayStateEntry("context_policy", "file", "append_only_durable_file", []string{"CONTEXTLATTICE_CONTEXT_POLICY_PATH"}, filepath.Join(".data", "orchestrator", "context_policy.ndjson")),
		gatewayStateEntry("context_passports", "file", "append_only_durable_file", []string{"CONTEXTLATTICE_CONTEXT_PASSPORT_PATH"}, filepath.Join(".data", "orchestrator", "context_passports.ndjson")),
		gatewayStateEntry("context_identity_keys", "file", "owner_only_durable_file", []string{"CONTEXTLATTICE_CONTEXT_IDENTITY_KEY_PATH"}, filepath.Join(".data", "orchestrator", "context_identity_keys.json")),
		gatewayStateEntry("context_mesh", "file", "owner_only_durable_file", []string{"CONTEXTLATTICE_CONTEXT_MESH_STATE_PATH"}, filepath.Join(".data", "orchestrator", "context_mesh_state.json")),
		gatewayStateEntry("skill_foundry", "file", "append_only_durable_file", []string{"CONTEXTLATTICE_SKILL_FOUNDRY_PATH"}, filepath.Join(".data", "orchestrator", "skill_foundry.ndjson")),
		gatewayStateEntry("runner_quality", "file", "append_only_durable_file", []string{"GO_RUNNER_QUALITY_LEDGER_PATH", "CONTEXTLATTICE_RUNNER_QUALITY_LEDGER_PATH", "CONTEXTLATTICE_RUNNER_QUALITY_LEDGER"}, filepath.Join(".data", "orchestrator", "runner_quality_ledger.ndjson")),
		gatewayStateEntry("token_impact", "file", "append_only_durable_file", []string{"GO_TOKEN_IMPACT_LEDGER_PATH"}, filepath.Join(".data", "orchestrator", "token_impact_ledger.ndjson")),
		gatewayStateEntry("context_pack_quality", "file", "append_only_durable_file", []string{"GO_CONTEXT_PACK_QUALITY_LEDGER_PATH"}, filepath.Join(".data", "orchestrator", "context_pack_quality_ledger.ndjson")),
		gatewayStateEntry("utility_ledger", "file", "append_only_durable_file", []string{"GO_UTILITY_LEDGER_PATH"}, filepath.Join(".data", "orchestrator", "utility_ledger.ndjson")),
		gatewayStateEntry("storage_ledger", "file", "append_only_durable_file", []string{"ORCH_STORAGE_LEDGER_PATH"}, filepath.Join(".data", "orchestrator", "storage_ledger.ndjson")),
		gatewayStateEntry("policy_laboratory", "file", "append_only_durable_file", []string{"CONTEXTLATTICE_FRONTIER_T5_LEDGER_PATH"}, filepath.Join(".data", "orchestrator", "frontier_t5_policy_lab.ndjson")),
		gatewayStateEntry("agent_fit", "file", "durable_file", []string{"CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_PATH"}, filepath.Join(".data", "orchestrator", "frontier_t6_agent_fit.json")),
		gatewayStateEntry("portable_continuation", "file", "durable_file", []string{"CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_STATE_PATH"}, filepath.Join(".data", "orchestrator", "frontier_t7_portable_continuation.json")),
		gatewayStateEntry("aggregate_signal", "file", "durable_file", []string{"CONTEXTLATTICE_AGGREGATE_SIGNAL_PATH"}, filepath.Join(".data", "orchestrator", "aggregate_signal_state.json")),
		gatewayStateEntry("topic_index", "file", "durable_file", []string{"TOPIC_INDEX_PATH"}, filepath.Join(".data", "orchestrator", "topic_index.json")),
		gatewayStateEntry("memory_write_history", "file", "append_only_durable_file", []string{"MEMORY_WRITE_HISTORY_PATH"}, filepath.Join(".data", "orchestrator", "memory_write_history.ndjson")),
		gatewayStateEntry("trading_history", "file", "append_only_durable_file", []string{"TRADING_HISTORY_PATH"}, filepath.Join(".data", "orchestrator", "trading_metrics.ndjson")),
		gatewayStateEntry("strategy_history", "file", "append_only_durable_file", []string{"STRATEGY_HISTORY_PATH"}, filepath.Join(".data", "orchestrator", "strategy_metrics.ndjson")),
		gatewayStateEntry("signal_history", "file", "append_only_durable_file", []string{"SIGNAL_HISTORY_PATH"}, filepath.Join(".data", "orchestrator", "solana_signals.ndjson")),
		gatewayStateEntry("override_history", "file", "append_only_durable_file", []string{"OVERRIDE_HISTORY_PATH"}, filepath.Join(".data", "orchestrator", "solana_overrides.ndjson")),
		gatewayStateEntry("memory_bank_cleanup_state", "file", "durable_file", []string{"ORCH_MEMORY_BANK_TELEMETRY_CLEANUP_STATE_PATH"}, filepath.Join(".data", "orchestrator", "memory_bank_telemetry_cleanup_state.json")),
		gatewayStateEntry("fanout_payload_blobs", "directory", "durable_blob_directory", []string{"FANOUT_OUTBOX_PAYLOAD_BLOB_DIR"}, filepath.Join(".data", "orchestrator", "fanout_payload_blobs")),
		gatewayStateEntry("mongo_raw_content_blobs", "directory", "durable_blob_directory", []string{"ORCH_MONGO_RAW_CONTENT_BLOB_DIR"}, filepath.Join(".data", "orchestrator", "mongo_raw_content_blobs")),
	}
	return entries
}

func gatewayStateInventoryPayload() map[string]any {
	return gatewaystate.Inventory(gatewayStateInventoryEntries())
}

func prepareGatewayStateRootForStartup() {
	root, err := gatewaystate.EnsureRoot()
	if err != nil {
		log.Printf("gateway-go state root unavailable: path=%q source=%s error=%v", root.Path, root.Source, err)
		return
	}
	inventory := gatewayStateInventoryPayload()
	log.Printf(
		"gateway-go state root ready: path=%q source=%s storage_tier=%s entries=%d inventory_ok=%t",
		root.Path,
		root.Source,
		root.StorageTier,
		anyToInt(inventory["entry_count"], 0),
		anyToBool(inventory["ok"]),
	)
}
