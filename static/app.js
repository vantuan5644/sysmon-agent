const state = {
  interval: 1000,
  timer: null,
  statusTimer: null,
  staleTimer: null,
  clientCheckTimer: null,
  clientCheckDebounceTimer: null,
  clientCheckInFlight: false,
  pendingClientCheckOptions: null,
  pendingClientCheckPromise: null,
  pendingClientCheckResolve: null,
  transientStatusTimer: null,
  paused: false,
  stream: null,
  streamFallback: false,
  streamFailures: 0,
  metricsInFlight: false,
  settingsInFlight: false,
  settingsRequestSeq: 0,
  issuesExpanded: false,
  issueMessages: [],
  clientIssueMessages: [],
  displayIssueMessages: [],
  metricIssueMessages: [],
  settingsIssueMessages: [],
  statusIssueMessages: [],
  alertsExpanded: false,
  alertMessages: [],
  staleDashboardBuild: "",
  staticRefreshInFlight: false,
  lastMetricsAtMS: 0,
  lastMetricTimestampMS: 0,
  lastCollectionDurationMS: null,
  connectionKind: "loading",
  wakeLock: null,
  wakeWanted: false,
  history: {
    cpu: [],
    mem: [],
    gpu: [],
    net: [],
  },
  settings: {
    dim: false,
    shift: true,
    panel: "all",
    thresholds: {
      cpu_warn: 70,
      memory_warn: 70,
      disk_warn: 70,
      gpu_warn: 70,
      temp_warn_c: 70,
    },
  },
};

const refreshOptionsMS = [250, 500, 1000, 2000];
const panelOptions = ["all", "performance", "storage", "network", "sensors", "gpu"];
const dashboardBuild = "sysmon-static-v102";
const netRingReferenceBytesPerSecond = 125000000;
const netRingWarnPercent = 90;
const defaultThresholds = {
  cpu_warn: 70,
  memory_warn: 70,
  disk_warn: 70,
  gpu_warn: 70,
  temp_warn_c: 70,
};
const thresholdTargets = [
  { key: "cpu_warn", label: "CPU", unit: "%", min: 50, max: 90, step: 5 },
  { key: "memory_warn", label: "RAM", unit: "%", min: 50, max: 90, step: 5 },
  { key: "disk_warn", label: "Disk", unit: "%", min: 50, max: 90, step: 5 },
  { key: "gpu_warn", label: "GPU", unit: "%", min: 50, max: 90, step: 5 },
  { key: "temp_warn_c", label: "Temp", unit: "C", min: 50, max: 90, step: 5 },
];
// Host-control toolbar. Each action maps to a fixed button id (looked up via
// getElementById, never querySelectorAll) so the verifier's DOM mock can wire
// them. Refresh interval + warn thresholds are deliberately NOT here -- they are
// host-side config (CLI flags / env), not touch controls.
const controlButtonIDs = {
  mic_mute: "micCtl",
  media_toggle: "mediaCtl",
  volume_mute: "volumeCtl",
  lock_screen: "lockCtl",
};
const controlActionLabels = {
  mic_mute: "Microphones",
  media_toggle: "Media",
  volume_mute: "Speaker",
  lock_screen: "Screen",
};
const metricsTimeoutMS = 4500;
const auxiliaryTimeoutMS = 3000;
// Number of consecutive EventSource failures (with no recovery in between)
// before we abandon the live /api/stream and fall back to polling /api/metrics
// for the rest of the session. A successful open/message resets the counter, so
// transient blips that recover never demote us; persistent failure does.
const streamFailureLimit = 3;
const clientCheckIntervalMS = 30000;
const clientCheckStaleAfterMS = clientCheckIntervalMS * 3;
const clientCheckDebounceMS = 500;
const collapsedIssueLimit = 5;
const sparklineSampleLimit = 24;
const wakePreferenceKey = "sysmon:wake-wanted";

const $ = (id) => document.getElementById(id);

document.addEventListener("DOMContentLoaded", () => {
  $("pauseBtn").addEventListener("click", togglePause);
  $("wakeBtn").addEventListener("click", toggleWakeLock);
  $("dimBtn").addEventListener("click", () => updateSettings({ dim: !state.settings.dim }, "settings_dim"));
  $("shiftBtn").addEventListener("click", () => updateSettings({ shift: !state.settings.shift }, "settings_shift"));
  for (const [action, id] of Object.entries(controlButtonIDs)) {
    const button = $(id);
    button.addEventListener("click", () => sendControl(action, button));
  }
  $("alertsPanel").addEventListener("click", toggleAlertsPanel);
  $("alertsPanel").addEventListener("keydown", handleAlertsPanelKeydown);
  $("issuesPanel").addEventListener("click", toggleIssuesPanel);
  $("issuesPanel").addEventListener("keydown", handleIssuesPanelKeydown);
  $("statusStrip").addEventListener("click", refreshNow);
  $("statusStrip").addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      refreshNow();
    }
  });
  setupPager();

  document.addEventListener("visibilitychange", handleVisibilityChange);
  window.addEventListener("online", refreshVisibleDashboard);
  window.addEventListener("offline", markDashboardOffline);
  window.addEventListener("pagehide", handlePageHide);
  window.addEventListener("pageshow", (event) => {
    if (event?.persisted) {
      syncVisibleTimers();
      refreshVisibleDashboard();
    }
  });
  window.addEventListener("resize", scheduleClientCheck);
  window.addEventListener("orientationchange", scheduleClientCheck);

  registerServiceWorker();

  restoreWakePreference();
  fetchSettings();
  fetchStatus();
  syncVisibleTimers();
  fetchMetrics();
  sendClientCheck();
});

// setupPager wires the iOS StandBy-style horizontal pager: native CSS scroll-snap
// does the swiping, this only keeps the page dots in sync and lets a dot tap jump
// to its page. Every scroll API is feature-guarded so the headless verifier (whose
// mock DOM has no scroll geometry) is a no-op rather than a crash.
function setupPager() {
  const pager = $("pager");
  const dots = [$("pageDot0"), $("pageDot1")];
  if (!pager || dots.some((dot) => !dot)) {
    return;
  }
  const pageCount = dots.length;
  let activePage = 0;

  const setActive = (index) => {
    const clamped = Math.max(0, Math.min(pageCount - 1, index));
    if (clamped === activePage) {
      return;
    }
    activePage = clamped;
    dots.forEach((dot, i) => {
      const on = i === clamped;
      dot.classList.toggle("active", on);
      dot.setAttribute("aria-current", on ? "true" : "false");
    });
  };

  pager.addEventListener("scroll", () => {
    const width = pager.clientWidth || 0;
    if (width > 0) {
      setActive(Math.round((pager.scrollLeft || 0) / width));
    }
  });

  dots.forEach((dot, index) => {
    dot.addEventListener("click", () => {
      const width = pager.clientWidth || 0;
      if (typeof pager.scrollTo === "function") {
        pager.scrollTo({ left: width * index, behavior: "smooth" });
      } else {
        pager.scrollLeft = width * index;
      }
      setActive(index);
    });
  });
}

function schedulePolling() {
  clearDashboardInterval("timer");
  if (state.paused || document.visibilityState !== "visible") {
    closeStream();
    return;
  }
  // Prefer the server-pushed stream: it serves warm sampler snapshots at the
  // host's fast-lane rate, so the dashboard renders live without a poll per
  // tick. Only when streaming is unsupported or has failed out do we set up the
  // periodic /api/metrics fetch.
  if (canUseStream()) {
    openStream();
    return;
  }
  closeStream();
  state.timer = setInterval(() => {
    if (shouldPollMetrics()) {
      fetchMetrics();
    }
  }, state.interval);
}

function shouldPollMetrics() {
  return !state.paused && document.visibilityState === "visible";
}

function canUseStream() {
  return !state.streamFallback && typeof window.EventSource === "function";
}

function openStream() {
  if (state.stream) {
    return;
  }
  let source;
  try {
    source = new EventSource("/api/stream");
  } catch {
    state.streamFallback = true;
    return;
  }
  state.stream = source;
  source.onopen = () => {
    state.streamFailures = 0;
  };
  source.onmessage = handleStreamMessage;
  source.onerror = handleStreamError;
}

function closeStream() {
  if (!state.stream) {
    return;
  }
  try {
    state.stream.close();
  } catch {
  }
  state.stream = null;
}

function handleStreamMessage(event) {
  if (state.paused || document.visibilityState !== "visible") {
    return;
  }
  let metrics;
  try {
    metrics = JSON.parse(event.data);
  } catch {
    return;
  }
  state.streamFailures = 0;
  state.lastMetricsAtMS = nowMS();
  render(metrics);
  setConnectionState("ok", "Live");
}

function handleStreamError(event) {
  // A CLOSED readyState means the browser will not auto-reconnect (server sent a
  // non-2xx, wrong content-type, or the route is absent) -- give up on the
  // stream immediately. A CONNECTING state is a transient blip the browser is
  // already retrying, so we only demote after streamFailureLimit such failures
  // with no successful open/message resetting the counter in between.
  const closed = event?.target?.readyState === 2 ||
    (state.stream && state.stream.readyState === 2);
  state.streamFailures += 1;
  if (!closed && state.streamFailures < streamFailureLimit) {
    if (state.connectionKind === "ok") {
      setConnectionState("warn", "Reconnecting");
    }
    return;
  }
  state.streamFallback = true;
  closeStream();
  if (!state.paused && document.visibilityState === "visible") {
    schedulePolling();
    fetchMetrics();
  }
}

function shouldPollStatus() {
  return document.visibilityState === "visible";
}

function scheduleStatusPolling() {
  clearDashboardInterval("statusTimer");
  if (document.visibilityState !== "visible") {
    return;
  }
  state.statusTimer = setInterval(() => {
    if (shouldPollStatus()) {
      fetchStatus();
    }
  }, 60000);
}

function scheduleStalePolling() {
  clearDashboardInterval("staleTimer");
  if (document.visibilityState !== "visible") {
    return;
  }
  state.staleTimer = setInterval(markStaleIfNeeded, 5000);
}

function syncVisibleTimers() {
  schedulePolling();
  scheduleStatusPolling();
  scheduleStalePolling();
  scheduleClientCheckPolling();
}

function stopVisibleTimers() {
  clearDashboardInterval("timer");
  clearDashboardInterval("statusTimer");
  clearDashboardInterval("staleTimer");
  clearDashboardInterval("clientCheckTimer");
  clearDashboardTimeout("clientCheckDebounceTimer");
  closeStream();
}

function clearDashboardInterval(name) {
  if (!state[name]) {
    return;
  }
  clearInterval(state[name]);
  state[name] = null;
}

function clearDashboardTimeout(name) {
  if (!state[name]) {
    return;
  }
  clearTimeout(state[name]);
  state[name] = null;
}

function registerServiceWorker() {
  if (!("serviceWorker" in navigator) || typeof navigator.serviceWorker.register !== "function") {
    return;
  }
  let reloading = false;
  if (typeof navigator.serviceWorker.addEventListener === "function") {
    navigator.serviceWorker.addEventListener("controllerchange", () => {
      if (reloading) {
        return;
      }
      reloading = true;
      window.location.reload();
    });
  }
  navigator.serviceWorker.register("/sw.js").catch(() => {});
}

function scheduleClientCheckPolling() {
  clearDashboardInterval("clientCheckTimer");
  if (document.visibilityState !== "visible") {
    return;
  }
  state.clientCheckTimer = setInterval(() => {
    if (shouldSendClientCheck()) {
      sendClientCheck();
    }
  }, clientCheckIntervalMS);
}

function scheduleClientCheck() {
  if (state.clientCheckDebounceTimer) {
    clearTimeout(state.clientCheckDebounceTimer);
  }
  state.clientCheckDebounceTimer = setTimeout(() => {
    state.clientCheckDebounceTimer = null;
    if (shouldSendClientCheck()) {
      sendClientCheck();
    }
  }, clientCheckDebounceMS);
}

function shouldSendClientCheck() {
  return document.visibilityState === "visible";
}

function refreshVisibleDashboard() {
  if (document.visibilityState !== "visible") {
    return;
  }
  if (state.wakeWanted && !state.wakeLock) {
    requestWakeLock();
  }
  sendClientCheck();
  markStaleIfNeeded();
  fetchStatus();
  if (!state.paused) {
    fetchMetrics();
  }
}

function handleVisibilityChange() {
  if (document.visibilityState === "hidden") {
    stopVisibleTimers();
    sendClientCheckBeacon();
    return;
  }
  syncVisibleTimers();
  refreshVisibleDashboard();
}

function handlePageHide() {
  stopVisibleTimers();
  sendClientCheckBeacon();
}

function markDashboardOffline() {
  if (state.paused) {
    return;
  }
  setConnectionState("bad", "Offline");
}

async function fetchMetrics() {
  if (state.metricsInFlight) {
    return;
  }
  state.metricsInFlight = true;
  if (state.lastMetricsAtMS === 0 || state.connectionKind === "bad") {
    setConnectionState("loading", "Updating");
  }
  try {
    const response = await fetchWithTimeout("/api/metrics", { cache: "no-store" }, metricsTimeoutMS);
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const metrics = await response.json();
    state.lastMetricsAtMS = nowMS();
    render(metrics);
    if (state.paused) {
      setConnectionState("paused", "Paused");
    } else {
      setConnectionState("ok", "Live");
    }
  } catch (error) {
    setConnectionState("bad", error.message || "Offline");
    renderMetricError(error);
  } finally {
    state.metricsInFlight = false;
  }
}

async function refreshNow() {
  if (state.staleDashboardBuild) {
    showTransientStatus(`Refreshing app ${state.staleDashboardBuild}`);
    await refreshStaticAssets();
    return;
  }
  showTransientStatus("Refreshing");
  const clientCheck = sendClientCheck({ interaction: "status_strip_tap" });
  fetchStatus();
  const metrics = fetchMetrics();
  Promise.allSettled([clientCheck, metrics]).then(([clientCheckResult]) => {
    if (clientCheckResult.status === "fulfilled" && clientCheckResult.value) {
      showTransientStatus(`Client check sent (${acceptedClientCheckModeLabel(clientCheckResult.value)})`);
    }
  });
}

function markStaleIfNeeded() {
  refreshMetricAge();
  if (state.paused || state.metricsInFlight || state.lastMetricsAtMS === 0 || state.connectionKind === "bad") {
    return;
  }
  const staleAfterMS = Math.max(10000, state.interval * 4);
  if (nowMS() - state.lastMetricsAtMS > staleAfterMS) {
    setConnectionState("warn", "Stale");
  }
}

async function fetchWithTimeout(path, options, timeoutMS) {
  let controller = null;
  const requestOptions = { ...options };
  if (typeof window.AbortController === "function") {
    controller = new AbortController();
    if ("signal" in controller) {
      requestOptions.signal = controller.signal;
    }
  }

  let timeout;
  try {
    return await Promise.race([
      fetch(path, requestOptions),
      new Promise((_, reject) => {
        timeout = setTimeout(() => {
          if (controller && typeof controller.abort === "function") {
            controller.abort();
          }
          reject(new Error("Request timed out"));
        }, timeoutMS);
      }),
    ]);
  } finally {
    clearTimeout(timeout);
  }
}

function sendClientCheck(options = {}) {
  const clientCheckOptions = normalizeClientCheckOptions(options);
  if (state.clientCheckInFlight) {
    if (clientCheckOptions.interaction) {
      return queueClientCheck(clientCheckOptions);
    }
    return state.pendingClientCheckPromise || Promise.resolve(false);
  }
  return runClientCheckRequest(clientCheckOptions);
}

function normalizeClientCheckOptions(options = {}) {
  const interaction = String(options.interaction || "").trim();
  return interaction ? { interaction } : {};
}

function queueClientCheck(options) {
  state.pendingClientCheckOptions = mergeClientCheckOptions(state.pendingClientCheckOptions, options);
  if (!state.pendingClientCheckPromise) {
    state.pendingClientCheckPromise = new Promise((resolve) => {
      state.pendingClientCheckResolve = resolve;
    });
  }
  return state.pendingClientCheckPromise;
}

function mergeClientCheckOptions(current = null, next = {}) {
  const interaction = preferredClientCheckInteraction(current?.interaction, next?.interaction);
  return interaction ? { interaction } : {};
}

function preferredClientCheckInteraction(current, next) {
  current = String(current || "").trim();
  next = String(next || "").trim();
  if (!next) {
    return current;
  }
  if (!current || next === "status_strip_tap") {
    return next;
  }
  if (current === "status_strip_tap") {
    return current;
  }
  return next;
}

function clearPendingClientCheck() {
  const pending = {
    options: state.pendingClientCheckOptions,
    resolve: state.pendingClientCheckResolve,
  };
  state.pendingClientCheckOptions = null;
  state.pendingClientCheckPromise = null;
  state.pendingClientCheckResolve = null;
  return pending;
}

function flushPendingClientCheck() {
  const pending = clearPendingClientCheck();
  if (!pending.options || !shouldSendClientCheck()) {
    if (pending.resolve) {
      pending.resolve(false);
    }
    return;
  }
  runClientCheckRequest(pending.options).then((result) => {
    if (pending.resolve) {
      pending.resolve(result);
    }
  });
}

function runClientCheckRequest(options = {}) {
  state.clientCheckInFlight = true;
  const body = JSON.stringify(clientCheckPayload(options));
  return fetchWithTimeout("/api/client-check", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body,
    cache: "no-store",
    keepalive: true,
  }, auxiliaryTimeoutMS)
    .then(async (response) => {
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      let accepted = null;
      try {
        accepted = await response.json();
      } catch {
        accepted = {};
      }
      clearClientCheckIssue();
      return accepted || {};
    })
    .catch((error) => {
      renderClientCheckError(error);
      return false;
    })
    .finally(() => {
      state.clientCheckInFlight = false;
      flushPendingClientCheck();
    });
}

function acceptedClientCheckModeLabel(clientCheck) {
  const displayMode = String(clientCheck?.display_mode || "").trim();
  if (displayMode) {
    return clientModeLabel(displayMode);
  }
  if (clientCheck?.standalone === true) {
    return "app";
  }
  return clientModeLabel(currentDisplayMode());
}

function sendClientCheckBeacon() {
  if (typeof navigator.sendBeacon !== "function" || typeof Blob !== "function") {
    return false;
  }
  const blob = new Blob([JSON.stringify(clientCheckPayload())], { type: "application/json" });
  return navigator.sendBeacon("/api/client-check", blob);
}

function clientCheckPayload(options = {}) {
  const displayMode = currentDisplayMode();
  const payload = {
    dashboard_build: dashboardBuild,
    viewport_width: positiveInteger(window.innerWidth),
    viewport_height: positiveInteger(window.innerHeight),
    screen_width: positiveInteger(window.screen?.width),
    screen_height: positiveInteger(window.screen?.height),
    device_pixel_ratio: positiveNumber(window.devicePixelRatio, 1),
    touch_points: positiveInteger(navigator.maxTouchPoints),
    display_mode: displayMode,
    standalone: displayMode === "standalone",
    visibility: String(document.visibilityState || ""),
    orientation: currentOrientation(),
  };
  const interaction = String(options.interaction || "").trim();
  if (interaction) {
    payload.interaction = interaction;
  }
  return payload;
}

function currentDisplayMode() {
  if (navigator.standalone) {
    return "standalone";
  }
  if (typeof window.matchMedia !== "function") {
    return "unknown";
  }
  for (const mode of ["standalone", "fullscreen", "minimal-ui", "browser"]) {
    if (window.matchMedia(`(display-mode: ${mode})`).matches) {
      return mode;
    }
  }
  return "unknown";
}

function currentOrientation() {
  const screenOrientation = window.screen?.orientation;
  if (screenOrientation?.type) {
    return String(screenOrientation.type);
  }
  if (typeof window.orientation === "number") {
    return `angle-${window.orientation}`;
  }
  if (window.innerWidth > window.innerHeight) {
    return "landscape";
  }
  if (window.innerHeight > window.innerWidth) {
    return "portrait";
  }
  return "unknown";
}

function positiveInteger(value) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric) || numeric <= 0) {
    return 0;
  }
  return Math.round(numeric);
}

function positiveNumber(value, fallback) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric) || numeric <= 0) {
    return fallback;
  }
  return numeric;
}

async function fetchStatus() {
  try {
    const response = await fetchWithTimeout("/api/status", { cache: "no-store" }, auxiliaryTimeoutMS);
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    renderStatus(await response.json());
  } catch (error) {
    $("agentMeta").textContent = "status NA";
    renderStatusError(error);
  }
}

function renderStatus(status) {
  if (status?.settings && !state.settingsInFlight) {
    applySettings(status.settings);
  }
  applyControlCapabilities(status?.controls);
  renderStatusIssues(status);
  renderDisplayModeIssues();
  const uptime = formatDuration(status?.uptime_seconds);
  const persistence = status?.settings_persisted ? "saved" : "memory";
  $("agentMeta").textContent = `up ${uptime} / ${persistence} / ${clientModeLabel(currentDisplayMode())}${clientCheckAgeLabel(statusProofClientCheck(status))}`;
}

function renderStatusIssues(status) {
  const serverBuild = String(status?.dashboard_build || "").trim();
  const messages = [];
  state.staleDashboardBuild = serverBuild && serverBuild !== dashboardBuild ? serverBuild : "";
  if (serverBuild && serverBuild !== dashboardBuild) {
    messages.push(`dashboard build stale: app ${dashboardBuild}, server ${serverBuild}; tap status strip to refresh app or re-add Home Screen app`);
  }
  const clientBuild = String(status?.client_check?.dashboard_build || "").trim();
  if (status?.client_check?.seen && clientBuild && clientBuild !== dashboardBuild) {
    messages.push(`latest client check stale: client ${clientBuild}, app ${dashboardBuild}; reload or re-add Home Screen app`);
  }
  const deviceBuild = String(status?.device_client_check?.dashboard_build || "").trim();
  if (status?.device_client_check?.seen && deviceBuild && deviceBuild !== dashboardBuild && deviceBuild !== clientBuild) {
    messages.push(`latest device check stale: client ${deviceBuild}, app ${dashboardBuild}; reload or re-add Home Screen app`);
  }
  const proofClientCheck = statusProofClientCheck(status);
  const proofLabel = proofClientCheck === status?.device_client_check ? "device" : "client";
  const clientCheckLastSeenMS = clientCheckLastSeenTimeMS(proofClientCheck);
  if (clientCheckLastSeenMS !== null && nowMS() - clientCheckLastSeenMS > clientCheckStaleAfterMS) {
    messages.push(`latest ${proofLabel} check stale: seen ${formatSampleAge(clientCheckLastSeenMS)} ago; tap status strip to refresh client check`);
  }
  state.statusIssueMessages = messages;
  renderIssuesPanel();
}

async function refreshStaticAssets() {
  if (state.staticRefreshInFlight) {
    return;
  }
  state.staticRefreshInFlight = true;
  stopVisibleTimers();
  try {
    await unregisterSysmonServiceWorkers();
    await deleteSysmonStaticCaches();
  } finally {
    window.location.reload();
  }
}

async function unregisterSysmonServiceWorkers() {
  const serviceWorker = navigator.serviceWorker;
  if (!serviceWorker || typeof serviceWorker.getRegistrations !== "function") {
    return;
  }
  const registrations = await serviceWorker.getRegistrations();
  await Promise.all(registrations.map((registration) => (
    registration && typeof registration.unregister === "function"
      ? registration.unregister()
      : false
  )));
}

async function deleteSysmonStaticCaches() {
  if (!window.caches || typeof window.caches.keys !== "function" || typeof window.caches.delete !== "function") {
    return;
  }
  const keys = await window.caches.keys();
  await Promise.all(keys
    .filter((key) => String(key).startsWith("sysmon-static-"))
    .map((key) => window.caches.delete(key)));
}

function renderStatusError(error) {
  const message = String(error?.message || "").trim();
  state.statusIssueMessages = [`status unavailable: ${message || "request failed"}`];
  renderIssuesPanel();
}

function renderDisplayModeIssues() {
  const displayMode = currentDisplayMode();
  state.displayIssueMessages = shouldWarnAboutDisplayMode(displayMode)
    ? [`Display is in ${clientModeLabel(displayMode)} mode; open the installed Home Screen app for final monitor verification`]
    : [];
  renderIssuesPanel();
}

function shouldWarnAboutDisplayMode(displayMode) {
  return isMobileClient() && displayMode !== "standalone";
}

function clientCheckAgeLabel(clientCheck) {
  const parsed = clientCheckLastSeenTimeMS(clientCheck);
  if (parsed === null) {
    return "";
  }
  return ` / seen ${formatSampleAge(parsed)}`;
}

function statusProofClientCheck(status) {
  return status?.device_client_check?.seen ? status.device_client_check : status?.client_check;
}

function clientCheckLastSeenTimeMS(clientCheck) {
  if (!clientCheck?.seen || !clientCheck.last_seen) {
    return null;
  }
  const parsed = Date.parse(clientCheck.last_seen);
  return Number.isFinite(parsed) ? parsed : null;
}

function isMobileClient() {
  const userAgent = String(navigator.userAgent || "");
  return /\b(iPhone|iPad|iPod|Android|Mobile)\b/.test(userAgent);
}

function clientModeLabel(displayMode) {
  switch (displayMode) {
    case "standalone":
      return "app";
    case "fullscreen":
      return "full";
    case "minimal-ui":
      return "mini";
    case "browser":
      return "web";
    default:
      return "mode NA";
  }
}

async function fetchSettings(options = {}) {
  const clearIssueOnSuccess = options.clearIssueOnSuccess !== false;
  const requestSeq = state.settingsRequestSeq;
  try {
    const response = await fetchWithTimeout("/api/settings", { cache: "no-store" }, auxiliaryTimeoutMS);
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const settings = await response.json();
    if (requestSeq === state.settingsRequestSeq) {
      applySettings(settings);
      if (clearIssueOnSuccess) {
        clearSettingsIssue();
      }
    }
  } catch (error) {
    if (requestSeq === state.settingsRequestSeq) {
      renderSettingsError("settings unavailable", error);
      applySettings(state.settings);
    }
  }
}

async function updateSettings(update, interaction = "") {
  const requestSeq = state.settingsRequestSeq + 1;
  state.settingsRequestSeq = requestSeq;
  state.settingsInFlight = true;
  const previousSettings = state.settings;
  const optimisticSettings = mergeSettings(state.settings, update);
  applySettings(optimisticSettings);
  try {
    const response = await fetchWithTimeout("/api/settings", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(update),
    }, auxiliaryTimeoutMS);
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const settings = await response.json();
    if (requestSeq === state.settingsRequestSeq) {
      applySettings(settings);
      clearSettingsIssue();
      sendSettingsInteraction(interaction);
    }
  } catch (error) {
    if (requestSeq === state.settingsRequestSeq) {
      applySettings(previousSettings);
      renderSettingsError("settings update failed", error);
      showTransientStatus(error.message ? `Settings failed: ${error.message}` : "Settings failed");
      fetchSettings({ clearIssueOnSuccess: false });
    }
  } finally {
    if (requestSeq === state.settingsRequestSeq) {
      state.settingsInFlight = false;
    }
  }
}

function sendSettingsInteraction(interaction) {
  interaction = String(interaction || "").trim();
  if (!interaction) {
    return;
  }
  sendClientCheck({ interaction });
}

function mergeSettings(base, update) {
  const next = { ...base, ...update };
  if (update?.thresholds) {
    next.thresholds = { ...(base.thresholds || defaultThresholds), ...update.thresholds };
  }
  return next;
}

function applySettings(settings) {
  const refreshMS = Number(settings?.refresh_ms);
  const panel = String(settings?.panel || "").toLowerCase();
  state.settings = {
    dim: Boolean(settings?.dim),
    shift: Boolean(settings?.shift),
    refresh_ms: refreshOptionsMS.includes(refreshMS) ? refreshMS : 1000,
    panel: panelOptions.includes(panel) ? panel : "all",
    thresholds: normalizeThresholds(settings?.thresholds),
  };
  state.interval = state.settings.refresh_ms;
  document.body.classList.toggle("dim", state.settings.dim);
  document.body.classList.toggle("shift", state.settings.shift);
  setPressed($("dimBtn"), state.settings.dim);
  setPressed($("shiftBtn"), state.settings.shift);
  schedulePolling();
}

function normalizeThresholds(thresholds) {
  const normalized = { ...defaultThresholds };
  for (const target of thresholdTargets) {
    normalized[target.key] = normalizeThresholdValue(thresholds?.[target.key], defaultThresholds[target.key], target);
  }
  return normalized;
}

function normalizeThresholdValue(value, fallback, target) {
  const numeric = Number(value);
  if (!Number.isInteger(numeric) || numeric < target.min || numeric > target.max) {
    return fallback;
  }
  return numeric;
}

function restoreWakePreference() {
  if (!readStoredBoolean(wakePreferenceKey)) {
    setWakeButtonIdle();
    return;
  }
  state.wakeWanted = true;
  requestWakeLock();
}

function readStoredBoolean(key) {
  try {
    return window.localStorage?.getItem(key) === "1";
  } catch {
    return false;
  }
}

function writeStoredBoolean(key, value) {
  try {
    window.localStorage?.setItem(key, value ? "1" : "0");
  } catch {
  }
}

function render(metrics) {
  $("hostname").textContent = metrics.hostname || "unknown";
  $("platform").textContent = [metrics.os, metrics.arch, metrics.platform].filter(Boolean).join(" / ");
  renderMetricTimestamp(metrics.timestamp, metrics.collection_duration_ms);
  renderMetricAlerts(metrics);
  renderCollectionErrors(metrics.collection_errors);

  const cpuMetric = metricPercent(metrics.cpu_percent);
  const memoryMetric = capacityPercent(metrics.memory);
  const primaryGPU = firstAvailableGPU(metrics.gpu);
  const gpuMetric = primaryGPU ? metricPercent(primaryGPU.usage_percent) : unavailable();
  const cpuTempMetric = numberMetric(metrics.cpu_temperature);
  const net = networkTotals(metrics.network);

  appendPrimaryMetricHistory({
    cpu: cpuMetric,
    mem: memoryMetric,
    gpu: gpuMetric,
    net: net.available ? { available: true, value: netRingPercent(net.rx) } : unavailable(),
  });

  // CPU: outer ring = utilization, inner ring = temperature, center = util %,
  // sub = live GHz. Temperature + package power show on the detail line below.
  const tempWarn = thresholdValue("temp_warn_c");
  setGauge("cpuGauge", "cpuValue", cpuMetric, "%", thresholdValue("cpu_warn"));
  setInnerRing("cpuGauge", tempRingMetric(cpuTempMetric, tempWarn));
  const cpuClock = numberMetric(metrics.cpu_clock);
  setGaugeSub("cpuSub", cpuClock.available ? (formatClock(cpuClock.value) || "--") : "--");

  // GPU: outer ring = utilization, inner ring = VRAM %, center = util %,
  // sub = used/total VRAM. Temperature + board power show on the detail line.
  const gpuWarn = thresholdValue("gpu_warn");
  setGauge("gpuGauge", "gpuValue", gpuMetric, "%", gpuWarn);
  const gpuVram = primaryGPU ? capacityPercent(primaryGPU.memory) : unavailable();
  setInnerRing("gpuGauge", vramRingMetric(gpuVram, gpuWarn));
  setGaugeSub("gpuSub", gpuVram.available
    ? `${formatGib(gpuVram.usedBytes)} / ${formatGib(gpuVram.totalBytes)}`
    : "--");

  // RAM: single ring (utilization), center = util %, sub = used/total bytes.
  setGauge("memGauge", "memValue", memoryMetric, "%", thresholdValue("memory_warn"));
  setGaugeSub("memSub", memoryMetric.available
    ? `${formatBytes(memoryMetric.usedBytes)} / ${formatBytes(memoryMetric.totalBytes)}`
    : "--");

  // NET: outer ring = download, inner ring = upload (both scaled to a 1 Gbps
  // reference), center = download rate, sub = upload rate.
  setNetGauge(net);

  renderPrimaryCardDetails(metrics, primaryGPU, net);
}

// networkTotals sums RX/TX byte rates across every interface into one aggregate
// pair for the NET gauge. available stays false until at least one interface
// reports a rate (the first poll is always warming up the counters).
function networkTotals(network) {
  if (!network || !network.available || !network.interfaces?.length) {
    return { available: false, rx: 0, tx: 0 };
  }
  let rx = 0;
  let tx = 0;
  let available = false;
  for (const item of network.interfaces) {
    const rxMetric = numberMetric(item.rx_bytes_per_second);
    const txMetric = numberMetric(item.tx_bytes_per_second);
    if (rxMetric.available) { rx += rxMetric.value; available = true; }
    if (txMetric.available) { tx += txMetric.value; available = true; }
  }
  return { available, rx, tx };
}

// netRingPercent scales a byte/second rate to a 0-100 ring fill against a 1 Gbps
// reference so the rings give a rough sense of how saturated the link is.
function netRingPercent(bytesPerSecond) {
  const value = finiteNumber(bytesPerSecond);
  if (value === null || value < 0) {
    return 0;
  }
  return clamp((value / netRingReferenceBytesPerSecond) * 100, 0, 100);
}

// setNetGauge drives the NET card: outer ring + center = download, inner ring +
// sub = upload. Network has no user threshold, so the rings warn against a fixed
// near-saturation mark and a missing sample mutes the gauge like any other.
function setNetGauge(net) {
  const gauge = $("netGauge");
  const value = $("netValue");
  if (!net.available) {
    gauge.classList.add("unavailable", "hide-inner");
    gauge.style.setProperty("--p", 100);
    gauge.style.setProperty("--c", "#394656");
    value.textContent = "NA";
    setGaugeSub("netSub", "--");
    return;
  }
  gauge.classList.remove("unavailable");
  const down = netRingPercent(net.rx);
  const up = netRingPercent(net.tx);
  gauge.style.setProperty("--p", down);
  gauge.style.setProperty("--c", colorFor(down, netRingWarnPercent));
  setInnerRing("netGauge", { available: true, value: up, color: colorFor(up, netRingWarnPercent) });
  value.textContent = `↓${formatRateCompact(net.rx)}`;
  setGaugeSub("netSub", `↑${formatRateCompact(net.tx)}`);
}

// setInnerRing drives the inner conic ring of a double gauge. Pass a ring with
// { available, value (0-100), color } or an unavailable metric to hide it.
function setInnerRing(gaugeId, ring) {
  const gauge = $(gaugeId);
  if (!gauge) { return; }
  if (!ring || !ring.available) {
    gauge.classList.add("hide-inner");
    return;
  }
  gauge.classList.remove("hide-inner");
  gauge.style.setProperty("--inner-p", clamp(ring.value, 0, 100));
  gauge.style.setProperty("--inner-c", ring.color || "var(--accent)");
}

function setGaugeSub(subId, text) {
  const el = $(subId);
  if (el) { el.textContent = text || "--"; }
}

function vramRingMetric(vram, warnThreshold) {
  if (!vram || !vram.available) {
    return { available: false };
  }
  return { available: true, value: vram.value, color: colorFor(vram.value, warnThreshold) };
}

// tempRingMetric scales a Celsius reading to a 0-100 ring fill (100C = full
// ring) and colors it with the temperature threshold so hot readings warn.
function tempRingMetric(tempMetric, warnThreshold) {
  const temp = numberMetric(tempMetric);
  if (!temp.available) {
    return { available: false };
  }
  return { available: true, value: temp.value, color: colorFor(temp.value, warnThreshold) };
}

function renderCollectionErrors(errors) {
  state.metricIssueMessages = Array.isArray(errors)
    ? errors.map((message) => String(message || "").trim()).filter(Boolean)
    : [];
  renderIssuesPanel();
}

function renderMetricError(error) {
  const message = String(error?.message || "").trim();
  state.metricIssueMessages = [`metrics unavailable: ${message || "request failed"}`];
  renderIssuesPanel();
}

function renderSettingsError(prefix, error) {
  const message = String(error?.message || "").trim();
  state.settingsIssueMessages = [`${prefix}: ${message || "request failed"}`];
  renderIssuesPanel();
}

function renderClientCheckError(error) {
  const message = String(error?.message || "").trim();
  state.clientIssueMessages = [`client check unavailable: ${message || "request failed"}`];
  renderIssuesPanel();
}

function clearClientCheckIssue() {
  if (state.clientIssueMessages.length === 0) {
    return;
  }
  state.clientIssueMessages = [];
  renderIssuesPanel();
}

function clearSettingsIssue() {
  if (state.settingsIssueMessages.length === 0) {
    return;
  }
  state.settingsIssueMessages = [];
  renderIssuesPanel();
}

function renderIssuesPanel() {
  const panel = $("issuesPanel");
  const list = $("issuesList");
  const summary = $("issuesSummary");
  const messages = [
    ...state.statusIssueMessages,
    ...state.displayIssueMessages,
    ...state.settingsIssueMessages,
    ...state.clientIssueMessages,
    ...state.metricIssueMessages,
  ];
  state.issueMessages = messages;
  if (messages.length <= collapsedIssueLimit) {
    state.issuesExpanded = false;
  }
  panel.hidden = messages.length === 0;
  // The "more status" page always shows something: the issue list, or an "all
  // clear" placeholder when there is nothing to report.
  const empty = $("issuesEmpty");
  if (empty) {
    empty.hidden = messages.length !== 0;
  }
  panel.classList.toggle("expanded", state.issuesExpanded);
  panel.setAttribute("aria-expanded", state.issuesExpanded ? "true" : "false");
  panel.setAttribute("aria-label", state.issuesExpanded ? "Collapse issue details" : "Expand issue details");
  list.textContent = "";
  if (messages.length === 0) {
    summary.textContent = "--";
    return;
  }

  summary.textContent = `${messages.length} issue${messages.length === 1 ? "" : "s"}`;
  const visibleMessages = state.issuesExpanded ? messages : messages.slice(0, collapsedIssueLimit);
  for (const message of visibleMessages) {
    list.append(issueRow(message));
  }
  if (!state.issuesExpanded && messages.length > collapsedIssueLimit) {
    list.append(issueRow(`${messages.length - collapsedIssueLimit} more`));
  }
}

function renderMetricAlerts(metrics) {
  const panel = $("alertsPanel");
  const list = $("alertsList");
  const summary = $("alertsSummary");
  const messages = metricAlertMessages(metrics);
  state.alertMessages = messages;
  if (messages.length <= collapsedIssueLimit) {
    state.alertsExpanded = false;
  }
  panel.hidden = messages.length === 0;
  panel.classList.toggle("expanded", state.alertsExpanded);
  panel.setAttribute("aria-expanded", state.alertsExpanded ? "true" : "false");
  panel.setAttribute("aria-label", state.alertsExpanded ? "Collapse alert details" : "Expand alert details");
  list.textContent = "";
  if (messages.length === 0) {
    summary.textContent = "--";
    return;
  }

  summary.textContent = `${messages.length} alert${messages.length === 1 ? "" : "s"}`;
  const visibleMessages = state.alertsExpanded ? messages : messages.slice(0, collapsedIssueLimit);
  for (const message of visibleMessages) {
    list.append(alertRow(message));
  }
  if (!state.alertsExpanded && messages.length > collapsedIssueLimit) {
    list.append(alertRow(`${messages.length - collapsedIssueLimit} more`));
  }
}

function metricAlertMessages(metrics) {
  const messages = [];
  addPercentAlert(messages, "CPU", metricPercent(metrics?.cpu_percent), thresholdValue("cpu_warn"));
  addPercentAlert(messages, "RAM", capacityPercent(metrics?.memory), thresholdValue("memory_warn"));
  for (const disk of metrics?.disks || []) {
    const label = disk.mountpoint || disk.name || "disk";
    addPercentAlert(messages, `Disk ${label}`, capacityPercent(disk.capacity), thresholdValue("disk_warn"));
  }
  for (const device of metrics?.gpu?.devices || []) {
    const label = device.name || "GPU";
    addPercentAlert(messages, `${label} load`, metricPercent(device.usage_percent), thresholdValue("gpu_warn"));
    addPercentAlert(messages, `${label} VRAM`, capacityPercent(device.memory), thresholdValue("gpu_warn"));
    addTemperatureAlert(messages, `${label} temp`, numberMetric(device.temperature_celsius), thresholdValue("temp_warn_c"));
  }
  for (const sensor of metrics?.temperatures?.sensors || []) {
    addTemperatureAlert(messages, sensor.name || "sensor", numberMetric(sensor.celsius), thresholdValue("temp_warn_c"));
  }
  return messages;
}

function addPercentAlert(messages, label, metric, threshold) {
  if (!metric.available || metric.value < threshold) {
    return;
  }
  messages.push(`${label} ${Math.round(metric.value)}% over ${threshold}%`);
}

function addTemperatureAlert(messages, label, metric, threshold) {
  if (!metric.available || metric.value < threshold) {
    return;
  }
  messages.push(`${label} ${Math.round(metric.value)}C over ${threshold}C`);
}

function toggleIssuesPanel() {
  // If the user is selecting text inside the rows to copy it, do not collapse
  // the panel out from under the selection.
  if (window.getSelection && window.getSelection().toString()) {
    return;
  }
  if (state.issueMessages.length <= collapsedIssueLimit) {
    return;
  }
  state.issuesExpanded = !state.issuesExpanded;
  renderIssuesPanel();
}

function toggleAlertsPanel() {
  if (window.getSelection && window.getSelection().toString()) {
    return;
  }
  if (state.alertMessages.length <= collapsedIssueLimit) {
    return;
  }
  state.alertsExpanded = !state.alertsExpanded;
  renderMetricAlertsFromMessages(state.alertMessages);
}

function renderMetricAlertsFromMessages(messages) {
  const panel = $("alertsPanel");
  const list = $("alertsList");
  const summary = $("alertsSummary");
  panel.hidden = messages.length === 0;
  panel.classList.toggle("expanded", state.alertsExpanded);
  panel.setAttribute("aria-expanded", state.alertsExpanded ? "true" : "false");
  panel.setAttribute("aria-label", state.alertsExpanded ? "Collapse alert details" : "Expand alert details");
  list.textContent = "";
  summary.textContent = messages.length === 0 ? "--" : `${messages.length} alert${messages.length === 1 ? "" : "s"}`;
  const visibleMessages = state.alertsExpanded ? messages : messages.slice(0, collapsedIssueLimit);
  for (const message of visibleMessages) {
    list.append(alertRow(message));
  }
  if (!state.alertsExpanded && messages.length > collapsedIssueLimit) {
    list.append(alertRow(`${messages.length - collapsedIssueLimit} more`));
  }
}

function handleAlertsPanelKeydown(event) {
  if (event.key !== "Enter" && event.key !== " ") {
    return;
  }
  event.preventDefault();
  toggleAlertsPanel();
}

function handleIssuesPanelKeydown(event) {
  if (event.key !== "Enter" && event.key !== " ") {
    return;
  }
  event.preventDefault();
  toggleIssuesPanel();
}

function setGauge(gaugeId, valueId, metric, unit, warnThreshold = defaultThresholds.cpu_warn) {
  const gauge = $(gaugeId);
  const value = $(valueId);
  const gaugeValue = metric.available ? clamp(metric.value, 0, 100) : 0;
  gauge.classList.toggle("unavailable", !metric.available);
  if (metric.available) {
    gauge.style.setProperty("--p", gaugeValue);
    gauge.style.setProperty("--c", colorFor(gaugeValue, warnThreshold));
  } else {
    gauge.style.setProperty("--p", 100);
    gauge.style.setProperty("--c", "#394656");
  }
  value.textContent = metric.available ? `${Math.round(metric.value)}${unit}` : "NA";
}

function appendPrimaryMetricHistory(metrics) {
  appendMetricHistory("cpu", metrics.cpu);
  appendMetricHistory("mem", metrics.mem);
  appendMetricHistory("gpu", metrics.gpu);
  appendMetricHistory("net", metrics.net);
  renderSparkline("cpuTrend", state.history.cpu, thresholdValue("cpu_warn"));
  renderSparkline("memTrend", state.history.mem, thresholdValue("memory_warn"));
  renderSparkline("gpuTrend", state.history.gpu, thresholdValue("gpu_warn"));
  renderSparkline("netTrend", state.history.net, netRingWarnPercent);
}

function appendMetricHistory(key, metric) {
  const samples = state.history[key] || [];
  samples.push(metric?.available ? clamp(metric.value, 0, 100) : null);
  if (samples.length > sparklineSampleLimit) {
    samples.splice(0, samples.length - sparklineSampleLimit);
  }
  state.history[key] = samples;
}

function renderSparkline(id, samples, warnThreshold) {
  const container = $(id);
  const recentSamples = (samples || []).slice(-sparklineSampleLimit);
  const paddedSamples = [
    ...Array(Math.max(0, sparklineSampleLimit - recentSamples.length)).fill(null),
    ...recentSamples,
  ];
  container.textContent = "";
  for (const sample of paddedSamples) {
    container.append(sparklineBar(sample, warnThreshold));
  }
}

function sparklineBar(sample, warnThreshold) {
  const bar = document.createElement("span");
  bar.className = "sparkline-bar";
  if (sample === null || !Number.isFinite(sample)) {
    bar.classList.add("unavailable");
    return bar;
  }
  const value = clamp(sample, 0, 100);
  bar.style.setProperty("--h", `${Math.max(6, Math.round(value))}%`);
  bar.style.setProperty("--c", colorFor(value, warnThreshold));
  return bar;
}

// renderPrimaryCardDetails fills the small detail line under each gauge with the
// readings NOT already shown by the rings/center labels: CPU/GPU temperature and
// power draw, RAM headroom, and the full network throughput. The detail line is
// colour-coded by the temperature threshold for the CPU and GPU cards.
function renderPrimaryCardDetails(metrics, primaryGPU, net) {
  const tempWarn = thresholdValue("temp_warn_c");

  const cpuTemp = numberMetric(metrics.cpu_temperature);
  const cpuPower = numberMetric(metrics.cpu_power);
  setCardDetail("cpuDetail", joinDetail([
    cpuTemp.available ? `${Math.round(cpuTemp.value)}C` : "",
    cpuPower.available ? formatPower(cpuPower.value) : "",
  ]), cpuTemp, tempWarn);

  const gpuTemp = primaryGPU ? numberMetric(primaryGPU.temperature_celsius) : unavailable();
  const gpuPower = primaryGPU ? numberMetric(primaryGPU.power_watts) : unavailable();
  setCardDetail("gpuDetail", joinDetail([
    gpuTemp.available ? `${Math.round(gpuTemp.value)}C` : "",
    gpuPower.available ? formatPower(gpuPower.value) : "",
  ]), gpuTemp, tempWarn);

  const memory = capacityPercent(metrics.memory);
  setCardDetail("memDetail", memory.available
    ? `${formatBytes(Math.max(0, memory.totalBytes - memory.usedBytes))} free`
    : "", null, null);

  setCardDetail("netDetail", net.available
    ? `${formatBytes(net.rx)}/s down / ${formatBytes(net.tx)}/s up`
    : "", null, null);
}

// joinDetail joins the non-empty parts of a detail line with a thin separator.
function joinDetail(parts) {
  return parts.filter(Boolean).join(" · ");
}

function setCardDetail(id, text, tempMetric, warnThreshold) {
  const el = $(id);
  if (!el) { return; }
  el.textContent = text || "--";
  el.classList.remove("warn", "crit");
  if (tempMetric && tempMetric.available) {
    const warn = finiteNumber(warnThreshold);
    if (warn !== null && tempMetric.value >= warn + 15) {
      el.classList.add("crit");
    } else if (warn !== null && tempMetric.value >= warn) {
      el.classList.add("warn");
    }
  }
}

function metricPercent(metric) {
  return numberMetric(metric);
}

function capacityPercent(capacity) {
  if (!capacity) {
    return unavailable("capacity unavailable");
  }
  if (!capacity.available) {
    return unavailable(capacity.error || "capacity unavailable");
  }
  const usedBytes = safeIntegerNumber(capacity.used_bytes);
  const totalBytes = safeIntegerNumber(capacity.total_bytes);
  const value = finiteNumber(capacity.percent);
  if (value === null || value < 0 || value > 100) {
    return unavailable(capacity.error || "invalid capacity value");
  }
  if (usedBytes === null || totalBytes === null || usedBytes < 0 || totalBytes <= 0 || usedBytes > totalBytes) {
    return unavailable(capacity.error || "invalid capacity counters");
  }
  return { available: true, value, usedBytes, totalBytes };
}

function numberMetric(metric) {
  if (!metric) {
    return unavailable("metric unavailable");
  }
  if (!metric.available) {
    return unavailable(metric.error || "metric unavailable");
  }
  const value = finiteNumber(metric.value);
  return value === null ? unavailable(metric.error || "invalid numeric value") : { available: true, value };
}

function unavailable(error = "") {
  return { available: false, value: 0, error };
}

function firstAvailableGPU(gpu) {
  if (!gpu || !gpu.available || !gpu.devices?.length) {
    return null;
  }
  return gpu.devices.find((device) => device.usage_percent?.available) || gpu.devices[0];
}

function issueRow(message) {
  const row = document.createElement("div");
  row.className = "row issue-row";
  row.textContent = message;
  return row;
}

function alertRow(message) {
  const row = document.createElement("div");
  row.className = "row alert-row";
  row.textContent = message;
  return row;
}

function setConnectionState(kind, text) {
  clearTransientStatus();
  state.connectionKind = kind;
  state.connectionText = text;
  const dot = $("statusDot");
  dot.classList.toggle("ok", kind === "ok");
  dot.classList.toggle("bad", kind === "bad");
  dot.classList.toggle("paused", kind === "paused");
  dot.classList.toggle("warn", kind === "warn" || kind === "loading");
  $("statusText").textContent = text;
}

function showTransientStatus(text) {
  clearTransientStatus();
  $("statusText").textContent = text;
  state.transientStatusTimer = setTimeout(() => {
    state.transientStatusTimer = null;
    $("statusText").textContent = state.connectionText || "Updating";
  }, 3500);
}

function clearTransientStatus() {
  if (!state.transientStatusTimer) {
    return;
  }
  clearTimeout(state.transientStatusTimer);
  state.transientStatusTimer = null;
}

function thresholdValue(key) {
  return state.settings.thresholds?.[key] ?? defaultThresholds[key] ?? defaultThresholds.cpu_warn;
}

// applyControlCapabilities enables/disables the host-control buttons from the
// /api/status `controls` array. A partial status without `controls` (or a
// non-array) is a no-op, so the buttons keep whatever state they last had.
function applyControlCapabilities(controls) {
  if (!Array.isArray(controls)) {
    return;
  }
  const available = new Set();
  for (const capability of controls) {
    if (capability && capability.available) {
      available.add(capability.action);
    }
  }
  for (const [action, id] of Object.entries(controlButtonIDs)) {
    const button = $(id);
    button.disabled = !available.has(action);
  }
}

// sendControl POSTs one host-control action and reflects the outcome. mic/volume
// are mute toggles whose `state` ("muted"/"unmuted") drives the pressed glyph;
// media/lock just confirm they were applied. Any failure degrades to a transient
// status line -- it never throws.
async function sendControl(action, button) {
  if (!button || button.disabled) {
    return;
  }
  const label = controlActionLabels[action] || "Control";
  try {
    const response = await fetchWithTimeout(
      "/api/control",
      {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ action }),
      },
      auxiliaryTimeoutMS,
    );
    if (!response.ok) {
      throw new Error(`HTTP ${response.status}`);
    }
    const result = await response.json();
    if (result?.applied === true) {
      reflectControlResult(action, button, result, label);
      return;
    }
    const reason = String(result?.error || result?.message || "unavailable").trim();
    showTransientStatus(`${label}: ${reason || "unavailable"}`);
  } catch (error) {
    showTransientStatus(`${label}: ${controlErrorText(error)}`);
  }
}

function reflectControlResult(action, button, result, label) {
  const resultState = String(result?.state || "").toLowerCase();
  if (action === "mic_mute" || action === "volume_mute") {
    let muted;
    if (resultState === "muted") {
      muted = true;
    } else if (resultState === "unmuted") {
      muted = false;
    } else {
      // Controllers that only report "toggled" (e.g. Linux): flip optimistically.
      muted = button.getAttribute("aria-pressed") !== "true";
    }
    setControlMuted(action, button, muted);
    showTransientStatus(`${label} ${muted ? "muted" : "on"}`);
    return;
  }
  if (action === "media_toggle") {
    showTransientStatus("Media play/pause");
    return;
  }
  if (action === "lock_screen") {
    showTransientStatus("Screen locked");
    return;
  }
  showTransientStatus(`${label} done`);
}

function setControlMuted(action, button, muted) {
  setPressed(button, muted);
  if (action === "mic_mute") {
    $("micCtlLabel").textContent = muted ? "Muted" : "Mic";
    return;
  }
  if (action === "volume_mute") {
    $("volumeCtlGlyph").textContent = muted ? "🔇" : "🔊";
    $("volumeCtlLabel").textContent = muted ? "Muted" : "Speaker";
  }
}

function controlErrorText(error) {
  if (error?.name === "AbortError") {
    return "timed out";
  }
  return "request failed";
}

function togglePause() {
  state.paused = !state.paused;
  const button = $("pauseBtn");
  setIconButton(button, state.paused ? "▶" : "Ⅱ", state.paused ? "Resume updates" : "Pause updates");
  setPressed(button, state.paused);
  if (state.paused) {
    schedulePolling();
    setConnectionState("paused", "Paused");
    return;
  }
  setConnectionState("loading", "Updating");
  schedulePolling();
  fetchMetrics();
}

async function toggleWakeLock() {
  state.wakeWanted = !state.wakeWanted;
  writeStoredBoolean(wakePreferenceKey, state.wakeWanted);
  if (!state.wakeWanted) {
    const lock = state.wakeLock;
    state.wakeLock = null;
    setWakeButtonIdle();
    if (lock) {
      try {
        await lock.release();
      } catch {
      }
    }
    return;
  }
  await requestWakeLock();
}

async function requestWakeLock() {
  const button = $("wakeBtn");
  if (!("wakeLock" in navigator)) {
    state.wakeWanted = false;
    writeStoredBoolean(wakePreferenceKey, false);
    setIconButton(button, "×", "Wake lock unavailable");
    setPressed(button, false);
    return;
  }
  try {
    const lock = await navigator.wakeLock.request("screen");
    state.wakeLock = lock;
    setIconButton(button, "☀", "Screen awake");
    setPressed(button, true);
    lock.addEventListener("release", () => handleWakeLockRelease(lock));
  } catch {
    state.wakeWanted = false;
    writeStoredBoolean(wakePreferenceKey, false);
    setIconButton(button, "×", "Wake lock denied");
    setPressed(button, false);
  }
}

function handleWakeLockRelease(lock) {
  if (state.wakeLock !== lock) {
    return;
  }
  state.wakeLock = null;
  if (!state.wakeWanted) {
    setWakeButtonIdle();
    return;
  }
  const button = $("wakeBtn");
  setIconButton(button, "☀", "Wake lock retrying");
  setPressed(button, false);
  if (document.visibilityState === "visible") {
    requestWakeLock();
  }
}

function setWakeButtonIdle() {
  const button = $("wakeBtn");
  setIconButton(button, "☀", "Keep screen awake");
  setPressed(button, false);
}

function setIconButton(button, glyph, label) {
  button.textContent = glyph;
  button.title = label;
  button.setAttribute("aria-label", label);
}

function setPressed(button, pressed) {
  button.classList.toggle("active", pressed);
  button.setAttribute("aria-pressed", pressed ? "true" : "false");
}

function formatBytes(bytes) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = finiteNumber(bytes);
  if (value === null || value < 0) {
    value = 0;
  }
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value >= 10 || unit === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[unit]}`;
}

// formatRateCompact renders a byte/second rate as a short SI-ish string for the
// NET gauge center/sub labels where space is tight (e.g. "12M", "8.4M", "940K").
// Decimal (1000) base keeps it aligned with how link speeds are quoted.
function formatRateCompact(bytesPerSecond) {
  const value = finiteNumber(bytesPerSecond);
  if (value === null || value < 0) {
    return "0";
  }
  const units = ["B", "K", "M", "G", "T"];
  let v = value;
  let u = 0;
  while (v >= 1000 && u < units.length - 1) {
    v /= 1000;
    u += 1;
  }
  return `${v >= 100 || u === 0 ? v.toFixed(0) : v.toFixed(1)}${units[u]}`;
}

function formatPower(watts) {
  const value = finiteNumber(watts);
  if (value === null || value < 0) {
    return "NA";
  }
  if (value >= 1000) {
    return `${(value / 1000).toFixed(2)} kW`;
  }
  return `${value >= 100 ? value.toFixed(0) : value.toFixed(1)} W`;
}

// formatClock renders a CPU clock reading (MHz from the API) as a compact GHz
// value, falling back to MHz for sub-GHz readings (rare on modern CPUs).
function formatClock(mhz) {
  const value = finiteNumber(mhz);
  if (value === null || value <= 0) {
    return null;
  }
  if (value >= 1000) {
    return `${(value / 1000).toFixed(2)} GHz`;
  }
  return `${Math.round(value)} MHz`;
}

// formatGib renders a byte count as whole gibibytes with one decimal for the
// fractional part. Used for GPU VRAM where percentage is less meaningful than
// the absolute used/total budget.
function formatGib(bytes) {
  const value = finiteNumber(bytes);
  if (value === null || value < 0) {
    return null;
  }
  const gib = value / (1024 ** 3);
  if (gib >= 100) {
    return `${gib.toFixed(0)} GB`;
  }
  return `${gib.toFixed(1)} GB`;
}

function formatTime(timestamp) {
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) {
    return "--";
  }
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

function renderMetricTimestamp(timestamp, durationMS) {
  const parsed = Date.parse(timestamp);
  if (!Number.isFinite(parsed)) {
    state.lastMetricTimestampMS = 0;
    state.lastCollectionDurationMS = null;
    $("updatedAt").textContent = "--";
    return;
  }
  state.lastMetricTimestampMS = parsed;
  state.lastCollectionDurationMS = normalizedDurationMS(durationMS);
  refreshMetricAge();
}

function refreshMetricAge() {
  if (!state.lastMetricTimestampMS) {
    return;
  }
  const durationLabel = state.lastCollectionDurationMS === null ? "" : ` / ${formatDurationMS(state.lastCollectionDurationMS)}`;
  $("updatedAt").textContent = `${formatTime(state.lastMetricTimestampMS)} / ${formatSampleAge(state.lastMetricTimestampMS)}${durationLabel}`;
}

function formatSampleAge(timestampMS) {
  const ageSeconds = Math.max(0, Math.floor((nowMS() - timestampMS) / 1000));
  if (ageSeconds < 60) {
    return `${ageSeconds}s`;
  }
  const ageMinutes = Math.floor(ageSeconds / 60);
  if (ageMinutes < 60) {
    return `${ageMinutes}m`;
  }
  return `${Math.floor(ageMinutes / 60)}h`;
}

function normalizedDurationMS(value) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric) || numeric < 0) {
    return null;
  }
  return Math.round(numeric);
}

function formatDurationMS(milliseconds) {
  if (milliseconds < 1000) {
    return `${milliseconds}ms`;
  }
  return `${(milliseconds / 1000).toFixed(milliseconds < 10000 ? 1 : 0)}s`;
}

function formatDuration(seconds) {
  const numeric = finiteNumber(seconds);
  let value = Math.max(0, Math.floor(numeric === null ? 0 : numeric));
  const days = Math.floor(value / 86400);
  value %= 86400;
  const hours = Math.floor(value / 3600);
  value %= 3600;
  const minutes = Math.floor(value / 60);

  if (days > 0) {
    return `${days}d ${hours}h`;
  }
  if (hours > 0) {
    return `${hours}h ${minutes}m`;
  }
  return `${minutes}m`;
}

function colorFor(percent, warnThreshold = defaultThresholds.cpu_warn) {
  const value = finiteNumber(percent);
  if (value === null) {
    return "var(--good)";
  }
  const warn = finiteNumber(warnThreshold) ?? defaultThresholds.cpu_warn;
  const critical = Math.min(100, warn + 15);
  if (value >= critical) {
    return "var(--bad)";
  }
  if (value >= warn) {
    return "var(--warn)";
  }
  return "var(--good)";
}

function clamp(value, min, max) {
  const numeric = finiteNumber(value);
  if (numeric === null) {
    return min;
  }
  return Math.max(min, Math.min(max, numeric));
}

function finiteNumber(value) {
  if (value === null || value === undefined || value === "") {
    return null;
  }
  const numeric = Number(value);
  return Number.isFinite(numeric) ? numeric : null;
}

function safeIntegerNumber(value) {
  const numeric = finiteNumber(value);
  return numeric !== null && Number.isSafeInteger(numeric) ? numeric : null;
}

function nowMS() {
  if (typeof window.__sysmonNow === "function") {
    return window.__sysmonNow();
  }
  return Date.now();
}
