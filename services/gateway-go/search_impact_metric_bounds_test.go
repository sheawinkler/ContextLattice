package main

import (
	"math"
	"testing"
)

func TestSearchImpactComparatorMetricValueValidRateBounds(t *testing.T) {
	rates := []string{
		"decision_impact_recall_at_5", "decision_impact_ndcg_at_5", "mrr", "numeric_exactness",
		"citation_coverage", "citation_exactness", "safety_failure_rate",
	}
	values := []struct {
		name  string
		value float64
		want  bool
	}{
		{"zero", 0, true},
		{"one", 1, true},
		{"midpoint", 0.5, true},
		{"below_zero", math.Nextafter(0, -1), false},
		{"above_one", math.Nextafter(1, 2), false},
		{"nan", math.NaN(), false},
		{"positive_infinity", math.Inf(1), false},
		{"negative_infinity", math.Inf(-1), false},
	}
	for _, metric := range rates {
		for _, value := range values {
			t.Run(metric+"/"+value.name, func(t *testing.T) {
				if got := searchImpactComparatorMetricValueValid(metric, value.value); got != value.want {
					t.Fatalf("valid(%q, %v)=%v, want %v", metric, value.value, got, value.want)
				}
			})
		}
	}
}

func TestSearchImpactComparatorMetricValueValidLatencyBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		value float64
		want  bool
	}{
		{"zero", 0, true},
		{"positive", 123.45, true},
		{"below_zero", math.Nextafter(0, -1), false},
		{"nan", math.NaN(), false},
		{"positive_infinity", math.Inf(1), false},
		{"negative_infinity", math.Inf(-1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := searchImpactComparatorMetricValueValid("p95_latency_ms", test.value); got != test.want {
				t.Fatalf("valid(latency, %v)=%v, want %v", test.value, got, test.want)
			}
		})
	}
}

func TestSearchImpactComparatorMetricValueValidRejectsUnknownMetric(t *testing.T) {
	if searchImpactComparatorMetricValueValid("unknown", 0.5) {
		t.Fatal("unknown comparator metric was accepted")
	}
}
