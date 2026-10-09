param(
    [string]$Version = $(if ($env:BEAM_VERSION) { $env:BEAM_VERSION } else { "latest" }),
    [string]$PullRequest = $(if ($env:BEAM_PR) { $env:BEAM_PR } else { "" }),
    [string]$CdnBaseUrl = $(if ($env:BEAM_CDN_BASE_URL) { $env:BEAM_CDN_BASE_URL } else { "https://cdn.b1m.ai/cli" }),
    [string]$InstallDirectory = $(if ($env:BEAM_INSTALL_DIR) { $env:BEAM_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Beam\bin" })
)

$ErrorActionPreference = "Stop"
$CdnBaseUrl = $CdnBaseUrl.TrimEnd("/")
$InstallDirectory = [IO.Path]::GetFullPath($InstallDirectory)
if ($PullRequest -and $PullRequest -notmatch "^[1-9][0-9]*$") {
    throw "PullRequest/BEAM_PR must be a positive integer without leading zeroes"
}

function Test-PathContains {
    param(
        [string]$PathValue,
        [string]$Directory
    )

    $expected = [Environment]::ExpandEnvironmentVariables($Directory.Trim().Trim('"')).TrimEnd("\")
    foreach ($entry in @($PathValue -split ";")) {
        $candidate = [Environment]::ExpandEnvironmentVariables($entry.Trim().Trim('"')).TrimEnd("\")
        if ($candidate -and [StringComparer]::OrdinalIgnoreCase.Equals($candidate, $expected)) {
            return $true
        }
    }
    return $false
}

function Add-ToUserPath {
    param([string]$Directory)

    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $added = $false
    if (-not (Test-PathContains $userPath $Directory)) {
        $updatedPath = if ([string]::IsNullOrWhiteSpace($userPath)) {
            $Directory
        } else {
            $userPath.TrimEnd(";") + ";" + $Directory
        }
        [Environment]::SetEnvironmentVariable("Path", $updatedPath, "User")
        $added = $true
    }
    if (-not (Test-PathContains $env:Path $Directory)) {
        $env:Path = $env:Path.TrimEnd(";") + ";" + $Directory
    }
    return $added
}

function Invoke-QuietCli {
    # Runs a CLI command best-effort and silently: an older CLI found on PATH,
    # or one without a running agent, must not print errors during an install.
    param(
        [string]$Cli,
        [string[]]$Arguments
    )

    # Scoped to this function: stderr lines must not become terminating errors.
    $ErrorActionPreference = "SilentlyContinue"
    try {
        & $Cli @Arguments 2>$null | Out-Null
        return $LASTEXITCODE -eq 0
    } catch {
        return $false
    }
}

if ($Version -eq "latest") {
    $manifest = if ($PullRequest) { "pr-$PullRequest.json" } else { "latest.json" }
    $release = Invoke-RestMethod -Uri "$CdnBaseUrl/$manifest"
    $Version = $release.version
}
$plainVersion = $Version.TrimStart("v")
if (-not $plainVersion -or $plainVersion -notmatch "^[0-9A-Za-z.-]+$") {
    throw "Invalid Beam version returned by the CDN: $Version"
}
$Version = "v$plainVersion"
$isDevelopment = $plainVersion -eq "dev" -or $plainVersion.StartsWith("dev.") -or $plainVersion.EndsWith("-dev") -or $plainVersion.Contains("-dev.")
$prNumber = if ($plainVersion -match "^0\.0\.0-pr\.([1-9][0-9]*)\.") { $Matches[1] } else { "" }
if ($PullRequest -and $PullRequest -ne $prNumber) {
    throw "CDN manifest did not resolve bundle for PR $PullRequest"
}
if ($plainVersion.StartsWith("0.0.0-pr.") -and -not $prNumber) {
    throw "Invalid PR bundle version returned by the CDN: $Version"
}
$cliBinary = if ($prNumber) { "beam-pr-$prNumber.exe" } elseif ($isDevelopment) { "beam-dev.exe" } else { "beam.exe" }
$legacyCliBinary = if ($prNumber) { "" } elseif ($isDevelopment) { "beam-cli-dev.exe" } else { "beam-cli.exe" }
$agentBinary = if ($prNumber) { "beam-tunnel-agent-pr$prNumber.exe" } elseif ($isDevelopment) { "beam-tunnel-agent-dev.exe" } else { "beam-tunnel-agent.exe" }
$archive = "beam_${plainVersion}_windows_amd64.zip"
$base = "$CdnBaseUrl/releases/$Version"
$temporary = Join-Path ([IO.Path]::GetTempPath()) ("beam-install-" + [guid]::NewGuid())
New-Item -ItemType Directory -Path $temporary | Out-Null
try {
    Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $temporary $archive)
    Invoke-WebRequest -Uri "$base/checksums.txt" -OutFile (Join-Path $temporary "checksums.txt")
    $expectedLine = Get-Content (Join-Path $temporary "checksums.txt") | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" }
    if (-not $expectedLine) { throw "Checksum for $archive is missing." }
    $expected = ($expectedLine -split "\s+")[0].ToLowerInvariant()
    $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $temporary $archive)).Hash.ToLowerInvariant()
    if ($actual -ne $expected) { throw "Checksum verification failed for $archive." }
    Expand-Archive -Path (Join-Path $temporary $archive) -DestinationPath $temporary -Force
    New-Item -ItemType Directory -Path $InstallDirectory -Force | Out-Null
    $installedBeam = Join-Path $InstallDirectory $cliBinary
    $legacyInstalledBeam = if ($legacyCliBinary) { Join-Path $InstallDirectory $legacyCliBinary } else { "" }
    $freshInstall = -not (Test-Path $installedBeam) -and (-not $legacyInstalledBeam -or -not (Test-Path $legacyInstalledBeam))

    # A previous installation may live in another PATH directory. Try every
    # Beam CLI name so its matching CLI can gracefully stop the running agent.
    $stopCandidates = if ($prNumber) { @($installedBeam) } else { @($installedBeam, $legacyInstalledBeam) }
    $stopNames = if ($prNumber) { @($cliBinary) } else { @("beam.exe", "beam-dev.exe", "beam-cli.exe", "beam-cli-dev.exe") }
    foreach ($name in $stopNames) {
        $command = Get-Command $name -CommandType Application -ErrorAction SilentlyContinue
        if ($command) {
            $stopCandidates += $command.Source
        }
    }
    $seenStopCandidates = @{}
    foreach ($candidate in $stopCandidates) {
        if ($candidate -and (Test-Path $candidate) -and -not $seenStopCandidates.ContainsKey($candidate)) {
            $seenStopCandidates[$candidate] = $true
            # Every released CLI stops the agent with `agent stop`.
            Invoke-QuietCli $candidate @("agent", "stop") | Out-Null
        }
    }
    foreach ($binary in @($cliBinary, $agentBinary)) {
        $staged = Join-Path $InstallDirectory (".$binary.new")
        $destination = Join-Path $InstallDirectory $binary
        Copy-Item (Join-Path $temporary $binary) $staged -Force
        # A stopped agent can hold its executable for a moment while it exits.
        for ($attempt = 1; ; $attempt++) {
            try {
                Move-Item $staged $destination -Force
                break
            } catch {
                if ($attempt -ge 20) {
                    Remove-Item $staged -Force -ErrorAction SilentlyContinue
                    throw "Could not replace ${destination}: $($_.Exception.Message.Trim()) If Beam is running, stop it with '$cliBinary agent stop' and run the installer again."
                }
                Start-Sleep -Milliseconds 500
            }
        }
    }
    if (-not $prNumber) {
        foreach ($legacyBinary in @($legacyCliBinary, "beam-agentd.exe")) {
            Remove-Item (Join-Path $InstallDirectory $legacyBinary) -Force -ErrorAction SilentlyContinue
        }
    }
    $pathAdded = Add-ToUserPath $InstallDirectory
    Write-Output "Installed $cliBinary and $agentBinary $Version to $InstallDirectory"
    if ($pathAdded) {
        Write-Output "Added $InstallDirectory to the current user's PATH"
    } else {
        Write-Output "$InstallDirectory is already in the current user's PATH"
    }

    $skipOnboarding = $env:BEAM_SKIP_ONBOARDING -match "^(1|true|yes)$"
    $interactive = [Environment]::UserInteractive
    try {
        $interactive = $interactive -and -not [Console]::IsInputRedirected -and -not [Console]::IsOutputRedirected
    } catch { }
    if ($freshInstall -and -not $skipOnboarding) {
        if ($interactive) {
            Write-Output ""
            Write-Output "Beam onboarding"
            Write-Output ""
            $previousProfile = $env:BEAM_POWERSHELL_PROFILE
            $onboardingExitCode = 1
            try {
                $env:BEAM_POWERSHELL_PROFILE = [string]$PROFILE
                & $installedBeam setup
                $onboardingExitCode = $LASTEXITCODE
            } catch {
                Write-Warning "Beam onboarding could not be launched: $($_.Exception.Message)"
            } finally {
                if ($null -eq $previousProfile) {
                    Remove-Item Env:BEAM_POWERSHELL_PROFILE -ErrorAction SilentlyContinue
                } else {
                    $env:BEAM_POWERSHELL_PROFILE = $previousProfile
                }
            }
            if ($onboardingExitCode -ne 0) {
                Write-Warning "Onboarding was not completed. Run '$cliBinary setup' to try again."
            }
        } else {
            Write-Output "Configure Beam when ready: $cliBinary setup"
        }
    }
} finally {
    Remove-Item $temporary -Recurse -Force -ErrorAction SilentlyContinue
}
