package main

import (
	"bytes"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestBuildTelemetryBlobUpsertUpdateAvoidsConflictingPaths(t *testing.T) {
	now := time.Date(2026, 3, 26, 12, 0, 0, 0, time.UTC)
	compressed := []byte{0x01, 0x02, 0x03}
	update := buildTelemetryBlobUpsertUpdate(
		"hash_ref",
		"hash_ref",
		"zstd",
		128,
		32,
		compressed,
		now,
	)

	setOnInsert, ok := update["$setOnInsert"].(bson.M)
	if !ok {
		t.Fatalf("expected $setOnInsert bson.M, got %T", update["$setOnInsert"])
	}
	set, ok := update["$set"].(bson.M)
	if !ok {
		t.Fatalf("expected $set bson.M, got %T", update["$set"])
	}
	inc, ok := update["$inc"].(bson.M)
	if !ok {
		t.Fatalf("expected $inc bson.M, got %T", update["$inc"])
	}

	if _, exists := setOnInsert["updated_at"]; exists {
		t.Fatalf("updated_at must not be present in $setOnInsert to avoid Mongo update path conflicts")
	}
	if _, exists := set["created_at"]; exists {
		t.Fatalf("created_at should remain immutable and only appear in $setOnInsert")
	}

	for key := range setOnInsert {
		if _, exists := set[key]; exists {
			t.Fatalf("conflicting update path %q appears in both $setOnInsert and $set", key)
		}
	}

	updatedAt, ok := set["updated_at"].(time.Time)
	if !ok || !updatedAt.Equal(now) {
		t.Fatalf("expected $set.updated_at=%s, got %#v", now.Format(time.RFC3339Nano), set["updated_at"])
	}
	createdAt, ok := setOnInsert["created_at"].(time.Time)
	if !ok || !createdAt.Equal(now) {
		t.Fatalf("expected $setOnInsert.created_at=%s, got %#v", now.Format(time.RFC3339Nano), setOnInsert["created_at"])
	}

	payload, ok := setOnInsert["payload"].(primitive.Binary)
	if !ok {
		t.Fatalf("expected payload primitive.Binary, got %T", setOnInsert["payload"])
	}
	if !bytes.Equal(payload.Data, compressed) {
		t.Fatalf("expected payload bytes %v, got %v", compressed, payload.Data)
	}

	if refCount := anyToInt(inc["ref_count"], 0); refCount != 1 {
		t.Fatalf("expected $inc.ref_count=1, got %#v", inc["ref_count"])
	}
}
