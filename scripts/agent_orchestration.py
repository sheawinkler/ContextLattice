#!/usr/bin/env python3
"""
Agent orchestration helper for ContextLattice.
Enables multi-agent coordination through shared memory + task tracking.
"""

import json
import os
import sys
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, Dict, List, Optional
from urllib.parse import quote, urljoin

try:
    from scripts.contextlattice_client import (
        DEFAULT_ORCHESTRATOR_URL,
        ContextLatticeClient,
    )
    from scripts.agent_contracts import (
        anti_scheming_protocol,
        contract_metadata,
        enforce_contract_limits,
        load_agent_contracts_registry,
        preflight_contracts_summary,
        stamp_validation,
        validate_agent_contract_payload,
    )
except ModuleNotFoundError:  # pragma: no cover - fallback when run from scripts/ root
    from contextlattice_client import (  # type: ignore[no-redef]
        DEFAULT_ORCHESTRATOR_URL,
        ContextLatticeClient,
    )
    from agent_contracts import (  # type: ignore[no-redef]
        anti_scheming_protocol,
        contract_metadata,
        enforce_contract_limits,
        load_agent_contracts_registry,
        preflight_contracts_summary,
        stamp_validation,
        validate_agent_contract_payload,
    )
DEFAULT_AGENT_ID = (
    os.getenv("CONTEXTLATTICE_AGENT_ID", "").strip()
    or os.getenv("MEMMCP_AGENT_ID", "").strip()
    or "codex_gpt5"
)


def json_bytes(value: Any) -> int:
    return len(json.dumps(value, sort_keys=True, separators=(",", ":")).encode("utf-8"))


DEFAULT_AGENT_PREFLIGHT_PROFILES: Dict[str, Dict[str, str]] = {
    "codex": {
        "agent_id": "codex_gpt5",
        "topic_path": "runbooks/codex-integration",
        "query": "codex preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "claude-code": {
        "agent_id": "claude_code_agent",
        "topic_path": "runbooks/claude-code-integration",
        "query": "claude code preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "opencode": {
        "agent_id": "opencode_agent",
        "topic_path": "runbooks/opencode-integration",
        "query": "opencode preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "hermes-agent": {
        "agent_id": "hermes_agent",
        "topic_path": "runbooks/hermes-agent-integration",
        "query": "hermes agent preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "chatgpt-web": {
        "agent_id": "chatgpt_web_agent",
        "topic_path": "runbooks/chatgpt-web-integration",
        "query": "chatgpt web session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "chatgpt-desktop": {
        "agent_id": "chatgpt_desktop_agent",
        "topic_path": "runbooks/chatgpt-desktop-integration",
        "query": "chatgpt desktop session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "claude-web": {
        "agent_id": "claude_web_agent",
        "topic_path": "runbooks/claude-web-integration",
        "query": "claude web session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
    "claude-desktop": {
        "agent_id": "claude_desktop_agent",
        "topic_path": "runbooks/claude-desktop-integration",
        "query": "claude desktop session preflight connectivity and retrieval",
        "retrieval_mode": "balanced",
    },
}
AGENT_PREFLIGHT_ALIASES = {
    "codex_gpt5": "codex",
    "claude-code": "claude-code",
    "claude_code": "claude-code",
    "opencode": "opencode",
    "hermes": "hermes-agent",
    "hermes_agent": "hermes-agent",
    "hermes-agent": "hermes-agent",
    "chatgpt": "chatgpt-web",
    "chatgpt-web": "chatgpt-web",
    "chatgpt-desktop": "chatgpt-desktop",
    "claude": "claude-web",
    "claude-web": "claude-web",
    "claude-desktop": "claude-desktop",
}
SKILLS_BASE_DIR = Path.home() / ".codex" / "skills"
DEFAULT_CONTEXTLATTICE_MISSION = (
    os.getenv("CONTEXTLATTICE_MISSION", "").strip()
    or "Compound knowledge across projects into better agent outcomes with less repeated inference."
)
DEFAULT_CONTEXTLATTICE_OBJECTIVE = (
    os.getenv("CONTEXTLATTICE_OBJECTIVE", "").strip()
    or "Improve longitudinal recall, retrieval quality, and orchestration decisions over time."
)
DEFAULT_CONTEXTLATTICE_GOAL = (
    os.getenv("CONTEXTLATTICE_GOAL", "").strip()
    or "Maximize useful context per token while preserving correctness, provenance, and latency discipline."
)


def objective_hierarchy(project: str, topic_path: str, session_id: str, mission: str, objective: str, goal: str) -> dict[str, Any]:
    return {
        "schema_id": "contextlattice_objective_hierarchy.v1",
        "project": {
            "name": str(project or "").strip(),
            "primary_objective": str(objective or "").strip(),
            "mission": str(mission or "").strip(),
        },
        "topic": {
            "path": str(topic_path or "").strip(),
            "objective": str(objective or "").strip(),
        },
        "session": {
            "id": str(session_id or "").strip(),
            "objective": str(objective or "").strip(),
            "goal": str(goal or "").strip(),
        },
        "current": {
            "mission": str(mission or "").strip(),
            "objective": str(objective or "").strip(),
            "goal": str(goal or "").strip(),
        },
    }


def objective_lineage(source: str = "agent_orchestration") -> dict[str, Any]:
    return {
        "schema_id": "contextlattice_objective_lineage.v1",
        "source": source,
        "precedence": ["user_request", "project_primary_objective", "topic_objective", "session_objective"],
        "drift": {
            "status": "not_detected",
            "reason": "offline policy package preserves the default ContextLattice mission, objective, and goal.",
        },
        "handoff_rule": "Carry mission, objective, goal, evidence, risks, and next action into the next bounded agent handoff.",
    }


DEFAULT_COMPACTION_TOPIC_PATH = (
    os.getenv("CONTEXTLATTICE_COMPACTION_TOPIC_PATH", "").strip()
    or "runbooks/context-compaction-handoff"
)
DEFAULT_COMPACTION_QUERY = (
    os.getenv("CONTEXTLATTICE_COMPACTION_QUERY", "").strip()
    or "context compaction handoff mission objective goal blockers next actions"
)
SKILL_AVAILABILITY = {
    "objective": (SKILLS_BASE_DIR / "objective" / "SKILL.md").exists(),
    "goal": (SKILLS_BASE_DIR / "goal" / "SKILL.md").exists(),
    "mission": (SKILLS_BASE_DIR / "mission" / "SKILL.md").exists(),
}


def _load_agent_preflight_profiles() -> Dict[str, Dict[str, str]]:
    profiles: Dict[str, Dict[str, str]] = {
        key: dict(value) for key, value in DEFAULT_AGENT_PREFLIGHT_PROFILES.items()
    }
    profile_path = Path(__file__).resolve().parents[1] / "config" / "agents" / "agent_profiles.json"
    if not profile_path.exists():
        return profiles
    try:
        payload = json.loads(profile_path.read_text(encoding="utf-8"))
    except Exception:
        return profiles
    raw_profiles = payload.get("profiles") if isinstance(payload, dict) else None
    if not isinstance(raw_profiles, dict):
        return profiles
    for raw_key, raw_profile in raw_profiles.items():
        key = str(raw_key or "").strip().lower()
        if not key:
            key = "codex"
        key = AGENT_PREFLIGHT_ALIASES.get(key, key)
        if not isinstance(raw_profile, dict):
            continue
        current = dict(profiles.get(key) or {})
        for field in ("agent_id", "topic_path", "query", "retrieval_mode"):
            value = str(raw_profile.get(field, "")).strip()
            if value:
                current[field] = value
        if current:
            profiles[key] = current
    return profiles


AGENT_PREFLIGHT_PROFILES = _load_agent_preflight_profiles()


def _normalize_agent_profile_key(agent: Optional[str]) -> str:
    candidate = str(agent or "").strip().lower()
    if not candidate:
        return "codex"
    return AGENT_PREFLIGHT_ALIASES.get(candidate, candidate)


def _resolve_agent_preflight_profile(agent: Optional[str]) -> tuple[str, Dict[str, str]]:
    key = _normalize_agent_profile_key(agent)
    profile = AGENT_PREFLIGHT_PROFILES.get(key)
    if not profile:
        key = "codex"
        profile = AGENT_PREFLIGHT_PROFILES.get(key) or DEFAULT_AGENT_PREFLIGHT_PROFILES[key]
    return key, dict(profile)


class ContextLatticeOrchestrator:
    """Helper for agent coordination via ContextLattice."""

    def __init__(
        self,
        orchestrator_url: str = DEFAULT_ORCHESTRATOR_URL,
        agent_id: str = DEFAULT_AGENT_ID,
    ):
        self.base_url = orchestrator_url.rstrip("/")
        self.agent_id = str(agent_id or "").strip() or DEFAULT_AGENT_ID
        self.http = ContextLatticeClient(
            base_url=self.base_url,
            timeout=30.0,
            role="orchestrator",
        )
        self.client = self.http.client

    def _encode_project_path(self, project: str, file_name: str | None = None) -> str:
        encoded_project = quote(project, safe="")
        if not file_name:
            return encoded_project
        cleaned = file_name.lstrip("/")
        parts = [quote(part, safe="") for part in cleaned.split("/") if part]
        return f"{encoded_project}/{'/'.join(parts)}" if parts else encoded_project

    def write(
        self,
        project: str,
        file_name: str,
        content: str,
        topic_path: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Write a file to ContextLattice."""
        payload: Dict[str, Any] = {
            "projectName": project,
            "fileName": file_name,
            "content": content,
        }
        if topic_path:
            payload["topicPath"] = topic_path
        resp = self.client.post(
            f"{self.base_url}/memory/write",
            json=payload,
        )
        resp.raise_for_status()
        return resp.json()

    def read(self, project: str, file_name: str) -> str:
        """Read a file from ContextLattice."""
        path = self._encode_project_path(project, file_name)
        resp = self.client.get(f"{self.base_url}/memory/files/{path}")
        resp.raise_for_status()
        if resp.headers.get("content-type", "").startswith("application/json"):
            return json.dumps(resp.json(), indent=2)
        return resp.text

    def list_files(self, project: str) -> List[str]:
        """List files in a project."""
        encoded_project = self._encode_project_path(project)
        resp = self.client.get(f"{self.base_url}/projects/{encoded_project}/files")
        resp.raise_for_status()
        data = resp.json()
        return data.get("files", [])

    def search(
        self,
        query: str,
        project: Optional[str] = None,
        limit: int = 10,
        fetch_content: bool = False,
    ) -> List[Dict[str, Any]]:
        """Semantic search across memory."""
        payload = self.search_with_lifecycle(
            query=query,
            project=project,
            limit=limit,
            fetch_content=fetch_content,
            wait_for_completion=False,
        )
        return payload.get("results", []) if isinstance(payload, dict) else []

    def _absolute_url(self, path_or_url: str) -> str:
        token = str(path_or_url or "").strip()
        if token.startswith("http://") or token.startswith("https://"):
            return token
        if not token.startswith("/"):
            token = "/" + token
        return urljoin(self.base_url + "/", token.lstrip("/"))

    @staticmethod
    def _lifecycle_summary(payload: Dict[str, Any]) -> Dict[str, Any]:
        lifecycle = payload.get("retrieval_lifecycle") if isinstance(payload, dict) else {}
        lifecycle = lifecycle if isinstance(lifecycle, dict) else {}
        status = str(lifecycle.get("status") or payload.get("status") or "").strip().lower()
        sources = lifecycle.get("sources") if isinstance(lifecycle.get("sources"), dict) else {}
        return {
            "status": status or "unknown",
            "result_state": str(lifecycle.get("result_state") or payload.get("result_state") or "").strip().lower() or None,
            "returned_now": list(sources.get("returned_now") or []),
            "pending": list(sources.get("pending") or payload.get("source_summary", {}).get("pending_sources", []) or []),
            "failed": list(sources.get("failed") or payload.get("source_summary", {}).get("failed_sources", []) or []),
            "timed_out": list(sources.get("timed_out") or payload.get("source_summary", {}).get("timed_out_sources", []) or []),
            "budget_exceeded": list(
                sources.get("budget_exceeded") or payload.get("source_summary", {}).get("budget_exceeded_sources", []) or []
            ),
            "next_actions": list(lifecycle.get("next_actions") or []),
        }

    @staticmethod
    def _extract_fact_summaries(pack: Dict[str, Any], max_items: int = 10) -> List[Dict[str, Any]]:
        """Normalize context-pack facts into compact summaries for policy handoff."""
        if not isinstance(pack, dict):
            return []
        nested_pack = pack.get("context_pack") if isinstance(pack.get("context_pack"), dict) else {}
        raw_facts = pack.get("facts")
        if not isinstance(raw_facts, list):
            raw_facts = nested_pack.get("facts")
        raw_results = pack.get("results")
        if not isinstance(raw_results, list):
            raw_results = nested_pack.get("results")
        if not isinstance(raw_facts, list):
            raw_facts = []
        normalized: List[Dict[str, Any]] = []
        for item in raw_facts:
            text = ""
            source = None
            topic = None
            score = None
            if isinstance(item, dict):
                text = str(item.get("text") or item.get("fact") or item.get("content") or "").strip()
                source = str(item.get("source") or item.get("file") or item.get("path") or "").strip() or None
                topic = str(item.get("topic_path") or item.get("topic") or "").strip() or None
                score = item.get("score")
            elif isinstance(item, str):
                text = item.strip()
            if not text:
                continue
            normalized.append(
                {
                    "text": text,
                    "source": source,
                    "topic_path": topic,
                    "score": score,
                }
            )
            if len(normalized) >= max(1, int(max_items)):
                break
        if normalized:
            return normalized
        if not isinstance(raw_results, list):
            return []
        for item in raw_results:
            if not isinstance(item, dict):
                continue
            summary = str(item.get("summary") or item.get("text") or item.get("content") or "").strip()
            if not summary:
                continue
            normalized.append(
                {
                    "text": summary,
                    "source": str(item.get("source") or item.get("file") or "").strip() or None,
                    "topic_path": str(item.get("topic_path") or item.get("topic") or "").strip() or None,
                    "score": item.get("score"),
                }
            )
            if len(normalized) >= max(1, int(max_items)):
                break
        return normalized

    @staticmethod
    def _build_agent_policy_context_package(
        *,
        agent: str,
        agent_id: str,
        project: str,
        topic_path: str,
        retrieval_mode: str,
        query: str,
        primary_pack: Dict[str, Any],
        mission_pack: Optional[Dict[str, Any]],
        mission_pack_error: Optional[str],
        objective_runtime: Optional[Dict[str, Any]] = None,
        session_id: str = "",
        action_executed: str = "agent.preflight.completed",
    ) -> Dict[str, Any]:
        """Build a portable objective/goal/mission package for downstream agents."""
        mission = DEFAULT_CONTEXTLATTICE_MISSION
        objective = DEFAULT_CONTEXTLATTICE_OBJECTIVE
        goal = DEFAULT_CONTEXTLATTICE_GOAL
        primary_facts = ContextLatticeOrchestrator._extract_fact_summaries(primary_pack, max_items=8)
        mission_facts = ContextLatticeOrchestrator._extract_fact_summaries(
            mission_pack if isinstance(mission_pack, dict) else {}, max_items=8
        )
        registry = load_agent_contracts_registry()
        protocol = anti_scheming_protocol(registry)
        format_contract = contract_metadata("policy_context_package.v1", registry)
        if not isinstance(objective_runtime, dict):
            objective_runtime = ContextLatticeOrchestrator._build_objective_runtime_state(
                agent=agent,
                agent_id=agent_id,
                project=project,
                topic_path=topic_path,
                retrieval_mode=retrieval_mode,
                query=query,
                session_id=session_id,
                action_executed=action_executed,
            )
        hierarchy = objective_runtime.get("objective_hierarchy") if isinstance(objective_runtime, dict) else None
        if not isinstance(hierarchy, dict):
            hierarchy = objective_hierarchy(project, topic_path, session_id, mission, objective, goal)
        lineage = objective_runtime.get("objective_lineage") if isinstance(objective_runtime, dict) else None
        if not isinstance(lineage, dict):
            lineage = objective_lineage("agent_policy_context_package")
        package = {
            "version": "2026-05-10",
            "agent": agent,
            "agent_id": agent_id,
            "project": project,
            "topic_path": topic_path,
            "query": query,
            "retrieval_mode": retrieval_mode,
            "mission": mission,
            "objective": objective,
            "goal": goal,
            "objective_hierarchy": hierarchy,
            "objective_lineage": lineage,
            "skills": {
                "required": ["objective", "goal"],
                "optional": ["mission"],
                "availability": dict(SKILL_AVAILABILITY),
            },
            "policy_contract": {
                "retrieve_before_inference": True,
                "anti_scheming_required": True,
                "objective_runtime_required": True,
                "checkpoint_during_execution": True,
                "final_recency_pass_required": True,
                "include_grounding": True,
                "include_retrieval_debug": True,
                "broaden_scope_on_zero_or_degraded": True,
                "format_validation_required": True,
                "contract_boundary_validated": True,
                "fail_closed_on_contract_violation": True,
            },
            "objective_runtime": objective_runtime,
            "anti_scheming_protocol": protocol,
            "handoff": {
                "disperse_to_agents": True,
                "handoff_prompt": (
                    f"Mission: {mission}\n"
                    f"Objective: {objective}\n"
                    f"Goal: {goal}\n"
                    "Policy: retrieve before inference, checkpoint key decisions, run final recency retrieval, "
                    "and change conclusions to match evidence."
                ),
            },
            "evidence": {
                "primary_facts": primary_facts,
                "mission_facts": mission_facts,
                "mission_pack_error": mission_pack_error,
            },
            "format_contract": format_contract,
        }
        before = json_bytes(package)
        package = enforce_contract_limits("policy_context_package.v1", package, registry)
        after = json_bytes(package)
        contract_findings = validate_agent_contract_payload("anti_scheming_protocol.v1", protocol, registry)
        contract_findings.extend(validate_agent_contract_payload("policy_context_package.v1", package, registry))
        package["format_contract"] = stamp_validation(format_contract, contract_findings, package, before, after)
        previous_counts = package["format_contract"].get("omitted_counts") if isinstance(package.get("format_contract"), dict) else None
        before = json_bytes(package)
        package = enforce_contract_limits("policy_context_package.v1", package, registry)
        after = json_bytes(package)
        contract_findings = validate_agent_contract_payload("anti_scheming_protocol.v1", package.get("anti_scheming_protocol"), registry)
        contract_findings.extend(validate_agent_contract_payload("policy_context_package.v1", package, registry))
        package["format_contract"] = stamp_validation(format_contract, contract_findings, package, before, after, previous_counts)
        previous_counts = package["format_contract"].get("omitted_counts") if isinstance(package.get("format_contract"), dict) else previous_counts
        before = json_bytes(package)
        package = enforce_contract_limits("policy_context_package.v1", package, registry)
        after = json_bytes(package)
        contract_findings = validate_agent_contract_payload("anti_scheming_protocol.v1", package.get("anti_scheming_protocol"), registry)
        contract_findings.extend(validate_agent_contract_payload("policy_context_package.v1", package, registry))
        package["format_contract"] = stamp_validation(format_contract, contract_findings, package, before, after, previous_counts)
        return package

    @staticmethod
    def _build_objective_runtime_state(
        *,
        agent: str,
        agent_id: str,
        project: str,
        topic_path: str,
        retrieval_mode: str,
        query: str,
        session_id: str = "",
        action_executed: str = "objective_runtime_contract_built",
    ) -> Dict[str, Any]:
        registry = load_agent_contracts_registry()
        payload = {
            "version": "2026-06-05",
            "agent": str(agent or "").strip(),
            "agent_id": str(agent_id or "").strip(),
            "project": str(project or "").strip(),
            "session_id": str(session_id or "").strip(),
            "objective_state": "active",
            "mission": DEFAULT_CONTEXTLATTICE_MISSION,
            "objective": DEFAULT_CONTEXTLATTICE_OBJECTIVE,
            "goal": DEFAULT_CONTEXTLATTICE_GOAL,
            "objective_hierarchy": objective_hierarchy(
                project,
                topic_path,
                session_id,
                DEFAULT_CONTEXTLATTICE_MISSION,
                DEFAULT_CONTEXTLATTICE_OBJECTIVE,
                DEFAULT_CONTEXTLATTICE_GOAL,
            ),
            "objective_lineage": objective_lineage("objective_runtime_state"),
            "scoreboard": {
                "primary_kpi": os.getenv("CONTEXTLATTICE_PRIMARY_KPI", "").strip()
                or "agent makes measurable progress toward the requested objective",
                "guardrail_kpi": os.getenv("CONTEXTLATTICE_GUARDRAIL_KPI", "").strip()
                or "outputs stay contract-valid, bounded, evidence-grounded, and generic across agent runners",
                "cadence_kpi": os.getenv("CONTEXTLATTICE_CADENCE_KPI", "").strip()
                or "each preflight, context pack, checkpoint, handoff, and completion emits objective/session evidence",
            },
            "action_executed": str(action_executed or "objective_runtime_contract_built").strip(),
            "evidence": {
                "required": [
                    "retrieved_context_or_explicit_no_data",
                    "deterministic_check_or_artifact_inspection",
                    "checkpoint_or_session_event_for_handoff",
                ],
                "current": [
                    {
                        "kind": "preflight_contract",
                        "query": str(query or "").strip()[:720],
                        "topic_path": str(topic_path or "").strip(),
                        "retrieval_mode": str(retrieval_mode or "").strip(),
                        "session_id": str(session_id or "").strip(),
                    }
                ],
            },
            "objective_delta": {
                "before": "objective state unproven until agent records an executed action with evidence",
                "after": "agent has a bounded objective runtime contract and session path for subsequent events",
            },
            "risk_or_blocker": {
                "status": "none_reported",
                "fastest_recovery_path": "run preflight or contextlattice-session ensure, then attach the returned session_id to context, checkpoint, and handoff calls",
            },
            "next_action": "execute the smallest useful action, verify it with matching artifacts, and emit a session event or checkpoint before handoff",
        }
        metadata = contract_metadata("objective_runtime_state.v1", registry)
        payload["format_contract"] = metadata
        before = json_bytes(payload)
        payload = enforce_contract_limits("objective_runtime_state.v1", payload, registry)
        after = json_bytes(payload)
        findings = validate_agent_contract_payload("objective_runtime_state.v1", payload, registry)
        payload["format_contract"] = stamp_validation(metadata, findings, payload, before, after)
        previous_counts = payload["format_contract"].get("omitted_counts") if isinstance(payload.get("format_contract"), dict) else None
        before = json_bytes(payload)
        payload = enforce_contract_limits("objective_runtime_state.v1", payload, registry)
        after = json_bytes(payload)
        findings = validate_agent_contract_payload("objective_runtime_state.v1", payload, registry)
        payload["format_contract"] = stamp_validation(metadata, findings, payload, before, after, previous_counts)
        previous_counts = payload["format_contract"].get("omitted_counts") if isinstance(payload.get("format_contract"), dict) else previous_counts
        before = json_bytes(payload)
        payload = enforce_contract_limits("objective_runtime_state.v1", payload, registry)
        after = json_bytes(payload)
        findings = validate_agent_contract_payload("objective_runtime_state.v1", payload, registry)
        payload["format_contract"] = stamp_validation(metadata, findings, payload, before, after, previous_counts)
        return payload

    def search_with_lifecycle(
        self,
        query: str,
        project: Optional[str] = None,
        topic_path: Optional[str] = None,
        limit: int = 10,
        fetch_content: bool = False,
        retrieval_mode: str = "balanced",
        include_grounding: bool = True,
        include_retrieval_debug: bool = False,
        agent_id: Optional[str] = None,
        deep_async: Optional[bool] = None,
        wait_for_completion: bool = False,
        poll_interval_secs: float = 1.5,
        max_wait_secs: float = 75.0,
    ) -> Dict[str, Any]:
        """
        Search memory and expose retrieval lifecycle details.

        For deep mode, async is enabled by default; callers can set
        `wait_for_completion=True` to block until final deep results arrive.
        """
        mode = str(retrieval_mode or "balanced").strip().lower() or "balanced"
        deep_async_value = deep_async
        if deep_async_value is None and mode == "deep":
            deep_async_value = True
        request_payload = {
            "query": query,
            "project": project,
            "topic_path": topic_path,
            "limit": limit,
            "fetch_content": fetch_content,
            "retrieval_mode": mode,
            "include_grounding": include_grounding,
            "include_retrieval_debug": include_retrieval_debug,
            "agent_id": str(agent_id or self.agent_id).strip() or DEFAULT_AGENT_ID,
            "deep_async": bool(deep_async_value) if deep_async_value is not None else None,
        }
        if request_payload["deep_async"] is None:
            request_payload.pop("deep_async", None)

        resp = self.client.post(f"{self.base_url}/memory/search", json=request_payload)
        resp.raise_for_status()
        initial = resp.json()
        lifecycle = self._lifecycle_summary(initial)
        results = initial.get("results") if isinstance(initial.get("results"), list) else []
        continuation = (
            initial.get("continuation_async")
            if isinstance(initial.get("continuation_async"), dict)
            else {}
        )
        token = (
            continuation.get("token")
            or initial.get("token")
            or initial.get("job_id")
        )
        poll_url = (
            continuation.get("poll_url")
            or initial.get("continuation_poll_url")
            or initial.get("job_poll_url")
            or initial.get("poll_url")
        )
        events_url = (
            continuation.get("events_url")
            or initial.get("continuation_events_url")
            or initial.get("events_url")
        )
        output: Dict[str, Any] = {
            "ok": True,
            "query": query,
            "project": project,
            "retrieval_mode": mode,
            "results": results,
            "lifecycle": lifecycle,
            "async": bool(initial.get("async") or token or continuation),
            "continuation_async": continuation,
            "token": token,
            "poll_url": poll_url,
            "events_url": events_url,
            "warnings": initial.get("warnings") if isinstance(initial.get("warnings"), list) else [],
            "initial_response": initial,
            "final_response": None,
        }

        status = lifecycle.get("status")
        is_pending = status in {"queued", "running", "partial"} and bool(output.get("token"))
        if not wait_for_completion or not is_pending:
            return output

        poll_url = str(output.get("poll_url") or "").strip()
        if not poll_url:
            return output

        deadline = time.monotonic() + max(1.0, float(max_wait_secs))
        poll_target = self._absolute_url(poll_url)
        while time.monotonic() < deadline:
            poll_resp = self.client.get(
                poll_target,
                params={"include_result": "true"},
            )
            if poll_resp.status_code == 404:
                time.sleep(max(0.2, float(poll_interval_secs)))
                continue
            poll_resp.raise_for_status()
            polled = poll_resp.json()
            job_status = str(polled.get("status") or "").strip().lower()
            if job_status in {"completed", "failed"}:
                final_payload = polled.get("result") if isinstance(polled.get("result"), dict) else polled
                output["final_response"] = final_payload
                if isinstance(final_payload, dict):
                    output["results"] = final_payload.get("results") if isinstance(final_payload.get("results"), list) else output["results"]
                    output["warnings"] = (
                        final_payload.get("warnings")
                        if isinstance(final_payload.get("warnings"), list)
                        else output["warnings"]
                    )
                    output["lifecycle"] = self._lifecycle_summary(final_payload)
                return output
            time.sleep(max(0.2, float(poll_interval_secs)))
        return output

    def context_pack(
        self,
        query: str,
        project: Optional[str] = None,
        topic_path: Optional[str] = None,
        retrieval_mode: str = "balanced",
        limit: int = 10,
        max_facts: int = 24,
        include_retrieval_debug: bool = True,
        agent_id: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Retrieve factual context pack for pre-inference grounding."""
        payload = {
            "query": query,
            "project": project,
            "topic_path": topic_path,
            "retrieval_mode": retrieval_mode,
            "limit": int(limit),
            "max_facts": int(max_facts),
            "include_retrieval_debug": bool(include_retrieval_debug),
            "agent_id": str(agent_id or self.agent_id).strip() or DEFAULT_AGENT_ID,
            "traffic_class": "user",
        }
        resp = self.client.post(f"{self.base_url}/memory/context-pack", json=payload)
        resp.raise_for_status()
        return resp.json()

    @staticmethod
    def _safe_file_token(value: str) -> str:
        token = "".join(ch if ch.isalnum() or ch in {"-", "_"} else "_" for ch in (value or "agent"))
        token = token.strip("_")
        return token or "agent"

    @staticmethod
    def _render_compaction_handoff_markdown(
        *,
        generated_at: str,
        agent_id: str,
        project: str,
        topic_path: str,
        retrieval_mode: str,
        summary: str,
        mission: str,
        objective: str,
        goal: str,
        handoff_metadata: Optional[Dict[str, Any]],
        facts: List[Dict[str, Any]],
        warnings: List[str],
    ) -> str:
        metadata = handoff_metadata or {}
        lines: List[str] = [
            "# Context Compaction Handoff",
            "",
            f"- generated_at: {generated_at}",
            f"- agent_id: {agent_id}",
            f"- project: {project}",
            f"- topic_path: {topic_path}",
            f"- retrieval_mode: {retrieval_mode}",
        ]
        for key in ("session_id", "cwd", "branch"):
            value = str(metadata.get(key) or "").strip()
            if value:
                lines.append(f"- {key}: {value}")
        lines.extend(
            [
                "",
                "## Active Objective Summary",
                summary.strip() or "_no explicit summary provided_",
                "",
            ]
        )
        if metadata:
            lines.append("## Session State")
            for key in ("objective", "next_action"):
                value = str(metadata.get(key) or "").strip()
                if value:
                    lines.append(f"- {key}: {value}")
            for key, label in (
                ("blockers", "blockers"),
                ("changed_files", "changed_files"),
                ("commands_run", "commands_run"),
            ):
                values = metadata.get(key) if isinstance(metadata.get(key), list) else []
                if values:
                    rendered = ", ".join(str(item) for item in values[:12])
                    lines.append(f"- {label}: {rendered}")
            lines.append("")
        lines.extend(
            [
                "## Mission / Objective / Goal",
                f"- mission: {mission}",
                f"- objective: {objective}",
                f"- goal: {goal}",
                "",
                "## Retrieved High-Signal Facts",
            ]
        )
        if facts:
            for fact in facts:
                text = str(fact.get("text") or "").strip()
                if not text:
                    continue
                source = str(fact.get("source") or "").strip()
                topic = str(fact.get("topic_path") or "").strip()
                tag_parts = [item for item in [source, topic] if item]
                tag = f" ({' | '.join(tag_parts)})" if tag_parts else ""
                lines.append(f"- {text}{tag}")
        else:
            lines.append("- _no facts returned in context-pack for this scope_")
        if warnings:
            lines.extend(["", "## Retrieval Warnings"])
            for warning in warnings:
                lines.append(f"- {warning}")
        lines.extend(
            [
                "",
                "## Resume Contract (post-compaction)",
                "- Re-run preflight for this agent profile.",
                "- Read this handoff topic before executing new actions.",
                "- Continue from the objective summary and next open execution step.",
            ]
        )
        return "\n".join(lines).strip() + "\n"

    def compaction_handoff(
        self,
        project: str,
        summary: str,
        topic_path: Optional[str] = None,
        retrieval_mode: str = "balanced",
        query: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Persist a detailed objective/mission handoff before compaction and
        immediately read it back to seed post-compaction continuity.
        """
        effective_topic_path = str(topic_path or DEFAULT_COMPACTION_TOPIC_PATH).strip() or DEFAULT_COMPACTION_TOPIC_PATH
        effective_query = str(query or DEFAULT_COMPACTION_QUERY).strip() or DEFAULT_COMPACTION_QUERY
        mode = str(retrieval_mode or "balanced").strip().lower() or "balanced"
        generated_at = datetime.now(UTC).isoformat().replace("+00:00", "Z")
        handoff_metadata: Dict[str, Any] = {}
        summary_text = str(summary or "").strip()
        try:
            parsed_summary = json.loads(summary_text)
            if isinstance(parsed_summary, dict) and parsed_summary.get("schema_version"):
                handoff_metadata = parsed_summary
                summary_text = str(parsed_summary.get("summary") or summary_text).strip()
        except Exception:
            pass
        primary_pack = self.context_pack(
            query=effective_query,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=mode,
            include_retrieval_debug=True,
            agent_id=self.agent_id,
            max_facts=24,
        )
        mission_pack = None
        mission_pack_error = None
        mission_query = (
            "mission objective goal cross-project synthesis longitudinal learning "
            "policy context package retrieval discipline"
        )
        mission_topic_path = (
            str(os.getenv("CONTEXTLATTICE_POLICY_TOPIC_PATH", "")).strip() or "runbooks/context-policy"
        )
        try:
            mission_pack = self.context_pack(
                query=mission_query,
                project=project,
                topic_path=mission_topic_path,
                retrieval_mode=mode,
                include_retrieval_debug=True,
                agent_id=self.agent_id,
                max_facts=12,
            )
            mission_results = self._extract_fact_summaries(mission_pack, max_items=2)
            if not mission_results:
                mission_pack = self.context_pack(
                    query=mission_query,
                    project=project,
                    topic_path=None,
                    retrieval_mode=mode,
                    include_retrieval_debug=True,
                    agent_id=self.agent_id,
                    max_facts=12,
                )
        except Exception as exc:  # pragma: no cover - defensive network path
            mission_pack_error = str(exc)

        policy_context_package = self._build_agent_policy_context_package(
            agent="compaction-handoff",
            agent_id=self.agent_id,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=mode,
            query=effective_query,
            primary_pack=primary_pack,
            mission_pack=mission_pack,
            mission_pack_error=mission_pack_error,
        )
        primary_facts = self._extract_fact_summaries(primary_pack, max_items=12)
        warnings = primary_pack.get("warnings") if isinstance(primary_pack.get("warnings"), list) else []
        markdown = self._render_compaction_handoff_markdown(
            generated_at=generated_at,
            agent_id=self.agent_id,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=mode,
            summary=summary_text,
            mission=policy_context_package.get("mission", DEFAULT_CONTEXTLATTICE_MISSION),
            objective=policy_context_package.get("objective", DEFAULT_CONTEXTLATTICE_OBJECTIVE),
            goal=policy_context_package.get("goal", DEFAULT_CONTEXTLATTICE_GOAL),
            handoff_metadata=handoff_metadata,
            facts=primary_facts,
            warnings=warnings,
        )
        file_token = self._safe_file_token(str(handoff_metadata.get("session_id") or self.agent_id))
        file_name = f"notes/compaction/{generated_at.replace(':', '').replace('-', '')}_{file_token}.md"
        write_result = self.write(
            project=project,
            file_name=file_name,
            content=markdown,
            topic_path=effective_topic_path,
        )
        readback = self.search_with_lifecycle(
            query=effective_query,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=mode,
            include_grounding=True,
            include_retrieval_debug=True,
            wait_for_completion=False,
            agent_id=self.agent_id,
        )
        return {
            "ok": True,
            "project": project,
            "topic_path": effective_topic_path,
            "retrieval_mode": mode,
            "file": file_name,
            "write": write_result,
            "readback": readback,
            "policy_context_package": policy_context_package,
            "context_pack": primary_pack,
            "handoff_metadata": handoff_metadata,
        }

    def codex_preflight(
        self,
        project: str,
        topic_path: str = "runbooks/codex-integration",
        query: str = "codex preflight connectivity and retrieval",
    ) -> Dict[str, Any]:
        """
        Codex-first preflight:
        - health
        - status
        - scoped search (with broadened fallback)
        - context-pack retrieval
        """
        return self.agent_preflight(
            agent="codex",
            project=project,
            topic_path=topic_path,
            query=query,
            retrieval_mode="balanced",
        )

    def agent_preflight(
        self,
        agent: str,
        project: str,
        topic_path: Optional[str] = None,
        query: Optional[str] = None,
        retrieval_mode: Optional[str] = None,
    ) -> Dict[str, Any]:
        """Preflight for a named agent profile with scoped search + context pack."""
        profile_key, profile = _resolve_agent_preflight_profile(agent)
        effective_topic_path = str(topic_path or profile.get("topic_path") or "").strip()
        effective_query = str(query or profile.get("query") or "").strip()
        effective_mode = str(retrieval_mode or profile.get("retrieval_mode") or "balanced").strip().lower() or "balanced"
        effective_agent_id = str(profile.get("agent_id") or self.agent_id).strip() or DEFAULT_AGENT_ID

        health = self.client.get(f"{self.base_url}/health")
        health.raise_for_status()
        health_json = health.json()

        status = self.client.get(f"{self.base_url}/status")
        status.raise_for_status()
        status_json = status.json()

        scoped = self.search_with_lifecycle(
            query=effective_query,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=effective_mode,
            include_grounding=True,
            include_retrieval_debug=True,
            wait_for_completion=False,
            agent_id=effective_agent_id,
        )
        broadened = None
        scoped_results = scoped.get("results") if isinstance(scoped.get("results"), list) else []
        scoped_lifecycle = scoped.get("lifecycle") if isinstance(scoped.get("lifecycle"), dict) else {}
        if not scoped_results or str(scoped_lifecycle.get("status") or "").strip().lower() in {"partial", "failed"}:
            broadened = self.search_with_lifecycle(
                query=effective_query,
                project=project,
                topic_path=None,
                retrieval_mode=effective_mode,
                include_grounding=True,
                include_retrieval_debug=True,
                wait_for_completion=False,
                agent_id=effective_agent_id,
            )

        pack = self.context_pack(
            query=effective_query,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=effective_mode,
            include_retrieval_debug=True,
            agent_id=effective_agent_id,
        )
        mission_query = (
            "mission objective goal cross-project synthesis longitudinal learning "
            "policy context package retrieval discipline"
        )
        mission_topic_path = (
            str(os.getenv("CONTEXTLATTICE_POLICY_TOPIC_PATH", "")).strip() or "runbooks/context-policy"
        )
        mission_pack: Optional[Dict[str, Any]] = None
        mission_pack_error: Optional[str] = None
        try:
            mission_pack = self.context_pack(
                query=mission_query,
                project=project,
                topic_path=mission_topic_path,
                retrieval_mode=effective_mode,
                max_facts=12,
                include_retrieval_debug=True,
                agent_id=effective_agent_id,
            )
            mission_results = self._extract_fact_summaries(mission_pack, max_items=2)
            if not mission_results:
                mission_pack = self.context_pack(
                    query=mission_query,
                    project=project,
                    topic_path=None,
                    retrieval_mode=effective_mode,
                    max_facts=12,
                    include_retrieval_debug=True,
                    agent_id=effective_agent_id,
                )
        except Exception as exc:
            mission_pack_error = str(exc)

        objective_runtime = self._build_objective_runtime_state(
            agent=profile_key,
            agent_id=effective_agent_id,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=effective_mode,
            query=effective_query,
            action_executed="agent.preflight.completed",
        )
        policy_context_package = self._build_agent_policy_context_package(
            agent=profile_key,
            agent_id=effective_agent_id,
            project=project,
            topic_path=effective_topic_path,
            retrieval_mode=effective_mode,
            query=effective_query,
            primary_pack=pack,
            mission_pack=mission_pack,
            mission_pack_error=mission_pack_error,
            objective_runtime=objective_runtime,
        )

        response = {
            "ok": True,
            "service": "python-agent-orchestration",
            "agent": profile_key,
            "agent_profile": profile,
            "agent_id": effective_agent_id,
            "project": project,
            "query": effective_query,
            "topic_path": effective_topic_path,
            "retrieval_mode": effective_mode,
            "orchestrator_url": self.base_url,
            "health": health_json,
            "status": status_json,
            "scoped_search": scoped,
            "broadened_search": broadened,
            "context_pack": pack,
            "mission_context_pack": mission_pack,
            "objective_runtime": objective_runtime,
            "policy_context_package": policy_context_package,
            "format_contracts": preflight_contracts_summary(),
        }
        before = json_bytes(response)
        response = enforce_contract_limits("agent_preflight_response.v1", response)
        after = json_bytes(response)
        preflight_findings = validate_agent_contract_payload("agent_preflight_response.v1", response)
        response["format_contracts"] = preflight_contracts_summary(preflight_findings, response, before, after)
        previous_counts = response["format_contracts"].get("omitted_counts") if isinstance(response.get("format_contracts"), dict) else None
        before = json_bytes(response)
        response = enforce_contract_limits("agent_preflight_response.v1", response)
        after = json_bytes(response)
        preflight_findings = validate_agent_contract_payload("agent_preflight_response.v1", response)
        response["format_contracts"] = preflight_contracts_summary(preflight_findings, response, before, after, previous_counts)
        previous_counts = response["format_contracts"].get("omitted_counts") if isinstance(response.get("format_contracts"), dict) else previous_counts
        before = json_bytes(response)
        response = enforce_contract_limits("agent_preflight_response.v1", response)
        after = json_bytes(response)
        preflight_findings = validate_agent_contract_payload("agent_preflight_response.v1", response)
        response["format_contracts"] = preflight_contracts_summary(preflight_findings, response, before, after, previous_counts)
        return response

    def status(self) -> Dict[str, Any]:
        """Get orchestrator + service status."""
        resp = self.client.get(f"{self.base_url}/status")
        resp.raise_for_status()
        return resp.json()


# Backwards-compatible alias for external scripts importing the old symbol.
MemMCPOrchestrator = ContextLatticeOrchestrator


class TaskCoordinator:
    """Coordinates tasks across multiple agents via ContextLattice."""

    def __init__(self, orchestrator: ContextLatticeOrchestrator, project: str):
        self.orch = orchestrator
        self.project = project

    def create_task_list(
        self, task_id: str, tasks: List[Dict[str, Any]]
    ) -> Dict[str, Any]:
        """Create a task list for agent execution."""
        timestamp = datetime.utcnow().strftime("%Y%m%d_%H%M%S")
        file_name = f"tasks/{task_id}_{timestamp}.json"

        payload = {
            "task_id": task_id,
            "created_at": datetime.utcnow().isoformat() + "Z",
            "status": "pending",
            "tasks": tasks,
        }

        self.orch.write(self.project, file_name, json.dumps(payload, indent=2))
        return {"file": file_name, "task_id": task_id}

    def update_task_status(
        self, file_name: str, task_index: int, status: str, result: Optional[str] = None
    ) -> None:
        """Update status of a specific task."""
        content = self.orch.read(self.project, file_name)
        data = json.loads(content)

        if task_index < len(data["tasks"]):
            data["tasks"][task_index]["status"] = status
            data["tasks"][task_index]["updated_at"] = (
                datetime.utcnow().isoformat() + "Z"
            )
            if result:
                data["tasks"][task_index]["result"] = result

        # Update overall status
        all_done = all(t.get("status") == "done" for t in data["tasks"])
        any_failed = any(t.get("status") == "failed" for t in data["tasks"])

        if all_done:
            data["status"] = "completed"
        elif any_failed:
            data["status"] = "failed"
        else:
            data["status"] = "in_progress"

        self.orch.write(self.project, file_name, json.dumps(data, indent=2))

    def read_task_list(self, file_name: str) -> Dict[str, Any]:
        """Read current task list state."""
        content = self.orch.read(self.project, file_name)
        return json.loads(content)

    def log_agent_handoff(
        self,
        from_agent: str,
        to_agent: str,
        context: str,
        task_file: Optional[str] = None,
    ) -> None:
        """Log agent handoff for traceability."""
        timestamp = datetime.utcnow().strftime("%Y%m%d_%H%M%S")
        file_name = f"briefings/handoff_{from_agent}_to_{to_agent}_{timestamp}.txt"

        content = f"""# Agent Handoff
From: {from_agent}
To: {to_agent}
Timestamp: {datetime.utcnow().isoformat()}Z

## Context
{context}
"""
        if task_file:
            content += f"\n## Task File\n{task_file}\n"

        self.orch.write(self.project, file_name, content)


def main():
    """CLI for agent orchestration."""
    if len(sys.argv) < 2:
        print("Usage: agent_orchestration.py <command> [args...]")
        print("\nCommands:")
        print("  write <project> <file> <content>")
        print("  read <project> <file>")
        print("  list <project>")
        print("  search <query> [project]")
        print("  search-lifecycle <query> [project] [mode] [wait]")
        print("  context-pack <query> [project] [mode] [topic_path]")
        print("  compaction-handoff <project> <summary> [topic_path] [mode] [query]")
        print("  preflight [project] [topic_path] [query]")
        print("  preflight-agent <agent> [project] [topic_path] [query] [mode]")
        print("  status")
        print("  create-tasks <project> <task_id> <tasks_json>")
        sys.exit(1)

    orch = ContextLatticeOrchestrator()
    cmd = sys.argv[1]

    if cmd == "write":
        project, file_name, content = sys.argv[2:5]
        result = orch.write(project, file_name, content)
        print(json.dumps(result, indent=2))

    elif cmd == "read":
        project, file_name = sys.argv[2:4]
        content = orch.read(project, file_name)
        print(content)

    elif cmd == "list":
        project = sys.argv[2]
        files = orch.list_files(project)
        print(json.dumps(files, indent=2))

    elif cmd == "search":
        query = sys.argv[2]
        project = sys.argv[3] if len(sys.argv) > 3 else None
        results = orch.search(query, project=project)
        print(json.dumps(results, indent=2))

    elif cmd == "search-lifecycle":
        query = sys.argv[2]
        project = sys.argv[3] if len(sys.argv) > 3 else None
        mode = sys.argv[4] if len(sys.argv) > 4 else "balanced"
        wait_flag = str(sys.argv[5]).strip().lower() if len(sys.argv) > 5 else ""
        wait_for_completion = wait_flag in {"1", "true", "yes", "wait", "on"}
        payload = orch.search_with_lifecycle(
            query=query,
            project=project,
            retrieval_mode=mode,
            wait_for_completion=wait_for_completion,
        )
        print(json.dumps(payload, indent=2))

    elif cmd == "context-pack":
        query = sys.argv[2]
        project = sys.argv[3] if len(sys.argv) > 3 else None
        mode = sys.argv[4] if len(sys.argv) > 4 else "balanced"
        topic_path = sys.argv[5] if len(sys.argv) > 5 else None
        payload = orch.context_pack(
            query=query,
            project=project,
            topic_path=topic_path,
            retrieval_mode=mode,
            include_retrieval_debug=True,
        )
        print(json.dumps(payload, indent=2))

    elif cmd == "compaction-handoff":
        project = sys.argv[2] if len(sys.argv) > 2 else os.getenv("CONTEXTLATTICE_PROJECT", "contextlattice")
        summary = sys.argv[3] if len(sys.argv) > 3 else "continue objective-aligned execution after context compaction"
        topic_path = sys.argv[4] if len(sys.argv) > 4 else DEFAULT_COMPACTION_TOPIC_PATH
        retrieval_mode = sys.argv[5] if len(sys.argv) > 5 else "balanced"
        query = sys.argv[6] if len(sys.argv) > 6 else DEFAULT_COMPACTION_QUERY
        payload = orch.compaction_handoff(
            project=project,
            summary=summary,
            topic_path=topic_path,
            retrieval_mode=retrieval_mode,
            query=query,
        )
        print(json.dumps(payload, indent=2))

    elif cmd == "preflight":
        project = sys.argv[2] if len(sys.argv) > 2 else os.getenv("CONTEXTLATTICE_PROJECT", "contextlattice")
        topic_path = sys.argv[3] if len(sys.argv) > 3 else "runbooks/codex-integration"
        query = sys.argv[4] if len(sys.argv) > 4 else "codex preflight connectivity and retrieval"
        payload = orch.codex_preflight(project=project, topic_path=topic_path, query=query)
        print(json.dumps(payload, indent=2))

    elif cmd == "preflight-agent":
        agent = sys.argv[2] if len(sys.argv) > 2 else "codex"
        project = sys.argv[3] if len(sys.argv) > 3 else os.getenv("CONTEXTLATTICE_PROJECT", "contextlattice")
        topic_path = sys.argv[4] if len(sys.argv) > 4 else None
        query = sys.argv[5] if len(sys.argv) > 5 else None
        retrieval_mode = sys.argv[6] if len(sys.argv) > 6 else None
        payload = orch.agent_preflight(
            agent=agent,
            project=project,
            topic_path=topic_path,
            query=query,
            retrieval_mode=retrieval_mode,
        )
        print(json.dumps(payload, indent=2))

    elif cmd == "status":
        status = orch.status()
        print(json.dumps(status, indent=2))

    elif cmd == "create-tasks":
        project = sys.argv[2]
        task_id = sys.argv[3]
        tasks_json = sys.argv[4]
        tasks = json.loads(tasks_json)
        coord = TaskCoordinator(orch, project)
        result = coord.create_task_list(task_id, tasks)
        print(json.dumps(result, indent=2))

    else:
        print(f"Unknown command: {cmd}")
        sys.exit(1)


if __name__ == "__main__":
    main()
