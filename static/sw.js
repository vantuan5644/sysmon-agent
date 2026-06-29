const STATIC_CACHE = "sysmon-static-v111";
const STATIC_ASSETS = ["/", "/styles.css", "/app.js", "/manifest.json", "/icon.svg", "/icon-180.png", "/icon-512.png"];
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
