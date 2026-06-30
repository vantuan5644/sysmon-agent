param(
    [ValidateSet('Install', 'Uninstall', 'Status')]
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
    [switch]$NoFirewall
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

try {
    switch ($Action) {
        'Install' { Install-Agent }
        'Uninstall' { Uninstall-Agent }
        'Status' { Show-Status }
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
