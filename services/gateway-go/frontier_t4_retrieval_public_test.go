package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFrontierT4PublicGovernanceRoutesAreDiscoveryOnly(t *testing.T) {
	routes := []struct {
		path    string
		feature string
		handler func(*server, http.ResponseWriter, *http.Request)
	}{
		{frontierT4RetrievalReceiptGovernancePath, "frontier_retrieval_receipt_governance", frontierT4RetrievalReceiptGovernanceRoute},
		{frontierT4CausalBridgeGovernancePath, "frontier_causal_bridge_governance", frontierT4CausalBridgeGovernanceRoute},
		{frontierT4CounterfactualEvalPath, "frontier_continuous_counterfactual_eval", frontierT4CounterfactualEvalRoute},
		{frontierT4EvidenceReputationPath, "frontier_evidence_reputation_activation", frontierT4EvidenceReputationRoute},
		{frontierT4RetrievalRegressionPath, "frontier_continuous_retrieval_regression", frontierT4RetrievalRegressionRoute},
		{frontierT4DefenseOperationsPath, "frontier_adversarial_defense_operations", frontierT4DefenseOperationsRoute},
	}

	for _, route := range routes {
		t.Run(route.feature, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest("GET", route.path, nil)
			route.handler(nil, recorder, request)
			if recorder.Code != 404 {
				t.Fatalf("status=%d; want 404", recorder.Code)
			}
			payload := map[string]any{}
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["schema_id"] != frontierT4RetrievalGovernanceContractID || payload["feature_id"] != route.feature {
				t.Fatalf("unexpected discovery receipt: %#v", payload)
			}
			if payload["error"] != "premium_retrieval_governance_unavailable" {
				t.Fatalf("unexpected error: %#v", payload["error"])
			}
		})
	}
}
