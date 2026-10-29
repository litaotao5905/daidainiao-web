"use strict";
// Minimal service worker so browsers offer "install app".
//  - It caches ONE static page (offline.html) and nothing else: no API
//    responses, no tokens, no downloaded files, no app shell.
//  - Every request except top-level navigations is left alone (the browser
//    handles it as if there were no worker), so /api/*, OAuth (/callback) and
//    file downloads behave exactly as in a normal tab.
//  - A navigation that fails because the local server is not running shows
//    offline.html instead of the browser's error page.
const CACHE = "ft-offline-v2";
const OFFLINE = "/offline.html";

self.addEventListener("install", (event) => {
  event.waitUntil(caches.open(CACHE).then((c) => c.add(new Request(OFFLINE, { cache: "reload" }))).then(() => self.skipWaiting()));
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", (event) => {
  const req = event.request;
  if (req.mode !== "navigate" || req.method !== "GET") return; // passthrough
  const u = new URL(req.url);
  if (u.origin !== self.location.origin || u.pathname.startsWith("/api/")) return; // login redirect, downloads, ...
  event.respondWith(
    fetch(req).catch(() => caches.match(OFFLINE).then((r) => r || new Response("本地服务未运行", { status: 503, headers: { "Content-Type": "text/plain; charset=utf-8" } }))),
  );
});
