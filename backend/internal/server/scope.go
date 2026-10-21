package server

import (
	"net/http"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

// sessionAccount is the logged-in account for ui, read from the live config so an admin's change
// to its locations applies to sessions that are already open. ok is false when the session is
// missing or its account has since been removed.
func (s *Server) sessionAccount(ui roleUI, r *http.Request) (u config.User, ok bool) {
	c, err := r.Cookie(ui.cookieName)
	if err != nil {
		return config.User{}, false
	}
	sess, found := s.sessions.Check(c.Value)
	if !found {
		return config.User{}, false
	}
	if sess.Role == adminUI.role {
		return config.User{Username: sess.Username, Role: string(sess.Role)}, true // admins see every location
	}
	return s.cfg.Load().UserByUsername(sess.Username)
}

// roomVisible returns whether the logged-in account may see a device's room. It fails closed: an
// unknown session, a removed account or an unknown device sees nothing.
func (s *Server) roomVisible(ui roleUI, r *http.Request) func(deviceID string) bool {
	u, ok := s.sessionAccount(ui, r)
	cfg := s.cfg.Load()
	return func(deviceID string) bool {
		if !ok {
			return false
		}
		room, found := cfg.RoomByDeviceID(deviceID)
		if !found {
			return !u.Limited()
		}
		return cfg.CanSeeRoom(u, room)
	}
}
