package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/lib/pq"
)

func TestIsPostgresUndefinedRelationFromPQCode(t *testing.T) {
	err := &pq.Error{Code: "42P01"}
	if !isPostgresUndefinedRelation(err) {
		t.Fatalf("expected undefined relation detector to match sqlstate 42P01")
	}
}

func TestIsPostgresUndefinedRelationFromMessageFallback(t *testing.T) {
	err := errors.New(`pq: relation "memory_events" does not exist`)
	if !isPostgresUndefinedRelation(err) {
		t.Fatalf("expected undefined relation detector to match message fallback")
	}
}

func TestIsPostgresUndefinedRelationRejectsUnrelatedErrors(t *testing.T) {
	err := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	if isPostgresUndefinedRelation(err) {
		t.Fatalf("expected unrelated error to be ignored")
	}
}

func TestNativePgvectorEnsureStatementsIncludesCoreDDL(t *testing.T) {
	statements := nativePgvectorEnsureStatements("memory_events", 768, 120)
	if len(statements) < 6 {
		t.Fatalf("expected ddl statements, got %d", len(statements))
	}
	joined := strings.Join(statements, "\n")
	for _, token := range []string{
		"CREATE EXTENSION IF NOT EXISTS vector",
		"CREATE TABLE IF NOT EXISTS memory_events",
		"embedding vector(768)",
		"memory_events_embedding_ivfflat_idx",
		"lists=120",
	} {
		if !strings.Contains(joined, token) {
			t.Fatalf("expected token %q in ddl: %s", token, joined)
		}
	}
}

func TestNativePgvectorEnsureStatementsNormalizesInvalidParameters(t *testing.T) {
	statements := nativePgvectorEnsureStatements("memory_events", 0, 0)
	joined := strings.Join(statements, "\n")
	if !strings.Contains(joined, "embedding vector(768)") {
		t.Fatalf("expected default embedding dim in ddl: %s", joined)
	}
	if !strings.Contains(joined, "lists=100") {
		t.Fatalf("expected default ivfflat lists in ddl: %s", joined)
	}
}
