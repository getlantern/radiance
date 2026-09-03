package usermessage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	wire "github.com/getlantern/common/usermessage"

	"github.com/getlantern/radiance/common"
)

func TestServiceBackoffAndReset(t *testing.T) {
	backoff := newRecordingBackoff()
	useFailureBackoff(t, backoff)
	fetcher := newScriptedFetcher()
	var logs safeBuffer
	service := newTestServiceWithLogger(
		t,
		fetcher,
		func() ClientContext { return testClientContext() },
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service.Start(ctx)

	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{err: errors.New("offline")}
	require.Equal(t, "wait", receiveBackoffCall(t, backoff))
	requireLogContains(t, &logs, "failure_count=1")

	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{err: errors.New("still offline")}
	require.Equal(t, "wait", receiveBackoffCall(t, backoff))
	requireLogContains(t, &logs, "failure_count=2")

	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{PollIntervalSeconds: 300}}
	require.Equal(t, "reset", receiveBackoffCall(t, backoff))
	requireLogContains(t, &logs, "result=no_message")
	logs.Reset()

	// The success parked the loop on the server's interval, so wake it instead
	// of waiting that out.
	service.Refresh()
	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{err: errors.New("failed after success")}
	require.Equal(t, "wait", receiveBackoffCall(t, backoff))
	requireLogContains(t, &logs, "failure_count=1")
}

func TestServiceUsesServerInterval(t *testing.T) {
	fetcher := newScriptedFetcher()
	service := newTestService(t, fetcher, func() ClientContext { return testClientContext() })

	fetcher.results <- fetchResult{response: wire.UserMessageResponse{PollIntervalSeconds: 300}}
	delay, err := service.fetchAndStore(context.Background())
	require.NoError(t, err)
	// Spreading load across clients is the server's job, so the interval is
	// used as sent.
	require.Equal(t, 5*time.Minute, delay)
}

func TestServiceRefreshAndActivity(t *testing.T) {
	service, fetcher, setContext := startService(t, testClientContext())

	require.Equal(t, "fa-IR", receiveFetch(t, fetcher).clientContext.Locale)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{PollIntervalSeconds: 300}}
	requireNoFetch(t, fetcher)

	setContext(func(c *ClientContext) { c.Locale = "en-US" })
	service.Refresh()
	require.Equal(t, "en-US", receiveFetch(t, fetcher).clientContext.Locale)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{PollIntervalSeconds: 300}}

	service.SetActivity(false, true)
	requireNoFetch(t, fetcher)
	service.SetActivity(true, true)
	require.Equal(t, "en-US", receiveFetch(t, fetcher).clientContext.Locale)
}

func TestServiceSeenAndExpiry(t *testing.T) {
	service, fetcher, setContext := startService(t, testClientContext())

	first := receiveFetch(t, fetcher)
	require.Empty(t, first.seen)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{
		PollIntervalSeconds: 300,
		Message:             testMessage("display-1", time.Now().Add(time.Hour)),
	}}
	requireCurrentDisplayID(t, service, "display-1")
	require.NoError(t, service.Acknowledge("display-1"))
	request := receiveFetch(t, fetcher)
	require.Equal(t, []string{"display-1"}, request.seen)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{PollIntervalSeconds: 300}}

	setContext(func(c *ClientContext) { c.UserID = "67890" })
	service.Refresh()
	request = receiveFetch(t, fetcher)
	require.Empty(t, request.seen)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{
		PollIntervalSeconds: 300,
		Message:             testMessage("display-2", time.Now().Add(200*time.Millisecond)),
	}}
	requireCurrentDisplayID(t, service, "display-2")
	require.Eventually(t, func() bool {
		message, err := service.Current()
		return err == nil && message == nil
	}, time.Second, time.Millisecond)
}

func TestServiceDiscardsStaleResponse(t *testing.T) {
	// Regression guard: a response fetched under one account must never be
	// surfaced after the account changed while the fetch was in flight.
	service, fetcher, setContext := startService(t, testClientContext())

	require.Equal(t, "12345", receiveFetch(t, fetcher).clientContext.UserID)
	setContext(func(c *ClientContext) { c.UserID = "67890" })
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{
		PollIntervalSeconds: 300,
		Message:             testMessage("display-1", time.Now().Add(time.Hour)),
	}}

	require.Equal(t, "67890", receiveFetch(t, fetcher).clientContext.UserID)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{PollIntervalSeconds: 300}}
	requireNoFetch(t, fetcher)

	message, err := service.Current()
	require.NoError(t, err)
	require.Nil(t, message)

	setContext(func(c *ClientContext) { c.UserID = "12345" })
	message, err = service.Current()
	require.NoError(t, err)
	require.Nil(t, message)
}

func TestServiceRefreshCancelsInFlightFetch(t *testing.T) {
	// Regression guard: Refresh must abort the in-flight fetch so a context
	// change is picked up immediately, not after the current request returns.
	service, fetcher, setContext := startService(t, testClientContext())

	require.Equal(t, "12345", receiveFetch(t, fetcher).clientContext.UserID)

	// Refresh while the first fetch is still blocked: it must be cancelled, not
	// awaited. The next fetch runs without any result delivered to the first.
	setContext(func(c *ClientContext) { c.UserID = "67890" })
	service.Refresh()
	require.Equal(t, "67890", receiveFetch(t, fetcher).clientContext.UserID)
}

func TestServiceDiscardsAckedReoffer(t *testing.T) {
	// Regression guard: a message acknowledged while a fetch is in flight must
	// not be resurfaced when that fetch re-offers it.
	service, fetcher, _ := startService(t, testClientContext())

	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{
		PollIntervalSeconds: 300,
		Message:             testMessage("display-1", time.Now().Add(time.Hour)),
	}}
	requireCurrentDisplayID(t, service, "display-1")

	// Put a second fetch in flight, then acknowledge display-1 before it returns.
	service.Refresh()
	require.Empty(t, receiveFetch(t, fetcher).seen)
	require.NoError(t, service.Acknowledge("display-1"))
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{
		PollIntervalSeconds: 300,
		Message:             testMessage("display-1", time.Now().Add(time.Hour)),
	}}

	// The ack's own refresh drives a third fetch that now reports display-1 seen.
	require.Equal(t, []string{"display-1"}, receiveFetch(t, fetcher).seen)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{PollIntervalSeconds: 300}}

	message, err := service.Current()
	require.NoError(t, err)
	require.Nil(t, message)
}

func TestServiceRecoversWhenCredentialsBecomeAvailable(t *testing.T) {
	// Regression guard: while credentials are incomplete the loop issues no
	// request; once they become valid it fetches — on its own with nothing
	// waking it (self-recheck), and immediately on an explicit refresh.
	for _, wake := range []bool{false, true} {
		name := "self-recheck"
		if wake {
			name = "on refresh"
		}
		t.Run(name, func(t *testing.T) {
			if !wake {
				// Nothing wakes the loop, so a short backoff keeps the
				// self-recheck from spinning on real time.
				useFailureBackoff(t, common.NewBackoff(time.Millisecond, 10*time.Millisecond))
			}
			service, fetcher, setContext := startService(t, ClientContext{})

			// Invalid credentials short-circuit before the fetcher, so no
			// request is made until they become valid.
			requireNoFetch(t, fetcher)

			setContext(func(c *ClientContext) { *c = testClientContext() })
			if wake {
				service.Refresh()
			}
			require.Equal(t, "12345", receiveFetch(t, fetcher).clientContext.UserID)
		})
	}
}

func TestServiceRefreshDuringBackoff(t *testing.T) {
	service, fetcher, _ := startService(t, testClientContext())

	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{err: errors.New("offline")}

	// The loop is now parked on the default ladder, whose first rung is far
	// longer than this test waits.
	requireNoFetch(t, fetcher)
	service.Refresh()
	receiveFetch(t, fetcher)
}

func TestServiceSafeLogging(t *testing.T) {
	useFailureBackoff(t, immediateBackoff{})
	fetcher := newScriptedFetcher()
	var logs safeBuffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	service, err := New(Options{
		DataDir:         t.TempDir(),
		Fetcher:         fetcher,
		ContextProvider: func() ClientContext { return testClientContext() },
		Logger:          logger,
	})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service.Start(ctx)

	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{err: &httpStatusError{statusCode: http.StatusUnauthorized}}
	requireLogContains(t, &logs, "category=authentication")
	requireLogContains(t, &logs, "http_status=401")
	service.Refresh()

	receiveFetch(t, fetcher)
	fetcher.results <- fetchResult{response: wire.UserMessageResponse{
		PollIntervalSeconds: 300,
		Message:             testMessage("display-1", time.Now().Add(time.Hour)),
	}}
	requireLogContains(t, &logs, "result=message_available")
	require.NotContains(t, logs.String(), "12345")
	require.NotContains(t, logs.String(), "secret-token")
	require.NotContains(t, logs.String(), "A safe localized message")
}

func TestServiceStopsOnCancel(t *testing.T) {
	fetcher := newScriptedFetcher()
	service := newTestService(t, fetcher, func() ClientContext { return testClientContext() })
	ctx, cancel := context.WithCancel(context.Background())
	service.Start(ctx)
	receiveFetch(t, fetcher)
	cancel()

	requireNoFetch(t, fetcher)
}

func TestContextNormalization(t *testing.T) {
	require.Equal(t, "macos", NormalizePlatform("darwin"))
	require.Equal(t, "windows", NormalizePlatform(" Windows "))
	require.Equal(t, "fa-IR", NormalizeLocale("fa-ir"))
	require.Equal(t, "en-US", NormalizeLocale("not a locale"))
}

type fetchCall struct {
	clientContext ClientContext
	seen          []string
}

type fetchResult struct {
	response wire.UserMessageResponse
	err      error
}

type scriptedFetcher struct {
	requests chan fetchCall
	results  chan fetchResult
}

func newScriptedFetcher() *scriptedFetcher {
	return &scriptedFetcher{
		requests: make(chan fetchCall, 8),
		results:  make(chan fetchResult, 8),
	}
}

func (f *scriptedFetcher) Fetch(
	ctx context.Context,
	clientContext ClientContext,
	seen []string,
) (wire.UserMessageResponse, error) {
	select {
	case f.requests <- fetchCall{clientContext: clientContext, seen: seen}:
	case <-ctx.Done():
		return wire.UserMessageResponse{}, ctx.Err()
	}
	select {
	case result := <-f.results:
		return result.response, result.err
	case <-ctx.Done():
		return wire.UserMessageResponse{}, ctx.Err()
	}
}

func newTestService(
	t *testing.T,
	fetcher Fetcher,
	provider func() ClientContext,
) *Service {
	t.Helper()
	return newTestServiceWithLogger(t, fetcher, provider, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// startService returns a started service and its scripted fetcher, with the
// client context seeded to initial. setContext mutates that context under the
// provider's lock, for tests that change account or credentials mid-run.
func startService(t *testing.T, initial ClientContext) (*Service, *scriptedFetcher, func(func(*ClientContext))) {
	t.Helper()
	fetcher := newScriptedFetcher()
	var mu sync.Mutex
	current := initial
	service := newTestService(t, fetcher, func() ClientContext {
		mu.Lock()
		defer mu.Unlock()
		return current
	})
	setContext := func(mutate func(*ClientContext)) {
		mu.Lock()
		mutate(&current)
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service.Start(ctx)
	return service, fetcher, setContext
}

func newTestServiceWithLogger(
	t *testing.T,
	fetcher Fetcher,
	provider func() ClientContext,
	logger *slog.Logger,
) *Service {
	t.Helper()
	service, err := New(Options{
		DataDir:         t.TempDir(),
		Fetcher:         fetcher,
		ContextProvider: provider,
		Logger:          logger,
	})
	require.NoError(t, err)
	return service
}

func receiveFetch(t *testing.T, fetcher *scriptedFetcher) fetchCall {
	t.Helper()
	select {
	case request := <-fetcher.requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fetch")
		return fetchCall{}
	}
}

func requireNoFetch(t *testing.T, fetcher *scriptedFetcher) {
	t.Helper()
	require.Never(t, func() bool {
		select {
		case <-fetcher.requests:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, time.Millisecond)
}

func requireLogContains(t *testing.T, logs interface{ String() string }, substring string) {
	t.Helper()
	require.Eventuallyf(t, func() bool {
		return strings.Contains(logs.String(), substring)
	}, time.Second, time.Millisecond, "logs:\n%s", logs.String())
}

func requireCurrentDisplayID(t *testing.T, service *Service, displayID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		message, err := service.Current()
		return err == nil && message != nil && message.DisplayID == displayID
	}, time.Second, time.Millisecond)
}

func useFailureBackoff(t *testing.T, backoff failureBackoff) {
	t.Helper()
	previous := newFailureBackoff
	newFailureBackoff = func() failureBackoff { return backoff }
	t.Cleanup(func() { newFailureBackoff = previous })
}

type immediateBackoff struct{}

func (immediateBackoff) WaitOn(context.Context, <-chan struct{}) {}
func (immediateBackoff) Reset()                                  {}

type recordingBackoff struct {
	calls chan string
}

func newRecordingBackoff() *recordingBackoff {
	return &recordingBackoff{calls: make(chan string, 16)}
}

func (b *recordingBackoff) WaitOn(context.Context, <-chan struct{}) { b.calls <- "wait" }

func (b *recordingBackoff) Reset() { b.calls <- "reset" }

func receiveBackoffCall(t *testing.T, backoff *recordingBackoff) string {
	t.Helper()
	select {
	case call := <-backoff.calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for backoff call")
		return ""
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *safeBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}
