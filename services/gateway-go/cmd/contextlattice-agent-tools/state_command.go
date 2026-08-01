package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/contextlattice/gateway-go/internal/gatewaystate"
)

func stateCommandUsage() string {
	return "contextlattice state {status|migrate|rollback} [--legacy-root path] [--state-root path] [--manifest path] [--apply --yes] [--pretty]"
}

func (c *cli) cmdState(args []string) error {
	subcommand := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		subcommand = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	parsed := parseArgs(
		args,
		mergeStringFlags(commonStringFlags(), map[string]string{
			"legacy-root": "legacy_root",
			"state-root":  "state_root",
			"manifest":    "manifest",
		}),
		mergeBoolFlags(commonBoolFlags(), map[string]string{
			"apply": "apply",
			"yes":   "yes",
		}),
	)
	if parsed.bool("help") {
		return c.emitUsage(stateCommandUsage())
	}
	switch subcommand {
	case "status":
		c.applyBaseURL(parsed)
		payload, _, err := c.requestJSON(context.Background(), http.MethodGet, "/telemetry/storage", nil, parsed.float("timeout", 10))
		if err != nil {
			return err
		}
		inventory := asMap(payload["gatewayState"])
		if len(inventory) == 0 {
			return errors.New("gateway storage telemetry did not include gatewayState inventory")
		}
		return c.emit(map[string]any{
			"ok":            asBool(inventory["ok"]),
			"schema_id":     "contextlattice_gateway_state_status.v1",
			"gateway_state": inventory,
		}, parsed.bool("pretty"))
	case "migrate":
		legacyRoot := parsed.string("legacy_root", "")
		if legacyRoot == "" {
			return fmt.Errorf("%s: migrate requires --legacy-root", stateCommandUsage())
		}
		result, migrationErr := gatewaystate.ExecuteMigration(gatewaystate.MigrationOptions{
			LegacyRoot: legacyRoot,
			StateRoot:  parsed.string("state_root", ""),
			Apply:      parsed.bool("apply"),
			Confirm:    parsed.bool("yes"),
		})
		if emitErr := c.emit(result, parsed.bool("pretty")); emitErr != nil {
			return emitErr
		}
		return migrationErr
	case "rollback":
		manifestPath := parsed.string("manifest", "")
		if manifestPath == "" {
			return fmt.Errorf("%s: rollback requires --manifest", stateCommandUsage())
		}
		result, rollbackErr := gatewaystate.RollbackMigration(manifestPath, parsed.bool("yes"))
		if emitErr := c.emit(result, parsed.bool("pretty")); emitErr != nil {
			return emitErr
		}
		return rollbackErr
	default:
		return fmt.Errorf("unknown state subcommand %q; %s", subcommand, stateCommandUsage())
	}
}
