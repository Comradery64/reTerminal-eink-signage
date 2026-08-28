package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// freeBusyScope is the minimal scope: read free/busy intervals only — never event titles/details.
const freeBusyScope = "https://www.googleapis.com/auth/calendar.freebusy"

// eventsScope is the wider scope needed to render meeting titles (config google.detail_level:
// "titles"). It lets the broker read full event detail on every calendar shared with it, so it is
// opt-in and never the default — see config.GoogleConfig.DetailLevel for the trade-off.
const eventsScope = "https://www.googleapis.com/auth/calendar.events.readonly"

// GoogleProvider reads room availability via the Calendar freeBusy API. It authenticates KEYLESS
// via Workload Identity Federation (external-account credentials): the broker's projected k8s
// service-account token is exchanged for short-lived Google credentials — no JSON key at rest, no
// domain-wide delegation, no user impersonation. The service account reads the rooms it has been
// granted freeBusyReader on (shared directly per room).
type GoogleProvider struct {
	http *http.Client
	// titles is true when the provider was built for detail_level "titles" and may call the events
	// API. False means it holds only the freebusy scope and physically cannot read a title.
	titles bool
	// downgraded latches when the events API refuses (typically the room is shared as
	// freeBusyReader rather than reader, so the wider scope was granted but the calendar was not).
	// Rather than leaving panels blank, the provider falls back to free/busy for the rest of the
	// process lifetime and says so loudly once. Fix the sharing, restart to re-arm.
	downgraded atomic.Bool
	log        *slog.Logger
}

// NewGoogle builds the provider from an external-account credential config (WIF). If credConfig is
// empty it falls back to Application Default Credentials (the GOOGLE_APPLICATION_CREDENTIALS env,
// which in k8s points at the mounted cred-config that references the projected token).
func NewGoogle(ctx context.Context, credConfig, detailLevel string, log *slog.Logger) (*GoogleProvider, error) {
	if log == nil {
		log = slog.Default()
	}
	titles := detailLevel == "titles"
	scope := freeBusyScope
	if titles {
		scope = eventsScope
	}
	var creds *google.Credentials
	var err error
	if credConfig != "" {
		var data []byte
		if data, err = os.ReadFile(credConfig); err != nil {
			return nil, fmt.Errorf("google: read cred config: %w", err)
		}
		creds, err = google.CredentialsFromJSON(ctx, data, scope)
	} else {
		creds, err = google.FindDefaultCredentials(ctx, scope)
	}
	if err != nil {
		return nil, fmt.Errorf("google: resolve WIF credentials: %w", err)
	}
	hc := oauth2.NewClient(ctx, creds.TokenSource)
	hc.Timeout = 15 * time.Second
	log.Info("google calendar provider ready", "detail_level", detailLevel, "scope", scope)
	return &GoogleProvider{http: hc, titles: titles, log: log}, nil
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
func (p *GoogleProvider) fetchFreeBusy(ctx context.Context, roomEmail string, from, to time.Time) (*Schedule, error) {
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

// FetchSchedule returns the room's schedule at the configured detail level. With detail_level
// "free_busy" (the default) this is the freeBusy API and no title can ever be returned. With
// "titles" it is the events API, falling back permanently to free/busy if that call is refused —
// a wall display showing "Busy" is a far better failure than one showing nothing.
func (p *GoogleProvider) FetchSchedule(ctx context.Context, roomEmail string, from, to time.Time) (*Schedule, error) {
	if !p.titles || p.downgraded.Load() {
		return p.fetchFreeBusy(ctx, roomEmail, from, to)
	}
	sched, err := p.fetchEvents(ctx, roomEmail, from, to)
	if err == nil {
		return sched, nil
	}
	if !isPermissionErr(err) {
		return nil, err // transient/unknown: surface it, don't silently degrade detail
	}
	// Latch once. The usual cause is a room still shared as freeBusyReader while detail_level says
	// "titles": the scope was widened but the calendar was not. Logged at ERROR because the
	// operator asked for titles and is not getting them, and nothing else would say so.
	if p.downgraded.CompareAndSwap(false, true) {
		p.log.Error("events API refused; falling back to free/busy for the rest of this process — "+
			"share each room with the service account as 'reader' (not freeBusyReader) to show titles, "+
			"then restart the broker to retry",
			"room", roomEmail, "err", err)
	}
	return p.fetchFreeBusy(ctx, roomEmail, from, to)
}

// isPermissionErr reports whether err is the API refusing us, as opposed to a network blip or a
// 5xx. Matching on the status this package itself formats keeps the classifier honest: these are
// the only two codes Google returns for "you may not read this calendar".
func isPermissionErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "status 403") || strings.Contains(s, "status 404")
}

type eventsResponse struct {
	Items []struct {
		Summary    string `json:"summary"`
		Visibility string `json:"visibility"`
		Status     string `json:"status"`
		Organizer  struct {
			DisplayName string `json:"displayName"`
			Email       string `json:"email"`
		} `json:"organizer"`
		Start struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"start"`
		End struct {
			DateTime string `json:"dateTime"`
			Date     string `json:"date"`
		} `json:"end"`
	} `json:"items"`
}

// fetchEvents reads real events, so Subject/Organizer are populated and panels can show a title.
// singleEvents=true expands recurring series into concrete instances, which is what a room display
// needs — an unexpanded weekly standup would otherwise render once and then never again.
func (p *GoogleProvider) fetchEvents(ctx context.Context, roomEmail string, from, to time.Time) (*Schedule, error) {
	q := url.Values{}
	q.Set("timeMin", from.UTC().Format(time.RFC3339))
	q.Set("timeMax", to.UTC().Format(time.RFC3339))
	q.Set("singleEvents", "true")
	q.Set("orderBy", "startTime")
	q.Set("maxResults", "50")
	endpoint := "https://www.googleapis.com/calendar/v3/calendars/" + url.PathEscape(roomEmail) + "/events?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("events request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("events status %d for %s", resp.StatusCode, roomEmail)
	}

	var er eventsResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("events decode: %w", err)
	}

	events := make([]Event, 0, len(er.Items))
	for _, it := range er.Items {
		if it.Status == "cancelled" {
			continue
		}
		// All-day entries carry Date, not DateTime, and a room blocked all day is still busy —
		// but it has no clock times to render, so treat it as an untimed skip rather than
		// inventing midnight-to-midnight bounds that would swamp the panel.
		if it.Start.DateTime == "" || it.End.DateTime == "" {
			continue
		}
		start, err1 := time.Parse(time.RFC3339, it.Start.DateTime)
		end, err2 := time.Parse(time.RFC3339, it.End.DateTime)
		if err1 != nil || err2 != nil {
			continue
		}
		organizer := it.Organizer.DisplayName
		if organizer == "" {
			organizer = it.Organizer.Email
		}
		events = append(events, Event{
			Subject:   it.Summary,
			Organizer: organizer,
			Start:     start,
			End:       end,
			// Honor the event's own sensitivity: render.meetingTitle turns this into
			// "Private meeting" rather than leaking the subject onto a corridor wall.
			Private: it.Visibility == "private" || it.Visibility == "confidential",
		})
	}
	return normalize(roomEmail, events, from, time.Now()), nil
}
