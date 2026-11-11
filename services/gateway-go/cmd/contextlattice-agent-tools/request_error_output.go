package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func writeCLIError(stderr io.Writer, err error) {
	var requestErr *cliRequestError
	if !errors.As(err, &requestErr) || requestErr == nil {
		fmt.Fprintln(stderr, err.Error())
		return
	}

	// Raw URLs, backend messages and wrapped errors may contain private data.
	evidence := requestErr.evidence()
	delete(evidence, "path")
	switch requestErr.Method {
	case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS":
	default:
		evidence["method"] = "unknown"
	}
	_ = json.NewEncoder(stderr).Encode(map[string]any{
		"ok": false, "error": requestErr.Error(), "error_evidence": evidence,
	})
}
