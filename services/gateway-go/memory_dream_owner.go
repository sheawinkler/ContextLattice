package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

const dreamOwnerFileEnv = "CONTEXTLATTICE_DREAM_TRANSFER_AUTHORITY_FILE"
const dreamOwnerSchema = "dream_transfer_local_authority.v1"
const dreamOwnerTTL = 15 * time.Minute

// The local edition has one custodian. Builds with workspace governance replace
// this check; an operator file is not a workspace credential.
var optionalDreamTransferLocalProjects = func(s *server, _, _ string) bool { return s != nil }

func dreamOwnerGrantID(id string) bool {
	tail, ok := strings.CutPrefix(id, "dgrant_")
	if !ok || len(tail) != 32 || tail != strings.ToLower(tail) {
		return false
	}
	_, err := hex.DecodeString(tail)
	return err == nil
}

// Every field is a string. Checking keys before decoding also rejects duplicates
// and case aliases rather than interpreting an ambiguous authorization.
func dreamOwnerFields(raw []byte) (map[string]string, bool) {
	const keys = "|schema_id|authority|grant_id|source_project|destination_project|source_passport_id|experience_ref|source_record_hash|caller_principal|issued_at|expires_at|"
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, false
	}
	out := make(map[string]string, 11)
	for d.More() {
		t, err = d.Token()
		k, ok := t.(string)
		if err != nil || !ok || k == "" || strings.Contains(k, "|") || !strings.Contains(keys, "|"+k+"|") {
			return nil, false
		}
		if _, duplicate := out[k]; duplicate {
			return nil, false
		}
		var value string
		if d.Decode(&value) != nil || value == "" || len(value) > 512 || strings.TrimSpace(value) != value ||
			strings.ContainsAny(value, "\r\n\t\x00") {
			return nil, false
		}
		out[k] = value
	}
	t, err = d.Token()
	if err != nil || t != json.Delim('}') || len(out) != 11 {
		return nil, false
	}
	_, err = d.Token()
	return out, err == io.EOF
}

func (s *server) dreamOwnerSnapshot(id string, now time.Time) (dreamTransferReceipt, bool) {
	var none dreamTransferReceipt
	if s == nil || !dreamOwnerGrantID(id) {
		return none, false
	}
	raw, stamp, ok := readDreamOwnerFile(os.Getenv(dreamOwnerFileEnv))
	if !ok {
		return none, false
	}
	f, ok := dreamOwnerFields(raw)
	if !ok || f["schema_id"] != dreamOwnerSchema || f["authority"] != "single_custodian_source_and_destination" || f["grant_id"] != id {
		return none, false
	}
	issued, e1 := time.Parse(time.RFC3339Nano, f["issued_at"])
	expires, e2 := time.Parse(time.RFC3339Nano, f["expires_at"])
	if e1 != nil || e2 != nil || issued.After(now) || !now.Before(expires) || expires.Sub(issued) > dreamOwnerTTL {
		return none, false
	}
	source, e1 := sanitizeMemoryProject(f["source_project"])
	dest, e2 := sanitizeMemoryProject(f["destination_project"])
	worker, proof, cut := strings.Cut(f["caller_principal"], "/cap-sha256:")
	worker, prefix := strings.CutPrefix(worker, "api-key-sha256:")
	hash, tagged := strings.CutPrefix(f["source_record_hash"], "sha256:")
	if e1 != nil || e2 != nil || source != f["source_project"] || dest != f["destination_project"] || strings.EqualFold(source, dest) ||
		!cut || !prefix || !isHexDigest(worker) || !isHexDigest(proof) || !tagged || !isHexDigest(hash) || len(f["experience_ref"]) > 160 ||
		!optionalDreamTransferLocalProjects(s, source, dest) {
		return none, false
	}
	return dreamTransferReceipt{
		ReceiptID: "doa_" + sha256Hex(string(raw)+"\x00"+stamp.Identity+"\x00"+stamp.ChangeToken),
		Kind:      "grant", GrantID: id, SourceProject: source, DestinationProject: dest,
		SourcePassportID: f["source_passport_id"], ExperienceRef: f["experience_ref"], SourceRecordHash: f["source_record_hash"],
		CallerPrincipal: f["caller_principal"], ExpiresAt: expires.UTC().Format(time.RFC3339Nano),
	}, true
}

func (s *server) dreamTransferOwnerPermits(grant dreamTransferReceipt, now time.Time) bool {
	file, ok := s.dreamOwnerSnapshot(grant.GrantID, now)
	return ok && file.ReceiptID == grant.ReceiptID && dreamTransferGrantMatches(file, grant)
}

func (s *server) loadDreamTransferOwnerGrant(ctx context.Context, id string) bool {
	caller, authenticated := dreamTransferIdentityFromContext(ctx)
	if !authenticated || ctx.Err() != nil || s == nil || s.contextMesh == nil || s.contextPassports == nil {
		return false
	}
	now := time.Now().UTC()
	file, ok := s.dreamOwnerSnapshot(id, now)
	if !ok || file.CallerPrincipal != caller.principal {
		return false
	}
	same := func(g dreamTransferReceipt, found, revoked bool) bool {
		return found && !revoked && g.ReceiptID == file.ReceiptID && dreamTransferGrantMatches(g, file)
	}
	if grant, found, revoked := s.contextMesh.dreamTransferGrant(id); found {
		return same(grant, found, revoked)
	}
	passport, found := s.contextPassports.get(file.SourcePassportID)
	if !found || passport.Project != file.SourceProject || len(verifyContextPassport(passport, now, true)) != 0 ||
		!dreamTransferHasSuccessfulImportReceipt(s.contextMesh, passport, s.contextPassports.identity) {
		return false
	}
	_, hash, found := dreamTransferConfirmedSourceClaim(passport, file.ExperienceRef)
	if !found || hash != file.SourceRecordHash {
		return false
	}
	// The ledger decides the first grant ID under its cross-process lock. A
	// simultaneous identical grant may have won; only that exact receipt counts.
	_ = s.contextMesh.appendDreamTransferReceipt(ctx, file, false)
	return same(s.contextMesh.dreamTransferGrant(id))
}
