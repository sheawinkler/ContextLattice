package main

import (
	"context"
	"time"
)

const (
	defaultAgentTaskRecoveryPoll = 5 * time.Second
	minAgentTaskRecoveryPoll     = 250 * time.Millisecond
	maxAgentTaskRecoveryPoll     = time.Minute
)

func agentTaskRecoveryPollInterval() time.Duration {
	interval := envDurationSeconds("GO_AGENT_TASK_RECOVERY_POLL_SECS", defaultAgentTaskRecoveryPoll.Seconds())
	if interval < minAgentTaskRecoveryPoll {
		return minAgentTaskRecoveryPoll
	}
	if interval > maxAgentTaskRecoveryPoll {
		return maxAgentTaskRecoveryPoll
	}
	return interval
}

// startTaskDeliveryRecoveryWorker owns the restart-safe reconciliation loop.
// The owned goroutine performs one bounded pass immediately and then retries
// on its configured cadence. It only claims durable rows whose worker claims
// are absent or expired; the SQLite claim is the cross-process fence.
func (s *server) startTaskDeliveryRecoveryWorker(parents ...context.Context) {
	if s == nil || s.taskLedger == nil {
		return
	}
	s.taskRecoveryMu.Lock()
	if s.taskRecoveryClosed || s.taskRecoveryCancel != nil {
		s.taskRecoveryMu.Unlock()
		return
	}
	parent := context.Background()
	if len(parents) > 0 && parents[0] != nil {
		parent = parents[0]
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.taskRecoveryCancel = cancel
	s.taskRecoveryDone = done
	s.taskRecoveryMu.Unlock()

	go func() {
		defer close(done)
		s.reconcileTaskDeliveryOnce(ctx)
		ticker := time.NewTicker(agentTaskRecoveryPollInterval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.reconcileTaskDeliveryOnce(ctx)
			}
		}
	}()
}

func (s *server) stopTaskDeliveryRecoveryWorker() {
	if s == nil {
		return
	}
	s.taskRecoveryMu.Lock()
	cancel := s.taskRecoveryCancel
	done := s.taskRecoveryDone
	s.taskRecoveryCancel = nil
	s.taskRecoveryDone = nil
	s.taskRecoveryMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (s *server) closeTaskDeliveryRuntime() error {
	if s == nil {
		return nil
	}
	s.taskRuntimeCloseOnce.Do(func() {
		s.taskRecoveryMu.Lock()
		s.taskRecoveryClosed = true
		s.taskRecoveryMu.Unlock()
		s.stopTaskDeliveryRecoveryWorker()
		if s.taskLedger != nil {
			s.taskRuntimeCloseErr = s.taskLedger.close()
		}
	})
	return s.taskRuntimeCloseErr
}
