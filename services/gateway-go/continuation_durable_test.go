package main

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func newContinuationDurableTestServer(t *testing.T) *server {
	t.Helper()
	policy := retrievalPolicy{
		continuationEventHistory:         16,
		continuationEventTTL:             5 * time.Minute,
		continuationMaxInflight:          1,
		continuationMaxInflightPerSource: 1,
		continuationMaxInflightOverrides: map[string]int{
			sourceLetta: 1,
		},
		continuationSourceCooldown:      0,
		continuationSourceCooldownBySrc: map[string]time.Duration{},
		continuationTimeoutDefault:      2 * time.Second,
		continuationSheddingEnabled:     false,
		continuationDurableEnabled:      true,
		continuationDurableDir:          t.TempDir(),
		continuationDurableMaxPending:   64,
		continuationDurableDrainBatch:   8,
		continuationDurablePollInterval: 250 * time.Millisecond,
		continuationDurableRetryBase:    500 * time.Millisecond,
		continuationDurableRetryMax:     5 * time.Second,
		continuationDurableMaxAttempts:  4,
	}
	s := &server{
		retrieval:                       policy,
		client:                          &http.Client{Timeout: 250 * time.Millisecond},
		continuationSem:                 make(chan struct{}, 1),
		continuationInFlight:            map[string]int{},
		continuationInFlightStarted:     map[string][]time.Time{},
		continuationRetrying:            map[string]int{},
		continuationSourceCooldownUntil: map[string]time.Time{},
		continuationSubscribers:         map[string][]chan map[string]any{},
		continuationHistory:             map[string][]map[string]any{},
		continuationExpiry:              map[string]time.Time{},
		lettaAgentBySession:             map[string]string{},
		lettaAgentVerifiedAt:            map[string]time.Time{},
	}
	s.continuationDurable = newContinuationDurableQueue(policy)
	return s
}

func TestScheduleOrDeferContinuationQueuesDurablyUnderPressure(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	// Saturate in-flight semaphore so continuation scheduling is forced to defer.
	s.continuationSem <- struct{}{}
	token := "durable-token-pressure"

	state, status, _ := s.scheduleOrDeferContinuation(
		http.Header{},
		map[string]any{
			"project": "algotraderv2_rust",
			"query":   "letta warm request",
		},
		sourceLetta,
		"timeout",
		token,
	)
	if state != "deferred" {
		t.Fatalf("expected deferred continuation state, got state=%q status=%q", state, status)
	}
	if status != "durable_queued" {
		t.Fatalf("expected durable_queued status, got %q", status)
	}
	durable := s.continuationDurable.snapshot()
	if durable.Pending != 1 {
		t.Fatalf("expected one durable pending continuation job, got %#v", durable)
	}
	queue := s.continuationQueueSnapshot()
	if queue.DurablePending != 1 {
		t.Fatalf("expected durable pending reflected in queue snapshot, got %#v", queue)
	}
	s.continuationMu.Lock()
	events := s.continuationHistory[token]
	s.continuationMu.Unlock()
	if len(events) == 0 {
		t.Fatalf("expected continuation history event for deferred queueing")
	}
	last := events[len(events)-1]
	if strings.TrimSpace(strings.ToLower(anyToString(last["status"]))) != "durable_queued" {
		t.Fatalf("expected durable_queued continuation event, got %#v", last)
	}
}

func TestDrainContinuationDurableQueueSchedulesAndClearsJob(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	s.continuationSem <- struct{}{}
	token := "durable-token-drain"
	state, _, _ := s.scheduleOrDeferContinuation(
		http.Header{},
		map[string]any{
			"project": "algotraderv2_rust",
			"query":   "warm letta cache",
		},
		sourceLetta,
		"timeout",
		token,
	)
	if state != "deferred" {
		t.Fatalf("expected deferred state before draining, got %q", state)
	}
	<-s.continuationSem
	s.drainContinuationDurableQueue()

	deadline := time.Now().Add(750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if s.continuationDurable.snapshot().Pending == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("expected durable continuation queue to drain to zero, snapshot=%#v", s.continuationDurable.snapshot())
}

func TestScheduleOrDeferContinuationReportsUnavailableWhenDurableDisabled(t *testing.T) {
	s := newContinuationDurableTestServer(t)
	s.continuationDurable.enabled = false
	s.continuationSem <- struct{}{}
	state, status, detail := s.scheduleOrDeferContinuation(
		http.Header{},
		map[string]any{
			"project": "algotraderv2_rust",
			"query":   "unavailable continuation",
		},
		sourceLetta,
		"timeout",
		"durable-token-disabled",
	)
	if state != "unavailable" {
		t.Fatalf("expected unavailable state when durable queue disabled, got %q", state)
	}
	if strings.TrimSpace(status) == "" {
		t.Fatalf("expected schedule status for unavailable continuation")
	}
	if strings.TrimSpace(anyToString(detail["durable_error"])) == "" {
		t.Fatalf("expected durable_error detail when durable queue disabled, got %#v", detail)
	}
}
