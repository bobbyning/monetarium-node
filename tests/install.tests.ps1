#requires -Modules Pester

<#
.SYNOPSIS
    Pester tests for install.ps1 (Windows).

.DESCRIPTION
    Dot-sources install.ps1 (which, thanks to the InvocationName guard at
    the bottom of that file, only loads its functions and does not run
    Main). Network calls (Invoke-RestMethod / Invoke-WebRequest) and the
    Windows-elevation check are mocked so the test never touches the real
    monetarium GitHub releases or launches real processes. Everything else
    (file writes, ACLs, zip extraction, config content) runs for real
    against a throwaway temp directory.
#>

BeforeAll {
    $Script:InstallScript = Join-Path $PSScriptRoot '..' 'install.ps1'
    . $Script:InstallScript -NoExec

    # The mocked release content for a repo, and the sha256 the mocked API
    # publishes for it. Both derive from the same string so they agree.
    function Get-MockReleaseContent {
        param([Parameter(Mandatory)][string]$Repo)
        return "mock exe for $Repo"
    }

    function Get-MockReleaseArchive {
        param([Parameter(Mandatory)][string]$Repo)
        # Build the release zip once per repo and cache its bytes. The mocked
        # API publishes the digest of these exact bytes and the mocked download
        # writes them, so verification always agrees. Rebuilding the zip would
        # not: Compress-Archive stamps entries with the current time.
        if (-not $script:MockArchives.ContainsKey($Repo)) {
            $tmp = Join-Path $env:TEMP ([guid]::NewGuid())
            New-Item -ItemType Directory -Path $tmp -Force | Out-Null
            try {
                $exe = Join-Path $tmp "$Repo.exe"
                Set-Content -Path $exe -Value (Get-MockReleaseContent -Repo $Repo)
                $zip = Join-Path $tmp "$Repo-windows-amd64.zip"
                Compress-Archive -Path $exe -DestinationPath $zip -Force
                $script:MockArchives[$Repo] = [IO.File]::ReadAllBytes($zip)
            } finally {
                Remove-Item -Path $tmp -Recurse -Force -ErrorAction SilentlyContinue
            }
        }
        return ,$script:MockArchives[$Repo]
    }

    function Get-MockReleaseHash {
        param([Parameter(Mandatory)][string]$Repo)
        $bytes = Get-MockReleaseArchive -Repo $Repo
        $sha = [Security.Cryptography.SHA256]::Create()
        try {
            return ([BitConverter]::ToString($sha.ComputeHash($bytes)) -replace '-', '').ToLowerInvariant()
        } finally {
            $sha.Dispose()
        }
    }

    # Stand-in for a live process. Only the members the installer touches.
    # HasExited is a script property so a test can flip it to simulate a
    # process that actually stopped.
    function New-FakeProcess {
        param([int]$Id = 4242)
        return [PSCustomObject]@{
            Id        = $Id
            HasExited = $false
        }
    }
}

Describe 'install.ps1' {

    BeforeEach {
        $Script:TestRoot = Join-Path $env:TEMP ([guid]::NewGuid())
        New-Item -ItemType Directory -Path $Script:TestRoot -Force | Out-Null

        # Redirect install/data/startup dirs into the throwaway root.
        $Script:InstallDir  = Join-Path $Script:TestRoot 'bin'
        $Script:DataDir     = Join-Path $Script:TestRoot 'data'
        $Script:WalletConf  = Join-Path $Script:DataDir 'monetarium-wallet.conf'
        $Script:NodeConf    = Join-Path $Script:DataDir 'monetarium.conf'
        $Script:CtlDir      = Join-Path $Script:TestRoot 'ctl'
        $Script:CtlConf     = Join-Path $Script:CtlDir 'monetarium-ctl.conf'
        $Script:Manifest    = Join-Path $Script:DataDir 'install.manifest'
        $Script:StartupDir  = Join-Path $Script:TestRoot 'startup'

        Mock Test-IsAdministrator { return $true }

        # Fake "latest release" API response. The digest is computed from the
        # same bytes the mocked download writes, so verification passes unless
        # a test deliberately breaks it.
        Mock Invoke-RestMethod {
            param($Uri)
            if ($Uri -notmatch '/repos/[^/]+/([^/]+)/releases/latest') {
                throw "Unexpected Invoke-RestMethod call: $Uri"
            }
            $repo = $Matches[1]
            $digest = if ($script:ReleaseDigestOverride -and $script:ReleaseDigestOverride.ContainsKey($repo)) {
                $script:ReleaseDigestOverride[$repo]
            } else {
                "sha256:$(Get-MockReleaseHash -Repo $repo)"
            }
            [PSCustomObject]@{
                assets = @(
                    [PSCustomObject]@{
                        name                 = "${repo}-windows-amd64.zip"
                        browser_download_url = "https://fixtures.test/${repo}-windows-amd64.zip"
                        digest               = $digest
                    }
                )
            }
        }

        # Fake download: build a small real zip containing a stub .exe. The
        # zip is built once per repo and cached, because Compress-Archive
        # stamps entries with the current time: rebuilding it would produce
        # different bytes than the digest the mocked API published.
        Mock Invoke-WebRequest {
            param($Uri, $OutFile)
            if ($Uri -notmatch '/([a-zA-Z0-9_-]+)-windows-amd64\.zip$') {
                throw "Unexpected Invoke-WebRequest call: $Uri"
            }
            $binName = $Matches[1]
            [IO.File]::WriteAllBytes($OutFile, (Get-MockReleaseArchive -Repo $binName))
        }

        # Don't launch real processes.
        Mock Start-Process { }

        # Wallet --create is tested end-to-end via Linux E2E; in Pester
        # the stub .exe isn't a real binary, so mock it as a no-op.
        Mock Create-Wallet { }

        # Post-install RPC configuration needs a live wallet. The normal-flow
        # tests only care that it runs, so it is stubbed to append the same
        # generate= line the real function writes.
        Mock Invoke-ConfigureMiningAndVoting {
            $genValue = if ($Script:MiningEnabled) { 'true' } else { 'false' }
            "`ngenerate=$genValue" | Add-Content -Path $Script:NodeConf -Encoding ASCII
        }

        # Get-Acl/Set-Acl are Windows-only. The ACL behavior itself is covered
        # by the Windows CI job; here the call is stubbed so the rest of the
        # install flow can run on any platform.
        Mock Protect-ConfigFile { }

        # The stub .exe is not a real binary, so the --version gate is mocked
        # here; the gate itself is exercised by the update-mode tests.
        Mock Test-BinaryVersion { return $true }

        # Update mode: no real processes, no real waits, no real ctl calls.
        $Script:ServiceStopTimeoutSecs = 0
        $Script:ServiceStartGraceSecs  = 0
        $script:ReleaseDigestOverride  = @{}
        $script:MockArchives           = @{}
        $script:RunningProcesses       = @{}
        $script:StoppedProcesses       = @()
        $script:StartedProcesses       = @()
        $script:StopRequests           = @()
        $script:StaleProcesses         = @{}
        $script:ProcessesThatWontStop  = @{}
        $script:ProcessesThatExitOnStart = @()

        Mock Get-MonetariumProcess {
            param($BinaryName)
            if ($script:RunningProcesses.ContainsKey($BinaryName)) { return $script:RunningProcesses[$BinaryName] }
            return $null
        }
        Mock Test-StaleProcess {
            param($BinaryName)
            return [bool]$script:StaleProcesses[$BinaryName]
        }
        Mock Stop-Process {
            param($Id, [switch]$Force)
            if ($script:ProcessesThatWontStop -contains $Id) { return }
            $script:StoppedProcesses += $Id
            foreach ($proc in $script:RunningProcesses.Values) {
                if ($proc.Id -eq $Id) { $proc.HasExited = $true }
            }
        }
        # The graceful RPC stop is what actually stops a healthy process here;
        # Stop-Process is only the forced fallback.
        Mock Request-GracefulStop {
            param($BinaryName)
            $script:StopRequests += $BinaryName
            if ($script:RunningProcesses.ContainsKey($BinaryName)) {
                $script:RunningProcesses[$BinaryName].HasExited = $true
            }
        }
        Mock Start-MonetariumProcess {
            param($BinaryName)
            $script:StartedProcesses += $BinaryName
            if ($script:ProcessesThatExitOnStart -contains $BinaryName) {
                Write-ErrAndThrow "$BinaryName exited immediately after restart."
            }
        }
    }

    AfterEach {
        Remove-Item -Path $Script:TestRoot -Recurse -Force -ErrorAction SilentlyContinue
    }

    Context 'normal flow' {
        BeforeEach {
            Mock Read-Host {
                ConvertTo-SecureString -String 'test-passphrase-123' -AsPlainText -Force
            }
        }

        It 'refuses to run when not elevated' {
            Mock Test-IsAdministrator { return $false }
            { Main } | Should -Throw '*elevated*'
        }

        It 'downloads and installs all three binaries' {
            Main
            Test-Path (Join-Path $Script:InstallDir 'monetarium-node.exe')   | Should -BeTrue
            Test-Path (Join-Path $Script:InstallDir 'monetarium-wallet.exe') | Should -BeTrue
            Test-Path (Join-Path $Script:InstallDir 'monetarium-ctl.exe')    | Should -BeTrue
        }

        It 'writes a wallet config containing the passphrase and staking flags' {
            Main
            $content = Get-Content -Path $Script:WalletConf -Raw
            $content | Should -Match 'pass=test-passphrase-123'
            $content | Should -Match 'enablevoting=0'
            $content | Should -Match 'enableticketbuyer=0'
            $content | Should -Match 'ticketbuyer\.limit=1'
            $content | Should -Match 'ticketbuyer\.balancetomaintainabsolute=1'
            $content | Should -Match 'gaplimit=20'
            $content | Should -Match 'accountgaplimit=10'
        }

        It 'writes a node config with rpc credentials and mining flag, and no hardcoded peers' {
            Main
            $content = Get-Content -Path $Script:NodeConf -Raw
            $content | Should -Match 'rpcuser=monetarium'
            $content | Should -Match 'rpcpass='
            # Peer discovery is handled by the HTTPS seeders compiled into
            # chaincfg, so no peers may be pinned in the generated config.
            $content | Should -Not -Match '(?m)^addpeer='
            $content | Should -Match 'generate=false'
        }

        It 'does not leave the passphrase visible in Main''s own console output' {
            $output = Main 6>&1 5>&1 3>&1 | Out-String
            $output | Should -Not -Match 'test-passphrase-123'
        }

        It 'locks down the wallet config ACL so only current user/SYSTEM/Administrators have access' {
            # Get-Acl is Windows-only, so the ACL itself is asserted by the
            # Windows CI job; here we assert the installer asks for it.
            Main
            Should -Invoke Protect-ConfigFile -Times 3 -Exactly
        }

        It 'writes a ctl config with matching rpc credentials' {
            Main
            $content = Get-Content -Path $Script:CtlConf -Raw
            $content | Should -Match 'rpcuser=monetarium'
            $content | Should -Match 'rpcpass='
        }

        It 'writes the install manifest' {
            Main
            Test-Path $Script:Manifest | Should -BeTrue
            $content = Get-Content -Path $Script:Manifest -Raw
            $content | Should -Match 'monetarium-node.exe'
            $content | Should -Match 'monetarium-wallet.exe'
            $content | Should -Match 'monetarium-ctl.exe'
            $content | Should -Match 'MonetariumNode'
            $content | Should -Match 'MonetariumWallet'
        }

        It 'creates startup scripts for node and wallet' {
            Main
            $nodeScript = Join-Path $Script:StartupDir 'MonetariumNode.vbs'
            $walletScript = Join-Path $Script:StartupDir 'MonetariumWallet.vbs'
            Test-Path $nodeScript   | Should -BeTrue
            Test-Path $walletScript | Should -BeTrue
            $nodeContent   = Get-Content -Path $nodeScript -Raw
            $walletContent = Get-Content -Path $walletScript -Raw
            $nodeContent   | Should -Match 'monetarium-node'
            $walletContent | Should -Match 'monetarium-wallet'
            $walletContent | Should -Match 'timeout'
        }

        It 'starts both processes during installation' {
            Main
            Should -Invoke Start-Process -Times 2 -Exactly
        }

        It 'prints the big warning banner' {
            $output = Main 6>&1 | Out-String
            $output | Should -Match 'WARNING'
            $output | Should -Match 'PLAINTEXT'
        }

        It 'verifies each download against the published sha256' {
            # A wrong digest must abort, which only happens if verification ran.
            $script:ReleaseDigestOverride['monetarium-ctl'] = 'sha256:' + ('0' * 64)
            { Main } | Should -Throw '*Checksum mismatch for monetarium-ctl*'
        }

        It 'refuses to install a download whose checksum does not match' {
            $script:ReleaseDigestOverride['monetarium-wallet'] = 'sha256:' + ('0' * 64)
            { Main } | Should -Throw '*Checksum mismatch*'
            Test-Path (Join-Path $Script:InstallDir 'monetarium-wallet.exe') | Should -BeFalse
        }

        It 'warns but installs when the release publishes no checksum' {
            $script:ReleaseDigestOverride['monetarium-node'] = $null
            $output = Main 6>&1 | Out-String
            $output | Should -Match 'No sha256 checksum published'
            Test-Path (Join-Path $Script:InstallDir 'monetarium-node.exe') | Should -BeTrue
        }
    }

    Context 'install mode detection' {
        BeforeEach {
            Mock Read-Host { '1' }
            Mock Test-InteractiveHost { return $true }
        }

        It 'installs when nothing is present' {
            Get-InstallMode | Should -Be 'Install'
        }

        It 'updates when both configs exist' {
            New-Item -ItemType Directory -Path $Script:DataDir -Force | Out-Null
            Set-Content -Path $Script:NodeConf -Value 'rpcuser=x'
            Set-Content -Path $Script:WalletConf -Value 'username=x'
            Get-InstallMode | Should -Be 'Update'
        }

        It 'updates when configs exist even without binaries' {
            New-Item -ItemType Directory -Path $Script:DataDir -Force | Out-Null
            Set-Content -Path $Script:NodeConf -Value 'rpcuser=x'
            Set-Content -Path $Script:WalletConf -Value 'username=x'
            Get-InstallMode | Should -Be 'Update'
        }

        It 'falls back to a full install when startup scripts exist without configs' {
            New-Item -ItemType Directory -Path $Script:StartupDir -Force | Out-Null
            Set-Content -Path (Join-Path $Script:StartupDir 'MonetariumNode.vbs') -Value 'x'
            Get-InstallMode | Should -Be 'Install'
        }

        It 'asks when startup scripts and a wallet database exist without configs' {
            New-Item -ItemType Directory -Path $Script:StartupDir -Force | Out-Null
            Set-Content -Path (Join-Path $Script:StartupDir 'MonetariumNode.vbs') -Value 'x'
            $walletDir = Join-Path (Get-WalletDataDir) 'mainnet'
            New-Item -ItemType Directory -Path $walletDir -Force | Out-Null
            Set-Content -Path (Join-Path $walletDir 'wallet.db') -Value 'x'
            try {
                Get-InstallMode | Should -Be 'Update'
                Should -Invoke Read-Host -Times 1 -Exactly
            } finally {
                Remove-Item -Path (Get-WalletDataDir) -Recurse -Force -ErrorAction SilentlyContinue
            }
        }

        It 'asks when only binaries are present' {
            New-Item -ItemType Directory -Path $Script:InstallDir -Force | Out-Null
            Set-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Value 'x'
            Get-InstallMode | Should -Be 'Update'
            Should -Invoke Read-Host -Times 1 -Exactly
        }

        It 'honours a full-install choice for ambiguous leftovers' {
            Mock Read-Host { '2' }
            New-Item -ItemType Directory -Path $Script:InstallDir -Force | Out-Null
            Set-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Value 'x'
            Get-InstallMode | Should -Be 'Install'
        }

        It 'aborts instead of guessing when there is no interactive host' {
            Mock Test-InteractiveHost { return $false }
            New-Item -ItemType Directory -Path $Script:InstallDir -Force | Out-Null
            Set-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Value 'x'
            { Get-InstallMode } | Should -Throw '*non-interactively*'
        }
    }

    Context 'update mode' {
        BeforeEach {
            # Install-only writers, mocked so the assertions below can prove
            # update mode never calls them.
            Mock Write-Config { }
            Mock Write-CtlConfig { }
            Mock Install-StartupScripts { }
            Mock Write-Manifest { }

            # A configured, installed node: configs, binaries and startup scripts.
            New-Item -ItemType Directory -Path $Script:DataDir -Force | Out-Null
            New-Item -ItemType Directory -Path $Script:InstallDir -Force | Out-Null
            New-Item -ItemType Directory -Path $Script:StartupDir -Force | Out-Null
            Set-Content -Path $Script:NodeConf -Value 'rpcuser=keep-me'
            Set-Content -Path $Script:WalletConf -Value 'username=keep-me'
            Set-Content -Path (Join-Path $Script:StartupDir 'MonetariumNode.vbs') -Value 'keep-me'
            foreach ($name in @('monetarium-node', 'monetarium-wallet', 'monetarium-ctl')) {
                Set-Content -Path (Join-Path $Script:InstallDir "$name.exe") -Value "old $name"
            }
            Mock Read-Host { throw 'update mode must never prompt' }
        }

        It 'replaces changed binaries and restarts the running node' {
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            Main
            (Get-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Raw) |
                Should -Match 'mock exe for monetarium-node'
            $script:StartedProcesses | Should -Be @('monetarium-node')
            # Stopped through the graceful RPC path, not a forced kill.
            $script:StopRequests | Should -Contain 'monetarium-node'
            $script:StoppedProcesses | Should -BeNullOrEmpty
        }

        It 'never prompts, rewrites configs, startup scripts or the manifest' {
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            $before = @{}
            foreach ($path in @($Script:NodeConf, $Script:WalletConf, (Join-Path $Script:StartupDir 'MonetariumNode.vbs'))) {
                $before[$path] = (Get-FileHash -Path $path).Hash
            }
            Main
            foreach ($path in $before.Keys) {
                (Get-FileHash -Path $path).Hash | Should -Be $before[$path]
            }
            Test-Path $Script:Manifest | Should -BeFalse
            Should -Invoke Read-Host -Times 0 -Exactly
            Should -Invoke Write-Config -Times 0 -Exactly
            Should -Invoke Install-StartupScripts -Times 0 -Exactly
            Should -Invoke Write-Manifest -Times 0 -Exactly
        }

        It 'does nothing when every binary is already current' {
            foreach ($name in @('monetarium-node', 'monetarium-wallet', 'monetarium-ctl')) {
                Set-Content -Path (Join-Path $Script:InstallDir "$name.exe") -Value (Get-MockReleaseContent -Repo $name)
            }
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            Main
            $script:StartedProcesses | Should -BeNullOrEmpty
            $script:StoppedProcesses | Should -BeNullOrEmpty
        }

        It 'restarts a stale process even when the binary is already current' {
            foreach ($name in @('monetarium-node', 'monetarium-wallet', 'monetarium-ctl')) {
                Set-Content -Path (Join-Path $Script:InstallDir "$name.exe") -Value (Get-MockReleaseContent -Repo $name)
            }
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            $script:StaleProcesses['monetarium-node'] = $true
            Main
            $script:StartedProcesses | Should -Be @('monetarium-node')
        }

        It 'never starts a process that was stopped before the update' {
            Main
            $script:StartedProcesses | Should -BeNullOrEmpty
            $script:StoppedProcesses | Should -BeNullOrEmpty
        }

        It 'restarts the node before the wallet' {
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess -Id 1
            $script:RunningProcesses['monetarium-wallet'] = New-FakeProcess -Id 2
            Main
            $script:StartedProcesses | Should -Be @('monetarium-node', 'monetarium-wallet')
        }

        It 'aborts with nothing changed when GitHub is unreachable' {
            Mock Invoke-RestMethod { throw 'network down' }
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            { Main } | Should -Throw '*Could not reach GitHub*'
            (Get-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Raw) | Should -Match 'old monetarium-node'
            $script:StoppedProcesses | Should -BeNullOrEmpty
            $script:StartedProcesses | Should -BeNullOrEmpty
        }

        It 'aborts with nothing changed when one repo fails its checksum' {
            $script:ReleaseDigestOverride['monetarium-wallet'] = 'sha256:' + ('0' * 64)
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            { Main } | Should -Throw '*Checksum mismatch*'
            (Get-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Raw) | Should -Match 'old monetarium-node'
            $script:StoppedProcesses | Should -BeNullOrEmpty
        }

        It 'aborts with nothing changed when a download fails its smoke test' {
            Mock Test-BinaryVersion { return $false }
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            { Main } | Should -Throw '*smoke test*'
            (Get-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Raw) | Should -Match 'old monetarium-node'
            $script:StoppedProcesses | Should -BeNullOrEmpty
        }

        It 'keeps the existing binary when the release publishes no checksum' {
            $script:ReleaseDigestOverride['monetarium-node'] = $null
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            Main
            (Get-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Raw) | Should -Match 'old monetarium-node'
            $script:StartedProcesses | Should -BeNullOrEmpty
        }

        It 'aborts with nothing replaced when a process will not stop' {
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess -Id 7
            $script:ProcessesThatWontStop = @(7)
            # The graceful stop is what fails here; the forced kill is the
            # fallback and must also leave the process running.
            Mock Request-GracefulStop { }
            { Main } | Should -Throw '*Could not stop monetarium-node*'
            (Get-Content -Path (Join-Path $Script:InstallDir 'monetarium-node.exe') -Raw) | Should -Match 'old monetarium-node'
        }

        It 'fails loudly when a restarted process exits immediately' {
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            $script:ProcessesThatExitOnStart = @('monetarium-node')
            { Main } | Should -Throw '*exited immediately*'
        }

        It 'repairs a missing binary without starting anything' {
            Remove-Item -Path (Join-Path $Script:InstallDir 'monetarium-ctl.exe') -Force
            Main
            Test-Path (Join-Path $Script:InstallDir 'monetarium-ctl.exe') | Should -BeTrue
            $script:StartedProcesses | Should -BeNullOrEmpty
        }

        It 'leaves no staging directory behind' {
            $script:RunningProcesses['monetarium-node'] = New-FakeProcess
            Main
            $Script:StagingDir | Should -BeNullOrEmpty
        }
    }
}

Describe 'Show-ConfigurationSummary' {

    BeforeEach {
        $script:SummaryLines = @()
        Mock Write-Host { $script:SummaryLines += $Object }
    }

    It 'shows Auto voting enabled when VotingEnabled is true' {
        $Script:MiningEnabled = $false; $Script:MiningAddr = $null
        $Script:TicketsEnabled = $false; $Script:ConsolidationAddr = $null
        $Script:VotingEnabled = $true

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Match 'Auto voting:\s+enabled'
    }

    It 'shows Auto voting disabled when VotingEnabled is false' {
        $Script:MiningEnabled = $false; $Script:MiningAddr = $null
        $Script:TicketsEnabled = $false; $Script:ConsolidationAddr = $null
        $Script:VotingEnabled = $false

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Match 'Auto voting:\s+disabled'
    }

    It 'shows fee consolidation address when ConsolidationAddr is set' {
        $Script:MiningEnabled = $false; $Script:MiningAddr = $null
        $Script:TicketsEnabled = $false; $Script:ConsolidationAddr = 'TcTestAddr123'
        $Script:VotingEnabled = $false

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Match 'Fee consolidation address:\s+TcTestAddr123'
    }

    It 'omits fee consolidation address when ConsolidationAddr is null' {
        $Script:MiningEnabled = $false; $Script:MiningAddr = $null
        $Script:TicketsEnabled = $false; $Script:ConsolidationAddr = $null
        $Script:VotingEnabled = $false

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Not -Match 'Fee consolidation'
    }

    It 'shows mining address when both MiningEnabled and MiningAddr are set' {
        $Script:MiningEnabled = $true; $Script:MiningAddr = 'TMiningAddr'
        $Script:TicketsEnabled = $false; $Script:ConsolidationAddr = $null
        $Script:VotingEnabled = $false

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Match 'Mining address:\s+TMiningAddr'
    }

    It 'omits mining address when MiningAddr is null' {
        $Script:MiningEnabled = $true; $Script:MiningAddr = $null
        $Script:TicketsEnabled = $false; $Script:ConsolidationAddr = $null
        $Script:VotingEnabled = $false

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Not -Match 'Mining address'
    }

    It 'shows ticket buyer lines when TicketsEnabled and ConsolidationAddr set' {
        $Script:MiningEnabled = $false; $Script:MiningAddr = $null
        $Script:TicketsEnabled = $true; $Script:ConsolidationAddr = 'TcAddr'
        $Script:TicketLimit = 3; $Script:TicketBalance = 10
        $Script:VotingEnabled = $false

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Match 'Ticket buyer limit:\s+3'
        $combined | Should -Match 'Balance to maintain:\s+10'
    }

    It 'omits ticket buyer lines when TicketsEnabled is false' {
        $Script:MiningEnabled = $false; $Script:MiningAddr = $null
        $Script:TicketsEnabled = $false; $Script:ConsolidationAddr = $null
        $Script:VotingEnabled = $false

        Show-ConfigurationSummary

        $combined = $script:SummaryLines -join "`n"
        $combined | Should -Not -Match 'Ticket buyer'
        $combined | Should -Not -Match 'Balance to maintain'
    }
}

Describe 'Get-Platform' {
    It 'maps AMD64 to windows-amd64' {
        $env:PROCESSOR_ARCHITECTURE = 'AMD64'
        Get-Platform | Should -Be 'windows-amd64'
    }

    It 'maps ARM64 to windows-arm64' {
        $env:PROCESSOR_ARCHITECTURE = 'ARM64'
        Get-Platform | Should -Be 'windows-arm64'
    }
}

Describe 'ConvertFrom-SecureStringToPlainText' {
    It 'round-trips a plaintext value through a SecureString' {
        $secure = ConvertTo-SecureString -String 'hello world' -AsPlainText -Force
        ConvertFrom-SecureStringToPlainText -SecureString $secure | Should -Be 'hello world'
    }
}
