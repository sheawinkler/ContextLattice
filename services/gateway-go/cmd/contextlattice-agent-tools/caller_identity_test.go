package main

import "testing"

func TestNativeCallerPrecedenceAndScopedLegacy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_NATIVE_SESSION_ID", "")
	t.Setenv("CONTEXTLATTICE_AGENT", "mercury-agent")
	t.Setenv("CONTEXTLATTICE_AGENT_ID", "operator-id")
	t.Setenv("MEMMCP_AGENT_ID", "legacy-id")
	t.Setenv("CONTEXTLATTICE_MODEL", "explicit/model")
	t.Setenv("CONTEXTLATTICE_AGENT_MODEL", "secondary-model")
	profiles := map[string]any{"mercury-agent": map[string]any{"agent_id": "profile-id", "model": "not-runtime-evidence"}}
	caller := resolveNativeCaller(parsedArgs{}, profiles)
	if caller.agentID != "operator-id" || caller.agent != "mercury-agent" || caller.model != "explicit/model" {
		t.Fatalf("operator values did not outrank aliases/defaults: %#v", caller)
	}
	caller = resolveNativeCaller(parsedArgs{values: map[string]string{"agent": "omp", "agent_id": "flag-id", "model": "unknown"}}, profiles)
	if caller.agentID != "flag-id" || caller.agent != "omp" || caller.model != "" || caller.metadata()["model_known"] != false {
		t.Fatalf("flags did not win or explicit unknown got a model: %#v", caller)
	}
	t.Setenv("CONTEXTLATTICE_AGENT_ID", "")
	t.Setenv("CONTEXTLATTICE_MODEL", "")
	caller = resolveNativeCaller(parsedArgs{}, profiles)
	if caller.agentID != "legacy-id" || caller.model != "secondary-model" {
		t.Fatalf("legacy env did not outrank the profile: %#v", caller)
	}
	t.Setenv("MEMMCP_AGENT_ID", "")
	t.Setenv("CONTEXTLATTICE_AGENT_MODEL", "")
	caller = resolveNativeCaller(parsedArgs{}, profiles)
	if caller.agentID != "profile-id" || caller.model != "" {
		t.Fatalf("profile failed actor fallback or invented runtime model: %#v", caller)
	}
	writeSessionStateWithExtras("alpha", "legacy-session", "legacy", "legacy-id", map[string]any{
		"latest_context_pack_quality": map[string]any{"sample_id": "cpq_foreign"},
	})
	if len((nativeCaller{agentID: "operator-id"}).readSessionState("alpha")) != 0 {
		t.Fatal("a mismatched legacy pointer crossed caller boundaries")
	}
	state := (nativeCaller{agentID: "legacy-id"}).readSessionState("alpha")
	if state["session_id"] != "legacy-session" || state["latest_context_pack_quality"] != nil {
		t.Fatalf("legacy pointer migration trusted shared quality state: %#v", state)
	}
	operator := nativeCaller{agentID: "operator-id"}
	operator.writeSessionState("alpha", "operator-session", "new", nil)
	if got := (nativeCaller{agentID: "operator-id"}).readSessionState("alpha"); got["session_id"] != "operator-session" {
		t.Fatalf("caller pointer was not retained: %#v", got)
	}
	t.Setenv("CONTEXTLATTICE_NATIVE_SESSION_ID", "native-other")
	if len(resolveNativeCaller(parsedArgs{values: map[string]string{"agent_id": "operator-id"}}, profiles).readSessionState("alpha")) != 0 {
		t.Fatal("native session namespaces leaked")
	}
}

func TestNativeCallerPendingOutcomeCannotCrossActorOrSession(t *testing.T) {
	t.Setenv("CONTEXTLATTICE_GLOBAL_HOME", t.TempDir())
	t.Setenv("CONTEXTLATTICE_CONTEXT_PACK_QUALITY_SAMPLE_ID", "")
	a, b := nativeCaller{agentID: "runner-a"}, nativeCaller{agentID: "runner-b"}
	recordContextPackQualityPending("alpha", "sess-a", "older", a.agentID, map[string]any{"sample_id": "cpq_a"}, a)
	recordContextPackQualityPending("alpha", "sess-next", "newer", a.agentID, map[string]any{"sample_id": "cpq_next"}, a)
	if b.pendingContextPackSample(parsedArgs{}, "alpha", "sess-a") != "" || a.pendingContextPackSample(parsedArgs{}, "alpha", "sess-b") != "" {
		t.Fatal("sample crossed caller or session scope")
	}
	if got := a.pendingContextPackSample(parsedArgs{}, "alpha", "sess-a"); got != "cpq_a" {
		t.Fatalf("older scoped session lost its sample: %q", got)
	}
	markContextPackQualityReported("alpha", "sess-a", map[string]any{"outcome_id": "outcome-a"}, a)
	if a.pendingContextPackSample(parsedArgs{}, "alpha", "sess-a") != "" || a.readSessionState("alpha")["session_id"] != "sess-next" {
		t.Fatal("complete of an older session did not retire only its sample")
	}
}

func TestNativeCallerDefaultDoesNotInventHarnessOrModel(t *testing.T) {
	for _, key := range []string{"CONTEXTLATTICE_AGENT", "CONTEXTLATTICE_AGENT_ID", "MEMMCP_AGENT_ID", "CONTEXTLATTICE_MODEL", "CONTEXTLATTICE_AGENT_MODEL"} {
		t.Setenv(key, "")
	}
	caller := resolveNativeCaller(parsedArgs{}, map[string]any{"codex": map[string]any{"agent_id": "codex", "model": "unused"}})
	if caller.agent != "agent-cli" || caller.agentID != "agent-cli" || caller.explicit || caller.metadata()["model_known"] != false {
		t.Fatalf("default invented attribution: %#v", caller)
	}
}
