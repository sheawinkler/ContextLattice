package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
)

const (
	contextPackLearnedComparatorAuthoritySchemaID = "context_pack_learned_comparator_authority.v1"
	contextPackLearnedComparatorProducerID        = "gateway-go"
	contextPackLearnedComparatorProfileID         = "saved_recall_canonical_no_user_preferences.v1"
	contextPackLearnedComparatorAuthorityFlag     = "activation_authority"
)

// contextPackLearnedComparatorAuthority keeps ordinary saved evaluations
// diagnostic. Only an explicitly authenticated evaluation can create an
// activation-authorizing artifact, and its retrieval profile is entirely
// server-owned: no caller headers, user id, or preference state cross into it.
type contextPackLearnedComparatorAuthority struct {
	Authorized bool
	Reason     string
	Workspace  string
	Envelope   map[string]any
	Headers    http.Header
}

func contextPackLearnedComparatorAuthorityKey(s *server) string {
	if key := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_LEARNED_COMPARATOR_AUTHORITY_KEY")); key != "" {
		return key
	}
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.orchestratorAPIKey)
}

func contextPackLearnedComparatorIngressKey(s *server) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(s.orchestratorAPIKey)
}

func contextPackLearnedComparatorAuthorityKeyRef(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	return "sha256:" + sha256Hex("context-pack-learned-comparator-authority-key.v1\x00"+key)
}

func contextPackLearnedComparatorAuthoritySignature(key, profileDigest string) string {
	key = strings.TrimSpace(key)
	profileDigest = strings.TrimSpace(profileDigest)
	if key == "" || !isSearchIntelligenceFullSHA256Ref(profileDigest) {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte("context-pack-learned-comparator-authority.v1\x00" + profileDigest))
	return "sha256:" + hex.EncodeToString(mac.Sum(nil))
}

func contextPackLearnedComparatorAuthorityUnsigned(workspaceRef, keyRef string) map[string]any {
	return map[string]any{
		"schema_id":           contextPackLearnedComparatorAuthoritySchemaID,
		"version":             1,
		"producer_id":         contextPackLearnedComparatorProducerID,
		"profile_id":          contextPackLearnedComparatorProfileID,
		"workspace_ref":       workspaceRef,
		"caller_personalized": false,
		"user_scope":          "none",
		"preferences":         "disabled",
		"headers":             "server_owned",
		"signing_key_ref":     keyRef,
	}
}

func contextPackLearnedComparatorAuthorityEnvelope(key, workspaceRef string) map[string]any {
	keyRef := contextPackLearnedComparatorAuthorityKeyRef(key)
	if contextPackLearnedDigestRef(workspaceRef) == "" || keyRef == "" {
		return nil
	}
	envelope := contextPackLearnedComparatorAuthorityUnsigned(workspaceRef, keyRef)
	profileDigest := contextPackLearnedCanonicalDigest(envelope)
	signature := contextPackLearnedComparatorAuthoritySignature(key, profileDigest)
	if profileDigest == "" || signature == "" {
		return nil
	}
	envelope["profile_digest"] = profileDigest
	envelope["authority_signature"] = signature
	return envelope
}

func contextPackLearnedComparatorAuthorityEnvelopeValid(s *server, raw map[string]any, workspaceRef string) bool {
	if contextPackLearnedDigestRef(workspaceRef) == "" || len(raw) == 0 {
		return false
	}
	allowed := map[string]struct{}{}
	for _, key := range []string{
		"schema_id", "version", "producer_id", "profile_id", "workspace_ref", "caller_personalized",
		"user_scope", "preferences", "headers", "signing_key_ref", "profile_digest", "authority_signature",
	} {
		allowed[key] = struct{}{}
	}
	for key := range raw {
		if _, ok := allowed[key]; !ok {
			return false
		}
	}
	if anyToString(raw["schema_id"]) != contextPackLearnedComparatorAuthoritySchemaID ||
		anyToInt(raw["version"], 0) != 1 ||
		anyToString(raw["producer_id"]) != contextPackLearnedComparatorProducerID ||
		anyToString(raw["profile_id"]) != contextPackLearnedComparatorProfileID ||
		anyToString(raw["workspace_ref"]) != workspaceRef ||
		anyToBool(raw["caller_personalized"]) ||
		anyToString(raw["user_scope"]) != "none" ||
		anyToString(raw["preferences"]) != "disabled" ||
		anyToString(raw["headers"]) != "server_owned" {
		return false
	}
	key := contextPackLearnedComparatorAuthorityKey(s)
	keyRef := contextPackLearnedComparatorAuthorityKeyRef(key)
	if keyRef == "" || anyToString(raw["signing_key_ref"]) != keyRef {
		return false
	}
	unsigned := contextPackLearnedComparatorAuthorityUnsigned(workspaceRef, keyRef)
	profileDigest := contextPackLearnedCanonicalDigest(unsigned)
	if profileDigest == "" || anyToString(raw["profile_digest"]) != profileDigest {
		return false
	}
	wantSignature := contextPackLearnedComparatorAuthoritySignature(key, profileDigest)
	gotSignature := strings.TrimSpace(anyToString(raw["authority_signature"]))
	if wantSignature == "" || !isSearchIntelligenceFullSHA256Ref(gotSignature) {
		return false
	}
	return hmac.Equal([]byte(wantSignature), []byte(gotSignature))
}

func contextPackLearnedCanonicalComparatorHeaders(s *server) http.Header {
	headers := make(http.Header)
	if s != nil {
		if key := strings.TrimSpace(s.orchestratorAPIKey); key != "" {
			headers.Set("X-Api-Key", key)
		}
	}
	return headers
}

func contextPackLearnedComparatorAuthorityRequested(payload map[string]any) bool {
	return anyToBool(payload[contextPackLearnedComparatorAuthorityFlag])
}

func contextPackLearnedComparatorAuthorityPayloadCanonical(payload map[string]any) bool {
	// Activation-authorizing comparisons accept only the authority request and
	// the canonical evaluate mode. Every other option remains available for
	// diagnostics, but makes the result ineligible to authorize activation.
	for key := range payload {
		if key != contextPackLearnedComparatorAuthorityFlag && key != "mode" {
			return false
		}
	}
	if mode := strings.TrimSpace(strings.ToLower(anyToString(payload["mode"]))); mode != "" && mode != "evaluate" {
		return false
	}
	return true
}

func contextPackLearnedComparatorAuthorityForRequest(s *server, r *http.Request, payload map[string]any) contextPackLearnedComparatorAuthority {
	result := contextPackLearnedComparatorAuthority{Reason: "diagnostic_evaluation"}
	if !contextPackLearnedComparatorAuthorityRequested(payload) {
		return result
	}
	if !contextPackLearnedComparatorAuthorityPayloadCanonical(payload) {
		result.Reason = "caller_personalized_evaluation"
		return result
	}
	signingKey := contextPackLearnedComparatorAuthorityKey(s)
	ingressKey := contextPackLearnedComparatorIngressKey(s)
	provided, explicit := requestAPIKey(r)
	if signingKey == "" {
		result.Reason = "authority_key_unavailable"
		return result
	}
	// Ingress authentication and artifact signing are deliberately separate
	// authorities. Rotating the dedicated signing key must not invalidate the
	// ordinary gateway credential, and possessing the signing secret must not
	// grant HTTP access.
	if ingressKey == "" || !explicit || !hmac.Equal([]byte(provided), []byte(ingressKey)) {
		result.Reason = "authority_authentication_required"
		return result
	}
	workspaceAuthority := optionalContextPackLearnedRequestAuthority(s, r)
	if !workspaceAuthority.Authorized {
		result.Reason = firstNonEmptyStrings(workspaceAuthority.Reason, "activation_authority_unavailable")
		return result
	}
	workspaceRef := contextPackLearnedScopeRef("workspace", workspaceAuthority.WorkspaceID)
	envelope := contextPackLearnedComparatorAuthorityEnvelope(signingKey, workspaceRef)
	if len(envelope) == 0 {
		result.Reason = "authority_envelope_unavailable"
		return result
	}
	result.Authorized = true
	result.Reason = "canonical_server_owned_profile"
	result.Workspace = workspaceRef
	result.Envelope = envelope
	result.Headers = contextPackLearnedCanonicalComparatorHeaders(s)
	return result
}
