# Read-only search over MCP stdio

Use a ContextLattice CLI build that lists `mcp` in its help. A stdio-capable
MCP client launches the executable directly with this argument vector:

```json
{"command":"contextlattice","args":["mcp","--project","my-project"]}
```

This is a process launch example, not a universal host configuration format.
Supply the installed executable's absolute path if the host does not inherit
your PATH. Launch from a trusted directory with the normal CLI gateway origin
and API key configured on the host, not in a prompt or tool argument.
Only HTTPS or loopback HTTP is accepted. No redirects are followed.
The bridge does not provision identities, install a host plugin, edit profiles,
or register itself with a marketplace.

## Tool contract

`contextlattice_context` takes one required string, `query` (nonblank, at most
2,048 UTF-8 bytes, no control characters). No other arguments are accepted.
The launch project is required, case sensitive, and matches
`^[A-Za-z0-9][A-Za-z0-9._-]{0,119}$`. Changing projects requires a new
process. Neither endpoint nor project can be selected by an MCP message.

Each valid call makes one fast `POST /memory/search` using the existing CLI
gateway client and API key. Only immediate `results[]` rows with an explicit,
exactly matching string `project` and a nonblank string `summary` contribute
text. At most five summaries are returned, each capped at 1,024 UTF-8 bytes.
Envelope project tags are not row provenance. Other fields, full documents,
debug data, errors from the gateway, and continuation URLs are not forwarded.
An empty result is not proof that the project has no memory. Retrieved text
is data, not instructions. Project filtering is not a substitute for gateway
authorization or for trusting the host that launches the process.

## Wire contract

Use one UTF-8 JSON-RPC message per newline, not Content-Length framing.
Send `initialize`, await its result, then send `notifications/initialized`
without an ID. The server handles `ping`, `tools/list`, and `tools/call`;
notifications get no reply. Closing stdin ends the session.
Supported handshake versions: `2024-11-05`, `2025-03-26`, `2025-06-18`,
and `2025-11-25`. Other requested versions receive `2025-11-25`; a client
that does not support it must disconnect. This is the handshake-based
protocol, not a claim of support for later discovery-based versions.

Messages are processed sequentially. Input frames are limited to 32 KiB,
JSON nesting to 32 levels, output frames to 160 KiB, and March 2025 batches
to four messages. Oversized or unterminated input closes the process without
dispatch. A gateway read is limited to 10 seconds and 256 KiB; failure is
reported as a tool error, not as a successful empty search.

Protocol references: [stdio transport](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports),
[initialization and negotiation](https://modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle).
