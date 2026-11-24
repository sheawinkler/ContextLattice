package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const namedDecisionEnv = "CONTEXTLATTICE_NAMED_DECISION"
const namedDecisionSchema = "temporal_decision_context.v1"

var namedDecisionPath = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*(/[A-Za-z0-9_-][A-Za-z0-9_.-]*)*$`)
var namedDecisionID = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

var namedDecisionActor = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:@-]{0,119}$`)

var namedDecisionSession = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,159}$`)

type namedDecisionPin struct{ project, topic, id, scope, actor, native string }

func selectedNamedDecision(p parsedArgs, project string) *namedDecisionPin {
	raw := os.Getenv(namedDecisionEnv)
	if raw == "" || len(raw) > 1024 || p.has("base_url") {
		return nil
	}
	// The selected address is independent of the pointer. No implicit project,
	// query-text match, topic prefix, session cache or repo profile lookup.
	requestProject, explicit := p.values["project"]
	if !explicit {
		requestProject = os.Getenv("CONTEXTLATTICE_PROJECT")
	}
	topic, explicit := p.values["topic_path"]
	if !explicit {
		topic = os.Getenv("CONTEXTLATTICE_ACTIVE_TOPIC_PATH")
	}
	scope := p.values["decision_scope"]
	if project != requestProject || !mcpProject.MatchString(project) || strings.ToLower(project) != project ||
		len(topic) > 240 || !namedDecisionPath.MatchString(topic) ||
		len(scope) > 120 || (scope != "" && !namedDecisionPath.MatchString(scope)) {
		return nil
	}
	v, ok := mcpDecode([]byte(raw))
	pin, _ := v.(map[string]any)
	id, _ := pin["decision_id"].(string)
	actor, explicit := p.values["agent_id"]
	if !explicit {
		actor = envString("CONTEXTLATTICE_AGENT_ID", envString("MEMMCP_AGENT_ID", ""))
	}
	native := p.string("native_session_id", envString("CONTEXTLATTICE_NATIVE_SESSION_ID", ""))
	if !ok || len(pin) != 5 || !namedDecisionActor.MatchString(actor) || len(native) > 160 || strings.HasPrefix(native, "-") || strings.IndexFunc(native, unicode.IsControl) >= 0 || pin["agent_id"] != actor ||
		pin["project"] != project || pin["topic_path"] != topic || pin["scope"] != scope || !namedDecisionID.MatchString(id) {
		return nil
	}
	return &namedDecisionPin{project, topic, id, scope, actor, native}
}

// Do not splice new fields into an attested packet or persist a looked-up
// decision in session state. Only the opted-in presentation changes shape.
func (c *cli) emitNamedDecision(base map[string]any, pin *namedDecisionPin, session string, packet bool, budget int, pretty bool) error {
	if pin == nil || base["ok"] != true || base["project"] != pin.project ||
		(packet && (base["schema_id"] != agentPacketContractID || base["topic_path"] != pin.topic)) {
		return c.emit(base, pretty)
	}
	for _, row := range []map[string]any{base, asMap(base["session"]), asMap(base["rollup"])} {
		if topic, exists := row["topic_path"]; exists && topic != "" && topic != pin.topic {
			return c.emit(base, pretty)
		}
		if actor, exists := row["agent_id"]; exists && actor != pin.actor {
			return c.emit(base, pretty)
		}
	}
	receipt := map[string]any{"project": pin.project, "topic_path": pin.topic, "decision_id": pin.id, "scope": pin.scope,
		"source": namedDecisionSchema, "status": "unavailable"}
	continuation, hasContinuation := base["continuation"]
	if namedDecisionSession.MatchString(session) && base["session_id"] == session {
		c.readNamedDecision(pin, session, receipt)
	}
	if receipt["status"] == "needs_source_check" && ((packet || hasContinuation) && !namedDecisionReady(asMap(continuation)) || asMap(base["decision_gate"])["refusal"] == true) {
		delete(receipt, "recorded_value")
		receipt["status"] = "warming_or_unavailable"
	}
	if size, ok := packCommandOutputWireBytes(receipt, false); !ok || size > 1536 {
		delete(receipt, "recorded_value")
	}
	envelope := map[string]any{"schema_id": "cli_scoped_recall.v1", "context": base, "named_decision": receipt}
	if budget > 0 {
		if _, fits := packCommandOutputModeForBudget(envelope, budget, false); !fits {
			delete(receipt, "recorded_value")
			if _, fits = packCommandOutputModeForBudget(envelope, budget, false); !fits {
				return c.emit(base, pretty)
			}
		}
	}
	return c.emit(envelope, false)
}

func namedDecisionReady(c map[string]any) bool {
	switch c["status"] {
	case "complete", "completed", "none", "ready":
	default:
		return false
	}
	switch c["result_state"] {
	case "", "ready", "complete", "completed", "final":
	default:
		return false
	}
	pending, ok := c["pending_sources"].([]any)
	return ok && len(pending) == 0 && c["token"] == ""
}

func (c *cli) readNamedDecision(pin *namedDecisionPin, session string, out map[string]any) {
	// Only use the operator-configured gateway and its normal CLI authority.
	// In particular, do not adopt a repo .env key, a caller URL override, or
	// follow a redirect with the scoped selector and credentials.
	key := os.Getenv("CONTEXTLATTICE_ORCHESTRATOR_API_KEY")
	if key == "" || c.apiKey != key || c.baseURL != baseURLFromEnv() || !conflictOriginAllowed(c.baseURL) || strings.ContainsAny(c.baseURL, "?#") {
		return
	}
	client := *c.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	var owner map[string]any
	sessionVerified := false
	client.Transport = decisionReadTransport(func(req *http.Request) (*http.Response, error) {
		bad := errors.New("named decision unavailable")
		sessionRead := !sessionVerified && req.Method == http.MethodGet && req.URL.String() == c.baseURL+"/v1/agents/sessions/"+url.PathEscape(session)
		ownerRead := sessionVerified && owner == nil && req.Method == http.MethodPost && req.URL.String() == c.baseURL+"/memory/claims/query" && pin.exactOwnerQuery(req, session)
		if !sessionRead && !ownerRead {
			return nil, bad
		}
		req = req.Clone(req.Context())
		req.Header.Set("Cache-Control", "no-store")
		resp, err := transport.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, bad
		}
		limit := 16 << 10
		if sessionRead {
			limit = 64 << 10
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
		if err != nil || len(b) > limit {
			return nil, bad
		}
		v, ok := mcpDecode(b)
		obj, _ := v.(map[string]any)
		if !ok || obj == nil {
			return nil, bad
		}
		if sessionRead {
			row := asMap(obj["session"])
			if obj["ok"] != true || row["id"] != session || row["project"] != pin.project || row["agent_id"] != pin.actor || row["status"] != "active" ||
				(pin.native != "" && row["native_session_id"] != pin.native) {
				return nil, bad
			}
			if stale, present := row["stale"]; present && stale != false {
				return nil, bad
			}
			sessionVerified = true
		} else {
			owner = obj
		}
		resp.Body = io.NopCloser(bytes.NewReader(b))
		return resp, nil
	})
	gateway := *c
	gateway.client = &client
	gateway.stdout, gateway.stderr = io.Discard, io.Discard
	start := time.Now()
	args := []string{"--raw", "--timeout", "2", "--project", pin.project, "--topic-path", pin.topic, "--decision-id", pin.id,
		"--agent-id", pin.actor, "--session-id", session}
	if pin.native != "" {
		args = append(args, "--native-session-id", pin.native)
	}
	if pin.scope != "" {
		args = append(args, "--scope", pin.scope)
	}
	// Invoke this edition's exact manual reader, never generic claim-query
	// parsing or a fallback to another edition's owner/credential contract.
	if err := gateway.cmdClaimQuery(args); err != nil {
		return
	}
	r := owner
	if !sessionVerified || len(r) != 11 || r["agent_id"] != pin.actor || r["session_id"] != session || r["ok"] != true || r["schema_id"] != namedDecisionSchema || r["project"] != pin.project || r["topic_path"] != pin.topic || r["decision_id"] != pin.id || r["scope"] != pin.scope {
		return
	}
	version := namedDecisionVersion(r["version"])
	stamp, _ := r["as_of"].(string)
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if version == 0 || err != nil || at.Before(start.Add(-5*time.Second)) || at.After(time.Now().Add(5*time.Second)) {
		return
	}
	d, _ := r["decision"].(map[string]any)
	switch d["state"] {
	case "unresolved_contradiction", "expired", "retracted", "pending":
		out["status"] = d["state"]
	case "active":
		if _, conflict := d["conflict_versions"]; conflict {
			return
		}
		current, _ := d["current"].(map[string]any)
		effective := namedDecisionVersion(current["version"])
		if effective == 0 || effective > version {
			return
		}
		out["current_version"] = effective
		out["status"] = "needs_source_check"
		// Never clip a negation or drop an applicable qualification. If the
		// whole decision does not fit, return only the address and versions.
		if text, ok := current["value"].(string); ok && strings.TrimSpace(text) != "" && len(text) <= 600 &&
			strings.IndexFunc(text, unicode.IsControl) < 0 && len(d) == 2 && len(current) == 2 {
			out["recorded_value"] = text
		}
	default:
		return
	}
	out["receipt_version"] = version
	out["as_of"] = stamp
	out["agent_id"], out["session_id"] = pin.actor, session
}

type decisionReadTransport func(*http.Request) (*http.Response, error)

// A legacy claim reader may ignore the exact selector. Check the request
// itself before delivery; never send a broad search as a compatibility probe.
func (pin *namedDecisionPin) exactOwnerQuery(req *http.Request, session string) bool {
	if req.GetBody == nil {
		return false
	}
	body, err := req.GetBody()
	if err != nil {
		return false
	}
	defer body.Close()
	raw, err := io.ReadAll(io.LimitReader(body, 2049))
	if err != nil || len(raw) > 2048 {
		return false
	}
	v, ok := mcpDecode(raw)
	q, _ := v.(map[string]any)
	s, _ := q["decision_context"].(map[string]any)
	scope, present := s["scope"]
	return ok && len(q) == 5 && q["agent_id"] == pin.actor && q["session_id"] == session && q["project"] == pin.project && q["topic_path"] == pin.topic && s["id"] == pin.id &&
		((pin.scope == "" && !present && len(s) == 1) || (present && scope == pin.scope && len(s) == 2))
}

func (f decisionReadTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func namedDecisionVersion(v any) int {
	n, ok := v.(json.Number)
	if !ok {
		return 0
	}
	i, err := n.Int64()
	if err != nil || i < 1 || i > 32 {
		return 0
	}
	return int(i)
}
