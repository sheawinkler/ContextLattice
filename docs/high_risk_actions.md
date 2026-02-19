# High-Risk Actions and Approval Gating

High-risk actions are tasks that could cause irreversible changes, external commitments, or financial impact. These require explicit approval before a worker will execute them.

## High-Risk Action Categories

A task is considered high risk if its payload indicates any of the following:
- Money movement (payments, transfers, purchases)
- Deleting or overwriting data
- Infrastructure or production changes
- External communications (emails, public posts, customer messages)
- Credential changes or security-sensitive actions
- Legal or contractual commitments

## How It Works

- When a task is created, the orchestrator evaluates `risk_level` or `action_type`.
- If `HIGH_RISK_APPROVAL_REQUIRED=true` and risk is high, the task is queued but **not claimable** until approved.
- Approval is recorded and then the task can be claimed by workers.

## Task Fields

You can mark risk at creation:

```
POST /agents/tasks
{
  "title": "Deploy to production",
  "project": "mem_mcp_lobehub",
  "risk_level": "high",
  "action_type": "prod_deploy",
  "payload": {
    "steps": ["run migrations", "deploy"]
  }
}
```

## Approving a Task

```
POST /agents/tasks/<task_id>/approve
{
  "approver": "sheaw",
  "note": "OK to deploy"
}
```

## Env Controls

- `HIGH_RISK_APPROVAL_REQUIRED=true`
- `HIGH_RISK_ACTIONS=payment,transfer_funds,delete_data,infra_change,prod_deploy,send_external_message,credential_change,purchase`

Set `HIGH_RISK_APPROVAL_REQUIRED=false` to disable gating.
