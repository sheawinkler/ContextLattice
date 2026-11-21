package main

import (
	"context"
	"strings"
	"unicode/utf8"
)

const (
	decisionWitnessFileLimit    = 32 * 1024
	decisionWitnessSectionLimit = 4 * 1024
	decisionWitnessUnknown      = "unknown"
	decisionWitnessUnchanged    = "unchanged"
	decisionWitnessRevise       = "revise"
)

// The descriptor names a single extent of an existing, original artifact pin.
// Each required group contains interchangeable original support pins.
type decisionArtifactSectionWitness struct {
	ArtifactInputID string     `json:"artifact_input_id"`
	StartByte       int        `json:"start_byte"`
	EndByte         int        `json:"end_byte"`
	RequiredGroups  [][]string `json:"required_groups"`
}

func (w *decisionArtifactSectionWitness) valid(inputs map[string]bool) bool {
	if !inputs[w.ArtifactInputID] || w.StartByte < 0 || w.EndByte <= w.StartByte ||
		w.EndByte > decisionWitnessFileLimit || w.EndByte-w.StartByte > decisionWitnessSectionLimit ||
		len(w.RequiredGroups) < 1 || len(w.RequiredGroups) > 4 {
		return false
	}
	used := map[string]bool{w.ArtifactInputID: true}
	for _, group := range w.RequiredGroups {
		if len(group) == 0 || len(group) > 7 {
			return false
		}
		for _, id := range group {
			if !inputs[id] || used[id] {
				return false
			}
			used[id] = true
		}
	}
	return true
}

func (w *decisionArtifactSectionWitness) usesInput(id string) bool {
	if w.ArtifactInputID == id {
		return true
	}
	for _, group := range w.RequiredGroups {
		if containsString(group, id) {
			return true
		}
	}
	return false
}

func (w *decisionArtifactSectionWitness) hasSection(content string) bool {
	if w.StartByte < 0 || w.EndByte <= w.StartByte || w.EndByte > len(content) {
		return false
	}
	section := content[w.StartByte:w.EndByte]
	return utf8.ValidString(section) && strings.TrimSpace(section) != ""
}

// Use only direct owner text, never a derivative, mirror or saved preview.
// This reader returns a verified whole file at the shared reader's minimum cap.
func (m *memoryStore) readDecisionWitnessInput(ctx context.Context, entry memoryStoreEntry) (string, bool) {
	if memoryEntryHasDerivation(entry) || isEphemeralMemoryIdentity(entry.FileName, entry.TopicPath, entry.Summary, entry.Lifecycle) {
		return "", false
	}
	content, _, err := m.readVerifiedEvidencePreview(ctx, entry.Project, entry.FileName, entry.ContentHash,
		decisionWitnessFileLimit, decisionWitnessFileLimit)
	return content, err == nil && ctx.Err() == nil
}

func (m *memoryStore) evaluateDecisionWitness(ctx context.Context, a *decisionApplicability, fence *memoryEdgeLogFenceToken) string {
	w := a.ArtifactSectionWitness
	pins := make(map[string]decisionInputPin, len(a.Inputs))
	for _, pin := range a.Inputs {
		pins[pin.MemoryID] = pin
	}
	walk := newMemoryDerivationWalk()
	check := func(id string) string {
		pin := pins[id]
		project, file, _, _, err := canonicalMemoryID(pin.MemoryID)
		if err != nil || !strings.EqualFold(a.Scope.Project, project) {
			return decisionWitnessUnknown
		}
		entry, found := m.currentEntry(project, file)
		if !found || entry.Project != project || entry.FileName != file || m.checkMemoryDerivation(ctx, entry, fence, walk, 0) != nil {
			return decisionWitnessUnknown
		}
		content, available := m.readDecisionWitnessInput(ctx, entry)
		if !available {
			return decisionWitnessUnknown
		}
		ref := memoryStructuredReference{TargetID: pin.MemoryID, TargetEventID: pin.TargetEventID, TargetContentHash: pin.TargetContentHash}
		matches := memoryDerivationReferenceMatchesTarget(ref, entry)
		if id == w.ArtifactInputID {
			if !matches || !w.hasSection(content) {
				return decisionWitnessUnknown
			}
		} else if !matches {
			return decisionWitnessRevise
		}
		return decisionWitnessUnchanged
	}

	if check(w.ArtifactInputID) != decisionWitnessUnchanged {
		return decisionWitnessUnknown
	}
	result := decisionWitnessUnchanged
	for _, group := range w.RequiredGroups {
		groupResult := decisionWitnessRevise
		for _, alternative := range group {
			status := check(alternative)
			if status == decisionWitnessUnchanged {
				groupResult = status
				break
			}
			if status == decisionWitnessUnknown {
				groupResult = status
			}
		}
		if groupResult == decisionWitnessRevise {
			return groupResult
		}
		if groupResult == decisionWitnessUnknown {
			result = groupResult
		}
	}
	return result
}
