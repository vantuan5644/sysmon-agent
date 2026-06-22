param(
    [string]$Bind = '127.0.0.1',
    [string]$CheckHost = '',
    [string]$DeviceUrl = '',
    [switch]$RequireDevice,
    [ValidateRange(1, 65535)]
    [int]$Port = 19099,
    [ValidateRange(0, 86400)]
    [int]$HoldSeconds = 0
)

$ErrorActionPreference = 'Stop'

$TempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("sysmon-verify-" + [guid]::NewGuid().ToString('N'))
$SettingsPath = Join-Path $TempRoot 'settings.json'
$AgentPath = Join-Path $TempRoot 'sysmon-agent.exe'
$StdoutPath = Join-Path $TempRoot 'stdout.log'
$StderrPath = Join-Path $TempRoot 'stderr.log'
$ReportPath = if ($env:SYSMON_VERIFY_REPORT) { $env:SYSMON_VERIFY_REPORT } else { Join-Path ([System.IO.Path]::GetTempPath()) 'sysmon-agent-verify-report.txt' }
$Proc = $null
$Succeeded = $false
$HoldStartedMs = $null

function Write-Report([string]$Value) {
    Add-Content -LiteralPath $ReportPath -Value $Value
}

function Quote-Arg([string]$Value) {
    if ($Value.Contains('"')) {
        throw "Command-line values cannot contain double quotes: $Value"
    }
    return '"' + $Value + '"'
}

function Invoke-Text([string]$Url) {
    return (Invoke-WebRequest -UseBasicParsing -TimeoutSec 4 -Uri $Url).Content
}

function Invoke-Headers([string]$Url) {
    return (Invoke-WebRequest -UseBasicParsing -TimeoutSec 4 -Uri $Url).Headers
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
    return $true
}

function Get-ClientCheckEntries([string]$Url) {
    $entries = @()
    $payload = $null
    try {
        $payload = Invoke-RestMethod -Method Get -Uri "$Url/api/client-checks" -TimeoutSec 4
    } catch {
        try {
            $payload = Invoke-RestMethod -Method Get -Uri "$Url/api/client-check" -TimeoutSec 4
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

    $entries += Get-StatusClientCheckEntries $Url
    return @($entries)
}

function Get-StatusClientCheckEntries([string]$Url) {
    try {
        $status = Invoke-RestMethod -Method Get -Uri "$Url/api/status" -TimeoutSec 4
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

function Assert-HeaderContains([string]$Url, [string]$Name, [string]$Expected) {
    $headers = Invoke-Headers $Url
    $value = [string]$headers[$Name]
    if (-not $value.Contains($Expected)) {
        throw "expected $Url header $Name to contain $Expected, got '$value'"
    }
}

function Resolve-CheckHost {
    if (-not [string]::IsNullOrWhiteSpace($CheckHost)) {
        return $CheckHost
    }
    switch ($Bind) {
        '0.0.0.0' { return '127.0.0.1' }
        '::' { return '::1' }
        '[::]' { return '::1' }
        default { return $Bind }
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

function Normalize-BindHost([string]$HostName) {
    if ($HostName.StartsWith('[') -and $HostName.EndsWith(']')) {
        return $HostName.Substring(1, $HostName.Length - 2)
    }
    return $HostName
}

function Test-WildcardBind {
    return $Bind.Trim() -in @('', '*', '0.0.0.0', '::', '[::]')
}

function Test-LoopbackBind {
    return $Bind.Trim() -in @('127.0.0.1', 'localhost', '::1', '[::1]')
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
    $ip = $HostName.Trim()
    [System.Net.IPAddress]$parsed = $null
    if ([System.Net.IPAddress]::TryParse($ip, [ref]$parsed)) {
        $bytes = $parsed.GetAddressBytes()
        if ($parsed.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetwork) {
            if ($bytes[0] -eq 100 -and $bytes[1] -ge 64 -and $bytes[1] -le 127) {
                return 0
            }
            if ($bytes[0] -eq 10 -or ($bytes[0] -eq 192 -and $bytes[1] -eq 168) -or ($bytes[0] -eq 172 -and $bytes[1] -ge 16 -and $bytes[1] -le 31)) {
                return 1
            }
            return 2
        }
        if ($parsed.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetworkV6) {
            if (($bytes[0] -band 0xfe) -eq 0xfc) {
                return 1
            }
        }
    }
    return 3
}

function Resolve-DeviceHosts {
    if (Test-LoopbackBind) {
        return @()
    }
    if (-not (Test-WildcardBind)) {
        return @(Normalize-BindHost $Bind)
    }

    $addresses = @()
    foreach ($family in @('IPv4', 'IPv6')) {
        try {
            $addresses += Get-NetIPAddress -AddressFamily $family -ErrorAction Stop |
                Where-Object { $_.IPAddress -and (Test-DeviceInterfaceName ([string]$_.InterfaceAlias)) } |
                Select-Object -ExpandProperty IPAddress
        } catch {
        }
    }

    if ($addresses.Count -eq 0) {
        try {
            $addresses += [System.Net.Dns]::GetHostAddresses([System.Net.Dns]::GetHostName()) |
                ForEach-Object { $_.IPAddressToString }
        } catch {
        }
    }

    return @($addresses |
        Where-Object {
            $ip = $_.Trim()
            $lower = $ip.ToLowerInvariant()
            $ip -and
                $ip -notmatch '^127\.' -and
                $ip -ne '::1' -and
                $ip -ne '0.0.0.0' -and
                $ip -ne '::' -and
                $ip -notmatch '^169\.254\.' -and
                -not $lower.StartsWith('fe80:')
        } |
        Select-Object -Unique |
        Sort-Object @{ Expression = { Get-DeviceHostPriority $_ }; Ascending = $true }, @{ Expression = { $_ }; Ascending = $true } |
        Select-Object -First 4)
}

function Write-DeviceUrls {
    $hosts = @(Resolve-DeviceHosts)
    if ($hosts.Count -eq 0) {
        Write-Host "  http://HOST_IP:$Port/"
        return
    }
    foreach ($hostName in $hosts) {
        Write-Host "  http://$(Format-UrlHost $hostName):$Port/"
    }
}

function Get-DeviceUrls {
    $hosts = @(Resolve-DeviceHosts)
    if ($hosts.Count -eq 0) {
        return @("http://HOST_IP:$Port/")
    }
    return @($hosts | ForEach-Object { "http://$(Format-UrlHost $_):$Port/" })
}

function Stop-Agent {
    if ($null -ne $Proc -and -not $Proc.HasExited) {
        Stop-Process -Id $Proc.Id -Force -ErrorAction SilentlyContinue
        $Proc.WaitForExit(5000) | Out-Null
    }
    if (Test-Path -LiteralPath $TempRoot) {
        Remove-Item -LiteralPath $TempRoot -Recurse -Force
    }
}

function Show-AgentLog {
    Write-Host 'sysmon-agent stdout:'
    if (Test-Path -LiteralPath $StdoutPath) {
        Get-Content -LiteralPath $StdoutPath -TotalCount 120
    }
    Write-Host 'sysmon-agent stderr:'
    if (Test-Path -LiteralPath $StderrPath) {
        Get-Content -LiteralPath $StderrPath -TotalCount 120
    }
}

$ResolvedCheckHost = Resolve-CheckHost
$BaseUrl = "http://$(Format-UrlHost $ResolvedCheckHost):$Port"

try {
    New-Item -ItemType Directory -Force -Path $TempRoot | Out-Null
    $ReportDir = [System.IO.Path]::GetDirectoryName($ReportPath)
    if (-not [string]::IsNullOrWhiteSpace($ReportDir)) {
        New-Item -ItemType Directory -Force -Path $ReportDir | Out-Null
    }
    Set-Content -LiteralPath $ReportPath -Value @(
        'sysmon-agent verification report'
        "started_at=$((Get-Date).ToString('o'))"
        "bind=$Bind"
        "port=$Port"
        "base_url=$BaseUrl"
        "settings_file=$SettingsPath"
        "hold_seconds=$HoldSeconds"
        "require_device=$($RequireDevice.IsPresent.ToString().ToLowerInvariant())"
        'installed_device_home_screen=not_verified'
    )
    if (-not [string]::IsNullOrWhiteSpace($DeviceUrl)) {
        Write-Report "configured_device_url=$DeviceUrl"
    }
    Set-Location $PSScriptRoot

    if (-not $env:GOCACHE) {
        $env:GOCACHE = Join-Path ([System.IO.Path]::GetTempPath()) 'go-build-cache'
    }
    New-Item -ItemType Directory -Force -Path $env:GOCACHE | Out-Null

    Write-Host 'Building sysmon-agent.exe...'
    & go build -o $AgentPath .
    if ($LASTEXITCODE -ne 0) {
        throw 'go build failed'
    }
    Write-Report 'build=pass'

    if (Get-Command node -ErrorAction SilentlyContinue) {
        Write-Host 'Checking dashboard JavaScript...'
        & node --check static/app.js
        if ($LASTEXITCODE -ne 0) {
            throw 'static/app.js syntax check failed'
        }
        & node --check static/sw.js
        if ($LASTEXITCODE -ne 0) {
            throw 'static/sw.js syntax check failed'
        }
        & node verify-dashboard.mjs
        if ($LASTEXITCODE -ne 0) {
            throw 'dashboard runtime smoke test failed'
        }
        $renderOutput = & node verify-render.mjs 2>&1
        if ($LASTEXITCODE -ne 0) {
            throw 'dashboard render smoke test failed'
        }
        $renderText = ($renderOutput | Out-String).TrimEnd()
        if (-not [string]::IsNullOrWhiteSpace($renderText)) {
            Write-Host $renderText
        }
        Write-Report 'dashboard_runtime=pass'
        if ($renderText -match '^ok:') {
            Write-Report 'dashboard_render=pass'
        } elseif ($renderText -match '^skip:') {
            Write-Report 'dashboard_render=skipped_browser_unavailable'
        } else {
            throw "unexpected verify-render output: $renderText"
        }
    } else {
        Write-Report 'dashboard_runtime=skipped_node_unavailable'
        Write-Report 'dashboard_render=skipped_node_unavailable'
    }

    $argumentLine = "-bind $(Quote-Arg $Bind) -port $Port -settings $(Quote-Arg $SettingsPath)"

    Write-Host "Starting sysmon-agent on ${Bind}:$Port; checking $BaseUrl..."
    $Proc = Start-Process `
        -FilePath $AgentPath `
        -ArgumentList $argumentLine `
        -RedirectStandardOutput $StdoutPath `
        -RedirectStandardError $StderrPath `
        -PassThru `
        -WindowStyle Hidden
    Write-Report 'agent_spawned=pass'

    $ready = $false
    for ($i = 0; $i -lt 40; $i++) {
        try {
            Invoke-Text "$BaseUrl/healthz" | Out-Null
            $ready = $true
            break
        } catch {
            if ($Proc.HasExited) {
                Show-AgentLog
                Write-Report 'agent_ready=fail'
                throw "sysmon-agent exited early with code $($Proc.ExitCode)"
            }
            Start-Sleep -Milliseconds 250
        }
    }
    if (-not $ready) {
        Show-AgentLog
        Write-Report 'agent_ready=fail'
        throw 'sysmon-agent did not become ready'
    }
    Write-Report 'agent_ready=pass'

    Write-Host 'Checking /healthz...'
    $health = Invoke-Text "$BaseUrl/healthz"
    if ($health -notmatch '"status":"ok"') {
        throw "unexpected /healthz response: $health"
    }
    Write-Report 'healthz=pass'

    Write-Host 'Checking /readyz...'
    $ready = Invoke-Text "$BaseUrl/readyz"
    foreach ($needle in @('"status":"ok"', '"metrics":true')) {
        if ($ready -notmatch [regex]::Escape($needle)) {
            throw "/readyz missing $needle"
        }
    }
    Write-Report 'readyz=pass'
    Write-ReadinessCollectionErrors 'readyz' ($ready | ConvertFrom-Json)

    Write-Host 'Checking /api/metrics...'
    $metrics = Invoke-Text "$BaseUrl/api/metrics"
    foreach ($needle in @('"hostname"', '"cpu_percent"', '"memory"')) {
        if ($metrics -notmatch [regex]::Escape($needle)) {
            throw "/api/metrics missing $needle"
        }
    }
    Write-Report 'metrics=pass'

    Write-Host 'Checking /api/status...'
    $status = Invoke-Text "$BaseUrl/api/status"
    foreach ($needle in @('"status":"ok"', '"dashboard_build"', '"uptime_seconds"', '"refresh_options_ms"', '"settings"')) {
        if ($status -notmatch [regex]::Escape($needle)) {
            throw "/api/status missing $needle"
        }
    }
    Write-Report 'status=pass'

    Write-Host 'Checking /api/client-check...'
    $clientCheck = Invoke-RestMethod `
        -Method Get `
        -Uri "$BaseUrl/api/client-check" `
        -TimeoutSec 4
    if ($clientCheck.seen) {
        throw "unexpected initial client check state: $($clientCheck | ConvertTo-Json -Compress)"
    }
    Write-Report 'client_check=pass'

    if (Get-Command node -ErrorAction SilentlyContinue) {
        Write-Host 'Checking live API schema...'
        & node verify-api.mjs --settings-roundtrip $BaseUrl
        if ($LASTEXITCODE -ne 0) {
            throw 'live API schema validation failed'
        }
        Write-Report 'api_schema=pass'
        Write-Report 'settings_roundtrip=pass'
    } else {
        Write-Report 'api_schema=skipped_node_unavailable'
    }

    Write-Host 'Checking response headers...'
    Assert-HeaderContains "$BaseUrl/api/status" 'Cache-Control' 'no-store'
    Assert-HeaderContains "$BaseUrl/api/status" 'Content-Security-Policy' "default-src 'self'"
    Assert-HeaderContains "$BaseUrl/" 'Cache-Control' 'no-cache'
    Assert-HeaderContains "$BaseUrl/" 'X-Content-Type-Options' 'nosniff'
    Write-Report 'headers=pass'

    Write-Host 'Checking dashboard assets...'
    if ((Invoke-Text "$BaseUrl/") -notmatch '<title>Sysmon</title>') {
        throw 'dashboard HTML missing Sysmon title'
    }
    if ((Invoke-Text "$BaseUrl/manifest.json") -notmatch '"short_name"') {
        throw 'manifest missing short_name'
    }
    if ((Invoke-Text "$BaseUrl/app.js") -notmatch 'fetchMetrics') {
        throw 'app.js missing fetchMetrics'
    }
    if ((Invoke-Text "$BaseUrl/sw.js") -notmatch 'isLiveEndpoint') {
        throw 'service worker missing live endpoint policy'
    }
    Write-Report 'dashboard_assets=pass'

    Write-Host 'Checking interactive settings API...'
    $settings = Invoke-RestMethod `
        -Method Post `
        -Uri "$BaseUrl/api/settings" `
        -Headers @{ Origin = $BaseUrl } `
        -ContentType 'application/json' `
        -Body '{"dim":true,"shift":true,"refresh_ms":2000,"panel":"gpu","thresholds":{"cpu_warn":80,"memory_warn":75,"disk_warn":85,"gpu_warn":80,"temp_warn_c":75}}' `
        -TimeoutSec 4
    if (-not $settings.dim -or -not $settings.shift -or $settings.refresh_ms -ne 2000 -or $settings.panel -ne 'gpu') {
        throw "unexpected settings response: $($settings | ConvertTo-Json -Compress)"
    }
    if ($settings.thresholds.cpu_warn -ne 80 -or $settings.thresholds.memory_warn -ne 75 -or $settings.thresholds.disk_warn -ne 85 -or $settings.thresholds.gpu_warn -ne 80 -or $settings.thresholds.temp_warn_c -ne 75) {
        throw "unexpected threshold settings response: $($settings | ConvertTo-Json -Compress)"
    }
    Write-Report 'settings_post=pass'

    Write-Host 'Checking settings readback and persistence...'
    $settingsReadback = Invoke-RestMethod `
        -Method Get `
        -Uri "$BaseUrl/api/settings" `
        -TimeoutSec 4
    if (-not $settingsReadback.dim -or -not $settingsReadback.shift -or $settingsReadback.refresh_ms -ne 2000 -or $settingsReadback.panel -ne 'gpu') {
        throw "unexpected settings readback: $($settingsReadback | ConvertTo-Json -Compress)"
    }
    if ($settingsReadback.thresholds.cpu_warn -ne 80 -or $settingsReadback.thresholds.memory_warn -ne 75 -or $settingsReadback.thresholds.disk_warn -ne 85 -or $settingsReadback.thresholds.gpu_warn -ne 80 -or $settingsReadback.thresholds.temp_warn_c -ne 75) {
        throw "unexpected threshold settings readback: $($settingsReadback | ConvertTo-Json -Compress)"
    }
    if (-not (Test-Path -LiteralPath $SettingsPath)) {
        throw "settings file was not written: $SettingsPath"
    }
    $settingsFile = Get-Content -Raw -LiteralPath $SettingsPath
    if ($settingsFile -notmatch '"shift":\s*true' -or $settingsFile -notmatch '"panel":\s*"gpu"' -or $settingsFile -notmatch '"cpu_warn":\s*80' -or $settingsFile -notmatch '"temp_warn_c":\s*75') {
        throw "settings file did not persist panel setting: $settingsFile"
    }
    Write-Report 'settings_persistence=pass'

    Write-Host ''
    Write-Host 'ok: sysmon-agent Windows smoke test passed'
    Write-Report 'smoke=pass'
    if ($HoldSeconds -gt 0) {
        Write-Report 'device_hold=offered'
        $deviceClientExpected = $false
        if (-not [string]::IsNullOrWhiteSpace($DeviceUrl)) {
            $deviceClientExpected = $true
            Write-Host 'Open from your device while this hold is active:'
            Write-Host "  $DeviceUrl"
            Write-Report "device_url=$DeviceUrl"
        } elseif ($Bind -in @('127.0.0.1', 'localhost', '::1', '[::1]')) {
            Write-Host 'Agent is bound to loopback. For a device LAN check, rerun with -Bind 0.0.0.0.'
            Write-Report 'device_urls=loopback_bind_unavailable'
        } else {
            $deviceClientExpected = $true
            Write-Host 'Open from your device while this hold is active:'
            Write-DeviceUrls
            foreach ($url in @(Get-DeviceUrls)) {
                Write-Report "device_url=$url"
            }
        }
        Write-Host "Holding sysmon-agent for ${HoldSeconds}s. Press Ctrl-C to stop early."
        $script:HoldStartedMs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
        Write-Report "hold_started_ms=$script:HoldStartedMs"
        Start-Sleep -Seconds $HoldSeconds
        if ($deviceClientExpected) {
            $seenClientCheck = $null
            $deviceClientCheck = $null
            $staleDeviceClientCheck = $null
            foreach ($clientCheckAfter in @(Get-ClientCheckEntries $BaseUrl)) {
                if ($null -eq $clientCheckAfter -or -not $clientCheckAfter.seen) {
                    continue
                }
                if ((Test-FreshClientCheck $clientCheckAfter) -and $null -eq $seenClientCheck) {
                    $seenClientCheck = $clientCheckAfter
                }
                if (Test-DeviceClientCheckEvidence $clientCheckAfter) {
                    if (Test-FreshClientCheck $clientCheckAfter) {
                        $deviceClientCheck = $clientCheckAfter
                        break
                    }
                    if ($null -eq $staleDeviceClientCheck) {
                        $staleDeviceClientCheck = $clientCheckAfter
                    }
                }
            }
            if ($null -ne $deviceClientCheck) {
                Write-ClientCheckReport $deviceClientCheck
                Write-Host 'Observed a fresh dashboard client check during the hold.'
                Write-Report 'device_client_seen=pass'
            } elseif ($null -ne $staleDeviceClientCheck) {
                Write-ClientCheckReport $staleDeviceClientCheck
                Write-Host 'Observed a device dashboard client check, but it was not fresh for this hold.'
                Write-Report 'device_client_seen=stale_client'
                if ($RequireDevice) {
                    throw 'required device client check was not observed'
                }
            } elseif ($null -ne $seenClientCheck) {
                Write-ClientCheckReport $seenClientCheck
                Write-Host 'Observed a dashboard client check, but it did not look like a device client.'
                Write-Report 'device_client_seen=unexpected_client'
                if ($RequireDevice) {
                    throw 'required device client check was not observed'
                }
            } else {
                Write-Host 'No dashboard client check was observed during the hold.'
                Write-Report 'device_client_seen=not_observed'
                if ($RequireDevice) {
                    throw 'required device client check was not observed'
                }
            }
        } else {
            Write-Report 'device_client_seen=not_attempted_loopback_bind'
            if ($RequireDevice) {
                throw 'required device client check cannot run on a loopback-only bind'
            }
        }
    } else {
        Write-Host 'For a device check, rerun with -Bind 0.0.0.0 -HoldSeconds 120 -RequireDevice.'
        Write-Report 'device_client_seen=not_attempted'
        if ($RequireDevice) {
            throw 'required device client check needs -HoldSeconds greater than zero'
        }
    }
    $Succeeded = $true
} finally {
    try {
        Write-Report "completed_at=$((Get-Date).ToString('o'))"
        if ($Succeeded) {
            Write-Report 'result=pass'
        } else {
            Write-Report 'result=fail'
        }
        Write-Host "Verification report: $ReportPath"
    } catch {
    }
    Stop-Agent
}
