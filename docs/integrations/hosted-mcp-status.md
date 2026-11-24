# MCP publication status

**Local stdio verified; hosted MCP pending.** The two JSON source records are
retained for release tooling. Neither is a submission-ready listing. An empty
`remotes` array is intentional. For the local command and tool contract, see
[stdio MCP](stdio-mcp.md).

`python3 scripts/launch_lock.py --mode local` does not probe a public host.
Public mode fails closed. A future maintained endpoint must be configured
explicitly in the server record and separately approved in the lock's exact
HTTPS URL allowlist. Never add operator credentials or key headers. The
anonymous probe requires a direct JSON response to MCP `initialize`
(protocol `2025-11-25`, server name `contextlattice`, version `1`, tools
capability). Redirects, login pages, and HTTP 401/403 do not qualify.

That exchange verifies only endpoint identity and advertised capability.
Before promotion, independently verify public-site ownership, intended client
authentication and a current registry record matching the maintained endpoint.
The lock neither performs those checks nor substitutes a local GO for them.
