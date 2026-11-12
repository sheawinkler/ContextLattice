package main

import (
	"sort"
	"strings"
)

// Project existing v2 proof only; do not infer new claims or fetch more evidence.
func agentPacketProofEvidence(claims []any) []any {
	out := []any{}
	seen := map[string]bool{}
	for _, raw := range claims[:minInt(len(claims), 64)] {
		claim := anyMap(raw)
		id := strings.TrimSpace(anyToString(claim["claim_id"]))
		statement := strings.TrimSpace(anyToString(claim["statement"]))
		status := anyToString(claim["proof_status"])
		if id == "" || len(id) > 160 || seen[id] || statement == "" ||
			(status != "supported" && status != "supported_with_limits" && status != "contested") {
			continue
		}
		support, supportOmitted := agentPacketProofReferences(contextPackAnyList(claim["support"]), 2, false)
		if len(support) == 0 {
			continue
		}
		opposition, oppositionOmitted := agentPacketProofReferences(contextPackAnyList(claim["opposition"]), 2, true)
		missing := []any{}
		missingTruncated := false
		for _, value := range anyToStringList(claim["missing_proof"], 3) {
			missing = append(missing, clipText(value, 96))
			missingTruncated = missingTruncated || len(value) > 96
		}
		proof := map[string]any{"claim_id": id, "status": status, "support": support, "opposition": opposition,
			"missing_proof": missing, "support_omitted": supportOmitted, "opposition_omitted": oppositionOmitted,
			"missing_proof_omitted": maxInt(0, len(contextPackAnyList(claim["missing_proof"]))-len(missing))}
		if missingTruncated {
			proof["detail_omitted"] = true
		}
		if temporal := anyToString(anyMap(claim["temporal"])["state"]); temporal != "" {
			proof["temporal_state"] = clipText(temporal, 48)
		}
		out = append(out, map[string]any{"kind": clipText(anyToString(claim["claim_type"]), 48), "text": statement,
			"score":    anyToFloat(anyMap(claim["confidence"])["final"]),
			"citation": firstNonEmptyStrings(anyToString(anyMap(support[0])["ref_id"]), anyToString(anyMap(support[0])["ref_id_sha256"])), "_packet_proof": proof})
		seen[id] = true
	}
	// Reserve material counterevidence before expendable supported summaries.
	sort.SliceStable(out, func(i, j int) bool {
		return agentPacketProofPriority(anyMap(out[i])) < agentPacketProofPriority(anyMap(out[j]))
	})
	return out
}

func agentPacketProofPriority(item map[string]any) int {
	proof := anyMap(item["_packet_proof"])
	if proof["status"] == "contested" {
		return 0
	}
	for _, raw := range contextPackAnyList(proof["opposition"]) {
		if agentPacketProofReferencePriority(anyMap(raw)) == 0 {
			return 0
		}
	}
	if len(contextPackAnyList(proof["missing_proof"])) > 0 {
		return 1
	}
	return 2
}

func agentPacketProofReferences(values []any, limit int, prioritizeCurrent bool) ([]any, int) {
	out := []any{}
	bounded := append([]any{}, values[:minInt(len(values), 16)]...)
	if prioritizeCurrent {
		sort.SliceStable(bounded, func(i, j int) bool {
			return agentPacketProofReferencePriority(anyMap(bounded[i])) < agentPacketProofReferencePriority(anyMap(bounded[j]))
		})
	}
	for _, raw := range bounded {
		if len(out) >= limit {
			break
		}
		ref := anyMap(raw)
		id := strings.TrimSpace(anyToString(ref["ref_id"]))
		if id == "" {
			continue
		}
		row := map[string]any{}
		if len(id) <= 160 {
			row["ref_id"] = id
		} else {
			row["ref_id_sha256"] = "sha256:" + sha256Hex(id)
			row["detail_omitted"] = true
			for _, field := range []struct {
				name  string
				limit int
			}{{"project", 128}, {"file", 240}, {"source", 48}} {
				value := anyToString(ref[field.name])
				if value != "" && len(value) <= field.limit {
					row[field.name] = value
				}
			}
		}
		for _, field := range []struct {
			name  string
			limit int
		}{{"kind", 48}, {"status", 48}, {"influence", 48}, {"content_hash", 72}, {"memory_id", 120}, {"excerpt", 96},
			{"role", 48}, {"observed_at", 40}, {"valid_from", 40}, {"valid_to", 40}} {
			value := strings.TrimSpace(anyToString(ref[field.name]))
			if field.name == "excerpt" && value == "" {
				value = strings.TrimSpace(anyToString(ref["statement"]))
			}
			if value == "" {
				continue
			}
			if (field.name == "content_hash" || field.name == "memory_id") && len(value) > field.limit {
				row["detail_omitted"] = true
				continue
			}
			row[field.name] = clipText(value, field.limit)
			if len(value) > field.limit {
				row["detail_omitted"] = true
			}
		}
		if historical, ok := ref["historical"].(bool); ok {
			row["historical"] = historical
		}
		for _, field := range []string{"citation", "project", "file", "source", "topic_path", "timestamp", "confidence"} {
			if _, present := ref[field]; present {
				if _, copied := row[field]; !copied {
					row["detail_omitted"] = true
				}
			}
		}
		out = append(out, row)
	}
	return out, len(values) - len(out)
}

func agentPacketProofReferencePriority(ref map[string]any) int {
	status := anyToString(ref["status"])
	if anyToBool(ref["historical"]) || ref["influence"] == "none" || ref["role"] == "historical_opposition" || ref["role"] == "temporally_invalid_opposition" {
		return 2
	}
	if status == "current" || status == "active" {
		return 0
	}
	return 1
}

func updateAgentPacketProofCoverage(packet map[string]any) {
	provenance := anyMap(packet["provenance"])
	if _, ok := provenance["proof_claim_candidates"]; !ok {
		return
	}
	returned := 0
	for _, raw := range contextPackAnyList(packet["evidence"]) {
		if len(anyMap(anyMap(raw)["proof"])) > 0 {
			returned++
		}
	}
	provenance["proof_claims_returned"] = returned
	provenance["proof_claims_omitted"] = maxInt(0, anyToInt(provenance["proof_claim_candidates"], 0)-returned)
	provenance["citation_count"] = agentPacketCitationCount(contextPackAnyList(packet["evidence"]))
	packet["provenance"] = provenance
}

func shrinkAgentPacketProofDetails(item map[string]any) {
	proof := anyMap(item["proof"])
	if len(proof) == 0 {
		return
	}
	for _, role := range []string{"support", "opposition"} {
		refs := contextPackAnyList(proof[role])
		if len(refs) > 1 {
			proof[role+"_omitted"] = anyToInt(proof[role+"_omitted"], 0) + len(refs) - 1
			refs = refs[:1]
		}
		for _, raw := range refs {
			delete(anyMap(raw), "excerpt")
		}
		proof[role] = refs
	}
	missing := []any{}
	previousMissing := contextPackAnyList(proof["missing_proof"])
	for _, value := range anyToStringList(proof["missing_proof"], 2) {
		missing = append(missing, clipText(value, 64))
	}
	proof["missing_proof_omitted"] = anyToInt(proof["missing_proof_omitted"], 0) + len(previousMissing) - len(missing)
	proof["missing_proof"] = missing
	proof["detail_omitted"] = true
	item["proof"] = proof
}

func minimizeAgentPacketProofDetails(item map[string]any) {
	if len(anyMap(item["proof"])) == 0 {
		return
	}
	shrinkAgentPacketProofDetails(item)
	proof := anyMap(item["proof"])
	previous := contextPackAnyList(proof["missing_proof"])
	missing := []any{}
	for _, value := range anyToStringList(proof["missing_proof"], 1) {
		missing = append(missing, clipText(value, 24))
	}
	proof["missing_proof_omitted"] = anyToInt(proof["missing_proof_omitted"], 0) + len(previous) - len(missing)
	proof["missing_proof"] = missing
	item["text"] = clipText(anyToString(item["text"]), 64)
}

func omitUnboundedAgentPacketProof(packet map[string]any, hard int) {
	hasProof := false
	for _, raw := range contextPackAnyList(packet["evidence"]) {
		hasProof = hasProof || len(anyMap(anyMap(raw)["proof"])) > 0
	}
	if !hasProof || contextPackCountAnyTokens(attachAgentPacketFormatContract(packet)).Tokens <= hard {
		return
	}
	packet["evidence"] = []any{}
	packet["decision_gate"] = compactAgentPacketDecisionGate(map[string]any{"decision": "abstain", "refusal": true,
		"reasons": []any{"proof_references_exceed_transport_budget"}, "policy": "no material action without the omitted proof; request full synthesis proof or a larger budget"})
	uncertainty := cloneAnyMap(anyMap(packet["uncertainty"]))
	uncertainty["status"], uncertainty["evidence_alignment"] = "insufficient_evidence", 0.0
	uncertainty["reasons"] = []any{"proof_references_exceed_transport_budget"}
	packet["uncertainty"] = uncertainty
	packet["prompt"] = clipText(agentPacketPrompt(anyToString(packet["query"]), anyToString(packet["surface"]), "insufficient_evidence", "abstain"), 420)
	packet["next_actions"] = []any{}
	packet["warnings"] = []any{"Proof references exceeded the token ceiling; no actionable evidence was emitted. Request full synthesis proof."}
}
