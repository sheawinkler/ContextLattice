package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var sqlIdentifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func nativeMindsdbEnabled() bool {
	if !envBool("MINDSDB_ENABLED", true) {
		return false
	}
	return envBool("MINDSDB_AUTOSYNC", true)
}

func nativeMindsdbSQLURL() string {
	if value := strings.TrimSpace(os.Getenv("GO_MINDSDB_SQL_URL")); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("MINDSDB_SQL_URL")); value != "" {
		return value
	}
	baseURL := strings.TrimSpace(os.Getenv("MINDSDB_URL"))
	if baseURL == "" {
		baseURL = "http://mindsdb:47334"
	}
	return strings.TrimRight(baseURL, "/") + "/api/sql/query"
}

func sanitizeSQLIdentifier(value string, fallback string) string {
	token := strings.TrimSpace(value)
	if token == "" {
		token = fallback
	}
	if !sqlIdentifierPattern.MatchString(token) {
		return fallback
	}
	return token
}

func escapeSQLLiteral(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}

func normalizeTimestampValue(value any) string {
	switch typed := value.(type) {
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case primitive.DateTime:
		return typed.Time().UTC().Format(time.RFC3339Nano)
	default:
		return strings.TrimSpace(anyToString(value))
	}
}

func mapFromAny(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case bson.M:
		out := make(map[string]any, len(typed))
		for key, row := range typed {
			out[key] = row
		}
		return out
	default:
		return map[string]any{}
	}
}

func parseMindsdbRows(payload map[string]any) []map[string]any {
	rowsValue, ok := payload["data"]
	if !ok {
		return nil
	}
	rowsList, ok := rowsValue.([]any)
	if !ok || len(rowsList) == 0 {
		return nil
	}
	if first, ok := rowsList[0].(map[string]any); ok {
		rows := make([]map[string]any, 0, len(rowsList))
		rows = append(rows, first)
		for _, item := range rowsList[1:] {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			rows = append(rows, row)
		}
		return rows
	}
	columns := anyToStringSlice(payload["column_names"])
	if len(columns) == 0 {
		return nil
	}
	rows := make([]map[string]any, 0, len(rowsList))
	for _, item := range rowsList {
		rowValues, ok := item.([]any)
		if !ok {
			continue
		}
		row := map[string]any{}
		for idx, column := range columns {
			if idx < len(rowValues) {
				row[column] = rowValues[idx]
			} else {
				row[column] = nil
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func (s *server) queryMongoRawSource(
	ctx context.Context,
	baseRequest map[string]any,
) ([]map[string]any, []string, error) {
	if !nativeSourceAdapterEnabled(sourceMongoRaw, true) {
		return nil, nil, errors.New("native mongo_raw adapter disabled")
	}
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, nil
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))
	scanLimit := maxInt(limit*12, envInt("GO_RETRIEVAL_MONGO_SCAN_LIMIT", 128))
	timeout := s.retrieval.sourceTimeouts[sourceMongoRaw]
	if timeout <= 0 {
		timeout = 6 * time.Second
	}

	warnings := []string{}
	rows := []map[string]any{}
	if s.telemetrySink != nil && s.telemetrySink.enabled && s.telemetrySink.events != nil {
		mongoRows, mongoErr := s.queryMongoRawTelemetryCollection(ctx, query, limit, scanLimit, projectFilter, topicFilter)
		if mongoErr != nil {
			warnings = append(warnings, "mongo_raw telemetry collection query failed: "+mongoErr.Error())
		} else {
			rows = append(rows, mongoRows...)
		}
	}

	if len(rows) == 0 && s.telemetrySpool != nil && s.telemetrySpool.enabled {
		spoolCtx, cancel := capContextTimeout(ctx, timeout)
		spoolRows, spoolErr := s.queryMongoRawSpool(spoolCtx, query, limit, scanLimit, projectFilter, topicFilter)
		cancel()
		if spoolErr != nil {
			warnings = append(warnings, "mongo_raw spool fallback query failed: "+spoolErr.Error())
		} else {
			rows = append(rows, spoolRows...)
			if len(spoolRows) > 0 {
				warnings = append(warnings, "mongo_raw results served from telemetry spool fallback")
			}
		}
	}

	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if len(rows) == 0 && len(warnings) > 0 {
		return rows, warnings, errors.New(strings.Join(warnings, "; "))
	}
	return rows, warnings, nil
}

func (s *server) queryMongoRawTelemetryCollection(
	ctx context.Context,
	query string,
	limit int,
	scanLimit int,
	projectFilter string,
	topicFilter string,
) ([]map[string]any, error) {
	filter := bson.M{}
	if projectFilter != "" {
		filter["project"] = projectFilter
	}
	if topicFilter != "" {
		filter["topic_path"] = bson.M{"$regex": "^" + regexp.QuoteMeta(topicFilter)}
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "created_at", Value: -1}}).
		SetLimit(int64(scanLimit)).
		SetProjection(bson.M{
			"project":        1,
			"file":           1,
			"file_name":      1,
			"summary":        1,
			"content_inline": 1,
			"topic_path":     1,
			"created_at":     1,
			"content_ref":    1,
			"content_hash":   1,
			"agent_id":       1,
			"session_id":     1,
			"tags":           1,
			"meta":           1,
		})
	timeout := s.retrieval.sourceTimeouts[sourceMongoRaw]
	if timeout <= 0 {
		timeout = 6 * time.Second
	}
	queryCtx, cancel := capContextTimeout(ctx, timeout)
	defer cancel()
	cursor, err := s.telemetrySink.events.Find(queryCtx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cursor.Close(queryCtx)

	rows := make([]map[string]any, 0, maxInt(limit, 8))
	for cursor.Next(queryCtx) {
		doc := bson.M{}
		if decodeErr := cursor.Decode(&doc); decodeErr != nil {
			continue
		}
		project := strings.TrimSpace(anyToString(doc["project"]))
		if project == "" {
			project = projectFilter
		}
		fileName := strings.TrimSpace(anyToString(doc["file"]))
		if fileName == "" {
			fileName = strings.TrimSpace(anyToString(doc["file_name"]))
		}
		topicPath := strings.TrimSpace(anyToString(doc["topic_path"]))
		if topicPath == "" {
			topicPath = deriveTopicFromFile(fileName)
		}
		summary := strings.TrimSpace(anyToString(doc["summary"]))
		contentInline := strings.TrimSpace(anyToString(doc["content_inline"]))
		if summary == "" {
			summary = clipText(contentInline, 500)
		}
		score := textMatchScore(query, project+"\n"+fileName+"\n"+topicPath+"\n"+summary+"\n"+contentInline)
		if score <= 0 {
			continue
		}
		meta := mapFromAny(doc["meta"])
		agentID := firstNonEmptyStrings(
			anyToString(doc["agent_id"]),
			anyToString(meta["agent_id"]),
			anyToString(meta["agent"]),
		)
		sessionID := firstNonEmptyStrings(
			anyToString(doc["session_id"]),
			anyToString(meta["session_id"]),
			anyToString(meta["session"]),
		)
		tags := normalizeTagList(doc["tags"], meta["tags"], meta["labels"])
		contentRef := strings.TrimSpace(anyToString(doc["content_ref"]))
		if contentRef == "" {
			contentHash := strings.TrimSpace(anyToString(doc["content_hash"]))
			if contentHash != "" {
				contentRef = "sha256:" + contentHash
			}
		}
		row := map[string]any{
			"project":     project,
			"file":        fileName,
			"summary":     summary,
			"score":       score,
			"source":      sourceMongoRaw,
			"topic_path":  topicPath,
			"created_at":  normalizeTimestampValue(doc["created_at"]),
			"content_ref": contentRef,
		}
		if agentID != "" {
			row["agent_id"] = agentID
		}
		if sessionID != "" {
			row["session_id"] = sessionID
		}
		if len(tags) > 0 {
			row["tags"] = tags
		}
		rows = append(rows, row)
		if len(rows) >= scanLimit {
			break
		}
	}
	if err := cursor.Err(); err != nil {
		return rows, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil
}

func (s *server) queryMongoRawSpool(
	ctx context.Context,
	query string,
	limit int,
	scanLimit int,
	projectFilter string,
	topicFilter string,
) ([]map[string]any, error) {
	if s.telemetrySpool == nil || !s.telemetrySpool.enabled {
		return nil, errors.New("telemetry spool unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	maxScanBytes := int64(clampInt(envInt("GO_RETRIEVAL_MONGO_SPOOL_MAX_SCAN_BYTES", 64*1024*1024), 256*1024, 2*1024*1024*1024))
	maxScanLines := clampInt(envInt("GO_RETRIEVAL_MONGO_SPOOL_MAX_SCAN_LINES", 250000), 1000, 5000000)
	paths := []string{s.telemetrySpool.path, s.telemetrySpool.backupPath}
	rows := make([]map[string]any, 0, maxInt(limit, 8))
	scannedBytes := int64(0)
	scannedLines := 0
	scanBudgetExceeded := false
pathLoop:
	for _, path := range paths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return rows, err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				_ = file.Close()
				return rows, ctx.Err()
			default:
			}
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			scannedLines += 1
			scannedBytes += int64(len(line))
			if scannedLines > maxScanLines || scannedBytes > maxScanBytes {
				scanBudgetExceeded = true
				_ = file.Close()
				break pathLoop
			}
			entry := map[string]any{}
			if err := json.Unmarshal([]byte(line), &entry); err != nil {
				continue
			}
			project := strings.TrimSpace(anyToString(entry["project"]))
			if projectFilter != "" && project != projectFilter {
				continue
			}
			fileName := strings.TrimSpace(anyToString(entry["file_name"]))
			topicPath := strings.TrimSpace(anyToString(entry["topic_path"]))
			if topicFilter != "" && topicPath != "" && topicPath != topicFilter && !strings.HasPrefix(topicPath, topicFilter+"/") {
				continue
			}
			content := strings.TrimSpace(anyToString(entry["content"]))
			summary := clipText(content, 500)
			score := textMatchScore(query, project+"\n"+fileName+"\n"+topicPath+"\n"+summary+"\n"+content)
			if score <= 0 {
				continue
			}
			row := map[string]any{
				"project":     project,
				"file":        fileName,
				"summary":     summary,
				"score":       score,
				"source":      sourceMongoRaw,
				"topic_path":  topicPath,
				"created_at":  strings.TrimSpace(anyToString(entry["timestamp"])),
				"content_ref": strings.TrimSpace(anyToString(entry["spool_ref"])),
			}
			if agentID := strings.TrimSpace(anyToString(entry["agent_id"])); agentID != "" {
				row["agent_id"] = agentID
			}
			if sessionID := strings.TrimSpace(anyToString(entry["session_id"])); sessionID != "" {
				row["session_id"] = sessionID
			}
			if tags := normalizeTagList(entry["tags"]); len(tags) > 0 {
				row["tags"] = tags
			}
			rows = append(rows, row)
			if len(rows) >= scanLimit {
				break
			}
		}
		_ = file.Close()
		if len(rows) >= scanLimit {
			break
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	if scanBudgetExceeded && len(rows) == 0 {
		return rows, errors.New("mongo_raw spool scan budget exceeded")
	}
	return rows, nil
}

func (s *server) queryMindsdbSource(
	ctx context.Context,
	baseRequest map[string]any,
) ([]map[string]any, []string, error) {
	if !nativeSourceAdapterEnabled(sourceMindsdb, true) {
		return nil, nil, errors.New("native mindsdb adapter disabled")
	}
	if !nativeMindsdbEnabled() {
		return nil, nil, errors.New("mindsdb disabled by config")
	}
	query := strings.TrimSpace(anyToString(baseRequest["query"]))
	if query == "" {
		return nil, nil, nil
	}
	limit := clampInt(anyToInt(baseRequest["limit"], 10), 1, 100)
	projectFilter := strings.TrimSpace(anyToString(baseRequest["project"]))
	topicFilter := strings.TrimSpace(anyToString(baseRequest["topic_path"]))
	scanLimit := maxInt(limit*12, envInt("GO_RETRIEVAL_MINDSDB_SCAN_LIMIT", 120))

	dbName := sanitizeSQLIdentifier(os.Getenv("MINDSDB_AUTOSYNC_DB"), "files")
	tableName := sanitizeSQLIdentifier(os.Getenv("MINDSDB_AUTOSYNC_TABLE"), "memory_events")
	whereParts := make([]string, 0, 6)
	if projectFilter != "" {
		whereParts = append(whereParts, "project = '"+escapeSQLLiteral(projectFilter)+"'")
	}
	if topicFilter != "" {
		escapedTopic := escapeSQLLiteral(topicFilter)
		whereParts = append(whereParts, "(file LIKE '"+escapedTopic+"/%' OR file LIKE '"+escapedTopic+"%')")
	}
	terms := queryTerms(query, 6)
	if len(terms) > 0 {
		termPredicates := make([]string, 0, len(terms)*2)
		for _, term := range terms {
			escaped := escapeSQLLiteral(strings.ToLower(term))
			termPredicates = append(termPredicates, "LOWER(summary) LIKE '%"+escaped+"%'")
			termPredicates = append(termPredicates, "LOWER(file) LIKE '%"+escaped+"%'")
		}
		whereParts = append(whereParts, "("+strings.Join(termPredicates, " OR ")+")")
	}
	whereClause := ""
	if len(whereParts) > 0 {
		whereClause = " WHERE " + strings.Join(whereParts, " AND ")
	}
	sqlQuery := "SELECT project, file, summary, created_at, agent_id, session_id, tags, content_ref, content_hash " +
		"FROM " + dbName + "." + tableName + whereClause + " ORDER BY created_at DESC LIMIT " + strconv.Itoa(scanLimit) + ";"

	sqlURL := strings.TrimSpace(nativeMindsdbSQLURL())
	if sqlURL == "" {
		return nil, nil, errors.New("mindsdb SQL URL is not configured")
	}
	payloadBytes, err := json.Marshal(map[string]any{"query": sqlQuery})
	if err != nil {
		return nil, nil, err
	}
	timeout := s.retrieval.sourceTimeouts[sourceMindsdb]
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	requestCtx, cancel := capContextTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, sqlURL, bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	responsePayload, err := parseJSONMap(body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode >= 400 || strings.EqualFold(anyToString(responsePayload["type"]), "error") {
		detail := strings.TrimSpace(anyToString(responsePayload["error_message"]))
		if detail == "" {
			detail = clipText(strings.TrimSpace(string(body)), 320)
		}
		return nil, nil, errors.New("mindsdb SQL query failed: " + detail)
	}
	parsedRows := parseMindsdbRows(responsePayload)
	rows := make([]map[string]any, 0, len(parsedRows))
	for _, record := range parsedRows {
		project := strings.TrimSpace(anyToString(record["project"]))
		if project == "" {
			project = projectFilter
		}
		fileName := strings.TrimSpace(anyToString(record["file"]))
		summary := strings.TrimSpace(anyToString(record["summary"]))
		if summary == "" {
			summary = "MindsDB memory row for " + fileName
		}
		topicPath := deriveTopicFromFile(fileName)
		if topicFilter != "" && topicPath != topicFilter && !strings.HasPrefix(topicPath, topicFilter+"/") {
			continue
		}
		score := textMatchScore(query, project+"\n"+fileName+"\n"+summary+"\n"+topicPath)
		if score <= 0 && len(terms) > 0 {
			continue
		}
		contentRef := strings.TrimSpace(anyToString(record["content_ref"]))
		if contentRef == "" {
			contentHash := strings.TrimSpace(anyToString(record["content_hash"]))
			if contentHash != "" {
				contentRef = "sha256:" + contentHash
			}
		}
		row := map[string]any{
			"project":     project,
			"file":        fileName,
			"summary":     summary,
			"score":       score,
			"source":      sourceMindsdb,
			"topic_path":  topicPath,
			"created_at":  normalizeTimestampValue(record["created_at"]),
			"content_ref": contentRef,
		}
		if agentID := strings.TrimSpace(anyToString(record["agent_id"])); agentID != "" {
			row["agent_id"] = agentID
		}
		if sessionID := strings.TrimSpace(anyToString(record["session_id"])); sessionID != "" {
			row["session_id"] = sessionID
		}
		if tags := normalizeTagList(record["tags"]); len(tags) > 0 {
			row["tags"] = tags
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return parseScore(rows[i]) > parseScore(rows[j])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, nil, nil
}
