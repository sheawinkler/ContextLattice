# gateway-go

Go ingress gateway for ContextLattice runtime service-mode APIs.

Responsibilities:
- Provide a stable Go front-door for retrieval + memory engine APIs.
- Proxy `/v1/retrieval/*` and `/v1/memory/*` calls to the backend engine URL.
- Keep Python as backend fallback while preserving a Go-first network path.
- Serve Go-native storage governance ops endpoints:
  - `GET /telemetry/storage`
  - `GET /telemetry/storage/ledger`
  - `POST /maintenance/storage/run`
  - `POST /maintenance/telemetry/blob-gc`

Env:
- `PORT` (default `8091`)
- `BACKEND_URL` (default `http://contextlattice-orchestrator:8075`)
- `GATEWAY_PROXY_TIMEOUT_SECS` (default `95`)
- `CONTEXTLATTICE_ORCHESTRATOR_API_KEY` (optional injected key for `/tools/*` calls)
- `CONTEXTLATTICE_WORKER_API_KEY` (optional worker key for role-split tool policy)
- `GO_TOOL_CALLS_ALLOW_ALL` (default `true`)
- `GO_TOOL_CALLS_ROLE_SPLIT_AUTO` (default `true`, only activates with distinct orchestrator/worker keys)
- `GO_TOOL_CALLS_ROLE_SPLIT_ENABLED` (manual override, default `false`)
- `GO_RETRIEVAL_SYNC_SOURCE_CONCURRENCY_DEFAULT` (default `2`)
- `GO_RETRIEVAL_SYNC_SOURCE_CONCURRENCY_OVERRIDES` (JSON object by source lane)
- `GO_RETRIEVAL_SYNC_QUEUE_AGE_WARN_SECS` (default `2.0`)
- `GO_RETRIEVAL_SYNC_QUEUE_AGE_HIGH_SECS` (default `5.0`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_ENABLED` (default `true`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_QUEUE_RATIO` (default `0.85`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_PENDING_HIGH` (default `max(2, continuation_max_inflight-1)`)
- `GO_RETRIEVAL_CONTINUATION_SHEDDING_SOURCES` (default `letta,memory_bank,mongo_raw,mindsdb`)
- `ORCH_RETRIEVAL_FAIL_OPEN_TIMEOUT_CONTINUATION_SOURCES` (default `letta,memory_bank,mindsdb,mongo_raw,qdrant`)
- `GO_RETRIEVAL_CONTINUATION_DURABLE_MAX_PENDING_PER_SOURCE` (default `24`)
- `GO_WRITE_DEFAULT_AGENT_ID`, `GO_WRITE_DEFAULT_SESSION_ID`, `GO_WRITE_DEFAULT_TAGS` (fallback metadata for writes that omit canonical fields)
- `GO_RETRIEVAL_TIMEOUT_CONTRACT_GRACE_SECS` (default `0.075`)
- `ORCH_STORAGE_LEDGER_PATH` (optional explicit path; default resolves from `GO_MEMORY_STORE_ROOT/_contextlattice/storage_ledger.ndjson`)
- `ORCH_STORAGE_LEDGER_READ_LIMIT_DEFAULT` (default `168`)
- `ORCH_STORAGE_LEDGER_READ_LIMIT_MAX` (default `5000`)

Dream Mode:
- `POST /memory/dream` and `POST /tools/dream`: bounded evidence-linked synthesis that requires successful structured LLM output. Non-LLM evidence packaging belongs to context-pack or review.
- `GO_DREAM_LLM_ENABLED` (default `true`)
- `GO_DREAM_MODEL` (explicit override; deprecated `qwen2.5-coder` is replaced for Dream Mode)
- `GO_DREAM_LLM_TIMEOUT_SECS` (default `600`)
- `GO_DREAM_LLM_MAX_TOKENS` (default `4096`)
- `GO_DREAM_ALLOW_UNCENSORED_MODELS` (default `false`)

Typed memory tools:
- `POST /tools/context_pack`: agent-facing wrapper around `/memory/context-pack`.
- `/memory/context-pack` and `/tools/context_pack` include bounded `agent_guidance` with deterministic evidence themes, risk markers, candidate attention links, missing-evidence signals, and prompt hints. These hints are attention scaffolding for agents/LLMs, not Dream Mode synthesis or final claims.
- `POST /memory/dream` and `POST /tools/dream`: bounded evidence-linked Dream Mode synthesis. Dream Mode requires successful structured LLM synthesis; non-LLM evidence packaging belongs to context-pack or review.
- `GET|POST /memory/review` and `/tools/review`: bounded deterministic review mode for repeated memory patterns, agent write intensity, and mitigation guidance.
- `POST /tools/checkpoint_write`: durable checkpoint write with lifecycle metadata.
- `POST /tools/ephemeral_memory_write`: scratch/test write that is hidden from normal retrieval.
- `POST /tools/ephemeral_memory_purge`: safe-prefix purge with `dry_run=true` by default and `confirm=true` required for deletion.
- `GET|POST /tools/memory_file_get`: exact file read by `project` + `file`.

Dream Mode defaults to `GO_DREAM_LLM_ENABLED=true`. Explicit request `model` and `GO_DREAM_MODEL` are honored unless they name deprecated `qwen2.5-coder`; otherwise Ollama routes can auto-select the best installed local Qwen 3.x model, preferring Qwen3.7/Qwen3.6 over `TASK_MODEL` and excluding abliterated/uncensored variants unless `CONTEXTLATTICE_DREAM_ALLOW_PRIVATE_EVAL_MODELS=true` (`GO_DREAM_ALLOW_UNCENSORED_MODELS=true` is a legacy alias). Qwen3.7-Max is API/proprietary as of May 2026, so use it through `sglang`, `vllm`, `mlx`, `mtplx`, `llama-cpp`, `tgi`, `tensorrt-llm`, or another OpenAI-compatible provider/base URL rather than expecting a local Ollama tag. Large Qwen3.6 local models are advisory opt-in targets only; the default GGUF recommendation is `mudler/Qwen3.6-35B-A3B-Claude-4.7-Opus-Reasoning-Distilled-APEX-MTP-GGUF`, while abliterated variants remain explicit private-eval only. If structured backend LLM synthesis is disabled, unavailable, or unparseable, Dream Mode returns a contract-valid `dream_unavailable` response with zero hypotheses and zero experiments.

Dream Mode reflection is enabled by default. The gateway scores the strongest hypothesis for novelty, evidence linkage, and actionability; if it misses `GO_DREAM_REFLECTION_MIN_SCORE` and LLM use is enabled, one bounded deepening pass asks the backend model for a stronger final JSON synthesis. OpenAI-compatible and Ollama responses must contain final assistant content. Reasoning-only outputs fail with instructions to fix the runtime template; `scripts/inference_template_conformance.sh` checks that contract, and `scripts/inference_mlx_server.sh --template-profile qwen-final-content` starts MLX with a Qwen final-content template.

Long nonlinear synthesis is bounded with infrastructure fail-safes instead of a short 60s cap: `GO_DREAM_LLM_TIMEOUT_SECS` defaults to `600`, `GO_DREAM_LLM_MAX_TOKENS` defaults to `4096`, and final output is clipped to the Dream Mode response contract. CLI wrapper:

```bash
scripts/agent/contextlattice-dream "invent the next memory primitive" --llm-timeout-secs 900 --llm-max-tokens 8192 --pretty
```

Run locally:

```bash
cd services/gateway-go
go run .
```

Default port: `8091`
