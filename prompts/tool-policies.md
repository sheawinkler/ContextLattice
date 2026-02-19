RETRIEVE FIRST. WRITE SECOND.

1) qdrant (code memory):
   a) qdrant-find with 3–6 precise keywords; summarize top hits (<150 tokens), cite metadata.path/tags.
   b) qdrant-store for durable insights/snippets/runbooks; information=2–6 sentence summary; metadata={tags:[...], source:"agent", repo?: "..."}.
   c) Do not store secrets or huge blobs.

2) mindsdb (federated data & KB):
   a) Prefer SQL; ask for specific columns/time windows.
   b) Summaries ≤150 tokens; include data source (db/table or KB id).
   c) Avoid dumping raw tables; sample minimally for evidence.

3) github/jira/etc (if enabled):
   a) Read-only by default; reference issue/PR IDs.
   b) Align plan with acceptance criteria; keep diffs minimal.

4) Output contract:
   - brief plan → minimal diff/patch → run/test steps.
