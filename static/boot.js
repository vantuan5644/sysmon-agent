// boot.js is the dashboard's first script, loaded before app.js and kept tiny
// and independent of it. If app.js fails to load, fails to parse or throws
// before it starts, the page would otherwise sit empty with no hint why; this
// shows the #scriptError banner instead. Once app.js runs it reports its own
// errors to the same banner and sets window.sysmonScriptErrors, and the boot
// handlers here step back.
(() => {
  // Armed at DOMContentLoaded, which waits for app.js to download and run, so
  // a slow connection does not count against it.
  const bootTimeoutMS = 10000;

  function showBootError(detail) {
    const banner = document.getElementById("scriptError");
    const text = document.getElementById("scriptErrorText");
    if (!banner || !text) {
      // Too early (the body is not parsed yet): show it once it is.
      document.addEventListener("DOMContentLoaded", () => showBootError(detail), { once: true });
      return;
    }
    const message = `Dashboard script error: ${detail}`;
    if (!banner.hidden) {
      // One banner: the first error stays, later ones only add to the count.
      if (banner.dataset.message !== message) {
        const more = Number(banner.dataset.more || 0) + 1;
        banner.dataset.more = String(more);
        const count = document.getElementById("scriptErrorMore");
        if (count) {
          count.textContent = ` (+${more} more)`;
        }
      }
      return;
    }
    banner.dataset.message = message;
    text.textContent = `${message} — `;
    const reload = document.getElementById("scriptErrorReload");
    if (reload && !reload.dataset.wired) {
      reload.dataset.wired = "1";
      reload.addEventListener("click", () => window.location.reload());
    }
    banner.hidden = false;
  }

  function errorDetail(error) {
    if (error && typeof error.message === "string" && error.message) {
      return error.message;
    }
    return String(error || "unknown error").slice(0, 200);
  }

  // Capture phase: a script that fails to load fires "error" on its element,
  // which does not bubble to window.
  window.addEventListener("error", (event) => {
    const target = event?.target;
    if (target && target !== window && target.tagName === "SCRIPT") {
      showBootError(`could not load ${target.getAttribute("src") || "a script"}`);
      return;
    }
    if (window.sysmonScriptErrors || !event || target?.tagName) {
      return;
    }
    if (event.message === "Script error." && !event.error) {
      // Another origin's script (an extension): the browser hides the detail.
      return;
    }
    showBootError(event.message || errorDetail(event.error));
  }, true);

  window.addEventListener("unhandledrejection", (event) => {
    if (!window.sysmonScriptErrors) {
      showBootError(errorDetail(event?.reason));
    }
  });

  document.addEventListener("DOMContentLoaded", () => {
    setTimeout(() => {
      if (!window.sysmonDashboardStarted) {
        showBootError("the dashboard did not start");
      }
    }, bootTimeoutMS);
  });
})();
