package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/contextlattice/gateway-go/internal/decisionresearch"
)

func TestConflictSourceNativeCLIOriginalAndSession(t *testing.T) {
	binary := os.Getenv("CONTEXTLATTICE_TEST_AGENT_TOOLS_BINARY")
	if binary == "" || runtime.GOOS == "windows" {
		t.Skip("requires qualified POSIX host and explicit native binary")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("expected absolute native binary")
	}
	const session, task, agent = "sess_research", "task_research", "reviewer"
	a := temporalClaim{ClaimID: "submitted", Project: "alpha", TopicPath: "review", Subject: "proposal", Predicate: "stage", Object: "submitted",
		Status: "active", Revision: 1, Contradicts: []string{"accepted"}}
	b := a
	b.ClaimID, b.Object, b.Contradicts = "accepted", "accepted", nil
	k, ok := scopedConflictCheck("alpha", "review", []temporalClaim{a, b})
	if !ok {
		t.Fatal("producer did not identify case")
	}
	owner := newDerivedMemoryTestStore(t)
	store := &agentSessionStore{path: filepath.Join(t.TempDir(), "sessions.json"), maxKeep: 16, maxEvents: 16, idleTTL: time.Hour,
		sessions: map[string]map[string]any{session: {"id": session, "project": "alpha", "agent_id": agent, "task_id": task,
			"status": "active", "scope_ownership": newAgentSessionPublicScopeOwnership("alpha")}}, events: map[string][]map[string]any{}, order: []string{session}}
	s := &server{agentSessions: store, memoryStore: owner, temporalClaims: &temporalClaimStore{enabled: true, claims: map[string]temporalClaim{a.ClaimID: a, b.ClaimID: b}}}
	var originals, posts atomic.Int32
	var packetBytes []byte
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/memory/synthesis-pack/v2":
			w.Header().Set("Content-Type", "application/json")
			w.Write(packetBytes)
		case "/memory/claims/query":
			s.memoryClaimsQuery(w, r)
		case "/v1/memory/get":
			originals.Add(1)
			s.memoryV1Get(w, r)
		case "/v1/agents/sessions/event":
			posts.Add(1)
			s.agentsSessionsEvent(w, r, "")
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer gateway.Close()
	packet := buildAgentPacket(map[string]any{"ok": true, "project": "alpha", "query": "review the decision", "agent_id": agent, "session_id": session, "task_id": task, "topic_path": "review",
		"token_impact":   map[string]any{"baseline_tokens_estimate": contextPackEstimateAnyTokens([]string{a.Object, b.Object})},
		"synthesis_pack": map[string]any{"decision_gate": map[string]any{"decision": "verify"}, "conflict_source_check": k}}, nil, "synthesis_pack_v2")
	wire := finalizeAgentPacketForRequest(packet, map[string]any{})
	if att := anyMap(wire["format_contract"]); att["contract_valid"] != true {
		t.Fatalf("actual producer did not attest packet: %v", att["validation"])
	}
	packetBytes, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	base := decisionresearch.Grant{Schema: "conflict_source_grant.v1", Binding: decisionresearch.Binding{Gateway: gateway.URL, Project: "alpha", Topic: "review", Session: session,
		Task: task, Attempt: "attempt1", Agent: agent}, CaseRef: k.Ref, Issued: time.Now().Add(-time.Minute), Expires: time.Now().Add(time.Hour),
		Source: decisionresearch.Source{Kind: "memory_json", MemoryID: "alpha::notes/approval.json", Field: "stage"}, Values: map[string]string{"submitted": "submitted", "accepted": "accepted"}}
	run := func(t *testing.T, g decisionresearch.Grant, wantStatus, wantClaim string) {
		t.Helper()
		home := t.TempDir()
		priv := filepath.Join(home, "operator")
		if err := os.Mkdir(priv, 0700); err != nil {
			t.Fatal(err)
		}
		grantPath, packetPath := filepath.Join(priv, "grant"), filepath.Join(priv, "packet")
		grantBytes, _ := json.Marshal(g)
		if err := os.WriteFile(grantPath, grantBytes, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(packetPath, packetBytes, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(binary, "policy-lab", "contradiction", "--operation", "research", "--project", "alpha", "--payload-file", packetPath, "--raw")
		cmd.Dir = home
		cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + home, "CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL,
			"CONTEXTLATTICE_AGENT_ID=" + agent, "CONTEXTLATTICE_ATTEMPT_ID=attempt1", "CONTEXTLATTICE_CONFLICT_GRANT_FILE=" + grantPath}
		data, err := cmd.CombinedOutput()
		var out struct {
			Mode        string
			Action      *bool `json:"action_authorized"`
			Mutated     *bool `json:"claim_state_changed"`
			Observation decisionresearch.Result
		}
		if err != nil || json.Unmarshal(data, &out) != nil || out.Mode != "advisory_only" || out.Action == nil || *out.Action || out.Mutated == nil || *out.Mutated ||
			out.Observation.Status != wantStatus || out.Observation.Claim != wantClaim || out.Observation.Handoff == "" || out.Observation.Case != k.Ref {
			t.Fatalf("native research want=%s/%s: err=%v output=%s", wantStatus, wantClaim, err, data)
		}
		_, events, _ := store.get(session)
		matches := 0
		for _, event := range events {
			if event["id"] == out.Observation.Handoff && anyMap(event["metadata"])["attempt_id"] == "attempt1" {
				matches++
			}
		}
		if matches != 1 || strings.Contains(string(data), "private original body") {
			t.Fatal("actual session receipt or redaction mismatch")
		}
	}
	t.Run("policy", func(t *testing.T) {
		for _, value := range []string{"submitted", "accepted"} {
			seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/approval.json", "review",
				`{"stage":"`+value+`","effective_at":"2025-01-01T00:00:00Z","note":"private original body"}`, "owner", "source")
			run(t, base, "supported_for_scope", value)
		}
		missing := base
		missing.Source.MemoryID = "alpha::notes/missing.json"
		run(t, missing, "source_unavailable", "")
		if originals.Load() != 3 || posts.Load() != 3 || s.temporalClaims.claims["accepted"].Revision != 1 {
			t.Fatal("unexpected reads, events or claim mutation")
		}
		// Optional explicit, credential-free qualification against one operator-named public PR.
		if repo := os.Getenv("CONTEXTLATTICE_TEST_PUBLIC_PR_REPO"); repo != "" {
			n, err := strconv.Atoi(os.Getenv("CONTEXTLATTICE_TEST_PUBLIC_PR_NUMBER"))
			if err != nil {
				t.Fatal(err)
			}
			g := base
			g.Source = decisionresearch.Source{Kind: "github_pr", Repo: repo, PR: n, Head: os.Getenv("CONTEXTLATTICE_TEST_PUBLIC_PR_HEAD")}
			g.Values = map[string]string{"submitted": "open", "accepted": "merged"}
			run(t, g, "supported_for_scope", "accepted")
			t.Log("verified one explicitly named real public merged PR with no credential")
		}
		t.Logf("one producer packet; opposite original revisions; withheld original; source_reads=%d bound_session_events=%d", originals.Load(), posts.Load())
	})
	t.Run("automatic", func(t *testing.T) {
		seedMemoryGraphRepairDoc(t, owner, "alpha", "notes/approval.json", "review", `{"stage":"accepted","effective_at":"2025-01-01T00:00:00Z"}`, "owner", "source")
		home := t.TempDir()
		priv := filepath.Join(home, "operator")
		if err := os.Mkdir(priv, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(priv, "grant")
		grant, _ := json.Marshal(base)
		if err := os.WriteFile(path, grant, 0600); err != nil {
			t.Fatal(err)
		}
		before := originals.Load()
		invoke := func(granted bool) ([]byte, []byte) {
			t.Helper()
			cmd := exec.Command(binary, "context", "review the decision", "--project", "alpha", "--topic-path", "review", "--session-id", session, "--agent-id", agent, "--task-id", task, "--raw")
			cmd.Dir = home
			cmd.Env = []string{"HOME=" + home, "TMPDIR=" + home, "PATH=/usr/bin:/bin", "CONTEXTLATTICE_ORCHESTRATOR_URL=" + gateway.URL,
				"CONTEXTLATTICE_ATTEMPT_ID=attempt1", "CONTEXTLATTICE_ASYNC_INBOX_AUTO_DRAIN=0"}
			if granted {
				cmd.Env = append(cmd.Env, "CONTEXTLATTICE_CONFLICT_GRANT_FILE="+path)
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("native context: %v, %s", err, stderr.String())
			}
			return stdout.Bytes(), stderr.Bytes()
		}
		plain, quiet := invoke(false)
		actual, adjunct := invoke(true)
		var obs decisionresearch.Result
		if !bytes.Equal(plain, actual) || len(bytes.TrimSpace(quiet)) != 0 || json.Unmarshal(adjunct, &obs) != nil ||
			obs.Claim != "accepted" || obs.Status != "supported_for_scope" || obs.Handoff == "" || originals.Load() != before+1 {
			t.Fatalf("automatic host did not deliver scoped observation without changing stdout: quiet=%q adjunct=%q reads=%d", quiet, adjunct, originals.Load()-before)
		}
		t.Log("ordinary CLI stdout byte-identical with grant on/off; one new original and one bound advisory event")
		again, receipt := invoke(true)
		var prior decisionresearch.Result
		if !bytes.Equal(again, plain) || json.Unmarshal(receipt, &prior) != nil || prior.Status != "already_checked_for_revision" ||
			prior.Handoff != obs.Handoff || prior.Claim != "" || originals.Load() != before+1 {
			t.Fatal("fresh process did not reuse just the authorized receipt")
		}
	})
}
