# Public Overview Site Source

This folder is the source for the public ContextLattice overview web pages.

## Files
- `index.html` - public landing page
- `architecture.html` - detailed runtime architecture
- `updates.html` - chronological updates page
- `installation.html` - install and launch command guide
- `integration.html` - client integration playbook
- `troubleshooting.html` - install/runtime recovery guide
- `contact.html` - contact page
- `styles.css` - shared styles
- `styles-gray.css` - grayscale/brutalist theme
- `assets/` - listing/social graphics (`contextlattice-og-1200x630.png`, `contextlattice-icon-512.png`)
- `.well-known/glama.json` - Glama server-claim metadata
- `.nojekyll` - enables serving dot-directories such as `.well-known` on GitHub Pages

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
- `updates.html`
- `installation.html`
- `integration.html`
- `troubleshooting.html`
- `contact.html`
- `styles.css`
- `styles-gray.css`
- `assets/`
- `.well-known/glama.json`
- `.nojekyll`

to the dedicated public overview mirror repository.
