#!/usr/bin/env python3
"""Context expansion runtime helpers for agent task execution."""

from __future__ import annotations

import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from copy import deepcopy
from datetime import datetime, timezone
from typing import Any, Optional

try:
    from scripts.contextlattice_client import resolve_orchestrator_api_key
except ModuleNotFoundError:  # pragma: no cover - fallback when run from scripts/ root
    from contextlattice_client import resolve_orchestrator_api_key  # type: ignore[no-redef]


def _env_bool(name: str, default: bool) -> bool:
    raw = str(os.getenv(name, "")).strip().lower()
    if not raw:
        return default
    return raw in {"1", "true", "yes", "on"}


def _env_int(name: str, default: int, minimum: int) -> int:
    raw = str(os.getenv(name, "")).strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        return default
    return max(minimum, value)


def _env_float(name: str, default: float, minimum: float) -> float:
    raw = str(os.getenv(name, "")).strip()
    if not raw:
        return default
    try:
        value = float(raw)
    except ValueError:
        return default
    return max(minimum, value)


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _approx_tokens(text: str) -> int:
    return max(1, len(text) // 4)


def _truncate_by_tokens(text: str, token_budget: int) -> str:
    budget = max(0, int(token_budget))
    if budget <= 0:
        return ""
    char_budget = budget * 4
    if len(text) <= char_budget:
        return text
    if char_budget <= 1:
        return text[:char_budget]
    return text[: max(1, char_budget - 1)] + "…"


def _normalize_mode(value: str | None) -> str:
    token = str(value or "").strip().lower()
    if token in {"fast", "balanced", "deep"}:
        return token
    return "balanced"


def _unique_ordered(items: list[str]) -> list[str]:
    seen: set[str] = set()
    output: list[str] = []
    for item in items:
        token = str(item or "").strip()
        if not token or token in seen:
            continue
        seen.add(token)
        output.append(token)
    return output


def _parse_topic_path(task: dict[str, Any]) -> str | None:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    topic = (
        payload.get("topic_path")
        or payload.get("topicPath")
        or task.get("topic_path")
        or task.get("topicPath")
    )
    token = str(topic or "").strip()
    return token or None


def _parse_query(task: dict[str, Any]) -> str:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    for key in (
        "query",
        "objective",
        "goal",
        "question",
        "search_query",
        "prompt",
        "instruction",
    ):
        value = str(payload.get(key) or "").strip()
        if value:
            return value
    title = str(task.get("title") or "").strip()
    if title:
        return title
    return "task context"


def _parse_project(task: dict[str, Any]) -> str:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    project = str(task.get("project") or payload.get("project") or "_global").strip()
    return project or "_global"


def _parse_tools(task: dict[str, Any]) -> list[str]:
    payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
    raw_tools: list[str] = []
    for key in ("tool", "tools", "required_tools", "tool_names"):
        value = payload.get(key)
        if isinstance(value, str):
            raw_tools.append(value)
        elif isinstance(value, list):
            for item in value:
                if isinstance(item, str):
                    raw_tools.append(item)
                elif isinstance(item, dict):
                    name = (
                        item.get("name")
                        or item.get("tool")
                        or item.get("id")
                        or item.get("type")
                    )
                    if isinstance(name, str):
                        raw_tools.append(name)
    return _unique_ordered(raw_tools)


def _fact_text(fact: dict[str, Any]) -> str:
    text = str(fact.get("text") or fact.get("summary") or "").strip()
    if text:
        return text
    return str(fact)


def _result_summary(row: dict[str, Any]) -> str:
    text = str(row.get("summary") or "").strip()
    if text:
        return text
    topic_rollup = row.get("topic_rollup") if isinstance(row.get("topic_rollup"), dict) else {}
    raw_refs = topic_rollup.get("raw_refs") if isinstance(topic_rollup.get("raw_refs"), list) else []
    if raw_refs:
        return f"Rollup refs: {', '.join(str(item) for item in raw_refs[:3])}"
    return ""


def _result_key(row: dict[str, Any]) -> str:
    return (
        f"{row.get('project')}|{row.get('file')}|{row.get('source')}"
        f"|{row.get('topic_path')}|{row.get('timestamp')}"
    )


def _fact_key(fact: dict[str, Any]) -> str:
    source = fact.get("source") if isinstance(fact.get("source"), dict) else {}
    return f"{source.get('project')}|{source.get('file')}|{source.get('source')}|{_fact_text(fact)}"


def _sort_results(results: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return sorted(
        results,
        key=lambda row: (
            -float(row.get("score") or 0.0),
            str(row.get("project") or ""),
            str(row.get("file") or ""),
            str(row.get("source") or ""),
        ),
    )


def _slice_with_budget(
    items: list[dict[str, Any]],
    text_getter,
    token_budget: int,
) -> list[dict[str, Any]]:
    budget = max(1, int(token_budget))
    used = 0
    output: list[dict[str, Any]] = []
    for item in items:
        rendered = str(text_getter(item) or "").strip()
        if not rendered:
            continue
        cost = _approx_tokens(rendered)
        if used + cost <= budget:
            output.append(item)
            used += cost
            continue
        remaining = budget - used
        if remaining < 32:
            break
        clipped = deepcopy(item)
        if "text" in clipped:
            clipped["text"] = _truncate_by_tokens(str(clipped.get("text") or ""), remaining)
        elif "summary" in clipped:
            clipped["summary"] = _truncate_by_tokens(str(clipped.get("summary") or ""), remaining)
        output.append(clipped)
        break
    return output


def _extract_tool_keywords(name: str) -> list[str]:
    parts = re.split(r"[^a-z0-9]+", str(name or "").lower())
    return [part for part in parts if len(part) >= 3]


def _extract_unresolved_lines(text: str, cap: int = 10) -> list[str]:
    lines = [line.strip("- ").strip() for line in str(text or "").splitlines() if line.strip()]
    unresolved: list[str] = []
    for line in lines:
        lower = line.lower()
        if (
            line.endswith("?")
            or "todo" in lower
            or "unknown" in lower
            or "unclear" in lower
            or "needs " in lower
            or "follow up" in lower
        ):
            unresolved.append(line)
        if len(unresolved) >= cap:
            break
    return unresolved


def _extract_decision_lines(text: str, cap: int = 10) -> list[str]:
    lines = [line.strip("- ").strip() for line in str(text or "").splitlines() if line.strip()]
    decisions: list[str] = []
    for line in lines:
        lower = line.lower()
        if (
            "decision" in lower
            or "we will" in lower
            or "i will" in lower
            or lower.startswith("do ")
            or lower.startswith("implement ")
            or "recommend" in lower
        ):
            decisions.append(line)
        if len(decisions) >= cap:
            break
    return decisions


class ContextExpansionRuntime:
    """Pre-inference context expansion + post-inference checkpoint loop."""

    def __init__(
        self,
        orchestrator_url: str,
        api_key: Optional[str] = None,
        agent_id: Optional[str] = None,
        caller_role: str = "worker",
    ):
        self.base_url = orchestrator_url.rstrip("/")
        role = str(caller_role or "").strip().lower() or "worker"
        self.api_key = str(api_key or "").strip() or resolve_orchestrator_api_key(role=role)
        default_agent_id = (
            str(os.getenv("CONTEXTLATTICE_AGENT_ID") or "").strip()
            or str(os.getenv("CONTEXTLATTICE_AGENT_ID") or "").strip()
            or "codex_gpt5"
        )
        self.agent_id = str(agent_id or "").strip() or default_agent_id
        self.enabled = _env_bool("CONTEXT_EXPANSION_ENABLED", True)
        self.min_results = _env_int("CONTEXT_EXPANSION_MIN_RESULTS", 3, 1)
        self.min_facts = _env_int("CONTEXT_EXPANSION_MIN_FACTS", 3, 1)
        self.max_results = _env_int("CONTEXT_EXPANSION_MAX_RESULTS", 12, 1)
        self.max_facts = _env_int("CONTEXT_EXPANSION_MAX_FACTS", 24, 1)
        self.budget_l0 = _env_int("CONTEXT_EXPANSION_L0_BUDGET_TOKENS", 1200, 128)
        self.budget_l1 = _env_int("CONTEXT_EXPANSION_L1_BUDGET_TOKENS", 800, 64)
        self.budget_l2 = _env_int("CONTEXT_EXPANSION_L2_BUDGET_TOKENS", 400, 32)
        self.prompt_budget = _env_int("CONTEXT_EXPANSION_PROMPT_BUDGET_CHARS", 18000, 2000)
        self.tool_slice_cap = _env_int("CONTEXT_EXPANSION_TOOL_SLICE_FACTS", 6, 1)
        self.deep_poll_secs = _env_float("CONTEXT_EXPANSION_DEEP_POLL_SECS", 8.0, 1.0)
        self.deep_poll_interval = _env_float("CONTEXT_EXPANSION_DEEP_POLL_INTERVAL_SECS", 1.5, 0.2)
        self.deep_escalation_enabled = _env_bool("CONTEXT_EXPANSION_DEEP_ESCALATION_ENABLED", True)
        self.retry_timeout = _env_float("CONTEXT_EXPANSION_HTTP_TIMEOUT_SECS", 30.0, 5.0)

    def _headers(self) -> dict[str, str]:
        headers = {"content-type": "application/json"}
        if self.api_key:
            headers["x-api-key"] = self.api_key
        return headers

    def _request_json(
        self,
        method: str,
        path: str,
        payload: Optional[dict[str, Any]] = None,
        timeout: Optional[float] = None,
    ) -> dict[str, Any]:
        url = f"{self.base_url}{path}"
        body = json.dumps(payload).encode("utf-8") if payload is not None else None
        req = urllib.request.Request(url, data=body, method=method.upper(), headers=self._headers())
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.retry_timeout) as response:
                content = response.read().decode("utf-8")
            return json.loads(content) if content else {}
        except urllib.error.HTTPError as exc:
            message = exc.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"{method} {path} failed: {exc.code} {message}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(f"{method} {path} failed: {exc}") from exc

    def _search(
        self,
        *,
        query: str,
        project: str,
        topic_path: str | None,
        retrieval_mode: str,
        deep_async: bool | None = None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "query": query,
            "project": project,
            "topic_path": topic_path,
            "limit": self.max_results,
            "include_grounding": True,
            "include_retrieval_debug": True,
            "retrieval_mode": retrieval_mode,
            "agent_id": self.agent_id,
            "traffic_class": "user",
        }
        if deep_async is not None:
            payload["deep_async"] = bool(deep_async)
        return self._request_json("POST", "/memory/search", payload)

    def _context_pack(
        self,
        *,
        query: str,
        project: str,
        topic_path: str | None,
        retrieval_mode: str,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "query": query,
            "project": project,
            "topic_path": topic_path,
            "limit": self.max_results,
            "max_facts": self.max_facts,
            "retrieval_mode": retrieval_mode,
            "include_retrieval_debug": True,
            "agent_id": self.agent_id,
            "traffic_class": "user",
        }
        return self._request_json("POST", "/memory/context-pack", payload)

    @staticmethod
    def _lifecycle_from_response(response: dict[str, Any]) -> dict[str, Any]:
        lifecycle = response.get("retrieval_lifecycle") if isinstance(response.get("retrieval_lifecycle"), dict) else {}
        source_summary = response.get("source_summary") if isinstance(response.get("source_summary"), dict) else {}
        sources = lifecycle.get("sources") if isinstance(lifecycle.get("sources"), dict) else {}
        return {
            "status": str(lifecycle.get("status") or "unknown"),
            "result_state": str(lifecycle.get("result_state") or response.get("result_state") or "unknown"),
            "degraded": bool(response.get("degraded", False)),
            "returned_now": list(sources.get("returned_now") or source_summary.get("returned_now") or []),
            "pending_sources": list(sources.get("pending") or source_summary.get("pending_sources") or []),
            "failed_sources": list(sources.get("failed") or source_summary.get("failed_sources") or []),
            "timed_out_sources": list(sources.get("timed_out") or source_summary.get("timed_out_sources") or []),
            "budget_exceeded_sources": list(
                sources.get("budget_exceeded") or source_summary.get("budget_exceeded_sources") or []
            ),
            "next_actions": list(lifecycle.get("next_actions") or []),
        }

    @staticmethod
    def _merge_responses(responses: list[dict[str, Any]]) -> tuple[list[dict[str, Any]], list[dict[str, Any]], list[dict[str, Any]], list[str]]:
        merged_results: dict[str, dict[str, Any]] = {}
        merged_facts: dict[str, dict[str, Any]] = {}
        merged_numeric: dict[str, dict[str, Any]] = {}
        warnings: list[str] = []
        for response in responses:
            if not isinstance(response, dict):
                continue
            rows = response.get("results") if isinstance(response.get("results"), list) else []
            for row in rows:
                if not isinstance(row, dict):
                    continue
                merged_results.setdefault(_result_key(row), row)
            grounding = response.get("grounding") if isinstance(response.get("grounding"), dict) else {}
            facts = grounding.get("facts") if isinstance(grounding.get("facts"), list) else []
            numeric = grounding.get("numeric_facts") if isinstance(grounding.get("numeric_facts"), list) else []
            for fact in facts:
                if isinstance(fact, dict):
                    merged_facts.setdefault(_fact_key(fact), fact)
            for item in numeric:
                if isinstance(item, dict):
                    key = json.dumps(item, sort_keys=True)
                    merged_numeric.setdefault(key, item)
            raw_warnings = response.get("warnings") if isinstance(response.get("warnings"), list) else []
            warnings.extend(str(item or "").strip() for item in raw_warnings if str(item or "").strip())
        return (
            _sort_results(list(merged_results.values())),
            list(merged_facts.values()),
            list(merged_numeric.values()),
            _unique_ordered(warnings),
        )

    @staticmethod
    def _extract_raw_refs(results: list[dict[str, Any]]) -> list[dict[str, Any]]:
        refs: list[dict[str, Any]] = []
        seen: set[str] = set()
        for row in results:
            if not isinstance(row, dict):
                continue
            rollup = row.get("topic_rollup") if isinstance(row.get("topic_rollup"), dict) else {}
            raw_refs = rollup.get("raw_refs") if isinstance(rollup.get("raw_refs"), list) else []
            for file_name in raw_refs:
                file_token = str(file_name or "").strip()
                if not file_token:
                    continue
                key = f"{row.get('project')}|{file_token}"
                if key in seen:
                    continue
                seen.add(key)
                refs.append(
                    {
                        "project": row.get("project"),
                        "file": file_token,
                        "topic_path": row.get("topic_path"),
                        "source": "topic_rollup_raw_ref",
                        "summary": f"Raw reference for detail dive: {file_token}",
                    }
                )
            partitions = rollup.get("file_partitions") if isinstance(rollup.get("file_partitions"), list) else []
            for partition in partitions:
                if not isinstance(partition, dict):
                    continue
                sample_files = partition.get("sample_files") if isinstance(partition.get("sample_files"), list) else []
                for sample in sample_files:
                    file_token = str(sample or "").strip()
                    if not file_token:
                        continue
                    key = f"{row.get('project')}|{file_token}"
                    if key in seen:
                        continue
                    seen.add(key)
                    refs.append(
                        {
                            "project": row.get("project"),
                            "file": file_token,
                            "topic_path": partition.get("topic_path") or row.get("topic_path"),
                            "source": "topic_rollup_partition_ref",
                            "summary": f"Partition reference for detail dive: {file_token}",
                        }
                    )
        return refs

    def _build_tool_slices(
        self,
        tools: list[str],
        facts: list[dict[str, Any]],
        raw_refs: list[dict[str, Any]],
    ) -> dict[str, dict[str, Any]]:
        slices: dict[str, dict[str, Any]] = {}
        if not tools:
            return slices
        for tool_name in tools:
            keywords = _extract_tool_keywords(tool_name)
            selected_facts: list[dict[str, Any]] = []
            selected_refs: list[dict[str, Any]] = []
            if keywords:
                for fact in facts:
                    text = _fact_text(fact).lower()
                    if any(keyword in text for keyword in keywords):
                        selected_facts.append(fact)
                        if len(selected_facts) >= self.tool_slice_cap:
                            break
                for ref in raw_refs:
                    text = f"{ref.get('file')} {ref.get('topic_path')} {ref.get('summary')}".lower()
                    if any(keyword in text for keyword in keywords):
                        selected_refs.append(ref)
                        if len(selected_refs) >= self.tool_slice_cap:
                            break
            if not selected_facts:
                selected_facts = facts[: self.tool_slice_cap]
            if not selected_refs:
                selected_refs = raw_refs[: min(3, self.tool_slice_cap)]
            slices[tool_name] = {
                "keywords": keywords,
                "facts": selected_facts,
                "raw_refs": selected_refs,
            }
        return slices

    def prepare(self, task: dict[str, Any]) -> dict[str, Any]:
        query = _parse_query(task)
        project = _parse_project(task)
        topic_path = _parse_topic_path(task)
        tools = _parse_tools(task)
        retrieval_mode = _normalize_mode(
            (task.get("payload") if isinstance(task.get("payload"), dict) else {}).get("retrieval_mode")
        )
        if not self.enabled:
            return {
                "enabled": False,
                "query": query,
                "project": project,
                "topic_path": topic_path,
                "warnings": ["context expansion disabled by CONTEXT_EXPANSION_ENABLED=false"],
                "lifecycle": {"status": "disabled", "result_state": "disabled", "degraded": False},
                "layers": {"l0_facts": [], "l1_rollups": [], "l2_raw_refs": []},
                "numeric_facts": [],
                "tool_slices": {},
                "expansion": {"broadened_scope": False, "deep_escalated": False, "steps": []},
            }

        responses: list[dict[str, Any]] = []
        expansion_steps: list[str] = []
        broadened_scope = False
        deep_escalated = False

        context_pack: dict[str, Any] = {}
        try:
            context_pack = self._context_pack(
                query=query,
                project=project,
                topic_path=topic_path,
                retrieval_mode=retrieval_mode,
            )
        except Exception as exc:
            expansion_steps.append(f"context_pack_failed:{exc}")

        primary: dict[str, Any] = {}
        try:
            primary = self._search(
                query=query,
                project=project,
                topic_path=topic_path,
                retrieval_mode=retrieval_mode,
                deep_async=False,
            )
            responses.append(primary)
        except Exception as exc:
            expansion_steps.append(f"primary_search_failed:{exc}")

        if context_pack.get("context_pack") and isinstance(context_pack["context_pack"], dict):
            # Promote context-pack content into a search-like payload for merge logic.
            cp = context_pack["context_pack"]
            pseudo = {
                "results": cp.get("results") if isinstance(cp.get("results"), list) else [],
                "grounding": {
                    "facts": cp.get("facts") if isinstance(cp.get("facts"), list) else [],
                    "numeric_facts": cp.get("numericFacts") if isinstance(cp.get("numericFacts"), list) else [],
                    "strict_numeric_copy": bool(cp.get("strictNumericCopy", True)),
                },
                "warnings": context_pack.get("warnings") if isinstance(context_pack.get("warnings"), list) else [],
                "degraded": False,
                "result_state": "ready",
            }
            responses.append(pseudo)

        context_pack_quality = (
            context_pack.get("context_pack_quality")
            if isinstance(context_pack.get("context_pack_quality"), dict)
            else {}
        )
        nested_context_pack = context_pack.get("context_pack") if isinstance(context_pack.get("context_pack"), dict) else {}
        if not context_pack_quality:
            context_pack_quality = (
                nested_context_pack.get("context_pack_quality")
                if isinstance(nested_context_pack.get("context_pack_quality"), dict)
                else nested_context_pack.get("contextPackQuality")
                if isinstance(nested_context_pack.get("contextPackQuality"), dict)
                else {}
            )
        token_impact = (
            context_pack.get("token_impact")
            if isinstance(context_pack.get("token_impact"), dict)
            else nested_context_pack.get("token_impact")
            if isinstance(nested_context_pack.get("token_impact"), dict)
            else nested_context_pack.get("tokenImpact")
            if isinstance(nested_context_pack.get("tokenImpact"), dict)
            else {}
        )

        lifecycle = self._lifecycle_from_response(primary if primary else {})
        results_now = primary.get("results") if isinstance(primary.get("results"), list) else []
        facts_now = (
            primary.get("grounding", {}).get("facts")
            if isinstance(primary.get("grounding"), dict)
            and isinstance(primary.get("grounding", {}).get("facts"), list)
            else []
        )
        should_expand = (
            len(results_now) < self.min_results
            or len(facts_now) < self.min_facts
            or bool(lifecycle.get("degraded"))
        )

        if should_expand and topic_path:
            try:
                broader = self._search(
                    query=query,
                    project=project,
                    topic_path=None,
                    retrieval_mode=retrieval_mode,
                    deep_async=False,
                )
                responses.append(broader)
                broadened_scope = True
                expansion_steps.append("broadened_scope_once")
                broader_results = broader.get("results") if isinstance(broader.get("results"), list) else []
                broader_facts = (
                    broader.get("grounding", {}).get("facts")
                    if isinstance(broader.get("grounding"), dict)
                    and isinstance(broader.get("grounding", {}).get("facts"), list)
                    else []
                )
                if len(broader_results) >= self.min_results and len(broader_facts) >= self.min_facts:
                    should_expand = False
            except Exception as exc:
                expansion_steps.append(f"broaden_scope_failed:{exc}")

        if should_expand and self.deep_escalation_enabled:
            try:
                deep = self._search(
                    query=query,
                    project=project,
                    topic_path=None,
                    retrieval_mode="deep",
                    deep_async=True,
                )
                responses.append(deep)
                deep_escalated = True
                expansion_steps.append("deep_escalation_requested")
                token = str(deep.get("token") or deep.get("job_id") or "").strip()
                if token:
                    deadline = time.monotonic() + self.deep_poll_secs
                    while time.monotonic() < deadline:
                        try:
                            polled = self._request_json(
                                "GET",
                                f"/memory/search/continuations/{urllib.parse.quote(token, safe='')}"
                                "?include_result=true",
                                None,
                                timeout=max(self.retry_timeout, 10.0),
                            )
                        except Exception:
                            break
                        status = str(polled.get("status") or "").lower()
                        if status in {"completed", "failed"}:
                            final_payload = polled.get("result") if isinstance(polled.get("result"), dict) else polled
                            if isinstance(final_payload, dict):
                                responses.append(final_payload)
                                expansion_steps.append(f"deep_escalation_{status}")
                            break
                        time.sleep(self.deep_poll_interval)
            except Exception as exc:
                expansion_steps.append(f"deep_escalation_failed:{exc}")

        merged_results, merged_facts, merged_numeric, warnings = self._merge_responses(responses)
        l0_facts = _slice_with_budget(merged_facts, _fact_text, self.budget_l0)

        rollup_rows = [row for row in merged_results if str(row.get("file") or "").startswith("_rollups/") or isinstance(row.get("topic_rollup"), dict)]
        l1_rollups = _slice_with_budget(rollup_rows, _result_summary, self.budget_l1)

        raw_refs = self._extract_raw_refs(merged_results)
        l2_raw_refs = _slice_with_budget(raw_refs, lambda row: str(row.get("summary") or ""), self.budget_l2)
        tool_slices = self._build_tool_slices(tools, l0_facts, l2_raw_refs)

        final_lifecycle = self._lifecycle_from_response(responses[-1] if responses else {})
        if not final_lifecycle.get("status") or final_lifecycle.get("status") == "unknown":
            final_lifecycle = lifecycle
        final_lifecycle["returned_now"] = _unique_ordered(
            list(final_lifecycle.get("returned_now") or [])
            + list((primary.get("source_summary") or {}).get("returned_now", []) if isinstance(primary, dict) else [])
        )
        final_lifecycle["pending_sources"] = _unique_ordered(
            list(final_lifecycle.get("pending_sources") or [])
            + list((primary.get("source_summary") or {}).get("pending_sources", []) if isinstance(primary, dict) else [])
        )

        return {
            "enabled": True,
            "query": query,
            "project": project,
            "topic_path": topic_path,
            "retrieval_mode": retrieval_mode,
            "lifecycle": final_lifecycle,
            "warnings": warnings,
            "layers": {
                "l0_facts": l0_facts,
                "l1_rollups": l1_rollups,
                "l2_raw_refs": l2_raw_refs,
            },
            "numeric_facts": merged_numeric[: self.max_facts],
            "tool_slices": tool_slices,
            "context_pack_quality": context_pack_quality,
            "token_impact": token_impact,
            "expansion": {
                "broadened_scope": broadened_scope,
                "deep_escalated": deep_escalated,
                "steps": expansion_steps,
            },
        }

    def render_for_prompt(self, bundle: dict[str, Any]) -> str:
        lifecycle = bundle.get("lifecycle") if isinstance(bundle.get("lifecycle"), dict) else {}
        layers = bundle.get("layers") if isinstance(bundle.get("layers"), dict) else {}
        l0 = layers.get("l0_facts") if isinstance(layers.get("l0_facts"), list) else []
        l1 = layers.get("l1_rollups") if isinstance(layers.get("l1_rollups"), list) else []
        l2 = layers.get("l2_raw_refs") if isinstance(layers.get("l2_raw_refs"), list) else []
        numeric = bundle.get("numeric_facts") if isinstance(bundle.get("numeric_facts"), list) else []
        tool_slices = bundle.get("tool_slices") if isinstance(bundle.get("tool_slices"), dict) else {}
        warnings = bundle.get("warnings") if isinstance(bundle.get("warnings"), list) else []
        expansion = bundle.get("expansion") if isinstance(bundle.get("expansion"), dict) else {}

        lines: list[str] = []
        lines.append("Context Expansion Pack (factual-first)")
        lines.append(
            "Lifecycle: "
            f"status={lifecycle.get('status')} "
            f"result_state={lifecycle.get('result_state')} "
            f"degraded={bool(lifecycle.get('degraded', False))}"
        )
        pending = lifecycle.get("pending_sources") if isinstance(lifecycle.get("pending_sources"), list) else []
        returned = lifecycle.get("returned_now") if isinstance(lifecycle.get("returned_now"), list) else []
        if returned:
            lines.append(f"Returned now: {', '.join(returned)}")
        if pending:
            lines.append(f"Pending sources: {', '.join(pending)}")
        if warnings:
            lines.append("Warnings:")
            for warning in warnings[:6]:
                lines.append(f"- {warning}")
        steps = expansion.get("steps") if isinstance(expansion.get("steps"), list) else []
        if steps:
            lines.append(f"Expansion steps: {', '.join(str(step) for step in steps)}")

        lines.append("L0 Facts:")
        for fact in l0[: self.max_facts]:
            source = fact.get("source") if isinstance(fact.get("source"), dict) else {}
            src_file = str(source.get("file") or fact.get("file") or "").strip()
            src_source = str(source.get("source") or fact.get("source") or "").strip()
            text = _truncate_by_tokens(_fact_text(fact), 100)
            if src_file or src_source:
                lines.append(f"- [{src_source}:{src_file}] {text}")
            else:
                lines.append(f"- {text}")

        if numeric:
            lines.append("Numeric Facts (verbatim copy):")
            for entry in numeric[: self.max_facts]:
                lines.append(f"- {json.dumps(entry, sort_keys=True)}")

        if l1:
            lines.append("L1 Rollups:")
            for row in l1[: self.max_results]:
                file_name = str(row.get("file") or "").strip()
                summary = _truncate_by_tokens(_result_summary(row), 80)
                lines.append(f"- [{file_name}] {summary}")

        if l2:
            lines.append("L2 Raw Refs:")
            for ref in l2[: self.max_results]:
                lines.append(
                    f"- [{ref.get('project')}:{ref.get('file')}] topic={ref.get('topic_path')}"
                )

        if tool_slices:
            lines.append("Tool Context Slices:")
            for tool_name, payload in tool_slices.items():
                facts = payload.get("facts") if isinstance(payload.get("facts"), list) else []
                refs = payload.get("raw_refs") if isinstance(payload.get("raw_refs"), list) else []
                lines.append(
                    f"- {tool_name}: facts={len(facts)} raw_refs={len(refs)}"
                )
                for fact in facts[:2]:
                    lines.append(f"  fact: {_truncate_by_tokens(_fact_text(fact), 48)}")
                for ref in refs[:2]:
                    lines.append(f"  ref: {ref.get('file')}")

        rendered = "\n".join(lines).strip()
        if len(rendered) > self.prompt_budget:
            rendered = rendered[: self.prompt_budget - 1] + "…"
        return rendered

    def write_checkpoint(
        self,
        *,
        task: dict[str, Any],
        bundle: dict[str, Any],
        output: str,
        provider: str,
        model: str,
        status: str,
    ) -> None:
        payload = task.get("payload") if isinstance(task.get("payload"), dict) else {}
        topic_path = str(
            payload.get("topic_path")
            or payload.get("topicPath")
            or "agent/checkpoints"
        ).strip()
        project = _parse_project(task)
        task_id = str(task.get("id") or f"adhoc-{int(time.time())}").strip()
        safe_task = re.sub(r"[^a-zA-Z0-9._-]+", "_", task_id)[:80] or "task"
        file_name = f"notes/agent-checkpoints/{datetime.now(timezone.utc).strftime('%Y-%m-%dT%H%M%SZ')}_{safe_task}_context_expansion.md"
        lifecycle = bundle.get("lifecycle") if isinstance(bundle.get("lifecycle"), dict) else {}
        expansion = bundle.get("expansion") if isinstance(bundle.get("expansion"), dict) else {}
        decisions = _extract_decision_lines(output)
        unresolved = _extract_unresolved_lines(output)
        lines: list[str] = []
        lines.append(f"checkpoint_at: {_utc_now()}")
        lines.append(f"task_id: {task_id}")
        lines.append(f"status: {status}")
        lines.append(f"provider: {provider}")
        lines.append(f"model: {model}")
        lines.append(f"query: {bundle.get('query')}")
        lines.append(f"topic_path: {topic_path}")
        lines.append(
            f"lifecycle: status={lifecycle.get('status')} result_state={lifecycle.get('result_state')} degraded={lifecycle.get('degraded')}"
        )
        pending = lifecycle.get("pending_sources") if isinstance(lifecycle.get("pending_sources"), list) else []
        if pending:
            lines.append("pending_sources:")
            for source in pending:
                lines.append(f"- {source}")
        steps = expansion.get("steps") if isinstance(expansion.get("steps"), list) else []
        if steps:
            lines.append("expansion_steps:")
            for step in steps:
                lines.append(f"- {step}")
        if decisions:
            lines.append("decisions:")
            for item in decisions:
                lines.append(f"- {item}")
        if unresolved:
            lines.append("unresolved_questions:")
            for item in unresolved:
                lines.append(f"- {item}")
        lines.append("output_excerpt:")
        lines.append(_truncate_by_tokens(str(output or "").strip(), 400))
        content = "\n".join(lines).strip()
        payload = {
            "projectName": project,
            "fileName": file_name,
            "content": content,
            "topicPath": topic_path,
        }
        try:
            self._request_json("POST", "/memory/write", payload)
        except Exception:
            # Checkpointing must never block task completion.
            return
