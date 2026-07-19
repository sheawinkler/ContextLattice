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

var optionalFrontierT3UtilityAnalyticsRoute = func(_ *server, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"ok": false, "error": "premium_utility_analytics_unavailable",
		"detail": "The local Utility Ledger remains available through contextlattice utility.",
	})
}

var optionalFrontierT3UtilityPolicyRoute = func(_ *server, w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"ok": false, "error": "premium_utility_policy_unavailable",
		"detail": "Policy evaluation requires the paid runtime artifact.",
	})
}

func (s *server) enforceOptionalFrontierT1ProjectBoundary(w http.ResponseWriter, r *http.Request, operationClass string) bool {
	return optionalFrontierT1ProjectBoundary(s, w, r, operationClass)
}

// optionalFrontierT10Route keeps paid Aggregate Signal cohort governance out
// of the public build. Entitled builds populate this registry from paid-only
// source; the public local preview/accounting route remains available.
type optionalFrontierT10Route struct {
	Name             string
	Path             string
	ContractID       string
	BoundarySurface  string
	BoundaryDetail   string
	OwnershipSurface string
	OwnershipDetail  string
	Handler          func(*server, http.ResponseWriter, *http.Request)
}

var optionalFrontierT10RouteRegistry []optionalFrontierT10Route

var optionalFrontierT10Routes = func() []optionalFrontierT10Route {
	return append([]optionalFrontierT10Route(nil), optionalFrontierT10RouteRegistry...)
}

func addOptionalFrontierT10Routes(routes ...optionalFrontierT10Route) {
	optionalFrontierT10RouteRegistry = append(optionalFrontierT10RouteRegistry, routes...)
}

func optionalFrontierT10RequiredRoutePaths() []string {
	routes := optionalFrontierT10Routes()
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			paths = append(paths, route.Path)
		}
	}
	return paths
}

func optionalFrontierT10BoundarySurfaces() []contextBoundarySurface {
	routes := optionalFrontierT10Routes()
	surfaces := make([]contextBoundarySurface, 0, len(routes))
	for _, route := range routes {
		if route.Path == "" {
			continue
		}
		surfaces = append(surfaces, contextBoundarySurface{
			Name: route.Name, Path: route.Path, Surface: route.BoundarySurface,
			ContractID: route.ContractID, RuntimeOwner: sourceOwnerGoNative,
			Required: true, Detail: route.BoundaryDetail,
		})
	}
	return surfaces
}

func optionalFrontierT10OwnedRoutes() []nativeOwnedRoute {
	routes := optionalFrontierT10Routes()
	owned := make([]nativeOwnedRoute, 0, len(routes))
	for _, route := range routes {
		if route.Path == "" {
			continue
		}
		owned = append(owned, nativeOwnedRoute{
			Path: route.Path, Surface: route.OwnershipSurface, Owner: sourceOwnerGoNative,
			Status: "native", Detail: route.OwnershipDetail, Required: true,
		})
	}
	return owned
}

func registerOptionalFrontierT10Routes(mux *http.ServeMux, s *server) {
	for _, route := range optionalFrontierT10Routes() {
		if route.Path == "" || route.Handler == nil {
			continue
		}
		handler := route.Handler
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(s, w, r)
		})
	}
}
