package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var setupDreamOwnerTestWorkspace = func(*testing.T, *server) {}

func dreamTransferWriteOwnerTestGrant(t *testing.T, s *server, grant dreamTransferReceipt, issued time.Time) string {
	t.Helper()
	setupDreamOwnerTestWorkspace(t, s)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "authorization.json")
	raw, err := json.Marshal(map[string]string{
		"schema_id": dreamOwnerSchema, "authority": "single_custodian_source_and_destination",
		"grant_id": grant.GrantID, "source_project": grant.SourceProject, "destination_project": grant.DestinationProject,
		"source_passport_id": grant.SourcePassportID, "experience_ref": grant.ExperienceRef, "source_record_hash": grant.SourceRecordHash,
		"caller_principal": grant.CallerPrincipal, "issued_at": issued.UTC().Format(time.RFC3339Nano), "expires_at": grant.ExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(dreamOwnerFileEnv, path)
	return path
}

func TestDreamOwnerAuthorityLifetimeAndCustody(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		if _, _, ok := readDreamOwnerFile("unused"); ok {
			t.Fatal("unsupported host accepted local authority")
		}
		return
	}
	s := &server{}
	now := time.Now().UTC()
	id := "dgrant_" + strings.Repeat("a", 32)
	g := dreamTransferReceipt{GrantID: id, SourceProject: "alpha", DestinationProject: "beta", SourcePassportID: "pass-one",
		ExperienceRef: "experiment-one", SourceRecordHash: "sha256:" + strings.Repeat("b", 64),
		CallerPrincipal: "api-key-sha256:" + strings.Repeat("c", 64) + "/cap-sha256:" + strings.Repeat("d", 64),
		ExpiresAt:       now.Add(8 * time.Minute).Format(time.RFC3339Nano)}
	path := dreamTransferWriteOwnerTestGrant(t, s, g, now)
	grant, ok := s.dreamOwnerSnapshot(id, now)
	if !ok || !s.dreamTransferOwnerPermits(grant, now) {
		t.Fatal("exact grant from an owner-only file was refused")
	}
	if s.dreamTransferOwnerPermits(grant, now.Add(8*time.Minute)) || s.dreamTransferOwnerPermits(grant, now.Add(-time.Second)) {
		t.Fatal("grant accepted outside its authorized time window")
	}
	changed := grant
	changed.DestinationProject = "gamma"
	if s.dreamTransferOwnerPermits(changed, now) {
		t.Fatal("grant crossed the destination boundary")
	}
	changed = grant
	changed.CallerPrincipal = strings.ReplaceAll(grant.CallerPrincipal, strings.Repeat("d", 64), strings.Repeat("e", 64))
	if s.dreamTransferOwnerPermits(changed, now) {
		t.Fatal("grant accepted another holder of the shared worker key")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The old file still exists, guaranteeing a different inode for its replacement.
	if err := os.Rename(path, path+".retired"); err != nil {
		t.Fatal(err)
	}
	if s.dreamTransferOwnerPermits(grant, now) {
		t.Fatal("grant survived withdrawal")
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if s.dreamTransferOwnerPermits(grant, now) {
		t.Fatal("byte-identical replacement revived withdrawn authority")
	}
	if err := os.Link(path, path+".link"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readDreamOwnerFile(path); ok {
		t.Fatal("hardlinked authority accepted")
	}
	if err := os.Remove(path + ".link"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readDreamOwnerFile(path); ok {
		t.Fatal("group-readable authority accepted")
	}
	if err := os.Symlink(path+".retired", path+".symlink"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readDreamOwnerFile(path + ".symlink"); ok {
		t.Fatal("symlink authority accepted")
	}
	for _, bad := range []string{
		strings.Replace(string(raw), "{", "{\"grant_id\":\"other\",", 1),
		strings.Replace(string(raw), "\"source_project\"", "\"Source_Project\"", 1),
		string(raw) + "{}",
	} {
		if _, ok := dreamOwnerFields([]byte(bad)); ok {
			t.Fatal("ambiguous or extra authorization data accepted")
		}
	}
}
