package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	mcpUsage           = "contextlattice mcp --project PROJECT"
	mcpTool            = "contextlattice_context"
	mcpLatestHandshake = "2025-11-25"
	mcpFrameLimit      = 32 << 10
	mcpGatewayLimit    = 256 << 10
	mcpOutputLimit     = 160 << 10
	mcpBatchLimit      = 4
	mcpRowLimit        = 5
	mcpSummaryLimit    = 1024
	mcpQueryLimit      = 2048
)

var mcpProject = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,119}$`)

type mcpObject = map[string]any
type mcpSession struct {
	client           *cli
	project, version string
	ready            bool
}

func (c *cli) cmdMCPStdio(args []string) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		return c.emitUsage(mcpUsage)
	}
	if len(args) != 2 || args[0] != "--project" || !mcpProject.MatchString(args[1]) {
		return errors.New(mcpUsage)
	}
	if !conflictOriginAllowed(c.baseURL) || strings.ContainsAny(c.baseURL, "?#") {
		return errors.New("MCP requires an HTTPS or loopback HTTP gateway origin")
	}
	s := mcpSession{client: c, project: args[1]}
	reader := bufio.NewReaderSize(c.stdin, mcpFrameLimit+1)
	for {
		line, err := reader.ReadSlice('\n')
		if err == io.EOF && len(line) == 0 {
			return nil
		}
		if err != nil || len(line) > mcpFrameLimit+1 {
			return errors.New("incomplete or oversized MCP stdio frame")
		}
		reply := s.frame(line[:len(line)-1])
		if reply == nil {
			continue
		}
		raw, err := json.Marshal(reply)
		if err != nil || len(raw) > mcpOutputLimit {
			return errors.New("invalid or oversized MCP output")
		}
		if _, err = c.stdout.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
}

// Use exact, unique member names even in nested objects (not Go struct field
// matching). Ambiguous project tags in a gateway response must never authorize a row.
func mcpDecode(raw []byte) (any, bool) {
	if !utf8.Valid(raw) || !json.Valid(raw) || !jsonEscapedSurrogatesValid(raw) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var read func(int) (any, bool)
	read = func(depth int) (any, bool) {
		if depth > 32 {
			return nil, false
		}
		t, err := d.Token()
		if err != nil {
			return nil, false
		}
		switch t {
		case json.Delim('{'):
			obj := mcpObject{}
			for d.More() {
				k, _ := d.Token()
				key := k.(string) // json.Valid guarantees an object member name.
				if _, duplicate := obj[key]; duplicate {
					return nil, false
				}
				v, ok := read(depth + 1)
				if !ok {
					return nil, false
				}
				obj[key] = v
			}
			_, err = d.Token()
			return obj, err == nil
		case json.Delim('['):
			list := []any{}
			for d.More() {
				v, ok := read(depth + 1)
				if !ok {
					return nil, false
				}
				list = append(list, v)
			}
			_, err = d.Token()
			return list, err == nil
		default:
			return t, true
		}
	}
	return read(0)
}

func mcpError(id any, code int, message string) mcpObject {
	return mcpObject{"jsonrpc": "2.0", "id": id, "error": mcpObject{"code": code, "message": message}}
}

func (s *mcpSession) frame(line []byte) any {
	value, ok := mcpDecode(line)
	if !ok {
		return mcpError(nil, -32700, "Invalid JSON")
	}
	if batch, ok := value.([]any); ok && s.version == "2025-03-26" && len(batch) > 0 && len(batch) <= mcpBatchLimit {
		var replies []mcpObject
		for _, req := range batch {
			if reply := s.request(req); reply != nil {
				replies = append(replies, reply)
			}
		}
		if len(replies) == 0 {
			return nil
		}
		return replies
	}
	if reply := s.request(value); reply != nil {
		return reply
	}
	return nil
}

func mcpKeys(obj mcpObject, keys ...string) bool {
	for k := range obj {
		if !slices.Contains(keys, k) {
			return false
		}
	}
	return true
}

func mcpValidID(value any) bool {
	switch id := value.(type) {
	case string:
		return len(id) <= 256
	case json.Number:
		_, err := id.Int64()
		return err == nil
	default:
		return false
	}
}

func (s *mcpSession) request(value any) mcpObject {
	req, _ := value.(mcpObject)
	id, hasID := req["id"]
	method, _ := req["method"].(string)
	if req["jsonrpc"] != "2.0" || method == "" || !mcpKeys(req, "jsonrpc", "id", "method", "params") || (hasID && !mcpValidID(id)) {
		return mcpError(nil, -32600, "Invalid request")
	}
	params, ok := req["params"].(mcpObject)
	_, hasParams := req["params"]
	meta, hasMeta := params["_meta"]
	_, validMeta := meta.(mcpObject)
	validParams := (!hasParams || ok) && (!hasMeta || validMeta)
	if !hasID {
		if method == "notifications/initialized" && s.version != "" && validParams && mcpKeys(params, "_meta") {
			s.ready = true
		}
		return nil
	}
	bad := func() mcpObject { return mcpError(id, -32602, "Invalid params") }
	if !validParams {
		return bad()
	}
	var result any
	switch method {
	case "initialize":
		if s.version != "" {
			return mcpError(id, -32600, "Already initialized")
		}
		requested, _ := params["protocolVersion"].(string)
		info, _ := params["clientInfo"].(mcpObject)
		name, _ := info["name"].(string)
		version, _ := info["version"].(string)
		if _, ok := params["capabilities"].(mcpObject); !ok || name == "" || version == "" || requested == "" || !mcpKeys(params, "_meta", "protocolVersion", "capabilities", "clientInfo") {
			return bad()
		}
		switch requested {
		case "2024-11-05", "2025-03-26", "2025-06-18", mcpLatestHandshake:
			s.version = requested
		default:
			s.version = mcpLatestHandshake
		}
		result = mcpObject{"protocolVersion": s.version, "capabilities": mcpObject{"tools": mcpObject{}}, "serverInfo": mcpObject{"name": "contextlattice", "version": "1"}}
	case "ping":
		if !mcpKeys(params, "_meta") {
			return bad()
		}
		result = mcpObject{}
	default:
		if !s.ready {
			return mcpError(id, -32002, "Not initialized")
		}
		switch method {
		case "tools/list":
			if !mcpKeys(params, "_meta") {
				return bad()
			}
			result = mcpObject{"tools": []any{mcpObject{
				"name": mcpTool, "description": "Search summaries in the launch project (read-only).",
				"inputSchema": mcpObject{
					"type": "object", "required": []string{"query"}, "additionalProperties": false,
					"properties": mcpObject{"query": mcpObject{"type": "string", "minLength": 1, "maxLength": mcpQueryLimit}},
				},
			}}}
		case "tools/call":
			args, _ := params["arguments"].(mcpObject)
			q, _ := args["query"].(string)
			if !mcpKeys(params, "_meta", "name", "arguments") || params["name"] != mcpTool || !mcpKeys(args, "query") || strings.TrimSpace(q) == "" || len(q) > mcpQueryLimit || strings.IndexFunc(q, unicode.IsControl) >= 0 {
				return bad()
			}
			result = s.search(q)
		default:
			return mcpError(id, -32601, "Method not found")
		}
	}
	return mcpObject{"jsonrpc": "2.0", "id": id, "result": result}
}

func mcpText(text string) mcpObject { return mcpObject{"type": "text", "text": text} }

func (s *mcpSession) search(query string) mcpObject {
	failure := mcpObject{"isError": true, "content": []any{mcpText("Project search unavailable.")}}
	payload := mcpObject{"project": s.project, "query": query, "limit": mcpRowLimit, "retrieval_mode": "fast", "fetch_content": false, "include_grounding": false, "include_retrieval_debug": false}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The bounded CLI source reader uses the configured API key only. The
	// general-purpose transport may provision local identity for entitlements.
	raw, err := s.client.conflictRequest(ctx, http.MethodPost, "/memory/search", payload, mcpGatewayLimit)
	if err != nil {
		return failure
	}
	value, ok := mcpDecode(raw)
	obj, _ := value.(mcpObject)
	rows, valid := obj["results"].([]any)
	status, declared := obj["ok"]
	scope, scoped := obj["project"]
	if !ok || !valid || (declared && status != true) || (scoped && scope != s.project) {
		return failure
	}
	content := []any{}
	for _, item := range rows {
		row, _ := item.(mcpObject)
		if row["project"] != s.project {
			continue
		}
		summary, _ := row["summary"].(string)
		if strings.TrimSpace(summary) == "" {
			continue
		}
		if len(summary) > mcpSummaryLimit {
			end := mcpSummaryLimit - 3
			for !utf8.RuneStart(summary[end]) {
				end--
			}
			summary = summary[:end] + "..."
		}
		content = append(content, mcpText(summary))
		if len(content) == mcpRowLimit {
			break
		}
	}
	if len(content) == 0 {
		content = append(content, mcpText("No matching project summaries in the immediate search results."))
	}
	return mcpObject{"content": content}
}
