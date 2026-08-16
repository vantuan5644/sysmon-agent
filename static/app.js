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
  controlArmedAction: null,
  controlArmTimer: null,
  updatePollTimer: null,
  paused: false,
  stream: null,
  streamFallback: false,
  streamFailures: 0,
  lastStreamAtMS: 0,
  streamReconnects: 0,
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
  lastProcesses: null,
  staticRefreshInFlight: false,
  lastMetricsAtMS: 0,
  lastMetricTimestampMS: 0,
  lastCollectionDurationMS: null,
  connectionKind: "loading",
  wakeLock: null,
  wakeWanted: false,
  // updateAvailable is the latest "update available" status from /api/status.
  // updateApplying flips true after a successful POST /api/update (the agent
  // returns 202 and the dashboard waits for the staleness reload).
  updateAvailable: false,
  updateLatestVersion: "",
  updateURL: "",
  updateApplying: false,
  updateMessage: "",
  // updateApplyingFromVersion is the agent version we were running when the
  // apply started. The agent reporting anything else means the swap landed, so
  // the "Updating..." state can be left even if the service-worker reload has
  // not fired yet. updateWatchdogTimer is the always-armed escape hatch.
  updateApplyingFromVersion: "",
  updateWatchdogTimer: null,
  autoShellRefreshAttempted: false,
  // agentVersion is the running binary's version tag from /api/status, and
  // agentChannel is "release" for a real vX.Y.Z tag or "dev" for an untagged
  // `go build`. A dev build never self-updates, so the channel also decides
  // whether any update UI is shown at all.
  agentVersion: "",
  agentChannel: "",
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
const dashboardBuild = "sysmon-static-v125";
const netRingReferenceBytesPerSecond = 125000000;
const netRingWarnPercent = 90;
// clockRingReferenceMHz is the fallback ceiling for the CPU inner ring when the
// CPU's max/boost clock isn't reported, so the ring still has a sensible 0-100
// scale. ~5 GHz covers current desktop boost clocks.
const clockRingReferenceMHz = 5000;
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
// updateDismissedKey returns the localStorage key for a per-version update
// dismissal. Dismissing vX.Y.Z once keeps the banner quiet across reloads until
// a newer version ships (then the new tag changes the key and the banner
// re-appears, by design, so the user hears about each new release once).
const updateDismissedKeyPrefix = "sysmon:update-dismissed-";
const metricsTimeoutMS = 4500;
const auxiliaryTimeoutMS = 3000;
// Number of consecutive EventSource failures (with no recovery in between)
// before we abandon the live /api/stream and fall back to polling /api/metrics
// for the rest of the session. A successful open/message resets the counter, so
// transient blips that recover never demote us; persistent failure does.
const streamFailureLimit = 3;
// How long an open /api/stream may go silent before the dashboard treats the
// connection as dead and rebuilds it. The sampler publishes a snapshot every
// fast-lane tick (~5 Hz by default), so this is ~75 missed frames -- silence
// this long is a dead socket, never a quiet one. The server's keepalive comments
// are deliberately not a liveness signal here: EventSource never surfaces them
// as message events, so only real data frames can reset this timer.
const streamSilenceLimitMS = 15000;
const clientCheckIntervalMS = 30000;
const clientCheckStaleAfterMS = clientCheckIntervalMS * 3;
const clientCheckDebounceMS = 500;
// controlArmWindowMS is how long the Lock Screen button stays armed after the
// first tap before it auto-disarms. The armed label ("Confirm?") is the
// authoritative affordance; this is short enough that an accidental arm does
// not linger but long enough to land the confirming second tap.
const controlArmWindowMS = 3000;
const collapsedIssueLimit = 5;
const sparklineSampleLimit = 24;
const wakePreferenceKey = "sysmon:wake-wanted";

// Set by setupPager so content re-renders (e.g. the Issues list growing) can ask
// the pager to re-measure the active page and resize to fit it.
let pagerSyncHeight = null;

// Per-process "App details" page UI state. Client-only -- not persisted
// server-side -- so a reload returns to the Apps view sorted by CPU. appsView
// is "apps" (grouped by executable) or "processes" (one row per PID); procSort
// drives the client-side re-sort within the server-sent top-N rows.
let appsView = "apps";
let procSort = { col: "cpu", dir: "desc" };

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
  $("updateApplyBtn").addEventListener("click", sendUpdate);
  $("updateDismissBtn").addEventListener("click", dismissUpdate);
  $("issuesPanel").addEventListener("click", toggleIssuesPanel);
  $("issuesPanel").addEventListener("keydown", handleIssuesPanelKeydown);
  $("processesViewApps").addEventListener("click", () => setProcessesView("apps"));
  $("processesViewProcs").addEventListener("click", () => setProcessesView("processes"));
  for (const button of document.querySelectorAll(".processes-sort")) {
    button.addEventListener("click", () => setProcSort(button.dataset.procSort));
  }
  $("statusStrip").addEventListener("click", refreshNow);
  $("statusStrip").addEventListener("keydown", (event) => {
    if (event.key === "Enter" || event.key === " ") {
      event.preventDefault();
      refreshNow();
    }
  });
  setupPager();
  reflectProcSort();

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
  const dots = [$("pageDot0"), $("pageDot1"), $("pageDot2")];
  if (!pager || dots.some((dot) => !dot)) {
    return;
  }
  // querySelectorAll is feature-guarded so the headless verifier's id-only DOM
  // mock yields an empty list (syncHeight then no-ops) instead of throwing.
  const pages =
    typeof pager.querySelectorAll === "function"
      ? Array.from(pager.querySelectorAll(".page"))
      : [];
  const pageCount = dots.length;
  let activePage = 0;

  // Landscape kiosk mode (CSS `@media (orientation: landscape) and
  // (max-height: 500px)`) deliberately flexes the pager to fill the viewport
  // height, so leave its height to CSS there and only pin a height in the
  // portrait/desktop layout.
  const landscapeQuery =
    typeof window.matchMedia === "function"
      ? window.matchMedia("(orientation: landscape) and (max-height: 500px)")
      : null;

  // Per-page scrolling: each .page now owns its own vertical scroller (CSS
  // height: 100% + overflow-y: auto on a bounded pager), so the pager no longer
  // needs its height pinned to the active page. Kept as a no-op stub because
  // the render paths still call syncPagerAfterRender() and the layout-less
  // verifier DOM must not touch geometry APIs. Guarded for that mock regardless.
  const syncHeight = () => {
    if (!pages.length || typeof pager.getBoundingClientRect !== "function") {
      return;
    }
  };
  pagerSyncHeight = syncHeight;

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
    // Reset the incoming page's vertical scroll to the top so swiping back from
    // a scrolled-down page lands at the top instead of inheriting a stale
    // scrollTop. Feature-guarded for the layout-less verifier DOM.
    const incoming = pages[clamped];
    if (incoming && typeof incoming.scrollTop === "number") {
      incoming.scrollTop = 0;
    }
  };

  // Re-measure once the horizontal swipe settles rather than mid-scroll, so a
  // taller incoming page only grows the pager after it has snapped into place
  // (the swipe itself stays at the outgoing page's height and looks smooth).
  let settleTimer = null;
  pager.addEventListener("scroll", () => {
    const width = pager.clientWidth || 0;
    if (width > 0) {
      setActive(Math.round((pager.scrollLeft || 0) / width));
    }
    if (settleTimer) {
      clearTimeout(settleTimer);
    }
    settleTimer = setTimeout(syncHeight, 90);
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
      syncHeight();
    });
  });

  if (landscapeQuery) {
    const onModeChange = () => syncHeight();
    if (typeof landscapeQuery.addEventListener === "function") {
      landscapeQuery.addEventListener("change", onModeChange);
    } else if (typeof landscapeQuery.addListener === "function") {
      landscapeQuery.addListener(onModeChange);
    }
  }

  window.addEventListener("resize", syncHeight);
  window.addEventListener("orientationchange", syncHeight);
  syncHeight();
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
  // Give the fresh connection a full silence window before the watchdog may
  // judge it, so a slow open is never mistaken for a dead socket.
  state.lastStreamAtMS = nowMS();
  source.onopen = () => {
    state.streamFailures = 0;
    state.lastStreamAtMS = nowMS();
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
  state.streamReconnects = 0;
  state.lastStreamAtMS = nowMS();
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
  state.staleTimer = setInterval(checkDashboardLiveness, 5000);
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

// checkDashboardLiveness is the periodic health tick: repair a dead stream
// first, then label the data stale if it is. Order matters -- the repair path
// refetches metrics, so labelling first would flag a staleness the same tick is
// already fixing.
function checkDashboardLiveness() {
  reviveStreamIfSilent();
  markStaleIfNeeded();
}

// reviveStreamIfSilent rebuilds a stream that stopped delivering without the
// browser ever firing `error`. An agent restart, a host reboot, a sleep/resume
// or a WiFi roam can leave the EventSource in a zombie OPEN state: the socket is
// dead but no error event ever arrives, so handleStreamError -- the only other
// recovery path -- never runs, and the dashboard sits frozen on its last frame
// until someone taps the status strip. Repeated silent reconnects mean streaming
// does not work on this path at all, so we demote to polling on the same
// threshold handleStreamError uses instead of reconnecting forever.
function reviveStreamIfSilent() {
  if (!state.stream || state.paused || document.visibilityState !== "visible") {
    return;
  }
  if (nowMS() - state.lastStreamAtMS <= streamSilenceLimitMS) {
    return;
  }
  state.streamReconnects += 1;
  if (state.streamReconnects >= streamFailureLimit) {
    state.streamFallback = true;
  }
  closeStream();
  // schedulePolling reopens the stream, or starts the metrics poll once the
  // reconnect budget is spent; the fetch repaints immediately either way rather
  // than leaving the last frame up until the new source delivers.
  schedulePolling();
  fetchMetrics();
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
  renderAgentVersion(status);
  renderUpdatePanel(status);
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
    maybeAutoRefreshStaleShell(serverBuild);
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

// releaseVersionPattern matches a real release tag ("v1.2.3" / "1.2.3", with an
// optional pre-release suffix). Anything else — notably the "dev" placeholder a
// plain `go build` leaves in main.version — is a dev build. This mirrors the
// agent's own parseSemver check so the badge agrees with what the server will
// actually do about updates.
const releaseVersionPattern = /^v?\d+\.\d+\.\d+([-+].*)?$/;

// renderAgentVersion fills the status-strip build badge: "v0.1.4" for a shipped
// release, "dev" for a hand-built binary. Knowing which one a host is running
// is the difference between "why is there no update banner" and a real problem.
function renderAgentVersion(status) {
  // Resolve the channel BEFORE touching the DOM. A service worker can serve a
  // cached index.html without #agentVersion alongside a newer app.js, and if
  // this bailed out on the missing element it would leave state.agentChannel
  // empty — which makes isDevBuild() false and un-suppresses the update panel
  // on a dev build. The channel must not depend on the badge existing.
  const version = String(status?.version || "").trim();
  state.agentVersion = version;
  // Older agents (and pre-update-subsystem builds) report no version at all;
  // treat that as "unknown", not as a release.
  state.agentChannel = version ? (releaseVersionPattern.test(version) ? "release" : "dev") : "";

  const badge = $("agentVersion");
  if (!badge) {
    return;
  }
  if (!state.agentChannel) {
    badge.hidden = true;
    badge.classList.remove("is-release", "is-dev");
    badge.textContent = "";
    return;
  }
  const isRelease = state.agentChannel === "release";
  badge.hidden = false;
  badge.classList.toggle("is-release", isRelease);
  badge.classList.toggle("is-dev", !isRelease);
  badge.textContent = isRelease ? `release ${version}` : version;
  badge.title = isRelease
    ? `Running release ${version}`
    : `Running a development build (${version}) - self-update is disabled`;
}

// isDevBuild reports whether the agent is an untagged build. Dev builds cannot
// self-update (the agent refuses, since there is no version to compare), so the
// entire update panel — banner *and* error states — is suppressed for them: the
// "dev" badge in the status strip already says why, and an update notice that
// can never be acted on is just noise.
function isDevBuild() {
  return state.agentChannel === "dev";
}

// renderUpdatePanel surfaces a non-intrusive banner when /api/status reports an
// update available. The banner says "vX.Y.Z available - Update" and is dismissed
// per-version (remembered in localStorage so it does not nag every load). When
// self-update is unsupported on the host (Linux / console run) the banner stays
// hidden even if an update is available; a one-line status-strip note could be
// added later if we want to direct users to the installer path.
function renderUpdatePanel(status) {
  const panel = $("updatePanel");
  if (!panel) {
    return;
  }
  // A dev build never self-updates, so it gets no update UI at all — not a
  // banner, not an error. The "dev" build badge is the explanation.
  if (isDevBuild()) {
    panel.hidden = true;
    panel.classList.remove("update-applying", "update-error");
    state.updateAvailable = false;
    return;
  }
  const update = status?.update || {};
  const latest = String(update.latest_version || "").trim();
  const available = update.available === true && latest !== "";
  state.updateAvailable = available;
  state.updateLatestVersion = latest;
  state.updateURL = String(update.url || "");

  panel.classList.remove("update-applying", "update-error");
  const text = $("updateText");
  const applyBtn = $("updateApplyBtn");

  // The agent reporting a version other than the one we started from means the
  // swap landed. Leave the applying state on that signal rather than waiting
  // for the service-worker staleness reload, which may not fire (and never
  // fires at all if the update rolled back to the same version).
  if (
    state.updateApplying &&
    state.agentVersion &&
    state.updateApplyingFromVersion &&
    state.agentVersion !== state.updateApplyingFromVersion
  ) {
    endUpdateApplying();
  }

  if (state.updateApplying) {
    panel.hidden = false;
    text.textContent = `Updating to ${latest} - the dashboard will reload automatically.`;
    panel.classList.add("update-applying");
    // Only the apply button is disabled. The dismiss × stays live: if the agent
    // never comes back, this box is the user's only way out of it.
    if (applyBtn) applyBtn.disabled = true;
    return;
  }

  const supported = status?.update_check_supported === true;
  const dismissed = latest !== "" && readStoredBoolean(updateDismissedKeyPrefix + latest);
  if (!available || !supported || dismissed) {
    panel.hidden = true;
    if (applyBtn) applyBtn.disabled = false;
    return;
  }

  panel.hidden = false;
  text.textContent = `${latest} available - Update`;
  if (applyBtn) applyBtn.disabled = false;
}

// sendUpdate triggers the in-dashboard self-update: POST /api/update with an
// empty body, then on 202 flip into the "Updating..." state and wait for the
// existing dashboard-build staleness reload (the new binary's /api/status
// reports a new dashboard_build, the service worker evicts the cached shell,
// and the dashboard reloads). Non-202 outcomes are surfaced in the banner so
// the user can see why it refused (501 unsupported, 409 disabled, 502 network).
async function sendUpdate() {
  if (state.updateApplying) {
    return;
  }
  const applyBtn = $("updateApplyBtn");
  if (applyBtn) applyBtn.disabled = true;
  beginUpdateApplying();
  renderUpdatePanel({ update: { available: true, latest_version: state.updateLatestVersion, url: state.updateURL } });
  try {
    const response = await fetchWithTimeout(
      "/api/update",
      { method: "POST", headers: { "Content-Type": "application/json" }, body: "" },
      auxiliaryTimeoutMS,
    );
    const decision = await response.json().catch(() => ({}));
    if (response.status === 202) {
      renderUpdatePanel({ update: { available: true, latest_version: decision?.latest_version || state.updateLatestVersion } });
      scheduleUpdatePoll();
      return;
    }
    endUpdateApplying();
    showUpdateError(decision, response.status);
  } catch (error) {
    endUpdateApplying();
    showUpdateError({ error: error?.message || "request failed" }, 0);
  }
}

// updateApplyWatchdogMS bounds how long the "Updating..." box may persist. The
// agent stops itself mid-swap, so a failed or rolled-back update can leave the
// dashboard waiting for a reload that never comes.
const updateApplyWatchdogMS = 5 * 60 * 1000;

// beginUpdateApplying enters the applying state and arms the escape hatch. The
// watchdog is armed here rather than in scheduleUpdatePoll so it covers every
// path into this state — including one where the POST succeeds but the poll is
// never scheduled.
function beginUpdateApplying() {
  state.updateApplying = true;
  state.updateApplyingFromVersion = state.agentVersion || "";
  clearUpdateWatchdog();
  state.updateWatchdogTimer = setTimeout(() => {
    if (!state.updateApplying) {
      return;
    }
    endUpdateApplying();
    showUpdateError(
      { error: "the agent did not come back in time; it may have rolled back to the previous version" },
      0,
    );
  }, updateApplyWatchdogMS);
}

// endUpdateApplying leaves the applying state and tears down both timers. Every
// exit from "Updating..." goes through here so the flag can never be left set
// with no timer still running to clear it.
function endUpdateApplying() {
  state.updateApplying = false;
  state.updateApplyingFromVersion = "";
  clearUpdatePoll();
  clearUpdateWatchdog();
}

function clearUpdateWatchdog() {
  if (state.updateWatchdogTimer) {
    clearTimeout(state.updateWatchdogTimer);
    state.updateWatchdogTimer = null;
  }
}

// scheduleUpdatePoll keeps the /api/status poll warm while the agent swaps +
// restarts so the dashboard notices the new build as soon as it is up. Without
// this the existing poll cadence would still catch it, but we poll more eagerly
// for a short window to surface the reload quickly.
function scheduleUpdatePoll() {
  if (state.updatePollTimer) {
    return;
  }
  state.updatePollTimer = setInterval(() => {
    if (!state.updateApplying) {
      clearUpdatePoll();
      return;
    }
    fetchStatus();
  }, 2000);
  // The timeout that bounds this state lives in beginUpdateApplying so it is
  // armed on every path, not only when the poll happens to be scheduled.
}

function clearUpdatePoll() {
  if (state.updatePollTimer) {
    clearInterval(state.updatePollTimer);
    state.updatePollTimer = null;
  }
}

// showUpdateError renders a refused update as a banner with the agent's reason
// (errUpdateUnsupported -> 501, errUpdateDisabled -> 409, errUpdateNetwork /
// errUpdateChecksum -> 502). The user can dismiss it; a retry re-checks the
// channel via the existing /api/status poll.
function showUpdateError(decision, status) {
  const panel = $("updatePanel");
  if (!panel) {
    return;
  }
  // Unreachable in normal use (a dev build never renders the Update button),
  // but keep the suppression here too so no code path can surface an update
  // failure on a build that was never going to update.
  if (isDevBuild()) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;
  panel.classList.add("update-error");
  const reason = String(decision?.error || `HTTP ${status || "?"}`).trim();
  const latest = state.updateLatestVersion || decision?.latest_version || "";
  const prefix = latest ? `${latest} update failed: ` : "Update failed: ";
  $("updateText").textContent = prefix + reason;
  const applyBtn = $("updateApplyBtn");
  if (applyBtn) applyBtn.disabled = false;
}

// dismissUpdate records a per-version dismissal in localStorage so the banner
// does not nag every load. A newer release ships under a new tag, which changes
// the dismissal key, so the banner re-appears for that release once.
function dismissUpdate() {
  // An explicit dismissal always wins, including out of the "Updating..."
  // state. Without this, renderUpdatePanel's applying branch re-shows the box
  // on every call and the × does nothing — leaving an unclosable panel if the
  // agent never comes back from a swap.
  endUpdateApplying();
  const panel = $("updatePanel");
  if (panel) {
    panel.hidden = true;
    panel.classList.remove("update-applying", "update-error");
  }
  const applyBtn = $("updateApplyBtn");
  if (applyBtn) applyBtn.disabled = false;
  state.updateAvailable = false;
  const latest = state.updateLatestVersion;
  if (latest) {
    writeStoredBoolean(updateDismissedKeyPrefix + latest, true);
  }
}

// autoShellRefreshKeyPrefix namespaces the once-per-server-build guard below.
const autoShellRefreshKeyPrefix = "sysmon:auto-shell-refresh-";

// maybeAutoRefreshStaleShell reloads the cached PWA shell when the agent is
// serving a newer build than the one running.
//
// Recovering by tapping the status strip has always been possible, but it
// requires knowing to do it — and a stale shell can be actively stuck (an older
// build could get wedged showing an update banner with no working dismiss),
// which is exactly when a user is least able to discover the gesture. So do it
// for them.
//
// Strictly once per server build, because refreshStaticAssets() reloads the
// page: if the reload does not resolve the mismatch, a second attempt would
// loop forever. The guard lives in localStorage (which survives the reload and
// the cache purge); if it cannot be persisted we skip the auto-refresh entirely
// and leave the user with the tap-to-refresh issue message, because an
// unguarded reload loop is far worse than a stale shell.
function maybeAutoRefreshStaleShell(serverBuild) {
  if (!serverBuild || serverBuild === dashboardBuild) {
    return;
  }
  if (state.autoShellRefreshAttempted || state.staticRefreshInFlight) {
    return;
  }
  const key = autoShellRefreshKeyPrefix + serverBuild;
  if (readStoredBoolean(key)) {
    return;
  }
  writeStoredBoolean(key, true);
  if (!readStoredBoolean(key)) {
    // Storage is unavailable or rejected the write; without a durable guard the
    // reloaded page would land right back here.
    return;
  }
  state.autoShellRefreshAttempted = true;
  showTransientStatus(`Updating app to ${serverBuild}`);
  refreshStaticAssets();
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
  const net = networkTotals(metrics.network);

  appendPrimaryMetricHistory({
    cpu: cpuMetric,
    mem: memoryMetric,
    gpu: gpuMetric,
    net: net.available ? { available: true, value: netRingPercent(net.rx) } : unavailable(),
  });

  // CPU: outer ring = utilization, inner ring = core clock (filled against the
  // max/boost clock), center = util %, sub = live/boost GHz -- the pair gives the
  // inner ring's fill a readable scale. Temperature + core/package power show on
  // the detail line below.
  setGauge("cpuGauge", "cpuValue", cpuMetric, "%", thresholdValue("cpu_warn"));
  setInnerRing("cpuGauge", clockRingMetric(metrics.cpu_clock, metrics.cpu_clock_max, metrics.cpu_clock_base));
  const cpuClock = numberMetric(metrics.cpu_clock);
  const cpuClockMax = numberMetric(metrics.cpu_clock_max);
  setGaugeSub("cpuSub", cpuClock.available
    ? (formatClockPair(cpuClock.value, cpuClockMax.available ? cpuClockMax.value : null) || "--")
    : "--");

  // GPU: outer ring = utilization, inner ring = VRAM %, center = util %,
  // sub = used/total VRAM. Temperature + board power show on the detail line.
  const gpuWarn = thresholdValue("gpu_warn");
  setGauge("gpuGauge", "gpuValue", gpuMetric, "%", gpuWarn);
  const gpuVram = primaryGPU ? capacityPercent(primaryGPU.memory) : unavailable();
  setInnerRing("gpuGauge", vramRingMetric(gpuVram, gpuWarn));
  setGaugeSub("gpuSub", gpuVram.available
    ? (formatGibPair(gpuVram.usedBytes, gpuVram.totalBytes) || "--")
    : "--");

  // RAM: single ring (utilization), center = util %, sub = used/total bytes.
  setGauge("memGauge", "memValue", memoryMetric, "%", thresholdValue("memory_warn"));
  setGaugeSub("memSub", memoryMetric.available
    ? `${formatBytes(memoryMetric.usedBytes)} / ${formatBytes(memoryMetric.totalBytes)}`
    : "--");

  // NET: outer ring = download, inner ring = upload (both scaled to a 1 Gbps
  // reference), center = download rate, sub = upload rate.
  setNetGauge(net);

  renderPrimaryCardDetails(metrics, primaryGPU);
  state.lastProcesses = metrics.processes || { available: false };
  renderApps(metrics.processes);
  renderStorage(metrics.storage);
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

// clockRingMetric scales the live CPU clock into a 0-100 ring fill so the inner
// ring fills as the CPU ramps toward peak frequency. When both base and max
// (boost ceiling) are reported it scales base->max, so the visible travel spans
// the actual boost headroom instead of 0->max (idle base clock would otherwise
// sit near the bottom). It degrades to clock/max, then to a fixed reference,
// when those aren't reported. A ~5% floor keeps a thin sliver visible whenever
// the CPU is clocking, so the ring is never fully empty at idle. Clock isn't an
// alert metric, so the ring keeps the steady accent colour rather than warning
// by threshold like utilization/temperature do.
function clockRingMetric(clockMetric, maxClockMetric, baseClockMetric) {
  const clock = numberMetric(clockMetric);
  if (!clock.available || clock.value <= 0) {
    return { available: false };
  }
  const max = numberMetric(maxClockMetric);
  const base = numberMetric(baseClockMetric);
  let value;
  if (base.available && base.value > 0 && max.available && max.value > base.value) {
    value = ((clock.value - base.value) / (max.value - base.value)) * 100; // base -> max
  } else if (max.available && max.value > 0) {
    value = (clock.value / max.value) * 100; // degenerate/missing base
  } else {
    value = (clock.value / clockRingReferenceMHz) * 100; // last-resort fixed reference
  }
  return { available: true, value: clamp(Math.max(5, value), 0, 100), color: "var(--accent)" };
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
    syncPagerAfterRender();
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
  syncPagerAfterRender();
}

// renderIssuesPanel changes the height of the second pager page (the issue list
// grows/shrinks, or expands on tap), so let the pager re-measure and resize to
// the active page. No-op until setupPager has run / in the layout-less verifier.
function syncPagerAfterRender() {
  if (typeof pagerSyncHeight === "function") {
    pagerSyncHeight();
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

// metricAlertMessages collects the threshold breaches the alerts panel surfaces.
// Readings that already warn on a primary gauge card are deliberately NOT
// repeated here: CPU/GPU/RAM usage lives on the outer rings, CPU die temp on
// the CPU inner ring + detail line, GPU die temp on the GPU detail line, and GPU
// VRAM on the GPU inner ring -- all of them turn red at their threshold, so
// listing them again would just be noise. Only disks (no card) and auxiliary
// temperature sensors (board, water, PSU, ...) that are not shown elsewhere
// surface as alerts.
function metricAlertMessages(metrics) {
  const messages = [];
  for (const disk of metrics?.disks || []) {
    const label = disk.mountpoint || disk.name || "disk";
    addPercentAlert(messages, `Disk ${label}`, capacityPercent(disk.capacity), thresholdValue("disk_warn"));
  }
  // Drive temperatures are alerted from storage.devices[] rather than from the
  // raw sensor list: the per-device reading is already matched to a drive, so it
  // alerts as "Samsung SSD 990 PRO" instead of an anonymous "nvme Composite"
  // repeated once per drive. isPrimaryCardTemperatureSensor suppresses the raw
  // storage sensors below so the two paths never double-report. The storage
  // panel only COLOURS its temperature, so without this a hot drive would raise
  // no alert at all.
  for (const device of metrics?.storage?.devices || []) {
    const label = String(device?.model || "").trim() || String(device?.name || "").trim() || "drive";
    addTemperatureAlert(messages, label, numberMetric(device.temperature_celsius), thresholdValue("temp_warn_c"));
  }
  for (const sensor of metrics?.temperatures?.sensors || []) {
    if (isPrimaryCardTemperatureSensor(sensor.name)) {
      continue;
    }
    addTemperatureAlert(messages, sensor.name || "sensor", numberMetric(sensor.celsius), thresholdValue("temp_warn_c"));
  }
  return messages;
}

// isPrimaryCardTemperatureSensor reports whether a temperature-sensor name is
// already shown on a primary gauge card: the CPU die reading that feeds the CPU
// card (inner ring + detail line), or a GPU die reading that feeds a GPU card
// detail line. Those carry their own warn colour on the cards, so alerting them
// again here just repeats what the rings already show; auxiliary sensors (board,
// water, PSU, chipset, ...) have no card and stay. Mirrors the Go-side
// pickCPUTemperature/isCPUTemperatureSensor classification in metrics.go.
function isPrimaryCardTemperatureSensor(name) {
  const n = String(name || "").toLowerCase();
  if (!n) {
    return false;
  }
  // GPU die sensors are each carried by a GPU device's temperature_celsius, so
  // any GPU-named sensor is treated as already shown on a GPU card.
  for (const fragment of ["gpu", "nvidia", "geforce", "radeon", "arc"]) {
    if (n.includes(fragment)) {
      return true;
    }
  }
  // Storage die sensors (NVMe/SATA SSD, HDD) are alerted from storage.devices[]
  // in metricAlertMessages, where each reading is already matched to a named
  // drive. Suppressing them here keeps the two paths from double-reporting --
  // the raw list would otherwise emit an anonymous "nvme Composite" per drive.
  for (const fragment of ["hdd", "ssd", "nvme", "disk"]) {
    if (n.includes(fragment)) {
      return true;
    }
  }
  // Reject non-CPU-die sensors (board, water, PSU, RAM) before the CPU
  // name check so e.g. a "CPU VRM"/"board" reading is not mistaken for the die.
  for (const fragment of [
    "dimm", "ram", "memory",
    "water", "ambient", "board", "chipset", "motherboard",
    "psu", "battery",
  ]) {
    if (n.includes(fragment)) {
      return false;
    }
  }
  for (const fragment of ["cpu", "core", "package", "socket", "tctl", "tdie", "tcase", "k10temp", "coretemp", "k8temp", "ryzen", "xeon", "epyc", "threadripper"]) {
    if (n.includes(fragment)) {
      return true;
    }
  }
  return false;
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
  messages.push(`${label} ${formatTemp(metric.value)} over ${formatTemp(threshold)}`);
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
  // CPU's trend sparkline is replaced by the per-core grid (renderCoreGrid).
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

// renderCoreGrid fills the CPU card's per-core strip from metrics.cpu_cores: a
// "busy N/M" count next to one thin bar per logical core (bar height = that
// core's busy %, highlighted when at/above the busy threshold). It answers
// "single- or multi-threaded?" -- a question the averaged cpu_percent hides. The
// element is left empty (CSS-hidden) when per-core data is unavailable.
function renderCoreGrid(cores) {
  const el = $("cpuCores");
  if (!el) { return; }
  el.textContent = "";
  if (!cores || !cores.available || !Array.isArray(cores.cores) || cores.cores.length === 0) {
    return;
  }
  const count = Number.isFinite(cores.count) ? cores.count : cores.cores.length;
  const busy = Number.isFinite(cores.busy) ? cores.busy : 0;
  const threshold = finiteNumber(cores.busy_threshold);

  const label = document.createElement("span");
  label.className = "core-grid-label";
  label.textContent = `busy ${busy}/${count}`;
  if (busy > 0) { label.classList.add("busy"); }
  el.append(label);

  const bars = document.createElement("span");
  bars.className = "core-grid-bars";
  for (const raw of cores.cores) {
    const bar = document.createElement("span");
    bar.className = "core-bar";
    const value = clamp(raw, 0, 100);
    bar.style.setProperty("--h", `${Math.max(8, Math.round(value))}%`);
    if (threshold !== null && value >= threshold) { bar.classList.add("busy"); }
    bars.append(bar);
  }
  el.append(bars);
}

// renderPrimaryCardDetails fills the small detail line under each gauge with the
// readings NOT already shown by the rings/center labels: CPU/GPU temperature and
// power draw, RAM headroom, and the host's Tailscale connectivity (online +
// exit-node state) under the NET card. The detail line is colour-coded by the
// temperature threshold for the CPU and GPU cards.
function renderPrimaryCardDetails(metrics, primaryGPU) {
  const tempWarn = thresholdValue("temp_warn_c");

  renderCardIdentities(metrics, primaryGPU);

  const cpuTemp = numberMetric(metrics.cpu_temperature);
  const cpuPower = numberMetric(metrics.cpu_power);
  // Both power figures matter -- the cores-only rail is what AMD Adrenalin and
  // Ryzen Master call "CPU power", while the package figure is what the socket
  // actually draws (~30 W more on a chiplet part, which includes the IO die and
  // misc rails). They render as a "core / package" pair rather than two labelled
  // segments so the whole detail line stays on one row of the CPU card. The pair
  // is AMD-only and collapses to the package figure alone elsewhere.
  const cpuCorePower = numberMetric(metrics.cpu_core_power);
  setCardDetail("cpuDetail", joinDetail([
    cpuTemp.available ? formatTemp(cpuTemp.value) : "",
    formatPowerPair(cpuPower, cpuCorePower),
  ]), cpuTemp, tempWarn);
  renderCoreGrid(metrics.cpu_cores);

  const gpuTemp = primaryGPU ? numberMetric(primaryGPU.temperature_celsius) : unavailable();
  const gpuPower = primaryGPU ? numberMetric(primaryGPU.power_watts) : unavailable();
  setCardDetail("gpuDetail", joinDetail([
    gpuTemp.available ? formatTemp(gpuTemp.value) : "",
    gpuPower.available ? formatPower(gpuPower.value) : "",
  ]), gpuTemp, tempWarn);

  const memory = capacityPercent(metrics.memory);
  const swap = capacityPercent(metrics.memory_swap);
  setCardDetail("memDetail", swap.available
    ? `⇅ ${formatBytes(swap.usedBytes)} swap`
    : "no swap", null, null);

  renderNetDetail(metrics.tailscale);
}

// renderApps drives the App details page: the Apps view (processes grouped by
// executable) and the Processes view (one row per PID), each with a client-side
// re-sort. It degrades to a graceful message when the platform cannot enumerate
// processes; unavailable per-field cells render as an em dash. No inner
// scrollbar -- the page grows with its rows and the pager re-measures the
// active page height afterward.
function renderApps(processes) {
  const body = $("processesBody");
  const summary = $("processesSummary");
  const panel = $("processesPanel");
  if (!body) {
    return;
  }
  body.textContent = "";
  if (!processes || !processes.available) {
    summary.textContent = processes?.error ? "unavailable" : "--";
    if (panel) {
      panel.classList.add("processes-unavailable");
    }
    body.append(processesMessage(processes?.error ? processes.error : "process metrics unavailable"));
    syncPagerAfterRender();
    return;
  }
  if (panel) {
    panel.classList.remove("processes-unavailable");
  }
  summary.textContent = `${processes.total || 0} process${processes.total === 1 ? "" : "es"}`;
  const rows = appsView === "processes" ? processes.processes || [] : processes.apps || [];
  if (rows.length === 0) {
    body.append(processesMessage("no processes reported"));
    syncPagerAfterRender();
    return;
  }
  const sorted = sortProcessRows(rows, procSort);
  for (const row of sorted) {
    body.append(processRow(row, appsView));
  }
  syncPagerAfterRender();
}

// setProcessesView toggles the Apps/Processes view and re-renders from the last
// metrics payload. Re-rendering rather than waiting for the next poll keeps the
// toggle snappy and lets the client-side sort apply immediately.
function setProcessesView(view) {
  if (view !== "apps" && view !== "processes") {
    return;
  }
  if (appsView === view) {
    return;
  }
  appsView = view;
  $("processesViewApps").classList.toggle("active", view === "apps");
  $("processesViewApps").setAttribute("aria-selected", view === "apps" ? "true" : "false");
  $("processesViewProcs").classList.toggle("active", view === "processes");
  $("processesViewProcs").setAttribute("aria-selected", view === "processes" ? "true" : "false");
  renderApps(lastProcessSet());
}

// renderStorage drives the per-drive storage panel on page 2: one row per
// physical device with model/name/size + temperature up top, a horizontal fill
// bar (coloured by the disk_warn threshold), and used/total + mountpoints below.
// A drive with no mounted filesystem (e.g. an unmounted NTFS drive) renders its
// temperature and a "\u2014" capacity rather than a bogus 0%. Hidden when the
// whole set is unavailable, mirroring the GPU/process panels.
function renderStorage(storage) {
  const panel = $("storagePanel");
  const list = $("storageList");
  const summary = $("storageSummary");
  if (!panel || !list || !summary) {
    return;
  }
  list.textContent = "";
  if (!storage || !storage.available || !Array.isArray(storage.devices) || storage.devices.length === 0) {
    panel.hidden = true;
    summary.textContent = "--";
    syncPagerAfterRender();
    return;
  }
  panel.hidden = false;
  summary.textContent = `${storage.devices.length} drive${storage.devices.length === 1 ? "" : "s"}`;
  const diskWarn = thresholdValue("disk_warn");
  const tempWarn = thresholdValue("temp_warn_c");
  for (const device of storage.devices) {
    list.append(storageRow(device, diskWarn, tempWarn));
  }
  syncPagerAfterRender();
}

function storageRow(device, diskWarn, tempWarn) {
  const row = document.createElement("div");
  row.className = "storage-row";

  const head = document.createElement("div");
  head.className = "storage-row-head";
  const model = document.createElement("span");
  model.className = "storage-model";
  model.textContent = storageDeviceLabel(device);
  const temp = numberMetric(device.temperature_celsius);
  const tempEl = document.createElement("span");
  tempEl.className = "storage-temp";
  tempEl.textContent = temp.available ? formatTemp(temp.value) : "--";
  if (temp.available) {
    tempEl.style.color = colorFor(temp.value, tempWarn);
  }
  head.append(model, tempEl);
  row.append(head);

  const cap = capacityPercent(device.capacity);
  const bar = document.createElement("div");
  bar.className = "storage-bar";
  const fill = document.createElement("span");
  fill.className = "storage-bar-fill";
  if (cap.available) {
    fill.style.setProperty("--w", `${clamp(cap.value, 0, 100)}%`);
    fill.style.setProperty("--c", colorFor(cap.value, diskWarn));
  } else {
    fill.classList.add("unavailable");
  }
  bar.append(fill);
  row.append(bar);

  const foot = document.createElement("div");
  foot.className = "storage-row-foot";
  const left = document.createElement("span");
  left.className = "storage-mounts";
  left.textContent = storageFootLeft(device, cap);
  const capText = document.createElement("span");
  capText.className = "storage-cap";
  capText.textContent = cap.available
    ? `${formatBytes(cap.usedBytes)} / ${formatBytes(cap.totalBytes)}`
    : "\u2014";
  foot.append(left, capText);
  row.append(foot);

  return row;
}

// storageDeviceLabel renders the row's identity line: model + short name +
// physical size (e.g. "Samsung 990 PRO 2TB \u00b7 nvme1n1 \u00b7 2.0 TB"),
// falling back to "drive" when nothing is reported.
function storageDeviceLabel(device) {
  const parts = [];
  const model = String(device?.model || "").trim();
  const name = String(device?.name || "").trim();
  if (model) {
    parts.push(model);
  }
  if (name) {
    parts.push(name);
  }
  const sizeBytes = safeIntegerNumber(device?.size_bytes);
  if (sizeBytes !== null && sizeBytes > 0) {
    parts.push(formatBytes(sizeBytes));
  }
  return parts.length > 0 ? parts.join(" \u00b7 ") : "drive";
}

// storageFootLeft renders the bar's caption: the device's mountpoints when it
// has mounted filesystems, otherwise the capacity error ("no mounted
// filesystems") so an unmounted drive explains itself rather than looking
// broken.
function storageFootLeft(device, cap) {
  const mounts = Array.isArray(device?.mountpoints) ? device.mountpoints.filter((m) => m) : [];
  if (mounts.length > 0) {
    return mounts.join(" \u00b7 ");
  }
  if (!cap.available) {
    return cap.error || "not mounted";
  }
  return "";
}

// setProcSort re-sorts the process rows by a column. Tapping the active column
// flips the direction; tapping a new column defaults to descending (ascending
// for the name column).
function setProcSort(col) {
  if (!procColumnKey(col)) {
    return;
  }
  if (procSort.col === col) {
    procSort = { col, dir: procSort.dir === "asc" ? "desc" : "asc" };
  } else {
    procSort = { col, dir: col === "name" ? "asc" : "desc" };
  }
  reflectProcSort();
  renderApps(lastProcessSet());
}

// lastProcessSet returns the processes block from the most recent metrics
// render so view/sort changes can re-render without a fresh fetch. The render
// path stashes it in state.lastProcesses.
function lastProcessSet() {
  return state.lastProcesses || { available: false };
}

// reflectProcSort marks the active sort column + direction on the header buttons
// so the CSS can show an indicator. Tolerates a missing header (layout-less
// verifier) by no-oping.
function reflectProcSort() {
  for (const button of document.querySelectorAll(".processes-sort")) {
    const active = button.dataset.procSort === procSort.col;
    button.classList.toggle("sorted", active);
    button.dataset.procDir = active ? procSort.dir : "";
  }
}

function sortProcessRows(rows, sort) {
  const key = procColumnKey(sort.col) || "cpu";
  const dir = sort.dir === "asc" ? 1 : -1;
  return [...rows].sort((a, b) => {
    // Name sorts lexicographically, with PID as a stable tiebreaker. Handled up
    // front because procRowValue collapses names to a constant, so routing the
    // name column through the numeric path below would only ever hit the
    // tiebreaker (sorting by PID, not name).
    if (key === "name") {
      const nameCmp = String(a.name || "").localeCompare(String(b.name || ""));
      return (nameCmp || (a.pid || 0) - (b.pid || 0)) * dir;
    }
    const av = procRowValue(a, key);
    const bv = procRowValue(b, key);
    if (av === bv) {
      // Stable tiebreaker: name, then PID (processes view only).
      const nameCmp = String(a.name || "").localeCompare(String(b.name || ""));
      if (nameCmp !== 0) {
        return nameCmp;
      }
      return ((a.pid || 0) - (b.pid || 0)) * dir;
    }
    return (av - bv) * dir;
  });
}

function procColumnKey(col) {
  switch (col) {
    case "name":
    case "cpu":
    case "memory":
    case "gpu":
    case "disk":
      return col;
    default:
      return null;
  }
}

// procRowValue extracts a comparable numeric magnitude for one row + column.
// Unavailable fields sort as -1 so they sink below available values in desc
// order (and rise above in asc), and the disk column sums read+write.
function procRowValue(row, key) {
  switch (key) {
    case "name":
      return 0;
    case "cpu":
      return procMagnitude(row.cpu_percent);
    case "memory":
      return procMagnitude(row.memory_bytes);
    case "gpu":
      return procMagnitude(row.gpu_memory);
    case "disk":
      return procMagnitude(row.disk_read) + procMagnitude(row.disk_write);
    default:
      return -1;
  }
}

function procMagnitude(metric) {
  const value = numberMetric(metric);
  return value.available ? value.value : -1;
}

// processRow builds one row element for the Apps or Processes view. The leading
// cell carries the name (plus a per-app PID count or a per-process PID), and the
// trailing cells carry CPU/RAM/GPU/Disk with per-cell heat colour.
function processRow(row, view) {
  const el = document.createElement("div");
  el.className = "processes-row";
  el.setAttribute("role", "row");

  const name = document.createElement("div");
  name.className = "processes-cell processes-name";
  name.textContent = view === "apps" ? procAppLabel(row) : procProcLabel(row);
  el.append(name);
  el.append(procCell(row.cpu_percent, formatProcPercent, thresholdValue("cpu_warn"), "cpu"));
  el.append(procCell(row.memory_bytes, (v) => formatBytes(v), null, "memory"));
  el.append(procCell(row.gpu_memory, (v) => formatBytes(v), null, "gpu"));
  const diskRead = numberMetric(row.disk_read);
  const diskWrite = numberMetric(row.disk_write);
  const diskValue = (diskRead.available ? diskRead.value : 0) + (diskWrite.available ? diskWrite.value : 0);
  el.append(procCell({
    available: diskRead.available || diskWrite.available,
    value: diskValue,
    unit: "B/s",
  }, formatRateCompact, null, "disk"));
  return el;
}

function procAppLabel(row) {
  const name = nonEmptyText(row.name) || "unknown";
  if (row.count && row.count > 1) {
    return `${name} · ${row.count}`;
  }
  return name;
}

function procProcLabel(row) {
  const name = nonEmptyText(row.name) || `pid ${row.pid}`;
  return `${name} · ${row.pid}`;
}

// procCell builds one numeric cell. Available values are formatted + heat-
// coloured against an optional warn threshold (CPU only); unavailable cells
// render an em dash with the muted colour so the row never fails on one sensor.
function procCell(metric, format, warnThreshold, column) {
  const cell = document.createElement("div");
  cell.className = `processes-cell processes-value processes-${column}`;
  const value = numberMetric(metric);
  if (!value.available) {
    cell.textContent = "—";
    cell.classList.add("unavailable");
    return cell;
  }
  cell.textContent = format(value.value);
  if (warnThreshold !== null) {
    const pct = column === "cpu" ? value.value : null;
    if (pct !== null) {
      cell.style.setProperty("--c", colorFor(pct, warnThreshold));
    }
  }
  return cell;
}

function formatProcPercent(value) {
  return `${Math.round(value)}%`;
}

function processesMessage(text) {
  const el = document.createElement("div");
  el.className = "processes-message";
  el.textContent = text;
  return el;
}

// renderCardIdentities fills each card's identity line: CPU model, GPU model,
// RAM type+speed (falling back to total size when the type/speed isn't readable,
// e.g. a non-root user-session service), and the active network (Wi-Fi SSID or
// wired link). Missing values leave the line blank; its reserved height keeps all
// four cards aligned.
function renderCardIdentities(metrics, primaryGPU) {
  setCardId("cpuName", metrics.cpu_name);
  setCardId("gpuName", primaryGPU ? primaryGPU.name : "");

  const memory = capacityPercent(metrics.memory);
  const memName = nonEmptyText(metrics.memory_name)
    || (memory.available ? formatBytes(memory.totalBytes) : "");
  setCardId("memName", memName);

  const uplink = metrics.network && metrics.network.uplink;
  setCardId("netName", uplinkDisplay(uplink));
}

// uplinkDisplay renders the active network identity with a leading glyph for the
// link kind -- signal bars for Wi-Fi, a wired-network plug for Ethernet -- so the
// NET card shows at a glance how the host is connected. The name is the SSID
// (Wi-Fi) or "Ethernet [+ link speed]" (wired); a degraded/absent uplink leaves
// the line blank. Glyph-only fallbacks cover a known kind with no resolved name.
function uplinkDisplay(uplink) {
  if (!uplink || !uplink.available) {
    return "";
  }
  const name = nonEmptyText(uplink.name);
  if (uplink.kind === "wifi") {
    return name ? `📶 ${name}` : "📶 Wi-Fi";
  }
  if (uplink.kind === "ethernet") {
    return name ? `🖧 ${name}` : "🖧 Ethernet";
  }
  return name;
}

function setCardId(id, text) {
  const el = $(id);
  if (!el) { return; }
  el.textContent = nonEmptyText(text);
}

function nonEmptyText(value) {
  return typeof value === "string" ? value.trim() : "";
}

// renderNetDetail fills the NET card's detail line with a "Tailscale" label
// followed by two connectivity icons -- a mesh icon (online/offline) and an
// exit-node icon (on/off). The icons carry no text of their own; their state is
// the colour plus an aria-label, and the leading label names what they describe.
function renderNetDetail(tailscale) {
  const el = $("netDetail");
  if (!el) { return; }
  el.classList.remove("warn", "crit");
  el.textContent = "";
  if (!tailscale || !tailscale.available) {
    el.textContent = "--";
    return;
  }
  const label = document.createElement("span");
  label.className = "ts-label";
  label.textContent = "Tailscale";
  el.append(
    label,
    statusPill(meshIcon(), tailscale.online ? "Tailscale online" : "Tailscale offline", tailscale.online ? "on" : "bad"),
    statusPill(exitNodeIcon(), tailscale.exit_node_enabled ? "Exit node on" : "Exit node off", tailscale.exit_node_enabled ? "on" : "dim"),
  );
}

// statusPill wraps a status icon in a colour-coded span. The icon inherits the
// pill's colour via currentColor; the visible state is the colour, and the
// spoken state is the aria-label/title (there is no visible text).
function statusPill(icon, label, state) {
  const span = document.createElement("span");
  span.className = `ts-pill ts-${state}`;
  span.setAttribute("role", "img");
  span.setAttribute("aria-label", label);
  span.title = label;
  span.append(icon);
  return span;
}

const SVG_ICON_NS = "http://www.w3.org/2000/svg";

// makeIcon builds an inline 24x24 SVG line-art icon (stroke = currentColor,
// fill = none) entirely through the DOM so it is XSS-safe and inherits the
// pill's status colour. children is a list of [tag, attrs] pairs.
function makeIcon(children) {
  const svg = document.createElementNS(SVG_ICON_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("width", "14");
  svg.setAttribute("height", "14");
  svg.setAttribute("fill", "none");
  svg.setAttribute("stroke", "currentColor");
  svg.setAttribute("stroke-width", "1.8");
  svg.setAttribute("stroke-linecap", "round");
  svg.setAttribute("stroke-linejoin", "round");
  svg.setAttribute("aria-hidden", "true");
  for (const [tag, attrs] of children) {
    const node = document.createElementNS(SVG_ICON_NS, tag);
    for (const [name, value] of Object.entries(attrs)) {
      node.setAttribute(name, value);
    }
    svg.append(node);
  }
  return svg;
}

// meshIcon -- three nodes joined in a triangle (a network mesh): the mark for
// the Tailscale connectivity pill.
function meshIcon() {
  return makeIcon([
    ["path", { d: "M12 7 L6.5 17.5 L17.5 17.5 Z" }],
    ["circle", { cx: 12, cy: 7, r: 2.3, fill: "currentColor", stroke: "none" }],
    ["circle", { cx: 6.5, cy: 17.5, r: 2.3, fill: "currentColor", stroke: "none" }],
    ["circle", { cx: 17.5, cy: 17.5, r: 2.3, fill: "currentColor", stroke: "none" }],
  ]);
}

// exitNodeIcon -- a doorway with an arrow leaving through it (egress): the mark
// for the Tailscale exit-node pill.
function exitNodeIcon() {
  return makeIcon([
    ["path", { d: "M10 21H6a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4" }],
    ["polyline", { points: "16 17 21 12 16 7" }],
    ["line", { x1: 21, y1: 12, x2: 9, y2: 12 }],
  ]);
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
  // Lock Screen is destructive enough to require an arm-then-confirm: the first
  // tap arms the button (label flips to "Confirm?"), and only a second tap
  // within controlArmWindowMS POSTs. Any other control button disarms it so a
  // stray arm never lingers. applyControlCapabilities rewrites .disabled on
  // every status poll, so the armed state lives in a CSS class, not disabled.
  if (action === "lock_screen" && state.controlArmedAction !== "lock_screen") {
    disarmControl();
    armLockControl(button);
    return;
  }
  disarmControl();
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

// armLockControl arms the Lock Screen button for the confirm window: it flips
// the label to "Confirm?", adds the armed CSS class, hints via the status strip,
// and starts the auto-disarm timer. The second tap (within the window) is what
// actually POSTs /api/control; the first tap only arms.
function armLockControl(button) {
  state.controlArmedAction = "lock_screen";
  button.classList.add("armed");
  const labelEl = $("lockCtlLabel");
  if (labelEl) {
    labelEl.textContent = "Confirm?";
  }
  showTransientStatus("Tap again to lock");
  clearDashboardTimeout("controlArmTimer");
  state.controlArmTimer = setTimeout(disarmControl, controlArmWindowMS);
}

// disarmControl clears any armed Lock Screen state: removes the armed class,
// restores the label, and cancels the timer. Safe to call when not armed.
function disarmControl() {
  const wasArmed = state.controlArmedAction;
  state.controlArmedAction = null;
  clearDashboardTimeout("controlArmTimer");
  if (wasArmed !== "lock_screen") {
    return;
  }
  const button = controlButtonIDs.lock_screen ? $(controlButtonIDs.lock_screen) : null;
  if (button) {
    button.classList.remove("armed");
  }
  const labelEl = $("lockCtlLabel");
  if (labelEl) {
    labelEl.textContent = "Lock";
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

// formatTemp renders a Celsius reading as a rounded "NN°C" string, matching the
// CPU/GPU card detail lines and temperature alert messages.
function formatTemp(celsius) {
  const value = finiteNumber(celsius);
  if (value === null) {
    return "--°C";
  }
  return `${Math.round(value)}°C`;
}

// formatPowerPair renders the CPU detail line's power segment. With both rails
// it is a compact "48 / 84 W" -- cores-only against whole-socket package power,
// the same part/whole idiom the GPU and RAM gauge subs use for used/total. Both
// sides are rounded to whole watts: formatPower's sub-100 W decimal buys no real
// precision on a rail that moves several watts between samples, and this segment
// shares one line with the temperature. Falls back to the single package figure
// (with its usual formatting) when the cores-only rail is unavailable, which is
// the permanent state on Intel and Linux.
function formatPowerPair(packageMetric, coreMetric) {
  if (!packageMetric || !packageMetric.available) {
    return "";
  }
  if (!coreMetric || !coreMetric.available) {
    return formatPower(packageMetric.value);
  }
  const core = finiteNumber(coreMetric.value);
  const total = finiteNumber(packageMetric.value);
  if (core === null || total === null || core < 0 || total < 0) {
    return formatPower(packageMetric.value);
  }
  // Unit is appended directly rather than via formatPower, which would re-apply
  // its sub-100 W decimal and undo the rounding ("48 / 84.0 W").
  return `${Math.round(core)} / ${Math.round(total)} W`;
}

// formatClockPair renders the CPU gauge sub as "4.4 / 5.4 GHz" -- the live clock
// against the boost ceiling, which is what the gauge's inner ring is filled
// against, so the sub now explains the ring instead of just repeating one end of
// it. One decimal per side (formatClock uses two) keeps the pair inside the gauge
// circle. Falls back to the single live value when the boost ceiling is missing
// or reads below the live clock -- a max under the current clock is a bad
// reading, and rendering "5.5 / 5.4" would look broken.
function formatClockPair(currentMhz, maxMhz) {
  const current = finiteNumber(currentMhz);
  const max = finiteNumber(maxMhz);
  if (current === null || current <= 0) {
    return null;
  }
  if (max === null || max <= 0 || max < current || current < 1000) {
    return formatClock(current);
  }
  return `${(current / 1000).toFixed(1)} / ${(max / 1000).toFixed(1)} GHz`;
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
// the absolute used/total budget. Pass omitUnit for the left side of a pair
// (see formatGibPair), which shares the trailing unit.
function formatGib(bytes, omitUnit) {
  const value = finiteNumber(bytes);
  if (value === null || value < 0) {
    return null;
  }
  const gib = value / (1024 ** 3);
  const text = gib >= 100 ? gib.toFixed(0) : gib.toFixed(1);
  return omitUnit ? text : `${text} GB`;
}

// formatGibPair renders a VRAM used/total pair as "2.0 / 8.0 GB". formatGib never
// switches unit, so the used side can drop its own "GB" and let the trailing one
// cover both -- matching the CPU gauge's "4.6 / 5.4 GHz" and keeping the sub
// inside the gauge circle. Deliberately not reused for the RAM gauge: that one
// formats through formatBytes, which picks a unit per value and so can
// legitimately pair a MB used with a GB total, where a single trailing unit would
// be wrong.
function formatGibPair(usedBytes, totalBytes) {
  const used = formatGib(usedBytes, true);
  const total = formatGib(totalBytes);
  if (used === null || total === null) {
    return null;
  }
  return `${used} / ${total}`;
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
