package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
)

func getWith(s *Server, ui roleUI, token, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.AddCookie(&http.Cookie{Name: ui.cookieName, Value: token})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// With a session store configured, a restart (a new Server on the same file) doesn't sign anyone
// out, and a logout before the restart still holds after it.
func TestLoginSurvivesServerRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	a := serverAt(t, path)
	live := a.sessions.Create(auth.RoleManager, testManagerUsername, auth.SessionFlags{})
	gone := a.sessions.Create(auth.RoleManager, testManagerUsername, auth.SessionFlags{})
	a.sessions.Revoke(gone)
	if rec := getWith(a, managerUI, live, "/manager"); rec.Code != http.StatusOK {
		t.Fatalf("before the restart: %d", rec.Code)
	}

	b := serverAt(t, path) // restart
	if rec := getWith(b, managerUI, live, "/manager"); rec.Code != http.StatusOK {
		t.Fatalf("after the restart the login must still work: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	if rec := getWith(b, managerUI, gone, "/manager"); rec.Code != http.StatusSeeOther {
		t.Errorf("a logged-out session must not come back: %d", rec.Code)
	}
}

// Without a session store (the default) a restart signs everyone out, exactly as before.
func TestNoSessionStoreMeansMemoryOnly(t *testing.T) {
	a := testServerWithAuth(t)
	tok := a.sessions.Create(auth.RoleManager, testManagerUsername, auth.SessionFlags{})
	b := testServerWithAuth(t)
	if rec := getWith(b, managerUI, tok, "/manager"); rec.Code != http.StatusSeeOther {
		t.Fatalf("a new server without a store must not know the old session: %d", rec.Code)
	}
}
