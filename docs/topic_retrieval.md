# Topic Retrieval Trees

memMCP builds a topic/subtopic tree from memory paths and uses it to filter retrieval.

## Topic Path Rules

- By default, the topic path is derived from the **directory** portion of `fileName`.
  - Example: `briefings/20260203_launch.md` -> topic path `briefings`
  - Example: `agents/protocols/approval.md` -> topic path `agents/protocols`
- You can override with `topicPath` on `/memory/write`.

## Indexing

- Each memory write generates topic tags:
  - `agents`, `agents/protocols`, `agents/protocols/approval`
- These tags are stored in Qdrant and in a local topic index file.

## Browse Topics

```
GET /memory/topics?project=mem_mcp_lobehub&depth=4
```

## Filter Search by Topic

```
POST /memory/search
{
  "query": "approval",
  "project": "mem_mcp_lobehub",
  "topic_path": "agents/protocols"
}
```

## Conventions

- Use lowercase and clear directory names for topic grouping.
- Prefer consistent depth: `domain/subdomain/artifact`.
