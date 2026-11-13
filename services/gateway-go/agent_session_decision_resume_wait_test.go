package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

type agentDecisionFenceWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *agentDecisionFenceWaitContext) Done() <-chan struct{} {
	// The actual owner-file lock calls Done only after a nonblocking lock
	// attempt reports contention. No timing guess or production test hook.
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestAgentDecisionResumeMutationWhileFinalOwnerFenceWaits(t *testing.T) {
	for _, mode := range []string{"checkpoint", "completion", "acknowledgement", "durable_hold"} {
		for _, compact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s_compact_%t", mode, compact), func(t *testing.T) {
				s, owner, id := agentDecisionResumeFixture(t)
				row, events, _ := s.agentSessions.get(id)
				alternate := alternateAgentDecisionBinding(t, s.continuity, row)
				now := time.Now().UTC()
				observation := s.observeAgentDecisionResume(context.Background(), row, now)
				payload := observation.render(row, events, now, compact)
				fence, err := owner.acquireMemoryEdgeLogFence()
				if err != nil {
					t.Fatal(err)
				}
				defer fence.release()
				deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				ctx := &agentDecisionFenceWaitContext{Context: deadline, waiting: make(chan struct{})}
				type result struct {
					payload map[string]any
					err     error
				}
				finished := make(chan result, 1)
				go func() {
					p, err := observation.finish(ctx, s, row, events, now, compact, payload)
					finished <- result{p, err}
				}()
				select {
				case <-ctx.waiting:
				case <-deadline.Done():
					t.Fatal("finish never waited on the real owner fence")
				}
				if mode == "acknowledgement" {
					ack := agentDecisionAckPayload(t, row, alternate, "evt_waiting_ack")
					_, _, err = s.agentSessions.acknowledgeDecisionScoped(id, ack, nil, s.continuity.resolveAgentDecisionBinding)
				} else {
					eventType := "checkpoint.written"
					if mode == "completion" {
						eventType = "session.completed"
					}
					if mode == "durable_hold" {
						s.agentSessions.decisionWriteHook = func(phase string) error {
							if phase == "after_replace" {
								return errors.New("committed hold during final owner wait")
							}
							return nil
						}
					}
					_, _, err = s.agentSessions.appendEvent(id, map[string]any{"type": eventType, "metadata": map[string]any{"next_action": "NESTED_STALE_ACTION"}})
				}
				if mode == "durable_hold" {
					if !ownerOnlyAtomicWriteCommitted(err) || !s.agentSessions.decisionUncertain {
						t.Fatal("hold fixture did not establish real post-replacement uncertainty", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				fence.release()
				select {
				case got := <-finished:
					if got.err != nil || agentDecisionResumeProjection(got.payload, compact)["status"] != "unknown" {
						t.Fatal("mutation during owner wait escaped the final session fence", got.err)
					}
					assertAgentDecisionResumeNoAction(t, got.payload, compact)
				case <-deadline.Done():
					t.Fatal("coordinated final fence did not finish")
				}
			})
		}
	}
}
