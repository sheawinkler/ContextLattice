package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"
)

func dreamTransferWorkerCredentials(s *server, r *http.Request) (key, proof string, ok bool) {
	if s == nil || r == nil || r.URL == nil || r.URL.RawQuery != "" ||
		len(r.Header.Values("Authorization")) != 0 || !s.toolCalls.roleSplitEnabled ||
		!s.toolCallAllowedForRole("dream_transfer", "worker") {
		return "", "", false
	}
	if _, denied := s.toolCalls.workerRole.denylist[normalizeToolPath("dream")]; denied {
		return "", "", false
	}
	keys, proofs := r.Header.Values("X-Api-Key"), r.Header.Values("X-ContextLattice-Dream-Proof")
	if len(keys) != 1 || len(proofs) != 1 || keys[0] == "" || strings.TrimSpace(keys[0]) != keys[0] ||
		!isHexDigest(proofs[0]) || secureTokenEqual(keys[0], s.orchestratorAPIKey) || !secureTokenEqual(keys[0], s.toolCalls.workerKey) {
		return "", "", false
	}
	return keys[0], proofs[0], true
}

// This is a permission name, not a route. It lets a worker confirm one
// authorized transfer without acquiring general Dream retrieval or inference.
func (s *server) serveDreamTransferWire(w http.ResponseWriter, r *http.Request, route string) {
	if _, _, allowed := dreamTransferWorkerCredentials(s, r); !allowed || strings.TrimSpace(s.orchestratorAPIKey) == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	payload, status := readDreamTransferWire(r)
	if status != http.StatusOK {
		writeJSON(w, status, map[string]any{"ok": false, "error": "invalid transfer request"})
		return
	}
	if !dreamTransferMemoryDispatchPayload(payload) {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "forbidden"})
		return
	}
	headers := make(http.Header)
	headers.Set("X-Api-Key", s.orchestratorAPIKey)
	response, status := s.buildDreamModeResponse(s.dreamTransferContext(r.Context(), r), headers, payload, route)
	if route == "/tools/dream" {
		response["tool"] = "dream"
		response = attachPayloadFormatContract(dreamModeResponseContractID, response, anyToString(response["agent_id"]), "dream", route)
	}
	writeJSON(w, status, response)
}

// The manual protocol has a flat envelope, a transfer object and at most a
// list of evidence IDs. Reject duplicate names before interpreting the request.
func readDreamTransferWire(r *http.Request) (map[string]any, int) {
	if r == nil || r.Body == nil {
		return nil, http.StatusBadRequest
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, dreamExperienceTransferMaxBytes+1))
	if len(raw) > dreamExperienceTransferMaxBytes {
		return nil, http.StatusRequestEntityTooLarge
	}
	if err != nil || !utf8.Valid(raw) {
		return nil, http.StatusBadRequest
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	var consume func(int) bool
	consume = func(depth int) bool {
		if depth > 4 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, complex := token.(json.Delim)
		if !complex {
			return true
		}
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				name, err := d.Token()
				key, ok := name.(string)
				if err != nil || !ok || key == "" || key != strings.ToLower(key) || keys[key] {
					return false
				}
				keys[key] = true
			}
			if !consume(depth + 1) {
				return false
			}
		}
		_, err = d.Token()
		return err == nil
	}
	if !consume(0) {
		return nil, http.StatusBadRequest
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, http.StatusBadRequest
	}
	var data map[string]any
	if json.Unmarshal(raw, &data) != nil || data == nil {
		return nil, http.StatusBadRequest
	}
	return data, http.StatusOK
}
