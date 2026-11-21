package main

import (
	"sort"
	"strings"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

// Missing, historical, differently scoped and unselected opponents are not
// research pointers. Returning their IDs could disclose a source the caller did not see.
func scopedConflictCheck(project, topic string, rows []temporalClaim) (decisionresearch.Case, bool) {
	byID := make(map[string]temporalClaim, len(rows))
	for _, row := range rows {
		if row.Project != project || !temporalClaimCanInfluence(row) ||
			(topic != "" && row.TopicPath != topic && !strings.HasPrefix(row.TopicPath, topic+"/")) {
			continue
		}
		byID[row.ClaimID] = row
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var selected decisionresearch.Case
	for _, id := range ids {
		left := byID[id]
		for _, target := range left.Contradicts {
			right, ok := byID[target]
			if !ok || right.TopicPath != left.TopicPath || left.Branch != right.Branch ||
				left.Subject == "" || left.Predicate == "" || left.Subject != right.Subject ||
				left.Predicate != right.Predicate || left.Object == "" || right.Object == "" || left.Object == right.Object {
				continue
			}
			c, ok := decisionresearch.New(project, left.TopicPath, left.Branch,
				decisionresearch.Claim{ID: id, Revision: left.Revision},
				decisionresearch.Claim{ID: target, Revision: right.Revision})
			if ok && (selected.Ref == "" || c.Ref < selected.Ref) {
				selected = c
			}
		}
	}
	return selected, selected.Ref != ""
}

const conflictCheckGuidance = "Before acting on the disputed premise, check one authorized original that distinguishes the two claim revisions. Record what it settles, or which evidence or owner is missing. Continue work that is safe under either claim. This is not permission to act."
