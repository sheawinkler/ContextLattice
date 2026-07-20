# Public Overview Site Source

This folder is the source for the public ContextLattice overview web pages.

## Files
- `index.html` - public landing page
- `architecture.html` - detailed runtime architecture
- `local-ai-workspaces.html` - comparison guide for local AI workspaces, agent harnesses, and ContextLattice's local-first intelligence-layer role
- `scaling-memory.html` - scaling story for rollups, vectors, durable writes, CLI workflows, Skills Index discovery, provenance, learning, and deeper recall lanes
- `wiki.html` - operator wiki with endpoint atlas and playbooks
- `updates.html` - chronological updates page
- `roadmap.html` - current product roadmap and release discipline
- `installation.html` - install and launch command guide
- `cli.html` - copy-ready CLI quickstart and validation commands
- `integration.html` - client integration playbook
- `premium.html` - paid tiers and free-vs-paid capability matrix
- `app.html` - high-level premium app surface for account/billing/download/key lifecycle
- `troubleshooting.html` - install/runtime recovery guide
- `contact.html` - contact page
- `llms.txt` - crawler/assistant guidance for safe public-site usage
- `commercial-truth.json` - generated public product, version, interface, and plan truth
- `robots.txt` - crawler policy
- `sitemap.xml` - canonical public-page index
- `styles.css` - shared styles
- `styles-gray.css` - grayscale/brutalist theme
- `styles-fracture.css` - fracture-ledger visual treatment
- `assets/` - listing/social graphics (`contextlattice-og-1200x630.png`, `contextlattice-icon-512.png`)
- `templates/` - copy-ready agent instruction templates (`AGENTS.contextlattice.md`, `SKILLS.contextlattice.md`)
- `templates/agents/` - agent-profile templates (`codex`, `claude-code`, `opencode`, `hermes-agent`, `omp`, `mercury-agent`, `pi`, `droid`, `chatgpt`, `claude`)
- `.well-known/glama.json` - Glama server-claim metadata
- `.nojekyll` - enables serving dot-directories such as `.well-known` on GitHub Pages

## Agent template quick start
Copy these into your own repo and adjust project/topic defaults:

```bash
cp docs/public_overview/templates/AGENTS.contextlattice.md ./AGENTS.md
cp docs/public_overview/templates/SKILLS.contextlattice.md ./SKILLS.md
# optional: agent-specific blocks
cp docs/public_overview/templates/agents/codex.md ./docs/agent_templates/codex.md
cp docs/public_overview/templates/agents/claude-code.md ./docs/agent_templates/claude-code.md
```

## Update workflow
1. Edit the required page(s).
2. Keep dates in `YYYY-MM-DD` format.
3. Run:

```bash
scripts/sync_public_overview.sh
```

This syncs:
- `index.html`
- `architecture.html`
- `local-ai-workspaces.html`
- `scaling-memory.html`
- `wiki.html`
- `updates.html`
- `roadmap.html`
- `installation.html`
- `cli.html`
- `integration.html`
- `premium.html`
- `app.html`
- `troubleshooting.html`
- `contact.html`
- `llms.txt`
- `commercial-truth.json`
- `robots.txt`
- `sitemap.xml`
- `styles.css`
- `styles-gray.css`
- `styles-fracture.css`
- `assets/`
- `.well-known/glama.json`
- `.nojekyll`

to the dedicated public overview publishing branch.

Default target: `sheawinkler/ContextLattice` on `gh-pages` (override with `PUBLIC_REPO` / `PUBLIC_BRANCH` if needed).
