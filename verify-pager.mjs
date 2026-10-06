#!/usr/bin/env node
// verify-pager.mjs -- geometry regression guard for the swipe pager layout.
//
// Why this exists as its own verifier: the pager's contract is purely a LAYOUT
// invariant, and the two existing verifiers are both blind to it.
// verify-dashboard.mjs runs app.js against a DOM mock with no geometry at all,
// and verify-render.mjs screenshots its own hand-written fixture rather than the
// real static/index.html. So a change that leaves every expected CSS token in
// place can still break the layout completely -- which is exactly what happened:
// `.page { height: 100% }` silently resolved to `auto` because `.shell` only had
// min-height, never a definite height. The pager then sized itself to the
// TALLEST page, the document scrolled, and pages 1 and 2 ended up with hundreds
// of pixels of blank space below them. Source-string assertions cannot catch
// that class of bug; only measuring the rendered box tree can.
//
// It loads the REAL static/index.html + static/styles.css in a headless
// Chromium, fills page 3 with a realistic row count, and asserts the three
// invariants that define the layout:
//
//   1. the document itself does not scroll (the shell is pinned to the viewport)
//   2. every page has the same height (the pager's height, not its own content)
//   3. an over-tall page scrolls INTERNALLY instead of growing the shell
//
// No socket is opened: the fixture is loaded over file:// with the stylesheet
// href rewritten to a relative path (index.html ships an absolute "/styles.css",
// which silently 404s under file:// and would make this verifier measure an
// unstyled page -- a trap worth naming, since it produces plausible numbers).
//
// Chromium-family only (it needs --dump-dom); degrades to a skip when no such
// browser is present, matching verify-render.mjs's convention.

import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const scriptDir = dirname(fileURLToPath(import.meta.url));
const tempRoot = mkdtempSync(join(tmpdir(), "sysmon-pager-"));

// Row count for page 3. processes.go sends up to processTopHardCap (30) rows, so
// this reproduces the worst case the dashboard actually renders.
const processRowCount = 30;
const viewport = { width: Number(process.env.SYSMON_LAYOUT_WIDTH || 390), height: Number(process.env.SYSMON_LAYOUT_HEIGHT || 667) };

try {
  const fixturePath = writeFixture(tempRoot);
  const browser = findChromiumBrowser();
  if (!browser) {
    console.log("ok: pager layout smoke skipped (no chromium-family headless browser available)");
  } else {
    const measured = measure(browser, fixturePath);
    if (measured.unsized) {
      // Not a failure: the browser never sized its window, so there is nothing
      // meaningful to assert. Fall through to cleanup rather than exiting here,
      // so the temp fixture is still removed.
      console.log(
        `ok: pager layout smoke skipped (${browser.name} reported a zero-height viewport; it did not size its headless window)`,
      );
    } else {
      assertPagerInvariants(measured);
      console.log(
        `ok: pager layout verified with ${browser.name} ` +
          `(viewport ${measured.viewportWidth}x${measured.viewport}px, pager ${measured.pagerHeight}px, ` +
          `${measured.pages.length} pages, tallest content ${Math.max(...measured.pages.map((p) => p.contentHeight))}px)`,
      );
    }
  }
} finally {
  if (process.env.SYSMON_KEEP_RENDER_ARTIFACTS === "1") {
    console.log(`pager artifacts kept in ${tempRoot}`);
  } else {
    rmSync(tempRoot, { recursive: true, force: true });
  }
}

// writeFixture builds a self-contained copy of the real dashboard shell:
// index.html with app.js/manifest/service-worker removed (so nothing reaches the
// network and no SSE stream keeps the page alive), the stylesheet href made
// relative, and a probe script appended that populates the pages and reports the
// measured box tree through a #PROBE element.
function writeFixture(root) {
  mkdirSync(root, { recursive: true, mode: 0o700 });
  writeFileSync(join(root, "styles.css"), readFileSync(join(scriptDir, "static", "styles.css")));
  // The stylesheet's @font-face url is relative, so the dashboard font loads
  // here too and the gauge-text checks measure the real glyphs.
  mkdirSync(join(root, "fonts"));
  writeFileSync(join(root, "fonts", "jetbrains-mono.woff2"), readFileSync(join(scriptDir, "static", "fonts", "jetbrains-mono.woff2")));

  let html = readFileSync(join(scriptDir, "static", "index.html"), "utf8");
  html = html.replace(/<script[^>]*app\.js[^>]*>\s*<\/script>/g, "");
  html = html.replace(/<link[^>]*manifest[^>]*>/g, "");
  // index.html references the stylesheet absolutely; under file:// that resolves
  // to the filesystem root and loads nothing.
  html = html.replace(/href="\/styles\.css"/g, 'href="styles.css"');
  if (!html.includes('href="styles.css"')) {
    throw new Error("fixture did not rewrite the stylesheet href; index.html markup changed");
  }
  html = html.replace("</body>", `${probeScript()}</body>`);

  const fixturePath = join(root, "pager.html");
  writeFileSync(fixturePath, html);
  return fixturePath;
}

function probeScript() {
  return `
<script>
window.addEventListener("load", function () {
  // Populate page 3 the way renderApps() does, at the worst-case row count.
  var body = document.getElementById("processesBody");
  for (var i = 0; i < ${processRowCount}; i++) {
    var row = document.createElement("div");
    row.className = "processes-row";
    row.innerHTML =
      '<span class="processes-cell">process-' + i + '</span>' +
      '<span class="processes-cell">1.0%</span>' +
      '<span class="processes-cell">100 MB</span>' +
      '<span class="processes-cell">&mdash;</span>' +
      '<span class="processes-cell">0 B/s</span>';
    body.appendChild(row);
  }
  // Page 2 carries the storage panel, which renderStorage() un-hides.
  var storage = document.getElementById("storagePanel");
  if (storage) {
    storage.hidden = false;
  }
  // Page 2 also carries the alerts panel (moved off the fixed shell chrome),
  // which renderMetricAlerts() un-hides and fills at the collapsed row count.
  // Same fixture contract as the quota page: a conditional element left hidden
  // measures 0px, so un-hide and FILL it before measuring.
  var alertsPanel = document.getElementById("alertsPanel");
  if (alertsPanel) {
    alertsPanel.hidden = false;
    var alertsList = document.getElementById("alertsList");
    for (var a = 0; a < 5; a++) {
      var alertRow = document.createElement("div");
      alertRow.className = "row alert-row";
      alertRow.textContent = "Motherboard 75\u00B0C over 70\u00B0C";
      alertsList.appendChild(alertRow);
    }
  }
  var alertsChip = document.getElementById("alertsChip");
  if (alertsChip) {
    alertsChip.hidden = false;
  }
  // Page 4 is conditional and each provider is conditional within it. Un-hide
  // both providers and populate quota rows plus seven-day charts so this probes
  // the real worst-case AI usage layout.
  var quotaPage = document.getElementById("quotaPage");
  if (quotaPage) {
    quotaPage.hidden = false;
    var claudeUsage = document.getElementById("claudeUsage");
    var codexUsage = document.getElementById("codexUsage");
    claudeUsage.hidden = false;
    codexUsage.hidden = false;
    var quotaList = document.getElementById("quotaList");
    for (var q = 0; q < 4; q++) {
      var quotaRow = document.createElement("div");
      quotaRow.className = "quota-row";
      quotaRow.innerHTML =
        '<div class="quota-row-head"><span class="quota-label">Weekly (model ' + q + ')</span>' +
        '<span class="quota-pct">' + (q * 20) + '%</span></div>' +
        '<div class="quota-bar"><span class="quota-bar-fill"></span></div>' +
        '<div class="quota-note">resets in 3d</div>';
      quotaList.appendChild(quotaRow);
    }
    var codexList = document.getElementById("codexQuotaList");
    var codexRow = document.createElement("div");
    codexRow.className = "quota-row";
    codexRow.innerHTML =
      '<div class="quota-row-head"><span class="quota-label">Weekly</span>' +
      '<span class="quota-pct">18%</span></div>' +
      '<div class="quota-bar"><span class="quota-bar-fill"></span></div>' +
      '<div class="quota-note">resets in 6d</div>';
    codexList.appendChild(codexRow);
    for (var chartId of ["claudeTokenChart", "codexTokenChart"]) {
      var chart = document.getElementById(chartId);
      for (var d = 0; d < 7; d++) {
        var day = document.createElement("div");
        day.className = "token-day";
        day.innerHTML =
          '<span class="token-day-value">' + (d + 1) + '.2M</span>' +
          '<span class="token-day-track"><span class="token-day-bar"></span></span>' +
          '<span class="token-day-label">Mon</span>';
        chart.appendChild(day);
      }
    }
  }
  var quotaDot = document.getElementById("pageDot3");
  if (quotaDot) {
    quotaDot.hidden = false;
  }

  // The widest string each gauge center can show (CPU, GPU, RAM, NET order).
  var gaugeSamples = ["100%", "100%", "100%", "\u219399.9M"];
  var gaugeSubSamples = ["4.7/5.5GHz", "23.9/24.0G", "1000/1024G", "\u219199.9M"];
  Array.from(document.querySelectorAll(".gauge")).forEach(function (gauge, index) {
    gauge.querySelector(".gauge-value").textContent = gaugeSamples[index];
    gauge.querySelector(".gauge-sub").textContent = gaugeSubSamples[index];
  });
  document.getElementById("gpuName").textContent = "NVIDIA GeForce RTX 4090 Founders Edition";
  var gpuDetail = document.getElementById("gpuDetail");
  gpuDetail.innerHTML = '49\u00b0C \u00b7 112 W<span class="gpu-hotspot-detail"> \u00b7 Hotspot 82\u00b0C</span>';
  var trends = ["gpuTrend", "memTrend", "netTrend"].map(function (id) {
    var trend = document.getElementById(id);
    for (var i = 0; i < 24; i++) {
      var bar = document.createElement("span");
      bar.className = "sparkline-bar";
      bar.style.setProperty("--h", "6%");
      trend.appendChild(bar);
    }
    return trend;
  });
  document.getElementById("cpuDetail").textContent = "58 C / 60 W";
  var cores = document.getElementById("cpuCores");
  cores.innerHTML = '<span class="core-grid-label">Busy Threads 0/32</span><span class="core-grid-bars">' +
    '<span class="core-bar" style="--h:6%"></span>'.repeat(32) + '</span>';
  var chartRows = [cores].concat(trends);
  var shell = document.querySelector(".shell");
  var pager = document.getElementById("pager");
  var pages = Array.prototype.slice.call(document.querySelectorAll(".page"));
  var alertsPanelEl = document.getElementById("alertsPanel");
  var alertsChipEl = document.getElementById("alertsChip");
  var cpuReading = document.createRange();
  cpuReading.selectNodeContents(document.getElementById("cpuDetail"));
  var report = {
    gauges: Array.from(document.querySelectorAll(".metric-card .gauge")).map(function (gauge) {
      var rect = gauge.getBoundingClientRect();
      var center = gauge.querySelector(".gauge-center");
      var centerRect = center.getBoundingClientRect();
      var value = gauge.querySelector(".gauge-value");
      var sub = gauge.querySelector(".gauge-sub");
      return { top: rect.top, bottom: rect.bottom, diameter: rect.width,
        centerWidth: centerRect.width, centerHeight: centerRect.height,
        valueWidth: value.clientWidth, valueContentWidth: value.scrollWidth,
        subWidth: sub.clientWidth, subContentWidth: sub.scrollWidth,
        cardTop: gauge.closest(".metric-card").getBoundingClientRect().top,
        labelTop: gauge.nextElementSibling.getBoundingClientRect().top };
    }),
    cpuBusyTop: cores.firstElementChild.getBoundingClientRect().top,
    cpuBusyBottom: cores.firstElementChild.getBoundingClientRect().bottom,
    cpuBarsTop: cores.lastElementChild.getBoundingClientRect().top,
    cpuReadingBottom: cpuReading.getBoundingClientRect().bottom,
    chartRows: chartRows.map(function (chart) {
      var rect = chart.getBoundingClientRect();
      var card = chart.closest(".metric-card").getBoundingClientRect();
      return { id: chart.id, height: rect.height, top: rect.top, cardTop: card.top,
        bottom: rect.bottom, cardBottom: card.bottom,
        pageBottom: chart.closest(".page").getBoundingClientRect().bottom,
        pageHeight: chart.closest(".page").clientHeight,
        pageContentHeight: chart.closest(".page").scrollHeight };
    }),
    trends: trends.map(function (trend) {
      var detail = trend.previousElementSibling;
      var rect = trend.getBoundingClientRect();
      return { id: trend.id, height: rect.height,
        top: rect.top, detailBottom: detail.getBoundingClientRect().bottom,
        bars: Array.from(trend.children).map(function (bar) { return bar.getBoundingClientRect().height; }) };
    }),
    cssLoaded: getComputedStyle(shell).display === "flex",
    gpuDetailWidth: gpuDetail.clientWidth,
    gpuDetailContentWidth: gpuDetail.scrollWidth,
    gpuDetailHeight: gpuDetail.clientHeight,
    gpuDetailContentHeight: gpuDetail.scrollHeight,
    hotspotColor: getComputedStyle(gpuDetail.querySelector(".gpu-hotspot-detail")).color,
    neutralColor: getComputedStyle(shell).getPropertyValue("--muted").trim(),
    viewport: window.innerHeight,
    viewportWidth: window.innerWidth,
    documentScrollHeight: document.documentElement.scrollHeight,
    shellHeight: shell.offsetHeight,
    pagerHeight: pager.offsetHeight,
    alertsPanelInPager: !!(alertsPanelEl && alertsPanelEl.closest(".page")),
    alertsChipInPager: !!(alertsChipEl && alertsChipEl.closest("#pager")),
    pages: pages.map(function (page) {
      return {
        height: page.offsetHeight,
        contentHeight: page.scrollHeight,
        scrollsInternally: page.scrollHeight > page.clientHeight + 1,
        hidden: page.hidden,
      };
    }),
  };
  var node = document.createElement("div");
  node.id = "PROBE";
  node.textContent = JSON.stringify(report);
  document.body.appendChild(node);
});
</script>
`;
}

// measure runs the fixture and returns the probe report. Chrome intermittently
// fails to apply --window-size before the load event fires, producing a
// zero-height viewport where every box measures 0 -- observed as a genuine race,
// not a property of the fixture or the flags (identical inputs alternate between
// 0 and the correct height). Retry rather than let that flakiness read as a
// layout regression; only report "unsized" if it never sizes.
function measure(browser, fixturePath) {
  let report = null;
  for (let attempt = 0; attempt < 4; attempt++) {
    report = measureOnce(browser, fixturePath);
    if (report.viewport > 0) {
      return report;
    }
  }
  report.unsized = true;
  return report;
}

function measureOnce(browser, fixturePath) {
  const profile = join(tempRoot, "profile");
  const result = spawnSync(
    browser.binary,
    [
      // --headless=new explicitly: the bare --headless alias does not honour
      // --window-size for file:// documents on some builds and reports a
      // zero-height viewport, which would make every measurement meaningless.
      "--headless=new",
      "--disable-gpu",
      "--no-sandbox",
      // Without these, Chrome's first-run setup on a COLD profile delays window
      // sizing past load: the very first run reports a zero-height viewport (and
      // every box measures 0) while re-runs against the warm profile succeed.
      // That intermittency would read as a flaky layout test.
      "--no-first-run",
      "--no-default-browser-check",
      `--window-size=${viewport.width},${viewport.height}`,
      "--virtual-time-budget=5000",
      `--user-data-dir=${profile}`,
      "--dump-dom",
      `file://${fixturePath}`,
    ],
    { encoding: "utf8", maxBuffer: 32 * 1024 * 1024 },
  );
  if (result.status !== 0) {
    throw new Error(`headless run failed: ${(result.stderr || "").slice(0, 400)}`);
  }
  // Match the probe ELEMENT, not the probe script's own source text.
  const match = /<div id="PROBE">([\s\S]*?)<\/div>/.exec(result.stdout || "");
  if (!match) {
    throw new Error("probe element missing; the page did not finish loading");
  }
  const report = JSON.parse(decodeEntities(match[1]));
  if (!report.cssLoaded) {
    throw new Error("stylesheet did not apply; the measurement would be meaningless");
  }
  return report;
}

function decodeEntities(text) {
  return text
    .replace(/&quot;/g, '"')
    .replace(/&#34;/g, '"')
    .replace(/&amp;/g, "&")
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">");
}

function assertPagerInvariants(m) {
  for (const gauge of m.gauges) {
    if (gauge.valueContentWidth > gauge.valueWidth + 1 || gauge.subContentWidth > gauge.subWidth + 1 ||
        gauge.centerWidth > gauge.diameter * 0.68 + 1 || gauge.centerHeight > gauge.diameter * 0.5 + 1) {
      throw new Error("Gauge text exceeds the center: " + JSON.stringify(gauge));
    }
  }
  if (m.gauges.some((gauge) => gauge.top < gauge.cardTop || gauge.bottom > gauge.labelTop)) {
    throw new Error("Gauge is clipped or overlaps its label: " + JSON.stringify(m.gauges));
  }
  if (m.cpuBusyBottom > m.cpuBarsTop || m.cpuBusyTop < m.cpuReadingBottom) {
    throw new Error("Busy Threads label overlaps CPU readings or bars");
  }
  for (const chart of m.chartRows) {
    if (Math.abs(chart.height - 24) > 0.5 || chart.bottom > chart.cardBottom || chart.bottom > chart.pageBottom || chart.pageContentHeight > chart.pageHeight + 1) {
      throw new Error(`chart row has incorrect height or exceeds its card: ${JSON.stringify(chart)}`);
    }
    for (const other of m.chartRows) {
      if (Math.abs(chart.cardTop - other.cardTop) < 1 && Math.abs(chart.top - other.top) > 1) {
        throw new Error(`chart rows are misaligned: ${JSON.stringify(m.chartRows)}`);
      }
    }
  }
  for (const trend of m.trends) {
    if (trend.bars.length !== 24 || trend.height < 12 || trend.top < trend.detailBottom || trend.bars.some((height) => height < 3)) {
      throw new Error(`${trend.id} history is missing, squeezed, or overlaps its details: ${JSON.stringify(trend)}`);
    }
  }
  if (m.gpuDetailContentWidth > m.gpuDetailWidth + 1 || m.gpuDetailContentHeight > m.gpuDetailHeight + 1) {
    throw new Error("GPU hotspot detail is clipped or overflows its card");
  }
  const detail =
    `viewport=${m.viewport} document=${m.documentScrollHeight} shell=${m.shellHeight} pager=${m.pagerHeight} ` +
    `pages=${JSON.stringify(m.pages)}`;

  // 1. The shell is pinned to the viewport, so the document never scrolls. This
  // is the invariant that broke: a content-sized shell is what pushed the page
  // dots below the fold and left blank space under the short pages.
  if (m.documentScrollHeight > m.viewport + 1) {
    throw new Error(
      `document scrolls (${m.documentScrollHeight}px > ${m.viewport}px viewport): the shell is not pinned to the viewport. ${detail}`,
    );
  }

  if (m.pages.length < 2) {
    throw new Error(`expected at least 2 pager pages, found ${m.pages.length}. ${detail}`);
  }

  // A conditional page left hidden measures 0px and would fail the fill assert
  // below as a bogus layout bug. The fixture above must un-hide every
  // conditional page before measuring; this guard turns the failure into the
  // actual diagnosis.
  for (const [index, page] of m.pages.entries()) {
    if (page.hidden) {
      throw new Error(
        `page ${index} is hidden; the fixture must un-hide every conditional page before measuring. ${detail}`,
      );
    }
  }

  // 2. Every page is the pager's height -- never its own content height. A page
  // shorter than the pager means align-items is not stretching them; a page
  // taller means it is inflating the pager instead of scrolling.
  for (const [index, page] of m.pages.entries()) {
    if (Math.abs(page.height - m.pagerHeight) > 1) {
      throw new Error(
        `page ${index} is ${page.height}px but the pager is ${m.pagerHeight}px; pages must fill the pager, not size to their content. ${detail}`,
      );
    }
  }

  // 3. The tall page must scroll inside itself. Without this the row list is
  // simply clipped, which is a different bug wearing the same numbers.
  const tallest = m.pages.reduce((a, b) => (b.contentHeight > a.contentHeight ? b : a));
  if (tallest.contentHeight <= m.pagerHeight) {
    throw new Error(
      `no page overflows the pager, so this run proves nothing; raise processRowCount. ${detail}`,
    );
  }
  if (!tallest.scrollsInternally) {
    throw new Error(
      `the tallest page (${tallest.contentHeight}px of content in a ${m.pagerHeight}px pager) does not scroll internally; its content is being clipped. ${detail}`,
    );
  }

  // 4. The alerts detail lives INSIDE a pager page ("More status") so a
  // populated alert list scrolls with its page instead of shrinking the pager
  // on every page, while the chip linking to it stays OUTSIDE the pager,
  // riding the status-strip row in the shell chrome. This is the measured
  // regression guard for the alerts-chip move -- the string tests can only
  // prove the markup moved, this proves the layout followed.
  if (!m.alertsPanelInPager) {
    throw new Error(
      `#alertsPanel is not inside a .page; the alerts detail must scroll with the "More status" page instead of permanently shrinking the pager. ${detail}`,
    );
  }
  if (m.alertsChipInPager) {
    throw new Error(
      `#alertsChip is inside the pager; it must stay outside it, riding the status-strip row. ${detail}`,
    );
  }
}

function findChromiumBrowser() {
  const candidates = [
    process.env.CHROME_BIN,
    "chromium",
    "chromium-browser",
    "google-chrome",
    "google-chrome-stable",
    "brave-browser",
  ].filter(Boolean);

  for (const candidate of candidates) {
    const binary = candidate.includes("/") ? resolve(candidate) : candidate;
    if (candidate.includes("/") && !existsSync(binary)) {
      continue;
    }
    const probe = spawnSync(binary, ["--version"], { encoding: "utf8" });
    if (probe.status === 0) {
      return { name: candidate, binary };
    }
  }
  return null;
}
