param(
    [ValidateSet('Install', 'Uninstall', 'Status', 'Update')]
    [string]$Action = 'Install',

    [ValidatePattern('^[A-Za-z0-9_.-]+$')]
    [string]$ServiceName = 'SysmonAgent',
    [string]$DisplayName = 'Sysmon Agent',
    [string]$Bind = '0.0.0.0',
    [ValidateRange(1, 65535)]
    [int]$Port = 9099,
    [ValidateRange(1, 300)]
    [int]$ReadinessTimeoutSeconds = 45,
    [string]$SettingsPath = "$env:ProgramData\SysmonAgent\settings.json",
    [switch]$NoFirewall,
    # Update-only: pin/downgrade to a specific vX.Y.Z tag (default: latest).
    [string]$UpdateVersion,
    # Update-only: reapply the same version even if the channel reports it as
    # not newer than the installed one. Useful for forcing a re-flash.
    [switch]$Force,
    # Update-only: report what would happen (resolve, compare, list the asset)
    # without downloading, verifying, or swapping.
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'

function Assert-Admin {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Run this script from an elevated PowerShell session.'
    }
}

function Get-AgentPath {
    $agent = Join-Path $PSScriptRoot 'sysmon-agent.exe'
    if (-not (Test-Path -LiteralPath $agent)) {
        throw "Missing $agent. Build it first with: go build -o sysmon-agent.exe ."
    }
    return (Resolve-Path -LiteralPath $agent).Path
}

function Quote-Arg([string]$Value) {
    if ($Value.Contains('"')) {
        throw "Command-line values cannot contain double quotes: $Value"
    }
    return '"' + $Value + '"'
}

function Get-BinaryPath {
    $agent = Get-AgentPath
    return "$(Quote-Arg $agent) -bind $(Quote-Arg $Bind) -port $Port -settings $(Quote-Arg $SettingsPath)"
}

function Get-FirewallRuleName {
    return "$ServiceName-$Port"
}

function Resolve-HealthHost {
    switch ($Bind.Trim()) {
        '' { return '127.0.0.1' }
        '*' { return '127.0.0.1' }
        '0.0.0.0' { return '127.0.0.1' }
        '::' { return '::1' }
        '[::]' { return '::1' }
        default {
            if ($Bind.StartsWith('[') -and $Bind.EndsWith(']')) {
                return $Bind.Substring(1, $Bind.Length - 2)
            }
            return $Bind
        }
    }
}

function Format-UrlHost([string]$HostName) {
    if ($HostName.StartsWith('[') -and $HostName.EndsWith(']')) {
        return $HostName
    }
    if ($HostName.Contains(':')) {
        return "[$HostName]"
    }
    return $HostName
}

function Test-UsableDeviceHost([string]$HostName) {
    if ([string]::IsNullOrWhiteSpace($HostName)) {
        return $false
    }
    $hostValue = $HostName.Trim('[', ']')
    $lower = $hostValue.ToLowerInvariant()
    if ($lower -eq 'localhost' -or $lower -eq '0.0.0.0' -or $lower -eq '::' -or $lower -eq '::1') {
        return $false
    }
    if ($lower.StartsWith('127.') -or $lower.StartsWith('169.254.') -or $lower.StartsWith('fe80:')) {
        return $false
    }
    return $true
}

function Test-DeviceInterfaceName([string]$InterfaceName) {
    if ([string]::IsNullOrWhiteSpace($InterfaceName)) {
        return $false
    }
    $name = $InterfaceName.Trim().ToLowerInvariant()
    foreach ($prefix in @(
        'docker',
        'hyper-v',
        'npcap loopback',
        'vethernet',
        'virtualbox host-only',
        'vmware network adapter'
    )) {
        if ($name.StartsWith($prefix)) {
            return $false
        }
    }
    foreach ($fragment in @(
        'default switch',
        'docker',
        'hyper-v',
        'loopback',
        'microsoft wi-fi direct virtual adapter',
        'nat network',
        'nat switch',
        'npcap',
        'virtualbox host-only',
        'vmware network adapter',
        'wi-fi direct virtual adapter',
        'wsl'
    )) {
        if ($name.Contains($fragment)) {
            return $false
        }
    }
    return $true
}

function Get-DeviceHostPriority([string]$HostName) {
    $hostValue = $HostName.Trim('[', ']')
    if ($hostValue -match '^100\.(\d+)\.') {
        $second = [int]$Matches[1]
        if ($second -ge 64 -and $second -le 127) {
            return 0
        }
    }
    if ($hostValue -match '^(10\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[0-1])\.)' -or $hostValue.ToLowerInvariant().StartsWith('fd')) {
        return 1
    }
    return 2
}

function Get-CandidateDeviceHosts {
    $hosts = New-Object System.Collections.Generic.List[string]
    if (Get-Command Get-NetIPAddress -ErrorAction SilentlyContinue) {
        try {
            Get-NetIPAddress -AddressFamily IPv4, IPv6 -ErrorAction Stop |
                Where-Object { $_.IPAddress -and (Test-DeviceInterfaceName ([string]$_.InterfaceAlias)) } |
                ForEach-Object { $hosts.Add([string]$_.IPAddress) }
        } catch {
        }
    }
    if ($hosts.Count -eq 0) {
        try {
            [System.Net.Dns]::GetHostAddresses([System.Net.Dns]::GetHostName()) |
                ForEach-Object { $hosts.Add($_.IPAddressToString) }
        } catch {
        }
    }

    $seen = @{}
    $hosts |
        Where-Object { Test-UsableDeviceHost $_ } |
        ForEach-Object {
            $hostValue = ([string]$_).Trim('[', ']')
            if (-not $seen.ContainsKey($hostValue)) {
                $seen[$hostValue] = $true
                [pscustomobject]@{
                    Priority = Get-DeviceHostPriority $hostValue
                    Host = $hostValue
                }
            }
        } |
        Sort-Object Priority, Host |
        ForEach-Object { $_.Host }
}

function Get-DeviceUrls {
    $bindHost = $Bind.Trim('[', ']')
    switch ($bindHost.ToLowerInvariant()) {
        '' { $hosts = @(Get-CandidateDeviceHosts) }
        '*' { $hosts = @(Get-CandidateDeviceHosts) }
        '0.0.0.0' { $hosts = @(Get-CandidateDeviceHosts) }
        '::' { $hosts = @(Get-CandidateDeviceHosts) }
        'localhost' { $hosts = @() }
        '127.0.0.1' { $hosts = @() }
        '::1' { $hosts = @() }
        default { $hosts = @($bindHost) }
    }
    $hosts |
        Where-Object { Test-UsableDeviceHost $_ } |
        Select-Object -First 5 |
        ForEach-Object { "http://$(Format-UrlHost $_):$Port/" }
}

function Show-DeviceHandoff {
    Write-Host ''
    Write-Host 'Sysmon device URLs:'
    $urls = @(Get-DeviceUrls)
    if ($urls.Count -gt 0) {
        $urls | ForEach-Object { Write-Host "  $_" }
    } else {
        Write-Host '  No direct LAN URL detected for the current bind address.'
        Write-Host '  Use -Bind 0.0.0.0 on a trusted LAN, or pass -DeviceUrl to verify-deployed-windows.ps1.'
    }

    Write-Host ''
    Write-Host 'Final Sysmon device verification:'
    Write-Host '  .\verify-deployed-windows.ps1 -HoldSeconds 120'
    Write-Host '  Add the Sysmon URL to your device Home Screen, then open that Home Screen app and tap the status strip during the hold.'
}

function Get-ReadinessCheckUrl {
    return "http://$(Format-UrlHost (Resolve-HealthHost)):$Port/readyz"
}

function Get-StatusCheckUrl {
    return "http://$(Format-UrlHost (Resolve-HealthHost)):$Port/api/status"
}

function Get-AgentBaseUrl {
    return "http://$(Format-UrlHost (Resolve-HealthHost)):$Port"
}

function Wait-AgentReady {
    $readyUrl = Get-ReadinessCheckUrl
    $deadline = (Get-Date).AddSeconds($ReadinessTimeoutSeconds)
    $lastError = $null
    do {
        try {
            $ready = Invoke-RestMethod -Method Get -Uri $readyUrl -TimeoutSec 4
            if ($ready.status -eq 'ok' -and $ready.metrics) {
                Write-Host "Readiness check ok: $readyUrl"
                return
            }
            $lastError = "unexpected readiness status: $($ready | ConvertTo-Json -Compress)"
        } catch {
            $lastError = $_.Exception.Message
        }

        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($null -eq $service) {
            throw "Service $ServiceName disappeared before becoming healthy."
        }
        if ($service.Status -eq 'Stopped') {
            throw "Service $ServiceName stopped before becoming ready. Last readiness error: $lastError"
        }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)

    throw "Service $ServiceName did not become ready at $readyUrl within ${ReadinessTimeoutSeconds}s. Last readiness error: $lastError"
}

function Start-AgentService {
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($null -eq $service) {
        throw "Service $ServiceName was not registered; cannot start it."
    }
    if ($service.Status -eq 'Running') {
        return
    }
    try {
        Start-Service -Name $ServiceName
    } catch {
        # The SCM start request can report a timeout on the very first boot while
        # the LibreHardwareMonitor kernel driver loads, even though the service
        # process is alive and reaches Running a moment later. Re-check before
        # treating it as a genuine failure.
        Start-Sleep -Seconds 2
        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($null -eq $service -or $service.Status -eq 'Stopped') {
            throw "Service $ServiceName failed to start: $($_.Exception.Message)"
        }
    }
}

function Wait-AgentReadyAdvisory {
    # Readiness is best-effort: once the service is registered and Running the
    # install has succeeded. The first cold boot can take longer than the
    # readiness window while sensor drivers load, so a slow warmup must not fail
    # the install (and force a manual re-run); the service self-heals and its
    # recovery actions restart it if it ever crashes.
    try {
        Wait-AgentReady
    } catch {
        Write-Warning "The service is installed and running, but did not report ready within ${ReadinessTimeoutSeconds}s. This is normal on the first boot while sensor drivers load; it should become ready shortly. Check 'install-windows.ps1 -Action Status' if it does not. Details: $($_.Exception.Message)"
    }
}

function Show-AgentReadiness {
    $readyUrl = Get-ReadinessCheckUrl
    try {
        $ready = Invoke-RestMethod -Method Get -Uri $readyUrl -TimeoutSec 4
        if ($ready.status -eq 'ok' -and $ready.metrics) {
            Write-Host "Dashboard readiness ok: $readyUrl"
            return
        }
        Write-Warning "Dashboard readiness failed at ${readyUrl}: $($ready | ConvertTo-Json -Compress)"
    } catch {
        Write-Warning "Dashboard readiness failed at ${readyUrl}: $($_.Exception.Message)"
    }
}

function Format-OnOff($Value) {
    if ([bool]$Value) {
        return 'on'
    }
    return 'off'
}

function Show-DashboardSettings {
    $statusUrl = Get-StatusCheckUrl
    try {
        $status = Invoke-RestMethod -Method Get -Uri $statusUrl -TimeoutSec 4
        $settings = $status.settings
        if ($null -eq $settings -or $null -eq $settings.refresh_ms -or [string]::IsNullOrWhiteSpace([string]$settings.panel) -or $null -eq $settings.thresholds) {
            Write-Warning "Dashboard settings unavailable at ${statusUrl}: $($status | ConvertTo-Json -Compress)"
            return
        }
        $persistence = if ($status.settings_persisted) { 'saved' } else { 'memory' }
        $dashboardBuild = if ([string]::IsNullOrWhiteSpace([string]$status.dashboard_build)) { 'unknown' } else { [string]$status.dashboard_build }
        Write-Host "Dashboard settings: $persistence, build=$dashboardBuild, refresh=$($settings.refresh_ms)ms, panel=$($settings.panel), dim=$(Format-OnOff $settings.dim), shift=$(Format-OnOff $settings.shift)"
        Write-Host "  thresholds: CPU $($settings.thresholds.cpu_warn)% / RAM $($settings.thresholds.memory_warn)% / Disk $($settings.thresholds.disk_warn)% / GPU $($settings.thresholds.gpu_warn)% / Temp $($settings.thresholds.temp_warn_c)C"
    } catch {
        Write-Warning "Dashboard settings failed at ${statusUrl}: $($_.Exception.Message)"
    }
}

function Test-DeviceClientCheckEvidence($ClientCheck) {
    $userAgent = [string]$ClientCheck.user_agent
    $viewportWidth = 0
    $viewportHeight = 0
    if (-not [int]::TryParse([string]$ClientCheck.viewport_width, [ref]$viewportWidth)) {
        return $false
    }
    if (-not [int]::TryParse([string]$ClientCheck.viewport_height, [ref]$viewportHeight)) {
        return $false
    }
    $mobileDevice = $userAgent.Contains('Mobile') -or $userAgent.Contains('iPhone') -or $userAgent.Contains('iPad') -or $userAgent.Contains('iPod') -or $userAgent.Contains('Android')
    return $mobileDevice -and $viewportWidth -gt 0 -and $viewportHeight -gt 0
}

function Test-StandaloneClientCheckEvidence($ClientCheck) {
    return $ClientCheck.standalone -eq $true -and [string]$ClientCheck.display_mode -eq 'standalone'
}

function Get-TimeMilliseconds($Value) {
    if ($null -eq $Value -or [string]::IsNullOrWhiteSpace([string]$Value)) {
        return $null
    }
    $parsed = [DateTimeOffset]::MinValue
    if ([DateTimeOffset]::TryParse([string]$Value, [System.Globalization.CultureInfo]::InvariantCulture, [System.Globalization.DateTimeStyles]::AssumeUniversal, [ref]$parsed)) {
        return $parsed.ToUnixTimeMilliseconds()
    }
    return $null
}

function Get-ClientCheckAgeSeconds($LastSeen) {
    $lastSeenMs = Get-TimeMilliseconds $LastSeen
    if ($null -eq $lastSeenMs) {
        return $null
    }
    $nowMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    return [Math]::Max(0, [int][Math]::Floor(($nowMs - $lastSeenMs) / 1000))
}

function Test-ClientCheckStale($AgeSeconds) {
    if ($null -eq $AgeSeconds) {
        return $false
    }
    $freshSeconds = Resolve-IntEnv 'SYSMON_STATUS_CLIENT_FRESH_SECONDS' 300
    return $AgeSeconds -gt $freshSeconds
}

function Get-CurrentDashboardBuild {
    $statusUrl = Get-StatusCheckUrl
    try {
        $status = Invoke-RestMethod -Method Get -Uri $statusUrl -TimeoutSec 4
        return [string]$status.dashboard_build
    } catch {
        return ''
    }
}

function Get-ClientCheckRank($ClientCheck) {
    if ((Test-DeviceClientCheckEvidence $ClientCheck) -and (Test-StandaloneClientCheckEvidence $ClientCheck) -and (Test-InteractionClientCheckEvidence $ClientCheck)) {
        return 4
    }
    if ((Test-DeviceClientCheckEvidence $ClientCheck) -and (Test-StandaloneClientCheckEvidence $ClientCheck)) {
        return 3
    }
    if (Test-DeviceClientCheckEvidence $ClientCheck) {
        return 2
    }
    if ($ClientCheck.seen -eq $true) {
        return 1
    }
    return 0
}

function Test-InteractionClientCheckEvidence($ClientCheck) {
    return ([string]$ClientCheck.interaction).ToLowerInvariant() -eq 'status_strip_tap'
}

function Get-BestClientCheck($ClientChecks) {
    $best = $null
    $bestRank = -1
    $bestLastSeenMs = $null
    foreach ($clientCheck in @($ClientChecks)) {
        if ($null -eq $clientCheck -or $clientCheck.seen -ne $true) {
            continue
        }
        $rank = Get-ClientCheckRank $clientCheck
        $lastSeenMs = Get-TimeMilliseconds $clientCheck.last_seen
        if ($null -eq $lastSeenMs) {
            $lastSeenMs = 0
        }
        if ($rank -gt $bestRank -or ($rank -eq $bestRank -and ($null -eq $bestLastSeenMs -or $lastSeenMs -gt $bestLastSeenMs))) {
            $best = $clientCheck
            $bestRank = $rank
            $bestLastSeenMs = $lastSeenMs
        }
    }
    return $best
}

function Get-LatestHomeScreenClientCheck($ClientChecks) {
    $best = $null
    $bestLastSeenMs = $null
    foreach ($clientCheck in @($ClientChecks)) {
        if ($null -eq $clientCheck -or $clientCheck.seen -ne $true) {
            continue
        }
        if (-not (Test-DeviceClientCheckEvidence $clientCheck) -or -not (Test-StandaloneClientCheckEvidence $clientCheck)) {
            continue
        }
        $lastSeenMs = Get-TimeMilliseconds $clientCheck.last_seen
        if ($null -eq $lastSeenMs) {
            $lastSeenMs = 0
        }
        if ($null -eq $best -or $null -eq $bestLastSeenMs -or $lastSeenMs -gt $bestLastSeenMs) {
            $best = $clientCheck
            $bestLastSeenMs = $lastSeenMs
        }
    }
    return $best
}

function Get-StatusClientCheck {
    try {
        $status = Invoke-RestMethod -Method Get -Uri (Get-StatusCheckUrl) -TimeoutSec 4
        return Get-BestClientCheck @($status.device_client_check, $status.client_check)
    } catch {
        return $null
    }
}

function Show-ClientCheckStatus {
    $baseUrl = Get-AgentBaseUrl
    $historyUrl = "$baseUrl/api/client-checks"
    $latestUrl = "$baseUrl/api/client-check"
    $url = $historyUrl
    try {
        $payload = Invoke-RestMethod -Method Get -Uri $historyUrl -TimeoutSec 4
    } catch {
        $url = $latestUrl
        try {
            $payload = Invoke-RestMethod -Method Get -Uri $latestUrl -TimeoutSec 4
        } catch {
            Write-Warning "device client: failed ($historyUrl): $($_.Exception.Message)"
            return
        }
    }

    $entries = @()
    if ($null -ne $payload.checks) {
        $entries = @($payload.checks)
    } elseif ($null -ne $payload) {
        $entries = @($payload)
    }
    $clientCheck = Get-BestClientCheck $entries
    $latestHomeScreenClientCheck = Get-LatestHomeScreenClientCheck $entries
    $statusClientCheck = Get-StatusClientCheck
    if ($null -ne $statusClientCheck -and $statusClientCheck.seen -eq $true) {
        $selectedClientCheck = Get-BestClientCheck @($clientCheck, $statusClientCheck)
        $selectedHomeScreenClientCheck = Get-LatestHomeScreenClientCheck @($latestHomeScreenClientCheck, $statusClientCheck)
        if ($null -ne $selectedHomeScreenClientCheck) {
            $latestHomeScreenClientCheck = $selectedHomeScreenClientCheck
        }
        if ($null -ne $selectedClientCheck -and [object]::ReferenceEquals($selectedClientCheck, $statusClientCheck)) {
            $url = Get-StatusCheckUrl
        }
        $clientCheck = $selectedClientCheck
    }
    if ($null -eq $clientCheck -or $clientCheck.seen -ne $true) {
        Write-Warning "device client: not observed yet ($url)"
        return
    }

    $label = if (Test-DeviceClientCheckEvidence $clientCheck) { 'device' } else { 'browser' }
    $viewportWidth = 0
    $viewportHeight = 0
    $viewport = 'unknown viewport'
    if ([int]::TryParse([string]$clientCheck.viewport_width, [ref]$viewportWidth) -and [int]::TryParse([string]$clientCheck.viewport_height, [ref]$viewportHeight) -and $viewportWidth -gt 0 -and $viewportHeight -gt 0) {
        $viewport = "${viewportWidth}x${viewportHeight}"
    }
    $displayMode = if ([string]::IsNullOrWhiteSpace([string]$clientCheck.display_mode)) { 'unknown' } else { [string]$clientCheck.display_mode }
    $standalone = if ($clientCheck.standalone -eq $true) { 'true' } else { 'false' }
    $interaction = [string]$clientCheck.interaction
    $lastSeen = if ([string]::IsNullOrWhiteSpace([string]$clientCheck.last_seen)) { 'unknown' } else { [string]$clientCheck.last_seen }
    $ageSeconds = Get-ClientCheckAgeSeconds $clientCheck.last_seen
    $ageLabel = Format-AgeLabel $ageSeconds
    $seenDetail = if ([string]::IsNullOrWhiteSpace($ageLabel)) { $lastSeen } else { "$lastSeen, $ageLabel" }
    $currentBuild = Get-CurrentDashboardBuild
    $clientBuild = [string]$clientCheck.dashboard_build
    $buildDetail = ''
    if (-not [string]::IsNullOrWhiteSpace($currentBuild)) {
        $shownBuild = if ([string]::IsNullOrWhiteSpace($clientBuild)) { 'unknown' } else { $clientBuild }
        $buildDetail = ", build=$shownBuild"
    } elseif (-not [string]::IsNullOrWhiteSpace($clientBuild)) {
        $buildDetail = ", build=$clientBuild"
    }
    $interactionDetail = if ([string]::IsNullOrWhiteSpace($interaction)) { '' } else { ", interaction=$interaction" }

    if (Test-StandaloneClientCheckEvidence $clientCheck) {
        if (Test-ClientCheckStale $ageSeconds) {
            Write-Warning "device client: $label Home Screen stale at $seenDetail ($viewport, display_mode=$displayMode$buildDetail$interactionDetail)"
            return
        }
        if (-not [string]::IsNullOrWhiteSpace($currentBuild) -and $clientBuild -ne $currentBuild) {
            $shownBuild = if ([string]::IsNullOrWhiteSpace($clientBuild)) { 'unknown' } else { $clientBuild }
            Write-Warning "device client: $label Home Screen stale dashboard build at $seenDetail ($viewport, display_mode=$displayMode, build=$shownBuild, current=$currentBuild$interactionDetail)"
            return
        }
        if (Test-InteractionClientCheckEvidence $clientCheck) {
            Write-Host "device client: $label Home Screen status-strip tap seen at $seenDetail ($viewport, display_mode=$displayMode$buildDetail$interactionDetail)"
            Show-RecentHomeScreenActivity $clientCheck $latestHomeScreenClientCheck
        } else {
            Write-Warning "device client: $label Home Screen seen without status-strip tap at $seenDetail ($viewport, display_mode=$displayMode$buildDetail)"
        }
        return
    }

    Write-Warning "device client: $label seen at $seenDetail ($viewport, display_mode=$displayMode, standalone=$standalone$buildDetail$interactionDetail)"
}

function Get-ClientCheckIdentityKey($ClientCheck) {
    if ($null -eq $ClientCheck) {
        return ''
    }
    return @(
        [string]$ClientCheck.last_seen
        [string]$ClientCheck.user_agent
        [string]$ClientCheck.viewport_width
        [string]$ClientCheck.viewport_height
        [string]$ClientCheck.display_mode
        [string]($ClientCheck.standalone -eq $true)
        [string]$ClientCheck.interaction
    ) -join "`t"
}

function Show-RecentHomeScreenActivity($ProofClientCheck, $ActivityClientCheck) {
    if ($null -eq $ActivityClientCheck -or $ActivityClientCheck.seen -ne $true) {
        return
    }
    if ((Get-ClientCheckIdentityKey $ProofClientCheck) -eq (Get-ClientCheckIdentityKey $ActivityClientCheck)) {
        return
    }
    $activityInteraction = [string]$ActivityClientCheck.interaction
    if ([string]::IsNullOrWhiteSpace($activityInteraction) -or $activityInteraction.ToLowerInvariant() -eq 'status_strip_tap') {
        return
    }
    $proofSeenMs = Get-TimeMilliseconds $ProofClientCheck.last_seen
    $activitySeenMs = Get-TimeMilliseconds $ActivityClientCheck.last_seen
    if ($null -ne $proofSeenMs -and $null -ne $activitySeenMs -and $activitySeenMs -le $proofSeenMs) {
        return
    }
    $activityLastSeen = if ([string]::IsNullOrWhiteSpace([string]$ActivityClientCheck.last_seen)) { 'unknown' } else { [string]$ActivityClientCheck.last_seen }
    $activityAgeSeconds = Get-ClientCheckAgeSeconds $ActivityClientCheck.last_seen
    $activityAgeLabel = Format-AgeLabel $activityAgeSeconds
    $activitySeenDetail = if ([string]::IsNullOrWhiteSpace($activityAgeLabel)) { $activityLastSeen } else { "$activityLastSeen, $activityAgeLabel" }
    $activityViewportWidth = 0
    $activityViewportHeight = 0
    $activityViewport = 'unknown viewport'
    if ([int]::TryParse([string]$ActivityClientCheck.viewport_width, [ref]$activityViewportWidth) -and [int]::TryParse([string]$ActivityClientCheck.viewport_height, [ref]$activityViewportHeight) -and $activityViewportWidth -gt 0 -and $activityViewportHeight -gt 0) {
        $activityViewport = "${activityViewportWidth}x${activityViewportHeight}"
    }
    Write-Host "device client: recent Home Screen activity at $activitySeenDetail ($activityViewport, interaction=$activityInteraction)"
}

function Resolve-IntEnv([string]$Name, [int]$Default) {
    $value = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrWhiteSpace($value)) {
        return $Default
    }
    $parsed = 0
    if ([int]::TryParse($value, [ref]$parsed) -and $parsed -ge 0) {
        return $parsed
    }
    return $Default
}

function Get-DeployedReportPath {
    if ($env:SYSMON_DEPLOY_VERIFY_REPORT) {
        return $env:SYSMON_DEPLOY_VERIFY_REPORT
    }
    return Join-Path ([System.IO.Path]::GetTempPath()) 'sysmon-agent-deployed-verify-report.txt'
}

function Get-DeployedReportField([string]$ReportPath, [string]$Key) {
    if (-not (Test-Path -LiteralPath $ReportPath)) {
        return ''
    }
    $prefix = "$Key="
    $value = ''
    foreach ($line in Get-Content -LiteralPath $ReportPath -ErrorAction Stop) {
        $text = [string]$line
        if ($text.StartsWith($prefix, [System.StringComparison]::Ordinal)) {
            $value = $text.Substring($prefix.Length)
        }
    }
    return $value
}

function Get-ReportAgeSeconds([string]$Timestamp) {
    if ([string]::IsNullOrWhiteSpace($Timestamp) -or $Timestamp -eq 'unknown') {
        return $null
    }
    try {
        $completed = [DateTimeOffset]::Parse($Timestamp).ToUniversalTime()
        $age = [DateTimeOffset]::UtcNow - $completed
        return [Math]::Max(0, [int][Math]::Floor($age.TotalSeconds))
    } catch {
        return $null
    }
}

function Format-AgeLabel($AgeSeconds) {
    if ($null -eq $AgeSeconds) {
        return ''
    }
    if ($AgeSeconds -lt 60) {
        return "age=${AgeSeconds}s"
    }
    if ($AgeSeconds -lt 3600) {
        return "age=$([Math]::Floor($AgeSeconds / 60))m"
    }
    return "age=$([Math]::Floor($AgeSeconds / 3600))h"
}

function Test-DeployedReportStale($AgeSeconds) {
    if ($null -eq $AgeSeconds) {
        return $false
    }
    $freshSeconds = Resolve-IntEnv 'SYSMON_DEPLOY_VERIFY_REPORT_FRESH_SECONDS' 86400
    return $AgeSeconds -gt $freshSeconds
}

function Show-DeployedReportUrl([string]$ReportPath) {
    $deviceUrl = Get-DeployedReportField $ReportPath 'device_url'
    if ([string]::IsNullOrWhiteSpace($deviceUrl)) {
        return
    }
    $deviceUrlSource = Get-DeployedReportField $ReportPath 'device_url_source'
    $sourceLabel = if ([string]::IsNullOrWhiteSpace($deviceUrlSource)) { '' } else { " ($deviceUrlSource)" }
    Write-Host "  Last device URL: $deviceUrl$sourceLabel"
}

function Show-DeployedDeviceGate {
    $reportPath = Get-DeployedReportPath
    if (-not (Test-Path -LiteralPath $reportPath)) {
        Write-Warning "Last deployed device gate: no report ($reportPath)"
        Write-Host '  Run: .\verify-deployed-windows.ps1 -HoldSeconds 120'
        return
    }

    try {
        $installed = Get-DeployedReportField $reportPath 'installed_device_home_screen'
        $result = Get-DeployedReportField $reportPath 'result'
        $completed = Get-DeployedReportField $reportPath 'completed_at'
        $clientSeen = Get-DeployedReportField $reportPath 'device_client_seen'
        $clientInteraction = Get-DeployedReportField $reportPath 'device_client_interaction'
        $dashboardBuild = Get-DeployedReportField $reportPath 'dashboard_build'
    } catch {
        Write-Warning "Last deployed device gate: unreadable report ($reportPath): $($_.Exception.Message)"
        Write-Host '  Run: .\verify-deployed-windows.ps1 -HoldSeconds 120'
        return
    }
    if ([string]::IsNullOrWhiteSpace($installed)) { $installed = 'not_verified' }
    if ([string]::IsNullOrWhiteSpace($result)) { $result = 'unknown' }
    if ([string]::IsNullOrWhiteSpace($completed)) { $completed = 'unknown' }

    $ageSeconds = Get-ReportAgeSeconds $completed
    $ageLabel = Format-AgeLabel $ageSeconds
    $ageSuffix = if ([string]::IsNullOrWhiteSpace($ageLabel)) { '' } else { ", $ageLabel" }
    $detailParts = @()
    if (-not [string]::IsNullOrWhiteSpace($clientSeen)) { $detailParts += "client=$clientSeen" }
    if (-not [string]::IsNullOrWhiteSpace($clientInteraction)) { $detailParts += "interaction=$clientInteraction" }
    if (-not [string]::IsNullOrWhiteSpace($dashboardBuild)) { $detailParts += "build=$dashboardBuild" }
    $detail = [string]::Join(', ', $detailParts)
    $passPrefix = if ([string]::IsNullOrWhiteSpace($detail)) { '' } else { "$detail, " }
    $failureDetail = if ([string]::IsNullOrWhiteSpace($detail)) { '' } else { ", $detail" }

    if ($installed -eq 'pass' -and $result -eq 'pass') {
        if (Test-DeployedReportStale $ageSeconds) {
            Write-Warning "Last deployed device gate: pass but stale (${passPrefix}completed=$completed$ageSuffix, report=$reportPath)"
            Show-DeployedReportUrl $reportPath
            Write-Host '  Run: .\verify-deployed-windows.ps1 -HoldSeconds 120'
            return
        }
        Write-Host "Last deployed device gate: pass (${passPrefix}completed=$completed$ageSuffix, report=$reportPath)"
        Show-DeployedReportUrl $reportPath
        return
    }

    Write-Warning "Last deployed device gate: $installed (result=$result$failureDetail, completed=$completed$ageSuffix, report=$reportPath)"
    Show-DeployedReportUrl $reportPath
    Write-Host '  Run: .\verify-deployed-windows.ps1 -HoldSeconds 120'
}

function Remove-ServiceFirewallRules {
    $rules = @(Get-NetFirewallRule -Name "$ServiceName-*" -ErrorAction SilentlyContinue)
    if ($rules.Count -gt 0) {
        $rules | Remove-NetFirewallRule
    }
}

function Set-ServiceRecovery {
    & sc.exe failure $ServiceName reset= 86400 actions= restart/5000/restart/5000/restart/30000 | Out-Null
    & sc.exe failureflag $ServiceName 1 | Out-Null
}

function Get-SystemProfileLocalAppData {
    # LocalSystem's LOCALAPPDATA. The service runs as LocalSystem, so its
    # %LOCALAPPDATA% is the system profile, not the installing admin's profile.
    $systemRoot = $env:SystemRoot
    if ([string]::IsNullOrWhiteSpace($systemRoot)) {
        $systemRoot = 'C:\Windows'
    }
    return Join-Path $systemRoot 'System32\config\systemprofile\AppData\Local'
}

function Find-LhmLibraryForAccount([string]$LocalAppData) {
    # Mirrors Find-LhmLibrary in lhm-bridge.ps1, parameterized by the account's
    # LOCALAPPDATA so we can resolve the library exactly as the service (running
    # as LocalSystem) will at runtime, not as the installing admin would. Keep
    # the candidate list in sync with lhm-bridge.ps1.
    $candidates = @(
        'C:\ProgramData\chocolatey\lib\librehardwaremonitor\tools\LibreHardwareMonitorLib.dll',
        'C:\Program Files\LibreHardwareMonitor\LibreHardwareMonitorLib.dll',
        'C:\Program Files (x86)\LibreHardwareMonitor\LibreHardwareMonitorLib.dll'
    )
    if (-not [string]::IsNullOrWhiteSpace($LocalAppData)) {
        $wingetDir = Join-Path $LocalAppData 'Microsoft\WinGet\Packages'
        if (Test-Path $wingetDir) {
            Get-ChildItem -Path $wingetDir -Filter 'LibreHardwareMonitorLib.dll' -Recurse -ErrorAction SilentlyContinue |
                ForEach-Object { $candidates += $_.FullName }
        }
    }
    foreach ($path in $candidates) {
        if ($path -and (Test-Path $path)) {
            return $path
        }
    }
    return $null
}

function Show-LhmLibraryStatus {
    # CPU package power and board/CPU/RAM temperatures come from the LHM bridge,
    # which loads LibreHardwareMonitorLib.dll. The bridge runs inside the agent,
    # and the agent runs as the LocalSystem service - so it can only see the DLL
    # in machine-wide locations or LocalSystem's own profile. A per-user WinGet
    # install (the default for `winget install`) lands under the installing
    # user's profile, which LocalSystem cannot read, so those sensors would
    # silently report unavailable. Surface that at install time instead.
    $serviceDll = Find-LhmLibraryForAccount (Get-SystemProfileLocalAppData)
    if ($serviceDll) {
        Write-Host "LibreHardwareMonitor library (service-visible): $serviceDll"
        return
    }

    $userDll = Find-LhmLibraryForAccount $env:LOCALAPPDATA
    if ($userDll) {
        Write-Warning "LibreHardwareMonitor is installed only in your user profile: $userDll"
        Write-Warning "The $ServiceName service runs as LocalSystem and cannot read a per-user install, so CPU package power and board temperatures will be unavailable."
    } else {
        Write-Warning "LibreHardwareMonitor was not found, so CPU package power and board temperatures will be unavailable."
    }
    Write-Host 'Fix: install it machine-wide so the LocalSystem service can load it:'
    Write-Host '  choco install librehardwaremonitor'
    Write-Host '  # or place a portable copy at C:\Program Files\LibreHardwareMonitor\'
}

function Find-PwshForService {
    # The LHM bridge needs PowerShell 7+ (pwsh); Windows PowerShell 5.1 cannot
    # host the .NET LibreHardwareMonitorLib.dll. The agent runs as LocalSystem,
    # which only sees the machine PATH and the system-wide install locations, not
    # a per-user pwsh install. Resolve it the same way the service will.
    $candidates = New-Object System.Collections.Generic.List[string]
    foreach ($base in @([Environment]::GetEnvironmentVariable('ProgramFiles'), [Environment]::GetEnvironmentVariable('ProgramFiles(x86)'))) {
        if ([string]::IsNullOrWhiteSpace($base)) {
            continue
        }
        Get-ChildItem -Path (Join-Path $base 'PowerShell\*\pwsh.exe') -ErrorAction SilentlyContinue |
            ForEach-Object { $candidates.Add($_.FullName) }
    }
    $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
    if (-not [string]::IsNullOrWhiteSpace($machinePath)) {
        foreach ($dir in $machinePath.Split(';')) {
            if ([string]::IsNullOrWhiteSpace($dir)) {
                continue
            }
            $candidates.Add((Join-Path $dir.Trim() 'pwsh.exe'))
        }
    }
    foreach ($path in $candidates) {
        if ($path -and (Test-Path -LiteralPath $path)) {
            return $path
        }
    }
    return $null
}

function Get-PwshMajorVersion([string]$PwshPath) {
    try {
        $output = & $PwshPath --version 2>$null
        if ("$output" -match '(\d+)\.(\d+)\.(\d+)') {
            return [int]$Matches[1]
        }
    } catch {
    }
    return 0
}

function Show-PwshStatus {
    $servicePwsh = Find-PwshForService
    if ($servicePwsh) {
        $major = Get-PwshMajorVersion $servicePwsh
        if ($major -ge 7) {
            Write-Host "PowerShell 7+ for the LHM bridge (service-visible): $servicePwsh (v$major)"
            return
        }
        if ($major -eq 0) {
            Write-Host "PowerShell (pwsh) for the LHM bridge (service-visible): $servicePwsh"
            return
        }
        Write-Warning "PowerShell at $servicePwsh is v$major; the LHM bridge needs 7+ to host LibreHardwareMonitorLib.dll, so CPU package power and board temperatures may be unavailable."
    } else {
        $userPwsh = Get-Command pwsh -ErrorAction SilentlyContinue
        if ($userPwsh) {
            Write-Warning "pwsh is on your account's PATH ($($userPwsh.Source)) but not the machine PATH or Program Files, so the $ServiceName LocalSystem service cannot see it."
        } else {
            Write-Warning "PowerShell 7+ (pwsh) was not found. The LHM bridge needs it (Windows PowerShell 5.1 cannot host LibreHardwareMonitorLib.dll), so CPU package power and board temperatures will be unavailable."
        }
    }
    Write-Host 'Fix: install PowerShell 7+ machine-wide so the LocalSystem service can use it:'
    Write-Host '  winget install --scope machine Microsoft.PowerShell'
    Write-Host '  # or: choco install powershell-core'
}

function Install-Agent {
    Assert-Admin

    $settingsDir = Split-Path -Parent $SettingsPath
    if ($settingsDir) {
        New-Item -ItemType Directory -Force -Path $settingsDir | Out-Null
    }

    $binaryPath = Get-BinaryPath
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($service) {
        if ($service.Status -ne 'Stopped') {
            Stop-Service -Name $ServiceName -Force
        }
        & sc.exe config $ServiceName binPath= $binaryPath start= auto DisplayName= $DisplayName | Out-Null
    } else {
        New-Service -Name $ServiceName -DisplayName $DisplayName -BinaryPathName $binaryPath -StartupType Automatic | Out-Null
    }
    Set-ServiceRecovery

    try {
        if (-not $NoFirewall) {
            Remove-ServiceFirewallRules
            $ruleName = Get-FirewallRuleName
            New-NetFirewallRule `
                -Name $ruleName `
                -DisplayName "$DisplayName ($Port)" `
                -Direction Inbound `
                -Action Allow `
                -Protocol TCP `
                -LocalPort $Port `
                -Profile Domain,Private | Out-Null
        } else {
            Remove-ServiceFirewallRules
        }
    } catch {
        # A firewall failure must not fail the whole install: the service is
        # already configured and the dashboard still works on this machine. Warn
        # so the user can open TCP $Port by hand if LAN devices cannot reach it.
        Write-Warning "Could not configure the Windows firewall rule for TCP port $Port. The service is still installed; open the port manually if other devices cannot reach the dashboard. Details: $($_.Exception.Message)"
    }

    Start-AgentService
    Wait-AgentReadyAdvisory
    Get-Service -Name $ServiceName
    Show-LhmLibraryStatus
    Show-PwshStatus
    Show-DeviceHandoff
}

function Uninstall-Agent {
    Assert-Admin

    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    if ($service) {
        if ($service.Status -ne 'Stopped') {
            Stop-Service -Name $ServiceName -Force
        }
        & sc.exe delete $ServiceName | Out-Null
    }

    Remove-ServiceFirewallRules
}

function Show-Status {
    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    $serviceInstalled = $false
    if ($service) {
        $serviceInstalled = $true
        $service
        Show-AgentReadiness
        Show-LhmLibraryStatus
        Show-PwshStatus
        Show-DashboardSettings
        Show-ClientCheckStatus
    } else {
        Write-Host "Service $ServiceName is not installed."
    }

    $rules = @(Get-NetFirewallRule -Name "$ServiceName-*" -ErrorAction SilentlyContinue)
    if ($rules.Count -gt 0) {
        $rules | Format-Table -AutoSize Name, Enabled, Profile, Direction, Action
    } else {
        Write-Host "Firewall rules for $ServiceName are not installed."
    }

    if ($serviceInstalled) {
        Show-DeployedDeviceGate
        Show-DeviceHandoff
    }
}

# --- Update engine ----------------------------------------------------------
# Phase 2 of the auto-update plan: a standalone, no-in-app-network-call update
# path that mirrors the in-dashboard self-update (Phase 3) but is fully driven
# from this script. Suitable for a Scheduled Task ("auto-update for power users
# with zero in-app network calls"). The shared swap+rollback contract is:
#   1. Stop the service (releases the exe lock).
#   2. Rename the live exe to *.old.exe (rollback copy).
#   3. Move the verified new exe into place.
#   4. Start the service.
#   5. Poll /readyz; on ready, delete *.old.exe and exit 0.
#   6. On not-ready / start failure, restore *.old.exe, start, fail loudly.
# Phase 3's agent-side detached helper (`sysmon-agent.exe --apply-update`) uses
# the same sequence.

function Get-InstalledExePath {
    # The service's binPath is the authoritative source for the installed exe.
    # sc.exe qc returns "BINARY_PATH_NAME   :  \"C:\\Program Files\\Sysmon Agent\\sysmon-agent.exe\" -bind ...".
    $output = & sc.exe qc $ServiceName 2>$null
    if ($LASTEXITCODE -ne 0) {
        return $null
    }
    foreach ($line in @($output)) {
        $text = [string]$line
        if ($text -notmatch 'BINARY_PATH_NAME') {
            continue
        }
        if ($text -match '"([^"]+sysmon-agent\.exe)"') {
            return $Matches[1]
        }
        if ($text -match '(:\s+)(\S+sysmon-agent\.exe)') {
            return $Matches[2]
        }
    }
    return $null
}

function Get-InstalledVersion {
    param([string]$ExePath)
    if ([string]::IsNullOrWhiteSpace($ExePath) -or -not (Test-Path -LiteralPath $ExePath)) {
        return $null
    }
    try {
        $raw = & $ExePath -version 2>$null
        $version = ([string]::Join("`n", @($raw))).Trim()
        if ($version -match '^v?\d+\.\d+\.\d+') {
            return $version
        }
        return $version
    } catch {
        return $null
    }
}

function ConvertTo-NormalizedVersionTag {
    param([string]$Version)
    $v = [string]$Version
    $v = $v.Trim()
    if ([string]::IsNullOrEmpty($v) -or $v -eq 'dev') {
        return $v
    }
    if (-not $v.StartsWith('v')) {
        $v = 'v' + $v
    }
    return $v
}

function Compare-VersionTag {
    # Returns 1 if $A > $B, -1 if $A < $B, 0 if equal. Falls back to a plain
    # string compare on unparseable tags so a malformed release never claims
    # "newer" than the running agent. Mirrors semver.compareSemver in update.go.
    param([string]$A, [string]$B)
    $a = ConvertTo-NormalizedVersionTag $A
    $b = ConvertTo-NormalizedVersionTag $B
    if ($a -eq $b) { return 0 }
    $aCore, $aPre = ($a -split '-', 2) + @('')
    $bCore, $bPre = ($b -split '-', 2) + @('')
    $aCore = $aCore.TrimStart('v')
    $bCore = $bCore.TrimStart('v')
    $aParts = $aCore.Split('.')
    $bParts = $bCore.Split('.')
    if ($aParts.Length -gt 3 -or $bParts.Length -gt 3 -or $aParts.Length -eq 0 -or $bParts.Length -eq 0) {
        return [string]::Compare($a, $b, [System.StringComparison]::Ordinal)
    }
    for ($i = 0; $i -lt 3; $i++) {
        $aNum = 0
        $bNum = 0
        if (-not [int]::TryParse($aParts[$i], [ref]$aNum) -or -not [int]::TryParse($bParts[$i], [ref]$bNum)) {
            return [string]::Compare($a, $b, [System.StringComparison]::Ordinal)
        }
        if ($aNum -ne $bNum) {
            if ($aNum -gt $bNum) { return 1 }
            return -1
        }
    }
    # A release without a pre-release ranks higher than one with.
    if ([string]::IsNullOrEmpty($aPre) -and [string]::IsNullOrEmpty($bPre)) { return 0 }
    if ([string]::IsNullOrEmpty($aPre)) { return 1 }
    if ([string]::IsNullOrEmpty($bPre)) { return -1 }
    return [string]::Compare($aPre, $bPre, [System.StringComparison]::Ordinal)
}

function Get-LatestRelease {
    param([string]$Repo = 'vantuan5644/sysmon-agent')
    # GitHub requires a User-Agent header on all API calls.
    $headers = @{ 'User-Agent' = 'sysmon-agent-installer'; 'Accept' = 'application/vnd.github+json' }
    $url = "https://api.github.com/repos/$Repo/releases/latest"
    try {
        return (Invoke-RestMethod -Method Get -Uri $url -Headers $headers -TimeoutSec 15), $null
    } catch {
        return $null, $_.Exception.Message
    }
}

function Get-ReleaseForTag {
    param([string]$Repo, [string]$Tag)
    $headers = @{ 'User-Agent' = 'sysmon-agent-installer'; 'Accept' = 'application/vnd.github+json' }
    $url = "https://api.github.com/repos/$Repo/releases/tags/$Tag"
    try {
        return (Invoke-RestMethod -Method Get -Uri $url -Headers $headers -TimeoutSec 15), $null
    } catch {
        return $null, $_.Exception.Message
    }
}

function Find-ReleaseAsset {
    param($Release, [string]$Name)
    foreach ($asset in @($Release.assets)) {
        if ([string]$asset.name -eq $Name) {
            return [string]$asset.browser_download_url
        }
    }
    return $null
}

function Read-PublishedChecksum {
    # Parses a sha256sum-style file and returns the lowercase hex digest for the
    # named asset. Lines look like '<hex>  <name>' (coreutils) but a single
    # space and the '*name' binary-mode marker are tolerated.
    param([byte[]]$Bytes, [string]$AssetName)
    $text = [System.Text.Encoding]::UTF8.GetString($Bytes)
    foreach ($line in $text -split "`n") {
        $trimmed = $line.Trim()
        if ([string]::IsNullOrEmpty($trimmed) -or $trimmed.StartsWith('#')) { continue }
        $fields = $trimmed -split '\s+'
        if ($fields.Length -lt 2) { continue }
        $digest = [string]$fields[0]
        $name = ([string]$fields[1]).TrimStart('*')
        if ($name -eq $AssetName -and $digest -match '^[0-9a-fA-F]{64}$') {
            return $digest.ToLowerInvariant()
        }
    }
    return $null
}

function Invoke-VerifiedSwap {
    # Stop service -> rename live to *.old.exe -> move verified to live -> start
    # service -> poll /readyz -> on failure, rollback from *.old.exe. Returns
    # $true on success, $false (with a written error) on rollback.
    param([string]$LiveExe, [string]$VerifiedExe)
    $backup = $LiveExe + '.old.exe'
    if (Test-Path -LiteralPath $backup) {
        Remove-Item -LiteralPath $backup -Force
    }
    try {
        $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
        if ($null -ne $service -and $service.Status -ne 'Stopped') {
            Stop-Service -Name $ServiceName -Force
        }
        $deadline = (Get-Date).AddSeconds(30)
        do {
            $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
            if ($null -eq $service -or $service.Status -eq 'Stopped') { break }
            Start-Sleep -Milliseconds 500
        } while ((Get-Date) -lt $deadline)

        Move-Item -LiteralPath $LiveExe -Destination $backup -Force
        Copy-Item -LiteralPath $VerifiedExe -Destination $LiveExe -Force
        Remove-Item -LiteralPath $VerifiedExe -Force -ErrorAction SilentlyContinue

        Start-AgentService
        try {
            Wait-AgentReady
        } catch {
            # Ready never came back: the new binary is bad. Roll back.
            Write-Warning "New binary did not become ready; rolling back. Details: $($_.Exception.Message)"
            $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
            if ($null -ne $service -and $service.Status -ne 'Stopped') {
                Stop-Service -Name $ServiceName -Force
            }
            Remove-Item -LiteralPath $LiveExe -Force -ErrorAction SilentlyContinue
            Move-Item -LiteralPath $backup -Destination $LiveExe -Force
            Start-AgentService
            return $false
        }

        Remove-Item -LiteralPath $backup -Force -ErrorAction SilentlyContinue
        return $true
    } catch {
        Write-Warning "Swap failed; attempting rollback. Details: $($_.Exception.Message)"
        if (Test-Path -LiteralPath $backup) {
            try {
                $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
                if ($null -ne $service -and $service.Status -ne 'Stopped') {
                    Stop-Service -Name $ServiceName -Force
                }
                if (-not (Test-Path -LiteralPath $LiveExe)) {
                    Move-Item -LiteralPath $backup -Destination $LiveExe -Force
                }
                Start-AgentService
            } catch {
                Write-Warning "Rollback also failed; the previous binary is at $backup. Details: $($_.Exception.Message)"
            }
        }
        throw
    }
}

function Update-UninstallDisplayVersion {
    # Keep the Add/Remove Programs entry in step with the binary we just
    # installed. The installer wrote this key; without this, Programs and
    # Features keeps advertising the version the user originally installed.
    # Best-effort: an install that did not come from the NSIS installer has no
    # such key, which is not an error.
    param([string]$Version)
    $display = ([string]$Version).TrimStart('v')
    if ([string]::IsNullOrWhiteSpace($display)) {
        return
    }
    foreach ($key in @(
            'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\Sysmon',
            'HKLM:\Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\Sysmon')) {
        try {
            if (Test-Path -LiteralPath $key) {
                Set-ItemProperty -LiteralPath $key -Name 'DisplayVersion' -Value $display -ErrorAction Stop
                Write-Host "Updated Add/Remove Programs version to $display."
            }
        } catch {
            Write-Warning "Could not update the Add/Remove Programs version at $key. Details: $($_.Exception.Message)"
        }
    }
}

function Update-Agent {
    Assert-Admin

    $liveExe = Get-InstalledExePath
    if ([string]::IsNullOrWhiteSpace($liveExe)) {
        throw "Service $ServiceName is not installed (or its binPath could not be read). Run -Action Install first."
    }
    $installDir = Split-Path -Parent $liveExe

    $currentVersion = Get-InstalledVersion -ExePath $liveExe
    Write-Host "Installed: $liveExe"
    Write-Host "Current version: $(if ([string]::IsNullOrWhiteSpace($currentVersion)) { 'unknown' } else { $currentVersion })"

    $repo = 'vantuan5644/sysmon-agent'
    if ([string]::IsNullOrWhiteSpace($UpdateVersion)) {
        $release, $netErr = Get-LatestRelease -Repo $repo
        $releaseKind = 'latest'
    } else {
        $wantedTag = ConvertTo-NormalizedVersionTag $UpdateVersion
        $release, $netErr = Get-ReleaseForTag -Repo $repo -Tag $wantedTag
        $releaseKind = "tagged ($wantedTag)"
    }
    if ($null -eq $release) {
        $detail = if ([string]::IsNullOrWhiteSpace($netErr)) { 'GitHub returned no release' } else { $netErr }
        Write-Warning "Could not fetch $releaseKind release from $repo. The agent is unchanged. Details: $detail"
        return
    }

    $latestTag = ConvertTo-NormalizedVersionTag $release.tag_name
    $releaseURL = [string]$release.html_url
    Write-Host "Channel ($releaseKind): $latestTag - $releaseURL"

    if (-not $Force) {
        if ([string]::IsNullOrEmpty($currentVersion) -or $currentVersion -eq 'dev') {
            Write-Warning "Installed version is unknown/dev; refusing to update without -Force. Re-run with -Force to apply $latestTag anyway."
            return
        }
        $cmp = Compare-VersionTag -A $latestTag -B $currentVersion
        if ($cmp -le 0) {
            Write-Host "Up to date ($currentVersion). Use -Force to reapply $latestTag."
            return
        }
    }

    $binaryURL = Find-ReleaseAsset -Release $release -Name 'sysmon-agent.exe'
    $checksumURL = Find-ReleaseAsset -Release $release -Name 'SHA256SUMS.txt'
    if ([string]::IsNullOrWhiteSpace($binaryURL)) {
        throw "Release $latestTag is missing the sysmon-agent.exe asset. Cannot update."
    }
    if ([string]::IsNullOrWhiteSpace($checksumURL)) {
        throw "Release $latestTag is missing the SHA256SUMS.txt asset; the download cannot be authenticated. Cannot update."
    }

    $downloadExe = Join-Path $installDir "sysmon-agent.new.exe"
    $downloadChecksums = Join-Path $installDir "SHA256SUMS.downloaded.txt"
    if ($DryRun) {
        Write-Host "Dry run: would download $binaryURL -> $downloadExe"
        Write-Host "Dry run: would download $checksumURL -> $downloadChecksums"
        Write-Host "Dry run: would verify SHA-256 against SHA256SUMS.txt entry for sysmon-agent.exe"
        Write-Host "Dry run: would stop service, swap $liveExe, start service, poll /readyz (rollback on failure)"
        return
    }

    Write-Host "Downloading $binaryURL"
    Invoke-WebRequest -Method Get -Uri $binaryURL -OutFile $downloadExe -UseBasicParsing
    Invoke-WebRequest -Method Get -Uri $checksumURL -OutFile $downloadChecksums -UseBasicParsing

    $checksumsBytes = [System.IO.File]::ReadAllBytes($downloadChecksums)
    $expected = Read-PublishedChecksum -Bytes $checksumsBytes -AssetName 'sysmon-agent.exe'
    if ([string]::IsNullOrEmpty($expected)) {
        Remove-Item -LiteralPath $downloadExe -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $downloadChecksums -Force -ErrorAction SilentlyContinue
        throw "SHA256SUMS.txt has no entry for sysmon-agent.exe; cannot authenticate the download. Aborted."
    }
    $actual = (Get-FileHash -LiteralPath $downloadExe -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actual -ne $expected) {
        Remove-Item -LiteralPath $downloadExe -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $downloadChecksums -Force -ErrorAction SilentlyContinue
        throw "Checksum mismatch: expected $expected, got $actual. Download discarded; the agent is unchanged."
    }
    Write-Host "Checksum verified: $actual"

    $ok = Invoke-VerifiedSwap -LiveExe $liveExe -VerifiedExe $downloadExe
    Remove-Item -LiteralPath $downloadChecksums -Force -ErrorAction SilentlyContinue
    if (-not $ok) {
        throw "Update to $latestTag failed readiness after swap and was rolled back. The previous binary is running."
    }

    $newVersion = Get-InstalledVersion -ExePath $liveExe
    Update-UninstallDisplayVersion -Version $latestTag
    Write-Host "Update applied: now running $(if ([string]::IsNullOrWhiteSpace($newVersion)) { 'unknown' } else { $newVersion })."
    Get-Service -Name $ServiceName
    Show-LhmLibraryStatus
    Show-PwshStatus
    Show-DashboardSettings
}


try {
    switch ($Action) {
        'Install' { Install-Agent }
        'Uninstall' { Uninstall-Agent }
        'Status' { Show-Status }
        'Update' { Update-Agent }
    }
} catch {
    # Surface a clear, non-zero exit only for genuine failures (cannot register
    # or start the service, missing admin/binary). Firewall and readiness are
    # already downgraded to warnings inside Install-Agent, so a healthy-but-slow
    # install no longer reports failure to the NSIS post-install step.
    Write-Error $_
    exit 1
}
exit 0
