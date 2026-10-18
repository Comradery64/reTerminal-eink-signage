package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// LarkRoutes tells the Lark app where a location's alerts go. Read from the live config on every
// send, so an /admin edit to a location takes effect without rebuilding the channel.
type LarkRoutes interface {
	// LarkRoute returns the location's person (email) and its own group (chat ID), either "".
	LarkRoute(location string) (email, chatID string)
	// LarkFallbackChat is the global group used when a location's person can't be reached.
	LarkFallbackChat() string
}

// LarkApp sends alerts as a Lark custom app (Open Platform), because custom-bot webhooks aren't
// available on every tenant. Each alert goes to its location's person as a DM; only if there is no
// person, or the DM fails, does it go to the location's group or else the global fallback group —
// never both. Lark answers HTTP 200 for most failures with the verdict in the body's `code`, so
// every response is checked. The app secret and the tenant token never appear in errors or logs.
type LarkApp struct {
	AppID     string
	AppSecret string
	BaseURL   string // e.g. https://open.larksuite.com/open-apis
	Routes    LarkRoutes
	Client    *http.Client
	now       func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

func NewLarkApp(appID, appSecret, baseURL string, routes LarkRoutes) *LarkApp {
	return &LarkApp{AppID: appID, AppSecret: appSecret, BaseURL: strings.TrimRight(baseURL, "/"), Routes: routes,
		Client: &http.Client{Timeout: 8 * time.Second}, now: time.Now}
}

func (l *LarkApp) Name() string { return "lark" }

// tokenRefreshMargin renews the tenant token this long before Lark says it expires (tokens last
// ~2h), so a send never races an expiring token.
const tokenRefreshMargin = 5 * time.Minute

// Lark codes meaning the tenant token itself was rejected: drop the cache and retry once.
var larkTokenInvalid = map[int]bool{99991661: true, 99991663: true, 99991668: true}

func (l *LarkApp) Send(ctx context.Context, m Message) error {
	text := m.Title + "\n" + m.Text
	email, chat := "", ""
	if l.Routes != nil {
		email, chat = l.Routes.LarkRoute(m.Location)
	}
	var dmErr error
	if email != "" {
		if dmErr = l.send(ctx, "email", email, text); dmErr == nil {
			return nil
		}
	}
	target := chat
	if target == "" && l.Routes != nil {
		target = l.Routes.LarkFallbackChat()
	}
	if target == "" {
		if dmErr != nil {
			return fmt.Errorf("DM failed and no fallback chat: %w", dmErr)
		}
		return fmt.Errorf("no Lark recipient for location %q", m.Location)
	}
	if err := l.send(ctx, "chat_id", target, text); err != nil {
		return errors.Join(dmErr, fmt.Errorf("fallback chat: %w", err))
	}
	return nil // delivered to the fallback chat; a failed DM is the caller's to log if it wants
}

// SendDM messages one person directly — the /admin "Send test DM" action.
func (l *LarkApp) SendDM(ctx context.Context, email string, m Message) error {
	return l.send(ctx, "email", email, m.Title+"\n"+m.Text)
}

func (l *LarkApp) send(ctx context.Context, idType, id, text string) error {
	content, _ := json.Marshal(map[string]string{"text": text})
	body, _ := json.Marshal(map[string]string{"receive_id": id, "msg_type": "text", "content": string(content)})
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := l.tenantToken(ctx)
		if err != nil {
			return err
		}
		code, msg, err := l.post(ctx, "/im/v1/messages?receive_id_type="+idType, tok, body)
		if err != nil {
			return err
		}
		if code == 0 {
			return nil
		}
		if larkTokenInvalid[code] && attempt == 0 {
			l.mu.Lock()
			l.token = ""
			l.mu.Unlock()
			continue
		}
		return fmt.Errorf("lark rejected the message to %s (code %d: %s)", idType, code, msg)
	}
	return errors.New("lark rejected the tenant token twice")
}

func (l *LarkApp) tenantToken(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.token != "" && l.now().Before(l.expires.Add(-tokenRefreshMargin)) {
		return l.token, nil
	}
	body, _ := json.Marshal(map[string]string{"app_id": l.AppID, "app_secret": l.AppSecret})
	var out struct {
		Code   int    `json:"code"`
		Msg    string `json:"msg"`
		Token  string `json:"tenant_access_token"`
		Expire int    `json:"expire"` // seconds
	}
	if err := l.postJSON(ctx, "/auth/v3/tenant_access_token/internal", "", body, &out); err != nil {
		return "", fmt.Errorf("lark auth: %w", err)
	}
	if out.Code != 0 || out.Token == "" {
		return "", fmt.Errorf("lark auth rejected the app credentials (code %d: %s)", out.Code, out.Msg)
	}
	l.token, l.expires = out.Token, l.now().Add(time.Duration(out.Expire)*time.Second)
	return l.token, nil
}

func (l *LarkApp) post(ctx context.Context, path, token string, body []byte) (int, string, error) {
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	err := l.postJSON(ctx, path, token, body, &out)
	return out.Code, out.Msg, err
}

func (l *LarkApp) postJSON(ctx context.Context, path, token string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return redactURL(err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return redactURL(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("unreadable response (HTTP %d)", resp.StatusCode)
	}
	return nil // HTTP status is not the verdict for Lark; callers check the body's code
}
