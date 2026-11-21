package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

const grantEnv = "CONTEXTLATTICE_CONFLICT_GRANT_FILE"

func (c *cli) observeContextConflict(packet map[string]any, project, topic, session, task, agent string) {
	if os.Getenv(grantEnv) == "" || project != firstString(packet["project"]) || topic != firstString(packet["topic_path"]) ||
		session == "" || session != firstString(packet["session_id"]) || task == "" || task != firstString(packet["task_id"]) ||
		agent == "" || agent != firstString(packet["agent_id"]) {
		return
	}
	c.handleConflictCheck(packet, decisionresearch.Binding{Gateway: c.baseURL, Project: project, Topic: topic, Session: session,
		Task: task, Attempt: envString("CONTEXTLATTICE_ATTEMPT_ID", envString("TASK_ATTEMPT_ID", "")), Agent: agent})
}

func readConflictGrant(path string) (decisionresearch.Grant, string, error) {
	var g decisionresearch.Grant
	bad := errors.New("conflict source grant unavailable")
	if !filepath.IsAbs(path) {
		return g, "", bad
	}
	data, err := readConflictPrivateFile(path, 8192)
	if err != nil {
		return g, "", bad
	}
	if decisionresearch.Decode(data, &g, true) != nil {
		return g, "", bad
	}
	hash := sha256.Sum256(data)
	return g, hex.EncodeToString(hash[:]), nil
}

type conflictCheckMemo struct {
	Key        string    `json:"key"`
	RetryAfter time.Time `json:"retry_after"`
	Attempts   int       `json:"attempts"`
	Event      string    `json:"event_id,omitempty"`
}

func conflictOriginAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
}

// The source grant is host-owned. Ignore instructions/URLs inside observations,
// never follow redirects, and cap bytes before decoding private content.
func (c *cli) conflictRequest(ctx context.Context, method, route string, payload any, limit int64) ([]byte, error) {
	bad := errors.New("authorized source unavailable")
	if !conflictOriginAllowed(c.baseURL) {
		return nil, bad
	}
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, bad
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.baseURL, "/")+route, body)
	if err != nil {
		return nil, bad
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("X-Api-Key", c.apiKey)
	}
	client := *c.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, bad
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, bad
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, bad
	}
	return data, nil
}

func (c *cli) conflictCaseCurrent(ctx context.Context, k decisionresearch.Case, b decisionresearch.Binding) bool {
	data, err := c.conflictRequest(ctx, http.MethodPost, "/memory/claims/query", map[string]any{
		"project": b.Project, "topic_path": b.Topic, "conflict_source_check": k, "session_id": b.Session, "task_id": b.Task, "agent_id": b.Agent,
	}, 4096)
	var row struct {
		OK      bool   `json:"ok"`
		Current bool   `json:"current"`
		Ref     string `json:"case_ref"`
	}
	return err == nil && json.Unmarshal(data, &row) == nil && row.OK && row.Current && row.Ref == k.Ref
}

func (c *cli) checkOriginal(ctx context.Context, g decisionresearch.Grant, k decisionresearch.Case) (decisionresearch.Observation, error) {
	if g.Source.Kind == "github_pr" {
		pr, err := decisionresearch.ReadGitHubPR(ctx, g.Source.Repo, g.Source.PR, g.Source.Head)
		if err != nil {
			return decisionresearch.Observation{}, err
		}
		return decisionresearch.Observation{Value: pr.State, Hash: pr.ContentHash, Revision: pr.Revision,
			Effective: pr.EffectiveAt, Published: pr.EffectiveAt}, nil
	}
	if g.Source.Kind != "memory_json" {
		return decisionresearch.Observation{}, errors.New("source not available")
	}
	data, err := c.conflictRequest(ctx, http.MethodGet, "/v1/memory/get?"+url.Values{"memory_id": []string{g.Source.MemoryID}}.Encode(), nil, 256<<10)
	if err != nil {
		return decisionresearch.Observation{}, err
	}
	return decisionresearch.MemoryOriginal(g.Source, k, data)
}

// stdout remains the byte-exact registered packet. Only a compact advisory
// result is shown on stderr and appended to this specific session; no raw source
// text, secret, approval, claim decision or executable instruction is forwarded.
func (c *cli) handleConflictCheck(packet map[string]any, b decisionresearch.Binding) {
	k, ok := packetConflictCase(packet, b)
	if !ok || os.Getenv(grantEnv) == "" {
		return
	}
	c.checkConflict(k, b, func(value decisionresearch.Result) {
		encoded, _ := json.Marshal(value)
		fmt.Fprintln(c.stderr, string(encoded))
	})
}

func packetConflictCase(packet map[string]any, b decisionresearch.Binding) (decisionresearch.Case, bool) {
	raw := asMap(packet["decision_gate"])["conflict_source_check"]
	if raw == nil || firstString(packet["schema_id"]) != agentPacketContractID || packet["ok"] != true {
		return decisionresearch.Case{}, false
	}
	data, err := json.Marshal(raw)
	var k decisionresearch.Case
	ok := err == nil && len(data) <= 2048 && decisionresearch.Decode(data, &k, true) == nil && k.ValidFor(b.Project, b.Topic)
	return k, ok
}

func (c *cli) checkConflict(k decisionresearch.Case, b decisionresearch.Binding, emit func(decisionresearch.Result)) {
	path := os.Getenv(grantEnv)
	now := time.Now().UTC()
	g, key, err := readConflictGrant(path)
	if err != nil || !g.Allows(k, b, now) || !conflictOriginAllowed(b.Gateway) {
		emit(decisionresearch.Unresolved(k, "not_authorized", now))
		return
	}
	unlock, err := acquireConflictSourceLock(path + ".lock")
	if err != nil {
		emit(decisionresearch.Unresolved(k, "check_in_progress_or_unavailable", now))
		return
	}
	defer unlock()
	valid := func() bool {
		fresh, revision, err := readConflictGrant(path)
		return err == nil && revision == key && fresh.Allows(k, b, time.Now().UTC())
	}
	if !valid() {
		emit(decisionresearch.Unresolved(k, "not_authorized", now))
		return
	}
	memoPath := path + ".last-check"
	prior, readErr := readConflictPrivateFile(memoPath, 2048)
	memo := conflictCheckMemo{Key: key, Attempts: 1, RetryAfter: now.Add(5 * time.Minute)}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if !errors.Is(readErr, os.ErrNotExist) {
		var m conflictCheckMemo
		if readErr != nil || decisionresearch.Decode(prior, &m, true) != nil || m.Attempts < 1 || m.Attempts > 2 {
			emit(decisionresearch.Unresolved(k, "check_cache_unavailable", now))
			return
		}
		if m.Key == key {
			if now.Before(m.RetryAfter) || m.Attempts >= 2 {
				repeat := decisionresearch.Unresolved(k, "already_checked_for_revision", now)
				// Reuse only the pointer, never an old recommendation. A receipt
				// from a superseded case or a withdrawn grant is not re-delivered.
				if m.Event != "" && c.conflictCaseCurrent(ctx, k, b) && valid() {
					repeat.Handoff = m.Event
				}
				emit(repeat)
				return
			}
			memo.Attempts = m.Attempts + 1
		}
	}
	if !c.conflictCaseCurrent(ctx, k, b) {
		emit(decisionresearch.Unresolved(k, "case_unavailable_or_changed", now))
		return
	}
	if !valid() {
		emit(decisionresearch.Unresolved(k, "not_authorized", now))
		return
	}
	// Reserve before issuing the read so a crash or a lost acknowledgement
	// cannot immediately repeat it. The bounded slot expires without a daemon.
	if err := writePrivateJSONArtifact(memoPath, memo); err != nil {
		emit(decisionresearch.Unresolved(k, "check_cache_unavailable", now))
		return
	}
	obs, readErr := c.checkOriginal(ctx, g, k)
	result := decisionresearch.Unresolved(k, "source_unavailable", time.Now().UTC())
	if readErr == nil {
		result = decisionresearch.Evaluate(g, k, b, obs, time.Now().UTC())
	}
	if !valid() {
		emit(decisionresearch.Unresolved(k, "not_authorized", time.Now().UTC()))
		return
	}
	if !c.conflictCaseCurrent(ctx, k, b) {
		emit(decisionresearch.Unresolved(k, "case_unavailable_or_changed", time.Now().UTC()))
		return
	}
	if !valid() {
		emit(decisionresearch.Unresolved(k, "not_authorized", time.Now().UTC()))
		return
	}
	event := map[string]any{"session_id": b.Session, "project": b.Project, "task_id": b.Task, "agent_id": b.Agent, "type": "conflict.source_checked",
		"summary": result.Status, "metadata": map[string]any{"result": result, "task_id": b.Task, "attempt_id": b.Attempt}}
	response, err := c.conflictRequest(ctx, http.MethodPost, "/v1/agents/sessions/event", event, 64<<10)
	var ack struct {
		OK    bool `json:"ok"`
		Event struct {
			ID       string `json:"id"`
			Session  string `json:"session_id"`
			Project  string `json:"project"`
			Agent    string `json:"agent_id"`
			Type     string `json:"type"`
			Metadata struct {
				Task    string                  `json:"task_id"`
				Attempt string                  `json:"attempt_id"`
				Result  decisionresearch.Result `json:"result"`
			} `json:"metadata"`
		} `json:"event"`
	}
	if err != nil || decisionresearch.Decode(response, &ack, false) != nil || !ack.OK || !strings.HasPrefix(ack.Event.ID, "evt_") || len(ack.Event.ID) > 128 ||
		ack.Event.Type != "conflict.source_checked" || ack.Event.Project != b.Project || ack.Event.Session != b.Session ||
		ack.Event.Agent != b.Agent || ack.Event.Metadata.Task != b.Task || ack.Event.Metadata.Attempt != b.Attempt || ack.Event.Metadata.Result != result {
		emit(decisionresearch.Unresolved(k, "handoff_unconfirmed", time.Now().UTC()))
		return
	}
	if !valid() {
		emit(decisionresearch.Unresolved(k, "not_authorized", time.Now().UTC()))
		return
	}
	result.Handoff = ack.Event.ID
	memo.Event = ack.Event.ID
	if readErr == nil {
		memo.RetryAfter = g.Expires
	}
	// The pre-read reservation remains authoritative even if updating its
	// optional receipt pointer fails; the session itself owns the event.
	_ = writePrivateJSONArtifact(memoPath, memo)
	emit(result)
}

// This is the existing Policy Lab's host-side, read-only operation. Never route
// this payload to the operator decision/appeal endpoint.
func (c *cli) policyConflictResearch(path, project string, pretty bool) error {
	if path == "" || project == "" {
		return errors.New("research requires an explicit project and compact packet file")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("research packet unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return errors.New("invalid research packet")
	}
	data, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	var packet map[string]any
	if err != nil || decisionresearch.Decode(data, &packet, false) != nil || firstString(packet["project"]) != project ||
		!adapterRegisteredEnvelopeAttestationValid(agentPacketContractID, packet) {
		return errors.New("invalid research packet or project")
	}
	b := decisionresearch.Binding{Gateway: c.baseURL, Project: project, Topic: firstString(packet["topic_path"]), Session: firstString(packet["session_id"]),
		Task: firstString(packet["task_id"]), Agent: envString("CONTEXTLATTICE_AGENT_ID", ""), Attempt: envString("CONTEXTLATTICE_ATTEMPT_ID", envString("TASK_ATTEMPT_ID", ""))}
	k, ok := packetConflictCase(packet, b)
	if !ok || b.Agent == "" || firstString(packet["agent_id"]) != b.Agent {
		return errors.New("no scoped research case for this actor")
	}
	var result decisionresearch.Result
	c.checkConflict(k, b, func(v decisionresearch.Result) { result = v })
	return c.emit(map[string]any{"mode": "advisory_only", "claim_state_changed": false, "action_authorized": false, "observation": result}, pretty)
}
