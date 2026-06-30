# Local Model Options

ContextLattice treats local models as opt-in inference connectors. It never bundles, pulls, or auto-downloads model weights. Use `/v1/inference/runtime-policy` or `scripts/inference_runtime_policy.sh` to see the live provider, hardware, and shortlist policy.

For GGUF models, llama.cpp is connector-only: run `llama-server` yourself, then point ContextLattice at it with `LLAMA_CPP_BASE_URL`. ContextLattice should not start or own llama.cpp in Lite.

Availability snapshot: checked against the Hugging Face model API on 2026-06-23. Re-check before shipping a preset because model cards, files, licenses, and gates can change.

## Small Models

| Model | Runtime | Role | Status |
|---|---|---|---|
| `usermma/Qwable-9B-Claude-Fable-5-mlx-8Bit` | MLX | Fast Apple Silicon coding/synthesis | Preferred small MLX candidate |
| `usermma/Qwable-9B-Claude-Fable-5-mlx-FP16` | MLX | Quality Apple Silicon coding/synthesis | Use when unified memory is sufficient |
| `yuxinlu1/gemma-4-12B-coder-fable5-composer2.5-v1-GGUF` | llama.cpp / LM Studio | Small coder/composer | GGUF opt-in |
| `Jackrong/Qwopus3.5-9B-Coder-MTP-GGUF` | llama.cpp / LM Studio | Small coder | GGUF opt-in |
| `usermma/LFM2.5-8B-A1B-Abliterated-Q3` | local quant | Experimental private eval | HF API returned auth-required/unverified; do not promote until verified |

Note: the reachable Qwable repos are under `usermma/...`; the `useremma/...` spelling returned auth/401 during verification.

## Medium Models

| Model | Runtime | Role | Status |
|---|---|---|---|
| `DJLougen/Qwable-5-27B-Coder-GGUF` | llama.cpp / LM Studio | Medium coder | GGUF opt-in |
| `Jackrong/Qwopus3.6-27B-v2-MTP-GGUF` | llama.cpp / LM Studio | Medium coding/synthesis | GGUF opt-in |
| `nightmedia/Qwen3.6-35B-A3B-Qwable-Holo3-Qwopus-qx64-hi-mlx` | MLX | Apple Silicon MoE synthesis | Higher-memory MLX candidate |
| `nex-agi/Nex-N2-mini` | SGLang / vLLM | Backend-native agentic synthesis | Prefer HF/safetensors serving |
| `Ex0bit/MYTHOS-26B-A4B-PRISM-PRO-DQ-MLX` | MLX | Creative synthesis | Benchmark before promotion |
| `XALIEN8881/MYTHOS-26B-A4B-PRISM-PRO-DQ-MLX` | MLX | Creative synthesis | Alternative MYTHOS MLX candidate |
| `Jiunsong/supergemma4-26b-uncensored-gguf-v2` | llama.cpp / LM Studio | Experimental private eval | Never default |
| `huihui-ai/Huihui-Qwable-3.6-27b-abliterated-GGUF` | llama.cpp / LM Studio | Experimental private eval | Never default |

## Watchlist

| Model | Runtime | Why keep it on the radar |
|---|---|---|
| `maidacundo/open-mythos-hf` | SGLang / vLLM | HF watchlist candidate for synthesis quality testing |
| `mradermacher/MYTHOS28BLORA-GGUF` | llama.cpp / LM Studio | GGUF MYTHOS lane worth benchmarking |
| `alphakek/gemma-4-E4B-it-heretic-mythos-v1` | SGLang / vLLM | Experimental HF watchlist candidate |
| `pat-jj/harness-1` | SGLang / vLLM | Potential retrieval/search harness model for tiny bounded planning loops |
| `openai/privacy-filter` | local classifier | Optional privacy classifier for write-boundary and harness-side filtering |

`pat-jj/harness-1` is interesting, but it should not become a default model just because the idea is attractive. The promotion gate is whether it beats deterministic retrieval planning and review heuristics in a bounded benchmark.

`openai/privacy-filter` belongs in both places if it proves useful: harness-side for early warning before an agent writes, and ContextLattice-side as a final write-boundary classifier. It must augment, not replace, deterministic secret detectors and `SECRETS_STORAGE_MODE=redact|block`.

## Running MLX

Download to an external disk or another explicit model cache, then run the server with the ContextLattice-safe final-content template:

```bash
huggingface-cli download usermma/Qwable-9B-Claude-Fable-5-mlx-8Bit \
  --local-dir /path/to/models/Qwable-9B-Claude-Fable-5-mlx-8Bit

scripts/inference_mlx_server.sh \
  --model /path/to/models/Qwable-9B-Claude-Fable-5-mlx-8Bit \
  --template-profile qwen-final-content

export ORCH_INFER_PROVIDER=mlx
export MLX_API_BASE=http://127.0.0.1:18087/v1
export TASK_MODEL=/path/to/models/Qwable-9B-Claude-Fable-5-mlx-8Bit
scripts/inference_template_conformance.sh --provider mlx --model "$TASK_MODEL"
```

## Running GGUF

Choose the quant file that fits memory, then serve it through an external llama.cpp or LM Studio process:

```bash
llama-server \
  -hf Jackrong/Qwopus3.5-9B-Coder-MTP-GGUF:<file.gguf> \
  --port 8080

export ORCH_INFER_PROVIDER=llama-cpp
export LLAMA_CPP_BASE_URL=http://127.0.0.1:8080
export TASK_MODEL=<served-model-name>
scripts/inference_template_conformance.sh --provider llama-cpp --model "$TASK_MODEL"
```

## Frontier Providers

Codex, ChatGPT, Claude, Claude Code, and similar managed agents should usually keep using their native frontier provider while ContextLattice acts as the sidecar memory/context service.

For Dream Mode or other ContextLattice-side synthesis with a frontier provider, expose an OpenAI-compatible endpoint directly or through a proxy such as LiteLLM/OpenRouter:

```bash
export ORCH_INFER_PROVIDER=openai-compatible
export OPENAI_API_BASE=https://<provider-or-proxy>/v1
export TASK_API_KEY=<provider-key>
export TASK_MODEL=<frontier-model>
scripts/inference_template_conformance.sh --provider openai-compatible --model "$TASK_MODEL" --base-url "$OPENAI_API_BASE"
```

If someone can serve a large model locally, they can follow the same medium-model contract: MLX on Apple Silicon, SGLang/vLLM on accelerators, or external llama.cpp/LM Studio connectors for GGUF.
