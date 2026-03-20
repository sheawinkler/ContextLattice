@echo off
setlocal
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0Monitor-ContextLattice.ps1" %*
endlocal
