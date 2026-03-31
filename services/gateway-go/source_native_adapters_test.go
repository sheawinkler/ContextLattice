package main

import "testing"

func TestCapMemoryBankBackendSequence(t *testing.T) {
	sequence := []string{"shodh_spike", "surrealdb_spike", "memvid_spike", "icm_spike"}
	capped := capMemoryBankBackendSequence(sequence, 2)
	if len(capped) != 2 {
		t.Fatalf("expected 2 backends after cap, got %d (%#v)", len(capped), capped)
	}
	if capped[0] != "shodh_spike" || capped[1] != "surrealdb_spike" {
		t.Fatalf("expected stable ordering after cap, got %#v", capped)
	}
	uncapped := capMemoryBankBackendSequence(sequence, 0)
	if len(uncapped) != len(sequence) {
		t.Fatalf("expected uncapped sequence length=%d, got %d", len(sequence), len(uncapped))
	}
}

func TestParseMemoryBankSpikeHedgeBackends(t *testing.T) {
	sequence := []string{"shodh_spike", "surrealdb_spike", "memvid_spike", "quickwit_spike"}
	candidates := parseMemoryBankSpikeHedgeBackends(sequence, "surrealdb_spike,shodh_spike,unknown", 2)
	if len(candidates) != 2 {
		t.Fatalf("expected 2 hedge candidates, got %d (%#v)", len(candidates), candidates)
	}
	// Sequence order is authoritative for deterministic tie-break behavior.
	if candidates[0] != "shodh_spike" || candidates[1] != "surrealdb_spike" {
		t.Fatalf("expected sequence-ordered hedge candidates, got %#v", candidates)
	}
	autoCandidates := parseMemoryBankSpikeHedgeBackends(sequence, "", 3)
	if len(autoCandidates) != 3 {
		t.Fatalf("expected first 3 sequence backends, got %#v", autoCandidates)
	}
	if autoCandidates[0] != "shodh_spike" || autoCandidates[2] != "memvid_spike" {
		t.Fatalf("unexpected automatic hedge backends: %#v", autoCandidates)
	}
}

func TestShouldReplaceMemoryBankHedgeWinnerDeterministicTieBreak(t *testing.T) {
	if !shouldReplaceMemoryBankHedgeWinner(0.91, 12, 0, -1, -1, -1) {
		t.Fatalf("expected first candidate to win when winner unset")
	}
	// Higher score always wins.
	if !shouldReplaceMemoryBankHedgeWinner(0.92, 8, 1, 0.91, 12, 0) {
		t.Fatalf("expected higher score candidate to win")
	}
	// With equal score, higher row count wins.
	if !shouldReplaceMemoryBankHedgeWinner(0.92, 13, 1, 0.92, 12, 0) {
		t.Fatalf("expected higher row-count candidate to win when score ties")
	}
	// With equal score and equal row count, lower order index wins (deterministic).
	if shouldReplaceMemoryBankHedgeWinner(0.92, 13, 2, 0.92, 13, 0) {
		t.Fatalf("did not expect later order candidate to replace existing deterministic winner")
	}
	if !shouldReplaceMemoryBankHedgeWinner(0.92, 13, 0, 0.92, 13, 2) {
		t.Fatalf("expected earlier order candidate to replace on full tie")
	}
}
