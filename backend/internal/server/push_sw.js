// Service worker for display alerts (served at /admin/push-sw.js and /manager/push-sw.js; see
// push.go). The broker sends {title, body, tag}; a repeat for the same display replaces the
// earlier notification rather than stacking.
self.addEventListener("push", (event) => {
  let data = {};
  try { data = event.data ? event.data.json() : {}; } catch (_) {}
  event.waitUntil(self.registration.showNotification(data.title || "Meeting display alert", {
    body: data.body || "",
    tag: data.tag || undefined,
    renotify: !!data.tag,
  }));
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const home = self.registration.scope; // /admin/ or /manager/
  event.waitUntil(self.clients.matchAll({ type: "window" }).then((tabs) => {
    for (const t of tabs) if (t.url.startsWith(home)) return t.focus();
    return self.clients.openWindow(home);
  }));
});
