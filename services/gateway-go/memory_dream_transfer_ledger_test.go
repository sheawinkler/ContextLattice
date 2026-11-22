package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDreamTransferLedgerIsIsolatedFromOrdinaryMesh(t *testing.T) {
	s, passport := newDreamTransferTestServer(t)
	mesh := s.contextMesh
	if _, err := os.Lstat(mesh.dreamLedgerPath()); !os.IsNotExist(err) {
		t.Fatal("ordinary Mesh operation touched optional transfer state")
	}
	original, err := os.ReadFile(mesh.path)
	if err != nil {
		t.Fatal(err)
	}
	g := dreamTransferReceipt{ReceiptID: "grant-receipt", Kind: "grant", GrantID: "grant-identity", SourceProject: passport.Project,
		SourcePassportID: passport.PassportID, ExperienceRef: "observation", SourceRecordHash: "sha256:" + strings.Repeat("a", 64),
		DestinationProject: "destination", CallerPrincipal: "scoped-worker", ExpiresAt: time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano)}
	if err := mesh.appendDreamTransferReceipt(context.Background(), g, false); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(mesh.path)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("manual transfer changed ordinary Mesh state bytes")
	}
	cold := newTestMeshStore(t, filepath.Dir(mesh.path), s.contextPassports)
	if _, found, revoked := cold.dreamTransferGrant(g.GrantID); !found || revoked {
		t.Fatal("fresh local owner did not recover signed authority")
	}
	if err := os.WriteFile(mesh.dreamLedgerPath(), []byte("invalid manual ledger"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := cold.dreamTransferGrant(g.GrantID); found {
		t.Fatal("invalid optional ledger reused previous authority")
	}
	if _, err := cold.createGrant(map[string]any{"recipient_id": "peer", "recipient": cold.identity.MeshRecipient,
		"project": "unrelated", "ttl_secs": 3600}); err != nil {
		t.Fatal("unrelated ordinary Mesh operation depended on optional ledger:", err)
	}
	still, err := os.ReadFile(mesh.dreamLedgerPath())
	if err != nil || string(still) != "invalid manual ledger" {
		t.Fatal("ordinary Mesh rewrote the separate optional ledger")
	}
}
