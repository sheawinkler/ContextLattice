# Public Overview Site Source

This folder is the source for the public ContextLattice overview web pages.

## Files
- `index.html` - public landing page
- `architecture.html` - detailed runtime architecture
- `wiki.html` - operator wiki with endpoint atlas and playbooks
- `updates.html` - chronological updates page
- `roadmap.html` - V3 integration and efficacy roadmap
- `installation.html` - install and launch command guide
- `integration.html` - client integration playbook
- `premium.html` - paid tiers and free-vs-paid capability matrix
- `app.html` - `app.contextlattice.io` public app-surface overview
- `troubleshooting.html` - install/runtime recovery guide
- `contact.html` - contact page
- `styles.css` - shared styles
- `styles-gray.css` - grayscale/brutalist theme
- `assets/` - listing/social graphics (`contextlattice-og-1200x630.png`, `contextlattice-icon-512.png`)
- `templates/` - copy-ready agent instruction templates (`AGENTS.contextlattice.md`, `SKILLS.contextlattice.md`)
- `templates/agents/` - agent-profile templates (`codex`, `claude-code`, `opencode`, `hermes-agent`, `chatgpt`, `claude`)
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
- `wiki.html`
- `updates.html`
- `roadmap.html`
- `installation.html`
- `integration.html`
- `premium.html`
- `app.html`
- `troubleshooting.html`
- `contact.html`
- `styles.css`
- `styles-gray.css`
- `assets/`
- `.well-known/glama.json`
- `.nojekyll`

to the dedicated public overview publishing branch.

Default target: `sheawinkler/ContextLattice` on `gh-pages` (override with `PUBLIC_REPO` / `PUBLIC_BRANCH` if needed).
