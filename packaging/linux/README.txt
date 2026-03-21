ContextLattice Linux Bootstrap Bundle
=====================================

This Linux bundle installs bootstrap scripts for local ContextLattice operation.

Included files:
- ContextLattice-Install.sh
- ContextLattice-Monitor.sh

Default install path:
- $HOME/ContextLattice

How to use:
1) Run ./ContextLattice-Install.sh
2) Wait for Docker compose stack launch
3) Run ./ContextLattice-Monitor.sh for health/status/telemetry checks

Requirements:
- Docker Engine/Desktop with Compose v2
- git
- curl
- jq
- internet access for repository clone/pull

Repository:
https://github.com/sheawinkler/ContextLattice
