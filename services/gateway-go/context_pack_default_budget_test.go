package main

import "testing"

func TestContextPackTokenBudgetUsesConfiguredDefaultWithoutOverridingExplicitBudget(t *testing.T) {
	t.Setenv("GO_CONTEXT_PACK_DEFAULT_TARGET_TOKENS", "4096")
	budget := contextPackTokenBudgetFromRequest(map[string]any{})
	if !budget.Active || budget.TargetContextPackTokens != 4096 || budget.RankedEvidenceTokens != 2457 {
		t.Fatalf("configured default token budget was not activated: %#v", budget)
	}

	explicit := contextPackTokenBudgetFromRequest(map[string]any{"target_context_pack_tokens": 260})
	if !explicit.Active || explicit.TargetContextPackTokens != 260 {
		t.Fatalf("explicit token budget did not override the default: %#v", explicit)
	}

	t.Setenv("GO_CONTEXT_PACK_DEFAULT_TARGET_TOKENS", "0")
	disabled := contextPackTokenBudgetFromRequest(map[string]any{})
	if disabled.Active || disabled.TargetContextPackTokens != 0 {
		t.Fatalf("zero operator default did not preserve the legacy opt-out: %#v", disabled)
	}
}
