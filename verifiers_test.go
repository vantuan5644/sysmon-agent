package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestLinuxVerifierSupportsManualDeviceHold(t *testing.T) {
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`url_host()`,
		`headers()`,
		`assert_header_contains()`,
		`assert_header_contains "${BASE_URL}/api/status" "Cache-Control" "no-store"`,
		`assert_header_contains "${BASE_URL}/api/status" "Content-Security-Policy" "default-src 'self'"`,
		`assert_header_contains "${BASE_URL}/" "Cache-Control" "no-cache"`,
		`assert_header_contains "${BASE_URL}/" "X-Content-Type-Options" "nosniff"`,
		`check_pwa_install_assets()`,
		`<link rel="apple-touch-icon" sizes="180x180" href="/icon-180.png">`,
		`"display"[[:space:]]*:[[:space:]]*"standalone"`,
		`"src"[[:space:]]*:[[:space:]]*"/icon-512.png"`,
		`check_icon_asset "${base_url}/icon-180.png" 180`,
		`check_icon_asset "${base_url}/icon-512.png" 512`,
		`check_png_dimensions()`,
		`data.readUInt32BE(16)`,
		`CHECK_HOST="${SYSMON_VERIFY_HOST:-}"`,
		`"0.0.0.0") CHECK_HOST="127.0.0.1" ;;`,
		`"::"|"[::]") CHECK_HOST="::1" ;;`,
		`HOLD_SECONDS="${SYSMON_VERIFY_HOLD_SECONDS:-${SYSMON_VERIFY_HOLD:-0}}"`,
		`DEVICE_URL="${SYSMON_VERIFY_DEVICE_URL:-}"`,
		`REQUIRE_DEVICE="${SYSMON_VERIFY_REQUIRE_DEVICE:-0}"`,
		`HOLD_STARTED_MS=""`,
		`SYSMON_VERIFY_REQUIRE_DEVICE must be 0/1, true/false, or yes/no`,
		`SYSMON_VERIFY_HOLD must be a non-negative number of seconds`,
		`REPORT_FILE="${SYSMON_VERIFY_REPORT:-/tmp/sysmon-agent-verify-report.txt}"`,
		`explain_listen_block_if_present()`,
		`report "listen=blocked_permission"`,
		`Run ./verify-no-listen.sh here, or rerun ./verify.sh on the target host where local sockets are permitted.`,
		`json_field()`,
		`json_millis_field()`,
		`report_readyz_collection_errors()`,
		`${prefix}_collection_errors=none`,
		`${prefix}_collection_error_${index + 1}=${message}`,
		`now_millis()`,
		`report_client_check_fields()`,
		`screen_width screen_height`,
		`touch_points`,
		`display_mode`,
		`orientation`,
		`client_check_has_device_evidence()`,
		`[[ "$user_agent" == *Mobile* || "$user_agent" == *iPhone* || "$user_agent" == *iPad* || "$user_agent" == *iPod* || "$user_agent" == *Android* ]] || return 1`,
		`client_check_is_fresh()`,
		`"$last_seen_ms" -ge "$HOLD_STARTED_MS"`,
		`report "require_device=$REQUIRE_DEVICE"`,
		`report "installed_device_home_screen=not_verified"`,
		`report "sysmon-agent verification report"`,
		`report "configured_device_url=$DEVICE_URL"`,
		`report "result=pass"`,
		`Verification report: $REPORT_FILE`,
		`candidate_priority()`,
		`"$first" -eq 100 && "$second" -ge 64 && "$second" -le 127`,
		`include_device_interface()`,
		`include_device_interface "$ifname"`,
		`vethernet*`,
		`*"wsl"*`,
		`candidate_device_hosts()`,
		`ip -o addr show scope global up`,
		`hostname -I`,
		`sort -n -k1,1 -k2,2`,
		`print_device_urls()`,
		`candidate_device_urls()`,
		`print_device_qr()`,
		`qrencode -t ANSIUTF8 "$url"`,
		`echo "  $url"`,
		`Checking live API schema`,
		`SYSMON_VERIFY_BASE_URL="$BASE_URL" node verify-api.mjs --settings-roundtrip`,
		`Checking /readyz`,
		`report "readyz=pass"`,
		`report_readyz_collection_errors "readyz" "$ready_json"`,
		`echo "$status_json" | grep -q '"dashboard_build"'`,
		`node verify-render.mjs`,
		`report "api_schema=pass"`,
		`report "dashboard_render=pass"`,
		`report "dashboard_render=skipped_browser_unavailable"`,
		`Checking /api/client-check`,
		`echo "$client_check_json" | grep -q '"seen":false'`,
		`report "client_check=pass"`,
		`check_pwa_install_assets "$BASE_URL"`,
		`report "dashboard_assets=pass"`,
		`Origin: ${BASE_URL}`,
		`Checking settings readback and persistence`,
		`settings_readback="$(fetch "${BASE_URL}/api/settings")"`,
		`echo "$settings_readback" | grep -q '"shift":true'`,
		`grep -q '"shift": true' "$SETTINGS_FILE"`,
		`echo "$settings_readback" | grep -q '"panel":"gpu"'`,
		`echo "$settings_readback" | grep -q '"cpu_warn":80'`,
		`echo "$settings_readback" | grep -q '"temp_warn_c":75'`,
		`test -s "$SETTINGS_FILE"`,
		`grep -q '"panel": "gpu"' "$SETTINGS_FILE"`,
		`grep -q '"cpu_warn": 80' "$SETTINGS_FILE"`,
		`grep -q '"temp_warn_c": 75' "$SETTINGS_FILE"`,
		`Starting sysmon-agent on ${BIND}:${PORT}; checking ${BASE_URL}`,
		`report "agent_spawned=pass"`,
		`report "agent_ready=fail"`,
		`report "agent_ready=pass"`,
		`Open from your phone or device while this hold is active:`,
		`if [[ -n "$DEVICE_URL" ]]; then`,
		`report "device_url=$url"`,
		`report "device_url=$DEVICE_URL"`,
		`sleep "$HOLD_SECONDS"`,
		`HOLD_STARTED_MS="$(now_millis)"`,
		`report "hold_started_ms=$HOLD_STARTED_MS"`,
		`fetch_client_check_payload()`,
		`/api/client-checks`,
		`merge_client_check_payloads()`,
		`status?.device_client_check`,
		`status?.client_check`,
		`client_check_entries()`,
		`client_check_payload="$(fetch_client_check_payload)"`,
		`report "device_client_seen=pass"`,
		`report_client_check_fields "$device_client_check"`,
		`report_client_check_fields "$stale_device_client_check"`,
		`report "device_client_seen=stale_client"`,
		`if client_check_is_fresh "$client_check_after" && [[ -z "$seen_client_check" ]]; then`,
		`report_client_check_fields "$seen_client_check"`,
		`report "device_client_seen=unexpected_client"`,
		`report "device_client_seen=not_observed"`,
		`report "device_client_seen=not_attempted_loopback_bind"`,
		`report "device_client_seen=not_attempted"`,
		`DEVICE_URL="${SYSMON_VERIFY_DEVICE_URL:-}"`,
		`SYSMON_VERIFY_BIND=0.0.0.0 SYSMON_VERIFY_HOLD=120`,
		`SYSMON_VERIFY_REQUIRE_DEVICE=1`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify.sh missing %q", needle)
		}
	}
}

func TestLinuxVerifierPNGDimensionCheck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	icon180, err := os.ReadFile("static/icon-180.png")
	if err != nil {
		t.Fatal(err)
	}
	icon512, err := os.ReadFile("static/icon-512.png")
	if err != nil {
		t.Fatal(err)
	}

	script := extractShellSection(t, string(data), "check_png_dimensions() {", "is_wildcard_bind() {") + `
check_png_dimensions "data:image/png;base64,${ICON_180}" 180
check_png_dimensions "data:image/png;base64,${ICON_512}" 512
if check_png_dimensions "data:image/png;base64,${ICON_180}" 512 >/dev/null 2>&1; then
    printf 'wrong_size=accepted\n'
else
    printf 'wrong_size=rejected\n'
fi
`
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Env = append(os.Environ(),
		"ICON_180="+base64.StdEncoding.EncodeToString(icon180),
		"ICON_512="+base64.StdEncoding.EncodeToString(icon512),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Linux verifier PNG dimension check failed: %v\n%s", err, out)
	}
	if got, want := string(out), "wrong_size=rejected\n"; got != want {
		t.Fatalf("Linux verifier PNG dimension check output = %q, want %q", got, want)
	}
}

func TestLinuxVerifierReportsBlockedListener(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := extractShellFunction(t, string(data), "explain_listen_block_if_present") + `
REPORT_READY=true
REPORT_FILE="$(mktemp)"
LOG_FILE="$(mktemp)"
report() { printf 'report:%s\n' "$1" >> "$REPORT_FILE"; }
printf 'server error: listen tcp 127.0.0.1:19099: socket: operation not permitted\n' > "$LOG_FILE"
explain_listen_block_if_present 2> "$LOG_FILE.stderr"
cat "$REPORT_FILE"
cat "$LOG_FILE.stderr"
printf 'server error: unrelated failure\n' > "$LOG_FILE"
: > "$REPORT_FILE"
: > "$LOG_FILE.stderr"
explain_listen_block_if_present 2> "$LOG_FILE.stderr"
if [[ -s "$REPORT_FILE" || -s "$LOG_FILE.stderr" ]]; then
    printf 'unexpected_unrelated_output\n'
fi
rm -f "$REPORT_FILE" "$LOG_FILE" "$LOG_FILE.stderr"
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("blocked listener reporting script failed: %v\n%s", err, out)
	}
	for _, needle := range []string{
		"report:listen=blocked_permission",
		"TCP listening is blocked in this environment.",
		"Run ./verify-no-listen.sh here, or rerun ./verify.sh on the target host where local sockets are permitted.",
	} {
		if !strings.Contains(string(out), needle) {
			t.Fatalf("blocked listener output missing %q:\n%s", needle, out)
		}
	}
	if strings.Contains(string(out), "unexpected_unrelated_output") {
		t.Fatalf("blocked listener detector matched unrelated errors:\n%s", out)
	}
}

func TestDeployedVerifierSupportsInstalledDeviceCheck(t *testing.T) {
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`#!/usr/bin/env bash`,
		`set -euo pipefail`,
		`SYSMON_DEPLOY_VERIFY_BASE_URL`,
		`SYSMON_DEPLOY_VERIFY_DEVICE_URL`,
		`SYSMON_DEPLOY_VERIFY_HOLD`,
		`SYSMON_DEPLOY_VERIFY_REQUIRE_DEVICE`,
		`SYSMON_DEPLOY_VERIFY_REQUIRE_STANDALONE`,
		`sysmon-agent deployed verification report`,
		`mode=deployed`,
		`require_standalone=$REQUIRE_STANDALONE`,
		`check_pwa_install_assets()`,
		`<link rel="manifest" href="/manifest.json">`,
		`<link rel="apple-touch-icon" sizes="180x180" href="/icon-180.png">`,
		`<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">`,
		`<meta name="apple-mobile-web-app-title" content="Sysmon">`,
		`"id"[[:space:]]*:[[:space:]]*"/"`,
		`"display"[[:space:]]*:[[:space:]]*"standalone"`,
		`"background_color"[[:space:]]*:[[:space:]]*"#080b10"`,
		`"theme_color"[[:space:]]*:[[:space:]]*"#080b10"`,
		`"src"[[:space:]]*:[[:space:]]*"/icon-512.png"`,
		`check_png_dimensions "${base_url}/icon-180.png" 180`,
		`check_png_dimensions "${base_url}/icon-512.png" 512`,
		`data.readUInt32BE(16)`,
		`readyz=pass`,
		`report_readyz_collection_errors()`,
		`report_readyz_collection_errors "readyz" "$ready_json"`,
		`report_readyz_collection_errors "public_readyz" "$public_ready_json"`,
		`node verify-api.mjs "$LOCAL_BASE_URL"`,
		`check_pwa_install_assets "$LOCAL_BASE_URL"`,
		`detected_device_url()`,
		`tailscale status --self --json`,
		`direct_lan_device_url()`,
		`include_device_interface()`,
		`include_device_interface "$ifname"`,
		`vethernet*`,
		`*"wsl"*`,
		`candidate_device_hosts()`,
		`local_base_url_part()`,
		`device_url_source=$DEVICE_URL_SOURCE`,
		`public_healthz=pass`,
		`public_readyz=pass`,
		`assert_header_contains "${device_url}/api/status" "Cache-Control" "no-store"`,
		`node verify-api.mjs "$device_url"`,
		`public_api_schema=pass`,
		`EXPECTED_DASHBOARD_BUILD=""`,
		`public_status_json="$(fetch "${device_url}/api/status")"`,
		`EXPECTED_DASHBOARD_BUILD="$(json_field "$public_status_json" dashboard_build`,
		`dashboard_build=$EXPECTED_DASHBOARD_BUILD`,
		`check_settings_same_origin_roundtrip()`,
		`Checking published interactive settings controls`,
		`Origin: ${base_url}`,
		`thresholds: {`,
		`settings thresholds.${key} roundtrip mismatch`,
		`status_after_settings="$(fetch "${base_url}/api/status")"`,
		`const status = JSON.parse(process.argv[1]);`,
		`const got = status.settings || {};`,
		`settings status.${key} roundtrip mismatch`,
		`settings status.thresholds.${key} roundtrip mismatch`,
		`public_settings_roundtrip=pass`,
		`check_client_check_same_origin_roundtrip()`,
		`Checking published client-check controls`,
		`\"dashboard_build\":\"${EXPECTED_DASHBOARD_BUILD}\"`,
		`\"interaction\":\"status_strip_tap\"`,
		`for (const key of ["dashboard_build",`,
		`"interaction", "viewport_width"`,
		`"${base_url}/api/client-check"`,
		`status_after_client_check="$(fetch "${base_url}/api/status")"`,
		`const got = status.client_check || {};`,
		`status client_check ${key} roundtrip mismatch`,
		`public_client_check_roundtrip=pass`,
		`check_pwa_install_assets "$device_url"`,
		`public_dashboard_assets=pass`,
		`initial_device_client_seen=true`,
		`INITIAL_CLIENT_LAST_SEEN_MS`,
		`HOLD_STARTED_MS="$(node -e 'process.stdout.write(String(Date.now()))')"`,
		`hold_started_ms=$HOLD_STARTED_MS`,
		`json_millis_field()`,
		`fetch_client_check_payload()`,
		`/api/client-checks`,
		`merge_client_check_payloads()`,
		`status?.device_client_check`,
		`status?.client_check`,
		`client_check_entries()`,
		`latest_device_client_check()`,
		`initial_device_client_check="$(latest_device_client_check "$initial_client_check_payload"`,
		`client_check_is_fresh()`,
		`"$last_seen_ms" -le "$INITIAL_CLIENT_LAST_SEEN_MS"`,
		`record_client_check_result()`,
		`if ! client_check_is_fresh "$json"; then`,
		`wait_for_client_check_evidence()`,
		`if client_check_is_fresh "$client_check" && [[ -z "$poll_seen_check" ]]; then`,
		`sleep "$remaining"`,
		`client_check_has_device_evidence()`,
		`client_check_has_standalone_evidence()`,
		`client_check_has_interaction_evidence()`,
		`client_check_has_current_dashboard_build()`,
		`require_interaction=$REQUIRE_INTERACTION`,
		`SYSMON_DEPLOY_VERIFY_REQUIRE_INTERACTION`,
		`Observed a fresh device dashboard client check, but it was not running the current dashboard build.`,
		`device_client_seen=stale_dashboard_build`,
		`device_client_seen=not_interactive`,
		`display_mode="$(json_field "$json" display_mode 2>/dev/null || true)"`,
		`[[ "${standalone,,}" == "true" && "${display_mode,,}" == "standalone" ]]`,
		`interaction="$(json_field "$json" interaction 2>/dev/null || true)"`,
		`[[ "${interaction,,}" == "status_strip_tap" ]]`,
		`report_client_check_fields "$json"`,
		`record_client_check_result "$client_check"`,
		`Observed a fresh Home Screen status-strip tap during the deployed hold.`,
		`Observed a fresh device dashboard client check during the deployed hold, but it was not in Home Screen standalone mode.`,
		`device_client_seen=pass`,
		`INSTALLED_DEVICE_HOME_SCREEN="pass"`,
		`INSTALLED_DEVICE_HOME_SCREEN="not_verified"`,
		`report "installed_device_home_screen=$INSTALLED_DEVICE_HOME_SCREEN"`,
		`device_client_seen=not_standalone`,
		`device_client_seen=stale_client`,
		`device_client_seen=unexpected_client`,
		`device_client_seen=not_observed`,
		`Open from the installed Home Screen Sysmon app on your phone or device during the hold:`,
		`confirm the status strip shows app mode`,
		`device_hold_instruction=home_screen_open_status_strip_tap`,
		`qrencode -t ANSIUTF8 "$url"`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify-deployed.sh missing %q", needle)
		}
	}

	settingsFunction := extractShellSection(t, script, "check_settings_same_origin_roundtrip() {", "check_client_check_same_origin_roundtrip() {")
	for _, needle := range []string{
		`status_after_settings="$(fetch "${base_url}/api/status")"`,
		`const got = status.settings || {};`,
		`settings status.thresholds.${key} roundtrip mismatch`,
	} {
		if !strings.Contains(settingsFunction, needle) {
			t.Fatalf("check_settings_same_origin_roundtrip missing %q", needle)
		}
	}
}

func TestDeployedVerifierPNGDimensionCheck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	icon180, err := os.ReadFile("static/icon-180.png")
	if err != nil {
		t.Fatal(err)
	}
	icon512, err := os.ReadFile("static/icon-512.png")
	if err != nil {
		t.Fatal(err)
	}

	script := extractShellSection(t, string(data), "check_png_dimensions() {", "check_settings_same_origin_roundtrip() {") + `
check_png_dimensions "data:image/png;base64,${ICON_180}" 180
check_png_dimensions "data:image/png;base64,${ICON_512}" 512
if check_png_dimensions "data:image/png;base64,${ICON_180}" 512 >/dev/null 2>&1; then
    printf 'wrong_size=accepted\n'
else
    printf 'wrong_size=rejected\n'
fi
`
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Env = append(os.Environ(),
		"ICON_180="+base64.StdEncoding.EncodeToString(icon180),
		"ICON_512="+base64.StdEncoding.EncodeToString(icon512),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deployed PNG dimension check failed: %v\n%s", err, out)
	}
	if got, want := string(out), "wrong_size=rejected\n"; got != want {
		t.Fatalf("deployed PNG dimension check output = %q, want %q", got, want)
	}
}

func TestDeployedVerifierFallsBackToLANURL(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}

	script := extractShellFunction(t, string(data), "url_host") +
		extractShellFunction(t, string(data), "direct_lan_device_url") +
		extractShellFunction(t, string(data), "detected_device_url") + `
tailscale_hostname() { return 1; }
normalize_base_url() { printf '%s' "$1"; }
local_base_url_part() {
    case "$1" in
        protocol) printf 'http' ;;
        port) printf '9099' ;;
    esac
}
candidate_device_hosts() { printf '%s\n' 100.90.1.2 192.168.1.40; }
DEVICE_URL=
DEVICE_URL_SOURCE=
LOCAL_BASE_URL=http://127.0.0.1:9099
url_file="$(mktemp)"
detected_device_url > "$url_file"
printf 'url=%s\n' "$(<"$url_file")"
rm -f "$url_file"
printf 'source=%s\n' "$DEVICE_URL_SOURCE"
`
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("deployed LAN URL fallback failed: %v\n%s", err, out)
	}
	want := "url=http://100.90.1.2:9099\nsource=direct_lan"
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("deployed LAN URL fallback = %q, want %q", strings.TrimSpace(string(out)), want)
	}
}

func TestWindowsDeployedVerifierFallsBackToLANURL(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not available")
	}
	psScript := `
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
foreach ($name in @("Normalize-BaseUrl", "Get-UrlHost", "Get-BaseUrlPart", "Get-DirectLanDeviceUrl", "Get-DetectedDeviceUrl")) {
    Import-VerifyFunction $name
}
function Get-TailscaleHostName { return "" }
function Get-CandidateDeviceHosts { return @("100.90.1.2", "192.168.1.40") }
$script:DeviceUrl = ""
$script:BaseUrl = "http://127.0.0.1:9099"
$script:DeviceUrlSource = ""
Write-Output "url=$(Get-DetectedDeviceUrl)"
Write-Output "source=$script:DeviceUrlSource"
`
	out, err := exec.Command("pwsh", "-NoProfile", "-Command", psScript).CombinedOutput()
	if err != nil {
		t.Fatalf("Windows deployed LAN URL fallback failed: %v\n%s", err, out)
	}
	want := "url=http://100.90.1.2:9099\nsource=direct_lan"
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("Windows deployed LAN URL fallback = %q, want %q", strings.TrimSpace(string(out)), want)
	}
}

func TestWindowsDeployedVerifierUsesStatusClientCheckFallback(t *testing.T) {
	data, err := os.ReadFile("verify-deployed-windows.ps1")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`Get-StatusClientCheckEntries`,
		`Invoke-RestMethod -Method Get -Uri "$BaseUrl/api/status"`,
		`$status.device_client_check`,
		`$status.client_check`,
		`$entries += Get-StatusClientCheckEntries $BaseUrl`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify-deployed-windows.ps1 missing %q", needle)
		}
	}
}

func TestDeployedVerifierRequiresStandaloneEvidence(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
` + extractShellFunction(t, string(data), "client_check_has_standalone_evidence") + `
check_client() {
    local name="$1"
    local json="$2"
    if client_check_has_standalone_evidence "$json"; then
        printf '%s=pass\n' "$name"
    else
        printf '%s=fail\n' "$name"
    fi
}

check_client standalone '{"standalone":true,"display_mode":"standalone"}'
check_client standalone_flag_only '{"standalone":true}'
check_client browser_mode '{"standalone":true,"display_mode":"browser"}'
check_client browser '{"standalone":false,"display_mode":"browser"}'
check_client missing '{}'
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed standalone script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"standalone=pass",
		"standalone_flag_only=fail",
		"browser_mode=fail",
		"browser=fail",
		"missing=fail",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed standalone evidence =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierRequiresStatusStripInteractionEvidence(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
` + extractShellFunction(t, string(data), "client_check_has_interaction_evidence") + `
check_client() {
    local name="$1"
    local json="$2"
    if client_check_has_interaction_evidence "$json"; then
        printf '%s=pass\n' "$name"
    else
        printf '%s=fail\n' "$name"
    fi
}

check_client tap '{"interaction":"status_strip_tap"}'
check_client tap_case '{"interaction":"Status_Strip_Tap"}'
check_client passive '{}'
check_client other '{"interaction":"auto"}'
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed interaction evidence script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"tap=pass",
		"tap_case=pass",
		"passive=fail",
		"other=fail",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed interaction evidence =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierInstalledHomeScreenReportRequiresStandalone(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
report() { printf 'report:%s\n' "$1"; }
report_client_check_fields() { :; }
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") +
		extractShellFunction(t, string(data), "client_check_has_standalone_evidence") +
		extractShellFunction(t, string(data), "client_check_has_interaction_evidence") +
		extractShellFunction(t, string(data), "client_check_has_current_dashboard_build") +
		extractShellFunction(t, string(data), "client_check_is_fresh") +
		extractShellFunction(t, string(data), "record_client_check_result") + `
HOLD_STARTED_MS=1
INITIAL_CLIENT_LAST_SEEN_MS=
client_browser='{"seen":true,"last_seen":"2026-06-21T08:00:00Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":false,"display_mode":"browser"}'
client_flag_only='{"seen":true,"last_seen":"2026-06-21T08:00:00Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"browser"}'
client_standalone='{"seen":true,"last_seen":"2026-06-21T08:00:00Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"standalone"}'
client_standalone_tap='{"seen":true,"last_seen":"2026-06-21T08:00:00Z","interaction":"status_strip_tap","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"standalone"}'
	REQUIRE_DEVICE=true
	REQUIRE_INTERACTION=false
	REQUIRE_STANDALONE=false
	INSTALLED_DEVICE_HOME_SCREEN=not_verified
	record_client_check_result "$client_browser"
	printf 'relaxed_status=%s\n' "$?"
	printf 'relaxed_installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
	REQUIRE_STANDALONE=true
	INSTALLED_DEVICE_HOME_SCREEN=not_verified
	if record_client_check_result "$client_browser"; then
	  printf 'strict_status=ok\n'
	else
	  printf 'strict_status=fail\n'
	fi
	printf 'strict_installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
	INSTALLED_DEVICE_HOME_SCREEN=not_verified
	if record_client_check_result "$client_flag_only"; then
	  printf 'flag_only_status=ok\n'
	else
	  printf 'flag_only_status=fail\n'
	fi
	printf 'flag_only_installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
	INSTALLED_DEVICE_HOME_SCREEN=not_verified
	record_client_check_result "$client_standalone"
	printf 'standalone_status=%s\n' "$?"
	printf 'standalone_installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
	REQUIRE_INTERACTION=true
	INSTALLED_DEVICE_HOME_SCREEN=not_verified
	if record_client_check_result "$client_standalone"; then
	  printf 'strict_interaction_status=ok\n'
	else
	  printf 'strict_interaction_status=fail\n'
	fi
	printf 'strict_interaction_installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
	INSTALLED_DEVICE_HOME_SCREEN=not_verified
	record_client_check_result "$client_standalone_tap"
	printf 'tap_status=%s\n' "$?"
	printf 'tap_installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed installed-home-screen report script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"Observed a fresh device dashboard client check during the deployed hold, but it was not in Home Screen standalone mode.",
		"report:device_client_seen=pass",
		"relaxed_status=0",
		"relaxed_installed=not_verified",
		"Observed a fresh device dashboard client check, but it was not in Home Screen standalone mode.",
		"report:device_client_seen=not_standalone",
		"strict_status=fail",
		"strict_installed=not_verified",
		"Observed a fresh device dashboard client check, but it was not in Home Screen standalone mode.",
		"report:device_client_seen=not_standalone",
		"flag_only_status=fail",
		"flag_only_installed=not_verified",
		"Observed a fresh Home Screen dashboard client check during the deployed hold.",
		"report:device_client_seen=pass",
		"standalone_status=0",
		"standalone_installed=pass",
		"Observed a fresh Home Screen dashboard client check, but it did not include the required status-strip tap.",
		"report:device_client_seen=not_interactive",
		"strict_interaction_status=fail",
		"strict_interaction_installed=not_verified",
		"Observed a fresh Home Screen status-strip tap during the deployed hold.",
		"report:device_client_seen=pass",
		"tap_status=0",
		"tap_installed=pass",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed installed-home-screen report =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierRejectsStaleDashboardBuild(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
report() { printf 'report:%s\n' "$1"; }
report_client_check_fields() { :; }
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") +
		extractShellFunction(t, string(data), "client_check_has_standalone_evidence") +
		extractShellFunction(t, string(data), "client_check_has_interaction_evidence") +
		extractShellFunction(t, string(data), "client_check_has_current_dashboard_build") +
		extractShellFunction(t, string(data), "client_check_is_fresh") +
		extractShellFunction(t, string(data), "record_client_check_result") + `
HOLD_STARTED_MS=1
INITIAL_CLIENT_LAST_SEEN_MS=
REQUIRE_DEVICE=true
REQUIRE_STANDALONE=true
REQUIRE_INTERACTION=true
EXPECTED_DASHBOARD_BUILD=sysmon-static-v97
INSTALLED_DEVICE_HOME_SCREEN=not_verified
client_old_build='{"seen":true,"last_seen":"2026-06-21T08:00:00Z","dashboard_build":"sysmon-static-v77","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"standalone"}'
if record_client_check_result "$client_old_build"; then
  printf 'status=ok\n'
else
  printf 'status=fail\n'
fi
printf 'installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed stale-build script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"Observed a fresh device dashboard client check, but it was not running the current dashboard build.",
		"report:device_client_seen=stale_dashboard_build",
		"status=fail",
		"installed=not_verified",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed stale-build report =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierFindsFreshStandaloneEntryInClientHistory(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
report() { printf 'report:%s\n' "$1"; }
report_client_check_fields() { :; }
fetch_client_check_payload() {
    cat <<'JSON'
{"checks":[
  {"seen":true,"last_seen":"2026-06-21T08:00:05.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"},
  {"seen":true,"last_seen":"2026-06-21T08:00:04.000Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":false,"display_mode":"browser"},
  {"seen":true,"last_seen":"2026-06-21T08:00:03.000Z","interaction":"status_strip_tap","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"standalone"}
]}
JSON
}
client_check_entries() {
    node -e 'const payload = JSON.parse(process.argv[1] || "{}"); for (const check of payload.checks || []) { process.stdout.write(JSON.stringify(check) + "\n"); }' "$1"
}
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") +
		extractShellFunction(t, string(data), "client_check_has_standalone_evidence") +
		extractShellFunction(t, string(data), "client_check_has_interaction_evidence") +
		extractShellFunction(t, string(data), "client_check_has_current_dashboard_build") +
		extractShellFunction(t, string(data), "client_check_is_fresh") +
		extractShellFunction(t, string(data), "record_client_check_result") +
		extractShellFunction(t, string(data), "wait_for_client_check_evidence") + `
HOLD_SECONDS=0
HOLD_STARTED_MS="$(node -e 'process.stdout.write(String(Date.parse("2026-06-21T08:00:02.000Z")))')"
INITIAL_CLIENT_LAST_SEEN_MS="$(node -e 'process.stdout.write(String(Date.parse("2026-06-21T08:00:01.000Z")))')"
REQUIRE_DEVICE=true
REQUIRE_STANDALONE=true
REQUIRE_INTERACTION=true
INSTALLED_DEVICE_HOME_SCREEN=not_verified
wait_for_client_check_evidence
printf 'installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed client history script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"Observed a fresh Home Screen status-strip tap during the deployed hold.",
		"report:device_client_seen=pass",
		"installed=pass",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed client history selection =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierIgnoresStaleNonDeviceClientHistory(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
report() { printf 'report:%s\n' "$1"; }
report_client_check_fields() { :; }
fetch_client_check_payload() {
    cat <<'JSON'
{"checks":[
  {"seen":true,"last_seen":"2026-06-21T08:00:01.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"}
]}
JSON
}
client_check_entries() {
    node -e 'const payload = JSON.parse(process.argv[1] || "{}"); for (const check of payload.checks || []) { process.stdout.write(JSON.stringify(check) + "\n"); }' "$1"
}
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") +
		extractShellFunction(t, string(data), "client_check_has_standalone_evidence") +
		extractShellFunction(t, string(data), "client_check_has_interaction_evidence") +
		extractShellFunction(t, string(data), "client_check_has_current_dashboard_build") +
		extractShellFunction(t, string(data), "client_check_is_fresh") +
		extractShellFunction(t, string(data), "record_client_check_result") +
		extractShellFunction(t, string(data), "wait_for_client_check_evidence") + `
HOLD_SECONDS=0
HOLD_STARTED_MS="$(node -e 'process.stdout.write(String(Date.parse("2026-06-21T08:00:02.000Z")))')"
INITIAL_CLIENT_LAST_SEEN_MS=
REQUIRE_DEVICE=false
REQUIRE_STANDALONE=true
REQUIRE_INTERACTION=true
INSTALLED_DEVICE_HOME_SCREEN=not_verified
wait_for_client_check_evidence
printf 'installed=%s\n' "$INSTALLED_DEVICE_HOME_SCREEN"
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed stale desktop history script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"No dashboard client check was observed during the deployed hold.",
		"report:device_client_seen=not_observed",
		"installed=not_verified",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed stale desktop history =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierInitialBaselineUsesLatestDeviceClient(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
client_check_entries() {
    node -e 'const payload = JSON.parse(process.argv[1] || "{}"); for (const check of payload.checks || []) { process.stdout.write(JSON.stringify(check) + "\n"); }' "$1"
}
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") +
		extractShellFunction(t, string(data), "latest_device_client_check") + `
mixed_payload='{"checks":[
  {"seen":true,"last_seen":"2026-06-21T08:00:05.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"},
  {"seen":true,"last_seen":"2026-06-21T08:00:03.000Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 16_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":375,"viewport_height":812,"standalone":true,"display_mode":"standalone"},
  {"seen":true,"last_seen":"2026-06-21T08:00:04.000Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"standalone"}
]}'
desktop_only_payload='{"checks":[
  {"seen":true,"last_seen":"2026-06-21T08:00:05.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"}
]}'
latest="$(latest_device_client_check "$mixed_payload")"
printf 'mixed_last_seen=%s\n' "$(json_field "$latest" last_seen)"
printf 'mixed_width=%s\n' "$(json_field "$latest" viewport_width)"
if latest_device_client_check "$desktop_only_payload" >/dev/null; then
  printf 'desktop_only=pass\n'
else
  printf 'desktop_only=fail\n'
fi
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed initial device baseline script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"mixed_last_seen=2026-06-21T08:00:04.000Z",
		"mixed_width=390",
		"desktop_only=fail",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed initial device baseline =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierMergesStatusClientChecks(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
` + extractShellSection(t, string(data), "merge_client_check_payloads() {", "\nclient_check_entries() {") +
		`
client_check_entries() {
    node -e 'const payload = JSON.parse(process.argv[1] || "{}"); for (const check of payload.checks || []) { process.stdout.write(JSON.stringify(check) + "\n"); }' "$1"
}
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") +
		extractShellFunction(t, string(data), "latest_device_client_check") + `
endpoint_payload='{"checks":[
  {"seen":true,"last_seen":"2026-06-21T08:00:05.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"}
]}'
status_payload='{
  "client_check":{"seen":true,"last_seen":"2026-06-21T08:00:05.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"},
  "device_client_check":{"seen":true,"last_seen":"2026-06-21T08:00:04.000Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"standalone"}
}'
merged="$(merge_client_check_payloads "$endpoint_payload" "$status_payload")"
printf 'check_count=%s\n' "$(client_check_entries "$merged" | wc -l | tr -d ' ')"
latest="$(latest_device_client_check "$merged")"
printf 'latest_last_seen=%s\n' "$(json_field "$latest" last_seen)"
printf 'latest_width=%s\n' "$(json_field "$latest" viewport_width)"
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed status client-check merge script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"check_count=3",
		"latest_last_seen=2026-06-21T08:00:04.000Z",
		"latest_width=390",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed status client-check merge =\n%s\nwant\n%s", out, want)
	}
}

func TestLinuxVerifierMergesStatusClientChecks(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$1" "$2"
}
` + extractShellSection(t, string(data), "merge_client_check_payloads() {", "\nclient_check_entries() {") +
		`
client_check_entries() {
    node -e 'const payload = JSON.parse(process.argv[1] || "{}"); for (const check of payload.checks || []) { process.stdout.write(JSON.stringify(check) + "\n"); }' "$1"
}
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") + `
endpoint_payload='{"checks":[
  {"seen":true,"last_seen":"2026-06-21T08:00:05.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"}
]}'
status_payload='{
  "client_check":{"seen":true,"last_seen":"2026-06-21T08:00:05.000Z","user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_0) AppleWebKit/605.1.15 Safari/605.1.15","viewport_width":1440,"viewport_height":900,"standalone":false,"display_mode":"browser"},
  "device_client_check":{"seen":true,"last_seen":"2026-06-21T08:00:04.000Z","user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":390,"viewport_height":844,"standalone":true,"display_mode":"standalone"}
}'
merged="$(merge_client_check_payloads "$endpoint_payload" "$status_payload")"
device_count=0
device_width=
while IFS= read -r check; do
  if client_check_has_device_evidence "$check"; then
    device_count=$((device_count + 1))
    device_width="$(json_field "$check" viewport_width)"
  fi
done < <(client_check_entries "$merged")
printf 'check_count=%s\n' "$(client_check_entries "$merged" | wc -l | tr -d ' ')"
printf 'device_count=%s\n' "$device_count"
printf 'device_width=%s\n' "$device_width"
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("linux status client-check merge script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"check_count=3",
		"device_count=1",
		"device_width=390",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("linux status client-check merge =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierRequiresFreshClientCheck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
` + extractShellFunction(t, string(data), "client_check_is_fresh") + `
	check_client() {
	    local name="$1"
	    local initial="$2"
	    local final="$3"
	    HOLD_STARTED_MS="$(node -e 'process.stdout.write(String(Date.parse("2026-01-01T00:00:02.000Z")))')"
	    INITIAL_CLIENT_LAST_SEEN_MS="$initial"
	    if client_check_is_fresh "{\"last_seen\":\"${final}\"}"; then
	        printf '%s=pass\n' "$name"
    else
        printf '%s=fail\n' "$name"
    fi
	}
	initial_ms="$(node -e 'process.stdout.write(String(Date.parse("2026-01-01T00:00:03.000Z")))')"

	check_client before_hold "" "2026-01-01T00:00:01.000Z"
	check_client at_hold "" "2026-01-01T00:00:02.000Z"
	check_client unchanged_initial "$initial_ms" "2026-01-01T00:00:03.000Z"
	check_client advanced_initial "$initial_ms" "2026-01-01T00:00:04.000Z"
	`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("deployed freshness script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"before_hold=fail",
		"at_hold=pass",
		"unchanged_initial=fail",
		"advanced_initial=pass",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("deployed freshness =\n%s\nwant\n%s", out, want)
	}
}

func TestLinuxVerifierRequiresFreshClientCheck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_millis_field() {
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$1" "$2"
}
` + extractShellFunction(t, string(data), "client_check_is_fresh") + `
	check_client() {
	    local name="$1"
	    local final="$2"
	    HOLD_STARTED_MS="$(node -e 'process.stdout.write(String(Date.parse("2026-01-01T00:00:02.000Z")))')"
	    if client_check_is_fresh "{\"last_seen\":\"${final}\"}"; then
	        printf '%s=pass\n' "$name"
    else
        printf '%s=fail\n' "$name"
    fi
	}

	check_client before_hold "2026-01-01T00:00:01.000Z"
	check_client at_hold "2026-01-01T00:00:02.000Z"
	check_client after_hold "2026-01-01T00:00:04.000Z"
	check_client invalid_time "not-a-date"
	`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("linux freshness script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"before_hold=fail",
		"at_hold=pass",
		"after_hold=pass",
		"invalid_time=fail",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("linux freshness =\n%s\nwant\n%s", out, want)
	}
}

func TestLinuxVerifierFormatsDeviceURLs(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := extractShellFunction(t, string(data), "url_host") +
		extractShellFunction(t, string(data), "candidate_device_urls") + `
PORT=19099
candidate_device_hosts() {
    printf '%s\n' '100.90.1.2' 'fd7a:115c:a1e0::1'
}
candidate_device_urls
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("candidate URL script failed: %v\n%s", err, out)
	}
	want := "http://100.90.1.2:19099/\nhttp://[fd7a:115c:a1e0::1]:19099/\n"
	if string(out) != want {
		t.Fatalf("candidate URLs =\n%s\nwant\n%s", out, want)
	}
}

func TestLinuxVerifierFiltersVirtualDeviceCandidateInterfaces(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ip", []byte(`#!/usr/bin/env bash
printf '%s\n' \
  '2: tailscale0 inet 100.64.1.2/32 scope global tailscale0' \
  '3: eth0 inet 192.168.1.40/24 scope global eth0' \
  '4: docker0 inet 172.17.0.1/16 scope global docker0' \
  '5: br-1234567890ab inet 172.18.0.1/16 scope global br-1234567890ab' \
  '6: vethabc@if3 inet 192.168.200.10/24 scope global vethabc' \
  '7: vEthernet inet 172.29.64.1/20 scope global vEthernet'
`), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/hostname", []byte(`#!/usr/bin/env bash
printf '%s\n' '10.0.0.8'
`), 0755); err != nil {
		t.Fatal(err)
	}

	script := extractShellFunction(t, string(data), "is_loopback_bind") +
		extractShellFunction(t, string(data), "is_wildcard_bind") +
		extractShellFunction(t, string(data), "normalize_bind_host") +
		extractShellFunction(t, string(data), "candidate_priority") +
		extractShellFunction(t, string(data), "include_device_interface") +
		extractShellSection(t, string(data), "candidate_device_hosts() {", "print_device_urls() {") + `
BIND=0.0.0.0
candidate_device_hosts
`
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verify.sh candidate host filter failed: %v\n%s", err, out)
	}
	want := "100.64.1.2\n192.168.1.40\n"
	if string(out) != want {
		t.Fatalf("verify.sh candidate hosts =\n%s\nwant\n%s", out, want)
	}
}

func TestDeployedVerifierFiltersVirtualDeviceCandidateInterfaces(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	data, err := os.ReadFile("verify-deployed.sh")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ip", []byte(`#!/usr/bin/env bash
printf '%s\n' \
  '2: tailscale0 inet 100.64.1.2/32 scope global tailscale0' \
  '3: eth0 inet 192.168.1.40/24 scope global eth0' \
  '4: docker0 inet 172.17.0.1/16 scope global docker0' \
  '5: br-1234567890ab inet 172.18.0.1/16 scope global br-1234567890ab' \
  '6: vethabc@if3 inet 192.168.200.10/24 scope global vethabc' \
  '7: vEthernet inet 172.29.64.1/20 scope global vEthernet'
`), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/hostname", []byte(`#!/usr/bin/env bash
printf '%s\n' '10.0.0.8'
`), 0755); err != nil {
		t.Fatal(err)
	}

	script := extractShellFunction(t, string(data), "candidate_priority") +
		extractShellFunction(t, string(data), "include_device_interface") +
		extractShellSection(t, string(data), "candidate_device_hosts() {", "direct_lan_device_url() {") + `
candidate_device_hosts
`
	cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("verify-deployed.sh candidate host filter failed: %v\n%s", err, out)
	}
	want := "100.64.1.2\n"
	if string(out) != want {
		t.Fatalf("verify-deployed.sh candidate hosts =\n%s\nwant\n%s", out, want)
	}
}

func TestLinuxVerifierCandidatePriorityOrder(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	function := extractShellFunction(t, string(data), "candidate_priority")
	script := function + `
for host in 100.90.1.2 10.0.0.5 192.168.1.20 172.16.1.1 8.8.8.8 fd7a:115c:a1e0::1 2001:4860:4860::8888; do
    printf '%s=%s\n' "$host" "$(candidate_priority "$host")"
done
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("candidate priority script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"100.90.1.2=0",
		"10.0.0.5=1",
		"192.168.1.20=1",
		"172.16.1.1=1",
		"8.8.8.8=2",
		"fd7a:115c:a1e0::1=1",
		"2001:4860:4860::8888=3",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("candidate priorities =\n%s\nwant\n%s", out, want)
	}
}

func TestLinuxVerifierStrictDeviceEvidence(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	data, err := os.ReadFile("verify.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := `
json_field() {
    node -e 'const value = JSON.parse(process.argv[1])[process.argv[2]]; if (value !== undefined && value !== null) process.stdout.write(String(value));' "$1" "$2"
}
` + extractShellFunction(t, string(data), "client_check_has_device_evidence") + `
check_client() {
    local name="$1"
    local json="$2"
    if client_check_has_device_evidence "$json"; then
        printf '%s=pass\n' "$name"
    else
        printf '%s=fail\n' "$name"
    fi
}

check_client iphone '{"user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1","viewport_width":390,"viewport_height":844}'
check_client ipad '{"user_agent":"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1","viewport_width":1024,"viewport_height":768}'
check_client android '{"user_agent":"Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Mobile Safari/537.36","viewport_width":390,"viewport_height":844}'
check_client desktop_browser '{"user_agent":"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15","viewport_width":1440,"viewport_height":900}'
check_client zero_viewport '{"user_agent":"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1","viewport_width":0,"viewport_height":844}'
`
	out, err := exec.Command("bash", "-euo", "pipefail", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("strict device evidence script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"iphone=pass",
		"ipad=pass",
		"android=pass",
		"desktop_browser=fail",
		"zero_viewport=fail",
	}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("strict device evidence =\n%s\nwant\n%s", out, want)
	}
}

func TestWindowsVerifierStrictDeviceEvidence(t *testing.T) {
	if _, err := exec.LookPath("pwsh"); err != nil {
		t.Skip("pwsh not available")
	}
	psScript := `
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

function Check-Client([string]$Name, $ClientCheck) {
    if (Test-DeviceClientCheckEvidence $ClientCheck) {
        Write-Output "$Name=pass"
    } else {
        Write-Output "$Name=fail"
    }
}

Check-Client "iphone" ([pscustomobject]@{
    user_agent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
    viewport_width = 390
    viewport_height = 844
})
Check-Client "ipad" ([pscustomobject]@{
    user_agent = "Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
    viewport_width = 1024
    viewport_height = 768
})
Check-Client "android" ([pscustomobject]@{
    user_agent = "Mozilla/5.0 (Linux; Android 14) AppleWebKit/537.36 Mobile Safari/537.36"
    viewport_width = 390
    viewport_height = 844
})
Check-Client "desktop_browser" ([pscustomobject]@{
    user_agent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Safari/605.1.15"
    viewport_width = 1440
    viewport_height = 900
})
Check-Client "zero_viewport" ([pscustomobject]@{
    user_agent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1"
    viewport_width = 0
    viewport_height = 844
})
`
	out, err := exec.Command("pwsh", "-NoProfile", "-Command", psScript).CombinedOutput()
	if err != nil {
		t.Fatalf("strict Windows device evidence script failed: %v\n%s", err, out)
	}
	want := strings.Join([]string{
		"iphone=pass",
		"ipad=pass",
		"android=pass",
		"desktop_browser=fail",
		"zero_viewport=fail",
	}, "\n")
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("strict Windows device evidence =\n%s\nwant\n%s", out, want)
	}
}

func TestRenderVerifierUsesHeadlessBrowserScreenshots(t *testing.T) {
	data, err := os.ReadFile("verify-render.mjs")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`phone-portrait`,
		`phone-landscape`,
		`findBrowsers()`,
		`firefox`,
		`brave-browser`,
		`captureScreenshot`,
		`verifyScreenshot`,
		`verifyStaticLayout`,
		`up 7h 12m / saved / app`,
		`03:16:00 / 4s / 142ms`,
		`class="panel alerts-panel"`,
		`CPU 86% over 70%`,
		`assertNarrowPhoneLayoutFits`,
		`decodePNG`,
		`SYSMON_RENDER_STRICT`,
		`dashboard static layout smoke passed`,
		`headless screenshots skipped`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify-render.mjs missing %q", needle)
		}
	}
}

func TestAPIVerifierValidatesLiveSchema(t *testing.T) {
	data, err := os.ReadFile("verify-api.mjs")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`SYSMON_VERIFY_BASE_URL`,
		`--sample`,
		`--settings-roundtrip`,
		`--client-check-roundtrip`,
		`const deviceUserAgent =`,
		`/healthz`,
		`/readyz`,
		`/api/status`,
		`/api/metrics`,
		`/api/settings`,
		`/api/client-check`,
		`/api/client-checks`,
		`validateMetrics`,
		`validateReadiness`,
		`readiness.collection_errors`,
		`validateNetwork`,
		`validateTemperatures`,
		`validateGPU`,
		`validateCollectionErrors`,
		`metrics.collection_errors`,
		`validateSettings`,
		`function validateStatus(status, expectedSettings = {})`,
		`status.dashboard_build === dashboardBuild`,
		`validateSettings(status.settings, expectedSettings);`,
		`validateClientCheck(status.client_check);`,
		`validateClientCheck(status.device_client_check);`,
		`client_check: sampleClientCheck(false),`,
		`device_client_check: sampleClientCheck(false),`,
		`validateStatus(sampleStatus(roundTripSampleSettings()), roundTripSettings);`,
		`const statusAfterSettings = await fetchJSON("/api/status");`,
		`validateStatus(statusAfterSettings, roundTripSettings);`,
		`const statusAfterClientCheck = await fetchJSON("/api/status");`,
		`validateClientCheck(statusAfterClientCheck.client_check, clientCheckPayload);`,
		`validateDeviceClientCheck(statusAfterClientCheck.device_client_check, clientCheckPayload);`,
		`validateClientCheck`,
		`validateDeviceClientCheck`,
		`validateClientCheckHistory`,
		`clientCheck.seen must be true when expected metadata is provided`,
		`assertSettingsRejectsMissingOrigin`,
		`settings without Origin returned HTTP`,
		`assertClientCheckRejectsMissingOrigin`,
		`client-check without Origin returned HTTP`,
		`browserPostHeaders()`,
		`"Origin": baseURL`,
		`"User-Agent": deviceUserAgent`,
		`roundTripSettings`,
		`clientCheckPayload`,
		`dashboard_build: dashboardBuild`,
		`interaction: "status_strip_tap"`,
		`display_mode: "standalone"`,
		`ok: API schema validation passed`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify-api.mjs missing %q", needle)
		}
	}
}

func TestNoListenVerifierDocumentsAutomatedAndManualGates(t *testing.T) {
	data, err := os.ReadFile("verify-no-listen.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, needle := range []string{
		`#!/usr/bin/env bash`,
		`set -euo pipefail`,
		`SYSMON_VERIFY_NO_LISTEN_REPORT`,
		`mode=no_listen`,
		`installed_device_home_screen=not_verified`,
		`manual_live_agent_smoke=required`,
		`manual_device_safari_client_check=required`,
		`manual_device_home_screen_client_check=required`,
		`gofmt -l ./*.go`,
		`go test ./...`,
		`go vet ./...`,
		`go run . -self-check`,
		`go build -o "$TEMP_DIR/sysmon-agent" .`,
		`GOOS=windows GOARCH=amd64 go test -c -o "$TEMP_DIR/sysmon-agent.test.exe" .`,
		`bash -n verify.sh`,
		`bash -n verify-deployed.sh`,
		`run_step "deployed_verifier_syntax" "deployed verifier syntax" bash -n verify-deployed.sh`,
		`node --check static/app.js`,
		`node --check static/sw.js`,
		`node verify-api.mjs --sample --settings-roundtrip --client-check-roundtrip`,
		`node verify-dashboard.mjs`,
		`node verify-render.mjs`,
		`dashboard_render=skipped_browser_unavailable`,
		`foreach ($scriptPath in @("./verify-windows.ps1", "./verify-deployed-windows.ps1", "./install-windows.ps1"))`,
		`Checking Windows strict device evidence logic`,
		`Test-DeviceClientCheckEvidence`,
		`Assert-ClientEvidence "iphone"`,
		`windows_client_evidence=pass`,
		`Checking Windows deployed client history logic`,
		`Test-CurrentDashboardBuildEvidence`,
		`Test-InteractionClientCheckEvidence`,
		`Get-LatestSeenClientCheck`,
		`-DeviceOnly $true`,
		`Windows deployed initial baseline did not select the latest device client check`,
		`Windows deployed initial baseline treated a desktop client check as device evidence`,
		`Wait-ForClientCheckEvidence "http://127.0.0.1:9099" 0 $true $true $true`,
		`windows_deployed_client_history=pass`,
		`Checking Windows installer recent Home Screen activity logic`,
		`Get-LatestHomeScreenClientCheck`,
		`Show-RecentHomeScreenActivity $proof $activity`,
		`Windows installer did not select the latest Home Screen activity`,
		`Windows installer recent activity output missing expected details`,
		`windows_installer_recent_activity=pass`,
		`windows_deployed_verifier_syntax=pass`,
		`windows_verifier_syntax=skipped_pwsh_unavailable`,
		`windows_deployed_verifier_syntax=skipped_pwsh_unavailable`,
		`windows_installer_syntax=skipped_pwsh_unavailable`,
		`windows_client_evidence=skipped_pwsh_unavailable`,
		`windows_deployed_client_history=skipped_pwsh_unavailable`,
		`windows_installer_recent_activity=skipped_pwsh_unavailable`,
		`systemd-analyze verify`,
		`deploy/sysmon-agent.service`,
		`sudo tailscale serve --bg --https=9443 http://127.0.0.1:9099   # publish the dashboard (or any HTTPS reverse proxy)`,
		`SYSMON_DEPLOY_VERIFY_HOLD=120 ./verify-deployed.sh`,
		`Add the printed deployed URL to the device Home Screen, then open that Home Screen app and tap the status strip during the hold window.`,
		`Windows installed-service equivalent:`,
		`.\\install-windows.ps1 -Action Install`,
		`.\\verify-deployed-windows.ps1 -HoldSeconds 120`,
		`Optional isolated smoke-agent gate:`,
		`SYSMON_VERIFY_BIND=0.0.0.0 SYSMON_VERIFY_HOLD=120 SYSMON_VERIFY_REQUIRE_DEVICE=1 ./verify.sh`,
	} {
		if !strings.Contains(script, needle) {
			t.Fatalf("verify-no-listen.sh missing %q", needle)
		}
	}
}

func extractShellFunction(t *testing.T, script, name string) string {
	t.Helper()
	start := strings.Index(script, name+"() {")
	if start < 0 {
		t.Fatalf("missing shell function %s", name)
	}
	remaining := script[start:]
	lines := strings.SplitAfter(remaining, "\n")
	var builder strings.Builder
	for _, line := range lines {
		builder.WriteString(line)
		if strings.TrimSpace(line) == "}" {
			return builder.String()
		}
	}
	t.Fatalf("shell function %s has no closing brace", name)
	return ""
}

func extractShellSection(t *testing.T, script, startNeedle, endNeedle string) string {
	t.Helper()
	start := strings.Index(script, startNeedle)
	if start < 0 {
		t.Fatalf("missing shell section start %q", startNeedle)
	}
	remaining := script[start:]
	end := strings.Index(remaining, endNeedle)
	if end < 0 {
		t.Fatalf("missing shell section end %q", endNeedle)
	}
	return remaining[:end]
}
