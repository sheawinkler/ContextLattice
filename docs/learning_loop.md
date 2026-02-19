# Learning Loop (Feedback + Preferences)

The learning loop lets memMCP incorporate user feedback and agent findings into retrieval context by default. It does **not** retrain models; instead it stores feedback and surfaces it during search so agents can adapt behavior.

## Endpoints

### Submit feedback
```
POST /feedback
{
  "project": "mem_mcp_lobehub",
  "user_id": "sheaw",
  "source": "user",
  "task_id": "<optional>",
  "rating": 5,
  "sentiment": "positive",
  "tags": ["tone", "style"],
  "content": "Prefer concise answers with next steps",
  "topic_path": "docs/agents"
}
```

### Fetch preferences
```
GET /preferences?project=mem_mcp_lobehub&user_id=sheaw
```

### Search with preference injection
```
POST /memory/search
{
  "query": "launch checklist",
  "project": "mem_mcp_lobehub",
  "user_id": "sheaw",
  "include_preferences": true
}
```

Response includes `preferences` when enabled.

## Defaults

- `LEARNING_LOOP_ENABLED=true` (default on)
- `PREFERENCE_MAX_ENTRIES=25`
- `FEEDBACK_MAX_CONTENT=2000`

## Notes
- Feedback is stored in the orchestrator SQLite DB and also written into memMCP memory under `feedback/<id>.md`.
- Task workers automatically log agent findings to `/feedback` with `source=agent`.
- If you want to disable preference injection for a call, set `include_preferences=false`.
