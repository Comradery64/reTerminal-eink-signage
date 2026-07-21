package server

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestReceptionistPageRequiresLogin(t *testing.T) {
	s := testServerWithAuth(t)
	srv := httptest.NewTLSServer(s.Handler())
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(srv.URL + "/receptionist")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("want redirect to login, got %d", resp.StatusCode)
	}
}

func TestReceptionistPageShowsStatusButNoWakeControl(t *testing.T) {
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

	resp, err := client.PostForm(srv.URL+"/receptionist/login", url.Values{"username": {testReceptionistUsername}, "password": {testReceptionistPassword}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("receptionist login: want 303, got %d", resp.StatusCode)
	}

	page, err := client.Get(srv.URL + "/receptionist")
	if err != nil || page.StatusCode != http.StatusOK {
		t.Fatalf("GET /receptionist: err=%v code=%v", err, page.StatusCode)
	}
	body, _ := io.ReadAll(page.Body)
	html := string(body)
	if !strings.Contains(html, "Aspen") {
		t.Errorf("receptionist page missing room name: %s", html)
	}
	if strings.Contains(html, "wake_mode") || strings.Contains(html, "Save") {
		t.Error("receptionist page must be read-only — no wake-mode control or save action")
	}
}

func TestReceptionistCredentialsDoNotGrantManagerAccess(t *testing.T) {
	s := testServerWithAuth(t)
	guarded := s.requireRole(managerUI, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	token := s.sessions.Create(receptionistUI.role)
	req := httptest.NewRequest(http.MethodGet, "/manager", nil)
	req.AddCookie(&http.Cookie{Name: managerUI.cookieName, Value: token})
	rec := httptest.NewRecorder()
	guarded(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("a receptionist session presented as manager_session must be rejected, got %d", rec.Code)
	}
}
