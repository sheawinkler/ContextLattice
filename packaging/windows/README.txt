ContextLattice Windows MSI Release Bundle
=========================================

The MSI embeds a lane-bound, checksummed ContextLattice ZIP payload from the
release tag. Installation and extraction do not clone or pull a repository.

Installed files:
- ContextLattice-Install.cmd
- ContextLattice-Monitor.cmd
- Install-ContextLattice.ps1
- Monitor-ContextLattice.ps1
- payload\contextlattice-payload.zip
- payload\contextlattice-payload.zip.sha256
- payload\contextlattice-release.json

Examples:
- Install/update and launch: ContextLattice-Install.cmd
- Offline extraction test: ContextLattice-Install.cmd -ExtractOnly -InstallDir C:\Temp\ContextLattice
- Install/update without launch: ContextLattice-Install.cmd -NoLaunch

Atomic updates preserve only .env, .data, data, and backups inside the install
directory. Docker volumes live outside that replacement. A modified legacy Git
checkout or unmanaged non-empty directory is refused instead of overwritten.
The installer verifies release identity and checksums before atomically replacing
tracked application files.

Launch requirement:
- Docker Desktop with Compose v2

Extraction uses built-in PowerShell Expand-Archive and Get-FileHash.
