package proxy

import (
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getlantern/radiance/config"
)

func TestProxy(t *testing.T) {
	config, err := config.GetConfig()
	require.NoError(t, err, "Failed to get config")

	p, err := NewProxy(config)
	require.NoError(t, err, "Failed to create proxy")

	rdy := make(chan struct{})
	go func() {
		close(rdy)
		err := p.ListenAndServe("localhost:8080")
		assert.NoError(t, err, "server failed")
	}()

	// yield and wait for the goroutine to start
	<-rdy

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: "localhost:8080"}),
		},
	}
	resp, err := client.Get("https://geo.getiantem.org/lookup/185.228.19.20")
	require.NoError(t, err, "client failed to make request")

	t.Logf("response: %+v", resp)

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "client failed to read response body")

	t.Log(string(body))
}
