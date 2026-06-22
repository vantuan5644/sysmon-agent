#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPORT_FILE="${SYSMON_VERIFY_NO_LISTEN_REPORT:-/tmp/sysmon-agent-no-listen-report.txt}"
TEMP_DIR="$(mktemp -d -t sysmon-no-listen.XXXXXX)"

cleanup() {
    local exit_code=$?
    rm -rf "$TEMP_DIR"
    if [[ "$exit_code" -eq 0 ]]; then
        report "result=pass"
    else
        report "result=fail"
    fi
    report "completed_at=$(date -Is)"
    echo "Verification report: $REPORT_FILE"
    return "$exit_code"
}
trap cleanup EXIT

report() {
    printf '%s\n' "$1" >> "$REPORT_FILE"
}

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "missing required command: $1" >&2
        exit 1
    fi
}

run_step() {
    local key="$1"
    local label="$2"
    shift 2
    echo "Checking ${label}..."
    "$@"
    report "${key}=pass"
}

cd "$SCRIPT_DIR"

require_cmd go
require_cmd bash

mkdir -p "$(dirname "$REPORT_FILE")"
: > "$REPORT_FILE"
report "sysmon-agent no-listen verification report"
report "started_at=$(date -Is)"
report "mode=no_listen"
report "installed_device_home_screen=not_verified"
report "manual_live_agent_smoke=required"
report "manual_device_safari_client_check=required"
report "manual_device_home_screen_client_check=required"

export GOCACHE="${GOCACHE:-/tmp/go-build-cache}"
mkdir -p "$GOCACHE"

echo "Checking gofmt..."
gofmt_out="$(gofmt -l ./*.go)"
if [[ -n "$gofmt_out" ]]; then
    echo "gofmt needed for:" >&2
    echo "$gofmt_out" >&2
    exit 1
fi
report "gofmt=pass"

run_step "go_test" "Go tests" go test ./...
run_step "go_vet" "go vet" go vet ./...
run_step "self_check" "in-process HTTP self-check" go run . -self-check
run_step "linux_build" "temporary Linux build" go build -o "$TEMP_DIR/sysmon-agent" .
run_step "windows_compile" "Windows test binary cross-compile" env GOOS=windows GOARCH=amd64 go test -c -o "$TEMP_DIR/sysmon-agent.test.exe" .
run_step "linux_verifier_syntax" "Linux smoke verifier syntax" bash -n verify.sh
run_step "deployed_verifier_syntax" "deployed verifier syntax" bash -n verify-deployed.sh

if command -v node >/dev/null 2>&1; then
    echo "Checking dashboard JavaScript syntax..."
    node --check static/app.js
    node --check static/sw.js
    node --check verify-api.mjs
    node --check verify-dashboard.mjs
    node --check verify-render.mjs
    report "javascript_syntax=pass"

    run_step "api_schema_sample" "API schema sample and interactive round trips" node verify-api.mjs --sample --settings-roundtrip --client-check-roundtrip
    run_step "dashboard_runtime" "dashboard runtime smoke" node verify-dashboard.mjs

    echo "Checking dashboard render smoke..."
    render_output="$(node verify-render.mjs)"
    echo "$render_output"
    if grep -q '^ok:' <<< "$render_output"; then
        report "dashboard_render=pass"
    elif grep -q '^skip:' <<< "$render_output"; then
        report "dashboard_render=skipped_browser_unavailable"
    else
        echo "unexpected verify-render output:" >&2
        echo "$render_output" >&2
        exit 1
    fi
else
    echo "Skipping Node-based dashboard checks; node is unavailable."
    report "javascript_syntax=skipped_node_unavailable"
    report "api_schema_sample=skipped_node_unavailable"
    report "dashboard_runtime=skipped_node_unavailable"
    report "dashboard_render=skipped_node_unavailable"
fi

if command -v pwsh >/dev/null 2>&1; then
    echo "Checking Windows PowerShell script syntax..."
    pwsh -NoProfile -Command '
        $ErrorActionPreference = "Stop"
        foreach ($scriptPath in @("./verify-windows.ps1", "./verify-deployed-windows.ps1", "./install-windows.ps1")) {
            $tokens = $null
            $errors = $null
            [System.Management.Automation.Language.Parser]::ParseFile((Resolve-Path $scriptPath), [ref]$tokens, [ref]$errors) | Out-Null
            if ($errors.Count -gt 0) {
                $errors | ForEach-Object { Write-Error "$($scriptPath): $_" }
                exit 1
            }
        }
    '
    report "windows_verifier_syntax=pass"
    report "windows_deployed_verifier_syntax=pass"
    report "windows_installer_syntax=pass"

    echo "Checking Windows strict device evidence logic..."
    pwsh -NoProfile -Command '
        $ErrorActionPreference = "Stop"
        $tokens = $null
        $errors = $null
        $verifyPath = [string](Resolve-Path "./verify-windows.ps1")
        $ast = [System.Management.Automation.Language.Parser]::ParseFile($verifyPath, [ref]$tokens, [ref]$errors)
        if ($errors.Count -gt 0) {
            $errors | ForEach-Object { Write-Error "$($verifyPath): $_" }
            exit 1
        }
        $functionAst = $ast.Find({
            param($node)
            $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
                $node.Name -eq "Test-DeviceClientCheckEvidence"
        }, $true)
        if ($null -eq $functionAst) {
            throw "missing Test-DeviceClientCheckEvidence in verify-windows.ps1"
        }
        . ([scriptblock]::Create($functionAst.Extent.Text))

        function Assert-ClientEvidence([string]$Name, $ClientCheck, [bool]$Expected) {
            $got = [bool](Test-DeviceClientCheckEvidence $ClientCheck)
            if ($got -ne $Expected) {
                throw "strict device evidence case $Name returned $got, expected $Expected"
            }
        }

        Assert-ClientEvidence "iphone" ([pscustomobject]@{
            user_agent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
            viewport_width = 390
            viewport_height = 844
        }) $true
        Assert-ClientEvidence "ipad" ([pscustomobject]@{
            user_agent = "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
            viewport_width = 1024
            viewport_height = 768
        }) $true
        Assert-ClientEvidence "android" ([pscustomobject]@{
            user_agent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Mobile Safari/537.36"
            viewport_width = 390
            viewport_height = 844
        }) $true
        Assert-ClientEvidence "desktop_browser" ([pscustomobject]@{
            user_agent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
            viewport_width = 1440
            viewport_height = 900
        }) $false
        Assert-ClientEvidence "zero_viewport" ([pscustomobject]@{
            user_agent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1"
            viewport_width = 0
            viewport_height = 844
        }) $false
    '
    report "windows_client_evidence=pass"

    echo "Checking Windows deployed client history logic..."
    pwsh -NoProfile -Command '
        $ErrorActionPreference = "Stop"
        $tokens = $null
        $errors = $null
        $verifyPath = [string](Resolve-Path "./verify-deployed-windows.ps1")
        $ast = [System.Management.Automation.Language.Parser]::ParseFile($verifyPath, [ref]$tokens, [ref]$errors)
        if ($errors.Count -gt 0) {
            $errors | ForEach-Object { Write-Error "$($verifyPath): $_" }
            exit 1
        }

        function Import-VerifyFunction([string]$Name) {
            $functionAst = $ast.Find({
                param($node)
                $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
                    $node.Name -eq $Name
            }, $true)
            if ($null -eq $functionAst) {
                throw "missing function $Name"
            }
            . ([scriptblock]::Create($functionAst.Extent.Text))
        }

        foreach ($name in @("Test-DeviceClientCheckEvidence", "Test-StandaloneClientCheckEvidence", "Test-InteractionClientCheckEvidence", "Test-CurrentDashboardBuildEvidence", "Get-TimeMilliseconds", "Test-FreshClientCheck", "Write-ClientCheckResult", "Get-LatestSeenClientCheck", "Wait-ForClientCheckEvidence")) {
            Import-VerifyFunction $name
        }

        function Write-Report([string]$Value) {
            Write-Output "report:$Value"
        }
        function Write-ClientCheckReport($ClientCheck) {
        }
        function Get-ClientCheckEntries([string]$BaseUrl) {
            return @(
                [pscustomobject]@{
                    seen = $true
                    last_seen = "2026-06-21T08:00:05.000Z"
                    user_agent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15"
                    viewport_width = 1440
                    viewport_height = 900
                    standalone = $false
                    display_mode = "browser"
                },
                [pscustomobject]@{
                    seen = $true
                    last_seen = "2026-06-21T08:00:03.000Z"
                    user_agent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1"
                    viewport_width = 390
                    viewport_height = 844
                    standalone = $false
                    display_mode = "browser"
                },
                [pscustomobject]@{
                    seen = $true
                    last_seen = "2026-06-21T08:00:04.000Z"
                    interaction = "status_strip_tap"
                    user_agent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1"
                    viewport_width = 390
                    viewport_height = 844
                    standalone = $true
                    display_mode = "standalone"
                }
            )
        }

        $latestDevice = Get-LatestSeenClientCheck -ClientChecks (Get-ClientCheckEntries "http://127.0.0.1:9099") -DeviceOnly $true
        if ($null -eq $latestDevice.ClientCheck -or $latestDevice.ClientCheck.last_seen -ne "2026-06-21T08:00:04.000Z") {
            throw "Windows deployed initial baseline did not select the latest device client check"
        }
        $desktopOnly = Get-LatestSeenClientCheck -ClientChecks @(
            [pscustomobject]@{
                seen = $true
                last_seen = "2026-06-21T08:00:05.000Z"
                user_agent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15"
                viewport_width = 1440
                viewport_height = 900
                standalone = $false
                display_mode = "browser"
            }
        ) -DeviceOnly $true
        if ($null -ne $desktopOnly.ClientCheck) {
            throw "Windows deployed initial baseline treated a desktop client check as device evidence"
        }

        $script:HoldStartedMs = [DateTimeOffset]::Parse("2026-06-21T08:00:02.000Z").ToUnixTimeMilliseconds()
        $script:InitialClientLastSeenMs = [DateTimeOffset]::Parse("2026-06-21T08:00:01.000Z").ToUnixTimeMilliseconds()
        $script:InstalledDeviceHomeScreen = "not_verified"
        Wait-ForClientCheckEvidence "http://127.0.0.1:9099" 0 $true $true $true
        if ($script:InstalledDeviceHomeScreen -ne "pass") {
            throw "Windows deployed history check did not mark installed Home Screen as pass"
        }
    '
    report "windows_deployed_client_history=pass"

    echo "Checking Windows installer recent Home Screen activity logic..."
    pwsh -NoProfile -Command '
        $ErrorActionPreference = "Stop"
        $tokens = $null
        $errors = $null
        $installerPath = [string](Resolve-Path "./install-windows.ps1")
        $ast = [System.Management.Automation.Language.Parser]::ParseFile($installerPath, [ref]$tokens, [ref]$errors)
        if ($errors.Count -gt 0) {
            $errors | ForEach-Object { Write-Error "$($installerPath): $_" }
            exit 1
        }

        function Import-InstallerFunction([string]$Name) {
            $functionAst = $ast.Find({
                param($node)
                $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
                    $node.Name -eq $Name
            }, $true)
            if ($null -eq $functionAst) {
                throw "missing function $Name"
            }
            . ([scriptblock]::Create($functionAst.Extent.Text))
        }

        foreach ($name in @("Test-DeviceClientCheckEvidence", "Test-StandaloneClientCheckEvidence", "Get-TimeMilliseconds", "Get-ClientCheckAgeSeconds", "Format-AgeLabel", "Get-LatestHomeScreenClientCheck", "Get-ClientCheckIdentityKey", "Show-RecentHomeScreenActivity")) {
            Import-InstallerFunction $name
        }

        $deviceUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1"
        $proof = [pscustomobject]@{
            seen = $true
            last_seen = "2026-06-21T08:00:04.000Z"
            interaction = "status_strip_tap"
            user_agent = $deviceUserAgent
            viewport_width = 390
            viewport_height = 844
            standalone = $true
            display_mode = "standalone"
        }
        $activity = [pscustomobject]@{
            seen = $true
            last_seen = "2026-06-21T08:00:05.000Z"
            interaction = "settings_refresh"
            user_agent = $deviceUserAgent
            viewport_width = 390
            viewport_height = 844
            standalone = $true
            display_mode = "standalone"
        }
        $olderActivity = [pscustomobject]@{
            seen = $true
            last_seen = "2026-06-21T08:00:03.000Z"
            interaction = "settings_panel"
            user_agent = $deviceUserAgent
            viewport_width = 390
            viewport_height = 844
            standalone = $true
            display_mode = "standalone"
        }

        $latestHomeScreen = Get-LatestHomeScreenClientCheck @($olderActivity, $proof, $activity)
        if ($null -eq $latestHomeScreen -or [string]$latestHomeScreen.interaction -ne "settings_refresh") {
            throw "Windows installer did not select the latest Home Screen activity"
        }

        $script:HostLines = @()
        function Write-Host {
            param([Parameter(ValueFromRemainingArguments = $true)]$Object)
            $script:HostLines += ($Object -join " ")
        }

        Show-RecentHomeScreenActivity $proof $activity
        $output = $script:HostLines -join "`n"
        if ($output -notmatch "recent Home Screen activity at 2026-06-21T08:00:05.000Z" -or $output -notmatch "interaction=settings_refresh") {
            throw "Windows installer recent activity output missing expected details: $output"
        }

        $script:HostLines = @()
        Show-RecentHomeScreenActivity $proof $proof
        if ($script:HostLines.Count -ne 0) {
            throw "Windows installer recent activity repeated the proof sample"
        }

        $script:HostLines = @()
        Show-RecentHomeScreenActivity $proof $olderActivity
        if ($script:HostLines.Count -ne 0) {
            throw "Windows installer recent activity reported an older activity sample"
        }
    '
    report "windows_installer_recent_activity=pass"
else
    report "windows_verifier_syntax=skipped_pwsh_unavailable"
    report "windows_deployed_verifier_syntax=skipped_pwsh_unavailable"
    report "windows_installer_syntax=skipped_pwsh_unavailable"
    report "windows_client_evidence=skipped_pwsh_unavailable"
    report "windows_deployed_client_history=skipped_pwsh_unavailable"
    report "windows_installer_recent_activity=skipped_pwsh_unavailable"
fi

if command -v systemd-analyze >/dev/null 2>&1; then
    run_step "systemd_verify" "systemd unit syntax" systemd-analyze verify \
        "$SCRIPT_DIR/deploy/sysmon-agent.service" \
        "$SCRIPT_DIR/deploy/sysmon-agent.user.service"
else
    report "systemd_verify=skipped_systemd_analyze_unavailable"
fi

echo ""
echo "ok: no-listen sysmon-agent checks passed"
echo "Manual gates still required on the real host:"
echo "  sudo tailscale serve --bg --https=9443 http://127.0.0.1:9099   # publish the dashboard (or any HTTPS reverse proxy)"
echo "  SYSMON_DEPLOY_VERIFY_HOLD=120 ./verify-deployed.sh"
echo "  Add the printed deployed URL to the device Home Screen, then open that Home Screen app and tap the status strip during the hold window."
echo "Windows installed-service equivalent:"
echo "  .\\install-windows.ps1 -Action Install"
echo "  .\\verify-deployed-windows.ps1 -HoldSeconds 120"
echo "Optional isolated smoke-agent gate:"
echo "  SYSMON_VERIFY_BIND=0.0.0.0 SYSMON_VERIFY_HOLD=120 SYSMON_VERIFY_REQUIRE_DEVICE=1 ./verify.sh"
echo "  Open the printed URL from a browser on the device during the hold window."
