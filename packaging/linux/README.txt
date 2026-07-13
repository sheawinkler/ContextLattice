ContextLattice Linux Release Bundle
===================================

This bundle contains a lane-bound, checksummed ContextLattice payload from the
release tag. Installation and extraction do not clone or pull a repository.

Included:
- ContextLattice-Install.sh
- ContextLattice-Monitor.sh
- payload/contextlattice-payload.tar.gz
- payload/contextlattice-payload.tar.gz.sha256
- payload/contextlattice-release.json

Examples:
- Install/update and launch: ./ContextLattice-Install.sh
- Install/update full stack: ./ContextLattice-Install.sh --full
- Offline extraction test: ./ContextLattice-Install.sh --extract-only --install-dir /tmp/contextlattice
- Install/update without launch: ./ContextLattice-Install.sh --no-launch

Atomic updates preserve only .env, .data, data, and backups inside the install
directory. Docker volumes live outside that replacement. A modified legacy Git
checkout or unmanaged non-empty directory is refused instead of overwritten.
The installer verifies release identity and checksums before atomically replacing
tracked application files.

Launch requirements:
- Docker Engine/Desktop with Compose v2
- curl (recommended for local health checks)

Extraction requirements:
- tar
- sha256sum or shasum
