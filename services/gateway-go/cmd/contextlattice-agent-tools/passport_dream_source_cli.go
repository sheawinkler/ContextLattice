package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

const cliDreamSourcePassportSchema = "context_passport.v1"
const cliDreamSourceResponseMaxBytes = 64 << 10

// Field order follows the signed v1 passport payload. This mode only exports
// roots with no parent and no optional portable-evidence identity.
type cliDreamSourcePassportPayload struct {
	SchemaID  string `json:"schema_id"`
	Version   int    `json:"version"`
	LineageID string `json:"lineage_id"`
	Project   string `json:"project"`
	Revision  int    `json:"revision"`
	CreatedAt string `json:"created_at"`
	ExpiresAt string `json:"expires_at"`
	Issuer    struct {
		InstanceID       string `json:"instance_id"`
		SigningKeyID     string `json:"signing_key_id"`
		SigningPublicKey string `json:"signing_public_key"`
	} `json:"issuer"`
	Scope        map[string]any   `json:"scope"`
	Objective    map[string]any   `json:"objective"`
	Claims       []map[string]any `json:"claims"`
	Evidence     []map[string]any `json:"evidence"`
	Lineage      map[string]any   `json:"lineage"`
	Capabilities []string         `json:"capabilities"`
	Redactions   map[string]any   `json:"redactions"`
	Replay       map[string]any   `json:"replay"`
}

type cliDreamSourcePassport struct {
	cliDreamSourcePassportPayload
	PassportID    string `json:"passport_id"`
	ContentDigest string `json:"content_digest"`
	Signature     struct {
		Algorithm string `json:"algorithm"`
		KeyID     string `json:"key_id"`
		Value     string `json:"value"`
	} `json:"signature"`
}

func (p cliDreamSourcePassport) validSignature() bool {
	raw, err := json.Marshal(p.cliDreamSourcePassportPayload)
	if err != nil {
		return false
	}
	hash := sha256.Sum256(raw)
	if p.ContentDigest != fmt.Sprintf("sha256:%x", hash) || p.PassportID != fmt.Sprintf("passport_%x", hash[:12]) {
		return false
	}
	issuer := p.Issuer
	keyID := sha256.Sum256([]byte(issuer.SigningPublicKey))
	if p.Signature.Algorithm != "Ed25519" || p.Signature.KeyID != issuer.SigningKeyID ||
		issuer.SigningKeyID != fmt.Sprintf("ed25519:%x", keyID[:12]) {
		return false
	}
	key, keyErr := base64.RawStdEncoding.DecodeString(issuer.SigningPublicKey)
	sig, sigErr := base64.RawStdEncoding.DecodeString(p.Signature.Value)
	signed, err := json.Marshal(struct {
		PassportID    string          `json:"passport_id"`
		ContentDigest string          `json:"content_digest"`
		Payload       json.RawMessage `json:"payload"`
	}{p.PassportID, p.ContentDigest, raw})
	return err == nil && keyErr == nil && sigErr == nil && len(key) == ed25519.PublicKeySize &&
		len(sig) == ed25519.SignatureSize && ed25519.Verify(key, signed, sig)
}

func validCLIDreamSourceReceipt(result map[string]json.RawMessage, project string, observation map[string]any) bool {
	if string(result["ok"]) != "true" || string(result["recorded"]) != "true" ||
		cliDreamTransferString(result, "schema_id") != cliDreamSourcePassportSchema {
		return false
	}
	var p cliDreamSourcePassport
	if decisionresearch.Decode(result["passport"], &p, true) != nil ||
		p.SchemaID != cliDreamSourcePassportSchema || p.Version != 1 || p.Revision != 1 ||
		p.Project != project || p.Scope["project"] != project || p.LineageID == "" || p.Issuer.InstanceID == "" ||
		p.Lineage["source_schema"] != "dream_source_observation.v1" || len(p.Replay) != 0 || len(p.Objective) != 0 ||
		len(p.Claims) != 1 || len(p.Evidence) != 1 || !p.validSignature() {
		return false
	}
	var reconciliation struct {
		Recorded   bool   `json:"recorded"`
		Conflict   bool   `json:"conflict"`
		PassportID string `json:"passport_id"`
		LineageID  string `json:"lineage_id"`
		Revision   int    `json:"revision"`
	}
	if json.Unmarshal(result["reconciliation"], &reconciliation) != nil ||
		!reconciliation.Recorded || reconciliation.Conflict || reconciliation.PassportID != p.PassportID ||
		reconciliation.LineageID != p.LineageID || reconciliation.Revision != p.Revision {
		return false
	}
	var source map[string]json.RawMessage
	if json.Unmarshal(result["transfer_source"], &source) != nil || len(source) != 3 ||
		cliDreamTransferString(source, "source_passport_id") != p.PassportID {
		return false
	}
	proof, claim := p.Evidence[0], p.Claims[0]
	if proof["kind"] != "source_memory_revision" || proof["memory_id"] != observation["memory_id"] ||
		proof["event_id"] != observation["event_id"] || proof["content_hash"] != observation["content_hash"] {
		return false
	}
	experience := cliDreamTransferString(source, "experience_ref")
	id, _ := proof["evidence_id"].(string)
	refs, _ := claim["evidence_refs"].([]any)
	mechanism, _ := claim["mechanism"].(string)
	outcome, _ := claim["observed_result"].(string)
	if experience == "" || claim["experience_id"] != experience || claim["kind"] != "conditional_experience" ||
		claim["status"] != "confirmed" || strings.TrimSpace(mechanism) == "" || strings.TrimSpace(outcome) == "" ||
		id == "" || len(refs) != 1 || refs[0] != id {
		return false
	}
	raw, err := json.Marshal(claim)
	return err == nil && cliDreamTransferString(source, "source_record_hash") == fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
}

func (c *cli) requestDreamSourcePassport(payload map[string]any, timeout float64) (map[string]any, error) {
	bad := errors.New("Dream source passport export was not confirmed")
	project, _ := payload["project"].(string)
	observation, _ := payload["transfer_observation"].(map[string]any)
	memoryID, _ := observation["memory_id"].(string)
	eventID, _ := observation["event_id"].(string)
	contentHash, _ := observation["content_hash"].(string)
	if strings.TrimSpace(c.apiKey) == "" || !finitePositiveFloat(timeout) || timeout > 3600 ||
		len(payload) != 2 || len(observation) != 3 || len(project) > 120 || !cliDreamTransferProject(project) ||
		len(memoryID) <= len(project)+2 || len(memoryID) > 1024 || !strings.HasPrefix(memoryID, project+"::") ||
		strings.TrimSpace(eventID) == "" || len(eventID) > 160 || !cliDreamTransferDigest(contentHash, "") {
		return nil, bad
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	raw, err := c.conflictRequest(ctx, http.MethodPost, "/memory/context-passport/export", payload, cliDreamSourceResponseMaxBytes)
	var receipt map[string]json.RawMessage
	if err != nil || !utf8.Valid(raw) || !jsonEscapedSurrogatesValid(raw) ||
		decisionresearch.Decode(raw, &receipt, false) != nil || !validCLIDreamSourceReceipt(receipt, project, observation) {
		return nil, bad
	}
	result := make(map[string]any, len(receipt))
	for key, value := range receipt {
		result[key] = value
	}
	return result, nil
}
