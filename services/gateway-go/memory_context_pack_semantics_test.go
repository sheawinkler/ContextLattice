package main

import "testing"

func TestContextPackAgentSectionsRecognizeDecisionOutcomeVocabulary(t *testing.T) {
	for _, summary := range []string{
		"Fleet steward review adjudication completed with a bounded final readback.",
		"Pull request merged after independent verification and approval.",
		"Operator disposition resolved the conflicting current-state records.",
	} {
		sections := contextPackAgentSections(nil, []map[string]any{{
			"project": "contextlattice", "file": "notes/outcome.md", "source": sourceTopicRollup,
			"topic_path": "orchestration/review", "summary": summary,
		}})
		if len(contextPackAnyList(sections["relevantDecisions"])) != 1 {
			t.Fatalf("verified decision outcome was not projected as decision evidence: summary=%q sections=%#v", summary, sections)
		}
	}
}

func TestContextPackAgentSectionsDoNotTreatSubstringAsDecisionOutcome(t *testing.T) {
	sections := contextPackAgentSections(nil, []map[string]any{{
		"project": "contextlattice", "file": "notes/pending.md", "source": sourceTopicRollup,
		"topic_path": "orchestration/review", "summary": "The unmerged candidate remains pending and has no approval.",
	}})
	if len(contextPackAnyList(sections["relevantDecisions"])) != 0 {
		t.Fatalf("pending unmerged work became decision evidence: %#v", sections)
	}
}
