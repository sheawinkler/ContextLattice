package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultAgentTaskLedgerRelPath   = ".data/orchestrator/agent_tasks.sqlite3"
	defaultAgentTaskArtifactRelPath = ".data/orchestrator/agent_task_artifacts"
	agentTaskLedgerSchemaVersion    = 10
	// Schema v2 materialized claim eligibility. Only v0/v1 ledgers need the
	// legacy metadata projection; replaying it on a later upgrade can overwrite
	// a server-authoritative canonical worker rebind or recovery hold.
	agentTaskClaimEligibilityProjectionSchemaVersion = 2
	agentTaskArtifactNamespaceMarker                 = ".contextlattice-agent-task-artifacts-v1"
	agentTaskArtifactNamespaceLock                   = ".contextlattice-agent-task-artifacts.lock"
	agentTaskLegacyMigrationMaxBytes                 = 32 * 1024 * 1024
	agentTaskLegacyMigrationMaxRows                  = 10000
	agentTaskDefaultExecutionRetries                 = 3
	agentTaskMaxExecutionRetries                     = 10
)

type agentTaskDeliveryLedger struct {
	db                    *sql.DB
	path                  string
	artifactRoot          string
	leaseTTL              time.Duration
	limits                agentTaskDeliveryLimits
	maxExecutionAttempts  int
	idGenerator           func(string) (string, error)
	artifactStageHook     func(string) error
	artifactReconcileHook func(string)
	executionRetryHook    func(string) error
}

var agentTaskArtifactNamespaceInit = make(chan struct{}, 1)

type agentTaskFileOpenMode uint8

const (
	agentTaskFileReadOnly agentTaskFileOpenMode = iota
	agentTaskFileReadWrite
	agentTaskFileReadWriteCreate
	agentTaskFileReadWriteCreateExclusive
)

type agentTaskFence struct {
	TaskID                         string
	AttemptID                      string
	LeaseID                        string
	WorkerID                       string
	WorkerInstanceID               string
	Generation                     int
	WorkerIdentityUpdateGeneration int
}

func fencePayload(fence agentTaskFence) map[string]any {
	return map[string]any{
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID, "generation": fence.Generation,
		"worker_identity_update_generation": fence.WorkerIdentityUpdateGeneration,
	}
}

func agentTaskLedgerPath() string {
	for _, key := range []string{"GO_AGENT_TASK_LEDGER_PATH", "AGENT_TASK_LEDGER_PATH"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return filepath.Clean(value)
		}
	}
	return resolveStoragePath("", defaultAgentTaskLedgerRelPath)
}

func agentTaskArtifactRoot() string {
	for _, key := range []string{"GO_AGENT_TASK_ARTIFACT_DIR", "GO_AGENT_ARTIFACT_STORE_DIR"} {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return filepath.Clean(value)
		}
	}
	// Keep task content in a dedicated namespace.  The memory blob root is an
	// unrelated store and must never be reconciled as if it were task-owned.
	if value := strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_CONTENT_BLOBS_PATH")); value != "" {
		return filepath.Join(filepath.Clean(value), "agent-task-delivery-v1")
	}
	return resolveStoragePath("", defaultAgentTaskArtifactRelPath)
}

func prepareAgentTaskArtifactNamespace(ctx context.Context, root string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case agentTaskArtifactNamespaceInit <- struct{}{}:
		defer func() { <-agentTaskArtifactNamespaceInit }()
	case <-ctx.Done():
		return ctx.Err()
	}
	rootFile, err := openAgentTaskDirectoryNoFollow(root)
	if err != nil {
		return fmt.Errorf("open task artifact namespace: %w", err)
	}
	defer rootFile.Close()
	var lockFile *os.File
	for attempt := 0; attempt < agentTaskIDCollisionRetries; attempt++ {
		lockFile, err = openAgentTaskFileAt(rootFile, agentTaskArtifactNamespaceLock, agentTaskFileReadWriteCreate, 0o600, 4096)
		if !errors.Is(err, os.ErrNotExist) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("open task artifact namespace lock: %w", err)
	}
	defer lockFile.Close()
	if err := agentTaskFlockContext(ctx, lockFile, true); err != nil {
		return fmt.Errorf("lock task artifact namespace initialization: %w", err)
	}
	defer agentTaskUnlock(lockFile)
	marker, markerErr := openAgentTaskFileAt(rootFile, agentTaskArtifactNamespaceMarker, agentTaskFileReadOnly, 0, 4096)
	if errors.Is(markerErr, os.ErrNotExist) {
		// Inspect the directory through the descriptor that passed the owner and
		// no-follow checks. Reopening the pathname here would reintroduce a swap
		// window between namespace validation and marker creation.
		entries, readErr := rootFile.ReadDir(-1)
		if readErr != nil {
			return fmt.Errorf("inspect task artifact namespace: %w", readErr)
		}
		for _, entry := range entries {
			if entry.Name() != agentTaskArtifactNamespaceLock {
				return errors.New("task artifact namespace is non-empty without its ownership marker")
			}
		}
		marker, markerErr = openAgentTaskFileAt(rootFile, agentTaskArtifactNamespaceMarker, agentTaskFileReadWriteCreateExclusive, 0o600, 4096)
		if markerErr != nil {
			return fmt.Errorf("create task artifact namespace marker: %w", markerErr)
		}
		if _, markerErr = marker.Write([]byte("namespace=agent-task-delivery-v1\n")); markerErr == nil {
			markerErr = marker.Sync()
		}
		if markerErr == nil {
			markerErr = agentTaskSyncDirectory(rootFile)
		}
	}
	if markerErr != nil {
		return fmt.Errorf("open task artifact namespace marker: %w", markerErr)
	}
	defer marker.Close()
	if _, err := marker.Seek(0, io.SeekStart); err != nil {
		return err
	}
	contents, err := io.ReadAll(io.LimitReader(marker, 4097))
	if err != nil || string(contents) != "namespace=agent-task-delivery-v1\n" {
		return errors.New("task artifact namespace ownership marker is invalid")
	}
	return nil
}

type agentTaskArtifactNamespaceLease struct {
	file *os.File
}

func (l *agentTaskDeliveryLedger) lockArtifactNamespaceContext(ctx context.Context, exclusive bool) (*agentTaskArtifactNamespaceLease, error) {
	rootFile, err := openAgentTaskDirectoryNoFollow(l.artifactRoot)
	if err != nil {
		return nil, err
	}
	lockFile, err := openAgentTaskFileAt(rootFile, agentTaskArtifactNamespaceLock, agentTaskFileReadWrite, 0, 4096)
	_ = rootFile.Close()
	if err != nil {
		return nil, err
	}
	if err := agentTaskFlockContext(ctx, lockFile, exclusive); err != nil {
		_ = lockFile.Close()
		return nil, err
	}
	return &agentTaskArtifactNamespaceLease{file: lockFile}, nil
}

func (l *agentTaskArtifactNamespaceLease) close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	unlockErr := agentTaskUnlock(file)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func newAgentTaskDeliveryLedgerFromEnv() (*agentTaskDeliveryLedger, error) {
	return newAgentTaskDeliveryLedgerFromEnvContext(context.Background())
}

func newAgentTaskDeliveryLedgerFromEnvContext(ctx context.Context) (*agentTaskDeliveryLedger, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	path := agentTaskLedgerPath()
	if path == "" {
		return nil, errors.New("agent task ledger path is empty")
	}
	if path != ":memory:" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, fmt.Errorf("create task ledger directory: %w", err)
		}
		if err := ensureOwnerOnlyDirectory(filepath.Dir(path), true); err != nil {
			return nil, fmt.Errorf("prepare task ledger directory: %w", err)
		}
	}
	artifactRoot := agentTaskArtifactRoot()
	if artifactRoot == "" {
		return nil, errors.New("agent task artifact root is empty")
	}
	absoluteArtifactRoot, err := filepath.Abs(artifactRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve task artifact root: %w", err)
	}
	artifactRoot = filepath.Clean(absoluteArtifactRoot)
	if err := os.MkdirAll(artifactRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create task artifact directory: %w", err)
	}
	if err := ensureOwnerOnlyDirectory(artifactRoot, true); err != nil {
		return nil, fmt.Errorf("prepare task artifact directory: %w", err)
	}
	resolvedArtifactRoot, err := filepath.EvalSymlinks(artifactRoot)
	if err != nil || filepath.Clean(resolvedArtifactRoot) != artifactRoot {
		return nil, errors.New("task artifact namespace must use a canonical path without symlink components")
	}
	if err := prepareAgentTaskArtifactNamespace(ctx, artifactRoot); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", agentTaskSQLiteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open task ledger sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ledger := &agentTaskDeliveryLedger{
		db:                   db,
		path:                 path,
		artifactRoot:         artifactRoot,
		leaseTTL:             time.Duration(maxInt(10, envInt("GO_AGENT_TASK_LEASE_SECS", defaultAgentTaskLeaseSecs))) * time.Second,
		limits:               defaultAgentTaskDeliveryLimits(),
		maxExecutionAttempts: clampInt(envInt("GO_AGENT_TASK_MAX_EXECUTION_ATTEMPTS", agentTaskDefaultExecutionRetries), 1, agentTaskMaxExecutionRetries),
		idGenerator:          newAgentTaskID,
	}
	if err := ledger.initialize(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return ledger, nil
}

// modernc.org/sqlite supports _txlock=immediate, which makes every
// database/sql transaction acquire the writer reservation at BEGIN.  This is
// required for cross-process claim/idempotency fencing; SetMaxOpenConns(1) is
// only an in-process optimization and is not an authority boundary.
func agentTaskSQLiteDSN(path string) string {
	path = strings.TrimSpace(path)
	if path == ":memory:" {
		return "file:agent-task-ledger?mode=memory&cache=shared&_txlock=immediate"
	}
	if strings.HasPrefix(path, "file:") {
		if strings.Contains(path, "?") {
			return path + "&_txlock=immediate"
		}
		return path + "?_txlock=immediate"
	}
	escaped := (&url.URL{Path: path}).EscapedPath()
	return "file:" + escaped + "?_txlock=immediate"
}

type agentTaskIDQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

var errAgentTaskIDCollision = errors.New("generated task delivery id collided with durable storage")

func agentTaskIDLookupSQL(kind string) (string, error) {
	switch kind {
	case "task":
		return `SELECT 1 FROM task_ledger_tasks WHERE id=?`, nil
	case "attempt":
		return `SELECT 1 FROM task_ledger_attempts WHERE attempt_id=?`, nil
	case "lease":
		return `SELECT 1 FROM task_ledger_attempts WHERE lease_id=?`, nil
	case "event":
		return `SELECT 1 FROM task_ledger_events WHERE event_id=?`, nil
	case "artifact":
		return `SELECT 1 FROM task_ledger_artifacts WHERE artifact_id=?`, nil
	case "result":
		return `SELECT 1 FROM task_ledger_results WHERE result_id=?`, nil
	case "publication":
		return `SELECT 1 FROM task_ledger_publications WHERE publication_id=?`, nil
	case "cleanup-receipt":
		return `SELECT 1 FROM task_ledger_cleanup_receipts WHERE cleanup_receipt_id=?`, nil
	case "publication-claim":
		return `SELECT 1 FROM task_ledger_publications WHERE worker_claim_id=?`, nil
	case "delivery":
		return `SELECT 1 FROM task_ledger_deliveries WHERE delivery_id=?`, nil
	case "delivery-claim":
		return `SELECT 1 FROM task_ledger_deliveries WHERE worker_claim_id=?`, nil
	case "review-claim":
		return `SELECT 1 FROM task_ledger_reviewer_claims WHERE claim_id=?`, nil
	case "review":
		return `SELECT 1 FROM task_ledger_reviews WHERE review_id=?`, nil
	case "answer":
		return `SELECT 1 FROM task_ledger_blocking_answers WHERE answer_id=?`, nil
	case "approval":
		return `SELECT 1 FROM task_ledger_approvals WHERE approval_id=?`, nil
	case "nonce":
		return `SELECT 1 FROM task_ledger_approvals WHERE nonce=?`, nil
	case "integration":
		return `SELECT 1 FROM task_ledger_integrations WHERE integration_id=?`, nil
	case "worker-identity":
		return `SELECT 1 FROM task_ledger_worker_identities WHERE identity_id=?`, nil
	case "worker-identity-update":
		return `SELECT 1 FROM task_ledger_worker_identity_updates WHERE update_id=?`, nil
	case "worker-identity-retirement":
		return `SELECT 1 FROM task_ledger_worker_identity_retirements WHERE retirement_id=?`, nil
	default:
		return "", fmt.Errorf("unsupported task delivery id namespace %q", kind)
	}
}

func (l *agentTaskDeliveryLedger) newUniqueID(ctx context.Context, queryer agentTaskIDQueryer, kind, prefix string) (string, error) {
	if l == nil || queryer == nil {
		return "", errors.New("task delivery id storage is unavailable")
	}
	generator := l.idGenerator
	if generator == nil {
		generator = newAgentTaskID
	}
	lookup, err := agentTaskIDLookupSQL(kind)
	if err != nil {
		return "", err
	}
	for attempt := 0; attempt < agentTaskIDCollisionRetries; attempt++ {
		candidate, generateErr := generator(prefix)
		if generateErr != nil {
			return "", generateErr
		}
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || len(candidate) > 2048 {
			return "", errors.New("generated task delivery id is empty or oversized")
		}
		var found int
		lookupErr := queryer.QueryRowContext(ctx, lookup, candidate).Scan(&found)
		if errors.Is(lookupErr, sql.ErrNoRows) {
			return candidate, nil
		}
		if lookupErr != nil {
			return "", lookupErr
		}
	}
	return "", fmt.Errorf("%w after %d attempts", errAgentTaskIDCollision, agentTaskIDCollisionRetries)
}

func (l *agentTaskDeliveryLedger) ensureIDAvailable(ctx context.Context, queryer agentTaskIDQueryer, kind, id string) error {
	lookup, err := agentTaskIDLookupSQL(kind)
	if err != nil {
		return err
	}
	var found int
	err = queryer.QueryRowContext(ctx, lookup, strings.TrimSpace(id)).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return errAgentTaskIDCollision
}

func (l *agentTaskDeliveryLedger) initialize(ctx context.Context) error {
	if l == nil || l.db == nil {
		return errors.New("agent task ledger is unavailable")
	}
	// The artifact namespace lock is the process-independent initialization
	// fence shared by every handle for this authoritative ledger. Acquire it
	// before connection pragmas so WAL selection and versioned DDL cannot race.
	schemaLease, err := l.lockArtifactNamespaceContext(ctx, true)
	if err != nil {
		return fmt.Errorf("lock serialized task ledger schema migration: %w", err)
	}
	defer schemaLease.close()
	pragmas := []string{
		// Install the connection-local busy handler before any pragma that may
		// need a write lock. Concurrent Gateway handles can otherwise race while
		// switching a fresh database into WAL mode before the serialized schema
		// migration transaction has a chance to fence them.
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
	}
	for _, pragma := range pragmas {
		if _, err := l.db.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("initialize task ledger %s: %w", pragma, err)
		}
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin serialized task ledger schema migration: %w", err)
	}
	defer tx.Rollback()
	var storedSchemaVersion int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&storedSchemaVersion); err != nil {
		return fmt.Errorf("read task ledger schema version: %w", err)
	}
	if storedSchemaVersion > agentTaskLedgerSchemaVersion {
		return fmt.Errorf("task ledger schema version %d is newer than supported version %d", storedSchemaVersion, agentTaskLedgerSchemaVersion)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS task_ledger_meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_worker_identities (
			identity_id TEXT PRIMARY KEY,
			principal_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			requested_worker_id TEXT NOT NULL,
			canonical_worker_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL,
			worker_instance_credential_verifier TEXT NOT NULL DEFAULT '',
			worker_instance_credential_generation INTEGER NOT NULL DEFAULT 1,
			worker_identity_update_generation INTEGER NOT NULL DEFAULT 0,
			acknowledged_generation INTEGER NOT NULL DEFAULT 0,
			requested_id_digest TEXT NOT NULL,
			identity_digest TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			closed_at TEXT NOT NULL DEFAULT '',
			UNIQUE(workspace_id, canonical_worker_id),
			UNIQUE(workspace_id, worker_instance_id)
		)`,
		`CREATE INDEX IF NOT EXISTS task_ledger_worker_identities_authority_idx ON task_ledger_worker_identities(workspace_id, principal_id, worker_instance_id)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_worker_identity_updates (
			update_id TEXT PRIMARY KEY,
			identity_id TEXT NOT NULL REFERENCES task_ledger_worker_identities(identity_id),
			principal_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL,
			old_worker_id TEXT NOT NULL,
			requested_worker_id TEXT NOT NULL,
			new_worker_id TEXT NOT NULL,
			canonical_worker_id TEXT NOT NULL,
			worker_identity_update_generation INTEGER NOT NULL,
			update_digest TEXT NOT NULL,
			receipt_digest TEXT NOT NULL,
			state TEXT NOT NULL,
			delivery_attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			delivered_at TEXT NOT NULL DEFAULT '',
			acknowledged_at TEXT NOT NULL DEFAULT '',
			ack_receipt_digest TEXT NOT NULL DEFAULT '',
			ack_receipt_payload_json TEXT NOT NULL DEFAULT '',
			ack_receipt_payload_version INTEGER NOT NULL DEFAULT 0,
			expires_at TEXT NOT NULL DEFAULT '',
			UNIQUE(identity_id, worker_identity_update_generation)
		)`,
		`CREATE INDEX IF NOT EXISTS task_ledger_worker_identity_updates_pending_idx ON task_ledger_worker_identity_updates(workspace_id, worker_instance_id, state, worker_identity_update_generation)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_worker_identity_retirements (
			retirement_id TEXT PRIMARY KEY,
			identity_id TEXT NOT NULL REFERENCES task_ledger_worker_identities(identity_id),
			principal_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			requested_worker_id TEXT NOT NULL,
			canonical_worker_id TEXT NOT NULL,
			tombstone_canonical_worker_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL,
			worker_identity_update_generation INTEGER NOT NULL,
			acknowledged_generation INTEGER NOT NULL,
			identity_digest TEXT NOT NULL,
			closed_identity_digest TEXT NOT NULL DEFAULT '',
			closed_status TEXT NOT NULL DEFAULT '',
			retirement_digest TEXT NOT NULL,
			retirement_receipt_digest TEXT NOT NULL,
			closed_at TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			UNIQUE(identity_id),
			UNIQUE(retirement_digest)
		)`,
		`CREATE INDEX IF NOT EXISTS task_ledger_worker_identity_retirements_authority_idx ON task_ledger_worker_identity_retirements(workspace_id, principal_id, worker_instance_id)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_tasks (
			id TEXT PRIMARY KEY,
			project TEXT NOT NULL,
			workspace_id TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL,
			objective TEXT NOT NULL,
			acceptance_json TEXT NOT NULL,
			task_class TEXT NOT NULL,
			execution_profile TEXT NOT NULL,
			risk_level TEXT NOT NULL,
			approval_policy_json TEXT NOT NULL,
			context_request_json TEXT NOT NULL,
			recipients_json TEXT NOT NULL,
			review_owner TEXT NOT NULL,
			requesting_agent_id TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL UNIQUE,
			priority INTEGER NOT NULL DEFAULT 0,
			claim_eligible INTEGER NOT NULL DEFAULT 0,
			claim_worker_id TEXT NOT NULL DEFAULT '',
			max_execution_attempts INTEGER NOT NULL DEFAULT 3,
			status TEXT NOT NULL,
			generation INTEGER NOT NULL DEFAULT 0,
			attempt_number INTEGER NOT NULL DEFAULT 0,
			active_attempt_id TEXT,
			result_id TEXT,
			publication_id TEXT,
			approved INTEGER NOT NULL DEFAULT 0,
			revision_envelope_json TEXT NOT NULL DEFAULT '{}',
			metadata_json TEXT NOT NULL DEFAULT '{}',
			legacy_payload_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_worker_task_bindings (
			task_id TEXT PRIMARY KEY REFERENCES task_ledger_tasks(id),
			identity_id TEXT NOT NULL REFERENCES task_ledger_worker_identities(identity_id),
			principal_id TEXT NOT NULL,
			workspace_id TEXT NOT NULL,
			requested_worker_id TEXT NOT NULL,
			canonical_worker_id TEXT NOT NULL,
			worker_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL,
			worker_identity_update_generation INTEGER NOT NULL,
			state TEXT NOT NULL DEFAULT 'bound',
			rebind_update_id TEXT NOT NULL DEFAULT '',
			rebind_receipt_digest TEXT NOT NULL DEFAULT '',
			rebind_acknowledged_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(task_id, identity_id)
		)`,
		`CREATE INDEX IF NOT EXISTS task_ledger_worker_task_bindings_identity_idx ON task_ledger_worker_task_bindings(identity_id,workspace_id,worker_instance_id,worker_identity_update_generation,state,task_id)`,
		`CREATE INDEX IF NOT EXISTS task_ledger_tasks_status_idx ON task_ledger_tasks(status, priority DESC, created_at ASC, id ASC)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_attempts (
			attempt_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			attempt_number INTEGER NOT NULL,
			lease_id TEXT NOT NULL UNIQUE,
			generation INTEGER NOT NULL,
			worker_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL,
			worker_identity_update_generation INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			context_pack_hash TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			lease_expires_at TEXT NOT NULL,
			claimed_at TEXT NOT NULL,
			heartbeat_at TEXT NOT NULL,
			completed_at TEXT NOT NULL DEFAULT '',
			runner_exit_code INTEGER NOT NULL DEFAULT 0,
			runner_exit_observed INTEGER NOT NULL DEFAULT 0,
			runner_status TEXT NOT NULL DEFAULT '',
			observation_digest TEXT NOT NULL DEFAULT '',
			failure_disposition TEXT NOT NULL DEFAULT '',
			revision_envelope_json TEXT NOT NULL DEFAULT '{}'
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_ledger_attempt_task_number_idx ON task_ledger_attempts(task_id, attempt_number)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_events (
			event_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			attempt_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			message TEXT NOT NULL,
			metadata_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS task_ledger_events_task_idx ON task_ledger_events(task_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_artifacts (
			artifact_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			attempt_id TEXT NOT NULL REFERENCES task_ledger_attempts(attempt_id),
			name TEXT NOT NULL,
			digest TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			media_type TEXT NOT NULL,
			redaction_status TEXT NOT NULL,
			redaction_receipt TEXT NOT NULL DEFAULT '',
			access_policy_json TEXT NOT NULL,
			retention_expires_at TEXT NOT NULL,
			content_ref TEXT NOT NULL,
			content_path TEXT NOT NULL,
			finalized INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			UNIQUE(task_id, attempt_id, digest)
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_results (
			result_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			attempt_id TEXT NOT NULL REFERENCES task_ledger_attempts(attempt_id),
			schema_id TEXT NOT NULL,
			status TEXT NOT NULL,
			execution_observed INTEGER NOT NULL,
			payload_json TEXT NOT NULL,
			digest TEXT NOT NULL,
			created_at TEXT NOT NULL,
			immutable INTEGER NOT NULL DEFAULT 1
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_publications (
			publication_id TEXT PRIMARY KEY,
			result_id TEXT NOT NULL UNIQUE REFERENCES task_ledger_results(result_id),
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			attempt_id TEXT NOT NULL REFERENCES task_ledger_attempts(attempt_id),
			idempotency_key TEXT NOT NULL UNIQUE,
			intent_digest TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			writeback_status TEXT NOT NULL,
			writeback_ref TEXT NOT NULL DEFAULT '',
			writeback_intent_json TEXT NOT NULL DEFAULT '{}',
			delivery_row_count INTEGER NOT NULL DEFAULT 0,
			recovery_owner TEXT NOT NULL DEFAULT 'gateway-publication-worker',
			next_action TEXT NOT NULL DEFAULT 'retry_writeback',
			last_error TEXT NOT NULL DEFAULT '',
			worker_claim_id TEXT NOT NULL DEFAULT '',
			worker_claimed_by TEXT NOT NULL DEFAULT '',
			worker_claim_expires_at TEXT NOT NULL DEFAULT '',
			worker_attempts INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_cleanup_receipts (
			cleanup_receipt_id TEXT PRIMARY KEY,
			cleanup_authorization_digest TEXT NOT NULL UNIQUE,
			publication_receipt_digest TEXT NOT NULL,
			publication_id TEXT NOT NULL REFERENCES task_ledger_publications(publication_id),
			result_id TEXT NOT NULL REFERENCES task_ledger_results(result_id),
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			attempt_id TEXT NOT NULL REFERENCES task_ledger_attempts(attempt_id),
			lease_id TEXT NOT NULL,
			assignment_generation INTEGER NOT NULL,
			lease_generation INTEGER NOT NULL,
			worker_id TEXT NOT NULL,
			worker_instance_id TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_deliveries (
			delivery_id TEXT PRIMARY KEY,
			publication_id TEXT NOT NULL REFERENCES task_ledger_publications(publication_id),
			result_id TEXT NOT NULL REFERENCES task_ledger_results(result_id),
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			recipient_id TEXT NOT NULL,
			role TEXT NOT NULL,
			observer INTEGER NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			reviewer_owner TEXT NOT NULL,
			status TEXT NOT NULL,
			dedupe_key TEXT NOT NULL UNIQUE,
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			next_action TEXT NOT NULL DEFAULT 'deliver_continuation_inbox',
			acknowledged_at TEXT NOT NULL DEFAULT '',
			worker_claim_id TEXT NOT NULL DEFAULT '',
			worker_claimed_by TEXT NOT NULL DEFAULT '',
			worker_claim_expires_at TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS task_ledger_deliveries_task_idx ON task_ledger_deliveries(task_id, status, created_at)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_reviewer_claims (
			claim_id TEXT PRIMARY KEY,
			result_id TEXT NOT NULL UNIQUE REFERENCES task_ledger_results(result_id),
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			delivery_id TEXT NOT NULL REFERENCES task_ledger_deliveries(delivery_id),
			reviewer_owner TEXT NOT NULL,
			actor TEXT NOT NULL,
			status TEXT NOT NULL,
			generation INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_reviews (
			review_id TEXT PRIMARY KEY,
			result_id TEXT NOT NULL UNIQUE REFERENCES task_ledger_results(result_id),
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			reviewer_owner TEXT NOT NULL,
			status TEXT NOT NULL,
			decision TEXT NOT NULL,
			actor TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			replacement_result_id TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_blocking_answers (
			answer_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			result_id TEXT NOT NULL REFERENCES task_ledger_results(result_id),
			delivery_id TEXT NOT NULL REFERENCES task_ledger_deliveries(delivery_id),
			actor TEXT NOT NULL,
			answer TEXT NOT NULL,
			source_attempt_id TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_approvals (
			approval_id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			attempt_id TEXT NOT NULL,
			result_or_commit_digest TEXT NOT NULL,
			target TEXT NOT NULL,
			policy_version TEXT NOT NULL,
			approver TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			nonce TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL,
			used_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_integrations (
			integration_id TEXT PRIMARY KEY,
			result_id TEXT NOT NULL REFERENCES task_ledger_results(result_id),
			task_id TEXT NOT NULL REFERENCES task_ledger_tasks(id),
			action TEXT NOT NULL,
			status TEXT NOT NULL,
			actor TEXT NOT NULL,
			digest TEXT NOT NULL,
			target TEXT NOT NULL DEFAULT '',
			policy_digest TEXT NOT NULL DEFAULT '',
			approval_id TEXT NOT NULL DEFAULT '',
			approval_expires_at TEXT NOT NULL DEFAULT '',
			approval_policy_version TEXT NOT NULL DEFAULT '',
			policy_evidence_digest TEXT NOT NULL DEFAULT '',
			provider_ref TEXT NOT NULL DEFAULT '',
			execution_receipt_digest TEXT NOT NULL DEFAULT '',
			merge_allowed INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_ledger_integrations_result_action_idx ON task_ledger_integrations(result_id,action,digest)`,
		`CREATE TABLE IF NOT EXISTS task_ledger_migration_receipts (
			receipt_id TEXT PRIMARY KEY,
			source_path TEXT NOT NULL,
			source_digest TEXT NOT NULL,
			phase TEXT NOT NULL,
			imported INTEGER NOT NULL DEFAULT 0,
			validated INTEGER NOT NULL DEFAULT 0,
			frozen INTEGER NOT NULL DEFAULT 0,
			rolled_back INTEGER NOT NULL DEFAULT 0,
			details_json TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize task ledger schema: %w", err)
		}
	}
	for _, column := range []struct {
		table string
		name  string
		def   string
	}{
		{table: "task_ledger_attempts", name: "runner_exit_observed", def: "INTEGER NOT NULL DEFAULT 0"},
		{table: "task_ledger_tasks", name: "workspace_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_tasks", name: "requesting_agent_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_tasks", name: "claim_eligible", def: "INTEGER NOT NULL DEFAULT 0"},
		{table: "task_ledger_tasks", name: "claim_worker_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_tasks", name: "max_execution_attempts", def: "INTEGER NOT NULL DEFAULT 3"},
		{table: "task_ledger_tasks", name: "revision_envelope_json", def: "TEXT NOT NULL DEFAULT '{}'"},
		{table: "task_ledger_attempts", name: "observation_digest", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_attempts", name: "failure_disposition", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_attempts", name: "revision_envelope_json", def: "TEXT NOT NULL DEFAULT '{}'"},
		{table: "task_ledger_attempts", name: "worker_identity_update_generation", def: "INTEGER NOT NULL DEFAULT 0"},
		{table: "task_ledger_worker_identity_updates", name: "ack_receipt_payload_json", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_worker_identity_updates", name: "ack_receipt_payload_version", def: "INTEGER NOT NULL DEFAULT 0"},
		{table: "task_ledger_artifacts", name: "content_ref", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_artifacts", name: "content_path", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_artifacts", name: "redaction_receipt", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_publications", name: "writeback_intent_json", def: "TEXT NOT NULL DEFAULT '{}'"},
		{table: "task_ledger_publications", name: "intent_digest", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_integrations", name: "execution_receipt_digest", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_integrations", name: "target", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_integrations", name: "policy_digest", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_integrations", name: "approval_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_integrations", name: "approval_expires_at", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_integrations", name: "approval_policy_version", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_integrations", name: "policy_evidence_digest", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_publications", name: "worker_claim_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_publications", name: "worker_claimed_by", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_publications", name: "worker_claim_expires_at", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_publications", name: "worker_attempts", def: "INTEGER NOT NULL DEFAULT 0"},
		{table: "task_ledger_deliveries", name: "worker_claim_id", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_deliveries", name: "worker_claimed_by", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_deliveries", name: "worker_claim_expires_at", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_worker_identity_retirements", name: "closed_identity_digest", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_worker_identity_retirements", name: "closed_status", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_worker_identities", name: "worker_instance_credential_verifier", def: "TEXT NOT NULL DEFAULT ''"},
		{table: "task_ledger_worker_identities", name: "worker_instance_credential_generation", def: "INTEGER NOT NULL DEFAULT 1"},
	} {
		if err := l.ensureColumnTx(ctx, tx, column.table, column.name, column.def); err != nil {
			return err
		}
	}
	if err := l.migrateLegacyWorkerIdentityAckReceiptsTx(ctx, tx); err != nil {
		return fmt.Errorf("migrate worker identity acknowledgement receipts: %w", err)
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS task_ledger_tasks_claim_idx ON task_ledger_tasks(status,claim_eligible,claim_worker_id,workspace_id,priority DESC,created_at ASC,id ASC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_ledger_delivery_worker_claim_idx ON task_ledger_deliveries(worker_claim_id) WHERE worker_claim_id<>''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_ledger_publication_worker_claim_idx ON task_ledger_publications(worker_claim_id) WHERE worker_claim_id<>''`,
		`CREATE UNIQUE INDEX IF NOT EXISTS task_ledger_migration_phase_digest_idx ON task_ledger_migration_receipts(source_digest,phase)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize task ledger index: %w", err)
		}
	}
	// Rows written by schema v1 did not carry materialized eligibility. Derive
	// it once under the same serialized migration transaction. Re-running this
	// projection on every restart would overwrite a server-authoritative
	// canonical rebind (or a durable disabled/recovery state) with the original
	// metadata worker hint.
	if storedSchemaVersion < agentTaskClaimEligibilityProjectionSchemaVersion {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET
			claim_worker_id=LOWER(TRIM(COALESCE(json_extract(metadata_json,'$.worker'),''))),
			claim_eligible=CASE
				WHEN status='queued'
					AND (COALESCE(json_extract(approval_policy_json,'$.required'),0)=0 OR approved=1)
					AND context_request_json LIKE '%"content_hash":"sha256:%'
					AND COALESCE(json_extract(context_request_json,'$.session_id'),'')<>''
				THEN 1 ELSE 0 END,
			max_execution_attempts=CASE WHEN max_execution_attempts<1 THEN ? WHEN max_execution_attempts>? THEN ? ELSE max_execution_attempts END`, l.maxExecutionAttempts, agentTaskMaxExecutionRetries, agentTaskMaxExecutionRetries); err != nil {
			return fmt.Errorf("derive task claim eligibility during schema migration: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, agentTaskLedgerSchemaVersion)); err != nil {
		return fmt.Errorf("set task ledger schema version: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_meta(key,value,updated_at) VALUES('schema_version',?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, fmt.Sprintf("%d", agentTaskLedgerSchemaVersion), agentTaskNow()); err != nil {
		return fmt.Errorf("record task ledger schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit serialized task ledger schema migration: %w", err)
	}
	if err := schemaLease.close(); err != nil {
		return fmt.Errorf("unlock serialized task ledger schema migration: %w", err)
	}
	if err := l.migrateLegacyWorkerIdentitiesAtStartup(ctx); err != nil {
		return fmt.Errorf("migrate legacy worker identity credentials at startup: %w", err)
	}
	if err := l.reconcileArtifactOrphans(ctx); err != nil {
		return err
	}
	return nil
}

func (l *agentTaskDeliveryLedger) ensureColumnTx(ctx context.Context, tx *sql.Tx, table, column, definition string) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return fmt.Errorf("inspect task ledger schema: %w", err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition); err != nil {
		return fmt.Errorf("upgrade task ledger schema %s.%s: %w", table, column, err)
	}
	return nil
}

func (l *agentTaskDeliveryLedger) reconcileArtifactOrphans(ctx context.Context) error {
	if strings.TrimSpace(l.artifactRoot) == "" {
		return nil
	}
	if l.artifactReconcileHook != nil {
		l.artifactReconcileHook("before_lock")
	}
	lease, err := l.lockArtifactNamespaceContext(ctx, true)
	if err != nil {
		return fmt.Errorf("lock task artifact reconciliation: %w", err)
	}
	defer lease.close()
	if l.artifactReconcileHook != nil {
		l.artifactReconcileHook("after_lock")
	}
	root, err := openAgentTaskDirectoryNoFollow(l.artifactRoot)
	if err != nil {
		return errors.New("task artifact namespace is not a real owner-only directory")
	}
	defer root.Close()
	marker, markerErr := openAgentTaskFileAt(root, agentTaskArtifactNamespaceMarker, agentTaskFileReadOnly, 0, 4096)
	if markerErr != nil {
		return errors.New("task artifact namespace marker is missing or unsafe")
	}
	_ = marker.Close()
	orphans := 0
	rootEntries, err := root.ReadDir(-1)
	if err != nil {
		return fmt.Errorf("read task artifact namespace: %w", err)
	}
	for _, rootEntry := range rootEntries {
		name := rootEntry.Name()
		if name == agentTaskArtifactNamespaceMarker || name == agentTaskArtifactNamespaceLock {
			continue
		}
		if len(name) != 2 || !isLowerHex(name) || rootEntry.Type()&os.ModeSymlink != 0 || !rootEntry.IsDir() {
			return fmt.Errorf("task artifact namespace contains unsafe root entry %q", name)
		}
		shard, openErr := openAgentTaskDirectoryAt(root, name, false)
		if openErr != nil {
			return fmt.Errorf("open task artifact shard %s: %w", name, openErr)
		}
		entries, readErr := shard.ReadDir(-1)
		if readErr != nil {
			_ = shard.Close()
			return fmt.Errorf("read task artifact shard %s: %w", name, readErr)
		}
		for _, entry := range entries {
			entryName := entry.Name()
			if strings.Contains(entryName, ".orphan-") {
				if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
					_ = shard.Close()
					return fmt.Errorf("task artifact orphan %q is not a regular file", entryName)
				}
				continue
			}
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				_ = shard.Close()
				return fmt.Errorf("task artifact shard contains unsafe entry %q", entryName)
			}
			isTemp := strings.HasPrefix(entryName, ".") && strings.HasSuffix(entryName, ".tmp")
			hexDigest := strings.TrimSuffix(entryName, ".blob")
			isCanonical := len(hexDigest) == 64 && strings.HasSuffix(entryName, ".blob") && isLowerHex(hexDigest) && strings.HasPrefix(hexDigest, name)
			if !isCanonical && !isTemp {
				_ = shard.Close()
				return fmt.Errorf("task artifact shard contains unknown file %q", entryName)
			}
			file, fileErr := openAgentTaskFileAt(shard, entryName, agentTaskFileReadOnly, 0, l.limits.MaxArtifactBytes)
			if fileErr != nil {
				_ = shard.Close()
				return fmt.Errorf("open task artifact file %q: %w", entryName, fileErr)
			}
			if isCanonical {
				digest := "sha256:" + hexDigest
				handle := &agentTaskArtifactHandle{file: file, path: filepath.Join(l.artifactRoot, name, entryName), digest: digest}
				if _, verifyErr := handle.readAndVerify(l.limits.MaxArtifactBytes); verifyErr != nil {
					_ = handle.close()
					_ = shard.Close()
					return fmt.Errorf("verify task artifact file %q: %w", entryName, verifyErr)
				}
				_ = handle.close()
				var referenced int
				if queryErr := l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_artifacts WHERE digest=? AND content_path=?`, digest, handle.path).Scan(&referenced); queryErr != nil {
					_ = shard.Close()
					return queryErr
				}
				if referenced > 0 {
					continue
				}
			} else {
				_ = file.Close()
			}
			// Preserve uncommitted bytes for forensic recovery; never delete a
			// content-addressed file during startup reconciliation.
			renamed := false
			for collision := 0; collision < agentTaskIDCollisionRetries; collision++ {
				orphanID, idErr := newAgentTaskID("orphan")
				if idErr != nil {
					_ = shard.Close()
					return idErr
				}
				orphanName := entryName + ".orphan-" + orphanID
				renameErr := agentTaskRenameAt(shard, entryName, orphanName)
				if errors.Is(renameErr, os.ErrExist) {
					continue
				}
				if renameErr != nil {
					_ = shard.Close()
					return renameErr
				}
				renamed = true
				orphans++
				break
			}
			if !renamed {
				_ = shard.Close()
				return errAgentTaskIDCollision
			}
		}
		_ = shard.Close()
	}
	if err := agentTaskSyncDirectory(root); err != nil {
		return fmt.Errorf("sync reconciled task artifact namespace: %w", err)
	}
	return l.setMeta(ctx, "artifact_orphan_last_scan_count", fmt.Sprintf("%d", orphans))
}

func isLowerHex(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return false
		}
	}
	return true
}

func (l *agentTaskDeliveryLedger) close() error {
	if l == nil || l.db == nil {
		return nil
	}
	return l.db.Close()
}

func encodeAgentTaskJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func decodeAgentTaskMap(raw string) map[string]any {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil || value == nil {
		return map[string]any{}
	}
	return value
}

func (l *agentTaskDeliveryLedger) setMeta(ctx context.Context, key, value string) error {
	if err := agentTaskValidateStructured(map[string]any{"key": key, "value": value}, "task ledger metadata", agentTaskEventMaxBytes*8); err != nil {
		return err
	}
	_, err := l.db.ExecContext(ctx, `INSERT INTO task_ledger_meta(key,value,updated_at) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, strings.TrimSpace(key), strings.TrimSpace(value), agentTaskNow())
	return err
}

func (l *agentTaskDeliveryLedger) getMeta(ctx context.Context, key string) (string, error) {
	var value string
	err := l.db.QueryRowContext(ctx, `SELECT value FROM task_ledger_meta WHERE key = ?`, strings.TrimSpace(key)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

type agentTaskSQLScanner interface {
	Scan(dest ...any) error
}

func scanAgentTaskRow(scanner agentTaskSQLScanner) (map[string]any, error) {
	var (
		id, project, workspaceID, title, objective, acceptanceJSON, taskClass, executionProfile, riskLevel string
		approvalJSON, contextJSON, recipientsJSON, reviewOwner, requestingAgentID, idempotencyKey          string
		claimWorkerID                                                                                      string
		status                                                                                             string
		priority, claimEligible, maxExecutionAttempts, generation, attemptNumber, approved                 int
		activeAttemptID, resultID, publicationID, revisionEnvelopeJSON, metadataJSON, legacyJSON           string
		createdAt, updatedAt                                                                               string
	)
	err := scanner.Scan(&id, &project, &workspaceID, &title, &objective, &acceptanceJSON, &taskClass, &executionProfile, &riskLevel, &approvalJSON, &contextJSON, &recipientsJSON, &reviewOwner, &requestingAgentID, &idempotencyKey, &priority, &claimEligible, &claimWorkerID, &maxExecutionAttempts, &status, &generation, &attemptNumber, &activeAttemptID, &resultID, &publicationID, &approved, &revisionEnvelopeJSON, &metadataJSON, &legacyJSON, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	row := map[string]any{
		"schema_id":              agentTaskManifestContractID,
		"contract_version":       1,
		"task_id":                id,
		"id":                     id,
		"project":                project,
		"workspace_id":           workspaceID,
		"title":                  title,
		"objective":              objective,
		"acceptance_criteria":    decodeAgentTaskStringList(acceptanceJSON),
		"task_class":             taskClass,
		"execution_profile":      executionProfile,
		"risk_level":             riskLevel,
		"approval_policy":        decodeAgentTaskMap(approvalJSON),
		"context_request":        decodeAgentTaskMap(contextJSON),
		"recipients":             decodeAgentTaskList(recipientsJSON),
		"review_owner":           reviewOwner,
		"requesting_agent_id":    requestingAgentID,
		"idempotency_key":        idempotencyKey,
		"priority":               priority,
		"claim_eligible":         claimEligible != 0,
		"claim_worker_id":        claimWorkerID,
		"max_execution_attempts": maxExecutionAttempts,
		"status":                 status,
		"generation":             generation,
		"attempt_number":         attemptNumber,
		"active_attempt_id":      activeAttemptID,
		"result_id":              resultID,
		"publication_id":         publicationID,
		"approved":               approved != 0,
		"revision_envelope":      decodeAgentTaskMap(revisionEnvelopeJSON),
		"metadata":               decodeAgentTaskMap(metadataJSON),
		"created_at":             createdAt,
		"updated_at":             updatedAt,
	}
	if legacy := decodeAgentTaskMap(legacyJSON); len(legacy) > 0 {
		row["legacy_payload"] = legacy
	}
	return row, nil
}

func decodeAgentTaskStringList(raw string) []string {
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil || list == nil {
		return []string{}
	}
	return list
}

func decodeAgentTaskList(raw string) []any {
	var list []any
	if err := json.Unmarshal([]byte(raw), &list); err != nil || list == nil {
		return []any{}
	}
	return list
}

const agentTaskSelectColumns = `id,project,workspace_id,title,objective,acceptance_json,task_class,execution_profile,risk_level,approval_policy_json,context_request_json,recipients_json,review_owner,requesting_agent_id,idempotency_key,priority,claim_eligible,claim_worker_id,max_execution_attempts,status,generation,attempt_number,active_attempt_id,result_id,publication_id,approved,revision_envelope_json,metadata_json,legacy_payload_json,created_at,updated_at`

func (l *agentTaskDeliveryLedger) queryTask(ctx context.Context, taskID string) (map[string]any, error) {
	return scanAgentTaskRow(l.db.QueryRowContext(ctx, `SELECT `+agentTaskSelectColumns+` FROM task_ledger_tasks WHERE id = ?`, strings.TrimSpace(taskID)))
}

func (l *agentTaskDeliveryLedger) queryTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (map[string]any, error) {
	return scanAgentTaskRow(tx.QueryRowContext(ctx, `SELECT `+agentTaskSelectColumns+` FROM task_ledger_tasks WHERE id = ?`, strings.TrimSpace(taskID)))
}

func (l *agentTaskDeliveryLedger) appendEventTx(ctx context.Context, tx *sql.Tx, taskID, attemptID, status, message string, metadata map[string]any) error {
	eventStatus := agentTaskStatus(status)
	if eventStatus == "" {
		eventStatus = strings.TrimSpace(strings.ToLower(status))
	}
	var eventCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_events WHERE task_id=? AND attempt_id=?`, strings.TrimSpace(taskID), strings.TrimSpace(attemptID)).Scan(&eventCount); err != nil {
		return err
	}
	if eventCount >= l.limits.MaxEvents {
		return fmt.Errorf("task event count exceeds %d event limit", l.limits.MaxEvents)
	}
	eventPayload := map[string]any{
		"task_id": strings.TrimSpace(taskID), "attempt_id": strings.TrimSpace(attemptID),
		"status": eventStatus, "message": message, "metadata": cloneAnyMap(metadata),
	}
	if err := agentTaskValidateStructured(eventPayload, "task event", l.limits.EventBytes); err != nil {
		return err
	}
	eventID, err := l.newUniqueID(ctx, tx, "event", "event")
	if err != nil {
		return err
	}
	metadataJSON := encodeAgentTaskJSON(eventPayload["metadata"])
	_, err = tx.ExecContext(ctx, `INSERT INTO task_ledger_events(event_id,task_id,attempt_id,status,message,metadata_json,created_at) VALUES(?,?,?,?,?,?,?)`, eventID, strings.TrimSpace(taskID), strings.TrimSpace(attemptID), eventStatus, message, metadataJSON, agentTaskNow())
	return err
}

func (l *agentTaskDeliveryLedger) taskEvents(ctx context.Context, taskID string) ([]map[string]any, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT event_id,attempt_id,status,message,metadata_json,created_at FROM task_ledger_events WHERE task_id = ? ORDER BY created_at ASC`, strings.TrimSpace(taskID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, attemptID, status, message, metadataJSON, createdAt string
		if err := rows.Scan(&id, &attemptID, &status, &message, &metadataJSON, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"event_id": id, "task_id": taskID, "attempt_id": attemptID, "status": status, "message": message, "metadata": decodeAgentTaskMap(metadataJSON), "created_at": createdAt})
	}
	return out, rows.Err()
}

func (l *agentTaskDeliveryLedger) submit(ctx context.Context, input map[string]any) (map[string]any, error) {
	if err := agentTaskValidateStructured(input, "task manifest input", agentTaskContextPackMaxBytes*2); err != nil {
		return nil, err
	}
	suppliedTaskID := strings.TrimSpace(anyToString(input["task_id"])) != ""
	for collisionAttempt := 0; collisionAttempt < agentTaskIDCollisionRetries; collisionAttempt++ {
		candidate := cloneAnyMap(input)
		if !suppliedTaskID {
			idempotencyKey := strings.TrimSpace(firstNonEmptyStrings(anyToString(candidate["idempotency_key"]), anyToString(candidate["idempotencyKey"])))
			var existingID string
			if idempotencyKey != "" {
				lookupErr := l.db.QueryRowContext(ctx, `SELECT id FROM task_ledger_tasks WHERE idempotency_key=?`, idempotencyKey).Scan(&existingID)
				if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
					return nil, lookupErr
				}
			}
			if existingID == "" {
				generatedID, err := l.newUniqueID(ctx, l.db, "task", "task")
				if err != nil {
					return nil, err
				}
				candidate["task_id"] = generatedID
			} else {
				candidate["task_id"] = existingID
			}
		}
		manifest, legacy, err := normalizeAgentTaskManifest(candidate)
		if err != nil {
			return nil, err
		}
		if err := agentTaskValidateStructured(manifest, "normalized task manifest", agentTaskContextPackMaxBytes*2); err != nil {
			return nil, err
		}
		row, err := l.submitNormalized(ctx, manifest, legacy, !suppliedTaskID)
		if errors.Is(err, errAgentTaskIDCollision) && !suppliedTaskID {
			continue
		}
		return row, err
	}
	return nil, fmt.Errorf("%w after %d task insert attempts", errAgentTaskIDCollision, agentTaskIDCollisionRetries)
}

func agentTaskClaimMaterialization(manifest map[string]any) (int, string, error) {
	metadata := cloneAnyMap(anyMap(manifest["metadata"]))
	workerID := strings.ToLower(strings.TrimSpace(anyToString(metadata["worker"])))
	if err := agentTaskValidateText(workerID, "task claim worker", 2048); err != nil {
		return 0, "", err
	}
	contextRequest := anyMap(manifest["context_request"])
	eligible := agentTaskCanonicalSHA256(anyToString(contextRequest["content_hash"])) && strings.TrimSpace(anyToString(contextRequest["session_id"])) != ""
	policy := anyMap(manifest["approval_policy"])
	if anyToBool(policy["required"]) && !anyToBool(manifest["approved"]) {
		eligible = false
	}
	return boolToSQLiteInt(eligible), workerID, nil
}

func (l *agentTaskDeliveryLedger) submitNormalized(ctx context.Context, manifest map[string]any, legacy, generatedTaskID bool) (map[string]any, error) {
	taskID := anyToString(manifest["task_id"])
	idempotencyKey := anyToString(manifest["idempotency_key"])
	digest := agentTaskDigest(manifest)
	if digest == "" {
		return nil, errors.New("task manifest is not canonically serializable")
	}
	claimEligible, claimWorkerID, err := agentTaskClaimMaterialization(manifest)
	if err != nil {
		return nil, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var existingID, existingKey, existingLegacyJSON string
	lookupErr := tx.QueryRowContext(ctx, `SELECT id,idempotency_key,legacy_payload_json FROM task_ledger_tasks WHERE id = ? OR idempotency_key = ? ORDER BY CASE WHEN id=? THEN 0 ELSE 1 END LIMIT 1`, taskID, idempotencyKey, taskID).Scan(&existingID, &existingKey, &existingLegacyJSON)
	if lookupErr == nil {
		if existingID == taskID && existingKey != idempotencyKey && generatedTaskID {
			return nil, errAgentTaskIDCollision
		}
		if existingID != taskID || existingKey != idempotencyKey {
			return nil, fmt.Errorf("idempotency key or task id already belongs to task %s", existingID)
		}
		storedManifestDigest := anyToString(decodeAgentTaskMap(existingLegacyJSON)["manifest_digest"])
		if storedManifestDigest == "" || storedManifestDigest != digest {
			return nil, errors.New("idempotency key replay does not match the immutable task manifest")
		}
		existing, queryErr := l.queryTaskTx(ctx, tx, existingID)
		if queryErr != nil {
			return nil, queryErr
		}
		existing["idempotent_replay"] = true
		existing["manifest_digest"] = storedManifestDigest
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return nil, lookupErr
	}
	if generatedTaskID {
		if err := l.ensureIDAvailable(ctx, tx, "task", taskID); err != nil {
			return nil, err
		}
	}
	metadata := cloneAnyMap(anyMap(manifest["metadata"]))
	if err := agentTaskValidateStructured(metadata, "task metadata", agentTaskEventMaxBytes*4); err != nil {
		return nil, err
	}
	legacyEvidence := map[string]any{"legacy": legacy, "manifest_digest": digest}
	if err := agentTaskValidateStructured(legacyEvidence, "task compatibility evidence", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	now := agentTaskNow()
	_, err = tx.ExecContext(ctx, `INSERT INTO task_ledger_tasks(id,project,workspace_id,title,objective,acceptance_json,task_class,execution_profile,risk_level,approval_policy_json,context_request_json,recipients_json,review_owner,requesting_agent_id,idempotency_key,priority,claim_eligible,claim_worker_id,max_execution_attempts,status,generation,attempt_number,active_attempt_id,result_id,publication_id,approved,metadata_json,legacy_payload_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		taskID,
		anyToString(manifest["project"]),
		anyToString(manifest["workspace_id"]),
		anyToString(manifest["title"]),
		anyToString(manifest["objective"]),
		encodeAgentTaskJSON(manifest["acceptance_criteria"]),
		anyToString(manifest["task_class"]),
		anyToString(manifest["execution_profile"]),
		anyToString(manifest["risk_level"]),
		encodeAgentTaskJSON(manifest["approval_policy"]),
		encodeAgentTaskJSON(manifest["context_request"]),
		encodeAgentTaskJSON(manifest["recipients"]),
		anyToString(manifest["review_owner"]),
		anyToString(manifest["requesting_agent_id"]),
		idempotencyKey,
		clampInt(anyToInt(manifest["priority"], 0), -1000000, 1000000),
		claimEligible,
		claimWorkerID,
		l.maxExecutionAttempts,
		"queued", 0, 0, "", "", "", anyToBool(manifest["approved"]), encodeAgentTaskJSON(metadata), encodeAgentTaskJSON(legacyEvidence), now, now)
	if err != nil {
		return nil, fmt.Errorf("insert task manifest: %w", err)
	}
	// A queued worker hint becomes an exact authorization binding only when
	// the manifest also identifies one unambiguous active instance.  A bare
	// requested ID (especially with same-requested-ID instances) stays
	// unbound and eligible; migration/retirement must never guess its owner.
	if claimWorkerID != "" {
		instanceHint := strings.TrimSpace(anyToString(metadata["worker_instance_id"]))
		var queuedIdentity agentWorkerIdentityRecord
		var identityErr error
		if instanceHint != "" {
			queuedIdentity, identityErr = scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE workspace_id=? AND worker_instance_id=? AND status='active'`, anyToString(manifest["workspace_id"]), instanceHint))
			if identityErr == nil && !strings.EqualFold(queuedIdentity.CanonicalWorkerID, claimWorkerID) && !strings.EqualFold(queuedIdentity.RequestedWorkerID, claimWorkerID) {
				identityErr = errors.New("worker task manifest instance binding does not match its worker ID")
			}
		} else {
			queuedIdentity, identityErr = scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE workspace_id=? AND canonical_worker_id=? AND status='active'`, anyToString(manifest["workspace_id"]), claimWorkerID))
		}
		if identityErr == nil {
			generation := queuedIdentity.IdentityUpdateGeneration
			workerID := queuedIdentity.CanonicalWorkerID
			if strings.EqualFold(claimWorkerID, queuedIdentity.RequestedWorkerID) && !strings.EqualFold(claimWorkerID, queuedIdentity.CanonicalWorkerID) {
				// A requested spelling is not a current canonical authority after
				// collision; leave it unbound rather than inventing a generation.
				identityErr = errors.New("worker task manifest requested ID is not the current canonical authority")
			} else if err := bindWorkerIdentityTaskTx(ctx, tx, taskID, queuedIdentity, workerID, generation); err != nil {
				return nil, err
			}
		}
		if identityErr != nil && !errors.Is(identityErr, sql.ErrNoRows) && !strings.Contains(identityErr.Error(), "requested ID is not the current canonical authority") {
			return nil, identityErr
		}
	}
	if err := l.appendEventTx(ctx, tx, taskID, "", "queued", "task accepted by gateway task ledger", map[string]any{"legacy_compatibility": legacy, "manifest_digest": digest}); err != nil {
		return nil, err
	}
	task, err := l.queryTaskTx(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

func (l *agentTaskDeliveryLedger) get(ctx context.Context, taskID string) (map[string]any, []map[string]any, error) {
	task, err := l.queryTask(ctx, taskID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	events, err := l.taskEvents(ctx, taskID)
	return task, events, err
}

func (l *agentTaskDeliveryLedger) list(ctx context.Context, status, project, agent string, limit int) ([]map[string]any, error) {
	limit = clampInt(limit, 1, 500)
	queryLimit := limit
	if strings.TrimSpace(agent) != "" {
		queryLimit = 500
	}
	where := []string{"1=1"}
	args := []any{}
	if status != "" {
		where = append(where, "status = ?")
		args = append(args, agentTaskStatus(status))
	}
	if project != "" {
		where = append(where, "project = ?")
		args = append(args, project)
	}
	args = append(args, queryLimit)
	rows, err := l.db.QueryContext(ctx, `SELECT `+agentTaskSelectColumns+` FROM task_ledger_tasks WHERE `+strings.Join(where, " AND ")+` ORDER BY priority DESC, created_at ASC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		row, scanErr := scanAgentTaskRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if agent != "" && !agentTaskTaskAllowsPrincipal(row, agent) {
			continue
		}
		out = append(out, row)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func (l *agentTaskDeliveryLedger) claimNext(ctx context.Context, workerID, workerInstanceID, workspaceID string) (map[string]any, error) {
	return l.claimTask(ctx, workerID, workerInstanceID, workspaceID, "")
}

func (l *agentTaskDeliveryLedger) claimTaskWithIdentity(ctx context.Context, workerID, workerInstanceID, workspaceID, preferredTaskID string, identityGeneration int) (map[string]any, error) {
	if identityGeneration < 0 {
		return nil, errors.New("worker identity update generation is invalid")
	}
	return l.claimTaskInternal(ctx, workerID, workerInstanceID, workspaceID, preferredTaskID, identityGeneration, true)
}

type agentTaskClaimCursor struct {
	Priority  int
	CreatedAt string
	TaskID    string
	Set       bool
}

// claimCandidateWindow returns only materialized SQLite-eligible rows. The
// cursor lets the Gateway apply current external workspace governance without
// restarting at the first 500 stale rows or holding a writer transaction.
func (l *agentTaskDeliveryLedger) claimCandidateWindow(ctx context.Context, workerID, workspaceID string, cursor agentTaskClaimCursor, limit int) ([]map[string]any, agentTaskClaimCursor, error) {
	workerID = strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(workerID, defaultAgentTaskWorker)))
	workspaceID = strings.TrimSpace(workspaceID)
	limit = clampInt(limit, 1, 128)
	query := `SELECT ` + agentTaskSelectColumns + ` FROM task_ledger_tasks WHERE status='queued' AND claim_eligible=1 AND (claim_worker_id='' OR claim_worker_id=?)`
	args := []any{workerID}
	if workspaceID != "" {
		query += ` AND lower(workspace_id)=lower(?)`
		args = append(args, workspaceID)
	}
	if cursor.Set {
		query += ` AND (priority<? OR (priority=? AND created_at>?) OR (priority=? AND created_at=? AND id>?))`
		args = append(args, cursor.Priority, cursor.Priority, cursor.CreatedAt, cursor.Priority, cursor.CreatedAt, cursor.TaskID)
	}
	query += ` ORDER BY priority DESC,created_at ASC,id ASC LIMIT ?`
	args = append(args, limit)
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, cursor, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit)
	next := cursor
	for rows.Next() {
		row, scanErr := scanAgentTaskRow(rows)
		if scanErr != nil {
			return nil, cursor, scanErr
		}
		out = append(out, row)
		next = agentTaskClaimCursor{Priority: anyToInt(row["priority"], 0), CreatedAt: anyToString(row["created_at"]), TaskID: anyToString(row["task_id"]), Set: true}
	}
	return out, next, rows.Err()
}

func (l *agentTaskDeliveryLedger) claimTask(ctx context.Context, workerID, workerInstanceID, workspaceID, preferredTaskID string) (map[string]any, error) {
	return l.claimTaskInternal(ctx, workerID, workerInstanceID, workspaceID, preferredTaskID, 0, false)
}

func (l *agentTaskDeliveryLedger) claimTaskInternal(ctx context.Context, workerID, workerInstanceID, workspaceID, preferredTaskID string, identityGeneration int, identityBound bool) (map[string]any, error) {
	workerID = strings.TrimSpace(firstNonEmptyStrings(workerID, defaultAgentTaskWorker))
	workerInstanceID = strings.TrimSpace(workerInstanceID)
	if workerInstanceID == "" {
		return nil, errors.New("worker_instance_id is required for an exact task claim fence")
	}
	workspaceID = strings.TrimSpace(workspaceID)
	preferredTaskID = strings.TrimSpace(preferredTaskID)
	for field, value := range map[string]string{"worker_id": workerID, "worker_instance_id": workerInstanceID, "workspace_id": workspaceID, "preferred_task_id": preferredTaskID} {
		if err := agentTaskValidateText(value, field, 2048); err != nil {
			return nil, err
		}
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var claimIdentity *agentWorkerIdentityRecord
	if identityBound {
		// The identity read used to select the candidate is intentionally not
		// authoritative. Retirement and claim both acquire the SQLite writer
		// transaction, so revalidate the complete identity fence here before
		// selecting or mutating a task. This makes the interleaving either
		// claim-before-retire (retirement sees the active attempt) or
		// retire-before-claim (the claim fails closed), never a stale claim after
		// a committed retirement.
		storedIdentity, identityErr := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE workspace_id=? AND worker_instance_id=?`, strings.ToLower(workspaceID), workerInstanceID))
		if errors.Is(identityErr, sql.ErrNoRows) {
			return nil, errors.New("worker identity instance fence is not registered")
		}
		if identityErr != nil {
			return nil, identityErr
		}
		if storedIdentity.WorkerInstanceID != workerInstanceID {
			return nil, errors.New("worker identity foreign-instance fence rejected")
		}
		if storedIdentity.Status != "active" {
			return nil, errors.New("worker identity is closed")
		}
		if !strings.EqualFold(storedIdentity.CanonicalWorkerID, workerID) {
			return nil, errors.New("worker identity canonical fence rejected")
		}
		if storedIdentity.IdentityUpdateGeneration != identityGeneration || storedIdentity.IdentityUpdateGeneration != storedIdentity.AcknowledgedGeneration {
			return nil, errWorkerIdentityUpdatePending
		}
		// Persist the exact server-issued spelling rather than a caller alias.
		workerID = storedIdentity.CanonicalWorkerID
		claimIdentity = &storedIdentity
	} else {
		// The trusted in-process compatibility surface may still supply an
		// instance. If it maps to an active identity, retain the exact binding
		// rather than leaving a newly leased task indistinguishable from an
		// unowned worker-ID hint.
		if candidate, identityErr := scanWorkerIdentity(tx.QueryRowContext(ctx, `SELECT `+workerIdentitySelectColumns+` FROM task_ledger_worker_identities WHERE workspace_id=? AND worker_instance_id=?`, strings.ToLower(workspaceID), workerInstanceID)); identityErr == nil && candidate.Status == "active" && (strings.EqualFold(candidate.CanonicalWorkerID, workerID) || strings.EqualFold(candidate.RequestedWorkerID, workerID)) {
			claimIdentity = &candidate
		}
	}
	query := `SELECT ` + agentTaskSelectColumns + ` FROM task_ledger_tasks WHERE status='queued' AND claim_eligible=1 AND (claim_worker_id='' OR claim_worker_id=?)`
	args := []any{strings.ToLower(workerID)}
	if claimIdentity != nil {
		query += ` AND (NOT EXISTS (SELECT 1 FROM task_ledger_worker_task_bindings b WHERE b.task_id=task_ledger_tasks.id AND b.state IN ('bound','rebind_pending')) OR EXISTS (SELECT 1 FROM task_ledger_worker_task_bindings b WHERE b.task_id=task_ledger_tasks.id AND b.identity_id=? AND b.principal_id=? AND lower(trim(b.workspace_id))=lower(?) AND b.worker_instance_id=? AND b.worker_identity_update_generation=? AND lower(trim(b.worker_id))=lower(?) AND b.state='bound'))`
		args = append(args, claimIdentity.IdentityID, claimIdentity.PrincipalID, claimIdentity.WorkspaceID, claimIdentity.WorkerInstanceID, claimIdentity.IdentityUpdateGeneration, workerID)
	} else {
		// Preserve the trusted compatibility surface for legacy unbound work and
		// the exact previously bound instance. Once a task has a durable identity
		// binding, a different instance cannot bypass it by copying the canonical
		// worker ID.
		query += ` AND (NOT EXISTS (SELECT 1 FROM task_ledger_worker_task_bindings b WHERE b.task_id=task_ledger_tasks.id AND b.state IN ('bound','rebind_pending')) OR EXISTS (SELECT 1 FROM task_ledger_worker_task_bindings b WHERE b.task_id=task_ledger_tasks.id AND lower(trim(b.worker_id))=lower(trim(?)) AND b.worker_instance_id=? AND b.state='bound'))`
		args = append(args, workerID, workerInstanceID)
	}
	if preferredTaskID != "" {
		query += ` AND id = ?`
		args = append(args, preferredTaskID)
	}
	if workspaceID != "" {
		query += ` AND lower(workspace_id) = lower(?)`
		args = append(args, workspaceID)
	}
	query += ` ORDER BY priority DESC, created_at ASC, id ASC LIMIT 1`
	selected, scanErr := scanAgentTaskRow(tx.QueryRowContext(ctx, query, args...))
	if errors.Is(scanErr, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if scanErr != nil {
		return nil, scanErr
	}
	taskID := anyToString(selected["task_id"])
	attemptNumber := anyToInt(selected["attempt_number"], 0) + 1
	generation := anyToInt(selected["generation"], 0) + 1
	attemptID, err := l.newUniqueID(ctx, tx, "attempt", "attempt")
	if err != nil {
		return nil, err
	}
	leaseID, err := l.newUniqueID(ctx, tx, "lease", "lease")
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expires := now.Add(l.leaseTTL).Format(time.RFC3339Nano)
	claimFence := agentTaskFence{TaskID: taskID, AttemptID: attemptID, LeaseID: leaseID, WorkerID: workerID, WorkerInstanceID: workerInstanceID, Generation: generation, WorkerIdentityUpdateGeneration: identityGeneration}
	revisionEnvelope := map[string]any{}
	revisionBase := anyMap(selected["revision_envelope"])
	if len(revisionBase) > 0 {
		var reviewID, reviewResultID, reviewTaskID, reviewReason, reviewDecision, reviewStatus, sourceAttemptID string
		var sourceGeneration int
		if err := tx.QueryRowContext(ctx, `SELECT r.review_id,r.result_id,r.task_id,r.reason,r.decision,r.status,x.attempt_id,a.generation FROM task_ledger_reviews r JOIN task_ledger_results x ON x.result_id=r.result_id AND x.task_id=r.task_id JOIN task_ledger_attempts a ON a.attempt_id=x.attempt_id AND a.task_id=x.task_id WHERE r.review_id=?`, anyToString(revisionBase["review_id"])).Scan(&reviewID, &reviewResultID, &reviewTaskID, &reviewReason, &reviewDecision, &reviewStatus, &sourceAttemptID, &sourceGeneration); err != nil {
			return nil, fmt.Errorf("validate revision source evidence: %w", err)
		}
		if reviewTaskID != taskID || reviewDecision != "request_changes" || reviewStatus != "changes_requested" || reviewID != anyToString(revisionBase["review_id"]) || reviewResultID != anyToString(revisionBase["source_result_id"]) || sourceAttemptID != anyToString(revisionBase["source_attempt_id"]) || sourceGeneration != anyToInt(revisionBase["source_generation"], 0) || reviewReason != anyToString(revisionBase["reason"]) {
			return nil, errors.New("revision envelope does not match the immutable request-changes review evidence")
		}
		revisionEnvelope, err = agentTaskMaterializeRevisionEnvelope(revisionBase, claimFence)
		if err != nil {
			return nil, err
		}
	}
	if !agentTaskAllowedTransition(anyToString(selected["status"]), "leased") {
		return nil, fmt.Errorf("invalid task transition %s -> leased", anyToString(selected["status"]))
	}
	updateResult, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='leased',claim_eligible=0,generation=?,attempt_number=?,active_attempt_id=?,revision_envelope_json='{}',updated_at=? WHERE id=? AND status='queued' AND claim_eligible=1`, generation, attemptNumber, attemptID, now.Format(time.RFC3339Nano), taskID)
	if err != nil {
		return nil, err
	}
	if affected, affectedErr := updateResult.RowsAffected(); affectedErr != nil || affected != 1 {
		return nil, errors.New("task claim lost its authoritative eligibility fence")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_attempts(attempt_id,task_id,attempt_number,lease_id,generation,worker_id,worker_instance_id,worker_identity_update_generation,status,context_pack_hash,session_id,lease_expires_at,claimed_at,heartbeat_at,revision_envelope_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, attemptID, taskID, attemptNumber, leaseID, generation, workerID, workerInstanceID, identityGeneration, "leased", anyToString(anyMap(selected["context_request"])["content_hash"]), anyToString(anyMap(selected["context_request"])["session_id"]), expires, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), encodeAgentTaskJSON(revisionEnvelope)); err != nil {
		return nil, err
	}
	if claimIdentity != nil {
		if err := bindWorkerIdentityTaskTx(ctx, tx, taskID, *claimIdentity, workerID, identityGeneration); err != nil {
			return nil, err
		}
	}
	if err := l.appendEventTx(ctx, tx, taskID, attemptID, "leased", "task claim issued with fenced lease", map[string]any{"worker_id": workerID, "worker_instance_id": workerInstanceID, "worker_identity_update_generation": identityGeneration, "lease_id": leaseID, "generation": generation}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	task, err := l.queryTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	attempt, err := l.attempt(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	lease := agentTaskContractPayload(agentTaskLeaseContractID, map[string]any{
		"schema_id": agentTaskLeaseContractID, "contract_version": 1,
		"task_id": taskID, "attempt_id": attemptID, "lease_id": leaseID,
		"worker": workerID, "worker_id": workerID, "worker_instance_id": workerInstanceID,
		"worker_identity_update_generation": identityGeneration,
		"generation":                        generation, "assignment_generation": generation, "lease_generation": generation,
		"runner": "gateway-go", "status": "claimed", "claimed_at": now.Format(time.RFC3339Nano), "expires_at": expires,
		"lease_expires_at": expires, "heartbeat_required": true, "heartbeat_interval_secs": maxInt(1, int(l.leaseTTL.Seconds()/3)),
		"worktree": "", "cwd": "", "allowed_paths": []string{}, "max_runtime_secs": int(l.leaseTTL.Seconds()),
		"capabilities": []string{}, "metadata": map[string]any{},
	})
	claim := map[string]any{"task": task, "attempt": attempt, "lease": lease, "authoritative_backend": "gateway-go-sqlite-wal"}
	if len(revisionEnvelope) > 0 {
		claim["revision_envelope"] = revisionEnvelope
		contextEnvelope := cloneAnyMap(anyMap(task["context_request"]))
		contextEnvelope["revision_envelope"] = revisionEnvelope
		claim["context"] = contextEnvelope
	}
	return claim, nil
}

func (l *agentTaskDeliveryLedger) attempt(ctx context.Context, attemptID string) (map[string]any, error) {
	return scanAgentTaskAttempt(l.db.QueryRowContext(ctx, `SELECT attempt_id,task_id,attempt_number,lease_id,generation,worker_id,worker_instance_id,worker_identity_update_generation,status,context_pack_hash,session_id,lease_expires_at,claimed_at,heartbeat_at,completed_at,runner_exit_code,runner_exit_observed,runner_status,observation_digest,failure_disposition,revision_envelope_json FROM task_ledger_attempts WHERE attempt_id=?`, strings.TrimSpace(attemptID)), l.leaseTTL)
}

func (l *agentTaskDeliveryLedger) attemptTx(ctx context.Context, tx *sql.Tx, attemptID string) (map[string]any, error) {
	return scanAgentTaskAttempt(tx.QueryRowContext(ctx, `SELECT attempt_id,task_id,attempt_number,lease_id,generation,worker_id,worker_instance_id,worker_identity_update_generation,status,context_pack_hash,session_id,lease_expires_at,claimed_at,heartbeat_at,completed_at,runner_exit_code,runner_exit_observed,runner_status,observation_digest,failure_disposition,revision_envelope_json FROM task_ledger_attempts WHERE attempt_id=?`, strings.TrimSpace(attemptID)), l.leaseTTL)
}

func scanAgentTaskAttempt(scanner agentTaskSQLScanner, leaseTTL time.Duration) (map[string]any, error) {
	var (
		attemptID                                                                                                                                 string
		taskID, leaseID, workerID, workerInstanceID, status, contextHash, sessionID, expiresAt, claimedAt, heartbeatAt, completedAt, runnerStatus string
		observationDigest, failureDisposition, revisionEnvelopeJSON                                                                               string
		attemptNumber, generation, identityGeneration, exitCode, exitObserved                                                                     int
	)
	err := scanner.Scan(&attemptID, &taskID, &attemptNumber, &leaseID, &generation, &workerID, &workerInstanceID, &identityGeneration, &status, &contextHash, &sessionID, &expiresAt, &claimedAt, &heartbeatAt, &completedAt, &exitCode, &exitObserved, &runnerStatus, &observationDigest, &failureDisposition, &revisionEnvelopeJSON)
	if err != nil {
		return nil, err
	}
	return agentTaskContractPayload(agentTaskAttemptContractID, map[string]any{
		"schema_id": agentTaskAttemptContractID, "contract_version": 1,
		"task_id": taskID, "attempt_id": attemptID, "attempt_number": attemptNumber, "lease_id": leaseID,
		"generation": generation, "assignment_generation": generation, "lease_generation": generation,
		"worker_identity_update_generation": identityGeneration, "worker_id": workerID, "worker_instance_id": workerInstanceID,
		"status": status, "context_pack_hash": contextHash, "session_id": sessionID, "lease_expires_at": expiresAt,
		"claimed_at": claimedAt, "heartbeat_at": heartbeatAt, "completed_at": completedAt,
		"runner_exit_code": exitCode, "runner_exit_observed": exitObserved != 0, "runner_status": runnerStatus,
		"observation_digest": observationDigest, "failure_disposition": failureDisposition,
		"revision_envelope":  decodeAgentTaskMap(revisionEnvelopeJSON),
		"heartbeat_required": true, "heartbeat_interval_secs": maxInt(1, int(leaseTTL.Seconds()/3)),
		"worktree": "", "cwd": "", "allowed_paths": []string{}, "max_runtime_secs": int(leaseTTL.Seconds()), "capabilities": []string{}, "metadata": map[string]any{},
	}), nil
}

func (l *agentTaskDeliveryLedger) fenceTx(ctx context.Context, tx *sql.Tx, fence agentTaskFence, requireActive bool) (map[string]any, error) {
	fence.TaskID = strings.TrimSpace(fence.TaskID)
	fence.AttemptID = strings.TrimSpace(fence.AttemptID)
	fence.LeaseID = strings.TrimSpace(fence.LeaseID)
	fence.WorkerID = strings.TrimSpace(fence.WorkerID)
	fence.WorkerInstanceID = strings.TrimSpace(fence.WorkerInstanceID)
	if fence.TaskID == "" || fence.AttemptID == "" || fence.LeaseID == "" || fence.WorkerID == "" || fence.WorkerInstanceID == "" || fence.Generation <= 0 {
		return nil, errors.New("complete task fence is required")
	}
	var (
		taskID, leaseID, workerID, workerInstanceID, status, expiresAt string
		generation, identityGeneration                                 int
	)
	err := tx.QueryRowContext(ctx, `SELECT task_id,lease_id,generation,worker_id,worker_instance_id,worker_identity_update_generation,status,lease_expires_at FROM task_ledger_attempts WHERE attempt_id=?`, fence.AttemptID).Scan(&taskID, &leaseID, &generation, &workerID, &workerInstanceID, &identityGeneration, &status, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errors.New("stale_lease_fence: attempt not found")
	}
	if err != nil {
		return nil, err
	}
	if taskID != fence.TaskID || leaseID != fence.LeaseID || generation != fence.Generation || workerID != fence.WorkerID || workerInstanceID != fence.WorkerInstanceID || identityGeneration != fence.WorkerIdentityUpdateGeneration {
		return nil, errors.New("stale_lease_fence: task, attempt, lease, worker, or generation mismatch")
	}
	if requireActive {
		expires, parseErr := time.Parse(time.RFC3339Nano, expiresAt)
		if parseErr != nil || !time.Now().UTC().Before(expires) {
			return nil, errors.New("stale_lease_fence: lease expired")
		}
		if status != "leased" && status != "running" && status != "waiting_for_input" {
			return nil, fmt.Errorf("stale_lease_fence: attempt status %s is not mutable", status)
		}
	}
	return map[string]any{"task_id": taskID, "attempt_id": fence.AttemptID, "lease_id": leaseID, "generation": generation, "worker_id": workerID, "worker_instance_id": workerInstanceID, "worker_identity_update_generation": identityGeneration, "status": status, "lease_expires_at": expiresAt}, nil
}

func (l *agentTaskDeliveryLedger) heartbeat(ctx context.Context, fence agentTaskFence) (map[string]any, error) {
	if err := agentTaskValidateStructured(fencePayload(fence), "heartbeat fence", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := l.fenceTx(ctx, tx, fence, true); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	expires := now.Add(l.leaseTTL).Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET status='running',lease_expires_at=?,heartbeat_at=? WHERE attempt_id=?`, expires, now.Format(time.RFC3339Nano), fence.AttemptID); err != nil {
		return nil, err
	}
	var taskStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM task_ledger_tasks WHERE id=?`, fence.TaskID).Scan(&taskStatus); err != nil {
		return nil, err
	}
	if taskStatus == "leased" && agentTaskAllowedTransition(taskStatus, "running") {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='running',updated_at=? WHERE id=?`, now.Format(time.RFC3339Nano), fence.TaskID); err != nil {
			return nil, err
		}
	}
	if err := l.appendEventTx(ctx, tx, fence.TaskID, fence.AttemptID, "running", "lease heartbeat accepted", map[string]any{"lease_id": fence.LeaseID, "generation": fence.Generation}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.attempt(ctx, fence.AttemptID)
}

func (l *agentTaskDeliveryLedger) cancelAttempt(ctx context.Context, fence agentTaskFence, terminationVerified bool, reason string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"fence": fencePayload(fence), "termination_verified": terminationVerified, "reason": reason}, "task cancellation", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := l.fenceTx(ctx, tx, fence, true); err != nil {
		return nil, err
	}
	var taskStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM task_ledger_tasks WHERE id=? AND active_attempt_id=?`, fence.TaskID, fence.AttemptID).Scan(&taskStatus); err != nil {
		return nil, err
	}
	target := "canceled"
	message := "fenced task attempt canceled after process-group termination was verified"
	if !terminationVerified {
		target = "quarantined"
		message = "task attempt quarantined because process-group termination was not verified"
	}
	if !agentTaskAllowedTransition(taskStatus, target) {
		return nil, fmt.Errorf("invalid task transition %s -> %s", taskStatus, target)
	}
	now := agentTaskNow()
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET status=?,completed_at=? WHERE attempt_id=?`, target, now, fence.AttemptID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status=?,updated_at=? WHERE id=? AND active_attempt_id=?`, target, now, fence.TaskID, fence.AttemptID); err != nil {
		return nil, err
	}
	if err := l.appendEventTx(ctx, tx, fence.TaskID, fence.AttemptID, target, message, map[string]any{"termination_verified": terminationVerified, "reason": agentTaskBoundedStringMust(reason, agentTaskEventMaxBytes/2)}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"task": mustTask(l, ctx, fence.TaskID), "attempt": mustAttempt(l, ctx, fence.AttemptID), "termination_verified": terminationVerified}, nil
}

func agentTaskTerminationResolutionDisposition(fence agentTaskFence, reason string) string {
	digest := agentTaskDigest(map[string]any{
		"fence": fencePayload(fence), "termination_verified": true, "reason": strings.TrimSpace(reason), "resolution": "requeue",
	})
	return "termination_verified_requeued:" + strings.TrimPrefix(digest, "sha256:")
}

// resolveQuarantinedAttempt is the sole path from an unverified termination
// quarantine back to the claim queue. Route authorization restricts it to the
// Gateway operator; this ledger method additionally binds the proof to the
// immutable attempt fence and makes exact retries restart-safe.
func (l *agentTaskDeliveryLedger) resolveQuarantinedAttempt(ctx context.Context, fence agentTaskFence, terminationVerified bool, reason string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"fence": fencePayload(fence), "termination_verified": terminationVerified, "reason": reason}, "quarantined attempt termination resolution", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	if !terminationVerified {
		return nil, errors.New("verified process-group termination is required to resolve quarantine")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errors.New("termination resolution reason is required")
	}
	disposition := agentTaskTerminationResolutionDisposition(fence, reason)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	fencedAttempt, err := l.fenceTx(ctx, tx, fence, false)
	if err != nil {
		return nil, err
	}
	var taskStatus, activeAttemptID string
	var taskGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT status,active_attempt_id,generation FROM task_ledger_tasks WHERE id=?`, fence.TaskID).Scan(&taskStatus, &activeAttemptID, &taskGeneration); err != nil {
		return nil, err
	}
	var storedDisposition, revisionEnvelopeJSON string
	if err := tx.QueryRowContext(ctx, `SELECT failure_disposition,revision_envelope_json FROM task_ledger_attempts WHERE attempt_id=?`, fence.AttemptID).Scan(&storedDisposition, &revisionEnvelopeJSON); err != nil {
		return nil, err
	}
	if strings.HasPrefix(storedDisposition, "termination_verified_requeued:") {
		if storedDisposition != disposition {
			return nil, errors.New("quarantine termination resolution is immutable and does not match the recorded proof")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return map[string]any{
			"task": mustTask(l, ctx, fence.TaskID), "attempt": mustAttempt(l, ctx, fence.AttemptID),
			"termination_verified": true, "requeued": true, "idempotent_replay": true,
		}, nil
	}
	if anyToString(fencedAttempt["status"]) != "quarantined" || taskStatus != "quarantined" || strings.TrimSpace(activeAttemptID) != fence.AttemptID || taskGeneration != fence.Generation {
		return nil, errors.New("stale_lease_fence: quarantine resolution does not match the active quarantined attempt")
	}
	if !agentTaskAllowedTransition(taskStatus, "queued") {
		return nil, fmt.Errorf("invalid task transition %s -> queued", taskStatus)
	}
	revisionSource, err := agentTaskRevisionSourceForRequeue(decodeAgentTaskMap(revisionEnvelopeJSON), fence)
	if err != nil {
		return nil, fmt.Errorf("restore quarantined revision instructions: %w", err)
	}
	quarantineIdentityAdoptionRequired := false
	if fence.WorkerIdentityUpdateGeneration == 0 {
		bound, bindingErr := workerIdentityGenerationZeroSourceBoundTx(ctx, tx, fence.TaskID, fence.WorkerID, fence.WorkerInstanceID)
		if bindingErr != nil {
			return nil, bindingErr
		}
		quarantineIdentityAdoptionRequired = !bound
	}
	now := agentTaskNow()
	updated, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET failure_disposition=? WHERE attempt_id=? AND task_id=? AND generation=? AND status='quarantined' AND failure_disposition=''`, disposition, fence.AttemptID, fence.TaskID, fence.Generation)
	if err != nil {
		return nil, err
	}
	if affected, affectedErr := updated.RowsAffected(); affectedErr != nil || affected != 1 {
		return nil, errors.New("stale_lease_fence: quarantine attempt proof lost its exact state")
	}
	updated, err = tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET
		status='queued',
		claim_eligible=CASE
			WHEN ?<>0 THEN 0
			WHEN (COALESCE(json_extract(approval_policy_json,'$.required'),0)=0 OR approved=1)
			 AND context_request_json LIKE '%"content_hash":"sha256:%'
			 AND COALESCE(json_extract(context_request_json,'$.session_id'),'')<>''
			THEN 1 ELSE 0 END,
		active_attempt_id='',result_id='',publication_id='',revision_envelope_json=?,updated_at=?
		WHERE id=? AND status='quarantined' AND active_attempt_id=? AND generation=?`, boolToSQLiteInt(quarantineIdentityAdoptionRequired), encodeAgentTaskJSON(revisionSource), now, fence.TaskID, fence.AttemptID, fence.Generation)
	if err != nil {
		return nil, err
	}
	if affected, affectedErr := updated.RowsAffected(); affectedErr != nil || affected != 1 {
		return nil, errors.New("stale_lease_fence: quarantine task proof lost its exact state")
	}
	if err := l.appendEventTx(ctx, tx, fence.TaskID, fence.AttemptID, "queued", "Gateway operator verified process-group termination and requeued a new fenced revision", map[string]any{
		"termination_verified": true, "reason": reason, "previous_status": "quarantined", "next_generation": fence.Generation + 1,
		"worker_identity_adoption_required": quarantineIdentityAdoptionRequired, "claim_eligible": !quarantineIdentityAdoptionRequired,
	}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{
		"task": mustTask(l, ctx, fence.TaskID), "attempt": mustAttempt(l, ctx, fence.AttemptID),
		"termination_verified": true, "requeued": true, "idempotent_replay": false,
	}, nil
}

func (l *agentTaskDeliveryLedger) observe(ctx context.Context, fence agentTaskFence, runnerStatus string, exitCode *int, metadata map[string]any) (map[string]any, error) {
	runnerStatus = strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(runnerStatus, "execution_observed")))
	if err := agentTaskValidateText(runnerStatus, "runner_status", 512); err != nil {
		return nil, err
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	if err := agentTaskValidateStructured(metadata, "runner observation metadata", l.limits.EventBytes/2); err != nil {
		return nil, err
	}
	targetStatus := "execution_observed"
	observedExit := exitCode != nil
	observedExitCode := 0
	if exitCode != nil {
		observedExitCode = *exitCode
	}
	if runnerStatus == "failed" || runnerStatus == "execution_failed" || (observedExit && observedExitCode != 0) {
		targetStatus = "execution_failed"
	}
	observation := map[string]any{
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID,
		"worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID, "generation": fence.Generation,
		"runner_status": runnerStatus, "exit_code": observedExitCode, "exit_observed": observedExit,
		"target_status": targetStatus, "metadata": cloneAnyMap(metadata),
	}
	if err := agentTaskValidateStructured(observation, "runner observation", l.limits.EventBytes); err != nil {
		return nil, err
	}
	observationDigest := agentTaskDigest(observation)
	if observationDigest == "" {
		return nil, errors.New("runner observation is not canonically serializable")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	fencedAttempt, err := l.fenceTx(ctx, tx, fence, false)
	if err != nil {
		return nil, err
	}
	var currentTaskStatus string
	var maxExecutionAttempts int
	if err := tx.QueryRowContext(ctx, `SELECT status,max_execution_attempts FROM task_ledger_tasks WHERE id=?`, fence.TaskID).Scan(&currentTaskStatus, &maxExecutionAttempts); err != nil {
		return nil, err
	}
	currentAttemptStatus := anyToString(fencedAttempt["status"])
	var attemptNumber int
	var storedObservationDigest, storedFailureDisposition, revisionEnvelopeJSON string
	if err := tx.QueryRowContext(ctx, `SELECT attempt_number,observation_digest,failure_disposition,revision_envelope_json FROM task_ledger_attempts WHERE attempt_id=?`, fence.AttemptID).Scan(&attemptNumber, &storedObservationDigest, &storedFailureDisposition, &revisionEnvelopeJSON); err != nil {
		return nil, err
	}
	if currentAttemptStatus == "execution_observed" || currentAttemptStatus == "execution_failed" {
		if storedObservationDigest == "" || storedObservationDigest != observationDigest || currentAttemptStatus != targetStatus {
			return nil, errors.New("immutable runner observation replay does not match the recorded outcome")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return map[string]any{"task": mustTask(l, ctx, fence.TaskID), "attempt": mustAttempt(l, ctx, fence.AttemptID), "execution_observed": targetStatus == "execution_observed", "failure_disposition": storedFailureDisposition, "idempotent_replay": true}, nil
	}
	if currentAttemptStatus != "leased" && currentAttemptStatus != "running" && currentAttemptStatus != "waiting_for_input" {
		return nil, fmt.Errorf("stale_lease_fence: attempt status %s is not mutable", currentAttemptStatus)
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, anyToString(fencedAttempt["lease_expires_at"]))
	if parseErr != nil || !time.Now().UTC().Before(expires) {
		return nil, errors.New("stale_lease_fence: lease expired")
	}
	if !agentTaskAllowedTransition(currentTaskStatus, targetStatus) {
		return nil, fmt.Errorf("invalid task transition %s -> %s", currentTaskStatus, targetStatus)
	}
	now := agentTaskNow()
	failureDisposition := ""
	retryIdentityAdoptionRequired := false
	if targetStatus == "execution_failed" {
		if attemptNumber < maxExecutionAttempts {
			failureDisposition = "retry_queued"
			if fence.WorkerIdentityUpdateGeneration == 0 {
				bound, bindingErr := workerIdentityGenerationZeroSourceBoundTx(ctx, tx, fence.TaskID, fence.WorkerID, fence.WorkerInstanceID)
				if bindingErr != nil {
					return nil, bindingErr
				}
				retryIdentityAdoptionRequired = !bound
			}
		} else {
			failureDisposition = "execution_dead_letter"
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET status=?,runner_status=?,runner_exit_code=?,runner_exit_observed=?,observation_digest=?,failure_disposition=?,completed_at=? WHERE attempt_id=?`, targetStatus, runnerStatus, observedExitCode, boolToSQLiteInt(observedExit), observationDigest, failureDisposition, now, fence.AttemptID); err != nil {
		return nil, err
	}
	if targetStatus == "execution_failed" {
		updated, updateErr := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='execution_failed',claim_eligible=0,updated_at=? WHERE id=? AND active_attempt_id=? AND status=?`, now, fence.TaskID, fence.AttemptID, currentTaskStatus)
		if updateErr != nil {
			return nil, updateErr
		}
		if affected, affectedErr := updated.RowsAffected(); affectedErr != nil || affected != 1 {
			return nil, errors.New("stale_lease_fence: execution observation lost the active task state")
		}
	}
	if err := l.appendEventTx(ctx, tx, fence.TaskID, fence.AttemptID, targetStatus, "runner outcome recorded as execution observation", map[string]any{"runner_status": runnerStatus, "exit_code": observedExitCode, "exit_observed": observedExit, "metadata": cloneAnyMap(metadata)}); err != nil {
		return nil, err
	}
	if l.executionRetryHook != nil {
		if err := l.executionRetryHook("after_attempt_observation"); err != nil {
			return nil, err
		}
	}
	if targetStatus == "execution_failed" {
		if failureDisposition == "retry_queued" {
			revisionSource, err := agentTaskRevisionSourceForRequeue(decodeAgentTaskMap(revisionEnvelopeJSON), fence)
			if err != nil {
				return nil, fmt.Errorf("restore failed revision instructions: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='queued',claim_eligible=CASE WHEN ?<>0 THEN 0 ELSE 1 END,active_attempt_id='',revision_envelope_json=?,updated_at=? WHERE id=? AND active_attempt_id=? AND status='execution_failed'`, boolToSQLiteInt(retryIdentityAdoptionRequired), encodeAgentTaskJSON(revisionSource), now, fence.TaskID, fence.AttemptID); err != nil {
				return nil, err
			}
			if err := l.appendEventTx(ctx, tx, fence.TaskID, fence.AttemptID, "queued", "failed execution attempt requeued within the bounded retry budget", map[string]any{"attempt_number": attemptNumber, "max_execution_attempts": maxExecutionAttempts, "next_generation": fence.Generation + 1, "observation_digest": observationDigest, "worker_identity_adoption_required": retryIdentityAdoptionRequired, "claim_eligible": !retryIdentityAdoptionRequired}); err != nil {
				return nil, err
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='dead_letter',claim_eligible=0,updated_at=? WHERE id=? AND active_attempt_id=? AND status='execution_failed'`, now, fence.TaskID, fence.AttemptID); err != nil {
				return nil, err
			}
			if err := l.appendEventTx(ctx, tx, fence.TaskID, fence.AttemptID, "dead_letter", "execution retry budget exhausted; task moved to explicit execution dead letter", map[string]any{"attempt_number": attemptNumber, "max_execution_attempts": maxExecutionAttempts, "observation_digest": observationDigest, "dead_letter_kind": "execution"}); err != nil {
				return nil, err
			}
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='execution_observed',claim_eligible=0,updated_at=? WHERE id=? AND active_attempt_id=?`, now, fence.TaskID, fence.AttemptID); err != nil {
		return nil, err
	}
	if l.executionRetryHook != nil {
		if err := l.executionRetryHook("before_observation_commit"); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return map[string]any{"task": mustTask(l, ctx, fence.TaskID), "attempt": mustAttempt(l, ctx, fence.AttemptID), "execution_observed": targetStatus == "execution_observed", "failure_disposition": failureDisposition}, nil
}

func mustTask(l *agentTaskDeliveryLedger, ctx context.Context, taskID string) map[string]any {
	row, _ := l.queryTask(ctx, taskID)
	return row
}

func mustAttempt(l *agentTaskDeliveryLedger, ctx context.Context, attemptID string) map[string]any {
	row, _ := l.attempt(ctx, attemptID)
	return row
}

func agentTaskArtifactBytes(raw map[string]any) ([]byte, bool, error) {
	if rawEncoded, exists := raw["content_base64"]; exists {
		encoded, ok := rawEncoded.(string)
		if !ok {
			return nil, false, errors.New("artifact content_base64 must be a string")
		}
		encoded = strings.TrimSpace(encoded)
		encodedBound := base64.StdEncoding.EncodedLen(agentTaskArtifactMaxBytes)
		if len(encoded) > encodedBound {
			return nil, false, errors.New("artifact content_base64 exceeds the configured encoded bound")
		}
		content, err := base64.StdEncoding.DecodeString(encoded)
		if len(content) > agentTaskArtifactMaxBytes {
			return nil, false, errors.New("artifact content exceeds the configured bound")
		}
		return content, true, err
	}
	if rawContent, exists := raw["content"]; exists {
		content, ok := rawContent.(string)
		if !ok {
			return nil, false, errors.New("artifact content must be a string")
		}
		if len(content) > agentTaskArtifactMaxBytes {
			return nil, false, errors.New("artifact content exceeds the configured bound")
		}
		return []byte(content), true, nil
	}
	return nil, false, nil
}

func readAgentTaskArtifactFileBounded(path string, maxBytes int64) ([]byte, error) {
	file, err := openAgentTaskFileNoFollow(path, maxBytes)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > maxBytes {
		return nil, errors.New("artifact content exceeds the configured bound")
	}
	return content, nil
}

type agentTaskArtifactHandle struct {
	file   *os.File
	path   string
	digest string
	size   int64
	stat   agentTaskFileStat
}

func (h *agentTaskArtifactHandle) close() error {
	if h == nil || h.file == nil {
		return nil
	}
	return h.file.Close()
}

func (h *agentTaskArtifactHandle) readAndVerify(maxBytes int64) ([]byte, error) {
	if h == nil || h.file == nil {
		return nil, errors.New("artifact descriptor is unavailable")
	}
	before, err := validateAgentTaskRegularFile(h.file, maxBytes)
	if err != nil {
		return nil, err
	}
	if _, err := h.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	content, err := io.ReadAll(io.LimitReader(h.file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != before.Size || int64(len(content)) > maxBytes {
		return nil, errors.New("artifact descriptor size changed during bounded verification")
	}
	if agentTaskBytesDigest(content) != h.digest {
		return nil, errors.New("artifact content failed immutable digest verification")
	}
	after, err := validateAgentTaskRegularFile(h.file, maxBytes)
	if err != nil {
		return nil, err
	}
	if before.Device != after.Device || before.FileID != after.FileID || before.Size != after.Size {
		return nil, errors.New("artifact descriptor changed during immutable verification")
	}
	h.stat = after
	h.size = after.Size
	_, err = h.file.Seek(0, io.SeekStart)
	return content, err
}

func (l *agentTaskDeliveryLedger) openArtifactDescriptor(digest string, maxBytes int64) (*agentTaskArtifactHandle, error) {
	path, err := l.artifactPath(digest)
	if err != nil {
		return nil, err
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	root, err := openAgentTaskDirectoryNoFollow(l.artifactRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	shard, err := openAgentTaskDirectoryAt(root, hexDigest[:2], false)
	if err != nil {
		return nil, err
	}
	defer shard.Close()
	file, err := openAgentTaskFileAt(shard, hexDigest+".blob", agentTaskFileReadOnly, 0, maxBytes)
	if err != nil {
		return nil, err
	}
	stat, err := validateAgentTaskRegularFile(file, maxBytes)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &agentTaskArtifactHandle{file: file, path: path, digest: digest, size: stat.Size, stat: stat}, nil
}

func (h *agentTaskArtifactHandle) revalidateBinding(l *agentTaskDeliveryLedger, maxBytes int64) error {
	if h == nil || h.file == nil {
		return errors.New("artifact descriptor is unavailable for binding")
	}
	if _, err := l.artifactPath(h.digest); err != nil || h.size < 0 || h.size > maxBytes {
		return errors.New("artifact descriptor lacks a canonical digest and size binding")
	}
	held, err := validateAgentTaskRegularFile(h.file, maxBytes)
	if err != nil {
		return err
	}
	// readAndVerify captured h.stat only after a bounded full digest read. Any
	// same-inode write, timestamp change, relink, chmod, or replacement changes
	// this complete stat identity and fails without doing file I/O in the
	// IMMEDIATE transaction.
	if !reflect.DeepEqual(held, h.stat) || held.Size != h.size {
		return errors.New("artifact digest-verified descriptor changed before immutable binding")
	}
	current, err := l.openArtifactDescriptor(h.digest, maxBytes)
	if err != nil {
		return err
	}
	defer current.close()
	if !reflect.DeepEqual(current.stat, held) {
		return errors.New("artifact canonical path no longer names the verified immutable descriptor")
	}
	return nil
}

func agentTaskArtifactDigest(raw map[string]any, content []byte, contentProvided bool) (string, error) {
	digest := strings.TrimSpace(anyToString(raw["digest"]))
	contentRef := strings.TrimSpace(anyToString(raw["content_ref"]))
	if digest != "" && contentRef != "" && digest != contentRef {
		return "", errors.New("artifact digest and content_ref must identify the same immutable bytes")
	}
	if digest == "" {
		digest = contentRef
	}
	if digest == "" && contentProvided {
		digest = agentTaskBytesDigest(content)
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", errors.New("artifact digest must be sha256:<64 lowercase hex characters>")
	}
	for _, ch := range digest[len("sha256:"):] {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return "", errors.New("artifact digest is not canonical lowercase sha256")
		}
	}
	if contentProvided && digest != agentTaskBytesDigest(content) {
		return "", errors.New("artifact checksum mismatch")
	}
	if rawSize, exists := raw["size_bytes"]; contentProvided && exists && int64(anyToInt(rawSize, -1)) != int64(len(content)) {
		return "", errors.New("artifact size_bytes does not match the verified immutable content")
	}
	return digest, nil
}

func (l *agentTaskDeliveryLedger) artifactPath(digest string) (string, error) {
	digest = strings.TrimPrefix(strings.TrimSpace(digest), "sha256:")
	if len(digest) != 64 {
		return "", errors.New("artifact digest is invalid")
	}
	for _, ch := range digest {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return "", errors.New("artifact digest is invalid")
		}
	}
	return filepath.Join(l.artifactRoot, digest[:2], digest+".blob"), nil
}

func (l *agentTaskDeliveryLedger) persistArtifactContent(digest string, content []byte, contentProvided bool) (*agentTaskArtifactHandle, error) {
	path, err := l.artifactPath(digest)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > l.limits.MaxArtifactBytes {
		return nil, errors.New("artifact content exceeds the configured bound")
	}
	if !contentProvided {
		handle, openErr := l.openArtifactDescriptor(digest, l.limits.MaxArtifactBytes)
		if openErr != nil {
			return nil, fmt.Errorf("artifact content is not present in the bounded artifact store: %w", openErr)
		}
		if _, verifyErr := handle.readAndVerify(l.limits.MaxArtifactBytes); verifyErr != nil {
			_ = handle.close()
			return nil, verifyErr
		}
		return handle, nil
	}
	if agentTaskBytesDigest(content) != digest {
		return nil, errors.New("artifact content does not match its immutable digest")
	}
	if existing, openErr := l.openArtifactDescriptor(digest, l.limits.MaxArtifactBytes); openErr == nil {
		if _, verifyErr := existing.readAndVerify(l.limits.MaxArtifactBytes); verifyErr != nil {
			_ = existing.close()
			return nil, verifyErr
		}
		return existing, nil
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return nil, openErr
	}
	hexDigest := strings.TrimPrefix(digest, "sha256:")
	root, err := openAgentTaskDirectoryNoFollow(l.artifactRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	shard, err := openAgentTaskDirectoryAt(root, hexDigest[:2], true)
	if err != nil {
		return nil, err
	}
	defer shard.Close()
	canonicalName := hexDigest + ".blob"
	var contentHandle *agentTaskArtifactHandle
	for attempt := 0; attempt < agentTaskIDCollisionRetries; attempt++ {
		tempID, idErr := newAgentTaskID("blobtmp")
		if idErr != nil {
			return nil, idErr
		}
		tempName := "." + tempID + ".tmp"
		temp, openErr := openAgentTaskFileAt(shard, tempName, agentTaskFileReadWriteCreateExclusive, 0o600, l.limits.MaxArtifactBytes)
		if errors.Is(openErr, os.ErrExist) {
			continue
		}
		if openErr != nil {
			return nil, openErr
		}
		cleanupTemp := true
		func() {
			defer func() {
				if cleanupTemp {
					_ = agentTaskUnlinkAt(shard, tempName)
					_ = temp.Close()
				}
			}()
			if _, err = temp.Write(content); err != nil {
				return
			}
			if err = temp.Sync(); err != nil {
				return
			}
			handle := &agentTaskArtifactHandle{file: temp, path: path, digest: digest, size: int64(len(content))}
			if _, err = handle.readAndVerify(l.limits.MaxArtifactBytes); err != nil {
				return
			}
			if linkErr := agentTaskLinkAt(shard, temp, tempName, canonicalName); linkErr != nil {
				if !errors.Is(linkErr, os.ErrExist) {
					err = linkErr
					return
				}
				_ = agentTaskUnlinkAt(shard, tempName)
				_ = temp.Close()
				cleanupTemp = false
				existing, existingErr := l.openArtifactDescriptor(digest, l.limits.MaxArtifactBytes)
				if existingErr != nil {
					err = existingErr
					return
				}
				if _, existingErr = existing.readAndVerify(l.limits.MaxArtifactBytes); existingErr != nil {
					_ = existing.close()
					err = existingErr
					return
				}
				handle = existing
			} else {
				if unlinkErr := agentTaskUnlinkAt(shard, tempName); unlinkErr != nil {
					err = unlinkErr
					return
				}
				cleanupTemp = false
				if _, statErr := validateAgentTaskRegularFile(temp, l.limits.MaxArtifactBytes); statErr != nil {
					err = statErr
					_ = temp.Close()
					return
				}
			}
			if syncErr := agentTaskSyncDirectory(shard); syncErr != nil {
				_ = handle.close()
				err = syncErr
				return
			}
			cleanupTemp = false
			content = nil
			// Reuse the named return channel through a local closure variable.
			path = handle.path
			contentHandle = handle
		}()
		if err != nil {
			return nil, fmt.Errorf("persist artifact content: %w", err)
		}
		if contentHandle != nil {
			return contentHandle, nil
		}
	}
	return nil, fmt.Errorf("persist artifact content: %w", errAgentTaskIDCollision)
}

func agentTaskScanCanonicalSecrets(content []byte, maxBytes int64, subject string) (string, error) {
	subject = firstNonEmptyStrings(strings.TrimSpace(subject), "task content")
	if int64(len(content)) > maxBytes {
		return "", fmt.Errorf("%s exceeds bounded scan size", subject)
	}
	filter := writeSecretFilterResult{Mode: "block"}
	_ = scrubWriteSecrets(string(content), &filter, 0)
	if filter.Findings > 0 {
		return "", fmt.Errorf("%s rejected by canonical Gateway secret boundary", subject)
	}
	contentDigest := agentTaskBytesDigest(content)
	receipt := agentTaskDigest(map[string]any{"scanner": "gateway-go-write-secret-filter.v1", "subject": subject, "content_digest": contentDigest, "result": "clean", "findings": 0})
	if receipt == "" {
		return "", fmt.Errorf("%s redaction receipt is not canonically serializable", subject)
	}
	return receipt, nil
}

func agentTaskScanArtifact(content []byte) (string, error) {
	return agentTaskScanCanonicalSecrets(content, agentTaskArtifactMaxBytes, "artifact")
}

func verifyAgentTaskArtifactFile(path, digest string, expectedSize, maxBytes int64) error {
	file, err := openAgentTaskFileNoFollow(path, maxBytes)
	if err != nil {
		return err
	}
	before, err := validateAgentTaskRegularFile(file, maxBytes)
	if err != nil {
		_ = file.Close()
		return err
	}
	defer file.Close()
	if expectedSize < 0 || expectedSize > maxBytes {
		return errors.New("artifact size exceeds configured bound")
	}
	hasher := sha256.New()
	read, err := io.CopyN(hasher, file, maxBytes+1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if read != expectedSize {
		return errors.New("artifact content size does not match immutable metadata")
	}
	actual := "sha256:" + hex.EncodeToString(hasher.Sum(nil))
	if actual != digest {
		return errors.New("artifact content failed immutable digest verification")
	}
	after, err := validateAgentTaskRegularFile(file, maxBytes)
	if err != nil {
		return err
	}
	if before.Device != after.Device || before.FileID != after.FileID || before.Size != after.Size {
		return errors.New("artifact descriptor changed during immutable verification")
	}
	return nil
}

type preparedAgentTaskArtifact struct {
	artifactID         string
	artifactIDSupplied bool
	name               string
	digest             string
	mediaType          string
	retention          string
	redaction          string
	redactionReceipt   string
	content            []byte
	contentProvided    bool
	sizeBytes          int64
	accessPolicy       map[string]any
	handle             *agentTaskArtifactHandle
	payload            map[string]any
}

type preparedAgentTaskDelivery struct {
	deliveryID string
	recipient  map[string]any
	dedupeKey  string
	payload    map[string]any
}

type preparedAgentTaskPublication struct {
	namespaceLease        *agentTaskArtifactNamespaceLease
	task                  map[string]any
	attempt               map[string]any
	stateDigest           string
	resultID              string
	resultIDSupplied      bool
	publicationID         string
	publicationIDSupplied bool
	idempotencyKey        string
	submittedIntentDigest string
	intentDigest          string
	result                map[string]any
	resultDigest          string
	writebackIntent       map[string]any
	artifacts             []preparedAgentTaskArtifact
	deliveries            []preparedAgentTaskDelivery
	existingPublicationID string
}

func (p *preparedAgentTaskPublication) close() {
	if p == nil {
		return
	}
	for index := range p.artifacts {
		_ = p.artifacts[index].handle.close()
	}
	_ = p.namespaceLease.close()
}

func agentTaskPublicationStateDigest(task, attempt map[string]any) string {
	return agentTaskDigest(map[string]any{
		"task": map[string]any{
			"task_id": task["task_id"], "project": task["project"], "workspace_id": task["workspace_id"],
			"status": task["status"], "generation": task["generation"], "active_attempt_id": task["active_attempt_id"],
			"result_id": task["result_id"], "publication_id": task["publication_id"], "execution_profile": task["execution_profile"],
			"risk_level": task["risk_level"], "context_request": task["context_request"], "recipients": task["recipients"],
			"review_owner": task["review_owner"], "requesting_agent_id": task["requesting_agent_id"],
		},
		"attempt": map[string]any{
			"task_id": attempt["task_id"], "attempt_id": attempt["attempt_id"], "lease_id": attempt["lease_id"],
			"generation": attempt["generation"], "worker_id": attempt["worker_id"], "worker_instance_id": attempt["worker_instance_id"],
			"status": attempt["status"], "context_pack_hash": attempt["context_pack_hash"], "session_id": attempt["session_id"],
			"lease_expires_at": attempt["lease_expires_at"], "runner_exit_observed": attempt["runner_exit_observed"],
			"runner_status": attempt["runner_status"], "observation_digest": attempt["observation_digest"],
		},
	})
}

func agentTaskPublicationIdempotencyKey(request, result map[string]any, resultID string) (string, error) {
	topLevel := strings.TrimSpace(anyToString(request["idempotency_key"]))
	resultLevel := strings.TrimSpace(anyToString(result["idempotency_key"]))
	if topLevel != "" && resultLevel != "" && topLevel != resultLevel {
		return "", errors.New("publication idempotency key copies do not match exactly")
	}
	key := firstNonEmptyStrings(topLevel, resultLevel, "task-result:"+strings.TrimSpace(resultID))
	if strings.TrimSpace(key) == "" {
		return "", errors.New("publication idempotency key is required")
	}
	if err := agentTaskValidateText(key, "idempotency_key", 2048); err != nil {
		return "", err
	}
	return key, nil
}

// agentTaskPublicationSubmittedIntentDigest reduces every accepted request
// shape to the caller-owned immutable material. Server-allocated IDs and
// derived result fields are intentionally excluded here; the stored result
// digest binds those values in agentTaskPublicationIntentDigest below. This
// lets two concurrently prepared copies with omitted IDs converge on the
// winning row while still rejecting any different key, body, artifact bytes,
// or exact worker fence.
func agentTaskPublicationRunnerExitRequired(request, task map[string]any) bool {
	if _, explicitlySet := request["runner_exit_required"]; explicitlySet {
		return anyToBool(request["runner_exit_required"])
	}
	profile := strings.ToLower(anyToString(task["execution_profile"]))
	return profile == "" || strings.Contains(profile, "local") || strings.Contains(profile, "runner") || strings.Contains(profile, "shell")
}

func agentTaskPublicationSubmittedIntentDigest(request map[string]any, fence agentTaskFence, idempotencyKey string, runnerExitRequired bool, maxArtifacts int) (string, error) {
	resultInput := cloneAnyMap(anyMap(request["result"]))
	if len(resultInput) == 0 {
		resultInput = cloneAnyMap(request)
		for _, field := range []string{"fence", "result", "artifacts", "runner_exit_required", "task_id", "attempt_id", "lease_id", "worker_id", "worker_instance_id", "generation", "assignment_generation", "lease_generation"} {
			delete(resultInput, field)
		}
	}
	requestedContextHash := strings.TrimSpace(firstNonEmptyStrings(anyToString(resultInput["context_pack_hash"]), anyToString(anyMap(request["attempt"])["context_pack_hash"])))
	delete(resultInput, "attempt")
	if requestedContextHash != "" {
		resultInput["context_pack_hash"] = requestedContextHash
	}
	artifacts := anySlice(request["artifacts"])
	if len(artifacts) == 0 {
		artifacts = anySlice(resultInput["artifacts"])
	}
	if len(artifacts) > maxArtifacts {
		return "", fmt.Errorf("artifact reference count exceeds %d", maxArtifacts)
	}
	for _, field := range []string{"schema_id", "contract_version", "format_contract", "result_id", "publication_id", "task_id", "attempt_id", "status", "execution_observed", "idempotency_key", "artifacts"} {
		delete(resultInput, field)
	}
	output := strings.TrimSpace(anyToString(resultInput["output"]))
	summary := strings.TrimSpace(firstNonEmptyStrings(anyToString(resultInput["summary"]), output))
	resultInput["summary"] = summary
	resultInput["output"] = output
	if err := agentTaskValidateStructured(resultInput, "publication submitted result intent", agentTaskContextPackMaxBytes); err != nil {
		return "", err
	}
	artifactIntent := make([]any, 0, len(artifacts))
	for _, raw := range artifacts {
		artifact := cloneAnyMap(anyMap(raw))
		if len(artifact) == 0 {
			return "", errors.New("artifact descriptor must be an object")
		}
		content, contentProvided, err := agentTaskArtifactBytes(artifact)
		if err != nil {
			return "", fmt.Errorf("artifact content decode: %w", err)
		}
		digest, err := agentTaskArtifactDigest(artifact, content, contentProvided)
		if err != nil {
			return "", err
		}
		delete(artifact, "content")
		delete(artifact, "content_base64")
		artifact["digest"] = digest
		if contentProvided {
			artifact["size_bytes"] = len(content)
		}
		if err := agentTaskValidateStructured(artifact, "publication submitted artifact intent", agentTaskEventMaxBytes); err != nil {
			return "", err
		}
		artifactIntent = append(artifactIntent, artifact)
	}
	intent := map[string]any{
		"schema_id":            "agent_task_publication_submitted_intent.v1",
		"fence":                fencePayload(fence),
		"idempotency_key":      idempotencyKey,
		"runner_exit_required": runnerExitRequired,
		"payload":              resultInput,
		"output":               output,
		"artifacts":            artifactIntent,
	}
	if err := agentTaskValidateStructured(intent, "publication submitted intent", agentTaskContextPackMaxBytes+agentTaskEventMaxBytes); err != nil {
		return "", err
	}
	digest := agentTaskDigest(intent)
	if digest == "" {
		return "", errors.New("publication submitted intent is not canonically serializable")
	}
	return digest, nil
}

func agentTaskPublicationIntentDigest(submittedIntentDigest, resultDigest string) string {
	return agentTaskDigest(map[string]any{
		"schema_id":               "agent_task_publication_intent.v1",
		"submitted_intent_digest": submittedIntentDigest,
		"result_digest":           resultDigest,
	})
}

func (l *agentTaskDeliveryLedger) preparePublication(ctx context.Context, fence agentTaskFence, request map[string]any) (_ *preparedAgentTaskPublication, resultErr error) {
	controls := cloneAnyMap(request)
	delete(controls, "result")
	delete(controls, "artifacts")
	if err := agentTaskValidateStructured(controls, "publication controls", agentTaskEventMaxBytes*4); err != nil {
		return nil, err
	}
	task, err := l.queryTask(ctx, fence.TaskID)
	if err != nil {
		return nil, err
	}
	attempt, err := l.attempt(ctx, fence.AttemptID)
	if err != nil {
		return nil, err
	}
	if anyToString(attempt["task_id"]) != fence.TaskID || anyToString(attempt["lease_id"]) != fence.LeaseID || anyToInt(attempt["generation"], 0) != fence.Generation || anyToString(attempt["worker_id"]) != fence.WorkerID || anyToString(attempt["worker_instance_id"]) != fence.WorkerInstanceID {
		return nil, errors.New("stale_lease_fence: task, attempt, lease, worker, or generation mismatch")
	}
	if anyToString(task["active_attempt_id"]) != fence.AttemptID {
		return nil, errors.New("stale_lease_fence: result publication attempt is not active")
	}
	runnerExitRequired := agentTaskPublicationRunnerExitRequired(request, task)
	requestedResultID := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["result_id"]), anyToString(anyMap(request["result"])["result_id"])))
	requestedPublicationID := strings.TrimSpace(firstNonEmptyStrings(anyToString(request["publication_id"]), anyToString(anyMap(request["result"])["publication_id"])))
	if storedPublicationID := strings.TrimSpace(anyToString(task["publication_id"])); storedPublicationID != "" {
		storedResultID := strings.TrimSpace(anyToString(task["result_id"]))
		if requestedPublicationID != "" && requestedPublicationID != storedPublicationID {
			return nil, errors.New("task already owns a different immutable result publication")
		}
		if requestedResultID != "" && requestedResultID != storedResultID {
			return nil, errors.New("task already owns a different immutable result")
		}
		requestedKey, keyErr := agentTaskPublicationIdempotencyKey(request, anyMap(request["result"]), storedResultID)
		if keyErr != nil {
			return nil, keyErr
		}
		submittedIntentDigest, intentErr := agentTaskPublicationSubmittedIntentDigest(request, fence, requestedKey, runnerExitRequired, l.limits.MaxArtifactReferences)
		if intentErr != nil {
			return nil, intentErr
		}
		var storedKey, storedTaskID, storedAttemptID, storedIntentDigest, storedResultDigest string
		if err := l.db.QueryRowContext(ctx, `SELECT p.idempotency_key,p.task_id,p.attempt_id,p.intent_digest,r.digest FROM task_ledger_publications p JOIN task_ledger_results r ON r.result_id=p.result_id AND r.task_id=p.task_id AND r.attempt_id=p.attempt_id WHERE p.publication_id=? AND p.result_id=?`, storedPublicationID, storedResultID).Scan(&storedKey, &storedTaskID, &storedAttemptID, &storedIntentDigest, &storedResultDigest); err != nil {
			return nil, err
		}
		if storedKey != requestedKey || storedTaskID != fence.TaskID || storedAttemptID != fence.AttemptID {
			return nil, errors.New("publication replay does not match the exact immutable idempotency binding")
		}
		if storedIntentDigest == "" || storedIntentDigest != agentTaskPublicationIntentDigest(submittedIntentDigest, storedResultDigest) {
			return nil, errors.New("publication replay does not match the canonical immutable publication intent")
		}
		return &preparedAgentTaskPublication{task: task, attempt: attempt, existingPublicationID: storedPublicationID}, nil
	}
	if anyToString(attempt["status"]) != "execution_observed" {
		return nil, fmt.Errorf("runner result requires execution_observed attempt; got %s", anyToString(attempt["status"]))
	}
	if anyToString(task["status"]) != "execution_observed" {
		return nil, fmt.Errorf("result publication requires execution_observed task; got %s", anyToString(task["status"]))
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, anyToString(attempt["lease_expires_at"]))
	if parseErr != nil || !time.Now().UTC().Before(expires) {
		return nil, errors.New("stale_lease_fence: lease expired before result publication was staged")
	}
	if runnerExitRequired && !anyToBool(attempt["runner_exit_observed"]) {
		return nil, errors.New("runner exit observation is required before publication")
	}
	prepared := &preparedAgentTaskPublication{task: task, attempt: attempt, stateDigest: agentTaskPublicationStateDigest(task, attempt)}
	if prepared.stateDigest == "" {
		return nil, errors.New("publication state snapshot is not canonically serializable")
	}
	defer func() {
		if resultErr != nil {
			prepared.close()
		}
	}()
	resultInput := cloneAnyMap(anyMap(request["result"]))
	if len(resultInput) == 0 {
		resultInput = cloneAnyMap(request)
		delete(resultInput, "fence")
		delete(resultInput, "result")
		delete(resultInput, "artifacts")
	}
	artifacts := anySlice(request["artifacts"])
	if len(artifacts) == 0 {
		artifacts = anySlice(resultInput["artifacts"])
	}
	delete(resultInput, "artifacts")
	if len(artifacts) > l.limits.MaxArtifactReferences {
		return nil, fmt.Errorf("artifact reference count exceeds %d", l.limits.MaxArtifactReferences)
	}
	prepared.resultIDSupplied = requestedResultID != ""
	prepared.resultID = requestedResultID
	if prepared.resultID == "" {
		prepared.resultID, err = l.newUniqueID(ctx, l.db, "result", "result")
		if err != nil {
			return nil, err
		}
	}
	prepared.publicationIDSupplied = requestedPublicationID != ""
	prepared.publicationID = requestedPublicationID
	if prepared.publicationID == "" {
		prepared.publicationID, err = l.newUniqueID(ctx, l.db, "publication", "publication")
		if err != nil {
			return nil, err
		}
	}
	prepared.idempotencyKey, err = agentTaskPublicationIdempotencyKey(request, resultInput, prepared.resultID)
	if err != nil {
		return nil, err
	}
	prepared.submittedIntentDigest, err = agentTaskPublicationSubmittedIntentDigest(request, fence, prepared.idempotencyKey, runnerExitRequired, l.limits.MaxArtifactReferences)
	if err != nil {
		return nil, err
	}
	// Idempotency belongs to the mutable publication record, not the immutable
	// result manifest. Normalize both accepted input locations to one ledger key
	// so route shape cannot change the result digest.
	delete(resultInput, "idempotency_key")
	for field, value := range map[string]string{"result_id": prepared.resultID, "publication_id": prepared.publicationID} {
		if err := agentTaskValidateText(value, field, 2048); err != nil {
			return nil, err
		}
	}
	resultInput["schema_id"] = agentTaskResultManifestContractID
	resultInput["contract_version"] = 1
	resultInput["result_id"] = prepared.resultID
	resultInput["task_id"] = fence.TaskID
	resultInput["attempt_id"] = fence.AttemptID
	resultInput["status"] = "publication_pending"
	resultInput["execution_observed"] = true
	summary := firstNonEmptyStrings(anyToString(resultInput["summary"]), anyToString(resultInput["output"]))
	if err := agentTaskValidateText(summary, "result summary", l.limits.SummaryBytes); err != nil {
		return nil, err
	}
	output := anyToString(resultInput["output"])
	if err := agentTaskValidateText(output, "result output", l.limits.NotificationBytes); err != nil {
		return nil, err
	}
	resultInput["summary"] = strings.TrimSpace(summary)
	resultInput["output"] = strings.TrimSpace(output)
	requestedContextHash := strings.TrimSpace(firstNonEmptyStrings(anyToString(resultInput["context_pack_hash"]), anyToString(anyMap(request["attempt"])["context_pack_hash"])))
	if anyToString(attempt["context_pack_hash"]) == "" || requestedContextHash != anyToString(attempt["context_pack_hash"]) {
		return nil, errors.New("result context_pack_hash does not match the server-owned attempt snapshot")
	}
	if strings.TrimSpace(anyToString(attempt["session_id"])) == "" {
		return nil, errors.New("server-owned attempt session linkage is required for publication")
	}
	resultInput["context_pack_hash"] = anyToString(attempt["context_pack_hash"])
	resultInput["publication_id"] = prepared.publicationID
	resultInput["artifacts"] = []any{}
	resultInput = agentTaskContractPayload(agentTaskResultManifestContractID, resultInput)
	if err := agentTaskRequireContract(agentTaskResultManifestContractID, resultInput); err != nil {
		return nil, err
	}
	if err := agentTaskValidateStructured(resultInput, "result manifest", agentTaskSummaryMaxBytes+agentTaskNotificationMaxBytes+agentTaskEventMaxBytes*4); err != nil {
		return nil, err
	}
	// The runner's cleanup target is caller-controlled manifest material. Bind
	// and validate it before any clean artifact byte is written so a malformed
	// cleanup contract cannot leave an unreferenced content-addressed blob.
	if _, _, err := agentTaskResultCleanupBinding(resultInput, fence.TaskID, fence.AttemptID); err != nil {
		return nil, err
	}
	if len(artifacts) > 0 {
		prepared.namespaceLease, err = l.lockArtifactNamespaceContext(ctx, false)
		if err != nil {
			return nil, fmt.Errorf("lock task artifact writer: %w", err)
		}
	}
	recipients := []string{}
	for _, recipient := range agentTaskRecipientRows(task) {
		recipients = append(recipients, anyToString(recipient["principal_id"]))
	}
	now := time.Now().UTC()
	seenDigests := map[string]bool{}
	var artifactTotal int64
	for _, raw := range artifacts {
		artifact := cloneAnyMap(anyMap(raw))
		if len(artifact) == 0 {
			return nil, errors.New("artifact descriptor must be an object")
		}
		descriptor := cloneAnyMap(artifact)
		delete(descriptor, "content")
		delete(descriptor, "content_base64")
		if err := agentTaskValidateStructured(descriptor, "artifact descriptor", agentTaskEventMaxBytes); err != nil {
			return nil, err
		}
		content, contentProvided, contentErr := agentTaskArtifactBytes(artifact)
		if contentErr != nil {
			return nil, fmt.Errorf("artifact content decode: %w", contentErr)
		}
		digest, digestErr := agentTaskArtifactDigest(artifact, content, contentProvided)
		if digestErr != nil {
			return nil, digestErr
		}
		if seenDigests[digest] {
			return nil, errors.New("artifact set contains a duplicate immutable digest")
		}
		seenDigests[digest] = true
		var handle *agentTaskArtifactHandle
		if !contentProvided {
			handle, contentErr = l.openArtifactDescriptor(digest, l.limits.MaxArtifactBytes)
			if contentErr == nil {
				content, contentErr = handle.readAndVerify(l.limits.MaxArtifactBytes)
			}
			if contentErr != nil {
				_ = handle.close()
				return nil, fmt.Errorf("artifact reference is not available with its exact digest: %w", contentErr)
			}
		}
		sizeBytes := int64(len(content))
		if rawSize, exists := artifact["size_bytes"]; exists && int64(anyToInt(rawSize, -1)) != sizeBytes {
			_ = handle.close()
			return nil, errors.New("artifact size_bytes does not match the verified immutable content")
		}
		if sizeBytes > l.limits.MaxArtifactBytes {
			_ = handle.close()
			return nil, fmt.Errorf("artifact exceeds %d byte limit", l.limits.MaxArtifactBytes)
		}
		artifactTotal += sizeBytes
		if artifactTotal > l.limits.MaxArtifactSetBytes {
			_ = handle.close()
			return nil, fmt.Errorf("attempt artifacts exceed %d byte limit", l.limits.MaxArtifactSetBytes)
		}
		redactionReceipt, scanErr := agentTaskScanArtifact(content)
		if scanErr != nil {
			_ = handle.close()
			return nil, scanErr
		}
		artifactID := strings.TrimSpace(anyToString(artifact["artifact_id"]))
		artifactIDSupplied := artifactID != ""
		if artifactID == "" {
			artifactID, err = l.newUniqueID(ctx, l.db, "artifact", "artifact")
			if err != nil {
				_ = handle.close()
				return nil, err
			}
		}
		if err := agentTaskValidateText(artifactID, "artifact_id", 2048); err != nil {
			_ = handle.close()
			return nil, err
		}
		name := strings.TrimSpace(firstNonEmptyStrings(anyToString(artifact["name"]), "artifact"))
		if name == "" {
			_ = handle.close()
			return nil, errors.New("artifact name is required")
		}
		if err := agentTaskValidateText(name, "artifact name", 2048); err != nil {
			_ = handle.close()
			return nil, err
		}
		retention := strings.TrimSpace(anyToString(artifact["retention_expires_at"]))
		if retention == "" {
			retention = now.Add(time.Duration(l.limits.RetentionDays) * 24 * time.Hour).Format(time.RFC3339Nano)
		}
		retentionTime, retentionErr := time.Parse(time.RFC3339Nano, retention)
		if retentionErr != nil || !retentionTime.After(now) || retentionTime.After(now.Add(366*24*time.Hour)) {
			_ = handle.close()
			return nil, errors.New("artifact retention timestamp is invalid or outside the bounded retention window")
		}
		mediaType := firstNonEmptyStrings(anyToString(artifact["media_type"]), "application/octet-stream")
		if err := agentTaskValidateText(mediaType, "artifact media_type", 512); err != nil {
			_ = handle.close()
			return nil, err
		}
		accessPolicy := map[string]any{"project": anyToString(task["project"]), "recipients": recipients, "expires_at": retention, "authority": "gateway-go"}
		payload := agentTaskContractPayload(agentTaskArtifactContractID, map[string]any{"schema_id": agentTaskArtifactContractID, "artifact_id": artifactID, "task_id": fence.TaskID, "attempt_id": fence.AttemptID, "name": name, "digest": digest, "size_bytes": sizeBytes, "media_type": mediaType, "redaction_status": "unverified", "access_policy": accessPolicy, "retention_expires_at": retention})
		if err := agentTaskRequireContract(agentTaskArtifactContractID, payload); err != nil {
			_ = handle.close()
			return nil, err
		}
		if err := agentTaskValidateStructured(payload, "artifact evidence", agentTaskEventMaxBytes*2); err != nil {
			_ = handle.close()
			return nil, err
		}
		prepared.artifacts = append(prepared.artifacts, preparedAgentTaskArtifact{artifactID: artifactID, artifactIDSupplied: artifactIDSupplied, name: name, digest: digest, mediaType: mediaType, retention: retention, redaction: "unverified", redactionReceipt: redactionReceipt, content: content, contentProvided: contentProvided, sizeBytes: sizeBytes, accessPolicy: accessPolicy, handle: handle, payload: payload})
	}
	// All descriptors and all bytes have now passed the canonical secret and
	// bounds boundary. Only now may clean content be written to the namespace.
	for index := range prepared.artifacts {
		artifact := &prepared.artifacts[index]
		if artifact.contentProvided {
			artifact.handle, err = l.persistArtifactContent(artifact.digest, artifact.content, true)
			if err != nil {
				return nil, err
			}
		}
		verified, verifyErr := artifact.handle.readAndVerify(l.limits.MaxArtifactBytes)
		if verifyErr != nil || int64(len(verified)) != artifact.sizeBytes {
			return nil, firstNonEmptyError(verifyErr, errors.New("artifact size changed after persistence"))
		}
		artifact.content = nil
	}
	if l.artifactStageHook != nil {
		if err := l.artifactStageHook("after_artifact_writes"); err != nil {
			return nil, err
		}
	}
	artifactRows := make([]any, 0, len(prepared.artifacts))
	for index := range prepared.artifacts {
		artifactRows = append(artifactRows, prepared.artifacts[index].payload)
	}
	resultInput["artifacts"] = artifactRows
	resultInput = agentTaskContractPayload(agentTaskResultManifestContractID, resultInput)
	if err := agentTaskRequireContract(agentTaskResultManifestContractID, resultInput); err != nil {
		return nil, err
	}
	if err := agentTaskValidateStructured(resultInput, "immutable result manifest", agentTaskContextPackMaxBytes); err != nil {
		return nil, err
	}
	prepared.result = resultInput
	prepared.resultDigest = agentTaskDigest(resultInput)
	if prepared.resultDigest == "" {
		return nil, errors.New("result manifest is not canonically serializable")
	}
	prepared.intentDigest = agentTaskPublicationIntentDigest(prepared.submittedIntentDigest, prepared.resultDigest)
	if prepared.intentDigest == "" {
		return nil, errors.New("publication intent is not canonically serializable")
	}
	contextRequest := anyMap(task["context_request"])
	writebackIntent := map[string]any{
		"schema_id": agentTaskWritebackIntentContractID, "contract_version": 1,
		"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "result_id": prepared.resultID,
		"project": anyToString(task["project"]), "session_id": anyToString(attempt["session_id"]),
		"topic_path": anyToString(contextRequest["topic_path"]), "required": true,
		"summary": agentTaskBoundedStringMust(anyToString(resultInput["summary"]), 2048), "result_digest": prepared.resultDigest,
		"worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID,
		"assignment_generation": fence.Generation, "lease_generation": fence.Generation,
		"requesting_agent_id": anyToString(task["requesting_agent_id"]), "review_agent_id": anyToString(task["review_owner"]),
	}
	writebackKey := agentTaskDigest(map[string]any{"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "result_id": prepared.resultID})
	writebackDigest := strings.TrimPrefix(writebackKey, "sha256:")
	writebackIntent["file_name"] = "notes/agent-tasks/" + writebackDigest + ".md"
	if strings.TrimSpace(anyToString(writebackIntent["topic_path"])) == "" {
		writebackIntent["topic_path"] = "tasks/" + writebackDigest
	}
	writebackIntent["idempotency_key"] = writebackKey
	writebackIntent = agentTaskContractPayload(agentTaskWritebackIntentContractID, writebackIntent)
	if err := agentTaskRequireContract(agentTaskWritebackIntentContractID, writebackIntent); err != nil {
		return nil, err
	}
	if err := agentTaskValidateStructured(writebackIntent, "writeback intent", agentTaskEventMaxBytes*2); err != nil {
		return nil, err
	}
	prepared.writebackIntent = writebackIntent
	recipientRows := agentTaskRecipientRows(task)
	if len(recipientRows) == 0 {
		return nil, errors.New("publication requires at least one recipient")
	}
	for _, recipient := range recipientRows {
		deliveryID, err := l.newUniqueID(ctx, l.db, "delivery", "delivery")
		if err != nil {
			return nil, err
		}
		principal := anyToString(recipient["principal_id"])
		dedupe := prepared.resultID + ":" + principal
		notice := agentTaskContractPayload(agentTaskDeliveryContractID, map[string]any{"schema_id": agentTaskDeliveryContractID, "contract_version": 1, "delivery_id": deliveryID, "result_id": prepared.resultID, "task_id": fence.TaskID, "recipient": recipient, "reviewer_owner": anyToString(task["review_owner"]), "status": "pending", "dedupe_key": dedupe, "summary": anyToString(resultInput["summary"]), "required_action": "review", "attempts": 0, "next_action": "deliver_continuation_inbox", "risk_level": task["risk_level"], "artifact_references": artifactRows})
		if err := agentTaskRequireContract(agentTaskDeliveryContractID, notice); err != nil {
			return nil, err
		}
		if err := agentTaskValidateStructured(notice, "delivery notice", agentTaskContextPackMaxBytes); err != nil {
			return nil, err
		}
		prepared.deliveries = append(prepared.deliveries, preparedAgentTaskDelivery{deliveryID: deliveryID, recipient: recipient, dedupeKey: dedupe, payload: notice})
	}
	return prepared, nil
}

func (l *agentTaskDeliveryLedger) commitPreparedPublication(ctx context.Context, fence agentTaskFence, prepared *preparedAgentTaskPublication) (string, error) {
	if prepared.existingPublicationID != "" {
		return prepared.existingPublicationID, nil
	}
	// Re-hash every descriptor immediately before the short transaction. The
	// held descriptors remain pinned through commit; inside the transaction we
	// only compare inode/size bindings and immutable state/digest descriptors.
	for index := range prepared.artifacts {
		verified, err := prepared.artifacts[index].handle.readAndVerify(l.limits.MaxArtifactBytes)
		if err != nil || int64(len(verified)) != prepared.artifacts[index].sizeBytes {
			return "", firstNonEmptyError(err, errors.New("artifact failed final pre-transaction verification"))
		}
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := l.fenceTx(ctx, tx, fence, false); err != nil {
		return "", err
	}
	task, err := l.queryTaskTx(ctx, tx, fence.TaskID)
	if err != nil {
		return "", err
	}
	var existingPublicationID, existingResultID, existingTaskID, existingAttemptID string
	var existingIdempotencyKey, existingIntentDigest, existingResultDigest string
	lookupErr := tx.QueryRowContext(ctx, `SELECT p.publication_id,p.result_id,p.task_id,p.attempt_id,p.idempotency_key,p.intent_digest,r.digest FROM task_ledger_publications p JOIN task_ledger_results r ON r.result_id=p.result_id AND r.task_id=p.task_id AND r.attempt_id=p.attempt_id WHERE p.idempotency_key=? OR p.result_id=? ORDER BY CASE WHEN p.idempotency_key=? THEN 0 ELSE 1 END LIMIT 1`, prepared.idempotencyKey, prepared.resultID, prepared.idempotencyKey).Scan(&existingPublicationID, &existingResultID, &existingTaskID, &existingAttemptID, &existingIdempotencyKey, &existingIntentDigest, &existingResultDigest)
	if lookupErr == nil {
		if existingTaskID != fence.TaskID || existingAttemptID != fence.AttemptID || existingIdempotencyKey != prepared.idempotencyKey || prepared.resultIDSupplied && existingResultID != prepared.resultID || prepared.publicationIDSupplied && existingPublicationID != prepared.publicationID {
			return "", errors.New("publication idempotency key is already bound to different immutable task evidence")
		}
		winnerIntentDigest := agentTaskPublicationIntentDigest(prepared.submittedIntentDigest, existingResultDigest)
		if existingIntentDigest == "" || existingIntentDigest != winnerIntentDigest {
			return "", errors.New("publication idempotency replay does not match the canonical immutable publication intent")
		}
		if err := tx.Commit(); err != nil {
			return "", err
		}
		return existingPublicationID, nil
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return "", lookupErr
	}
	attempt, err := l.attemptTx(ctx, tx, fence.AttemptID)
	if err != nil {
		return "", err
	}
	if agentTaskPublicationStateDigest(task, attempt) != prepared.stateDigest {
		return "", errors.New("stale_lease_fence: task or attempt state changed during publication preparation")
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, anyToString(attempt["lease_expires_at"]))
	if parseErr != nil || !time.Now().UTC().Before(expires) {
		return "", errors.New("stale_lease_fence: lease expired before publication commit")
	}
	for index := range prepared.artifacts {
		if err := prepared.artifacts[index].handle.revalidateBinding(l, l.limits.MaxArtifactBytes); err != nil {
			return "", err
		}
	}
	for _, id := range []struct {
		kind, value string
		generated   bool
	}{
		{kind: "result", value: prepared.resultID, generated: !prepared.resultIDSupplied},
		{kind: "publication", value: prepared.publicationID, generated: !prepared.publicationIDSupplied},
	} {
		if err := l.ensureIDAvailable(ctx, tx, id.kind, id.value); err != nil {
			if errors.Is(err, errAgentTaskIDCollision) && id.generated {
				return "", errAgentTaskIDCollision
			}
			return "", fmt.Errorf("immutable %s id already exists", id.kind)
		}
	}
	for index := range prepared.artifacts {
		artifact := &prepared.artifacts[index]
		if err := l.ensureIDAvailable(ctx, tx, "artifact", artifact.artifactID); err != nil {
			if errors.Is(err, errAgentTaskIDCollision) && !artifact.artifactIDSupplied {
				return "", errAgentTaskIDCollision
			}
			return "", errors.New("immutable artifact id already exists")
		}
	}
	for _, delivery := range prepared.deliveries {
		if err := l.ensureIDAvailable(ctx, tx, "delivery", delivery.deliveryID); err != nil {
			return "", errAgentTaskIDCollision
		}
	}
	now := agentTaskNow()
	for index := range prepared.artifacts {
		artifact := &prepared.artifacts[index]
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_artifacts(artifact_id,task_id,attempt_id,name,digest,size_bytes,media_type,redaction_status,redaction_receipt,access_policy_json,retention_expires_at,content_ref,content_path,finalized,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, artifact.artifactID, fence.TaskID, fence.AttemptID, artifact.name, artifact.digest, artifact.sizeBytes, artifact.mediaType, artifact.redaction, artifact.redactionReceipt, encodeAgentTaskJSON(artifact.accessPolicy), artifact.retention, artifact.digest, artifact.handle.path, 0, now); err != nil {
			return "", fmt.Errorf("stage artifact: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_results(result_id,task_id,attempt_id,schema_id,status,execution_observed,payload_json,digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, prepared.resultID, fence.TaskID, fence.AttemptID, agentTaskResultManifestContractID, "publication_pending", 1, encodeAgentTaskJSON(prepared.result), prepared.resultDigest, now); err != nil {
		return "", fmt.Errorf("stage immutable result: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_publications(publication_id,result_id,task_id,attempt_id,idempotency_key,intent_digest,status,writeback_status,writeback_intent_json,delivery_row_count,recovery_owner,next_action,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, prepared.publicationID, prepared.resultID, fence.TaskID, fence.AttemptID, prepared.idempotencyKey, prepared.intentDigest, "writeback_pending", "pending", encodeAgentTaskJSON(prepared.writebackIntent), len(prepared.deliveries), "gateway-publication-worker", "retry_writeback", now, now); err != nil {
		return "", fmt.Errorf("stage publication: %w", err)
	}
	for _, delivery := range prepared.deliveries {
		recipient := delivery.recipient
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_deliveries(delivery_id,publication_id,result_id,task_id,recipient_id,role,observer,session_id,reviewer_owner,status,dedupe_key,attempts,payload_json,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, delivery.deliveryID, prepared.publicationID, prepared.resultID, fence.TaskID, anyToString(recipient["principal_id"]), anyToString(recipient["role"]), anyToBool(recipient["observer"]), anyToString(recipient["session_id"]), anyToString(task["review_owner"]), "pending", delivery.dedupeKey, 0, encodeAgentTaskJSON(delivery.payload), now, now); err != nil {
			return "", fmt.Errorf("stage delivery row: %w", err)
		}
	}
	update, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='writeback_pending',claim_eligible=0,result_id=?,publication_id=?,updated_at=? WHERE id=? AND status='execution_observed' AND active_attempt_id=? AND generation=?`, prepared.resultID, prepared.publicationID, now, fence.TaskID, fence.AttemptID, fence.Generation)
	if err != nil {
		return "", err
	}
	if affected, err := update.RowsAffected(); err != nil || affected != 1 {
		return "", errors.New("stale_lease_fence: publication lost the exact task state fence")
	}
	if err := l.appendEventTx(ctx, tx, fence.TaskID, fence.AttemptID, "writeback_pending", "immutable result, artifacts, writeback intent, and delivery rows staged", map[string]any{"result_id": prepared.resultID, "publication_id": prepared.publicationID, "delivery_row_count": len(prepared.deliveries), "result_digest": prepared.resultDigest, "publication_intent_digest": prepared.intentDigest}); err != nil {
		return "", err
	}
	if l.artifactStageHook != nil {
		if err := l.artifactStageHook("before_publication_commit"); err != nil {
			return "", err
		}
	}
	// The transaction is still short, but the path-to-descriptor binding must
	// be the final pre-commit check. A same-size rename/replacement after the
	// first check must roll back every ledger row rather than bind foreign bytes.
	for index := range prepared.artifacts {
		if err := prepared.artifacts[index].handle.revalidateBinding(l, l.limits.MaxArtifactBytes); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return prepared.publicationID, nil
}

func (l *agentTaskDeliveryLedger) stagePublication(ctx context.Context, fence agentTaskFence, request map[string]any) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"task_id": fence.TaskID, "attempt_id": fence.AttemptID, "lease_id": fence.LeaseID, "worker_id": fence.WorkerID, "worker_instance_id": fence.WorkerInstanceID, "generation": fence.Generation}, "publication fence", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	for collisionAttempt := 0; collisionAttempt < agentTaskIDCollisionRetries; collisionAttempt++ {
		prepared, err := l.preparePublication(ctx, fence, request)
		if err != nil {
			return nil, err
		}
		publicationID, commitErr := l.commitPreparedPublication(ctx, fence, prepared)
		prepared.close()
		if errors.Is(commitErr, errAgentTaskIDCollision) {
			continue
		}
		if commitErr != nil {
			return nil, commitErr
		}
		return l.publication(ctx, publicationID)
	}
	return nil, fmt.Errorf("%w after %d publication attempts", errAgentTaskIDCollision, agentTaskIDCollisionRetries)
}

func anySlice(value any) []any {
	switch typed := value.(type) {
	case []any:
		return typed
	case []map[string]any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, item)
		}
		return out
	default:
		return []any{}
	}
}

type agentTaskPublicationBoundaryIdentity struct {
	PublicationID        string
	ResultID             string
	ResultDigest         string
	PublicationStatus    string
	TaskID               string
	AttemptID            string
	LeaseID              string
	WorkerID             string
	WorkerInstanceID     string
	IdempotencyKey       string
	WorkspaceRef         string
	CleanupID            string
	AssignmentGeneration int
	LeaseGeneration      int
}

func agentTaskPayloadHasOnly(payload map[string]any, allowed ...string) bool {
	if payload == nil || len(payload) != len(allowed) {
		return false
	}
	set := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		set[field] = struct{}{}
	}
	for field := range payload {
		if _, ok := set[field]; !ok {
			return false
		}
	}
	return true
}

func agentTaskDigestExcluding(payload map[string]any, digestField string) string {
	material := cloneAnyMap(payload)
	delete(material, digestField)
	return agentTaskDigest(material)
}

func agentTaskCanonicalMapEqual(left, right map[string]any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}

func agentTaskPublicationBoundary(identity agentTaskPublicationBoundaryIdentity) (map[string]any, map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{
		"publication_id": identity.PublicationID, "result_id": identity.ResultID, "result_digest": identity.ResultDigest,
		"task_id": identity.TaskID, "attempt_id": identity.AttemptID, "lease_id": identity.LeaseID,
		"worker_id": identity.WorkerID, "worker_instance_id": identity.WorkerInstanceID,
		"idempotency_key": identity.IdempotencyKey, "assignment_generation": identity.AssignmentGeneration,
		"lease_generation": identity.LeaseGeneration, "workspace_ref": identity.WorkspaceRef, "cleanup_id": identity.CleanupID,
		"publication_status": identity.PublicationStatus,
	}, "publication receipt identity", agentTaskEventMaxBytes*2); err != nil {
		return nil, nil, err
	}
	if identity.PublicationID == "" || identity.ResultID == "" || identity.TaskID == "" || identity.AttemptID == "" || identity.LeaseID == "" || identity.WorkerID == "" || identity.WorkerInstanceID == "" || identity.IdempotencyKey == "" || identity.WorkspaceRef == "" || identity.CleanupID == "" || !agentTaskCanonicalSHA256(identity.ResultDigest) || identity.AssignmentGeneration <= 0 || identity.LeaseGeneration <= 0 || identity.AssignmentGeneration != identity.LeaseGeneration {
		return nil, nil, errors.New("publication receipt identity is incomplete or inconsistent")
	}
	expectedCleanupID := agentTaskCleanupID(identity.TaskID, identity.AttemptID, identity.WorkspaceRef)
	if identity.CleanupID != expectedCleanupID {
		return nil, nil, errors.New("publication cleanup identity does not match the immutable task workspace")
	}
	// The receipt proves the immutable staging boundary and therefore must not
	// change when the mutable outbox status advances or exhausts retries. This
	// keeps restart reconciliation byte-for-byte identical to the publish
	// response while still rejecting rows outside the durable state machine.
	switch identity.PublicationStatus {
	case "writeback_pending", "writeback_failed", "committed", "dead_letter":
	default:
		return nil, nil, errors.New("publication is not in a cleanup-authorizable durable state")
	}
	receiptState := "staged"
	receiptIDMaterial := agentTaskDigest(map[string]any{"publication_id": identity.PublicationID, "result_id": identity.ResultID, "task_id": identity.TaskID, "attempt_id": identity.AttemptID})
	authorizationIDMaterial := agentTaskDigest(map[string]any{"cleanup_id": identity.CleanupID, "publication_id": identity.PublicationID, "attempt_id": identity.AttemptID})
	receipt := map[string]any{
		"schema_id":  agentTaskPublicationReceiptID,
		"receipt_id": "publication-receipt-" + strings.TrimPrefix(receiptIDMaterial, "sha256:")[:32],
		"authority":  "gateway-go-sqlite-wal", "durable": true, "state": receiptState,
		"publication_id": identity.PublicationID, "result_id": identity.ResultID,
		"task_id": identity.TaskID, "attempt_id": identity.AttemptID, "lease_id": identity.LeaseID,
		"generation": identity.LeaseGeneration, "worker_id": identity.WorkerID, "worker_instance_id": identity.WorkerInstanceID,
	}
	receipt["receipt_digest"] = agentTaskDigest(receipt)
	authorization := map[string]any{
		"schema_id":        agentTaskCleanupAuthorizationID,
		"authorization_id": "cleanup-authorization-" + strings.TrimPrefix(authorizationIDMaterial, "sha256:")[:32],
		"authority":        "gateway-go-sqlite-wal", "authorized": true, "attempt_terminal": true, "durable": true,
		"state": "authorized", "cleanup_id": identity.CleanupID, "workspace_ref": identity.WorkspaceRef,
		"publication_id": identity.PublicationID, "result_id": identity.ResultID,
		"task_id": identity.TaskID, "attempt_id": identity.AttemptID, "lease_id": identity.LeaseID,
		"generation": identity.LeaseGeneration, "worker_id": identity.WorkerID, "worker_instance_id": identity.WorkerInstanceID,
	}
	authorization["authorization_digest"] = agentTaskDigest(authorization)
	if err := verifyAgentTaskPublicationReceipt(receipt); err != nil {
		return nil, nil, err
	}
	if err := verifyAgentTaskCleanupAuthorization(authorization); err != nil {
		return nil, nil, err
	}
	return receipt, authorization, nil
}

func verifyAgentTaskPublicationReceipt(receipt map[string]any) error {
	if !agentTaskPayloadHasOnly(receipt, "schema_id", "receipt_id", "receipt_digest", "authority", "durable", "state", "publication_id", "result_id", "task_id", "attempt_id", "lease_id", "generation", "worker_id", "worker_instance_id") {
		return errors.New("publication_receipt is not a closed contract")
	}
	if anyToString(receipt["schema_id"]) != agentTaskPublicationReceiptID || anyToString(receipt["receipt_id"]) == "" || anyToString(receipt["authority"]) != "gateway-go-sqlite-wal" || !anyToBool(receipt["durable"]) || (anyToString(receipt["state"]) != "staged" && anyToString(receipt["state"]) != "committed") {
		return errors.New("publication_receipt contract flags are invalid")
	}
	digest := strings.TrimSpace(anyToString(receipt["receipt_digest"]))
	if !agentTaskCanonicalSHA256(digest) || digest != agentTaskDigestExcluding(receipt, "receipt_digest") {
		return errors.New("publication_receipt digest does not match its closed material")
	}
	return agentTaskValidateStructured(receipt, "publication_receipt", agentTaskEventMaxBytes*2)
}

func verifyAgentTaskCleanupAuthorization(authorization map[string]any) error {
	if !agentTaskPayloadHasOnly(authorization, "schema_id", "authorization_id", "authorization_digest", "authority", "authorized", "attempt_terminal", "durable", "state", "cleanup_id", "workspace_ref", "publication_id", "result_id", "task_id", "attempt_id", "lease_id", "generation", "worker_id", "worker_instance_id") {
		return errors.New("cleanup_authorization is not a closed contract")
	}
	if anyToString(authorization["schema_id"]) != agentTaskCleanupAuthorizationID || anyToString(authorization["authorization_id"]) == "" || anyToString(authorization["authority"]) != "gateway-go-sqlite-wal" || !anyToBool(authorization["authorized"]) || !anyToBool(authorization["attempt_terminal"]) || !anyToBool(authorization["durable"]) || anyToString(authorization["state"]) != "authorized" {
		return errors.New("cleanup_authorization contract flags are invalid")
	}
	digest := strings.TrimSpace(anyToString(authorization["authorization_digest"]))
	if !agentTaskCanonicalSHA256(digest) || digest != agentTaskDigestExcluding(authorization, "authorization_digest") {
		return errors.New("cleanup_authorization digest does not match its closed material")
	}
	return agentTaskValidateStructured(authorization, "cleanup_authorization", agentTaskEventMaxBytes*2)
}

func agentTaskCleanupID(taskID, attemptID, workspaceRef string) string {
	sum := sha256.Sum256([]byte(taskID + "\x00" + attemptID + "\x00" + workspaceRef))
	return "cleanup-" + hex.EncodeToString(sum[:])[:32]
}

func agentTaskResultCleanupBinding(result map[string]any, taskID, attemptID string) (string, string, error) {
	workspaceRef := strings.TrimSpace(firstNonEmptyStrings(
		anyToString(anyMap(result["workspace"])["workspace_ref"]),
		anyToString(anyMap(result["coding"])["workspace_ref"]),
		anyToString(anyMap(result["non_coding"])["workspace_ref"]),
	))
	cleanupID := strings.TrimSpace(anyToString(anyMap(result["cleanup"])["cleanup_id"]))
	if workspaceRef == "" || cleanupID == "" || cleanupID != agentTaskCleanupID(taskID, attemptID, workspaceRef) {
		return "", "", errors.New("result manifest lacks an exact cleanup workspace binding")
	}
	if err := agentTaskValidateStructured(map[string]any{"workspace_ref": workspaceRef, "cleanup_id": cleanupID}, "result cleanup binding", agentTaskEventMaxBytes); err != nil {
		return "", "", err
	}
	return workspaceRef, cleanupID, nil
}

func (l *agentTaskDeliveryLedger) publication(ctx context.Context, publicationID string) (map[string]any, error) {
	var (
		resultID, taskID, attemptID, idempotencyKey, intentDigest, status, writebackStatus, writebackRef, writebackIntentJSON, recoveryOwner, nextAction, lastError, createdAt, updatedAt string
		deliveryCount                                                                                                                                                                     int
	)
	err := l.db.QueryRowContext(ctx, `SELECT publication_id,result_id,task_id,attempt_id,idempotency_key,intent_digest,status,writeback_status,writeback_ref,writeback_intent_json,delivery_row_count,recovery_owner,next_action,last_error,created_at,updated_at FROM task_ledger_publications WHERE publication_id=?`, strings.TrimSpace(publicationID)).Scan(&publicationID, &resultID, &taskID, &attemptID, &idempotencyKey, &intentDigest, &status, &writebackStatus, &writebackRef, &writebackIntentJSON, &deliveryCount, &recoveryOwner, &nextAction, &lastError, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	resultPayload, resultErr := l.resultPayload(ctx, resultID)
	if resultErr != nil {
		return nil, resultErr
	}
	var leaseID, workerID, workerInstanceID, attemptTaskID string
	var generation int
	if err := l.db.QueryRowContext(ctx, `SELECT lease_id,generation,worker_id,worker_instance_id,task_id FROM task_ledger_attempts WHERE attempt_id=?`, attemptID).Scan(&leaseID, &generation, &workerID, &workerInstanceID, &attemptTaskID); err != nil {
		return nil, err
	}
	if attemptTaskID != taskID || anyToString(resultPayload["task_id"]) != taskID || anyToString(resultPayload["attempt_id"]) != attemptID {
		return nil, errors.New("publication evidence does not bind to one immutable task attempt")
	}
	workspaceRef, cleanupID, bindingErr := agentTaskResultCleanupBinding(resultPayload, taskID, attemptID)
	if bindingErr != nil {
		return nil, bindingErr
	}
	publicationReceipt, cleanupAuthorization, boundaryErr := agentTaskPublicationBoundary(agentTaskPublicationBoundaryIdentity{
		PublicationID: publicationID, ResultID: resultID, ResultDigest: anyToString(resultPayload["result_digest"]),
		TaskID: taskID, AttemptID: attemptID, LeaseID: leaseID, WorkerID: workerID, WorkerInstanceID: workerInstanceID,
		IdempotencyKey: idempotencyKey, WorkspaceRef: workspaceRef, CleanupID: cleanupID, PublicationStatus: status,
		AssignmentGeneration: generation, LeaseGeneration: generation,
	})
	if boundaryErr != nil {
		return nil, boundaryErr
	}
	deliveries, deliveryErr := l.deliveries(ctx, taskID, resultID)
	if deliveryErr != nil {
		return nil, deliveryErr
	}
	payload := map[string]any{
		"schema_id": agentTaskPublicationContractID, "contract_version": 1, "publication_id": publicationID,
		"result_id": resultID, "task_id": taskID, "attempt_id": attemptID, "status": status,
		"writeback_status": writebackStatus, "writeback_ref": writebackRef, "delivery_row_count": deliveryCount,
		"idempotency_key": idempotencyKey, "intent_digest": intentDigest, "recovery_owner": recoveryOwner, "next_action": nextAction,
		"lease_id": leaseID, "generation": generation, "assignment_generation": generation, "lease_generation": generation,
		"worker_id": workerID, "worker_instance_id": workerInstanceID,
		"publication_receipt": publicationReceipt, "cleanup_authorization": cleanupAuthorization,
		"writeback_intent": decodeAgentTaskMap(writebackIntentJSON), "last_error": lastError, "created_at": createdAt, "updated_at": updatedAt, "result": resultPayload, "deliveries": deliveries,
	}
	return agentTaskContractPayload(agentTaskPublicationContractID, payload), nil
}

func (l *agentTaskDeliveryLedger) publicationForExactFence(ctx context.Context, fence agentTaskFence, idempotencyKey string) (map[string]any, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if err := agentTaskValidateStructured(map[string]any{"fence": fencePayload(fence), "idempotency_key": idempotencyKey}, "publication reconciliation fence", agentTaskEventMaxBytes*2); err != nil {
		return nil, err
	}
	if idempotencyKey == "" {
		return nil, errors.New("publication reconciliation requires the exact idempotency key")
	}
	identity := agentTaskPublicationBoundaryIdentity{}
	var writebackStatus, resultJSON string
	var resultObserved, resultImmutable int
	err := l.db.QueryRowContext(ctx, `SELECT p.publication_id,p.result_id,p.idempotency_key,p.status,p.writeback_status,
		p.task_id,p.attempt_id,a.lease_id,a.generation,a.generation,a.worker_id,a.worker_instance_id,
		r.digest,r.payload_json,r.execution_observed,r.immutable
		FROM task_ledger_publications p
		JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id
		JOIN task_ledger_results r ON r.result_id=p.result_id AND r.task_id=p.task_id AND r.attempt_id=p.attempt_id
		WHERE p.task_id=? AND p.attempt_id=?`, fence.TaskID, fence.AttemptID).Scan(
		&identity.PublicationID, &identity.ResultID, &identity.IdempotencyKey, &identity.PublicationStatus, &writebackStatus,
		&identity.TaskID, &identity.AttemptID, &identity.LeaseID, &identity.AssignmentGeneration, &identity.LeaseGeneration,
		&identity.WorkerID, &identity.WorkerInstanceID, &identity.ResultDigest, &resultJSON, &resultObserved, &resultImmutable,
	)
	if err != nil {
		return nil, err
	}
	if identity.IdempotencyKey != idempotencyKey || identity.TaskID != fence.TaskID || identity.AttemptID != fence.AttemptID || identity.LeaseID != fence.LeaseID || identity.LeaseGeneration != fence.Generation || identity.AssignmentGeneration != fence.Generation || identity.WorkerID != fence.WorkerID || identity.WorkerInstanceID != fence.WorkerInstanceID {
		return nil, errors.New("stale_lease_fence: publication reconciliation identity does not match immutable evidence")
	}
	result := decodeAgentTaskMap(resultJSON)
	if resultObserved != 1 || resultImmutable != 1 || anyToString(result["task_id"]) != identity.TaskID || anyToString(result["attempt_id"]) != identity.AttemptID || agentTaskDigest(result) != identity.ResultDigest {
		return nil, errors.New("publication reconciliation is not backed by exact immutable result evidence")
	}
	identity.WorkspaceRef, identity.CleanupID, err = agentTaskResultCleanupBinding(result, identity.TaskID, identity.AttemptID)
	if err != nil {
		return nil, err
	}
	publicationReceipt, cleanupAuthorization, err := agentTaskPublicationBoundary(identity)
	if err != nil {
		return nil, err
	}
	expectedWritebackStatus := map[string]string{
		"writeback_pending": "pending",
		"writeback_failed":  "failed",
		"committed":         "committed",
		"dead_letter":       "dead_letter",
	}[identity.PublicationStatus]
	if expectedWritebackStatus == "" || writebackStatus != expectedWritebackStatus || anyToString(publicationReceipt["state"]) != "staged" {
		return nil, errors.New("publication reconciliation state is outside the canonical cleanup-authorizable matrix")
	}
	reconciliation := map[string]any{
		"schema_id":      agentTaskPublicationReconciliationID,
		"publication_id": identity.PublicationID, "result_id": identity.ResultID, "idempotency_key": identity.IdempotencyKey,
		"task_id": identity.TaskID, "attempt_id": identity.AttemptID, "lease_id": identity.LeaseID,
		"generation": identity.LeaseGeneration, "assignment_generation": identity.AssignmentGeneration, "lease_generation": identity.LeaseGeneration,
		"worker_id": identity.WorkerID, "worker_instance_id": identity.WorkerInstanceID,
		"status": identity.PublicationStatus, "writeback_status": writebackStatus,
		"publication_receipt": publicationReceipt, "cleanup_authorization": cleanupAuthorization,
	}
	if err := agentTaskRequireContract(agentTaskPublicationReconciliationID, reconciliation); err != nil {
		return nil, err
	}
	return reconciliation, nil
}

func verifyAgentTaskCleanupReceipt(receipt map[string]any) error {
	baseFields := []string{"schema_id", "receipt_id", "receipt_digest", "authority", "state", "cleanup_id", "workspace_ref", "publication_id", "result_id", "task_id", "attempt_id", "lease_id", "generation", "worker_id", "worker_instance_id"}
	acknowledged := false
	if _, exists := receipt["recorded"]; exists {
		acknowledged = true
		baseFields = append(baseFields, "recorded", "durable", "acknowledged")
	}
	if !agentTaskPayloadHasOnly(receipt, baseFields...) {
		return errors.New("cleanup receipt is not a closed contract")
	}
	if anyToString(receipt["schema_id"]) != agentTaskCleanupReceiptID || anyToString(receipt["receipt_id"]) == "" || anyToString(receipt["authority"]) != "task-execution-worker" || anyToString(receipt["state"]) != "cleaned" || (acknowledged && (!anyToBool(receipt["recorded"]) || !anyToBool(receipt["durable"]) || !anyToBool(receipt["acknowledged"]))) {
		return errors.New("cleanup receipt contract flags are invalid")
	}
	digest := strings.TrimSpace(anyToString(receipt["receipt_digest"]))
	digestMaterial := cloneAnyMap(receipt)
	delete(digestMaterial, "receipt_digest")
	delete(digestMaterial, "recorded")
	delete(digestMaterial, "durable")
	delete(digestMaterial, "acknowledged")
	if !agentTaskCanonicalSHA256(digest) || digest != agentTaskDigest(digestMaterial) {
		return errors.New("cleanup receipt digest does not match its closed material")
	}
	return agentTaskValidateStructured(receipt, "cleanup receipt", agentTaskEventMaxBytes*2)
}

func (l *agentTaskDeliveryLedger) acknowledgeCleanup(ctx context.Context, taskID, attemptID string, request map[string]any) (map[string]any, error) {
	if err := agentTaskValidateStructured(request, "cleanup acknowledgement request", agentTaskEventMaxBytes*4); err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	attemptID = strings.TrimSpace(attemptID)
	receipt := cloneAnyMap(anyMap(request["cleanup_receipt"]))
	if len(receipt) == 0 && anyToString(request["schema_id"]) == agentTaskCleanupReceiptID {
		receipt = cloneAnyMap(request)
	}
	if err := verifyAgentTaskCleanupReceipt(receipt); err != nil {
		return nil, err
	}
	if taskID == "" || attemptID == "" || anyToString(receipt["task_id"]) != taskID || anyToString(receipt["attempt_id"]) != attemptID {
		return nil, errors.New("cleanup receipt does not match the requested task attempt")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	identity := agentTaskPublicationBoundaryIdentity{}
	var resultObserved, resultImmutable int
	var resultJSON string
	lookupErr := tx.QueryRowContext(ctx, `SELECT p.publication_id,p.result_id,p.task_id,p.attempt_id,p.idempotency_key,p.status,r.digest,r.payload_json,a.lease_id,a.generation,a.generation,a.worker_id,a.worker_instance_id,r.execution_observed,r.immutable
		FROM task_ledger_publications p
		JOIN task_ledger_results r ON r.result_id=p.result_id AND r.task_id=p.task_id AND r.attempt_id=p.attempt_id
		JOIN task_ledger_attempts a ON a.attempt_id=p.attempt_id AND a.task_id=p.task_id
		WHERE p.publication_id=?`, anyToString(receipt["publication_id"])).Scan(
		&identity.PublicationID, &identity.ResultID, &identity.TaskID, &identity.AttemptID, &identity.IdempotencyKey,
		&identity.PublicationStatus, &identity.ResultDigest, &resultJSON, &identity.LeaseID, &identity.AssignmentGeneration, &identity.LeaseGeneration,
		&identity.WorkerID, &identity.WorkerInstanceID, &resultObserved, &resultImmutable,
	)
	if lookupErr != nil {
		return nil, lookupErr
	}
	if resultObserved != 1 || resultImmutable != 1 || identity.TaskID != taskID || identity.AttemptID != attemptID {
		return nil, errors.New("cleanup authorization is not backed by immutable terminal attempt evidence")
	}
	identity.WorkspaceRef, identity.CleanupID, err = agentTaskResultCleanupBinding(decodeAgentTaskMap(resultJSON), identity.TaskID, identity.AttemptID)
	if err != nil {
		return nil, err
	}
	expectedPublicationReceipt, expectedAuthorization, boundaryErr := agentTaskPublicationBoundary(identity)
	if boundaryErr != nil {
		return nil, boundaryErr
	}
	expectedCleanupReceipt := map[string]string{
		"schema_id": agentTaskCleanupReceiptID, "authority": "task-execution-worker", "state": "cleaned",
		"cleanup_id": identity.CleanupID, "workspace_ref": identity.WorkspaceRef,
		"publication_id": identity.PublicationID, "result_id": identity.ResultID,
		"task_id": identity.TaskID, "attempt_id": identity.AttemptID, "lease_id": identity.LeaseID,
		"worker_id": identity.WorkerID, "worker_instance_id": identity.WorkerInstanceID,
	}
	for field, expected := range expectedCleanupReceipt {
		if anyToString(receipt[field]) != expected {
			return nil, errors.New("cleanup receipt does not match durable publication authorization")
		}
	}
	if anyToInt(receipt["generation"], 0) != identity.LeaseGeneration {
		return nil, errors.New("cleanup receipt does not match durable publication authorization")
	}
	var existingJSON string
	existingErr := tx.QueryRowContext(ctx, `SELECT payload_json FROM task_ledger_cleanup_receipts WHERE cleanup_authorization_digest=? OR cleanup_receipt_id=?`, anyToString(expectedAuthorization["authorization_digest"]), anyToString(receipt["receipt_id"])).Scan(&existingJSON)
	if existingErr == nil {
		existing := decodeAgentTaskMap(existingJSON)
		if err := verifyAgentTaskCleanupReceipt(existing); err != nil {
			return nil, fmt.Errorf("stored cleanup receipt is invalid: %w", err)
		}
		for field, value := range receipt {
			if !agentTaskCanonicalMapEqual(map[string]any{"value": existing[field]}, map[string]any{"value": value}) {
				return nil, errors.New("cleanup receipt identity is already bound to different immutable evidence")
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(existingErr, sql.ErrNoRows) {
		return nil, existingErr
	}
	now := agentTaskNow()
	acknowledgedReceipt := cloneAnyMap(receipt)
	acknowledgedReceipt["recorded"] = true
	acknowledgedReceipt["durable"] = true
	acknowledgedReceipt["acknowledged"] = true
	if err := verifyAgentTaskCleanupReceipt(acknowledgedReceipt); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_cleanup_receipts(cleanup_receipt_id,cleanup_authorization_digest,publication_receipt_digest,publication_id,result_id,task_id,attempt_id,lease_id,assignment_generation,lease_generation,worker_id,worker_instance_id,idempotency_key,payload_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		receipt["receipt_id"], expectedAuthorization["authorization_digest"], expectedPublicationReceipt["receipt_digest"], identity.PublicationID, identity.ResultID,
		identity.TaskID, identity.AttemptID, identity.LeaseID, identity.AssignmentGeneration, identity.LeaseGeneration,
		identity.WorkerID, identity.WorkerInstanceID, identity.IdempotencyKey, encodeAgentTaskJSON(acknowledgedReceipt), now); err != nil {
		return nil, fmt.Errorf("record durable cleanup acknowledgement: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return acknowledgedReceipt, nil
}

func (l *agentTaskDeliveryLedger) resultPayload(ctx context.Context, resultID string) (map[string]any, error) {
	var payloadJSON, digest, status, schemaID, taskID, attemptID, createdAt string
	var observed, immutable int
	err := l.db.QueryRowContext(ctx, `SELECT payload_json,digest,status,schema_id,task_id,attempt_id,created_at,execution_observed,immutable FROM task_ledger_results WHERE result_id=?`, strings.TrimSpace(resultID)).Scan(&payloadJSON, &digest, &status, &schemaID, &taskID, &attemptID, &createdAt, &observed, &immutable)
	if err != nil {
		return nil, err
	}
	payload := decodeAgentTaskMap(payloadJSON)
	payload["result_id"] = resultID
	payload["task_id"] = taskID
	payload["attempt_id"] = attemptID
	payload["schema_id"] = schemaID
	payload["status"] = status
	payload["execution_observed"] = observed != 0
	payload["result_digest"] = digest
	payload["immutable"] = immutable != 0
	return payload, nil
}

func (l *agentTaskDeliveryLedger) finalizePublication(ctx context.Context, publicationID, writebackStatus, writebackRef, lastError string) (map[string]any, error) {
	return l.finalizePublicationClaim(ctx, publicationID, "", writebackStatus, writebackRef, lastError)
}

func (l *agentTaskDeliveryLedger) finalizePublicationClaim(ctx context.Context, publicationID, claimID, writebackStatus, writebackRef, lastError string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"publication_id": publicationID, "claim_id": claimID, "writeback_status": writebackStatus, "writeback_ref": writebackRef, "last_error": lastError}, "publication writeback receipt", agentTaskEventMaxBytes*2); err != nil {
		return nil, err
	}
	writebackStatus = strings.TrimSpace(strings.ToLower(writebackStatus))
	if writebackStatus == "" {
		writebackStatus = "committed"
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taskID, resultID, attemptID, currentStatus, currentClaimID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,result_id,attempt_id,status,worker_claim_id FROM task_ledger_publications WHERE publication_id=?`, strings.TrimSpace(publicationID)).Scan(&taskID, &resultID, &attemptID, &currentStatus, &currentClaimID); err != nil {
		return nil, err
	}
	if claimID != "" && currentClaimID != claimID {
		return nil, errors.New("stale publication worker claim")
	}
	if writebackStatus != "committed" && writebackStatus != "succeeded" && writebackStatus != "ok" {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_publications SET status='writeback_failed',writeback_status=?,writeback_ref=?,last_error=?,next_action='retry_writeback',worker_claim_id='',worker_claimed_by='',worker_claim_expires_at='',updated_at=? WHERE publication_id=?`, writebackStatus, writebackRef, lastError, agentTaskNow(), publicationID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='writeback_failed',updated_at=? WHERE id=? AND status IN ('writeback_pending','writeback_failed')`, agentTaskNow(), taskID); err != nil {
			return nil, err
		}
		if err := l.appendEventTx(ctx, tx, taskID, attemptID, "writeback_failed", "required ContextLattice writeback did not commit", map[string]any{"publication_id": publicationID, "reason": lastError, "writeback_status": writebackStatus}); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return l.publication(ctx, publicationID)
	}
	var deliveryCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_deliveries WHERE publication_id=?`, publicationID).Scan(&deliveryCount); err != nil {
		return nil, err
	}
	if deliveryCount < 1 {
		return nil, errors.New("publication cannot succeed without durable delivery rows")
	}
	var unscannedArtifacts int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_artifacts WHERE task_id=? AND attempt_id=? AND redaction_receipt=''`, taskID, attemptID).Scan(&unscannedArtifacts); err != nil {
		return nil, err
	}
	if unscannedArtifacts > 0 {
		return nil, errors.New("publication cannot succeed without an authoritative artifact redaction receipt")
	}
	if currentStatus == "committed" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return l.publication(ctx, publicationID)
	}
	now := agentTaskNow()
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_publications SET status='committed',writeback_status='committed',writeback_ref=?,last_error='',next_action='deliver_continuation_inbox',worker_claim_id='',worker_claimed_by='',worker_claim_expires_at='',updated_at=? WHERE publication_id=?`, writebackRef, now, publicationID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_results SET status='result_published' WHERE result_id=?`, resultID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_artifacts SET finalized=1,redaction_status='verified' WHERE task_id=? AND attempt_id=? AND redaction_receipt<>''`, taskID, attemptID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='result_published',updated_at=? WHERE id=?`, now, taskID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='execution_succeeded',updated_at=? WHERE id=?`, now, taskID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET status='completed',completed_at=? WHERE attempt_id=?`, now, attemptID); err != nil {
		return nil, err
	}
	if err := l.appendEventTx(ctx, tx, taskID, attemptID, "execution_succeeded", "required writeback and delivery rows committed", map[string]any{"publication_id": publicationID, "result_id": resultID, "delivery_row_count": deliveryCount}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.publication(ctx, publicationID)
}

func (l *agentTaskDeliveryLedger) deliveries(ctx context.Context, taskID, resultID string) ([]map[string]any, error) {
	query := `SELECT delivery_id,publication_id,result_id,task_id,recipient_id,role,observer,session_id,reviewer_owner,status,dedupe_key,attempts,last_error,next_action,acknowledged_at,worker_claim_id,worker_claimed_by,worker_claim_expires_at,payload_json,created_at,updated_at FROM task_ledger_deliveries WHERE task_id=?`
	args := []any{strings.TrimSpace(taskID)}
	if strings.TrimSpace(resultID) != "" {
		query += " AND result_id=?"
		args = append(args, strings.TrimSpace(resultID))
	}
	query += " ORDER BY created_at ASC"
	rows, err := l.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var deliveryID, publicationIDValue, resultIDValue, taskIDValue, recipientID, role, sessionID, reviewerOwner, status, dedupeKey, lastError, nextAction, ackAt, claimID, claimedBy, claimExpires, payloadJSON, createdAt, updatedAt string
		var observer int
		var attempts int
		if err := rows.Scan(&deliveryID, &publicationIDValue, &resultIDValue, &taskIDValue, &recipientID, &role, &observer, &sessionID, &reviewerOwner, &status, &dedupeKey, &attempts, &lastError, &nextAction, &ackAt, &claimID, &claimedBy, &claimExpires, &payloadJSON, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		payload := decodeAgentTaskMap(payloadJSON)
		payload["delivery_id"] = deliveryID
		payload["publication_id"] = publicationIDValue
		payload["result_id"] = resultIDValue
		payload["task_id"] = taskIDValue
		payload["recipient_id"] = recipientID
		payload["role"] = role
		payload["observer"] = observer != 0
		payload["session_id"] = sessionID
		payload["reviewer_owner"] = reviewerOwner
		payload["status"] = status
		payload["dedupe_key"] = dedupeKey
		payload["attempts"] = attempts
		payload["last_error"] = lastError
		payload["next_action"] = nextAction
		payload["acknowledged_at"] = ackAt
		payload["worker_claim_id"] = claimID
		payload["worker_claimed_by"] = claimedBy
		payload["worker_claim_expires_at"] = claimExpires
		payload["created_at"] = createdAt
		payload["updated_at"] = updatedAt
		out = append(out, payload)
	}
	return out, rows.Err()
}

func (l *agentTaskDeliveryLedger) deliver(ctx context.Context, deliveryID, outcome, reason string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"delivery_id": deliveryID, "outcome": outcome, "reason": reason}, "delivery outcome", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	outcome = strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(outcome, "delivered")))
	if outcome != "delivered" && outcome != "failed" && outcome != "dead_letter" {
		return nil, errors.New("delivery outcome must be delivered, failed, or dead_letter")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taskID, resultID, status string
	var attempts int
	if err := tx.QueryRowContext(ctx, `SELECT task_id,result_id,status,attempts FROM task_ledger_deliveries WHERE delivery_id=?`, strings.TrimSpace(deliveryID)).Scan(&taskID, &resultID, &status, &attempts); err != nil {
		return nil, err
	}
	if status == "acknowledged" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		rows, _ := l.deliveries(ctx, taskID, resultID)
		for _, row := range rows {
			if anyToString(row["delivery_id"]) == deliveryID {
				return row, nil
			}
		}
		return nil, sql.ErrNoRows
	}
	if (status == "delivered" && outcome == "delivered") || (status == "dead_letter" && outcome == "dead_letter") {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		rows, _ := l.deliveries(ctx, taskID, resultID)
		for _, row := range rows {
			if anyToString(row["delivery_id"]) == deliveryID {
				return row, nil
			}
		}
		return nil, sql.ErrNoRows
	}
	attempts++
	nextAction := "acknowledge_review"
	if outcome == "failed" {
		nextAction = "retry_continuation_inbox"
	}
	if outcome == "dead_letter" {
		nextAction = "owner_reconcile_delivery"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_deliveries SET status=?,attempts=?,last_error=?,next_action=?,updated_at=? WHERE delivery_id=?`, outcome, attempts, strings.TrimSpace(reason), nextAction, agentTaskNow(), deliveryID); err != nil {
		return nil, err
	}
	if err := l.appendEventTx(ctx, tx, taskID, "", "delivery_"+outcome, "task delivery projection updated", map[string]any{"delivery_id": deliveryID, "result_id": resultID, "reason": reason, "attempts": attempts}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rows, err := l.deliveries(ctx, taskID, resultID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if anyToString(row["delivery_id"]) == deliveryID {
			return row, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (l *agentTaskDeliveryLedger) acknowledgeDelivery(ctx context.Context, deliveryID, actor string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"delivery_id": deliveryID, "actor": actor}, "delivery acknowledgement", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		return nil, errors.New("delivery acknowledgement actor is required")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var taskID, resultID, recipientID, status string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,result_id,recipient_id,status FROM task_ledger_deliveries WHERE delivery_id=?`, strings.TrimSpace(deliveryID)).Scan(&taskID, &resultID, &recipientID, &status); err != nil {
		return nil, err
	}
	if !strings.EqualFold(actor, recipientID) {
		return nil, errors.New("delivery acknowledgement is not authorized for recipient")
	}
	if status == "acknowledged" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		rows, _ := l.deliveries(ctx, taskID, resultID)
		for _, row := range rows {
			if anyToString(row["delivery_id"]) == deliveryID {
				return row, nil
			}
		}
	}
	if status != "delivered" && status != "pending" && status != "failed" {
		return nil, fmt.Errorf("delivery cannot be acknowledged from %s", status)
	}
	now := agentTaskNow()
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_deliveries SET status='acknowledged',acknowledged_at=?,next_action='review',updated_at=? WHERE delivery_id=?`, now, now, deliveryID); err != nil {
		return nil, err
	}
	if err := l.appendEventTx(ctx, tx, taskID, "", "delivery_acknowledged", "recipient acknowledged task delivery", map[string]any{"delivery_id": deliveryID, "recipient_id": recipientID}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	rows, err := l.deliveries(ctx, taskID, resultID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if anyToString(row["delivery_id"]) == deliveryID {
			return row, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (l *agentTaskDeliveryLedger) claimReview(ctx context.Context, taskID, resultID, deliveryID, actor string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"task_id": taskID, "result_id": resultID, "delivery_id": deliveryID, "actor": actor}, "reviewer claim", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	resultID = strings.TrimSpace(resultID)
	deliveryID = strings.TrimSpace(deliveryID)
	actor = strings.TrimSpace(actor)
	if taskID == "" || resultID == "" || deliveryID == "" || actor == "" {
		return nil, errors.New("reviewer claim requires task_id, result_id, delivery_id, and actor")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owner, taskStatus, deliveryTaskID, deliveryResultID string
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT review_owner,status,generation FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&owner, &taskStatus, &generation); err != nil {
		return nil, err
	}
	if !strings.EqualFold(actor, owner) {
		return nil, errors.New("reviewer claim is authorized only for canonical reviewer")
	}
	if err := tx.QueryRowContext(ctx, `SELECT task_id,result_id FROM task_ledger_deliveries WHERE delivery_id=?`, deliveryID).Scan(&deliveryTaskID, &deliveryResultID); err != nil {
		return nil, err
	}
	if deliveryTaskID != taskID || deliveryResultID != resultID {
		return nil, errors.New("reviewer claim delivery is not bound to the exact task result")
	}
	var claimID, claimedBy, status, createdAt, updatedAt string
	var storedGeneration int
	lookupErr := tx.QueryRowContext(ctx, `SELECT claim_id,actor,status,generation,created_at,updated_at FROM task_ledger_reviewer_claims WHERE result_id=?`, resultID).Scan(&claimID, &claimedBy, &status, &storedGeneration, &createdAt, &updatedAt)
	if lookupErr == nil {
		if !strings.EqualFold(claimedBy, actor) {
			return nil, errors.New("task result already has a different reviewer claim owner")
		}
		payload := agentTaskContractPayload(agentTaskReviewerClaimContractID, map[string]any{"schema_id": agentTaskReviewerClaimContractID, "contract_version": 1, "claim_id": claimID, "task_id": taskID, "result_id": resultID, "delivery_id": deliveryID, "reviewer_owner": owner, "actor": actor, "status": status, "generation": storedGeneration, "created_at": createdAt, "updated_at": updatedAt, "idempotent_replay": true})
		if err := agentTaskRequireContract(agentTaskReviewerClaimContractID, payload); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return payload, nil
	}
	if !errors.Is(lookupErr, sql.ErrNoRows) {
		return nil, lookupErr
	}
	if taskStatus != "execution_succeeded" && taskStatus != "review_pending" {
		return nil, fmt.Errorf("reviewer claim is not available from task status %s", taskStatus)
	}
	claimID, err = l.newUniqueID(ctx, tx, "review-claim", "review-claim")
	if err != nil {
		return nil, err
	}
	now := agentTaskNow()
	payload := agentTaskContractPayload(agentTaskReviewerClaimContractID, map[string]any{"schema_id": agentTaskReviewerClaimContractID, "contract_version": 1, "claim_id": claimID, "task_id": taskID, "result_id": resultID, "delivery_id": deliveryID, "reviewer_owner": owner, "actor": actor, "status": "active", "generation": generation, "created_at": now, "updated_at": now})
	if err := agentTaskRequireContract(agentTaskReviewerClaimContractID, payload); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_reviewer_claims(claim_id,result_id,task_id,delivery_id,reviewer_owner,actor,status,generation,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, claimID, resultID, taskID, deliveryID, owner, actor, "active", generation, now, now); err != nil {
		return nil, err
	}
	if taskStatus == "execution_succeeded" {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='review_pending',updated_at=? WHERE id=?`, now, taskID); err != nil {
			return nil, err
		}
	}
	if err := l.appendEventTx(ctx, tx, taskID, "", "review_pending", "canonical reviewer claimed immutable task result", map[string]any{"claim_id": claimID, "result_id": resultID, "delivery_id": deliveryID, "actor": actor, "generation": generation}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return payload, nil
}

func (l *agentTaskDeliveryLedger) review(ctx context.Context, taskID, resultID, actor, decision, reason, replacementResultID string) (map[string]any, error) {
	return l.reviewWithFence(ctx, taskID, resultID, actor, decision, reason, replacementResultID, "", 0)
}

func agentTaskReviewIsTerminal(status string) bool {
	status = strings.TrimSpace(strings.ToLower(status))
	return status != "" && status != "acknowledged" && status != "review_blocked"
}

func agentTaskReviewReplayMatches(storedActor, storedDecision, storedReason, storedReplacement, actor, decision, reason, replacement string) bool {
	return strings.EqualFold(strings.TrimSpace(storedActor), strings.TrimSpace(actor)) &&
		strings.TrimSpace(strings.ToLower(storedDecision)) == strings.TrimSpace(strings.ToLower(decision)) &&
		storedReason == reason && storedReplacement == replacement
}

// reviewWithFence binds a review decision to the immutable result attempt and
// generation. The public wrapper derives that fence for in-process callers;
// request_changes routes require callers to submit it explicitly.
func (l *agentTaskDeliveryLedger) reviewWithFence(ctx context.Context, taskID, resultID, actor, decision, reason, replacementResultID, sourceAttemptID string, sourceGeneration int) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"task_id": taskID, "result_id": resultID, "actor": actor, "decision": decision, "reason": reason, "replacement_result_id": replacementResultID, "source_attempt_id": sourceAttemptID, "source_generation": sourceGeneration}, "task review", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	resultID = strings.TrimSpace(resultID)
	actor = strings.TrimSpace(actor)
	decision = strings.TrimSpace(strings.ToLower(decision))
	sourceAttemptID = strings.TrimSpace(sourceAttemptID)
	if taskID == "" || resultID == "" || actor == "" || decision == "" {
		return nil, errors.New("task_id, result_id, actor, and decision are required")
	}
	explicitFence := sourceAttemptID != "" || sourceGeneration != 0
	if explicitFence && (sourceAttemptID == "" || sourceGeneration <= 0) {
		return nil, errors.New("review source fence requires source_attempt_id and positive source_generation")
	}
	allowedDecisions := map[string]bool{"acknowledge": true, "accept": true, "request_changes": true, "reject": true, "block": true, "supersede": true, "knowledge_accept": true, "leave_unintegrated": true}
	if !allowedDecisions[decision] {
		return nil, fmt.Errorf("unsupported review decision %q", decision)
	}
	reason, reasonTruncated := agentTaskBoundedString(reason, 3000)
	if reasonTruncated {
		return nil, errors.New("review reason exceeds the 3000 byte limit")
	}
	replacementResultID = strings.TrimSpace(replacementResultID)
	if _, truncated := agentTaskBoundedString(replacementResultID, 2048); truncated {
		return nil, errors.New("replacement_result_id exceeds the 2048 byte limit")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var reviewerOwner, taskStatus, activeAttemptID, currentResultID string
	var taskGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT review_owner,status,COALESCE(active_attempt_id,''),COALESCE(result_id,''),generation FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&reviewerOwner, &taskStatus, &activeAttemptID, &currentResultID, &taskGeneration); err != nil {
		return nil, err
	}
	if !strings.EqualFold(actor, reviewerOwner) {
		return nil, errors.New("review action is authorized only for canonical reviewer")
	}
	var resultTaskID, resultAttemptID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id,attempt_id FROM task_ledger_results WHERE result_id=?`, resultID).Scan(&resultTaskID, &resultAttemptID); err != nil {
		return nil, err
	}
	if resultTaskID != taskID {
		return nil, errors.New("review result is not bound to the exact task")
	}
	var resultWorkerID, resultWorkerInstanceID string
	var resultGeneration, resultIdentityGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT generation,worker_id,worker_instance_id,worker_identity_update_generation FROM task_ledger_attempts WHERE attempt_id=? AND task_id=?`, resultAttemptID, taskID).Scan(&resultGeneration, &resultWorkerID, &resultWorkerInstanceID, &resultIdentityGeneration); err != nil {
		return nil, err
	}
	if !explicitFence {
		sourceAttemptID = resultAttemptID
		sourceGeneration = resultGeneration
	}
	if sourceAttemptID != resultAttemptID || sourceGeneration != resultGeneration {
		return nil, errors.New("stale_review_fence: source attempt or generation does not match the immutable result")
	}

	var existingReviewID, existingStatus, existingDecision, existingActor, existingReason, existingReplacement string
	lookupErr := tx.QueryRowContext(ctx, `SELECT review_id,status,decision,actor,reason,replacement_result_id FROM task_ledger_reviews WHERE result_id=?`, resultID).Scan(&existingReviewID, &existingStatus, &existingDecision, &existingActor, &existingReason, &existingReplacement)
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return nil, lookupErr
	}
	if agentTaskReviewIsTerminal(existingStatus) {
		if !agentTaskReviewReplayMatches(existingActor, existingDecision, existingReason, existingReplacement, actor, decision, reason, replacementResultID) {
			return nil, errors.New("terminal review decision is immutable")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return l.reviewPayload(ctx, existingReviewID)
	}
	if existingStatus == "acknowledged" && decision == "acknowledge" {
		if !agentTaskReviewReplayMatches(existingActor, existingDecision, existingReason, existingReplacement, actor, decision, reason, replacementResultID) {
			return nil, errors.New("review acknowledgement is immutable")
		}
		if currentResultID != resultID || activeAttemptID != resultAttemptID || taskGeneration != resultGeneration || (taskStatus != "execution_succeeded" && taskStatus != "review_pending") {
			return nil, errors.New("review acknowledgement is allowed only before a terminal decision on the current result")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return l.reviewPayload(ctx, existingReviewID)
	}
	if currentResultID != resultID || activeAttemptID != resultAttemptID || taskGeneration != resultGeneration {
		return nil, errors.New("stale_review_fence: task no longer points to the exact result attempt and generation")
	}
	if decision == "acknowledge" && taskStatus != "execution_succeeded" && taskStatus != "review_pending" {
		return nil, errors.New("review acknowledgement is allowed only before a terminal decision on the current result")
	}
	var claimID, claimOwner, claimActor, claimStatus string
	var claimGeneration int
	if decision != "acknowledge" {
		if err := tx.QueryRowContext(ctx, `SELECT claim_id,reviewer_owner,actor,status,generation FROM task_ledger_reviewer_claims WHERE result_id=? AND task_id=?`, resultID, taskID).Scan(&claimID, &claimOwner, &claimActor, &claimStatus, &claimGeneration); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errors.New("active canonical reviewer claim is required before a review decision")
			}
			return nil, err
		}
		if !strings.EqualFold(claimOwner, reviewerOwner) || !strings.EqualFold(claimActor, actor) || claimStatus != "active" || claimGeneration != resultGeneration {
			return nil, errors.New("active canonical reviewer claim for the exact result generation is required before a review decision")
		}
	}
	if decision != "acknowledge" && taskStatus != "review_pending" && taskStatus != "review_blocked" && taskStatus != "execution_succeeded" {
		return nil, fmt.Errorf("review action is not available from task status %s", taskStatus)
	}
	revisionIdentityAdoptionRequired := false
	if decision == "request_changes" && resultIdentityGeneration == 0 {
		bound, bindingErr := workerIdentityGenerationZeroSourceBoundTx(ctx, tx, taskID, resultWorkerID, resultWorkerInstanceID)
		if bindingErr != nil {
			return nil, bindingErr
		}
		revisionIdentityAdoptionRequired = !bound
	}
	newStatus := "acknowledged"
	taskTarget := taskStatus
	switch decision {
	case "acknowledge":
		if taskStatus == "execution_succeeded" {
			taskTarget = "review_pending"
		}
	case "accept":
		newStatus, taskTarget = "accepted_for_integration", "accepted_for_integration"
	case "request_changes":
		newStatus, taskTarget = "changes_requested", "changes_requested"
	case "reject":
		newStatus, taskTarget = "rejected", "rejected"
	case "block":
		newStatus, taskTarget = "review_blocked", "review_blocked"
	case "supersede":
		if strings.TrimSpace(replacementResultID) == "" {
			return nil, errors.New("supersede requires replacement_result_id")
		}
		newStatus, taskTarget = "superseded", "superseded"
	case "knowledge_accept":
		newStatus, taskTarget = "knowledge_accepted", "knowledge_accepted"
	case "leave_unintegrated":
		newStatus, taskTarget = "unintegrated", "unintegrated"
	}
	if taskTarget != taskStatus && !agentTaskAllowedTransition(taskStatus, taskTarget) {
		return nil, fmt.Errorf("invalid review task transition %s -> %s", taskStatus, taskTarget)
	}
	if decision == "request_changes" && !agentTaskAllowedTransition(taskTarget, "queued") {
		return nil, fmt.Errorf("invalid request-changes requeue transition %s -> queued", taskTarget)
	}
	now := agentTaskNow()
	if existingReviewID == "" {
		existingReviewID, err = l.newUniqueID(ctx, tx, "review", "review")
		if err != nil {
			return nil, err
		}
	}
	reviewContractPayload := agentTaskContractPayload(agentTaskReviewContractID, map[string]any{"schema_id": agentTaskReviewContractID, "contract_version": 1, "review_id": existingReviewID, "result_id": resultID, "task_id": taskID, "source_attempt_id": sourceAttemptID, "source_generation": sourceGeneration, "reviewer_owner": reviewerOwner, "status": newStatus, "decision": decision, "actor": actor, "reason": reason, "replacement_result_id": replacementResultID, "created_at": now, "updated_at": now})
	if err := agentTaskRequireContract(agentTaskReviewContractID, reviewContractPayload); err != nil {
		return nil, err
	}
	if lookupErr != nil {
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_reviews(review_id,result_id,task_id,reviewer_owner,status,decision,actor,reason,replacement_result_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, existingReviewID, resultID, taskID, reviewerOwner, newStatus, decision, actor, reason, replacementResultID, now, now); err != nil {
			return nil, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_reviews SET status=?,decision=?,actor=?,reason=?,replacement_result_id=?,updated_at=? WHERE review_id=?`, newStatus, decision, actor, reason, replacementResultID, now, existingReviewID); err != nil {
			return nil, err
		}
	}
	if decision != "acknowledge" && decision != "block" {
		// A terminal review closes the exact durable custody claim in the same
		// transaction as the review and task transition. A block retains active
		// custody so the canonical reviewer can resume after the answer arrives.
		claimUpdate, claimErr := tx.ExecContext(ctx, `UPDATE task_ledger_reviewer_claims SET status=?,updated_at=? WHERE claim_id=? AND result_id=? AND task_id=? AND reviewer_owner=? AND actor=? AND status='active' AND generation=?`, newStatus, now, claimID, resultID, taskID, claimOwner, claimActor, claimGeneration)
		if claimErr != nil {
			return nil, claimErr
		}
		if affected, affectedErr := claimUpdate.RowsAffected(); affectedErr != nil {
			return nil, affectedErr
		} else if affected != 1 {
			return nil, errors.New("reviewer claim terminal transition CAS lost")
		}
	}
	if decision == "request_changes" {
		revisionBase := map[string]any{
			"schema_id": "agent_task_revision_source.v1", "review_id": existingReviewID, "task_id": taskID,
			"source_result_id": resultID, "source_attempt_id": sourceAttemptID, "source_generation": sourceGeneration,
			"reason": reason,
		}
		if err := agentTaskValidateStructured(revisionBase, "request-changes revision source", agentTaskEventMaxBytes); err != nil {
			return nil, err
		}
		updatedTask, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET
			status='queued',
			claim_eligible=CASE
				WHEN ?<>0 THEN 0
				WHEN (COALESCE(json_extract(approval_policy_json,'$.required'),0)=0 OR approved=1)
				 AND context_request_json LIKE '%"content_hash":"sha256:%'
				 AND COALESCE(json_extract(context_request_json,'$.session_id'),'')<>''
				THEN 1 ELSE 0 END,
			active_attempt_id='',result_id='',publication_id='',revision_envelope_json=?,updated_at=?
			WHERE id=? AND status=? AND active_attempt_id=? AND result_id=? AND generation=?`, boolToSQLiteInt(revisionIdentityAdoptionRequired), encodeAgentTaskJSON(revisionBase), now, taskID, taskStatus, resultAttemptID, resultID, resultGeneration)
		if err != nil {
			return nil, err
		}
		if affected, affectedErr := updatedTask.RowsAffected(); affectedErr != nil || affected != 1 {
			return nil, errors.New("stale_review_fence: task changed before request-changes requeue")
		}
	} else if taskTarget != taskStatus {
		updatedTask, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status=?,updated_at=? WHERE id=? AND status=? AND active_attempt_id=? AND result_id=? AND generation=?`, taskTarget, now, taskID, taskStatus, resultAttemptID, resultID, resultGeneration)
		if err != nil {
			return nil, err
		}
		if affected, affectedErr := updatedTask.RowsAffected(); affectedErr != nil || affected != 1 {
			return nil, errors.New("stale_review_fence: task changed before review decision commit")
		}
	}
	if err := l.appendEventTx(ctx, tx, taskID, sourceAttemptID, "review_"+decision, "canonical reviewer recorded explicit task review decision", map[string]any{"review_id": existingReviewID, "result_id": resultID, "source_attempt_id": sourceAttemptID, "source_generation": sourceGeneration, "actor": actor, "decision": decision, "replacement_result_id": replacementResultID, "reason": reason, "worker_identity_adoption_required": revisionIdentityAdoptionRequired}); err != nil {
		return nil, err
	}
	if decision == "request_changes" {
		if err := l.appendEventTx(ctx, tx, taskID, sourceAttemptID, "queued", "canonical reviewer requested changes and queued a new fenced revision", map[string]any{"review_id": existingReviewID, "source_result_id": resultID, "source_attempt_id": sourceAttemptID, "source_generation": sourceGeneration, "next_generation": sourceGeneration + 1, "worker_identity_adoption_required": revisionIdentityAdoptionRequired, "claim_eligible": !revisionIdentityAdoptionRequired}); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return l.reviewPayload(ctx, existingReviewID)
}

func (l *agentTaskDeliveryLedger) reviewPayload(ctx context.Context, reviewID string) (map[string]any, error) {
	var resultID, taskID, sourceAttemptID, reviewerOwner, status, decision, actor, reason, replacementResultID, createdAt, updatedAt string
	var sourceGeneration int
	err := l.db.QueryRowContext(ctx, `SELECT r.result_id,r.task_id,x.attempt_id,a.generation,r.reviewer_owner,r.status,r.decision,r.actor,r.reason,r.replacement_result_id,r.created_at,r.updated_at FROM task_ledger_reviews r JOIN task_ledger_results x ON x.result_id=r.result_id AND x.task_id=r.task_id JOIN task_ledger_attempts a ON a.attempt_id=x.attempt_id AND a.task_id=x.task_id WHERE r.review_id=?`, strings.TrimSpace(reviewID)).Scan(&resultID, &taskID, &sourceAttemptID, &sourceGeneration, &reviewerOwner, &status, &decision, &actor, &reason, &replacementResultID, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	payload := agentTaskContractPayload(agentTaskReviewContractID, map[string]any{"schema_id": agentTaskReviewContractID, "contract_version": 1, "review_id": reviewID, "result_id": resultID, "task_id": taskID, "source_attempt_id": sourceAttemptID, "source_generation": sourceGeneration, "reviewer_owner": reviewerOwner, "status": status, "decision": decision, "actor": actor, "reason": reason, "replacement_result_id": replacementResultID, "created_at": createdAt, "updated_at": updatedAt})
	if err := agentTaskRequireContract(agentTaskReviewContractID, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (l *agentTaskDeliveryLedger) answerBlockingQuestion(ctx context.Context, taskID, resultID, deliveryID, actor, answer, sourceAttemptID string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"task_id": taskID, "result_id": resultID, "delivery_id": deliveryID, "actor": actor, "answer": answer, "source_attempt_id": sourceAttemptID}, "blocking answer", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	if strings.TrimSpace(taskID) == "" || strings.TrimSpace(resultID) == "" || strings.TrimSpace(deliveryID) == "" || strings.TrimSpace(actor) == "" || strings.TrimSpace(answer) == "" || strings.TrimSpace(sourceAttemptID) == "" {
		return nil, errors.New("blocking answer requires task, result, delivery, actor, answer, and source attempt")
	}
	answer, answerTruncated := agentTaskBoundedString(answer, 4096)
	if answerTruncated {
		return nil, errors.New("blocking answer exceeds the 4096 byte limit")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owner, recipient, deliveryAttemptID, resultWorkerID, resultWorkerInstanceID string
	var resultIdentityGeneration int
	if err := tx.QueryRowContext(ctx, `SELECT reviewer_owner,recipient_id FROM task_ledger_deliveries WHERE delivery_id=? AND task_id=? AND result_id=?`, deliveryID, taskID, resultID).Scan(&owner, &recipient); err != nil {
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT r.attempt_id,a.worker_id,a.worker_instance_id,a.worker_identity_update_generation FROM task_ledger_results r JOIN task_ledger_attempts a ON a.attempt_id=r.attempt_id AND a.task_id=r.task_id WHERE r.result_id=? AND r.task_id=?`, resultID, taskID).Scan(&deliveryAttemptID, &resultWorkerID, &resultWorkerInstanceID, &resultIdentityGeneration); err != nil {
		return nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(sourceAttemptID), strings.TrimSpace(deliveryAttemptID)) {
		return nil, errors.New("blocking answer source attempt is not bound to the result")
	}
	if !strings.EqualFold(actor, owner) && !strings.EqualFold(actor, recipient) {
		return nil, errors.New("blocking answer actor is not a delivery participant")
	}
	var taskStatus string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&taskStatus); err != nil {
		return nil, err
	}
	if taskStatus != "review_blocked" {
		return nil, fmt.Errorf("blocking answer is not available from task status %s", taskStatus)
	}
	answerIdentityAdoptionRequired := false
	if resultIdentityGeneration == 0 {
		bound, bindingErr := workerIdentityGenerationZeroSourceBoundTx(ctx, tx, taskID, resultWorkerID, resultWorkerInstanceID)
		if bindingErr != nil {
			return nil, bindingErr
		}
		answerIdentityAdoptionRequired = !bound
	}
	answerID, err := l.newUniqueID(ctx, tx, "answer", "answer")
	if err != nil {
		return nil, err
	}
	payload := agentTaskContractPayload(agentTaskBlockingAnswerContractID, map[string]any{"schema_id": agentTaskBlockingAnswerContractID, "contract_version": 1, "answer_id": answerID, "task_id": taskID, "result_id": resultID, "delivery_id": deliveryID, "actor": actor, "answer": answer, "source_attempt_id": sourceAttemptID, "next_action": "claim_new_fenced_attempt", "queued": true})
	if err := agentTaskRequireContract(agentTaskBlockingAnswerContractID, payload); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_blocking_answers(answer_id,task_id,result_id,delivery_id,actor,answer,source_attempt_id,created_at) VALUES(?,?,?,?,?,?,?,?)`, answerID, taskID, resultID, deliveryID, actor, answer, sourceAttemptID, agentTaskNow()); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='queued',claim_eligible=CASE WHEN ?<>0 THEN 0 ELSE 1 END,active_attempt_id='',result_id='',publication_id='',updated_at=? WHERE id=? AND status='review_blocked'`, boolToSQLiteInt(answerIdentityAdoptionRequired), agentTaskNow(), taskID); err != nil {
		return nil, err
	}
	if err := l.appendEventTx(ctx, tx, taskID, sourceAttemptID, "queued", "canonical reviewer answer recorded and a new fenced attempt queued", map[string]any{"answer_id": answerID, "delivery_id": deliveryID, "source_result_id": resultID, "source_attempt_id": sourceAttemptID, "worker_identity_adoption_required": answerIdentityAdoptionRequired, "claim_eligible": !answerIdentityAdoptionRequired}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return payload, nil
}

func agentTaskBoundedStringMust(value string, maxBytes int) string {
	bounded, _ := agentTaskBoundedString(value, maxBytes)
	return bounded
}

func boolToSQLiteInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func (l *agentTaskDeliveryLedger) createApproval(ctx context.Context, request map[string]any) (map[string]any, error) {
	if err := agentTaskValidateStructured(request, "approval request", agentTaskEventMaxBytes*2); err != nil {
		return nil, err
	}
	taskID := strings.TrimSpace(anyToString(request["task_id"]))
	attemptID := strings.TrimSpace(anyToString(request["attempt_id"]))
	approver := strings.TrimSpace(anyToString(request["approver"]))
	if taskID == "" || attemptID == "" || approver == "" {
		return nil, errors.New("approval requires task_id, attempt_id, and approver")
	}
	task, err := l.queryTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(approver, anyToString(task["review_owner"])) {
		return nil, errors.New("approval actor is not the canonical reviewer")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var attemptTaskID string
	if err := tx.QueryRowContext(ctx, `SELECT task_id FROM task_ledger_attempts WHERE attempt_id=?`, attemptID).Scan(&attemptTaskID); err != nil {
		return nil, err
	}
	if attemptTaskID != taskID {
		return nil, errors.New("approval attempt is not bound to the exact task")
	}
	approvalID := strings.TrimSpace(anyToString(request["approval_id"]))
	if approvalID == "" {
		approvalID, err = l.newUniqueID(ctx, tx, "approval", "approval")
		if err != nil {
			return nil, err
		}
	}
	nonce := strings.TrimSpace(anyToString(request["nonce"]))
	if nonce == "" {
		nonce, err = l.newUniqueID(ctx, tx, "nonce", "nonce")
		if err != nil {
			return nil, err
		}
	}
	expires := strings.TrimSpace(anyToString(request["expires_at"]))
	if expires == "" {
		expires = time.Now().UTC().Add(15 * time.Minute).Format(time.RFC3339Nano)
	}
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, expires)
	if expiresErr != nil || !expiresAt.After(time.Now().UTC()) || expiresAt.After(time.Now().UTC().Add(24*time.Hour)) {
		return nil, errors.New("approval expiry is invalid or outside the bounded approval window")
	}
	payload := map[string]any{"schema_id": agentTaskApprovalContractID, "contract_version": 1, "approval_id": approvalID, "task_id": taskID, "attempt_id": attemptID, "result_or_commit_digest": anyToString(request["result_or_commit_digest"]), "target": anyToString(request["target"]), "policy_version": firstNonEmptyStrings(anyToString(request["policy_version"]), "task-delivery-core.v1"), "approver": approver, "expires_at": expires, "nonce": nonce, "status": "valid"}
	if strings.TrimSpace(anyToString(payload["result_or_commit_digest"])) == "" || strings.TrimSpace(anyToString(payload["target"])) == "" {
		return nil, errors.New("approval requires exact result_or_commit_digest and target")
	}
	payload = agentTaskContractPayload(agentTaskApprovalContractID, payload)
	if err := agentTaskRequireContract(agentTaskApprovalContractID, payload); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO task_ledger_approvals(approval_id,task_id,attempt_id,result_or_commit_digest,target,policy_version,approver,expires_at,nonce,status,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, approvalID, taskID, attemptID, anyToString(payload["result_or_commit_digest"]), anyToString(payload["target"]), anyToString(payload["policy_version"]), approver, expires, nonce, "valid", agentTaskNow())
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return payload, nil
}

func (l *agentTaskDeliveryLedger) approveLegacy(ctx context.Context, taskID, approver, note string) (map[string]any, error) {
	if err := agentTaskValidateStructured(map[string]any{"task_id": taskID, "approver": approver, "note": note}, "compatibility approval", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	taskID = strings.TrimSpace(taskID)
	approver = strings.TrimSpace(approver)
	if taskID == "" || approver == "" {
		return nil, errors.New("legacy approval requires task_id and approver")
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var owner, status string
	if err := tx.QueryRowContext(ctx, `SELECT review_owner,status FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&owner, &status); err != nil {
		return nil, err
	}
	if !strings.EqualFold(owner, approver) {
		return nil, errors.New("approval actor is not the canonical reviewer")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET approved=1,claim_eligible=CASE WHEN status='queued' AND context_request_json LIKE '%"content_hash":"sha256:%' AND COALESCE(json_extract(context_request_json,'$.session_id'),'')<>'' THEN 1 ELSE claim_eligible END,updated_at=? WHERE id=?`, agentTaskNow(), taskID); err != nil {
		return nil, err
	}
	if err := l.appendEventTx(ctx, tx, taskID, "", "approval_pending", "compatibility approval recorded by gateway task ledger", map[string]any{"approver": approver, "note": agentTaskBoundedStringMust(note, agentTaskEventMaxBytes), "previous_status": status}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	task, err := l.queryTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	return task, nil
}

type agentTaskApprovalEvidence struct {
	ID            string
	ExpiresAt     string
	PolicyVersion string
}

func agentTaskApprovalPolicyVersion(policy map[string]any) string {
	return strings.TrimSpace(firstNonEmptyStrings(anyToString(policy["policy_version"]), anyToString(policy["version"]), "task-delivery-core.v1"))
}

func agentTaskIntegrationPolicyEvidenceDigest(taskID, resultID, attemptID, action, digest, target, policyDigest string, approvalRequired bool, approval agentTaskApprovalEvidence) string {
	return agentTaskDigest(map[string]any{
		"schema_id": "agent_task_integration_policy_evidence.v1", "task_id": taskID, "result_id": resultID,
		"attempt_id": attemptID, "action": action, "digest": digest, "target": target,
		"policy_digest": policyDigest, "approval_required": approvalRequired,
		"approval_id": approval.ID, "approval_expires_at": approval.ExpiresAt, "approval_policy_version": approval.PolicyVersion,
	})
}

func (l *agentTaskDeliveryLedger) validApprovalTx(ctx context.Context, tx *sql.Tx, taskID, attemptID, digest, target, actor, policyVersion string) (agentTaskApprovalEvidence, error) {
	var approvalID, approvalTask, approvalAttempt, approvalDigest, approvalTarget, approvalPolicyVersion, approver, expiresAt, status, usedAt string
	err := tx.QueryRowContext(ctx, `SELECT approval_id,task_id,attempt_id,result_or_commit_digest,target,policy_version,approver,expires_at,status,used_at FROM task_ledger_approvals WHERE task_id=? AND attempt_id=? AND result_or_commit_digest=? AND target=? ORDER BY created_at DESC LIMIT 1`, taskID, attemptID, digest, target).Scan(&approvalID, &approvalTask, &approvalAttempt, &approvalDigest, &approvalTarget, &approvalPolicyVersion, &approver, &expiresAt, &status, &usedAt)
	if err != nil {
		return agentTaskApprovalEvidence{}, err
	}
	expires, parseErr := time.Parse(time.RFC3339Nano, expiresAt)
	if parseErr != nil || !time.Now().UTC().Before(expires) || status != "valid" || usedAt != "" || !strings.EqualFold(approver, actor) || approvalTask != taskID || approvalAttempt != attemptID || approvalDigest != digest || approvalTarget != target || approvalPolicyVersion != policyVersion {
		return agentTaskApprovalEvidence{}, errors.New("approval missing, expired, mismatched, or already used")
	}
	result, err := tx.ExecContext(ctx, `UPDATE task_ledger_approvals SET status='used',used_at=? WHERE approval_id=? AND status='valid' AND used_at=''`, agentTaskNow(), approvalID)
	if err != nil {
		return agentTaskApprovalEvidence{}, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return agentTaskApprovalEvidence{}, errors.New("approval was already consumed")
	}
	return agentTaskApprovalEvidence{ID: approvalID, ExpiresAt: expiresAt, PolicyVersion: approvalPolicyVersion}, nil
}

func (l *agentTaskDeliveryLedger) integrate(ctx context.Context, request map[string]any) (map[string]any, error) {
	if err := agentTaskValidateStructured(request, "integration request", agentTaskEventMaxBytes*4); err != nil {
		return nil, err
	}
	taskID := strings.TrimSpace(anyToString(request["task_id"]))
	resultID := strings.TrimSpace(anyToString(request["result_id"]))
	actor := strings.TrimSpace(anyToString(request["actor"]))
	action := strings.TrimSpace(strings.ToLower(anyToString(request["action"])))
	if taskID == "" || resultID == "" || actor == "" || action == "" {
		return nil, errors.New("integration requires task_id, result_id, actor, and action")
	}
	allowedActions := map[string]bool{
		"open_pr": true, "update_pr": true, "export_patch": true, "promote_knowledge": true,
		"follow_up_task": true, "leave_unintegrated": true, "merge": true,
	}
	if !allowedActions[action] {
		return nil, fmt.Errorf("unsupported integration action %q", action)
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var reviewerOwner, taskStatus, attemptID, digest, policyJSON string
	if err := tx.QueryRowContext(ctx, `SELECT review_owner,status,approval_policy_json FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&reviewerOwner, &taskStatus, &policyJSON); err != nil {
		return nil, err
	}
	if !strings.EqualFold(actor, reviewerOwner) {
		return nil, errors.New("integration action is authorized only for canonical reviewer")
	}
	if err := tx.QueryRowContext(ctx, `SELECT attempt_id,digest FROM task_ledger_results WHERE result_id=? AND task_id=?`, resultID, taskID).Scan(&attemptID, &digest); err != nil {
		return nil, err
	}
	requestedDigest := strings.TrimSpace(anyToString(request["digest"]))
	if requestedDigest == "" || requestedDigest != digest {
		return nil, errors.New("integration digest must exactly match the immutable result digest")
	}
	policy := decodeAgentTaskMap(policyJSON)
	policyDigest := agentTaskDigest(policy)
	if policyDigest == "" {
		return nil, errors.New("integration approval policy is not canonically serializable")
	}
	approvalRequired := anyToBool(policy["required"]) && action != "merge"
	policyVersion := agentTaskApprovalPolicyVersion(policy)
	providerAction := action == "open_pr" || action == "update_pr" || action == "export_patch" || action == "promote_knowledge"
	target := strings.TrimSpace(anyToString(request["target"]))
	if _, truncated := agentTaskBoundedString(target, 2048); truncated {
		return nil, errors.New("integration target exceeds the 2048 byte limit")
	}
	if providerAction && target == "" {
		return nil, errors.New("provider integration action requires a nonempty exact target")
	}
	providerRef := ""
	executionReceiptDigest := ""
	providerReceipt := anyMap(request["execution_receipt"])
	if len(providerReceipt) == 0 {
		providerReceipt = anyMap(request["provider_receipt"])
	}
	if providerAction {
		if len(providerReceipt) == 0 {
			return nil, errors.New("provider action requires a verified provider-neutral execution receipt")
		}
		receiptID := strings.TrimSpace(anyToString(providerReceipt["receipt_id"]))
		receiptAuthority := strings.TrimSpace(strings.ToLower(anyToString(providerReceipt["authority"])))
		receiptStatus := strings.TrimSpace(strings.ToLower(anyToString(providerReceipt["status"])))
		receiptDigest := strings.TrimSpace(anyToString(providerReceipt["result_digest"]))
		receiptTarget := strings.TrimSpace(anyToString(providerReceipt["target"]))
		if receiptID == "" || receiptAuthority != "provider-neutral" || (receiptStatus != "succeeded" && receiptStatus != "committed" && receiptStatus != "accepted") || receiptDigest != digest || receiptTarget == "" || receiptTarget != target {
			return nil, errors.New("provider execution receipt is missing authoritative status, exact result digest, or exact nonempty target")
		}
		executionReceiptDigest = agentTaskDigest(providerReceipt)
		if executionReceiptDigest == "" {
			return nil, errors.New("provider execution receipt is not canonically serializable")
		}
		claimedReceiptDigest := strings.TrimSpace(firstNonEmptyStrings(anyToString(providerReceipt["receipt_digest"]), anyToString(request["execution_receipt_digest"])))
		if claimedReceiptDigest != "" && claimedReceiptDigest != executionReceiptDigest {
			return nil, errors.New("provider execution receipt digest does not match its canonical content")
		}
		providerRef = strings.TrimSpace(anyToString(providerReceipt["provider_ref"]))
		if providerRef == "" {
			providerRef = receiptID
		}
	} else if strings.TrimSpace(anyToString(request["provider_ref"])) != "" {
		return nil, errors.New("provider_ref is accepted only from a verified provider-neutral execution receipt")
	}
	if _, truncated := agentTaskBoundedString(providerRef, 2048); truncated {
		return nil, errors.New("provider_ref exceeds the 2048 byte limit")
	}
	integrationID := strings.TrimSpace(anyToString(request["integration_id"]))
	integrationIDSupplied := integrationID != ""
	if _, truncated := agentTaskBoundedString(integrationID, 2048); truncated {
		return nil, errors.New("integration_id exceeds the 2048 byte limit")
	}
	var existingID, existingResultID, existingTaskID, existingAction, existingStatus, existingActor, existingDigest string
	var existingTarget, existingPolicyDigest, existingApprovalID, existingApprovalExpiresAt, existingApprovalPolicyVersion string
	var existingPolicyEvidenceDigest, existingProviderRef, existingExecutionReceiptDigest, existingError string
	var existingMergeAllowed int
	var lookupErr error
	if integrationIDSupplied {
		lookupErr = tx.QueryRowContext(ctx, `SELECT integration_id,result_id,task_id,action,status,actor,digest,target,policy_digest,approval_id,approval_expires_at,approval_policy_version,policy_evidence_digest,provider_ref,execution_receipt_digest,merge_allowed,error FROM task_ledger_integrations WHERE integration_id=?`, integrationID).Scan(&existingID, &existingResultID, &existingTaskID, &existingAction, &existingStatus, &existingActor, &existingDigest, &existingTarget, &existingPolicyDigest, &existingApprovalID, &existingApprovalExpiresAt, &existingApprovalPolicyVersion, &existingPolicyEvidenceDigest, &existingProviderRef, &existingExecutionReceiptDigest, &existingMergeAllowed, &existingError)
	} else {
		lookupErr = tx.QueryRowContext(ctx, `SELECT integration_id,result_id,task_id,action,status,actor,digest,target,policy_digest,approval_id,approval_expires_at,approval_policy_version,policy_evidence_digest,provider_ref,execution_receipt_digest,merge_allowed,error FROM task_ledger_integrations WHERE result_id=? AND action=? AND digest=?`, resultID, action, digest).Scan(&existingID, &existingResultID, &existingTaskID, &existingAction, &existingStatus, &existingActor, &existingDigest, &existingTarget, &existingPolicyDigest, &existingApprovalID, &existingApprovalExpiresAt, &existingApprovalPolicyVersion, &existingPolicyEvidenceDigest, &existingProviderRef, &existingExecutionReceiptDigest, &existingMergeAllowed, &existingError)
	}
	if lookupErr == nil {
		if existingResultID != resultID || existingTaskID != taskID || existingAction != action || existingDigest != digest || existingActor != actor {
			return nil, errors.New("integration replay identity does not exactly match the recorded task, result, action, digest, and actor")
		}
		if existingTarget != target {
			return nil, errors.New("integration replay target does not exactly match the recorded immutable target")
		}
		if existingPolicyDigest == "" || existingPolicyDigest != policyDigest {
			return nil, errors.New("integration replay approval policy does not exactly match the recorded policy evidence")
		}
		existingApproval := agentTaskApprovalEvidence{ID: existingApprovalID, ExpiresAt: existingApprovalExpiresAt, PolicyVersion: existingApprovalPolicyVersion}
		expectedPolicyEvidenceDigest := agentTaskIntegrationPolicyEvidenceDigest(taskID, resultID, attemptID, action, digest, target, policyDigest, approvalRequired, existingApproval)
		if existingPolicyEvidenceDigest == "" || existingPolicyEvidenceDigest != expectedPolicyEvidenceDigest || approvalRequired != (existingApprovalID != "") {
			return nil, errors.New("integration replay policy or approval expiry evidence does not exactly match the recorded decision")
		}
		if existingApprovalID != "" {
			var approvalTaskID, approvalAttemptID, approvalDigest, approvalTarget, approvalVersion, approver, approvalExpiry, approvalStatus, approvalUsedAt string
			if err := tx.QueryRowContext(ctx, `SELECT task_id,attempt_id,result_or_commit_digest,target,policy_version,approver,expires_at,status,used_at FROM task_ledger_approvals WHERE approval_id=?`, existingApprovalID).Scan(&approvalTaskID, &approvalAttemptID, &approvalDigest, &approvalTarget, &approvalVersion, &approver, &approvalExpiry, &approvalStatus, &approvalUsedAt); err != nil {
				return nil, fmt.Errorf("integration replay approval evidence: %w", err)
			}
			if approvalTaskID != taskID || approvalAttemptID != attemptID || approvalDigest != digest || approvalTarget != target || approvalVersion != existingApprovalPolicyVersion || approvalExpiry != existingApprovalExpiresAt || !strings.EqualFold(approver, actor) || approvalStatus != "used" || approvalUsedAt == "" {
				return nil, errors.New("integration replay approval expiry evidence no longer matches its immutable source")
			}
		}
		if providerAction && (existingProviderRef != providerRef || existingExecutionReceiptDigest != executionReceiptDigest) {
			return nil, errors.New("integration replay provider receipt does not exactly match the recorded execution evidence")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		payload := map[string]any{"schema_id": agentTaskIntegrationContractID, "contract_version": 1, "integration_id": existingID, "result_id": existingResultID, "task_id": existingTaskID, "action": existingAction, "status": existingStatus, "actor": existingActor, "digest": existingDigest, "target": existingTarget, "policy_digest": existingPolicyDigest, "approval_required": existingApprovalID != "", "approval_id": existingApprovalID, "approval_expires_at": existingApprovalExpiresAt, "approval_policy_version": existingApprovalPolicyVersion, "policy_evidence_digest": existingPolicyEvidenceDigest, "provider_ref": existingProviderRef, "execution_receipt_digest": existingExecutionReceiptDigest, "merge_allowed": existingMergeAllowed != 0}
		if existingAction == "follow_up_task" {
			payload["follow_up_task_id"] = existingProviderRef
		}
		if existingError != "" {
			payload["error"] = existingError
		}
		return agentTaskContractPayload(agentTaskIntegrationContractID, payload), nil
	} else if !errors.Is(lookupErr, sql.ErrNoRows) {
		return nil, lookupErr
	}
	if integrationIDSupplied {
		var tupleIntegrationID string
		tupleErr := tx.QueryRowContext(ctx, `SELECT integration_id FROM task_ledger_integrations WHERE result_id=? AND action=? AND digest=?`, resultID, action, digest).Scan(&tupleIntegrationID)
		if tupleErr == nil {
			return nil, errors.New("integration replay supplied a different integration_id for an existing result action")
		}
		if !errors.Is(tupleErr, sql.ErrNoRows) {
			return nil, tupleErr
		}
	} else {
		integrationID, err = l.newUniqueID(ctx, tx, "integration", "integration")
		if err != nil {
			return nil, err
		}
	}
	// Core never performs a merge side effect.  A merge request is recorded as
	// rejected below and must not consume an approval intended for a provider
	// operation that Core does not execute.
	approval := agentTaskApprovalEvidence{PolicyVersion: policyVersion}
	if approvalRequired {
		var approvalErr error
		approval, approvalErr = l.validApprovalTx(ctx, tx, taskID, attemptID, digest, target, actor, policyVersion)
		if approvalErr != nil {
			return nil, fmt.Errorf("integration approval required: %w", approvalErr)
		}
	}
	approvalID := approval.ID
	policyEvidenceDigest := agentTaskIntegrationPolicyEvidenceDigest(taskID, resultID, attemptID, action, digest, target, policyDigest, approvalRequired, approval)
	if policyEvidenceDigest == "" {
		return nil, errors.New("integration policy evidence is not canonically serializable")
	}
	status := "integrated"
	taskTarget := "integrated"
	errorMessage := ""
	mergeAllowed := false
	followUpTaskID := ""
	switch action {
	case "merge":
		status = "rejected"
		taskTarget = "integration_failed"
		errorMessage = "automatic merge is not part of Core task delivery"
	case "leave_unintegrated":
		status = "unintegrated"
		taskTarget = "unintegrated"
	case "follow_up_task":
		status = "follow_up_queued"
		taskTarget = "unintegrated"
		followUpTaskID, err = l.newUniqueID(ctx, tx, "task", "task")
		if err != nil {
			return nil, err
		}
		providerRef = followUpTaskID
	case "promote_knowledge":
		if taskStatus != "knowledge_accepted" {
			return nil, fmt.Errorf("knowledge promotion requires knowledge_accepted review state, got %s", taskStatus)
		}
	case "open_pr", "update_pr", "export_patch":
		if taskStatus != "accepted_for_integration" && taskStatus != "integration_pending" {
			return nil, fmt.Errorf("integration action is not available from task status %s", taskStatus)
		}
	}
	if taskTarget != taskStatus && !agentTaskAllowedTransition(taskStatus, taskTarget) {
		if taskStatus == "accepted_for_integration" && taskTarget == "integrated" {
			// The explicit integration action records both pending and final state
			// in one authoritative transaction.
		} else {
			return nil, fmt.Errorf("invalid integration task transition %s -> %s", taskStatus, taskTarget)
		}
	}
	now := agentTaskNow()
	integrationPayload := agentTaskContractPayload(agentTaskIntegrationContractID, map[string]any{"schema_id": agentTaskIntegrationContractID, "contract_version": 1, "integration_id": integrationID, "result_id": resultID, "task_id": taskID, "action": action, "status": status, "actor": actor, "digest": digest, "target": target, "policy_digest": policyDigest, "approval_required": approvalRequired, "approval_id": approval.ID, "approval_expires_at": approval.ExpiresAt, "approval_policy_version": approval.PolicyVersion, "policy_evidence_digest": policyEvidenceDigest, "provider_ref": providerRef, "execution_receipt_digest": executionReceiptDigest, "merge_allowed": mergeAllowed})
	if followUpTaskID != "" {
		integrationPayload["follow_up_task_id"] = followUpTaskID
	}
	if errorMessage != "" {
		integrationPayload["error"] = errorMessage
	}
	if err := agentTaskRequireContract(agentTaskIntegrationContractID, integrationPayload); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_integrations(integration_id,result_id,task_id,action,status,actor,digest,target,policy_digest,approval_id,approval_expires_at,approval_policy_version,policy_evidence_digest,provider_ref,execution_receipt_digest,merge_allowed,error,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, integrationID, resultID, taskID, action, status, actor, digest, target, policyDigest, approval.ID, approval.ExpiresAt, approval.PolicyVersion, policyEvidenceDigest, providerRef, executionReceiptDigest, boolToSQLiteInt(mergeAllowed), errorMessage, now); err != nil {
		return nil, err
	}
	if followUpTaskID != "" {
		var parentMetadataJSON string
		if err := tx.QueryRowContext(ctx, `SELECT metadata_json FROM task_ledger_tasks WHERE id=?`, taskID).Scan(&parentMetadataJSON); err != nil {
			return nil, err
		}
		followUpMetadata := decodeAgentTaskMap(parentMetadataJSON)
		followUpMetadata["parent_task_id"] = taskID
		followUpMetadata["parent_result_id"] = resultID
		followUpMetadata["integration_id"] = integrationID
		followUpIdempotency := "follow-up:" + resultID + ":" + strings.TrimPrefix(digest, "sha256:")
		followUpLegacy := encodeAgentTaskJSON(map[string]any{"follow_up": true, "parent_task_id": taskID, "parent_result_id": resultID, "integration_id": integrationID})
		if err := agentTaskValidateStructured(followUpMetadata, "follow-up task metadata", agentTaskEventMaxBytes*4); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_ledger_tasks(id,project,workspace_id,title,objective,acceptance_json,task_class,execution_profile,risk_level,approval_policy_json,context_request_json,recipients_json,review_owner,requesting_agent_id,idempotency_key,priority,claim_eligible,claim_worker_id,max_execution_attempts,status,generation,attempt_number,active_attempt_id,result_id,publication_id,approved,metadata_json,legacy_payload_json,created_at,updated_at) SELECT ?,project,workspace_id,title,objective,acceptance_json,task_class,execution_profile,risk_level,approval_policy_json,context_request_json,recipients_json,review_owner,requesting_agent_id,?,priority,CASE WHEN (COALESCE(json_extract(approval_policy_json,'$.required'),0)=0 OR approved=1) AND context_request_json LIKE '%"content_hash":"sha256:%' AND COALESCE(json_extract(context_request_json,'$.session_id'),'')<>'' THEN 1 ELSE 0 END,claim_worker_id,max_execution_attempts,'queued',0,0,'','','',approved,?,?,?,? FROM task_ledger_tasks WHERE id=?`, followUpTaskID, followUpIdempotency, encodeAgentTaskJSON(followUpMetadata), followUpLegacy, now, now, taskID); err != nil {
			return nil, err
		}
		followUpTask, err := l.queryTaskTx(ctx, tx, followUpTaskID)
		if err != nil {
			return nil, err
		}
		followUpDigest := agentTaskDigest(followUpTask)
		if followUpDigest == "" {
			return nil, errors.New("follow-up task manifest is not canonically serializable")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET legacy_payload_json=? WHERE id=?`, encodeAgentTaskJSON(map[string]any{"follow_up": true, "parent_task_id": taskID, "parent_result_id": resultID, "integration_id": integrationID, "manifest_digest": followUpDigest}), followUpTaskID); err != nil {
			return nil, err
		}
		if err := l.appendEventTx(ctx, tx, followUpTaskID, "", "queued", "canonical reviewer created a bounded follow-up task from immutable result evidence", map[string]any{"parent_task_id": taskID, "parent_result_id": resultID, "integration_id": integrationID, "manifest_digest": followUpDigest}); err != nil {
			return nil, err
		}
	}
	if taskStatus == "accepted_for_integration" && status == "integrated" {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='integration_pending',updated_at=? WHERE id=?`, now, taskID); err != nil {
			return nil, err
		}
		if err := l.appendEventTx(ctx, tx, taskID, attemptID, "integration_pending", "explicit reviewer integration action accepted", map[string]any{"integration_id": integrationID, "action": action, "target": target, "policy_digest": policyDigest, "policy_evidence_digest": policyEvidenceDigest, "approval_id": approvalID, "approval_expires_at": approval.ExpiresAt}); err != nil {
			return nil, err
		}
	}
	if taskTarget != taskStatus || (taskStatus == "accepted_for_integration" && status == "integrated") {
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status=?,updated_at=? WHERE id=?`, taskTarget, now, taskID); err != nil {
			return nil, err
		}
	}
	if err := l.appendEventTx(ctx, tx, taskID, attemptID, taskTarget, "explicit integration decision recorded", map[string]any{"integration_id": integrationID, "result_id": resultID, "action": action, "target": target, "policy_digest": policyDigest, "policy_evidence_digest": policyEvidenceDigest, "provider_ref": providerRef, "approval_id": approvalID, "approval_expires_at": approval.ExpiresAt, "merge_allowed": mergeAllowed, "error": errorMessage}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return integrationPayload, nil
}

func legacyAgentTaskRows(raw []byte) ([]map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return []map[string]any{}, nil
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("legacy task document is invalid json: %w", err)
	}
	rows := anySlice(document["tasks"])
	out := make([]map[string]any, 0, len(rows))
	for index, rawRow := range rows {
		row := cloneAnyMap(anyMap(rawRow))
		if len(row) == 0 {
			return nil, fmt.Errorf("legacy task row %d is not an object", index)
		}
		if strings.TrimSpace(anyToString(row["id"])) == "" {
			return nil, fmt.Errorf("legacy task row %d is missing its server-owned id", index)
		}
		out = append(out, row)
	}
	return out, nil
}

func legacyAgentTaskManifest(row map[string]any) map[string]any {
	payload := cloneAnyMap(anyMap(row["payload"]))
	if len(payload) == 0 {
		payload = cloneAnyMap(row)
	}
	taskID := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["task_id"]), anyToString(row["id"])))
	project := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["project"]), anyToString(payload["project"]), "default"))
	objective := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["objective"]), anyToString(payload["objective"]), anyToString(row["title"]), "migrated legacy task"))
	recipient := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["agent"]), anyToString(payload["agent"]), "gateway-reviewer"))
	acceptance := anyToStringSlice(row["acceptance_criteria"])
	if len(acceptance) == 0 {
		acceptance = anyToStringSlice(payload["acceptance_criteria"])
	}
	if len(acceptance) == 0 {
		acceptance = []string{"Review the migrated legacy task result and record an explicit outcome."}
	}
	metadata := cloneAnyMap(anyMap(row["metadata"]))
	metadata["legacy_status"] = anyToString(row["status"])
	metadata["legacy_id"] = taskID
	manifest := map[string]any{
		"task_id": taskID, "project": project,
		"title":     firstNonEmptyStrings(anyToString(row["title"]), objective),
		"objective": objective, "acceptance_criteria": acceptance,
		"task_class":        firstNonEmptyStrings(anyToString(row["task_class"]), anyToString(payload["task_class"]), "non_coding"),
		"execution_profile": firstNonEmptyStrings(anyToString(row["execution_profile"]), anyToString(payload["execution_profile"]), "legacy-compatible"),
		"risk_level":        firstNonEmptyStrings(anyToString(row["risk_level"]), anyToString(payload["risk_level"]), "low"),
		"approval_policy":   map[string]any{"required": anyToBool(row["approval_required"]) || anyToBool(payload["approval_required"])},
		"context_request":   cloneAnyMap(anyMap(row["context_request"])),
		"recipients":        []any{map[string]any{"principal_id": recipient, "role": "reviewer", "project": project, "observer": false}},
		"review_owner":      recipient, "idempotency_key": "legacy:" + taskID,
		"priority": anyToInt(row["priority"], 0), "metadata": metadata,
		"approved": anyToBool(row["approved"]) || anyToBool(payload["approved"]),
	}
	return manifest
}

func canonicalAgentTaskMigrationPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("legacy task source path is empty")
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", fmt.Errorf("resolve legacy task source path: %w", err)
	}
	absolute = filepath.Clean(absolute)
	parent := filepath.Dir(absolute)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil || filepath.Clean(resolvedParent) != parent {
		return "", errors.New("legacy task source parent must be an existing canonical path without symlink components")
	}
	info, statErr := os.Lstat(absolute)
	if errors.Is(statErr, os.ErrNotExist) {
		return absolute, nil
	}
	if statErr != nil {
		return "", fmt.Errorf("inspect legacy task source path: %w", statErr)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("legacy task source must be a regular file without symlinks")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil || filepath.Clean(resolved) != absolute {
		return "", errors.New("legacy task source must be a canonical path without symlink components")
	}
	return absolute, nil
}

func agentTaskMigrationAllowedPaths() ([]string, error) {
	configured := []string{agentTasksPath()}
	if extra := strings.TrimSpace(os.Getenv("GO_AGENT_TASK_MIGRATION_SOURCE_PATHS")); extra != "" {
		configured = append(configured, filepath.SplitList(extra)...)
	}
	seen := map[string]bool{}
	allowed := make([]string, 0, len(configured))
	for _, candidate := range configured {
		canonical, err := canonicalAgentTaskMigrationPath(candidate)
		if err != nil {
			return nil, fmt.Errorf("canonicalize server-owned migration source: %w", err)
		}
		if !seen[canonical] {
			seen[canonical] = true
			allowed = append(allowed, canonical)
		}
	}
	return allowed, nil
}

func agentTaskMigrationSourcePath(request map[string]any) (string, error) {
	allowed, err := agentTaskMigrationAllowedPaths()
	if err != nil {
		return "", err
	}
	requested := strings.TrimSpace(anyToString(request["source_path"]))
	if requested == "" {
		return allowed[0], nil
	}
	canonical, err := canonicalAgentTaskMigrationPath(requested)
	if err != nil {
		return "", err
	}
	for _, candidate := range allowed {
		if canonical == candidate {
			return canonical, nil
		}
	}
	return "", errors.New("legacy task source is not a server-owned allowlisted path")
}

func (l *agentTaskDeliveryLedger) migration(ctx context.Context, request map[string]any) (map[string]any, error) {
	if err := agentTaskValidateStructured(request, "legacy migration request", agentTaskEventMaxBytes); err != nil {
		return nil, err
	}
	phase := strings.TrimSpace(strings.ToLower(firstNonEmptyStrings(anyToString(request["phase"]), "validate")))
	if phase != "import" && phase != "freeze" && phase != "validate" && phase != "rollback" {
		return nil, errors.New("migration phase must be import, freeze, validate, or rollback")
	}
	sourcePath, pathErr := agentTaskMigrationSourcePath(request)
	if pathErr != nil {
		return nil, pathErr
	}
	raw, readErr := readAgentTaskArtifactFileBounded(sourcePath, agentTaskLegacyMigrationMaxBytes)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, fmt.Errorf("read legacy task source: %w", readErr)
	}
	if errors.Is(readErr, os.ErrNotExist) {
		raw = []byte(`{"tasks":[]}`)
	}
	if _, scanErr := agentTaskScanCanonicalSecrets(raw, agentTaskLegacyMigrationMaxBytes, "legacy task source"); scanErr != nil {
		return nil, scanErr
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("legacy task document is invalid json: %w", err)
	}
	if err := agentTaskValidateStructured(document, "legacy task document", agentTaskLegacyMigrationMaxBytes); err != nil {
		return nil, err
	}
	sourceDigest := agentTaskBytesDigest(raw)
	var existingReceipt string
	if err := l.db.QueryRowContext(ctx, `SELECT receipt_id FROM task_ledger_migration_receipts WHERE source_digest=? AND phase=? ORDER BY created_at DESC LIMIT 1`, sourceDigest, phase).Scan(&existingReceipt); err == nil {
		return l.migrationReceipt(ctx, existingReceipt)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := legacyAgentTaskRows(raw)
	if err != nil {
		return nil, err
	}
	if len(rows) > agentTaskLegacyMigrationMaxRows {
		return nil, fmt.Errorf("legacy task migration exceeds the %d row limit", agentTaskLegacyMigrationMaxRows)
	}
	validatedManifests := make([]map[string]any, 0, len(rows))
	seenTaskIDs := map[string]bool{}
	seenIdempotencyKeys := map[string]bool{}
	for index, row := range rows {
		if err := agentTaskValidateStructured(row, fmt.Sprintf("legacy task row %d", index), agentTaskContextPackMaxBytes*2); err != nil {
			return nil, err
		}
		manifest := legacyAgentTaskManifest(row)
		normalized, _, normalizeErr := normalizeAgentTaskManifest(manifest)
		if normalizeErr != nil {
			return nil, fmt.Errorf("validate legacy task %s: %w", anyToString(manifest["task_id"]), normalizeErr)
		}
		if err := agentTaskValidateStructured(normalized, fmt.Sprintf("legacy task manifest %d", index), agentTaskContextPackMaxBytes*2); err != nil {
			return nil, err
		}
		taskID := anyToString(normalized["task_id"])
		idempotencyKey := anyToString(normalized["idempotency_key"])
		if seenTaskIDs[taskID] || seenIdempotencyKeys[idempotencyKey] {
			return nil, errors.New("legacy task migration contains duplicate task or idempotency identity")
		}
		seenTaskIDs[taskID] = true
		seenIdempotencyKeys[idempotencyKey] = true
		var existingID, existingKey, existingLegacyJSON string
		lookupErr := l.db.QueryRowContext(ctx, `SELECT id,idempotency_key,legacy_payload_json FROM task_ledger_tasks WHERE id=? OR idempotency_key=? LIMIT 1`, taskID, idempotencyKey).Scan(&existingID, &existingKey, &existingLegacyJSON)
		if lookupErr == nil {
			storedDigest := anyToString(decodeAgentTaskMap(existingLegacyJSON)["manifest_digest"])
			if existingID != taskID || existingKey != idempotencyKey || storedDigest != agentTaskDigest(normalized) {
				return nil, fmt.Errorf("legacy task %s conflicts with immutable authoritative evidence", taskID)
			}
		} else if !errors.Is(lookupErr, sql.ErrNoRows) {
			return nil, lookupErr
		}
		validatedManifests = append(validatedManifests, manifest)
	}
	details := map[string]any{"source_digest": sourceDigest, "phase": phase, "legacy_task_count": len(rows), "imported_task_ids": []string{}, "validation_errors": []string{}}
	if phase == "import" {
		ids := make([]string, 0, len(validatedManifests))
		for _, manifest := range validatedManifests {
			ids = append(ids, anyToString(manifest["task_id"]))
		}
		sort.Strings(ids)
		details["imported_task_ids"] = ids
	}
	if err := agentTaskValidateStructured(details, "legacy migration details", agentTaskEventMaxBytes*8); err != nil {
		return nil, err
	}
	imported := 0
	if phase == "import" {
		for _, manifest := range validatedManifests {
			if _, submitErr := l.submit(ctx, manifest); submitErr != nil {
				if strings.Contains(strings.ToLower(submitErr.Error()), "idempotency key already") {
					return nil, submitErr
				}
				return nil, fmt.Errorf("import legacy task %s: %w", anyToString(manifest["task_id"]), submitErr)
			}
			imported++
		}
	}
	if phase == "freeze" {
		if err := l.setMeta(ctx, "legacy_writes_frozen", "true"); err != nil {
			return nil, err
		}
		details["legacy_writes_frozen"] = true
	}
	validated := false
	if phase == "validate" {
		validationErrors := []string{}
		for _, row := range rows {
			taskID := strings.TrimSpace(firstNonEmptyStrings(anyToString(row["task_id"]), anyToString(row["id"])))
			if taskID == "" {
				validationErrors = append(validationErrors, "legacy task missing id")
				continue
			}
			if _, queryErr := l.queryTask(ctx, taskID); queryErr != nil {
				validationErrors = append(validationErrors, taskID+": missing from authoritative ledger")
			}
		}
		details["validation_errors"] = validationErrors
		validated = len(validationErrors) == 0
		details["validated"] = validated
		if !validated {
			return nil, errors.New("legacy task migration validation failed")
		}
	}
	rolledBack := false
	if phase == "rollback" {
		checkpoint := map[string]any{"source_digest": sourceDigest, "captured_at": agentTaskNow(), "authoritative_writer": "gateway-go"}
		if err := l.setMeta(ctx, "legacy_writes_rollback_checkpoint", encodeAgentTaskJSON(checkpoint)); err != nil {
			return nil, err
		}
		details["rollback_checkpoint"] = checkpoint
		details["rollback_is_non_destructive"] = true
		rolledBack = true
	}
	if err := agentTaskValidateStructured(details, "legacy migration receipt details", agentTaskEventMaxBytes*8); err != nil {
		return nil, err
	}
	receiptID := "migration_" + strings.TrimPrefix(sourceDigest, "sha256:")[:24] + "_" + phase
	now := agentTaskNow()
	_, err = l.db.ExecContext(ctx, `INSERT INTO task_ledger_migration_receipts(receipt_id,source_path,source_digest,phase,imported,validated,frozen,rolled_back,details_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, receiptID, sourcePath, sourceDigest, phase, boolToSQLiteInt(imported > 0 || phase == "import" && len(rows) == 0), boolToSQLiteInt(validated), boolToSQLiteInt(phase == "freeze"), boolToSQLiteInt(rolledBack), encodeAgentTaskJSON(details), now)
	if err != nil {
		var convergedReceipt string
		if lookupErr := l.db.QueryRowContext(ctx, `SELECT receipt_id FROM task_ledger_migration_receipts WHERE source_digest=? AND phase=?`, sourceDigest, phase).Scan(&convergedReceipt); lookupErr == nil {
			return l.migrationReceipt(ctx, convergedReceipt)
		}
		return nil, err
	}
	return l.migrationReceipt(ctx, receiptID)
}

func (l *agentTaskDeliveryLedger) migrationReceipt(ctx context.Context, receiptID string) (map[string]any, error) {
	var sourceDigest, phase, detailsJSON, createdAt string
	var imported, validated, frozen, rolledBack int
	if err := l.db.QueryRowContext(ctx, `SELECT source_digest,phase,imported,validated,frozen,rolled_back,details_json,created_at FROM task_ledger_migration_receipts WHERE receipt_id=?`, strings.TrimSpace(receiptID)).Scan(&sourceDigest, &phase, &imported, &validated, &frozen, &rolledBack, &detailsJSON, &createdAt); err != nil {
		return nil, err
	}
	return map[string]any{"receipt_id": receiptID, "source_digest": sourceDigest, "phase": phase, "imported": imported != 0, "validated": validated != 0, "frozen": frozen != 0, "rolled_back": rolledBack != 0, "details": decodeAgentTaskMap(detailsJSON), "created_at": createdAt, "authoritative_backend": "gateway-go-sqlite-wal"}, nil
}

func (l *agentTaskDeliveryLedger) recoverExpired(ctx context.Context, limit int) ([]map[string]any, error) {
	limit = clampInt(limit, 1, 1000)
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT attempt_id,task_id,lease_expires_at,status FROM task_ledger_attempts WHERE status IN ('leased','running','waiting_for_input') ORDER BY lease_expires_at ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	type expiredAttempt struct {
		attemptID, taskID, expiresAt, status string
	}
	candidates := []expiredAttempt{}
	now := time.Now().UTC()
	for rows.Next() {
		var item expiredAttempt
		if err := rows.Scan(&item.attemptID, &item.taskID, &item.expiresAt, &item.status); err != nil {
			rows.Close()
			return nil, err
		}
		expires, parseErr := time.Parse(time.RFC3339Nano, item.expiresAt)
		if parseErr != nil || !now.Before(expires) {
			candidates = append(candidates, item)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	recovered := make([]map[string]any, 0, len(candidates))
	for _, item := range candidates {
		var taskStatus string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM task_ledger_tasks WHERE id=?`, item.taskID).Scan(&taskStatus); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_attempts SET status='quarantined',completed_at=? WHERE attempt_id=? AND status IN ('leased','running','waiting_for_input')`, agentTaskNow(), item.attemptID); err != nil {
			return nil, err
		}
		if taskStatus == "leased" || taskStatus == "running" || taskStatus == "waiting_for_input" {
			if _, err := tx.ExecContext(ctx, `UPDATE task_ledger_tasks SET status='quarantined',updated_at=? WHERE id=?`, agentTaskNow(), item.taskID); err != nil {
				return nil, err
			}
		}
		if err := l.appendEventTx(ctx, tx, item.taskID, item.attemptID, "quarantined", "expired lease quarantined because runner termination is not verified", map[string]any{"previous_status": taskStatus, "attempt_status": item.status, "lease_expires_at": item.expiresAt, "recovery": true, "termination_verified": false, "next_action": "verify_runner_process_group_termination"}); err != nil {
			return nil, err
		}
		recovered = append(recovered, map[string]any{"task_id": item.taskID, "attempt_id": item.attemptID, "previous_status": taskStatus, "status": "quarantined", "reason": "lease_expired_termination_unverified", "termination_verified": false})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return recovered, nil
}

func (l *agentTaskDeliveryLedger) artifact(ctx context.Context, artifactID, principal string) (map[string]any, error) {
	artifactID = strings.TrimSpace(artifactID)
	principal = strings.TrimSpace(principal)
	if artifactID == "" || principal == "" {
		return nil, errors.New("artifact read requires artifact_id and principal")
	}
	var taskID, attemptID, name, digest, mediaType, redaction, redactionReceipt, accessJSON, retention, contentRef, contentPath string
	var sizeBytes int64
	var finalized int
	if err := l.db.QueryRowContext(ctx, `SELECT task_id,attempt_id,name,digest,size_bytes,media_type,redaction_status,redaction_receipt,access_policy_json,retention_expires_at,content_ref,content_path,finalized FROM task_ledger_artifacts WHERE artifact_id=?`, artifactID).Scan(&taskID, &attemptID, &name, &digest, &sizeBytes, &mediaType, &redaction, &redactionReceipt, &accessJSON, &retention, &contentRef, &contentPath, &finalized); err != nil {
		return nil, err
	}
	if finalized == 0 {
		return nil, errors.New("artifact is staged and not finalized")
	}
	expires, expiryErr := time.Parse(time.RFC3339Nano, retention)
	if expiryErr != nil {
		return nil, errors.New("artifact retention metadata is invalid")
	}
	if !time.Now().UTC().Before(expires) {
		return nil, errors.New("artifact reference has expired")
	}
	task, err := l.queryTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	authorized := strings.EqualFold(principal, "gateway-service") || strings.EqualFold(principal, anyToString(task["review_owner"]))
	for _, recipient := range agentTaskRecipientRows(task) {
		if strings.EqualFold(principal, anyToString(recipient["principal_id"])) {
			authorized = true
			break
		}
	}
	if !authorized {
		return nil, errors.New("artifact principal is not an authorized task recipient")
	}
	canonicalPath, pathErr := l.artifactPath(digest)
	if pathErr != nil {
		return nil, pathErr
	}
	if contentPath != "" && filepath.Clean(contentPath) != canonicalPath {
		return nil, errors.New("artifact database path is not the canonical digest path")
	}
	if sizeBytes < 0 || sizeBytes > l.limits.MaxArtifactBytes {
		return nil, errors.New("artifact size exceeds configured bound")
	}
	artifactPayload := agentTaskContractPayload(agentTaskArtifactContractID, map[string]any{"schema_id": agentTaskArtifactContractID, "contract_version": 1, "artifact_id": artifactID, "task_id": taskID, "attempt_id": attemptID, "name": name, "digest": digest, "size_bytes": sizeBytes, "media_type": mediaType, "redaction_status": redaction, "redaction_receipt": redactionReceipt, "access_policy": decodeAgentTaskMap(accessJSON), "retention_expires_at": retention, "content_ref": contentRef, "finalized": true})
	handle, openErr := l.openArtifactDescriptor(digest, l.limits.MaxArtifactBytes)
	if openErr != nil {
		return nil, errors.New("artifact content is unavailable or no longer matches its immutable digest")
	}
	verified, verifyErr := handle.readAndVerify(l.limits.MaxArtifactBytes)
	_ = handle.close()
	if verifyErr != nil || int64(len(verified)) != sizeBytes || handle.path != canonicalPath {
		return nil, errors.New("artifact content is unavailable or no longer matches its immutable digest")
	}
	return map[string]any{"artifact": artifactPayload, "authorized_principal": principal, "download": "scoped_stream"}, nil
}

func (l *agentTaskDeliveryLedger) artifactFile(ctx context.Context, artifactID, principal string) (*os.File, int64, error) {
	metadata, err := l.artifact(ctx, artifactID, principal)
	if err != nil {
		return nil, 0, err
	}
	artifact := anyMap(metadata["artifact"])
	digest := anyToString(artifact["digest"])
	handle, err := l.openArtifactDescriptor(digest, l.limits.MaxArtifactBytes)
	if err != nil {
		return nil, 0, err
	}
	content, err := handle.readAndVerify(l.limits.MaxArtifactBytes)
	if err != nil || int64(len(content)) != int64(anyToInt(artifact["size_bytes"], -1)) {
		_ = handle.close()
		return nil, 0, errors.New("artifact content is unavailable or failed immutable verification")
	}
	return handle.file, int64(len(content)), nil
}

func (l *agentTaskDeliveryLedger) runtimeSnapshot(ctx context.Context) map[string]any {
	ledgerPathConfigured := false
	artifactStoreConfigured := false
	if l != nil {
		ledgerPathConfigured = strings.TrimSpace(l.path) != ""
		artifactStoreConfigured = strings.TrimSpace(l.artifactRoot) != ""
	}
	payload := map[string]any{
		"backend": "gateway-go", "owner": "gateway-go", "storage": "sqlite", "journal": "wal", "synchronous": "full",
		"schema_version": agentTaskLedgerSchemaVersion, "sole_authoritative_writer": true, "legacy_writer_mode": "read_only",
		"artifact_store": "external_bounded_content_addressed", "artifact_content_in_sqlite": false,
		"ledger_path_configured": ledgerPathConfigured, "artifact_store_configured": artifactStoreConfigured,
	}
	if l == nil || l.db == nil {
		payload["ready"] = false
		payload["error"] = "ledger unavailable"
		return payload
	}
	payload["ready"] = true
	counts := map[string]int{}
	rows, err := l.db.QueryContext(ctx, `SELECT status,COUNT(*) FROM task_ledger_tasks GROUP BY status`)
	if err == nil {
		for rows.Next() {
			var status string
			var count int
			if scanErr := rows.Scan(&status, &count); scanErr == nil {
				counts[status] = count
			}
		}
		rows.Close()
	}
	payload["task_counts"] = counts
	var pendingWriteback, pendingDeliveries, attempts int
	_ = l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_publications WHERE status='writeback_pending'`).Scan(&pendingWriteback)
	_ = l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_deliveries WHERE status IN ('pending','failed')`).Scan(&pendingDeliveries)
	_ = l.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM task_ledger_attempts WHERE status IN ('leased','running','waiting_for_input')`).Scan(&attempts)
	payload["pending_writeback"] = pendingWriteback
	payload["pending_deliveries"] = pendingDeliveries
	payload["active_attempts"] = attempts
	frozen, _ := l.getMeta(ctx, "legacy_writes_frozen")
	payload["legacy_writes_frozen"] = frozen == "true"
	return payload
}
