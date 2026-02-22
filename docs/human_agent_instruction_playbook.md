# Human Agent Instruction Playbook

Use this guide when you want a human operator to reliably direct any agent runtime (ChatGPT app, Claude chat apps, Claude Code, Codex, OpenClaw/ZeroClaw/IronClaw) to leverage Context Lattice correctly.

## 1) Paste-once startup instruction

Use this as the first message in a new session:

```text
You must use Context Lattice as the memory/context layer.

Runtime:
- Orchestrator: http://127.0.0.1:8075
- API key: MEMMCP_ORCHESTRATOR_API_KEY from my local .env

Required behavior:
1) Before planning, call POST /memory/search with compact query + project/topic filters.
2) During long tasks, checkpoint major decisions/outcomes via POST /memory/write.
3) Before final answer, run one more POST /memory/search for recency.
4) Keep writes compact (summary, decisions, diffs). Do not dump full transcripts.
5) If memory endpoints fail, continue task and report degraded-memory mode explicitly.
```

## 2) Task kickoff template

```text
Project: <project_name>
Task: <goal>
Scope: <files/components>
Retrieve first:
- query: "<goal + key nouns>"
- project filter: <project_name>
- topic filter: <topic_path>
Then propose a plan using retrieved context.
```

## 3) Checkpoint template (during task)

```text
Write a memory checkpoint now:
- projectName: <project_name>
- fileName: checkpoints/<date_or_milestone>.md
- content: 5-10 bullet summary of decisions, assumptions, tradeoffs, and current status
```

## 4) Completion template

```text
Before final response:
1) retrieve once more with a recency-biased query
2) return final answer
3) write final memory entry including:
   - what changed
   - why
   - known risks
   - next recommended step
4) ask for 1-5 context quality feedback and write it as a short feedback memory
```

## 5) Team-sharing pattern

- Resolve an agentic issue once.
- Capture decision + rationale as memory.
- Reuse that context across teammates and future agent sessions.
- Keep naming conventions stable (`projectName`, topic paths, checkpoint files) so retrieval stays precise.

## 6) Anti-patterns to avoid

- Writing full chat logs as memory.
- Omitting project/topic filters on retrieval.
- Large verbose writes for tiny state changes.
- Skipping recency retrieval before final response.
