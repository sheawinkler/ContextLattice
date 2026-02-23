# Task Agents (Optional)

ContextLattice includes a lightweight task queue so you can orchestrate agents locally without standing up a heavy control plane. Tasks are stored in a small SQLite DB and claimed via HTTP.

## Quickstart (default Trae-compatible runner)
```bash
# Start the ContextLattice stack first.
gmake mem

# Run the task worker (defaults to Trae-style + local model)
scripts/launch_task_agent.sh
```

Defaults:
- `TASK_AGENT=trae`
- `TASK_MODEL_PROVIDER=ollama`
- `TASK_MODEL=llama3.2:1b`

If you want a different model/provider, pass flags:
```bash
scripts/launch_task_agent.sh --model-provider lmstudio --model "qwen2.5-coder-7b"
```

## API endpoints
- `POST /agents/tasks` – create a task
- `GET /agents/tasks` – list tasks
- `POST /agents/tasks/next` – claim the next queued task
- `GET /agents/tasks/{id}` – fetch task + events
- `POST /agents/tasks/{id}/status` – update status (running/succeeded/failed/etc.)
- `POST /agents/tasks/{id}/approve` – approve a high-risk task

### Example: create a task
```bash
curl -fsS http://127.0.0.1:8075/agents/tasks \\
  -H "content-type: application/json" \\
  -d '{"title":"Draft a rollout plan","project":"_global","priority":2,"payload":{"scope":"pilot"}}'
```

### High-risk actions and approvals
Tasks can include risk metadata. If a task is high-risk and approval is required, workers will not claim it until approved.

```bash
curl -fsS http://127.0.0.1:8075/agents/tasks \\
  -H "content-type: application/json" \\
  -d '{\"title\":\"Deploy to prod\",\"project\":\"_global\",\"risk_level\":\"high\",\"action_type\":\"prod_deploy\"}'

curl -fsS http://127.0.0.1:8075/agents/tasks/<task_id>/approve \\
  -H \"content-type: application/json\" \\
  -d '{\"approver\":\"sheaw\",\"note\":\"ok\"}'
```

## Supported agents (compatibility)
The task worker supports the following agents by name:
`Letta`, `Trae`, `AutoGen`, `CrewAI`, `LangGraph`, `OpenHands`.

You can wire any of them in by setting a runner command:
- `TASK_AGENT_CMD` (generic)
- or `TRAE_CMD`, `LETTA_CMD`, `AUTOGEN_CMD`, `CREWAI_CMD`, `LANGGRAPH_CMD`, `OPENHANDS_CMD`

When a command is set, the worker executes it with the task context in env vars:
`TASK_ID`, `TASK_TITLE`, `TASK_PROJECT`, `TASK_PAYLOAD`, `TASK_AGENT`, `TASK_MODEL_PROVIDER`, `TASK_MODEL`, `TASK_BASE_URL`, `TASK_API_KEY`, `MEMMCP_ORCHESTRATOR_URL`.

### Example: use AutoGen as the runner
```bash
export AUTOGEN_CMD="python ./scripts/agent_runners/autogen_runner.py"
scripts/launch_task_agent.sh --task-agent autogen
```

### Built-in compatibility shims (lightweight)
If you want a quick, dependency-free runner, we ship tiny shims that call the same
OpenAI-compatible/Llama/Ollama flow and write results back into ContextLattice:

- `scripts/agent_runners/trae_runner.py`
- `scripts/agent_runners/letta_runner.py`
- `scripts/agent_runners/autogen_runner.py`
- `scripts/agent_runners/crewai_runner.py`
- `scripts/agent_runners/langgraph_runner.py`
- `scripts/agent_runners/openhands_runner.py`

Use them by setting `*_CMD`:
```bash
export LANGGRAPH_CMD="python scripts/agent_runners/langgraph_runner.py"
scripts/launch_task_agent.sh --task-agent langgraph
```

## Local model sources (wiring guide)

### Ollama (local)
```bash
ollama pull llama3.2:1b
export TASK_MODEL_PROVIDER=ollama
export TASK_MODEL=llama3.2:1b
# optional:
export TASK_BASE_URL=http://127.0.0.1:11434
```

### LM Studio (local, OpenAI-compatible)
```bash
# LM Studio default server: http://127.0.0.1:1234
export TASK_MODEL_PROVIDER=lmstudio
export TASK_MODEL="qwen2.5-coder-7b"
export TASK_BASE_URL=http://127.0.0.1:1234
```

### vLLM / OpenAI-compatible servers
```bash
export TASK_MODEL_PROVIDER=openai-compatible
export TASK_MODEL="llama-3.1-8b-instruct"
export TASK_BASE_URL=http://127.0.0.1:8000
```

### LocalAI / text-generation-webui (OpenAI-compatible)
```bash
export TASK_MODEL_PROVIDER=openai-compatible
export TASK_MODEL="your-local-model"
export TASK_BASE_URL=http://127.0.0.1:8080
```

### llama.cpp server
```bash
export TASK_MODEL_PROVIDER=llama-cpp
export TASK_MODEL="llama-3.1-8b-instruct"
export TASK_BASE_URL=http://127.0.0.1:8080
```

### OpenAI (hosted)
```bash
export TASK_MODEL_PROVIDER=openai
export TASK_MODEL="gpt-4o-mini"
export TASK_API_KEY="..."
export TASK_BASE_URL=https://api.openai.com
```

## Do we already support LangGraph?
Yes—via `--task-agent langgraph` and `LANGGRAPH_CMD`. If you want a starter shim,
use `scripts/agent_runners/langgraph_runner.py` and swap in your LangGraph flow
once you pick the exact graph.
