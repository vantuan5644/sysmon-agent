#!/usr/bin/env node
import { readFileSync } from "node:fs";
import vm from "node:vm";

class ClassList {
  constructor(element) {
    this.element = element;
    this.values = new Set();
  }

  add(name) {
    this.values.add(name);
    this.sync();
  }

  remove(name) {
    this.values.delete(name);
    this.sync();
  }

  contains(name) {
    return this.values.has(name);
  }

  toggle(name, force) {
    const shouldAdd = force === undefined ? !this.values.has(name) : Boolean(force);
    if (shouldAdd) {
      this.values.add(name);
    } else {
      this.values.delete(name);
    }
    this.sync();
    return shouldAdd;
  }

  sync() {
    this.element._className = [...this.values].join(" ");
  }
}

class Element {
  constructor(tagName = "div") {
    this.tagName = tagName.toUpperCase();
    this.children = [];
    this.dataset = {};
    this.listeners = {};
    this.attributes = {};
    this.parentNode = null;
    this.hidden = false;
    this.style = {
      values: new Map(),
      setProperty: (name, value) => {
        this.style.values.set(name, String(value));
      },
    };
    this.classList = new ClassList(this);
    this._textContent = "";
    this._className = "";
  }

  get className() {
    return this._className;
  }

  set className(value) {
    this._className = String(value || "");
    this.classList.values = new Set(this._className.split(/\s+/).filter(Boolean));
  }

  get textContent() {
    if (this.children.length === 0) {
      return this._textContent;
    }
    // Real browsers concatenate every descendant's text; mirror that so
    // detail lines built from child spans (e.g. the NET Tailscale pills)
    // read back as a single string.
    let text = this._textContent;
    for (const child of this.children) {
      text += child && typeof child === "object" ? (child.textContent ?? "") : String(child);
    }
    return text;
  }

  set textContent(value) {
    this._textContent = String(value);
    this.children = [];
  }

  addEventListener(name, callback) {
    this.listeners[name] ??= [];
    this.listeners[name].push(callback);
  }

  setAttribute(name, value) {
    this.attributes[name] = String(value);
  }

  getAttribute(name) {
    return this.attributes[name] || null;
  }

  append(...children) {
    for (const child of children) {
      if (child && typeof child === "object") {
        child.parentNode = this;
      }
      this.children.push(child);
    }
  }

  async click() {
    const callbacks = this.listeners.click || [];
    await Promise.all(callbacks.map((callback) => callback({ target: this })));
  }

  async dispatch(name, event = {}) {
    const callbacks = this.listeners[name] || [];
    const payload = {
      target: this,
      preventDefault: () => {
        payload.defaultPrevented = true;
      },
      defaultPrevented: false,
      ...event,
    };
    await Promise.all(callbacks.map((callback) => callback(payload)));
    return payload;
  }
}

class Document {
  constructor(html) {
    this.listeners = {};
    this.body = new Element("body");
    this.elements = new Map();
    this.visibilityState = "visible";
    this.intervalButtons = extractDataElements(html, "interval");
    this.panelButtons = extractDataElements(html, "panel");
    this.panelJumpTargets = extractDataElements(html, "panel-jump");

    for (const id of html.matchAll(/\bid="([^"]+)"/g)) {
      this.elements.set(id[1], new Element());
    }
  }

  addEventListener(name, callback) {
    this.listeners[name] ??= [];
    this.listeners[name].push(callback);
  }

  createElement(tagName) {
    return new Element(tagName);
  }

  createElementNS(_ns, tagName) {
    return new Element(tagName);
  }

  getElementById(id) {
    const element = this.elements.get(id);
    if (!element) {
      throw new Error(`missing fixture element #${id}`);
    }
    return element;
  }

  querySelectorAll(selector) {
    if (selector === "[data-interval]") {
      return this.intervalButtons;
    }
    if (selector === "[data-panel]") {
      return this.panelButtons;
    }
    if (selector === "[data-panel-jump]") {
      return this.panelJumpTargets;
    }
    return [];
  }

  dispatch(name) {
    for (const callback of this.listeners[name] || []) {
      callback();
    }
  }
}

function extractDataElements(html, name) {
  const datasetKey = name.replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());
  return [...html.matchAll(new RegExp(`<[^>]+data-${name}="([^"]+)"[^>]*>`, "g"))].map((match) => {
    const element = new Element();
    element.dataset[datasetKey] = match[1];
    return element;
  });
}

function response(body, status = 200) {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
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
    cpu_power: { available: true, value: 88.5, unit: "W" },
    psu_output_power: { available: true, value: 312.5, unit: "W" },
    cpu_clock: { available: true, value: 3600, unit: "MHz" },
    cpu_clock_max: { available: true, value: 5200, unit: "MHz" },
    cpu_temperature: { available: true, value: 52.0, unit: "C" },
    cpu_cores: { available: true, cores: [95, 12, 88, 4, 70, 3, 2, 1], count: 8, busy: 2, busy_threshold: 80 },
    memory: { available: true, used_bytes: 8589934592, total_bytes: 17179869184, percent: 50 },
    memory_swap: { available: true, used_bytes: 2147483648, total_bytes: 8589934592, percent: 25 },
    disks: [{
      name: "root",
      mountpoint: "/",
      fs_type: "ext4",
      capacity: { available: true, used_bytes: 500, total_bytes: 1000, percent: 50 },
    }],
    network: {
      available: true,
      uplink: { available: true, kind: "wifi", name: "BiBi-Pro-Max" },
      interfaces: [{
        name: "eth0",
        rx_bytes_total: 100000,
        tx_bytes_total: 50000,
        rx_bytes_per_second: { available: true, value: 2048, unit: "B/s" },
        tx_bytes_per_second: { available: true, value: 1024, unit: "B/s" },
      }],
    },
    tailscale: { available: true, online: true, exit_node_enabled: false },
    temperatures: {
      available: true,
      sensors: [
        { name: "CPU", celsius: { available: true, value: 52, unit: "C" } },
        { name: "Chipset", celsius: { available: true, value: 68, unit: "C" } },
        { name: "Ambient", celsius: { available: true, value: 31, unit: "C" } },
        { name: "Mystery", celsius: { available: false, error: "sensor unavailable" } },
      ],
    },
    gpu: {
      available: true,
      devices: [{
        name: "GPU 0",
        usage_percent: { available: true, value: 25, unit: "%" },
        power_watts: { available: true, value: 112.0, unit: "W" },
        memory: { available: true, used_bytes: 2147483648, total_bytes: 8589934592, percent: 25 },
        temperature_celsius: { available: true, value: 49, unit: "C" },
      }],
    },
  };
}

function degradedMetrics() {
  const metrics = sampleMetrics();
  metrics.cpu_temperature = { available: false, error: "no CPU temperature sensor reported" };
  metrics.disks = [{
    name: "backup",
    mountpoint: "/backup",
    fs_type: "ext4",
    capacity: { available: false, error: "statfs denied" },
  }];
  metrics.network = { available: false, error: "no non-loopback network interfaces found" };
  metrics.temperatures = { available: false, error: "no supported temperature sensors found" };
  metrics.gpu = { available: false, error: "nvidia-smi not found" };
  metrics.collection_errors = [
    "disk /backup: statfs denied",
    "network: no non-loopback network interfaces found",
    "temperatures: no supported temperature sensors found",
    "gpu: nvidia-smi not found",
  ];
  return metrics;
}

function manyIssueMetrics() {
  const metrics = degradedMetrics();
  metrics.collection_errors = [
    "disk /backup: statfs denied",
    "network: no non-loopback network interfaces found",
    "temperatures: no supported temperature sensors found",
    "gpu: nvidia-smi not found",
    "gpu Intel usage: DRM GPU usage not exposed",
    "gpu Intel memory: DRM VRAM usage not exposed",
    "gpu Intel temperature: no hwmon temperature exposed",
  ];
  return metrics;
}

function alertMetrics() {
  const metrics = sampleMetrics();
  // Primary-gauge readings are pushed high on purpose: CPU/RAM/GPU usage and
  // the CPU/GPU die temperatures already turn their own rings/detail lines red,
  // so they must NOT also flood the alerts panel. Only disk usage and auxiliary
  // temperature sensors (board, water, PSU) -- none of which have a card --
  // should surface as alerts here.
  metrics.cpu_percent = { available: true, value: 86, unit: "%" };
  metrics.cpu_temperature = { available: true, value: 82, unit: "C" };
  metrics.memory = { available: true, used_bytes: 80, total_bytes: 100, percent: 80 };
  metrics.gpu.devices[0].usage_percent = { available: true, value: 92, unit: "%" };
  metrics.gpu.devices[0].memory = { available: true, used_bytes: 90, total_bytes: 100, percent: 90 };
  metrics.gpu.devices[0].temperature_celsius = { available: true, value: 82, unit: "C" };
  metrics.disks = [
    {
      name: "root",
      mountpoint: "/",
      fs_type: "ext4",
      capacity: { available: true, used_bytes: 92, total_bytes: 100, percent: 92 },
    },
    {
      name: "backup",
      mountpoint: "/backup",
      fs_type: "ext4",
      capacity: { available: true, used_bytes: 91, total_bytes: 100, percent: 91 },
    },
    {
      name: "media",
      mountpoint: "/media",
      fs_type: "ext4",
      capacity: { available: true, used_bytes: 90, total_bytes: 100, percent: 90 },
    },
    {
      name: "photos",
      mountpoint: "/photos",
      fs_type: "ext4",
      capacity: { available: true, used_bytes: 89, total_bytes: 100, percent: 89 },
    },
  ];
  metrics.temperatures.sensors = [
    { name: "CPU Package", celsius: { available: true, value: 82, unit: "C" } },
    { name: "GPU Core", celsius: { available: true, value: 82, unit: "C" } },
    { name: "Motherboard", celsius: { available: true, value: 75, unit: "C" } },
    { name: "Water In", celsius: { available: true, value: 74, unit: "C" } },
    { name: "PSU", celsius: { available: true, value: 73, unit: "C" } },
  ];
  return metrics;
}

function unavailableWarmingNetworkMetrics() {
  const metrics = sampleMetrics();
  metrics.network = { available: false, error: "network sampler is warming up" };
  return metrics;
}

function warmingNetworkMetrics() {
  const metrics = sampleMetrics();
  metrics.network = {
    available: true,
    interfaces: [{
      name: "wlan0",
      rx_bytes_total: 100,
      tx_bytes_total: 200,
      rx_bytes_per_second: { available: false, unit: "B/s", error: "interface is warming up" },
      tx_bytes_per_second: { available: false, unit: "B/s", error: "interface is warming up" },
    }],
  };
  return metrics;
}

function resetNetworkMetrics() {
  const metrics = sampleMetrics();
  metrics.network = {
    available: true,
    interfaces: [{
      name: "eth0",
      rx_bytes_total: 400,
      tx_bytes_total: 900,
      rx_bytes_per_second: { available: false, unit: "B/s", error: "network counter reset" },
      tx_bytes_per_second: { available: false, unit: "B/s", error: "network counter reset" },
    }],
  };
  return metrics;
}

function malformedNumericMetrics() {
  const metrics = sampleMetrics();
  metrics.cpu_percent = { available: true, value: Number.NaN, unit: "%" };
  metrics.cpu_clock = { available: true, value: Number.NaN, unit: "MHz" };
  metrics.cpu_temperature = { available: true, value: Number.NaN, unit: "C" };
  metrics.memory = { available: true, used_bytes: "bad", total_bytes: Number.POSITIVE_INFINITY, percent: Number.POSITIVE_INFINITY };
  metrics.disks = [{
    name: "root",
    mountpoint: "/",
    fs_type: "ext4",
    capacity: { available: true, used_bytes: Number.NaN, total_bytes: "bad", percent: "not-a-number" },
  }];
  metrics.network = {
    available: true,
    interfaces: [{
      name: "eth0",
      rx_bytes_total: 1,
      tx_bytes_total: 2,
      rx_bytes_per_second: { available: true, value: "bad", unit: "B/s" },
      tx_bytes_per_second: { available: true, value: Number.POSITIVE_INFINITY, unit: "B/s" },
    }],
  };
  metrics.temperatures = {
    available: true,
    sensors: [{ name: "CPU", celsius: { available: true, value: Number.NaN, unit: "C" } }],
  };
  metrics.gpu = {
    available: true,
    devices: [{
      name: "GPU 0",
      usage_percent: { available: true, value: Number.NaN, unit: "%" },
      memory: { available: true, used_bytes: 100, total_bytes: 400, percent: "bad" },
      temperature_celsius: { available: true, value: Number.POSITIVE_INFINITY, unit: "C" },
    }],
  };
  return metrics;
}

function malformedCapacityCountersMetrics() {
  const metrics = sampleMetrics();
  metrics.memory = { available: true, used_bytes: "bad", total_bytes: 17179869184, percent: 50 };
  metrics.disks = [{
    name: "root",
    mountpoint: "/",
    fs_type: "ext4",
    capacity: { available: true, used_bytes: 900, total_bytes: 100, percent: 90 },
  }];
  metrics.gpu.devices[0].memory = { available: true, used_bytes: 100, total_bytes: 0, percent: 25 };
  return metrics;
}

function unsafeCapacityCountersMetrics() {
  const metrics = sampleMetrics();
  const unsafeTotal = Number.MAX_SAFE_INTEGER + 1;
  metrics.memory = { available: true, used_bytes: 100, total_bytes: unsafeTotal, percent: 1 };
  metrics.disks = [{
    name: "huge",
    mountpoint: "/huge",
    fs_type: "ext4",
    capacity: { available: true, used_bytes: 100, total_bytes: unsafeTotal, percent: 1 },
  }];
  metrics.gpu.devices[0].memory = { available: true, used_bytes: 100, total_bytes: unsafeTotal, percent: 1 };
  return metrics;
}

function partialGPUFallbackMetrics() {
  const metrics = sampleMetrics();
  metrics.gpu = {
    available: true,
    devices: [{
      name: "Integrated GPU",
      usage_percent: { available: false, unit: "%", error: "GPU usage requires vendor tools" },
      memory: { available: false, error: "GPU memory usage unavailable" },
      temperature_celsius: { available: false, unit: "C", error: "GPU temperature requires vendor tools" },
    }],
  };
  return metrics;
}

function sampleStatus() {
  return {
    status: "ok",
    dashboard_build: "sysmon-static-v111",
    started_at: new Date(Date.now() - 3720 * 1000).toISOString(),
    uptime_seconds: 3720,
    os: "linux",
    arch: "amd64",
    settings_persisted: true,
    refresh_options_ms: [250, 500, 1000, 2000],
    panel_options: ["all", "gpu", "network", "performance", "sensors", "storage"],
    settings,
    client_check: { seen: false, standalone: false },
    device_client_check: { seen: false, standalone: false },
  };
}

function sampleObservedStatus(clientCheck = {}) {
  const check = {
    seen: true,
    last_seen: new Date(fakeNow - 12_000).toISOString(),
    dashboard_build: "sysmon-static-v111",
    user_agent: "Mozilla/5.0 iPhone Mobile Safari",
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
    ...clientCheck,
  };
  return {
    ...sampleStatus(),
    client_check: check,
    device_client_check: check,
  };
}

const defaultThresholds = {
  cpu_warn: 70,
  memory_warn: 70,
  disk_warn: 70,
  gpu_warn: 70,
  temp_warn_c: 70,
};

function defaultSettings() {
  return {
    dim: false,
    shift: false,
    refresh_ms: 2000,
    panel: "gpu",
    thresholds: { ...defaultThresholds },
  };
}

function mergeDashboardSettings(current, update) {
  const next = { ...current, ...update };
  if (update.thresholds) {
    next.thresholds = { ...(current.thresholds || defaultThresholds), ...update.thresholds };
  }
  return next;
}

function assert(condition, message) {
  if (!condition) {
    throw new Error(message);
  }
}

async function flushMicrotasks() {
  for (let i = 0; i < 8; i += 1) {
    await Promise.resolve();
  }
}

function rowTitle(listId, index) {
  return document.getElementById(listId).children[index].children[0].children[0].textContent;
}

function rowSubtitle(listId, index) {
  return document.getElementById(listId).children[index].children[0].children[1]?.textContent || "";
}

function rowValue(listId, index) {
  return document.getElementById(listId).children[index].children[1].textContent;
}

function rowBar(listId, index) {
  return document.getElementById(listId).children[index].children[2];
}

function sparklineBars(id) {
  return document.getElementById(id).children;
}

function lastSparklineBar(id) {
  const bars = sparklineBars(id);
  return bars[bars.length - 1];
}

function deferredResponse(body) {
  let resolve;
  const promise = new Promise((done) => {
    resolve = () => done(response(body));
  });
  return { promise, resolve };
}

const html = readFileSync(new URL("./static/index.html", import.meta.url), "utf8");
const app = readFileSync(new URL("./static/app.js", import.meta.url), "utf8");
const document = new Document(html);
let settings = defaultSettings();
let fakeNow = 1000;
let metricsRequests = 0;
let statusRequests = 0;
let clientCheckRequests = 0;
let beaconRequests = 0;
let lastClientCheck = null;
let lastBeaconPath = "";
let lastBeaconBody = null;
let reloadCount = 0;
let unregisterCount = 0;
const deletedCacheKeys = [];
let nextTimeoutId = 1;
let nextIntervalId = 1;
const timeoutCallbacks = new Map();
const intervalCallbacks = new Map();
const windowListeners = new Map();
const localStorageValues = new Map();

function runTimeouts() {
	const callbacks = [...timeoutCallbacks.values()];
	timeoutCallbacks.clear();
	for (const callback of callbacks) {
		callback();
	}
}

function runIntervalsForDelay(delay) {
	for (const entry of intervalCallbacks.values()) {
		if (entry.delay === delay) {
			entry.callback();
		}
	}
}

function intervalCountForDelay(delay) {
	let count = 0;
	for (const entry of intervalCallbacks.values()) {
		if (entry.delay === delay) {
			count += 1;
		}
	}
	return count;
}

const context = {
	AbortController,
	Blob,
	console,
	document,
	  navigator: {
	    serviceWorker: {
	      addEventListener: () => {},
	      getRegistrations: async () => [{
	        unregister: async () => {
	          unregisterCount += 1;
	          return true;
	        },
	      }],
	      register: async () => ({}),
	    },
	    maxTouchPoints: 5,
	    sendBeacon: (path, body) => {
	      beaconRequests += 1;
	      lastBeaconPath = path;
	      lastBeaconBody = body;
	      return true;
	    },
	  },
  localStorage: {
    getItem: (key) => localStorageValues.get(key) ?? null,
    setItem: (key, value) => {
      localStorageValues.set(key, String(value));
    },
  },
  caches: {
    keys: async () => ["sysmon-static-v90", "other-cache"],
    delete: async (key) => {
      deletedCacheKeys.push(key);
      return true;
    },
  },
  location: {
    reload: () => {
      reloadCount += 1;
    },
  },
  innerWidth: 390,
	innerHeight: 844,
	devicePixelRatio: 3,
  screen: {
    width: 390,
    height: 844,
    orientation: { type: "portrait-primary" },
  },
	matchMedia: (query) => ({ matches: query === "(display-mode: standalone)" }),
	addEventListener: (name, callback) => {
		if (!windowListeners.has(name)) {
			windowListeners.set(name, []);
		}
		windowListeners.get(name).push(callback);
	},
	dispatchWindow: (name, event = {}) => {
		for (const callback of windowListeners.get(name) || []) {
			callback(event);
		}
	},
	fetch: async (path, options = {}) => {
    if (path === "/api/metrics") {
      metricsRequests += 1;
      return response(sampleMetrics());
    }
    if (path === "/api/status") {
      statusRequests += 1;
      return response(sampleStatus());
    }
    if (path === "/api/settings" && options.method === "POST") {
      settings = mergeDashboardSettings(settings, JSON.parse(options.body));
      return response(settings);
    }
    if (path === "/api/settings") {
      return response(settings);
    }
    if (path === "/api/client-check" && options.method === "POST") {
      clientCheckRequests += 1;
      lastClientCheck = JSON.parse(options.body);
      return response({ seen: true, last_seen: new Date().toISOString(), ...lastClientCheck });
    }
    return response({ error: "not found" }, 404);
  },
	setInterval: (callback, delay) => {
		const id = nextIntervalId;
		nextIntervalId += 1;
		intervalCallbacks.set(id, { callback, delay });
		return id;
	},
	clearInterval: (id) => {
		intervalCallbacks.delete(id);
	},
  setTimeout: (callback) => {
    const id = nextTimeoutId;
    nextTimeoutId += 1;
    timeoutCallbacks.set(id, callback);
    return id;
  },
	clearTimeout: (id) => {
		timeoutCallbacks.delete(id);
	},
  runIntervalsForDelay,
	intervalCountForDelay,
	totalIntervalCount: () => intervalCallbacks.size,
  reloadCount: () => reloadCount,
  unregisterCount: () => unregisterCount,
  deletedCacheKeys: () => [...deletedCacheKeys],
	__sysmonNow: () => fakeNow,
};
context.window = context;

vm.runInNewContext(app, context, { filename: "static/app.js" });
document.dispatch("DOMContentLoaded");
await flushMicrotasks();
const initialPassiveClientCheck = { ...lastClientCheck };
const initialClientCheckRequestCount = clientCheckRequests;

assert(document.getElementById("hostname").textContent === "labbox", "hostname did not render");
assert(document.getElementById("cpuValue").textContent === "37%", "CPU gauge did not render");
assert(document.getElementById("cpuSub").textContent === "3.60 GHz", "CPU gauge sub did not render live clock");
assert(!document.getElementById("cpuGauge").classList.contains("hide-inner"), "available CPU clock did not show the inner ring");
assert(document.getElementById("cpuGauge").style.values.get("--inner-p") === String(3600 / 5200 * 100), "CPU clock inner ring fill did not scale clock against max clock");
assert(document.getElementById("cpuGauge").style.values.get("--inner-c") === "var(--accent)", "CPU clock inner ring did not use the steady accent color");
assert(document.getElementById("cpuDetail").textContent === "52°C · 88.5 W", "CPU card detail did not render temp + power");
assert(document.getElementById("cpuCores").children[0].textContent === "busy 2/8", "CPU core grid did not render busy count");
assert(document.getElementById("cpuCores").children[1].children.length === 8, "CPU core grid did not render one bar per core");
assert(document.getElementById("memValue").textContent === "50%", "memory gauge did not render");
assert(document.getElementById("memSub").textContent === "8.0 GB / 16 GB", "memory gauge sub did not render used/total");
assert(document.getElementById("memDetail").textContent === "⇅ 2.0 GB swap", "memory card detail did not render live swap used");
assert(document.getElementById("gpuValue").textContent === "25%", "GPU gauge did not render");
assert(document.getElementById("gpuSub").textContent === "2.0 GB / 8.0 GB", "GPU gauge sub did not render VRAM used/total");
assert(document.getElementById("gpuDetail").textContent === "49°C · 112 W", "GPU card detail did not render temp + power");
assert(document.getElementById("netValue").textContent.includes("2.0K"), "NET gauge did not render download rate");
assert(document.getElementById("netSub").textContent.includes("1.0K"), "NET gauge sub did not render upload rate");
assert(document.getElementById("cpuName").textContent === "AMD Ryzen 9 7950X", "CPU identity line did not render the CPU model");
assert(document.getElementById("gpuName").textContent === "GPU 0", "GPU identity line did not render the GPU model");
assert(document.getElementById("memName").textContent === "DDR5 · 6000 MT/s", "RAM identity line did not render type + speed");
assert(document.getElementById("netName").textContent === "BiBi-Pro-Max", "NET identity line did not render the Wi-Fi SSID");
assert(document.getElementById("netDetail").textContent === "Tailscale", "NET card detail did not render the Tailscale label");
{
  const netChildren = document.getElementById("netDetail").children;
  assert(netChildren.length === 3, "NET card detail did not render a Tailscale label plus two status pills");
  assert(netChildren[0].textContent === "Tailscale", "NET card detail did not lead with the 'Tailscale' label");
  const tsPills = [netChildren[1], netChildren[2]];
  assert(tsPills[0].getAttribute("aria-label") === "Tailscale online", "NET Tailscale pill did not report online state");
  assert(tsPills[0].classList.values.has("ts-on") === true, "NET Tailscale pill was not coloured online");
  assert(tsPills[1].getAttribute("aria-label") === "Exit node off", "NET exit-node pill did not report off state");
  assert(tsPills[1].classList.values.has("ts-dim") === true, "NET exit-node pill was not coloured off");
}
assert(sparklineBars("memTrend").length === 24, "memory sparkline did not render fixed sample slots");
assert(sparklineBars("gpuTrend").length === 24, "GPU sparkline did not render fixed sample slots");
assert(sparklineBars("netTrend").length === 24, "network sparkline did not render fixed sample slots");
assert(lastSparklineBar("memTrend").style.values.get("--h") === "50%", "memory sparkline did not render latest value");
assert(lastSparklineBar("gpuTrend").style.values.get("--h") === "25%", "GPU sparkline did not render latest value");
assert(lastSparklineBar("netTrend").style.values.get("--h") === "6%", "network sparkline did not render floored latest value");
assert(document.getElementById("updatedAt").textContent.endsWith("/ 0s / 142ms"), "metric timestamp did not include sample age and collection duration");
assert(document.getElementById("alertsPanel").hidden === true, "healthy metrics showed threshold alerts panel");
assert(document.getElementById("issuesPanel").hidden === true, "healthy metrics showed collector issues panel");
assert(document.getElementById("agentMeta").textContent === "up 1h 2m / saved / app", "agent status metadata did not render");
context.renderStatus(sampleObservedStatus());
assert(document.getElementById("agentMeta").textContent === "up 1h 2m / saved / app / seen 12s", "agent status metadata did not render client-check age");
context.renderStatus(sampleStatus());
context.renderStatus({ uptime_seconds: Number.POSITIVE_INFINITY, settings_persisted: false });
assert(document.getElementById("agentMeta").textContent === "up 0m / memory / app", "malformed status uptime did not fall back cleanly");
context.renderStatus({ ...sampleStatus(), dashboard_build: "sysmon-static-v99" });
assert(document.getElementById("issuesPanel").hidden === false, "stale dashboard build did not show issues panel");
assert(document.getElementById("issuesSummary").textContent === "1 issue", "stale dashboard build issue count did not render");
assert(document.getElementById("issuesList").children[0].textContent === "dashboard build stale: app sysmon-static-v111, server sysmon-static-v99; tap status strip to refresh app or re-add Home Screen app", "stale dashboard build issue did not render");
await document.getElementById("statusStrip").click();
await flushMicrotasks();
assert(context.reloadCount() === 1, "stale dashboard status-strip tap did not reload the app");
assert(context.unregisterCount() === 1, "stale dashboard refresh did not unregister the service worker");
assert(context.deletedCacheKeys().join(",") === "sysmon-static-v90", "stale dashboard refresh deleted the wrong caches");
context.syncVisibleTimers();
const mixedIssueMetrics = {
  ...sampleMetrics(),
  collection_errors: [
    "cpu: unavailable",
    "memory: unavailable",
    "disk: unavailable",
    "network: warming up",
    "temperature: unavailable",
    "gpu: unavailable",
  ],
};
context.render(mixedIssueMetrics);
assert(document.getElementById("issuesSummary").textContent === "7 issues", "mixed status and metric issue count did not render");
assert(document.getElementById("issuesList").children.length === 6, "collapsed mixed issues did not show five rows plus overflow count");
await document.getElementById("issuesPanel").click();
assert(document.getElementById("issuesPanel").getAttribute("aria-expanded") === "true", "mixed issues panel tap did not expand details");
assert(document.getElementById("issuesList").children.length === 7, "expanded mixed issues did not render every issue");
assert(
  Array.from(document.getElementById("issuesList").children)
    .filter((row) => row.textContent.startsWith("dashboard build stale:")).length === 1,
  "expanding mixed issues duplicated the stale dashboard build issue",
);
context.render(sampleMetrics());
context.renderStatus(sampleStatus());
assert(document.getElementById("agentMeta").textContent === "up 1h 2m / saved / app", "valid status metadata did not restore after malformed status");
assert(document.getElementById("issuesPanel").hidden === true, "matching dashboard build did not clear stale-build issue");
context.renderStatus(sampleObservedStatus({ dashboard_build: "sysmon-static-v80" }));
assert(document.getElementById("issuesPanel").hidden === false, "stale client-check build did not show issues panel");
assert(document.getElementById("issuesList").children[0].textContent === "latest client check stale: client sysmon-static-v80, app sysmon-static-v111; reload or re-add Home Screen app", "stale client-check build issue did not render");
context.renderStatus(sampleStatus());
context.renderStatus(sampleObservedStatus({ last_seen: new Date(fakeNow - 120_000).toISOString() }));
assert(document.getElementById("issuesPanel").hidden === false, "stale client-check timestamp did not show issues panel");
assert(document.getElementById("issuesList").children[0].textContent === "latest device check stale: seen 2m ago; tap status strip to refresh client check", "stale client-check timestamp issue did not render");
context.renderStatus({
  ...sampleStatus(),
  client_check: {
    seen: true,
    last_seen: new Date(fakeNow - 1_000).toISOString(),
    dashboard_build: "sysmon-static-v111",
    user_agent: "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0",
    viewport_width: 1440,
    viewport_height: 900,
    display_mode: "browser",
    standalone: false,
  },
  device_client_check: {
    seen: true,
    last_seen: new Date(fakeNow - 120_000).toISOString(),
    dashboard_build: "sysmon-static-v111",
    user_agent: "Mozilla/5.0 iPhone Mobile Safari",
    viewport_width: 390,
    viewport_height: 844,
    display_mode: "standalone",
    standalone: true,
  },
});
assert(document.getElementById("agentMeta").textContent === "up 1h 2m / saved / app / seen 2m", "agent metadata did not prefer device client-check age");
assert(document.getElementById("issuesList").children[0].textContent === "latest device check stale: seen 2m ago; tap status strip to refresh client check", "device stale issue did not survive newer desktop client check");
context.renderStatus(sampleStatus());
const standaloneMatchMedia = context.matchMedia;
context.navigator.userAgent = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 Mobile Safari/604.1";
context.matchMedia = (query) => ({ matches: query === "(display-mode: browser)" });
context.renderStatus(sampleStatus());
assert(document.getElementById("agentMeta").textContent === "up 1h 2m / saved / web", "device browser mode did not render web metadata");
assert(document.getElementById("issuesPanel").hidden === false, "device browser mode did not show Home Screen issue");
assert(document.getElementById("issuesList").children[0].textContent === "Display is in web mode; open the installed Home Screen app for final monitor verification", "device browser mode issue did not render");
context.matchMedia = standaloneMatchMedia;
context.renderStatus(sampleStatus());
assert(document.getElementById("issuesPanel").hidden === true, "standalone device mode did not clear Home Screen issue");
context.navigator.userAgent = "";
context.applySettings({ dim: false, shift: false, refresh_ms: 1000, panel: "all", thresholds: { ...defaultThresholds } });
context.renderStatus(sampleStatus());
assert(context.intervalCountForDelay(2000) === 1, "status settings did not reschedule the metrics timer to the saved refresh interval");
fakeNow = Date.parse("2026-01-01T00:00:45.000Z");
context.render({ ...sampleMetrics(), timestamp: "2026-01-01T00:00:05.000Z" });
assert(document.getElementById("updatedAt").textContent.endsWith("/ 40s / 142ms"), "metric timestamp did not show seconds-old sample age");
fakeNow = Date.parse("2026-01-01T00:02:06.000Z");
context.refreshMetricAge();
assert(document.getElementById("updatedAt").textContent.endsWith("/ 2m / 142ms"), "metric age did not refresh to minutes");
context.render({ ...sampleMetrics(), collection_duration_ms: 1250 });
assert(document.getElementById("updatedAt").textContent.endsWith("/ 0s / 1.3s"), "metric timestamp did not format second-scale collection duration");
context.render({ ...sampleMetrics(), collection_duration_ms: -1 });
assert(document.getElementById("updatedAt").textContent.endsWith("/ 0s"), "invalid collection duration should not render");
fakeNow = 1000;
context.render(alertMetrics());
const alertsPanel = document.getElementById("alertsPanel");
const alertsList = document.getElementById("alertsList");
assert(alertsPanel.hidden === false, "threshold breaches did not show alerts panel");
assert(document.getElementById("alertsSummary").textContent === "7 alerts", "threshold alerts summary did not render count");
assert(alertsPanel.getAttribute("aria-expanded") === "false", "many threshold alerts started expanded");
assert(alertsList.children.length === 6, "collapsed alerts did not show five rows plus overflow count");
assert(alertsList.children[0].textContent === "Disk / 92% over 70%", "alert row did not render disk threshold breach");
assert(alertsList.children[5].textContent === "2 more", "collapsed alerts did not show remaining count");
await alertsPanel.click();
assert(alertsPanel.getAttribute("aria-expanded") === "true", "alert panel tap did not expand details");
assert(alertsList.children.length === 7, "expanded alerts did not render every alert");
const alertCollapseKey = await alertsPanel.dispatch("keydown", { key: " " });
assert(alertCollapseKey.defaultPrevented === true, "alert panel space key did not prevent default scrolling");
assert(alertsPanel.getAttribute("aria-expanded") === "false", "alert panel space key did not collapse details");
context.render(sampleMetrics());
assert(document.getElementById("alertsPanel").hidden === true, "normal metrics did not hide threshold alerts panel");
context.render(sampleMetrics());
assert(context.intervalCountForDelay(2000) === 1, "refresh setting did not keep the metrics timer at the saved interval");
assert(document.getElementById("dimBtn").getAttribute("aria-pressed") === "false", "inactive dim control exposed pressed state");
// Warn thresholds are host-side config now (CLI flags / env) -- there is no
// on-device threshold control to click. A raised threshold must still recolor
// the gauges, so drive it through the settings path instead of removed buttons.
context.applySettings({ ...defaultSettings(), thresholds: { ...defaultThresholds, cpu_warn: 80 } });
context.render({ ...sampleMetrics(), cpu_percent: { available: true, value: 76, unit: "%" } });
assert(document.getElementById("cpuGauge").style.values.get("--c") === "var(--good)", "raised CPU warning threshold did not affect gauge color");
const settingsAfterLocalUpdates = settings;
settings = mergeDashboardSettings(settings, { refresh_ms: 1000, panel: "network" });
context.renderStatus(sampleStatus());
assert(context.intervalCountForDelay(1000) === 1, "status settings did not sync remote refresh after local update completed");
settings = settingsAfterLocalUpdates;
context.renderStatus(sampleStatus());
runTimeouts();
assert(metricsRequests === 1, "initial load did not fetch metrics once");
assert(statusRequests === 1, "initial load did not fetch status once");
assert(initialClientCheckRequestCount === 1, "initial load did not send a client check");
assert(beaconRequests === 0, "initial foreground client check used beacon instead of fetch");
assert(context.totalIntervalCount() === 4, "visible dashboard did not register all polling timers");
assert(context.intervalCountForDelay(2000) === 1, "saved refresh setting did not reschedule the metrics timer");
assert(context.intervalCountForDelay(60000) === 1, "visible dashboard did not register the status timer");
assert(context.intervalCountForDelay(30000) === 1, "visible dashboard did not register the client-check timer");
assert(context.intervalCountForDelay(5000) === 1, "visible dashboard did not register the stale-sample timer");
assert(initialPassiveClientCheck.viewport_width === 390, "client check did not include viewport width");
assert(initialPassiveClientCheck.dashboard_build === "sysmon-static-v111", "client check did not include current dashboard build");
assert(initialPassiveClientCheck.viewport_height === 844, "client check did not include viewport height");
assert(initialPassiveClientCheck.screen_width === 390, "client check did not include screen width");
assert(initialPassiveClientCheck.screen_height === 844, "client check did not include screen height");
assert(initialPassiveClientCheck.device_pixel_ratio === 3, "client check did not include device pixel ratio");
assert(initialPassiveClientCheck.touch_points === 5, "client check did not include touch capability");
assert(initialPassiveClientCheck.display_mode === "standalone", "client check did not include standalone display mode");
assert(initialPassiveClientCheck.standalone === true, "client check did not include standalone display state");
assert(initialPassiveClientCheck.visibility === "visible", "client check did not include visibility state");
assert(initialPassiveClientCheck.orientation === "portrait-primary", "client check did not include orientation");
assert(!("interaction" in initialPassiveClientCheck), "passive client check should not include interaction evidence");
const healthyFetch = context.fetch;
context.fetch = async (path, options = {}) => {
  if (path === "/api/status") {
    statusRequests += 1;
    throw new Error("network down");
  }
  return healthyFetch(path, options);
};
await context.fetchStatus();
assert(document.getElementById("agentMeta").textContent === "status NA", "failed status fetch did not render status NA metadata");
assert(document.getElementById("issuesPanel").hidden === false, "failed status fetch did not show issues panel");
assert(document.getElementById("issuesSummary").textContent === "1 issue", "failed status fetch issue count did not render");
assert(document.getElementById("issuesList").children[0].textContent === "status unavailable: network down", "failed status fetch issue did not render");
context.fetch = healthyFetch;
await context.fetchStatus();
assert(document.getElementById("issuesPanel").hidden === true, "healthy status fetch did not clear status failure issue");
const healthyMetricsFetch = context.fetch;
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    throw new Error("metrics down");
  }
  return healthyMetricsFetch(path, options);
};
await context.fetchMetrics();
assert(document.getElementById("statusText").textContent === "metrics down", "failed metrics fetch did not render bad status message");
assert(document.getElementById("issuesPanel").hidden === false, "failed metrics fetch did not show issues panel");
assert(document.getElementById("issuesSummary").textContent === "1 issue", "failed metrics fetch issue count did not render");
assert(document.getElementById("issuesList").children[0].textContent === "metrics unavailable: metrics down", "failed metrics fetch issue did not render");
context.fetch = healthyMetricsFetch;
await context.fetchMetrics();
assert(document.getElementById("issuesPanel").hidden === true, "healthy metrics fetch did not clear metrics failure issue");
const clientChecksBeforeResize = clientCheckRequests;
context.innerWidth = 844;
context.innerHeight = 390;
context.dispatchWindow("resize");
runTimeouts();
await flushMicrotasks();
assert(clientCheckRequests === clientChecksBeforeResize + 1, "resize did not refresh the client check");
assert(lastClientCheck.viewport_width === 844, "resize client check did not update viewport width");
assert(lastClientCheck.viewport_height === 390, "resize client check did not update viewport height");
document.visibilityState = "hidden";
context.dispatchWindow("orientationchange");
runTimeouts();
await flushMicrotasks();
assert(clientCheckRequests === clientChecksBeforeResize + 1, "hidden orientation change sent a client check");
document.visibilityState = "visible";
context.runIntervalsForDelay(30000);
await flushMicrotasks();
assert(clientCheckRequests === clientChecksBeforeResize + 2, "visible heartbeat did not refresh the client check");
assert(beaconRequests === 0, "visible heartbeat used beacon instead of fetch");
assert(context.shouldPollMetrics(), "visible unpaused dashboard should poll metrics");
assert(context.shouldPollStatus(), "visible dashboard should poll status");
document.visibilityState = "hidden";
assert(!context.shouldPollMetrics(), "hidden dashboard should not poll metrics from the interval");
assert(!context.shouldPollStatus(), "hidden dashboard should not poll status from the interval");
document.visibilityState = "visible";
assert(context.shouldPollMetrics(), "visible dashboard should resume interval polling eligibility");
assert(context.shouldPollStatus(), "visible dashboard should resume status polling eligibility");

context.applySettings({ dim: false, shift: false, refresh_ms: 999, panel: "shell" });
assert(context.intervalCountForDelay(1000) === 1, "invalid refresh setting did not fall back to 1s");

context.applySettings(settings);

await document.getElementById("shiftBtn").click();
await flushMicrotasks();
assert(settings.shift === true, "shift control did not update settings");
assert(document.body.classList.contains("shift"), "shift setting did not apply body class");
assert(document.getElementById("shiftBtn").classList.contains("active"), "shift control did not sync active state");
assert(document.getElementById("shiftBtn").getAttribute("aria-pressed") === "true", "shift control did not expose pressed state");

await document.getElementById("wakeBtn").click();
assert(document.getElementById("wakeBtn").textContent === "×", "unsupported wake lock did not show unavailable state");
assert(document.getElementById("wakeBtn").getAttribute("aria-label") === "Wake lock unavailable", "unsupported wake lock did not update accessible label");
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "false", "unsupported wake lock exposed pressed state");
assert(localStorageValues.get("sysmon:wake-wanted") === "0", "unsupported wake lock did not persist disabled preference");
await document.getElementById("wakeBtn").click();
assert(document.getElementById("wakeBtn").textContent === "×", "unsupported wake lock did not stay unavailable on retry");
assert(document.getElementById("wakeBtn").getAttribute("aria-label") === "Wake lock unavailable", "unsupported wake lock did not keep unavailable label");
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "false", "unsupported wake lock retry exposed pressed state");

let wakeRequests = 0;
let activeWakeRelease = null;
context.navigator.wakeLock = {
  request: async () => {
    wakeRequests += 1;
    const lock = {
      addEventListener: (name, callback) => {
        if (name === "release") {
          activeWakeRelease = callback;
        }
      },
      release: async () => {
        if (activeWakeRelease) {
          activeWakeRelease();
        }
      },
    };
    return lock;
  },
};
await document.getElementById("wakeBtn").click();
await flushMicrotasks();
assert(wakeRequests === 1, "wake lock support did not request a screen lock");
assert(document.getElementById("wakeBtn").getAttribute("aria-label") === "Screen awake", "wake lock support did not show active label");
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "true", "wake lock support did not expose active pressed state");
assert(localStorageValues.get("sysmon:wake-wanted") === "1", "enabled wake lock did not persist enabled preference");
activeWakeRelease();
await flushMicrotasks();
assert(wakeRequests === 2, "released wake lock was not reacquired while still wanted");
assert(document.getElementById("wakeBtn").getAttribute("aria-label") === "Screen awake", "reacquired wake lock did not restore active label");
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "true", "reacquired wake lock did not restore pressed state");
await document.getElementById("wakeBtn").click();
await flushMicrotasks();
assert(document.getElementById("wakeBtn").getAttribute("aria-label") === "Keep screen awake", "turning wake lock off did not restore idle label");
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "false", "turning wake lock off did not clear pressed state");
assert(localStorageValues.get("sysmon:wake-wanted") === "0", "turning wake lock off did not persist disabled preference");

let silentReleaseCalls = 0;
context.navigator.wakeLock = {
  request: async () => ({
    addEventListener: () => {},
    release: async () => {
      silentReleaseCalls += 1;
    },
  }),
};
await document.getElementById("wakeBtn").click();
await flushMicrotasks();
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "true", "silent wake lock did not activate");
await document.getElementById("wakeBtn").click();
await flushMicrotasks();
assert(silentReleaseCalls === 1, "silent wake lock release was not called");
assert(document.getElementById("wakeBtn").getAttribute("aria-label") === "Keep screen awake", "silent wake lock release did not restore idle label");
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "false", "silent wake lock release did not clear pressed state");

let failedReleaseCalls = 0;
context.navigator.wakeLock = {
  request: async () => ({
    addEventListener: () => {},
    release: async () => {
      failedReleaseCalls += 1;
      throw new Error("release failed");
    },
  }),
};
await document.getElementById("wakeBtn").click();
await flushMicrotasks();
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "true", "failing-release wake lock did not activate");
await document.getElementById("wakeBtn").click();
await flushMicrotasks();
assert(failedReleaseCalls === 1, "failing wake lock release was not called");
assert(document.getElementById("wakeBtn").getAttribute("aria-label") === "Keep screen awake", "failing wake lock release did not restore idle label");
assert(document.getElementById("wakeBtn").getAttribute("aria-pressed") === "false", "failing wake lock release did not clear pressed state");

await document.getElementById("pauseBtn").click();
assert(document.getElementById("pauseBtn").textContent === "▶", "pause button did not switch to resume icon");
assert(document.getElementById("pauseBtn").getAttribute("aria-label") === "Resume updates", "pause button did not update accessible label");
assert(document.getElementById("pauseBtn").getAttribute("aria-pressed") === "true", "pause button did not expose pressed state");
assert(document.getElementById("statusText").textContent === "Paused", "pause control did not update status");
assert(document.getElementById("statusDot").classList.contains("paused"), "paused status did not show paused indicator");
assert(!document.getElementById("statusDot").classList.contains("warn"), "paused status looked like a warning");
assert(context.totalIntervalCount() === 3, "paused dashboard kept the metrics polling timer active");
assert(context.intervalCountForDelay(2000) === 0, "paused dashboard kept polling metrics at the refresh interval");
assert(context.intervalCountForDelay(60000) === 1, "paused dashboard stopped status polling");
assert(context.intervalCountForDelay(30000) === 1, "paused dashboard stopped client-check polling");
const metricsBeforePausedStatusTap = metricsRequests;
const statusBeforePausedStatusTap = statusRequests;
const clientChecksBeforePausedStatusTap = clientCheckRequests;
await document.getElementById("statusStrip").click();
assert(document.getElementById("statusText").textContent === "Refreshing", "paused status strip tap did not show refresh feedback");
await flushMicrotasks();
assert(metricsRequests === metricsBeforePausedStatusTap + 1, "paused status strip tap did not refresh metrics");
assert(statusRequests === statusBeforePausedStatusTap + 1, "paused status strip tap did not refresh status");
assert(clientCheckRequests === clientChecksBeforePausedStatusTap + 1, "paused status strip tap did not refresh client check");
assert(lastClientCheck.interaction === "status_strip_tap", "paused status strip tap did not include interaction evidence");
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Client check sent (app)", "paused status strip tap did not confirm client check success");
runTimeouts();
assert(document.getElementById("statusText").textContent === "Paused", "paused status strip refresh did not restore paused state");
const beaconsBeforeHiddenPause = beaconRequests;
document.visibilityState = "hidden";
document.dispatch("visibilitychange");
await flushMicrotasks();
assert(metricsRequests === metricsBeforePausedStatusTap + 1, "hidden visibility change fetched metrics while paused");
assert(statusRequests === statusBeforePausedStatusTap + 1, "hidden visibility change fetched status while paused");
assert(beaconRequests === beaconsBeforeHiddenPause + 1, "hidden visibility change did not send beacon client check");
assert(lastBeaconPath === "/api/client-check", "hidden client check beacon used wrong path");
assert(JSON.parse(await lastBeaconBody.text()).visibility === "hidden", "hidden client check beacon did not capture hidden visibility");
assert(context.totalIntervalCount() === 0, "hidden dashboard kept polling timers active");
document.visibilityState = "visible";
document.dispatch("visibilitychange");
await flushMicrotasks();
assert(metricsRequests === metricsBeforePausedStatusTap + 1, "visible visibility change fetched metrics while paused");
assert(statusRequests === statusBeforePausedStatusTap + 2, "visible visibility change did not refresh status while paused");
assert(context.totalIntervalCount() === 3, "visible paused dashboard did not restore non-metrics timers");
assert(context.intervalCountForDelay(2000) === 0, "visible paused dashboard restored the metrics timer");
const resumePoll = deferredResponse(sampleMetrics());
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return resumePoll.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};
const metricsBeforeResume = metricsRequests;
await document.getElementById("pauseBtn").click();
assert(document.getElementById("pauseBtn").textContent === "Ⅱ", "pause button did not switch back to pause icon");
assert(document.getElementById("pauseBtn").getAttribute("aria-pressed") === "false", "pause button did not clear pressed state on resume");
assert(!document.getElementById("statusDot").classList.contains("paused"), "resume left paused indicator active");
assert(context.totalIntervalCount() === 4, "resumed dashboard did not restore all polling timers");
assert(context.intervalCountForDelay(2000) === 1, "resumed dashboard did not restore the metrics timer");
assert(metricsRequests === metricsBeforeResume + 1, "resume did not trigger an immediate metrics refresh");
assert(document.getElementById("statusText").textContent === "Updating", "resume reported live before refresh completed");
assert(document.getElementById("statusDot").classList.contains("warn"), "resume did not show updating warning while refresh was in flight");
resumePoll.resolve();
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Live", "resume did not restore live state after refresh completed");

const visibleRefreshPoll = deferredResponse(sampleMetrics());
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return visibleRefreshPoll.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};

const metricsBeforeVisibleRefresh = metricsRequests;
const statusBeforeVisibleRefresh = statusRequests;
document.visibilityState = "hidden";
document.dispatch("visibilitychange");
await flushMicrotasks();
assert(metricsRequests === metricsBeforeVisibleRefresh, "hidden visibility change fetched metrics");
assert(statusRequests === statusBeforeVisibleRefresh, "hidden visibility change fetched status");
assert(context.totalIntervalCount() === 0, "hidden visible-refresh scenario kept polling timers active");
document.visibilityState = "visible";
document.dispatch("visibilitychange");
await flushMicrotasks();
assert(metricsRequests === metricsBeforeVisibleRefresh + 1, "visible visibility change did not refresh metrics");
assert(statusRequests === statusBeforeVisibleRefresh + 1, "visible visibility change did not refresh status");
assert(context.totalIntervalCount() === 4, "visible refresh scenario did not restore polling timers");
assert(context.intervalCountForDelay(2000) === 1, "visible refresh scenario did not restore metrics polling");
visibleRefreshPoll.resolve();
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Live", "visible refresh did not restore live state before stale check");

const beaconsBeforePagehide = beaconRequests;
context.dispatchWindow("pagehide");
await flushMicrotasks();
assert(beaconRequests === beaconsBeforePagehide + 1, "pagehide did not send a final client-check beacon");
assert(context.totalIntervalCount() === 0, "pagehide kept polling timers active");
const metricsBeforePageshow = metricsRequests;
const statusBeforePageshow = statusRequests;
context.dispatchWindow("pageshow", { persisted: true });
await flushMicrotasks();
assert(context.totalIntervalCount() === 4, "persisted pageshow did not restore polling timers");
assert(metricsRequests === metricsBeforePageshow + 1, "persisted pageshow did not refresh metrics");
assert(statusRequests === statusBeforePageshow + 1, "persisted pageshow did not refresh status");

fakeNow = 12001;
context.markStaleIfNeeded();
assert(
  document.getElementById("statusText").textContent === "Stale",
  `stale metrics did not update status, got ${document.getElementById("statusText").textContent}`,
);
assert(document.getElementById("statusDot").classList.contains("warn"), "stale metrics did not show warning state");

const stalePoll = deferredResponse(sampleMetrics());
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return stalePoll.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};
const metricsBeforeStaleFetch = metricsRequests;
const staleFetch = context.fetchMetrics();
await flushMicrotasks();
assert(metricsRequests === metricsBeforeStaleFetch + 1, "stale refresh did not request metrics");
assert(document.getElementById("statusText").textContent === "Stale", "stale refresh hid stale status while in flight");
assert(document.getElementById("statusDot").classList.contains("warn"), "stale refresh removed warning state while in flight");
stalePoll.resolve();
await staleFetch;
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Live", "stale refresh did not restore live state after success");

const statusTapPoll = deferredResponse(sampleMetrics());
const statusTapClientCheck = deferredResponse({ seen: true, display_mode: "standalone", standalone: true });
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return statusTapPoll.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  if (path === "/api/client-check" && options.method === "POST") {
    clientCheckRequests += 1;
    lastClientCheck = JSON.parse(options.body);
    return statusTapClientCheck.promise;
  }
  return response({ error: "not found" }, 404);
};
const clientChecksBeforeStatusTap = clientCheckRequests;
const metricsBeforeStatusTap = metricsRequests;
await document.getElementById("statusStrip").click();
await flushMicrotasks();
assert(metricsRequests === metricsBeforeStatusTap + 1, "status strip tap did not trigger metrics refresh");
assert(clientCheckRequests === clientChecksBeforeStatusTap + 1, "status strip tap did not refresh the client check");
assert(lastClientCheck.interaction === "status_strip_tap", "status strip tap did not include interaction evidence");
assert(document.getElementById("statusText").textContent === "Refreshing", "status strip tap did not show refresh feedback");
statusTapClientCheck.resolve();
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Refreshing", "status strip tap confirmed client check before metrics settled");
statusTapPoll.resolve();
await flushMicrotasks();
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Client check sent (app)", "status strip tap did not confirm client check success");
runTimeouts();
assert(document.getElementById("statusText").textContent === "Live", "status strip refresh did not restore live state");

const browserModeTapMetrics = deferredResponse(sampleMetrics());
const browserModeClientCheck = deferredResponse({ seen: true, display_mode: "browser", standalone: false });
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return browserModeTapMetrics.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  if (path === "/api/client-check" && options.method === "POST") {
    clientCheckRequests += 1;
    lastClientCheck = JSON.parse(options.body);
    return browserModeClientCheck.promise;
  }
  return response({ error: "not found" }, 404);
};
await document.getElementById("statusStrip").click();
await flushMicrotasks();
browserModeClientCheck.resolve();
browserModeTapMetrics.resolve();
await flushMicrotasks();
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Client check sent (web)", "status strip tap did not use accepted client-check display mode");
runTimeouts();

context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    return response({ error: "same-origin required" }, 403);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};
await document.getElementById("dimBtn").click();
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Settings failed: HTTP 403", "settings failure did not show transient error");
assert(document.getElementById("statusDot").classList.contains("ok"), "settings failure changed live indicator to offline");
assert(!document.getElementById("statusDot").classList.contains("bad"), "settings failure showed bad connection indicator");
assert(document.getElementById("issuesPanel").hidden === false, "settings failure did not show issues panel");
assert(document.getElementById("issuesSummary").textContent === "1 issue", "settings failure issue count did not render");
assert(document.getElementById("issuesList").children[0].textContent === "settings update failed: HTTP 403", "settings failure issue did not render");
runTimeouts();
assert(document.getElementById("statusText").textContent === "Live", "settings failure did not restore previous connection label");
assert(document.getElementById("issuesPanel").hidden === false, "settings failure issue cleared with transient status");
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};
await context.updateSettings({ dim: false });
assert(document.getElementById("issuesPanel").hidden === true, "successful settings update did not clear settings failure issue");
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    throw new Error("settings read down");
  }
  return response({ error: "not found" }, 404);
};
await context.fetchSettings();
assert(document.getElementById("issuesPanel").hidden === false, "settings read failure did not show issues panel");
assert(document.getElementById("issuesList").children[0].textContent === "settings unavailable: settings read down", "settings read failure issue did not render");
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};
await context.fetchSettings();
assert(document.getElementById("issuesPanel").hidden === true, "successful settings fetch did not clear settings read failure issue");

const steadyPoll = deferredResponse(sampleMetrics());
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return steadyPoll.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};
const metricsBeforeSteadyFetch = metricsRequests;
const steadyFetch = context.fetchMetrics();
await flushMicrotasks();
assert(metricsRequests === metricsBeforeSteadyFetch + 1, "steady poll did not request metrics");
assert(document.getElementById("statusText").textContent === "Live", "steady poll flickered live status to updating");
steadyPoll.resolve();
await steadyFetch;
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "Live", "steady poll did not finish live");

context.fetch = async (path) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response({ error: "offline" }, 503);
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  return response(settings);
};
await context.fetchMetrics();
await flushMicrotasks();
assert(document.getElementById("statusText").textContent === "HTTP 503", "metrics failure did not render offline status");
fakeNow = 30000;
context.markStaleIfNeeded();
assert(document.getElementById("statusText").textContent === "HTTP 503", "stale checker overwrote offline status");
assert(document.getElementById("statusDot").classList.contains("bad"), "offline status lost bad indicator");

context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  if (path === "/api/client-check" && options.method === "POST") {
    clientCheckRequests += 1;
    lastClientCheck = JSON.parse(options.body);
    return response({ seen: true, last_seen: new Date().toISOString(), ...lastClientCheck });
  }
  return response({ error: "not found" }, 404);
};

// The refresh interval is host-side config (CLI flag / env) now; there is no
// on-device refresh control to click. Timer-reschedule behaviour from a saved
// refresh setting is covered by the applySettings/intervalCountForDelay checks
// above and the settings-ordering checks below.

settings = mergeDashboardSettings(settings, { dim: false });
context.applySettings(settings);
const pendingSettingsPosts = [];
context.fetch = async (path, options = {}) => {
  if (path === "/api/settings" && options.method === "POST") {
    const body = JSON.parse(options.body);
    const pending = deferredResponse({ ...settings, ...body });
    pendingSettingsPosts.push({ body, ...pending });
    return pending.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  return response(settings);
};
const firstDimTap = document.getElementById("dimBtn").click();
await flushMicrotasks();
assert(pendingSettingsPosts.length === 1, "first dim tap did not post settings");
assert(pendingSettingsPosts[0].body.dim === true, "first dim tap did not post enabled dim setting");
assert(document.body.classList.contains("dim"), "first dim tap did not apply optimistic dim state");
assert(document.getElementById("dimBtn").getAttribute("aria-pressed") === "true", "first dim tap did not expose optimistic pressed state");
const secondDimTap = document.getElementById("dimBtn").click();
await flushMicrotasks();
assert(pendingSettingsPosts.length === 2, "second dim tap did not post settings");
assert(pendingSettingsPosts[1].body.dim === false, "second dim tap read stale dim state");
assert(!document.body.classList.contains("dim"), "second dim tap did not undo optimistic dim state");
assert(document.getElementById("dimBtn").getAttribute("aria-pressed") === "false", "second dim tap did not clear optimistic pressed state");
pendingSettingsPosts[1].resolve();
await secondDimTap;
await flushMicrotasks();
pendingSettingsPosts[0].resolve();
await firstDimTap;
await flushMicrotasks();
assert(!document.body.classList.contains("dim"), "older dim settings response overwrote latest optimistic state");

const olderSettingsResponse = deferredResponse(mergeDashboardSettings(defaultSettings(), { dim: false, shift: true, refresh_ms: 2000 }));
const newerSettingsResponse = deferredResponse(mergeDashboardSettings(defaultSettings(), { dim: false, shift: true, refresh_ms: 500 }));
let settingsPostCount = 0;
context.fetch = async (path, options = {}) => {
  if (path === "/api/settings" && options.method === "POST") {
    settingsPostCount += 1;
    return settingsPostCount === 1 ? olderSettingsResponse.promise : newerSettingsResponse.promise;
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  return response(settings);
};
const olderSettingsUpdate = context.updateSettings({ refresh_ms: 2000 });
const newerSettingsUpdate = context.updateSettings({ refresh_ms: 500 });
await flushMicrotasks();
newerSettingsResponse.resolve();
await newerSettingsUpdate;
await flushMicrotasks();
assert(context.intervalCountForDelay(500) === 1, "newer settings response did not apply");
olderSettingsResponse.resolve();
await olderSettingsUpdate;
await flushMicrotasks();
assert(context.intervalCountForDelay(500) === 1, "older settings response overwrote newer refresh state");
assert(context.intervalCountForDelay(2000) === 0, "older settings response left stale refresh selection");

context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/settings" && options.method === "POST") {
    settings = mergeDashboardSettings(settings, JSON.parse(options.body));
    return response(settings);
  }
  if (path === "/api/settings") {
    return response(settings);
  }
  return response({ error: "not found" }, 404);
};

context.render(warmingNetworkMetrics());
assert(document.getElementById("netValue").textContent === "NA", "warming network rates did not mute the NET gauge");
assert(document.getElementById("netGauge").classList.contains("unavailable"), "warming network did not mark the NET gauge unavailable");

context.render(unavailableWarmingNetworkMetrics());
assert(document.getElementById("netValue").textContent === "NA", "unavailable warming network did not mute the NET gauge");

context.render(resetNetworkMetrics());
assert(document.getElementById("netValue").textContent === "NA", "reset network counters did not mute the NET gauge");
assert(document.getElementById("netSub").textContent === "--", "reset network counters did not blank the NET gauge sub");

context.render(malformedNumericMetrics());
assert(document.getElementById("cpuValue").textContent === "NA", "malformed CPU value did not render as NA");
assert(document.getElementById("memValue").textContent === "NA", "malformed memory value did not render as NA");
assert(document.getElementById("gpuValue").textContent === "NA", "malformed GPU usage did not render as NA");
assert(document.getElementById("netValue").textContent === "NA", "malformed network rates did not mute the NET gauge");
assert(document.getElementById("cpuGauge").classList.contains("unavailable"), "malformed CPU gauge did not get unavailable class");
assert(document.getElementById("cpuGauge").style.values.get("--p") === "100", "malformed CPU gauge did not use full muted ring");
assert(document.getElementById("cpuGauge").style.values.get("--c") === "#394656", "malformed CPU gauge did not use muted color");
assert(document.getElementById("memGauge").classList.contains("unavailable"), "malformed memory gauge did not get unavailable class");
assert(document.getElementById("cpuGauge").classList.contains("hide-inner"), "malformed CPU clock did not hide the inner ring");

context.render(malformedCapacityCountersMetrics());
assert(document.getElementById("memValue").textContent === "NA", "malformed memory byte counters did not render as NA");
assert(document.getElementById("memGauge").classList.contains("unavailable"), "malformed memory counters did not mute the RAM gauge");
assert(document.getElementById("gpuGauge").classList.contains("hide-inner"), "malformed GPU VRAM counters did not hide the inner ring");

context.render(unsafeCapacityCountersMetrics());
assert(document.getElementById("memValue").textContent === "NA", "unsafe memory byte counters did not render as NA");
assert(document.getElementById("memGauge").classList.contains("unavailable"), "unsafe memory counters did not mute the RAM gauge");

context.render(partialGPUFallbackMetrics());
assert(document.getElementById("gpuValue").textContent === "NA", "partial GPU fallback did not render NA value");
assert(document.getElementById("gpuGauge").classList.contains("unavailable"), "partial GPU fallback did not mute the GPU gauge");
assert(document.getElementById("gpuSub").textContent === "--", "partial GPU fallback did not blank the VRAM sub");
assert(document.getElementById("gpuDetail").textContent === "--", "partial GPU fallback did not blank the GPU detail line");

context.render(degradedMetrics());
assert(document.getElementById("gpuValue").textContent === "NA", "unavailable GPU gauge did not render as NA");
assert(document.getElementById("netValue").textContent === "NA", "unavailable network did not mute the NET gauge");
assert(!document.getElementById("cpuGauge").classList.contains("hide-inner"), "unavailable CPU temperature wrongly hid the clock-driven inner ring");
assert(lastSparklineBar("gpuTrend").classList.contains("unavailable"), "unavailable GPU sample did not render muted sparkline bar");
assert(lastSparklineBar("netTrend").classList.contains("unavailable"), "unavailable network sample did not render muted sparkline bar");
assert(document.getElementById("gpuGauge").classList.contains("unavailable"), "unavailable GPU gauge did not get unavailable class");
assert(document.getElementById("gpuGauge").style.values.get("--p") === "100", "unavailable GPU gauge did not use full muted ring");
assert(document.getElementById("gpuGauge").style.values.get("--c") === "#394656", "unavailable GPU gauge did not use muted color");
assert(document.getElementById("netGauge").classList.contains("unavailable"), "unavailable network gauge did not get unavailable class");
assert(document.getElementById("issuesPanel").hidden === false, "degraded metrics did not show collector issues panel");
assert(document.getElementById("issuesSummary").textContent === "4 issues", "collector issues summary did not render count");
assert(document.getElementById("issuesList").children.length === 4, "collector issues list did not render all issues");
assert(document.getElementById("issuesList").children[0].textContent === "disk /backup: statfs denied", "collector issue row did not render first issue");

context.render(manyIssueMetrics());
const issuesPanel = document.getElementById("issuesPanel");
const issuesList = document.getElementById("issuesList");
assert(issuesPanel.getAttribute("aria-expanded") === "false", "many collector issues started expanded");
assert(!issuesPanel.classList.contains("expanded"), "many collector issues started with expanded class");
assert(issuesList.children.length === 6, "collapsed collector issues did not show five rows plus overflow count");
assert(issuesList.children[5].textContent === "2 more", "collapsed collector issues did not show remaining count");
await issuesPanel.click();
assert(issuesPanel.getAttribute("aria-expanded") === "true", "issue panel tap did not expose expanded state");
assert(issuesPanel.classList.contains("expanded"), "issue panel tap did not add expanded class");
assert(issuesList.children.length === 7, "expanded collector issues did not render every issue");
assert(issuesList.children[6].textContent === "gpu Intel temperature: no hwmon temperature exposed", "expanded collector issues missed final issue");
const collapseKey = await issuesPanel.dispatch("keydown", { key: " " });
assert(collapseKey.defaultPrevented === true, "issue panel space key did not prevent default scrolling");
assert(issuesPanel.getAttribute("aria-expanded") === "false", "issue panel space key did not collapse details");
assert(issuesList.children.length === 6, "collapsed issue panel did not restore overflow count");
const ignoredKey = await issuesPanel.dispatch("keydown", { key: "Escape" });
assert(ignoredKey.defaultPrevented === false, "issue panel ignored key unexpectedly prevented default");
assert(issuesPanel.getAttribute("aria-expanded") === "false", "issue panel ignored key toggled details");

metricsRequests = 0;
statusRequests = 0;
clientCheckRequests = 0;
context.fetch = async (path, options = {}) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return response(sampleMetrics());
  }
  if (path === "/api/status") {
    statusRequests += 1;
    return response(sampleStatus());
  }
  if (path === "/api/client-check" && options.method === "POST") {
    clientCheckRequests += 1;
    lastClientCheck = JSON.parse(options.body);
    return response({ seen: true, last_seen: new Date().toISOString(), ...lastClientCheck });
  }
  return response(settings);
};
document.visibilityState = "visible";
context.dispatchWindow("pageshow", { persisted: false });
await flushMicrotasks();
assert(metricsRequests === 0 && statusRequests === 0 && clientCheckRequests === 0, "non-persisted pageshow triggered a lifecycle refresh");
context.dispatchWindow("online");
await flushMicrotasks();
assert(metricsRequests === 1, "online event did not refresh metrics");
assert(statusRequests === 1, "online event did not refresh status");
assert(clientCheckRequests === 1, "online event did not refresh client check");
await flushMicrotasks();
context.dispatchWindow("pageshow", { persisted: true });
await flushMicrotasks();
assert(metricsRequests === 2, "persisted pageshow did not refresh metrics");
assert(statusRequests === 2, "persisted pageshow did not refresh status");
assert(clientCheckRequests === 2, "persisted pageshow did not refresh client check");
document.visibilityState = "hidden";
context.dispatchWindow("online");
await flushMicrotasks();
assert(metricsRequests === 2 && statusRequests === 2 && clientCheckRequests === 2, "hidden online event refreshed the dashboard");
document.visibilityState = "visible";
context.dispatchWindow("offline");
assert(document.getElementById("statusText").textContent === "Offline", "offline event did not mark dashboard offline");
assert(document.getElementById("statusDot").classList.contains("bad"), "offline event did not mark status as bad");
const beaconsBeforePageHide = beaconRequests;
context.dispatchWindow("pagehide");
assert(beaconRequests === beaconsBeforePageHide + 1, "pagehide did not send beacon client check");
assert(lastBeaconPath === "/api/client-check", "pagehide client check beacon used wrong path");

const healthyClientCheckFetch = context.fetch;
context.fetch = async (path, options = {}) => {
  if (path === "/api/client-check" && options.method === "POST") {
    clientCheckRequests += 1;
    return response({ error: "same-origin required" }, 403);
  }
  return healthyClientCheckFetch(path, options);
};
await context.sendClientCheck();
assert(document.getElementById("issuesPanel").hidden === false, "failed client check did not show issues panel");
assert(document.getElementById("issuesSummary").textContent === "1 issue", "failed client check issue count did not render");
assert(document.getElementById("issuesList").children[0].textContent === "client check unavailable: HTTP 403", "failed client check issue did not render");
context.fetch = healthyClientCheckFetch;
await context.sendClientCheck();
assert(document.getElementById("issuesPanel").hidden === true, "successful client check did not clear client check issue");

const pendingMetrics = deferredResponse(sampleMetrics());
metricsRequests = 0;
context.fetch = async (path) => {
  if (path === "/api/metrics") {
    metricsRequests += 1;
    return pendingMetrics.promise;
  }
  if (path === "/api/status") {
    return response(sampleStatus());
  }
  return response(settings);
};
const firstFetch = context.fetchMetrics();
const secondFetch = context.fetchMetrics();
await flushMicrotasks();
assert(metricsRequests === 1, "overlapping metrics polls were not coalesced");
pendingMetrics.resolve();
await Promise.all([firstFetch, secondFetch]);
await flushMicrotasks();

const originalAbortController = context.AbortController;
context.AbortController = undefined;
context.fetch = async () => new Promise(() => {});
const fallbackTimeout = context.fetchWithTimeout("/api/metrics", { cache: "no-store" }, 25)
  .then(
    () => "resolved",
    (error) => error.message,
  );
runTimeouts();
assert(await fallbackTimeout === "Request timed out", "fetch timeout fallback did not reject without AbortController");

let abortCalls = 0;
context.AbortController = class {
  constructor() {
    this.signal = { ignored: true };
  }

  abort() {
    abortCalls += 1;
  }
};
context.fetch = async () => new Promise(() => {});
const abortTimeout = context.fetchWithTimeout("/api/metrics", { cache: "no-store" }, 25)
  .then(
    () => "resolved",
    (error) => error.message,
  );
runTimeouts();
assert(await abortTimeout === "Request timed out", "fetch timeout race did not reject when abort was ignored");
assert(abortCalls === 1, "fetch timeout did not call AbortController.abort");
context.AbortController = originalAbortController;

console.log("ok: dashboard runtime smoke test passed");
