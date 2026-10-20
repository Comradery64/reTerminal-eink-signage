package server

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/notify"
)

// The /admin "Locations" section: offices, who is told about each office's displays, and a
// "Send test DM" that messages only that location's configured person, after a browser confirm.

type locationRow struct {
	ID, Name, Email, ChatID, Delivery string
	Default                           bool
	Rooms                             int
}

type locationsPanel struct {
	Rows        []locationRow
	Tested      string // email just DM'd successfully
	TestedGroup string // location name whose group just received a test message
}

func buildLocationsPanel(cfg *config.Config, tested, testedGroup string) locationsPanel {
	p := locationsPanel{Tested: tested, TestedGroup: testedGroup}
	for _, l := range cfg.Locations {
		row := locationRow{ID: l.ID, Name: l.Name, Email: l.LarkUserEmail, ChatID: l.LarkChatID, Delivery: l.Delivery, Default: l.ID == cfg.DefaultLocation}
		for _, r := range cfg.Rooms {
			if cfg.LocationOf(r) == l.ID {
				row.Rooms++
			}
		}
		p.Rows = append(p.Rows, row)
	}
	return p
}

func (s *Server) saveLocationsCfg(w http.ResponseWriter, r *http.Request, next *config.Config, err error, what string) {
	if err != nil {
		s.log.Error("admin location change rejected", "what", what, "err", err)
		adminRedirectError(w, r, "Couldn't "+what+".||"+err.Error(), "locations")
		return
	}
	if err := s.applyConfig(r.Context(), next, true); err != nil {
		adminRedirectError(w, r, err.Error(), "locations")
		return
	}
	http.Redirect(w, r, "/admin?saved=locations#locations", http.StatusSeeOther)
}

func (s *Server) handleAdminSaveLocation(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	l := config.Location{
		ID:            strings.TrimSpace(r.PostForm.Get("id")),
		Name:          strings.TrimSpace(r.PostForm.Get("name")),
		LarkUserEmail: strings.TrimSpace(r.PostForm.Get("lark_user_email")),
		LarkChatID:    strings.TrimSpace(r.PostForm.Get("lark_chat_id")),
		Delivery:      strings.TrimSpace(r.PostForm.Get("delivery")),
	}
	next, err := s.cfg.Load().WithLocation(l)
	s.saveLocationsCfg(w, r, next, err, "save that location")
}

func (s *Server) handleAdminDeleteLocation(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next, err := s.cfg.Load().WithoutLocation(r.PostForm.Get("id"))
	s.saveLocationsCfg(w, r, next, err, "remove that location")
}

func (s *Server) handleAdminDefaultLocation(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	next, err := s.cfg.Load().WithDefaultLocation(r.PostForm.Get("id"))
	s.saveLocationsCfg(w, r, next, err, "change the default location")
}

// handleAdminTestDM DMs exactly the configured person of one location — the recipient comes from
// the saved config, never from the form, so this can't be used to message an arbitrary address.
func (s *Server) handleAdminTestDM(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cfg := s.cfg.Load()
	loc, ok := cfg.LocationByID(r.PostForm.Get("id"))
	switch {
	case !ok:
		http.Error(w, "unknown location", http.StatusBadRequest)
		return
	case loc.LarkUserEmail == "":
		adminRedirectError(w, r, fmt.Sprintf("%s has no person to message.", loc.Name), "locations")
		return
	case cfg.Alerts.LarkApp.AppID == "" || cfg.Alerts.LarkApp.AppSecret == "":
		adminRedirectError(w, r, "Can't send Lark messages yet: the Lark app secret (LARK_APP_SECRET) isn't set on the server.", "locations")
		return
	}
	app := notify.NewLarkApp(cfg.Alerts.LarkApp.AppID, cfg.Alerts.LarkApp.AppSecret, cfg.Alerts.LarkApp.BaseURL, nil)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	user := s.sessionUsername(adminUI, r)
	err := app.SendDM(ctx, loc.LarkUserEmail, notify.Message{
		Title: "🔔 Test from the meeting-display broker",
		Text: fmt.Sprintf("Hi — you're the contact for %s's meeting-room displays. You'll get a message here if one runs low on battery or stops checking in. (Test sent by %s at %s.)",
			loc.Name, user, time.Now().UTC().Format("15:04 UTC")),
	})
	if err != nil { // notify redacts URLs and never includes the token or secret
		s.log.Error("admin test DM failed", "location", loc.ID, "err", err)
		adminRedirectError(w, r, "The test DM didn't go through.||"+err.Error(), "locations")
		return
	}
	s.log.Info("admin test DM sent", "location", loc.ID, "user", user)
	http.Redirect(w, r, "/admin?saved=locations&dm="+template.URLQueryEscaper(loc.LarkUserEmail)+"#locations", http.StatusSeeOther)
}

// handleAdminTestGroup posts to exactly one location's configured group — like the test DM, the
// target comes from the saved config, never the form. Everyone in that group will see it.
func (s *Server) handleAdminTestGroup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	cfg := s.cfg.Load()
	loc, ok := cfg.LocationByID(r.PostForm.Get("id"))
	switch {
	case !ok:
		http.Error(w, "unknown location", http.StatusBadRequest)
		return
	case loc.LarkChatID == "":
		adminRedirectError(w, r, fmt.Sprintf("%s has no group chat ID.", loc.Name), "locations")
		return
	case cfg.Alerts.LarkApp.AppID == "" || cfg.Alerts.LarkApp.AppSecret == "":
		adminRedirectError(w, r, "Can't send Lark messages yet: the Lark app secret (LARK_APP_SECRET) isn't set on the server.", "locations")
		return
	}
	app := notify.NewLarkApp(cfg.Alerts.LarkApp.AppID, cfg.Alerts.LarkApp.AppSecret, cfg.Alerts.LarkApp.BaseURL, nil)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	user := s.sessionUsername(adminUI, r)
	err := app.SendChat(ctx, loc.LarkChatID, notify.Message{
		Title: "🔔 Test from the meeting-display broker",
		Text: fmt.Sprintf("This group receives alerts for %s's meeting-room displays (low battery, offline). (Test sent by %s at %s.)",
			loc.Name, user, time.Now().UTC().Format("15:04 UTC")),
	})
	if err != nil {
		s.log.Error("admin test group message failed", "location", loc.ID, "err", err)
		adminRedirectError(w, r, "The test message didn't go through.||"+err.Error(), "locations")
		return
	}
	s.log.Info("admin test group message sent", "location", loc.ID, "user", user)
	http.Redirect(w, r, "/admin?saved=locations&group="+template.URLQueryEscaper(loc.Name)+"#locations", http.StatusSeeOther)
}
