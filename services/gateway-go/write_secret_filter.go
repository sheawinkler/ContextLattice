package main

import (
	"errors"
	"os"
	"regexp"
	"strings"
)

const writeSecretRedaction = "[REDACTED]"

type writeSecretFilterResult struct {
	Mode          string
	Findings      int
	KeyFindings   int
	ValueFindings int
	Redactions    int
}

var errWriteSecretBlocked = errors.New("potential secret detected; storage blocked by SECRETS_STORAGE_MODE=block")

var writeSecretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|token|secret|password|passwd|private[_-]?key|access[_-]?key|client[_-]?secret)\s*[:=]\s*[^\s,;]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9\-._~+/]+=*`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9]{16,}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----.*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
}

func writeSecretsStorageMode() string {
	mode := strings.ToLower(strings.TrimSpace(firstNonEmptyStrings(
		os.Getenv("GO_WRITE_SECRETS_STORAGE_MODE"),
		os.Getenv("SECRETS_STORAGE_MODE"),
		"redact",
	)))
	switch mode {
	case "allow", "block", "redact":
		return mode
	default:
		return "redact"
	}
}

func writeSecretKey(key string) bool {
	normalized := strings.ToLower(strings.TrimSpace(key))
	normalized = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(normalized)
	normalized = strings.Trim(normalized, "_")
	if normalized == "" {
		return false
	}
	switch normalized {
	case "apikey", "api_key", "token", "secret", "password", "passwd", "credential", "credentials",
		"privatekey", "private_key", "authorization", "accesskey", "access_key", "clientsecret", "client_secret":
		return true
	}
	for _, suffix := range []string{
		"_api_key", "_token", "_secret", "_password", "_passwd", "_credential", "_credentials",
		"_private_key", "_access_key", "_authorization",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

func writeSecretKeyHasSensitiveValue(value any) bool {
	switch typed := value.(type) {
	case string:
		normalized := strings.TrimSpace(typed)
		return normalized != "" && !strings.EqualFold(normalized, writeSecretRedaction)
	case []any:
		return len(typed) > 0
	case []string:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return false
	}
}

func writeSecretValue(value string, result *writeSecretFilterResult) string {
	scrubbed := value
	for _, pattern := range writeSecretValuePatterns {
		matches := pattern.FindAllStringIndex(scrubbed, -1)
		if len(matches) == 0 {
			continue
		}
		result.Findings += len(matches)
		result.ValueFindings += len(matches)
		if result.Mode == "redact" {
			scrubbed = pattern.ReplaceAllString(scrubbed, writeSecretRedaction)
			result.Redactions += len(matches)
		}
	}
	return scrubbed
}

func scrubWriteSecrets(value any, result *writeSecretFilterResult, depth int) any {
	if depth > 32 {
		return value
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, nested := range typed {
			if writeSecretKey(key) && writeSecretKeyHasSensitiveValue(nested) {
				result.Findings++
				result.KeyFindings++
				if result.Mode == "redact" {
					out[key] = writeSecretRedaction
					result.Redactions++
				} else {
					out[key] = nested
				}
				continue
			}
			out[key] = scrubWriteSecrets(nested, result, depth+1)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, nested := range typed {
			out = append(out, scrubWriteSecrets(nested, result, depth+1))
		}
		return out
	case []string:
		out := make([]string, 0, len(typed))
		for _, nested := range typed {
			out = append(out, writeSecretValue(nested, result))
		}
		return out
	case string:
		return writeSecretValue(typed, result)
	default:
		return value
	}
}

func writeSecurityPayload(item normalizedWrite) map[string]any {
	if len(item.raw) > 0 {
		return cloneMap(item.raw)
	}
	payload := map[string]any{
		"projectName": item.project,
		"fileName":    item.fileName,
		"content":     item.content,
		"topicPath":   item.topicPath,
		"agent_id":    item.agentID,
		"session_id":  item.sessionID,
		"tags":        append([]string{}, item.tags...),
		"created_at":  item.createdAt,
		"lifecycle":   item.lifecycle,
	}
	return payload
}

func syncSecuredWriteFields(item normalizedWrite, secured map[string]any) normalizedWrite {
	item.raw = secured
	item.project = strings.TrimSpace(firstNonEmptyStrings(
		anyToString(secured["projectName"]),
		anyToString(secured["project"]),
		item.project,
	))
	item.fileName = strings.TrimSpace(firstNonEmptyStrings(
		anyToString(secured["fileName"]),
		anyToString(secured["file_name"]),
		anyToString(secured["file"]),
		item.fileName,
	))
	item.content = strings.TrimSpace(firstNonEmptyStrings(anyToString(secured["content"]), item.content))
	item.topicPath = strings.TrimSpace(firstNonEmptyStrings(
		anyToString(secured["topicPath"]),
		anyToString(secured["topic_path"]),
		item.topicPath,
	))
	meta := normalizeWriteMetadata(secured)
	item.agentID = firstNonEmptyStrings(meta.agentID, item.agentID)
	item.sessionID = firstNonEmptyStrings(meta.sessionID, item.sessionID)
	if len(meta.tags) > 0 {
		item.tags = meta.tags
	}
	item.createdAt = firstNonEmptyStrings(meta.createdAt, item.createdAt)
	item.lifecycle = firstNonEmptyStrings(meta.lifecycle, item.lifecycle)
	return item
}

func secureNormalizedWrite(item normalizedWrite) (normalizedWrite, writeSecretFilterResult, error) {
	result := writeSecretFilterResult{Mode: writeSecretsStorageMode()}
	raw := writeSecurityPayload(item)
	secured, _ := scrubWriteSecrets(raw, &result, 0).(map[string]any)
	if secured == nil {
		secured = map[string]any{}
	}
	if result.Mode == "block" && result.Findings > 0 {
		return item, result, errWriteSecretBlocked
	}
	return syncSecuredWriteFields(item, secured), result, nil
}

func mergeWriteSecretFilterResults(left writeSecretFilterResult, right writeSecretFilterResult) writeSecretFilterResult {
	if left.Mode == "" {
		left.Mode = right.Mode
	}
	left.Findings += right.Findings
	left.KeyFindings += right.KeyFindings
	left.ValueFindings += right.ValueFindings
	left.Redactions += right.Redactions
	return left
}

func writeSecretFilterPayload(result writeSecretFilterResult) map[string]any {
	return map[string]any{
		"mode":           result.Mode,
		"findings":       result.Findings,
		"key_findings":   result.KeyFindings,
		"value_findings": result.ValueFindings,
		"redactions":     result.Redactions,
	}
}

func attachWriteSecretFilter(payload map[string]any, result writeSecretFilterResult) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["secret_filter"] = writeSecretFilterPayload(result)
	if result.Redactions > 0 {
		warnings := parseWarnings(payload["warnings"])
		payload["warnings"] = dedupeWarnings(append(warnings, "potential secrets were redacted before storage"))
	}
	return payload
}

func (s *server) recordWriteSecretFilter(result writeSecretFilterResult, blocked bool) {
	if s == nil || result.Findings <= 0 {
		return
	}
	s.writeSecretFindings.Add(uint64(result.Findings))
	s.writeSecretRedactions.Add(uint64(result.Redactions))
	if blocked {
		s.writeSecretBlocked.Add(1)
	}
}
