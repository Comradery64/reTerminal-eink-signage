// Package admin builds the read-only view model for the /admin page — a pure function over
// *config.Config, mirroring internal/status's "Build(cfg, ...) []View" pattern. The HTTP handler
// (internal/server/admin.go) is a thin wrapper that calls Build and executes a template.
package admin

import "github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"

// RoomView is one room as shown on the admin rooms table/edit form. Unlike the /manager view
// (which shows the *resolved effective* wake mode), this shows the *raw* per-room override so an
// admin edits exactly what's stored — WakeMode == "" means "no override, uses fleet default".
// TokenConfigured never exposes the token hash itself, only whether one is set.
type RoomView struct {
	DeviceID            string
	Name                string
	Room                string
	WakeMode            string
	FlatIntervalSeconds uint32
	TokenConfigured     bool
}

// UserView is one login account as shown on the admin Access panel. Never exposes the password
// hash — only whether one is set, mirroring RoomView.TokenConfigured.
type UserView struct {
	Username string
	Role     string
}

// View is the full admin page's data.
type View struct {
	Wake     config.WakeConfig
	Alerts   config.AlertConfig
	Firmware config.FirmwareConfig
	Rooms    []RoomView
	Users    []UserView
}

// Build derives the admin view from cfg. Pure function — no I/O, no auth, safe to unit test in
// isolation.
func Build(cfg *config.Config) View {
	rooms := make([]RoomView, 0, len(cfg.Rooms))
	for _, r := range cfg.Rooms {
		rv := RoomView{
			DeviceID:        r.DeviceID,
			Name:            r.Name,
			Room:            r.Room,
			TokenConfigured: r.TokenSHA256 != "",
		}
		if r.WakeMode != nil {
			rv.WakeMode = *r.WakeMode
		}
		if r.FlatIntervalSeconds != nil {
			rv.FlatIntervalSeconds = *r.FlatIntervalSeconds
		}
		rooms = append(rooms, rv)
	}

	users := make([]UserView, 0, len(cfg.Users))
	for _, u := range cfg.Users {
		users = append(users, UserView{Username: u.Username, Role: u.Role})
	}

	return View{Wake: cfg.Wake, Alerts: cfg.Alerts, Firmware: cfg.Firmware, Rooms: rooms, Users: users}
}
