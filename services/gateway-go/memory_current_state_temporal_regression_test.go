package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestCurrentStateTemporalScoreEquivalentInstantsBeatDifferentInstants(t *testing.T) {
	instant := time.Date(2032, 1, 1, 0, 15, 30, 120000000, time.UTC)
	offsets := []int{-720, -345, -15, 0, 15, 345, 765, 840}
	for _, queryOffset := range offsets {
		for _, bodyOffset := range offsets {
			t.Run(fmt.Sprintf("query_%d_body_%d", queryOffset, bodyOffset), func(t *testing.T) {
				queryAt := instant.In(time.FixedZone("query", queryOffset*60))
				bodyAt := instant.In(time.FixedZone("body", bodyOffset*60))
				wrongAt := time.Date(queryAt.Year(), queryAt.Month(), queryAt.Day(), queryAt.Hour(), queryAt.Minute(), queryAt.Second(), queryAt.Nanosecond(), bodyAt.Location())
				if wrongAt.Equal(instant) {
					wrongAt = wrongAt.Add(time.Second)
				}
				query := "photon detector spectrum " + queryAt.Format("2006-01-02T15:04:05.000000000Z07:00")
				body := "photon, detector, spectrum observed " + strings.ToLower(bodyAt.Format(time.RFC3339Nano))
				wrong := "photon, detector, spectrum observed " + wrongAt.Format(time.RFC3339Nano)
				correctScore := currentStateLexicalScore(query, "laboratory/optics", body)
				wrongScore := currentStateLexicalScore(query, "laboratory/optics", wrong)
				if correctScore <= wrongScore {
					t.Fatalf("representation defeats instant: correct=%v wrong=%v query=%q body=%q", correctScore, wrongScore, query, body)
				}
				store := currentStateSearchTestStore(
					memoryStoreEntry{Project: "laboratory", FileName: "records/target.md", TopicPath: "laboratory/optics", Summary: body, EventID: "correct", Lifecycle: "durable", CreatedAt: "2025-01-01T00:00:00Z"},
					memoryStoreEntry{Project: "laboratory", FileName: "records/newer.md", TopicPath: "laboratory/optics", Summary: wrong, EventID: "different", Lifecycle: "durable", CreatedAt: "2025-02-01T00:00:00Z"},
				)
				rows, _, err := store.searchCurrentStateRows(context.Background(), query, "laboratory", "laboratory/optics", 5, false, false)
				if err != nil || len(rows) == 0 || anyToString(rows[0]["event_id"]) != "correct" {
					t.Fatalf("equivalent instant lost to newer wrong record: err=%v rows=%#v", err, rows)
				}
			})
		}
	}
}

func TestCurrentStatePlainQueryScoresExactlyPreserveLegacyTimestampBodies(t *testing.T) {
	mismatches := 0
	for i := 0; i < 256; i++ {
		stamp := time.Date(2041+i%17, time.Month(1+i%12), 1+i%27, i%24, i%60, i%60, i*1001, time.UTC).Format(time.RFC3339Nano)
		query := "photon detector spectrum"
		if i%2 == 1 {
			query += " id=decorated ts=" + stamp
		}
		body := "photon, detector, spectrum observed (" + stamp + "), expires " + stamp
		got := currentStateLexicalScore(query, "laboratory/optics", body)
		want := legacyTemporalRegressionScore(query, "laboratory/optics", body)
		if math.Float64bits(got) != math.Float64bits(want) {
			mismatches++
			if mismatches <= 3 {
				t.Logf("case=%d got=%.17g legacy=%.17g", i, got, want)
			}
		}
	}
	if mismatches != 0 {
		t.Fatalf("non-temporal query changed %d/256 scores", mismatches)
	}
}

// The reference below is the unmodified pre-timestamp score body from 997e6835.
func legacyTemporalRegressionScore(query string, topicPath string, summary string) float64 {
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
