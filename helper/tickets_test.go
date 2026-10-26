package helper

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
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
	assert.Equal(t, Tickets{Inviter: []string{inv}, Volunteer: []string{vol}, Lifetime: 900 * time.Second}, got)
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
	src := NewSource(context.Background(), f.fetch)
	src.now = clock.now

	assert.Empty(t, src.Inviter(), "the first read starts a fetch and does not wait for it")
	require.Eventually(t, func() bool { return f.calls.Load() == 1 }, time.Second, time.Millisecond)
	f.results <- fetchResult{t: Tickets{Inviter: []string{inv}, Volunteer: []string{vol}, Lifetime: 15 * time.Minute}}
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
	src := NewSource(context.Background(), f.fetch)
	src.Inviter()
	require.Eventually(t, func() bool { return len(src.Inviter()) == 1 }, time.Second, time.Millisecond)
	got := src.Inviter()
	got[0] = "changed"
	assert.Equal(t, []string{inv}, src.Inviter())
}

func TestFetcherSendsFeatureOverride(t *testing.T) {
	require.NoError(t, settings.Set(settings.FeatureOverridesKey, "helper_invites"))
	t.Cleanup(func() { _ = settings.Set(settings.FeatureOverridesKey, "") })
	var override string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		override = r.Header.Get("X-Lantern-Feature-Override")
		w.Write([]byte(`{"tickets":[]}`))
	}))
	defer srv.Close()
	_, err := NewFetcher(srv.Client(), srv.URL+"/v1").Fetch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "helper_invites", override)
}

func TestFetcherBusyStatusesAreUnavailable(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }))
		_, err := NewFetcher(srv.Client(), srv.URL+"/v1").Fetch(context.Background())
		srv.Close()
		assert.ErrorIs(t, err, ErrUnavailable, "status %d", code)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer srv.Close()
	_, err := NewFetcher(srv.Client(), srv.URL+"/v1").Fetch(context.Background())
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnavailable)
}

// newTestSource returns a Source on a fake clock, fetching from f.
func newTestSource(f *fakeFetch) (*Source, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	src := NewSource(context.Background(), f.fetch)
	src.now = clock.now
	return src, clock
}

// settle waits for the fetch in flight to finish.
func settle(t *testing.T, src *Source) {
	t.Helper()
	require.Eventually(t, func() bool { return !src.isFetching() }, time.Second, time.Millisecond)
}

func (s *Source) dueIn() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.due.Sub(s.now())
}

func TestSourceDropsExpiredTickets(t *testing.T) {
	inv, vol := testTickets(t)
	f := &fakeFetch{results: make(chan fetchResult, 1)}
	src, clock := newTestSource(f)

	f.results <- fetchResult{t: Tickets{Inviter: []string{inv}, Volunteer: []string{vol}, Lifetime: 15 * time.Minute}}
	src.Inviter()
	settle(t, src)
	require.Equal(t, []string{inv}, src.Inviter())

	clock.add(RefreshInterval)
	for i := 0; i < 4; i++ {
		f.results <- fetchResult{err: errors.New("network down")}
		src.Inviter()
		settle(t, src)
		clock.add(RetryInterval)
	}
	// 14 minutes after the fetch: still valid.
	f.results <- fetchResult{err: errors.New("network down")}
	assert.Equal(t, []string{inv}, src.Inviter())
	settle(t, src)
	clock.add(time.Minute)
	f.results <- fetchResult{err: errors.New("network down")}
	assert.Empty(t, src.Inviter(), "an expired ticket is never served")
	assert.Empty(t, src.Volunteer(), "an expired ticket is never served")
	settle(t, src)
}

func TestSourceExpiryFallsBackToTheClaim(t *testing.T) {
	inv, _ := testTickets(t) // claims expiry at real now + 15m
	f := &fakeFetch{results: make(chan fetchResult, 1)}
	src := NewSource(context.Background(), f.fetch)
	clock := &fakeClock{t: time.Now()}
	src.now = clock.now
	f.results <- fetchResult{t: Tickets{Inviter: []string{inv}}}
	src.Inviter()
	settle(t, src)
	assert.Equal(t, []string{inv}, src.Inviter())
	clock.add(15*time.Minute + time.Second)
	f.results <- fetchResult{err: errors.New("network down")}
	assert.Empty(t, src.Inviter())
	settle(t, src)
}

func TestSourceRecoversFromAPanickingFetch(t *testing.T) {
	var calls atomic.Int32
	src := NewSource(context.Background(), func(context.Context) (Tickets, error) {
		calls.Add(1)
		panic("boom")
	})
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	src.now = clock.now
	src.Inviter()
	settle(t, src)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, RetryInterval, src.dueIn())
	clock.add(RetryInterval)
	src.Inviter()
	settle(t, src)
	assert.Equal(t, int32(2), calls.Load(), "a panic does not stop later fetches")
}

func TestSourceBacksOffWhileBusy(t *testing.T) {
	f := &fakeFetch{results: make(chan fetchResult, 1)}
	src, clock := newTestSource(f)
	step := func(r fetchResult) time.Duration {
		f.results <- r
		src.Inviter()
		settle(t, src)
		d := src.dueIn()
		clock.add(d)
		return d
	}
	busy := fetchResult{err: fmt.Errorf("ticket request: status 429: %w", ErrUnavailable)}
	assert.Equal(t, 2*time.Minute, step(busy))
	assert.Equal(t, 4*time.Minute, step(busy))
	assert.Equal(t, 8*time.Minute, step(busy))
	assert.Equal(t, RefreshInterval, step(busy))
	assert.Equal(t, RefreshInterval, step(busy))
	assert.Equal(t, RetryInterval, step(fetchResult{err: errors.New("network down")}), "an ordinary failure ends the streak")
	assert.Equal(t, 2*time.Minute, step(busy))
	assert.Equal(t, RefreshInterval, step(fetchResult{t: Tickets{Lifetime: 15 * time.Minute}}))
	assert.Equal(t, 2*time.Minute, step(busy), "a success resets the backoff")
}

func TestSourceRefreshFollowsTheLifetime(t *testing.T) {
	f := &fakeFetch{results: make(chan fetchResult, 1)}
	src, _ := newTestSource(f)
	f.results <- fetchResult{t: Tickets{Lifetime: 6 * time.Minute}}
	src.Inviter()
	settle(t, src)
	assert.Equal(t, 4*time.Minute, src.dueIn())
}

func TestSourceFetchesOncePerDuePeriod(t *testing.T) {
	inv, _ := testTickets(t)
	f := &fakeFetch{results: make(chan fetchResult)}
	src, clock := newTestSource(f)
	readAll := func() {
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				src.Inviter()
				src.Volunteer()
			}()
		}
		wg.Wait()
	}
	readAll()
	require.Eventually(t, func() bool { return f.calls.Load() == 1 }, time.Second, time.Millisecond)
	readAll()
	f.results <- fetchResult{t: Tickets{Inviter: []string{inv}, Lifetime: 15 * time.Minute}}
	settle(t, src)
	readAll()
	assert.Never(t, func() bool { return f.calls.Load() > 1 }, 50*time.Millisecond, time.Millisecond)
	clock.add(RefreshInterval)
	readAll()
	require.Eventually(t, func() bool { return f.calls.Load() == 2 }, time.Second, time.Millisecond)
	assert.Never(t, func() bool { return f.calls.Load() > 2 }, 50*time.Millisecond, time.Millisecond)
	f.results <- fetchResult{}
	settle(t, src)
}

func TestSourceCancelsFetchWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	var calls atomic.Int32
	src := NewSource(ctx, func(fctx context.Context) (Tickets, error) {
		calls.Add(1)
		close(started)
		<-fctx.Done()
		return Tickets{}, fctx.Err()
	})
	src.Inviter()
	<-started
	cancel()
	settle(t, src)
	src.now = func() time.Time { return time.Now().Add(time.Hour) }
	src.Inviter()
	assert.Never(t, func() bool { return calls.Load() > 1 }, 50*time.Millisecond, time.Millisecond, "a cancelled Source never fetches again")
}

// Expiry runs from when the request was sent, so a slow response does not
// stretch a ticket's life.
func TestSourceExpiryRunsFromTheRequest(t *testing.T) {
	inv, _ := testTickets(t)
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	start := clock.now()
	var calls atomic.Int32
	src := NewSource(context.Background(), func(context.Context) (Tickets, error) {
		if calls.Add(1) == 1 {
			clock.add(20 * time.Second)
			return Tickets{Inviter: []string{inv}, Lifetime: 15 * time.Minute}, nil
		}
		return Tickets{}, errors.New("network down")
	})
	src.now = clock.now
	src.Inviter()
	settle(t, src)
	clock.add(start.Add(15*time.Minute - time.Second).Sub(clock.now()))
	assert.Equal(t, []string{inv}, src.Inviter())
	settle(t, src)
	clock.add(start.Add(15 * time.Minute).Sub(clock.now()))
	assert.Empty(t, src.Inviter(), "expired 15 minutes after the request, not the response")
	settle(t, src)
}

// Renewal also runs from when the request was sent, so a slow response does
// not eat into the margin before the tickets expire.
func TestSourceRenewalRunsFromTheRequest(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	src := NewSource(context.Background(), func(context.Context) (Tickets, error) {
		clock.add(25 * time.Second)
		return Tickets{Lifetime: 6 * time.Minute}, nil
	})
	src.now = clock.now
	src.Inviter()
	settle(t, src)
	assert.Equal(t, 4*time.Minute-25*time.Second, src.dueIn())
}
