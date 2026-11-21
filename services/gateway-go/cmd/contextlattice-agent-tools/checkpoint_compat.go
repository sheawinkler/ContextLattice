package main

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
)

const checkpointUsage = "contextlattice_checkpoint --project <name> --topic-path <path> --file <logical.md> [--content <text>|--content-file <path>|--stdin] [--query <text>] [--timeout <seconds>]"

func (c *cli) cmdCheckpoint(args []string) error {
	parsed := parsedArgs{values: map[string]string{}, bools: map[string]bool{}}
	flags := map[string]string{
		"--project": "project", "-p": "project",
		"--topic-path": "topic_path", "-t": "topic_path",
		"--file": "file", "-f": "file",
		"--content": "content", "-c": "content",
		"--content-file": "content_file", "--timeout": "timeout",
		"--query": "query", "-q": "query", // Retained for callers; readback is exact.
	}
	for len(args) > 0 {
		switch args[0] {
		case "--help", "-h":
			return c.emitUsage(checkpointUsage)
		case "--stdin":
			parsed.bools["stdin"] = true
			args = args[1:]
		default:
			field, ok := flags[args[0]]
			if !ok || len(args) < 2 {
				return errors.New(checkpointUsage)
			}
			parsed.values[field] = args[1]
			args = args[2:]
		}
	}
	project := envString("CONTEXTLATTICE_PROJECT", "contextlattice")
	if parsed.has("project") {
		project = strings.TrimSpace(parsed.values["project"])
	}
	file := parsed.string("file", "")
	if project == "" || parsed.string("topic_path", "") == "" || file == "" {
		return errors.New("checkpoint requires a project, topic path and file")
	}
	timeout, err := strconv.ParseFloat(parsed.string("timeout", envString("CONTEXTLATTICE_HOOK_TIMEOUT_SECS", "30")), 64)
	if err != nil || !finitePositiveFloat(timeout) {
		return errors.New("checkpoint timeout must be positive and finite")
	}
	content, err := readCheckpointContent(parsed, os.Stdin)
	if err != nil {
		return err
	}
	// Reject paths the write or the HTTP router would normalize differently
	// before the write, so the readback always names the same logical file.
	segments := strings.Split(file, "/")
	if strings.ContainsAny(project, "/\\") || project == "." || project == ".." ||
		strings.Contains(file, "\\") {
		return errors.New("invalid checkpoint path")
	}
	for index, segment := range segments {
		if segment == "" || segment == "." || segment == ".." || strings.TrimSpace(segment) != segment {
			return errors.New("invalid checkpoint path")
		}
		segments[index] = url.PathEscape(segment)
	}

	checkpoint := *c
	client := *c.client
	// An exact logical read is not proved by bytes from a redirected path.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	checkpoint.client = &client
	payload, _, _, err := checkpoint.prepareCallerWrite(parsed, project, file, content, true)
	if err != nil {
		return err
	}
	ctx := context.Background()
	write, status, err := checkpoint.requestJSON(ctx, http.MethodPost, "/memory/write", payload, timeout)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 || write["ok"] != true {
		return errors.New("checkpoint write was not explicitly confirmed")
	}
	body, status, _, err := checkpoint.requestGatewayBytes(ctx, http.MethodGet,
		"/memory/files/"+url.PathEscape(project)+"/"+strings.Join(segments, "/"), nil, timeout, nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return errors.New("checkpoint file readback failed")
	}
	// The old hook used shell command substitution, which strips trailing LFs.
	exact := strings.TrimRight(string(body), "\n") == content
	if ref := firstString(write["content_ref"]); exact && strings.HasPrefix(ref, "sha256:") {
		exact = ref == "sha256:"+hex.EncodeToString(sha256Sum(body))
	}
	readback := map[string]any{"degraded": false, "count": 0, "first_file": nil}
	if exact {
		readback["count"], readback["first_file"] = 1, file
	}
	if err := c.emit(map[string]any{
		"ok": exact,
		"write": map[string]any{
			"ok": true, "source": write["source"], "content_ref": write["content_ref"],
		},
		"readback": readback,
	}, false); err != nil {
		return err
	}
	if !exact {
		return errors.New("checkpoint file readback did not match the submitted content")
	}
	return nil
}

func readCheckpointContent(parsed parsedArgs, stdin io.Reader) (string, error) {
	input := stdin
	stream := parsed.bool("stdin")
	if path := parsed.values["content_file"]; path != "" {
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer file.Close()
		input, stream = file, true
	}
	content := parsed.values["content"]
	if stream {
		raw, err := io.ReadAll(io.LimitReader(input, maximumGatewayResponseBytes+1))
		if err != nil {
			return "", err
		}
		content = strings.TrimRight(string(raw), "\n")
		if len(raw) > maximumGatewayResponseBytes {
			return "", errors.New("checkpoint content exceeds the bounded CLI limit")
		}
	}
	if content == "" || len(content) > maximumGatewayResponseBytes {
		return "", errors.New("checkpoint requires nonempty bounded content")
	}
	return content, nil
}
