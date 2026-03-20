ContextLattice Windows MSI Bootstrap
====================================

This MSI installs bootstrap scripts for local ContextLattice operation on Windows.

Installed files:
- ContextLattice-Install.cmd
- ContextLattice-Monitor.cmd
- Install-ContextLattice.ps1
- Monitor-ContextLattice.ps1

Default install path:
- C:\Program Files\ContextLattice

How to use:
1) Open "ContextLattice-Install.cmd" as Administrator.
2) Wait for Docker compose stack launch.
3) Open "ContextLattice-Monitor.cmd" for health/status/telemetry checks.

Requirements:
- Docker Desktop (running)
- Git for Windows
- Internet access for repository clone/pull

Repository:
https://github.com/sheawinkler/ContextLattice
