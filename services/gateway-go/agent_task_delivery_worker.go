package main

// The task-delivery worker is the only component allowed to turn staged
// publication and outbox rows into ContextLattice projections.  HTTP callers
// can request a bounded reconciliation pass, but cannot supply a writeback
// receipt or mark a recipient delivered themselves.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

const (
	agentTaskDeliveryMaxAttempts = 8
	agentTaskWorkerClaimTTL      = 30 * time.Second
	agentTaskPublicationWorkerID = "gateway-publication-worker"
	agentTaskWritebackEventType  = "agent.task.writeback"
	agentTaskDeliveryEventType   = "agent.task.delivery"
)

func agentTaskSessionMatches(session map[string]any, project string, principals ...string) bool {
	if session == nil || !strings.EqualFold(strings.TrimSpace(anyToString(session["project"])), strings.TrimSpace(project)) {
		return false
	}
	agentID := strings.TrimSpace(firstNonEmptyStrings(anyToString(session["agent_id"]), anyToString(session["agent"])))
	if agentID == "" {
		return false
	}
	for _, principal := range principals {
		if strings.EqualFold(agentID, strings.TrimSpace(principal)) {
			return true
		}
	}
	return false
}

func agentTaskWorkerClaimExpiry() string {
	ttl := envDurationSeconds("GO_AGENT_TASK_WORKER_CLAIM_TTL_SECS", agentTaskWorkerClaimTTL.Seconds())
	if ttl < time.Second {
		ttl = time.Second
	}
	if ttl > 5*time.Minute {
		ttl = 5 * time.Minute
	}
	return time.Now().UTC().Add(ttl).Format(time.RFC3339Nano)
}

func (l *agentTaskDeliveryLedger) claimDelivery(ctx context.Context, deliveryID, workerID string) (map[string]any, bool, error) {
	if err := agentTaskValidateStructured(map[string]any{"delivery_id": deliveryID, "worker_id": workerID}, "delivery worker claim", agentTaskEventMaxBytes); err != nil {
		return nil, false, err
	}
	deliveryID = strings.TrimSpace(deliveryID)
	workerID = strings.TrimSpace(workerID)
	if deliveryID == "" || workerID == "" {
		return nil, false, errors.New("delivery_id and worker identity are required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var taskID, resultID, status, nextAction, lastError, existingClaimID, existingClaimedBy, existingClaimExpires string
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT task_id,result_id,status,attempts,next_action,last_error,worker_claim_id,worker_claimed_by,worker_claim_expires_at FROM task_ledger_deliveries WHERE delivery_id=?`, deliveryID).Scan(&taskID, &resultID, &status, &attempts, &nextAction, &lastError, &existingClaimID, &existingClaimedBy, &existingClaimExpires); err != nil {
		return nil, false, err
	}
	if status == "delivered" || status == "acknowledged" || status == "dead_letter" {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		row, rowErr := l.deliveryByID(ctx, deliveryID, taskID, resultID)
		return row, false, rowErr
	}
	if status == "delivering" {
		expires, parseErr := time.Parse(time.RFC3339Nano, existingClaimExpires)
		if parseErr == nil && time.Now().UTC().Before(expires) {
			if err := tx.Commit(); err != nil {
				return nil, false, err
			}
			row, rowErr := l.deliveryByID(ctx, deliveryID, taskID, resultID)
			return row, false, rowErr
		}
		status = "failed"
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_deliveries SET status='failed',last_error='expired delivery worker claim',next_action='retry_continuation_inbox',worker_claim_id='',worker_claimed_by='',worker_claim_expires_at='',updated_at=? WHERE delivery_id=?`, agentTaskNow(), deliveryID); err != nil {
			return nil, false, err
		}
	}
	if status != "pending" && status != "failed" {
		return nil, false, fmt.Errorf("delivery is not retryable from %s", status)
	}
	attempts++
	if attempts > agentTaskDeliveryMaxAttempts {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_deliveries SET status='dead_letter',attempts=?,last_error=?,next_action='owner_reconcile_delivery',worker_claim_id='',worker_claimed_by='',worker_claim_expires_at='',updated_at=? WHERE delivery_id=?`, attempts, firstNonEmptyStrings(lastError, "delivery retry budget exhausted"), agentTaskNow(), deliveryID); err != nil {
			return nil, false, err
		}
		if err := l.appendEventTx(ctx, tx, taskID, "", "delivery_dead_letter", "task delivery retry budget exhausted", map[string]any{"delivery_id": deliveryID, "attempts": attempts}); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		row, rowErr := l.deliveryByID(ctx, deliveryID, taskID, resultID)
		return row, false, rowErr
	}
	claimID, err := l.newUniqueID(ctx, tx, "delivery-claim", "delivery-claim")
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_deliveries SET status='delivering',attempts=?,last_error='',next_action='project_continuation_inbox',worker_claim_id=?,worker_claimed_by=?,worker_claim_expires_at=?,updated_at=? WHERE delivery_id=? AND status IN ('pending','failed')`, attempts, claimID, workerID, agentTaskWorkerClaimExpiry(), agentTaskNow(), deliveryID); err != nil {
		return nil, false, err
	}
	if err := l.appendEventTx(ctx, tx, taskID, "", "delivery_attempted", "gateway delivery worker claimed an outbox row", map[string]any{"delivery_id": deliveryID, "attempts": attempts}); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	row, rowErr := l.deliveryByID(ctx, deliveryID, taskID, resultID)
	if row != nil {
		row["worker_claim_id"] = claimID
		row["worker_claimed_by"] = workerID
	}
	return row, true, rowErr
}

func (l *agentTaskDeliveryLedger) deliveryByID(ctx context.Context, deliveryID, taskID, resultID string) (map[string]any, error) {
	rows, err := l.deliveries(ctx, taskID, resultID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if strings.EqualFold(anyToString(row["delivery_id"]), deliveryID) {
			return row, nil
		}
	}
	return nil, errors.New("delivery outbox row not found")
}

func (l *agentTaskDeliveryLedger) claimPublication(ctx context.Context, publicationID, workerID string) (map[string]any, bool, error) {
	if err := agentTaskValidateStructured(map[string]any{"publication_id": publicationID, "worker_id": workerID}, "publication worker claim", agentTaskEventMaxBytes); err != nil {
		return nil, false, err
	}
	publicationID = strings.TrimSpace(publicationID)
	workerID = strings.TrimSpace(workerID)
	if publicationID == "" || workerID == "" {
		return nil, false, errors.New("publication_id and worker identity are required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var taskID, attemptID, status, claimID, claimedBy, claimExpires, lastError string
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT task_id,attempt_id,status,worker_claim_id,worker_claimed_by,worker_claim_expires_at,last_error,worker_attempts FROM task_ledger_publications WHERE publication_id=?`, publicationID).Scan(&taskID, &attemptID, &status, &claimID, &claimedBy, &claimExpires, &lastError, &attempts); err != nil {
		return nil, false, err
	}
	if status == "committed" {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		publication, err := l.publication(ctx, publicationID)
		return publication, false, err
	}
	if status != "writeback_pending" && status != "writeback_failed" {
		return nil, false, fmt.Errorf("publication is not retryable from %s", status)
	}
	if expires, parseErr := time.Parse(time.RFC3339Nano, claimExpires); claimID != "" && parseErr == nil && time.Now().UTC().Before(expires) {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		publication, err := l.publication(ctx, publicationID)
		return publication, false, err
	}
	if attempts >= agentTaskDeliveryMaxAttempts {
		reason := firstNonEmptyStrings(lastError, "writeback retry budget exhausted")
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_publications SET status='dead_letter',writeback_status='dead_letter',last_error=?,next_action='owner_reconcile_writeback',worker_claim_id='',worker_claimed_by='',worker_claim_expires_at='',updated_at=? WHERE publication_id=?`, reason, agentTaskNow(), publicationID); err != nil {
			return nil, false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='dead_letter',updated_at=? WHERE id=? AND status IN ('writeback_pending','writeback_failed')`, agentTaskNow(), taskID); err != nil {
			return nil, false, err
		}
		if err := l.appendEventTx(ctx, tx, taskID, attemptID, "dead_letter", "task publication retry budget exhausted", map[string]any{"publication_id": publicationID, "attempts": attempts, "reason": reason}); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		publication, err := l.publication(ctx, publicationID)
		return publication, false, err
	}
	attempts++
	claimID, err = l.newUniqueID(ctx, tx, "publication-claim", "publication-claim")
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_publications SET worker_attempts=?,worker_claim_id=?,worker_claimed_by=?,worker_claim_expires_at=?,last_error='',next_action='commit_writeback',updated_at=? WHERE publication_id=? AND status IN ('writeback_pending','writeback_failed')`, attempts, claimID, workerID, agentTaskWorkerClaimExpiry(), agentTaskNow(), publicationID); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	publication, err := l.publication(ctx, publicationID)
	if publication != nil {
		publication["worker_claim_id"] = claimID
		publication["worker_claimed_by"] = workerID
	}
	return publication, true, err
}

func (l *agentTaskDeliveryLedger) pendingPublicationIDs(ctx context.Context, limit int) ([]string, error) {
	limit = clampInt(limit, 1, 100)
	rows, err := l.db.QueryContext(ctx, `SELECT publication_id FROM task_ledger_publications WHERE status IN ('writeback_pending','writeback_failed') AND (worker_claim_id='' OR worker_claim_expires_at='' OR worker_claim_expires_at<=?) ORDER BY updated_at ASC LIMIT ?`, agentTaskNow(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (l *agentTaskDeliveryLedger) pendingDeliveryIDs(ctx context.Context, limit int) ([]string, error) {
	limit = clampInt(limit, 1, 200)
	rows, err := l.db.QueryContext(ctx, `SELECT delivery_id FROM task_ledger_deliveries WHERE (status IN ('pending','failed') OR (status='delivering' AND worker_claim_expires_at<>'' AND worker_claim_expires_at<=?)) ORDER BY updated_at ASC LIMIT ?`, agentTaskNow(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (l *agentTaskDeliveryLedger) finishDelivery(ctx context.Context, deliveryID, claimID, workerID, status, reason string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"delivery_id": deliveryID, "claim_id": claimID, "worker_id": workerID, "status": status, "reason": reason}, "delivery worker receipt", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	deliveryID = strings.TrimSpace(deliveryID)
	claimID = strings.TrimSpace(claimID)
	workerID = strings.TrimSpace(workerID)
	status = strings.TrimSpace(strings.ToLower(status))
	if status != "delivered" && status != "failed" && status != "dead_letter" {
		return nil, errors.New("delivery worker outcome is invalid")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taskID, resultID, currentStatus, currentClaimID, currentClaimedBy, claimExpires string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,result_id,status,worker_claim_id,worker_claimed_by,worker_claim_expires_at FROM task_ledger_deliveries WHERE delivery_id=?`, deliveryID).Scan(&taskID, &resultID, &currentStatus, &currentClaimID, &currentClaimedBy, &claimExpires); err != nil {
		return nil, err
	}
	if currentStatus == "acknowledged" || currentStatus == "dead_letter" || (currentStatus == "delivered" && status == "delivered") {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return l.deliveryByID(ctx, deliveryID, taskID, resultID)
	}
	if claimID == "" || workerID == "" || currentClaimID != claimID || currentClaimedBy != workerID {
		return nil, errors.New("stale delivery worker claim")
	}
	if expires, parseErr := time.Parse(time.RFC3339Nano, claimExpires); parseErr != nil || !time.Now().UTC().Before(expires) {
		return nil, errors.New("expired delivery worker claim")
	}
	nextAction := "acknowledge_review"
	if status == "failed" {
		nextAction = "retry_continuation_inbox"
	}
	if status == "dead_letter" {
		nextAction = "owner_reconcile_delivery"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_deliveries SET status=?,last_error=?,next_action=?,worker_claim_id='',worker_claimed_by='',worker_claim_expires_at='',updated_at=? WHERE delivery_id=? AND status='delivering' AND worker_claim_id=? AND worker_claimed_by=?`, status, strings.TrimSpace(reason), nextAction, agentTaskNow(), deliveryID, claimID, workerID); err != nil {
		return nil, err
	}
	if err := l.appendEventTx(ctx, tx, taskID, "", "delivery_"+status, "gateway delivery worker recorded the projection outcome", map[string]any{"delivery_id": deliveryID, "reason": strings.TrimSpace(reason)}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.deliveryByID(ctx, deliveryID, taskID, resultID)
}

func (s *server) projectTaskDeliveryDurably(ctx context.Context, delivery map[string]any) bool {
	if s == nil || delivery == nil {
		return false
	}
	// The SQLite outbox remains authoritative.  A session event is only a
	// projection receipt and a nil result means the recipient is offline or the
	// scoped session is unavailable; the row stays retryable in SQLite.
	sessionID := strings.TrimSpace(anyToString(delivery["session_id"]))
	if sessionID == "" {
		recipient := anyMap(delivery["recipient"])
		sessionID = strings.TrimSpace(anyToString(recipient["session_id"]))
	}
	if sessionID == "" {
		return false
	}
	if s.taskDeliveryProjectionFault != nil {
		if err := s.taskDeliveryProjectionFault(); err != nil {
			return false
		}
	}
	if s.agentSessions == nil {
		return false
	}
	if session, events, exists := s.agentSessions.get(sessionID); exists {
		recipient := anyMap(delivery["recipient"])
		project := firstNonEmptyStrings(anyToString(recipient["project"]), anyToString(delivery["project"]))
		if !agentTaskSessionMatches(session, project, anyToString(recipient["principal_id"])) {
			return false
		}
		for _, event := range events {
			metadata := anyMap(event["metadata"])
			priorDelivery := anyMap(metadata["task_delivery"])
			if strings.EqualFold(anyToString(priorDelivery["dedupe_key"]), anyToString(delivery["dedupe_key"])) && strings.EqualFold(anyToString(event["type"]), agentTaskDeliveryEventType) {
				return true
			}
		}
	} else {
		return false
	}
	payload := map[string]any{
		"schema_id": agentTaskDeliveryContractID, "contract_version": 1,
		"task_id": anyToString(delivery["task_id"]), "result_id": anyToString(delivery["result_id"]),
		"delivery_id": anyToString(delivery["delivery_id"]), "publication_id": anyToString(delivery["publication_id"]),
		"recipient": anyMap(delivery["recipient"]), "reviewer_owner": anyToString(delivery["reviewer_owner"]),
		"status": "delivered", "dedupe_key": anyToString(delivery["dedupe_key"]),
		"summary": anyToString(delivery["summary"]), "required_action": "review",
		"attempts": anyToInt(delivery["attempts"], 0), "next_action": "acknowledge_review",
		"session_id": sessionID,
	}
	payload = agentTaskContractPayload(agentTaskDeliveryContractID, payload)
	message, _ := agentTaskBoundedString(firstNonEmptyStrings(anyToString(delivery["summary"]), "Task result is ready for review."), agentTaskNotificationMaxBytes)
	steering := attachPayloadFormatContract(steeringCommentContractID, map[string]any{
		"ok": true, "schema_id": steeringCommentContractID, "audience": "requesting_agent",
		"severity": "info", "message": message, "suggested_action": "Drain the task delivery and review the immutable result.",
		"reason": "durable task-delivery outbox projection", "project": anyToString(delivery["project"]),
		"session_id": sessionID, "agent_id": anyToString(anyMap(delivery["recipient"])["principal_id"]),
		"source": "agent_task_delivery", "trigger_event": agentTaskDeliveryEventType, "trigger_status": "delivered",
		"returned_sources": []any{}, "pending_sources": []any{}, "failed_sources": []any{},
		"retrieval_progress": map[string]any{"status": "completed", "result_state": "task_delivery", "modeled_progress": map[string]any{"progress_pct": 100}},
		"delivery":           map[string]any{"session_event_type": agentTaskDeliveryEventType, "delivery_id": anyToString(delivery["delivery_id"]), "dedupe_key": anyToString(delivery["dedupe_key"])},
	}, anyToString(anyMap(delivery["recipient"])["principal_id"]), "agent_task_delivery", "/v1/agents/sessions/{session_id}/events")
	projection := cloneAnyMap(payload)
	payload["metadata"] = map[string]any{"agent_visible": true, "steering_comment": steering, "task_delivery": projection}
	if err := agentTaskValidateStructured(payload, "task delivery session projection", agentTaskContextPackMaxBytes); err != nil {
		return false
	}
	return s.recordAgentSessionEvent(sessionID, agentTaskDeliveryEventType, payload) != nil
}

func (s *server) commitTaskMemoryWrite(ctx context.Context, intent map[string]any) (map[string]any, error) {
	if err := agentTaskValidateStructured(intent, "task memory writeback intent", agentTaskEventMaxBytes*4); err != nil {
		return nil, err
	}
	if s == nil || s.memoryStore == nil || !s.memoryStore.isEnabled() {
		return nil, errors.New("authoritative Gateway memory store is unavailable")
	}
	project := strings.TrimSpace(anyToString(intent["project"]))
	fileName := strings.TrimSpace(anyToString(intent["file_name"]))
	topicPath := strings.TrimSpace(anyToString(intent["topic_path"]))
	content := strings.TrimSpace(anyToString(intent["summary"]))
	if project == "" || fileName == "" || topicPath == "" || content == "" {
		return nil, errors.New("bounded task writeback intent is incomplete")
	}
	write := normalizedWrite{
		project: project, fileName: fileName, content: content, topicPath: topicPath,
		agentID: firstNonEmptyStrings(anyToString(intent["worker_id"]), "gateway-go"), sessionID: strings.TrimSpace(anyToString(intent["session_id"])),
		tags:      []string{"task-delivery", "task:" + strings.TrimSpace(anyToString(intent["task_id"]))},
		lifecycle: "durable", storageTier: "hot", idempotencyKey: strings.TrimSpace(anyToString(intent["idempotency_key"])),
		raw: map[string]any{
			"task_id": anyToString(intent["task_id"]), "attempt_id": anyToString(intent["attempt_id"]), "result_id": anyToString(intent["result_id"]),
			"worker_id": anyToString(intent["worker_id"]), "worker_instance_id": anyToString(intent["worker_instance_id"]),
			"assignment_generation": anyToInt(intent["assignment_generation"], 0), "lease_generation": anyToInt(intent["lease_generation"], 0),
			"requesting_agent_id": anyToString(intent["requesting_agent_id"]), "review_agent_id": anyToString(intent["review_agent_id"]),
			"authoritative_writer": "gateway-go",
		},
		taskAttribution: map[string]any{
			"task_id": anyToString(intent["task_id"]), "attempt_id": anyToString(intent["attempt_id"]), "result_id": anyToString(intent["result_id"]),
			"worker_id": anyToString(intent["worker_id"]), "worker_instance_id": anyToString(intent["worker_instance_id"]),
			"assignment_generation": anyToInt(intent["assignment_generation"], 0), "lease_generation": anyToInt(intent["lease_generation"], 0),
			"requesting_agent_id": anyToString(intent["requesting_agent_id"]), "review_agent_id": anyToString(intent["review_agent_id"]),
			"authoritative_writer": "gateway-go",
		},
	}
	if err := s.writePolicy.validateWrite(write); err != nil {
		return nil, err
	}
	filtered, _, err := secureNormalizedWrite(write)
	if err != nil {
		return nil, errors.New("task writeback content failed the Gateway secret boundary")
	}
	filtered = s.classifyWrite(filtered)
	if s.taskMemoryWriteFault != nil {
		if err := s.taskMemoryWriteFault("before"); err != nil {
			return nil, err
		}
	}
	entry, deduped, err := s.memoryStore.put(filtered)
	if err != nil {
		return nil, fmt.Errorf("commit task memory writeback: %w", err)
	}
	if s.taskMemoryWriteFault != nil {
		if err := s.taskMemoryWriteFault("after"); err != nil {
			return nil, err
		}
	}
	receipt := map[string]any{
		"memory_id": project + "::" + fileName, "event_id": entry.EventID, "content_hash": entry.ContentHash,
		"content_ref": entry.ContentRef, "object_id": entry.ObjectID, "idempotency_key": filtered.idempotencyKey,
		"deduped": deduped, "authority": "gateway-go-memory-store",
		"worker_id": anyToString(intent["worker_id"]), "worker_instance_id": anyToString(intent["worker_instance_id"]),
		"assignment_generation": anyToInt(intent["assignment_generation"], 0), "lease_generation": anyToInt(intent["lease_generation"], 0),
		"requesting_agent_id": anyToString(intent["requesting_agent_id"]), "review_agent_id": anyToString(intent["review_agent_id"]),
	}
	if agentTaskDigest(receipt) == "" {
		return nil, errors.New("task memory writeback receipt is not canonically serializable")
	}
	return receipt, nil
}

func (s *server) runTaskDeliveryOutbox(ctx context.Context, deliveryID string) (map[string]any, error) {
	if s == nil || s.taskLedger == nil {
		return nil, errors.New("authoritative task ledger unavailable")
	}
	row, claimed, err := s.taskLedger.claimDelivery(ctx, deliveryID, agentTaskPublicationWorkerID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return row, nil
	}
	if governanceErr := s.authorizeTaskResource(ctx, anyToString(row["task_id"]), agentTaskRouteAuth{Principal: "gateway-service", Service: true}); governanceErr != nil {
		return s.taskLedger.finishDelivery(ctx, deliveryID, anyToString(row["worker_claim_id"]), agentTaskPublicationWorkerID, "failed", "task workspace governance is no longer active")
	}
	if s.taskDeliveryFault != nil {
		if err := s.taskDeliveryFault("after_claim"); err != nil {
			return nil, err
		}
	}
	claimID := anyToString(row["worker_claim_id"])
	if s.taskDeliveryFault != nil {
		if err := s.taskDeliveryFault("before_projection"); err != nil {
			return nil, err
		}
	}
	if s.projectTaskDeliveryDurably(ctx, row) {
		if s.taskDeliveryFault != nil {
			if err := s.taskDeliveryFault("after_projection"); err != nil {
				return nil, err
			}
		}
		delivery, finishErr := s.taskLedger.finishDelivery(ctx, deliveryID, claimID, agentTaskPublicationWorkerID, "delivered", "")
		if finishErr == nil && s.taskDeliveryFault != nil {
			if err := s.taskDeliveryFault("after_finish"); err != nil {
				return nil, err
			}
		}
		return delivery, finishErr
	}
	return s.taskLedger.finishDelivery(ctx, deliveryID, claimID, agentTaskPublicationWorkerID, "failed", "recipient session is unavailable")
}

func (s *server) runTaskPublicationWorker(ctx context.Context, publicationID string) (map[string]any, error) {
	if s == nil || s.taskLedger == nil {
		return nil, errors.New("authoritative task ledger unavailable")
	}
	publicationID = strings.TrimSpace(publicationID)
	publication, claimed, err := s.taskLedger.claimPublication(ctx, publicationID, agentTaskPublicationWorkerID)
	if err != nil {
		return nil, err
	}
	if !claimed {
		return publication, nil
	}
	if governanceErr := s.authorizeTaskResource(ctx, anyToString(publication["task_id"]), agentTaskRouteAuth{Principal: "gateway-service", Service: true}); governanceErr != nil {
		return s.taskLedger.finalizePublicationClaim(ctx, publicationID, anyToString(publication["worker_claim_id"]), "failed", "", "task workspace governance is no longer active")
	}
	if s.taskPublicationFault != nil {
		if err := s.taskPublicationFault("after_claim"); err != nil {
			return nil, err
		}
	}
	claimID := anyToString(publication["worker_claim_id"])
	intent := anyMap(publication["writeback_intent"])
	if !anyToBool(intent["required"]) {
		return nil, errors.New("publication writeback intent is missing or not required")
	}
	// Writeback is executed by the Gateway against the existing scoped session
	// event surface.  The caller cannot choose a status or receipt reference.
	sessionID := strings.TrimSpace(anyToString(intent["session_id"]))
	if sessionID == "" {
		return s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "failed", "", "required ContextLattice session is unavailable")
	}
	if s.agentSessions == nil {
		return s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "failed", "", "required ContextLattice session is unavailable")
	}
	session, _, exists := s.agentSessions.get(sessionID)
	if !exists || !agentTaskSessionMatches(session, anyToString(intent["project"]), anyToString(intent["requesting_agent_id"]), anyToString(intent["review_agent_id"])) {
		return s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "failed", "", "required ContextLattice session is outside the task project or principal scope")
	}
	if s.taskPublicationFault != nil {
		if err := s.taskPublicationFault("before_memory_put"); err != nil {
			return nil, err
		}
	}
	memoryReceipt, memoryErr := s.commitTaskMemoryWrite(ctx, intent)
	if memoryErr != nil {
		return s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "failed", "", memoryErr.Error())
	}
	if s.taskPublicationFault != nil {
		if err := s.taskPublicationFault("after_memory_put"); err != nil {
			return nil, err
		}
	}
	writebackPayload := map[string]any{
		"schema_id": agentTaskWritebackIntentContractID, "contract_version": 1,
		"task_id": anyToString(intent["task_id"]), "attempt_id": anyToString(intent["attempt_id"]),
		"result_id": anyToString(intent["result_id"]), "project": anyToString(intent["project"]),
		"session_id": sessionID, "topic_path": anyToString(intent["topic_path"]), "required": true,
		"summary": anyToString(intent["summary"]), "result_digest": anyToString(intent["result_digest"]),
		"worker_id": anyToString(intent["worker_id"]), "worker_instance_id": anyToString(intent["worker_instance_id"]),
		"assignment_generation": anyToInt(intent["assignment_generation"], 0), "lease_generation": anyToInt(intent["lease_generation"], 0),
		"requesting_agent_id": anyToString(intent["requesting_agent_id"]), "review_agent_id": anyToString(intent["review_agent_id"]),
		"memory_receipt":  memoryReceipt,
		"idempotency_key": anyToString(intent["idempotency_key"]),
	}
	writebackPayload = agentTaskContractPayload(agentTaskWritebackIntentContractID, writebackPayload)
	writebackRecorded := false
	if s.agentSessions != nil {
		if _, events, exists := s.agentSessions.get(sessionID); exists {
			for _, event := range events {
				if strings.EqualFold(anyToString(event["type"]), agentTaskWritebackEventType) && strings.EqualFold(anyToString(anyMap(event["memory_receipt"])["idempotency_key"]), anyToString(memoryReceipt["idempotency_key"])) {
					writebackRecorded = true
					break
				}
				priorWriteback := anyMap(anyMap(event["metadata"])["writeback"])
				if strings.EqualFold(anyToString(event["type"]), agentTaskWritebackEventType) && (strings.EqualFold(anyToString(priorWriteback["idempotency_key"]), anyToString(memoryReceipt["idempotency_key"])) || strings.EqualFold(anyToString(anyMap(priorWriteback["memory_receipt"])["idempotency_key"]), anyToString(memoryReceipt["idempotency_key"]))) {
					writebackRecorded = true
					break
				}
			}
		}
	}
	writebackProjection := cloneAnyMap(writebackPayload)
	writebackPayload["metadata"] = map[string]any{"writeback": writebackProjection}
	if err := agentTaskValidateStructured(writebackPayload, "task writeback session projection", agentTaskContextPackMaxBytes); err != nil {
		return s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "failed", "", err.Error())
	}
	if !writebackRecorded && s.taskSessionEventFault != nil {
		if err := s.taskSessionEventFault(agentTaskWritebackEventType); err != nil {
			return s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "failed", "", err.Error())
		}
	}
	if !writebackRecorded && s.recordAgentSessionEvent(sessionID, agentTaskWritebackEventType, writebackPayload) == nil {
		return s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "failed", "", "required ContextLattice writeback did not commit")
	}
	if s.taskPublicationFault != nil {
		if err := s.taskPublicationFault("after_session_event"); err != nil {
			return nil, err
		}
	}
	receipt := agentTaskDigest(map[string]any{"publication_id": publicationID, "session_id": sessionID, "result_digest": anyToString(intent["result_digest"]), "memory_receipt": memoryReceipt, "event_type": agentTaskWritebackEventType})
	if receipt == "" {
		return nil, errors.New("Gateway writeback receipt is not canonically serializable")
	}
	if s.taskPublicationFault != nil {
		if err := s.taskPublicationFault("before_finalize"); err != nil {
			return nil, err
		}
	}
	publication, err = s.taskLedger.finalizePublicationClaim(ctx, publicationID, claimID, "committed", receipt, "")
	if err != nil {
		return nil, err
	}
	if s.taskPublicationFault != nil {
		if err := s.taskPublicationFault("after_finalize"); err != nil {
			return nil, err
		}
	}
	for _, raw := range anySlice(publication["deliveries"]) {
		delivery := anyMap(raw)
		if strings.TrimSpace(anyToString(delivery["status"])) != "pending" && strings.TrimSpace(anyToString(delivery["status"])) != "failed" {
			continue
		}
		_, _ = s.runTaskDeliveryOutbox(ctx, anyToString(delivery["delivery_id"]))
	}
	return s.taskLedger.publication(ctx, publicationID)
}

func (s *server) reconcileTaskDeliveryOnce(ctx context.Context) {
	if s == nil || s.taskLedger == nil {
		return
	}
	if _, err := s.taskLedger.recoverExpired(ctx, 200); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("gateway-go task lease recovery failed: %v", err)
	}
	publicationIDs, err := s.taskLedger.pendingPublicationIDs(ctx, 32)
	if err == nil {
		for _, publicationID := range publicationIDs {
			if _, workerErr := s.runTaskPublicationWorker(ctx, publicationID); workerErr != nil && !errors.Is(workerErr, context.Canceled) {
				log.Printf("gateway-go task publication recovery failed publication=%s: %v", publicationID, workerErr)
			}
		}
	} else if !errors.Is(err, context.Canceled) {
		log.Printf("gateway-go task publication scan failed: %v", err)
	}
	deliveryIDs, err := s.taskLedger.pendingDeliveryIDs(ctx, 128)
	if err == nil {
		for _, deliveryID := range deliveryIDs {
			if _, workerErr := s.runTaskDeliveryOutbox(ctx, deliveryID); workerErr != nil && !errors.Is(workerErr, context.Canceled) {
				log.Printf("gateway-go task delivery recovery failed delivery=%s: %v", deliveryID, workerErr)
			}
		}
	} else if !errors.Is(err, context.Canceled) {
		log.Printf("gateway-go task delivery scan failed: %v", err)
	}
}
