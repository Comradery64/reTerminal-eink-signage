package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// freeBusyScope is the minimal scope: read free/busy intervals only — never event titles/details.
const freeBusyScope = "https://www.googleapis.com/auth/calendar.freebusy"

// GoogleProvider reads room availability via the Calendar freeBusy API. It authenticates KEYLESS
// via Workload Identity Federation (external-account credentials): the broker's projected k8s
// service-account token is exchanged for short-lived Google credentials — no JSON key at rest, no
// domain-wide delegation, no user impersonation. The service account reads the rooms it has been
// granted freeBusyReader on (shared directly per room).
type GoogleProvider struct {
	http *http.Client
}

// NewGoogle builds the provider from an external-account credential config (WIF). If credConfig is
// empty it falls back to Application Default Credentials (the GOOGLE_APPLICATION_CREDENTIALS env,
// which in k8s points at the mounted cred-config that references the projected token).
func NewGoogle(ctx context.Context, credConfig string) (*GoogleProvider, error) {
	var creds *google.Credentials
	var err error
	if credConfig != "" {
		var data []byte
		if data, err = os.ReadFile(credConfig); err != nil {
			return nil, fmt.Errorf("google: read cred config: %w", err)
		}
		creds, err = google.CredentialsFromJSON(ctx, data, freeBusyScope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, freeBusyScope)
	}
	if err != nil {
		return nil, fmt.Errorf("google: resolve WIF credentials: %w", err)
	}
	hc := oauth2.NewClient(ctx, creds.TokenSource)
	hc.Timeout = 15 * time.Second
	return &GoogleProvider{http: hc}, nil
}

type freeBusyResponse struct {
	Calendars map[string]struct {
		Errors []struct {
			Domain string `json:"domain"`
			Reason string `json:"reason"`
		} `json:"errors"`
		Busy []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"busy"`
	} `json:"calendars"`
}

// FetchSchedule queries freeBusy for one room and maps each busy interval to a subject-less Event.
// We never see meeting titles or organizers — only that the room is occupied.
func (p *GoogleProvider) FetchSchedule(ctx context.Context, roomEmail string, from, to time.Time) (*Schedule, error) {
	body, _ := json.Marshal(map[string]any{
		"timeMin": from.UTC().Format(time.RFC3339),
		"timeMax": to.UTC().Format(time.RFC3339),
		"items":   []map[string]string{{"id": roomEmail}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://www.googleapis.com/calendar/v3/freeBusy", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("freebusy request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("freebusy status %d for %s", resp.StatusCode, roomEmail)
	}

	var fb freeBusyResponse
	if err := json.NewDecoder(resp.Body).Decode(&fb); err != nil {
		return nil, fmt.Errorf("freebusy decode: %w", err)
	}
	cal, ok := fb.Calendars[roomEmail]
	if !ok {
		return nil, fmt.Errorf("freebusy: no entry for %s", roomEmail)
	}
	if len(cal.Errors) > 0 {
		// Most common: the SA hasn't been granted freeBusyReader on this room yet.
		return nil, fmt.Errorf("freebusy: calendar error for %s: %s", roomEmail, cal.Errors[0].Reason)
	}

	events := make([]Event, 0, len(cal.Busy))
	for _, b := range cal.Busy {
		start, err1 := time.Parse(time.RFC3339, b.Start)
		end, err2 := time.Parse(time.RFC3339, b.End)
		if err1 != nil || err2 != nil {
			continue
		}
		// Free/busy gives no title — render as a generic "Busy" block.
		events = append(events, Event{Start: start.UTC(), End: end.UTC()})
	}
	return normalize(roomEmail, events, from, time.Now()), nil
}

var _ Provider = (*GoogleProvider)(nil)
