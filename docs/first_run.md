# First run checklist

Use this when bringing up ContextLattice locally for the first time.

## 1) Run first-run setup + smoke (recommended)

```bash
BOOTSTRAP=1 scripts/first_run.sh
```

`scripts/first_run.sh` applies secure defaults by default:
- `MEMMCP_ENV=production`, `ORCH_SECURITY_STRICT=true`
- `ORCH_PUBLIC_STATUS=false`, `ORCH_PUBLIC_DOCS=false`
- `MESSAGING_WEBHOOK_PUBLIC=false`
- `HOST_BIND_ADDRESS=127.0.0.1`
- Generates `MEMMCP_ORCHESTRATOR_API_KEY` when missing
- `SECRETS_STORAGE_MODE=redact`
- `MINDSDB_REQUIRED=auto` (smoke requires MindsDB only when `COMPOSE_PROFILES` includes `analytics`)

## 2) Launch modes (after secure bootstrap)

`gmake mem-up` launches whatever `COMPOSE_PROFILES` is set to in `.env` (`core` by default).

```bash
gmake mem-up
gmake mem-up-lite
gmake mem-up-full
```

Letta mode:
- default: local Letta (`llm` profile enabled, no API key required)
- external: pass `--letta-api-key <key>` (and optionally `--letta-url <url>`) to disable local `llm` profile and use hosted Letta

Examples:

```bash
# local Letta + secure defaults (default behavior)
scripts/first_run.sh

# hosted Letta key/url + secure defaults
scripts/first_run.sh --letta-api-key "$LETTA_API_KEY" --letta-url "https://api.letta.com"

# allow secrets storage (no redaction)
scripts/first_run.sh --allow-secrets-storage

# block any write that looks like it contains secrets
scripts/first_run.sh --block-secrets-storage

# opt out of secure defaults for local-only experimentation
scripts/first_run.sh --insecure-local
```

Optional stricter MindsDB timeout:

```bash
BOOTSTRAP=1 MINDSDB_READY_TIMEOUT=180 scripts/first_run.sh
```

## 3) Validate health

```bash
ORCH_KEY="$(awk -F= '/^MEMMCP_ORCHESTRATOR_API_KEY=/{print substr($0,index($0,"=")+1)}' .env)"
curl -fsS http://127.0.0.1:8075/health | jq
curl -fsS -H "x-api-key: ${ORCH_KEY}" http://127.0.0.1:8075/status | jq
```

Then open the dashboard `/setup` page for interactive checks.

## 4) Terminal status view (optional)

```bash
python3 scripts/terminal_dashboard.py
```
