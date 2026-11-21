package decisionresearch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"time"
)

// A host grant is supplied by the operator, never by the retrieved document.
// Its two string values declare what observation would distinguish the claims.
type Binding struct {
	Gateway string `json:"gateway"`
	Project string `json:"project"`
	Topic   string `json:"topic_path"`
	Session string `json:"session_id"`
	Task    string `json:"task_id"`
	Attempt string `json:"attempt_id"`
	Agent   string `json:"agent_id"`
}
type Source struct {
	Kind     string `json:"kind"`
	MemoryID string `json:"memory_id,omitempty"`
	ObjectID string `json:"object_id,omitempty"`
	Field    string `json:"field,omitempty"`
	Repo     string `json:"repo,omitempty"`
	PR       int    `json:"pr,omitempty"`
	Head     string `json:"head,omitempty"`
}
type Grant struct {
	Schema  string            `json:"schema_id"`
	Binding Binding           `json:"binding"`
	CaseRef string            `json:"case_ref"`
	Source  Source            `json:"source"`
	Values  map[string]string `json:"values_by_claim"`
	Issued  time.Time         `json:"issued_at"`
	Expires time.Time         `json:"expires_at"`
	KnownBy *time.Time        `json:"as_known_at,omitempty"`
}

func (g Grant) Allows(c Case, b Binding, now time.Time) bool {
	if g.Schema != "conflict_source_grant.v1" || g.CaseRef != c.Ref || g.Binding != b ||
		!c.ValidFor(b.Project, b.Topic) || g.Issued.IsZero() || g.Issued.After(now) || !now.Before(g.Expires) ||
		g.Expires.Sub(g.Issued) > 24*time.Hour ||
		!claimID.MatchString(b.Session) || !claimID.MatchString(b.Task) || !claimID.MatchString(b.Attempt) || !claimID.MatchString(b.Agent) {
		return false
	}
	a, z := g.Values[c.Claims[0].ID], g.Values[c.Claims[1].ID]
	if len(g.Values) != 2 || a == "" || z == "" || a == z || len(a) > 128 || len(z) > 128 {
		return false
	}
	switch s := g.Source; s.Kind {
	case "memory_json":
		project, file, ok := strings.Cut(s.MemoryID, "::")
		return ok && project == b.Project && len(file) <= 400 && fs.ValidPath(file) && file != "." &&
			!strings.ContainsAny(file, "\\:\r\n") && component.MatchString(s.Field) && (s.ObjectID == "" || claimID.MatchString(s.ObjectID)) &&
			s.Repo == "" && s.PR == 0 && s.Head == ""
	case "github_pr":
		return s.MemoryID == "" && s.ObjectID == "" && s.Field == "" && s.Repo != "" && s.PR > 0 && len(s.Head) == 40
	}
	return false
}

type Observation struct {
	Value, Hash, Revision string
	Effective, Published  time.Time
}
type Result struct {
	Schema      string `json:"schema_id"`
	Case        string `json:"case_ref"`
	Status      string `json:"status"`
	Claim       string `json:"supported_claim_id,omitempty"`
	SourceHash  string `json:"source_hash,omitempty"`
	Revision    string `json:"source_revision,omitempty"`
	EffectiveAt string `json:"effective_at,omitempty"`
	ObservedAt  string `json:"observed_at"`
	Handoff     string `json:"session_event_id,omitempty"`
}

func Unresolved(c Case, reason string, now time.Time) Result {
	return Result{Schema: "conflict_source_result.v1", Case: c.Ref, Status: reason, ObservedAt: now.UTC().Format(time.RFC3339Nano)}
}

// Evaluate produces a time- and revision-scoped recommendation, not a lifecycle decision.
func Evaluate(g Grant, c Case, b Binding, o Observation, now time.Time) Result {
	r := Unresolved(c, "still_contested", now)
	if !g.Allows(c, b, now) {
		r.Status = "not_authorized"
		return r
	}
	if g.KnownBy != nil && now.After(*g.KnownBy) {
		r.Status = "not_known_at_cutoff"
		return r
	}
	digest, err := hex.DecodeString(o.Hash)
	if err != nil || len(digest) != sha256.Size || !claimID.MatchString(o.Revision) ||
		o.Effective.IsZero() || o.Published.IsZero() || o.Effective.After(now) || o.Published.After(now) ||
		(g.KnownBy != nil && (g.KnownBy.After(now) || o.Published.After(*g.KnownBy) || o.Effective.After(*g.KnownBy))) {
		return r
	}
	r.SourceHash, r.Revision, r.EffectiveAt = o.Hash, o.Revision, o.Effective.UTC().Format(time.RFC3339Nano)
	for _, claim := range c.Claims {
		if o.Value == g.Values[claim.ID] {
			r.Status, r.Claim = "supported_for_scope", claim.ID
			break
		}
	}
	return r
}

// MemoryOriginal verifies the named owner's exact bytes and revision. The original
// is data; only the predeclared scalar and timestamp are evaluated, never its prose.
func MemoryOriginal(s Source, c Case, data []byte) (Observation, error) {
	bad := errors.New("source identity or observation unavailable")
	var row struct {
		Owner    string `json:"source"`
		MemoryID string `json:"memory_id"`
		Memory   struct {
			Project string    `json:"project"`
			File    string    `json:"file"`
			Topic   string    `json:"topic_path"`
			Event   string    `json:"event_id"`
			Object  string    `json:"object_id"`
			Hash    string    `json:"content_hash"`
			Content string    `json:"content"`
			Updated time.Time `json:"updated_at"`
		} `json:"memory"`
	}
	if Decode(data, &row, false) != nil {
		return Observation{}, bad
	}
	m := row.Memory
	h := sha256.Sum256([]byte(m.Content))
	ownerHash := sha256.Sum256([]byte(strings.ToLower(strings.Join([]string{m.Project, m.File, m.Topic, m.Hash}, "|"))))
	wantObject := "obj_" + hex.EncodeToString(ownerHash[:12])
	if s.Kind != "memory_json" || row.Owner != "go_memory_store" || row.MemoryID != s.MemoryID || c.Project != m.Project || c.Topic != m.Topic ||
		m.Project+"::"+m.File != s.MemoryID || m.Object != wantObject || (s.ObjectID != "" && m.Object != s.ObjectID) || !claimID.MatchString(m.Event) ||
		m.Hash != hex.EncodeToString(h[:]) || len(m.Content) > 64<<10 || m.Updated.IsZero() {
		return Observation{}, bad
	}
	var fields map[string]json.RawMessage
	var value string
	var effective time.Time
	if Decode([]byte(m.Content), &fields, false) != nil || json.Unmarshal(fields[s.Field], &value) != nil ||
		json.Unmarshal(fields["effective_at"], &effective) != nil || value == "" || len(value) > 128 {
		return Observation{}, bad
	}
	return Observation{Value: value, Hash: m.Hash, Revision: m.Event, Effective: effective, Published: m.Updated}, nil
}
