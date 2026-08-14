package main

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var (
	adapterPublicSHA256Pattern      = regexp.MustCompile(`^(?:sha256:)?[0-9a-f]{40,64}$`)
	adapterPublicOpaqueRefPattern   = regexp.MustCompile(`^(?:artifact|authorization|cleanup|container|orphan|publication|receipt|result|snapshot|workspace)-[A-Za-z0-9._:@-]{1,255}$`)
	adapterPublicSecretValuePattern = regexp.MustCompile(`(?i)(?:\b(?:bearer|basic)\s+\S+|(?:^|[^A-Za-z0-9])(?:sk|pk|rk)-[A-Za-z0-9_-]{4,}|https?://[^\s/:@]+:[^\s/@]+@|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password|secret)\s*[:=])`)
	adapterPublicOpaqueValuePattern = regexp.MustCompile(`(?:^|[^A-Za-z0-9])[A-Za-z0-9][A-Za-z0-9_-]{47,}(?:$|[^A-Za-z0-9])`)
	adapterPublicPEMPattern         = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*(?:PRIVATE KEY|OPENSSH PRIVATE KEY|CERTIFICATE)-----.*?-----END [A-Z0-9 ]*(?:PRIVATE KEY|OPENSSH PRIVATE KEY|CERTIFICATE)-----`)
	adapterPublicHeaderPattern      = regexp.MustCompile(`(?im)^\s*(?:authorization|proxy-authorization|x-api-key|cookie|set-cookie)\s*:`)
	adapterPublicQueryPattern       = regexp.MustCompile(`(?i)[?&](?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password|secret|session(?:[_-]?id)?)=`)
	adapterPublicWindowsPathPattern = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])(?:[A-Z]:[\\/]|\\\\)[^\r\n"'<>]*`)
	adapterPublicUnixPathPattern    = regexp.MustCompile(`(?:^|[^A-Za-z0-9/])/[A-Za-z0-9._~+-][^\r\n"'<>]*`)
)

func adapterContractDefinition(contractID string) map[string]any {
	generated := map[string]any{}
	if json.Unmarshal([]byte(generatedAdapterPublicContractsJSON), &generated) == nil {
		if contract := asMap(generated[contractID]); len(contract) > 0 {
			return contract
		}
	}
	paths := []string{}
	if explicit := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_AGENT_CONTRACTS_PATH")); explicit != "" {
		paths = append(paths, explicit)
	}
	paths = append(paths,
		filepath.Join(repoRoot(), "config", "agent_contracts", "agent_output_contracts.json"),
		filepath.Join("config", "agent_contracts", "agent_output_contracts.json"),
		filepath.Join("..", "config", "agent_contracts", "agent_output_contracts.json"),
		filepath.Join("..", "..", "config", "agent_contracts", "agent_output_contracts.json"),
		filepath.Join("..", "..", "..", "config", "agent_contracts", "agent_output_contracts.json"),
		filepath.Join("..", "..", "..", "..", "config", "agent_contracts", "agent_output_contracts.json"),
	)
	if executable, err := os.Executable(); err == nil {
		for directory := filepath.Dir(executable); directory != filepath.Dir(directory); directory = filepath.Dir(directory) {
			paths = append(paths, filepath.Join(directory, "config", "agent_contracts", "agent_output_contracts.json"))
		}
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		registry := map[string]any{}
		if json.Unmarshal(raw, &registry) != nil {
			continue
		}
		return asMap(asMap(registry["contracts"])[contractID])
	}
	return nil
}

func adapterContractFindings(contractID string, payload map[string]any) []string {
	contract := adapterContractDefinition(contractID)
	if len(contract) == 0 {
		return []string{"missing_contract"}
	}
	if !adapterJSONDomainValid(payload, 0) {
		return []string{"payload_json_domain_invalid"}
	}
	findings := []string{}
	if allowed := adapterStringList(contract["allowed_fields"]); len(allowed) > 0 {
		allowedSet := map[string]bool{}
		for _, field := range allowed {
			allowedSet[field] = true
		}
		for field := range payload {
			if !allowedSet[field] {
				findings = append(findings, "unexpected_field:"+field)
			}
		}
	}
	for _, field := range adapterStringList(contract["required_fields"]) {
		if _, exists := payload[field]; !exists {
			findings = append(findings, "missing_required_field:"+field)
		}
	}
	for path, rawExpected := range asMap(contract["field_types"]) {
		value, exists := adapterDottedPathGet(payload, path)
		if exists && !adapterContractTypeMatches(value, firstString(rawExpected)) {
			findings = append(findings, "field_type_mismatch:"+path)
		}
	}
	for path, rawFields := range asMap(contract["required_fields_by_path"]) {
		value, exists := adapterDottedPathGet(payload, path)
		target, objectOK := value.(map[string]any)
		if !exists || !objectOK {
			findings = append(findings, "missing_required_object:"+path)
			continue
		}
		for _, field := range adapterStringList(rawFields) {
			if _, exists := target[field]; !exists {
				findings = append(findings, "missing_required_nested_field:"+path+"."+field)
			}
		}
	}
	for path, rawNeedle := range asMap(contract["required_string_contains"]) {
		value, _ := adapterDottedPathGet(payload, path)
		if !strings.Contains(firstString(value), firstString(rawNeedle)) {
			findings = append(findings, "required_string_missing:"+path)
		}
	}
	for _, path := range adapterStringList(contract["required_true_paths"]) {
		value, exists := adapterDottedPathGet(payload, path)
		if !exists || value != true {
			findings = append(findings, "required_true_missing:"+path)
		}
	}
	for _, path := range adapterStringList(contract["required_false_paths"]) {
		value, exists := adapterDottedPathGet(payload, path)
		if !exists || value != false {
			findings = append(findings, "required_false_missing:"+path)
		}
	}
	maximumTotal := asInt(contract["max_total_json_bytes"])
	maximumString := asInt(contract["max_string_bytes"])
	maximumList := asInt(contract["max_list_items"])
	if raw, err := json.Marshal(payload); err != nil || maximumTotal < 1 || len(raw) > maximumTotal {
		findings = append(findings, "json_bytes_exceed_contract")
	}
	adapterWalkContractLimits(payload, maximumString, maximumList, "", &findings)
	for path, rawMaximum := range asMap(contract["max_bytes_by_path"]) {
		value, exists := adapterDottedPathGet(payload, path)
		text, textOK := value.(string)
		maximum := asInt(rawMaximum)
		if exists && textOK && maximum > 0 && len([]byte(text)) > maximum {
			findings = append(findings, "string_bytes_exceed_contract:"+path)
		}
	}
	forbidden := map[string]bool{}
	for _, field := range adapterStringList(contract["forbidden_fields"]) {
		forbidden[adapterCanonicalFieldKey(field)] = true
	}
	if len(forbidden) > 0 {
		if firstString(contract["forbidden_scope"]) == "root" {
			for field := range payload {
				if forbidden[adapterCanonicalFieldKey(field)] {
					findings = append(findings, "forbidden_field_present:"+field)
				}
			}
		} else {
			adapterWalkForbiddenFields(payload, forbidden, "", &findings)
		}
	}
	if contractID == generatedUniversalAgentAdapterResponseContractID || contractID == generatedLifecycleReceiptContractID {
		identityFields := []string{"session_id", "agent_id"}
		if contractID == generatedLifecycleReceiptContractID {
			identityFields = []string{"session_id"}
		}
		allowedIdentityField := map[string]bool{}
		for _, field := range identityFields {
			allowedIdentityField[field] = true
		}
		omitted := map[string]bool{}
		for _, item := range asList(payload["identity_omitted"]) {
			if marker, ok := item.(string); ok && allowedIdentityField[marker] {
				omitted[marker] = true
			} else {
				findings = append(findings, "identity_omission_marker_invalid")
			}
		}
		for _, field := range identityFields {
			value, typed := payload[field].(string)
			present := typed && strings.TrimSpace(value) != ""
			if present == omitted[field] {
				findings = append(findings, "identity_evidence_invalid:"+field)
			}
		}
	}
	return findings
}

func adapterStringList(value any) []string {
	items := asList(value)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func adapterDottedPathGet(payload map[string]any, path string) (any, bool) {
	var current any = payload
	for _, part := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func adapterContractTypeMatches(value any, expected string) bool {
	switch expected {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "list":
		_, ok := value.([]any)
		return ok
	case "list[string]":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(string); !ok {
				return false
			}
		}
		return true
	case "list[object]":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		for _, item := range items {
			if _, ok := item.(map[string]any); !ok {
				return false
			}
		}
		return true
	case "string":
		_, ok := value.(string)
		return ok
	case "bool":
		_, ok := value.(bool)
		return ok
	case "int":
		return adapterJSONInteger(value)
	case "number":
		return adapterJSONNumber(value)
	default:
		return false
	}
}

func adapterJSONInteger(value any) bool {
	switch typed := value.(type) {
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32:
		return true
	case uint64:
		return typed <= math.MaxInt64
	case json.Number:
		text := typed.String()
		if strings.ContainsAny(text, ".eE") {
			return false
		}
		_, err := strconv.ParseInt(text, 10, 64)
		return err == nil
	default:
		return false
	}
}

func adapterJSONNumber(value any) bool {
	if adapterJSONInteger(value) {
		return true
	}
	switch typed := value.(type) {
	case float32:
		return !math.IsNaN(float64(typed)) && !math.IsInf(float64(typed), 0)
	case float64:
		return !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case json.Number:
		return adapterJSONNumberLexemeValid(typed)
	default:
		return false
	}
}

func adapterJSONNumberLexemeValid(value json.Number) bool {
	text := value.String()
	if text == "" || strings.TrimSpace(text) != text || !json.Valid([]byte(text)) {
		return false
	}
	if !strings.ContainsAny(text, ".eE") {
		_, err := strconv.ParseInt(text, 10, 64)
		return err == nil
	}
	parsed, err := strconv.ParseFloat(text, 64)
	return (err == nil || (errors.Is(err, strconv.ErrRange) && parsed == 0)) &&
		!math.IsNaN(parsed) && !math.IsInf(parsed, 0)
}

func adapterWalkContractLimits(value any, maximumString, maximumList int, path string, findings *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for field, child := range typed {
			next := field
			if path != "" {
				next = path + "." + field
			}
			adapterWalkContractLimits(child, maximumString, maximumList, next, findings)
		}
	case []any:
		if maximumList > 0 && len(typed) > maximumList {
			*findings = append(*findings, "list_items_exceed_contract:"+path)
		}
		for index, child := range typed {
			adapterWalkContractLimits(child, maximumString, maximumList, path+"["+strconv.Itoa(index)+"]", findings)
		}
	case string:
		if maximumString > 0 && len([]byte(typed)) > maximumString {
			*findings = append(*findings, "string_bytes_exceed_contract:"+path)
		}
	}
}

func adapterCanonicalFieldKey(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(value) {
		if unicode.IsLetter(char) || unicode.IsDigit(char) {
			builder.WriteRune(char)
		}
	}
	return builder.String()
}

func adapterWalkForbiddenFields(value any, forbidden map[string]bool, path string, findings *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for field, child := range typed {
			next := field
			if path != "" {
				next = path + "." + field
			}
			if forbidden[adapterCanonicalFieldKey(field)] {
				*findings = append(*findings, "forbidden_field_present:"+next)
			}
			adapterWalkForbiddenFields(child, forbidden, next, findings)
		}
	case []any:
		for index, child := range typed {
			adapterWalkForbiddenFields(child, forbidden, path+"["+strconv.Itoa(index)+"]", findings)
		}
	}
}

func adapterJSONDomainValid(value any, depth int) bool {
	if depth > 64 || value == nil {
		return depth <= 64
	}
	if number, ok := value.(json.Number); ok {
		return adapterJSONNumberLexemeValid(number)
	}
	reflected := reflect.ValueOf(value)
	for reflected.Kind() == reflect.Interface {
		if reflected.IsNil() {
			return true
		}
		reflected = reflected.Elem()
	}
	switch reflected.Kind() {
	case reflect.Bool:
		return true
	case reflect.String:
		return utf8.ValidString(reflected.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflected.Uint() <= math.MaxInt64
	case reflect.Float32, reflect.Float64:
		number := reflected.Float()
		return !math.IsNaN(number) && !math.IsInf(number, 0)
	case reflect.Map:
		if reflected.Type().Key().Kind() != reflect.String {
			return false
		}
		for _, key := range reflected.MapKeys() {
			if !utf8.ValidString(key.String()) || !adapterJSONDomainValid(reflected.MapIndex(key).Interface(), depth+1) {
				return false
			}
		}
		return true
	case reflect.Slice, reflect.Array:
		for index := 0; index < reflected.Len(); index++ {
			if !adapterJSONDomainValid(reflected.Index(index).Interface(), depth+1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func adapterSensitivePublicKey(key string) bool {
	normalized := adapterCanonicalFieldKey(key)
	exact := map[string]bool{
		"api": true, "auth": true, "authorization": true, "bearer": true, "cookie": true,
		"credential": true, "credentials": true, "key": true, "password": true, "passwd": true,
		"pem": true, "proxyauthorization": true, "pwd": true, "secret": true, "session": true,
		"sessionid": true, "setcookie": true, "sig": true, "signature": true, "token": true, "userinfo": true,
	}
	if exact[normalized] {
		return true
	}
	for _, suffix := range []string{"api", "accesstoken", "auth", "authdata", "authheader", "authvalue", "apikey", "apitoken", "apisecret", "authtoken", "clientsecret", "credential", "credentials", "password", "passwd", "privatekey", "refreshtoken", "secretkey", "sessionid", "token", "userinfo"} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	for _, fragment := range []string{"apikey", "apitoken", "apisecret", "authorization", "clientsecret", "cookie", "credential", "key", "password", "passwd", "pem", "privatekey", "refreshtoken", "secret", "secretkey", "session", "sig", "userinfo"} {
		if strings.Contains(normalized, fragment) {
			return true
		}
	}
	return false
}

func adapterPublicMapKeySafe(key string) bool {
	if key == "" || len([]byte(key)) > 128 || !utf8.ValidString(key) {
		return false
	}
	if adapterPublicSecretValuePattern.MatchString(key) || adapterPublicOpaqueValuePattern.MatchString(key) ||
		adapterPublicPEMPattern.MatchString(key) || adapterPublicHeaderPattern.MatchString(key) ||
		adapterPublicQueryPattern.MatchString(key) || adapterPublicWindowsPathPattern.MatchString(key) ||
		adapterPublicUnixPathPattern.MatchString(key) || strings.Contains(strings.ToLower(key), "file://") {
		return false
	}
	for _, char := range key {
		if unicode.IsLetter(char) || unicode.IsDigit(char) || strings.ContainsRune("._:@-[]", char) {
			continue
		}
		return false
	}
	return true
}

func adapterPublicReferenceValue(key string, value any) bool {
	normalized := adapterCanonicalFieldKey(key)
	publicReferenceKeys := map[string]bool{
		"artifactdigest": true, "artifactdigests": true, "authorizationdigest": true,
		"authorizationid": true, "basesha": true, "canonicaldigest": true, "cleanupid": true,
		"containerref": true, "contentdigest": true, "contextpackhash": true, "diffdigest": true,
		"digest": true, "evidencedigest": true, "finalhead": true, "finaltree": true,
		"orphanref": true, "publicationid": true, "receiptdigest": true, "receipthash": true,
		"receiptid": true, "recorddigest": true, "redactionreceipt": true, "resultid": true,
		"sessionid": true, "snapshotid": true, "verifiedtree": true, "workspaceref": true,
	}
	if !publicReferenceKeys[normalized] {
		return false
	}
	if normalized == "artifactdigests" {
		items, ok := value.([]any)
		if !ok || len(items) > 64 {
			return false
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok || !adapterPublicSHA256Pattern.MatchString(strings.TrimSpace(text)) {
				return false
			}
		}
		return true
	}
	text, ok := value.(string)
	if !ok {
		return false
	}
	if strings.HasSuffix(normalized, "digest") || strings.HasSuffix(normalized, "hash") ||
		normalized == "redactionreceipt" || normalized == "basesha" || normalized == "finalhead" ||
		normalized == "finaltree" || normalized == "verifiedtree" {
		return adapterPublicSHA256Pattern.MatchString(strings.TrimSpace(text))
	}
	prefixes := map[string]string{
		"authorizationid": "authorization-", "cleanupid": "cleanup-", "containerref": "container-",
		"orphanref": "orphan-", "publicationid": "publication-", "resultid": "result-",
		"sessionid": "session-", "snapshotid": "snapshot-", "workspaceref": "workspace-",
	}
	if normalized == "receiptid" {
		trimmed := strings.TrimSpace(text)
		return (strings.HasPrefix(trimmed, "receipt-") || strings.HasPrefix(trimmed, "cleanup-receipt-")) &&
			adapterPublicOpaqueRefPattern.MatchString(trimmed)
	}
	prefix := prefixes[normalized]
	return prefix != "" && strings.HasPrefix(strings.TrimSpace(text), prefix) && adapterPublicOpaqueRefPattern.MatchString(strings.TrimSpace(text))
}

func adapterPublicString(value string) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	candidates := []string{value}
	seen := map[string]bool{value: true}
	for depth := 0; depth < 3; depth++ {
		start := len(candidates)
		for _, candidate := range candidates[:start] {
			for _, decode := range []func(string) (string, error){url.PathUnescape, url.QueryUnescape} {
				decoded, err := decode(candidate)
				if err == nil && decoded != candidate && utf8.ValidString(decoded) && !seen[decoded] {
					seen[decoded] = true
					candidates = append(candidates, decoded)
				}
			}
		}
	}
	for _, candidate := range candidates {
		lower := strings.ToLower(candidate)
		if adapterPublicSecretValuePattern.MatchString(candidate) || adapterPublicOpaqueValuePattern.MatchString(candidate) ||
			adapterPublicPEMPattern.MatchString(candidate) || adapterPublicHeaderPattern.MatchString(candidate) ||
			adapterPublicQueryPattern.MatchString(candidate) {
			return "[REDACTED]", true
		}
		if frontierT7LooksLikeLocalPortableContinuationPath(candidate) || strings.Contains(lower, "file://") ||
			adapterPublicWindowsPathPattern.MatchString(candidate) || adapterPublicUnixPathPattern.MatchString(candidate) {
			return "[REDACTED_PATH]", true
		}
	}
	return value, true
}

func adapterRegisteredFormatContract(contractID string) map[string]any {
	contract := adapterContractDefinition(contractID)
	return map[string]any{
		"registry_id":          generatedAgentContractRegistryID,
		"registry_version":     generatedAgentContractRegistryVersion,
		"schema_id":            contractID,
		"contract_version":     asInt(contract["contract_version"]),
		"required_output_mode": firstString(contract["required_output_mode"]),
		"validator":            "contextlattice.boundary.v1",
		"forbidden_fields":     adapterStringList(contract["forbidden_fields"]),
		"contract_valid":       true,
		"truncated":            false,
		"omitted_counts":       map[string]any{},
		"actual_json_bytes":    0,
		"max_total_json_bytes": asInt(contract["max_total_json_bytes"]),
		"max_string_bytes":     asInt(contract["max_string_bytes"]),
		"max_list_items":       asInt(contract["max_list_items"]),
		"validation":           map[string]any{"status": "passed", "errors": []any{}},
	}
}

func adapterStampRegisteredEnvelope(contractID string, payload map[string]any) (map[string]any, bool) {
	if len(adapterContractDefinition(contractID)) == 0 || !adapterJSONDomainValid(payload, 0) {
		return nil, false
	}
	payload["format_contract"] = adapterRegisteredFormatContract(contractID)
	formatContract := asMap(payload["format_contract"])
	for attempts := 0; attempts < 12; attempts++ {
		if len(adapterContractFindings(contractID, payload)) != 0 {
			return nil, false
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, false
		}
		if asInt(formatContract["actual_json_bytes"]) == len(encoded) {
			return payload, adapterRegisteredEnvelopeAttestationValid(contractID, payload)
		}
		formatContract["actual_json_bytes"] = len(encoded)
	}
	return nil, false
}

func adapterPublicStringList(value any, limit, itemBytes int) []any {
	items := asList(value)
	if len(items) > limit {
		items = items[:limit]
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			continue
		}
		text = adapterCompactPublicString(text, itemBytes)
		if text != "" {
			result = append(result, text)
		}
	}
	return result
}

func adapterPublicNonnegativeInt(value any) int {
	parsed, ok := adapterExactJSONInt(value)
	if !ok || parsed < 0 {
		return 0
	}
	return parsed
}

func adapterPublicRunAdvisorSignals(value any, section string) map[string]any {
	signals := asMap(value)
	result := map[string]any{}
	objectiveBools := map[string]bool{
		"mission_present": true, "objective_present": true, "goal_present": true,
		"project_primary_objective_present": true, "topic_objective_present": true,
		"session_objective_present": true,
	}
	objectiveCounts := map[string]bool{"subobjective_count": true, "query_token_count": true, "context_token_count": true}
	graphCounts := map[string]bool{
		"edge_samples": true, "seed_count": true, "candidate_count": true,
		"added_evidence_count": true, "hydration_failure_count": true,
		"graph_touches": true, "handoffs": true, "checkpoints": true,
	}
	for name, raw := range signals {
		if section == "objective_coherence" && objectiveBools[name] {
			if value, ok := raw.(bool); ok {
				result[name] = value
			}
			continue
		}
		if section == "objective_coherence" && objectiveCounts[name] {
			if value, ok := adapterExactJSONInt(raw); ok && value >= 0 && value <= math.MaxInt32 {
				result[name] = value
			}
			continue
		}
		if section == "objective_coherence" && name == "shared_terms" {
			result[name] = adapterPublicStringList(raw, 12, 160)
			continue
		}
		if section == "graph_quality" && graphCounts[name] {
			if value, ok := adapterExactJSONInt(raw); ok && value >= 0 && value <= math.MaxInt32 {
				result[name] = value
			}
			continue
		}
		if section == "graph_quality" && name == "relations" {
			relations := map[string]any{}
			for relation, rawCount := range asMap(raw) {
				publicRelation := adapterCompactPublicString(relation, 120)
				count, countOK := adapterExactJSONInt(rawCount)
				if publicRelation != "" && countOK && count >= 0 && count <= math.MaxInt32 && len(relations) < 32 {
					relations[publicRelation] = count
				}
			}
			result[name] = relations
		}
	}
	return result
}

func adapterPublicRunAdvisorVisibility(value any) map[string]any {
	visibility := asMap(value)
	result := map[string]any{}
	for _, field := range []string{"best_surface", "watch_command", "poll_command", "session_event_type"} {
		if text := adapterCompactPublicString(firstString(visibility[field]), 500); text != "" {
			result[field] = text
		}
	}
	return result
}

func adapterPublicContinuationRoute(value any) string {
	text, ok := value.(string)
	text = strings.TrimSpace(text)
	const prefix = "/memory/search/continuations/"
	if !ok || len([]byte(text)) > 240 || !utf8.ValidString(text) || !strings.HasPrefix(text, prefix) ||
		strings.Contains(text, "..") || strings.ContainsAny(text, "\\%?#") {
		return ""
	}
	tail := strings.TrimPrefix(text, prefix)
	parts := strings.Split(tail, "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" || len([]byte(parts[0])) > 160 || (len(parts) == 2 && parts[1] != "events") {
		return ""
	}
	for _, char := range parts[0] {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._:@-", char) {
			continue
		}
		return ""
	}
	return text
}

func adapterPublicContextPackOutcomeReport(value map[string]any, sessionID string) (map[string]any, bool) {
	if firstString(value["schema_id"]) != adapterContextPackOutcomeSchemaID ||
		firstString(value["endpoint"]) != adapterContextPackOutcomeRoute {
		return nil, false
	}
	sampleID, sampleOK := value["sample_id"].(string)
	publicSessionID, sessionOK := adapterPublicIdentity(sessionID, true)
	if !sampleOK || !adapterPublicQualitySampleID(sampleID) || !sessionOK {
		return nil, false
	}
	return contextPackOutcomeReport(publicSessionID, strings.TrimSpace(sampleID)), true
}

func adapterPublicRunAdvisorModeledProgress(value any) map[string]any {
	progress := asMap(value)
	result := map[string]any{}
	if probabilistic, ok := progress["probabilistic"].(bool); ok {
		result["probabilistic"] = probabilistic
	}
	for _, field := range []string{"progress", "progress_pct", "eta_secs", "confidence", "elapsed_secs"} {
		if number, ok := adapterFiniteOutcomeNumber(progress[field]); ok && number >= 0 && number <= 1_000_000_000 {
			result[field] = number
		}
	}
	if band := adapterCompactPublicString(firstString(progress["confidence_band"]), 40); band != "" {
		result["confidence_band"] = band
	}
	result["pending_sources"] = adapterPublicStringList(progress["pending_sources"], 16, 160)
	estimates := map[string]any{}
	for source, raw := range asMap(progress["estimated_by_source_secs"]) {
		name := adapterCompactPublicString(source, 120)
		number, ok := adapterFiniteOutcomeNumber(raw)
		if name != "" && ok && number >= 0 && number <= 1_000_000_000 && len(estimates) < 16 {
			estimates[name] = number
		}
	}
	if len(estimates) > 0 {
		result["estimated_by_source_secs"] = estimates
	}
	return result
}

func adapterPublicRunAdvisorSourceSummary(value any) map[string]any {
	summary := asMap(value)
	result := map[string]any{}
	for _, field := range []string{"sources", "returned_now", "pending_sources", "warming_sources", "timed_out_sources", "failed_sources", "budget_exceeded_sources", "skipped_sources"} {
		result[field] = adapterPublicStringList(summary[field], 16, 160)
	}
	return result
}

func adapterPublicRunAdvisorRetrievalLifecycle(value any) map[string]any {
	lifecycle := asMap(value)
	result := map[string]any{}
	for _, field := range []string{"status", "result_state"} {
		if text := adapterCompactPublicString(firstString(lifecycle[field]), 80); text != "" {
			result[field] = text
		}
	}
	for _, field := range []string{"returned", "pending", "warming", "failed", "timed_out", "budget_exceeded"} {
		result[field] = adapterPublicStringList(lifecycle[field], 16, 160)
	}
	return result
}

func adapterPublicRunAdvisorRetrievalProgress(value any) map[string]any {
	progress := asMap(value)
	result := map[string]any{}
	for _, field := range []string{"status", "result_state", "created_at", "updated_at", "completed_at"} {
		if text := adapterCompactPublicString(firstString(progress[field]), 240); text != "" {
			result[field] = text
		}
	}
	for _, field := range []string{"poll_url", "events_url"} {
		if route := adapterPublicContinuationRoute(progress[field]); route != "" {
			result[field] = route
		}
	}
	if modeled := adapterPublicRunAdvisorModeledProgress(progress["modeled_progress"]); len(modeled) > 0 {
		result["modeled_progress"] = modeled
	}
	if summary := adapterPublicRunAdvisorSourceSummary(progress["source_summary"]); len(summary) > 0 {
		result["source_summary"] = summary
	}
	if lifecycle := adapterPublicRunAdvisorRetrievalLifecycle(progress["retrieval_lifecycle"]); len(lifecycle) > 0 {
		result["retrieval_lifecycle"] = lifecycle
	}
	if visibility := adapterPublicRunAdvisorVisibility(progress["agent_visibility"]); len(visibility) > 0 {
		result["agent_visibility"] = visibility
	}
	return result
}

func adapterPublicRunAdvisorNextActions(value any) []any {
	items := asList(value)
	if len(items) > 5 {
		items = items[:5]
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			if text = adapterCompactPublicString(text, 500); text != "" {
				result = append(result, text)
			}
			continue
		}
		action := asMap(item)
		projected := map[string]any{}
		for _, field := range []string{"label", "command", "reason"} {
			limit := 500
			if field == "label" {
				limit = 120
			}
			if text := adapterCompactPublicString(firstString(action[field]), limit); text != "" {
				projected[field] = text
			}
		}
		if firstString(projected["label"]) != "" && firstString(projected["command"]) != "" {
			result = append(result, projected)
		}
	}
	return result
}

func adapterPublicRunAdvisor(value any) (map[string]any, bool) {
	advisor := asMap(value)
	promptQuality := asMap(advisor["prompt_quality"])
	retrieval := asMap(advisor["retrieval_advice"])
	continuation := asMap(advisor["continuation"])
	objective := asMap(advisor["objective_coherence"])
	graph := asMap(advisor["graph_quality"])
	advisorOK := true
	if rawOK, exists := advisor["ok"]; exists {
		advisorOK = rawOK == true
	}
	public := map[string]any{
		"ok":        advisorOK,
		"schema_id": "run_advisor.v1",
		"posture":   firstNonEmpty(adapterCompactPublicString(firstString(advisor["posture"]), 80), "needs_retrieval"),
		"prompt_quality": map[string]any{
			"score":                  adapterPublicNonnegativeInt(promptQuality["score"]),
			"state":                  firstNonEmpty(adapterCompactPublicString(firstString(promptQuality["state"]), 80), "needs_context"),
			"ranked_evidence_count":  adapterPublicNonnegativeInt(promptQuality["ranked_evidence_count"]),
			"reference_prompt_chars": adapterPublicNonnegativeInt(promptQuality["reference_prompt_chars"]),
			"returned_source_count":  adapterPublicNonnegativeInt(promptQuality["returned_source_count"]),
			"complete":               promptQuality["complete"] == true,
			"missing":                adapterPublicStringList(promptQuality["missing"], 8, 240),
		},
		"retrieval_advice": map[string]any{
			"recommended_mode":     firstNonEmpty(adapterCompactPublicString(firstString(retrieval["recommended_mode"]), 80), "balanced"),
			"recommended_surface":  firstNonEmpty(adapterCompactPublicString(firstString(retrieval["recommended_surface"]), 120), "cli_for_local_agents"),
			"rationale":            adapterPublicStringList(retrieval["rationale"], 8, 240),
			"blocking_recommended": retrieval["blocking_recommended"] == true,
		},
		"continuation": map[string]any{
			"status":                  firstNonEmpty(adapterCompactPublicString(firstString(continuation["status"]), 80), "unknown"),
			"poll_url":                adapterPublicContinuationRoute(continuation["poll_url"]),
			"events_url":              adapterPublicContinuationRoute(continuation["events_url"]),
			"pending_sources":         adapterPublicStringList(continuation["pending_sources"], 8, 240),
			"warming_sources":         adapterPublicStringList(continuation["warming_sources"], 8, 240),
			"failed_sources":          adapterPublicStringList(continuation["failed_sources"], 4, 240),
			"timed_out_sources":       adapterPublicStringList(continuation["timed_out_sources"], 4, 240),
			"budget_exceeded_sources": adapterPublicStringList(continuation["budget_exceeded_sources"], 4, 240),
			"continuation_available":  continuation["continuation_available"] == true,
			"modeled_progress":        adapterPublicRunAdvisorModeledProgress(continuation["modeled_progress"]),
			"retrieval_progress":      adapterPublicRunAdvisorRetrievalProgress(continuation["retrieval_progress"]),
			"agent_visibility":        adapterPublicRunAdvisorVisibility(continuation["agent_visibility"]),
			"repair_instruction": firstNonEmpty(
				adapterCompactPublicString(firstString(continuation["repair_instruction"]), 900),
				"Rerun context compilation when evidence is missing.",
			),
			"agent_followup_command":   adapterCompactPublicString(firstString(continuation["agent_followup_command"]), 500),
			"agent_followup_endpoint":  adapterPublicContinuationRoute(continuation["agent_followup_endpoint"]),
			"agent_followup_transport": firstNonEmpty(adapterCompactPublicString(firstString(continuation["agent_followup_transport"]), 80), "none"),
		},
		"objective_coherence": map[string]any{
			"score":   adapterPublicNonnegativeInt(objective["score"]),
			"status":  firstNonEmpty(adapterCompactPublicString(firstString(objective["status"]), 80), "missing"),
			"signals": adapterPublicRunAdvisorSignals(objective["signals"], "objective_coherence"),
			"repair_instruction": firstNonEmpty(
				adapterCompactPublicString(firstString(objective["repair_instruction"]), 900),
				"Carry objective, goal, and mission into the next prompt packet.",
			),
		},
		"graph_quality": map[string]any{
			"status":         firstNonEmpty(adapterCompactPublicString(firstString(graph["status"]), 80), "not_sampled"),
			"score":          adapterPublicNonnegativeInt(graph["score"]),
			"used":           graph["used"] == true,
			"skipped_reason": adapterCompactPublicString(firstString(graph["skipped_reason"]), 240),
			"signals":        adapterPublicRunAdvisorSignals(graph["signals"], "graph_quality"),
			"recommendation": firstNonEmpty(
				adapterCompactPublicString(firstString(graph["recommendation"]), 900),
				"Run contextlattice_memory_topology when graph evidence matters.",
			),
		},
		"next_actions": adapterPublicRunAdvisorNextActions(advisor["next_actions"]),
	}
	return adapterStampRegisteredEnvelope("run_advisor.v1", public)
}

func adapterRestampNestedPublicContracts(value any, root bool) bool {
	switch typed := value.(type) {
	case map[string]any:
		for name, child := range typed {
			if !adapterRestampNestedPublicContracts(child, false) {
				return false
			}
			typed[name] = child
		}
		if root {
			return true
		}
		formatContract := asMap(typed["format_contract"])
		contractID := firstString(formatContract["schema_id"])
		switch contractID {
		case "agent_preflight_response.v1", "objective_runtime_state.v1", "policy_context_package.v1":
			if _, ok := adapterStampRegisteredEnvelope(contractID, typed); ok {
				return true
			}
			delete(typed, "format_contract")
		}
		return true
	case []any:
		for _, child := range typed {
			if !adapterRestampNestedPublicContracts(child, false) {
				return false
			}
		}
	}
	return true
}
