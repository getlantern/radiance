// Package helper fetches and renews this device's helper pairing tickets: the
// short-lived credentials lantern-cloud issues for each pairing the device is
// in, which put it in the pairing's room on freddie. A volunteer widget sends
// its volunteer tickets with every genesis message; an Unbounded consumer sends
// its inviter tickets when it subscribes.
//
// Tickets are credentials: nothing here logs them or a response body.
package helper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/broflake/common/helperticket"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
)

const (
	// RefreshInterval is how long after a successful fetch the next is due.
	// Tickets live 15 minutes, so readers always hold valid ones.
	RefreshInterval = 10 * time.Minute
	// RetryInterval is how long after a failed fetch the next is due. The
	// last tickets are kept meanwhile.
	RetryInterval = time.Minute
	// DisabledInterval is how long after a 404, which means helper_invites is
	// off for this client, the next fetch is due.
	DisabledInterval = time.Hour

	fetchTimeout = 30 * time.Second
	maxBody      = 1 << 20
)

// ErrDisabled is Fetch's error when lantern-cloud has helper invites off for
// this client.
var ErrDisabled = errors.New("helper invites are off for this client")

// Tickets are a device's pairing tickets, by role.
type Tickets struct {
	Inviter   []string
	Volunteer []string
}

// Split sorts tickets by the role each claims. It does not verify them:
// freddie does.
func Split(tickets []string) Tickets {
	var t Tickets
	for _, tk := range tickets {
		switch role, _ := helperticket.Peek(tk); role {
		case helperticket.RoleInviter:
			t.Inviter = append(t.Inviter, tk)
		case helperticket.RoleVolunteer:
			t.Volunteer = append(t.Volunteer, tk)
		}
	}
	return t
}

// Fetcher asks lantern-cloud for this device's tickets.
type Fetcher struct {
	httpClient *http.Client
	baseURL    string
}

// NewFetcher returns a Fetcher. baseURL includes the API version segment,
// as common.GetBaseURL() returns it.
func NewFetcher(httpClient *http.Client, baseURL string) *Fetcher {
	return &Fetcher{httpClient: httpClient, baseURL: baseURL}
}

// Fetch calls POST {baseURL}/helper/tickets, authenticated by the device ID
// header like the peer API, and splits the tickets by role.
func (f *Fetcher) Fetch(ctx context.Context) (Tickets, error) {
	r, err := common.NewRequestWithHeaders(ctx, http.MethodPost, f.baseURL+"/helper/tickets", strings.NewReader("{}"))
	if err != nil {
		return Tickets{}, fmt.Errorf("build ticket request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	if val := settings.GetString(settings.FeatureOverridesKey); val != "" {
		r.Header.Set("X-Lantern-Feature-Override", val)
	}
	resp, err := f.httpClient.Do(r)
	if err != nil {
		return Tickets{}, fmt.Errorf("send ticket request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Tickets{}, ErrDisabled
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return Tickets{}, fmt.Errorf("ticket request: status %d", resp.StatusCode)
	}
	var body struct {
		Tickets []string `json:"tickets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&body); err != nil {
		return Tickets{}, errors.New("ticket response: malformed JSON")
	}
	return Split(body.Tickets), nil
}

// Source keeps a device's tickets fresh for code that reads them on its
// signaling path. A read never blocks: it returns what the last fetch got,
// and when a fetch is due it starts one in the background. So a reader that
// keeps reading always holds valid tickets, and one that paused for longer
// than a ticket's life sends stale ones once (freddie ignores them) and fresh
// ones from its next read after the fetch. Nothing is fetched until the first
// read, so a device that never reads never calls the API.
type Source struct {
	fetch func(context.Context) (Tickets, error)
	now   func() time.Time

	mu       sync.Mutex
	tickets  Tickets
	due      time.Time // zero: due at the first read
	fetching bool
}

// NewSource returns a Source that fetches with fetch, Fetcher.Fetch outside
// tests.
func NewSource(fetch func(context.Context) (Tickets, error)) *Source {
	return &Source{fetch: fetch, now: time.Now}
}

// Inviter returns the current inviter tickets.
func (s *Source) Inviter() []string {
	return s.read(func(t Tickets) []string { return t.Inviter })
}

// Volunteer returns the current volunteer tickets.
func (s *Source) Volunteer() []string {
	return s.read(func(t Tickets) []string { return t.Volunteer })
}

func (s *Source) read(pick func(Tickets) []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fetching && !s.now().Before(s.due) {
		s.fetching = true
		go s.refresh()
	}
	return slices.Clone(pick(s.tickets))
}

func (s *Source) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	t, err := s.fetch(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetching = false
	switch {
	case errors.Is(err, ErrDisabled):
		s.tickets = Tickets{}
		s.due = s.now().Add(DisabledInterval)
	case err != nil:
		slog.Debug("helper: fetching pairing tickets failed; keeping the last ones", "error", err)
		s.due = s.now().Add(RetryInterval)
	default:
		s.tickets = t
		s.due = s.now().Add(RefreshInterval)
	}
}

// isFetching reports whether a fetch is in flight. For tests.
func (s *Source) isFetching() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetching
}
