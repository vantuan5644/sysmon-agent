#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

normalize_base_url() {
    local url="$1"
    while [[ "$url" == */ ]]; do
        url="${url%/}"
    done
    printf '%s' "$url"
}

url_host() {
    local host="$1"
    if [[ "$host" == \[*\] ]]; then
        printf '%s' "$host"
    elif [[ "$host" == *:* ]]; then
        printf '[%s]' "$host"
    else
        printf '%s' "$host"
    fi
}

LOCAL_BASE_URL="$(normalize_base_url "${SYSMON_DEPLOY_VERIFY_BASE_URL:-${SYSMON_VERIFY_BASE_URL:-http://127.0.0.1:${SYSMON_PORT:-9099}}}")"
DEVICE_URL="${SYSMON_DEPLOY_VERIFY_DEVICE_URL:-${SYSMON_VERIFY_DEVICE_URL:-}}"
HOLD_SECONDS="${SYSMON_DEPLOY_VERIFY_HOLD_SECONDS:-${SYSMON_DEPLOY_VERIFY_HOLD:-120}}"
REQUIRE_DEVICE="${SYSMON_DEPLOY_VERIFY_REQUIRE_DEVICE:-${SYSMON_VERIFY_REQUIRE_DEVICE:-1}}"
REQUIRE_STANDALONE="${SYSMON_DEPLOY_VERIFY_REQUIRE_STANDALONE:-1}"
REQUIRE_INTERACTION="${SYSMON_DEPLOY_VERIFY_REQUIRE_INTERACTION:-1}"
REPORT_FILE="${SYSMON_DEPLOY_VERIFY_REPORT:-/tmp/sysmon-agent-deployed-verify-report.txt}"
HOLD_STARTED_MS=""
INITIAL_CLIENT_LAST_SEEN_MS=""
DEVICE_URL_SOURCE=""
INSTALLED_DEVICE_HOME_SCREEN="not_verified"
EXPECTED_DASHBOARD_BUILD=""
REPORT_READY=false

if [[ -z "$LOCAL_BASE_URL" ]]; then
    echo "SYSMON_DEPLOY_VERIFY_BASE_URL must not be empty" >&2
    exit 1
fi
if ! [[ "$HOLD_SECONDS" =~ ^[0-9]+$ ]]; then
    echo "SYSMON_DEPLOY_VERIFY_HOLD must be a non-negative number of seconds" >&2
    exit 1
fi
case "${REQUIRE_DEVICE,,}" in
    0|false|no|"") REQUIRE_DEVICE=false ;;
    1|true|yes) REQUIRE_DEVICE=true ;;
    *)
        echo "SYSMON_DEPLOY_VERIFY_REQUIRE_DEVICE must be 0/1, true/false, or yes/no" >&2
        exit 1
        ;;
esac
case "${REQUIRE_STANDALONE,,}" in
    0|false|no|"") REQUIRE_STANDALONE=false ;;
    1|true|yes) REQUIRE_STANDALONE=true ;;
    *)
        echo "SYSMON_DEPLOY_VERIFY_REQUIRE_STANDALONE must be 0/1, true/false, or yes/no" >&2
        exit 1
        ;;
esac
case "${REQUIRE_INTERACTION,,}" in
    0|false|no|"") REQUIRE_INTERACTION=false ;;
    1|true|yes) REQUIRE_INTERACTION=true ;;
    *)
        echo "SYSMON_DEPLOY_VERIFY_REQUIRE_INTERACTION must be 0/1, true/false, or yes/no" >&2
        exit 1
        ;;
esac

report() {
    if [[ "$REPORT_READY" == "true" ]]; then
        printf '%s\n' "$1" >> "$REPORT_FILE"
    fi
}

cleanup() {
    local exit_code=$?
    if [[ "$REPORT_READY" == "true" ]]; then
        report "installed_device_home_screen=$INSTALLED_DEVICE_HOME_SCREEN"
        if [[ "$exit_code" -eq 0 ]]; then
            report "result=pass"
        else
            report "result=fail"
        fi
        report "completed_at=$(date -Is)"
        echo "Verification report: $REPORT_FILE"
    fi
    return "$exit_code"
}
trap cleanup EXIT

require_cmd() {
    if ! command -v "$1" >/dev/null 2>&1; then
        echo "missing required command: $1" >&2
        exit 1
    fi
}

fetch() {
    curl -fsS --max-time 5 "$1"
}

headers() {
    curl -fsS --max-time 5 -D - -o /dev/null "$1" | tr -d '\r'
}

assert_header_contains() {
    local url="$1"
    local header="$2"
    local expected="$3"
    local response_headers
    response_headers="$(headers "$url")"
    if ! grep -qi "^${header}: .*${expected}" <<< "$response_headers"; then
        echo "expected ${url} header ${header} to contain ${expected}" >&2
        echo "$response_headers" >&2
        exit 1
    fi
}

json_field() {
    local json="$1"
    local field="$2"
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
if (value !== undefined && value !== null) {
  process.stdout.write(String(value));
}
' "$json" "$field"
}

json_millis_field() {
    local json="$1"
    local field="$2"
    node -e '
const value = JSON.parse(process.argv[1])[process.argv[2]];
const parsed = Date.parse(value);
if (Number.isFinite(parsed)) {
  process.stdout.write(String(parsed));
}
' "$json" "$field"
}

report_readyz_collection_errors() {
    local prefix="$1"
    local json="$2"
    local lines
    if ! lines="$(node -e '
const prefix = process.argv[1];
const payload = JSON.parse(process.argv[2]);
const values = Array.isArray(payload.collection_errors) ? payload.collection_errors : [];
const errors = values
  .map((value) => String(value == null ? "" : value).trim().replace(/\s+/g, " "))
  .filter(Boolean);
if (errors.length === 0) {
  console.log(`${prefix}_collection_errors=none`);
} else {
  console.log(`${prefix}_collection_errors=${errors.length}`);
  errors.forEach((message, index) => console.log(`${prefix}_collection_error_${index + 1}=${message}`));
}
' "$prefix" "$json" 2>/dev/null)"; then
        report "${prefix}_collection_errors=parse_failed"
        return
    fi
    while IFS= read -r line; do
        [[ -n "$line" ]] && report "$line"
    done <<< "$lines"
}

report_client_check_fields() {
    local json="$1"
    local field
    local value
    for field in last_seen dashboard_build interaction user_agent viewport_width viewport_height screen_width screen_height device_pixel_ratio touch_points display_mode standalone visibility orientation; do
        if value="$(json_field "$json" "$field" 2>/dev/null)" && [[ -n "$value" ]]; then
            value="${value//$'\n'/ }"
            report "device_client_${field}=${value}"
        fi
    done
}

client_check_has_device_evidence() {
    local json="$1"
    local user_agent viewport_width viewport_height
    user_agent="$(json_field "$json" user_agent 2>/dev/null || true)"
    viewport_width="$(json_field "$json" viewport_width 2>/dev/null || true)"
    viewport_height="$(json_field "$json" viewport_height 2>/dev/null || true)"
    [[ "$user_agent" == *Mobile* || "$user_agent" == *iPhone* || "$user_agent" == *iPad* || "$user_agent" == *iPod* || "$user_agent" == *Android* ]] || return 1
    [[ "$viewport_width" =~ ^[0-9]+$ && "$viewport_width" -gt 0 ]] || return 1
    [[ "$viewport_height" =~ ^[0-9]+$ && "$viewport_height" -gt 0 ]] || return 1
}

client_check_has_standalone_evidence() {
    local json="$1"
    local standalone display_mode
    standalone="$(json_field "$json" standalone 2>/dev/null || true)"
    display_mode="$(json_field "$json" display_mode 2>/dev/null || true)"
    [[ "${standalone,,}" == "true" && "${display_mode,,}" == "standalone" ]]
}

client_check_has_interaction_evidence() {
    local json="$1"
    local interaction
    interaction="$(json_field "$json" interaction 2>/dev/null || true)"
    [[ "${interaction,,}" == "status_strip_tap" ]]
}

client_check_has_current_dashboard_build() {
    local json="$1"
    local dashboard_build
    [[ -n "${EXPECTED_DASHBOARD_BUILD:-}" ]] || return 0
    dashboard_build="$(json_field "$json" dashboard_build 2>/dev/null || true)"
    [[ "$dashboard_build" == "$EXPECTED_DASHBOARD_BUILD" ]]
}

client_check_is_fresh() {
    local json="$1"
    local last_seen_ms
    last_seen_ms="$(json_millis_field "$json" last_seen 2>/dev/null || true)"
    [[ "$last_seen_ms" =~ ^[0-9]+$ ]] || return 1
    [[ "$HOLD_STARTED_MS" =~ ^[0-9]+$ && "$last_seen_ms" -ge "$HOLD_STARTED_MS" ]] || return 1
    if [[ "$INITIAL_CLIENT_LAST_SEEN_MS" =~ ^[0-9]+$ && "$last_seen_ms" -le "$INITIAL_CLIENT_LAST_SEEN_MS" ]]; then
        return 1
    fi
    return 0
}

record_client_check_result() {
    local json="$1"
    if ! grep -q '"seen":true' <<< "$json"; then
        echo "No dashboard client check was observed during the deployed hold."
        report "device_client_seen=not_observed"
        [[ "$REQUIRE_DEVICE" == "true" ]] && return 1
        return 0
    fi

    if client_check_is_fresh "$json" && client_check_has_device_evidence "$json"; then
        report_client_check_fields "$json"
        if ! client_check_has_current_dashboard_build "$json"; then
            echo "Observed a fresh device dashboard client check, but it was not running the current dashboard build."
            report "device_client_seen=stale_dashboard_build"
            return 1
        fi
        if client_check_has_standalone_evidence "$json"; then
            if [[ "$REQUIRE_INTERACTION" == "true" ]] && ! client_check_has_interaction_evidence "$json"; then
                echo "Observed a fresh Home Screen dashboard client check, but it did not include the required status-strip tap."
                report "device_client_seen=not_interactive"
                return 1
            fi
            if client_check_has_interaction_evidence "$json"; then
                echo "Observed a fresh Home Screen status-strip tap during the deployed hold."
            else
                echo "Observed a fresh Home Screen dashboard client check during the deployed hold."
            fi
            report "device_client_seen=pass"
            INSTALLED_DEVICE_HOME_SCREEN="pass"
            return 0
        fi
        if [[ "$REQUIRE_STANDALONE" == "true" ]]; then
            echo "Observed a fresh device dashboard client check, but it was not in Home Screen standalone mode."
            report "device_client_seen=not_standalone"
            return 1
        fi
        if [[ "$REQUIRE_INTERACTION" == "true" ]] && ! client_check_has_interaction_evidence "$json"; then
            echo "Observed a fresh device dashboard client check, but it did not include the required status-strip tap."
            report "device_client_seen=not_interactive"
            return 1
        fi
        echo "Observed a fresh device dashboard client check during the deployed hold, but it was not in Home Screen standalone mode."
        report "device_client_seen=pass"
        return 0
    fi

    if client_check_has_device_evidence "$json"; then
        report_client_check_fields "$json"
        echo "Observed a device dashboard client check, but it was not fresh for this run."
        report "device_client_seen=stale_client"
        [[ "$REQUIRE_DEVICE" == "true" ]] && return 1
        return 0
    fi

    if ! client_check_is_fresh "$json"; then
        echo "No dashboard client check was observed during the deployed hold."
        report "device_client_seen=not_observed"
        [[ "$REQUIRE_DEVICE" == "true" ]] && return 1
        return 0
    fi

    report_client_check_fields "$json"
    echo "Observed a dashboard client check, but it did not look like a phone or device client."
    report "device_client_seen=unexpected_client"
    [[ "$REQUIRE_DEVICE" == "true" ]] && return 1
    return 0
}

fetch_client_check_payload() {
    local payload
    local status_payload
    payload="$(fetch "${LOCAL_BASE_URL}/api/client-checks" 2>/dev/null || true)"
    if [[ -z "$payload" ]]; then
        payload="$(fetch "${LOCAL_BASE_URL}/api/client-check" 2>/dev/null || true)"
    fi
    status_payload="$(fetch "${LOCAL_BASE_URL}/api/status" 2>/dev/null || true)"
    merge_client_check_payloads "$payload" "$status_payload"
}

merge_client_check_payloads() {
    local payload="$1"
    local status_payload="$2"
    node -e '
function parseJSON(value) {
  if (!value) {
    return null;
  }
  try {
    return JSON.parse(value);
  } catch {
    return null;
  }
}
const payload = parseJSON(process.argv[1]);
const status = parseJSON(process.argv[2]);
const checks = [];
function add(check) {
  if (check && typeof check === "object" && !Array.isArray(check)) {
    checks.push(check);
  }
}
if (Array.isArray(payload?.checks)) {
  for (const check of payload.checks) {
    add(check);
  }
} else {
  add(payload);
}
add(status?.device_client_check);
add(status?.client_check);
process.stdout.write(JSON.stringify({ checks }));
' "$payload" "$status_payload"
}

client_check_entries() {
    local json="$1"
    node -e '
let payload;
try {
  payload = JSON.parse(process.argv[1] || "{}");
} catch {
  process.exit(0);
}
const checks = Array.isArray(payload?.checks)
  ? payload.checks
  : (payload && typeof payload === "object" ? [payload] : []);
for (const check of checks) {
  if (check && typeof check === "object") {
    process.stdout.write(`${JSON.stringify(check)}\n`);
  }
}
' "$json"
}

latest_device_client_check() {
    local json="$1"
    local client_check
    local check_ms
    local latest_check=""
    local latest_ms=""

    while IFS= read -r client_check; do
        [[ -n "$client_check" ]] || continue
        client_check_has_device_evidence "$client_check" || continue
        check_ms="$(json_millis_field "$client_check" last_seen 2>/dev/null || true)"
        [[ "$check_ms" =~ ^[0-9]+$ ]] || continue
        if [[ -z "$latest_ms" || "$check_ms" -gt "$latest_ms" ]]; then
            latest_ms="$check_ms"
            latest_check="$client_check"
        fi
    done < <(client_check_entries "$json")

    [[ -n "$latest_check" ]] || return 1
    printf '%s' "$latest_check"
}

wait_for_client_check_evidence() {
    local deadline=$(( $(date +%s) + HOLD_SECONDS ))
    local client_check_payload=""
    local client_check=""
    local last_seen_check=""
    local last_device_check=""
    local poll_seen_check=""
    local poll_device_check=""
    local now
    local remaining

    while true; do
        client_check_payload="$(fetch_client_check_payload)"
        poll_seen_check=""
        poll_device_check=""
        while IFS= read -r client_check; do
            [[ -n "$client_check" ]] || continue
            if ! grep -q '"seen":true' <<< "$client_check"; then
                continue
            fi
            if client_check_is_fresh "$client_check" && [[ -z "$poll_seen_check" ]]; then
                poll_seen_check="$client_check"
            fi
            if client_check_has_device_evidence "$client_check" && [[ -z "$poll_device_check" ]]; then
                poll_device_check="$client_check"
            fi
            if client_check_is_fresh "$client_check" && client_check_has_device_evidence "$client_check" && client_check_has_current_dashboard_build "$client_check"; then
                if [[ "$REQUIRE_STANDALONE" != "true" ]] || client_check_has_standalone_evidence "$client_check"; then
                    if [[ "$REQUIRE_INTERACTION" != "true" ]] || client_check_has_interaction_evidence "$client_check"; then
                        record_client_check_result "$client_check"
                        return $?
                    fi
                fi
            fi
        done < <(client_check_entries "$client_check_payload")
        if [[ -n "$poll_seen_check" ]]; then
            last_seen_check="$poll_seen_check"
        fi
        if [[ -n "$poll_device_check" ]]; then
            last_device_check="$poll_device_check"
        fi

        now="$(date +%s)"
        if [[ "$now" -ge "$deadline" ]]; then
            break
        fi
        remaining=$(( deadline - now ))
        if [[ "$remaining" -gt 2 ]]; then
            sleep 2
        else
            sleep "$remaining"
        fi
    done

    if [[ -n "$last_device_check" ]]; then
        record_client_check_result "$last_device_check"
    elif [[ -n "$last_seen_check" ]]; then
        record_client_check_result "$last_seen_check"
    else
        record_client_check_result '{"seen":false}'
    fi
}

tailscale_hostname() {
    if ! command -v tailscale >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
        return 1
    fi
    tailscale status --self --json 2>/dev/null | jq -r '.Self.DNSName // ""' | sed 's/\.$//'
}

local_base_url_part() {
    local part="$1"
    node -e '
const url = new URL(process.argv[1]);
const part = process.argv[2];
if (part === "protocol") {
  process.stdout.write(url.protocol.replace(/:$/, ""));
} else if (part === "port") {
  process.stdout.write(url.port || (url.protocol === "https:" ? "443" : "80"));
}
' "$LOCAL_BASE_URL" "$part"
}

candidate_priority() {
    local host="$1"
    if [[ "$host" == 100.* ]]; then
        local second="${host#100.}"
        second="${second%%.*}"
        if [[ "$second" =~ ^[0-9]+$ && "$second" -ge 64 && "$second" -le 127 ]]; then
            echo 0
            return
        fi
    fi
    case "$host" in
        10.*|192.168.*|172.16.*|172.17.*|172.18.*|172.19.*|172.20.*|172.21.*|172.22.*|172.23.*|172.24.*|172.25.*|172.26.*|172.27.*|172.28.*|172.29.*|172.30.*|172.31.*) echo 1 ;;
        fd*) echo 1 ;;
        *) echo 2 ;;
    esac
}

include_device_interface() {
    local name="${1:-}"
    name="${name%%@*}"
    name="${name,,}"
    case "$name" in
        ""|lo|br-*|cni*|docker*|flannel*|hyper-v*|kube-ipvs*|nerdctl*|npcap\ loopback*|podman*|veth*|vethernet*|virbr*|virtualbox\ host-only*|vmware\ network\ adapter*)
            return 1
            ;;
    esac
    case "$name" in
        *"default switch"*|*"docker"*|*"hyper-v"*|*"loopback"*|*"microsoft wi-fi direct virtual adapter"*|*"nat network"*|*"nat switch"*|*"npcap"*|*"virtualbox host-only"*|*"vmware network adapter"*|*"wi-fi direct virtual adapter"*|*"wsl"*)
            return 1
            ;;
    esac
    return 0
}

candidate_device_hosts() {
    local -a hosts=()
    local -A seen=()
    if command -v ip >/dev/null 2>&1; then
        while read -r _ ifname family cidr _; do
            [[ "$family" == inet || "$family" == inet6 ]] || continue
            include_device_interface "$ifname" || continue
            hosts+=("${cidr%%/*}")
        done < <(ip -o addr show scope global up 2>/dev/null)
    fi
    if [[ "${#hosts[@]}" -eq 0 ]] && command -v hostname >/dev/null 2>&1; then
        while IFS= read -r host; do
            [[ -n "$host" ]] && hosts+=("$host")
        done < <(
            hostname -I 2>/dev/null | tr ' ' '\n'
        )
    fi

    printf '%s\n' "${hosts[@]}" | while IFS= read -r host; do
        [[ -z "$host" ]] && continue
        local lower="${host,,}"
        case "$lower" in
            127.*|"::1"|"0.0.0.0"|"::"|169.254.*|fe80:*) continue ;;
        esac
        if [[ -n "${seen[$host]:-}" ]]; then
            continue
        fi
        seen["$host"]=1
        printf '%s\t%s\n' "$(candidate_priority "$host")" "$host"
    done | sort -n -k1,1 -k2,2 | awk -F '\t' '
        $2 != "" {
            print $2
            exit
        }
    '
}

direct_lan_device_url() {
    local host
    host="$(candidate_device_hosts | head -n 1)"
    [[ -n "$host" ]] || return 1
    printf '%s://%s:%s' "$(local_base_url_part protocol)" "$(url_host "$host")" "$(local_base_url_part port)"
}

detected_device_url() {
    if [[ -n "$DEVICE_URL" ]]; then
        DEVICE_URL_SOURCE="explicit"
        normalize_base_url "$DEVICE_URL"
        return 0
    fi

    local hostname
    if ! hostname="$(tailscale_hostname)" || [[ -z "$hostname" || "$hostname" == "null" ]]; then
        local lan_url
        if lan_url="$(direct_lan_device_url)"; then
            DEVICE_URL_SOURCE="direct_lan"
            printf '%s' "$lan_url"
            return 0
        fi
        return 1
    fi

    DEVICE_URL_SOURCE="tailscale"
    local https_port="${SYSMON_HTTPS:-9443}"
    if [[ "$https_port" == "443" ]]; then
        printf 'https://%s' "$hostname"
    else
        printf 'https://%s:%s' "$hostname" "$https_port"
    fi
}

print_device_qr() {
    local url="$1"
    if ! command -v qrencode >/dev/null 2>&1 || [[ -z "$url" ]]; then
        return 0
    fi
    echo ""
    echo "Scan from your phone or device:"
    qrencode -t ANSIUTF8 "$url" || true
}

check_pwa_install_assets() {
    local base_url="$1"
    local html
    local manifest
    local app_js
    local sw_js

    html="$(fetch "${base_url}/")"
    grep -q '<title>Sysmon</title>' <<< "$html"
    grep -q '<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">' <<< "$html"
    grep -q '<meta name="apple-mobile-web-app-capable" content="yes">' <<< "$html"
    grep -q '<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">' <<< "$html"
    grep -q '<meta name="apple-mobile-web-app-title" content="Sysmon">' <<< "$html"
    grep -q '<link rel="manifest" href="/manifest.json">' <<< "$html"
    grep -q '<link rel="apple-touch-icon" sizes="180x180" href="/icon-180.png">' <<< "$html"

    manifest="$(fetch "${base_url}/manifest.json")"
    grep -q '"short_name"[[:space:]]*:[[:space:]]*"Sysmon"' <<< "$manifest"
    grep -q '"id"[[:space:]]*:[[:space:]]*"/"' <<< "$manifest"
    grep -q '"display"[[:space:]]*:[[:space:]]*"standalone"' <<< "$manifest"
    grep -q '"start_url"[[:space:]]*:[[:space:]]*"/"' <<< "$manifest"
    grep -q '"scope"[[:space:]]*:[[:space:]]*"/"' <<< "$manifest"
    grep -q '"background_color"[[:space:]]*:[[:space:]]*"#080b10"' <<< "$manifest"
    grep -q '"theme_color"[[:space:]]*:[[:space:]]*"#080b10"' <<< "$manifest"
    grep -q '"src"[[:space:]]*:[[:space:]]*"/icon-180.png"' <<< "$manifest"
    grep -q '"src"[[:space:]]*:[[:space:]]*"/icon-512.png"' <<< "$manifest"

    app_js="$(fetch "${base_url}/app.js")"
    grep -q 'fetchMetrics' <<< "$app_js"
    grep -q 'clientCheckPayload' <<< "$app_js"

    sw_js="$(fetch "${base_url}/sw.js")"
    grep -q 'isLiveEndpoint' <<< "$sw_js"
    grep -q '"/manifest.json"' <<< "$sw_js"
    grep -q '"/icon-180.png"' <<< "$sw_js"
    grep -q '"/icon-512.png"' <<< "$sw_js"

    check_png_dimensions "${base_url}/icon-180.png" 180
    check_png_dimensions "${base_url}/icon-512.png" 512
}

check_png_dimensions() {
    local url="$1"
    local expected_size="$2"
    node -e '
(async () => {
  const url = process.argv[1];
  const expectedSize = Number(process.argv[2]);
  const response = await fetch(url);
  if (!response.ok) {
    throw new Error(`${url} returned HTTP ${response.status}`);
  }
  const data = Buffer.from(await response.arrayBuffer());
  const signature = "89504e470d0a1a0a";
  if (data.length < 24 || data.subarray(0, 8).toString("hex") !== signature) {
    throw new Error(`${url} is not a PNG`);
  }
  if (data.subarray(12, 16).toString("ascii") !== "IHDR") {
    throw new Error(`${url} has no PNG IHDR header`);
  }
  const width = data.readUInt32BE(16);
  const height = data.readUInt32BE(20);
  if (width !== expectedSize || height !== expectedSize) {
    throw new Error(`${url} is ${width}x${height}, want ${expectedSize}x${expectedSize}`);
  }
})().catch((error) => {
  console.error(error.message);
  process.exit(1);
});
' "$url" "$expected_size"
}

check_settings_same_origin_roundtrip() {
    local base_url="$1"
    local settings
    local update
    local posted
    local status_after_settings

    settings="$(fetch "${base_url}/api/settings")"
    update="$(node -e '
const settings = JSON.parse(process.argv[1]);
const thresholds = settings.thresholds || {};
const update = {
  dim: Boolean(settings.dim),
  shift: Boolean(settings.shift),
  refresh_ms: Number(settings.refresh_ms),
  panel: String(settings.panel || "all"),
  thresholds: {
    cpu_warn: Number(thresholds.cpu_warn),
    memory_warn: Number(thresholds.memory_warn),
    disk_warn: Number(thresholds.disk_warn),
    gpu_warn: Number(thresholds.gpu_warn),
    temp_warn_c: Number(thresholds.temp_warn_c),
  },
};
process.stdout.write(JSON.stringify(update));
' "$settings")"

    posted="$(curl -fsS --max-time 5 \
        -H "Content-Type: application/json" \
        -H "Origin: ${base_url}" \
        --data "$update" \
        "${base_url}/api/settings")"
    node -e '
const got = JSON.parse(process.argv[1]);
const want = JSON.parse(process.argv[2]);
const gotThresholds = got.thresholds || {};
for (const key of ["dim", "shift", "refresh_ms", "panel"]) {
  if (got[key] !== want[key]) {
    throw new Error(`settings ${key} roundtrip mismatch: got ${got[key]}, want ${want[key]}`);
  }
}
for (const key of ["cpu_warn", "memory_warn", "disk_warn", "gpu_warn", "temp_warn_c"]) {
  if (gotThresholds[key] !== want.thresholds[key]) {
    throw new Error(`settings thresholds.${key} roundtrip mismatch: got ${gotThresholds[key]}, want ${want.thresholds[key]}`);
  }
}
' "$posted" "$update"

    status_after_settings="$(fetch "${base_url}/api/status")"
    node -e '
const status = JSON.parse(process.argv[1]);
const want = JSON.parse(process.argv[2]);
const got = status.settings || {};
const gotThresholds = got.thresholds || {};
for (const key of ["dim", "shift", "refresh_ms", "panel"]) {
  if (got[key] !== want[key]) {
    throw new Error(`settings status.${key} roundtrip mismatch: got ${got[key]}, want ${want[key]}`);
  }
}
for (const key of ["cpu_warn", "memory_warn", "disk_warn", "gpu_warn", "temp_warn_c"]) {
  if (gotThresholds[key] !== want.thresholds[key]) {
    throw new Error(`settings status.thresholds.${key} roundtrip mismatch: got ${gotThresholds[key]}, want ${want.thresholds[key]}`);
  }
}
' "$status_after_settings" "$update"
}

check_client_check_same_origin_roundtrip() {
    local base_url="$1"
    local update
    local posted
    local status_after_client_check

    update="{\"dashboard_build\":\"${EXPECTED_DASHBOARD_BUILD}\",\"interaction\":\"status_strip_tap\",\"viewport_width\":390,\"viewport_height\":844,\"screen_width\":390,\"screen_height\":844,\"device_pixel_ratio\":3,\"touch_points\":5,\"display_mode\":\"browser\",\"standalone\":false,\"visibility\":\"visible\",\"orientation\":\"verify-public\"}"
    posted="$(curl -fsS --max-time 5 \
        -H "Content-Type: application/json" \
        -H "Origin: ${base_url}" \
        --data "$update" \
        "${base_url}/api/client-check")"
    node -e '
const got = JSON.parse(process.argv[1]);
const want = JSON.parse(process.argv[2]);
if (got.seen !== true) {
  throw new Error("client-check roundtrip did not mark the client as seen");
}
for (const key of ["dashboard_build", "interaction", "viewport_width", "viewport_height", "screen_width", "screen_height", "device_pixel_ratio", "touch_points", "display_mode", "standalone", "visibility", "orientation"]) {
  if (got[key] !== want[key]) {
    throw new Error(`client-check ${key} roundtrip mismatch: got ${got[key]}, want ${want[key]}`);
  }
}
if (!got.last_seen || Number.isNaN(Date.parse(got.last_seen))) {
  throw new Error("client-check roundtrip did not return a valid last_seen timestamp");
}
' "$posted" "$update"

    status_after_client_check="$(fetch "${base_url}/api/status")"
    node -e '
const status = JSON.parse(process.argv[1]);
const want = JSON.parse(process.argv[2]);
const got = status.client_check || {};
if (got.seen !== true) {
  throw new Error("status client_check did not mark the client as seen");
}
for (const key of ["dashboard_build", "interaction", "viewport_width", "viewport_height", "screen_width", "screen_height", "device_pixel_ratio", "touch_points", "display_mode", "standalone", "visibility", "orientation"]) {
  if (got[key] !== want[key]) {
    throw new Error(`status client_check ${key} roundtrip mismatch: got ${got[key]}, want ${want[key]}`);
  }
}
if (!got.last_seen || Number.isNaN(Date.parse(got.last_seen))) {
  throw new Error("status client_check roundtrip did not return a valid last_seen timestamp");
}
' "$status_after_client_check" "$update"
}

require_cmd curl
require_cmd node

cd "$SCRIPT_DIR"

mkdir -p "$(dirname "$REPORT_FILE")"
: > "$REPORT_FILE"
REPORT_READY=true
report "sysmon-agent deployed verification report"
report "started_at=$(date -Is)"
report "mode=deployed"
report "local_base_url=$LOCAL_BASE_URL"
report "hold_seconds=$HOLD_SECONDS"
report "require_device=$REQUIRE_DEVICE"
report "require_standalone=$REQUIRE_STANDALONE"
report "require_interaction=$REQUIRE_INTERACTION"

echo "Checking deployed sysmon-agent at $LOCAL_BASE_URL..."
health_json="$(fetch "${LOCAL_BASE_URL}/healthz")"
grep -q '"status":"ok"' <<< "$health_json"
report "healthz=pass"

ready_json="$(fetch "${LOCAL_BASE_URL}/readyz")"
grep -q '"status":"ok"' <<< "$ready_json"
grep -q '"metrics":true' <<< "$ready_json"
report "readyz=pass"
report_readyz_collection_errors "readyz" "$ready_json"

assert_header_contains "${LOCAL_BASE_URL}/api/status" "Cache-Control" "no-store"
assert_header_contains "${LOCAL_BASE_URL}/api/status" "Content-Security-Policy" "default-src 'self'"
assert_header_contains "${LOCAL_BASE_URL}/" "Cache-Control" "no-cache"
assert_header_contains "${LOCAL_BASE_URL}/" "X-Content-Type-Options" "nosniff"
report "headers=pass"

echo "Checking deployed API schema..."
node verify-api.mjs "$LOCAL_BASE_URL"
report "api_schema=pass"

echo "Checking deployed dashboard/PWA install assets..."
check_pwa_install_assets "$LOCAL_BASE_URL"
report "dashboard_assets=pass"

device_url_file="$(mktemp)"
if detected_device_url > "$device_url_file"; then
    device_url="$(<"$device_url_file")"
else
    device_url=""
fi
rm -f "$device_url_file"
if [[ -z "$device_url" ]]; then
    echo "Could not determine device URL. Set SYSMON_DEPLOY_VERIFY_DEVICE_URL=https://TAILSCALE_HOST:9443/ or bind the service to a LAN address." >&2
    report "device_url=unavailable"
    exit 1
fi
device_url="$(normalize_base_url "$device_url")"
report "device_url=$device_url"
if [[ -n "$DEVICE_URL_SOURCE" ]]; then
    report "device_url_source=$DEVICE_URL_SOURCE"
fi

echo "Checking published device URL (${DEVICE_URL_SOURCE:-unknown}): $device_url"
public_health_json="$(fetch "${device_url}/healthz")"
grep -q '"status":"ok"' <<< "$public_health_json"
report "public_healthz=pass"

public_ready_json="$(fetch "${device_url}/readyz")"
grep -q '"status":"ok"' <<< "$public_ready_json"
grep -q '"metrics":true' <<< "$public_ready_json"
report "public_readyz=pass"
report_readyz_collection_errors "public_readyz" "$public_ready_json"

assert_header_contains "${device_url}/api/status" "Cache-Control" "no-store"
assert_header_contains "${device_url}/api/status" "Content-Security-Policy" "default-src 'self'"
assert_header_contains "${device_url}/" "Cache-Control" "no-cache"
assert_header_contains "${device_url}/" "X-Content-Type-Options" "nosniff"
report "public_headers=pass"

echo "Checking published API schema..."
node verify-api.mjs "$device_url"
report "public_api_schema=pass"

public_status_json="$(fetch "${device_url}/api/status")"
EXPECTED_DASHBOARD_BUILD="$(json_field "$public_status_json" dashboard_build 2>/dev/null || true)"
if [[ -z "$EXPECTED_DASHBOARD_BUILD" ]]; then
    echo "Published /api/status did not report dashboard_build." >&2
    report "dashboard_build=unavailable"
    exit 1
fi
report "dashboard_build=$EXPECTED_DASHBOARD_BUILD"

echo "Checking published interactive settings controls..."
check_settings_same_origin_roundtrip "$device_url"
report "public_settings_roundtrip=pass"

echo "Checking published dashboard/PWA install assets..."
check_pwa_install_assets "$device_url"
report "public_dashboard_assets=pass"

initial_client_check_payload="$(fetch_client_check_payload)"
initial_device_client_check="$(latest_device_client_check "$initial_client_check_payload" 2>/dev/null || true)"
if [[ -n "$initial_device_client_check" ]] && INITIAL_CLIENT_LAST_SEEN_MS="$(json_millis_field "$initial_device_client_check" last_seen 2>/dev/null)" && [[ -n "$INITIAL_CLIENT_LAST_SEEN_MS" ]]; then
    report "initial_device_client_seen=true"
    if initial_last_seen="$(json_field "$initial_device_client_check" last_seen 2>/dev/null)" && [[ -n "$initial_last_seen" ]]; then
        report "initial_device_client_last_seen=$initial_last_seen"
    fi
else
    INITIAL_CLIENT_LAST_SEEN_MS=""
    report "initial_device_client_seen=false"
fi

echo "Checking published client-check controls..."
check_client_check_same_origin_roundtrip "$device_url"
report "public_client_check_roundtrip=pass"

if [[ "$HOLD_SECONDS" -eq 0 ]]; then
    report "device_client_seen=not_attempted"
    if [[ "$REQUIRE_DEVICE" == "true" ]]; then
        echo "required device client check needs SYSMON_DEPLOY_VERIFY_HOLD greater than zero" >&2
        exit 1
    fi
    echo "ok: deployed sysmon-agent checks passed without device hold"
    exit 0
fi

echo ""
echo "Open from the installed Home Screen Sysmon app on your phone or device during the hold:"
echo "  $device_url"
echo "On the device, confirm the status strip shows app mode, then tap the status strip once to refresh the client check."
print_device_qr "$device_url"
echo "Holding deployed sysmon-agent verification for ${HOLD_SECONDS}s. Press Ctrl-C to stop early."
HOLD_STARTED_MS="$(node -e 'process.stdout.write(String(Date.now()))')"
report "hold_started_at=$(date -Is)"
report "hold_started_ms=$HOLD_STARTED_MS"
report "device_hold_instruction=home_screen_open_status_strip_tap"
wait_for_client_check_evidence

echo "ok: deployed sysmon-agent checks passed"
