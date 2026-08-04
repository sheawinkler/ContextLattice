package main

import "math"

func searchImpactComparatorMetricValueValid(metric string, value float64) bool {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return false
	}
	switch metric {
	case "decision_impact_recall_at_5", "decision_impact_ndcg_at_5", "mrr", "numeric_exactness", "citation_coverage", "citation_exactness", "safety_failure_rate":
		return value >= 0 && value <= 1
	case "p95_latency_ms":
		return value >= 0
	default:
		return false
	}
}
