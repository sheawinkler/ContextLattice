package main

import (
	"encoding/json"
	"errors"
	"strings"
)

const portableEvidenceIdentitySchemaID = "portable_evidence_identity.v1"

// portableEvidenceIdentity is deliberately evidence-only. It carries the
// bounded identity of a response proof across signed portable boundaries, but
// it is never an authority, an activation input, or an outcome-credit source.
// Keep this type pointer-valued at its call sites so omitempty preserves the
// exact legacy signed JSON when no identity is present.
type portableEvidenceIdentity struct {
	SchemaID                  string `json:"schema_id"`
	Version                   int    `json:"version"`
	ResponseDigest            string `json:"response_digest"`
	CognitionDigest           string `json:"cognition_digest"`
	TemporalPremiseDigest     string `json:"temporal_premise_digest"`
	OrderedComponentSetDigest string `json:"ordered_component_set_digest"`
	ScopeDigest               string `json:"scope_digest"`
	GapDigest                 string `json:"gap_digest"`
	ActionBoundaryDigest      string `json:"action_boundary_digest"`
	Basis                     string `json:"basis"`
	CanAuthorize              bool   `json:"can_authorize"`
	IdentityDigest            string `json:"identity_digest"`
}

type portableEvidenceIdentityUnsigned struct {
	SchemaID                  string `json:"schema_id"`
	Version                   int    `json:"version"`
	ResponseDigest            string `json:"response_digest"`
	CognitionDigest           string `json:"cognition_digest"`
	TemporalPremiseDigest     string `json:"temporal_premise_digest"`
	OrderedComponentSetDigest string `json:"ordered_component_set_digest"`
	ScopeDigest               string `json:"scope_digest"`
	GapDigest                 string `json:"gap_digest"`
	ActionBoundaryDigest      string `json:"action_boundary_digest"`
	Basis                     string `json:"basis"`
	CanAuthorize              bool   `json:"can_authorize"`
}

func portableEvidenceIdentityUnsignedValue(identity portableEvidenceIdentity) portableEvidenceIdentityUnsigned {
	return portableEvidenceIdentityUnsigned{
		SchemaID: identity.SchemaID, Version: identity.Version,
		ResponseDigest: identity.ResponseDigest, CognitionDigest: identity.CognitionDigest,
		TemporalPremiseDigest:     identity.TemporalPremiseDigest,
		OrderedComponentSetDigest: identity.OrderedComponentSetDigest,
		ScopeDigest:               identity.ScopeDigest,
		GapDigest:                 identity.GapDigest,
		ActionBoundaryDigest:      identity.ActionBoundaryDigest,
		Basis:                     identity.Basis, CanAuthorize: identity.CanAuthorize,
	}
}

func portableEvidenceIdentityDigest(identity portableEvidenceIdentity) string {
	return frontierT7Digest(portableEvidenceIdentityUnsignedValue(identity))
}

func validatePortableEvidenceIdentity(identity *portableEvidenceIdentity) error {
	if identity == nil {
		return nil
	}
	if identity.SchemaID != portableEvidenceIdentitySchemaID || identity.Version != 1 {
		return errors.New("portable evidence identity schema is unsupported")
	}
	for field, value := range map[string]string{
		"response_digest":              identity.ResponseDigest,
		"cognition_digest":             identity.CognitionDigest,
		"temporal_premise_digest":      identity.TemporalPremiseDigest,
		"ordered_component_set_digest": identity.OrderedComponentSetDigest,
		"scope_digest":                 identity.ScopeDigest,
		"gap_digest":                   identity.GapDigest,
		"action_boundary_digest":       identity.ActionBoundaryDigest,
	} {
		if !frontierT7ValidDigest(value) {
			return errors.New("portable evidence identity " + field + " must be a SHA-256 digest")
		}
	}
	if identity.Basis != "evidence_only" || identity.CanAuthorize {
		return errors.New("portable evidence identity cannot authorize")
	}
	if identity.IdentityDigest != portableEvidenceIdentityDigest(*identity) {
		return errors.New("portable evidence identity digest mismatch")
	}
	return nil
}

func decodePortableEvidenceIdentity(value any) (*portableEvidenceIdentity, error) {
	if value == nil {
		return nil, errors.New("portable evidence identity must be an object")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, errors.New("portable evidence identity must be an object")
	}
	for _, field := range []string{
		"schema_id", "version", "response_digest", "cognition_digest", "temporal_premise_digest",
		"ordered_component_set_digest", "scope_digest", "gap_digest", "action_boundary_digest",
		"basis", "can_authorize", "identity_digest",
	} {
		value, present := fields[field]
		if !present || strings.TrimSpace(string(value)) == "null" {
			return nil, errors.New("portable evidence identity is partial")
		}
	}
	var identity portableEvidenceIdentity
	if err := strictJSONDecode(raw, &identity); err != nil {
		return nil, err
	}
	if err := validatePortableEvidenceIdentity(&identity); err != nil {
		return nil, err
	}
	return &identity, nil
}

func portableEvidenceIdentityContractFindings(object map[string]any) []map[string]any {
	findings := []map[string]any{}
	if object == nil {
		return []map[string]any{{"reason": "payload_not_object", "contract_id": portableEvidenceIdentitySchemaID}}
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return []map[string]any{{"reason": "identity_not_json", "contract_id": portableEvidenceIdentitySchemaID}}
	}
	var identity portableEvidenceIdentity
	if err := strictJSONDecode(raw, &identity); err != nil {
		return []map[string]any{{"reason": "identity_not_closed", "detail": err.Error(), "contract_id": portableEvidenceIdentitySchemaID}}
	}
	if err := validatePortableEvidenceIdentity(&identity); err != nil {
		findings = append(findings, map[string]any{"reason": "identity_invalid", "detail": err.Error(), "contract_id": portableEvidenceIdentitySchemaID})
	}
	return findings
}
