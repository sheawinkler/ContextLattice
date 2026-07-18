package main

import "net/http"

const (
	frontierT4RetrievalGovernanceContractID = "frontier_t4_retrieval_governance.v1"

	frontierT4RetrievalReceiptGovernancePath = "/memory/retrieval/receipts/governance"
	frontierT4CausalBridgeGovernancePath     = "/memory/causal-bridges/governance"
	frontierT4CounterfactualEvalPath         = "/memory/retrieval/ablation/operations"
	frontierT4EvidenceReputationPath         = "/memory/evidence-reputation/activation"
	frontierT4RetrievalRegressionPath        = "/memory/recall/regressions/operations"
	frontierT4DefenseOperationsPath          = "/memory/trust/defense/operations"
)

func frontierT4PublicGovernanceUnavailable(w http.ResponseWriter, feature string) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"ok":         false,
		"schema_id":  frontierT4RetrievalGovernanceContractID,
		"feature_id": feature,
		"error":      "premium_retrieval_governance_unavailable",
		"detail":     "Local retrieval trust and receipts remain available; governed workspace operations require the paid runtime artifact.",
	})
}

func frontierT4RetrievalReceiptGovernanceRoute(_ *server, w http.ResponseWriter, _ *http.Request) {
	frontierT4PublicGovernanceUnavailable(w, "frontier_retrieval_receipt_governance")
}

func frontierT4CausalBridgeGovernanceRoute(_ *server, w http.ResponseWriter, _ *http.Request) {
	frontierT4PublicGovernanceUnavailable(w, "frontier_causal_bridge_governance")
}

func frontierT4CounterfactualEvalRoute(_ *server, w http.ResponseWriter, _ *http.Request) {
	frontierT4PublicGovernanceUnavailable(w, "frontier_continuous_counterfactual_eval")
}

func frontierT4EvidenceReputationRoute(_ *server, w http.ResponseWriter, _ *http.Request) {
	frontierT4PublicGovernanceUnavailable(w, "frontier_evidence_reputation_activation")
}

func frontierT4RetrievalRegressionRoute(_ *server, w http.ResponseWriter, _ *http.Request) {
	frontierT4PublicGovernanceUnavailable(w, "frontier_continuous_retrieval_regression")
}

func frontierT4DefenseOperationsRoute(_ *server, w http.ResponseWriter, _ *http.Request) {
	frontierT4PublicGovernanceUnavailable(w, "frontier_adversarial_defense_operations")
}
