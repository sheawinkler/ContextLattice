// Package decisionresearch binds an advisory check to two exact claim revisions.
// A case identifies the disagreement; it grants no source access or action rights.
package decisionresearch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

const Schema = "conflict_source_check.v1"

type Claim struct {
	ID       string `json:"claim_id"`
	Revision int    `json:"revision"`
}

type Case struct {
	Schema  string   `json:"schema_id"`
	Ref     string   `json:"case_ref"`
	Project string   `json:"project"`
	Topic   string   `json:"topic_path"`
	Branch  string   `json:"branch,omitempty"`
	Claims  [2]Claim `json:"claims"`
}

var component = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,119}$`)
var claimID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

func validPath(p string) bool {
	if len(p) > 240 {
		return false
	}
	if p == "" {
		return true
	}
	for _, part := range strings.Split(p, "/") {
		if !component.MatchString(part) {
			return false
		}
	}
	return true
}

func New(project, topic, branch string, a, b Claim) (Case, bool) {
	if !component.MatchString(project) || !validPath(topic) || !validPath(branch) ||
		!claimID.MatchString(a.ID) || !claimID.MatchString(b.ID) ||
		a.ID == b.ID || a.Revision < 1 || b.Revision < 1 {
		return Case{}, false
	}
	if b.ID < a.ID {
		a, b = b, a
	}
	c := Case{Schema: Schema, Project: project, Topic: topic, Branch: branch, Claims: [2]Claim{a, b}}
	body, _ := json.Marshal(c)
	h := sha256.Sum256(body)
	c.Ref = "cc_" + hex.EncodeToString(h[:])
	return c, true
}

func (c Case) ValidFor(project, topic string) bool {
	if c.Project != project || !validPath(topic) ||
		(topic != "" && c.Topic != topic && !strings.HasPrefix(c.Topic, topic+"/")) {
		return false
	}
	want, ok := New(c.Project, c.Topic, c.Branch, c.Claims[0], c.Claims[1])
	return ok && c == want
}
