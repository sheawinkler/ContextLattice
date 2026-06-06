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

New-Item -ItemType Directory -Path $ScriptsDir -Force | Out-Null
New-Item -ItemType Directory -Path $AgentScriptsDir -Force | Out-Null
New-Item -ItemType Directory -Path $BinDir -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $GlobalHome "config\agent_contracts") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $GlobalHome "config\agents") -Force | Out-Null
New-Item -ItemType Directory -Path (Join-Path $GlobalHome "config\model_compat") -Force | Out-Null

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

Set-Content -Path (Join-Path $BinDir "contextlattice_search.cmd") -Value $searchCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_write.cmd") -Value $writeCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_orchestration.cmd") -Value $orchCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_adapter.cmd") -Value $adapterCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_agent_runtime_proof.cmd") -Value $proofCmd -Encoding Ascii

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
Write-Host "  contextlattice_write -h"
Write-Host "  contextlattice_agent_adapter profiles"
Write-Host "  contextlattice_agent_runtime_proof --pretty"
