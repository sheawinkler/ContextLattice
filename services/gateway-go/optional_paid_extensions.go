package main

import (
	"errors"
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

var optionalFrontierT1ProjectBoundaryLock = func(*server) func() {
	return func() {}
}

// Optional task-delivery authority hooks keep paid workspace governance and
// signed runtime-license identity out of the public core. Public builds fail
// closed unless an entitled extension installs these resolvers; service-key
// operations remain independently authenticated by the Gateway.
var optionalAgentTaskProjectWorkspace = func(_ *server, _ string) (string, error) {
	return "", errors.New("workspace project binding governance is unavailable")
}

var optionalAgentTaskSignedRouteAuthorization = func(_ *server, _ *http.Request) (agentTaskRouteAuth, bool, error) {
	return agentTaskRouteAuth{}, false, nil
}

var optionalMemoryGraphRepairApplyAuthorization = func(_ *server, w http.ResponseWriter, _ *http.Request, _ *memoryGraphRepairRequest) (string, bool) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"ok": false, "error": "signed runtime-license entitlement enforcement is required for graph repair apply",
	})
	return "", false
}

var optionalRetrievalPromotionGovernanceAuthorization = func(_ *server, w http.ResponseWriter, _ *http.Request, _ map[string]any, _, _ string) (string, map[string]any, bool) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"ok": false, "error": "premium_retrieval_promotion_governance_unavailable",
	})
	return "", nil, false
}

// Public builds retain outcome and impact intelligence but cannot apply paid
// learned influence. Entitled source installs credential and governed-policy
// resolvers without moving paid identity into the public core.
var optionalContextPackLearnedRequestAuthority = func(_ *server, _ *http.Request) contextPackLearnedActivationAuthority {
	return contextPackLearnedActivationAuthority{Reason: "paid_activation_unavailable"}
}

var optionalContextPackLearnedPolicyAuthority = func(_ *server, authority contextPackLearnedActivationAuthority, _, _, _ string) contextPackLearnedActivationAuthority {
	authority.Authorized = false
	authority.Reason = "paid_activation_unavailable"
	return authority
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

// optionalFrontierT5Route lets entitled builds install paid Policy Laboratory
// routes without leaving paid symbols in the public projection.
type optionalFrontierT5Route struct {
	Name             string
	Path             string
	ContractID       string
	BoundarySurface  string
	BoundaryDetail   string
	OwnershipSurface string
	OwnershipDetail  string
	Handler          func(*server, http.ResponseWriter, *http.Request)
}

var optionalFrontierT5Routes = func() []optionalFrontierT5Route { return nil }

// optionalRecallResponseCanaryRoute keeps response-canary policy mutation and
// storage out of the public build. The public composition policy remains a
// typed zero-default interface.
type optionalRecallResponseCanaryRoute = optionalFrontierT5Route

var optionalRecallResponseCanaryRoutes = func() []optionalRecallResponseCanaryRoute { return nil }

func optionalRecallResponseCanaryRequiredRoutePaths() []string {
	routes := optionalRecallResponseCanaryRoutes()
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			paths = append(paths, route.Path)
		}
	}
	return paths
}

func optionalRecallResponseCanaryBoundarySurfaces() []contextBoundarySurface {
	routes := optionalRecallResponseCanaryRoutes()
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

func optionalRecallResponseCanaryOwnedRoutes() []nativeOwnedRoute {
	routes := optionalRecallResponseCanaryRoutes()
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

func registerOptionalRecallResponseCanaryRoutes(mux *http.ServeMux, s *server) {
	for _, route := range optionalRecallResponseCanaryRoutes() {
		if route.Path == "" || route.Handler == nil {
			continue
		}
		handler := route.Handler
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(s, w, r)
		})
	}
}

// optionalFrontierT6Route keeps paid Agent Fit governance out of the public
// build. Entitled builds populate the registry from their paid-only source.
type optionalFrontierT6Route struct {
	Name             string
	Path             string
	ContractID       string
	BoundarySurface  string
	BoundaryDetail   string
	OwnershipSurface string
	OwnershipDetail  string
	Handler          func(*server, http.ResponseWriter, *http.Request)
}

var optionalFrontierT6Routes = func() []optionalFrontierT6Route { return nil }

// optionalFrontierT7Route keeps paid portable-continuation governance out of
// the public build. Entitled builds populate the registry from paid-only source.
type optionalFrontierT7Route struct {
	Name             string
	Path             string
	ContractID       string
	BoundarySurface  string
	BoundaryDetail   string
	OwnershipSurface string
	OwnershipDetail  string
	Handler          func(*server, http.ResponseWriter, *http.Request)
}

var optionalFrontierT7Routes = func() []optionalFrontierT7Route { return nil }

// optionalFrontierT8Route keeps paid Skill Evolution governance out of the
// public build. Entitled builds populate the registry from paid-only source.
type optionalFrontierT8Route struct {
	Name             string
	Path             string
	ContractID       string
	BoundarySurface  string
	BoundaryDetail   string
	OwnershipSurface string
	OwnershipDetail  string
	Handler          func(*server, http.ResponseWriter, *http.Request)
}

var optionalFrontierT8Routes = func() []optionalFrontierT8Route { return nil }

// optionalFrontierT9Route keeps paid Continuity Zero automation out of the
// public build. Entitled builds populate this registry from paid-only source.
type optionalFrontierT9Route struct {
	Name             string
	Path             string
	ContractID       string
	BoundarySurface  string
	BoundaryDetail   string
	OwnershipSurface string
	OwnershipDetail  string
	Handler          func(*server, http.ResponseWriter, *http.Request)
}

var optionalFrontierT9Routes = func() []optionalFrontierT9Route { return nil }

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

func optionalFrontierT9RequiredRoutePaths() []string {
	routes := optionalFrontierT9Routes()
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			paths = append(paths, route.Path)
		}
	}
	return paths
}

func optionalFrontierT9BoundarySurfaces() []contextBoundarySurface {
	routes := optionalFrontierT9Routes()
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

func optionalFrontierT9OwnedRoutes() []nativeOwnedRoute {
	routes := optionalFrontierT9Routes()
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

func registerOptionalFrontierT9Routes(mux *http.ServeMux, s *server) {
	for _, route := range optionalFrontierT9Routes() {
		if route.Path == "" || route.Handler == nil {
			continue
		}
		handler := route.Handler
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(s, w, r)
		})
	}
	registerOptionalFrontierT10Routes(mux, s)
}

func optionalFrontierT8RequiredRoutePaths() []string {
	routes := optionalFrontierT8Routes()
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			paths = append(paths, route.Path)
		}
	}
	return paths
}

func optionalFrontierT8BoundarySurfaces() []contextBoundarySurface {
	routes := optionalFrontierT8Routes()
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

func optionalFrontierT8OwnedRoutes() []nativeOwnedRoute {
	routes := optionalFrontierT8Routes()
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

func registerOptionalFrontierT8Routes(mux *http.ServeMux, s *server) {
	for _, route := range optionalFrontierT8Routes() {
		if route.Path == "" || route.Handler == nil {
			continue
		}
		handler := route.Handler
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(s, w, r)
		})
	}
	registerOptionalFrontierT9Routes(mux, s)
}

func optionalFrontierT7RequiredRoutePaths() []string {
	routes := optionalFrontierT7Routes()
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			paths = append(paths, route.Path)
		}
	}
	return paths
}

func optionalFrontierT7BoundarySurfaces() []contextBoundarySurface {
	routes := optionalFrontierT7Routes()
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

func optionalFrontierT7OwnedRoutes() []nativeOwnedRoute {
	routes := optionalFrontierT7Routes()
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

func registerOptionalFrontierT7Routes(mux *http.ServeMux, s *server) {
	for _, route := range optionalFrontierT7Routes() {
		if route.Path == "" || route.Handler == nil {
			continue
		}
		handler := route.Handler
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(s, w, r)
		})
	}
	registerOptionalFrontierT8Routes(mux, s)
}

func optionalFrontierT6RequiredRoutePaths() []string {
	routes := optionalFrontierT6Routes()
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			paths = append(paths, route.Path)
		}
	}
	return paths
}

func optionalFrontierT6BoundarySurfaces() []contextBoundarySurface {
	routes := optionalFrontierT6Routes()
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

func optionalFrontierT6OwnedRoutes() []nativeOwnedRoute {
	routes := optionalFrontierT6Routes()
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

func registerOptionalFrontierT6Routes(mux *http.ServeMux, s *server) {
	for _, route := range optionalFrontierT6Routes() {
		if route.Path == "" || route.Handler == nil {
			continue
		}
		handler := route.Handler
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(s, w, r)
		})
	}
	registerOptionalFrontierT7Routes(mux, s)
}

func optionalFrontierT5RequiredRoutePaths() []string {
	routes := optionalFrontierT5Routes()
	paths := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Path != "" {
			paths = append(paths, route.Path)
		}
	}
	return paths
}

func optionalFrontierT5BoundarySurfaces() []contextBoundarySurface {
	routes := optionalFrontierT5Routes()
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

func optionalFrontierT5OwnedRoutes() []nativeOwnedRoute {
	routes := optionalFrontierT5Routes()
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

func registerOptionalFrontierT5Routes(mux *http.ServeMux, s *server) {
	for _, route := range optionalFrontierT5Routes() {
		if route.Path == "" || route.Handler == nil {
			continue
		}
		handler := route.Handler
		mux.HandleFunc(route.Path, func(w http.ResponseWriter, r *http.Request) {
			handler(s, w, r)
		})
	}
	registerOptionalFrontierT6Routes(mux, s)
}

func (s *server) lockOptionalFrontierT1ProjectBoundary() func() {
	unlock := optionalFrontierT1ProjectBoundaryLock(s)
	if unlock == nil {
		return func() {}
	}
	return unlock
}

func (s *server) enforceOptionalFrontierT1ProjectBoundary(w http.ResponseWriter, r *http.Request, operationClass string) bool {
	return optionalFrontierT1ProjectBoundary(s, w, r, operationClass)
}
