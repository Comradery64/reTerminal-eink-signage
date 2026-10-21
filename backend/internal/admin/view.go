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
	DeviceID string
	Name     string
	Room     string
	// Label is the panel's position within its room, set only where a room has several displays
	// (config.Room.Label). Empty otherwise, so the rooms table stays unchanged for the common case.
	Label               string
	WakeMode            string
	FlatIntervalSeconds uint32
	TokenConfigured     bool
	Location            string // the room's own location ID; "" = uses the default
	LocationName        string // effective location's name, for display
}

// UserView is one login account as shown on the admin Access panel. Never exposes the password
// hash — only whether one is set, mirroring RoomView.TokenConfigured.
type UserView struct {
	Username  string
	Role      string
	Locations []string // locations the account is limited to; empty = every location
	TwoFactor bool     // the account has enrolled an authenticator app
}

// FleetView is the provisioning wizard's fleet Wi-Fi state. WifiPSKConfigured never exposes the
// PSK itself, only whether one is present — mirroring RoomView.TokenConfigured.
type FleetView struct {
	WifiSSID          string
	WifiPSKConfigured bool
}

// View is the full admin page's data.
type View struct {
	Wake     config.WakeConfig
	Alerts   config.AlertConfig
	Firmware config.FirmwareConfig
	Fleet    FleetView
	Rooms    []RoomView
	Users    []UserView
	// Locations are shown as configured. LarkUserEmail is a work address, not a secret; it's
	// displayed so an admin can see who each office's alerts go to.
	Locations       []config.Location
	DefaultLocation string
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
			Label:           r.Label,
			TokenConfigured: r.TokenSHA256 != "",
			Location:        r.Location,
		}
		if l, ok := cfg.LocationByID(cfg.LocationOf(r)); ok {
			rv.LocationName = l.Name
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
		users = append(users, UserView{Username: u.Username, Role: u.Role, Locations: u.Locations, TwoFactor: u.TOTPSecret != ""})
	}

	return View{
		Wake:     cfg.Wake,
		Alerts:   cfg.Alerts,
		Firmware: cfg.Firmware,
		Fleet: FleetView{
			WifiSSID:          cfg.Fleet.WifiSSID,
			WifiPSKConfigured: cfg.Fleet.WifiPSK != "",
		},
		Rooms:           rooms,
		Users:           users,
		Locations:       append([]config.Location(nil), cfg.Locations...),
		DefaultLocation: cfg.DefaultLocation,
	}
}
