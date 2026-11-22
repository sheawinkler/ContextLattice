package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	cliDreamTransferSchema      = "dream_experience_transfer.v1"
	cliDreamTransferMaxBytes    = 64 << 10
	cliDreamTransferRoute       = "/memory/dream"
	cliDreamTransferUsage       = "contextlattice dream transfer --project <destination> --payload-file <request.json> [--pretty]"
	cliDreamAuthorityUsage      = "contextlattice dream authorize-local --source <project> --destination <project> --passport <id> --experience <ref> --source-digest <sha256:digest> --grant-file <path> --proof-file <path> --ttl <duration up to 15m>"
	cliDreamTransferProofEnv    = "CONTEXTLATTICE_DREAM_TRANSFER_PROOF_FILE"
	cliDreamTransferProofHeader = "X-ContextLattice-Dream-Proof"
)

type cliDreamTransferArgs struct {
	project, file string
	pretty, help  bool
}

func parseCLIDreamTransferArgs(args []string) (cliDreamTransferArgs, error) {
	var out cliDreamTransferArgs
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		out.help = true
		return out, nil
	}
	if args[0] != "transfer" {
		return out, errors.New(cliDreamTransferUsage)
	}
	flags := flag.NewFlagSet("dream transfer", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	for name, target := range map[string]*string{"project": &out.project, "payload-file": &out.file} {
		flags.Func(name, "", func(value string) error {
			if *target != "" || value == "" || strings.HasPrefix(value, "-") {
				return errors.New("expected one explicit value")
			}
			*target = value
			return nil
		})
	}
	var raw bool
	flags.BoolVar(&out.pretty, "pretty", false, "")
	flags.BoolVar(&raw, "raw", false, "")
	flags.BoolVar(&out.help, "help", false, "")
	err := flags.Parse(args[1:])
	if errors.Is(err, flag.ErrHelp) || (err == nil && out.help) {
		out.help = true
		return out, nil
	}
	if err != nil || flags.NArg() != 0 || (out.pretty && raw) ||
		!cliDreamTransferProject(out.project) || strings.TrimSpace(out.file) == "" {
		return out, errors.New(cliDreamTransferUsage)
	}
	return out, nil
}

func cliDreamTransferProject(project string) bool {
	return project != "" && strings.TrimSpace(project) == project && project != "." && project != ".." &&
		!strings.HasPrefix(project, "_") && !strings.Contains(project, "::") && !strings.ContainsAny(project, "/\\")
}

func cliDreamTransferString(object map[string]json.RawMessage, key string) string {
	var value string
	if json.Unmarshal(object[key], &value) != nil {
		return ""
	}
	return value
}

func cliDreamTransferResultStatus(status string) bool {
	switch status {
	case "confirmed", "narrowed", "rejected", "unproven":
		return true
	default:
		return false
	}
}

func cliDreamTransferDigest(value, prefix string) bool {
	hex, ok := strings.CutPrefix(value, prefix)
	size := 64
	if prefix == "dtx_" {
		size = 32
	}
	if !ok || len(hex) != size {
		return false
	}
	for _, ch := range hex {
		if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

// This checks the response envelope only; it is not proof that the returned
// record was authorized or bound to this specific request.
func cliDreamTransferReceiptEnvelope(response map[string]json.RawMessage, project, action, expectedStatus, transferID string) (map[string]json.RawMessage, bool) {
	var receipt map[string]json.RawMessage
	if string(response["ok"]) != "true" || string(response["persisted"]) != "true" ||
		cliDreamTransferString(response, "project") != project ||
		cliDreamTransferString(response, "mode") != "dream_transfer" ||
		json.Unmarshal(response["dream_experience_transfer"], &receipt) != nil ||
		cliDreamTransferString(receipt, "schema_id") != cliDreamTransferSchema ||
		cliDreamTransferString(receipt, "destination_project") != project ||
		cliDreamTransferString(receipt, "action") != action ||
		cliDreamTransferString(receipt, "status") != expectedStatus ||
		cliDreamTransferString(receipt, "home_project_effect") != "unchanged" {
		return nil, false
	}
	actualID := cliDreamTransferString(receipt, "transfer_id")
	if !cliDreamTransferDigest(actualID, "dtx_") || (transferID != "" && actualID != transferID) {
		return nil, false
	}
	return receipt, true
}

func validateCLIDreamTransferPayload(project string, transfer map[string]json.RawMessage) (action, status, id string, err error) {
	invalid := errors.New("invalid dream transfer payload for the selected destination or action")
	// The schema tag identifies the local file; the gateway accepts only action fields.
	if _, present := transfer["schema_id"]; present {
		if cliDreamTransferString(transfer, "schema_id") != cliDreamTransferSchema {
			return "", "", "", invalid
		}
		delete(transfer, "schema_id")
	}
	action = cliDreamTransferString(transfer, "action")
	switch action {
	case "freeze":
		required := map[string]int{
			"action": 6, "grant_id": 0, "source_passport_id": 0, "experience_ref": 160,
			"destination_project": 0, "mechanism": 1000, "mismatch": 1000,
			"prediction": 1000, "falsifier": 1000, "expires_at": 0,
		}
		if len(transfer) != len(required) || cliDreamTransferString(transfer, "destination_project") != project {
			return "", "", "", invalid
		}
		for field, limit := range required {
			value := cliDreamTransferString(transfer, field)
			if value == "" || strings.TrimSpace(value) != value || (limit > 0 && len(value) > limit) {
				return "", "", "", invalid
			}
		}
		if _, err := time.Parse(time.RFC3339Nano, cliDreamTransferString(transfer, "expires_at")); err != nil {
			return "", "", "", invalid
		}
		return action, "frozen", "", nil
	case "record_result":
		for field := range transfer {
			switch field {
			case "action", "transfer_id", "result", "evidence_refs":
			default:
				return "", "", "", invalid
			}
		}
		id, status = cliDreamTransferString(transfer, "transfer_id"), cliDreamTransferString(transfer, "result")
		if !cliDreamTransferDigest(id, "dtx_") || !cliDreamTransferResultStatus(status) {
			return "", "", "", invalid
		}
		var refs []string
		if raw, present := transfer["evidence_refs"]; present && (json.Unmarshal(raw, &refs) != nil || refs == nil) {
			return "", "", "", invalid
		}
		if len(refs) > 8 || (len(refs) == 0 && status != "unproven") {
			return "", "", "", invalid
		}
		prefix := project + "::"
		for _, ref := range refs {
			if !strings.HasPrefix(ref, prefix) || len(ref) == len(prefix) || strings.TrimSpace(ref) != ref {
				return "", "", "", invalid
			}
		}
		return action, status, id, nil
	default:
		return "", "", "", invalid
	}
}

func cliDreamTransferRequestDigest(project string, transfer map[string]json.RawMessage) (string, error) {
	// Match the gateway's UseNumber decode and canonical map encoding, not
	// the payload file's original whitespace, key order or string escapes.
	raw, err := json.Marshal(transfer)
	if err != nil {
		return "", err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		return "", err
	}
	wire, err := json.Marshal(map[string]any{"project": project, "experience_transfer": decoded})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(wire)), nil
}

func (c *cli) cmdDream(args []string) error {
	opts, err := parseCLIDreamTransferArgs(args)
	if err != nil {
		return err
	}
	if opts.help {
		if len(args) == 0 || args[0] != "transfer" {
			return c.emitUsage(cliDreamAuthorityUsage + "\n" + cliDreamTransferUsage)
		}
		return c.emitUsage(cliDreamTransferUsage)
	}
	transfer, err := readCLIDreamTransferFile(opts.file)
	if err != nil {
		return err
	}
	action, status, id, err := validateCLIDreamTransferPayload(opts.project, transfer)
	if err != nil {
		return err
	}
	digest, err := cliDreamTransferRequestDigest(opts.project, transfer)
	if err != nil {
		return err
	}
	workerKey := strings.TrimSpace(os.Getenv("CONTEXTLATTICE_WORKER_API_KEY"))
	if workerKey == "" || workerKey == c.apiKey {
		return errors.New("dream transfer requires a separate CONTEXTLATTICE_WORKER_API_KEY")
	}
	proof := readCLIDreamTransferProof(os.Getenv(cliDreamTransferProofEnv))
	if !cliDreamTransferDigest(proof, "") {
		return errors.New("dream transfer requires an owner-only, valid " + cliDreamTransferProofEnv)
	}
	headers := make(http.Header)
	headers.Set("X-API-Key", workerKey)
	headers.Set(cliDreamTransferProofHeader, proof)
	response, err := c.postCLIDreamTransfer(opts.project, transfer, headers)
	if err != nil {
		return err
	}
	receipt, valid := cliDreamTransferReceiptEnvelope(response, opts.project, action, status, id)
	if !valid || cliDreamTransferString(receipt, "request_digest") != digest ||
		!cliDreamTransferDigest(cliDreamTransferString(receipt, "record_content_hash"), "") {
		return errors.New("dream transfer receipt not bound to the exact request; inspect the destination before retrying")
	}
	for _, object := range []map[string]json.RawMessage{response, receipt} {
		if projection, present := object["projection_persisted"]; present && string(projection) != "true" {
			return errors.New("dream transfer projection not confirmed; inspect the destination before retrying")
		}
	}
	verified := make(map[string]json.RawMessage)
	for _, field := range []string{"schema_id", "action", "status", "transfer_id", "destination_project",
		"home_project_effect", "request_digest", "record_content_hash", "projection_persisted"} {
		if value, ok := receipt[field]; ok {
			verified[field] = value
		}
	}
	return c.emit(map[string]any{
		"ok": true, "persisted": true, "mode": "dream_transfer", "project": opts.project,
		"dream_experience_transfer": verified,
	}, opts.pretty)
}

func readCLIDreamTransferFile(path string) (map[string]json.RawMessage, error) {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > cliDreamTransferMaxBytes {
		return nil, errors.New("dream transfer requires a regular --payload-file of at most 65536 bytes")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("could not open dream transfer payload file")
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("dream transfer payload file must be regular")
	}
	raw, err := io.ReadAll(io.LimitReader(file, cliDreamTransferMaxBytes+1))
	if err != nil || len(raw) > cliDreamTransferMaxBytes {
		return nil, errors.New("could not read bounded dream transfer payload")
	}
	var payload map[string]json.RawMessage
	if !decodeCLIDreamTransferJSON(raw, &payload) || payload == nil {
		return nil, errors.New("dream transfer payload must be one unambiguous UTF-8 JSON object")
	}
	return payload, nil
}

// Check keys before decoding so case variants and duplicate keys cannot
// describe a different action or receipt to another JSON reader.
func decodeCLIDreamTransferJSON(raw []byte, target any) bool {
	if !utf8.Valid(raw) || !jsonEscapedSurrogatesValid(raw) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 24 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return true
		}
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || name != strings.ToLower(name) || keys[name] {
					return false
				}
				keys[name] = true
			}
			if !walk(depth + 1) {
				return false
			}
		}
		_, err = d.Token()
		return err == nil
	}
	if !walk(0) {
		return false
	}
	if _, err := d.Token(); err != io.EOF {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

func (c *cli) postCLIDreamTransfer(project string, transfer map[string]json.RawMessage, headers http.Header) (map[string]json.RawMessage, error) {
	origin, err := url.Parse(c.baseURL)
	if err != nil || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.ForceQuery ||
		origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") ||
		(origin.Scheme != "http" && origin.Scheme != "https") {
		return nil, errors.New("dream transfer requires a configured gateway origin")
	}
	if origin.Scheme == "http" && origin.Hostname() != "localhost" && !net.ParseIP(origin.Hostname()).IsLoopback() {
		return nil, errors.New("dream transfer requires HTTPS for a non-loopback gateway")
	}
	body, err := json.Marshal(map[string]any{
		"project": project, "experience_transfer": transfer,
		"use_llm": false, "persist": false,
	})
	if err != nil || len(body) > cliDreamTransferMaxBytes {
		return nil, errors.New("dream transfer request exceeds its payload bound")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+cliDreamTransferRoute, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("could not construct dream transfer request")
	}
	req.Header = headers.Clone()
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	req.Header.Set("Content-Type", "application/json")
	// Do not forward a mutation or credentials to any redirect target.
	client := *c.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("dream transfer not confirmed by gateway; inspect the destination before retrying")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("dream transfer not confirmed by gateway; inspect the destination before retrying")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, cliDreamTransferMaxBytes+1))
	var result map[string]json.RawMessage
	if err != nil || len(raw) > cliDreamTransferMaxBytes || !decodeCLIDreamTransferJSON(raw, &result) || result == nil {
		return nil, errors.New("invalid dream transfer response; inspect the destination before retrying")
	}
	return result, nil
}
