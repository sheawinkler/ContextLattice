# kalliste-alpha — Pricing (v0.1 draft)

**Model:** Hosted cloud subscriptions (usage + seats) with a free dev tier, plus enterprise.  
Anchors: vector DBs gate higher SLAs/features on paid tiers. Pinecone Standard starts at $50/mo (Enterprise $500/mo). Weaviate Cloud entry tiers start at $45/mo (Plus $280/mo). Qdrant Cloud advertises a free dev cluster (≈1GB RAM / 4GB disk) as an on-ramp. Observability anchor: Langfuse Cloud (Hobby free 50k units, Core $29/mo, Pro $199/mo). [Sources: Pinecone, Weaviate, Qdrant, Langfuse pricing]  

## Tiers (initial)

- **Free (dev)**  
  Projects: 1 • Storage: **1 GB** • Calls: **200k/mo** • Community support

- **Pro ($19/user/mo)**  
  Projects: 5 • Storage: **10 GB** • Calls: **5M/mo** • OAuth SSO-lite • Activity logs

- **Team ($149/org/mo + $15/user/mo)**  
  Projects: 20 • Storage: **50 GB** • Calls: **20M/mo** • Org SSO (OIDC/SAML) • RBAC • Backups • Audit

- **Enterprise (annual, custom)**  
  SSO/SAML • VPC/on-prem • HA/SLA (99.9–99.95%) • Full audit/retention • Private networking • Priority support

## Overage (starting points)
- **JSON-RPC calls:** $0.30 per million  
- **Storage:** $0.30 per GB-month

> These are intentionally below vector DB read pricing to fit a “memory service” cost profile; tune against your true COGS and customer usage.

## Notes
- “Cloud storage will cost money” — usage is metered for storage + JSON-RPC requests.  
- Repo stays open for local use (see LICENSE).  
- Enterprise mirrors what buyers expect from adjacent infra (RBAC, observability, HA, VPC/on-prem).  
- Public landing/overview messaging should keep the same hierarchy: local-first free start, paid pilot, then enterprise.

References: Pinecone pricing, Weaviate pricing, Qdrant Cloud free tier, Langfuse pricing.  
