Param(
    [string]$RepoUrl = "https://github.com/sheawinkler/ContextLattice.git",
    [string]$InstallDir = "$env:USERPROFILE\ContextLattice",
    [switch]$FullMode
)

$ErrorActionPreference = "Stop"

function Require-Command {
    Param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$Hint
    )
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "$Name is required. $Hint"
    }
}

function Get-EnvValue {
    Param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Key
    )
    if (-not (Test-Path $Path)) {
        return ""
    }
    $line = Select-String -Path $Path -Pattern "^$Key=" | Select-Object -Last 1
    if ($null -eq $line) {
        return ""
    }
    return ($line.Line -replace "^$Key=", "")
}

function Set-EnvValue {
    Param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Key,
        [Parameter(Mandatory = $true)][string]$Value
    )
    $newLine = "$Key=$Value"
    if (-not (Test-Path $Path)) {
        Set-Content -Path $Path -Value $newLine -Encoding Ascii
        return
    }
    $lines = Get-Content -Path $Path
    $updated = $false
    for ($i = 0; $i -lt $lines.Count; $i++) {
        if ($lines[$i] -match "^$Key=") {
            $lines[$i] = $newLine
            $updated = $true
            break
        }
    }
    if (-not $updated) {
        $lines += $newLine
    }
    Set-Content -Path $Path -Value $lines -Encoding Ascii
}

function New-OrchestratorKey {
    return "cl_$([guid]::NewGuid().ToString('N'))$([guid]::NewGuid().ToString('N').Substring(0, 16))"
}

Write-Host "== ContextLattice Windows Installer =="
Write-Host "Repo: $RepoUrl"
Write-Host "Install dir: $InstallDir"

Require-Command -Name "git" -Hint "Install Git for Windows, then rerun."
Require-Command -Name "docker" -Hint "Install Docker Desktop and ensure it is running."

if (-not (Test-Path $InstallDir)) {
    Write-Host "Cloning repository..."
    git clone $RepoUrl $InstallDir
} else {
    Write-Host "Updating existing repository..."
    git -C $InstallDir pull --ff-only
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "git pull failed; continuing with existing local checkout."
    }
}

Set-Location $InstallDir

if (-not (Test-Path ".env")) {
    Copy-Item ".env.example" ".env"
}

$envPath = Join-Path $InstallDir ".env"
$key = Get-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_API_KEY"
if ([string]::IsNullOrWhiteSpace($key)) {
    $key = New-OrchestratorKey
}

Set-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_API_KEY" -Value $key
Set-EnvValue -Path $envPath -Key "MEMMCP_ORCHESTRATOR_API_KEY" -Value $key
Set-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_URL" -Value "http://127.0.0.1:8075"
Set-EnvValue -Path $envPath -Key "MEMMCP_ORCHESTRATOR_URL" -Value "http://127.0.0.1:8075"
Set-EnvValue -Path $envPath -Key "HOST_BIND_ADDRESS" -Value "127.0.0.1"
Set-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ENV" -Value "production"
Set-EnvValue -Path $envPath -Key "ORCH_SECURITY_STRICT" -Value "true"

$composeFile = "docker-compose.lite.yml"
if ($FullMode.IsPresent) {
    $composeFile = "docker-compose.yml"
}

Write-Host "Launching stack with $composeFile ..."
docker compose -f $composeFile up -d --build
if ($LASTEXITCODE -ne 0) {
    throw "docker compose failed."
}

Start-Sleep -Seconds 5

try {
    $health = Invoke-RestMethod -Uri "http://127.0.0.1:8075/health" -Method Get -TimeoutSec 20
    Write-Host "Health check: ok=$($health.ok)"
} catch {
    Write-Warning "Health endpoint not ready yet: $($_.Exception.Message)"
}

try {
    $headers = @{ "x-api-key" = $key }
    Invoke-RestMethod -Uri "http://127.0.0.1:8075/status" -Headers $headers -Method Get -TimeoutSec 20 | Out-Null
    Write-Host "Status check: ok"
} catch {
    Write-Warning "Status endpoint not ready yet: $($_.Exception.Message)"
}

Write-Host ""
Write-Host "Install complete."
Write-Host "Run ContextLattice-Monitor.cmd for health + telemetry checks."

try {
    Start-Process "http://127.0.0.1:3000" | Out-Null
} catch {
    Write-Warning "Could not open dashboard URL automatically."
}
