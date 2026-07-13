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
	if commercialTruthProductVersion != "3.17.0" || commercialTruthStableTag != "v3.17.0" || commercialTruthReleaseTrain != "3.17" {
		t.Fatalf(
			"unexpected generated release truth: version=%q tag=%q train=%q",
			commercialTruthProductVersion,
			commercialTruthStableTag,
			commercialTruthReleaseTrain,
		)
	}
	if len(commercialTruthProtectedPaidRoutes) != 13 {
		t.Fatalf("protected route count=%d, want 13", len(commercialTruthProtectedPaidRoutes))
	}
	enterprise := commercialTruthPlans["enterprise"]
	if !enterprise.CustomPricing || enterprise.SelfServePurchasable || enterprise.MonthlyUSD != nil || enterprise.AnnualUSD != nil {
		t.Fatalf("enterprise pricing must be custom and non-self-serve: %#v", enterprise)
	}
}
