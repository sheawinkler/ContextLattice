package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const temporalClaimTestObservation = "2025-01-01T00:00:00Z"
const temporalClaimTestCheck = "2025-01-01T01:00:00Z"

type temporalClaimTestSigner struct {
	issuer temporalClaimEvidenceIssuer
	key    ed25519.PrivateKey
}

func newTemporalClaimTestSigner(t *testing.T, origin, role string) temporalClaimTestSigner {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return temporalClaimTestSigner{
		issuer: temporalClaimEvidenceIssuer{
			Project: "test-project", TopicPath: "frontier/t5", PublicKey: base64.RawStdEncoding.EncodeToString(public),
			Role: role, OriginID: origin, Subject: "service", Predicate: "status",
		},
		key: private,
	}
}

func configureTemporalClaimTestSigners(t *testing.T, signers ...temporalClaimTestSigner) {
	t.Helper()
	issuers := []temporalClaimEvidenceIssuer{}
	for _, signer := range signers {
		issuers = append(issuers, signer.issuer)
	}
	raw, err := json.Marshal(issuers)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(temporalClaimEvidenceIssuerEnv, string(raw))
}

func temporalClaimTestPayload(id, object string) map[string]any {
	return map[string]any{
		"project": "test-project", "topic_path": "frontier/t5", "claim_id": id,
		"subject": "service", "predicate": "status", "object": object,
		"observed_at": temporalClaimTestObservation, "confidence": 0.8,
		"verification": map[string]any{"status": "verified", "checker": "writer-supplied"},
	}
}

func temporalClaimTestEvidence(id string) map[string]any {
	return map[string]any{
		"ref_id": id, "kind": "experiment", "uri": "https://source.example/result",
		"content_hash": sha256Hex("same measurement bytes"), "observed_at": temporalClaimTestObservation,
	}
}

func signedTemporalClaimTestEvidence(t *testing.T, signer temporalClaimTestSigner, payload map[string]any, polarity, observationID string, evidence map[string]any) map[string]any {
	t.Helper()
	return signTimedTemporalClaimTestEvidence(t, signer, payload, polarity, observationID, temporalClaimTestCheck, evidence)
}

func signTimedTemporalClaimTestEvidence(t *testing.T, signer temporalClaimTestSigner, payload map[string]any, polarity, observationID, checkedAt string, evidence map[string]any) map[string]any {
	t.Helper()
	claim, err := normalizeTemporalClaim(payload)
	if err != nil {
		t.Fatal(err)
	}
	receipt := temporalClaimEvidenceReceipt{ObservationID: observationID, CheckedAt: checkedAt}
	ref := normalizeClaimEvidence([]any{evidence}, 1)[0]
	raw, err := json.Marshal(temporalClaimEvidenceSigningPayload(claim, polarity, ref, receipt, signer.issuer))
	if err != nil {
		t.Fatal(err)
	}
	signed := cloneMap(evidence)
	signed["receipt"] = map[string]any{
		"observation_id": observationID, "checked_at": checkedAt,
		"signature": map[string]any{
			"algorithm": "Ed25519", "key_id": "ed25519:" + digestPrefix(signer.issuer.PublicKey, 24),
			"value": base64.RawStdEncoding.EncodeToString(ed25519.Sign(signer.key, raw)),
		},
	}
	return signed
}

func writeTemporalClaimTestHTTP(t *testing.T, s *server, payload map[string]any, tool bool) temporalClaim {
	t.Helper()
	write := s.memoryClaimsWrite
	path := "/memory/claims"
	if tool {
		write = s.toolsClaimWrite
		path = "/tools/claim_write"
		payload = map[string]any{"claim": payload}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	write(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw)))
	var result struct {
		Claim temporalClaim `json:"claim"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != http.StatusOK || result.Claim.ClaimID == "" {
		t.Fatalf("%s write: %d %s (%v)", path, w.Code, w.Body.String(), err)
	}
	return result.Claim
}

func temporalClaimTestRecommendationRow(recommendation map[string]any, id string) map[string]any {
	for _, raw := range contextPackAnyList(recommendation["claims"]) {
		if row := anyMap(raw); row["claim_id"] == id {
			return row
		}
	}
	return nil
}

func newTemporalClaimEvidenceTestServer(t *testing.T) *server {
	t.Helper()
	s := newFrontierT5PolicyLabTestServer(t)
	s.toolCalls.allowlist = map[string]struct{}{"/tools/claim_write": {}}
	return s
}

func TestTemporalClaimEvidenceWriterOwnerSourceAndBoundReceipt(t *testing.T) {
	s := newTemporalClaimEvidenceTestServer(t)
	owner := newTemporalClaimTestSigner(t, "owner-fixture", "owner")
	verifier := newTemporalClaimTestSigner(t, "experiment-fixture", "verifier")
	configureTemporalClaimTestSigners(t, owner, verifier)
	claim := temporalClaimTestPayload("a", "ready")
	entry := putFrontierT5TestMemory(t, s, "test-project", "notes/source.md", "original evidence", "active", "hot", nil)
	local := map[string]any{
		"kind": "memory", "memory_id": "test-project::notes/source.md",
		"content_hash": entry.ContentHash, "observed_at": temporalClaimTestObservation,
	}
	asserted := temporalClaimTestEvidence("asserted")
	asserted["admission"] = map[string]any{"authority": temporalClaimIndependentBinding, "observation_key": "sha256:" + sha256Hex("forged")}
	asserted["candidate_utility_verification"] = map[string]any{
		"independently_verified": true, "verification_status": "verified",
		"outcome_id": "result-a", "evidence_digest": "sha256:" + sha256Hex("not claim proof"),
	}
	claim["support"] = []any{
		asserted, local,
		signedTemporalClaimTestEvidence(t, owner, claim, "support", "owner-1", temporalClaimTestEvidence("owner")),
		signedTemporalClaimTestEvidence(t, verifier, claim, "support", "test-run-1", local),
	}
	admitted := writeTemporalClaimTestHTTP(t, s, claim, false)
	want := []string{temporalClaimWriterAssertion, temporalClaimCheckedSource, temporalClaimOwnerStatement, temporalClaimIndependentBinding}
	for i, ref := range admitted.Support {
		if got := temporalClaimEvidenceAuthority(ref); got != want[i] {
			t.Fatalf("support[%d] authority=%s, want=%s; ref=%#v", i, got, want[i], ref)
		}
	}
	if len(admitted.Support) != len(want) || admitted.Verification["status"] != "verified" ||
		!admitted.Support[3].Admission.SourceChecked || len(temporalClaimBoundObservations(admitted, admitted.Support, configuredTemporalClaimEvidenceIssuers(), time.Now())) != 1 {
		t.Fatalf("evidence lanes or historical label lost: %#v", admitted)
	}
	if !utilitySHA256DigestValid(admitted.Support[3].Admission.ReceiptDigest) {
		t.Fatalf("admitted receipt digest missing: %#v", admitted.Support[3].Admission)
	}
	legacy := temporalClaimTestPayload("legacy", "blocked")
	legacy["support"] = []any{temporalClaimTestEvidence("old-citation")}
	upsertFrontierT5Claim(t, s, legacy)
	reopened := &temporalClaimStore{path: s.temporalClaims.path, maxClaims: 256, enabled: true, claims: map[string]temporalClaim{}}
	if err := reopened.load(); err != nil {
		t.Fatal(err)
	}
	reloaded := reopened.claims["a"]
	if got := temporalClaimBoundObservations(reloaded, reloaded.Support, configuredTemporalClaimEvidenceIssuers(), time.Now()); len(got) != 1 {
		t.Fatalf("ledger replay lost admitted proof: %#v", got)
	}

	// An out-of-band byte change leaves the index untouched. A fresh claim
	// admission must check the actual bytes and reject the old revision digest.
	path := filepath.Join(s.memoryStore.policy.rootPath, "test-project", "notes", "source.md")
	if err := os.WriteFile(path, []byte("different content"), 0o600); err != nil {
		t.Fatal(err)
	}
	historical := reopened.claims["legacy"]
	if historical.Verification["status"] != "verified" || len(historical.Support) != 1 ||
		len(temporalClaimBoundObservations(historical, historical.Support, configuredTemporalClaimEvidenceIssuers(), time.Now())) != 0 ||
		anyMap(temporalClaimMaps([]temporalClaim{historical})[0])["verification_status_source"] != "writer_reported" {
		t.Fatalf("legacy verification was lost or upgraded on read: %#v", historical)
	}
	changed := writeTemporalClaimTestHTTP(t, s, claim, true)
	if changed.Support[1].Admission != nil || changed.Support[3].Admission != nil {
		t.Fatalf("changed bytes were admitted: %#v", changed.Support)
	}
}

func TestTemporalClaimEvidenceRejectsForgedAndDifferentScopeReceipts(t *testing.T) {
	s := newFrontierT5PolicyLabTestServer(t)
	verifier := newTemporalClaimTestSigner(t, "experiment-fixture", "verifier")
	owner := newTemporalClaimTestSigner(t, "owner-fixture", "owner")
	unknown := newTemporalClaimTestSigner(t, "unregistered", "verifier")
	configureTemporalClaimTestSigners(t, verifier, owner)
	claim := temporalClaimTestPayload("a", "ready")
	evidence := temporalClaimTestEvidence("ref")
	valid := signedTemporalClaimTestEvidence(t, verifier, claim, "support", "valid-1", evidence)

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any, map[string]any) map[string]any
	}{
		{"forged signature", func(_ map[string]any, ref map[string]any) map[string]any {
			sig := anyMap(anyMap(ref["receipt"])["signature"])
			sig["value"] = base64.RawStdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
			return ref
		}},
		{"unknown issuer", func(c, _ map[string]any) map[string]any {
			return signedTemporalClaimTestEvidence(t, unknown, c, "support", "x", evidence)
		}},
		{"different project", func(c, _ map[string]any) map[string]any {
			c["project"] = "other-project"
			return signedTemporalClaimTestEvidence(t, verifier, c, "support", "x", evidence)
		}},
		{"different topic", func(c, _ map[string]any) map[string]any {
			c["topic_path"] = "other/topic"
			return signedTemporalClaimTestEvidence(t, verifier, c, "support", "x", evidence)
		}},
		{"different claim", func(c, ref map[string]any) map[string]any { c["claim_id"] = "b"; return ref }},
		{"different object", func(c, ref map[string]any) map[string]any { c["object"] = "blocked"; return ref }},
		{"changed digest", func(_ map[string]any, ref map[string]any) map[string]any {
			ref["content_hash"] = sha256Hex("different bytes")
			return ref
		}},
		{"invalid observation time", func(c, _ map[string]any) map[string]any {
			ref := cloneMap(evidence)
			ref["observed_at"] = "yesterday"
			return signedTemporalClaimTestEvidence(t, verifier, c, "support", "x", ref)
		}},
		{"check predates observation", func(c, _ map[string]any) map[string]any {
			return signTimedTemporalClaimTestEvidence(t, verifier, c, "support", "x", "2024-12-31T23:59:59Z", evidence)
		}},
		{"future check", func(c, _ map[string]any) map[string]any {
			return signTimedTemporalClaimTestEvidence(t, verifier, c, "support", "x", time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano), evidence)
		}},
		{"opposite polarity", func(c, _ map[string]any) map[string]any {
			return signedTemporalClaimTestEvidence(t, verifier, c, "opposition", "x", evidence)
		}},
		{"owner outside subject", func(c, _ map[string]any) map[string]any {
			c["subject"] = "unrelated"
			return signedTemporalClaimTestEvidence(t, owner, c, "support", "x", evidence)
		}},
		{"different scope memory", func(c, _ map[string]any) map[string]any {
			entry := putFrontierT5TestMemory(t, s, "other-project", "notes/foreign.md", "foreign", "active", "hot", nil)
			ref := map[string]any{"kind": "memory", "memory_id": "other-project::notes/foreign.md",
				"content_hash": entry.ContentHash, "observed_at": temporalClaimTestObservation}
			return signedTemporalClaimTestEvidence(t, verifier, c, "support", "x", ref)
		}},
		{"memory citation without an exact ID", func(c, _ map[string]any) map[string]any {
			ref := cloneMap(evidence)
			ref["kind"] = "memory"
			return signedTemporalClaimTestEvidence(t, verifier, c, "support", "x", ref)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cloneJSONMap(claim)
			c["support"] = []any{tc.mutate(c, cloneJSONMap(valid))}
			got := writeTemporalClaimTestHTTP(t, s, c, false)
			if len(got.Support) != 1 || got.Support[0].Admission != nil {
				t.Fatalf("unbound citation was admitted: %#v", got.Support)
			}
		})
	}
}

func TestFrontierT5ContradictionNonCurrentClaimsAreNotLiveAdvice(t *testing.T) {
	signer := newTemporalClaimTestSigner(t, "experiment", "verifier")
	configureTemporalClaimTestSigners(t, signer)
	for _, tc := range []struct {
		name, field, value, exclusion string
	}{
		{"failed", "verification", "failed", "verification_failed"},
		{"retracted", "status", "retracted", "claim_not_current"},
		{"superseded", "status", "superseded", "claim_not_current"},
		{"expired", "status", "expired", "claim_not_current"},
		{"legacy historical", "legacy_status", "historical", "claim_not_current"},
		{"validity ended", "valid_to", "2025-01-02T00:00:00Z", "claim_not_current"},
		{"validity not started", "valid_from", time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), "claim_not_current"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTemporalClaimEvidenceTestServer(t)
			blocked := temporalClaimTestPayload("excluded", "contradictory")
			blocked["confidence"] = 1.0
			blocked["contradicts"] = []any{"current"}
			if tc.field == "verification" {
				blocked["verification"] = map[string]any{"status": tc.value}
			} else if tc.field != "legacy_status" {
				blocked[tc.field] = tc.value
			}
			blocked["support"] = []any{signedTemporalClaimTestEvidence(t, signer, blocked, "support", "excluded-run", temporalClaimTestEvidence("excluded"))}
			stored := writeTemporalClaimTestHTTP(t, s, blocked, false)
			if len(stored.Support) != 1 || temporalClaimEvidenceAuthority(stored.Support[0]) != temporalClaimIndependentBinding {
				t.Fatalf("test did not admit a valid receipt on the excluded claim: %#v", stored.Support)
			}
			if tc.field == "legacy_status" {
				stored.Status = tc.value
				s.temporalClaims.mu.Lock()
				s.temporalClaims.setClaimLocked(stored)
				s.temporalClaims.mu.Unlock()
			}
			current := temporalClaimTestPayload("current", "ready")
			current["contradicts"] = []any{"contender", "excluded"}
			current["support"] = []any{signedTemporalClaimTestEvidence(t, signer, current, "support", "current-run", temporalClaimTestEvidence("current"))}
			writeTemporalClaimTestHTTP(t, s, current, false)
			writeTemporalClaimTestHTTP(t, s, temporalClaimTestPayload("contender", "not ready"), false)

			for _, payload := range []map[string]any{
				{"project": "test-project", "claim_ids": []any{"current", "contender", "excluded"}},
				{"project": "test-project", "query": "service"},
			} {
				response, err := s.buildContradictionResolution(payload)
				if err != nil {
					t.Fatal(err)
				}
				rec := anyMap(response["recommendation"])
				row := temporalClaimTestRecommendationRow(rec, "excluded")
				losers := anyToStringList(rec["losing_claim_ids"], 3)
				if rec["winning_claim_id"] != "current" || len(losers) != 1 || losers[0] != "contender" ||
					row["current_eligible"] != false || row["current_exclusion_reason"] != tc.exclusion ||
					row["bound_support_count"] != 0 || row["evidence_weight"] != 0.0 {
					t.Fatalf("non-current claim influenced advice: %#v", rec)
				}
			}
			if tc.name == "retracted" {
				response, err := s.buildContradictionResolution(map[string]any{
					"operation": "decide", "project": "test-project", "claim_ids": []any{"current", "excluded"},
					"approved": true, "operator": "test-operator", "reason": "explicit manual review", "winning_claim_id": "excluded",
				})
				if err != nil || anyMap(response["recommendation"])["status"] != "abstained" ||
					anyMap(response["resolution"])["winning_claim_id"] != "excluded" {
					t.Fatalf("explicit adjudication was coupled to the live gate: response=%#v err=%v", response, err)
				}
			}
		})
	}
}

func TestFrontierT5ContradictionIssuerRevocationPreservesHistoricalAdmission(t *testing.T) {
	s := newTemporalClaimEvidenceTestServer(t)
	signer := newTemporalClaimTestSigner(t, "experiment", "verifier")
	configureTemporalClaimTestSigners(t, signer)
	left := temporalClaimTestPayload("a", "ready")
	left["contradicts"] = []any{"b"}
	left["support"] = []any{signedTemporalClaimTestEvidence(t, signer, left, "support", "a-run", temporalClaimTestEvidence("a"))}
	writeTemporalClaimTestHTTP(t, s, left, false)
	writeTemporalClaimTestHTTP(t, s, temporalClaimTestPayload("b", "blocked"), false)
	recommend := func(operation string) map[string]any {
		t.Helper()
		response, err := s.buildContradictionResolution(map[string]any{
			"operation": operation, "project": "test-project", "claim_ids": []any{"a", "b"},
		})
		if err != nil {
			t.Fatal(err)
		}
		return anyMap(response["recommendation"])
	}
	if active := recommend("recommend"); active["winning_claim_id"] != "a" || active["source_rechecked"] != false {
		t.Fatalf("expected advice from admitted evidence without a fresh read: %#v", active)
	}
	for _, change := range []struct {
		name    string
		signers []temporalClaimTestSigner
	}{
		{"removed key", nil},
		{"same key reassigned to another origin", []temporalClaimTestSigner{{issuer: temporalClaimEvidenceIssuer{
			Project: signer.issuer.Project, TopicPath: signer.issuer.TopicPath, PublicKey: signer.issuer.PublicKey,
			Role: "verifier", OriginID: "different-experiment",
		}}}},
	} {
		t.Run(change.name, func(t *testing.T) {
			configureTemporalClaimTestSigners(t, change.signers...)
			for _, operation := range []string{"recommend", "status"} {
				rec := recommend(operation)
				row := temporalClaimTestRecommendationRow(rec, "a")
				counts, _ := row["support_citation_authority_counts"].(map[string]int)
				if rec["status"] != "abstained" || row["bound_support_count"] != 0 ||
					counts[temporalClaimInactiveAttestation] != 1 || counts[temporalClaimIndependentBinding] != 0 ||
					rec["source_rechecked"] != false {
					t.Fatalf("%s still presented an inactive admission as current: %#v", operation, rec)
				}
			}
			archived := anyMap(temporalClaimMaps(s.temporalClaims.query(temporalClaimQuery{
				Project: "test-project", Predicate: "status", Query: "ready", Limit: 1,
			}))[0])
			admission := anyMap(anyMap(contextPackAnyList(archived["support"])[0])["admission"])
			if archived["evidence_admission_basis"] != "at_write_not_rechecked" ||
				admission["authority"] != temporalClaimIndependentBinding || admission["checked_at"] != temporalClaimTestCheck {
				t.Fatalf("revocation erased or refreshed historical evidence: %#v", archived)
			}
		})
	}
}
