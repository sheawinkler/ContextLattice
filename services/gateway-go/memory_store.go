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
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type memoryStorePolicy struct {
	enabled                    bool
	rootPath                   string
	historyPath                string
	contentAddressed           bool
	contentBlobsPath           string
	contentLinkMode            string
	rollupUseHistoryIndex      bool
	historyStartupMaxLines     int
	historyStartupTailMaxBytes int64
	maxRecent                  int
	scanLimit                  int
	maxSummaryChars            int
	maxRollupSnippets          int
	maxRollupReadBytes         int64
	maxRollupFileBytes         int64
	rollupCacheTTL             time.Duration
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
	CreatedAt   string   `json:"created_at"`
	RawBytes    int      `json:"raw_bytes,omitempty"`
	Source      string   `json:"source,omitempty"`
}

type memoryStoreDoc struct {
	Project   string
	FileName  string
	TopicPath string
	Summary   string
	UpdatedAt time.Time
}

type topicRollupAggregate struct {
	path           string
	project        string
	depth          int
	eventCount     int
	recentCount    int
	uniqueFiles    map[string]struct{}
	latestAt       time.Time
	summarySnips   []string
	children       map[string]struct{}
	filePartitions []map[string]any
}

type memoryStore struct {
	policy      memoryStorePolicy
	mu          sync.RWMutex
	recent      []memoryStoreEntry
	latestTopic map[string]string
	latestHash  map[string]string
	rollupCache map[string]topicRollupCacheEntry
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
	historyStartupMaxLines := envInt("GO_MEMORY_STORE_HISTORY_STARTUP_MAX_LINES", 20000)
	if historyStartupMaxLines < 0 {
		historyStartupMaxLines = 0
	}
	historyStartupTailMaxBytes := int64(clampInt(
		envInt("GO_MEMORY_STORE_HISTORY_STARTUP_TAIL_MAX_BYTES", 64*1024*1024),
		1024*1024,
		1024*1024*1024,
	))
	return memoryStorePolicy{
		enabled:                    envBool("GO_MEMORY_STORE_ENABLED", true),
		rootPath:                   root,
		historyPath:                filepath.Clean(historyPath),
		contentAddressed:           envBool("GO_MEMORY_STORE_CONTENT_ADDRESSING_ENABLED", true),
		contentBlobsPath:           filepath.Clean(contentBlobsPath),
		contentLinkMode:            contentLinkMode,
		rollupUseHistoryIndex:      envBool("GO_MEMORY_STORE_ROLLUP_USE_HISTORY_INDEX", true),
		historyStartupMaxLines:     historyStartupMaxLines,
		historyStartupTailMaxBytes: historyStartupTailMaxBytes,
		maxRecent:                  clampInt(envInt("GO_MEMORY_STORE_MAX_RECENT", 6000), 64, 100000),
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
		rollupCacheTTL: envDurationSeconds("GO_MEMORY_STORE_ROLLUP_CACHE_TTL_SECS", 15),
	}
}

func newMemoryStoreFromEnv() (*memoryStore, error) {
	policy := loadMemoryStorePolicy()
	store := &memoryStore{
		policy:      policy,
		recent:      make([]memoryStoreEntry, 0, policy.maxRecent),
		latestTopic: map[string]string{},
		latestHash:  map[string]string{},
		rollupCache: map[string]topicRollupCacheEntry{},
	}
	if !policy.enabled {
		return store, nil
	}
	if err := os.MkdirAll(policy.rootPath, 0o755); err != nil {
		return nil, fmt.Errorf("create memory store root: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(policy.historyPath), 0o755); err != nil {
		return nil, fmt.Errorf("create memory store history directory: %w", err)
	}
	if policy.contentAddressed {
		if err := os.MkdirAll(policy.contentBlobsPath, 0o755); err != nil {
			return nil, fmt.Errorf("create memory store content blobs directory: %w", err)
		}
	}
	if err := store.loadHistory(); err != nil {
		return nil, err
	}
	return store, nil
}

func (m *memoryStore) loadHistory() error {
	if m == nil || !m.policy.enabled {
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
			m.recordEntry(entry)
			loaded += 1
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
		m.recordEntry(entry)
		loaded += 1
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan memory store history: %w", err)
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
	return strings.Join(clean, "/"), nil
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
	if strings.TrimSpace(entry.EventID) == "" {
		entry.EventID = primitive.NewObjectID().Hex()
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
	key := memoryStoreKey(entry.Project, entry.FileName)
	if key != "::" {
		m.latestTopic[key] = entry.TopicPath
		if strings.TrimSpace(entry.ContentHash) != "" {
			m.latestHash[key] = entry.ContentHash
		}
	}
	m.invalidateTopicRollupCacheLocked(entry.Project)
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

func topicRollupCacheKey(project string, minCount int) string {
	if minCount < 1 {
		minCount = 1
	}
	return normalizeRollupProject(project) + "|" + strconv.Itoa(minCount)
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

func (m *memoryStore) getTopicRollupCache(project string, minCount int) (topicRollupCacheEntry, bool) {
	if m == nil || m.policy.rollupCacheTTL <= 0 {
		return topicRollupCacheEntry{}, false
	}
	key := topicRollupCacheKey(project, minCount)
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

func (m *memoryStore) putTopicRollupCache(project string, minCount int, rows []map[string]any, total int) {
	if m == nil || m.policy.rollupCacheTTL <= 0 {
		return
	}
	key := topicRollupCacheKey(project, minCount)
	m.mu.Lock()
	m.rollupCache[key] = topicRollupCacheEntry{
		generatedAt: time.Now(),
		total:       total,
		topics:      cloneTopicRows(rows),
	}
	m.mu.Unlock()
}

func (m *memoryStore) appendHistory(entry memoryStoreEntry) error {
	if m == nil || !m.policy.enabled {
		return nil
	}
	payload, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode memory history entry: %w", err)
	}
	line := append(payload, '\n')
	file, err := os.OpenFile(m.policy.historyPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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
	tmpPath := path + ".tmp-" + primitive.NewObjectID().Hex()
	if err := os.WriteFile(tmpPath, content, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func copyFileAtomic(src string, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmpPath := dst + ".tmp-" + primitive.NewObjectID().Hex()
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
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func (m *memoryStore) ensureBlob(contentHash string, content string) (string, error) {
	blobPath, err := m.blobPathForHash(contentHash)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(blobPath); err == nil {
		return blobPath, nil
	}
	if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
		return "", fmt.Errorf("create blob directory: %w", err)
	}
	if err := writeAtomicFile(blobPath, []byte(content), 0o644); err != nil {
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
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return fmt.Errorf("create memory file directory: %w", err)
	}
	tmpPath := filePath + ".tmp-" + primitive.NewObjectID().Hex()

	mode := m.policy.contentLinkMode
	if mode == "" {
		mode = "hardlink"
	}
	linkErr := error(nil)
	switch mode {
	case "symlink":
		linkErr = os.Symlink(blobPath, tmpPath)
	case "copy":
		linkErr = copyFileAtomic(blobPath, filePath, 0o644)
	default:
		linkErr = os.Link(blobPath, tmpPath)
	}

	usedTmpPath := mode != "copy"
	if linkErr != nil && mode != "copy" {
		// Hardlink/symlink can fail on some filesystems or policies; fall back to copy.
		usedTmpPath = false
		linkErr = copyFileAtomic(blobPath, filePath, 0o644)
	}
	if linkErr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("materialize memory content from blob: %w", linkErr)
	}
	if usedTmpPath {
		if err := os.Rename(tmpPath, filePath); err != nil {
			_ = os.Remove(tmpPath)
			return fmt.Errorf("commit linked memory file: %w", err)
		}
	}
	return nil
}

func (m *memoryStore) put(item normalizedWrite) (memoryStoreEntry, bool, error) {
	if m == nil || !m.policy.enabled {
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
	content := item.content
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	topicPath := sanitizeTopicPath(item.topicPath, fileName)
	contentHash := sha256Hex(content)
	key := memoryStoreKey(project, fileName)
	filePath := filepath.Join(m.policy.rootPath, project, filepath.FromSlash(fileName))

	m.mu.RLock()
	previousHash := m.latestHash[key]
	previousTopic := m.latestTopic[key]
	m.mu.RUnlock()
	deduped := previousHash != "" && previousHash == contentHash

	buildEntry := func() memoryStoreEntry {
		return memoryStoreEntry{
			EventID:     primitive.NewObjectID().Hex(),
			Project:     project,
			FileName:    fileName,
			TopicPath:   topicPath,
			AgentID:     item.agentID,
			SessionID:   item.sessionID,
			Tags:        append([]string{}, item.tags...),
			Summary:     clipSummary(content, m.policy.maxSummaryChars),
			ContentHash: contentHash,
			ContentRef:  "sha256:" + contentHash,
			CreatedAt:   nowUTCISO(),
			RawBytes:    len(content),
			Source:      "go_memory_store",
		}
	}

	if deduped && strings.EqualFold(strings.TrimSpace(previousTopic), strings.TrimSpace(topicPath)) {
		// No payload or topic change: skip redundant rewrite/history append.
		if _, err := os.Stat(filePath); err == nil {
			return buildEntry(), true, nil
		}
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
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			return memoryStoreEntry{}, false, fmt.Errorf("create memory file directory: %w", err)
		}
		if err := writeAtomicFile(filePath, []byte(content), 0o644); err != nil {
			return memoryStoreEntry{}, false, fmt.Errorf("commit memory file: %w", err)
		}
	}

	entry := buildEntry()

	m.mu.Lock()
	m.recordEntry(entry)
	m.mu.Unlock()
	if err := m.appendHistory(entry); err != nil {
		return memoryStoreEntry{}, false, err
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
	if m == nil || !m.policy.enabled {
		return "", nil, errors.New("go memory store is disabled")
	}
	cleanProject, err := sanitizeMemoryProject(project)
	if err != nil {
		return "", nil, err
	}
	cleanFile, err := sanitizeMemoryFile(fileName)
	if err != nil {
		return "", nil, err
	}
	abs := filepath.Join(m.policy.rootPath, cleanProject, filepath.FromSlash(cleanFile))
	bytes, err := os.ReadFile(abs)
	if err != nil {
		return "", nil, err
	}
	info, statErr := os.Stat(abs)
	if statErr != nil {
		return string(bytes), nil, nil
	}
	return string(bytes), info, nil
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
	result := map[string]any{
		"memory_id": memoryID,
		"memory": map[string]any{
			"project":    project,
			"file":       fileName,
			"topic_path": deriveTopicFromFile(fileName),
			"content":    content,
		},
		"source": "go_memory_store",
	}
	if info != nil {
		result["memory"].(map[string]any)["updated_at"] = info.ModTime().UTC().Format(time.RFC3339Nano)
		result["memory"].(map[string]any)["raw_bytes"] = info.Size()
	}
	return result, nil
}

func (m *memoryStore) recentItems(project string, topicPath string, limit int, offset int) []map[string]any {
	if m == nil || !m.policy.enabled {
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
			"created_at":   entry.CreatedAt,
			"raw_bytes":    entry.RawBytes,
			"source":       entry.Source,
		})
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

func (m *memoryStore) collectDocs(ctx context.Context, projectFilter string) ([]memoryStoreDoc, error) {
	if m == nil || !m.policy.enabled {
		return []memoryStoreDoc{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if docs, ok := m.collectDocsFromHistoryIndex(ctx, projectFilter); ok {
		return docs, nil
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
	m.mu.RUnlock()

	docs := make([]memoryStoreDoc, 0, 1024)
	scanned := 0
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
		topic := latestTopic[memoryStoreKey(project, fileName)]
		if strings.TrimSpace(topic) == "" {
			topic = deriveTopicFromFile(fileName)
		}
		updatedAt := time.Time{}
		if info != nil {
			updatedAt = info.ModTime().UTC()
		}
		docs = append(docs, memoryStoreDoc{
			Project:   project,
			FileName:  fileName,
			TopicPath: topic,
			Summary:   clipSummary(string(bytes), m.policy.maxSummaryChars),
			UpdatedAt: updatedAt,
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

func (m *memoryStore) collectDocsFromHistoryIndex(ctx context.Context, projectFilter string) ([]memoryStoreDoc, bool) {
	if m == nil || !m.policy.rollupUseHistoryIndex {
		return nil, false
	}
	normalizedProject := strings.TrimSpace(projectFilter)
	m.mu.RLock()
	latestTopic := make(map[string]string, len(m.latestTopic))
	for key, value := range m.latestTopic {
		latestTopic[key] = value
	}
	recent := append([]memoryStoreEntry(nil), m.recent...)
	m.mu.RUnlock()
	if len(latestTopic) == 0 {
		return nil, false
	}
	type recentMeta struct {
		summary string
		updated time.Time
	}
	metadataByKey := map[string]recentMeta{}
	for i := len(recent) - 1; i >= 0; i-- {
		entry := recent[i]
		key := memoryStoreKey(entry.Project, entry.FileName)
		if key == "::" {
			continue
		}
		if _, exists := metadataByKey[key]; exists {
			continue
		}
		updated := time.Time{}
		if parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(entry.CreatedAt)); err == nil {
			updated = parsed.UTC()
		}
		metadataByKey[key] = recentMeta{
			summary: strings.TrimSpace(entry.Summary),
			updated: updated,
		}
	}
	docs := make([]memoryStoreDoc, 0, len(latestTopic))
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
		topic := strings.TrimSpace(topicPath)
		if topic == "" {
			topic = deriveTopicFromFile(fileName)
		}
		meta := metadataByKey[key]
		docs = append(docs, memoryStoreDoc{
			Project:   project,
			FileName:  fileName,
			TopicPath: topic,
			Summary:   clipSummary(meta.summary, m.policy.maxSummaryChars),
			UpdatedAt: meta.updated,
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
	return m.topicRollupsWithContext(context.Background(), project, minCount, limit, offset)
}

func (m *memoryStore) topicRollupsWithContext(ctx context.Context, project string, minCount int, limit int, offset int) map[string]any {
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
	if cached, ok := m.getTopicRollupCache(project, minCount); ok {
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
			"project":               project,
			"topics":                out,
			"total":                 total,
			"offset":                offset,
			"limit":                 limit,
			"min_count":             minCount,
			"historyEntriesScanned": len(m.recent),
			"historyEntriesDeduped": len(m.recent),
			"generatedAt":           nowUTCISO(),
			"cache":                 "hit",
		}
	}
	rows, err := m.collectDocs(ctx, project)
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
					path:           prefix,
					project:        doc.Project,
					depth:          topicDepth(prefix),
					uniqueFiles:    map[string]struct{}{},
					children:       map[string]struct{}{},
					summarySnips:   []string{},
					filePartitions: []map[string]any{},
				}
				aggs[prefix] = agg
			}
			agg.eventCount += 1
			agg.uniqueFiles[doc.FileName] = struct{}{}
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
		latest := any(nil)
		if !agg.latestAt.IsZero() {
			latest = agg.latestAt.UTC().Format(time.RFC3339Nano)
		}
		topics = append(topics, map[string]any{
			"project":          agg.project,
			"path":             agg.path,
			"depth":            agg.depth,
			"eventCount":       agg.eventCount,
			"recentEventCount": 0,
			"uniqueFileCount":  len(uniqueFiles),
			"uniqueFiles":      uniqueFiles,
			"latestTimestamp":  latest,
			"summarySnippets":  agg.summarySnips,
			"numericFacts":     []any{},
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
	m.putTopicRollupCache(project, minCount, topics, total)
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
		"project":               project,
		"topics":                out,
		"total":                 total,
		"offset":                offset,
		"limit":                 limit,
		"min_count":             minCount,
		"historyEntriesScanned": len(m.recent),
		"historyEntriesDeduped": len(m.recent),
		"generatedAt":           nowUTCISO(),
		"cache":                 "miss",
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
