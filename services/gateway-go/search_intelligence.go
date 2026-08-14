package main

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	searchIntelligenceContractID       = "search_intelligence.v1"
	searchIntelligenceVersion          = 1
	searchIntelligenceFrontierLimit    = 12
	searchIntelligenceLocatorRefLimit  = 4
	searchIntelligenceRRFK             = 60.0
	searchIntelligenceCandidateVersion = "search_candidate.v1"
	searchIntelligenceActionLimit      = 8
)

type searchIntelligenceCandidate struct {
	CandidateRef string
	ContentRef   string
	PassageRef   string
	LocatorRef   string
}

// searchIntelligenceGatewayProvenanceEnvelope is only constructed by gateway
// row normalization. Backend JSON decodes to map[string]any and therefore
// cannot satisfy this type assertion in searchIntelligenceProvenance.
type searchIntelligenceGatewayProvenanceEnvelope struct {
	Source         string `json:"source"`
	SourceOwner    string `json:"source_owner"`
	ServerObserved bool   `json:"server_observed"`
}

// searchIntelligenceGatewayTrustEnvelope is constructed only after gateway row
// normalization has replaced a backend-supplied assessment. Its unexported
// fields deliberately keep this internal authority marker out of literal
// search results; search intelligence must not consume raw backend trust keys.
type searchIntelligenceGatewayTrustEnvelope struct {
	trustLabel  string
	quarantined bool
}

func searchIntelligenceGatewayObservedProvenance(source, sourceOwner string) searchIntelligenceGatewayProvenanceEnvelope {
	return searchIntelligenceGatewayProvenanceEnvelope{
		Source:         strings.TrimSpace(strings.ToLower(source)),
		SourceOwner:    strings.TrimSpace(sourceOwner),
		ServerObserved: true,
	}
}

func searchIntelligenceNormalizeGatewayTrustRows(rows []map[string]any) []map[string]any {
	for _, row := range rows {
		if row == nil {
			continue
		}
		// These are reporter-controlled fields until the gateway rewrites them.
		// Do not preserve them on normalized rows or let them reach a ranking path.
		for _, key := range []string{"trust_label", "trust_state", "quarantined"} {
			delete(row, key)
		}
		provenance, gatewayObserved := row["gateway_provenance"].(searchIntelligenceGatewayProvenanceEnvelope)
		if !gatewayObserved || !provenance.ServerObserved || strings.TrimSpace(provenance.Source) == "" || strings.TrimSpace(provenance.SourceOwner) == "" {
			delete(row, "gateway_trust_assessment")
			continue
		}
		assessment := anyMap(row["memory_trust_assessment"])
		label := strings.ToLower(strings.TrimSpace(anyToString(assessment["trust_label"])))
		quarantined := anyToBool(anyMap(assessment["quarantine"])["quarantined"])
		if quarantined || label == "quarantined" {
			label = "quarantined"
			quarantined = true
		}
		switch label {
		case "trusted", "bounded", "reliable", "observed", "untrusted", "rejected", "unsafe", "quarantined":
		default:
			label = "unknown"
		}
		row["gateway_trust_assessment"] = searchIntelligenceGatewayTrustEnvelope{
			trustLabel:  label,
			quarantined: quarantined,
		}
	}
	return rows
}

func searchIntelligenceStripGatewayTrustRows(rows []map[string]any) {
	for _, row := range rows {
		if row != nil {
			delete(row, "gateway_trust_assessment")
		}
	}
}

type searchIntelligenceInput struct {
	RowsBySource    map[string][]map[string]any
	AllMerged       []map[string]any
	Literal         []map[string]any
	ResultState     string
	Query           string
	RetrievalIntent string
	AsOf            time.Time
	AsOfSource      string
}

type searchIntelligenceFrontierCandidate struct {
	Identity        searchIntelligenceCandidate
	NativeRank      int
	BestNativeScore float64
	SourceRanks     map[string]int
	SourceNames     []string
	WeightedRRF     float64
	LocatorRefs     map[string]struct{}
	MetadataRows    []map[string]any
	Impact          searchIntelligenceCandidateImpact
}

type searchIntelligenceCandidateImpact struct {
	QueryAlignmentScore  float64
	QueryAlignmentStatus string
	QueryTokenCount      int
	MatchedTokenCount    int
	SourceScore          float64
	ProvenanceScore      float64
	ProvenanceStatus     string
	ReliabilityScore     float64
	CurrentnessScore     float64
	CurrentnessStatus    string
	VerificationScore    float64
	VerificationStatus   string
	VerifierCount        int
	ContradictionState   string
	TrustState           string
	Quarantined          bool
	Untrusted            bool
	DiversityScore       float64
	AcquisitionStatus    string
	AcquisitionRisk      float64
	Probability          float64
	Consequence          float64
	EvidenceReliability  float64
	ExpectedRegret       float64
	LeadEligible         bool
	UnknownMetadataCount int
	Actions              []string
}

func searchIntelligenceCandidateIdentity(row map[string]any) searchIntelligenceCandidate {
	contentRef := searchIntelligenceContentRef(row)
	passage, passageKnown := searchIntelligencePassageValue(row)
	passageRef := searchIntelligenceSHA256Ref(passage)
	identityPassageRef := passageRef
	if !passageKnown {
		identityPassageRef = searchIntelligenceSHA256Ref("locator_fallback\x00" + searchIntelligenceFallbackIdentity(row))
	}
	locatorRef := searchIntelligenceSHA256Ref(searchIntelligenceLocator(row))
	return searchIntelligenceCandidate{
		CandidateRef: searchIntelligenceSHA256Ref(strings.Join([]string{
			searchIntelligenceCandidateVersion,
			contentRef,
			identityPassageRef,
		}, "\x00")),
		ContentRef: contentRef,
		PassageRef: passageRef,
		LocatorRef: locatorRef,
	}
}

func searchIntelligenceContentRef(row map[string]any) string {
	if content, observed := searchIntelligenceContentValue(row); observed {
		return searchIntelligenceSHA256Ref(content)
	}
	for _, key := range []string{"content_ref", "content_hash"} {
		if ref := searchIntelligenceCanonicalSHA256Ref(anyToString(row[key])); ref != "" {
			return ref
		}
	}
	return searchIntelligenceSHA256Ref("empty_content")
}

func searchIntelligenceContentValue(row map[string]any) (string, bool) {
	for _, key := range []string{"content", "text", "content_excerpt", "excerpt", "summary", "claim"} {
		if value := strings.TrimSpace(anyToString(row[key])); value != "" {
			return searchIntelligenceNormalizeText(value), true
		}
	}
	return "empty_content", false
}

func searchIntelligencePassage(row map[string]any) string {
	value, _ := searchIntelligencePassageValue(row)
	return value
}

func searchIntelligencePassageValue(row map[string]any) (string, bool) {
	for _, key := range []string{"passage", "content_excerpt", "excerpt", "text", "summary", "claim", "content"} {
		if value := strings.TrimSpace(anyToString(row[key])); value != "" {
			return searchIntelligenceNormalizeText(value), true
		}
	}
	return "empty_passage", false
}

func searchIntelligenceFallbackIdentity(row map[string]any) string {
	// Without observed passage text, a content digest alone cannot prove that
	// two explicitly located chunks are the same passage. Bind the fallback to
	// file identity plus passage-level locators, but exclude redundant store IDs
	// such as project::file: one backend may emit that ID while another emits only
	// project/file for the same unsegmented result. Distinct chunks/ranges remain
	// distinct, while equivalent file-level rows collapse across stores.
	parts := make([]string, 0, 10)
	for _, key := range []string{
		"project", "file", "passage_id", "chunk_id", "letta_passage_id",
		"line_start", "line_end", "start_offset", "end_offset",
	} {
		if value := strings.TrimSpace(anyToString(row[key])); value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\x00")
	}
	return searchIntelligenceLocator(row)
}

func searchIntelligenceLocator(row map[string]any) string {
	parts := make([]string, 0, 12)
	for _, key := range []string{
		"project", "file", "topic_path", "memory_id", "id", "passage_id", "chunk_id",
		"letta_passage_id", "line_start", "line_end", "start_offset", "end_offset",
	} {
		if value := strings.TrimSpace(anyToString(row[key])); value != "" {
			parts = append(parts, key+"="+value)
		}
	}
	if len(parts) == 0 {
		return "unlocated"
	}
	return strings.Join(parts, "\x00")
}

func searchIntelligenceNormalizeText(value string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
}

func searchIntelligenceSHA256Ref(value string) string {
	return "sha256:" + sha256Hex(value)
}

func searchIntelligenceCanonicalSHA256Ref(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !strings.HasPrefix(value, "sha256:") {
		if len(value) == 64 {
			value = "sha256:" + value
		} else {
			return ""
		}
	}
	if !isSearchIntelligenceFullSHA256Ref(value) {
		return ""
	}
	return value
}

func isSearchIntelligenceFullSHA256Ref(value string) bool {
	return utilitySHA256DigestValid(value)
}

func buildSearchIntelligence(input searchIntelligenceInput) map[string]any {
	resultState := strings.TrimSpace(strings.ToLower(input.ResultState))
	if resultState == "" {
		resultState = "empty"
	}
	queryTokens := searchIntelligenceTokenSet(input.Query)
	intentClass, consequence := searchIntelligenceIntentClass(input.RetrievalIntent)
	asOf := input.AsOf.UTC()
	asOfKnown := !input.AsOf.IsZero()
	frontier := searchIntelligenceFrontier(input, queryTokens, consequence)
	return map[string]any{
		"schema_id": searchIntelligenceContractID,
		"version":   searchIntelligenceVersion,
		"mode":      "shadow",
		"activation": map[string]any{
			"learned_ranking_applied":    false,
			"calibrated_ranking_applied": false,
			"reason":                     "decision frontier is heuristic evidence-only; literal result ordering remains native",
		},
		"decision_context": map[string]any{
			"query_status":      searchIntelligenceKnownStatus(len(queryTokens) > 0),
			"query_token_count": len(queryTokens),
			"retrieval_intent":  intentClass,
			"as_of":             searchIntelligenceTimeString(asOf, asOfKnown),
			"as_of_status":      searchIntelligenceKnownStatus(asOfKnown),
			"as_of_source":      searchIntelligenceAsOfSource(input.AsOfSource, asOfKnown),
		},
		"literal_results": map[string]any{
			"status":         resultState,
			"ordering":       "native_score_desc_preserved",
			"returned_count": len(input.Literal),
		},
		"promotion":         retrievalPromotionSearchEnvelope(input),
		"decision_frontier": frontier,
	}
}

func searchIntelligenceFrontier(input searchIntelligenceInput, queryTokens map[string]struct{}, consequence float64) map[string]any {
	candidates := map[string]*searchIntelligenceFrontierCandidate{}
	for index, row := range input.AllMerged {
		identity := searchIntelligenceCandidateIdentity(row)
		candidate := candidates[identity.CandidateRef]
		if candidate == nil {
			candidate = &searchIntelligenceFrontierCandidate{
				Identity:        identity,
				NativeRank:      index + 1,
				BestNativeScore: parseScore(row),
				SourceRanks:     map[string]int{},
				LocatorRefs:     map[string]struct{}{},
			}
			candidates[identity.CandidateRef] = candidate
		} else {
			candidate.NativeRank = minInt(candidate.NativeRank, index+1)
			candidate.BestNativeScore = maxFloat(candidate.BestNativeScore, parseScore(row))
		}
		candidate.LocatorRefs[identity.LocatorRef] = struct{}{}
		candidate.MetadataRows = append(candidate.MetadataRows, row)
	}

	sourceRows := searchIntelligenceNormalizedSourceRows(input.RowsBySource)
	sources := make([]string, 0, len(sourceRows))
	for source := range sourceRows {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	for _, source := range sources {
		for index, row := range sourceRows[source] {
			identity := searchIntelligenceCandidateIdentity(row)
			candidate := candidates[identity.CandidateRef]
			if candidate == nil {
				continue
			}
			candidate.LocatorRefs[identity.LocatorRef] = struct{}{}
			if previous, exists := candidate.SourceRanks[source]; !exists || index+1 < previous {
				candidate.SourceRanks[source] = index + 1
			}
			candidate.MetadataRows = append(candidate.MetadataRows, row)
		}
	}

	ordered := make([]*searchIntelligenceFrontierCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.SourceNames = searchIntelligenceSortedSourceNames(candidate.SourceRanks)
		candidate.WeightedRRF = searchIntelligenceWeightedRRFScore(candidate.SourceRanks, candidate.SourceNames)
		candidate.Impact = searchIntelligenceCandidateDecisionImpact(candidate, queryTokens, input.AsOf, consequence)
		ordered = append(ordered, candidate)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		left, right := ordered[i].Impact, ordered[j].Impact
		if left.LeadEligible != right.LeadEligible {
			return left.LeadEligible
		}
		if math.Abs(left.ExpectedRegret-right.ExpectedRegret) < 1e-12 {
			leftRRF, rightRRF := ordered[i].WeightedRRF, ordered[j].WeightedRRF
			if math.Abs(leftRRF-rightRRF) >= 1e-12 {
				return leftRRF > rightRRF
			}
			if ordered[i].NativeRank == ordered[j].NativeRank {
				return ordered[i].Identity.CandidateRef < ordered[j].Identity.CandidateRef
			}
			return ordered[i].NativeRank < ordered[j].NativeRank
		}
		return left.ExpectedRegret > right.ExpectedRegret
	})

	limit := minInt(len(ordered), searchIntelligenceFrontierLimit)
	rendered := make([]any, 0, limit)
	for index, candidate := range ordered[:limit] {
		reasons := []any{"weighted_reciprocal_rank_fusion", "heuristic_expected_regret_reduction"}
		if len(candidate.SourceRanks) > 1 {
			reasons = append(reasons, "multi_source_support")
		} else {
			reasons = append(reasons, "single_source_candidate")
		}
		if len(candidate.LocatorRefs) > 1 {
			reasons = append(reasons, "cross_store_content_duplicate_collapsed")
		}
		for _, reason := range searchIntelligenceImpactReasons(candidate.Impact) {
			reasons = append(reasons, reason)
		}
		locatorRefs := searchIntelligenceSortedRefs(candidate.LocatorRefs, searchIntelligenceLocatorRefLimit)
		rendered = append(rendered, map[string]any{
			"refs": map[string]any{
				"candidate_ref": candidate.Identity.CandidateRef,
				"content_ref":   candidate.Identity.ContentRef,
				"passage_ref":   candidate.Identity.PassageRef,
				"locator_refs":  locatorRefs,
			},
			"reasons": reasons,
			"features": map[string]any{
				"shadow_rank":           index + 1,
				"native_rank":           candidate.NativeRank,
				"weighted_rrf_score":    roundFloat(candidate.WeightedRRF, 8),
				"rrf_rank_constant":     searchIntelligenceRRFK,
				"source_count":          len(candidate.SourceRanks),
				"best_native_score":     roundFloat(candidate.BestNativeScore, 8),
				"locator_variant_count": len(candidate.LocatorRefs),
				"query_alignment": map[string]any{
					"status":              candidate.Impact.QueryAlignmentStatus,
					"score":               roundFloat(candidate.Impact.QueryAlignmentScore, 6),
					"query_token_count":   candidate.Impact.QueryTokenCount,
					"matched_token_count": candidate.Impact.MatchedTokenCount,
				},
				"reliability": map[string]any{
					"status":            searchIntelligenceReliabilityStatus(candidate.Impact),
					"score":             roundFloat(candidate.Impact.ReliabilityScore, 6),
					"source_score":      roundFloat(candidate.Impact.SourceScore, 6),
					"provenance_status": candidate.Impact.ProvenanceStatus,
				},
				"currentness": map[string]any{
					"status": candidate.Impact.CurrentnessStatus,
					"score":  roundFloat(candidate.Impact.CurrentnessScore, 6),
				},
				"verification": map[string]any{
					"status":                     candidate.Impact.VerificationStatus,
					"score":                      roundFloat(candidate.Impact.VerificationScore, 6),
					"independent_verifier_count": candidate.Impact.VerifierCount,
				},
				"contradiction": map[string]any{
					"state": candidate.Impact.ContradictionState,
				},
				"trust": map[string]any{
					"state":         candidate.Impact.TrustState,
					"quarantined":   candidate.Impact.Quarantined,
					"lead_eligible": candidate.Impact.LeadEligible,
				},
				"diversity": map[string]any{
					"status":                 "observed",
					"score":                  roundFloat(candidate.Impact.DiversityScore, 6),
					"cross_store_duplicates": maxInt(0, len(candidate.LocatorRefs)-1),
				},
				"acquisition_cost": map[string]any{
					"status":     candidate.Impact.AcquisitionStatus,
					"risk_proxy": roundFloat(candidate.Impact.AcquisitionRisk, 6),
				},
				"decision_impact": map[string]any{
					"method":                              "heuristic_expected_regret_reduction",
					"learned":                             false,
					"calibrated":                          false,
					"probability_evidence_changes_action": roundFloat(candidate.Impact.Probability, 6),
					"consequence_of_wrong_action":         roundFloat(candidate.Impact.Consequence, 6),
					"evidence_reliability":                roundFloat(candidate.Impact.EvidenceReliability, 6),
					"heuristic_expected_regret_reduction": roundFloat(candidate.Impact.ExpectedRegret, 6),
					"metadata_unknown_count":              candidate.Impact.UnknownMetadataCount,
				},
			},
		})
	}
	leading := searchIntelligenceLeadingCandidate(ordered)
	actions := searchIntelligenceRecommendedActions(ordered)
	return map[string]any{
		"status":          "shadow_only",
		"candidate_count": len(ordered),
		"candidate_limit": searchIntelligenceFrontierLimit,
		"fusion": map[string]any{
			"method":                  "weighted_reciprocal_rank_fusion",
			"rank_constant":           searchIntelligenceRRFK,
			"weight_strategy":         "deterministic_source_prior",
			"learned_weights_applied": false,
		},
		"decision_impact": map[string]any{
			"method":        "heuristic_expected_regret_reduction",
			"formula":       "probability_evidence_changes_action * consequence_of_wrong_action * evidence_reliability - acquisition_cost_risk",
			"learned":       false,
			"calibrated":    false,
			"advisory_only": true,
		},
		"recommendation_state":                  searchIntelligenceRecommendationState(leading),
		"leading_candidate_ref":                 searchIntelligenceLeadingRef(leading),
		"recommended_verification_actions":      actions,
		"recommended_verification_action_limit": searchIntelligenceActionLimit,
		"aggregate_signals":                     searchIntelligenceAggregateSignals(ordered),
		"candidates":                            rendered,
	}
}

func searchIntelligenceNormalizedSourceRows(rowsBySource map[string][]map[string]any) map[string][]map[string]any {
	keys := make([]string, 0, len(rowsBySource))
	for source := range rowsBySource {
		keys = append(keys, source)
	}
	sort.Strings(keys)
	out := map[string][]map[string]any{}
	for _, source := range keys {
		normalized := strings.TrimSpace(strings.ToLower(source))
		if normalized == "" {
			continue
		}
		out[normalized] = append(out[normalized], rowsBySource[source]...)
	}
	return out
}

func searchIntelligenceCandidateDecisionImpact(candidate *searchIntelligenceFrontierCandidate, queryTokens map[string]struct{}, asOf time.Time, consequence float64) searchIntelligenceCandidateImpact {
	impact := searchIntelligenceCandidateImpact{
		QueryAlignmentStatus: searchIntelligenceKnownStatus(len(queryTokens) > 0),
		QueryTokenCount:      len(queryTokens),
		Consequence:          consequence,
	}
	impact.QueryAlignmentScore, impact.MatchedTokenCount = searchIntelligenceQueryAlignment(candidate.MetadataRows, queryTokens)
	impact.SourceScore = searchIntelligenceSourceScore(candidate)
	impact.ProvenanceStatus, impact.ProvenanceScore = searchIntelligenceProvenance(candidate.MetadataRows)
	impact.CurrentnessStatus, impact.CurrentnessScore = searchIntelligenceCurrentness(candidate.MetadataRows, asOf)
	impact.VerificationStatus, impact.VerificationScore, impact.VerifierCount = searchIntelligenceVerification(candidate.MetadataRows)
	impact.ContradictionState = searchIntelligenceContradictionState(candidate.MetadataRows)
	impact.TrustState, impact.Quarantined, impact.Untrusted = searchIntelligenceTrustState(candidate.MetadataRows)
	impact.DiversityScore = searchIntelligenceDiversityScore(candidate)
	impact.AcquisitionStatus, impact.AcquisitionRisk = searchIntelligenceAcquisitionRisk(candidate.MetadataRows)

	provenanceFactor := 0.5
	if impact.ProvenanceStatus == "observed" {
		provenanceFactor = 1
	} else if impact.ProvenanceStatus == "unverified" {
		provenanceFactor = 0.25
	}
	verificationFactor := 0.25
	if impact.VerificationStatus == "independently_verified" {
		verificationFactor = 1
	} else if impact.VerificationStatus == "verified" {
		verificationFactor = 0.8
	} else if impact.VerificationStatus == "claimed" || impact.VerificationStatus == "insufficient_evidence" || impact.VerificationStatus == "failed" {
		verificationFactor = 0.1
	}
	currentnessFactor := 0.35
	switch impact.CurrentnessStatus {
	case "current":
		currentnessFactor = 1
	case "recent":
		currentnessFactor = 0.75
	case "aging":
		currentnessFactor = 0.5
	case "stale":
		currentnessFactor = 0.2
	}
	trustFactor := 0.7
	if impact.TrustState == "bounded" || impact.TrustState == "trusted" {
		trustFactor = 1
	}
	if impact.Quarantined || impact.Untrusted {
		trustFactor = 0
	}
	contradictionFactor := 1.0
	if impact.ContradictionState == "unresolved" {
		contradictionFactor = 0.35
	}
	impact.EvidenceReliability = clampFloat(
		impact.SourceScore*provenanceFactor*verificationFactor*currentnessFactor*trustFactor*contradictionFactor,
		0,
		1,
	)
	impact.ReliabilityScore = impact.EvidenceReliability
	impact.Probability = clampFloat(0.1+0.75*impact.QueryAlignmentScore+0.15*impact.DiversityScore, 0, 1)
	impact.ExpectedRegret = impact.Probability*impact.Consequence*impact.EvidenceReliability - impact.AcquisitionRisk
	if impact.ContradictionState == "unresolved" {
		impact.ExpectedRegret -= 0.15
	}
	if impact.Quarantined || impact.Untrusted {
		impact.ExpectedRegret -= 1
	}
	impact.ExpectedRegret = roundFloat(impact.ExpectedRegret, 8)
	impact.LeadEligible = !impact.Quarantined && !impact.Untrusted
	impact.UnknownMetadataCount = searchIntelligenceUnknownMetadataCount(impact)
	impact.Actions = searchIntelligenceImpactActions(impact)
	return impact
}

func searchIntelligenceTokenSet(value string) map[string]struct{} {
	tokens := map[string]struct{}{}
	for _, token := range strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		if len([]rune(token)) < 3 || searchIntelligenceStopToken(token) {
			continue
		}
		tokens[token] = struct{}{}
	}
	return tokens
}

func searchIntelligenceStopToken(token string) bool {
	switch token {
	case "and", "are", "for", "from", "how", "into", "that", "the", "this", "with":
		return true
	default:
		return false
	}
}

func searchIntelligenceQueryAlignment(rows []map[string]any, queryTokens map[string]struct{}) (float64, int) {
	if len(queryTokens) == 0 {
		return 0, 0
	}
	passageTokens := map[string]struct{}{}
	for _, row := range rows {
		for token := range searchIntelligenceTokenSet(searchIntelligencePassage(row)) {
			passageTokens[token] = struct{}{}
		}
	}
	matched := 0
	for token := range queryTokens {
		if _, exists := passageTokens[token]; exists {
			matched++
		}
	}
	return float64(matched) / float64(len(queryTokens)), matched
}

func searchIntelligenceSourceScore(candidate *searchIntelligenceFrontierCandidate) float64 {
	if candidate == nil || len(candidate.SourceRanks) == 0 {
		return 0
	}
	sources := candidate.SourceNames
	if len(sources) == 0 {
		sources = searchIntelligenceSortedSourceNames(candidate.SourceRanks)
	}
	total := 0.0
	for _, source := range sources {
		total += searchIntelligenceSourceWeight(source)
	}
	return clampFloat(total/float64(len(sources)), 0, 1)
}

func searchIntelligenceProvenance(rows []map[string]any) (string, float64) {
	seen := false
	for _, row := range rows {
		envelope, present := row["gateway_provenance"].(searchIntelligenceGatewayProvenanceEnvelope)
		if !present {
			continue
		}
		seen = true
		if envelope.ServerObserved && envelope.Source != "" && envelope.SourceOwner != "" {
			return "observed", 1
		}
	}
	if seen {
		return "unverified", 0
	}
	return "unknown", 0
}

func searchIntelligenceCurrentness(rows []map[string]any, asOf time.Time) (string, float64) {
	if asOf.IsZero() {
		return "unknown", 0
	}
	latest := time.Time{}
	for _, row := range rows {
		for _, key := range []string{"timestamp", "updated_at", "updatedAt", "verified_at", "captured_at", "created_at", "createdAt"} {
			candidate, ok := parseTimeBestEffort(anyToString(row[key]))
			if !ok || candidate.After(asOf.Add(time.Minute)) || candidate.Before(latest) {
				continue
			}
			latest = candidate
		}
	}
	if latest.IsZero() {
		return "unknown", 0
	}
	ageDays := math.Max(0, asOf.Sub(latest).Hours()/24)
	switch {
	case ageDays <= 30:
		return "current", 1
	case ageDays <= 90:
		return "recent", 0.7
	case ageDays <= 365:
		return "aging", 0.4
	default:
		return "stale", 0.1
	}
}

func searchIntelligenceVerification(rows []map[string]any) (string, float64, int) {
	claimed := false
	for _, row := range rows {
		if len(anyMap(row["candidate_utility_verification"])) > 0 || len(anyMap(row["verification"])) > 0 || anyToBool(row["verification_passed"]) || anyToBool(row["independently_verified"]) || anyToString(row["verification_status"]) != "" {
			claimed = true
		}
	}
	if claimed {
		return "claimed", 0, 0
	}
	return "unknown", 0, 0
}

func searchIntelligenceContradictionState(rows []map[string]any) string {
	state := "unknown"
	for _, row := range rows {
		if anyToBool(row["has_unresolved_contradiction"]) || anyToBool(row["unresolved_contradiction"]) || anyToBool(row["contradicted"]) {
			return "unresolved"
		}
		candidate := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(anyToString(row["contradiction_state"]), anyToString(anyMap(row["contradiction"])["state"]))))
		switch candidate {
		case "unresolved", "contradicted", "open", "opposed":
			return "unresolved"
		case "resolved":
			if state == "unknown" {
				state = "resolved"
			}
		case "clear", "none", "not_contradicted":
			if state == "unknown" {
				state = "clear"
			}
		}
	}
	return state
}

func searchIntelligenceTrustState(rows []map[string]any) (string, bool, bool) {
	state := "unknown"
	for _, row := range rows {
		assessment, gatewayOwned := row["gateway_trust_assessment"].(searchIntelligenceGatewayTrustEnvelope)
		if !gatewayOwned {
			continue
		}
		if assessment.quarantined || assessment.trustLabel == "quarantined" {
			return "quarantined", true, true
		}
		switch assessment.trustLabel {
		case "untrusted", "rejected", "unsafe":
			return "untrusted", false, true
		case "trusted":
			state = "trusted"
		case "bounded", "reliable", "observed":
			if state == "unknown" {
				state = "bounded"
			}
		}
	}
	return state, false, false
}

func searchIntelligenceDiversityScore(candidate *searchIntelligenceFrontierCandidate) float64 {
	if candidate == nil {
		return 0
	}
	switch len(candidate.SourceRanks) {
	case 0:
		return 0
	case 1:
		return 0.25
	case 2:
		return 0.6
	default:
		return 0.85
	}
}

func searchIntelligenceAcquisitionRisk(rows []map[string]any) (string, float64) {
	found := false
	risk := 0.0
	for _, row := range rows {
		for _, key := range []string{"acquisition_cost_proxy", "acquisition_cost", "retrieval_cost"} {
			if value, ok := searchIntelligenceNumeric(row[key]); ok && value >= 0 {
				found = true
				risk = math.Max(risk, clampFloat(value, 0, 1))
			}
		}
		if value, ok := searchIntelligenceNumeric(row["cost_microusd"]); ok && value >= 0 {
			found = true
			risk = math.Max(risk, clampFloat(value/1_000_000, 0, 1))
		}
		for _, key := range []string{"latency_ms", "duration_ms"} {
			if value, ok := searchIntelligenceNumeric(row[key]); ok && value >= 0 {
				found = true
				risk = math.Max(risk, clampFloat(value/10_000, 0, 1))
			}
		}
		if value, ok := searchIntelligenceNumeric(row["tool_calls"]); ok && value >= 0 {
			found = true
			risk = math.Max(risk, clampFloat(value/10, 0, 1))
		}
	}
	if !found {
		return "unknown", 0.35
	}
	return "observed", roundFloat(0.05+0.95*risk, 8)
}

func searchIntelligenceNumeric(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func searchIntelligenceUnknownMetadataCount(impact searchIntelligenceCandidateImpact) int {
	count := 0
	for _, status := range []string{impact.ProvenanceStatus, impact.CurrentnessStatus, impact.VerificationStatus, impact.ContradictionState, impact.TrustState, impact.AcquisitionStatus} {
		if status == "unknown" {
			count++
		}
	}
	return count
}

func searchIntelligenceImpactActions(impact searchIntelligenceCandidateImpact) []string {
	actions := []string{}
	if impact.Quarantined || impact.Untrusted {
		actions = append(actions, "exclude_quarantined_evidence")
	}
	if impact.ContradictionState == "unresolved" {
		actions = append(actions, "resolve_contradiction")
	}
	if impact.VerificationStatus != "independently_verified" {
		actions = append(actions, "independent_verification_needed")
	}
	if impact.CurrentnessStatus == "unknown" {
		actions = append(actions, "timestamp_needed")
	}
	if impact.ProvenanceStatus == "unknown" || impact.ProvenanceStatus == "unverified" {
		actions = append(actions, "provenance_needed")
	}
	if impact.AcquisitionStatus == "unknown" {
		actions = append(actions, "acquisition_cost_unknown")
	}
	if impact.QueryAlignmentStatus == "unknown" || impact.QueryAlignmentScore < 0.2 {
		actions = append(actions, "query_alignment_review_needed")
	}
	return actions
}

func searchIntelligenceImpactReasons(impact searchIntelligenceCandidateImpact) []any {
	reasons := []any{}
	if impact.CurrentnessStatus == "current" && impact.VerificationStatus == "independently_verified" {
		reasons = append(reasons, "current_independently_verified_evidence")
	}
	if impact.CurrentnessStatus == "stale" {
		reasons = append(reasons, "stale_evidence")
	}
	if impact.ContradictionState == "unresolved" {
		reasons = append(reasons, "unresolved_contradiction")
	}
	if impact.Quarantined || impact.Untrusted {
		reasons = append(reasons, "quarantined_or_untrusted")
	}
	if impact.UnknownMetadataCount > 0 {
		reasons = append(reasons, "metadata_unknown")
	}
	return reasons
}

func searchIntelligenceReliabilityStatus(impact searchIntelligenceCandidateImpact) string {
	if impact.SourceScore == 0 {
		return "unknown"
	}
	if impact.ProvenanceStatus == "observed" && impact.VerificationStatus == "independently_verified" {
		return "observed"
	}
	return "partial"
}

func searchIntelligenceLeadingCandidate(candidates []*searchIntelligenceFrontierCandidate) *searchIntelligenceFrontierCandidate {
	for _, candidate := range candidates {
		if candidate != nil && candidate.Impact.LeadEligible {
			return candidate
		}
	}
	return nil
}

func searchIntelligenceLeadingRef(candidate *searchIntelligenceFrontierCandidate) string {
	if candidate == nil || !candidate.Impact.LeadEligible {
		return ""
	}
	return candidate.Identity.CandidateRef
}

func searchIntelligenceRecommendationState(candidate *searchIntelligenceFrontierCandidate) string {
	if candidate == nil {
		return "abstain"
	}
	if candidate.Impact.VerificationStatus == "independently_verified" && candidate.Impact.CurrentnessStatus == "current" && candidate.Impact.ContradictionState != "unresolved" {
		return "evidence_ready"
	}
	return "verify_before_action"
}

func searchIntelligenceRecommendedActions(candidates []*searchIntelligenceFrontierCandidate) []any {
	seen := map[string]struct{}{}
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		for _, action := range candidate.Impact.Actions {
			seen[action] = struct{}{}
		}
	}
	order := []string{
		"exclude_quarantined_evidence", "resolve_contradiction", "independent_verification_needed",
		"timestamp_needed", "provenance_needed", "acquisition_cost_unknown", "query_alignment_review_needed",
	}
	out := make([]any, 0, minInt(len(order), searchIntelligenceActionLimit))
	for _, action := range order {
		if _, present := seen[action]; !present {
			continue
		}
		out = append(out, action)
		if len(out) >= searchIntelligenceActionLimit {
			break
		}
	}
	return out
}

func searchIntelligenceAggregateSignals(candidates []*searchIntelligenceFrontierCandidate) map[string]any {
	eligible := 0
	quarantined := 0
	unresolved := 0
	verified := 0
	current := 0
	unknown := 0
	totalRegret := 0.0
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		impact := candidate.Impact
		if impact.LeadEligible {
			eligible++
		}
		if impact.Quarantined || impact.Untrusted {
			quarantined++
		}
		if impact.ContradictionState == "unresolved" {
			unresolved++
		}
		if impact.VerificationStatus == "independently_verified" {
			verified++
		}
		if impact.CurrentnessStatus == "current" {
			current++
		}
		if impact.UnknownMetadataCount > 0 {
			unknown++
		}
		totalRegret += impact.ExpectedRegret
	}
	meanRegret := 0.0
	if len(candidates) > 0 {
		meanRegret = totalRegret / float64(len(candidates))
	}
	return map[string]any{
		"eligible_candidate_count":                 eligible,
		"quarantined_candidate_count":              quarantined,
		"unresolved_contradiction_count":           unresolved,
		"independently_verified_candidate_count":   verified,
		"current_candidate_count":                  current,
		"unknown_metadata_candidate_count":         unknown,
		"mean_heuristic_expected_regret_reduction": roundFloat(meanRegret, 6),
	}
}

func searchIntelligenceIntentClass(intent string) (string, float64) {
	switch strings.ToLower(strings.TrimSpace(intent)) {
	case "safety", "security", "compliance", "financial", "release":
		return strings.ToLower(strings.TrimSpace(intent)), 0.95
	case "decision", "diagnosis", "planning", "incident":
		return strings.ToLower(strings.TrimSpace(intent)), 0.85
	case "research", "information", "context":
		return strings.ToLower(strings.TrimSpace(intent)), 0.6
	default:
		return "other", 0.5
	}
}

func searchIntelligenceKnownStatus(known bool) string {
	if known {
		return "observed"
	}
	return "unknown"
}

func searchIntelligenceTimeString(value time.Time, known bool) string {
	if !known {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func searchIntelligenceAsOfSource(value string, asOfKnown bool) string {
	if !asOfKnown {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "request", "gateway_observed":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "provided"
	}
}

func searchIntelligenceResolveAsOf(request map[string]any, observedAt time.Time) (time.Time, string) {
	for _, key := range []string{"as_of", "asOf"} {
		if raw := strings.TrimSpace(anyToString(request[key])); raw != "" {
			if parsed, ok := parseTimeBestEffort(raw); ok {
				return parsed, "request"
			}
		}
	}
	return observedAt.UTC(), "gateway_observed"
}

func searchIntelligenceWeightedRRFScore(sourceRanks map[string]int, sources []string) float64 {
	score := 0.0
	for _, source := range sources {
		rank := sourceRanks[source]
		if rank <= 0 {
			continue
		}
		score += searchIntelligenceSourceWeight(source) / (searchIntelligenceRRFK + float64(rank))
	}
	return score
}

func searchIntelligenceSortedSourceNames(sourceRanks map[string]int) []string {
	sources := make([]string, 0, len(sourceRanks))
	for source := range sourceRanks {
		sources = append(sources, source)
	}
	sort.Strings(sources)
	return sources
}

func searchIntelligenceSourceWeight(source string) float64 {
	return clampFloat(retrievalSourcePrior(strings.TrimSpace(strings.ToLower(source))), 0.5, 1.0)
}

func searchIntelligenceSortedRefs(values map[string]struct{}, limit int) []any {
	refs := mapKeysSorted(values)
	if len(refs) > limit {
		refs = refs[:limit]
	}
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref)
	}
	return out
}
