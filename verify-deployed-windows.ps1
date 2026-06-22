param(
    [string]$BaseUrl = '',
    [string]$DeviceUrl = '',
    [ValidateRange(0, 86400)]
    [int]$HoldSeconds = 120,
    [bool]$RequireDevice = $true,
    [bool]$RequireStandalone = $true,
    [bool]$RequireInteraction = $true,
    [string]$ReportPath = ''
)

$ErrorActionPreference = 'Stop'
$script:ExpectedDashboardBuild = ''

function Normalize-BaseUrl([string]$Url) {
    while ($Url.EndsWith('/')) {
        $Url = $Url.Substring(0, $Url.Length - 1)
    }
    return $Url
}

function Resolve-BoolEnv([string]$Name, [bool]$Default) {
    $value = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrWhiteSpace($value)) {
        return $Default
    }
    switch ($value.Trim().ToLowerInvariant()) {
        '0' { return $false }
        'false' { return $false }
        'no' { return $false }
        '1' { return $true }
        'true' { return $true }
        'yes' { return $true }
        default { throw "$Name must be 0/1, true/false, or yes/no" }
    }
}

function Resolve-IntEnv([string]$Name, [int]$Default) {
    $value = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrWhiteSpace($value)) {
        return $Default
    }
    $parsed = 0
    if (-not [int]::TryParse($value, [ref]$parsed) -or $parsed -lt 0) {
        throw "$Name must be a non-negative number"
    }
    return $parsed
}

function Invoke-Text([string]$Url) {
    return (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri $Url).Content
}

function Invoke-Headers([string]$Url) {
    return (Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri $Url).Headers
}

function Assert-HeaderContains([string]$Url, [string]$Name, [string]$Expected) {
    $headers = Invoke-Headers $Url
    $value = [string]$headers[$Name]
    if (-not $value.Contains($Expected)) {
        throw "expected $Url header $Name to contain $Expected, got '$value'"
    }
}

function Write-Report([string]$Value) {
    Add-Content -LiteralPath $ReportPath -Value $Value
}

function Write-ClientCheckReport($ClientCheck) {
    foreach ($field in @('last_seen', 'dashboard_build', 'interaction', 'user_agent', 'viewport_width', 'viewport_height', 'screen_width', 'screen_height', 'device_pixel_ratio', 'touch_points', 'display_mode', 'standalone', 'visibility', 'orientation')) {
        if ($null -ne $ClientCheck.$field -and [string]$ClientCheck.$field -ne '') {
            Write-Report "device_client_${field}=$($ClientCheck.$field)"
        }
    }
}

function Write-ReadinessCollectionErrors([string]$Prefix, $Ready) {
    $errors = @()
    if ($null -ne $Ready.collection_errors) {
        $errors = @($Ready.collection_errors |
            Where-Object { -not [string]::IsNullOrWhiteSpace([string]$_) } |
            ForEach-Object { ([string]$_).Trim() -replace '\s+', ' ' })
    }
    if ($errors.Count -eq 0) {
        Write-Report "${Prefix}_collection_errors=none"
        return
    }
    Write-Report "${Prefix}_collection_errors=$($errors.Count)"
    for ($i = 0; $i -lt $errors.Count; $i++) {
        Write-Report "${Prefix}_collection_error_$($i + 1)=$($errors[$i])"
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

function Test-InteractionClientCheckEvidence($ClientCheck) {
    return ([string]$ClientCheck.interaction).ToLowerInvariant() -eq 'status_strip_tap'
}

function Test-CurrentDashboardBuildEvidence($ClientCheck) {
    if ([string]::IsNullOrWhiteSpace($script:ExpectedDashboardBuild)) {
        return $true
    }
    return [string]$ClientCheck.dashboard_build -eq $script:ExpectedDashboardBuild
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

function Test-FreshClientCheck($ClientCheck) {
    $lastSeenMs = Get-TimeMilliseconds $ClientCheck.last_seen
    if ($null -eq $lastSeenMs) {
        return $false
    }
    if ($null -eq $script:HoldStartedMs -or $lastSeenMs -lt $script:HoldStartedMs) {
        return $false
    }
    if ($null -ne $script:InitialClientLastSeenMs -and $lastSeenMs -le $script:InitialClientLastSeenMs) {
        return $false
    }
    return $true
}

function Write-ClientCheckResult($ClientCheck, [bool]$RequireDevice, [bool]$RequireStandalone, [bool]$RequireInteraction) {
    if ($null -eq $ClientCheck -or -not $ClientCheck.seen) {
        Write-Host 'No dashboard client check was observed during the deployed hold.'
        Write-Report 'device_client_seen=not_observed'
        if ($RequireDevice) {
            throw 'required device client check was not observed'
        }
        return
    }

    if ((Test-FreshClientCheck $ClientCheck) -and (Test-DeviceClientCheckEvidence $ClientCheck)) {
        Write-ClientCheckReport $ClientCheck
        if (-not (Test-CurrentDashboardBuildEvidence $ClientCheck)) {
            Write-Host 'Observed a fresh device dashboard client check, but it was not running the current dashboard build.'
            Write-Report 'device_client_seen=stale_dashboard_build'
            throw 'required current dashboard build was not observed on the device client'
        }
        if (Test-StandaloneClientCheckEvidence $ClientCheck) {
            if ($RequireInteraction -and -not (Test-InteractionClientCheckEvidence $ClientCheck)) {
                Write-Host 'Observed a fresh device Home Screen dashboard client check, but it did not include the required status-strip tap.'
                Write-Report 'device_client_seen=not_interactive'
                throw 'required device status-strip tap was not observed'
            }
            if (Test-InteractionClientCheckEvidence $ClientCheck) {
                Write-Host 'Observed a fresh device Home Screen status-strip tap during the deployed hold.'
            } else {
                Write-Host 'Observed a fresh device Home Screen dashboard client check during the deployed hold.'
            }
            Write-Report 'device_client_seen=pass'
            $script:InstalledDeviceHomeScreen = 'pass'
            return
        }
        if ($RequireStandalone) {
            Write-Host 'Observed a fresh device dashboard client check, but it was not in Home Screen standalone mode.'
            Write-Report 'device_client_seen=not_standalone'
            throw 'required Home Screen standalone client check was not observed'
        }
        if ($RequireInteraction -and -not (Test-InteractionClientCheckEvidence $ClientCheck)) {
            Write-Host 'Observed a fresh device dashboard client check, but it did not include the required status-strip tap.'
            Write-Report 'device_client_seen=not_interactive'
            throw 'required device status-strip tap was not observed'
        }
        Write-Host 'Observed a fresh device dashboard client check during the deployed hold, but it was not in Home Screen standalone mode.'
        Write-Report 'device_client_seen=pass'
        return
    }

    if (Test-DeviceClientCheckEvidence $ClientCheck) {
        Write-ClientCheckReport $ClientCheck
        Write-Host 'Observed a device dashboard client check, but it was not fresh for this run.'
        Write-Report 'device_client_seen=stale_client'
        if ($RequireDevice) {
            throw 'required fresh device client check was not observed'
        }
        return
    }

    if (-not (Test-FreshClientCheck $ClientCheck)) {
        Write-Host 'No dashboard client check was observed during the deployed hold.'
        Write-Report 'device_client_seen=not_observed'
        if ($RequireDevice) {
            throw 'required device client check was not observed'
        }
        return
    }

    Write-ClientCheckReport $ClientCheck
    Write-Host 'Observed a dashboard client check, but it did not look like a device client.'
    Write-Report 'device_client_seen=unexpected_client'
    if ($RequireDevice) {
        throw 'required device client check was not observed'
    }
}

function Get-ClientCheckEntries([string]$BaseUrl) {
    $entries = @()
    $payload = $null
    try {
        $payload = Invoke-RestMethod -Method Get -Uri "$BaseUrl/api/client-checks" -TimeoutSec 5
    } catch {
        try {
            $payload = Invoke-RestMethod -Method Get -Uri "$BaseUrl/api/client-check" -TimeoutSec 5
        } catch {
            $payload = $null
        }
    }

    if ($null -ne $payload) {
        if ($null -ne $payload.checks) {
            $entries += @($payload.checks)
        } else {
            $entries += $payload
        }
    }

    $entries += Get-StatusClientCheckEntries $BaseUrl
    return @($entries)
}

function Get-StatusClientCheckEntries([string]$BaseUrl) {
    try {
        $status = Invoke-RestMethod -Method Get -Uri "$BaseUrl/api/status" -TimeoutSec 5
    } catch {
        return @()
    }

    $entries = @()
    if ($null -ne $status.device_client_check) {
        $entries += $status.device_client_check
    }
    if ($null -ne $status.client_check) {
        $entries += $status.client_check
    }
    return @($entries)
}

function Get-LatestSeenClientCheck($ClientChecks, [bool]$DeviceOnly = $false) {
    $latest = $null
    $latestMs = $null
    foreach ($clientCheck in @($ClientChecks)) {
        if ($null -eq $clientCheck -or -not $clientCheck.seen) {
            continue
        }
        if ($DeviceOnly -and -not (Test-DeviceClientCheckEvidence $clientCheck)) {
            continue
        }
        $lastSeenMs = Get-TimeMilliseconds $clientCheck.last_seen
        if ($null -eq $lastSeenMs) {
            continue
        }
        if ($null -eq $latestMs -or $lastSeenMs -gt $latestMs) {
            $latest = $clientCheck
            $latestMs = $lastSeenMs
        }
    }
    return [pscustomobject]@{
        ClientCheck = $latest
        LastSeenMs = $latestMs
    }
}

function Wait-ForClientCheckEvidence([string]$BaseUrl, [int]$HoldSeconds, [bool]$RequireDevice, [bool]$RequireStandalone, [bool]$RequireInteraction) {
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($HoldSeconds)
    $lastSeenClientCheck = $null
    $lastDeviceClientCheck = $null

    while ($true) {
        $pollSeenClientCheck = $null
        $pollDeviceClientCheck = $null
        foreach ($clientCheck in @(Get-ClientCheckEntries $BaseUrl)) {
            if ($null -eq $clientCheck -or -not $clientCheck.seen) {
                continue
            }
            if ((Test-FreshClientCheck $clientCheck) -and $null -eq $pollSeenClientCheck) {
                $pollSeenClientCheck = $clientCheck
            }
            if ((Test-DeviceClientCheckEvidence $clientCheck) -and $null -eq $pollDeviceClientCheck) {
                $pollDeviceClientCheck = $clientCheck
            }
            if ((Test-FreshClientCheck $clientCheck) -and (Test-DeviceClientCheckEvidence $clientCheck) -and (Test-CurrentDashboardBuildEvidence $clientCheck)) {
                if (-not $RequireStandalone -or (Test-StandaloneClientCheckEvidence $clientCheck)) {
                    if (-not $RequireInteraction -or (Test-InteractionClientCheckEvidence $clientCheck)) {
                        Write-ClientCheckResult $clientCheck $RequireDevice $RequireStandalone $RequireInteraction
                        return
                    }
                }
            }
        }
        if ($null -ne $pollSeenClientCheck) {
            $lastSeenClientCheck = $pollSeenClientCheck
        }
        if ($null -ne $pollDeviceClientCheck) {
            $lastDeviceClientCheck = $pollDeviceClientCheck
        }

        $remaining = ($deadline - [DateTimeOffset]::UtcNow).TotalSeconds
        if ($remaining -le 0) {
            break
        }
        Start-Sleep -Seconds ([Math]::Min(2, [Math]::Max(1, [int][Math]::Ceiling($remaining))))
    }

    if ($null -ne $lastDeviceClientCheck) {
        Write-ClientCheckResult $lastDeviceClientCheck $RequireDevice $RequireStandalone $RequireInteraction
    } elseif ($null -ne $lastSeenClientCheck) {
        Write-ClientCheckResult $lastSeenClientCheck $RequireDevice $RequireStandalone $RequireInteraction
    } else {
        Write-ClientCheckResult $null $RequireDevice $RequireStandalone $RequireInteraction
    }
}

function Get-TailscaleHostName {
    if (-not (Get-Command tailscale -ErrorAction SilentlyContinue)) {
        return ''
    }
    try {
        $status = (& tailscale status --self --json 2>$null) | ConvertFrom-Json
        if ($status.Self.DNSName) {
            return ([string]$status.Self.DNSName).TrimEnd('.')
        }
    } catch {
    }
    return ''
}

function Get-UrlHost([string]$HostName) {
    $hostValue = $HostName.Trim('[', ']')
    if ($hostValue.Contains(':')) {
        return "[$hostValue]"
    }
    return $hostValue
}

function Get-BaseUrlPart([string]$Url, [string]$Part) {
    $uri = [Uri]$Url
    switch ($Part) {
        'scheme' { return $uri.Scheme }
        'port' {
            if ($uri.Port -gt 0) {
                return [string]$uri.Port
            }
            if ($uri.Scheme -eq 'https') {
                return '443'
            }
            return '80'
        }
        default { throw "unsupported URL part: $Part" }
    }
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

function Get-CandidatePriority([string]$HostName) {
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
                    Priority = Get-CandidatePriority $hostValue
                    Host = $hostValue
                }
            }
        } |
        Sort-Object Priority, Host |
        ForEach-Object { $_.Host }
}

function Get-DirectLanDeviceUrl([string]$BaseUrl) {
    $hostName = @(Get-CandidateDeviceHosts | Select-Object -First 1)[0]
    if ([string]::IsNullOrWhiteSpace($hostName)) {
        return ''
    }
    $scheme = Get-BaseUrlPart $BaseUrl 'scheme'
    $port = Get-BaseUrlPart $BaseUrl 'port'
    return "${scheme}://$(Get-UrlHost $hostName):$port"
}

function Get-DetectedDeviceUrl {
    if (-not [string]::IsNullOrWhiteSpace($DeviceUrl)) {
        $script:DeviceUrlSource = 'explicit'
        return Normalize-BaseUrl $DeviceUrl
    }
    $hostName = Get-TailscaleHostName
    if ([string]::IsNullOrWhiteSpace($hostName)) {
        $lanUrl = Get-DirectLanDeviceUrl $BaseUrl
        if (-not [string]::IsNullOrWhiteSpace($lanUrl)) {
            $script:DeviceUrlSource = 'direct_lan'
        }
        return $lanUrl
    }
    $script:DeviceUrlSource = 'tailscale'
    $httpsPort = if ($env:SYSMON_HTTPS) { $env:SYSMON_HTTPS } else { '9443' }
    if ($httpsPort -eq '443') {
        return "https://$hostName"
    }
    return "https://${hostName}:$httpsPort"
}

function Test-PwaInstallAssets([string]$Url) {
    $html = Invoke-Text "$Url/"
    if ($html -notmatch '<title>Sysmon</title>') {
        throw 'dashboard HTML missing Sysmon title'
    }
    foreach ($required in @(
        '<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">',
        '<meta name="apple-mobile-web-app-capable" content="yes">',
        '<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">',
        '<meta name="apple-mobile-web-app-title" content="Sysmon">'
    )) {
        if (-not $html.Contains($required)) {
            throw "dashboard HTML missing PWA install metadata: $required"
        }
    }
    if ($html -notmatch '<link rel="manifest" href="/manifest.json">') {
        throw 'dashboard HTML missing manifest link'
    }
    if ($html -notmatch '<link rel="apple-touch-icon" sizes="180x180" href="/icon-180.png">') {
        throw 'dashboard HTML missing Apple touch icon'
    }

    $manifest = Invoke-Text "$Url/manifest.json"
    foreach ($pattern in @(
        '"short_name"\s*:\s*"Sysmon"',
        '"id"\s*:\s*"/"',
        '"display"\s*:\s*"standalone"',
        '"start_url"\s*:\s*"/"',
        '"scope"\s*:\s*"/"',
        '"background_color"\s*:\s*"#080b10"',
        '"theme_color"\s*:\s*"#080b10"',
        '"src"\s*:\s*"/icon-180.png"',
        '"src"\s*:\s*"/icon-512.png"'
    )) {
        if ($manifest -notmatch $pattern) {
            throw "manifest missing expected install metadata: $pattern"
        }
    }

    $app = Invoke-Text "$Url/app.js"
    if ($app -notmatch 'fetchMetrics' -or $app -notmatch 'clientCheckPayload') {
        throw 'app.js missing dashboard/client-check logic'
    }

    $sw = Invoke-Text "$Url/sw.js"
    if ($sw -notmatch 'isLiveEndpoint' -or $sw -notmatch '"/manifest.json"' -or $sw -notmatch '"/icon-180.png"' -or $sw -notmatch '"/icon-512.png"') {
        throw 'service worker missing PWA cache policy'
    }

    Test-PngDimensions "$Url/icon-180.png" 180
    Test-PngDimensions "$Url/icon-512.png" 512
}

function Test-PngDimensions([string]$Url, [int]$ExpectedSize) {
    $tempFile = [System.IO.Path]::GetTempFileName()
    try {
        Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Uri $Url -OutFile $tempFile
        $bytes = [System.IO.File]::ReadAllBytes($tempFile)
        $signature = [byte[]](0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a)
        if ($bytes.Length -lt 24) {
            throw "$Url is too small to be a PNG"
        }
        for ($i = 0; $i -lt $signature.Length; $i++) {
            if ($bytes[$i] -ne $signature[$i]) {
                throw "$Url is not a PNG"
            }
        }
        $chunkType = [System.Text.Encoding]::ASCII.GetString($bytes, 12, 4)
        if ($chunkType -ne 'IHDR') {
            throw "$Url has no PNG IHDR header"
        }
        $width = [System.Net.IPAddress]::NetworkToHostOrder([BitConverter]::ToInt32($bytes, 16))
        $height = [System.Net.IPAddress]::NetworkToHostOrder([BitConverter]::ToInt32($bytes, 20))
        if ($width -ne $ExpectedSize -or $height -ne $ExpectedSize) {
            throw "$Url is ${width}x${height}, want ${ExpectedSize}x${ExpectedSize}"
        }
    } finally {
        Remove-Item -LiteralPath $tempFile -Force -ErrorAction SilentlyContinue
    }
}

function Test-SettingsSameOriginRoundTrip([string]$Url) {
    $settings = Invoke-RestMethod -Method Get -Uri "$Url/api/settings" -TimeoutSec 5
    $bodyObject = [ordered]@{
        dim = [bool]$settings.dim
        shift = [bool]$settings.shift
        refresh_ms = [int]$settings.refresh_ms
        panel = [string]$settings.panel
        thresholds = [ordered]@{
            cpu_warn = [int]$settings.thresholds.cpu_warn
            memory_warn = [int]$settings.thresholds.memory_warn
            disk_warn = [int]$settings.thresholds.disk_warn
            gpu_warn = [int]$settings.thresholds.gpu_warn
            temp_warn_c = [int]$settings.thresholds.temp_warn_c
        }
    }
    $body = $bodyObject | ConvertTo-Json -Compress
    $posted = Invoke-RestMethod `
        -Method Post `
        -Uri "$Url/api/settings" `
        -Headers @{ Origin = $Url } `
        -ContentType 'application/json' `
        -Body $body `
        -TimeoutSec 5

    foreach ($key in @('dim', 'shift', 'refresh_ms', 'panel')) {
        if ([string]$posted.$key -ne [string]$bodyObject[$key]) {
            throw "settings $key roundtrip mismatch: got $($posted.$key), want $($bodyObject[$key])"
        }
    }
    foreach ($key in @('cpu_warn', 'memory_warn', 'disk_warn', 'gpu_warn', 'temp_warn_c')) {
        $expectedThresholds = $bodyObject['thresholds']
        if ([int]$posted.thresholds.$key -ne [int]$expectedThresholds[$key]) {
            throw "settings thresholds.$key roundtrip mismatch: got $($posted.thresholds.$key), want $($expectedThresholds[$key])"
        }
    }

    $statusAfterSettings = Invoke-RestMethod -Method Get -Uri "$Url/api/status" -TimeoutSec 5
    $statusSettings = $statusAfterSettings.settings
    foreach ($key in @('dim', 'shift', 'refresh_ms', 'panel')) {
        if ([string]$statusSettings.$key -ne [string]$bodyObject[$key]) {
            throw "settings status.$key roundtrip mismatch: got $($statusSettings.$key), want $($bodyObject[$key])"
        }
    }
    foreach ($key in @('cpu_warn', 'memory_warn', 'disk_warn', 'gpu_warn', 'temp_warn_c')) {
        $expectedThresholds = $bodyObject['thresholds']
        if ([int]$statusSettings.thresholds.$key -ne [int]$expectedThresholds[$key]) {
            throw "settings status.thresholds.$key roundtrip mismatch: got $($statusSettings.thresholds.$key), want $($expectedThresholds[$key])"
        }
    }
}

function Test-ClientCheckSameOriginRoundTrip([string]$Url) {
    $bodyObject = [ordered]@{
        dashboard_build = $script:ExpectedDashboardBuild
        interaction = 'status_strip_tap'
        viewport_width = 390
        viewport_height = 844
        screen_width = 390
        screen_height = 844
        device_pixel_ratio = 3
        touch_points = 5
        display_mode = 'browser'
        standalone = $false
        visibility = 'visible'
        orientation = 'verify-public'
    }
    $body = $bodyObject | ConvertTo-Json -Compress
    $posted = Invoke-RestMethod `
        -Method Post `
        -Uri "$Url/api/client-check" `
        -Headers @{ Origin = $Url } `
        -ContentType 'application/json' `
        -Body $body `
        -TimeoutSec 5

    if ($posted.seen -ne $true) {
        throw 'client-check roundtrip did not mark the client as seen'
    }
    foreach ($key in @('dashboard_build', 'interaction', 'viewport_width', 'viewport_height', 'screen_width', 'screen_height', 'device_pixel_ratio', 'touch_points', 'display_mode', 'standalone', 'visibility', 'orientation')) {
        if ([string]$posted.$key -ne [string]$bodyObject[$key]) {
            throw "client-check $key roundtrip mismatch: got $($posted.$key), want $($bodyObject[$key])"
        }
    }
    if ($null -eq (Get-TimeMilliseconds $posted.last_seen)) {
        throw 'client-check roundtrip did not return a valid last_seen timestamp'
    }

    $statusAfterClientCheck = Invoke-RestMethod -Method Get -Uri "$Url/api/status" -TimeoutSec 5
    $statusClientCheck = $statusAfterClientCheck.client_check
    if ($statusClientCheck.seen -ne $true) {
        throw 'status client_check did not mark the client as seen'
    }
    foreach ($key in @('dashboard_build', 'interaction', 'viewport_width', 'viewport_height', 'screen_width', 'screen_height', 'device_pixel_ratio', 'touch_points', 'display_mode', 'standalone', 'visibility', 'orientation')) {
        if ([string]$statusClientCheck.$key -ne [string]$bodyObject[$key]) {
            throw "status client_check $key roundtrip mismatch: got $($statusClientCheck.$key), want $($bodyObject[$key])"
        }
    }
    if ($null -eq (Get-TimeMilliseconds $statusClientCheck.last_seen)) {
        throw 'status client_check roundtrip did not return a valid last_seen timestamp'
    }
}

if (-not $PSBoundParameters.ContainsKey('BaseUrl')) {
    if ($env:SYSMON_DEPLOY_VERIFY_BASE_URL) {
        $BaseUrl = $env:SYSMON_DEPLOY_VERIFY_BASE_URL
    } elseif ($env:SYSMON_VERIFY_BASE_URL) {
        $BaseUrl = $env:SYSMON_VERIFY_BASE_URL
    } else {
        $port = if ($env:SYSMON_PORT) { $env:SYSMON_PORT } else { '9099' }
        $BaseUrl = "http://127.0.0.1:$port"
    }
}
if (-not $PSBoundParameters.ContainsKey('DeviceUrl')) {
    if ($env:SYSMON_DEPLOY_VERIFY_DEVICE_URL) {
        $DeviceUrl = $env:SYSMON_DEPLOY_VERIFY_DEVICE_URL
    } elseif ($env:SYSMON_VERIFY_DEVICE_URL) {
        $DeviceUrl = $env:SYSMON_VERIFY_DEVICE_URL
    }
}
if (-not $PSBoundParameters.ContainsKey('HoldSeconds')) {
    $HoldSeconds = Resolve-IntEnv 'SYSMON_DEPLOY_VERIFY_HOLD_SECONDS' (Resolve-IntEnv 'SYSMON_DEPLOY_VERIFY_HOLD' 120)
}
if (-not $PSBoundParameters.ContainsKey('RequireDevice')) {
    $RequireDevice = Resolve-BoolEnv 'SYSMON_DEPLOY_VERIFY_REQUIRE_DEVICE' (Resolve-BoolEnv 'SYSMON_VERIFY_REQUIRE_DEVICE' $true)
}
if (-not $PSBoundParameters.ContainsKey('RequireStandalone')) {
    $RequireStandalone = Resolve-BoolEnv 'SYSMON_DEPLOY_VERIFY_REQUIRE_STANDALONE' $true
}
if (-not $PSBoundParameters.ContainsKey('RequireInteraction')) {
    $RequireInteraction = Resolve-BoolEnv 'SYSMON_DEPLOY_VERIFY_REQUIRE_INTERACTION' $true
}
if (-not $PSBoundParameters.ContainsKey('ReportPath')) {
    $ReportPath = if ($env:SYSMON_DEPLOY_VERIFY_REPORT) { $env:SYSMON_DEPLOY_VERIFY_REPORT } else { Join-Path ([System.IO.Path]::GetTempPath()) 'sysmon-agent-deployed-verify-report.txt' }
}

$BaseUrl = Normalize-BaseUrl $BaseUrl
$script:HoldStartedMs = $null
$script:InitialClientLastSeenMs = $null
$script:DeviceUrlSource = ''
$script:InstalledDeviceHomeScreen = 'not_verified'
$Succeeded = $false

try {
    $ReportDir = [System.IO.Path]::GetDirectoryName($ReportPath)
    if (-not [string]::IsNullOrWhiteSpace($ReportDir)) {
        New-Item -ItemType Directory -Force -Path $ReportDir | Out-Null
    }
    Set-Content -LiteralPath $ReportPath -Value @(
        'sysmon-agent deployed verification report'
        "started_at=$((Get-Date).ToString('o'))"
        'mode=deployed'
        "local_base_url=$BaseUrl"
        "hold_seconds=$HoldSeconds"
        "require_device=$($RequireDevice.ToString().ToLowerInvariant())"
        "require_standalone=$($RequireStandalone.ToString().ToLowerInvariant())"
        "require_interaction=$($RequireInteraction.ToString().ToLowerInvariant())"
    )

    Set-Location $PSScriptRoot

    Write-Host "Checking deployed sysmon-agent at $BaseUrl..."
    $health = Invoke-RestMethod -Method Get -Uri "$BaseUrl/healthz" -TimeoutSec 5
    if ($health.status -ne 'ok') {
        throw "unexpected health response: $($health | ConvertTo-Json -Compress)"
    }
    Write-Report 'healthz=pass'

    $ready = Invoke-RestMethod -Method Get -Uri "$BaseUrl/readyz" -TimeoutSec 8
    if ($ready.status -ne 'ok' -or -not $ready.metrics) {
        throw "unexpected readiness response: $($ready | ConvertTo-Json -Compress)"
    }
    Write-Report 'readyz=pass'
    Write-ReadinessCollectionErrors 'readyz' $ready

    Assert-HeaderContains "$BaseUrl/api/status" 'Cache-Control' 'no-store'
    Assert-HeaderContains "$BaseUrl/api/status" 'Content-Security-Policy' "default-src 'self'"
    Assert-HeaderContains "$BaseUrl/" 'Cache-Control' 'no-cache'
    Assert-HeaderContains "$BaseUrl/" 'X-Content-Type-Options' 'nosniff'
    Write-Report 'headers=pass'

    if (-not (Get-Command node -ErrorAction SilentlyContinue)) {
        throw 'node is required for deployed API schema validation'
    }
    Write-Host 'Checking deployed API schema...'
    & node verify-api.mjs $BaseUrl
    if ($LASTEXITCODE -ne 0) {
        throw 'deployed API schema validation failed'
    }
    Write-Report 'api_schema=pass'

    Write-Host 'Checking deployed dashboard/PWA install assets...'
    Test-PwaInstallAssets $BaseUrl
    Write-Report 'dashboard_assets=pass'

    $phoneUrl = Get-DetectedDeviceUrl
    if ([string]::IsNullOrWhiteSpace($phoneUrl)) {
        Write-Report 'device_url=unavailable'
        throw 'Could not determine device URL. Set -DeviceUrl/SYSMON_DEPLOY_VERIFY_DEVICE_URL or bind the service to a LAN address.'
    }
    $phoneUrl = Normalize-BaseUrl $phoneUrl
    Write-Report "device_url=$phoneUrl"
    if (-not [string]::IsNullOrWhiteSpace($script:DeviceUrlSource)) {
        Write-Report "device_url_source=$script:DeviceUrlSource"
    }

    Write-Host "Checking published device URL ($($script:DeviceUrlSource)): $phoneUrl"
    $publicHealth = Invoke-RestMethod -Method Get -Uri "$phoneUrl/healthz" -TimeoutSec 5
    if ($publicHealth.status -ne 'ok') {
        throw "unexpected published health response: $($publicHealth | ConvertTo-Json -Compress)"
    }
    Write-Report 'public_healthz=pass'

    $publicReady = Invoke-RestMethod -Method Get -Uri "$phoneUrl/readyz" -TimeoutSec 8
    if ($publicReady.status -ne 'ok' -or -not $publicReady.metrics) {
        throw "unexpected published readiness response: $($publicReady | ConvertTo-Json -Compress)"
    }
    Write-Report 'public_readyz=pass'
    Write-ReadinessCollectionErrors 'public_readyz' $publicReady

    Assert-HeaderContains "$phoneUrl/api/status" 'Cache-Control' 'no-store'
    Assert-HeaderContains "$phoneUrl/api/status" 'Content-Security-Policy' "default-src 'self'"
    Assert-HeaderContains "$phoneUrl/" 'Cache-Control' 'no-cache'
    Assert-HeaderContains "$phoneUrl/" 'X-Content-Type-Options' 'nosniff'
    Write-Report 'public_headers=pass'

    Write-Host 'Checking published API schema...'
    & node verify-api.mjs $phoneUrl
    if ($LASTEXITCODE -ne 0) {
        throw 'published API schema validation failed'
    }
    Write-Report 'public_api_schema=pass'

    $publicStatus = Invoke-RestMethod -Method Get -Uri "$phoneUrl/api/status" -TimeoutSec 5
    $script:ExpectedDashboardBuild = [string]$publicStatus.dashboard_build
    if ([string]::IsNullOrWhiteSpace($script:ExpectedDashboardBuild)) {
        Write-Report 'dashboard_build=unavailable'
        throw 'published /api/status did not report dashboard_build'
    }
    Write-Report "dashboard_build=$script:ExpectedDashboardBuild"

    Write-Host 'Checking published interactive settings controls...'
    Test-SettingsSameOriginRoundTrip $phoneUrl
    Write-Report 'public_settings_roundtrip=pass'

    Write-Host 'Checking published dashboard/PWA install assets...'
    Test-PwaInstallAssets $phoneUrl
    Write-Report 'public_dashboard_assets=pass'

    $initialClientCheck = Get-LatestSeenClientCheck -ClientChecks (Get-ClientCheckEntries $BaseUrl) -DeviceOnly $true
    if ($null -ne $initialClientCheck.ClientCheck) {
        Write-Report 'initial_device_client_seen=true'
        if ($initialClientCheck.ClientCheck.last_seen) {
            Write-Report "initial_device_client_last_seen=$($initialClientCheck.ClientCheck.last_seen)"
            $script:InitialClientLastSeenMs = $initialClientCheck.LastSeenMs
        }
    } else {
        Write-Report 'initial_device_client_seen=false'
    }

    Write-Host 'Checking published client-check controls...'
    Test-ClientCheckSameOriginRoundTrip $phoneUrl
    Write-Report 'public_client_check_roundtrip=pass'

    if ($HoldSeconds -eq 0) {
        Write-Report 'device_client_seen=not_attempted'
        if ($RequireDevice) {
            throw 'required device client check needs HoldSeconds greater than zero'
        }
        $Succeeded = $true
        Write-Host 'ok: deployed sysmon-agent checks passed without device hold'
        return
    }

    Write-Host ''
    Write-Host 'Open from the installed Home Screen Sysmon app on your device during the hold:'
    Write-Host "  $phoneUrl"
    Write-Host 'On your device, confirm the status strip shows app mode, then tap the status strip once to refresh the client check.'
    Write-Host "Holding deployed sysmon-agent verification for ${HoldSeconds}s. Press Ctrl-C to stop early."
    $script:HoldStartedMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
    Write-Report "hold_started_at=$((Get-Date).ToString('o'))"
    Write-Report "hold_started_ms=$script:HoldStartedMs"
    Write-Report 'device_hold_instruction=home_screen_open_status_strip_tap'
    Wait-ForClientCheckEvidence $BaseUrl $HoldSeconds $RequireDevice $RequireStandalone $RequireInteraction

    $Succeeded = $true
    Write-Host 'ok: deployed sysmon-agent checks passed'
} finally {
    try {
        Write-Report "installed_device_home_screen=$script:InstalledDeviceHomeScreen"
        if ($Succeeded) {
            Write-Report 'result=pass'
        } else {
            Write-Report 'result=fail'
        }
        Write-Report "completed_at=$((Get-Date).ToString('o'))"
        Write-Host "Verification report: $ReportPath"
    } catch {
    }
}
