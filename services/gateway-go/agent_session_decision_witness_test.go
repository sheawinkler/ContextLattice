package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	witnessArtifact = "notes/artifact.md"
	witnessBackup   = "notes/backup.md"
	witnessPrimary  = "notes/input.md"
	witnessDecisive = "notes/decisive.md"
)

func decisionWitnessFixture(t *testing.T) (*continuityStore, *memoryStore, map[string]any) {
	t.Helper()
	store, owner, payload := decisionApplicabilityFixture(t)
	a := anyMap(payload["applicability"])
	inputs := contextPackAnyList(a["inputs"])
	for _, row := range []struct{ file, content string }{
		{witnessArtifact, "caf\xc3\xa9 section body\n\nSibling section"},
		{witnessBackup, "Alternative source body"},
		{witnessDecisive, "Decisive source body"},
	} {
		entry := seedMemoryGraphRepairDoc(t, owner, "alpha", row.file, "pilot/witness", row.content, "test", "session")
		inputs = append(inputs, map[string]any{"memory_id": "alpha::" + row.file, "target_event_id": entry.EventID, "target_content_hash": entry.ContentHash})
	}
	a["inputs"] = inputs
	a["artifact_section_witness"] = map[string]any{"artifact_input_id": "alpha::" + witnessArtifact,
		"start_byte": 0, "end_byte": len("caf\xc3\xa9 section body"),
		"required_groups": [][]string{{"alpha::" + witnessPrimary, "alpha::" + witnessBackup}, {"alpha::" + witnessDecisive}},
	}
	return store, owner, payload
}

func TestDecisionArtifactSectionWitnessValidatesOriginalRegistration(t *testing.T) {
	store, owner, payload := decisionWitnessFixture(t)
	for _, tc := range []struct {
		name  string
		key   string
		value any
	}{
		{"case alias", "Start_Byte", 0},
		{"null byte offset", "start_byte", nil},
		{"oversized range", "end_byte", decisionWitnessSectionLimit + 1},
		{"unknown artifact", "artifact_input_id", "alpha::notes/unregistered.md"},
		{"null groups", "required_groups", nil},
		{"empty alternative group", "required_groups", [][]string{{}}},
		{"artifact cannot support itself", "required_groups", [][]string{{"alpha::" + witnessArtifact}}},
		{"unregistered source", "required_groups", [][]string{{"alpha::notes/unregistered.md"}}},
		{"reused support", "required_groups", [][]string{{"alpha::" + witnessPrimary}, {"alpha::" + witnessPrimary}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := cloneJSONMap(payload)
			w := anyMap(anyMap(copy["applicability"])["artifact_section_witness"])
			w[tc.key] = tc.value
			if _, _, err := store.recordDecisionChangeWithOwner(copy, decisionOwnerValidator(owner)); err == nil {
				t.Fatal("invalid section witness was accepted")
			}
		})
	}
	for _, end := range []int{4, 512} {
		copy := cloneJSONMap(payload)
		anyMap(anyMap(copy["applicability"])["artifact_section_witness"])["end_byte"] = end
		if _, _, err := store.recordDecisionChangeWithOwner(copy, decisionOwnerValidator(owner)); !errors.Is(err, errDecisionApplicabilityOwner) {
			t.Fatalf("unproven section boundary %d: %v", end, err)
		}
	}
	change, _, err := store.recordDecisionChangeWithOwner(payload, decisionOwnerValidator(owner))
	if err != nil || change.Applicability == nil || change.Applicability.ArtifactSectionWitness == nil {
		t.Fatalf("valid section not retained: %v", err)
	}
	store.close()
	reopened, err := newContinuityStoreFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	replay, _, err := reopened.recordDecisionChange(payload)
	if err != nil || !replay.idempotentReplay || agentDecisionDigest(replay) != agentDecisionDigest(change) {
		t.Fatalf("exact replay lost the accepted section witness: %v", err)
	}
	conflict := cloneJSONMap(payload)
	anyMap(anyMap(conflict["applicability"])["artifact_section_witness"])["end_byte"] = 5
	if _, _, err := reopened.recordDecisionChangeWithOwner(conflict, decisionOwnerValidator(owner)); err == nil {
		t.Fatal("changed section reused the accepted record's idempotency key")
	}
}

func TestAgentDecisionArtifactSectionWitnessRecomputesShadowOnly(t *testing.T) {
	continuity, owner, payload := decisionWitnessFixture(t)
	s, _, sessionID := agentDecisionResumeStartFixture(t, continuity, owner, payload)
	expect := func(want string) {
		t.Helper()
		for _, compact := range []bool{false, true} {
			response := decisionResumeResponse(t, s, sessionID, compact)
			projection := anyMap(response["decision_continuity"])
			if compact {
				projection = anyMap(anyMap(response["provenance"])["decision_continuity"])
			}
			witness := anyMap(projection["artifact_section_witness"])
			if response["ok"] != true || projection["status"] != "unknown" ||
				len(witness) != 2 || witness["status"] != want || witness["mode"] != "manual_shadow_only" {
				t.Fatalf("compact=%v want shadow %q, got %#v", compact, want, projection)
			}
			raw, _ := json.Marshal(response)
			for _, text := range []string{"source body", "section body", "Historical deployment intent", "Deploy the pinned build", "historical_intent",
				witnessPrimary, witnessBackup, witnessDecisive, witnessArtifact} {
				if strings.Contains(string(raw), text) {
					t.Fatalf("compact=%v leaked %q", compact, text)
				}
			}
		}
	}
	ledgerBefore, err := os.ReadFile(continuity.path)
	if err != nil {
		t.Fatal(err)
	}
	sessionBefore, err := os.ReadFile(s.agentSessions.path)
	if err != nil {
		t.Fatal(err)
	}
	expect(decisionWitnessUnchanged)
	seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/sibling.md", "pilot/witness", "Unrelated edit", "test", "session")
	expect(decisionWitnessUnchanged)

	if _, _, err := owner.put(normalizedWrite{project: "alpha", fileName: witnessPrimary, topicPath: "pilot/witness",
		content: "Withdrawn first alternative", lifecycle: "retracted"}); err != nil {
		t.Fatal(err)
	}
	expect(decisionWitnessUnchanged)

	backupPath := filepath.Join(owner.policy.rootPath, "alpha", witnessBackup)
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}
	expect(decisionWitnessUnknown) // The withdrawn alternative cannot stand in for unavailable current bytes.
	if err := os.WriteFile(backupPath, backupBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	expect(decisionWitnessUnchanged)

	// An authorized replacement proves loss of the decisive original pin.
	seedMemoryGraphRepairDoc(t, owner, "alpha", witnessDecisive, "pilot/witness", "Published withdrawal notice", "test", "session")
	expect(decisionWitnessRevise)
	decisivePath := filepath.Join(owner.policy.rootPath, "alpha", witnessDecisive)
	if err := os.Remove(decisivePath); err != nil {
		t.Fatal(err)
	}
	expect(decisionWitnessUnknown) // Metadata alone does not prove current visibility.

	for _, snapshot := range []struct {
		path   string
		before []byte
	}{{continuity.path, ledgerBefore}, {s.agentSessions.path, sessionBefore}} {
		after, err := os.ReadFile(snapshot.path)
		if err != nil || !bytes.Equal(snapshot.before, after) {
			t.Fatalf("shadow resume modified durable state: %v", err)
		}
	}
}

func TestAgentDecisionArtifactSectionWitnessFinalFenceAndScope(t *testing.T) {
	continuity, owner, payload := decisionWitnessFixture(t)
	s, _, sessionID := agentDecisionResumeStartFixture(t, continuity, owner, payload)
	ctx, now := context.Background(), time.Now().UTC()
	row, events, _ := s.agentSessions.get(sessionID)
	o := s.observeAgentDecisionResume(ctx, row, now)
	if o.witness == nil {
		t.Fatal("selected record lost its witness")
	}
	seedMemoryGraphRepairDoc(t, owner, "alpha", witnessDecisive, "pilot/witness", "Changed after initial observation", "test", "session")
	final, err := o.finish(ctx, s, row, events, now, false, nil)
	witness := anyMap(anyMap(final["decision_continuity"])["artifact_section_witness"])
	if err != nil || witness["status"] != decisionWitnessRevise {
		t.Fatalf("final resume did not use fresh owner evidence: %v %#v", err, witness)
	}
	rowRestricted := cloneJSONMap(row)
	rowRestricted["scope_ownership"] = map[string]any{"kind": "foreign", "project": "alpha"}
	if status, visible, err := s.finishAgentDecisionWitness(ctx, rowRestricted, o.witness, func() error { return nil }); err != nil || visible || status != decisionWitnessUnknown {
		t.Fatalf("restricted scope received witness details: %q %t %v", status, visible, err)
	}
	delete(rowRestricted, "scope_ownership")
	if status, visible, err := s.finishAgentDecisionWitness(ctx, rowRestricted, o.witness, func() error { return nil }); err != nil || visible || status != decisionWitnessUnknown {
		t.Fatalf("unproven scope received witness details: %q %t %v", status, visible, err)
	}
	fence, err := owner.acquireMemoryEdgeLogFence()
	if err != nil {
		t.Fatal(err)
	}
	defer fence.release()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	wait := &agentDecisionFenceWaitContext{Context: deadline, waiting: make(chan struct{})}
	type outcome struct {
		payload map[string]any
		err     error
	}
	finished := make(chan outcome, 1)
	go func() {
		p, err := o.finish(wait, s, row, events, now, true, nil)
		finished <- outcome{p, err}
	}()
	select {
	case <-wait.waiting:
	case <-deadline.Done():
		t.Fatal("witness did not wait on the owner fence")
	}
	if _, _, err := s.agentSessions.appendEvent(sessionID, map[string]any{"type": "checkpoint.written", "summary": "A newer checkpoint"}); err != nil {
		t.Fatal(err)
	}
	fence.release()
	select {
	case got := <-finished:
		witness = anyMap(agentDecisionResumeProjection(got.payload, true)["artifact_section_witness"])
		if got.err != nil || witness["status"] != decisionWitnessUnknown {
			t.Fatalf("session state drift did not suppress stale witness: %v %#v", got.err, witness)
		}
	case <-deadline.Done():
		t.Fatal("witness did not finish after the owner fence was released")
	}
}
