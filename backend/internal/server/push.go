package server

import (
	_ "embed"
	"encoding/json"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// Browser push enrolment for logged-in admins and managers: a page with "Enable notifications
// on this browser", the service worker that shows the notifications, and the subscribe /
// unsubscribe endpoints. Served under each role's own prefix (/admin/..., /manager/...) so the
// service worker's scope and the session cookie's path line up without any ingress change.

//go:embed push_sw.js
var pushServiceWorker []byte

const maxSubscribeBody = 4 << 10

func (s *Server) registerPushRoutes(mux *http.ServeMux) {
	for _, ui := range []roleUI{adminUI, managerUI} {
		base := ui.homePath
		mux.HandleFunc("GET "+base+"/push-sw.js", handlePushServiceWorker)
		mux.HandleFunc("GET "+base+"/notifications", s.requireRole(ui, s.handleNotificationsPage(ui)))
		mux.HandleFunc("POST "+base+"/api/push/subscribe", s.requireRole(ui, s.handlePushSubscribe(ui)))
		mux.HandleFunc("POST "+base+"/api/push/unsubscribe", s.requireRole(ui, s.handlePushUnsubscribe(ui)))
	}
}

func handlePushServiceWorker(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(pushServiceWorker)
}

// sessionUsername returns the logged-in username for ui. Only called behind requireRole, which
// has already validated the session.
func (s *Server) sessionUsername(ui roleUI, r *http.Request) string {
	c, err := r.Cookie(ui.cookieName)
	if err != nil {
		return ""
	}
	sess, ok := s.sessions.Check(c.Value)
	if !ok {
		return ""
	}
	return sess.Username
}

// subscribeRequest is PushSubscription.toJSON() as the browser produces it.
type subscribeRequest struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

func decodeSubscribe(w http.ResponseWriter, r *http.Request) (subscribeRequest, bool) {
	var req subscribeRequest
	// JSON-only: together with the SameSite=Strict session cookie this keeps a cross-site form
	// post from enrolling an attacker's endpoint.
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "expected application/json", http.StatusUnsupportedMediaType)
		return req, false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxSubscribeBody)).Decode(&req); err != nil {
		http.Error(w, "bad subscription", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func (s *Server) handlePushSubscribe(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.vapidPublicKey == "" {
			http.Error(w, "browser notifications are not enabled on this server", http.StatusConflict)
			return
		}
		req, ok := decodeSubscribe(w, r)
		if !ok {
			return
		}
		user := s.sessionUsername(ui, r)
		next, err := s.cfg.Load().WithPushSubscription(config.PushSubscription{
			Username: user, Endpoint: req.Endpoint, P256DH: req.Keys.P256DH, Auth: req.Keys.Auth,
			Created: time.Now().UTC().Truncate(time.Second),
		})
		if err != nil {
			http.Error(w, "invalid subscription: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.applyConfig(r.Context(), next, false); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.log.Info("push subscription added", "user", user, "push_service", originOf(req.Endpoint))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handlePushUnsubscribe(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeSubscribe(w, r)
		if !ok {
			return
		}
		user := s.sessionUsername(ui, r)
		cfg := s.cfg.Load()
		// Only the subscription's owner can remove it this way (admins manage others' from /admin).
		owned := false
		for _, sub := range cfg.PushSubscriptionsFor(user) {
			if sub.Endpoint == req.Endpoint {
				owned = true
			}
		}
		if !owned {
			w.WriteHeader(http.StatusNoContent) // already gone, or not yours: nothing to do
			return
		}
		next, _ := cfg.WithoutPushSubscription(req.Endpoint)
		if err := s.applyConfig(r.Context(), next, false); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.log.Info("push subscription removed", "user", user, "push_service", originOf(req.Endpoint))
		w.WriteHeader(http.StatusNoContent)
	}
}

type notificationsView struct {
	Base           string
	VAPIDPublicKey string
	Devices        int
}

func (s *Server) handleNotificationsPage(ui roleUI) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = notificationsPageTmpl.Execute(w, notificationsView{
			Base:           ui.homePath,
			VAPIDPublicKey: s.vapidPublicKey,
			Devices:        len(s.cfg.Load().PushSubscriptionsFor(s.sessionUsername(ui, r))),
		})
	}
}

// originOf returns scheme://host — the only part of a push endpoint that's safe to log (the
// path is a per-browser capability).
func originOf(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

var notificationsPageTmpl = template.Must(template.New("notifications").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Display alerts</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:36rem;margin:3rem auto;padding:0 1rem;color:#222}
button{font:inherit;padding:.6rem 1.1rem;border-radius:.4rem;border:1px solid #2a6;background:#2a6;color:#fff;cursor:pointer}
button.secondary{background:#fff;color:#2a6}#state{margin:1rem 0;font-weight:600}.muted{color:#666}</style></head>
<body>
<p><a href="{{.Base}}">← Back</a></p>
<h1>Display alerts on this browser</h1>
<p>Get a notification here when a meeting-room display runs low on battery or stops checking in,
and when it's back.</p>
{{if not .VAPIDPublicKey}}
<p id="state">Browser notifications aren't turned on for this server yet. An admin needs to add
the <code>webpush</code> channel.</p>
{{else}}
<p class="muted">Your account has notifications on {{.Devices}} browser(s).</p>
<p id="state">Checking…</p>
<button id="enable" hidden>Enable notifications on this browser</button>
<button id="disable" class="secondary" hidden>Turn off on this browser</button>
<script>
const BASE = {{.Base}}, KEY = {{.VAPIDPublicKey}};
const $ = id => document.getElementById(id);
function keyBytes(b64) {
  const s = atob((b64 + "=".repeat((4 - b64.length % 4) % 4)).replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(s, c => c.charCodeAt(0));
}
async function post(path, sub) {
  const r = await fetch(BASE + path, {method: "POST", headers: {"Content-Type": "application/json"},
    body: JSON.stringify(sub), credentials: "same-origin"});
  if (!r.ok) throw new Error(await r.text());
}
async function refresh() {
  if (!("serviceWorker" in navigator) || !("PushManager" in window)) {
    $("state").textContent = "This browser doesn't support push notifications."; return;
  }
  const reg = await navigator.serviceWorker.register(BASE + "/push-sw.js", {scope: BASE + "/"});
  const sub = await reg.pushManager.getSubscription();
  if (Notification.permission === "denied") {
    $("state").textContent = "Notifications are blocked for this site — allow them in the browser's site settings.";
  } else {
    $("state").textContent = sub ? "On — this browser will get display alerts." : "Off on this browser.";
  }
  $("enable").hidden = !!sub || Notification.permission === "denied";
  $("disable").hidden = !sub;
}
$("enable").onclick = async () => {
  try {
    const reg = await navigator.serviceWorker.ready;
    const sub = await reg.pushManager.subscribe({userVisibleOnly: true, applicationServerKey: keyBytes(KEY)});
    await post("/api/push/subscribe", sub.toJSON());
  } catch (e) { $("state").textContent = "Couldn't turn on notifications: " + e.message; return; }
  refresh();
};
$("disable").onclick = async () => {
  const reg = await navigator.serviceWorker.ready;
  const sub = await reg.pushManager.getSubscription();
  if (sub) { await post("/api/push/unsubscribe", sub.toJSON()); await sub.unsubscribe(); }
  refresh();
};
refresh();
</script>
{{end}}
</body></html>
`))
