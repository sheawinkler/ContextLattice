# Launch Service (Modular)

This module makes launch planning reusable across products.

Use one config file per application, then generate:
- `docs/publish_execution_tracker.md`
- `docs/launch_channel_copybook.md`

## Why this exists

- Keep launch operations modular and repeatable.
- Reuse the same MCP-first distribution framework for future apps.
- Maintain one source of truth for channels, timestamps, and copy blocks.

## Inputs

- Config: `launch_service/config/contextlattice.launch.json`
  - product metadata
  - channel matrix
  - MT/PT schedules
  - run-of-show
  - copy blocks

## Generate docs

```bash
python3 launch_service/generate_launch_docs.py \
  --config launch_service/config/contextlattice.launch.json \
  --tracker-out docs/publish_execution_tracker.md \
  --copybook-out docs/launch_channel_copybook.md
```

## Reuse for another app

1. Copy `launch_service/config/contextlattice.launch.json`.
2. Rename and update product metadata, links, channels, schedules, and copy.
3. Run `generate_launch_docs.py` with the new config.
4. Commit generated docs for that app.
