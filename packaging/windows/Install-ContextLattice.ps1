Param(
    [string]$InstallDir = "$env:USERPROFILE\ContextLattice",
    [switch]$FullMode,
    [switch]$ExtractOnly,
    [switch]$NoLaunch,
    [switch]$AllowPaidToPublicDowngrade
)

$ErrorActionPreference = "Stop"
$ExpectedReleaseLane = "@RELEASE_LANE@"

if ($ExpectedReleaseLane -ne "public") {
    throw "Public installer lane was not baked at build time."
}
$ExpectedSourceRepository = "sheawinkler/ContextLattice"
$ExpectedSourceRef = "refs/heads/main"

function Read-ReleaseMetadata {
    Param(
        [Parameter(Mandatory = $true)][string]$Path,
        [switch]$InstalledCopy
    )

    $item = Get-Item -LiteralPath $Path
    if ($item.Length -gt 4096) {
        throw "Release metadata exceeds its 4096-byte bound: $Path"
    }
    try {
        $metadata = Get-Content -LiteralPath $Path -Raw | ConvertFrom-Json
    } catch {
        throw "Release metadata is not valid JSON: $Path"
    }

    if ($InstalledCopy.IsPresent) {
        if ($metadata.lane -notin @("paid", "public")) {
            throw "Installed release lane is invalid; refusing an ambiguous replacement."
        }
        return $metadata
    }

    if ($metadata.schema_id -ne "contextlattice_release_payload.v2") {
        throw "Unsupported release metadata schema: $($metadata.schema_id)"
    }
    if ($metadata.lane -ne $ExpectedReleaseLane) {
        throw "Release lane mismatch: installer=$ExpectedReleaseLane payload=$($metadata.lane)"
    }
    if ($metadata.commit -notmatch "^[0-9a-f]{40}$") {
        throw "Release metadata commit is invalid."
    }
    if ($metadata.tag -notmatch "^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$") {
        throw "Release metadata tag is invalid."
    }
    if ($metadata.release_ref -ne "refs/tags/$($metadata.tag)") {
        throw "Release metadata tag ref is invalid."
    }
    if ($metadata.source -ne "approved_lane_tagged_checkout") {
        throw "Release metadata source is invalid."
    }
    if ($metadata.approved_source_repository -ne $ExpectedSourceRepository) {
        throw "Release source repository mismatch for $ExpectedReleaseLane lane."
    }
    if ($metadata.approved_source_ref -ne $ExpectedSourceRef) {
        throw "Release source ref mismatch for $ExpectedReleaseLane lane."
    }
    if ($metadata.tag -notlike "*-public" -and $metadata.tag -notmatch '^v\d+\.\d+\.\d+$') {
        throw "Public release metadata tag must be vX.Y.Z or end with '-public'."
    }
    return $metadata
}

function Assert-NoReparsePoints {
    Param([Parameter(Mandatory = $true)][string]$Path)

    $root = Get-Item -LiteralPath $Path -Force
    if (($root.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Reparse points are not allowed: $Path"
    }
    foreach ($item in Get-ChildItem -LiteralPath $Path -Force -Recurse) {
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Reparse points are not allowed: $($item.FullName)"
        }
    }
}

function Copy-PayloadTree {
    Param(
        [Parameter(Mandatory = $true)][string]$Source,
        [Parameter(Mandatory = $true)][string]$Destination
    )

    New-Item -ItemType Directory -Path $Destination -Force | Out-Null
    foreach ($item in Get-ChildItem -LiteralPath $Source -Force) {
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Reparse points are not allowed: $($item.FullName)"
        }
        $destinationPath = Join-Path $Destination $item.Name
        if ($item.PSIsContainer) {
            Copy-PayloadTree -Source $item.FullName -Destination $destinationPath
        } else {
            Copy-Item -LiteralPath $item.FullName -Destination $destinationPath -Force
        }
    }
}

function Get-EnvValue {
    Param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Key
    )

    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) {
        return ""
    }
    $prefix = "$Key="
    $value = ""
    foreach ($line in [System.IO.File]::ReadAllLines($Path)) {
        if ($line.StartsWith($prefix, [System.StringComparison]::Ordinal)) {
            $value = $line.Substring($prefix.Length)
        }
    }
    return $value
}

function Set-EnvValue {
    Param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Key,
        [Parameter(Mandatory = $true)][string]$Value
    )

    $newLine = "$Key=$Value"
    $prefix = "$Key="
    $result = New-Object System.Collections.Generic.List[string]
    $updated = $false
    foreach ($line in [System.IO.File]::ReadAllLines($Path)) {
        if ($line.StartsWith($prefix, [System.StringComparison]::Ordinal)) {
            if (-not $updated) {
                $result.Add($newLine)
                $updated = $true
            }
            continue
        }
        $result.Add($line)
    }
    if (-not $updated) {
        $result.Add($newLine)
    }

    $tempPath = "$Path.tmp-$([guid]::NewGuid().ToString('N'))"
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllLines($tempPath, $result, $utf8NoBom)
    Move-Item -LiteralPath $tempPath -Destination $Path -Force
}

function New-OrchestratorKey {
    $bytes = New-Object byte[] 24
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $rng.GetBytes($bytes)
    } finally {
        $rng.Dispose()
    }
    $hex = -join ($bytes | ForEach-Object { $_.ToString("x2") })
    return "cl_$hex"
}

$payloadDir = Join-Path $PSScriptRoot "payload"
$archivePath = Join-Path $payloadDir "contextlattice-payload.zip"
$checksumPath = "$archivePath.sha256"
$metadataPath = Join-Path $payloadDir "contextlattice-release.json"

if (-not (Test-Path -LiteralPath $archivePath -PathType Leaf) -or
    -not (Test-Path -LiteralPath $checksumPath -PathType Leaf) -or
    -not (Test-Path -LiteralPath $metadataPath -PathType Leaf)) {
    throw "Embedded release payload is missing or incomplete."
}

$metadata = Read-ReleaseMetadata -Path $metadataPath
$checksumItem = Get-Item -LiteralPath $checksumPath
if ($checksumItem.Length -gt 256) {
    throw "Payload checksum file exceeds its 256-byte bound."
}
$checksumParts = (Get-Content -LiteralPath $checksumPath -TotalCount 1).Trim() -split "\s+"
if ($checksumParts.Count -lt 2 -or
    $checksumParts[0] -notmatch "^[0-9a-fA-F]{64}$" -or
    $checksumParts[1] -ne "contextlattice-payload.zip") {
    throw "Payload checksum file is invalid."
}
$expectedChecksum = $checksumParts[0].ToLowerInvariant()
$actualChecksum = (Get-FileHash -LiteralPath $archivePath -Algorithm SHA256).Hash.ToLowerInvariant()
if ($actualChecksum -ne $expectedChecksum) {
    throw "Embedded payload checksum mismatch."
}

$fullInstallDir = [System.IO.Path]::GetFullPath($InstallDir)
$installRoot = [System.IO.Path]::GetPathRoot($fullInstallDir)
if ($fullInstallDir -eq $installRoot) {
    throw "Refusing to install over a filesystem root."
}
$trimChars = [char[]]@(
    [System.IO.Path]::DirectorySeparatorChar,
    [System.IO.Path]::AltDirectorySeparatorChar
)
$InstallDir = $fullInstallDir.TrimEnd($trimChars)
$installParent = Split-Path -Parent $InstallDir
$installName = Split-Path -Leaf $InstallDir
if ([string]::IsNullOrWhiteSpace($installName)) {
    throw "Install directory must name a non-root path."
}
New-Item -ItemType Directory -Path $installParent -Force | Out-Null
if (Test-Path -LiteralPath $InstallDir) {
    $installItem = Get-Item -LiteralPath $InstallDir -Force
    if (-not $installItem.PSIsContainer) {
        throw "Install path exists but is not a directory: $InstallDir"
    }
    if (($installItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Install directory cannot be a reparse point: $InstallDir"
    }
}

$installedMetadataPath = Join-Path $InstallDir ".contextlattice-release.json"
if ((Test-Path -LiteralPath $InstallDir -PathType Container) -and
    -not (Test-Path -LiteralPath $installedMetadataPath -PathType Leaf)) {
    $legacyGitDir = Join-Path $InstallDir ".git"
    if (Test-Path -LiteralPath $legacyGitDir) {
        if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
            throw "Git is required to verify the legacy checkout before migration."
        }
        $trackedDiff = & git -C $InstallDir status --porcelain --untracked-files=all
        if ($LASTEXITCODE -ne 0) {
            throw "Could not verify the legacy checkout before migration."
        }
        if ($trackedDiff) {
            throw "Legacy checkout has local changes or untracked files; preserve or commit them before installer migration."
        }
        Write-Host "Migrating clean legacy repository checkout to a release-managed install."
    } else {
        $preservedNames = @(".env", ".data", "data", "backups")
        $unmanagedEntries = @(Get-ChildItem -LiteralPath $InstallDir -Force | Where-Object {
            $_.Name -notin $preservedNames
        })
        if ($unmanagedEntries.Count -gt 0) {
            throw "Existing install is unmanaged; move it aside before installing so files are not silently replaced."
        }
    }
}

$installedLane = ""
if (Test-Path -LiteralPath $installedMetadataPath -PathType Leaf) {
    $installedMetadata = Read-ReleaseMetadata -Path $installedMetadataPath -InstalledCopy
    $installedLane = $installedMetadata.lane
}
$installedPaidMarkers = @(
    "config\runtime-license",
    "services\gateway-go\cognition_activation_entitled.go",
    "services\gateway-go\context_mesh_orchestration_entitled.go",
    "services\gateway-go\frontier_t1_governance_entitled.go",
    "services\gateway-go\frontier_t2_packet_retention_entitled.go",
    "services\gateway-go\frontier_t2_proof_timeline_entitled.go",
    "services\gateway-go\frontier_t3_utility_entitled.go",
    "services\gateway-go\frontier_t4_retrieval_entitled.go"
)
$installedHasPaidMarkers = $false
foreach ($relativePath in $installedPaidMarkers) {
    if (Test-Path -LiteralPath (Join-Path $InstallDir $relativePath)) {
        $installedHasPaidMarkers = $true
        break
    }
}
if ($installedLane -eq "public" -and $installedHasPaidMarkers) {
    throw "Installed public metadata contradicts paid runtime files; refusing an ambiguous replacement."
}
if ([string]::IsNullOrEmpty($installedLane) -and $installedHasPaidMarkers) {
    $installedLane = "paid"
}
if ($installedLane -eq "paid" -and $ExpectedReleaseLane -eq "public") {
    if (-not $AllowPaidToPublicDowngrade.IsPresent) {
        throw "Paid-to-public downgrade refused; rerun with -AllowPaidToPublicDowngrade to remove paid files while preserving only declared user state/config."
    }
    Write-Host "Authorized paid-to-public downgrade: paid files will be removed; .env, .data, data, and backups will be preserved."
}

Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [System.IO.Compression.ZipFile]::OpenRead($archivePath)
try {
    $seenEntries = @{}
    foreach ($entry in $zip.Entries) {
        $path = $entry.FullName
        $segments = $path -split "/"
        $unixType = (($entry.ExternalAttributes -shr 16) -band 0xF000)
        if ([string]::IsNullOrWhiteSpace($path) -or
            -not $path.StartsWith("contextlattice/", [System.StringComparison]::Ordinal) -or
            $path.Contains("\") -or
            [System.IO.Path]::IsPathRooted($path) -or
            $segments -contains ".." -or
            $unixType -eq 0xA000) {
            throw "Embedded payload contains an unsafe archive path or link: $path"
        }
        if ($seenEntries.ContainsKey($path)) {
            throw "Embedded payload contains a duplicate archive path: $path"
        }
        $seenEntries[$path] = $true
    }
} finally {
    $zip.Dispose()
}

$tempExtract = Join-Path ([System.IO.Path]::GetTempPath()) ("contextlattice-install-" + [guid]::NewGuid().ToString("N"))
$transactionDir = Join-Path $installParent (".$installName.install-" + [guid]::NewGuid().ToString("N"))
$stagedInstall = Join-Path $transactionDir "staged"
$previousInstall = Join-Path $transactionDir "previous"
$backupMoved = $false
$stagePublished = $false
$committed = $false
$keepTransaction = $false

New-Item -ItemType Directory -Path $tempExtract -Force | Out-Null
New-Item -ItemType Directory -Path $stagedInstall -Force | Out-Null
try {
    Expand-Archive -LiteralPath $archivePath -DestinationPath $tempExtract -Force
    $payloadRoot = Join-Path $tempExtract "contextlattice"
    $embeddedMetadata = Join-Path $payloadRoot ".contextlattice-release.json"
    if (-not (Test-Path -LiteralPath $payloadRoot -PathType Container) -or
        -not (Test-Path -LiteralPath $embeddedMetadata -PathType Leaf)) {
        throw "Embedded payload root or metadata is missing."
    }
    Assert-NoReparsePoints -Path $payloadRoot
    if ((Get-FileHash -LiteralPath $embeddedMetadata -Algorithm SHA256).Hash -ne
        (Get-FileHash -LiteralPath $metadataPath -Algorithm SHA256).Hash) {
        throw "Embedded and installer release metadata differ."
    }
    foreach ($forbidden in @(".env", ".data", "data", "backups")) {
        if (Test-Path -LiteralPath (Join-Path $payloadRoot $forbidden)) {
            throw "Payload contains local environment or runtime data path: $forbidden"
        }
    }

    foreach ($relativePath in @(
        "docs\private",
        "private_docs",
        "private",
        ".ops",
        "config\runtime-license",
        "services\gateway-go\cognition_activation_entitled.go",
        "services\gateway-go\context_mesh_orchestration_entitled.go",
        "services\gateway-go\frontier_t1_governance_entitled.go",
        "services\gateway-go\frontier_t2_packet_retention_entitled.go",
        "services\gateway-go\frontier_t2_proof_timeline_entitled.go",
        "services\gateway-go\frontier_t3_utility_entitled.go",
        "services\gateway-go\frontier_t4_retrieval_entitled.go",
        "config\frontier_t1_release_provenance.v1.json"
    )) {
        if (Test-Path -LiteralPath (Join-Path $payloadRoot $relativePath)) {
            throw "Public payload contains a paid/private path: $relativePath"
        }
    }
    $runtimePattern = "context_policy_activation\.v1|context_mesh_orchestration\.v1|frontier_t1_governance_state\.v1|frontier_delta_packet_automation\.v1|frontier_shared_proof_timeline\.v1|frontier_t4_retrieval_governance_state\.v1|contextlattice_runtime_license_public_keys\.v1|GO_V4_(ENTITLEMENT|RUNTIME_LICENSE|MACHINE_BINDING)|runtimeLicenseVerifier|runtimeLicenseSchemaID"
    foreach ($relativePath in @("Dockerfile.gateway-go", "docker-compose.yml")) {
        $runtimePath = Join-Path $payloadRoot $relativePath
        if ((Test-Path -LiteralPath $runtimePath -PathType Leaf) -and
            (Select-String -LiteralPath $runtimePath -Pattern $runtimePattern -Quiet)) {
            throw "Public payload contains paid/private runtime markers in $relativePath."
        }
    }
    $gatewayPath = Join-Path $payloadRoot "services\gateway-go"
    if (Test-Path -LiteralPath $gatewayPath -PathType Container) {
        $gatewayFiles = @(Get-ChildItem -LiteralPath $gatewayPath -File -Recurse)
        if ($gatewayFiles.Count -gt 0 -and
            ($gatewayFiles | Select-String -Pattern $runtimePattern -Quiet)) {
            throw "Public payload contains paid/private gateway markers."
        }
    }

    Copy-PayloadTree -Source $payloadRoot -Destination $stagedInstall

    foreach ($relativePath in @(".env", ".data", "data", "backups")) {
        $sourcePath = Join-Path $InstallDir $relativePath
        if (-not (Test-Path -LiteralPath $sourcePath)) {
            continue
        }
        $sourceItem = Get-Item -LiteralPath $sourcePath -Force
        if (($sourceItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Preserved state path cannot be a reparse point: $relativePath"
        }
        $destinationPath = Join-Path $stagedInstall $relativePath
        if ($relativePath -eq ".env") {
            if ($sourceItem.PSIsContainer) {
                throw "Preserved .env path is not a regular file."
            }
            [System.IO.File]::WriteAllBytes($destinationPath, [System.IO.File]::ReadAllBytes($sourcePath))
        } else {
            if (-not $sourceItem.PSIsContainer) {
                throw "Preserved state path is not a directory: $relativePath"
            }
            Assert-NoReparsePoints -Path $sourcePath
            Copy-PayloadTree -Source $sourcePath -Destination $destinationPath
        }
    }

    $envPath = Join-Path $stagedInstall ".env"
    if (-not (Test-Path -LiteralPath $envPath -PathType Leaf)) {
        $envExample = Join-Path $stagedInstall ".env.example"
        if (-not (Test-Path -LiteralPath $envExample -PathType Leaf)) {
            throw "Payload is missing .env.example."
        }
        [System.IO.File]::WriteAllBytes($envPath, [System.IO.File]::ReadAllBytes($envExample))
        $key = Get-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_API_KEY"
        if ([string]::IsNullOrWhiteSpace($key)) {
            $key = New-OrchestratorKey
        }
        Set-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_API_KEY" -Value $key
        Set-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ORCHESTRATOR_URL" -Value "http://127.0.0.1:8075"
        Set-EnvValue -Path $envPath -Key "HOST_BIND_ADDRESS" -Value "127.0.0.1"
        Set-EnvValue -Path $envPath -Key "CONTEXTLATTICE_ENV" -Value "production"
        Set-EnvValue -Path $envPath -Key "ORCH_SECURITY_STRICT" -Value "true"
    } else {
        Write-Host "Preserving existing $InstallDir\.env secrets and custom settings."
    }

    if (Test-Path -LiteralPath $InstallDir) {
        $backupMoved = $true
        Move-Item -LiteralPath $InstallDir -Destination $previousInstall
    }
    $stagePublished = $true
    Move-Item -LiteralPath $stagedInstall -Destination $InstallDir
    $committed = $true
} finally {
    if (-not $committed) {
        if ($backupMoved -and (Test-Path -LiteralPath $previousInstall -PathType Container)) {
            try {
                if (Test-Path -LiteralPath $InstallDir) {
                    Remove-Item -LiteralPath $InstallDir -Recurse -Force
                }
                Move-Item -LiteralPath $previousInstall -Destination $InstallDir
            } catch {
                $keepTransaction = $true
                Write-Error "Automatic rollback failed; previous install remains at $previousInstall" -ErrorAction Continue
            }
        } elseif ($stagePublished -and (Test-Path -LiteralPath $InstallDir)) {
            Remove-Item -LiteralPath $InstallDir -Recurse -Force -ErrorAction SilentlyContinue
        }
    }
    Remove-Item -LiteralPath $tempExtract -Recurse -Force -ErrorAction SilentlyContinue
    if (-not $keepTransaction) {
        Remove-Item -LiteralPath $transactionDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Write-Host "ContextLattice $($metadata.lane) payload $($metadata.tag) ($($metadata.commit)) installed atomically at $InstallDir."
if ($ExtractOnly.IsPresent -or $NoLaunch.IsPresent) {
    exit 0
}

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw "Docker Desktop with Compose v2 is required to launch ContextLattice."
}

Set-Location $InstallDir
if (Test-Path -LiteralPath "scripts\install_global_agent_tools.ps1") {
    try {
        & powershell -ExecutionPolicy Bypass -File "scripts\install_global_agent_tools.ps1"
    } catch {
        Write-Warning "Global agent tool install failed: $($_.Exception.Message)"
    }
}

$composeFile = "docker-compose.lite.yml"
if ($FullMode.IsPresent) {
    $composeFile = "docker-compose.yml"
}
Write-Host "Launching stack with $composeFile ..."
docker compose -f $composeFile up -d --build
if ($LASTEXITCODE -ne 0) {
    throw "docker compose failed."
}

Write-Host "Install complete. API: http://127.0.0.1:8075 Dashboard: http://127.0.0.1:3000"
try {
    Start-Process "http://127.0.0.1:3000" | Out-Null
} catch {
    Write-Warning "Could not open dashboard URL automatically."
}
