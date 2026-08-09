package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"
)

func newTestPassportStore(t testing.TB, root string) *contextPassportStore {
	t.Helper()
	store, err := newContextPassportStore(contextPassportStoreConfig{
		Enabled: true, Path: filepath.Join(root, "passports.ndjson"),
		KeyPath: filepath.Join(root, "identity.json"), MaxBytes: 512 * 1024,
		MaxEntries: 64, MaxItemBytes: 128 * 1024, Fsync: false,
	})
	if err != nil {
		t.Fatalf("new passport store: %v", err)
	}
	return store
}

func newTestMeshStore(t testing.TB, root string, passports *contextPassportStore) *contextMeshStore {
	t.Helper()
	store, err := newContextMeshStore(contextMeshStoreConfig{
		Enabled: true, Path: filepath.Join(root, "mesh.json"), MaxBytes: 256 * 1024,
		MaxGrants: 32, MaxRevocations: 32, MaxReceipts: 32, MaxEnvelopeBytes: 256 * 1024,
		MaxPlaintextBytes: 128 * 1024, Fsync: false,
	}, passports)
	if err != nil {
		t.Fatalf("new mesh store: %v", err)
	}
	return store
}

func signedTestPassport(t testing.TB, store *contextPassportStore, project, lineage string, revision int, parent *contextPassport, marker string) contextPassport {
	t.Helper()
	now := time.Now().UTC()
	passport := contextPassport{
		SchemaID: contextPassportContractID, Version: 1, LineageID: lineage,
		Project: project, Revision: revision, CreatedAt: now.Format(time.RFC3339Nano),
		ExpiresAt: now.Add(24 * time.Hour).Format(time.RFC3339Nano),
		Issuer: contextPassportIssuer{
			InstanceID: store.identity.InstanceID, AgentID: "test_agent",
			SigningKeyID:     store.identity.SigningKeyID,
			SigningPublicKey: store.identity.SigningPublicKey,
		},
		Scope:        map[string]any{"project": project, "topic_path": "tests"},
		Objective:    map[string]any{"objective": "prove " + marker},
		Claims:       []map[string]any{{"portable_id": "claim_" + marker, "statement": "claim " + marker}},
		Evidence:     []map[string]any{{"portable_id": "evidence_" + marker, "content_ref": "sha256:" + digestPrefix(marker, 32)}},
		Lineage:      map[string]any{"source_schema": synthesisPackV2ContractID},
		Capabilities: defaultPassportCapabilities(),
		Redactions:   map[string]any{"applied": false},
		Replay:       map[string]any{"project": project, "query": "query " + marker, "retrieval_mode": "balanced"},
	}
	if parent != nil {
		passport.ParentPassportID = parent.PassportID
		passport.ParentDigest = parent.ContentDigest
	}
	if err := signContextPassport(&passport, store.identity); err != nil {
		t.Fatalf("sign passport: %v", err)
	}
	return passport
}

func TestContextIdentityPrivateKeysNeverEnterPublicMetadata(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	metadata, err := json.Marshal(store.identity.publicMetadata())
	if err != nil {
		t.Fatal(err)
	}
	text := string(metadata)
	if strings.Contains(text, "AGE-SECRET-KEY") || strings.Contains(text, store.identity.SigningPrivateKey) || strings.Contains(text, "private_key\":") {
		t.Fatalf("public metadata leaked private key material: %s", text)
	}
	info, err := os.Stat(filepath.Join(root, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity key file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestContextPassportTamperAndExpiryFailClosed(t *testing.T) {
	store := newTestPassportStore(t, t.TempDir())
	passport := signedTestPassport(t, store, "contextlattice", "lineage_test", 1, nil, "root")
	if findings := verifyContextPassport(passport, time.Now().UTC(), true); len(findings) != 0 {
		t.Fatalf("valid passport findings: %v", findings)
	}
	tampered := passport
	tampered.Claims = append([]map[string]any(nil), passport.Claims...)
	tampered.Claims[0] = cloneMap(tampered.Claims[0])
	tampered.Claims[0]["statement"] = "tampered"
	findings := verifyContextPassport(tampered, time.Now().UTC(), true)
	if !containsString(findings, "content_digest_mismatch") || !containsString(findings, "signature_invalid") {
		t.Fatalf("tamper findings = %v", findings)
	}
	findings = verifyContextPassport(passport, time.Now().UTC().Add(48*time.Hour), true)
	if !containsString(findings, "passport_expired") {
		t.Fatalf("expiry findings = %v", findings)
	}
}

func TestContextPassportReconciliationPreservesConflictBranches(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	rootPassport := signedTestPassport(t, store, "contextlattice", "lineage_reconcile", 1, nil, "root")
	rootResult, err := store.record(rootPassport)
	if err != nil || rootResult.Action != "record_root" || !rootResult.Recorded {
		t.Fatalf("record root result=%+v err=%v", rootResult, err)
	}
	idempotent, err := store.record(rootPassport)
	if err != nil || !idempotent.Idempotent || idempotent.Recorded {
		t.Fatalf("idempotent result=%+v err=%v", idempotent, err)
	}
	child := signedTestPassport(t, store, "contextlattice", rootPassport.LineageID, 2, &rootPassport, "child-a")
	advanced, err := store.record(child)
	if err != nil || advanced.Action != "advance" || advanced.Conflict {
		t.Fatalf("advance result=%+v err=%v", advanced, err)
	}
	branch := signedTestPassport(t, store, "contextlattice", rootPassport.LineageID, 2, &rootPassport, "child-b")
	conflict, err := store.record(branch)
	if err != nil || !conflict.Conflict || !conflict.PreservedAsBranch || !conflict.Recorded {
		t.Fatalf("conflict result=%+v err=%v", conflict, err)
	}
	if len(store.passports) != 3 {
		t.Fatalf("passport count=%d, want 3", len(store.passports))
	}
	reloaded := newTestPassportStore(t, root)
	if len(reloaded.passports) != 3 || !reloaded.reconciliations[branch.PassportID].Recorded {
		t.Fatalf("reloaded state missing recorded conflict: %+v", reloaded.reconciliations[branch.PassportID])
	}
}

func TestContextPassportBatchIsConflictFreeAndRejectsIncompleteTransactions(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	first := signedTestPassport(t, store, "contextlattice", "lineage_batch_a", 1, nil, "first")
	second := signedTestPassport(t, store, "contextlattice", "lineage_batch_b", 1, nil, "second")
	reconciliations, err := store.recordBatch([]contextPassport{first, second}, true)
	if err != nil || len(reconciliations) != 2 || len(store.passports) != 2 {
		t.Fatalf("record batch reconciliations=%+v err=%v count=%d", reconciliations, err, len(store.passports))
	}
	conflict := signedTestPassport(t, store, "contextlattice", first.LineageID, 1, nil, "conflict")
	third := signedTestPassport(t, store, "contextlattice", "lineage_batch_c", 1, nil, "third")
	if _, err := store.recordBatch([]contextPassport{conflict, third}, true); err == nil {
		t.Fatal("conflicting batch was accepted")
	}
	if len(store.passports) != 2 {
		t.Fatalf("rejected batch partially mutated memory: %d", len(store.passports))
	}

	incomplete := contextPassportLedgerRow{
		SchemaID: contextPassportLedgerSchemaID, RecordedAt: nowUTCISO(),
		BatchID: "passport_batch_incomplete", BatchIndex: 0, BatchSize: 2,
		Passport: third, Reconciliation: contextPassportReconciliation{
			Action: "record_root", Reason: "new_lineage_root", PassportID: third.PassportID,
			LineageID: third.LineageID, Revision: third.Revision, Recorded: true,
		},
	}
	if err := store.appendRows([]contextPassportLedgerRow{incomplete}); err != nil {
		t.Fatal(err)
	}
	_, err = newContextPassportStore(contextPassportStoreConfig{
		Enabled: true, Path: filepath.Join(root, "passports.ndjson"),
		KeyPath: filepath.Join(root, "identity.json"), MaxBytes: 512 * 1024,
		MaxEntries: 64, MaxItemBytes: 128 * 1024, Fsync: false,
	})
	if err == nil {
		t.Fatal("incomplete transaction was accepted as a valid ledger prefix")
	}
}

func TestContextPassportCompactionFailurePreservesInMemoryState(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	passport := signedTestPassport(t, store, "contextlattice", "lineage_compaction_failure", 1, nil, "root")
	if _, err := store.record(passport); err != nil {
		t.Fatal(err)
	}
	store.maxBytes = 1
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	err := store.compactIfNeeded()
	if chmodErr := os.Chmod(root, 0o700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil {
		t.Skip("filesystem permits temp-file creation in a read-only directory")
	}
	if _, ok := store.get(passport.PassportID); !ok || len(store.passports) != 1 {
		t.Fatalf("failed compaction pruned in-memory passport: count=%d", len(store.passports))
	}
}

func TestContextPassportLedgerRejectsRollbackAndMalformedTail(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	first := signedTestPassport(t, store, "contextlattice", "lineage_anchor", 1, nil, "first")
	if _, err := store.record(first); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(root, "passports.ndjson")
	oldLedger, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	second := signedTestPassport(t, store, "contextlattice", first.LineageID, 2, &first, "second")
	if _, err := store.record(second); err != nil {
		t.Fatal(err)
	}
	anchorRaw, err := os.ReadFile(ledgerPath + ".anchor")
	if err != nil {
		t.Fatal(err)
	}
	if len(anchorRaw) > 4096 || bytes.Count(anchorRaw, []byte{'\n'}) != 1 {
		t.Fatalf("passport anchor is not a bounded single checkpoint: bytes=%d lines=%d", len(anchorRaw), bytes.Count(anchorRaw, []byte{'\n'}))
	}
	if err := os.WriteFile(ledgerPath, oldLedger, 0o600); err != nil {
		t.Fatal(err)
	}
	// Restore only the ledger prefix while retaining the current owner-only
	// anchor. A valid older prefix must not become authoritative.
	if _, err := newContextPassportStore(contextPassportStoreConfig{
		Enabled: true, Path: ledgerPath, KeyPath: filepath.Join(root, "identity.json"),
		MaxBytes: 512 * 1024, MaxEntries: 64, MaxItemBytes: 128 * 1024,
	}); err == nil {
		t.Fatal("restored older passport ledger prefix was accepted")
	}

	if err := os.WriteFile(ledgerPath, append(oldLedger, []byte("{\"schema_id\":\"truncated")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newContextPassportStore(contextPassportStoreConfig{
		Enabled: true, Path: ledgerPath, KeyPath: filepath.Join(root, "identity.json"),
		MaxBytes: 512 * 1024, MaxEntries: 64, MaxItemBytes: 128 * 1024,
	}); err == nil {
		t.Fatal("malformed passport ledger tail was accepted")
	}
}

func TestContextPassportCommitUnknownDisablesWriter(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	anchorDirectory := filepath.Join(root, "anchor-directory")
	if err := os.Mkdir(anchorDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	store.anchorPath = anchorDirectory
	passport := signedTestPassport(t, store, "contextlattice", "lineage_commit_unknown", 1, nil, "first")
	if _, err := store.record(passport); err == nil || !ownerOnlyAtomicWriteCommitted(err) {
		t.Fatalf("passport anchor commit-unknown was not surfaced: %v", err)
	}
	if store.enabled || !strings.HasPrefix(store.lastError, "commit_unknown:") {
		t.Fatalf("passport store remained writable after commit-unknown: enabled=%v error=%q", store.enabled, store.lastError)
	}
	if _, err := store.record(passport); err == nil {
		t.Fatal("disabled passport store accepted a second write")
	}
}

func TestContextPassportLegacyLedgerMigratesToDurableChain(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	passport := signedTestPassport(t, store, "contextlattice", "lineage_legacy", 1, nil, "legacy")
	if _, err := store.record(passport); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(root, "passports.ndjson")
	anchorPath := ledgerPath + ".anchor"
	raw, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var legacy contextPassportLedgerRow
	if err := json.Unmarshal(bytes.TrimSpace(raw), &legacy); err != nil {
		t.Fatal(err)
	}
	legacy.PrevEntryHash, legacy.EntryHash = "", ""
	legacyRaw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledgerPath, append(legacyRaw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(anchorPath); err != nil {
		t.Fatal(err)
	}
	migrated := newTestPassportStore(t, root)
	if _, ok := migrated.get(passport.PassportID); !ok {
		t.Fatal("legacy passport was not retained after migration")
	}
	if _, err := os.Stat(anchorPath); err != nil {
		t.Fatalf("legacy anchor was not created: %v", err)
	}
	if _, err := newContextPassportStore(contextPassportStoreConfig{
		Enabled: true, Path: ledgerPath, KeyPath: filepath.Join(root, "identity.json"),
		MaxBytes: 512 * 1024, MaxEntries: 64, MaxItemBytes: 128 * 1024,
	}); err != nil {
		t.Fatalf("migrated passport ledger did not restart: %v", err)
	}
}

func TestContextPassportCompactionRechainsAndRestarts(t *testing.T) {
	root := t.TempDir()
	store := newTestPassportStore(t, root)
	first := signedTestPassport(t, store, "contextlattice", "lineage_compaction", 1, nil, "first")
	if _, err := store.record(first); err != nil {
		t.Fatal(err)
	}
	second := signedTestPassport(t, store, "contextlattice", first.LineageID, 2, &first, "second")
	if _, err := store.record(second); err != nil {
		t.Fatal(err)
	}
	store.maxBytes = 1
	if err := store.compactIfNeeded(); err != nil {
		t.Fatal(err)
	}
	restarted := newTestPassportStore(t, root)
	if len(restarted.passports) != 1 {
		t.Fatalf("compacted ledger retained %d passports, want one bounded row", len(restarted.passports))
	}
}

func TestPortableValueRedactsSecretsTokensAndMachineRoots(t *testing.T) {
	stats := &portableRedactionStats{}
	githubToken := "ghp" + "_abcdefghijklmnopqrstuvwxyz123456"
	value := portableMap(map[string]any{
		"api_key":      "must disappear",
		"note":         "Bearer abcdefghijklmnopqrstuvwxyz /Users/example/private " + githubToken,
		"token_budget": 4096,
	}, stats)
	encoded, _ := json.Marshal(value)
	text := string(encoded)
	for _, forbidden := range []string{"must disappear", "/Users/example", githubToken, "Bearer abcdefghijklmnopqrstuvwxyz"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("portable output retained %q: %s", forbidden, text)
		}
	}
	if anyToInt(value["token_budget"], 0) != 4096 || stats.SecretKeys == 0 || stats.Tokens == 0 || stats.Paths == 0 {
		t.Fatalf("redaction stats/value unexpected: value=%v stats=%+v", value, stats)
	}
}

func TestPortableSecretKeyCanonicalizesAliasesAndPreservesTokenBudgets(t *testing.T) {
	for _, key := range []string{
		"accessToken", "access-token", "ACCESS.TOKEN", "refreshToken", "refresh-token",
		"privateKey", "private/key", "apiKey", "API-Key", "clientSecret", "credentialID",
	} {
		if !portableSecretKey(key) {
			t.Fatalf("portable secret key %q was accepted", key)
		}
	}
	for _, key := range []string{"token_budget", "tokenBudget", "maxPromptTokens", "prompt-tokens", "provider.tokens", "estimatedTokens"} {
		if portableSecretKey(key) {
			t.Fatalf("legitimate token budget key %q was rejected", key)
		}
	}
}

func TestPortableValueRecursivelyDropsCanonicalSecretAliases(t *testing.T) {
	stats := &portableRedactionStats{}
	value := portableMap(map[string]any{
		"safe": map[string]any{
			"accessToken":     "nested-access-token",
			"mixed-separator": map[string]any{"private.Key": "nested-private-key"},
			"tokenBudget":     2048,
		},
		"items": []any{map[string]any{"apiKey": "nested-api-key", "refresh-token": "nested-refresh-token"}},
	}, stats)
	encoded, _ := json.Marshal(value)
	text := string(encoded)
	for _, forbidden := range []string{"nested-access-token", "nested-private-key", "nested-api-key", "nested-refresh-token", "accessToken", "private.Key", "apiKey", "refresh-token"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("portable output retained %q: %s", forbidden, text)
		}
	}
	if anyToInt(anyMap(value["safe"])["tokenBudget"], 0) != 2048 || stats.SecretKeys != 4 {
		t.Fatalf("recursive redaction stats/value unexpected: value=%v stats=%+v", value, stats)
	}
}

func TestContextPassportDiffTracksClaimsEvidenceAndParent(t *testing.T) {
	store := newTestPassportStore(t, t.TempDir())
	base := signedTestPassport(t, store, "contextlattice", "lineage_diff", 1, nil, "base")
	target := signedTestPassport(t, store, "contextlattice", base.LineageID, 2, &base, "target")
	diff := buildPassportDiff(base, target)
	if !anyToBool(diff["same_lineage"]) || !anyToBool(diff["parent_link_valid"]) {
		t.Fatalf("diff lineage invalid: %v", diff)
	}
	claims := anyMap(diff["claims"])
	if len(anyToStringSlice(claims["added"])) != 1 || len(anyToStringSlice(claims["removed"])) != 1 {
		t.Fatalf("claim diff = %v", claims)
	}
}

func TestContextMeshRoundTripWrongRecipientTamperAndRevocation(t *testing.T) {
	senderRoot := t.TempDir()
	receiverRoot := t.TempDir()
	wrongRoot := t.TempDir()
	senderPassports := newTestPassportStore(t, senderRoot)
	receiverPassports := newTestPassportStore(t, receiverRoot)
	wrongPassports := newTestPassportStore(t, wrongRoot)
	senderMesh := newTestMeshStore(t, senderRoot, senderPassports)
	receiverMesh := newTestMeshStore(t, receiverRoot, receiverPassports)
	wrongMesh := newTestMeshStore(t, wrongRoot, wrongPassports)
	server := &server{contextPassports: senderPassports, contextMesh: senderMesh}

	passport := signedTestPassport(t, senderPassports, "contextlattice", "lineage_mesh", 1, nil, "mesh")
	if _, err := senderPassports.record(passport); err != nil {
		t.Fatal(err)
	}
	grant, err := senderMesh.createGrant(map[string]any{
		"recipient_id": "receiver", "recipient": receiverMesh.identity.MeshRecipient,
		"project": "contextlattice", "ttl_secs": 3600,
	})
	if err != nil {
		t.Fatalf("create grant: %v", err)
	}
	envelope, err := server.createMeshEnvelope(passport, []string{grant.GrantID}, "")
	if err != nil {
		t.Fatalf("create envelope: %v", err)
	}
	payload, recipientKeyID, err := receiverMesh.decryptEnvelope(envelope)
	if err != nil || payload.Passport.PassportID != passport.PassportID || recipientKeyID != receiverMesh.identity.MeshKeyID {
		t.Fatalf("decrypt payload=%+v recipient=%s err=%v", payload, recipientKeyID, err)
	}
	if _, _, err := wrongMesh.decryptEnvelope(envelope); err == nil || !strings.Contains(err.Error(), "wrong recipient") {
		t.Fatalf("wrong recipient error = %v", err)
	}
	tampered := envelope
	tampered.Ciphertext = tampered.Ciphertext[:len(tampered.Ciphertext)-2] + "AA"
	if _, _, err := receiverMesh.decryptEnvelope(tampered); err == nil {
		t.Fatal("tampered envelope decrypted")
	}
	projectMismatch := envelope
	payload, _, err = receiverMesh.decryptEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	payload.Passport.Project = "different-project"
	if err := signContextPassport(&payload.Passport, senderPassports.identity); err != nil {
		t.Fatal(err)
	}
	payload.PassportDigest = payload.Passport.ContentDigest
	if err := signMeshPayload(&payload, senderPassports.identity); err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, receiverMesh.identityRecipientForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _ := json.Marshal(payload)
	_, _ = writer.Write(plaintext)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	projectMismatch.Ciphertext = base64.RawStdEncoding.EncodeToString(encrypted.Bytes())
	projectMismatch.CiphertextBytes = encrypted.Len()
	digest := sha256.Sum256(encrypted.Bytes())
	projectMismatch.CiphertextDigest = "sha256:" + hex.EncodeToString(digest[:])
	if err := signMeshEnvelope(&projectMismatch, senderPassports.identity); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiverMesh.decryptEnvelope(projectMismatch); err == nil || !strings.Contains(err.Error(), "passport_project_binding_mismatch") {
		t.Fatalf("project mismatch error = %v", err)
	}
	if _, _, err := receiverMesh.revokeGrant(grant.GrantID, "receiver denylist"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := receiverMesh.decryptEnvelope(envelope); err == nil || !strings.Contains(err.Error(), "locally revoked") {
		t.Fatalf("revoked grant error = %v", err)
	}
}

func (s *contextMeshStore) identityRecipientForTest(t testing.TB) age.Recipient {
	t.Helper()
	recipient, err := age.ParseX25519Recipient(s.identity.MeshRecipient)
	if err != nil {
		t.Fatal(err)
	}
	return recipient
}

func TestContextMeshImportIsDryRunThenIdempotentApply(t *testing.T) {
	senderRoot := t.TempDir()
	receiverRoot := t.TempDir()
	senderPassports := newTestPassportStore(t, senderRoot)
	receiverPassports := newTestPassportStore(t, receiverRoot)
	senderMesh := newTestMeshStore(t, senderRoot, senderPassports)
	receiverMesh := newTestMeshStore(t, receiverRoot, receiverPassports)
	sender := &server{contextPassports: senderPassports, contextMesh: senderMesh}
	receiver := &server{contextPassports: receiverPassports, contextMesh: receiverMesh}
	passport := signedTestPassport(t, senderPassports, "contextlattice", "lineage_import", 1, nil, "import")
	if _, err := senderPassports.record(passport); err != nil {
		t.Fatal(err)
	}
	grant, err := senderMesh.createGrant(map[string]any{"recipient_id": "receiver", "recipient": receiverMesh.identity.MeshRecipient, "project": "contextlattice", "ttl_secs": 3600})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := sender.createMeshEnvelope(passport, []string{grant.GrantID}, "")
	if err != nil {
		t.Fatal(err)
	}
	dryRun, status := receiver.reconcileMeshEnvelope(envelope, false)
	if status != 200 || !anyToBool(dryRun["ok"]) || anyToBool(dryRun["applied"]) || len(receiverPassports.passports) != 0 {
		t.Fatalf("dry run status=%d response=%v count=%d", status, dryRun, len(receiverPassports.passports))
	}
	applied, status := receiver.reconcileMeshEnvelope(envelope, true)
	if status != 200 || !anyToBool(applied["applied"]) || len(receiverPassports.passports) != 1 || len(receiverMesh.receipts) != 1 {
		t.Fatalf("apply status=%d response=%v passports=%d receipts=%d", status, applied, len(receiverPassports.passports), len(receiverMesh.receipts))
	}
	replayed, status := receiver.reconcileMeshEnvelope(envelope, true)
	reconciliation, _ := replayed["reconciliation"].(contextPassportReconciliation)
	if status != 200 || reconciliation.Action != "idempotent" || len(receiverPassports.passports) != 1 || len(receiverMesh.receipts) != 2 {
		t.Fatalf("idempotent status=%d response=%v passports=%d receipts=%d", status, replayed, len(receiverPassports.passports), len(receiverMesh.receipts))
	}
}

func TestContextMeshReclaimsOnlyInactiveGrantsAndBoundsRevocations(t *testing.T) {
	root := t.TempDir()
	passports := newTestPassportStore(t, root)
	mesh := newTestMeshStore(t, root, passports)
	mesh.maxGrants = 2
	mesh.maxRevocations = 3
	create := func(id string) contextMeshGrant {
		t.Helper()
		grant, err := mesh.createGrant(map[string]any{
			"recipient_id": id, "recipient": mesh.identity.MeshRecipient,
			"project": "contextlattice", "ttl_secs": 3600,
		})
		if err != nil {
			t.Fatal(err)
		}
		return grant
	}
	first := create("first")
	second := create("second")
	if _, _, err := mesh.revokeGrant(first.GrantID, "retired"); err != nil {
		t.Fatal(err)
	}
	third := create("third")
	if _, exists := mesh.grants[first.GrantID]; exists {
		t.Fatal("revoked grant was not reclaimed at capacity")
	}
	if _, exists := mesh.grants[second.GrantID]; !exists {
		t.Fatal("active grant was reclaimed")
	}
	if _, exists := mesh.grants[third.GrantID]; !exists {
		t.Fatal("new grant missing after capacity reclamation")
	}
	for index, grantID := range []string{"remote_one", "remote_two"} {
		reason := "local denylist"
		if index == 0 {
			reason = "Bearer abcdefghijklmnopqrstuvwxyz /Users/example/private"
		}
		_, revocation, err := mesh.revokeGrant(grantID, reason)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(revocation.Reason, "abcdefghijklmnopqrstuvwxyz") || strings.Contains(revocation.Reason, "/Users/") {
			t.Fatalf("revocation reason leaked portable secret/path: %q", revocation.Reason)
		}
	}
	if _, _, err := mesh.revokeGrant("remote_three", "capacity proof"); err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("revocation capacity error = %v", err)
	}
	if len(mesh.revocations) != 3 {
		t.Fatalf("revocation count=%d, want 3", len(mesh.revocations))
	}
}

func TestContextMeshGrantAndRevocationRoutesReturnValidContracts(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	root := t.TempDir()
	passports := newTestPassportStore(t, root)
	mesh := newTestMeshStore(t, root, passports)
	gateway := httptest.NewServer(buildMux(&server{contextPassports: passports, contextMesh: mesh}))
	defer gateway.Close()

	created := postJSONForTest(t, gateway.URL+"/memory/context-mesh/grants", `{
  "recipient_id":"local-test",
  "recipient":"`+mesh.identity.MeshRecipient+`",
  "project":"contextlattice",
  "ttl_secs":3600
}`)
	assertBoundaryContractPassed(t, contextMeshGrantContractID, created)
	assertBoundaryMetadata(t, created, "format_contract", false)
	grantID := anyToString(anyMap(created["grant"])["grant_id"])
	if grantID == "" {
		t.Fatalf("grant route omitted grant id: %#v", created)
	}

	for name, body := range map[string]string{
		"known_grant":    `{"grant_id":"` + grantID + `","reason":"route test"}`,
		"tombstone_only": `{"grant_id":"grant_remote_unknown","reason":"local denylist"}`,
	} {
		t.Run(name, func(t *testing.T) {
			revoked := postJSONForTest(t, gateway.URL+"/memory/context-mesh/grants/revoke", body)
			assertBoundaryContractPassed(t, contextMeshRevocationContractID, revoked)
			assertBoundaryMetadata(t, revoked, "format_contract", false)
			if anyToString(anyMap(revoked["revocation"])["grant_id"]) == "" {
				t.Fatalf("revocation route omitted grant id: %#v", revoked)
			}
		})
	}
}

func TestContextPassportAndMeshHTTPRoutesReturnValidContracts(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY", "")
	root := t.TempDir()
	passports := newTestPassportStore(t, root)
	mesh := newTestMeshStore(t, root, passports)
	server := &server{contextPassports: passports, contextMesh: mesh}
	gateway := httptest.NewServer(buildMux(server))
	defer gateway.Close()

	base := signedTestPassport(t, passports, "contextlattice", "lineage_http", 1, nil, "base")
	target := signedTestPassport(t, passports, "contextlattice", base.LineageID, 2, &base, "target")
	for _, passport := range []contextPassport{base, target} {
		if _, err := passports.record(passport); err != nil {
			t.Fatal(err)
		}
	}
	post := func(path string, payload map[string]any) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return postJSONForTest(t, gateway.URL+path, string(encoded))
	}
	assertContract := func(contractID string, payload map[string]any) {
		t.Helper()
		assertBoundaryContractPassed(t, contractID, payload)
		assertBoundaryMetadata(t, payload, "format_contract", false)
	}

	verified := post("/memory/context-passport/verify", map[string]any{"passport": base})
	assertContract(contextPassportVerifyContractID, verified)
	diffed := post("/memory/context-passport/diff", map[string]any{"base_passport_id": base.PassportID, "target_passport_id": target.PassportID})
	assertContract(contextPassportDiffContractID, diffed)
	replayed := post("/memory/context-passport/replay", map[string]any{"passport_id": target.PassportID})
	assertContract(contextPassportReplayContractID, replayed)
	if anyToBool(replayed["execution_performed"]) || anyToBool(replayed["ordinary_memory_mutated"]) {
		t.Fatalf("replay crossed instruction boundary: %#v", replayed)
	}
	imported := post("/memory/context-passport/import", map[string]any{"passport": base})
	assertContract(contextPassportContractID, imported)

	grant, err := mesh.createGrant(map[string]any{
		"recipient_id": "same-instance", "recipient": mesh.identity.MeshRecipient,
		"project": "contextlattice", "ttl_secs": 3600,
	})
	if err != nil {
		t.Fatal(err)
	}
	exported := post("/memory/context-mesh/export", map[string]any{"passport_id": target.PassportID, "grant_ids": []string{grant.GrantID}})
	assertContract(contextMeshEnvelopeContractID, exported)
	if anyToBool(exported["delivery_performed"]) || anyToBool(exported["transport_owned_by_contextlattice"]) {
		t.Fatalf("mesh export claimed transport: %#v", exported)
	}
	envelope := exported["envelope"]
	dryRun := post("/memory/context-mesh/import", map[string]any{"envelope": envelope})
	assertContract(contextMeshImportContractID, dryRun)
	if !anyToBool(dryRun["dry_run"]) || anyToBool(dryRun["applied"]) {
		t.Fatalf("mesh import was not dry-run first: %#v", dryRun)
	}
	applied := post("/memory/context-mesh/import", map[string]any{"envelope": envelope, "apply": true})
	assertContract(contextMeshImportContractID, applied)
	if !anyToBool(applied["applied"]) {
		t.Fatalf("explicit mesh apply failed: %#v", applied)
	}
}

func TestContextPassportAndMeshReadSurfacesRejectInvalidAuthorization(t *testing.T) {
	root := t.TempDir()
	passports := newTestPassportStore(t, root)
	mesh := newTestMeshStore(t, root, passports)
	gateway := httptest.NewServer(buildMux(&server{contextPassports: passports, contextMesh: mesh, orchestratorAPIKey: "required-test-key"}))
	defer gateway.Close()
	for _, path := range []string{
		"/memory/context-mesh/identity",
		"/memory/context-mesh/grants",
		"/telemetry/context-passport",
		"/telemetry/context-mesh",
	} {
		request, err := http.NewRequest(http.MethodGet, gateway.URL+path, nil)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		request.Header.Set("X-Api-Key", "wrong-key")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("get %s status=%d, want 401", path, response.StatusCode)
		}
	}
}

func TestStrictJSONDecodeRejectsTrailingValues(t *testing.T) {
	var value map[string]any
	if err := strictJSONDecode([]byte(`{"ok":true} {"extra":true}`), &value); err == nil {
		t.Fatal("trailing JSON value accepted")
	}
}

func BenchmarkContextPassportSignVerify(b *testing.B) {
	store := newTestPassportStore(b, b.TempDir())
	passport := signedTestPassport(b, store, "contextlattice", "lineage_bench", 1, nil, "bench")
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		candidate := passport
		candidate.Objective = map[string]any{"iteration": index}
		if err := signContextPassport(&candidate, store.identity); err != nil {
			b.Fatal(err)
		}
		if findings := verifyContextPassport(candidate, time.Now().UTC(), true); len(findings) != 0 {
			b.Fatal(findings)
		}
	}
}

func BenchmarkContextMeshEncryptDecrypt(b *testing.B) {
	root := b.TempDir()
	passports := newTestPassportStore(b, root)
	mesh := newTestMeshStore(b, root, passports)
	server := &server{contextPassports: passports, contextMesh: mesh}
	passport := signedTestPassport(b, passports, "contextlattice", "lineage_mesh_bench", 1, nil, "mesh-bench")
	grant, err := mesh.createGrant(map[string]any{
		"recipient_id": "same-instance", "recipient": mesh.identity.MeshRecipient,
		"project": "contextlattice", "ttl_secs": 3600,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		envelope, err := server.createMeshEnvelope(passport, []string{grant.GrantID}, "")
		if err != nil {
			b.Fatal(err)
		}
		payload, _, err := mesh.decryptEnvelope(envelope)
		if err != nil || payload.Passport.PassportID != passport.PassportID {
			b.Fatalf("decrypt err=%v passport=%s", err, payload.Passport.PassportID)
		}
	}
}
