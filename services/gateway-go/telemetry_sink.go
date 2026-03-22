package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

const (
	telemetryBlobSchemaVersion = 1
	telemetryEventSchemaV2     = 2
)

type telemetrySink struct {
	enabled                 bool
	client                  *mongo.Client
	events                  *mongo.Collection
	blobs                   *mongo.Collection
	blobCompressionMinBytes int
	blobCodec               string
	blobCodecZstdLevel      int
	contentPreviewChars     int
	retentionDays           int
	gcEnabled               bool
	gcInterval              time.Duration
	gcGrace                 time.Duration
	gcBatchLimit            int
}

type telemetryIngestResult struct {
	EventID         string
	ContentRef      string
	ContentHash     string
	StoredInline    bool
	CompressedBytes int
	RawBytes        int
	Codec           string
}

type telemetryBlobRecord struct {
	ID string `bson:"_id"`
}

func newTelemetrySinkFromEnv() (*telemetrySink, error) {
	enabled := envBool("GO_TELEMETRY_SINK_ENABLED", true)
	if !enabled {
		return &telemetrySink{enabled: false}, nil
	}
	mongoURI := strings.TrimSpace(os.Getenv("MONGODB_URI"))
	if mongoURI == "" {
		mongoURI = "mongodb://mongo:27017"
	}
	dbName := strings.TrimSpace(os.Getenv("GO_TELEMETRY_DB"))
	if dbName == "" {
		dbName = strings.TrimSpace(os.Getenv("ORCH_TELEMETRY_DB"))
	}
	if dbName == "" {
		dbName = "memmcp_raw"
	}
	eventsCollection := strings.TrimSpace(os.Getenv("GO_TELEMETRY_EVENTS_COLLECTION"))
	if eventsCollection == "" {
		eventsCollection = "memory_write_telemetry"
	}
	blobsCollection := strings.TrimSpace(os.Getenv("GO_TELEMETRY_BLOBS_COLLECTION"))
	if blobsCollection == "" {
		blobsCollection = "memory_write_blobs"
	}
	connectTimeout := envDurationSeconds("GO_TELEMETRY_CONNECT_TIMEOUT_SECS", 5)
	if connectTimeout < time.Second {
		connectTimeout = time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	client, err := mongo.Connect(
		ctx,
		options.Client().ApplyURI(mongoURI),
	)
	if err != nil {
		return nil, fmt.Errorf("connect mongo telemetry sink: %w", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		return nil, fmt.Errorf("ping mongo telemetry sink: %w", err)
	}

	sink := &telemetrySink{
		enabled:                 true,
		client:                  client,
		events:                  client.Database(dbName).Collection(eventsCollection),
		blobs:                   client.Database(dbName).Collection(blobsCollection),
		blobCompressionMinBytes: envInt("GO_TELEMETRY_BLOB_COMPRESSION_MIN_BYTES", 256),
		blobCodec:               strings.ToLower(strings.TrimSpace(os.Getenv("GO_TELEMETRY_BLOB_CODEC"))),
		blobCodecZstdLevel:      envInt("GO_TELEMETRY_BLOB_ZSTD_LEVEL", 3),
		contentPreviewChars:     envInt("GO_TELEMETRY_CONTENT_PREVIEW_CHARS", 240),
		retentionDays:           envInt("GO_TELEMETRY_RETENTION_DAYS", 75),
		gcEnabled:               envBool("GO_TELEMETRY_BLOB_GC_ENABLED", true),
		gcInterval:              envDurationSeconds("GO_TELEMETRY_BLOB_GC_INTERVAL_SECS", 3600),
		gcGrace:                 envDurationSeconds("GO_TELEMETRY_BLOB_GC_GRACE_SECS", 3600*24),
		gcBatchLimit:            envInt("GO_TELEMETRY_BLOB_GC_BATCH_LIMIT", 500),
	}
	if sink.blobCompressionMinBytes < 64 {
		sink.blobCompressionMinBytes = 64
	}
	if sink.blobCodec == "" {
		sink.blobCodec = "zstd"
	}
	if sink.blobCodec != "zstd" && sink.blobCodec != "gzip" {
		sink.blobCodec = "zstd"
	}
	if sink.blobCodecZstdLevel < 1 {
		sink.blobCodecZstdLevel = 1
	}
	if sink.blobCodecZstdLevel > 19 {
		sink.blobCodecZstdLevel = 19
	}
	if sink.contentPreviewChars < 80 {
		sink.contentPreviewChars = 80
	}
	if sink.retentionDays < 1 {
		sink.retentionDays = 75
	}
	if sink.gcInterval < 30*time.Second {
		sink.gcInterval = 30 * time.Second
	}
	if sink.gcGrace < 15*time.Minute {
		sink.gcGrace = 15 * time.Minute
	}
	if sink.gcBatchLimit < 50 {
		sink.gcBatchLimit = 50
	}

	if err := sink.ensureIndexes(ctx); err != nil {
		return nil, err
	}
	if sink.gcEnabled {
		sink.startBlobGCWorker()
	}
	return sink, nil
}

func (s *telemetrySink) ensureIndexes(ctx context.Context) error {
	if s == nil || !s.enabled {
		return nil
	}
	expireAfterSecs := int32(s.retentionDays * 24 * 3600)
	_, err := s.events.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "created_at", Value: 1}},
			Options: options.Index().SetName("ttl_created_at").SetExpireAfterSeconds(expireAfterSecs),
		},
		{
			Keys:    bson.D{{Key: "content_ref", Value: 1}},
			Options: options.Index().SetName("content_ref"),
		},
		{
			Keys:    bson.D{{Key: "project", Value: 1}, {Key: "topic_path", Value: 1}, {Key: "created_at", Value: -1}},
			Options: options.Index().SetName("project_topic_created"),
		},
	})
	if err != nil {
		return fmt.Errorf("create telemetry event indexes: %w", err)
	}
	_, err = s.blobs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "updated_at", Value: 1}},
			Options: options.Index().SetName("updated_at"),
		},
		{
			Keys:    bson.D{{Key: "ref_count", Value: 1}},
			Options: options.Index().SetName("ref_count"),
		},
	})
	if err != nil {
		return fmt.Errorf("create telemetry blob indexes: %w", err)
	}
	return nil
}

func (s *telemetrySink) ingestWrite(
	ctx context.Context,
	item normalizedWrite,
	meta map[string]any,
) (telemetryIngestResult, error) {
	if s == nil || !s.enabled {
		return telemetryIngestResult{}, fmt.Errorf("telemetry sink disabled")
	}
	now := time.Now().UTC()
	eventID := primitive.NewObjectID().Hex()
	contentHash := sha256Hex(item.content)
	preview := clipText(item.content, s.contentPreviewChars)
	storedInline := true
	contentRef := ""
	compressedBytes := 0
	rawBytes := len(item.content)
	codecUsed := ""

	if rawBytes >= s.blobCompressionMinBytes {
		compressed, codec, err := compressTelemetryContent(item.content, s.blobCodec, s.blobCodecZstdLevel)
		if err != nil {
			return telemetryIngestResult{}, fmt.Errorf("compress telemetry content: %w", err)
		}
		codecUsed = codec
		compressedBytes = len(compressed)
		contentRef = contentHash
		storedInline = false
		blobDoc := bson.M{
			"_id":              contentRef,
			"schema_version":   telemetryBlobSchemaVersion,
			"codec":            codec,
			"content_hash":     contentHash,
			"content_bytes":    rawBytes,
			"compressed_bytes": compressedBytes,
			"payload":          primitive.Binary{Subtype: 0x00, Data: compressed},
			"created_at":       now,
			"updated_at":       now,
		}
		_, err = s.blobs.UpdateOne(
			ctx,
			bson.M{"_id": contentRef},
			bson.M{
				"$setOnInsert": blobDoc,
				"$set":         bson.M{"updated_at": now},
				"$inc":         bson.M{"ref_count": 1},
			},
			options.Update().SetUpsert(true),
		)
		if err != nil {
			return telemetryIngestResult{}, fmt.Errorf("upsert telemetry blob: %w", err)
		}
	}

	doc := bson.M{
		"_id":              eventID,
		"event_id":         eventID,
		"schema_version":   telemetryEventSchemaV2,
		"project":          item.project,
		"file":             item.fileName,
		"topic_path":       item.topicPath,
		"summary":          preview,
		"content_hash":     contentHash,
		"content_ref":      contentRef,
		"content_inline":   "",
		"raw_bytes":        rawBytes,
		"compressed_bytes": compressedBytes,
		"telemetry_like":   true,
		"lane":             "telemetry_mongo_only",
		"created_at":       now,
		"updated_at":       now,
	}
	if storedInline {
		doc["content_inline"] = item.content
	}
	if len(meta) > 0 {
		doc["meta"] = meta
	}

	_, err := s.events.InsertOne(ctx, doc)
	if err != nil {
		if contentRef != "" {
			_, _ = s.blobs.UpdateOne(
				ctx,
				bson.M{"_id": contentRef},
				bson.M{"$inc": bson.M{"ref_count": -1}, "$set": bson.M{"updated_at": now}},
			)
		}
		return telemetryIngestResult{}, fmt.Errorf("insert telemetry event: %w", err)
	}

	return telemetryIngestResult{
		EventID:         eventID,
		ContentRef:      contentRef,
		ContentHash:     contentHash,
		StoredInline:    storedInline,
		CompressedBytes: compressedBytes,
		RawBytes:        rawBytes,
		Codec:           codecUsed,
	}, nil
}

func (s *telemetrySink) runBlobGCOnce(ctx context.Context) (map[string]any, error) {
	if s == nil || !s.enabled {
		return map[string]any{"enabled": false}, nil
	}
	cutoff := time.Now().UTC().Add(-s.gcGrace)
	filter := bson.M{"updated_at": bson.M{"$lt": cutoff}}
	findOpts := options.Find().SetLimit(int64(s.gcBatchLimit)).SetSort(bson.D{{Key: "updated_at", Value: 1}})
	cursor, err := s.blobs.Find(ctx, filter, findOpts)
	if err != nil {
		return nil, fmt.Errorf("query telemetry blobs for gc: %w", err)
	}
	defer cursor.Close(ctx)

	scanned := 0
	deleted := 0
	failed := 0
	for cursor.Next(ctx) {
		scanned += 1
		var blob telemetryBlobRecord
		if err := cursor.Decode(&blob); err != nil {
			failed += 1
			continue
		}
		if strings.TrimSpace(blob.ID) == "" {
			continue
		}
		count, err := s.events.CountDocuments(
			ctx,
			bson.M{"content_ref": blob.ID},
			options.Count().SetLimit(1),
		)
		if err != nil {
			failed += 1
			continue
		}
		if count > 0 {
			continue
		}
		if _, err := s.blobs.DeleteOne(ctx, bson.M{"_id": blob.ID}); err != nil {
			failed += 1
			continue
		}
		deleted += 1
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate telemetry blob gc cursor: %w", err)
	}
	return map[string]any{
		"enabled":    true,
		"scanned":    scanned,
		"deleted":    deleted,
		"failed":     failed,
		"cutoffIso":  cutoff.Format(time.RFC3339),
		"batchLimit": s.gcBatchLimit,
	}, nil
}

func (s *telemetrySink) summary(ctx context.Context) (map[string]any, error) {
	if s == nil || !s.enabled {
		return map[string]any{"enabled": false}, nil
	}
	eventCount, eventErr := s.events.EstimatedDocumentCount(ctx)
	blobCount, blobErr := s.blobs.EstimatedDocumentCount(ctx)
	summary := map[string]any{
		"enabled":                 true,
		"database":                s.events.Database().Name(),
		"eventsCollection":        s.events.Name(),
		"blobsCollection":         s.blobs.Name(),
		"retentionDays":           s.retentionDays,
		"blobCompressionMinBytes": s.blobCompressionMinBytes,
		"blobCodec":               s.blobCodec,
		"blobCodecZstdLevel":      s.blobCodecZstdLevel,
		"blobGCEnabled":           s.gcEnabled,
		"blobGCIntervalSecs":      int(s.gcInterval / time.Second),
		"blobGCGraceSecs":         int(s.gcGrace / time.Second),
		"blobGCBatchLimit":        s.gcBatchLimit,
		"estimatedEventDocs":      eventCount,
		"estimatedBlobDocs":       blobCount,
		"capturedAt":              time.Now().UTC().Format(time.RFC3339),
	}
	if eventErr != nil {
		summary["eventCountError"] = eventErr.Error()
	}
	if blobErr != nil {
		summary["blobCountError"] = blobErr.Error()
	}
	return summary, nil
}

func (s *telemetrySink) startBlobGCWorker() {
	if s == nil || !s.enabled || !s.gcEnabled {
		return
	}
	interval := s.gcInterval
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			<-ticker.C
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			result, err := s.runBlobGCOnce(ctx)
			cancel()
			if err != nil {
				log.Printf("telemetry blob gc failed: %v", err)
				continue
			}
			if deleted := anyToInt(result["deleted"], 0); deleted > 0 {
				log.Printf("telemetry blob gc deleted=%d scanned=%d", deleted, anyToInt(result["scanned"], 0))
			}
		}
	}()
}

func sha256Hex(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func gzipCompress(value string) ([]byte, error) {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(writer, value); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func zstdCompress(value string, level int) ([]byte, error) {
	var buffer bytes.Buffer
	encoderLevel := zstd.SpeedFastest
	switch {
	case level >= 12:
		encoderLevel = zstd.SpeedBestCompression
	case level >= 7:
		encoderLevel = zstd.SpeedBetterCompression
	case level >= 3:
		encoderLevel = zstd.SpeedDefault
	}
	encoder, err := zstd.NewWriter(&buffer, zstd.WithEncoderLevel(encoderLevel))
	if err != nil {
		return nil, err
	}
	if _, err := encoder.Write([]byte(value)); err != nil {
		_ = encoder.Close()
		return nil, err
	}
	if err := encoder.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func compressTelemetryContent(value string, requestedCodec string, zstdLevel int) ([]byte, string, error) {
	codec := strings.ToLower(strings.TrimSpace(requestedCodec))
	if codec == "" {
		codec = "zstd"
	}
	if codec == "zstd" {
		compressed, err := zstdCompress(value, zstdLevel)
		if err == nil {
			return compressed, "zstd", nil
		}
		compressed, gzipErr := gzipCompress(value)
		if gzipErr != nil {
			return nil, "", fmt.Errorf("zstd=%v; gzip=%v", err, gzipErr)
		}
		return compressed, "gzip", nil
	}
	compressed, err := gzipCompress(value)
	if err != nil {
		return nil, "", err
	}
	return compressed, "gzip", nil
}
