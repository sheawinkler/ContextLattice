package main

import (
	"sort"
	"strings"
)

const (
	recallResponseMaxCompactBytes    = 16000
	recallResponseMaxCompactTokens   = 4000
	recallResponseMaxOracleRefs      = 10
	recallResponseMaxProofCandidates = recallResponseMaxEvidence + recallResponseMaxConflicts + (2 * recallResponseMaxProofRefs)
)

type recallResponseProofCompression struct {
	Candidates           []string
	Selected             []string
	Obligations          []string
	OracleChecked        bool
	OraclePassed         bool
	Sufficient           bool
	FailureStage         string
	ProtectedObligations int
	ProtectedWitnesses   int
}

type recallResponseProofObligation struct {
	Name         string
	Alternatives []string
}

// recallResponseCompressProof chooses the smallest deterministic proof set
// that still covers the primary result and every protected counterevidence
// reference. Temporal, snapshot, and receipt identity remain separate digest
// obligations in the proof spine and do not consume the eight ref budget.
func recallResponseCompressProof(response, proof map[string]any, policy validatedRecallResponsePolicyInput, sources ...map[string]any) (map[string]any, recallResponseProofCompression, bool) {
	var source map[string]any
	if len(sources) > 0 {
		source = sources[0]
	}
	candidates := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && !containsString(candidates, value) && len(candidates) < recallResponseMaxProofCandidates {
			candidates = append(candidates, value)
		}
	}
	for _, raw := range contextPackAnyList(proof["proof_refs"]) {
		add(anyToString(raw))
	}
	// The proof spine is bounded to eight emitted refs, but the deterministic
	// minimizer must see the complete bounded response evidence before it can
	// choose those eight. Protected and module-specific witnesses may occur
	// after the first eight ranked rows.
	for _, raw := range contextPackAnyList(response["evidence"]) {
		add(anyToString(anyMap(raw)["ref_id"]))
	}
	primary := strings.TrimSpace(anyToString(proof["primary_result"]))
	conflictRefs := recallResponseStringValues(proof["conflict_refs"])
	gapRefs := recallResponseStringValues(proof["gap_refs"])
	for _, ref := range append([]string{primary}, append(conflictRefs, gapRefs...)...) {
		add(ref)
	}
	moduleKinds, modulesOK := recallResponseSelectedModuleKinds(response, proof, policy, source)
	if !modulesOK {
		return nil, recallResponseProofCompression{Candidates: candidates, FailureStage: recallResponseFallbackStageModuleValidation}, false
	}
	obligations := []recallResponseProofObligation{}
	addObligation := func(name string, alternatives []string) {
		clean := []string{}
		for _, ref := range alternatives {
			ref = strings.TrimSpace(ref)
			if ref != "" && !containsString(clean, ref) {
				clean = append(clean, ref)
			}
		}
		if len(clean) > 0 {
			obligations = append(obligations, recallResponseProofObligation{Name: name, Alternatives: clean})
		}
	}
	if primary != "" {
		addObligation("primary_result", []string{primary})
	}
	for _, ref := range conflictRefs {
		addObligation("conflict:"+ref, []string{ref})
	}
	for _, ref := range gapRefs {
		addObligation("gap:"+ref, []string{ref})
	}
	for _, kind := range moduleKinds {
		obligations = append(obligations, recallResponseProofObligation{
			Name: "module:" + kind, Alternatives: recallResponseModuleRefs(kind, response, candidates, source),
		})
	}
	obligationNames := make([]string, 0, len(obligations))
	for _, obligation := range obligations {
		obligationNames = append(obligationNames, obligation.Name)
	}
	protected := 0
	for _, obligation := range obligations {
		if !strings.HasPrefix(obligation.Name, "module:") {
			protected++
		}
	}
	// Protected refs are selected first. Remaining module obligations use a
	// stable greedy cover over the original proof order.
	selected := []string{}
	addSelected := func(value string) {
		value = strings.TrimSpace(value)
		if value != "" && !containsString(selected, value) && len(selected) < recallResponseMaxProofRefs {
			selected = append(selected, value)
		}
	}
	addSelected(primary)
	for _, ref := range conflictRefs {
		addSelected(ref)
	}
	for _, ref := range gapRefs {
		addSelected(ref)
	}
	for !recallResponseProofObligationsSatisfied(selected, obligations) && len(selected) < recallResponseMaxProofRefs {
		best := ""
		bestCoverage := 0
		for _, candidate := range candidates {
			if containsString(selected, candidate) {
				continue
			}
			coverage := recallResponseProofCandidateCoverage(candidate, selected, obligations)
			if coverage > bestCoverage {
				best, bestCoverage = candidate, coverage
			}
		}
		if bestCoverage == 0 {
			break
		}
		addSelected(best)
	}
	compression := recallResponseProofCompression{
		Candidates:           append([]string(nil), candidates...),
		Selected:             append([]string(nil), selected...),
		Obligations:          obligationNames,
		ProtectedObligations: protected,
		Sufficient:           recallResponseProofObligationsSatisfied(selected, obligations),
	}
	for _, obligation := range obligations {
		if !strings.HasPrefix(obligation.Name, "module:") && recallResponseProofObligationsSatisfied(selected, []recallResponseProofObligation{obligation}) {
			compression.ProtectedWitnesses++
		}
	}
	if len(candidates) <= recallResponseMaxOracleRefs {
		compression.OracleChecked = true
		minimum, ok := recallResponseExhaustiveMinimumObligations(candidates, obligations)
		compression.OraclePassed = ok && len(minimum) == len(selected)
	} else {
		compression.OraclePassed = true
	}
	compression.Sufficient = compression.Sufficient && compression.OraclePassed
	if !compression.Sufficient {
		if compression.ProtectedWitnesses < compression.ProtectedObligations {
			compression.FailureStage = recallResponseFallbackStageProtectedWitness
		} else {
			compression.FailureStage = recallResponseFallbackStageCompression
		}
		return nil, compression, false
	}

	compressed := cloneJSONMap(proof)
	compressed["proof_refs"] = recallResponseAnyStrings(selected)
	compressed["conflict_refs"] = recallResponseAnyStrings(intersectStrings(conflictRefs, selected))
	compressed["gap_refs"] = recallResponseAnyStrings(intersectStrings(gapRefs, selected))
	coverage := []any{}
	for _, raw := range contextPackAnyList(proof["coverage"]) {
		row := cloneJSONMap(anyMap(raw))
		switch anyToString(row["obligation"]) {
		case "primary_result":
			row["proof_refs"] = recallResponseAnyStrings(intersectStrings([]string{primary}, selected))
		case "conflict_free":
			row["proof_refs"] = recallResponseAnyStrings(intersectStrings(conflictRefs, selected))
		case "material_gaps_resolved":
			row["proof_refs"] = recallResponseAnyStrings(intersectStrings(gapRefs, selected))
		}
		coverage = append(coverage, row)
	}
	for _, kind := range moduleKinds {
		alternatives := recallResponseModuleRefs(kind, response, candidates, source)
		witnesses := intersectStrings(alternatives, selected)
		if len(witnesses) > 1 {
			witnesses = witnesses[:1]
		}
		coverage = append(coverage, map[string]any{
			"obligation": "module:" + kind,
			"status":     recallResponseCoverageStatus(recallResponseProofObligationsSatisfied(selected, []recallResponseProofObligation{{Name: "module:" + kind, Alternatives: alternatives}})),
			"proof_refs": recallResponseAnyStrings(witnesses),
		})
	}
	coverage = append(coverage, map[string]any{
		"obligation": "minimal_sufficient_proof",
		"status":     recallResponseCoverageStatus(compression.Sufficient),
		"proof_refs": recallResponseAnyStrings(selected),
	})
	compressed["coverage"] = coverage
	return compressed, compression, true
}

func recallResponseStringValues(value any) []string {
	values := []string{}
	for _, raw := range contextPackAnyList(value) {
		value := strings.TrimSpace(anyToString(raw))
		if value != "" && !containsString(values, value) {
			values = append(values, value)
		}
	}
	return values
}

func intersectStrings(left, right []string) []string {
	set := map[string]bool{}
	for _, value := range right {
		set[value] = true
	}
	out := []string{}
	for _, value := range left {
		if set[value] && !containsString(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func recallResponseProofObligationSatisfied(selected map[string]bool, obligation recallResponseProofObligation) bool {
	for _, ref := range obligation.Alternatives {
		if selected[ref] {
			return true
		}
	}
	return false
}

func recallResponseProofObligationsSatisfied(selected []string, obligations []recallResponseProofObligation) bool {
	if len(selected) > recallResponseMaxProofRefs {
		return false
	}
	set := map[string]bool{}
	for _, ref := range selected {
		set[ref] = true
	}
	for _, obligation := range obligations {
		if !recallResponseProofObligationSatisfied(set, obligation) {
			return false
		}
	}
	return true
}

func recallResponseProofCandidateCoverage(candidate string, selected []string, obligations []recallResponseProofObligation) int {
	selectedSet := map[string]bool{}
	for _, ref := range selected {
		selectedSet[ref] = true
	}
	coverage := 0
	for _, obligation := range obligations {
		if recallResponseProofObligationSatisfied(selectedSet, obligation) || !containsString(obligation.Alternatives, candidate) {
			continue
		}
		coverage++
	}
	return coverage
}

// recallResponseExhaustiveMinimum is intentionally bounded to small cases.
// It is an oracle for tests and fixture-sized projections, never an unbounded
// production search. Ties are resolved by the input order, which is stable.
func recallResponseExhaustiveMinimum(candidates, obligations []string) ([]string, bool) {
	typed := make([]recallResponseProofObligation, 0, len(obligations))
	for _, obligation := range obligations {
		parts := strings.SplitN(obligation, ":", 2)
		if len(parts) != 2 {
			return nil, false
		}
		typed = append(typed, recallResponseProofObligation{Name: parts[0], Alternatives: []string{parts[1]}})
	}
	return recallResponseExhaustiveMinimumObligations(candidates, typed)
}

func recallResponseExhaustiveMinimumObligations(candidates []string, obligations []recallResponseProofObligation) ([]string, bool) {
	if len(candidates) > recallResponseMaxOracleRefs {
		return nil, false
	}
	if len(candidates) == 0 {
		return []string{}, len(obligations) == 0
	}
	best := []string(nil)
	limit := 1 << len(candidates)
	for mask := 0; mask < limit; mask++ {
		selected := []string{}
		for index, candidate := range candidates {
			if mask&(1<<index) != 0 {
				selected = append(selected, candidate)
			}
		}
		if !recallResponseProofObligationsSatisfied(selected, obligations) {
			continue
		}
		if best == nil || len(selected) < len(best) {
			best = selected
		}
	}
	return best, best != nil
}

func recallResponseCompactBudget(value any) (int, int) {
	canonical := recallResponseCanonicalJSON(value)
	bytes := len([]byte(canonical))
	tokens := (len([]rune(canonical)) + 3) / 4
	return bytes, tokens
}

func recallResponseNearestRank(values []int, percentile int) int {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int(nil), values...)
	sort.Ints(ordered)
	if percentile <= 0 {
		return ordered[0]
	}
	index := (len(ordered)*percentile + 99) / 100
	if index < 1 {
		index = 1
	}
	if index > len(ordered) {
		index = len(ordered)
	}
	return ordered[index-1]
}
