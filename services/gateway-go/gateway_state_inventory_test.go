package main

import (
	"path/filepath"
	"testing"

	"github.com/contextlattice/gateway-go/internal/gatewaystate"
)

func gatewayStateInventoryEntryByID(t *testing.T, id string) gatewaystate.EntryInput {
	t.Helper()
	for _, entry := range gatewayStateInventoryEntries() {
		if entry.ID == id {
			return entry
		}
	}
	t.Fatalf("gateway state inventory omitted %q", id)
	return gatewaystate.EntryInput{}
}

func TestGatewayStateInventoryEntryIDsAreUnique(t *testing.T) {
	seen := map[string]struct{}{}
	for _, entry := range gatewayStateInventoryEntries() {
		if _, exists := seen[entry.ID]; exists {
			t.Fatalf("gateway state inventory contains duplicate id %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
	}
}

func TestContextPackRegressionFixtureInventoryUsesExplicitPath(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	fixturePath := filepath.Join(t.TempDir(), "fixture", "regressions.ndjson")
	t.Setenv(gatewaystate.RootEnv, stateRoot)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", filepath.Join(t.TempDir(), "quality.ndjson"))
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", fixturePath)

	entry := gatewayStateInventoryEntryByID(t, "context_pack_regression_fixtures")
	if entry.Path != fixturePath || entry.Source != "surface_override" || entry.SourceEnv != "GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH" || entry.StorageTier != "explicit_override" {
		t.Fatalf("explicit fixture path did not retain its exact inventory resolution: %#v", entry)
	}
	if entry.Kind != "file" || entry.PersistenceClass != "owner_only_bounded_durable_file" {
		t.Fatalf("explicit fixture inventory weakened owner-only persistence: %#v", entry)
	}
}

func TestContextPackRegressionFixtureInventoryFallsBackBesideExplicitQualityLedger(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	qualityPath := filepath.Join(t.TempDir(), "quality", "quality.ndjson")
	t.Setenv(gatewaystate.RootEnv, stateRoot)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", qualityPath)
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", "")

	entry := gatewayStateInventoryEntryByID(t, "context_pack_regression_fixtures")
	wantPath := filepath.Join(filepath.Dir(qualityPath), contextPackRegressionFixtureLedgerFilename)
	if entry.Path != wantPath || entry.Source != "derived_quality_ledger" || entry.SourceEnv != "GO_CONTEXT_PACK_QUALITY_LEDGER_PATH" || entry.StorageTier != "explicit_override" {
		t.Fatalf("fixture path did not derive from explicit quality ledger: got=%#v wantPath=%q", entry, wantPath)
	}
}

func TestContextPackRegressionFixtureInventoryFallsBackToCanonicalStateRoot(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	t.Setenv(gatewaystate.RootEnv, stateRoot)
	t.Setenv("GO_CONTEXT_PACK_QUALITY_LEDGER_PATH", "")
	t.Setenv("GO_CONTEXT_PACK_REGRESSION_FIXTURE_LEDGER_PATH", "")

	entry := gatewayStateInventoryEntryByID(t, "context_pack_regression_fixtures")
	wantPath := filepath.Join(stateRoot, contextPackRegressionFixtureLedgerFilename)
	if entry.Path != wantPath || entry.Source != "derived_quality_ledger" || entry.SourceEnv != gatewaystate.RootEnv || entry.StorageTier != "operator_configured" {
		t.Fatalf("fixture path did not derive from canonical state root: got=%#v wantPath=%q", entry, wantPath)
	}
}
