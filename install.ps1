#requires -Version 5.1
<#
.SYNOPSIS
    Installer for monetarium-node + monetarium-wallet + monetarium-ctl (Windows).

.DESCRIPTION
    Fresh install:
    1. Detects CPU architecture and downloads the matching Windows binaries,
       verifying each against its published sha256 digest.
    2. Prompts for the wallet's private passphrase (hidden input).
    3. Writes that passphrase into the wallet's config file so it can
       auto-unlock on start (needed for unattended ticket buying / voting).
    4. Writes .vbs launchers into the user's Startup folder (Windows'
       equivalent of systemd/launchd here, since these binaries don't speak
       the Windows Service Control Manager protocol) and starts node +
       wallet now.
    5. Prints a large, hard-to-miss warning about what this implies.

    Update mode:
    Re-running the script on a machine that already has configs switches to
    update mode automatically, with no flags. It only downloads, verifies and
    swaps the release binaries, then restarts the processes that were running
    and whose binary changed. It never prompts and never touches configs, the
    wallet, the startup scripts or the manifest.

    Functions are split out and the "Main" entry point only runs when this
    file is executed directly -- dot-sourcing it (as the Pester tests do)
    loads the functions without running the installer.

.NOTES
    Must be run from an elevated ("Run as Administrator") PowerShell.
    Windows will not let a running .exe be replaced, so update mode stops the
    affected process first: gracefully through monetarium-ctl when possible,
    otherwise with a warned forced termination.
#>

[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingWriteHost', '')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingPlainTextForPassword', '')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingUsernameAndPasswordParams', '')]
[Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSUseShouldProcessForStateChangingFunctions', '')]
[CmdletBinding()]
param(
    # Used by tests to be extra explicit that Main should not auto-run.
    [switch]$NoExec
)

$ErrorActionPreference = 'Stop'

# --------------------------------------------------------------------------
# Config
# --------------------------------------------------------------------------
$Script:GitHubOrg  = 'monetarium'
$Script:InstallDir = Join-Path $env:ProgramFiles 'Monetarium'

# ProgramData is a hidden system folder. Fall back to LOCALAPPDATA if absent.
$baseData = if (Test-Path $env:ProgramData) { $env:ProgramData } else { $env:LOCALAPPDATA }
$Script:DataDir    = Join-Path $baseData 'Monetarium'
$Script:WalletConf = Join-Path $Script:DataDir 'monetarium-wallet.conf'
$Script:NodeConf   = Join-Path $Script:DataDir 'monetarium.conf'

# ctl config - written to ctl's own appdata dir so "monetarium-ctl" works
# without --configfile on every invocation.
$Script:CtlDir   = Join-Path $env:LOCALAPPDATA 'Monetarium-ctl'
$Script:CtlConf  = Join-Path $Script:CtlDir 'monetarium-ctl.conf'

# Install manifest - lists every file this script creates.
$Script:Manifest = Join-Path $Script:DataDir 'install.manifest'

# Startup folder for auto-start scripts (overridable in tests).
$Script:StartupDir = Join-Path $env:APPDATA 'Microsoft\Windows\Start Menu\Programs\Startup'

# Mining & ticket buyer settings (set by prompts)
$Script:MiningEnabled  = $false
$Script:MiningCores    = $null
$Script:TicketsEnabled = $false
$Script:TicketLimit    = $null
$Script:TicketBalance  = $null
$Script:VotingEnabled  = $false

# Wallet addresses (populated after RPC)
$Script:MiningAddr        = $null
$Script:ConsolidationAddr = $null

# RPC credentials (generated in Main, used by Write-Config and Write-CtlConfig)
$Script:RpcUser   = $null
$Script:RpcSecret = $null

# Update mode: staging directory for verified downloads, and the timings used
# when stopping/starting processes (tests set these to 0 to stay fast).
$Script:StagingDir              = $null
# Matches systemd's default TimeoutStopSec: a node on a slow disk needs more
# than a few seconds to flush before it is killed.
$Script:ServiceStopTimeoutSecs  = 90
$Script:ServiceStartGraceSecs   = 2

# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------
function Write-Info {
    param([string]$Message)
    Write-Host "[install] $Message" -ForegroundColor Cyan
}

function Write-ErrAndThrow {
    param([string]$Message)
    Write-Host "ERROR: $Message" -ForegroundColor Red
    throw $Message
}

function Write-Warn {
    param([string]$Message)
    Write-Host "[install] WARNING: $Message" -ForegroundColor Yellow
}

function Test-IsAdministrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = New-Object Security.Principal.WindowsPrincipal($identity)
    return $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# --------------------------------------------------------------------------
# 1. Detect platform
# --------------------------------------------------------------------------
function Get-Platform {
    $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
        'ARM64'  { 'arm64' }
        'AMD64'  { 'amd64' }
        default  { 'amd64' }
    }
    return "windows-$arch"
}

# --------------------------------------------------------------------------
# 2. Download binaries
# --------------------------------------------------------------------------
function Get-LatestReleaseAsset {
    param(
        [Parameter(Mandatory)][string]$Repo,
        [Parameter(Mandatory)][string]$Platform
    )

    $api = "https://api.github.com/repos/$($Script:GitHubOrg)/$Repo/releases/latest"
    try {
        $release = Invoke-RestMethod -Uri $api -Headers @{ 'User-Agent' = 'monetarium-installer' }
    } catch {
        Write-ErrAndThrow "Could not reach GitHub for '$Repo' release info: $($_.Exception.Message). Nothing was changed."
    }

    $asset = $release.assets | Where-Object { $_.name -like "*$Platform*" } | Select-Object -First 1
    if (-not $asset) {
        Write-ErrAndThrow "Could not find a release asset for '$Repo' matching platform '$Platform'. Check https://github.com/$($Script:GitHubOrg)/$Repo/releases manually."
    }
    return $asset
}

function Get-FileSha256 {
    param([Parameter(Mandatory)][string]$Path)
    return (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
}

function Save-ReleaseBinary {
    param(
        [Parameter(Mandatory)][string]$Repo,
        [Parameter(Mandatory)][string]$BinaryName,
        [Parameter(Mandatory)][string]$Platform,
        [Parameter(Mandatory)][string]$Destination,
        [switch]$RequireDigest
    )

    # Downloads the release asset for $BinaryName and writes a verified copy to
    # $Destination. The download is checked against the asset's published
    # sha256 before the caller ever sees it. With -RequireDigest (update mode)
    # an asset without a published checksum is skipped so the installed binary
    # is kept, matching install.sh. Returns $true when a file was written.
    $asset = Get-LatestReleaseAsset -Repo $Repo -Platform $Platform
    $url = $asset.browser_download_url
    $digest = $asset.digest
    if ($digest) { $digest = ($digest -replace '^sha256:', '').ToLowerInvariant() }

    if (-not $digest -and $RequireDigest) {
        Write-Info "Skipping $BinaryName - release asset has no published checksum; keeping existing binary."
        return $false
    }
    if (-not $digest) {
        # A release that publishes no checksum must not break every fresh
        # install, so warn and rely on transport TLS, exactly like the
        # pre-checksum installer did.
        Write-Warn "No sha256 checksum published for $BinaryName; installing without verification."
    }

    $tmpDir = Join-Path $env:TEMP ([guid]::NewGuid())
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null
    try {
        $archivePath = Join-Path $tmpDir (Split-Path $url -Leaf)
        Write-Info "Downloading $BinaryName from: $url"
        try {
            Invoke-WebRequest -Uri $url -OutFile $archivePath -UseBasicParsing
        } catch {
            Write-ErrAndThrow "Failed to download $BinaryName from ${url}: $($_.Exception.Message)"
        }

        if ($digest) {
            $actual = Get-FileSha256 -Path $archivePath
            if ($actual -ne $digest) {
                Write-ErrAndThrow "Checksum mismatch for ${BinaryName}: expected sha256:$digest, got $actual. Refusing to install a corrupted or tampered download."
            }
            Write-Info "Checksum verified for $BinaryName (sha256:$digest)."
        }

        if ($archivePath -like '*.zip') {
            Expand-Archive -Path $archivePath -DestinationPath $tmpDir -Force
        } else {
            # Raw binary (not archived) - rename to the expected name
            $destName = Join-Path $tmpDir "$BinaryName.exe"
            Move-Item -LiteralPath $archivePath -Destination $destName -Force
        }

        $found = Get-ChildItem -Path $tmpDir -Recurse -Filter "$BinaryName.exe" | Select-Object -First 1
        if (-not $found) {
            $found = Get-ChildItem -Path $tmpDir -Recurse -Filter "$BinaryName-*.exe" | Select-Object -First 1
        }
        if (-not $found) {
            Write-ErrAndThrow "Could not locate '$BinaryName.exe' inside the downloaded archive."
        }

        Copy-Item -LiteralPath $found.FullName -Destination $Destination -Force
        return $true
    } finally {
        Remove-Item -Path $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Install-Binary {
    param(
        [Parameter(Mandatory)][string]$Repo,
        [Parameter(Mandatory)][string]$BinaryName,
        [Parameter(Mandatory)][string]$Platform
    )

    if (-not (Test-Path -LiteralPath $Script:InstallDir)) {
        New-Item -ItemType Directory -Path $Script:InstallDir -Force | Out-Null
    }

    $destination = Join-Path $Script:InstallDir "$BinaryName.exe"
    if (Test-Path -LiteralPath $destination) {
        Write-Info "Skipping $BinaryName - already installed at $destination"
        return
    }

    Write-Info "Fetching latest release info for $Repo..."
    $null = Save-ReleaseBinary -Repo $Repo -BinaryName $BinaryName -Platform $Platform -Destination $destination

    Write-Info "Installed $BinaryName -> $destination"
}

# --------------------------------------------------------------------------
# 2b. Update mode: re-running on an existing install
# --------------------------------------------------------------------------
# Re-running this script on an installed node must behave as an updater, not a
# reinstall. Windows refuses to replace a running .exe, so update mode stops
# the affected process, swaps the binary and starts it again. It never touches
# configs, the wallet, the startup scripts or the manifest, and it never asks
# for the passphrase.

function Test-InteractiveHost {
    try {
        return -not [Console]::IsInputRedirected
    } catch {
        return $true
    }
}

function Read-InstallModeChoice {
    param([Parameter(Mandatory)][string]$Reason)

    if (-not (Test-InteractiveHost)) {
        Write-ErrAndThrow "$Reason Cannot choose between install and update non-interactively. Run this script from a terminal, or remove the leftover files to force a clean install."
    }

    Write-Warn $Reason
    $answer = Read-Host -Prompt 'Install or update? [1] Update binaries only [2] Full install'
    switch -Regex ($answer) {
        '^[2fF]' { return 'Install' }
        default  { return 'Update' }
    }
}

function Get-InstallMode {
    # Returns 'Install' or 'Update'. A fresh install is chosen ONLY when
    # nothing at all is on disk; anything else is an existing installation and
    # asks, because a full install rewrites the configs and startup scripts.
    $nodeConf = Test-Path -LiteralPath $Script:NodeConf
    $walletConf = Test-Path -LiteralPath $Script:WalletConf
    $configs = $nodeConf -and $walletConf

    $knownBinaries = @('monetarium-node', 'monetarium-wallet', 'monetarium-ctl')
    $installedCount = 0
    foreach ($name in $knownBinaries) {
        if (Test-Path -LiteralPath (Join-Path $Script:InstallDir "$name.exe")) { $installedCount++ }
    }

    $startupScripts = (Test-Path -LiteralPath (Join-Path $Script:StartupDir 'MonetariumNode.vbs')) -or
                      (Test-Path -LiteralPath (Join-Path $Script:StartupDir 'MonetariumWallet.vbs'))
    $walletDb = Test-Path -LiteralPath (Join-Path (Get-WalletDataDir) 'mainnet\wallet.db')
    $manifest = Test-Path -LiteralPath $Script:Manifest

    if ($configs) { return 'Update' }

    # A fresh install is chosen ONLY when nothing at all is on disk. Anything
    # else is an existing installation: a full install rewrites the configs
    # and the startup scripts, which is the loss update mode exists to avoid.
    if (-not $nodeConf -and -not $walletConf -and -not $startupScripts -and
        $installedCount -eq 0 -and -not $walletDb -and -not $manifest) {
        return 'Install'
    }

    if ($nodeConf -and -not $walletConf) {
        return Read-InstallModeChoice -Reason 'A node config exists but the wallet config is missing.'
    }
    if ($walletConf -and -not $nodeConf) {
        return Read-InstallModeChoice -Reason 'A wallet config exists but the node config is missing.'
    }
    if ($startupScripts) {
        return Read-InstallModeChoice -Reason 'Existing startup scripts found, but no config files.'
    }
    return Read-InstallModeChoice -Reason 'Existing Monetarium binaries or wallet data found, but no config files or startup scripts.'
}

function Get-MonetariumProcess {
    param([Parameter(Mandatory)][string]$BinaryName)

    # Match on the executable path so a copy run from another directory is
    # never touched. If the path cannot be read, fall back to the process name
    # (this script only ever installs into $Script:InstallDir).
    $exe = Join-Path $Script:InstallDir "$BinaryName.exe"
    $processes = @(Get-Process -Name $BinaryName -ErrorAction SilentlyContinue)
    foreach ($process in $processes) {
        $path = $null
        try {
            $path = $process.Path
        } catch {
            # Access denied or the process already exited: fall back to the
            # name match below.
            $path = $null
        }
        if (-not $path -or $path -ieq $exe) { return $process }
    }
    return $null
}

function Test-StaleProcess {
    param([Parameter(Mandatory)][string]$BinaryName)

    # A previous run can swap the binary while the old process keeps running:
    # the hashes then match but the process still uses the old image, so a
    # plain "already current" check would leave it running stale forever.
    $process = Get-MonetariumProcess -BinaryName $BinaryName
    if (-not $process) { return $false }
    $exe = Join-Path $Script:InstallDir "$BinaryName.exe"
    if (-not (Test-Path -LiteralPath $exe)) { return $false }
    try {
        return ($process.StartTime -lt (Get-Item -LiteralPath $exe).LastWriteTime)
    } catch {
        return $false
    }
}

function Test-BinaryVersion {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$BinaryName
    )

    try {
        $null = & $Path --version 2>&1
        return ($LASTEXITCODE -eq 0)
    } catch {
        Write-Info "--version smoke test for $BinaryName failed: $($_.Exception.Message)"
        return $false
    }
}

function Test-ProcessExited {
    param([Parameter(Mandatory)]$Process)

    # Refresh so HasExited reports the current state instead of a cached one.
    if ($Process -is [System.Diagnostics.Process]) { $Process.Refresh() }
    return [bool]$Process.HasExited
}

function Request-GracefulStop {
    param([Parameter(Mandatory)][string]$BinaryName)

    # monetarium-ctl is a generic RPC client; "stop" shuts the node down and
    # "--wallet stop" does the same for the wallet. Its config is the one this
    # installer writes, so no credentials are passed here.
    $ctlExe = Join-Path $Script:InstallDir 'monetarium-ctl.exe'
    if (-not (Test-Path -LiteralPath $ctlExe)) { return }

    $stopArgs = @('stop')
    if ($BinaryName -eq 'monetarium-wallet') { $stopArgs = @('--wallet', 'stop') }
    try {
        & $ctlExe @stopArgs *> $null
    } catch {
        Write-Info "Graceful stop request for $BinaryName failed: $($_.Exception.Message)"
    }
}

function Stop-MonetariumProcess {
    param([Parameter(Mandatory)][string]$BinaryName)

    # Returns $true when a running process was stopped and $false when it was
    # not running. A forced kill is a warned last resort: TerminateProcess
    # gives the node no chance to flush, so it is used only when the graceful
    # RPC stop did not take.
    $process = Get-MonetariumProcess -BinaryName $BinaryName
    if (-not $process) { return $false }

    Request-GracefulStop -BinaryName $BinaryName
    for ($i = 0; $i -lt ($Script:ServiceStopTimeoutSecs * 2); $i++) {
        if (Test-ProcessExited -Process $process) { break }
        Start-Sleep -Milliseconds 500
    }

    if (-not (Test-ProcessExited -Process $process)) {
        Write-Warn "$BinaryName did not stop gracefully; terminating it. Unsaved in-memory state is lost."
        Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
        for ($i = 0; $i -lt ($Script:ServiceStopTimeoutSecs * 2); $i++) {
            if (Test-ProcessExited -Process $process) { break }
            Start-Sleep -Milliseconds 500
        }
    }

    if (-not (Test-ProcessExited -Process $process)) {
        Write-ErrAndThrow "Could not stop $BinaryName (pid $($process.Id)). Nothing was replaced."
    }

    Write-Info "Stopped $BinaryName."
    return $true
}

function Install-StagedBinary {
    param(
        [Parameter(Mandatory)][string]$BinaryName,
        [Parameter(Mandatory)][string]$StagedPath
    )

    # Rename the old binary aside, copy the verified one in, then re-check the
    # installed hash. Any failure puts the previous binary back.
    $destination = Join-Path $Script:InstallDir "$BinaryName.exe"
    $backup = "$destination.old"
    $hadPrevious = Test-Path -LiteralPath $destination
    Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue

    $restored = $false
    try {
        if ($hadPrevious) {
            Move-Item -LiteralPath $destination -Destination $backup -Force
        }
        Copy-Item -LiteralPath $StagedPath -Destination $destination -Force
        if ((Get-FileSha256 -Path $destination) -ne (Get-FileSha256 -Path $StagedPath)) {
            throw 'The installed file does not match the verified download.'
        }
    } catch {
        $failure = $_
        if ($hadPrevious -and (Test-Path -LiteralPath $backup)) {
            try {
                Move-Item -LiteralPath $backup -Destination $destination -Force
                $restored = $true
            } catch {
                # Leave $restored false so the caller reports the backup path.
                $restored = $false
            }
        }
        if ($restored) {
            Write-ErrAndThrow "Could not install ${BinaryName} into $($Script:InstallDir): $failure The previous binary was restored."
        }
        Write-ErrAndThrow "Could not install ${BinaryName} into $($Script:InstallDir): $failure Restore it manually from $backup."
    }

    if ($hadPrevious) { Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue }
    Write-Info "Installed $BinaryName -> $destination"
}

function Start-MonetariumProcess {
    param([Parameter(Mandatory)][string]$BinaryName)

    $exe = Join-Path $Script:InstallDir "$BinaryName.exe"
    $conf = if ($BinaryName -eq 'monetarium-node') { $Script:NodeConf } else { $Script:WalletConf }

    $process = Start-Process -FilePath $exe -ArgumentList "--configfile=`"$conf`"" -PassThru -WindowStyle Hidden
    if ($Script:ServiceStartGraceSecs -gt 0) { Start-Sleep -Seconds $Script:ServiceStartGraceSecs }
    if (Test-ProcessExited -Process $process) {
        Write-ErrAndThrow "$BinaryName exited immediately after restart. Check the config at $conf and run the binary manually to see the error."
    }

    Write-Info "Restarted $BinaryName."
}

function Invoke-Update {
    param([Parameter(Mandatory)][string]$Platform)

    $binaries = @('monetarium-node', 'monetarium-wallet', 'monetarium-ctl')

    # Capture the pre-update state first: after the swap a process is either
    # running or not, and the restart decision must reflect how it started.
    $nodeWasRunning = $null -ne (Get-MonetariumProcess -BinaryName 'monetarium-node')
    $walletWasRunning = $null -ne (Get-MonetariumProcess -BinaryName 'monetarium-wallet')

    Write-Info 'Existing installation detected - updating binaries only.'
    Write-Info 'Configs, wallet, startup scripts and settings are left untouched.'

    $stopped = @()
    $Script:StagingDir = Join-Path $env:TEMP ("monetarium-update-" + [guid]::NewGuid())
    New-Item -ItemType Directory -Path $Script:StagingDir -Force | Out-Null
    try {
        # Stage and verify everything before a single installed binary is
        # touched, so one failing repo aborts with nothing changed.
        $staged = @{}
        foreach ($name in $binaries) {
            $stagedPath = Join-Path $Script:StagingDir "$name.exe"
            $written = Save-ReleaseBinary -Repo $name -BinaryName $name -Platform $Platform `
                -Destination $stagedPath -RequireDigest
            if (-not $written) { continue }
            if (-not (Test-BinaryVersion -Path $stagedPath -BinaryName $name)) {
                Write-ErrAndThrow "Downloaded $name failed its --version smoke test. Refusing to install it; nothing was changed."
            }
            $staged[$name] = $stagedPath
            Write-Info "Staged $name."
        }

        $changed = @()
        foreach ($name in $staged.Keys) {
            $destination = Join-Path $Script:InstallDir "$name.exe"
            if (-not (Test-Path -LiteralPath $destination)) {
                $changed += $name
                Write-Info "Repairing missing $name."
                continue
            }
            if ((Get-FileSha256 -Path $destination) -ne (Get-FileSha256 -Path $staged[$name])) {
                $changed += $name
                Write-Info "Update available for $name (installed hash differs from release asset)."
            } else {
                Write-Info "Skipping $name - already current at $destination"
            }
        }

        # A stale process (started before its binary was written) must be
        # restarted even when the hashes match, or it keeps running the old
        # image forever. Checked before the "nothing to do" early return.
        $restartNode = $nodeWasRunning -and (
            ($changed -contains 'monetarium-node') -or (Test-StaleProcess -BinaryName 'monetarium-node'))
        $restartWallet = $walletWasRunning -and (
            ($changed -contains 'monetarium-wallet') -or (Test-StaleProcess -BinaryName 'monetarium-wallet'))

        if ($changed.Count -eq 0 -and -not $restartNode -and -not $restartWallet) {
            Write-Info 'All binaries are already current - nothing to do.'
            return
        }

        # Stop the wallet before the node; restart the node before the wallet
        # so the wallet finds its RPC endpoint on the way up.
        if ($restartWallet) {
            $null = Stop-MonetariumProcess -BinaryName 'monetarium-wallet'
            $stopped += 'monetarium-wallet'
        }
        if ($restartNode) {
            $null = Stop-MonetariumProcess -BinaryName 'monetarium-node'
            $stopped += 'monetarium-node'
        }

        foreach ($name in $changed) {
            Install-StagedBinary -BinaryName $name -StagedPath $staged[$name]
        }

        if ($restartNode) { Start-MonetariumProcess -BinaryName 'monetarium-node' }
        if ($restartWallet) { Start-MonetariumProcess -BinaryName 'monetarium-wallet' }

        if ($changed.Count -gt 0) {
            Write-Info "Replaced: $($changed -join ', ')"
        }
    } finally {
        # Whatever this run stopped must be running again when it exits, even
        # if the swap or a start threw. Otherwise a failed update leaves the
        # node down, and the next run sees it stopped and never starts it.
        foreach ($name in $stopped) {
            if (Get-MonetariumProcess -BinaryName $name) { continue }
            try {
                Start-MonetariumProcess -BinaryName $name
            } catch {
                Write-Warn "$name is not running and could not be restarted: $($_.Exception.Message)"
            }
        }
        if ($Script:StagingDir -and (Test-Path -LiteralPath $Script:StagingDir)) {
            Remove-Item -LiteralPath $Script:StagingDir -Recurse -Force -ErrorAction SilentlyContinue
        }
        $Script:StagingDir = $null
    }
}

function Add-InstallDirToPath {
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if ($machinePath -notlike "*$($Script:InstallDir)*") {
        [Environment]::SetEnvironmentVariable('Path', "$machinePath;$($Script:InstallDir)", 'Machine')
        Write-Info "Added $($Script:InstallDir) to the system PATH (open a new shell to pick it up)."
    }
    if ($env:Path -notlike "*$($Script:InstallDir)*") {
        $env:Path = "$env:Path;$($Script:InstallDir)"
    }
}

# --------------------------------------------------------------------------
# 3. Prompt for passphrase (hidden)
# --------------------------------------------------------------------------
function ConvertFrom-SecureStringToPlainText {
    param([Parameter(Mandatory)][System.Security.SecureString]$SecureString)

    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($SecureString)
    try {
        return [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
    }
    finally {
        [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
    }
}

function Read-WalletPassphrase {
    while ($true) {
        $secure1 = Read-Host -Prompt 'Enter wallet private passphrase' -AsSecureString
        $secure2 = Read-Host -Prompt 'Confirm passphrase' -AsSecureString

        $plain1 = ConvertFrom-SecureStringToPlainText -SecureString $secure1
        $plain2 = ConvertFrom-SecureStringToPlainText -SecureString $secure2

        if ([string]::IsNullOrEmpty($plain1)) {
            Write-Host 'Passphrase cannot be empty. Try again.' -ForegroundColor Red
            continue
        }
        if ($plain1 -ne $plain2) {
            Write-Host 'Passphrases did not match. Try again.' -ForegroundColor Red
            continue
        }
        return $plain1
    }
}

function Get-RandomToken {
    param([int]$Length = 32)
    # Over-allocate raw bytes since stripping non-alphanumeric base64
    # characters (+, /, =) shortens the string unpredictably.
    $bytes = New-Object byte[] ($Length * 3)
    [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $alnum = [Convert]::ToBase64String($bytes) -replace '[^a-zA-Z0-9]', ''
    if ($alnum.Length -lt $Length) {
        Write-ErrAndThrow 'Failed to generate a sufficiently long random token.'
    }
    return $alnum.Substring(0, $Length)
}

# --------------------------------------------------------------------------
# 4. Write config files
# --------------------------------------------------------------------------
function Protect-ConfigFile {
    param([Parameter(Mandatory)][string]$Path)

    $acl = Get-Acl -Path $Path
    $acl.SetAccessRuleProtection($true, $false)  # disable inheritance, drop inherited rules

    foreach ($identity in @($env:USERNAME, 'SYSTEM', 'Administrators')) {
        $rule = New-Object Security.AccessControl.FileSystemAccessRule(
            $identity, 'FullControl', 'Allow'
        )
        $acl.AddAccessRule($rule)
    }
    Set-Acl -Path $Path -AclObject $acl
}

function Write-Config {
    param(
        [Parameter(Mandatory)][string]$Passphrase,
        [Parameter(Mandatory)][string]$RpcUser,
        [Parameter(Mandatory)][string]$RpcSecret
    )

    New-Item -ItemType Directory -Path $Script:DataDir -Force | Out-Null
    $timestamp = (Get-Date).ToUniversalTime().ToString('o')

    @"
; monetarium.conf - generated by install.ps1 on $timestamp
rpcuser=$RpcUser
rpcpass=$RpcSecret
"@ | Write-ConfText $Script:NodeConf

    $tbEnabled = 0;   if ($Script:TicketsEnabled) { $tbEnabled = 1 }
    $voteEnabled = 0; if ($Script:VotingEnabled)  { $voteEnabled = 1 }
    $tktLimit   = if ($Script:TicketLimit -ne $null)   { $Script:TicketLimit }   else { 1 }
    $tktBalance = if ($Script:TicketBalance -ne $null) { $Script:TicketBalance } else { 1 }

    @"
; monetarium-wallet.conf - generated by install.ps1 on $timestamp
; WARNING: 'pass' below stores the wallet unlock passphrase in plaintext
; so the wallet can auto-unlock at startup for
; unattended ticket buying / voting. See the warning printed by install.ps1.
username=$RpcUser
password=$RpcSecret
pass=$Passphrase
enablevoting=$voteEnabled
enableticketbuyer=$tbEnabled
ticketbuyer.limit=$tktLimit
ticketbuyer.balancetomaintainabsolute=$tktBalance
; Gap limit for address discovery
gaplimit=20
accountgaplimit=10
"@ | Write-ConfText $Script:WalletConf

    Protect-ConfigFile -Path $Script:NodeConf
    Protect-ConfigFile -Path $Script:WalletConf

    Write-Info "Config files written to $($Script:DataDir)."
    Write-Info "  (ProgramData is a hidden system folder - type the path directly into File Explorer or enable 'Show hidden files'.)"
    Write-Info "Access restricted to the current user, SYSTEM, and Administrators."
}

function Write-CtlConfig {
    New-Item -ItemType Directory -Path $Script:CtlDir -Force | Out-Null
    $timestamp = (Get-Date).ToUniversalTime().ToString('o')

    @"
; monetarium-ctl.conf - generated by install.ps1 on $timestamp
; RPC credentials matching the node config.
rpcuser=$($Script:RpcUser)
rpcpass=$($Script:RpcSecret)
"@ | Write-ConfText $Script:CtlConf

    Protect-ConfigFile -Path $Script:CtlConf
    Write-Info "ctl config written to $($Script:CtlConf)"
}

# --------------------------------------------------------------------------
# 5. Create wallet
# --------------------------------------------------------------------------
function Get-WalletDataDir {
    $localAppData = if ($env:LOCALAPPDATA) { $env:LOCALAPPDATA } else { $env:APPDATA }
    return Join-Path $localAppData 'Monetarium-wallet'
}

function Create-Wallet {
    $walletDb = Join-Path (Get-WalletDataDir) 'mainnet\wallet.db'

    if (Test-Path $walletDb) {
        Write-Info "Wallet database already exists at $walletDb - skipping creation."
        return
    }

    $walletExe = Join-Path $Script:InstallDir 'monetarium-wallet.exe'

    Write-Info "Creating wallet. Follow the interactive prompts."
    Write-Info "When asked, type 'yes' to use the passphrase you just entered."

    & $walletExe --create --configfile "$Script:WalletConf" 2>&1

    if (-not (Test-Path $walletDb)) {
        Write-ErrAndThrow "Wallet creation failed - database not found at $walletDb"
    }

    Write-Info 'Wallet created successfully.'
}

# --------------------------------------------------------------------------
# 6. Startup scripts (Windows Startup folder — no Task Scheduler needed)
# --------------------------------------------------------------------------
function Install-StartupScripts {
    $nodeExe   = Join-Path $Script:InstallDir 'monetarium-node.exe'
    $walletExe = Join-Path $Script:InstallDir 'monetarium-wallet.exe'

    if (-not (Test-Path $Script:StartupDir)) {
        New-Item -ItemType Directory -Path $Script:StartupDir -Force | Out-Null
    }

    # VBS launcher — runs the binary with ZERO window visibility.
    @"
Set WshShell = CreateObject("WScript.Shell")
WshShell.Run "$($nodeExe) --configfile=`"$($Script:NodeConf)`"", 0, False
"@ | Write-ConfText (Join-Path $Script:StartupDir 'MonetariumNode.vbs')

    @"
Set WshShell = CreateObject("WScript.Shell")
WshShell.Run "cmd.exe /c timeout /t 15 /nobreak >nul && ""$walletExe"" --configfile=""$($Script:WalletConf)""", 0, False
"@ | Write-ConfText (Join-Path $Script:StartupDir 'MonetariumWallet.vbs')

    Write-Info "Startup scripts written to $Script:StartupDir"
    Write-Info "  MonetariumNode.vbs, MonetariumWallet.vbs"

    # Start both processes now.
    Start-Process -WindowStyle Hidden -FilePath $nodeExe `
        -ArgumentList "--configfile=`"$($Script:NodeConf)`""
    Write-Info 'Started monetarium-node.'

    Start-Sleep -Seconds 2
    Start-Process -WindowStyle Hidden -FilePath $walletExe `
        -ArgumentList "--configfile=`"$($Script:WalletConf)`""
    Write-Info 'Started monetarium-wallet.'
}

# --------------------------------------------------------------------------
# 7. Prompt for CPU mining
# --------------------------------------------------------------------------
function Read-MiningPrompt {
    $answer = Read-Host -Prompt 'Enable CPU mining? (y/N)'
    switch -Regex ($answer) {
        '^[yY]([eE][sS])?$' { $Script:MiningEnabled = $true }
    }

    if (-not $Script:MiningEnabled) { return }

    $cpuCount = [Environment]::ProcessorCount
    $recommended = [Math]::Max(1, [Math]::Floor($cpuCount / 2))

    $answer = Read-Host -Prompt "You have ${cpuCount} CPU core(s). How many cores to use for mining? (recommended: ${recommended})"
    $Script:MiningCores = if ($answer -ne '') { $answer } else { $recommended }
    while ($true) {
        if ($Script:MiningCores -match '^\d+$') { break }
        $answer = Read-Host -Prompt 'Please enter a valid number'
        $Script:MiningCores = if ($answer -ne '') { $answer } else { $recommended }
    }
}

# --------------------------------------------------------------------------
# 8. Prompt for ticket buyer
# --------------------------------------------------------------------------
function Read-TicketBuyerPrompt {
    $answer = Read-Host -Prompt 'Enable automatic ticket purchase for staking? (y/N)'
    switch -Regex ($answer) {
        '^[yY]([eE][sS])?$' { $Script:TicketsEnabled = $true }
    }

    if ($Script:TicketsEnabled) {
        $answer = Read-Host -Prompt 'Maximum number of tickets per purchase (ticketbuyer.limit)'
        $Script:TicketLimit = if ($answer -ne '') { $answer } else { 5 }
        while ($true) {
            if ($Script:TicketLimit -match '^\d+$') { break }
            $answer = Read-Host -Prompt 'Please enter a valid number'
            $Script:TicketLimit = $answer
        }

        $answer = Read-Host -Prompt 'Minimum VAR balance to keep in wallet (ticketbuyer.balancetomaintainabsolute)'
        $Script:TicketBalance = if ($answer -ne '') { $answer } else { 0 }
        while ($true) {
            if ($Script:TicketBalance -match '^\d+(\.\d+)?$') { break }
            $answer = Read-Host -Prompt 'Please enter a valid number'
            $Script:TicketBalance = $answer
        }
    }

    $answer = Read-Host -Prompt 'Enable voting? (y/N)'
    switch -Regex ($answer) {
        '^[yY]([eE][sS])?$' { $Script:VotingEnabled = $true }
    }
}

# --------------------------------------------------------------------------
# 9. Post-install: configure mining & ticket buyer via RPC
# --------------------------------------------------------------------------

# Write-ConfText writes configuration and launcher files as BOM-less UTF-8.
# Set-Content -Encoding ASCII replaces non-ASCII characters with '?', which
# for a wallet passphrase or a user profile path means the file no longer
# matches what the user entered.  UTF-8 without a BOM is byte-identical for
# ASCII-only inputs, so nothing else changes.
function Write-ConfText {
    param(
        [Parameter(Mandatory = $true, Position = 0)] [string] $Path,
        [Parameter(Mandatory = $true, Position = 1, ValueFromPipeline = $true)] [string] $Text,
        [switch] $Append
    )
    process {
        $utf8NoBom = [System.Text.UTF8Encoding]::new($false)
        if ($Append) {
            [System.IO.File]::AppendAllText($Path, $Text + "`r`n", $utf8NoBom)
        } else {
            [System.IO.File]::WriteAllText($Path, $Text + "`r`n", $utf8NoBom)
        }
    }
}

function Invoke-ConfigureMiningAndVoting {
    # generate stays false until a mining address actually exists: the node
    # refuses to load a configuration with generate=true and no miningaddr,
    # so writing it up front would leave the node unable to start on any
    # wallet-RPC failure below.
    @"

; Mining configuration - added post-install by install.ps1
generate=false
"@ | Write-ConfText $Script:NodeConf

    if (-not $Script:MiningEnabled -and -not $Script:TicketsEnabled) { return }

    $ctlExe = Join-Path $Script:InstallDir 'monetarium-ctl.exe'
    $walletExe = Join-Path $Script:InstallDir 'monetarium-wallet.exe'
    $rpcCert = Join-Path (Get-WalletDataDir) 'rpc.cert'
    $configChanged = $false
    $proc = $null
    $rpcReady = $false

    # Check if wallet is already running (previous install re-run).
    try {
        $null = & $ctlExe --wallet getinfo 2>$null
        if ($LASTEXITCODE -eq 0) {
            $rpcReady = $true
            Write-Info 'Wallet already running.'
        }
    } catch {
        Write-Info 'Starting wallet for RPC configuration...'
        try {
            $proc = Start-Process -FilePath $walletExe `
                -ArgumentList "--configfile=`"$($Script:WalletConf)`"" `
                -PassThru -WindowStyle Hidden -ErrorAction Stop
        } catch {
            Write-Info 'Failed to start wallet process. Configure addresses manually.'
            return
        }
        if (-not $proc) { return }
    }

    if (-not $rpcReady) {
        Write-Info 'Waiting for wallet RPC to become available...'
        for ($i = 0; $i -lt 60; $i++) {
            if ($proc.HasExited) { break }
            try {
                $null = & $ctlExe --wallet getinfo 2>$null
                if ($LASTEXITCODE -eq 0) {
                    $rpcReady = $true
                    break
                }
            } catch { }
            Start-Sleep -Seconds 2
        }
    }

    if (-not $rpcReady) {
        Write-Info 'Wallet RPC not ready after 60 seconds.'
        Write-Info 'Configure mining/ticket addresses manually after the node syncs:'
        if ($Script:MiningEnabled) {
            Write-Info "  Edit $($Script:NodeConf): add generate=true and miningaddr=<address>"
        }
        if ($Script:TicketsEnabled) {
            Write-Info "  After startup: $ctlExe --wallet setvotefeeconsolidationaddress default <address>"
        }
        if ($proc -and -not $proc.HasExited) { $proc.Kill() }
        return
    }

    Write-Host 'Wallet RPC ready.' -ForegroundColor Green

    if ($Script:MiningEnabled) {
        $miningAddr = & $ctlExe --wallet getnewaddress 2>$null
        if ($miningAddr) {
            $Script:MiningAddr = $miningAddr
            "generate=true" | Write-ConfText $Script:NodeConf -Append
            "miningaddr=${miningAddr}" | Write-ConfText $Script:NodeConf -Append
            $configChanged = $true
        }
    } else {
        $Script:MiningAddr = & $ctlExe --wallet getnewaddress 2>$null
        if ($Script:MiningAddr) {
            "miningaddr=$($Script:MiningAddr)" | Add-Content -Path $Script:NodeConf -Encoding ASCII
        }
    }

    # Generate consolidation address always and set it on the wallet.
    $consolidationAddr = & $ctlExe --wallet getnewaddress 2>$null
    if ($consolidationAddr) {
        $Script:ConsolidationAddr = $consolidationAddr
        & $ctlExe --wallet setvotefeeconsolidationaddress "default" $consolidationAddr 2>$null | Out-Null
    }

    if ($proc) {
        Write-Info 'Stopping wallet...'
        & $ctlExe --wallet stop *>$null
        Start-Sleep -Seconds 3
        if (-not $proc.HasExited) { $proc.Kill() }
    }

    Show-ConfigurationSummary
}

# --------------------------------------------------------------------------
# 9b. Configuration Summary display
# --------------------------------------------------------------------------
function Show-ConfigurationSummary {
    Write-Host ''
    Write-Host '=============================================' -ForegroundColor Green
    Write-Host '  Configuration Summary' -ForegroundColor Green
    Write-Host '=============================================' -ForegroundColor Green
    if ($Script:MiningAddr) {
        Write-Host "Mining address:              $($Script:MiningAddr)"
    }
    if ($Script:MiningCores) {
        Write-Host "CPU cores for mining:        $($Script:MiningCores)"
    }
    if ($Script:TicketsEnabled) {
        Write-Host "Ticket buyer limit:          $($Script:TicketLimit)"
        Write-Host "Balance to maintain:         $($Script:TicketBalance)"
    }
    if ($Script:ConsolidationAddr) {
        Write-Host "Fee consolidation address:   $($Script:ConsolidationAddr)"
    }
    if ($Script:VotingEnabled) {
        Write-Host "Auto voting:                 enabled"
    } else {
        Write-Host "Auto voting:                 disabled"
    }
    Write-Host ''
}

# --------------------------------------------------------------------------
# 10. Install manifest
# --------------------------------------------------------------------------
function Write-Manifest {
    $walletDb = Join-Path (Get-WalletDataDir) 'mainnet\wallet.db'
    $timestamp = (Get-Date).ToUniversalTime().ToString('o')

    @"
# Install manifest - files created by install.ps1
# Run date: $timestamp
#
# To uninstall, stop the processes first:
#   taskkill /f /im monetarium-node.exe
#   taskkill /f /im monetarium-wallet.exe
# Then remove %APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\Monetarium*.vbs
#
# Then remove the files listed below.
# Backup and remove the wallet database FIRST if it holds funds!
# The wallet seed was shown during installation - without it the
# wallet.db is unrecoverable.

binaries:
  $(Join-Path $Script:InstallDir 'monetarium-node.exe')
  $(Join-Path $Script:InstallDir 'monetarium-wallet.exe')
  $(Join-Path $Script:InstallDir 'monetarium-ctl.exe')

configs:
  $($Script:NodeConf)
  $($Script:WalletConf)
  $($Script:CtlConf)

wallet database (back up seed before removing):
  ${walletDb}

data directories:
  $($Script:DataDir)
  $($Script:CtlDir)

"@ | Set-Content -Path $Script:Manifest -Encoding ASCII

    if ($Script:MiningEnabled -and $Script:MiningAddr) {
        @"

mining:
  enabled:   true
  cores:     $($Script:MiningCores)
  address:   $($Script:MiningAddr)

"@ | Add-Content -Path $Script:Manifest -Encoding ASCII
    }

    if ($Script:TicketsEnabled) {
        @"

ticket buyer:
  enabled:                  true
  limit:                    $($Script:TicketLimit)
  balance to maintain:      $($Script:TicketBalance)

"@ | Add-Content -Path $Script:Manifest -Encoding ASCII
        if ($Script:ConsolidationAddr) {
            "  fee consolidation addr:   $($Script:ConsolidationAddr)" | Add-Content -Path $Script:Manifest -Encoding ASCII
        }
        "" | Add-Content -Path $Script:Manifest -Encoding ASCII
    }

    @"

startup scripts:
  $(Join-Path $Script:StartupDir 'MonetariumNode.vbs')
  $(Join-Path $Script:StartupDir 'MonetariumWallet.vbs')

This manifest: $($Script:Manifest)
"@ | Add-Content -Path $Script:Manifest -Encoding ASCII

    Write-Info "Install manifest written to $($Script:Manifest)"
}

# --------------------------------------------------------------------------
# 11. Big warning
# --------------------------------------------------------------------------
function Show-BigWarning {
    $bar = '=' * 70
    Write-Host ''
    Write-Host $bar -ForegroundColor Red
    Write-Host '                       !!!  WARNING  !!!' -ForegroundColor Red
    Write-Host $bar -ForegroundColor Red
    Write-Host " This script has written your wallet's PRIVATE PASSPHRASE in" -ForegroundColor Red
    Write-Host ' PLAINTEXT to:' -ForegroundColor Red
    Write-Host "     $($Script:WalletConf)" -ForegroundColor Red
    Write-Host ''
    Write-Host ' This is required so the wallet can auto-unlock and buy' -ForegroundColor Red
    Write-Host ' tickets / vote WITHOUT any further action from you. It also' -ForegroundColor Red
    Write-Host ' means:' -ForegroundColor Red
    Write-Host '   - Anyone who can read that file (an admin account, a backup,' -ForegroundColor Red
    Write-Host '     a compromised process, malware running as your user) can' -ForegroundColor Red
    Write-Host '     unlock your wallet and move your funds.' -ForegroundColor Red
    Write-Host '   - This machine is now effectively a HOT WALLET that stays' -ForegroundColor Red
    Write-Host '     unlocked 24/7. There is no PIN, 2FA, or manual approval' -ForegroundColor Red
    Write-Host '     step standing between an attacker and your coins.' -ForegroundColor Red
    Write-Host '   - If this machine is compromised, funds can be drained' -ForegroundColor Red
    Write-Host '     automatically, with no prompt and no warning.' -ForegroundColor Red
    Write-Host ''
    Write-Host ' Recommended precautions:' -ForegroundColor Red
    Write-Host '   - Only put funds on this wallet that you can afford to' -ForegroundColor Red
    Write-Host '     lose, sized for ticket-buying/voting purposes only.' -ForegroundColor Red
    Write-Host '   - Keep the bulk of your holdings in a separate, offline' -ForegroundColor Red
    Write-Host '     or hardware-secured wallet, not this one.' -ForegroundColor Red
    Write-Host '   - Keep Windows Update, antivirus, and firewall active and' -ForegroundColor Red
    Write-Host '     restrict who has admin/RDP access to this machine.' -ForegroundColor Red
    Write-Host '   - Rotate the passphrase and re-run this script if you' -ForegroundColor Red
    Write-Host '     ever suspect the machine has been compromised.' -ForegroundColor Red
    Write-Host $bar -ForegroundColor Red
}

# --------------------------------------------------------------------------
# Main
# --------------------------------------------------------------------------
function Invoke-Install {
    param([Parameter(Mandatory)][string]$Platform)

    Install-Binary -Repo 'monetarium-node'   -BinaryName 'monetarium-node'   -Platform $Platform
    Install-Binary -Repo 'monetarium-wallet' -BinaryName 'monetarium-wallet' -Platform $Platform
    Install-Binary -Repo 'monetarium-ctl'    -BinaryName 'monetarium-ctl'    -Platform $Platform

    Add-InstallDirToPath

    $passphrase = Read-WalletPassphrase
    $Script:RpcUser = 'monetarium'
    $Script:RpcSecret = Get-RandomToken -Length 32

    Read-TicketBuyerPrompt

    Write-Config -Passphrase $passphrase -RpcUser $Script:RpcUser -RpcSecret $Script:RpcSecret
    Remove-Variable -Name passphrase -ErrorAction SilentlyContinue

    Write-CtlConfig
    Create-Wallet

    Read-MiningPrompt

    Invoke-ConfigureMiningAndVoting
    Install-StartupScripts
    Write-Manifest

    Write-Host ''
    Write-Host 'Installation complete.' -ForegroundColor Green
    Show-BigWarning
}

function Main {
    if (-not (Test-IsAdministrator)) {
        Write-ErrAndThrow 'This script must be run from an elevated PowerShell (Run as Administrator).'
    }

    $platform = Get-Platform
    Write-Info "Detected platform: $platform"

    if ((Get-InstallMode) -eq 'Update') {
        Invoke-Update -Platform $platform
        Write-Host ''
        Write-Host 'Update complete.' -ForegroundColor Green
        return
    }

    Invoke-Install -Platform $platform
}

# Only auto-run when this file is executed directly.
# Dot-sourcing (". .\install.ps1") -- as the Pester tests do -- loads the
# functions above without invoking the installer.
if ($MyInvocation.InvocationName -ne '.' -and -not $NoExec) {
    Main
}
