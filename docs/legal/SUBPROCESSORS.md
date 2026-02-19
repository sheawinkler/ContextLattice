# Subprocessors

Updated: 2026-02-18

This list reflects subprocessors commonly used in hosted or optional
integrations. Local/self-host deployments can run without most hosted vendors.

## Core platform

| Provider | Purpose | Data categories |
| --- | --- | --- |
| Docker (host runtime tooling) | Container runtime/distribution | Infrastructure metadata |
| GitHub | Source hosting, CI/CD | Repository metadata, operational logs |

## Optional service integrations (BYO recommended)

| Provider | Purpose | Data categories |
| --- | --- | --- |
| Qdrant Cloud | Vector storage/retrieval | Embeddings, metadata, memory payload summaries |
| MindsDB | SQL/analytics sync | Memory summaries and metadata |
| Letta | Archival memory/RAG augmentation | Curated memory summaries/context |
| Langfuse | Observability/tracing | Request/latency/trace metadata |
| OpenAI / Anthropic / Google / Ollama-compatible endpoints | Model/embedding providers (if configured) | Prompt/response payloads sent by customer workflows |

## Notes

- Customer can disable integrations and keep all sinks local.
- Customer controls third-party account setup and credentials in BYO mode.
- This list will be updated when hosted defaults change.
