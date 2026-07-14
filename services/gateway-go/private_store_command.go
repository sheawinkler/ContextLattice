package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

func runOwnerOnlyMigrationCommand(args []string, stdout io.Writer, stderr io.Writer) (bool, int) {
	if len(args) < 2 || strings.TrimSpace(args[1]) != "owner-only-migrate" {
		return false, 0
	}
	flags := flag.NewFlagSet("owner-only-migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", "", "sensitive store root to migrate")
	force := flags.Bool("force", false, "verify every entry even when the durable receipt is complete")
	if err := flags.Parse(args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return true, 0
		}
		return true, 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "owner-only-migrate does not accept positional arguments")
		return true, 2
	}
	cleanRoot := strings.TrimSpace(*root)
	if cleanRoot == "" {
		cleanRoot = strings.TrimSpace(os.Getenv("GO_MEMORY_STORE_ROOT"))
	}
	if cleanRoot == "" {
		fmt.Fprintln(stderr, "owner-only-migrate requires --root or GO_MEMORY_STORE_ROOT")
		return true, 2
	}

	report, err := migrateOwnerOnlyStoreWithOptions(cleanRoot, ownerOnlyMigrationOptions{force: *force})
	if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
		fmt.Fprintln(stderr, "owner-only-migrate could not encode its result")
		return true, 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "owner-only-migrate failed; inspect the selected local root")
		return true, 1
	}
	return true, 0
}
