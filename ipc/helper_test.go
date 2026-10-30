package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	semconv "github.com/getlantern/semconv"

	"github.com/getlantern/radiance/backend"
	"github.com/getlantern/radiance/helper"
)

func serveHelper(t *testing.T, api *localapi, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&buf).Encode(body))
	}
	r := httptest.NewRequest(method, path, &buf)
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	return w
}

func TestHelperRelayEndpoint(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	w := serveHelper(t, api, http.MethodPost, helperRelayEndpoint, HelperRelayRequest{Op: "peer/register"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "unknown helper operation\n", w.Body.String())

	w = serveHelper(t, api, http.MethodPost, helperRelayEndpoint, HelperRelayRequest{Op: "pairings"})
	assert.Equal(t, http.StatusBadGateway, w.Code, "a backend without a relayer cannot reach lantern-cloud")
	assert.Equal(t, "helper relay failed\n", w.Body.String(), "the error is fixed text")

	w = serveHelper(t, api, http.MethodGet, helperRelayEndpoint, nil)
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHelperRelayEndpointRejectsAMalformedBody(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	r := httptest.NewRequest(http.MethodPost, helperRelayEndpoint, strings.NewReader(`{"op":`))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "bad helper relay request\n", w.Body.String())
}

// localTransport serves a Client's requests from api in-process.
type localTransport struct{ api *localapi }

func (t localTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	t.api.ServeHTTP(w, r)
	return w.Result(), nil
}

// A relayed request goes from ipc.Client through the endpoint and the
// backend's relayer to lantern-cloud, and lantern-cloud's answer, success or
// not, comes back as a RelayResult.
func TestHelperRelayClientRoundTrip(t *testing.T) {
	var gotPath, gotBody string
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		if r.URL.Path == "/v1/helper/invites/claim" {
			http.Error(w, "invite already claimed or expired", http.StatusConflict)
			return
		}
		_, _ = w.Write([]byte(`{"pairings":[]}`))
	}))
	defer cloud.Close()
	b := &backend.LocalBackend{}
	b.SetHelperRelayForTest(helper.NewRelayer(cloud.Client(), cloud.URL+"/v1", nil))
	c := newClient()
	c.http.Transport = localTransport{api: newLocalAPI(b, false)}

	res, err := c.HelperRelay(context.Background(), "pairings", []byte(`{"x":1}`))
	require.NoError(t, err)
	assert.Equal(t, "/v1/helper/pairings", gotPath)
	assert.JSONEq(t, `{"x":1}`, gotBody)
	assert.Equal(t, http.StatusOK, res.Status)
	assert.JSONEq(t, `{"pairings":[]}`, string(res.Body))
	assert.Empty(t, res.Error)

	res, err = c.HelperRelay(context.Background(), "invites/claim", []byte(`{}`))
	require.NoError(t, err, "lantern-cloud's error is a result, not a relay failure")
	assert.Equal(t, http.StatusConflict, res.Status)
	assert.Equal(t, "invite already claimed or expired", res.Error)
	assert.Nil(t, res.Body)

	_, err = c.HelperRelay(context.Background(), "peer/register", nil)
	var ipcErr *Error
	require.ErrorAs(t, err, &ipcErr)
	assert.Equal(t, http.StatusBadRequest, ipcErr.Status)
}

func TestHelperActivityEndpoint(t *testing.T) {
	api := newLocalAPI(&backend.LocalBackend{}, false)
	w := serveHelper(t, api, http.MethodGet, helperActivityEndpoint, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var a struct {
		Day           string `json:"day"`
		ActiveSeconds int64  `json:"active_seconds"`
		RelayedBytes  uint64 `json:"relayed_bytes"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &a))
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, a.Day)
	assert.Zero(t, a.ActiveSeconds)
}

func TestHelperActivityClientRoundTrip(t *testing.T) {
	c := newClient()
	c.http.Transport = localTransport{api: newLocalAPI(&backend.LocalBackend{}, false)}
	a, err := c.HelperActivity(context.Background())
	require.NoError(t, err)
	assert.Regexp(t, `^\d{4}-\d{2}-\d{2}$`, a.Day)
	assert.Zero(t, a.RelayedBytes)
}

func TestHelperLiveClientRoundTrip(t *testing.T) {
	helper.DefaultLive.Reset()
	t.Cleanup(helper.DefaultLive.Reset)
	c := newClient()
	c.http.Transport = localTransport{api: newLocalAPI(&backend.LocalBackend{}, false)}

	s, err := c.HelperLive(context.Background())
	require.NoError(t, err)
	assert.Empty(t, s.Pairings)

	helper.DefaultLive.Open("test", "k", "pairing-1", nil)
	helper.DefaultLive.Add("pairing-1", helper.LiveThreshold)
	s, err = c.HelperLive(context.Background())
	require.NoError(t, err)
	require.Len(t, s.Pairings, 1)
	assert.Equal(t, "pairing-1", s.Pairings[0].PairingID)
	assert.True(t, s.Pairings[0].Connected)
	assert.NotZero(t, s.Pairings[0].Since)

	w := serveHelper(t, newLocalAPI(&backend.LocalBackend{}, false), http.MethodGet, helperLiveEndpoint, nil)
	require.Equal(t, http.StatusOK, w.Code)
	var raw struct {
		Pairings []map[string]any `json:"pairings"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &raw))
	require.Len(t, raw.Pairings, 1)
	assert.ElementsMatch(t, []string{"pairing_id", "connected", "since"}, mapKeys(raw.Pairings[0]))
}

func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A body that is not JSON is refused before marshalling, with fixed text
// rather than an encoder error that could quote it.
func TestHelperRelayClientRefusesANonJSONBody(t *testing.T) {
	c := newClient()
	c.http.Transport = localTransport{api: newLocalAPI(&backend.LocalBackend{}, false)}
	_, err := c.HelperRelay(context.Background(), "invites", []byte("not json secret"))
	assert.EqualError(t, err, "helper relay: body is not JSON")
}

// In-process calls carry the caller's span into the handler chain. The logger
// must not mark it with a /helper/ route, which would show the device uses
// helper invites; other routes are still annotated.
func TestLoggerSkipsHelperRoutes(t *testing.T) {
	mem := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(mem))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	api := newLocalAPI(&backend.LocalBackend{}, false)
	routeOf := func(method, path string) (string, bool) {
		mem.Reset()
		ctx, span := tp.Tracer("caller").Start(context.Background(), "caller")
		r := httptest.NewRequest(method, path, strings.NewReader(`{"op":"pairings"}`)).WithContext(ctx)
		api.ServeHTTP(httptest.NewRecorder(), r)
		span.End()
		for _, s := range mem.GetSpans() {
			for _, kv := range s.Attributes {
				if kv.Key == semconv.HTTPRouteKey {
					return kv.Value.AsString(), true
				}
			}
		}
		return "", false
	}
	_, ok := routeOf(http.MethodPost, helperRelayEndpoint)
	assert.False(t, ok, "relay")
	_, ok = routeOf(http.MethodGet, helperActivityEndpoint)
	assert.False(t, ok, "activity")
	_, ok = routeOf(http.MethodGet, helperLiveEndpoint)
	assert.False(t, ok, "live")
	route, ok := routeOf(http.MethodGet, "/not-a-route")
	assert.True(t, ok)
	assert.Equal(t, "/not-a-route", route)
}
