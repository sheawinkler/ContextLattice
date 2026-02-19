# First run checklist

Use this when bringing up ContextLattice locally for the first time.

## 1) Start the stack

```bash
gmake mem-up
```

## 2) Run the first-run smoke

```bash
scripts/first_run.sh
```

`scripts/first_run.sh` now configures Letta mode:
- default: local Letta (`llm` profile enabled, no API key required)
- external: pass `--letta-api-key <key>` (and optionally `--letta-url <url>`) to disable local `llm` profile and use hosted Letta

Examples:

```bash
# local Letta (default)
scripts/first_run.sh

# hosted Letta key/url
scripts/first_run.sh --letta-api-key "$LETTA_API_KEY" --letta-url "https://api.letta.com"
```

Optional: bootstrap + stricter MindsDB timeout.

```bash
BOOTSTRAP=1 MINDSDB_READY_TIMEOUT=180 scripts/first_run.sh
```

## 3) Validate in the dashboard

Open the console and visit `/status` and `/setup` to verify health and run a sample write.

## 4) Terminal status view (optional)

```bash
python3 scripts/terminal_dashboard.py
```
