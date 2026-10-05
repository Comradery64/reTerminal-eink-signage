package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// LarkWebhook posts to a Lark (Feishu) custom-bot webhook. Lark answers HTTP 200 even when it
// rejects a message, with the verdict in the JSON body ({"code":0} on success; older bots use
// "StatusCode"), so the body is checked, not just the status.
type LarkWebhook struct {
	URL    string
	Client *http.Client
}

func NewLarkWebhook(url string) *LarkWebhook {
	return &LarkWebhook{URL: url, Client: &http.Client{Timeout: 8 * time.Second}}
}

func (l *LarkWebhook) Name() string { return "lark" }

func (l *LarkWebhook) Send(ctx context.Context, m Message) error {
	body, _ := json.Marshal(map[string]any{
		"msg_type": "text",
		"content":  map[string]string{"text": m.Title + "\n" + m.Text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.URL, bytes.NewReader(body))
	if err != nil {
		return redactURL(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.Client.Do(req)
	if err != nil {
		return redactURL(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("lark webhook status %d", resp.StatusCode)
	}
	var verdict struct {
		Code       *int   `json:"code"`
		StatusCode *int   `json:"StatusCode"`
		Msg        string `json:"msg"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return fmt.Errorf("lark webhook: unreadable response")
	}
	for _, c := range []*int{verdict.Code, verdict.StatusCode} {
		if c != nil && *c != 0 {
			return fmt.Errorf("lark webhook rejected the message (code %d: %s)", *c, verdict.Msg)
		}
	}
	return nil
}
