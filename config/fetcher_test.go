package config

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestFetchConfig(t *testing.T) {
	fetcher := newFetcher(&http.Client{Transport: &mockRoundtripper{t: t}})
	_, err := fetcher.fetchConfig()
	assert.NoError(t, err)
}

func Test_fetcher_fetchConfig(t *testing.T) {
	client := &http.Client{Timeout: 10 * time.Second}
	c, err := newFetcher(client).fetchConfig()
	require.NoError(t, err)

	for _, p := range c.Proxy.Proxies {
		t.Logf("Proxy: %s %s", p.Track, p.Protocol)
	}
}

type mockRoundtripper struct {
	t *testing.T
}

func (m *mockRoundtripper) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	require.NoError(m.t, err)

	cfg := new(ConfigRequest)
	err = proto.Unmarshal(body, cfg)
	require.NoError(m.t, err)

	ci := cfg.GetClientInfo()
	require.NotNil(m.t, ci, "missing client info")
	assert.NotEmpty(m.t, ci.FlashlightVersion, "config request missing flashlight version")
	assert.NotEmpty(m.t, ci.ClientVersion, "config request missing client version")
	assert.NotEmpty(m.t, ci.UserId, "config request missing user id")

	p := cfg.GetProxy()
	require.NotNil(m.t, p, "missing proxy info")

	return &http.Response{StatusCode: http.StatusNoContent}, nil
}
