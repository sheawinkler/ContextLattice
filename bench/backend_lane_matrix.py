#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import math
import statistics
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import httpx


def percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    if len(values) == 1:
        return float(values[0])
    idx = (len(values) - 1) * pct
    low = int(math.floor(idx))
    high = min(len(values) - 1, low + 1)
    weight = idx - low
    return (values[low] * (1.0 - weight)) + (values[high] * weight)


def _compact_float(value: float) -> float:
    return round(float(value), 3)


def _derive_policy_mismatch(
    requested: dict[str, Any] | None,
    effective: dict[str, Any] | None,
) -> dict[str, Any]:
    req = requested if isinstance(requested, dict) else {}
    eff = effective if isinstance(effective, dict) else {}
    mismatched: dict[str, Any] = {}
    for key, req_value in req.items():
        eff_value = eff.get(key)
        if req_value != eff_value:
            mismatched[key] = {"requested": req_value, "effective": eff_value}
    return mismatched


@dataclass(frozen=True)
class CaseConfig:
    query: str
    retrieval_mode: str
    limit: int


@dataclass(frozen=True)
class ProfileConfig:
    description: str
    data_store: str
    data_model: str
    index_type: str
    search_type: str
    sources: list[str]
    source_weights: dict[str, float]
    backend_policy: dict[str, Any]


CASES: dict[str, CaseConfig] = {
    "short_context": CaseConfig(
        query="source quality qdrant topic rollups",
        retrieval_mode="fast",
        limit=8,
    ),
    "deep_read": CaseConfig(
        query="deep retrieval stability timeout source quality",
        retrieval_mode="deep",
        limit=16,
    ),
    "ops_focus": CaseConfig(
        query="fanout queue pressure deadletters backlog quality",
        retrieval_mode="balanced",
        limit=12,
    ),
}


PROFILES: dict[str, ProfileConfig] = {
    "baseline_qdrant_rollups": ProfileConfig(
        description="Control lane with explicit qdrant + topic_rollups only.",
        data_store="qdrant+topic_rollups",
        data_model="dense-vector+object-rollup",
        index_type="hnsw+partitioned-rollup",
        search_type="semantic+rollup-hybrid",
        sources=["qdrant", "topic_rollups"],
        source_weights={"qdrant": 1.0, "topic_rollups": 0.9},
        backend_policy={
            "vector_backend": "auto",
            "lexical_backend": "auto",
            "memory_bank_backend": "native",
            "strict": False,
        },
    ),
    "rust_lane_usearch_tantivy": ProfileConfig(
        description="Rust-first lane request: usearch_ann + tantivy_lexical.",
        data_store="qdrant+topic_rollups+memory_bank(native)",
        data_model="dense-vector+lexical",
        index_type="usearch-ann+tantivy-lexical",
        search_type="hybrid-semantic-lexical",
        sources=["qdrant", "topic_rollups", "memory_bank"],
        source_weights={"qdrant": 1.0, "topic_rollups": 0.9, "memory_bank": 0.6},
        backend_policy={
            "vector_backend": "usearch_ann",
            "lexical_backend": "tantivy_lexical",
            "memory_bank_backend": "native",
            "strict": True,
        },
    ),
    "memory_bank_meilisearch_spike": ProfileConfig(
        description="Memory-bank spike lane request: meilisearch_spike.",
        data_store="memory_bank(meilisearch)+qdrant+topic_rollups",
        data_model="lexical+semantic",
        index_type="meilisearch+hnsw",
        search_type="hybrid-meili-semantic",
        sources=["qdrant", "topic_rollups", "memory_bank"],
        source_weights={"qdrant": 1.0, "topic_rollups": 0.9, "memory_bank": 0.6},
        backend_policy={
            "vector_backend": "auto",
            "lexical_backend": "auto",
            "memory_bank_backend": "meilisearch_spike",
            "strict": False,
        },
    ),
    "memory_bank_quickwit_spike": ProfileConfig(
        description="Memory-bank spike lane request: quickwit_spike.",
        data_store="memory_bank(quickwit_compat)+qdrant+topic_rollups",
        data_model="lexical+semantic",
        index_type="inverted-index+hnsw",
        search_type="hybrid-quickwit-compat-semantic",
        sources=["qdrant", "topic_rollups", "memory_bank"],
        source_weights={"qdrant": 1.0, "topic_rollups": 0.9, "memory_bank": 0.6},
        backend_policy={
            "vector_backend": "auto",
            "lexical_backend": "auto",
            "memory_bank_backend": "quickwit_spike",
            "strict": False,
        },
    ),
    "memory_bank_tantivy_spike": ProfileConfig(
        description="Memory-bank spike lane request: tantivy_spike.",
        data_store="memory_bank(tantivy)+qdrant+topic_rollups",
        data_model="lexical+semantic",
        index_type="tantivy-inverted+hnsw",
        search_type="hybrid-tantivy-semantic",
        sources=["qdrant", "topic_rollups", "memory_bank"],
        source_weights={"qdrant": 1.0, "topic_rollups": 0.9, "memory_bank": 0.6},
        backend_policy={
            "vector_backend": "auto",
            "lexical_backend": "auto",
            "memory_bank_backend": "tantivy_spike",
            "strict": False,
        },
    ),
}


def _seed_items(project: str) -> list[dict[str, Any]]:
    return [
        {
            "project": project,
            "file_name": "bench/seed/source_quality.md",
            "topic_path": "benchmarks/retrieval/source_quality",
            "content": (
                "source quality qdrant topic rollups retrieval coverage baseline. "
                "staged retrieval should return grounded rows quickly."
            ),
        },
        {
            "project": project,
            "file_name": "bench/seed/deep_stability.md",
            "topic_path": "benchmarks/retrieval/deep_stability",
            "content": (
                "deep retrieval stability timeout source quality read performance. "
                "prefer grounded context and deferred slow-source continuation."
            ),
        },
        {
            "project": project,
            "file_name": "bench/seed/ops_queue.md",
            "topic_path": "benchmarks/retrieval/ops",
            "content": (
                "fanout queue pressure deadletters backlog quality diagnostics. "
                "operator-facing retrieval should surface returned and pending sources."
            ),
        },
    ]


def _seed_benchmark_corpus(
    client: httpx.Client,
    *,
    base_url: str,
    headers: dict[str, str],
    project: str,
) -> dict[str, Any]:
    seeded = {"attempted": 0, "succeeded": 0, "errors": [], "verified": False, "verificationResults": 0}
    for item in _seed_items(project):
        seeded["attempted"] += 1
        try:
            resp = client.post(
                f"{base_url}/v1/memory/put",
                headers=headers,
                json={"item": item},
            )
            if resp.status_code >= 400:
                seeded["errors"].append(f"{resp.status_code}:{resp.text[:180]}")
                continue
            seeded["succeeded"] += 1
        except Exception as exc:
            seeded["errors"].append(str(exc))
    if seeded["succeeded"] == 0:
        return seeded
    time.sleep(1.2)
    verify_payload = {
        "request": {
            "query": CASES["short_context"].query,
            "project": project,
            "limit": 5,
            "retrieval_mode": "fast",
            "sources": ["qdrant", "topic_rollups"],
            "traffic_class": "benchmark",
        }
    }
    for _ in range(4):
        try:
            resp = client.post(
                f"{base_url}/v1/retrieval/query-with-grounding",
                headers=headers,
                json=verify_payload,
            )
            if resp.status_code < 400:
                payload = resp.json()
                rows = payload.get("results") if isinstance(payload, dict) else []
                count = len(rows) if isinstance(rows, list) else 0
                seeded["verificationResults"] = count
                if count > 0:
                    seeded["verified"] = True
                    break
        except Exception:
            pass
        time.sleep(0.8)
    return seeded


def _fetch_retrieval_telemetry(client: httpx.Client, base_url: str, headers: dict[str, str]) -> dict[str, Any]:
    try:
        resp = client.get(f"{base_url}/telemetry/retrieval", headers=headers)
        if resp.status_code >= 400:
            return {}
        payload = resp.json()
        if not isinstance(payload, dict):
            return {}
        return payload
    except Exception:
        return {}


def _extract_memory_bank_backend_snapshot(payload: dict[str, Any]) -> dict[str, Any]:
    state = payload.get("memoryBankBackend")
    if not isinstance(state, dict):
        return {}
    return {
        "defaultEnabled": bool(state.get("defaultEnabled")),
        "mode": str(state.get("mode") or ""),
        "spikeUrlConfigured": bool(state.get("spikeUrlConfigured")),
        "attempts": int(state.get("attempts") or 0),
        "successes": int(state.get("successes") or 0),
        "failures": int(state.get("failures") or 0),
        "fallbacks": int(state.get("fallbacks") or 0),
        "lastError": state.get("lastError"),
        "lastLatencyMs": state.get("lastLatencyMs"),
    }


def _extract_source_latency_snapshot(payload: dict[str, Any]) -> dict[str, Any]:
    latency = payload.get("latency") if isinstance(payload.get("latency"), dict) else {}
    sources = latency.get("sources") if isinstance(latency.get("sources"), dict) else {}
    out: dict[str, Any] = {}
    for source_name in sorted(sources.keys()):
        row = sources.get(source_name)
        if not isinstance(row, dict):
            continue
        out[str(source_name)] = {
            "requests": int(row.get("requests") or 0),
            "errors": int(row.get("errors") or 0),
            "timeouts": int(row.get("timeouts") or 0),
            "budgetExceeded": int(row.get("budgetExceeded") or 0),
            "p50Ms": float(row.get("p50Ms") or 0.0),
            "p95Ms": float(row.get("p95Ms") or 0.0),
            "p99Ms": float(row.get("p99Ms") or 0.0),
        }
    return out


def _delta_counter(after: dict[str, Any], before: dict[str, Any], key: str) -> int:
    return int(after.get(key) or 0) - int(before.get(key) or 0)


def run_case(
    client: httpx.Client,
    *,
    base_url: str,
    headers: dict[str, str],
    profile_name: str,
    profile: ProfileConfig,
    case_name: str,
    case: CaseConfig,
    project: str,
    runs: int,
    cache_bust: bool,
) -> dict[str, Any]:
    latencies: list[float] = []
    top_scores: list[float] = []
    result_counts: list[int] = []
    status_codes: list[int] = []
    error_messages: list[str] = []
    cache_hits = 0
    policy_mismatches: list[dict[str, Any]] = []
    effective_policy_samples: list[dict[str, Any]] = []
    source_count_samples: list[dict[str, int]] = []

    for idx in range(max(1, runs)):
        q = case.query
        if cache_bust:
            q = f"{q} :: profile={profile_name} case={case_name} run={idx + 1}"
        req_payload = {
            "query": q,
            "project": project,
            "limit": case.limit,
            "retrieval_mode": case.retrieval_mode,
            "retrieval_intent": "decision",
            "bypass_pathway_cache": True,
            "sources": list(profile.sources),
            "source_weights": dict(profile.source_weights),
            "backend_policy": dict(profile.backend_policy),
            "include_retrieval_debug": True,
            "traffic_class": "benchmark",
        }
        started = time.perf_counter()
        try:
            resp = client.post(
                f"{base_url}/v1/retrieval/query-with-grounding",
                headers=headers,
                json={"request": req_payload},
            )
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            latencies.append(elapsed_ms)
            status_codes.append(int(resp.status_code))
            if resp.status_code >= 400:
                error_messages.append(f"{resp.status_code}:{resp.text[:180]}")
                continue
            payload = resp.json()
            results = payload.get("results") if isinstance(payload, dict) else []
            retrieval_debug = payload.get("retrieval_debug") if isinstance(payload, dict) else {}
            if isinstance(retrieval_debug, dict):
                cache = retrieval_debug.get("cache")
                if isinstance(cache, dict) and bool(cache.get("pathway_hit")):
                    cache_hits += 1
                requested_policy = (
                    retrieval_debug.get("policy", {}).get("runtimeBackendPolicy")
                    if isinstance(retrieval_debug.get("policy"), dict)
                    else None
                )
                effective_policy = (
                    retrieval_debug.get("source_policy", {}).get("runtime_backend_policy")
                    if isinstance(retrieval_debug.get("source_policy"), dict)
                    else None
                )
                if isinstance(effective_policy, dict):
                    effective_policy_samples.append(effective_policy)
                mismatch = _derive_policy_mismatch(requested_policy, effective_policy)
                if mismatch:
                    policy_mismatches.append(mismatch)
                source_counts = retrieval_debug.get("source_counts")
                if isinstance(source_counts, dict):
                    source_count_samples.append(
                        {
                            str(k): int(v or 0)
                            for k, v in source_counts.items()
                        }
                    )
            if isinstance(results, list):
                result_counts.append(len(results))
                top = results[0] if results else {}
                top_scores.append(float(top.get("score") or 0.0) if isinstance(top, dict) else 0.0)
            else:
                result_counts.append(0)
                top_scores.append(0.0)
        except Exception as exc:
            elapsed_ms = (time.perf_counter() - started) * 1000.0
            latencies.append(elapsed_ms)
            status_codes.append(0)
            error_messages.append(str(exc))

    latencies.sort()
    status_failures = sum(1 for code in status_codes if code >= 400 or code == 0)
    error_rate = float(status_failures) / float(max(1, len(status_codes)))
    return {
        "runs": len(status_codes),
        "statusCodes": status_codes,
        "errors": error_messages,
        "errorCount": len(error_messages),
        "errorRate": round(error_rate, 6),
        "cacheHitCount": cache_hits,
        "cacheHitRate": round(float(cache_hits) / float(max(1, len(status_codes))), 6),
        "latencyMs": {
            "p50": _compact_float(percentile(latencies, 0.50)),
            "p95": _compact_float(percentile(latencies, 0.95)),
            "p99": _compact_float(percentile(latencies, 0.99)),
            "mean": _compact_float(statistics.mean(latencies) if latencies else 0.0),
            "min": _compact_float(min(latencies) if latencies else 0.0),
            "max": _compact_float(max(latencies) if latencies else 0.0),
        },
        "resultCount": {
            "mean": _compact_float(statistics.mean(result_counts) if result_counts else 0.0),
            "min": int(min(result_counts) if result_counts else 0),
            "max": int(max(result_counts) if result_counts else 0),
        },
        "topScore": {
            "mean": _compact_float(statistics.mean(top_scores) if top_scores else 0.0),
            "max": _compact_float(max(top_scores) if top_scores else 0.0),
        },
        "policyMismatchCount": len(policy_mismatches),
        "policyMismatchesSample": policy_mismatches[:3],
        "effectivePolicySample": effective_policy_samples[-1] if effective_policy_samples else {},
        "sourceCountsSample": source_count_samples[-1] if source_count_samples else {},
    }


def _summarize_profile(profile_cases: dict[str, dict[str, Any]]) -> dict[str, Any]:
    p95_values: list[float] = []
    error_rates: list[float] = []
    policy_mismatch_total = 0
    for payload in profile_cases.values():
        latency = payload.get("latencyMs") if isinstance(payload.get("latencyMs"), dict) else {}
        p95_values.append(float(latency.get("p95") or 0.0))
        error_rates.append(float(payload.get("errorRate") or 0.0))
        policy_mismatch_total += int(payload.get("policyMismatchCount") or 0)
    return {
        "avgP95Ms": _compact_float(statistics.mean(p95_values) if p95_values else 0.0),
        "maxP95Ms": _compact_float(max(p95_values) if p95_values else 0.0),
        "avgErrorRate": round(statistics.mean(error_rates) if error_rates else 0.0, 6),
        "policyMismatchTotal": policy_mismatch_total,
    }


def run(args: argparse.Namespace) -> dict[str, Any]:
    base_url = str(args.base_url).rstrip("/")
    headers = {"content-type": "application/json"}
    if args.api_key:
        headers["x-api-key"] = args.api_key

    requested_profiles = [token.strip() for token in str(args.profiles).split(",") if token.strip()]
    if not requested_profiles:
        requested_profiles = list(PROFILES.keys())
    unknown_profiles = [name for name in requested_profiles if name not in PROFILES]
    if unknown_profiles:
        raise SystemExit(f"Unknown profile(s): {', '.join(unknown_profiles)}")

    selected_cases = [token.strip() for token in str(args.cases).split(",") if token.strip()]
    if not selected_cases:
        selected_cases = list(CASES.keys())
    unknown_cases = [name for name in selected_cases if name not in CASES]
    if unknown_cases:
        raise SystemExit(f"Unknown case(s): {', '.join(unknown_cases)}")

    results: dict[str, Any] = {}
    baseline_profile = "baseline_qdrant_rollups"
    baseline_avg_p95 = None

    seed_state: dict[str, Any] = {"attempted": 0, "succeeded": 0, "errors": [], "verified": False, "verificationResults": 0}
    with httpx.Client(timeout=float(args.timeout)) as client:
        if bool(args.seed_corpus):
            seed_state = _seed_benchmark_corpus(
                client,
                base_url=base_url,
                headers=headers,
                project=args.project,
            )
        overall_before = _fetch_retrieval_telemetry(client, base_url, headers)
        overall_before_mb = _extract_memory_bank_backend_snapshot(overall_before)
        overall_before_sources = _extract_source_latency_snapshot(overall_before)
        for profile_name in requested_profiles:
            profile = PROFILES[profile_name]
            profile_before = _fetch_retrieval_telemetry(client, base_url, headers)
            profile_before_mb = _extract_memory_bank_backend_snapshot(profile_before)
            case_rows: dict[str, Any] = {}
            for case_name in selected_cases:
                case_rows[case_name] = run_case(
                    client,
                    base_url=base_url,
                    headers=headers,
                    profile_name=profile_name,
                    profile=profile,
                    case_name=case_name,
                    case=CASES[case_name],
                    project=args.project,
                    runs=max(1, int(args.runs)),
                    cache_bust=bool(args.cache_bust),
                )
            profile_after = _fetch_retrieval_telemetry(client, base_url, headers)
            profile_after_mb = _extract_memory_bank_backend_snapshot(profile_after)
            profile_summary = _summarize_profile(case_rows)
            if profile_name == baseline_profile:
                baseline_avg_p95 = float(profile_summary.get("avgP95Ms") or 0.0)
            delta = {
                "attempts": _delta_counter(profile_after_mb, profile_before_mb, "attempts"),
                "successes": _delta_counter(profile_after_mb, profile_before_mb, "successes"),
                "failures": _delta_counter(profile_after_mb, profile_before_mb, "failures"),
                "fallbacks": _delta_counter(profile_after_mb, profile_before_mb, "fallbacks"),
            }
            spike_backend_requested = str(profile.backend_policy.get("memory_bank_backend") or "native")
            spike_backend_enabled = spike_backend_requested not in {"", "native", "disabled"}
            profile_notes: list[str] = []
            if spike_backend_enabled:
                if not bool(profile_before_mb.get("spikeUrlConfigured")):
                    profile_notes.append("memory-bank spike URL is not configured; spike backends can only fall back to native.")
                if delta["attempts"] <= 0:
                    profile_notes.append("no spike backend attempts were recorded for this profile.")
                elif delta["successes"] <= 0:
                    profile_notes.append("spike backend attempts did not succeed during this run.")
            if int(profile_summary.get("policyMismatchTotal") or 0) > 0:
                profile_notes.append("requested backend policy differed from effective runtime policy on some runs.")
            results[profile_name] = {
                "description": profile.description,
                "profileDescriptor": {
                    "data_store": profile.data_store,
                    "data_model": profile.data_model,
                    "index_type": profile.index_type,
                    "search_type": profile.search_type,
                },
                "request": {
                    "sources": profile.sources,
                    "source_weights": profile.source_weights,
                    "backend_policy": profile.backend_policy,
                },
                "cases": case_rows,
                "summary": profile_summary,
                "memoryBankBackend": {
                    "before": profile_before_mb,
                    "after": profile_after_mb,
                    "delta": delta,
                },
                "notes": profile_notes,
            }
        overall_after = _fetch_retrieval_telemetry(client, base_url, headers)
        overall_after_mb = _extract_memory_bank_backend_snapshot(overall_after)
        overall_after_sources = _extract_source_latency_snapshot(overall_after)

    for profile_name, profile_payload in results.items():
        summary = profile_payload.get("summary", {})
        avg_p95 = float(summary.get("avgP95Ms") or 0.0)
        if baseline_avg_p95 and baseline_avg_p95 > 0.0:
            delta_pct = ((baseline_avg_p95 - avg_p95) / baseline_avg_p95) * 100.0
            summary["avgP95DeltaVsBaselinePct"] = round(delta_pct, 3)
        else:
            summary["avgP95DeltaVsBaselinePct"] = 0.0

    recommendations: list[str] = []
    rust_summary = results.get("rust_lane_usearch_tantivy", {}).get("summary", {})
    rust_delta = float(rust_summary.get("avgP95DeltaVsBaselinePct") or 0.0)
    if rust_delta > 0.0:
        recommendations.append(
            f"Rust lane request improved avg p95 by {rust_delta:.3f}% vs baseline in this run."
        )
    elif rust_delta < 0.0:
        recommendations.append(
            f"Rust lane request regressed avg p95 by {abs(rust_delta):.3f}% vs baseline in this run."
        )
    else:
        recommendations.append("Rust lane request was neutral vs baseline in this run.")

    spike_profiles = [
        "memory_bank_meilisearch_spike",
        "memory_bank_quickwit_spike",
        "memory_bank_tantivy_spike",
    ]
    if any(
        not bool(results.get(name, {}).get("memoryBankBackend", {}).get("before", {}).get("spikeUrlConfigured"))
        for name in spike_profiles
        if name in results
    ):
        recommendations.append(
            "Memory-bank spike sidecar URL is not configured; meilisearch/quickwit/tantivy spikes are currently fallback-only measurements."
        )
    if bool(args.seed_corpus) and not bool(seed_state.get("verified")):
        recommendations.append(
            "Benchmark seed corpus writes did not verify as retrievable before matrix run; recall coverage may be sparse."
        )

    return {
        "generatedAt": datetime.now(timezone.utc).isoformat(),
        "baseUrl": base_url,
        "project": args.project,
        "runsPerCase": int(args.runs),
        "cases": selected_cases,
        "profiles": requested_profiles,
        "cacheBustQueries": bool(args.cache_bust),
        "seedCorpus": bool(args.seed_corpus),
        "seedState": seed_state,
        "results": results,
        "overallMemoryBankBackend": {
            "before": overall_before_mb,
            "after": overall_after_mb,
            "delta": {
                "attempts": _delta_counter(overall_after_mb, overall_before_mb, "attempts"),
                "successes": _delta_counter(overall_after_mb, overall_before_mb, "successes"),
                "failures": _delta_counter(overall_after_mb, overall_before_mb, "failures"),
                "fallbacks": _delta_counter(overall_after_mb, overall_before_mb, "fallbacks"),
            },
        },
        "overallSourceLatency": {
            "before": overall_before_sources,
            "after": overall_after_sources,
        },
        "recommendations": recommendations,
    }


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Backend lane benchmark matrix (baseline qdrant+rollups vs lexical spikes vs rust lane)."
    )
    parser.add_argument("--base-url", default="http://127.0.0.1:8075")
    parser.add_argument("--api-key", default="")
    parser.add_argument("--project", default="perf_backend_lanes")
    parser.add_argument("--runs", type=int, default=3)
    parser.add_argument("--timeout", type=float, default=90.0)
    parser.add_argument(
        "--profiles",
        default="baseline_qdrant_rollups,rust_lane_usearch_tantivy,memory_bank_meilisearch_spike,memory_bank_quickwit_spike,memory_bank_tantivy_spike",
    )
    parser.add_argument("--cases", default="short_context,deep_read,ops_focus")
    parser.add_argument("--cache-bust", action="store_true", default=True)
    parser.add_argument("--no-cache-bust", dest="cache_bust", action="store_false")
    parser.add_argument("--seed-corpus", action="store_true", default=True)
    parser.add_argument("--no-seed-corpus", dest="seed_corpus", action="store_false")
    parser.add_argument("--output", default="")
    args = parser.parse_args()

    payload = run(args)
    rendered = json.dumps(payload, indent=2, sort_keys=True)
    print(rendered)

    output = args.output or (
        "bench/results/backend_lane_matrix_"
        + datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        + ".json"
    )
    output_path = Path(output)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    output_path.write_text(rendered + "\n", encoding="utf-8")
    latest_path = output_path.parent / "backend_lane_matrix_latest.json"
    latest_path.write_text(rendered + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
