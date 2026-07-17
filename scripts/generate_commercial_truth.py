#!/usr/bin/env python3
"""Generate commercial plan and release projections from one canonical contract."""

from __future__ import annotations

import argparse
import copy
import hashlib
import html
import json
import re
import sys
from pathlib import Path
from typing import Any, Callable


CONTRACT_PATH = Path("config/commercial_truth.v1.json")
PUBLIC_JSON_PATH = Path("docs/public_overview/commercial-truth.json")
TYPESCRIPT_PATH = Path("contextlattice-dashboard/lib/billing/commercial.generated.ts")
GO_PATH = Path("services/gateway-go/commercial_contract_generated.go")
ENTITLEMENT_POLICY_PATH = Path("services/gateway-go/entitlement_policy.go")
STATIC_PAGE_PATHS = (
    Path("docs/public_overview/premium.html"),
    Path("docs/public_overview/index.html"),
    Path("docs/public_overview/index-orb-white.html"),
    Path("docs/public_overview/llms.txt"),
    Path("docs/public_overview/cli.html"),
    Path("docs/public_overview/installation.html"),
)

HTML_BEGIN = "<!-- BEGIN GENERATED COMMERCIAL TRUTH: {name} -->"
HTML_END = "<!-- END GENERATED COMMERCIAL TRUTH: {name} -->"
TEXT_BEGIN = "# BEGIN GENERATED COMMERCIAL TRUTH: {name}"
TEXT_END = "# END GENERATED COMMERCIAL TRUTH: {name}"


class ContractError(ValueError):
    pass


def canonical_json_bytes(payload: Any) -> bytes:
    return json.dumps(payload, ensure_ascii=True, separators=(",", ":"), sort_keys=True).encode("utf-8")


def contract_sha256(contract: dict[str, Any]) -> str:
    return hashlib.sha256(canonical_json_bytes(contract)).hexdigest()


def entitlement_contract(contract: dict[str, Any]) -> dict[str, Any]:
    """Return runtime billing/entitlement truth without release-proof posture."""
    payload = copy.deepcopy(contract)
    payload.pop("release_availability", None)
    return payload


def load_contract(root: Path) -> dict[str, Any]:
    path = root / CONTRACT_PATH
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ContractError(f"cannot load {CONTRACT_PATH}: {exc}") from exc
    if not isinstance(payload, dict):
        raise ContractError("commercial contract must be a JSON object")
    validate_contract(payload)
    return payload


def require_string(obj: dict[str, Any], key: str, where: str) -> str:
    value = obj.get(key)
    if not isinstance(value, str) or not value.strip():
        raise ContractError(f"{where}.{key} must be a non-empty string")
    return value.strip()


def validate_optional_limit(value: Any, where: str) -> None:
    if value is None:
        return
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ContractError(f"{where} must be null or a non-negative integer")


def validate_contract(contract: dict[str, Any]) -> None:
    if contract.get("schema_id") != "contextlattice_commercial_truth.v1":
        raise ContractError("schema_id must be contextlattice_commercial_truth.v1")
    if contract.get("schema_version") != 1 or contract.get("contract_version") != 1:
        raise ContractError("schema_version and contract_version must both be 1")

    product = contract.get("product")
    if not isinstance(product, dict):
        raise ContractError("product must be an object")
    version = require_string(product, "version", "product")
    if not re.fullmatch(r"\d+\.\d+\.\d+", version):
        raise ContractError("product.version must be strict semver without a prefix")
    if require_string(product, "stable_tag", "product") != f"v{version}":
        raise ContractError("product.stable_tag must equal v<product.version>")
    expected_train = ".".join(version.split(".")[:2])
    if require_string(product, "release_train", "product") != expected_train:
        raise ContractError("product.release_train must equal the major.minor version")
    if product.get("primary_interface") != "cli":
        raise ContractError("product.primary_interface must be cli")
    if product.get("python_role") != "build_and_development_tooling_only":
        raise ContractError("Python must be declared as build and development tooling only")

    currency = contract.get("currency")
    if currency != "USD":
        raise ContractError("currency must be USD")

    features = contract.get("features")
    if not isinstance(features, list) or not features:
        raise ContractError("features must be a non-empty array")
    feature_ids: set[str] = set()
    for index, feature in enumerate(features):
        where = f"features[{index}]"
        if not isinstance(feature, dict):
            raise ContractError(f"{where} must be an object")
        feature_id = require_string(feature, "id", where)
        if not re.fullmatch(r"[a-z][a-z0-9_]*", feature_id):
            raise ContractError(f"{where}.id must be a stable snake_case identifier")
        if feature_id in feature_ids:
            raise ContractError(f"duplicate feature id: {feature_id}")
        feature_ids.add(feature_id)
        require_string(feature, "buyer_label", where)
        require_string(feature, "description", where)

    release_availability = contract.get("release_availability")
    if not isinstance(release_availability, dict):
        raise ContractError("release_availability must be an object")
    frontier_feature_ids = {feature_id for feature_id in feature_ids if feature_id.startswith("frontier_")}
    if set(release_availability) != frontier_feature_ids:
        raise ContractError(
            "release_availability must exactly cover Frontier features: "
            f"{sorted(frontier_feature_ids)}"
        )
    allowed_release_postures = {
        ("controlled_activation_preview", "IN_PROGRESS", "UNPROVEN", False),
        ("generally_available", "PASS", "PROVEN", True),
    }
    for feature_id, posture in release_availability.items():
        where = f"release_availability.{feature_id}"
        if not isinstance(posture, dict):
            raise ContractError(f"{where} must be an object")
        observed = (
            require_string(posture, "availability", where),
            require_string(posture, "release_gate", where),
            require_string(posture, "release_decision", where),
            posture.get("production_proven"),
        )
        if observed not in allowed_release_postures:
            raise ContractError(f"{where} contains an unsupported or contradictory release posture: {observed!r}")

    plans = contract.get("plans")
    if not isinstance(plans, list) or not plans:
        raise ContractError("plans must be a non-empty array")
    plan_ids: set[str] = set()
    paid_plan_ids: set[str] = set()
    for index, plan in enumerate(plans):
        where = f"plans[{index}]"
        if not isinstance(plan, dict):
            raise ContractError(f"{where} must be an object")
        plan_id = require_string(plan, "id", where)
        if not re.fullmatch(r"[a-z][a-z0-9_-]*", plan_id):
            raise ContractError(f"{where}.id must be a stable lowercase identifier")
        if plan_id in plan_ids:
            raise ContractError(f"duplicate plan id: {plan_id}")
        plan_ids.add(plan_id)
        require_string(plan, "buyer_label", where)
        require_string(plan, "description", where)
        if not isinstance(plan.get("paid"), bool) or not isinstance(plan.get("self_serve_purchasable"), bool):
            raise ContractError(f"{where} paid and self_serve_purchasable must be booleans")
        if plan["paid"]:
            paid_plan_ids.add(plan_id)

        pricing = plan.get("pricing")
        if not isinstance(pricing, dict) or not isinstance(pricing.get("custom"), bool):
            raise ContractError(f"{where}.pricing must declare custom as a boolean")
        monthly = pricing.get("monthly_usd")
        annual = pricing.get("annual_usd")
        if pricing["custom"]:
            if monthly is not None or annual is not None or plan["self_serve_purchasable"]:
                raise ContractError(f"{where} custom pricing must be null and not self-serve")
        else:
            for key, value in (("monthly_usd", monthly), ("annual_usd", annual)):
                if isinstance(value, bool) or not isinstance(value, int) or value < 0:
                    raise ContractError(f"{where}.pricing.{key} must be a non-negative integer")

        limits = plan.get("limits")
        if not isinstance(limits, dict):
            raise ContractError(f"{where}.limits must be an object")
        expected_limits = {"max_api_keys", "max_projects", "max_write_bytes", "included_seats"}
        if set(limits) != expected_limits:
            raise ContractError(f"{where}.limits must contain exactly {sorted(expected_limits)}")
        for key, value in limits.items():
            validate_optional_limit(value, f"{where}.limits.{key}")

        assigned = plan.get("feature_ids")
        if not isinstance(assigned, list) or not assigned:
            raise ContractError(f"{where}.feature_ids must be a non-empty array")
        if len(assigned) != len(set(assigned)):
            raise ContractError(f"{where}.feature_ids contains duplicates")
        unknown = sorted(set(assigned) - feature_ids)
        if unknown:
            raise ContractError(f"{where}.feature_ids contains unknown ids: {unknown}")

    aliases = contract.get("aliases")
    if not isinstance(aliases, dict):
        raise ContractError("aliases must be an object")
    exact = aliases.get("exact")
    patterns = aliases.get("patterns")
    if not isinstance(exact, dict) or not isinstance(patterns, list):
        raise ContractError("aliases.exact must be an object and aliases.patterns must be an array")
    for alias, target in exact.items():
        if not isinstance(alias, str) or not alias or target not in plan_ids:
            raise ContractError(f"invalid exact plan alias: {alias!r} -> {target!r}")
    required_exact_aliases = {"pro": "operator", "business": "team"}
    for alias, target in required_exact_aliases.items():
        if exact.get(alias) != target:
            raise ContractError(f"required legacy alias is missing: {alias} -> {target}")
    enterprise_pattern_found = False
    for index, row in enumerate(patterns):
        if not isinstance(row, dict):
            raise ContractError(f"aliases.patterns[{index}] must be an object")
        pattern = require_string(row, "pattern", f"aliases.patterns[{index}]")
        target = require_string(row, "target", f"aliases.patterns[{index}]")
        if target not in plan_ids:
            raise ContractError(f"aliases.patterns[{index}] targets unknown plan {target}")
        try:
            re.compile(pattern)
        except re.error as exc:
            raise ContractError(f"aliases.patterns[{index}] has invalid regex: {exc}") from exc
        if target == "enterprise" and re.fullmatch(pattern, "enterprise-seats-100"):
            enterprise_pattern_found = True
    if not enterprise_pattern_found:
        raise ContractError("a legacy enterprise-seats-* alias pattern is required")

    route_contract = contract.get("paid_route_contract")
    if not isinstance(route_contract, dict):
        raise ContractError("paid_route_contract must be an object")
    if route_contract.get("feature_id") not in feature_ids:
        raise ContractError("paid_route_contract.feature_id must reference a declared feature")
    eligible = route_contract.get("eligible_plan_ids")
    if not isinstance(eligible, list) or not eligible:
        raise ContractError("paid_route_contract.eligible_plan_ids must be a non-empty array")
    if set(eligible) - paid_plan_ids:
        raise ContractError("paid routes may only reference declared paid plans")
    roles = route_contract.get("allowed_roles")
    if not isinstance(roles, list) or not roles or any(not isinstance(role, str) or not role for role in roles):
        raise ContractError("paid_route_contract.allowed_roles must be non-empty strings")
    routes = route_contract.get("routes")
    if not isinstance(routes, list) or not routes or len(routes) != len(set(routes)):
        raise ContractError("paid_route_contract.routes must be a unique, non-empty array")
    for route in routes:
        if not isinstance(route, str) or not route.startswith("/") or " " in route:
            raise ContractError(f"invalid protected route: {route!r}")

    feature_route_contracts = contract.get("paid_feature_route_contracts")
    if not isinstance(feature_route_contracts, list) or not feature_route_contracts:
        raise ContractError("paid_feature_route_contracts must be a non-empty array")
    feature_route_seen: set[str] = set()
    generic_routes = set(routes)
    generic_roles = set(route_contract["allowed_roles"])
    for index, row in enumerate(feature_route_contracts):
        where = f"paid_feature_route_contracts[{index}]"
        if not isinstance(row, dict):
            raise ContractError(f"{where} must be an object")
        feature_id = require_string(row, "feature_id", where)
        if feature_id not in feature_ids or feature_id == route_contract.get("feature_id"):
            raise ContractError(f"{where}.feature_id must reference a distinct declared feature")
        row_eligible = row.get("eligible_plan_ids")
        expected_eligible = {plan["id"] for plan in plans if feature_id in plan["feature_ids"]}
        if not isinstance(row_eligible, list) or set(row_eligible) != expected_eligible or set(row_eligible) - paid_plan_ids:
            raise ContractError(f"{where}.eligible_plan_ids must exactly match paid plans carrying {feature_id}")
        row_roles = row.get("allowed_roles")
        if not isinstance(row_roles, list) or not row_roles or any(not isinstance(role, str) or not role for role in row_roles):
            raise ContractError(f"{where}.allowed_roles must be non-empty strings")
        if not set(row_roles).issubset(generic_roles):
            raise ContractError(f"{where}.allowed_roles must be a subset of paid_route_contract.allowed_roles")
        row_routes = row.get("routes")
        if not isinstance(row_routes, list) or not row_routes or len(row_routes) != len(set(row_routes)):
            raise ContractError(f"{where}.routes must be a unique, non-empty array")
        for route in row_routes:
            if route not in generic_routes:
                raise ContractError(f"{where} route is absent from paid_route_contract.routes: {route!r}")
            if route in feature_route_seen:
                raise ContractError(f"feature-specific paid route is assigned more than once: {route!r}")
            feature_route_seen.add(route)


def public_payload(contract: dict[str, Any]) -> dict[str, Any]:
    payload = copy.deepcopy(contract)
    payload["contract_sha256"] = contract_sha256(entitlement_contract(contract))
    payload["commercial_truth_sha256"] = contract_sha256(contract)
    serialized = json.dumps(payload, ensure_ascii=True)
    forbidden = ("/Users/", "/Volumes/", "file://", "~/.", "BEGIN PRIVATE KEY", "password=")
    for token in forbidden:
        if token.lower() in serialized.lower():
            raise ContractError(f"public commercial truth contains forbidden private or secret token: {token}")
    return payload


def entitlement_payload(contract: dict[str, Any]) -> dict[str, Any]:
    payload = entitlement_contract(contract)
    payload["contract_sha256"] = contract_sha256(payload)
    return payload


def go_runtime_payload(contract: dict[str, Any], include_entitled_runtime: bool) -> dict[str, Any]:
    """Keep buyer truth public without compiling absent paid features into the OSS gateway."""
    payload = entitlement_payload(contract)
    if include_entitled_runtime:
        return payload

    omitted_features = {
        "frontier_delta_packet_automation",
        "frontier_shared_proof_timeline",
    }
    omitted_routes = {
        "/memory/agent-packet/shared",
        "/memory/agent-proof-timeline/shared",
        "/memory/agent-proof-timeline/shared/lifecycle",
    }
    payload["features"] = [
        feature for feature in payload["features"] if feature["id"] not in omitted_features
    ]
    for plan in payload["plans"]:
        plan["feature_ids"] = [
            feature_id for feature_id in plan["feature_ids"] if feature_id not in omitted_features
        ]
    payload["paid_route_contract"]["routes"] = [
        route for route in payload["paid_route_contract"]["routes"] if route not in omitted_routes
    ]
    payload["paid_feature_route_contracts"] = [
        row for row in payload["paid_feature_route_contracts"] if row["feature_id"] not in omitted_features
    ]
    return payload


def render_typescript(contract: dict[str, Any]) -> str:
    payload = entitlement_payload(contract)
    plan_ids = " | ".join(json.dumps(plan["id"]) for plan in contract["plans"])
    serialized = json.dumps(payload, ensure_ascii=True, indent=2)
    return f'''// Code generated by scripts/generate_commercial_truth.py; DO NOT EDIT.

export type CommercialPlanId = {plan_ids};

export const COMMERCIAL_CONTRACT_SHA256 = {json.dumps(payload["contract_sha256"])};
export const COMMERCIAL_TRUTH = {serialized} as const;
export const COMMERCIAL_PLANS = COMMERCIAL_TRUTH.plans;

const commercialPlanIds = new Set<string>(COMMERCIAL_PLANS.map((plan) => plan.id));
const commercialPlanAliases = COMMERCIAL_TRUTH.aliases.exact as Record<string, CommercialPlanId>;

export function normalizeCommercialPlanId(input: unknown): CommercialPlanId | null {{
  const raw = String(input || "").trim().toLowerCase();
  if (!raw) return null;
  if (commercialPlanIds.has(raw)) return raw as CommercialPlanId;
  const exact = commercialPlanAliases[raw];
  if (exact) return exact;
  for (const alias of COMMERCIAL_TRUTH.aliases.patterns) {{
    if (new RegExp(alias.pattern).test(raw)) return alias.target as CommercialPlanId;
  }}
  return null;
}}

export function commercialPlanById(input: unknown) {{
  const planId = normalizeCommercialPlanId(input);
  return planId ? COMMERCIAL_PLANS.find((plan) => plan.id === planId) || null : null;
}}
'''


def go_string(value: str) -> str:
    return json.dumps(value, ensure_ascii=True)


def go_optional_int(value: int | None) -> str:
    return "nil" if value is None else f"commercialTruthInt({value})"


def go_string_slice(values: list[str]) -> str:
    return "[]string{" + ", ".join(go_string(value) for value in values) + "}"


def render_go(contract: dict[str, Any], include_entitled_runtime: bool) -> str:
    payload = go_runtime_payload(contract, include_entitled_runtime)
    plan_rows: list[str] = []
    plan_key_width = max(len(go_string(plan["id"]) + ":") for plan in payload["plans"])
    for plan in payload["plans"]:
        pricing = plan["pricing"]
        limits = plan["limits"]
        key = go_string(plan["id"]) + ":"
        plan_rows.append(
            "\t"
            + key.ljust(plan_key_width + 1)
            + "{"
            + f"ID: {go_string(plan['id'])}, BuyerLabel: {go_string(plan['buyer_label'])}, "
            + f"Description: {go_string(plan['description'])}, Paid: {str(plan['paid']).lower()}, "
            + f"SelfServePurchasable: {str(plan['self_serve_purchasable']).lower()}, "
            + f"MonthlyUSD: {go_optional_int(pricing['monthly_usd'])}, AnnualUSD: {go_optional_int(pricing['annual_usd'])}, "
            + f"CustomPricing: {str(pricing['custom']).lower()}, "
            + "Limits: commercialTruthLimits{"
            + f"MaxAPIKeys: {go_optional_int(limits['max_api_keys'])}, MaxProjects: {go_optional_int(limits['max_projects'])}, "
            + f"MaxWriteBytes: {go_optional_int(limits['max_write_bytes'])}, IncludedSeats: {go_optional_int(limits['included_seats'])}"
            + "}, "
            + f"FeatureIDs: {go_string_slice(plan['feature_ids'])}"
            + "},"
        )
    alias_width = max(len(go_string(alias) + ":") for alias in payload["aliases"]["exact"])
    alias_rows = [
        "\t" + (go_string(alias) + ":").ljust(alias_width + 1) + go_string(target) + ","
        for alias, target in payload["aliases"]["exact"].items()
    ]
    pattern_rows = [
        "\t{Pattern: regexp.MustCompile(" + go_string(row["pattern"]) + "), Target: " + go_string(row["target"]) + "},"
        for row in payload["aliases"]["patterns"]
    ]
    route_rows = [f"\t{go_string(route)}," for route in payload["paid_route_contract"]["routes"]]
    feature_routes = [
        (route, row)
        for row in payload["paid_feature_route_contracts"]
        for route in row["routes"]
    ]
    feature_route_key_width = max(len(go_string(route) + ":") for route, _ in feature_routes)
    feature_route_rows = [
        "\t"
        + (go_string(route) + ":").ljust(feature_route_key_width + 1)
        + "{FeatureID: "
        + go_string(row["feature_id"])
        + ", EligiblePlanIDs: "
        + go_string_slice(row["eligible_plan_ids"])
        + ", AllowedRoles: "
        + go_string_slice(row["allowed_roles"])
        + "},"
        for route, row in feature_routes
    ]
    return f'''// Code generated by scripts/generate_commercial_truth.py; DO NOT EDIT.

package main

import (
\t"regexp"
\t"strings"
)

const commercialTruthSchemaID = {go_string(payload["schema_id"])}
const commercialTruthProductVersion = {go_string(payload["product"]["version"])}
const commercialTruthStableTag = {go_string(payload["product"]["stable_tag"])}
const commercialTruthReleaseTrain = {go_string(payload["product"]["release_train"])}
const commercialTruthContractSHA256 = {go_string(payload["contract_sha256"])}

type commercialTruthLimits struct {{
\tMaxAPIKeys    *int
\tMaxProjects   *int
\tMaxWriteBytes *int
\tIncludedSeats *int
}}

type commercialTruthPlan struct {{
\tID                   string
\tBuyerLabel           string
\tDescription          string
\tPaid                 bool
\tSelfServePurchasable bool
\tMonthlyUSD           *int
\tAnnualUSD            *int
\tCustomPricing        bool
\tLimits               commercialTruthLimits
\tFeatureIDs           []string
}}

type commercialTruthAliasPattern struct {{
\tPattern *regexp.Regexp
\tTarget  string
}}

type commercialTruthFeatureRouteRequirement struct {{
	FeatureID       string
	EligiblePlanIDs []string
	AllowedRoles    []string
}}

func commercialTruthInt(value int) *int {{ return &value }}

var commercialTruthPlans = map[string]commercialTruthPlan{{
{chr(10).join(plan_rows)}
}}

var commercialTruthPlanAliases = map[string]string{{
{chr(10).join(alias_rows)}
}}

var commercialTruthPlanAliasPatterns = []commercialTruthAliasPattern{{
{chr(10).join(pattern_rows)}
}}

var commercialTruthProtectedPaidRoutes = []string{{
{chr(10).join(route_rows)}
}}

var commercialTruthPaidRouteEligiblePlans = {go_string_slice(contract["paid_route_contract"]["eligible_plan_ids"])}
var commercialTruthPaidRouteAllowedRoles = {go_string_slice(contract["paid_route_contract"]["allowed_roles"])}

var commercialTruthPaidFeatureRouteRequirements = map[string]commercialTruthFeatureRouteRequirement{{
{chr(10).join(feature_route_rows)}
}}

func commercialTruthPaidRouteRequiredFeature(path string) string {{
	return commercialTruthPaidFeatureRouteRequirements[strings.TrimSpace(path)].FeatureID
}}

func normalizeCommercialTruthPlanID(raw string) string {{
\tplan := strings.ToLower(strings.TrimSpace(raw))
\tif plan == "" {{
\t\treturn ""
\t}}
\tif _, ok := commercialTruthPlans[plan]; ok {{
\t\treturn plan
\t}}
\tif target, ok := commercialTruthPlanAliases[plan]; ok {{
\t\treturn target
\t}}
\tfor _, alias := range commercialTruthPlanAliasPatterns {{
\t\tif alias.Pattern.MatchString(plan) {{
\t\t\treturn alias.Target
\t\t}}
\t}}
\treturn ""
}}
'''


def marker_tokens(name: str, text_style: bool = False) -> tuple[str, str]:
    if text_style:
        return TEXT_BEGIN.format(name=name), TEXT_END.format(name=name)
    return HTML_BEGIN.format(name=name), HTML_END.format(name=name)


def marker_block(name: str, body: str, indent: str = "", text_style: bool = False) -> str:
    begin, end = marker_tokens(name, text_style=text_style)
    lines = [f"{indent}{begin}"]
    lines.extend(f"{indent}{line}" if line else "" for line in body.splitlines())
    lines.append(f"{indent}{end}")
    return "\n".join(lines)


def replace_existing_marker(text: str, name: str, replacement: str, text_style: bool = False) -> tuple[str, bool]:
    begin, end = marker_tokens(name, text_style=text_style)
    pattern = re.compile(
        r"^[ \t]*" + re.escape(begin) + r".*?^[ \t]*" + re.escape(end),
        re.DOTALL | re.MULTILINE,
    )
    if not pattern.search(text):
        return text, False
    return pattern.sub(replacement, text, count=1), True


def replace_or_bootstrap(
    text: str,
    name: str,
    replacement: str,
    bootstrap: Callable[[str, str], str],
    *,
    text_style: bool = False,
) -> str:
    updated, found = replace_existing_marker(text, name, replacement, text_style=text_style)
    return updated if found else bootstrap(text, replacement)


def replace_once(text: str, pattern: str, replacement: str, description: str, flags: int = 0) -> str:
    updated, count = re.subn(pattern, lambda _: replacement, text, count=1, flags=flags)
    if count != 1:
        raise ContractError(f"cannot bootstrap generated marker for {description}")
    return updated


def feature_labels(contract: dict[str, Any]) -> dict[str, str]:
    labels: dict[str, str] = {}
    availability = contract["release_availability"]
    for feature in contract["features"]:
        label = feature["buyer_label"]
        posture = availability.get(feature["id"], {})
        if posture.get("availability") == "controlled_activation_preview":
            label += " (controlled preview)"
        labels[feature["id"]] = label
    return labels


def plan_price_label(plan: dict[str, Any]) -> str:
    if plan["pricing"]["custom"]:
        return "Custom"
    monthly = plan["pricing"]["monthly_usd"]
    annual = plan["pricing"]["annual_usd"]
    if monthly == 0 and annual == 0:
        return "$0"
    return f"${monthly}/mo or ${annual}/yr"


def render_plan_rows(contract: dict[str, Any]) -> str:
    labels = feature_labels(contract)
    rows: list[str] = []
    for plan in contract["plans"]:
        included = ", ".join(labels[feature_id] for feature_id in plan["feature_ids"])
        rows.extend(
            [
                "<tr>",
                f"  <td><strong>{html.escape(plan['buyer_label'])}</strong></td>",
                f"  <td>{html.escape(plan_price_label(plan))}</td>",
                f"  <td>{html.escape(plan['description'])}</td>",
                f"  <td>{html.escape(included)}</td>",
                "</tr>",
            ]
        )
    return "\n".join(rows)


def render_capability_rows(contract: dict[str, Any]) -> str:
    plans = contract["plans"]
    free_plan = next(plan for plan in plans if plan["id"] == "free")
    free_features = set(free_plan["feature_ids"])
    rows: list[str] = []
    for feature in contract["features"]:
        feature_id = feature["id"]
        eligible = [plan["buyer_label"] for plan in plans if plan["paid"] and feature_id in plan["feature_ids"]]
        public_label = "Included" if feature_id in free_features else "Not included"
        posture = contract["release_availability"].get(feature_id, {})
        if posture.get("availability") == "controlled_activation_preview":
            premium_label = "Controlled activation preview: " + ", ".join(eligible)
        else:
            premium_label = "Included" if len(eligible) == len([plan for plan in plans if plan["paid"]]) else ", ".join(eligible)
        rows.extend(
            [
                "<tr>",
                f"  <td><strong>{html.escape(feature['buyer_label'])}</strong><br><span class=\"muted-note\">{html.escape(feature['description'])}</span></td>",
                f"  <td>{public_label}</td>",
                f"  <td>{html.escape(premium_label or 'Not included')}</td>",
                "</tr>",
            ]
        )
    return "\n".join(rows)


def insert_after_pattern(text: str, pattern: str, block: str, description: str) -> str:
    match = re.search(pattern, text)
    if not match:
        raise ContractError(f"cannot bootstrap generated marker for {description}")
    return text[: match.end()] + "\n" + block + text[match.end() :]


def update_premium(text: str, contract: dict[str, Any]) -> str:
    version = contract["product"]["version"]
    intro = marker_block(
        "premium-release",
        f'<p class="muted-note">Current release: <code>v{version}</code>. The CLI is the primary product interface; the dashboard is the account and visibility companion.</p>',
        indent="      ",
    )
    text = replace_or_bootstrap(
        text,
        "premium-release",
        intro,
        lambda value, block: insert_after_pattern(
            value,
            r'      <p class="muted-note">Hosted target surface: <code>app\.contextlattice\.io</code></p>',
            block,
            "premium release intro",
        ),
    )

    plans = marker_block("premium-plans", render_plan_rows(contract), indent="            ")
    updated, found = replace_existing_marker(text, "premium-plans", plans)
    if found:
        text = updated
    else:
        anchor = text.find("<h3>Subscription lanes</h3>")
        if anchor < 0:
            raise ContractError("cannot find premium subscription lane table")
        tbody_start = text.find("<tbody>", anchor)
        tbody_end = text.find("</tbody>", tbody_start)
        if tbody_start < 0 or tbody_end < 0:
            raise ContractError("cannot find premium subscription lane tbody")
        content_start = tbody_start + len("<tbody>")
        text = text[:content_start] + "\n" + plans + "\n          " + text[tbody_end:]

    capabilities = marker_block(
        "premium-capabilities",
        render_capability_rows(contract),
        indent="            ",
    )
    updated, found = replace_existing_marker(text, "premium-capabilities", capabilities)
    if found:
        return updated
    anchor = text.find("<h3>Capability deltas</h3>")
    if anchor < 0:
        raise ContractError("cannot find premium capability table")
    tbody_start = text.find("<tbody>", anchor)
    tbody_end = text.find("</tbody>", tbody_start)
    if tbody_start < 0 or tbody_end < 0:
        raise ContractError("cannot find premium capability tbody")
    content_start = tbody_start + len("<tbody>")
    return text[:content_start] + "\n" + capabilities + "\n          " + text[tbody_end:]


def bootstrap_index_card(text: str, title: str, block: str, description: str) -> str:
    pattern = (
        r'(?s)(<h3>'
        + re.escape(title)
        + r'</h3>.*?<ul class="spec-list">[^\S\r\n]*\r?\n)'
        + r'[ \t]*<li><strong>App version lane:</strong>.*?</li>'
    )
    match = re.search(pattern, text)
    if not match:
        raise ContractError(f"cannot bootstrap generated marker for {description}")
    return text[: match.start()] + match.group(1) + block + text[match.end() :]


def update_index(text: str, contract: dict[str, Any], page_key: str) -> str:
    version = contract["product"]["version"]
    train = contract["product"]["release_train"]
    heading = marker_block(
        f"{page_key}-release-heading",
        f"<h3>Public v{train} (current release train)</h3>",
        indent="          ",
    )
    text = replace_or_bootstrap(
        text,
        f"{page_key}-release-heading",
        heading,
        lambda value, block: replace_once(
            value,
            r'          <h3>Public v\d+\.\d+ \(current (?:launch lane|release train)\)</h3>',
            block,
            f"{page_key} release heading",
        ),
    )

    summary_body = "\n".join(
        [
            f'<li><strong>Current public release:</strong> <code>v{version}</code></li>',
            "<li><strong>Primary interface:</strong> CLI workflows for install, context, resume, remember, finish, correct, and doctor; dashboard, HTTP, and MCP are companion surfaces</li>",
        ]
    )
    summary = marker_block(f"{page_key}-release-summary", summary_body, indent="            ")
    text = replace_or_bootstrap(
        text,
        f"{page_key}-release-summary",
        summary,
        lambda value, block: replace_once(
            value,
            r'            <li><strong>Current public release:</strong>.*?</li>\s*'
            r'            <li><strong>Primary interface:</strong>.*?</li>',
            block,
            f"{page_key} release summary",
            flags=re.DOTALL,
        ),
    )

    diagram = marker_block(
        f"{page_key}-release-diagram",
        f'A[Agent or App] --> B["Public Lane: v{train}"]',
        indent="  ",
    )
    text = replace_or_bootstrap(
        text,
        f"{page_key}-release-diagram",
        diagram,
        lambda value, block: replace_once(
            value,
            r'  A\[Agent or App\] --> B\["Public Lane: v\d+\.\d+"\]',
            block,
            f"{page_key} release diagram",
        ),
    )

    card_specs = (
        ("hf", "Hugging Face / Glama lite", f'<li><strong>App version lane:</strong> Public <code>v{train}.x</code></li>'),
        ("lite", "Lite mode", f'<li><strong>App version lane:</strong> Public <code>v{train}.x</code></li>'),
        ("full", "Full mode", f'<li><strong>App version lane:</strong> Public <code>v{train}.x</code> Full</li>'),
    )
    for suffix, title, body in card_specs:
        name = f"{page_key}-deployment-{suffix}"
        block = marker_block(name, body, indent="            ")
        text = replace_or_bootstrap(
            text,
            name,
            block,
            lambda value, replacement, title=title, name=name: bootstrap_index_card(
                value, title, replacement, name
            ),
        )
    return text


def update_llms(text: str, contract: dict[str, Any]) -> str:
    version = contract["product"]["version"]
    body = (
        f"- Current public release baseline is v{version}. The CLI is the primary product interface; "
        "the dashboard is the visibility and account companion, with HTTP and MCP available for programmatic integration."
    )
    block = marker_block("llms-release", body, text_style=True)
    return replace_or_bootstrap(
        text,
        "llms-release",
        block,
        lambda value, replacement: replace_once(
            value,
            r'- Current public release baseline is v\d+\.\d+\.\d+:.*',
            replacement,
            "llms release",
        ),
        text_style=True,
    )


def update_cli(text: str, contract: dict[str, Any]) -> str:
    version = contract["product"]["version"]
    summary = marker_block(
        "cli-release-summary",
        f'<p class="muted-note">Current release: <code>v{version}</code>. The CLI is the primary product interface; dashboard, HTTP, and MCP remain companion surfaces.</p>',
        indent="      ",
    )
    text = replace_or_bootstrap(
        text,
        "cli-release-summary",
        summary,
        lambda value, replacement: insert_after_pattern(
            value,
            r'      <p class="sub">Use these commands as the canonical local flow.*?</p>',
            replacement,
            "CLI release summary",
        ),
    )
    body = (
        "contextlattice_claim_write --project contextlattice --subject release "
        f"--predicate current_version --object {version} --pretty"
    )
    block = marker_block("cli-release-claim", body, indent="")
    return replace_or_bootstrap(
        text,
        "cli-release-claim",
        block,
        lambda value, replacement: replace_once(
            value,
            r'contextlattice_claim_write --project contextlattice --subject release '
            r'--predicate current_version --object \d+\.\d+\.\d+ --pretty',
            replacement,
            "CLI release claim",
        ),
    )


def update_installation(text: str, contract: dict[str, Any]) -> str:
    train = contract["product"]["release_train"]
    sizing_body = "\n".join(
        [
            f'<li><strong>Public <code>v{train}.x</code> Hugging Face / Glama lite:</strong> <code>2-4</code> vCPU, <code>4-8 GB</code> RAM, <code>20-50 GB</code> SSD</li>',
            f'<li><strong>Public <code>v{train}.x</code> local Lite:</strong> <code>2-4</code> vCPU, <code>8-12 GB</code> RAM, <code>25-80 GB</code> SSD</li>',
            f'<li><strong>Public <code>v{train}.x</code> local Lite advanced:</strong> <code>4-6</code> vCPU, <code>12-16 GB</code> RAM, <code>80-140 GB</code> SSD</li>',
            f'<li><strong>Public <code>v{train}.x</code> local Full (no spike-lab):</strong> <code>6-8</code> vCPU, <code>12-20 GB</code> RAM, <code>100-180 GB</code> SSD</li>',
            f'<li><strong>Public <code>v{train}.x</code> local Full + spike-lab:</strong> <code>8-12</code> vCPU, <code>24-32 GB</code> RAM, <code>180-300 GB</code> SSD/NVMe</li>',
        ]
    )
    sizing = marker_block("installation-sizing-release", sizing_body, indent="          ")
    text = replace_or_bootstrap(
        text,
        "installation-sizing-release",
        sizing,
        lambda value, replacement: replace_once(
            value,
            r'          <li><strong>Public <code>v\d+\.\d+\.x</code> Hugging Face / Glama lite:.*?'
            r'          <li><strong>Public <code>v\d+\.\d+\.x</code> local Full \+ spike-lab:.*?</li>',
            replacement,
            "installation sizing release",
            flags=re.DOTALL,
        ),
    )

    card_specs = (
        ("hf", "Hugging Face / Glama lite", f'<li><strong>App version lane:</strong> Public <code>v{train}.x</code></li>'),
        ("lite", "Lite core installation", f'<li><strong>App version lane:</strong> Public <code>v{train}.x</code></li>'),
        ("full", "Full installation", f'<li><strong>App version lane:</strong> Public <code>v{train}.x</code> Full</li>'),
    )
    for suffix, title, body in card_specs:
        name = f"installation-deployment-{suffix}"
        block = marker_block(name, body, indent="            ")
        text = replace_or_bootstrap(
            text,
            name,
            block,
            lambda value, replacement, title=title, name=name: bootstrap_index_card(
                value, title, replacement, name
            ),
        )
    return text


def render_static_page(path: Path, text: str, contract: dict[str, Any]) -> str:
    if path.name == "premium.html":
        return update_premium(text, contract)
    if path.name == "index.html":
        return update_index(text, contract, "index")
    if path.name == "index-orb-white.html":
        return update_index(text, contract, "index-orb-white")
    if path.name == "llms.txt":
        return update_llms(text, contract)
    if path.name == "cli.html":
        return update_cli(text, contract)
    if path.name == "installation.html":
        return update_installation(text, contract)
    raise ContractError(f"unsupported static page: {path}")


def expected_outputs(root: Path, contract: dict[str, Any]) -> dict[Path, str]:
    payload = public_payload(contract)
    outputs: dict[Path, str] = {
        root / PUBLIC_JSON_PATH: json.dumps(payload, ensure_ascii=True, indent=2) + "\n",
        root / TYPESCRIPT_PATH: render_typescript(contract),
        root / GO_PATH: render_go(contract, include_entitled_runtime=(root / ENTITLEMENT_POLICY_PATH).is_file()),
    }
    for relative_path in STATIC_PAGE_PATHS:
        path = root / relative_path
        try:
            current = path.read_text(encoding="utf-8")
        except OSError as exc:
            raise ContractError(f"cannot read static page {relative_path}: {exc}") from exc
        outputs[path] = render_static_page(relative_path, current, contract)
    return outputs


def generate(root: Path, check: bool) -> tuple[bool, list[str]]:
    contract = load_contract(root)
    outputs = expected_outputs(root, contract)
    drift: list[str] = []
    for path, expected in outputs.items():
        current = path.read_text(encoding="utf-8") if path.exists() else None
        if current == expected:
            continue
        relative = str(path.relative_to(root))
        drift.append(relative)
        if not check:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(expected, encoding="utf-8")
    return not drift, drift


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true", help="Fail if generated files or marker blocks drift.")
    parser.add_argument("--root", default="", help="Repository root; defaults to the script parent repository.")
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    root = Path(args.root).expanduser().resolve() if args.root else Path(__file__).resolve().parents[1]
    try:
        clean, drift = generate(root, check=args.check)
    except ContractError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}), file=sys.stderr)
        return 1
    payload = {
        "ok": clean if args.check else True,
        "mode": "check" if args.check else "write",
        "contract": str(CONTRACT_PATH),
        "drift": drift,
    }
    stream = sys.stdout if payload["ok"] else sys.stderr
    print(json.dumps(payload, sort_keys=True), file=stream)
    return 0 if payload["ok"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
