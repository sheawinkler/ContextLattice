package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func waitTelemetryTest(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("telemetry initialization did not complete")
	}
}

func TestTelemetryStartupRecoversAfterInitialFailure(t *testing.T) {
	sink := &telemetrySink{enabled: true}
	retry := make(chan struct{})
	var calls atomic.Int32
	sink.initializeWithRecovery(context.Background(), time.Second, func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("mongodb://private-user:private-password@private-host/?token=secret")
		}
		return nil
	}, func(ctx context.Context, _ time.Duration) bool {
		select {
		case <-retry:
			return true
		case <-ctx.Done():
			return false
		}
	})
	t.Cleanup(func() { _ = sink.close(context.Background()) })
	if sink.isReady() {
		t.Fatal("failed initialization was published as ready")
	}
	before, _ := json.Marshal(sink.initializationSnapshot())
	for _, secret := range []string{"private-user", "private-password", "private-host", "secret"} {
		if strings.Contains(string(before), secret) {
			t.Fatalf("status leaked initialization error: %s", before)
		}
	}
	close(retry)
	waitTelemetryTest(t, sink.startup.done)
	if !sink.isReady() || calls.Load() != 2 || sink.initializationSnapshot()["phase"] != "ready" {
		t.Fatalf("late dependency did not recover: %#v calls=%d", sink.initializationSnapshot(), calls.Load())
	}
}

func TestTelemetryStartupBoundsRetriesAndCancellation(t *testing.T) {
	sink := &telemetrySink{enabled: true}
	var attempts atomic.Int32
	delays := make(chan time.Duration, 12)
	blocked := make(chan struct{})
	sink.initializeWithRecovery(context.Background(), time.Second, func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("attempt is missing deadline")
		}
		attempts.Add(1)
		return errors.New("not ready")
	}, func(ctx context.Context, delay time.Duration) bool {
		delays <- delay
		if attempts.Load() < 8 {
			return true
		}
		close(blocked)
		<-ctx.Done()
		return false
	})
	waitTelemetryTest(t, blocked)
	if err := sink.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(delays)
	expected := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	var observed []time.Duration
	for delay := range delays {
		observed = append(observed, delay)
	}
	if len(observed) != len(expected) {
		t.Fatalf("unexpected retries: %v", observed)
	}
	for i := range expected {
		if observed[i] != expected[i] {
			t.Fatalf("unbounded or incorrect retry schedule: %v", observed)
		}
	}
	if sink.isReady() || sink.initializationSnapshot()["phase"] != "stopped" {
		t.Fatalf("canceled recovery remained ready: %#v", sink.initializationSnapshot())
	}
}

func TestTelemetryStartupCancelsInFlightInitialization(t *testing.T) {
	sink := &telemetrySink{enabled: true}
	entered := make(chan struct{})
	parent, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		sink.initializeWithRecovery(parent, time.Second, func(ctx context.Context) error {
			close(entered)
			<-ctx.Done()
			return nil // A late nil must not publish a canceled initialization.
		}, waitTelemetryRetry)
	}()
	waitTelemetryTest(t, entered)
	cancel()
	waitTelemetryTest(t, finished)
	waitTelemetryTest(t, sink.startup.done)
	if sink.isReady() {
		t.Fatal("canceled initializer published success")
	}
	if err := sink.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryStartupKeepsWaitingSummaryUnknown(t *testing.T) {
	sink := &telemetrySink{enabled: true}
	sink.initializeWithRecovery(context.Background(), time.Second,
		func(context.Context) error { return errors.New("unavailable") },
		func(ctx context.Context, _ time.Duration) bool { <-ctx.Done(); return false })
	t.Cleanup(func() { _ = sink.close(context.Background()) })
	summary, err := sink.summary(context.Background())
	if err != nil || summary["enabled"] != true || summary["ready"] != false {
		t.Fatalf("unexpected waiting summary: %#v err=%v", summary, err)
	}
	for _, key := range []string{"estimatedEventDocs", "estimatedBlobDocs"} {
		if _, present := summary[key]; present {
			t.Fatalf("unknown count was synthesized: %#v", summary)
		}
	}
	if _, err := sink.ingestWrite(context.Background(), normalizedWrite{}, nil); err == nil {
		t.Fatal("waiting sink admitted a write before indexes")
	}
}

func TestTelemetryStartupDisabledAndMalformedConfiguration(t *testing.T) {
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	sink, err := newTelemetrySinkFromEnv()
	if err != nil || sink.isReady() || sink.startup != nil || sink.client != nil {
		t.Fatalf("disabled telemetry attempted initialization: %#v %v", sink, err)
	}
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "true")
	t.Setenv("MONGODB_URI", "not-a-mongo-uri-private-password")
	sink, err = newTelemetrySinkFromEnv()
	if err == nil || sink != nil || strings.Contains(err.Error(), "private-password") {
		t.Fatalf("invalid configuration did not fail privately: sink=%v err=%v", sink, err)
	}
}

func TestTelemetryStartupSuccessAndLegacyFixtures(t *testing.T) {
	var nilSink *telemetrySink
	if nilSink.isReady() || (&telemetrySink{}).isReady() || !(&telemetrySink{enabled: true}).isReady() {
		t.Fatal("legacy readiness semantics changed")
	}
	sink := &telemetrySink{enabled: true}
	parent, cancel := context.WithCancel(context.Background())
	sink.initializeWithRecovery(parent, time.Second, func(context.Context) error { return nil },
		func(context.Context, time.Duration) bool { t.Error("ready sink retried"); return false })
	waitTelemetryTest(t, sink.startup.done)
	if !sink.isReady() {
		t.Fatal("healthy initialization stayed unavailable")
	}
	cancel()
	if sink.isReady() || sink.initializationSnapshot()["phase"] != "stopped" {
		t.Fatal("canceled parent left telemetry ready")
	}
	if err := sink.close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryRecoveryExpiredAttemptNeverBecomesReady(t *testing.T) {
	sink := &telemetrySink{enabled: true}
	sink.initializeWithRecovery(context.Background(), time.Nanosecond, func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}, func(ctx context.Context, _ time.Duration) bool { <-ctx.Done(); return false })
	t.Cleanup(func() { _ = sink.close(context.Background()) })
	if sink.isReady() || sink.initializationSnapshot()["phase"] != "waiting" {
		t.Fatal("expired attempt incorrectly published ready")
	}
}

func TestTelemetryRecoveryCloseWithCanceledCallerDisconnectsClient(t *testing.T) {
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	sink := &telemetrySink{enabled: true, client: client}
	sink.initializeWithRecovery(context.Background(), time.Second,
		func(context.Context) error { return errors.New("offline") }, waitTelemetryRetry)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = sink.close(ctx)
	waitTelemetryTest(t, sink.startup.done)
	if err := client.Ping(context.Background(), nil); !errors.Is(err, mongo.ErrClientDisconnected) {
		t.Fatalf("canceled caller orphaned Mongo client: %v", err)
	}
	if err := sink.close(context.Background()); err != nil {
		t.Fatalf("repeated close failed: %v", err)
	}
}

func TestTelemetryRecoveryWaitingConsumersUseSpool(t *testing.T) {
	t.Setenv("GO_TELEMETRY_SINK_ENABLED", "false")
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_SPOOL_PATH", filepath.Join(t.TempDir(), "spool.ndjson"))
	t.Setenv("GO_RETRIEVAL_STAGED_ENABLED", "false")
	t.Setenv("GO_RUNTIME_STRICT_NO_PYTHON", "true")
	s := newTestServer(t, "http://127.0.0.1:1")
	sink := &telemetrySink{enabled: true} // Nil collections make any premature Mongo call fail.
	sink.initializeWithRecovery(context.Background(), time.Second,
		func(context.Context) error { return errors.New("offline") }, waitTelemetryRetry)
	t.Cleanup(func() { _ = sink.close(context.Background()) })
	s.telemetrySink = sink
	item := normalizedWrite{project: "startup-proof", fileName: "telemetry__waiting.json", content: `{"cpu":0.8}`, topicPath: "telemetry/runtime"}
	result, code, err := s.routeTelemetryWrite(context.Background(), item, "/memory/write")
	if err != nil || code != http.StatusAccepted || result["lane"] != "telemetry_spool_fallback" {
		t.Fatalf("waiting write bypassed spool: %#v code=%d err=%v", result, code, err)
	}
	raw, err := os.ReadFile(os.Getenv("GO_TELEMETRY_SPOOL_PATH"))
	if err != nil || !strings.Contains(string(raw), "startup-proof") {
		t.Fatalf("spooled write missing: %v", err)
	}
	_, _, detail := s.strictRuntimeLaneStatus(sourceMongoRaw)
	if !strings.Contains(detail, "spool") {
		t.Fatalf("waiting sink falsely advertises Mongo: %s", detail)
	}
	transport, _ := retrievalSourceTransport(s, sourceMongoRaw, sourceOwnerGoNative)
	if transport != "in_process" {
		t.Fatalf("waiting fallback has wrong transport: %s", transport)
	}
	w := httptest.NewRecorder()
	s.telemetryBlobGC(w, httptest.NewRequest(http.MethodPost, "/telemetry/blob-gc", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("waiting sink allowed GC: %d %s", w.Code, w.Body.String())
	}
}

func TestTelemetryRecoveryConcurrentReadinessAndClose(t *testing.T) {
	sink := &telemetrySink{enabled: true}
	retry := make(chan struct{})
	var calls atomic.Int32
	sink.initializeWithRecovery(context.Background(), time.Second, func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("offline")
		}
		return nil
	}, func(ctx context.Context, _ time.Duration) bool {
		select {
		case <-retry:
			return true
		case <-ctx.Done():
			return false
		}
	})
	var readers sync.WaitGroup
	for range 8 {
		readers.Go(func() {
			for range 100 {
				_ = sink.initializationSnapshot()
				_ = sink.isReady()
			}
		})
	}
	close(retry)
	for range 4 {
		readers.Go(func() { _ = sink.close(context.Background()) })
	}
	readers.Wait()
	waitTelemetryTest(t, sink.startup.done)
	if sink.isReady() {
		t.Fatal("concurrent close left sink ready")
	}
}

func TestTelemetryRecoveryGatewayActivationAndLateConstructionClose(t *testing.T) {
	for _, scenario := range []string{"activation_panic", "late_constructor"} {
		t.Run(scenario, func(t *testing.T) {
			sink := &telemetrySink{enabled: true}
			sink.initializeWithRecovery(context.Background(), time.Second,
				func(context.Context) error { return errors.New("offline") }, waitTelemetryRetry)
			t.Cleanup(func() { _ = sink.close(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			terminal := startGatewayRuntime(ctx, func(context.Context) *server {
				close(entered)
				if scenario == "late_constructor" {
					<-release
				}
				return &server{telemetrySink: sink}
			}, func(*server) { panic("activation fault") })
			waitTelemetryTest(t, entered)
			if scenario == "late_constructor" {
				cancel()
			}
			select {
			case result := <-terminal:
				if result.err == nil {
					t.Fatal("fault unexpectedly activated")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no terminal result")
			}
			close(release)
			waitTelemetryTest(t, sink.startup.done)
			if sink.isReady() || sink.startup.ctx.Err() == nil {
				t.Fatal("orphaned telemetry runtime")
			}
		})
	}
}

func TestTelemetryMongoFailureSummaryAndFallbackStayPrivate(t *testing.T) {
	client, err := mongo.Connect(options.Client().ApplyURI("mongodb://private-user:private-password@private-host:27017"))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink := &telemetrySink{enabled: true, client: client,
		events: client.Database("fixture").Collection("events"), blobs: client.Database("fixture").Collection("blobs")}
	summary, err := sink.summary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"estimatedEventDocs", "estimatedBlobDocs"} {
		if _, exists := summary[field]; exists {
			t.Fatalf("failed count became known: %#v", summary)
		}
	}
	if summary["eventCountError"] != "mongo_event_count_failed" || summary["blobCountError"] != "mongo_blob_count_failed" {
		t.Fatalf("missing fixed error codes: %#v", summary)
	}
	t.Setenv("GO_TELEMETRY_SPOOL_ENABLED", "true")
	t.Setenv("GO_TELEMETRY_SPOOL_PATH", filepath.Join(t.TempDir(), "spool.ndjson"))
	s := newTestServer(t, "http://127.0.0.1:1")
	s.telemetrySink = sink
	result, status, err := s.routeTelemetryWrite(context.Background(), normalizedWrite{
		project: "privacy", fileName: "telemetry__private.json", content: "{}", topicPath: "telemetry/runtime"}, "/memory/write")
	if err != nil || status != http.StatusAccepted || result["degraded_reason"] != "mongo_telemetry_write_failed" {
		t.Fatalf("unsafe fallback: %#v err=%v", result, err)
	}
	encoded, _ := json.Marshal([]any{summary, result})
	for _, secret := range []string{"private-user", "private-password", "private-host", "mongodb://"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("Mongo failure leaked %s", secret)
		}
	}
}
