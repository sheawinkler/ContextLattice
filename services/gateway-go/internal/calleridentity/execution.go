package calleridentity

import (
	"regexp"
	"strings"
)

var (
	part         = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)
	harnessPart  = regexp.MustCompile(`^[a-z][a-z0-9]{0,23}(-[a-z0-9]{1,24}){0,2}$`)
	versionPart  = regexp.MustCompile(`^[0-9]{1,4}(\.[0-9]{1,4}){0,2}$`)
	snapshot     = regexp.MustCompile(`-[0-9]{8}$`)
	versionFirst = regexp.MustCompile(`^([a-z][a-z0-9]*)-([0-9]{1,4}(?:[.-][0-9]{1,4}){0,2})(?:-([a-z][a-z0-9]*))?$`)
	familyFirst  = regexp.MustCompile(`^([a-z][a-z0-9]*)-([a-z][a-z0-9]*)-([0-9]{1,4}(?:[.-][0-9]{1,4}){0,2})$`)
)

// Facts describe an invocation, never a principal or a verified signature.
type Facts struct {
	Harness, Model, Origin, Family, Version, Effort, Observation string
}

func token(raw string, pattern *regexp.Regexp) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if Model(s) == "" || !pattern.MatchString(s) {
		return ""
	}
	return s
}

func Harness(raw string) string {
	switch raw = token(raw, harnessPart); raw {
	case "hermes-agent":
		return "hermes"
	case "agent-cli", "compact-hook":
		return ""
	default:
		return raw
	}
}

func reasoningEffort(raw string) string {
	switch s := strings.ToLower(strings.TrimSpace(raw)); s {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return s
	}
	return ""
}

func modelParts(raw string) (string, string, string) {
	s := strings.ToLower(Model(raw))
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	s = snapshot.ReplaceAllString(s, "")
	if m := familyFirst.FindStringSubmatch(s); m != nil {
		return token(m[1], part), token(m[2], part), strings.ReplaceAll(m[3], "-", ".")
	}
	if m := versionFirst.FindStringSubmatch(s); m != nil {
		return token(m[1], part), token(m[3], part), strings.ReplaceAll(m[2], "-", ".")
	}
	return "", "", ""
}

func mergeFact(inferred, reported string, pattern *regexp.Regexp) (string, bool) {
	if reported == "" {
		return inferred, true
	}
	v := token(reported, pattern)
	return v, v != "" && (inferred == "" || v == inferred)
}

// Metadata derives the label on both sides of the transport. A supplied
// display label is never trusted; partial observations replace old ones.
func (f Facts) Metadata(agentID string) map[string]any {
	out := Metadata(agentID, f.Model)
	origin, family, version := modelParts(f.Model)
	origin, o := mergeFact(origin, f.Origin, part)
	family, a := mergeFact(family, f.Family, part)
	version, v := mergeFact(version, f.Version, versionPart)
	h, e := Harness(f.Harness), reasoningEffort(f.Effort)
	out["execution_status"] = "partial"
	if !o || !a || !v {
		out["execution_status"] = "conflicting"
		return out
	}
	for k, val := range map[string]string{"harness": h, "model_origin": origin, "model_family": family, "model_version": version, "reasoning_effort": e} {
		if val != "" {
			out[k] = val
		}
	}
	switch f.Observation {
	case "invocation", "codex_thread_index", "host_event", "session_start":
		out["observation"] = f.Observation
	}
	if h != "" && origin != "" && family != "" && version != "" && e != "" {
		out["execution_status"] = "complete"
		out["execution_label"] = strings.Join([]string{h, origin, family, version, e}, "-")
	}
	return out
}
