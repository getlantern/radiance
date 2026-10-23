package helper

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getlantern/broflake/common/helperticket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/common"
	"github.com/getlantern/radiance/common/settings"
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "radiance-helper-settings-*")
	if err != nil {
		panic(err)
	}
	if err := settings.InitSettings(dir); err != nil {
		os.RemoveAll(dir)
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// testTickets returns an inviter and a volunteer ticket for one pairing.
func testTickets(t *testing.T) (inviter, volunteer string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	sign := func(role helperticket.Role) string {
		tk, err := helperticket.Sign(priv, helperticket.Ticket{
			PairingID: "6f1c2b0e-8a7d-4c1e-9f00-0123456789ab", Role: role, Expires: time.Now().Add(15 * time.Minute)})
		require.NoError(t, err)
		return tk
	}
	return sign(helperticket.RoleInviter), sign(helperticket.RoleVolunteer)
}

func TestFetcherSplitsTicketsByRole(t *testing.T) {
	inv, vol := testTickets(t)
	require.NoError(t, settings.Set(settings.DeviceIDKey, "device-A"))
	var method, path, device string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, device = r.Method, r.URL.Path, r.Header.Get(common.DeviceIDHeader)
		json.NewEncoder(w).Encode(map[string]any{"tickets": []string{vol, inv, "junk"}, "lifetime_seconds": 900})
	}))
	defer srv.Close()

	got, err := NewFetcher(srv.Client(), srv.URL+"/v1").Fetch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/v1/helper/tickets", path)
	assert.Equal(t, "device-A", device)
	assert.Equal(t, Tickets{Inviter: []string{inv}, Volunteer: []string{vol}}, got)
}

func TestFetcherNotFoundMeansDisabled(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := NewFetcher(srv.Client(), srv.URL+"/v1").Fetch(context.Background())
	assert.ErrorIs(t, err, ErrDisabled)
}

// The response body is tickets, so an error never quotes it.
func TestFetcherErrorOmitsTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("eyJzZWNyZXQi.c2ln"))
	}))
	defer srv.Close()
	_, err := NewFetcher(srv.Client(), srv.URL+"/v1").Fetch(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "eyJzZWNyZXQi")
}

// fakeFetch hands out results the test sends, and counts calls.
type fakeFetch struct {
	calls   atomic.Int32
	results chan fetchResult
}

type fetchResult struct {
	t   Tickets
	err error
}

func (f *fakeFetch) fetch(ctx context.Context) (Tickets, error) {
	f.calls.Add(1)
	r := <-f.results
	return r.t, r.err
}

// fakeClock is a settable now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func TestSourceRefreshesOnRead(t *testing.T) {
	inv, vol := testTickets(t)
	f := &fakeFetch{results: make(chan fetchResult)}
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	src := NewSource(f.fetch)
	src.now = clock.now

	assert.Empty(t, src.Inviter(), "the first read starts a fetch and does not wait for it")
	require.Eventually(t, func() bool { return f.calls.Load() == 1 }, time.Second, time.Millisecond)
	f.results <- fetchResult{t: Tickets{Inviter: []string{inv}, Volunteer: []string{vol}}}
	require.Eventually(t, func() bool { return len(src.Inviter()) == 1 }, time.Second, time.Millisecond)
	assert.Equal(t, []string{vol}, src.Volunteer())

	clock.add(RefreshInterval - time.Second)
	src.Inviter()
	assert.Never(t, func() bool { return f.calls.Load() > 1 }, 50*time.Millisecond, time.Millisecond, "fetched before the refresh was due")

	clock.add(time.Second)
	src.Volunteer()
	require.Eventually(t, func() bool { return f.calls.Load() == 2 }, time.Second, time.Millisecond)
	f.results <- fetchResult{err: errors.New("network down")}
	require.Eventually(t, func() bool { return !src.isFetching() }, time.Second, time.Millisecond)
	assert.Equal(t, []string{inv}, src.Inviter(), "a failed fetch keeps the last tickets")

	clock.add(RetryInterval)
	src.Inviter()
	require.Eventually(t, func() bool { return f.calls.Load() == 3 }, time.Second, time.Millisecond)
	f.results <- fetchResult{err: ErrDisabled}
	require.Eventually(t, func() bool { return len(src.Inviter()) == 0 }, time.Second, time.Millisecond)
	assert.Empty(t, src.Volunteer(), "helper_invites off clears the tickets")

	clock.add(DisabledInterval - time.Second)
	src.Inviter()
	assert.Never(t, func() bool { return f.calls.Load() > 3 }, 50*time.Millisecond, time.Millisecond, "fetched before the disabled backoff ended")
	clock.add(time.Second)
	src.Inviter()
	require.Eventually(t, func() bool { return f.calls.Load() == 4 }, time.Second, time.Millisecond)
	f.results <- fetchResult{}
}

func TestSourceReturnsCopies(t *testing.T) {
	inv, _ := testTickets(t)
	f := &fakeFetch{results: make(chan fetchResult, 1)}
	f.results <- fetchResult{t: Tickets{Inviter: []string{inv}}}
	src := NewSource(f.fetch)
	src.Inviter()
	require.Eventually(t, func() bool { return len(src.Inviter()) == 1 }, time.Second, time.Millisecond)
	got := src.Inviter()
	got[0] = "changed"
	assert.Equal(t, []string{inv}, src.Inviter())
}
