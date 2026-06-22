#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

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

PORT="${SYSMON_VERIFY_PORT:-19099}"
BIND="${SYSMON_VERIFY_BIND:-127.0.0.1}"
CHECK_HOST="${SYSMON_VERIFY_HOST:-}"
if [[ -z "$CHECK_HOST" ]]; then
    case "$BIND" in
        "0.0.0.0") CHECK_HOST="127.0.0.1" ;;
        "::"|"[::]") CHECK_HOST="::1" ;;
        *) CHECK_HOST="$BIND" ;;
    esac
fi
BASE_URL="http://$(url_host "$CHECK_HOST"):${PORT}"
HOLD_SECONDS="${SYSMON_VERIFY_HOLD_SECONDS:-${SYSMON_VERIFY_HOLD:-0}}"
DEVICE_URL="${SYSMON_VERIFY_DEVICE_URL:-}"
REQUIRE_DEVICE="${SYSMON_VERIFY_REQUIRE_DEVICE:-0}"
if ! [[ "$HOLD_SECONDS" =~ ^[0-9]+$ ]]; then
    echo "SYSMON_VERIFY_HOLD must be a non-negative number of seconds" >&2
    exit 1
fi
case "${REQUIRE_DEVICE,,}" in
    0|false|no|"") REQUIRE_DEVICE=false ;;
    1|true|yes) REQUIRE_DEVICE=true ;;
    *)
        echo "SYSMON_VERIFY_REQUIRE_DEVICE must be 0/1, true/false, or yes/no" >&2
        exit 1
        ;;
esac
SETTINGS_DIR="$(mktemp -d -t sysmon-settings.XXXXXX)"
SETTINGS_FILE="${SETTINGS_DIR}/settings.json"
BINARY="${SETTINGS_DIR}/sysmon-agent"
LOG_FILE="$(mktemp -t sysmon-agent.XXXXXX.log)"
REPORT_FILE="${SYSMON_VERIFY_REPORT:-/tmp/sysmon-agent-verify-report.txt}"
REPORT_READY=false
HOLD_STARTED_MS=""
PID=""

report() {
    if [[ "$REPORT_READY" == "true" ]]; then
        printf '%s\n' "$1" >> "$REPORT_FILE"
    fi
}

cleanup() {
    local exit_code=$?
    if [[ -n "$PID" ]] && kill -0 "$PID" 2>/dev/null; then
        kill "$PID" 2>/dev/null || true
        wait "$PID" 2>/dev/null || true
    fi
    rm -rf "$SETTINGS_DIR"
    rm -f "$LOG_FILE"
    if [[ "$REPORT_READY" == "true" ]]; then
        report "completed_at=$(date -Is)"
        if [[ "$exit_code" -eq 0 ]]; then
            report "result=pass"
        else
            report "result=fail"
        fi
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
    curl -fsS --max-time 4 "$1"
}

explain_listen_block_if_present() {
    if grep -Eqi 'listen tcp .*socket: operation not permitted|listen tcp .*permission denied|bind: permission denied' "$LOG_FILE"; then
        report "listen=blocked_permission"
        echo "" >&2
        echo "TCP listening is blocked in this environment." >&2
        echo "Run ./verify-no-listen.sh here, or rerun ./verify.sh on the target host where local sockets are permitted." >&2
    fi
}

json_field() {
    local json="$1"
    local field="$2"
    if ! command -v node >/dev/null 2>&1; then
        return 1
    fi
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
    if ! command -v node >/dev/null 2>&1; then
        return 1
    fi
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
    if ! command -v node >/dev/null 2>&1; then
        report "${prefix}_collection_errors=skipped_node_unavailable"
        return
    fi
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

now_millis() {
    if command -v node >/dev/null 2>&1; then
        node -e 'process.stdout.write(String(Date.now()))'
    else
        printf '%s000' "$(date +%s)"
    fi
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

client_check_is_fresh() {
    local json="$1"
    local last_seen_ms
    last_seen_ms="$(json_millis_field "$json" last_seen 2>/dev/null || true)"
    [[ "$last_seen_ms" =~ ^[0-9]+$ ]] || return 1
    [[ "$HOLD_STARTED_MS" =~ ^[0-9]+$ && "$last_seen_ms" -ge "$HOLD_STARTED_MS" ]] || return 1
}

fetch_client_check_payload() {
    local payload
    local status_payload
    if command -v node >/dev/null 2>&1; then
        payload="$(fetch "${BASE_URL}/api/client-checks" 2>/dev/null || true)"
        if [[ -z "$payload" ]]; then
            payload="$(fetch "${BASE_URL}/api/client-check" 2>/dev/null || true)"
        fi
        status_payload="$(fetch "${BASE_URL}/api/status" 2>/dev/null || true)"
        merge_client_check_payloads "$payload" "$status_payload"
        return 0
    fi
    fetch "${BASE_URL}/api/client-check" 2>/dev/null || true
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
    if ! command -v node >/dev/null 2>&1; then
        if grep -q '"seen":' <<< "$json" && ! grep -q '"checks"' <<< "$json"; then
            printf '%s\n' "$json"
        fi
        return 0
    fi
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

headers() {
    curl -fsS --max-time 4 -D - -o /dev/null "$1" | tr -d '\r'
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

    check_icon_asset "${base_url}/icon-180.png" 180
    check_icon_asset "${base_url}/icon-512.png" 512
}

check_icon_asset() {
    local url="$1"
    local expected_size="$2"
    if command -v node >/dev/null 2>&1; then
        check_png_dimensions "$url" "$expected_size"
        return
    fi
    fetch "$url" >/dev/null
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

is_wildcard_bind() {
    case "$BIND" in
        ""|"*"|"0.0.0.0"|"::"|"[::]") return 0 ;;
        *) return 1 ;;
    esac
}

is_loopback_bind() {
    case "$BIND" in
        "127.0.0.1"|"localhost"|"::1"|"[::1]") return 0 ;;
        *) return 1 ;;
    esac
}

normalize_bind_host() {
    local host="$1"
    if [[ "$host" == \[*\] ]]; then
        host="${host#[}"
        host="${host%]}"
    fi
    printf '%s\n' "$host"
}

candidate_priority() {
    local host="${1,,}"
    if [[ "$host" == *:* ]]; then
        case "$host" in
            fc*|fd*) echo 1 ;;
            *) echo 3 ;;
        esac
        return 0
    fi

    local oct1="" oct2=""
    IFS=. read -r oct1 oct2 _ <<< "$host"
    if [[ ! "$oct1" =~ ^[0-9]+$ || ! "$oct2" =~ ^[0-9]+$ ]]; then
        echo 3
        return 0
    fi

    local first=$((10#$oct1))
    local second=$((10#$oct2))
    if [[ "$first" -eq 100 && "$second" -ge 64 && "$second" -le 127 ]]; then
        echo 0
    elif [[ "$first" -eq 10 || ( "$first" -eq 192 && "$second" -eq 168 ) || ( "$first" -eq 172 && "$second" -ge 16 && "$second" -le 31 ) ]]; then
        echo 1
    else
        echo 2
    fi
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
    if is_loopback_bind; then
        return 0
    fi
    if ! is_wildcard_bind; then
        normalize_bind_host "$BIND"
        return 0
    fi

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
            count += 1
            if (count >= 4) {
                exit
            }
        }
    '
}

print_device_urls() {
    local url
    while IFS= read -r url; do
        [[ -z "$url" ]] && continue
        echo "  $url"
    done < <(candidate_device_urls)
}

candidate_device_urls() {
    local host
    local printed=false
    while IFS= read -r host; do
        [[ -z "$host" ]] && continue
        printed=true
        echo "http://$(url_host "$host"):${PORT}/"
    done < <(candidate_device_hosts)
    if [[ "$printed" != "true" ]]; then
        echo "http://HOST_IP:${PORT}/"
    fi
}

print_device_qr() {
    if ! command -v qrencode >/dev/null 2>&1; then
        return 0
    fi
    local url
    url="${1:-}"
    if [[ -z "$url" ]]; then
        url="$(candidate_device_urls | head -n 1)"
    fi
    if [[ -z "$url" || "$url" == *HOST_IP* ]]; then
        return 0
    fi
    echo ""
    echo "Scan first URL from your phone:"
    qrencode -t ANSIUTF8 "$url" || true
}

require_cmd go
require_cmd curl

cd "$SCRIPT_DIR"

mkdir -p "$(dirname "$REPORT_FILE")"
: > "$REPORT_FILE"
REPORT_READY=true
report "sysmon-agent verification report"
report "started_at=$(date -Is)"
report "bind=$BIND"
report "port=$PORT"
report "base_url=$BASE_URL"
report "settings_file=$SETTINGS_FILE"
report "hold_seconds=$HOLD_SECONDS"
report "require_device=$REQUIRE_DEVICE"
report "installed_device_home_screen=not_verified"
if [[ -n "$DEVICE_URL" ]]; then
    report "configured_device_url=$DEVICE_URL"
fi

export GOCACHE="${GOCACHE:-/tmp/go-build-cache}"
mkdir -p "$GOCACHE"

echo "Building sysmon-agent..."
go build -o "$BINARY" .
report "build=pass"

if command -v node >/dev/null 2>&1; then
    echo "Checking dashboard JavaScript..."
    node --check static/app.js
    node --check static/sw.js
    node verify-dashboard.mjs
    render_output="$(node verify-render.mjs)"
    echo "$render_output"
    report "dashboard_runtime=pass"
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
    report "dashboard_runtime=skipped_node_unavailable"
    report "dashboard_render=skipped_node_unavailable"
fi

echo "Starting sysmon-agent on ${BIND}:${PORT}; checking ${BASE_URL}..."
"$BINARY" -bind "$BIND" -port "$PORT" -settings "$SETTINGS_FILE" >"$LOG_FILE" 2>&1 &
PID="$!"
report "agent_spawned=pass"

ready=false
for _ in {1..40}; do
    if fetch "${BASE_URL}/healthz" >/dev/null 2>&1; then
        ready=true
        break
    fi
    if ! kill -0 "$PID" 2>/dev/null; then
        echo "sysmon-agent exited early:" >&2
        sed -n '1,120p' "$LOG_FILE" >&2
        explain_listen_block_if_present
        report "agent_ready=fail"
        exit 1
    fi
    sleep 0.25
done

if [[ "$ready" != "true" ]]; then
    echo "sysmon-agent did not become ready:" >&2
    sed -n '1,120p' "$LOG_FILE" >&2
    explain_listen_block_if_present
    report "agent_ready=fail"
    exit 1
fi
report "agent_ready=pass"

echo "Checking /healthz..."
fetch "${BASE_URL}/healthz" | grep -q '"status":"ok"'
report "healthz=pass"

echo "Checking /readyz..."
ready_json="$(fetch "${BASE_URL}/readyz")"
grep -q '"status":"ok"' <<< "$ready_json"
grep -q '"metrics":true' <<< "$ready_json"
report "readyz=pass"
report_readyz_collection_errors "readyz" "$ready_json"

echo "Checking /api/metrics..."
metrics_json="$(fetch "${BASE_URL}/api/metrics")"
echo "$metrics_json" | grep -q '"hostname"'
echo "$metrics_json" | grep -q '"cpu_percent"'
echo "$metrics_json" | grep -q '"memory"'
report "metrics=pass"

echo "Checking /api/status..."
status_json="$(fetch "${BASE_URL}/api/status")"
echo "$status_json" | grep -q '"status":"ok"'
echo "$status_json" | grep -q '"dashboard_build"'
echo "$status_json" | grep -q '"uptime_seconds"'
echo "$status_json" | grep -q '"refresh_options_ms"'
echo "$status_json" | grep -q '"settings"'
report "status=pass"

echo "Checking /api/client-check..."
client_check_json="$(fetch "${BASE_URL}/api/client-check")"
echo "$client_check_json" | grep -q '"seen":false'
report "client_check=pass"

if command -v node >/dev/null 2>&1; then
    echo "Checking live API schema..."
    SYSMON_VERIFY_BASE_URL="$BASE_URL" node verify-api.mjs --settings-roundtrip
    report "api_schema=pass"
    report "settings_roundtrip=pass"
else
    report "api_schema=skipped_node_unavailable"
fi

echo "Checking response headers..."
assert_header_contains "${BASE_URL}/api/status" "Cache-Control" "no-store"
assert_header_contains "${BASE_URL}/api/status" "Content-Security-Policy" "default-src 'self'"
assert_header_contains "${BASE_URL}/" "Cache-Control" "no-cache"
assert_header_contains "${BASE_URL}/" "X-Content-Type-Options" "nosniff"
report "headers=pass"

echo "Checking dashboard assets..."
check_pwa_install_assets "$BASE_URL"
report "dashboard_assets=pass"

echo "Checking interactive settings API..."
settings_json="$(curl -fsS --max-time 4 \
    -H 'Content-Type: application/json' \
    -H "Origin: ${BASE_URL}" \
    -d '{"dim":true,"shift":true,"refresh_ms":2000,"panel":"gpu","thresholds":{"cpu_warn":80,"memory_warn":75,"disk_warn":85,"gpu_warn":80,"temp_warn_c":75}}' \
    "${BASE_URL}/api/settings")"
echo "$settings_json" | grep -q '"dim":true'
echo "$settings_json" | grep -q '"shift":true'
echo "$settings_json" | grep -q '"refresh_ms":2000'
echo "$settings_json" | grep -q '"panel":"gpu"'
echo "$settings_json" | grep -q '"cpu_warn":80'
echo "$settings_json" | grep -q '"memory_warn":75'
echo "$settings_json" | grep -q '"disk_warn":85'
echo "$settings_json" | grep -q '"gpu_warn":80'
echo "$settings_json" | grep -q '"temp_warn_c":75'
report "settings_post=pass"

echo "Checking settings readback and persistence..."
settings_readback="$(fetch "${BASE_URL}/api/settings")"
echo "$settings_readback" | grep -q '"dim":true'
echo "$settings_readback" | grep -q '"shift":true'
echo "$settings_readback" | grep -q '"refresh_ms":2000'
echo "$settings_readback" | grep -q '"panel":"gpu"'
echo "$settings_readback" | grep -q '"cpu_warn":80'
echo "$settings_readback" | grep -q '"memory_warn":75'
echo "$settings_readback" | grep -q '"disk_warn":85'
echo "$settings_readback" | grep -q '"gpu_warn":80'
echo "$settings_readback" | grep -q '"temp_warn_c":75'
test -s "$SETTINGS_FILE"
grep -q '"shift": true' "$SETTINGS_FILE"
grep -q '"panel": "gpu"' "$SETTINGS_FILE"
grep -q '"cpu_warn": 80' "$SETTINGS_FILE"
grep -q '"temp_warn_c": 75' "$SETTINGS_FILE"
report "settings_persistence=pass"

echo ""
echo "ok: sysmon-agent smoke test passed"
report "smoke=pass"
if [[ "$HOLD_SECONDS" -gt 0 ]]; then
    report "device_hold=offered"
    DEVICE_CLIENT_EXPECTED=false
    if [[ -n "$DEVICE_URL" ]]; then
        DEVICE_CLIENT_EXPECTED=true
        echo "Open from your phone or device while this hold is active:"
        echo "  $DEVICE_URL"
        report "device_url=$DEVICE_URL"
        print_device_qr "$DEVICE_URL"
    elif [[ "$BIND" == "127.0.0.1" || "$BIND" == "localhost" || "$BIND" == "::1" || "$BIND" == "[::1]" ]]; then
        echo "Agent is bound to loopback. For a device LAN check, rerun with SYSMON_VERIFY_BIND=0.0.0.0."
        report "device_urls=loopback_bind_unavailable"
    else
        DEVICE_CLIENT_EXPECTED=true
        echo "Open from your phone or device while this hold is active:"
        print_device_urls
        while IFS= read -r url; do
            [[ -z "$url" ]] && continue
            report "device_url=$url"
        done < <(candidate_device_urls)
        print_device_qr
    fi
    echo "Holding sysmon-agent for ${HOLD_SECONDS}s. Press Ctrl-C to stop early."
    HOLD_STARTED_MS="$(now_millis)"
    report "hold_started_ms=$HOLD_STARTED_MS"
    sleep "$HOLD_SECONDS"
    if [[ "$DEVICE_CLIENT_EXPECTED" == "true" ]]; then
        client_check_payload="$(fetch_client_check_payload)"
        seen_client_check=""
        device_client_check=""
        stale_device_client_check=""
        while IFS= read -r client_check_after; do
            [[ -n "$client_check_after" ]] || continue
            if ! grep -q '"seen":true' <<< "$client_check_after"; then
                continue
            fi
            if client_check_is_fresh "$client_check_after" && [[ -z "$seen_client_check" ]]; then
                seen_client_check="$client_check_after"
            fi
            if client_check_has_device_evidence "$client_check_after"; then
                if client_check_is_fresh "$client_check_after"; then
                    device_client_check="$client_check_after"
                    break
                fi
                if [[ -z "$stale_device_client_check" ]]; then
                    stale_device_client_check="$client_check_after"
                fi
            fi
        done < <(client_check_entries "$client_check_payload")
        if [[ -n "$device_client_check" ]]; then
            report_client_check_fields "$device_client_check"
            echo "Observed a fresh device dashboard client check during the hold."
            report "device_client_seen=pass"
        elif [[ -n "$stale_device_client_check" ]]; then
            report_client_check_fields "$stale_device_client_check"
            echo "Observed a device dashboard client check, but it was not fresh for this hold."
            report "device_client_seen=stale_client"
            if [[ "$REQUIRE_DEVICE" == "true" ]]; then
                exit 1
            fi
        elif [[ -n "$seen_client_check" ]]; then
            report_client_check_fields "$seen_client_check"
            echo "Observed a dashboard client check, but it did not look like a phone or device client."
            report "device_client_seen=unexpected_client"
            if [[ "$REQUIRE_DEVICE" == "true" ]]; then
                exit 1
            fi
        else
            echo "No dashboard client check was observed during the hold."
            report "device_client_seen=not_observed"
            if [[ "$REQUIRE_DEVICE" == "true" ]]; then
                exit 1
            fi
        fi
    else
        report "device_client_seen=not_attempted_loopback_bind"
        if [[ "$REQUIRE_DEVICE" == "true" ]]; then
            exit 1
        fi
    fi
else
    echo "For a device check, rerun with SYSMON_VERIFY_BIND=0.0.0.0 SYSMON_VERIFY_HOLD=120 SYSMON_VERIFY_REQUIRE_DEVICE=1."
    report "device_client_seen=not_attempted"
    if [[ "$REQUIRE_DEVICE" == "true" ]]; then
        exit 1
    fi
fi
