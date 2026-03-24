Param(
    [string]$InstallDir = "$env:USERPROFILE\ContextLattice",
    [string]$BaseUrl = "http://127.0.0.1:8075",
    [string]$ApiKey = ""
)

$ErrorActionPreference = "Stop"

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

Write-Host "== ContextLattice Monitor =="

$envPath = Join-Path $InstallDir ".env"
if ([string]::IsNullOrWhiteSpace($ApiKey)) {
    $ApiKey = Get-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_API_KEY"
}
if ([string]::IsNullOrWhiteSpace($ApiKey)) {
    $ApiKey = Get-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_API_KEY"
}

$health = Invoke-RestMethod -Uri "$BaseUrl/health" -Method Get -TimeoutSec 20
Write-Host ""
Write-Host "/health"
$health | ConvertTo-Json -Depth 8

if (-not [string]::IsNullOrWhiteSpace($ApiKey)) {
    $headers = @{ "x-api-key" = $ApiKey }

    Write-Host ""
    Write-Host "/status"
    $status = Invoke-RestMethod -Uri "$BaseUrl/status" -Headers $headers -Method Get -TimeoutSec 30
    $status | ConvertTo-Json -Depth 8

    Write-Host ""
    Write-Host "/telemetry/fanout"
    $fanout = Invoke-RestMethod -Uri "$BaseUrl/telemetry/fanout" -Headers $headers -Method Get -TimeoutSec 30
    $fanout | ConvertTo-Json -Depth 8
} else {
    Write-Warning "API key not found in $envPath; skipping authenticated checks."
}

try {
    Start-Process "http://127.0.0.1:3000" | Out-Null
} catch {
    Write-Warning "Could not open dashboard URL automatically."
}
