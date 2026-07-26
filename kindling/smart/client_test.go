package smart

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubSmartTransport replaces the strategy search for the duration of a test.
// fn decides, per host, whether that host turned out to be reachable.
func stubSmartTransport(t *testing.T, fn func(host string) (http.RoundTripper, error)) {
	t.Helper()
	original := newSmartTransport
	newSmartTransport = func(_ io.Writer, host string) (http.RoundTripper, error) {
		return fn(host)
	}
	t.Cleanup(func() { newSmartTransport = original })
}

// echoHostRoundTripper answers with the host whose transport served the
// request, so a test can tell which dialer a request went through.
type echoHostRoundTripper struct {
	host string
}

func (e echoHostRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(e.host)),
		Request:    req,
	}, nil
}

func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	res, err := client.Get(url)
	require.NoError(t, err)
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	return string(body)
}

func TestBlockedHostDoesNotBlockOthers(t *testing.T) {
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		if host == "blocked.example" {
			return nil, errors.New("could not find a working strategy")
		}
		return echoHostRoundTripper{host: host}, nil
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://good.example/config", "https://blocked.example/config.gz")
	require.NoError(t, err)

	assert.Equal(t, "good.example", getBody(t, client, "https://good.example/config"))

	_, err = client.Get("https://blocked.example/config.gz")
	require.Error(t, err)

	assert.Equal(t, "good.example", getBody(t, client, "https://good.example/config"),
		"a host with no working strategy must not disturb a reachable one")
}

func TestSlowSearchDoesNotDelayOtherHosts(t *testing.T) {
	// The slow host's search blocks until cleanup rather than sleeping: it
	// cannot finish while the test runs, so the fast host answering at all
	// proves its request never waited on it — no wall-clock race on a loaded
	// CI runner.
	slowSearching := make(chan struct{})
	t.Cleanup(func() { close(slowSearching) })
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		if host == "slow.example" {
			<-slowSearching
		}
		return echoHostRoundTripper{host: host}, nil
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://slow.example/config", "https://fast.example/config")
	require.NoError(t, err)

	// Bounded so a regression fails the test instead of hanging the suite.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://fast.example/config", nil)
	require.NoError(t, err)
	res, err := client.Do(req)
	require.NoError(t, err, "a search for one host must stay off another host's request path")
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	require.NoError(t, err)
	assert.Equal(t, "fast.example", string(body))
}

func TestFailedSearchIsRetried(t *testing.T) {
	var blocked atomic.Bool
	blocked.Store(true)
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		if blocked.Load() {
			return nil, errors.New("offline")
		}
		return echoHostRoundTripper{host: host}, nil
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://config.example/config")
	require.NoError(t, err)

	_, err = client.Get("https://config.example/config")
	require.Error(t, err)

	blocked.Store(false)
	assert.Equal(t, "config.example", getBody(t, client, "https://config.example/config"),
		"a host must be able to recover without a restart")
}

func TestConcurrentRequestsShareOneSearch(t *testing.T) {
	var searches atomic.Int32
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		searches.Add(1)
		time.Sleep(50 * time.Millisecond)
		return echoHostRoundTripper{host: host}, nil
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://config.example/config")
	require.NoError(t, err)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := client.Get("https://config.example/config")
			if assert.NoError(t, err) {
				res.Body.Close()
			}
		}()
	}
	wg.Wait()

	assert.EqualValues(t, 1, searches.Load(), "a host searches once, however many requests are waiting on it")
}

// TestSuccessfulSearchIsRemembered issues its requests one after another, so
// that each has already settled when the next arrives: concurrent requests
// would collapse into one search whether or not the result is ever kept.
func TestSuccessfulSearchIsRemembered(t *testing.T) {
	var searches atomic.Int32
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		searches.Add(1)
		return echoHostRoundTripper{host: host}, nil
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://config.example/config")
	require.NoError(t, err)

	for range 5 {
		assert.Equal(t, "config.example", getBody(t, client, "https://config.example/config"))
	}

	assert.EqualValues(t, 1, searches.Load(),
		"a host with a working transport must not search again")
}

func TestConstructionStartsTheSearchWithoutBlocking(t *testing.T) {
	// The search blocks until cleanup rather than sleeping: it cannot finish
	// while the test runs, so construction returning at all proves it does not
	// wait on the search (radiance has to start while offline). A regression
	// deadlocks construction and fails via the test timeout.
	searched := make(chan string, 1)
	searching := make(chan struct{})
	t.Cleanup(func() { close(searching) })
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		searched <- host
		<-searching
		return echoHostRoundTripper{host: host}, nil
	})

	_, err := NewHTTPClientWithSmartTransport(io.Discard, "https://config.example/config")
	require.NoError(t, err)

	select {
	case host := <-searched:
		assert.Equal(t, "config.example", host)
	case <-time.After(5 * time.Second):
		t.Fatal("the search has to be under way before a caller waits on it")
	}
}

func TestRequestGivesUpOnSearchWhenItsContextEnds(t *testing.T) {
	neverFinishes := make(chan struct{})
	t.Cleanup(func() { close(neverFinishes) })
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		<-neverFinishes
		return echoHostRoundTripper{host: host}, nil
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://config.example/config")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://config.example/config", nil)
	require.NoError(t, err)

	start := time.Now()
	_, err = client.Do(req)
	require.Error(t, err)
	assert.Less(t, time.Since(start), time.Second, "a request must be able to abandon a search it is waiting on")
}

// TestSearchStarterGivesUpWhenItsContextEnds covers the caller that starts the
// search rather than joining one, which has no other way out of a search that
// never returns.
func TestSearchStarterGivesUpWhenItsContextEnds(t *testing.T) {
	neverFinishes := make(chan struct{})
	t.Cleanup(func() { close(neverFinishes) })

	ht := &hostTransport{
		host:      "config.example",
		logWriter: io.Discard,
		newTransport: func(io.Writer, string) (http.RoundTripper, error) {
			<-neverFinishes
			return nil, nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ht.roundTripper(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
}

func TestPanickingSearchBecomesAnError(t *testing.T) {
	stubSmartTransport(t, func(string) (http.RoundTripper, error) {
		panic("strategy search exploded")
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://config.example/config")
	require.NoError(t, err)

	_, err = client.Get("https://config.example/config")
	require.ErrorContains(t, err, "panicked",
		"a panicking search must not take the process down with it")
}

func TestUnknownHostUsesFirstConfiguredTransport(t *testing.T) {
	stubSmartTransport(t, func(host string) (http.RoundTripper, error) {
		return echoHostRoundTripper{host: host}, nil
	})

	client, err := NewHTTPClientWithSmartTransport(io.Discard, "https://first.example/config", "https://second.example/config")
	require.NoError(t, err)

	assert.Equal(t, "first.example", getBody(t, client, "https://elsewhere.example/config"))
}

func TestConfigHosts(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []string
		hosts     []string
		wantErr   bool
	}{
		{
			name:      "single URL",
			addresses: []string{"https://config.example/config"},
			hosts:     []string{"config.example"},
		},
		{
			name:      "port is not part of the host",
			addresses: []string{"https://config.example:8443/config"},
			hosts:     []string{"config.example"},
		},
		{
			name:      "repeated host is dialed by one transport",
			addresses: []string{"https://config.example/a", "https://config.example/b"},
			hosts:     []string{"config.example"},
		},
		{
			name:      "hostless address is skipped",
			addresses: []string{"config.example/a", "https://config.example/b"},
			hosts:     []string{"config.example"},
		},
		{
			name:      "no host at all",
			addresses: []string{"config.example/a"},
			wantErr:   true,
		},
		{
			name:      "no addresses",
			addresses: nil,
			wantErr:   true,
		},
		{
			name:      "unparseable URL",
			addresses: []string{"https://config.example/\x7f"},
			wantErr:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			hosts, err := configHosts(test.addresses)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, test.hosts, hosts)
		})
	}
}
