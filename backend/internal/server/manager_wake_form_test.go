package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/config"
)

func postWake(t *testing.T, s *Server, device string, form url.Values) string {
	t.Helper()
	form.Set("device_id", device)
	rec := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "POST", "/manager/wake/save", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("wake save: %d", rec.Code)
	}
	return rec.Header().Get("Location")
}

// smartRoom puts rt-1 on its own Smart override, the way a room can be left after a test.
func smartRoom(t *testing.T, fleetMode string) *Server {
	t.Helper()
	s := testServerWithAuth(t)
	c := *s.cfg.Load()
	c.Wake.Mode = fleetMode
	c.Rooms = append([]config.Room(nil), c.Rooms...)
	smart := "smart"
	c.Rooms[0].WakeMode = &smart
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s.cfg.Store(&c)
	return s
}

// A room that was left on Smart can go back to Simple without typing a number: blank means "follow
// the fleet's own intervals", and since the fleet is Simple that removes the override entirely.
func TestSimpleWithBlankIntervalFollowsTheFleetDefault(t *testing.T) {
	for _, blank := range []string{"", "0", "  "} {
		s := smartRoom(t, "")
		loc := postWake(t, s, "rt-1", url.Values{"wake_mode": {"flat"}, "interval_seconds": {blank}})
		if strings.Contains(loc, "error") {
			t.Fatalf("blank %q must save: %s", blank, loc)
		}
		if r, _ := s.cfg.Load().RoomByDeviceID("rt-1"); r.WakeMode != nil || r.FlatIntervalSeconds != nil {
			t.Errorf("blank %q: expected no override, got mode=%v interval=%v", blank, r.WakeMode, r.FlatIntervalSeconds)
		}
	}
}

// If the fleet default is Smart, "Simple, fleet intervals" has to be an explicit Simple override.
func TestSimpleWithBlankIntervalWhenFleetIsSmartPinsSimple(t *testing.T) {
	s := smartRoom(t, "smart")
	postWake(t, s, "rt-1", url.Values{"wake_mode": {"flat"}, "interval_seconds": {""}})
	r, _ := s.cfg.Load().RoomByDeviceID("rt-1")
	if r.WakeMode == nil || *r.WakeMode != "flat" || r.FlatIntervalSeconds != nil {
		t.Fatalf("want an explicit flat override with no interval, got mode=%v interval=%v", r.WakeMode, r.FlatIntervalSeconds)
	}
}

// A typed interval still sets that interval; anything under the form's 60 s minimum or not a number
// is refused without changing the room.
func TestSimpleIntervalValidation(t *testing.T) {
	s := smartRoom(t, "")
	postWake(t, s, "rt-1", url.Values{"wake_mode": {"flat"}, "interval_seconds": {"600"}})
	if r, _ := s.cfg.Load().RoomByDeviceID("rt-1"); r.WakeMode == nil || *r.WakeMode != "flat" || r.FlatIntervalSeconds == nil || *r.FlatIntervalSeconds != 600 {
		t.Fatalf("600 must set a 600 s Simple override: %+v", r)
	}
	for _, bad := range []string{"30", "59", "abc", "-5", "1.5"} {
		loc := postWake(t, s, "rt-1", url.Values{"wake_mode": {"flat"}, "interval_seconds": {bad}})
		if loc != "/manager?error=invalid_interval" {
			t.Errorf("interval %q: got %q, want invalid_interval", bad, loc)
		}
	}
	if r, _ := s.cfg.Load().RoomByDeviceID("rt-1"); r.FlatIntervalSeconds == nil || *r.FlatIntervalSeconds != 600 {
		t.Error("a refused value changed the room")
	}
}

// The page doesn't pre-fill a 0 that its own minimum would reject; a room with an interval shows it.
func TestManagerFormDoesNotPrefillZero(t *testing.T) {
	s := smartRoom(t, "")
	body := asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager", nil).Body.String()
	if strings.Contains(body, `name="interval_seconds" min="60" step="60" value="0"`) || strings.Contains(body, `value="0"`) {
		t.Fatal("the interval box is pre-filled with 0, which the form then refuses")
	}
	if !strings.Contains(body, "Blank = fleet default") {
		t.Error("the interval box should say what blank means")
	}
	postWake(t, s, "rt-1", url.Values{"wake_mode": {"flat"}, "interval_seconds": {"900"}})
	body = asAccount(s, managerUI, auth.RoleManager, testManagerUsername, "GET", "/manager", nil).Body.String()
	if !strings.Contains(body, `value="900"`) {
		t.Error("an interval that is set should be shown")
	}
}
