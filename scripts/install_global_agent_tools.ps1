Param(
    [string]$GlobalHome = "$env:USERPROFILE\.contextlattice",
    [switch]$SkipVenv
)

$ErrorActionPreference = "Stop"

$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$ScriptsDir = Join-Path $GlobalHome "scripts"
$AgentScriptsDir = Join-Path $ScriptsDir "agent"
$BinDir = Join-Path $GlobalHome "bin"
$VenvDir = Join-Path $GlobalHome "venv-agent-tools"
$VenvPython = Join-Path $VenvDir "Scripts\python.exe"
$HookEnvFile = Join-Path $GlobalHome "agent_hooks.env"

function ConvertTo-ShellDoubleQuoted {
    param([string]$Value)
    return '"' + ($Value -replace '"', '\"') + '"'
}

New-Item -ItemType Directory -Path $ScriptsDir -Force | Out-Null
New-Item -ItemType Directory -Path $AgentScriptsDir -Force | Out-Null
New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $GlobalHome "config\agent_contracts") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $GlobalHome "config\agents") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $GlobalHome "config\model_compat") -Force | Out-Null

$hookEnvLines = @(
    "export CONTEXTLATTICE_REPO_ROOT=$(ConvertTo-ShellDoubleQuoted $RepoRoot)",
    "export CONTEXTLATTICE_ORCHESTRATOR_URL=""http://127.0.0.1:8075""",
    "export MEMMCP_ORCHESTRATOR_URL=""http://127.0.0.1:8075""",
    "export CONTEXTLATTICE_AGENT_ID=""codex_gpt5""",
    "export MEMMCP_AGENT_ID=""codex_gpt5"""
)
Set-Content -Path $HookEnvFile -Value $hookEnvLines -Encoding Ascii

$sourceScripts = @(
    "agent_orchestration.py",
    "agent_contracts.py",
    "contextlattice_client.py",
    "contextlattice_search.py",
    "contextlattice_write.py"
)

foreach ($name in $sourceScripts) {
    $src = Join-Path $RepoRoot "scripts\$name"
    if (-not (Test-Path $src)) {
        throw "Missing required source script: $src"
    }
    Copy-Item -Path $src -Destination (Join-Path $ScriptsDir $name) -Force
}

Get-ChildItem -Path (Join-Path $RepoRoot "scripts\agent") -File | ForEach-Object {
    Copy-Item -Path $_.FullName -Destination (Join-Path $AgentScriptsDir $_.Name) -Force
}

Get-ChildItem -Path (Join-Path $RepoRoot "config\agent_contracts") -Filter "*.json" -File | ForEach-Object {
    Copy-Item -Path $_.FullName -Destination (Join-Path $GlobalHome "config\agent_contracts\$($_.Name)") -Force
}
Get-ChildItem -Path (Join-Path $RepoRoot "config\agents") -Filter "*.json" -File | ForEach-Object {
    Copy-Item -Path $_.FullName -Destination (Join-Path $GlobalHome "config\agents\$($_.Name)") -Force
}
Get-ChildItem -Path (Join-Path $RepoRoot "config\model_compat") -Filter "*.json" -File | ForEach-Object {
    Copy-Item -Path $_.FullName -Destination (Join-Path $GlobalHome "config\model_compat\$($_.Name)") -Force
}

if (-not $SkipVenv.IsPresent) {
    $pythonCmd = Get-Command python -ErrorAction SilentlyContinue
    if ($null -eq $pythonCmd) {
        $pythonCmd = Get-Command py -ErrorAction SilentlyContinue
    }
    if ($null -eq $pythonCmd) {
        throw "python/py is required to install global agent tools."
    }

    if (-not (Test-Path $VenvPython)) {
        if ($pythonCmd.Name -eq "py") {
            & py -3 -m venv $VenvDir
        } else {
            & python -m venv $VenvDir
        }
    }
    & $VenvPython -m pip install --disable-pip-version-check --quiet --upgrade pip
    & $VenvPython -m pip install --disable-pip-version-check --quiet "httpx>=0.27,<1.0"
}

$searchCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\contextlattice_search.py
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$writeCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\contextlattice_write.py
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$packCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\contextlattice-pack
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$orchCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent_orchestration.py
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$adapterCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\contextlattice-agent-adapter
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$adoptCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\contextlattice-adopt
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$doctorCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\contextlattice-adopt
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" doctor %*
"@

$proofCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\agent-runtime-proof-pack
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$adoptionProofCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\agent-adoption-proof-matrix
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$sessionCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\contextlattice-session
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$traceCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\agent-run-trace
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$runAdvisorCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\contextlattice-run-advisor
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$runtimeDoctorCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\audit-agent-runtime-install
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$contextBoundaryCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\audit-context-boundary
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$memoryTopologyCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\audit-memory-topology
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$sourceBackfillCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\source-backfill-memory
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$skillsIndexCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\contextlattice-skills-index
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

$codexSessionStoreDoctorCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\audit-codex-session-store
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%PYTHON_EXE%" "%SCRIPT_PATH%" %*
"@

Set-Content -Path (Join-Path $BinDir "contextlattice_search.cmd") -Value $searchCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_write.cmd") -Value $writeCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_pack.cmd") -Value $packCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_orchestration.cmd") -Value $orchCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_adopt.cmd") -Value $adoptCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_doctor.cmd") -Value $doctorCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_adapter.cmd") -Value $adapterCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_runtime_proof.cmd") -Value $proofCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_adoption_proof.cmd") -Value $adoptionProofCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_session.cmd") -Value $sessionCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_trace.cmd") -Value $traceCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_run_advisor.cmd") -Value $runAdvisorCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_runtime_doctor.cmd") -Value $runtimeDoctorCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_context_boundary.cmd") -Value $contextBoundaryCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_memory_topology.cmd") -Value $memoryTopologyCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_source_backfill.cmd") -Value $sourceBackfillCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_skills_index.cmd") -Value $skillsIndexCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_codex_session_store_doctor.cmd") -Value $codexSessionStoreDoctorCmd -Encoding Ascii

$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($null -eq $userPath) { $userPath = "" }
$normalizedBin = $BinDir.ToLowerInvariant()
$pathParts = $userPath.Split(';', [System.StringSplitOptions]::RemoveEmptyEntries)
$hasBin = $false
foreach ($part in $pathParts) {
    if ($part.Trim().ToLowerInvariant() -eq $normalizedBin) {
        $hasBin = $true
        break
    }
}
if (-not $hasBin) {
    $newPath = if ([string]::IsNullOrWhiteSpace($userPath)) { $BinDir } else { "$userPath;$BinDir" }
    [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
}

Write-Host "Installed global ContextLattice tools to $GlobalHome"
Write-Host "Open a new terminal and verify:"
Write-Host "  contextlattice_search -h"
Write-Host "  contextlattice_pack `"what should this agent know before solving the task?`" --project contextlattice --pretty"
Write-Host "  contextlattice_write -h"
Write-Host "  contextlattice_adopt status --pretty"
Write-Host "  contextlattice_doctor --agents codex --skip-provider-smoke --pretty"
Write-Host "  contextlattice_adopt proof --agents codex --skip-provider-smoke --pretty"
Write-Host "  contextlattice_agent_adapter profiles"
Write-Host "  contextlattice_agent_session runtime --pretty"
Write-Host "  contextlattice_agent_runtime_proof --pretty"
Write-Host "  contextlattice_agent_adoption_proof --skip-provider-smoke --progress --pretty"
Write-Host "  contextlattice_context_boundary --pretty"
Write-Host "  contextlattice_agent_trace --session-id <session-id> --tree"
Write-Host "  contextlattice_run_advisor --session-id <session-id> --pretty"
Write-Host "  contextlattice_memory_topology --pretty"
Write-Host "  contextlattice_agent_runtime_doctor --pretty"
Write-Host "  contextlattice_source_backfill --source jsonl --path data.jsonl --project my-project --pretty"
Write-Host "  contextlattice_skills_index search agent --pretty"
Write-Host "  contextlattice_codex_session_store_doctor --pretty"
