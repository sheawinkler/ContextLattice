# Public Overview Site Source

This folder is the source for the public `memmcp-overview` web pages.

## Files
- `index.html` - public landing page
- `architecture.html` - detailed runtime architecture
- `updates.html` - chronological updates page
- `installation.html` - install and launch command guide
- `integration.html` - client integration playbook
- `troubleshooting.html` - install/runtime recovery guide
- `contact.html` - contact page
- `styles.css` - shared styles

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

to the public repo `sheawinkler/memmcp-overview`.
