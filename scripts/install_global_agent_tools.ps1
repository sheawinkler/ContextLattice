Param(
    [string]$GlobalHome = "$env:USERPROFILE\.contextlattice",
    [switch]$SkipVenv,
    [switch]$IncludeDevPythonTools,
    [switch]$NoAgentHooks
)

$ErrorActionPreference = "Stop"

$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$ScriptsDir = Join-Path $GlobalHome "scripts"
$AgentScriptsDir = Join-Path $ScriptsDir "agent"
$BinDir = Join-Path $GlobalHome "bin"
$VenvDir = Join-Path $GlobalHome "venv-agent-tools"
$VenvPython = Join-Path $VenvDir "Scripts\python.exe"
$HookEnvFile = Join-Path $GlobalHome "agent_hooks.env"
$HomeDir = if ($env:USERPROFILE) { $env:USERPROFILE } else { [Environment]::GetFolderPath("UserProfile") }

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

$managedHookEnvKeys = @(
    "CONTEXTLATTICE_REPO_ROOT",
    "CONTEXTLATTICE_ORCHESTRATOR_URL",
    "MEMMCP_ORCHESTRATOR_URL",
    "CONTEXTLATTICE_AGENT_ID",
    "MEMMCP_AGENT_ID"
)
$existingHookEnvLines = @()
if (Test-Path $HookEnvFile) {
    $existingHookEnvLines = Get-Content -Path $HookEnvFile | Where-Object {
        $line = [string]$_
        if ($line -match '^\s*#\s*Local-only ContextLattice hook policy\. Not part of any repo\.\s*$') {
            return $false
        }
        foreach ($key in $managedHookEnvKeys) {
            if ($line -match "^\s*(export\s+)?$([regex]::Escape($key))=") {
                return $false
            }
        }
        return $true
    }
}

$hookEnvLines = @(
    "# Local-only ContextLattice hook policy. Not part of any repo.",
    "export CONTEXTLATTICE_REPO_ROOT=$(ConvertTo-ShellDoubleQuoted $RepoRoot)",
    "export CONTEXTLATTICE_ORCHESTRATOR_URL=""http://127.0.0.1:8075""",
    "export MEMMCP_ORCHESTRATOR_URL=""http://127.0.0.1:8075""",
    "export CONTEXTLATTICE_AGENT_ID=""codex_gpt5""",
    "export MEMMCP_AGENT_ID=""codex_gpt5"""
)
if ($existingHookEnvLines.Count -gt 0) {
    $hookEnvLines += $existingHookEnvLines
}
Set-Content -Path $HookEnvFile -Value $hookEnvLines -Encoding Ascii

function Test-AgentCommand {
    param([string[]]$Names)
    foreach ($name in $Names) {
        if (Get-Command $name -ErrorAction SilentlyContinue) {
            return $true
        }
    }
    return $false
}

function Add-AgentInstructionHook {
    param(
        [string]$Path,
        [string]$Profile,
        [string]$Label,
        [string]$AgentId,
        [string]$TopicPath
    )
    $begin = "<!-- >>> contextlattice-agent-install:$Profile >>>"
    $end = "<!-- <<< contextlattice-agent-install:$Profile <<< -->"
    $block = @(
        $begin,
        "# ContextLattice $Label Hook",
        "",
        "ContextLattice is the local memory and context layer for this agent.",
        "",
        "- Profile: ``$Profile``",
        "- Stable agent id: ``$AgentId``",
        "- Topic path: ``$TopicPath``",
        "- Default orchestrator: ``http://127.0.0.1:8075``",
        "",
        "Before substantial planning or coding, retrieve scoped context:",
        "",
        "~~~bash",
        "contextlattice_agent_start --soft --compact",
        "contextlattice_agent_adapter bootstrap --agent $Profile --project contextlattice --pretty",
        "contextlattice_agent_adapter context-pack --agent $Profile --project contextlattice --pretty",
        "~~~",
        "",
        "During long work, checkpoint decisions with ``contextlattice_checkpoint`` or ``contextlattice_agent_adapter checkpoint``.",
        "Before handoff or compaction, write a concise handoff through ``contextlattice_agent_adapter handoff``.",
        "If ContextLattice is unreachable, continue from local evidence and state ``degraded-memory mode`` explicitly.",
        $end
    ) -join "`r`n"
    $current = ""
    if (Test-Path $Path) {
        $current = Get-Content -Path $Path -Raw -ErrorAction SilentlyContinue
    }
    $pattern = [regex]::Escape($begin) + "[\s\S]*?" + [regex]::Escape($end) + "\r?\n?"
    $without = [regex]::Replace([string]$current, $pattern, "").TrimEnd()
    $next = if ($without.Length -gt 0) { "$without`r`n`r`n$block" } else { $block }
    New-Item -ItemType Directory -Path (Split-Path $Path -Parent) -Force | Out-Null
    Set-Content -Path $Path -Value $next -Encoding Ascii
}

if (-not $NoAgentHooks.IsPresent) {
    $ompPath = Join-Path $HomeDir ".omp\agent\AGENTS.md"
    if ((Test-AgentCommand @("omp")) -or (Test-Path (Join-Path $HomeDir ".omp\agent"))) {
        Add-AgentInstructionHook -Path $ompPath -Profile "omp" -Label "OMP" -AgentId "omp_agent" -TopicPath "runbooks/omp-integration"
        Write-Host "Installed OMP ContextLattice instruction hook in `$HOME/.omp/agent/AGENTS.md"
    }
    $mercuryPath = Join-Path $HomeDir ".mercury\soul.md"
    if ((Test-AgentCommand @("mercury-agent", "mercury")) -or (Test-Path (Join-Path $HomeDir ".mercury"))) {
        Add-AgentInstructionHook -Path $mercuryPath -Profile "mercury-agent" -Label "Mercury Agent" -AgentId "mercury_agent" -TopicPath "runbooks/mercury-agent-integration"
        Write-Host "Installed Mercury ContextLattice instruction hook in `$HOME/.mercury/soul.md"
    }
}

$hookRuntimePythonFiles = @(
    "scripts\agent\_common.py",
    "scripts\agent\audit-codex-session-store",
    "scripts\agent\compaction-handoff-payload",
    "scripts\agent\contextlattice-session",
    "scripts\agent_contracts.py",
    "scripts\agent_orchestration.py",
    "scripts\contextlattice_client.py"
)
foreach ($relPath in $hookRuntimePythonFiles) {
    $src = Join-Path $RepoRoot $relPath
    if (-not (Test-Path $src)) {
        throw "Missing required hook runtime script: $src"
    }
    $dstRel = $relPath -replace '^scripts\\', ''
    $dst = Join-Path $ScriptsDir $dstRel
    New-Item -ItemType Directory -Path (Split-Path $dst -Parent) -Force | Out-Null
    Copy-Item -Path $src -Destination $dst -Force
}

if ($IncludeDevPythonTools.IsPresent) {
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

if ($IncludeDevPythonTools.IsPresent -and -not $SkipVenv.IsPresent) {
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

$GoCmd = Get-Command go -ErrorAction SilentlyContinue
if ($null -eq $GoCmd) {
    throw "go is required to install Go-native ContextLattice agent tools."
}
$GoTool = Join-Path $BinDir "contextlattice-agent-tools.exe"
if (Test-Path $GoTool) {
    Remove-Item $GoTool -Force
}
Push-Location (Join-Path $RepoRoot "services\gateway-go")
try {
    & go build -o $GoTool .\cmd\contextlattice-agent-tools
} finally {
    Pop-Location
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

$asyncInboxHookCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set GO_TOOL=%TOOL_HOME%\bin\contextlattice-agent-tools.exe
if not exist "%GO_TOOL%" (
  exit /b 0
)
"%GO_TOOL%" async-inbox-drain --timeout 1.5 --max-items 1 %*
exit /b 0
"@

$runnerQualityCmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set PYTHON_EXE=%TOOL_HOME%\venv-agent-tools\Scripts\python.exe
set SCRIPT_PATH=%TOOL_HOME%\scripts\agent\runner-quality
if not exist "%PYTHON_EXE%" (
  echo Missing %PYTHON_EXE%. Run scripts\install_global_agent_tools.ps1 -IncludeDevPythonTools first.
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
Set-Content -Path (Join-Path $BinDir "contextlattice_async_inbox_hook.cmd") -Value $asyncInboxHookCmd -Encoding Ascii
Set-Content -Path (Join-Path $BinDir "contextlattice_runner_quality.cmd") -Value $runnerQualityCmd -Encoding Ascii

function Write-GoNativeCmd {
    param([string]$Name)
    $cmd = @"
@echo off
set TOOL_HOME=%CONTEXTLATTICE_GLOBAL_HOME%
if "%TOOL_HOME%"=="" set TOOL_HOME=%USERPROFILE%\.contextlattice
set GO_TOOL=%TOOL_HOME%\bin\contextlattice-agent-tools.exe
if not exist "%GO_TOOL%" (
  echo Missing %GO_TOOL%. Run scripts\install_global_agent_tools.ps1 first.
  exit /b 1
)
"%GO_TOOL%" %*
"@
    Set-Content -Path (Join-Path $BinDir "$Name.cmd") -Value $cmd -Encoding Ascii
}

$goNativeCommands = @(
    "contextlattice_search",
    "contextlattice_pack",
    "contextlattice_synthesis_pack",
    "contextlattice_write",
    "contextlattice_adopt",
    "contextlattice_doctor",
    "contextlattice_agent_adapter",
    "contextlattice_agent_session",
    "contextlattice_async_inbox_drain",
    "contextlattice_agent_trace",
    "contextlattice_run_advisor",
    "contextlattice_agent_runtime_proof",
    "contextlattice_agent_adoption_proof",
    "contextlattice_agent_runtime_doctor",
    "contextlattice_strict_runtime_native_ownership",
    "contextlattice_context_boundary",
    "contextlattice_memory_topology",
    "contextlattice_skills_index",
    "contextlattice_runner_quality"
)
foreach ($name in $goNativeCommands) {
    Write-GoNativeCmd $name
}

if (-not $IncludeDevPythonTools.IsPresent) {
    foreach ($name in @(
        "contextlattice_agent_orchestration",
        "contextlattice_source_backfill",
        "contextlattice_codex_session_store_doctor"
    )) {
        $path = Join-Path $BinDir "$name.cmd"
        if (Test-Path $path) {
            Remove-Item $path -Force
        }
    }
}

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
Write-Host "  contextlattice_synthesis_pack `"what should this agent know before solving the task?`" --project contextlattice --pretty"
Write-Host "  contextlattice_write -h"
Write-Host "  contextlattice_adopt status --pretty"
Write-Host "  contextlattice_doctor --agents codex --skip-provider-smoke --pretty"
Write-Host "  contextlattice_adopt proof --agents codex --skip-provider-smoke --pretty"
Write-Host "  contextlattice_agent_adapter profiles"
Write-Host "  contextlattice_agent_session runtime --pretty"
Write-Host "  contextlattice_agent_runtime_proof --pretty"
Write-Host "  contextlattice_agent_adoption_proof --skip-provider-smoke --progress --pretty"
Write-Host "  contextlattice_context_boundary --pretty"
Write-Host "  contextlattice_async_inbox_drain --session-id <session-id>"
Write-Host "  contextlattice_async_inbox_hook --session-id <session-id>"
Write-Host "  contextlattice_agent_trace --session-id <session-id> --tree"
Write-Host "  contextlattice_run_advisor --session-id <session-id> --pretty"
Write-Host "  contextlattice_memory_topology --pretty"
Write-Host "  contextlattice_agent_runtime_doctor --pretty"
Write-Host "  contextlattice_skills_index search agent --pretty"
Write-Host "  contextlattice_runner_quality --pretty"
if ($IncludeDevPythonTools.IsPresent) {
    Write-Host "  contextlattice_source_backfill --source jsonl --path data.jsonl --project my-project --pretty"
    Write-Host "  contextlattice_codex_session_store_doctor --pretty"
}
