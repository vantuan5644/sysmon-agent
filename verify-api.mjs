#!/usr/bin/env node

const defaultBaseURL = process.env.SYSMON_VERIFY_BASE_URL || "http://127.0.0.1:19099";
const args = process.argv.slice(2);
const sampleMode = args.includes("--sample");
const settingsRoundTrip = args.includes("--settings-roundtrip");
const clientCheckRoundTrip = args.includes("--client-check-roundtrip");
const baseURL = normalizeBaseURL(args.find((arg) => !arg.startsWith("--")) || defaultBaseURL);
const timeoutMS = 5000;
const dashboardBuild = "sysmon-static-v112";
const deviceUserAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1";
const roundTripSettings = {
  dim: true,
  shift: true,
  refresh_ms: 2000,
  panel: "gpu",
  thresholds: {
    cpu_warn: 80,
    memory_warn: 75,
    disk_warn: 85,
    gpu_warn: 80,
    temp_warn_c: 75,
  },
};
const clientCheckPayload = {
  dashboard_build: dashboardBuild,
  interaction: "status_strip_tap",
  viewport_width: 390,
  viewport_height: 844,
  screen_width: 390,
  screen_height: 844,
  device_pixel_ratio: 3,
  touch_points: 5,
  display_mode: "standalone",
  standalone: true,
  visibility: "visible",
  orientation: "portrait-primary",
};

if (sampleMode) {
  validateHealth({ status: "ok" });
  validateReadiness(sampleReadiness());
  validateStatus(sampleStatus());
  validateMetrics(sampleMetrics());
  validateSettings(sampleSettings());
  validateClientCheck(sampleClientCheck(false));
  validateClientCheckHistory(sampleClientCheckHistory(false));
  if (settingsRoundTrip) {
    validateSettings(roundTripSampleSettings(), roundTripSettings);
    validateStatus(sampleStatus(roundTripSampleSettings()), roundTripSettings);
  }
  if (clientCheckRoundTrip) {
    validateClientCheck(sampleClientCheck(true), clientCheckPayload);
    validateDeviceClientCheck(sampleClientCheck(true), clientCheckPayload);
    validateClientCheckHistory(sampleClientCheckHistory(true), clientCheckPayload);
  }
  console.log("ok: API schema sample validation passed");
} else {
  const health = await fetchJSON("/healthz");
  validateHealth(health);

  const readiness = await fetchJSON("/readyz");
  validateReadiness(readiness);

  const status = await fetchJSON("/api/status");
  validateStatus(status);

  const metrics = await fetchJSON("/api/metrics");
  validateMetrics(metrics);

  const settings = await fetchJSON("/api/settings");
  validateSettings(settings);

  const clientCheck = await fetchJSON("/api/client-check");
  validateClientCheck(clientCheck);

  const clientCheckHistory = await fetchJSON("/api/client-checks");
  validateClientCheckHistory(clientCheckHistory);

  if (settingsRoundTrip) {
    await assertSettingsRejectsMissingOrigin();

    const updatedSettings = await fetchJSON("/api/settings", {
      method: "POST",
      headers: browserPostHeaders(),
      body: JSON.stringify(roundTripSettings),
    });
    validateSettings(updatedSettings, roundTripSettings);

    const readbackSettings = await fetchJSON("/api/settings");
    validateSettings(readbackSettings, roundTripSettings);

    const statusAfterSettings = await fetchJSON("/api/status");
    validateStatus(statusAfterSettings, roundTripSettings);
  }

  if (clientCheckRoundTrip) {
    await assertClientCheckRejectsMissingOrigin();

    const recordedClientCheck = await fetchJSON("/api/client-check", {
      method: "POST",
      headers: browserPostHeaders(),
      body: JSON.stringify(clientCheckPayload),
    });
    validateClientCheck(recordedClientCheck, clientCheckPayload);

    const readbackClientCheck = await fetchJSON("/api/client-check");
    validateClientCheck(readbackClientCheck, clientCheckPayload);

    const readbackClientCheckHistory = await fetchJSON("/api/client-checks");
    validateClientCheckHistory(readbackClientCheckHistory, clientCheckPayload);

    const statusAfterClientCheck = await fetchJSON("/api/status");
    validateStatus(statusAfterClientCheck);
    validateClientCheck(statusAfterClientCheck.client_check, clientCheckPayload);
    validateDeviceClientCheck(statusAfterClientCheck.device_client_check, clientCheckPayload);
  }

  console.log(`ok: API schema validation passed for ${baseURL}`);
}

function normalizeBaseURL(value) {
  return String(value || "").replace(/\/+$/, "");
}

async function fetchJSON(path, options = {}) {
  if (!("fetch" in globalThis)) {
    throw new Error("Node fetch API is unavailable; use Node 18 or newer");
  }
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMS);
  try {
    const response = await fetch(`${baseURL}${path}`, {
      cache: "no-store",
      ...options,
      signal: controller.signal,
    });
    if (!response.ok) {
      throw new Error(`${path} returned HTTP ${response.status}`);
    }
    return await response.json();
  } finally {
    clearTimeout(timeout);
  }
}

async function fetchStatus(path, options = {}) {
  if (!("fetch" in globalThis)) {
    throw new Error("Node fetch API is unavailable; use Node 18 or newer");
  }
  const controller = new AbortController();
  const timeout = setTimeout(() => controller.abort(), timeoutMS);
  try {
    const response = await fetch(`${baseURL}${path}`, {
      cache: "no-store",
      ...options,
      signal: controller.signal,
    });
    return response.status;
  } finally {
    clearTimeout(timeout);
  }
}

function browserPostHeaders() {
  return {
    "Content-Type": "application/json",
    "Origin": baseURL,
    "User-Agent": deviceUserAgent,
  };
}

async function assertClientCheckRejectsMissingOrigin() {
  const status = await fetchStatus("/api/client-check", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(clientCheckPayload),
  });
  assert(status === 403, `client-check without Origin returned HTTP ${status}, want 403`);
}

async function assertSettingsRejectsMissingOrigin() {
  const status = await fetchStatus("/api/settings", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(roundTripSettings),
  });
  assert(status === 403, `settings without Origin returned HTTP ${status}, want 403`);
}

function validateHealth(health) {
  assertObject(health, "health");
  assert(health.status === "ok", "health.status must be ok");
}

function validateReadiness(readiness) {
  assertObject(readiness, "readiness");
  assert(readiness.status === "ok", "readiness.status must be ok");
  assert(readiness.metrics === true, "readiness.metrics must be true");
  assertNonEmptyString(readiness.hostname, "readiness.hostname");
  assertTimestamp(readiness.timestamp, "readiness.timestamp", { allowStale: false });
  validateCollectionErrors(readiness.collection_errors, "readiness.collection_errors");
}

function validateStatus(status, expectedSettings = {}) {
  assertObject(status, "status");
  assert(status.status === "ok", "status.status must be ok");
  assert(status.dashboard_build === dashboardBuild, `status.dashboard_build = ${status.dashboard_build}, want ${dashboardBuild}`);
  assertTimestamp(status.started_at, "status.started_at", { allowStale: true });
  assertInteger(status.uptime_seconds, "status.uptime_seconds", { min: 0 });
  assertNonEmptyString(status.os, "status.os");
  assertNonEmptyString(status.arch, "status.arch");
  assert(typeof status.settings_persisted === "boolean", "status.settings_persisted must be boolean");
  assertArrayIncludes(status.refresh_options_ms, [250, 500, 1000, 2000], "status.refresh_options_ms");
  assertArrayIncludes(status.panel_options, ["all", "performance", "storage", "network", "sensors", "gpu"], "status.panel_options");
  validateSettings(status.settings, expectedSettings);
  validateClientCheck(status.client_check);
  validateClientCheck(status.device_client_check);
}

function validateMetrics(metrics) {
  assertObject(metrics, "metrics");
  assertNonEmptyString(metrics.hostname, "metrics.hostname");
  assertNonEmptyString(metrics.os, "metrics.os");
  assertNonEmptyString(metrics.arch, "metrics.arch");
  assertTimestamp(metrics.timestamp, "metrics.timestamp", { allowStale: false });
  assertInteger(metrics.collection_duration_ms, "metrics.collection_duration_ms", { min: 0 });
  validateNumberMetric(metrics.cpu_percent, "metrics.cpu_percent", { unit: "%", allowUnavailable: false, min: 0, max: 100 });
  validateCPUCores(metrics.cpu_cores);
  validateCapacityMetric(metrics.memory, "metrics.memory", { allowUnavailable: false });

  assert(Array.isArray(metrics.disks) && metrics.disks.length > 0, "metrics.disks must include at least one item");
  metrics.disks.forEach((disk, index) => {
    assertObject(disk, `metrics.disks[${index}]`);
    assert(
      nonEmptyString(disk.name) || nonEmptyString(disk.mountpoint),
      `metrics.disks[${index}] must include name or mountpoint`,
    );
    validateCapacityMetric(disk.capacity, `metrics.disks[${index}].capacity`, { allowUnavailable: true });
  });

  validateNetwork(metrics.network);
  validateTemperatures(metrics.temperatures);
  validateGPU(metrics.gpu);
  validateCollectionErrors(metrics.collection_errors);
}

// validateCPUCores checks the optional per-core utilization set. It degrades as
// a whole (available:false on hosts/platforms that cannot read it), so an
// unavailable set is accepted; an available one must carry one busy percent per
// core with a consistent busy count and threshold.
function validateCPUCores(cores) {
  assertObject(cores, "metrics.cpu_cores");
  assert(typeof cores.available === "boolean", "metrics.cpu_cores.available must be boolean");
  if (!cores.available) {
    assertNonEmptyString(cores.error, "metrics.cpu_cores.error");
    return;
  }
  assert(Array.isArray(cores.cores) && cores.cores.length > 0, "metrics.cpu_cores.cores must be a non-empty array when available");
  assertInteger(cores.count, "metrics.cpu_cores.count", { min: 1 });
  assert(cores.count === cores.cores.length, `metrics.cpu_cores.count = ${cores.count}, want ${cores.cores.length}`);
  assert(typeof cores.busy_threshold === "number" && cores.busy_threshold > 0, "metrics.cpu_cores.busy_threshold must be a positive number");
  assertInteger(cores.busy, "metrics.cpu_cores.busy", { min: 0, max: cores.count });
  let busy = 0;
  cores.cores.forEach((percent, index) => {
    assert(typeof percent === "number" && percent >= 0 && percent <= 100, `metrics.cpu_cores.cores[${index}] must be 0-100`);
    if (percent >= cores.busy_threshold) {
      busy += 1;
    }
  });
  assert(busy === cores.busy, `metrics.cpu_cores.busy = ${cores.busy}, want ${busy} (cores at/above ${cores.busy_threshold}%)`);
}

function validateSettings(settings, expected = {}) {
  assertObject(settings, "settings");
  assert(typeof settings.dim === "boolean", "settings.dim must be boolean");
  assert(typeof settings.shift === "boolean", "settings.shift must be boolean");
  assert([250, 500, 1000, 2000].includes(settings.refresh_ms), "settings.refresh_ms must be one of 250, 500, 1000, 2000");
  assert(["all", "performance", "storage", "network", "sensors", "gpu"].includes(settings.panel), "settings.panel is invalid");
  validateThresholds(settings.thresholds, expected.thresholds || {});
  assertTimestamp(settings.updated_at, "settings.updated_at", { allowStale: true });
  for (const [key, value] of Object.entries(expected)) {
    if (key === "thresholds") {
      continue;
    }
    assert(settings[key] === value, `settings.${key} = ${settings[key]}, want ${value}`);
  }
}

function validateThresholds(thresholds, expected = {}) {
  assertObject(thresholds, "settings.thresholds");
  for (const key of ["cpu_warn", "memory_warn", "disk_warn", "gpu_warn", "temp_warn_c"]) {
    assertInteger(thresholds[key], `settings.thresholds.${key}`, { min: 50, max: 90 });
    if (Object.prototype.hasOwnProperty.call(expected, key)) {
      assert(thresholds[key] === expected[key], `settings.thresholds.${key} = ${thresholds[key]}, want ${expected[key]}`);
    }
  }
}

function validateClientCheck(clientCheck, expected = {}) {
  assertObject(clientCheck, "clientCheck");
  assert(typeof clientCheck.seen === "boolean", "clientCheck.seen must be boolean");
  if (!clientCheck.seen) {
    assert(
      Object.keys(expected).length === 0,
      "clientCheck.seen must be true when expected metadata is provided",
    );
    return;
  }
  assertTimestamp(clientCheck.last_seen, "clientCheck.last_seen", { allowStale: true });
  if ("user_agent" in clientCheck) {
    assert(typeof clientCheck.user_agent === "string", "clientCheck.user_agent must be a string");
  }
  if ("dashboard_build" in clientCheck) {
    assert(typeof clientCheck.dashboard_build === "string", "clientCheck.dashboard_build must be a string");
    assert(clientCheck.dashboard_build.length <= 64, "clientCheck.dashboard_build is too long");
  }
  if ("interaction" in clientCheck) {
    assert(typeof clientCheck.interaction === "string", "clientCheck.interaction must be a string");
    assert(clientCheck.interaction.length <= 64, "clientCheck.interaction is too long");
  }
  assertInteger(clientCheck.viewport_width, "clientCheck.viewport_width", { min: 0 });
  assertInteger(clientCheck.viewport_height, "clientCheck.viewport_height", { min: 0 });
  assert(clientCheck.viewport_width <= 10000, "clientCheck.viewport_width out of range");
  assert(clientCheck.viewport_height <= 10000, "clientCheck.viewport_height out of range");
  if ("screen_width" in clientCheck) {
    assertInteger(clientCheck.screen_width, "clientCheck.screen_width", { min: 0 });
    assert(clientCheck.screen_width <= 10000, "clientCheck.screen_width out of range");
  }
  if ("screen_height" in clientCheck) {
    assertInteger(clientCheck.screen_height, "clientCheck.screen_height", { min: 0 });
    assert(clientCheck.screen_height <= 10000, "clientCheck.screen_height out of range");
  }
  assertFiniteNumber(clientCheck.device_pixel_ratio, "clientCheck.device_pixel_ratio");
  assert(clientCheck.device_pixel_ratio >= 0 && clientCheck.device_pixel_ratio <= 16, "clientCheck.device_pixel_ratio out of range");
  if ("touch_points" in clientCheck) {
    assertInteger(clientCheck.touch_points, "clientCheck.touch_points", { min: 0 });
    assert(clientCheck.touch_points <= 20, "clientCheck.touch_points out of range");
  }
  if ("display_mode" in clientCheck) {
    assert(typeof clientCheck.display_mode === "string", "clientCheck.display_mode must be a string");
    assert(clientCheck.display_mode.length <= 32, "clientCheck.display_mode is too long");
  }
  assert(typeof clientCheck.standalone === "boolean", "clientCheck.standalone must be boolean");
  if ("visibility" in clientCheck) {
    assert(typeof clientCheck.visibility === "string", "clientCheck.visibility must be a string");
  }
  if ("orientation" in clientCheck) {
    assert(typeof clientCheck.orientation === "string", "clientCheck.orientation must be a string");
  }
  for (const [key, value] of Object.entries(expected)) {
    assert(clientCheck[key] === value, `clientCheck.${key} = ${clientCheck[key]}, want ${value}`);
  }
}

function validateDeviceClientCheck(clientCheck, expected = {}) {
  validateClientCheck(clientCheck, expected);
  assert(clientCheck.seen === true, "device clientCheck must be seen");
  assert(clientCheck.viewport_width > 0, "device clientCheck.viewport_width must be positive");
  assert(clientCheck.viewport_height > 0, "device clientCheck.viewport_height must be positive");
  assert(typeof clientCheck.user_agent === "string", "device clientCheck.user_agent must be a string");
  assert(
    clientCheck.user_agent.includes("Mobile") ||
      clientCheck.user_agent.includes("Android") ||
      clientCheck.user_agent.includes("iPhone") ||
      clientCheck.user_agent.includes("iPad") ||
      clientCheck.user_agent.includes("iPod"),
    "device clientCheck.user_agent must identify a mobile device",
  );
}

function validateClientCheckHistory(history, expectedLatest = {}) {
  assertObject(history, "clientCheckHistory");
  assert(Array.isArray(history.checks), "clientCheckHistory.checks must be an array");
  assert(history.checks.length <= 16, "clientCheckHistory.checks has too many entries");
  let previousLastSeen = Number.POSITIVE_INFINITY;
  history.checks.forEach((check, index) => {
    validateClientCheck(check, index === 0 ? expectedLatest : {});
    assert(check.seen, `clientCheckHistory.checks[${index}].seen must be true`);
    const lastSeen = Date.parse(check.last_seen);
    assert(lastSeen <= previousLastSeen, "clientCheckHistory.checks must be newest first");
    previousLastSeen = lastSeen;
  });
}

function validateNetwork(network) {
  assertObject(network, "metrics.network");
  if (!network.available) {
    assertNonEmptyString(network.error, "metrics.network.error");
    return;
  }
  assert(Array.isArray(network.interfaces) && network.interfaces.length > 0, "metrics.network.interfaces must be non-empty");
  network.interfaces.forEach((iface, index) => {
    assertObject(iface, `metrics.network.interfaces[${index}]`);
    assertNonEmptyString(iface.name, `metrics.network.interfaces[${index}].name`);
    validateNumberMetric(iface.rx_bytes_per_second, `metrics.network.interfaces[${index}].rx_bytes_per_second`, {
      unit: "B/s",
      allowUnavailable: true,
      min: 0,
    });
    validateNumberMetric(iface.tx_bytes_per_second, `metrics.network.interfaces[${index}].tx_bytes_per_second`, {
      unit: "B/s",
      allowUnavailable: true,
      min: 0,
    });
  });
  validateNetworkUplink(network.uplink);
}

// validateNetworkUplink checks the optional active-network identity. It degrades
// (available:false) when there is no default route or the SSID can't be read, so
// an unavailable uplink is accepted; an available one must name a kind + label.
function validateNetworkUplink(uplink) {
  if (uplink === undefined) {
    return;
  }
  assertObject(uplink, "metrics.network.uplink");
  assert(typeof uplink.available === "boolean", "metrics.network.uplink.available must be boolean");
  if (!uplink.available) {
    assertNonEmptyString(uplink.error, "metrics.network.uplink.error");
    return;
  }
  assert(["wifi", "ethernet"].includes(uplink.kind), `metrics.network.uplink.kind = ${uplink.kind}, want wifi or ethernet`);
  assertNonEmptyString(uplink.name, "metrics.network.uplink.name");
}

function validateTemperatures(temperatures) {
  assertObject(temperatures, "metrics.temperatures");
  if (!temperatures.available) {
    assertNonEmptyString(temperatures.error, "metrics.temperatures.error");
    return;
  }
  assert(Array.isArray(temperatures.sensors) && temperatures.sensors.length > 0, "metrics.temperatures.sensors must be non-empty");
  temperatures.sensors.forEach((sensor, index) => {
    assertObject(sensor, `metrics.temperatures.sensors[${index}]`);
    assertNonEmptyString(sensor.name, `metrics.temperatures.sensors[${index}].name`);
    validateNumberMetric(sensor.celsius, `metrics.temperatures.sensors[${index}].celsius`, {
      unit: "C",
      allowUnavailable: true,
      min: -50,
      max: 150,
    });
  });
}

function validateGPU(gpu) {
  assertObject(gpu, "metrics.gpu");
  if (!gpu.available) {
    assertNonEmptyString(gpu.error, "metrics.gpu.error");
    return;
  }
  assert(Array.isArray(gpu.devices) && gpu.devices.length > 0, "metrics.gpu.devices must be non-empty");
  gpu.devices.forEach((device, index) => {
    assertObject(device, `metrics.gpu.devices[${index}]`);
    assertNonEmptyString(device.name, `metrics.gpu.devices[${index}].name`);
    validateNumberMetric(device.usage_percent, `metrics.gpu.devices[${index}].usage_percent`, {
      unit: "%",
      allowUnavailable: true,
      min: 0,
      max: 100,
    });
    validateCapacityMetric(device.memory, `metrics.gpu.devices[${index}].memory`, { allowUnavailable: true });
    validateNumberMetric(device.temperature_celsius, `metrics.gpu.devices[${index}].temperature_celsius`, {
      unit: "C",
      allowUnavailable: true,
      min: -50,
      max: 150,
    });
  });
}

function validateCollectionErrors(errors, name = "metrics.collection_errors") {
  if (errors === undefined) {
    return;
  }
  assert(Array.isArray(errors), `${name} must be an array`);
  errors.forEach((message, index) => {
    assertNonEmptyString(message, `${name}[${index}]`);
  });
}

function validateNumberMetric(metric, name, { unit, allowUnavailable, min = Number.NEGATIVE_INFINITY, max = Number.POSITIVE_INFINITY }) {
  assertObject(metric, name);
  assert(metric.unit === unit, `${name}.unit must be ${unit}`);
  assert(typeof metric.available === "boolean", `${name}.available must be boolean`);
  if (!metric.available) {
    assert(allowUnavailable, `${name} is unavailable: ${metric.error || ""}`);
    assertNonEmptyString(metric.error, `${name}.error`);
    return;
  }
  assertFiniteNumber(metric.value, `${name}.value`);
  assert(metric.value >= min && metric.value <= max, `${name}.value out of range: ${metric.value}`);
}

function validateCapacityMetric(metric, name, { allowUnavailable }) {
  assertObject(metric, name);
  assert(typeof metric.available === "boolean", `${name}.available must be boolean`);
  if (!metric.available) {
    assert(allowUnavailable, `${name} is unavailable: ${metric.error || ""}`);
    assertNonEmptyString(metric.error, `${name}.error`);
    return;
  }
  assertInteger(metric.used_bytes, `${name}.used_bytes`, { min: 0 });
  assertInteger(metric.total_bytes, `${name}.total_bytes`, { min: 1 });
  assert(metric.used_bytes <= metric.total_bytes, `${name}.used_bytes exceeds total_bytes`);
  assertFiniteNumber(metric.percent, `${name}.percent`);
  assert(metric.percent >= 0 && metric.percent <= 100, `${name}.percent out of range: ${metric.percent}`);
}

function assertTimestamp(value, name, { allowStale }) {
  assertNonEmptyString(value, name);
  const timestamp = Date.parse(value);
  assert(Number.isFinite(timestamp), `${name} must be an ISO timestamp`);
  const ageMS = Date.now() - timestamp;
  assert(ageMS > -5000, `${name} is too far in the future`);
  if (!allowStale) {
    assert(ageMS < 60000, `${name} is stale`);
  }
}

function assertArrayIncludes(values, required, name) {
  assert(Array.isArray(values), `${name} must be an array`);
  for (const item of required) {
    assert(values.includes(item), `${name} must include ${item}`);
  }
}

function assertObject(value, name) {
  assert(value && typeof value === "object" && !Array.isArray(value), `${name} must be an object`);
}

function assertNonEmptyString(value, name) {
  assert(nonEmptyString(value), `${name} must be a non-empty string`);
}

function nonEmptyString(value) {
  return typeof value === "string" && value.trim() !== "";
}

function assertInteger(value, name, { min, max }) {
  assert(Number.isInteger(value), `${name} must be an integer`);
  assert(value >= min, `${name} must be >= ${min}`);
  if (max !== undefined) {
    assert(value <= max, `${name} must be <= ${max}`);
  }
}

function assertFiniteNumber(value, name) {
  assert(typeof value === "number" && Number.isFinite(value), `${name} must be a finite number`);
}

function assert(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}

function sampleStatus(settings = sampleSettings()) {
  return {
    status: "ok",
    dashboard_build: dashboardBuild,
    started_at: new Date(Date.now() - 3600_000).toISOString(),
    uptime_seconds: 3600,
    os: "linux",
    arch: "amd64",
    settings_persisted: true,
    refresh_options_ms: [250, 500, 1000, 2000],
    panel_options: ["all", "gpu", "network", "performance", "sensors", "storage"],
    settings,
    client_check: sampleClientCheck(false),
    device_client_check: sampleClientCheck(false),
  };
}

function sampleReadiness() {
  return {
    status: "ok",
    metrics: true,
    hostname: "labbox",
    timestamp: new Date().toISOString(),
    collection_errors: [
      "temperatures: no supported temperature sensors found",
      "gpu: nvidia-smi not found",
    ],
  };
}

function sampleMetrics() {
  return {
    hostname: "labbox",
    os: "linux",
    arch: "amd64",
    platform: "6.8.0-homelab",
    timestamp: new Date().toISOString(),
    collection_duration_ms: 142,
    cpu_name: "AMD Ryzen 9 7950X",
    memory_name: "DDR5 · 6000 MT/s",
    cpu_percent: { available: true, value: 37, unit: "%" },
    cpu_power: { available: false, unit: "W", error: "no CPU package power counters found" },
    psu_output_power: { available: false, unit: "W", error: "no PSU output power sensor exposed on Linux" },
    cpu_clock: { available: false, unit: "MHz", error: "CPU clock frequency not exposed" },
    cpu_clock_max: { available: false, unit: "MHz", error: "CPU max clock frequency not exposed" },
    cpu_clock_base: { available: false, unit: "MHz", error: "CPU base clock frequency not exposed" },
    cpu_temperature: { available: false, unit: "C", error: "no CPU temperature sensor reported" },
    cpu_cores: { available: true, cores: [95, 12, 88, 4], count: 4, busy: 2, busy_threshold: 80 },
    memory: { available: true, used_bytes: 8_589_934_592, total_bytes: 17_179_869_184, percent: 50 },
    disks: [{
      name: "root",
      mountpoint: "/",
      fs_type: "ext4",
      capacity: { available: true, used_bytes: 50_000_000_000, total_bytes: 100_000_000_000, percent: 50 },
    }],
    network: {
      available: true,
      uplink: { available: true, kind: "wifi", name: "BiBi-Pro-Max" },
      interfaces: [{
        name: "tailscale0",
        rx_bytes_total: 1000,
        tx_bytes_total: 500,
        rx_bytes_per_second: { available: true, value: 2048, unit: "B/s" },
        tx_bytes_per_second: { available: true, value: 1024, unit: "B/s" },
      }],
    },
    temperatures: {
      available: false,
      sensors: [],
      error: "no supported temperature sensors found",
    },
    gpu: {
      available: true,
      devices: [{
        name: "Intel GPU card1",
        usage_percent: { available: false, unit: "%", error: "DRM GPU usage not exposed" },
        power_watts: { available: false, unit: "W", error: "DRM GPU power not exposed" },
        memory: { available: false, error: "DRM VRAM usage not exposed" },
        temperature_celsius: { available: true, value: 49, unit: "C" },
      }],
      error: "nvidia-smi not found",
    },
    collection_errors: [
      "cpu_power: no CPU package power counters found",
      "psu_output_power: no PSU output power sensor exposed on Linux",
      "cpu_clock: CPU clock frequency not exposed",
      "cpu_temperature: no CPU temperature sensor reported",
      "temperatures: no supported temperature sensors found",
      "gpu: nvidia-smi not found",
      "gpu Intel GPU card1 usage: DRM GPU usage not exposed",
      "gpu Intel GPU card1 power: DRM GPU power not exposed",
      "gpu Intel GPU card1 memory: DRM VRAM usage not exposed",
    ],
  };
}

function sampleSettings() {
  return {
    dim: false,
    shift: true,
    refresh_ms: 1000,
    panel: "gpu",
    thresholds: {
      cpu_warn: 70,
      memory_warn: 70,
      disk_warn: 70,
      gpu_warn: 70,
      temp_warn_c: 70,
    },
    updated_at: new Date().toISOString(),
  };
}

function roundTripSampleSettings() {
  return {
    ...roundTripSettings,
    updated_at: new Date().toISOString(),
  };
}

function sampleClientCheck(seen) {
  if (!seen) {
    return { seen: false, standalone: false };
  }
  return {
    seen: true,
    last_seen: new Date().toISOString(),
    user_agent: deviceUserAgent,
    ...clientCheckPayload,
  };
}

function sampleClientCheckHistory(seen) {
  if (!seen) {
    return { checks: [] };
  }
  return { checks: [sampleClientCheck(true)] };
}
