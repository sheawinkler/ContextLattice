package main

import (
	"context"
	"encoding/json"
	"os"
	"time"
)

const (
	temporalClaimEvidenceIssuerEnv   = "CONTEXTLATTICE_TEMPORAL_CLAIM_EVIDENCE_ISSUERS"
	temporalClaimReceiptSchema       = "temporal_claim_evidence_receipt.v1"
	temporalClaimWriterAssertion     = "writer_assertion"
	temporalClaimCheckedSource       = "source_checked"
	temporalClaimOwnerStatement      = "owner_authoritative_statement"
	temporalClaimIndependentBinding  = "independently_bound"
	temporalClaimInactiveAttestation = "inactive_attestation"
)

type temporalClaimEvidenceAdmission struct {
	Authority         string `json:"authority"`
	SourceChecked     bool   `json:"source_checked,omitempty"`
	IssuerKeyID       string `json:"issuer_key_id,omitempty"`
	IssuerGrantDigest string `json:"issuer_grant_digest,omitempty"`
	ReceiptDigest     string `json:"receipt_digest,omitempty"`
	ObservationKey    string `json:"observation_key,omitempty"`
	CheckedAt         string `json:"checked_at,omitempty"`
}

type temporalClaimEvidenceReceipt struct {
	ObservationID string
	CheckedAt     string
	Signature     contextPassportSignature
}

func parseTemporalClaimEvidenceReceipt(raw map[string]any) temporalClaimEvidenceReceipt {
	signature := anyMap(raw["signature"])
	return temporalClaimEvidenceReceipt{
		ObservationID: anyToString(raw["observation_id"]),
		CheckedAt:     anyToString(raw["checked_at"]),
		Signature: contextPassportSignature{
			Algorithm: anyToString(signature["algorithm"]),
			KeyID:     anyToString(signature["key_id"]),
			Value:     anyToString(signature["value"]),
		},
	}
}

// The operator registry is a JSON array of exact project/topic grants. Keys
// are raw standard-base64 Ed25519 public keys. Verifier origins identify the
// upstream observation namespace; owner grants name an exact subject/predicate.
type temporalClaimEvidenceIssuer struct {
	Project   string `json:"project"`
	TopicPath string `json:"topic_path"`
	PublicKey string `json:"public_key"`
	Role      string `json:"role"`
	OriginID  string `json:"origin_id"`
	Subject   string `json:"subject"`
	Predicate string `json:"predicate"`
}

func configuredTemporalClaimEvidenceIssuers() []temporalClaimEvidenceIssuer {
	raw := os.Getenv(temporalClaimEvidenceIssuerEnv)
	if raw == "" || len(raw) > 64*1024 {
		return nil
	}
	var issuers []temporalClaimEvidenceIssuer
	if json.Unmarshal([]byte(raw), &issuers) != nil || len(issuers) > 64 {
		return nil
	}
	return issuers
}

func temporalClaimEvidenceIssuerFor(issuers []temporalClaimEvidenceIssuer, claim temporalClaim, keyID string) (temporalClaimEvidenceIssuer, bool) {
	var match temporalClaimEvidenceIssuer
	found := false
	for _, issuer := range issuers {
		if issuer.Project != claim.Project || issuer.TopicPath != claim.TopicPath ||
			keyID != "ed25519:"+digestPrefix(issuer.PublicKey, 24) {
			continue
		}
		if found {
			return temporalClaimEvidenceIssuer{}, false
		}
		match, found = issuer, true
	}
	return match, found
}

func temporalClaimIssuerGrantDigest(issuer temporalClaimEvidenceIssuer) string {
	raw, _ := json.Marshal(issuer)
	return "sha256:" + sha256Hex(string(raw))
}

func temporalClaimEvidenceDigest(value string) string {
	return "sha256:" + normalizeProjectedContentHash(value)
}

// A Utility outcome or candidate-result digest is not a claim attestation.
// Require a domain-separated signature over the exact statement, scope,
// polarity and cited revision. The registry, not the submitter, assigns role
// and source origin; keys for mirrors or rotations must share an origin and
// sign the original observation ID, not a mirror or verification event ID.
func temporalClaimEvidenceSigningPayload(claim temporalClaim, polarity string, evidence temporalClaimEvidence, receipt temporalClaimEvidenceReceipt, issuer temporalClaimEvidenceIssuer) map[string]any {
	return map[string]any{
		"schema_id":   temporalClaimReceiptSchema,
		"issuer_role": issuer.Role,
		"origin_id":   issuer.OriginID,
		"claim": map[string]any{
			"project": claim.Project, "topic_path": claim.TopicPath, "claim_id": claim.ClaimID,
			"subject": claim.Subject, "predicate": claim.Predicate, "object": claim.Object,
			"statement": claim.Statement, "observed_at": claim.ObservedAt,
			"valid_from": claim.ValidFrom, "valid_to": claim.ValidTo, "branch": claim.Branch, "commit": claim.Commit,
		},
		"polarity": polarity, "observation_id": receipt.ObservationID, "checked_at": receipt.CheckedAt,
		"evidence": map[string]any{
			"ref_id": evidence.RefID, "kind": evidence.Kind, "memory_id": evidence.MemoryID,
			"uri": evidence.URI, "content_hash": temporalClaimEvidenceDigest(evidence.ContentHash),
			"excerpt": evidence.Excerpt, "observed_at": evidence.ObservedAt,
		},
	}
}

func (s *server) admitTemporalClaimEvidence(ctx context.Context, claim *temporalClaim, now time.Time) {
	issuers := configuredTemporalClaimEvidenceIssuers()
	for _, side := range []struct {
		polarity string
		rows     []temporalClaimEvidence
	}{{"support", claim.Support}, {"opposition", claim.Opposition}} {
		for i := range side.rows {
			ref := &side.rows[i]
			receipt := ref.receipt
			ref.receipt = temporalClaimEvidenceReceipt{}
			digest := temporalClaimEvidenceDigest(ref.ContentHash)
			if !utilitySHA256DigestValid(digest) {
				continue
			}
			observedAt, err := time.Parse(time.RFC3339Nano, ref.ObservedAt)
			if ref.ObservedAt != "" && (err != nil || observedAt.After(now)) {
				continue
			}
			localSource := ref.Kind == "memory" || ref.MemoryID != ""
			sourceChecked := localSource && s.checkTemporalClaimMemorySource(ctx, *claim, *ref, digest)
			if sourceChecked {
				ref.Admission = &temporalClaimEvidenceAdmission{
					Authority: temporalClaimCheckedSource, SourceChecked: true, CheckedAt: now.Format(time.RFC3339Nano),
				}
			}
			if (localSource && !sourceChecked) || observedAt.IsZero() ||
				!temporalClaimIDPattern.MatchString(receipt.ObservationID) {
				continue
			}
			checkedAt, err := time.Parse(time.RFC3339Nano, receipt.CheckedAt)
			claimAt, _ := time.Parse(time.RFC3339Nano, claim.ObservedAt)
			if err != nil || claimAt.IsZero() || checkedAt.Before(observedAt) || checkedAt.Before(claimAt) || checkedAt.After(now) {
				continue
			}
			keyID := receipt.Signature.KeyID
			issuer, found := temporalClaimEvidenceIssuerFor(issuers, *claim, keyID)
			if !found {
				continue
			}
			authority, observationKey := "", ""
			switch issuer.Role {
			case "owner":
				if issuer.Subject != claim.Subject || issuer.Predicate != claim.Predicate {
					continue
				}
				authority = temporalClaimOwnerStatement
			case "verifier":
				if !temporalClaimIDPattern.MatchString(issuer.OriginID) {
					continue
				}
				authority = temporalClaimIndependentBinding
				scope, _ := json.Marshal([4]string{claim.Project, claim.TopicPath, issuer.OriginID, receipt.ObservationID})
				observationKey = "sha256:" + sha256Hex(string(scope))
			default:
				continue
			}
			signingIssuer := contextPassportIssuer{SigningKeyID: keyID, SigningPublicKey: issuer.PublicKey}
			if !verifySignedBytes(temporalClaimEvidenceSigningPayload(*claim, side.polarity, *ref, receipt, issuer), receipt.Signature, signingIssuer) {
				continue
			}
			ref.Admission = &temporalClaimEvidenceAdmission{
				Authority: authority, SourceChecked: sourceChecked, IssuerKeyID: keyID,
				IssuerGrantDigest: temporalClaimIssuerGrantDigest(issuer),
				ReceiptDigest:     "sha256:" + sha256Hex(receipt.Signature.Value), ObservationKey: observationKey,
				CheckedAt: checkedAt.UTC().Format(time.RFC3339Nano),
			}
		}
	}
}

func (s *server) checkTemporalClaimMemorySource(ctx context.Context, claim temporalClaim, ref temporalClaimEvidence, digest string) bool {
	if s.memoryStore == nil || ref.Kind != "memory" || ref.URI != "" {
		return false
	}
	project, file, err := splitEngineMemoryID(ref.MemoryID)
	if err != nil || project != claim.Project {
		return false
	}
	file, err = sanitizeMemoryFile(file)
	if err != nil {
		return false
	}
	entry, ok := s.memoryStore.currentEntry(project, file)
	if !ok || entry.Project != project || entry.TopicPath != claim.TopicPath ||
		normalizeMemoryLifecycle(entry.Lifecycle) == "retired" || temporalClaimEvidenceDigest(entry.ContentHash) != digest {
		return false
	}
	_, _, err = s.memoryStore.readVerifiedEvidencePreview(ctx, project, file, digest, 32*1024, 4096)
	if err != nil {
		return false
	}
	current, ok := s.memoryStore.currentEntry(project, file)
	return ok && current.EventID == entry.EventID
}

func temporalClaimEvidenceAuthority(ref temporalClaimEvidence) string {
	if ref.Admission != nil {
		return ref.Admission.Authority
	}
	return temporalClaimWriterAssertion
}

// This checks the current grant, not the old signature or source bytes.
// A retained admission without its exact original grant is only history.
func temporalClaimCurrentEvidenceAuthority(claim temporalClaim, ref temporalClaimEvidence, issuers []temporalClaimEvidenceIssuer, now time.Time) string {
	authority := temporalClaimEvidenceAuthority(ref)
	if authority != temporalClaimIndependentBinding && authority != temporalClaimOwnerStatement {
		return authority
	}
	admission := ref.Admission
	issuer, found := temporalClaimEvidenceIssuerFor(issuers, claim, admission.IssuerKeyID)
	checkedAt, err := time.Parse(time.RFC3339Nano, admission.CheckedAt)
	if !found || err != nil || checkedAt.After(now) ||
		admission.IssuerGrantDigest != temporalClaimIssuerGrantDigest(issuer) ||
		!utilitySHA256DigestValid(admission.ReceiptDigest) {
		return temporalClaimInactiveAttestation
	}
	if authority == temporalClaimIndependentBinding && issuer.Role != "verifier" ||
		authority == temporalClaimOwnerStatement && issuer.Role != "owner" {
		return temporalClaimInactiveAttestation
	}
	return authority
}

func temporalClaimBoundObservations(claim temporalClaim, refs []temporalClaimEvidence, issuers []temporalClaimEvidenceIssuer, now time.Time) map[string]struct{} {
	keys := make(map[string]struct{})
	for _, ref := range refs {
		if temporalClaimCurrentEvidenceAuthority(claim, ref, issuers, now) == temporalClaimIndependentBinding &&
			utilitySHA256DigestValid(ref.Admission.ObservationKey) {
			keys[ref.Admission.ObservationKey] = struct{}{}
		}
	}
	return keys
}

func temporalClaimEvidenceAuthorityCounts(project string, claim temporalClaim, refs []temporalClaimEvidence, issuers []temporalClaimEvidenceIssuer, now time.Time) map[string]int {
	counts := make(map[string]int)
	for _, ref := range refs {
		authority := temporalClaimWriterAssertion
		if claim.Project == project {
			authority = temporalClaimCurrentEvidenceAuthority(claim, ref, issuers, now)
		}
		counts[authority]++
	}
	return counts
}
