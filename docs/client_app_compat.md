# Client app compatibility

Updated: 2026-02-18

This doc tracks practical integration status for ChatGPT, Gemini, and Perplexity with Context Lattice.

## Baseline strategy

- **Local-first**: keep Context Lattice local (`gmake mem-up`) and expose MCP on `http://127.0.0.1:53130/mcp`.
- **Hosted/BYO**: expose a remote MCP endpoint over HTTPS + auth.
- For clients that cannot connect directly to your local MCP endpoint, deploy a thin remote proxy.

## Compatibility matrix

| Client | Native MCP status | Local MCP | Remote MCP | Recommended path |
| --- | --- | --- | --- | --- |
| ChatGPT (web + apps) | Supported via Connectors / Developer mode | No (remote only) | Yes | Host a remote MCP endpoint and register as a custom connector |
| Gemini API clients | MCP supported in SDK/API (function-calling + interactions) | Yes | Yes | Connect Gemini SDK/API directly to local MCP for dev, remote MCP for hosted |
| Gemini consumer app | No documented custom MCP connector flow | N/A | N/A | Use a small API bridge (Gemini API + MCP backend) |
| Perplexity Mac app | Local MCP available for paid rollout | Yes | Remote coming soon | Use local MCP on macOS now |
| Perplexity web | No general direct local MCP path documented | No | Emerging / account-dependent | Use remote MCP when available, otherwise bridge/proxy |

Inference note:
- Gemini consumer app and Perplexity web rows are based on published docs for API/Mac support and lack of a formal custom-MCP setup path for those app surfaces.

## Setup flows

### 1) ChatGPT (remote MCP)

1. Run Context Lattice locally or hosted.
2. Expose a secure remote MCP URL (HTTPS).
3. In ChatGPT Developer mode, add your MCP connector endpoint.
4. Validate with read-only tool calls first, then allow write paths.

### 2) Gemini API (local or hosted MCP)

1. Start Context Lattice (`gmake mem-up`).
2. Use Gemini SDK MCP integration against local MCP URL for development.
3. For hosted operation, point Gemini tools at your remote MCP URL.
4. Keep orchestrator retrieval/write contract stable (`/memory/search`, `/memory/write`).

### 3) Perplexity

1. For macOS app, configure local MCP servers from app settings.
2. For web or cross-device use, prefer remote MCP when account supports it.
3. If remote MCP is unavailable, route through a lightweight hosted bridge.

## Source links

- OpenAI Connectors + custom MCP: https://help.openai.com/en/articles/11487775/
- OpenAI Developer mode MCP details: https://help.openai.com/en/articles/12584461-developer-mode-apps-and-full-mcp-connectors-in-chatgpt-beta.svgz
- OpenAI MCP build docs: https://platform.openai.com/docs/mcp/
- Gemini MCP in function calling docs: https://ai.google.dev/gemini-api/docs/function-calling
- Gemini remote MCP in interactions API docs: https://ai.google.dev/gemini-api/docs/interactions
- Perplexity local/remote MCP support article: https://www.perplexity.ai/help-center/en/articles/11502712-local-and-remote-mcps-for-perplexity
