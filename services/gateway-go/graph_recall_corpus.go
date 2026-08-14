package main

// This file owns the closed graph-recall benchmark contract.  The ordinary
// saved-recall v3 case set remains a separate artifact and keeps its scoring
// and lifecycle unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	savedRecallGraphCorpusSchemaID                 = "saved_recall_graph_corpus.v1"
	savedRecallGraphCorpusVersion                  = 1
	savedRecallGraphCorpusOwner                    = "gateway-go"
	savedRecallGraphCorpusRelativePath             = ".data/orchestrator/recall_graph_corpus.json"
	savedRecallGraphCorpusMaxSourceDocs            = savedRecallEvalV3MaxSourceDocs
	savedRecallGraphCorpusDevelopmentCases         = 200
	savedRecallGraphCorpusHoldoutCases             = 100
	savedRecallGraphCorpusTotalCases               = savedRecallGraphCorpusDevelopmentCases + savedRecallGraphCorpusHoldoutCases
	savedRecallGraphCorpusPositiveCases            = 90
	savedRecallGraphCorpusHardNegatives            = 10
	savedRecallGraphCorpusDevelopmentPositives     = 180
	savedRecallGraphCorpusTotalPositiveCases       = savedRecallGraphCorpusDevelopmentPositives + savedRecallGraphCorpusPositiveCases
	savedRecallGraphCorpusRelationDevelopmentCases = savedRecallGraphCorpusDevelopmentPositives / 3
	savedRecallGraphCorpusRelationHoldoutCases     = savedRecallGraphCorpusPositiveCases / 3
	savedRecallGraphCorpusDevelopmentHardNegatives = savedRecallGraphCorpusDevelopmentCases - savedRecallGraphCorpusDevelopmentPositives
	savedRecallGraphCorpusTotalHardNegatives       = savedRecallGraphCorpusDevelopmentHardNegatives + savedRecallGraphCorpusHardNegatives
	savedRecallGraphCorpusMinProjects              = 5
	savedRecallGraphCorpusMinAgentFamilies         = 5
	savedRecallGraphCorpusMinSessions              = 20
	savedRecallGraphCorpusMinIncremental           = 30
	savedRecallGraphCorpusGraphRecallGate          = 0.90
	savedRecallGraphCorpusIncrementalGate          = 0.90
	savedRecallGraphCorpusNegativeGate             = 1.0
	savedRecallGraphCorpusMaxSnapshotEdges         = 200000
	savedRecallGraphIncrementalControlSchemaID     = "saved_recall_graph_incremental_control.v1"
	savedRecallGraphIncrementalControlVersion      = 1
	savedRecallGraphIncrementalControlAuthority    = "execute_retrieval_server"
)

var savedRecallGraphRelations = []string{"references", "same_session", "same_topic"}

// Aliases make the contract names discoverable to callers that use the
// shorter graph-recall terminology.
const (
	graphRecallCorpusSchemaID         = savedRecallGraphCorpusSchemaID
	graphRecallCorpusVersion          = savedRecallGraphCorpusVersion
	graphRecallCorpusDevelopmentCases = savedRecallGraphCorpusDevelopmentCases
	graphRecallCorpusHoldoutCases     = savedRecallGraphCorpusHoldoutCases
	graphRecallCorpusPositiveCases    = savedRecallGraphCorpusPositiveCases
	graphRecallCorpusHardNegatives    = savedRecallGraphCorpusHardNegatives
)

type savedRecallGraphCorpusConfig struct {
	Path           string
	SchemaID       string
	Version        any
	UpdatedAt      any
	Source         string
	Synthetic      bool
	Owner          string
	ProjectScope   string
	CaseSetDigest  string
	Manifest       map[string]any
	Snapshot       map[string]any
	Custody        map[string]any
	Cost           map[string]any
	DirectBaseline map[string]any
	Cases          []map[string]any
}

type graphRecallCorpusRecord struct {
	Project                   string
	TopicPath                 string
	AgentID                   string
	AgentFamily               string
	SessionID                 string
	SourceFamily              string
	SeedID                    string
	TargetID                  string
	SeedFile                  string
	TargetFile                string
	Relation                  string
	EdgeID                    string
	Confidence                float64
	SeedUpdatedAt             time.Time
	TargetUpdatedAt           time.Time
	TimeBucket                string
	LineageKey                string
	Query                     string
	HardNegative              bool
	NegativeProof             string
	ObservedRelations         []string
	NegativeSnapshotDigest    string
	NegativeAdjacencyComplete bool
	NegativeContentChecked    bool
	NegativeSemanticChecked   bool
	NegativeContentProof      string
	NegativeSemanticProof     string
	IncrementalControl        map[string]any
}

func resolveSavedRecallGraphCorpusPath() string {
	return resolveStoragePath("ORCH_RECALL_GRAPH_CORPUS_PATH", savedRecallGraphCorpusRelativePath)
}

func normalizeGraphCorpusRelation(value string) string {
	value = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(value, "-", "_")))
	for _, relation := range savedRecallGraphRelations {
		if value == relation {
			return relation
		}
	}
	return ""
}

func graphCorpusAgentFamily(agentID, sourceFamily string) string {
	value := strings.TrimSpace(strings.ToLower(agentID))
	if value == "" {
		value = strings.TrimSpace(strings.ToLower(sourceFamily))
	}
	if value == "" {
		return ""
	}
	// Families are intentionally stable and opaque in persisted diversity
	// metadata.  Explicit family suffixes are retained; otherwise the complete
	// bounded agent identity is the family, avoiding guessed provider labels.
	return value
}

func graphCorpusTimeBucket(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format("2006-01")
}

func graphCorpusLineageKey(project, agentFamily, sessionID, timeBucket, seedID, targetID, relation string) string {
	// The relation argument is retained for source compatibility with the
	// generator helpers, but split lineage is deliberately relation-independent:
	// the same seed/target pair must never cross topology or split boundaries.
	_ = relation
	return sha256Hex(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(project)),
		strings.ToLower(strings.TrimSpace(agentFamily)),
		strings.ToLower(strings.TrimSpace(sessionID)),
		strings.ToLower(strings.TrimSpace(timeBucket)),
		strings.ToLower(strings.TrimSpace(seedID)),
		strings.ToLower(strings.TrimSpace(targetID)),
	}, "\x00"))
}

func graphCorpusSplitScopeKey(project, agentFamily, sessionID, timeBucket string) string {
	return sha256Hex(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(project)),
		strings.ToLower(strings.TrimSpace(agentFamily)),
		strings.ToLower(strings.TrimSpace(sessionID)),
		strings.ToLower(strings.TrimSpace(timeBucket)),
	}, "\x00"))
}

func graphCorpusCaseSetDigest(cases []map[string]any) string {
	semanticCases := make([]map[string]any, 0, len(cases))
	for _, rawCase := range cases {
		row := cloneJSONMap(rawCase)
		control := cloneJSONMap(anyMap(row["incremental_control"]))
		if len(control) > 0 {
			delete(control, "captured_at")
			delete(control, "digest")
			visible := cloneJSONMap(anyMap(control["control_final_model_visible"]))
			if len(visible) > 0 {
				// The complete production response digest binds that composition
				// attempt, including temporal quality custody. The ordered evidence
				// digest below is the stable paired semantic identity.
				delete(visible, "production_response_digest")
				control["control_final_model_visible"] = visible
			}
			cost := cloneJSONMap(anyMap(control["cost_observability"]))
			if len(cost) > 0 {
				delete(cost, "captured_at")
				// The server receipt digest binds captured_at. Exclude that
				// temporal wrapper as well so a custody-time refresh does not
				// silently redefine the semantic benchmark case set.
				delete(cost, "digest")
				control["cost_observability"] = cost
			}
			row["incremental_control"] = control
		}
		semanticCases = append(semanticCases, row)
	}
	canonical, err := json.Marshal(semanticCases)
	if err != nil {
		return sha256Hex(savedRecallGraphCorpusSchemaID + ".empty")
	}
	return sha256Hex(string(canonical))
}

func graphCorpusCaseCustodyDigest(cases []map[string]any) string {
	canonical, err := json.Marshal(cases)
	if err != nil {
		return sha256Hex(savedRecallGraphCorpusSchemaID + ".custody.invalid")
	}
	return sha256Hex(string(canonical))
}

func graphCorpusSnapshotCaptureDigest(snapshot map[string]any) string {
	return "sha256:" + graphCorpusDigestMap(snapshot)
}

func graphCorpusCandidatesDigest(candidates []recallEvalSourceCandidate) string {
	rows := make([]map[string]any, 0, len(candidates))
	for _, candidate := range candidates {
		rows = append(rows, map[string]any{
			"project": candidate.doc.Project, "file": strings.Trim(candidate.doc.FileName, "/"),
			"topic_path": normalizeTopicPathLoose(candidate.doc.TopicPath), "summary": candidate.doc.Summary,
			"updated_at": anyTimeString(candidate.doc.UpdatedAt), "object_id": candidate.doc.ObjectID,
			"horizon": candidate.doc.Horizon, "score": candidate.doc.Score,
			"lifecycle": candidate.doc.Lifecycle, "storage_tier": candidate.doc.StorageTier,
			"agent_id": candidate.agentID, "session_id": candidate.sessionID,
			"source_family": candidate.sourceFamily, "created_at": anyTimeString(candidate.createdAt),
			"stable_key": candidate.stableKey,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return strings.Join([]string{anyToString(rows[i]["project"]), anyToString(rows[i]["file"]), anyToString(rows[i]["stable_key"])}, "\x00") < strings.Join([]string{anyToString(rows[j]["project"]), anyToString(rows[j]["file"]), anyToString(rows[j]["stable_key"])}, "\x00")
	})
	canonical, err := json.Marshal(rows)
	if err != nil {
		return sha256Hex(savedRecallGraphCorpusSchemaID + ".candidates.invalid")
	}
	return sha256Hex(string(canonical))
}

func graphCorpusSourceEdgeSnapshotDigest(candidates []recallEvalSourceCandidate, edges []memoryEdgeEntry) string {
	return "sha256:" + graphCorpusDigestMap(map[string]any{
		"schema_id":              "saved_recall_graph_source_edge_snapshot.v1",
		"version":                1,
		"source_snapshot_digest": "sha256:" + graphCorpusCandidatesDigest(candidates),
		"edge_snapshot_digest":   "sha256:" + graphCorpusEdgesDigest(edges),
	})
}

func graphRecallControlRequestProjection(request map[string]any) map[string]any {
	projection := map[string]any{}
	for _, key := range []string{
		"query", "limit", "project", "topic_path", "retrieval_mode", "retrieval_intent", "sources",
		"include_grounding", "include_retrieval_debug", "include_preferences", "rerank_with_learning", "user_id", "agent_id",
		"auto_escalate", "deep_async", "callback_url", "traffic_class",
	} {
		if value, present := request[key]; present {
			projection[key] = cloneJSONValue(value)
		}
	}
	return projection
}

func graphRecallControlRequestDigest(request map[string]any) string {
	return "sha256:" + graphCorpusDigestMap(graphRecallControlRequestProjection(request))
}

func graphRecallControlResponseDigest(response map[string]any) string {
	projection := map[string]any{
		"results":          parseRows(response["results"]),
		"grounding":        cloneJSONMap(anyMap(response["grounding"])),
		"retrieval_mode":   anyToString(response["retrieval_mode"]),
		"retrieval_intent": anyToString(response["retrieval_intent"]),
		"traffic_class":    anyToString(response["traffic_class"]),
	}
	return "sha256:" + graphCorpusDigestMap(projection)
}

func graphCorpusEdgesDigest(edges []memoryEdgeEntry) string {
	rows := make([]map[string]any, 0, len(edges))
	for _, edge := range edges {
		rows = append(rows, map[string]any{
			"edge_id": edge.EdgeID, "source_id": edge.SourceID, "target_id": edge.TargetID,
			"relation": normalizeGraphCorpusRelation(edge.Relation), "project": edge.Project,
			"topic_path": edge.TopicPath, "confidence": edge.Confidence,
			"provenance": cloneJSONMap(edge.Provenance), "metadata": cloneJSONMap(edge.Metadata),
			"agent_id": edge.AgentID, "session_id": edge.SessionID, "lifecycle": edge.Lifecycle,
			"created_at": edge.CreatedAt, "source": edge.Source,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left := strings.Join([]string{anyToString(rows[i]["edge_id"]), anyToString(rows[i]["source_id"]), anyToString(rows[i]["target_id"]), anyToString(rows[i]["relation"])}, "\x00")
		right := strings.Join([]string{anyToString(rows[j]["edge_id"]), anyToString(rows[j]["source_id"]), anyToString(rows[j]["target_id"]), anyToString(rows[j]["relation"])}, "\x00")
		return left < right
	})
	canonical, err := json.Marshal(rows)
	if err != nil {
		return sha256Hex(savedRecallGraphCorpusSchemaID + ".edges.invalid")
	}
	return sha256Hex(string(canonical))
}

func graphCorpusDigestMap(value map[string]any, omit ...string) string {
	copyValue := cloneJSONMap(value)
	for _, key := range omit {
		delete(copyValue, key)
	}
	canonical, err := json.Marshal(copyValue)
	if err != nil {
		return sha256Hex(savedRecallGraphCorpusSchemaID + ".invalid")
	}
	return sha256Hex(string(canonical))
}

func graphCorpusSortedStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func graphCorpusInsufficiencyReceipt(source string, population, required map[string]int, availableRelations map[string]int, detail string) map[string]any {
	return map[string]any{
		"schema_id":             "saved_recall_graph_corpus_insufficiency.v1",
		"status":                "insufficient_source_population",
		"benchmark_eligible":    false,
		"source":                source,
		"detail":                detail,
		"population":            population,
		"required":              required,
		"available_relations":   availableRelations,
		"fabricated_cases":      0,
		"raw_store_scanned":     false,
		"source_scope_enforced": true,
	}
}

func graphCorpusRecordSortKey(record graphRecallCorpusRecord) string {
	return strings.Join([]string{
		record.Relation, record.Project, record.AgentFamily, record.SessionID,
		record.TimeBucket, record.SeedID, record.TargetID, record.EdgeID,
	}, "\x00")
}

func graphCorpusRecordCaseID(record graphRecallCorpusRecord, split string, ordinal int) string {
	return "graph-" + sha256Hex(strings.Join([]string{
		savedRecallGraphCorpusSchemaID, split, record.Relation, record.LineageKey,
		record.SeedID, record.TargetID, fmt.Sprintf("%d", ordinal),
	}, "\x00"))[:24]
}

func graphCorpusIncrementalControlKey(record graphRecallCorpusRecord) string {
	return strings.TrimSpace(record.LineageKey)
}

func graphCorpusIncrementalControlsFromStats(sourceStats map[string]any) map[string]map[string]any {
	controls := map[string]map[string]any{}
	for key, raw := range anyMap(sourceStats["incremental_controls"]) {
		key = strings.TrimSpace(key)
		control := cloneJSONMap(anyMap(raw))
		if key == "" || len(control) == 0 {
			continue
		}
		controls[key] = control
	}
	return controls
}

func graphCorpusIncrementalControlValid(control map[string]any, record graphRecallCorpusRecord, snapshotDigest, sourceSnapshotDigest, edgeSnapshotDigest string) bool {
	if len(control) == 0 || anyToString(control["schema_id"]) != savedRecallGraphIncrementalControlSchemaID || anyToInt(control["version"], 0) != savedRecallGraphIncrementalControlVersion || anyToString(control["authority"]) != savedRecallGraphIncrementalControlAuthority {
		return false
	}
	if !anyToBool(control["graph_influence_disabled"]) || anyToBool(control["graph_backend_consulted"]) || anyToBool(control["graph_results_used"]) || anyToBool(control["candidate_allocation_active"]) || anyToBool(control["treatment_active"]) || anyToString(control["traffic_class"]) != "evaluation_holdout" {
		return false
	}
	if _, present := control["target_direct_hit"]; !present {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(anyToString(control["seed_target_lineage"])), strings.TrimSpace(record.LineageKey)) || strings.TrimSpace(anyToString(control["seed_memory_id"])) == "" || strings.TrimSpace(anyToString(control["target_memory_id"])) == "" || !graphRecallMemoryIDEqual(anyToString(control["seed_memory_id"]), record.SeedID) || !graphRecallMemoryIDEqual(anyToString(control["target_memory_id"]), record.TargetID) {
		return false
	}
	if anyToInt(control["control_k"], 0) != defaultRecallEvalK || strings.TrimSpace(anyToString(control["control_request_digest"])) == "" || strings.TrimSpace(anyToString(control["control_response_digest"])) == "" || anyToString(control["control_composition_path"]) != "production_final_response_seam_graph_disabled" || !graphRecallFinalModelVisibleProjectionValid(anyMap(control["control_final_model_visible"])) {
		return false
	}
	if anyToBool(control["target_direct_hit"]) != graphRecallFinalModelVisibleContains(anyMap(control["control_final_model_visible"]), record.TargetID, map[string]struct{}{strings.Trim(record.TargetFile, "/"): {}}) {
		return false
	}
	if _, ok := graphRecallControlLatencyValid(control); !ok {
		return false
	}
	if strings.TrimSpace(snapshotDigest) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(control["control_snapshot_digest"])), strings.TrimSpace(snapshotDigest)) || !strings.EqualFold(strings.TrimSpace(anyToString(control["source_snapshot_digest"])), strings.TrimSpace(sourceSnapshotDigest)) || !strings.EqualFold(strings.TrimSpace(anyToString(control["edge_snapshot_digest"])), strings.TrimSpace(edgeSnapshotDigest)) {
		return false
	}
	if !graphRecallEconomicsObservationComplete(map[string]any{"cost_observability": cloneJSONMap(anyMap(control["cost_observability"]))}) {
		return false
	}
	if strings.TrimSpace(anyToString(control["case_snapshot_digest"])) != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(anyToString(control["case_snapshot_digest"]))), "sha256:") {
		return false
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(control["captured_at"]))); err != nil || capturedAt.IsZero() {
		return false
	}
	identity := anyMap(control["source_runtime_identity"])
	currentIdentity := contextLatticeBuildIdentity()
	if anyToBool(currentIdentity["source_bound"]) && (!anyToBool(identity["source_bound"]) || !strings.EqualFold(anyToString(identity["source_commit"]), anyToString(currentIdentity["source_commit"])) || !strings.EqualFold(anyToString(identity["source_tree"]), anyToString(currentIdentity["source_tree"]))) {
		return false
	}
	return true
}

func graphCorpusBindIncrementalControls(records []graphRecallCorpusRecord, sourceStats map[string]any, snapshotDigest, sourceSnapshotDigest, edgeSnapshotDigest string) ([]graphRecallCorpusRecord, string) {
	controls := graphCorpusIncrementalControlsFromStats(sourceStats)
	if len(controls) == 0 {
		return nil, "sealed graph-disabled incremental control receipt is missing"
	}
	bound := make([]graphRecallCorpusRecord, 0, len(records))
	for _, record := range records {
		key := graphCorpusIncrementalControlKey(record)
		control := cloneJSONMap(controls[key])
		if len(control) == 0 {
			bound = append(bound, record)
			continue
		}
		if !graphCorpusIncrementalControlValid(control, record, snapshotDigest, sourceSnapshotDigest, edgeSnapshotDigest) {
			return nil, "sealed graph-disabled incremental control receipt is missing or does not bind the exact seed-target lineage"
		}
		record.IncrementalControl = control
		bound = append(bound, record)
	}
	return bound, ""
}

func graphCorpusBindCaseIncrementalControl(row map[string]any, snapshotDigest string) {
	control := cloneJSONMap(anyMap(row["incremental_control"]))
	if len(control) == 0 {
		return
	}
	control["case_id"] = anyToString(row["id"])
	control["case_snapshot_digest"] = snapshotDigest
	control["case_binding"] = "case_id_and_seed_target_lineage"
	control["digest"] = "sha256:" + graphCorpusDigestMap(control, "digest")
	row["incremental_control"] = control
}

func graphCorpusIncrementalControlRowValid(row map[string]any, snapshot map[string]any) bool {
	control := anyMap(row["incremental_control"])
	if len(control) == 0 || anyToString(control["schema_id"]) != savedRecallGraphIncrementalControlSchemaID || anyToInt(control["version"], 0) != savedRecallGraphIncrementalControlVersion || anyToString(control["authority"]) != savedRecallGraphIncrementalControlAuthority {
		return false
	}
	if _, present := control["target_direct_hit"]; !present || !anyToBool(control["graph_influence_disabled"]) || anyToBool(control["graph_backend_consulted"]) || anyToBool(control["graph_results_used"]) || anyToBool(control["candidate_allocation_active"]) || anyToBool(control["treatment_active"]) || anyToString(control["traffic_class"]) != "evaluation_holdout" || anyToString(control["case_id"]) != anyToString(row["id"]) || anyToString(control["case_binding"]) != "case_id_and_seed_target_lineage" {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(anyToString(control["seed_target_lineage"])), strings.TrimSpace(anyToString(row["seed_target_lineage"]))) || !graphRecallMemoryIDEqual(anyToString(control["seed_memory_id"]), anyToString(row["seed_memory_id"])) || !graphRecallMemoryIDEqual(anyToString(control["target_memory_id"]), anyToString(row["target_memory_id"])) {
		return false
	}
	if anyToInt(control["control_k"], 0) != defaultRecallEvalK || !strings.EqualFold(strings.TrimSpace(anyToString(control["control_snapshot_digest"])), strings.TrimSpace(anyToString(snapshot["source_edge_snapshot_digest"]))) || !strings.EqualFold(strings.TrimSpace(anyToString(control["source_snapshot_digest"])), strings.TrimSpace(anyToString(snapshot["source_snapshot_digest"]))) || !strings.EqualFold(strings.TrimSpace(anyToString(control["edge_snapshot_digest"])), strings.TrimSpace(anyToString(snapshot["edge_snapshot_digest"]))) || !strings.EqualFold(strings.TrimSpace(anyToString(control["case_snapshot_digest"])), strings.TrimSpace(anyToString(snapshot["digest"]))) {
		return false
	}
	if strings.TrimSpace(anyToString(control["control_request_digest"])) == "" || strings.TrimSpace(anyToString(control["control_response_digest"])) == "" || anyToString(control["control_composition_path"]) != "production_final_response_seam_graph_disabled" || !graphRecallFinalModelVisibleProjectionValid(anyMap(control["control_final_model_visible"])) || !graphRecallEconomicsObservationComplete(map[string]any{"cost_observability": cloneJSONMap(anyMap(control["cost_observability"]))}) {
		return false
	}
	if anyToBool(control["target_direct_hit"]) != graphRecallFinalModelVisibleContains(anyMap(control["control_final_model_visible"]), anyToString(row["target_memory_id"]), normalizeExpectedFileTokens(row["graph_expected_files"])) {
		return false
	}
	if _, ok := graphRecallControlLatencyValid(control); !ok {
		return false
	}
	if anyToBool(row["incremental_needed"]) == anyToBool(control["target_direct_hit"]) {
		return false
	}
	if strings.TrimSpace(anyToString(control["digest"])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(control["digest"])), "sha256:"+graphCorpusDigestMap(control, "digest")) {
		return false
	}
	identity := anyMap(control["source_runtime_identity"])
	currentIdentity := contextLatticeBuildIdentity()
	if !anyToBool(identity["source_bound"]) && anyToBool(currentIdentity["source_bound"]) {
		return false
	}
	if anyToBool(currentIdentity["source_bound"]) && (!strings.EqualFold(anyToString(identity["source_commit"]), anyToString(currentIdentity["source_commit"])) || !strings.EqualFold(anyToString(identity["source_tree"]), anyToString(currentIdentity["source_tree"]))) {
		return false
	}
	return true
}

func graphCorpusIncrementalControlRowSourceValid(row map[string]any) bool {
	control := anyMap(row["incremental_control"])
	if len(control) == 0 || anyToString(control["schema_id"]) != savedRecallGraphIncrementalControlSchemaID || anyToInt(control["version"], 0) != savedRecallGraphIncrementalControlVersion || anyToString(control["authority"]) != savedRecallGraphIncrementalControlAuthority {
		return false
	}
	if _, present := control["target_direct_hit"]; !present || !anyToBool(control["graph_influence_disabled"]) || anyToBool(control["graph_backend_consulted"]) || anyToBool(control["graph_results_used"]) || anyToBool(control["candidate_allocation_active"]) || anyToBool(control["treatment_active"]) || anyToString(control["traffic_class"]) != "evaluation_holdout" || anyToInt(control["control_k"], 0) != defaultRecallEvalK || strings.TrimSpace(anyToString(control["control_request_digest"])) == "" || strings.TrimSpace(anyToString(control["control_response_digest"])) == "" || anyToString(control["control_composition_path"]) != "production_final_response_seam_graph_disabled" || !graphRecallFinalModelVisibleProjectionValid(anyMap(control["control_final_model_visible"])) || !graphRecallEconomicsObservationComplete(map[string]any{"cost_observability": cloneJSONMap(anyMap(control["cost_observability"]))}) {
		return false
	}
	if anyToBool(control["target_direct_hit"]) != graphRecallFinalModelVisibleContains(anyMap(control["control_final_model_visible"]), anyToString(row["target_memory_id"]), normalizeExpectedFileTokens(row["graph_expected_files"])) {
		return false
	}
	if _, ok := graphRecallControlLatencyValid(control); !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(anyToString(control["seed_target_lineage"])), strings.TrimSpace(anyToString(row["seed_target_lineage"]))) && graphRecallMemoryIDEqual(anyToString(control["seed_memory_id"]), anyToString(row["seed_memory_id"])) && graphRecallMemoryIDEqual(anyToString(control["target_memory_id"]), anyToString(row["target_memory_id"])) && anyToBool(row["incremental_needed"]) != anyToBool(control["target_direct_hit"])
}

func graphCorpusRecordToCase(record graphRecallCorpusRecord, split string, ordinal int, incremental bool) map[string]any {
	caseID := graphCorpusRecordCaseID(record, split, ordinal)
	row := map[string]any{
		"id":                       caseID,
		"schema_id":                savedRecallGraphCorpusSchemaID,
		"split":                    split,
		"owner":                    savedRecallGraphCorpusOwner,
		"project":                  record.Project,
		"project_scope":            record.Project,
		"topic_path":               record.TopicPath,
		"agent_id":                 record.AgentID,
		"agent_family":             record.AgentFamily,
		"session_id":               record.SessionID,
		"source_family":            firstNonEmptyStrings(record.SourceFamily, "unknown"),
		"query":                    record.Query,
		"limit":                    defaultRecallEvalK,
		"k":                        defaultRecallEvalK,
		"direct_expected_files":    []string{record.SeedFile},
		"expected_files":           []string{record.SeedFile},
		"graph_expected_files":     []string{record.TargetFile},
		"graph_seed_memory_id":     record.SeedID,
		"graph_target_memory_id":   record.TargetID,
		"seed_memory_id":           record.SeedID,
		"target_memory_id":         record.TargetID,
		"relation":                 record.Relation,
		"graph_expected_relations": []string{record.Relation},
		"edge_id":                  record.EdgeID,
		"edge_confidence":          roundFloat(record.Confidence, 6),
		"seed_updated_at":          anyTimeString(record.SeedUpdatedAt),
		"target_updated_at":        anyTimeString(record.TargetUpdatedAt),
		"source_updated_at":        anyTimeString(record.SeedUpdatedAt),
		"time_bucket":              record.TimeBucket,
		"split_scope_key":          graphCorpusSplitScopeKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket),
		"seed_target_lineage":      record.LineageKey,
		"incremental_needed":       incremental,
		"incremental_control":      cloneJSONMap(record.IncrementalControl),
		"case_kind":                "graph_topology_positive",
		"positive":                 true,
		"hard_negative":            false,
		"oracle_leakage":           "query_excludes_seed_and_target_file_names; labels remain in closed oracle",
		"label_derivation":         "bounded_live_index_memory_edge",
		"provenance": map[string]any{
			"owner":                  savedRecallGraphCorpusOwner,
			"source_index":           "gateway_current_state_index",
			"project_scope_verified": true,
			"seed_project":           record.Project,
			"target_project":         record.Project,
			"edge_project":           record.Project,
			"raw_store_scanned":      false,
		},
	}
	return row
}

func graphCorpusHardNegativeCase(record graphRecallCorpusRecord, split string, ordinal int) map[string]any {
	row := graphCorpusRecordToCase(record, split, ordinal, false)
	row["id"] = graphCorpusRecordCaseID(record, split, ordinal) + "-negative"
	row["case_kind"] = "graph_hard_negative"
	row["positive"] = false
	row["hard_negative"] = true
	row["relation"] = "hard_negative"
	row["graph_expected_relations"] = []string{}
	row["edge_id"] = ""
	row["edge_confidence"] = nil
	row["graph_expected_files"] = []string{}
	row["forbidden_graph_files"] = []string{record.TargetFile}
	row["forbidden_memory_ids"] = []string{record.TargetID}
	row["negative_target_memory_id"] = record.TargetID
	row["negative_proof"] = record.NegativeProof
	row["negative_adjacency_complete"] = record.NegativeAdjacencyComplete
	row["negative_edge_snapshot_digest"] = record.NegativeSnapshotDigest
	row["negative_content_overlap_checked"] = record.NegativeContentChecked
	row["negative_content_overlap"] = false
	row["negative_content_overlap_proof"] = record.NegativeContentProof
	row["negative_semantic_overlap_checked"] = record.NegativeSemanticChecked
	row["negative_semantic_overlap"] = false
	row["negative_semantic_overlap_proof"] = record.NegativeSemanticProof
	row["negative_overlap_authority"] = "gateway_current_state_index"
	row["negative_oracle"] = map[string]any{"expected_graph_hit": false, "forbidden_target": record.TargetID, "forbidden_file": record.TargetFile}
	row["observed_relations"] = graphCorpusSortedStrings(record.ObservedRelations)
	row["label_derivation"] = "bounded_live_index_absence_of_allowed_relation_and_current_target_content_semantic_disjointness"
	return row
}

func graphCorpusCurrentSemanticRelationValid(seed, target memoryStoreDoc, relation, seedSession, targetSession string) bool {
	relation = normalizeGraphCorpusRelation(relation)
	if relation == "" || !shouldSurfaceMemoryLifecycle(seed.Lifecycle, false) || !shouldSurfaceMemoryLifecycle(target.Lifecycle, false) {
		return false
	}
	if isEphemeralMemoryIdentity(seed.FileName, seed.TopicPath, seed.Summary, seed.Lifecycle) || isEphemeralMemoryIdentity(target.FileName, target.TopicPath, target.Summary, target.Lifecycle) {
		return false
	}
	seedProject, _, seedID, _, seedErr := canonicalMemoryID(seed.Project + "::" + seed.FileName)
	_, _, targetID, _, targetErr := canonicalMemoryID(target.Project + "::" + target.FileName)
	if seedErr != nil || targetErr != nil || strings.EqualFold(seedID, targetID) || !strings.EqualFold(strings.TrimSpace(seed.Project), strings.TrimSpace(target.Project)) {
		return false
	}
	switch relation {
	case "references":
		known := map[string]memoryEdgeBackfillDoc{
			strings.ToLower(targetID): {MemoryID: targetID},
		}
		for _, currentTargetID := range referencedMemoryIDs(seedProject, seed.Summary, known) {
			if strings.EqualFold(currentTargetID, targetID) {
				return true
			}
		}
		return false
	case "same_session":
		seedSession = strings.TrimSpace(seedSession)
		targetSession = strings.TrimSpace(targetSession)
		return seedSession != "" && targetSession != "" && seedSession == targetSession
	case "same_topic":
		seedTopic := normalizeTopicPathLoose(seed.TopicPath)
		targetTopic := normalizeTopicPathLoose(target.TopicPath)
		return seedTopic != "" && targetTopic != "" && seedTopic == targetTopic
	default:
		return false
	}
}

var graphCorpusContentOverlapStopTokens = map[string]struct{}{
	"bounded": {}, "current": {}, "document": {}, "evidence": {}, "fixture": {}, "graph": {},
	"memory": {}, "notes": {}, "project": {}, "retrieval": {}, "seed": {}, "sequence": {},
	"source": {}, "summary": {}, "target": {}, "topic": {}, "with": {},
}

var graphCorpusSemanticOverlapStopTokens = map[string]struct{}{
	"graph": {}, "memory": {}, "notes": {}, "project": {}, "source": {}, "topic": {},
}

func graphCorpusMeaningfulOverlapTokens(value string, stop map[string]struct{}) map[string]struct{} {
	tokens := map[string]struct{}{}
	for _, raw := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		raw = strings.TrimSpace(raw)
		if raw == "" || len(raw) < 3 {
			continue
		}
		allDigits := true
		for _, r := range raw {
			if !unicode.IsDigit(r) {
				allDigits = false
				break
			}
		}
		if allDigits {
			continue
		}
		if _, ignored := stop[raw]; ignored {
			continue
		}
		tokens[raw] = struct{}{}
	}
	return tokens
}

func graphCorpusLexicalTokens(value string) []string {
	tokens := make([]string, 0)
	for _, raw := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if token := strings.TrimSpace(raw); token != "" {
			tokens = append(tokens, token)
		}
	}
	return tokens
}

func graphCorpusTokenPhraseContains(haystack, needle string) bool {
	haystackTokens := graphCorpusLexicalTokens(haystack)
	needleTokens := graphCorpusLexicalTokens(needle)
	if len(needleTokens) == 0 || len(needleTokens) > len(haystackTokens) {
		return false
	}
	for start := 0; start+len(needleTokens) <= len(haystackTokens); start++ {
		matched := true
		for offset := range needleTokens {
			if haystackTokens[start+offset] != needleTokens[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func graphCorpusFileOracleOverlap(query, fileName string) bool {
	base := filepath.Base(strings.Trim(strings.TrimSpace(strings.ReplaceAll(fileName, "\\", "/")), "/"))
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	if strings.TrimSpace(stem) == "" {
		return false
	}
	if graphCorpusTokenPhraseContains(query, stem) {
		return true
	}
	queryTokens := graphCorpusMeaningfulOverlapTokens(query, nil)
	fileTokens := graphCorpusMeaningfulOverlapTokens(stem, nil)
	return graphCorpusTokenIntersection(queryTokens, fileTokens) > 0
}

func graphCorpusTokenIntersection(left, right map[string]struct{}) int {
	if len(left) > len(right) {
		left, right = right, left
	}
	shared := 0
	for token := range left {
		if _, ok := right[token]; ok {
			shared++
		}
	}
	return shared
}

// graphCorpusCurrentContentSemanticOverlap is deliberately bounded and
// deterministic. It uses only the exact current-state endpoint summaries and
// topic paths; it does not infer similarity through a provider or a graph
// edge. The two-token threshold preserves valid negatives that merely share
// benchmark boilerplate while rejecting a target whose current content or
// semantic topic anchors are already present in the seed query.
func graphCorpusCurrentContentSemanticOverlap(seed, target memoryStoreDoc) (bool, bool, string) {
	if !shouldSurfaceMemoryLifecycle(seed.Lifecycle, false) || !shouldSurfaceMemoryLifecycle(target.Lifecycle, false) || !strings.EqualFold(strings.TrimSpace(seed.Project), strings.TrimSpace(target.Project)) {
		return false, false, "current_endpoint_scope_unavailable"
	}
	contentTokens := graphCorpusMeaningfulOverlapTokens(seed.Summary, graphCorpusContentOverlapStopTokens)
	targetContentTokens := graphCorpusMeaningfulOverlapTokens(target.Summary, graphCorpusContentOverlapStopTokens)
	if graphCorpusTokenPhraseContains(seed.Summary, target.Summary) {
		return true, false, "target_current_summary_phrase_in_seed_content"
	}
	contentShared := graphCorpusTokenIntersection(contentTokens, targetContentTokens)
	if contentShared >= 2 && (contentShared*2 >= len(targetContentTokens) || contentShared >= 3) {
		return true, false, "target_current_content_anchor_overlap"
	}
	seedTopicTokens := graphCorpusMeaningfulOverlapTokens(seed.TopicPath, graphCorpusSemanticOverlapStopTokens)
	targetTopicTokens := graphCorpusMeaningfulOverlapTokens(target.TopicPath, graphCorpusSemanticOverlapStopTokens)
	semanticShared := graphCorpusTokenIntersection(seedTopicTokens, targetTopicTokens)
	if semanticShared >= 2 && (semanticShared*2 >= len(targetTopicTokens) || semanticShared >= 3) {
		return false, true, "target_current_semantic_topic_overlap"
	}
	return false, false, "current_target_content_and_semantic_anchors_disjoint"
}

func graphCorpusCurrentRelationValid(seed, target memoryStoreDoc, edge memoryEdgeEntry, seedSession, targetSession string) bool {
	relation := normalizeGraphCorpusRelation(edge.Relation)
	if relation == "" || !shouldSurfaceMemoryLifecycle(edge.Lifecycle, false) {
		return false
	}
	seedProject, _, seedID, _, seedErr := canonicalMemoryID(seed.Project + "::" + seed.FileName)
	_, _, targetID, _, targetErr := canonicalMemoryID(target.Project + "::" + target.FileName)
	if seedErr != nil || targetErr != nil || strings.TrimSpace(edge.EdgeID) == "" || !strings.EqualFold(strings.TrimSpace(edge.Source), memoryEdgeSource) || !strings.EqualFold(strings.TrimSpace(edge.Project), seedProject) || edge.EdgeID != deterministicMemoryEdgeID(seedID, relation, targetID) {
		return false
	}
	for _, receipt := range []map[string]any{edge.Provenance, edge.Metadata} {
		if anyToBool(receipt["retired"]) || anyToBool(receipt["stale"]) || anyToBool(receipt["superseded"]) || anyToBool(receipt["retracted"]) || strings.TrimSpace(anyToString(receipt["superseded_by"])) != "" {
			return false
		}
		if active, present := receipt["active"]; present && !anyToBool(active) {
			return false
		}
		if valid, present := receipt["valid"]; present && !anyToBool(valid) {
			return false
		}
	}
	return graphCorpusCurrentSemanticRelationValid(seed, target, relation, seedSession, targetSession)
}

func graphCorpusRecordFromEdge(seed, target memoryStoreDoc, edge memoryEdgeEntry, seedAgent, seedSession, targetAgent, targetSession string) (graphRecallCorpusRecord, bool) {
	relation := normalizeGraphCorpusRelation(edge.Relation)
	if relation == "" {
		return graphRecallCorpusRecord{}, false
	}
	seedProject := strings.TrimSpace(seed.Project)
	targetProject := strings.TrimSpace(target.Project)
	edgeProject := strings.TrimSpace(edge.Project)
	if seedProject == "" || !strings.EqualFold(seedProject, targetProject) || (edgeProject != "" && !strings.EqualFold(seedProject, edgeProject)) {
		return graphRecallCorpusRecord{}, false
	}
	_, _, seedID, _, seedErr := canonicalMemoryID(seedProject + "::" + seed.FileName)
	_, _, targetID, _, targetErr := canonicalMemoryID(targetProject + "::" + target.FileName)
	if seedErr != nil || targetErr != nil || strings.EqualFold(seedID, targetID) {
		return graphRecallCorpusRecord{}, false
	}
	if !graphCorpusCurrentRelationValid(seed, target, edge, seedSession, targetSession) {
		return graphRecallCorpusRecord{}, false
	}
	seedUpdated := seed.UpdatedAt.UTC()
	targetUpdated := target.UpdatedAt.UTC()
	family := graphCorpusAgentFamily(seedAgent, "")
	if family == "" {
		family = graphCorpusAgentFamily(targetAgent, "")
	}
	session := strings.TrimSpace(seedSession)
	if session == "" {
		session = strings.TrimSpace(targetSession)
	}
	timeBucket := graphCorpusTimeBucket(seedUpdated)
	query := recallEvalQueryFromDoc(seed)
	query = recallEvalRedactFileTokens(query, target.FileName)
	if query == "" {
		return graphRecallCorpusRecord{}, false
	}
	record := graphRecallCorpusRecord{
		Project: seedProject, TopicPath: normalizeTopicPathLoose(seed.TopicPath),
		AgentID: strings.TrimSpace(seedAgent), AgentFamily: family, SessionID: session,
		SeedID: seedID, TargetID: targetID, SeedFile: strings.Trim(seed.FileName, "/"),
		TargetFile: strings.Trim(target.FileName, "/"), Relation: relation,
		EdgeID: strings.TrimSpace(edge.EdgeID), Confidence: edge.Confidence,
		SeedUpdatedAt: seedUpdated, TargetUpdatedAt: targetUpdated,
		TimeBucket: timeBucket, Query: query,
	}
	record.LineageKey = graphCorpusLineageKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket, record.SeedID, record.TargetID, record.Relation)
	return record, true
}

func graphCorpusPopulationDimensions(records []graphRecallCorpusRecord) map[string]int {
	projects := map[string]struct{}{}
	agents := map[string]struct{}{}
	sessions := map[string]struct{}{}
	for _, record := range records {
		if record.Project != "" {
			projects[strings.ToLower(record.Project)] = struct{}{}
		}
		if record.AgentFamily != "" {
			agents[strings.ToLower(record.AgentFamily)] = struct{}{}
		}
		if record.SessionID != "" {
			sessions[strings.ToLower(record.SessionID)] = struct{}{}
		}
	}
	return map[string]int{"projects": len(projects), "agent_families": len(agents), "sessions": len(sessions)}
}

func graphCorpusRequiredDimensions() map[string]int {
	return map[string]int{"projects": savedRecallGraphCorpusMinProjects, "agent_families": savedRecallGraphCorpusMinAgentFamilies, "sessions": savedRecallGraphCorpusMinSessions}
}

func graphCorpusEnoughDimensions(records []graphRecallCorpusRecord) bool {
	population := graphCorpusPopulationDimensions(records)
	for key, required := range graphCorpusRequiredDimensions() {
		if population[key] < required {
			return false
		}
	}
	return true
}

func graphCorpusRelationCounts(records []graphRecallCorpusRecord) map[string]int {
	counts := map[string]int{}
	for _, relation := range savedRecallGraphRelations {
		counts[relation] = 0
	}
	for _, record := range records {
		if record.HardNegative {
			continue
		}
		counts[record.Relation]++
	}
	return counts
}

func graphCorpusSelectRecords(records []graphRecallCorpusRecord, relation string, count int, holdout bool, usedLineage, usedScopes map[string]struct{}) []graphRecallCorpusRecord {
	if count <= 0 {
		return []graphRecallCorpusRecord{}
	}
	candidates := make([]graphRecallCorpusRecord, 0, len(records))
	for _, record := range records {
		if record.HardNegative || record.Relation != relation {
			continue
		}
		if _, used := usedLineage[record.LineageKey]; used {
			continue
		}
		scopeKey := graphCorpusSplitScopeKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket)
		if _, used := usedScopes[scopeKey]; used {
			continue
		}
		rank := sha256Hex(strings.Join([]string{"saved_recall_graph_corpus.v1.pick", relation, record.LineageKey, record.EdgeID}, "\x00"))
		if holdout && strings.HasPrefix(rank, "8") {
			candidates = append(candidates, record)
		} else if !holdout && !strings.HasPrefix(rank, "8") {
			candidates = append(candidates, record)
		}
	}
	// A hash bucket is only a deterministic preference. If it cannot fill the
	// requested quota, the caller's bounded population is still authoritative.
	sort.SliceStable(candidates, func(i, j int) bool {
		left := sha256Hex(strings.Join([]string{"saved_recall_graph_corpus.order", candidates[i].LineageKey, candidates[i].EdgeID}, "\x00"))
		right := sha256Hex(strings.Join([]string{"saved_recall_graph_corpus.order", candidates[j].LineageKey, candidates[j].EdgeID}, "\x00"))
		return left < right
	})
	if len(candidates) < count {
		fallback := make([]graphRecallCorpusRecord, 0, len(records))
		for _, record := range records {
			if record.HardNegative || record.Relation != relation {
				continue
			}
			if _, used := usedLineage[record.LineageKey]; used {
				continue
			}
			scopeKey := graphCorpusSplitScopeKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket)
			if _, used := usedScopes[scopeKey]; used {
				continue
			}
			fallback = append(fallback, record)
		}
		sort.SliceStable(fallback, func(i, j int) bool {
			return graphCorpusRecordSortKey(fallback[i]) < graphCorpusRecordSortKey(fallback[j])
		})
		seen := map[string]struct{}{}
		for _, record := range candidates {
			seen[record.LineageKey] = struct{}{}
		}
		for _, record := range fallback {
			if _, exists := seen[record.LineageKey]; exists {
				continue
			}
			candidates = append(candidates, record)
			seen[record.LineageKey] = struct{}{}
		}
	}
	if len(candidates) > count {
		candidates = candidates[:count]
	}
	return candidates
}

func graphCorpusBuildRecords(candidates []recallEvalSourceCandidate, edges []memoryEdgeEntry) ([]graphRecallCorpusRecord, map[string]int) {
	docs := map[string]memoryStoreDoc{}
	identity := map[string]recallEvalIdentity{}
	for _, candidate := range candidates {
		_, _, memoryID, _, err := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
		if err != nil {
			continue
		}
		docs[strings.ToLower(memoryID)] = candidate.doc
		identity[strings.ToLower(memoryID)] = recallEvalIdentity{agentID: candidate.agentID, sessionID: candidate.sessionID, createdAt: candidate.createdAt}
	}
	records := make([]graphRecallCorpusRecord, 0, len(edges))
	seen := map[string]struct{}{}
	for _, edge := range edges {
		if edge.Confidence < 0.95 || normalizeGraphCorpusRelation(edge.Relation) == "" {
			continue
		}
		_, _, sourceID, _, sourceErr := canonicalMemoryID(edge.SourceID)
		_, _, targetID, _, targetErr := canonicalMemoryID(edge.TargetID)
		if sourceErr != nil || targetErr != nil {
			continue
		}
		seed, sourceOK := docs[strings.ToLower(sourceID)]
		target, targetOK := docs[strings.ToLower(targetID)]
		if !sourceOK || !targetOK {
			continue
		}
		seedIdentity := identity[strings.ToLower(sourceID)]
		targetIdentity := identity[strings.ToLower(targetID)]
		record, ok := graphCorpusRecordFromEdge(seed, target, edge, seedIdentity.agentID, seedIdentity.sessionID, targetIdentity.agentID, targetIdentity.sessionID)
		if !ok {
			continue
		}
		seenKey := record.Relation + "\x00" + record.LineageKey
		if _, exists := seen[seenKey]; exists {
			continue
		}
		seen[seenKey] = struct{}{}
		records = append(records, record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		return graphCorpusRecordSortKey(records[i]) < graphCorpusRecordSortKey(records[j])
	})
	return records, graphCorpusRelationCounts(records)
}

func graphCorpusControlCostReceipt(totals graphRecallEvaluationTotals) map[string]any {
	attempted := totals.costObservationExpected
	observed := totals.costObservationObserved
	missing := maxInt(savedRecallGraphCorpusTotalPositiveCases-observed, 0)
	transportObserved := attempted == savedRecallGraphCorpusTotalPositiveCases && observed == savedRecallGraphCorpusTotalPositiveCases && totals.costObservationMissing == 0
	policyRun := graphRecallSourcePolicyRunReceipt(totals)
	provenZero := transportObserved && totals.networkCallsKnown && totals.localBackendCallsKnown && totals.externalNetworkCallsKnown && totals.externalNetworkZeroProven && totals.providerCallsKnown && totals.providerTokensKnown && totals.providerCostKnown && totals.externalNetworkCalls == 0 && totals.providerCalls == 0 && totals.providerTokens == 0 && totals.providerCostMicros == 0 && anyToBool(policyRun["eligible"])
	receipt := map[string]any{
		"schema_id": retrievalCostObservabilitySchemaID, "authority": retrievalCostObservabilityAuthority,
		"scope":         "saved_recall_graph_control_capture",
		"network_calls": totals.networkCalls, "network_calls_observed": totals.networkCallsKnown,
		"local_backend_calls": totals.localBackendCalls, "local_backend_calls_observed": totals.localBackendCallsKnown,
		"external_network_calls": totals.externalNetworkCalls, "external_network_calls_observed": totals.externalNetworkCallsKnown, "external_network_zero_proven": totals.externalNetworkZeroProven,
		"provider_calls": totals.providerCalls, "provider_calls_observed": totals.providerCallsKnown,
		"provider_tokens": totals.providerTokens, "provider_tokens_observed": totals.providerTokensKnown,
		"provider_cost_microusd": totals.providerCostMicros, "provider_cost_observed": totals.providerCostKnown,
		"control_requests_expected":     savedRecallGraphCorpusTotalPositiveCases,
		"control_requests_attempted":    attempted,
		"control_requests_observed":     observed,
		"control_requests_missing":      missing,
		"observation_expected":          savedRecallGraphCorpusTotalPositiveCases,
		"observation_expected_required": savedRecallGraphCorpusTotalPositiveCases,
		"observation_attempted":         attempted,
		"observation_observed":          observed,
		"observation_missing":           missing,
		"observation_failures":          totals.costObservationMissing,
		"transport_observed":            transportObserved, "proven_zero": provenZero,
		"traffic_class": "evaluation_holdout", "source_policy_run": policyRun,
		"source_policy_observed": totals.sourcePolicyObserved, "source_policy_consistent": totals.sourcePolicyConsistent,
		"truth": "server-owned preflight and observed per-control receipts; missing observations fail closed",
	}
	receipt["digest"] = "sha256:" + graphCorpusDigestMap(receipt, "digest")
	return receipt
}

func (s *server) captureSavedRecallGraphIncrementalControls(ctx context.Context, records []graphRecallCorpusRecord, snapshotDigest, sourceSnapshotDigest, edgeSnapshotDigest string) (map[string]any, map[string]any, string) {
	if s == nil || len(records) == 0 {
		return nil, nil, "graph-disabled control capture requires an enabled server and bounded positive records"
	}
	used := map[string]struct{}{}
	usedScopes := map[string]struct{}{}
	selected := make([]graphRecallCorpusRecord, 0, savedRecallGraphCorpusTotalPositiveCases)
	for _, relation := range savedRecallGraphRelations {
		for _, count := range []struct {
			holdout bool
			count   int
		}{{false, savedRecallGraphCorpusRelationDevelopmentCases}, {true, savedRecallGraphCorpusRelationHoldoutCases}} {
			rows := graphCorpusSelectRecords(records, relation, count.count, count.holdout, used, usedScopes)
			if len(rows) != count.count {
				return nil, nil, "graph-disabled control capture could not close the deterministic topology cohort"
			}
			for _, record := range rows {
				used[record.LineageKey] = struct{}{}
				usedScopes[graphCorpusSplitScopeKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket)] = struct{}{}
				selected = append(selected, record)
			}
		}
	}
	controls := map[string]any{}
	totals := graphRecallEvaluationTotals{}
	for _, record := range selected {
		request := map[string]any{
			"query": record.Query, "limit": defaultRecallEvalK, "project": record.Project, "topic_path": record.TopicPath,
			"retrieval_mode": "balanced", "retrieval_intent": "decision", "include_grounding": true,
			"include_retrieval_debug": false, "include_preferences": false, "rerank_with_learning": false,
			"sources": []string{sourceTopicRollup},
			"user_id": "", "agent_id": record.AgentID, "auto_escalate": false, "deep_async": false,
			"callback_url": "", "traffic_class": "evaluation_holdout", "graph_incremental_control": true,
			"graph_control_seed_memory_id": record.SeedID, "graph_control_target_memory_id": record.TargetID,
			"graph_control_target_files": []string{record.TargetFile}, "graph_control_seed_target_lineage": record.LineageKey,
			"graph_control_snapshot_digest": snapshotDigest, "graph_control_source_snapshot_digest": sourceSnapshotDigest,
			"graph_control_edge_snapshot_digest": edgeSnapshotDigest, "graph_control_k": defaultRecallEvalK,
		}
		controlStarted := time.Now()
		response, status, err := s.executeRetrieval(ctx, nil, request, true)
		graphRecallRecordEconomics(response, &totals)
		if err != nil || status < 200 || status >= 300 {
			return nil, graphCorpusControlCostReceipt(totals), "graph-disabled control retrieval failed before the fixed incremental cohort was sealed"
		}
		if !graphRecallEconomicsObservationComplete(response) {
			return nil, graphCorpusControlCostReceipt(totals), "graph-disabled control retrieval did not prove preflight-enforced provider and external-network zero"
		}
		receipt := cloneJSONMap(anyMap(response["graph_incremental_control_receipt"]))
		controlVisible, _, composed := s.graphRecallProductionResponseProjection(ctx, graphRecallControlRequestProjection(request), response, true)
		if !composed {
			return nil, graphCorpusControlCostReceipt(totals), "graph-disabled control did not traverse the production final-response composition seam"
		}
		controlLatencyMs := float64(time.Since(controlStarted).Microseconds()) / 1000
		receipt["control_latency_ms"] = roundFloat(controlLatencyMs, 3)
		receipt["control_latency_scope"] = graphRecallPairedLatencyScope
		receipt["control_latency_comparable"] = true
		receipt["target_retrieval_hit"] = receipt["target_direct_hit"]
		receipt["target_direct_hit"] = graphRecallFinalModelVisibleContains(controlVisible, record.TargetID, map[string]struct{}{strings.Trim(record.TargetFile, "/"): {}})
		receipt["control_final_model_visible"] = controlVisible
		receipt["control_composition_path"] = "production_final_response_seam_graph_disabled"
		receipt["digest"] = "sha256:" + graphCorpusDigestMap(receipt, "digest")
		if !graphCorpusIncrementalControlValid(receipt, record, snapshotDigest, sourceSnapshotDigest, edgeSnapshotDigest) {
			return nil, graphCorpusControlCostReceipt(totals), "graph-disabled control receipt failed exact seed-target, snapshot, request, response, or economics binding"
		}
		controls[record.LineageKey] = receipt
	}
	cost := graphCorpusControlCostReceipt(totals)
	if !anyToBool(cost["proven_zero"]) || anyToInt(cost["control_requests_observed"], 0) != savedRecallGraphCorpusTotalPositiveCases {
		return nil, cost, "graph-disabled control capture did not close the exact provider-free 270-request cohort"
	}
	return controls, cost, ""
}

// graphCorpusHydrateEdgeEndpoints closes the ordinary bottom-K sampling gap:
// graph edges are authoritative topology rows, so both endpoints must be
// hydrated from the current-state index even when a target falls outside the
// ordinary document sample. The endpoint set is bounded by the complete edge
// snapshot cap and never reads the hot raw store.
func graphCorpusHydrateEdgeEndpoints(ctx context.Context, m *memoryStore, edges []memoryEdgeEntry, project, topicPrefix string) []recallEvalSourceCandidate {
	if m == nil || len(edges) == 0 {
		return []recallEvalSourceCandidate{}
	}
	keysByID := map[string]recallEvalRankedKey{}
	for _, edge := range edges {
		for _, rawID := range []string{edge.SourceID, edge.TargetID} {
			_, _, canonical, key, err := canonicalMemoryID(rawID)
			if err != nil {
				continue
			}
			if project != "" && !strings.EqualFold(strings.TrimSpace(strings.SplitN(canonical, "::", 2)[0]), strings.TrimSpace(project)) {
				continue
			}
			keysByID[strings.ToLower(canonical)] = recallEvalRankedKey{key: key, rank: recallEvalRankedKeyRank(key)}
		}
	}
	keys := make([]recallEvalRankedKey, 0, len(keysByID))
	for _, key := range keysByID {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool { return keys[i].key < keys[j].key })
	entries := recallEvalCurrentStateEntriesForKeys(ctx, m, keys)
	identity, _, identityErr := recallEvalMetadataForKeys(ctx, m, keys)
	if identityErr != nil {
		return []recallEvalSourceCandidate{}
	}
	candidates, _ := recallEvalCandidatesFromCurrentStates(ctx, m, entries, identity, map[string]struct{}{}, project, topicPrefix, len(keys), "current_state_edge_endpoint_hydration")
	return candidates
}

func graphCorpusMergeCandidates(base, endpoints []recallEvalSourceCandidate) []recallEvalSourceCandidate {
	merged := append([]recallEvalSourceCandidate(nil), base...)
	seen := map[string]struct{}{}
	for _, candidate := range merged {
		_, _, memoryID, _, err := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
		if err == nil {
			seen[strings.ToLower(memoryID)] = struct{}{}
		}
	}
	for _, candidate := range endpoints {
		_, _, memoryID, _, err := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
		if err != nil {
			continue
		}
		if _, exists := seen[strings.ToLower(memoryID)]; exists {
			continue
		}
		seen[strings.ToLower(memoryID)] = struct{}{}
		merged = append(merged, candidate)
	}
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].stableKey < merged[j].stableKey })
	return merged
}

func graphCorpusBuildNegativeRecords(candidates []recallEvalSourceCandidate, positive []graphRecallCorpusRecord, max int) []graphRecallCorpusRecord {
	return graphCorpusBuildNegativeRecordsWithSnapshot(candidates, positive, nil, nil, max)
}

func graphCorpusBuildNegativeRecordsWithSnapshot(candidates []recallEvalSourceCandidate, positive []graphRecallCorpusRecord, edges []memoryEdgeEntry, completeSeeds map[string]struct{}, max int) []graphRecallCorpusRecord {
	if max < 1 {
		return []graphRecallCorpusRecord{}
	}
	if len(edges) == 0 || len(completeSeeds) == 0 {
		return []graphRecallCorpusRecord{}
	}
	docs := make([]recallEvalSourceCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.doc.Project == "" || candidate.doc.FileName == "" || recallEvalQueryFromDoc(candidate.doc) == "" || !shouldSurfaceMemoryLifecycle(candidate.doc.Lifecycle, false) || isEphemeralMemoryIdentity(candidate.doc.FileName, candidate.doc.TopicPath, candidate.doc.Summary, candidate.doc.Lifecycle) {
			continue
		}
		docs = append(docs, candidate)
	}
	sort.SliceStable(docs, func(i, j int) bool { return docs[i].stableKey < docs[j].stableKey })
	connected := map[string]map[string]struct{}{}
	for _, record := range positive {
		if connected[record.SeedID] == nil {
			connected[record.SeedID] = map[string]struct{}{}
		}
		connected[record.SeedID][record.TargetID] = struct{}{}
	}
	observed := map[string]map[string]map[string]struct{}{}
	for _, edge := range edges {
		_, _, sourceID, _, sourceErr := canonicalMemoryID(edge.SourceID)
		_, _, targetID, _, targetErr := canonicalMemoryID(edge.TargetID)
		if sourceErr != nil || targetErr != nil || normalizeGraphCorpusRelation(edge.Relation) == "" {
			continue
		}
		for _, pair := range [][2]string{{sourceID, targetID}, {targetID, sourceID}} {
			if observed[pair[0]] == nil {
				observed[pair[0]] = map[string]map[string]struct{}{}
			}
			if observed[pair[0]][pair[1]] == nil {
				observed[pair[0]][pair[1]] = map[string]struct{}{}
			}
			observed[pair[0]][pair[1]][normalizeGraphCorpusRelation(edge.Relation)] = struct{}{}
		}
	}
	negatives := make([]graphRecallCorpusRecord, 0, max)
	for index, seed := range docs {
		if len(negatives) >= max {
			break
		}
		_, _, seedID, _, seedErr := canonicalMemoryID(seed.doc.Project + "::" + seed.doc.FileName)
		if seedErr != nil {
			continue
		}
		if _, complete := completeSeeds[seedID]; !complete {
			continue
		}
		for offset := 1; offset < len(docs); offset++ {
			target := docs[(index+offset)%len(docs)]
			if !strings.EqualFold(seed.doc.Project, target.doc.Project) || strings.EqualFold(seed.doc.FileName, target.doc.FileName) {
				continue
			}
			_, _, targetID, _, targetErr := canonicalMemoryID(target.doc.Project + "::" + target.doc.FileName)
			if targetErr != nil {
				continue
			}
			if _, complete := completeSeeds[targetID]; !complete {
				continue
			}
			currentSemanticRelation := false
			for _, relation := range savedRecallGraphRelations {
				if graphCorpusCurrentSemanticRelationValid(seed.doc, target.doc, relation, seed.sessionID, target.sessionID) {
					currentSemanticRelation = true
					break
				}
			}
			if currentSemanticRelation {
				continue
			}
			contentOverlap, semanticOverlap, _ := graphCorpusCurrentContentSemanticOverlap(seed.doc, target.doc)
			if contentOverlap || semanticOverlap {
				continue
			}
			if _, linked := connected[seedID][targetID]; linked {
				continue
			}
			relations := observed[seedID][targetID]
			if len(relations) > 0 {
				continue
			}
			family := graphCorpusAgentFamily(seed.agentID, seed.sourceFamily)
			timeBucket := graphCorpusTimeBucket(seed.createdAt)
			observedRelations := make([]string, 0, len(relations))
			for relation := range relations {
				observedRelations = append(observedRelations, relation)
			}
			record := graphRecallCorpusRecord{
				Project: seed.doc.Project, TopicPath: normalizeTopicPathLoose(seed.doc.TopicPath),
				AgentID: seed.agentID, AgentFamily: family, SessionID: seed.sessionID,
				SourceFamily: seed.sourceFamily, SeedID: seedID, TargetID: targetID,
				SeedFile: strings.Trim(seed.doc.FileName, "/"), TargetFile: strings.Trim(target.doc.FileName, "/"),
				TimeBucket: timeBucket, Query: recallEvalQueryFromDoc(seed.doc), HardNegative: true,
				NegativeProof:     "complete_bounded_edge_snapshot_and_current_endpoints_contain_no_references_same_session_same_topic_target_content_or_semantic_overlap",
				ObservedRelations: graphCorpusSortedStrings(observedRelations), NegativeSnapshotDigest: "sha256:" + graphCorpusEdgesDigest(edges), NegativeAdjacencyComplete: true,
				NegativeContentChecked: true, NegativeSemanticChecked: true,
				NegativeContentProof:  "gateway_current_state_target_summary_content_anchors_disjoint",
				NegativeSemanticProof: "gateway_current_state_topic_semantic_anchors_disjoint",
			}
			record.Query = recallEvalRedactFileTokens(record.Query, record.TargetFile)
			record.LineageKey = graphCorpusLineageKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket, record.SeedID, record.TargetID, "hard_negative")
			negatives = append(negatives, record)
			break
		}
	}
	return negatives
}

// buildSavedRecallGraphCorpusFromCandidates is deterministic for a fixed
// bounded candidate/edge snapshot and seed. It never reads the hot raw store.
func buildSavedRecallGraphCorpusFromCandidates(candidates []recallEvalSourceCandidate, edges []memoryEdgeEntry, seed string, source string, sourceStats map[string]any) map[string]any {
	seed = firstNonEmptyStrings(strings.TrimSpace(seed), "graph-v1")
	indexMode := strings.ToLower(strings.TrimSpace(anyToString(sourceStats["index_mode"])))
	if !anyToBool(sourceStats["bounded"]) || !anyToBool(sourceStats["index_integrity"]) || !strings.Contains(indexMode, "current_state") {
		return graphCorpusInsufficientArtifact(source, map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "authoritative current-state index receipt is missing or integrity-invalid")
	}
	edgeSnapshot := anyMap(sourceStats["edge_snapshot"])
	if anyToString(edgeSnapshot["schema_id"]) != "saved_recall_graph_edge_snapshot.v1" || !anyToBool(edgeSnapshot["complete"]) || anyToBool(edgeSnapshot["truncated"]) || !anyToBool(edgeSnapshot["continuation_complete"]) {
		return graphCorpusInsufficientArtifact(source, map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "bounded edge snapshot is incomplete; hard-negative absence cannot be proven")
	}
	if !strings.EqualFold(strings.TrimSpace(anyToString(edgeSnapshot["edge_digest"])), "sha256:"+graphCorpusEdgesDigest(edges)) {
		return graphCorpusInsufficientArtifact(source, map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "bounded edge snapshot digest does not bind the supplied adjacency rows")
	}
	sourceSnapshotDigest := "sha256:" + graphCorpusCandidatesDigest(candidates)
	sourceEdgeSnapshotDigest := graphCorpusSourceEdgeSnapshotDigest(candidates, edges)
	if expected := strings.TrimSpace(anyToString(sourceStats["source_snapshot_digest"])); expected != "" && !strings.EqualFold(expected, sourceSnapshotDigest) {
		return graphCorpusInsufficientArtifact(source, map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "bounded source snapshot digest does not bind the supplied current-state rows")
	}
	if expected := strings.TrimSpace(anyToString(sourceStats["source_edge_snapshot_digest"])); expected != "" && !strings.EqualFold(expected, sourceEdgeSnapshotDigest) {
		return graphCorpusInsufficientArtifact(source, map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "combined source and edge snapshot digest does not bind one immutable evaluation snapshot")
	}
	if !anyToBool(sourceStats["snapshot_stable_during_control_capture"]) {
		return graphCorpusInsufficientArtifact(source, map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "source and edge snapshot stability was not proven across paired control capture")
	}
	completeSeeds := map[string]struct{}{}
	for _, rawID := range anyToStringSlice(sourceStats["complete_seed_ids"]) {
		if _, _, canonical, _, err := canonicalMemoryID(rawID); err == nil {
			completeSeeds[canonical] = struct{}{}
		}
	}
	if len(completeSeeds) == 0 {
		return graphCorpusInsufficientArtifact(source, map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "bounded edge snapshot has no complete seed adjacency receipts")
	}
	records, availableRelations := graphCorpusBuildRecords(candidates, edges)
	population := graphCorpusPopulationDimensions(records)
	required := graphCorpusRequiredDimensions()
	if !graphCorpusEnoughDimensions(records) {
		return graphCorpusInsufficientArtifact(source, population, required, availableRelations, "source population cannot satisfy project, agent-family, and session minima")
	}
	positiveNeeded := map[string]int{
		"references":   savedRecallGraphCorpusRelationDevelopmentCases + savedRecallGraphCorpusRelationHoldoutCases,
		"same_session": savedRecallGraphCorpusRelationDevelopmentCases + savedRecallGraphCorpusRelationHoldoutCases,
		"same_topic":   savedRecallGraphCorpusRelationDevelopmentCases + savedRecallGraphCorpusRelationHoldoutCases,
	}
	for relation, needed := range positiveNeeded {
		if availableRelations[relation] < needed {
			return graphCorpusInsufficientArtifact(source, population, required, availableRelations, "source population cannot satisfy development and holdout topology quotas for "+relation)
		}
	}
	boundRecords, controlDetail := graphCorpusBindIncrementalControls(records, sourceStats, sourceEdgeSnapshotDigest, sourceSnapshotDigest, anyToString(edgeSnapshot["digest"]))
	if controlDetail != "" {
		return graphCorpusInsufficientArtifact(source, population, required, availableRelations, controlDetail)
	}
	records = boundRecords
	negatives := graphCorpusBuildNegativeRecordsWithSnapshot(candidates, records, edges, completeSeeds, len(candidates))
	if len(negatives) < savedRecallGraphCorpusTotalHardNegatives {
		return graphCorpusInsufficientArtifact(source, population, required, availableRelations, "source population cannot produce split-complete development and holdout hard negatives")
	}
	used := map[string]struct{}{}
	usedScopes := map[string]struct{}{}
	development := make([]map[string]any, 0, savedRecallGraphCorpusDevelopmentCases)
	holdout := make([]map[string]any, 0, savedRecallGraphCorpusHoldoutCases)
	for _, relation := range savedRecallGraphRelations {
		devRecords := graphCorpusSelectRecords(records, relation, savedRecallGraphCorpusRelationDevelopmentCases, false, used, usedScopes)
		for index, record := range devRecords {
			used[record.LineageKey] = struct{}{}
			usedScopes[graphCorpusSplitScopeKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket)] = struct{}{}
			development = append(development, graphCorpusRecordToCase(record, "development", index, !anyToBool(record.IncrementalControl["target_direct_hit"])))
		}
		holdoutRecords := graphCorpusSelectRecords(records, relation, savedRecallGraphCorpusRelationHoldoutCases, true, used, usedScopes)
		for index, record := range holdoutRecords {
			used[record.LineageKey] = struct{}{}
			usedScopes[graphCorpusSplitScopeKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket)] = struct{}{}
			holdout = append(holdout, graphCorpusRecordToCase(record, "holdout", index, !anyToBool(record.IncrementalControl["target_direct_hit"])))
		}
	}
	for _, row := range append(append([]map[string]any{}, development...), holdout...) {
		if !graphCorpusIncrementalControlRowSourceValid(row) {
			return graphCorpusInsufficientArtifact(source, population, required, availableRelations, "selected topology case is missing its pre-treatment graph-disabled control receipt")
		}
	}
	negativeDevelopment := make([]graphRecallCorpusRecord, 0, savedRecallGraphCorpusDevelopmentHardNegatives)
	negativeHoldout := make([]graphRecallCorpusRecord, 0, savedRecallGraphCorpusHardNegatives)
	for _, record := range negatives {
		scopeKey := graphCorpusSplitScopeKey(record.Project, record.AgentFamily, record.SessionID, record.TimeBucket)
		if _, usedScope := usedScopes[scopeKey]; usedScope {
			continue
		}
		if len(negativeDevelopment) < savedRecallGraphCorpusDevelopmentHardNegatives {
			negativeDevelopment = append(negativeDevelopment, record)
			usedScopes[scopeKey] = struct{}{}
			continue
		}
		if len(negativeHoldout) < savedRecallGraphCorpusHardNegatives {
			negativeHoldout = append(negativeHoldout, record)
			usedScopes[scopeKey] = struct{}{}
		}
		if len(negativeHoldout) == savedRecallGraphCorpusHardNegatives {
			break
		}
	}
	if len(negativeDevelopment) != savedRecallGraphCorpusDevelopmentHardNegatives || len(negativeHoldout) != savedRecallGraphCorpusHardNegatives {
		return graphCorpusInsufficientArtifact(source, population, required, availableRelations, "source population cannot produce split-disjoint hard-negative scopes")
	}
	for index, record := range negativeDevelopment {
		record.NegativeSnapshotDigest = anyToString(edgeSnapshot["digest"])
		development = append(development, graphCorpusHardNegativeCase(record, "development", index))
	}
	for index, record := range negativeHoldout {
		record.NegativeSnapshotDigest = anyToString(edgeSnapshot["digest"])
		holdout = append(holdout, graphCorpusHardNegativeCase(record, "holdout", index))
	}
	sort.SliceStable(development, func(i, j int) bool { return anyToString(development[i]["id"]) < anyToString(development[j]["id"]) })
	sort.SliceStable(holdout, func(i, j int) bool { return anyToString(holdout[i]["id"]) < anyToString(holdout[j]["id"]) })
	cases := append(append([]map[string]any{}, development...), holdout...)
	snapshotSourceStats := cloneJSONMap(sourceStats)
	// Capture time is custody metadata. Do not let the nested source receipt
	// make an otherwise identical bounded snapshot acquire a new identity.
	delete(snapshotSourceStats, "captured_at")
	delete(snapshotSourceStats, "incremental_controls")
	snapshot := map[string]any{
		"schema_id":                   "saved_recall_graph_snapshot.v1",
		"source":                      source,
		"source_cap":                  savedRecallGraphCorpusMaxSourceDocs,
		"candidate_count":             len(candidates),
		"edge_count":                  len(edges),
		"eligible_edge_count":         len(records),
		"population":                  population,
		"source_stats":                snapshotSourceStats,
		"source_runtime_identity":     cloneJSONMap(anyMap(sourceStats["runtime_identity"])),
		"captured_at":                 anyToString(sourceStats["captured_at"]),
		"raw_store_scanned":           false,
		"authoritative_owner":         savedRecallGraphCorpusOwner,
		"authoritative_source_index":  "gateway_current_state_index",
		"source_index_mode":           indexMode,
		"capture_project":             anyToString(sourceStats["capture_project"]),
		"capture_topic_prefix":        anyToString(sourceStats["capture_topic_prefix"]),
		"capture_scope_bound":         true,
		"source_snapshot_digest":      sourceSnapshotDigest,
		"source_edge_snapshot_digest": sourceEdgeSnapshotDigest,
		"snapshot_version":            1,
		"project_scope_rule":          "seed_target_edge_same_project",
		"edge_snapshot_schema_id":     edgeSnapshot["schema_id"],
		"edge_snapshot_digest":        edgeSnapshot["digest"],
		"edge_snapshot_complete":      anyToBool(edgeSnapshot["complete"]),
		"edge_snapshot_truncated":     anyToBool(edgeSnapshot["truncated"]),
		"negative_adjacency_complete": true,
	}
	// Capture time is custody metadata, not corpus identity; excluding it keeps
	// a replay over the same frozen index deterministic while retaining the
	// timestamp for temporal promotion binding.
	snapshot["digest"] = "sha256:" + graphCorpusDigestMap(snapshot, "digest", "captured_at")
	incrementalNeededCount := 0
	holdoutIncrementalNeededCount := 0
	for _, row := range cases {
		if !anyToBool(row["hard_negative"]) {
			graphCorpusBindCaseIncrementalControl(row, anyToString(snapshot["digest"]))
			if anyToBool(row["incremental_needed"]) {
				incrementalNeededCount++
				if strings.EqualFold(anyToString(row["split"]), "holdout") {
					holdoutIncrementalNeededCount++
				}
			}
		}
	}
	caseDigest := graphCorpusCaseSetDigest(cases)
	caseCustodyDigest := graphCorpusCaseCustodyDigest(cases)
	topologyCounts := map[string]any{"references": 0, "same_session": 0, "same_topic": 0, "hard_negative": 0}
	for _, row := range cases {
		kind := strings.TrimSpace(anyToString(row["relation"]))
		if anyToBool(row["hard_negative"]) {
			kind = "hard_negative"
		}
		topologyCounts[kind] = anyToInt(topologyCounts[kind], 0) + 1
	}
	controlCost := cloneJSONMap(anyMap(sourceStats["incremental_control_cost"]))
	if anyToString(controlCost["schema_id"]) != retrievalCostObservabilitySchemaID || anyToString(controlCost["authority"]) != retrievalCostObservabilityAuthority || !anyToBool(controlCost["transport_observed"]) || !anyToBool(controlCost["proven_zero"]) || anyToInt(controlCost["control_requests_expected"], 0) != savedRecallGraphCorpusTotalPositiveCases || anyToInt(controlCost["control_requests_observed"], 0) != savedRecallGraphCorpusTotalPositiveCases || anyToInt(controlCost["control_requests_missing"], -1) != 0 || strings.TrimSpace(anyToString(controlCost["digest"])) == "" || !strings.EqualFold(anyToString(controlCost["digest"]), "sha256:"+graphCorpusDigestMap(controlCost, "digest")) {
		return graphCorpusInsufficientArtifact(source, population, required, availableRelations, "graph-disabled control economics receipt is missing, incomplete, or not bound to all 270 controls")
	}
	controlCost["model_calls"] = anyToInt(controlCost["provider_calls"], 0)
	controlCost["edge_reads"] = len(edges)
	controlCost["source_reads"] = "bounded_current_state_index"
	controlCost["cost_truth"] = "preflight-enforced and observed-only; every one of 270 graph-disabled controls is receipt-bound"
	controlCost["digest"] = "sha256:" + graphCorpusDigestMap(controlCost, "digest")
	manifest := map[string]any{
		"schema_id":                             "saved_recall_graph_manifest.v1",
		"corpus_schema_id":                      savedRecallGraphCorpusSchemaID,
		"generator":                             "gateway-go/bounded-current-state-index",
		"seed":                                  seed,
		"development_count":                     len(development),
		"holdout_count":                         len(holdout),
		"case_count":                            len(cases),
		"topology_counts":                       topologyCounts,
		"incremental_needed_case_count":         incrementalNeededCount,
		"holdout_incremental_needed_case_count": holdoutIncrementalNeededCount,
		"source_snapshot_digest":                snapshot["digest"],
		"source_edge_snapshot_digest":           sourceEdgeSnapshotDigest,
		"control_cost_digest":                   controlCost["digest"],
		"control_requests_expected":             savedRecallGraphCorpusTotalPositiveCases,
		"control_requests_observed":             controlCost["control_requests_observed"],
		"control_requests_missing":              controlCost["control_requests_missing"],
		"raw_store_scanned":                     false,
		"network_calls":                         controlCost["network_calls"],
		"external_network_calls":                controlCost["external_network_calls"],
		"provider_calls":                        controlCost["provider_calls"],
		"provider_tokens_observed":              controlCost["provider_tokens"],
		"provider_cost_microusd_observed":       controlCost["provider_cost_microusd"],
	}
	manifest["digest"] = "sha256:" + graphCorpusDigestMap(manifest)
	custody := map[string]any{
		"schema_id":                   "saved_recall_graph_custody.v1",
		"owner":                       savedRecallGraphCorpusOwner,
		"mode":                        "frozen_live_index",
		"synthetic":                   false,
		"sealed_holdout":              true,
		"promotional_claims_allowed":  false,
		"source_snapshot_digest":      snapshot["digest"],
		"snapshot_capture_digest":     graphCorpusSnapshotCaptureDigest(snapshot),
		"case_set_digest":             "sha256:" + caseDigest,
		"case_capture_digest":         "sha256:" + caseCustodyDigest,
		"manifest_digest":             manifest["digest"],
		"control_cost_digest":         controlCost["digest"],
		"source_edge_snapshot_digest": sourceEdgeSnapshotDigest,
		"raw_store_scanned":           false,
		"oracle_separated":            true,
	}
	cost := cloneJSONMap(controlCost)
	return map[string]any{
		"schema_id": savedRecallGraphCorpusSchemaID, "version": savedRecallGraphCorpusVersion,
		"updatedAt": nowUTCISO(), "source": source, "synthetic": false,
		"owner": savedRecallGraphCorpusOwner, "project_scope": "owner_scoped_per_case",
		"case_set_digest": "sha256:" + caseDigest, "snapshot": snapshot,
		"manifest": manifest, "custody": custody, "cost": cost,
		"direct_baseline_binding": graphCorpusDirectBaselineBindingFromStats(sourceStats),
		"population":              population, "required_population": required,
		"topology_counts": topologyCounts, "cases": cases,
		"development_cases": development, "holdout_cases": holdout,
		"incremental_needed_cases": filterGraphCorpusIncrementalCases(holdout),
		"insufficiency_receipt":    nil,
	}
}

func graphCorpusInsufficientArtifact(source string, population, required, availableRelations map[string]int, detail string) map[string]any {
	receipt := graphCorpusInsufficiencyReceipt(source, population, required, availableRelations, detail)
	return map[string]any{
		"schema_id": savedRecallGraphCorpusSchemaID, "version": savedRecallGraphCorpusVersion,
		"source": source, "synthetic": false, "owner": savedRecallGraphCorpusOwner,
		"project_scope": "owner_scoped_per_case", "case_set_digest": "",
		"manifest":                map[string]any{"schema_id": "saved_recall_graph_manifest.v1", "digest": ""},
		"custody":                 map[string]any{"schema_id": "saved_recall_graph_custody.v1", "owner": savedRecallGraphCorpusOwner, "sealed_holdout": false, "promotional_claims_allowed": false},
		"cost":                    map[string]any{"network_calls": 0, "provider_calls": 0, "provider_tokens": 0, "provider_cost_microusd": 0, "cost_truth": "observed_only"},
		"direct_baseline_binding": nil,
		"population":              population, "required_population": required, "topology_counts": availableRelations,
		"cases": []map[string]any{}, "development_cases": []map[string]any{}, "holdout_cases": []map[string]any{},
		"incremental_needed_cases": []map[string]any{}, "insufficiency_receipt": receipt,
	}
}

func filterGraphCorpusIncrementalCases(cases []map[string]any) []map[string]any {
	result := make([]map[string]any, 0)
	for _, row := range cases {
		if anyToBool(row["incremental_needed"]) && !anyToBool(row["hard_negative"]) {
			result = append(result, cloneJSONMap(row))
		}
	}
	return result
}

func loadSavedRecallGraphCorpusConfig() (savedRecallGraphCorpusConfig, error) {
	path := resolveSavedRecallGraphCorpusPath()
	envPath := strings.TrimSpace(os.Getenv("ORCH_RECALL_GRAPH_CORPUS_PATH"))
	if err := prepareOwnerOnlyFile(path, envPath == ""); err != nil {
		return savedRecallGraphCorpusConfig{Path: path}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return savedRecallGraphCorpusConfig{Path: path}, nil
	}
	payload := map[string]any{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return savedRecallGraphCorpusConfig{Path: path}, nil
	}
	rows, _ := payload["cases"].([]any)
	cases := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if mapped := anyMap(row); len(mapped) > 0 {
			cases = append(cases, mapped)
		}
	}
	return savedRecallGraphCorpusConfig{
		Path: path, SchemaID: anyToString(payload["schema_id"]), Version: payload["version"],
		UpdatedAt: payload["updatedAt"], Source: anyToString(payload["source"]), Synthetic: anyToBool(payload["synthetic"]),
		Owner: anyToString(payload["owner"]), ProjectScope: anyToString(payload["project_scope"]),
		CaseSetDigest: anyToString(payload["case_set_digest"]), Manifest: cloneJSONMap(anyMap(payload["manifest"])),
		Snapshot: cloneJSONMap(anyMap(payload["snapshot"])), Custody: cloneJSONMap(anyMap(payload["custody"])),
		Cost: cloneJSONMap(anyMap(payload["cost"])), DirectBaseline: cloneJSONMap(anyMap(payload["direct_baseline_binding"])), Cases: cases,
	}, nil
}

func savedRecallGraphCorpusConfigFromArtifact(path string, payload map[string]any) savedRecallGraphCorpusConfig {
	rows := make([]map[string]any, 0)
	switch raw := payload["cases"].(type) {
	case []map[string]any:
		rows = append(rows, raw...)
	case []any:
		for _, item := range raw {
			if mapped := anyMap(item); len(mapped) > 0 {
				rows = append(rows, mapped)
			}
		}
	}
	return savedRecallGraphCorpusConfig{
		Path: path, SchemaID: anyToString(payload["schema_id"]), Version: payload["version"],
		UpdatedAt: payload["updatedAt"], Source: anyToString(payload["source"]), Synthetic: anyToBool(payload["synthetic"]),
		Owner: anyToString(payload["owner"]), ProjectScope: anyToString(payload["project_scope"]),
		CaseSetDigest: anyToString(payload["case_set_digest"]), Manifest: cloneJSONMap(anyMap(payload["manifest"])),
		Snapshot: cloneJSONMap(anyMap(payload["snapshot"])), Custody: cloneJSONMap(anyMap(payload["custody"])),
		Cost: cloneJSONMap(anyMap(payload["cost"])), DirectBaseline: cloneJSONMap(anyMap(payload["direct_baseline_binding"])), Cases: rows,
	}
}

func saveSavedRecallGraphCorpusArtifact(path string, artifact map[string]any) error {
	raw, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	if err := prepareOwnerOnlyFile(path, strings.TrimSpace(os.Getenv("ORCH_RECALL_GRAPH_CORPUS_PATH")) == ""); err != nil {
		return err
	}
	return writeOwnerOnlyAtomicFile(path, raw, false)
}

// saveSavedRecallGraphCorpusArtifactIfHealthy is the only canonical write
// gate. An insufficient refresh is an attempt receipt, never a replacement
// for the last valid sealed corpus.
func saveSavedRecallGraphCorpusArtifactIfHealthy(path string, artifact map[string]any) (map[string]any, bool, error) {
	health := savedRecallGraphCorpusHealth(path, artifact)
	if !anyToBool(health["valid"]) || !anyToBool(health["benchmark_eligible"]) {
		return health, false, nil
	}
	if err := saveSavedRecallGraphCorpusArtifact(path, artifact); err != nil {
		return health, false, err
	}
	return health, true, nil
}

func saveSavedRecallGraphCorpusAttemptReceipt(path string, artifact map[string]any, health map[string]any) error {
	attempt := map[string]any{
		"schema_id":             "saved_recall_graph_corpus_attempt.v1",
		"status":                "rejected_before_canonical_replace",
		"benchmark_eligible":    false,
		"canonical_path_opaque": ownerOnlyStoreRef("recall_graph_corpus"),
		"case_set_digest":       anyToString(artifact["case_set_digest"]),
		"insufficiency_receipt": cloneJSONMap(anyMap(artifact["insufficiency_receipt"])),
		"health":                health,
		"captured_at":           nowUTCISO(),
	}
	attempt["digest"] = "sha256:" + graphCorpusDigestMap(attempt)
	raw, err := json.MarshalIndent(attempt, "", "  ")
	if err != nil {
		return err
	}
	attemptPath := filepath.Clean(path + ".attempt.json")
	if err := prepareOwnerOnlyFile(attemptPath, false); err != nil {
		return err
	}
	return writeOwnerOnlyAtomicFile(attemptPath, raw, false)
}

func graphCorpusDirectBaselineBinding(cfg recallEvalSavedConfig) map[string]any {
	health := validateSavedRecallEvalCaseSet(cfg)
	baselineCases, baselineSplit := savedRecallDirectBaselineCases(cfg)
	return map[string]any{
		"schema_id":                  cfg.SchemaID,
		"version":                    cfg.Version,
		"k":                          cfg.K,
		"case_set_digest":            cfg.CaseSetDigest,
		"snapshot_digest":            anyToString(anyMap(cfg.Snapshot)["digest"]),
		"benchmark_eligible":         anyToBool(health["benchmark_eligible"]),
		"binding_kind":               "frozen_direct_saved_recall_artifact",
		"evaluation_split":           baselineSplit,
		"evaluation_case_set_digest": "sha256:" + recallEvalCaseSetDigest(baselineCases),
		"evaluation_case_count":      len(baselineCases),
		"evaluation_traffic_class":   "evaluation_holdout",
		"file_names_disclosed":       false,
	}
}

func graphCorpusDirectBaselineBindingFromStats(sourceStats map[string]any) map[string]any {
	binding := cloneJSONMap(anyMap(sourceStats["direct_baseline_binding"]))
	if len(binding) == 0 {
		return nil
	}
	// Only opaque artifact identity belongs in the graph corpus manifest.
	delete(binding, "path")
	delete(binding, "file")
	delete(binding, "seed_file")
	delete(binding, "target_file")
	return binding
}

func savedRecallGraphCorpusHealth(path string, artifact map[string]any) map[string]any {
	cfg := savedRecallGraphCorpusConfigFromArtifact(path, artifact)
	return validateSavedRecallGraphCorpusConfig(cfg)
}

func graphCorpusControlCostValid(cost map[string]any) bool {
	if anyToString(cost["schema_id"]) != retrievalCostObservabilitySchemaID || anyToString(cost["authority"]) != retrievalCostObservabilityAuthority || anyToString(cost["scope"]) != "saved_recall_graph_control_capture" || !anyToBool(cost["transport_observed"]) || !anyToBool(cost["proven_zero"]) {
		return false
	}
	if strings.TrimSpace(anyToString(cost["digest"])) == "" || !strings.EqualFold(anyToString(cost["digest"]), "sha256:"+graphCorpusDigestMap(cost, "digest")) {
		return false
	}
	for _, field := range []string{"network_calls_observed", "local_backend_calls_observed", "external_network_calls_observed", "external_network_zero_proven", "provider_calls_observed", "provider_tokens_observed", "provider_cost_observed", "source_policy_observed", "source_policy_consistent"} {
		if !anyToBool(cost[field]) {
			return false
		}
	}
	if anyToInt(cost["external_network_calls"], -1) != 0 || anyToInt(cost["provider_calls"], -1) != 0 || anyToInt(cost["provider_tokens"], -1) != 0 || anyToInt(cost["provider_cost_microusd"], -1) != 0 {
		return false
	}
	for _, field := range []string{"control_requests_expected", "control_requests_attempted", "control_requests_observed", "observation_expected", "observation_expected_required", "observation_attempted", "observation_observed"} {
		if anyToInt(cost[field], 0) != savedRecallGraphCorpusTotalPositiveCases {
			return false
		}
	}
	if anyToInt(cost["control_requests_missing"], -1) != 0 || anyToInt(cost["observation_missing"], -1) != 0 || anyToInt(cost["observation_failures"], -1) != 0 || anyToString(cost["traffic_class"]) != "evaluation_holdout" {
		return false
	}
	policy := anyMap(cost["source_policy_run"])
	if anyToString(policy["schema_id"]) != retrievalEvaluationSourcePolicySchemaID || anyToInt(policy["version"], 0) != retrievalEvaluationSourcePolicyVersion || anyToString(policy["receipt_kind"]) != "run" || anyToString(policy["authority"]) != retrievalCostObservabilityAuthority || !anyToBool(policy["server_owned"]) || !anyToBool(policy["evaluation_holdout"]) || !anyToBool(policy["eligible"]) || anyToString(policy["allowed_transport"]) != "in_process" || anyToString(policy["provider_policy"]) != "provider_incapable_in_process_only" || !anyToBool(policy["redirect_escape_disabled"]) || anyToInt(policy["expected_case_count"], 0) != savedRecallGraphCorpusTotalPositiveCases || anyToInt(policy["observed_case_count"], 0) != savedRecallGraphCorpusTotalPositiveCases || len(anyToStringSlice(policy["policy_digests"])) == 0 {
		return false
	}
	if strings.TrimSpace(anyToString(policy["digest"])) == "" || !strings.EqualFold(anyToString(policy["digest"]), "sha256:"+graphCorpusDigestMap(policy, "digest")) {
		return false
	}
	identity := anyMap(policy["source_runtime_identity"])
	currentIdentity := contextLatticeBuildIdentity()
	return anyToBool(identity["source_bound"]) && anyToBool(currentIdentity["source_bound"]) && strings.EqualFold(anyToString(identity["source_commit"]), anyToString(currentIdentity["source_commit"])) && strings.EqualFold(anyToString(identity["source_tree"]), anyToString(currentIdentity["source_tree"]))
}

func graphCorpusControlCostMatchesCases(cost map[string]any, cases []map[string]any) bool {
	totals := graphRecallEvaluationTotals{}
	positiveCount := 0
	for _, row := range cases {
		if anyToBool(row["hard_negative"]) {
			continue
		}
		positiveCount++
		control := anyMap(row["incremental_control"])
		graphRecallRecordEconomics(map[string]any{
			"cost_observability": cloneJSONMap(anyMap(control["cost_observability"])),
		}, &totals)
	}
	if positiveCount != savedRecallGraphCorpusTotalPositiveCases {
		return false
	}
	recomputed := graphCorpusControlCostReceipt(totals)
	persistedCore := cloneJSONMap(cost)
	for _, field := range []string{"model_calls", "edge_reads", "source_reads", "cost_truth"} {
		delete(persistedCore, field)
	}
	return graphCorpusDigestMap(persistedCore, "digest") == graphCorpusDigestMap(recomputed, "digest")
}

func validateSavedRecallGraphCorpusConfig(cfg savedRecallGraphCorpusConfig) map[string]any {
	issues := make([]map[string]any, 0)
	add := func(code, detail string) { issues = append(issues, map[string]any{"code": code, "detail": detail}) }
	if cfg.SchemaID != savedRecallGraphCorpusSchemaID || anyToInt(cfg.Version, 0) != savedRecallGraphCorpusVersion {
		add("schema_version_mismatch", "graph corpus is not saved_recall_graph_corpus.v1 version 1")
	}
	if cfg.Synthetic {
		add("synthetic_corpus", "synthetic graph cases cannot support promotion")
	}
	if cfg.Owner != savedRecallGraphCorpusOwner {
		add("owner_mismatch", "graph corpus owner must be gateway-go")
	}
	if strings.TrimSpace(cfg.Source) == "" || cfg.ProjectScope != "owner_scoped_per_case" {
		add("corpus_scope_unbound", "graph corpus source and owner-scoped project contract are required")
	}
	if anyToString(cfg.Manifest["schema_id"]) != "saved_recall_graph_manifest.v1" || anyToString(cfg.Manifest["corpus_schema_id"]) != savedRecallGraphCorpusSchemaID {
		add("manifest_schema_mismatch", "graph manifest must use the closed saved-recall graph schema")
	}
	if anyToString(cfg.Snapshot["schema_id"]) != "saved_recall_graph_snapshot.v1" {
		add("snapshot_schema_mismatch", "graph snapshot must use saved_recall_graph_snapshot.v1")
	}
	if anyToString(cfg.Custody["schema_id"]) != "saved_recall_graph_custody.v1" || anyToString(cfg.Custody["owner"]) != savedRecallGraphCorpusOwner || anyToString(cfg.Custody["mode"]) != "frozen_live_index" || !anyToBool(cfg.Custody["oracle_separated"]) {
		add("custody_schema_mismatch", "graph custody must be the gateway-owned frozen live-index contract with a separated oracle")
	}
	if len(cfg.Cases) != savedRecallGraphCorpusTotalCases {
		add("case_count_mismatch", fmt.Sprintf("expected %d graph cases, got %d", savedRecallGraphCorpusTotalCases, len(cfg.Cases)))
	}
	if strings.TrimSpace(cfg.CaseSetDigest) == "" || !strings.EqualFold(cfg.CaseSetDigest, "sha256:"+graphCorpusCaseSetDigest(cfg.Cases)) {
		add("case_set_digest_mismatch", "case_set_digest does not match the closed case list")
	}
	if strings.TrimSpace(anyToString(cfg.Custody["case_capture_digest"])) == "" || !strings.EqualFold(anyToString(cfg.Custody["case_capture_digest"]), "sha256:"+graphCorpusCaseCustodyDigest(cfg.Cases)) {
		add("case_capture_digest_mismatch", "temporal control custody does not bind the complete captured case list")
	}
	if anyToBool(cfg.Custody["synthetic"]) || !anyToBool(cfg.Custody["sealed_holdout"]) || anyToBool(cfg.Custody["promotional_claims_allowed"]) {
		add("invalid_custody", "holdout custody must be frozen, sealed, non-synthetic, and non-promotional")
	}
	if len(cfg.DirectBaseline) == 0 {
		add("direct_baseline_unbound", "graph corpus must bind the frozen direct saved-recall artifact before promotion")
	} else {
		if anyToString(cfg.DirectBaseline["schema_id"]) != savedRecallEvalV3SchemaID || anyToInt(cfg.DirectBaseline["version"], 0) != savedRecallEvalV3Version {
			add("direct_baseline_schema_mismatch", "direct baseline binding must identify saved_recall_eval_case_set.v3")
		}
		if strings.TrimSpace(anyToString(cfg.DirectBaseline["case_set_digest"])) == "" || strings.TrimSpace(anyToString(cfg.DirectBaseline["snapshot_digest"])) == "" || anyToInt(cfg.DirectBaseline["k"], 0) != defaultRecallEvalK || !anyToBool(cfg.DirectBaseline["benchmark_eligible"]) || anyToString(cfg.DirectBaseline["binding_kind"]) != "frozen_direct_saved_recall_artifact" {
			add("direct_baseline_artifact_unhealthy", "direct baseline binding must carry an eligible frozen case-set digest")
		}
		if split := strings.ToLower(strings.TrimSpace(anyToString(cfg.DirectBaseline["evaluation_split"]))); split != "holdout" && split != "all" {
			add("direct_baseline_evaluation_cohort_unbound", "direct baseline binding must identify the frozen holdout or all-case evaluation cohort")
		}
		if strings.TrimSpace(anyToString(cfg.DirectBaseline["evaluation_case_set_digest"])) == "" || anyToInt(cfg.DirectBaseline["evaluation_case_count"], 0) < 1 {
			add("direct_baseline_evaluation_cohort_unbound", "direct baseline binding must carry the evaluated case-set digest and count")
		}
		if anyToString(cfg.DirectBaseline["evaluation_traffic_class"]) != "evaluation_holdout" {
			add("direct_baseline_traffic_class_invalid", "direct baseline must use the non-synthetic evaluation_holdout traffic class")
		}
		if anyToBool(cfg.DirectBaseline["file_names_disclosed"]) {
			add("direct_baseline_private_metadata_leak", "direct baseline binding must not disclose seed or target file names")
		}
	}
	if anyToBool(cfg.Custody["raw_store_scanned"]) || anyToBool(cfg.Manifest["raw_store_scanned"]) || anyToBool(cfg.Snapshot["raw_store_scanned"]) {
		add("raw_store_scan_forbidden", "graph corpus must use bounded indexes and snapshots, never the hot raw store")
	}
	if strings.TrimSpace(anyToString(cfg.Snapshot["digest"])) == "" || !strings.EqualFold(anyToString(cfg.Snapshot["digest"]), "sha256:"+graphCorpusDigestMap(cfg.Snapshot, "digest", "captured_at")) {
		add("snapshot_digest_mismatch", "graph snapshot digest is not deterministic for the captured bounded snapshot")
	}
	if strings.TrimSpace(anyToString(cfg.Custody["snapshot_capture_digest"])) == "" || !strings.EqualFold(anyToString(cfg.Custody["snapshot_capture_digest"]), graphCorpusSnapshotCaptureDigest(cfg.Snapshot)) {
		add("snapshot_capture_digest_mismatch", "temporal snapshot custody does not bind the complete captured snapshot")
	}
	if capturedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(anyToString(cfg.Snapshot["captured_at"]))); err != nil || capturedAt.IsZero() {
		add("snapshot_capture_time_invalid", "graph snapshot capture time must be a valid nonzero server timestamp")
	}
	currentIdentity := contextLatticeBuildIdentity()
	recordedIdentity := anyMap(cfg.Snapshot["source_runtime_identity"])
	if anyToBool(currentIdentity["source_bound"]) {
		if !anyToBool(recordedIdentity["source_bound"]) || !strings.EqualFold(anyToString(recordedIdentity["source_commit"]), anyToString(currentIdentity["source_commit"])) || !strings.EqualFold(anyToString(recordedIdentity["source_tree"]), anyToString(currentIdentity["source_tree"])) {
			add("source_runtime_identity_mismatch", "graph corpus snapshot was captured by a different or unbound gateway build")
		}
	}
	if anyToString(cfg.Snapshot["edge_snapshot_schema_id"]) != "saved_recall_graph_edge_snapshot.v1" || strings.TrimSpace(anyToString(cfg.Snapshot["edge_snapshot_digest"])) == "" || strings.TrimSpace(anyToString(cfg.Snapshot["source_snapshot_digest"])) == "" || strings.TrimSpace(anyToString(cfg.Snapshot["source_edge_snapshot_digest"])) == "" || anyToInt(cfg.Snapshot["snapshot_version"], 0) != 1 || !anyToBool(cfg.Snapshot["capture_scope_bound"]) || !anyToBool(cfg.Snapshot["edge_snapshot_complete"]) || anyToBool(cfg.Snapshot["edge_snapshot_truncated"]) || !anyToBool(cfg.Snapshot["negative_adjacency_complete"]) || !strings.Contains(strings.ToLower(anyToString(cfg.Snapshot["source_index_mode"])), "current_state") || !anyToBool(anyMap(cfg.Snapshot["source_stats"])["snapshot_stable_during_control_capture"]) {
		add("incomplete_edge_snapshot", "hard-negative eligibility requires a complete, non-truncated bounded adjacency snapshot")
	}
	if strings.TrimSpace(anyToString(cfg.Manifest["digest"])) == "" || !strings.EqualFold(anyToString(cfg.Manifest["digest"]), "sha256:"+graphCorpusDigestMap(cfg.Manifest, "digest")) {
		add("manifest_digest_mismatch", "manifest digest is not deterministic for the persisted manifest")
	}
	if strings.TrimSpace(anyToString(cfg.Custody["manifest_digest"])) != strings.TrimSpace(anyToString(cfg.Manifest["digest"])) {
		add("custody_manifest_mismatch", "custody is not bound to the manifest digest")
	}
	if strings.TrimSpace(anyToString(cfg.Custody["case_set_digest"])) != strings.TrimSpace(cfg.CaseSetDigest) {
		add("custody_case_set_mismatch", "custody is not bound to the case-set digest")
	}
	if !strings.EqualFold(anyToString(cfg.Custody["source_snapshot_digest"]), anyToString(cfg.Snapshot["digest"])) {
		add("custody_snapshot_mismatch", "custody is not bound to the exact semantic graph snapshot")
	}
	if !graphCorpusControlCostValid(cfg.Cost) {
		add("control_cost_invalid", "all 270 graph-disabled controls require complete preflight-enforced provider-free economics receipts")
	}
	if !graphCorpusControlCostMatchesCases(cfg.Cost, cfg.Cases) {
		add("control_cost_case_receipt_mismatch", "aggregate economics must be recomputable from all 270 sealed per-case control receipts")
	}
	if !strings.EqualFold(anyToString(cfg.Manifest["control_cost_digest"]), anyToString(cfg.Cost["digest"])) || !strings.EqualFold(anyToString(cfg.Custody["control_cost_digest"]), anyToString(cfg.Cost["digest"])) || anyToInt(cfg.Manifest["control_requests_expected"], 0) != savedRecallGraphCorpusTotalPositiveCases || anyToInt(cfg.Manifest["control_requests_observed"], 0) != savedRecallGraphCorpusTotalPositiveCases || anyToInt(cfg.Manifest["control_requests_missing"], -1) != 0 {
		add("control_cost_custody_mismatch", "manifest and custody must bind the exact complete 270-control economics receipt")
	}
	if !strings.EqualFold(anyToString(cfg.Custody["source_edge_snapshot_digest"]), anyToString(cfg.Snapshot["source_edge_snapshot_digest"])) || !strings.EqualFold(anyToString(cfg.Manifest["source_edge_snapshot_digest"]), anyToString(cfg.Snapshot["source_edge_snapshot_digest"])) {
		add("source_edge_snapshot_custody_mismatch", "manifest and custody must bind the one semantic source and edge snapshot")
	}
	caseIDs := map[string]struct{}{}
	lineageBySplit := map[string]map[string]struct{}{"development": {}, "holdout": {}}
	lineageOwner := map[string]string{}
	scopeBySplit := map[string]map[string]struct{}{"development": {}, "holdout": {}}
	scopeOwner := map[string]string{}
	projects := map[string]struct{}{}
	agents := map[string]struct{}{}
	sessions := map[string]struct{}{}
	relationCounts := map[string]int{"references": 0, "same_session": 0, "same_topic": 0, "hard_negative": 0}
	holdoutRelationCounts := map[string]int{"references": 0, "same_session": 0, "same_topic": 0, "hard_negative": 0}
	developmentCount := 0
	holdoutCount := 0
	incrementalCount := 0
	holdoutIncrementalCount := 0
	for _, row := range cfg.Cases {
		if anyToString(row["schema_id"]) != savedRecallGraphCorpusSchemaID {
			add("case_schema_mismatch", "every row must use the closed graph corpus schema")
		}
		id := strings.ToLower(strings.TrimSpace(anyToString(row["id"])))
		if id == "" {
			add("missing_case_id", "graph case has no stable id")
		} else if _, exists := caseIDs[id]; exists {
			add("duplicate_case_id", "graph case ids must be unique")
		} else {
			caseIDs[id] = struct{}{}
		}
		split := strings.ToLower(strings.TrimSpace(anyToString(row["split"])))
		if split != "development" && split != "holdout" {
			add("invalid_split", "graph case split must be development or holdout")
		}
		if split == "" {
			split = "invalid"
		}
		if split == "development" {
			developmentCount++
		} else if split == "holdout" {
			holdoutCount++
		}
		if lineageBySplit[split] == nil {
			lineageBySplit[split] = map[string]struct{}{}
		}
		if scopeBySplit[split] == nil {
			scopeBySplit[split] = map[string]struct{}{}
		}
		owner := strings.TrimSpace(anyToString(row["owner"]))
		project := strings.TrimSpace(anyToString(row["project"]))
		provenance := anyMap(row["provenance"])
		if owner != savedRecallGraphCorpusOwner {
			add("case_owner_mismatch", "every graph case must be owned by gateway-go")
		}
		if project == "" || !strings.EqualFold(project, strings.TrimSpace(anyToString(row["project_scope"]))) {
			add("project_scope_mismatch", "every graph case must have a non-empty project bound to its project scope")
		}
		authoritativeOwner := anyToString(cfg.Snapshot["authoritative_owner"])
		authoritativeSource := anyToString(cfg.Snapshot["authoritative_source_index"])
		if authoritativeOwner == "" || authoritativeSource == "" || authoritativeOwner != savedRecallGraphCorpusOwner || authoritativeSource != "gateway_current_state_index" || anyToString(provenance["owner"]) != authoritativeOwner || anyToString(provenance["source_index"]) != authoritativeSource || !anyToBool(provenance["project_scope_verified"]) || anyToBool(provenance["raw_store_scanned"]) || !strings.EqualFold(anyToString(provenance["edge_project"]), project) || !strings.EqualFold(anyToString(provenance["seed_project"]), project) || !strings.EqualFold(anyToString(provenance["target_project"]), project) {
			add("authoritative_provenance_mismatch", "owner and project scope must be proven by the bounded gateway current-state provenance")
		}
		projects[strings.ToLower(project)] = struct{}{}
		family := strings.TrimSpace(anyToString(row["agent_family"]))
		session := strings.TrimSpace(anyToString(row["session_id"]))
		if family == "" {
			add("missing_agent_family", "agent family is required for split and diversity proof")
		} else {
			agents[strings.ToLower(family)] = struct{}{}
		}
		if session == "" {
			add("missing_session", "session id is required for split and diversity proof")
		} else {
			sessions[strings.ToLower(session)] = struct{}{}
		}
		lineage := strings.TrimSpace(anyToString(row["seed_target_lineage"]))
		if lineage == "" {
			add("missing_lineage", "seed-target lineage is required")
		} else if _, exists := lineageBySplit[split][lineage]; exists {
			add("duplicate_lineage", "seed-target lineage is repeated within a split")
		} else {
			lineageBySplit[split][lineage] = struct{}{}
			if priorSplit, exists := lineageOwner[lineage]; exists && priorSplit != split {
				add("lineage_split_overlap", "seed-target lineage crosses development and holdout splits")
			}
			lineageOwner[lineage] = split
		}
		expectedLineage := graphCorpusLineageKey(project, family, session, anyToString(row["time_bucket"]), anyToString(row["seed_memory_id"]), anyToString(row["target_memory_id"]), "")
		if lineage != "" && !strings.EqualFold(lineage, expectedLineage) {
			add("lineage_binding_mismatch", "seed-target lineage does not bind the authoritative project, agent, session, time, seed, and target fields")
		}
		scopeKey := strings.TrimSpace(anyToString(row["split_scope_key"]))
		if scopeKey == "" {
			add("missing_split_scope", "project, agent, session, and time scope key is required for leakage control")
		} else {
			expectedScope := graphCorpusSplitScopeKey(project, family, session, anyToString(row["time_bucket"]))
			if !strings.EqualFold(scopeKey, expectedScope) {
				add("split_scope_binding_mismatch", "split scope key does not bind project, agent family, session, and time bucket")
			}
			if _, exists := scopeBySplit[split][scopeKey]; exists {
				add("duplicate_split_scope", "project/agent/session/time scope is repeated within the same split")
			} else {
				scopeBySplit[split][scopeKey] = struct{}{}
			}
			if priorSplit, exists := scopeOwner[scopeKey]; exists && priorSplit != split {
				add("split_scope_overlap", "project/agent/session/time scope crosses development and holdout splits")
			}
			scopeOwner[scopeKey] = split
		}
		relation := strings.TrimSpace(anyToString(row["relation"]))
		negative := anyToBool(row["hard_negative"])
		if negative {
			relation = "hard_negative"
			if anyToString(row["case_kind"]) != "graph_hard_negative" {
				add("hard_negative_kind_mismatch", "hard-negative rows must use graph_hard_negative")
			}
			if strings.TrimSpace(anyToString(row["edge_id"])) != "" || len(anyToStringSlice(row["graph_expected_relations"])) != 0 {
				add("hard_negative_edge_proof_invalid", "hard negatives must have no allowed edge label")
			}
			if strings.TrimSpace(anyToString(row["negative_proof"])) == "" {
				add("missing_negative_proof", "hard negatives require a bounded absence proof")
			}
			if !anyToBool(row["negative_content_overlap_checked"]) || anyToBool(row["negative_content_overlap"]) || anyToString(row["negative_content_overlap_proof"]) != "gateway_current_state_target_summary_content_anchors_disjoint" || anyToString(row["negative_overlap_authority"]) != "gateway_current_state_index" {
				add("negative_content_overlap_unproven", "hard negatives require exact current-state target-content disjointness proof")
			}
			if !anyToBool(row["negative_semantic_overlap_checked"]) || anyToBool(row["negative_semantic_overlap"]) || anyToString(row["negative_semantic_overlap_proof"]) != "gateway_current_state_topic_semantic_anchors_disjoint" || anyToString(row["negative_overlap_authority"]) != "gateway_current_state_index" {
				add("negative_semantic_overlap_unproven", "hard negatives require exact current-state semantic-anchor disjointness proof")
			}
			if !anyToBool(row["negative_adjacency_complete"]) || strings.TrimSpace(anyToString(row["negative_edge_snapshot_digest"])) == "" || !strings.EqualFold(strings.TrimSpace(anyToString(row["negative_edge_snapshot_digest"])), strings.TrimSpace(anyToString(cfg.Snapshot["edge_snapshot_digest"]))) {
				add("negative_adjacency_incomplete", "hard negatives require a complete edge-snapshot receipt for the exact forbidden pair")
			}
			if len(anyToStringSlice(row["observed_relations"])) != 0 {
				add("negative_observed_relation", "hard-negative absence proof must record no allowed relation for the exact pair")
			}
			forbiddenFiles := normalizeExpectedFileTokens(row["forbidden_graph_files"])
			rawForbiddenFiles := anyToStringSlice(row["forbidden_graph_files"])
			forbiddenIDs := anyToStringSlice(row["forbidden_memory_ids"])
			forbiddenFile := ""
			for file := range forbiddenFiles {
				forbiddenFile = file
				break
			}
			oracle := anyMap(row["negative_oracle"])
			oracleExpected, oracleExpectedPresent := oracle["expected_graph_hit"]
			if len(forbiddenFiles) != 1 || len(rawForbiddenFiles) != 1 || len(forbiddenIDs) != 1 || !oracleExpectedPresent || anyToBool(oracleExpected) || !strings.EqualFold(strings.TrimSpace(anyToString(oracle["forbidden_target"])), strings.TrimSpace(forbiddenIDs[0])) || !strings.EqualFold(strings.Trim(strings.TrimSpace(anyToString(oracle["forbidden_file"])), "/"), strings.Trim(forbiddenFile, "/")) || strings.EqualFold(strings.TrimSpace(anyToString(row["negative_target_memory_id"])), "") || !strings.EqualFold(strings.TrimSpace(anyToString(row["negative_target_memory_id"])), strings.TrimSpace(forbiddenIDs[0])) || strings.EqualFold(strings.TrimSpace(anyToString(row["target_memory_id"])), "") || !strings.EqualFold(strings.TrimSpace(anyToString(row["target_memory_id"])), strings.TrimSpace(forbiddenIDs[0])) {
				add("invalid_negative_oracle", "hard negatives require one explicit forbidden target and expected_graph_hit=false")
			}
			if len(forbiddenFiles) == 1 && len(rawForbiddenFiles) == 1 && len(forbiddenIDs) == 1 {
				rawForbiddenFile := strings.Trim(strings.TrimSpace(strings.ReplaceAll(rawForbiddenFiles[0], "\\", "/")), "/")
				_, canonicalFile, canonicalID, _, canonicalErr := canonicalMemoryID(project + "::" + rawForbiddenFile)
				rawID := strings.TrimSpace(forbiddenIDs[0])
				_, _, normalizedRawID, _, rawIDErr := canonicalMemoryID(rawID)
				if canonicalErr != nil || rawIDErr != nil || rawID != normalizedRawID || canonicalID != rawID || canonicalFile != rawForbiddenFile || anyToString(row["negative_target_memory_id"]) != canonicalID || anyToString(row["target_memory_id"]) != canonicalID || anyToString(row["graph_target_memory_id"]) != canonicalID || anyToString(oracle["forbidden_target"]) != canonicalID || strings.Trim(anyToString(oracle["forbidden_file"]), "/") != canonicalFile {
					add("negative_target_identity_binding_mismatch", "hard-negative forbidden file must bind the exact canonical project::file current memory id")
				}
			}
		} else {
			if normalizeGraphCorpusRelation(relation) == "" {
				add("invalid_topology_relation", "positive graph cases must use references, same_session, or same_topic")
			}
			if anyToString(row["case_kind"]) != "graph_topology_positive" {
				add("positive_kind_mismatch", "topology positives must use graph_topology_positive")
			}
			expectedRelations := anyToStringSlice(row["graph_expected_relations"])
			if len(expectedRelations) != 1 || normalizeGraphCorpusRelation(expectedRelations[0]) != normalizeGraphCorpusRelation(relation) {
				add("positive_relation_oracle_mismatch", "positive relation oracle must exactly match the selected topology relation")
			}
		}
		if _, ok := relationCounts[relation]; ok {
			relationCounts[relation]++
			if split == "holdout" {
				holdoutRelationCounts[relation]++
			}
		}
		if !negative && !anyToBool(row["positive"]) {
			add("positive_label_missing", "topology positives must carry positive=true")
		}
		if negative && anyToBool(row["positive"]) {
			add("negative_label_conflict", "hard negatives cannot carry positive=true")
		}
		seedID := strings.TrimSpace(anyToString(row["seed_memory_id"]))
		targetID := strings.TrimSpace(anyToString(row["target_memory_id"]))
		if seedID == "" || targetID == "" || strings.EqualFold(seedID, targetID) {
			add("invalid_seed_target", "graph cases require distinct canonical seed and target memory ids")
		}
		seedFiles := normalizeExpectedFileTokens(row["expected_files"])
		directSeedFiles := normalizeExpectedFileTokens(row["direct_expected_files"])
		if len(seedFiles) != 1 || len(directSeedFiles) != 1 {
			add("seed_file_oracle_mismatch", "graph cases require one identical direct seed file oracle")
		} else {
			seedFile := ""
			for file := range seedFiles {
				seedFile = file
			}
			if _, exists := directSeedFiles[seedFile]; !exists {
				add("seed_file_oracle_mismatch", "graph cases require one identical direct seed file oracle")
			}
			if _, _, canonicalSeedID, _, err := canonicalMemoryID(project + "::" + seedFile); err != nil || !strings.EqualFold(canonicalSeedID, seedID) || !strings.EqualFold(anyToString(row["graph_seed_memory_id"]), seedID) {
				add("seed_identity_binding_mismatch", "seed file and graph seed identity must bind the canonical project memory id")
			}
		}
		if !negative && len(normalizeExpectedFileTokens(row["graph_expected_files"])) != 1 {
			add("missing_graph_target", "positive graph cases require exactly one graph target")
		}
		if !negative {
			targetFiles := normalizeExpectedFileTokens(row["graph_expected_files"])
			for targetFile := range targetFiles {
				if _, _, canonicalTargetID, _, err := canonicalMemoryID(project + "::" + targetFile); err != nil || !strings.EqualFold(canonicalTargetID, targetID) || !strings.EqualFold(anyToString(row["graph_target_memory_id"]), targetID) {
					add("target_identity_binding_mismatch", "target file and graph target identity must bind the canonical project memory id")
				}
			}
			if anyToString(row["edge_id"]) != deterministicMemoryEdgeID(seedID, normalizeGraphCorpusRelation(relation), targetID) || anyToFloat(row["edge_confidence"]) < 0.95 {
				add("edge_identity_binding_mismatch", "positive edge id, relation, confidence, seed, and target must be deterministically bound")
			}
		}
		if !negative && !graphCorpusIncrementalControlRowValid(row, cfg.Snapshot) {
			add("incremental_control_invalid", "every topology positive must carry a server-owned graph-disabled control bound to its case and snapshot")
		}
		if anyToBool(row["incremental_needed"]) && !negative {
			incrementalCount++
			if split == "holdout" {
				holdoutIncrementalCount++
			}
		}
		query := strings.ToLower(strings.TrimSpace(anyToString(row["query"])))
		if anyToInt(row["k"], 0) != defaultRecallEvalK || anyToInt(row["limit"], 0) != defaultRecallEvalK {
			add("recall_at_5_contract_mismatch", "every closed graph case must fix both k and retrieval limit to 5")
		}
		for _, files := range []map[string]struct{}{normalizeExpectedFileTokens(row["expected_files"]), normalizeExpectedFileTokens(row["graph_expected_files"]), normalizeExpectedFileTokens(row["forbidden_graph_files"])} {
			for file := range files {
				if graphCorpusFileOracleOverlap(query, file) {
					add("query_oracle_leakage", "graph query contains a seed or target file token")
				}
			}
		}
	}
	if len(projects) < savedRecallGraphCorpusMinProjects {
		add("insufficient_projects", fmt.Sprintf("need at least %d projects, got %d", savedRecallGraphCorpusMinProjects, len(projects)))
	}
	if len(agents) < savedRecallGraphCorpusMinAgentFamilies {
		add("insufficient_agent_families", fmt.Sprintf("need at least %d agent families, got %d", savedRecallGraphCorpusMinAgentFamilies, len(agents)))
	}
	if len(sessions) < savedRecallGraphCorpusMinSessions {
		add("insufficient_sessions", fmt.Sprintf("need at least %d sessions, got %d", savedRecallGraphCorpusMinSessions, len(sessions)))
	}
	for _, relation := range savedRecallGraphRelations {
		developmentRelationCount := relationCounts[relation] - holdoutRelationCounts[relation]
		if relationCounts[relation] != savedRecallGraphCorpusRelationDevelopmentCases+savedRecallGraphCorpusRelationHoldoutCases || developmentRelationCount != savedRecallGraphCorpusRelationDevelopmentCases || holdoutRelationCounts[relation] != savedRecallGraphCorpusRelationHoldoutCases {
			add("topology_quota_mismatch", fmt.Sprintf("relation %s requires 60 development and 30 holdout positives", relation))
		}
	}
	if relationCounts["hard_negative"] != savedRecallGraphCorpusTotalHardNegatives || holdoutRelationCounts["hard_negative"] != savedRecallGraphCorpusHardNegatives {
		add("hard_negative_quota_mismatch", fmt.Sprintf("requires %d development and %d holdout hard negatives", savedRecallGraphCorpusDevelopmentHardNegatives, savedRecallGraphCorpusHardNegatives))
	}
	if anyToInt(cfg.Manifest["incremental_needed_case_count"], -1) != incrementalCount || anyToInt(cfg.Manifest["holdout_incremental_needed_case_count"], -1) != holdoutIncrementalCount {
		add("incremental_manifest_mismatch", "manifest incremental-needed counts must bind the sealed pre-treatment labels")
	}
	manifestTopology := anyMap(cfg.Manifest["topology_counts"])
	if developmentCount != savedRecallGraphCorpusDevelopmentCases || holdoutCount != savedRecallGraphCorpusHoldoutCases || anyToInt(cfg.Manifest["development_count"], -1) != developmentCount || anyToInt(cfg.Manifest["holdout_count"], -1) != holdoutCount || anyToInt(cfg.Manifest["case_count"], -1) != len(cfg.Cases) {
		add("split_manifest_mismatch", "manifest must bind the exact 200 development and 100 holdout rows")
	}
	for relation, count := range relationCounts {
		if anyToInt(manifestTopology[relation], -1) != count {
			add("topology_manifest_mismatch", "manifest topology counts must equal the closed case rows")
			break
		}
	}
	if incrementalCount < savedRecallGraphCorpusMinIncremental {
		add("incremental_denominator_below_30", "closed corpus must report at least 30 incremental-needed positive cases")
	}
	if holdoutIncrementalCount < savedRecallGraphCorpusMinIncremental {
		add("holdout_incremental_denominator_below_30", "sealed holdout must report at least 30 pre-treatment graph-needed positive cases")
	}
	status := "healthy"
	if len(issues) > 0 {
		status = "invalid"
	}
	return map[string]any{
		"valid":                      len(issues) == 0,
		"benchmark_eligible":         len(issues) == 0 && len(cfg.Cases) == savedRecallGraphCorpusTotalCases,
		"status":                     status,
		"schema_id":                  cfg.SchemaID,
		"version":                    cfg.Version,
		"case_count":                 len(cfg.Cases),
		"development_count":          developmentCount,
		"holdout_count":              holdoutCount,
		"topology_cases":             relationCounts,
		"holdout_topology":           holdoutRelationCounts,
		"incremental_needed":         incrementalCount,
		"holdout_incremental_needed": holdoutIncrementalCount,
		"population":                 map[string]int{"projects": len(projects), "agent_families": len(agents), "sessions": len(sessions)},
		"case_set_digest":            cfg.CaseSetDigest,
		"manifest_digest":            anyToString(cfg.Manifest["digest"]),
		"custody":                    cloneJSONMap(cfg.Custody),
		"issues":                     issues,
	}
}

func graphRecallCorpusValidationReceipt(cfg savedRecallGraphCorpusConfig, health map[string]any) map[string]any {
	receipt := map[string]any{
		"schema_id":               "saved_recall_graph_validation.v1",
		"version":                 1,
		"authority":               savedRecallGraphCorpusOwner,
		"server_owned":            true,
		"valid":                   anyToBool(health["valid"]),
		"benchmark_eligible":      anyToBool(health["benchmark_eligible"]),
		"case_count":              len(cfg.Cases),
		"case_set_digest":         cfg.CaseSetDigest,
		"manifest_digest":         anyToString(cfg.Manifest["digest"]),
		"custody_case_set_digest": anyToString(cfg.Custody["case_set_digest"]),
		"captured_at":             nowUTCISO(),
	}
	receipt["digest"] = "sha256:" + graphCorpusDigestMap(receipt)
	return receipt
}

func (s *server) currentSavedRecallGraphSourceEdgeDigest(ctx context.Context, snapshot map[string]any) (string, error) {
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() {
		return "", errors.New("memory store unavailable for graph snapshot verification")
	}
	project := strings.TrimSpace(anyToString(snapshot["capture_project"]))
	topicPrefix := normalizeTopicPathLoose(anyToString(snapshot["capture_topic_prefix"]))
	candidates, _, stats := s.recallEvalIndexedCandidates(ctx, project, topicPrefix, savedRecallGraphCorpusMaxSourceDocs)
	if !anyToBool(stats["bounded"]) || !anyToBool(stats["index_integrity"]) || anyToBool(stats["context_cancelled"]) {
		return "", errors.New("current-state source snapshot is unavailable or integrity-invalid")
	}
	edges, complete, err := s.memoryStore.listMemoryEdgesComplete(ctx, memoryEdgeQuery{Project: project}, savedRecallGraphCorpusMaxSnapshotEdges)
	if err != nil {
		return "", err
	}
	if !complete {
		return "", errors.New("current edge snapshot is incomplete")
	}
	candidates = graphCorpusMergeCandidates(candidates, graphCorpusHydrateEdgeEndpoints(ctx, s.memoryStore, edges, project, topicPrefix))
	return graphCorpusSourceEdgeSnapshotDigest(candidates, edges), nil
}

func (s *server) buildSavedRecallGraphCorpus(ctx context.Context, project, topicPrefix, seed string) map[string]any {
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() {
		return graphCorpusInsufficientArtifact("memory_store_unavailable", map[string]int{}, graphCorpusRequiredDimensions(), map[string]int{}, "bounded graph corpus generation requires the enabled gateway-go memory store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	project = strings.TrimSpace(project)
	topicPrefix = normalizeTopicPathLoose(topicPrefix)
	candidates, source, stats := s.recallEvalIndexedCandidates(ctx, project, topicPrefix, savedRecallGraphCorpusMaxSourceDocs)
	// Freeze the bounded adjacency index first. Hydrating endpoint identities
	// from that exact snapshot closes the false-negative gap where a target is
	// outside the ordinary 20k bottom-K document sample.
	edges, edgeSnapshotComplete, edgeErr := s.memoryStore.listMemoryEdgesComplete(ctx, memoryEdgeQuery{Project: project}, savedRecallGraphCorpusMaxSnapshotEdges)
	if edgeErr != nil {
		edgeSnapshotComplete = false
	}
	endpointCandidates := graphCorpusHydrateEdgeEndpoints(ctx, s.memoryStore, edges, project, topicPrefix)
	candidateCountBeforeEndpointHydration := len(candidates)
	candidates = graphCorpusMergeCandidates(candidates, endpointCandidates)
	completeSeedIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		_, _, seedID, _, err := canonicalMemoryID(candidate.doc.Project + "::" + candidate.doc.FileName)
		if err == nil && edgeSnapshotComplete {
			completeSeedIDs = append(completeSeedIDs, seedID)
		}
	}
	directCfg, _ := loadSavedRecallEvalConfig()
	if stats == nil {
		stats = map[string]any{}
	}
	stats = cloneJSONMap(stats)
	stats["runtime_identity"] = contextLatticeBuildIdentity()
	stats["captured_at"] = nowUTCISO()
	stats["capture_project"] = project
	stats["capture_topic_prefix"] = topicPrefix
	stats["candidate_count_before_endpoint_hydration"] = candidateCountBeforeEndpointHydration
	stats["edge_endpoint_hydration_count"] = len(endpointCandidates)
	stats["edge_endpoint_hydration_complete"] = edgeSnapshotComplete
	edgeSnapshot := map[string]any{
		"schema_id": "saved_recall_graph_edge_snapshot.v1", "complete": edgeSnapshotComplete,
		"truncated": !edgeSnapshotComplete, "continuation_complete": edgeSnapshotComplete,
		"candidate_count": len(candidates), "complete_seed_count": len(completeSeedIDs), "endpoint_hydration_count": len(endpointCandidates),
	}
	edgeSnapshot["edge_digest"] = "sha256:" + graphCorpusEdgesDigest(edges)
	edgeSnapshot["digest"] = "sha256:" + graphCorpusDigestMap(edgeSnapshot)
	stats["edge_snapshot"] = edgeSnapshot
	stats["source_snapshot_digest"] = "sha256:" + graphCorpusCandidatesDigest(candidates)
	stats["source_edge_snapshot_digest"] = graphCorpusSourceEdgeSnapshotDigest(candidates, edges)
	stats["complete_seed_ids"] = completeSeedIDs
	stats["direct_baseline_binding"] = graphCorpusDirectBaselineBinding(directCfg)
	preRecords, availableRelations := graphCorpusBuildRecords(candidates, edges)
	if graphCorpusEnoughDimensions(preRecords) && availableRelations["references"] >= 90 && availableRelations["same_session"] >= 90 && availableRelations["same_topic"] >= 90 {
		controls, controlCost, controlDetail := s.captureSavedRecallGraphIncrementalControls(ctx, preRecords, anyToString(stats["source_edge_snapshot_digest"]), anyToString(stats["source_snapshot_digest"]), anyToString(edgeSnapshot["digest"]))
		stats["incremental_control_cost"] = controlCost
		stats["incremental_control_capture"] = map[string]any{"authority": savedRecallGraphIncrementalControlAuthority, "expected": savedRecallGraphCorpusTotalPositiveCases, "observed": len(controls), "detail": controlDetail}
		if controlDetail == "" {
			verificationCandidates, _, verificationStats := s.recallEvalIndexedCandidates(ctx, project, topicPrefix, savedRecallGraphCorpusMaxSourceDocs)
			verificationEdges, verificationComplete, verificationErr := s.memoryStore.listMemoryEdgesComplete(ctx, memoryEdgeQuery{Project: project}, savedRecallGraphCorpusMaxSnapshotEdges)
			if verificationErr == nil && verificationComplete && anyToBool(verificationStats["index_integrity"]) {
				verificationCandidates = graphCorpusMergeCandidates(verificationCandidates, graphCorpusHydrateEdgeEndpoints(ctx, s.memoryStore, verificationEdges, project, topicPrefix))
			}
			if verificationErr != nil || !verificationComplete || !strings.EqualFold(graphCorpusSourceEdgeSnapshotDigest(verificationCandidates, verificationEdges), anyToString(stats["source_edge_snapshot_digest"])) {
				return graphCorpusInsufficientArtifact(source, graphCorpusPopulationDimensions(preRecords), graphCorpusRequiredDimensions(), availableRelations, "source or edge state changed while the paired graph control cohort was captured")
			}
			stats["snapshot_stable_during_control_capture"] = true
			stats["incremental_controls"] = controls
		}
	}
	return buildSavedRecallGraphCorpusFromCandidates(candidates, edges, seed, source, stats)
}
