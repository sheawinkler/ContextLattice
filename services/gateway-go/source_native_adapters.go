package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

var (
	nativeQdrantDimCacheMu sync.Mutex
	nativeQdrantDimCache   = map[string]int{}

	nativePgvectorDBMu      sync.Mutex
	nativePgvectorDBByDSN   = map[string]*sql.DB{}
	nativePgvectorSchemaMu  sync.Mutex
	nativePgvectorSchemaSet = map[string]struct{}{}
	nativePgvectorColsMu    sync.Mutex
	nativePgvectorColsByKey = map[string]map[string]struct{}{}

	memoryBankSpikeBackendChoices = map[string]struct{}{
		"native":            {},
		"disabled":          {},
		"meilisearch_spike": {},
		"quickwit_spike":    {},
		"tantivy_spike":     {},
		"lancedb_spike":     {},
		"trieve_spike":      {},
		"helixdb_spike":     {},
		"icm_spike":         {},
		"shodh_spike":       {},
		"memvid_spike":      {},
		"surrealdb_spike":   {},
	}
)

func nativeSourceAdapterEnabled(source string, fallback bool) bool {
	envName := "GO_RETRIEVAL_NATIVE_" + strings.ToUpper(strings.TrimSpace(source)) + "_ENABLED"
	envName = strings.ReplaceAll(envName, "-", "_")
	return envBool(envName, fallback)
}

func nativeQdrantURL() string {
	for _, key := range []string{"QDRANT_LOCAL_URL", "QDRANT_URL"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return strings.TrimRight(value, "/")
		}
	}
	return ""
}

func nativeQdrantCollection() string {
	token := strings.TrimSpace(os.Getenv("ORCH_QDRANT_COLLECTION"))
	if token == "" {
		token = "contextlattice_notes"
	}
	return token
}

func nativeFastembedBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("ORCH_FASTEMBED_RS_BASE_URL")), "/")
}

func nativeFastembedRoute() string {
	route := strings.TrimSpace(os.Getenv("ORCH_FASTEMBED_RS_ROUTE"))
	if route == "" {
		route = "/embed"
	}
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	return route
}

func nativeDefaultEmbedDim() int {
	return maxInt(8, envInt("ORCH_PGVECTOR_EMBED_DIM", 768))
}

func nativeAdjustVectorDim(vector []float64, dim int) []float64 {
	if dim <= 0 {
		return vector
	}
	if len(vector) == dim {
		return vector
	}
	if len(vector) > dim {
		return append([]float64(nil), vector[:dim]...)
	}
	out := make([]float64, dim)
	copy(out, vector)
	return out
}

func nativeCheapEmbedding(text string, dim int) []float64 {
	if dim <= 0 {
		dim = nativeDefaultEmbedDim()
	}
	if dim <= 0 {
		dim = 768
	}
	tokens := queryTerms(text, 64)
	if len(tokens) == 0 {
		normalized := strings.TrimSpace(strings.ToLower(text))
		if normalized == "" {
			normalized = "contextlattice"
		}
		tokens = []string{normalized}
	}
	vector := make([]float64, dim)
	for idx, token := range tokens {
		sum := sha256.Sum256([]byte(token))
		weight := 1.0 + float64(idx%4)*0.075
		for j := 0; j < 8; j++ {
			slotRaw := int(sum[j*2])<<8 | int(sum[j*2+1])
			slot := slotRaw % dim
			value := float64(int(sum[31-j])%97+3) / 100.0
			vector[slot] += value * weight
		}
	}
	norm := 0.0
	for _, value := range vector {
		norm += value * value
	}
	norm = math.Sqrt(norm)
	if norm <= 0 {
		vector[0] = 1.0
		return vector
	}
	for idx, value := range vector {
		vector[idx] = value / norm
	}
	return vector
}

func nativeEmbedQueryVector(
	ctx context.Context,
	client *http.Client,
	query string,
	targetDim int,
) ([]float64, []string, error) {
	warnings := []string{}
	if !nativeSourceAdapterEnabled("fastembed", true) {
		vector := nativeCheapEmbedding(query, targetDim)
		return vector, warnings, nil
	}
	baseURL := nativeFastembedBaseURL()
	if baseURL == "" {
		vector := nativeCheapEmbedding(query, targetDim)
		return vector, warnings, nil
	}

	payload := map[string]any{
		"input": []string{query},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		vector := nativeCheapEmbedding(query, targetDim)
		warnings = append(warnings, "fastembed adapter serialization failed; using cheap embedding fallback: "+err.Error())
		return vector, warnings, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+nativeFastembedRoute(), bytes.NewReader(body))
	if err != nil {
		vector := nativeCheapEmbedding(query, targetDim)
		warnings = append(warnings, "fastembed adapter request build failed; using cheap embedding fallback: "+err.Error())
		return vector, warnings, nil
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		vector := nativeCheapEmbedding(query, targetDim)
		warnings = append(warnings, "fastembed adapter unavailable; using cheap embedding fallback: "+err.Error())
		return vector, warnings, nil
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		vector := nativeCheapEmbedding(query, targetDim)
		message := "fastembed adapter status=" + strconv.Itoa(resp.StatusCode)
		if trimmed := strings.TrimSpace(string(bodyBytes)); trimmed != "" {
			message += " body=" + trimmed
		}
		warnings = append(warnings, message+"; using cheap embedding fallback")
		return vector, warnings, nil
	}
	responsePayload, err := parseJSONMap(bodyBytes)
	if err != nil {
		vector := nativeCheapEmbedding(query, targetDim)
		warnings = append(warnings, "fastembed adapter response parse failed; using cheap embedding fallback: "+err.Error())
		return vector, warnings, nil
	}
	rawVectors, _ := responsePayload["vectors"].([]any)
	if len(rawVectors) == 0 {
		vector := nativeCheapEmbedding(query, targetDim)
		warnings = append(warnings, "fastembed adapter returned empty vectors; using cheap embedding fallback")
		return vector, warnings, nil
	}
	firstVector, _ := rawVectors[0].([]any)
	if len(firstVector) == 0 {
		vector := nativeCheapEmbedding(query, targetDim)
		warnings = append(warnings, "fastembed adapter returned empty vector row; using cheap embedding fallback")
		return vector, warnings, nil
	}
	vector := make([]float64, 0, len(firstVector))
	for _, value := range firstVector {
		vector = append(vector, anyToFloat(value))
	}
	vector = nativeAdjustVectorDim(vector, targetDim)
	if len(vector) == 0 {
		vector = nativeCheapEmbedding(query, targetDim)
		warnings = append(warnings, "fastembed adapter vector coercion failed; using cheap embedding fallback")
	}
	return vector, warnings, nil
}

func nativeQdrantCollectionDimCacheKey(baseURL string, collection string) string {
	return strings.TrimSpace(strings.ToLower(baseURL)) + "|" + strings.TrimSpace(strings.ToLower(collection))
}

func nativeQdrantCachedDim(baseURL string, collection string) int {
	key := nativeQdrantCollectionDimCacheKey(baseURL, collection)
	nativeQdrantDimCacheMu.Lock()
	defer nativeQdrantDimCacheMu.Unlock()
	return nativeQdrantDimCache[key]
}

func nativeQdrantSetCachedDim(baseURL string, collection string, dim int) {
	if dim <= 0 {
		return
	}
	key := nativeQdrantCollectionDimCacheKey(baseURL, collection)
	nativeQdrantDimCacheMu.Lock()
	nativeQdrantDimCache[key] = dim
	nativeQdrantDimCacheMu.Unlock()
}

func nativeQdrantCollectionDim(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	collection string,
) (int, error) {
	if cached := nativeQdrantCachedDim(baseURL, collection); cached > 0 {
		return cached, nil
	}
	requestURL := baseURL + "/collections/" + url.PathEscape(collection)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		return 0, errors.New("qdrant collection status=" + strconv.Itoa(resp.StatusCode))
	}
	payload, err := parseJSONMap(body)
	if err != nil {
		return 0, err
	}
	result, _ := payload["result"].(map[string]any)
	config, _ := result["config"].(map[string]any)
	params, _ := config["params"].(map[string]any)
	vectors := params["vectors"]
	dim := 0
	switch typed := vectors.(type) {
	case map[string]any:
		if size := anyToInt(typed["size"], 0); size > 0 {
			dim = size
		} else {
			for _, value := range typed {
				named, ok := value.(map[string]any)
				if !ok {
					continue
				}
				if size := anyToInt(named["size"], 0); size > 0 {
					dim = size
					break
				}
			}
		}
	}
	if dim <= 0 {
		return 0, errors.New("qdrant collection dimension unavailable")
	}
	nativeQdrantSetCachedDim(baseURL, collection, dim)
	return dim, nil
}

func (s *server) queryQdrantSource(
	ctx context.Context,
	baseRequest map[string]any,
) ([]map[string]any, []string, error) {
	if !nativeSourceAdapterEnabled(sourceQdrant, true) {
		return nil, nil, errors.New("native qdrant adapter disabled")
	}
	baseURL := nativeQdrantURL()
	if baseURL == "" {
		return nil, nil, errors.New("qdrant URL not configured")
	}
	collection := nativeQdrantCollection()
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, errors.New("query is required")
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	scanLimit := maxInt(limit*4, limit)
	if capLimit := envInt("ORCH_QDRANT_FILTERLESS_LIMIT_CAP", 96); capLimit > 0 && scanLimit > capLimit {
		scanLimit = capLimit
	}
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))

	vectorDim, err := nativeQdrantCollectionDim(ctx, s.client, baseURL, collection)
	warnings := []string{}
	if err != nil || vectorDim <= 0 {
		vectorDim = nativeDefaultEmbedDim()
		if err != nil {
			warnings = append(warnings, "qdrant collection dimension probe failed; using default embed dim: "+err.Error())
		}
	}
	queryVector, embedWarnings, err := nativeEmbedQueryVector(ctx, s.client, query, vectorDim)
	if err != nil {
		return nil, append(warnings, embedWarnings...), err
	}
	warnings = append(warnings, embedWarnings...)

	searchPayload := map[string]any{
		"vector":       queryVector,
		"limit":        scanLimit,
		"with_payload": true,
	}
	mustFilters := []map[string]any{}
	if projectFilter != "" {
		mustFilters = append(mustFilters, map[string]any{
			"key": "project",
			"match": map[string]any{
				"value": projectFilter,
			},
		})
	}
	if topicFilter != "" {
		mustFilters = append(mustFilters, map[string]any{
			"key": "topic_tags",
			"match": map[string]any{
				"value": topicFilter,
			},
		})
	}
	if len(mustFilters) > 0 {
		searchPayload["filter"] = map[string]any{
			"must": mustFilters,
		}
	}

	bodyBytes, err := json.Marshal(searchPayload)
	if err != nil {
		return nil, warnings, err
	}
	requestURL := baseURL + "/collections/" + url.PathEscape(collection) + "/points/search"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, warnings, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, warnings, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, warnings, err
	}
	if resp.StatusCode >= 400 {
		return nil, warnings, errors.New("qdrant search status=" + strconv.Itoa(resp.StatusCode))
	}
	payload, err := parseJSONMap(responseBody)
	if err != nil {
		return nil, warnings, err
	}
	hits, _ := payload["result"].([]any)
	rows := make([]map[string]any, 0, len(hits))
	for _, raw := range hits {
		hit, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		entry, _ := hit["payload"].(map[string]any)
		project := strings.TrimSpace(anyToString(entry["project"]))
		fileName := strings.TrimSpace(anyToString(entry["file"]))
		summary := strings.TrimSpace(anyToString(entry["summary"]))
		if project == "" || fileName == "" || summary == "" {
			continue
		}
		if projectFilter != "" && project != projectFilter {
			continue
		}
		topicPath := strings.TrimSpace(anyToString(entry["topic_path"]))
		if topicFilter != "" {
			if topicPath == "" || (topicPath != topicFilter && !strings.HasPrefix(topicPath, topicFilter+"/")) {
				continue
			}
		}
		score := anyToFloat(hit["score"])
		if score <= 0 {
			score = textMatchScore(query, project+"\n"+fileName+"\n"+summary)
		}
		if score <= 0 {
			continue
		}
		rows = append(rows, map[string]any{
			"project":    project,
			"file":       fileName,
			"summary":    summary,
			"score":      score,
			"source":     sourceQdrant,
			"topic_path": topicPath,
			"created_at": entry["created_at"],
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, warnings, nil
}

func nativeWeaviateURL() string {
	token := strings.TrimSpace(os.Getenv("WEAVIATE_URL"))
	return strings.TrimRight(token, "/")
}

func nativeWeaviateClass() string {
	token := strings.TrimSpace(os.Getenv("ORCH_WEAVIATE_CLASS"))
	token = regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString(token, "")
	if token == "" {
		token = "ContextLatticeNote"
	}
	return token
}

func nativeWeaviateHeaders() map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
	}
	if key := strings.TrimSpace(os.Getenv("WEAVIATE_API_KEY")); key != "" {
		headers["Authorization"] = "Bearer " + key
	}
	return headers
}

func nativeGraphQLEscape(text string) string {
	replacer := strings.NewReplacer(
		`\\`, `\\\\`,
		`"`, `\"`,
		"\n", " ",
		"\r", " ",
	)
	return replacer.Replace(text)
}

func (s *server) queryWeaviateSource(
	ctx context.Context,
	baseRequest map[string]any,
) ([]map[string]any, []string, error) {
	if !nativeSourceAdapterEnabled(sourceWeaviate, true) {
		return nil, nil, errors.New("native weaviate adapter disabled")
	}
	if !envBool("ORCH_WEAVIATE_ENABLED", false) {
		return []map[string]any{}, nil, nil
	}
	baseURL := nativeWeaviateURL()
	if baseURL == "" {
		return nil, nil, errors.New("weaviate URL not configured")
	}
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, errors.New("query is required")
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))

	whereClauses := []string{}
	if projectFilter != "" {
		whereClauses = append(whereClauses, `{path:["project"],operator:Equal,valueText:"`+nativeGraphQLEscape(projectFilter)+`"}`)
	}
	if topicFilter != "" {
		whereClauses = append(whereClauses, `{path:["topic_path"],operator:Like,valueText:"`+nativeGraphQLEscape(topicFilter)+`*"}`)
	}
	whereBlock := ""
	if len(whereClauses) == 1 {
		whereBlock = " where:" + whereClauses[0]
	} else if len(whereClauses) > 1 {
		whereBlock = " where:{operator:And operands:[" + strings.Join(whereClauses, ",") + "]}"
	}
	className := nativeWeaviateClass()
	graphql := "{ Get { " + className + "(limit:" + strconv.Itoa(limit) +
		` bm25:{query:"` + nativeGraphQLEscape(query) + `"}` + whereBlock +
		`){ project file summary topic_path _additional { score } } } }`
	requestPayload := map[string]any{"query": graphql}
	body, err := json.Marshal(requestPayload)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	for key, value := range nativeWeaviateHeaders() {
		req.Header.Set(key, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, nil, errors.New("weaviate search status=" + strconv.Itoa(resp.StatusCode))
	}
	payload, err := parseJSONMap(responseBody)
	if err != nil {
		return nil, nil, err
	}
	data, _ := payload["data"].(map[string]any)
	getPayload, _ := data["Get"].(map[string]any)
	rawRows, _ := getPayload[className].([]any)
	rows := make([]map[string]any, 0, len(rawRows))
	for _, raw := range rawRows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		project := strings.TrimSpace(anyToString(row["project"]))
		fileName := strings.TrimSpace(anyToString(row["file"]))
		summary := strings.TrimSpace(anyToString(row["summary"]))
		if project == "" || fileName == "" || summary == "" {
			continue
		}
		topicPath := strings.TrimSpace(anyToString(row["topic_path"]))
		if topicFilter != "" {
			if topicPath == "" || (topicPath != topicFilter && !strings.HasPrefix(topicPath, topicFilter+"/")) {
				continue
			}
		}
		additional, _ := row["_additional"].(map[string]any)
		score := anyToFloat(additional["score"])
		if score <= 0 {
			score = textMatchScore(query, project+"\n"+fileName+"\n"+summary)
		}
		if score <= 0 {
			continue
		}
		rows = append(rows, map[string]any{
			"project":    project,
			"file":       fileName,
			"summary":    summary,
			"score":      score,
			"source":     sourceWeaviate,
			"topic_path": topicPath,
		})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil, nil
}

func nativePgvectorEnabled() bool {
	return envBool("ORCH_PGVECTOR_ENABLED", true)
}

func nativePgvectorFanoutEnabled() bool {
	return envBool("ORCH_PGVECTOR_FANOUT_ENABLED", true)
}

func nativePgvectorDSN() string {
	for _, key := range []string{"ORCH_PGVECTOR_DSN", "PGVECTOR_DSN"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return normalizePgvectorDSN(value)
		}
	}
	return ""
}

func normalizePgvectorDSN(raw string) string {
	dsn := strings.TrimSpace(raw)
	if dsn == "" {
		return ""
	}
	lower := strings.ToLower(dsn)
	if strings.Contains(lower, "sslmode=") {
		return dsn
	}
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err == nil {
			query := parsed.Query()
			if query.Get("sslmode") == "" {
				query.Set("sslmode", "disable")
				parsed.RawQuery = query.Encode()
			}
			return parsed.String()
		}
		if strings.Contains(dsn, "?") {
			return dsn + "&sslmode=disable"
		}
		return dsn + "?sslmode=disable"
	}
	return dsn + " sslmode=disable"
}

func nativePgvectorTable() string {
	token := strings.TrimSpace(os.Getenv("ORCH_PGVECTOR_TABLE"))
	if token == "" {
		token = "memory_events"
	}
	token = strings.ToLower(token)
	re := regexp.MustCompile(`[^a-zA-Z0-9_]`)
	token = re.ReplaceAllString(token, "_")
	token = strings.Trim(token, "_")
	if token == "" {
		token = "memory_events"
	}
	return token
}

func nativePgvectorDB(ctx context.Context, dsn string) (*sql.DB, error) {
	nativePgvectorDBMu.Lock()
	if cached := nativePgvectorDBByDSN[dsn]; cached != nil {
		nativePgvectorDBMu.Unlock()
		return cached, nil
	}
	nativePgvectorDBMu.Unlock()

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(maxInt(1, envInt("ORCH_PGVECTOR_POOL_MAX_SIZE", 12)))
	db.SetMaxIdleConns(maxInt(1, envInt("ORCH_PGVECTOR_POOL_MIN_SIZE", 1)))
	db.SetConnMaxLifetime(30 * time.Minute)

	pingTimeout := envDurationSeconds("ORCH_PGVECTOR_CONNECT_TIMEOUT_SECS", 8)
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, err
	}

	nativePgvectorDBMu.Lock()
	if cached := nativePgvectorDBByDSN[dsn]; cached != nil {
		nativePgvectorDBMu.Unlock()
		_ = db.Close()
		return cached, nil
	}
	nativePgvectorDBByDSN[dsn] = db
	nativePgvectorDBMu.Unlock()
	return db, nil
}

func nativePgvectorSchemaCacheKey(dsn string, tableName string, dim int) string {
	return strings.TrimSpace(dsn) + "|" + strings.TrimSpace(tableName) + "|" + strconv.Itoa(dim)
}

func nativePgvectorColumnCacheKey(dsn string, tableName string) string {
	return strings.TrimSpace(dsn) + "|" + strings.TrimSpace(tableName)
}

func nativePgvectorEnsureStatements(tableName string, dim int, ivfLists int) []string {
	if dim <= 0 {
		dim = nativeDefaultEmbedDim()
	}
	if dim <= 0 {
		dim = 768
	}
	if ivfLists < 1 {
		ivfLists = 100
	}
	projectIdx := tableName + "_project_idx"
	topicIdx := tableName + "_topic_idx"
	createdIdx := tableName + "_created_idx"
	embedIdx := tableName + "_embedding_ivfflat_idx"
	return []string{
		"CREATE EXTENSION IF NOT EXISTS vector;",
		fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s ("+
				"id BIGSERIAL PRIMARY KEY,"+
				"project TEXT NOT NULL,"+
				"file TEXT NOT NULL,"+
				"summary TEXT NOT NULL,"+
				"topic_path TEXT NOT NULL DEFAULT '',"+
				"created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),"+
				"embedding vector(%d) NOT NULL);",
			tableName,
			dim,
		),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (project);", projectIdx, tableName),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (topic_path);", topicIdx, tableName),
		fmt.Sprintf("CREATE INDEX IF NOT EXISTS %s ON %s (created_at DESC);", createdIdx, tableName),
		fmt.Sprintf(
			"CREATE INDEX IF NOT EXISTS %s ON %s USING ivfflat (embedding vector_cosine_ops) WITH (lists=%d);",
			embedIdx,
			tableName,
			ivfLists,
		),
	}
}

func nativeEnsurePgvectorSchema(
	ctx context.Context,
	db *sql.DB,
	dsn string,
	tableName string,
	dim int,
) error {
	cacheKey := nativePgvectorSchemaCacheKey(dsn, tableName, dim)
	nativePgvectorSchemaMu.Lock()
	if _, ok := nativePgvectorSchemaSet[cacheKey]; ok {
		nativePgvectorSchemaMu.Unlock()
		return nil
	}
	nativePgvectorSchemaMu.Unlock()

	timeout := envDurationSeconds("ORCH_PGVECTOR_SCHEMA_TIMEOUT_SECS", 12)
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	ensureCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	qualified := "public." + tableName
	var existing sql.NullString
	if err := db.QueryRowContext(ensureCtx, "SELECT to_regclass($1);", qualified).Scan(&existing); err != nil {
		return err
	}
	if existing.Valid && strings.TrimSpace(existing.String) != "" {
		// Legacy table already exists; do not attempt shape mutations here.
		nativePgvectorSchemaMu.Lock()
		nativePgvectorSchemaSet[cacheKey] = struct{}{}
		nativePgvectorSchemaMu.Unlock()
		return nil
	}
	ivfLists := maxInt(1, envInt("ORCH_PGVECTOR_IVFFLAT_LISTS", 100))
	for _, statement := range nativePgvectorEnsureStatements(tableName, dim, ivfLists) {
		if _, err := db.ExecContext(ensureCtx, statement); err != nil {
			return err
		}
	}
	nativePgvectorSchemaMu.Lock()
	nativePgvectorSchemaSet[cacheKey] = struct{}{}
	nativePgvectorSchemaMu.Unlock()
	return nil
}

func nativePgvectorTableColumns(
	ctx context.Context,
	db *sql.DB,
	dsn string,
	tableName string,
) (map[string]struct{}, error) {
	cacheKey := nativePgvectorColumnCacheKey(dsn, tableName)
	nativePgvectorColsMu.Lock()
	if cached, ok := nativePgvectorColsByKey[cacheKey]; ok && len(cached) > 0 {
		copySet := make(map[string]struct{}, len(cached))
		for key := range cached {
			copySet[key] = struct{}{}
		}
		nativePgvectorColsMu.Unlock()
		return copySet, nil
	}
	nativePgvectorColsMu.Unlock()

	timeout := envDurationSeconds("ORCH_PGVECTOR_SCHEMA_TIMEOUT_SECS", 12)
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	queryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rows, err := db.QueryContext(
		queryCtx,
		"SELECT column_name FROM information_schema.columns WHERE table_schema='public' AND table_name=$1;",
		tableName,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := map[string]struct{}{}
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			continue
		}
		name = strings.TrimSpace(strings.ToLower(name))
		if name == "" {
			continue
		}
		columns[name] = struct{}{}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		return nil, rowsErr
	}
	nativePgvectorColsMu.Lock()
	nativePgvectorColsByKey[cacheKey] = columns
	nativePgvectorColsMu.Unlock()
	copySet := make(map[string]struct{}, len(columns))
	for key := range columns {
		copySet[key] = struct{}{}
	}
	return copySet, nil
}

func clipRunes(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}

func parseOptionalRFC3339(raw string, fallback time.Time) time.Time {
	token := strings.TrimSpace(raw)
	if token == "" {
		return fallback
	}
	parsed, err := time.Parse(time.RFC3339Nano, token)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, token)
		if err != nil {
			return fallback
		}
	}
	return parsed.UTC()
}

func (s *server) upsertPgvectorFromWrite(
	ctx context.Context,
	item normalizedWrite,
	eventID string,
) (string, error) {
	if !nativeSourceAdapterEnabled(sourcePgvector, true) {
		return "skipped_adapter_disabled", nil
	}
	if !nativePgvectorEnabled() {
		return "skipped_source_disabled", nil
	}
	if !nativePgvectorFanoutEnabled() {
		return "skipped_fanout_disabled", nil
	}
	dsn := nativePgvectorDSN()
	if dsn == "" {
		return "skipped_unconfigured", nil
	}
	db, err := nativePgvectorDB(ctx, dsn)
	if err != nil {
		return "failed_connect", err
	}
	tableName := nativePgvectorTable()
	embedDim := nativeDefaultEmbedDim()
	vector, _, err := nativeEmbedQueryVector(ctx, s.client, item.content, embedDim)
	if err != nil {
		return "failed_embed", err
	}
	if err := nativeEnsurePgvectorSchema(ctx, db, dsn, tableName, len(vector)); err != nil {
		return "failed_schema", err
	}
	columns, err := nativePgvectorTableColumns(ctx, db, dsn, tableName)
	if err != nil {
		return "failed_schema_introspect", err
	}
	summary := strings.TrimSpace(item.content)
	if summary == "" {
		summary = item.fileName
	}
	summary = clipRunes(summary, 1200)
	createdAt := parseOptionalRFC3339(item.createdAt, time.Now().UTC())
	if strings.TrimSpace(eventID) == "" {
		sum := sha256.Sum256([]byte(item.project + "|" + item.fileName + "|" + item.topicPath + "|" + item.content + "|" + createdAt.Format(time.RFC3339Nano)))
		eventID = "gw_" + fmt.Sprintf("%x", sum[:8])
	}
	insertColumns := []string{}
	insertValues := []any{}
	placeholders := []string{}
	appendValue := func(column string, value any, isVector bool) {
		insertColumns = append(insertColumns, column)
		insertValues = append(insertValues, value)
		placeholder := "$" + strconv.Itoa(len(insertValues))
		if isVector {
			placeholder += "::vector"
		}
		placeholders = append(placeholders, placeholder)
	}
	if _, ok := columns["event_id"]; ok {
		appendValue("event_id", eventID, false)
	}
	appendValue("project", strings.TrimSpace(item.project), false)
	appendValue("file", strings.TrimSpace(item.fileName), false)
	appendValue("summary", summary, false)
	if _, ok := columns["topic_path"]; ok {
		appendValue("topic_path", strings.TrimSpace(item.topicPath), false)
	}
	if _, ok := columns["created_at"]; ok {
		appendValue("created_at", createdAt, false)
	}
	if _, ok := columns["updated_at"]; ok {
		appendValue("updated_at", createdAt, false)
	}
	appendValue("embedding", nativePgvectorLiteral(vector), true)

	insertSQL := "INSERT INTO " + tableName + " (" + strings.Join(insertColumns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ");"
	_, err = db.ExecContext(ctx, insertSQL, insertValues...)
	if err != nil {
		return "failed_insert", err
	}
	return "succeeded", nil
}

func nativePgvectorLiteral(vector []float64) string {
	if len(vector) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(vector))
	for _, value := range vector {
		parts = append(parts, strconv.FormatFloat(value, 'f', 8, 64))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func isPostgresUndefinedRelation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		return string(pgErr.Code) == "42P01"
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "relation") && strings.Contains(lower, "does not exist")
}

func (s *server) queryPostgresPgvectorSource(
	ctx context.Context,
	baseRequest map[string]any,
) ([]map[string]any, []string, error) {
	if !nativeSourceAdapterEnabled(sourcePgvector, true) {
		return nil, nil, errors.New("native pgvector adapter disabled")
	}
	if !nativePgvectorEnabled() {
		return []map[string]any{}, nil, nil
	}
	dsn := nativePgvectorDSN()
	if dsn == "" {
		return nil, nil, errors.New("pgvector dsn not configured")
	}
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, errors.New("query is required")
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	scanLimit := maxInt(limit*8, envInt("ORCH_RETRIEVAL_PGVECTOR_SCAN_LIMIT", 320))
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))

	embedDim := nativeDefaultEmbedDim()
	vector, warnings, err := nativeEmbedQueryVector(ctx, s.client, query, embedDim)
	if err != nil {
		return nil, warnings, err
	}
	db, err := nativePgvectorDB(ctx, dsn)
	if err != nil {
		return nil, warnings, err
	}
	tableName := nativePgvectorTable()
	if ensureErr := nativeEnsurePgvectorSchema(ctx, db, dsn, tableName, len(vector)); ensureErr != nil {
		return nil, warnings, ensureErr
	}
	sqlQuery := "SELECT project, file, summary, topic_path, created_at, (1 - (embedding <=> $1::vector)) AS similarity " +
		"FROM " + tableName + " " +
		"WHERE ($2::text = '' OR project = $2) " +
		"AND ($3::text = '' OR topic_path LIKE ($3 || '%')) " +
		"ORDER BY embedding <=> $1::vector " +
		"LIMIT $4;"

	rowsResult, err := db.QueryContext(
		ctx,
		sqlQuery,
		nativePgvectorLiteral(vector),
		projectFilter,
		topicFilter,
		scanLimit,
	)
	if err != nil {
		if isPostgresUndefinedRelation(err) {
			warnings = append(
				warnings,
				"postgres_pgvector relation "+tableName+" missing; returning empty lane (set ORCH_PGVECTOR_TABLE or provision pgvector schema)",
			)
			return []map[string]any{}, warnings, nil
		}
		return nil, warnings, err
	}
	defer rowsResult.Close()

	rows := []map[string]any{}
	for rowsResult.Next() {
		var (
			project    string
			fileName   string
			summary    string
			topicPath  sql.NullString
			createdAt  sql.NullTime
			similarity sql.NullFloat64
		)
		if scanErr := rowsResult.Scan(&project, &fileName, &summary, &topicPath, &createdAt, &similarity); scanErr != nil {
			continue
		}
		project = strings.TrimSpace(project)
		fileName = strings.TrimSpace(fileName)
		summary = strings.TrimSpace(summary)
		if project == "" || fileName == "" || summary == "" {
			continue
		}
		if projectFilter != "" && project != projectFilter {
			continue
		}
		topic := strings.TrimSpace(topicPath.String)
		if topicFilter != "" {
			if topic == "" || (topic != topicFilter && !strings.HasPrefix(topic, topicFilter+"/")) {
				continue
			}
		}
		score := 0.0
		if similarity.Valid {
			score = similarity.Float64
		}
		lexical := textMatchScore(query, project+"\n"+fileName+"\n"+summary)
		if lexical > score {
			score = lexical
		}
		if score <= 0 {
			continue
		}
		row := map[string]any{
			"project":    project,
			"file":       fileName,
			"summary":    summary,
			"score":      score,
			"source":     sourcePgvector,
			"topic_path": topic,
		}
		if createdAt.Valid {
			row["created_at"] = createdAt.Time.UTC().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}
	if rowsErr := rowsResult.Err(); rowsErr != nil {
		return nil, warnings, rowsErr
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, warnings, nil
}

func normalizeMemoryBankSpikeBackend(raw string, fallback string) string {
	token := strings.TrimSpace(strings.ToLower(raw))
	if _, ok := memoryBankSpikeBackendChoices[token]; ok {
		return token
	}
	fallback = strings.TrimSpace(strings.ToLower(fallback))
	if _, ok := memoryBankSpikeBackendChoices[fallback]; ok {
		return fallback
	}
	return "native"
}

func parseMemoryBankSpikeFallbackBackends(
	requestedBackend string,
	configuredList string,
	configuredPrimary string,
) []string {
	out := []string{}
	appendBackend := func(candidate string) {
		normalized := normalizeMemoryBankSpikeBackend(candidate, "")
		if normalized == "" || normalized == "native" || normalized == "disabled" {
			return
		}
		if normalized == requestedBackend {
			return
		}
		for _, existing := range out {
			if existing == normalized {
				return
			}
		}
		out = append(out, normalized)
	}
	if strings.TrimSpace(configuredPrimary) != "" {
		appendBackend(configuredPrimary)
	}
	for _, part := range strings.Split(configuredList, ",") {
		appendBackend(part)
	}
	return out
}

func capMemoryBankBackendSequence(sequence []string, maxBackends int) []string {
	if maxBackends <= 0 || len(sequence) <= maxBackends {
		return append([]string(nil), sequence...)
	}
	return append([]string(nil), sequence[:maxBackends]...)
}

func parseMemoryBankSpikeHedgeBackends(
	sequence []string,
	configuredList string,
	maxParallel int,
) []string {
	if len(sequence) < 2 || maxParallel < 2 {
		return []string{}
	}
	maxParallel = clampInt(maxParallel, 2, len(sequence))
	configuredSet := map[string]struct{}{}
	configured := []string{}
	for _, token := range strings.Split(configuredList, ",") {
		normalized := normalizeMemoryBankSpikeBackend(token, "")
		if normalized == "" || normalized == "native" || normalized == "disabled" {
			continue
		}
		if _, exists := configuredSet[normalized]; exists {
			continue
		}
		configuredSet[normalized] = struct{}{}
		configured = append(configured, normalized)
	}
	candidates := make([]string, 0, maxParallel)
	for _, backend := range sequence {
		if len(configured) > 0 {
			if _, ok := configuredSet[backend]; !ok {
				continue
			}
		}
		already := false
		for _, existing := range candidates {
			if existing == backend {
				already = true
				break
			}
		}
		if already {
			continue
		}
		candidates = append(candidates, backend)
		if len(candidates) >= maxParallel {
			break
		}
	}
	if len(candidates) < 2 {
		return []string{}
	}
	return candidates
}

func memoryBankSpikeBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("ORCH_MEMORY_BANK_SPIKE_HTTP_URL")), "/")
}

func memoryBankSpikeSearchRoute() string {
	route := strings.TrimSpace(os.Getenv("ORCH_MEMORY_BANK_SPIKE_SEARCH_ROUTE"))
	if route == "" {
		route = "/search"
	}
	if !strings.HasPrefix(route, "/") {
		route = "/" + route
	}
	return route
}

func memoryBankSpikeTimeoutForBackend(backend string) time.Duration {
	backend = strings.TrimSpace(strings.ToLower(backend))
	timeoutSecs := envFloat("ORCH_MEMORY_BANK_SPIKE_TIMEOUT_SECS", 1.5)
	if timeoutSecs <= 0 {
		timeoutSecs = 1.5
	}
	overrideEnv := ""
	switch backend {
	case "icm_spike":
		overrideEnv = "ORCH_MEMORY_BANK_SPIKE_TIMEOUT_SECS_ICM"
	case "shodh_spike":
		overrideEnv = "ORCH_MEMORY_BANK_SPIKE_TIMEOUT_SECS_SHODH"
	case "memvid_spike":
		overrideEnv = "ORCH_MEMORY_BANK_SPIKE_TIMEOUT_SECS_MEMVID"
	case "surrealdb_spike":
		overrideEnv = "ORCH_MEMORY_BANK_SPIKE_TIMEOUT_SECS_SURREALDB"
	}
	if overrideEnv != "" {
		override := envFloat(overrideEnv, timeoutSecs)
		if override > 0 {
			timeoutSecs = override
		}
	}
	if timeoutSecs < 0.2 {
		timeoutSecs = 0.2
	}
	return time.Duration(timeoutSecs * float64(time.Second))
}

func classifyMemoryBankSpikeError(err error) (string, string, bool) {
	if err == nil {
		return "", "", false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "timeout", true
	}
	if os.IsTimeout(err) {
		return "timeout", "timeout", true
	}
	lowered := strings.TrimSpace(strings.ToLower(err.Error()))
	if strings.Contains(lowered, "timeout") || strings.Contains(lowered, "deadline exceeded") {
		return "timeout", "timeout", true
	}
	if strings.Contains(lowered, "status=") {
		code := "http_error"
		if idx := strings.Index(lowered, "status="); idx >= 0 {
			statusToken := lowered[idx+7:]
			for i, r := range statusToken {
				if r < '0' || r > '9' {
					statusToken = statusToken[:i]
					break
				}
			}
			if strings.TrimSpace(statusToken) != "" {
				code = "http_" + strings.TrimSpace(statusToken)
			}
		}
		return "http_error", code, false
	}
	if strings.Contains(lowered, "not configured") {
		return "config_error", "backend_not_configured", false
	}
	if strings.Contains(lowered, "connection refused") || strings.Contains(lowered, "no such host") {
		return "request_error", "network_error", false
	}
	return "backend_error", "backend_error", false
}

type memoryBankSpikeProbeResult struct {
	backend    string
	rows       []map[string]any
	err        error
	timedOut   bool
	errorKind  string
	errorCode  string
	timeout    time.Duration
	elapsedMs  float64
	orderIndex int
}

func shouldReplaceMemoryBankHedgeWinner(
	candidateScore float64,
	candidateRows int,
	candidateOrder int,
	winnerScore float64,
	winnerRows int,
	winnerOrder int,
) bool {
	if winnerOrder < 0 {
		return true
	}
	if candidateScore > winnerScore {
		return true
	}
	if candidateScore < winnerScore {
		return false
	}
	if candidateRows > winnerRows {
		return true
	}
	if candidateRows < winnerRows {
		return false
	}
	return candidateOrder < winnerOrder
}

func (s *server) queryMemoryBankSpikeHedge(
	ctx context.Context,
	query string,
	limit int,
	projectFilter string,
	topicFilter string,
	backends []string,
) ([]map[string]any, string, []map[string]any) {
	if len(backends) < 2 {
		return nil, "", []map[string]any{}
	}
	results := make([]memoryBankSpikeProbeResult, 0, len(backends))
	resultCh := make(chan memoryBankSpikeProbeResult, len(backends))
	var wg sync.WaitGroup
	for idx, backend := range backends {
		wg.Add(1)
		go func(orderIndex int, candidate string) {
			defer wg.Done()
			timeout := memoryBankSpikeTimeoutForBackend(candidate)
			started := time.Now()
			rows, err := s.queryMemoryBankSpikeBackend(ctx, query, limit, projectFilter, topicFilter, candidate, timeout)
			errorKind, errorCode, timedOut := classifyMemoryBankSpikeError(err)
			resultCh <- memoryBankSpikeProbeResult{
				backend:    candidate,
				rows:       rows,
				err:        err,
				timedOut:   timedOut,
				errorKind:  errorKind,
				errorCode:  errorCode,
				timeout:    timeout,
				elapsedMs:  roundFloat(float64(time.Since(started).Milliseconds()), 3),
				orderIndex: orderIndex,
			}
		}(idx, backend)
	}
	wg.Wait()
	close(resultCh)
	for result := range resultCh {
		results = append(results, result)
	}
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].orderIndex < results[j].orderIndex
	})
	winnerRows := []map[string]any{}
	winnerBackend := ""
	winnerScore := -1.0
	winnerCount := -1
	winnerOrder := -1
	steps := make([]map[string]any, 0, len(results)+1)
	for _, result := range results {
		step := map[string]any{
			"backend":      result.backend,
			"trigger":      "hedge_probe",
			"timeout_secs": roundFloat(result.timeout.Seconds(), 3),
			"elapsed_ms":   result.elapsedMs,
			"rows":         len(result.rows),
			"order_index":  result.orderIndex + 1,
			"order_total":  len(results),
		}
		if result.err != nil {
			step["status"] = "error"
			step["reason"] = "backend_error"
			if result.timedOut {
				step["reason"] = "timeout"
			}
			step["error"] = strings.TrimSpace(result.err.Error())
			step["error_kind"] = result.errorKind
			step["error_code"] = result.errorCode
			step["timed_out"] = result.timedOut
			step["policy_action"] = "hedge_continue"
			step["terminal"] = false
			steps = append(steps, step)
			continue
		}
		if len(result.rows) == 0 {
			step["status"] = "empty"
			step["reason"] = "no_rows"
			step["policy_action"] = "hedge_continue"
			step["terminal"] = false
			steps = append(steps, step)
			continue
		}
		topScore := parseScore(result.rows[0])
		step["status"] = "success"
		step["reason"] = "rows_returned"
		step["top_score"] = roundFloat(topScore, 6)
		step["policy_action"] = "hedge_candidate"
		step["terminal"] = false
		steps = append(steps, step)
		if shouldReplaceMemoryBankHedgeWinner(
			topScore,
			len(result.rows),
			result.orderIndex,
			winnerScore,
			winnerCount,
			winnerOrder,
		) {
			winnerRows = result.rows
			winnerBackend = result.backend
			winnerScore = topScore
			winnerCount = len(result.rows)
			winnerOrder = result.orderIndex
		}
	}
	if winnerBackend != "" && len(winnerRows) > 0 {
		steps = append(steps, map[string]any{
			"backend":       winnerBackend,
			"status":        "winner",
			"reason":        "top_score_then_rows_then_order",
			"policy_action": "return_rows",
			"rows":          len(winnerRows),
			"top_score":     roundFloat(winnerScore, 6),
			"terminal":      true,
		})
	}
	return winnerRows, winnerBackend, steps
}

func (s *server) queryMemoryBankSpikeBackend(
	ctx context.Context,
	query string,
	limit int,
	projectFilter string,
	topicFilter string,
	backend string,
	timeout time.Duration,
) ([]map[string]any, error) {
	baseURL := memoryBankSpikeBaseURL()
	if baseURL == "" {
		return nil, errors.New("memory-bank spike backend URL is not configured")
	}
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, context.DeadlineExceeded
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload := map[string]any{
		"query":   strings.TrimSpace(query),
		"limit":   clampInt(limit, 1, 100),
		"backend": backend,
	}
	if strings.TrimSpace(projectFilter) != "" {
		payload["project"] = strings.TrimSpace(projectFilter)
	}
	if strings.TrimSpace(topicFilter) != "" {
		payload["topic_path"] = strings.TrimSpace(topicFilter)
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	requestURL := baseURL + memoryBankSpikeSearchRoute()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, requestURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		detail := strings.TrimSpace(string(responseBody))
		if detail == "" {
			return nil, errors.New("memory-bank spike status=" + strconv.Itoa(resp.StatusCode))
		}
		return nil, errors.New("memory-bank spike status=" + strconv.Itoa(resp.StatusCode) + " body=" + detail)
	}
	responsePayload, err := parseJSONMap(responseBody)
	if err != nil {
		return nil, err
	}
	rawRows, _ := responsePayload["results"].([]any)
	if len(rawRows) == 0 {
		rawRows, _ = responsePayload["rows"].([]any)
	}
	rows := make([]map[string]any, 0, len(rawRows))
	for _, raw := range rawRows {
		row, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		project := strings.TrimSpace(anyToString(row["project"]))
		if project == "" {
			project = strings.TrimSpace(projectFilter)
		}
		fileName := strings.TrimSpace(anyToString(row["file"]))
		if fileName == "" {
			fileName = strings.TrimSpace(anyToString(row["path"]))
		}
		summary := strings.TrimSpace(anyToString(row["summary"]))
		if summary == "" {
			summary = strings.TrimSpace(anyToString(row["content"]))
		}
		if project == "" || fileName == "" || summary == "" {
			continue
		}
		if strings.TrimSpace(projectFilter) != "" && project != strings.TrimSpace(projectFilter) {
			continue
		}
		topicPath := strings.TrimSpace(anyToString(row["topic_path"]))
		if strings.TrimSpace(topicFilter) != "" {
			prefix := strings.TrimSpace(topicFilter)
			if topicPath == "" || (topicPath != prefix && !strings.HasPrefix(topicPath, prefix+"/")) {
				continue
			}
		}
		score := anyToFloat(row["score"])
		if score <= 0 {
			score = textMatchScore(query, fileName+"\n"+summary)
		}
		if score <= 0 {
			continue
		}
		parsed := map[string]any{
			"project":    project,
			"file":       fileName,
			"summary":    summary,
			"score":      score,
			"source":     sourceMemoryBank,
			"topic_path": topicPath,
			"backend":    backend,
		}
		if createdAt := strings.TrimSpace(anyToString(row["created_at"])); createdAt != "" {
			parsed["created_at"] = createdAt
		}
		rows = append(rows, parsed)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (s *server) queryMemoryBankSource(
	ctx context.Context,
	incomingHeaders http.Header,
	baseRequest map[string]any,
	explicitSourceOverride bool,
) ([]map[string]any, []string, map[string]any, string, error) {
	if !nativeSourceAdapterEnabled(sourceMemoryBank, true) {
		return nil, nil, nil, sourceOwnerGoNative, errors.New("native memory_bank adapter disabled")
	}
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, nil, sourceOwnerGoNative, errors.New("query is required")
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))
	backendPolicy := map[string]any{}
	if existing, ok := baseRequest["backend_policy"].(map[string]any); ok {
		backendPolicy = existing
	}
	backendRequested := normalizeMemoryBankSpikeBackend(
		anyToString(backendPolicy["memory_bank_backend"]),
		normalizeMemoryBankSpikeBackend(os.Getenv("ORCH_MEMORY_BANK_SEARCH_BACKEND"), "shodh_spike"),
	)
	fallbackList := parseMemoryBankSpikeFallbackBackends(
		backendRequested,
		os.Getenv("ORCH_MEMORY_BANK_SPIKE_FALLBACK_BACKENDS"),
		os.Getenv("ORCH_MEMORY_BANK_SPIKE_FALLBACK_BACKEND"),
	)
	sequence := []string{}
	if backendRequested != "disabled" {
		sequence = append(sequence, backendRequested)
	}
	if backendRequested != "native" && backendRequested != "disabled" {
		sequence = append(sequence, fallbackList...)
	}
	maxChainBackends := envInt("ORCH_MEMORY_BANK_SPIKE_MAX_CHAIN_BACKENDS", 3)
	if maxChainBackends < 1 {
		maxChainBackends = 1
	}
	sequence = capMemoryBankBackendSequence(sequence, maxChainBackends)
	emptyFallbackEnabled := envBool("ORCH_MEMORY_BANK_SPIKE_EMPTY_RESULT_FALLBACK", true)
	fallbackToNativeEnabled := envBool("ORCH_MEMORY_BANK_SPIKE_FALLBACK_TO_NATIVE", true)
	if s.strictNoPythonRuntime {
		fallbackToNativeEnabled = false
	}
	hedgeEnabled := envBool("ORCH_MEMORY_BANK_SPIKE_HEDGE_ENABLED", false)
	hedgeMaxParallel := envInt("ORCH_MEMORY_BANK_SPIKE_HEDGE_MAX_PARALLEL", 2)
	if hedgeMaxParallel < 2 {
		hedgeMaxParallel = 2
	}
	hedgeConfiguredList := strings.TrimSpace(os.Getenv("ORCH_MEMORY_BANK_SPIKE_HEDGE_BACKENDS"))
	hedgeBackends := []string{}
	if hedgeEnabled {
		hedgeBackends = parseMemoryBankSpikeHedgeBackends(sequence, hedgeConfiguredList, hedgeMaxParallel)
	}
	steps := []map[string]any{}
	selectedBackend := ""
	nativeUsed := false
	nativeRows := 0

	appendStep := func(step map[string]any) {
		if len(step) == 0 {
			return
		}
		steps = append(steps, step)
	}
	buildTrace := func(reason string) map[string]any {
		return map[string]any{
			"version":               1,
			"backend_requested":     backendRequested,
			"backend_sequence":      append([]string{}, sequence...),
			"steps":                 steps,
			"selected_backend":      selectedBackend,
			"native_used":           nativeUsed || selectedBackend == "native" || backendRequested == "native",
			"native_rows":           nativeRows,
			"empty_result_fallback": emptyFallbackEnabled,
			"fallback_to_native":    fallbackToNativeEnabled,
			"policy": map[string]any{
				"deterministic":               true,
				"spike_sequence":              append([]string{}, sequence...),
				"max_chain_backends":          maxChainBackends,
				"on_rows_returned":            "return_rows",
				"on_empty_result":             "fallback_next_then_native_if_enabled",
				"on_backend_timeout_or_error": "fallback_next_then_native_if_enabled",
				"terminal_without_native":     !fallbackToNativeEnabled,
				"hedge_enabled":               hedgeEnabled,
				"hedge_backends":              append([]string{}, hedgeBackends...),
				"hedge_max_parallel":          hedgeMaxParallel,
				"hedge_tie_break":             "top_score_then_rows_then_order",
			},
			"reason": strings.TrimSpace(strings.ToLower(reason)),
		}
	}
	runNative := func(reasonPrefix string) ([]map[string]any, []string, map[string]any, string, error) {
		if s.strictNoPythonRuntime {
			appendStep(map[string]any{
				"backend":       "native",
				"status":        "blocked",
				"reason":        "python_runtime_disabled",
				"trigger":       "strict_runtime",
				"policy_action": "return_empty",
				"terminal":      true,
			})
			return []map[string]any{}, nil, buildTrace(reasonPrefix + "_native_blocked"), sourceOwnerGoNative, errors.New("native memory_bank backend disabled by strict runtime policy")
		}
		nativeUsed = true
		nativeStarted := time.Now()
		rows, warnings, _, err := s.queryBackendSourceSingle(
			ctx,
			incomingHeaders,
			baseRequest,
			sourceMemoryBank,
			explicitSourceOverride,
			"native",
		)
		if err != nil {
			appendStep(map[string]any{
				"backend":       "native",
				"status":        "error",
				"reason":        "native_backend_error",
				"trigger":       "backend_exception",
				"policy_action": "return_empty",
				"error":         strings.TrimSpace(err.Error()),
				"elapsed_ms":    roundFloat(float64(time.Since(nativeStarted).Milliseconds()), 3),
				"terminal":      true,
			})
			return nil, warnings, buildTrace(reasonPrefix + "_native_error"), sourceOwnerGoNative, err
		}
		nativeRows = len(rows)
		selectedBackend = "native"
		stepStatus := "success"
		stepReason := "native_rows_returned"
		stepAction := "return_rows"
		terminal := true
		if len(rows) == 0 {
			stepStatus = "empty"
			stepReason = "native_no_rows"
			stepAction = "return_empty"
		}
		appendStep(map[string]any{
			"backend":       "native",
			"status":        stepStatus,
			"reason":        stepReason,
			"rows":          len(rows),
			"trigger":       "native_scan",
			"policy_action": stepAction,
			"elapsed_ms":    roundFloat(float64(time.Since(nativeStarted).Milliseconds()), 3),
			"terminal":      terminal,
		})
		return rows, warnings, buildTrace(reasonPrefix + "_native_completed"), sourceOwnerGoNative, nil
	}

	if backendRequested == "disabled" {
		return []map[string]any{}, nil, buildTrace("backend_disabled"), sourceOwnerGoNative, nil
	}
	if backendRequested == "native" {
		if s.strictNoPythonRuntime {
			return []map[string]any{}, nil, buildTrace("native_requested_blocked"), sourceOwnerGoNative, errors.New("memory_bank native backend is disabled by strict runtime policy")
		}
		return runNative("native_requested")
	}
	attemptedBackends := map[string]struct{}{}
	if hedgeEnabled && len(hedgeBackends) >= 2 {
		hedgeRows, hedgeWinner, hedgeSteps := s.queryMemoryBankSpikeHedge(
			ctx,
			query,
			limit,
			projectFilter,
			topicFilter,
			hedgeBackends,
		)
		for _, backend := range hedgeBackends {
			attemptedBackends[backend] = struct{}{}
		}
		for _, step := range hedgeSteps {
			appendStep(step)
		}
		if len(hedgeRows) > 0 && strings.TrimSpace(hedgeWinner) != "" {
			selectedBackend = strings.TrimSpace(hedgeWinner)
			return hedgeRows, nil, buildTrace("spike_hedge_success"), sourceOwnerGoNative, nil
		}
		appendStep(map[string]any{
			"backend":       strings.Join(hedgeBackends, ","),
			"status":        "fallback",
			"reason":        "hedge_exhausted",
			"trigger":       "hedge_probe",
			"policy_action": "fallback_sequence",
			"terminal":      false,
		})
	}

	remainingSequence := make([]string, 0, len(sequence))
	for _, backend := range sequence {
		if _, attempted := attemptedBackends[backend]; attempted {
			continue
		}
		remainingSequence = append(remainingSequence, backend)
	}
	if len(remainingSequence) == 0 {
		if fallbackToNativeEnabled {
			return runNative("spike_hedge_exhausted")
		}
		return []map[string]any{}, nil, buildTrace("spike_hedge_exhausted_terminal"), sourceOwnerGoNative, nil
	}

	chainTotal := len(remainingSequence)
	for idx, backend := range remainingSequence {
		chainIndex := idx + 1
		isLast := chainIndex == chainTotal
		timeout := memoryBankSpikeTimeoutForBackend(backend)
		started := time.Now()
		rows, err := s.queryMemoryBankSpikeBackend(ctx, query, limit, projectFilter, topicFilter, backend, timeout)
		elapsedMs := roundFloat(float64(time.Since(started).Milliseconds()), 3)
		if err == nil && len(rows) > 0 {
			selectedBackend = backend
			appendStep(map[string]any{
				"backend":       backend,
				"status":        "success",
				"reason":        "rows_returned",
				"rows":          len(rows),
				"trigger":       "rows_returned",
				"policy_action": "return_rows",
				"timeout_secs":  roundFloat(timeout.Seconds(), 3),
				"elapsed_ms":    elapsedMs,
				"chain_index":   chainIndex,
				"chain_total":   chainTotal,
				"terminal":      true,
			})
			return rows, nil, buildTrace("spike_success"), sourceOwnerGoNative, nil
		}
		if err == nil {
			action := "return_empty"
			nextBackend := "native"
			if emptyFallbackEnabled && !isLast {
				action = "fallback_next"
				nextBackend = sequence[idx+1]
			} else if fallbackToNativeEnabled {
				action = "fallback_native"
				nextBackend = "native"
			}
			appendStep(map[string]any{
				"backend":       backend,
				"status":        "empty",
				"reason":        "no_rows",
				"rows":          0,
				"trigger":       "empty_result",
				"policy_action": action,
				"next_backend":  nextBackend,
				"timeout_secs":  roundFloat(timeout.Seconds(), 3),
				"elapsed_ms":    elapsedMs,
				"chain_index":   chainIndex,
				"chain_total":   chainTotal,
				"terminal":      action == "return_empty",
			})
			if action == "fallback_next" {
				appendStep(map[string]any{
					"backend":       backend,
					"status":        "fallback",
					"reason":        "empty_result_to_next",
					"trigger":       "empty_result",
					"policy_action": "fallback_next",
					"next_backend":  nextBackend,
					"chain_index":   chainIndex,
					"chain_total":   chainTotal,
					"terminal":      false,
				})
				continue
			}
			if action == "fallback_native" {
				appendStep(map[string]any{
					"backend":       backend,
					"status":        "fallback",
					"reason":        "empty_result_to_native",
					"trigger":       "empty_result",
					"policy_action": "fallback_native",
					"next_backend":  "native",
					"chain_index":   chainIndex,
					"chain_total":   chainTotal,
					"terminal":      false,
				})
				return runNative("spike_empty")
			}
			selectedBackend = backend
			return []map[string]any{}, nil, buildTrace("spike_empty_terminal"), sourceOwnerGoNative, nil
		}
		errorKind, errorCode, timedOut := classifyMemoryBankSpikeError(err)
		action := "return_empty"
		nextBackend := "native"
		if !isLast {
			action = "fallback_next"
			nextBackend = sequence[idx+1]
		} else if fallbackToNativeEnabled {
			action = "fallback_native"
			nextBackend = "native"
		}
		appendStep(map[string]any{
			"backend": backend,
			"status":  "error",
			"reason": func() string {
				if timedOut {
					return "timeout"
				}
				return "backend_error"
			}(),
			"trigger":       "backend_exception",
			"policy_action": action,
			"error":         strings.TrimSpace(err.Error()),
			"error_kind":    errorKind,
			"error_code":    errorCode,
			"timed_out":     timedOut,
			"next_backend":  nextBackend,
			"timeout_secs":  roundFloat(timeout.Seconds(), 3),
			"elapsed_ms":    elapsedMs,
			"chain_index":   chainIndex,
			"chain_total":   chainTotal,
			"terminal":      action == "return_empty",
		})
		if action == "fallback_next" {
			appendStep(map[string]any{
				"backend": backend,
				"status":  "fallback",
				"reason":  "error_to_next",
				"trigger": func() string {
					if timedOut {
						return "timeout"
					}
					return "backend_exception"
				}(),
				"policy_action": "fallback_next",
				"error_kind":    errorKind,
				"error_code":    errorCode,
				"timed_out":     timedOut,
				"next_backend":  nextBackend,
				"chain_index":   chainIndex,
				"chain_total":   chainTotal,
				"terminal":      false,
			})
			continue
		}
		if action == "fallback_native" {
			appendStep(map[string]any{
				"backend": backend,
				"status":  "fallback",
				"reason":  "error_to_native",
				"trigger": func() string {
					if timedOut {
						return "timeout"
					}
					return "backend_exception"
				}(),
				"policy_action": "fallback_native",
				"error_kind":    errorKind,
				"error_code":    errorCode,
				"timed_out":     timedOut,
				"next_backend":  "native",
				"chain_index":   chainIndex,
				"chain_total":   chainTotal,
				"terminal":      false,
			})
			return runNative("spike_error")
		}
		selectedBackend = backend
		return []map[string]any{}, nil, buildTrace("spike_error_terminal"), sourceOwnerGoNative, nil
	}

	return []map[string]any{}, nil, buildTrace("spike_no_candidates"), sourceOwnerGoNative, nil
}
