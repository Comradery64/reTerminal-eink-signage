package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/auth"
)

func TestDashboardPageRequiresLogin(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(srv.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect to login, got %d", resp.StatusCode)
	}
}

func TestDashboardPageShowsStatusButNoWakeControl(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := srv.Client()
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }

	resp, err := client.PostForm(srv.URL+"/dashboard/login", url.Values{"username": {testViewerUsername}, "password": {testViewerPassword}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("viewer login: want 303, got %d", resp.StatusCode)
	}

	page, err := client.Get(srv.URL + "/dashboard")
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /dashboard: err=%v code=%v", err, page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	html := string(body)
	if !strings.Contains(html, "Aspen") {
		t.Errorf("dashboard page missing room name: %s", html)
	}
	if strings.Contains(html, "wake_mode") || strings.Contains(html, "Save") {
		t.Error("dashboard page must be read-only — no wake-mode control or save action")
	}
}

func TestDashboardCredentialsDoNotGrantManagerAccess(t *testing.T) {
	s := testServerWithAuth(t)
	guarded := s.requireRole(managerUI, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	token := s.sessions.Create(viewerUI.role, "viewer-account", auth.SessionFlags{})
	req := httptest.NewRequest(http.MethodGet, "/manager", nil)
	req.AddCookie(&http.Cookie{Name: managerUI.cookieName, Value: token})
	rec := httptest.NewRecorder()
	guarded(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a viewer session presented as manager_session must be rejected, got %d", rec.Code)
	}
}

func TestDashboardPreviewServesLastRenderedImage(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := srv.Client()
	client.Jar = jar
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := client.PostForm(srv.URL+"/dashboard/login", url.Values{"username": {testViewerUsername}, "password": {testViewerPassword}}); err != nil {
		t.Fatal(err)
	}

	resp, err := client.Get(srv.URL + "/dashboard/preview/rt-1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("no poller has run in this test server, so no cached preview should exist yet: got %d", resp.StatusCode)
	}
}
