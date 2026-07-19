package main

import "testing"

func TestCommercialTruthGeneratedPlanNormalization(t *testing.T) {
	tests := map[string]string{
		"free":                 "free",
		"starter":              "starter",
		"team":                 "team",
		"operator":             "operator",
		"enterprise":           "enterprise",
		"pro":                  "operator",
		"business":             "team",
		"enterprise-seats-100": "enterprise",
		"ENTERPRISE-SEATS-900": "enterprise",
		"unknown":              "",
	}
	for input, expected := range tests {
		if actual := normalizeCommercialTruthPlanID(input); actual != expected {
			t.Fatalf("normalizeCommercialTruthPlanID(%q)=%q, want %q", input, actual, expected)
		}
	}
}

func TestCommercialTruthGeneratedReleaseAndRoutes(t *testing.T) {
	if commercialTruthProductVersion != "3.25.0" || commercialTruthStableTag != "v3.25.0" || commercialTruthReleaseTrain != "3.25" {
		t.Fatalf(
			"unexpected generated release truth: version=%q tag=%q train=%q",
			commercialTruthProductVersion,
			commercialTruthStableTag,
			commercialTruthReleaseTrain,
		)
	}
	protectedRoutes := make(map[string]struct{}, len(commercialTruthProtectedPaidRoutes))
	for _, path := range commercialTruthProtectedPaidRoutes {
		if _, duplicate := protectedRoutes[path]; duplicate {
			t.Fatalf("duplicate protected paid route: %s", path)
		}
		protectedRoutes[path] = struct{}{}
	}
	for path := range commercialTruthPaidFeatureRouteRequirements {
		if _, protected := protectedRoutes[path]; !protected {
			t.Fatalf("feature-bound route is not protected: %s", path)
		}
	}
	wantFeatures := map[string]string{
		"/memory/continuity/automation":               "frontier_semantic_continuity_automation",
		"/memory/objectives/shared":                   "frontier_shared_objective_graph",
		"/memory/decision-changes/shared":             "frontier_shared_decision_provenance",
		"/telemetry/utility/analytics":                "frontier_utility_analytics",
		"/telemetry/utility/policy/evaluate":          "frontier_verified_efficiency_operations",
		"/memory/agent-fit/steering/governance":       "frontier_agent_fit_governance",
		"/memory/agent-fit/profile/governance":        "frontier_agent_fit_governance",
		"/memory/agent-fit/context-prep/governance":   "frontier_agent_fit_governance",
		"/memory/agent-fit/selection/activation":      "frontier_agent_fit_governance",
		"/memory/skills/foundry/evolution/governance": "frontier_skill_evolution_governance",
	}
	for path, featureID := range wantFeatures {
		if got := commercialTruthPaidRouteRequiredFeature(path); got != featureID {
			t.Fatalf("paid route feature path=%s got=%s want=%s", path, got, featureID)
		}
	}
	for _, path := range []string{
		"/memory/agent-packet/shared",
		"/memory/agent-proof-timeline/shared",
		"/memory/agent-proof-timeline/shared/lifecycle",
	} {
		if got := commercialTruthPaidRouteRequiredFeature(path); got != "" {
			t.Fatalf("OSS runtime generated an unavailable T2 paid route %s: %s", path, got)
		}
	}
	if *commercialTruthPlans["team"].Limits.IncludedSeats != 5 || *commercialTruthPlans["starter"].Limits.IncludedSeats != 1 {
		t.Fatalf("generated included-seat semantics drifted")
	}
	enterprise := commercialTruthPlans["enterprise"]
	if !enterprise.CustomPricing || enterprise.SelfServePurchasable || enterprise.MonthlyUSD != nil || enterprise.AnnualUSD != nil {
		t.Fatalf("enterprise pricing must be custom and non-self-serve: %#v", enterprise)
	}
}
