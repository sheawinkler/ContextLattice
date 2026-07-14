package main

import (
	"net/http"
)

// optionalFrontierT1Runtime keeps paid runtime ownership out of the public
// build while preserving a common lifecycle and status surface.
type optionalFrontierT1Runtime interface {
	snapshot() map[string]any
	close()
}

type disabledOptionalFrontierT1Runtime struct {
	lastErrorCode string
}

func (r *disabledOptionalFrontierT1Runtime) snapshot() map[string]any {
	result := map[string]any{
		"schema_id":  "frontier_t1_governance_state.v1",
		"configured": false,
		"enabled":    false,
		"ready":      false,
	}
	if r != nil && r.lastErrorCode != "" {
		result["last_error_code"] = r.lastErrorCode
	}
	return result
}

func (*disabledOptionalFrontierT1Runtime) close() {}

var newOptionalFrontierT1RuntimeFromEnv = func() (optionalFrontierT1Runtime, error) {
	return &disabledOptionalFrontierT1Runtime{}, nil
}

var optionalFrontierT1RuntimeAfterError = func(error) optionalFrontierT1Runtime {
	return &disabledOptionalFrontierT1Runtime{lastErrorCode: "optional_runtime_unavailable"}
}

var optionalFrontierT1FeatureForPath = func(string) string { return "" }

var optionalFrontierT1ExplicitDevBypassActive = func() bool { return false }

var optionalFrontierT1DevelopmentAuthorization = func(string, string) (map[string]any, error) {
	return nil, nil
}

var optionalFrontierT1ProjectBoundary = func(*server, http.ResponseWriter, *http.Request, string) bool {
	return true
}

func (s *server) enforceOptionalFrontierT1ProjectBoundary(w http.ResponseWriter, r *http.Request, operationClass string) bool {
	return optionalFrontierT1ProjectBoundary(s, w, r, operationClass)
}
