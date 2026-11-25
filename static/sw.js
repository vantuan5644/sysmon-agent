const STATIC_CACHE = "sysmon-static-v179";
const STATIC_ASSETS = ["/", "/styles.css", "/boot.js", "/app.js", "/manifest.json", "/icon.svg", "/icon-180.png", "/icon-512.png", "/fonts/jetbrains-mono.woff2"];
const STATIC_ASSET_SET = new Set(STATIC_ASSETS);

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE)
      .then((cache) => cache.addAll(STATIC_ASSETS))
      .then(() => self.skipWaiting()),
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((key) => key !== STATIC_CACHE).map((key) => caches.delete(key))),
    ).then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const url = new URL(event.request.url);
  if (isLiveEndpoint(url)) {
    event.respondWith(fetch(event.request));
    return;
  }
  event.respondWith(
    fetch(event.request)
      .then((response) => {
        if (shouldCacheStaticRequest(event.request, url, response)) {
          const copy = response.clone();
          caches.open(STATIC_CACHE).then((cache) => cache.put(event.request, copy));
        }
        return response;
      })
      .catch(async () => {
        const cached = await caches.match(event.request);
        if (cached) {
          return cached;
        }
        if (event.request.mode === "navigate") {
          return caches.match("/");
        }
        return Response.error();
      }),
  );
});

function isLiveEndpoint(url) {
  return url.pathname.startsWith("/api/") || url.pathname === "/healthz" || url.pathname === "/readyz";
}

function shouldCacheStaticRequest(request, url, response) {
  return request.method === "GET" && response.ok && STATIC_ASSET_SET.has(url.pathname);
}

// Host alerts over Web Push (alerts_webpush.go). The agent encrypts one
// small JSON object per notice: title, body, tag, kind, host and url.
self.addEventListener("push", (event) => {
  let message = {};
  try {
    message = event.data ? event.data.json() : {};
  } catch {
    message = { body: event.data ? event.data.text() : "" };
  }
  const title = String(message.title || "sysmon-agent").slice(0, 120);
  const options = {
    body: String(message.body || "").slice(0, 600),
    tag: String(message.tag || "sysmon-alert").slice(0, 120),
    icon: "/icon-180.png",
    badge: "/icon-180.png",
    data: { url: sameOriginPath(message.url) },
    renotify: message.kind === "firing",
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const target = new URL(sameOriginPath(event.notification.data?.url), self.location.origin).href;
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((windows) => {
      for (const client of windows) {
        if (new URL(client.url).origin === self.location.origin && "focus" in client) {
          return client.focus();
        }
      }
      return self.clients.openWindow ? self.clients.openWindow(target) : undefined;
    }),
  );
});

// sameOriginPath keeps a notification's link on this agent.
function sameOriginPath(value) {
  const path = typeof value === "string" ? value : "/";
  return path.startsWith("/") && !path.startsWith("//") ? path : "/";
}
