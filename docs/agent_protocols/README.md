# Agent Protocols

This directory contains **global** protocols that ALL agents should follow, regardless of which project they're working on.

## Structure

### Global vs Project-Specific
- **Global protocols** (`_global/agent_protocols/` in ContextLattice): Cross-project behavior, conventions, learnings
- **Project decisions** (`{projectName}/decisions/` in ContextLattice): Specific to individual codebases

### Storage Pattern in ContextLattice

```
ContextLattice Storage:
├── _global/
│   ├── agent_protocols/        # How agents should behave everywhere
│   │   ├── logging_protocol.md
│   │   ├── testing_standards.md
│   │   └── commit_guidelines.md
│   ├── shared_learnings/        # Cross-project patterns that work
│   │   ├── docker_compose_best_practices.md
│   │   └── mcp_integration_patterns.md
│   └── infrastructure/          # Shared infra decisions
│       └── deployment_conventions.md
│
└── {projectName}/               # e.g., "algotraderv2_rust", "mem_mcp_lobehub"
    ├── decisions/               # Project-specific technical decisions
    │   └── YYYYMMDD_*.txt
    ├── briefings/               # Handoff summaries for project
    │   └── YYYYMMDD_*.txt
    └── conventions/             # Project-specific patterns
        └── code_style.md
```

## Why This Matters

1. **Consistency**: Agents across all projects follow the same protocols
2. **Knowledge Sharing**: Learnings from one project benefit others
3. **Onboarding**: New agents can read global protocols once, not per-project
4. **Evolution**: Update protocol once, all agents see it

## Current Protocols

### [logging_protocol.md](./logging_protocol.md)
Core protocol for decision logging via ContextLattice. Defines:
- Two entrypoints (orchestrator REST vs raw MCP)
- Logging format and file naming conventions
- Retrieval patterns
- Health check commands
- Expectations for all agents

## Usage

When working in ANY project, agents should:
1. Read global protocols from `_global/agent_protocols/`
2. Read project-specific decisions from `{projectName}/decisions/`
3. Log project work to `{projectName}/decisions/`
4. Log cross-project learnings to `_global/shared_learnings/`
5. Update protocols in `_global/agent_protocols/` when conventions change

## Logging Global Protocols to ContextLattice

To ensure these protocols are accessible via ContextLattice:

```bash
export MEMMCP_ORCHESTRATOR_URL=http://127.0.0.1:8075

# Log the protocol
curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/write" \
  -H 'content-type: application/json' \
  -d '{
    "projectName": "_global",
    "fileName": "agent_protocols/logging_protocol.md",
    "content": "<contents of logging_protocol.md>"
  }'
```

This way, agents can retrieve it via:
```bash
curl -fsS "$MEMMCP_ORCHESTRATOR_URL/memory/files/_global/agent_protocols/logging_protocol.md"
```
