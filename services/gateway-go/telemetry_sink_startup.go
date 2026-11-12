package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

type telemetrySinkStartup struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	ready  atomic.Bool
	mu     sync.Mutex
	tries  uint64
	phase  string
	code   string
}

func (s *telemetrySink) isReady() bool {
	return s != nil && s.enabled && (s.startup == nil ||
		(s.startup.ctx.Err() == nil && s.startup.ready.Load()))
}

func (s *telemetrySink) initializationSnapshot() map[string]any {
	result := map[string]any{"configured": s != nil && s.enabled, "ready": s.isReady(), "phase": "disabled"}
	if s == nil || !s.enabled {
		return result
	}
	if s.startup == nil {
		result["phase"] = "ready"
		return result
	}
	s.startup.mu.Lock()
	defer s.startup.mu.Unlock()
	result["ready"] = s.isReady()
	result["phase"] = s.startup.phase
	result["attempts"] = s.startup.tries
	if s.startup.code != "" {
		result["last_error_code"] = s.startup.code
	}
	if s.startup.ctx.Err() != nil {
		result["phase"] = "stopped"
	}
	return result
}

func waitTelemetryRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// The client and configuration remain immutable; only readiness is published.
func (s *telemetrySink) initializeWithRecovery(parent context.Context, timeout time.Duration,
	initialize func(context.Context) error, wait func(context.Context, time.Duration) bool,
) {
	ctx, cancel := context.WithCancel(parent)
	s.startup = &telemetrySinkStartup{ctx: ctx, cancel: cancel, done: make(chan struct{}), phase: "waiting"}
	attempt := func() bool {
		if ctx.Err() != nil {
			return false
		}
		s.startup.mu.Lock()
		s.startup.tries++
		s.startup.mu.Unlock()
		attemptCtx, stop := context.WithTimeout(ctx, timeout)
		err := initialize(attemptCtx)
		if err == nil {
			err = attemptCtx.Err()
		}
		stop()
		s.startup.mu.Lock()
		defer s.startup.mu.Unlock()
		if ctx.Err() != nil {
			s.startup.phase = "stopped"
			return false
		}
		if err != nil {
			s.startup.phase = "waiting"
			s.startup.code = "mongo_initialization_failed"
			return false
		}
		s.startup.phase = "ready"
		s.startup.code = ""
		s.startup.ready.Store(true)
		return true
	}
	if attempt() {
		s.startBlobGCWorker()
		close(s.startup.done)
		return
	}
	go func() {
		defer close(s.startup.done)
		for delay := time.Second; ctx.Err() == nil && wait(ctx, delay); {
			if attempt() {
				s.startBlobGCWorker()
				return
			}
			if delay < 30*time.Second {
				delay *= 2
				if delay > 30*time.Second {
					delay = 30 * time.Second
				}
			}
		}
	}()
}

func (s *telemetrySink) close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	var waitErr error
	if s.startup != nil {
		s.startup.cancel()
		select {
		case <-s.startup.done:
		case <-ctx.Done():
			waitErr = ctx.Err()
		}
	}
	if s.client != nil {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		disconnectErr := s.client.Disconnect(cleanup)
		if errors.Is(disconnectErr, mongo.ErrClientDisconnected) {
			disconnectErr = nil
		}
		return errors.Join(waitErr, disconnectErr)
	}
	return waitErr
}

func (s *server) closeTelemetrySinkRuntime() error {
	if s == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.telemetrySink.close(ctx)
}
