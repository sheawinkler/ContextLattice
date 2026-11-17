Param(
    [string]$InstallDir = "$env:USERPROFILE\ContextLattice",
    [switch]$FullMode,
    [switch]$ExtractOnly,
    [switch]$NoLaunch,
    [switch]$AllowPaidToPublicDowngrade
)

$ErrorActionPreference = "Stop"
$ExpectedReleaseLane = "@RELEASE_LANE@"

if (-not $ExtractOnly.IsPresent -and -not $NoLaunch.IsPresent) {
    if (-not (Get-Command docker -ErrorAction SilentlyContinue) -or
        -not (Get-Command curl.exe -ErrorAction SilentlyContinue)) {
        throw "Docker Compose and curl.exe are required to launch and verify ContextLattice."
    }
    & docker compose version *> $null
    if ($LASTEXITCODE -ne 0) { throw "Docker Compose v2 is required to launch ContextLattice." }
    $dockerServerOS = @(& docker info --format '{{.OSType}}')
    if ($LASTEXITCODE -ne 0 -or $dockerServerOS.Count -ne 1 -or $dockerServerOS[0] -cne 'linux') {
        throw "A reachable Linux-container Docker daemon is required; no installation files were replaced."
    }
}

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
    if ($metadata.source_tree -notmatch "^[0-9a-f]{40}$") {
        throw "Release metadata source tree is invalid."
    }
    if ($metadata.runtime_version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$') {
        throw "Release metadata runtime version is invalid."
    }
    if ($metadata.build_channel -notin @("stable", "candidate")) {
        throw "Release metadata build channel is invalid."
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

function Invoke-BoundedDockerReadOnly {
    Param([string[]]$DockerArgs, [int]$Limit, [System.Diagnostics.Stopwatch]$Clock)

    $process = New-Object System.Diagnostics.Process
    $process.StartInfo.FileName = (Get-Command docker -CommandType Application -ErrorAction Stop).Source
    # Only fixed arguments, templates without quotes, and validated hex IDs enter here.
    if (@($DockerArgs | Where-Object { $_.Contains('"') }).Count -ne 0) {
        throw "Invalid Docker inspection argument."
    }
    $process.StartInfo.Arguments = ($DockerArgs | ForEach-Object { '"' + $_ + '"' }) -join ' '
    $process.StartInfo.UseShellExecute = $false
    $process.StartInfo.CreateNoWindow = $true
    $process.StartInfo.RedirectStandardOutput = $true
    $process.StartInfo.RedirectStandardError = $true
    $output = New-Object System.Text.StringBuilder
    [char[]]$outBuffer = New-Object char[] 4096
    [char[]]$errBuffer = New-Object char[] 4096
    $errorSize = 0
    $endMs = [Math]::Min(30000, $Clock.ElapsedMilliseconds + 10000)
    try {
        if (-not $process.Start()) { throw "Docker inspection could not start." }
        $outRead = $process.StandardOutput.ReadAsync($outBuffer, 0, $outBuffer.Length)
        $errRead = $process.StandardError.ReadAsync($errBuffer, 0, $errBuffer.Length)
        while ($null -ne $outRead -or $null -ne $errRead -or -not $process.HasExited) {
            if ($Clock.ElapsedMilliseconds -ge $endMs) { throw "Docker inspection timed out." }
            if ($null -ne $outRead -and $outRead.IsCompleted) {
                $count = $outRead.GetAwaiter().GetResult()
                if ($output.Length + $count -gt $Limit) { throw "Docker inspection output exceeds its bound." }
                if ($count -eq 0) { $outRead = $null } else {
                    [void]$output.Append($outBuffer, 0, $count)
                    $outRead = $process.StandardOutput.ReadAsync($outBuffer, 0, $outBuffer.Length)
                }
            }
            if ($null -ne $errRead -and $errRead.IsCompleted) {
                $count = $errRead.GetAwaiter().GetResult()
                $errorSize += $count
                if ($errorSize -gt $Limit) { throw "Docker inspection diagnostics exceed their bound." }
                if ($count -eq 0) { $errRead = $null } else {
                    $errRead = $process.StandardError.ReadAsync($errBuffer, 0, $errBuffer.Length)
                }
            }
            [System.Threading.Thread]::Sleep(10)
        }
        if ($process.ExitCode -ne 0) { throw "Docker inspection failed; check the selected context and daemon/socket." }
        return $output.ToString()
    } finally {
        # Terminate only our own read-only CLI child, never a container or daemon.
        try { if (-not $process.HasExited) { $process.Kill() } } catch { }
        $process.Dispose()
    }
}

function ConvertTo-BindComparisonPath {
    Param([string]$Source)

    if ([string]::IsNullOrWhiteSpace($Source) -or $Source.Length -gt 4096 -or $Source.Contains([string][char]0)) {
        throw "Docker bind source is malformed."
    }
    $normalized = $Source.Replace('\', '/')
    $nativePath = $null
    if ($normalized -match '^[a-zA-Z]:/' -or $normalized.StartsWith('//')) {
        $normalized = [System.IO.Path]::GetFullPath($normalized).Replace('\', '/')
        $nativePath = $normalized
        if ($normalized -match '^([a-zA-Z]):/(.*)$') {
            $normalized = '/' + $Matches[1] + '/' + $Matches[2]
        }
    } elseif (-not $normalized.StartsWith('/')) {
        throw "Docker bind source is not absolute."
    }
    # Put native drives and Docker Desktop/WSL aliases in one /c/... namespace.
    # Namespace ancestors conservatively cover every host drive.
    $parts = New-Object 'System.Collections.Generic.List[string]'
    foreach ($part in ($normalized -split '/')) {
        if ($part -eq '' -or $part -eq '.') { continue }
        if ($part -eq '..') {
            if ($parts.Count -gt 0) { $parts.RemoveAt($parts.Count - 1) }
        } else { $parts.Add($part) }
    }
    $normalized = ('/' + ($parts -join '/')).ToLowerInvariant()
    if ($normalized -in @('/run', '/run/desktop', '/run/desktop/mnt')) { return '/' }
    $normalized = $normalized -replace '^/(run/desktop/mnt/host|host_mnt|mnt)(?=/|$)', ''
    if ($normalized.Length -eq 0) { return '/' }
    if ($normalized -match '^/([a-z])(?:/(.*))?$') {
        $nativePath = $Matches[1] + ':/' + $Matches[2]
    }
    # Lexical comparisons cannot prove disjointness through junctions/symlinks.
    # Refuse instead of introducing a new native path-resolution engine.
    while (-not [string]::IsNullOrEmpty($nativePath)) {
        if (Test-Path -LiteralPath $nativePath) {
            $item = Get-Item -LiteralPath $nativePath -Force
            if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                throw "Cannot prove Docker bind safety through a reparse point; stop owned writers and use an offline file-only environment."
            }
        }
        $parentPath = [System.IO.Path]::GetDirectoryName($nativePath)
        if ($parentPath -eq $nativePath) { break }
        $nativePath = $parentPath
    }
    return $normalized
}

function Assert-NoLivePreservedBinds {
    if (-not (Get-Command docker -CommandType Application -ErrorAction SilentlyContinue)) {
        if (-not $ExtractOnly.IsPresent -and -not $NoLaunch.IsPresent) {
            throw "Docker is required for preserved-state safety inspection."
        }
        Write-Warning "Offline file-only install: Docker is absent; all writers must already be stopped, including writers on other Docker contexts/hosts."
        return
    }
    $clock = [System.Diagnostics.Stopwatch]::StartNew()
    $preserved = @('.env', '.data', 'data', 'backups') | ForEach-Object {
        ConvertTo-BindComparisonPath -Source (Join-Path $InstallDir $_)
    }
    $rawIds = Invoke-BoundedDockerReadOnly -DockerArgs @('ps', '--no-trunc', '--quiet') -Limit 65536 -Clock $clock
    $ids = @()
    if ($rawIds.Length -ne 0) { $ids = @($rawIds.TrimEnd("`r", "`n") -split '\r?\n') }
    if ($ids.Count -gt 256 -or @($ids | Select-Object -Unique).Count -ne $ids.Count -or
        @($ids | Where-Object { $_ -cnotmatch '\A[0-9a-f]{64}\z' }).Count -ne 0) {
        throw "Docker running-container IDs are malformed or exceed the 256-container bound."
    }
    foreach ($containerId in $ids) {
        $raw = Invoke-BoundedDockerReadOnly -DockerArgs @('inspect', '--type', 'container', '--format',
            '[{{json .State.Running}},{{json .Mounts}}]', $containerId) -Limit 1048576 -Clock $clock
        try { $probe = ConvertFrom-Json -InputObject $raw } catch { throw "Docker mount inspection is not valid JSON." }
        if ($probe -isnot [array] -or $probe.Count -ne 2 -or $probe[0] -isnot [bool] -or
            $probe[1] -isnot [array] -or $probe[1].Count -gt 1024) {
            throw "Docker mount inspection has an invalid shape or exceeds the mount bound."
        }
        foreach ($mount in $probe[1]) {
            if ($mount -isnot [System.Management.Automation.PSCustomObject] -or
                $mount.Type -cnotin @('bind', 'volume', 'tmpfs') -or $mount.RW -isnot [bool]) {
                throw "Docker mount inspection is malformed."
            }
            if ($mount.Type -cne 'bind') { continue }
            if ($mount.Source -isnot [string]) { throw "Docker bind source is malformed." }
            if (-not $probe[0] -or -not $mount.RW) { continue }
            $source = ConvertTo-BindComparisonPath -Source $mount.Source
            foreach ($target in $preserved) {
                if ($source -eq $target -or $source.StartsWith($target.TrimEnd('/') + '/', [System.StringComparison]::Ordinal) -or
                    $target.StartsWith($source.TrimEnd('/') + '/', [System.StringComparison]::Ordinal)) {
                    throw "Running container $($containerId.Substring(0, 12)) has a writable bind overlapping install-local preserved state; stop that owned writer before update and rerun. Do not stop unrelated containers."
                }
            }
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
    "services\gateway-go\frontier_t4_retrieval_entitled.go",
    "services\gateway-go\frontier_t5_policy_lab_entitled.go",
    "services\gateway-go\frontier_t6_agent_fit_entitled.go",
    "services\gateway-go\frontier_t7_portable_continuation_entitled.go",
    "services\gateway-go\frontier_t7_portable_continuation_entitled_test.go",
    "services\gateway-go\frontier_t8_skill_evolution_entitled.go",
    "services\gateway-go\frontier_t8_skill_evolution_entitled_test.go",
    "services\gateway-go\frontier_t9_continuity_zero_entitled.go",
    "services\gateway-go\frontier_t9_continuity_zero_entitled_test.go",
    "services\gateway-go\frontier_t10_aggregate_signal_entitled.go",
    "services\gateway-go\frontier_t10_aggregate_signal_entitled_test.go"
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

# Precondition only: keep all writers stopped through preservation/publication.
# File-only switches do not bypass inspection when Docker is available.
Assert-NoLivePreservedBinds

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
        "services\gateway-go\frontier_t5_policy_lab_entitled.go",
        "docs\evals\v3.22-frontier-t5-paid-activation.json",
        "services\gateway-go\frontier_t6_agent_fit_entitled.go",
        "docs\evals\v3.23-frontier-t6-paid-activation.json",
        "services\gateway-go\frontier_t7_portable_continuation_entitled.go",
        "services\gateway-go\frontier_t7_portable_continuation_entitled_test.go",
        "docs\evals\v3.24-frontier-t7-paid-activation.json",
        "services\gateway-go\frontier_t8_skill_evolution_entitled.go",
        "services\gateway-go\frontier_t8_skill_evolution_entitled_test.go",
        "docs\evals\v3.25-frontier-t8-paid-activation.json",
        "services\gateway-go\frontier_t9_continuity_zero_entitled.go",
        "services\gateway-go\frontier_t9_continuity_zero_entitled_test.go",
        "docs\evals\v3.26-frontier-t9-paid-activation.json",
        "services\gateway-go\frontier_t10_aggregate_signal_entitled.go",
        "services\gateway-go\frontier_t10_aggregate_signal_entitled_test.go",
        "services\gateway-go\frontier_t10_secure_aggregation_research_private.go",
        "services\gateway-go\frontier_t10_secure_aggregation_research_private_test.go",
        "docs\evals\v4.0-frontier-t10-paid-activation.json",
        "config\frontier_t1_release_provenance.v1.json"
    )) {
        if (Test-Path -LiteralPath (Join-Path $payloadRoot $relativePath)) {
            throw "Public payload contains a paid/private path: $relativePath"
        }
    }
    $runtimePattern = "context_policy_activation\.v1|context_mesh_orchestration\.v1|frontier_delta_packet_automation\.v1|frontier_shared_proof_timeline\.v1|frontier_t4_retrieval_governance_state\.v1|frontier_t5_policy_laboratory_governance_state\.v1|frontier_t6_agent_fit_governance_state\.v1|frontier_t7_portable_continuation_governance_state\.v1|frontier_t8_skill_evolution_governance_state\.v1|frontier_t9_continuity_zero_governance_state\.v1|frontier_t10_aggregate_governance_state\.v1|contextlattice_runtime_license_public_keys\.v1|CONTEXTLATTICE_FRONTIER_T2_|CONTEXTLATTICE_FRONTIER_T5_POLICY_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T6_AGENT_FIT_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T7_PORTABLE_CONTINUATION_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T8_SKILL_EVOLUTION_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T9_CONTINUITY_ZERO_GOVERNANCE|CONTEXTLATTICE_FRONTIER_T10_AGGREGATE_GOVERNANCE|GO_V4_(ENTITLEMENT|RUNTIME_LICENSE|MACHINE_BINDING)|runtimeLicenseVerifier|runtimeLicenseSchemaID"
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
    if (-not $keepTransaction -and -not $committed) {
        Remove-Item -LiteralPath $transactionDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Write-Host "ContextLattice $($metadata.lane) payload $($metadata.tag) ($($metadata.commit)) installed atomically at $InstallDir."
if ($ExtractOnly.IsPresent -or $NoLaunch.IsPresent) {
    Remove-Item -LiteralPath $transactionDir -Recurse -Force -ErrorAction SilentlyContinue
    exit 0
}

try {
    Set-Location $InstallDir
} catch {
    Write-Warning "Launch failed after file commit; retaining both current install at $InstallDir and transaction at $transactionDir. Runtime state may have changed; automatic rollback is unsafe."
    throw
}

function Get-GatewayReadinessIdentity {
    Param([string]$ContainerId)
    # No embedded quotes: Windows PowerShell 5.1 uses legacy native argv passing.
    $format = '[{{json .State.Running}},{{if .State.Health}}{{json .State.Health.Status}}{{else}}null{{end}},{{json .Config.Labels}},{{json .State.StartedAt}},{{json .RestartCount}}]'
    $raw = (& docker inspect --format $format $ContainerId | Out-String)
    if ($LASTEXITCODE -ne 0) { return $null }
    $probe = @($raw | ConvertFrom-Json)
    if ($probe.Count -ne 5 -or $probe[0] -isnot [bool] -or $probe[1] -isnot [string] -or
        $probe[2] -isnot [System.Management.Automation.PSCustomObject] -or
        $probe[2].'org.opencontainers.image.revision' -isnot [string] -or
        $probe[2].'io.contextlattice.source-tree' -isnot [string] -or
        $probe[2].'org.opencontainers.image.version' -isnot [string] -or
        [string]::IsNullOrWhiteSpace([string]$probe[3]) -or
        ($probe[4] -isnot [int] -and $probe[4] -isnot [long]) -or $probe[4] -lt 0) { return $null }
    $identity = @($probe[0], $probe[1], $probe[2].'org.opencontainers.image.revision',
        $probe[2].'io.contextlattice.source-tree', $probe[2].'org.opencontainers.image.version',
        $probe[3], $probe[4])
    return ConvertTo-Json -InputObject $identity -Compress
}

function Test-GatewayReadiness {
    Param([string]$ComposeFile, $Metadata)
    try {
        $id = @(& docker compose -f $ComposeFile ps -q gateway-go)
        if ($LASTEXITCODE -ne 0 -or $id.Count -ne 1 -or $id[0] -cnotmatch '^[0-9a-f]{64}$') { return $false }
        $state = Get-GatewayReadinessIdentity -ContainerId $id[0]
        if ([string]::IsNullOrWhiteSpace($state)) { return $false }
        $fields = @($state | ConvertFrom-Json)
        if ($fields.Count -ne 7 -or $fields[0] -ne $true -or $fields[1] -cne 'healthy' -or
            $fields[2] -cne $Metadata.commit -or $fields[3] -cne $Metadata.source_tree -or
            $fields[4] -cne $Metadata.runtime_version -or [string]::IsNullOrWhiteSpace($fields[5]) -or
            $fields[6] -cnotmatch '^[0-9]+$') { return $false }
        $port = & docker exec $id[0] printenv PORT
        if ($LASTEXITCODE -ne 0 -or $port -notmatch '^[0-9]{1,5}$') { return $false }
        $bindings = @(& docker port $id[0] "$port/tcp")
        if ($LASTEXITCODE -ne 0) { return $false }
        $loopback = @($bindings | Where-Object { $_ -match '^(127\.0\.0\.1|0\.0\.0\.0):([0-9]+)$' })
        if ($loopback.Count -ne 1 -or $loopback[0] -notmatch '^(127\.0\.0\.1|0\.0\.0\.0):([0-9]+)$') { return $false }
        $hostPort = $Matches[2]
        $inside = (& docker exec $id[0] wget -qO- -T 3 "http://127.0.0.1:$port/readyz" | Out-String)
        if ($LASTEXITCODE -ne 0) { return $false }
        $outside = (& curl.exe --noproxy '*' -fsS --max-time 3 "http://127.0.0.1:$hostPort/readyz" | Out-String)
        if ($LASTEXITCODE -ne 0) { return $false }
        $a = $inside | ConvertFrom-Json
        $b = $outside | ConvertFrom-Json
        foreach ($response in @($a, $b)) {
            if ($response -isnot [System.Management.Automation.PSCustomObject] -or
                $response.build -isnot [System.Management.Automation.PSCustomObject] -or
                $response.service -isnot [string] -or $response.build.source_commit -isnot [string] -or
                $response.build.source_tree -isnot [string] -or
                $response.ok -isnot [bool] -or $response.ready -isnot [bool] -or
                $response.ok -ne $true -or $response.ready -ne $true -or $response.service -cne 'gateway-go' -or
                $response.build.source_commit -cne $Metadata.commit -or $response.build.source_tree -cne $Metadata.source_tree) { return $false }
            if ($response.build.boot_nonce -isnot [string] -or $response.build.boot_nonce -cnotmatch '\A[0-9a-f]{32}\z') { return $false }
        }
        if ([string]::IsNullOrWhiteSpace($a.build.boot_nonce) -or $a.build.boot_nonce -cne $b.build.boot_nonce) { return $false }
        $again = @(& docker compose -f $ComposeFile ps -q gateway-go)
        if ($LASTEXITCODE -ne 0 -or $again.Count -ne 1 -or $again[0] -cne $id[0]) { return $false }
        $finalState = Get-GatewayReadinessIdentity -ContainerId $id[0]
        if ([string]::IsNullOrWhiteSpace($finalState) -or $finalState -cne $state) { return $false }
        $script:gatewayReadyPort = $hostPort
        return $true
    } catch { return $false }
}

$composeFile = "docker-compose.lite.yml"
if ($FullMode.IsPresent) {
    $composeFile = "docker-compose.yml"
}
Write-Host "Launching stack with $composeFile ..."
$launchIdentity = @{
    "CONTEXTLATTICE_BUILD_VERSION" = [string]$metadata.runtime_version
    "CONTEXTLATTICE_BUILD_CHANNEL" = [string]$metadata.build_channel
    "CONTEXTLATTICE_SOURCE_COMMIT" = [string]$metadata.commit
    "CONTEXTLATTICE_SOURCE_TREE" = [string]$metadata.source_tree
}
$previousLaunchIdentity = @{}
foreach ($setting in $launchIdentity.GetEnumerator()) {
    $previousLaunchIdentity[$setting.Key] = [Environment]::GetEnvironmentVariable($setting.Key, "Process")
    [Environment]::SetEnvironmentVariable($setting.Key, $setting.Value, "Process")
}
try {
    & docker compose -f $composeFile up -d --build
    if ($LASTEXITCODE -ne 0) {
        throw "docker compose failed."
    }
    $ready = $false
    for ($attempt = 0; $attempt -lt 30; $attempt++) {
        if (Test-GatewayReadiness -ComposeFile $composeFile -Metadata $metadata) { $ready = $true; break }
        Start-Sleep -Seconds 2
    }
    if (-not $ready) { throw "Installed gateway readiness was not verified." }
} catch {
    Write-Warning "Launch/readiness failed after file commit; retaining both current install at $InstallDir and transaction at $transactionDir. Runtime state may have changed; automatic rollback is unsafe."
    throw
} finally {
    foreach ($setting in $previousLaunchIdentity.GetEnumerator()) {
        [Environment]::SetEnvironmentVariable($setting.Key, $setting.Value, "Process")
    }
}

Remove-Item -LiteralPath $transactionDir -Recurse -Force -ErrorAction SilentlyContinue
if (Test-Path -LiteralPath "scripts\install_global_agent_tools.ps1") {
    try {
        & powershell -ExecutionPolicy Bypass -File "scripts\install_global_agent_tools.ps1"
    } catch {
        Write-Warning "Global agent tool install failed: $($_.Exception.Message)"
    }
}
Write-Host "Install complete. Gateway API: http://127.0.0.1:$gatewayReadyPort Dashboard: http://127.0.0.1:3000"
try {
    Start-Process "http://127.0.0.1:3000" | Out-Null
} catch {
    Write-Warning "Could not open dashboard URL automatically."
}
