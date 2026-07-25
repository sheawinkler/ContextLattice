package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
)

const (
	frontierT8CLISkillEvolutionPath = "/memory/skills/foundry/evolution"
	frontierT8CLIMaxInputBytes      = 512 * 1024
)

type frontierT8CLIEvolutionSpec struct {
	wireOperation   string
	contractID      string
	explicitHandoff bool
}

var frontierT8CLIEvolutionOperations = map[string]frontierT8CLIEvolutionSpec{
	"usage-record": {
		wireOperation: "record_usage_receipt", contractID: "skill_usage_receipt.v1",
	},
	"efficacy-review": {
		wireOperation: "derive_efficacy_review", contractID: "skill_efficacy_review.v1",
	},
	"reusable-candidate": {
		wireOperation: "derive_reusable_candidate", contractID: "reusable_skill_candidate.v1",
	},
	"foundry-handoff": {
		wireOperation: "handoff_reusable_candidate", contractID: "reusable_skill_candidate.v1", explicitHandoff: true,
	},
	"retirement-candidate": {
		wireOperation: "derive_retirement_candidate", contractID: "skill_retirement_candidate.v1",
	},
}

func frontierT8SkillEvolutionUsage() string {
	return "contextlattice_agent_tools skill-evolution {usage-record|efficacy-review|reusable-candidate|foundry-handoff|retirement-candidate} --payload-file request.json [--pretty|--raw]"
}

func (c *cli) cmdSkillEvolution(args []string) error {
	parsed := parseArgs(args, mergeStringFlags(commonStringFlags(), map[string]string{
		"payload-file": "payload_file",
	}), commonBoolFlags())
	if parsed.bool("help") || len(parsed.pos) == 0 {
		return c.emitUsage(frontierT8SkillEvolutionUsage())
	}
	if len(parsed.pos) != 1 {
		return errors.New("skill-evolution accepts exactly one operation")
	}
	operation := strings.ToLower(strings.TrimSpace(parsed.pos[0]))
	spec, ok := frontierT8CLIEvolutionOperations[operation]
	if !ok {
		return errors.New("unknown skill-evolution operation")
	}
	payloadPath := parsed.string("payload_file", "")
	if payloadPath == "" {
		return errors.New("--payload-file is required")
	}
	input, err := frontierT8ReadSkillEvolutionInput(payloadPath)
	if err != nil {
		return err
	}
	wire := map[string]any{"operation": spec.wireOperation, "input": input}
	if spec.explicitHandoff {
		wire["explicit_handoff"] = true
	}
	if agentID := parsed.string("agent_id", ""); agentID != "" {
		wire["agent_id"] = agentID
	}
	c.applyBaseURL(parsed)
	result, status, err := c.requestJSON(context.Background(), http.MethodPost, frontierT8CLISkillEvolutionPath, wire, parsed.float("timeout", 30))
	if err != nil {
		if status > 0 {
			return fmt.Errorf("skill-evolution %s request failed with status %d", operation, status)
		}
		return fmt.Errorf("skill-evolution %s request failed", operation)
	}
	if firstString(result["schema_id"]) != spec.contractID || firstString(asMap(asMap(result["format_contract"])["validation"])["status"]) != "passed" {
		return errors.New("skill-evolution response failed its public contract")
	}
	return c.emit(result, parsed.bool("pretty") || !parsed.bool("raw"))
}

func frontierT8ReadSkillEvolutionInput(path string) (map[string]any, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("open skill-evolution payload file failed")
	}
	if info.Mode()&os.ModeSymlink != 0 || !frontierT7OwnerOnlyFileModeAllowed(info.Mode(), runtime.GOOS) {
		return nil, errors.New("skill-evolution payload file must be a regular owner-only file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("open skill-evolution payload file failed")
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || openedInfo.Mode()&os.ModeSymlink != 0 || !frontierT7OwnerOnlyFileModeAllowed(openedInfo.Mode(), runtime.GOOS) {
		return nil, errors.New("skill-evolution payload file must be a regular owner-only file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, frontierT8CLIMaxInputBytes+1))
	if err != nil {
		return nil, errors.New("read skill-evolution payload file failed")
	}
	if len(raw) > frontierT8CLIMaxInputBytes {
		return nil, errors.New("skill-evolution payload file exceeds the input limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return nil, errors.New("skill-evolution payload file must contain one JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("skill-evolution payload file must contain one JSON object")
	}
	return payload, nil
}
