package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func decisionFlagsPresent(args []string) bool {
	for _, arg := range args {
		key, _, _ := strings.Cut(arg, "=")
		switch key {
		case "--decision-id", "--effect", "--expect-version", "--target-version", "--scope":
			return true
		}
	}
	return false
}

// Exact selectors deliberately do not use adapter defaults, session files or
// a positional note. A malformed flag must not become feedback or claim text.
func (c *cli) cmdTemporalDecision(args []string, change bool) error {
	flags := map[string]string{"project": "project", "topic-path": "topic_path", "decision-id": "id",
		"scope": "scope", "session-id": "session_id", "agent-id": "agent_id", "native-session-id": "native_session_id", "base-url": "base_url", "timeout": "timeout"}
	if change {
		flags = mergeStringFlags(flags, map[string]string{"effect": "effect", "expect-version": "expected_version", "target-version": "target_version",
			"object": "value", "valid-from": "valid_from", "valid-to": "valid_to"})
	} else {
		flags["as-of"] = "as_of"
	}
	bools := commonBoolFlags()
	p := parseArgs(args, flags, bools)
	if p.bool("help") {
		return c.emitUsage("contextlattice correct --project P --topic-path T --agent-id A --session-id S --decision-id ID --effect set|replace|exception|refine|detail|contradict|retract|expire --expect-version N [--target-version V] [--scope LABEL] [--object VALUE] [--valid-from RFC3339] [--valid-to RFC3339]\ncontextlattice claim-query --project P --topic-path T --agent-id A --session-id S --decision-id ID [--scope LABEL] [--as-of RFC3339]")
	}
	if len(p.pos) != 0 {
		return errors.New("decision commands require named options only; do not mix decision and feedback options")
	}
	seen := map[string]bool{}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		key, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if seen[key] {
			return errors.New("duplicate decision option")
		}
		seen[key] = true
	}
	project, topic, id := p.string("project", ""), p.string("topic_path", ""), p.string("id", "")
	session, actor := p.string("session_id", ""), p.string("agent_id", "")
	if project == "" || topic == "" || id == "" || session == "" || actor == "" {
		return errors.New("an explicit project, topic, decision ID, actor and active session are required")
	}
	intent := map[string]any{"id": id}
	for _, name := range []string{"scope", "effect", "value", "valid_from", "valid_to", "as_of"} {
		if value, present := p.values[name]; present {
			if value == "" {
				return fmt.Errorf("missing value for %s", name)
			}
			intent[name] = value
		}
	}
	for _, name := range []string{"expected_version", "target_version"} {
		if raw, ok := p.values[name]; ok {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || strconv.Itoa(n) != raw {
				return fmt.Errorf("invalid %s", name)
			}
			intent[name] = n
		}
	}
	path, selector, schema := "/memory/claims/query", "decision_context", "temporal_decision_context.v1"
	if change {
		if intent["effect"] == nil || intent["expected_version"] == nil {
			return errors.New("--effect and --expect-version are required")
		}
		path, selector, schema = "/memory/claims", "decision_change", "temporal_decision_receipt.v1"
	}
	local := *c
	local.applyBaseURL(p)
	u, err := url.Parse(local.baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		(u.Hostname() != "localhost" && !net.ParseIP(u.Hostname()).IsLoopback()) {
		return errors.New("manual decisions require a local gateway")
	}
	client := *local.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	local.client = &client
	if err := local.checkCallerSession(project, session, resolveNativeCaller(p, nil), p.float("timeout", 10)); err != nil {
		return err
	}
	out, _, err := local.requestJSON(context.Background(), http.MethodPost, path,
		map[string]any{"project": project, "topic_path": topic, "session_id": session, "agent_id": actor, selector: intent}, p.float("timeout", 10))
	if err != nil {
		return err
	}
	// Older gateways ignore unknown read selectors. Never print their broad
	// claim output as the answer to an exact decision request.
	if out["ok"] != true || out["schema_id"] != schema || out["project"] != project || out["session_id"] != session || out["agent_id"] != actor ||
		out["topic_path"] != topic || out["decision_id"] != id || asInt(out["version"]) < 1 ||
		(change && (out["recorded"] != true || out["effect"] != intent["effect"] || asInt(out["version"]) != intent["expected_version"].(int)+1)) ||
		(!change && (out["scope"] != p.string("scope", "") || firstString(asMap(out["decision"])["state"]) == "")) {
		return errors.New("gateway did not confirm the scoped decision")
	}
	return c.emit(out, !p.bool("raw"))
}
