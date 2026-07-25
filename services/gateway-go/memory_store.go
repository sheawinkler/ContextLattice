package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type memoryStorePolicy struct {
	enabled                    bool
	rootPath                   string
	historyPath                string
	currentStatePath           string
	accessLogPath              string
	edgePath                   string
	agentEdgePath              string
	contentAddressed           bool
	contentBlobsPath           string
	contentLinkMode            string
	exactStateIndexPath        string
	exactStateMaxPaths         int
	rollupUseHistoryIndex      bool
	historyStartupMaxLines     int
	historyStartupTailMaxBytes int64
	accessStartupMaxLines      int
	edgeStartupMaxLines        int
	agentEdgeStartupMaxLines   int
	maxRecent                  int
	maxEdges                   int
	maxAgentEdges              int
	maxEdgeNeighbors           int
	graphExcludeLowValue       bool
	graphExcludeTopicPrefixes  []string
	graphExcludeFilePatterns   []string
	graphExcludeFileSuffixes   []string
	graphExcludeRootJSON       []string
	scanLimit                  int
	maxSummaryChars            int
	maxRollupSnippets          int
	maxRollupReadBytes         int64
	maxRollupFileBytes         int64
	rollupCacheTTL             time.Duration
	hotIndexMaxAgeDays         int
	userHorizonEnabled         bool
	userHorizonTagPrefix       string
	confidencePriorAlpha       float64
	confidencePriorBeta        float64
	confidenceWriteWeight      float64
	confidenceReadWeight       float64
}

type memoryStoreEntry struct {
	EventID     string   `json:"event_id"`
	Project     string   `json:"project"`
	FileName    string   `json:"file"`
	TopicPath   string   `json:"topic_path,omitempty"`
	AgentID     string   `json:"agent_id,omitempty"`
	SessionID   string   `json:"session_id,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	ContentHash string   `json:"content_hash,omitempty"`
	ContentRef  string   `json:"content_ref,omitempty"`
	DataClass   string   `json:"data_class,omitempty"`
	Lifecycle   string   `json:"lifecycle,omitempty"`
	StorageTier string   `json:"storage_tier,omitempty"`
	ObjectID    string   `json:"object_id,omitempty"`
	HorizonDays int      `json:"horizon_days,omitempty"`
	DiffState   string   `json:"diff_state,omitempty"`
	DiffDelta   float64  `json:"diff_delta,omitempty"`
	Confidence  float64  `json:"confidence,omitempty"`
	LastAccess  string   `json:"last_accessed_at,omitempty"`
	CreatedAt   string   `json:"created_at"`
	RawBytes    int      `json:"raw_bytes,omitempty"`
	Source      string   `json:"source,omitempty"`
}

type memoryStoreDoc struct {
	Project     string
	FileName    string
	TopicPath   string
	Summary     string
	UpdatedAt   time.Time
	ObjectID    string
	Horizon     int
	Score       float64
	LastTouch   time.Time
	Lifecycle   string
	StorageTier string
}

type memoryAccessLogEntry struct {
	Project    string `json:"project"`
	FileName   string `json:"file"`
	Reason     string `json:"reason,omitempty"`
	AccessedAt string `json:"accessed_at"`
}

type topicRollupAggregate struct {
	path            string
	project         string
	depth           int
	eventCount      int
	recentCount     int
	confidenceSum   float64
	confidenceCount int
	maxHorizonDays  int
	uniqueFiles     map[string]struct{}
	uniqueAgents    map[string]struct{}
	uniqueSessions  map[string]struct{}
	lifecycleCounts map[string]int
	diffStateCounts map[string]int
	rawBytes        int
	latestAt        time.Time
	summarySnips    []string
	children        map[string]struct{}
	filePartitions  []map[string]any
}

type topicRollupSignal struct {
	recentCount        int
	writeCount         int
	unattributedWrites int
	uniqueAgents       map[string]struct{}
	uniqueSessions     map[string]struct{}
	lifecycleCounts    map[string]int
	diffStateCounts    map[string]int
	latestAt           time.Time
	rawBytes           int
}

type memoryStore struct {
	policy               memoryStorePolicy
	ready                atomic.Bool
	migration            *ownerOnlyMigrationRuntime
	migrationOnce        sync.Once
	initializeOnce       sync.Once
	initializeErr        error
	mu                   sync.RWMutex
	recent               []memoryStoreEntry
	currentState         map[string]memoryCurrentState
	latestTopic          map[string]string
	latestHash           map[string]string
	latestHorizon        map[string]int
	latestLifecycle      map[string]string
	latestStorageTier    map[string]string
	lastAccess           map[string]time.Time
	confidence           map[string]confidenceState
	rollupCache          map[string]topicRollupCacheEntry
	edges                map[string]memoryEdgeEntry
	edgeOrder            []string
	edgeOrdinal          map[string]int64
	nextEdgeOrdinal      int64
	edgeAdjacency        map[string]map[string]struct{}
	agentEdges           map[string]AgentEventEdge
	agentEdgeOrder       []string
	exactStatePaths      map[string]struct{}
	exactStateCount      atomic.Int64
	pathLocksMu          sync.Mutex
	pathLocks            map[string]*memoryPathLock
	beforeInitialize     func()
	beforeOrdinaryCommit func()
	beforeEdgeCommit     func()
}

type memoryPathLock struct {
	mu   sync.Mutex
	refs int
}

type confidenceState struct {
	alpha float64
	beta  float64
}

type topicRollupCacheEntry struct {
	generatedAt time.Time
	total       int
	topics      []map[string]any
}

func loadMemoryStorePolicy() memoryStorePolicy {
	root := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_ROOT"))
	if root == "" {
		root = strings.TrimSpace(os.Getenv("MEMORY_BANK_ROOT"))
	}
	if root == "" {
		root = filepath.Join(os.TempDir(), "contextlattice-memory-bank")
	}
	root = filepath.Clean(root)

	historyPath := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_HISTORY_PATH"))
	if historyPath == "" {
		historyPath = filepath.Join(root, "_contextlattice", "memory_write_history.ndjson")
	}
	currentStatePath := filepath.Join(root, "_contextlattice", "memory_current_state")
	accessLogPath := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_ACCESS_LOG_PATH"))
	if accessLogPath == "" {
		accessLogPath = filepath.Join(root, "_contextlattice", "memory_access_log.ndjson")
	}
	edgePath := strings.TrimSpace(os.Getenv("GO_MEMORY_GRAPH_EDGE_PATH"))
	if edgePath == "" {
		edgePath = filepath.Join(root, "_contextlattice", "memory_edges.ndjson")
	}
	agentEdgePath := strings.TrimSpace(os.Getenv("GO_MEMORY_AGENT_EDGE_PATH"))
	if agentEdgePath == "" {
		agentEdgePath = filepath.Join(root, "_contextlattice", "memory_agent_event_edges.ndjson")
	}
	contentBlobsPath := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH"))
	if contentBlobsPath == "" {
		contentBlobsPath = filepath.Join(root, "_contextlattice", "content_blobs")
	}
	contentLinkMode := strings.ToLower(strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_CONTENT_LINK_MODE")))
	switch contentLinkMode {
	case "", "hardlink":
		contentLinkMode = "hardlink"
	case "copy", "symlink":
	default:
		contentLinkMode = "hardlink"
	}
	exactStateIndexPath := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_EXACT_STATE_INDEX_PATH"))
	if exactStateIndexPath == "" {
		exactStateIndexPath = filepath.Join(root, "_contextlattice", "exact_state_paths.json")
	}
	historyStartupMaxLines := envInt("GO_MEMORY_STORE_HISTORY_STARTUP_MAX_LINES", 20000)
	if historyStartupMaxLines < 0 {
		historyStartupMaxLines = 0
	}
	historyStartupTailMaxBytes := int64(clampInt(
		envInt("GO_MEMORY_STORE_HISTORY_STARTUP_TAIL_MAX_BYTES", 64*1024*1024),
		1024*1024,
		1024*1024*1024,
	))
	accessStartupMaxLines := envInt("GO_MEMORY_STORE_ACCESS_STARTUP_MAX_LINES", 50000)
	if accessStartupMaxLines < 0 {
		accessStartupMaxLines = 0
	}
	edgeStartupMaxLines := envInt("GO_MEMORY_GRAPH_EDGE_STARTUP_MAX_LINES", 50000)
	if edgeStartupMaxLines < 0 {
		edgeStartupMaxLines = 0
	}
	agentEdgeStartupMaxLines := envInt("GO_MEMORY_AGENT_EDGE_STARTUP_MAX_LINES", 50000)
	if agentEdgeStartupMaxLines < 0 {
		agentEdgeStartupMaxLines = 0
	}
	hotIndexMaxAgeDays := envInt("GO_MEMORY_STORE_HOT_INDEX_MAX_AGE_DAYS", 0)
	if hotIndexMaxAgeDays < 0 {
		hotIndexMaxAgeDays = 0
	}
	if hotIndexMaxAgeDays > 36500 {
		hotIndexMaxAgeDays = 36500
	}
	userHorizonTagPrefix := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_HORIZON_TAG_PREFIX"))
	if userHorizonTagPrefix == "" {
		userHorizonTagPrefix = "horizon_days:"
	}
	confidencePriorAlpha := envFloat("GO_MEMORY_STORE_CONFIDENCE_PRIOR_ALPHA", 1.0)
	confidencePriorBeta := envFloat("GO_MEMORY_STORE_CONFIDENCE_PRIOR_BETA", 1.0)
	confidenceWriteWeight := envFloat("GO_MEMORY_STORE_CONFIDENCE_WRITE_WEIGHT", 0.5)
	confidenceReadWeight := envFloat("GO_MEMORY_STORE_CONFIDENCE_READ_WEIGHT", 1.0)
	if confidencePriorAlpha <= 0 {
		confidencePriorAlpha = 1.0
	}
	if confidencePriorBeta <= 0 {
		confidencePriorBeta = 1.0
	}
	if confidenceWriteWeight <= 0 {
		confidenceWriteWeight = 0.5
	}
	if confidenceReadWeight <= 0 {
		confidenceReadWeight = 1.0
	}
	return memoryStorePolicy{
		enabled:                    envBool("GO_MEMORY_STORE_ENABLED", true),
		rootPath:                   root,
		historyPath:                filepath.Clean(historyPath),
		currentStatePath:           filepath.Clean(currentStatePath),
		accessLogPath:              filepath.Clean(accessLogPath),
		edgePath:                   filepath.Clean(edgePath),
		agentEdgePath:              filepath.Clean(agentEdgePath),
		contentAddressed:           envBool("GO_MEMORY_STORE_CONTENT_ADDRESSING_ENABLED", true),
		contentBlobsPath:           filepath.Clean(contentBlobsPath),
		contentLinkMode:            contentLinkMode,
		exactStateIndexPath:        filepath.Clean(exactStateIndexPath),
		exactStateMaxPaths:         clampInt(envInt("GO_MEMORY_STORE_EXACT_STATE_MAX_PATHS", 10000), 1, 100000),
		rollupUseHistoryIndex:      envBool("GO_MEMORY_STORE_ROLLUP_USE_HISTORY_INDEX", true),
		historyStartupMaxLines:     historyStartupMaxLines,
		historyStartupTailMaxBytes: historyStartupTailMaxBytes,
		accessStartupMaxLines:      accessStartupMaxLines,
		edgeStartupMaxLines:        edgeStartupMaxLines,
		agentEdgeStartupMaxLines:   agentEdgeStartupMaxLines,
		maxRecent:                  clampInt(envInt("GO_MEMORY_STORE_MAX_RECENT", 6000), 64, 100000),
		maxEdges:                   clampInt(envInt("GO_MEMORY_GRAPH_EDGE_MAX", 100000), 100, 1000000),
		maxAgentEdges:              clampInt(envInt("GO_MEMORY_AGENT_EDGE_MAX", 100000), 100, 1000000),
		maxEdgeNeighbors:           clampInt(envInt("GO_MEMORY_GRAPH_EDGE_NEIGHBOR_MAX", 200), 1, 1000),
		graphExcludeLowValue:       envBool("GO_MEMORY_GRAPH_EXCLUDE_LOW_VALUE", true),
		graphExcludeTopicPrefixes:  memoryGraphCSVEnvWithFallback("GO_MEMORY_GRAPH_EXCLUDE_TOPIC_PREFIXES", "LOW_VALUE_TOPIC_PREFIXES", "telemetry,metrics,signals,overrides,perf,tmp,state,states,snapshots,health,stats,allocations,system_state,logs,log,debug,trace,queue"),
		graphExcludeFilePatterns:   memoryGraphCSVEnvWithFallback("GO_MEMORY_GRAPH_EXCLUDE_FILE_PATTERNS", "LOW_VALUE_FILE_PATTERNS", "index__*.json,*_agg-latest.json,*_agg-*.json,*__agg-*.json,telemetry__*.json,*__state__*.json,*__stats__*.json,*__snapshots__*.json,*__health__*.json,*__allocations__*.json,*__import-*.json,*__imports__*.json,*.log,*.ndjson,*.jsonl"),
		graphExcludeFileSuffixes:   memoryGraphCSVEnvWithFallback("GO_MEMORY_GRAPH_EXCLUDE_FILE_SUFFIXES", "LOW_VALUE_FILE_SUFFIXES", "__latest.json,__rollup.json,.log,.ndjson,.jsonl"),
		graphExcludeRootJSON:       memoryGraphCSVEnvWithFallback("GO_MEMORY_GRAPH_EXCLUDE_ROOT_JSON_PREFIXES", "LETTA_LOW_VALUE_ROOT_JSON_PREFIXES", "arena__,risk__,dex__,operating_mode__,router__,portfolio__,positions__,strategy__,orderbook__,pricing__,discovery__,attribution__,exits__,trades__"),
		scanLimit:                  clampInt(envInt("GO_MEMORY_STORE_SCAN_LIMIT", 250000), 256, 1000000),
		maxSummaryChars:            clampInt(envInt("GO_MEMORY_STORE_MAX_SUMMARY_CHARS", 400), 80, 4000),
		maxRollupSnippets:          clampInt(envInt("GO_MEMORY_STORE_ROLLUP_SNIPPETS", 3), 1, 8),
		maxRollupReadBytes: int64(clampInt(
			envInt("GO_MEMORY_STORE_ROLLUP_MAX_READ_BYTES", 65536),
			1024,
			4*1024*1024,
		)),
		maxRollupFileBytes: int64(clampInt(
			envInt("GO_MEMORY_STORE_ROLLUP_MAX_FILE_BYTES", 2*1024*1024),
			1024,
			64*1024*1024,
		)),
		rollupCacheTTL:        envDurationSeconds("GO_MEMORY_STORE_ROLLUP_CACHE_TTL_SECS", 15),
		hotIndexMaxAgeDays:    hotIndexMaxAgeDays,
		userHorizonEnabled:    envBool("GO_MEMORY_STORE_USER_HORIZON_ENABLED", true),
		userHorizonTagPrefix:  userHorizonTagPrefix,
		confidencePriorAlpha:  confidencePriorAlpha,
		confidencePriorBeta:   confidencePriorBeta,
		confidenceWriteWeight: confidenceWriteWeight,
		confidenceReadWeight:  confidenceReadWeight,
	}
}

func newMemoryStoreFromEnv() (*memoryStore, error) {
	policy := loadMemoryStorePolicy()
	store := &memoryStore{
		policy:            policy,
		recent:            make([]memoryStoreEntry, 0, policy.maxRecent),
		currentState:      map[string]memoryCurrentState{},
		latestTopic:       map[string]string{},
		latestHash:        map[string]string{},
		latestHorizon:     map[string]int{},
		latestLifecycle:   map[string]string{},
		latestStorageTier: map[string]string{},
		lastAccess:        map[string]time.Time{},
		confidence:        map[string]confidenceState{},
		rollupCache:       map[string]topicRollupCacheEntry{},
		edges:             map[string]memoryEdgeEntry{},
		edgeOrder:         []string{},
		edgeOrdinal:       map[string]int64{},
		edgeAdjacency:     map[string]map[string]struct{}{},
		agentEdges:        map[string]AgentEventEdge{},
		agentEdgeOrder:    []string{},
		exactStatePaths:   map[string]struct{}{},
		pathLocks:         map[string]*memoryPathLock{},
	}
	store.migration = newOwnerOnlyMigrationRuntime(policy.rootPath, policy.enabled)
	if !policy.enabled {
		return store, nil
	}
	// Reject a symlinked store root before registry initialization can follow it.
	if err := ensureOwnerOnlyDirectory(policy.rootPath, false); err != nil {
		store.migration.markBlocked(ownerOnlyMigrationReport{}, err)
		return store, fmt.Errorf("validate memory store root: %w", err)
	}
	// Load the bounded exact-state registry before the corpus migration so every
	// retrieval lane can suppress stale semantic copies during startup.
	if err := store.loadExactStateIndex(); err != nil {
		store.migration.markBlocked(ownerOnlyMigrationReport{}, err)
		return store, fmt.Errorf("load exact state registry: %w", err)
	}
	store.migration.markStarted(false)
	report, err := migrateOwnerOnlyStoreWithOptions(policy.rootPath, ownerOnlyMigrationOptions{
		maxDuration: ownerOnlyMigrationStartupBudget(),
	})
	if err == nil {
		if envBool("GO_MEMORY_STORE_BACKGROUND_HYDRATION_ENABLED", true) {
			store.startOwnerOnlyHydrationBackground(report)
			return store, nil
		}
		store.migration.markHydrating(report, false)
		if err := store.finishOwnerOnlyMigration(report); err != nil {
			return store, fmt.Errorf("initialize memory store after owner-only migration: %w", err)
		}
		return store, nil
	}
	if errors.Is(err, errOwnerOnlyMigrationYield) || errors.Is(err, errOwnerOnlyMigrationLocked) {
		store.migration.markWaiting(report, err)
		if envBool("CONTEXTLATTICE_OWNER_ONLY_MIGRATION_BACKGROUND_ENABLED", true) {
			store.startOwnerOnlyMigrationBackground()
			return store, nil
		}
		store.migration.markBlocked(report, err)
		return store, fmt.Errorf("owner-only migration requires background continuation: %w", err)
	}
	store.migration.markBlocked(report, err)
	return store, fmt.Errorf("migrate memory store to owner-only access: %w", err)
}

func (m *memoryStore) isConfigured() bool {
	return m != nil && m.policy.enabled
}

func (m *memoryStore) isEnabled() bool {
	return m != nil && m.policy.enabled && m.ready.Load()
}

func (m *memoryStore) loadHistory() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	exactStatePaths := m.exactStatePathsSnapshot()
	writePolicy := loadWriteIngressPolicy()
	dirtyCurrentStateShards := map[int]struct{}{}
	recordLoadedEntry := func(entry memoryStoreEntry) {
		m.recordEntry(entry)
		key := memoryStoreKey(entry.Project, entry.FileName)
		if key != "::" {
			dirtyCurrentStateShards[memoryCurrentStateShardForKey(key)] = struct{}{}
		}
	}
	persistLoadedCurrentState := func() error {
		if err := m.persistCurrentStateShardsLocked(dirtyCurrentStateShards); err != nil {
			return fmt.Errorf("persist memory current state after history load: %w", err)
		}
		return nil
	}
	file, err := os.Open(m.policy.historyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open memory store history: %w", err)
	}
	defer file.Close()

	maxStartupLines := m.policy.historyStartupMaxLines
	if maxStartupLines > 0 {
		ordered, err := readHistoryTailLines(file, maxStartupLines, m.policy.historyStartupTailMaxBytes)
		if err != nil {
			return fmt.Errorf("read memory store history tail: %w", err)
		}
		loaded := 0
		for _, line := range ordered {
			var entry memoryStoreEntry
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}
			if entry.DataClass == dataClassRuntimeStateMirror ||
				exactStatePathSetContains(exactStatePaths, entry.Project, entry.FileName) ||
				writePolicy.isDurableMemoryFile(normalizedWrite{project: entry.Project, fileName: entry.FileName}) {
				continue
			}
			recordLoadedEntry(entry)
			loaded += 1
		}
		if err := persistLoadedCurrentState(); err != nil {
			return err
		}
		log.Printf(
			"gateway-go memory store history startup load: scanned=%d loaded=%d cap=%d mode=tail",
			len(ordered),
			loaded,
			maxStartupLines,
		)
		return nil
	}

	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 0, 1024*64)
	scanner.Buffer(buffer, 1024*1024)
	loaded := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry memoryStoreEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		if entry.DataClass == dataClassRuntimeStateMirror ||
			exactStatePathSetContains(exactStatePaths, entry.Project, entry.FileName) ||
			writePolicy.isDurableMemoryFile(normalizedWrite{project: entry.Project, fileName: entry.FileName}) {
			continue
		}
		recordLoadedEntry(entry)
		loaded += 1
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan memory store history: %w", err)
	}
	if err := persistLoadedCurrentState(); err != nil {
		return err
	}
	log.Printf("gateway-go memory store history startup load: scanned=%d loaded=%d cap=%d", loaded, loaded, 0)
	return nil
}

func readHistoryTailLines(file *os.File, maxLines int, maxBytes int64) ([]string, error) {
	if file == nil || maxLines <= 0 {
		return []string{}, nil
	}
	if maxBytes < 1 {
		maxBytes = 64 * 1024 * 1024
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size <= 0 {
		return []string{}, nil
	}
	const chunkSize int64 = 64 * 1024
	buf := make([]byte, 0, minInt64(size, maxBytes))
	pos := size
	newlineCount := 0
	for pos > 0 && newlineCount <= maxLines {
		readSize := chunkSize
		if pos < readSize {
			readSize = pos
		}
		pos -= readSize
		if _, err := file.Seek(pos, io.SeekStart); err != nil {
			return nil, err
		}
		chunk := make([]byte, readSize)
		n, readErr := io.ReadFull(file, chunk)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return nil, readErr
		}
		if n <= 0 {
			break
		}
		chunk = chunk[:n]
		available := int(maxBytes) - len(buf)
		if available <= 0 {
			break
		}
		if len(chunk) > available {
			chunk = chunk[len(chunk)-available:]
			pos = 0
		}
		buf = append(chunk, buf...)
		newlineCount = bytes.Count(buf, []byte{'\n'})
		if int64(len(buf)) >= maxBytes {
			break
		}
	}
	linesRaw := strings.Split(string(buf), "\n")
	if pos > 0 && len(linesRaw) > 0 {
		// Buffer started mid-line; drop partial head for valid NDJSON parsing.
		linesRaw = linesRaw[1:]
	}
	lines := make([]string, 0, len(linesRaw))
	for _, line := range linesRaw {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lines = append(lines, trimmed)
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines, nil
}

func parseHorizonDaysFromTags(tags []string, prefix string) (int, bool) {
	basePrefix := strings.ToLower(strings.TrimSpace(prefix))
	if basePrefix == "" {
		basePrefix = "horizon_days:"
	}
	for _, raw := range tags {
		tag := strings.TrimSpace(strings.ToLower(raw))
		if tag == "" {
			continue
		}
		if !strings.HasPrefix(tag, basePrefix) {
			continue
		}
		suffix := strings.TrimSpace(strings.TrimPrefix(tag, basePrefix))
		if suffix == "" {
			continue
		}
		value, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		if value < 0 {
			value = 0
		}
		if value > 36500 {
			value = 36500
		}
		return value, true
	}
	return 0, false
}

func tokenSetSimilarity(left string, right string) float64 {
	left = strings.TrimSpace(strings.ToLower(left))
	right = strings.TrimSpace(strings.ToLower(right))
	if left == right {
		return 1.0
	}
	leftSet := map[string]struct{}{}
	for _, token := range strings.Fields(left) {
		if token == "" {
			continue
		}
		leftSet[token] = struct{}{}
	}
	rightSet := map[string]struct{}{}
	for _, token := range strings.Fields(right) {
		if token == "" {
			continue
		}
		rightSet[token] = struct{}{}
	}
	if len(leftSet) == 0 && len(rightSet) == 0 {
		return 1.0
	}
	if len(leftSet) == 0 || len(rightSet) == 0 {
		return 0.0
	}
	union := map[string]struct{}{}
	for token := range leftSet {
		union[token] = struct{}{}
	}
	for token := range rightSet {
		union[token] = struct{}{}
	}
	shared := 0
	for token := range leftSet {
		if _, ok := rightSet[token]; ok {
			shared += 1
		}
	}
	return float64(shared) / float64(len(union))
}

func diffStateFromDelta(delta float64) string {
	if delta <= 0.03 {
		return "unchanged"
	}
	if delta <= 0.35 {
		return "revision"
	}
	return "rewrite"
}

func (m *memoryStore) objectIDFor(project string, fileName string, topicPath string, contentHash string) string {
	seed := strings.ToLower(strings.TrimSpace(project)) + "|" +
		strings.ToLower(strings.TrimSpace(fileName)) + "|" +
		strings.ToLower(strings.TrimSpace(topicPath)) + "|" +
		strings.ToLower(strings.TrimSpace(contentHash))
	digest := sha256Hex(seed)
	if len(digest) > 24 {
		digest = digest[:24]
	}
	return "obj_" + digest
}

func (m *memoryStore) loadAccessLog() error {
	if m == nil || !m.isConfigured() {
		return nil
	}
	file, err := os.Open(m.policy.accessLogPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("open memory store access log: %w", err)
	}
	defer file.Close()

	limit := m.policy.accessStartupMaxLines
	lines := []string{}
	if limit > 0 {
		lines, err = readHistoryTailLines(file, limit, 32*1024*1024)
		if err != nil {
			return fmt.Errorf("read memory store access log tail: %w", err)
		}
	} else {
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				lines = append(lines, line)
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("scan memory store access log: %w", err)
		}
	}

	loaded := 0
	for _, line := range lines {
		entry := memoryAccessLogEntry{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}
		project, err := sanitizeMemoryProject(entry.Project)
		if err != nil {
			continue
		}
		fileName, err := sanitizeMemoryFile(entry.FileName)
		if err != nil {
			continue
		}
		accessedAt, ok := parseTimeBestEffort(entry.AccessedAt)
		if !ok {
			continue
		}
		key := memoryStoreKey(project, fileName)
		if key == "::" {
			continue
		}
		if current, ok := m.lastAccess[key]; !ok || accessedAt.After(current) {
			m.lastAccess[key] = accessedAt
			loaded += 1
		}
	}
	if loaded > 0 {
		log.Printf("gateway-go memory store access log startup load: loaded=%d cap=%d", loaded, limit)
	}
	return nil
}

func (m *memoryStore) appendAccessLog(project string, fileName string, reason string, at time.Time) {
	if m == nil || !m.isEnabled() {
		return
	}
	entry := memoryAccessLogEntry{
		Project:    project,
		FileName:   fileName,
		Reason:     strings.TrimSpace(reason),
		AccessedAt: at.UTC().Format(time.RFC3339Nano),
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line := append(payload, '\n')
	file, err := openOwnerOnlyAppend(m.policy.accessLogPath, true)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(line)
}

func (m *memoryStore) markAccess(project string, fileName string, reason string) {
	if m == nil || !m.isEnabled() {
		return
	}
	at := time.Now().UTC()
	key := memoryStoreKey(project, fileName)
	if key == "::" {
		return
	}
	m.mu.Lock()
	current, ok := m.lastAccess[key]
	if !ok || at.After(current) {
		m.lastAccess[key] = at
	}
	if state, ok := m.confidence[key]; ok {
		state.alpha += m.policy.confidenceReadWeight
		m.confidence[key] = state
	}
	m.mu.Unlock()
	m.appendAccessLog(project, fileName, reason, at)
}

func sanitizeMemoryProject(project string) (string, error) {
	token := strings.TrimSpace(project)
	if token == "" {
		return "", errors.New("project must be non-empty")
	}
	if strings.Contains(token, "/") || strings.Contains(token, "\\") {
		return "", errors.New("project must not contain path separators")
	}
	if token == "." || token == ".." {
		return "", errors.New("project path is invalid")
	}
	if strings.HasPrefix(token, "_") {
		return "", errors.New("project token must not start with underscore")
	}
	if strings.Contains(token, "::") {
		return "", errors.New("project must not contain the memory key delimiter")
	}
	return token, nil
}

func sanitizeMemoryFile(fileName string) (string, error) {
	trimmed := strings.TrimSpace(strings.ReplaceAll(fileName, "\\", "/"))
	if trimmed == "" {
		return "", errors.New("fileName must be non-empty")
	}
	trimmed = strings.TrimPrefix(trimmed, "/")
	parts := strings.Split(trimmed, "/")
	clean := make([]string, 0, len(parts))
	for _, part := range parts {
		token := strings.TrimSpace(part)
		if token == "" || token == "." {
			continue
		}
		if token == ".." {
			return "", errors.New("fileName must not contain traversal segments")
		}
		clean = append(clean, token)
	}
	if len(clean) == 0 {
		return "", errors.New("fileName must include a file path")
	}
	canonical := strings.Join(clean, "/")
	if strings.Contains(canonical, "::") {
		return "", errors.New("fileName must not contain the memory key delimiter")
	}
	return canonical, nil
}

func sanitizeTopicPath(topicPath string, fallbackFile string) string {
	normalized, ok := normalizeBrowserTopicPath(topicPath)
	if ok && strings.TrimSpace(normalized) != "" {
		return normalized
	}
	derived := deriveTopicFromFile(fallbackFile)
	if derived == "" {
		return "root"
	}
	return derived
}

func deriveTopicFromFile(fileName string) string {
	normalized := strings.TrimSpace(strings.ReplaceAll(fileName, "\\", "/"))
	normalized = strings.TrimPrefix(normalized, "/")
	if normalized == "" {
		return "root"
	}
	dir := strings.TrimSpace(filepath.ToSlash(filepath.Dir(normalized)))
	dir = strings.Trim(dir, "/")
	if dir == "" || dir == "." {
		return "root"
	}
	return dir
}

func memoryStoreKey(project string, fileName string) string {
	return strings.ToLower(strings.TrimSpace(project)) + "::" + strings.ToLower(strings.TrimSpace(fileName))
}

func (m *memoryStore) lockMemoryPath(key string) func() {
	if m == nil {
		return func() {}
	}
	m.pathLocksMu.Lock()
	if m.pathLocks == nil {
		m.pathLocks = map[string]*memoryPathLock{}
	}
	lock := m.pathLocks[key]
	if lock == nil {
		lock = &memoryPathLock{}
		m.pathLocks[key] = lock
	}
	lock.refs++
	m.pathLocksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.pathLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(m.pathLocks, key)
		}
		m.pathLocksMu.Unlock()
	}
}

func (m *memoryStore) lockMemoryPaths(keys ...string) func() {
	unique := make(map[string]struct{}, len(keys))
	ordered := make([]string, 0, len(keys))
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := unique[key]; exists {
			continue
		}
		unique[key] = struct{}{}
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	unlocks := make([]func(), 0, len(ordered))
	for _, key := range ordered {
		unlocks = append(unlocks, m.lockMemoryPath(key))
	}
	return func() {
		for idx := len(unlocks) - 1; idx >= 0; idx-- {
			unlocks[idx]()
		}
	}
}

func clipSummary(content string, maxChars int) string {
	if maxChars < 80 {
		maxChars = 80
	}
	return clipText(strings.TrimSpace(content), maxChars)
}

func readFileHeadWithContext(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return []byte{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	const chunkSize = 32 * 1024
	buffer := make([]byte, 0, minInt64(maxBytes, 64*1024))
	chunk := make([]byte, chunkSize)
	remaining := maxBytes
	for remaining > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		toRead := len(chunk)
		if int64(toRead) > remaining {
			toRead = int(remaining)
		}
		n, readErr := file.Read(chunk[:toRead])
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			remaining -= int64(n)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return buffer, nil
}

func minInt64(left int64, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func (m *memoryStore) recordEntry(entry memoryStoreEntry) {
	if m == nil {
		return
	}
	if m.latestStorageTier == nil {
		m.latestStorageTier = map[string]string{}
	}
	if strings.TrimSpace(entry.EventID) == "" {
		entry.EventID = bson.NewObjectID().Hex()
	}
	if strings.TrimSpace(entry.CreatedAt) == "" {
		entry.CreatedAt = nowUTCISO()
	}
	if strings.TrimSpace(entry.TopicPath) == "" {
		entry.TopicPath = deriveTopicFromFile(entry.FileName)
	}
	if strings.TrimSpace(entry.Source) == "" {
		entry.Source = "go_memory_store"
	}
	if strings.TrimSpace(entry.Lifecycle) == "" {
		entry.Lifecycle = normalizeMemoryLifecycle(lifecycleFromTags(entry.Tags))
	}
	entry.StorageTier = normalizeMemoryStorageTier(entry.StorageTier)
	key := memoryStoreKey(entry.Project, entry.FileName)
	becameCurrent := m.applyCurrentStateEntryLocked(entry)
	if isMemoryTombstone(entry) {
		if becameCurrent && key != "::" {
			delete(m.latestTopic, key)
			delete(m.latestHash, key)
			delete(m.latestHorizon, key)
			delete(m.latestLifecycle, key)
			delete(m.latestStorageTier, key)
			delete(m.lastAccess, key)
			delete(m.confidence, key)
		}
		if becameCurrent {
			m.invalidateTopicRollupCacheLocked(entry.Project)
		}
		m.recent = append(m.recent, entry)
		if len(m.recent) > m.policy.maxRecent {
			over := len(m.recent) - m.policy.maxRecent
			m.recent = m.recent[over:]
		}
		return
	}
	if becameCurrent && key != "::" {
		m.latestTopic[key] = entry.TopicPath
		m.latestLifecycle[key] = normalizeMemoryLifecycle(entry.Lifecycle)
		m.latestStorageTier[key] = normalizeMemoryStorageTier(entry.StorageTier)
		if strings.TrimSpace(entry.ContentHash) != "" {
			m.latestHash[key] = entry.ContentHash
		}
		if entry.HorizonDays != 0 {
			m.latestHorizon[key] = entry.HorizonDays
		}
		if strings.TrimSpace(entry.LastAccess) != "" {
			if accessedAt, ok := parseTimeBestEffort(entry.LastAccess); ok {
				if current, exists := m.lastAccess[key]; !exists || accessedAt.After(current) {
					m.lastAccess[key] = accessedAt
				}
			}
		}
		if entry.Confidence > 0 {
			alpha := m.policy.confidencePriorAlpha + (entry.Confidence * (m.policy.confidenceReadWeight + m.policy.confidenceWriteWeight))
			beta := m.policy.confidencePriorBeta + ((1.0 - entry.Confidence) * (m.policy.confidenceReadWeight + m.policy.confidenceWriteWeight))
			m.confidence[key] = confidenceState{alpha: alpha, beta: beta}
		}
	}
	if becameCurrent {
		m.invalidateTopicRollupCacheLocked(entry.Project)
	}
	m.recent = append(m.recent, entry)
	if len(m.recent) > m.policy.maxRecent {
		over := len(m.recent) - m.policy.maxRecent
		// Avoid O(n^2) copies during large history replays at startup.
		m.recent = m.recent[over:]
	}
}

func cloneTopicRows(rows []map[string]any) []map[string]any {
	if len(rows) == 0 {
		return []map[string]any{}
	}
	cloned := make([]map[string]any, len(rows))
	copy(cloned, rows)
	return cloned
}

func normalizeRollupProject(project string) string {
	return strings.ToLower(strings.TrimSpace(project))
}

func topicRollupCacheKey(project string, minCount int, includeCold bool, includeEphemeral bool) string {
	if minCount < 1 {
		minCount = 1
	}
	mode := "hot"
	if includeCold {
		mode = "cold"
	}
	lifecycleMode := "durable"
	if includeEphemeral {
		lifecycleMode = "all"
	}
	return normalizeRollupProject(project) + "|" + strconv.Itoa(minCount) + "|" + mode + "|" + lifecycleMode
}

func (m *memoryStore) invalidateTopicRollupCacheLocked(project string) {
	if m == nil || len(m.rollupCache) == 0 {
		return
	}
	prefix := normalizeRollupProject(project)
	for key := range m.rollupCache {
		parts := strings.SplitN(key, "|", 2)
		if len(parts) == 0 {
			continue
		}
		if strings.EqualFold(parts[0], prefix) {
			delete(m.rollupCache, key)
		}
	}
}

func (m *memoryStore) getTopicRollupCache(project string, minCount int, includeCold bool, includeEphemeral bool) (topicRollupCacheEntry, bool) {
	if m == nil || m.policy.rollupCacheTTL <= 0 {
		return topicRollupCacheEntry{}, false
	}
	key := topicRollupCacheKey(project, minCount, includeCold, includeEphemeral)
	now := time.Now()
	m.mu.RLock()
	entry, ok := m.rollupCache[key]
	m.mu.RUnlock()
	if !ok {
		return topicRollupCacheEntry{}, false
	}
	if now.Sub(entry.generatedAt) > m.policy.rollupCacheTTL {
		m.mu.Lock()
		delete(m.rollupCache, key)
		m.mu.Unlock()
		return topicRollupCacheEntry{}, false
	}
	entry.topics = cloneTopicRows(entry.topics)
	return entry, true
}

func (m *memoryStore) putTopicRollupCache(project string, minCount int, includeCold bool, includeEphemeral bool, rows []map[string]any, total int) {
	if m == nil || m.policy.rollupCacheTTL <= 0 {
		return
	}
	key := topicRollupCacheKey(project, minCount, includeCold, includeEphemeral)
	m.mu.Lock()
	m.rollupCache[key] = topicRollupCacheEntry{
		generatedAt: time.Now(),
		total:       total,
		topics:      cloneTopicRows(rows),
	}
	m.mu.Unlock()
}

func (m *memoryStore) appendHistory(entry memoryStoreEntry) error {
	if m == nil || !m.isEnabled() {
		return nil
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode memory history entry: %w", err)
	}
	line := append(payload, '\n')
	file, err := openOwnerOnlyAppend(m.policy.historyPath, true)
	if err != nil {
		return fmt.Errorf("open memory history append: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("append memory history: %w", err)
	}
	return nil
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, ch := range value {
		if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
			continue
		}
		return false
	}
	return true
}

func (m *memoryStore) blobPathForHash(contentHash string) (string, error) {
	if m == nil {
		return "", errors.New("memory store unavailable")
	}
	token := strings.ToLower(strings.TrimSpace(contentHash))
	if !isHexDigest(token) {
		return "", errors.New("invalid content hash")
	}
	prefix := token[:2]
	return filepath.Join(m.policy.contentBlobsPath, prefix, token+".txt"), nil
}

func writeAtomicFile(path string, content []byte, mode fs.FileMode) error {
	tmpPath := path + ".tmp-" + bson.NewObjectID().Hex()
	if err := os.WriteFile(tmpPath, content, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, mode.Perm()); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := replaceOwnerOnlyFile(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Chmod(path, mode.Perm())
}

func copyFileAtomic(src string, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmpPath := dst + ".tmp-" + bson.NewObjectID().Hex()
	out, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := replaceOwnerOnlyFile(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Chmod(dst, mode.Perm())
}

func (m *memoryStore) ensureBlob(contentHash string, content string) (string, error) {
	blobPath, err := m.blobPathForHash(contentHash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(blobPath); err == nil {
		return blobPath, nil
	}
	if err := ensureOwnerOnlyDirectory(filepath.Dir(blobPath), true); err != nil {
		return "", fmt.Errorf("create blob directory: %w", err)
	}
	if err := writeOwnerOnlyAtomicFile(blobPath, []byte(content), true); err != nil {
		if statErr := func() error {
			_, e := os.Stat(blobPath)
			return e
		}(); statErr == nil {
			return blobPath, nil
		}
		return "", fmt.Errorf("write blob file: %w", err)
	}
	return blobPath, nil
}

func (m *memoryStore) linkOrCopyBlob(blobPath string, filePath string) error {
	if err := ensureOwnerOnlyDirectory(filepath.Dir(filePath), true); err != nil {
		return fmt.Errorf("create memory file directory: %w", err)
	}
	tmpPath := filePath + ".tmp-" + bson.NewObjectID().Hex()

	mode := m.policy.contentLinkMode
	if mode == "" {
		mode = "hardlink"
	}
	linkErr := error(nil)
	switch mode {
	case "symlink":
		linkErr = os.Symlink(blobPath, tmpPath)
	case "copy":
		linkErr = copyFileAtomic(blobPath, filePath, ownerOnlyFileMode)
	default:
		linkErr = os.Link(blobPath, tmpPath)
	}

	usedTmpPath := mode != "copy"
	if linkErr != nil && mode != "copy" {
		// Hardlink/symlink can fail on some filesystems or policies; fall back to copy.
		usedTmpPath = false
		linkErr = copyFileAtomic(blobPath, filePath, ownerOnlyFileMode)
	}
	if linkErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("materialize memory content from blob: %w", linkErr)
	}
	if usedTmpPath {
		if err := replaceOwnerOnlyFile(tmpPath, filePath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("commit linked memory file: %w", err)
		}
	}
	return nil
}

func (m *memoryStore) putExactStateMirror(item normalizedWrite) (memoryStoreEntry, bool, error) {
	project, err := sanitizeMemoryProject(item.project)
	if err != nil {
		return memoryStoreEntry{}, false, err
	}
	fileName, err := sanitizeMemoryFile(item.fileName)
	if err != nil {
		return memoryStoreEntry{}, false, err
	}
	if err := m.registerExactStatePathLocked(project, fileName); err != nil {
		return memoryStoreEntry{}, false, err
	}
	content := item.content
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	filePath := filepath.Join(m.policy.rootPath, project, filepath.FromSlash(fileName))
	if err := ensureOwnerOnlyDirectory(filepath.Dir(filePath), true); err != nil {
		return memoryStoreEntry{}, false, fmt.Errorf("create exact state mirror directory: %w", err)
	}
	if err := ensureOwnerOnlyFile(filePath); err != nil {
		return memoryStoreEntry{}, false, fmt.Errorf("prepare exact state mirror: %w", err)
	}
	previous := ""
	if raw, readErr := os.ReadFile(filePath); readErr == nil {
		previous = string(raw)
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return memoryStoreEntry{}, false, fmt.Errorf("read exact state mirror: %w", readErr)
	}
	deduped := previous == content
	if !deduped {
		if err := writeOwnerOnlyDurableAtomicFile(filePath, []byte(content), true); err != nil {
			return memoryStoreEntry{}, false, fmt.Errorf("commit exact state mirror: %w", err)
		}
	}

	diffState := "new"
	diffDelta := 1.0
	if previous != "" {
		if deduped {
			diffState = "unchanged"
			diffDelta = 0
		} else {
			diffDelta = 1 - tokenSetSimilarity(previous, content)
			if diffDelta < 0 {
				diffDelta = 0
			}
			if diffDelta > 1 {
				diffDelta = 1
			}
			diffState = diffStateFromDelta(diffDelta)
		}
	}
	topicPath := sanitizeTopicPath(item.topicPath, fileName)
	contentHash := sha256Hex(content)
	nowISO := nowUTCISO()
	return memoryStoreEntry{
		EventID:     bson.NewObjectID().Hex(),
		Project:     project,
		FileName:    fileName,
		TopicPath:   topicPath,
		AgentID:     item.agentID,
		SessionID:   item.sessionID,
		Tags:        append([]string{}, item.tags...),
		Summary:     clipSummary(content, m.policy.maxSummaryChars),
		ContentHash: contentHash,
		ContentRef:  "sha256:" + contentHash,
		DataClass:   dataClassRuntimeStateMirror,
		Lifecycle:   normalizeMemoryLifecycle(item.lifecycle),
		StorageTier: normalizeMemoryStorageTier(item.storageTier),
		ObjectID:    m.objectIDFor(project, fileName, topicPath, dataClassRuntimeStateMirror),
		HorizonDays: -1,
		DiffState:   diffState,
		DiffDelta:   diffDelta,
		Confidence:  1,
		LastAccess:  nowISO,
		CreatedAt:   nowISO,
		RawBytes:    len(content),
		Source:      "go_exact_state_mirror",
	}, deduped, nil
}

func (m *memoryStore) put(item normalizedWrite) (memoryStoreEntry, bool, error) {
	if m == nil || !m.isEnabled() {
		return memoryStoreEntry{}, false, errors.New("go memory store is disabled")
	}
	project, err := sanitizeMemoryProject(item.project)
	if err != nil {
		return memoryStoreEntry{}, false, err
	}
	fileName, err := sanitizeMemoryFile(item.fileName)
	if err != nil {
		return memoryStoreEntry{}, false, err
	}
	item.project = project
	item.fileName = fileName
	key := memoryStoreKey(project, fileName)
	unlockPath := m.lockMemoryPath(key)
	defer unlockPath()
	if item.dataClass == dataClassRuntimeStateMirror || m.isExactStatePath(project, fileName) {
		item.dataClass = dataClassRuntimeStateMirror
		return m.putExactStateMirror(item)
	}
	content := item.content
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	topicPath := sanitizeTopicPath(item.topicPath, fileName)
	contentHash := sha256Hex(content)
	filePath := filepath.Join(m.policy.rootPath, project, filepath.FromSlash(fileName))
	objectID := m.objectIDFor(project, fileName, topicPath, contentHash)
	now := time.Now().UTC()
	nowISO := now.Format(time.RFC3339Nano)

	horizonDays := 0
	if m.policy.userHorizonEnabled {
		if parsed, ok := parseHorizonDaysFromTags(item.tags, m.policy.userHorizonTagPrefix); ok {
			// -1 means explicit infinite horizon override for this file.
			if parsed == 0 {
				horizonDays = -1
			} else {
				horizonDays = parsed
			}
		}
	}

	m.mu.RLock()
	previousHash := m.latestHash[key]
	previousTopic := m.latestTopic[key]
	previousHorizon := m.latestHorizon[key]
	previousLifecycle := normalizeMemoryLifecycle(m.latestLifecycle[key])
	previousStorageTier := normalizeMemoryStorageTier(m.latestStorageTier[key])
	previousState, previousStateExists := m.currentState[key]
	previousTags := append([]string(nil), previousState.Entry.Tags...)
	confState := m.confidence[key]
	m.mu.RUnlock()
	deduped := previousHash != "" && previousHash == contentHash

	previousContent := ""
	if bytes, readErr := os.ReadFile(filePath); readErr == nil {
		previousContent = string(bytes)
	}
	diffDelta := 1.0
	diffState := "new"
	if previousHash != "" {
		if deduped {
			diffDelta = 0.0
			diffState = "unchanged"
		} else {
			similarity := tokenSetSimilarity(previousContent, content)
			diffDelta = 1.0 - similarity
			if diffDelta < 0 {
				diffDelta = 0
			}
			if diffDelta > 1 {
				diffDelta = 1
			}
			diffState = diffStateFromDelta(diffDelta)
		}
	}

	if confState.alpha <= 0 || confState.beta <= 0 {
		confState = confidenceState{
			alpha: m.policy.confidencePriorAlpha,
			beta:  m.policy.confidencePriorBeta,
		}
	}
	confState.alpha += m.policy.confidenceWriteWeight
	if diffState == "rewrite" {
		confState.beta += m.policy.confidenceWriteWeight * 0.5
	}
	confidence := confState.alpha / (confState.alpha + confState.beta)
	if confidence < 0 {
		confidence = 0
	}
	if confidence > 1 {
		confidence = 1
	}

	buildEntry := func() memoryStoreEntry {
		return memoryStoreEntry{
			EventID:     bson.NewObjectID().Hex(),
			Project:     project,
			FileName:    fileName,
			TopicPath:   topicPath,
			AgentID:     item.agentID,
			SessionID:   item.sessionID,
			Tags:        append([]string{}, item.tags...),
			Summary:     clipSummary(content, m.policy.maxSummaryChars),
			ContentHash: contentHash,
			ContentRef:  "sha256:" + contentHash,
			DataClass:   dataClassLearningMemory,
			Lifecycle:   normalizeMemoryLifecycle(item.lifecycle),
			StorageTier: normalizeMemoryStorageTier(item.storageTier),
			ObjectID:    objectID,
			HorizonDays: horizonDays,
			DiffState:   diffState,
			DiffDelta:   diffDelta,
			Confidence:  confidence,
			LastAccess:  nowISO,
			CreatedAt:   nowISO,
			RawBytes:    len(content),
			Source:      "go_memory_store",
		}
	}

	if deduped &&
		strings.EqualFold(strings.TrimSpace(previousTopic), strings.TrimSpace(topicPath)) &&
		previousLifecycle == normalizeMemoryLifecycle(item.lifecycle) &&
		previousStorageTier == normalizeMemoryStorageTier(item.storageTier) &&
		previousStateExists && memoryTagsEqual(previousTags, item.tags) &&
		horizonDays == 0 &&
		previousHorizon == 0 {
		// No payload or topic change: skip redundant rewrite/history append.
		if _, err := os.Stat(filePath); err == nil {
			m.mu.Lock()
			m.confidence[key] = confState
			m.mu.Unlock()
			m.markAccess(project, fileName, "write_dedup")
			return buildEntry(), true, nil
		}
	}

	if m.beforeOrdinaryCommit != nil {
		m.beforeOrdinaryCommit()
	}
	if m.policy.contentAddressed {
		blobPath, err := m.ensureBlob(contentHash, content)
		if err != nil {
			return memoryStoreEntry{}, false, err
		}
		if err := m.linkOrCopyBlob(blobPath, filePath); err != nil {
			return memoryStoreEntry{}, false, err
		}
	} else {
		if err := ensureOwnerOnlyDirectory(filepath.Dir(filePath), true); err != nil {
			return memoryStoreEntry{}, false, fmt.Errorf("create memory file directory: %w", err)
		}
		if err := writeOwnerOnlyAtomicFile(filePath, []byte(content), true); err != nil {
			return memoryStoreEntry{}, false, fmt.Errorf("commit memory file: %w", err)
		}
	}

	entry := buildEntry()
	if err := m.appendHistory(entry); err != nil {
		return memoryStoreEntry{}, false, err
	}
	if err := m.persistAndRecordEntry(entry); err != nil {
		return memoryStoreEntry{}, false, err
	}
	m.appendAccessLog(project, fileName, "write", now)
	if err := m.storeAgentEdges(entry); err != nil {
		log.Printf("memory agent edge fanout skipped: %v", err)
	}
	return entry, deduped, nil
}

func (m *memoryStore) parseProjectFileFromPath(path string) (string, string, error) {
	normalized := strings.TrimPrefix(strings.TrimSpace(path), "/memory/files/")
	if normalized == "" || normalized == path {
		return "", "", errors.New("memory file path is required")
	}
	parts := strings.Split(normalized, "/")
	if len(parts) < 2 {
		return "", "", errors.New("memory file path must include project and file")
	}
	project := parts[0]
	fileName := strings.Join(parts[1:], "/")
	cleanProject, err := sanitizeMemoryProject(project)
	if err != nil {
		return "", "", err
	}
	cleanFile, err := sanitizeMemoryFile(fileName)
	if err != nil {
		return "", "", err
	}
	return cleanProject, cleanFile, nil
}

func (m *memoryStore) readFile(project string, fileName string) (string, os.FileInfo, error) {
	content, info, cleanProject, cleanFile, err := m.readFileUntracked(project, fileName)
	if err != nil {
		return "", nil, err
	}
	m.markAccess(cleanProject, cleanFile, "read")
	return content, info, nil
}

func (m *memoryStore) readFileUntracked(project string, fileName string) (string, os.FileInfo, string, string, error) {
	if m == nil || !m.isEnabled() {
		return "", nil, "", "", errors.New("go memory store is disabled")
	}
	cleanProject, err := sanitizeMemoryProject(project)
	if err != nil {
		return "", nil, "", "", err
	}
	cleanFile, err := sanitizeMemoryFile(fileName)
	if err != nil {
		return "", nil, "", "", err
	}
	abs := filepath.Join(m.policy.rootPath, cleanProject, filepath.FromSlash(cleanFile))
	bytes, err := os.ReadFile(abs)
	if err != nil {
		return "", nil, "", "", err
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return string(bytes), nil, cleanProject, cleanFile, nil
	}
	return string(bytes), info, cleanProject, cleanFile, nil
}

func (m *memoryStore) purgeEphemeral(project string, topicPath string, filePrefix string, dryRun bool, reason string) (map[string]any, error) {
	if m == nil || !m.isEnabled() {
		return map[string]any{"ok": false, "error": "go memory store is disabled"}, errors.New("go memory store is disabled")
	}
	cleanProject, err := sanitizeMemoryProject(project)
	if err != nil {
		return nil, err
	}
	cleanPrefix, err := sanitizeMemoryFile(filePrefix)
	if err != nil {
		return nil, err
	}
	cleanTopic := strings.Trim(strings.TrimSpace(topicPath), "/")
	if !safeEphemeralPurgeSelector(cleanProject, cleanTopic, cleanPrefix) {
		return nil, errors.New("unsafe ephemeral purge selector")
	}
	if strings.TrimSpace(reason) == "" {
		reason = "ephemeral_memory_purge"
	}

	type candidate struct {
		project   string
		fileName  string
		topicPath string
		lifecycle string
	}
	candidates := []candidate{}
	m.mu.RLock()
	for key, storedTopic := range m.latestTopic {
		rowProject, rowFile, ok := parseMemoryStoreKeyToken(key)
		if !ok {
			continue
		}
		if rowProject != cleanProject {
			continue
		}
		if !strings.HasPrefix(rowFile, cleanPrefix) {
			continue
		}
		if cleanTopic != "" {
			normalizedTopic := strings.Trim(strings.TrimSpace(storedTopic), "/")
			if normalizedTopic != cleanTopic && !strings.HasPrefix(normalizedTopic, cleanTopic+"/") {
				continue
			}
		}
		lifecycle := normalizeMemoryLifecycle(m.latestLifecycle[key])
		if !isEphemeralMemoryIdentity(rowFile, storedTopic, "", lifecycle) {
			continue
		}
		candidates = append(candidates, candidate{
			project:   rowProject,
			fileName:  rowFile,
			topicPath: storedTopic,
			lifecycle: lifecycle,
		})
	}
	m.mu.RUnlock()

	filesDeleted := 0
	filesMissing := 0
	tombstoned := 0
	errorsOut := []string{}
	if !dryRun {
		for _, row := range candidates {
			abs := filepath.Join(m.policy.rootPath, row.project, filepath.FromSlash(row.fileName))
			if err := os.Remove(abs); err == nil {
				filesDeleted += 1
			} else if errors.Is(err, os.ErrNotExist) {
				filesMissing += 1
			} else {
				errorsOut = append(errorsOut, row.fileName+": "+err.Error())
				continue
			}
			now := nowUTCISO()
			tombstone := memoryStoreEntry{
				EventID:   bson.NewObjectID().Hex(),
				Project:   row.project,
				FileName:  row.fileName,
				TopicPath: row.topicPath,
				DataClass: "memory_tombstone",
				Lifecycle: normalizeMemoryLifecycle(row.lifecycle),
				Summary:   reason,
				CreatedAt: now,
				Source:    "go_memory_store",
			}
			if err := m.appendHistory(tombstone); err != nil {
				errorsOut = append(errorsOut, row.fileName+": tombstone append failed: "+err.Error())
				continue
			}
			if err := m.persistAndRecordEntry(tombstone); err != nil {
				errorsOut = append(errorsOut, row.fileName+": tombstone state persist failed: "+err.Error())
				continue
			}
			tombstoned += 1
		}
	}
	return map[string]any{
		"ok":             len(errorsOut) == 0,
		"dry_run":        dryRun,
		"matched":        len(candidates),
		"files_deleted":  filesDeleted,
		"files_missing":  filesMissing,
		"tombstoned":     tombstoned,
		"errors":         errorsOut,
		"project":        cleanProject,
		"topic_path":     cleanTopic,
		"file_prefix":    cleanPrefix,
		"source":         "go_memory_store",
		"purge_semantic": "ephemeral_tombstone",
	}, nil
}

func (m *memoryStore) fetchByMemoryID(memoryID string) (map[string]any, error) {
	project, fileName, err := splitEngineMemoryID(memoryID)
	if err != nil {
		return nil, err
	}
	content, info, err := m.readFile(project, fileName)
	if err != nil {
		return nil, err
	}
	key := memoryStoreKey(project, fileName)
	horizonDays := m.effectiveHotHorizonDays(key)
	confidence := m.confidenceForKey(key)
	lastAccess := ""
	if touched := m.docLastTouch(key, time.Time{}); !touched.IsZero() {
		lastAccess = touched.UTC().Format(time.RFC3339Nano)
	}
	contentHash := sha256Hex(content)
	result := map[string]any{
		"memory_id": memoryID,
		"memory": map[string]any{
			"project":      project,
			"file":         fileName,
			"topic_path":   deriveTopicFromFile(fileName),
			"content":      content,
			"object_id":    m.objectIDFor(project, fileName, deriveTopicFromFile(fileName), contentHash),
			"horizon_days": horizonDays,
			"confidence":   confidence,
			"content_hash": contentHash,
			"content_ref":  "sha256:" + contentHash,
		},
		"source": "go_memory_store",
	}
	if lastAccess != "" {
		result["memory"].(map[string]any)["last_accessed_at"] = lastAccess
	}
	if info != nil {
		result["memory"].(map[string]any)["updated_at"] = info.ModTime().UTC().Format(time.RFC3339Nano)
		result["memory"].(map[string]any)["raw_bytes"] = info.Size()
	}
	return result, nil
}

func (m *memoryStore) recentItems(project string, topicPath string, limit int, offset int) []map[string]any {
	if m == nil || !m.isEnabled() {
		return []map[string]any{}
	}
	cleanProject := strings.TrimSpace(project)
	cleanTopic := strings.Trim(strings.TrimSpace(strings.ToLower(topicPath)), "/")
	if limit < 1 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}

	m.mu.RLock()
	entries := append([]memoryStoreEntry(nil), m.recent...)
	m.mu.RUnlock()

	rows := make([]map[string]any, 0, limit)
	skipped := 0
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		if isMemoryTombstone(entry) {
			continue
		}
		if cleanProject != "" && !strings.EqualFold(strings.TrimSpace(entry.Project), cleanProject) {
			continue
		}
		normEntryTopic := strings.Trim(strings.ToLower(strings.TrimSpace(entry.TopicPath)), "/")
		if cleanTopic != "" {
			if normEntryTopic != cleanTopic && !strings.HasPrefix(normEntryTopic, cleanTopic+"/") {
				continue
			}
		}
		if skipped < offset {
			skipped += 1
			continue
		}
		rows = append(rows, map[string]any{
			"event_id":     entry.EventID,
			"project":      entry.Project,
			"file":         entry.FileName,
			"topic_path":   entry.TopicPath,
			"summary":      entry.Summary,
			"content_hash": entry.ContentHash,
			"data_class":   entry.DataClass,
			"lifecycle":    normalizeMemoryLifecycle(entry.Lifecycle),
			"storage_tier": normalizeMemoryStorageTier(entry.StorageTier),
			"object_id":    entry.ObjectID,
			"horizon_days": entry.HorizonDays,
			"diff_state":   entry.DiffState,
			"diff_delta":   entry.DiffDelta,
			"confidence":   entry.Confidence,
			"last_accessed_at": func() any {
				if strings.TrimSpace(entry.LastAccess) == "" {
					return nil
				}
				return entry.LastAccess
			}(),
			"created_at": entry.CreatedAt,
			"raw_bytes":  entry.RawBytes,
			"source":     entry.Source,
		})
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

func (m *memoryStore) effectiveHotHorizonDays(key string) int {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if key != "::" {
		if specific, ok := m.latestHorizon[key]; ok {
			// -1 represents explicit infinite horizon override.
			if specific < 0 {
				return 0
			}
			return specific
		}
	}
	return m.policy.hotIndexMaxAgeDays
}

func (m *memoryStore) docLastTouch(key string, updatedAt time.Time) time.Time {
	if m == nil {
		return updatedAt
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if key != "::" {
		if accessAt, ok := m.lastAccess[key]; ok && !accessAt.IsZero() {
			if updatedAt.IsZero() || accessAt.After(updatedAt) {
				return accessAt
			}
		}
	}
	return updatedAt
}

func (m *memoryStore) confidenceForKey(key string) float64 {
	if m == nil || key == "::" {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	state, ok := m.confidence[key]
	if !ok {
		return 0
	}
	denominator := state.alpha + state.beta
	if denominator <= 0 {
		return 0
	}
	score := state.alpha / denominator
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func memoryStoreIncludeOptions(options []bool) (includeCold bool, includeEphemeral bool) {
	if len(options) > 0 {
		includeCold = options[0]
	}
	if len(options) > 1 {
		includeEphemeral = options[1]
	}
	return includeCold, includeEphemeral
}

func (m *memoryStore) collectDocs(ctx context.Context, projectFilter string, options ...bool) ([]memoryStoreDoc, error) {
	includeCold, includeEphemeral := memoryStoreIncludeOptions(options)
	if m == nil || !m.isEnabled() {
		return []memoryStoreDoc{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if docs, ok := m.collectDocsFromHistoryIndex(ctx, projectFilter, includeCold, includeEphemeral); ok {
		return docs, nil
	}
	return m.collectDocsFromDisk(ctx, projectFilter, includeCold, includeEphemeral)
}

func (m *memoryStore) collectDocsFromDisk(ctx context.Context, projectFilter string, options ...bool) ([]memoryStoreDoc, error) {
	includeCold, includeEphemeral := memoryStoreIncludeOptions(options)
	if m == nil || !m.isEnabled() {
		return []memoryStoreDoc{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	root := m.policy.rootPath
	if strings.TrimSpace(projectFilter) != "" {
		project, err := sanitizeMemoryProject(projectFilter)
		if err != nil {
			return nil, err
		}
		root = filepath.Join(m.policy.rootPath, project)
	}
	if _, err := os.Stat(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []memoryStoreDoc{}, nil
		}
		return nil, err
	}

	m.mu.RLock()
	latestTopic := make(map[string]string, len(m.latestTopic))
	for key, value := range m.latestTopic {
		latestTopic[key] = value
	}
	latestLifecycle := make(map[string]string, len(m.latestLifecycle))
	for key, value := range m.latestLifecycle {
		latestLifecycle[key] = value
	}
	latestStorageTier := make(map[string]string, len(m.latestStorageTier))
	for key, value := range m.latestStorageTier {
		latestStorageTier[key] = value
	}
	exactStatePaths := make(map[string]struct{}, len(m.exactStatePaths))
	for key := range m.exactStatePaths {
		exactStatePaths[key] = struct{}{}
	}
	m.mu.RUnlock()

	docs := make([]memoryStoreDoc, 0, 1024)
	scanned := 0
	writePolicy := loadWriteIngressPolicy()
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			base := strings.TrimSpace(entry.Name())
			if strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			if strings.EqualFold(base, "_contextlattice") {
				return filepath.SkipDir
			}
			return nil
		}
		scanned += 1
		if scanned > m.policy.scanLimit {
			return errors.New("scan_limit_reached")
		}
		rel, err := filepath.Rel(m.policy.rootPath, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		parts := strings.Split(rel, "/")
		if len(parts) < 2 {
			return nil
		}
		project := parts[0]
		if strings.HasPrefix(project, "_") {
			return nil
		}
		if strings.TrimSpace(projectFilter) != "" && !strings.EqualFold(project, projectFilter) {
			return nil
		}
		fileName := strings.Join(parts[1:], "/")
		if strings.TrimSpace(fileName) == "" {
			return nil
		}
		if exactStatePathSetContains(exactStatePaths, project, fileName) || writePolicy.isDurableMemoryFile(normalizedWrite{project: project, fileName: fileName}) {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return nil
		}
		if info.Size() > m.policy.maxRollupFileBytes {
			return nil
		}
		bytes, readErr := readFileHeadWithContext(ctx, path, m.policy.maxRollupReadBytes)
		if readErr != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		key := memoryStoreKey(project, fileName)
		topic := latestTopic[key]
		if strings.TrimSpace(topic) == "" {
			topic = deriveTopicFromFile(fileName)
		}
		lifecycle := normalizeMemoryLifecycle(latestLifecycle[key])
		storageTier := normalizeMemoryStorageTier(latestStorageTier[key])
		if isEphemeralMemoryIdentity(fileName, topic, string(bytes), lifecycle) {
			lifecycle = "test"
		}
		if !shouldSurfaceMemoryLifecycle(lifecycle, includeEphemeral) {
			return nil
		}
		if !includeCold && (storageTier == "deep" || storageTier == "retired") {
			return nil
		}
		updatedAt := time.Time{}
		if info != nil {
			updatedAt = info.ModTime().UTC()
		}
		lastTouch := m.docLastTouch(key, updatedAt)
		horizonDays := m.effectiveHotHorizonDays(key)
		if !includeCold && horizonDays > 0 && !lastTouch.IsZero() {
			cutoff := time.Now().UTC().Add(-time.Duration(horizonDays) * 24 * time.Hour)
			if lastTouch.Before(cutoff) {
				return nil
			}
		}
		docs = append(docs, memoryStoreDoc{
			Project:     project,
			FileName:    fileName,
			TopicPath:   topic,
			Summary:     clipSummary(string(bytes), m.policy.maxSummaryChars),
			UpdatedAt:   updatedAt,
			ObjectID:    m.objectIDFor(project, fileName, topic, sha256Hex(string(bytes))),
			Horizon:     horizonDays,
			Score:       m.confidenceForKey(key),
			LastTouch:   lastTouch,
			Lifecycle:   lifecycle,
			StorageTier: storageTier,
		})
		return nil
	})
	if walkErr != nil && (errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded)) {
		return nil, walkErr
	}
	if walkErr != nil && !strings.Contains(strings.ToLower(walkErr.Error()), "scan_limit_reached") {
		return nil, walkErr
	}
	return docs, nil
}

func parseMemoryStoreKeyToken(token string) (string, string, bool) {
	if strings.Count(strings.TrimSpace(token), "::") != 1 {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimSpace(token), "::", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	project := strings.TrimSpace(parts[0])
	fileName := strings.TrimSpace(parts[1])
	if project == "" || fileName == "" {
		return "", "", false
	}
	return project, fileName, true
}

func (m *memoryStore) collectDocsFromHistoryIndex(ctx context.Context, projectFilter string, options ...bool) ([]memoryStoreDoc, bool) {
	includeCold, includeEphemeral := memoryStoreIncludeOptions(options)
	if m == nil || !m.policy.rollupUseHistoryIndex {
		return nil, false
	}
	normalizedProject := strings.TrimSpace(projectFilter)
	m.mu.RLock()
	latestTopic := make(map[string]string, len(m.latestTopic))
	for key, value := range m.latestTopic {
		latestTopic[key] = value
	}
	currentState := make(map[string]memoryCurrentState, len(m.currentState))
	for key, state := range m.currentState {
		currentState[key] = state
	}
	exactStatePaths := make(map[string]struct{}, len(m.exactStatePaths))
	for key := range m.exactStatePaths {
		exactStatePaths[key] = struct{}{}
	}
	m.mu.RUnlock()
	if len(latestTopic) == 0 {
		return nil, false
	}
	type recentMeta struct {
		summary     string
		updated     time.Time
		objectID    string
		horizon     int
		confidence  float64
		lastAccess  string
		contentRef  string
		lifecycle   string
		storageTier string
	}
	metadataByKey := map[string]recentMeta{}
	for key, state := range currentState {
		if state.Tombstone {
			continue
		}
		entry := state.Entry
		if key == "::" {
			continue
		}
		updated := time.Time{}
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.CreatedAt)); err == nil {
			updated = parsed.UTC()
		}
		metadataByKey[key] = recentMeta{
			summary:     strings.TrimSpace(entry.Summary),
			updated:     updated,
			objectID:    strings.TrimSpace(entry.ObjectID),
			horizon:     entry.HorizonDays,
			confidence:  entry.Confidence,
			lastAccess:  strings.TrimSpace(entry.LastAccess),
			contentRef:  strings.TrimSpace(entry.ContentRef),
			lifecycle:   normalizeMemoryLifecycle(entry.Lifecycle),
			storageTier: normalizeMemoryStorageTier(entry.StorageTier),
		}
	}
	docs := make([]memoryStoreDoc, 0, len(latestTopic))
	writePolicy := loadWriteIngressPolicy()
	for key, topicPath := range latestTopic {
		select {
		case <-ctx.Done():
			return docs, true
		default:
		}
		project, fileName, ok := parseMemoryStoreKeyToken(key)
		if !ok {
			continue
		}
		if normalizedProject != "" && !strings.EqualFold(project, normalizedProject) {
			continue
		}
		if exactStatePathSetContains(exactStatePaths, project, fileName) || writePolicy.isDurableMemoryFile(normalizedWrite{project: project, fileName: fileName}) {
			continue
		}
		topic := strings.TrimSpace(topicPath)
		if topic == "" {
			topic = deriveTopicFromFile(fileName)
		}
		meta := metadataByKey[key]
		lifecycle := normalizeMemoryLifecycle(meta.lifecycle)
		if isEphemeralMemoryIdentity(fileName, topic, meta.summary, lifecycle) {
			lifecycle = "test"
		}
		if !shouldSurfaceMemoryLifecycle(lifecycle, includeEphemeral) {
			continue
		}
		storageTier := normalizeMemoryStorageTier(meta.storageTier)
		if !includeCold && (storageTier == "deep" || storageTier == "retired") {
			continue
		}
		effectiveHorizon := m.effectiveHotHorizonDays(key)
		if meta.horizon != 0 {
			effectiveHorizon = meta.horizon
			if effectiveHorizon < 0 {
				effectiveHorizon = 0
			}
		}
		lastTouch := meta.updated
		if accessedAt, ok := parseTimeBestEffort(meta.lastAccess); ok {
			if lastTouch.IsZero() || accessedAt.After(lastTouch) {
				lastTouch = accessedAt
			}
		}
		lastTouch = m.docLastTouch(key, lastTouch)
		if !includeCold && effectiveHorizon > 0 && !lastTouch.IsZero() {
			cutoff := time.Now().UTC().Add(-time.Duration(effectiveHorizon) * 24 * time.Hour)
			if lastTouch.Before(cutoff) {
				continue
			}
		}
		objectID := strings.TrimSpace(meta.objectID)
		if objectID == "" {
			objectID = m.objectIDFor(project, fileName, topic, strings.TrimPrefix(strings.ToLower(meta.contentRef), "sha256:"))
		}
		score := meta.confidence
		if score <= 0 {
			score = m.confidenceForKey(key)
		}
		docs = append(docs, memoryStoreDoc{
			Project:     project,
			FileName:    fileName,
			TopicPath:   topic,
			Summary:     clipSummary(meta.summary, m.policy.maxSummaryChars),
			UpdatedAt:   meta.updated,
			ObjectID:    objectID,
			Horizon:     effectiveHorizon,
			Score:       score,
			LastTouch:   lastTouch,
			Lifecycle:   lifecycle,
			StorageTier: storageTier,
		})
	}
	return docs, true
}

func topicPrefixes(topic string) []string {
	normalized := strings.Trim(strings.TrimSpace(topic), "/")
	if normalized == "" {
		return []string{"root"}
	}
	parts := strings.Split(normalized, "/")
	prefixes := make([]string, 0, len(parts))
	for idx := range parts {
		prefixes = append(prefixes, strings.Join(parts[:idx+1], "/"))
	}
	return prefixes
}

func topicDepth(topic string) int {
	normalized := strings.Trim(strings.TrimSpace(topic), "/")
	if normalized == "" || normalized == "root" {
		return 1
	}
	return strings.Count(normalized, "/") + 1
}

func (m *memoryStore) topicRollups(project string, minCount int, limit int, offset int) map[string]any {
	return m.topicRollupsWithContext(context.Background(), project, minCount, limit, offset, false)
}

func (m *memoryStore) topicRollupsWithContext(
	ctx context.Context,
	project string,
	minCount int,
	limit int,
	offset int,
	options ...bool,
) map[string]any {
	includeCold, _ := memoryStoreIncludeOptions(options)
	return m.topicRollupsWithOptions(ctx, project, minCount, limit, offset, includeCold, false)
}

func (m *memoryStore) topicRollupSignals(project string, includeCold bool, includeEphemeral bool) map[string]*topicRollupSignal {
	if m == nil || !m.isEnabled() {
		return map[string]*topicRollupSignal{}
	}
	windowHours := envInt("GO_MEMORY_REVIEW_RECENT_WINDOW_HOURS", 72)
	if windowHours < 1 {
		windowHours = 72
	}
	if windowHours > 2160 {
		windowHours = 2160
	}
	cutoff := time.Now().UTC().Add(-time.Duration(windowHours) * time.Hour)
	projectFilter := strings.TrimSpace(project)

	m.mu.RLock()
	entries := append([]memoryStoreEntry(nil), m.recent...)
	m.mu.RUnlock()

	signals := map[string]*topicRollupSignal{}
	ensure := func(path string) *topicRollupSignal {
		signal := signals[path]
		if signal == nil {
			signal = &topicRollupSignal{
				uniqueAgents:    map[string]struct{}{},
				uniqueSessions:  map[string]struct{}{},
				lifecycleCounts: map[string]int{},
				diffStateCounts: map[string]int{},
			}
			signals[path] = signal
		}
		return signal
	}

	for _, entry := range entries {
		if isMemoryTombstone(entry) {
			continue
		}
		if projectFilter != "" && !strings.EqualFold(strings.TrimSpace(entry.Project), projectFilter) {
			continue
		}
		topicPath := strings.Trim(strings.TrimSpace(entry.TopicPath), "/")
		if topicPath == "" {
			topicPath = deriveTopicFromFile(entry.FileName)
		}
		lifecycle := normalizeMemoryLifecycle(entry.Lifecycle)
		if !shouldSurfaceMemoryLifecycle(lifecycle, includeEphemeral) {
			continue
		}
		createdAt := time.Time{}
		if parsed, ok := parseTimeBestEffort(entry.CreatedAt); ok {
			createdAt = parsed.UTC()
		}
		key := memoryStoreKey(entry.Project, entry.FileName)
		effectiveHorizon := m.effectiveHotHorizonDays(key)
		if entry.HorizonDays != 0 {
			effectiveHorizon = entry.HorizonDays
			if effectiveHorizon < 0 {
				effectiveHorizon = 0
			}
		}
		lastTouch := createdAt
		if accessedAt, ok := parseTimeBestEffort(entry.LastAccess); ok && (lastTouch.IsZero() || accessedAt.After(lastTouch)) {
			lastTouch = accessedAt.UTC()
		}
		lastTouch = m.docLastTouch(key, lastTouch)
		if !includeCold && effectiveHorizon > 0 && !lastTouch.IsZero() {
			hotCutoff := time.Now().UTC().Add(-time.Duration(effectiveHorizon) * 24 * time.Hour)
			if lastTouch.Before(hotCutoff) {
				continue
			}
		}
		recent := createdAt.IsZero() || createdAt.After(cutoff) || createdAt.Equal(cutoff)
		for _, prefix := range topicPrefixes(topicPath) {
			signal := ensure(prefix)
			signal.writeCount += 1
			if recent {
				signal.recentCount += 1
			}
			if agentID := strings.TrimSpace(entry.AgentID); agentID != "" {
				signal.uniqueAgents[agentID] = struct{}{}
			} else {
				signal.unattributedWrites += 1
			}
			if sessionID := strings.TrimSpace(entry.SessionID); sessionID != "" {
				signal.uniqueSessions[sessionID] = struct{}{}
			}
			signal.lifecycleCounts[lifecycle] += 1
			diffState := strings.TrimSpace(entry.DiffState)
			if diffState == "" {
				diffState = "unknown"
			}
			signal.diffStateCounts[diffState] += 1
			signal.rawBytes += entry.RawBytes
			if !createdAt.IsZero() && (signal.latestAt.IsZero() || createdAt.After(signal.latestAt)) {
				signal.latestAt = createdAt
			}
		}
	}
	return signals
}

func topicRollupIntensityScore(eventCount int, recentCount int, uniqueAgentCount int, uniqueSessionCount int, rewriteCount int) int {
	score := recentCount*8 + uniqueAgentCount*12 + uniqueSessionCount*6 + rewriteCount*5 + int(math.Sqrt(float64(maxInt(eventCount, 0)))*6)
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func cloneIntCounts(counts map[string]int) map[string]int {
	out := map[string]int{}
	for key, value := range counts {
		out[key] = value
	}
	return out
}

func intCountsToAny(counts map[string]int) map[string]any {
	out := map[string]any{}
	for key, value := range counts {
		out[key] = value
	}
	return out
}

func (m *memoryStore) topicRollupsWithOptions(
	ctx context.Context,
	project string,
	minCount int,
	limit int,
	offset int,
	includeCold bool,
	includeEphemeral bool,
) map[string]any {
	if minCount < 1 {
		minCount = 1
	}
	if limit < 1 {
		limit = 200
	}
	if limit > 2000 {
		limit = 2000
	}
	if offset < 0 {
		offset = 0
	}
	if cached, ok := m.getTopicRollupCache(project, minCount, includeCold, includeEphemeral); ok {
		topics := cached.topics
		total := cached.total
		if offset >= total {
			topics = []map[string]any{}
		} else {
			topics = topics[offset:]
		}
		if len(topics) > limit {
			topics = topics[:limit]
		}
		out := make([]any, 0, len(topics))
		for _, row := range topics {
			out = append(out, row)
		}
		return map[string]any{
			"project":                project,
			"topics":                 out,
			"total":                  total,
			"offset":                 offset,
			"limit":                  limit,
			"min_count":              minCount,
			"historyEntriesScanned":  len(m.recent),
			"historyEntriesDeduped":  len(m.recent),
			"generatedAt":            nowUTCISO(),
			"cache":                  "hit",
			"include_cold":           includeCold,
			"include_ephemeral":      includeEphemeral,
			"hot_index_horizon_days": m.policy.hotIndexMaxAgeDays,
			"user_horizon_enabled":   m.policy.userHorizonEnabled,
		}
	}
	rows, err := m.collectDocs(ctx, project, includeCold, includeEphemeral)
	if err != nil {
		return map[string]any{
			"project": project,
			"topics":  []any{},
			"total":   0,
			"error":   err.Error(),
		}
	}

	aggs := map[string]*topicRollupAggregate{}
	for _, doc := range rows {
		prefixes := topicPrefixes(doc.TopicPath)
		for _, prefix := range prefixes {
			agg := aggs[prefix]
			if agg == nil {
				agg = &topicRollupAggregate{
					path:            prefix,
					project:         doc.Project,
					depth:           topicDepth(prefix),
					uniqueFiles:     map[string]struct{}{},
					uniqueAgents:    map[string]struct{}{},
					uniqueSessions:  map[string]struct{}{},
					lifecycleCounts: map[string]int{},
					diffStateCounts: map[string]int{},
					children:        map[string]struct{}{},
					summarySnips:    []string{},
					filePartitions:  []map[string]any{},
				}
				aggs[prefix] = agg
			}
			agg.eventCount += 1
			if doc.Score > 0 {
				agg.confidenceSum += doc.Score
				agg.confidenceCount += 1
			}
			if doc.Horizon > agg.maxHorizonDays {
				agg.maxHorizonDays = doc.Horizon
			}
			agg.uniqueFiles[doc.FileName] = struct{}{}
			lifecycle := normalizeMemoryLifecycle(doc.Lifecycle)
			agg.lifecycleCounts[lifecycle] += 1
			if !doc.UpdatedAt.IsZero() && (agg.latestAt.IsZero() || doc.UpdatedAt.After(agg.latestAt)) {
				agg.latestAt = doc.UpdatedAt
			}
			if doc.Summary != "" && len(agg.summarySnips) < m.policy.maxRollupSnippets {
				dup := false
				for _, existing := range agg.summarySnips {
					if strings.EqualFold(existing, doc.Summary) {
						dup = true
						break
					}
				}
				if !dup {
					agg.summarySnips = append(agg.summarySnips, doc.Summary)
				}
			}
			if len(agg.filePartitions) < 5 {
				agg.filePartitions = append(agg.filePartitions, map[string]any{
					"file":       doc.FileName,
					"topic_path": doc.TopicPath,
					"lifecycle":  normalizeMemoryLifecycle(doc.Lifecycle),
				})
			}
		}
		prefixesCount := len(prefixes)
		for idx := 0; idx < prefixesCount-1; idx++ {
			parent := prefixes[idx]
			child := prefixes[idx+1]
			if agg, ok := aggs[parent]; ok {
				agg.children[child] = struct{}{}
			}
		}
	}

	signals := m.topicRollupSignals(project, includeCold, includeEphemeral)
	for path, signal := range signals {
		if signal == nil {
			continue
		}
		agg := aggs[path]
		if agg == nil {
			projectName := strings.TrimSpace(project)
			if projectName == "" {
				projectName = "workspace"
			}
			agg = &topicRollupAggregate{
				path:            path,
				project:         projectName,
				depth:           topicDepth(path),
				uniqueFiles:     map[string]struct{}{},
				uniqueAgents:    map[string]struct{}{},
				uniqueSessions:  map[string]struct{}{},
				lifecycleCounts: map[string]int{},
				diffStateCounts: map[string]int{},
				children:        map[string]struct{}{},
				summarySnips:    []string{},
				filePartitions:  []map[string]any{},
			}
			aggs[path] = agg
		}
		if agg.eventCount == 0 {
			agg.eventCount = signal.writeCount
		}
		agg.recentCount = maxInt(agg.recentCount, signal.recentCount)
		agg.rawBytes += signal.rawBytes
		for agent := range signal.uniqueAgents {
			agg.uniqueAgents[agent] = struct{}{}
		}
		for session := range signal.uniqueSessions {
			agg.uniqueSessions[session] = struct{}{}
		}
		agg.lifecycleCounts = cloneIntCounts(signal.lifecycleCounts)
		agg.diffStateCounts = cloneIntCounts(signal.diffStateCounts)
		if !signal.latestAt.IsZero() && (agg.latestAt.IsZero() || signal.latestAt.After(agg.latestAt)) {
			agg.latestAt = signal.latestAt
		}
	}

	topics := make([]map[string]any, 0, len(aggs))
	for _, agg := range aggs {
		if agg.eventCount < minCount {
			continue
		}
		uniqueFiles := make([]string, 0, len(agg.uniqueFiles))
		for fileName := range agg.uniqueFiles {
			uniqueFiles = append(uniqueFiles, fileName)
		}
		sort.Strings(uniqueFiles)
		children := make([]string, 0, len(agg.children))
		for child := range agg.children {
			children = append(children, child)
		}
		sort.Strings(children)
		uniqueAgents := make([]string, 0, len(agg.uniqueAgents))
		for agent := range agg.uniqueAgents {
			uniqueAgents = append(uniqueAgents, agent)
		}
		sort.Strings(uniqueAgents)
		uniqueSessions := make([]string, 0, len(agg.uniqueSessions))
		for session := range agg.uniqueSessions {
			uniqueSessions = append(uniqueSessions, session)
		}
		sort.Strings(uniqueSessions)
		latest := any(nil)
		if !agg.latestAt.IsZero() {
			latest = agg.latestAt.UTC().Format(time.RFC3339Nano)
		}
		confidenceMean := 0.0
		if agg.confidenceCount > 0 {
			confidenceMean = agg.confidenceSum / float64(agg.confidenceCount)
		}
		writeCount := agg.eventCount
		if signal := signals[agg.path]; signal != nil && signal.writeCount > writeCount {
			writeCount = signal.writeCount
		}
		rewriteCount := agg.diffStateCounts["rewrite"]
		topics = append(topics, map[string]any{
			"project":             agg.project,
			"path":                agg.path,
			"depth":               agg.depth,
			"eventCount":          agg.eventCount,
			"recentEventCount":    agg.recentCount,
			"writeCount":          writeCount,
			"uniqueFileCount":     len(uniqueFiles),
			"uniqueFiles":         uniqueFiles,
			"uniqueAgentCount":    len(uniqueAgents),
			"uniqueAgents":        uniqueAgents,
			"uniqueSessionCount":  len(uniqueSessions),
			"uniqueSessions":      uniqueSessions,
			"lifecycleCounts":     intCountsToAny(agg.lifecycleCounts),
			"diffStateCounts":     intCountsToAny(agg.diffStateCounts),
			"rawBytes":            agg.rawBytes,
			"agentIntensityScore": topicRollupIntensityScore(agg.eventCount, agg.recentCount, len(uniqueAgents), len(uniqueSessions), rewriteCount),
			"latestTimestamp":     latest,
			"summarySnippets":     agg.summarySnips,
			"confidenceMean":      confidenceMean,
			"maxHorizonDays":      agg.maxHorizonDays,
			"numericFacts":        []any{},
			"inference": []string{
				"Go memory-store rollup generated from project files and write history.",
			},
			"children":       children,
			"filePartitions": agg.filePartitions,
		})
	}

	sort.SliceStable(topics, func(i, j int) bool {
		leftCount := anyToInt(topics[i]["eventCount"], 0)
		rightCount := anyToInt(topics[j]["eventCount"], 0)
		if leftCount == rightCount {
			return strings.TrimSpace(anyToString(topics[i]["path"])) < strings.TrimSpace(anyToString(topics[j]["path"]))
		}
		return leftCount > rightCount
	})

	total := len(topics)
	m.putTopicRollupCache(project, minCount, includeCold, includeEphemeral, topics, total)
	if offset >= total {
		topics = []map[string]any{}
	} else {
		topics = topics[offset:]
	}
	if len(topics) > limit {
		topics = topics[:limit]
	}

	out := make([]any, 0, len(topics))
	for _, row := range topics {
		out = append(out, row)
	}
	return map[string]any{
		"project":                project,
		"topics":                 out,
		"total":                  total,
		"offset":                 offset,
		"limit":                  limit,
		"min_count":              minCount,
		"historyEntriesScanned":  len(m.recent),
		"historyEntriesDeduped":  len(m.recent),
		"generatedAt":            nowUTCISO(),
		"cache":                  "miss",
		"include_cold":           includeCold,
		"include_ephemeral":      includeEphemeral,
		"hot_index_horizon_days": m.policy.hotIndexMaxAgeDays,
		"user_horizon_enabled":   m.policy.userHorizonEnabled,
	}
}

func buildTopicTree(counts map[string]int) map[string]any {
	type node struct {
		count    int
		children map[string]*node
	}
	root := &node{children: map[string]*node{}}
	ensure := func(path string) *node {
		parts := strings.Split(path, "/")
		curr := root
		for _, part := range parts {
			if strings.TrimSpace(part) == "" {
				continue
			}
			next := curr.children[part]
			if next == nil {
				next = &node{children: map[string]*node{}}
				curr.children[part] = next
			}
			curr = next
		}
		return curr
	}
	for path, count := range counts {
		n := ensure(path)
		n.count = count
	}
	var render func(n *node) map[string]any
	render = func(n *node) map[string]any {
		children := map[string]any{}
		keys := make([]string, 0, len(n.children))
		for key := range n.children {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			children[key] = render(n.children[key])
		}
		return map[string]any{
			"count":    n.count,
			"children": children,
		}
	}
	return render(root)
}

func (m *memoryStore) topicList(project string, minCount int, limit int, depth int) map[string]any {
	if minCount < 1 {
		minCount = 1
	}
	if limit < 1 {
		limit = 200
	}
	if limit > 5000 {
		limit = 5000
	}
	if depth < 1 {
		depth = 8
	}
	rollups := m.topicRollups(project, minCount, 500000, 0)
	rawTopics, _ := rollups["topics"].([]any)
	rows := make([]map[string]any, 0, len(rawTopics))
	for _, raw := range rawTopics {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := strings.TrimSpace(anyToString(row["path"]))
		if path == "" {
			continue
		}
		if topicDepth(path) > depth {
			continue
		}
		rows = append(rows, map[string]any{
			"project": strings.TrimSpace(anyToString(row["project"])),
			"path":    path,
			"count":   anyToInt(row["eventCount"], 0),
		})
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, row)
	}
	return map[string]any{
		"topics":    out,
		"total":     len(rows),
		"limit":     limit,
		"min_count": minCount,
		"depth":     depth,
		"project":   project,
	}
}

func (m *memoryStore) topicTree(project string) map[string]any {
	rollups := m.topicRollups(project, 1, 500000, 0)
	rawTopics, _ := rollups["topics"].([]any)
	counts := map[string]int{}
	for _, raw := range rawTopics {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		path := strings.TrimSpace(anyToString(row["path"]))
		if path == "" {
			continue
		}
		counts[path] = anyToInt(row["eventCount"], 0)
	}
	return map[string]any{
		"project": project,
		"topics":  buildTopicTree(counts),
	}
}
