package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var errCurrentStateSearchIndexUnavailable = errors.New("current-state search index unavailable")

type currentStateSearchStats struct {
	ProjectDocuments int
	ProjectTopics    int
	TopicsScanned    int
	Scanned          int
	Matched          int
	ExactMatched     int
	AncestorMatched  int
	AncestorUsed     bool
	AncestorPrefix   string
	AncestorDepth    int
	AncestorPrefixes []string
	ScopeExhaustive  bool
	IndexGeneration  uint64
}

type currentStateSearchCandidate struct {
	state            memoryCurrentState
	topicPath        string
	exactState       bool
	effectiveHorizon int
	indexedLastTouch time.Time
	retrievalScope   string
	ancestorPrefix   string
	ancestorDistance int
}

const (
	currentStateRetrievalScopeExact    = "exact_topic"
	currentStateRetrievalScopeAncestor = "nearest_topic_ancestor"
)

// searchCurrentStateRows is the bounded, file-citing half of the Go-owned
// topic-rollup lane. It searches only the caller-selected project's active
// current-state index and never uses a requested or expected file name as a
// ranking signal. Historical as_of reads deliberately remain on the temporal
// retrieval paths because current state cannot reconstruct an older winner.
func (m *memoryStore) searchCurrentStateRows(
	ctx context.Context,
	query string,
	project string,
	topicPrefix string,
	limit int,
	includeCold bool,
	includeEphemeral bool,
) ([]map[string]any, currentStateSearchStats, error) {
	return m.searchCurrentStateRowsScoped(
		ctx,
		query,
		project,
		topicPrefix,
		limit,
		includeCold,
		includeEphemeral,
		0,
		0,
		0,
	)
}

// searchCurrentStateRowsWithAncestorFallback preserves exact-topic ordering
// and fills a sparse exact result only from the nearest topic ancestor. The
// fallback never crosses projects and never widens to a project-wide scan.
func (m *memoryStore) searchCurrentStateRowsWithAncestorFallback(
	ctx context.Context,
	query string,
	project string,
	topicPrefix string,
	limit int,
	includeCold bool,
	includeEphemeral bool,
	minimumExactRows int,
	minimumExactScore float64,
	minimumAncestorScore float64,
) ([]map[string]any, currentStateSearchStats, error) {
	return m.searchCurrentStateRowsScoped(
		ctx,
		query,
		project,
		topicPrefix,
		limit,
		includeCold,
		includeEphemeral,
		maxInt(1, minimumExactRows),
		clampFloat(minimumExactScore, 0, 1),
		clampFloat(minimumAncestorScore, 0, 1),
	)
}

func (m *memoryStore) searchCurrentStateRowsScoped(
	ctx context.Context,
	query string,
	project string,
	topicPrefix string,
	limit int,
	includeCold bool,
	includeEphemeral bool,
	minimumExactRows int,
	minimumExactScore float64,
	minimumAncestorScore float64,
) ([]map[string]any, currentStateSearchStats, error) {
	stats := currentStateSearchStats{}
	if m == nil || !m.isEnabled() {
		return []map[string]any{}, stats, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	query = strings.TrimSpace(query)
	project = strings.TrimSpace(project)
	topicPrefix = normalizeTopicPathLoose(topicPrefix)
	if query == "" || project == "" {
		return []map[string]any{}, stats, nil
	}
	cleanProject, err := sanitizeMemoryProject(project)
	if err != nil {
		return nil, stats, err
	}
	limit = clampInt(limit, 1, 100)
	ancestorPrefixes := []string{}
	if minimumExactRows > 0 {
		ancestorPrefixes = topicAncestorPrefixes(
			topicPrefix,
			clampInt(envInt("GO_RETRIEVAL_AUTHORITATIVE_CURRENT_STATE_ANCESTOR_MAX_DEPTH", 8), 1, 8),
		)
		stats.AncestorPrefixes = append([]string(nil), ancestorPrefixes...)
		if len(ancestorPrefixes) > 0 {
			stats.AncestorPrefix = ancestorPrefixes[0]
		}
	}
	maxProjectDocuments := clampInt(
		envInt("GO_MEMORY_CURRENT_STATE_SEARCH_MAX_PROJECT_DOCS", 250000),
		1000,
		250000,
	)
	writePolicy := loadWriteIngressPolicy()

	// Validate the complete project denominator and capture only the requested
	// topic's immutable state while holding one read lock. The topic projection
	// is maintained atomically with the primary project index, so this lane is
	// proportional to distinct project topics plus selected rows rather than all
	// project documents. Count and generation bindings keep a partial or stale
	// projection fail-closed.
	candidates := []currentStateSearchCandidate{}
	m.mu.RLock()
	if m.currentKeysByProject == nil || m.currentKeyCountsByProject == nil ||
		m.currentKeysByProjectTopic == nil || m.currentTopicKeyCountsByProject == nil ||
		m.currentKeyIndexGeneration == nil || m.currentTopicIndexGeneration == nil {
		m.mu.RUnlock()
		return nil, stats, errCurrentStateSearchIndexUnavailable
	}
	projectKey := normalizeCurrentKeyIndexProject(cleanProject)
	keys, keysPresent := m.currentKeysByProject[projectKey]
	expectedCount, countPresent := m.currentKeyCountsByProject[projectKey]
	topics, topicsPresent := m.currentKeysByProjectTopic[projectKey]
	topicKeyCount, topicCountPresent := m.currentTopicKeyCountsByProject[projectKey]
	keyGeneration, keyGenerationPresent := m.currentKeyIndexGeneration[projectKey]
	topicGeneration, topicGenerationPresent := m.currentTopicIndexGeneration[projectKey]
	if !keysPresent && !countPresent {
		if topicsPresent || topicCountPresent || keyGenerationPresent || topicGenerationPresent {
			m.mu.RUnlock()
			return nil, stats, errCurrentStateSearchIndexUnavailable
		}
		m.mu.RUnlock()
		return []map[string]any{}, stats, nil
	}
	if !keysPresent || !countPresent || !topicsPresent || !topicCountPresent ||
		!keyGenerationPresent || !topicGenerationPresent || expectedCount < 0 ||
		topicKeyCount < 0 || len(keys) != expectedCount || topicKeyCount != expectedCount ||
		keyGeneration != topicGeneration {
		m.mu.RUnlock()
		return nil, stats, errCurrentStateSearchIndexUnavailable
	}
	stats.ProjectDocuments = expectedCount
	stats.ProjectTopics = len(topics)
	stats.IndexGeneration = keyGeneration
	if expectedCount > maxProjectDocuments {
		m.mu.RUnlock()
		return nil, stats, fmt.Errorf(
			"%w: project_documents=%d configured_limit=%d",
			errCurrentStateSearchIndexUnavailable,
			expectedCount,
			maxProjectDocuments,
		)
	}
	candidates = make([]currentStateSearchCandidate, 0, minInt(expectedCount, limit*16))
	indexedTopicKeys := 0
	for indexedTopic, topicKeys := range topics {
		select {
		case <-ctx.Done():
			m.mu.RUnlock()
			return nil, stats, ctx.Err()
		default:
		}
		stats.TopicsScanned++
		indexedTopicKeys += len(topicKeys)
		topicPath := indexedTopic
		if topicPath == currentStateUnscopedTopicBucket {
			continue
		}
		exactTopic := recallEvalTopicMatchesPrefix(topicPath, topicPrefix)
		ancestorDistance := 0
		ancestorPrefix := ""
		if !exactTopic {
			for index, candidatePrefix := range ancestorPrefixes {
				if recallEvalTopicMatchesPrefix(topicPath, candidatePrefix) {
					ancestorDistance = index + 1
					ancestorPrefix = candidatePrefix
					break
				}
			}
		}
		if !exactTopic && ancestorDistance == 0 {
			continue
		}
		retrievalScope := currentStateRetrievalScopeExact
		if ancestorDistance > 0 {
			retrievalScope = currentStateRetrievalScopeAncestor
		}
		for key := range topicKeys {
			select {
			case <-ctx.Done():
				m.mu.RUnlock()
				return nil, stats, ctx.Err()
			default:
			}
			indexedProject, fileName, keyOK := parseMemoryStoreKeyToken(key)
			state, stateOK := m.currentState[key]
			latestTopic, topicOK := m.latestTopic[key]
			_, primaryKeyPresent := keys[key]
			if !keyOK || !strings.EqualFold(indexedProject, cleanProject) || fileName == "" ||
				!stateOK || state.Tombstone || !topicOK || !primaryKeyPresent ||
				currentStateTopicBucket(latestTopic) != indexedTopic {
				m.mu.RUnlock()
				return nil, stats, errCurrentStateSearchIndexUnavailable
			}
			stats.Scanned++
			state.Entry.Tags = append([]string(nil), state.Entry.Tags...)
			effectiveHorizon := m.policy.hotIndexMaxAgeDays
			if specific, ok := m.latestHorizon[key]; ok {
				if specific < 0 {
					effectiveHorizon = 0
				} else {
					effectiveHorizon = specific
				}
			}
			if state.Entry.HorizonDays != 0 {
				effectiveHorizon = maxInt(state.Entry.HorizonDays, 0)
			}
			candidates = append(candidates, currentStateSearchCandidate{
				state:            state,
				topicPath:        topicPath,
				exactState:       exactStatePathSetContains(m.exactStatePaths, state.Entry.Project, state.Entry.FileName),
				effectiveHorizon: effectiveHorizon,
				indexedLastTouch: m.lastAccess[key],
				retrievalScope:   retrievalScope,
				ancestorPrefix:   ancestorPrefix,
				ancestorDistance: ancestorDistance,
			})
		}
	}
	if indexedTopicKeys != topicKeyCount {
		m.mu.RUnlock()
		return nil, stats, errCurrentStateSearchIndexUnavailable
	}
	m.mu.RUnlock()

	exactRows := make([]map[string]any, 0, minInt(limit*4, len(candidates)))
	ancestorRows := make([]map[string]any, 0, minInt(limit*4, len(candidates)))
	for _, candidate := range candidates {
		select {
		case <-ctx.Done():
			return nil, stats, ctx.Err()
		default:
		}
		state := candidate.state
		entry := state.Entry
		if candidate.exactState ||
			writePolicy.isDurableMemoryFile(normalizedWrite{project: entry.Project, fileName: entry.FileName}) {
			continue
		}
		topicPath := candidate.topicPath
		lifecycle := normalizeMemoryLifecycle(entry.Lifecycle)
		if isEphemeralMemoryIdentity(entry.FileName, topicPath, entry.Summary, lifecycle) ||
			!shouldSurfaceMemoryLifecycle(lifecycle, includeEphemeral) {
			continue
		}
		storageTier := normalizeMemoryStorageTier(entry.StorageTier)
		if !includeCold && (storageTier == "deep" || storageTier == "retired") {
			continue
		}
		effectiveHorizon := candidate.effectiveHorizon
		lastTouch := time.Time{}
		if parsed, ok := parseTimeBestEffort(entry.CreatedAt); ok && (lastTouch.IsZero() || parsed.After(lastTouch)) {
			lastTouch = parsed
		}
		if parsed, ok := parseTimeBestEffort(entry.LastAccess); ok && (lastTouch.IsZero() || parsed.After(lastTouch)) {
			lastTouch = parsed
		}
		if !candidate.indexedLastTouch.IsZero() && (lastTouch.IsZero() || candidate.indexedLastTouch.After(lastTouch)) {
			lastTouch = candidate.indexedLastTouch
		}
		if !includeCold && effectiveHorizon > 0 && !lastTouch.IsZero() &&
			lastTouch.Before(time.Now().UTC().Add(-time.Duration(effectiveHorizon)*24*time.Hour)) {
			continue
		}
		summary := strings.TrimSpace(entry.Summary)
		if summary == "" {
			continue
		}
		score := currentStateLexicalScore(query, topicPath, summary)
		if score <= 0 {
			continue
		}
		stats.Matched++
		if candidate.retrievalScope == currentStateRetrievalScopeAncestor {
			stats.AncestorMatched++
		} else {
			stats.ExactMatched++
		}
		row := map[string]any{
			"project":              entry.Project,
			"file":                 entry.FileName,
			"summary":              summary,
			"score":                score,
			"source":               sourceTopicRollup,
			"topic_path":           topicPath,
			"created_at":           entry.CreatedAt,
			"confidence":           entry.Confidence,
			"horizon_days":         effectiveHorizon,
			"event_id":             entry.EventID,
			"content_hash":         entry.ContentHash,
			"lifecycle":            lifecycle,
			"storage_tier":         storageTier,
			"legal_hold":           state.LegalHold,
			"projection_authority": "current_event",
			"retrieval_lane":       "current_state_index",
			"retrieval_scope":      candidate.retrievalScope,
		}
		if candidate.retrievalScope == currentStateRetrievalScopeAncestor {
			row["retrieval_ancestor_prefix"] = candidate.ancestorPrefix
			row["retrieval_ancestor_distance"] = candidate.ancestorDistance
			ancestorRows = append(ancestorRows, row)
		} else {
			exactRows = append(exactRows, row)
		}
	}
	sortRows := func(rows []map[string]any) {
		sort.SliceStable(rows, func(i, j int) bool {
			leftScore, rightScore := parseScore(rows[i]), parseScore(rows[j])
			if leftScore != rightScore {
				return leftScore > rightScore
			}
			leftAt, leftOK := parseTimeBestEffort(anyToString(rows[i]["created_at"]))
			rightAt, rightOK := parseTimeBestEffort(anyToString(rows[j]["created_at"]))
			if leftOK && rightOK && !leftAt.Equal(rightAt) {
				return leftAt.After(rightAt)
			}
			if leftOK != rightOK {
				return leftOK
			}
			leftIdentity := strings.ToLower(anyToString(rows[i]["project"]) + "\x00" + anyToString(rows[i]["file"]))
			rightIdentity := strings.ToLower(anyToString(rows[j]["project"]) + "\x00" + anyToString(rows[j]["file"]))
			return leftIdentity < rightIdentity
		})
	}
	sortRows(exactRows)
	sort.SliceStable(ancestorRows, func(i, j int) bool {
		leftDistance := anyToInt(ancestorRows[i]["retrieval_ancestor_distance"], 0)
		rightDistance := anyToInt(ancestorRows[j]["retrieval_ancestor_distance"], 0)
		if leftDistance != rightDistance {
			return leftDistance < rightDistance
		}
		leftScore, rightScore := parseScore(ancestorRows[i]), parseScore(ancestorRows[j])
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		leftIdentity := strings.ToLower(anyToString(ancestorRows[i]["project"]) + "\x00" + anyToString(ancestorRows[i]["file"]))
		rightIdentity := strings.ToLower(anyToString(ancestorRows[j]["project"]) + "\x00" + anyToString(ancestorRows[j]["file"]))
		return leftIdentity < rightIdentity
	})

	rows := append([]map[string]any(nil), exactRows...)
	if minimumExactRows > 0 && len(ancestorPrefixes) > 0 {
		qualifiedExact := 0
		for _, row := range exactRows {
			if parseScore(row) >= minimumExactScore {
				qualifiedExact++
			}
		}
		if qualifiedExact < minimumExactRows {
			selectedQualified := qualifiedExact
			selectedDistance := 0
			for _, row := range ancestorRows {
				if parseScore(row) < minimumAncestorScore {
					continue
				}
				distance := anyToInt(row["retrieval_ancestor_distance"], 0)
				if selectedDistance > 0 && distance != selectedDistance && selectedQualified >= minimumExactRows {
					break
				}
				selectedDistance = distance
				rows = append(rows, row)
				stats.AncestorUsed = true
				selectedQualified++
				if distance > stats.AncestorDepth {
					stats.AncestorDepth = distance
					stats.AncestorPrefix = anyToString(row["retrieval_ancestor_prefix"])
				}
				if len(rows) >= limit {
					break
				}
			}
		}
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	stats.ScopeExhaustive = true
	return rows, stats, nil
}

func nearestTopicAncestor(topicPath string) string {
	normalized := normalizeTopicPathLoose(topicPath)
	separator := strings.LastIndex(normalized, "/")
	if separator <= 0 {
		return ""
	}
	return strings.Trim(normalized[:separator], "/")
}

func topicAncestorPrefixes(topicPath string, maxDepth int) []string {
	normalized := normalizeTopicPathLoose(topicPath)
	maxDepth = clampInt(maxDepth, 1, 8)
	prefixes := make([]string, 0, maxDepth)
	for len(prefixes) < maxDepth {
		normalized = nearestTopicAncestor(normalized)
		if normalized == "" {
			break
		}
		prefixes = append(prefixes, normalized)
	}
	return prefixes
}

func currentStateLexicalScore(query string, topicPath string, summary string) float64 {
	if normalized, changed := deriveCoverageRescueQuery(query, 2); changed {
		query = normalized
	}
	normalizedTopic := strings.ReplaceAll(normalizeTopicPathLoose(topicPath), "/", " ")
	body := strings.TrimSpace(normalizedTopic + " " + strings.TrimSpace(summary))
	queryTokens := lexicalTokenSet(query)
	bodyTokens := lexicalTokenSet(body)
	if len(queryTokens) == 0 || len(bodyTokens) == 0 {
		return 0
	}
	matched := 0
	for token := range queryTokens {
		if _, exists := bodyTokens[token]; exists {
			matched++
		}
	}
	if matched == 0 {
		return 0
	}
	queryCoverage := float64(matched) / float64(len(queryTokens))
	union := len(queryTokens) + len(bodyTokens) - matched
	jaccard := 0.0
	if union > 0 {
		jaccard = float64(matched) / float64(union)
	}
	topicCoverage := 0.0
	topicTokens := lexicalTokenSet(normalizedTopic)
	if len(topicTokens) > 0 {
		topicHits := 0
		for token := range topicTokens {
			if _, exists := queryTokens[token]; exists {
				topicHits++
			}
		}
		topicCoverage = float64(topicHits) / float64(len(topicTokens))
	}
	score := queryCoverage*0.72 + jaccard*0.20 + topicCoverage*0.08
	if strings.Contains(strings.ToLower(body), strings.ToLower(strings.TrimSpace(query))) {
		score = 1.0
	}
	if score > 1 {
		return 1
	}
	return score
}

func currentStateSearchAsOfSupported(raw any) bool {
	value := strings.TrimSpace(anyToString(raw))
	if value == "" {
		return true
	}
	asOf, ok := parseTimeBestEffort(value)
	return ok && !asOf.Before(time.Now().UTC())
}
