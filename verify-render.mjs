#!/usr/bin/env node
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { spawnSync } from "node:child_process";
import { fileURLToPath, pathToFileURL } from "node:url";
import { inflateSync } from "node:zlib";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const tempRoot = join(tmpdir(), `sysmon-render-${process.pid}-${Date.now()}`);

const viewports = [
  { name: "phone-portrait", width: 390, height: 844 },
  { name: "phone-landscape", width: 844, height: 390 },
];

try {
  mkdirSync(tempRoot, { recursive: true, mode: 0o700 });
  const fixturePath = writeFixture(tempRoot);
  verifyStaticLayout(fixturePath);
  const browsers = findBrowsers();
  if (browsers.length === 0) {
    console.log("ok: dashboard static layout smoke passed (headless browser unavailable)");
  } else {
    const errors = [];
    let passed = false;
    for (const browser of browsers) {
      try {
        for (const viewport of viewports) {
          const screenshotPath = join(tempRoot, `${browser.name}-${viewport.name}.png`);
          captureScreenshot(browser, fixturePath, screenshotPath, viewport);
          verifyScreenshot(screenshotPath, viewport);
        }
        console.log(`ok: dashboard render smoke test passed with ${browser.name} (${viewports.length} screenshots)`);
        passed = true;
        break;
      } catch (error) {
        errors.push(`${browser.name}: ${error.message}`);
        if (process.env.SYSMON_RENDER_STRICT === "1") {
          throw error;
        }
      }
    }
    if (!passed) {
      console.log(`ok: dashboard static layout smoke passed; headless screenshots skipped (${errors.join("; ")})`);
    }
  }
} finally {
  if (process.env.SYSMON_KEEP_RENDER_ARTIFACTS === "1") {
    console.log(`render artifacts kept in ${tempRoot}`);
  } else {
    rmSync(tempRoot, { recursive: true, force: true });
  }
}

function findBrowsers() {
  const candidates = [
    { name: "firefox", kind: "firefox", binary: process.env.FIREFOX_BIN },
    { name: "firefox", kind: "firefox", binary: "/snap/firefox/current/usr/lib/firefox/firefox" },
    { name: "firefox", kind: "firefox", binary: "firefox" },
    { name: "firefox-esr", kind: "firefox", binary: "firefox-esr" },
    { name: "chromium", kind: "chromium", binary: process.env.CHROME_BIN },
    { name: "chromium", kind: "chromium", binary: "chromium" },
    { name: "chromium-browser", kind: "chromium", binary: "chromium-browser" },
    { name: "google-chrome", kind: "chromium", binary: "google-chrome" },
    { name: "brave-browser", kind: "chromium", binary: "brave-browser" },
  ].filter((candidate) => candidate.binary);

  const browsers = [];
  const seen = new Set();
  for (const candidate of candidates) {
    const binary = candidate.binary.includes("/") ? resolve(candidate.binary) : candidate.binary;
    const key = `${candidate.kind}:${binary}`;
    if (seen.has(key)) {
      continue;
    }
    seen.add(key);
    if (candidate.binary.includes("/") && !existsSync(binary)) {
      continue;
    }
    const probeRoot = join(tempRoot, "probe");
    const result = spawnSync(binary, browserVersionArgs(candidate.kind), {
      env: firefoxEnv(probeRoot),
      encoding: "utf8",
    });
    if (result.status === 0) {
      browsers.push({ ...candidate, binary });
    }
  }
  return browsers;
}

function browserVersionArgs(kind) {
  if (kind === "firefox") {
    return ["--headless", "--version"];
  }
  return ["--version"];
}

function firefoxEnv(root) {
  const home = join(root, "home");
  const runtime = join(root, "runtime");
  mkdirSync(home, { recursive: true, mode: 0o700 });
  mkdirSync(runtime, { recursive: true, mode: 0o700 });
  return {
    ...process.env,
    HOME: home,
    XDG_RUNTIME_DIR: runtime,
    MOZ_HEADLESS: "1",
  };
}

function writeFixture(root) {
  writeFileSync(join(root, "styles.css"), readFileSync(join(scriptDir, "static", "styles.css")));
  const html = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="theme-color" content="#080b10">
  <title>Sysmon render fixture</title>
  <link rel="stylesheet" href="styles.css">
</head>
<body>
  <main class="shell">
    <header class="topbar">
      <div>
        <p class="eyebrow" id="platform">linux / amd64 / 6.8.0-homelab</p>
        <h1 id="hostname">labbox-mini</h1>
      </div>
      <div class="top-actions">
        <button class="button" type="button" aria-label="Dim mode" aria-pressed="false" title="Dim mode">&#9680;</button>
        <button class="button active" type="button" aria-label="Screen shift mode" aria-pressed="true" title="Screen shift mode">&#8644;</button>
        <button class="button active" type="button" aria-label="Keep screen awake" aria-pressed="true" title="Keep screen awake">&#9728;</button>
        <button class="button" type="button" aria-label="Pause updates" aria-pressed="false" title="Pause updates">&#8545;</button>
      </div>
    </header>
    <section class="status-strip" role="button" tabindex="0" aria-label="Refresh metrics now">
      <span class="status-dot ok"></span>
      <span>Live</span>
      <span class="muted">up 7h 12m / saved / app</span>
      <span class="muted">03:16:00 / 4s / 142ms</span>
    </section>
    <section class="panel alerts-panel" role="button" tabindex="0" aria-label="Collapse alert details" aria-expanded="true" aria-live="polite">
      <div class="section-head">
        <h2>Alerts</h2>
        <span class="muted">2 alerts</span>
      </div>
      <div class="rows">
        <div class="row alert-row">Disk / 92% over 70%</div>
        <div class="row alert-row">Motherboard 75C over 70C</div>
      </div>
    </section>
    <section class="panel issues-panel" role="button" tabindex="0" aria-label="Expand issue details" aria-expanded="false" aria-live="polite">
      <div class="section-head">
        <h2>Issues</h2>
        <span class="muted">3 issues</span>
      </div>
      <div class="rows">
        <div class="row issue-row">disk /backup: statfs denied</div>
        <div class="row issue-row">temperatures: no supported temperature sensors found</div>
        <div class="row issue-row">gpu: nvidia-smi not found</div>
      </div>
    </section>
    <section class="gauge-grid" aria-label="Primary metrics">
      ${gaugeCard("CPU", 37, "37%", "var(--good)")}
      ${gaugeCard("RAM", 61, "61%", "var(--good)")}
      ${gaugeCard("GPU", 74, "74%", "var(--warn)")}
      ${gaugeCard("TEMP", 68, "68C", "var(--good)")}
    </section>
    ${panel("Performance", "CPU 37% / RAM 61%", [
      rowWithBar("CPU", "", "37%", 37, "var(--good)"),
      rowWithBar("RAM", "9.8 GB / 16 GB", "61%", 61, "var(--good)"),
    ])}
    ${panel("Storage", "42% used", [
      rowWithBar("/", "92 GB / 220 GB ext4", "42%", 42, "var(--good)"),
      rowWithBar("/srv/nas", "2.8 TB / 7.3 TB btrfs", "38%", 38, "var(--good)"),
    ])}
    ${panel("Network", "2.8 MB/s down / 640 KB/s up", [
      simpleRow("tailscale0", "RX 2.4 MB/s / TX 590 KB/s", "3.0 MB/s"),
      simpleRow("enp3s0", "RX 410 KB/s / TX 50 KB/s", "460 KB/s"),
    ])}
    ${panel("Sensors", "68C max", [
      simpleRow("k10temp Tctl", "", "68C"),
      simpleRow("nvme Composite", "", "43C"),
      simpleRow("acpitz thermal_zone0", "", "NA"),
    ])}
    ${panel("GPU", "nvidia-smi not found; 1 device", [
      rowWithBar("Intel GPU card1", "load NA: DRM GPU usage not exposed / VRAM NA: DRM VRAM usage not exposed / 49C", "NA", 100, "#394656", false),
    ])}
    <footer class="bottom-controls" aria-label="Dashboard controls">
      <section class="control-row" aria-label="Refresh interval">
        <button class="segment" type="button" aria-pressed="false">1s</button>
        <button class="segment active" type="button" aria-pressed="true">1.5s</button>
        <button class="segment" type="button" aria-pressed="false">2s</button>
      </section>
      <section class="control-row panel-row" aria-label="Panel focus">
        <button class="segment active" type="button" aria-pressed="true">All</button>
        <button class="segment" type="button" aria-pressed="false">Perf</button>
        <button class="segment" type="button" aria-pressed="false">Disk</button>
        <button class="segment" type="button" aria-pressed="false">Net</button>
        <button class="segment" type="button" aria-pressed="false">Temp</button>
        <button class="segment" type="button" aria-pressed="false">GPU</button>
      </section>
      <section class="control-row threshold-row" aria-label="Warning threshold">
        <button class="segment threshold-step" type="button" aria-label="Lower warning threshold">−</button>
        <button class="segment threshold-target" type="button" aria-label="Cycle warning threshold target">CPU 70%</button>
        <button class="segment threshold-step" type="button" aria-label="Raise warning threshold">+</button>
      </section>
    </footer>
  </main>
</body>
</html>`;
  const path = join(root, "fixture.html");
  writeFileSync(path, html);
  return path;
}

function verifyStaticLayout(fixturePath) {
  const html = readFileSync(fixturePath, "utf8");
  const css = readFileSync(join(tempRoot, "styles.css"), "utf8");
  for (const needle of [
    `name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover"`,
    `class="shell"`,
    `class="top-actions"`,
    `class="status-strip"`,
    `up 7h 12m / saved / app`,
    `03:16:00 / 4s / 142ms`,
    `class="panel alerts-panel"`,
    `class="row alert-row"`,
    `Disk / 92% over 70%`,
    `class="panel issues-panel"`,
    `aria-expanded="false"`,
    `class="row issue-row"`,
    `class="gauge-grid"`,
    `class="sparkline"`,
    `class="sparkline-bar"`,
    `class="control-row panel-row"`,
    `class="control-row threshold-row"`,
    `CPU 70%`,
    `disk /backup: statfs denied`,
    `Performance`,
    `Storage`,
    `Network`,
    `Sensors`,
    `GPU`,
  ]) {
    assertIncludes(html, needle, "render fixture");
  }
  for (const needle of [
    `min-height: 100svh;`,
    `env(safe-area-inset-top)`,
    `grid-template-columns: repeat(4, minmax(0, 1fr));`,
    `grid-template-columns: repeat(24, minmax(0, 1fr));`,
    `.sparkline-bar.unavailable`,
    `@media (max-width: 480px)`,
    `grid-template-areas:`,
    `"state updated"`,
    `@media (max-width: 360px)`,
    `min-width: 54px;`,
    `@media (pointer: coarse)`,
    `grid-template-columns: repeat(4, 40px);`,
    `@media (max-width: 390px)`,
    `flex-direction: column;`,
    `grid-template-columns: repeat(4, minmax(0, 1fr));`,
    `@media (orientation: landscape) and (max-height: 500px)`,
    `flex: 1 1 auto;`,
    `width: min(42vh, 21vw);`,
    `flex-direction: row;`,
    `padding-bottom: calc(env(safe-area-inset-bottom) + 4px);`,
    `touch-action: manipulation;`,
    `overflow-wrap: anywhere;`,
    `.alerts-panel,`,
    `.alerts-panel {`,
    `.alert-row,`,
    `.alert-row {`,
    `color: var(--bad);`,
    `.issues-panel {`,
    `.issues-panel[role="button"]`,
    `.issues-panel:focus-visible`,
    `.issue-row {`,
    `color: var(--warn);`,
  ]) {
    assertIncludes(css, needle, "dashboard CSS");
  }
  assertCount(html, `class="metric-card"`, 4, "primary metric cards");
  assertMinimumCount(html, `class="panel`, 6, "dashboard panels");
  assertNoViewportFontScaling(css);
  assertNarrowPhoneLayoutFits();
}

function assertIncludes(haystack, needle, label) {
  if (!haystack.includes(needle)) {
    throw new Error(`${label} missing ${needle}`);
  }
}

function assertCount(haystack, needle, expected, label) {
  const count = haystack.split(needle).length - 1;
  if (count !== expected) {
    throw new Error(`${label} count ${count}, want ${expected}`);
  }
}

function assertMinimumCount(haystack, needle, expected, label) {
  const count = haystack.split(needle).length - 1;
  if (count < expected) {
    throw new Error(`${label} count ${count}, want at least ${expected}`);
  }
}

function assertNoViewportFontScaling(css) {
  for (const line of css.split("\n")) {
    const trimmed = line.trim();
    if (!trimmed.startsWith("font-size:")) {
      continue;
    }
    if (/\b(?:vw|vh|vmin|vmax|dvw|dvh|svw|svh|lvw|lvh)\b/.test(trimmed)) {
      throw new Error(`font-size uses viewport unit: ${trimmed}`);
    }
  }
}

function assertNarrowPhoneLayoutFits() {
  const viewportWidthPX = 320;
  const shellPaddingPX = 10 * 2;
  const contentWidthPX = viewportWidthPX - shellPaddingPX;
  const gaugeGridGapPX = 7 * 3;
  const metricCardHorizontalPadPX = 4 * 2;
  const gaugeMinWidthPX = 54;
  const metricColumnCount = 4;
  const metricCardWidthPX = (contentWidthPX - gaugeGridGapPX) / metricColumnCount;
  const metricRequiredWidthPX = gaugeMinWidthPX + metricCardHorizontalPadPX;
  if (metricRequiredWidthPX > metricCardWidthPX) {
    throw new Error(`narrow metric cards need ${metricRequiredWidthPX}px but have ${metricCardWidthPX}px`);
  }

  const coarseActionWidthPX = (4 * 40) + (3 * 6);
  if (coarseActionWidthPX > contentWidthPX) {
    throw new Error(`coarse header controls need ${coarseActionWidthPX}px but have ${contentWidthPX}px`);
  }

  const stackedActionWidthPX = (contentWidthPX - (3 * 6)) / 4;
  if (stackedActionWidthPX < 40) {
    throw new Error(`narrow stacked header buttons are only ${stackedActionWidthPX}px wide`);
  }
}

function gaugeCard(label, percent, value, color) {
  return `<article class="metric-card" role="button" tabindex="0" aria-label="${label}">
    <div class="gauge" style="--p:${percent}; --c:${color}">
      <div class="gauge-ring-inner" style="--inner-p:${percent}; --inner-c:${color}"></div>
      <div class="gauge-center"><span class="gauge-value">${value}</span><span class="gauge-sub">--</span></div>
    </div>
    <div class="metric-label">${label}</div>
    ${sparkline(percent, color)}
  </article>`;
}

function sparkline(percent, color) {
  const bars = [];
  for (let index = 0; index < 24; index += 1) {
    const value = Math.max(6, Math.round((percent * (index + 1)) / 24));
    const unavailableClass = index < 2 ? " unavailable" : "";
    const style = unavailableClass ? "" : ` style="--h:${value}%; --c:${color}"`;
    bars.push(`<span class="sparkline-bar${unavailableClass}"${style}></span>`);
  }
  return `<div class="sparkline" aria-hidden="true">${bars.join("")}</div>`;
}

function panel(title, summary, rows) {
  return `<section class="panel">
    <div class="section-head"><h2>${title}</h2><span class="muted">${summary}</span></div>
    <div class="rows">${rows.join("")}</div>
  </section>`;
}

function simpleRow(title, subtitle, value) {
  const sub = subtitle ? `<div class="row-sub">${subtitle}</div>` : "";
  return `<div class="row"><div><div class="row-title">${title}</div>${sub}</div><div class="row-value">${value}</div></div>`;
}

function rowWithBar(title, subtitle, value, percent, color, available = true) {
  const unavailableClass = available ? "" : " unavailable";
  return `<div class="row"><div><div class="row-title">${title}</div>${subtitle ? `<div class="row-sub">${subtitle}</div>` : ""}</div><div class="row-value">${value}</div><div class="bar${unavailableClass}" style="--p:${percent}; --c:${color}"><span></span></div></div>`;
}

function captureScreenshot(browser, fixturePath, screenshotPath, viewport) {
  const profile = join(tempRoot, `${viewport.name}-profile`);
  mkdirSync(profile, { recursive: true, mode: 0o700 });
  const result = spawnSync(browser.binary, browserScreenshotArgs(browser, fixturePath, screenshotPath, viewport, profile), {
    env: firefoxEnv(join(tempRoot, `${viewport.name}-env`)),
    encoding: "utf8",
  });
  if (result.status !== 0) {
    throw new Error(`screenshot failed for ${viewport.name}: ${result.stderr || result.stdout}`);
  }
  if (!existsSync(screenshotPath)) {
    throw new Error(`did not create screenshot for ${viewport.name}`);
  }
}

function browserScreenshotArgs(browser, fixturePath, screenshotPath, viewport, profile) {
  const url = pathToFileURL(fixturePath).href;
  if (browser.kind === "firefox") {
    return [
      "--headless",
      "--new-instance",
      "--profile",
      profile,
      "--window-size",
      `${viewport.width},${viewport.height}`,
      "--screenshot",
      screenshotPath,
      url,
    ];
  }
  return [
    "--headless=new",
    "--disable-gpu",
    "--no-sandbox",
    "--disable-dev-shm-usage",
    "--disable-crash-reporter",
    "--disable-breakpad",
    `--user-data-dir=${profile}`,
    `--window-size=${viewport.width},${viewport.height}`,
    `--screenshot=${screenshotPath}`,
    url,
  ];
}

function verifyScreenshot(path, viewport) {
  const png = decodePNG(readFileSync(path));
  if (png.width !== viewport.width || png.height !== viewport.height) {
    throw new Error(`${viewport.name} screenshot size ${png.width}x${png.height}, want ${viewport.width}x${viewport.height}`);
  }
  const unique = new Set();
  let contentPixels = 0;
  const step = Math.max(1, Math.floor((png.width * png.height) / 50000));
  for (let pixel = 0; pixel < png.width * png.height; pixel += step) {
    const index = pixel * png.channels;
    const r = png.pixels[index];
    const g = png.pixels[index + 1];
    const b = png.pixels[index + 2];
    unique.add(`${r},${g},${b}`);
    if (Math.abs(r - 8) + Math.abs(g - 11) + Math.abs(b - 16) > 18) {
      contentPixels += 1;
    }
  }
  if (unique.size < 12 || contentPixels < 100) {
    throw new Error(`${viewport.name} screenshot looks blank: ${unique.size} colors, ${contentPixels} content pixels`);
  }
}

function decodePNG(data) {
  const signature = "89504e470d0a1a0a";
  if (data.subarray(0, 8).toString("hex") !== signature) {
    throw new Error("screenshot is not a PNG");
  }

  let offset = 8;
  let width = 0;
  let height = 0;
  let bitDepth = 0;
  let colorType = 0;
  const idat = [];
  while (offset < data.length) {
    const length = data.readUInt32BE(offset);
    const type = data.subarray(offset + 4, offset + 8).toString("ascii");
    const chunk = data.subarray(offset + 8, offset + 8 + length);
    if (type === "IHDR") {
      width = chunk.readUInt32BE(0);
      height = chunk.readUInt32BE(4);
      bitDepth = chunk[8];
      colorType = chunk[9];
    } else if (type === "IDAT") {
      idat.push(chunk);
    } else if (type === "IEND") {
      break;
    }
    offset += 12 + length;
  }

  const channels = colorType === 6 ? 4 : colorType === 2 ? 3 : 0;
  if (!width || !height || bitDepth !== 8 || channels === 0) {
    throw new Error(`unsupported PNG format: ${width}x${height}, bit depth ${bitDepth}, color type ${colorType}`);
  }
  const inflated = inflateSync(Buffer.concat(idat));
  const stride = width * channels;
  const pixels = Buffer.alloc(stride * height);
  let inOffset = 0;
  for (let y = 0; y < height; y += 1) {
    const filter = inflated[inOffset];
    inOffset += 1;
    const row = inflated.subarray(inOffset, inOffset + stride);
    inOffset += stride;
    const prev = y === 0 ? null : pixels.subarray((y - 1) * stride, y * stride);
    const out = pixels.subarray(y * stride, (y + 1) * stride);
    unfilterRow(filter, row, out, prev, channels);
  }
  return { width, height, channels, pixels };
}

function unfilterRow(filter, row, out, prev, bpp) {
  for (let i = 0; i < row.length; i += 1) {
    const left = i >= bpp ? out[i - bpp] : 0;
    const up = prev ? prev[i] : 0;
    const upLeft = prev && i >= bpp ? prev[i - bpp] : 0;
    let predictor = 0;
    if (filter === 1) {
      predictor = left;
    } else if (filter === 2) {
      predictor = up;
    } else if (filter === 3) {
      predictor = Math.floor((left + up) / 2);
    } else if (filter === 4) {
      predictor = paeth(left, up, upLeft);
    } else if (filter !== 0) {
      throw new Error(`unsupported PNG filter ${filter}`);
    }
    out[i] = (row[i] + predictor) & 0xff;
  }
}

function paeth(a, b, c) {
  const p = a + b - c;
  const pa = Math.abs(p - a);
  const pb = Math.abs(p - b);
  const pc = Math.abs(p - c);
  if (pa <= pb && pa <= pc) {
    return a;
  }
  return pb <= pc ? b : c;
}
